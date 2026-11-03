package fusion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type interruptedBillingExecutor struct {
	awaitExecutor
	failSettlement   bool
	beforeSettlement func()
}

func (x *interruptedBillingExecutor) Settle(ctx context.Context, a Attempt) (float64, error) {
	if x.beforeSettlement != nil {
		x.beforeSettlement()
	}
	if x.failSettlement {
		return 0, errors.New("synthetic billing unavailable")
	}
	return x.awaitExecutor.Settle(ctx, a)
}

func TestFusionInterruptedBillingFailureMustFenceCache(t *testing.T) {
	e, s, c, clock, refresh := cacheFixture(t)
	base := testRequest("new task")
	s.Profile.Compaction = &CompactionPolicy{Enabled: true, Sidekick: []int{2000, 3000, 10000}, TimeoutSeconds: 30, MaxAttempts: 2, MaxOutputTokens: 1024}
	s.ProfileRevision = s.Profile.Revision()
	var err error
	s.Contract, err = contract(base)
	require.NoError(t, err)
	items, err := ParseInput(base)
	require.NoError(t, err)
	s.Requests = []RequestRecord{{Key: "original", Digest: Digest(struct {
		Input    []apicompat.ResponsesInputItem
		Previous string
		Contract string
	}{items, base.PreviousResponseID, s.Contract})}}
	s.ActiveRequest = 0
	s.TurnHandoffs, s.TurnRequests = 1, 1
	s.TurnCostUSD, s.AuxReservedUSD = .3, .1
	cut := safeSummaryPrefix(s, Sidekick, s.Profile.MaxContextBytes/2)
	j := CompactionJob{ID: "pending-billing-summary", Role: Sidekick, Turn: s.Turn, ProfileRevision: s.ProfileRevision, PrefixCount: cut, PrefixDigest: Digest(s.Sidekick[:cut]), Policy: summaryPolicy, State: "preparing", CreatedAt: clock.Now().Add(-5 * time.Second), Deadline: clock.Now().Add(25 * time.Second), EstimateUSD: .1}
	s.roleContext(Sidekick).Job = &j
	r := &compactionRecord{Job: j, Attempt: Attempt{ID: j.ID, Purpose: Compaction, Role: Sidekick, Turn: s.Turn, Model: s.Profile.SidekickModel, StartedAt: j.CreatedAt, Status: "started", EstimateUSD: .1}}
	require.NoError(t, e.writeCompaction(context.Background(), s, r, nil))
	require.NoError(t, e.reserveBudget(context.Background(), s, j.ID, Compaction, .1))
	_, raw, err := e.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.NoError(t, e.save(context.Background(), s, &raw))
	lead := testRequest("immutable live Lead prefix")
	lead.Model = s.Profile.LeadModel
	require.NoError(t, c.Capture(context.Background(), s, lead, "original-lead", "verified-route"))
	store := &awaitDeadlineStore{AtomicStore: e.Store, key: compactKey(s, j.ID)}
	e.Store = store
	store.onExpired = func() error {
		latest, old, err := e.readCompaction(context.Background(), s, &j)
		if err != nil {
			return err
		}
		latest.Attempt.Status = "completed"
		latest.Attempt.Response = summaryResponse()
		final := true
		latest.Attempt.UsageFinal = &final
		latest.Job.State = "ready"
		latest.Summary, err = parseSummary(latest.Attempt.Response)
		if err != nil {
			return err
		}
		return e.writeCompaction(context.Background(), s, latest, old)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	x := &interruptedBillingExecutor{failSettlement: true}
	x.beforeSettlement = func() {
		state, _, err := c.Load(context.Background(), s.Scope, s.ID)
		require.NoError(t, err)
		clock.Advance(11 * time.Second)
		require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
		t.Logf("during_interrupted_receipt_settlement cache_state=%s refresh_calls=%d synthetic_refresh_charge=%.2f", state.State, refresh.calls, refresh.charged)
		assert.Zero(t, refresh.calls, "optional workers must be fenced before entering potentially blocked receipt settlement")
	}
	_, err = e.Advance(ctx, Input{Scope: s.Scope, SessionID: s.ID, AccountID: s.AccountID, Profile: s.Profile, Request: base, IdempotencyKey: "original"}, x)
	requireCode(t, err, "billing_pending")
	require.True(t, store.waited)
	state, _, err := c.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	t.Logf("after_client_deadline_plus_billing_error cache_state=%s stop_reason=%s inference_calls=%d refresh_calls=%d", state.State, state.StopReason, len(x.calls), refresh.calls)
	assert.Equal(t, "STOPPED", state.State, "client interruption must fence optional paid workers even if receipt settlement is pending")
	clock.Advance(11 * time.Second)
	require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
	t.Logf("after_due_tick refresh_calls=%d synthetic_refresh_charge=%.2f", refresh.calls, refresh.charged)
	assert.Zero(t, refresh.calls, "optional cache refresh must not send another paid request after client interruption")
}
