//go:build devine2e

package devine2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

func TestDevinAgentClients(t *testing.T) {
	if os.Getenv("DEVIN_E2E_AGENT_SUITE") != "true" {
		t.Skip("use make test-devin-agent-e2e for pinned real-client processes")
	}
	mode := os.Getenv("DEVIN_E2E_MODE")
	var fixture *nativeFixture
	if mode == "mock" {
		fixture = newNativeFixture(t)
		fixture.agentMode.Store(true)
	}
	runDevinCloudGatewaySuite(t, mode, fixture, true)
}

type agentRequest struct {
	Session, ClientSession, ErrorCode string
	items                             []apicompat.ResponsesInputItem
	HasReadResult                     bool
	Status                            int
	Stream, Forwarded                 bool
}

// This is an observable loopback admission boundary, not a credential proxy.
// It preserves the client's protocol and session headers, limits model calls,
// and clamps requested output before the production gateway receives a request.
type agentAdmission struct {
	server        *httptest.Server
	mu            sync.Mutex
	model         string
	forwarded     int
	maxForwarded  int
	requests      []*agentRequest
	interruptFile string
	active        chan struct{}
}

type agentRequestContext struct{}

func newAgentAdmission(t *testing.T, app *application, model string, maxForwarded int) *agentAdmission {
	t.Helper()
	p := &agentAdmission{model: model, maxForwarded: maxForwarded, active: make(chan struct{}, 1)}
	target, err := url.Parse(app.base)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(response *http.Response) error {
		record, _ := response.Request.Context().Value(agentRequestContext{}).(*agentRequest)
		if record != nil {
			p.mu.Lock()
			record.Status = response.StatusCode
			record.Session = response.Header.Get(fusion.HeaderSession)
			if response.StatusCode >= 400 {
				body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
				if err != nil {
					p.mu.Unlock()
					return err
				}
				_ = response.Body.Close()
				response.Body = io.NopCloser(bytes.NewReader(body))
				var envelope struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				_ = json.Unmarshal(body, &envelope)
				// Known code only; never retain provider messages or bodies.
				if strings.HasPrefix(envelope.Error.Code, "devin_fusion_") {
					record.ErrorCode = envelope.Error.Code
				}
			}
			p.mu.Unlock()
		}
		return nil
	}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/models" {
			proxy.ServeHTTP(w, r)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unsupported E2E route", 404)
			return
		}
		var body map[string]any
		if json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body) != nil || body["model"] != p.model {
			http.Error(w, "unexpected E2E model or body", 400)
			return
		}
		p.mu.Lock()
		record := &agentRequest{Stream: body["stream"] == true}
		wire, _ := json.Marshal(body)
		var chat apicompat.ChatCompletionsRequest
		if json.Unmarshal(wire, &chat) == nil {
			if converted, err := apicompat.ChatCompletionsToResponsesWithMinOutputTokens(&chat, 1); err == nil {
				record.items, _ = fusion.ParseInput(converted)
			}
		}
		if messages, ok := body["messages"].([]any); ok {
			for _, value := range messages {
				message, _ := value.(map[string]any)
				content, _ := json.Marshal(message["content"])
				if message["role"] == "tool" && bytes.Contains(content, []byte("E2E_TOOL_RESULT")) {
					record.HasReadResult = true
				}
			}
		}
		for _, header := range []string{"X-Session-Id", "X-OpenCode-Session", "X-Litellm-Session-Id"} {
			if value := r.Header.Get(header); value != "" {
				record.ClientSession = fusion.Digest(value)
				break
			}
		}
		p.requests = append(p.requests, record)
		if len(p.requests) > 12 || p.forwarded >= p.maxForwarded {
			p.mu.Unlock()
			http.Error(w, "E2E request budget exhausted", 402)
			return
		}
		interrupt := p.interruptFile
		p.interruptFile = ""
		p.mu.Unlock()
		select {
		case p.active <- struct{}{}:
			defer func() { <-p.active }()
		default:
			http.Error(w, "E2E permits one active request", 409)
			return
		}
		if interrupt != "" {
			_ = os.WriteFile(interrupt, []byte("not-forwarded\n"), 0600)
			<-r.Context().Done()
			return
		}
		// SDK retries also pass this gate. No runner retry after an uncertain send.
		p.mu.Lock()
		p.forwarded++
		record.Forwarded = true
		p.mu.Unlock()
		if requested, ok := body["max_tokens"].(float64); !ok || requested <= 0 || requested > 1024 {
			body["max_tokens"] = 1024
		}
		delete(body, "max_completion_tokens")
		encoded, _ := json.Marshal(body)
		r.Body = io.NopCloser(bytes.NewReader(encoded))
		r.ContentLength = int64(len(encoded))
		r = r.WithContext(context.WithValue(r.Context(), agentRequestContext{}, record))
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *agentAdmission) snapshot() (int, []agentRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	requests := make([]agentRequest, 0, len(p.requests))
	for _, request := range p.requests {
		requests = append(requests, *request)
	}
	return p.forwarded, requests
}

type clientEvidence struct {
	Client       string `json:"client"`
	Action       string `json:"action"`
	Success      bool   `json:"success"`
	ExpectedText bool   `json:"saw_expected_text"`
	ToolCount    int    `json:"tool_completed"`
	Interrupted  bool   `json:"interrupted_before_forward"`
	ExitCode     int    `json:"exit_code"`
	Frames       int    `json:"frames"`
}

func runPinnedClient(t *testing.T, client, mode, directory, model, key, endpoint, action, interrupt string) clientEvidence {
	t.Helper()
	script, err := filepath.Abs("../../../tools/devin-e2e-clients/run.py")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	args := []string{script, "--directory", directory, "--client", client, "--model", model, "--endpoint", endpoint + "/v1", "--mode", mode, "--action", action}
	if interrupt != "" {
		args = append(args, "--interrupt-file", interrupt)
	}
	command := exec.CommandContext(ctx, "python3", args...)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if name == "DEVIN_E2E_PAT" || name == "DEVIN_E2E_CREDENTIAL" || name == "DEVIN_PAT" || name == "DEVIN_API_KEY" || name == "SUB2API_API_KEY" || name == "SUB2API_USER_TOKEN" {
			continue
		}
		command.Env = append(command.Env, variable)
	}
	command.Env = append(command.Env, "DEVIN_E2E_GATEWAY_KEY="+key)
	output, runErr := command.Output()
	var evidence clientEvidence
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(output), &evidence), "client runner did not return structural evidence")
	t.Logf("client=%s action=%s exit=%d frames=%d completed_read=%d expected_text=%v interrupted_before_forward=%v", client, action, evidence.ExitCode, evidence.Frames, evidence.ToolCount, evidence.ExpectedText, evidence.Interrupted)
	require.NoError(t, runErr, "client process failed; inspect structural admission records")
	require.True(t, evidence.Success, "client completion evidence missing")
	return evidence
}

func eligibleAgentModels(models []*pb.ClientModelConfig) []*pb.ClientModelConfig {
	var capable []*pb.ClientModelConfig
	for _, model := range models {
		if model.GetModelInfo().GetModelFeatures().GetSupportsToolCalls() && model.GetMaxTokens() >= 32768 && model.GetModelInfo().GetMaxOutputTokens() >= 1024 && !model.GetDisabled() && !model.GetIsCapacityLimited() && devin.PlanModel(model).Mode == devin.RouteDirect {
			capable = append(capable, model)
		}
	}
	return capable
}

func selectAgentModels(models []*pb.ClientModelConfig) (string, string, error) {
	capable := eligibleAgentModels(models)
	first, err := selectSmokeModel(capable, "", 1024)
	if err != nil {
		return "", "", err
	}
	var remaining []*pb.ClientModelConfig
	for _, model := range capable {
		if devin.ModelID(model) != first.Model {
			remaining = append(remaining, model)
		}
	}
	second, err := selectSmokeModel(remaining, "", 1024)
	if err != nil {
		return first.Model, first.Model, nil
	}
	return first.Model, second.Model, nil
}

func runAgentCases(t *testing.T, app *application, fixture *nativeFixture, mode, admin, user, key string, keyID, accountID int64, models []*pb.ClientModelConfig) {
	t.Helper()
	lead, sidekick, err := selectAgentModels(models)
	require.NoError(t, err)
	batch := os.Getenv("DEVIN_E2E_AGENT_BATCH")
	if batch == "" {
		batch = "canary"
	}
	require.Contains(t, []string{"canary", "matrix"}, batch)
	if mode == "mock" {
		batch = "matrix"
	}
	quota := .25
	if batch == "matrix" {
		quota = 1
	}
	pricesReady := fixture != nil
	var priceEvidence []map[string]any
	if fixture == nil {
		var priced []*pb.ClientModelConfig
		seen := map[string]bool{}
		for _, candidate := range eligibleAgentModels(models) {
			model := devin.ModelID(candidate)
			if seen[model] {
				continue
			}
			seen[model] = true
			pricing := api(t, app, "GET", "/admin/channels/model-pricing?model="+url.QueryEscape(model), admin, nil)
			found := pricing.Get("found").Bool() && pricing.Get("output_price").Float() > 0
			priceEvidence = append(priceEvidence, map[string]any{"model": model, "found": found, "input_price": pricing.Get("input_price").Float(), "output_price": pricing.Get("output_price").Float()})
			if found {
				priced = append(priced, candidate)
			}
		}
		pricesReady = len(priced) > 0
		if pricesReady {
			lead, sidekick, err = selectAgentModels(priced)
			require.NoError(t, err)
		}
	}
	plan := map[string]any{
		"mode": mode, "batch": batch, "direct_model": lead, "fusion_lead": lead, "fusion_sidekick": sidekick, "fusion_members_same_model": lead == sidekick,
		"client_versions": map[string]string{"opencode": "1.19.4", "omo": "5.1.44", "omp": "18.12.6", "bun": "1.4.4"},
		"prices_ready":    pricesReady, "prices": priceEvidence, "provider_spending_hard_cap": false, "gateway_key_quota_usd": quota,
		"concurrency": 1, "max_output_tokens": 1024, "max_client_seconds": 75, "runner_retries_after_uncertain_send": 0,
		"fusion_limits":         map[string]any{"max_handoffs_per_turn": 1, "max_internal_requests_per_turn": 6, "max_external_tool_calls_per_turn": 2, "max_turn_wall_seconds": 90, "max_cost_usd_per_turn": .10, "max_output_tokens": 1024, "compaction": false, "cache_keepalive": false},
		"compatibility_profile": map[string]any{"omp_supersede_reads": false, "omp_drop_useless": false, "default_omp_pruning_result": "known incompatible after repeated reads; not passed"},
		"matrix":                map[string]any{"client_process_launches": 42, "minimum_forwarded_gateway_requests": 42, "max_forwarded_gateway_requests": 48, "max_http_attempts_including_rejections": 72, "fusion_max_internal_requests_per_turn": 6},
		"canary":                map[string]any{"client": "omp", "model_kind": "direct", "client_process_launches": 1, "expected_forwarded_gateway_requests": 2, "max_forwarded_gateway_requests": 3},
		"scope":                 "real client processes -> current production gateway -> synthetic native upstream in mock; real upstream only in authorized live",
	}
	writeAgentArtifact(t, "agent-plan.json", plan)
	writeAgentArtifact(t, "agent-known-default-incompatibilities.json", map[string]any{
		"omp_18_12_6_default_pruning": map[string]any{"result": "recorded_failure_not_a_default_client_pass", "error_code": "devin_fusion_history_conflict", "http_status": 409, "source": "tools/devin-e2e-clients/fixtures/omp-18.12.6-read-replay.json"},
		"tested_profile":              "owned read-only workspace; OMP automatic compaction, superseded reads and useless-result pruning disabled",
	})
	if mode == "plan" {
		t.Logf("agent plan ready; model prices ready=%v; no client inference requested", pricesReady)
		return
	}
	if mode == "live" {
		require.Equal(t, "true", os.Getenv("DEVIN_E2E_ALLOW_PAID"))
		require.True(t, pricesReady, "discovered members lack verified billing prices; stop before generation")
	}
	profile := fusion.DefaultProfile("fusion-e2e-agents", lead, sidekick)
	profile.MaxRequests, profile.MaxHandoffs, profile.MaxTools = 6, 1, 2
	profile.Compaction = &fusion.CompactionPolicy{Enabled: false}
	profile.Keepalive = &fusion.KeepalivePolicy{Enabled: false}
	profile.MaxOutputTokens, profile.MaxWallSeconds, profile.MaxCostUSD = 1024, 90, .10
	current := api(t, app, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil)
	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(current.Get("extra").Raw), &extra))
	extra["devin_fusion_enabled"], extra["devin_fusion_profiles"] = true, []fusion.Profile{profile}
	api(t, app, "PUT", fmt.Sprintf("/admin/accounts/%d", accountID), admin, map[string]any{"extra": extra})
	clients := []string{"opencode", "omo", "omp"}
	kinds := []string{"direct", "fusion"}
	if batch == "canary" {
		clients, kinds = []string{"omp"}, []string{"direct"}
	}
	for _, client := range clients {
		for _, kind := range kinds {
			if !t.Run(client+"/"+kind, func(t *testing.T) {
				model := lead
				if kind == "fusion" {
					model = profile.PublicModel
				}
				admission := newAgentAdmission(t, app, model, 8)
				if batch == "canary" {
					admission.maxForwarded = 3
				}
				defer func() {
					forwarded, records := admission.snapshot()
					var safe []map[string]any
					for _, record := range records {
						safe = append(safe, map[string]any{"status": record.Status, "stream": record.Stream, "forwarded": record.Forwarded, "error_code": record.ErrorCode,
							"client_session_hash": record.ClientSession, "fusion_session_hash": fusion.Digest(record.Session)})
					}
					unknown := t.Failed() && forwarded > 0
					for _, record := range records {
						unknown = unknown || record.Forwarded && record.Status == 0
					}
					writeAgentArtifact(t, "agent-"+client+"-"+kind+".json", map[string]any{"client": client, "model_kind": kind, "forwarded": forwarded, "requests": safe, "passed": !t.Failed(), "unknown_forwarded_result": unknown, "profile": "explicit compatibility settings; not default client settings"})
					if kind == "fusion" && t.Failed() {
						diagnoseAgentHistory(t, app, keyID, records)
					}
					if fixture != nil && t.Failed() {
						fixture.mu.Lock()
						defer fixture.mu.Unlock()
						var traces []map[string]any
						for _, chat := range fixture.chats {
							var prompts []map[string]any
							for _, prompt := range chat.ChatMessagePrompts {
								prompts = append(prompts, map[string]any{"source": prompt.Source.String(), "has_tool_id": prompt.ToolCallId != "", "result_marker": strings.Contains(prompt.Prompt, "E2E_TOOL_RESULT"), "tool_error": prompt.ToolResultIsError})
							}
							traces = append(traces, map[string]any{"model": chat.ChatModelUid, "prompts": prompts})
						}
						writeAgentArtifact(t, "agent-native-structure.json", traces)
					}
				}()
				root := t.TempDir()
				usageBefore := agentUsageCount(t, app, keyID)
				nativeStart := 0
				if fixture != nil {
					fixture.mu.Lock()
					nativeStart = len(fixture.chats)
					fixture.mu.Unlock()
				}
				observed := map[string][]agentRequest{}
				run := func(name, directory, action string, read bool, interrupt string) clientEvidence {
					_, before := admission.snapshot()
					if fixture != nil {
						fixture.mu.Lock()
						fixture.probePath = filepath.Join(directory, "workspace", "probe.txt")
						fixture.mu.Unlock()
						fixture.agentRead.Store(read)
					}
					var evidence clientEvidence
					require.True(t, t.Run(name, func(t *testing.T) {
						evidence = runPinnedClient(t, client, mode, directory, model, key, admission.server.URL, action, interrupt)
					}))
					_, after := admission.snapshot()
					observed[name] = after[len(before):]
					return evidence
				}
				if batch != "canary" {
					run("text_and_streaming", filepath.Join(root, "text"), "text", false, "")
				}
				tools := filepath.Join(root, "tools")
				evidence := run("client_read_and_result", tools, "tool", true, "")
				require.Positive(t, evidence.ToolCount, "real client did not report completing read")
				if batch != "canary" {
					run("continue", tools, "continue", false, "")
					app.stop(t)
					app.start(t)
					run("continue_after_backend_restart", tools, "continue", false, "")
					run("fresh_session", filepath.Join(root, "fresh"), "text", false, "")
					before, _ := admission.snapshot()
					marker := filepath.Join(root, "not-forwarded")
					admission.mu.Lock()
					admission.interruptFile = marker
					admission.mu.Unlock()
					interrupted := run("interrupt_before_upstream_send", filepath.Join(root, "interrupt"), "text", false, marker)
					require.True(t, interrupted.Interrupted)
					after, _ := admission.snapshot()
					require.Equal(t, before, after, "interrupted request must not be forwarded")
					run("one_retry_after_confirmed_unsent", filepath.Join(root, "interrupt"), "continue", false, "")
				}
				hasResult := false
				for _, record := range observed["client_read_and_result"] {
					hasResult = hasResult || record.HasReadResult
				}
				require.True(t, hasResult, "actual tool output never returned through the public gateway")
				if batch != "canary" {
					for _, continuation := range []string{"continue", "continue_after_backend_restart"} {
						keptResult := false
						for _, record := range observed[continuation] {
							keptResult = keptResult || record.HasReadResult
						}
						require.True(t, keptResult, "client continuation dropped prior tool history")
					}
					identity := func(name string, fusionID bool) string {
						var id string
						for _, record := range observed[name] {
							if !record.Forwarded || record.Status != 200 {
								continue
							}
							next := record.ClientSession
							if fusionID {
								next = record.Session
							}
							if id != "" {
								require.True(t, id == next, "tool roundtrip changed its session identity")
							}
							id = next
						}
						return id
					}
					for _, fusionID := range []bool{false, true} {
						if fusionID && kind != "fusion" {
							continue
						}
						id := identity("client_read_and_result", fusionID)
						if fusionID {
							require.NotEmpty(t, id)
						}
						if id != "" {
							require.True(t, id == identity("continue", fusionID), "continuation changed its session identity")
							require.True(t, id == identity("continue_after_backend_restart", fusionID), "restart lost session identity")
							require.True(t, id != identity("fresh_session", fusionID), "fresh client reused the prior session")
						}
					}
				}
				forwarded, records := admission.snapshot()
				require.LessOrEqual(t, forwarded, admission.maxForwarded)
				for _, record := range records {
					if record.Forwarded {
						require.Equal(t, 200, record.Status, "forwarded client request failed: %s", record.ErrorCode)
						require.True(t, record.Stream, "expected actual streaming client requests")
					}
				}
				verifyAgentUsage(t, app, keyID, usageBefore+forwarded)
				if kind == "fusion" {
					verifyAgentFusion(t, app, keyID, records, client)
					if fixture != nil {
						fixture.verifyAgentNativeContexts(t, nativeStart, client, lead, sidekick)
					}
				}
			}) {
				t.Fatal("stop the batch after a failed or uncertain client run")
			}
		}
	}
}

func writeAgentArtifact(t *testing.T, name string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	require.NoError(t, err)
	if dir := os.Getenv("DEVIN_E2E_ARTIFACTS"); dir != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), append(data, '\n'), 0600))
	}
}

func agentDatabase(t *testing.T, app *application) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", fmt.Sprintf("host=127.0.0.1 port=%s user=devin_e2e dbname=%s password=%s sslmode=disable", os.Getenv("DEVIN_E2E_PG_PORT"), app.database, os.Getenv("DEVIN_E2E_DB_PASSWORD")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func agentUsageCount(t *testing.T, app *application, keyID int64) int {
	t.Helper()
	db := agentDatabase(t, app)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM usage_logs WHERE api_key_id=$1 AND input_tokens>0 AND output_tokens>0 AND actual_cost>0", keyID).Scan(&count))
	return count
}

func verifyAgentUsage(t *testing.T, app *application, keyID int64, minimum int) {
	t.Helper()
	db := agentDatabase(t, app)
	require.Eventually(t, func() bool {
		var count int
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return db.QueryRowContext(ctx, "SELECT count(*) FROM usage_logs WHERE api_key_id=$1 AND input_tokens>0 AND output_tokens>0 AND actual_cost>0", keyID).Scan(&count) == nil && count >= minimum
	}, 15*time.Second, 200*time.Millisecond, "client usage not persisted for its public key")
}

func verifyAgentFusion(t *testing.T, app *application, keyID int64, records []agentRequest, client string) {
	t.Helper()
	db := agentDatabase(t, app)
	var userID int64
	require.NoError(t, db.QueryRow("SELECT user_id FROM api_keys WHERE id=$1", keyID).Scan(&userID))
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT")})
	t.Cleanup(func() { _ = rdb.Close() })
	seen := map[string]bool{}
	for _, record := range records {
		if !record.Forwarded || record.Status != 200 || seen[record.Session] {
			continue
		}
		require.NotEmpty(t, record.Session, "Fusion response lacks its persisted session identity")
		seen[record.Session] = true
		stored, err := rdb.Get(context.Background(), "devin:fusion:session:"+fusion.Scope(userID, keyID)+":"+fusion.Digest(record.Session)).Bytes()
		require.NoError(t, err)
		var session fusion.Session
		require.NoError(t, json.Unmarshal(stored, &session))
		require.Equal(t, fusion.Completed, session.State, "Fusion did not reach reviewed completion")
		require.NotEmpty(t, session.Lead)
		require.NotEmpty(t, session.Sidekick)
		require.NotEqual(t, session.LaneScope(fusion.Lead), session.LaneScope(fusion.Sidekick))
		require.NotEmpty(t, session.Handoffs, "Fusion-named output alone is insufficient")
		for _, handoff := range session.Handoffs {
			require.NotEmpty(t, handoff.Report, "Sidekick report missing")
		}
		roles := map[fusion.Role]int{}
		for _, attempt := range session.Attempts {
			roles[attempt.Role]++
			require.Equal(t, "completed", attempt.Status)
			require.True(t, attempt.Settled && attempt.Applied, "role usage not durably settled")
			require.Positive(t, attempt.CostUSD)
			id := "fusion:" + attempt.ID
			require.Eventually(t, func() bool {
				var count int
				var cost float64
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				return db.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(actual_cost),0) FROM usage_logs WHERE request_id=$1 AND api_key_id=$2 AND actual_cost>0", id, keyID).Scan(&count, &cost) == nil && count == 1 && math.Abs(cost-attempt.CostUSD) <= 1e-9
			}, 10*time.Second, 200*time.Millisecond, "role-level usage missing or duplicated")
		}
		require.GreaterOrEqual(t, roles[fusion.Lead], 2)
		require.Positive(t, roles[fusion.Sidekick])
		writeAgentArtifact(t, "fusion-"+client+"-"+fusion.Digest(record.Session)[:12]+".json", map[string]any{
			"state": session.State, "turns": session.Turn, "handoffs": len(session.Handoffs), "role_attempts": roles,
			"separate_contexts": true, "reports_present": true, "role_usage_settled": true, "public_key_usage_persisted": true,
		})
	}
	require.NotEmpty(t, seen)
}
