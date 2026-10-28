//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDevinWebSocketToolContinuationUsesConnectionHistory(t *testing.T) {
	turn := 0
	cascadeID := ""
	gateway := devinCompatGateway(t, func(request *pb.GetChatMessageRequest) []*pb.GetChatMessageResponse {
		turn++
		if turn == 1 {
			cascadeID = request.CascadeId
		} else {
			assert.Equal(t, cascadeID, request.CascadeId, "one websocket must retain its native conversation scope")
		}
		if turn >= 3 {
			assert.Equal(t, "auto", request.ToolChoice.GetOptionName())
			if turn == 3 {
				assert.Empty(t, request.Tools)
			} else if assert.Len(t, request.Tools, 1) {
				assert.Equal(t, "lookup", request.Tools[0].Name)
			}
			return []*pb.GetChatMessageResponse{{DeltaText: "done", StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN}}
		}
		if !assert.Len(t, request.Tools, 1) {
			return nil
		}
		assert.Equal(t, "apply_patch", request.Tools[0].Name)
		if turn == 1 {
			return []*pb.GetChatMessageResponse{{DeltaToolCalls: []*pb.ChatToolCall{{Id: "call_patch", Name: "apply_patch", ArgumentsJson: `{"input":"patch"}`}}, StopReason: pb.StopReason_STOP_REASON_FUNCTION_CALL}}
		}
		var result, user string
		for _, prompt := range request.ChatMessagePrompts {
			if prompt.ToolCallId == "call_patch" {
				result = prompt.Prompt
			}
			if prompt.Source == pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_USER {
				user = prompt.Prompt
			}
		}
		assert.Equal(t, "applied", result)
		assert.Equal(t, "fix it", user)
		return []*pb.GetChatMessageResponse{{DeltaText: "done", StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN}}
	})
	gateway.cfg = &config.Config{}
	gateway.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	gateway.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	gateway.cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	gateway.cache = &stubGatewayCache{}
	account := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "test"}}
	serverDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		_, first, err := conn.Read(ctx)
		if err != nil {
			serverDone <- err
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r
		proxyErr := gateway.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "test", first, nil)
		if proxyErr != nil {
			t.Logf("Devin websocket bridge: %v", proxyErr)
		}
		serverDone <- proxyErr
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"swe-2-max","input":"fix it","tools":[{"type":"custom","name":"apply_patch"}],"tool_choice":{"type":"custom","name":"apply_patch"}}`)))
	readCompleted := func() gjson.Result {
		t.Helper()
		for {
			_, message, err := conn.Read(ctx)
			require.NoError(t, err)
			event := gjson.ParseBytes(message)
			if event.Get("type").String() == "response.completed" {
				return event.Get("response")
			}
		}
	}
	first := readCompleted()
	require.Equal(t, "custom_tool_call", first.Get("output.0.type").String())
	// The response ID must not import another connection's transcript, even
	// when both connections use the same account and model.
	foreign, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer foreign.CloseNow()
	foreignTurn, err := json.Marshal(map[string]any{"type": "response.create", "model": "swe-2-max", "previous_response_id": first.Get("id").String(), "input": "another connection"})
	require.NoError(t, err)
	require.NoError(t, foreign.Write(ctx, coderws.MessageText, foreignTurn))
	_, _, err = foreign.Read(ctx)
	require.Error(t, err)
	select {
	case failure := <-serverDone:
		require.ErrorContains(t, failure, "previous response on this connection")
	case <-ctx.Done():
		t.Fatal("cross-connection continuation did not fail")
	}
	followup, err := json.Marshal(map[string]any{
		"type": "response.create", "previous_response_id": first.Get("id").String(),
		"input": []any{map[string]any{"type": "custom_tool_call_output", "call_id": "call_patch", "output": "applied"}},
	})
	require.NoError(t, err)
	require.NoError(t, conn.Write(ctx, coderws.MessageText, followup))
	second := readCompleted()
	require.Equal(t, "done", second.Get("output.0.content.0.text").String())
	// Explicit replacement declarations must not inherit a stale named choice.
	for _, tools := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]`)} {
		replacement, err := json.Marshal(map[string]any{"type": "response.create", "previous_response_id": second.Get("id").String(), "input": "continue", "tools": tools})
		require.NoError(t, err)
		require.NoError(t, conn.Write(ctx, coderws.MessageText, replacement))
		second = readCompleted()
	}
	require.NoError(t, conn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case failure := <-serverDone:
		require.NoError(t, failure)
	case <-ctx.Done():
		t.Fatal("websocket bridge did not release the session")
	}
	require.Equal(t, 4, turn)
}

func TestDevinWebSocketWriterPreservesPartialFramesAndIgnoresKeepalives(t *testing.T) {
	var messages []string
	w := &devinWSResponseWriter{header: make(http.Header), status: 200, size: -1, writeMessage: func(body []byte) error {
		messages = append(messages, string(body))
		return nil
	}}
	_, err := w.Write([]byte(": ping\n\nevent: response.completed\ndata: {\"type\":\"response.completed\","))
	require.NoError(t, err)
	require.Empty(t, messages)
	_, err = w.Write([]byte("\"response\":{\"output\":[]}}\n\n"))
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.True(t, w.terminal)
}

func TestDevinWebSocketWarmupNeverStartsGeneration(t *testing.T) {
	gateway := devinCompatGateway(t, func(*pb.GetChatMessageRequest) []*pb.GetChatMessageResponse {
		t.Error("generate:false must not invoke native chat")
		return nil
	})
	upstreamCalls := 0
	gateway.devin.upstream.(*devinHTTP).observe = func(*http.Request, int64) { upstreamCalls++ }
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	account := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "test"}}
	var events []gjson.Result
	result, err := gateway.proxyDevinWSBridgeTurn(context.Background(), c, account, []byte(`{"type":"response.create","model":"swe-2-max","generate":false,"tools":[{"type":"custom","name":"apply_patch"}]}`), func(message []byte) error {
		events = append(events, gjson.ParseBytes(message))
		return nil
	})
	require.NoError(t, err)
	require.Zero(t, upstreamCalls)
	require.Len(t, events, 2)
	require.Equal(t, "response.created", events[0].Get("type").String())
	require.Equal(t, "response.completed", events[1].Get("type").String())
	require.Equal(t, result.ResponseID, events[1].Get("response.id").String())
	require.Empty(t, events[1].Get("response.output").Array())
	require.Zero(t, result.Usage.InputTokens)
	require.NotEmpty(t, c.GetString(devinWSCascadeScopeKey))
	state, _ := c.Get(devinWSRequestStateKey)
	require.Contains(t, string(state.(map[string]json.RawMessage)["tools"]), "apply_patch")
}

func TestDevinWebSocketReplayPreservesActiveToolSearchDiscovery(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	fields := map[string]json.RawMessage{
		"tools": json.RawMessage(`[{"type":"tool_search"}]`),
		"input": json.RawMessage(`[{"type":"tool_search_output","call_id":"search","tools":[{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{}}}]}]}]`),
	}
	mapping, err := prepareDevinWSReplayTools(c, fields)
	require.NoError(t, err)
	require.Equal(t, "tool_search_output", gjson.GetBytes(fields["input"], "0.type").String(), "the main adapter must still promote active discoveries")
	require.Contains(t, mapping.NamespaceTools, "files__read")
	c.Set(devinWSReplayToolsKey, mapping)
	fields["tools"] = json.RawMessage(`[]`)
	_, err = prepareDevinWSReplayTools(c, fields)
	require.NoError(t, err)
	require.Equal(t, "function_call_output", gjson.GetBytes(fields["input"], "0.type").String())
	require.JSONEq(t, `[]`, string(fields["tools"]), "retired discoveries must not re-enable tools")
}
