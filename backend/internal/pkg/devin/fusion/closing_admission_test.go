package fusion

import (
	"context"
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestFusionClosingSmallBudgetAdmission(t *testing.T) {
	for _, limit := range []int{3, 4} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			in := input(testRequest("report evidence"))
			in.Profile = DefaultProfile("fusion-test", "lead-real", "worker-real")
			in.Profile.MaxRequests = limit
			in.ClosingPolicy = ClosingReserveV1
			x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), textResponse("actual report"), textResponse("reviewed")}}
			store := newMemoryStore()
			_, err := New(store).Advance(context.Background(), in, x)
			t.Logf("limit=%d executor_calls=%d settled_calls=%d error=%v", limit, len(x.calls), len(x.settled), err)
			requireCode(t, err, "closing_policy_invalid")
			require.Empty(t, x.calls, "reject before the first physical inference")
			require.Empty(t, x.settled)
			_, _, err = New(store).Load(context.Background(), in.Scope, in.SessionID)
			require.Error(t, err, "no session should have been persisted")
		})
	}
}

func TestFusionClosingSmallBudgetStillFinishesWhenSupported(t *testing.T) {
	for _, tc := range []struct {
		limit   int
		compact bool
		optIn   bool
	}{{3, false, true}, {4, false, true}, {5, true, true}, {3, true, false}, {4, true, false}} {
		t.Run(fmt.Sprintf("limit%d_compact%t_optin%t", tc.limit, tc.compact, tc.optIn), func(t *testing.T) {
			in := input(testRequest("report evidence"))
			in.Profile = DefaultProfile("fusion-test", "lead-real", "worker-real")
			in.Profile.MaxRequests = tc.limit
			in.Profile.Compaction = &CompactionPolicy{Enabled: tc.compact}
			if tc.optIn {
				in.ClosingPolicy = ClosingReserveV1
			}
			x := &scriptExecutor{t: t, responses: []*apicompat.ResponsesResponse{delegation("d"), textResponse("actual report"), textResponse("reviewed")}}
			store := newMemoryStore()
			out, err := New(store).Advance(context.Background(), in, x)
			require.NoError(t, err)
			require.Equal(t, Completed, out.State)
			require.Len(t, x.calls, 3)
			require.Len(t, x.settled, 3)
			s, _, err := New(store).Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			require.Equal(t, 1, s.Turn)
			require.Equal(t, tc.limit, s.Profile.MaxRequests)
			require.Equal(t, "actual report", s.Handoffs[0].Report)
			require.Equal(t, 3, s.TurnRequests)
			require.Equal(t, 5.0, s.Profile.MaxCostUSD)
			require.Equal(t, 900, s.Profile.MaxWallSeconds)
		})
	}
}
