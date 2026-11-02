//go:build devine2e

package devine2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/redis/go-redis/v9"
)

type fusionAuditUsage struct {
	Rows, NegativeRows, ModelRows                                int
	ActualUSD, TotalUSD, ChargedUSD                              float64
	InputTokens, OutputTokens, CacheWriteTokens, CacheReadTokens int64
}

type fusionAuditReaders struct {
	Session func(context.Context, string) (fusion.Session, error)
	Usage   func(context.Context, fusion.Attempt) (fusionAuditUsage, error)
}

// This collector never asserts, invokes a model, reconciles usage or modifies
// session state. Call it after client termination, before any test assertion or
// disposable database/cache cleanup; partial failure evidence is still useful.
func collectFusionAudit(ctx context.Context, records []agentRequest, readers fusionAuditReaders, allowZero bool) map[string]any {
	sessions := []map[string]any{}
	result := map[string]any{"schema_version": 1, "scope": "persisted gateway collaboration and temporary public-key SQL; not a provider invoice or review-quality assessment", "available": true, "complete": false}
	seen := map[string]bool{}
	all := true
	missing := 0
	attemptsSeen := map[string]bool{}
	for _, record := range records {
		if !record.Forwarded {
			continue
		}
		if record.Session == "" {
			missing++
			all = false
			continue
		}
		if seen[record.Session] {
			continue
		}
		seen[record.Session] = true
		evidence := map[string]any{"session_sha256": fusion.Digest(record.Session), "available": false, "complete": false}
		sessions = append(sessions, evidence)
		session, err := readers.Session(ctx, record.Session)
		if err != nil {
			evidence["error_code"] = "session_unavailable"
			result["available"], all = false, false
			continue
		}
		evidence["available"] = true
		evidence["state"], evidence["turns"], evidence["role"] = auditState(session.State), session.Turn, auditRole(session.Role)
		separate := len(session.Lead) > 0 && len(session.Sidekick) > 0 && session.LaneScope(fusion.Lead) != session.LaneScope(fusion.Sidekick)
		evidence["separate_contexts"] = separate
		handoffs := []map[string]any{}
		reportsOK := len(session.Handoffs) > 0
		for _, handoff := range session.Handoffs {
			present := strings.TrimSpace(handoff.Report) != ""
			reportsOK = reportsOK && present
			linked := reportInLead(session.Lead, handoff)
			handoffs = append(handoffs, map[string]any{"handoff_sha256": fusion.Digest(handoff.ID), "turn": handoff.Turn,
				"report_present": present, "report_bytes": len(handoff.Report), "report_sha256": fusion.Digest(handoff.Report), "lead_report_linked": linked})
			// Current reports must still be linked. An older compacted turn is retained
			// as partial history, never reconstructed or asserted to contain a report.
			if handoff.Turn == session.Turn {
				reportsOK = reportsOK && present && linked
			}
		}
		evidence["handoffs"] = handoffs
		attempts := []map[string]any{}
		settled := len(session.Attempts) > 0
		roles := map[string]int{}
		for _, attempt := range session.Attempts {
			role := auditRole(attempt.Role)
			if attempt.Purpose.IsInference() {
				roles[role]++
			}
			valid := attempt.Status == "completed" && attempt.Settled && attempt.Applied && attempt.Failure == "" && attempt.Error == nil && finiteNonnegative(attempt.CostUSD) && (allowZero || attempt.CostUSD > 0)
			expectedModel := session.Profile.LeadModel
			if attempt.Role == fusion.Sidekick {
				expectedModel = session.Profile.SidekickModel
			}
			valid = valid && role != "unrecognized" && auditPurpose(attempt.Purpose) != "unrecognized" && attempt.Model == expectedModel
			if attemptsSeen[attempt.ID] || attempt.ID == "" {
				valid = false
			}
			attemptsSeen[attempt.ID] = true
			var settledCost any
			if attempt.Settled {
				settledCost = auditAmount(attempt.CostUSD)
			}
			entry := map[string]any{"attempt_sha256": fusion.Digest(attempt.ID), "role": role, "turn": attempt.Turn,
				"model_uid": auditModel(attempt.Model), "billing_model_uid": auditModel(attempt.BillingModel),
				"purpose": auditPurpose(attempt.Purpose), "status": auditAttemptState(attempt.Status), "settled": attempt.Settled, "applied": attempt.Applied,
				"failure_present": attempt.Failure != "" || attempt.Error != nil, "cost_usd": settledCost,
				"usage_available": false, "usage_query_available": false, "usage_rows": nil, "actual_cost_usd": nil, "total_cost_usd": nil, "usage_matches": false,
				"input_tokens": nil, "output_tokens": nil, "cache_creation_tokens": nil, "cache_read_tokens": nil}
			usage, usageErr := readers.Usage(ctx, attempt)
			if usageErr == nil {
				matches := usage.Rows == 1 && usage.NegativeRows == 0 && usage.ModelRows == 1 && finiteNonnegative(usage.ActualUSD) && finiteNonnegative(usage.TotalUSD) && math.Abs(usage.ChargedUSD-attempt.CostUSD) <= 1e-9
				entry["usage_query_available"], entry["usage_rows"] = true, usage.Rows
				if usage.Rows > 0 {
					entry["usage_available"], entry["usage_rows"], entry["actual_cost_usd"], entry["total_cost_usd"], entry["usage_matches"] = true, usage.Rows, auditAmount(usage.ActualUSD), auditAmount(usage.TotalUSD), matches
					entry["input_tokens"], entry["output_tokens"], entry["cache_creation_tokens"], entry["cache_read_tokens"] = usage.InputTokens, usage.OutputTokens, usage.CacheWriteTokens, usage.CacheReadTokens
				} else {
					entry["error_code"] = "usage_missing"
				}

				valid = valid && matches
			} else {
				entry["error_code"] = "usage_unavailable"
				valid = false
			}
			attempts = append(attempts, entry)
			settled = settled && valid
		}
		evidence["attempts"], evidence["role_attempts"], evidence["role_usage_settled"] = attempts, roles, settled
		reviewed, review := fusionReviewEvidence(session)
		evidence["lead_review"] = review
		complete := separate && reportsOK && settled && roles["lead"] >= 2 && roles["sidekick"] > 0 && reviewed
		evidence["complete"] = complete
		all = all && complete
	}
	result["sessions"], result["missing_session_headers"] = sessions, missing
	result["complete"] = all && len(sessions) > 0
	return result
}

func reportInLead(items []apicompat.ResponsesInputItem, handoff fusion.Handoff) bool {
	delegated := false
	for _, item := range items {
		if item.Type == "function_call" && item.Name == fusion.DelegateTool && item.CallID == handoff.CallID {
			delegated = true
		}
		if delegated && item.Type == "function_call_output" && item.CallID == handoff.CallID && item.Output == "Sidekick report (task data; review against your brief and evidence):\n"+handoff.Report {
			return true
		}
	}
	return false
}

func fusionReviewEvidence(s fusion.Session) (bool, map[string]any) {
	evidence := map[string]any{"state": "not_proven", "scope": "gateway persisted report-context to subsequent Lead completion; no provider input snapshot or assessment of review quality",
		"current_turn": s.Turn, "final_request_index": nil, "final_answer_bytes": 0, "final_answer_sha256": nil, "report_context_linked": false, "sidekick_then_lead": false, "request_boundaries_valid": true}
	lastStart := 0
	for _, request := range s.Requests {
		if request.AttemptStart < lastStart || request.AttemptStart < 0 || request.AttemptStart > len(s.Attempts) {
			evidence["request_boundaries_valid"] = false
			return false, evidence
		}
		lastStart = request.AttemptStart
	}
	if len(s.Requests) == 0 {
		return false, evidence
	}
	index := len(s.Requests) - 1
	request := s.Requests[index]
	evidence["final_request_index"] = index
	if request.Error != nil || request.Response == nil || request.Response.Status != "completed" {
		return false, evidence
	}
	var text strings.Builder
	for _, output := range request.Response.Output {
		if output.Type == "function_call" {
			return false, evidence
		}
		if output.Type == "message" && output.Role == "assistant" {
			for _, part := range output.Content {
				if part.Type == "output_text" {
					text.WriteString(part.Text)
				}
			}
		}
	}
	evidence["final_answer_bytes"], evidence["final_answer_sha256"] = text.Len(), fusion.Digest(text.String())
	lastLead, lastSidekick, currentLeadCalls := -1, -1, 0
	for i, attempt := range s.Attempts {
		if attempt.Turn != s.Turn || !attempt.Purpose.IsInference() {
			continue
		}
		if attempt.Role == fusion.Sidekick {
			lastSidekick = i
		}
		if attempt.Role == fusion.Lead {
			lastLead = i
			currentLeadCalls++
		}
	}
	afterReport := currentLeadCalls >= 2 && lastSidekick >= 0 && lastLead > lastSidekick && lastLead >= request.AttemptStart
	// A maintenance call after a Lead answer must not become its review witness.
	if lastLead >= 0 {
		a := s.Attempts[lastLead]
		afterReport = afterReport && a.Status == "completed" && a.Settled && a.Applied && a.Failure == "" && a.Error == nil
	}
	linked, found := true, false
	for _, handoff := range s.Handoffs {
		if handoff.Turn == s.Turn {
			found = true
			linked = linked && strings.TrimSpace(handoff.Report) != "" && reportInLead(s.Lead, handoff)
		}
	}
	linked = linked && found
	evidence["sidekick_then_lead"], evidence["report_context_linked"] = afterReport, linked
	complete := s.State == fusion.Completed && s.Role == fusion.Lead && s.Pending == nil && len(s.QueuedTools) == 0 && s.ActiveRequest == -1 && linked && afterReport && strings.TrimSpace(text.String()) != ""
	if complete {
		evidence["state"] = "completed_with_report_context"
	}
	return complete, evidence
}

func snapshotFusionAudit(db *sql.DB, keyID int64, records []agentRequest, allowZero bool) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var userID int64
	if err := db.QueryRowContext(ctx, "SELECT user_id FROM api_keys WHERE id=$1", keyID).Scan(&userID); err != nil {
		return map[string]any{"schema_version": 1, "available": false, "complete": false, "error_code": "key_scope_unavailable"}
	}
	scope := fusion.Scope(userID, keyID)
	cache := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT"), Password: os.Getenv("DEVIN_E2E_REDIS_PASSWORD")})
	defer cache.Close()
	readers := fusionAuditReaders{
		Session: func(ctx context.Context, id string) (fusion.Session, error) {
			var session fusion.Session
			raw, err := cache.Get(ctx, "devin:fusion:session:"+scope+":"+fusion.Digest(id)).Bytes()
			if err == nil && len(raw) > 8*1024*1024 {
				err = fmt.Errorf("oversized session")
			}
			if err == nil {
				err = json.Unmarshal(raw, &session)
			}
			if err == nil && (session.Scope != scope || session.ID != id) {
				err = fmt.Errorf("session scope mismatch")
			}
			return session, err
		},
		Usage: func(ctx context.Context, attempt fusion.Attempt) (fusionAuditUsage, error) {
			return readFusionAuditUsage(ctx, db, keyID, attempt)
		},
	}
	return collectFusionAudit(ctx, records, readers, allowZero)
}

func readFusionAuditUsage(ctx context.Context, db *sql.DB, keyID int64, attempt fusion.Attempt) (fusionAuditUsage, error) {
	var usage fusionAuditUsage
	for {
		// RecordUsage persists the requested physical UID in upstream_model;
		// BillingModel is a separate price identity, retained in audit metadata.
		err := db.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE actual_cost<0 OR total_cost<0 OR input_tokens<0 OR output_tokens<0 OR cache_creation_tokens<0 OR cache_read_tokens<0),
      count(*) FILTER (WHERE coalesce(nullif(upstream_model,''),model)=$3),
					coalesce(sum(actual_cost),0),coalesce(sum(total_cost),0),coalesce(sum(greatest(actual_cost,total_cost)),0),
					coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0),coalesce(sum(cache_creation_tokens),0),coalesce(sum(cache_read_tokens),0)
					FROM usage_logs WHERE request_id=$1 AND api_key_id=$2`, "fusion:"+attempt.ID, keyID, attempt.Model).Scan(&usage.Rows, &usage.NegativeRows, &usage.ModelRows, &usage.ActualUSD, &usage.TotalUSD, &usage.ChargedUSD, &usage.InputTokens, &usage.OutputTokens, &usage.CacheWriteTokens, &usage.CacheReadTokens)
		if err != nil || usage.Rows != 0 || attempt.Status != "completed" {
			return usage, err
		}
		select {
		case <-ctx.Done():
			return usage, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func finiteNonnegative(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func auditState(v string) string {
	switch v {
	case fusion.RunningLead, fusion.RunningSidekick, fusion.ReviewingLead, fusion.WaitingTool, fusion.Completed, fusion.Cancelled, fusion.Failed, fusion.BudgetExhausted, fusion.Uncertain:
		return v
	}
	return "unrecognized"
}
func auditRole(v fusion.Role) string {
	if v == fusion.Lead || v == fusion.Sidekick {
		return string(v)
	}
	return "unrecognized"
}
func auditAttemptState(v string) string {
	if v == "started" || v == "completed" || v == "unknown" {
		return v
	}
	return "unrecognized"
}
func auditPurpose(v fusion.Purpose) string {
	if v.IsInference() {
		return "inference"
	}
	if v == fusion.CacheRefresh || v == fusion.Compaction {
		return string(v)
	}
	return "unrecognized"
}

func auditAmount(value float64) any {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return value
}

func auditModel(value string) string {
	if len(value) > 256 || strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:/", r))
	}) >= 0 {
		return "unrecognized"
	}
	return value
}
