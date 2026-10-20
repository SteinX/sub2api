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
	gateway := &OpenAIGatewayService{}
	_, _, err := gateway.GetAccessToken(context.Background(), &a)
	require.Error(t, err, "unsupported endpoints must not use Devin credentials as OpenAI credentials")
}
