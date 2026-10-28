package config

import (
	"math"
	"testing"

	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
	"github.com/stretchr/testify/require"
)

func TestDevinContextPolicyAndRolloutValidation(t *testing.T) {
	c := DevinContextConfig{Enabled: true, AutoCompaction: true}
	p := c.Policy()
	require.Equal(t, mc.DefaultPolicy(), p)
	require.NoError(t, c.Validate(), "empty allowlists deliberately admit no keys/models")
	for _, mutate := range []func(*DevinContextConfig){func(c *DevinContextConfig) { c.MaxTurnCostUSD = math.NaN() }, func(c *DevinContextConfig) { c.MaxMaintenanceCostUSD = math.Inf(1) }, func(c *DevinContextConfig) { c.TTLSeconds = 1 }, func(c *DevinContextConfig) { c.MaxRequests = -1 }, func(c *DevinContextConfig) { c.MaxSessions = 1001 }, func(c *DevinContextConfig) { c.SummaryAttempts = 20 }, func(c *DevinContextConfig) { c.APIKeyIDs = []int64{-1} }, func(c *DevinContextConfig) { c.ModelUIDs = []string{" "} }} {
		bad := c
		mutate(&bad)
		require.Error(t, bad.Validate())
	}
	c.MaxRequests = 8000
	c.AutoCompaction = false
	p = c.Policy()
	require.Equal(t, 8000, p.MaxRequests)
	require.False(t, p.Auto)
	require.NoError(t, p.Validate())
	c.Enabled = false
	c.TTLSeconds = -1
	require.Error(t, c.Validate(), "GUI-enabled accounts still use shared limits when the legacy rollout is disabled")
}
