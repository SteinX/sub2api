package fusion

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
