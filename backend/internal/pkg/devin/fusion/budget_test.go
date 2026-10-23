package fusion

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestFusionTurnBudgetConcurrentPurposes(t *testing.T) {
	e := New(newMemoryStore())
	s := &Session{ID: "shared", Scope: Scope(1, 2), Turn: 1, Profile: testProfile(), ExpiresAt: time.Now().Add(time.Hour)}
	s.Profile.MaxCostUSD = 1
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for n := range 60 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			purpose := []Purpose{Inference, Compaction, CacheRefresh}[n%3]
			if e.reserveBudget(context.Background(), s, fmt.Sprint(n), purpose, .125) == nil {
				admitted.Add(1)
			}
		}(n)
	}
	wg.Wait()
	require.EqualValues(t, 8, admitted.Load(), "all purposes must compete atomically")
}
func TestFusionTurnBudgetUnknownSettlementAndTurnIsolation(t *testing.T) {
	e := New(newMemoryStore())
	s := &Session{ID: "unknown", Scope: Scope(1, 2), Turn: 1, Profile: testProfile(), ExpiresAt: time.Now().Add(time.Hour)}
	s.Profile.MaxCostUSD = 1
	require.NoError(t, e.reserveBudget(context.Background(), s, "warm", CacheRefresh, .7))
	require.Error(t, e.reserveBudget(context.Background(), s, "foreground", Inference, .4))
	require.NoError(t, e.settleBudget(context.Background(), s, Attempt{ID: "warm", Turn: 1, Purpose: CacheRefresh}))
	require.Error(t, e.reserveBudget(context.Background(), s, "summary", Compaction, .4), "unknown must keep its estimate")
	s.Turn = 2
	require.NoError(t, e.reserveBudget(context.Background(), s, "new", Inference, .9))
	a := Attempt{ID: "warm", Turn: 1, Purpose: CacheRefresh, Settled: true, CostUSD: .1}
	require.NoError(t, e.settleBudget(context.Background(), s, a))
	require.NoError(t, e.settleBudget(context.Background(), s, a))
	require.Error(t, e.reserveBudget(context.Background(), s, "new-summary", Compaction, .2), "late settlement must not release a new turn's budget")
	s.Turn = 1
	require.NoError(t, e.reserveBudget(context.Background(), s, "summary", Compaction, .4))
}
func TestFusionWarmReservationBlocksInferenceAndCompaction(t *testing.T) {
	t.Run("inference", func(t *testing.T) {
		e := New(newMemoryStore())
		p := testProfile()
		p.MaxCostUSD = 1.05
		s := &Session{ID: "new-session", Scope: Scope(1, 2), Turn: 1, Profile: p, ExpiresAt: time.Now().Add(time.Hour)}
		require.NoError(t, e.reserveBudget(context.Background(), s, "unresolved-warm", CacheRefresh, .1))
		x := &scriptExecutor{t: t}
		_, err := e.Advance(context.Background(), Input{Scope: s.Scope, SessionID: s.ID, AccountID: 7, Profile: p, Request: testRequest("task")}, x)
		require.ErrorContains(t, err, "shared Fusion turn cost budget")
		require.Empty(t, x.calls)
	})
	t.Run("compaction", func(t *testing.T) {
		e, s, raw, req := summarySession(t, Sidekick)
		s.Profile.MaxCostUSD = .15
		s.ProfileRevision = s.Profile.Revision()
		require.NoError(t, e.save(context.Background(), s, &raw))
		require.NoError(t, e.reserveBudget(context.Background(), s, "unresolved-warm", CacheRefresh, .1))
		x := &summaryExecutor{}
		_, err := e.prepareCompaction(context.Background(), s, &raw, req, x, false)
		require.ErrorContains(t, err, "shared Fusion turn cost budget")
		require.Empty(t, x.calls)
	})
}

func TestFusionUnsentInferenceReleasesBudgetAfterSaveFailure(t *testing.T) {
	store := newMemoryStore()
	e := New(store)
	store.failCAS = func(next []byte) bool {
		var s Session
		return json.Unmarshal(next, &s) == nil && len(s.Attempts) == 1
	}
	in := input(testRequest("task"))
	in.Profile.MaxCostUSD = 1.05
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{tool("inspect", "read_file", `{}`)}}
	_, err := e.Advance(context.Background(), in, x)
	require.ErrorContains(t, err, "store interruption")
	require.Empty(t, x.calls)
	_, err = e.Advance(context.Background(), in, x)
	require.NoError(t, err, "an unsent attempt must not consume the retry's budget")
	require.Len(t, x.calls, 1)
}

func TestFusionUnsentCompactionReleasesBudgetAfterPreparationFailure(t *testing.T) {
	for _, stage := range []string{"journal", "session"} {
		t.Run(stage, func(t *testing.T) {
			e, s, raw, req := summarySession(t, Sidekick)
			s.Profile.MaxCostUSD = .1
			s.ProfileRevision = s.Profile.Revision()
			require.NoError(t, e.save(context.Background(), s, &raw))
			store, ok := e.Store.(*memoryStore)
			require.True(t, ok)
			store.failCAS = func(next []byte) bool {
				if stage == "journal" {
					var r compactionRecord
					return json.Unmarshal(next, &r) == nil && r.Attempt.Status == "prepared"
				}
				var candidate Session
				return json.Unmarshal(next, &candidate) == nil && candidate.AuxReservedUSD > 0
			}
			x := &summaryExecutor{response: summaryResponse()}
			_, err := e.prepareCompaction(context.Background(), s, &raw, req, x, false)
			require.ErrorContains(t, err, "store interruption")
			require.Empty(t, x.calls)
			require.Zero(t, s.AuxReservedUSD)
			require.Nil(t, s.Contexts[Sidekick].Job)
			_, err = e.prepareCompaction(context.Background(), s, &raw, req, x, false)
			require.NoError(t, err)
			require.Len(t, x.calls, 1)
		})
	}
}
