package devin

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

const MaxModelObservations = 16
const MaxUsageObservations = 32

// DiagnosticModelID bounds untrusted identifiers without logging arbitrary
// content. Long or malformed values become an opaque, non-credential digest.
func DiagnosticModelID(s string) string {
	if s == "" {
		return ""
	}
	valid := len(s) <= 128
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.' && r != '/' && r != ':' && r != '@' && r != '+' {
			valid = false
			break
		}
	}
	if valid {
		return s
	}
	return "id_" + diagnosticRef(s)
}

func diagnosticRef(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

type UsageObservation struct {
	Sequence         int    `json:"sequence"`
	MessageRef       string `json:"message_ref,omitempty"`
	Model            string `json:"model,omitempty"`
	RequestedModel   string `json:"requested_model,omitempty"`
	BillingModel     string `json:"billing_model,omitempty"`
	InputTokens      uint64 `json:"input_tokens"`
	OutputTokens     uint64 `json:"output_tokens"`
	CacheReadTokens  uint64 `json:"cache_read_tokens"`
	CacheWriteTokens uint64 `json:"cache_write_tokens"`
}

// ModelEvidence observes identities separately from the existing terminal usage
// snapshot used for billing. Repeated snapshots are never summed here.
type ModelEvidence struct {
	ActualModels []string           `json:"actual_models,omitempty"`
	Usage        []UsageObservation `json:"usage_snapshots,omitempty"`
	Truncated    bool               `json:"truncated"`
	usageCount   int
}

func (e *ModelEvidence) Observe(m *pb.GetChatMessageResponse) {
	if model := DiagnosticModelID(m.GetActualModelUid()); model != "" && !slices.Contains(e.ActualModels, model) {
		if len(e.ActualModels) < MaxModelObservations {
			e.ActualModels = append(e.ActualModels, model)
		} else {
			e.Truncated = true
		}
	}
	if u := m.GetUsage(); u != nil {
		e.usageCount++
		ref := u.MessageId
		if ref == "" {
			ref = m.MessageId
		}
		observation := UsageObservation{Sequence: e.usageCount, MessageRef: diagnosticRef(ref), Model: DiagnosticModelID(u.ModelUid), RequestedModel: DiagnosticModelID(u.RequestedModelUid), BillingModel: DiagnosticModelID(u.BillingModelUid), InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens}
		if len(e.Usage) < MaxUsageObservations {
			e.Usage = append(e.Usage, observation)
		} else {
			// Keep the terminal observation as well as the bounded initial history.
			e.Usage[len(e.Usage)-1], e.Truncated = observation, true
		}
	}
}

type RouteMapping struct {
	Stage string `json:"stage"`
	From  string `json:"from"`
	To    string `json:"to"`
}

type RouteAttempt struct {
	Number              int      `json:"number"`
	AssignmentUsed      bool     `json:"assignment_used"`
	AssignedModel       string   `json:"assigned_model,omitempty"`
	AssignedHarnessUIDs []string `json:"assigned_harness_uids,omitempty"`
	WireModel           string   `json:"wire_model,omitempty"`
	GenerationAttempted bool     `json:"generation_attempted"`
}

// ModelRouteTrace is internal diagnostic data, never added to standard model
// response bodies. No prompt, tool payload, JWT, account host or session ID fits
// in this type. The admin system-log sink applies its existing access/retention.
type ModelRouteTrace struct {
	TraceID                    string         `json:"trace_id"`
	ClientRequestedModel       string         `json:"client_requested_model"`
	GatewayEntryModel          string         `json:"gateway_entry_model"`
	MappedModel                string         `json:"mapped_model"`
	MappingSteps               []RouteMapping `json:"mapping_steps,omitempty"`
	CatalogRevision            string         `json:"catalog_revision,omitempty"`
	SchemaRevision             string         `json:"schema_revision"`
	PolicyRevision             string         `json:"policy_revision"`
	IdentityRevision           string         `json:"identity_revision"`
	CatalogKind                string         `json:"catalog_kind,omitempty"`
	RouteMode                  string         `json:"route_mode"`
	CatalogHarnessUIDs         []string       `json:"catalog_harness_uids,omitempty"`
	DecisionCode               string         `json:"decision_code,omitempty"`
	DecisionOrigin             string         `json:"decision_origin,omitempty"`
	DecisionPhase              string         `json:"decision_phase"`
	ClientRequestedModelSource string         `json:"client_requested_model_source"`
	MechanismStatus            string         `json:"mechanism_status"`
	CapabilitySource           string         `json:"capability_source,omitempty"`
	Attempts                   []RouteAttempt `json:"attempts,omitempty"`
	Evidence                   ModelEvidence  `json:"evidence"`
	BillingModel               string         `json:"billing_model,omitempty"`
	BillingSource              string         `json:"billing_source,omitempty"`
	Outcome                    string         `json:"outcome"`
}

func DiagnosticIDs(ids []string) []string {
	result := make([]string, 0, min(len(ids), MaxModelObservations))
	for _, id := range ids {
		if len(result) == MaxModelObservations {
			break
		}
		result = append(result, DiagnosticModelID(id))
	}
	return result
}
