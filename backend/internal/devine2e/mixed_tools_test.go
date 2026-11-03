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

// Synthetic native replies reproduce the confirmed mixed-call condition, not
// the missing real-user SSE frames. Exercise public SSE, restart, native history
// and SQL settlement through the production binary; this is not real PAT E2E.
func TestDevinFusionMixedToolsPublicResponses(t *testing.T) {
	if os.Getenv("DEVIN_E2E_MODE") != "mock" {
		t.Skip("requires isolated mock dependencies")
	}
	f := newNativeFixture(t)
	f.agentMode.Store(true)
	var native atomic.Int32
	f.chatReply = func(req *pb.GetChatMessageRequest, reply *pb.GetChatMessageResponse) {
		n := native.Add(1)
		wantModel := fixtureModel
		if n == 4 || n == 5 || n == 7 || n == 8 {
			wantModel = fixtureSidekick
		}
		assert.Equal(t, wantModel, req.ChatModelUid)
		reply.DeltaText, reply.DeltaToolCalls = "", nil
		reply.Usage.BillingModelUid = req.ChatModelUid
		call := func(id, name string) *pb.ChatToolCall {
			return &pb.ChatToolCall{Id: id, Name: name, ArgumentsJson: `{"path":"fixture.go"}`}
		}
		switch n {
		case 1:
			reply.DeltaToolCalls = []*pb.ChatToolCall{call("prior-read", "read_file")}
		case 2:
			reply.DeltaToolCalls = []*pb.ChatToolCall{call("prior-edit", "edit_file")}
		case 3:
			reply.DeltaToolCalls = []*pb.ChatToolCall{call("lead-read", "read_file"), {
				Id: "delegate", Name: fusion.DelegateTool, ArgumentsJson: `{"task":"Verify the synthetic fixture","acceptance_criteria":"Report evidence"}`,
			}, call("lead-edit", "edit_file")}
		case 4:
			reply.DeltaToolCalls = []*pb.ChatToolCall{call("worker-read", "read_file"), call("worker-edit", "edit_file")}
		case 5:
			var ids []string
			for _, prompt := range req.ChatMessagePrompts {
				if prompt.ToolCallId != "" {
					ids = append(ids, prompt.ToolCallId)
				}
			}
			assert.Equal(t, []string{"worker-read", "worker-edit"}, ids)
			reply.DeltaText = "Private first report: fixture test failed; rework required"
		case 6:
			var ids []string
			for _, prompt := range req.ChatMessagePrompts {
				if prompt.ToolCallId != "" {
					ids = append(ids, prompt.ToolCallId)
				}
			}
			assert.Equal(t, []string{"prior-read", "prior-edit", "lead-read", "delegate", "lead-edit"}, ids)
			assert.NotContains(t, fmt.Sprint(req.ChatMessagePrompts), "result-worker-read")
			assert.Contains(t, fmt.Sprint(req.ChatMessagePrompts), "Private first report: fixture test failed; rework required")
			reply.DeltaToolCalls = []*pb.ChatToolCall{{
				Id: "delegate-rework", Name: fusion.DelegateTool, ArgumentsJson: `{"task":"Rework the original fixture after the failed test","acceptance_criteria":"Report passing evidence for the original task"}`,
			}, call("lead-verify", "read_file")}
		case 7:
			prompts := fmt.Sprint(req.ChatMessagePrompts)
			assert.Contains(t, prompts, "result-worker-read")
			assert.Contains(t, prompts, "result-worker-edit")
			assert.Contains(t, prompts, "Private first report: fixture test failed; rework required")
			assert.Contains(t, prompts, "Rework the original fixture after the failed test")
			assert.NotContains(t, prompts, "result-lead-edit")
			reply.DeltaToolCalls = []*pb.ChatToolCall{call("worker-rework", "edit_file")}
		case 8:
			var ids []string
			for _, prompt := range req.ChatMessagePrompts {
				if prompt.ToolCallId != "" {
					ids = append(ids, prompt.ToolCallId)
				}
			}
			assert.Equal(t, []string{"worker-read", "worker-edit", "worker-rework"}, ids)
			assert.Contains(t, fmt.Sprint(req.ChatMessagePrompts), "result-worker-rework")
			reply.DeltaText = "Private rework report: original fixture now passes"
		case 9:
			var ids []string
			for _, prompt := range req.ChatMessagePrompts {
				if prompt.ToolCallId != "" {
					ids = append(ids, prompt.ToolCallId)
				}
			}
			assert.Equal(t, []string{"prior-read", "prior-edit", "lead-read", "delegate", "lead-edit", "delegate-rework", "lead-verify"}, ids)
			prompts := fmt.Sprint(req.ChatMessagePrompts)
			assert.Contains(t, prompts, "Verify the fixture.")
			assert.Contains(t, prompts, "Private first report: fixture test failed; rework required")
			assert.Contains(t, prompts, "Private rework report: original fixture now passes")
			assert.NotContains(t, prompts, "result-worker-rework")
			reply.DeltaText = "Reviewed the fixture successfully."
		default:
			t.Errorf("unexpected repeat native generation %d", n)
		}
		reply.StopReason = pb.StopReason_STOP_REASON_STOP_PATTERN
		if len(reply.DeltaToolCalls) > 0 {
			reply.StopReason = pb.StopReason_STOP_REASON_FUNCTION_CALL
		}
	}
	a := newApplication(t, f, 2)
	admin := login(t, a, "admin@devin-e2e.invalid", a.password)
	compliance := api(t, a, "GET", "/admin/compliance", admin, nil)
	api(t, a, "POST", "/admin/compliance/accept", admin, map[string]string{"language": "en", "phrase": compliance.Get("ack_phrase_en").String()})
	group := api(t, a, "POST", "/admin/groups", admin, map[string]any{
		"name": "mixed-tools", "platform": "devin", "rate_multiplier": 1, "subscription_type": "standard", "is_exclusive": true,
		"model_pricing": []any{
			map[string]any{"models": []string{fixtureModel}, "platform": "devin", "billing_mode": "token", "input_price": 1e-6, "output_price": 2e-6, "cache_write_price": 3e-6, "cache_read_price": .5e-6},
			map[string]any{"models": []string{fixtureSidekick}, "platform": "devin", "billing_mode": "token", "input_price": 2e-6, "output_price": 4e-6, "cache_write_price": 6e-6, "cache_read_price": 1e-6},
		}})
	groupID := group.Get("id").Int()
	accountID, err := addPATAccount(t, a, admin, "cog_e2e_mixed_tools", groupID, f.registerProxy(t, a, admin))
	require.NoError(t, err)
	const publicModel = "fusion-e2e-mixed-tools"
	account := api(t, a, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil)
	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(account.Get("extra").Raw), &extra))
	extra["devin_fusion_profiles"] = []fusion.Profile{fusion.DefaultProfile(publicModel, fixtureModel, fixtureSidekick)}
	api(t, a, "PUT", fmt.Sprintf("/admin/accounts/%d", accountID), admin, map[string]any{"extra": extra})
	password := randomID()
	a.redactor.addSecret(password)
	createdUser := api(t, a, "POST", "/admin/users", admin, map[string]any{"email": "mixed@devin-e2e.invalid", "password": password, "balance": 100, "concurrency": 4, "allowed_groups": []int64{groupID}})
	user := login(t, a, "mixed@devin-e2e.invalid", password)
	created := api(t, a, "POST", "/keys", user, map[string]any{"name": "mixed-tools", "group_id": groupID})
	key, keyID := created.Get("key").String(), created.Get("id").Int()
	a.redactor.addSecret(key)
	db := agentDatabase(t, a)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT"), Password: os.Getenv("DEVIN_E2E_REDIS_PASSWORD"), DB: a.redisDB})
	t.Cleanup(func() { _ = rdb.Close() })
	engine := fusion.New(devin.NewRedisSessionStore(rdb))
	session := "mixed-" + randomID()
	load := func() *fusion.Session {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s, _, err := engine.Load(ctx, fusion.Scope(createdUser.Get("id").Int(), keyID), session)
		require.NoError(t, err)
		return s
	}
	history := []any{map[string]any{"type": "message", "role": "user", "content": "Verify the fixture."}}
	tools := []any{}
	for _, name := range []string{"read_file", "edit_file"} {
		tools = append(tools, map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]string{"type": "string"}}}})
	}
	send := func(step int) gjson.Result {
		payload, err := json.Marshal(map[string]any{"model": publicModel, "stream": true, "store": false, "max_output_tokens": 1024, "input": history, "tools": tools})
		require.NoError(t, err)
		request := func() ([]byte, gjson.Result) {
			req, err := http.NewRequest("POST", a.base+"/v1/responses", bytes.NewReader(payload))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(fusion.HeaderSession, session)
			req.Header.Set("Idempotency-Key", fmt.Sprintf("step-%d", step))
			resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode, "public error code=%s (body withheld)", gjson.GetBytes(body, "error.code").String())
			require.Equal(t, session, resp.Header.Get(fusion.HeaderSession))
			var complete gjson.Result
			for _, line := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(line, "data: ") {
					event := gjson.Parse(strings.TrimPrefix(line, "data: "))
					require.NotEqual(t, "response.failed", event.Get("type").String())
					require.NotEqual(t, "error", event.Get("type").String())
					if event.Get("type").String() == "response.completed" {
						complete = event.Get("response")
					}
				}
			}
			require.True(t, complete.Exists(), "SSE keepalives are not completion")
			return body, complete
		}
		_, complete := request()
		before := native.Load()
		_, replay := request()
		require.Equal(t, complete.Get("id").String(), replay.Get("id").String())
		require.Equal(t, before, native.Load(), "idempotent publication cannot repeat paid inference")
		return complete
	}
	response := send(0)
	started := load().TurnStarted
	for n, id := range []string{"prior-read", "prior-edit", "lead-read", "worker-read", "worker-edit", "lead-edit", "worker-rework", "lead-verify"} {
		s := load()
		require.Equal(t, 1, s.Turn)
		require.Equal(t, started, s.TurnStarted)
		require.Equal(t, id, s.Pending.InternalID)
		call := response.Get("output.0")
		require.Equal(t, "function_call", call.Get("type").String())
		require.NotEqual(t, fusion.DelegateTool, call.Get("name").String())
		history = append(history, map[string]any{"type": "function_call", "call_id": call.Get("call_id").String(), "name": call.Get("name").String(), "arguments": call.Get("arguments").String()},
			map[string]any{"type": "function_call_output", "call_id": call.Get("call_id").String(), "output": "result-" + id})
		if id == "worker-read" || id == "worker-rework" {
			a.stop(t)
			a.start(t) // Restore both the child batch and suspended Lead tail.
		}
		response = send(n + 1)
	}
	require.Equal(t, "Reviewed the fixture successfully.", response.Get("output.0.content.0.text").String())
	s := load()
	require.Equal(t, fusion.Completed, s.State)
	require.Equal(t, 1, s.Turn)
	require.Equal(t, started, s.TurnStarted)
	require.Equal(t, 9, s.TurnRequests)
	require.Equal(t, 8, s.TurnTools)
	require.Equal(t, 2, s.TurnHandoffs)
	require.Len(t, s.Handoffs, 2)
	require.Equal(t, "delegate", s.Handoffs[0].CallID)
	require.Equal(t, "Private first report: fixture test failed; rework required", s.Handoffs[0].Report)
	require.Equal(t, "delegate-rework", s.Handoffs[1].CallID)
	require.Equal(t, "Private rework report: original fixture now passes", s.Handoffs[1].Report)
	require.Empty(t, s.QueuedTools)
	require.Nil(t, s.Pending)
	require.Len(t, s.Attempts, 9)
	for _, attempt := range s.Attempts {
		require.True(t, attempt.Settled)
	}
	var rows, unique, leadRows, sidekickRows int
	var cost float64
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT count(*), count(DISTINCT request_id), count(*) FILTER(WHERE upstream_model=$2), count(*) FILTER(WHERE upstream_model=$3), coalesce(sum(actual_cost),0) FROM usage_logs WHERE api_key_id=$1`, keyID, fixtureModel, fixtureSidekick).
			Scan(&rows, &unique, &leadRows, &sidekickRows, &cost) == nil && rows == 9
	}, 10*time.Second, 100*time.Millisecond)
	require.Equal(t, 9, unique)
	var dedup int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM usage_billing_dedup WHERE api_key_id=$1`, keyID).Scan(&dedup))
	require.Equal(t, 9, dedup)
	require.Equal(t, 5, leadRows)
	require.Equal(t, 4, sidekickRows)
	require.InDelta(t, .0003315, cost, 1e-10)
	require.InDelta(t, cost, s.TurnCostUSD, 1e-10)
	for _, member := range []struct {
		model     string
		cacheRead int
		cacheCost float64
		cost      float64
	}{
		{fixtureModel, 25, .0000125, .0001275},
		{fixtureSidekick, 20, .00002, .000204},
	} {
		var cacheRead int
		var cacheCost, memberCost float64
		require.NoError(t, db.QueryRow(`SELECT coalesce(sum(cache_read_tokens),0), coalesce(sum(cache_read_cost),0), coalesce(sum(actual_cost),0) FROM usage_logs WHERE api_key_id=$1 AND upstream_model=$2`, keyID, member.model).Scan(&cacheRead, &cacheCost, &memberCost))
		require.Equal(t, member.cacheRead, cacheRead)
		require.InDelta(t, member.cacheCost, cacheCost, 1e-10)
		require.InDelta(t, member.cost, memberCost, 1e-10)
	}
	require.EqualValues(t, 9, native.Load())
}
