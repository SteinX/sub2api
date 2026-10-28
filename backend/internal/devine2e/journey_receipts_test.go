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
	for _, scenario := range []string{"canonical", "whitespace", "after_extra", "after_forbidden", "fake_ok", "failed_tests", "forbidden_equiv", "fusion_failure",
		"result_bash", "result_history_resume", "result_wrong_artifact", "result_forged_test", "result_stale_test", "result_fusion", "result_log_reread", "shell_exit127", "read_error_recover", "result_shell_fallback"} {
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
			if scenario == "result_fusion" {
				model = fusionModel
				fixture.journey.testScenario = "result_bash"
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
			if scenario == "whitespace" || scenario == "after_forbidden" || scenario == "forbidden_equiv" {
				// Deliberately stricter existing USER preferences, not product policy.
				cmd.Args = append(cmd.Args, "--fixture-user-policy", "test-command")
			}
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
			if scenario == "fusion_failure" || scenario == "result_fusion" {
				_, records := admission.snapshot()
				report["fusion_execution"] = snapshotFusionAudit(agentDatabase(t, app), keyID, records, false)
			}
			report["batch_guard"] = fence.snapshot()
			report["client_process_cleanup_verified"] = cleanupErr == nil
			report["fixture_batch_billing"] = snapshotMockBilling(t, app, fixture, keyID)
			writeAgentArtifact(t, "journey-test-receipt-"+scenario+".json", report)
			require.NoError(t, parseErr)
			require.NoError(t, cleanupErr)
			task, taskPresent := report["task"].(map[string]any)
			require.True(t, taskPresent, "runner returned no task report (stage=%v)", report["stage"])
			if scenario == "canonical" || scenario == "after_extra" || scenario == "shell_exit127" || scenario == "read_error_recover" || strings.HasPrefix(scenario, "result_") && scenario != "result_wrong_artifact" {
				require.NoError(t, runErr, "permitted actual test executions must be recognized")
				require.Equal(t, "passed", report["status"])
				require.Equal(t, true, report["independent_project_tests_passed"])
				if scenario == "shell_exit127" || scenario == "read_error_recover" {
					require.Equal(t, float64(1), task["recoverable_tool_errors"])
					require.Equal(t, true, report["same_client_session"])
					require.Equal(t, true, report["artifact_verified_before_oracle"])
					require.Equal(t, false, report["batch_guard"].(map[string]any)["stopped"])
					forwarded, _ := admission.snapshot()
					require.Greater(t, forwarded, 1, "the actual CLI delivered failure feedback before a new model decision")
					if scenario == "shell_exit127" {
						diagnostic := task["tool_calls"].([]any)[0].(map[string]any)["failure_diagnostic"].(map[string]any)
						require.Equal(t, float64(127), diagnostic["client_exit_code"])
					}
				}
				if scenario == "result_forged_test" || scenario == "result_stale_test" {
					require.Equal(t, false, task["client_tests_succeeded"], "untrusted client receipt cannot decide result correctness")
				} else {
					require.Equal(t, true, task["client_tests_succeeded"], "diagnostic only; independent tests authorize success")
				}
				if strings.HasPrefix(scenario, "result_") {
					require.Empty(t, task["source_mutation_tools"], "Bash repair is proved by changed source and oracle, not a named Edit/Write tool")
					require.Equal(t, float64(0), task["tools"].(map[string]any)["read"])
					require.Equal(t, true, report["independent_project_test_passed"])
					resumed := report["resume"].(map[string]any)
					require.Equal(t, float64(0), resumed["tools"].(map[string]any)["read"])
					if scenario == "result_history_resume" {
						require.Empty(t, resumed["tool_calls"], "a correct same-session resume need not reread the file")
					}
					if scenario == "result_fusion" {
						require.Equal(t, true, report["fusion_execution"].(map[string]any)["complete"])
					}
					if scenario == "result_log_reread" {
						diagnostics := task["test_execution"].(map[string]any)
						calls := diagnostics["calls"].([]any)
						require.Len(t, calls, 2)
						require.Equal(t, "repeated_display", calls[1].(map[string]any)["receipt_status"])
						require.Equal(t, false, diagnostics["conflicting_evidence"])
					}
					if scenario == "result_shell_fallback" {
						for _, value := range task["tool_calls"].([]any) {
							require.NotContains(t, value.(map[string]any), "failure_diagnostic", "a successful shell fallback is a successful tool, even if its output mentions a missing program")
						}
					}
					if scenario == "result_forged_test" {
						receipt := task["test_execution"].(map[string]any)["calls"].([]any)[0].(map[string]any)
						require.Equal(t, "file_unavailable", receipt["receipt_status"])
					}
				}
				if scenario == "after_extra" {
					calls := task["test_execution"].(map[string]any)["calls"].([]any)
					require.Len(t, calls, 2)
					require.NotEqual(t, calls[0].(map[string]any)["run_sha256"], calls[1].(map[string]any)["run_sha256"], "each actual execution must produce its own receipt")
				}
			} else {
				require.Error(t, runErr, "a denied command or missing/failed real test cannot pass")
				require.Equal(t, "failed", report["status"])
				require.Nil(t, report["resume"])
				if scenario == "after_forbidden" || scenario == "result_wrong_artifact" {
					require.Equal(t, true, task["client_tests_succeeded"], "a later denied tool must not erase the earlier successful test evidence")
				} else {
					require.Equal(t, false, task["client_tests_succeeded"])
				}
				if strings.HasPrefix(scenario, "result_") {
					require.Equal(t, true, report["source_changed_by_task"])
					require.Equal(t, true, report["independent_project_test_passed"])
					if scenario == "result_wrong_artifact" {
						require.Equal(t, false, report["artifact_verified_before_oracle"])
					} else {
						require.Equal(t, true, report["artifact_verified_before_oracle"])
					}
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
				if scenario == "whitespace" || scenario == "after_forbidden" || scenario == "forbidden_equiv" {
					denied := false
					for _, value := range task["tool_calls"].([]any) {
						call := value.(map[string]any)
						denied = denied || call["name"] == "bash" && call["status"] == "error" && call["error_kind"] == "client_permission_rejection"
					}
					require.True(t, denied, "prove the product copy preserved this user's explicit native deny policy")
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
