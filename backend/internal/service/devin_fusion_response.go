package service

import (
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// One buffered public lifecycle, independent of the number of private role calls.
func devinFusionResponseEvents(response *apicompat.ResponsesResponse) []apicompat.ResponsesStreamEvent {
	start := *response
	start.Status = "in_progress"
	start.Output = []apicompat.ResponsesOutput{}
	start.Usage = nil
	events := []apicompat.ResponsesStreamEvent{{Type: "response.created", Response: &start}, {Type: "response.in_progress", Response: &start}}
	for i, o := range response.Output {
		initial := o
		initial.Status = "in_progress"
		initial.Arguments = ""
		initial.Content = nil
		events = append(events, apicompat.ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: i, Item: &initial})
		switch o.Type {
		case "reasoning":
			for _, summary := range o.Summary {
				if summary.Text != "" {
					events = append(events, apicompat.ResponsesStreamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: i, ItemID: o.ID, Delta: summary.Text})
				}
			}
		case "function_call":
			events = append(events, apicompat.ResponsesStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: i, ItemID: o.ID, CallID: o.CallID, Delta: o.Arguments}, apicompat.ResponsesStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: i, ItemID: o.ID, CallID: o.CallID, Name: o.Name, Arguments: o.Arguments})
		case "message":
			for j, p := range o.Content {
				empty := p
				empty.Text = ""
				part := p
				events = append(events, apicompat.ResponsesStreamEvent{Type: "response.content_part.added", OutputIndex: i, ContentIndex: j, ItemID: o.ID, Part: &empty}, apicompat.ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: i, ContentIndex: j, ItemID: o.ID, Delta: p.Text}, apicompat.ResponsesStreamEvent{Type: "response.output_text.done", OutputIndex: i, ContentIndex: j, ItemID: o.ID, Text: p.Text}, apicompat.ResponsesStreamEvent{Type: "response.content_part.done", OutputIndex: i, ContentIndex: j, ItemID: o.ID, Part: &part})
			}
		}
		item := o
		events = append(events, apicompat.ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: i, Item: &item})
	}
	events = append(events, apicompat.ResponsesStreamEvent{Type: "response.completed", Response: response})
	for i := range events {
		events[i].SequenceNumber = i
	}
	return events
}
func writeDevinFusionResponse(c *gin.Context, r *apicompat.ResponsesResponse, protocol string, stream bool, mapping apicompat.ResponsesClientToolMapping, includeUsage ...bool) error {
	if !stream {
		switch protocol {
		case "chat":
			response := apicompat.ResponsesToChatCompletions(r, r.Model)
			response.Choices[0].Message.ReasoningSignature = devinChatReasoningSignature(r)
			c.JSON(200, response)
		case "anthropic":
			c.JSON(200, apicompat.ResponsesToAnthropic(r, r.Model))
		default:
			b, err := json.Marshal(r)
			if err != nil {
				return err
			}
			b, _, err = apicompat.RestoreResponsesClientToolPayload(b, mapping)
			if err != nil {
				return err
			}
			c.Data(200, "application/json", b)
		}
		return nil
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	anth := apicompat.NewResponsesEventToAnthropicState()
	chat := apicompat.NewResponsesEventToChatState()
	chat.IncludeUsage = len(includeUsage) > 0 && includeUsage[0]
	restorer := apicompat.NewResponsesClientToolStreamRestorer(mapping)
	write := func(s string) error {
		if c.Request.Context().Err() != nil {
			return c.Request.Context().Err()
		}
		if _, err := c.Writer.WriteString(s); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	}
	for _, e := range devinFusionResponseEvents(r) {
		switch protocol {
		case "chat":
			if e.Type == "response.completed" {
				if signature := devinChatReasoningSignature(r); signature != "" {
					chunk := apicompat.ChatCompletionsChunk{ID: chat.ID, Object: "chat.completion.chunk", Created: chat.Created, Model: r.Model,
						Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: apicompat.ChatDelta{ReasoningSignature: &signature}}}}
					data, err := apicompat.ChatChunkToSSE(chunk)
					if err != nil {
						return err
					}
					if err = write(data); err != nil {
						return err
					}
				}
			}
			for _, chunk := range apicompat.ResponsesEventToChatChunks(&e, chat) {
				data, err := apicompat.ChatChunkToSSE(chunk)
				if err != nil {
					return err
				}
				if err = write(data); err != nil {
					return err
				}
			}
		case "anthropic":
			for _, evt := range apicompat.ResponsesEventToAnthropicEvents(&e, anth) {
				data, err := apicompat.ResponsesAnthropicEventToSSE(evt)
				if err != nil {
					return err
				}
				if err = write(data); err != nil {
					return err
				}
			}
		default:
			for _, evt := range restorer.Restore(e) {
				b, err := json.Marshal(evt)
				if err != nil {
					return err
				}
				if err = write(fmt.Sprintf("event: %s\ndata: %s\n\n", evt.Type, b)); err != nil {
					return err
				}
			}
		}
	}
	if protocol == "chat" {
		return write("data: [DONE]\n\n")
	}
	return nil
}
