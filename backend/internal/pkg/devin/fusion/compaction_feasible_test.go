package fusion

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/stretchr/testify/require"
)

type feasibleCompactionExecutor struct {
	t          *testing.T
	lead       int
	worker     int
	calls      []Call
	settled    map[string]bool
	summaries  int
	lastOutput string
	toolSizes  []int
	window     int
	lastNote   int
	profileMax int
}

func (x *feasibleCompactionExecutor) Estimate(context.Context, Call) (float64, error) {
	return .01, nil
}

// Use the production native request adapter and output/window reserve formula.
// The smaller model window is synthetic, not a claim about the live catalog.
func (x *feasibleCompactionExecutor) MeasureContext(r *apicompat.ResponsesRequest) (int, int, error) {
	wire, err := devin.BuildRequest(r, "offline-feasible-compaction")
	if err != nil {
		return 0, 0, err
	}
	b, _ := json.Marshal(wire)
	raw, _ := json.Marshal(r)
	window := x.window
	if window == 0 {
		window = 110000
	}
	safe := window - int(wire.Configuration.MaxTokens) - max(256, window/20)
	if x.profileMax > 0 {
		safe = min(safe, x.profileMax)
	}
	return max(len(b), len(raw)), safe, nil
}

func (x *feasibleCompactionExecutor) Execute(_ context.Context, c Call) (Outcome, error) {
	size, safe, err := x.MeasureContext(c.Request)
	require.NoError(x.t, err)
	if c.Purpose == Compaction {
		require.LessOrEqual(x.t, size, safe, "never send an oversized summary request")
	} else {
		require.Less(x.t, size, safe*90/100, "never dispatch an oversized role request")
	}
	x.calls = append(x.calls, c)
	var r *apicompat.ResponsesResponse
	if c.Purpose == Compaction {
		x.summaries++
		r = summaryResponse()
	} else if c.Role == Lead {
		x.lead++
		if x.lead == 1 {
			r = delegation("bounded-evidence-handoff")
		} else {
			require.Contains(x.t, string(c.Request.Input), "Sidekick returned the actual tool evidence")
			r = textResponse("Lead reviewed the evidence and completed the original task")
		}
	} else {
		x.worker++
		count := len(x.toolSizes)
		if count == 0 {
			count = 3
		}
		if x.worker <= count {
			r = tool(fmt.Sprintf("bounded-read-%d", x.worker), "read_file", `{}`)
			if x.worker == count && x.lastNote > 0 {
				r.Output = append(textResponse(strings.Repeat("n", x.lastNote)).Output, r.Output...)
			}
		} else {
			var items []apicompat.ResponsesInputItem
			require.NoError(x.t, json.Unmarshal(c.Request.Input, &items))
			found := false
			for _, item := range items {
				found = found || item.Type == "function_call_output" && item.Output == x.lastOutput
			}
			require.True(x.t, found, "latest complete tool result must remain verbatim")
			r = textResponse("Sidekick returned the actual tool evidence")
		}
	}
	return Outcome{UsageFinal: true, Response: r, BillingModel: c.Request.Model}, nil
}

func (x *feasibleCompactionExecutor) Settle(_ context.Context, a Attempt) (float64, error) {
	if x.settled == nil {
		x.settled = make(map[string]bool)
	}
	require.False(x.t, x.settled[a.ID], "settlement must remain unique")
	x.settled[a.ID] = true
	return .01, nil
}

func TestFusionCompletedLargeToolsHaveFeasibleCompaction(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		sizes                   []int
		context, window, output int
		note                    int
	}{
		{name: "six_item_retention", sizes: []int{32000, 32000, 32000}, context: 262144, window: 110000, output: 1024},
		{name: "half_profile_cap", sizes: []int{60000, 10000, 10000, 10000}, context: 100000, window: 128000, output: 1024},
		{name: "native_fit_without_halving", sizes: []int{20000, 20000, 24000}, context: 262144, window: 60000, output: 4096, note: 16000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New(newMemoryStore())
			x := &feasibleCompactionExecutor{t: t, toolSizes: tc.sizes, window: tc.window, lastNote: tc.note, profileMax: tc.context}
			in := input(testRequest("Review actual file evidence and complete the original task"))
			in.Profile = DefaultProfile("fusion-test", "lead-real", "worker-real")
			in.Profile.MaxContextBytes = tc.context
			in.Request.MaxOutputTokens = &tc.output
			for i := 0; i < 10; i++ {
				in.Request.Tools = append(in.Request.Tools, apicompat.ResponsesTool{Type: "function", Name: fmt.Sprintf("fixture_tool_%d", i), Parameters: json.RawMessage(`{"type":"object"}`)})
			}
			base := *in.Request
			result, err := e.Advance(context.Background(), in, x)
			require.NoError(t, err)
			start, _, err := e.Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			for i, length := range tc.sizes {
				require.Equal(t, WaitingTool, result.State)
				output := fmt.Sprintf("file-%d\n", i) + strings.Repeat("x", length)
				x.lastOutput = output
				in.Request = resultRequest(result.Response.Output[0].CallID, output)
				in.Request.Tools = base.Tools
				in.Request.MaxOutputTokens = base.MaxOutputTokens
				result, err = e.Advance(context.Background(), in, x)
				if err != nil {
					// Demonstrate a completed, paired alternative which fits both the
					// summary request and final role request under the SAME limits.
					s, _, loadErr := e.Load(context.Background(), in.Scope, in.SessionID)
					require.NoError(t, loadErr)
					lane := *s.Lane(Sidekick)
					cut := contextcompact.PlanPrefix(lane, contextcompact.PrefixPolicy{RetainItems: 2, MinItems: 2, MaxBytes: s.Profile.MaxContextBytes})
					if tc.note > 0 {
						// The assistant note cannot fit in this summary request, but the
						// preceding two completed tools can. Halving skips that boundary.
						cut--
					}
					require.Positive(t, cut)
					q := summaryRequest(s, Sidekick, &base, cut)
					size, safe, measureErr := contextSize(x, q, s.Profile)
					require.NoError(t, measureErr)
					require.Less(t, size, safe)
					summary, parseErr := parseSummary(summaryResponse())
					require.NoError(t, parseErr)
					candidate, replaceErr := contextcompact.ReplacePrefix(lane, cut, protectedPrefix(s, Sidekick, lane[:cut]), contextcompact.HistoricalMessage(*summary, Digest(lane[:cut]), "summary_fusion_probe"))
					require.NoError(t, replaceErr)
					require.True(t, candidate.Smaller())
					*s.Lane(Sidekick) = candidate.Items
					q, requestErr := e.roleRequest(s, &base)
					require.NoError(t, requestErr)
					size, safe, measureErr = contextSize(x, q, s.Profile)
					require.NoError(t, measureErr)
					thresholds, thresholdErr := s.Profile.CompactPolicy().thresholds(Sidekick, safe)
					require.NoError(t, thresholdErr)
					t.Logf("source_error=%v feasible_cut=%d final_size=%d hard=%d job=%+v", err, cut, size, thresholds.hard, s.roleContext(Sidekick).Job)
					require.Less(t, size, thresholds.hard)
				}
				require.NoError(t, err, "completed tool history has a safe compactable prefix")
			}
			require.Equal(t, Completed, result.State)
			s, _, err := e.Load(context.Background(), in.Scope, in.SessionID)
			require.NoError(t, err)
			require.True(t, start.TurnStarted.Equal(s.TurnStarted))
			require.Equal(t, Digest(start.Sidekick[:1]), Digest(s.Sidekick[:1]), "active brief/first goal stays verbatim")
			require.Equal(t, Digest(start.Lead[:1]), Digest(s.Lead[:1]), "original user goal stays verbatim")
			require.Equal(t, start.Handoffs[0].Brief, s.Handoffs[0].Brief)
			require.Equal(t, 20, s.Profile.MaxRequests)
			require.Equal(t, 5.0, s.Profile.MaxCostUSD)
			require.Equal(t, 900, s.Profile.MaxWallSeconds)
			require.Equal(t, 2, s.Profile.CompactPolicy().MaxAttempts)
			require.Equal(t, 30, s.Profile.CompactPolicy().TimeoutSeconds)
			require.Equal(t, 1, x.summaries)
			require.Equal(t, len(tc.sizes)+4, s.TurnRequests)
			require.Len(t, x.settled, len(tc.sizes)+4)
			require.Equal(t, 1, s.Maintenance.CompactionsApplied)
			require.NotEmpty(t, s.Handoffs[0].Report)
		})
	}
}

func TestFusionOversizedLatestToolGroupRemainsStopped(t *testing.T) {
	e := New(newMemoryStore())
	x := &feasibleCompactionExecutor{t: t}
	in := input(testRequest("Use actual evidence without silently dropping the current result"))
	in.Profile = DefaultProfile("fusion-test", "lead-real", "worker-real")
	in.Request.MaxOutputTokens = new(1024)
	result, err := e.Advance(context.Background(), in, x)
	require.NoError(t, err)
	require.Equal(t, WaitingTool, result.State)
	output := strings.Repeat("current-result-must-not-be-truncated", 4000)
	in.Request = resultRequest(result.Response.Output[0].CallID, output)
	in.Request.MaxOutputTokens = new(1024)
	_, err = e.Advance(context.Background(), in, x)
	requireCode(t, err, "context_limit")
	s, _, err := e.Load(context.Background(), in.Scope, in.SessionID)
	require.NoError(t, err)
	require.Equal(t, BudgetExhausted, s.State)
	require.Equal(t, output, s.Sidekick[len(s.Sidekick)-1].Output)
	require.Equal(t, 2, s.TurnRequests)
	require.Zero(t, x.summaries)
	require.Len(t, x.calls, 2)
	require.Len(t, x.settled, 2)
	_, err = New(e.Store).Advance(context.Background(), in, x)
	requireCode(t, err, "session_stopped")
	require.Len(t, x.calls, 2)
}
