//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDevinIdentifiedPricingExactCanonicalAndCatalogUpdates(t *testing.T) {
	const base, uid = "claude-opus-5-5", "claude-opus-5-5-medium"
	// Deliberately different synthetic rates establish precedence, not a price
	// claim about either provider. No remote catalog/configuration is modified.
	canonical := &LiteLLMModelPricing{InputCostPerToken: 3e-6, OutputCostPerToken: 7e-6}
	exact := &LiteLLMModelPricing{InputCostPerToken: 9e-6, OutputCostPerToken: 13e-6}
	for _, tc := range []struct {
		name  string
		cards map[string]*LiteLLMModelPricing
		want  *LiteLLMModelPricing
	}{
		{"empty", map[string]*LiteLLMModelPricing{}, nil},
		{"canonical only", map[string]*LiteLLMModelPricing{base: canonical}, canonical},
		{"exact only", map[string]*LiteLLMModelPricing{uid: exact}, exact},
		{"conflict prefers physical exact", map[string]*LiteLLMModelPricing{base: canonical, uid: exact}, exact},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := &PricingService{pricingData: tc.cards}
			require.Same(t, tc.want, catalog.GetIdentifiedModelPricing(uid))
			billing := NewBillingService(&config.Config{}, catalog)
			require.True(t, billing.HasIdentifiedTokenPricing(uid), "missing dynamic entry still permits the established built-in card")
			if tc.want != nil {
				cost, err := billing.CalculateCost(uid, UsageTokens{InputTokens: 1000, OutputTokens: 100}, 1)
				require.NoError(t, err)
				require.InDelta(t, 1000*tc.want.InputCostPerToken+100*tc.want.OutputCostPerToken, cost.TotalCost, 1e-12)
			}
			require.False(t, billing.HasIdentifiedTokenPricing("swe-2-high"), "unknown SWE price is never invented")
		})
	}
	catalog := &PricingService{pricingData: map[string]*LiteLLMModelPricing{base: canonical}}
	require.Same(t, canonical, catalog.GetIdentifiedModelPricing(uid))
	catalog.mu.Lock()
	catalog.pricingData = map[string]*LiteLLMModelPricing{base: canonical, uid: exact}
	catalog.mu.Unlock()
	require.Same(t, exact, catalog.GetIdentifiedModelPricing(uid), "a new exact entry must become effective without restarting billing")
}
