package fusion

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/google/uuid"
)

const summaryPolicy = "fusion-summary-v1"

type historySummary = contextcompact.Summary

// Keep the Fusion error namespace and persisted v1 summary contract stable.
func compactionError(err error) error {
	var compact *contextcompact.Error
	if errors.As(err, &compact) {
		return E(compact.Code, compact.Message, compact.Status)
	}
	return err
}

type compactionRecord struct {
	Job     CompactionJob               `json:"job"`
	Attempt Attempt                     `json:"attempt"`
	Request *apicompat.ResponsesRequest `json:"request,omitempty"`
	Summary *historySummary             `json:"summary,omitempty"`
}

func compactKey(s *Session, id string) string { return indexKey(s.Scope, "compaction", s.ID+":"+id) }
func (e *Engine) readCompaction(ctx context.Context, s *Session, j *CompactionJob) (*compactionRecord, []byte, error) {
	b, err := e.Store.Get(ctx, compactKey(s, j.ID))
	if err != nil {
		return nil, nil, err
	}
	var r compactionRecord
	if json.Unmarshal(b, &r) != nil || r.Job.ID != j.ID || r.Job.PrefixDigest != j.PrefixDigest || r.Job.ProfileRevision != s.ProfileRevision || r.Job.Policy == "" || r.Job.Policy != j.Policy || r.Attempt.Purpose != Compaction {
		return nil, nil, E("session_corrupt", "invalid compaction attempt journal", 503)
	}
	return &r, b, nil
}
func (e *Engine) writeCompaction(ctx context.Context, s *Session, r *compactionRecord, old []byte) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	ok, err := e.Store.CompareAndSwap(ctx, compactKey(s, r.Job.ID), old, b, max(s.ExpiresAt.Sub(e.now())+24*time.Hour, time.Hour))
	if err != nil {
		return err
	}
	if !ok {
		return E("revision_conflict", "compaction result changed concurrently; retry the original request", 409)
	}
	return nil
}
func contextSize(x Executor, req *apicompat.ResponsesRequest, p Profile) (int, int, error) {
	var measure contextcompact.MeasureRequest
	if m, ok := x.(ContextMeasurer); ok {
		measure = m.MeasureContext
	}
	budget, err := contextcompact.Measure(req, p.MaxContextBytes, measure)
	return budget.InputEstimate, budget.SafeInput, compactionError(err)
}

// A cut must contain complete call/result groups. Initial instructions/user goal
// and the active handoff remain verbatim; pending calls always stay in the tail.
func safeSummaryPrefix(s *Session, role Role, limit int) int {
	return contextcompact.PlanPrefix(*s.Lane(role), contextcompact.PrefixPolicy{RetainItems: 6, MinItems: 2, MaxBytes: limit})
}
func protectedPrefix(s *Session, role Role, lane []apicompat.ResponsesInputItem) []apicompat.ResponsesInputItem {
	policy := contextcompact.Protection{AllUserMessages: role == Lead, SummaryIDPrefix: "summary_fusion_"}
	if role == Sidekick && len(s.Handoffs) > 0 && s.Handoffs[len(s.Handoffs)-1].Turn == s.Turn {
		policy.ActiveUserMarker = s.Handoffs[len(s.Handoffs)-1].ID
	}
	if role == Lead && len(s.Handoffs) > 0 && s.Handoffs[len(s.Handoffs)-1].Turn == s.Turn {
		policy.ActiveToolCallID = s.Handoffs[len(s.Handoffs)-1].CallID
	}
	return contextcompact.ProtectedItems(lane, policy)
}
func summaryRequest(s *Session, role Role, base *apicompat.ResponsesRequest, cut int) *apicompat.ResponsesRequest {
	model := s.Profile.LeadModel
	if role == Sidekick {
		model = s.Profile.SidekickModel
	}
	n := min(s.Profile.CompactPolicy().MaxOutputTokens, s.Profile.MaxOutputTokens)
	if base.MaxOutputTokens != nil {
		n = min(n, *base.MaxOutputTokens)
	}
	return contextcompact.BuildSummaryRequest((*s.Lane(role))[:cut], model, n)
}
func parseSummary(r *apicompat.ResponsesResponse) (*historySummary, error) {
	summary, err := contextcompact.ParseSummary(r)
	return summary, compactionError(err)
}

func (e *Engine) performCompaction(ctx context.Context, s *Session, r *compactionRecord, x Executor) {
	old, _ := json.Marshal(r)
	if ctx.Err() != nil {
		r.Attempt.Status = "completed"
		r.Attempt.Settled = true
		r.Job.State = "cancelled"
		r.Request = nil
		_ = e.writeCompaction(context.WithoutCancel(ctx), s, r, old)
		return
	}
	r.Attempt.Status = "started"
	if e.writeCompaction(ctx, s, r, old) != nil {
		return
	}
	old, _ = json.Marshal(r)
	call := Call{AttemptID: r.Attempt.ID, Purpose: Compaction, Role: r.Job.Role, Request: r.Request, LaneScope: s.LaneScope(r.Job.Role) + ":summary:" + r.Job.ID, PricingAt: r.Attempt.StartedAt, EstimateUSD: r.Attempt.EstimateUSD}
	outcome, err := x.Execute(ctx, call)
	r.Attempt.Response = outcome.Response
	r.Attempt.BillingModel = outcome.BillingModel
	r.Request = nil
	if outcome.Response != nil && outcome.Response.Usage != nil {
		r.Attempt.Status = "completed"
	} else if outcome.KnownFailure {
		r.Attempt.Status = "completed"
		r.Attempt.Settled = true
	} else {
		r.Attempt.Status = "unknown"
	}
	if err != nil {
		r.Attempt.Failure = "compaction_failed"
	}
	r.Job.State = "failed"
	if err == nil {
		r.Summary, err = parseSummary(outcome.Response)
		if err == nil {
			r.Job.State = "ready"
		}
	}
	if r.Attempt.Status == "unknown" {
		r.Job.State = "unknown"
	}
	// Save observed usage before billing. A restarted foreground can settle this
	// journal without issuing another summary request.
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if e.writeCompaction(persist, s, r, old) != nil {
		if r.Attempt.Status == "completed" && !r.Attempt.Settled {
			_, _ = x.Settle(persist, r.Attempt)
		}
		return
	}
	old, _ = json.Marshal(r)
	if r.Attempt.Status == "completed" && !r.Attempt.Settled {
		cost, billErr := x.Settle(persist, r.Attempt)
		if billErr == nil && validCost(cost) {
			r.Attempt.CostUSD = cost
			r.Attempt.Settled = true
			_ = e.writeCompaction(persist, s, r, old)
			_ = e.settleBudget(persist, s, r.Attempt)
		}
	}
}

func (e *Engine) collectCompactions(ctx context.Context, s *Session, raw *[]byte, x Executor) error {
	var pendingErr error
	for _, c := range s.Contexts {
		j := c.Job
		if j == nil || j.Accounted {
			continue
		}
		r, old, err := e.readCompaction(ctx, s, j)
		if errors.Is(err, devin.ErrSessionNotFound) {
			pendingErr = errors.Join(pendingErr, E("compaction_pending", "summary preparation was interrupted before its journal; retry this request", 503))
			continue
		}
		if err != nil {
			pendingErr = errors.Join(pendingErr, err)
			continue
		}
		if r.Attempt.Status == "prepared" || r.Attempt.Status == "started" {
			if e.now().Before(j.Deadline) {
				continue
			}
			if r.Attempt.Status == "prepared" {
				r.Attempt.Status = "completed"
				r.Attempt.Settled = true
				r.Job.State = "failed"
			} else {
				r.Attempt.Status = "unknown"
				r.Job.State = "unknown"
			}
			if err = e.writeCompaction(ctx, s, r, old); err != nil {
				return err
			}
			old, _ = json.Marshal(r)
		}
		if r.Attempt.Status == "unknown" {
			pendingErr = errors.Join(pendingErr, E("compaction_uncertain", "summary provider outcome is unknown; no inference will be automatically replayed", 409))
			continue
		}
		if r.Attempt.Status != "completed" {
			continue
		}
		if !r.Attempt.Settled {
			cost, billErr := x.Settle(ctx, r.Attempt)
			if billErr != nil || !validCost(cost) {
				pendingErr = errors.Join(pendingErr, E("billing_pending", "compaction usage settlement is pending", 503))
				continue
			}
			r.Attempt.Settled = true
			r.Attempt.CostUSD = cost
			if err = e.writeCompaction(ctx, s, r, old); err != nil {
				return err
			}
		}
		if err = e.settleBudget(ctx, s, r.Attempt); err != nil {
			return err
		}
		j.State = r.Job.State
		j.Accounted = true
		s.AuxReservedUSD = max(0, s.AuxReservedUSD-j.EstimateUSD)
		if j.Turn == s.Turn {
			s.TurnCostUSD += r.Attempt.CostUSD
		}
		s.Maintenance.CompactionCostUSD += r.Attempt.CostUSD
		combined := aggregateAllUsage([]Attempt{{Response: &apicompat.ResponsesResponse{Usage: s.Maintenance.CompactionUsage}}, r.Attempt})
		s.Maintenance.CompactionUsage = combined
		if j.State != "ready" {
			s.Maintenance.CompactionsFailed++
		}
		e.observe(MaintenanceEvent{Purpose: Compaction, Role: j.Role, Stage: j.State, JobID: j.ID})
		if err = e.save(ctx, s, raw); err != nil {
			return err
		}
	}
	return pendingErr
}

func (e *Engine) applyCompaction(ctx context.Context, s *Session, raw *[]byte, role Role, base *apicompat.ResponsesRequest, x Executor) (bool, error) {
	c := s.roleContext(role)
	j := c.Job
	if j == nil || !j.Accounted || j.State != "ready" {
		return false, nil
	}
	if cancelled, err := e.cancelled(ctx, s); err != nil {
		return false, err
	} else if cancelled || s.State == Cancelled {
		j.State = "cancelled"
		return false, e.save(ctx, s, raw)
	}
	if s.Pending != nil && s.Pending.Role == role {
		return false, E("tool_pending", "summary application must wait for the real pending tool result", 409)
	}
	lane := s.Lane(role)
	source := contextcompact.Snapshot{Scope: s.Scope, SessionID: s.ID, Lane: contextcompact.Lane(j.Role), Policy: j.Policy, Revision: j.ProfileRevision, Epoch: j.Epoch, PrefixCount: j.PrefixCount, PrefixDigest: j.PrefixDigest}
	current := contextcompact.Snapshot{Scope: s.Scope, SessionID: s.ID, Lane: contextcompact.Lane(role), Policy: summaryPolicy, Revision: s.ProfileRevision, Epoch: c.Epoch}
	if !source.Matches(current, *lane) {
		j.State = "stale"
		s.Maintenance.CompactionsStale++
		e.observe(MaintenanceEvent{Purpose: Compaction, Role: role, Stage: "stale", JobID: j.ID})
		return false, e.save(ctx, s, raw)
	}
	r, _, err := e.readCompaction(ctx, s, j)
	if err != nil {
		return false, err
	}
	if r.Summary == nil {
		return false, E("session_corrupt", "ready summary is missing its structured result", 503)
	}
	prefix := append([]apicompat.ResponsesInputItem(nil), (*lane)[:j.PrefixCount]...)
	message := contextcompact.HistoricalMessage(*r.Summary, j.PrefixDigest, "summary_fusion_"+j.ID)
	replacement, err := contextcompact.ReplacePrefix(*lane, j.PrefixCount, protectedPrefix(s, role, prefix), message)
	if err != nil {
		return false, compactionError(err)
	}
	if !replacement.Smaller() {
		j.State = "failed"
		j.Reason = "not_smaller"
		s.Maintenance.CompactionsFailed++
		return false, e.save(ctx, s, raw)
	}
	oldLane := *lane
	*lane = replacement.Items
	copySession := *s
	copySession.Role = role
	req, err := e.roleRequest(&copySession, base)
	if err != nil {
		*lane = oldLane
		return false, err
	}
	size, safe, err := contextSize(x, req, s.Profile)
	t, thresholdErr := s.Profile.CompactPolicy().thresholds(role, safe)
	if err != nil || thresholdErr != nil || size >= t.hard {
		*lane = oldLane
		j.State = "failed"
		j.Reason = "still_over_budget"
		s.Maintenance.CompactionsFailed++
		return false, e.save(ctx, s, raw)
	}
	// Archive before the atomic view switch. Prefix digests and epochs allow
	// append-only suffix growth, while rejecting rewrites and duplicate apply.
	archive, _ := json.Marshal(prefix)
	if s.Maintenance.ArchivedBytes+len(archive) > 12*1024*1024 {
		*lane = oldLane
		j.State = "failed"
		j.Reason = "archive_limit"
		s.Maintenance.CompactionsFailed++
		return false, e.save(ctx, s, raw)
	}
	if err = e.Store.Set(ctx, indexKey(s.Scope, "compact_prefix", s.ID+":"+j.ID), archive, s.ExpiresAt.Sub(e.now())+24*time.Hour); err != nil {
		*lane = oldLane
		return false, err
	}
	if role == Lead && e.Cache != nil {
		if err = e.Cache.Stop(ctx, s, "compaction_applied"); err != nil {
			*lane = oldLane
			return false, err
		}
	}
	c.Archives = append(c.Archives, PrefixArchive{JobID: j.ID, Digest: j.PrefixDigest, Count: j.PrefixCount, ContextEpoch: c.Epoch})
	s.Maintenance.ArchivedBytes += len(archive)
	c.Epoch++
	j.State = "applied"
	s.Maintenance.CompactionsApplied++
	if err = e.save(ctx, s, raw); err != nil {
		return false, err
	}
	e.observe(MaintenanceEvent{Purpose: Compaction, Role: role, Stage: "applied", JobID: j.ID, InputBefore: replacement.BeforeBytes, InputAfter: replacement.AfterBytes})
	return true, nil
}

func (e *Engine) prepareCompaction(ctx context.Context, s *Session, raw *[]byte, base *apicompat.ResponsesRequest, x Executor, background bool) (bool, error) {
	role := s.Role
	c := s.roleContext(role)
	p := s.Profile.CompactPolicy()
	admissionCtx := ctx
	if background {
		bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		admissionCtx = bounded
	}
	if c.AttemptTurn != s.Turn {
		c.AttemptTurn = s.Turn
		c.Attempts = 0
	}
	if c.Attempts >= p.MaxAttempts {
		return false, nil
	}
	if c.Job != nil && (c.Job.State == "preparing" || c.Job.State == "ready" || !c.Job.Accounted) {
		return false, nil
	}
	var closeChild func()
	child := x
	if background {
		factory, ok := x.(AuxiliaryFactory)
		if !ok || e.Runner == nil {
			return false, nil
		}
		var acquired bool
		child, closeChild, acquired = factory.Auxiliary(admissionCtx)
		if !acquired {
			return false, nil
		}
	}
	if closeChild == nil {
		closeChild = func() {}
	}
	launched := false
	defer func() {
		if !launched {
			closeChild()
		}
	}()
	// The summary's own request must fit. Shrink only the selected completed
	// prefix, never individual messages or required active handoff constraints.
	cut := safeSummaryPrefix(s, role, s.Profile.MaxContextBytes/2)
	var req *apicompat.ResponsesRequest
	for cut > 0 {
		req = summaryRequest(s, role, base, cut)
		size, safe, err := contextSize(child, req, s.Profile)
		if err == nil && size <= safe {
			break
		}
		cut = safeSummaryPrefix(s, role, cutBytes((*s.Lane(role))[:cut])/2)
	}
	if cut == 0 {
		return false, nil
	}
	if s.Maintenance.ArchivedBytes+cutBytes((*s.Lane(role))[:cut]) > 12*1024*1024 {
		return false, nil
	}
	j := CompactionJob{ID: uuid.NewString(), Role: role, Turn: s.Turn, Epoch: c.Epoch, ProfileRevision: s.ProfileRevision, PrefixCount: cut, PrefixDigest: Digest((*s.Lane(role))[:cut]), State: "preparing", CreatedAt: e.now(), Deadline: e.now().Add(time.Duration(p.TimeoutSeconds) * time.Second)}
	j.Deadline = minTime(j.Deadline, s.TurnStarted.Add(time.Duration(s.Profile.MaxWallSeconds)*time.Second))
	j.Deadline = minTime(j.Deadline, s.ExpiresAt)
	j.Policy = summaryPolicy
	call := Call{AttemptID: j.ID, Purpose: Compaction, Role: role, Request: req, LaneScope: s.LaneScope(role) + ":summary:" + j.ID, PricingAt: e.now()}
	estimate, err := child.Estimate(admissionCtx, call)
	if err != nil {
		if background && admissionCtx.Err() != nil {
			return false, E("compaction_skipped", "background summary admission did not complete in time", 503)
		}
		return false, err
	}
	if !validCost(estimate) || s.TurnRequests >= s.Profile.MaxRequests || s.TurnCostUSD+s.AuxReservedUSD+estimate > s.Profile.MaxCostUSD {
		return false, E("budget_exhausted", "summary would exceed the Fusion turn budget", 409)
	}
	if err = e.reserveBudget(ctx, s, j.ID, Compaction, estimate); err != nil {
		return false, err
	}
	j.EstimateUSD = estimate
	previousJob := c.Job
	c.Job = &j
	c.Attempts++
	s.TurnRequests++
	s.AuxReservedUSD += estimate
	s.Maintenance.CompactionsPrepared++
	r := &compactionRecord{Job: j, Request: req, Attempt: Attempt{ID: j.ID, Purpose: Compaction, Role: role, Turn: s.Turn, Model: req.Model, StartedAt: call.PricingAt, Status: "prepared", EstimateUSD: estimate}}
	abortPreparation := func(cause error) (bool, error) {
		// No executor has been launched. Restore the caller's counters before
		// its error handling can persist them, and fence an acknowledged or
		// ambiguously committed preparation journal as a known zero-cost result.
		c.Job = previousJob
		c.Attempts--
		s.TurnRequests--
		s.AuxReservedUSD -= estimate
		s.Maintenance.CompactionsPrepared--
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		r.Attempt.Status, r.Attempt.Settled = "completed", true
		r.Job.State, r.Job.Reason, r.Request = "failed", "request_not_sent", nil
		_, old, readErr := e.readCompaction(cleanup, s, &j)
		var journalErr error
		if readErr == nil {
			journalErr = e.writeCompaction(cleanup, s, r, old)
		} else if !errors.Is(readErr, devin.ErrSessionNotFound) {
			journalErr = readErr
		}
		return false, errors.Join(cause, journalErr, e.settleBudget(cleanup, s, r.Attempt))
	}
	// Journal first: if session save fails, no network call has started. An
	// orphan prepared record expires without spending money.
	if err = e.writeCompaction(ctx, s, r, nil); err != nil {
		return abortPreparation(err)
	}
	if err = e.save(ctx, s, raw); err != nil {
		return abortPreparation(err)
	}
	e.observe(MaintenanceEvent{Purpose: Compaction, Role: role, Stage: "prepared", JobID: j.ID, InputBefore: cutBytes((*s.Lane(role))[:cut])})
	snapshot := Session{ID: s.ID, Scope: s.Scope, AccountID: s.AccountID, Profile: s.Profile, ProfileRevision: s.ProfileRevision, ExpiresAt: s.ExpiresAt, Contexts: map[Role]*RoleContext{role: {Epoch: c.Epoch}}}
	if background {
		launched = e.Runner.Start(ctx, s.Scope, s.ID, j.ID, max(0, j.Deadline.Sub(e.now())), func(bounded context.Context) { defer closeChild(); e.performCompaction(bounded, &snapshot, r, child) })
		if launched {
			return true, nil
		}
		r.Attempt.Status = "completed"
		r.Attempt.Settled = true
		r.Job.State = "failed"
		r.Job.Reason = "background_capacity"
		old, _ := json.Marshal(&compactionRecord{Job: j, Request: req, Attempt: Attempt{ID: j.ID, Purpose: Compaction, Role: role, Turn: s.Turn, Model: req.Model, StartedAt: call.PricingAt, Status: "prepared", EstimateUSD: estimate}})
		return false, e.writeCompaction(ctx, s, r, old)
	}
	bounded, cancel := context.WithDeadline(ctx, j.Deadline)
	defer cancel()
	e.performCompaction(bounded, &snapshot, r, child)
	return true, nil
}
func cutBytes(items []apicompat.ResponsesInputItem) int { return contextcompact.SizeBytes(items) }

func (e *Engine) awaitCompaction(ctx context.Context, s *Session, j *CompactionJob) error {
	deadline := minTime(j.Deadline, s.TurnStarted.Add(time.Duration(s.Profile.MaxWallSeconds)*time.Second))
	bounded, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		r, _, err := e.readCompaction(bounded, s, j)
		if err != nil {
			return err
		}
		if r.Attempt.Status != "started" && r.Attempt.Status != "prepared" {
			return nil
		}
		select {
		case <-bounded.Done():
			return E("context_limit", "summary was not ready before the hard context deadline", 413)
		case <-ticker.C:
		}
	}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// prepareRoleContext is the only view-switch point, immediately before a normal
// role request. It never mutates public history, tool ownership or handoff IDs.
func (e *Engine) prepareRoleContext(ctx context.Context, s *Session, raw *[]byte, base *apicompat.ResponsesRequest, x Executor) (*apicompat.ResponsesRequest, error) {
	if err := e.collectCompactions(ctx, s, raw, x); err != nil {
		return nil, err
	}
	req, err := e.roleRequest(s, base)
	if err != nil {
		return nil, err
	}
	size, safe, err := contextSize(x, req, s.Profile)
	if err != nil {
		return nil, err
	}
	p := s.Profile.CompactPolicy()
	t, err := p.thresholds(s.Role, safe)
	if err != nil {
		return nil, err
	}
	fixed := *req
	fixed.Input, _ = json.Marshal(protectedPrefix(s, s.Role, *s.Lane(s.Role)))
	fixedSize, _, fixedErr := contextSize(x, &fixed, s.Profile)
	if fixedErr != nil {
		return nil, fixedErr
	}
	if fixedSize >= t.hard {
		return nil, E("context_limit", "required instructions, tools or protected task context exceed the hard input budget", 413)
	}
	if !p.Enabled {
		if size >= t.hard {
			return nil, E("context_limit", "role reached its hard context limit; compaction is disabled", 413)
		}
		return req, nil
	}
	if size >= t.apply {
		applied, applyErr := e.applyCompaction(ctx, s, raw, s.Role, base, x)
		if applyErr != nil {
			return nil, applyErr
		}
		if applied {
			return e.roleRequest(s, base)
		}
	}
	if size >= t.hard {
		e.observe(MaintenanceEvent{Purpose: Compaction, Role: s.Role, Stage: "hard_gate", InputBefore: size})
		c := s.roleContext(s.Role)
		if c.Job != nil && !c.Job.Accounted {
			if err = e.awaitCompaction(ctx, s, c.Job); err != nil {
				return nil, err
			}
			if err = e.collectCompactions(ctx, s, raw, x); err != nil {
				return nil, err
			}
		}
		if applied, applyErr := e.applyCompaction(ctx, s, raw, s.Role, base, x); applyErr != nil {
			return nil, applyErr
		} else if applied {
			return e.roleRequest(s, base)
		}
		if _, err = e.prepareCompaction(ctx, s, raw, base, x, false); err != nil {
			return nil, err
		}
		if err = e.collectCompactions(ctx, s, raw, x); err != nil {
			return nil, err
		}
		if applied, applyErr := e.applyCompaction(ctx, s, raw, s.Role, base, x); applyErr != nil {
			return nil, applyErr
		} else if applied {
			return e.roleRequest(s, base)
		}
		return nil, E("context_limit", "no safe, smaller summary fits this role's hard context budget", 413)
	}
	if size >= t.spawn {
		if _, err = e.prepareCompaction(ctx, s, raw, base, x, true); err != nil {
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != "devin_fusion_budget_exhausted" && typed.Code != "devin_fusion_billing_denied" && typed.Code != "devin_fusion_pricing_unavailable" && typed.Code != "devin_fusion_compaction_skipped" {
				return nil, err
			}
		}
	}
	return req, nil
}
