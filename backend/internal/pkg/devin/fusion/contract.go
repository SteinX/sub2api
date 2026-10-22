package fusion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

func ParseInput(r *apicompat.ResponsesRequest) ([]apicompat.ResponsesInputItem, error) {
	if r == nil {
		return nil, E("input_invalid", "missing request", 400)
	}
	var text string
	var items []apicompat.ResponsesInputItem
	if json.Unmarshal(r.Input, &text) == nil {
		b, _ := json.Marshal(text)
		items = []apicompat.ResponsesInputItem{{Type: "message", Role: "user", Content: b}}
	} else if json.Unmarshal(r.Input, &items) != nil {
		return nil, E("input_invalid", "input must be text or a message array", 400)
	}
	out := make([]apicompat.ResponsesInputItem, 0, len(items))
	for _, i := range items {
		if i.Type == "reasoning" {
			continue
		} // Public reasoning is never a private lane checkpoint.
		i.ID = ""
		i.EncryptedContent = ""
		switch i.Type {
		case "function_call":
			if i.CallID == "" || i.Name == "" || !json.Valid([]byte(i.Arguments)) {
				return nil, E("input_invalid", "invalid tool call history", 400)
			}
			var args any
			_ = json.Unmarshal([]byte(i.Arguments), &args)
			b, _ := json.Marshal(args)
			i.Arguments = string(b)
		case "function_call_output":
			if i.CallID == "" {
				return nil, E("input_invalid", "tool result has no call ID", 400)
			}
		case "", "message":
			i.Type = "message"
			if i.Role != "user" && i.Role != "assistant" && i.Role != "system" && i.Role != "developer" {
				return nil, E("input_invalid", "unsupported message role", 400)
			}
			var t string
			if json.Unmarshal(i.Content, &t) == nil {
				i.Content, _ = json.Marshal([]map[string]any{{"type": "text", "text": t}})
			} else {
				var parts []map[string]any
				if json.Unmarshal(i.Content, &parts) != nil {
					return nil, E("input_invalid", "invalid message content", 400)
				}
				for _, p := range parts {
					if p["type"] == "input_text" || p["type"] == "output_text" {
						p["type"] = "text"
					}
					delete(p, "prompt_cache_breakpoint")
				}
				i.Content, _ = json.Marshal(parts)
			}
		default:
			return nil, E("input_invalid", "unsupported Fusion input item: "+i.Type, 400)
		}
		out = append(out, i)
	}
	if len(out) == 0 {
		return nil, E("input_invalid", "empty input", 400)
	}
	return out, nil
}

func contract(r *apicompat.ResponsesRequest) (string, error) {
	if r.Store != nil && *r.Store {
		return "", E("input_invalid", "Fusion stores scoped session checkpoints; store:true and public response retrieval are unsupported", 400)
	}
	seen := map[string]bool{}
	for _, t := range r.Tools {
		if t.Type != "function" || t.Name == "" || t.Name == DelegateTool || seen[t.Name] || len(t.Name) > 128 {
			return "", E("tools_unsupported", "Fusion accepts distinct client function tools; sub2api_sidekick is reserved", 400)
		}
		if len(t.Parameters) > 0 && !json.Valid(t.Parameters) {
			return "", E("input_invalid", "invalid tool schema", 400)
		}
		seen[t.Name] = true
	}
	choice := strings.TrimSpace(string(r.ToolChoice))
	if choice != "" && choice != "null" && choice != `"auto"` && choice != `"none"` {
		return "", E("tools_unsupported", "Fusion supports auto or none tool choice", 400)
	}
	if r.Text != nil && r.Text.Format != nil { // Internal delegation/report must remain textual.
		b, _ := json.Marshal(r.Text.Format)
		if !bytes.Contains(b, []byte(`"text"`)) {
			return "", E("input_invalid", "structured final output is not supported by this Fusion policy", 400)
		}
	}
	return Digest(struct {
		Instructions string
		Tools        []apicompat.ResponsesTool
		Choice       string
	}{r.Instructions, r.Tools, choice}), nil
}
func publicItems(r *apicompat.ResponsesResponse) []apicompat.ResponsesInputItem {
	var out []apicompat.ResponsesInputItem
	for _, o := range r.Output {
		switch o.Type {
		case "message":
			var parts []map[string]any
			for _, p := range o.Content {
				if p.Type == "output_text" {
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				}
			}
			b, _ := json.Marshal(parts)
			out = append(out, apicompat.ResponsesInputItem{Type: "message", Role: "assistant", Content: b})
		case "function_call":
			var args any
			_ = json.Unmarshal([]byte(o.Arguments), &args)
			b, _ := json.Marshal(args)
			out = append(out, apicompat.ResponsesInputItem{Type: o.Type, CallID: o.CallID, Name: o.Name, Arguments: string(b)})
		}
	}
	return out
}
func laneItems(r *apicompat.ResponsesResponse) []apicompat.ResponsesInputItem {
	var out []apicompat.ResponsesInputItem
	for _, o := range r.Output {
		switch o.Type {
		case "reasoning":
			out = append(out, apicompat.ResponsesInputItem{Type: "reasoning", EncryptedContent: o.EncryptedContent})
		case "function_call":
			out = append(out, apicompat.ResponsesInputItem{Type: o.Type, CallID: o.CallID, Name: o.Name, Arguments: o.Arguments})
		case "message":
			b, _ := json.Marshal(o.Content)
			out = append(out, apicompat.ResponsesInputItem{Type: "message", Role: "assistant", Content: b})
		}
	}
	return out
}
func textMessage(role, text string) apicompat.ResponsesInputItem {
	b, _ := json.Marshal(text)
	return apicompat.ResponsesInputItem{Type: "message", Role: role, Content: b}
}
func outputText(r *apicompat.ResponsesResponse) string {
	var b strings.Builder
	for _, o := range r.Output {
		if o.Type == "message" {
			for _, p := range o.Content {
				if p.Type == "output_text" {
					_, _ = b.WriteString(p.Text)
				}
			}
		}
	}
	return b.String()
}
func delta(s *Session, items []apicompat.ResponsesInputItem, hasLineage bool) ([]apicompat.ResponsesInputItem, error) {
	if len(items) >= len(s.Public) && len(s.Public) > 0 && Digest(items[:len(s.Public)]) == Digest(s.Public) {
		return items[len(s.Public):], nil
	}
	// Incremental requests are accepted only with scoped session/response/tool lineage.
	if hasLineage {
		for _, i := range items {
			if i.Type == "function_call" || i.Role == "assistant" || i.Role == "system" || i.Role == "developer" {
				return nil, E("history_conflict", "public transcript was rewritten, truncated or forked", 409)
			}
		}
		return items, nil
	}
	return nil, E("history_conflict", fmt.Sprintf("public history does not match session revision %d", s.Revision), 409)
}
