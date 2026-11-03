package fusion

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/stretchr/testify/require"
)

func mixedTools(calls ...*apicompat.ResponsesResponse) *apicompat.ResponsesResponse {
	r := *calls[0]
	r.Output = nil
	for _, call := range calls {
		r.Output = append(r.Output, call.Output...)
	}
	return &r
}

func TestFusionMixedToolsPreserveOrderOwnershipAndBudgetAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name string
		lead []*apicompat.ResponsesResponse
		ids  []string
	}{
		{"delegate first", []*apicompat.ResponsesResponse{delegation("delegate"), tool("lead-read", "read_file", `{}`)}, []string{"native-read", "native-edit", "lead-read"}},
		{"delegate last", []*apicompat.ResponsesResponse{tool("lead-read", "read_file", `{}`), delegation("delegate")}, []string{"lead-read", "native-read", "native-edit"}},
		{"delegate between client tools", []*apicompat.ResponsesResponse{tool("lead-read", "read_file", `{}`), delegation("delegate"), tool("lead-edit", "edit_file", `{}`)}, []string{"lead-read", "native-read", "native-edit", "lead-edit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{mixedTools(tc.lead...), queuedToolResponse(), textResponse("Sidekick report"), textResponse("reviewed answer")}}
			in := input(testRequest("fix the fixture"))
			in.IdempotencyKey = "initial"
			response, err := New(store).Advance(context.Background(), in, x)
			require.NoError(t, err)
			var started time.Time
			for _, id := range tc.ids {
				require.Equal(t, WaitingTool, response.State)
				require.Len(t, response.Response.Output, 1)
				call := response.Response.Output[0]
				require.NotEqual(t, DelegateTool, call.Name, "delegation stays private")
				s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
				require.NoError(t, err)
				require.Equal(t, id, s.Pending.InternalID)
				wantRole := Lead
				if id == "native-read" || id == "native-edit" {
					wantRole = Sidekick
				}
				require.Equal(t, wantRole, s.Pending.Role)
				require.Equal(t, 1, s.Turn)
				if started.IsZero() {
					started = s.TurnStarted
				}
				require.Equal(t, started, s.TurnStarted)
				before, charged := len(x.calls), x.charged
				replay, err := New(store).Advance(context.Background(), in, x)
				require.NoError(t, err)
				require.True(t, replay.Replayed)
				require.Equal(t, call.CallID, replay.Response.Output[0].CallID)
				require.Len(t, x.calls, before)
				require.Equal(t, charged, x.charged)
				in.Request = resultRequest(call.CallID, "result-"+id)
				in.IdempotencyKey = "result-" + id
				response, err = New(store).Advance(context.Background(), in, x)
				require.NoError(t, err)
			}
			require.Equal(t, Completed, response.State)
			require.Equal(t, "reviewed answer", outputText(response.Response))
			require.Len(t, x.calls, 4)
			require.Len(t, x.settled, 4)
			require.InDelta(t, 2.4, x.charged, 1e-9)
			s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			require.Nil(t, s.Pending)
			require.Empty(t, s.QueuedTools)
			require.Equal(t, 1, s.Turn)
			require.Equal(t, started, s.TurnStarted)
			require.Equal(t, 4, s.TurnRequests)
			require.Equal(t, len(tc.ids), s.TurnTools)
			require.Equal(t, 1, s.TurnHandoffs)
			require.InDelta(t, x.charged, s.TurnCostUSD, 1e-9)
			var final []apicompat.ResponsesInputItem
			require.NoError(t, json.Unmarshal(x.calls[3].Request.Input, &final))
			var got []string
			for _, item := range final {
				if item.Type == "function_call_output" {
					got = append(got, item.CallID)
				}
			}
			var want []string
			for _, call := range tc.lead {
				want = append(want, call.Output[0].CallID)
			}
			require.Equal(t, want, got, "each original Lead call gets its own result, in emitted order")
			for _, call := range x.calls {
				_, err := devin.BuildRequest(call.Request, call.LaneScope)
				require.NoError(t, err)
				require.NotContains(t, string(call.Request.Input), "call_fusion_")
			}
		})
	}
}

func TestFusionMixedToolsMultipleDelegationsAreSequential(t *testing.T) {
	store := newMemoryStore()
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{mixedTools(delegation("first"), delegation("second")), textResponse("first report"), textResponse("second report"), textResponse("reviewed both")}}
	in := input(testRequest("two bounded briefs"))
	response, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Equal(t, Completed, response.State)
	require.Len(t, x.calls, 4)
	require.Equal(t, []Role{Lead, Sidekick, Sidekick, Lead}, []Role{x.calls[0].Role, x.calls[1].Role, x.calls[2].Role, x.calls[3].Role})
	s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Len(t, s.Handoffs, 2)
	require.Equal(t, "first", s.Handoffs[0].CallID)
	require.Equal(t, "first report", s.Handoffs[0].Report)
	require.Equal(t, "second", s.Handoffs[1].CallID)
	require.Equal(t, "second report", s.Handoffs[1].Report)
	require.Contains(t, string(x.calls[2].Request.Input), "first report")
	require.Len(t, x.settled, 4)
	for _, call := range x.calls {
		_, err := devin.BuildRequest(call.Request, call.LaneScope)
		require.NoError(t, err)
	}
}

func TestFusionMixedToolsValidateEntireBatchBeforePublishing(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   func(*Input)
		lead *apicompat.ResponsesResponse
		code string
	}{
		{"invalid later brief", nil, mixedTools(tool("read", "read_file", `{}`), tool("d", DelegateTool, `{"task":"missing acceptance"}`)), "brief_invalid"},
		{"client budget", func(i *Input) { i.Profile.MaxTools = 1 }, mixedTools(tool("read", "read_file", `{}`), delegation("d"), tool("edit", "edit_file", `{}`)), "budget_exhausted"},
		{"handoff budget", func(i *Input) { i.Profile.MaxHandoffs = 1 }, mixedTools(tool("read", "read_file", `{}`), delegation("d1"), delegation("d2")), "budget_exhausted"},
		{"duplicate call id", nil, mixedTools(tool("same", "read_file", `{}`), delegation("same")), "tool_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{tc.lead}}
			in := input(testRequest("task"))
			if tc.in != nil {
				tc.in(&in)
			}
			response, err := New(newMemoryStore()).Advance(context.Background(), in, x)
			requireCode(t, err, tc.code)
			require.Nil(t, response.Response)
			require.Len(t, x.calls, 1)
			require.Len(t, x.settled, 1, "known Lead usage remains billable even for an invalid batch")
		})
	}
}

func TestFusionMixedToolsCancellationNeverStartsQueuedDelegation(t *testing.T) {
	store := newMemoryStore()
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{mixedTools(tool("read", "read_file", `{}`), delegation("d"))}}
	in := input(testRequest("task"))
	e := New(store)
	response, err := e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.NoError(t, e.Cancel(context.Background(), in.Scope, in.SessionID))
	in.Request = resultRequest(response.Response.Output[0].CallID, "late result")
	_, err = New(store).Advance(context.Background(), in, x)
	requireCode(t, err, "cancelled")
	require.Len(t, x.calls, 1)
	require.Len(t, x.settled, 1)
}

func TestFusionMixedToolsKeepWholeTurnCostRequestAndTimeLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit func(*Input)
		calls int
	}{
		{"cost", func(i *Input) { i.Profile.MaxCostUSD = 1.5 }, 1},
		{"requests", func(i *Input) { i.Profile.MaxRequests = 3 }, 3},
		{"deadline", func(i *Input) {}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			clock := time.Now()
			engine := func() *Engine {
				e := New(store)
				e.Now = func() time.Time { return clock }
				return e
			}
			x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{mixedTools(tool("lead-read", "read_file", `{}`), delegation("d")), queuedToolResponse(), textResponse("worker report"), textResponse("done")}}
			in := input(testRequest("bounded task"))
			tc.limit(&in)
			response, err := engine().Advance(context.Background(), in, x)
			require.NoError(t, err)
			s, _, err := engine().Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			started := s.TurnStarted
			if tc.name == "deadline" {
				clock = clock.Add(901 * time.Second)
			}
			for n := 0; n < 5 && err == nil; n++ {
				require.Equal(t, WaitingTool, response.State)
				in.Request = resultRequest(response.Response.Output[0].CallID, "tool evidence")
				in.IdempotencyKey = "result-" + response.Response.Output[0].CallID
				response, err = engine().Advance(context.Background(), in, x)
			}
			requireCode(t, err, "budget_exhausted")
			require.Len(t, x.calls, tc.calls)
			require.Len(t, x.settled, tc.calls)
			s, _, loadErr := engine().Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, loadErr)
			require.Equal(t, 1, s.Turn)
			require.Equal(t, started, s.TurnStarted)
			require.Equal(t, tc.calls, s.TurnRequests)
			require.InDelta(t, x.charged, s.TurnCostUSD, 1e-9)
		})
	}
}

func TestFusionMixedToolsRecoverBillingAndQueueSnapshotsWithoutReexecution(t *testing.T) {
	for _, stage := range []string{"billing", "after_lead_apply", "after_sidekick_report", "after_publication"} {
		t.Run(stage, func(t *testing.T) {
			store := newMemoryStore()
			x := &scriptExecutor{t: t, failSettle: stage == "billing", responses: []*apicompat.ResponsesResponse{mixedTools(delegation("d"), tool("lead-read", "read_file", `{}`)), queuedToolResponse(), textResponse("worker report"), textResponse("done")}}
			fired := stage == "billing"
			if stage != "billing" {
				store.failCAS = func(raw []byte) bool {
					var s Session
					if json.Unmarshal(raw, &s) != nil || len(s.Attempts) == 0 {
						return false
					}
					last := s.Attempts[len(s.Attempts)-1]
					match := stage == "after_lead_apply" && last.Applied && s.Role == Sidekick && len(s.QueuedTools) > 0 ||
						stage == "after_sidekick_report" && last.Applied && s.Role == Lead && s.Pending != nil && s.Pending.InternalID == "lead-read" ||
						stage == "after_publication" && s.ActiveRequest == -1
					if match {
						fired = true
					}
					return match
				}
			}
			in := input(testRequest("crash-safe mixed batch"))
			in.IdempotencyKey = "initial"
			recovered := 0
			for n := 0; n < 6; n++ {
				response, err := New(store).Advance(context.Background(), in, x)
				if err != nil {
					require.True(t, fired)
					recovered++
					response, err = New(store).Advance(context.Background(), in, x)
				}
				require.NoError(t, err)
				if response.State == Completed {
					break
				}
				in.Request = resultRequest(response.Response.Output[0].CallID, "tool evidence")
				in.IdempotencyKey = "result-" + response.Response.Output[0].CallID
			}
			require.Equal(t, 1, recovered)
			require.Len(t, x.calls, 4)
			require.Len(t, x.settled, 4)
			require.InDelta(t, 2.4, x.charged, 1e-9)
			s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			require.Equal(t, Completed, s.State)
			require.Len(t, s.Handoffs, 1)
			require.Nil(t, s.Pending)
			require.Empty(t, s.QueuedTools)
		})
	}
}

func TestFusionMixedToolsCancellationAfterReportStopsRemainingDelegations(t *testing.T) {
	store := newMemoryStore()
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{mixedTools(delegation("first"), tool("lead-read", "read_file", `{}`), delegation("second")), textResponse("first report")}}
	in := input(testRequest("task"))
	e := New(store)
	response, err := e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Equal(t, "read_file", response.Response.Output[0].Name)
	require.NoError(t, e.Cancel(context.Background(), in.Scope, in.SessionID))
	in.Request = resultRequest(response.Response.Output[0].CallID, "late result")
	_, err = New(store).Advance(context.Background(), in, x)
	requireCode(t, err, "cancelled")
	require.Len(t, x.calls, 2)
	require.Len(t, x.settled, 2)
}

func TestFusionMixedToolsDeadlineStopsQueuedClientPublication(t *testing.T) {
	store := newMemoryStore()
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{mixedTools(tool("read", "read_file", `{}`), tool("edit", "edit_file", `{}`), delegation("d"))}}
	in := input(testRequest("task"))
	e := New(store)
	first, err := e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	s, _, err := e.Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	e.Now = func() time.Time { return s.TurnStarted.Add(901 * time.Second) }
	in.Request = resultRequest(first.Response.Output[0].CallID, "read result")
	response, err := e.Advance(context.Background(), in, x)
	requireCode(t, err, "budget_exhausted")
	require.Nil(t, response.Response, "expired task cannot publish the next side-effecting call")
	require.Len(t, x.calls, 1)
}

func TestFusionMixedToolsSidekickCannotQueueDelegation(t *testing.T) {
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("lead"), mixedTools(tool("read", "read_file", `{}`), delegation("forbidden"))}}
	response, err := New(newMemoryStore()).Advance(context.Background(), input(testRequest("task")), x)
	requireCode(t, err, "tool_invalid")
	require.Nil(t, response.Response)
	require.Len(t, x.calls, 2)
	require.Len(t, x.settled, 2)
}
