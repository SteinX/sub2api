package service

import (
	"context"
	"encoding/json"
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
	result, pricing := s.inspectDevinPricing(ctx, key, model)
	if !result.Calculable || contextTokens < 1 || contextTokens > 8_000_000 || outputTokens < 1 || outputTokens > 32768 {
		return result
	}
	if pricing == nil {
		result.CostBoundUnavailable = "effective_price_resolver_unavailable"
		return result
	}
	ctx = withDevinPricing(ctx, pricing)
	resolved := pricing.Prices[model].resolved()
	if resolved == nil || resolved.Mode != BillingModeToken || len(resolved.Intervals) > 0 || len(resolved.RequestTiers) > 0 || resolved.BasePricing == nil {
		result.CostBoundUnavailable = "non_flat_token_pricing"
		return result
	}
	if resolved.longContextPricingEnabled && resolved.BasePricing.LongContextInputThreshold > 0 {
		result.CostBoundUnavailable = "context_tier_pricing"
		return result
	}
	if key.Group.PeakRateEnabled || resolved.channelPricing != nil && resolved.channelPricing.TimePricing != nil || resolved.Source == PricingSourceLiteLLM && isDeepSeekModel(model) {
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
	result, _ := s.inspectDevinPricing(ctx, key, model)
	return result
}

func (s *OpenAIGatewayService) inspectDevinPricing(ctx context.Context, key *APIKey, model string) (DevinPricingAdmission, *devinPricingSnapshot) {
	result := DevinPricingAdmission{Model: model, Source: "unidentified"}
	if s == nil || key == nil || key.Group == nil || key.Group.Platform != PlatformDevin || strings.HasPrefix(strings.ToLower(model), "fusion-") {
		return result, nil
	}
	pricing, accountDependent := s.devinInspectionPricing(ctx, key, model)
	if accountDependent {
		result.Source = "account_dependent"
		return result, nil
	}
	ctx = withDevinPricing(ctx, pricing)
	result.Identified, _ = s.hasIdentifiedOpenAIResponsePricing(ctx, model, key)
	if !result.Identified {
		return result, pricing
	}
	result.Source = "identified_default"
	if resolved := s.resolveOpenAIChannelPricing(ctx, model, key); resolved != nil {
		result.Source = resolved.Source
	} else if price, ok := devinPriceFromContext(ctx, key, model); ok && price.Resolved != nil && price.Resolved.Source == PricingSourceDevinFree {
		result.Source = PricingSourceDevinFree
	}
	x := &devinFusionExecutor{service: s, key: key}
	cost, err := x.cost(ctx, model, UsageTokens{InputTokens: 1, OutputTokens: 1, CacheReadTokens: 1, CacheCreationTokens: 1}, time.Now())
	result.Calculable = err == nil && cost != nil
	if result.Calculable {
		result.ZeroEffectiveCharge = cost.ActualCost == 0 && cost.TotalCost == 0
	}
	return result, pricing
}

// Discovery has no selected physical account. Only quote a common price across
// eligible accounts; one account's promotion must not authorize a paid sibling.
func (s *OpenAIGatewayService) devinInspectionPricing(ctx context.Context, key *APIKey, model string) (*devinPricingSnapshot, bool) {
	base := s.captureDevinPricing(ctx, nil, key, nil, time.Time{}, model)
	if base == nil || s.devin == nil || s.accountRepo == nil {
		return base, false
	}
	if p := base.Prices[model].Resolved; p != nil && (p.Source == PricingSourceGroup || p.Source == PricingSourceChannel) {
		return base, false
	}
	accounts, err := s.devinFusionAccounts(ctx, key.GroupID)
	if err != nil {
		return base, false
	}
	var common *devinPricingSnapshot
	var previous []byte
	for i := range accounts {
		a := &accounts[i]
		if a.Platform != PlatformDevin || !a.IsSchedulable() || a.GetMappedModel(model) != model || !a.supportsDevinModel(model) {
			continue
		}
		// Scheduler candidates are metadata projections, without the full proxy
		// and credential fields required to read this account's live catalog.
		full, err := s.accountRepo.GetByID(ctx, a.ID)
		if err != nil || full == nil || full.ID != a.ID || full.Platform != PlatformDevin {
			return nil, true
		}
		a = full
		models, until, _ := s.devin.pricingModels(ctx, a)
		p := s.captureDevinPricing(ctx, a, key, models, until, model)
		if p == nil {
			return nil, true
		}
		card, _ := json.Marshal(p.Prices[model])
		if common != nil && string(card) != string(previous) {
			return nil, true
		}
		common, previous = p, card
	}
	if common != nil {
		return common, false
	}
	return base, false
}
