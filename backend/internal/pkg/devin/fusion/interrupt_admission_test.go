package fusion

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type interruptedAdmissionExecutor struct {
	awaitExecutor
	cancel  context.CancelFunc
	entries int
}

func (x *interruptedAdmissionExecutor) Execute(context.Context, Call) (Outcome, error) {
	x.entries++
	x.cancel()
	return Outcome{KnownFailure: true, NotStarted: true}, context.Canceled
}

type interruptedAdmissionJournalStore struct {
	AtomicStore
	fired, fail bool
	hook        func()
}

func (s *interruptedAdmissionJournalStore) Set(ctx context.Context, key string, data []byte, ttl time.Duration) error {
	var a Attempt
	if !s.fired && strings.Contains(key, ":attempt:") && json.Unmarshal(data, &a) == nil && a.NotStarted && a.Applied {
		s.fired = true
		s.hook()
		if s.fail {
			return errors.New("synthetic known-zero journal failure")
		}
	}
	return s.AtomicStore.Set(ctx, key, data, ttl)
}

func TestFusionNoSendCancellationFencesBeforePersistence(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "known_zero_journal_ok", true: "known_zero_journal_error"}[fail], func(t *testing.T) {
			e, s, c, clock, refresh := cacheFixture(t)
			base := testRequest("new task")
			s.Profile.Compaction = &CompactionPolicy{Enabled: false}
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
			s.TurnHandoffs, s.TurnRequests = 1, 2
			s.TurnCostUSD = .2
			_, raw, err := e.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			require.NoError(t, e.save(context.Background(), s, &raw))
			lead := testRequest("immutable Lead prefix")
			lead.Model = s.Profile.LeadModel
			require.NoError(t, c.Capture(context.Background(), s, lead, "previous-lead", "verified-route"))
			store := &interruptedAdmissionJournalStore{AtomicStore: e.Store, fail: fail}
			store.hook = func() {
				state, _, err := c.Load(context.Background(), s.Scope, s.ID)
				require.NoError(t, err)
				clock.Advance(11 * time.Second)
				require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
				t.Logf("during_known_zero_journal fail=%t cache=%s refresh_calls=%d", fail, state.State, refresh.calls)
				assert.Zero(t, refresh.calls, "known-no-send client interruption must fence before any receipt persistence")
			}
			e.Store = store
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			x := &interruptedAdmissionExecutor{cancel: cancel}
			_, err = e.Advance(ctx, Input{Scope: s.Scope, SessionID: s.ID, AccountID: s.AccountID, Profile: s.Profile, Request: base, IdempotencyKey: "original"}, x)
			require.ErrorIs(t, err, context.Canceled)
			require.True(t, store.fired)
			state, _, err := c.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			assert.Equal(t, "STOPPED", state.State)
			clock.Advance(11 * time.Second)
			require.NoError(t, c.Tick(context.Background(), s.Scope, s.ID, state.Epoch, state.WindowID))
			t.Logf("after_no_send_return fail=%t cache=%s execute_entries=%d generation_calls=%d refresh_calls=%d synthetic_refresh_charge=%.2f", fail, state.State, x.entries, len(x.calls), refresh.calls, refresh.charged)
			assert.Zero(t, refresh.calls)
			assert.Empty(t, x.calls)
			assert.Zero(t, x.charged)
			after, _, err := e.Load(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			assert.Equal(t, 3, after.TurnRequests, "the originally admitted no-send attempt remains counted")
			assert.Equal(t, 0, after.ActiveRequest, "an early interruption keeps exact request recovery")
			require.Equal(t, 1, len(after.Attempts))
			assert.True(t, after.Attempts[0].NotStarted)
			assert.True(t, after.Attempts[0].Settled)
			assert.Equal(t, 0.0, after.Attempts[0].CostUSD)
		})
	}
}
