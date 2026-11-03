//go:build devine2e

package devine2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Public Responses/SSE tool continuation through the production binary, native
// adapter, SQL and Redis. Native decisions are synthetic; this is not a Bun
// transcript replay, an actual CLI run or real PAT acceptance.
func TestDevinFusionCompactionFitPublicResponses(t *testing.T) {
	if os.Getenv("DEVIN_E2E_MODE") != "mock" {
		t.Skip("requires isolated mock dependencies")
	}
	const lead, worker = "claude-opus-5-xhigh", "swe-2-high"
	const model = "fusion-claude-opus-5-xhigh-sidekick-swe-2-high"
	f := newNativeFixture(t)
	f.agentMode.Store(true)
	for _, uid := range []string{lead, worker} {
		window, tier := int32(262144), pb.ModelCostTier_MODEL_COST_TIER_HIGH
		if uid == worker {
			window, tier = 128000, pb.ModelCostTier_MODEL_COST_TIER_FREE
		}
		f.catalog = append(f.catalog, &pb.ClientModelConfig{ModelUid: uid, PricingType: pb.ModelPricingType_MODEL_PRICING_TYPE_API, ModelCostTier: tier, MaxTokens: window, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 8192, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}})
	}
	outputs := []string{}
	for i := 0; i < 3; i++ {
		outputs = append(outputs, fmt.Sprintf("complete-source-%d\n", i)+strings.Repeat("x", 36000))
	}
	var native atomic.Int32
	f.chatReply = func(req *pb.GetChatMessageRequest, reply *pb.GetChatMessageResponse) {
		n := native.Add(1)
		reply.DeltaText, reply.DeltaToolCalls = "", nil
		reply.Usage.BillingModelUid = req.ChatModelUid
		switch {
		case n == 1:
			assert.Equal(t, lead, req.ChatModelUid)
			reply.DeltaToolCalls = []*pb.ChatToolCall{{Id: "original-handoff", Name: fusion.DelegateTool, ArgumentsJson: `{"task":"Review actual source evidence","constraints":"Keep the original interface","acceptance_criteria":"Return a report supported by the tool results"}`}}
		case n >= 2 && n <= 4:
			assert.Equal(t, worker, req.ChatModelUid)
			reply.DeltaToolCalls = []*pb.ChatToolCall{{Id: fmt.Sprintf("read-%d", n-1), Name: "read_file", ArgumentsJson: `{}`}}
		case n == 5:
			assert.Equal(t, worker, req.ChatModelUid)
			assert.Empty(t, req.Tools)
			assert.Equal(t, "none", req.ToolChoice.GetOptionName())
			assert.Contains(t, fmt.Sprint(req.ChatMessagePrompts), "complete-source-0")
			assert.Contains(t, fmt.Sprint(req.ChatMessagePrompts), "complete-source-1")
			reply.DeltaText = `{"goal":"Keep the original interface","current_state":"First two completed file reads inspected","decisions":[],"changes":[],"evidence":["Two completed source reads"],"checks":[],"open_questions":[],"next_steps":["Review the latest complete result"],"files":[]}`
		case n == 6:
			assert.Equal(t, worker, req.ChatModelUid)
			found := false
			for _, prompt := range req.ChatMessagePrompts {
				found = found || prompt.Prompt == outputs[2]
			}
			assert.True(t, found, "latest complete tool result must reach the actual native role unchanged")
			assert.Contains(t, fmt.Sprint(req.ChatMessagePrompts), "Keep the original interface")
			reply.DeltaText = "Actual Sidekick report: reviewed the complete source evidence"
		case n == 7:
			assert.Equal(t, lead, req.ChatModelUid)
			assert.Contains(t, fmt.Sprint(req.ChatMessagePrompts), "Actual Sidekick report")
			assert.NotContains(t, fmt.Sprint(req.ChatMessagePrompts), outputs[2])
			reply.DeltaText = "Lead reviewed the returned report and completed the original task"
		default:
			t.Errorf("unexpected extra generation %d", n)
		}
		reply.StopReason = pb.StopReason_STOP_REASON_STOP_PATTERN
		if len(reply.DeltaToolCalls) > 0 {
			reply.StopReason = pb.StopReason_STOP_REASON_FUNCTION_CALL
		}
	}
	a := newApplication(t, f, 3)
	admin := login(t, a, "admin@devin-e2e.invalid", a.password)
	compliance := api(t, a, "GET", "/admin/compliance", admin, nil)
	api(t, a, "POST", "/admin/compliance/accept", admin, map[string]string{"language": "en", "phrase": compliance.Get("ack_phrase_en").String()})
	group := api(t, a, "POST", "/admin/groups", admin, map[string]any{"name": "compaction-fit", "platform": "devin", "rate_multiplier": 1, "subscription_type": "standard", "is_exclusive": true})
	groupID := group.Get("id").Int()
	_, err := addPATAccount(t, a, admin, "cog_e2e_compaction_fit", groupID, f.registerProxy(t, a, admin))
	require.NoError(t, err)
	password := randomID()
	a.redactor.addSecret(password)
	createdUser := api(t, a, "POST", "/admin/users", admin, map[string]any{"email": "compaction-fit@devin-e2e.invalid", "password": password, "balance": 100, "concurrency": 4, "allowed_groups": []int64{groupID}})
	user := login(t, a, "compaction-fit@devin-e2e.invalid", password)
	created := api(t, a, "POST", "/keys", user, map[string]any{"name": "compaction-fit", "group_id": groupID})
	key, keyID := created.Get("key").String(), created.Get("id").Int()
	a.redactor.addSecret(key)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT"), Password: os.Getenv("DEVIN_E2E_REDIS_PASSWORD"), DB: 3})
	t.Cleanup(func() { _ = rdb.Close() })
	engine := fusion.New(devin.NewRedisSessionStore(rdb))
	session := "compaction-fit-" + randomID()
	scope := fusion.Scope(createdUser.Get("id").Int(), keyID)
	load := func() *fusion.Session {
		s, _, err := engine.Load(context.Background(), scope, session)
		require.NoError(t, err)
		return s
	}
	tools := []any{map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object"}}}
	for i := 0; i < 11; i++ {
		tools = append(tools, map[string]any{"type": "function", "name": fmt.Sprintf("fixture_tool_%d", i), "parameters": map[string]any{"type": "object"}})
	}
	history := []any{map[string]any{"type": "message", "role": "user", "content": "Review actual source evidence and preserve the original interface"}}
	send := func(step int) gjson.Result {
		payload, err := json.Marshal(map[string]any{"model": model, "stream": true, "store": false, "max_output_tokens": 1024, "tools": tools, "input": history})
		require.NoError(t, err)
		req, err := http.NewRequest("POST", a.base+"/v1/responses", bytes.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(fusion.HeaderSession, session)
		req.Header.Set("Idempotency-Key", fmt.Sprintf("step-%d", step))
		if step == 0 {
			req.Header.Set(fusion.HeaderClosingPolicy, fusion.ClosingReserveV1)
		}
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode, "error=%s", gjson.GetBytes(body, "error.code").String())
		var completed gjson.Result
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			event := gjson.Parse(strings.TrimPrefix(line, "data: "))
			require.NotEqual(t, "response.failed", event.Get("type").String())
			require.NotEqual(t, "error", event.Get("type").String())
			if event.Get("type").String() == "response.completed" {
				completed = event.Get("response")
			}
		}
		require.True(t, completed.Exists(), "an HTTP200 or keepalive is not completion")
		return completed
	}
	response := send(0)
	started := load().TurnStarted
	for step, output := range outputs {
		require.Equal(t, "function_call", response.Get("output.0.type").String())
		for _, item := range response.Get("output").Array() {
			history = append(history, item.Value())
		}
		history = append(history, map[string]any{"type": "function_call_output", "call_id": response.Get("output.0.call_id").String(), "output": output})
		response = send(step + 1)
	}
	require.Equal(t, "message", response.Get("output.0.type").String())
	s := load()
	require.Equal(t, fusion.Completed, s.State)
	require.True(t, started.Equal(s.TurnStarted))
	require.Equal(t, 20, s.Profile.MaxRequests)
	require.Equal(t, 5.0, s.Profile.MaxCostUSD)
	require.Equal(t, 900, s.Profile.MaxWallSeconds)
	require.Equal(t, 7, s.TurnRequests)
	require.Equal(t, 1, s.Maintenance.CompactionsApplied)
	require.Equal(t, "applied", s.Contexts[fusion.Sidekick].Job.State)
	require.True(t, s.Contexts[fusion.Sidekick].Job.Accounted)
	require.NotEmpty(t, s.Handoffs[0].Report)
	require.EqualValues(t, 7, native.Load())
	db := agentDatabase(t, a)
	var rows, unique, leadRows, workerRows, dedup int
	var cost, workerCost float64
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT count(*),count(DISTINCT request_id),count(*) FILTER(WHERE upstream_model=$2),count(*) FILTER(WHERE upstream_model=$3),coalesce(sum(actual_cost),0),coalesce(sum(actual_cost) FILTER(WHERE upstream_model=$3),0) FROM usage_logs WHERE api_key_id=$1`, keyID, lead, worker).Scan(&rows, &unique, &leadRows, &workerRows, &cost, &workerCost) == nil && rows == 7
	}, 10*time.Second, 100*time.Millisecond)
	require.Equal(t, 7, unique)
	require.Equal(t, 2, leadRows)
	require.Equal(t, 5, workerRows)
	require.Zero(t, workerCost, "identified FREE calls still require unique settlement rows")
	require.InDelta(t, cost, s.TurnCostUSD, 1e-10)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE api_key_id=$1`, keyID).Scan(&dedup))
	require.Equal(t, 7, dedup)
}
