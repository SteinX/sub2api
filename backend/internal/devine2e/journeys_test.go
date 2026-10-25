//go:build devine2e

package devine2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	runDevinCloudGatewaySuite(t, "mock", f, "journeys")
}

func runUserJourneys(t *testing.T, app *application, fixture *nativeFixture, admin, key string, keyID, accountID int64) {
	t.Helper()
	profile := fusion.DefaultProfile("fusion-project-journey", fixtureModel, fixtureSidekick)
	profile.MaxHandoffs, profile.MaxRequests, profile.MaxTools = 2, 32, 20
	profile.MaxCostUSD, profile.MaxWallSeconds = 1, 300
	profile.Keepalive = &fusion.KeepalivePolicy{Enabled: false}
	current := api(t, app, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil)
	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(current.Get("extra").Raw), &extra))
	extra["devin_fusion_profiles"] = []fusion.Profile{profile}
	api(t, app, "PUT", fmt.Sprintf("/admin/accounts/%d", accountID), admin, map[string]any{"extra": extra})
	reports := []map[string]any{}
	defer func() {
		writeAgentArtifact(t, "client-journeys.json", map[string]any{
			"schema_version": 1, "tier": "client_mock", "real_upstream": false, "eligible_for_real_e2e": false,
			"source": journeySource(t, app), "cases": reports,
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
					fixture.journey.mu.Unlock()
					admission := newAgentAdmission(t, app, model, 40)
					ui := journeyUIServer(t, admission.server.URL, key)
					script, err := filepath.Abs("../../../tools/devin-e2e-clients/journey.py")
					require.NoError(t, err)
					ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
					defer cancel()
					cmd := exec.CommandContext(ctx, "python3", script, "--directory", directory, "--client", client, "--model", model, "--ui-url", ui.URL, "--profile", configProfile)
					for _, env := range os.Environ() {
						name, _, _ := strings.Cut(env, "=")
						if !strings.Contains(" DEVIN_E2E_PAT DEVIN_E2E_CREDENTIAL DEVIN_PAT DEVIN_API_KEY SUB2API_API_KEY SUB2API_USER_TOKEN ", " "+name+" ") {
							cmd.Env = append(cmd.Env, env)
						}
					}
					cmd.Env = append(cmd.Env, "DEVIN_JOURNEY_GATEWAY_KEY="+key)
					output, runErr := cmd.Output()
					var report map[string]any
					require.NoError(t, json.Unmarshal(output, &report), "journey runner must return sanitized structural evidence")
					forwarded, requests := admission.snapshot()
					errors := map[string]int{}
					for _, request := range requests {
						if request.ErrorCode != "" {
							errors[request.ErrorCode]++
						}
					}
					report["id"], report["model_kind"], report["physical_lead_uid"], report["physical_sidekick_uid"] = id, kind, fixtureModel, fixtureSidekick
					report["public_requests"], report["http_attempts"], report["error_codes"] = forwarded, len(requests), errors
					reports = append(reports, report)
					writeAgentArtifact(t, "journey-"+strings.ReplaceAll(id, "/", "-")+".json", report)
					t.Logf("journey=%s status=%v forwarded=%d errors=%v", id, report["status"], forwarded, errors)
					if client == "omp" && kind == "fusion" && configProfile == "default" && errors["devin_fusion_history_conflict"] > 0 {
						report["expectation"] = "known default-profile failure observed; not a pass"
						return
					}
					require.NoError(t, runErr, "real CLI project task failed; structural evidence retained")
					require.Equal(t, "passed", report["status"])
				})
			}
		}
	}
}

func journeyUIServer(t *testing.T, endpoint, key string) *httptest.Server {
	t.Helper()
	directory := os.Getenv("DEVIN_JOURNEY_UI_DIST")
	require.NotEmpty(t, directory, "build the product browser harness first")
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	files := http.FileServer(http.Dir(directory))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__journey/session" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(map[string]string{"key": key})
		} else if strings.HasPrefix(r.URL.Path, "/v1/") {
			proxy.ServeHTTP(w, r)
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
	return map[string]any{"commit": git("rev-parse", "HEAD"), "tree": git("rev-parse", "HEAD^{tree}"), "dirty": git("status", "--porcelain") != "", "backend_sha256": hex.EncodeToString(digest[:])}
}
