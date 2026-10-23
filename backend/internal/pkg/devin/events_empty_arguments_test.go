package devin

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
)

func TestNativeEmptyArgumentsOnlyNormalizeDeclaredOptionalObjects(t *testing.T) {
	for _, schema := range []string{`{"type":"object","properties":{},"additionalProperties":false}`, `{"type":"object","properties":{"optional":{"type":"string"}},"required":[]}`} {
		var events []apicompat.ResponsesStreamEvent
		d := NewEventDecoder("physical-model", func(event apicompat.ResponsesStreamEvent) error { events = append(events, event); return nil }, &pb.ChatToolDefinition{Name: "run_tests", JsonSchemaString: schema})
		require.NoError(t, d.Apply(&pb.GetChatMessageResponse{DeltaToolCalls: []*pb.ChatToolCall{{Id: "native-call", Name: "run_tests"}}}))
		require.NoError(t, d.Apply(&pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_FUNCTION_CALL, Usage: &pb.ModelUsageStats{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 40}}))
		require.NoError(t, d.Finish())
		require.Equal(t, `{}`, d.Response.Output[0].Arguments)
		require.Equal(t, "completed", d.Response.Status)
		require.Equal(t, 40, d.Response.Usage.InputTokensDetails.CachedTokens)
		var delta strings.Builder
		for _, event := range events {
			if event.Type == "response.function_call_arguments.delta" {
				_, _ = delta.WriteString(event.Delta)
			}
			if event.Type == "response.function_call_arguments.done" {
				require.Equal(t, `{}`, event.Arguments)
			}
		}
		require.Equal(t, `{}`, delta.String())
	}
	for _, test := range []struct {
		schema, name, arguments string
		stop                    pb.StopReason
	}{
		{`{"type":"object","required":["path"]}`, "run_tests", "", pb.StopReason_STOP_REASON_FUNCTION_CALL},
		{`{"type":"object","minProperties":1}`, "run_tests", "", pb.StopReason_STOP_REASON_FUNCTION_CALL},
		{`{"type":"object","allOf":[]}`, "run_tests", "", pb.StopReason_STOP_REASON_FUNCTION_CALL},
		{`{"type":"object","additionalProperties":null}`, "run_tests", "", pb.StopReason_STOP_REASON_FUNCTION_CALL},
		{`{"type":"array"}`, "run_tests", "", pb.StopReason_STOP_REASON_FUNCTION_CALL},
		{`{"type":"object"}`, "undeclared", "", pb.StopReason_STOP_REASON_FUNCTION_CALL},
		{`{"type":"object"}`, "run_tests", `{`, pb.StopReason_STOP_REASON_FUNCTION_CALL},
		{`{"type":"object"}`, "run_tests", "", pb.StopReason_STOP_REASON_MAX_TOKENS},
		{`{"type":"object"}`, "run_tests", "", pb.StopReason_STOP_REASON_UNSPECIFIED},
	} {
		d := NewEventDecoder("physical-model", func(apicompat.ResponsesStreamEvent) error { return nil }, &pb.ChatToolDefinition{Name: "run_tests", JsonSchemaString: test.schema})
		require.NoError(t, d.Apply(&pb.GetChatMessageResponse{DeltaToolCalls: []*pb.ChatToolCall{{Id: "call", Name: test.name, ArgumentsJson: test.arguments}}, StopReason: test.stop}))
		require.ErrorContains(t, d.Finish(), "incomplete tool arguments")
	}
}
