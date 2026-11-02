//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type warmKeys struct {
	APIKeyQuotaUpdater
	key   *APIKey
	reads atomic.Int64
}

type warmUserRepo struct {
	*openAIRecordUsageUserRepoStub
	user *User
	err  error
}

type warmSubscriptionRepo struct {
	UserSubscriptionRepository
	subscription *UserSubscription
}

func (r *warmSubscriptionRepo) GetByID(context.Context, int64) (*UserSubscription, error) {
	return r.subscription, nil
}

func TestFusionWarmBillingModeChangeRetainsOriginalFundingSource(t *testing.T) {
	for _, originalMode := range []string{SubscriptionTypeStandard, SubscriptionTypeSubscription} {
		t.Run(originalMode, func(t *testing.T) {
			f := newWarmFixture(t)
			groupID := int64(9)
			f.keys.key.GroupID = &groupID
			group := &Group{ID: groupID, Platform: PlatformDevin, Status: StatusActive, RateMultiplier: 1, SubscriptionType: originalMode}
			f.keys.key.Group = group
			f.snapshot.Identity.GroupID = groupID
			if originalMode == SubscriptionTypeSubscription {
				f.snapshot.Identity.SubscriptionID = 88
				f.service.userSubRepo = &warmSubscriptionRepo{subscription: &UserSubscription{ID: 88, UserID: 33, GroupID: groupID, Status: SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour)}}
			}
			_, err := f.adapter.executor(context.Background(), f.snapshot, false)
			require.NoError(t, err)
			group.SubscriptionType = SubscriptionTypeSubscription
			if originalMode == SubscriptionTypeSubscription {
				group.SubscriptionType = SubscriptionTypeStandard
			}
			a := fusion.Attempt{ID: "original-funding-warm", Turn: 1, Purpose: fusion.CacheRefresh, Role: fusion.Lead, Model: f.snapshot.Model, BillingModel: f.snapshot.Model, StartedAt: time.Now(), EstimateUSD: .1, Status: "completed", Response: &apicompat.ResponsesResponse{Status: "completed", Usage: &apicompat.ResponsesUsage{InputTokens: 100, OutputTokens: 1}}}
			_, err = f.adapter.Settle(context.Background(), f.snapshot, a)
			require.ErrorContains(t, err, "original cache refresh billing mode changed")
			_, err = f.adapter.Estimate(context.Background(), f.snapshot)
			require.ErrorContains(t, err, "original cache refresh billing mode changed")
			require.Empty(t, f.bills.commands, "known usage must remain pending instead of charging another funding source")
			require.Zero(t, f.calls.Load())
			group.SubscriptionType = originalMode
			x, err := f.adapter.executor(context.Background(), f.snapshot, false)
			require.NoError(t, err, "restoring the original mode makes settlement recoverable")
			if originalMode == SubscriptionTypeSubscription {
				require.EqualValues(t, 88, x.subscription.ID)
			} else {
				require.Nil(t, x.subscription)
			}
		})
	}
}

func (r *warmUserRepo) GetByID(context.Context, int64) (*User, error) {
	return r.user, r.err
}

func TestFusionWarmRechecksHydratedUserGroupGrants(t *testing.T) {
	for _, exclusive := range []bool{true, false} {
		t.Run(fmt.Sprint(exclusive), func(t *testing.T) {
			f := newWarmFixture(t)
			groupID := int64(9)
			f.keys.key.GroupID = &groupID
			f.keys.key.Group = &Group{ID: groupID, Platform: PlatformDevin, Status: StatusActive, RateMultiplier: 1, IsExclusive: exclusive}
			f.account.GroupIDs = []int64{groupID}
			f.snapshot.Identity.GroupID = groupID
			current := *f.keys.key.User
			current.RestrictPublicGroups = !exclusive
			current.AllowedGroups = []int64{groupID}
			repo := &warmUserRepo{openAIRecordUsageUserRepoStub: &openAIRecordUsageUserRepoStub{}, user: &current}
			f.service.userRepo = repo
			// The API-key projection lacks allowed_groups; the hydrated user is
			// authoritative for both admission and revocation while idle.
			estimate, err := f.adapter.Estimate(context.Background(), f.snapshot)
			require.NoError(t, err)
			a := fusion.Attempt{ID: "grant-paid-warm", Turn: 1, Purpose: fusion.CacheRefresh, Role: fusion.Lead, Model: f.snapshot.Model, StartedAt: time.Now(), EstimateUSD: estimate}
			result, err := f.adapter.Refresh(context.Background(), f.snapshot, a)
			require.NoError(t, err)
			current.AllowedGroups = nil
			_, ok := f.adapter.Acquire(context.Background(), f.snapshot)
			require.False(t, ok)
			_, err = f.adapter.Estimate(context.Background(), f.snapshot)
			require.ErrorContains(t, err, "user group grant changed")
			_, err = f.adapter.Refresh(context.Background(), f.snapshot, a)
			require.ErrorContains(t, err, "user group grant changed")
			require.EqualValues(t, 1, f.calls.Load())
			a.Response, a.BillingModel = result.Outcome.Response, result.Outcome.BillingModel
			repo.err = errors.New("user authorization lookup unavailable")
			_, err = f.adapter.Estimate(context.Background(), f.snapshot)
			require.Error(t, err)
			_, err = f.adapter.Settle(context.Background(), f.snapshot, a)
			require.NoError(t, err, "settlement does not need a current group grant or user authorization lookup")
			require.Len(t, f.bills.commands, 1)
		})
	}
}

func TestFusionWarmHonorsAccountModelWhitelistAndMapping(t *testing.T) {
	for _, mode := range []string{"whitelist removed", "alias retargeted", "legacy missing account model"} {
		t.Run(mode, func(t *testing.T) {
			f := newWarmFixture(t)
			f.snapshot.Identity.AccountModel = "account-fusion-alias"
			f.account.Credentials["model_mapping"] = map[string]any{"account-fusion-alias": f.session.PublicModel}
			estimate, err := f.adapter.Estimate(context.Background(), f.snapshot)
			require.NoError(t, err, "an allowed account alias resolves to the captured profile")
			a := fusion.Attempt{ID: "mapping-paid-warm", Turn: 1, Purpose: fusion.CacheRefresh, Role: fusion.Lead, Model: f.snapshot.Model, StartedAt: time.Now(), EstimateUSD: estimate}
			result, err := f.adapter.Refresh(context.Background(), f.snapshot, a)
			require.NoError(t, err)
			switch mode {
			case "whitelist removed":
				f.account.Credentials["model_mapping"] = map[string]any{"another-model": f.session.Profile.SidekickModel}
			case "alias retargeted":
				f.account.Credentials["model_mapping"] = map[string]any{"account-fusion-alias": f.session.Profile.SidekickModel}
			case "legacy missing account model":
				f.snapshot.Identity.AccountModel = ""
			}
			_, ok := f.adapter.Acquire(context.Background(), f.snapshot)
			require.False(t, ok)
			_, err = f.adapter.Estimate(context.Background(), f.snapshot)
			require.Error(t, err)
			_, err = f.adapter.Refresh(context.Background(), f.snapshot, a)
			require.Error(t, err)
			require.EqualValues(t, 1, f.calls.Load())
			a.Response, a.BillingModel = result.Outcome.Response, result.Outcome.BillingModel
			_, err = f.adapter.Settle(context.Background(), f.snapshot, a)
			require.NoError(t, err, "model restrictions cannot erase existing fees")
			require.Len(t, f.bills.commands, 1)
		})
	}
}

type warmQuotaCache struct {
	*fusionTestBillingCache
	quotaMu   sync.Mutex
	reads     []string
	writes    []string
	exhausted bool
}

func (c *warmQuotaCache) GetUserPlatformQuotaCache(_ context.Context, _ int64, platform string) (*UserPlatformQuotaCacheEntry, bool, error) {
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()
	c.reads = append(c.reads, platform)
	limit, usage := 10.0, 0.0
	if c.exhausted {
		usage = limit
	}
	now := time.Now()
	return &UserPlatformQuotaCacheEntry{SchemaVersion: UserPlatformQuotaCacheSchemaV1, DailyLimitUSD: &limit, DailyUsageUSD: usage, DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}, true, nil
}

func (c *warmQuotaCache) IncrUserPlatformQuotaUsageCache(_ context.Context, _ int64, platform string, _ float64, _ time.Duration, _ bool) error {
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()
	c.writes = append(c.writes, platform)
	return nil
}

func TestFusionWarmCompositeQuotaSurvivesDetachedContext(t *testing.T) {
	f := newWarmFixture(t)
	groupID := int64(9)
	f.keys.key.GroupID = &groupID
	f.keys.key.Group = &Group{ID: groupID, Platform: PlatformComposite, Status: StatusActive, RateMultiplier: 1}
	f.account.GroupIDs = []int64{groupID}
	f.snapshot.Identity.GroupID = groupID
	f.snapshot.Identity.QuotaPlatform = PlatformDevin
	quota := &warmQuotaCache{fusionTestBillingCache: f.cache, exhausted: true}
	f.service.billingCacheService.cache = quota
	f.service.billingCacheService.userPlatformQuotaRepo = &fakeQuotaRepo{}
	f.service.userPlatformQuotaRepo = &fakeQuotaRepo{}
	f.service.cfg.Database.UserPlatformQuotaFlusherEnabled = true
	estimate, err := f.adapter.Estimate(context.Background(), f.snapshot)
	require.NoError(t, err)
	a := fusion.Attempt{ID: "composite-paid-warm", Turn: 1, Purpose: fusion.CacheRefresh, Role: fusion.Lead, Model: f.snapshot.Model, StartedAt: time.Now(), EstimateUSD: estimate}
	_, err = f.adapter.Refresh(context.Background(), f.snapshot, a)
	require.ErrorContains(t, err, "balance, quota or request limit")
	require.Equal(t, []string{PlatformDevin}, quota.reads)
	require.Zero(t, f.calls.Load())
	quota.exhausted = false
	result, err := f.adapter.Refresh(context.Background(), f.snapshot, a)
	require.NoError(t, err)
	a.Response, a.BillingModel = result.Outcome.Response, result.Outcome.BillingModel
	_, err = f.adapter.Settle(context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformOpenAI), f.snapshot, a)
	require.NoError(t, err)
	require.Equal(t, []string{PlatformDevin}, quota.writes)
	for _, platform := range quota.reads {
		require.Equal(t, PlatformDevin, platform)
	}
	f.snapshot.Identity.QuotaPlatform = ""
	_, err = f.adapter.Estimate(context.Background(), f.snapshot)
	require.ErrorContains(t, err, "quota platform is unavailable")
}

func TestFusionWarmRechecksOriginalClientModelAllowlist(t *testing.T) {
	f := newWarmFixture(t)
	groupID := int64(9)
	group := &Group{ID: groupID, Platform: PlatformComposite, Status: StatusActive, RateMultiplier: 1, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"my-fusion-alias"}}}
	f.keys.key.GroupID, f.keys.key.Group = &groupID, group
	f.account.GroupIDs = []int64{groupID}
	f.snapshot.Identity.GroupID = groupID
	f.snapshot.Identity.ClientModel = "my-fusion-alias"
	estimate, err := f.adapter.Estimate(context.Background(), f.snapshot)
	require.NoError(t, err, "the ingress alias is allowed even when the resolved profile is absent from the allowlist")
	a := fusion.Attempt{ID: "alias-paid-warm", Turn: 1, Purpose: fusion.CacheRefresh, Role: fusion.Lead, Model: f.snapshot.Model, StartedAt: time.Now(), EstimateUSD: estimate}
	result, err := f.adapter.Refresh(context.Background(), f.snapshot, a)
	require.NoError(t, err)
	group.ModelAllowlist.Models = []string{f.session.PublicModel}
	_, err = f.adapter.Estimate(context.Background(), f.snapshot)
	require.ErrorContains(t, err, "client model is no longer allowed")
	_, err = f.adapter.Refresh(context.Background(), f.snapshot, a)
	require.ErrorContains(t, err, "client model is no longer allowed")
	require.EqualValues(t, 1, f.calls.Load())
	// Revoking admission cannot erase fees already incurred under that alias.
	a.Response, a.BillingModel = result.Outcome.Response, result.Outcome.BillingModel
	_, err = f.adapter.Settle(context.Background(), f.snapshot, a)
	require.NoError(t, err)
	require.Len(t, f.bills.commands, 1)
	f.snapshot.Identity.ClientModel = ""
	_, err = f.adapter.Estimate(context.Background(), f.snapshot)
	require.Error(t, err, "legacy snapshots cannot bypass an enabled allowlist")
}

func TestFusionWarmIngressRecoversCacheOnlyBillingAfterCancel(t *testing.T) {
	for _, retryCancel := range []bool{true, false} {
		t.Run(fmt.Sprint(retryCancel), func(t *testing.T) {
			f := newWarmFixture(t)
			f.service.concurrencyService = NewConcurrencyService(&fusionAuxCapacityCache{})
			f.bills.fail = true
			require.ErrorContains(t, f.tick(t), "settlement is pending")
			require.False(t, f.session.NeedsUsageReconciliation(), "only the independent cache journal needs recovery")
			f.service.cfg.Gateway.DevinFusionCacheWarmEnabled = false
			bind := func(cancel bool) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				c.Request.Header.Set(fusion.HeaderSession, f.session.ID)
				if cancel {
					c.Request.Header.Set(fusion.HeaderCancel, "true")
				}
				bound, err := f.service.BindDevinFusionRequest(c, f.keys.key, f.session.PublicModel, fusionProtocolBody("responses", false, ""), "responses", f.keys)
				require.True(t, bound)
				if cancel {
					require.True(t, DevinFusionHandled(err))
					require.Equal(t, 200, w.Code)
				} else {
					require.ErrorContains(t, err, "session is CANCELLED")
				}
				return w
			}
			require.Contains(t, bind(true).Body.String(), `"usage_settlement":"pending"`)
			f.bills.fail = false
			bind(retryCancel)
			bind(retryCancel)
			pending, err := f.controller.NeedsReconciliation(context.Background(), f.session.Scope, f.session.ID)
			require.NoError(t, err)
			require.False(t, pending)
			require.Len(t, f.bills.commands, 1)
			require.Zero(t, f.cache.count())
			require.EqualValues(t, 1, f.calls.Load(), "billing recovery must not replay the warm call")
		})
	}
}

func TestFusionWarmIngressCancelKeepsUnknownCacheUsagePending(t *testing.T) {
	f := newWarmFixture(t)
	f.service.concurrencyService = NewConcurrencyService(&fusionAuxCapacityCache{})
	f.chat = func(*http.Request, *pb.GetChatMessageRequest) (*http.Response, error) {
		return nil, errors.New("connection lost after send")
	}
	require.NoError(t, f.tick(t))
	f.service.cfg.Gateway.DevinFusionCacheWarmEnabled = false
	for range 2 {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Request.Header.Set(fusion.HeaderSession, f.session.ID)
		c.Request.Header.Set(fusion.HeaderCancel, "true")
		_, err := f.service.BindDevinFusionRequest(c, f.keys.key, f.session.PublicModel, fusionProtocolBody("responses", false, ""), "responses", f.keys)
		require.True(t, DevinFusionHandled(err))
		require.Contains(t, w.Body.String(), `"usage_settlement":"pending"`)
	}
	require.Empty(t, f.bills.commands)
	require.Equal(t, 1, f.cache.count())
	require.EqualValues(t, 1, f.calls.Load())
}

func (k *warmKeys) GetByID(context.Context, int64) (*APIKey, error) {
	k.reads.Add(1)
	return k.key, nil
}

type warmFixture struct {
	adapter    *devinCacheWarmAdapter
	controller *fusion.CacheKeepaliveController
	engine     *fusion.Engine
	session    *fusion.Session
	snapshot   fusion.CacheSnapshot
	service    *OpenAIGatewayService
	keys       *warmKeys
	account    *Account
	models     []*pb.ClientModelConfig
	cache      *fusionTestBillingCache
	bills      *fusionBillingRepo
	logs       *openAIRecordUsageLogRepoStub
	calls      atomic.Int64
	now        atomic.Int64
	chat       func(*http.Request, *pb.GetChatMessageRequest) (*http.Response, error)
}

func newWarmFixture(t *testing.T) *warmFixture {
	t.Helper()
	f := &warmFixture{}
	f.now.Store(time.Now().UnixNano())
	p := fusionTestProfile()
	p.LeadModel = "claude-opus-5-5-medium"
	p.Keepalive = &fusion.KeepalivePolicy{Enabled: true, IntervalSeconds: 10, MaxWindowSeconds: 60, MaxAttempts: 3, MaxCostUSD: 1, TimeoutSeconds: 3}
	f.models = []*pb.ClientModelConfig{{ModelUid: p.LeadModel, MaxTokens: 100000, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}}, {ModelUid: p.SidekickModel, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}}}
	f.account = &Account{ID: 7, Platform: PlatformDevin, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 3, Credentials: map[string]any{"access_token": "synthetic-token"}, Extra: devinCatalogExtra(f.models, "fixture")}
	f.account.Extra[fusion.ExtraProfiles] = []fusion.Profile{p}
	f.keys = &warmKeys{key: &APIKey{ID: 22, UserID: 33, Status: StatusActive, User: &User{ID: 33, Status: StatusActive, Balance: 100, Concurrency: 3}}}
	f.cache = &fusionTestBillingCache{newMemInflightCache(100)}
	f.bills = &fusionBillingRepo{}
	f.logs = &openAIRecordUsageLogRepoStub{inserted: true}
	s := newOpenAIRecordUsageServiceWithBillingRepoForTest(f.logs, f.bills, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	s.userRepo = &warmUserRepo{openAIRecordUsageUserRepoStub: &openAIRecordUsageUserRepoStub{}, user: f.keys.key.User}
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.DevinFusionCacheWarmEnabled = true
	cfg.Billing.InflightReservation.Enabled = true
	s.cfg = cfg
	// Explicit fixture rates; do not treat a family fallback as identified pricing.
	s.billingService.fallbackPrices[p.LeadModel] = &ModelPricing{InputPricePerToken: 4e-6, OutputPricePerToken: 20e-6, CacheCreationPricePerToken: 5e-6, CacheReadPricePerToken: .2e-6}
	s.accountRepo = &fusionAccountRepo{account: f.account}
	s.billingCacheService = NewBillingCacheService(f.cache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(s.billingCacheService.Stop)
	upstream := &devinHTTP{handle: func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == devin.ChatPath {
			f.calls.Add(1)
			_, b, err := devin.ReadFrame(r.Body)
			if err != nil {
				return nil, err
			}
			wire := &pb.GetChatMessageRequest{}
			if err = proto.Unmarshal(b, wire); err != nil {
				return nil, err
			}
			if f.chat != nil {
				return f.chat(r, wire)
			}
			return warmReply(&pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_MAX_TOKENS, Usage: &pb.ModelUsageStats{InputTokens: 4, CacheReadTokens: 96, OutputTokens: 1}}), nil
		}
		var message proto.Message
		switch r.URL.Path {
		case devin.ModelsPath:
			message = &pb.GetCliModelConfigsResponse{ClientModelConfigs: f.models}
		case devin.UserJWTPath:
			message = &pb.GetUserJwtResponse{UserJwt: "fixture-jwt"}
		default:
			return devinReply(404, nil), nil
		}
		b, err := proto.Marshal(message)
		return devinReply(200, b), err
	}}
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := devin.NewRedisSessionStore(rdb)
	s.devin = NewDevinService(s.accountRepo, nil, upstream, cfg, store)
	f.service = s
	f.adapter = &devinCacheWarmAdapter{s, f.keys, f.keys}
	f.engine = fusion.New(store)
	f.controller = fusion.NewCacheKeepaliveController(store)
	f.controller.Adapter = f.adapter
	f.controller.Now = func() time.Time { return time.Unix(0, f.now.Load()) }
	f.session = &fusion.Session{ID: "warm-session", Scope: fusion.Scope(33, 22), AccountID: 7, Turn: 1, TurnStarted: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Profile: p, ProfileRevision: p.Revision(), PublicModel: p.PublicModel, State: fusion.RunningSidekick, Role: fusion.Sidekick, ActiveRequest: -1, Contract: "fixed-contract", Handoffs: []fusion.Handoff{{ID: "handoff-1", Turn: 1}}, Public: []apicompat.ResponsesInputItem{{Type: "message", Role: "user", Content: json.RawMessage(`"task"`)}}}
	f.save(t)
	out := 512
	req := &apicompat.ResponsesRequest{Model: p.LeadModel, Instructions: "immutable lead policy", Input: json.RawMessage(`"immutable lead input"`), MaxOutputTokens: &out, Tools: []apicompat.ResponsesTool{{Type: "function", Name: fusion.DelegateTool, Parameters: json.RawMessage(`{"type":"object"}`)}}}
	require.NoError(t, f.controller.Capture(context.Background(), f.session, req, "original-lead", s.devin.runtimeKey(context.Background(), f.account), fusion.CacheIdentity{UserID: 33, APIKeyID: 22, InboundEndpoint: "/v1/responses", QuotaPlatform: PlatformDevin, ClientModel: p.PublicModel, AccountModel: p.PublicModel}))
	state, _, err := f.controller.Load(context.Background(), f.session.Scope, f.session.ID)
	require.NoError(t, err)
	f.snapshot = *state.Snapshot
	return f
}
func warmReply(message *pb.GetChatMessageResponse) *http.Response {
	b, _ := proto.Marshal(message)
	return devinReply(200, append(devinTestFrame(0, b), devinTestFrame(2, []byte(`{}`))...))
}

func (f *warmFixture) useProfile(t *testing.T, p fusion.Profile, models []*pb.ClientModelConfig, inferred bool) {
	t.Helper()
	data, err := json.Marshal(f.snapshot.Request)
	require.NoError(t, err)
	var request apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal(data, &request))
	request.Model = p.LeadModel
	f.models = models
	f.account.Extra = map[string]any{devinCatalogExtraKey: &devin.CatalogSnapshot{Models: models}}
	if !inferred {
		f.account.Extra[fusion.ExtraProfiles] = []fusion.Profile{p}
	}
	f.session.Profile = p
	f.session.ProfileRevision = p.Revision()
	f.session.PublicModel = p.PublicModel
	f.save(t)
	require.NoError(t, f.controller.Capture(context.Background(), f.session, &request, "lead-profile-fixture", f.service.devin.runtimeKey(context.Background(), f.account), fusion.CacheIdentity{UserID: 33, APIKeyID: 22, InboundEndpoint: "/v1/responses", QuotaPlatform: PlatformDevin, ClientModel: p.PublicModel, AccountModel: p.PublicModel}))
	state, _, err := f.controller.Load(context.Background(), f.session.Scope, f.session.ID)
	require.NoError(t, err)
	f.snapshot = *state.Snapshot
}

func TestFusionWarmAllConcreteMemberFamiliesDefaultOn(t *testing.T) {
	members := []struct{ uid, harness string }{
		{"claude-opus-5-5-medium", "strawberry-pancake"}, {"claude-opus-5-5-max-fast", "strawberry-pancake"},
		{"claude-fable-5-1-high", "strawberry-pancake"}, {"claude-sonnet-5-5-low", "strawberry-pancake"},
		{"gpt-6-1-sol-high-priority", "gpt-5p6"}, {"gpt-6-astra-max", "gpt-5p6-codex"},
		{"gpt-5-4-low", "gpt-5p4-persistent"}, {"gpt-6-luna-high", "gpt-5p6"},
		{"gemini-4-argon-medium", "gemini-3-pro"}, {"glm-5-2", "strawberry-pancake"},
		{"swe-2-medium", "swe-1p6"}, {"future-concrete-model", ""},
	}
	for _, lead := range members {
		for _, inferred := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/inferred=%t", lead.uid, inferred), func(t *testing.T) {
				f := newWarmFixture(t)
				public := "fusion-" + lead.uid + "-sidekick-gpt-5.4-nano"
				if !inferred {
					public = "fusion-custom-pair"
				}
				p := fusion.DefaultProfile(public, lead.uid, "gpt-5.4-nano")
				model := &pb.ClientModelConfig{ModelUid: lead.uid, MaxTokens: 100000, ModelInfo: &pb.ModelInfo{ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}, MaxOutputTokens: 4096}}
				if lead.harness != "" {
					model.ModelInfo.HarnessUids = []string{lead.harness}
				}
				f.service.billingService.pricingService = &PricingService{pricingData: map[string]*LiteLLMModelPricing{lead.uid: {InputCostPerToken: 4e-6, OutputCostPerToken: 20e-6, CacheCreationInputTokenCost: 5e-6, CacheReadInputTokenCost: .2e-6}}}
				f.useProfile(t, p, []*pb.ClientModelConfig{model, f.models[1]}, inferred)
				require.True(t, f.adapter.Supports(f.snapshot))
				require.True(t, p.KeepalivePolicy().Enabled)
				require.NotNil(t, devinFusionProfile(f.account, p.PublicModel))
				f.service.concurrencyService = NewConcurrencyService(&warmCapacity{})
				f.chat = func(_ *http.Request, wire *pb.GetChatMessageRequest) (*http.Response, error) {
					require.Equal(t, lead.uid, wire.ChatModelUid)
					require.EqualValues(t, 1, wire.Configuration.MaxTokens)
					output := uint64(1)
					if strings.HasPrefix(lead.uid, "gpt-") {
						output = 32
					}
					return warmReply(&pb.GetChatMessageResponse{DeltaText: "x", StopReason: pb.StopReason_STOP_REASON_MAX_TOKENS, Usage: &pb.ModelUsageStats{InputTokens: 50, CacheReadTokens: 50, OutputTokens: output}}), nil
				}
				_, before, err := f.engine.Load(context.Background(), f.session.Scope, f.session.ID)
				require.NoError(t, err)
				f.now.Add(int64(240 * time.Second))
				state, _, err := f.controller.Load(context.Background(), f.session.Scope, f.session.ID)
				require.NoError(t, err)
				require.NoError(t, f.controller.Tick(context.Background(), f.session.Scope, f.session.ID, state.Epoch, state.WindowID))
				_, after, err := f.engine.Load(context.Background(), f.session.Scope, f.session.ID)
				require.NoError(t, err)
				require.Equal(t, string(before), string(after))
				require.Len(t, f.bills.commands, 1)
				require.Zero(t, f.cache.count())
				state, _, err = f.controller.Load(context.Background(), f.session.Scope, f.session.ID)
				require.NoError(t, err)
				require.False(t, state.LastCacheConfirmedAt.IsZero())
			})
		}
	}
}

func TestFusionWarmGPTUsageAllowanceIsReservedWithoutRaisingWireCap(t *testing.T) {
	f := newWarmFixture(t)
	p := fusion.DefaultProfile("fusion-budget-fixture", "gpt-6-sol-high", "gpt-5.4-nano")
	p.Keepalive = &fusion.KeepalivePolicy{Enabled: true, MaxCostUSD: .05}
	f.service.billingService.pricingService = &PricingService{pricingData: map[string]*LiteLLMModelPricing{p.LeadModel: {InputCostPerToken: 1e-12, OutputCostPerToken: .01, CacheCreationInputTokenCost: 1e-12}}}
	model := &pb.ClientModelConfig{ModelUid: p.LeadModel, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}}
	f.useProfile(t, p, []*pb.ClientModelConfig{model, f.models[1]}, false)
	request, err := warmRequest(f.snapshot)
	require.NoError(t, err)
	require.Equal(t, 1, *request.MaxOutputTokens)
	priced, err := warmEstimateRequest(f.snapshot)
	require.NoError(t, err)
	require.Equal(t, 32, *priced.MaxOutputTokens)
	estimate, err := f.adapter.Estimate(context.Background(), f.snapshot)
	require.NoError(t, err)
	require.GreaterOrEqual(t, estimate, .32)
	f.service.concurrencyService = NewConcurrencyService(&warmCapacity{})
	f.now.Add(int64(240 * time.Second))
	state, _, err := f.controller.Load(context.Background(), f.session.Scope, f.session.ID)
	require.NoError(t, err)
	require.NoError(t, f.controller.Tick(context.Background(), f.session.Scope, f.session.ID, state.Epoch, state.WindowID))
	require.Zero(t, f.calls.Load(), "a low monetary cap must stop before GPT's billed allowance can be sent")
	state, _, err = f.controller.Load(context.Background(), f.session.Scope, f.session.ID)
	require.NoError(t, err)
	require.Equal(t, "cost_budget_exhausted", state.StopReason)
}
func (f *warmFixture) save(t *testing.T) {
	t.Helper()
	b, err := json.Marshal(f.session)
	require.NoError(t, err)
	require.NoError(t, f.engine.Store.Set(context.Background(), "devin:fusion:session:"+f.session.Scope+":"+fusion.Digest(f.session.ID), b, time.Hour))
}
func (f *warmFixture) tick(t *testing.T) error {
	t.Helper()
	state, _, err := f.controller.Load(context.Background(), f.session.Scope, f.session.ID)
	require.NoError(t, err)
	f.now.Add(int64(10 * time.Second))
	return f.controller.Tick(context.Background(), f.session.Scope, f.session.ID, state.Epoch, state.WindowID)
}

func TestFusionWarmAdapterShapeBillingAndImmutableSnapshot(t *testing.T) {
	f := newWarmFixture(t)
	before, err := json.Marshal(f.snapshot)
	require.NoError(t, err)
	baseline, err := devin.BuildRequest(f.snapshot.Request, f.snapshot.LaneScope)
	require.NoError(t, err)
	f.chat = func(_ *http.Request, wire *pb.GetChatMessageRequest) (*http.Response, error) {
		require.EqualValues(t, 1, wire.Configuration.MaxTokens)
		require.Equal(t, baseline.Prompt, wire.Prompt)
		require.Equal(t, baseline.CascadeId, wire.CascadeId)
		require.Equal(t, fmt.Sprint(baseline.ChatMessagePrompts), fmt.Sprint(wire.ChatMessagePrompts))
		require.Equal(t, fmt.Sprint(baseline.Tools), fmt.Sprint(wire.Tools))
		require.NotEqual(t, baseline.ExecutionId, wire.ExecutionId)
		return warmReply(&pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_MAX_TOKENS, Usage: &pb.ModelUsageStats{InputTokens: 4, CacheReadTokens: 96, OutputTokens: 1}}), nil
	}
	// Capacity is exercised separately; this test invokes the real adapter and
	// durable billing pipeline without a public HTTP response writer.
	estimate, err := f.adapter.Estimate(context.Background(), f.snapshot)
	require.NoError(t, err)
	require.Greater(t, estimate, 0.0)
	a := fusion.Attempt{ID: "paid-warm", Turn: 1, Purpose: fusion.CacheRefresh, Role: fusion.Lead, Model: f.snapshot.Model, StartedAt: time.Now(), EstimateUSD: estimate}
	result, err := f.adapter.Refresh(context.Background(), f.snapshot, a)
	require.NoError(t, err)
	require.True(t, result.RequestSucceeded)
	require.True(t, result.CacheConfirmed)
	require.Empty(t, result.Outcome.Response.Output)
	require.Equal(t, 1, f.cache.count())
	require.Empty(t, f.bills.commands)
	a.Response, a.BillingModel = result.Outcome.Response, result.Outcome.BillingModel
	cost, err := f.adapter.Settle(context.Background(), f.snapshot, a)
	require.NoError(t, err)
	require.InDelta(t, 4*4e-6+96*.2e-6+20e-6, cost, 1e-10)
	_, err = f.adapter.Settle(context.Background(), f.snapshot, a)
	require.NoError(t, err)
	require.Len(t, f.bills.commands, 1)
	require.Equal(t, "fusion:paid-warm", f.bills.commands[0].RequestID)
	require.Equal(t, 96, f.bills.commands[0].CacheReadTokens)
	require.Equal(t, 1, f.bills.commands[0].OutputTokens)
	require.Contains(t, *f.logs.lastLog.ModelMappingChain, "cache_refresh/lead")
	require.Zero(t, f.cache.count())
	after, _ := json.Marshal(f.snapshot)
	require.Equal(t, string(before), string(after))
}

func TestFusionWarmAdapterRejectsUnverifiedAndRevokedBindings(t *testing.T) {
	for _, kind := range []string{"process off", "profile model mismatch", "prefix changed", "key disabled", "key expired", "user disabled", "account paused", "credentials rotated", "profile disabled", "group changed", "budget raised"} {
		t.Run(kind, func(t *testing.T) {
			f := newWarmFixture(t)
			switch kind {
			case "process off":
				f.service.cfg.Gateway.DevinFusionCacheWarmEnabled = false
			case "profile model mismatch":
				f.snapshot.Model = "gpt-5.5"
			case "prefix changed":
				f.snapshot.Request.Input = json.RawMessage(`"other"`)
			case "key disabled":
				f.keys.key.Status = StatusAPIKeyDisabled
			case "key expired":
				expired := time.Now().Add(-time.Minute)
				f.keys.key.ExpiresAt = &expired
			case "user disabled":
				f.keys.key.User.Status = "disabled"
			case "account paused":
				f.account.Schedulable = false
			case "credentials rotated":
				f.account.Credentials["access_token"] = "rotated-fixture"
			case "profile disabled":
				f.account.Extra[fusion.ExtraEnabled] = false
			case "group changed":
				id := int64(99)
				f.keys.key.GroupID = &id
			case "budget raised":
				p := f.session.Profile
				p.MaxCostUSD++
				f.account.Extra[fusion.ExtraProfiles] = []fusion.Profile{p}
			}
			_, err := f.adapter.Refresh(context.Background(), f.snapshot, fusion.Attempt{ID: "denied", Purpose: fusion.CacheRefresh, Role: fusion.Lead, StartedAt: time.Now(), EstimateUSD: 1})
			require.Error(t, err)
			require.Zero(t, f.calls.Load())
			require.Zero(t, f.cache.count())
			require.Empty(t, f.bills.commands)
		})
	}
}

func TestFusionWarmAdapterTerminalShapeAndUsageFailures(t *testing.T) {
	for _, kind := range []string{"text", "thinking", "tool", "cold", "partial cache", "no stop", "no usage", "wrong model", "wrong usage model", "wrong billing model", "unknown transport", "known denied", "no cache", "over output cap", "partial", "error stop", "zero stats"} {
		t.Run(kind, func(t *testing.T) {
			f := newWarmFixture(t)
			f.chat = func(_ *http.Request, _ *pb.GetChatMessageRequest) (*http.Response, error) {
				m := &pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_MAX_TOKENS, Usage: &pb.ModelUsageStats{InputTokens: 4, CacheReadTokens: 96, OutputTokens: 1}}
				switch kind {
				case "text":
					m.DeltaText = "x"
				case "thinking":
					m.DeltaThinking = "x"
				case "tool":
					m.DeltaToolCalls = []*pb.ChatToolCall{{Id: "do-not-execute", Name: fusion.DelegateTool}}
				case "cold":
					m.Usage.CacheReadTokens = 0
					m.Usage.CacheWriteTokens = 96
				case "partial cache":
					m.Usage.CacheReadTokens = 50
					m.Usage.InputTokens = 50
				case "no stop":
					m.StopReason = pb.StopReason_STOP_REASON_UNSPECIFIED
				case "no usage":
					m.Usage = nil
				case "wrong model":
					m.ActualModelUid = proto.String("gpt-5.5")
				case "wrong usage model":
					m.Usage.ModelUid = "gpt-5.5"
				case "wrong billing model":
					m.Usage.BillingModelUid = "gpt-5.5"
				case "unknown transport":
					return nil, errors.New("connection reset after send")
				case "no cache":
					m.Usage.CacheReadTokens = 0
				case "over output cap":
					m.Usage.OutputTokens = 2
				case "partial":
					m.StopReason = pb.StopReason_STOP_REASON_PARTIAL
				case "zero stats":
					m.Usage = &pb.ModelUsageStats{}
				case "error stop":
					m.StopReason = pb.StopReason_STOP_REASON_ERROR
				case "known denied":
					return devinReply(403, nil), nil
				}
				return warmReply(m), nil
			}
			estimate, err := f.adapter.Estimate(context.Background(), f.snapshot)
			require.NoError(t, err)
			a := fusion.Attempt{ID: "shape", Purpose: fusion.CacheRefresh, Role: fusion.Lead, Model: f.snapshot.Model, StartedAt: time.Now(), EstimateUSD: estimate}
			result, _ := f.adapter.Refresh(context.Background(), f.snapshot, a)
			if kind == "zero stats" {
				require.True(t, result.UsageUncertain)
			}
			expected := kind == "text" || kind == "thinking" || kind == "tool" || kind == "cold" || kind == "partial cache" || kind == "no stop"
			require.Equal(t, expected, result.CacheConfirmed)
			if expected {
				require.True(t, result.RequestSucceeded)
				require.Empty(t, result.Outcome.Response.Output)
			}
			if kind == "text" || kind == "thinking" || kind == "tool" {
				require.True(t, result.DiscardedOutput)
			}
			if kind == "known denied" {
				require.True(t, result.Outcome.KnownFailure)
				require.Zero(t, f.cache.count())
				return
			}
			require.Equal(t, 1, f.cache.count(), "observed/unknown usage must remain reserved")
			if result.Outcome.Response != nil && result.Outcome.Response.Usage != nil && !result.UsageUncertain {
				a.Response, a.BillingModel = result.Outcome.Response, result.Outcome.BillingModel
				_, err = f.adapter.Settle(context.Background(), f.snapshot, a)
				require.NoError(t, err)
				require.Len(t, f.bills.commands, 1)
				require.Zero(t, f.cache.count())
			}
		})
	}
}

func TestFusionWarmBillingRecoversAfterCancelWithoutReplay(t *testing.T) {
	f := newWarmFixture(t)
	f.service.concurrencyService = NewConcurrencyService(&fusionAuxCapacityCache{})
	f.bills.fail = true
	err := f.tick(t)
	require.ErrorContains(t, err, "settlement is pending")
	require.EqualValues(t, 1, f.calls.Load())
	require.Equal(t, 1, f.cache.count())
	require.NoError(t, f.controller.Stop(context.Background(), f.session, "cancelled"))
	f.bills.fail = false
	f.keys.key.Status = StatusAPIKeyDisabled
	f.service.cfg.Gateway.DevinFusionCacheWarmEnabled = false
	restartEngine := f.service.devin.fusionEngine()
	f.service.devin.configureCacheWarm(f.service, f.keys)
	restart := restartEngine.Cache
	require.Nil(t, restart.Scheduler)
	require.NoError(t, restart.Resume(context.Background(), f.session))
	require.NoError(t, restart.Resume(context.Background(), f.session))
	require.Len(t, f.bills.commands, 1)
	require.EqualValues(t, 1, f.calls.Load())
	require.Zero(t, f.cache.count())
	state, _, err := restart.Load(context.Background(), f.session.Scope, f.session.ID)
	require.NoError(t, err)
	require.Equal(t, "STOPPED", state.State)
	require.Nil(t, state.Snapshot)
	require.Empty(t, state.Pending)
	require.Greater(t, state.CumulativeCostUSD, 0.0)
}

func TestFusionWarmCancelRetainsLateUsageOrUnknownReservation(t *testing.T) {
	for _, knownUsage := range []bool{true, false} {
		t.Run(fmt.Sprint(knownUsage), func(t *testing.T) {
			f := newWarmFixture(t)
			f.service.concurrencyService = NewConcurrencyService(&warmCapacity{})
			entered := make(chan struct{})
			f.chat = func(r *http.Request, _ *pb.GetChatMessageRequest) (*http.Response, error) {
				close(entered)
				<-r.Context().Done()
				if !knownUsage {
					return nil, r.Context().Err()
				}
				return warmReply(&pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_MAX_TOKENS, Usage: &pb.ModelUsageStats{InputTokens: 4, CacheReadTokens: 96, OutputTokens: 1}}), nil
			}
			done := make(chan error, 1)
			go func() { done <- f.tick(t) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("warm request did not start")
			}
			other := fusion.NewCacheKeepaliveController(f.engine.Store)
			require.NoError(t, other.Stop(context.Background(), f.session, "cancelled"))
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("warm cancellation did not finish")
			}
			state, _, err := f.controller.Load(context.Background(), f.session.Scope, f.session.ID)
			require.NoError(t, err)
			require.Equal(t, "STOPPED", state.State)
			require.Nil(t, state.Snapshot)
			if knownUsage {
				require.Len(t, f.bills.commands, 1)
				require.Zero(t, f.cache.count())
				require.Empty(t, state.Pending)
				require.Greater(t, state.CumulativeCostUSD, 0.0)
			} else {
				require.Empty(t, f.bills.commands)
				require.Equal(t, 1, f.cache.count())
				require.Len(t, state.Pending, 1)
				require.Greater(t, state.ReservedUSD, 0.0)
			}
			require.NoError(t, f.controller.Resume(context.Background(), f.session))
			require.EqualValues(t, 1, f.calls.Load())
		})
	}
}

type warmCapacity struct {
	ConcurrencyCache
	mu                           sync.Mutex
	account, user, waiting, peak int
	failUser                     bool
}

func (c *warmCapacity) GetAccountWaitingCount(context.Context, int64) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waiting, nil
}
func (c *warmCapacity) AcquireAccountSlot(_ context.Context, _ int64, limit int, _ string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit > 0 && c.account >= limit {
		return false, nil
	}
	c.account++
	if c.account > c.peak {
		c.peak = c.account
	}
	return true, nil
}
func (c *warmCapacity) ReleaseAccountSlot(context.Context, int64, string) error {
	c.mu.Lock()
	c.account--
	c.mu.Unlock()
	return nil
}
func (c *warmCapacity) AcquireUserSlot(_ context.Context, _ int64, limit int, _ string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failUser || limit > 0 && c.user >= limit {
		return false, nil
	}
	c.user++
	return true, nil
}
func (c *warmCapacity) ReleaseUserSlot(context.Context, int64, string) error {
	c.mu.Lock()
	c.user--
	c.mu.Unlock()
	return nil
}
func TestFusionWarmCapacityIsNonQueuedAndReleasesPartialAdmission(t *testing.T) {
	f := newWarmFixture(t)
	capacity := &warmCapacity{}
	f.service.concurrencyService = NewConcurrencyService(capacity)
	var wg sync.WaitGroup
	var releases sync.Map
	var admitted atomic.Int64
	for n := range 64 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			release, ok := f.adapter.Acquire(context.Background(), f.snapshot)
			if ok {
				admitted.Add(1)
				releases.Store(n, release)
			}
		}(n)
	}
	wg.Wait()
	require.EqualValues(t, 2, admitted.Load(), "one of three account and user slots stays available to foreground work")
	releases.Range(func(_, value any) bool { value.(func())(); return true })
	require.Zero(t, capacity.account)
	require.Zero(t, capacity.user)
	require.Equal(t, 2, capacity.peak)
	capacity.failUser = true
	_, ok := f.adapter.Acquire(context.Background(), f.snapshot)
	require.False(t, ok)
	require.Zero(t, capacity.account)
	capacity.failUser = false
	capacity.waiting = 1
	_, ok = f.adapter.Acquire(context.Background(), f.snapshot)
	require.False(t, ok)
	require.Zero(t, capacity.account)
	f.account.Concurrency = 1
	capacity.waiting = 0
	_, ok = f.adapter.Acquire(context.Background(), f.snapshot)
	require.False(t, ok)
}

func TestFusionWarmAdmissionFailsBeforePaidSend(t *testing.T) {
	for _, condition := range []string{"unknown price", "no reservation backend", "insufficient balance", "estimate increased"} {
		t.Run(condition, func(t *testing.T) {
			f := newWarmFixture(t)
			estimate, err := f.adapter.Estimate(context.Background(), f.snapshot)
			require.NoError(t, err)
			switch condition {
			case "unknown price":
				delete(f.service.billingService.fallbackPrices, f.snapshot.Model)
				delete(f.service.billingService.fallbackPrices, normalizeAntigravityClaudeTierAlias(f.snapshot.Model))
				require.False(t, f.service.billingService.HasIdentifiedTokenPricing(f.snapshot.Model), "remove both the exact and known base card to exercise unknown pricing")
			case "no reservation backend":
				f.service.cfg.Billing.InflightReservation.Enabled = false
			case "insufficient balance":
				f.cache.mu.Lock()
				f.cache.balance = .0000001
				f.cache.mu.Unlock()
			case "estimate increased":
				estimate /= 2
			}
			_, err = f.adapter.Refresh(context.Background(), f.snapshot, fusion.Attempt{ID: condition, Purpose: fusion.CacheRefresh, Role: fusion.Lead, EstimateUSD: estimate, StartedAt: time.Now()})
			require.Error(t, err)
			require.Zero(t, f.calls.Load())
			require.Zero(t, f.cache.count())
			require.Empty(t, f.bills.commands)
		})
	}
}

func TestFusionWarmTimerWiredThroughGatewayAndOptOut(t *testing.T) {
	t.Run("explicit process opt out", func(t *testing.T) {
		f := newWarmFixture(t)
		f.service.cfg.Gateway.DevinFusionCacheWarmEnabled = false
		e := f.service.devin.fusionEngine()
		f.service.devin.configureCacheWarm(f.service, f.keys)
		require.Nil(t, e.Cache.Scheduler)
		require.False(t, e.Cache.Adapter.Supports(f.snapshot))
	})
	t.Run("scheduled warm", func(t *testing.T) {
		f := newWarmFixture(t)
		p := f.session.Profile
		p.Keepalive.IntervalSeconds = 1
		p.Keepalive.MaxAttempts = 1
		f.account.Extra[fusion.ExtraProfiles] = []fusion.Profile{p}
		f.service.concurrencyService = NewConcurrencyService(&warmCapacity{})
		t.Cleanup(func() { require.NoError(t, f.service.CloseDevinCacheWarm(context.Background())) })
		f.chat = func(_ *http.Request, wire *pb.GetChatMessageRequest) (*http.Response, error) {
			m := &pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN, Usage: &pb.ModelUsageStats{InputTokens: 4, CacheReadTokens: 96, OutputTokens: 20}}
			if wire.Configuration.MaxTokens == 1 {
				m.StopReason = pb.StopReason_STOP_REASON_MAX_TOKENS
				m.Usage.OutputTokens = 1
			} else if wire.ChatModelUid == p.LeadModel {
				m.DeltaToolCalls = []*pb.ChatToolCall{{Id: "delegate", Name: fusion.DelegateTool, ArgumentsJson: `{"task":"fix bug","acceptance_criteria":"read file"}`}}
			} else {
				m.DeltaToolCalls = []*pb.ChatToolCall{{Id: "read", Name: "read_file", ArgumentsJson: `{"path":"main.go"}`}}
			}
			return warmReply(m), nil
		}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Set("api_key", f.keys.key)
		body := fusionProtocolBody("responses", false, "")
		_, err := f.service.BindDevinFusionRequest(c, f.keys.key, p.PublicModel, body, "responses", f.keys)
		require.NoError(t, err)
		_, err = f.service.forwardDevin(c.Request.Context(), c, f.account, body, "responses", "unused", "")
		require.NoError(t, err)
		e := f.service.devin.fusionEngine()
		id := w.Header().Get(fusion.HeaderSession)
		_, before, err := e.Load(context.Background(), fusion.Scope(33, 22), id)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			state, _, loadErr := e.Cache.Load(context.Background(), fusion.Scope(33, 22), id)
			return loadErr == nil && state.CumulativeCostUSD > 0
		}, 4*time.Second, 10*time.Millisecond)
		require.NoError(t, f.service.CloseDevinCacheWarm(context.Background()))
		_, after, err := e.Load(context.Background(), fusion.Scope(33, 22), id)
		require.NoError(t, err)
		require.Equal(t, string(before), string(after))
		require.EqualValues(t, 3, f.calls.Load())
		require.Len(t, f.bills.commands, 3)
		require.Zero(t, f.cache.count())
		require.Contains(t, *f.logs.lastLog.ModelMappingChain, "cache_refresh/lead")
	})
}
