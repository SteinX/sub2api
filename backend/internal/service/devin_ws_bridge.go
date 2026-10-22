package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const devinWSRequestStateKey = "devin_ws_request_state"
const devinWSCascadeScopeKey = "devin_ws_cascade_scope"
const devinWSReplayToolsKey = "devin_ws_replay_tools"

// BindDevinFusionWebSocketRequest performs the same scoped binding as HTTP
// before scheduling. Control replies must use WS frames after the upgrade.
func (s *OpenAIGatewayService) BindDevinFusionWebSocketRequest(c *gin.Context, key *APIKey, model string, body []byte, updater APIKeyQuotaUpdater, write func([]byte) error) (bool, error) {
	writer := &devinWSResponseWriter{header: make(http.Header), status: http.StatusOK, size: -1, captureOnly: true}
	bridge := c.Copy()
	bridge.Writer = writer
	bound, err := s.BindDevinFusionRequest(bridge, key, model, body, "responses", updater)
	c.Request = bridge.Request
	if DevinFusionHandled(err) {
		var control map[string]any
		if decodeErr := json.Unmarshal(writer.pending.Bytes(), &control); decodeErr != nil {
			return bound, decodeErr
		}
		control["type"] = "fusion.session"
		message, marshalErr := json.Marshal(control)
		if marshalErr != nil {
			return bound, marshalErr
		}
		if writeErr := write(message); writeErr != nil {
			return bound, writeErr
		}
	}
	return bound, err
}

// Reuse the HTTP adapter for each WS turn. The shared WS ingress owns admission,
// usage, cancellation and connection-local transcript replay.
func (s *OpenAIGatewayService) proxyDevinWSBridgeTurn(ctx context.Context, c *gin.Context, account *Account, payload []byte, write func([]byte) error) (*OpenAIForwardResult, error) {
	if c == nil || c.Request == nil || write == nil {
		return nil, errors.New("devin websocket bridge requires a request and writer")
	}
	body, err := prepareOpenAIWSHTTPBridgeBody(account, payload)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if DevinFusionRequest(ctx) {
		if previous := gjson.GetBytes(payload, "previous_response_id"); previous.Type == gjson.String && previous.String() != "" {
			fields["previous_response_id"] = json.RawMessage(previous.Raw)
		}
	}
	// Shared ingress represents a string input as a string transcript item.
	// Responses item arrays require explicit user messages on the native adapter.
	if input := fields["input"]; len(input) > 0 && input[0] == '[' {
		var items []json.RawMessage
		if err = json.Unmarshal(input, &items); err != nil {
			return nil, err
		}
		for i, item := range items {
			var text string
			if json.Unmarshal(item, &text) == nil {
				items[i], _ = json.Marshal(map[string]string{"role": "user", "content": text})
			}
		}
		fields["input"], err = json.Marshal(items)
		if err != nil {
			return nil, err
		}
	}
	// WebSocket declarations persist until replaced, including an explicit [].
	state, _ := c.Get(devinWSRequestStateKey)
	previous, _ := state.(map[string]json.RawMessage)
	if _, replacesTools := fields["tools"]; replacesTools {
		if _, replacesChoice := fields["tool_choice"]; !replacesChoice {
			// A replacement declaration starts a new selection policy. Inheriting
			// a named choice can refer to a tool that is no longer declared.
			fields["tool_choice"] = json.RawMessage(`"auto"`)
		}
	}
	next := make(map[string]json.RawMessage)
	for _, key := range []string{"tools", "tool_choice", "parallel_tool_calls", "instructions"} {
		if value, present := fields[key]; present {
			next[key] = append(json.RawMessage(nil), value...)
		} else if value, present := previous[key]; present {
			fields[key] = value
			next[key] = value
		}
	}
	var replayTools apicompat.ResponsesClientToolMapping
	if !DevinFusionRequest(ctx) {
		replayTools, err = prepareDevinWSReplayTools(c, fields)
		if err != nil {
			return nil, err
		}
	}
	body, err = json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	cacheKey := c.GetString(devinWSCascadeScopeKey)
	if cacheKey == "" {
		cacheKey = "websocket:" + uuid.NewString()
	}
	// Codex uses generate:false to warm the connection. A native chat call here
	// would generate and charge for a response the client never requested.
	if generate := gjson.GetBytes(payload, "generate"); generate.Type == gjson.False {
		store := false
		response := apicompat.ResponsesResponse{
			ID: "resp_" + uuid.NewString(), Object: "response", CreatedAt: time.Now().Unix(),
			Model: gjson.GetBytes(body, "model").String(), Status: "in_progress", Store: &store,
			Output: []apicompat.ResponsesOutput{},
		}
		for i, eventType := range []string{"response.created", "response.completed"} {
			if i > 0 {
				response.Status = "completed"
				response.Usage = &apicompat.ResponsesUsage{}
			}
			message, marshalErr := json.Marshal(apicompat.ResponsesStreamEvent{Type: eventType, SequenceNumber: i, Response: &response})
			if marshalErr != nil {
				return nil, marshalErr
			}
			if err := write(message); err != nil {
				return nil, err
			}
		}
		c.Set(devinWSRequestStateKey, next)
		c.Set(devinWSCascadeScopeKey, cacheKey)
		c.Set(devinWSReplayToolsKey, replayTools)
		return &OpenAIForwardResult{Model: response.Model, BillingModel: response.Model, ResponseID: response.ID, Stream: true, ResponseHeaders: make(http.Header), wsReplayInputExists: true}, nil
	}
	writer := &devinWSResponseWriter{ResponseWriter: c.Writer, header: make(http.Header), status: http.StatusOK, size: -1, writeMessage: write}
	bridge := c.Copy()
	bridge.Writer = writer
	bridge.Request = c.Request.Clone(ctx)
	result, err := s.forwardDevin(ctx, bridge, account, body, "responses", cacheKey, "")
	if writer.err != nil {
		return result, writer.err
	}
	if err != nil {
		if writer.size >= 0 && writer.status >= 400 {
			var failure map[string]any
			if json.Unmarshal(writer.pending.Bytes(), &failure) == nil {
				failure["type"] = "error"
				if message, marshalErr := json.Marshal(failure); marshalErr == nil {
					if writeErr := write(message); writeErr != nil {
						return result, writeErr
					}
				}
			}
		}
		return result, err
	}
	if result == nil || !writer.terminal {
		return result, fmt.Errorf("devin websocket response has no terminal event")
	}
	c.Set(devinWSRequestStateKey, next)
	c.Set(devinWSCascadeScopeKey, cacheKey)
	c.Set(devinWSReplayToolsKey, replayTools)
	result.wsReplayInput = writer.output
	result.wsReplayInputExists = true
	result.wsAccountFailoverReplayInput = writer.output
	result.ResponseHeaders = make(http.Header)
	return result, nil
}

// Retired tools remain in history even after an explicit tools reset. Lower
// their old call/result items without adding them to the current declarations.
func prepareDevinWSReplayTools(c *gin.Context, fields map[string]json.RawMessage) (apicompat.ResponsesClientToolMapping, error) {
	value, _ := c.Get(devinWSReplayToolsKey)
	previous, _ := value.(apicompat.ResponsesClientToolMapping)
	mapping := apicompat.ResponsesClientToolMapping{CustomTools: map[string]bool{}, NamespaceTools: map[string]apicompat.ResponsesNamespaceName{}, ToolSearch: previous.ToolSearch}
	maps.Copy(mapping.CustomTools, previous.CustomTools)
	maps.Copy(mapping.NamespaceTools, previous.NamespaceTools)
	activeToolSearch := false
	if raw, present := fields["tools"]; present {
		var tools any
		if err := decodeOpenAIJSONUseNumber(raw, &tools); err != nil {
			return mapping, err
		}
		declarations := map[string]any{"tools": tools}
		if input, present := fields["input"]; present {
			var items any
			if err := decodeOpenAIJSONUseNumber(input, &items); err != nil {
				return mapping, err
			}
			declarations["input"] = items
		}
		current, _, err := apicompat.AdaptResponsesClientTools(declarations)
		if err != nil {
			return mapping, err
		}
		maps.Copy(mapping.CustomTools, current.CustomTools)
		maps.Copy(mapping.NamespaceTools, current.NamespaceTools)
		mapping.ToolSearch = mapping.ToolSearch || current.ToolSearch
		activeToolSearch = current.ToolSearch
	}
	if raw, present := fields["input"]; present {
		var input any
		if err := decodeOpenAIJSONUseNumber(raw, &input); err != nil {
			return mapping, err
		}
		history := map[string]any{"input": input}
		replay := mapping
		// An active search declaration must still see the raw discovery output
		// in the main adapter, which promotes the discovered tools for this turn.
		replay.ToolSearch = mapping.ToolSearch && !activeToolSearch
		_, changed, err := apicompat.AdaptResponsesClientToolsWithInheritedMapping(history, replay)
		if err != nil {
			return mapping, err
		}
		if changed {
			fields["input"], err = json.Marshal(history["input"])
			if err != nil {
				return mapping, err
			}
		}
	}
	return mapping, nil
}

// Each SSE data object becomes one WS text frame. Keepalives remain transport
// details and never enter the replay transcript.
type devinWSResponseWriter struct {
	gin.ResponseWriter
	header       http.Header
	status, size int
	pending      bytes.Buffer
	writeMessage func([]byte) error
	err          error
	terminal     bool
	captureOnly  bool
	output       []json.RawMessage
}

func (w *devinWSResponseWriter) Header() http.Header { return w.header }
func (w *devinWSResponseWriter) Status() int         { return w.status }
func (w *devinWSResponseWriter) Size() int           { return w.size }
func (w *devinWSResponseWriter) Written() bool       { return w.size >= 0 }
func (w *devinWSResponseWriter) WriteHeader(status int) {
	if !w.Written() {
		w.status = status
	}
}
func (w *devinWSResponseWriter) WriteHeaderNow() {
	if w.size < 0 {
		w.size = 0
	}
}
func (w *devinWSResponseWriter) Flush() { w.WriteHeaderNow() }
func (w *devinWSResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}
func (w *devinWSResponseWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.WriteHeaderNow()
	w.size += len(data)
	_, _ = w.pending.Write(data) // bytes.Buffer.Write always returns a nil error.
	if w.captureOnly || w.status >= 400 {
		return len(data), nil
	}
	for {
		buffer := w.pending.Bytes()
		boundary := bytes.Index(buffer, []byte("\n\n"))
		if boundary < 0 {
			break
		}
		frame := append([]byte(nil), buffer[:boundary]...)
		w.pending.Next(boundary + 2)
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			message := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			var event struct {
				Type     string `json:"type"`
				Response struct {
					Output []json.RawMessage `json:"output"`
				} `json:"response"`
			}
			if err := json.Unmarshal(message, &event); err != nil {
				w.err = err
				return 0, err
			}
			if event.Type == "response.completed" || event.Type == "response.incomplete" {
				w.terminal = true
				w.output = event.Response.Output
			}
			if err := w.writeMessage(message); err != nil {
				w.err = err
				return 0, err
			}
		}
	}
	return len(data), nil
}
