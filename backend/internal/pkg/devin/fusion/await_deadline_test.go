package fusion

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The third summary read is awaitCompaction, after run's initial collection and
// prepareRoleContext's initial collection. A blocked store read deterministically
// returns the actual parent/turn context error, optionally after a final receipt
// is committed by the simulated original worker. No runtime source is patched.
type awaitDeadlineStore struct {
	AtomicStore
	key       string
	reads     int
	waited    bool
	onAwait   func()
	onExpired func() error
}

func (s *awaitDeadlineStore) Get(ctx context.Context, key string) ([]byte, error) {
	if key == s.key {
		s.reads++
		if s.reads == 3 {
			s.waited = true
			if s.onAwait != nil {
				s.onAwait()
			}
			<-ctx.Done()
			if s.onExpired != nil {
				if err := s.onExpired(); err != nil {
					return nil, err
				}
			}
			return nil, ctx.Err()
		}
	}
	return s.AtomicStore.Get(ctx, key)
}

type awaitExecutor struct {
	calls   []Call
	settled map[string]bool
	charged float64
}

func (x *awaitExecutor) Estimate(context.Context, Call) (float64, error) { return .1, nil }
func (x *awaitExecutor) Execute(ctx context.Context, call Call) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{KnownFailure: true, NotStarted: true}, err
	}
	x.calls = append(x.calls, call)
	text := "Sidekick report: preserved actual evidence and original brief"
	if call.Role == Lead {
		text = "Lead reviewed the preserved Sidekick evidence"
	}
	return Outcome{Response: textResponse(text), UsageFinal: true, BillingModel: call.Request.Model}, nil
}
func (x *awaitExecutor) Settle(_ context.Context, a Attempt) (float64, error) {
	if x.settled == nil {
		x.settled = map[string]bool{}
	}
	if !x.settled[a.ID] {
		x.settled[a.ID] = true
		x.charged += .1
	}
	return .1, nil
}

func TestFusionAdvanceHardGateInterruptions(t *testing.T) {
	for _, tc := range []struct {
		name                                                 string
		turnExpires, jobExpires, knownAtExpiry, cancelClient bool
		wantState                                            string
		canResume                                            bool
	}{
		{name: "client_deadline_live_summary", wantState: RunningSidekick, canResume: true},
		{name: "client_deadline_known_receipt", knownAtExpiry: true, wantState: RunningSidekick, canResume: true},
		{name: "client_cancel_known_receipt", knownAtExpiry: true, cancelClient: true, wantState: RunningSidekick, canResume: true},
		{name: "parent_and_child_expired_unknown", jobExpires: true, wantState: Uncertain},
		{name: "original_900s_expired_unknown", turnExpires: true, jobExpires: true, wantState: Uncertain},
		{name: "original_900s_expired_known", turnExpires: true, jobExpires: true, knownAtExpiry: true, wantState: BudgetExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s, raw, base := summarySession(t, Sidekick)
			s.Profile.Compaction = &CompactionPolicy{Enabled: true, Sidekick: []int{2000, 3000, 10000}, TimeoutSeconds: 30, MaxAttempts: 2, MaxOutputTokens: 1024}
			s.ProfileRevision = s.Profile.Revision()
			require.NoError(t, s.Profile.Validate())
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
			s.TurnHandoffs, s.TurnTools, s.TurnRequests = 1, 12, 1
			s.TurnCostUSD, s.AuxReservedUSD = .3, .1
			s.Lead = []apicompat.ResponsesInputItem{textMessage("user", "Original goal: preserve compatibility"), {Type: "function_call", Name: DelegateTool, CallID: s.Handoffs[0].CallID, Arguments: s.Handoffs[0].Brief}}
			expiry := time.Now().Add(250 * time.Millisecond)
			if tc.turnExpires {
				s.TurnStarted = expiry.Add(-900 * time.Second)
			}
			originalStart := s.TurnStarted
			cut := safeSummaryPrefix(s, Sidekick, s.Profile.MaxContextBytes/2)
			require.Positive(t, cut)
			j := CompactionJob{ID: "in-flight-original-summary", Role: Sidekick, Turn: s.Turn, Epoch: s.roleContext(Sidekick).Epoch, ProfileRevision: s.ProfileRevision, PrefixCount: cut, PrefixDigest: Digest(s.Sidekick[:cut]), Policy: summaryPolicy, State: "preparing", CreatedAt: time.Now().Add(-5 * time.Second), Deadline: time.Now().Add(25 * time.Second), EstimateUSD: .1}
			if tc.jobExpires {
				j.Deadline = expiry
			}
			s.roleContext(Sidekick).Job = &j
			r := &compactionRecord{Job: j, Request: summaryRequest(s, Sidekick, base, cut), Attempt: Attempt{ID: j.ID, Purpose: Compaction, Role: Sidekick, Turn: s.Turn, Model: s.Profile.SidekickModel, StartedAt: j.CreatedAt, Status: "started", EstimateUSD: .1}}
			require.NoError(t, e.writeCompaction(context.Background(), s, r, nil))
			require.NoError(t, e.reserveBudget(context.Background(), s, j.ID, Compaction, .1))
			require.NoError(t, e.save(context.Background(), s, &raw))
			injectFinal := func() error {
				latest, old, err := e.readCompaction(context.Background(), s, &j)
				if err != nil {
					return err
				}
				latest.Attempt.Status = "completed"
				latest.Attempt.Response = summaryResponse()
				final := true
				latest.Attempt.UsageFinal = &final
				latest.Job.State = "ready"
				latest.Request = nil
				latest.Summary, err = parseSummary(latest.Attempt.Response)
				if err != nil {
					return err
				}
				return e.writeCompaction(context.Background(), s, latest, old)
			}
			store := &awaitDeadlineStore{AtomicStore: e.Store, key: compactKey(s, j.ID)}
			e.Store = store
			parent := context.Background()
			var cancel context.CancelFunc
			if tc.turnExpires {
				parent, cancel = context.WithCancel(parent)
			} else if tc.cancelClient {
				parent, cancel = context.WithCancel(parent)
				store.onAwait = cancel
			} else {
				parent, cancel = context.WithDeadline(parent, expiry)
			}
			defer cancel()
			if tc.knownAtExpiry {
				store.onExpired = injectFinal
			}
			x := &awaitExecutor{}
			in := Input{Scope: s.Scope, SessionID: s.ID, AccountID: s.AccountID, Profile: s.Profile, Request: base, IdempotencyKey: "original"}
			_, firstErr := e.Advance(parent, in, x)
			require.Error(t, firstErr)
			require.True(t, store.waited, "public Advance must reach await's third read")
			restored, _, err := e.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			observed, _, err := e.readCompaction(context.Background(), s, &j)
			require.NoError(t, err)
			t.Logf("first_error=%v state=%s active=%d receipt=%s settled=%t calls=%d synthetic_charge=%.2f original_wall_remaining=%s", firstErr, restored.State, restored.ActiveRequest, observed.Attempt.Status, observed.Attempt.Settled, len(x.calls), x.charged, time.Until(originalStart.Add(900*time.Second)))
			assert.Equal(t, tc.wantState, restored.State)
			assert.True(t, originalStart.Equal(restored.TurnStarted))
			assert.Equal(t, 1, restored.TurnRequests)
			assert.Empty(t, x.calls, "interrupt must never start a new physical inference")
			if tc.canResume {
				assert.Equal(t, 0, restored.ActiveRequest)
			} else {
				assert.Equal(t, -1, restored.ActiveRequest)
			}
			if tc.knownAtExpiry {
				assert.True(t, observed.Attempt.Settled, "known receipt must collect before classifying client/turn expiry")
			}
			if tc.jobExpires && !tc.knownAtExpiry {
				assert.Equal(t, "unknown", observed.Attempt.Status)
			}
			if !tc.knownAtExpiry {
				require.NoError(t, injectFinal())
			}
			for range 2 {
				_, err = New(e.Store).ReconcileUsage(context.Background(), s.Scope, s.ID, x)
				require.NoError(t, err)
			}
			assert.InDelta(t, .1, x.charged, 1e-9, "late final usage is settled once without starting a model")
			res, retryErr := New(e.Store).Advance(context.Background(), in, x)
			after, _, err := e.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			t.Logf("retry_error=%v state=%s calls=%d unique_settlements=%d turn=%d requests=%d same_turn_start=%t", retryErr, after.State, len(x.calls), len(x.settled), after.Turn, after.TurnRequests, originalStart.Equal(after.TurnStarted))
			assert.True(t, originalStart.Equal(after.TurnStarted))
			assert.Equal(t, 900, after.Profile.MaxWallSeconds)
			if tc.canResume {
				assert.NoError(t, retryErr)
				if retryErr == nil {
					assert.Equal(t, Completed, res.State)
					assert.Len(t, x.calls, 2)
					assert.Equal(t, 3, after.TurnRequests)
					assert.Equal(t, 1, after.Turn)
				}
			} else {
				assert.Error(t, retryErr)
				assert.Empty(t, x.calls, "unknown or expired original turn must not be revived by a late receipt")
			}
		})
	}
}
