package devin

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"google.golang.org/protobuf/proto"
)

const (
	RoutePolicyRevision   = "devin-routes-v2-auto-fusion"
	CatalogSchemaRevision = "cascade-model-info-harness-20-v1"

	KindDirect         = "direct_model"
	KindRouter         = "router_candidate"
	KindHarness        = "harness_backed_candidate"
	KindPairing        = "client_pairing_candidate"
	KindUnknownSpecial = "unknown_special"

	RouteDirect  = "direct_chat"
	RouteAssign  = "assign_then_chat"
	RouteBlocked = "blocked"

	CodeFusionUnsupported = "devin_fusion_orchestration_unsupported"
	CodeRouteUnverified   = "devin_model_route_unverified"
	CodeAssignmentHarness = "devin_assignment_harness_unsupported"
	CodeRouteMismatch     = "devin_upstream_route_mismatch"
	CodeModelUnavailable  = "devin_model_unavailable"
	CodeModelCapability   = "devin_model_capability_unsupported"
)

// RouteError is a terminal route/capability decision, never an account or auth
// failure. Its messages contain no upstream payload, credentials or user content.
type RouteError struct {
	Code string
}

func (e *RouteError) Error() string   { return e.Code + ": " + e.Message() }
func (e *RouteError) Retryable() bool { return false }
func (e *RouteError) Origin() string {
	if e.Code == CodeRouteMismatch {
		return "upstream_contract"
	}
	return "gateway_policy"
}
func (e *RouteError) HTTPStatus() int {
	if e.Code == CodeRouteMismatch {
		return 502
	}
	return 400
}
func (e *RouteError) Message() string {
	switch e.Code {
	case CodeFusionUnsupported:
		return "This Fusion model ID could not be resolved to an available Lead/Sidekick pair. Sync the account catalog or configure a pair override. No generation request was sent."
	case CodeRouteUnverified:
		return "This model requires an unverified execution route. No generation request was sent."
	case CodeAssignmentHarness:
		return "The assigned model requires an unsupported harness. No generation request was sent."
	case CodeRouteMismatch:
		return "Devin reported an execution route that conflicts with the approved route."
	case CodeModelUnavailable:
		return "This Devin account does not advertise an available route for the requested model."
	case CodeModelCapability:
		return "The final Devin model does not support the requested output limit, images or tools. No generation request was sent."
	default:
		return "The Devin model route is unsupported."
	}
}

// ModelRoutePlan describes transport support, not proof of server-side Fusion.
// Plans are recomputed from catalog metadata, never cached as capability grants.
type ModelRoutePlan struct {
	ModelUID         string   `json:"model_uid"`
	CatalogKind      string   `json:"catalog_kind"`
	Mode             string   `json:"route_mode"`
	LeadModelUID     string   `json:"lead_model_uid,omitempty"`
	SidekickModelUID string   `json:"sidekick_model_uid,omitempty"`
	PairSource       string   `json:"pair_source,omitempty"`
	HarnessUIDs      []string `json:"harness_uids,omitempty"`
	DecisionCode     string   `json:"decision_code,omitempty"`
	MechanismStatus  string   `json:"mechanism_status"`
}

func (p ModelRoutePlan) Validate() error {
	if p.Mode == RouteBlocked {
		return &RouteError{Code: p.DecisionCode}
	}
	if p.ModelUID == "" || (p.Mode != RouteDirect && p.Mode != RouteAssign) {
		return &RouteError{Code: CodeRouteMismatch}
	}
	return nil
}

// UID classification only identifies special candidates. It never extracts a
// Lead, invents a pairing relation or permits a special UID based on its name.
func PlanModelUID(uid string) ModelRoutePlan {
	p := ModelRoutePlan{ModelUID: uid, CatalogKind: KindDirect, Mode: RouteDirect, MechanismStatus: "not_requested"}
	normalized := strings.ToLower(strings.TrimSpace(uid))
	if strings.HasPrefix(normalized, "fusion") {
		p.CatalogKind, p.Mode, p.MechanismStatus, p.DecisionCode = KindUnknownSpecial, RouteBlocked, "unverified", CodeRouteUnverified
		if strings.HasPrefix(normalized, "fusion-") {
			lead, sidekick, paired := strings.Cut(strings.TrimPrefix(normalized, "fusion-"), "-sidekick-")
			if paired && lead != "" && sidekick != "" {
				p.CatalogKind, p.DecisionCode = KindPairing, CodeFusionUnsupported
			}
		}
	}
	if uid == "" {
		p.Mode, p.DecisionCode = RouteBlocked, CodeModelUnavailable
	}
	return p
}

func PlanModel(m *pb.ClientModelConfig) ModelRoutePlan {
	p := PlanModelUID(ModelID(m))
	info := m.GetModelInfo()
	p.HarnessUIDs = append([]string(nil), info.GetHarnessUids()...)
	if m == nil || m.GetDisabled() {
		p.Mode, p.DecisionCode = RouteBlocked, CodeModelUnavailable
		return p
	}
	// A special identity must not be hidden in one of the alternate UID fields.
	for _, uid := range []string{info.GetModelUid(), m.GetModelOrAlias().GetModelUid()} {
		if alternate := PlanModelUID(uid); uid != "" && alternate.Mode == RouteBlocked {
			p.CatalogKind, p.Mode, p.DecisionCode, p.MechanismStatus = alternate.CatalogKind, RouteBlocked, alternate.DecisionCode, "unverified"
		}
	}
	if p.Mode == RouteBlocked {
		return p
	}
	if len(p.HarnessUIDs) > 0 {
		p.CatalogKind, p.Mode, p.DecisionCode, p.MechanismStatus = KindHarness, RouteBlocked, CodeRouteUnverified, "unverified"
	} else if label := strings.ToLower(strings.TrimSpace(m.GetLabel())); label == "fusion" || strings.HasPrefix(label, "fusion ") || strings.HasPrefix(label, "fusion-") {
		p.CatalogKind, p.Mode, p.DecisionCode, p.MechanismStatus = KindUnknownSpecial, RouteBlocked, CodeRouteUnverified, "unverified"
	} else if info.GetIsModelRouter() || info.GetDisplayOption() == pb.DisplayOption_DISPLAY_OPTION_MODEL_ROUTER {
		p.CatalogKind, p.Mode = KindRouter, RouteAssign
	}
	return p
}

func FindModel(models []*pb.ClientModelConfig, uid string) *pb.ClientModelConfig {
	for _, m := range models {
		if ModelID(m) == uid {
			return m
		}
	}
	return nil
}

// CheckModelIntent runs on every visible mapping step as well as its target.
// An alias is not consent to silently replace Fusion with a single Lead.
func CheckModelIntent(models []*pb.ClientModelConfig, uids ...string) error {
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		p := PlanModelUID(uid)
		if m := FindModel(models, uid); m != nil {
			p = PlanModel(m)
		}
		if err := p.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ValidateAssignment checks the execution contract before mutating the request.
// Dynamic ordinary targets need not have their own catalog row. A second router
// or any special target is not recursively assigned or reduced to a Lead.
func ValidateAssignment(a *pb.ModelAssignment, models []*pb.ClientModelConfig) error {
	if len(a.GetHarnessUids()) > 0 {
		return &RouteError{Code: CodeAssignmentHarness}
	}
	if a.GetModelUid() == "" || a.GetAssignmentJwt() == "" {
		return &RouteError{Code: CodeRouteMismatch}
	}
	p := PlanModelUID(a.ModelUid)
	if m := FindModel(models, a.ModelUid); m != nil {
		p = PlanModel(m)
	}
	if p.Mode != RouteDirect {
		return &RouteError{Code: CodeRouteMismatch}
	}
	return nil
}

// CatalogSnapshot retains advertised metadata for reclassification and admin
// diagnosis; Routes is a display projection, not an authorization source.
type CatalogSnapshot struct {
	Revision         string                  `json:"revision"`
	SchemaRevision   string                  `json:"schema_revision"`
	PolicyRevision   string                  `json:"policy_revision"`
	IdentityRevision string                  `json:"identity_revision"`
	Models           []*pb.ClientModelConfig `json:"models"`
	Routes           []ModelRoutePlan        `json:"routes"`
}

func CloneModels(models []*pb.ClientModelConfig) []*pb.ClientModelConfig {
	if models == nil {
		return nil
	}
	cloned := make([]*pb.ClientModelConfig, len(models))
	for i, m := range models {
		if m != nil {
			cloned[i] = proto.CloneOf(m)
		}
	}
	return cloned
}

func NewCatalogSnapshot(models []*pb.ClientModelConfig, identityRevision string) *CatalogSnapshot {
	s := &CatalogSnapshot{SchemaRevision: CatalogSchemaRevision, PolicyRevision: RoutePolicyRevision, IdentityRevision: identityRevision, Models: CloneModels(models), Routes: make([]ModelRoutePlan, 0, len(models))}
	s.Revision = CatalogRevision(s.Models)
	for _, m := range s.Models {
		s.Routes = append(s.Routes, PlanModel(m))
	}
	return s
}

func CatalogRevision(models []*pb.ClientModelConfig) string {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(&pb.GetCliModelConfigsResponse{ClientModelConfigs: models})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

func AvailableModelIDs(models []*pb.ClientModelConfig) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if p := PlanModel(m); p.Validate() == nil {
			ids = append(ids, p.ModelUID)
		}
	}
	return ids
}

// ValidateModelCapabilities is run both for the selected entry and for a known
// assigned target. A dynamic target inherits its router's advertised contract;
// absence from the directory alone does not make an ordinary assignment invalid.
func ValidateModelCapabilities(req *pb.GetChatMessageRequest, model *pb.ClientModelConfig, requestedMax *int) error {
	if model == nil {
		return nil
	}
	if limit := model.GetModelInfo().GetMaxOutputTokens(); limit > 0 {
		if requestedMax != nil && *requestedMax > int(limit) {
			return &RouteError{Code: CodeModelCapability}
		}
		if req.GetConfiguration().GetMaxTokens() > uint64(limit) {
			req.Configuration.MaxTokens = uint64(limit)
		}
	}
	features := model.GetModelInfo().GetModelFeatures()
	for _, p := range req.ChatMessagePrompts {
		if len(p.Images) > 0 && !model.GetSupportsImages() && !features.GetSupportsImages() {
			return &RouteError{Code: CodeModelCapability}
		}
	}
	if features != nil && len(req.Tools) > 0 && !features.GetSupportsToolCalls() {
		return &RouteError{Code: CodeModelCapability}
	}
	return nil
}
