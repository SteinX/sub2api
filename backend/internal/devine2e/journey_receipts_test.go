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
func verifyProjectTestReceipts(t *testing.T, app *application, fixture *nativeFixture, key, password, fusionModel string, keyID int64) {
	t.Helper()
	require.Equal(t, "mock", os.Getenv("DEVIN_E2E_MODE"))
	for _, scenario := range []string{"canonical", "whitespace", "after_extra", "after_forbidden", "fake_ok", "failed_tests", "forbidden_equiv", "fusion_failure"} {
		t.Run("test_receipt_"+scenario, func(t *testing.T) {
			directory := t.TempDir()
			fixture.journey.mu.Lock()
			fixture.journey.workspace = filepath.Join(directory, "workspace")
			fixture.journey.mutationTool = "write"
			fixture.journey.testScenario = scenario
			model := fixtureModel
			if scenario == "fusion_failure" {
				model = fusionModel
				fixture.journey.testScenario = "fake_ok"
			}
			fixture.journey.mu.Unlock()
			t.Cleanup(func() {
				fixture.journey.mu.Lock()
				fixture.journey.testScenario = ""
				fixture.journey.mu.Unlock()
			})
			admission := newJourneyAdmission(t, app, model, 16)
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
			cmd := exec.CommandContext(ctx, "python3", script, "--directory", directory, "--client", "opencode", "--model", model, "--ui-url", ui.URL, "--profile", "project")
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
			output, runErr := runGuardedJourney(t, cmd, app, admission, fence)
			cleanupErr := cleanupJourneyProcessesAt(processRegistry, containerRegistry)
			report := map[string]any{"status": "failed", "stage": "runner_output"}
			parseErr := json.Unmarshal(output, &report)
			if report == nil {
				report = map[string]any{"status": "failed", "stage": "runner_output"}
			}
			if scenario == "fusion_failure" {
				_, records := admission.snapshot()
				report["fusion_execution"] = snapshotFusionAudit(agentDatabase(t, app), keyID, records, false)
			}
			report["batch_guard"] = fence.snapshot()
			report["fixture_batch_billing"] = snapshotMockBilling(t, app, fixture, keyID)
			writeAgentArtifact(t, "journey-test-receipt-"+scenario+".json", report)
			require.NoError(t, parseErr)
			require.NoError(t, cleanupErr)
			task := report["task"].(map[string]any)
			if scenario == "canonical" || scenario == "after_extra" {
				require.NoError(t, runErr, "permitted actual test executions must be recognized")
				require.Equal(t, "passed", report["status"])
				require.Equal(t, true, task["client_tests_succeeded"])
				require.Equal(t, true, task["test_command_verified"])
				if scenario == "after_extra" {
					calls := task["test_execution"].(map[string]any)["calls"].([]any)
					require.Len(t, calls, 2)
					require.NotEqual(t, calls[0].(map[string]any)["run_sha256"], calls[1].(map[string]any)["run_sha256"], "each actual execution must produce its own receipt")
				}
			} else {
				require.Error(t, runErr, "a denied command or missing/failed real test cannot pass")
				require.Equal(t, "failed", report["status"])
				require.Nil(t, report["resume"])
				if scenario == "after_forbidden" {
					require.Equal(t, true, task["client_tests_succeeded"], "a later denied tool must not erase the earlier successful test evidence")
				} else {
					require.Equal(t, false, task["client_tests_succeeded"])
				}
				if scenario == "fusion_failure" {
					require.Contains(t, report["fusion_execution"], "complete", "retain partial local collaboration evidence before cleanup; a stopped turn is not retrospectively complete")
				}
				require.Equal(t, true, report["batch_guard"].(map[string]any)["stopped"])
				require.Equal(t, true, report["batch_guard"].(map[string]any)["owned_backend_death_verified"])
				guard := report["batch_guard"].(map[string]any)
				stop := guard["first_failure"].(map[string]any)["gate_closed_at_ns"].(int64)
				for _, event := range guard["journal"].([]map[string]any) {
					if event["action"] == "forward" {
						require.Less(t, event["at_ns"].(int64), stop, "no SDK retry, auto compact or continuation may be newly admitted after the first fatal observation")
					}
				}
				if scenario == "whitespace" || scenario == "after_forbidden" || scenario == "fake_ok" || scenario == "forbidden_equiv" {
					denied := false
					for _, value := range task["tool_calls"].([]any) {
						call := value.(map[string]any)
						denied = denied || call["name"] == "bash" && call["status"] == "error" && call["error_kind"] == "client_permission_rejection"
					}
					require.True(t, denied, "prove the actual CLI rejected this command under the copied product policy")
				}
				if scenario == "failed_tests" {
					call := task["test_execution"].(map[string]any)["calls"].([]any)[0].(map[string]any)
					require.Equal(t, true, call["execution_verified"])
					require.Equal(t, false, call["passed"])
					require.Equal(t, float64(1), call["exit_code"])
				}
			}
		})
	}
}
