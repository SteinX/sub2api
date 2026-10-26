//go:build devine2e

package devine2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func verifyJourneyCatalogRecovery(t *testing.T, app *application, fixture *nativeFixture, key, password string) {
	t.Helper()
	fixture.mu.Lock()
	before := len(fixture.chats)
	fixture.mu.Unlock()
	target, err := url.Parse(app.base)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var catalogs atomic.Int64
	var canceled atomic.Bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.URL.Query().Get("client_version") == "" {
			http.Error(w, "unexpected probe route", http.StatusBadRequest)
			return
		}
		if catalogs.Add(1) == 1 {
			timer := time.NewTimer(35 * time.Second)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				canceled.Store(true)
				return
			case <-timer.C:
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(endpoint.Close)
	ui := journeyUIServer(t, app.base, endpoint.URL)
	directory, err := os.MkdirTemp("", "devin-catalog-recovery-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	script, err := filepath.Abs("../../../tools/devin-e2e-clients/journey.py")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", script, "--directory", directory, "--client", "opencode", "--model", fixtureModel, "--ui-url", ui.URL, "--profile", "project", "--stage", "handoff", "--upstream", "synthetic", "--catalog-recovery-probe")
	gracefulJourneyCommand(command)
	processRegistry, containerRegistry := journeyRegistryEnvironment(t, app)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if !strings.Contains(" DEVIN_E2E_PAT DEVIN_E2E_CREDENTIAL DEVIN_PAT DEVIN_API_KEY SUB2API_API_KEY SUB2API_USER_TOKEN DEVIN_JOURNEY_PLAN_FILE DEVIN_JOURNEY_APPROVAL_FILE DEVIN_JOURNEY_RUN_LEDGER ", " "+name+" ") {
			command.Env = append(command.Env, variable)
		}
	}
	command.Env = append(command.Env, "DEVIN_JOURNEY_GATEWAY_KEY="+key, "DEVIN_JOURNEY_UI_PASSWORD="+password,
		"DEVIN_JOURNEY_PROCESS_REGISTRY="+processRegistry, "DEVIN_JOURNEY_CONTAINER_REGISTRY="+containerRegistry)
	output, runErr := command.Output()
	cleanupErr := cleanupJourneyProcessesAt(processRegistry, containerRegistry)
	var report map[string]any
	parseErr := json.Unmarshal(output, &report)
	defer func() {
		writeAgentArtifact(t, "journey-catalog-recovery-ui.json", map[string]any{"ui": report, "catalog_requests": catalogs.Load(), "canceled_first_request": canceled.Load(), "cleanup_verified": cleanupErr == nil, "synthetic_upstream": true})
	}()
	require.NoError(t, parseErr)
	require.NoError(t, cleanupErr)
	require.NoError(t, runErr, "the actual product must expose loading, a bounded error and a usable explicit refresh")
	require.Equal(t, "handoff_only", report["status"])
	handoff, ok := report["handoff"].(map[string]any)
	require.True(t, ok)
	recovery, ok := handoff["catalog_recovery"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(238), recovery["models_after_retry"], "the 239 raw entries include one deliberately unverified harness, which must stay hidden")
	require.Equal(t, false, recovery["blocked_fixture_model_visible"])
	require.EqualValues(t, 2, catalogs.Load(), "one stalled request and one user-initiated retry; no automatic retry")
	require.Eventually(t, canceled.Load, time.Second, 10*time.Millisecond, "the timed-out browser fetch must release the pending server request")
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	require.Len(t, fixture.chats, before, "configuration recovery must not generate model content")
}
