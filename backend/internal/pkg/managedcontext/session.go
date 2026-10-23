package managedcontext

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	cc "github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
)

type Result struct {
	Response  *apicompat.ResponsesResponse
	SessionID string
	Revision  int64
	Replayed  bool
}

func (e *Engine) Advance(ctx context.Context, op *Operation, or *Record, ol *Lease, parsed Parsed, x Executor) (*Result, error) {
	bounded, stop := context.WithTimeout(ctx, time.Duration(op.Policy.WallSeconds)*time.Second)
	defer stop()
	ctx = bounded
	// Operation identity is durable before an inference/session exists.
	if response, err := e.Response(ctx, op); response != nil || err != nil {
		return &Result{Response: response, SessionID: op.SessionID, Replayed: true}, err
	}
	var s *Session
	var sr *Record
	var err error
	fresh := parsed.Request.PreviousResponseID == "" && op.SessionID == ""
	if fresh {
		hasUser := false
		for _, i := range parsed.Items {
			if i.Role == "user" {
				hasUser = true
			}
			if i.Type != "message" || i.Role == "assistant" {
				return nil, E("input_invalid", "new managed sessions require a new user conversation", 400)
			}
		}
		if !hasUser {
			return nil, E("input_invalid", "new managed sessions require a user message", 400)
		}
		template := *parsed.Request
		template.Input = nil
		template.PreviousResponseID = ""
		template.Stream = false
		n := min(1024, outputLimit(op.Policy, x))
		if template.MaxOutputTokens != nil {
			n = *template.MaxOutputTokens
		}
		if n < 1 || n > outputLimit(op.Policy, x) {
			return nil, E("input_invalid", "max_output_tokens exceeds managed policy", 400)
		}
		template.MaxOutputTokens = &n
		s = &Session{Owner: op.Owner, ID: "ctx_" + cc.Digest([]string{op.Scope, op.ID})[:48], Schema: 2, Model: op.Model, Policy: op.Policy, Template: template, Contract: contract(&template), Status: "ready", ExpiresAt: e.now().Add(time.Duration(op.Policy.TTLSeconds) * time.Second)}
		sr = record(s.Owner, "session", s.ID, s.Status, s, s.ExpiresAt)
		created, createErr := e.Store.Create(ctx, sr, &s.Policy)
		if createErr != nil {
			return nil, createErr
		}
		if !created {
			s, sr, err = e.LoadSession(ctx, op.Scope, s.ID)
			if err != nil {
				return nil, err
			}
		}
		op.SessionID = s.ID
		if err = e.save(ctx, or, op, op.State, ol, nil, nil); err != nil {
			return nil, err
		}
	} else {
		s, sr, err = e.LoadSession(ctx, op.Scope, op.SessionID)
		if err != nil {
			return nil, err
		}
	}
	if bound, ok := x.(interface{ BindSession(string) }); ok {
		bound.BindSession(s.ID)
	}
	ctx, lease, release, err := e.lease(ctx, s.Scope, "session", s.ID)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, unwatch := e.watchCancel(ctx, s.Scope, s.ID)
	defer unwatch()
	// Reload after acquiring execution ownership; no stale pre-lease view is used.
	s, sr, err = e.LoadSession(ctx, s.Scope, s.ID)
	if err != nil {
		return nil, err
	}
	if err = e.ensureLive(ctx, s); err != nil {
		return nil, err
	}
	ctx, sessionStop := context.WithDeadline(ctx, s.ExpiresAt)
	defer sessionStop()
	if s.AccountID != op.AccountID || s.Model != op.Model {
		return nil, E("account_bound", "managed model/account cannot change", 409)
	}
	if err = e.recoverOperation(ctx, op, or, ol, x); err != nil {
		return nil, err
	}
	if err = e.collect(ctx, s, sr, &lease, x); err != nil {
		return nil, err
	}
	if s.Status == "uncertain" && s.OperationID == op.ID {
		s.Status = "running"
	}
	if s.Status == "uncertain" || s.Status == "failed" {
		return nil, E("session_stopped", "managed session requires a new conversation", 409)
	}
	if s.OperationID != "" && s.OperationID != op.ID {
		return nil, E("request_incomplete", "resume the in-progress request with its original idempotency key", 409)
	}
	req, err := Inherit(parsed, s)
	if err != nil {
		return nil, err
	}
	req.Model = s.Model
	n := *s.Template.MaxOutputTokens
	if req.MaxOutputTokens != nil {
		n = *req.MaxOutputTokens
	}
	if n < 1 || n > outputLimit(s.Policy, x) {
		return nil, E("input_invalid", "max_output_tokens exceeds managed policy", 400)
	}
	req.MaxOutputTokens = &n
	if s.OperationID == "" {
		if parsed.Request.PreviousResponseID != s.Head {
			return nil, E("lineage_conflict", "previous_response_id is not the current session head", 409)
		}
		if s.Requests >= s.Policy.MaxRequests {
			return nil, E("request_limit", "managed session request budget reached", 413)
		}
		if s.PendingCall != "" {
			if len(parsed.Items) != 1 || parsed.Items[0].Type != "function_call_output" || parsed.Items[0].CallID != s.PendingCall {
				return nil, E("tool_pending", "return the pending tool result before adding new input", 409)
			}
		} else {
			for _, i := range parsed.Items {
				if i.Type != "message" || i.Role == "assistant" {
					return nil, E("input_invalid", "only new user input is accepted at this session boundary", 400)
				}
			}
		}
		blobs := []Blob{}
		for _, item := range parsed.Items {
			ref, b := e.itemBlob(s, item, true, false)
			s.Active = append(s.Active, ref)
			s.HistoryBytes += int64(ref.Bytes)
			blobs = append(blobs, b)
		}
		s.PendingCall = ""
		s.OperationID = op.ID
		s.Requests++
		s.Status = "running"
		// Reserve bounded room for the foreground and maintenance outputs before
		// paying for either. Quotas count this reservation across all user sessions.
		s.ReservedBytes = int64((n*3+s.Policy.SummaryOutputTokens*(s.Policy.SummaryAttempts*2+1))*64 + 65536)
		if err = e.save(ctx, sr, s, s.Status, &lease, blobs, &s.Policy); err != nil {
			return nil, err
		}
	}
	// A persisted physical result is recovered before considering another call.
	a, response, err := e.replayableAttempt(ctx, op, "inference", x)
	if err != nil {
		return nil, e.stopOperation(ctx, op, or, ol, s, sr, &lease, err)
	}
	if response == nil {
		req, err = e.prepare(ctx, s, sr, &lease, op, or, ol, req, x)
		if err != nil {
			return nil, e.stopOperation(ctx, op, or, ol, s, sr, &lease, err)
		}
		if err = e.ensureLive(ctx, s); err != nil {
			return nil, err
		}
		budget, measureErr := x.Measure(req)
		if measureErr != nil {
			return nil, measureErr
		}
		s.LastBudget = &budget
		if err = e.save(ctx, sr, s, s.Status, &lease, nil, &s.Policy); err != nil {
			return nil, err
		}
		op.State = "running"
		if err = e.save(ctx, or, op, op.State, ol, nil, nil); err != nil {
			return nil, err
		}
		a, response, err = e.call(ctx, op, or, ol, req, "inference", x)
		if err != nil {
			return nil, e.stopOperation(ctx, op, or, ol, s, sr, &lease, err)
		}
	}
	// A late known result may still be billed, but cannot publish a new head
	// after cancellation or the session deadline.
	if err = e.ensureLive(ctx, s); err != nil {
		return nil, err
	}
	items, err := outputItems(response)
	if err != nil {
		return nil, e.stopOperation(ctx, op, or, ol, s, sr, &lease, err)
	}
	// Verify tool names against the fixed client contract, not just their shape.
	for _, item := range items {
		if item.Type == "function_call" {
			found := false
			for _, tool := range s.Template.Tools {
				if tool.Name == item.Name {
					found = true
					break
				}
			}
			if !found {
				return nil, e.stopOperation(ctx, op, or, ol, s, sr, &lease, E("tool_contract", "provider requested an undeclared client tool", 502))
			}
		}
	}
	response.ID = "resp_ctx_" + op.ID
	response.Model = s.Model
	if response.CreatedAt == 0 {
		response.CreatedAt = op.CreatedAt.Unix()
	}
	blobs := []Blob{}
	for _, item := range items {
		if item.Type == "function_call" {
			s.PendingCall = item.CallID
		}
		ref, b := e.itemBlob(s, item, item.Type != "reasoning", false)
		s.Active = append(s.Active, ref)
		s.HistoryBytes += int64(ref.Bytes)
		blobs = append(blobs, b)
	}
	b, _ := json.Marshal(response)
	responseID := "public_" + op.ID
	blobs = append(blobs, Blob{ID: responseID, SessionID: s.ID, Visibility: "private", Kind: "response", Data: b, ExpiresAt: or.ExpiresAt})
	s.HistoryBytes += int64(len(b))
	s.Head = response.ID
	s.OperationID = ""
	s.ReservedBytes = 0
	s.Status = "ready"
	if s.PendingCall != "" {
		s.Status = "waiting_tool"
	}
	if a != nil {
		s.InferenceUsage = addUsage(s.InferenceUsage, a.Usage)
	}
	if err = e.save(ctx, sr, s, s.Status, &lease, blobs, &s.Policy); err != nil {
		return nil, err
	}
	op.State = "completed"
	op.ResponseRef = responseID
	if err = e.save(ctx, or, op, op.State, ol, nil, nil); err != nil {
		return nil, err
	}
	return &Result{Response: response, SessionID: s.ID, Revision: sr.Revision}, nil
}
func (e *Engine) stopOperation(ctx context.Context, op *Operation, r *Record, l *Lease, s *Session, sr *Record, sl *Lease, cause error) error {
	p, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	if v := typed(cause); v.Status == 503 && (v.Code == "context_billing_pending" || v.Code == "context_unavailable") {
		return cause
	}
	op.Error = typed(cause)
	op.State = "failed"
	s.Status = "failed"
	if op.Error.Code == "context_attempt_uncertain" {
		op.State = "uncertain"
		s.Status = "uncertain"
	}
	// Retain the active operation and storage reservation for reconciliation.
	_ = e.save(p, r, op, op.State, l, nil, nil)
	_ = e.save(p, sr, s, s.Status, sl, nil, &s.Policy)
	return cause
}

// RecoverPublished repairs the final operation pointer after a commit/response
// crash without appending the terminal output to the lane a second time.
func (e *Engine) RecoverPublished(ctx context.Context, op *Operation, r *Record, l *Lease) (*apicompat.ResponsesResponse, error) {
	if op.ResponseRef != "" {
		return e.Response(ctx, op)
	}
	if op.SessionID == "" {
		return nil, nil
	}
	// This immutable blob is created only in the same transaction that publishes
	// the session head. It remains proof of publication after newer turns advance.
	responseID := "public_" + op.ID
	b, err := e.Store.Blob(ctx, op.Scope, responseID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var response apicompat.ResponsesResponse
	if json.Unmarshal(b, &response) != nil || response.ID != "resp_ctx_"+op.ID {
		return nil, E("corrupt", "published response failed validation", 503)
	}
	op.ResponseRef = responseID
	op.State = "completed"
	op.Error = nil
	if err = e.save(ctx, r, op, op.State, l, nil, nil); err != nil {
		return nil, err
	}
	return &response, nil
}
