package fusion

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFusionKeepaliveDefaultsAndExplicitOptOut(t *testing.T) {
	for _, p := range []Profile{testProfile(), DefaultProfile("fusion-lead-sidekick-worker", "lead", "worker")} {
		policy := p.KeepalivePolicy()
		require.True(t, policy.Enabled)
		require.Equal(t, 240, policy.IntervalSeconds)
		require.Equal(t, 900, policy.MaxWindowSeconds)
		require.Equal(t, 2, policy.MaxAttempts)
		require.Equal(t, .5, policy.MaxCostUSD)
		require.Equal(t, 30, policy.TimeoutSeconds)
		require.NoError(t, policy.Validate())
		p.Keepalive = &KeepalivePolicy{Enabled: false}
		require.False(t, p.KeepalivePolicy().Enabled)
		require.NoError(t, p.Validate())
	}
	p := testProfile()
	p.MaxCostUSD = .05
	require.Equal(t, .05, p.KeepalivePolicy().MaxCostUSD)
}

func TestFusionKeepaliveJSONInheritsLimitsWithoutOverridingOptOut(t *testing.T) {
	for _, test := range []struct {
		raw      string
		enabled  bool
		interval int
	}{
		{`{}`, true, 240}, {`{"interval_seconds":90}`, true, 90},
		{`{"enabled":true}`, true, 240}, {`{"enabled":false}`, false, 0},
	} {
		t.Run(test.raw, func(t *testing.T) {
			p := testProfile()
			p.Keepalive = &KeepalivePolicy{}
			require.NoError(t, json.Unmarshal([]byte(test.raw), p.Keepalive))
			policy := p.KeepalivePolicy()
			require.Equal(t, test.enabled, policy.Enabled)
			require.Equal(t, test.interval, policy.IntervalSeconds)
			require.NoError(t, p.Validate())
			encoded, err := json.Marshal(p)
			require.NoError(t, err)
			var restored Profile
			require.NoError(t, json.Unmarshal(encoded, &restored))
			require.Equal(t, policy, restored.KeepalivePolicy())
			require.Equal(t, p.Revision(), restored.Revision())
		})
	}
}
