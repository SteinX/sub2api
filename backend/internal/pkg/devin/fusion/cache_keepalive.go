package fusion

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/google/uuid"
)

// Both a bounded profile policy and a separately enabled verified adapter are
// required. The gateway process switch defaults to false.
type KeepalivePolicy struct {
	Enabled          bool    `json:"enabled"`
	IntervalSeconds  int     `json:"interval_seconds"`
	MaxWindowSeconds int     `json:"max_window_seconds"`
	MaxAttempts      int     `json:"max_attempts_per_turn"`
	MaxCostUSD       float64 `json:"max_cost_usd_per_turn"`
	TimeoutSeconds   int     `json:"timeout_seconds"`
}

func (p Profile) KeepalivePolicy() KeepalivePolicy {
	if p.Keepalive != nil {
		return *p.Keepalive
	}
	return KeepalivePolicy{}
}
func (p KeepalivePolicy) Validate() error {
	if !p.Enabled {
		return nil
	}
	if p.IntervalSeconds < 1 || p.IntervalSeconds > 3600 || p.MaxWindowSeconds < p.IntervalSeconds || p.MaxWindowSeconds > 3600 || p.MaxAttempts < 1 || p.MaxAttempts > 16 || !validCost(p.MaxCostUSD) || p.MaxCostUSD <= 0 || p.MaxCostUSD > 5 || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 60 {
		return E("keepalive_invalid", "cache refresh needs explicit bounded interval, window, attempts, timeout and cost", 400)
	}
	return nil
}

const CacheRefreshShape = "devin-full-prefix-output1-v1"

type CacheIdentity struct {
	UserID          int64  `json:"user_id"`
	APIKeyID        int64  `json:"api_key_id"`
	SubscriptionID  int64  `json:"subscription_id,omitempty"`
	GroupID         int64  `json:"group_id,omitempty"`
	InboundEndpoint string `json:"inbound_endpoint"`
}

type CacheSnapshot struct {
	ShapeRevision   string                      `json:"shape_revision"`
	Profile         Profile                     `json:"profile"`
	Identity        *CacheIdentity              `json:"identity,omitempty"`
	LaneScope       string                      `json:"lane_scope"`
	Scope           string                      `json:"scope"`
	SessionID       string                      `json:"session_id"`
	AccountID       int64                       `json:"account_id"`
	RouteIdentity   string                      `json:"route_identity"`
	Model           string                      `json:"model"`
	ProfileRevision string                      `json:"profile_revision"`
	ContextEpoch    uint64                      `json:"context_epoch"`
	SourceAttemptID string                      `json:"source_attempt_id"`
	PrefixDigest    string                      `json:"prefix_digest"`
	ContractDigest  string                      `json:"contract_digest"`
	Request         *apicompat.ResponsesRequest `json:"request"`
}
type CacheControl struct {
	Epoch                uint64          `json:"epoch"`
	Turn                 int             `json:"turn"`
	State                string          `json:"state"`
	StopReason           string          `json:"stop_reason,omitempty"`
	Snapshot             *CacheSnapshot  `json:"snapshot,omitempty"`
	WindowID             string          `json:"window_id,omitempty"`
	WindowStartedAt      time.Time       `json:"window_started_at"`
	NextDueAt            time.Time       `json:"next_due_at"`
	LastAttemptAt        time.Time       `json:"last_attempt_at"`
	LastRequestSuccessAt time.Time       `json:"last_request_success_at"`
	LastCacheConfirmedAt time.Time       `json:"last_cache_confirmed_at"`
	Attempts             int             `json:"attempts"`
	SpentUSD             float64         `json:"spent_usd"`
	ReservedUSD          float64         `json:"reserved_usd"`
	Pending              []string        `json:"pending,omitempty"`
	InFlightID           string          `json:"in_flight_id,omitempty"`
	Accounted            map[string]bool `json:"accounted,omitempty"`
	CumulativeCostUSD    float64         `json:"cumulative_cost_usd"`
}
type RefreshResult struct {
	// Observed stats without a terminal stop are retained for audit, not treated as a final bill.
	UsageUncertain   bool
	Outcome          Outcome
	RequestSucceeded bool
	CacheConfirmed   bool
}

// Only an implementation backed by request/response fixtures and verified
// non-mutating provider semantics may return true from Supports. It owns
// low-priority capacity, fresh authentication and strict monetary reservation.
type VerifiedRefreshAdapter interface {
	Supports(CacheSnapshot) bool
	Acquire(context.Context, CacheSnapshot) (release func(), acquired bool)
	Estimate(context.Context, CacheSnapshot) (float64, error)
	Refresh(context.Context, CacheSnapshot, Attempt) (RefreshResult, error)
	Settle(context.Context, CacheSnapshot, Attempt) (float64, error)
}
type CacheKeepaliveController struct {
	Store     AtomicStore
	Now       func() time.Time
	Adapter   VerifiedRefreshAdapter
	Scheduler *CacheWarmScheduler
	mu        sync.Mutex
	cancels   map[string]context.CancelFunc
}

func NewCacheKeepaliveController(store devin.SessionStore) *CacheKeepaliveController {
	atomic, _ := store.(AtomicStore)
	return &CacheKeepaliveController{Store: atomic, Now: time.Now, cancels: map[string]context.CancelFunc{}}
}
func (c *CacheKeepaliveController) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
func cacheKey(scope, id string) string { return indexKey(scope, "lead_cache", id) }
func (c *CacheKeepaliveController) Load(ctx context.Context, scope, id string) (*CacheControl, []byte, error) {
	if c == nil || c.Store == nil {
		return nil, nil, E("store_unavailable", "cache lifecycle requires shared state", 503)
	}
	b, err := c.Store.Get(ctx, cacheKey(scope, id))
	if errors.Is(err, devin.ErrSessionNotFound) {
		return &CacheControl{State: "NO_SNAPSHOT"}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var state CacheControl
	if json.Unmarshal(b, &state) != nil {
		return nil, nil, E("session_corrupt", "invalid cache lifecycle state", 503)
	}
	return &state, b, nil
}
func (c *CacheKeepaliveController) update(ctx context.Context, s *Session, fn func(*CacheControl)) error {
	if c == nil || c.Store == nil {
		return nil
	}
	for n := 0; n < 8; n++ {
		state, old, err := c.Load(ctx, s.Scope, s.ID)
		if err != nil {
			return err
		}
		fn(state)
		b, _ := json.Marshal(state)
		ok, err := c.Store.CompareAndSwap(ctx, cacheKey(s.Scope, s.ID), old, b, max(s.ExpiresAt.Sub(c.now()), time.Second))
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return E("revision_conflict", "cache lifecycle changed concurrently", 409)
}
func (c *CacheKeepaliveController) schedule(ctx context.Context, s *Session) {
	if c.Scheduler == nil {
		return
	}
	state, _, err := c.Load(ctx, s.Scope, s.ID)
	if err != nil {
		c.Scheduler.Forget(s.Scope, s.ID)
		return
	}
	p := s.Profile.KeepalivePolicy()
	until := minTime(s.ExpiresAt, s.TurnStarted.Add(time.Duration(s.Profile.MaxWallSeconds)*time.Second))
	until = minTime(until, state.WindowStartedAt.Add(time.Duration(p.MaxWindowSeconds)*time.Second))
	c.Scheduler.Watch(s.Scope, s.ID, state, until, time.Duration(max(1, p.IntervalSeconds))*time.Second)
}

func (c *CacheKeepaliveController) Stop(ctx context.Context, s *Session, reason string) error {
	if c == nil {
		return nil
	}
	c.Scheduler.Forget(s.Scope, s.ID)
	c.mu.Lock()
	if cancel := c.cancels[cacheKey(s.Scope, s.ID)]; cancel != nil {
		cancel()
	}
	c.mu.Unlock()
	return c.update(ctx, s, func(state *CacheControl) {
		state.Epoch++
		state.State = "STOPPED"
		state.StopReason = reason
		state.Snapshot = nil
		state.InFlightID = ""
	})
}
func activeSidekick(s *Session) bool {
	if len(s.Handoffs) == 0 || s.Handoffs[len(s.Handoffs)-1].Turn != s.Turn || s.Handoffs[len(s.Handoffs)-1].Report != "" {
		return false
	}
	return s.Role == Sidekick && (s.State == RunningSidekick || s.State == WaitingTool && s.Pending != nil && s.Pending.Role == Sidekick)
}
func (c *CacheKeepaliveController) Capture(ctx context.Context, s *Session, request *apicompat.ResponsesRequest, attemptID, routeIdentity string, identity ...CacheIdentity) error {
	if c == nil {
		return nil
	}
	if request == nil || request.Model != s.Profile.LeadModel || len(request.Input) == 0 || attemptID == "" {
		return c.Stop(ctx, s, "snapshot_unavailable")
	}
	// Serialize once to detach maps/slices from the foreground request.
	b, _ := json.Marshal(request)
	var immutable apicompat.ResponsesRequest
	if json.Unmarshal(b, &immutable) != nil {
		return E("input_invalid", "cannot capture cache candidate", 400)
	}
	snapshot := &CacheSnapshot{ShapeRevision: CacheRefreshShape, Profile: s.Profile, LaneScope: s.LaneScope(Lead), Scope: s.Scope, SessionID: s.ID, AccountID: s.AccountID, RouteIdentity: routeIdentity, Model: s.Profile.LeadModel, ProfileRevision: s.ProfileRevision, ContextEpoch: s.roleContext(Lead).Epoch, SourceAttemptID: attemptID, PrefixDigest: Digest(immutable.Input), ContractDigest: s.Contract, Request: &immutable}
	if len(identity) > 0 {
		snapshot.Identity = &identity[0]
	}
	defer c.schedule(ctx, s)
	return c.update(ctx, s, func(state *CacheControl) {
		if state.Turn != s.Turn {
			state.Turn = s.Turn
			state.Attempts = 0
			state.SpentUSD = 0
			state.ReservedUSD = 0
		}
		state.Epoch++
		state.Snapshot = snapshot
		state.LastCacheConfirmedAt = time.Time{}
		state.InFlightID = ""
		state.State = "NO_SNAPSHOT"
		state.StopReason = "no_active_handoff"
		if !s.Profile.KeepalivePolicy().Enabled {
			state.State = "DISABLED"
			state.StopReason = "profile_disabled"
			snapshot.Request = nil
			return
		}
		if c.Adapter == nil || !c.Adapter.Supports(*snapshot) {
			state.State = "DISABLED"
			state.StopReason = "refresh_protocol_unverified"
			snapshot.Request = nil
			return
		}
		if !activeSidekick(s) {
			return
		}
		state.State = "ARMED"
		state.StopReason = ""
		state.WindowID = s.Handoffs[len(s.Handoffs)-1].ID
		state.WindowStartedAt = c.now()
		state.NextDueAt = c.now().Add(time.Duration(s.Profile.KeepalivePolicy().IntervalSeconds) * time.Second)
	})
}

// Observe preserves a Lead snapshot/epoch/due time across Sidekick-only progress.
func (c *CacheKeepaliveController) Observe(ctx context.Context, s *Session) error {
	if c == nil {
		return nil
	}
	if err := c.Resume(ctx, s); err != nil {
		return err
	}
	if !activeSidekick(s) {
		return c.Stop(ctx, s, "no_active_handoff")
	}
	defer c.schedule(ctx, s)
	return c.update(ctx, s, func(state *CacheControl) {
		if state.State != "NO_SNAPSHOT" || state.Snapshot == nil || !s.Profile.KeepalivePolicy().Enabled || c.Adapter == nil || !c.Adapter.Supports(*state.Snapshot) {
			return
		}
		state.State = "ARMED"
		state.StopReason = ""
		state.WindowID = s.Handoffs[len(s.Handoffs)-1].ID
		state.WindowStartedAt = c.now()
		state.NextDueAt = c.now().Add(time.Duration(s.Profile.KeepalivePolicy().IntervalSeconds) * time.Second)
	})
}

type refreshRecord struct {
	Snapshot         CacheSnapshot `json:"snapshot"`
	Attempt          Attempt       `json:"attempt"`
	Epoch            uint64        `json:"epoch"`
	WindowID         string        `json:"window_id"`
	Owner            string        `json:"owner"`
	RequestSucceeded bool          `json:"request_succeeded"`
	CacheConfirmed   bool          `json:"cache_confirmed"`
	Reason           string        `json:"reason,omitempty"`
}

func (c *CacheKeepaliveController) writeRefresh(ctx context.Context, s *Session, r *refreshRecord) error {
	b, _ := json.Marshal(r)
	return c.Store.Set(ctx, indexKey(s.Scope, "cache_refresh", s.ID+":"+r.Attempt.ID), b, max(s.ExpiresAt.Sub(c.now())+24*time.Hour, time.Hour))
}

// Tick is fenced by a shared lease and epoch; the bounded scheduler never
// catches up missed ticks. A zero-output terminal response still incurs usage.
func (c *CacheKeepaliveController) Tick(ctx context.Context, scope, id string, epoch uint64, window string) error {
	if c == nil || c.Adapter == nil {
		return nil
	}
	// Also bound store/billing recovery before a profile-specific send deadline
	// is available. Settlement after a send has its own 30-second drain.
	ctx, finishTick := context.WithTimeout(ctx, 95*time.Second)
	defer finishTick()
	engine := New(c.Store)
	engine.Now = c.Now
	s, _, err := engine.Load(ctx, scope, id)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		c.schedule(cleanup, s)
	}()
	if err = c.Resume(ctx, s); err != nil {
		return err
	}
	if cancelled, err := engine.cancelled(ctx, s); err != nil {
		return err
	} else if cancelled {
		return c.Stop(ctx, s, "cancelled")
	}
	state, old, err := c.Load(ctx, scope, id)
	if err != nil {
		return err
	}
	p := s.Profile.KeepalivePolicy()
	if !p.Enabled || !activeSidekick(s) || state.State != "ARMED" || len(state.Pending) > 0 || state.Epoch != epoch || state.WindowID != window || state.Snapshot == nil || !c.Adapter.Supports(*state.Snapshot) {
		return nil
	}
	contextEpoch := uint64(0)
	if current := s.Contexts[Lead]; current != nil {
		contextEpoch = current.Epoch
	}
	snapshot := state.Snapshot
	if snapshot.Scope != scope || snapshot.SessionID != id || snapshot.AccountID != s.AccountID || snapshot.Model != s.Profile.LeadModel || snapshot.ProfileRevision != s.ProfileRevision || snapshot.ContextEpoch != contextEpoch || snapshot.ContractDigest != s.Contract || state.WindowID != s.Handoffs[len(s.Handoffs)-1].ID {
		return c.Stop(ctx, s, "snapshot_invalid")
	}
	if c.now().Before(state.NextDueAt) {
		return nil
	}
	if c.now().Sub(state.WindowStartedAt) >= time.Duration(p.MaxWindowSeconds)*time.Second || state.Attempts >= p.MaxAttempts || state.SpentUSD+state.ReservedUSD >= p.MaxCostUSD {
		return c.Stop(ctx, s, "window_or_budget_exhausted")
	}
	remaining := min(s.TurnStarted.Add(time.Duration(s.Profile.MaxWallSeconds)*time.Second).Sub(c.now()), time.Duration(p.TimeoutSeconds)*time.Second, state.WindowStartedAt.Add(time.Duration(p.MaxWindowSeconds)*time.Second).Sub(c.now()), s.ExpiresAt.Sub(c.now()))
	tickCtx, tickCancel := context.WithTimeout(ctx, remaining)
	defer tickCancel()
	ctx = tickCtx
	unlock, ok, err := c.Store.Acquire(ctx, cacheKey(scope, id)+":refresh_lock", time.Duration(p.TimeoutSeconds+5)*time.Second)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer unlock()
	release, ok := c.Adapter.Acquire(ctx, *state.Snapshot)
	if !ok {
		return nil
	}
	if release == nil {
		release = func() {}
	}
	defer release()
	estimate, err := c.Adapter.Estimate(ctx, *state.Snapshot)
	if err != nil {
		return err
	}
	if !validCost(estimate) || state.SpentUSD+state.ReservedUSD+estimate > p.MaxCostUSD {
		return c.Stop(ctx, s, "cost_budget_exhausted")
	}
	owner := uuid.NewString()
	attemptID := uuid.NewString()
	if err = engine.reserveBudget(ctx, s, attemptID, CacheRefresh, estimate); err != nil {
		return err
	}
	// A failed claim is known not to have sent anything.
	claimedBudget := false
	defer func() {
		if !claimedBudget {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			_ = engine.settleBudget(cleanup, s, Attempt{ID: attemptID, Purpose: CacheRefresh, Turn: s.Turn, Settled: true})
		}
	}()
	state.Pending = append(state.Pending, attemptID)
	state.State = "IN_FLIGHT"
	state.InFlightID = attemptID
	state.Attempts++
	state.LastAttemptAt = c.now()
	state.ReservedUSD += estimate
	next, _ := json.Marshal(state)
	claimed, err := c.Store.CompareAndSwap(ctx, cacheKey(scope, id), old, next, s.ExpiresAt.Sub(c.now()))
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	claimedBudget = true
	metadata := *snapshot
	metadata.Request = nil // Recovery needs identity, never another generation.
	r := &refreshRecord{Snapshot: metadata, Attempt: Attempt{ID: attemptID, Purpose: CacheRefresh, Role: Lead, Turn: s.Turn, Model: state.Snapshot.Model, StartedAt: c.now(), Status: "prepared", EstimateUSD: estimate}, Epoch: epoch, WindowID: window, Owner: owner}
	if err = c.writeRefresh(ctx, s, r); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	c.mu.Lock()
	c.cancels[cacheKey(scope, id)] = cancel
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.cancels, cacheKey(scope, id)); c.mu.Unlock() }()
	// Fencing at the last send boundary. A changed epoch never dispatches.
	latest, _, err := c.Load(bounded, scope, id)
	if err != nil {
		return err
	}
	if latest.Epoch != epoch || latest.WindowID != window || latest.InFlightID != attemptID {
		r.Attempt.Status = "completed"
		r.Attempt.Settled = true
		r.Reason = "stale_before_send"
		if err = c.writeRefresh(context.WithoutCancel(ctx), s, r); err != nil {
			return err
		}
		return c.finishRefresh(context.WithoutCancel(ctx), s, r)
	}
	if cancelled, err := engine.cancelled(bounded, s); err != nil {
		return err
	} else if cancelled {
		r.Attempt.Status = "completed"
		r.Attempt.Settled = true
		r.Reason = "cancelled_before_send"
		if err = c.writeRefresh(context.WithoutCancel(ctx), s, r); err != nil {
			return err
		}
		return c.finishRefresh(context.WithoutCancel(ctx), s, r)
	}
	if bounded.Err() != nil {
		r.Attempt.Status = "completed"
		r.Attempt.Settled = true
		r.Reason = "stale_before_send"
		if err = c.writeRefresh(context.WithoutCancel(ctx), s, r); err != nil {
			return err
		}
		return c.finishRefresh(context.WithoutCancel(ctx), s, r)
	}
	r.Attempt.Status = "started"
	if err = c.writeRefresh(bounded, s, r); err != nil {
		return err
	}
	// Cross-process cancellation and handoff/epoch changes also abort the wire.
	fenceDone := make(chan struct{})
	go func() {
		defer close(fenceDone)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-bounded.Done():
				return
			case <-ticker.C:
				current, _, readErr := c.Load(bounded, scope, id)
				stopped, cancelErr := engine.cancelled(bounded, s)
				live, _, loadErr := engine.Load(bounded, scope, id)
				if readErr != nil || cancelErr != nil || loadErr != nil || stopped || current.Epoch != epoch || current.WindowID != window || current.InFlightID != attemptID || !activeSidekick(live) {
					cancel()
					return
				}
			}
		}
	}()
	result, refreshErr := c.Adapter.Refresh(bounded, *state.Snapshot, r.Attempt)
	cancel()
	<-fenceDone
	r.Attempt.Response = result.Outcome.Response
	r.Attempt.BillingModel = result.Outcome.BillingModel
	r.RequestSucceeded = result.RequestSucceeded
	r.CacheConfirmed = result.CacheConfirmed
	if r.Attempt.Response != nil && r.Attempt.Response.Usage != nil && !result.UsageUncertain {
		r.Attempt.Status = "completed"
	} else if result.Outcome.KnownFailure {
		r.Attempt.Status = "completed"
		r.Attempt.Settled = true
	} else {
		r.Attempt.Status = "unknown"
	}
	if refreshErr != nil {
		r.Reason = "refresh_failed"
		if result.Outcome.KnownFailure {
			r.Reason = "terminal_refresh_failure"
		}
	}
	if r.Attempt.Response != nil && len(r.Attempt.Response.Output) > 0 {
		r.Reason = "incompatible_output"
		r.CacheConfirmed = false
	}
	persist, finish := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer finish()
	journalErr := c.writeRefresh(persist, s, r)
	if r.Attempt.Status == "completed" && !r.Attempt.Settled {
		cost, billErr := c.Adapter.Settle(persist, r.Snapshot, r.Attempt)
		if billErr != nil || !validCost(cost) {
			return E("billing_pending", "cache-refresh usage settlement is pending", 503)
		}
		r.Attempt.CostUSD = cost
		r.Attempt.Settled = true
	}
	if err = c.writeRefresh(persist, s, r); err != nil {
		return err
	}
	if journalErr != nil {
		return journalErr
	}
	return c.finishRefresh(persist, s, r)
}

func (c *CacheKeepaliveController) finishRefresh(ctx context.Context, s *Session, r *refreshRecord) error {
	engine := New(c.Store)
	engine.Now = c.Now
	if err := engine.settleBudget(ctx, s, r.Attempt); err != nil {
		return err
	}
	return c.update(ctx, s, func(current *CacheControl) {
		if r.Attempt.Settled {
			pending := current.Pending[:0]
			for _, id := range current.Pending {
				if id != r.Attempt.ID {
					pending = append(pending, id)
				}
			}
			current.Pending = pending
		}
		// Even stale work remains in the same turn's cost accounting. Its callback
		// cannot replace snapshots, advance due time or arm a newer epoch.
		if r.Attempt.Settled && !current.Accounted[r.Attempt.ID] {
			if current.Accounted == nil {
				current.Accounted = map[string]bool{}
			}
			current.Accounted[r.Attempt.ID] = true
			current.CumulativeCostUSD += r.Attempt.CostUSD
			if current.Turn == r.Attempt.Turn {
				current.ReservedUSD = max(0, current.ReservedUSD-r.Attempt.EstimateUSD)
				current.SpentUSD += r.Attempt.CostUSD
			}
		}
		if current.Epoch != r.Epoch || current.WindowID != r.WindowID || current.InFlightID != r.Attempt.ID {
			return
		}
		current.InFlightID = ""
		if r.RequestSucceeded {
			current.LastRequestSuccessAt = c.now()
		}
		if r.CacheConfirmed && r.Attempt.Status != "unknown" {
			current.LastCacheConfirmedAt = c.now()
		}
		if r.Attempt.Status == "unknown" || !r.RequestSucceeded || !r.CacheConfirmed || r.Reason == "incompatible_output" || r.Reason == "terminal_refresh_failure" || r.Reason == "cancelled_before_send" || r.Reason == "stale_before_send" {
			current.State = "STOPPED"
			current.StopReason = r.Reason
			if current.StopReason == "" {
				current.StopReason = "cache_not_confirmed"
			}
			if r.Attempt.Status == "unknown" {
				current.StopReason = "attempt_unknown"
			}
			return
		}
		if r.RequestSucceeded {
			current.LastRequestSuccessAt = c.now()
		}
		if r.CacheConfirmed {
			current.LastCacheConfirmedAt = c.now()
		}
		current.State = "ARMED"
		p := s.Profile.KeepalivePolicy()
		current.NextDueAt = c.now().Add(time.Duration(p.IntervalSeconds) * time.Second)
		if current.Attempts >= p.MaxAttempts || current.SpentUSD+current.ReservedUSD >= p.MaxCostUSD {
			current.State = "STOPPED"
			current.StopReason = "budget_exhausted"
		}
	})
}

// Resume reconciles a durable terminal result, or fences an expired unknown
// attempt. It never repeats a provider call or catches up missed intervals.
func (c *CacheKeepaliveController) Resume(ctx context.Context, s *Session) error {
	if c == nil || c.Store == nil {
		return nil
	}
	state, _, err := c.Load(ctx, s.Scope, s.ID)
	if err != nil {
		return err
	}
	ids := append([]string(nil), state.Pending...)
	if len(ids) == 0 && state.InFlightID != "" {
		ids = append(ids, state.InFlightID)
	}
	for _, id := range ids {
		if err := c.resumeAttempt(ctx, s, id); err != nil {
			return err
		}
	}
	return nil
}

func (c *CacheKeepaliveController) resumeAttempt(ctx context.Context, s *Session, id string) error {
	b, err := c.Store.Get(ctx, indexKey(s.Scope, "cache_refresh", s.ID+":"+id))
	if errors.Is(err, devin.ErrSessionNotFound) {
		return c.Stop(ctx, s, "attempt_journal_missing")
	}
	if err != nil {
		return err
	}
	var r refreshRecord
	if json.Unmarshal(b, &r) != nil || r.Attempt.ID != id || r.Attempt.Purpose != CacheRefresh {
		return E("session_corrupt", "invalid cache-refresh attempt", 503)
	}
	if r.Attempt.Status == "started" || r.Attempt.Status == "prepared" {
		unlock, acquired, lockErr := c.Store.Acquire(ctx, cacheKey(s.Scope, s.ID)+":refresh_lock", 5*time.Second)
		if lockErr != nil {
			return lockErr
		}
		if !acquired {
			return nil
		}
		defer unlock()
		// The execution may have completed while this caller waited for its lease.
		latest, readErr := c.Store.Get(ctx, indexKey(s.Scope, "cache_refresh", s.ID+":"+id))
		if readErr != nil {
			return readErr
		}
		if json.Unmarshal(latest, &r) != nil || r.Attempt.ID != id || r.Attempt.Purpose != CacheRefresh {
			return E("session_corrupt", "invalid cache-refresh attempt", 503)
		}
		if r.Attempt.Status == "started" || r.Attempt.Status == "prepared" {
			if c.now().Before(r.Attempt.StartedAt.Add(time.Duration(s.Profile.KeepalivePolicy().TimeoutSeconds) * time.Second)) {
				return nil
			}
			if r.Attempt.Status == "prepared" {
				r.Attempt.Status = "completed"
				r.Attempt.Settled = true
				r.Reason = "stale_before_send"
			} else {
				r.Attempt.Status = "unknown"
			}
		}
	}
	if r.Attempt.Status == "completed" && !r.Attempt.Settled {
		if c.Adapter == nil {
			return E("billing_pending", "cache-refresh adapter is required to reconcile known usage", 503)
		}
		cost, err := c.Adapter.Settle(ctx, r.Snapshot, r.Attempt)
		if err != nil || !validCost(cost) {
			return E("billing_pending", "cache-refresh usage settlement is pending", 503)
		}
		r.Attempt.Settled = true
		r.Attempt.CostUSD = cost
	}
	if err = c.writeRefresh(ctx, s, &r); err != nil {
		return err
	}
	return c.finishRefresh(ctx, s, &r)
}
