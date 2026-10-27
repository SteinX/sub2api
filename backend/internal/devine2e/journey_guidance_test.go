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
func projectGuidanceProof(request *pb.GetChatMessageRequest, workspace string) map[string]any {
	var text strings.Builder
	text.WriteString(request.Prompt)
	for _, prompt := range request.ChatMessagePrompts {
		text.WriteString("\n")
		text.WriteString(prompt.Prompt)
	}
	content := text.String()
	configured := false
	for _, line := range strings.Split(content, "\n") {
		const prefix = "Project configuration JSON: "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		var data struct {
			Workspace string `json:"workspace"`
			Command   string `json:"test_command"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &data) == nil {
			configured = data.Workspace == workspace && data.Command == "python3 run_tests.py"
		}
	}
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
		"configuration_json_matches":         configured,
		"restricted_project_prompt":          strings.Contains(content, "not a general shell"),
		"dedicated_file_tool_guidance":       strings.Contains(content, "Read (read)") && strings.Contains(content, "Glob (glob)") && strings.Contains(content, "Grep (grep)") && strings.Contains(content, "Edit (edit)"),
		"exact_command_and_workdir_guidance": strings.Contains(content, "command exactly equal to test_command and workdir exactly equal to workspace"),
		"no_directory_shell_guidance":        strings.Contains(content, "Do not use Bash for pwd, ls, cat"),
		"project_task_prompt":                strings.Contains(content, "Repair price_math.py so the project tests pass"),
		"prompt_sha256":                      hex.EncodeToString(fingerprint[:]), "tools": tools,
		"tool_definition_sha256": definitions,
		"bash_command_parameter": command, "bash_workdir_parameter": workdir,
		"generic_bash_description_retained": generalBash,
	}
}

func verifyRestrictedProjectGuidance(t *testing.T, app *application, fixture *nativeFixture, key, password string, keyID int64) {
	t.Helper()
	require.Equal(t, "mock", os.Getenv("DEVIN_E2E_MODE"))
	for _, scenario := range []string{"canonical", "ls", "test_alias"} {
		if !t.Run("restricted_project_guidance_"+scenario, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "project with spaces")
			workspace := filepath.Join(directory, "workspace")
			fixture.journey.mu.Lock()
			fixture.journey.workspace, fixture.journey.mutationTool, fixture.journey.testScenario = workspace, "edit", "guidance_"+scenario
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
				proof = projectGuidanceProof(captured[0], workspace)
			}
			report["native_request_guidance"] = proof
			report["batch_guard"] = fence.snapshot()
			report["fixture_batch_billing"] = snapshotMockBilling(t, app, fixture, keyID)
			report["client_process_cleanup_verified"] = cleanupErr == nil
			defer func() { writeAgentArtifact(t, "journey-restricted-guidance-"+scenario+".json", report) }()
			require.NoError(t, parseErr)
			require.NoError(t, cleanupErr)
			require.NotEmpty(t, captured, "inspect actual CLI/native requests, not a separately generated prompt")
			for _, field := range []string{"configuration_json_matches", "restricted_project_prompt", "dedicated_file_tool_guidance", "exact_command_and_workdir_guidance", "no_directory_shell_guidance", "project_task_prompt", "bash_command_parameter", "bash_workdir_parameter"} {
				require.Equal(t, true, proof[field], "actual model input is missing %s", field)
			}
			tools := proof["tools"].([]string)
			for _, name := range []string{"read", "glob", "grep", "edit", "bash"} {
				require.Contains(t, tools, name)
			}
			for _, name := range []string{"task", "webfetch", "websearch"} {
				require.NotContains(t, tools, name)
			}
			task := report["task"].(map[string]any)
			dependencies := report["runtime_dependencies"].(map[string]any)["rg"].(map[string]any)
			require.Equal(t, true, dependencies["runnable"], "the isolated CLI test environment must supply its rg dependency; do not download it from the networkless client")
			if scenario == "canonical" {
				require.NoError(t, runErr)
				require.Equal(t, "passed", report["status"])
				require.Equal(t, true, report["source_changed_by_task"])
				require.Equal(t, true, task["client_tests_succeeded"])
				for _, name := range []string{"glob", "grep", "edit", "bash"} {
					require.Equal(t, float64(1), task["tools"].(map[string]any)[name], "exercise the permitted %s path in the actual CLI", name)
				}
				require.Equal(t, true, report["same_client_session"])
				observedWorkdir := false
				for _, request := range captured {
					for _, prompt := range request.ChatMessagePrompts {
						for _, call := range prompt.ToolCalls {
							if call.Name == "bash" {
								var args map[string]any
								require.NoError(t, json.Unmarshal([]byte(call.ArgumentsJson), &args))
								require.Equal(t, "python3 run_tests.py", args["command"])
								require.Equal(t, workspace, args["workdir"])
								observedWorkdir = true
							}
						}
					}
				}
				report["exact_test_workdir_observed"] = observedWorkdir
				require.True(t, observedWorkdir)
			} else {
				require.Error(t, runErr)
				require.Nil(t, report["resume"])
				require.Equal(t, false, report["source_changed_by_task"])
				require.Len(t, captured, 1, "the first denied tool must stop further model calls")
				calls := task["tool_calls"].([]any)
				require.Len(t, calls, 1)
				call := calls[0].(map[string]any)
				require.Equal(t, "client_permission_rejection", call["error_kind"])
				require.Equal(t, false, call["test_command"])
				category := "ls"
				if scenario == "test_alias" {
					category = "test_path_alias"
				}
				require.Equal(t, category, call["command_category"])
				guard := fence.snapshot()
				require.Equal(t, true, guard["stopped"])
				require.Equal(t, true, guard["owned_backend_death_verified"])
				require.Equal(t, "permission_rejection", guard["first_failure"].(map[string]any)["reason"])
			}
		}) {
			t.Fatal("restricted project guidance mock failed; stop for diagnosis")
		}
	}
}
