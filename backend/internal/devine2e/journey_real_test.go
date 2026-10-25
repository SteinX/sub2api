//go:build devine2e

package devine2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type approvedJourneyPlan struct {
	Models struct {
		Direct   string `json:"direct"`
		Lead     string `json:"lead"`
		Sidekick string `json:"sidekick"`
	} `json:"models"`
	Clients             []string `json:"clients"`
	ReferencePricingSHA string   `json:"reference_pricing_sha256"`
	GroupPricingSHA     string   `json:"group_pricing_sha256"`
	Limits              struct {
		Requests    int     `json:"public_requests"`
		Output      int     `json:"output_tokens"`
		TurnSeconds int     `json:"turn_seconds"`
		Calls       int     `json:"internal_calls_per_turn"`
		Tools       int     `json:"tools_per_turn"`
		Handoffs    int     `json:"handoffs_per_turn"`
		LocalUSD    float64 `json:"local_usd"`
		TurnUSD     float64 `json:"turn_usd"`
	} `json:"limits"`
}

func approvedPlan(t *testing.T) approvedJourneyPlan {
	t.Helper()
	require.Equal(t, "true", os.Getenv("DEVIN_JOURNEY_REAL"), "use the approved supervisor, never invoke a real journey directly")
	_, err := journeyPolicy("verify")
	require.NoError(t, err, "approval, clean source and claimed durable ledger are required before native auth")
	data, err := os.ReadFile(os.Getenv("DEVIN_JOURNEY_PLAN_FILE"))
	require.NoError(t, err)
	var plan approvedJourneyPlan
	require.NoError(t, json.Unmarshal(data, &plan))
	return plan
}

func TestDevinClientUserJourneysAuthorized(t *testing.T) {
	if os.Getenv("DEVIN_JOURNEY_REAL") != "true" {
		t.Skip("requires a new explicit plan/approval; never runs in mock CI")
	}
	_ = approvedPlan(t)
	runDevinCloudGatewaySuite(t, os.Getenv("DEVIN_JOURNEY_RUN_MODE"), nil, "journeys")
}

func approvedGroupPricing(t *testing.T, plan approvedJourneyPlan) []any {
	t.Helper()
	name := os.Getenv("DEVIN_JOURNEY_GROUP_PRICING_FILE")
	if plan.GroupPricingSHA == "" {
		require.Empty(t, name, "unreviewed pricing input is forbidden")
		return nil
	}
	data, err := os.ReadFile(name)
	require.NoError(t, err, "provide only the approved non-secret group pricing snapshot")
	sum := sha256.Sum256(data)
	require.Equal(t, plan.GroupPricingSHA, hex.EncodeToString(sum[:]))
	var prices []any
	require.NoError(t, json.Unmarshal(data, &prices))
	require.NotEmpty(t, prices)
	return prices
}

func journeyPricingProof(t *testing.T, app *application, key string, models []string) []map[string]any {
	t.Helper()
	raw := request(t, app, "GET", "/v1/models?pricing_models="+url.QueryEscape(strings.Join(models, ",")), key, nil, 200)
	require.Equal(t, "devin.pricing_admission", gjson.GetBytes(raw, "object").String())
	var response struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Len(t, response.Data, len(models))
	for i, entry := range response.Data {
		require.Equal(t, models[i], entry["model"])
	}
	return response.Data
}

func runAuthorizedUserJourneys(t *testing.T, app *application, mode, admin, key, password string, keyID, accountID int64, catalog devin.CatalogSnapshot) {
	t.Helper()
	plan := approvedPlan(t)
	for _, uid := range []string{plan.Models.Direct, plan.Models.Lead, plan.Models.Sidekick} {
		_, err := selectSmokeModel(eligibleAgentModels(catalog.Models), uid, plan.Limits.Output)
		require.NoError(t, err, "every exact approved model must be granted and tool capable; do not silently substitute a model")
	}
	profile := fusion.DefaultProfile("fusion-project-journey", plan.Models.Lead, plan.Models.Sidekick)
	profile.MaxHandoffs, profile.MaxRequests, profile.MaxTools = plan.Limits.Handoffs, plan.Limits.Calls, plan.Limits.Tools
	profile.MaxCostUSD, profile.MaxWallSeconds, profile.MaxOutputTokens = plan.Limits.TurnUSD, plan.Limits.TurnSeconds, plan.Limits.Output
	profile.Keepalive = &fusion.KeepalivePolicy{Enabled: false}
	current := api(t, app, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil)
	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(current.Get("extra").Raw), &extra))
	extra["devin_fusion_enabled"], extra["devin_fusion_profiles"] = true, []fusion.Profile{profile}
	api(t, app, "PUT", fmt.Sprintf("/admin/accounts/%d", accountID), admin, map[string]any{"extra": extra, "concurrency": 1})
	var proofs []map[string]any
	reports := []map[string]any{}
	budget := &authorizedJourneyBudget{db: agentDatabase(t, app), keyID: keyID}
	defer func() {
		cost, err := budget.cost()
		writeAgentArtifact(t, "private-product-live.json", map[string]any{
			"schema_version": 1, "tier": "product_live_partial", "real_upstream": true, "eligible_for_real_e2e": false,
			"source": journeySource(t, app), "mode": mode, "cases": reports, "physical_pricing": proofs,
			"pricing_scope": "isolated preview public key/group/channel; no claim about deployed production configuration",
			"billing":       map[string]any{"local_cost_available": err == nil, "observed_local_cost_usd": cost, "provider_spending_hard_cap": false},
			"unverified":    []string{"deployed existing group/channel", "OMO Senpi identity", "in-flight disconnect", "client workers", "non-OMP compact/fork"},
		})
	}()
	proofs = journeyPricingProof(t, app, key, []string{plan.Models.Direct, plan.Models.Lead, plan.Models.Sidekick})
	for _, entry := range proofs {
		require.Equal(t, true, entry["identified"], "physical UID %v has no identified price in this public key's group/channel; stop before inference", entry["model"])
		require.Equal(t, true, entry["calculable"], "physical UID %v price cannot be calculated; stop before inference", entry["model"])
	}
	for _, client := range plan.Clients {
		for _, kind := range []string{"direct", "fusion"} {
			profiles := []string{"project"}
			if client == "omp" {
				profiles = []string{"default", "compatibility"}
			}
			for _, configProfile := range profiles {
				id := client + "/" + kind + "/" + configProfile
				if !t.Run(id, func(t *testing.T) {
					model := plan.Models.Direct
					if kind == "fusion" {
						model = profile.PublicModel
					}
					directory, err := os.MkdirTemp("", "devin-approved-project-")
					require.NoError(t, err)
					t.Cleanup(func() { _ = os.RemoveAll(directory) })
					admission := newJourneyAdmission(t, app, model, plan.Limits.Requests)
					admission.runBudget, admission.outputLimit = budget, plan.Limits.Output
					ui := journeyUIServer(t, app.base, admission.server.URL)
					script, err := filepath.Abs("../../../tools/devin-e2e-clients/journey.py")
					require.NoError(t, err)
					ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
					defer cancel()
					stage := "task"
					if mode == "plan" {
						stage = "handoff"
					}
					cmd := exec.CommandContext(ctx, "python3", script, "--directory", directory, "--client", client, "--model", model, "--ui-url", ui.URL, "--profile", configProfile, "--stage", stage, "--upstream", "real")
					for _, variable := range os.Environ() {
						name, _, _ := strings.Cut(variable, "=")
						if strings.Contains(" DEVIN_E2E_PAT DEVIN_E2E_CREDENTIAL DEVIN_PAT DEVIN_API_KEY SUB2API_API_KEY SUB2API_USER_TOKEN DEVIN_JOURNEY_PLAN_FILE DEVIN_JOURNEY_APPROVAL_FILE DEVIN_JOURNEY_RUN_LEDGER ", " "+name+" ") {
							continue
						}
						cmd.Env = append(cmd.Env, variable)
					}
					cmd.Env = append(cmd.Env, "DEVIN_JOURNEY_GATEWAY_KEY="+key, "DEVIN_JOURNEY_UI_PASSWORD="+password)
					output, runErr := cmd.Output()
					report := map[string]any{"status": "failed", "stage": "runner_output"}
					parseErr := json.Unmarshal(output, &report)
					forwarded, records := admission.snapshot()
					report["id"], report["model_kind"], report["public_model_uid"] = id, kind, model
					report["physical_lead_uid"], report["physical_sidekick_uid"] = plan.Models.Direct, ""
					if kind == "fusion" {
						report["physical_lead_uid"], report["physical_sidekick_uid"] = plan.Models.Lead, plan.Models.Sidekick
					}
					report["public_requests"], report["reasoning_scope"] = forwarded, "exact approved physical UIDs; OMP thinking off, OpenCode/OMO client default"
					usageModels := map[string]int{}
					rows, dbErr := budget.db.Query(`SELECT coalesce(nullif(upstream_model,''),model),count(*) FROM usage_logs WHERE api_key_id=$1 GROUP BY 1`, keyID)
					if dbErr == nil {
						for rows.Next() {
							var uid string
							var count int
							if rows.Scan(&uid, &count) != nil {
								dbErr = fmt.Errorf("usage scan unavailable")
								break
							}
							usageModels[uid] = count
						}
						if err := rows.Err(); err != nil {
							dbErr = err
						}
						_ = rows.Close()
					}
					report["observed_physical_models_cumulative"] = usageModels
					report["usage_scope"] = "persisted local public-key execution records, cumulative; not a provider invoice"
					reports = append(reports, report)
					savePrivateAgentUsage(t, app, keyID, client, kind)
					require.NoError(t, parseErr, "retain private structural evidence; never repeat an unknown send")
					require.NoError(t, runErr, "first client failure stops this approved batch")
					require.True(t, budget.healthy(), "global request accounting failed or an earlier send is uncertain; stop")
					require.NoError(t, dbErr, "physical-model execution evidence unavailable")
					if report["status"] == "unsupported_by_product" {
						require.Zero(t, forwarded)
						return
					}
					if mode == "plan" {
						require.Equal(t, "handoff_only", report["status"])
						require.Zero(t, forwarded)
						return
					}
					require.Equal(t, "passed", report["status"])
					if kind == "direct" {
						require.Positive(t, usageModels[plan.Models.Direct], "actual physical model execution not persisted")
					}
					if kind == "fusion" {
						verifyAgentFusion(t, app, keyID, records, "private-product-"+client, true)
					}
					if client == "omp" && kind == "direct" && configProfile == "compatibility" && os.Getenv("DEVIN_JOURNEY_CAPABILITIES") == "true" {
						capabilities, ok := report["capability_evidence"].(map[string]any)
						require.True(t, ok)
						require.Equal(t, "passed", capabilities["status"])
					}
				}) {
					t.Fatal("approved real journey stopped at first failure; no automatic batch retry")
				}
			}
		}
	}
}
