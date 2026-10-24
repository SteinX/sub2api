package fusion

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func refused(kind RejectionKind, delay time.Duration) (Outcome, error) {
	return Outcome{KnownFailure: true, Rejection: &Rejection{Kind: kind, NotExecuted: true, RetryAfter: delay}}, errors.New("explicit provider refusal")
}

func TestFusionExplicitRejectionsHaveIndependentBoundedAttempts(t *testing.T) {
	store := newMemoryStore()
	x := &scriptExecutor{t: t}
	x.execute = func(c Call) (Outcome, error) {
		switch len(x.calls) {
		case 1:
			return refused(RejectionRateLimit, 0)
		case 2:
			return refused(RejectionAuth, 0)
		case 3:
			return Outcome{Response: delegation("delegate"), BillingModel: c.Request.Model}, nil
		case 4:
			return Outcome{Response: textResponse("Sidekick report"), BillingModel: c.Request.Model}, nil
		default:
			return Outcome{Response: textResponse("Reviewed answer"), BillingModel: c.Request.Model}, nil
		}
	}
	in := input(testRequest("fix the issue"))
	result, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Equal(t, Completed, result.State)
	require.Len(t, x.calls, 5)
	require.Empty(t, x.calls[0].RetryOf)
	require.False(t, x.calls[0].AuthRecoveryUsed)
	require.Equal(t, x.calls[0].AttemptID, x.calls[1].RetryOf)
	require.Equal(t, RejectionRateLimit, x.calls[1].RecoveryKind)
	require.False(t, x.calls[1].AuthRecoveryUsed)
	require.Equal(t, x.calls[1].AttemptID, x.calls[2].RetryOf)
	require.Equal(t, RejectionAuth, x.calls[2].RecoveryKind)
	require.True(t, x.calls[2].AuthRecoveryUsed)
	require.False(t, x.calls[3].AuthRecoveryUsed, "the next logical inference has its own authentication recovery allowance")
	require.Empty(t, x.calls[3].RetryOf)
	ids := map[string]bool{}
	for _, call := range x.calls {
		require.False(t, ids[call.AttemptID])
		ids[call.AttemptID] = true
	}
	for _, call := range x.calls[:3] {
		require.Equal(t, Lead, call.Role)
		require.Equal(t, in.Profile.LeadModel, call.Request.Model)
		require.Equal(t, x.calls[0].LaneScope, call.LaneScope)
		require.Equal(t, Digest(x.calls[0].Request), Digest(call.Request))
	}
	s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, 5, s.TurnRequests)
	require.Len(t, s.Attempts, 5)
	for _, a := range s.Attempts[:2] {
		require.True(t, a.Applied)
		require.True(t, a.Settled)
		require.Zero(t, a.CostUSD)
		require.NotNil(t, a.Rejection)
		require.False(t, a.RetryNotBefore.IsZero())
	}
	require.Len(t, x.settled, 3, "rejected sends must not be billed as successful inferences")
	_, err = New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Len(t, x.calls, 5, "HTTP replay must not reset or repeat the physical chain")
}

func TestFusionRejectionLimitsAndRequestBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  RejectionKind
		count int
		code  string
	}{
		{"three sends", RejectionModelCapacity, 3, "recovery_exhausted"},
		{"one authentication recovery", RejectionAuth, 2, "auth_recovery_exhausted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			x := &scriptExecutor{t: t, execute: func(Call) (Outcome, error) { return refused(tc.kind, 0) }}
			in := input(testRequest("task"))
			_, err := New(store).Advance(context.Background(), in, x)
			requireCode(t, err, tc.code)
			require.Len(t, x.calls, tc.count)
			require.Empty(t, x.settled)
			s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			require.Equal(t, Failed, s.State)
			require.Equal(t, tc.count, s.TurnRequests)
			_, err = New(store).Advance(context.Background(), in, x)
			requireCode(t, err, "session_stopped")
			require.Len(t, x.calls, tc.count)
		})
	}
	t.Run("rejections consume normal request allowance", func(t *testing.T) {
		x := &scriptExecutor{t: t}
		x.execute = func(c Call) (Outcome, error) {
			if len(x.calls) <= 2 {
				return refused(RejectionRateLimit, 0)
			}
			return Outcome{Response: delegation("delegate"), BillingModel: c.Request.Model}, nil
		}
		in := input(testRequest("task"))
		in.Profile.MaxRequests = 3
		_, err := New(newMemoryStore()).Advance(context.Background(), in, x)
		requireCode(t, err, "budget_exhausted")
		require.Len(t, x.calls, 3)
		require.Len(t, x.settled, 1)
	})
}

func TestFusionAmbiguousOrObservedOutcomesNeverAuthorizeRecovery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome Outcome
		state   string
	}{
		{"EOF", Outcome{}, Uncertain},
		{"metadata without terminal rejection", Outcome{Rejection: &Rejection{Kind: RejectionAuth, NotExecuted: true}}, Uncertain},
		{"not known unexecuted", Outcome{KnownFailure: true, Rejection: &Rejection{Kind: RejectionAuth}}, Failed},
		{"invalid wait", Outcome{KnownFailure: true, Rejection: &Rejection{Kind: RejectionRateLimit, NotExecuted: true, RetryAfter: -time.Second}}, Failed},
		{"zero usage is still observed", Outcome{KnownFailure: true, Rejection: &Rejection{Kind: RejectionRateLimit, NotExecuted: true}, Response: &apicompat.ResponsesResponse{Usage: &apicompat.ResponsesUsage{}}}, Failed},
		{"partial tool", Outcome{KnownFailure: true, Rejection: &Rejection{Kind: RejectionModelCapacity, NotExecuted: true}, Response: &apicompat.ResponsesResponse{Output: []apicompat.ResponsesOutput{{Type: "function_call", CallID: "partial", Name: "edit_file", Arguments: `{`}}}}, Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			x := &scriptExecutor{t: t, execute: func(Call) (Outcome, error) { return tc.outcome, errors.New("transport or refused call") }}
			in := input(testRequest("task"))
			_, err := New(store).Advance(context.Background(), in, x)
			require.Error(t, err)
			require.Len(t, x.calls, 1)
			s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			require.Equal(t, tc.state, s.State)
			require.Nil(t, s.Attempts[0].Rejection)
			if tc.state == Uncertain {
				require.False(t, s.Attempts[0].Settled)
			}
		})
	}
}

func TestFusionRecoveryPersistsWaitAcrossClientDisconnect(t *testing.T) {
	store := newMemoryStore()
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{tool("read", "read_file", `{}`)}}
	x.execute = func(Call) (Outcome, error) {
		x.execute = nil
		return refused(RejectionRateLimit, 100*time.Millisecond)
	}
	in := input(testRequest("task"))
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := New(store).Advance(ctx, in, x)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, x.calls, 1)
	s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	anchor := s.Attempts[0].RetryNotBefore
	require.True(t, anchor.After(time.Now()))
	result, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Equal(t, WaitingTool, result.State)
	require.Len(t, x.calls, 2)
	require.False(t, time.Now().Before(anchor), "reconnect must honor the original provider wait")
	s, _, err = New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, anchor, s.Attempts[0].RetryNotBefore)
	require.Equal(t, x.calls[0].AttemptID, x.calls[1].RetryOf)
}

func TestFusionRecoveryWaitIsCancelledOrRejectedByDeadline(t *testing.T) {
	t.Run("explicit cancellation during wait", func(t *testing.T) {
		store := newMemoryStore()
		first := make(chan struct{})
		x := &scriptExecutor{t: t, execute: func(Call) (Outcome, error) {
			close(first)
			return refused(RejectionRateLimit, 5*time.Second)
		}}
		in := input(testRequest("task"))
		done := make(chan error, 1)
		go func() { _, err := New(store).Advance(context.Background(), in, x); done <- err }()
		<-first
		<-time.After(15 * time.Millisecond)
		require.NoError(t, New(store).Cancel(context.Background(), in.Scope, in.SessionID))
		select {
		case err := <-done:
			requireCode(t, err, "cancelled")
		case <-time.After(time.Second):
			t.Fatal("durable cancellation did not interrupt recovery wait")
		}
		require.Len(t, x.calls, 1)
	})
	t.Run("hint cannot fit turn", func(t *testing.T) {
		x := &scriptExecutor{t: t, execute: func(Call) (Outcome, error) { return refused(RejectionRateLimit, 11*time.Second) }}
		in := input(testRequest("task"))
		in.Profile.MaxWallSeconds = 10
		started := time.Now()
		_, err := New(newMemoryStore()).Advance(context.Background(), in, x)
		requireCode(t, err, "budget_exhausted")
		require.Less(t, time.Since(started), time.Second)
		require.Len(t, x.calls, 1)
	})
}

type rejectionJournalStore struct {
	*memoryStore
	fail bool
}

func (s *rejectionJournalStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if s.fail && strings.Contains(key, ":attempt:") {
		s.fail = false
		return errors.New("rejection journal unavailable")
	}
	return s.memoryStore.Set(ctx, key, value, ttl)
}

func TestFusionRecoveryNeedsDurableRefusalAndSurvivesRestart(t *testing.T) {
	for _, failure := range []string{"journal", "outcome snapshot", "recovery snapshot", "budget settlement"} {
		t.Run(failure, func(t *testing.T) {
			memory := newMemoryStore()
			store := &rejectionJournalStore{memoryStore: memory, fail: failure == "journal"}
			matches := 0
			if failure != "journal" {
				memory.failCAS = func(next []byte) bool {
					if failure == "budget settlement" {
						var budget turnBudget
						if json.Unmarshal(next, &budget) == nil && len(budget.Entries) == 1 {
							for _, entry := range budget.Entries {
								return entry.Settled && entry.Amount == 0
							}
						}
						return false
					}
					var s Session
					if json.Unmarshal(next, &s) == nil && len(s.Attempts) == 1 && s.Attempts[0].Rejection != nil && !s.Attempts[0].Applied {
						matches++
						return matches == 1 && failure == "outcome snapshot" || matches == 2 && failure == "recovery snapshot"
					}
					return false
				}
			}
			x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{tool("read", "read_file", `{}`)}}
			x.execute = func(Call) (Outcome, error) {
				x.execute = nil
				return refused(RejectionAuth, 0)
			}
			in := input(testRequest("task"))
			in.Profile.MaxCostUSD = 1.05
			_, err := New(store).Advance(context.Background(), in, x)
			require.Error(t, err)
			require.Len(t, x.calls, 1, "a persistence failure must not send a replacement inference")
			result, err := New(store).Advance(context.Background(), in, x)
			require.NoError(t, err)
			require.Equal(t, WaitingTool, result.State)
			require.Len(t, x.calls, 2)
			require.Equal(t, x.calls[0].AttemptID, x.calls[1].RetryOf)
			require.Equal(t, RejectionAuth, x.calls[1].RecoveryKind)
			require.True(t, x.calls[1].AuthRecoveryUsed)
			require.Len(t, x.settled, 1)
			_, err = New(store).Advance(context.Background(), in, x)
			require.NoError(t, err)
			require.Len(t, x.calls, 2)
			require.Len(t, x.settled, 1)
		})
	}
}

func TestFusionPaidRecoveryOutcomeSurvivesSnapshotFailure(t *testing.T) {
	store := newMemoryStore()
	store.failCAS = func(next []byte) bool {
		var s Session
		return json.Unmarshal(next, &s) == nil && len(s.Attempts) == 2 && s.Attempts[1].Status == "completed"
	}
	x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{tool("read", "read_file", `{}`)}}
	x.execute = func(Call) (Outcome, error) {
		x.execute = nil
		return refused(RejectionAuth, 0)
	}
	in := input(testRequest("task"))
	_, err := New(store).Advance(context.Background(), in, x)
	require.ErrorContains(t, err, "store interruption")
	require.Len(t, x.calls, 2)
	require.Len(t, x.settled, 1)
	result, err := New(store).Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Equal(t, WaitingTool, result.State)
	require.Len(t, x.calls, 2, "the known paid replacement must be restored, not regenerated")
	require.Len(t, x.settled, 1)
	s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, x.calls[0].AttemptID, s.Attempts[1].RetryOf)
	require.Equal(t, RejectionAuth, s.Attempts[1].RecoveryKind)
	require.Equal(t, 1.0, s.TurnCostUSD)
}

func TestFusionNotStartedClaimCannotDiscardObservedUsageOnCancellation(t *testing.T) {
	store := newMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	x := &scriptExecutor{t: t, execute: func(c Call) (Outcome, error) {
		cancel()
		return Outcome{Response: textResponse("partial"), BillingModel: c.Request.Model, NotStarted: true, ClientDisconnected: true}, context.Canceled
	}}
	in := input(testRequest("task"))
	_, err := New(store).Advance(ctx, in, x)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, x.settled, 1)
	s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.False(t, s.Attempts[0].NotStarted)
	require.Equal(t, 1.0, s.Attempts[0].CostUSD)
	require.True(t, s.Attempts[0].Settled)
}

func TestFusionUnsentRecoveryRetainsAuthenticationLimit(t *testing.T) {
	store := newMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	x := &scriptExecutor{t: t}
	x.execute = func(c Call) (Outcome, error) {
		switch len(x.calls) {
		case 1:
			return refused(RejectionAuth, 0)
		case 2:
			cancel()
			return Outcome{KnownFailure: true, NotStarted: true}, context.Canceled
		default:
			require.Empty(t, c.RecoveryKind, "a resumed admission must not mint another credential")
			require.True(t, c.AuthRecoveryUsed)
			return refused(RejectionAuth, 0)
		}
	}
	in := input(testRequest("task"))
	_, err := New(store).Advance(ctx, in, x)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, x.calls, 2)
	_, err = New(store).Advance(context.Background(), in, x)
	requireCode(t, err, "recovery_exhausted")
	require.Len(t, x.calls, 3)
	require.Equal(t, x.calls[1].AttemptID, x.calls[2].RetryOf)
	require.True(t, x.calls[1].AuthRecoveryUsed)
}

type interruptedAdmissionStore struct {
	*memoryStore
	failure string
}

func (s *interruptedAdmissionStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	var a Attempt
	if s.failure == "journal" && strings.Contains(key, ":attempt:") && json.Unmarshal(value, &a) == nil && a.NotStarted {
		s.failure = ""
		return errors.New("paused admission journal unavailable")
	}
	return s.memoryStore.Set(ctx, key, value, ttl)
}

func (s *interruptedAdmissionStore) CompareAndSwap(ctx context.Context, key string, old, next []byte, ttl time.Duration) (bool, error) {
	fail := false
	if s.failure == "snapshot" {
		var snapshot Session
		fail = json.Unmarshal(next, &snapshot) == nil && len(snapshot.Attempts) == 2 && snapshot.Attempts[1].NotStarted
	}
	if s.failure == "budget" {
		var budget turnBudget
		if json.Unmarshal(next, &budget) == nil && len(budget.Entries) == 2 {
			fail = true
			for _, entry := range budget.Entries {
				fail = fail && entry.Settled && entry.Amount == 0
			}
		}
	}
	if fail {
		s.failure = ""
		return false, errors.New("paused admission persistence unavailable")
	}
	return s.memoryStore.CompareAndSwap(ctx, key, old, next, ttl)
}

func TestFusionInterruptedRecoveryAdmissionPersistenceNeverResetsAuthentication(t *testing.T) {
	for _, failure := range []string{"journal", "budget", "snapshot"} {
		t.Run(failure, func(t *testing.T) {
			store := &interruptedAdmissionStore{memoryStore: newMemoryStore(), failure: failure}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			x := &scriptExecutor{t: t}
			x.execute = func(c Call) (Outcome, error) {
				switch len(x.calls) {
				case 1:
					return refused(RejectionAuth, 0)
				case 2:
					cancel()
					return Outcome{KnownFailure: true, NotStarted: true}, context.Canceled
				default:
					require.Empty(t, c.RecoveryKind)
					require.True(t, c.AuthRecoveryUsed)
					return Outcome{Response: tool("read", "read_file", `{}`), BillingModel: c.Request.Model}, nil
				}
			}
			in := input(testRequest("task"))
			in.Profile.MaxCostUSD = 1.05
			_, err := New(store).Advance(ctx, in, x)
			require.ErrorContains(t, err, "unavailable")
			require.Len(t, x.calls, 2)
			result, err := New(store).Advance(context.Background(), in, x)
			require.NoError(t, err)
			require.Equal(t, WaitingTool, result.State)
			require.Len(t, x.calls, 3)
			require.Equal(t, x.calls[1].AttemptID, x.calls[2].RetryOf)
			require.Len(t, x.settled, 1)
		})
	}
}
