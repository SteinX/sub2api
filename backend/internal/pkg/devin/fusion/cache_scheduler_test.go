package fusion

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFusionWarmTimerNoCatchupAndShutdown(t *testing.T) {
	_, s, c, clock, f := cacheFixture(t)
	c.Scheduler = newCacheWarmScheduler(c, 1, 1, time.Millisecond)
	t.Cleanup(func() { require.NoError(t, c.Scheduler.Close(context.Background())) })
	c.schedule(context.Background(), s)
	clock.Advance(60 * time.Second)
	require.Eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.calls == 1 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		state, _, _ := c.Load(context.Background(), s.Scope, s.ID)
		return state != nil && state.State == "ARMED"
	}, time.Second, time.Millisecond)
	state, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, clock.Now().Add(10*time.Second), state.NextDueAt)
	require.NoError(t, c.Scheduler.Close(context.Background()))
	clock.Advance(20 * time.Second)
	c.Scheduler.dispatch()
	f.mu.Lock()
	require.Equal(t, 1, f.calls)
	f.mu.Unlock()
}

type cancelRefresh struct {
	refreshFake
	entered chan struct{}
	exited  atomic.Bool
}

func (f *cancelRefresh) Refresh(ctx context.Context, _ CacheSnapshot, _ Attempt) (RefreshResult, error) {
	close(f.entered)
	<-ctx.Done()
	f.exited.Store(true)
	return RefreshResult{}, ctx.Err()
}
func TestFusionWarmCancellationAcrossControllersAndShutdown(t *testing.T) {
	for _, mode := range []string{"local stop", "other process", "shutdown", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			_, s, c, clock, _ := cacheFixture(t)
			f := &cancelRefresh{entered: make(chan struct{})}
			f.supported = true
			c.Adapter = f
			if mode == "timeout" {
				s.Profile.Keepalive.TimeoutSeconds = 1
				s.ProfileRevision = s.Profile.Revision()
				e := New(c.Store)
				_, raw, err := e.Load(context.Background(), s.Scope, s.ID)
				require.NoError(t, err)
				require.NoError(t, e.save(context.Background(), s, &raw))
				req := testRequest("new")
				req.Model = s.Profile.LeadModel
				require.NoError(t, c.Capture(context.Background(), s, req, "next", "route"))
			}
			c.Scheduler = newCacheWarmScheduler(c, 1, 1, time.Millisecond)
			t.Cleanup(func() { require.NoError(t, c.Scheduler.Close(context.Background())) })
			c.schedule(context.Background(), s)
			clock.Advance(10 * time.Second)
			select {
			case <-f.entered:
			case <-time.After(time.Second):
				t.Fatal("timer did not dispatch")
			}
			switch mode {
			case "local stop":
				require.NoError(t, c.Stop(context.Background(), s, "foreground_lead"))
			case "other process":
				other := NewCacheKeepaliveController(c.Store)
				require.NoError(t, other.Stop(context.Background(), s, "cancelled"))
			case "shutdown":
				require.NoError(t, c.Scheduler.Close(context.Background()))
			}
			require.Eventually(t, f.exited.Load, 2*time.Second, time.Millisecond)
			require.Eventually(t, func() bool {
				state, _, _ := c.Load(context.Background(), s.Scope, s.ID)
				return state != nil && len(state.Pending) == 1 && state.ReservedUSD > 0
			}, time.Second, time.Millisecond)
			state, _, err := c.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			require.Equal(t, 1, state.Attempts, "unknown network outcomes cannot be retried")
		})
	}
}

func TestFusionWarmTimerBoundsWorkersAndWatches(t *testing.T) {
	_, s, c, clock, _ := cacheFixture(t)
	f := &cancelRefresh{entered: make(chan struct{})}
	f.supported = true
	c.Adapter = f
	c.Scheduler = newCacheWarmScheduler(c, 1, 1, time.Millisecond)
	t.Cleanup(func() { require.NoError(t, c.Scheduler.Close(context.Background())) })
	state, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	c.schedule(context.Background(), s)
	c.Scheduler.Watch("other", "id", state, clock.Now().Add(time.Hour), time.Second)
	c.Scheduler.mu.Lock()
	require.Len(t, c.Scheduler.watches, 1)
	c.Scheduler.mu.Unlock()
	clock.Advance(10 * time.Second)
	<-f.entered
	for range 10 {
		c.Scheduler.dispatch()
	}
	c.Scheduler.mu.Lock()
	require.Equal(t, 1, c.Scheduler.workers)
	c.Scheduler.mu.Unlock()
	require.NoError(t, c.Scheduler.Close(context.Background()))
}

func TestFusionWarmTimerExpiresWithoutSending(t *testing.T) {
	_, s, c, clock, f := cacheFixture(t)
	c.Scheduler = newCacheWarmScheduler(c, 1, 1, time.Millisecond)
	t.Cleanup(func() { require.NoError(t, c.Scheduler.Close(context.Background())) })
	c.schedule(context.Background(), s)
	clock.Advance(110 * time.Second)
	require.Eventually(t, func() bool {
		state, _, err := c.Load(context.Background(), s.Scope, s.ID)
		return err == nil && state.State == "STOPPED"
	}, time.Second, time.Millisecond)
	require.NoError(t, c.Scheduler.Close(context.Background()))
	require.Zero(t, f.calls)
}
