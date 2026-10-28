//go:build devine2e

package devine2e

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
)

func TestJourneyToolLimitLongScenarioDoesNotInheritEarlierErrorTerminal(t *testing.T) {
	workspace := "/synthetic/project"
	request := &pb.GetChatMessageRequest{ChatModelUid: fixtureSidekick, ChatMessagePrompts: []*pb.ChatMessagePrompt{{Source: pb.ChatMessageSource_CHAT_MESSAGE_SOURCE_USER, Prompt: "BOUNDED_LONG_PROJECT: finish the bounded project"}}}
	add := func(name, path string) {
		id := fmt.Sprintf("call-%d", len(request.ChatMessagePrompts))
		arguments, _ := json.Marshal(map[string]any{"filePath": filepath.Join(workspace, path)})
		request.ChatMessagePrompts = append(request.ChatMessagePrompts, &pb.ChatMessagePrompt{ToolCalls: []*pb.ChatToolCall{{Id: id, Name: name, ArgumentsJson: string(arguments)}}}, &pb.ChatMessagePrompt{ToolCallId: id, Prompt: "completed"})
	}
	add("read", "price_math.py")
	for i := 0; i < 8; i++ {
		add("read", fmt.Sprintf("context/part%02d.txt", i))
	}
	add("edit", "price_math.py")
	add("bash", "")
	// This fixture models completed tests, not an arbitrary Bash file read.
	request.ChatMessagePrompts[len(request.ChatMessagePrompts)-1].Prompt = "SUB2API_TEST_RECEIPT_V1:{}"
	add("read", "out/report.json")
	fixture := &projectJourney{workspace: workspace, mutationTool: "early_marker_error", testScenario: "bounded_long_complete"}
	previous := &pb.GetChatMessageResponse{Usage: &pb.ModelUsageStats{}}
	fixture.reply(request, previous)
	require.Equal(t, pb.StopReason_STOP_REASON_MAX_TOKENS, previous.StopReason, "prove stale negative state truncates an otherwise complete long reply")
	restore := fixture.boundedLongScenario(workspace, "complete")
	current := &pb.GetChatMessageResponse{Usage: &pb.ModelUsageStats{}}
	fixture.reply(request, current)
	require.Equal(t, pb.StopReason_STOP_REASON_STOP_PATTERN, current.StopReason)
	require.NotEmpty(t, current.DeltaText)
	require.Empty(t, current.DeltaToolCalls)
	restore()
	require.Equal(t, "early_marker_error", fixture.mutationTool, "isolation restores the caller's prior fixture state")
}
