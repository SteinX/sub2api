package devin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestNativeRequestDefaultsAndModelLimits(t *testing.T) {
	r := &apicompat.ResponsesRequest{Model: "swe-2-high", Input: json.RawMessage(`"hi"`)}
	q, err := BuildRequest(r, "account-test:1")
	require.NoError(t, err)
	require.Equal(t, uint64(64000), q.Configuration.MaxTokens)
	require.Equal(t, uint64(400), q.Configuration.MaxNewlines)
	require.Equal(t, uint64(40), q.Configuration.TopK)
	require.Equal(t, float64(float32(0.95)), q.Configuration.TopP)
	require.Equal(t, 1.0, q.Configuration.Temperature)
	require.Empty(t, q.Configuration.StopPatterns)
	model := &pb.ClientModelConfig{ModelInfo: &pb.ModelInfo{MaxOutputTokens: 32000, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}}
	require.NoError(t, ApplyModelConfig(q, model, r))
	require.Equal(t, uint64(32000), q.Configuration.MaxTokens)
	require.True(t, q.DisableParallelToolCalls)
	limit := 32001
	r.MaxOutputTokens = &limit
	var capability *RouteError
	require.ErrorAs(t, ApplyModelConfig(q, model, r), &capability)
	require.Equal(t, CodeModelCapability, capability.Code)
}

func TestCredentialEncodingPreservesNativeKeyKinds(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"  eyJheader.eyJpayload.signature  ", "devin-session-token$eyJheader.eyJpayload.signature"},
		{"devin-session-token$eyJheader.eyJpayload.signature", "devin-session-token$eyJheader.eyJpayload.signature"},
		{"sk-ws-legacy", "sk-ws-legacy"},
		{"devin-session-token$sk-ws-legacy", "sk-ws-legacy"},
		{"opaque-cli-token", "opaque-cli-token"},
		{"", ""},
	} {
		require.Equal(t, tc.want, NormalizeToken(tc.input))
	}
}

func TestNativeModelOrAliasUsesEnumAndUIDWireFields(t *testing.T) {
	// Independent wire fixture: legacy Model enum (field 1, varint 7), then
	// current model_uid (field 3, string). Field 1 must not be decoded as text.
	var model pb.ModelOrAlias
	require.NoError(t, proto.Unmarshal([]byte{0x08, 0x07}, &model))
	require.Equal(t, int32(7), model.GetModel())
	require.NoError(t, proto.Unmarshal(append([]byte{0x1a, 0x0a}, []byte("swe-2-high")...), &model))
	require.Equal(t, "swe-2-high", ModelID(&pb.ClientModelConfig{ModelOrAlias: &model}))
}

func TestUpstreamFailuresKeepRPCAndTraceWithoutLeakingPayload(t *testing.T) {
	for _, path := range []string{UserJWTPath, ChatPath} {
		t.Run(path[strings.LastIndex(path, "/")+1:], func(t *testing.T) {
			client := &Client{BaseURL: "https://server.codeium.com", Do: func(*http.Request) (*http.Response, error) {
				payload := `{"code":"invalid_argument","message":"secret-token an internal error occurred (trace ID: 0123456789abcdef0123456789abcdef)"}`
				status := 400
				if path == ChatPath {
					payload = string(frame(2, []byte(`{"error":`+payload+`}`)))
					status = 200
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(payload))}, nil
			}}
			var err error
			if path == ChatPath {
				err = client.Chat(context.Background(), client.BaseURL, "token", &pb.GetChatMessageRequest{}, func(*pb.GetChatMessageResponse) error { return nil })
			} else {
				_, err = client.Prepare(context.Background(), "token", &pb.GetChatMessageRequest{ChatModelUid: "model"}, PlanModelUID("model"), nil)
			}
			var failure *Error
			require.ErrorAs(t, err, &failure)
			require.Equal(t, "0123456789abcdef0123456789abcdef", failure.TraceID)
			require.Equal(t, path[strings.LastIndex(path, "/")+1:], failure.Operation)
			require.Contains(t, err.Error(), "trace ID:")
			require.NotContains(t, err.Error(), "secret-token")
		})
	}
}

func TestToolReplayNormalizesForeignIDsWithoutBreakingPairs(t *testing.T) {
	foreign := "call|" + strings.Repeat("x", 80)
	input, err := json.Marshal([]map[string]string{
		{"type": "function_call", "call_id": foreign, "name": "exec", "arguments": "{}"},
		{"type": "function_call_output", "call_id": foreign, "output": "done"},
	})
	require.NoError(t, err)
	r := &apicompat.ResponsesRequest{Model: "fusion", Input: input}
	first, err := BuildRequest(r, "tenant:session")
	require.NoError(t, err)
	second, err := BuildRequest(r, "tenant:session")
	require.NoError(t, err)
	callID := first.ChatMessagePrompts[0].ToolCalls[0].Id
	require.LessOrEqual(t, len(callID), 40)
	require.NotContains(t, callID, "|")
	require.Equal(t, callID, first.ChatMessagePrompts[1].ToolCallId)
	require.Equal(t, callID, second.ChatMessagePrompts[0].ToolCalls[0].Id)
}

func TestGoogleToolSchemaNormalizationPreservesUnionAndPropertyNames(t *testing.T) {
	request := &pb.GetChatMessageRequest{ChatModelUid: "MODEL_GOOGLE_GEMINI_FLASH", Tools: []*pb.ChatToolDefinition{{Name: "run", JsonSchemaString: `{"type":"object","properties":{"items":{"type":["array","string","null"],"items":{"type":["number","null"]}},"type":{"const":"object"}}}`}}}
	require.NoError(t, NormalizeToolSchemas(request))
	var schema map[string]any
	require.NoError(t, json.Unmarshal([]byte(request.Tools[0].JsonSchemaString), &schema))
	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	items, ok := properties["items"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, items["nullable"])
	require.NotContains(t, items, "type")
	branches, ok := items["anyOf"].([]any)
	require.True(t, ok)
	require.Len(t, branches, 2)
	arrayBranch, ok := branches[0].(map[string]any)
	require.True(t, ok)
	stringBranch, ok := branches[1].(map[string]any)
	require.True(t, ok)
	require.Contains(t, arrayBranch, "items")
	require.NotContains(t, stringBranch, "items")
	typeProperty, ok := properties["type"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []any{"object"}, typeProperty["enum"])
	original := request.Tools[0].JsonSchemaString
	request.ChatModelUid = "swe-2-max"
	require.NoError(t, NormalizeToolSchemas(request))
	require.Equal(t, original, request.Tools[0].JsonSchemaString)
}
