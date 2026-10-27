//go:build devine2e

package devine2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
)

// Observe requests received by the synthetic native server, after the copied
// product configuration and real pinned CLI have constructed the model input.
// Persist only fixed observations/hashes, never prompts, commands or paths.
func nativeClientProof(request *pb.GetChatMessageRequest) map[string]any {
	var text strings.Builder
	text.WriteString(request.Prompt)
	for _, prompt := range request.ChatMessagePrompts {
		text.WriteString("\n")
		text.WriteString(prompt.Prompt)
	}
	content := text.String()
	tools := []string{}
	definitions := map[string]string{}
	command, workdir, generalBash := false, false, false
	for _, tool := range request.Tools {
		switch tool.Name {
		case "read", "glob", "grep", "edit", "write", "apply_patch", "bash", "task", "webfetch", "websearch":
			tools = append(tools, tool.Name)
			data, _ := json.Marshal([]string{tool.Description, tool.JsonSchemaString})
			sum := sha256.Sum256(data)
			definitions[tool.Name] = hex.EncodeToString(sum[:])
		default:
			tools = append(tools, "other")
		}
		if tool.Name == "bash" {
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if json.Unmarshal([]byte(tool.JsonSchemaString), &schema) == nil {
				_, command = schema.Properties["command"]
				_, workdir = schema.Properties["workdir"]
			}
			generalBash = strings.Contains(tool.Description, "Directory Verification")
		}
	}
	sort.Strings(tools)
	fingerprint := sha256.Sum256([]byte(content))
	return map[string]any{
		"product_agent_override_absent":      !strings.Contains(content, "Project configuration JSON:"),
		"directory_shell_prohibition_absent": !strings.Contains(content, "Do not use Bash for pwd, ls, cat"),
		"project_task_prompt":                strings.Contains(content, "Repair price_math.py so the project tests pass"),
		"prompt_sha256":                      hex.EncodeToString(fingerprint[:]), "tools": tools,
		"tool_definition_sha256": definitions,
		"bash_command_parameter": command, "bash_workdir_parameter": workdir,
		"generic_bash_description_retained": generalBash,
	}
}

func verifyNativeClientWorkflow(t *testing.T, app *application, fixture *nativeFixture, key, password string, keyID int64) {
	t.Helper()
	require.Equal(t, "mock", os.Getenv("DEVIN_E2E_MODE"))
	for _, scenario := range []string{"default", "alias", "approved", "ask", "deny"} {
		if !t.Run("native_client_workflow_"+scenario, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "project with spaces")
			workspace := filepath.Join(directory, "workspace")
			fixture.journey.mu.Lock()
			fixture.journey.workspace, fixture.journey.mutationTool, fixture.journey.testScenario = workspace, "edit", "native_"+scenario
			fixture.journey.mu.Unlock()
			fixture.mu.Lock()
			before := len(fixture.chats)
			fixture.mu.Unlock()
			admission := newJourneyAdmission(t, app, fixtureModel, 16)
			fence := newJourneyFence(app, nil)
			t.Cleanup(func() {
				fixture.journey.mu.Lock()
				fixture.journey.testScenario, fixture.journey.mutationTool = "", ""
				fixture.journey.mu.Unlock()
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
			if scenario == "ask" || scenario == "deny" || scenario == "approved" {
				cmd.Args = append(cmd.Args, "--fixture-user-policy", scenario)
			}
			gracefulJourneyCommand(cmd)
			processes, containers := journeyRegistryEnvironment(t, app)
			cmd.Env = append(fixturePolicyEnvironment(), "DEVIN_JOURNEY_GATEWAY_KEY="+key, "DEVIN_JOURNEY_UI_PASSWORD="+password, "DEVIN_JOURNEY_PROCESS_REGISTRY="+processes, "DEVIN_JOURNEY_CONTAINER_REGISTRY="+containers, "DEVIN_JOURNEY_CAPABILITIES=false")
			output, runErr := runGuardedJourney(t, cmd, app, admission, fence)
			cleanupErr := cleanupJourneyProcessesAt(processes, containers)
			report := map[string]any{}
			parseErr := json.Unmarshal(output, &report)
			if report == nil {
				report = map[string]any{}
			}
			fixture.mu.Lock()
			captured := append([]*pb.GetChatMessageRequest(nil), fixture.chats[before:]...)
			fixture.mu.Unlock()
			proof := map[string]any{"available": len(captured) > 0}
			if len(captured) > 0 {
				proof = nativeClientProof(captured[0])
			}
			report["native_request_client_defaults"] = proof
			report["expected_native_policy"] = scenario
			report["batch_guard"] = fence.snapshot()
			report["fixture_batch_billing"] = snapshotMockBilling(t, app, fixture, keyID)
			report["client_process_cleanup_verified"] = cleanupErr == nil
			defer func() { writeAgentArtifact(t, "journey-native-client-"+scenario+".json", report) }()
			require.NoError(t, parseErr)
			require.NoError(t, cleanupErr)
			require.NotEmpty(t, captured, "inspect actual CLI/native requests, not a separately generated prompt")
			for _, field := range []string{"product_agent_override_absent", "directory_shell_prohibition_absent", "project_task_prompt"} {
				require.Equal(t, true, proof[field], "actual model input is missing %s", field)
			}
			tools := proof["tools"].([]string)
			for _, name := range []string{"read", "glob", "grep", "edit"} {
				require.Contains(t, tools, name)
			}
			if scenario == "deny" {
				require.NotContains(t, tools, "bash", "the copied product config must preserve the user's native tool denial")
			} else {
				require.Contains(t, tools, "bash")
				require.Equal(t, true, proof["bash_command_parameter"])
				require.Equal(t, true, proof["bash_workdir_parameter"])
			}
			task := report["task"].(map[string]any)
			dependencies := report["runtime_dependencies"].(map[string]any)["rg"].(map[string]any)
			require.Equal(t, true, dependencies["runnable"], "the isolated CLI test environment must supply its rg dependency; do not download it from the networkless client")
			if scenario != "ask" && scenario != "deny" {
				require.NoError(t, runErr)
				require.Equal(t, "passed", report["status"])
				require.Equal(t, true, report["source_changed_by_task"])
				require.Equal(t, true, task["client_tests_succeeded"])
				for _, name := range []string{"glob", "grep", "edit"} {
					require.Equal(t, float64(1), task["tools"].(map[string]any)[name], "exercise the permitted %s path in the actual CLI", name)
				}
				require.Equal(t, float64(2), task["tools"].(map[string]any)["bash"], "ordinary ls plus the actual project test must execute")
				require.Equal(t, true, report["same_client_session"])
				observedWorkdir := false
				for _, request := range captured {
					for _, prompt := range request.ChatMessagePrompts {
						for _, call := range prompt.ToolCalls {
							if call.Name == "bash" {
								var args map[string]any
								require.NoError(t, json.Unmarshal([]byte(call.ArgumentsJson), &args))
								if args["command"] == "ls" {
									continue
								}
								expected := "python3 run_tests.py"
								if scenario == "alias" {
									expected = "python3 ./run_tests.py"
								}
								require.Equal(t, expected, args["command"], "do not rewrite the model's command")
								require.Equal(t, workspace, args["workdir"])
								observedWorkdir = true
							}
						}
					}
				}
				report["actual_test_workdir_observed"] = observedWorkdir
				require.True(t, observedWorkdir)
			} else {
				require.Error(t, runErr)
				require.Nil(t, report["resume"])
				require.Equal(t, false, report["source_changed_by_task"])
				require.Len(t, captured, 1)
				guard := fence.snapshot()
				if scenario == "ask" {
					calls := task["tool_calls"].([]any)
					require.Len(t, calls, 1)
					call := calls[0].(map[string]any)
					require.Equal(t, "client_permission_rejection", call["error_kind"])
					require.Equal(t, false, call["test_command"])
					require.Equal(t, "ls", call["command_category"])
					require.Equal(t, true, task["native_permission_request_seen"])
					require.Equal(t, true, task["native_noninteractive_auto_reject_seen"], "do not bypass native approval in a noninteractive client")
					require.Equal(t, "permission_rejection", guard["first_failure"].(map[string]any)["reason"])
					report["expected_outcome"] = "native_user_ask_stops_noninteractive_run"
				} else {
					require.Empty(t, task["tool_calls"])
					require.Equal(t, false, task["client_tests_succeeded"])
					require.Equal(t, "source_or_oracle_failure", guard["first_failure"].(map[string]any)["reason"])
					report["expected_outcome"] = "native_user_deny_removes_bash_and_project_remains_unmodified"
				}
				require.Equal(t, true, guard["stopped"])
				require.Equal(t, true, guard["owned_backend_death_verified"])
			}
		}) {
			t.Fatal("native client workflow mock failed; stop for diagnosis")
		}
	}
}
