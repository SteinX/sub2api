package fusion

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/stretchr/testify/require"
)

func queuedToolResponse() *apicompat.ResponsesResponse {
	response := tool("native-read", "read_file", `{"path":"main.go"}`)
	response.Output = append(response.Output, tool("native-edit", "edit_file", `{"path":"main.go"}`).Output...)
	return response
}

func TestFusionQueuedClientToolsPersistSerializeAndBillOnce(t *testing.T) {
	store := newMemoryStore()
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), queuedToolResponse(), textResponse("Sidekick report"), textResponse("reviewed answer")}}
	in := input(testRequest("fix main.go"))
	first, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Len(t, first.Response.Output, 1)
	require.Equal(t, "read_file", first.Response.Output[0].Name)
	require.Len(t, x.calls, 2)
	in.Request = resultRequest(first.Response.Output[0].CallID, "original file")
	in.IdempotencyKey = "first-result"
	second, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Equal(t, WaitingTool, second.State)
	require.Len(t, second.Response.Output, 1)
	require.Equal(t, "edit_file", second.Response.Output[0].Name)
	require.Len(t, x.calls, 2, "queued publication must not invoke a physical model")
	require.Len(t, x.settled, 2)
	require.Zero(t, second.Response.Usage.InputTokens, "the batch usage was already reported and settled")
	replay, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, second.Response.Output[0].CallID, replay.Response.Output[0].CallID)
	require.Len(t, x.calls, 2)
	in.Request = resultRequest(second.Response.Output[0].CallID, "edited file")
	in.IdempotencyKey = "second-result"
	last, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Equal(t, "reviewed answer", outputText(last.Response))
	require.Len(t, x.calls, 4)
	require.Len(t, x.settled, 4)
	require.Equal(t, Sidekick, x.calls[2].Role)
	_, err = devin.BuildRequest(x.calls[2].Request, x.calls[2].LaneScope)
	require.NoError(t, err, "both original call IDs and results must form complete native tool history")
}

func TestFusionQueuedClientToolsRespectBudgetAndCancellation(t *testing.T) {
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), queuedToolResponse()}}
	in := input(testRequest("task"))
	in.Profile.MaxTools = 1
	result, err := New(newMemoryStore()).Advance(context.Background(), in, x)
	requireCode(t, err, "budget_exhausted")
	require.Nil(t, result.Response, "reject an over-budget batch before any client call is published")
	require.Len(t, x.settled, 2)
	store := newMemoryStore()
	e := New(store)
	x = &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), queuedToolResponse()}}
	in = input(testRequest("task"))
	first, err := e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.NoError(t, e.Cancel(context.Background(), in.Scope, in.SessionID))
	in.Request = resultRequest(first.Response.Output[0].CallID, "result after cancellation")
	_, err = New(store).Advance(context.Background(), in, x)
	requireCode(t, err, "cancelled")
	require.Len(t, x.calls, 2)
}

func TestFusionQueuedClientToolOwnershipCannotChangeAcrossRestore(t *testing.T) {
	store := newMemoryStore()
	e := New(store)
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), queuedToolResponse()}}
	in := input(testRequest("task"))
	first, err := e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	s, raw, err := e.Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	s.QueuedTools[0].Role = Lead
	require.NoError(t, e.save(context.Background(), s, &raw))
	in.Request = resultRequest(first.Response.Output[0].CallID, "read result")
	_, err = New(store).Advance(context.Background(), in, x)
	requireCode(t, err, "session_corrupt")
	require.Len(t, x.calls, 2, "a restored queue cannot transfer a tool to another role")
}
