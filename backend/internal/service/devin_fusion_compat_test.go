//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

type fusionCompatBillingCache struct{ *fusionTestBillingCache }

func (*fusionCompatBillingCache) GetAPIKeyRateLimit(context.Context, int64) (*APIKeyRateLimitCacheData, error) {
	now := time.Now().Unix()
	return &APIKeyRateLimitCacheData{Window5h: now, Window1d: now, Window7d: now}, nil
}
func (*fusionCompatBillingCache) UpdateAPIKeyRateLimitUsage(context.Context, int64, float64) error {
	return nil
}

func TestDevinFusionEntryPreservesClientToolDeclarations(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, transport := range []string{"JSON", "SSE", "WS"} {
			t.Run(map[bool]string{false: "namespace", true: "custom"}[custom]+"/"+transport, func(t *testing.T) {
				stream := transport != "JSON"
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
					return []*pb.GetChatMessageResponse{{DeltaToolCalls: []*pb.ChatToolCall{{Id: fmt.Sprintf("native-tool-%d", turn), Name: name, ArgumentsJson: args}}, StopReason: pb.StopReason_STOP_REASON_FUNCTION_CALL, ActualModelUid: &profile.LeadModel, Usage: &pb.ModelUsageStats{InputTokens: 10, OutputTokens: 5, BillingModelUid: q.ChatModelUid}}}
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
				s.billingCacheService = NewBillingCacheService(&fusionCompatBillingCache{&fusionTestBillingCache{newMemInflightCache(100)}}, nil, nil, nil, nil, nil, cfg, nil)
				defer s.billingCacheService.Stop()
				declaration := `{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{}}}]}`
				if custom {
					declaration = `{"type":"custom","name":"apply_patch"}`
				}
				body := []byte(`{"model":"fusion-team","input":"fix it","stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,"tools":[` + declaration + `]}`)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				key := &APIKey{ID: 22, UserID: 33, Quota: 10, RateLimit1d: 10, User: &User{ID: 33, Balance: 100}}
				c.Set("api_key", key)
				updater := &openAIRecordUsageAPIKeyQuotaStub{}
				bound := false
				var err error
				var result *OpenAIForwardResult
				if transport == "WS" {
					bound, err = s.BindDevinFusionWebSocketRequest(c, key, profile.PublicModel, body, updater, func([]byte) error { return nil })
				} else {
					bound, err = s.BindDevinFusionRequest(c, key, profile.PublicModel, body, "responses", updater)
				}
				require.NoError(t, err)
				require.True(t, bound)
				if transport == "WS" {
					result, err = s.proxyDevinWSBridgeTurn(c.Request.Context(), c, account, body, func(message []byte) error {
						_, writeErr := w.Write(message)
						return writeErr
					})
				} else {
					result, err = s.forwardDevin(c.Request.Context(), c, account, body, "responses", "session", "")
				}
				require.NoError(t, err, w.Body.String())
				require.True(t, result.FusionUsageSettled)
				require.Equal(t, 1, turn)
				require.Len(t, billing.commands, 1)
				require.Positive(t, billing.commands[0].APIKeyQuotaCost)
				require.Positive(t, billing.commands[0].APIKeyRateLimitCost)
				if custom {
					require.Contains(t, w.Body.String(), `"type":"custom_tool_call"`)
					require.Contains(t, w.Body.String(), `"input":"patch"`)
				} else {
					require.Contains(t, w.Body.String(), `"namespace":"files"`)
					require.Contains(t, w.Body.String(), `"name":"read"`)
				}
				if transport == "WS" {
					require.NotEmpty(t, result.wsReplayInput)
					callID := gjson.GetBytes(result.wsReplayInput[0], "call_id").String()
					outputType := "function_call_output"
					if custom {
						outputType = "custom_tool_call_output"
					}
					followup, marshalErr := json.Marshal(map[string]any{"model": profile.PublicModel, "previous_response_id": result.ResponseID, "input": []any{map[string]string{"type": outputType, "call_id": callID, "output": "applied"}}})
					require.NoError(t, marshalErr)
					result, err = s.proxyDevinWSBridgeTurn(c.Request.Context(), c, account, followup, func([]byte) error { return nil })
					require.NoError(t, err)
					require.True(t, result.FusionUsageSettled)
					require.Equal(t, 2, turn)
					require.Len(t, billing.commands, 2)
					binding, ok := c.Request.Context().Value(devinFusionBindingKey{}).(*devinFusionBinding)
					require.True(t, ok)
					controlWriter := httptest.NewRecorder()
					reconnect, _ := gin.CreateTestContext(controlWriter)
					reconnect.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
					reconnect.Request.Header.Set(fusion.HeaderSession, binding.SessionID)
					write := func(message []byte) error {
						require.Equal(t, "cancel_requested", gjson.GetBytes(message, "status").String())
						require.Equal(t, "fusion.session", gjson.GetBytes(message, "type").String())
						return nil
					}
					bound, err = s.BindDevinFusionWebSocketRequest(reconnect, key, profile.PublicModel, body, updater, write)
					require.NoError(t, err)
					require.True(t, bound)
					require.Equal(t, account.ID, devinFusionBoundAccount(reconnect.Request.Context()), "reconnect must pin scheduling to the durable owner")
					reconnect.Request.Header.Set(fusion.HeaderCancel, "true")
					_, err = s.BindDevinFusionWebSocketRequest(reconnect, key, profile.PublicModel, body, updater, write)
					require.True(t, DevinFusionHandled(err))
					require.Empty(t, controlWriter.Body.String(), "an upgraded request must send controls through WS frames")
					require.Equal(t, 2, turn, "binding and cancellation must not start inference")
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
