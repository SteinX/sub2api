package fusion

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
)

type Engine struct {
	Store         AtomicStore
	Now           func() time.Time
	Runner        *MaintenanceRunner
	Cache         *CacheKeepaliveController
	RouteIdentity string
	CacheIdentity CacheIdentity
	Observer      func(MaintenanceEvent)
}

func New(store devin.SessionStore) *Engine {
	atomic, _ := store.(AtomicStore)
	return &Engine{Store: atomic, Now: time.Now}
}
func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
func sessionKey(scope, id string) string { return "devin:fusion:session:" + scope + ":" + Digest(id) }
func indexKey(scope, kind, id string) string {
	return "devin:fusion:index:" + scope + ":" + kind + ":" + Digest(id)
}
func (e *Engine) Load(ctx context.Context, scope, id string) (*Session, []byte, error) {
	if e.Store == nil {
		return nil, nil, E("store_unavailable", "Fusion requires a shared atomic session store", 503)
	}
	b, err := e.Store.Get(ctx, sessionKey(scope, id))
	if err != nil {
		return nil, nil, err
	}
	var s Session
	if json.Unmarshal(b, &s) != nil || s.Scope != scope || s.ID != id || s.ProfileRevision != s.Profile.Revision() {
		return nil, nil, E("session_corrupt", "stored Fusion state failed validation", 503)
	}
	if !e.now().Before(s.ExpiresAt) {
		return nil, nil, E("session_expired", "Fusion session expired; start a new session", 410)
	}
	return &s, b, nil
}
func (e *Engine) save(ctx context.Context, s *Session, old *[]byte) error {
	s.Revision++
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(b) > 12*1024*1024 {
		return E("context_limit", "Fusion session storage limit reached", 413)
	}
	ttl := s.ExpiresAt.Sub(e.now())
	if ttl <= 0 {
		return E("session_expired", "Fusion session expired", 410)
	}
	ok, err := e.Store.CompareAndSwap(ctx, sessionKey(s.Scope, s.ID), *old, b, ttl)
	if err != nil {
		return err
	}
	if !ok {
		return E("revision_conflict", "session changed concurrently; retry with the same idempotency key", 409)
	}
	*old = b
	return nil
}
func (e *Engine) putIndex(ctx context.Context, s *Session, kind, id string) error {
	key := indexKey(s.Scope, kind, id)
	next := []byte(s.ID)
	ttl := s.ExpiresAt.Sub(e.now()) + 24*time.Hour
	for n := 0; n < 3; n++ {
		old, err := e.Store.Get(ctx, key)
		if err != nil && !errors.Is(err, devin.ErrSessionNotFound) {
			return err
		}
		if string(old) == s.ID {
			return nil
		}
		if len(old) > 0 {
			if kind != "transcript" {
				return E("idempotency_conflict", "request or dispatch ID already belongs to another Fusion session", 409)
			}
			if string(old) == "!ambiguous" {
				return nil
			}
			next = []byte("!ambiguous")
		}
		ok, err := e.Store.CompareAndSwap(ctx, key, old, next, ttl)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return E("revision_conflict", "session index is busy", 409)
}
func (e *Engine) index(ctx context.Context, scope, kind, id string) (string, error) {
	if e.Store == nil {
		return "", nil
	}
	b, err := e.Store.Get(ctx, indexKey(scope, kind, id))
	if errors.Is(err, devin.ErrSessionNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if string(b) == "!ambiguous" {
		return "", E("session_ambiguous", "multiple conversations have this transcript; send X-Sub2API-Fusion-Session", 409)
	}
	return string(b), nil
}

// Resolve uses authenticated scope and actual protocol lineage, never cache keys
// or a hash of the initial prompt as a conversation identity.
func (e *Engine) Resolve(ctx context.Context, scope, explicit, idempotency string, r *apicompat.ResponsesRequest) (string, int64, error) {
	if explicit != "" && !SafeID(explicit) {
		return "", 0, E("session_invalid", "session ID must be 1..128 printable non-space characters", 400)
	}
	items, err := ParseInput(r)
	if err != nil {
		return "", 0, err
	}
	id := explicit
	bind := func(next string) error {
		if next != "" {
			if id != "" && id != next {
				return E("session_conflict", "request references different Fusion sessions", 409)
			}
			id = next
		}
		return nil
	}
	if r.PreviousResponseID != "" {
		next, err := e.index(ctx, scope, "response", r.PreviousResponseID)
		if err != nil {
			return "", 0, err
		}
		if next == "" {
			return "", 0, E("response_unknown", "previous response is unavailable in this API-key scope", 409)
		}
		if err = bind(next); err != nil {
			return "", 0, err
		}
	}
	for _, i := range items {
		if i.Type == "function_call_output" {
			next, err := e.index(ctx, scope, "tool", i.CallID)
			if err != nil {
				return "", 0, err
			}
			if next == "" {
				return "", 0, E("tool_unknown", "tool call is unavailable in this API-key scope", 409)
			}
			if err = bind(next); err != nil {
				return "", 0, err
			}
		}
	}
	if id == "" && idempotency != "" {
		next, err := e.index(ctx, scope, "request", idempotency)
		if err != nil {
			return "", 0, err
		}
		id = next
	}
	if id == "" {
		// Standard Chat/Messages clients resend history. Check the most recent public
		// assistant boundaries; identical independent chats become explicitly ambiguous.
		checked := 0
		for n := len(items) - 1; n > 0 && checked < 16; n-- {
			if items[n-1].Role != "assistant" && items[n-1].Type != "function_call" {
				continue
			}
			checked++
			next, err := e.index(ctx, scope, "transcript", Digest(items[:n]))
			if err != nil {
				return "", 0, err
			}
			if next != "" {
				id = next
				break
			}
		}
	}
	if id == "" {
		return "", 0, nil
	}
	s, _, err := e.Load(ctx, scope, id)
	if errors.Is(err, devin.ErrSessionNotFound) {
		seen, seenErr := e.index(ctx, scope, "seen", id)
		if seenErr != nil {
			return "", 0, seenErr
		}
		if seen != "" {
			return "", 0, E("session_expired", "Fusion session expired; use a new session ID", 410)
		}
		return id, 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	return id, s.AccountID, nil
}
func (e *Engine) Cancel(ctx context.Context, scope, id string) error {
	s, raw, err := e.Load(ctx, scope, id)
	if err != nil {
		return err
	}
	if err = e.Store.Set(ctx, indexKey(scope, "cancel", id), []byte("1"), s.ExpiresAt.Sub(e.now())+24*time.Hour); err != nil {
		return err
	}
	e.Runner.Cancel(scope, id)
	if e.Cache != nil {
		_ = e.Cache.Stop(ctx, s, "cancelled")
	}
	if s.ActiveRequest < 0 {
		s.State = Cancelled
		s.Pending = nil
		_ = e.save(ctx, s, &raw)
	}
	return nil
}
func (e *Engine) cancelled(ctx context.Context, s *Session) (bool, error) {
	_, err := e.Store.Get(ctx, indexKey(s.Scope, "cancel", s.ID))
	if errors.Is(err, devin.ErrSessionNotFound) {
		return false, nil
	}
	return err == nil, err
}

// A physical result journal is independent of the growing conversation snapshot.
// If its snapshot write fails, known usage can still be billed without reissuing
// the inference. The billing repository deduplicates the stable attempt ID.
func (e *Engine) journal(ctx context.Context, s *Session, a *Attempt) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return e.Store.Set(ctx, indexKey(s.Scope, "attempt", s.ID+":"+a.ID), b, max(s.ExpiresAt.Sub(e.now())+24*time.Hour, time.Hour))
}
func (e *Engine) recoverAttempt(ctx context.Context, s *Session, a *Attempt) error {
	if a.Status != "started" && a.Status != "unknown" {
		return nil
	}
	b, err := e.Store.Get(ctx, indexKey(s.Scope, "attempt", s.ID+":"+a.ID))
	if errors.Is(err, devin.ErrSessionNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved Attempt
	if json.Unmarshal(b, &saved) != nil || saved.ID != a.ID || saved.Role != a.Role || saved.Model != a.Model || saved.Turn != a.Turn || saved.RetryOf != a.RetryOf || saved.RecoveryKind != a.RecoveryKind {
		return E("session_corrupt", "physical usage journal failed validation", 503)
	}
	if saved.Status == "completed" {
		if saved.Settled && !a.Settled {
			s.TurnCostUSD += saved.CostUSD
		}
		*a = saved
	}
	return nil
}
