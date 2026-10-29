// Package fusion implements gateway-owned Lead/Sidekick collaboration. It does
// not interpret Devin's opaque harnesses or run tools on the gateway host.
package fusion

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
)

const PolicyVersion = "gateway-fusion-v1"
const DelegateTool = "sub2api_sidekick"
const HeaderSession = "X-Sub2API-Fusion-Session"
const HeaderCancel = "X-Sub2API-Fusion-Cancel"
const ExtraProfiles = "devin_fusion_profiles"

type Profile struct {
	Enabled         bool              `json:"enabled"`
	PublicModel     string            `json:"public_model"`
	LeadModel       string            `json:"lead_model_uid"`
	SidekickModel   string            `json:"sidekick_model_uid"`
	MaxHandoffs     int               `json:"max_handoffs_per_turn"`
	MaxRequests     int               `json:"max_internal_requests_per_turn"`
	MaxTools        int               `json:"max_external_tool_calls_per_turn"`
	MaxWallSeconds  int               `json:"max_turn_wall_time_seconds"`
	MaxCostUSD      float64           `json:"max_cost_usd_per_turn"`
	MaxContextBytes int               `json:"max_context_bytes"`
	MaxOutputTokens int               `json:"max_output_tokens"`
	TTLSeconds      int               `json:"session_ttl_seconds"`
	Compaction      *CompactionPolicy `json:"compaction,omitempty"`
	Keepalive       *KeepalivePolicy  `json:"cache_keepalive,omitempty"`
}

func (p Profile) Validate() error {
	if len(p.PublicModel) > 128 || !strings.HasPrefix(strings.ToLower(p.PublicModel), "fusion-") || strings.TrimSpace(p.PublicModel) != p.PublicModel || strings.ContainsAny(p.PublicModel, " \t\r\n") {
		return E("profile_invalid", "public_model must be a fusion-* identifier", 400)
	}
	for _, m := range []string{p.LeadModel, p.SidekickModel} {
		if strings.TrimSpace(m) == "" || len(m) > 256 || devin.PlanModelUID(m).Mode != devin.RouteDirect {
			return E("profile_invalid", "both members must be explicit concrete model UIDs", 400)
		}
	}
	if p.MaxHandoffs < 1 || p.MaxHandoffs > 8 || p.MaxRequests < 3 || p.MaxRequests > 64 || p.MaxTools < 1 || p.MaxTools > 128 || p.MaxWallSeconds < 10 || p.MaxWallSeconds > 1800 || p.MaxContextBytes < 1024 || p.MaxContextBytes > 2*1024*1024 || p.MaxOutputTokens < 64 || p.MaxOutputTokens > 32768 || p.TTLSeconds < 60 || p.TTLSeconds > 7*24*3600 || math.IsNaN(p.MaxCostUSD) || math.IsInf(p.MaxCostUSD, 0) || p.MaxCostUSD <= 0 || p.MaxCostUSD > 100 {
		return E("profile_invalid", "Fusion requires bounded handoffs, requests, tools, wall time, context, output, cost and session TTL", 400)
	}
	if err := p.CompactPolicy().Validate(); err != nil {
		return err
	}
	return p.KeepalivePolicy().Validate()
}
func (p Profile) Revision() string {
	return Digest(struct {
		Policy  string
		Profile Profile
	}{PolicyVersion, p})
}
func Digest(v any) string              { return contextcompact.Digest(v) }
func Scope(userID, keyID int64) string { return Digest([]int64{userID, keyID}) }
func SafeID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

type Error struct {
	Code    string
	Message string
	Status  int
}

func E(code, message string, status int) *Error {
	return &Error{"devin_fusion_" + code, message, status}
}
func (e *Error) Error() string   { return e.Code + ": " + e.Message }
func (e *Error) Retryable() bool { return false }

type Role string

const (
	Lead     Role = "lead"
	Sidekick Role = "sidekick"
)
const (
	RunningLead     = "RUNNING_LEAD"
	RunningSidekick = "RUNNING_SIDEKICK"
	ReviewingLead   = "REVIEWING_LEAD"
	WaitingTool     = "WAITING_CLIENT_TOOL"
	Completed       = "TURN_COMPLETED"
	Cancelled       = "CANCELLED"
	Failed          = "FAILED"
	BudgetExhausted = "BUDGET_EXHAUSTED"
	Uncertain       = "UNCERTAIN"
)

type PendingTool struct {
	PublicID   string `json:"public_id"`
	InternalID string `json:"internal_id"`
	Role       Role   `json:"role"`
	Turn       int    `json:"turn"`
	HandoffID  string `json:"handoff_id,omitempty"`
	Name       string `json:"name"`
	Arguments  string `json:"arguments"`
}
type Handoff struct {
	ID     string `json:"id"`
	CallID string `json:"call_id"`
	Turn   int    `json:"turn"`
	Brief  string `json:"brief"`
	Report string `json:"report,omitempty"`
}
type Attempt struct {
	ID              string                       `json:"id"`
	Purpose         Purpose                      `json:"purpose,omitempty"`
	Role            Role                         `json:"role"`
	Turn            int                          `json:"turn"`
	Model           string                       `json:"model"`
	BillingModel    string                       `json:"billing_model,omitempty"`
	PricingSnapshot json.RawMessage              `json:"pricing_snapshot,omitempty"`
	StartedAt       time.Time                    `json:"started_at"`
	Status          string                       `json:"status"` // started, completed, unknown
	Response        *apicompat.ResponsesResponse `json:"response,omitempty"`
	Settled         bool                         `json:"settled"`
	Applied         bool                         `json:"applied"`
	CostUSD         float64                      `json:"cost_usd"`
	EstimateUSD     float64                      `json:"estimate_usd"`
	Failure         string                       `json:"failure,omitempty"`
	Error           *Error                       `json:"error,omitempty"`
}
type RequestRecord struct {
	Key          string                       `json:"key"`
	Digest       string                       `json:"digest"`
	Response     *apicompat.ResponsesResponse `json:"response,omitempty"`
	Error        *Error                       `json:"error,omitempty"`
	AttemptStart int                          `json:"attempt_start"`
}
type Session struct {
	ID              string                         `json:"id"`
	Scope           string                         `json:"scope"`
	AccountID       int64                          `json:"account_id"`
	Profile         Profile                        `json:"profile"`
	ProfileRevision string                         `json:"profile_revision"`
	PublicModel     string                         `json:"public_model"`
	Revision        int64                          `json:"revision"`
	State           string                         `json:"state"`
	Role            Role                           `json:"role"`
	Turn            int                            `json:"turn"`
	TurnStarted     time.Time                      `json:"turn_started"`
	ExpiresAt       time.Time                      `json:"expires_at"`
	Contract        string                         `json:"contract"`
	Public          []apicompat.ResponsesInputItem `json:"public"`
	Lead            []apicompat.ResponsesInputItem `json:"lead"`
	Sidekick        []apicompat.ResponsesInputItem `json:"sidekick"`
	Handoffs        []Handoff                      `json:"handoffs"`
	Pending         *PendingTool                   `json:"pending,omitempty"`
	Attempts        []Attempt                      `json:"attempts"`
	Requests        []RequestRecord                `json:"requests"`
	ActiveRequest   int                            `json:"active_request"`
	TurnRequests    int                            `json:"turn_requests"`
	TurnTools       int                            `json:"turn_tools"`
	TurnHandoffs    int                            `json:"turn_handoffs"`
	TurnCostUSD     float64                        `json:"turn_cost_usd"`
	AuxReservedUSD  float64                        `json:"aux_reserved_usd,omitempty"`
	Contexts        map[Role]*RoleContext          `json:"contexts,omitempty"`
	Maintenance     MaintenanceStats               `json:"maintenance,omitempty"`

	TurnDelegationReminders int           `json:"turn_delegation_reminders,omitempty"`
	QueuedTools             []PendingTool `json:"queued_tools,omitempty"`
}

func (s *Session) Lane(role Role) *[]apicompat.ResponsesInputItem {
	if role == Sidekick {
		return &s.Sidekick
	}
	return &s.Lead
}
func (s *Session) LaneScope(role Role) string {
	base := fmt.Sprintf("fusion:%s:%d:%s:%s:%s", s.Scope, s.AccountID, s.ID, role, s.ProfileRevision)
	if c := s.Contexts[role]; c != nil && c.Epoch > 0 {
		return fmt.Sprintf("%s:context:%d", base, c.Epoch)
	}
	return base
}

func (s *Session) NeedsUsageReconciliation() bool {
	if len(s.Attempts) > 0 {
		a := s.Attempts[len(s.Attempts)-1]
		if !a.Applied && (!a.Settled || a.Status == "started" || a.Status == "unknown") {
			return true
		}
	}
	for _, c := range s.Contexts {
		if c.Job != nil && !c.Job.Accounted {
			return true
		}
	}
	return false
}

type Call struct {
	AttemptID   string
	Role        Role
	Request     *apicompat.ResponsesRequest
	LaneScope   string
	PricingAt   time.Time
	EstimateUSD float64
	Purpose     Purpose
}
type Outcome struct {
	Response        *apicompat.ResponsesResponse
	BillingModel    string
	PricingSnapshot json.RawMessage
	// KnownFailure means a terminal provider outcome was observed; ambiguous
	// transports with no terminal usage must never be automatically reissued.
	KnownFailure       bool
	NotStarted         bool
	ClientDisconnected bool
}

// Executor keeps physical inference, pricing and billing outside the state machine.
// Settle MUST be idempotent by Attempt.ID, including after process restart.
type Executor interface {
	Estimate(context.Context, Call) (float64, error)
	Execute(context.Context, Call) (Outcome, error)
	Settle(context.Context, Attempt) (float64, error)
}

// ContextMeasurer measures the actual provider request shape conservatively.
// Limits include fixed prompts/tools and reserve output plus an estimation margin.
type ContextMeasurer interface {
	MeasureContext(*apicompat.ResponsesRequest) (size, safeInput int, err error)
}

// AuxiliaryFactory returns an executor which owns its own billing reservations
// and immutable request metadata, never the completed HTTP writer/context.
// A busy background lane must return false immediately, without queuing.
type AuxiliaryFactory interface {
	Auxiliary(context.Context) (Executor, func(), bool)
}
type Input struct {
	Scope, SessionID, IdempotencyKey string
	AccountID                        int64
	Profile                          Profile
	Request                          *apicompat.ResponsesRequest
	Cancel                           bool
}
type Result struct {
	Response         *apicompat.ResponsesResponse
	SessionID, State string
	Replayed         bool
}

// AtomicStore is required: a cache-only or process-local store is insufficient.
type AtomicStore interface {
	devin.SessionStore
	CompareAndSwap(context.Context, string, []byte, []byte, time.Duration) (bool, error)
}
