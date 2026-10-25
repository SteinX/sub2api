//go:build devine2e

package devine2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const longGoal = "Preserve the public API and use only synthetic fragment files"
const longFinal = "E2E_LONG_OK phase1=36 phase2=100"

// This is a public-protocol tool executor, not an OpenCode/OMO/OMP process.
// The compiled backend, SQL/Redis state, native serialization and billing are real.
func TestDevinLongToolWorkflow(t *testing.T) {
	if os.Getenv("DEVIN_E2E_LONG_TASK") != "true" {
		t.Skip("use make test-devin-long-e2e for the bounded long workflow")
	}
	mode := os.Getenv("DEVIN_E2E_MODE")
	var fixture *nativeFixture
	if mode == "mock" {
		fixture = newNativeFixture(t)
		fixture.agentMode.Store(true)
		fixture.longWorkflow = &longNativeWorkflow{}
	}
	if mode == "live" {
		require.Equal(t, "true", os.Getenv("DEVIN_E2E_LONG_ALLOW_PAID"), "approve the long-task plan separately from the short client matrix")
	}
	runDevinCloudGatewaySuite(t, mode, fixture, "long")
}

func longTurnBudget(raw string) (float64, error) {
	if raw == "" {
		return .10, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 || value > .50 {
		return 0, fmt.Errorf("long-task turn budget must be positive and no greater than $0.50")
	}
	return value, nil
}

func TestLongWorkflowBudgetBounds(t *testing.T) {
	for _, raw := range []string{"NaN", "Inf", "0", "-1", ".51", "invalid"} {
		_, err := longTurnBudget(raw)
		require.Error(t, err)
	}
	for raw, want := range map[string]float64{"": .10, ".05": .05, ".5": .50} {
		got, err := longTurnBudget(raw)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func runLongWorkflow(t *testing.T, app *application, fixture *nativeFixture, mode, admin, key string, keyID, accountID int64, models []*pb.ClientModelConfig) {
	t.Helper()
	budget, err := longTurnBudget(os.Getenv("DEVIN_E2E_LONG_MAX_TURN_USD"))
	require.NoError(t, err)
	lead, sidekick, err := selectAgentModels(models)
	require.NoError(t, err)
	pricesReady := fixture != nil
	var prices []map[string]any
	if fixture == nil {
		// Choose the least expensive identified price, using the same concrete
		// member in separate role lanes to bound the cost of repeated reads.
		best := math.Inf(1)
		for _, candidate := range eligibleAgentModels(models) {
			id := devin.ModelID(candidate)
			price := api(t, app, "GET", "/admin/channels/model-pricing?model="+url.QueryEscape(id), admin, nil)
			identified := price.Get("found").Bool() && price.Get("identified").Bool() && price.Get("output_price").Float() > 0
			prices = append(prices, map[string]any{"model": id, "identified": identified, "input_price": price.Get("input_price").Float(), "output_price": price.Get("output_price").Float()})
			score := 18000*price.Get("input_price").Float() + 1024*price.Get("output_price").Float()
			if identified && score < best {
				lead, sidekick, best, pricesReady = id, id, score, true
			}
		}
	}
	if mode == "live" {
		expected := os.Getenv("DEVIN_E2E_LONG_EXPECT_MODEL")
		require.NotEmpty(t, expected, "pin the reviewed long-task model before generation")
		require.Equal(t, expected, lead, "model selection changed since the reviewed plan; stop before generation")
	}
	profile := fusion.DefaultProfile("fusion-e2e-long", lead, sidekick)
	profile.MaxRequests, profile.MaxHandoffs, profile.MaxTools = 64, 2, 20
	profile.MaxOutputTokens, profile.MaxWallSeconds, profile.MaxCostUSD = 1024, 300, budget
	profile.MaxContextBytes = 24000
	profile.Compaction = &fusion.CompactionPolicy{Enabled: true, Lead: []int{18000, 21000, 23000}, Sidekick: []int{10000, 12000, 18000}, MaxAttempts: 4, TimeoutSeconds: 30, MaxOutputTokens: 1024}
	profile.Keepalive = &fusion.KeepalivePolicy{Enabled: false}
	require.NoError(t, profile.Validate())
	quota, err := agentKeyQuota(mode, "matrix", os.Getenv("DEVIN_E2E_AGENT_KEY_QUOTA"))
	require.NoError(t, err)
	writeAgentArtifact(t, "long-plan.json", map[string]any{
		"mode": mode, "profile": profile, "prices_ready": pricesReady, "prices": prices,
		"max_public_requests": 24, "concurrency": 1, "gateway_key_quota_usd": quota,
		"provider_spending_hard_cap": false, "automatic_retries": 0,
		"task":     "two handoffs; read 16 synthetic fragments; preserve the goal through compaction; review two sums; restart after read 8",
		"boundary": "public Responses API tool executor; not a pinned CLI client process",
	})
	if mode == "plan" {
		t.Logf("long-task plan ready; identified prices=%v; no generation requested", pricesReady)
		return
	}
	require.True(t, pricesReady, "no identified concrete model price; stop before generation")
	current := api(t, app, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil)
	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(current.Get("extra").Raw), &extra))
	extra["devin_fusion_enabled"], extra[fusion.ExtraProfiles] = true, []fusion.Profile{profile}
	api(t, app, "PUT", fmt.Sprintf("/admin/accounts/%d", accountID), admin, map[string]any{"extra": extra, "concurrency": 1})
	require.Equal(t, int64(1), api(t, app, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil).Get("concurrency").Int(), "one account slot keeps summary inference inline")
	defer savePrivateAgentUsage(t, app, keyID, "long-protocol", "fusion")
	root := t.TempDir()
	for part := 1; part <= 16; part++ {
		data := fmt.Sprintf("PART=%d VALUE=%d\n%s\nEND_PART=%d\n", part, part, strings.Repeat("Synthetic historical evidence. Preserve the public API. ", 25), part)
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("fragment-%02d.txt", part)), []byte(data), 0600))
	}
	tool := map[string]any{"type": "function", "name": "read_fragment", "description": "Read one synthetic fragment; part is 1 through 16", "parameters": map[string]any{"type": "object", "properties": map[string]any{"part": map[string]any{"type": "integer", "minimum": 1, "maximum": 16}}, "required": []string{"part"}, "additionalProperties": false}}
	goal := longGoal + ". Lead must delegate exactly two phases to Sidekick: phase 1 reads parts 1-8, phase 2 reads parts 9-16. Sidekick must read each part exactly once, one tool call at a time, and report the sum of VALUE fields with evidence. Lead reviews both reports and returns E2E_LONG_OK phase1=<sum> phase2=<sum>. Retain completed parts and running sums in any context summary. No other tools or delegated client agents."
	var input any = goal
	var sessionID string
	seenIDs, seenParts := map[string]bool{}, map[int]bool{}
	requests, restarted, finished := 0, false, false
	started := time.Now()
	defer func() {
		writeAgentArtifact(t, "long-workflow.json", map[string]any{"passed": !t.Failed() && finished, "public_requests": requests, "unique_tools": len(seenIDs), "unique_parts": len(seenParts), "backend_restarted": restarted, "seconds": time.Since(started).Seconds(), "mode": mode, "boundary": "public protocol executor; not a CLI process"})
	}()
	for requests < 24 && !finished {
		payload := map[string]any{"model": profile.PublicModel, "input": input, "tools": []any{tool}, "max_output_tokens": 1024, "stream": false}
		requests++ // A failed/uncertain send consumes admission and is never retried.
		response, returnedSession := longRequest(t, app, key, sessionID, payload)
		if sessionID == "" {
			sessionID = returnedSession
		}
		require.NotEmpty(t, sessionID)
		require.Equal(t, sessionID, returnedSession)
		var results []apicompat.ResponsesInputItem
		var text strings.Builder
		for _, item := range response.Output {
			if item.Type != "function_call" {
				for _, content := range item.Content {
					text.WriteString(content.Text)
				}
				continue
			}
			require.Equal(t, "read_fragment", item.Name)
			require.False(t, seenIDs[item.CallID], "duplicate public tool ID")
			var args struct {
				Part int `json:"part"`
			}
			decoder := json.NewDecoder(strings.NewReader(item.Arguments))
			decoder.DisallowUnknownFields()
			require.NoError(t, decoder.Decode(&args))
			require.GreaterOrEqual(t, args.Part, 1)
			require.LessOrEqual(t, args.Part, 16)
			require.False(t, seenParts[args.Part], "fragment read repeated")
			seenIDs[item.CallID], seenParts[args.Part] = true, true
			data, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("fragment-%02d.txt", args.Part)))
			require.NoError(t, err)
			results = append(results, apicompat.ResponsesInputItem{Type: "function_call_output", CallID: item.CallID, Output: string(data)})
		}
		if len(results) == 0 {
			require.Contains(t, text.String(), longFinal, "reviewed answer must contain independently checked sums")
			require.Len(t, seenParts, 16)
			finished = true
			break
		}
		require.Len(t, results, 1, "the long workflow is serial")
		if len(seenParts) == 8 && !restarted {
			app.stop(t)
			app.start(t)
			restarted = true
		}
		input = results
	}
	require.True(t, finished, "bounded public request allowance exhausted")
	verifyAgentFusion(t, app, keyID, []agentRequest{{Session: sessionID, Status: 200, Forwarded: true}}, "long-protocol")
	db := agentDatabase(t, app)
	var userID int64
	require.NoError(t, db.QueryRow("SELECT user_id FROM api_keys WHERE id=$1", keyID).Scan(&userID))
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT")})
	defer rdb.Close()
	stored, err := rdb.Get(context.Background(), "devin:fusion:session:"+fusion.Scope(userID, keyID)+":"+fusion.Digest(sessionID)).Bytes()
	require.NoError(t, err)
	var session fusion.Session
	require.NoError(t, json.Unmarshal(stored, &session))
	require.Len(t, session.Handoffs, 2)
	require.NotNil(t, session.Contexts[fusion.Sidekick])
	require.Positive(t, session.Contexts[fusion.Sidekick].Epoch, "Sidekick must actually compact during this task")
	require.Positive(t, session.Maintenance.CompactionsApplied)
	require.Positive(t, session.Maintenance.CompactionCostUSD)
	summaryRows := 0
	for _, lane := range session.Contexts {
		for _, archive := range lane.Archives {
			journal, err := rdb.Get(context.Background(), "devin:fusion:index:"+session.Scope+":compaction:"+fusion.Digest(session.ID+":"+archive.JobID)).Bytes()
			require.NoError(t, err)
			var record struct {
				Attempt fusion.Attempt `json:"attempt"`
			}
			require.NoError(t, json.Unmarshal(journal, &record))
			require.Equal(t, fusion.Compaction, record.Attempt.Purpose)
			require.Equal(t, "completed", record.Attempt.Status)
			require.True(t, record.Attempt.Settled)
			require.Eventually(t, func() bool {
				var count int
				var cost float64
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				return db.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(actual_cost),0) FROM usage_logs WHERE request_id=$1 AND api_key_id=$2", "fusion:"+record.Attempt.ID, keyID).Scan(&count, &cost) == nil && count == 1 && math.Abs(cost-record.Attempt.CostUSD) <= 1e-9
			}, 10*time.Second, 200*time.Millisecond, "summary usage missing or duplicated")
			summaryRows++
		}
	}
	require.Equal(t, session.Maintenance.CompactionsApplied, summaryRows)
	leadBytes, err := json.Marshal(session.Lead)
	require.NoError(t, err)
	require.Contains(t, string(leadBytes), longGoal)
	require.NotContains(t, string(leadBytes), strings.Repeat("Synthetic historical evidence. Preserve the public API. ", 3), "raw Sidekick evidence leaked into the Lead lane")
	writeAgentArtifact(t, "long-context.json", map[string]any{"handoffs": len(session.Handoffs), "compactions_applied": session.Maintenance.CompactionsApplied, "sidekick_epoch": session.Contexts[fusion.Sidekick].Epoch, "goal_preserved": true, "raw_tool_results_isolated": true, "maintenance": session.Maintenance})
	if fixture != nil {
		fixture.longWorkflow.mu.Lock()
		defer fixture.longWorkflow.mu.Unlock()
		require.Equal(t, 3, fixture.longWorkflow.leadCalls)
		require.Equal(t, 16, fixture.longWorkflow.tools)
		require.Positive(t, fixture.longWorkflow.summaries)
	}
}

func longRequest(t *testing.T, app *application, key, sessionID string, payload any) (*apicompat.ResponsesResponse, string) {
	t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	req, err := http.NewRequest("POST", app.base+"/v1/responses", bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set(fusion.HeaderSession, sessionID)
	}
	response, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	require.NoError(t, err, "long-task send failed or uncertain; no retry permitted")
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	require.NoError(t, err)
	// Never emit raw model/error bodies, identifiers or credentials.
	if response.StatusCode != http.StatusOK {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &envelope) == nil && strings.HasPrefix(envelope.Error.Code, "devin_fusion_") {
			t.Logf("long-task local rejection code=%s; generation stopped", envelope.Error.Code)
		}
	}
	require.Equal(t, http.StatusOK, response.StatusCode, "long-task request failed; body withheld")
	var result apicompat.ResponsesResponse
	require.NoError(t, json.Unmarshal(body, &result))
	return &result, response.Header.Get(fusion.HeaderSession)
}

type longNativeWorkflow struct {
	mu                                             sync.Mutex
	leadCalls, phase, phaseTools, tools, summaries int
}

func (w *longNativeWorkflow) reply(request *pb.GetChatMessageRequest, reply *pb.GetChatMessageResponse) {
	w.mu.Lock()
	defer w.mu.Unlock()
	reply.DeltaText, reply.DeltaToolCalls = longFinal, nil
	reply.StopReason = pb.StopReason_STOP_REASON_STOP_PATTERN
	reply.Usage.BillingModelUid = request.ChatModelUid
	if len(request.Tools) == 0 {
		w.summaries++
		reply.DeltaText = fmt.Sprintf(`{"goal":%q,"current_state":"phase %d; completed %d reads in this phase","decisions":["Read each fragment once"],"changes":[],"evidence":["Tool results are retained in the archived prefix"],"checks":[],"open_questions":[],"next_steps":["Continue the active phase"],"files":["synthetic fragments"]}`, longGoal, w.phase, w.phaseTools)
		return
	}
	call := func(name, args, id string) {
		reply.DeltaText = ""
		reply.DeltaToolCalls = []*pb.ChatToolCall{{Id: id, Name: name, ArgumentsJson: args}}
		reply.StopReason = pb.StopReason_STOP_REASON_FUNCTION_CALL
	}
	if request.ChatModelUid == fixtureModel {
		w.leadCalls++
		if w.leadCalls <= 2 {
			w.phase, w.phaseTools = w.leadCalls, 0
			call(fusion.DelegateTool, fmt.Sprintf(`{"task":"Read synthetic parts %d through %d and report their VALUE sum","constraints":%q,"acceptance_criteria":"Report the sum with checked parts; retain progress in compaction"}`, (w.phase-1)*8+1, w.phase*8, longGoal), fmt.Sprintf("long-delegate-%d", w.phase))
		}
		return
	}
	if w.phaseTools < 8 {
		w.phaseTools++
		w.tools++
		part := (w.phase-1)*8 + w.phaseTools
		call("read_fragment", fmt.Sprintf(`{"part":%d}`, part), fmt.Sprintf("long-read-%d", part))
		return
	}
	reply.DeltaText = fmt.Sprintf("Reviewed phase %d: all eight fragment values checked; sum=%d. %s.", w.phase, map[int]int{1: 36, 2: 100}[w.phase], longGoal)
}
