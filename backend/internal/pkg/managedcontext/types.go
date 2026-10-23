// Package managedcontext owns provider-independent persistent context sessions.
// The store is authoritative; executors own provider admission and billing.
package managedcontext

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	cc "github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
)

const (
	HeaderMode    = "X-Sub2API-Context-Mode"
	HeaderSession = "X-Sub2API-Context-Session"
	PolicyVersion = "managed-context-v1"
)

var ErrNotFound = errors.New("context resource not found")

func E(code, message string, status int) *cc.Error {
	return &cc.Error{Code: "context_" + strings.TrimPrefix(code, "context_"), Message: message, Status: status}
}

type Policy struct {
	Version                   string  `json:"version"`
	Auto                      bool    `json:"auto"`
	TTLSeconds                int     `json:"ttl_seconds"`
	MaxHistoryBytes           int64   `json:"max_history_bytes"`
	MaxUserHistoryBytes       int64   `json:"max_user_history_bytes"`
	MaxSessions               int     `json:"max_sessions"`
	MaxUserSessions           int     `json:"max_user_sessions"`
	MaxRequests               int     `json:"max_requests"`
	MaxTurnRequests           int     `json:"max_turn_requests"`
	MaxTurnCostUSD            float64 `json:"max_turn_cost_usd"`
	MaxMaintenanceCostUSD     float64 `json:"max_maintenance_cost_usd"`
	MaxUserMaintenanceCostUSD float64 `json:"max_user_maintenance_cost_usd"`
	MaxContextBytes           int     `json:"max_context_bytes"`
	MaxOutputTokens           int     `json:"max_output_tokens"`
	WallSeconds               int     `json:"wall_seconds"`
	SummarySeconds            int     `json:"summary_seconds"`
	SummaryOutputTokens       int     `json:"summary_output_tokens"`
	SummaryAttempts           int     `json:"summary_attempts"`
}

func DefaultPolicy() Policy {
	return Policy{Version: PolicyVersion, Auto: true, TTLSeconds: 86400, MaxHistoryBytes: 64 << 20, MaxUserHistoryBytes: 256 << 20, MaxSessions: 8, MaxUserSessions: 32, MaxRequests: 4096, MaxTurnRequests: 4, MaxTurnCostUSD: 5, MaxMaintenanceCostUSD: 10, MaxUserMaintenanceCostUSD: 50, MaxContextBytes: 2 << 20, MaxOutputTokens: 4096, WallSeconds: 180, SummarySeconds: 30, SummaryOutputTokens: 1024, SummaryAttempts: 2}
}
func (p Policy) Validate() error {
	if p.Version != PolicyVersion || p.TTLSeconds < 60 || p.TTLSeconds > 604800 || p.MaxHistoryBytes < 1<<20 || p.MaxHistoryBytes > 1<<30 || p.MaxUserHistoryBytes < p.MaxHistoryBytes || p.MaxUserHistoryBytes > 4<<30 || p.MaxSessions < 1 || p.MaxSessions > 100 || p.MaxUserSessions < p.MaxSessions || p.MaxUserSessions > 1000 || p.MaxRequests < 1 || p.MaxRequests > 100000 || p.MaxTurnRequests < 2 || p.MaxTurnRequests > 16 || !(p.MaxTurnCostUSD > 0 && p.MaxTurnCostUSD <= 100) || !(p.MaxMaintenanceCostUSD > 0 && p.MaxMaintenanceCostUSD <= 100) || !(p.MaxUserMaintenanceCostUSD >= p.MaxMaintenanceCostUSD && p.MaxUserMaintenanceCostUSD <= 1000) || p.MaxContextBytes < 1024 || p.MaxContextBytes > 2<<20 || p.MaxOutputTokens < 64 || p.MaxOutputTokens > 32768 || p.WallSeconds < 10 || p.WallSeconds > 1800 || p.SummarySeconds < 1 || p.SummarySeconds > 120 || p.SummaryOutputTokens < 64 || p.SummaryOutputTokens > 4096 || p.SummaryAttempts < 1 || p.SummaryAttempts > 4 {
		return E("policy_invalid", "managed context policy requires bounded storage, calls, time, output and cost", 400)
	}
	return nil
}

type Owner struct {
	Scope          string `json:"scope"`
	UserID         int64  `json:"user_id"`
	KeyID          int64  `json:"key_id"`
	AccountID      int64  `json:"account_id"`
	GroupID        int64  `json:"group_id,omitempty"`
	SubscriptionID int64  `json:"subscription_id,omitempty"`
}
type Ref struct {
	ID      string `json:"id"`
	Digest  string `json:"digest"`
	Bytes   int    `json:"bytes"`
	Summary bool   `json:"summary,omitempty"`
}
type Session struct {
	Owner
	ID                     string                     `json:"id"`
	Schema                 int                        `json:"schema"`
	Model                  string                     `json:"model"`
	Policy                 Policy                     `json:"policy"`
	Template               apicompat.ResponsesRequest `json:"template"`
	Contract               string                     `json:"contract"`
	Epoch                  uint64                     `json:"epoch"`
	Head                   string                     `json:"head"`
	Active                 []Ref                      `json:"active"`
	OperationID            string                     `json:"operation_id,omitempty"`
	JobID                  string                     `json:"job_id,omitempty"`
	AppliedJobID           string                     `json:"applied_job_id,omitempty"`
	AppliedJobAfter        int                        `json:"applied_job_after,omitempty"`
	PendingCall            string                     `json:"pending_call,omitempty"`
	Status                 string                     `json:"status"`
	HistoryBytes           int64                      `json:"history_bytes"`
	ReservedBytes          int64                      `json:"reserved_bytes"`
	ReservationOperationID string                     `json:"reservation_operation_id,omitempty"`
	Requests               int                        `json:"requests"`
	LastBudget             *cc.Budget                 `json:"context_budget,omitempty"`
	MaintenanceCostUSD     float64                    `json:"maintenance_cost_usd"`
	InferenceUsage         *apicompat.ResponsesUsage  `json:"inference_usage,omitempty"`
	MaintenanceUsage       *apicompat.ResponsesUsage  `json:"maintenance_usage,omitempty"`
	AccountedJobs          map[string]bool            `json:"accounted_jobs,omitempty"`
	ExpiresAt              time.Time                  `json:"expires_at"`
}

type Operation struct {
	Owner
	ID            string         `json:"id"`
	Namespace     string         `json:"namespace"`
	Fingerprint   string         `json:"fingerprint"`
	SessionID     string         `json:"session_id,omitempty"`
	Model         string         `json:"model"`
	Policy        Policy         `json:"policy"`
	State         string         `json:"state"`
	Attempts      []string       `json:"attempts,omitempty"`
	ResponseRef   string         `json:"response_ref,omitempty"`
	Compact       *CompactResult `json:"compact,omitempty"`
	JobID         string         `json:"job_id,omitempty"`
	ReservedBytes int64          `json:"reserved_bytes,omitempty"`
	Error         *cc.Error      `json:"error,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}
type Attempt struct {
	Owner
	ID             string                    `json:"id"`
	OperationID    string                    `json:"operation_id"`
	SessionID      string                    `json:"session_id,omitempty"`
	Model          string                    `json:"model"`
	BillingModel   string                    `json:"billing_model,omitempty"`
	Purpose        string                    `json:"purpose"`
	State          string                    `json:"state"`
	Settled        bool                      `json:"settled"`
	CostUSD        float64                   `json:"cost_usd"`
	EstimateUSD    float64                   `json:"estimate_usd"`
	ResponseRef    string                    `json:"response_ref,omitempty"`
	Usage          *apicompat.ResponsesUsage `json:"usage,omitempty"`
	ResponseID     string                    `json:"response_id,omitempty"`
	ResponseModel  string                    `json:"response_model,omitempty"`
	ResponseStatus string                    `json:"response_status,omitempty"`
	Error          *cc.Error                 `json:"error,omitempty"`
	StartedAt      time.Time                 `json:"started_at"`
	Deadline       time.Time                 `json:"deadline"`
	Policy         Policy                    `json:"policy"`
}
type Job struct {
	Model string                    `json:"model"`
	Usage *apicompat.ResponsesUsage `json:"usage,omitempty"`
	Owner
	ID          string      `json:"id"`
	OperationID string      `json:"operation_id"`
	SessionID   string      `json:"session_id,omitempty"`
	Snapshot    cc.Snapshot `json:"snapshot"`
	State       string      `json:"state"`
	Trigger     string      `json:"trigger"`
	AttemptID   string      `json:"attempt_id"`
	Summary     *cc.Summary `json:"summary,omitempty"`
	Target      int         `json:"target"`
	Before      int         `json:"before"`
	After       int         `json:"after"`
	Reason      string      `json:"reason,omitempty"`
	Accounted   bool        `json:"accounted"`
	Deadline    time.Time   `json:"deadline"`
}
type CompactResult struct {
	Status     string                    `json:"status"`
	Input      []cc.Item                 `json:"input"`
	ArtifactID string                    `json:"artifact_id,omitempty"`
	Before     int                       `json:"estimated_input_tokens_before"`
	After      int                       `json:"estimated_input_tokens_after"`
	Method     string                    `json:"estimate_method"`
	Usage      *apicompat.ResponsesUsage `json:"summary_usage,omitempty"`
}

type Record struct {
	Owner
	Kind      string
	ID        string
	Revision  int64
	Data      json.RawMessage
	Status    string
	ExpiresAt time.Time
	Cancelled bool
}
type Lease struct {
	Scope, Kind, ID, Owner string
	Token                  int64
}
type Blob struct {
	ID, SessionID, Visibility, Kind, Digest string
	Data                                    []byte
	ExpiresAt                               time.Time
}
type HistoryEntry struct {
	Sequence   int64           `json:"cursor"`
	ID         string          `json:"id"`
	Bytes      int             `json:"bytes"`
	Digest     string          `json:"digest"`
	Kind       string          `json:"kind"`
	Data       json.RawMessage `json:"item,omitempty"`
	ContentRef string          `json:"content_ref,omitempty"`
}
type UsageStats struct {
	Known, Unknown         int
	Inference, Maintenance *apicompat.ResponsesUsage
	MaintenanceCostUSD     float64
}

// CAS atomically updates one bounded record and writes immutable blobs. Session
// commits must carry its live lease; terminal physical results deliberately do
// not require a session lease, so late known usage remains recoverable.
type Store interface {
	Read(context.Context, string, string, string) (*Record, error)
	Create(context.Context, *Record, *Policy) (bool, error)
	CAS(context.Context, *Record, *Lease, []Blob, *Policy) error
	Acquire(context.Context, string, string, string, string, time.Duration) (Lease, error)
	Renew(context.Context, Lease, time.Duration) error
	Release(context.Context, Lease) error
	Cancel(context.Context, string, string) error
	Blob(context.Context, string, string) ([]byte, error)
	History(context.Context, string, string, int64, int) ([]HistoryEntry, error)
	Chunk(context.Context, string, string, string, int, int) ([]byte, int, string, error)
	Pending(context.Context, int) ([]*Record, error)
	Unsettled(context.Context, string, string) (int, int, error)
	UsageStats(context.Context, string, string) (UsageStats, error)
	Cleanup(context.Context, int) (int64, error)
}

type Call struct {
	ID, SessionID, Model, Purpose, LaneScope string
	Request                                  *apicompat.ResponsesRequest
	StartedAt                                time.Time
	EstimateUSD                              float64
}
type Outcome struct {
	Response                 *apicompat.ResponsesResponse
	BillingModel             string
	KnownFailure, NotStarted bool
}
type Executor interface {
	Measure(*apicompat.ResponsesRequest) (cc.Budget, error)
	Estimate(context.Context, Call) (float64, error)
	Execute(context.Context, Call) (Outcome, error)
	Settle(context.Context, Attempt) (float64, error)
	Auxiliary(context.Context) (Executor, func(), bool)
}
