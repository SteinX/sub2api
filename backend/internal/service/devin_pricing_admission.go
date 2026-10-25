package service

import (
	"context"
	"math"
	"strings"
	"time"
)

// DevinPricingAdmission exposes only the calling key's identified price status,
// not rates, account/group/channel IDs, or provider credentials. It performs the
// same physical-member price checks as Fusion without reserving or generating.
type DevinPricingAdmission struct {
	Model                string   `json:"model"`
	Identified           bool     `json:"identified"`
	Calculable           bool     `json:"calculable"`
	Source               string   `json:"source"`
	ZeroEffectiveCharge  bool     `json:"zero_effective_charge"`
	ContextCostUpperUSD  *float64 `json:"context_cost_upper_usd,omitempty"`
	CostBoundUnavailable string   `json:"cost_bound_unavailable,omitempty"`
}

// InspectDevinContextPrice uses the same effective physical price, charging a
// full declared context independently as input, cache read and cache write.
// This deliberately conservative local quote is not a supplier spending cap.
func (s *OpenAIGatewayService) InspectDevinContextPrice(ctx context.Context, key *APIKey, model string, contextTokens, outputTokens int) DevinPricingAdmission {
	result := s.InspectDevinPricing(ctx, key, model)
	if !result.Calculable || contextTokens < 1 || contextTokens > 8_000_000 || outputTokens < 1 || outputTokens > 32768 {
		return result
	}
	if s.resolver == nil {
		result.CostBoundUnavailable = "effective_price_resolver_unavailable"
		return result
	}
	resolved := s.resolver.Resolve(ctx, PricingInput{Model: model, Group: key.Group, GroupID: key.GroupID})
	if resolved == nil || resolved.Mode != BillingModeToken || len(resolved.Intervals) > 0 || len(resolved.RequestTiers) > 0 || resolved.BasePricing == nil {
		result.CostBoundUnavailable = "non_flat_token_pricing"
		return result
	}
	if resolved.longContextPricingEnabled && resolved.BasePricing.LongContextInputThreshold > 0 {
		result.CostBoundUnavailable = "context_tier_pricing"
		return result
	}
	if resolved.channelPricing != nil && resolved.channelPricing.TimePricing != nil || resolved.Source == PricingSourceLiteLLM && isDeepSeekModel(model) {
		result.CostBoundUnavailable = "time_varying_pricing"
		return result
	}
	x := &devinFusionExecutor{service: s, key: key}
	cost, err := x.cost(ctx, model, UsageTokens{InputTokens: contextTokens, CacheReadTokens: contextTokens, CacheCreationTokens: contextTokens, OutputTokens: outputTokens}, time.Now())
	if err == nil && cost != nil {
		upper := math.Max(cost.ActualCost, cost.TotalCost)
		// Native aggregate cache writes use the short cache lane. Also quote
		// the whole context in the long cache lane so mixed TTL cannot lower
		// the estimate when a caller's identified flat card supports it.
		long, err := x.cost(ctx, model, UsageTokens{InputTokens: contextTokens, CacheReadTokens: contextTokens, CacheCreationTokens: contextTokens, CacheCreation1hTokens: contextTokens, OutputTokens: outputTokens}, time.Now())
		if err != nil || long == nil {
			result.CostBoundUnavailable = "cache_price_bound_unavailable"
			return result
		}
		upper = math.Max(upper, math.Max(long.ActualCost, long.TotalCost))
		if upper >= 0 && !math.IsNaN(upper) && !math.IsInf(upper, 0) {
			result.ContextCostUpperUSD = &upper
		}
	}
	return result
}

func (s *OpenAIGatewayService) InspectDevinPricing(ctx context.Context, key *APIKey, model string) DevinPricingAdmission {
	result := DevinPricingAdmission{Model: model, Source: "unidentified"}
	if s == nil || key == nil || key.Group == nil || key.Group.Platform != PlatformDevin || strings.HasPrefix(strings.ToLower(model), "fusion-") {
		return result
	}
	result.Identified, _ = s.hasIdentifiedOpenAIResponsePricing(ctx, model, key)
	if !result.Identified {
		return result
	}
	result.Source = "identified_default"
	if resolved := s.resolveOpenAIChannelPricing(ctx, model, key); resolved != nil {
		result.Source = resolved.Source
	}
	x := &devinFusionExecutor{service: s, key: key}
	cost, err := x.cost(ctx, model, UsageTokens{InputTokens: 1, OutputTokens: 1, CacheReadTokens: 1, CacheCreationTokens: 1}, time.Now())
	result.Calculable = err == nil && cost != nil
	if result.Calculable {
		result.ZeroEffectiveCharge = cost.ActualCost == 0 && cost.TotalCost == 0
	}
	return result
}
