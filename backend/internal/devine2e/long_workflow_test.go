//go:build devine2e

package devine2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
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
		fault := os.Getenv("DEVIN_E2E_LONG_MOCK_FAILURE")
		require.Contains(t, []string{"", "sidekick_incomplete_tool"}, fault)
		fixture.longWorkflow.failSidekick = fault != ""
	}
	if mode != "mock" {
		require.Empty(t, os.Getenv("DEVIN_E2E_LONG_MOCK_FAILURE"), "synthetic faults cannot be selected for real accounts")
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

func longPublicRequestLimit(raw string) (int, error) {
	if raw == "" {
		return 24, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 24 {
		return 0, fmt.Errorf("long-task public request limit must be between 1 and 24")
	}
	return value, nil
}

func TestLongWorkflowBudgetBounds(t *testing.T) {
	for _, raw := range []string{"0", "25", "-1", "NaN", "1.5"} {
		_, err := longPublicRequestLimit(raw)
		require.Error(t, err)
	}
	limit, err := longPublicRequestLimit("23")
	require.NoError(t, err)
	require.Equal(t, 23, limit)
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
	maxPublicRequests, err := longPublicRequestLimit(os.Getenv("DEVIN_E2E_LONG_MAX_PUBLIC_REQUESTS"))
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
		require.True(t, expected == lead, "model selection changed since the reviewed plan; stop before generation")
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
		"max_public_requests": maxPublicRequests, "concurrency": 1, "gateway_key_quota_usd": quota,
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
	values, sums := make([]int, 16), [2]int{}
	for part := 1; part <= 16; part++ {
		sample, err := rand.Int(rand.Reader, big.NewInt(900))
		require.NoError(t, err)
		values[part-1] = int(sample.Int64()) + 100
		sums[(part-1)/8] += values[part-1]
		data := fmt.Sprintf("PART=%d VALUE=%d\n%s\nEND_PART=%d\n", part, values[part-1], strings.Repeat("Synthetic historical evidence. Preserve the public API. ", 25), part)
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("fragment-%02d.txt", part)), []byte(data), 0600))
	}
	expectedFinal := fmt.Sprintf("E2E_LONG_OK phase1=%d phase2=%d", sums[0], sums[1])
	if fixture != nil {
		fixture.longWorkflow.mu.Lock()
		fixture.longWorkflow.values, fixture.longWorkflow.sums, fixture.longWorkflow.final = values, sums, expectedFinal
		fixture.longWorkflow.mu.Unlock()
	}
	tool := map[string]any{"type": "function", "name": "read_fragment", "description": "Read one synthetic fragment; part is 1 through 16", "parameters": map[string]any{"type": "object", "properties": map[string]any{"part": map[string]any{"type": "integer", "minimum": 1, "maximum": 16}}, "required": []string{"part"}, "additionalProperties": false}}
	goal := longGoal + ". Lead must delegate exactly two phases to Sidekick: phase 1 reads parts 1-8, phase 2 reads parts 9-16. Sidekick must read each part exactly once, one tool call at a time, and report the sum of VALUE fields with evidence. Lead reviews both reports and returns E2E_LONG_OK phase1=<sum> phase2=<sum>. Retain completed parts and running sums in any context summary. No other tools or delegated client agents."
	var input any = goal
	var sessionID string
	defer func() {
		if t.Failed() && sessionID != "" {
			// Retain structural state/cost before the disposable Redis is removed.
			// This helper does not serialize tool contents or credentials.
			savePrivateLongFailure(t, app, keyID, sessionID)
		}
	}()
	seenIDs, seenParts := map[string]bool{}, map[int]bool{}
	requests, restarted, finished := 0, false, false
	started := time.Now()
	defer func() {
		writeAgentArtifact(t, "long-workflow.json", map[string]any{"passed": !t.Failed() && finished, "public_requests": requests, "unique_tools": len(seenIDs), "unique_parts": len(seenParts), "backend_restarted": restarted, "seconds": time.Since(started).Seconds(), "mode": mode, "boundary": "public protocol executor; not a CLI process"})
	}()
	for requests < maxPublicRequests && !finished {
		payload := map[string]any{"model": profile.PublicModel, "input": input, "tools": []any{tool}, "max_output_tokens": 1024, "stream": false}
		requests++ // A failed/uncertain send consumes admission and is never retried.
		response, returnedSession := longRequest(t, app, key, &sessionID, payload)
		if sessionID == "" {
			sessionID = returnedSession
		}
		require.NotEmpty(t, sessionID)
		require.True(t, sessionID == returnedSession, "Fusion session identity changed; values withheld")
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
			require.True(t, decoder.Decode(&args) == nil, "invalid tool arguments; details withheld")
			require.GreaterOrEqual(t, args.Part, 1)
			require.LessOrEqual(t, args.Part, 16)
			require.False(t, seenParts[args.Part], "fragment read repeated")
			seenIDs[item.CallID], seenParts[args.Part] = true, true
			data, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("fragment-%02d.txt", args.Part)))
			require.NoError(t, err)
			results = append(results, apicompat.ResponsesInputItem{Type: "function_call_output", CallID: item.CallID, Output: string(data)})
		}
		if len(results) == 0 {
			require.True(t, strings.Contains(text.String(), expectedFinal), "reviewed answer must contain independently checked sums; text withheld")
			require.Len(t, seenParts, 16)
			finished = true
			break
		}
		require.Equal(t, 1, len(results), "the long workflow is serial; results withheld")
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
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT"), Password: os.Getenv("DEVIN_E2E_REDIS_PASSWORD")})
	defer rdb.Close()
	stored, err := rdb.Get(context.Background(), "devin:fusion:session:"+fusion.Scope(userID, keyID)+":"+fusion.Digest(sessionID)).Bytes()
	require.NoError(t, err)
	var session fusion.Session
	require.NoError(t, json.Unmarshal(stored, &session))
	require.Equal(t, 2, len(session.Handoffs), "unexpected handoff count; briefs and reports withheld")
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
	require.True(t, strings.Contains(string(leadBytes), longGoal), "original goal missing; history withheld")
	require.False(t, strings.Contains(string(leadBytes), strings.Repeat("Synthetic historical evidence. Preserve the public API. ", 3)), "raw Sidekick evidence leaked into the Lead lane; history withheld")
	writeAgentArtifact(t, "long-context.json", map[string]any{"handoffs": len(session.Handoffs), "compactions_applied": session.Maintenance.CompactionsApplied, "sidekick_epoch": session.Contexts[fusion.Sidekick].Epoch, "goal_preserved": true, "raw_tool_results_isolated": true, "maintenance": session.Maintenance})
	if fixture != nil {
		fixture.verifyAgentNativeContexts(t, 0, "long-protocol", lead, sidekick)
		fixture.longWorkflow.mu.Lock()
		defer fixture.longWorkflow.mu.Unlock()
		require.Equal(t, 3, fixture.longWorkflow.leadCalls)
		require.Equal(t, 16, fixture.longWorkflow.tools)
		require.Positive(t, fixture.longWorkflow.summaries)
	}
}

func savePrivateLongFailure(t *testing.T, app *application, keyID int64, sessionID string) {
	t.Helper()
	evidence := map[string]any{"available": false}
	defer func() { writeAgentArtifact(t, "private-long-failure-state.json", evidence) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	db := agentDatabase(t, app)
	var userID int64
	if db.QueryRowContext(ctx, "SELECT user_id FROM api_keys WHERE id=$1", keyID).Scan(&userID) != nil {
		return
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT"), Password: os.Getenv("DEVIN_E2E_REDIS_PASSWORD")})
	defer rdb.Close()
	stored, err := rdb.Get(ctx, "devin:fusion:session:"+fusion.Scope(userID, keyID)+":"+fusion.Digest(sessionID)).Bytes()
	if err != nil {
		return
	}
	var session fusion.Session
	if json.Unmarshal(stored, &session) != nil {
		return
	}
	var attempts []map[string]any
	for _, a := range session.Attempts {
		var toolShapes []map[string]any
		if a.Response != nil {
			for _, item := range a.Response.Output {
				if item.Type != "function_call" || len(toolShapes) >= 20 {
					continue
				}
				toolShapes = append(toolShapes, map[string]any{
					"declared_test_tool": item.Name == "read_fragment" || item.Name == fusion.DelegateTool,
					"argument_bytes":     len(item.Arguments), "json_valid": json.Valid([]byte(item.Arguments)),
					"part_key_mentions": strings.Count(item.Arguments, `"part"`),
					"object_starts":     strings.Count(item.Arguments, "{"), "object_ends": strings.Count(item.Arguments, "}"),
				})
			}
		}
		attempts = append(attempts, map[string]any{"role": a.Role, "purpose": a.Purpose, "status": a.Status,
			"settled": a.Settled, "applied": a.Applied, "usage_observed": a.Response != nil && a.Response.Usage != nil,
			"cost_usd": a.CostUSD, "failure": a.Failure, "tool_shapes": toolShapes})
	}
	evidence = map[string]any{"available": true, "state": session.State, "turn_requests": session.TurnRequests,
		"turn_tools": session.TurnTools, "turn_handoffs": session.TurnHandoffs, "turn_cost_usd": session.TurnCostUSD,
		"attempts": attempts, "maintenance": session.Maintenance}
}

func longRequest(t *testing.T, app *application, key string, sessionID *string, payload any) (*apicompat.ResponsesResponse, string) {
	t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	req, err := http.NewRequest("POST", app.base+"/v1/responses", bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if *sessionID != "" {
		req.Header.Set(fusion.HeaderSession, *sessionID)
	}
	response, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	require.NoError(t, err, "long-task send failed or uncertain; no retry permitted")
	defer response.Body.Close()
	// Persist the first error response's identity before an assertion exits.
	if *sessionID == "" {
		*sessionID = response.Header.Get(fusion.HeaderSession)
	}
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
	values                                         []int
	sums                                           [2]int
	final                                          string
	failSidekick                                   bool
}

func (w *longNativeWorkflow) reply(request *pb.GetChatMessageRequest, reply *pb.GetChatMessageResponse) {
	w.mu.Lock()
	defer w.mu.Unlock()
	reply.DeltaText, reply.DeltaToolCalls = w.final, nil
	reply.StopReason = pb.StopReason_STOP_REASON_STOP_PATTERN
	reply.Usage.BillingModelUid = request.ChatModelUid
	if len(request.Tools) == 0 {
		w.summaries++
		partial := 0
		if w.phase > 0 {
			for _, value := range w.values[(w.phase-1)*8 : (w.phase-1)*8+w.phaseTools] {
				partial += value
			}
		}
		reply.DeltaText = fmt.Sprintf(`{"goal":%q,"current_state":"phase %d; completed %d reads in this phase; running sum %d","decisions":["Read each fragment once"],"changes":[],"evidence":["Tool results are retained in the archived prefix"],"checks":[],"open_questions":[],"next_steps":["Continue the active phase"],"files":["synthetic fragments"]}`, longGoal, w.phase, w.phaseTools, partial)
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
		if w.failSidekick {
			reply.DeltaToolCalls[0].ArgumentsJson = `{"part":`
		}
		return
	}
	reply.DeltaText = fmt.Sprintf("Reviewed phase %d: all eight fragment values checked; sum=%d. %s.", w.phase, w.sums[w.phase-1], longGoal)
}
