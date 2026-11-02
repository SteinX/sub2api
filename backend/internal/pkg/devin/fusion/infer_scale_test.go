package fusion

import (
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

func ambiguousAliasCatalog(count int) []*pb.ClientModelConfig {
	var models []*pb.ClientModelConfig
	for _, role := range []string{"lead", "sidekick"} {
		for i := 0; i < count; i++ {
			models = append(models, &pb.ClientModelConfig{ModelUid: fmt.Sprintf("synthetic-%s-%d", role, i),
				ModelInfo: &pb.ModelInfo{ModelUid: "shared-" + role, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}})
		}
	}
	return models
}

func TestInferLargeAliasSetsRemainAmbiguous(t *testing.T) {
	models := ambiguousAliasCatalog(80)
	_, err := InferProfile("fusion-shared-lead-sidekick-shared-sidekick", models)
	requireCode(t, err, "pair_ambiguous")
	_, err = InferProfile("fusion-shared-lead-sidekick-missing", models)
	requireCode(t, err, "pair_unresolved")
}

func BenchmarkInferAmbiguousAliasSets(b *testing.B) {
	for _, count := range []int{80, 160} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			models := ambiguousAliasCatalog(count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := InferProfile("fusion-shared-lead-sidekick-shared-sidekick", models); err == nil {
					b.Fatal("ambiguous pair accepted")
				}
			}
		})
	}
}
