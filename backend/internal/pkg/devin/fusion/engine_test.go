package fusion

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type memoryStore struct {
	mu      sync.Mutex
	values  map[string][]byte
	locks   map[string]bool
	failCAS func([]byte) bool
}

func newMemoryStore() *memoryStore {
	return &memoryStore{values: map[string][]byte{}, locks: map[string]bool{}}
}
func (m *memoryStore) Get(_ context.Context, k string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.values[k]
	if !ok {
		return nil, devin.ErrSessionNotFound
	}
	return append([]byte(nil), b...), nil
}
func (m *memoryStore) Set(_ context.Context, k string, b []byte, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[k] = append([]byte(nil), b...)
	return nil
}
func (m *memoryStore) Delete(_ context.Context, k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, k)
	return nil
}
func (m *memoryStore) Acquire(_ context.Context, k string, _ time.Duration) (func(), bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks[k] {
		return nil, false, nil
	}
	m.locks[k] = true
	return func() { m.mu.Lock(); delete(m.locks, k); m.mu.Unlock() }, true, nil
}
func (m *memoryStore) CompareAndSwap(_ context.Context, k string, old, next []byte, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failCAS != nil && m.failCAS(next) {
		m.failCAS = nil
		return false, errors.New("simulated store interruption")
	}
	current, ok := m.values[k]
	if old == nil && ok || old != nil && (!ok || string(current) != string(old)) {
		return false, nil
	}
	m.values[k] = append([]byte(nil), next...)
	return true, nil
}
func testProfile() Profile {
	return Profile{Enabled: true, PublicModel: "fusion-test", LeadModel: "lead-real", SidekickModel: "worker-real", MaxHandoffs: 4, MaxRequests: 20, MaxTools: 20, MaxWallSeconds: 900, MaxCostUSD: 30, MaxContextBytes: 256 * 1024, MaxOutputTokens: 1024, TTLSeconds: 3600}
}
func testRequest(input any) *apicompat.ResponsesRequest {
	b, _ := json.Marshal(input)
	return &apicompat.ResponsesRequest{Model: "fusion-test", Input: b, Tools: []apicompat.ResponsesTool{{Type: "function", Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}, {Type: "function", Name: "edit_file", Parameters: json.RawMessage(`{"type":"object"}`)}}}
}
func tool(id, name, args string) *apicompat.ResponsesResponse {
	return &apicompat.ResponsesResponse{Model: "physical", Status: "completed", Output: []apicompat.ResponsesOutput{{Type: "function_call", CallID: id, Name: name, Arguments: args}}, Usage: &apicompat.ResponsesUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110, CacheCreationInputTokens: 10, InputTokensDetails: &apicompat.ResponsesInputTokensDetails{CachedTokens: 40}}}
}
func textResponse(text string) *apicompat.ResponsesResponse {
	r := tool("unused", "unused", `{}`)
	r.Output = []apicompat.ResponsesOutput{{Type: "message", Role: "assistant", Content: []apicompat.ResponsesContentPart{{Type: "output_text", Text: text}}}}
	return r
}
func delegation(id string) *apicompat.ResponsesResponse {
	return tool(id, DelegateTool, `{"task":"Implement the change","constraints":"Use only client tools","acceptance_criteria":"Tests pass"}`)
}

type scriptExecutor struct {
	t          *testing.T
	responses  []*apicompat.ResponsesResponse
	calls      []Call
	settled    map[string]bool
	charged    float64
	failSettle bool
	execute    func(Call) (Outcome, error)
}

func (x *scriptExecutor) Estimate(context.Context, Call) (float64, error) { return 1, nil }
func (x *scriptExecutor) Execute(_ context.Context, c Call) (Outcome, error) {
	x.calls = append(x.calls, c)
	if x.execute != nil {
		return x.execute(c)
	}
	require.NotEmpty(x.t, x.responses)
	r := x.responses[0]
	x.responses = x.responses[1:]
	return Outcome{Response: r, BillingModel: c.Request.Model}, nil
}
func (x *scriptExecutor) Settle(_ context.Context, a Attempt) (float64, error) {
	if x.failSettle {
		x.failSettle = false
		return 0, errors.New("billing temporarily down")
	}
	if x.settled == nil {
		x.settled = map[string]bool{}
	}
	cost := 1.0
	if a.Role == Sidekick {
		cost = 0.2
	}
	if !x.settled[a.ID] {
		x.settled[a.ID] = true
		x.charged += cost
	}
	return cost, nil
}
func input(r *apicompat.ResponsesRequest) Input {
	return Input{Scope: Scope(1, 2), SessionID: "chat-one", AccountID: 7, Profile: testProfile(), Request: r}
}
func resultRequest(callID, output string) *apicompat.ResponsesRequest {
	return testRequest([]apicompat.ResponsesInputItem{{Type: "function_call_output", CallID: callID, Output: output}})
}
func requireCode(t *testing.T, err error, suffix string) {
	t.Helper()
	var typed *Error
	require.ErrorAs(t, err, &typed)
	require.Equal(t, "devin_fusion_"+suffix, typed.Code)
}

func TestFusionPersistentLanesToolsReworkAndPerRequestUsage(t *testing.T) {
	store := newMemoryStore()
	engine := New(store)
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("lead-delegate-1"), tool("worker-read", "read_file", `{"path":"main.go"}`), tool("worker-edit", "edit_file", `{"path":"main.go"}`), textResponse("Sidekick report: edited main.go; tests initially failed"), delegation("lead-delegate-2"), textResponse("Sidekick report: fixed tests; evidence: go test passed"), tool("lead-read", "read_file", `{"path":"main_test.go"}`), textResponse("Implemented and reviewed; tests pass.")}}
	first, err := engine.Advance(context.Background(), input(testRequest("Fix the bug")), x)
	require.NoError(t, err)
	require.Equal(t, WaitingTool, first.State)
	require.Equal(t, "read_file", first.Response.Output[0].Name)
	require.NotContains(t, first.Response.Output[0].CallID, "worker-read")
	require.Equal(t, 200, first.Response.Usage.InputTokens)
	sid, account, err := engine.Resolve(context.Background(), Scope(1, 2), "", "", resultRequest(first.Response.Output[0].CallID, "file contents"))
	require.NoError(t, err)
	require.Equal(t, first.SessionID, sid)
	require.EqualValues(t, 7, account)
	second, err := New(store).Advance(context.Background(), input(resultRequest(first.Response.Output[0].CallID, "file contents")), x)
	require.NoError(t, err)
	require.Equal(t, "edit_file", second.Response.Output[0].Name)
	require.Equal(t, 100, second.Response.Usage.InputTokens)
	third, err := New(store).Advance(context.Background(), input(resultRequest(second.Response.Output[0].CallID, "edited; test failed")), x)
	require.NoError(t, err)
	require.Equal(t, "read_file", third.Response.Output[0].Name)
	require.Equal(t, 400, third.Response.Usage.InputTokens)
	fourth, err := engine.Advance(context.Background(), input(resultRequest(third.Response.Output[0].CallID, "test source verified")), x)
	require.NoError(t, err)
	require.Equal(t, Completed, fourth.State)
	require.Equal(t, "Implemented and reviewed; tests pass.", outputText(fourth.Response))
	require.Equal(t, 100, fourth.Response.Usage.InputTokens)
	require.Len(t, x.calls, 8)
	require.InDelta(t, 4.8, x.charged, 0.0001)
	laneScopes := map[Role]string{}
	for _, c := range x.calls {
		if previous := laneScopes[c.Role]; previous != "" {
			require.Equal(t, previous, c.LaneScope)
		}
		laneScopes[c.Role] = c.LaneScope
		require.NotEqual(t, laneScopes[Lead], laneScopes[Sidekick])
		require.NotContains(t, string(c.Request.Input), "call_fusion_")
	}
	require.Contains(t, string(x.calls[5].Request.Input), "worker-read")
	require.Contains(t, string(x.calls[5].Request.Input), "worker-edit")
	require.Contains(t, string(x.calls[5].Request.Input), "file contents")
	require.NotContains(t, string(x.calls[4].Request.Input), "file contents")
	require.Contains(t, string(x.calls[4].Request.Input), "Sidekick report")
	session, _, err := engine.Load(context.Background(), Scope(1, 2), first.SessionID)
	require.NoError(t, err)
	require.Len(t, session.Handoffs, 2)
	require.NotEqual(t, session.Handoffs[0].ID, session.Handoffs[1].ID)
	require.NotContains(t, string(mustJSON(fourth.Response)), DelegateTool)
	require.NotContains(t, string(mustJSON(third.Response)), "Sidekick report")
	// A new user turn retains both lanes and consults the Sidekick again.
	x.responses = []*apicompat.ResponsesResponse{delegation("lead-delegate-3"), textResponse("Follow-up evidence"), textResponse("Follow-up done")}
	next := input(testRequest("Now explain the change"))
	next.IdempotencyKey = "next-turn"
	_, err = engine.Advance(context.Background(), next, x)
	require.NoError(t, err)
	require.Contains(t, string(x.calls[9].Request.Input), "worker-edit")
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestFusionReplayOwnershipAndConflictingResults(t *testing.T) {
	store := newMemoryStore()
	e := New(store)
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), tool("r", "read_file", `{}`), textResponse("report"), textResponse("done")}}
	in := input(testRequest("task"))
	in.IdempotencyKey = "initial"
	first, err := e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	replay, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, first.Response, replay.Response)
	require.Len(t, x.calls, 2)
	in.Request = testRequest("different task")
	_, err = e.Advance(context.Background(), in, x)
	requireCode(t, err, "idempotency_conflict")
	_, _, err = e.Resolve(context.Background(), Scope(1, 99), "", "", resultRequest(first.Response.Output[0].CallID, "stolen"))
	requireCode(t, err, "tool_unknown")
	foreign := input(resultRequest(first.Response.Output[0].CallID, "stolen"))
	foreign.AccountID = 8
	_, err = e.Advance(context.Background(), foreign, x)
	requireCode(t, err, "account_bound")
	_, err = e.Advance(context.Background(), input(testRequest("steering while tool pending")), x)
	requireCode(t, err, "tool_pending")
	toolInput := input(resultRequest(first.Response.Output[0].CallID, "result"))
	last, err := e.Advance(context.Background(), toolInput, x)
	require.NoError(t, err)
	again, err := e.Advance(context.Background(), toolInput, x)
	require.NoError(t, err)
	require.True(t, again.Replayed)
	require.Equal(t, last.Response.ID, again.Response.ID)
	require.Len(t, x.calls, 4)
	_, err = e.Advance(context.Background(), input(resultRequest(first.Response.Output[0].CallID, "conflicting result")), x)
	requireCode(t, err, "history_conflict")
}
func TestFusionCrashRecoveryReusesDurableOutcomes(t *testing.T) {
	for _, stage := range []string{"before_outcome", "after_outcome", "after_apply", "after_publication"} {
		t.Run(stage, func(t *testing.T) {
			store := newMemoryStore()
			e := New(store)
			x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), tool("r", "read_file", `{}`)}}
			fired := false
			store.failCAS = func(b []byte) bool {
				var s Session
				if json.Unmarshal(b, &s) != nil || len(s.Attempts) == 0 {
					return false
				}
				a := s.Attempts[len(s.Attempts)-1]
				match := stage == "before_outcome" && a.Status == "completed" && !a.Applied || stage == "after_outcome" && a.Settled && !a.Applied || stage == "after_apply" && a.Applied || stage == "after_publication" && s.ActiveRequest == -1
				if match {
					fired = true
				}
				return match
			}
			in := input(testRequest("task"))
			_, err := e.Advance(context.Background(), in, x)
			require.Error(t, err)
			require.True(t, fired)
			result, err := New(store).Advance(context.Background(), in, x)

			require.NoError(t, err)
			require.Equal(t, WaitingTool, result.State)
			require.Len(t, x.calls, 2)
			require.InDelta(t, 1.2, x.charged, 0.0001)
		})
	}
}
func TestFusionBillingRetryAndCancellation(t *testing.T) {
	store := newMemoryStore()
	e := New(store)
	x := &scriptExecutor{t: t, failSettle: true, responses: []*apicompat.ResponsesResponse{delegation("d"), tool("r", "read_file", `{}`)}}
	in := input(testRequest("task"))
	_, err := e.Advance(context.Background(), in, x)
	requireCode(t, err, "billing_pending")
	require.Len(t, x.calls, 1)
	first, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Len(t, x.calls, 2)
	require.NoError(t, e.Cancel(context.Background(), in.Scope, in.SessionID))
	_, err = e.Advance(context.Background(), input(resultRequest(first.Response.Output[0].CallID, "late output")), x)
	requireCode(t, err, "cancelled")
	require.Len(t, x.calls, 2)
}
func TestFusionBudgetContextToolAndContractBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Input, *scriptExecutor)
		code   string
	}{
		{"cost", func(i *Input, x *scriptExecutor) { i.Profile.MaxCostUSD = 0.5 }, "budget_exhausted"},
		{"undeclared tool", func(i *Input, x *scriptExecutor) {
			x.responses = []*apicompat.ResponsesResponse{delegation("d"), tool("bad", "shell_not_supplied", `{}`)}
		}, "tool_invalid"},
		{"mixed delegation", func(i *Input, x *scriptExecutor) {
			r := delegation("d")
			r.Output = append(r.Output, tool("b", "edit_file", `{}`).Output...)
			x.responses = []*apicompat.ResponsesResponse{r}
		}, "parallel_tools_unsupported"},
		{"no delegation", func(i *Input, x *scriptExecutor) {
			x.responses = []*apicompat.ResponsesResponse{textResponse("unreviewed draft"), textResponse("pretend done")}
		}, "delegation_required"},
		{"reserved tool", func(i *Input, x *scriptExecutor) {
			i.Request.Tools = append(i.Request.Tools, apicompat.ResponsesTool{Type: "function", Name: DelegateTool})
		}, "tools_unsupported"},
		{"context", func(i *Input, x *scriptExecutor) {
			i.Profile.MaxContextBytes = 1024
			i.Request = testRequest(strings.Repeat("x", 2048))
		}, "context_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New(newMemoryStore())
			in := input(testRequest("task"))
			x := &scriptExecutor{t: t}
			tc.change(&in, x)
			_, err := e.Advance(context.Background(), in, x)
			requireCode(t, err, tc.code)
		})
	}
	e := New(newMemoryStore())
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), tool("r", "read_file", `{}`)}}
	first, err := e.Advance(context.Background(), input(testRequest("task")), x)
	require.NoError(t, err)
	changed := input(resultRequest(first.Response.Output[0].CallID, "result"))
	changed.Request.Tools = changed.Request.Tools[:1]
	_, err = e.Advance(context.Background(), changed, x)
	requireCode(t, err, "contract_changed")
	e.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	_, _, err = e.Load(context.Background(), changed.Scope, changed.SessionID)
	requireCode(t, err, "session_expired")
}
func TestFusionFullHistoryAndResponseLineage(t *testing.T) {
	store := newMemoryStore()
	e := New(store)
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), textResponse("report"), textResponse("done")}}
	first, err := e.Advance(context.Background(), input(testRequest("task")), x)
	require.NoError(t, err)
	history := []apicompat.ResponsesInputItem{textMessage("user", "task"), textMessage("assistant", "done"), textMessage("user", "next")}
	r := testRequest(history)
	sid, account, err := e.Resolve(context.Background(), Scope(1, 2), "", "", r)
	require.NoError(t, err)
	require.Equal(t, first.SessionID, sid)
	require.EqualValues(t, 7, account)
	r = testRequest("next")
	r.PreviousResponseID = first.Response.ID
	sid, _, err = e.Resolve(context.Background(), Scope(1, 2), "", "", r)
	require.NoError(t, err)
	require.Equal(t, first.SessionID, sid)
	history[1] = textMessage("assistant", "rewritten")
	_, err = e.Advance(context.Background(), input(testRequest(history)), x)
	requireCode(t, err, "history_conflict")
}
func TestFusionBusySessionAndCAS(t *testing.T) {
	store := newMemoryStore()
	e := New(store)
	entered := make(chan struct{})
	finish := make(chan struct{})
	x := &scriptExecutor{t: t, execute: func(c Call) (Outcome, error) {
		close(entered)
		<-finish
		return Outcome{}, errors.New("unknown transport")
	}}
	done := make(chan error, 1)
	go func() { _, err := e.Advance(context.Background(), input(testRequest("task")), x); done <- err }()
	<-entered
	_, err := e.Advance(context.Background(), input(testRequest("task")), &scriptExecutor{t: t})
	requireCode(t, err, "session_busy")
	close(finish)
	requireCode(t, <-done, "attempt_uncertain")
	ok, err := store.CompareAndSwap(context.Background(), "cas", nil, []byte("1"), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.CompareAndSwap(context.Background(), "cas", []byte("stale"), []byte("2"), time.Minute)
	require.NoError(t, err)
	require.False(t, ok)
}
func TestFusionRedisAtomicPersistenceAndExpiry(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer func() { _ = client.Close() }()
	store := devin.NewRedisSessionStore(client)
	e := New(store)
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), tool("r", "read_file", `{}`)}}
	in := input(testRequest("task"))
	in.Profile.TTLSeconds = 60
	first, err := e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	sid, account, err := New(store).Resolve(context.Background(), in.Scope, "", "", resultRequest(first.Response.Output[0].CallID, "result"))
	require.NoError(t, err)
	require.Equal(t, in.SessionID, sid)
	require.EqualValues(t, 7, account)
	server.FastForward(61 * time.Second)
	_, _, err = New(store).Resolve(context.Background(), in.Scope, in.SessionID, "", testRequest("late"))
	requireCode(t, err, "session_expired")
}

func TestFusionDisconnectStopsBeforeNextRoleAndCanResume(t *testing.T) {
	store := newMemoryStore()
	engine := New(store)
	ctx, cancel := context.WithCancel(context.Background())
	x := &scriptExecutor{t: t}
	x.execute = func(c Call) (Outcome, error) {
		cancel()
		return Outcome{Response: delegation("d"), BillingModel: c.Request.Model}, nil
	}
	in := input(testRequest("task"))
	_, err := engine.Advance(ctx, in, x)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, x.calls, 1)
	require.Len(t, x.settled, 1)
	x.execute = nil
	x.responses = []*apicompat.ResponsesResponse{textResponse("report"), textResponse("done")}
	result, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Equal(t, Completed, result.State)
	require.Len(t, x.calls, 3)
	require.Len(t, x.settled, 3)
}
func TestFusionProfileRevisionPinnedAndGlobalIdempotencyClaim(t *testing.T) {
	store := newMemoryStore()
	engine := New(store)
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), tool("r", "read_file", `{}`), textResponse("report"), textResponse("done")}}
	firstInput := input(testRequest("task"))
	firstInput.IdempotencyKey = "global-key"
	first, err := engine.Advance(context.Background(), firstInput, x)
	require.NoError(t, err)
	next := input(resultRequest(first.Response.Output[0].CallID, "output"))
	next.Profile.SidekickModel = "changed-worker"
	_, err = engine.Advance(context.Background(), next, x)
	require.NoError(t, err)
	require.Equal(t, "worker-real", x.calls[2].Request.Model)
	other := firstInput
	other.SessionID = "independent-chat"
	_, err = engine.Advance(context.Background(), other, x)
	requireCode(t, err, "idempotency_conflict")
	require.Len(t, x.calls, 4)
}
func TestFusionIdenticalIndependentTranscriptsRequireSessionLineage(t *testing.T) {
	store := newMemoryStore()
	e := New(store)
	for _, id := range []string{"independent-a", "independent-b"} {
		in := input(testRequest("task"))
		in.SessionID = id
		x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), textResponse("report"), textResponse("done")}}
		_, err := e.Advance(context.Background(), in, x)
		require.NoError(t, err)
	}
	r := testRequest([]apicompat.ResponsesInputItem{textMessage("user", "task"), textMessage("assistant", "done"), textMessage("user", "next")})
	_, _, err := e.Resolve(context.Background(), Scope(1, 2), "", "", r)
	requireCode(t, err, "session_ambiguous")
	sid, _, err := e.Resolve(context.Background(), Scope(1, 2), "independent-a", "", r)
	require.NoError(t, err)
	require.Equal(t, "independent-a", sid)
}

func TestFusionCancellationStillSettlesKnownPendingUsage(t *testing.T) {
	store := newMemoryStore()
	e := New(store)
	x := &scriptExecutor{t: t, failSettle: true, responses: []*apicompat.ResponsesResponse{delegation("d")}}
	in := input(testRequest("task"))
	_, err := e.Advance(context.Background(), in, x)
	requireCode(t, err, "billing_pending")
	require.NoError(t, e.Cancel(context.Background(), in.Scope, in.SessionID))
	_, err = New(store).Advance(context.Background(), in, x)
	requireCode(t, err, "cancelled")
	require.Len(t, x.calls, 1)
	require.Len(t, x.settled, 1)
	s, _, err := e.Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, Cancelled, s.State)
	require.True(t, s.Attempts[0].Settled)
}
func TestFusionWriteDisconnectStopsRoleScheduling(t *testing.T) {
	store := newMemoryStore()
	e := New(store)
	x := &scriptExecutor{t: t, execute: func(c Call) (Outcome, error) {
		return Outcome{Response: delegation("d"), BillingModel: c.Request.Model, ClientDisconnected: true}, nil
	}}
	in := input(testRequest("task"))
	_, err := e.Advance(context.Background(), in, x)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, x.calls, 1)
	require.Len(t, x.settled, 1)
	x.execute = nil
	x.responses = []*apicompat.ResponsesResponse{textResponse("report"), textResponse("done")}
	_, err = e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Len(t, x.calls, 3)
}

func TestFusionTextOnlyClientsStillGetBothRoles(t *testing.T) {
	e := New(newMemoryStore())
	r := testRequest("analyze this design")
	r.Tools = nil
	r.ToolChoice = json.RawMessage(`"none"`)
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), textResponse("independent design assessment"), textResponse("Lead-reviewed answer")}}
	result, err := e.Advance(context.Background(), input(r), x)
	require.NoError(t, err)
	require.Equal(t, "Lead-reviewed answer", outputText(result.Response))
	require.Len(t, x.calls, 3)
	require.Len(t, x.calls[0].Request.Tools, 1)
	require.Empty(t, x.calls[1].Request.Tools)
}

func TestFusionLeadUsesAutoAndStableToolsAcrossHandoff(t *testing.T) {
	for _, textOnly := range []bool{false, true} {
		r := testRequest("analyze this design")
		if textOnly {
			r.Tools = nil
			r.ToolChoice = json.RawMessage(`"none"`)
		}
		s := &Session{Profile: testProfile(), Role: Lead}
		e := New(newMemoryStore())
		planning, err := e.roleRequest(s, r)
		require.NoError(t, err)
		s.TurnHandoffs = 1
		review, err := e.roleRequest(s, r)
		require.NoError(t, err)
		require.JSONEq(t, `"auto"`, string(planning.ToolChoice), "thinking-enabled members reject forced named tools")
		require.Equal(t, planning.ToolChoice, review.ToolChoice)
		require.Equal(t, planning.Instructions, review.Instructions)
		require.Equal(t, Digest(planning.Tools), Digest(review.Tools), "changing the tool contract loses the Lead cache prefix")
		require.Equal(t, DelegateTool, planning.Tools[len(planning.Tools)-1].Name)
		for _, tool := range r.Tools {
			require.NotEqual(t, DelegateTool, tool.Name, "do not mutate the caller's fixed tool set")
		}
	}
}

func TestFusionHandoffGuidancePreservesStableToolsAndHardLimit(t *testing.T) {
	e := New(newMemoryStore())
	p := testProfile()
	p.MaxHandoffs = 2
	s := &Session{Profile: p, Role: Lead}
	base := testRequest("complete two phases and review")
	planning, err := e.roleRequest(s, base)
	require.NoError(t, err)
	require.Contains(t, planning.Tools[len(planning.Tools)-1].Description, "at most 2 Sidekick handoffs per user turn")
	require.Contains(t, planning.Instructions, "do not request another Sidekick verification or rework")
	s.TurnHandoffs = 2
	review, err := e.roleRequest(s, base)
	require.NoError(t, err)
	require.Equal(t, planning.Instructions, review.Instructions)
	require.Equal(t, Digest(planning.Tools), Digest(review.Tools), "guidance must not remove historical tool definitions or change the cache prefix")

	store := newMemoryStore()
	engine := New(store)
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{
		delegation("phase-1"), textResponse("phase one evidence"),
		delegation("phase-2"), textResponse("phase two evidence"),
		delegation("extra-verification"),
	}}
	in := input(base)
	in.Profile.MaxHandoffs = 2
	_, err = engine.Advance(context.Background(), in, x)
	requireCode(t, err, "budget_exhausted")
	require.Len(t, x.calls, 5, "a third Sidekick must never start")
	require.Len(t, x.settled, 5, "retain usage for the Lead's rejected extra delegation")
	saved, _, err := engine.Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, BudgetExhausted, saved.State)
	require.Equal(t, 2, saved.TurnHandoffs)
}

func TestFusionDelegationReminderIsBilledAndResetPerTurn(t *testing.T) {
	store := newMemoryStore()
	e := New(store)
	x := &scriptExecutor{t: t}
	for turn := 1; turn <= 2; turn++ {
		x.responses = []*apicompat.ResponsesResponse{textResponse("unreviewed draft"), delegation("delegate"), textResponse("independent report"), textResponse("reviewed answer")}
		in := input(testRequest("analyze this design"))
		in.IdempotencyKey = strings.Repeat("turn", turn)
		result, err := e.Advance(context.Background(), in, x)
		require.NoError(t, err)
		require.Equal(t, Completed, result.State)
		require.Equal(t, "reviewed answer", outputText(result.Response))
		require.Equal(t, 400, result.Response.Usage.InputTokens, "include the unpublished draft's actual usage")
		require.Len(t, x.calls, 4*turn)
		require.Len(t, x.settled, 4*turn)
		s, _, err := e.Load(context.Background(), in.Scope, in.SessionID)
		require.NoError(t, err)
		require.Equal(t, 1, s.TurnDelegationReminders)
		require.Equal(t, 1, s.TurnHandoffs)
		replayed, err := New(store).Advance(context.Background(), in, x)
		require.NoError(t, err)
		require.True(t, replayed.Replayed)
		require.Len(t, x.calls, 4*turn)
	}
}

func TestFusionDelegationReminderCannotBypassTurnBudgetOrRepeat(t *testing.T) {
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{textResponse("first draft"), textResponse("second draft")}}
	_, err := New(newMemoryStore()).Advance(context.Background(), input(testRequest("task")), x)
	requireCode(t, err, "delegation_required")
	require.Len(t, x.calls, 2, "a second draft must stop instead of prompting again")
	require.Len(t, x.settled, 2)
	x = &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{textResponse("draft"), delegation("d"), textResponse("report"), textResponse("final")}}
	in := input(testRequest("task"))
	in.Profile.MaxRequests = 3
	_, err = New(newMemoryStore()).Advance(context.Background(), in, x)
	requireCode(t, err, "budget_exhausted")
	require.Len(t, x.calls, 3, "the reminder's inference consumes the ordinary request budget")
	require.Len(t, x.settled, 3)
}

func TestFusionDelegationReminderRecoversWithoutRepeatingDraftInference(t *testing.T) {
	store := newMemoryStore()
	store.failCAS = func(data []byte) bool {
		var s Session
		return json.Unmarshal(data, &s) == nil && s.TurnDelegationReminders == 1
	}
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{textResponse("unreviewed draft"), delegation("d"), textResponse("report"), textResponse("reviewed answer")}}
	in := input(testRequest("task"))
	in.IdempotencyKey = "same-request-after-save-failure"
	_, err := New(store).Advance(context.Background(), in, x)
	require.ErrorContains(t, err, "simulated store interruption")
	require.Len(t, x.calls, 1)
	require.Len(t, x.settled, 1)
	result, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Equal(t, "reviewed answer", outputText(result.Response))
	require.Len(t, x.calls, 4)
	require.Len(t, x.settled, 4)
	s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, 1, s.TurnDelegationReminders)
}
