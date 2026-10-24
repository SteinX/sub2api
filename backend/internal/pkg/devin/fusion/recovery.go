package fusion

import (
	"context"
	"time"
)

const maxInferenceAttempts = 3

func validRejection(r *Rejection) bool {
	if r == nil || !r.NotExecuted || r.RetryAfter < 0 {
		return false
	}
	return r.Kind == RejectionAuth || r.Kind == RejectionRateLimit || r.Kind == RejectionModelCapacity
}

// Recovery cannot be inferred from an empty response or a transport error. Even
// all-zero usage is observed usage, so it must not authorize another send.
func acceptedRejection(outcome Outcome, err error) *Rejection {
	if err == nil || !outcome.KnownFailure || !validRejection(outcome.Rejection) {
		return nil
	}
	if outcome.Response != nil && (outcome.Response.Usage != nil || len(outcome.Response.Output) != 0) {
		return nil
	}
	r := *outcome.Rejection
	return &r
}

func recoverableAttempt(a *Attempt) bool {
	if a == nil || a.Status != "completed" || !a.Settled {
		return false
	}
	// An admission interrupted before sending can resume the same logical call,
	// while preserving the already allocated recovery and request budgets.
	if a.NotStarted && a.Applied && a.Failure == "" {
		return true
	}
	return a.Failure != "" && validRejection(a.Rejection) && (a.Response == nil || a.Response.Usage == nil && len(a.Response.Output) == 0)
}

// The chain lives in the attempt ledger, so an HTTP retry or process restart
// cannot reset its send limit or allocate a second authentication recovery.
func recoveryChain(s *Session, a *Attempt) (attempts int, authUsed bool, err error) {
	attempts = 1
	current := a
	for {
		authUsed = authUsed || current.RecoveryKind == RejectionAuth
		if current.RetryOf == "" {
			return attempts, authUsed, nil
		}
		var prior *Attempt
		for i := range s.Attempts {
			if s.Attempts[i].ID == current.RetryOf {
				prior = &s.Attempts[i]
				break
			}
		}
		if prior == nil || prior.ID == current.ID || prior.Role != a.Role || prior.Model != a.Model || prior.Turn != a.Turn || attempts >= len(s.Attempts) {
			return 0, false, E("session_corrupt", "invalid physical recovery chain", 503)
		}
		attempts++
		current = prior
	}
}

func (e *Engine) prepareRecovery(ctx, persistCtx context.Context, s *Session, raw *[]byte, a *Attempt) (RejectionKind, bool, error) {
	count, authUsed, err := recoveryChain(s, a)
	if err != nil {
		return "", false, err
	}
	if count >= maxInferenceAttempts {
		return "", authUsed, E("recovery_exhausted", "explicit provider rejection recovery limit reached", 503)
	}
	kind := RejectionKind("")
	if a.Rejection != nil {
		kind = a.Rejection.Kind
		if kind == RejectionAuth && authUsed {
			return "", true, E("auth_recovery_exhausted", "authentication recovery was already attempted for this inference", 401)
		}
		if a.RetryNotBefore.IsZero() {
			return "", authUsed, E("session_corrupt", "rejected inference has no durable recovery time", 503)
		}
	}
	// Both the result journal and the conversation must acknowledge the refusal
	// before any replacement inference can start. A failed write only pauses it.
	if err = e.journal(persistCtx, s, a); err != nil {
		return "", authUsed, err
	}
	if err = e.settleBudget(persistCtx, s, *a); err != nil {
		return "", authUsed, err
	}
	if err = e.save(persistCtx, s, raw); err != nil {
		return "", authUsed, err
	}
	if err = e.waitRecovery(ctx, s, a.RetryNotBefore); err != nil {
		return "", authUsed, err
	}
	return kind, authUsed || kind == RejectionAuth, nil
}

func (e *Engine) waitRecovery(ctx context.Context, s *Session, notBefore time.Time) error {
	deadline := s.TurnStarted.Add(time.Duration(s.Profile.MaxWallSeconds) * time.Second)
	if !e.now().Before(deadline) || !notBefore.Before(deadline) {
		return E("budget_exhausted", "provider recovery wait exceeds the remaining Fusion turn deadline", 409)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cancelled, err := e.cancelled(ctx, s); err != nil {
		return err
	} else if cancelled {
		return E("cancelled", "Fusion session was cancelled during recovery", 409)
	}
	timer := time.NewTimer(max(0, notBefore.Sub(e.now())))
	defer timer.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			if !e.now().Before(deadline) {
				return E("budget_exhausted", "Fusion turn deadline expired during recovery", 409)
			}
			return ctx.Err()
		case <-poll.C:
			if cancelled, err := e.cancelled(ctx, s); err != nil {
				return err
			} else if cancelled {
				return E("cancelled", "Fusion session was cancelled during recovery", 409)
			}
		case <-timer.C:
			if err := ctx.Err(); err != nil {
				return err
			}
			if cancelled, err := e.cancelled(ctx, s); err != nil {
				return err
			} else if cancelled {
				return E("cancelled", "Fusion session was cancelled during recovery", 409)
			}
			if !e.now().Before(deadline) {
				return E("budget_exhausted", "Fusion turn deadline expired during recovery", 409)
			}
			return nil
		}
	}
}
