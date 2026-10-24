//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func fusionAdmissionServices(t *testing.T) (*DevinService, *DevinService) {
	t.Helper()
	server := miniredis.RunT(t)
	makeService := func() *DevinService {
		client := redis.NewClient(&redis.Options{Addr: server.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		return &DevinService{store: devin.NewRedisSessionStore(client)}
	}
	return makeService(), makeService()
}

func TestFusionAdmissionSharedAccountAndConcreteModelScopes(t *testing.T) {
	first, restarted := fusionAdmissionServices(t)
	account := &Account{ID: 7, Credentials: map[string]any{"api_key": "synthetic-secret"}}
	ctx := context.Background()
	require.NoError(t, first.noteFusionRejection(ctx, account, "lead", &fusion.Rejection{Kind: fusion.RejectionModelCapacity, NotExecuted: true, RetryAfter: time.Minute}))
	require.ErrorContains(t, restarted.waitFusionAdmission(ctx, account, "lead", fusion.CacheRefresh), "admission_deferred")
	require.NoError(t, restarted.waitFusionAdmission(ctx, account, "sidekick", fusion.CacheRefresh), "a model refusal must not suppress the other member")
	require.NoError(t, restarted.waitFusionAdmission(ctx, &Account{ID: 8}, "lead", fusion.CacheRefresh))
	require.NoError(t, restarted.waitFusionAdmission(ctx, &Account{ID: 7, Credentials: map[string]any{"base_url": "https://other.example"}}, "lead", fusion.CacheRefresh))
	require.NoError(t, first.noteFusionRejection(ctx, account, "lead", &fusion.Rejection{Kind: fusion.RejectionRateLimit, NotExecuted: true, RetryAfter: time.Minute}))
	require.ErrorContains(t, restarted.waitFusionAdmission(ctx, account, "sidekick", fusion.Compaction), "admission_deferred", "account quota applies to both roles and maintenance")
	for _, key := range fusionAdmissionKeys(account, "lead") {
		require.NotContains(t, key, "synthetic-secret")
	}
}

func TestFusionAdmissionConcurrentShortHintsCannotReopenLongCooldown(t *testing.T) {
	first, second := fusionAdmissionServices(t)
	account := &Account{ID: 7}
	ctx := context.Background()
	require.NoError(t, first.noteFusionRejection(ctx, account, "lead", &fusion.Rejection{Kind: fusion.RejectionRateLimit, NotExecuted: true, RetryAfter: 10 * time.Minute}))
	key := fusionAdmissionKeys(account, "lead")[0]
	before, err := first.store.Get(ctx, key)
	require.NoError(t, err)
	var workers sync.WaitGroup
	errorsSeen := make(chan error, 16)
	for n := range 16 {
		workers.Add(1)
		go func(delay time.Duration) {
			defer workers.Done()
			errorsSeen <- second.noteFusionRejection(ctx, account, "lead", &fusion.Rejection{Kind: fusion.RejectionRateLimit, NotExecuted: true, RetryAfter: delay})
		}(time.Duration(n+1) * time.Second)
	}
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		require.NoError(t, err)
	}
	after, err := second.store.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	started := time.Now()
	require.ErrorContains(t, second.waitFusionAdmission(bounded, account, "sidekick", fusion.Inference), "budget_exhausted")
	require.Less(t, time.Since(started), 500*time.Millisecond, "a hint longer than the remaining budget must not be shortened and retried")
}

func TestFusionAdmissionForegroundWaitAndCancellation(t *testing.T) {
	first, second := fusionAdmissionServices(t)
	account := &Account{ID: 7}
	ctx := context.Background()
	require.NoError(t, first.noteFusionRejection(ctx, account, "lead", &fusion.Rejection{Kind: fusion.RejectionModelCapacity, NotExecuted: true, RetryAfter: 40 * time.Millisecond}))
	bounded, release := context.WithTimeout(ctx, time.Second)
	defer release()
	require.NoError(t, second.waitFusionAdmission(bounded, account, "lead", fusion.Inference))
	require.NoError(t, first.noteFusionRejection(ctx, account, "lead", &fusion.Rejection{Kind: fusion.RejectionRateLimit, NotExecuted: true, RetryAfter: time.Minute}))
	cancelled, cancel := context.WithTimeout(ctx, 2*time.Minute)
	done := make(chan error, 1)
	go func() { done <- second.waitFusionAdmission(cancelled, account, "lead", fusion.Inference) }()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancelled foreground wait remained queued")
	}
}

func TestFusionAdmissionDoesNotPersistAmbiguityOrAuthentication(t *testing.T) {
	first, second := fusionAdmissionServices(t)
	account := &Account{ID: 7}
	ctx := context.Background()
	for _, rejection := range []*fusion.Rejection{
		nil,
		{Kind: fusion.RejectionRateLimit, NotExecuted: false, RetryAfter: time.Minute},
		{Kind: fusion.RejectionAuth, NotExecuted: true, RetryAfter: time.Minute},
		{Kind: fusion.RejectionRateLimit, NotExecuted: true},
	} {
		require.NoError(t, first.noteFusionRejection(ctx, account, "lead", rejection))
	}
	require.NoError(t, second.waitFusionAdmission(ctx, account, "lead", fusion.CacheRefresh))
}

type fusionAdmissionBrokenStore struct{ devin.SessionStore }

func (fusionAdmissionBrokenStore) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("synthetic store outage")
}

func TestFusionAdmissionFailsClosedWhenPersistenceIsUnavailable(t *testing.T) {
	first, _ := fusionAdmissionServices(t)
	account := &Account{ID: 7}
	key := fusionAdmissionKeys(account, "lead")[0]
	require.NoError(t, first.store.Set(context.Background(), key, []byte(`{"until":"invalid"}`), time.Minute))
	require.ErrorContains(t, first.waitFusionAdmission(context.Background(), account, "lead", fusion.Inference), "store_unavailable")
	broken := &DevinService{store: fusionAdmissionBrokenStore{SessionStore: first.store}}
	require.ErrorContains(t, broken.waitFusionAdmission(context.Background(), account, "lead", fusion.Inference), "store_unavailable")
	require.ErrorContains(t, broken.noteFusionRejection(context.Background(), account, "lead", &fusion.Rejection{Kind: fusion.RejectionRateLimit, NotExecuted: true, RetryAfter: time.Minute}), "atomic persistence")
}

func TestFusionWarmSkipsSharedCooldownWithoutAnotherGeneration(t *testing.T) {
	f := newWarmFixture(t)
	ctx := context.Background()
	require.NoError(t, f.service.devin.noteFusionRejection(ctx, f.account, f.snapshot.Model, &fusion.Rejection{Kind: fusion.RejectionModelCapacity, NotExecuted: true, RetryAfter: time.Minute}))
	result, err := f.adapter.Refresh(ctx, f.snapshot, fusion.Attempt{ID: "blocked-maintenance", Purpose: fusion.CacheRefresh, Role: fusion.Lead, EstimateUSD: 1, StartedAt: time.Now()})
	require.ErrorContains(t, err, "admission_deferred")
	require.True(t, result.Outcome.KnownFailure)
	require.True(t, result.Outcome.NotStarted)
	require.Zero(t, f.calls.Load())
	require.Empty(t, f.bills.commands)
}

type fusionAdmissionAccountRepo struct{ *fusionAccountRepo }

func (*fusionAdmissionAccountRepo) SetRateLimited(context.Context, int64, time.Time) error {
	return nil
}

func TestFusionWarmRefusalSuppressesOtherWorkersWithoutReplaying(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		frame []byte
	}{
		{"rate_limit", devinTestFrame(2, []byte(`{"error":{"code":"resource_exhausted","message":"synthetic refusal"},"metadata":{"retry-after":["60"]}}`))},
		{"model_capacity", devinTestFrame(2, []byte(`{"error":{"code":"unimplemented","message":"We are currently experiencing capacity issues with this serving model"}}`))},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			f := newWarmFixture(t)
			f.service.accountRepo = &fusionAdmissionAccountRepo{f.service.accountRepo.(*fusionAccountRepo)}
			f.chat = func(*http.Request, *pb.GetChatMessageRequest) (*http.Response, error) {
				return devinReply(200, fixture.frame), nil
			}
			ctx := context.Background()
			result, err := f.adapter.Refresh(ctx, f.snapshot, fusion.Attempt{ID: "refused-maintenance", Purpose: fusion.CacheRefresh, Role: fusion.Lead, EstimateUSD: 1, StartedAt: time.Now()})
			require.Error(t, err)
			require.True(t, result.Outcome.KnownFailure)
			require.EqualValues(t, 1, f.calls.Load())
			restarted := &DevinService{store: f.service.devin.store}
			require.ErrorContains(t, restarted.waitFusionAdmission(ctx, f.account, f.snapshot.Model, fusion.CacheRefresh), "admission_deferred")
			var admission fusionAdmission
			keys := fusionAdmissionKeys(f.account, f.snapshot.Model)
			key := keys[1]
			if fixture.name == "rate_limit" {
				key = keys[0]
			}
			raw, err := restarted.store.Get(ctx, key)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &admission))
			require.True(t, admission.Until.After(time.Now()))
		})
	}
}
