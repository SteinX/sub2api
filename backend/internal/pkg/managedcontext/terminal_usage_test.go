package managedcontext

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	cc "github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
	"github.com/stretchr/testify/require"
)

type accountingExecutor struct {
	fakeExecutor
	outcome Outcome
	err     error
}

func (x *accountingExecutor) Execute(_ context.Context, c Call) (Outcome, error) {
	x.calls = append(x.calls, c)
	return x.outcome, x.err
}

func TestManagedFinalAccountingIsIndependentOfOutputValidation(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial_usage", true: "terminal_invalid_output"}[final], func(t *testing.T) {
			e := New(newMem())
			x := &accountingExecutor{outcome: Outcome{UsageFinal: final, Response: &apicompat.ResponsesResponse{Status: "in_progress", Usage: &apicompat.ResponsesUsage{InputTokens: 9, OutputTokens: 2, TotalTokens: 11}}}, err: errors.New("stream or output failed")}
			p := parsed(t, "preserve original task", "")
			_, err := run(t, e, x, "accounting", "", p)
			require.Error(t, err)
			op, _, err := load[Operation](context.Background(), e.Store, owner().Scope, "operation", "op_"+cc.Digest([]string{"responses:", "accounting"}))
			require.NoError(t, err)
			require.Len(t, op.Attempts, 1)
			a, _, err := load[Attempt](context.Background(), e.Store, owner().Scope, "attempt", op.Attempts[0])
			require.NoError(t, err)
			require.Equal(t, final, a.Settled)
			require.NotNil(t, a.UsageFinal)
			require.Equal(t, final, *a.UsageFinal)
			_, err = run(t, New(e.Store), x, "accounting", "", p)
			require.Error(t, err)
			require.Len(t, x.calls, 1)
			if final {
				require.Equal(t, "terminal", a.State)
				require.Equal(t, 1, x.charges)
			} else {
				require.Equal(t, "unknown", a.State)
				require.Zero(t, x.charges)
			}
		})
	}
}
