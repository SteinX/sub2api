package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
)

type devinFusionBindingKey struct{}
type devinFusionBinding struct {
	SessionID string
	AccountID int64
	Profile   *fusion.Profile
	Updater   APIKeyQuotaUpdater
}

func DevinFusionRequest(ctx context.Context) bool {
	b, _ := ctx.Value(devinFusionBindingKey{}).(*devinFusionBinding)
	return b != nil
}
func devinFusionBoundAccount(ctx context.Context) int64 {
	b, _ := ctx.Value(devinFusionBindingKey{}).(*devinFusionBinding)
	if b != nil {
		return b.AccountID
	}
	return 0
}

func devinFusionProfiles(a *Account) ([]fusion.Profile, error) {
	if a == nil || a.Platform != PlatformDevin || a.Extra == nil || a.Extra[fusion.ExtraProfiles] == nil {
		return nil, nil
	}
	b, err := json.Marshal(a.Extra[fusion.ExtraProfiles])
	if err != nil {
		return nil, err
	}
	var profiles []fusion.Profile
	if json.Unmarshal(b, &profiles) != nil || len(profiles) > 16 {
		return nil, fusion.E("profile_invalid", "devin_fusion_profiles must contain at most 16 profiles", 400)
	}
	seen := map[string]bool{}
	for _, p := range profiles {
		if err = p.Validate(); err != nil {
			return nil, err
		}
		name := strings.ToLower(p.PublicModel)
		if seen[name] {
			return nil, fusion.E("profile_invalid", "Fusion public model names must be unique", 400)
		}
		seen[name] = true
	}
	return profiles, nil
}
func devinFusionEnabled(a *Account) bool {
	if a == nil || a.Platform != PlatformDevin {
		return false
	}
	enabled, exists := a.Extra[fusion.ExtraEnabled]
	return !exists || enabled == true
}
func devinFusionCatalog(a *Account) []*pb.ClientModelConfig {
	if snapshot := devinAccountCatalog(a); snapshot != nil && len(snapshot.Models) > 0 {
		return snapshot.Models
	}
	var models []*pb.ClientModelConfig
	for _, id := range a.devinModelIDs() {
		models = append(models, &pb.ClientModelConfig{ModelUid: id})
	}
	return models
}
func devinFusionProfile(a *Account, model string) *fusion.Profile {
	if !devinFusionEnabled(a) || !strings.HasPrefix(strings.ToLower(model), "fusion") {
		return nil
	}
	profiles, err := devinFusionProfiles(a)
	if err != nil {
		return nil
	}
	for _, p := range profiles {
		if strings.EqualFold(p.PublicModel, model) {
			if p.Enabled {
				return &p
			}
			return nil
		}
	}
	p, err := fusion.InferProfile(model, devinFusionCatalog(a))
	if err != nil {
		return nil
	}
	return p
}
func devinEffectiveModelRoutes(models []*pb.ClientModelConfig, a *Account) []devin.ModelRoutePlan {
	if a == nil {
		a = &Account{Platform: PlatformDevin, Extra: map[string]any{devinCatalogExtraKey: &devin.CatalogSnapshot{Models: models}}}
	} else {
		clone := *a
		clone.Extra = maps.Clone(a.Extra)
		if clone.Extra == nil {
			clone.Extra = map[string]any{}
		}
		clone.Extra[devinCatalogExtraKey] = &devin.CatalogSnapshot{Models: models}
		a = &clone
	}
	routes := make([]devin.ModelRoutePlan, 0, len(models))
	configured, _ := devinFusionProfiles(a)
	for _, m := range models {
		plan := devin.PlanModel(m)
		if p := devinFusionProfile(a, devin.ModelID(m)); p != nil && validateDevinFusionMembers(*p, models) == nil {
			plan.Mode = fusion.RouteMode
			plan.CatalogKind = devin.KindPairing
			plan.DecisionCode = ""
			plan.MechanismStatus = "gateway_orchestrated"
			plan.LeadModelUID = p.LeadModel
			plan.SidekickModelUID = p.SidekickModel
			plan.PairSource = "model_id"
			for _, override := range configured {
				if override.PublicModel == p.PublicModel {
					plan.PairSource = "configured"
					break
				}
			}
		}
		routes = append(routes, plan)
	}
	return routes
}
func devinEffectiveModelIDs(models []*pb.ClientModelConfig, a *Account) []string {
	var ids []string
	for _, p := range devinEffectiveModelRoutes(models, a) {
		if p.Mode != devin.RouteBlocked {
			ids = append(ids, p.ModelUID)
		}
	}
	return ids
}

// DevinExtraForView refreshes derived routing metadata without changing stored
// account settings. Existing accounts immediately show the default-on policy.
func DevinExtraForView(a *Account) map[string]any {
	if a == nil {
		return nil
	}
	if a.Platform != PlatformDevin {
		return a.Extra
	}
	catalog := devinAccountCatalog(a)
	if catalog == nil {
		return a.Extra
	}
	copy := *catalog
	copy.Routes = devinEffectiveModelRoutes(catalog.Models, a)
	copy.PolicyRevision = devin.RoutePolicyRevision
	extra := maps.Clone(a.Extra)
	extra[devinCatalogExtraKey] = &copy
	return extra
}

func validateDevinFusionMembers(p fusion.Profile, models []*pb.ClientModelConfig) error {
	for _, uid := range []string{p.LeadModel, p.SidekickModel} {
		m := devin.FindModel(models, uid)
		if m == nil || devin.PlanModel(m).Mode != devin.RouteDirect {
			return fusion.E("member_unavailable", "both Fusion members must be available concrete models without opaque harnesses", 400)
		}
		if f := m.GetModelInfo().GetModelFeatures(); f != nil && !f.GetSupportsToolCalls() {
			return fusion.E("member_unavailable", "both Fusion members must support function tools", 400)
		}
	}
	return nil
}
func (s *OpenAIGatewayService) devinFusionAccounts(ctx context.Context, groupID *int64) ([]Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, nil
	}
	if s.schedulerSnapshot != nil {
		accounts, _, err := s.schedulerSnapshot.ListSchedulableAccounts(ctx, groupID, PlatformDevin, true)
		return accounts, err
	}
	if groupID != nil {
		return s.accountRepo.ListSchedulableByGroupID(ctx, *groupID)
	}
	return s.accountRepo.ListSchedulable(ctx)
}
func (s *OpenAIGatewayService) HasDevinFusionProfile(ctx context.Context, groupID *int64, model string) bool {
	if s != nil && s.channelService != nil {
		mapping, _ := s.ResolveChannelMappingAndRestrict(ctx, groupID, model)
		if mapping.Mapped {
			model = mapping.MappedModel
		}
	}
	accounts, err := s.devinFusionAccounts(ctx, groupID)
	if err != nil {
		return false
	}
	for i := range accounts {
		a := &accounts[i]
		if devinFusionProfile(a, a.GetMappedModel(model)) != nil {
			return true
		}
	}
	return false
}

func parseDevinFusionRequest(body []byte, protocol string) (*apicompat.ResponsesRequest, apicompat.ResponsesClientToolMapping, error) {
	var req *apicompat.ResponsesRequest
	var mapping apicompat.ResponsesClientToolMapping
	var err error
	switch protocol {
	case "anthropic":
		var r apicompat.AnthropicRequest
		if err = json.Unmarshal(body, &r); err == nil {
			req, err = apicompat.AnthropicToResponsesWithMinOutputTokens(&r, 1)
		}
	case "chat":
		var r apicompat.ChatCompletionsRequest
		if err = json.Unmarshal(body, &r); err == nil {
			req, err = apicompat.ChatCompletionsToResponsesWithMinOutputTokens(&r, 1)
		}
	default:
		var raw map[string]any
		if err = json.Unmarshal(body, &raw); err == nil {
			mapping, _, err = apicompat.AdaptResponsesClientTools(raw)
			if err == nil && mapping.ToolSearch {
				err = fusion.E("tools_unsupported", "dynamic tool discovery changes the Fusion contract; supply a fixed tool set", 400)
			}
			if err == nil {
				var b []byte
				b, err = json.Marshal(raw)
				if err == nil {
					req = &apicompat.ResponsesRequest{}
					err = json.Unmarshal(b, req)
				}
			}
		}
	}
	if err != nil {
		return nil, mapping, err
	}
	var stream struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &stream)
	req.Stream = stream.Stream
	return req, mapping, nil
}

// BindDevinFusionRequest runs before account selection. It restores actual
// protocol lineage and pins the scheduler to the session's original account.
func (s *OpenAIGatewayService) BindDevinFusionRequest(c *gin.Context, key *APIKey, model string, body []byte, protocol string, updater APIKeyQuotaUpdater) (bool, error) {
	if s == nil || s.devin == nil || key == nil {
		return false, nil
	}
	ctx := c.Request.Context()
	explicitSession := c.GetHeader(fusion.HeaderSession)
	configured := s.HasDevinFusionProfile(ctx, key.GroupID, model)
	hint := strings.HasPrefix(strings.ToLower(model), "fusion") || explicitSession != "" || strings.Contains(string(body), "call_fusion_") || strings.Contains(string(body), "resp_fusion_")
	if !configured && !hint {
		return false, nil
	}
	// Native client headers correlate ordinary model requests too. Adopt them
	// only after identifying Fusion intent; they must not opt normal tool turns
	// into Fusion's separate tool ownership and session validation.
	if explicitSession == "" {
		if clientSession := ExtractClientSessionID(c); clientSession != "" {
			explicitSession = "client_" + fusion.Digest(clientSession)[:48]
		}
	}
	req, _, err := parseDevinFusionRequest(body, protocol)
	if err != nil {
		return true, fusion.E("input_invalid", err.Error(), 400)
	}
	engine := s.devin.fusionEngine()
	scope := fusion.Scope(key.UserID, key.ID)
	id, accountID, err := engine.Resolve(ctx, scope, explicitSession, c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		return true, err
	}
	if !configured && accountID == 0 {
		return false, nil
	}
	binding := &devinFusionBinding{SessionID: id, AccountID: accountID, Updater: updater}
	if accountID > 0 {
		session, _, err := engine.Load(ctx, scope, id)
		if err != nil {
			return true, err
		}
		binding.Profile = &session.Profile
		if session.Role == fusion.Lead && engine.Cache != nil {
			_ = engine.Cache.Stop(ctx, session, "foreground_requested")
		}
	}
	c.Request = c.Request.WithContext(context.WithValue(ctx, devinFusionBindingKey{}, binding))
	if id != "" {
		c.Header(fusion.HeaderSession, id)
	}
	if c.GetHeader(fusion.HeaderCancel) != "" {
		if c.GetHeader(fusion.HeaderCancel) != "true" || accountID == 0 {
			return true, fusion.E("cancel_invalid", "cancel requires true and an existing scoped session", 400)
		}
		if err = engine.Cancel(ctx, scope, id); err != nil {
			return true, err
		}
		c.JSON(200, gin.H{"object": "fusion.session", "id": id, "status": "cancel_requested"})
		return true, errDevinFusionHandled
	}
	return true, nil
}

var errDevinFusionHandled = errors.New("fusion control request handled")

func DevinFusionHandled(err error) bool { return errors.Is(err, errDevinFusionHandled) }

func WriteDevinFusionError(c *gin.Context, protocol string, err error) bool {
	var typed *fusion.Error
	if !errors.As(err, &typed) {
		return false
	}
	typ := "invalid_request_error"
	if typed.Status >= 500 {
		typ = "api_error"
	} else {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalModelConfiguration)
	}
	c.Set(OpsLocalErrorTypeKey, typ)
	c.Set(OpsLocalErrorCodeKey, typed.Code)
	c.Set(ResponseCommittedKey, true)
	event := any(gin.H{"error": gin.H{"type": typ, "code": typed.Code, "message": typed.Message}})
	if protocol == "anthropic" {
		event = gin.H{"type": "error", "error": gin.H{"type": typ, "message": typed.Error()}}
	}
	if c.Writer.Written() {
		MarkOpsStreamErrorValue(c, OpsStreamError{ErrType: typ, Code: typed.Code, Message: typed.Message, IntendedStatus: typed.Status, CountTowardsSLA: typed.Status >= 500, RequestScoped: typed.Status < 500})
		if protocol == "responses" {
			event = gin.H{"type": "error", "code": typed.Code, "message": typed.Message}
		}
		b, _ := json.Marshal(event)
		if protocol == "chat" {
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", b)
		} else {
			_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", b)
		}
		c.Writer.Flush()
	} else {
		// Buffered internal stages may have prepared SSE headers without sending
		// anything. Until the stream is committed, errors remain ordinary JSON.
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(typed.Status, event)
	}
	return true
}

// ValidateDevinFusionExtra validates admin writes before persistence. Runtime
// still refreshes and verifies both model grants before a paid call.
func ValidateDevinFusionExtra(platform string, extra map[string]any) error {
	if extra == nil {
		return nil
	}
	if enabled, exists := extra[fusion.ExtraEnabled]; exists {
		if platform != PlatformDevin {
			return infraerrors.BadRequest("INVALID_FUSION_PROFILE", "Fusion is available on Devin accounts")
		}
		if _, ok := enabled.(bool); !ok {
			return infraerrors.BadRequest("INVALID_FUSION_PROFILE", "devin_fusion_enabled must be boolean")
		}
	}
	if extra[fusion.ExtraProfiles] == nil {
		return nil
	}
	if platform != PlatformDevin {
		return infraerrors.BadRequest("INVALID_FUSION_PROFILE", "Fusion profiles are available on Devin accounts")
	}
	a := &Account{Platform: platform, Extra: extra}
	profiles, err := devinFusionProfiles(a)
	if err != nil {
		return infraerrors.BadRequest("INVALID_FUSION_PROFILE", err.Error())
	}
	for _, p := range profiles {
		if !p.Enabled {
			continue
		}
		catalog := devinAccountCatalog(a)
		if catalog == nil {
			return infraerrors.BadRequest("INVALID_FUSION_PROFILE", "sync the account model catalog before enabling Fusion")
		}
		if err = validateDevinFusionMembers(p, catalog.Models); err != nil {
			return infraerrors.BadRequest("INVALID_FUSION_PROFILE", err.Error())
		}
	}
	return nil
}
