package fusion

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

type virtualCacheClock struct{ ns atomic.Int64 }

func newCacheClock() *virtualCacheClock {
	c := &virtualCacheClock{}
	c.ns.Store(time.Now().UnixNano())
	return c
}
func (c *virtualCacheClock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *virtualCacheClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

type refreshFake struct {
	mu        sync.Mutex
	supported bool
	busy      bool
	result    RefreshResult
	err       error
	calls     int
	billed    map[string]bool
	charged   float64
	started   chan struct{}
	finish    chan struct{}
}

func (f *refreshFake) Supports(CacheSnapshot) bool { return f.supported }
func (f *refreshFake) Acquire(context.Context, CacheSnapshot) (func(), bool) {
	return func() {}, !f.busy
}
func (f *refreshFake) Estimate(context.Context, CacheSnapshot) (float64, error) { return .1, nil }
func (f *refreshFake) Refresh(_ context.Context, _ CacheSnapshot, _ string) (RefreshResult, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.started != nil {
		close(f.started)
		<-f.finish
	}
	return f.result, f.err
}
func (f *refreshFake) Settle(_ context.Context, a Attempt) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.billed == nil {
		f.billed = map[string]bool{}
	}
	if !f.billed[a.ID] {
		f.billed[a.ID] = true
		f.charged += .04
	}
	return .04, nil
}
func cacheFixture(t *testing.T) (*Engine, *Session, *CacheKeepaliveController, *virtualCacheClock, *refreshFake) {
	t.Helper()
	e, s, raw, _ := summarySession(t, Sidekick)
	clock := newCacheClock()
	e.Now = clock.Now
	s.Profile.Keepalive = &KeepalivePolicy{Enabled: true, IntervalSeconds: 10, MaxWindowSeconds: 100, MaxAttempts: 3, MaxCostUSD: 1, TimeoutSeconds: 5}
	s.ProfileRevision = s.Profile.Revision()
	require.NoError(t, e.save(context.Background(), s, &raw))
	f := &refreshFake{supported: true, result: RefreshResult{RequestSucceeded: true, CacheConfirmed: true, Outcome: Outcome{Response: &apicompat.ResponsesResponse{Status: "completed", Usage: &apicompat.ResponsesUsage{InputTokens: 40, OutputTokens: 0, TotalTokens: 40}}, BillingModel: s.Profile.LeadModel}}}
	c := NewCacheKeepaliveController(e.Store)
	c.Now = clock.Now
	c.Adapter = f
	e.Cache = c
	r := testRequest("immutable lead prefix")
	r.Model = s.Profile.LeadModel
	require.NoError(t, c.Capture(context.Background(), s, r, "lead-attempt", "host-and-identity-digest"))
	return e, s, c, clock, f
}
func TestFusionKeepaliveIsDisabledWithoutVerifiedAdapter(t *testing.T) {
	_, s, c, clock, f := cacheFixture(t)
	c.Adapter = nil
	r := testRequest("prefix")
	r.Model = s.Profile.LeadModel
	require.NoError(t, c.Capture(context.Background(), s, r, "next-lead", "route"))
	state, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, "DISABLED", state.State)
	require.Equal(t, "refresh_protocol_unverified", state.StopReason)
	require.Nil(t, state.Snapshot.Request)
	clock.Advance(time.Hour)
	require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
	require.Zero(t, f.calls)
	c.Adapter = f
	require.NoError(t, c.Capture(context.Background(), s, nil, "", ""))
	state, _, err = c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, "snapshot_unavailable", state.StopReason)
}
func TestFusionKeepaliveIndependentEpochWindowAndNoCatchup(t *testing.T) {
	e, s, c, clock, f := cacheFixture(t)
	state, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	initialEpoch, due, start := state.Epoch, state.NextDueAt, state.WindowStartedAt
	for range 4 {
		s.Sidekick = append(s.Sidekick, textMessage("user", "sidekick-only progress"))
		_, raw, err := e.Load(context.Background(), s.Scope, s.ID)
		require.NoError(t, err)
		require.NoError(t, e.save(context.Background(), s, &raw))
		require.NoError(t, c.Observe(context.Background(), s))
	}
	state, _, err = c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, initialEpoch, state.Epoch)
	require.Equal(t, due, state.NextDueAt)
	require.Equal(t, start, state.WindowStartedAt)
	publicBefore := Digest(s.Public)
	clock.Advance(60 * time.Second)
	require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
	require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
	require.Equal(t, 1, f.calls)
	after, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, start, after.WindowStartedAt)
	require.False(t, after.LastCacheConfirmedAt.IsZero())
	require.InDelta(t, .04, after.SpentUSD, .00001)
	require.Equal(t, publicBefore, Digest(s.Public))
	clock.Advance(50 * time.Second)
	require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, after.Epoch, after.WindowID))
	require.Equal(t, 1, f.calls)
	after, _, err = c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, "STOPPED", after.State)
}
func TestFusionKeepaliveFencesLateCallbacksAndConcurrentOwners(t *testing.T) {
	_, s, c, clock, f := cacheFixture(t)
	f.started = make(chan struct{})
	f.finish = make(chan struct{})
	state, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	clock.Advance(10 * time.Second)
	done := make(chan error, 1)
	go func() { done <- c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID) }()
	<-f.started
	other := NewCacheKeepaliveController(c.Store)
	other.Now = clock.Now
	other.Adapter = f
	require.NoError(t, other.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
	require.NoError(t, c.Stop(context.Background(), s, "foreground_lead"))
	next := testRequest("new prefix after Lead resumes")
	next.Model = s.Profile.LeadModel
	require.NoError(t, c.Capture(context.Background(), s, next, "new-lead-attempt", "new-route"))
	fresh, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	close(f.finish)
	require.NoError(t, <-done)
	after, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, fresh.Epoch, after.Epoch)
	require.Equal(t, fresh.Snapshot.PrefixDigest, after.Snapshot.PrefixDigest)
	require.Equal(t, fresh.NextDueAt, after.NextDueAt)
	require.True(t, after.LastCacheConfirmedAt.IsZero())
	require.InDelta(t, .04, f.charged, .00001)
	require.Equal(t, 1, f.calls)
}
func TestFusionKeepaliveConfirmationUnknownCostAndUnexpectedOutput(t *testing.T) {
	for _, kind := range []string{"http success only", "unknown cost", "tool output", "terminal failure", "capacity"} {
		t.Run(kind, func(t *testing.T) {
			_, s, c, clock, f := cacheFixture(t)
			state, _, err := c.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			switch kind {
			case "http success only":
				f.result.CacheConfirmed = false
			case "unknown cost":
				f.result.Outcome.Response = nil
			case "tool output":
				f.result.Outcome.Response = tool("never-execute", "shell", `{}`)
			case "terminal failure":
				f.result.Outcome = Outcome{KnownFailure: true}
				f.err = errors.New("unsupported refresh protocol")
			case "capacity":
				f.busy = true
			}
			clock.Advance(10 * time.Second)
			require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
			after, _, err := c.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			switch kind {
			case "http success only":
				require.True(t, after.LastCacheConfirmedAt.IsZero())
				require.False(t, after.LastRequestSuccessAt.IsZero())
			case "unknown cost":
				require.Equal(t, "STOPPED", after.State)
				require.Greater(t, after.ReservedUSD, 0.0)
				require.Zero(t, f.charged)
			case "tool output", "terminal failure":
				require.Equal(t, "STOPPED", after.State)
			case "capacity":
				require.Zero(t, after.Attempts)
				require.Zero(t, f.calls)
			}
		})
	}
}

func TestFusionKeepaliveOneTokenUsageNeverEntersBusinessHistory(t *testing.T) {
	for _, visibleOutput := range []bool{false, true} {
		e, s, c, clock, adapter := cacheFixture(t)
		before, _, err := e.Load(context.Background(), s.Scope, s.ID)
		require.NoError(t, err)
		state, _, err := c.Load(context.Background(), s.Scope, s.ID)
		require.NoError(t, err)
		// A bounded native warm request can report max_tokens and one billed
		// output token without any visible content. This is distinct from a
		// business generation, whose decoder correctly rejects no content.
		adapter.result.Outcome.Response = &apicompat.ResponsesResponse{Status: "incomplete", IncompleteDetails: &apicompat.ResponsesIncompleteDetails{Reason: "max_output_tokens"}, Usage: &apicompat.ResponsesUsage{InputTokens: 8219, OutputTokens: 1, TotalTokens: 8220, InputTokensDetails: &apicompat.ResponsesInputTokensDetails{CachedTokens: 8215}}}
		if visibleOutput {
			adapter.result.Outcome.Response.Output = textResponse("x").Output
		}
		clock.Advance(10 * time.Second)
		require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
		after, _, err := e.Load(context.Background(), s.Scope, s.ID)
		require.NoError(t, err)
		require.Equal(t, Digest(before), Digest(after), "warm output and usage belong to their independent ledger")
		control, _, err := c.Load(context.Background(), s.Scope, s.ID)
		require.NoError(t, err)
		require.InDelta(t, .04, control.CumulativeCostUSD, .00001, "one output token is still settled")
		if visibleOutput {
			require.Equal(t, "STOPPED", control.State)
			require.True(t, control.LastCacheConfirmedAt.IsZero())
		} else {
			require.Equal(t, "ARMED", control.State)
			require.False(t, control.LastCacheConfirmedAt.IsZero())
		}
		require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
		require.Equal(t, 1, adapter.calls, "the same due tick must not be sent twice")
	}
}

func TestFusionKeepaliveSnapshotOwnsNestedRequestData(t *testing.T) {
	_, s, c, _, _ := cacheFixture(t)
	request := testRequest("immutable input")
	request.Model = s.Profile.LeadModel
	output := 32
	request.MaxOutputTokens = &output
	require.NoError(t, c.Capture(context.Background(), s, request, "captured-lead", "route"))
	before, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	digest := Digest(before.Snapshot.Request)
	request.Instructions = "caller changed instructions"
	request.Input[0] = ' '
	request.Tools[0].Description = "caller changed tool contract"
	request.Tools[0].Parameters[0] = ' '
	output = 1
	after, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, digest, Digest(after.Snapshot.Request))
	require.Equal(t, 32, *after.Snapshot.Request.MaxOutputTokens)
}
func TestFusionKeepaliveRestartUnknownAndIdempotentCost(t *testing.T) {
	_, s, c, clock, f := cacheFixture(t)
	state, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	r := &refreshRecord{Attempt: Attempt{ID: "crashed-attempt", Purpose: CacheRefresh, Role: Lead, Turn: s.Turn, StartedAt: clock.Now(), Status: "started", EstimateUSD: .1}, Epoch: state.Epoch, WindowID: state.WindowID, Owner: "old-owner"}
	require.NoError(t, c.writeRefresh(context.Background(), s, r))
	require.NoError(t, c.update(context.Background(), s, func(control *CacheControl) {
		control.State = "IN_FLIGHT"
		control.InFlightID = r.Attempt.ID
		control.ReservedUSD = .1
	}))
	clock.Advance(6 * time.Second)
	restart := NewCacheKeepaliveController(c.Store)
	restart.Now = clock.Now
	restart.Adapter = f
	require.NoError(t, restart.Resume(context.Background(), s))
	after, _, err := restart.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, "attempt_unknown", after.StopReason)
	require.Greater(t, after.ReservedUSD, 0.0)
	require.Zero(t, f.calls)
	r.Attempt.Status = "completed"
	r.Attempt.Settled = true
	r.Attempt.CostUSD = .04
	require.NoError(t, restart.finishRefresh(context.Background(), s, r))
	require.NoError(t, restart.finishRefresh(context.Background(), s, r))
	after, _, err = restart.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.InDelta(t, .04, after.CumulativeCostUSD, .00001)
	encoded, _ := json.Marshal(after)
	require.NotContains(t, string(encoded), "Authorization")
}
func TestFusionSidekickCompactionPreservesLeadCacheButLeadCompactionInvalidates(t *testing.T) {
	e, s, c, _, _ := cacheFixture(t)
	e.Cache = c
	state, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	var raw []byte
	s, raw, err = e.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	x := &summaryExecutor{response: summaryResponse()}
	base := testRequest("task")
	ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, false)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, e.collectCompactions(context.Background(), s, &raw, x))
	ok, err = e.applyCompaction(context.Background(), s, &raw, Sidekick, base, x)
	require.NoError(t, err)
	require.True(t, ok)
	after, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, state.Epoch, after.Epoch)
	s.Role = Lead
	s.Lead = append([]apicompat.ResponsesInputItem(nil), s.Sidekick...)
	for range 6 {
		s.Lead = append(s.Lead, textMessage("user", "another completed phase "+strings.Repeat("x", 600)))
	}
	require.NoError(t, e.save(context.Background(), s, &raw))
	ok, err = e.prepareCompaction(context.Background(), s, &raw, base, x, false)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, e.collectCompactions(context.Background(), s, &raw, x))
	ok, err = e.applyCompaction(context.Background(), s, &raw, Lead, base, x)
	require.NoError(t, err)
	require.True(t, ok)
	after, _, err = c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Greater(t, after.Epoch, state.Epoch)
	require.Nil(t, after.Snapshot)
}
