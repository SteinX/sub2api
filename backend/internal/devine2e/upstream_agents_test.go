//go:build devine2e

package devine2e

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
)

// The real CLI must execute read itself. Only the model's deterministic tool
// decision is synthetic; no file is read by this native upstream fixture.
func (f *nativeFixture) agentReply(request *pb.GetChatMessageRequest, reply *pb.GetChatMessageResponse) {
	reply.DeltaText, reply.DeltaToolCalls = "E2E_OK", nil
	reply.Usage.BillingModelUid = request.ChatModelUid
	reply.StopReason = pb.StopReason_STOP_REASON_STOP_PATTERN
	lastUser, lastReport, lastRead := -1, -1, -1
	toolNames := map[string]string{}
	for index, prompt := range request.ChatMessagePrompts {
		for _, tool := range prompt.ToolCalls {
			toolNames[tool.Id] = tool.Name
		}
		if prompt.Source == pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_USER && prompt.ToolCallId == "" {
			lastUser = index
		}
		if toolNames[prompt.ToolCallId] == fusion.DelegateTool {
			lastReport = index
		}
		if prompt.ToolCallId != "" && strings.Contains(prompt.Prompt, "E2E_TOOL_RESULT") && !prompt.ToolResultIsError {
			lastRead = index
		}
	}
	f.mu.Lock()
	path, sequence := f.probePath, len(f.chats)
	f.mu.Unlock()
	call := func(name, args, kind string) {
		reply.DeltaText = ""
		reply.DeltaToolCalls = []*pb.ChatToolCall{{Id: "call_agent_" + kind + "_" + strconv.Itoa(sequence), Name: name, ArgumentsJson: args}}
		reply.StopReason = pb.StopReason_STOP_REASON_FUNCTION_CALL
	}
	for _, tool := range request.Tools {
		if tool.Name == fusion.DelegateTool && lastReport < lastUser {
			call(tool.Name, `{"task":"Complete the user's short E2E task; read probe.txt when requested","constraints":"Use only the declared read tool","acceptance_criteria":"Return E2E_OK after checking the task"}`, "delegate")
			return
		}
	}
	if !f.agentRead.Load() || lastRead > lastUser {
		return
	}
	for _, tool := range request.Tools {
		if tool.Name != "read" && tool.Name != "read_file" {
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if json.Unmarshal([]byte(tool.JsonSchemaString), &schema) != nil {
			continue
		}
		for _, field := range []string{"filePath", "path", "file_path"} {
			if _, exists := schema.Properties[field]; !exists {
				continue
			}
			args := map[string]any{field: path}
			if _, exists := schema.Properties["i"]; exists {
				args["i"] = "read the synthetic E2E fixture"
			}
			encoded, _ := json.Marshal(args)
			call(tool.Name, string(encoded), "read")
			return
		}
	}
}

func (f *nativeFixture) verifyAgentNativeContexts(t *testing.T, start int, client, lead, sidekick string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	contexts := map[fusion.Role]map[string]bool{fusion.Lead: {}, fusion.Sidekick: {}}
	calls := map[fusion.Role]int{}
	for _, request := range f.chats[start:] {
		role, model := fusion.Sidekick, sidekick
		for _, tool := range request.Tools {
			if tool.Name == fusion.DelegateTool {
				role, model = fusion.Lead, lead
			}
		}
		require.Equal(t, model, request.ChatModelUid, "native call used the wrong role model")
		require.NotEmpty(t, request.CascadeId)
		contexts[role][fusion.Digest(request.CascadeId)] = true
		calls[role]++
	}
	require.NotEmpty(t, contexts[fusion.Lead])
	require.NotEmpty(t, contexts[fusion.Sidekick])
	for id := range contexts[fusion.Lead] {
		require.False(t, contexts[fusion.Sidekick][id], "Lead and Sidekick shared a native Cascade context")
	}
	writeAgentArtifact(t, "agent-native-contexts-"+client+".json", map[string]any{"role_calls": calls, "role_context_hashes": contexts, "distinct_native_contexts": true, "boundary": "synthetic upstream; actual production serialization"})
}
