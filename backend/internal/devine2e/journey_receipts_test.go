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

// These replies ask the actual CLI to execute tools. The fixture never executes
// commands or writes a test receipt on the client's behalf.
func verifyProjectTestReceipts(t *testing.T, app *application, fixture *nativeFixture, key, password string) {
	t.Helper()
	require.Equal(t, "mock", os.Getenv("DEVIN_E2E_MODE"))
	for _, scenario := range []string{"canonical", "whitespace", "after_extra", "after_forbidden", "fake_ok", "failed_tests", "forbidden_equiv"} {
		t.Run("test_receipt_"+scenario, func(t *testing.T) {
			directory := t.TempDir()
			fixture.journey.mu.Lock()
			fixture.journey.workspace = filepath.Join(directory, "workspace")
			fixture.journey.mutationTool = "write"
			fixture.journey.testScenario = scenario
			fixture.journey.mu.Unlock()
			t.Cleanup(func() {
				fixture.journey.mu.Lock()
				fixture.journey.testScenario = ""
				fixture.journey.mu.Unlock()
			})
			admission := newJourneyAdmission(t, app, fixtureModel, 16)
			ui := journeyUIServer(t, app.base, admission.server.URL)
			script, err := filepath.Abs("../../../tools/devin-e2e-clients/journey.py")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "python3", script, "--directory", directory, "--client", "opencode", "--model", fixtureModel, "--ui-url", ui.URL, "--profile", "project")
			gracefulJourneyCommand(cmd)
			processRegistry, containerRegistry := journeyRegistryEnvironment(t, app)
			for _, variable := range os.Environ() {
				name, _, _ := strings.Cut(variable, "=")
				if !strings.Contains(" DEVIN_E2E_PAT DEVIN_E2E_CREDENTIAL DEVIN_PAT DEVIN_API_KEY SUB2API_API_KEY SUB2API_USER_TOKEN ", " "+name+" ") {
					cmd.Env = append(cmd.Env, variable)
				}
			}
			cmd.Env = append(cmd.Env, "DEVIN_JOURNEY_GATEWAY_KEY="+key, "DEVIN_JOURNEY_UI_PASSWORD="+password,
				"DEVIN_JOURNEY_PROCESS_REGISTRY="+processRegistry, "DEVIN_JOURNEY_CONTAINER_REGISTRY="+containerRegistry)
			output, runErr := cmd.Output()
			cleanupErr := cleanupJourneyProcessesAt(processRegistry, containerRegistry)
			var report map[string]any
			require.NoError(t, json.Unmarshal(output, &report))
			writeAgentArtifact(t, "journey-test-receipt-"+scenario+".json", report)
			require.NoError(t, cleanupErr)
			task := report["task"].(map[string]any)
			if scenario == "canonical" || scenario == "whitespace" || scenario == "after_extra" {
				require.NoError(t, runErr, "permitted actual test executions must be recognized")
				require.Equal(t, "passed", report["status"])
				require.Equal(t, true, task["client_tests_succeeded"])
			} else {
				require.Error(t, runErr, "a denied command or missing/failed real test cannot pass")
				require.Equal(t, "failed", report["status"])
				require.Nil(t, report["resume"])
				if scenario == "after_forbidden" {
					require.Equal(t, true, task["client_tests_succeeded"], "a later denied tool must not erase the earlier successful test evidence")
				} else {
					require.Equal(t, false, task["client_tests_succeeded"])
				}
			}
		})
	}
}
