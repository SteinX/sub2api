package fusion

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
)

// All physical purposes compete for one durable per-turn monetary ledger.
// Unknown outcomes keep their estimate; only known settlement releases it.
type budgetEntry struct {
	Purpose Purpose `json:"purpose"`
	Amount  float64 `json:"amount"`
	Settled bool    `json:"settled"`
}
type turnBudget struct {
	LegacySpent float64                `json:"legacy_spent"`
	Entries     map[string]budgetEntry `json:"entries"`
}

func budgetKey(s *Session, turn int) string {
	return indexKey(s.Scope, "turn_budget", s.ID+":"+strconv.Itoa(turn))
}
func (e *Engine) updateBudget(ctx context.Context, s *Session, turn int, fn func(*turnBudget) error) error {
	key := budgetKey(s, turn)
	for n := 0; n < 64; n++ {
		old, err := e.Store.Get(ctx, key)
		if err != nil && !errors.Is(err, devin.ErrSessionNotFound) {
			return err
		}
		b := turnBudget{Entries: map[string]budgetEntry{}}
		if len(old) > 0 {
			if json.Unmarshal(old, &b) != nil || b.Entries == nil {
				return E("session_corrupt", "invalid turn cost ledger", 503)
			}
		} else if turn == s.Turn {
			// Import an existing session once, including unresolved old attempts.
			b.LegacySpent = s.TurnCostUSD
			for _, a := range s.Attempts {
				if a.Turn != turn {
					continue
				}
				amount := a.EstimateUSD
				if a.Settled {
					amount = a.CostUSD
					b.LegacySpent -= amount
				}
				b.Entries[a.ID] = budgetEntry{a.Purpose, amount, a.Settled}
			}
			b.LegacySpent = max(0, b.LegacySpent)
			for _, c := range s.Contexts {
				if c.Job != nil && c.Job.Turn == turn && !c.Job.Accounted {
					b.Entries[c.Job.ID] = budgetEntry{Compaction, c.Job.EstimateUSD, false}
				}
			}
		}
		if err = fn(&b); err != nil {
			return err
		}
		next, _ := json.Marshal(b)
		ok, err := e.Store.CompareAndSwap(ctx, key, old, next, max(s.ExpiresAt.Sub(e.now())+24*time.Hour, time.Hour))
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return E("revision_conflict", "turn cost ledger changed concurrently", 409)
}
func (e *Engine) reserveBudget(ctx context.Context, s *Session, id string, purpose Purpose, amount float64) error {
	if !validCost(amount) {
		return E("budget_exhausted", "invalid physical request estimate", 409)
	}
	return e.updateBudget(ctx, s, s.Turn, func(b *turnBudget) error {
		if prior, ok := b.Entries[id]; ok {
			if prior.Purpose != purpose || prior.Settled || prior.Amount != amount {
				return E("session_corrupt", "attempt budget identity changed", 503)
			}
			return nil
		}
		total := b.LegacySpent + amount
		for _, entry := range b.Entries {
			total += entry.Amount
		}
		if total > s.Profile.MaxCostUSD {
			return E("budget_exhausted", "physical request would exceed the shared Fusion turn cost budget", 409)
		}
		b.Entries[id] = budgetEntry{purpose, amount, false}
		return nil
	})
}
func (e *Engine) settleBudget(ctx context.Context, s *Session, a Attempt) error {
	if !a.Settled {
		return nil
	}
	if !validCost(a.CostUSD) {
		return E("billing_invalid", "invalid settled physical request cost", 503)
	}
	return e.updateBudget(ctx, s, a.Turn, func(b *turnBudget) error {
		if prior, ok := b.Entries[a.ID]; ok && prior.Settled {
			if prior.Amount != a.CostUSD {
				return E("billing_invalid", "settled physical request cost changed", 503)
			}
			return nil
		}
		b.Entries[a.ID] = budgetEntry{a.Purpose, a.CostUSD, true}
		return nil
	})
}
