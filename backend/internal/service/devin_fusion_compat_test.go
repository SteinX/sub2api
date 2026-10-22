//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDevinFusionEntryPreservesClientToolDeclarations(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(map[bool]string{false: "namespace", true: "custom"}[custom]+map[bool]string{false: "/JSON", true: "/SSE"}[stream], func(t *testing.T) {
				profile := fusionTestProfile()
				turn := 0
				gateway := devinCompatGateway(t, func(q *pb.GetChatMessageRequest) []*pb.GetChatMessageResponse {
					turn++
					name := ""
					for _, tool := range q.Tools {
						if tool.Name != fusion.DelegateTool {
							name = tool.Name
						}
					}
					assert.NotEmpty(t, name)
					assert.Equal(t, profile.LeadModel, q.ChatModelUid)
					args := `{}`
					if custom {
						args = `{"input":"patch"}`
					}
					return []*pb.GetChatMessageResponse{{DeltaToolCalls: []*pb.ChatToolCall{{Id: "native-tool", Name: name, ArgumentsJson: args}}, StopReason: pb.StopReason_STOP_REASON_FUNCTION_CALL, ActualModelUid: &profile.LeadModel, Usage: &pb.ModelUsageStats{InputTokens: 10, OutputTokens: 5, BillingModelUid: q.ChatModelUid}}}
				})
				cfg := &config.Config{}
				cfg.Default.RateMultiplier = 1
				cfg.Billing.InflightReservation.Enabled = true
				models := []*pb.ClientModelConfig{}
				for _, uid := range []string{profile.LeadModel, profile.SidekickModel} {
					models = append(models, &pb.ClientModelConfig{ModelUid: uid, MaxTokens: 262144, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}})
				}
				account := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "test"}, Extra: devinCatalogExtra(models, "fixture")}
				account.Extra[fusion.ExtraProfiles] = []fusion.Profile{profile}
				upstream := gateway.devin.upstream.(*devinHTTP)
				original := upstream.handle
				upstream.handle = func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == devin.ModelsPath {
						return devinProtoReply(t, &pb.GetCliModelConfigsResponse{ClientModelConfigs: models}), nil
					}
					return original(r)
				}
				billing := &fusionBillingRepo{}
				s := newOpenAIRecordUsageServiceWithBillingRepoForTest(&openAIRecordUsageLogRepoStub{inserted: true}, billing, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
				s.cfg, s.devin, s.accountRepo = cfg, gateway.devin, &fusionAccountRepo{account: account}
				s.billingCacheService = NewBillingCacheService(&fusionTestBillingCache{newMemInflightCache(100)}, nil, nil, nil, nil, nil, cfg, nil)
				defer s.billingCacheService.Stop()
				declaration := `{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{}}}]}`
				if custom {
					declaration = `{"type":"custom","name":"apply_patch"}`
				}
				body := []byte(`{"model":"fusion-team","input":"fix it","stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,"tools":[` + declaration + `]}`)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				key := &APIKey{ID: 22, UserID: 33, User: &User{ID: 33, Balance: 100}}
				c.Set("api_key", key)
				bound, err := s.BindDevinFusionRequest(c, key, profile.PublicModel, body, "responses", &openAIRecordUsageAPIKeyQuotaStub{})
				require.NoError(t, err)
				require.True(t, bound)
				result, err := s.forwardDevin(c.Request.Context(), c, account, body, "responses", "session", "")
				require.NoError(t, err, w.Body.String())
				require.True(t, result.FusionUsageSettled)
				require.Equal(t, 1, turn)
				require.Len(t, billing.commands, 1)
				if custom {
					require.Contains(t, w.Body.String(), `"type":"custom_tool_call"`)
					require.Contains(t, w.Body.String(), `"input":"patch"`)
				} else {
					require.Contains(t, w.Body.String(), `"namespace":"files"`)
					require.Contains(t, w.Body.String(), `"name":"read"`)
				}
			})
		}
	}
}

func TestDevinFusionChatReasoningSignatureAndStreamUsage(t *testing.T) {
	decoder := devin.NewEventDecoder("fusion-team", func(apicompat.ResponsesStreamEvent) error { return nil })
	require.NoError(t, decoder.Apply(&pb.GetChatMessageResponse{MessageId: "assistant", DeltaThinking: "thinking", DeltaSignature: "signature", DeltaText: "answer", Usage: &pb.ModelUsageStats{InputTokens: 3, OutputTokens: 2}, StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN}))
	require.NoError(t, decoder.Finish())
	for _, stream := range []bool{false, true} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(context.Background())
		require.NoError(t, writeDevinFusionResponse(c, &decoder.Response, "chat", stream, apicompat.ResponsesClientToolMapping{}, true))
		if !stream {
			require.Equal(t, devinChatReasoningSignature(&decoder.Response), gjson.Get(w.Body.String(), "choices.0.message.reasoning_signature").String())
			continue
		}
		require.Contains(t, w.Body.String(), `"reasoning_content":"thinking"`)
		var signature string
		usage := false
		for _, line := range strings.Split(w.Body.String(), "\n") {
			if !strings.HasPrefix(line, "data: {") {
				continue
			}
			var chunk apicompat.ChatCompletionsChunk
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk))
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.ReasoningSignature != nil {
				signature = *chunk.Choices[0].Delta.ReasoningSignature
			}
			if chunk.Usage != nil {
				usage = chunk.Usage.PromptTokens == 3
			}
		}
		require.Equal(t, devinChatReasoningSignature(&decoder.Response), signature)
		require.True(t, usage)
	}
}
