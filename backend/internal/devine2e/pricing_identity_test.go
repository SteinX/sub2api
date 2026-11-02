//go:build devine2e

package devine2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// This is a public-key protocol regression against the production binary and
// disposable SQL/Redis. It reproduces the sanitized OMP request shape; the
// native catalog and model replies are synthetic, not real-client acceptance.
func TestDevinOpusPriceIdentityPublicResponses(t *testing.T) {
	if os.Getenv("DEVIN_E2E_MODE") != "mock" {
		t.Skip("requires the isolated mock runner")
	}
	const lead = "claude-opus-5-xhigh"
	const sidekick = "swe-2-high"
	const unknown = "claude-opus-5-unknown"
	const public = "fusion-claude-opus-5-xhigh-sidekick-swe-2-high"
	f := newNativeFixture(t)
	f.agentMode.Store(true)
	for _, uid := range []string{lead, sidekick, unknown} {
		tier := pb.ModelCostTier_MODEL_COST_TIER_HIGH
		if uid == sidekick {
			tier = pb.ModelCostTier_MODEL_COST_TIER_FREE
		}
		f.catalog = append(f.catalog, &pb.ClientModelConfig{ModelUid: uid, PricingType: pb.ModelPricingType_MODEL_PRICING_TYPE_API, ModelCostTier: tier, MaxTokens: 262144,
			ModelInfo: &pb.ModelInfo{MaxOutputTokens: 8192, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}})
	}
	// This fixture has its own SQL database; its reused row IDs must not pick
	// up another scenario's scheduler/account records from Redis DB 0.
	a := newApplication(t, f, 1)
	admin := login(t, a, "admin@devin-e2e.invalid", a.password)
	compliance := api(t, a, "GET", "/admin/compliance", admin, nil)
	api(t, a, "POST", "/admin/compliance/accept", admin, map[string]string{"language": "en", "phrase": compliance.Get("ack_phrase_en").String()})
	group := api(t, a, "POST", "/admin/groups", admin, map[string]any{"name": "opus-pricing", "platform": "devin", "rate_multiplier": 1, "subscription_type": "standard", "is_exclusive": true})
	groupID := group.Get("id").Int()
	proxyID := f.registerProxy(t, a, admin)
	accountID, err := addPATAccount(t, a, admin, "cog_e2e_opus_pricing", groupID, proxyID)
	require.NoError(t, err)
	account := api(t, a, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil)
	require.Equal(t, proxyID, account.Get("proxy_id").Int())
	require.True(t, account.Get("schedulable").Bool())
	require.EqualValues(t, 4, account.Get(`extra.devin_model_catalog.models.#(model_uid=="swe-2-high").model_cost_tier`).Int())
	password := randomID()
	a.redactor.addSecret(password)
	api(t, a, "POST", "/admin/users", admin, map[string]any{"email": "user@devin-e2e.invalid", "password": password, "balance": 100, "concurrency": 4, "allowed_groups": []int64{groupID}})
	user := login(t, a, "user@devin-e2e.invalid", password)
	created := api(t, a, "POST", "/keys", user, map[string]any{"name": "opus-pricing", "group_id": groupID})
	key, keyID := created.Get("key").String(), created.Get("id").Int()
	require.NotEmpty(t, key)
	a.redactor.addSecret(key)
	db := agentDatabase(t, a)
	tools := make([]any, 12)
	for i := range tools {
		tools[i] = map[string]any{"type": "function", "name": fmt.Sprintf("fixture_tool_%d", i), "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	}
	body := map[string]any{"model": public, "stream": true, "store": false, "max_output_tokens": 4096, "tools": tools,
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply with E2E_OK."}}}}}
	var wantInput, wantCached, wantOutput, wantTotal float64
	for batch, source := range []string{"identified_default", "group"} {
		inputRate, cachedRate, outputRate, writeRate := 5e-6, .5e-6, 25e-6, 6.25e-6
		if batch == 1 {
			// Deliberately different synthetic sale rates for the exact real UID.
			inputRate, cachedRate, outputRate, writeRate = 7e-6, .7e-6, 31e-6, 8.75e-6
			api(t, a, "PUT", fmt.Sprintf("/admin/groups/%d", groupID), admin, map[string]any{"model_pricing": []any{map[string]any{
				"models": []string{lead}, "platform": "devin", "billing_mode": "token", "input_price": inputRate, "cache_read_price": cachedRate, "output_price": outputRate, "cache_write_price": writeRate,
			}}})
		}
		quote := request(t, a, "GET", "/v1/models?pricing_models="+lead+","+sidekick+","+unknown, key, nil, 200)
		prices := gjson.GetBytes(quote, "data").Array()
		require.Len(t, prices, 3)
		require.True(t, prices[0].Get("identified").Bool())
		require.True(t, prices[0].Get("calculable").Bool())
		require.Equal(t, source, prices[0].Get("source").String())
		require.False(t, prices[0].Get("zero_effective_charge").Bool())
		require.Equal(t, "devin_catalog_free", prices[1].Get("source").String(), "mock native catalog calls=%d, mints=%d", f.catalogCalls.Load(), f.mints.Load())
		require.True(t, prices[1].Get("zero_effective_charge").Bool())
		require.False(t, prices[2].Get("identified").Bool())
		data := request(t, a, "POST", "/v1/responses", key, body, 200)
		validateStream(t, data, "responses")
		// The fixture emits two paid Lead calls and one FREE Sidekick call.
		wantInput += 2 * 11 * inputRate
		wantCached += 2 * 5 * cachedRate
		wantOutput += 2 * 3 * outputRate
		wantTotal += 2 * (11*inputRate + 5*cachedRate + 3*outputRate + 2*writeRate)
		var rows, unique, paidRows, freeRows int
		var input, cached, output, total, freeCost float64
		require.Eventually(t, func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return db.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT request_id),
				count(*) FILTER(WHERE upstream_model=$2),count(*) FILTER(WHERE upstream_model=$3),
				coalesce(sum(input_cost),0),coalesce(sum(cache_read_cost),0),coalesce(sum(output_cost),0),coalesce(sum(total_cost),0),
				coalesce(sum(actual_cost) FILTER(WHERE upstream_model=$3),0) FROM usage_logs WHERE api_key_id=$1`, keyID, lead, sidekick).
				Scan(&rows, &unique, &paidRows, &freeRows, &input, &cached, &output, &total, &freeCost) == nil && rows == 3*(batch+1)
		}, 10*time.Second, 100*time.Millisecond)
		require.Equal(t, rows, unique)
		require.Equal(t, 2*(batch+1), paidRows)
		require.Equal(t, batch+1, freeRows)
		require.InDelta(t, wantInput, input, 1e-9)
		require.InDelta(t, wantCached, cached, 1e-9)
		require.InDelta(t, wantOutput, output, 1e-9)
		require.InDelta(t, wantTotal, total, 1e-9)
		require.Zero(t, freeCost)
	}
	// The suffix association must not turn an arbitrary new paid UID into a
	// known card, even though it is a granted concrete model in this catalog.
	body["model"] = "fusion-" + unknown + "-sidekick-" + sidekick
	data := request(t, a, "POST", "/v1/responses", key, body, 400)
	require.Equal(t, "devin_fusion_pricing_unavailable", gjson.GetBytes(data, "error.code").String())
	require.Contains(t, gjson.GetBytes(data, "error.message").String(), unknown)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.chats, 6, "unknown pricing must not start another inference")
	counts := map[string]int{}
	for _, call := range f.chats {
		counts[call.ChatModelUid]++
	}
	require.Equal(t, map[string]int{lead: 4, sidekick: 2}, counts, "pricing normalization must preserve the actual effort UID on the wire")
	require.GreaterOrEqual(t, f.catalogCalls.Load(), int32(2), "pricing must read the full committed account's live catalog, beyond onboarding")
}
