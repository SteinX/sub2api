//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type devinListingRepo struct {
	AccountRepository
	accounts []Account
}

func (r *devinListingRepo) ListSchedulable(context.Context) ([]Account, error) {
	return r.accounts, nil
}

func TestDevinModelsListingUsesAccountGrantsAndMappings(t *testing.T) {
	a := Account{Platform: PlatformDevin, Credentials: map[string]any{}, Extra: map[string]any{"devin_models": []any{"swe-2-max", "swe-2-high"}}}
	repo := &devinListingRepo{accounts: []Account{a, {Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"claude": "claude"}}}}}
	s := &GatewayService{accountRepo: repo}
	require.Equal(t, []string{"swe-2-high", "swe-2-max"}, s.GetAvailableModels(context.Background(), nil, PlatformDevin))
	repo.accounts[0].Credentials = map[string]any{"model_mapping": map[string]any{"my-alias": "swe-2-max"}}
	require.Equal(t, []string{"my-alias"}, s.GetAvailableModels(context.Background(), nil, PlatformDevin))
	repo.accounts[0].Credentials = map[string]any{"model_mapping": map[string]any{"unavailable-alias": "unavailable-model"}}
	require.Empty(t, s.GetAvailableModels(context.Background(), nil, PlatformDevin))
	gateway := &OpenAIGatewayService{}
	_, _, err := gateway.GetAccessToken(context.Background(), &a)
	require.Error(t, err, "unsupported endpoints must not use Devin credentials as OpenAI credentials")
}

func TestDevinSchedulingUsesCredentialScopedCatalogs(t *testing.T) {
	ctx := context.Background()
	accounts := []Account{
		{ID: 1, Platform: PlatformDevin, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Extra: map[string]any{"devin_models": []any{"first-model"}}},
		{ID: 2, Platform: PlatformDevin, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Extra: map[string]any{"devin_models": []string{"second-model"}}},
	}
	listing := &GatewayService{accountRepo: &devinListingRepo{accounts: accounts}}
	require.Equal(t, []string{"first-model", "second-model"}, listing.GetAvailableModels(ctx, nil, PlatformDevin))
	scheduler := &defaultOpenAIAccountScheduler{}
	for i := range accounts {
		for _, model := range []string{"first-model", "second-model"} {
			want := i == 0 && model == "first-model" || i == 1 && model == "second-model"
			require.Equal(t, want, accounts[i].IsModelSupported(model))
			reason := openAICompatibleAccountEligibilityFailureReasonBeforeProfit(ctx, &accounts[i], PlatformDevin, model, false, "")
			require.Equal(t, want, reason == "", "legacy scheduler: %s", reason)
			ok, reason := scheduler.isAccountRequestCompatibleReason(ctx, &accounts[i], OpenAIAccountScheduleRequest{RequestedModel: model})
			require.Equal(t, want, ok, "advanced scheduler: %s", reason)
		}
	}
	account := &accounts[0]
	account.Credentials = map[string]any{"model_mapping": map[string]any{"public-*": "first-model", "missing": "absent-model"}}
	require.True(t, account.IsModelSupported("public-alias"))
	require.False(t, account.IsModelSupported("missing"))
	require.False(t, account.IsModelSupported("first-model"), "explicit aliases keep their whitelist semantics")
	account.Extra = nil
	require.False(t, account.IsModelSupported("public-alias"), "missing catalogs must not grant every model")
}
