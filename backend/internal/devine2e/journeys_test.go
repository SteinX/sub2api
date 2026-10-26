//go:build devine2e

package devine2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestDevinClientUserJourneysMock(t *testing.T) {
	if os.Getenv("DEVIN_JOURNEY_SUITE") != "true" {
		t.Skip("use make test-devin-user-journeys-mock")
	}
	require.Equal(t, "mock", os.Getenv("DEVIN_E2E_MODE"), "this runner has no real-upstream authorization")
	f := newNativeFixture(t)
	f.agentMode.Store(true)
	f.journey = &projectJourney{}
	f.largeCatalog.Store(true)
	runDevinCloudGatewaySuite(t, "mock", f, "journeys")
}

func runUserJourneys(t *testing.T, app *application, fixture *nativeFixture, admin, key, userPassword string, keyID, accountID int64) {
	t.Helper()
	t.Run("private_cache_requires_authentication", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		unauth := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT")})
		defer unauth.Close()
		err := unauth.Ping(ctx).Err()
		require.Error(t, err)
		require.Contains(t, err.Error(), "NOAUTH")
		auth := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT"), Password: os.Getenv("DEVIN_E2E_REDIS_PASSWORD")})
		defer auth.Close()
		require.NoError(t, auth.Ping(ctx).Err())
	})
	t.Run("public_physical_price_preflight_has_no_inference_or_usage", func(t *testing.T) {
		fixture.mu.Lock()
		before := len(fixture.chats)
		fixture.mu.Unlock()
		proofs := journeyPricingProof(t, app, key, []string{fixtureModel, fixtureSidekick, "unknown-private-model"})
		for _, entry := range proofs[:2] {
			require.Equal(t, true, entry["identified"])
			require.Equal(t, true, entry["calculable"])
			require.Equal(t, "group", entry["source"])
		}
		require.Equal(t, false, proofs[2]["identified"])
		require.Equal(t, "not_advertised", proofs[2]["source"])
		quote := request(t, app, "GET", "/v1/models?pricing_models="+url.QueryEscape(fixtureModel)+"&context_tokens=32768&output_tokens=1024", key, nil, 200)
		var quoted struct {
			Data []struct {
				Cost float64 `json:"context_cost_upper_usd"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(quote, &quoted))
		require.Len(t, quoted.Data, 1)
		require.GreaterOrEqual(t, quoted.Data[0].Cost, 32768*(.000001+.000001+.0000001)+1024*.000002, "a cache TTL upper bound may be larger but must never underquote the explicit flat card")
		request(t, app, "GET", "/v1/models?pricing_models="+url.QueryEscape(fixtureModel)+"&context_tokens=0&output_tokens=1024", key, nil, 400)
		fixture.mu.Lock()
		require.Equal(t, before, len(fixture.chats))
		fixture.mu.Unlock()
		require.Zero(t, agentUsageCount(t, app, keyID))
		cost, err := (&authorizedJourneyBudget{db: agentDatabase(t, app), keyID: keyID}).cost()
		require.NoError(t, err)
		require.Zero(t, cost)
	})
	profile := fusion.DefaultProfile("fusion-project-journey", fixtureModel, fixtureSidekick)
	if !t.Run("slow_catalog_timeout_and_manual_refresh", func(t *testing.T) {
		verifyJourneyCatalogRecovery(t, app, fixture, key, userPassword)
	}) {
		t.Fatal("product catalog failure recovery did not complete")
	}
	profile.MaxHandoffs, profile.MaxRequests, profile.MaxTools = 2, 32, 20
	profile.MaxCostUSD, profile.MaxWallSeconds = 1, 300
	profile.Keepalive = &fusion.KeepalivePolicy{Enabled: false}
	current := api(t, app, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil)
	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(current.Get("extra").Raw), &extra))
	extra["devin_fusion_profiles"] = []fusion.Profile{profile}
	api(t, app, "PUT", fmt.Sprintf("/admin/accounts/%d", accountID), admin, map[string]any{"extra": extra})
	verifyProjectToolAdmission(t, app, fixture, key, userPassword, profile.PublicModel, keyID)
	if os.Getenv("DEVIN_JOURNEY_TOOL_LIMITS_ONLY") == "true" {
		return
	}
	verifyProjectTestReceipts(t, app, fixture, key, userPassword, profile.PublicModel, keyID)
	verifyProjectFirstErrorClients(t, app, fixture, key, userPassword, keyID)
	if os.Getenv("DEVIN_JOURNEY_EVIDENCE_ONLY") == "true" {
		return // Only this explicitly synthetic runner has a focused evidence mode.
	}
	verifyProjectMutationNegatives(t, app, fixture, key, userPassword, keyID)
	preMatrixBilling := snapshotMockBilling(t, app, fixture, keyID)
	require.Equal(t, true, preMatrixBilling["available"], "retain unresolved fixture settlement, but a missing audit query cannot pass")
	// A fresh synthetic key isolates the healthy matrix from deliberately
	// interrupted fixture writers. Only mock mode may do this; approved live
	// batches never rotate their key or reset the durable budget.
	require.Equal(t, "mock", os.Getenv("DEVIN_E2E_MODE"))
	user := login(t, app, "user@devin-e2e.invalid", userPassword)
	prior := api(t, app, "GET", fmt.Sprintf("/keys/%d", keyID), user, nil)
	remaining := prior.Get("quota").Float() - prior.Get("quota_used").Float()
	require.Positive(t, remaining, "mock key rotation cannot add quota")
	api(t, app, "PUT", fmt.Sprintf("/keys/%d", keyID), user, map[string]any{"name": "interrupted-fixtures", "status": "inactive"})
	positiveKey := api(t, app, "POST", "/keys", user, map[string]any{"name": "e2e-key", "group_id": prior.Get("group_id").Int(), "quota": remaining})
	key, keyID = positiveKey.Get("key").String(), positiveKey.Get("id").Int()
	require.NotEmpty(t, key)
	app.redactor.addSecret(key)
	fixture.mu.Lock()
	positiveNativeStart := len(fixture.chats)
	fixture.mu.Unlock()
	fence := newJourneyFence(app, nil)
	reports := []map[string]any{}
	billing := map[string]any{"verified": false, "verification_scope": "supported-case batch on a separate synthetic key", "tier": "synthetic billing; not a provider invoice", "pre_matrix_fixture_batches": preMatrixBilling, "all_batches_fully_settled": false}
	defer func() {
		writeAgentArtifact(t, "client-journeys.json", map[string]any{
			"schema_version": 1, "tier": "client_mock", "real_upstream": false, "eligible_for_real_e2e": false,
			"source": journeySource(t, app), "cases": reports,
			"billing":    billing,
			"unverified": []string{"real upstream", "deployed existing group/channel rules", "OMO Senpi identity", "unexpected disconnect", "client compact", "client fork", "client workers"},
		})
	}()
	for _, client := range []string{"opencode", "omo", "omp"} {
		for _, kind := range []string{"direct", "fusion"} {
			profiles := []string{"project"}
			if client == "omp" {
				profiles = []string{"default", "compatibility"}
			}
			for _, configProfile := range profiles {
				id := client + "/" + kind + "/" + configProfile
				if filter := os.Getenv("DEVIN_JOURNEY_CASE"); filter != "" && filter != id {
					continue
				}
				t.Run(id, func(t *testing.T) {
					model := fixtureModel
					if kind == "fusion" {
						model = profile.PublicModel
					}
					directory, err := os.MkdirTemp("", "devin-project-journey-")
					require.NoError(t, err)
					t.Cleanup(func() { _ = os.RemoveAll(directory) })
					fixture.journey.mu.Lock()
					fixture.journey.workspace = filepath.Join(directory, "workspace")
					fixture.journey.mutationTool = "write"
					if client == "opencode" && kind == "direct" {
						fixture.journey.mutationTool = "edit"
					}
					fixture.journey.maxContextBytes = 0
					fixture.journey.mu.Unlock()
					admission := newJourneyAdmission(t, app, model, 40)
					fixture.mu.Lock()
					nativeStart := len(fixture.chats)
					fixture.mu.Unlock()
					usageBefore := agentUsageCount(t, app, keyID)
					ui := journeyUIServer(t, app.base, admission.server.URL)
					script, err := filepath.Abs("../../../tools/devin-e2e-clients/journey.py")
					require.NoError(t, err)
					ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
					defer cancel()
					cmd := exec.CommandContext(ctx, "python3", script, "--directory", directory, "--client", client, "--model", model, "--ui-url", ui.URL, "--profile", configProfile)
					gracefulJourneyCommand(cmd)
					processRegistry, containerRegistry := journeyRegistryEnvironment(t, app)
					for _, env := range os.Environ() {
						name, _, _ := strings.Cut(env, "=")
						if !strings.Contains(" DEVIN_E2E_PAT DEVIN_E2E_CREDENTIAL DEVIN_PAT DEVIN_API_KEY SUB2API_API_KEY SUB2API_USER_TOKEN ", " "+name+" ") {
							cmd.Env = append(cmd.Env, env)
						}
					}
					cmd.Env = append(cmd.Env, "DEVIN_JOURNEY_GATEWAY_KEY="+key)
					cmd.Env = append(cmd.Env, "DEVIN_JOURNEY_UI_PASSWORD="+userPassword)
					cmd.Env = append(cmd.Env, "DEVIN_JOURNEY_PROCESS_REGISTRY="+processRegistry, "DEVIN_JOURNEY_CONTAINER_REGISTRY="+containerRegistry)
					output, runErr := runGuardedJourney(t, cmd, app, admission, fence)
					cleanupErr := cleanupJourneyProcessesAt(processRegistry, containerRegistry)
					report := map[string]any{"status": "failed", "stage": "runner_output"}
					parseErr := json.Unmarshal(output, &report)
					if report == nil {
						report = map[string]any{"status": "failed", "stage": "runner_output"}
					}
					report["batch_guard"] = fence.snapshot()
					forwarded, requests := admission.snapshot()
					if kind == "fusion" && forwarded > 0 {
						report["fusion_execution"] = snapshotFusionAudit(agentDatabase(t, app), keyID, requests, false)
					}
					errors := map[string]int{}
					for _, request := range requests {
						if request.ErrorCode != "" {
							errors[request.ErrorCode]++
						}
					}
					report["id"], report["model_kind"], report["physical_lead_uid"] = id, kind, fixtureModel
					report["public_model_uid"] = model
					report["physical_sidekick_uid"] = ""
					if kind == "fusion" {
						report["physical_sidekick_uid"] = fixtureSidekick
					}
					fixture.mu.Lock()
					uids, roles := map[string]int{}, map[string]int{}
					contexts := map[string]map[string]bool{"lead": {}, "sidekick": {}}
					for _, native := range fixture.chats[nativeStart:] {
						role := "lead"
						if kind == "fusion" && native.ChatModelUid == fixtureSidekick {
							role = "sidekick"
						}
						uids[native.ChatModelUid]++
						roles[role]++
						contexts[role][fusion.Digest(native.CascadeId)] = true
					}
					nativeCount := len(fixture.chats) - nativeStart
					fixture.mu.Unlock()
					report["observed_physical_models"], report["observed_role_calls"] = uids, roles
					report["native_context_hashes"] = contexts
					fixture.journey.mu.Lock()
					report["native_context_max_bytes"] = fixture.journey.maxContextBytes
					fixture.journey.mu.Unlock()
					report["reasoning_scope"] = map[string]string{"model_uid_effort": "fixture UID swe-2-max", "client_thinking": map[bool]string{true: "off", false: "client default"}[client == "omp"]}
					report["public_requests"], report["http_attempts"], report["error_codes"] = forwarded, len(requests), errors
					capabilitiesRequired := os.Getenv("DEVIN_JOURNEY_CAPABILITIES") == "true" && client == "omp" && kind == "direct" && configProfile == "compatibility"
					report["capabilities_required"] = capabilitiesRequired
					independentCapabilities := os.Getenv("DEVIN_JOURNEY_CAPABILITIES") == "independent" && client == "omp" && kind == "direct" && configProfile == "compatibility"
					if independentCapabilities {
						report["capabilities_scope"] = "fork_pressure_idle_only"
					}
					if report["status"] == "unsupported_by_product" {
						report["expectation"] = "product refuses known incompatible configuration; task not run"
					}
					reports = append(reports, report)
					writeAgentArtifact(t, "journey-"+strings.ReplaceAll(id, "/", "-")+".json", report)
					require.NoError(t, parseErr, "journey runner must return sanitized structural evidence")
					require.NoError(t, cleanupErr, "mock client cleanup not verified")
					t.Logf("journey=%s status=%v forwarded=%d errors=%v", id, report["status"], forwarded, errors)
					if client == "omp" && kind == "fusion" && configProfile == "default" && report["status"] == "unsupported_by_product" {
						require.NoError(t, runErr)
						require.Zero(t, forwarded)
						require.Zero(t, nativeCount)
						return
					}
					if client == "omp" && kind == "fusion" && configProfile == "default" && errors["devin_fusion_history_conflict"] > 0 {
						report["expectation"] = "known default-profile failure observed; not a pass"
						return
					}
					require.NoError(t, runErr, "real CLI project task failed; structural evidence retained")
					require.Equal(t, "passed", report["status"])
					for _, phase := range []string{"task", "resume"} {
						require.Equal(t, false, report[phase].(map[string]any)["final_text_matches"], "normal project completion uses natural language, not a fixed marker")
					}
					if capabilitiesRequired || independentCapabilities {
						capabilities, ok := report["capability_evidence"].(map[string]any)
						require.True(t, ok)
						for _, capability := range []string{"fork", "context_pressure", "idle_process_recovery"} {
							require.Equal(t, "passed", capabilities[capability], "actual OMP capability failed; evidence retained")
						}
						require.Equal(t, true, capabilities["cleanup_passed"])
						if capabilitiesRequired {
							require.Equal(t, "passed", capabilities["status"])
							require.Equal(t, "passed", capabilities["compact"])
						} else {
							require.Equal(t, "partial", capabilities["status"])
							require.Equal(t, "not_run", capabilities["compact"])
						}
						require.GreaterOrEqual(t, report["native_context_max_bytes"], 131072, "pressure must actually reach forwarded model history, not just files on disk")
					}
					require.Positive(t, uids[fixtureModel])
					if kind == "fusion" {
						require.Positive(t, uids[fixtureSidekick])
						for context := range contexts["lead"] {
							require.False(t, contexts["sidekick"][context], "roles must keep separate native histories")
						}
					}
					verifyAgentUsage(t, app, keyID, usageBefore+nativeCount)
					if kind == "fusion" {
						require.Equal(t, true, report["fusion_execution"].(map[string]any)["complete"], "retain partial collaboration evidence even when client proof failed")
					}
				})
			}
		}
	}
	require.NotEmpty(t, reports, "case filter did not select any journey")
	fixture.mu.Lock()
	nativeTotal := len(fixture.chats) - positiveNativeStart
	fixture.mu.Unlock()
	db := agentDatabase(t, app)
	require.Eventually(t, func() bool {
		var rows, unique, charged int
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := db.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT request_id),count(*) FILTER (WHERE actual_cost>0)
			FROM usage_logs WHERE api_key_id=$1`, keyID).Scan(&rows, &unique, &charged)
		return err == nil && rows == nativeTotal && unique == rows && charged == rows
	}, 15*time.Second, 200*time.Millisecond, "every supported-case native inference needs exactly one charged row on its isolated synthetic key")
	billing["verified"], billing["native_calls"], billing["unique_charged_rows"] = true, nativeTotal, nativeTotal
	billing["all_batches_fully_settled"] = preMatrixBilling["settlement_complete"] == true
}

func journeyUIServer(t *testing.T, backend, endpoint string) *httptest.Server {
	t.Helper()
	directory := os.Getenv("DEVIN_JOURNEY_UI_DIST")
	require.NotEmpty(t, directory, "build the complete product application first")
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	apiTarget, err := url.Parse(backend)
	require.NoError(t, err)
	apiProxy := httputil.NewSingleHostReverseProxy(apiTarget)
	files := http.FileServer(http.Dir(directory))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			proxy.ServeHTTP(w, r)
		} else if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/setup/") || r.URL.Path == "/health" {
			apiProxy.ServeHTTP(w, r)
		} else if r.URL.Path == "/login" || r.URL.Path == "/keys" || r.URL.Path == "/dashboard" || r.URL.Path == "/" {
			http.ServeFile(w, r, filepath.Join(directory, "index.html"))
		} else {
			files.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func journeySource(t *testing.T, app *application) map[string]any {
	t.Helper()
	git := func(args ...string) string {
		out, err := exec.Command("git", args...).Output()
		require.NoError(t, err)
		return strings.TrimSpace(string(out))
	}
	data, err := os.ReadFile(app.binary)
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	ui := sha256.New()
	root := os.Getenv("DEVIN_JOURNEY_UI_DIST")
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = ui.Write([]byte(filepath.ToSlash(relative) + "\x00"))
		_, _ = ui.Write(data)
		return nil
	}))
	lock, err := os.ReadFile("../../../tools/devin-e2e-clients/package-lock.json")
	require.NoError(t, err)
	lockDigest := sha256.Sum256(lock)
	return map[string]any{"commit": git("rev-parse", "HEAD"), "tree": git("rev-parse", "HEAD^{tree}"), "dirty": git("status", "--porcelain") != "", "backend_sha256": hex.EncodeToString(digest[:]),
		"ui_bundle_sha256": hex.EncodeToString(ui.Sum(nil)), "client_lock_sha256": hex.EncodeToString(lockDigest[:])}
}
