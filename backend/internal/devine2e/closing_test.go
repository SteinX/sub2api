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

// Public SSE/protocol regression through the production binary, SQL and Redis.
// Native replies and client tool results are synthetic: this is not live PAT
// acceptance, actual OMP execution or an a3 transcript replay.
func TestDevinFusionClosingPublicResponses(t *testing.T) {
	if os.Getenv("DEVIN_E2E_MODE") != "mock" {
		t.Skip("requires isolated mock dependencies")
	}
	const lead = "claude-opus-5-xhigh"
	const worker = "swe-2-high"
	const model = "fusion-claude-opus-5-xhigh-sidekick-swe-2-high"
	f := newNativeFixture(t)
	f.agentMode.Store(true)
	for _, uid := range []string{lead, worker} {
		tier := pb.ModelCostTier_MODEL_COST_TIER_HIGH
		if uid == worker {
			tier = pb.ModelCostTier_MODEL_COST_TIER_FREE
		}
		f.catalog = append(f.catalog, &pb.ClientModelConfig{ModelUid: uid, PricingType: pb.ModelPricingType_MODEL_PRICING_TYPE_API, ModelCostTier: tier, MaxTokens: 262144, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 8192, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}})
	}
	var native atomic.Int32
	f.chatReply = func(req *pb.GetChatMessageRequest, reply *pb.GetChatMessageResponse) {
		n := native.Add(1)
		reply.DeltaText, reply.DeltaToolCalls = "", nil
		reply.Usage.BillingModelUid = req.ChatModelUid
		call := func(id string) *pb.ChatToolCall {
			return &pb.ChatToolCall{Id: id, Name: "fixture_tool_0", ArgumentsJson: `{"path":"private-fixture-path"}`}
		}
		switch {
		case n <= 3:
			assert.Equal(t, lead, req.ChatModelUid)
			reply.DeltaToolCalls = []*pb.ChatToolCall{call(fmt.Sprintf("lead-%d", n))}
		case n == 4:
			assert.Equal(t, lead, req.ChatModelUid)
			reply.DeltaToolCalls = []*pb.ChatToolCall{{Id: "original-delegation", Name: fusion.DelegateTool, ArgumentsJson: `{"task":"Investigate the original fixture","acceptance_criteria":"Report actual evidence and unfinished work"}`}}
		case n <= 15:
			assert.Equal(t, worker, req.ChatModelUid)
			reply.DeltaToolCalls = []*pb.ChatToolCall{call(fmt.Sprintf("worker-%d", n))}
		case n == 16:
			assert.Equal(t, worker, req.ChatModelUid)
			reply.DeltaToolCalls = []*pb.ChatToolCall{call("last-error"), call("last-success")}
		case n == 17:
			assert.Equal(t, worker, req.ChatModelUid)
			assert.Equal(t, "none", req.ToolChoice.GetOptionName())
			assert.Contains(t, fmt.Sprint(req.ChatMessagePrompts), "private-tool-result-last-error")
			assert.Contains(t, fmt.Sprint(req.ChatMessagePrompts), "private-tool-result-last-success")
			reply.DeltaText = "Partial Sidekick report: investigation evidence retained; implementation and tests unfinished"
		case n == 18:
			assert.Equal(t, lead, req.ChatModelUid)
			assert.Equal(t, "none", req.ToolChoice.GetOptionName())
			assert.Contains(t, fmt.Sprint(req.ChatMessagePrompts), "Partial Sidekick report")
			assert.NotContains(t, fmt.Sprint(req.ChatMessagePrompts), "private-tool-result-last-success")
			reply.DeltaText = "Reviewed the actual report. The task remains unfinished and requires further work."
		default:
			t.Errorf("unexpected additional native call %d", n)
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
	group := api(t, a, "POST", "/admin/groups", admin, map[string]any{"name": "closing", "platform": "devin", "rate_multiplier": 1, "subscription_type": "standard", "is_exclusive": true})
	groupID := group.Get("id").Int()
	accountID, err := addPATAccount(t, a, admin, "cog_e2e_closing", groupID, f.registerProxy(t, a, admin))
	require.NoError(t, err)
	password := randomID()
	a.redactor.addSecret(password)
	createdUser := api(t, a, "POST", "/admin/users", admin, map[string]any{"email": "closing@devin-e2e.invalid", "password": password, "balance": 100, "concurrency": 4, "allowed_groups": []int64{groupID}})
	user := login(t, a, "closing@devin-e2e.invalid", password)
	created := api(t, a, "POST", "/keys", user, map[string]any{"name": "closing", "group_id": groupID})
	key, keyID := created.Get("key").String(), created.Get("id").Int()
	a.redactor.addSecret(key)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT"), Password: os.Getenv("DEVIN_E2E_REDIS_PASSWORD"), DB: 3})
	t.Cleanup(func() { _ = rdb.Close() })
	engine := fusion.New(devin.NewRedisSessionStore(rdb))
	session := "closing-" + randomID()
	scope := fusion.Scope(createdUser.Get("id").Int(), keyID)
	load := func() *fusion.Session {
		s, _, err := engine.Load(context.Background(), scope, session)
		require.NoError(t, err)
		return s
	}
	tools := make([]any, 12)
	for i := range tools {
		tools[i] = map[string]any{"type": "function", "name": fmt.Sprintf("fixture_tool_%d", i), "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]string{"type": "string"}}}}
	}
	history := []any{map[string]any{"type": "message", "role": "user", "content": "Investigate the fixture and report actual evidence or remaining work."}}
	send := func(step int) gjson.Result {
		payload, err := json.Marshal(map[string]any{"model": model, "stream": true, "store": false, "max_output_tokens": 1024, "tools": tools, "input": history})
		require.NoError(t, err)
		do := func() gjson.Result {
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
				if strings.HasPrefix(line, "data: ") {
					event := gjson.Parse(strings.TrimPrefix(line, "data: "))
					require.NotEqual(t, "response.failed", event.Get("type").String())
					require.NotEqual(t, "error", event.Get("type").String())
					if event.Get("type").String() == "response.completed" {
						completed = event.Get("response")
					}
				}
			}
			require.True(t, completed.Exists(), "a keepalive is not completion")
			return completed
		}
		result := do()
		before := native.Load()
		replay := do()
		require.Equal(t, result.Get("id").String(), replay.Get("id").String())
		require.Equal(t, before, native.Load())
		return result
	}
	response := send(0)
	started := load().TurnStarted
	for step := 1; step < 30 && response.Get("output.0.type").String() == "function_call"; step++ {
		s := load()
		require.Equal(t, 1, s.Turn)
		require.Equal(t, started, s.TurnStarted)
		call := response.Get("output.0")
		id := s.Pending.InternalID
		status := "success"
		if id == "last-error" {
			status = "error"
		}
		history = append(history, map[string]any{"type": "function_call", "call_id": call.Get("call_id").String(), "name": call.Get("name").String(), "arguments": call.Get("arguments").String()}, map[string]any{"type": "function_call_output", "call_id": call.Get("call_id").String(), "output": "private-tool-result-" + id + " status=" + status})
		if id == "worker-8" || id == "last-success" {
			a.stop(t)
			a.start(t)
		}
		response = send(step)
	}
	s := load()
	require.Equal(t, fusion.Completed, s.State)
	require.Equal(t, 1, s.Turn)
	require.Equal(t, started, s.TurnStarted)
	require.Equal(t, 20, s.Profile.MaxRequests)
	require.Equal(t, 5.0, s.Profile.MaxCostUSD)
	require.Equal(t, 900, s.Profile.MaxWallSeconds)
	require.Equal(t, 18, s.TurnRequests)
	require.EqualValues(t, 18, native.Load())
	require.Equal(t, 16, s.TurnTools)
	require.Equal(t, fusion.BudgetClosed, s.Requests[len(s.Requests)-1].Outcome)
	require.Contains(t, response.Get("output.0.content.0.text").String(), "does not certify")
	require.Contains(t, response.Get("output.0.content.0.text").String(), "unfinished")
	require.Contains(t, s.Handoffs[0].Report, "Partial Sidekick report")
	require.Empty(t, s.QueuedTools)
	require.Nil(t, s.Pending)
	resultTrace := s.Requests[len(s.Requests)-1].ToolResult
	require.NotNil(t, resultTrace)
	require.Equal(t, "last-success", resultTrace.InternalID)
	require.Equal(t, fusion.Sidekick, resultTrace.Role)
	require.Equal(t, s.Attempts[15].ID, resultTrace.SourceAttemptID)
	b, err := rdb.Get(context.Background(), "devin:fusion:index:"+scope+":attempt:"+fusion.Digest(session+":"+s.Attempts[16].ID)).Bytes()
	require.NoError(t, err)
	var reportAttempt fusion.Attempt
	require.NoError(t, json.Unmarshal(b, &reportAttempt))
	require.Contains(t, string(reportAttempt.RequestSnapshot), "private-tool-result-last-success")
	db := agentDatabase(t, a)
	var rows, unique, dedup, leadRows, workerRows int
	var cost, workerCost float64
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT count(*),count(DISTINCT request_id),count(*) FILTER(WHERE upstream_model=$2),count(*) FILTER(WHERE upstream_model=$3),coalesce(sum(actual_cost),0),coalesce(sum(actual_cost) FILTER(WHERE upstream_model=$3),0) FROM usage_logs WHERE api_key_id=$1`, keyID, lead, worker).Scan(&rows, &unique, &leadRows, &workerRows, &cost, &workerCost) == nil && rows == 18
	}, 10*time.Second, 100*time.Millisecond)
	require.Equal(t, 18, unique)
	require.Equal(t, 5, leadRows)
	require.Equal(t, 13, workerRows)
	require.Zero(t, workerCost)
	require.InDelta(t, cost, s.TurnCostUSD, 1e-10)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE api_key_id=$1`, keyID).Scan(&dedup))
	require.Equal(t, 18, dedup)
	account := api(t, a, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil)
	require.Empty(t, account.Get("extra.devin_fusion_profiles").Array(), "the opt-in must not write account profiles")
}
