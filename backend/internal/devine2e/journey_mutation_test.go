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

// Only synthetic model replies are controlled here. Login, configuration copy,
// the CLI, its file tools, tests and the runner's independent oracle are real.
func verifyProjectMutationNegatives(t *testing.T, app *application, fixture *nativeFixture, key, password string, keyID int64) {
	t.Helper()
	require.Equal(t, "mock", os.Getenv("DEVIN_E2E_MODE"))
	for _, mutation := range []string{"noop", "failed_edit", "early_marker_error"} {
		t.Run("mutation_negative_"+mutation, func(t *testing.T) {
			directory := t.TempDir()
			fixture.journey.mu.Lock()
			fixture.journey.workspace = filepath.Join(directory, "workspace")
			fixture.journey.mutationTool = mutation
			fixture.journey.mu.Unlock()
			admission := newJourneyAdmission(t, app, fixtureModel, 12)
			fence := newJourneyFence(app, nil)
			t.Cleanup(func() {
				if fence.snapshot()["stopped"] == true {
					app.stop(t)
					app.start(t)
				}
			})
			ui := journeyUIServer(t, app.base, admission.server.URL)
			script, err := filepath.Abs("../../../tools/devin-e2e-clients/journey.py")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "python3", script, "--directory", directory, "--client", "opencode", "--model", fixtureModel, "--ui-url", ui.URL, "--profile", "project")
			gracefulJourneyCommand(cmd)
			processRegistry, containerRegistry := journeyRegistryEnvironment(t, app)
			for _, env := range os.Environ() {
				name, _, _ := strings.Cut(env, "=")
				if !strings.Contains(" DEVIN_E2E_PAT DEVIN_E2E_CREDENTIAL DEVIN_PAT DEVIN_API_KEY SUB2API_API_KEY SUB2API_USER_TOKEN ", " "+name+" ") {
					cmd.Env = append(cmd.Env, env)
				}
			}
			cmd.Env = append(cmd.Env, "DEVIN_JOURNEY_GATEWAY_KEY="+key, "DEVIN_JOURNEY_UI_PASSWORD="+password,
				"DEVIN_JOURNEY_PROCESS_REGISTRY="+processRegistry, "DEVIN_JOURNEY_CONTAINER_REGISTRY="+containerRegistry)
			output, runErr := runGuardedJourney(t, cmd, app, admission, fence)
			cleanupErr := cleanupJourneyProcessesAt(processRegistry, containerRegistry)
			var report map[string]any
			require.NoError(t, json.Unmarshal(output, &report))
			report["batch_guard"] = fence.snapshot()
			report["fixture_batch_billing"] = snapshotMockBilling(t, app, fixture, keyID)
			writeAgentArtifact(t, "journey-mutation-negative-"+mutation+".json", report)
			require.NoError(t, cleanupErr)
			require.Error(t, runErr, "a false final answer must not certify a broken project")
			require.Equal(t, "failed", report["status"])
			require.Equal(t, mutation == "early_marker_error", report["source_changed_by_task"])
			require.Equal(t, true, report["protected_project_files_intact"])
			require.Nil(t, report["resume"])
			require.Nil(t, report["artifact_verified_after_resume"], "no resume was attempted")
			task := report["task"].(map[string]any)
			if mutation == "noop" {
				require.Equal(t, true, task["passed"], "fixture must produce a normal, superficially successful CLI terminal")
				require.Equal(t, false, report["independent_project_test_passed"])
				require.Equal(t, true, task["final_text_matches"], "marker alone cannot prove the project was completed")
			} else if mutation == "failed_edit" {
				require.Equal(t, true, task["event_error"])
				states := task["tool_states"].(map[string]any)
				require.Equal(t, float64(1), states["edit"].(map[string]any)["error"])
			} else {
				require.Equal(t, true, task["marker_seen_anywhere"], "prove an early marker really reached the client")
				require.Equal(t, true, report["independent_project_test_executed"])
				require.Equal(t, true, report["independent_project_test_passed"], "successful project proof cannot replace a failed CLI terminal")
				require.Equal(t, false, task["passed"])
			}
		})
	}
}
