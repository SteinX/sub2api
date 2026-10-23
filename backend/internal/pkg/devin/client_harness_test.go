package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestClientHarnessCatalogHintsDoNotBlockConcreteModels(t *testing.T) {
	for _, harness := range []string{"strawberry-pancake", "swe-1p6-exec", "swe-1p6", "swe-1p5", "gpt-5p6-codex", "gpt-5p6", "gemini-3-pro", "gpt-5p4-persistent"} {
		t.Run(harness, func(t *testing.T) {
			model := &pb.ClientModelConfig{ModelUid: "concrete-model", ModelInfo: &pb.ModelInfo{HarnessUids: []string{harness}}}
			plan := PlanModel(model)
			require.Equal(t, RouteDirect, plan.Mode)
			require.Equal(t, KindDirect, plan.CatalogKind)
			require.Equal(t, []string{harness}, plan.HarnessUIDs, "retain catalog hints for diagnosis")
			require.NoError(t, CheckModelIntent([]*pb.ClientModelConfig{model}, model.ModelUid))
		})
	}
	for _, model := range []*pb.ClientModelConfig{
		{ModelUid: "concrete", ModelInfo: &pb.ModelInfo{HarnessUids: []string{"strawberry-pancake", "unknown-future-harness"}}},
		{ModelUid: "concrete", ModelInfo: &pb.ModelInfo{HarnessUids: []string{"fusion"}}},
		{ModelUid: "fusion-lead-sidekick-worker", ModelInfo: &pb.ModelInfo{HarnessUids: []string{"strawberry-pancake"}}},
		{ModelUid: "opaque", Label: "Fusion Fast", ModelInfo: &pb.ModelInfo{HarnessUids: []string{"strawberry-pancake"}}},
	} {
		require.Equal(t, RouteBlocked, PlanModel(model).Mode)
		require.Error(t, CheckModelIntent([]*pb.ClientModelConfig{model}, model.ModelUid))
	}
}

func TestClientHarnessMembersKeepNativeChatUIDAndCacheUsage(t *testing.T) {
	for _, tc := range []struct {
		uid       string
		harnesses []string
	}{
		{"claude-opus-5-5-medium", []string{"strawberry-pancake"}},
		{"swe-2-medium", []string{"swe-1p6-exec", "swe-1p6", "swe-1p5"}},
	} {
		t.Run(tc.uid, func(t *testing.T) {
			model := &pb.ClientModelConfig{ModelUid: tc.uid, ModelInfo: &pb.ModelInfo{HarnessUids: tc.harnesses}}
			var paths []string
			client := &Client{BaseURL: "https://example.test", Do: func(request *http.Request) (*http.Response, error) {
				paths = append(paths, request.URL.Path)
				var payload []byte
				switch request.URL.Path {
				case UserJWTPath:
					payload, _ = proto.Marshal(&pb.GetUserJwtResponse{UserJwt: "synthetic-user-jwt"})
				case ChatPath:
					_, body, err := ReadFrame(request.Body)
					require.NoError(t, err)
					var wire pb.GetChatMessageRequest
					require.NoError(t, proto.Unmarshal(body, &wire))
					require.Equal(t, tc.uid, wire.ChatModelUid)
					require.Empty(t, wire.GetModelAssignmentJwt())
					payload, _ = proto.Marshal(&pb.GetChatMessageResponse{DeltaText: "OK", StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN, Usage: &pb.ModelUsageStats{InputTokens: 3, CacheReadTokens: 40, CacheWriteTokens: 7, OutputTokens: 2}})
					payload = append(frame(0, payload), frame(2, []byte(`{}`))...)
				default:
					t.Fatalf("concrete members must not invoke assignment: %s", request.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(payload))}, nil
			}}
			request := &apicompat.ResponsesRequest{Model: tc.uid, Input: json.RawMessage(`"Reply with OK."`)}
			wire, err := BuildRequest(request, "synthetic-concrete-member")
			require.NoError(t, err)
			prepared, err := client.Prepare(context.Background(), "synthetic-token", wire, PlanModel(model), []*pb.ClientModelConfig{model})
			require.NoError(t, err)
			require.False(t, prepared.AssignmentUsed)
			decoder := NewEventDecoder(tc.uid, func(apicompat.ResponsesStreamEvent) error { return nil })
			require.NoError(t, client.Chat(context.Background(), prepared.BaseURL, "synthetic-token", wire, decoder.Apply))
			require.NoError(t, decoder.Finish())
			require.Equal(t, []string{UserJWTPath, ChatPath}, paths)
			require.Equal(t, 50, decoder.Response.Usage.InputTokens)
			require.Equal(t, 40, decoder.Response.Usage.InputTokensDetails.CachedTokens)
			require.Equal(t, 7, decoder.Response.Usage.CacheCreationInputTokens)
		})
	}
}
