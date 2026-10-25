//go:build unit

package service

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDevinFusionIdentifiedClaude55FallbackTiers(t *testing.T) {
	for _, catalog := range []struct {
		name string
		data *PricingService
	}{
		{name: "no catalog"},
		{name: "empty catalog", data: &PricingService{pricingData: map[string]*LiteLLMModelPricing{}}},
	} {
		t.Run(catalog.name, func(t *testing.T) {
			billing := NewBillingService(&config.Config{}, catalog.data)
			tokens := UsageTokens{InputTokens: 1000, OutputTokens: 100, CacheCreationTokens: 20, CacheReadTokens: 10}
			for _, base := range []string{"claude-opus-5-5", "claude-sonnet-5-5"} {
				for _, tier := range []string{"low", "medium", "high"} {
					uid := base + "-" + tier
					require.True(t, billing.HasIdentifiedTokenPricing(uid), "known effort tiers must use the identified base card: %s", uid)
					baseCost, err := billing.CalculateCost(base, tokens, 1)
					require.NoError(t, err)
					tierCost, err := billing.CalculateCost(uid, tokens, 1)
					require.NoError(t, err)
					require.InDelta(t, baseCost.TotalCost, tierCost.TotalCost, 1e-12, "standard token charges must use the same base card")
				}
			}
			for _, unknown := range []string{"swe-2-high", "claude-opus-5-5-unknown", "made-up-opus-5-5-medium", "claude-sonnet-5-5-medium-extra"} {
				require.False(t, billing.HasIdentifiedTokenPricing(unknown), "unconfigured UIDs and family guesses must stay blocked: %s", unknown)
			}
		})
	}
}

func TestDevinContextQuoteRejectsNonMonotonicPriceIntervals(t *testing.T) {
	const model = "e2e-tiered-price"
	for _, mode := range []BillingMode{BillingModePerRequest, BillingModeToken} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			billing := NewBillingService(cfg, nil)
			maxShort := 1000
			card := ChannelModelPricing{Platform: PlatformDevin, Models: []string{model}, BillingMode: mode, InputPrice: testPtrFloat64(.001), OutputPrice: testPtrFloat64(.001), PerRequestPrice: testPtrFloat64(.5), Intervals: []PricingInterval{
				{MinTokens: 0, MaxTokens: &maxShort, PerRequestPrice: testPtrFloat64(.5), InputPrice: testPtrFloat64(.001)},
				{MinTokens: 1001, PerRequestPrice: testPtrFloat64(.01), InputPrice: testPtrFloat64(.000000001)},
			}}
			group := &Group{ID: 1, Platform: PlatformDevin, RateMultiplier: 1, LongContextPricingEnabled: true}
			channel := &ChannelService{}
			if mode == BillingModePerRequest {
				group.ModelPricing = []ChannelModelPricing{card}
			} else {
				channel.cache.Store(populateChannelCache([]Channel{{ID: 7, Status: StatusActive, GroupIDs: []int64{group.ID}, ModelPricing: []ChannelModelPricing{card}}}, map[int64]string{group.ID: PlatformDevin}))
			}
			bills := &fusionBillingRepo{}
			svc := &OpenAIGatewayService{cfg: cfg, billingService: billing, resolver: NewModelPricingResolver(channel, billing), usageBillingRepo: bills}
			key := &APIKey{ID: 2, GroupID: &group.ID, Group: group, User: &User{ID: 3, Balance: 10}}
			price := svc.InspectDevinContextPrice(context.Background(), key, model, 32768, 1024)
			require.True(t, price.Identified)
			require.True(t, price.Calculable)
			require.Nil(t, price.ContextCostUpperUSD, "a low long-context tier must not authorize a more expensive short request")
			require.Equal(t, "non_flat_token_pricing", price.CostBoundUnavailable)
			if mode == BillingModePerRequest {
				x := &devinFusionExecutor{service: svc, key: key}
				short, err := x.cost(context.Background(), model, UsageTokens{InputTokens: 1, OutputTokens: 1}, time.Now())
				require.NoError(t, err)
				long, err := x.cost(context.Background(), model, UsageTokens{InputTokens: 32768, OutputTokens: 1024}, time.Now())
				require.NoError(t, err)
				require.Greater(t, short.TotalCost, long.TotalCost, "prove the non-monotonic configuration is actually active before testing its refusal")
			}
			require.Empty(t, bills.commands)
		})
	}
}

func TestDevinFusionMemberPricingUsesPhysicalUIDAndGroupScope(t *testing.T) {
	const lead = "claude-opus-5-5-medium"
	const sidekick = "swe-2-high"
	const public = "fusion-" + lead + "-sidekick-" + sidekick
	// Synthetic test rates exercise explicit pricing. They are not a published
	// SWE price card and are never installed by production or live test code.
	card := ChannelModelPricing{
		Models: []string{sidekick}, Platform: PlatformDevin, BillingMode: BillingModeToken,
		InputPrice: testPtrFloat64(2e-6), OutputPrice: testPtrFloat64(4e-6),
	}
	for _, tc := range []struct {
		name            string
		groupCard       bool
		virtualCard     bool
		channelGroup    int64
		channelPlatform string
		wantSource      string
	}{
		{name: "missing real member price"},
		{name: "virtual UID cannot price physical members", virtualCard: true},
		{name: "explicit physical group card", groupCard: true, wantSource: PricingSourceGroup},
		{name: "explicit physical channel card", channelGroup: 1, channelPlatform: PlatformDevin, wantSource: PricingSourceChannel},
		{name: "another group cannot supply the price", channelGroup: 2, channelPlatform: PlatformDevin},
		{name: "another platform cannot supply the price", channelGroup: 1, channelPlatform: PlatformOpenAI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			billing := NewBillingService(cfg, nil)
			group := &Group{ID: 1, Platform: PlatformDevin, RateMultiplier: 1}
			if tc.groupCard || tc.virtualCard {
				group.ModelPricing = []ChannelModelPricing{card.Clone()}
				if tc.virtualCard {
					group.ModelPricing[0].Models = []string{public}
				}
			}
			var channel *ChannelService
			if tc.channelGroup != 0 {
				chCard := card.Clone()
				chCard.Platform = tc.channelPlatform
				channel = &ChannelService{}
				channel.cache.Store(populateChannelCache([]Channel{{
					ID: 7, Name: "synthetic-pricing", Status: StatusActive,
					GroupIDs: []int64{tc.channelGroup}, ModelPricing: []ChannelModelPricing{chCard},
				}}, map[int64]string{tc.channelGroup: PlatformDevin}))
			}
			resolver := NewModelPricingResolver(channel, billing)
			bills := &fusionBillingRepo{}
			svc := &OpenAIGatewayService{
				cfg: cfg, billingService: billing, resolver: resolver,
				billingCacheService: &BillingCacheService{}, usageBillingRepo: bills,
			}
			key := &APIKey{ID: 2, GroupID: &group.ID, Group: group, User: &User{ID: 3, Balance: 10}}
			x := &devinFusionExecutor{service: svc, key: key, profile: fusion.Profile{LeadModel: lead, SidekickModel: sidekick}}
			out := 128
			call := fusion.Call{Request: &apicompat.ResponsesRequest{Model: lead, MaxOutputTokens: &out}, PricingAt: time.Now()}
			identified, _ := svc.hasIdentifiedOpenAIResponsePricing(context.Background(), lead, key)
			require.True(t, identified, "known Lead effort alias must use its built-in card")
			admission := svc.InspectDevinPricing(context.Background(), key, sidekick)
			require.Equal(t, tc.wantSource != "", admission.Identified)
			require.Equal(t, tc.wantSource != "", admission.Calculable)
			if tc.wantSource != "" {
				require.Equal(t, tc.wantSource, admission.Source)
				quoted := svc.InspectDevinContextPrice(context.Background(), key, sidekick, 32768, 1024)
				require.NotNil(t, quoted.ContextCostUpperUSD)
				require.GreaterOrEqual(t, *quoted.ContextCostUpperUSD, 32768*2e-6+1024*4e-6, "quote must include a full context and maximum output at the actual scoped price")
			}
			require.False(t, svc.InspectDevinPricing(context.Background(), key, public).Identified)
			estimate, err := x.estimate(context.Background(), call, false)
			if tc.wantSource == "" {
				var pricingErr *fusion.Error
				require.ErrorAs(t, err, &pricingErr)
				require.Equal(t, "devin_fusion_pricing_unavailable", pricingErr.Code)
				require.Equal(t, 400, pricingErr.Status)
				require.Contains(t, pricingErr.Message, sidekick, "the missing physical UID must be actionable")
				require.Zero(t, estimate)
				for _, protocol := range []string{"responses", "chat", "anthropic"} {
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest("POST", "/v1/"+protocol, nil)
					require.True(t, WriteDevinFusionError(c, protocol, pricingErr))
					require.Equal(t, 400, w.Code)
					require.Contains(t, w.Body.String(), sidekick)
					require.Contains(t, w.Body.String(), "devin_fusion_pricing_unavailable")
				}
			} else {
				require.NoError(t, err)
				require.Positive(t, estimate)
				resolved := resolver.Resolve(context.Background(), PricingInput{Model: sidekick, Group: group, GroupID: &group.ID})
				require.Equal(t, tc.wantSource, resolved.Source)
				cost, err := x.cost(context.Background(), sidekick, UsageTokens{InputTokens: 100, OutputTokens: 10}, call.PricingAt)
				require.NoError(t, err)
				require.InDelta(t, 100*2e-6+10*4e-6, cost.ActualCost, 1e-12)
			}
			require.Empty(t, bills.commands, "estimation must not charge usage")
		})
	}
}
