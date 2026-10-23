package managedcontext

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	cc "github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
)

func (e *Engine) CompactResponse(ctx context.Context, op *Operation) (*CompactResult, error) {
	if op.Error != nil && (op.State == "failed" || op.State == "uncertain") {
		return nil, op.Error
	}
	if op.State != "completed" || op.ResponseRef == "" {
		return nil, nil
	}
	b, err := e.Store.Blob(ctx, op.Scope, op.ResponseRef)
	if err != nil {
		return nil, err
	}
	var result CompactResult
	if json.Unmarshal(b, &result) != nil {
		return nil, E("corrupt", "stored compact result is invalid", 503)
	}
	if err = e.compactJob(ctx, op, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
func (e *Engine) Stateless(ctx context.Context, op *Operation, r *Record, l *Lease, parsed Parsed, target int, x Executor) (out *CompactResult, resultErr error) {
	ctx, stop := context.WithTimeout(ctx, time.Duration(op.Policy.WallSeconds)*time.Second)
	defer stop()
	if result, err := e.CompactResponse(ctx, op); result != nil || err != nil {
		return result, err
	}
	if err := e.recoverOperation(ctx, op, r, l, x); err != nil {
		return nil, err
	}
	base := *parsed.Request
	base.Model = op.Model
	n := outputLimit(op.Policy, x)
	if base.MaxOutputTokens != nil {
		n = *base.MaxOutputTokens
	}
	if n < 1 || n > outputLimit(op.Policy, x) {
		return nil, E("input_invalid", "max_output_tokens exceeds managed policy", 400)
	}
	base.MaxOutputTokens = &n
	items := parsed.Items
	budget, err := x.Measure(requestWith(&base, items))
	if err != nil {
		return nil, err
	}
	gates, err := cc.ResolveThresholds(nil, budget.SafeInput)
	if err != nil {
		return nil, err
	}
	if target == 0 {
		target = budget.SafeInput / 2
	}
	if target < 1 || target >= gates.Hard {
		return nil, E("input_invalid", "target_input_tokens must be below the model hard budget", 400)
	}
	result := &CompactResult{Status: "no_op", Input: items, Before: budget.InputEstimate, After: budget.InputEstimate, Method: budget.Method, ArtifactID: "cmp_" + op.ID}
	job := &Job{Model: op.Model, Owner: op.Owner, ID: result.ArtifactID, OperationID: op.ID, State: "prepared", Trigger: "manual", Before: result.Before, Target: target, Snapshot: cc.Snapshot{Scope: op.Scope, PrefixDigest: op.Fingerprint, Policy: op.Policy.Version, ModelRevision: op.Model}}
	if _, err = e.Store.Create(ctx, record(op.Owner, "job", job.ID, job.State, job, r.ExpiresAt), nil); err != nil {
		return nil, err
	}
	op.JobID = job.ID
	defer func() {
		if resultErr != nil {
			p, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			e.failCompactJob(p, op, job.ID, typed(resultErr))
		}
	}()
	encodedResult, _ := json.Marshal(result)
	op.ReservedBytes = int64(len(encodedResult) + 4096)
	if result.After > target {
		op.ReservedBytes += int64((op.Policy.SummaryOutputTokens*(op.Policy.SummaryAttempts*2+1))*64 + 65536)
	}
	if err = e.save(ctx, r, op, op.State, l, nil, &op.Policy); err != nil {
		return nil, err
	}
	// Resume from a known successful summary after a crash before publishing the
	// stateless result. Each summary is linked to its exact quoted prefix below.
	for pass := 0; result.After > target && pass < op.Policy.SummaryAttempts; pass++ {
		keep := cc.ProtectedItems(items, protection())
		fixed, err := x.Measure(requestWith(&base, keep))
		if err != nil {
			return nil, err
		}
		if fixed.InputEstimate >= target {
			return nil, E("target_unreachable", "protected context exceeds the requested target", 413)
		}
		cut := cc.PlanPrefix(items, cc.PrefixPolicy{RetainItems: 1, MinItems: 2, MaxBytes: budget.SafeInput})
		var summaryReq *apicompat.ResponsesRequest
		for cut > 0 {
			summaryReq = cc.BuildSummaryRequest(items[:cut], op.Model, summaryLimit(op.Policy, x))
			m, err := x.Measure(summaryReq)
			if err == nil && m.InputEstimate < m.SafeInput {
				break
			}
			cut = cc.PlanPrefix(items, cc.PrefixPolicy{RetainItems: 1, MinItems: 2, MaxBytes: cc.SizeBytes(items[:cut]) / 2})
		}
		if cut < 2 {
			return nil, E("target_unreachable", "no complete historical prefix can be summarized", 413)
		}
		minimum := append(cc.ProtectedItems(items[:cut], protection()), items[cut:]...)
		tailBudget, measureErr := x.Measure(requestWith(&base, minimum))
		if measureErr != nil {
			return nil, measureErr
		}
		if tailBudget.InputEstimate >= target {
			return nil, E("target_unreachable", "protected context and current tool group exceed the target", 413)
		}
		purpose := "compaction"
		if pass > 0 {
			purpose = "compaction_" + strconv.Itoa(pass)
		}
		a, response, err := e.replayableAttempt(ctx, op, purpose, x)
		if err != nil {
			return nil, err
		}
		if response == nil {
			a, response, err = e.call(ctx, op, r, l, summaryReq, purpose, x)
			if err != nil {
				return nil, err
			}
		}
		summary, err := cc.ParseSummary(response)
		if err != nil {
			return nil, err
		}
		msg := cc.HistoricalMessage(*summary, cc.Digest(items[:cut]), "summary_context_"+result.ArtifactID)
		replacement, err := cc.ReplacePrefix(items, cut, cc.ProtectedItems(items[:cut], protection()), msg)
		if err != nil {
			return nil, err
		}
		if !replacement.Smaller() {
			return nil, E("target_unreachable", "summary did not reduce the historical context", 413)
		}
		items = replacement.Items
		measured, err := x.Measure(requestWith(&base, items))
		if err != nil {
			return nil, err
		}
		result.Input = items
		result.After = measured.InputEstimate
		result.Status = "compacted"
		if a != nil {
			result.Usage = addUsage(result.Usage, a.Usage)
		}
	}
	if result.After > target {
		return nil, E("target_unreachable", "bounded compaction could not reach the requested target", 413)
	}
	b, _ := json.Marshal(result)
	op.ResponseRef = "compact_" + op.ID
	meta := *result
	meta.Input = nil
	op.Compact = &meta
	op.State = "completed"
	op.ReservedBytes = 0
	blob := Blob{ID: op.ResponseRef, Visibility: "private", Kind: "compact_result", Data: b, ExpiresAt: r.ExpiresAt}
	if err = e.save(ctx, r, op, op.State, l, []Blob{blob}, &op.Policy); err != nil {
		return nil, err
	}
	if err = e.compactJob(ctx, op, result); err != nil {
		return nil, err
	}
	return result, nil
}
func (e *Engine) compactJob(ctx context.Context, op *Operation, result *CompactResult) error {
	job, jr, err := load[Job](ctx, e.Store, op.Scope, "job", result.ArtifactID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if job == nil {
		job = &Job{Model: op.Model, Owner: op.Owner, ID: result.ArtifactID, OperationID: op.ID, Trigger: "manual", Before: result.Before}
	}
	job.State = result.Status
	job.After = result.After
	job.Accounted = true
	job.Reason = ""
	job.Usage = result.Usage
	if len(op.Attempts) > 0 {
		job.AttemptID = op.Attempts[len(op.Attempts)-1]
	}
	if jr == nil {
		_, err = e.Store.Create(ctx, record(op.Owner, "job", job.ID, job.State, job, op.CreatedAt.Add(48*time.Hour)), nil)
		return err
	}
	return e.save(ctx, jr, job, job.State, nil, nil, nil)
}
func (e *Engine) failCompactJob(ctx context.Context, op *Operation, id string, cause *cc.Error) {
	j, jr, err := load[Job](ctx, e.Store, op.Scope, "job", id)
	if err != nil {
		return
	}
	j.State = "failed"
	j.Reason = cause.Code
	j.Accounted = true
	j.Usage = nil
	if cause.Code == "context_attempt_uncertain" {
		j.State = "uncertain"
	} else if cause.Status == 503 {
		j.State = "needs_retry"
	}
	for _, id := range op.Attempts {
		a, _, readErr := load[Attempt](ctx, e.Store, op.Scope, "attempt", id)
		if readErr != nil {
			j.Accounted = false
			continue
		}
		j.AttemptID = id
		j.Usage = addUsage(j.Usage, a.Usage)
		j.Accounted = j.Accounted && a.Settled
	}
	_ = e.save(ctx, jr, j, j.State, nil, nil, nil)
}

// RecoverInternal returns an already-terminal manual job without model discovery,
// account slots or new inference. It also repairs a missing operation receipt.
func (e *Engine) RecoverInternal(ctx context.Context, op *Operation, or *Record, ol *Lease) (*Job, error) {
	id := op.JobID
	if id == "" {
		id = "cmp_" + op.ID
	}
	j, _, err := e.LoadJob(ctx, op.Scope, id)
	if errors.Is(err, ErrNotFound) && op.JobID == "" && op.SessionID != "" {
		// A paid job can commit its session marker before the operation link.
		s, _, readErr := e.LoadSession(ctx, op.Scope, op.SessionID)
		if readErr != nil {
			return nil, readErr
		}
		id = s.JobID
		if id == "" {
			id = s.AppliedJobID
		}
		if id == "" {
			return nil, nil
		}
		j, _, err = e.LoadJob(ctx, op.Scope, id)
	}
	if errors.Is(err, ErrNotFound) && op.JobID == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if j.OperationID != op.ID || j.SessionID != op.SessionID {
		return nil, nil
	}
	if j.State != "applied" && j.State != "failed" && j.State != "stale" && j.State != "no_op" {
		return nil, nil
	}
	if op.JobID != j.ID || op.State != "completed" {
		op.JobID, op.State = j.ID, "completed"
		if err = e.save(ctx, or, op, op.State, ol, nil, nil); err != nil {
			return nil, err
		}
	}
	return j, nil
}

func (e *Engine) Internal(ctx context.Context, op *Operation, or *Record, ol *Lease, expected int64, x Executor) (*Job, error) {
	ctx, stop := context.WithTimeout(ctx, time.Duration(op.Policy.WallSeconds)*time.Second)
	defer stop()
	if j, err := e.RecoverInternal(ctx, op, or, ol); j != nil || err != nil {
		return j, err
	}
	noOpID := "cmp_" + op.ID
	ctx, lease, release, err := e.lease(ctx, op.Scope, "session", op.SessionID)
	if err != nil {
		return nil, err
	}
	defer release()
	s, sr, err := e.LoadSession(ctx, op.Scope, op.SessionID)
	if err != nil {
		return nil, err
	}
	if err = e.ensureLive(ctx, s); err != nil {
		return nil, err
	}
	if op.JobID == "" && s.JobID != "" {
		j, _, readErr := load[Job](ctx, e.Store, s.Scope, "job", s.JobID)
		if readErr != nil {
			return nil, readErr
		}
		if j.OperationID == op.ID {
			op.JobID = j.ID
			if err = e.save(ctx, or, op, op.State, ol, nil, nil); err != nil {
				return nil, err
			}
		}
	}
	if op.JobID == "" && sr.Revision != expected {
		return nil, E("revision_conflict", "expected_revision no longer matches the session", 409)
	}
	if s.OperationID != "" || s.PendingCall != "" {
		return nil, E("tool_pending", "finish the active request and pending tool before manual compaction", 409)
	}
	if err = e.collect(ctx, s, sr, &lease, x); err != nil {
		return nil, err
	}
	if op.JobID == "" {
		base := s.Template
		items, err := e.messages(ctx, s.Scope, s.Active)
		if err != nil {
			return nil, err
		}
		m, err := x.Measure(requestWith(&base, items))
		if err != nil {
			return nil, err
		}
		if m.InputEstimate <= m.SafeInput/2 {
			j := &Job{Model: s.Model, Owner: s.Owner, ID: noOpID, OperationID: op.ID, SessionID: s.ID, State: "no_op", Trigger: "manual", Before: m.InputEstimate, After: m.InputEstimate, Target: m.SafeInput / 2, Accounted: true}
			if _, err = e.Store.Create(ctx, record(j.Owner, "job", j.ID, j.State, j, or.ExpiresAt), nil); err != nil {
				return nil, err
			}
			op.JobID, op.State = j.ID, "completed"
			return j, e.save(ctx, or, op, op.State, ol, nil, nil)
		}
		started, err := e.startSummary(ctx, s, sr, &lease, op, or, ol, &base, x, false, "manual", m.SafeInput/2)
		if err != nil {
			return nil, err
		}
		if !started {
			return nil, E("target_unreachable", "there is no eligible completed prefix", 413)
		}
		op.JobID = s.JobID
		if err = e.save(ctx, or, op, op.State, ol, nil, nil); err != nil {
			return nil, err
		}
	}
	if err = e.collect(ctx, s, sr, &lease, x); err != nil {
		return nil, err
	}
	_, err = e.apply(ctx, s, sr, &lease, &s.Template, x)
	if err != nil {
		return nil, err
	}
	j, _, err := load[Job](ctx, e.Store, s.Scope, "job", op.JobID)
	if err != nil {
		return nil, err
	}
	s.ReservedBytes = 0
	s.ReservationOperationID = ""
	if err = e.save(ctx, sr, s, s.Status, &lease, nil, &s.Policy); err != nil {
		return nil, err
	}
	op.State = "completed"
	if err = e.save(ctx, or, op, op.State, ol, nil, nil); err != nil {
		return nil, err
	}
	return j, nil
}

// WithOperation serializes all retries of one idempotent logical request.
func (e *Engine) WithOperation(ctx context.Context, op *Operation, fn func(context.Context, *Operation, *Record, *Lease) error) error {
	// Detach once, before starting either lease heartbeat. Inner work inherits
	// lease loss, shutdown and the bounded deadline even if the client leaves.
	ctx, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(op.Policy.WallSeconds)*time.Second)
	defer stop()
	ctx, untrack, err := e.track(ctx)
	if err != nil {
		return err
	}
	defer untrack()
	ctx, l, release, err := e.lease(ctx, op.Scope, "operation", op.ID)
	if err != nil {
		return err
	}
	defer release()
	latest, r, err := load[Operation](ctx, e.Store, op.Scope, "operation", op.ID)
	if err != nil {
		return err
	}
	return fn(ctx, latest, r, &l)
}
