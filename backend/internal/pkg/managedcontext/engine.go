package managedcontext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	cc "github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
	"github.com/google/uuid"
)

type Engine struct {
	DisableAuto bool

	Store    Store
	Now      func() time.Time
	slots    chan struct{}
	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
	Observer func(string, string)
	events   map[string]uint64
	root     context.Context
	shutdown context.CancelFunc
	wg       sync.WaitGroup
	closed   bool
}

func New(store Store) *Engine {
	root, stop := context.WithCancel(context.Background())
	return &Engine{Store: store, Now: time.Now, slots: make(chan struct{}, 16), cancels: map[string]context.CancelFunc{}, root: root, shutdown: stop, events: map[string]uint64{}}
}

// track prevents shutdown from closing storage while detached work is draining.
func (e *Engine) track(ctx context.Context) (context.Context, func(), error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, nil, E("unavailable", "context runner is stopping", 503)
	}
	e.wg.Add(1)
	e.mu.Unlock()
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.root, cancel)
	return child, func() { stop(); cancel(); e.wg.Done() }, nil
}
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	e.closed = true
	e.shutdown()
	e.mu.Unlock()
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *Engine) emit(event, trigger string) {
	e.mu.Lock()
	e.events[event]++
	e.mu.Unlock()
	if e.Observer != nil {
		e.Observer(event, trigger)
	}
}
func (e *Engine) Metrics() map[string]uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]uint64, len(e.events)+1)
	for k, v := range e.events {
		out[k] = v
	}
	out["background_active"] = uint64(len(e.slots))
	return out
}
func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
func digestBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validCost(n float64) bool    { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 }
func outputLimit(p Policy, x Executor) int {
	n := p.MaxOutputTokens
	if limits, ok := x.(interface{ OutputLimit() int }); ok && limits.OutputLimit() > 0 {
		n = min(n, limits.OutputLimit())
	}
	return n
}
func summaryLimit(p Policy, x Executor) int {
	n := p.SummaryOutputTokens
	if limits, ok := x.(interface{ OutputLimit() int }); ok && limits.OutputLimit() > 0 {
		n = min(n, limits.OutputLimit())
	}
	return n
}
func typed(err error) *cc.Error {
	var v *cc.Error
	if errors.As(err, &v) {
		if !strings.HasPrefix(v.Code, "context_") {
			return E(v.Code, v.Message, v.Status)
		}
		return v
	}
	return E("unavailable", "context operation could not complete", 503)
}
func record(owner Owner, kind, id, status string, v any, expiry time.Time) *Record {
	b, _ := json.Marshal(v)
	return &Record{Owner: owner, Kind: kind, ID: id, Data: b, Status: status, ExpiresAt: expiry}
}
func decode[T any](r *Record) (*T, error) {
	var v T
	if json.Unmarshal(r.Data, &v) != nil {
		return nil, E("corrupt", "stored context record failed validation", 503)
	}
	return &v, nil
}
func load[T any](ctx context.Context, s Store, scope, kind, id string) (*T, *Record, error) {
	r, err := s.Read(ctx, scope, kind, id)
	if err != nil {
		return nil, nil, err
	}
	v, err := decode[T](r)
	return v, r, err
}
func (e *Engine) save(ctx context.Context, r *Record, v any, status string, l *Lease, blobs []Blob, p *Policy) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.Data = b
	r.Status = status
	if err = e.Store.CAS(ctx, r, l, blobs, p); err != nil {
		return err
	}
	// The SQL store sets authoritative retained-byte totals in this commit.
	if r.Kind == "session" {
		return json.Unmarshal(r.Data, v)
	}
	return nil
}
func (e *Engine) LoadSession(ctx context.Context, scope, id string) (*Session, *Record, error) {
	s, r, err := load[Session](ctx, e.Store, scope, "session", id)
	if err != nil {
		return nil, nil, err
	}
	if s.Scope != scope || s.ID != id || s.Schema != 2 || s.Policy.Validate() != nil {
		return nil, nil, E("corrupt", "stored context session failed validation", 503)
	}
	if r.Cancelled {
		s.Status = "cancelled"
	}
	return s, r, nil
}
func (e *Engine) lease(ctx context.Context, scope, kind, id string) (context.Context, Lease, func(), error) {
	l, err := e.Store.Acquire(ctx, scope, kind, id, uuid.NewString(), 60*time.Second)
	if err != nil {
		return ctx, l, nil, err
	}
	owned, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-owned.Done():
				return
			case <-ticker.C:
				if e.Store.Renew(owned, l, 60*time.Second) != nil {
					cancel()
					return
				}
			}
		}
	}()
	release := func() {
		cancel()
		<-done
		c, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_ = e.Store.Release(c, l)
	}
	return owned, l, release, nil
}
func (e *Engine) Cancel(ctx context.Context, scope, id string) error {
	if err := e.Store.Cancel(ctx, scope, id); err != nil {
		return err
	}
	e.mu.Lock()
	prefix := scope + ":" + id + ":"
	for key, cancel := range e.cancels {
		if strings.HasPrefix(key, prefix) {
			cancel()
		}
	}
	e.mu.Unlock()
	return nil
}
func (e *Engine) watchCancel(ctx context.Context, scope, id string) (context.Context, func()) {
	child, cancel := context.WithCancel(ctx)
	key := scope + ":" + id + ":" + uuid.NewString()
	e.mu.Lock()
	e.cancels[key] = cancel
	e.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-child.Done():
				return
			case <-t.C:
				r, err := e.Store.Read(child, scope, "session", id)
				if err != nil || r.Cancelled {
					cancel()
					return
				}
			}
		}
	}()
	return child, func() { cancel(); <-done; e.mu.Lock(); delete(e.cancels, key); e.mu.Unlock() }
}
func (e *Engine) ensureLive(ctx context.Context, s *Session) error {
	r, err := e.Store.Read(ctx, s.Scope, "session", s.ID)
	if err != nil {
		return err
	}
	if r.Cancelled {
		return E("cancelled", "managed session was cancelled", 409)
	}
	if !e.now().Before(s.ExpiresAt) {
		return E("expired", "managed session expired", 410)
	}
	return ctx.Err()
}
func (e *Engine) messages(ctx context.Context, scope string, refs []Ref) ([]cc.Item, error) {
	out := make([]cc.Item, 0, len(refs))
	for _, ref := range refs {
		b, err := e.Store.Blob(ctx, scope, ref.ID)
		if err != nil {
			return nil, err
		}
		if len(b) != ref.Bytes || digestBytes(b) != ref.Digest {
			return nil, E("corrupt", "active history reference failed validation", 503)
		}
		var item cc.Item
		if json.Unmarshal(b, &item) != nil {
			return nil, E("corrupt", "invalid active history item", 503)
		}
		out = append(out, item)
	}
	return out, nil
}
func (e *Engine) itemBlob(s *Session, item cc.Item, public, summary bool) (Ref, Blob) {
	if item.ID == "" {
		item.ID = "msg_ctx_" + uuid.NewString()
	}
	b, _ := json.Marshal(item)
	v := "private"
	if public {
		v = "public"
	}
	ref := Ref{ID: item.ID, Digest: digestBytes(b), Bytes: len(b), Summary: summary}
	return ref, Blob{ID: ref.ID, SessionID: s.ID, Visibility: v, Kind: "message", Digest: ref.Digest, Data: b, ExpiresAt: s.ExpiresAt.Add(24 * time.Hour)}
}
func requestWith(template *apicompat.ResponsesRequest, items []cc.Item) *apicompat.ResponsesRequest {
	r := *template
	r.Input, _ = json.Marshal(items)
	r.PreviousResponseID = ""
	r.Store = nil
	r.Stream = false
	return &r
}
func addUsage(a, b *apicompat.ResponsesUsage) *apicompat.ResponsesUsage {
	if b == nil {
		return a
	}
	if a == nil {
		a = &apicompat.ResponsesUsage{}
	}
	next := *a
	next.InputTokens += b.InputTokens
	next.OutputTokens += b.OutputTokens
	next.TotalTokens += b.TotalTokens
	next.CacheCreationInputTokens += b.CacheCreationInputTokens
	cached := 0
	if a.InputTokensDetails != nil {
		cached += a.InputTokensDetails.CachedTokens
	}
	if b.InputTokensDetails != nil {
		cached += b.InputTokensDetails.CachedTokens
	}
	next.InputTokensDetails = &apicompat.ResponsesInputTokensDetails{CachedTokens: cached}
	return &next
}

// Begin durably binds a logical idempotency key before account selection. An
// operation lease prevents two creators from selecting two different accounts.
func (e *Engine) Begin(ctx context.Context, owner Owner, namespace, key, fingerprint, model, session string, p Policy) (*Operation, *Record, error) {
	if e.Store == nil {
		return nil, nil, E("unavailable", "persistent context store is unavailable", 503)
	}
	if key == "" || len(key) > 128 || strings.ContainsAny(key, " \t\r\n") {
		return nil, nil, E("input_invalid", "Idempotency-Key must contain 1..128 non-space characters", 400)
	}
	if len(model) > 256 || len(session) > 128 || len(namespace) > 256 {
		return nil, nil, E("input_invalid", "managed model and session identifiers exceed their metadata limits", 400)
	}
	if err := p.Validate(); err != nil {
		return nil, nil, err
	}
	id := "op_" + cc.Digest([]string{namespace, key})
	op := &Operation{Owner: owner, ID: id, Namespace: namespace, Fingerprint: fingerprint, SessionID: session, Model: model, Policy: p, State: "prepared", CreatedAt: e.now()}
	r := record(owner, "operation", id, op.State, op, e.now().Add(48*time.Hour))
	_, err := e.Store.Create(ctx, r, &p)
	if err != nil {
		return nil, nil, err
	}
	op, r, err = load[Operation](ctx, e.Store, owner.Scope, "operation", id)
	if err != nil {
		return nil, nil, err
	}
	if op.Fingerprint != fingerprint || op.Namespace != namespace {
		return nil, nil, E("idempotency_conflict", "idempotency key was used for different input", 409)
	}
	return op, r, nil
}
func (e *Engine) Bind(ctx context.Context, op *Operation, r *Record, l *Lease, owner Owner, session string) error {
	if op.AccountID != 0 && op.AccountID != owner.AccountID {
		return E("account_bound", "operation is bound to another account", 409)
	}
	op.Owner = owner
	op.SessionID = session
	r.Owner = owner
	return e.save(ctx, r, op, op.State, l, nil, nil)
}
func (e *Engine) Response(ctx context.Context, op *Operation) (*apicompat.ResponsesResponse, error) {
	if op.State == "failed" || op.State == "uncertain" {
		if op.Error != nil {
			return nil, op.Error
		}
		return nil, E("attempt_uncertain", "provider result is unknown", 409)
	}
	if op.ResponseRef == "" {
		return nil, nil
	}
	b, err := e.Store.Blob(ctx, op.Scope, op.ResponseRef)
	if err != nil {
		return nil, err
	}
	var response apicompat.ResponsesResponse
	if json.Unmarshal(b, &response) != nil {
		return nil, E("corrupt", "stored response failed validation", 503)
	}
	return &response, nil
}
func (e *Engine) settle(ctx context.Context, a *Attempt, r *Record, x Executor) error {
	if a.State != "terminal" || a.Settled {
		return nil
	}
	if x == nil {
		return E("billing_pending", "settlement executor is unavailable", 503)
	}
	cost, err := x.Settle(ctx, *a)
	if err != nil {
		e.emit("billing_pending", a.Purpose)
		return E("billing_pending", "observed context usage is awaiting settlement", 503)
	}
	if !validCost(cost) {
		return E("billing_pending", "invalid settlement result", 503)
	}
	a.Settled = true
	a.CostUSD = cost
	if err = e.save(ctx, r, a, a.State, nil, nil, nil); err != nil {
		latest, _, readErr := load[Attempt](ctx, e.Store, a.Scope, "attempt", a.ID)
		if readErr == nil && latest.Settled {
			return nil
		}
		return E("billing_pending", "usage was billed but its receipt is awaiting reconciliation", 503)
	}
	return nil
}
func (e *Engine) ReconcileAttempt(ctx context.Context, scope, id string, x Executor) error {
	a, r, err := load[Attempt](ctx, e.Store, scope, "attempt", id)
	if err != nil {
		return err
	}
	if a.State == "prepared" && !e.now().Before(a.Deadline) {
		a.State = "terminal"
		a.Settled = true
		a.Error = E("not_dispatched", "prepared call expired before dispatch", 503)
		if err = e.save(ctx, r, a, a.State, nil, nil, nil); err != nil {
			return err
		}
	}
	if a.State == "dispatching" && !e.now().Before(a.Deadline) {
		a.State = "unknown"
		if err = e.save(ctx, r, a, a.State, nil, nil, nil); err != nil {
			return err
		}
	}
	return e.settle(ctx, a, r, x)
}
func (e *Engine) recoverOperation(ctx context.Context, op *Operation, r *Record, l *Lease, x Executor) error {
	for _, id := range op.Attempts {
		if err := e.ReconcileAttempt(ctx, op.Scope, id, x); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) prepareCall(ctx context.Context, op *Operation, opRecord *Record, opLease *Lease, req *apicompat.ResponsesRequest, purpose string, x Executor) (*Attempt, *Record, Call, error) {
	if len(op.Attempts) >= op.Policy.MaxTurnRequests {
		return nil, nil, Call{}, E("budget_exhausted", "physical call budget reached", 409)
	}
	call := Call{ID: "att_" + uuid.NewString(), SessionID: op.SessionID, Model: req.Model, Purpose: purpose, LaneScope: "context:" + op.Scope + ":" + op.SessionID + ":main", Request: req, StartedAt: e.now()}
	if op.SessionID == "" {
		call.LaneScope = "context:" + op.Scope + ":" + op.ID
	}
	if strings.HasPrefix(purpose, "compaction") {
		call.LaneScope += ":summary:" + call.ID
	}
	estimate, err := x.Estimate(ctx, call)
	if err != nil {
		return nil, nil, Call{}, err
	}
	if !validCost(estimate) {
		return nil, nil, Call{}, E("budget_exhausted", "invalid physical call estimate", 409)
	}
	total := estimate
	for _, id := range op.Attempts {
		a, _, err := load[Attempt](ctx, e.Store, op.Scope, "attempt", id)
		if err != nil {
			return nil, nil, Call{}, err
		}
		if a.Settled {
			total += a.CostUSD
		} else {
			total += a.EstimateUSD
		}
	}
	if total > op.Policy.MaxTurnCostUSD {
		return nil, nil, Call{}, E("budget_exhausted", "context turn cost budget reached", 402)
	}
	a := &Attempt{Owner: op.Owner, ID: call.ID, OperationID: op.ID, SessionID: op.SessionID, Model: req.Model, Purpose: purpose, State: "prepared", EstimateUSD: estimate, StartedAt: call.StartedAt, Deadline: e.now().Add(time.Duration(op.Policy.WallSeconds) * time.Second), Policy: op.Policy}
	if d, ok := ctx.Deadline(); ok && d.Before(a.Deadline) {
		a.Deadline = d
	}
	call.EstimateUSD = estimate
	expiry := opRecord.ExpiresAt
	if retained := e.now().Add(time.Duration(op.Policy.TTLSeconds)*time.Second + 24*time.Hour); retained.After(expiry) {
		expiry = retained
	}
	ar := record(a.Owner, "attempt", a.ID, a.State, a, expiry)
	if _, err = e.Store.Create(ctx, ar, &op.Policy); err != nil {
		return nil, nil, Call{}, err
	}
	op.Attempts = append(op.Attempts, a.ID)
	if err = e.save(ctx, opRecord, op, op.State, opLease, nil, nil); err != nil {
		return nil, nil, Call{}, err
	}
	return a, ar, call, nil
}
func (e *Engine) executeCall(ctx context.Context, a *Attempt, ar *Record, call Call, x Executor) (*Attempt, *apicompat.ResponsesResponse, error) {
	var err error
	a.State = "dispatching"
	if err = e.save(ctx, ar, a, a.State, nil, nil, nil); err != nil {
		return nil, nil, err
	}
	outcome, executeErr := x.Execute(ctx, call)
	a.BillingModel = outcome.BillingModel
	if a.BillingModel == "" {
		a.BillingModel = a.Model
	}
	a.State = "unknown"
	var blobs []Blob
	if outcome.Response != nil {
		a.Usage = outcome.Response.Usage
		a.ResponseID = outcome.Response.ID
		a.ResponseModel = outcome.Response.Model
		a.ResponseStatus = outcome.Response.Status
		if a.Usage != nil {
			a.State = "terminal"
		}
		b, _ := json.Marshal(outcome.Response)
		a.ResponseRef = "result_" + a.ID
		blobs = []Blob{{ID: a.ResponseRef, SessionID: a.SessionID, Visibility: "private", Kind: "provider_result", Data: b, ExpiresAt: ar.ExpiresAt}}
	}
	if outcome.KnownFailure && a.Usage == nil {
		a.State = "terminal"
		a.Settled = true
	}
	if executeErr != nil {
		a.Error = typed(executeErr)
	}
	persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer stop()
	if err = e.saveObservedAttempt(persist, a, ar, blobs); err != nil {
		// Preserve the terminal usage even if the provider returned an oversized body.
		a.ResponseRef = ""
		a.Error = E("result_unavailable", "provider result could not be persisted", 503)
		if fallback := e.saveObservedAttempt(persist, a, ar, nil); fallback != nil {
			return a, nil, fallback
		}
	}
	if a.State == "unknown" {
		e.emit("attempt_unknown", a.Purpose)
		return a, nil, E("attempt_uncertain", "provider outcome is unknown; this attempt will not be replayed", 409)
	}
	e.emit("attempt_terminal", a.Purpose)
	if err = e.settle(persist, a, ar, x); err != nil {
		return a, nil, err
	}
	if a.Error != nil {
		return a, nil, a.Error
	}
	if outcome.Response == nil || outcome.Response.Usage == nil {
		return a, nil, E("provider_failed", "provider returned no terminal response", 502)
	}
	return a, outcome.Response, nil
}

// Recovery may mark a timed-out dispatch unknown while its original worker is
// still draining a late terminal response. Refresh its journal revision without
// weakening the session fence; observed terminal usage must survive that race.
func (e *Engine) saveObservedAttempt(ctx context.Context, a *Attempt, r *Record, blobs []Blob) error {
	for tries := 0; tries < 3; tries++ {
		p := &a.Policy
		if len(blobs) == 0 {
			p = nil
		}
		err := e.save(ctx, r, a, a.State, nil, blobs, p)
		if err == nil {
			return nil
		}
		latest, lr, readErr := load[Attempt](ctx, e.Store, a.Scope, "attempt", a.ID)
		if readErr != nil || lr.Revision == r.Revision {
			return err
		}
		*r = *lr
		if latest.State == "terminal" && latest.Usage != nil {
			*a = *latest
			return nil
		}
	}
	return E("unavailable", "physical outcome journal changed during recovery", 503)
}
func (e *Engine) call(ctx context.Context, op *Operation, opRecord *Record, opLease *Lease, req *apicompat.ResponsesRequest, purpose string, x Executor) (*Attempt, *apicompat.ResponsesResponse, error) {
	a, ar, call, err := e.prepareCall(ctx, op, opRecord, opLease, req, purpose, x)
	if err != nil {
		return nil, nil, err
	}
	return e.executeCall(ctx, a, ar, call, x)
}

// ReplayableAttempt retrieves a terminal result without issuing a provider call.
func (e *Engine) replayableAttempt(ctx context.Context, op *Operation, purpose string, x Executor) (*Attempt, *apicompat.ResponsesResponse, error) {
	for i := len(op.Attempts) - 1; i >= 0; i-- {
		a, _, err := load[Attempt](ctx, e.Store, op.Scope, "attempt", op.Attempts[i])
		if err != nil {
			return nil, nil, err
		}
		if a.Purpose != purpose {
			continue
		}
		if err = e.ReconcileAttempt(ctx, op.Scope, a.ID, x); err != nil {
			return nil, nil, err
		}
		a, r, err := load[Attempt](ctx, e.Store, op.Scope, "attempt", a.ID)
		if err != nil {
			return nil, nil, err
		}
		if a.State == "prepared" {
			a.State = "terminal"
			a.Settled = true
			a.Error = E("not_dispatched", "prepared call was interrupted before dispatch", 503)
			if err = e.save(ctx, r, a, a.State, nil, nil, nil); err != nil {
				return nil, nil, err
			}
			return a, nil, a.Error
		}
		if a.State != "terminal" {
			return a, nil, E("attempt_uncertain", "existing provider attempt has no recoverable terminal result", 409)
		}
		if a.Error != nil {
			return a, nil, a.Error
		}
		if a.ResponseRef == "" {
			return a, nil, E("result_unavailable", "physical result is unavailable", 503)
		}
		b, err := e.Store.Blob(ctx, op.Scope, a.ResponseRef)
		if err != nil {
			return nil, nil, err
		}
		var response apicompat.ResponsesResponse
		if json.Unmarshal(b, &response) != nil {
			return nil, nil, E("corrupt", "physical result is corrupt", 503)
		}
		return a, &response, nil
	}
	return nil, nil, nil
}

// ReconcileKnown recovers only observed terminal outcomes. It never dispatches.
func (e *Engine) ReconcileKnown(ctx context.Context, op *Operation, r *Record, l *Lease, x Executor) error {
	if x == nil {
		return E("unavailable", "settlement executor is unavailable", 503)
	}
	for _, id := range op.Attempts {
		if err := e.ReconcileAttempt(ctx, op.Scope, id, x); err != nil {
			return err
		}
		a, _, err := load[Attempt](ctx, e.Store, op.Scope, "attempt", id)
		if err != nil {
			return err
		}
		if a.State == "unknown" {
			return E("attempt_uncertain", "provider outcome remains unknown", 409)
		}
		if a.State == "dispatching" {
			return E("busy", "the original physical attempt is still in progress", 409)
		}
	}
	if op.State == "uncertain" {
		op.State = "running"
		op.Error = nil
		return e.save(ctx, r, op, op.State, l, nil, nil)
	}
	return nil
}
