package config

import (
	"fmt"
	"strings"

	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
)

// DevinContextConfig supplies the legacy allowlisted rollout and shared limits.
// Account overrides can opt in independently; ForceDisabled blocks both paths.
type DevinContextConfig struct {
	Enabled                   bool     `mapstructure:"enabled"`
	ForceDisabled             bool     `mapstructure:"force_disabled"`
	ManualCompact             bool     `mapstructure:"manual_compact"`
	AutoCompaction            bool     `mapstructure:"auto_compaction"`
	APIKeyIDs                 []int64  `mapstructure:"api_key_ids"`
	ModelUIDs                 []string `mapstructure:"model_uids"`
	AccountIDs                []int64  `mapstructure:"account_ids"`
	TTLSeconds                int      `mapstructure:"ttl_seconds"`
	MaxHistoryBytes           int64    `mapstructure:"max_history_bytes"`
	MaxUserHistoryBytes       int64    `mapstructure:"max_user_history_bytes"`
	MaxSessions               int      `mapstructure:"max_sessions_per_key"`
	MaxUserSessions           int      `mapstructure:"max_sessions_per_user"`
	MaxRequests               int      `mapstructure:"max_requests"`
	MaxTurnRequests           int      `mapstructure:"max_turn_requests"`
	MaxTurnCostUSD            float64  `mapstructure:"max_turn_cost_usd"`
	MaxMaintenanceCostUSD     float64  `mapstructure:"max_maintenance_cost_usd"`
	MaxUserMaintenanceCostUSD float64  `mapstructure:"max_user_maintenance_cost_usd"`
	MaxContextBytes           int      `mapstructure:"max_context_bytes"`
	MaxOutputTokens           int      `mapstructure:"max_output_tokens"`
	WallSeconds               int      `mapstructure:"wall_seconds"`
	SummarySeconds            int      `mapstructure:"summary_seconds"`
	SummaryOutputTokens       int      `mapstructure:"summary_output_tokens"`
	SummaryAttempts           int      `mapstructure:"summary_attempts"`
}

func (c DevinContextConfig) Policy() mc.Policy {
	p := mc.DefaultPolicy()
	p.Auto = c.AutoCompaction
	if c.TTLSeconds != 0 {
		p.TTLSeconds = c.TTLSeconds
	}
	if c.MaxHistoryBytes != 0 {
		p.MaxHistoryBytes = c.MaxHistoryBytes
	}
	if c.MaxUserHistoryBytes != 0 {
		p.MaxUserHistoryBytes = c.MaxUserHistoryBytes
	}
	if c.MaxSessions != 0 {
		p.MaxSessions = c.MaxSessions
	}
	if c.MaxUserSessions != 0 {
		p.MaxUserSessions = c.MaxUserSessions
	}
	if c.MaxRequests != 0 {
		p.MaxRequests = c.MaxRequests
	}
	if c.MaxTurnRequests != 0 {
		p.MaxTurnRequests = c.MaxTurnRequests
	}
	if c.MaxTurnCostUSD != 0 {
		p.MaxTurnCostUSD = c.MaxTurnCostUSD
	}
	if c.MaxMaintenanceCostUSD != 0 {
		p.MaxMaintenanceCostUSD = c.MaxMaintenanceCostUSD
	}
	if c.MaxUserMaintenanceCostUSD != 0 {
		p.MaxUserMaintenanceCostUSD = c.MaxUserMaintenanceCostUSD
	}
	if c.MaxContextBytes != 0 {
		p.MaxContextBytes = c.MaxContextBytes
	}
	if c.MaxOutputTokens != 0 {
		p.MaxOutputTokens = c.MaxOutputTokens
	}
	if c.WallSeconds != 0 {
		p.WallSeconds = c.WallSeconds
	}
	if c.SummarySeconds != 0 {
		p.SummarySeconds = c.SummarySeconds
	}
	if c.SummaryOutputTokens != 0 {
		p.SummaryOutputTokens = c.SummaryOutputTokens
	}
	if c.SummaryAttempts != 0 {
		p.SummaryAttempts = c.SummaryAttempts
	}
	return p
}

func (c DevinContextConfig) Validate() error {
	// Limits also apply to accounts enabled from the GUI when Enabled is false.
	if err := c.Policy().Validate(); err != nil {
		return fmt.Errorf("gateway.devin_context: %w", err)
	}
	for _, id := range append(append([]int64{}, c.APIKeyIDs...), c.AccountIDs...) {
		if id < 1 {
			return fmt.Errorf("gateway.devin_context: allowlist IDs must be positive")
		}
	}
	for _, model := range c.ModelUIDs {
		if strings.TrimSpace(model) == "" || len(model) > 256 {
			return fmt.Errorf("gateway.devin_context: model_uids require concrete nonempty UIDs")
		}
	}
	return nil
}
