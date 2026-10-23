package fusion

import (
	"context"
	"sync"
	"time"
)

type cacheWatch struct {
	scope, id, window string
	epoch             uint64
	due, until        time.Time
	interval          time.Duration
	running           bool
}

// CacheWarmScheduler retains IDs only. It has bounded watches/workers, no work
// queue, no missed-tick catch-up and no restart scan that could replay a call.
type CacheWarmScheduler struct {
	controller               *CacheKeepaliveController
	mu                       sync.Mutex
	watches                  map[string]cacheWatch
	workers, capacity, limit int
	ctx                      context.Context
	cancel                   context.CancelFunc
	done                     chan struct{}
	wg                       sync.WaitGroup
}

func NewCacheWarmScheduler(c *CacheKeepaliveController, capacity, limit int) *CacheWarmScheduler {
	return newCacheWarmScheduler(c, max(1, capacity), max(1, limit), time.Second)
}
func newCacheWarmScheduler(c *CacheKeepaliveController, capacity, limit int, resolution time.Duration) *CacheWarmScheduler {
	ctx, cancel := context.WithCancel(context.Background())
	s := &CacheWarmScheduler{controller: c, watches: map[string]cacheWatch{}, capacity: capacity, limit: limit, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		t := time.NewTicker(resolution)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.dispatch()
			}
		}
	}()
	return s
}
func (s *CacheWarmScheduler) Watch(scope, id string, state *CacheControl, until time.Time, interval time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := cacheKey(scope, id)
	if s.ctx.Err() != nil {
		return
	}
	if state.State != "ARMED" && state.State != "IN_FLIGHT" {
		delete(s.watches, key)
		return
	}
	previous, exists := s.watches[key]
	if !exists && len(s.watches) >= s.limit {
		return
	}
	next := cacheWatch{scope: scope, id: id, epoch: state.Epoch, window: state.WindowID, due: state.NextDueAt, until: until, interval: interval}
	if exists && previous.epoch == next.epoch && previous.window == next.window {
		next.due = maxTime(next.due, previous.due)
		next.running = previous.running
	}
	s.watches[key] = next
}
func (s *CacheWarmScheduler) Forget(scope, id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.watches, cacheKey(scope, id))
	s.mu.Unlock()
}
func (s *CacheWarmScheduler) dispatch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	now := s.controller.now()
	for key, w := range s.watches {
		expired := !now.Before(w.until)
		if w.running || (!expired && now.Before(w.due)) || s.workers >= s.capacity {
			continue
		}
		w.running = true
		w.due = now.Add(w.interval)
		s.watches[key] = w
		s.workers++
		s.wg.Add(1)
		go func(key string, w cacheWatch) {
			defer s.wg.Done()
			// Tick journals/settles even on cancellation. All execution is bounded.
			_ = s.controller.Tick(s.ctx, w.scope, w.id, w.epoch, w.window)
			s.mu.Lock()
			s.workers--
			if current, ok := s.watches[key]; ok && current.epoch == w.epoch && current.window == w.window {
				// Session expiry, a shorter turn deadline, or unavailable state must
				// not turn an expired watch into a busy loop. Never remove a new epoch.
				if !s.controller.now().Before(w.until) {
					delete(s.watches, key)
				} else {
					current.running = false
					s.watches[key] = current
				}
			}
			s.mu.Unlock()
		}(key, w)
	}
}
func (s *CacheWarmScheduler) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.cancel()
	<-s.done
	// dispatch has stopped, so Add cannot race Wait.
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
