package fusion

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const (
	closingDrain   = "drain"
	closingConsult = "consult"
	closingReport  = "report"
	closingReview  = "review"
	BudgetClosed   = "budget_closed"
)

const budgetCloseNotice = "Further work was stopped to preserve this turn's limits. The review below may include unfinished work; this response does not certify that all task acceptance criteria were met.\n\n"

type ClosingState struct {
	Phase           string    `json:"phase"`
	StartedAt       time.Time `json:"started_at"`
	RequestsAtStart int       `json:"requests_at_start"`
}

func ValidateClosingPolicy(policy string) error {
	if policy != "" && policy != ClosingReserveV1 {
		return E("closing_policy_invalid", "closing policy must be reserve-v1 or omitted", 400)
	}
	return nil
}

func (s *Session) closingEnabled() bool { return s.Profile.ClosingPolicy == ClosingReserveV1 }
func (s *Session) isClosing() bool      { return s.closingEnabled() && s.Closing != nil }

// Accepted delegations are obligations even while suspended in a parent's
// queue. Reporting them and the final Lead review must remain real inferences.
func closingCalls(s *Session, additionalDelegations int) (lead, sidekick int) {
	for _, h := range s.Handoffs {
		if h.Turn == s.Turn && h.Report == "" {
			sidekick++
		}
	}
	for _, tool := range s.QueuedTools {
		if tool.Turn == s.Turn && tool.Name == DelegateTool {
			sidekick++
		}
	}
	sidekick += additionalDelegations
	lead = 1
	if s.TurnHandoffs == 0 && sidekick == 0 {
		// A Lead-only final must never masquerade as Fusion. The first real
		// consultation still needs a Lead delegation and a Sidekick report.
		lead++
		sidekick = 1
	}
	return
}

func remainingSummaries(s *Session, role Role) int {
	p := s.Profile.CompactPolicy()
	if !p.Enabled {
		return 0
	}
	used := 0
	if c := s.Contexts[role]; c != nil && c.AttemptTurn == s.Turn {
		used = c.Attempts
	}
	return max(0, p.MaxAttempts-used)
}

func closingReserve(s *Session, additionalDelegations int) int {
	lead, worker := closingCalls(s, additionalDelegations)
	return lead + worker + min(lead, remainingSummaries(s, Lead)) + min(worker, remainingSummaries(s, Sidekick))
}

func closingPhase(s *Session) string {
	if !s.isClosing() {
		return ""
	}
	if s.Pending != nil {
		return closingDrain
	}
	if s.Role == Sidekick {
		return closingReport
	}
	if s.TurnHandoffs == 0 {
		return closingConsult
	}
	return closingReview
}

func (e *Engine) updateClosing(ctx context.Context, s *Session, raw *[]byte) (bool, error) {
	if !s.closingEnabled() || s.State == Completed {
		return false, nil
	}
	started := false
	if s.Closing == nil && s.Profile.MaxRequests-s.TurnRequests <= closingReserve(s, 0) {
		s.Closing = &ClosingState{StartedAt: e.now(), RequestsAtStart: s.TurnRequests}
		started = true
	}
	if s.Closing != nil && s.Closing.Phase != closingPhase(s) {
		s.Closing.Phase = closingPhase(s)
		if err := e.save(ctx, s, raw); err != nil {
			return started, err
		}
		e.workflow(s, WorkflowEvent{Stage: "closing_phase", Phase: s.Closing.Phase})
	}
	return started, nil
}

func closingRequest(s *Session, req *apicompat.ResponsesRequest) {
	if !s.closingEnabled() {
		return
	}
	var lane []apicompat.ResponsesInputItem
	_ = json.Unmarshal(req.Input, &lane)
	note := fmt.Sprintf("Gateway request-budget notice (workflow control, not a new user turn): %d of %d counted requests used, including summaries; the original cost limit and deadline still apply.", s.TurnRequests, s.Profile.MaxRequests)
	switch closingPhase(s) {
	case closingConsult:
		note += " Further workspace work is closed. Use sub2api_sidekick once to obtain an honest evidence/status report for the original task, then review it. Do not claim the task is complete without evidence."
	case closingReport:
		note += " Further workspace work is closed. Return your report now: completed work, actual checks and failures, uncertainty and remaining work. No tools or delegation are available; never invent results."
		req.Tools = nil
		req.ToolChoice = json.RawMessage(`"none"`)
	case closingReview:
		note += " Further workspace work is closed. Review the actual Sidekick report(s) against the original task and finalize now, explicitly stating unfinished or unverified work. No tools or further delegation are available; never claim unsupported success."
		req.Tools = nil
		req.ToolChoice = json.RawMessage(`"none"`)
	}
	// Keep the stable instruction/history prefix. This private notice is not
	// adopted into Public or used to start/reset a logical user turn.
	lane = append(lane, textMessage("user", note))
	req.Input, _ = json.Marshal(lane)
}

func validateClosingOutput(a *Attempt) error {
	if a.ClosingPhase == "" {
		return nil
	}
	for _, out := range a.Response.Output {
		if out.Type == "function_call" && (a.ClosingPhase != closingConsult || out.Name != DelegateTool) {
			return E("closing_tools_unsupported", "model requested new work during the report/review phase; no tool was executed", 502)
		}
	}
	return nil
}

func traceTool(tool *PendingTool, now time.Time) *ToolTrace {
	return &ToolTrace{PublicID: tool.PublicID, InternalID: tool.InternalID, Role: tool.Role, HandoffID: tool.HandoffID, SourceAttemptID: tool.SourceAttemptID, At: now}
}

// Only gateway-generated identifiers and sizes enter this observer. Its events
// are diagnostic, never authority to execute or settle a request again.
type WorkflowEvent struct {
	Stage        string
	Phase        string
	SessionRef   string
	Turn         int
	Requests     int
	Limit        int
	Role         Role
	AttemptID    string
	PublicCallID string
	ResultBytes  int
}

func (e *Engine) workflow(s *Session, event WorkflowEvent) {
	if !s.closingEnabled() || e.WorkflowObserver == nil {
		return
	}
	event.SessionRef, event.Turn, event.Requests, event.Limit = Digest(s.ID), s.Turn, s.TurnRequests, s.Profile.MaxRequests
	if event.Role == "" {
		event.Role = s.Role
	}
	e.WorkflowObserver(event)
}
