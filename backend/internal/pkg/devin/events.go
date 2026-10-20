package devin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/google/uuid"
)

// EventDecoder converts Cascade deltas into the application's existing Responses events.
type EventDecoder struct {
	Response        apicompat.ResponsesResponse
	emit            func(apicompat.ResponsesStreamEvent) error
	seq             int
	active          int
	kind            string
	calls           map[string]int
	activeCall      string
	thinking        string
	signature       string
	sealedSignature string
	messageID       string
	stop            pb.StopReason
	Started         bool
	ActualModel     string
	BillingModel    string
}

func NewEventDecoder(model string, emit func(apicompat.ResponsesStreamEvent) error) *EventDecoder {
	return &EventDecoder{Response: apicompat.ResponsesResponse{ID: "resp_" + uuid.NewString(), Object: "response", CreatedAt: time.Now().Unix(), Model: model, Status: "in_progress", Output: []apicompat.ResponsesOutput{}}, emit: emit, active: -1, calls: map[string]int{}}
}
func (d *EventDecoder) event(e apicompat.ResponsesStreamEvent) error {
	e.SequenceNumber = d.seq
	d.seq++
	return d.emit(e)
}
func (d *EventDecoder) start() error {
	if d.Started {
		return nil
	}
	d.Started = true
	r := d.Response
	return d.event(apicompat.ResponsesStreamEvent{Type: "response.created", Response: &r})
}
func (d *EventDecoder) block(kind string) error {
	if d.kind == kind {
		return nil
	}
	if err := d.closeBlock(); err != nil {
		return err
	}
	d.active = len(d.Response.Output)
	d.kind = kind
	item := apicompat.ResponsesOutput{ID: kind + "_" + uuid.NewString(), Type: kind, Status: "in_progress"}
	if kind == "message" {
		item.Role = "assistant"
		item.Content = []apicompat.ResponsesContentPart{{Type: "output_text", Text: ""}}
	} else {
		item.Summary = []apicompat.ResponsesSummary{{Type: "summary_text", Text: ""}}
	}
	d.Response.Output = append(d.Response.Output, item)
	if err := d.event(apicompat.ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: d.active, Item: &item}); err != nil {
		return err
	}
	if kind == "message" {
		return d.event(apicompat.ResponsesStreamEvent{Type: "response.content_part.added", OutputIndex: d.active, ItemID: item.ID, Part: &apicompat.ResponsesContentPart{Type: "output_text", Text: ""}})
	}
	return nil
}
func (d *EventDecoder) closeBlock() error {
	if d.active < 0 {
		return nil
	}
	item := &d.Response.Output[d.active]
	item.Status = "completed"
	if item.Type == "message" {
		if err := d.event(apicompat.ResponsesStreamEvent{Type: "response.output_text.done", OutputIndex: d.active, ItemID: item.ID, Text: item.Content[0].Text}); err != nil {
			return err
		}
		if err := d.event(apicompat.ResponsesStreamEvent{Type: "response.content_part.done", OutputIndex: d.active, ItemID: item.ID, Part: &item.Content[0]}); err != nil {
			return err
		}
	}
	if item.Type == "reasoning" {
		b, _ := json.Marshal(reasoningReplay{d.thinking, d.signature, d.messageID})
		item.EncryptedContent = signaturePrefix + base64.RawStdEncoding.EncodeToString(b)
		d.sealedSignature = d.signature
	}
	err := d.event(apicompat.ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: d.active, Item: item})
	d.active = -1
	d.kind = ""
	return err
}
func (d *EventDecoder) Apply(m *pb.GetChatMessageResponse) error {
	if m.MessageId != "" {
		d.messageID = m.MessageId
	}
	if m.GetActualModelUid() != "" {
		d.ActualModel = m.GetActualModelUid()
	}
	if m.StopReason != pb.StopReason_STOP_REASON_UNSPECIFIED {
		d.stop = m.StopReason
	}
	if m.Usage != nil {
		u := m.Usage
		if model := strings.TrimSpace(u.BillingModelUid); model != "" {
			d.BillingModel = model
		}
		d.Response.Usage = &apicompat.ResponsesUsage{InputTokens: int(u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens), OutputTokens: int(u.OutputTokens), TotalTokens: int(u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens), CacheCreationInputTokens: int(u.CacheWriteTokens), InputTokensDetails: &apicompat.ResponsesInputTokensDetails{CachedTokens: int(u.CacheReadTokens), CacheCreationTokens: int(u.CacheWriteTokens)}}
	}
	if m.DeltaText == "" && m.DeltaThinking == "" && m.DeltaSignature == "" && len(m.DeltaToolCalls) == 0 {
		return nil
	}
	if err := d.start(); err != nil {
		return err
	}
	if m.DeltaThinking != "" {
		if err := d.block("reasoning"); err != nil {
			return err
		}
		d.thinking += m.DeltaThinking
		d.Response.Output[d.active].Summary[0].Text += m.DeltaThinking
		if err := d.event(apicompat.ResponsesStreamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: d.active, ItemID: d.Response.Output[d.active].ID, Delta: m.DeltaThinking}); err != nil {
			return err
		}
	}
	d.signature += m.DeltaSignature
	if m.DeltaText != "" {
		if err := d.block("message"); err != nil {
			return err
		}
		d.Response.Output[d.active].Content[0].Text += m.DeltaText
		if err := d.event(apicompat.ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: d.active, ItemID: d.Response.Output[d.active].ID, Delta: m.DeltaText}); err != nil {
			return err
		}
	}
	for _, call := range m.DeltaToolCalls {
		if err := d.closeBlock(); err != nil {
			return err
		}
		id := call.Id
		if id == "" {
			id = d.activeCall
		}
		if id == "" {
			return fmt.Errorf("Devin tool argument delta has no call")
		}
		index, ok := d.calls[id]
		if !ok {
			if call.Name == "" {
				return fmt.Errorf("Devin tool call has no name")
			}
			index = len(d.Response.Output)
			d.calls[id] = index
			d.Response.Output = append(d.Response.Output, apicompat.ResponsesOutput{Type: "function_call", ID: "fc_" + uuid.NewString(), CallID: id, Name: call.Name, Arguments: "", Status: "in_progress"})
			item := d.Response.Output[index]
			if err := d.event(apicompat.ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: index, Item: &item}); err != nil {
				return err
			}
		}
		d.activeCall = id
		d.Response.Output[index].Arguments += call.ArgumentsJson
		if call.ArgumentsJson != "" {
			if err := d.event(apicompat.ResponsesStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: index, ItemID: d.Response.Output[index].ID, CallID: id, Delta: call.ArgumentsJson}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Finish is called only after a successful Connect trailer, never on bare EOF.
func (d *EventDecoder) Finish() error {
	if d.stop == pb.StopReason_STOP_REASON_ERROR || d.stop == pb.StopReason_STOP_REASON_CONTENT_FILTER {
		return &Error{Code: "permission_denied"}
	}
	if !d.Started {
		return fmt.Errorf("Devin returned no content")
	}
	if err := d.closeBlock(); err != nil {
		return err
	}
	// Signatures may arrive after text/tool output. Preserve them in a final replay block.
	if d.signature != "" && d.signature != d.sealedSignature {
		b, _ := json.Marshal(reasoningReplay{d.thinking, d.signature, d.messageID})
		item := apicompat.ResponsesOutput{Type: "reasoning", ID: "reasoning_" + uuid.NewString(), Status: "completed", Summary: []apicompat.ResponsesSummary{}, EncryptedContent: signaturePrefix + base64.RawStdEncoding.EncodeToString(b)}
		index := len(d.Response.Output)
		d.Response.Output = append(d.Response.Output, item)
		if err := d.event(apicompat.ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: index, Item: &item}); err != nil {
			return err
		}
		if err := d.event(apicompat.ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: index, Item: &item}); err != nil {
			return err
		}
	}
	for i := range d.Response.Output {
		item := &d.Response.Output[i]
		if item.Type != "function_call" {
			continue
		}
		if !json.Valid([]byte(item.Arguments)) {
			return fmt.Errorf("Devin returned incomplete tool arguments")
		}
		item.Status = "completed"
		if err := d.event(apicompat.ResponsesStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: i, ItemID: item.ID, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments}); err != nil {
			return err
		}
		if err := d.event(apicompat.ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: i, Item: item}); err != nil {
			return err
		}
	}
	d.Response.Status = "completed"
	typ := "response.completed"
	if d.stop == pb.StopReason_STOP_REASON_MAX_TOKENS || d.stop == pb.StopReason_STOP_REASON_PARTIAL || d.stop == pb.StopReason_STOP_REASON_INCOMPLETE {
		d.Response.Status = "incomplete"
		d.Response.IncompleteDetails = &apicompat.ResponsesIncompleteDetails{Reason: "max_output_tokens"}
		typ = "response.incomplete"
	}
	r := d.Response
	return d.event(apicompat.ResponsesStreamEvent{Type: typ, Response: &r})
}
