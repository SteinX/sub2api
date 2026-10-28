package devin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

// ApplyModelConfig keeps account probes and protocol adapters on the same model
// limits and capabilities advertised by the authenticated CLI catalog.
func ApplyModelConfig(q *pb.GetChatMessageRequest, model *pb.ClientModelConfig, request *apicompat.ResponsesRequest) error {
	features := model.GetModelInfo().GetModelFeatures()
	if err := ValidateModelCapabilities(q, model, request.MaxOutputTokens); err != nil {
		return err
	}
	if features != nil && !features.GetSupportsParallelToolCalls() {
		q.DisableParallelToolCalls = true
	}
	return nil
}
