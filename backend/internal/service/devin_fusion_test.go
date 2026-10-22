//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type fusionAccountRepo struct {
	AccountRepository
	account *Account
}

func (r *fusionAccountRepo) ListSchedulable(context.Context) ([]Account, error) {
	return []Account{*r.account}, nil
}
func (r *fusionAccountRepo) ListSchedulableByGroupID(context.Context, int64) ([]Account, error) {
	return []Account{*r.account}, nil
}
func (r *fusionAccountRepo) GetByID(context.Context, int64) (*Account, error) { return r.account, nil }

type fusionTestBillingCache struct{ *memInflightCache }

func (c *fusionTestBillingCache) ReserveInflightScope(context.Context, string, string, float64, time.Duration) (float64, error) {
	return 0, nil
}
func (c *fusionTestBillingCache) ReleaseInflightScope(context.Context, string, string) error {
	return nil
}

type fusionBillingRepo struct {
	UsageBillingRepository
	commands []*UsageBillingCommand
	seen     map[string]bool
}

func (r *fusionBillingRepo) Apply(_ context.Context, c *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	if r.seen[c.RequestID] {
		return &UsageBillingApplyResult{Applied: false}, nil
	}
	r.seen[c.RequestID] = true
	copy := *c
	r.commands = append(r.commands, &copy)
	return &UsageBillingApplyResult{Applied: true}, nil
}
func fusionTestProfile() fusion.Profile {
	return fusion.Profile{Enabled: true, PublicModel: "fusion-team", LeadModel: "gpt-5.5", SidekickModel: "gpt-5.4-nano", MaxHandoffs: 4, MaxRequests: 20, MaxTools: 40, MaxWallSeconds: 900, MaxCostUSD: 10, MaxContextBytes: 262144, MaxOutputTokens: 1024, TTLSeconds: 3600}
}
func fusionProtocolBody(protocol string, stream bool, callID string) []byte {
	if protocol == "responses" {
		input := any("fix the bug")
		if callID != "" {
			input = []any{map[string]any{"type": "function_call_output", "call_id": callID, "output": "verified private file content"}}
		}
		b, _ := json.Marshal(map[string]any{"model": "fusion-team", "stream": stream, "input": input, "tools": []any{map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}, "max_output_tokens": 128})
		return b
	}
	messages := []any{map[string]any{"role": "user", "content": "fix the bug"}}
	if protocol == "chat" {
		if callID != "" {
			messages = append(messages, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"main.go"}`}}}}, map[string]any{"role": "tool", "tool_call_id": callID, "content": "verified private file content"})
		}
		b, _ := json.Marshal(map[string]any{"model": "fusion-team", "stream": stream, "messages": messages, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}}, "max_completion_tokens": 128})
		return b
	}
	if callID != "" {
		messages = append(messages, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": callID, "name": "read_file", "input": map[string]any{"path": "main.go"}}}}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": "verified private file content"}}})
	}
	b, _ := json.Marshal(map[string]any{"model": "fusion-team", "stream": stream, "messages": messages, "tools": []any{map[string]any{"name": "read_file", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}, "max_tokens": 128})
	return b
}

func TestFusionGatewayAcrossProtocolsAndBilling(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		for _, protocol := range []string{"responses", "chat", "anthropic"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("automatic=%t/%s/stream=%t", automatic, protocol, stream), func(t *testing.T) {
					profile := fusionTestProfile()
					if automatic {
						profile = fusion.DefaultProfile("fusion-"+profile.LeadModel+"-sidekick-"+profile.SidekickModel, profile.LeadModel, profile.SidekickModel)
					}
					models := []*pb.ClientModelConfig{}
					for _, uid := range []string{profile.LeadModel, profile.SidekickModel} {
						models = append(models, &pb.ClientModelConfig{ModelUid: uid, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}})
					}
					if automatic {
						models = append(models, &pb.ClientModelConfig{ModelUid: profile.PublicModel, ModelInfo: &pb.ModelInfo{IsModelRouter: true, HarnessUids: []string{"opaque-public-harness"}}})
					}
					a := &Account{ID: 7, Platform: PlatformDevin, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2, Credentials: map[string]any{"access_token": "synthetic-token"}, Extra: devinCatalogExtra(models, "fixture")}
					if !automatic {
						a.Extra[fusion.ExtraProfiles] = []fusion.Profile{profile}
					}
					require.True(t, a.supportsDevinModel(profile.PublicModel))
					require.NoError(t, ValidateDevinFusionExtra(a.Platform, a.Extra))
					var wires []*pb.GetChatMessageRequest
					upstream := &devinHTTP{handle: func(r *http.Request) (*http.Response, error) {
						var reply proto.Message
						switch r.URL.Path {
						case devin.ModelsPath:
							reply = &pb.GetCliModelConfigsResponse{ClientModelConfigs: models}
						case devin.UserJWTPath:
							reply = &pb.GetUserJwtResponse{UserJwt: "private-jwt"}
						case devin.ChatPath:
							_, b, err := devin.ReadFrame(r.Body)
							assert.NoError(t, err)
							wire := &pb.GetChatMessageRequest{}
							assert.NoError(t, proto.Unmarshal(b, wire))
							wires = append(wires, wire)
							message := &pb.GetChatMessageResponse{ActualModelUid: proto.String(wire.ChatModelUid), MessageId: fmt.Sprint(len(wires)), StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN, Usage: &pb.ModelUsageStats{InputTokens: 100, CacheReadTokens: 40, CacheWriteTokens: 10, OutputTokens: 20, BillingModelUid: wire.ChatModelUid}}
							switch len(wires) {
							case 1:
								assert.Equal(t, profile.LeadModel, wire.ChatModelUid)
								message.DeltaToolCalls = []*pb.ChatToolCall{{Id: "internal-delegation", Name: fusion.DelegateTool, ArgumentsJson: `{"task":"fix bug","acceptance_criteria":"read and verify main.go"}`}}
							case 2:
								assert.Equal(t, profile.SidekickModel, wire.ChatModelUid)
								message.DeltaText = "private Sidekick planning"
								message.DeltaToolCalls = []*pb.ChatToolCall{{Id: "internal-read", Name: "read_file", ArgumentsJson: `{"path":"main.go"}`}}
							case 3:
								assert.Equal(t, profile.SidekickModel, wire.ChatModelUid)
								assert.Contains(t, fmt.Sprint(wire.ChatMessagePrompts), "verified private file content")
								message.DeltaText = "Private Sidekick report: bug located; read_file evidence verified"
							case 4:
								assert.Equal(t, profile.LeadModel, wire.ChatModelUid)
								assert.Contains(t, fmt.Sprint(wire.ChatMessagePrompts), "Private Sidekick report")
								assert.NotContains(t, fmt.Sprint(wire.ChatMessagePrompts), "verified private file content")
								message.DeltaText = "Reviewed and completed the task."
							default:
								t.Errorf("unexpected repeat inference %d", len(wires))
								return devinReply(500, nil), nil
							}
							b, err = proto.Marshal(message)
							assert.NoError(t, err)
							return devinReply(200, append(devinTestFrame(0, b), devinTestFrame(2, []byte(`{}`))...)), nil
						default:
							t.Errorf("unexpected endpoint %s", r.URL.Path)
							return devinReply(404, nil), nil
						}
						b, err := proto.Marshal(reply)
						assert.NoError(t, err)
						return devinReply(200, b), nil
					}}
					rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
					defer rdb.Close()
					repo := &fusionAccountRepo{account: a}
					billingRepo := &fusionBillingRepo{}
					usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
					svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
					cfg := &config.Config{}
					cfg.Default.RateMultiplier = 1
					cfg.Billing.InflightReservation.Enabled = true
					svc.cfg = cfg
					cache := &fusionTestBillingCache{newMemInflightCache(100)}
					svc.billingCacheService = NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
					defer svc.billingCacheService.Stop()
					svc.accountRepo = repo
					svc.devin = NewDevinService(repo, nil, upstream, cfg, devin.NewRedisSessionStore(rdb))
					key := &APIKey{ID: 22, UserID: 33, User: &User{ID: 33, Balance: 100}}
					request := func(body []byte) (*httptest.ResponseRecorder, *OpenAIForwardResult, error) {
						body = []byte(strings.ReplaceAll(string(body), "fusion-team", profile.PublicModel))
						w := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(w)
						ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "same-public-request-id")
						c.Request = httptest.NewRequest("POST", "/v1/"+protocol, nil).WithContext(ctx)
						c.Set("api_key", key)
						recognized, err := svc.BindDevinFusionRequest(c, key, profile.PublicModel, body, protocol, &openAIRecordUsageAPIKeyQuotaStub{})
						require.NoError(t, err)
						require.True(t, recognized)
						result, err := svc.forwardDevin(c.Request.Context(), c, a, body, protocol, "ignored-cache-key", "")
						return w, result, err
					}
					w, first, err := request(fusionProtocolBody(protocol, stream, ""))
					require.NoError(t, err)
					require.Equal(t, 200, w.Code, w.Body.String())
					require.True(t, first.FusionUsageSettled)
					require.Equal(t, 300, first.Usage.InputTokens)
					require.Len(t, wires, 2)
					require.Len(t, billingRepo.commands, 2)
					callID := regexp.MustCompile(`call_fusion_[0-9a-f-]+`).FindString(w.Body.String())
					require.NotEmpty(t, callID)
					require.NotContains(t, w.Body.String(), fusion.DelegateTool)
					require.NotContains(t, w.Body.String(), "private Sidekick planning")
					require.NotContains(t, w.Body.String(), "internal-read")
					require.NotEmpty(t, w.Header().Get(fusion.HeaderSession))
					body := fusionProtocolBody(protocol, stream, callID)
					w, last, err := request(body)
					require.NoError(t, err)
					require.Equal(t, 200, w.Code, w.Body.String())
					require.Contains(t, w.Body.String(), "Reviewed and completed")
					require.NotContains(t, w.Body.String(), "Private Sidekick report")
					require.Equal(t, 300, last.Usage.InputTokens)
					require.Len(t, wires, 4)
					require.Len(t, billingRepo.commands, 4)
					require.Equal(t, wires[0].CascadeId, wires[3].CascadeId)
					require.Equal(t, wires[1].CascadeId, wires[2].CascadeId)
					require.NotEqual(t, wires[0].CascadeId, wires[1].CascadeId)
					for i, wire := range wires {
						for j := 0; j < i; j++ {
							require.NotEqual(t, wires[j].ExecutionId, wire.ExecutionId)
						}
					}
					ids := map[string]bool{}
					for _, command := range billingRepo.commands {
						require.True(t, strings.HasPrefix(command.RequestID, "fusion:"))
						require.False(t, ids[command.RequestID])
						ids[command.RequestID] = true
						require.EqualValues(t, 100, command.InputTokens)
						require.EqualValues(t, 40, command.CacheReadTokens)
						require.EqualValues(t, 10, command.CacheCreationTokens)
					}
					require.NotEqual(t, billingRepo.commands[0].BalanceCost, billingRepo.commands[1].BalanceCost, "Lead and Sidekick use their own physical prices")
					require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: last}))
					require.Len(t, billingRepo.commands, 4, "the aggregate cannot bill again")
					// A durable replay has no new inference or charge, including when the
					// balance has since reached zero.
					cache.mu.Lock()
					cache.balance = 0
					cache.mu.Unlock()
					_, _, err = request(body)
					require.NoError(t, err)
					require.Len(t, wires, 4)
					require.Len(t, billingRepo.commands, 4, "identical result retry replays the saved response")
				})
			}
		}
	}
}

func TestFusionPinnedAccountAndProfileValidation(t *testing.T) {
	p := fusionTestProfile()
	a := &Account{ID: 7, Platform: PlatformDevin, Extra: map[string]any{fusion.ExtraProfiles: []fusion.Profile{p}}}
	require.Error(t, ValidateDevinFusionExtra(PlatformDevin, a.Extra))
	require.Error(t, ValidateDevinFusionExtra(PlatformOpenAI, a.Extra))
	ctx := context.WithValue(context.Background(), devinFusionBindingKey{}, &devinFusionBinding{AccountID: 7})
	require.Equal(t, "fusion_account_bound", openAICompatibleAccountEligibilityFailureReasonBeforeProfit(ctx, &Account{ID: 8}, PlatformDevin, "fusion-team", false, OpenAIEndpointCapabilityResponses))
	scheduler := &defaultOpenAIAccountScheduler{}
	ok, reason := scheduler.isAccountRequestCompatibleReason(ctx, &Account{ID: 8}, OpenAIAccountScheduleRequest{})
	require.False(t, ok)
	require.Equal(t, "fusion_account_bound", reason)
	service := &OpenAIGatewayService{}
	err := service.ExplainDevinRouteDenial(ctx, nil, "fusion-team")
	var typed *fusion.Error
	require.ErrorAs(t, err, &typed)
	require.Equal(t, "devin_fusion_account_bound", typed.Code)
	duplicate := p
	duplicate.PublicModel = strings.ToUpper(p.PublicModel)
	_, err = devinFusionProfiles(&Account{Platform: PlatformDevin, Extra: map[string]any{fusion.ExtraProfiles: []fusion.Profile{p, duplicate}}})
	require.Error(t, err, "profile names are resolved without case sensitivity")
}

func TestFusionNativeSessionHeadersDoNotAdoptOrdinaryToolTurns(t *testing.T) {
	p := fusionTestProfile()
	models := []*pb.ClientModelConfig{{ModelUid: p.LeadModel}, {ModelUid: p.SidekickModel}, {ModelUid: "fusion-" + p.LeadModel + "-sidekick-" + p.SidekickModel}}
	a := &Account{ID: 7, Platform: PlatformDevin, Extra: devinCatalogExtra(models, "fixture")}
	s := &OpenAIGatewayService{devin: &DevinService{}, accountRepo: &fusionAccountRepo{account: a}}
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		for _, header := range []string{"X-Session-Id", "X-OpenCode-Session", "X-Claude-Code-Session-Id"} {
			t.Run(protocol+"/"+header, func(t *testing.T) {
				body := []byte(strings.ReplaceAll(string(fusionProtocolBody(protocol, false, "call_ordinary_tool")), p.PublicModel, p.LeadModel))
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+protocol, nil)
				c.Request.Header.Set(header, "native-session")
				bound, err := s.BindDevinFusionRequest(c, &APIKey{ID: 2, UserID: 1}, p.LeadModel, body, protocol, nil)
				require.NoError(t, err)
				require.False(t, bound)
				require.False(t, DevinFusionRequest(c.Request.Context()))
			})
		}
	}
}

func TestFusionResponsesCustomAndNamespaceTools(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("custom=%t/stream=%t", custom, stream), func(t *testing.T) {
				declaration := `{"type":"namespace","name":"files","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`
				if custom {
					declaration = `{"type":"custom","name":"apply_patch","format":{"type":"text"}}`
				}
				body := []byte(`{"model":"fusion-team","input":"task","tools":[` + declaration + `]}`)
				req, mapping, err := parseDevinFusionRequest(body, "responses")
				require.NoError(t, err)
				require.Len(t, req.Tools, 1)
				require.Equal(t, "function", req.Tools[0].Type)
				args := `{"path":"main.go"}`
				if custom {
					args = `{"input":"*** Begin Patch\n*** End Patch"}`
				}
				response := &apicompat.ResponsesResponse{ID: "resp_fusion_fixture", Object: "response", Model: "fusion-team", Status: "completed", Output: []apicompat.ResponsesOutput{{Type: "function_call", ID: "fc_fixture", CallID: "call_fusion_fixture", Name: req.Tools[0].Name, Arguments: args, Status: "completed"}}}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				require.NoError(t, writeDevinFusionResponse(c, response, "responses", stream, mapping))
				require.Contains(t, w.Body.String(), "call_fusion_fixture")
				if custom {
					require.Contains(t, w.Body.String(), "custom_tool_call")
					require.Contains(t, w.Body.String(), "*** Begin Patch")
					if stream {
						require.Contains(t, w.Body.String(), "response.custom_tool_call_input.delta")
					}
				} else {
					require.Contains(t, w.Body.String(), `"namespace":"files"`)
					require.Contains(t, w.Body.String(), `"name":"read_file"`)
				}
			})
		}
	}
}

type fusionBrokenReserveCache struct{ *fusionTestBillingCache }

func (c *fusionBrokenReserveCache) ReserveInflightBalance(context.Context, int64, string, float64, float64, time.Duration) (bool, float64, error) {
	return false, 0, fmt.Errorf("redis unavailable")
}
func TestFusionReservationsFailClosed(t *testing.T) {
	cfg := &config.Config{}
	cfg.Billing.InflightReservation.Enabled = true
	cache := &fusionBrokenReserveCache{&fusionTestBillingCache{newMemInflightCache(100)}}
	billing := &BillingCacheService{cfg: cfg, cache: cache}
	x := &devinFusionExecutor{service: &OpenAIGatewayService{billingCacheService: billing}, profile: fusionTestProfile(), key: &APIKey{ID: 2, UserID: 1, User: &User{ID: 1}}}
	reservation, err := x.reserve(context.Background(), 0.1)
	require.Error(t, err)
	require.Nil(t, reservation)
}

func TestFusionErrorBeforePublicStreamUsesJSON(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/"+protocol, nil)
			c.Header("Content-Type", "text/event-stream")
			require.True(t, WriteDevinFusionError(c, protocol, fusion.E("tool_pending", "return the pending tool result", 409)))
			require.Equal(t, 409, w.Code)
			require.Contains(t, w.Header().Get("Content-Type"), "application/json")
			require.True(t, json.Valid(w.Body.Bytes()))
			require.NotContains(t, w.Body.String(), "event:")
		})
	}
}

func TestFusionAutomaticPairDiscoveryAndOptOut(t *testing.T) {
	p := fusionTestProfile()
	public := "fusion-" + p.LeadModel + "-sidekick-" + p.SidekickModel
	models := []*pb.ClientModelConfig{{ModelUid: p.LeadModel}, {ModelUid: p.SidekickModel}, {ModelUid: public, ModelInfo: &pb.ModelInfo{IsModelRouter: true, HarnessUids: []string{"public-only-harness"}}}}
	a := &Account{Platform: PlatformDevin, Extra: devinCatalogExtra(models, "fixture")}
	inferred := devinFusionProfile(a, public)
	require.NotNil(t, inferred)
	require.True(t, inferred.Enabled)
	require.Equal(t, p.LeadModel, inferred.LeadModel)
	require.True(t, a.supportsDevinModel(public))
	require.Contains(t, devinEffectiveModelIDs(models, a), public)
	gateway := &OpenAIGatewayService{accountRepo: &fusionAccountRepo{account: a}}
	require.NoError(t, gateway.ExplainDevinRouteDenial(context.Background(), nil, public), "a valid pair must preserve ordinary scheduler capacity errors")
	view := DevinExtraForView(a)
	snapshot := view[devinCatalogExtraKey].(*devin.CatalogSnapshot)
	require.Equal(t, fusion.RouteMode, snapshot.Routes[2].Mode)
	require.Equal(t, "model_id", snapshot.Routes[2].PairSource)
	require.Equal(t, p.LeadModel, snapshot.Routes[2].LeadModelUID)
	body, err := DevinModelsBody(models, a)
	require.NoError(t, err)
	require.Contains(t, string(body), public)
	// Explicit per-name overrides win, including disabling an inferred pair.
	disabled := *inferred
	disabled.Enabled = false
	a.Extra[fusion.ExtraProfiles] = []fusion.Profile{disabled}
	require.Nil(t, devinFusionProfile(a, public))
	require.False(t, a.supportsDevinModel(public))
	require.NotContains(t, devinEffectiveModelIDs(models, a), public)
	require.Error(t, gateway.ExplainDevinRouteDenial(context.Background(), nil, public))
	custom := *inferred
	custom.LeadModel = p.SidekickModel
	a.Extra[fusion.ExtraProfiles] = []fusion.Profile{custom}
	require.Equal(t, p.SidekickModel, devinFusionProfile(a, public).LeadModel)
	a.Extra[fusion.ExtraEnabled] = false
	require.Nil(t, devinFusionProfile(a, public))
	require.False(t, a.supportsDevinModel(public))
	a.Extra[fusion.ExtraEnabled] = "true"
	require.Error(t, ValidateDevinFusionExtra(PlatformDevin, a.Extra))
}

type fusionAuxCapacityCache struct {
	ConcurrencyCache
	accountLimit, userLimit       int
	accountReleases, userReleases int
	waiting                       int
}

func (c *fusionAuxCapacityCache) GetAccountWaitingCount(context.Context, int64) (int, error) {
	return c.waiting, nil
}
func (c *fusionAuxCapacityCache) AcquireAccountSlot(_ context.Context, _ int64, limit int, _ string) (bool, error) {
	c.accountLimit = limit
	return true, nil
}
func (c *fusionAuxCapacityCache) AcquireUserSlot(_ context.Context, _ int64, limit int, _ string) (bool, error) {
	c.userLimit = limit
	return true, nil
}
func (c *fusionAuxCapacityCache) ReleaseAccountSlot(context.Context, int64, string) error {
	c.accountReleases++
	return nil
}
func (c *fusionAuxCapacityCache) ReleaseUserSlot(context.Context, int64, string) error {
	c.userReleases++
	return nil
}

func TestFusionBackgroundSummaryLeavesForegroundCapacityAndOwnsMetadata(t *testing.T) {
	cache := &fusionAuxCapacityCache{}
	svc := &OpenAIGatewayService{concurrencyService: NewConcurrencyService(cache)}
	x := &devinFusionExecutor{service: svc, c: &gin.Context{}, account: &Account{ID: 7, Concurrency: 4}, key: &APIKey{UserID: 1, User: &User{ID: 1, Concurrency: 3}}, stream: true, inboundEndpoint: "/v1/responses", userAgent: "client", reservations: map[string]*InflightReservation{"foreground": nil}}
	child, release, ok := x.Auxiliary(context.Background())
	require.True(t, ok)
	background, validChild := child.(*devinFusionExecutor)
	require.True(t, validChild)
	require.Nil(t, background.c)
	require.False(t, background.stream)
	require.Equal(t, "/v1/responses", background.inboundEndpoint)
	require.Empty(t, background.reservations)
	require.Equal(t, 3, cache.accountLimit)
	require.Equal(t, 2, cache.userLimit)
	release()
	require.Equal(t, 1, cache.accountReleases)
	require.Equal(t, 1, cache.userReleases)
	require.Contains(t, x.reservations, "foreground")
	cache.waiting = 1
	_, _, ok = x.Auxiliary(context.Background())
	require.False(t, ok)
	cache.waiting = 0
	x.account.Concurrency = 1
	_, _, ok = x.Auxiliary(context.Background())
	require.False(t, ok)
}

func TestFusionSummaryUsageSettlesWithoutHTTPAndHasSeparatePurpose(t *testing.T) {
	logs := &openAIRecordUsageLogRepoStub{inserted: true}
	bills := &fusionBillingRepo{}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(logs, bills, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	x := &devinFusionExecutor{service: svc, account: &Account{ID: 7, Platform: PlatformDevin}, key: &APIKey{ID: 2, UserID: 1, User: &User{ID: 1}}, profile: fusionTestProfile(), sessionID: "session", publicModel: "fusion-team", inboundEndpoint: "/v1/responses", userAgent: "test-client", ipAddress: "127.0.0.1", reservations: map[string]*InflightReservation{}}
	a := fusion.Attempt{ID: "summary-independent-attempt", Purpose: fusion.Compaction, Role: fusion.Sidekick, Model: x.profile.SidekickModel, BillingModel: x.profile.SidekickModel, StartedAt: time.Now(), Response: &apicompat.ResponsesResponse{Model: x.profile.SidekickModel, Usage: &apicompat.ResponsesUsage{InputTokens: 100, OutputTokens: 20}}}
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "same-outer-id")
	first, err := x.Settle(ctx, a)
	require.NoError(t, err)
	require.Greater(t, first, 0.0)
	_, err = x.Settle(ctx, a)
	require.NoError(t, err)
	require.Len(t, bills.commands, 1)
	require.Equal(t, "fusion:"+a.ID, bills.commands[0].RequestID)
	require.NotNil(t, logs.lastLog.ModelMappingChain)
	require.Contains(t, *logs.lastLog.ModelMappingChain, "compaction/sidekick")
}

func TestFusionContextMeasurementIncludesWireToolsAndOutputReserve(t *testing.T) {
	p := fusionTestProfile()
	p.MaxContextBytes = 20000
	x := &devinFusionExecutor{profile: p, models: []*pb.ClientModelConfig{{ModelUid: p.LeadModel, MaxTokens: 8192}}}
	out := 1024
	req := &apicompat.ResponsesRequest{Model: p.LeadModel, Input: json.RawMessage(`"task"`), MaxOutputTokens: &out}
	small, safe, err := x.MeasureContext(req)
	require.NoError(t, err)
	require.Positive(t, small)
	require.Less(t, safe, 8192-out)
	req.Tools = []apicompat.ResponsesTool{{Type: "function", Name: "read_file", Description: strings.Repeat("schema context ", 100), Parameters: json.RawMessage(`{"type":"object"}`)}}
	large, _, err := x.MeasureContext(req)
	require.NoError(t, err)
	require.Greater(t, large, small+1000)
}

func TestFusionPendingSummaryReservationSurvivesHTTPAndReleasesOnRecovery(t *testing.T) {
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	cfg.Billing.InflightReservation.Enabled = true
	cache := &fusionTestBillingCache{newMemInflightCache(100)}
	logs := &openAIRecordUsageLogRepoStub{inserted: true}
	bills := &fusionBillingRepo{}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(logs, bills, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	svc.cfg = cfg
	svc.billingCacheService = NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
	defer svc.billingCacheService.Stop()
	x := &devinFusionExecutor{service: svc, account: &Account{ID: 7, Platform: PlatformDevin}, profile: fusionTestProfile(), key: &APIKey{ID: 2, UserID: 1, User: &User{ID: 1, Balance: 100}}, publicModel: "fusion-team", reservations: map[string]*InflightReservation{}, holdUnsettled: map[string]bool{"summary-held": true}}
	r, err := x.reserve(context.Background(), 1, "summary-held")
	require.NoError(t, err)
	x.reservations["summary-held"] = r
	x.Close()
	require.Equal(t, 1, cache.count())
	recovery := *x
	recovery.reservations = map[string]*InflightReservation{}
	a := fusion.Attempt{ID: "summary-held", Purpose: fusion.Compaction, Role: fusion.Sidekick, Model: x.profile.SidekickModel, BillingModel: x.profile.SidekickModel, EstimateUSD: 1, StartedAt: time.Now(), Response: &apicompat.ResponsesResponse{Model: x.profile.SidekickModel, Usage: &apicompat.ResponsesUsage{InputTokens: 100, OutputTokens: 10}}}
	_, err = recovery.Settle(context.Background(), a)
	require.NoError(t, err)
	require.Zero(t, cache.count())
	require.Len(t, bills.commands, 1)
}
