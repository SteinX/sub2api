//go:build devine2e

package devine2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Real product login/Key/model/copy -> unmodified pinned OpenCode -> public
// gateway -> scripted model. This is a client/mock regression, never live E2E.
func verifyProjectToolAdmission(t *testing.T, app *application, fixture *nativeFixture, key, password, fusionModel string, keyID int64) {
	t.Helper()
	for _, kind := range []string{"direct", "fusion"} {
		for _, limit := range []int{12, 13} {
			name := fmt.Sprintf("tool_admission_%s_%d", kind, limit)
			if !t.Run(name, func(t *testing.T) {
				directory := t.TempDir()
				fixture.journey.mu.Lock()
				fixture.journey.workspace = filepath.Join(directory, "workspace")
				fixture.journey.testScenario = fmt.Sprintf("tool_limit_%d", limit)
				fixture.journey.mutationTool = "edit"
				fixture.journey.mu.Unlock()
				t.Cleanup(func() {
					fixture.journey.mu.Lock()
					fixture.journey.testScenario = ""
					fixture.journey.mutationTool = ""
					fixture.journey.mu.Unlock()
					app.stop(t)
					app.start(t)
				})
				model := fixtureModel
				if kind == "fusion" {
					model = fusionModel
				}
				budget := newFixtureLedgerBudget(t, &authorizedJourneyBudget{db: agentDatabase(t, app), keyID: keyID})
				fence := newJourneyFence(app, budget)
				id := "opencode/" + kind + "/project"
				require.NoError(t, fence.installToolLimits(journeyToolLimits{Operations: []string{id + "/task", id + "/resume"}, PerOperation: 12, Total: 48}))
				admission := newJourneyAdmission(t, app, model, 40)
				admission.runBudget = budget
				admission.toolCase = id
				ui := journeyUIServer(t, app.base, admission.server.URL)
				script, err := filepath.Abs("../../../tools/devin-e2e-clients/journey.py")
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
				defer cancel()
				cmd := exec.CommandContext(ctx, "python3", script, "--directory", directory, "--client", "opencode", "--model", model, "--ui-url", ui.URL, "--profile", "project")
				gracefulJourneyCommand(cmd)
				processRegistry, containerRegistry := journeyRegistryEnvironment(t, app)
				cmd.Env = fixturePolicyEnvironment()
				cmd.Env = append(cmd.Env, "DEVIN_JOURNEY_GATEWAY_KEY="+key, "DEVIN_JOURNEY_UI_PASSWORD="+password, "DEVIN_JOURNEY_PROCESS_REGISTRY="+processRegistry, "DEVIN_JOURNEY_CONTAINER_REGISTRY="+containerRegistry, "DEVIN_JOURNEY_CAPABILITIES=false")
				output, runErr := runGuardedJourney(t, cmd, app, admission, fence)
				cleanupErr := cleanupJourneyProcessesAt(processRegistry, containerRegistry)
				report := map[string]any{}
				parseErr := json.Unmarshal(output, &report)
				if report == nil {
					report = map[string]any{}
				}
				report["batch_guard"] = fence.snapshot()
				report["client_process_cleanup_verified"] = cleanupErr == nil
				report["fixture_kind"], report["proposed_tools_in_task"] = kind, limit
				// Deliberately synthetic scope: no executable native approval exists.
				raw, ledgerErr := os.ReadFile(budget.ledger)
				var ledger map[string]any
				if ledgerErr == nil {
					ledgerErr = json.Unmarshal(raw, &ledger)
				}
				if ledgerErr == nil {
					report["durable_tool_budget"] = map[string]any{"reserved": ledger["tools_reserved"], "operations": ledger["tool_operations"], "stopped": ledger["stopped"]}
				}
				defer func() { writeAgentArtifact(t, "journey-"+name+".json", report) }()
				require.NoError(t, parseErr)
				require.NoError(t, cleanupErr)
				require.NoError(t, ledgerErr)
				task, ok := report["task"].(map[string]any)
				require.True(t, ok)
				calls := task["tool_calls"].([]any)
				require.Len(t, calls, 12, "thirteenth tool must not even start")
				for _, value := range calls {
					require.Equal(t, "completed", value.(map[string]any)["status"])
				}
				tests := task["test_execution"].(map[string]any)["calls"].([]any)
				require.Len(t, tests, 7, "a thirteenth bash would create an eighth independent receipt")
				for _, receipt := range tests {
					require.Equal(t, true, receipt.(map[string]any)["execution_verified"])
				}
				if limit == 12 {
					require.NoError(t, runErr)
					require.Equal(t, "passed", report["status"])
					require.Equal(t, true, report["same_client_session"])
					require.EqualValues(t, 13, ledger["tools_reserved"], "12 in task, 1 in the separately approved resume")
					require.Equal(t, false, ledger["stopped"])
				} else {
					require.Error(t, runErr)
					require.Equal(t, "failed", report["status"])
					require.Nil(t, report["resume"])
					guard := fence.snapshot()
					require.Equal(t, "tool_budget_exhausted", guard["first_failure"].(map[string]any)["reason"])
					require.Equal(t, true, guard["first_failure"].(map[string]any)["stop_persistence_verified"])
					require.Equal(t, true, ledger["stopped"])
					require.EqualValues(t, 12, ledger["tools_reserved"])
					// A fresh process with the same ledger cannot bypass the stop, despite
					// a new session and unused public/cost quota. No request is sent here.
					before := string(raw)
					_, err := budget.ToolBeginForRestart(id + "/resume")
					require.Error(t, err)
					after, err := os.ReadFile(budget.ledger)
					require.NoError(t, err)
					require.Equal(t, before, string(after))
				}
			}) {
				t.Fatal("tool limit regression failed; stop the mock suite for diagnosis")
			}
		}
	}
}

func (b *fixtureLedgerBudget) ToolBeginForRestart(operation string) ([]byte, error) {
	output, err := b.command("-c", fixtureLedgerAction, b.policyRoot, b.ledger, b.plan, "tool_begin", operation)
	if err != nil {
		return output, err
	}
	var decision struct {
		Authorized bool `json:"authorized"`
	}
	if json.Unmarshal(output, &decision) != nil || !decision.Authorized {
		return output, fmt.Errorf("stopped ledger denied restart")
	}
	return output, nil
}
