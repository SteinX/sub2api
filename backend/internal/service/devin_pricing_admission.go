package service

import (
	"context"
	"strings"
	"time"
)

// DevinPricingAdmission exposes only the calling key's identified price status,
// not rates, account/group/channel IDs, or provider credentials. It performs the
// same physical-member price checks as Fusion without reserving or generating.
type DevinPricingAdmission struct {
	Model               string `json:"model"`
	Identified          bool   `json:"identified"`
	Calculable          bool   `json:"calculable"`
	Source              string `json:"source"`
	ZeroEffectiveCharge bool   `json:"zero_effective_charge"`
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
