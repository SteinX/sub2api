package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
)

type fusionAdmission struct {
	Until time.Time `json:"until"`
}

// Account refusals cover both roles and maintenance; serving-model refusals
// cover only that concrete member. Neither key depends on a caller's session.
func fusionAdmissionKeys(account *Account, model string) [2]string {
	base := strings.TrimRight(strings.TrimSpace(account.GetCredential("base_url")), "/")
	if base == "" {
		base = devin.DefaultBaseURL
	}
	prefix := "devin:fusion:admission:" + fusion.Digest([]string{strconv.FormatInt(account.ID, 10), base})
	return [2]string{prefix + ":account", prefix + ":model:" + fusion.Digest(model)}
}

// waitFusionAdmission rechecks shared deadlines before sending. Maintenance
// never waits for a cooldown or consumes a foreground recovery opportunity.
func (s *DevinService) waitFusionAdmission(ctx context.Context, account *Account, model string, purpose fusion.Purpose) error {
	if s == nil || s.store == nil || account == nil {
		return fusion.E("store_unavailable", "shared Fusion admission is unavailable", 503)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var until time.Time
		for _, key := range fusionAdmissionKeys(account, model) {
			raw, err := s.store.Get(ctx, key)
			if errors.Is(err, devin.ErrSessionNotFound) {
				continue
			}
			if err != nil {
				return fusion.E("store_unavailable", "shared Fusion admission could not be read", 503)
			}
			var gate fusionAdmission
			if json.Unmarshal(raw, &gate) != nil || gate.Until.IsZero() {
				return fusion.E("store_unavailable", "shared Fusion admission is invalid", 503)
			}
			if gate.Until.After(until) {
				until = gate.Until
			}
		}
		if !until.After(time.Now()) {
			return nil
		}
		if !purpose.IsInference() {
			return fusion.E("admission_deferred", "background Fusion maintenance skipped during upstream cooldown", 429)
		}
		deadline, bounded := ctx.Deadline()
		if !bounded || !until.Before(deadline) {
			return fusion.E("budget_exhausted", "upstream cooldown exceeds the remaining Fusion turn deadline", 409)
		}
		timer := time.NewTimer(min(time.Until(until), 250*time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// noteFusionRejection stores an observed refusal even after caller disconnect.
// CAS keeps a shorter hint from reopening a cooldown extended by another worker.
func (s *DevinService) noteFusionRejection(ctx context.Context, account *Account, model string, rejection *fusion.Rejection) error {
	if rejection == nil || !rejection.NotExecuted || rejection.RetryAfter <= 0 || rejection.Kind == fusion.RejectionAuth {
		return nil
	}
	if s == nil || s.store == nil || account == nil {
		return fusion.E("store_unavailable", "shared Fusion admission is unavailable", 503)
	}
	keys := fusionAdmissionKeys(account, model)
	var key string
	switch rejection.Kind {
	case fusion.RejectionRateLimit:
		key = keys[0]
	case fusion.RejectionModelCapacity:
		key = keys[1]
	default:
		return nil
	}
	store, ok := s.store.(fusion.AtomicStore)
	if !ok {
		return fusion.E("store_unavailable", "shared Fusion admission requires atomic persistence", 503)
	}
	until := time.Now().Add(rejection.RetryAfter)
	writeCtx, release := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer release()
	for {
		raw, err := store.Get(writeCtx, key)
		if errors.Is(err, devin.ErrSessionNotFound) {
			raw, err = nil, nil
		}
		if err != nil {
			return fusion.E("store_unavailable", "upstream cooldown could not be read", 503)
		}
		if raw != nil {
			var current fusionAdmission
			if json.Unmarshal(raw, &current) != nil || current.Until.IsZero() {
				return fusion.E("store_unavailable", "shared Fusion admission is invalid", 503)
			}
			if !until.After(current.Until) {
				return nil
			}
		}
		ttl := time.Until(until)
		if ttl <= 0 {
			return nil
		}
		next, _ := json.Marshal(fusionAdmission{Until: until})
		updated, err := store.CompareAndSwap(writeCtx, key, raw, next, ttl)
		if err != nil {
			return fusion.E("store_unavailable", "upstream cooldown could not be persisted", 503)
		}
		if updated {
			return nil
		}
		if err := writeCtx.Err(); err != nil {
			return fusion.E("store_unavailable", "upstream cooldown could not be persisted", 503)
		}
	}
}
