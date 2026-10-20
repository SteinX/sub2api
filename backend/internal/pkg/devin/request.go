package devin

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const signaturePrefix = "devin_v1:"

type reasoningReplay struct {
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	MessageID string `json:"message_id"`
}

// BuildRequest carries complete history; scope must include the downstream tenant.
func BuildRequest(r *apicompat.ResponsesRequest, scope string) (*pb.GetChatMessageRequest, error) {
	if strings.TrimSpace(r.Model) == "" {
		return nil, fmt.Errorf("model is required")
	}
	if r.PreviousResponseID != "" {
		return nil, fmt.Errorf("devin requires complete input history; previous_response_id is unsupported")
	}
	if r.Store != nil && *r.Store {
		return nil, fmt.Errorf("devin does not support persistent Responses storage")
	}
	if r.Text != nil && len(r.Text.Format) > 0 {
		var f struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(r.Text.Format, &f) != nil || (f.Type != "" && f.Type != "text") {
			return nil, fmt.Errorf("devin does not support structured response_format")
		}
	}
	cascade := uuid.NewSHA1(uuid.NameSpaceOID, []byte(scope+"\x00"+r.Model)).String()
	maxTokens := uint64(64000)
	if r.MaxOutputTokens != nil {
		if *r.MaxOutputTokens <= 0 {
			return nil, fmt.Errorf("max_output_tokens must be positive")
		}
		maxTokens = uint64(*r.MaxOutputTokens)
	}
	temp := 0.4
	if r.Temperature != nil {
		temp = *r.Temperature
	}
	if temp < 0 {
		return nil, fmt.Errorf("temperature must not be negative")
	}
	if temp == 0 {
		temp = 0.0001
	}
	q := &pb.GetChatMessageRequest{Prompt: r.Instructions, ChatModelUid: r.Model, CascadeId: cascade, ExecutionId: uuid.NewString(), RequestType: pb.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE, PlannerMode: pb.ConversationalPlannerMode_CONVERSATIONAL_PLANNER_MODE_DEFAULT, SystemPromptCacheOptions: &pb.PromptCacheOptions{Type: pb.CacheControlType_CACHE_CONTROL_TYPE_EPHEMERAL}, Configuration: &pb.CompletionConfiguration{NumCompletions: 1, MaxTokens: maxTokens, Temperature: temp, TopP: 1, StopPatterns: []string{"<|user|>", "<|bot|>", "<|context_request|>", "<|endoftext|>", "<|end_of_turn|>"}}}
	if r.TopP != nil {
		q.Configuration.TopP = *r.TopP
	}
	if r.ParallelToolCalls != nil {
		q.DisableParallelToolCalls = !*r.ParallelToolCalls
	}
	var input []apicompat.ResponsesInputItem
	var plain string
	if json.Unmarshal(r.Input, &plain) == nil {
		raw, _ := json.Marshal(plain)
		input = []apicompat.ResponsesInputItem{{Role: "user", Content: raw}}
	} else if err := json.Unmarshal(r.Input, &input); err != nil {
		return nil, fmt.Errorf("input must be text or an item array")
	}
	for i, item := range input {
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s:%d", cascade, i))).String()
		p := &pb.ChatMessagePrompt{MessageId: id, PromptCacheOptions: &pb.PromptCacheOptions{Type: pb.CacheControlType_CACHE_CONTROL_TYPE_EPHEMERAL}}
		switch item.Type {
		case "function_call":
			if item.CallID == "" || item.Name == "" || !json.Valid([]byte(item.Arguments)) {
				return nil, fmt.Errorf("invalid function_call history")
			}
			p.Source = pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM
			p.ToolCalls = []*pb.ChatToolCall{{Id: item.CallID, Name: item.Name, ArgumentsJson: item.Arguments}}
		case "function_call_output":
			p.Source = pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL
			p.ToolCallId = item.CallID
			p.Prompt = item.Output
		case "reasoning":
			if !strings.HasPrefix(item.EncryptedContent, signaturePrefix) {
				continue
			}
			raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(item.EncryptedContent, signaturePrefix))
			if err != nil {
				return nil, fmt.Errorf("invalid Devin reasoning replay")
			}
			var rr reasoningReplay
			if json.Unmarshal(raw, &rr) != nil {
				return nil, fmt.Errorf("invalid Devin reasoning replay")
			}
			p.Source = pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM
			p.Thinking = rr.Thinking
			p.Signature = rr.Signature
			if rr.MessageID != "" {
				p.MessageId = rr.MessageID
			}
		case "", "message":
			text, images, err := inputContent(item.Content)
			if err != nil {
				return nil, err
			}
			switch item.Role {
			case "system", "developer":
				q.Prompt += "\n" + text
				continue
			case "user":
				p.Source = pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_USER
			case "assistant":
				p.Source = pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM
			default:
				return nil, fmt.Errorf("unsupported message role %q", item.Role)
			}
			p.Prompt = text
			p.Images = images
		default:
			return nil, fmt.Errorf("unsupported Devin input item %q", item.Type)
		}
		q.ChatMessagePrompts = append(q.ChatMessagePrompts, p)
	}
	// Cascade pairs calls/results in order. Keep each call immediately before its result.
	paired, err := pairTools(q.ChatMessagePrompts)
	if err != nil {
		return nil, err
	}
	q.ChatMessagePrompts = paired
	seen := map[string]bool{}
	for _, t := range r.Tools {
		if t.Type != "function" {
			return nil, fmt.Errorf("devin does not support tool type %q", t.Type)
		}
		if t.Name == "" || seen[t.Name] {
			return nil, fmt.Errorf("tool names must be nonempty and unique")
		}
		seen[t.Name] = true
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		if !json.Valid(schema) {
			return nil, fmt.Errorf("invalid tool schema")
		}
		q.Tools = append(q.Tools, &pb.ChatToolDefinition{Name: t.Name, Description: t.Description, JsonSchemaString: string(schema)})
	}
	q.ToolChoice = &pb.ChatToolChoice{Choice: &pb.ChatToolChoice_OptionName{OptionName: "auto"}}
	if len(r.ToolChoice) > 0 {
		var choice string
		if json.Unmarshal(r.ToolChoice, &choice) == nil {
			if choice != "auto" && choice != "none" && choice != "required" {
				return nil, fmt.Errorf("invalid tool_choice")
			}
			q.ToolChoice.Choice = &pb.ChatToolChoice_OptionName{OptionName: choice}
		} else {
			var c struct{ Type, Name string }
			if json.Unmarshal(r.ToolChoice, &c) != nil || c.Type != "function" || !seen[c.Name] {
				return nil, fmt.Errorf("invalid named tool_choice")
			}
			q.ToolChoice.Choice = &pb.ChatToolChoice_ToolName{ToolName: c.Name}
		}
	}
	return q, nil
}

func inputContent(raw json.RawMessage) (string, []*pb.ImageData, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil, nil
	}
	var parts []apicompat.ResponsesContentPart
	if json.Unmarshal(raw, &parts) != nil {
		return "", nil, fmt.Errorf("invalid message content")
	}
	var text strings.Builder
	var images []*pb.ImageData
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			_, _ = text.WriteString(p.Text)
		case "input_image":
			meta, data, ok := strings.Cut(p.ImageURL, ",")
			if !ok || !strings.HasPrefix(meta, "data:image/") || !strings.HasSuffix(meta, ";base64") {
				return "", nil, fmt.Errorf("devin images require a base64 data URL")
			}
			if _, err := base64.StdEncoding.DecodeString(data); err != nil {
				return "", nil, fmt.Errorf("invalid image data")
			}
			images = append(images, &pb.ImageData{Base64Data: data, MimeType: strings.TrimSuffix(strings.TrimPrefix(meta, "data:"), ";base64")})
		default:
			return "", nil, fmt.Errorf("unsupported Devin content %q", p.Type)
		}
	}
	return text.String(), images, nil
}

func pairTools(in []*pb.ChatMessagePrompt) ([]*pb.ChatMessagePrompt, error) {
	results := map[string]*pb.ChatMessagePrompt{}
	calls := map[string]bool{}
	for _, p := range in {
		if p.Source == pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL {
			if p.ToolCallId == "" || !calls[p.ToolCallId] {
				return nil, fmt.Errorf("tool result requires a preceding function_call with the same call_id")
			}
			if _, exists := results[p.ToolCallId]; exists {
				return nil, fmt.Errorf("duplicate tool result call_id")
			}
			results[p.ToolCallId] = p
			continue
		}
		for _, call := range p.ToolCalls {
			if call == nil || call.Id == "" || calls[call.Id] {
				return nil, fmt.Errorf("function_call IDs must be nonempty and unique")
			}
			calls[call.Id] = true
		}
	}
	for id := range calls {
		if _, exists := results[id]; !exists {
			return nil, fmt.Errorf("function_call requires a matching tool result")
		}
	}
	var out []*pb.ChatMessagePrompt
	for _, p := range in {
		if p.Source == pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL {
			continue
		}
		if len(p.ToolCalls) == 0 {
			out = append(out, p)
			continue
		}
		for _, call := range p.ToolCalls {
			cp, ok := proto.Clone(p).(*pb.ChatMessagePrompt)
			if !ok {
				return nil, fmt.Errorf("could not copy Devin tool history")
			}
			cp.ToolCalls = []*pb.ChatToolCall{call}
			out = append(out, cp)
			if result, ok := results[call.Id]; ok {
				out = append(out, result)
			}
		}
	}
	return out, nil
}

// CredentialFingerprint invalidates runtime caches when credentials or proxy change.
func CredentialFingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%x", sum[:])
}
