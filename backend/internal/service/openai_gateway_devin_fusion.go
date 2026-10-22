package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type devinFusionExecutor struct {
	service            *OpenAIGatewayService
	c                  *gin.Context
	account            *Account
	key                *APIKey
	subscription       *UserSubscription
	updater            APIKeyQuotaUpdater
	profile            fusion.Profile
	sessionID          string
	publicModel        string
	client             *devin.Client
	models             []*pb.ClientModelConfig
	reservations       map[string]*InflightReservation
	stream             bool
	clientDisconnected bool
	inboundEndpoint    string
	userAgent          string
	ipAddress          string
	holdUnsettled      map[string]bool
}

func (x *devinFusionExecutor) Close() {
	for id, r := range x.reservations {
		if !x.holdUnsettled[id] {
			r.HandlerDone()
		}
	}
}

func (x *devinFusionExecutor) MeasureContext(req *apicompat.ResponsesRequest) (int, int, error) {
	wire, err := devin.BuildRequest(req, "fusion:capacity-check")
	if err != nil {
		return 0, 0, fusion.E("context_limit", "role request cannot be represented safely for context measurement", 413)
	}
	b, _ := json.Marshal(wire)
	raw, _ := json.Marshal(req)
	safe := x.profile.MaxContextBytes
	if model := devin.FindModel(x.models, req.Model); model != nil {
		if limit := int(model.GetMaxTokens()); limit > 0 {
			margin := max(256, limit/20)
			safe = min(safe, limit-int(wire.Configuration.MaxTokens)-margin)
		}
	}
	return max(len(b), len(raw)), safe, nil
}

func (x *devinFusionExecutor) Auxiliary(ctx context.Context) (fusion.Executor, func(), bool) {
	s := x.service.concurrencyService
	if s == nil || x.key == nil || x.key.User == nil {
		return nil, nil, false
	}
	// Leave one slot available to newly arriving foreground work. A necessary
	// hard-limit summary instead runs inline under the caller's existing slot.
	accountLimit, userLimit := x.account.Concurrency, x.key.User.Concurrency
	if accountLimit == 1 || userLimit == 1 {
		return nil, nil, false
	}
	if accountLimit > 0 {
		accountLimit--
	}
	if userLimit > 0 {
		userLimit--
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	waiting, err := s.GetAccountWaitingCount(bounded, x.account.ID)
	if err != nil || waiting > 0 {
		return nil, nil, false
	}
	account, err := s.AcquireAccountSlot(bounded, x.account.ID, accountLimit)
	if err != nil || account == nil || !account.Acquired {
		return nil, nil, false
	}
	user, err := s.AcquireUserSlot(bounded, x.key.UserID, userLimit)
	if err != nil || user == nil || !user.Acquired {
		account.ReleaseFunc()
		return nil, nil, false
	}
	child := *x
	child.c = nil
	child.stream = false
	child.clientDisconnected = false
	child.reservations = map[string]*InflightReservation{}
	child.holdUnsettled = map[string]bool{}
	return &child, func() { child.Close(); user.ReleaseFunc(); account.ReleaseFunc() }, true
}
func (x *devinFusionExecutor) cost(ctx context.Context, model string, u UsageTokens, at time.Time) (*CostBreakdown, error) {
	s := x.service
	if s.billingService == nil {
		return nil, fusion.E("billing_unavailable", "physical model pricing is unavailable", 503)
	}
	if identified, _ := s.hasIdentifiedOpenAIResponsePricing(ctx, model, x.key); !identified {
		return nil, fusion.E("pricing_unavailable", "configure identified pricing for each real Fusion model UID", 400)
	}
	multiplier := 1.0
	if s.cfg != nil {
		multiplier = s.cfg.Default.RateMultiplier
	}
	if x.key.GroupID != nil && x.key.Group != nil {
		multiplier = s.ResolveUserGroupRateMultiplier(ctx, x.key.UserID, *x.key.GroupID, x.key.Group.RateMultiplier)
	}
	multiplier, _ = computePeakAwareMultipliers(x.key, multiplier, at)
	cost, err := s.calculateOpenAIRecordUsageTokenCost(ctx, x.key, model, multiplier, at, u, "", "", openAILongContextBillingGate(x.account), false)
	if err != nil || cost == nil {
		return nil, fusion.E("pricing_unavailable", "configure pricing for each real Fusion member before inference", 400)
	}
	return cost, nil
}
func (x *devinFusionExecutor) Estimate(ctx context.Context, call fusion.Call) (float64, error) {
	s := x.service
	if x.key == nil || x.key.User == nil || s.billingCacheService == nil || s.usageBillingRepo == nil {
		return 0, fusion.E("billing_unavailable", "Fusion requires durable usage billing and billing eligibility checks", 503)
	}
	simple := s.cfg != nil && s.cfg.RunMode == config.RunModeSimple
	if !simple {
		if !s.billingCacheService.InflightReservationEnabled() {
			return 0, fusion.E("billing_unavailable", "enable billing.inflight_reservation for Fusion", 503)
		}
		if _, ok := s.billingCacheService.cache.(InflightBalanceReservationCache); !ok {
			return 0, fusion.E("billing_unavailable", "Fusion requires atomic inflight balance reservations", 503)
		}
		if _, ok := s.billingCacheService.cache.(InflightScopeReservationCache); !ok {
			return 0, fusion.E("billing_unavailable", "Fusion requires atomic inflight quota reservations", 503)
		}
	}
	if err := s.billingCacheService.CheckBillingEligibility(ctx, x.key.User, x.key, x.key.Group, x.subscription, QuotaPlatform(ctx, x.key)); err != nil {
		return 0, fusion.E("billing_denied", "balance, quota or request limit does not permit another Fusion inference", 402)
	}
	if (x.key.Quota > 0 || x.key.HasRateLimits()) && x.updater == nil {
		return 0, fusion.E("billing_unavailable", "API key quota accounting is unavailable", 503)
	}
	// Check both member prices before the first paid inference, while allowing
	// replay of a durable public response without another billing admission.
	for _, uid := range []string{x.profile.LeadModel, x.profile.SidekickModel} {
		if _, err := x.cost(ctx, uid, UsageTokens{InputTokens: 1, OutputTokens: 1}, call.PricingAt); err != nil {
			return 0, err
		}
	}
	b, _ := json.Marshal(call.Request)
	// Conservative estimate: byte-count input tokens plus a full cache-write
	// bucket and the enforced output cap. The final charge uses terminal usage.
	n := len(b)
	out := *call.Request.MaxOutputTokens
	c, err := x.cost(ctx, call.Request.Model, UsageTokens{InputTokens: n, CacheCreationTokens: n, OutputTokens: out}, call.PricingAt)
	if err != nil {
		return 0, err
	}
	return math.Max(c.ActualCost, c.TotalCost), nil
}
func (x *devinFusionExecutor) Execute(ctx context.Context, call fusion.Call) (fusion.Outcome, error) {
	s := x.service
	reservation, err := x.reserve(ctx, call.EstimateUSD, call.AttemptID)
	if err != nil {
		return fusion.Outcome{KnownFailure: true, NotStarted: true}, fusion.E("billing_denied", "another inference cannot reserve sufficient balance or quota", 402)
	}
	x.reservations[call.AttemptID] = reservation
	wire, err := devin.BuildRequest(call.Request, call.LaneScope)
	if err != nil {
		return fusion.Outcome{KnownFailure: true, NotStarted: true}, err
	}
	selected := devin.FindModel(x.models, call.Request.Model)
	if limit := int(selected.GetMaxTokens()); limit > 0 {
		payload, _ := json.Marshal(wire)
		if len(payload)+int(wire.Configuration.MaxTokens) > limit {
			return fusion.Outcome{KnownFailure: true, NotStarted: true}, fusion.E("context_limit", "role context exceeds the concrete model window", 413)
		}
	}
	if err = devin.ValidateModelCapabilities(wire, selected, call.Request.MaxOutputTokens); err != nil {
		return fusion.Outcome{KnownFailure: true, NotStarted: true}, err
	}
	token, err := s.devin.Token(ctx, x.account, false)
	if err != nil {
		return fusion.Outcome{KnownFailure: true, NotStarted: true}, err
	}
	prepared, err := x.client.Prepare(ctx, token, wire, devin.PlanModel(selected), x.models)
	if devin.IsUnauthenticated(err) && x.account.GetCredential("api_key") != "" && ctx.Err() == nil {
		token, err = s.devin.RecoverToken(ctx, x.account, token)
		if err == nil {
			wire.ModelAssignmentJwt = nil
			prepared, err = x.client.Prepare(ctx, token, wire, devin.PlanModel(selected), x.models)
		}
	}
	if err != nil {
		return fusion.Outcome{KnownFailure: true, NotStarted: true}, err
	}
	decoder := devin.NewEventDecoder(call.Request.Model, func(apicompat.ResponsesStreamEvent) error { return nil })
	if err := ctx.Err(); err != nil {
		return fusion.Outcome{KnownFailure: true, NotStarted: true}, err
	}
	err = consumeDevinFusionChat(ctx, x.c, x.client, prepared.BaseURL, token, wire, decoder, x.stream, &x.clientDisconnected)
	if err == nil {
		err = decoder.Finish()
	}
	billingModel := strings.TrimSpace(decoder.BillingModel)
	if billingModel == "" {
		billingModel = wire.ChatModelUid
	}
	// Profile members are concrete. A changed execution identity is never a
	// silent lane/model switch, but any terminal usage is still settled.
	if decoder.ActualModel != "" && decoder.ActualModel != wire.ChatModelUid || len(decoder.Evidence.ActualModels) > 1 {
		err = fusion.E("route_mismatch", "upstream execution model differs from the pinned Fusion member", 502)
	}
	outcome := fusion.Outcome{Response: &decoder.Response, BillingModel: billingModel, ClientDisconnected: x.clientDisconnected}
	if err != nil {
		var upstream *devin.Error
		if errors.As(err, &upstream) && !decoder.Started {
			status := upstream.HTTPStatus()
			outcome.KnownFailure = status == 400 || status == 401 || status == 403 || status == 429
			if s.accountRepo != nil {
				if status == 429 {
					_ = s.accountRepo.SetRateLimited(context.WithoutCancel(ctx), x.account.ID, time.Now().Add(time.Minute))
				}
				if status == 401 {
					_ = s.accountRepo.SetError(context.WithoutCancel(ctx), x.account.ID, "Devin authorization expired; reconnect this account")
				}
			}
		}
	}
	if decoder.Response.Usage != nil || !outcome.KnownFailure {
		if x.holdUnsettled == nil {
			x.holdUnsettled = map[string]bool{}
		}
		x.holdUnsettled[call.AttemptID] = true
	}
	logger.FromContext(ctx).Info("devin.fusion_attempt", zap.String("component", "audit.devin.fusion"), zap.Int64("account_id", x.account.ID), zap.String("role", string(call.Role)), zap.String("purpose", string(call.Purpose)), zap.String("attempt_id", call.AttemptID), zap.String("model", devin.DiagnosticModelID(wire.ChatModelUid)), zap.Bool("usage_observed", decoder.Response.Usage != nil))
	return outcome, err
}
func (x *devinFusionExecutor) Settle(ctx context.Context, a fusion.Attempt) (float64, error) {
	if a.Response == nil || a.Response.Usage == nil {
		return 0, nil
	}
	u := a.Response.Usage
	cached := 0
	if u.InputTokensDetails != nil {
		cached = u.InputTokensDetails.CachedTokens
	}
	cost, err := x.cost(ctx, a.BillingModel, UsageTokens{InputTokens: max(u.InputTokens-cached-u.CacheCreationInputTokens, 0), OutputTokens: u.OutputTokens, CacheReadTokens: cached, CacheCreationTokens: u.CacheCreationInputTokens}, a.StartedAt)
	if err != nil {
		return 0, err
	}
	result := &OpenAIForwardResult{RequestID: "fusion:" + a.ID, ResponseID: a.Response.ID, Model: x.publicModel, BillingModel: a.BillingModel, UpstreamModel: a.Model, UpstreamResponseModel: a.Response.Model, Usage: OpenAIUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadInputTokens: cached, CacheCreationInputTokens: u.CacheCreationInputTokens}, Duration: time.Since(a.StartedAt), UpstreamEndpoint: devin.ChatPath}
	purpose := a.Purpose
	if purpose == "" {
		purpose = fusion.Inference
	}
	err = x.service.RecordUsage(ctx, &OpenAIRecordUsageInput{Result: result, APIKey: x.key, User: x.key.User, Account: x.account, Subscription: x.subscription, APIKeyService: x.updater, SessionID: x.sessionID, PricingAt: a.StartedAt, InboundEndpoint: x.inboundEndpoint, UpstreamEndpoint: devin.ChatPath, UserAgent: x.userAgent, IPAddress: x.ipAddress, QuotaPlatform: QuotaPlatform(ctx, x.key), RequestPayloadHash: fusion.Digest(a.ID), ChannelUsageFields: ChannelUsageFields{OriginalModel: x.publicModel, BillingModelSource: BillingModelSourceUpstream, ModelMappingChain: fmt.Sprintf("%s→%s/%s:%s", x.profile.PublicModel, purpose, a.Role, a.Model)}})
	if err != nil {
		return 0, err
	}
	x.releaseSettledReservation(a)
	return math.Max(cost.ActualCost, cost.TotalCost), nil
}

func (s *OpenAIGatewayService) forwardDevinFusion(ctx context.Context, c *gin.Context, a *Account, body []byte, protocol, original string, p fusion.Profile) (*OpenAIForwardResult, error) {
	start := time.Now()
	ctx = devin.WithClientIdentity(ctx)
	req, mapping, err := parseDevinFusionRequest(body, protocol)
	fail := func(err error) (*OpenAIForwardResult, error) {
		if !WriteDevinFusionError(c, protocol, err) {
			err = fusion.E("unavailable", "Fusion could not advance the session", 502)
			WriteDevinFusionError(c, protocol, err)
		}
		return &OpenAIForwardResult{Model: original, FusionUsageSettled: true, Stream: req != nil && req.Stream, Duration: time.Since(start)}, err
	}
	if err != nil {
		return fail(fusion.E("input_invalid", err.Error(), 400))
	}
	req.Model = original
	var key *APIKey
	if v, ok := c.Get("api_key"); ok {
		key, _ = v.(*APIKey)
	}
	if key == nil {
		return fail(fusion.E("scope_missing", "Fusion requires an authenticated API key", 401))
	}
	binding, _ := ctx.Value(devinFusionBindingKey{}).(*devinFusionBinding)
	if binding == nil { // Also covers internal service callers; ingress normally binds first.
		_, err = s.BindDevinFusionRequest(c, key, original, body, protocol, nil)
		if err != nil {
			return fail(err)
		}
		binding, _ = c.Request.Context().Value(devinFusionBindingKey{}).(*devinFusionBinding)
	}
	if binding == nil {
		return fail(fusion.E("profile_unavailable", "Fusion profile is not available for this request", 400))
	}
	if binding.AccountID > 0 && binding.AccountID != a.ID {
		return fail(fusion.E("account_bound", "bound Fusion account is unavailable", 409))
	}
	if binding.Profile != nil {
		p = *binding.Profile
	}
	models, err := s.devin.Models(ctx, a)
	if err != nil {
		return fail(fusion.E("catalog_unavailable", "could not verify Fusion model grants", 503))
	}
	if err = validateDevinFusionMembers(p, models); err != nil {
		return fail(err)
	}
	// Verify both member output/tool capabilities before the first paid inference.
	maxTokens := p.MaxOutputTokens
	if req.MaxOutputTokens != nil {
		maxTokens = min(maxTokens, *req.MaxOutputTokens)
	}
	for _, uid := range []string{p.LeadModel, p.SidekickModel} {
		m := devin.FindModel(models, uid)
		if limit := int(m.GetModelInfo().GetMaxOutputTokens()); limit > 0 {
			maxTokens = min(maxTokens, limit)
		}
	}
	if maxTokens <= 0 {
		return fail(fusion.E("input_invalid", "max output tokens must be positive", 400))
	}
	req.MaxOutputTokens = &maxTokens
	client, err := s.devin.Client(ctx, a)
	if err != nil {
		return fail(err)
	}
	var subscription *UserSubscription
	if v, ok := c.Get("subscription"); ok {
		subscription, _ = v.(*UserSubscription)
	}
	x := &devinFusionExecutor{service: s, c: c, account: a, key: key, subscription: subscription, updater: binding.Updater, profile: p, sessionID: binding.SessionID, publicModel: original, client: client, models: models, reservations: map[string]*InflightReservation{}, stream: req.Stream, inboundEndpoint: c.Request.URL.Path, userAgent: c.Request.UserAgent(), ipAddress: c.ClientIP()}
	defer x.Close()
	for _, role := range []fusion.Role{fusion.Lead, fusion.Sidekick} {
		probe := *req
		probe.Model = p.LeadModel
		if role == fusion.Sidekick {
			probe.Model = p.SidekickModel
		}
		probe.Input = json.RawMessage(`"capacity check"`)
		probe.PreviousResponseID = ""
		probe.Store = nil
		_, safe, measureErr := x.MeasureContext(&probe)
		if measureErr != nil {
			return fail(measureErr)
		}
		if err = p.ValidateContextBudget(role, safe); err != nil {
			return fail(err)
		}
	}
	if x.sessionID == "" {
		x.sessionID = uuid.NewString()
		binding.SessionID = x.sessionID
	}
	c.Header(fusion.HeaderSession, x.sessionID)
	if req.Stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
	}
	engine := s.devin.fusionEngine()
	engine.RouteIdentity = s.devin.runtimeKey(ctx, a)
	log := logger.FromContext(ctx)
	engine.Observer = func(event fusion.MaintenanceEvent) {
		log.Info("devin.fusion_maintenance", zap.String("component", "audit.devin.fusion"), zap.String("purpose", string(event.Purpose)), zap.String("role", string(event.Role)), zap.String("stage", event.Stage), zap.String("job_id", event.JobID), zap.String("reason", event.Reason), zap.Int("input_before", event.InputBefore), zap.Int("input_after", event.InputAfter))
	}
	result, err := engine.Advance(ctx, fusion.Input{Scope: fusion.Scope(key.UserID, key.ID), SessionID: x.sessionID, IdempotencyKey: c.GetHeader("Idempotency-Key"), AccountID: a.ID, Profile: p, Request: req}, x)
	if err != nil {
		if ctx.Err() != nil || x.clientDisconnected {
			return &OpenAIForwardResult{Model: original, FusionUsageSettled: true, ClientDisconnect: true, Stream: req.Stream}, err
		}
		return fail(err)
	}
	c.Header("X-Sub2API-Fusion-State", result.State)
	response := result.Response
	if response == nil {
		return fail(fusion.E("response_missing", "Fusion has no publishable response", 503))
	}
	if err = writeDevinFusionResponse(c, response, protocol, req.Stream, mapping); err == nil {
		err = ctx.Err()
	}
	if err != nil {
		engine.Runner.Cancel(fusion.Scope(key.UserID, key.ID), x.sessionID)
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		if session, _, loadErr := engine.Load(cleanup, fusion.Scope(key.UserID, key.ID), x.sessionID); loadErr == nil && engine.Cache != nil {
			_ = engine.Cache.Stop(cleanup, session, "public_response_disconnected")
		}
		cancel()
		return &OpenAIForwardResult{Model: original, FusionUsageSettled: true, ClientDisconnect: true, Stream: req.Stream}, err
	}
	u := response.Usage
	forward := &OpenAIForwardResult{RequestID: response.ID, ResponseID: response.ID, Model: original, UpstreamEndpoint: devin.ChatPath, Stream: req.Stream, FusionUsageSettled: true, Duration: time.Since(start)}
	if u != nil {
		forward.Usage = OpenAIUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens}
		if u.InputTokensDetails != nil {
			forward.Usage.CacheReadInputTokens = u.InputTokensDetails.CachedTokens
		}
	}
	return forward, nil
}

func consumeDevinFusionChat(ctx context.Context, c *gin.Context, client *devin.Client, base, token string, wire *pb.GetChatMessageRequest, d *devin.EventDecoder, stream bool, disconnected *bool) error {
	// A disconnected client may leave one physical inference to drain for at most
	// 90 seconds. The turn deadline always remains in force. No role starts here.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Minute)
	}
	upstream, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	type event struct {
		m    *pb.GetChatMessageResponse
		err  error
		done bool
	}
	events := make(chan event, 1)
	go func() {
		err := client.Chat(upstream, base, token, wire, func(m *pb.GetChatMessageResponse) error {
			select {
			case events <- event{m: m}:
				return nil
			case <-upstream.Done():
				return upstream.Err()
			}
		})
		select {
		case events <- event{err: err, done: true}:
		case <-upstream.Done():
		}
	}()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	requestDone := ctx.Done()
	var drain *time.Timer
	var drained <-chan time.Time
	defer func() {
		if drain != nil {
			drain.Stop()
		}
	}()
	for {
		select {
		case <-requestDone:
			*disconnected = true
			requestDone = nil
			drain = time.NewTimer(90 * time.Second)
			drained = drain.C
		case <-drained:
			return context.Canceled
		case <-upstream.Done():
			return upstream.Err()
		case <-tick.C:
			if stream && ctx.Err() == nil && !*disconnected {
				if _, err := c.Writer.WriteString(": keepalive\n\n"); err != nil {
					*disconnected = true
					if drain == nil {
						drain = time.NewTimer(90 * time.Second)
						drained = drain.C
					}
				} else {
					c.Writer.Flush()
				}
			}
		case e := <-events:
			if e.done {
				return e.err
			}
			if err := d.Apply(e.m); err != nil {
				return err
			}
		}
	}
}
