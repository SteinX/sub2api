//go:build devine2e

package devine2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func verifyProjectFirstErrorClients(t *testing.T, app *application, fixture *nativeFixture, key, password string) {
	t.Helper()
	for _, client := range []string{"opencode", "omo", "omp"} {
		t.Run("first_tool_error_"+client, func(t *testing.T) {
			directory := t.TempDir()
			fixture.journey.mu.Lock()
			fixture.journey.workspace = filepath.Join(directory, "workspace")
			fixture.journey.testScenario = "read_error_retry"
			fixture.journey.mu.Unlock()
			fence := newJourneyFence(app, nil)
			t.Cleanup(func() {
				fixture.journey.mu.Lock()
				fixture.journey.testScenario = ""
				fixture.journey.mu.Unlock()
				app.stop(t)
				app.start(t)
			})
			admission := newJourneyAdmission(t, app, fixtureModel, 12)
			ui := journeyUIServer(t, app.base, admission.server.URL)
			script, err := filepath.Abs("../../../tools/devin-e2e-clients/journey.py")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "python3", script, "--directory", directory, "--client", client, "--model", fixtureModel, "--ui-url", ui.URL, "--profile", "compatibility")
			gracefulJourneyCommand(cmd)
			processRegistry, containerRegistry := journeyRegistryEnvironment(t, app)
			for _, variable := range os.Environ() {
				name, _, _ := strings.Cut(variable, "=")
				if !strings.Contains(" DEVIN_E2E_PAT DEVIN_E2E_CREDENTIAL DEVIN_PAT DEVIN_API_KEY SUB2API_API_KEY SUB2API_USER_TOKEN ", " "+name+" ") {
					cmd.Env = append(cmd.Env, variable)
				}
			}
			cmd.Env = append(cmd.Env, "DEVIN_JOURNEY_GATEWAY_KEY="+key, "DEVIN_JOURNEY_UI_PASSWORD="+password, "DEVIN_JOURNEY_PROCESS_REGISTRY="+processRegistry, "DEVIN_JOURNEY_CONTAINER_REGISTRY="+containerRegistry)
			fixture.mu.Lock()
			before := len(fixture.chats)
			fixture.mu.Unlock()
			output, runErr := runGuardedJourney(t, cmd, app, admission, fence)
			cleanupErr := cleanupJourneyProcessesAt(processRegistry, containerRegistry)
			report := map[string]any{}
			parseErr := json.Unmarshal(output, &report)
			report["batch_guard"] = fence.snapshot()
			forwarded, _ := admission.snapshot()
			fixture.mu.Lock()
			nativeCalls := len(fixture.chats) - before
			fixture.mu.Unlock()
			report["public_requests"], report["observed_native_calls"] = forwarded, nativeCalls
			writeAgentArtifact(t, "journey-first-error-"+client+".json", report)
			require.NoError(t, parseErr)
			require.NoError(t, cleanupErr)
			require.Error(t, runErr)
			require.Equal(t, "failed", report["status"])
			require.Nil(t, report["resume"])
			require.Equal(t, 1, forwarded, "a tool-error retry or auto compact must not reach the gateway")
			require.Equal(t, 1, nativeCalls)
			guard := fence.snapshot()
			require.Equal(t, true, guard["stopped"])
			require.Equal(t, true, guard["owned_backend_death_verified"])
			require.Equal(t, "tool_error", guard["first_failure"].(map[string]any)["reason"])
		})
	}
}
