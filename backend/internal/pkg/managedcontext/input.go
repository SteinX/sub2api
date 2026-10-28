package managedcontext

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	cc "github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
)

type Parsed struct {
	Request                                          *apicompat.ResponsesRequest
	Items                                            []cc.Item
	InstructionsPresent, ToolsPresent, ChoicePresent bool
}

func Parse(body []byte, stateless bool) (Parsed, error) {
	var raw map[string]json.RawMessage
	var r apicompat.ResponsesRequest
	if json.Unmarshal(body, &raw) != nil || json.Unmarshal(body, &r) != nil {
		return Parsed{}, E("input_invalid", "request must be a JSON object", 400)
	}
	if r.Model == "" {
		return Parsed{}, E("input_invalid", "model is required", 400)
	}
	if r.Store != nil && *r.Store {
		return Parsed{}, E("input_invalid", "managed context does not support store:true", 400)
	}
	if r.Text != nil && r.Text.Format != nil {
		return Parsed{}, E("input_invalid", "managed context currently supports plain text output", 400)
	}
	if len(body) > 2<<20 {
		return Parsed{}, E("input_invalid", "managed request exceeds 2 MiB", 413)
	}
	if stateless && (r.PreviousResponseID != "" || r.Stream) {
		return Parsed{}, E("input_invalid", "stateless compact requires complete input and stream:false", 400)
	}
	if value := raw["service_tier"]; len(value) > 0 && string(value) != "null" && string(value) != `"auto"` {
		return Parsed{}, E("input_invalid", "managed context supports service_tier:auto only", 400)
	}
	if len(raw["compaction_trigger"]) > 0 {
		return Parsed{}, E("input_invalid", "native compaction items are not supported", 400)
	}
	// These options have stateful semantics that this policy does not emulate.
	for _, name := range []string{"background", "conversation", "context_management", "truncation"} {
		if value := raw[name]; len(value) > 0 && string(value) != "null" && string(value) != "false" {
			return Parsed{}, E("input_invalid", "unsupported managed option: "+name, 400)
		}
	}
	var text string
	var items []cc.Item
	if json.Unmarshal(r.Input, &text) == nil {
		b, _ := json.Marshal(text)
		items = []cc.Item{{Type: "message", Role: "user", Content: b}}
	} else if json.Unmarshal(r.Input, &items) != nil {
		return Parsed{}, E("input_invalid", "input must be text or a message array", 400)
	}
	if len(items) == 0 {
		return Parsed{}, E("input_invalid", "input is empty", 400)
	}
	open := map[string]bool{}
	for n := range items {
		i := &items[n]
		i.ID = ""
		i.EncryptedContent = ""
		switch i.Type {
		case "", "message":
			i.Type = "message"
			if i.Role != "user" && i.Role != "assistant" && i.Role != "system" && i.Role != "developer" {
				return Parsed{}, E("input_invalid", "unsupported message role", 400)
			}
			var value string
			if json.Unmarshal(i.Content, &value) == nil {
				i.Content, _ = json.Marshal(value)
			} else {
				var parts []apicompat.ResponsesContentPart
				if json.Unmarshal(i.Content, &parts) != nil || len(parts) == 0 {
					return Parsed{}, E("input_invalid", "invalid text content", 400)
				}
				for _, p := range parts {
					if p.Type != "input_text" && p.Type != "output_text" && p.Type != "text" {
						return Parsed{}, E("input_invalid", "managed contexts currently accept text only", 400)
					}
				}
				i.Content, _ = json.Marshal(parts)
			}
		case "function_call":
			if !stateless {
				return Parsed{}, E("input_invalid", "managed input cannot import tool calls", 400)
			}
			if i.CallID == "" || i.Name == "" || !json.Valid([]byte(i.Arguments)) || open[i.CallID] {
				return Parsed{}, E("input_invalid", "invalid historical tool call", 400)
			}
			open[i.CallID] = true
		case "function_call_output":
			if i.CallID == "" {
				return Parsed{}, E("input_invalid", "tool result has no call ID", 400)
			}
			if stateless {
				if !open[i.CallID] {
					return Parsed{}, E("input_invalid", "orphan historical tool result", 400)
				}
				delete(open, i.CallID)
			}
		default:
			return Parsed{}, E("input_invalid", "unsupported managed input item: "+i.Type, 400)
		}
	}
	if stateless && len(open) > 0 {
		return Parsed{}, E("tool_pending", "stateless compaction requires complete historical tool groups", 409)
	}
	seen := map[string]bool{}
	for _, tool := range r.Tools {
		if tool.Type != "function" || tool.Name == "" || seen[tool.Name] || len(tool.Name) > 128 || len(tool.Parameters) > 0 && !json.Valid(tool.Parameters) {
			return Parsed{}, E("input_invalid", "managed contexts accept distinct function tools", 400)
		}
		seen[tool.Name] = true
	}
	choice := strings.TrimSpace(string(r.ToolChoice))
	if choice != "" && choice != "null" && choice != `"auto"` && choice != `"none"` {
		return Parsed{}, E("input_invalid", "managed tool_choice must be auto or none", 400)
	}
	parallel := false
	r.ParallelToolCalls = &parallel
	_, instructions := raw["instructions"]
	_, tools := raw["tools"]
	_, toolChoice := raw["tool_choice"]
	return Parsed{Request: &r, Items: items, InstructionsPresent: instructions, ToolsPresent: tools, ChoicePresent: toolChoice}, nil
}
func contract(r *apicompat.ResponsesRequest) string {
	return cc.Digest(struct {
		Instructions string
		Tools        []apicompat.ResponsesTool
		Choice       string
	}{r.Instructions, r.Tools, strings.TrimSpace(string(r.ToolChoice))})
}
func Inherit(parsed Parsed, s *Session) (*apicompat.ResponsesRequest, error) {
	r := *parsed.Request
	if !parsed.InstructionsPresent {
		r.Instructions = s.Template.Instructions
	}
	if !parsed.ToolsPresent {
		r.Tools = s.Template.Tools
	}
	if !parsed.ChoicePresent {
		r.ToolChoice = s.Template.ToolChoice
	}
	if contract(&r) != s.Contract {
		return nil, E("contract_changed", "instructions or tools changed; start a new managed session", 409)
	}
	return &r, nil
}
func outputItems(r *apicompat.ResponsesResponse) ([]cc.Item, error) {
	var out []cc.Item
	calls := 0
	for _, o := range r.Output {
		switch o.Type {
		case "message":
			b, _ := json.Marshal(o.Content)
			out = append(out, cc.Item{Type: "message", Role: "assistant", Content: b})
		case "reasoning":
			out = append(out, cc.Item{Type: "reasoning", EncryptedContent: o.EncryptedContent})
		case "function_call":
			calls++
			if calls > 1 || o.CallID == "" || o.Name == "" || !json.Valid([]byte(o.Arguments)) {
				return nil, E("tool_contract", "provider returned an unsupported tool group", 502)
			}
			out = append(out, cc.Item{Type: "function_call", CallID: o.CallID, Name: o.Name, Arguments: o.Arguments})
		default:
			return nil, E("tool_contract", "provider returned an unsupported output item", 502)
		}
	}
	if r.Status != "completed" {
		return nil, E("provider_failed", "provider did not complete the response", 502)
	}
	return out, nil
}
