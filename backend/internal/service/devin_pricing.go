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

	// Freeze possible terminal billing identities before inference, but persist
	// only the selected and actually reported prices in the physical receipt.
	candidates         map[string]devinModelPrice
	candidateFreeUntil map[string]time.Time
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
	if !ok {
		// A snapshot miss is historical unknown evidence, never permission to
		// consult prices published after this inference.
		price = unknownDevinModelPrice()
	}
	return price, true
}

func unknownDevinModelPrice() devinModelPrice {
	return devinModelPrice{Resolved: &ResolvedPricing{Mode: BillingModeToken}}
}

func (p *devinPricingSnapshot) pinModel(uid string) {
	uid = strings.TrimSpace(uid)
	if p == nil || uid == "" {
		return
	}
	if _, exists := p.Prices[uid]; exists {
		return
	}
	price, known := p.candidates[uid]
	if !known {
		price = unknownDevinModelPrice()
	}
	p.Prices[uid] = price
	if until := p.candidateFreeUntil[uid]; !until.IsZero() && (p.ValidUntil.IsZero() || until.Before(p.ValidUntil)) {
		p.ValidUntil = until
	}
}

// Candidate promotions may expire while routing/authentication is in progress.
// Check them at the send boundary; expiry during generation must not reprice
// that already-started inference or its later durable settlement.
func (p *devinPricingSnapshot) readyForInference(now time.Time) bool {
	if p == nil {
		return true
	}
	if !p.ValidUntil.IsZero() && !now.Before(p.ValidUntil) {
		return false
	}
	for uid, until := range p.candidateFreeUntil {
		if !now.Before(until) {
			p.candidates[uid] = unknownDevinModelPrice()
			delete(p.candidateFreeUntil, uid)
		}
	}
	return true
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
	candidateUIDs := append([]string(nil), uids...)
	for _, model := range models {
		candidateUIDs = append(candidateUIDs, devin.ModelID(model))
	}
	freeUntil := map[string]time.Time{}
	for _, uid := range candidateUIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if _, exists := p.Prices[uid]; exists {
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
			freeUntil[uid] = until
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
	frozen.candidates, frozen.candidateFreeUntil = frozen.Prices, freeUntil
	frozen.Prices = map[string]devinModelPrice{}
	for _, uid := range uids {
		frozen.pinModel(uid)
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
