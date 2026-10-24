//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/stretchr/testify/require"
)

type fusionRecoveryLegacyRateLimitRepo struct {
	AccountRepository
	legacyWrites int
}

func (r *fusionRecoveryLegacyRateLimitRepo) SetRateLimited(context.Context, int64, time.Time) error {
	r.legacyWrites++
	return nil
}

type fusionRecoveryAtomicRateLimitRepo struct {
	*fusionRecoveryLegacyRateLimitRepo
	reset        time.Time
	extendWrites int
	fail         bool
}

func (r *fusionRecoveryAtomicRateLimitRepo) SetRateLimitedIfLater(_ context.Context, _ int64, reset time.Time) error {
	r.extendWrites++
	if r.fail {
		return errors.New("synthetic atomic write failure")
	}
	if reset.After(r.reset) {
		r.reset = reset
	}
	return nil
}

func TestFusionRejectionSQLCooldownUsesAtomicNeverShortenWrite(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserve_longer_reset", true: "error_has_no_unsafe_fallback"}[fail], func(t *testing.T) {
			shared, _ := fusionAdmissionServices(t)
			existing := time.Now().Add(10 * time.Minute)
			repo := &fusionRecoveryAtomicRateLimitRepo{fusionRecoveryLegacyRateLimitRepo: &fusionRecoveryLegacyRateLimitRepo{}, reset: existing, fail: fail}
			x := &devinFusionExecutor{service: &OpenAIGatewayService{devin: shared, accountRepo: repo}, account: &Account{ID: 7}}
			r, err := x.recordFusionRejection(context.Background(), fusion.Call{Request: &apicompat.ResponsesRequest{Model: "lead"}}, &devin.Error{Code: "resource_exhausted", Status: 429, RejectionKind: devin.RejectionRateLimit, HasRetryAfter: true, RetryAfter: time.Minute})
			require.NoError(t, err)
			require.NotNil(t, r)
			require.Equal(t, 1, repo.extendWrites)
			require.Zero(t, repo.legacyWrites, "no failure or stale snapshot may invoke the shortening setter")
			require.Equal(t, existing, repo.reset)
			require.ErrorContains(t, shared.waitFusionAdmission(context.Background(), x.account, "sidekick", fusion.CacheRefresh), "admission_deferred", "a SQL mirror failure does not discard the durable Fusion gate")
		})
	}
}

func TestFusionRejectionLegacyRepositoryKeepsRedisGateWithoutUnsafeSQLWrite(t *testing.T) {
	shared, _ := fusionAdmissionServices(t)
	repo := &fusionRecoveryLegacyRateLimitRepo{}
	x := &devinFusionExecutor{service: &OpenAIGatewayService{devin: shared, accountRepo: repo}, account: &Account{ID: 7}}
	r, err := x.recordFusionRejection(context.Background(), fusion.Call{Request: &apicompat.ResponsesRequest{Model: "lead"}}, &devin.Error{Code: "resource_exhausted", Status: 429, RejectionKind: devin.RejectionRateLimit, HasRetryAfter: true, RetryAfter: time.Minute})
	require.NoError(t, err)
	require.NotNil(t, r)
	require.Zero(t, repo.legacyWrites)
	require.ErrorContains(t, shared.waitFusionAdmission(context.Background(), x.account, "sidekick", fusion.CacheRefresh), "admission_deferred")
}
