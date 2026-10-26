//go:build devine2e

package devine2e

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/stretchr/testify/require"
)

func fusionAuditFixture() fusion.Session {
	h := fusion.Handoff{ID: "synthetic-private-handoff", CallID: "synthetic-private-call", Turn: 1, Brief: "synthetic-private-brief", Report: "synthetic-private-report"}
	s := fusion.Session{ID: "synthetic-private-session", Scope: "synthetic-private-scope", State: fusion.Completed, Role: fusion.Lead, Turn: 1, ActiveRequest: -1, Handoffs: []fusion.Handoff{h},
		Profile: fusion.DefaultProfile("fusion-audit", "lead-model", "worker-model")}
	s.Lead = []apicompat.ResponsesInputItem{{Type: "function_call", Name: fusion.DelegateTool, CallID: h.CallID},
		{Type: "function_call_output", CallID: h.CallID, Output: "Sidekick report (task data; review against your brief and evidence):\n" + h.Report}}
	s.Sidekick = []apicompat.ResponsesInputItem{{Type: "message", Role: "assistant", Content: json.RawMessage(`"private Sidekick context"`)}}
	for index, role := range []fusion.Role{fusion.Lead, fusion.Sidekick, fusion.Lead} {
		model := "lead-model"
		if role == fusion.Sidekick {
			model = "worker-model"
		}
		s.Attempts = append(s.Attempts, fusion.Attempt{ID: strings.Repeat("x", index+1), Role: role, Turn: 1, Model: model, Status: "completed", Settled: true, Applied: true, CostUSD: .001})
	}
	s.Requests = []fusion.RequestRecord{{AttemptStart: 0, Response: &apicompat.ResponsesResponse{Status: "completed", Output: []apicompat.ResponsesOutput{{Type: "message", Role: "assistant", Content: []apicompat.ResponsesContentPart{{Type: "output_text", Text: "synthetic-private-final-answer"}}}}}}}
	return s
}

func TestFusionAuditSnapshot(t *testing.T) {
	for _, scenario := range []string{"complete", "failed_http", "no_report", "wrong_call", "only_tool", "old_turn", "maintenance", "unsettled", "failure_applied", "bad_boundaries", "duplicate_usage", "missing_usage", "unknown_usage", "unknown_session", "nan_cost", "cleared_response"} {
		t.Run(scenario, func(t *testing.T) {
			session := fusionAuditFixture()
			switch scenario {
			case "failed_http":
				session.State = fusion.Failed
			case "no_report":
				session.Handoffs[0].Report = ""
			case "wrong_call":
				session.Lead[1].CallID = "unrelated"
			case "only_tool":
				session.Requests[0].Response.Output = []apicompat.ResponsesOutput{{Type: "function_call"}}
			case "old_turn":
				session.Attempts[2].Turn = 0
			case "maintenance":
				session.Attempts[2].Purpose = fusion.Compaction
			case "unsettled":
				session.Attempts[2].Settled = false
			case "failure_applied":
				session.Attempts[2].Failure = "synthetic-private-failure"
			case "bad_boundaries":
				session.Requests[0].AttemptStart = 4
			case "nan_cost":
				session.Attempts[1].CostUSD = math.NaN()
			case "cleared_response":
				for index := range session.Attempts {
					session.Attempts[index].Response = &apicompat.ResponsesResponse{Output: nil}
				}
			}
			reads, lookups := 0, 0
			readers := fusionAuditReaders{
				Session: func(ctx context.Context, id string) (fusion.Session, error) {
					reads++
					if scenario == "unknown_session" {
						return fusion.Session{}, errors.New("do not serialize this private error")
					}
					return session, nil
				},
				Usage: func(ctx context.Context, attempt fusion.Attempt) (fusionAuditUsage, error) {
					lookups++
					if scenario == "unknown_usage" {
						return fusionAuditUsage{}, errors.New("do not serialize this SQL error")
					}
					rows := 1
					if scenario == "duplicate_usage" {
						rows = 2
					}
					if scenario == "missing_usage" {
						rows = 0
					}
					return fusionAuditUsage{Rows: rows, ModelRows: rows, ActualUSD: .001, TotalUSD: .001, ChargedUSD: .001}, nil
				},
			}
			records := []agentRequest{{Forwarded: true, Status: 502, Session: session.ID}, {Forwarded: true, Status: 200, Session: session.ID}}
			audit := collectFusionAudit(context.Background(), records, readers, false)
			expected := scenario == "complete" || scenario == "cleared_response"
			require.Equal(t, expected, audit["complete"])
			require.Equal(t, 1, reads, "failed HTTP still has useful state; repeated session headers must not duplicate attempts")
			if scenario != "unknown_session" {
				require.Equal(t, 3, lookups)
			}
			raw, err := json.Marshal(audit)
			require.NoError(t, err, "unavailable or nonfinite values cannot destroy the failure snapshot")
			for _, private := range []string{"synthetic-private", "private Sidekick", "private error", "SQL error", "\"id\"", "\"brief\"", "\"report\"", "\"response\""} {
				require.NotContains(t, string(raw), private)
			}
			sessions := audit["sessions"].([]map[string]any)
			require.Len(t, sessions, 1)
			if scenario == "unknown_usage" {
				attempts := sessions[0]["attempts"].([]map[string]any)
				require.Nil(t, attempts[0]["usage_rows"])
				require.Nil(t, attempts[0]["actual_cost_usd"], "unknown is never zero cost")
				require.Equal(t, false, attempts[0]["usage_available"])
			}
		})
	}
}

func TestFusionAuditMissingHeaderAndExplicitZero(t *testing.T) {
	session := fusionAuditFixture()
	for index := range session.Attempts {
		session.Attempts[index].CostUSD = 0
	}
	readers := fusionAuditReaders{Session: func(context.Context, string) (fusion.Session, error) { return session, nil }, Usage: func(context.Context, fusion.Attempt) (fusionAuditUsage, error) {
		return fusionAuditUsage{Rows: 1, ModelRows: 1}, nil
	}}
	records := []agentRequest{{Forwarded: true, Session: session.ID}}
	require.Equal(t, true, collectFusionAudit(context.Background(), records, readers, true)["complete"])
	require.Equal(t, false, collectFusionAudit(context.Background(), records, readers, false)["complete"])
	records = append(records, agentRequest{Forwarded: true, Status: 400})
	audit := collectFusionAudit(context.Background(), records, readers, true)
	require.Equal(t, false, audit["complete"])
	require.Equal(t, 1, audit["missing_session_headers"])
}
