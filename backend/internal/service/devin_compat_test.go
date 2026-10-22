//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/proto"
)

func devinCompatGateway(t *testing.T, chat func(*pb.GetChatMessageRequest) []*pb.GetChatMessageResponse) *OpenAIGatewayService {
	t.Helper()
	d, _, _ := devinTestService(t)
	upstream := d.upstream.(*devinHTTP)
	original := upstream.handle
	upstream.handle = func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != devin.ChatPath {
			return original(r)
		}
		_, body, err := devin.ReadFrame(r.Body)
		assert.NoError(t, err)
		var request pb.GetChatMessageRequest
		assert.NoError(t, proto.Unmarshal(body, &request))
		var response []byte
		for _, event := range chat(&request) {
			body, err := proto.Marshal(event)
			assert.NoError(t, err)
			response = append(response, devinTestFrame(0, body)...)
		}
		response = append(response, devinTestFrame(2, []byte(`{}`))...)
		return devinReply(http.StatusOK, response), nil
	}
	return &OpenAIGatewayService{devin: d}
}

func devinCompatRequest(t *testing.T, gateway *OpenAIGatewayService, body []byte, protocol string) (*httptest.ResponseRecorder, *OpenAIForwardResult, error) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	a := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "test"}}
	result, err := gateway.forwardDevin(context.Background(), c, a, body, protocol, "compat-session", "")
	return w, result, err
}

func TestDevinCustomToolsRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "JSON", true: "SSE"}[stream], func(t *testing.T) {
			turn := 0
			input := "*** Begin Patch\n*** Add File: hello.txt\n+hello\n*** End Patch"
			gateway := devinCompatGateway(t, func(request *pb.GetChatMessageRequest) []*pb.GetChatMessageResponse {
				turn++
				if !assert.Len(t, request.Tools, 1) {
					return nil
				}
				assert.Equal(t, "apply_patch", request.Tools[0].Name)
				assert.Equal(t, "string", gjson.Get(request.Tools[0].JsonSchemaString, "properties.input.type").String())
				if turn == 2 {
					var signature, result string
					for _, prompt := range request.ChatMessagePrompts {
						if prompt.Signature != "" {
							signature = prompt.Signature
							assert.Len(t, prompt.ToolCalls, 1, "signed reasoning must share the assistant prompt with its tool calls")
							assert.Equal(t, "assistant-id", prompt.MessageId)
						}
						if prompt.ToolCallId == "call_patch" {
							result = prompt.Prompt
						}
					}
					assert.Equal(t, "signed-thinking", signature)
					assert.Equal(t, "patch applied", result)
					return []*pb.GetChatMessageResponse{{DeltaText: "done", StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN}}
				}
				arguments, err := json.Marshal(map[string]string{"input": input})
				assert.NoError(t, err)
				return []*pb.GetChatMessageResponse{
					{MessageId: "assistant-id", DeltaThinking: "make a file", DeltaSignature: "signed-thinking"},
					{DeltaToolCalls: []*pb.ChatToolCall{{Id: "call_patch", Name: "apply_patch", ArgumentsJson: string(arguments[:12])}}},
					{DeltaToolCalls: []*pb.ChatToolCall{{ArgumentsJson: string(arguments[12:])}}, StopReason: pb.StopReason_STOP_REASON_FUNCTION_CALL},
				}
			})
			body, err := json.Marshal(map[string]any{
				"model": "swe-2-max", "input": "make a file", "stream": stream,
				"tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}},
			})
			require.NoError(t, err)
			w, _, err := devinCompatRequest(t, gateway, body, "responses")
			require.NoError(t, err, w.Body.String())
			var response gjson.Result
			if stream {
				sequence := int64(0)
				for _, line := range strings.Split(w.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					event := gjson.Parse(strings.TrimPrefix(line, "data: "))
					require.Equal(t, sequence, event.Get("sequence_number").Int())
					sequence++
					if event.Get("type").String() == "response.completed" {
						response = event.Get("response")
					}
				}
				require.Contains(t, w.Body.String(), "response.custom_tool_call_input.done")
				require.NotContains(t, w.Body.String(), `"type":"function_call"`)
			} else {
				response = gjson.Parse(w.Body.String())
			}
			require.Equal(t, "custom_tool_call", response.Get("output.1.type").String())
			require.Equal(t, input, response.Get("output.1.input").String())
			var history []any
			require.NoError(t, json.Unmarshal([]byte(response.Get("output").Raw), &history))
			history = append([]any{map[string]any{"role": "user", "content": "make a file"}}, history...)
			history = append(history, map[string]any{"type": "custom_tool_call_output", "call_id": "call_patch", "output": "patch applied"})
			followup, err := json.Marshal(map[string]any{
				"model": "swe-2-max", "input": history,
				"tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}},
			})
			require.NoError(t, err)
			w, _, err = devinCompatRequest(t, gateway, followup, "responses")
			require.NoError(t, err, w.Body.String())
			require.Equal(t, "done", gjson.Get(w.Body.String(), "output.0.content.0.text").String())
		})
	}
}

func TestDevinNamespaceToolsAndNamedChoice(t *testing.T) {
	gateway := devinCompatGateway(t, func(request *pb.GetChatMessageRequest) []*pb.GetChatMessageResponse {
		if !assert.Len(t, request.Tools, 1) {
			return nil
		}
		name := request.Tools[0].Name
		assert.Equal(t, name, request.ToolChoice.GetToolName())
		return []*pb.GetChatMessageResponse{{DeltaToolCalls: []*pb.ChatToolCall{{Id: "call_ns", Name: name, ArgumentsJson: `{}`}}, StopReason: pb.StopReason_STOP_REASON_FUNCTION_CALL}}
	})
	body := []byte(`{"model":"swe-2-max","input":"run it","tools":[{"type":"namespace","name":"shell","tools":[{"type":"function","name":"run"}]}],"tool_choice":{"type":"function","name":"run","namespace":"shell"}}`)
	w, _, err := devinCompatRequest(t, gateway, body, "responses")
	require.NoError(t, err, w.Body.String())
	require.Equal(t, "run", gjson.Get(w.Body.String(), "output.0.name").String())
	require.Equal(t, "shell", gjson.Get(w.Body.String(), "output.0.namespace").String())
}

func TestDevinRejectsInvalidJSONAndUnsupportedOptionsBeforeUpstream(t *testing.T) {
	gateway := &OpenAIGatewayService{devin: &DevinService{}}
	for _, body := range []string{
		`{"model":"swe-2-max","input":"hi","tools":[{"type":"custom","name":"exec"}]} {}`,
		`{"model":"swe-2-max","input":"hi","n":2}`,
		`{"model":"swe-2-max","input":"hi","stop":{}}`,
	} {
		w, _, err := devinCompatRequest(t, gateway, []byte(body), "responses")
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, w.Code)
	}
}

func TestDevinChatReasoningReplayAndUsageContract(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "JSON", true: "SSE"}[stream], func(t *testing.T) {
			turn := 0
			gateway := devinCompatGateway(t, func(request *pb.GetChatMessageRequest) []*pb.GetChatMessageResponse {
				turn++
				assert.Equal(t, []string{"END"}, request.Configuration.StopPatterns)
				if turn == 2 {
					var signature string
					for _, prompt := range request.ChatMessagePrompts {
						if prompt.Signature != "" {
							signature = prompt.Signature
							assert.Equal(t, "thinking", prompt.Thinking)
						}
					}
					assert.Equal(t, "chat-signature", signature)
				}
				return []*pb.GetChatMessageResponse{
					{MessageId: "assistant", DeltaThinking: "thinking", DeltaSignature: "chat-signature"},
					{DeltaText: "answer", StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN, Usage: &pb.ModelUsageStats{InputTokens: 3, OutputTokens: 2}},
				}
			})
			body, err := json.Marshal(map[string]any{
				"model": "swe-2-max", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
				"stream": stream, "stream_options": map[string]bool{"include_usage": true}, "stop": "END",
			})
			require.NoError(t, err)
			w, _, err := devinCompatRequest(t, gateway, body, "chat")
			require.NoError(t, err, w.Body.String())
			var signature string
			if stream {
				usageChunks := 0
				for _, line := range strings.Split(w.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data: {") {
						continue
					}
					chunk := gjson.Parse(strings.TrimPrefix(line, "data: "))
					if value := chunk.Get("choices.0.delta.reasoning_signature").String(); value != "" {
						signature = value
					}
					if chunk.Get("usage").Exists() {
						usageChunks++
						require.Empty(t, chunk.Get("choices").Array())
					}
				}
				require.Equal(t, 1, usageChunks)
				require.True(t, strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n"))
			} else {
				signature = gjson.Get(w.Body.String(), "choices.0.message.reasoning_signature").String()
			}
			require.True(t, strings.HasPrefix(signature, "devin_v1:"))
			followup, err := json.Marshal(map[string]any{
				"model": "swe-2-max", "stop": "END",
				"messages": []any{
					map[string]any{"role": "user", "content": "hello"},
					map[string]any{"role": "assistant", "content": "answer", "reasoning_content": "thinking", "reasoning_signature": signature},
					map[string]any{"role": "user", "content": "continue"},
				},
			})
			require.NoError(t, err)
			w, _, err = devinCompatRequest(t, gateway, followup, "chat")
			require.NoError(t, err, w.Body.String())
		})
	}
}

func TestDevinRouterAssignsConcreteModelBeforeNormalizingSchemas(t *testing.T) {
	gateway := devinCompatGateway(t, func(request *pb.GetChatMessageRequest) []*pb.GetChatMessageResponse {
		assert.Equal(t, "MODEL_GOOGLE_GEMINI_FLASH", request.ChatModelUid)
		assert.Equal(t, "fusion-assignment", request.GetModelAssignmentJwt())
		if assert.Len(t, request.Tools, 1) {
			assert.Equal(t, "number", gjson.Get(request.Tools[0].JsonSchemaString, "properties.value.type").String())
			assert.True(t, gjson.Get(request.Tools[0].JsonSchemaString, "properties.value.nullable").Bool())
		}
		return []*pb.GetChatMessageResponse{{DeltaText: "fusion answer", ActualModelUid: proto.String("gemini-result"), Usage: &pb.ModelUsageStats{BillingModelUid: "gemini-billing"}, StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN}}
	})
	upstream := gateway.devin.upstream.(*devinHTTP)
	original := upstream.handle
	assignments := 0
	upstream.handle = func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case devin.ModelsPath:
			body, err := proto.Marshal(&pb.GetCliModelConfigsResponse{ClientModelConfigs: []*pb.ClientModelConfig{{ModelUid: "router-test", ModelInfo: &pb.ModelInfo{IsModelRouter: true, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}}}})
			assert.NoError(t, err)
			return devinReply(200, body), nil
		case devin.AssignPath:
			assignments++
			body, err := proto.Marshal(&pb.AssignModelResponse{Assignment: &pb.ModelAssignment{ModelUid: "MODEL_GOOGLE_GEMINI_FLASH", AssignmentJwt: "fusion-assignment"}})
			assert.NoError(t, err)
			return devinReply(200, body), nil
		default:
			return original(request)
		}
	}
	w, result, err := devinCompatRequest(t, gateway, []byte(`{"model":"router-test","input":"hello","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{"value":{"type":["number","null"]}}}}]}`), "responses")
	require.NoError(t, err, w.Body.String())
	require.Equal(t, 1, assignments)
	require.Equal(t, "router-test", gjson.Get(w.Body.String(), "model").String())
	require.Equal(t, "gemini-billing", result.BillingModel)
	require.Equal(t, "gemini-result", result.UpstreamResponseModel)
}
