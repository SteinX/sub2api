//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerCacheRowVersionGuardRepairsLegacyAndSurvivesStampLoss(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	old := devinSchedulerCacheAccount()
	old.UpdatedAt = time.Date(2026, 1, 1, 0, 0, 0, 123456000, time.UTC)
	newer := old
	newer.Name = "newer"
	newer.UpdatedAt = old.UpdatedAt.Add(time.Microsecond)
	require.NoError(t, cache.SetAccount(ctx, &newer))
	// Key loss alone must not permit a delayed older write.
	require.NoError(t, cache.rdb.Del(ctx, schedulerAccountWriteStampPrefix+"301").Err())
	require.ErrorIs(t, cache.SetAccount(ctx, &old), service.ErrSchedulerBucketWriteFenced)
	full, err := cache.GetAccount(ctx, old.ID)
	require.NoError(t, err)
	require.Equal(t, "newer", full.Name)
	require.NoError(t, cache.SetAccount(ctx, &newer))

	// An old process strips the new top-level marker. New readers refuse it;
	// the same authoritative DB row can repair it without a new version.
	legacy, err := json.Marshal(newer)
	require.NoError(t, err)
	require.NoError(t, cache.rdb.Set(ctx, schedulerAccountKey("301"), legacy, 0).Err())
	require.NoError(t, cache.rdb.Set(ctx, schedulerAccountMetaKey("301"), legacy, 0).Err())
	_, err = cache.GetAccount(ctx, old.ID)
	require.Error(t, err)
	require.NoError(t, cache.SetAccount(ctx, &newer))
	full, err = cache.GetAccount(ctx, old.ID)
	require.NoError(t, err)
	require.Equal(t, "newer", full.Name)

	conflict := newer
	conflict.Name = "different row at the same version"
	require.ErrorIs(t, cache.SetAccount(ctx, &conflict), service.ErrSchedulerBucketWriteFenced)
	conflict.UpdatedAt = newer.UpdatedAt.Add(time.Microsecond)
	require.NoError(t, cache.SetAccount(ctx, &conflict))
	unknown := conflict
	unknown.UpdatedAt = time.Time{}
	require.ErrorIs(t, cache.SetAccount(ctx, &unknown), service.ErrSchedulerBucketWriteFenced)
}

func TestSchedulerCacheStaleEncodingFailureAndDeletedRowStayClosed(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	old := devinSchedulerCacheAccount()
	old.UpdatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := old
	newer.Name = "keep-newer"
	newer.UpdatedAt = old.UpdatedAt.Add(time.Microsecond)
	require.NoError(t, cache.SetAccount(ctx, &newer))
	invalid := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	old.ExpiresAt = &invalid
	require.NoError(t, cache.SetAccount(ctx, &old))
	full, err := cache.GetAccount(ctx, old.ID)
	require.NoError(t, err)
	require.Equal(t, "keep-newer", full.Name)
	require.NoError(t, cache.DeleteAccount(ctx, old.ID))
	require.ErrorIs(t, cache.SetAccount(ctx, &newer), service.ErrSchedulerBucketWriteFenced)
	full, err = cache.GetAccount(ctx, old.ID)
	require.NoError(t, err)
	require.Nil(t, full)
}

func TestSchedulerCacheDelayedRebuildCannotUndoConfiguredDevinProfile(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 3, Platform: service.PlatformDevin, Mode: service.SchedulerModeSingle}
	old := devinSchedulerCacheAccount()
	old.UpdatedAt = time.Date(2026, 1, 1, 0, 0, 0, 123456000, time.UTC)
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{old}))
	_, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)

	// A background rebuild already owns old. The admin commits and publishes
	// newer before that rebuild resumes; no timing or polling is required.
	newer := devinSchedulerCacheAccount()
	newer.UpdatedAt = old.UpdatedAt.Add(time.Microsecond)
	newer.Extra[fusion.ExtraProfiles] = []fusion.Profile{fusion.DefaultProfile("fusion-configured", "gpt-5-4-mini-low", "swe-2-high")}
	require.NoError(t, cache.SetAccount(ctx, &newer))
	assertNew := func() {
		full, err := cache.GetAccount(ctx, old.ID)
		require.NoError(t, err)
		require.NotNil(t, full)
		require.True(t, full.IsModelSupported("fusion-configured"))
		snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
		require.NoError(t, err)
		require.True(t, hit)
		require.Len(t, snapshot, 1)
		require.True(t, snapshot[0].IsModelSupported("fusion-configured"))
	}
	assertNew()
	_ = cache.SetSnapshot(ctx, bucket, token, []service.Account{old})
	assertNew()
	_ = cache.SetAccount(ctx, &old)
	assertNew()
}

func devinSchedulerCacheAccount() service.Account {
	models := []*pb.ClientModelConfig{
		{ModelUid: "gpt-5-4-mini-low", ModelInfo: &pb.ModelInfo{ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}},
		{ModelUid: "swe-2-high", ModelInfo: &pb.ModelInfo{ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}},
		{ModelUid: "disabled-model", Disabled: true},
		{ModelUid: "opaque-harness", ModelInfo: &pb.ModelInfo{HarnessUids: []string{"unknown-harness"}}},
		{ModelUid: "hidden-fusion", ModelInfo: &pb.ModelInfo{ModelUid: "fusion-opaque"}},
		{ModelUid: "no-tools", ModelInfo: &pb.ModelInfo{ModelFeatures: &pb.ModelFeatures{}}},
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, devin.ModelID(model))
	}
	return service.Account{
		ID:          301,
		Platform:    service.PlatformDevin,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		GroupIDs:    []int64{3},
		Credentials: map[string]any{"access_token": "synthetic-access-token"},
		Extra: map[string]any{
			"devin_models":        ids,
			"devin_model_catalog": devin.NewCatalogSnapshot(models, "test-identity"),
			"unused_large_field":  strings.Repeat("unrelated", 4096),
		},
	}
}

func TestSchedulerCacheSnapshotPreservesDevinModelEligibility(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 3, Platform: service.PlatformDevin, Mode: service.SchedulerModeSingle}
	account := devinSchedulerCacheAccount()
	account.Extra[fusion.ExtraEnabled] = true
	account.Extra[fusion.ExtraProfiles] = []fusion.Profile{fusion.DefaultProfile("fusion-custom", "gpt-5-4-mini-low", "swe-2-high")}
	want := map[string]bool{
		"gpt-5-4-mini-low": true,
		"swe-2-high":       true,
		"fusion-custom":    true,
		"fusion-gpt-5-4-mini-low-sidekick-swe-2-high":   true,
		"fusion-gpt-5-4-mini-low-sidekick-no-tools":     false,
		"fusion-gpt-5-4-mini-low-sidekick-absent-model": false,
		"disabled-model": false,
		"opaque-harness": false,
		"hidden-fusion":  false,
		"absent-model":   false,
	}
	for model, supported := range want {
		require.Equal(t, supported, account.IsModelSupported(model), "full account: %s", model)
	}

	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{account}))
	snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, snapshot, 1)
	cached := snapshot[0]
	for model, supported := range want {
		require.Equal(t, supported, cached.IsModelSupported(model), "cached account: %s", model)
	}
	require.Equal(t, true, cached.Extra[fusion.ExtraEnabled])
	require.NotEmpty(t, cached.Extra[fusion.ExtraProfiles])
	require.NotEmpty(t, cached.Extra["devin_model_catalog"])
	require.Empty(t, cached.GetCredential("access_token"))
	require.NotContains(t, cached.Extra, "unused_large_field")
	full, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, "synthetic-access-token", full.GetCredential("access_token"))
	require.Equal(t, account.Extra["unused_large_field"], full.Extra["unused_large_field"])

	account.Extra[fusion.ExtraEnabled] = false
	require.NoError(t, cache.SetAccount(ctx, &account))
	snapshot, hit, err = cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, snapshot, 1)
	require.True(t, snapshot[0].IsModelSupported("gpt-5-4-mini-low"))
	require.False(t, snapshot[0].IsModelSupported("fusion-custom"))
	require.False(t, snapshot[0].IsModelSupported("fusion-gpt-5-4-mini-low-sidekick-swe-2-high"))
}

func TestSchedulerCacheSnapshotPreservesDevinFusionPolicy(t *testing.T) {
	const model = "fusion-gpt-5-4-mini-low-sidekick-swe-2-high"
	disabled := fusion.DefaultProfile(model, "gpt-5-4-mini-low", "swe-2-high")
	disabled.Enabled = false
	for _, tc := range []struct {
		name      string
		extra     map[string]any
		supported bool
	}{
		{name: "default enabled", supported: true},
		{name: "account disabled", extra: map[string]any{fusion.ExtraEnabled: false}},
		{name: "invalid enable value", extra: map[string]any{fusion.ExtraEnabled: "true"}},
		{name: "profile disabled", extra: map[string]any{fusion.ExtraProfiles: []fusion.Profile{disabled}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cache := newSchedulerCacheUnit(t)
			bucket := service.SchedulerBucket{GroupID: 3, Platform: service.PlatformDevin, Mode: service.SchedulerModeSingle}
			account := devinSchedulerCacheAccount()
			for key, value := range tc.extra {
				account.Extra[key] = value
			}
			require.Equal(t, tc.supported, account.IsModelSupported(model))
			token, err := cache.CaptureBucketWriteToken(ctx, bucket)
			require.NoError(t, err)
			require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{account}))
			snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
			require.NoError(t, err)
			require.True(t, hit)
			require.Len(t, snapshot, 1)
			require.True(t, snapshot[0].IsModelSupported("gpt-5-4-mini-low"))
			require.Equal(t, tc.supported, snapshot[0].IsModelSupported(model))
		})
	}
}
