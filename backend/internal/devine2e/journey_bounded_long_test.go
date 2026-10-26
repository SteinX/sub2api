//go:build devine2e

package devine2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var longLinePrefix = regexp.MustCompile(`(?m)^[ \t]*[0-9]+(?:\||:) ?`)

func normalizedLongOutput(value string) string {
	return longLinePrefix.ReplaceAllString(strings.ReplaceAll(value, "\r\n", "\n"), "")
}

// Pressure at the actual PUBLIC boundary; not a claim about a native model's
// context after role isolation/compaction. Only successful, source-complete
// requests and actual completed read call IDs can supply this proof.
func longPublicContext(directory string, records []agentRequest, report map[string]any) map[string]any {
	result := map[string]any{"complete": false, "max_complete_parts": 0, "complete_payload_bytes": 0, "scope": "single successful public request, completed read tool outputs; not native role context/compaction", "compression": "not_claimed"}
	root, err := os.OpenRoot(filepath.Join(directory, "workspace"))
	if err != nil {
		return result
	}
	defer root.Close()
	task, _ := report["task"].(map[string]any)
	actualReads := map[string]bool{}
	actualTools, _ := task["tool_calls"].([]any)
	for _, value := range actualTools {
		tool, _ := value.(map[string]any)
		id, _ := tool["call_sha256"].(string)
		if tool["name"] == "read" && tool["status"] == "completed" {
			actualReads[id] = true
		}
	}
	reads, _ := task["long_reads"].(map[string]any)
	parts, _ := reads["parts"].([]any)
	type partData struct{ name, text string }
	expected := map[string]partData{}
	hashes, _ := report["long_file_sha256"].(map[string]any)
	for _, value := range parts {
		part, ok := value.(map[string]any)
		if !ok || part["complete"] != true {
			continue
		}
		name, _ := part["part"].(string)
		id, _ := part["call_sha256"].(string)
		if !actualReads[id] {
			continue
		}
		if !regexp.MustCompile(`^context/part0[0-7]\.txt$`).MatchString(name) {
			continue
		}
		raw, readErr := root.ReadFile(name)
		if readErr != nil || len(raw) != 16384 || callHash(string(raw)) != hashes[name] || callHash(string(raw)) != part["file_sha256"] {
			continue
		}
		expected[id] = partData{name, string(raw)}
	}
	best := 0
	for _, record := range records {
		if !record.Forwarded || record.Status != 200 || record.response == nil || !record.response.eof || !validJourneyCompletion(record.response.data, record.Stream) {
			continue
		}
		found := map[string]bool{}
		for _, item := range record.items {
			if item.Type != "function_call_output" {
				continue
			}
			id := callHash(item.CallID)
			if raw, ok := expected[id]; ok && strings.Contains(normalizedLongOutput(item.Output), raw.text) {
				found[raw.name] = true
			}
		}
		if len(found) > best {
			best = len(found)
			result["max_complete_parts"] = best
			result["complete_payload_bytes"] = best * 16384
		}
	}
	result["complete"] = best == 8 && len(expected) == 8
	return result
}

func verifyBoundedLongJourney(t *testing.T, app *application, fixture *nativeFixture, password, fusionModel string, originalKeyID int64) {
	t.Helper()
	user := login(t, app, "user@devin-e2e.invalid", password)
	original := api(t, app, "GET", fmt.Sprintf("/keys/%d", originalKeyID), user, nil)
	api(t, app, "PUT", fmt.Sprintf("/keys/%d", originalKeyID), user, map[string]any{"name": "saved-short-fixture-key"})
	defer api(t, app, "PUT", fmt.Sprintf("/keys/%d", originalKeyID), user, map[string]any{"name": "e2e-key"})
	for _, scenario := range []string{"complete", "missing_part", "wrong_fact", "tool_recall"} {
		// Independent synthetic fixtures, not budget resets of an authorized batch.
		// The real combined runner always retains ONE key and ONE durable ledger.
		created := api(t, app, "POST", "/keys", user, map[string]any{"name": "e2e-key", "group_id": original.Get("group_id").Int(), "quota": 1})
		key, keyID := created.Get("key").String(), created.Get("id").Int()
		app.redactor.addSecret(key)
		passed := t.Run("bounded_long_"+scenario, func(t *testing.T) {
			directory := t.TempDir()
			fixture.journey.mu.Lock()
			fixture.journey.workspace = filepath.Join(directory, "workspace")
			fixture.journey.testScenario = "bounded_long_" + scenario
			fixture.journey.mu.Unlock()
			t.Cleanup(func() {
				fixture.journey.mu.Lock()
				fixture.journey.testScenario = ""
				fixture.journey.mu.Unlock()
				app.stop(t)
				app.start(t)
			})
			budget := newFixtureLedgerBudget(t, &authorizedJourneyBudget{db: agentDatabase(t, app), keyID: keyID}, true)
			fence := newJourneyFence(app, budget)
			id := "opencode/fusion/long"
			require.NoError(t, fence.installToolLimits(journeyToolLimits{Operations: []string{id + "/task", id + "/resume"}, PerOperation: 12, Total: 60, NoToolsOperations: []string{id + "/resume"}}))
			admission := newJourneyAdmission(t, app, fusionModel, 40)
			admission.runBudget = budget
			admission.toolCase = id
			ui := journeyUIServer(t, app.base, admission.server.URL)
			script, err := filepath.Abs("../../../tools/devin-e2e-clients/journey.py")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "python3", script, "--directory", directory, "--client", "opencode", "--model", fusionModel, "--ui-url", ui.URL, "--profile", "project", "--project-kind", "long")
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
			_, records := admission.snapshot()
			report["batch_guard"] = fence.snapshot()
			report["public_context_witness"] = longPublicContext(directory, records, report)
			report["client_process_cleanup_verified"] = cleanupErr == nil
			report["fusion_execution"] = snapshotFusionAudit(agentDatabase(t, app), keyID, records, false)
			defer func() { writeAgentArtifact(t, "journey-bounded-long-"+scenario+".json", report) }()
			require.NoError(t, parseErr)
			require.NoError(t, cleanupErr)
			require.EqualValues(t, 131072, report["dataset_bytes"])
			require.Equal(t, false, report["compression_claim"])
			require.Equal(t, "not_claimed", report["compact"])
			if scenario == "complete" {
				require.NoError(t, runErr)
				require.Equal(t, "passed", report["status"])
				require.Equal(t, true, report["same_client_session"])
				require.Equal(t, true, report["client_restarted_verified"])
				require.Equal(t, true, report["public_context_witness"].(map[string]any)["complete"])
				require.Equal(t, true, report["fusion_execution"].(map[string]any)["complete"])
				resumed := report["resume"].(map[string]any)
				require.Empty(t, resumed["tool_calls"])
				require.Equal(t, true, resumed["memory_recall"].(map[string]any)["passed"])
			} else {
				require.Error(t, runErr)
				require.Equal(t, "failed", report["status"])
				if scenario == "missing_part" {
					require.Nil(t, report["resume"])
					require.Equal(t, false, report["public_context_witness"].(map[string]any)["complete"])
				}
				if scenario == "wrong_fact" {
					require.Equal(t, false, report["resume"].(map[string]any)["memory_recall"].(map[string]any)["passed"])
				}
				if scenario == "tool_recall" {
					require.Equal(t, "tool_budget_exhausted", fence.snapshot()["first_failure"].(map[string]any)["reason"])
					require.Empty(t, report["resume"].(map[string]any)["tool_calls"])
				}
			}
		})
		api(t, app, "PUT", fmt.Sprintf("/keys/%d", keyID), user, map[string]any{"name": "bounded-long-" + scenario, "status": "inactive"})
		if !passed {
			t.Fatal("bounded long mock regression failed; stop for diagnosis")
		}
	}
}
