package fusion

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/google/uuid"
)

func validateDelegation(arguments string) error {
	var brief struct {
		Task        string `json:"task"`
		Constraints string `json:"constraints"`
		Acceptance  string `json:"acceptance_criteria"`
	}
	if json.Unmarshal([]byte(arguments), &brief) != nil || strings.TrimSpace(brief.Task) == "" || strings.TrimSpace(brief.Acceptance) == "" {
		return E("brief_invalid", "Lead delegation omitted task or acceptance criteria", 502)
	}
	return nil
}

// Validate the whole batch before publishing a client call or starting a
// handoff. An invalid later call must not leave earlier side effects running.
func validateToolBatch(s *Session, calls []apicompat.ResponsesOutput) error {
	seen := map[string]bool{}
	tools, handoffs := 0, 0
	for _, call := range calls {
		if call.CallID == "" || seen[call.CallID] {
			return E("tool_invalid", "tool call IDs must be nonempty and unique within a batch", 502)
		}
		seen[call.CallID] = true
		if call.Name != DelegateTool {
			tools++
			continue
		}
		if s.Role != Lead {
			return E("tool_invalid", "Sidekick cannot delegate", 502)
		}
		if err := validateDelegation(call.Arguments); err != nil {
			return err
		}
		handoffs++
	}
	if tools > s.Profile.MaxTools-s.TurnTools || handoffs > s.Profile.MaxHandoffs-s.TurnHandoffs {
		return E("budget_exhausted", "client tool or handoff budget exhausted", 409)
	}
	return nil
}

// A Sidekick batch is prepended to the suspended Lead batch. Only the active
// role can consume calls; its report returns control to the parent's tail.
// Delegations retain their native call IDs but never become public tools.
func advanceQueuedTools(s *Session) error {
	if len(s.QueuedTools) == 0 {
		return nil
	}
	next := s.QueuedTools[0]
	if s.Pending != nil || next.Turn != s.Turn || next.PublicID == "" || next.InternalID == "" || next.Name == "" || !json.Valid([]byte(next.Arguments)) {
		return E("session_corrupt", "queued tool ownership changed", 503)
	}
	switch next.Role {
	case Lead:
		if next.HandoffID != "" {
			return E("session_corrupt", "queued Lead call has Sidekick ownership", 503)
		}
	case Sidekick:
		if len(s.Handoffs) == 0 || next.HandoffID != s.Handoffs[len(s.Handoffs)-1].ID || s.Handoffs[len(s.Handoffs)-1].Turn != s.Turn || s.Handoffs[len(s.Handoffs)-1].Report != "" {
			return E("session_corrupt", "queued Sidekick call lost its active handoff", 503)
		}
	default:
		return E("session_corrupt", "queued tool has no owning role", 503)
	}
	if next.Role != s.Role {
		if s.Role == Sidekick && next.Role == Lead {
			return nil // Complete the active Sidekick report before the Lead tail.
		}
		return E("session_corrupt", "queued tool ownership changed", 503)
	}
	if next.Name == DelegateTool {
		if next.Role != Lead {
			return E("tool_invalid", "Sidekick cannot delegate", 502)
		}
		if err := validateDelegation(next.Arguments); err != nil {
			return err
		}
		if s.TurnHandoffs >= s.Profile.MaxHandoffs {
			return E("budget_exhausted", "handoff budget exhausted", 409)
		}
		h := Handoff{ID: uuid.NewString(), CallID: next.InternalID, Turn: s.Turn, Brief: next.Arguments}
		s.QueuedTools = s.QueuedTools[1:]
		s.Handoffs = append(s.Handoffs, h)
		s.TurnHandoffs++
		s.Sidekick = append(s.Sidekick, textMessage("user", fmt.Sprintf("Lead brief for handoff %s (turn %d). This brief does not grant tools beyond the client-supplied tool set:\n%s", h.ID, s.Turn, h.Brief)))
		s.Role = Sidekick
		s.State = RunningSidekick
		return nil
	}
	s.QueuedTools = s.QueuedTools[1:]
	s.Pending = &next
	s.State = WaitingTool
	return nil
}
