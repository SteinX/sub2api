package service

import (
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

// Devin speaks function tools upstream. Preserve the client's tool contract in
// both directions using the same reversible adapter as the other gateways.
func adaptDevinResponsesClientTools(body []byte) ([]byte, apicompat.ResponsesClientToolMapping, error) {
	if !json.Valid(body) {
		return body, apicompat.ResponsesClientToolMapping{}, fmt.Errorf("invalid Responses request JSON")
	}
	return adaptResponsesClientToolsForFunctionUpstream(body, "Devin")
}

func applyDevinGenerationOptions(wire *pb.GetChatMessageRequest, body []byte, protocol string) error {
	var options struct {
		N             *int            `json:"n"`
		Stop          json.RawMessage `json:"stop"`
		StopSequences []string        `json:"stop_sequences"`
		TopK          *uint64         `json:"top_k"`
	}
	if err := json.Unmarshal(body, &options); err != nil {
		return err
	}
	if options.N != nil && *options.N != 1 {
		return fmt.Errorf("devin supports only n=1")
	}
	if options.TopK != nil {
		wire.Configuration.TopK = *options.TopK
	}
	if protocol == "anthropic" {
		wire.Configuration.StopPatterns = options.StopSequences
	} else if len(options.Stop) > 0 && string(options.Stop) != "null" {
		var single string
		if json.Unmarshal(options.Stop, &single) == nil {
			wire.Configuration.StopPatterns = []string{single}
		} else {
			var sequences []string
			if err := json.Unmarshal(options.Stop, &sequences); err != nil {
				return fmt.Errorf("stop must be a string or an array of strings")
			}
			wire.Configuration.StopPatterns = sequences
		}
	}
	return nil
}

func devinChatReasoningSignature(response *apicompat.ResponsesResponse) string {
	var signature string
	for _, item := range response.Output {
		if item.Type == "reasoning" && item.EncryptedContent != "" {
			signature = item.EncryptedContent
		}
	}
	return signature
}
