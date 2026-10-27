//go:build devine2e

package devine2e

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

// This is a scripted MODEL, not a tool executor. It never opens project files,
// runs commands, creates output artifacts or decides whether the journey passed.
type projectJourney struct {
	mu              sync.Mutex
	workspace       string
	mutationTool    string
	testScenario    string
	maxContextBytes int
}

func (j *projectJourney) boundedLongScenario(workspace, scenario string) func() {
	j.mu.Lock()
	oldWorkspace, oldMutation, oldScenario, oldMax := j.workspace, j.mutationTool, j.testScenario, j.maxContextBytes
	j.workspace, j.mutationTool, j.testScenario, j.maxContextBytes = workspace, "edit", "bounded_long_"+scenario, 0
	j.mu.Unlock()
	return func() {
		j.mu.Lock()
		j.workspace, j.mutationTool, j.testScenario, j.maxContextBytes = oldWorkspace, oldMutation, oldScenario, oldMax
		j.mu.Unlock()
	}
}

const fixedProjectSource = `import json
from pathlib import Path

def discounted(subtotal, discount):
    return subtotal - discount

def report():
    data = json.loads(Path("requirements.json").read_text())
    result = {"nonce": data["nonce"], "totals": [discounted(row[0], row[1]) for row in data["rows"]]}
    Path("out").mkdir(exist_ok=True)
    Path("out/report.json").write_text(json.dumps(result))
    return result
`

func (j *projectJourney) reply(request *pb.GetChatMessageRequest, reply *pb.GetChatMessageResponse) {
	j.mu.Lock()
	workspace := j.workspace
	mutationTool := j.mutationTool
	testScenario := j.testScenario
	j.mu.Unlock()
	if mutationTool == "" {
		mutationTool = "write"
	}
	reply.DeltaText, reply.DeltaToolCalls = "PROJECT_FAILED", nil
	reply.Usage.BillingModelUid = request.ChatModelUid
	reply.StopReason = pb.StopReason_STOP_REASON_STOP_PATTERN
	var history strings.Builder
	for _, prompt := range request.ChatMessagePrompts {
		history.WriteString(prompt.Prompt)
	}
	j.mu.Lock()
	if history.Len() > j.maxContextBytes {
		j.maxContextBytes = history.Len()
	}
	j.mu.Unlock()
	// A fixed eleven-token fixture response makes a real client's compactor
	// believe even a large tool history is empty. Use a stated synthetic estimate
	// that grows with the actual forwarded context; never treat it as real usage.
	reply.Usage.InputTokens = uint64(1 + history.Len()/4)
	checkpointNonce := regexp.MustCompile(`CONTEXT_NONCE=([a-f0-9]{24})`).FindStringSubmatch(history.String())
	if len(request.Tools) == 0 && len(checkpointNonce) == 2 {
		reply.DeltaText = "Project checkpoint: CONTEXT_NONCE=" + checkpointNonce[1] + "; verified subtotal minus discount, tests passed, report at out/report.json."
		return
	}
	lastUser, lastReport := -1, -1
	userTask := ""
	for i, prompt := range request.ChatMessagePrompts {
		if prompt.Source == pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_USER && prompt.ToolCallId == "" {
			lastUser, userTask = i, prompt.Prompt
		}
	}
	resume := strings.Contains(userTask, "Resume this project session")
	longRecall := strings.Contains(userTask, "BOUNDED_LONG_RECALL")
	longTask := strings.Contains(userTask, "BOUNDED_LONG_PROJECT")
	pressure := strings.Contains(userTask, "Exercise context pressure")
	if strings.Contains(userTask, "Recall the shared CONTEXT_NONCE") {
		if len(checkpointNonce) == 2 {
			reply.DeltaText = checkpointNonce[1]
		}
		return
	}
	final := "The project is repaired. Tests passed and the JSON report was verified."
	if resume {
		final = "The existing project report remains correct; the completed work is verified."
	}
	if mutationTool == "noop" {
		final = "PROJECT_COMPLETE" // deliberately false marker, without a valid artifact
	}
	defer func() {
		if mutationTool == "early_marker_error" {
			if len(reply.DeltaToolCalls) > 0 {
				reply.DeltaText = "PROJECT_COMPLETE"
			} else if reply.DeltaText == final {
				reply.StopReason = pb.StopReason_STOP_REASON_MAX_TOKENS
			}
		}
	}()
	type priorCall struct{ name, path string }
	calls := map[string]priorCall{}
	reads := map[string]int{}
	wrote, tested, mutationFailed := false, false, false
	globbed, grepped := false, false
	requestedCommands := map[string]int{}
	requestedCallIDs := map[string]bool{}
	for i, prompt := range request.ChatMessagePrompts {
		for _, call := range prompt.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(call.ArgumentsJson), &args)
			if call.Name == "bash" && !requestedCallIDs[call.Id] {
				if command, ok := args["command"].(string); ok {
					requestedCommands[command]++
					requestedCallIDs[call.Id] = true
				}
			}
			path := ""
			for _, field := range []string{"filePath", "path", "file_path"} {
				if value, ok := args[field].(string); ok {
					path = value
				}
			}
			calls[call.Id] = priorCall{call.Name, path}
		}
		call := calls[prompt.ToolCallId]
		if prompt.ToolResultIsError && (call.name == "edit" || call.name == "write") {
			mutationFailed = true
		}
		if call.name == fusion.DelegateTool {
			lastReport = i
		}
		if i <= lastUser || prompt.ToolCallId == "" || prompt.ToolResultIsError {
			continue
		}
		switch call.name {
		case "read", "read_file":
			reads[filepath.Base(call.path)]++
		case "write", "edit":
			wrote = true
		case "bash":
			tested = true
		case "glob":
			globbed = true
		case "grep":
			grepped = true
		}
	}
	call := func(tool *pb.ChatToolDefinition, args map[string]any) {
		args["i"] = "Complete the requested synthetic project task"
		encoded, _ := json.Marshal(args)
		reply.DeltaText = ""
		reply.DeltaToolCalls = []*pb.ChatToolCall{{Id: "project-" + strconv.Itoa(len(request.ChatMessagePrompts)) + "-" + tool.Name, Name: tool.Name, ArgumentsJson: string(encoded)}}
		reply.StopReason = pb.StopReason_STOP_REASON_FUNCTION_CALL
	}
	if testScenario == "guidance_ls" || testScenario == "guidance_test_alias" {
		command := "ls"
		if testScenario == "guidance_test_alias" {
			command = "python3 ./run_tests.py"
		}
		for _, tool := range request.Tools {
			if tool.Name == "bash" {
				call(tool, map[string]any{"command": command, "workdir": workspace, "description": "Synthetic restricted-command rejection"})
				return
			}
		}
	}
	if testScenario == "guidance_canonical" && !resume && (!globbed || !grepped) {
		name, pattern := "glob", "*.py"
		if globbed {
			name, pattern = "grep", "discounted"
		}
		for _, tool := range request.Tools {
			if tool.Name == name {
				call(tool, journeyPathArgs(tool, workspace, map[string]any{"pattern": pattern}))
				return
			}
		}
	}
	if testScenario == "read_error_retry" {
		// Deliberately request the same missing file on every model call. The
		// real CLI would retry/continue after the tool error without the fence.
		for _, tool := range request.Tools {
			if tool.Name == "read" {
				call(tool, journeyPathArgs(tool, filepath.Join(workspace, "missing-probe.txt"), map[string]any{}))
				return
			}
		}
	}
	for _, tool := range request.Tools {
		if tool.Name == fusion.DelegateTool {
			if lastReport < lastUser {
				call(tool, map[string]any{"task": userTask, "acceptance_criteria": "Complete the project task, run tests and report the verified result"})
			} else {
				reply.DeltaText = final
				if longRecall {
					reply.DeltaText = longRecallText(history.String(), testScenario)
				}
			}
			return
		}
	}
	if longRecall {
		if testScenario == "bounded_long_tool_recall" {
			for _, tool := range request.Tools {
				if tool.Name == "read" {
					call(tool, journeyPathArgs(tool, filepath.Join(workspace, "context/part00.txt"), map[string]any{}))
					return
				}
			}
		}
		reply.DeltaText = longRecallText(history.String(), testScenario)
		return
	}
	if longTask {
		path := ""
		if reads["price_math.py"] == 0 {
			path = "price_math.py"
		} else {
			for index := 0; index < 8; index++ {
				if testScenario == "bounded_long_missing_part" && index == 7 {
					continue
				}
				name := fmt.Sprintf("part%02d.txt", index)
				if reads[name] == 0 {
					path = filepath.Join("context", name)
					break
				}
			}
		}
		if path != "" {
			for _, tool := range request.Tools {
				if tool.Name == "read" {
					call(tool, journeyPathArgs(tool, filepath.Join(workspace, path), map[string]any{}))
					return
				}
			}
		} else if !wrote {
			for _, tool := range request.Tools {
				if tool.Name == "edit" {
					call(tool, journeyPathArgs(tool, filepath.Join(workspace, "price_math.py"), map[string]any{"oldString": "return subtotal + discount", "newString": "return subtotal - discount"}))
					return
				}
			}
		} else if !tested {
			for _, tool := range request.Tools {
				if tool.Name == "bash" {
					call(tool, map[string]any{"command": "python3 run_tests.py", "description": "Test bounded long project"})
					return
				}
			}
		} else if reads["report.json"] == 0 {
			for _, tool := range request.Tools {
				if tool.Name == "read" {
					call(tool, journeyPathArgs(tool, filepath.Join(workspace, "out/report.json"), map[string]any{}))
					return
				}
			}
		} else {
			reply.DeltaText = final
		}
		return
	}
	readPath := ""
	if pressure {
		for index := 0; index < 16; index++ {
			name := "part-" + fmt.Sprintf("%02d", index) + ".txt"
			if reads[name] == 0 {
				readPath = filepath.Join("context", name)
				break
			}
		}
		if readPath == "" {
			reply.DeltaText = "CONTEXT_READY"
			return
		}
	}
	switch {
	case pressure:
	case resume && reads["report.json"] == 0:
		readPath = "out/report.json"
	case resume:
		reply.DeltaText = final
		return
	case reads["requirements.json"] == 0:
		readPath = "requirements.json"
	case reads["price_math.py"] == 0:
		readPath = "price_math.py"
	case mutationTool == "failed_edit" && mutationFailed:
		reply.DeltaText = final // an errored tool cannot be hidden by normal final text
		return
	case !wrote:
		name := mutationTool
		if mutationTool == "noop" || mutationTool == "early_marker_error" {
			name = "write"
		} else if mutationTool == "failed_edit" {
			name = "edit"
		}
		for _, tool := range request.Tools {
			if tool.Name == name {
				args := map[string]any{"content": fixedProjectSource}
				if testScenario == "failed_tests" {
					args["content"] = "# Intentionally unrepaired test fixture\n" + strings.Replace(fixedProjectSource, "subtotal - discount", "subtotal + discount", 1)
				}
				if mutationTool == "noop" {
					args["content"] = strings.Replace(fixedProjectSource, "subtotal - discount", "subtotal + discount", 1)
				} else if name == "edit" {
					old := "return subtotal + discount"
					if mutationTool == "failed_edit" {
						old = "SYNTHETIC_ABSENT_EDIT_TARGET"
					}
					args = map[string]any{"oldString": old, "newString": "return subtotal - discount"}
				}
				call(tool, journeyPathArgs(tool, filepath.Join(workspace, "price_math.py"), args))
				return
			}
		}
		return
	case mutationTool == "noop":
		reply.DeltaText = final // deliberately false success; no source repair or tests
		return
	case testScenario == "tool_limit_12" || testScenario == "tool_limit_13":
		// Actual CLI execution creates unique protected test receipts. The
		// thirteenth proposed tool is another exact permitted test command,
		// so its absence can be proved independently of the response parser.
		target := 12
		if testScenario == "tool_limit_13" {
			target = 13
		}
		if !tested || reads["requirements.json"] >= 2 && reads["report.json"] > 0 && requestedCommands["python3 run_tests.py"] < target-5 {
			for _, tool := range request.Tools {
				if tool.Name == "bash" {
					call(tool, map[string]any{"command": "python3 run_tests.py", "description": "Bounded tool admission fixture"})
					return
				}
			}
		} else if reads["requirements.json"] < 2 {
			readPath = "requirements.json"
		} else if reads["report.json"] == 0 {
			readPath = "out/report.json"
		} else {
			reply.DeltaText = final
			return
		}
	case testScenario != "":

		commands := map[string][]string{
			"guidance_canonical": {"python3 run_tests.py"},
			"canonical":          {"python3 run_tests.py"},
			"whitespace":         {"python3   run_tests.py"},
			"after_extra":        {"python3 run_tests.py", "python3 run_tests.py"},
			"after_forbidden":    {"python3 run_tests.py", "printf SYNTHETIC_EXTRA"},
			"fake_ok":            {"printf 'Ran 1 test\\n\\nOK\\n'"},
			"failed_tests":       {"python3 run_tests.py"},
			"forbidden_equiv":    {"python3 run_tests.py && printf SYNTHETIC_EXTRA"},
		}[testScenario]
		plannedCommands := map[string]int{}
		for _, command := range commands {
			plannedCommands[command]++
			if requestedCommands[command] >= plannedCommands[command] {
				continue
			}
			for _, tool := range request.Tools {
				if tool.Name == "bash" {
					args := map[string]any{"command": command, "description": "Synthetic test-execution evidence scenario"}
					if testScenario == "guidance_canonical" {
						args["workdir"] = workspace
					}
					call(tool, args)
					return
				}
			}
		}
		if reads["requirements.json"] < 2 {
			readPath = "requirements.json"
		} else if reads["report.json"] == 0 && tested {
			readPath = "out/report.json"
		} else {
			reply.DeltaText = final
			return
		}
	case !tested:
		for _, tool := range request.Tools {
			if tool.Name == "bash" {
				call(tool, map[string]any{"command": "python3 run_tests.py", "description": "Run project tests"})
				return
			}
		}
		return
	case reads["requirements.json"] < 2:
		readPath = "requirements.json"
	case reads["report.json"] == 0:
		readPath = "out/report.json"
	default:
		reply.DeltaText = final
		return
	}
	for _, tool := range request.Tools {
		if tool.Name == "read" || tool.Name == "read_file" {
			call(tool, journeyPathArgs(tool, filepath.Join(workspace, readPath), map[string]any{}))
			return
		}
	}
}

// Values come only from actual forwarded model history/tool results, never
// from opening project files or an oracle injecting the expected answer.
func longRecallText(history, scenario string) string {
	facts := map[string]string{}
	pattern := regexp.MustCompile(`\b(part0[0-7])\b[\s"']*[:=][\s"']*([a-f0-9]{32})\b`)
	for _, match := range pattern.FindAllStringSubmatch(history, -1) {
		facts[match[1]] = match[2]
	}
	if scenario == "bounded_long_wrong_fact" {
		facts["part00"] = strings.Repeat("0", 32)
	}
	encoded, _ := json.Marshal(facts)
	return string(encoded)
}

func journeyPathArgs(tool *pb.ChatToolDefinition, path string, args map[string]any) map[string]any {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal([]byte(tool.JsonSchemaString), &schema)
	for _, field := range []string{"filePath", "path", "file_path"} {
		if _, exists := schema.Properties[field]; exists {
			args[field] = path
			break
		}
	}
	return args
}
