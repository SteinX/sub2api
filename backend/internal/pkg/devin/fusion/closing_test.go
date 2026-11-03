package fusion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/stretchr/testify/require"
)

// Respond to the actual native request's available tools, not the private
// closing state. All progress, reports and reviews remain physical calls.
type closingExecutor struct {
	t                 *testing.T
	calls             []Call
	settled           map[string]float64
	leads, workers    int
	summaries         map[Role]int
	hardSummary       bool
	violate           bool
	leadCloseEstimate float64
	failReportSettle  bool
}

func (x *closingExecutor) Estimate(_ context.Context, c Call) (float64, error) {
	if x.leadCloseEstimate > 0 && c.Role == Lead && string(c.Request.ToolChoice) == `"none"` {
		return x.leadCloseEstimate, nil
	}
	return .01, nil
}
func (x *closingExecutor) MeasureContext(r *apicompat.ResponsesRequest) (int, int, error) {
	b, _ := json.Marshal(r)
	role := Lead
	if r.Model == "worker-real" {
		role = Sidekick
	}
	if x.hardSummary && string(r.ToolChoice) == `"none"` && strings.Contains(string(r.Input), "Gateway request-budget notice") && x.summaries[role] == 0 {
		return 50000, 40000, nil
	}
	return len(b), 40000, nil
}
func (x *closingExecutor) Execute(_ context.Context, c Call) (Outcome, error) {
	x.calls = append(x.calls, c)
	wire, err := devin.BuildRequest(c.Request, c.LaneScope)
	require.NoError(x.t, err)
	if c.Purpose == Compaction {
		if x.summaries == nil {
			x.summaries = map[Role]int{}
		}
		x.summaries[c.Role]++
		return Outcome{UsageFinal: true, Response: summaryResponse()}, nil
	}
	if string(c.Request.ToolChoice) == `"none"` {
		require.Empty(x.t, c.Request.Tools)
		require.Equal(x.t, "none", wire.ToolChoice.GetOptionName())
		if x.violate {
			return Outcome{UsageFinal: true, Response: tool("forbidden-new-work", "read_file", `{}`)}, nil
		}
		if c.Role == Sidekick {
			return Outcome{UsageFinal: true, Response: textResponse("Partial Sidekick report: evidence retained; implementation and tests are unfinished")}, nil
		}
		require.Contains(x.t, string(c.Request.Input), "Partial Sidekick report")
		return Outcome{UsageFinal: true, Response: textResponse("Reviewed actual Sidekick evidence: work remains unfinished; no passing tests claimed")}, nil
	}
	if c.Role == Lead {
		x.leads++
		if x.leads == 4 {
			return Outcome{UsageFinal: true, Response: delegation("original-brief")}, nil
		}
		return Outcome{UsageFinal: true, Response: tool(fmt.Sprintf("lead-%d", x.leads), "read_file", `{}`)}, nil
	}
	x.workers++
	return Outcome{UsageFinal: true, Response: tool(fmt.Sprintf("worker-%d", x.workers), "read_file", `{}`)}, nil
}
func (x *closingExecutor) Settle(_ context.Context, a Attempt) (float64, error) {
	if x.failReportSettle && a.ClosingPhase == closingReport {
		x.failReportSettle = false
		return 0, errors.New("report settlement interrupted")
	}
	if x.settled == nil {
		x.settled = map[string]float64{}
	}
	cost := 0.0
	if a.Role == Lead {
		cost = .01
	}
	x.settled[a.ID] = cost
	return cost, nil
}

func TestFusionClosingRecoveryKeepsReportsAndUniqueSettlements(t *testing.T) {
	for _, stage := range []string{"report_settlement", "report_snapshot"} {
		t.Run(stage, func(t *testing.T) {
			store := newMemoryStore()
			x := &closingExecutor{t: t, failReportSettle: stage == "report_settlement"}
			if stage == "report_snapshot" {
				store.failCAS = func(raw []byte) bool {
					var s Session
					return json.Unmarshal(raw, &s) == nil && s.isClosing() && s.Role == Lead && len(s.Handoffs) > 0 && s.Handoffs[0].Report != "" && len(s.Attempts) > 0 && s.Attempts[len(s.Attempts)-1].Applied
				}
			}
			in := input(testRequest("task"))
			in.ClosingPolicy = ClosingReserveV1
			in.Profile = DefaultProfile("fusion-test", "lead-real", "worker-real")
			interrupted := false
			for n := 0; n < 40; n++ {
				out, err := New(store).Advance(context.Background(), in, x)
				if err != nil {
					require.False(t, interrupted)
					interrupted = true
					require.Len(t, x.calls, 17)
					out, err = New(store).Advance(context.Background(), in, x)
				}
				require.NoError(t, err)
				if out.State == Completed {
					break
				}
				in.Request = resultRequest(out.Response.Output[0].CallID, fmt.Sprintf("result-%d", n))
				in.IdempotencyKey = fmt.Sprintf("result-%d", n)
			}
			require.True(t, interrupted)
			s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			require.Equal(t, Completed, s.State)
			require.Equal(t, 1, s.Turn)
			require.Len(t, x.calls, 18)
			require.Len(t, x.settled, 18)
			require.Contains(t, s.Handoffs[0].Report, "Partial Sidekick report")
			require.Zero(t, s.AuxReservedUSD)
		})
	}
}

func TestFusionClosingCannotSpendPastFiveDollarGateToReview(t *testing.T) {
	store := newMemoryStore()
	x := &closingExecutor{t: t, leadCloseEstimate: 5}
	in := input(testRequest("task"))
	in.ClosingPolicy = ClosingReserveV1
	in.Profile = DefaultProfile("fusion-test", "lead-real", "worker-real")
	for n := 0; n < 30; n++ {
		out, err := New(store).Advance(context.Background(), in, x)
		if err != nil {
			requireCode(t, err, "budget_exhausted")
			break
		}
		in.Request = resultRequest(out.Response.Output[0].CallID, "result")
	}
	s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, 5.0, s.Profile.MaxCostUSD)
	require.Equal(t, BudgetExhausted, s.State)
	require.NotEmpty(t, s.Handoffs[0].Report)
	require.Len(t, x.calls, 17)
	require.Len(t, x.settled, 17)
	require.Nil(t, s.Requests[len(s.Requests)-1].Response)
}

func TestFusionClosingToolAcceptanceTraceFollowsCASAndDoesNotLeakContent(t *testing.T) {
	store := newMemoryStore()
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), tool("private-native-id", "read_file", `{"path":"private-source-path"}`), textResponse("actual report"), textResponse("review")}}
	in := input(testRequest("private-user-text"))
	in.ClosingPolicy = ClosingReserveV1
	e := New(store)
	var events []WorkflowEvent
	e.WorkflowObserver = func(event WorkflowEvent) { events = append(events, event) }
	out, err := e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	in.Request = resultRequest(out.Response.Output[0].CallID, "private-secret-output")
	store.failCAS = func(raw []byte) bool {
		var s Session
		return json.Unmarshal(raw, &s) == nil && len(s.Requests) > 1 && s.Requests[len(s.Requests)-1].ToolResult != nil
	}
	_, err = e.Advance(context.Background(), in, x)
	require.Error(t, err)
	accepted := 0
	for _, event := range events {
		if event.Stage == "tool_result_accepted" {
			accepted++
		}
	}
	require.Zero(t, accepted)
	require.Len(t, x.calls, 2)
	_, err = e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	for _, event := range events {
		if event.Stage == "tool_result_accepted" {
			accepted++
			require.Equal(t, len("private-secret-output"), event.ResultBytes)
		}
	}
	require.Equal(t, 1, accepted)
	b, _ := json.Marshal(events)
	for _, secret := range []string{"private-user-text", "private-native-id", "private-source-path", "private-secret-output"} {
		require.NotContains(t, string(b), secret)
	}
	s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, out.Response.Output[0].CallID, s.Requests[1].ToolResult.PublicID)
	require.Equal(t, "private-native-id", s.Requests[1].ToolResult.InternalID)
	require.Equal(t, s.Attempts[1].ID, s.Requests[1].ToolResult.SourceAttemptID)
}

func TestFusionClosingDefault20PreservesReportsBudgetAndNativeHistory(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(fmt.Sprintf("hard_summary_%t", hard), func(t *testing.T) {
			store := newMemoryStore()
			x := &closingExecutor{t: t, hardSummary: hard}
			in := input(testRequest("Investigate and report the actual unfinished work"))
			in.Profile = DefaultProfile("fusion-test", "lead-real", "worker-real")
			in.Profile.MaxContextBytes = 40000
			in.ClosingPolicy = ClosingReserveV1
			var started time.Time
			var out *Result
			for n := 0; n < 40; n++ {
				e := New(store)
				var err error
				out, err = e.Advance(context.Background(), in, x)
				require.NoError(t, err)
				s, _, err := e.Load(context.Background(), in.Scope, in.SessionID)
				require.NoError(t, err)
				if started.IsZero() {
					started = s.TurnStarted
				}
				require.Equal(t, started, s.TurnStarted)
				require.Equal(t, 1, s.Turn)
				require.Equal(t, 20, s.Profile.MaxRequests)
				require.Equal(t, 5.0, s.Profile.MaxCostUSD)
				require.Equal(t, 900, s.Profile.MaxWallSeconds)
				before := len(x.calls)
				replay, err := New(store).Advance(context.Background(), in, x)
				require.NoError(t, err)
				require.True(t, replay.Replayed)
				require.Equal(t, out.Response.ID, replay.Response.ID)
				require.Len(t, x.calls, before)
				if out.State == Completed {
					break
				}
				require.Equal(t, WaitingTool, out.State)
				in.Request = resultRequest(out.Response.Output[0].CallID, fmt.Sprintf("private result %d:", n)+strings.Repeat("x", 2000))
				in.IdempotencyKey = fmt.Sprintf("result-%d", n)
				in.ClosingPolicy = "" // Omitting on continuation preserves the opt-in.
			}
			s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			require.Equal(t, Completed, s.State)
			require.Equal(t, BudgetClosed, out.Outcome)
			require.Contains(t, outputText(out.Response), budgetCloseNotice)
			require.Contains(t, outputText(out.Response), "unfinished")
			require.Equal(t, 16, s.Closing.RequestsAtStart)
			require.Equal(t, 12, x.workers)
			require.Contains(t, s.Handoffs[0].Report, "Partial Sidekick report")
			require.Contains(t, string(x.calls[len(x.calls)-1].Request.Input), s.Handoffs[0].Report)
			wantCalls := 18
			if hard {
				wantCalls = 20
				require.Equal(t, map[Role]int{Lead: 1, Sidekick: 1}, x.summaries)
			}
			require.Equal(t, wantCalls, s.TurnRequests)
			require.Len(t, x.calls, wantCalls)
			require.Len(t, x.settled, wantCalls)
			require.Zero(t, s.AuxReservedUSD)
			require.Less(t, s.TurnCostUSD, 5.0)
			for _, r := range s.Requests {
				if r.ToolDispatch != nil {
					require.NotEmpty(t, r.ToolDispatch.SourceAttemptID)
				}
				if r.ToolResult != nil {
					require.Positive(t, r.ToolResult.ResultBytes)
					require.NotEmpty(t, r.ToolResult.SourceAttemptID)
				}
			}
		})
	}
}

func TestFusionClosingOptInPinnedAndLegacyScopeUnchanged(t *testing.T) {
	defaults := DefaultProfile("fusion-test", "lead-real", "worker-real")
	// Independently checked with Load from the actual stx.4 source tree.
	require.Equal(t, "c135683c67160e0dd8e1783e4c928c57368d54f0761e08c2787a0856364ac841", defaults.Revision())
	defaults.ClosingPolicy = ClosingReserveV1
	require.Equal(t, "ffed20844c21524b05c0c30296a666d7297587b508f445a6f2ed21bc1ced1fa8", defaults.Revision())
	p := testProfile()
	// Omitting the policy must preserve the old serialized profile shape.
	legacyBytes, _ := json.Marshal(p)
	require.NotContains(t, string(legacyBytes), "closing_policy")
	store := newMemoryStore()
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), tool("w", "read_file", `{}`)}}
	in := input(testRequest("task"))
	out, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, p.Revision(), s.ProfileRevision)
	require.Nil(t, s.Closing)
	require.Empty(t, s.Profile.ClosingPolicy)
	for _, r := range s.Requests {
		require.Nil(t, r.ToolDispatch)
		require.Nil(t, r.ToolResult)
	}
	in.Request = resultRequest(out.Response.Output[0].CallID, "result")
	in.ClosingPolicy = ClosingReserveV1
	_, err = New(store).Advance(context.Background(), in, x)
	requireCode(t, err, "closing_policy_conflict")
	require.Len(t, x.calls, 2)
	in.Scope = Scope(1, 99)
	in.Request = testRequest("a new key/session may independently opt in")
	in.SessionID = "other-key-session"
	y := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d2"), tool("w2", "read_file", `{}`)}}
	_, err = New(store).Advance(context.Background(), in, y)
	require.NoError(t, err)
	other, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, ClosingReserveV1, other.Profile.ClosingPolicy)
	require.NotEqual(t, p.Revision(), other.ProfileRevision)
	require.Empty(t, p.ClosingPolicy)
}

func TestFusionClosingUnknownStoredPolicyCannotSilentlyResume(t *testing.T) {
	store := newMemoryStore()
	p := testProfile()
	p.ClosingPolicy = "future-unsupported-version"
	s := Session{ID: "unknown-policy", Scope: Scope(1, 2), Profile: p, ProfileRevision: p.Revision(), ExpiresAt: time.Now().Add(time.Hour)}
	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.NoError(t, store.Set(context.Background(), sessionKey(s.Scope, s.ID), b, time.Hour))
	_, _, err = New(store).Load(context.Background(), s.Scope, s.ID)
	requireCode(t, err, "session_corrupt")
}

func TestFusionClosingAccountsPreparedSummaryOnceAndSkipsOptionalWork(t *testing.T) {
	e, s, raw, base := summarySession(t, Sidekick)
	s.Profile.MaxRequests = 20
	s.Profile.ClosingPolicy = ClosingReserveV1
	s.ProfileRevision = s.Profile.Revision()
	s.TurnRequests = 15
	require.NoError(t, e.save(context.Background(), s, &raw))
	x := &summaryExecutor{response: summaryResponse(), started: make(chan struct{}), finish: make(chan struct{})}
	ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, true)
	require.NoError(t, err)
	require.True(t, ok)
	select {
	case <-x.started:
	case <-time.After(5 * time.Second):
		t.Fatal("summary did not start")
	}
	started, err := e.updateClosing(context.Background(), s, &raw)
	require.NoError(t, err)
	require.True(t, started)
	require.Equal(t, 16, s.TurnRequests)
	close(x.finish)
	require.Eventually(t, func() bool {
		r, _, err := e.readCompaction(context.Background(), s, s.roleContext(Sidekick).Job)
		return err == nil && r.Attempt.Settled && r.Job.State == "ready"
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, e.collectCompactions(context.Background(), s, &raw, x))
	for range 2 {
		ok, err = e.prepareCompaction(context.Background(), s, &raw, base, x, true)
		require.NoError(t, err)
		require.False(t, ok)
		require.NoError(t, e.collectCompactions(context.Background(), s, &raw, x))
	}
	require.Equal(t, 16, s.TurnRequests)
	require.Zero(t, s.AuxReservedUSD)
	x.mu.Lock()
	require.Len(t, x.calls, 1)
	require.Len(t, x.billed, 1)
	require.InDelta(t, .1, x.charged, 1e-9)
	x.mu.Unlock()
	require.InDelta(t, .1, s.TurnCostUSD, 1e-9)
}

func TestFusionClosingStopsCacheAcrossAcceptedQueuedHandoff(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued_%t", queued), func(t *testing.T) {
			e, s, c, clock, x := cacheFixture(t)
			_, raw, err := e.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			s.Profile.ClosingPolicy = ClosingReserveV1
			s.Profile.MaxRequests = 20
			s.ProfileRevision = s.Profile.Revision()
			s.TurnRequests = 16
			require.NoError(t, e.save(context.Background(), s, &raw))
			req := testRequest("immutable private prefix")
			req.Model = s.Profile.LeadModel
			require.NoError(t, c.Capture(context.Background(), s, req, "lead", "route"))
			state, _, err := c.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			require.Equal(t, "ARMED", state.State)
			if queued {
				s.Handoffs[len(s.Handoffs)-1].Report = "actual prior report"
				s.Role = Lead
				s.State = WaitingTool
				s.Pending = &PendingTool{Role: Lead, Turn: s.Turn, PublicID: "pending", InternalID: "native", Name: "read_file", Arguments: `{}`}
				s.QueuedTools = []PendingTool{{Role: Lead, Turn: s.Turn, PublicID: "queued", InternalID: "delegate", Name: DelegateTool, Arguments: `{"task":"task","acceptance_criteria":"report evidence"}`}}
			}
			_, err = e.updateClosing(context.Background(), s, &raw)
			require.NoError(t, err)
			require.NoError(t, c.Observe(context.Background(), s))
			clock.Advance(10 * time.Second)
			require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
			state, _, err = c.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			require.Equal(t, "STOPPED", state.State)
			require.Equal(t, "budget_closing", state.StopReason)
			require.Zero(t, x.calls)
			require.Zero(t, state.ReservedUSD)
			require.Empty(t, state.Pending)
		})
	}
}

func TestFusionClosingPreservesAcceptedMixedQueueAcrossRestarts(t *testing.T) {
	store := newMemoryStore()
	in := input(testRequest("task"))
	in.Profile = DefaultProfile("fusion-test", "lead-real", "worker-real")
	in.Profile.MaxRequests = 8
	in.ClosingPolicy = ClosingReserveV1
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{
		mixedTools(tool("lead-read", "read_file", `{}`), delegation("d1"), delegation("d2"), tool("lead-tail", "edit_file", `{}`)),
		queuedToolResponse(), textResponse("report 1, partial"), textResponse("report 2, no more tools"), textResponse("reviewed both reports; unfinished"),
	}}
	var names []string
	for i := 0; i < 10; i++ {
		out, err := New(store).Advance(context.Background(), in, x)
		require.NoError(t, err)
		if out.State == Completed {
			require.Equal(t, BudgetClosed, out.Outcome)
			break
		}
		names = append(names, out.Response.Output[0].Name)
		in.Request = resultRequest(out.Response.Output[0].CallID, "accepted result")
		in.IdempotencyKey = fmt.Sprintf("result-%d", i)
	}
	require.Equal(t, []string{"read_file", "read_file", "edit_file", "edit_file"}, names)
	s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, Completed, s.State)
	require.Equal(t, "report 1, partial", s.Handoffs[0].Report)
	require.Equal(t, "report 2, no more tools", s.Handoffs[1].Report)
	require.Contains(t, string(x.calls[4].Request.Input), "report 1, partial")
	require.Contains(t, string(x.calls[4].Request.Input), "report 2, no more tools")
	require.Len(t, x.calls, 5)
	require.Len(t, x.settled, 5)
}

func TestFusionClosingRejectsUnsafeBatchAndNewWorkWithoutSideEffects(t *testing.T) {
	t.Run("new_delegation_obligations", func(t *testing.T) {
		in := input(testRequest("task"))
		in.ClosingPolicy = ClosingReserveV1
		in.Profile.MaxRequests = 7
		x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{mixedTools(tool("never-published", "read_file", `{}`), delegation("d1"), delegation("d2"), delegation("d3"))}}
		store := newMemoryStore()
		out, err := New(store).Advance(context.Background(), in, x)
		requireCode(t, err, "budget_exhausted")
		require.Nil(t, out.Response)
		s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
		require.NoError(t, err)
		require.Empty(t, s.Handoffs)
		require.Nil(t, s.Pending)
		require.Len(t, x.settled, 1)
	})
	t.Run("model_ignores_none", func(t *testing.T) {
		in := input(testRequest("task"))
		in.ClosingPolicy = ClosingReserveV1
		in.Profile = DefaultProfile("fusion-test", "lead-real", "worker-real")
		x := &closingExecutor{t: t, violate: true}
		store := newMemoryStore()
		for n := 0; n < 30; n++ {
			out, err := New(store).Advance(context.Background(), in, x)
			if err != nil {
				requireCode(t, err, "closing_tools_unsupported")
				break
			}
			in.Request = resultRequest(out.Response.Output[0].CallID, "result")
		}
		s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
		require.NoError(t, err)
		require.Equal(t, Failed, s.State)
		require.Nil(t, s.Pending)
		require.Empty(t, s.Handoffs[0].Report)
		require.Len(t, x.calls, 17)
		require.Len(t, x.settled, 17)
	})
}

func TestFusionClosingCancellationDeadlineAndUnknownNeverDispatchMore(t *testing.T) {
	for _, kind := range []string{"cancel", "deadline", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			store := newMemoryStore()
			in := input(testRequest("task"))
			in.ClosingPolicy = ClosingReserveV1
			x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), tool("w", "read_file", `{}`)}}
			out, err := New(store).Advance(context.Background(), in, x)
			require.NoError(t, err)
			s, raw, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			s.Closing = &ClosingState{Phase: closingDrain, StartedAt: time.Now(), RequestsAtStart: s.TurnRequests}
			require.NoError(t, New(store).save(context.Background(), s, &raw))
			in.Request = resultRequest(out.Response.Output[0].CallID, "last result")
			e := New(store)
			switch kind {
			case "cancel":
				require.NoError(t, e.Cancel(context.Background(), in.Scope, in.SessionID))
			case "deadline":
				e.Now = func() time.Time { return s.TurnStarted.Add(901 * time.Second) }
			case "unknown":
				x.execute = func(Call) (Outcome, error) { return Outcome{}, errors.New("unknown transport") }
			}
			_, err = e.Advance(context.Background(), in, x)
			if kind == "cancel" {
				requireCode(t, err, "cancelled")
				require.Len(t, x.calls, 2)
			}
			if kind == "deadline" {
				requireCode(t, err, "budget_exhausted")
				require.Len(t, x.calls, 2)
			}
			if kind == "unknown" {
				requireCode(t, err, "attempt_uncertain")
				require.Len(t, x.calls, 3)
				_, again := New(store).Advance(context.Background(), in, x)
				requireCode(t, again, "session_stopped")
				require.Len(t, x.calls, 3)
			}
		})
	}
}
