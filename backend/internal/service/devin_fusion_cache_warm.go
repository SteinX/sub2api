package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

type fusionWarmKeyReader interface {
	GetByID(context.Context, int64) (*APIKey, error)
}

// The adapter retains services, never a Gin context, response writer, token or
// foreground executor. Every operation reloads billing/authentication subjects.
type devinCacheWarmAdapter struct {
	service *OpenAIGatewayService
	keys    fusionWarmKeyReader
	updater APIKeyQuotaUpdater
}

// The router is installed before the controller is exposed. A later foreground
// authentication can bind its long-lived key service without racing a timer.
type devinCacheWarmRouter struct {
	binding atomic.Pointer[devinCacheWarmAdapter]
}

func (r *devinCacheWarmRouter) Supports(s fusion.CacheSnapshot) bool {
	a := r.binding.Load()
	return a != nil && a.Supports(s)
}
func (r *devinCacheWarmRouter) Acquire(ctx context.Context, s fusion.CacheSnapshot) (func(), bool) {
	a := r.binding.Load()
	if a == nil {
		return nil, false
	}
	return a.Acquire(ctx, s)
}
func (r *devinCacheWarmRouter) Estimate(ctx context.Context, s fusion.CacheSnapshot) (float64, error) {
	a := r.binding.Load()
	if a == nil {
		return 0, fusion.E("refresh_unavailable", "cache refresh is disabled", 503)
	}
	return a.Estimate(ctx, s)
}
func (r *devinCacheWarmRouter) Refresh(ctx context.Context, s fusion.CacheSnapshot, attempt fusion.Attempt) (fusion.RefreshResult, error) {
	a := r.binding.Load()
	if a == nil {
		return fusion.RefreshResult{Outcome: fusion.Outcome{KnownFailure: true, NotStarted: true}}, fusion.E("refresh_unavailable", "cache refresh is disabled", 503)
	}
	return a.Refresh(ctx, s, attempt)
}
func (r *devinCacheWarmRouter) Settle(ctx context.Context, s fusion.CacheSnapshot, attempt fusion.Attempt) (float64, error) {
	a := r.binding.Load()
	if a == nil {
		return 0, fusion.E("billing_pending", "cache refresh billing service is unavailable", 503)
	}
	return a.Settle(ctx, s, attempt)
}
func (s *DevinService) configureCacheWarm(gateway *OpenAIGatewayService, updater APIKeyQuotaUpdater) {
	keys, ok := updater.(fusionWarmKeyReader)
	if !ok {
		return
	}
	s.mu.Lock()
	router := s.fusionWarm
	s.mu.Unlock()
	if router != nil {
		router.binding.CompareAndSwap(nil, &devinCacheWarmAdapter{gateway, keys, updater})
	}
}
func (s *OpenAIGatewayService) CloseDevinCacheWarm(ctx context.Context) error {
	if s == nil || s.devin == nil {
		return nil
	}
	s.devin.mu.Lock()
	cache := s.devin.fusionCache
	s.devin.mu.Unlock()
	if cache != nil {
		return cache.Scheduler.Close(ctx)
	}
	return nil
}
func (a *devinCacheWarmAdapter) Supports(s fusion.CacheSnapshot) bool {
	return a.service.cfg != nil && a.service.cfg.Gateway.DevinFusionCacheWarmEnabled &&
		s.ShapeRevision == fusion.CacheRefreshShape && s.Profile.Revision() == s.ProfileRevision && s.Profile.LeadModel == s.Model &&
		devin.PlanModelUID(s.Model).Mode == devin.RouteDirect && s.Request != nil && s.Request.Model == s.Model &&
		s.Identity != nil && s.Identity.UserID > 0 && s.Identity.APIKeyID > 0 &&
		s.Scope == fusion.Scope(s.Identity.UserID, s.Identity.APIKeyID) && s.LaneScope != "" &&
		s.RouteIdentity != "" && s.PrefixDigest == fusion.Digest(s.Request.Input)
}

// Native GPT one-token requests report a 32-token usage floor. Keep the wire
// cap at one; only monetary admission and terminal usage checks use this bound.
func warmOutputTokens(uid string) int {
	uid = strings.ToLower(strings.TrimSpace(uid))
	if slash := strings.LastIndex(uid, "/"); slash >= 0 {
		uid = uid[slash+1:]
	}
	if strings.HasPrefix(uid, "gpt-") {
		return 32
	}
	return 1
}
func warmRequest(s fusion.CacheSnapshot) (*apicompat.ResponsesRequest, error) {
	b, err := json.Marshal(s.Request)
	if err != nil {
		return nil, err
	}
	var r apicompat.ResponsesRequest
	if err = json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	limit := 1
	r.MaxOutputTokens = &limit
	return &r, nil
}

// Estimation needs the billed token allowance, without changing the preserved
// warm request. Raising GPT's wire cap can trigger substantially more reasoning.
func warmEstimateRequest(s fusion.CacheSnapshot) (*apicompat.ResponsesRequest, error) {
	request, err := warmRequest(s)
	if err != nil {
		return nil, err
	}
	limit := warmOutputTokens(s.Model)
	request.MaxOutputTokens = &limit
	return request, nil
}

func cacheWarmContext(ctx context.Context, snapshot fusion.CacheSnapshot) context.Context {
	ctx = devin.WithClientIdentity(ctx)
	if snapshot.Identity != nil && snapshot.Identity.QuotaPlatform != "" {
		ctx = context.WithValue(ctx, ctxkey.ForcePlatform, snapshot.Identity.QuotaPlatform)
		ctx = WithResolvedTargetPlatform(ctx, snapshot.Identity.QuotaPlatform)
	}
	return ctx
}

func (a *devinCacheWarmAdapter) executor(ctx context.Context, snapshot fusion.CacheSnapshot, admit bool) (*devinFusionExecutor, error) {
	s := a.service
	identity := snapshot.Identity
	if identity == nil || a.keys == nil || s.devin == nil || s.accountRepo == nil {
		return nil, fusion.E("refresh_unavailable", "cache refresh identity is unavailable", 503)
	}
	key, err := a.keys.GetByID(ctx, identity.APIKeyID)
	if err != nil || key == nil || key.User == nil || key.UserID != identity.UserID || key.User.ID != identity.UserID || snapshot.Scope != fusion.Scope(key.UserID, key.ID) {
		return nil, fusion.E("refresh_unavailable", "cache refresh billing subject is unavailable", 503)
	}
	account, err := s.accountRepo.GetByID(ctx, snapshot.AccountID)
	if err != nil || account == nil || account.ID != snapshot.AccountID {
		return nil, fusion.E("refresh_unavailable", "cache refresh account is unavailable", 503)
	}
	profile := snapshot.Profile
	groupID := int64(0)
	if key.GroupID != nil {
		groupID = *key.GroupID
	}
	if groupID != identity.GroupID {
		return nil, fusion.E("billing_pending", "original cache refresh billing group is unavailable", 503)
	}
	// A pinned subscription ID identifies the admitted funding source. Never
	// reinterpret an incurred charge using a group's newly selected mode.
	if (key.Group != nil && key.Group.IsSubscriptionType()) != (identity.SubscriptionID != 0) {
		return nil, fusion.E("billing_pending", "original cache refresh billing mode changed", 503)
	}
	if key.Group != nil && key.Group.Platform == PlatformComposite && identity.QuotaPlatform == "" {
		return nil, fusion.E("billing_pending", "original cache refresh quota platform is unavailable", 503)
	}
	if admit {
		if s.userRepo == nil {
			return nil, fusion.E("refresh_unavailable", "cache refresh user authorization is unavailable", 503)
		}
		// APIKey.GetByID projects the user row without allowed_groups. Reload
		// through UserRepository so revoked exclusive/public grants take effect.
		user, userErr := s.userRepo.GetByID(ctx, identity.UserID)
		if userErr != nil || user == nil || user.ID != identity.UserID || !user.IsActive() {
			return nil, fusion.E("refresh_unavailable", "cache refresh user authorization changed", 409)
		}
		keyCopy := *key
		keyCopy.User = user
		key = &keyCopy
		// Account model mapping has a mutable hot-path cache. Keep this fresh
		// admission local even when a repository snapshot is shared by callers.
		accountCopy := *account
		account = &accountCopy
		p := devinFusionProfile(account, profile.PublicModel)
		if !a.Supports(snapshot) || !key.IsActive() || key.IsExpired() || key.IsQuotaExhausted() || !key.User.IsActive() || !account.IsSchedulable() || p == nil || p.Revision() != snapshot.ProfileRevision || !p.KeepalivePolicy().Enabled || s.devin.runtimeKey(ctx, account) != snapshot.RouteIdentity {
			return nil, fusion.E("refresh_unavailable", "cache refresh authorization or route changed", 409)
		}
		if groupID != identity.GroupID || groupID != 0 && (key.Group == nil || !key.Group.IsActive() || key.Group.Platform != PlatformDevin && key.Group.Platform != PlatformComposite || !slices.Contains(account.GroupIDs, groupID)) {
			return nil, fusion.E("refresh_unavailable", "cache refresh group access changed", 409)
		}
		if key.Group.ModelAllowlistEnabled() && (strings.TrimSpace(identity.ClientModel) == "" || !key.Group.ModelAllowlist.Allows(identity.ClientModel)) {
			return nil, fusion.E("refresh_unavailable", "cache refresh client model is no longer allowed", 409)
		}
		if key.Group != nil && !key.Group.IsSubscriptionType() && !user.CanBindGroup(groupID, key.Group.IsExclusive) {
			return nil, fusion.E("refresh_unavailable", "cache refresh user group grant changed", 409)
		}
		accountModel := identity.AccountModel
		if accountModel == "" {
			if len(account.GetModelMapping()) > 0 {
				return nil, fusion.E("refresh_unavailable", "original cache refresh account model is unavailable", 409)
			}
			accountModel = profile.PublicModel
		}
		if !account.supportsDevinModel(accountModel) || account.GetMappedModel(accountModel) != profile.PublicModel {
			return nil, fusion.E("refresh_unavailable", "cache refresh account model access changed", 409)
		}
	}
	var subscription *UserSubscription
	if identity.SubscriptionID != 0 {
		if s.userSubRepo == nil {
			return nil, fusion.E("refresh_unavailable", "cache refresh subscription is unavailable", 503)
		}
		subscription, err = s.userSubRepo.GetByID(ctx, identity.SubscriptionID)
		if err != nil || subscription == nil || subscription.ID != identity.SubscriptionID || subscription.UserID != key.UserID || subscription.GroupID != identity.GroupID || admit && !subscription.IsActive() {
			return nil, fusion.E("refresh_unavailable", "cache refresh subscription changed", 409)
		}
	}
	x := &devinFusionExecutor{service: s, account: account, key: key, subscription: subscription, updater: a.updater, profile: profile, sessionID: snapshot.SessionID, publicModel: profile.PublicModel, inboundEndpoint: identity.InboundEndpoint, reservations: map[string]*InflightReservation{}, holdUnsettled: map[string]bool{}}
	return x, nil
}
func (a *devinCacheWarmAdapter) Acquire(ctx context.Context, s fusion.CacheSnapshot) (func(), bool) {
	ctx = cacheWarmContext(ctx, s)
	x, err := a.executor(ctx, s, true)
	if err != nil {
		return nil, false
	}
	_, release, ok := x.Auxiliary(ctx)
	return release, ok
}
func (a *devinCacheWarmAdapter) Estimate(ctx context.Context, s fusion.CacheSnapshot) (float64, error) {
	ctx = cacheWarmContext(ctx, s)
	x, err := a.executor(ctx, s, true)
	if err != nil {
		return 0, err
	}
	req, err := warmEstimateRequest(s)
	if err != nil {
		return 0, err
	}
	// Reserve a cold-cache upper bound, including writes. A cache hit is never
	// assumed when admitting a request or checking balance/quota.
	return x.estimate(ctx, fusion.Call{Purpose: fusion.CacheRefresh, Role: fusion.Lead, Request: req, PricingAt: time.Now()}, false)
}
func (a *devinCacheWarmAdapter) Settle(ctx context.Context, s fusion.CacheSnapshot, attempt fusion.Attempt) (float64, error) {
	ctx = cacheWarmContext(ctx, s)
	x, err := a.executor(ctx, s, false)
	if err != nil {
		return 0, err
	}
	defer x.Close()
	return x.Settle(ctx, attempt)
}
func (a *devinCacheWarmAdapter) Refresh(ctx context.Context, snapshot fusion.CacheSnapshot, attempt fusion.Attempt) (fusion.RefreshResult, error) {
	id := attempt.ID
	if id == "" || attempt.Purpose != fusion.CacheRefresh || attempt.Role != fusion.Lead || math.IsNaN(attempt.EstimateUSD) || math.IsInf(attempt.EstimateUSD, 0) || attempt.EstimateUSD < 0 {
		return fusion.RefreshResult{Outcome: fusion.Outcome{KnownFailure: true, NotStarted: true}}, fusion.E("refresh_unavailable", "invalid cache refresh attempt", 503)
	}
	ctx = cacheWarmContext(ctx, snapshot)
	x, err := a.executor(ctx, snapshot, true)
	result := fusion.RefreshResult{Outcome: fusion.Outcome{KnownFailure: true, NotStarted: true}}
	if err != nil {
		return result, err
	}
	defer x.Close()
	// Force grant discovery before each physical refresh; never reuse a stale
	// foreground token/client/catalog or retry a generation after ambiguity.
	x.models, err = a.service.devin.Models(ctx, x.account, true)
	if err != nil {
		return result, err
	}
	if err = validateDevinFusionMembers(x.profile, x.models); err != nil {
		return result, err
	}
	req, err := warmRequest(snapshot)
	if err != nil {
		return result, err
	}
	priced, err := warmEstimateRequest(snapshot)
	if err != nil {
		return result, err
	}
	estimate, err := x.Estimate(ctx, fusion.Call{AttemptID: id, Purpose: fusion.CacheRefresh, Role: fusion.Lead, Request: priced, PricingAt: attempt.StartedAt})
	if err != nil {
		return result, err
	}
	if estimate > attempt.EstimateUSD {
		return result, fusion.E("budget_exhausted", "cache refresh estimate increased before send", 409)
	}
	r, err := x.reserve(ctx, attempt.EstimateUSD, id)
	if err != nil {
		return result, err
	}
	x.reservations[id] = r
	wire, err := devin.BuildRequest(req, snapshot.LaneScope)
	if err != nil {
		return result, err
	}
	if err = devin.ApplyModelConfig(wire, devin.FindModel(x.models, snapshot.Model), req); err != nil {
		return result, err
	}
	x.client, err = a.service.devin.Client(ctx, x.account)
	if err != nil {
		return result, err
	}
	token, err := a.service.devin.Token(ctx, x.account, false)
	if err != nil {
		return result, err
	}
	prepared, err := x.client.Prepare(ctx, token, wire, devin.PlanModel(devin.FindModel(x.models, snapshot.Model)), x.models)
	if devin.IsUnauthenticated(err) && x.account.GetCredential("api_key") != "" && ctx.Err() == nil {
		token, err = a.service.devin.RecoverToken(ctx, x.account, token)
		if err == nil {
			wire.ModelAssignmentJwt = nil
			prepared, err = x.client.Prepare(ctx, token, wire, devin.PlanModel(devin.FindModel(x.models, snapshot.Model)), x.models)
		}
	}
	if err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if !x.pricing.readyForInference(time.Now()) {
		return result, fusion.E("pricing_unavailable", "account catalog price expired before cache refresh", 400)
	}
	d := devin.NewEventDecoder(snapshot.Model, func(apicompat.ResponsesStreamEvent) error { return nil }, wire.Tools...)
	stop := pb.StopReason_STOP_REASON_UNSPECIFIED
	visible := false
	mismatch := false
	err = x.client.Chat(ctx, prepared.BaseURL, token, wire, func(m *pb.GetChatMessageResponse) error {
		if m.StopReason != pb.StopReason_STOP_REASON_UNSPECIFIED {
			stop = m.StopReason
		}
		for _, uid := range []string{m.GetActualModelUid(), m.GetUsage().GetModelUid(), m.GetUsage().GetBillingModelUid()} {
			if uid != "" && uid != snapshot.Model {
				mismatch = true
			}
		}
		visible = visible || m.DeltaText != "" || m.DeltaThinking != "" || m.DeltaSignature != "" || len(m.DeltaToolCalls) > 0
		// Decode only identities and usage. A one-token response may contain text,
		// reasoning or an incomplete tool fragment on any concrete member model.
		// None is executed, parsed as a business result or retained in a lane.
		stats := &pb.GetChatMessageResponse{MessageId: m.MessageId, StopReason: m.StopReason, Usage: m.Usage, ActualModelUid: m.ActualModelUid}
		return d.Apply(stats)
	})
	billingModel := strings.TrimSpace(d.BillingModel)
	if billingModel == "" {
		billingModel = snapshot.Model
	}
	x.pricing.pinModel(billingModel)
	result.Outcome = fusion.Outcome{Response: &d.Response, BillingModel: billingModel, PricingSnapshot: x.pricing.marshal()}
	if err != nil {
		var upstream *devin.Error
		if errors.As(err, &upstream) && !visible && d.Response.Usage == nil {
			status := upstream.HTTPStatus()
			result.Outcome.KnownFailure = status == 400 || status == 401 || status == 403 || status == 429
		}
	}
	if d.Response.Usage != nil || !result.Outcome.KnownFailure {
		x.holdUnsettled[id] = true
	}
	// A clean Connect trailer is terminal even when a provider omits stop_reason.
	// Broken streams without a known terminal stop keep their monetary reserve.
	u := d.Response.Usage
	knownStop := stop != pb.StopReason_STOP_REASON_UNSPECIFIED && stop != pb.StopReason_STOP_REASON_PARTIAL && stop != pb.StopReason_STOP_REASON_INCOMPLETE
	result.UsageUncertain = u != nil && (u.InputTokens <= 0 || !knownStop && err != nil)
	result.DiscardedOutput = visible
	complete := stop != pb.StopReason_STOP_REASON_PARTIAL && stop != pb.StopReason_STOP_REASON_INCOMPLETE && stop != pb.StopReason_STOP_REASON_ERROR && stop != pb.StopReason_STOP_REASON_CONTENT_FILTER && stop != pb.StopReason_STOP_REASON_NONFINITE_LOGIT_OR_PROB
	compatible := complete && !mismatch && u != nil && u.InputTokens > 0 && u.OutputTokens >= 0 && u.OutputTokens <= warmOutputTokens(snapshot.Model) && billingModel == snapshot.Model
	if err == nil && u != nil && !result.UsageUncertain {
		d.Response.Status = "completed"
	}
	result.RequestSucceeded = err == nil && compatible
	// Reported reads and writes both attest cache activity. Providers quantize
	// read blocks differently, so no family-specific full-prefix ratio is imposed.
	result.CacheConfirmed = result.RequestSucceeded && (u.CacheCreationInputTokens > 0 || u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0)
	if err == nil && !compatible {
		err = fusion.E("refresh_incompatible", "cache refresh lacks a bounded terminal usage result", 502)
	}
	return result, err
}
