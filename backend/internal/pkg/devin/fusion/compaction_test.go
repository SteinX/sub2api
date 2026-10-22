package fusion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

type summaryExecutor struct {
	mu         sync.Mutex
	calls      []Call
	billed     map[string]bool
	charged    float64
	response   *apicompat.ResponsesResponse
	failSettle bool
	started    chan struct{}
	finish     chan struct{}
}

func (x *summaryExecutor) Estimate(context.Context, Call) (float64, error) { return .1, nil }
func (x *summaryExecutor) Execute(ctx context.Context, call Call) (Outcome, error) {
	x.mu.Lock()
	x.calls = append(x.calls, call)
	x.mu.Unlock()
	if x.started != nil {
		close(x.started)
		select {
		case <-x.finish:
		case <-ctx.Done():
		}
	}
	return Outcome{Response: x.response, BillingModel: call.Request.Model}, nil
}
func (x *summaryExecutor) Settle(_ context.Context, a Attempt) (float64, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.failSettle {
		x.failSettle = false
		return 0, errors.New("billing interrupted")
	}
	if x.billed == nil {
		x.billed = map[string]bool{}
	}
	if !x.billed[a.ID] {
		x.billed[a.ID] = true
		x.charged += .1
	}
	return .1, nil
}
func (x *summaryExecutor) Auxiliary(context.Context) (Executor, func(), bool) {
	return x, func() {}, true
}
func summaryResponse() *apicompat.ResponsesResponse {
	return textResponse(`{"goal":"Preserve original constraints","current_state":"Historical work summarized","decisions":["Keep the existing interface"],"changes":[],"evidence":["Real tool output retained by reference"],"checks":["One check failed"],"open_questions":[],"next_steps":["Verify the remaining change"],"files":["main.go"]}`)
}
func summarySession(t *testing.T, role Role) (*Engine, *Session, []byte, *apicompat.ResponsesRequest) {
	t.Helper()
	e := New(newMemoryStore())
	e.Runner = NewMaintenanceRunner(2)
	p := testProfile()
	p.MaxContextBytes = 24000
	p.MaxRequests = 64
	s := &Session{ID: "summary-chat", Scope: Scope(1, 2), AccountID: 7, Profile: p, ProfileRevision: p.Revision(), PublicModel: p.PublicModel, Role: role, State: RunningSidekick, Turn: 1, TurnStarted: time.Now(), ExpiresAt: time.Now().Add(time.Hour), ActiveRequest: -1}
	s.Handoffs = []Handoff{{ID: "active-handoff", CallID: "delegate-real-id", Turn: 1, Brief: `{"task":"Keep the original public API","constraints":"Never edit generated files","acceptance_criteria":"Actual tests pass"}`}}
	lane := []apicompat.ResponsesInputItem{textMessage("user", "Original user goal: keep compatibility; never edit generated files")}
	if role == Sidekick {
		lane = append(lane, textMessage("user", "Lead brief for active-handoff: "+s.Handoffs[0].Brief))
	}
	for i := 0; i < 12; i++ {
		id := string(rune('a' + i))
		lane = append(lane, apicompat.ResponsesInputItem{Type: "function_call", CallID: id, Name: "read_file", Arguments: `{}`}, apicompat.ResponsesInputItem{Type: "function_call_output", CallID: id, Output: strings.Repeat("old completed tool evidence ", 35)})
	}
	*s.Lane(role) = lane
	s.Public = []apicompat.ResponsesInputItem{textMessage("user", "public checkpoint")}
	var raw []byte
	require.NoError(t, e.save(context.Background(), s, &raw))
	return e, s, raw, testRequest("new task")
}
func TestFusionCompactionThresholdsAndDisabledHardGate(t *testing.T) {
	p := testProfile()
	p.Compaction = &CompactionPolicy{Enabled: true, Lead: []int{1000, 3000}, Sidekick: []int{500, 1500, 2500}}
	require.NoError(t, p.Validate())
	lead, err := p.CompactPolicy().thresholds(Lead, 5000)
	require.NoError(t, err)
	require.Equal(t, 3000, lead.apply)
	worker, err := p.CompactPolicy().thresholds(Sidekick, 4000)
	require.NoError(t, err)
	require.Equal(t, 1500, worker.apply)
	require.Error(t, p.ValidateContextBudget(Sidekick, 2000))
	for _, v := range [][]int{{1000}, {2000, 1000}, {1000, 3000, 2000}, {0, 1}} {
		p.Compaction.Lead = v
		require.Error(t, p.Validate())
	}
	e, s, raw, base := summarySession(t, Sidekick)
	s.Profile.Compaction = &CompactionPolicy{Enabled: false}
	s.Profile.MaxContextBytes = 4000
	x := &summaryExecutor{response: summaryResponse()}
	_, err = e.prepareRoleContext(context.Background(), s, &raw, base, x)
	requireCode(t, err, "context_limit")
	require.Empty(t, x.calls)
}
func TestFusionCompactionPreservesHandoffToolsSuffixAndArchives(t *testing.T) {
	e, s, raw, base := summarySession(t, Sidekick)
	x := &summaryExecutor{response: summaryResponse()}
	s.Lead = []apicompat.ResponsesInputItem{textMessage("user", "Lead goal"), {Type: "function_call", CallID: "delegate-real-id", Name: DelegateTool, Arguments: s.Handoffs[0].Brief}}
	leadBefore := Digest(s.Lead)
	publicBefore := Digest(s.Public)
	s.Sidekick = append([]apicompat.ResponsesInputItem{{Type: "reasoning", EncryptedContent: "private-replay-must-not-enter-summary"}}, s.Sidekick...)
	ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, false)
	require.NoError(t, err)
	require.True(t, ok)
	j := s.roleContext(Sidekick).Job
	before := append([]apicompat.ResponsesInputItem(nil), s.Sidekick...)
	require.NotContains(t, string(x.calls[0].Request.Input), "private-replay-must-not-enter-summary")
	suffix := Digest(before[j.PrefixCount:])
	prefix := Digest(before[:j.PrefixCount])
	cutCalls := map[string]bool{}
	for _, i := range before[:j.PrefixCount] {
		if i.Type == "function_call" {
			cutCalls[i.CallID] = true
		}
		if i.Type == "function_call_output" {
			delete(cutCalls, i.CallID)
		}
	}
	require.Empty(t, cutCalls)
	require.NoError(t, e.collectCompactions(context.Background(), s, &raw, x))
	ok, err = e.applyCompaction(context.Background(), s, &raw, Sidekick, base, x)
	require.NoError(t, err)
	require.True(t, ok)
	require.EqualValues(t, 1, s.roleContext(Sidekick).Epoch)
	require.Equal(t, leadBefore, Digest(s.Lead))
	require.Equal(t, publicBefore, Digest(s.Public))
	encoded := string(mustJSON(s.Sidekick))
	require.Contains(t, encoded, "Never edit generated files")
	require.Contains(t, encoded, "active-handoff")
	require.Equal(t, suffix, Digest(s.Sidekick[len(s.Sidekick)-(len(before)-j.PrefixCount):]))
	archive, err := e.Store.Get(context.Background(), indexKey(s.Scope, "compact_prefix", s.ID+":"+j.ID))
	require.NoError(t, err)
	var archived []apicompat.ResponsesInputItem
	require.NoError(t, json.Unmarshal(archive, &archived))
	require.Equal(t, prefix, Digest(archived))
	ok, err = e.applyCompaction(context.Background(), s, &raw, Sidekick, base, x)
	require.NoError(t, err)
	require.False(t, ok)
	require.InDelta(t, .1, x.charged, .00001)
}
func TestFusionAsyncCompactionKeepsAppendedSuffixAndDoesNotApplyBelowThreshold(t *testing.T) {
	e, s, raw, base := summarySession(t, Sidekick)
	x := &summaryExecutor{response: summaryResponse(), started: make(chan struct{}), finish: make(chan struct{})}
	ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, true)
	require.NoError(t, err)
	require.True(t, ok)
	<-x.started
	j := s.roleContext(Sidekick).Job
	ok, err = e.prepareCompaction(context.Background(), s, &raw, base, x, true)
	require.NoError(t, err)
	require.False(t, ok)
	s.Sidekick = append(s.Sidekick, textMessage("user", "New suffix appended while summary was running"))
	require.NoError(t, e.save(context.Background(), s, &raw))
	close(x.finish)
	<-e.Runner.Done(j.ID)
	require.NoError(t, e.collectCompactions(context.Background(), s, &raw, x))
	require.Equal(t, "ready", j.State)
	// A ready job remains unapplied below apply. The real suffix is never the
	// snapshot captured by the summary worker.
	_, err = e.prepareRoleContext(context.Background(), s, &raw, base, x)
	require.NoError(t, err)
	require.Equal(t, "ready", j.State)
	ok, err = e.applyCompaction(context.Background(), s, &raw, Sidekick, base, x)
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, string(s.Sidekick[len(s.Sidekick)-1].Content), "New suffix appended")
	require.Len(t, x.calls, 1)
	require.Empty(t, x.calls[0].Request.Tools)
	require.Equal(t, Compaction, x.calls[0].Purpose)
}
func TestFusionStaleAndCancelledSummariesAreBilledButNotApplied(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "rewritten prefix", true: "cancelled session"}[cancelled], func(t *testing.T) {
			e, s, raw, base := summarySession(t, Sidekick)
			x := &summaryExecutor{response: summaryResponse()}
			ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, false)
			require.NoError(t, err)
			require.True(t, ok)
			require.NoError(t, e.collectCompactions(context.Background(), s, &raw, x))
			if cancelled {
				require.NoError(t, e.Cancel(context.Background(), s.Scope, s.ID))
				s, raw, err = e.Load(context.Background(), s.Scope, s.ID)
				require.NoError(t, err)
			} else {
				s.Sidekick[0] = textMessage("user", "rewritten prefix")
				require.NoError(t, e.save(context.Background(), s, &raw))
			}
			before := Digest(s.Sidekick)
			ok, err = e.applyCompaction(context.Background(), s, &raw, Sidekick, base, x)
			require.NoError(t, err)
			require.False(t, ok)
			require.Equal(t, before, Digest(s.Sidekick))
			require.InDelta(t, .1, x.charged, .00001)
		})
	}
}
func TestFusionCompactionBillingRecoveryAndUnknownNeverReplay(t *testing.T) {
	e, s, raw, base := summarySession(t, Sidekick)
	x := &summaryExecutor{response: summaryResponse(), failSettle: true}
	ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, false)
	require.NoError(t, err)
	require.True(t, ok)
	restarted := New(e.Store)
	s, raw, err = restarted.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.NoError(t, restarted.collectCompactions(context.Background(), s, &raw, x))
	require.Len(t, x.calls, 1)
	require.InDelta(t, .1, x.charged, .00001)
	require.NoError(t, restarted.collectCompactions(context.Background(), s, &raw, x))
	require.InDelta(t, .1, s.Maintenance.CompactionCostUSD, .00001)
	// A durable started record with no terminal result is never issued again.
	s.roleContext(Sidekick).Job = nil
	x.response = nil
	ok, err = restarted.prepareCompaction(context.Background(), s, &raw, base, x, false)
	require.NoError(t, err)
	require.True(t, ok)
	err = restarted.collectCompactions(context.Background(), s, &raw, x)
	requireCode(t, err, "compaction_uncertain")
	require.Len(t, x.calls, 2)
	require.Greater(t, s.AuxReservedUSD, 0.0)
}
func TestFusionCompactionRejectsUnsafeSummariesAndUnshrinkableContext(t *testing.T) {
	for _, response := range []*apicompat.ResponsesResponse{textResponse(""), textResponse("not JSON"), tool("bad", "shell", `{}`)} {
		e, s, raw, base := summarySession(t, Sidekick)
		x := &summaryExecutor{response: response}
		ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, false)
		require.NoError(t, err)
		require.True(t, ok)
		require.NoError(t, e.collectCompactions(context.Background(), s, &raw, x))
		require.Equal(t, "failed", s.roleContext(Sidekick).Job.State)
		require.InDelta(t, .1, x.charged, .00001)
	}
	e, s, raw, base := summarySession(t, Lead)
	x := &summaryExecutor{response: summaryResponse()}
	base.Instructions = strings.Repeat("required system constraint ", 2000)
	_, err := e.prepareRoleContext(context.Background(), s, &raw, base, x)
	requireCode(t, err, "context_limit")
	require.LessOrEqual(t, len(x.calls), 1)
}

type longFusionExecutor struct {
	leadCalls int
	handoff   int
	tools     int
	normal    int
	summaries int
	billed    map[string]bool
}

func (x *longFusionExecutor) Estimate(context.Context, Call) (float64, error) { return .01, nil }
func (x *longFusionExecutor) MeasureContext(r *apicompat.ResponsesRequest) (int, int, error) {
	safe := 16000
	if r.Model == "worker-real" {
		safe = 12000
	}
	b, _ := json.Marshal(r)
	return len(b), safe, nil
}
func (x *longFusionExecutor) Execute(_ context.Context, c Call) (Outcome, error) {
	if c.Purpose == Compaction {
		x.summaries++
		return Outcome{Response: summaryResponse(), BillingModel: c.Request.Model}, nil
	}
	x.normal++
	if c.Role == Lead {
		x.leadCalls++
		if x.leadCalls <= 2 {
			x.handoff++
			x.tools = 0
			return Outcome{Response: delegation(fmt.Sprintf("delegation-%d", x.handoff)), BillingModel: c.Request.Model}, nil
		}
		return Outcome{Response: textResponse("Final reviewed result"), BillingModel: c.Request.Model}, nil
	}
	if x.tools < 8 {
		x.tools++
		return Outcome{Response: tool(fmt.Sprintf("worker-%d-%d", x.handoff, x.tools), "read_file", `{}`), BillingModel: c.Request.Model}, nil
	}
	return Outcome{Response: textResponse("Completed the current brief with tool evidence"), BillingModel: c.Request.Model}, nil
}
func (x *longFusionExecutor) Settle(_ context.Context, a Attempt) (float64, error) {
	if x.billed == nil {
		x.billed = map[string]bool{}
	}
	x.billed[a.ID] = true
	return .01, nil
}

func TestFusionLongToolWorkflowSurvivesIndependentRoleCompaction(t *testing.T) {
	e := New(newMemoryStore())
	x := &longFusionExecutor{}
	in := input(testRequest("Fix the bug; keep the public API unchanged"))
	in.Profile.MaxContextBytes = 16000
	in.Profile.MaxRequests = 64
	in.Profile.Compaction = &CompactionPolicy{Enabled: true, MaxAttempts: 4}
	result, err := e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	inputTokens := result.Response.Usage.InputTokens
	publicCalls := map[string]bool{}
	for result.State == WaitingTool {
		call := result.Response.Output[0]
		require.False(t, publicCalls[call.CallID])
		publicCalls[call.CallID] = true
		in.Request = resultRequest(call.CallID, strings.Repeat("Actual client tool result; test failed then corrected. ", 25))
		result, err = e.Advance(context.Background(), in, x)
		require.NoError(t, err)
		inputTokens += result.Response.Usage.InputTokens
	}
	require.Equal(t, Completed, result.State)
	require.Len(t, publicCalls, 16)
	require.Equal(t, 2, x.handoff)
	require.Greater(t, x.summaries, 0)
	require.Equal(t, x.normal*100, inputTokens, "public usage excludes independently billed summaries")
	s, _, err := e.Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Greater(t, s.roleContext(Sidekick).Epoch, uint64(0))
	require.Zero(t, s.roleContext(Lead).Epoch)
	require.Len(t, x.billed, x.normal+x.summaries)
	require.Equal(t, x.summaries, s.Maintenance.CompactionsApplied)
	require.NotContains(t, string(mustJSON(result.Response)), "Historical summary")
	require.Len(t, s.Handoffs, 2)
	for _, h := range s.Handoffs {
		require.NotEmpty(t, h.Report)
	}
}

func TestFusionSummaryCASRecoveryAppliesOnceAndRetainsSource(t *testing.T) {
	e, s, raw, base := summarySession(t, Sidekick)
	x := &summaryExecutor{response: summaryResponse()}
	ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, false)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, e.collectCompactions(context.Background(), s, &raw, x))
	store, validStore := e.Store.(*memoryStore)
	require.True(t, validStore)
	store.failCAS = func(b []byte) bool {
		var candidate Session
		if json.Unmarshal(b, &candidate) != nil {
			return false
		}
		return candidate.Maintenance.CompactionsApplied > 0
	}
	ok, err = e.applyCompaction(context.Background(), s, &raw, Sidekick, base, x)
	require.Error(t, err)
	require.False(t, ok)
	restart := New(e.Store)
	s, raw, err = restart.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	ok, err = restart.applyCompaction(context.Background(), s, &raw, Sidekick, base, x)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, s.roleContext(Sidekick).Archives, 1)
	require.Equal(t, 1, s.Maintenance.CompactionsApplied)
	require.Len(t, x.calls, 1)
}

func TestFusionSummaryArchiveBudgetStopsNewPaidPreparation(t *testing.T) {
	e, s, raw, base := summarySession(t, Sidekick)
	s.Maintenance.ArchivedBytes = 12 * 1024 * 1024
	x := &summaryExecutor{response: summaryResponse()}
	ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, false)
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, x.calls)
}

func TestFusionCompactionKeepsLaterLeadGoalsVerbatim(t *testing.T) {
	e, s, raw, base := summarySession(t, Lead)
	x := &summaryExecutor{response: summaryResponse()}
	goal := textMessage("user", "Current turn constraint: preserve the exact billing format ABC-917")
	s.Lead = append(s.Lead[:1], append([]apicompat.ResponsesInputItem{goal}, s.Lead[1:]...)...)
	report := "Real current Sidekick report: verification failed; keep the original API"
	pair := []apicompat.ResponsesInputItem{{Type: "function_call", Name: DelegateTool, CallID: s.Handoffs[0].CallID, Arguments: s.Handoffs[0].Brief}, {Type: "function_call_output", CallID: s.Handoffs[0].CallID, Output: report}}
	s.Lead = append(s.Lead[:2], append(pair, s.Lead[2:]...)...)
	s.Handoffs[0].Report = report
	ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, false)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, e.collectCompactions(context.Background(), s, &raw, x))
	ok, err = e.applyCompaction(context.Background(), s, &raw, Lead, base, x)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, Digest(goal), Digest(s.Lead[1]))
	require.Equal(t, Digest(pair), Digest(s.Lead[2:4]))
}

func TestFusionLateCancelledSummaryOnlySettlesUsage(t *testing.T) {
	e, s, raw, base := summarySession(t, Sidekick)
	x := &summaryExecutor{response: summaryResponse(), started: make(chan struct{}), finish: make(chan struct{})}
	ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, true)
	require.NoError(t, err)
	require.True(t, ok)
	<-x.started
	id := s.roleContext(Sidekick).Job.ID
	require.NoError(t, e.Cancel(context.Background(), s.Scope, s.ID))
	close(x.finish)
	<-e.Runner.Done(id)
	s, raw, err = e.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.NoError(t, e.collectCompactions(context.Background(), s, &raw, x))
	before := Digest(s.Sidekick)
	ok, err = e.applyCompaction(context.Background(), s, &raw, Sidekick, base, x)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, Cancelled, s.State)
	require.Equal(t, before, Digest(s.Sidekick))
	require.InDelta(t, .1, x.charged, .00001)
}
