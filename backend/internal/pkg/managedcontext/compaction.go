package managedcontext

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	cc "github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
	"github.com/google/uuid"
)

func (e *Engine) collect(ctx context.Context, s *Session, sr *Record, l *Lease, x Executor) error {
	if s.JobID == "" {
		return nil
	}
	return e.collectJob(ctx, s, sr, l, s.JobID, x)
}

func (e *Engine) collectJob(ctx context.Context, s *Session, sr *Record, l *Lease, id string, x Executor) error {
	j, jr, err := load[Job](ctx, e.Store, s.Scope, "job", id)
	if err != nil {
		return err
	}
	if s.AppliedJobID == j.ID && j.State != "applied" {
		j.State = "applied"
		j.After = s.AppliedJobAfter
		if err = e.save(ctx, jr, j, j.State, nil, nil, nil); err != nil {
			return err
		}
	}
	if j.AttemptID == "" {
		return nil
	}
	if err = e.ReconcileAttempt(ctx, s.Scope, j.AttemptID, x); err != nil {
		return err
	}
	a, _, err := load[Attempt](ctx, e.Store, s.Scope, "attempt", j.AttemptID)
	if err != nil {
		return err
	}
	if a.State == "dispatching" || a.State == "prepared" {
		if e.now().Before(j.Deadline) {
			return nil
		}
		if a.State == "prepared" {
			return E("compaction_pending", "summary preparation did not finish before its deadline", 503)
		}
	}
	if a.State == "unknown" {
		return E("attempt_uncertain", "summary provider outcome is unknown", 409)
	}
	if !a.Settled {
		return E("billing_pending", "summary settlement is pending", 503)
	}
	// A crashed/late worker may have persisted terminal usage before updating its
	// job. Reconstruct only from that observed terminal result, never by replay.
	if j.State == "running" || j.State == "prepared" || j.State == "uncertain" {
		j.State = "failed"
		if a.Error == nil && a.ResponseRef != "" {
			b, readErr := e.Store.Blob(ctx, s.Scope, a.ResponseRef)
			if readErr != nil {
				return readErr
			}
			var response apicompat.ResponsesResponse
			if json.Unmarshal(b, &response) == nil {
				j.Summary, err = cc.ParseSummary(&response)
				if err == nil {
					j.State = "ready"
				}
			}
		}
		if err = e.save(ctx, jr, j, j.State, nil, nil, nil); err != nil {
			return err
		}
	}
	if !j.Accounted {
		if !s.AccountedJobs[j.ID] {
			s.MaintenanceCostUSD += a.CostUSD
			s.MaintenanceUsage = addUsage(s.MaintenanceUsage, a.Usage)
			s.AccountedJobs = map[string]bool{j.ID: true}
			if err = e.save(ctx, sr, s, s.Status, l, nil, &s.Policy); err != nil {
				return err
			}
		}
		j.Accounted = true
		if err = e.save(ctx, jr, j, j.State, nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}
func protection() cc.Protection {
	return cc.Protection{AllUserMessages: true, SummaryIDPrefix: "summary_context_"}
}
func (e *Engine) apply(ctx context.Context, s *Session, sr *Record, l *Lease, base *apicompat.ResponsesRequest, x Executor) (bool, error) {
	if s.JobID == "" || s.PendingCall != "" {
		return false, nil
	}
	j, jr, err := load[Job](ctx, e.Store, s.Scope, "job", s.JobID)
	if err != nil {
		return false, err
	}
	if j.State != "ready" || !j.Accounted || j.Summary == nil {
		return false, nil
	}
	if s.AppliedJobID == j.ID {
		j.State = "applied"
		j.After = s.AppliedJobAfter
		return true, e.save(ctx, jr, j, j.State, nil, nil, nil)
	}
	items, err := e.messages(ctx, s.Scope, s.Active)
	if err != nil {
		return false, err
	}
	current := cc.Snapshot{Scope: s.Scope, SessionID: s.ID, Lane: cc.Main, Epoch: s.Epoch, Revision: s.Contract, Policy: s.Policy.Version, ModelRevision: s.Model}
	if !j.Snapshot.Matches(current, items) {
		j.State = "stale"
		return false, e.save(ctx, jr, j, j.State, nil, nil, nil)
	}
	message := cc.HistoricalMessage(*j.Summary, j.Snapshot.PrefixDigest, "summary_context_"+j.ID)
	keep := cc.ProtectedItems(items[:j.Snapshot.PrefixCount], protection())
	candidate, err := cc.ReplacePrefix(items, j.Snapshot.PrefixCount, keep, message)
	if err != nil {
		return false, err
	}
	budget, err := x.Measure(requestWith(base, candidate.Items))
	if err != nil {
		return false, err
	}
	target := min(j.Target, budget.SafeInput*60/100-1)
	if !candidate.Smaller() || budget.InputEstimate > target {
		j.State = "failed"
		j.Reason = "target_unreachable"
		return false, e.save(ctx, jr, j, j.State, nil, nil, nil)
	}
	next := []Ref{}
	for _, item := range keep {
		for _, ref := range s.Active[:j.Snapshot.PrefixCount] {
			if ref.ID == item.ID {
				next = append(next, ref)
				break
			}
		}
	}
	ref, blob := e.itemBlob(s, message, false, true)
	next = append(next, ref)
	next = append(next, s.Active[j.Snapshot.PrefixCount:]...)
	s.Active = next
	s.Epoch++
	s.AppliedJobID = j.ID
	s.AppliedJobAfter = budget.InputEstimate
	s.LastBudget = &budget
	s.HistoryBytes += int64(ref.Bytes)
	// Logical summary output consumes its share before foreground publication.
	s.ReservedBytes = max(s.ReservedBytes-int64(ref.Bytes), 0)
	if j.Trigger == "manual" && s.ReservationOperationID == j.OperationID {
		// Applying a manual summary and releasing its reservation are one
		// session commit, so interruption cannot leave quota permanently held.
		s.ReservedBytes = 0
		s.ReservationOperationID = ""
	}
	if err = e.ensureLive(ctx, s); err != nil {
		return false, err
	}
	if err = e.save(ctx, sr, s, s.Status, l, []Blob{blob}, &s.Policy); err != nil {
		return false, err
	}
	j.State = "applied"
	j.After = budget.InputEstimate
	if err = e.save(ctx, jr, j, j.State, nil, nil, nil); err != nil {
		return true, err
	}
	e.emit("applied", j.Trigger)
	return true, nil
}
func (e *Engine) startSummary(ctx context.Context, s *Session, sr *Record, l *Lease, op *Operation, or *Record, ol *Lease, base *apicompat.ResponsesRequest, x Executor, background bool, trigger string, target int) (started bool, resultErr error) {
	if s.JobID != "" {
		j, _, err := load[Job](ctx, e.Store, s.Scope, "job", s.JobID)
		if err != nil {
			return false, err
		}
		if !j.Accounted || j.State == "ready" {
			return false, nil
		}
	}
	count := 0
	for _, id := range op.Attempts {
		a, _, err := load[Attempt](ctx, e.Store, op.Scope, "attempt", id)
		if err != nil {
			return false, err
		}
		if a.Purpose == "compaction" {
			count++
		}
	}
	if count >= s.Policy.SummaryAttempts {
		return false, nil
	}
	items, err := e.messages(ctx, s.Scope, s.Active)
	if err != nil {
		return false, err
	}
	measured, err := x.Measure(requestWith(base, items))
	if err != nil {
		return false, err
	}
	fixed, err := x.Measure(requestWith(base, cc.ProtectedItems(items, protection())))
	if err != nil {
		return false, err
	}
	if fixed.InputEstimate >= target {
		return false, E("target_unreachable", "protected instructions or task context exceed the compaction target", 413)
	}
	cut := cc.PlanPrefix(items, cc.PrefixPolicy{RetainItems: 1, MinItems: 2, MaxBytes: measured.SafeInput})
	// Keep an opaque reasoning frame with the following live execution group.
	for cut > 0 && items[cut-1].Type == "reasoning" {
		cut--
	}
	var req *apicompat.ResponsesRequest
	for cut > 0 {
		req = cc.BuildSummaryRequest(items[:cut], s.Model, summaryLimit(s.Policy, x))
		budget, measureErr := x.Measure(req)
		if measureErr == nil && budget.InputEstimate < budget.SafeInput {
			break
		}
		cut = cc.PlanPrefix(items, cc.PrefixPolicy{RetainItems: 1, MinItems: 2, MaxBytes: cc.SizeBytes(items[:cut]) / 2})
	}
	if cut < 2 {
		return false, nil
	}
	minimum := append(cc.ProtectedItems(items[:cut], protection()), items[cut:]...)
	tailBudget, err := x.Measure(requestWith(base, minimum))
	if err != nil {
		return false, err
	}
	if tailBudget.InputEstimate >= target {
		return false, E("target_unreachable", "protected context and the current tool group exceed the target", 413)
	}
	child := x
	release := func() {}
	if background {
		select {
		case e.slots <- struct{}{}:
		default:
			return false, nil
		}
		var acquired bool
		child, release, acquired = x.Auxiliary(ctx)
		if !acquired {
			<-e.slots
			return false, nil
		}
	}
	launched := false
	defer func() {
		if !launched {
			release()
			if background {
				<-e.slots
			}
		}
	}()
	// Admission is checked before creating a paid attempt, including session-wide
	// maintenance spend. The same estimate is rechecked by prepareCall per turn.
	estimate, err := child.Estimate(ctx, Call{Request: req, Model: req.Model, Purpose: "compaction", StartedAt: e.now()})
	if err != nil {
		return false, err
	}
	if !validCost(estimate) || s.MaintenanceCostUSD+estimate > s.Policy.MaxMaintenanceCostUSD {
		return false, E("budget_exhausted", "session maintenance cost budget reached", 402)
	}
	j := &Job{Model: s.Model, Owner: s.Owner, ID: "cmp_" + uuid.NewString(), OperationID: op.ID, SessionID: s.ID, Trigger: trigger, State: "prepared", Target: target, Before: measured.InputEstimate, Deadline: e.now().Add(time.Duration(s.Policy.SummarySeconds) * time.Second), Snapshot: cc.Snapshot{Scope: s.Scope, SessionID: s.ID, Lane: cc.Main, Epoch: s.Epoch, Revision: s.Contract, Policy: s.Policy.Version, ModelRevision: s.Model, PrefixCount: cut, PrefixDigest: cc.Digest(items[:cut])}}
	if d, ok := ctx.Deadline(); ok && d.Before(j.Deadline) {
		j.Deadline = d
	}
	if s.ExpiresAt.Before(j.Deadline) {
		j.Deadline = s.ExpiresAt
	}
	parent := ctx
	if background {
		parent = context.WithoutCancel(ctx)
	}
	bounded, stop := context.WithDeadline(parent, j.Deadline)
	untrack := func() {}
	if background {
		bounded, untrack, err = e.track(bounded)
		if err != nil {
			stop()
			return false, err
		}
	}
	startedWork := false
	defer func() {
		if !startedWork {
			stop()
			untrack()
		}
	}()
	// An explicit internal compact has no foreground output reservation yet.
	allocatedReservation := false
	defer func() {
		if !allocatedReservation || launched {
			return
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		// Reload the committed view: preparation may have changed only its
		// local job pointer before failing. Roll back just our own promise.
		current, cr, cleanupErr := e.LoadSession(persist, s.Scope, s.ID)
		if cleanupErr == nil && current.ReservationOperationID == op.ID {
			current.ReservedBytes, current.ReservationOperationID = 0, ""
			cleanupErr = e.save(persist, cr, current, current.Status, l, nil, &current.Policy)
			if cleanupErr == nil {
				*s, *sr = *current, *cr
			}
		}
		if cleanupErr != nil {
			resultErr = E("unavailable", "summary preparation reservation could not be rolled back", 503)
		}
	}()
	if s.ReservedBytes == 0 {
		s.ReservedBytes = int64((s.Policy.SummaryOutputTokens*(s.Policy.SummaryAttempts*2+1))*64 + 65536)
		s.ReservationOperationID = op.ID
		allocatedReservation = true
		if err = e.save(ctx, sr, s, s.Status, l, nil, &s.Policy); err != nil {
			return false, err
		}
	}
	a, ar, call, err := e.prepareCall(ctx, op, or, ol, req, "compaction", child)
	if err != nil {
		stop()
		return false, err
	}
	a.Deadline = j.Deadline
	if background {
		// This immutable allocation partitions the turn's existing promise;
		// its physical result remains funded after the foreground completes.
		j.AllocatedBytes = int64(summaryLimit(s.Policy, child) * 2 * 64)
		j.ReservedBytes = j.AllocatedBytes
		a.ReservationID = j.ID
	}
	if err = e.save(ctx, ar, a, a.State, nil, nil, nil); err != nil {
		stop()
		return false, err
	}
	j.AttemptID = a.ID
	jr := record(j.Owner, "job", j.ID, j.State, j, s.ExpiresAt.Add(24*time.Hour))
	if _, err = e.Store.Create(ctx, jr, nil); err != nil {
		stop()
		return false, err
	}
	s.JobID = j.ID
	if err = e.save(ctx, sr, s, s.Status, l, nil, &s.Policy); err != nil {
		stop()
		return false, err
	}
	work := func() {
		defer untrack()
		defer stop()
		defer release()
		if background {
			defer func() { <-e.slots }()
		}
		bounded, unwatch := e.watchCancel(bounded, j.Scope, j.SessionID)
		defer unwatch()
		if e.ensureLive(bounded, &Session{Owner: j.Owner, ID: j.SessionID, ExpiresAt: j.Deadline}) != nil {
			return
		}
		j.State = "running"
		if e.save(bounded, jr, j, j.State, nil, nil, nil) != nil {
			return
		}
		_, response, callErr := e.executeCall(bounded, a, ar, call, child)
		j.State = "failed"
		if callErr == nil {
			j.Summary, callErr = cc.ParseSummary(response)
			if callErr == nil {
				j.State = "ready"
			}
		}
		if a.State == "unknown" {
			j.State = "uncertain"
		}
		if callErr != nil {
			j.Reason = typed(callErr).Code
		}
		j.ReservedBytes = 0
		persist, cancel := context.WithTimeout(context.WithoutCancel(bounded), 10*time.Second)
		defer cancel()
		_ = e.save(persist, jr, j, j.State, nil, nil, nil)
		e.emit(j.State, j.Trigger)
	}
	e.emit("summary_started", trigger)
	launched = true
	startedWork = true
	if background {
		go work()
	} else {
		work()
	}
	return true, nil
}
func (e *Engine) prepare(ctx context.Context, s *Session, sr *Record, l *Lease, op *Operation, or *Record, ol *Lease, base *apicompat.ResponsesRequest, x Executor) (*apicompat.ResponsesRequest, error) {
	if err := e.collect(ctx, s, sr, l, x); err != nil {
		return nil, err
	}
	items, err := e.messages(ctx, s.Scope, s.Active)
	if err != nil {
		return nil, err
	}
	req := requestWith(base, items)
	budget, err := x.Measure(req)
	if err != nil {
		return nil, err
	}
	gates, err := cc.ResolveThresholds(nil, budget.SafeInput)
	if err != nil {
		return nil, err
	}
	fixed, err := x.Measure(requestWith(base, cc.ProtectedItems(items, protection())))
	if err != nil {
		return nil, err
	}
	if fixed.InputEstimate >= gates.Hard {
		return nil, E("context_limit", "protected instructions or tools exceed the hard context budget", 413)
	}
	auto := s.Policy.Auto
	// Providers may resolve current account settings without mutating the shared
	// engine or a persisted policy. This also lets an existing session opt in.
	if current, ok := x.(interface {
		AutoCompactionEnabled(context.Context) (bool, error)
	}); ok {
		auto, err = current.AutoCompactionEnabled(ctx)
		if err != nil {
			return nil, err
		}
	}
	if !auto || e.DisableAuto {
		if budget.InputEstimate >= gates.Hard {
			return nil, E("context_limit", "hard input budget reached while auto compaction is disabled", 413)
		}
		return req, nil
	}
	if budget.InputEstimate >= gates.Apply {
		if applied, err := e.apply(ctx, s, sr, l, base, x); err != nil {
			return nil, err
		} else if applied {
			items, err = e.messages(ctx, s.Scope, s.Active)
			return requestWith(base, items), err
		}
	}
	if budget.InputEstimate >= gates.Hard {
		if s.JobID != "" {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				j, _, err := load[Job](ctx, e.Store, s.Scope, "job", s.JobID)
				if err != nil {
					return nil, err
				}
				if j.State != "prepared" && j.State != "running" {
					break
				}
				if !e.now().Before(j.Deadline) {
					break
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-ticker.C:
				}
			}
			if err = e.collect(ctx, s, sr, l, x); err != nil {
				return nil, err
			}
			if applied, err := e.apply(ctx, s, sr, l, base, x); err != nil {
				return nil, err
			} else if applied {
				items, err = e.messages(ctx, s.Scope, s.Active)
				return requestWith(base, items), err
			}
		}
		for n := 0; n < s.Policy.SummaryAttempts; n++ {
			started, err := e.startSummary(ctx, s, sr, l, op, or, ol, base, x, false, "hard", budget.SafeInput*60/100-1)
			if err != nil {
				return nil, err
			}
			if !started {
				break
			}
			if err = e.collect(ctx, s, sr, l, x); err != nil {
				return nil, err
			}
			if applied, err := e.apply(ctx, s, sr, l, base, x); err != nil {
				return nil, err
			} else if applied {
				items, err = e.messages(ctx, s.Scope, s.Active)
				return requestWith(base, items), err
			}
		}
		return nil, E("context_limit", "no safe summary can fit this context", 413)
	}
	if budget.InputEstimate >= gates.Spawn {
		_, err = e.startSummary(ctx, s, sr, l, op, or, ol, base, x, true, "auto", budget.SafeInput*60/100-1)
		if err != nil {
			if v := typed(err); v.Code != "context_budget_exhausted" && v.Code != "context_target_unreachable" && v.Status != 402 {
				return nil, err
			}
		}
	}
	return req, nil
}

// LoadJob derives the visible job phase from durable terminal evidence even if
// its worker stopped after saving usage but before publishing the job result.
func (e *Engine) LoadJob(ctx context.Context, scope, id string) (*Job, *Record, error) {
	j, jr, err := load[Job](ctx, e.Store, scope, "job", id)
	if err != nil {
		return nil, nil, err
	}
	if j.SessionID != "" && j.State != "applied" {
		s, _, readErr := e.LoadSession(ctx, scope, j.SessionID)
		if readErr == nil && s.AppliedJobID == j.ID {
			j.State = "applied"
			j.After = s.AppliedJobAfter
			if err = e.save(ctx, jr, j, j.State, nil, nil, nil); err != nil {
				return nil, nil, err
			}
			return j, jr, nil
		}
	}
	if j.AttemptID == "" || (j.State != "prepared" && j.State != "running" && j.State != "uncertain") {
		return j, jr, nil
	}
	a, _, err := load[Attempt](ctx, e.Store, scope, "attempt", j.AttemptID)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case a.State == "unknown":
		j.State = "uncertain"
		j.Reason = "context_attempt_uncertain"
	case a.State == "prepared" && !e.now().Before(a.Deadline):
		j.State = "failed"
		j.Reason = "context_not_dispatched"
	case a.State == "terminal" && a.Settled:
		j.State = "failed"
		if a.Error != nil {
			j.Reason = a.Error.Code
		} else if a.ResponseRef != "" {
			b, readErr := e.Store.Blob(ctx, scope, a.ResponseRef)
			if readErr != nil {
				return nil, nil, readErr
			}
			var response apicompat.ResponsesResponse
			if json.Unmarshal(b, &response) == nil {
				j.Summary, err = cc.ParseSummary(&response)
				if err == nil {
					j.State = "ready"
				} else {
					j.Reason = "context_summary_invalid"
				}
			}
		}
	default:
		return j, jr, nil
	}
	if err = e.save(ctx, jr, j, j.State, nil, nil, nil); err != nil {
		return nil, nil, err
	}
	return j, jr, nil
}
