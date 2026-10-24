package devin

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestNativeExecutionEvidenceDoesNotStartPublicOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message *pb.GetChatMessageResponse
	}{
		{"generated tokens", &pb.GetChatMessageResponse{DeltaTokens: 1}},
		{"credit cost", &pb.GetChatMessageResponse{CreditCost: 1}},
		{"committed credits", &pb.GetChatMessageResponse{CommittedCreditCost: 1}},
		{"committed ACU", &pb.GetChatMessageResponse{CommittedAcuCost: 0.001}},
		{"redacted thinking", &pb.GetChatMessageResponse{ThinkingRedacted: true}},
		{"terminal generation stop", &pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_MAX_TOKENS}},
		{"partial generation stop", &pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_PARTIAL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []apicompat.ResponsesStreamEvent
			decoder := NewEventDecoder("physical-model", func(e apicompat.ResponsesStreamEvent) error {
				events = append(events, e)
				return nil
			})
			require.NoError(t, decoder.Apply(tc.message))
			require.True(t, decoder.ExecutionObserved, "native execution cannot authorize a zero-execution retry")
			require.False(t, decoder.Started)
			require.Empty(t, events, "accounting evidence must not emit a public Responses lifecycle")
			require.Empty(t, decoder.Response.Output)
			require.Nil(t, decoder.Response.Usage, "credit observations must not invent token usage")
			require.NoError(t, decoder.Apply(&pb.GetChatMessageResponse{}))
			require.True(t, decoder.ExecutionObserved, "a later empty frame cannot erase execution evidence")
			require.Error(t, decoder.Finish(), "execution evidence alone does not make an empty generation successful")
		})
	}
}

func TestNativeExecutionEvidenceRetainsPublicOutputAndZeroUsageContracts(t *testing.T) {
	for _, message := range []*pb.GetChatMessageResponse{
		{DeltaText: "text"},
		{DeltaThinking: "thinking"},
		{DeltaSignature: "signature"},
		{DeltaToolCalls: []*pb.ChatToolCall{{Id: "call", Name: "read", ArgumentsJson: `{}`}}},
	} {
		decoder := NewEventDecoder("physical-model", func(apicompat.ResponsesStreamEvent) error { return nil })
		require.NoError(t, decoder.Apply(message))
		require.True(t, decoder.Started)
		require.True(t, decoder.ExecutionObserved)
	}
	decoder := NewEventDecoder("physical-model", func(apicompat.ResponsesStreamEvent) error { return nil })
	require.NoError(t, decoder.Apply(&pb.GetChatMessageResponse{Usage: &pb.ModelUsageStats{}}))
	require.True(t, decoder.ExecutionObserved, "even zero usage is an upstream accounting result")
	require.NotNil(t, decoder.Response.Usage)
	require.False(t, decoder.Started)

	decoder = NewEventDecoder("physical-model", func(apicompat.ResponsesStreamEvent) error { return nil })
	err := decoder.Apply(&pb.GetChatMessageResponse{Usage: &pb.ModelUsageStats{InputTokens: 1}, ActualModelUid: proto.String("fusion")})
	require.Error(t, err)
	require.True(t, decoder.ExecutionObserved, "route rejection cannot erase already observed usage")
}

func TestNativeIdentityAllocationAloneIsNotExecutionEvidence(t *testing.T) {
	require.False(t, HasExecutionEvidence(nil))
	message := &pb.GetChatMessageResponse{MessageId: "allocated-message", OutputId: "allocated-output", RequestId: "allocated-request", ActualModelUid: proto.String("physical-model")}
	decoder := NewEventDecoder("physical-model", func(apicompat.ResponsesStreamEvent) error { return nil })
	require.NoError(t, decoder.Apply(message))
	require.False(t, decoder.ExecutionObserved)
	require.False(t, decoder.Started)
	require.False(t, HasExecutionEvidence(&pb.GetChatMessageResponse{}))
}
