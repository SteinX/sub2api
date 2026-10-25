//go:build devine2e

package devine2e

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

// This is a scripted MODEL, not a tool executor. It never opens project files,
// runs commands, creates output artifacts or decides whether the journey passed.
type projectJourney struct {
	mu        sync.Mutex
	workspace string
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
	j.mu.Unlock()
	reply.DeltaText, reply.DeltaToolCalls = "PROJECT_FAILED", nil
	reply.Usage.BillingModelUid = request.ChatModelUid
	reply.StopReason = pb.StopReason_STOP_REASON_STOP_PATTERN
	lastUser, lastReport := -1, -1
	userTask := ""
	for i, prompt := range request.ChatMessagePrompts {
		if prompt.Source == pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_USER && prompt.ToolCallId == "" {
			lastUser, userTask = i, prompt.Prompt
		}
	}
	resume := strings.Contains(userTask, "Resume this project session")
	final := "PROJECT_COMPLETE"
	if resume {
		final = "PROJECT_REVIEWED"
	}
	type priorCall struct{ name, path string }
	calls := map[string]priorCall{}
	reads := map[string]int{}
	wrote, tested := false, false
	for i, prompt := range request.ChatMessagePrompts {
		for _, call := range prompt.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(call.ArgumentsJson), &args)
			path := ""
			for _, field := range []string{"filePath", "path", "file_path"} {
				if value, ok := args[field].(string); ok {
					path = value
				}
			}
			calls[call.Id] = priorCall{call.Name, path}
		}
		call := calls[prompt.ToolCallId]
		if call.name == fusion.DelegateTool {
			lastReport = i
		}
		if i <= lastUser || prompt.ToolCallId == "" || prompt.ToolResultIsError {
			continue
		}
		switch call.name {
		case "read", "read_file":
			reads[filepath.Base(call.path)]++
		case "write":
			wrote = true
		case "bash":
			tested = true
		}
	}
	call := func(tool *pb.ChatToolDefinition, args map[string]any) {
		args["i"] = "Complete the requested synthetic project task"
		encoded, _ := json.Marshal(args)
		reply.DeltaText = ""
		reply.DeltaToolCalls = []*pb.ChatToolCall{{Id: "project-" + strconv.Itoa(len(request.ChatMessagePrompts)) + "-" + tool.Name, Name: tool.Name, ArgumentsJson: string(encoded)}}
		reply.StopReason = pb.StopReason_STOP_REASON_FUNCTION_CALL
	}
	for _, tool := range request.Tools {
		if tool.Name == fusion.DelegateTool {
			if lastReport < lastUser {
				call(tool, map[string]any{"task": userTask, "acceptance_criteria": "Complete the project task, run tests and report the verified result"})
			} else {
				reply.DeltaText = final
			}
			return
		}
	}
	readPath := ""
	switch {
	case resume && reads["report.json"] == 0:
		readPath = "out/report.json"
	case resume:
		reply.DeltaText = final
		return
	case reads["requirements.json"] == 0:
		readPath = "requirements.json"
	case reads["price_math.py"] == 0:
		readPath = "price_math.py"
	case !wrote:
		for _, tool := range request.Tools {
			if tool.Name == "write" {
				call(tool, journeyPathArgs(tool, filepath.Join(workspace, "price_math.py"), map[string]any{"content": fixedProjectSource}))
				return
			}
		}
		return
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
