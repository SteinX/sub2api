package devin

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

// ModelFreeUntil accepts only the account catalog's explicit FREE tier. A zero
// credit multiplier, a label or an active promotion alone says nothing about
// token prices. BYOK does not establish free supplier inference either.
// catalogUntil bounds evidence even when the provider gives no promotion end.
func ModelFreeUntil(m *pb.ClientModelConfig, now, catalogUntil time.Time) (time.Time, bool) {
	if m == nil || m.GetDisabled() || ModelID(m) == "" || m.GetModelCostTier() != pb.ModelCostTier_MODEL_COST_TIER_FREE || m.GetPricingType() == pb.ModelPricingType_MODEL_PRICING_TYPE_BYOK || !now.Before(catalogUntil) {
		return time.Time{}, false
	}
	until := catalogUntil
	if promo := m.GetPromoStatus(); promo != nil {
		// An empty status carries no promotion claim; a permanent FREE model
		// may still have that default message in its account catalog.
		if !promo.IsActive && (promo.EndDate != nil || promo.Label != "") {
			return time.Time{}, false
		}
		if end := promo.GetEndDate(); end != nil {
			if end.CheckValid() != nil || !now.Before(end.AsTime()) {
				return time.Time{}, false
			}
			if end.AsTime().Before(until) {
				until = end.AsTime()
			}
		}
	}
	return until, true
}
