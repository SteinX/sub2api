//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDevinFusionGatewayIngressPreservesQueuedHandoffCandidate(t *testing.T) {
	f := newWarmFixture(t)
	t.Cleanup(func() { require.NoError(t, f.service.CloseDevinCacheWarm(context.Background())) })
	generation := 0
	f.chat = func(_ *http.Request, wire *pb.GetChatMessageRequest) (*http.Response, error) {
		generation++
		reply := &pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_FUNCTION_CALL, Usage: &pb.ModelUsageStats{InputTokens: 4, CacheReadTokens: 96, OutputTokens: 20}}
		switch generation {
		case 1:
			reply.DeltaToolCalls = []*pb.ChatToolCall{
				{Id: "lead-read", Name: "read_file", ArgumentsJson: `{"path":"main.go"}`},
				{Id: "d1", Name: fusion.DelegateTool, ArgumentsJson: `{"task":"inspect original task","acceptance_criteria":"report evidence"}`},
			}
		case 2:
			reply.DeltaToolCalls = []*pb.ChatToolCall{{Id: "worker-read", Name: "read_file", ArgumentsJson: `{"path":"main.go"}`}}
		default:
			t.Fatalf("unexpected inference %d", generation)
		}
		return warmReply(reply), nil
	}
	send := func(body []byte, sessionID string) (*apicompat.ResponsesResponse, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		if sessionID != "" {
			c.Request.Header.Set(fusion.HeaderSession, sessionID)
		}
		c.Set("api_key", f.keys.key)
		_, err := f.service.BindDevinFusionRequest(c, f.keys.key, f.session.PublicModel, body, "responses", f.keys)
		require.NoError(t, err)
		_, err = f.service.forwardDevin(c.Request.Context(), c, f.account, body, "responses", "unused", "")
		require.NoError(t, err)
		require.Equal(t, 200, w.Code)
		var response apicompat.ResponsesResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		return &response, w.Header().Get(fusion.HeaderSession)
	}
	response, id := send(fusionProtocolBody("responses", false, ""), "")
	e := f.service.devin.fusionEngine()
	scope := fusion.Scope(f.keys.key.UserID, f.keys.key.ID)
	before, _, err := e.Cache.Load(context.Background(), scope, id)
	require.NoError(t, err)
	require.NotNil(t, before.Snapshot)
	require.Equal(t, "NO_SNAPSHOT", before.State)
	t.Logf("before ingress cache=%s candidate_present=%t", before.State, before.Snapshot != nil)
	// Ingress may bind successfully while later admission does not forward the
	// request. Repeating this boundary must retain the same parked candidate.
	for attempt := 0; attempt < 2; attempt++ {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Request.Header.Set(fusion.HeaderSession, id)
		_, err := f.service.BindDevinFusionRequest(c, f.keys.key, f.session.PublicModel, fusionProtocolBody("responses", false, response.Output[0].CallID), "responses", f.keys)
		require.NoError(t, err)
		parked, _, err := e.Cache.Load(context.Background(), scope, id)
		require.NoError(t, err)
		t.Logf("bound_without_forward attempt=%d cache=%s candidate_present=%t generations=%d", attempt+1, parked.State, parked.Snapshot != nil, generation)
		require.NotNil(t, parked.Snapshot, "binding without forwarding must not discard a queued handoff candidate")
		require.Equal(t, before.Snapshot.PrefixDigest, parked.Snapshot.PrefixDigest)
		require.Equal(t, "NO_SNAPSHOT", parked.State)
		require.Equal(t, 1, generation)
	}
	_, _ = send(fusionProtocolBody("responses", false, response.Output[0].CallID), id)
	s, _, err := e.Load(context.Background(), scope, id)
	require.NoError(t, err)
	require.Equal(t, fusion.Sidekick, s.Role)
	require.Equal(t, "worker-read", s.Pending.InternalID)
	after, _, err := e.Cache.Load(context.Background(), scope, id)
	require.NoError(t, err)
	t.Logf("after ingress cache=%s candidate_present=%t stop_reason=%s active_handoff=%s generations=%d", after.State, after.Snapshot != nil, after.StopReason, s.Handoffs[len(s.Handoffs)-1].ID, generation)
	require.NotNil(t, after.Snapshot, "the real tool-result ingress must preserve the queued handoff candidate")
	require.Equal(t, "ARMED", after.State)
	require.Equal(t, s.Handoffs[len(s.Handoffs)-1].ID, after.WindowID)
}
