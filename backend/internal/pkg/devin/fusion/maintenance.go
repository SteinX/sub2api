package fusion

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
)

type Purpose string

type MaintenanceEvent struct {
	Purpose     Purpose
	Role        Role
	Stage       string
	JobID       string
	Reason      string
	InputBefore int
	InputAfter  int
}

func (e *Engine) observe(event MaintenanceEvent) {
	if e.Observer != nil {
		e.Observer(event)
	}
}

const (
	Inference    Purpose = "inference"
	CacheRefresh Purpose = "cache_refresh"
	Compaction   Purpose = "compaction"
)

func (p Purpose) IsInference() bool { return p == "" || p == Inference }

// Thresholds are conservative input units (one UTF-8 byte per possible token).
// Two values mean spawn,hard with apply=hard; zero values use budget ratios.
type CompactionPolicy struct {
	Enabled         bool  `json:"enabled"`
	Lead            []int `json:"lead_thresholds,omitempty"`
	Sidekick        []int `json:"sidekick_thresholds,omitempty"`
	MaxAttempts     int   `json:"max_attempts_per_role_per_turn"`
	TimeoutSeconds  int   `json:"timeout_seconds"`
	MaxOutputTokens int   `json:"max_output_tokens"`
}

func (p Profile) CompactPolicy() CompactionPolicy {
	c := CompactionPolicy{Enabled: true, MaxAttempts: 2, TimeoutSeconds: 30, MaxOutputTokens: 1024}
	if p.Compaction != nil {
		c = *p.Compaction
		if c.MaxAttempts == 0 {
			c.MaxAttempts = 2
		}
		if c.TimeoutSeconds == 0 {
			c.TimeoutSeconds = 30
		}
		if c.MaxOutputTokens == 0 {
			c.MaxOutputTokens = 1024
		}
	}
	return c
}
func (p CompactionPolicy) Validate() error {
	for _, values := range [][]int{p.Lead, p.Sidekick} {
		if err := contextcompact.ValidateThresholds(values, 64, 2*1024*1024); err != nil {
			return compactionError(err)
		}
	}
	if p.MaxAttempts < 1 || p.MaxAttempts > 4 || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 120 || p.MaxOutputTokens < 64 || p.MaxOutputTokens > 4096 {
		return E("compaction_invalid", "compaction requires bounded attempts, timeout and output", 400)
	}
	return nil
}

type thresholds struct{ spawn, apply, hard int }

func (p CompactionPolicy) thresholds(role Role, safe int) (thresholds, error) {
	v := p.Lead
	if role == Sidekick && len(p.Sidekick) > 0 {
		v = p.Sidekick
	}
	t, err := contextcompact.ResolveThresholds(v, safe)
	return thresholds{t.Spawn, t.Apply, t.Hard}, compactionError(err)
}

type RoleContext struct {
	Epoch       uint64          `json:"epoch"`
	Job         *CompactionJob  `json:"job,omitempty"`
	AttemptTurn int             `json:"attempt_turn,omitempty"`
	Attempts    int             `json:"attempts,omitempty"`
	Archives    []PrefixArchive `json:"archives,omitempty"`
}
type PrefixArchive struct {
	JobID        string `json:"job_id"`
	Digest       string `json:"digest"`
	Count        int    `json:"count"`
	ContextEpoch uint64 `json:"context_epoch"`
}
type CompactionJob struct {
	ID              string    `json:"id"`
	Role            Role      `json:"role"`
	Turn            int       `json:"turn"`
	Epoch           uint64    `json:"epoch"`
	ProfileRevision string    `json:"profile_revision"`
	PrefixCount     int       `json:"prefix_count"`
	PrefixDigest    string    `json:"prefix_digest"`
	State           string    `json:"state"`
	CreatedAt       time.Time `json:"created_at"`
	Deadline        time.Time `json:"deadline"`
	EstimateUSD     float64   `json:"estimate_usd"`
	Accounted       bool      `json:"accounted"`
	Reason          string    `json:"reason,omitempty"`
	Policy          string    `json:"policy"`
}

func (p Profile) ValidateContextBudget(role Role, safe int) error {
	_, err := p.CompactPolicy().thresholds(role, safe)
	return err
}

type MaintenanceStats struct {
	ArchivedBytes       int                       `json:"archived_bytes"`
	CompactionsPrepared int                       `json:"compactions_prepared"`
	CompactionsApplied  int                       `json:"compactions_applied"`
	CompactionsStale    int                       `json:"compactions_stale"`
	CompactionsFailed   int                       `json:"compactions_failed"`
	CompactionCostUSD   float64                   `json:"compaction_cost_usd"`
	CompactionUsage     *apicompat.ResponsesUsage `json:"compaction_usage,omitempty"`
}

func (s *Session) roleContext(role Role) *RoleContext {
	if s.Contexts == nil {
		s.Contexts = map[Role]*RoleContext{}
	}
	if s.Contexts[role] == nil {
		s.Contexts[role] = &RoleContext{}
	}
	return s.Contexts[role]
}
func validCost(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 }

// MaintenanceRunner owns only bounded Fusion summary calls, not session state.
// Jobs/results live in the shared store; a restart never replays an unknown call.
type MaintenanceRunner struct {
	mu       sync.Mutex
	jobs     map[string]maintenanceRun
	capacity int
}
type maintenanceRun struct {
	session string
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewMaintenanceRunner(capacity int) *MaintenanceRunner {
	if capacity <= 0 {
		capacity = 16
	}
	return &MaintenanceRunner{jobs: map[string]maintenanceRun{}, capacity: capacity}
}
func (r *MaintenanceRunner) Start(ctx context.Context, scope, session, id string, timeout time.Duration, fn func(context.Context)) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	if len(r.jobs) >= r.capacity {
		r.mu.Unlock()
		return false
	}
	if _, exists := r.jobs[id]; exists {
		r.mu.Unlock()
		return false
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	r.jobs[id] = maintenanceRun{session: Digest([]string{scope, session}), cancel: cancel, done: make(chan struct{})}
	r.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			r.mu.Lock()
			run := r.jobs[id]
			delete(r.jobs, id)
			close(run.done)
			r.mu.Unlock()
		}()
		fn(bounded)
	}()
	return true
}
func (r *MaintenanceRunner) Cancel(scope, session string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := Digest([]string{scope, session})
	for _, run := range r.jobs {
		if run.session == key {
			run.cancel()
		}
	}
}
func (r *MaintenanceRunner) Done(id string) <-chan struct{} {
	if r != nil {
		r.mu.Lock()
		run, ok := r.jobs[id]
		r.mu.Unlock()
		if ok {
			return run.done
		}
	}
	ch := make(chan struct{})
	close(ch)
	return ch
}
