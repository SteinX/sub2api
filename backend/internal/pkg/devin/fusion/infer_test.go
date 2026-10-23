package fusion

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestFusionCatalogFastAliasPreservesEffortAndLane(t *testing.T) {
	for _, fast := range []bool{true, false} {
		models := inferenceModels("gpt-6-1-sol-high", "gpt-6-1-sol-high-priority", "worker")
		if fast {
			models[1].ModelFamilyMetadata = &pb.ModelFamilyMetadata{Entries: []*pb.ModelFamilyMetadataEntry{{Key: "Fast Mode", Value: &pb.ModelFamilyMetadataValue{Order: 1}}}}
		}
		profile, err := InferProfile("fusion-gpt-6-1-sol-high-fast-sidekick-worker", models)
		if fast {
			require.NoError(t, err)
			require.Equal(t, "gpt-6-1-sol-high-priority", profile.LeadModel)
			require.True(t, profile.KeepalivePolicy().Enabled)
		} else {
			require.Error(t, err)
		}
		_, err = InferProfile("fusion-gpt-6-1-sol-max-fast-sidekick-worker", models)
		require.Error(t, err, "must not change the requested effort")
		models[1].Disabled = true
		_, err = InferProfile("fusion-gpt-6-1-sol-high-fast-sidekick-worker", models)
		require.Error(t, err, "must not fall back to ordinary high when Fast is disabled")
	}
}

func inferenceModels(ids ...string) []*pb.ClientModelConfig {
	var models []*pb.ClientModelConfig
	for _, id := range ids {
		models = append(models, &pb.ClientModelConfig{ModelUid: id, ModelInfo: &pb.ModelInfo{ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}})
	}
	return models
}
func TestFusionInferDefaultPairFromModelID(t *testing.T) {
	for _, id := range []string{"fusion-lead-fast-priority-sidekick-worker-many-hyphens", "FUSION-lead-fast-priority-SIDEKICK-worker-many-hyphens"} {
		p, err := InferProfile(id, inferenceModels("lead-fast-priority", "worker-many-hyphens"))
		require.NoError(t, err)
		require.Equal(t, "lead-fast-priority", p.LeadModel)
		require.Equal(t, "worker-many-hyphens", p.SidekickModel)
		require.True(t, p.Enabled)
		require.Positive(t, p.MaxCostUSD)
		require.NoError(t, p.Validate())
	}
	models := inferenceModels("lead.real.v1", "worker_real_v2")
	p, err := InferProfile("fusion-lead-real-v1-sidekick-worker-real-v2", models)
	require.NoError(t, err)
	require.Equal(t, "lead.real.v1", p.LeadModel)
	require.Equal(t, "worker_real_v2", p.SidekickModel)
}
func TestFusionInferenceRejectsMissingAmbiguousAndNonConcreteMembers(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		models   []*pb.ClientModelConfig
		code     string
	}{
		{"missing worker", "fusion-lead-sidekick-worker", inferenceModels("lead"), "pair_unresolved"},
		{"missing lead", "fusion-lead-sidekick-worker", inferenceModels("worker"), "pair_unresolved"},
		{"not encoded", "fusion", inferenceModels("lead", "worker"), "pair_unresolved"},
		{"empty member", "fusion-lead-sidekick-", inferenceModels("lead", "worker"), "pair_unresolved"},
		{"ambiguous punctuation", "fusion-lead-real-v1-sidekick-worker", inferenceModels("lead.real_v1", "lead_real.v1", "worker"), "pair_ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) { _, err := InferProfile(tc.id, tc.models); requireCode(t, err, tc.code) })
	}
	for _, kind := range []string{"router", "harness", "disabled", "no tools"} {
		t.Run(kind, func(t *testing.T) {
			models := inferenceModels("lead", "worker")
			m := models[1]
			switch kind {
			case "router":
				m.ModelInfo.IsModelRouter = true
			case "harness":
				m.ModelInfo.HarnessUids = []string{"opaque"}
			case "disabled":
				m.Disabled = true
			case "no tools":
				m.ModelInfo.ModelFeatures.SupportsToolCalls = false
			}
			_, err := InferProfile("fusion-lead-sidekick-worker", models)
			requireCode(t, err, "pair_unresolved")
		})
	}
}
func TestFusionInferenceUsesConcreteCatalogAliasAndNeverPublicHarness(t *testing.T) {
	models := inferenceModels("concrete-lead", "worker")
	models[0].ModelInfo.ModelUid = "lead-alias"
	models = append(models, &pb.ClientModelConfig{ModelUid: "fusion-lead-alias-sidekick-worker", ModelInfo: &pb.ModelInfo{IsModelRouter: true, HarnessUids: []string{"official-opaque-harness"}}})
	p, err := InferProfile("fusion-lead-alias-sidekick-worker", models)
	require.NoError(t, err)
	require.Equal(t, "concrete-lead", p.LeadModel)
	require.Equal(t, "worker", p.SidekickModel)
}

func TestFusionInfersNativeMembersWithClientHarnessHints(t *testing.T) {
	const public = "fusion-claude-opus-5-5-medium-sidekick-swe-2-medium"
	models := inferenceModels("claude-opus-5-5-medium", "swe-2-medium")
	models[0].ModelInfo.HarnessUids = []string{"strawberry-pancake"}
	models[1].ModelInfo.HarnessUids = []string{"swe-1p6-exec", "swe-1p6", "swe-1p5"}
	models = append(models, &pb.ClientModelConfig{ModelUid: public, ModelInfo: &pb.ModelInfo{IsModelRouter: true, HarnessUids: []string{"fusion", "strawberry-pancake"}}})
	profile, err := InferProfile(public, models)
	require.NoError(t, err)
	require.Equal(t, models[0].ModelUid, profile.LeadModel)
	require.Equal(t, models[1].ModelUid, profile.SidekickModel)
	models[1].ModelInfo.HarnessUids = append(models[1].ModelInfo.HarnessUids, "unverified-future-harness")
	_, err = InferProfile(public, models)
	requireCode(t, err, "pair_unresolved")
}
