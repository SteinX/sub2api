package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

const PricingSourceDevinFree = "devin_catalog_free"

// A price belongs to one physical account and billing group, never to a global
// UID. Persist terminal attempts with this snapshot so delayed settlement does
// not reprice them after a promotion or configuration change.
type devinPricingSnapshot struct {
	Version    int                        `json:"version"`
	AccountID  int64                      `json:"account_id"`
	GroupID    int64                      `json:"group_id"`
	ValidUntil time.Time                  `json:"valid_until"`
	Prices     map[string]devinModelPrice `json:"prices"`
}

type devinModelPrice struct {
	Identified  bool                 `json:"identified"`
	Resolved    *ResolvedPricing     `json:"resolved"`
	Configured  *ChannelModelPricing `json:"configured,omitempty"`
	LongContext bool                 `json:"long_context"`
}

func (p devinModelPrice) resolved() *ResolvedPricing {
	if p.Resolved == nil {
		return nil
	}
	r := *p.Resolved
	r.channelPricing = p.Configured
	r.longContextPricingEnabled = p.LongContext
	return &r
}

type devinPricingContextKey struct{}

func withDevinPricing(ctx context.Context, p *devinPricingSnapshot) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, devinPricingContextKey{}, p)
}

func devinPriceFromContext(ctx context.Context, key *APIKey, model string) (devinModelPrice, bool) {
	p, _ := ctx.Value(devinPricingContextKey{}).(*devinPricingSnapshot)
	if p == nil || p.GroupID != devinPricingGroupID(key) {
		return devinModelPrice{}, false
	}
	price, ok := p.Prices[strings.TrimSpace(model)]
	return price, ok
}

func devinPricingGroupID(key *APIKey) int64 {
	if key != nil && key.Group != nil {
		return key.Group.ID
	}
	return 0
}

// pricingModels uses only the existing live credential-scoped cache. Persisted
// scheduling catalogs are deliberately not pricing evidence.
func (s *DevinService) pricingModels(ctx context.Context, a *Account) ([]*pb.ClientModelConfig, time.Time, error) {
	catalog, err := s.modelCatalog(ctx, a)
	return catalog.models, catalog.validUntil, err
}

func (s *OpenAIGatewayService) captureDevinPricing(ctx context.Context, a *Account, key *APIKey, models []*pb.ClientModelConfig, catalogUntil time.Time, uids ...string) *devinPricingSnapshot {
	p := &devinPricingSnapshot{Version: 1, GroupID: devinPricingGroupID(key), Prices: map[string]devinModelPrice{}}
	if a != nil {
		p.AccountID = a.ID
	}
	for _, uid := range uids {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		var resolved *ResolvedPricing
		if s.resolver != nil {
			input := PricingInput{Model: uid}
			if key != nil {
				input.Group, input.GroupID = key.Group, key.GroupID
			}
			resolved = s.resolver.Resolve(ctx, input)
		} else if s.billingService != nil {
			resolved = NewModelPricingResolver(nil, s.billingService).Resolve(ctx, PricingInput{Model: uid})
		}
		configured := resolved != nil && (resolved.Source == PricingSourceGroup || resolved.Source == PricingSourceChannel)
		identified := configured || s.billingService != nil && s.billingService.HasIdentifiedTokenPricing(uid)
		// Explicit sale prices retain their precedence. Account FREE evidence
		// overrides only the generic price catalog/fallback, including paid ones.
		if until, free := devin.ModelFreeUntil(devin.FindModel(models, uid), time.Now(), catalogUntil); !configured && free && a != nil && a.Platform == PlatformDevin {
			resolved = &ResolvedPricing{Mode: BillingModeToken, Source: PricingSourceDevinFree, BasePricing: &ModelPricing{CacheCreationPriceExplicit: true, ImageOutputPriceExplicit: true, SupportsCacheBreakdown: true}, SupportsCacheBreakdown: true}
			identified = true
			if p.ValidUntil.IsZero() || until.Before(p.ValidUntil) {
				p.ValidUntil = until
			}
		}
		price := devinModelPrice{Identified: identified, Resolved: resolved}
		if resolved != nil {
			price.Configured, price.LongContext = resolved.channelPricing, resolved.longContextPricingEnabled
		}
		p.Prices[uid] = price
	}
	// Detach nested rate/configuration maps from mutable service state. The
	// same serializable representation is carried by durable physical attempts.
	b, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	var frozen devinPricingSnapshot
	if json.Unmarshal(b, &frozen) != nil {
		return nil
	}
	return &frozen
}

func (p *devinPricingSnapshot) marshal() json.RawMessage {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	return b
}

func (p *devinPricingSnapshot) expired() bool {
	return p != nil && !p.ValidUntil.IsZero() && !time.Now().Before(p.ValidUntil)
}

func restoreDevinPricing(raw json.RawMessage, account *Account, key *APIKey) (*devinPricingSnapshot, error) {
	var p devinPricingSnapshot
	if json.Unmarshal(raw, &p) != nil || p.Version != 1 || account == nil || p.AccountID != account.ID || p.GroupID != devinPricingGroupID(key) || len(p.Prices) == 0 {
		return nil, errors.New("physical price snapshot is unavailable or belongs to another billing scope")
	}
	return &p, nil
}
