//go:build unit

package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
)

// The lookup checks cancellation before discovery and again after receiving its
// singleflight result. Pause the second check so another caller can complete a
// forced refresh before this caller returns. No sleeps or production test hooks.
type devinCatalogResultGate struct {
	context.Context
	checks  atomic.Int32
	ready   chan struct{}
	resume  chan struct{}
	release sync.Once
}

func (c *devinCatalogResultGate) Err() error {
	if c.checks.Add(1) == 2 {
		close(c.ready)
		<-c.resume
	}
	return c.Context.Err()
}

func TestDevinPricingModelsKeepSingleflightCatalogAndDeadlineTogether(t *testing.T) {
	f := newCatalogPricingFixture(t)
	f.models[1].PromoStatus.EndDate = nil
	d := f.service.devin
	ctx := &devinCatalogResultGate{Context: context.Background(), ready: make(chan struct{}), resume: make(chan struct{})}
	release := func() { ctx.release.Do(func() { close(ctx.resume) }) }
	t.Cleanup(release)
	type lookupResult struct {
		models []*pb.ClientModelConfig
		until  time.Time
		err    error
	}
	resultA := make(chan lookupResult, 1)
	go func() {
		models, until, err := d.pricingModels(ctx, f.account)
		resultA <- lookupResult{models, until, err}
	}()
	select {
	case <-ctx.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("request A did not receive its singleflight result")
	}

	d.mu.Lock()
	old := d.runtime[d.runtimeKey(context.Background(), f.account)]
	d.mu.Unlock()
	require.Equal(t, pb.ModelCostTier_MODEL_COST_TIER_FREE, devin.FindModel(old.models, futureFreeUID).ModelCostTier)
	require.Nil(t, devin.FindModel(old.models, futureFreeUID).GetPromoStatus().GetEndDate())
	// Request B uses exactly the forced-refresh API used by cache keepalive.
	f.models[1].ModelCostTier = pb.ModelCostTier_MODEL_COST_TIER_LOW
	refreshCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	paid, err := d.Models(refreshCtx, f.account, true)
	require.NoError(t, err)
	require.Equal(t, pb.ModelCostTier_MODEL_COST_TIER_LOW, devin.FindModel(paid, futureFreeUID).ModelCostTier)
	d.mu.Lock()
	newer := d.runtime[d.runtimeKey(context.Background(), f.account)]
	d.mu.Unlock()
	require.True(t, newer.modelsUntil.After(old.modelsUntil), "the forced refresh must have its own later freshness window")

	release()
	var a lookupResult
	select {
	case a = <-resultA:
	case <-time.After(5 * time.Second):
		t.Fatal("request A did not return after releasing its result")
	}
	require.NoError(t, a.err)
	require.Equal(t, pb.ModelCostTier_MODEL_COST_TIER_FREE, devin.FindModel(a.models, futureFreeUID).ModelCostTier)
	require.True(t, old.modelsUntil.Equal(a.until), "the old FREE catalog must not acquire request B's newer paid-catalog deadline")
	snapshot := f.service.captureDevinPricing(context.Background(), f.account, f.key, a.models, a.until, futureFreeUID)
	require.NotNil(t, snapshot)
	require.True(t, old.modelsUntil.Equal(snapshot.ValidUntil))
	_, freeAfterOriginalExpiry := devin.ModelFreeUntil(devin.FindModel(a.models, futureFreeUID), old.modelsUntil, a.until)
	require.False(t, freeAfterOriginalExpiry, "refreshing a paid catalog cannot extend the old FREE evidence")
	current := f.service.InspectDevinPricing(context.Background(), f.key, futureFreeUID)
	require.False(t, current.Identified)
	require.False(t, current.ZeroEffectiveCharge)
	require.Equal(t, 2, f.modelCalls)
	require.Zero(t, f.chatCalls)
}
