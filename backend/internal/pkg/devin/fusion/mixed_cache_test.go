package fusion

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestFusionQueuedHandoffsKeepLeadCacheAndSharedBudgetAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lead      *apicompat.ResponsesResponse
		responses []*apicompat.ResponsesResponse
		public    []string
	}{
		{"client before delegation", mixedTools(tool("lead-read", "read_file", `{}`), delegation("d1")),
			[]*apicompat.ResponsesResponse{tool("worker-1", "read_file", `{}`), textResponse("first report"), textResponse("reviewed")},
			[]string{"lead-read", "worker-1"}},
		{"consecutive delegations", mixedTools(delegation("d1"), delegation("d2")),
			[]*apicompat.ResponsesResponse{tool("worker-1", "read_file", `{}`), textResponse("first report"), tool("worker-2", "read_file", `{}`), textResponse("second report"), textResponse("reviewed")},
			[]string{"worker-1", "worker-2"}},
		{"client between delegations", mixedTools(delegation("d1"), tool("lead-read", "read_file", `{}`), delegation("d2")),
			[]*apicompat.ResponsesResponse{tool("worker-1", "read_file", `{}`), textResponse("first report"), tool("worker-2", "read_file", `{}`), textResponse("second report"), textResponse("reviewed")},
			[]string{"worker-1", "lead-read", "worker-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			clock := newCacheClock()
			fake := &refreshFake{supported: true, result: RefreshResult{RequestSucceeded: true, CacheConfirmed: true, Outcome: Outcome{Response: &apicompat.ResponsesResponse{Status: "completed", Usage: &apicompat.ResponsesUsage{InputTokens: 40, TotalTokens: 40}}, BillingModel: "lead-real"}}}
			engine := func() (*Engine, *CacheKeepaliveController) {
				cache := NewCacheKeepaliveController(store)
				cache.Now, cache.Adapter = clock.Now, fake
				e := New(store)
				e.Now, e.Cache = clock.Now, cache
				return e, cache
			}
			x := &scriptExecutor{t: t, responses: append([]*apicompat.ResponsesResponse{tc.lead}, tc.responses...)}
			in := input(testRequest("complete original task with worker evidence"))
			in.Profile.Keepalive = &KeepalivePolicy{Enabled: true, IntervalSeconds: 10, MaxWindowSeconds: 100, MaxAttempts: 2, MaxCostUSD: .5, TimeoutSeconds: 5}
			in.IdempotencyKey = "initial"
			var started time.Time
			var prefix, priorWindow string
			var priorEpoch uint64
			warmed := 0
			for step := 0; ; step++ {
				e, cache := engine() // Restore candidate, queue and counters each step.
				response, err := e.Advance(context.Background(), in, x)
				require.NoError(t, err)
				s, _, err := e.Load(context.Background(), in.Scope, in.SessionID)
				require.NoError(t, err)
				if started.IsZero() {
					started = s.TurnStarted
				}
				require.Equal(t, 1, s.Turn)
				require.Equal(t, started, s.TurnStarted)
				state, _, err := cache.Load(context.Background(), in.Scope, in.SessionID)
				require.NoError(t, err)
				require.Equal(t, warmed, state.Attempts)
				require.InDelta(t, float64(warmed)*.04, state.SpentUSD, 1e-10)
				if response.State == Completed {
					require.Equal(t, len(tc.public), step)
					require.Equal(t, "STOPPED", state.State)
					require.Nil(t, state.Snapshot)
					break
				}
				require.Less(t, step, len(tc.public))
				require.Equal(t, tc.public[step], s.Pending.InternalID)
				require.NotNil(t, state.Snapshot)
				require.Equal(t, s.Attempts[0].ID, state.Snapshot.SourceAttemptID)
				if prefix == "" {
					prefix = state.Snapshot.PrefixDigest
				}
				require.Equal(t, prefix, state.Snapshot.PrefixDigest)
				if s.Pending.Role == Lead {
					require.Equal(t, "NO_SNAPSHOT", state.State, "park the candidate without warming while the client owns a Lead tool")
				} else {
					require.Equal(t, "ARMED", state.State)
					require.Equal(t, s.Handoffs[len(s.Handoffs)-1].ID, state.WindowID)
					require.False(t, state.WindowDeadline.After(started.Add(time.Duration(in.Profile.MaxWallSeconds)*time.Second)))
					if priorWindow != "" {
						require.NotEqual(t, priorWindow, state.WindowID)
						require.Greater(t, state.Epoch, priorEpoch)
						require.NoError(t, cache.Tick(context.Background(), in.Scope, in.SessionID, priorEpoch, priorWindow))
						require.Equal(t, warmed, fake.calls, "old-window callbacks cannot spend in the new handoff")
					}
					priorEpoch, priorWindow = state.Epoch, state.WindowID
					clock.Advance(state.NextDueAt.Sub(clock.Now()))
					require.NoError(t, cache.Tick(context.Background(), in.Scope, in.SessionID, state.Epoch, state.WindowID))
					warmed++
					require.Equal(t, warmed, fake.calls)
				}
				t.Logf("step=%d role=%s native_id=%s handoff=%s cache=%s warmed=%d", step, s.Pending.Role, s.Pending.InternalID, s.Pending.HandoffID, state.State, warmed)
				in.Request = resultRequest(response.Response.Output[0].CallID, "tool evidence")
				in.IdempotencyKey = "result-" + s.Pending.InternalID
			}
			e, _ := engine()
			s, _, err := e.Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			raw, err := store.Get(context.Background(), budgetKey(s, s.Turn))
			require.NoError(t, err)
			var ledger turnBudget
			require.NoError(t, json.Unmarshal(raw, &ledger))
			purposeCounts := map[Purpose]int{}
			spent := ledger.LegacySpent
			for _, entry := range ledger.Entries {
				require.True(t, entry.Settled)
				purposeCounts[entry.Purpose]++
				spent += entry.Amount
			}
			require.Equal(t, len(x.calls), purposeCounts[Inference])
			require.Equal(t, warmed, purposeCounts[CacheRefresh])
			require.Len(t, fake.billed, warmed)
			require.InDelta(t, x.charged+fake.charged, spent, 1e-10)
			require.LessOrEqual(t, spent, in.Profile.MaxCostUSD)
		})
	}
}

func TestFusionQueuedHandoffFencesLateRefreshWithoutResettingCost(t *testing.T) {
	e, s, cache, clock, fake := cacheFixture(t)
	fake.started, fake.finish = make(chan struct{}), make(chan struct{})
	state, _, err := cache.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	clock.Advance(10 * time.Second)
	done := make(chan error, 1)
	go func() { done <- cache.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID) }()
	<-fake.started
	s.Handoffs[len(s.Handoffs)-1].Report = "first report"
	s.Handoffs = append(s.Handoffs, Handoff{ID: "next-queued-handoff", CallID: "d2", Turn: s.Turn, Brief: `{}`})
	_, raw, err := e.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.NoError(t, e.save(context.Background(), s, &raw))
	require.NoError(t, cache.Observe(context.Background(), s))
	fresh, _, err := cache.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, "ARMED", fresh.State)
	require.Equal(t, "next-queued-handoff", fresh.WindowID)
	require.Greater(t, fresh.Epoch, state.Epoch)
	require.Equal(t, state.Snapshot.PrefixDigest, fresh.Snapshot.PrefixDigest)
	close(fake.finish)
	require.NoError(t, <-done)
	after, _, err := cache.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, fresh.Epoch, after.Epoch)
	require.Equal(t, fresh.WindowID, after.WindowID)
	require.Equal(t, fresh.NextDueAt, after.NextDueAt)
	require.Equal(t, 1, after.Attempts)
	require.InDelta(t, .04, after.SpentUSD, 1e-10)
	require.Empty(t, after.Pending)
	require.Len(t, fake.billed, 1)
}

func TestFusionQueuedHandoffDoesNotReviveStoppedCache(t *testing.T) {
	for _, reason := range []string{"attempt_unknown", "budget_exhausted", "refresh_protocol_unverified"} {
		t.Run(reason, func(t *testing.T) {
			e, s, cache, _, fake := cacheFixture(t)
			require.NoError(t, cache.update(context.Background(), s, func(state *CacheControl) {
				state.State, state.StopReason = "STOPPED", reason
			}))
			s.Handoffs[len(s.Handoffs)-1].Report = "first report"
			s.Handoffs = append(s.Handoffs, Handoff{ID: "next-queued-handoff", CallID: "d2", Turn: s.Turn})
			_, raw, err := e.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			require.NoError(t, e.save(context.Background(), s, &raw))
			require.NoError(t, cache.Observe(context.Background(), s))
			state, _, err := cache.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			require.Equal(t, "STOPPED", state.State)
			require.Equal(t, reason, state.StopReason)
			require.Zero(t, fake.calls)
		})
	}
}
