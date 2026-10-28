package contextcompact

import (
	"encoding/json"
	"strings"
)

// PrefixPolicy specifies retention rather than coupling the planner to a
// particular agent. Fusion v1 retains six items and requires a two-item prefix.
type PrefixPolicy struct {
	RetainItems int
	MinItems    int
	MaxBytes    int
}

// PlanPrefix selects a bounded, completed prefix without cutting an open tool
// call/result group. Input tool IDs are validated by the protocol adapter.
func PlanPrefix(items []Item, policy PrefixPolicy) int {
	cut := len(items) - max(policy.RetainItems, 0)
	if cut <= 0 {
		return 0
	}
	open := map[string]bool{}
	used, best := 0, 0
	for i := 0; i < cut; i++ {
		item := items[i]
		b, _ := json.Marshal(item)
		used += len(b) + 1
		if used > policy.MaxBytes {
			break
		}
		if item.Type == "function_call" {
			open[item.CallID] = true
		}
		if item.Type == "function_call_output" {
			delete(open, item.CallID)
		}
		if len(open) == 0 {
			best = i + 1
		}
	}
	if best < max(policy.MinItems, 1) {
		return 0
	}
	return best
}

// Protection describes exact source items that must survive lossy compaction.
// ActiveUserMarker and ActiveToolCallID preserve an in-progress collaboration
// brief/hand-off without exposing collaboration concepts to the planner.
type Protection struct {
	AllUserMessages  bool
	SummaryIDPrefix  string
	ActiveUserMarker string
	ActiveToolCallID string
}

func ProtectedItems(items []Item, policy Protection) []Item {
	var out []Item
	goal := false
	for _, item := range items {
		if policy.ActiveToolCallID != "" && (item.Type == "function_call" || item.Type == "function_call_output") && item.CallID == policy.ActiveToolCallID {
			out = append(out, item)
			continue
		}
		if item.Type != "message" {
			continue
		}
		if item.Role == "system" || item.Role == "developer" || item.Role == "user" &&
			(!goal || policy.AllUserMessages && (policy.SummaryIDPrefix == "" || !strings.HasPrefix(item.ID, policy.SummaryIDPrefix)) ||
				policy.ActiveUserMarker != "" && strings.Contains(string(item.Content), policy.ActiveUserMarker)) {
			out = append(out, item)
			if item.Role == "user" {
				goal = true
			}
		}
	}
	return out
}

func SizeBytes(items []Item) int {
	b, _ := json.Marshal(items)
	return len(b)
}
