//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const futureFreeUID = "future-promoted-v99"

type catalogPricingFixture struct {
	service     *OpenAIGatewayService
	account     *Account
	key         *APIKey
	models      []*pb.ClientModelConfig
	modelsFor   map[string][]*pb.ClientModelConfig
	modelCalls  int
	chatCalls   int
	failCatalog bool
	chat        func(*pb.GetChatMessageRequest) *pb.GetChatMessageResponse
	logs        *openAIRecordUsageLogRepoStub
	bills       *fusionBillingRepo
}

func newCatalogPricingFixture(t *testing.T) *catalogPricingFixture {
	t.Helper()
	// This binary is generated from a separate subset of upstream's full
	// schema, so field-number/type mistakes in cascade.proto fail end to end.
	wire, err := os.ReadFile("../pkg/devin/testdata/model_pricing/models.bin")
	require.NoError(t, err)
	var catalog pb.GetCliModelConfigsResponse
	require.NoError(t, proto.Unmarshal(wire, &catalog))
	originalCatalog := proto.Clone(&catalog)
	f := &catalogPricingFixture{models: catalog.ClientModelConfigs, modelsFor: map[string][]*pb.ClientModelConfig{}, logs: &openAIRecordUsageLogRepoStub{inserted: true}, bills: &fusionBillingRepo{}}
	f.account = &Account{ID: 7, Platform: PlatformDevin, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 3, Credentials: map[string]any{"access_token": "synthetic-free-account"}, Extra: devinCatalogExtra(f.models, "fixture")}
	group := &Group{ID: 1, Platform: PlatformDevin, RateMultiplier: 1, Status: StatusActive}
	f.key = &APIKey{ID: 22, UserID: 33, GroupID: &group.ID, Group: group, User: &User{ID: 33, Balance: 100, Concurrency: 3, Status: StatusActive}}
	s := newOpenAIRecordUsageServiceWithBillingRepoForTest(f.logs, f.bills, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	cfg.Billing.InflightReservation.Enabled = true
	s.cfg = cfg
	s.resolver = NewModelPricingResolver(nil, s.billingService)
	s.accountRepo = &fusionAccountRepo{account: f.account}
	s.billingCacheService = NewBillingCacheService(&fusionTestBillingCache{newMemInflightCache(100)}, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(s.billingCacheService.Stop)
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	upstream := &devinHTTP{handle: func(r *http.Request) (*http.Response, error) {
		var message proto.Message
		switch r.URL.Path {
		case devin.ModelsPath:
			f.modelCalls++
			if f.failCatalog {
				return nil, errors.New("synthetic discovery unavailable")
			}
			models := f.models
			if token, _, ok := r.BasicAuth(); ok && f.modelsFor[token] != nil {
				models = f.modelsFor[token]
			}
			// Use the independent wire unchanged for the initial catalog.
			if f.modelCalls == 1 && proto.Equal(originalCatalog, &pb.GetCliModelConfigsResponse{ClientModelConfigs: models}) {
				return devinReply(200, wire), nil
			}
			message = &pb.GetCliModelConfigsResponse{ClientModelConfigs: models}
		case devin.UserJWTPath:
			message = &pb.GetUserJwtResponse{UserJwt: "synthetic-user-jwt"}
		case devin.ChatPath:
			f.chatCalls++
			_, b, readErr := devin.ReadFrame(r.Body)
			require.NoError(t, readErr)
			var request pb.GetChatMessageRequest
			require.NoError(t, proto.Unmarshal(b, &request))
			reply := &pb.GetChatMessageResponse{MessageId: fmt.Sprint(f.chatCalls), DeltaText: "done", StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN, ActualModelUid: proto.String(request.ChatModelUid), Usage: &pb.ModelUsageStats{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 40, CacheWriteTokens: 10, BillingModelUid: request.ChatModelUid}}
			if f.chat != nil {
				custom := f.chat(&request)
				custom.MessageId, custom.Usage, custom.ActualModelUid, custom.StopReason = reply.MessageId, reply.Usage, reply.ActualModelUid, reply.StopReason
				reply = custom
			}
			b, marshalErr := proto.Marshal(reply)
			require.NoError(t, marshalErr)
			return devinReply(200, append(devinTestFrame(0, b), devinTestFrame(2, []byte(`{}`))...)), nil
		default:
			return nil, fmt.Errorf("unexpected mock path %s", r.URL.Path)
		}
		b, marshalErr := proto.Marshal(message)
		return devinReply(200, b), marshalErr
	}}
	s.devin = NewDevinService(s.accountRepo, nil, upstream, cfg, devin.NewRedisSessionStore(rdb))
	f.service = s
	return f
}

func (f *catalogPricingFixture) expireCatalog() {
	f.service.devin.mu.Lock()
	defer f.service.devin.mu.Unlock()
	for k, e := range f.service.devin.runtime {
		e.modelsUntil = time.Now().Add(-time.Second)
		f.service.devin.runtime[k] = e
	}
}

func (f *catalogPricingFixture) executor() *devinFusionExecutor {
	p := fusionTestProfile()
	p.SidekickModel = futureFreeUID
	return &devinFusionExecutor{service: f.service, account: f.account, key: f.key, profile: p, models: f.models, publicModel: p.PublicModel, reservations: map[string]*InflightReservation{}}
}

func TestDevinCatalogPriceAdmissionAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*pb.ClientModelConfig)
		free bool
	}{
		{"new UID explicit free", func(*pb.ClientModelConfig) {}, true},
		{"permanent free", func(m *pb.ClientModelConfig) { m.PromoStatus = nil }, true},
		{"free without end still expires with cache", func(m *pb.ClientModelConfig) { m.PromoStatus.EndDate = nil }, true},
		{"discount promotion", func(m *pb.ClientModelConfig) { m.ModelCostTier = pb.ModelCostTier_MODEL_COST_TIER_LOW }, false},
		{"missing fields and scalar zero", func(m *pb.ClientModelConfig) {
			m.ModelCostTier = 0
			m.PricingType = 0
			m.PromoStatus = nil
			m.CreditMultiplier = 0
		}, false},
		{"misleading label", func(m *pb.ClientModelConfig) { m.ModelCostTier = 0; m.Label = "FREE promotion" }, false},
		{"ended promotion", func(m *pb.ClientModelConfig) { m.PromoStatus.EndDate = timestamppb.New(time.Now().Add(-time.Second)) }, false},
		{"inactive promotion", func(m *pb.ClientModelConfig) { m.PromoStatus.IsActive = false }, false},
		{"invalid timestamp", func(m *pb.ClientModelConfig) { m.PromoStatus.EndDate = &timestamppb.Timestamp{Seconds: 253402300800} }, false},
		{"BYOK is not free inference", func(m *pb.ClientModelConfig) { m.PricingType = pb.ModelPricingType_MODEL_PRICING_TYPE_BYOK }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCatalogPricingFixture(t)
			tc.edit(f.models[1])
			quote := f.service.InspectDevinContextPrice(context.Background(), f.key, futureFreeUID, 32768, 1024)
			require.False(t, f.service.billingService.HasIdentifiedTokenPricing(futureFreeUID), "an account promotion must not install a global price")
			require.Equal(t, tc.free, quote.Identified)
			require.Equal(t, tc.free, quote.Calculable)
			require.Equal(t, tc.free, quote.ZeroEffectiveCharge)
			if tc.free {
				require.Equal(t, PricingSourceDevinFree, quote.Source)
				require.NotNil(t, quote.ContextCostUpperUSD)
				require.Zero(t, *quote.ContextCostUpperUSD)
			}
			out := 128
			x := f.executor()
			estimate, err := x.Estimate(context.Background(), fusion.Call{AttemptID: "test", Request: &apicompat.ResponsesRequest{Model: x.profile.LeadModel, Input: json.RawMessage(`"hello"`), MaxOutputTokens: &out}, PricingAt: time.Now()})
			if tc.free {
				require.NoError(t, err)
				require.Positive(t, estimate)
			} else {
				require.ErrorContains(t, err, futureFreeUID)
			}
			require.Equal(t, 1, f.modelCalls, "admission and inspection must reuse the credential-scoped two-minute cache")
			require.Zero(t, f.chatCalls)
			require.Empty(t, f.bills.commands)
		})
	}
	t.Run("expired evidence cannot survive failed refresh", func(t *testing.T) {
		f := newCatalogPricingFixture(t)
		require.True(t, f.service.InspectDevinPricing(context.Background(), f.key, futureFreeUID).Identified)
		f.expireCatalog()
		f.failCatalog = true
		quote := f.service.InspectDevinPricing(context.Background(), f.key, futureFreeUID)
		require.False(t, quote.Identified)
		x := f.executor()
		_, err := x.cost(context.Background(), futureFreeUID, UsageTokens{InputTokens: 1}, time.Now())
		require.ErrorContains(t, err, futureFreeUID)
		require.Zero(t, f.chatCalls)
	})
	t.Run("promotion end shortens cache lifetime", func(t *testing.T) {
		f := newCatalogPricingFixture(t)
		end := time.Now().Add(30 * time.Second)
		f.models[1].PromoStatus.EndDate = timestamppb.New(end)
		require.True(t, f.service.InspectDevinPricing(context.Background(), f.key, futureFreeUID).Identified)
		f.service.devin.mu.Lock()
		entry := f.service.devin.runtime[f.service.devin.runtimeKey(context.Background(), f.account)]
		f.service.devin.mu.Unlock()
		require.True(t, end.Equal(entry.modelsUntil))
		f.models[1].PromoStatus.EndDate = timestamppb.New(time.Now().Add(-time.Second))
		f.expireCatalog()
		require.False(t, f.service.InspectDevinPricing(context.Background(), f.key, futureFreeUID).Identified)
	})
}

func TestDevinCatalogPriceOrdinaryForwardUsesSnapshot(t *testing.T) {
	for _, override := range []string{"catalog", "group", "channel"} {
		t.Run(override, func(t *testing.T) {
			f := newCatalogPricingFixture(t)
			card := ChannelModelPricing{Models: []string{futureFreeUID}, Platform: PlatformDevin, BillingMode: BillingModeToken, InputPrice: testPtrFloat64(2e-6), OutputPrice: testPtrFloat64(4e-6), CacheReadPrice: testPtrFloat64(1e-6), CacheWritePrice: testPtrFloat64(3e-6)}
			switch override {
			case "catalog":
				ch := &ChannelService{}
				ch.cache.Store(populateChannelCache([]Channel{{ID: 4, Status: StatusActive, GroupIDs: []int64{f.key.Group.ID}}}, map[int64]string{f.key.Group.ID: PlatformDevin}))
				f.service.channelService = ch
			case "group":
				f.key.Group.ModelPricing = []ChannelModelPricing{card}
			case "channel":
				ch := &ChannelService{}
				ch.cache.Store(populateChannelCache([]Channel{{ID: 4, Status: StatusActive, GroupIDs: []int64{f.key.Group.ID}, ModelPricing: []ChannelModelPricing{card}}}, map[int64]string{f.key.Group.ID: PlatformDevin}))
				f.service.resolver = NewModelPricingResolver(ch, f.service.billingService)
			}
			// Even an identified generic paid card is superseded by account FREE.
			f.service.billingService.fallbackPrices[futureFreeUID] = &ModelPricing{InputPricePerToken: .5, OutputPricePerToken: .5}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			c.Set("api_key", f.key)
			body := []byte(`{"model":"` + futureFreeUID + `","input":"hello","max_output_tokens":128}`)
			result, err := f.service.forwardDevin(c.Request.Context(), c, f.account, body, "responses", "test", "")
			require.NoError(t, err)
			require.Equal(t, 200, w.Code)
			require.Equal(t, futureFreeUID, result.BillingModel)
			require.NotNil(t, result.devinPricing)
			// Simulate delayed async usage after both catalog and admin edits.
			f.key.Group.ModelPricing = nil
			f.service.resolver = NewModelPricingResolver(nil, f.service.billingService)
			f.models[1].ModelCostTier = pb.ModelCostTier_MODEL_COST_TIER_HIGH
			f.expireCatalog()
			f.failCatalog = true
			require.NoError(t, f.service.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: result, APIKey: f.key, User: f.key.User, Account: f.account}))
			require.NotNil(t, f.logs.lastLog)
			if override == "catalog" {
				require.Zero(t, f.logs.lastLog.TotalCost)
				require.Zero(t, f.logs.lastLog.ActualCost)
				require.NotNil(t, f.logs.lastLog.AccountStatsCost)
				require.Zero(t, *f.logs.lastLog.AccountStatsCost)
			} else {
				want := 100*2e-6 + 20*4e-6 + 40*1e-6 + 10*3e-6
				require.InDelta(t, want, f.logs.lastLog.TotalCost, 1e-12)
			}
			require.Equal(t, 1, f.modelCalls, "settlement must never refetch the catalog")
		})
	}
}

func TestDevinCatalogPriceOrdinaryMissingPriceDoesNotReprice(t *testing.T) {
	f := newCatalogPricingFixture(t)
	f.models[1].ModelCostTier = pb.ModelCostTier_MODEL_COST_TIER_UNSPECIFIED
	f.models[1].PromoStatus = nil
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set("api_key", f.key)
	body := []byte(`{"model":"` + futureFreeUID + `","input":"hello","max_output_tokens":128}`)
	result, err := f.service.forwardDevin(c.Request.Context(), c, f.account, body, "responses", "test", "")
	require.NoError(t, err)
	require.NotNil(t, result.devinPricing)
	require.False(t, result.devinPricing.Prices[futureFreeUID].Identified)
	require.Nil(t, result.devinPricing.Prices[futureFreeUID].Resolved.BasePricing)
	// The ordinary missing-price path remains a zero-cost record. A card
	// published after inference must not silently add a retrospective charge.
	f.service.billingService.fallbackPrices[futureFreeUID] = &ModelPricing{InputPricePerToken: .5, OutputPricePerToken: .5}
	require.NoError(t, f.service.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: result, APIKey: f.key, User: f.key.User, Account: f.account}))
	require.Zero(t, f.logs.lastLog.TotalCost)
	require.Zero(t, f.logs.lastLog.ActualCost)
	// Missing remains distinct from explicitly identified catalog FREE.
	require.False(t, result.devinPricing.Prices[futureFreeUID].Identified)
}

func TestDevinCatalogPriceFusionFullFlowAndRecovery(t *testing.T) {
	f := newCatalogPricingFixture(t)
	x := f.executor()
	f.account.Extra[fusion.ExtraProfiles] = []fusion.Profile{x.profile}
	f.chat = func(wire *pb.GetChatMessageRequest) *pb.GetChatMessageResponse {
		switch f.chatCalls {
		case 1:
			require.Equal(t, x.profile.LeadModel, wire.ChatModelUid)
			return &pb.GetChatMessageResponse{DeltaToolCalls: []*pb.ChatToolCall{{Id: "delegate", Name: fusion.DelegateTool, ArgumentsJson: `{"task":"inspect","acceptance_criteria":"report the result"}`}}}
		case 2:
			require.Equal(t, futureFreeUID, wire.ChatModelUid)
			return &pb.GetChatMessageResponse{DeltaText: "inspection complete"}
		default:
			return &pb.GetChatMessageResponse{DeltaText: "complete"}
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set("api_key", f.key)
	body := []byte(`{"model":"fusion-team","input":"hello","max_output_tokens":128}`)
	_, err := f.service.BindDevinFusionRequest(c, f.key, "fusion-team", body, "responses", nil)
	require.NoError(t, err)
	_, err = f.service.forwardDevin(c.Request.Context(), c, f.account, body, "responses", "", "")
	require.NoError(t, err)
	require.Equal(t, 3, f.chatCalls)
	engine := f.service.devin.fusionEngine()
	sess, _, err := engine.Load(context.Background(), fusion.Scope(f.key.UserID, f.key.ID), w.Header().Get(fusion.HeaderSession))
	require.NoError(t, err)
	require.Len(t, sess.Attempts, 3)
	a := sess.Attempts[1]
	require.Equal(t, futureFreeUID, a.Model)
	require.True(t, a.Settled)
	require.Zero(t, a.CostUSD)
	require.NotEmpty(t, a.PricingSnapshot)
	// Recover a terminal receipt in a new executor after the promotion ended.
	f.models[1].PromoStatus.EndDate = timestamppb.New(time.Now().Add(-time.Second))
	f.expireCatalog()
	f.failCatalog = true
	recovered := f.executor()
	cost, err := recovered.Settle(context.Background(), a)
	require.NoError(t, err)
	require.Zero(t, cost)
	require.Equal(t, 3, f.chatCalls)
	require.Len(t, f.bills.commands, 3, "recovery must retain the idempotent billing attempt ID")
	require.Equal(t, 1, f.modelCalls, "receipt recovery must not fetch current account prices")
	legacy := a
	legacy.PricingSnapshot = nil
	f.failCatalog = false
	f.models[1].PromoStatus.EndDate = timestamppb.New(time.Unix(4102444800, 0))
	_, err = recovered.Settle(context.Background(), legacy)
	require.ErrorContains(t, err, futureFreeUID, "a legacy receipt cannot borrow a current promotion as historical price evidence")
	other := *f.account
	other.ID++
	recovered.account = &other
	_, err = recovered.Settle(context.Background(), a)
	require.ErrorContains(t, err, "another billing scope")
}

func TestDevinCatalogPriceCredentialIsolation(t *testing.T) {
	f := newCatalogPricingFixture(t)
	ctx := context.Background()
	paid := *f.account
	paid.ID++
	paid.Credentials = map[string]any{"access_token": "synthetic-paid-account"}
	paidModels := devin.CloneModels(f.models)
	paidModels[1].ModelCostTier = pb.ModelCostTier_MODEL_COST_TIER_LOW
	f.modelsFor[paid.GetCredential("access_token")] = paidModels
	free := f.executor()
	_, err := free.cost(ctx, futureFreeUID, UsageTokens{InputTokens: 100}, time.Now())
	require.NoError(t, err)
	paidExecutor := f.executor()
	paidExecutor.account = &paid
	_, err = paidExecutor.cost(ctx, futureFreeUID, UsageTokens{InputTokens: 100}, time.Now())
	require.ErrorContains(t, err, futureFreeUID)
	require.Equal(t, 2, f.modelCalls)
	// Rotating credentials on the same account also invalidates free evidence.
	f.account.Credentials = paid.Credentials
	_, err = f.executor().cost(ctx, futureFreeUID, UsageTokens{InputTokens: 100}, time.Now())
	require.ErrorContains(t, err, futureFreeUID)
	require.Equal(t, 3, f.modelCalls)
}

func TestDevinCatalogPriceFusionUsesSelectedAccount(t *testing.T) {
	f := newCatalogPricingFixture(t)
	require.True(t, f.service.InspectDevinPricing(context.Background(), f.key, futureFreeUID).Identified)
	paid := *f.account
	paid.ID++
	paid.Credentials = map[string]any{"access_token": "synthetic-selected-paid-account"}
	paidModels := devin.CloneModels(f.models)
	paidModels[1].ModelCostTier = pb.ModelCostTier_MODEL_COST_TIER_LOW
	f.modelsFor[paid.GetCredential("access_token")] = paidModels
	f.account = &paid
	f.service.accountRepo = &fusionAccountRepo{account: f.account}
	f.account.Extra[fusion.ExtraProfiles] = []fusion.Profile{f.executor().profile}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set("api_key", f.key)
	body := []byte(`{"model":"fusion-team","input":"hello","max_output_tokens":128}`)
	_, err := f.service.BindDevinFusionRequest(c, f.key, "fusion-team", body, "responses", nil)
	require.NoError(t, err)
	_, err = f.service.forwardDevin(c.Request.Context(), c, f.account, body, "responses", "", "")
	require.ErrorContains(t, err, futureFreeUID)
	require.Equal(t, 400, w.Code)
	require.Contains(t, w.Body.String(), "devin_fusion_pricing_unavailable")
	require.Zero(t, f.chatCalls, "a free sibling must not authorize the selected paid account")
}

func TestDevinCatalogPriceExpiresBeforeSend(t *testing.T) {
	f := newCatalogPricingFixture(t)
	x := f.executor()
	out := 128
	call := fusion.Call{AttemptID: "expires-before-send", Request: &apicompat.ResponsesRequest{Model: futureFreeUID, Input: json.RawMessage(`"hello"`), MaxOutputTokens: &out}, PricingAt: time.Now()}
	var err error
	call.EstimateUSD, err = x.Estimate(context.Background(), call)
	require.NoError(t, err)
	x.pricing.ValidUntil = time.Now().Add(-time.Second)
	result, err := x.Execute(context.Background(), call)
	require.ErrorContains(t, err, "expired before inference")
	require.True(t, result.NotStarted)
	require.True(t, result.KnownFailure)
	require.Zero(t, f.chatCalls)
}

type catalogPricingAccounts struct {
	AccountRepository
	accounts []Account
}

func (r *catalogPricingAccounts) ListSchedulableByGroupID(context.Context, int64) ([]Account, error) {
	return r.accounts, nil
}

func TestDevinCatalogPriceInspectionAcrossAccounts(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_effective_price=%t", same), func(t *testing.T) {
			f := newCatalogPricingFixture(t)
			other := *f.account
			other.ID++
			other.Credentials = map[string]any{"access_token": "synthetic-other-account"}
			models := devin.CloneModels(f.models)
			if !same {
				models[1].ModelCostTier = pb.ModelCostTier_MODEL_COST_TIER_LOW
			}
			f.modelsFor[other.GetCredential("access_token")] = models
			f.service.accountRepo = &catalogPricingAccounts{accounts: []Account{*f.account, other}}
			quote := f.service.InspectDevinContextPrice(context.Background(), f.key, futureFreeUID, 32768, 1024)
			require.Equal(t, same, quote.Identified)
			require.Equal(t, same, quote.ZeroEffectiveCharge)
			if same {
				require.Equal(t, PricingSourceDevinFree, quote.Source)
				require.NotNil(t, quote.ContextCostUpperUSD)
			} else {
				require.Equal(t, "account_dependent", quote.Source)
				require.Nil(t, quote.ContextCostUpperUSD)
			}
			require.Zero(t, f.chatCalls)
			require.Empty(t, f.bills.commands)
		})
	}
}

func (f *catalogPricingFixture) toolTurnBody(callID string) []byte {
	input := any("hello")
	if callID != "" {
		input = []any{map[string]any{"type": "function_call_output", "call_id": callID, "output": "verified"}}
	}
	b, _ := json.Marshal(map[string]any{"model": "fusion-team", "input": input, "max_output_tokens": 128, "tools": []any{map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object"}}}})
	return b
}

func (f *catalogPricingFixture) fusionRequest(body []byte, session string) (*httptest.ResponseRecorder, error) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set(fusion.HeaderSession, session)
	c.Set("api_key", f.key)
	_, err := f.service.BindDevinFusionRequest(c, f.key, "fusion-team", body, "responses", nil)
	if err == nil {
		_, err = f.service.forwardDevin(c.Request.Context(), c, f.account, body, "responses", "", "")
	}
	return w, err
}

func (f *catalogPricingFixture) scriptedToolTurn(sideEffect func()) {
	f.account.Extra[fusion.ExtraProfiles] = []fusion.Profile{f.executor().profile}
	f.chat = func(wire *pb.GetChatMessageRequest) *pb.GetChatMessageResponse {
		switch f.chatCalls {
		case 1:
			return &pb.GetChatMessageResponse{DeltaToolCalls: []*pb.ChatToolCall{{Id: "delegate", Name: fusion.DelegateTool, ArgumentsJson: `{"task":"inspect","acceptance_criteria":"verify"}`}}}
		case 2:
			if sideEffect != nil {
				sideEffect()
			}
			return &pb.GetChatMessageResponse{DeltaToolCalls: []*pb.ChatToolCall{{Id: "private-read", Name: "read_file", ArgumentsJson: `{}`}}}
		default:
			return &pb.GetChatMessageResponse{DeltaText: "complete"}
		}
	}
}

func TestDevinCatalogPriceFusionRechecksEachHTTPRound(t *testing.T) {
	for _, mode := range []string{"still-free", "promotion-ended", "explicit-price-added-during-generation"} {
		t.Run(mode, func(t *testing.T) {
			f := newCatalogPricingFixture(t)
			f.scriptedToolTurn(func() {
				if mode == "explicit-price-added-during-generation" {
					f.key.Group.ModelPricing = []ChannelModelPricing{{Models: []string{futureFreeUID}, Platform: PlatformDevin, BillingMode: BillingModeToken, InputPrice: testPtrFloat64(.001), OutputPrice: testPtrFloat64(.002)}}
				}
			})
			first, err := f.fusionRequest(f.toolTurnBody(""), "")
			require.NoError(t, err)
			require.Equal(t, 2, f.chatCalls)
			var response apicompat.ResponsesResponse
			require.NoError(t, json.Unmarshal(first.Body.Bytes(), &response))
			require.NotEmpty(t, response.Output)
			callID := response.Output[0].CallID
			require.NotEmpty(t, callID)
			sessionID := first.Header().Get(fusion.HeaderSession)
			sess, _, err := f.service.devin.fusionEngine().Load(context.Background(), fusion.Scope(f.key.UserID, f.key.ID), sessionID)
			require.NoError(t, err)
			require.Len(t, sess.Attempts, 2)
			require.Equal(t, futureFreeUID, sess.Attempts[1].Model)
			require.Zero(t, sess.Attempts[1].CostUSD, "the first physical Sidekick call must retain its pre-send FREE snapshot, including after an admin edit inside generation")
			if mode == "promotion-ended" {
				f.models[1].PromoStatus.EndDate = timestamppb.New(time.Now().Add(-time.Second))
			}
			f.expireCatalog()
			_, err = f.fusionRequest(f.toolTurnBody(callID), sessionID)
			if mode == "promotion-ended" {
				require.ErrorContains(t, err, futureFreeUID)
				require.Equal(t, 2, f.chatCalls, "an expired first-round promotion must not authorize any second-round generation")
				return
			}
			require.NoError(t, err)
			require.Equal(t, 4, f.chatCalls)
			sess, _, err = f.service.devin.fusionEngine().Load(context.Background(), fusion.Scope(f.key.UserID, f.key.ID), sessionID)
			require.NoError(t, err)
			require.Len(t, sess.Attempts, 4)
			require.Equal(t, futureFreeUID, sess.Attempts[2].Model)
			if mode == "still-free" {
				require.Zero(t, sess.Attempts[2].CostUSD)
			} else {
				require.Positive(t, sess.Attempts[2].CostUSD, "a new call must adopt the newly explicit scoped rate")
			}
			require.Equal(t, 2, f.modelCalls)
		})
	}
}

func TestDevinCatalogPriceIngressRecoveryUsesPersistedSnapshot(t *testing.T) {
	f := newCatalogPricingFixture(t)
	f.scriptedToolTurn(func() { f.bills.fail = true })
	first, err := f.fusionRequest(f.toolTurnBody(""), "")
	require.Error(t, err)
	sessionID := first.Header().Get(fusion.HeaderSession)
	scope := fusion.Scope(f.key.UserID, f.key.ID)
	sess, _, err := f.service.devin.fusionEngine().Load(context.Background(), scope, sessionID)
	require.NoError(t, err)
	require.Len(t, sess.Attempts, 2)
	require.False(t, sess.Attempts[1].Settled)
	require.NotEmpty(t, sess.Attempts[1].PricingSnapshot)
	f.bills.fail = false
	f.failCatalog = true
	f.expireCatalog()
	// A new ingress creates a new settlement executor in Bind, before account
	// scheduling or catalog refresh. It must recover from the stored receipt.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set(fusion.HeaderSession, sessionID)
	c.Set("api_key", f.key)
	_, err = f.service.BindDevinFusionRequest(c, f.key, "fusion-team", f.toolTurnBody(""), "responses", nil)
	require.NoError(t, err)
	sess, _, err = f.service.devin.fusionEngine().Load(context.Background(), scope, sessionID)
	require.NoError(t, err)
	require.True(t, sess.Attempts[1].Settled)
	require.Zero(t, sess.Attempts[1].CostUSD)
	require.Equal(t, 2, f.chatCalls)
	require.Equal(t, 1, f.modelCalls, "ingress settlement must not fetch the current catalog")
	require.Len(t, f.bills.commands, 2)
}
