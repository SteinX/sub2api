package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/google/uuid"
)

// Ordinary inflight reservations deliberately fail open. A multi-inference
// Fusion turn requires a confirmed reservation before each physical request.
func (x *devinFusionExecutor) reserve(ctx context.Context, amount float64, attemptIDs ...string) (*InflightReservation, error) {
	s := x.service.billingCacheService
	if amount <= 0 || s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return nil, nil
	}
	rc, ok := s.cache.(InflightBalanceReservationCache)
	if !ok {
		return nil, fusion.E("billing_unavailable", "atomic balance reservations are unavailable", 503)
	}
	sc, ok := s.cache.(InflightScopeReservationCache)
	if !ok {
		return nil, fusion.E("billing_unavailable", "atomic quota reservations are unavailable", 503)
	}
	r := &InflightReservation{userID: x.key.UserID, requestID: uuid.NewString(), amount: amount, ttl: time.Duration(x.profile.MaxWallSeconds+120) * time.Second, scopeCache: sc}
	if len(attemptIDs) > 0 {
		r.requestID = x.attemptKey(attemptIDs[0])
	}
	r.refs.Store(1)
	fail := func(err error) (*InflightReservation, error) { r.Release(); return nil, err }
	reservationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	subscriptionBill := x.key.Group != nil && x.key.Group.IsSubscriptionType() && x.subscription != nil
	if !subscriptionBill {
		balance, err := s.GetUserBalance(reservationCtx, x.key.UserID)
		if err != nil {
			return fail(err)
		}
		if balance < amount {
			return fail(ErrInsufficientBalance)
		}
		allowed, _, err := rc.ReserveInflightBalance(reservationCtx, x.key.UserID, r.requestID, amount, balance, r.ttl)
		if err != nil {
			return fail(err)
		}
		if !allowed {
			return fail(ErrInsufficientBalance)
		}
		r.cache = rc
	}
	reserveScope := func(scope string) (float64, error) {
		pending, err := sc.ReserveInflightScope(reservationCtx, scope, r.requestID, amount, r.ttl)
		if err != nil {
			return 0, err
		}
		r.scopes = append(r.scopes, scope)
		return pending, nil
	}
	if subscriptionBill && (x.key.Group.HasDailyLimit() || x.key.Group.HasWeeklyLimit() || x.key.Group.HasMonthlyLimit()) {
		pending, err := reserveScope(inflightSubscriptionScope(x.subscription.ID))
		if err != nil {
			return fail(err)
		}
		if err = s.checkFixedChargeSubscription(reservationCtx, x.key.UserID, x.key.Group, amount+pending); err != nil {
			return fail(err)
		}
	}
	if x.key.Quota > 0 || x.key.HasRateLimits() {
		pending, err := reserveScope(inflightAPIKeyScope(x.key.ID))
		if err != nil {
			return fail(err)
		}
		if err = s.checkFixedChargeAPIKey(reservationCtx, x.key, amount+pending); err != nil {
			return fail(err)
		}
	}
	return r, nil
}

// Stable IDs let a restarted settlement release the original reservation too.
// Releases of absent scope entries are idempotent; no new money is reserved here.
func (x *devinFusionExecutor) releaseSettledReservation(a fusion.Attempt) {
	if r := x.reservations[a.ID]; r != nil {
		r.HandlerDone()
		delete(x.reservations, a.ID)
		delete(x.holdUnsettled, a.ID)
		return
	}
	if a.EstimateUSD <= 0 || x.service.billingCacheService == nil {
		return
	}
	s := x.service.billingCacheService
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return
	}
	rc, ok := s.cache.(InflightBalanceReservationCache)
	if !ok {
		return
	}
	sc, ok := s.cache.(InflightScopeReservationCache)
	if !ok {
		return
	}
	r := &InflightReservation{cache: rc, scopeCache: sc, userID: x.key.UserID, requestID: x.attemptKey(a.ID), scopes: []string{inflightAPIKeyScope(x.key.ID)}}
	if x.subscription != nil {
		r.scopes = append(r.scopes, inflightSubscriptionScope(x.subscription.ID))
	}
	r.Release()
}

func (x *devinFusionExecutor) attemptKey(id string) string {
	prefix := x.attemptPrefix
	if prefix == "" {
		prefix = "fusion"
	}
	return prefix + ":" + id
}
