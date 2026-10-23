package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	cc "github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func DevinContextIntent(c *gin.Context) bool {
	return c.GetHeader(mc.HeaderMode) != "" || c.GetHeader(mc.HeaderSession) != ""
}
func (s *DevinService) managedEngine() *mc.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.contextEngine == nil && s.contextStore != nil {
		s.contextEngine = mc.New(s.contextStore)
		s.contextEngine.Observer = func(event, trigger string) {
			logger.L().Info("devin.context_event", zap.String("event", event), zap.String("trigger", trigger))
		}
	}
	return s.contextEngine
}
func (s *OpenAIGatewayService) ContextEngine() (*mc.Engine, error) {
	if s == nil || s.devin == nil || s.devin.managedEngine() == nil {
		return nil, mc.E("unavailable", "persistent context service is unavailable", 503)
	}
	return s.devin.managedEngine(), nil
}
func (s *OpenAIGatewayService) contextPolicy() mc.Policy {
	if s.cfg == nil {
		return mc.DefaultPolicy()
	}
	return s.cfg.Gateway.DevinContext.Policy()
}

// CheckContextAccess checks the request envelope. Entitlement and manual/auto
// switches are checked against the selected account before any paid work.
func (s *OpenAIGatewayService) CheckContextAccess(key *APIKey, model string) error {
	if s.cfg == nil || s.cfg.Gateway.DevinContext.ForceDisabled || key == nil || key.User == nil || key.Group == nil || key.Group.Platform != PlatformDevin {
		return mc.E("disabled", "managed context is not enabled for this key", 404)
	}
	c := s.cfg.Gateway.DevinContext
	if len(c.APIKeyIDs) > 0 && !slices.Contains(c.APIKeyIDs, key.ID) {
		return mc.E("disabled", "managed context is not enabled for this key", 404)
	}
	if model != "" && (len(c.ModelUIDs) > 0 && !slices.Contains(c.ModelUIDs, model) || key.Group.ModelAllowlistEnabled() && !key.Group.ModelAllowlist.Allows(model)) {
		return mc.E("model_unavailable", "model is not enabled for managed context", 404)
	}
	return nil
}
func contextScope(key *APIKey) string { return fusion.Scope(key.UserID, key.ID) }
func (s *OpenAIGatewayService) contextAccount(ctx context.Context, key *APIKey, id int64, model string, manual bool) (*Account, error) {
	if id > 0 {
		a, err := s.accountRepo.GetByID(ctx, id)
		if err != nil || a == nil || a.Platform != PlatformDevin {
			return nil, mc.E("account_bound", "original context account is unavailable", 409)
		}
		return a, nil
	}
	accounts, err := s.devinFusionAccounts(ctx, key.GroupID)
	if err != nil {
		return nil, err
	}
	var denied error
	for i := range accounts {
		a := &accounts[i]
		if a.Platform == PlatformDevin && a.IsSchedulable() && a.GetMappedModel(model) == model && a.supportsDevinModel(model) {
			// Scheduler snapshots may lag an account edit. Resolve the override
			// from the repository so an enabled account is immediately selectable.
			fresh, err := s.accountRepo.GetByID(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			if _, err = s.checkContextAccountAccess(key, fresh, model, manual); err == nil {
				return fresh, nil
			}
			denied = err
		}
	}
	if denied != nil {
		return nil, denied
	}
	return nil, mc.E("account_unavailable", "no authorized Devin account is available for this concrete model", 503)
}

type contextExecutor struct {
	physical *devinFusionExecutor
	keys     *APIKeyService
	owner    mc.Owner
	manual   bool
}

func (x *contextExecutor) settings(ctx context.Context) (devinContextSettings, error) {
	s := x.physical.service
	if x.keys == nil {
		return devinContextSettings{}, mc.E("unavailable", "current key validation is unavailable", 503)
	}
	key, err := x.keys.GetByID(ctx, x.owner.KeyID)
	if err != nil || key == nil || key.UserID != x.owner.UserID || !key.IsActive() || key.IsExpired() || key.User == nil || key.User.Status != StatusActive {
		return devinContextSettings{}, mc.E("authorization_changed", "API key is no longer available for inference", 403)
	}
	a, err := s.accountRepo.GetByID(ctx, x.owner.AccountID)
	if err != nil || a == nil || !a.IsSchedulable() || a.Platform != PlatformDevin {
		return devinContextSettings{}, mc.E("account_bound", "bound account is unavailable for another inference", 409)
	}
	if key.GroupID != nil && !slices.Contains(a.GroupIDs, *key.GroupID) {
		return devinContextSettings{}, mc.E("account_bound", "bound account no longer belongs to this group", 409)
	}
	if x.owner.GroupID > 0 && (key.GroupID == nil || *key.GroupID != x.owner.GroupID) {
		return devinContextSettings{}, mc.E("authorization_changed", "managed session billing group changed", 403)
	}
	if a.GetMappedModel(x.physical.publicModel) != x.physical.publicModel || !a.supportsDevinModel(x.physical.publicModel) {
		return devinContextSettings{}, mc.E("model_unavailable", "bound account no longer grants the concrete model", 409)
	}
	return s.checkContextAccountAccess(key, a, x.physical.publicModel, x.manual)
}
func (x *contextExecutor) check(ctx context.Context, purpose string) error {
	settings, err := x.settings(ctx)
	if err != nil {
		return err
	}
	if !x.manual && strings.HasPrefix(purpose, "compaction") && !settings.Auto {
		return mc.E("auto_disabled", "automatic compaction is disabled for this account", 409)
	}
	return nil
}
func (x *contextExecutor) AutoCompactionEnabled(ctx context.Context) (bool, error) {
	settings, err := x.settings(ctx)
	return settings.Auto, err
}
func contextError(err error) error {
	var f *fusion.Error
	if errors.As(err, &f) {
		return mc.E(strings.TrimPrefix(f.Code, "devin_fusion_"), strings.ReplaceAll(f.Message, "Fusion", "managed context"), f.Status)
	}
	return err
}
func (x *contextExecutor) Measure(r *apicompat.ResponsesRequest) (cc.Budget, error) {
	size, safe, err := x.physical.MeasureContext(r)
	if err != nil {
		var f *fusion.Error
		if !errors.As(err, &f) {
			return cc.Budget{}, mc.E("input_invalid", err.Error(), 400)
		}
	}
	return cc.Budget{InputEstimate: size, SafeInput: safe, Method: cc.ConservativeUTF8V1}, contextError(err)
}
func (x *contextExecutor) OutputLimit() int { return x.physical.profile.MaxOutputTokens }
func physicalCall(c mc.Call) fusion.Call {
	if strings.HasPrefix(c.Purpose, "compaction") {
		c.Purpose = "compaction"
	}
	return fusion.Call{AttemptID: c.ID, Role: fusion.Role("main"), Purpose: fusion.Purpose(c.Purpose), Request: c.Request, LaneScope: c.LaneScope, PricingAt: c.StartedAt, EstimateUSD: c.EstimateUSD}
}
func (x *contextExecutor) Estimate(ctx context.Context, c mc.Call) (float64, error) {
	if err := x.check(ctx, c.Purpose); err != nil {
		return 0, err
	}
	cost, err := x.physical.Estimate(ctx, physicalCall(c))
	return cost, contextError(err)
}
func (x *contextExecutor) Execute(ctx context.Context, c mc.Call) (mc.Outcome, error) {
	if err := x.check(ctx, c.Purpose); err != nil {
		return mc.Outcome{KnownFailure: true, NotStarted: true}, err
	}
	o, err := x.physical.Execute(ctx, physicalCall(c))
	return mc.Outcome{Response: o.Response, BillingModel: o.BillingModel, KnownFailure: o.KnownFailure, NotStarted: o.NotStarted}, contextError(err)
}
func (x *contextExecutor) Settle(ctx context.Context, a mc.Attempt) (float64, error) {
	response := &apicompat.ResponsesResponse{ID: a.ResponseID, Model: a.ResponseModel, Status: a.ResponseStatus, Usage: a.Usage}
	cost, err := x.physical.Settle(ctx, fusion.Attempt{ID: a.ID, Purpose: fusion.Purpose(contextBillingPurpose(a.Purpose)), Role: fusion.Role("main"), Model: a.Model, BillingModel: a.BillingModel, StartedAt: a.StartedAt, Response: response, EstimateUSD: a.EstimateUSD})
	return cost, contextError(err)
}
func (x *contextExecutor) Auxiliary(ctx context.Context) (mc.Executor, func(), bool) {
	child, release, ok := x.physical.Auxiliary(ctx)
	if !ok {
		return nil, nil, false
	}
	p, ok := child.(*devinFusionExecutor)
	if !ok {
		release()
		return nil, nil, false
	}
	return &contextExecutor{physical: p, keys: x.keys, owner: x.owner, manual: x.manual}, release, true
}
func (s *OpenAIGatewayService) contextPhysical(ctx context.Context, key *APIKey, keys *APIKeyService, owner mc.Owner, model, session string, p mc.Policy, c *gin.Context, inference bool) (*contextExecutor, error) {
	if !inference && owner.GroupID > 0 && (key.GroupID == nil || *key.GroupID != owner.GroupID) {
		historical := *key
		group, groupErr := keys.groupRepo.GetByID(ctx, owner.GroupID)
		if groupErr != nil {
			return nil, mc.E("billing_pending", "original billing group is unavailable", 503)
		}
		historical.Group = group
		groupID := owner.GroupID
		historical.GroupID = &groupID
		key = &historical
	}
	a, err := s.contextAccount(ctx, key, owner.AccountID, model, false)
	if err != nil {
		return nil, err
	}
	owner.AccountID = a.ID
	profile := fusion.Profile{PublicModel: model, LeadModel: model, SidekickModel: model, MaxContextBytes: p.MaxContextBytes, MaxOutputTokens: max(p.MaxOutputTokens, p.SummaryOutputTokens), MaxWallSeconds: p.WallSeconds}
	x := &devinFusionExecutor{attemptPrefix: "context", service: s, account: a, key: key, updater: keys, profile: profile, sessionID: session, publicModel: model, reservations: map[string]*InflightReservation{}, holdUnsettled: map[string]bool{}}
	if c != nil {
		x.inboundEndpoint = c.Request.URL.Path
		x.userAgent = c.Request.UserAgent()
		x.ipAddress = c.ClientIP()
	}
	if owner.SubscriptionID > 0 {
		if s.userSubRepo == nil {
			return nil, mc.E("billing_pending", "original subscription is unavailable", 503)
		}
		x.subscription, err = s.userSubRepo.GetByID(ctx, owner.SubscriptionID)
		if err != nil {
			return nil, err
		}
	}
	if inference {
		if !a.IsSchedulable() {
			return nil, mc.E("account_bound", "bound account is currently unavailable", 409)
		}
		x.models, err = s.devin.Models(ctx, a)
		if err != nil {
			return nil, err
		}
		selected := devin.FindModel(x.models, model)
		if selected == nil || devin.PlanModel(selected).Mode != devin.RouteDirect || selected.GetMaxTokens() == 0 {
			return nil, mc.E("model_unavailable", "managed context requires an authorized concrete model with a known window", 400)
		}
		if features := selected.GetModelInfo().GetModelFeatures(); features == nil || !features.GetSupportsToolCalls() {
			return nil, mc.E("model_unavailable", "model does not support the managed function-tool contract", 400)
		}
		if maxOutput := int(selected.GetModelInfo().GetMaxOutputTokens()); maxOutput > 0 {
			x.profile.MaxOutputTokens = min(x.profile.MaxOutputTokens, maxOutput)
		}
		x.client, err = s.devin.Client(ctx, a)
		if err != nil {
			return nil, err
		}
	}
	return &contextExecutor{physical: x, keys: keys, owner: owner}, nil
}

type ContextRunResult struct {
	Response  *apicompat.ResponsesResponse
	Compact   *mc.CompactResult
	Job       *mc.Job
	SessionID string
	Revision  int64
	Replayed  bool
}

func (s *OpenAIGatewayService) RunContext(c *gin.Context, key *APIKey, keys *APIKeyService, body []byte, mode, session string, expected int64, target int) (*ContextRunResult, error) {
	ctx := devin.WithClientIdentity(c.Request.Context())
	manual := mode != "responses"
	if err := s.CheckContextAccess(key, ""); err != nil {
		return nil, err
	}
	engine, err := s.ContextEngine()
	if err != nil {
		return nil, err
	}
	policy := s.contextPolicy()
	var parsed mc.Parsed
	model := ""
	if mode == "internal" {
		existing, _, err := engine.LoadSession(ctx, contextScope(key), session)
		if err != nil {
			return nil, err
		}
		model = existing.Model
		policy = existing.Policy
		parsed = mc.Parsed{Request: &existing.Template}
	} else {
		parsed, err = mc.Parse(body, mode == "stateless")
		if err != nil {
			return nil, err
		}
		model = parsed.Request.Model
	}
	if err = s.CheckContextAccess(key, model); err != nil {
		return nil, err
	}
	if devin.PlanModelUID(model).Mode != devin.RouteDirect {
		return nil, mc.E("model_unavailable", "select a concrete model UID for managed context", 400)
	}
	if mode == "responses" && c.GetHeader(mc.HeaderMode) != "managed" {
		return nil, mc.E("input_invalid", "X-Sub2API-Context-Mode must be managed", 400)
	}
	if mode == "responses" && ((session == "") != (parsed.Request.PreviousResponseID == "")) {
		return nil, mc.E("lineage_conflict", "continuation requires both context session and previous_response_id", 409)
	}
	owner := mc.Owner{Scope: contextScope(key), UserID: key.UserID, KeyID: key.ID}
	if key.GroupID != nil {
		owner.GroupID = *key.GroupID
	}
	if v, ok := c.Get("subscription"); ok {
		if sub, ok := v.(*UserSubscription); ok && sub != nil {
			owner.SubscriptionID = sub.ID
		}
	}
	namespace := mode + ":" + session
	fingerprint := cc.Digest(struct {
		Body     json.RawMessage
		Session  string
		Expected int64
		Target   int
	}{body, session, expected, target})
	op, _, err := engine.Begin(ctx, owner, namespace, c.GetHeader("Idempotency-Key"), fingerprint, model, session, policy)
	if err != nil {
		return nil, err
	}
	out := new(ContextRunResult)
	err = engine.WithOperation(ctx, op, func(ctx context.Context, op *mc.Operation, r *mc.Record, l *mc.Lease) error {
		// Published idempotent results need no fresh token/model discovery or slots.
		if mode == "responses" {
			response, err := engine.RecoverPublished(ctx, op, r, l)
			if err != nil {
				return err
			}
			if response != nil {
				out.Response = response
				out.SessionID = op.SessionID
				out.Replayed = true
				if _, sr, readErr := engine.LoadSession(ctx, op.Scope, op.SessionID); readErr == nil {
					out.Revision = sr.Revision
				}
				return nil
			}
			if op.State != "uncertain" {
				if response, err = engine.Response(ctx, op); response != nil || err != nil {
					out.Response = response
					out.SessionID = op.SessionID
					out.Replayed = true
					if _, sr, readErr := engine.LoadSession(ctx, op.Scope, op.SessionID); readErr == nil {
						out.Revision = sr.Revision
					}
					return err
				}
			}
		}
		if mode == "stateless" && op.State != "uncertain" {
			if result, err := engine.CompactResponse(ctx, op); result != nil || err != nil {
				out.Compact = result
				out.Replayed = true
				return err
			}
		}
		if op.SessionID != "" {
			sess, _, err := engine.LoadSession(ctx, owner.Scope, op.SessionID)
			if err != nil {
				return err
			}
			owner = sess.Owner
			policy = sess.Policy
		}
		if op.AccountID > 0 {
			owner = op.Owner
		}
		if op.AccountID > 0 {
			recovery, err := s.contextPhysical(ctx, key, keys, owner, model, op.SessionID, policy, c, false)
			if err != nil {
				return err
			}
			defer recovery.physical.Close()
			if err = engine.ReconcileKnown(ctx, op, r, l, recovery); err != nil {
				return err
			}
		}
		account, err := s.contextAccount(ctx, key, owner.AccountID, model, manual)
		if err != nil {
			return err
		}
		if _, err = s.checkContextAccountAccess(key, account, model, manual); err != nil {
			return err
		}
		owner.AccountID = account.ID
		if s.concurrencyService == nil {
			return mc.E("unavailable", "shared inference concurrency is unavailable", 503)
		}
		userSlot, err := s.concurrencyService.AcquireUserSlot(ctx, key.UserID, key.User.Concurrency)
		if err != nil {
			return err
		}
		if userSlot == nil || !userSlot.Acquired {
			return mc.E("busy", "user inference concurrency is full", 503)
		}
		defer userSlot.ReleaseFunc()
		accountSlot, err := s.concurrencyService.AcquireAccountSlot(ctx, account.ID, account.Concurrency)
		if err != nil {
			return err
		}
		if accountSlot == nil || !accountSlot.Acquired {
			return mc.E("busy", "bound account inference concurrency is full", 503)
		}
		defer accountSlot.ReleaseFunc()
		x, err := s.contextPhysical(ctx, key, keys, owner, model, op.SessionID, policy, c, true)
		if err != nil {
			return err
		}
		defer x.physical.Close()
		x.manual = manual
		if err = engine.Bind(ctx, op, r, l, owner, op.SessionID); err != nil {
			return err
		}
		switch mode {
		case "responses":
			result, err := engine.Advance(ctx, op, r, l, parsed, x)
			if result != nil {
				out.Response = result.Response
				out.SessionID = result.SessionID
				out.Revision = result.Revision
				out.Replayed = result.Replayed
			}
			return err
		case "stateless":
			out.Compact, err = engine.Stateless(ctx, op, r, l, parsed, target, x)
			if op.JobID != "" {
				out.Job = &mc.Job{ID: op.JobID}
			}
			return err
		case "internal":
			out.Job, err = engine.Internal(ctx, op, r, l, expected, x)
			out.SessionID = op.SessionID
			return err
		default:
			return mc.E("input_invalid", "unsupported context operation", 400)
		}
	})
	return out, err
}

func (x *contextExecutor) BindSession(id string) { x.physical.sessionID = id }

func ContextScope(key *APIKey) string { return contextScope(key) }
func (s *OpenAIGatewayService) CheckContextModelAccess(key *APIKey, model string) error {
	if key == nil || key.Group == nil || key.Group.Platform != PlatformDevin || key.Group.ModelAllowlistEnabled() && !key.Group.ModelAllowlist.Allows(model) {
		return mc.E("model_unavailable", "model is not available for this group", 404)
	}
	return nil
}
func WriteContextResponse(c *gin.Context, r *apicompat.ResponsesResponse, stream bool) error {
	return writeDevinFusionResponse(c, r, "responses", stream, apicompat.ResponsesClientToolMapping{})
}

func contextBillingPurpose(p string) string {
	if strings.HasPrefix(p, "compaction") {
		return "compaction"
	}
	return p
}
