//go:build devine2e

package devine2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
)

// Onboarding failures expose only known RPC stages, never upstream text or PATs.
func onboardingFailure(message string) error {
	stage := "credential validation"
	for _, candidate := range []string{"GetSelfDevinSessionToken", "GetCliModelConfigs"} {
		if strings.Contains(message, candidate) {
			stage = candidate
			break
		}
	}
	code := "rejected"
	for _, candidate := range []string{"permission_denied", "unauthenticated", "no available models"} {
		if strings.Contains(message, candidate) {
			code = candidate
			break
		}
	}
	return fmt.Errorf("PAT onboarding failed at %s (%s); no account was committed and no inference was requested", stage, code)
}

func addPATAccount(t *testing.T, a *application, admin, pat string, groupID, proxyID int64) (int64, error) {
	t.Helper()
	a.redactor.addSecret(pat)
	body := map[string]any{"mode": "key", "content": pat}
	if proxyID > 0 {
		body["proxy_id"] = proxyID
	}
	session := api(t, a, "POST", "/admin/devin/auth-sessions", admin, body)
	if session.Get("status").String() != "ready" {
		return 0, onboardingFailure(session.Get("error").String())
	}
	id := session.Get("id").String()
	require.NotEmpty(t, id)
	committed := api(t, a, "POST", "/admin/devin/auth-sessions/"+id+"/commit", admin, map[string]any{
		"name": "devin-e2e", "group_ids": []int64{groupID}, "concurrency": 4,
		"priority": 1, "rate_multiplier": 1,
	})
	require.Equal(t, "connected", committed.Get("status").String())
	accountID := committed.Get("account_id").Int()
	require.Positive(t, accountID)
	return accountID, nil
}

type smokeSelection struct {
	Mode             string   `json:"mode"`
	Model            string   `json:"model"`
	CreditMultiplier *float32 `json:"advertised_credit_multiplier,omitempty"`
	SelectionPolicy  string   `json:"selection_policy"`
	GatewayRequests  int      `json:"gateway_requests"`
	MaxOutputTokens  int      `json:"max_output_tokens_per_request"`
	PaidAllowed      bool     `json:"paid_allowed"`
	SpendingHardCap  bool     `json:"provider_spending_hard_cap"`
}

// Use the authenticated grants, never a guessed UID or a fabricated dollar rate.
// Positive native credit multipliers are relative cost evidence. Zero/missing
// values mean unknown, not free. Stable UID ordering breaks ties deterministically.
func selectSmokeModel(models []*pb.ClientModelConfig, override string, maxOutput int) (smokeSelection, error) {
	var candidates []*pb.ClientModelConfig
	for _, model := range models {
		uid := devin.ModelID(model)
		limit := model.GetModelInfo().GetMaxOutputTokens()
		if uid == "" || strings.HasPrefix(uid, "cog_") || model.GetIsCapacityLimited() ||
			devin.PlanModel(model).Mode != devin.RouteDirect || limit > 0 && maxOutput > int(limit) {
			continue
		}
		if override == "" || uid == override {
			candidates = append(candidates, model)
		}
	}
	if len(candidates) == 0 {
		return smokeSelection{}, errors.New("no granted direct model supports the smoke budget; no inference was requested")
	}
	known := func(m *pb.ClientModelConfig) bool {
		n := float64(m.GetCreditMultiplier())
		return n > 0 && !math.IsNaN(n) && !math.IsInf(n, 0)
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if known(a) != known(b) {
			return known(a)
		}
		if known(a) && a.GetCreditMultiplier() != b.GetCreditMultiplier() {
			return a.GetCreditMultiplier() < b.GetCreditMultiplier()
		}
		return devin.ModelID(a) < devin.ModelID(b)
	})
	selected := candidates[0]
	result := smokeSelection{Model: devin.ModelID(selected), SelectionPolicy: "granted-direct/known-positive-credit/lowest-credit/uid", GatewayRequests: 6, MaxOutputTokens: maxOutput}
	if known(selected) {
		value := selected.GetCreditMultiplier()
		result.CreditMultiplier = &value
	} else {
		result.SelectionPolicy = "granted-direct/price-unknown/uid"
	}
	return result, nil
}

func saveSmokePlan(t *testing.T, selection smokeSelection) {
	t.Helper()
	data, err := json.MarshalIndent(selection, "", "  ")
	require.NoError(t, err)
	if dir := os.Getenv("DEVIN_E2E_ARTIFACTS"); dir != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "smoke-plan.json"), append(data, '\n'), 0600))
	}
	credit := "unknown"
	if selection.CreditMultiplier != nil {
		credit = fmt.Sprint(*selection.CreditMultiplier)
	}
	t.Logf("discovered model=%s; advertised credit multiplier=%s; %d planned gateway requests; output cap=%d; paid_allowed=%v; provider spending hard cap=false", selection.Model, credit, selection.GatewayRequests, selection.MaxOutputTokens, selection.PaidAllowed)
}

func TestSmokeModelSelectionUsesAccountGrants(t *testing.T) {
	model := func(id string, credit float32) *pb.ClientModelConfig {
		return &pb.ClientModelConfig{ModelUid: id, CreditMultiplier: credit, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096}}
	}
	blocked := model("opaque-cheap", .01)
	blocked.ModelInfo.HarnessUids = []string{"unknown-harness"}
	disabled := model("disabled-cheap", .01)
	disabled.Disabled = true
	busy := model("capacity-cheap", .01)
	busy.IsCapacityLimited = true
	tiny := model("tiny-output", .01)
	tiny.ModelInfo.MaxOutputTokens = 128
	models := []*pb.ClientModelConfig{model("unknown-price", 0), model("costly", 8), model("cheap-b", 1), model("cheap-a", 1), model("fusion-hidden", .01), blocked, disabled, busy, tiny}
	selection, err := selectSmokeModel(models, "", 1024)
	require.NoError(t, err)
	require.Equal(t, "cheap-a", selection.Model)
	require.NotNil(t, selection.CreditMultiplier)
	require.Equal(t, float32(1), *selection.CreditMultiplier)
	_, err = selectSmokeModel(models, "not-granted", 1024)
	require.Error(t, err)
	_, err = selectSmokeModel(models, "disabled-cheap", 1024)
	require.Error(t, err)
	_, err = selectSmokeModel(models, "tiny-output", 1024)
	require.Error(t, err)
	_, err = selectSmokeModel(nil, "", 1024)
	require.Error(t, err)
	unknown, err := selectSmokeModel([]*pb.ClientModelConfig{model("b", 0), model("a", 0)}, "", 1024)
	require.NoError(t, err)
	require.Equal(t, "a", unknown.Model)
	require.Nil(t, unknown.CreditMultiplier)
}
