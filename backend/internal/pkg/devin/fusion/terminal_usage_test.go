package fusion

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// A final accounting receipt is independent of output usability and deadlines.
type terminalExecutor struct {
	summaryExecutor
	outcome      Outcome
	err          error
	beforeReturn func()
}

func (x *terminalExecutor) Execute(_ context.Context, call Call) (Outcome, error) {
	x.calls = append(x.calls, call)
	if x.beforeReturn != nil {
		x.beforeReturn()
	}
	return x.outcome, x.err
}

func TestFusionPartialUsageInferenceMustRemainUnknown(t *testing.T) {
	e := New(newMemoryStore())
	r := textResponse("partial")
	r.Status = "in_progress"
	x := &scriptExecutor{t: t, execute: func(Call) (Outcome, error) {
		return Outcome{Response: r}, context.DeadlineExceeded
	}}
	_, err := e.Advance(context.Background(), input(testRequest("preserve original goal")), x)
	require.Error(t, err)
	s, _, err := e.Load(context.Background(), Scope(1, 2), "chat-one")
	require.NoError(t, err)
	a := s.Attempts[0]
	t.Logf("status=%s settled=%t synthetic_charge=%.2f response_status=%s session=%s", a.Status, a.Settled, x.charged, a.Response.Status, s.State)
	require.Equal(t, "unknown", a.Status, "partial usage with deadline is not final accounting evidence")
	require.False(t, a.Settled)
	require.Zero(t, x.charged)
	_, err = New(e.Store).Advance(context.Background(), input(testRequest("preserve original goal")), x)
	require.Error(t, err)
	require.Len(t, x.calls, 1, "recovery must inspect the original attempt without generating again")
}

func TestFusionPartialUsageCompactionMustRemainUnknown(t *testing.T) {
	e, s, raw, base := summarySession(t, Sidekick)
	r := &apicompat.ResponsesResponse{Status: "in_progress", Usage: &apicompat.ResponsesUsage{InputTokens: 100, OutputTokens: 2, TotalTokens: 102}}
	x := &terminalExecutor{outcome: Outcome{Response: r}, err: context.DeadlineExceeded}
	ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, false)
	require.NoError(t, err)
	require.True(t, ok)
	stored, _, err := e.readCompaction(context.Background(), s, s.roleContext(Sidekick).Job)
	require.NoError(t, err)
	t.Logf("status=%s job=%s settled=%t synthetic_charge=%.2f response_status=%s", stored.Attempt.Status, stored.Job.State, stored.Attempt.Settled, x.charged, stored.Attempt.Response.Status)
	require.Equal(t, "unknown", stored.Attempt.Status)
	require.False(t, stored.Attempt.Settled)
	require.Zero(t, x.charged)
	requireCode(t, e.collectCompactions(context.Background(), s, &raw, x), "compaction_uncertain")
	require.NoError(t, e.Cancel(context.Background(), s.Scope, s.ID))
	for range 2 {
		_, err = New(e.Store).ReconcileUsage(context.Background(), s.Scope, s.ID, x)
		requireCode(t, err, "compaction_uncertain")
	}
	restored, _, err := e.Load(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Equal(t, Cancelled, restored.State)
	require.Greater(t, restored.AuxReservedUSD, 0.0)
	require.True(t, s.TurnStarted.Equal(restored.TurnStarted))
	require.Len(t, x.calls, 1)
	require.Zero(t, x.charged)
}

func TestFusionLateTerminalCompactionMustRetainReceipt(t *testing.T) {
	e, s, raw, base := summarySession(t, Sidekick)
	x := &terminalExecutor{outcome: Outcome{Response: summaryResponse(), UsageFinal: true}}
	x.beforeReturn = func() {
		j := s.roleContext(Sidekick).Job
		now := j.Deadline.Add(time.Millisecond)
		e.Now = func() time.Time { return now }
		err := e.collectCompactions(context.Background(), s, &raw, x)
		requireCode(t, err, "compaction_uncertain")
	}
	ok, err := e.prepareCompaction(context.Background(), s, &raw, base, x, false)
	require.NoError(t, err)
	require.True(t, ok)
	j := s.roleContext(Sidekick).Job
	stored, _, err := e.readCompaction(context.Background(), s, j)
	require.NoError(t, err)
	t.Logf("journal=%s job=%s settled=%t response_saved=%t synthetic_charge=%.2f aux_reserved=%.2f", stored.Attempt.Status, stored.Job.State, stored.Attempt.Settled, stored.Attempt.Response != nil, x.charged, s.AuxReservedUSD)
	require.Equal(t, "completed", stored.Attempt.Status, "late known terminal usage must replace recovery unknown without another provider call")
	require.True(t, stored.Attempt.Settled)
	require.NotNil(t, stored.Attempt.Response)
	require.Len(t, x.calls, 1)
	for range 2 {
		require.NoError(t, New(e.Store).collectCompactions(context.Background(), s, &raw, x))
	}
	require.Zero(t, s.AuxReservedUSD)
	require.Equal(t, .1, s.TurnCostUSD)
	require.Equal(t, .1, x.charged)
	require.Len(t, x.billed, 1)
}

func TestFusionExpiredSummaryReportsUnknownBeforeHardContextError(t *testing.T) {
	e, s, raw, base := summarySession(t, Sidekick)
	p := CompactionPolicy{Enabled: true, Sidekick: []int{2000, 3000, 10000}, MaxAttempts: 2, TimeoutSeconds: 30, MaxOutputTokens: 1024}
	s.Profile.Compaction = &p
	s.ProfileRevision = s.Profile.Revision()
	j := CompactionJob{ID: "expired-summary", Role: Sidekick, Turn: s.Turn, ProfileRevision: s.ProfileRevision, PrefixCount: 2, PrefixDigest: Digest(s.Sidekick[:2]), Policy: summaryPolicy, CreatedAt: time.Now().Add(-31 * time.Second), Deadline: time.Now().Add(-time.Second), EstimateUSD: .1}
	s.roleContext(Sidekick).Job = &j
	s.AuxReservedUSD, s.TurnRequests = .1, 1
	r := &compactionRecord{Job: j, Attempt: Attempt{ID: j.ID, Purpose: Compaction, Role: j.Role, Turn: j.Turn, Model: s.Profile.SidekickModel, StartedAt: j.CreatedAt, EstimateUSD: .1, Status: "started"}}
	require.NoError(t, e.writeCompaction(context.Background(), s, r, nil))
	require.NoError(t, e.save(context.Background(), s, &raw))
	require.NoError(t, e.awaitCompaction(context.Background(), s, &j))
	x := &summaryExecutor{}
	_, err := e.prepareRoleContext(context.Background(), s, &raw, base, x)
	requireCode(t, err, "compaction_uncertain")
	require.Empty(t, x.calls)
	require.Zero(t, x.charged)
	require.Equal(t, .1, s.AuxReservedUSD)
	require.Equal(t, 900, s.Profile.MaxWallSeconds)
	require.Equal(t, 30, s.Profile.CompactPolicy().TimeoutSeconds)
}

func TestFusionCompactionRecoveryRequiresFinalAccountingReceipt(t *testing.T) {
	no, yes := false, true
	for _, tc := range []struct {
		name, status     string
		final            *bool
		settled, unknown bool
	}{
		{name: "legacy_completed", status: "completed"},
		{name: "legacy_incomplete", status: "incomplete"},
		{name: "legacy_partial", status: "in_progress", unknown: true},
		{name: "explicit_unknown_overrides_status", status: "completed", final: &no, unknown: true},
		{name: "final_usage_with_invalid_output", status: "in_progress", final: &yes},
		{name: "historical_settled_is_not_rebilled", status: "in_progress", settled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s, raw, _ := summarySession(t, Sidekick)
			j := CompactionJob{ID: "old-receipt", Role: Sidekick, Turn: s.Turn, ProfileRevision: s.ProfileRevision, PrefixCount: 2, PrefixDigest: Digest(s.Sidekick[:2]), Policy: summaryPolicy, State: "failed", EstimateUSD: .1}
			s.roleContext(Sidekick).Job = &j
			s.AuxReservedUSD = .1
			response := textResponse("no usable summary")
			response.Status = tc.status
			r := &compactionRecord{Job: j, Attempt: Attempt{ID: j.ID, Purpose: Compaction, Role: j.Role, Turn: j.Turn, Status: "completed", Response: response, UsageFinal: tc.final, Settled: tc.settled}}
			require.NoError(t, e.writeCompaction(context.Background(), s, r, nil))
			require.NoError(t, e.save(context.Background(), s, &raw))
			x := &summaryExecutor{}
			for range 2 {
				err := e.collectCompactions(context.Background(), s, &raw, x)
				if tc.unknown {
					requireCode(t, err, "compaction_uncertain")
				} else {
					require.NoError(t, err)
				}
			}
			require.Empty(t, x.calls)
			if tc.unknown || tc.settled {
				require.Zero(t, x.charged)
			} else {
				require.Equal(t, .1, x.charged)
			}
			require.Equal(t, tc.unknown, s.AuxReservedUSD > 0)
		})
	}
}
