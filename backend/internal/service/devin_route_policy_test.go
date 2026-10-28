//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/proto"
)

// Any accidental penalty call on this embedded nil repository panics the test.
type devinNoPenaltyRepo struct{ AccountRepository }

func devinRoutingModels() []*pb.ClientModelConfig {
	return []*pb.ClientModelConfig{
		{ModelUid: "swe-2-max", ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true, SupportsImages: true}}},
		{ModelUid: "router", ModelInfo: &pb.ModelInfo{IsModelRouter: true, MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true, SupportsImages: true}}},
		{ModelUid: "display-router", ModelInfo: &pb.ModelInfo{DisplayOption: pb.DisplayOption_DISPLAY_OPTION_MODEL_ROUTER}},
		{ModelUid: "opaque-harness", ModelInfo: &pb.ModelInfo{IsModelRouter: true, HarnessUids: []string{"fusion"}}},
		{ModelUid: "fusion-lead-fast-sidekick-worker"},
		{ModelUid: "limited-target", ModelInfo: &pb.ModelInfo{MaxOutputTokens: 16, ModelFeatures: &pb.ModelFeatures{}}},
	}
}

func devinRouteBody(protocol, model string, stream bool) []byte {
	if protocol == "responses" {
		return fmt.Appendf(nil, `{"model":%q,"input":"private-request-content","max_output_tokens":32,"stream":%t}`, model, stream)
	}
	if protocol == "anthropic" {
		return fmt.Appendf(nil, `{"model":%q,"messages":[{"role":"user","content":"private-request-content"}],"max_tokens":32,"stream":%t}`, model, stream)
	}
	return fmt.Appendf(nil, `{"model":%q,"messages":[{"role":"user","content":"private-request-content"}],"max_completion_tokens":32,"stream":%t}`, model, stream)
}

func TestDevinRoutePolicyAcrossProtocols(t *testing.T) {
	for _, tc := range []struct {
		name, model, mapped, inbound, code string
		assignment                         *pb.ModelAssignment
		mapping                            map[string]any
		wantStatus                         int
		wantChat, wantAssign               int
	}{
		{name: "ordinary", model: "swe-2-max", wantStatus: 200, wantChat: 1},
		{name: "ordinary alias", model: "friendly", mapping: map[string]any{"friendly": "swe-2-max"}, wantStatus: 200, wantChat: 1},
		{name: "unused dispatch hint", model: "friendly", mapped: "fusion", mapping: map[string]any{"friendly": "swe-2-max"}, wantStatus: 200, wantChat: 1},
		{name: "router", model: "router", assignment: &pb.ModelAssignment{ModelUid: "swe-2-max", AssignmentJwt: "private-assignment"}, wantStatus: 200, wantChat: 1, wantAssign: 1},
		{name: "display router dynamic target", model: "display-router", assignment: &pb.ModelAssignment{ModelUid: "dynamic-ordinary", AssignmentJwt: "private-assignment"}, wantStatus: 200, wantChat: 1, wantAssign: 1},
		{name: "pair", model: "fusion-lead-fast-sidekick-worker", code: devin.CodeFusionUnsupported, wantStatus: 400},
		{name: "bare fusion", model: "fusion", code: devin.CodeRouteUnverified, wantStatus: 400},
		{name: "sidekick", model: "fusion-sidekick-worker", code: devin.CodeRouteUnverified, wantStatus: 400},
		{name: "harness preflight", model: "opaque-harness", code: devin.CodeRouteUnverified, wantStatus: 400},
		{name: "account mapping cannot hide pair", model: "fusion-lead-fast-sidekick-worker", mapping: map[string]any{"fusion-lead-fast-sidekick-worker": "swe-2-max"}, code: devin.CodeFusionUnsupported, wantStatus: 400},
		{name: "channel mapping cannot hide pair", model: "swe-2-max", inbound: "fusion-lead-fast-sidekick-worker", code: devin.CodeFusionUnsupported, wantStatus: 400},
		{name: "alias cannot hide harness", model: "friendly", mapping: map[string]any{"friendly": "opaque-harness"}, code: devin.CodeRouteUnverified, wantStatus: 400},
		{name: "mapped target pair", model: "friendly", mapped: "fusion-lead-fast-sidekick-worker", code: devin.CodeFusionUnsupported, wantStatus: 400},
		{name: "assignment harness", model: "router", assignment: &pb.ModelAssignment{ModelUid: "swe-2-max", AssignmentJwt: "private-assignment", HarnessUids: []string{"fusion"}}, code: devin.CodeAssignmentHarness, wantStatus: 400, wantAssign: 1},
		{name: "assignment pair", model: "router", assignment: &pb.ModelAssignment{ModelUid: "fusion-lead-sidekick-worker", AssignmentJwt: "private-assignment"}, code: devin.CodeRouteMismatch, wantStatus: 502, wantAssign: 1},
		{name: "assignment catalog harness", model: "router", assignment: &pb.ModelAssignment{ModelUid: "opaque-harness", AssignmentJwt: "private-assignment"}, code: devin.CodeRouteMismatch, wantStatus: 502, wantAssign: 1},
		{name: "assignment recursive router", model: "router", assignment: &pb.ModelAssignment{ModelUid: "display-router", AssignmentJwt: "private-assignment"}, code: devin.CodeRouteMismatch, wantStatus: 502, wantAssign: 1},
		{name: "assignment no jwt", model: "router", assignment: &pb.ModelAssignment{ModelUid: "swe-2-max"}, code: devin.CodeRouteMismatch, wantStatus: 502, wantAssign: 1},
		{name: "final output limit", model: "router", assignment: &pb.ModelAssignment{ModelUid: "limited-target", AssignmentJwt: "private-assignment"}, code: devin.CodeModelCapability, wantStatus: 400, wantAssign: 1},
	} {
		for _, protocol := range []string{"responses", "chat", "anthropic"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.name, protocol, stream), func(t *testing.T) {
					var mints, assignments, chats int
					base := devinFakeUpstream(t, &atomic.Int32{}, &atomic.Int32{})
					upstream := &devinHTTP{handle: func(r *http.Request) (*http.Response, error) {
						switch r.URL.Path {
						case devin.MintPath:
							mints++
						case devin.ModelsPath:
							b, err := proto.Marshal(&pb.GetCliModelConfigsResponse{ClientModelConfigs: devinRoutingModels()})
							require.NoError(t, err)
							return devinReply(200, b), nil
						case devin.AssignPath:
							assignments++
							b, err := proto.Marshal(&pb.AssignModelResponse{Assignment: tc.assignment})
							require.NoError(t, err)
							return devinReply(200, b), nil
						case devin.ChatPath:
							chats++
						}
						return base.handle(r)
					}}
					creds := map[string]any{"api_key": "cog_private-synthetic"}
					if tc.mapping != nil {
						creds["model_mapping"] = tc.mapping
					}
					a := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeAPIKey, Credentials: creds}
					d := NewDevinService(nil, nil, upstream, &config.Config{}, nil)
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					ctx := context.Background()
					if tc.inbound != "" {
						ctx = context.WithValue(ctx, ctxkey.InboundModel, tc.inbound)
					}
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+protocol, nil).WithContext(ctx)
					result, err := (&OpenAIGatewayService{devin: d, accountRepo: &devinNoPenaltyRepo{}}).forwardDevin(ctx, c, a, devinRouteBody(protocol, tc.model, stream), protocol, "private-session", tc.mapped)
					require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
					require.Equal(t, tc.wantChat, chats)
					require.Equal(t, tc.wantAssign, assignments)
					require.LessOrEqual(t, mints, 1, "route rejection must not recover auth")
					require.NotEmpty(t, w.Header().Get("X-Sub2API-Request-Id"))
					if tc.code == "" {
						require.NoError(t, err)
						require.NotNil(t, result)
						require.Equal(t, tc.model, result.Model)
						require.Equal(t, "completed", result.DevinRoute.Outcome)
						require.Equal(t, 18, result.Usage.InputTokens)
						if tc.name == "unused dispatch hint" {
							require.Equal(t, []devin.RouteMapping{{Stage: "account_alias", From: "friendly", To: "swe-2-max"}}, result.DevinRoute.MappingSteps)
						}
						if tc.assignment != nil {
							require.Equal(t, tc.assignment.ModelUid, result.UpstreamModel)
							require.True(t, result.DevinRoute.Attempts[0].AssignmentUsed)
						}
					} else {
						var routeErr *devin.RouteError
						require.ErrorAs(t, err, &routeErr)
						require.Equal(t, tc.code, routeErr.Code)
						var failover *UpstreamFailoverError
						require.False(t, errors.As(err, &failover))
						require.Nil(t, result)
						require.Contains(t, w.Body.String(), tc.code)
						require.Contains(t, w.Header().Get("Content-Type"), "application/json")
						require.NotContains(t, w.Body.String(), "[DONE]")
					}
					trace := c.MustGet("devin_route_trace").(*devin.ModelRouteTrace)
					require.NotContains(t, trace.MechanismStatus, "fusion_verified")
					b, err := json.Marshal(trace)
					require.NoError(t, err)
					for _, secret := range []string{"private-assignment", "user-jwt", "cog_private-synthetic", "private-request-content", "private-session"} {
						require.NotContains(t, string(b), secret)
					}
				})
			}
		}
	}
}

func TestDevinCatalogRoutingUsedByListingAndSchedulers(t *testing.T) {
	models := devinRoutingModels()
	extra := devinCatalogExtra(models, "synthetic-identity")
	// Exercise the database JSON shape, not just an in-memory typed pointer.
	b, err := json.Marshal(extra)
	require.NoError(t, err)
	var stored map[string]any
	require.NoError(t, json.Unmarshal(b, &stored))
	a := Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Extra: stored, Credentials: map[string]any{}}
	require.Contains(t, a.devinModelIDs(), "opaque-harness", "diagnostic catalog must retain blocked entries")
	listing := &GatewayService{accountRepo: &devinListingRepo{accounts: []Account{a}}}
	available := listing.GetAvailableModels(context.Background(), nil, PlatformDevin)
	require.ElementsMatch(t, []string{"swe-2-max", "router", "display-router", "limited-target"}, available)
	for _, m := range models {
		uid := devin.ModelID(m)
		want := devin.PlanModel(m).Validate() == nil
		require.Equal(t, want, a.IsModelSupported(uid))
		reason := openAICompatibleAccountEligibilityFailureReasonBeforeProfit(context.Background(), &a, PlatformDevin, uid, false, "")
		require.Equal(t, want, reason == "", "legacy scheduler: %s", reason)
		ok, reason := (&defaultOpenAIAccountScheduler{}).isAccountRequestCompatibleReason(context.Background(), &a, OpenAIAccountScheduleRequest{RequestedModel: uid})
		require.Equal(t, want, ok, "advanced scheduler: %s", reason)
	}
	a.Credentials["model_mapping"] = map[string]any{"friendly": "opaque-harness", "fusion-lead-sidekick-worker": "swe-2-max", "plain-alias": "swe-2-max"}
	require.False(t, a.IsModelSupported("friendly"))
	require.False(t, a.IsModelSupported("fusion-lead-sidekick-worker"))
	require.True(t, a.IsModelSupported("plain-alias"))
	listing.accountRepo = &devinListingRepo{accounts: []Account{a}}
	require.Equal(t, []string{"plain-alias"}, listing.GetAvailableModels(context.Background(), nil, PlatformDevin))
}

func TestDevinNoAccountExplanationDoesNotMaskSupportedRoutes(t *testing.T) {
	a := Account{ID: 1, Platform: PlatformDevin, Extra: devinCatalogExtra(devinRoutingModels(), "identity"), Credentials: map[string]any{"model_mapping": map[string]any{"friendly": "opaque-harness"}}}
	repo := &devinListingRepo{accounts: []Account{a}}
	s := &OpenAIGatewayService{accountRepo: repo}
	var denial *devin.RouteError
	require.ErrorAs(t, s.ExplainDevinRouteDenial(context.Background(), nil, "friendly"), &denial)
	require.Equal(t, devin.CodeRouteUnverified, denial.Code)
	other := Account{ID: 2, Platform: PlatformDevin, Extra: devinCatalogExtra(devinRoutingModels(), "identity"), Credentials: map[string]any{"model_mapping": map[string]any{"friendly": "swe-2-max"}}}
	repo.accounts = append(repo.accounts, other)
	require.NoError(t, s.ExplainDevinRouteDenial(context.Background(), nil, "friendly"), "a supported account may be busy; that is not a semantic denial")
}

func TestDevinRouteRejectionsDoNotAffectSchedulerHealth(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	openAIAdvancedSchedulerSettingCache.Store(&cachedOpenAIAdvancedSchedulerSetting{enabled: true, expiresAt: time.Now().Add(time.Minute).UnixNano()})
	stats := newOpenAIAccountRuntimeStats()
	scheduler := &defaultOpenAIAccountScheduler{stats: stats}
	s := &OpenAIGatewayService{openaiScheduler: scheduler}
	s.openaiSchedulerOnce.Do(func() {})
	a := &Account{ID: 5, Platform: PlatformDevin}
	for _, code := range []string{devin.CodeFusionUnsupported, devin.CodeRouteUnverified, devin.CodeAssignmentHarness, devin.CodeRouteMismatch, devin.CodeModelCapability} {
		require.False(t, s.ReportOpenAIAccountScheduleResult(a, "router", false, nil, fmt.Errorf("wrapped: %w", &devin.RouteError{Code: code})))
	}
	require.Zero(t, stats.size(), "local routing decisions must not become account failures")
	s.ReportOpenAIAccountScheduleResult(a, "router", false, nil, &devin.Error{Code: "unavailable", Status: 503})
	require.Equal(t, 1, stats.size(), "genuine upstream failures must still be reported")
}

func TestDevinRouteErrorsAfterConcurrencyKeepalive(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		c.Header("Content-Type", "text/event-stream")
		_, err := c.Writer.WriteString(": keepalive\n\n")
		require.NoError(t, err)
		require.True(t, WriteDevinRouteError(c, protocol, &devin.RouteError{Code: devin.CodeRouteUnverified}))
		require.Equal(t, 200, w.Code)
		require.Contains(t, w.Body.String(), "data:")
		require.Contains(t, w.Body.String(), devin.CodeRouteUnverified)
		require.NotContains(t, w.Body.String(), "[DONE]")
		failure, ok := GetOpsStreamError(c)
		require.True(t, ok)
		require.Equal(t, 400, failure.IntendedStatus)
		require.False(t, failure.CountTowardsSLA)
		require.True(t, failure.RequestScoped)
	}
}

func TestDevinManualSyncKeepsRawMetadataAndReturnsEffectiveModels(t *testing.T) {
	upstream := &devinHTTP{handle: func(r *http.Request) (*http.Response, error) {
		require.Equal(t, devin.ModelsPath, r.URL.Path)
		b, err := proto.Marshal(&pb.GetCliModelConfigsResponse{ClientModelConfigs: devinRoutingModels()})
		require.NoError(t, err)
		return devinReply(200, b), nil
	}}
	repo := &devinFeatureRepo{}
	d := NewDevinService(repo, nil, upstream, &config.Config{}, nil)
	a := &Account{ID: 1, Platform: PlatformDevin, Credentials: map[string]any{"access_token": "synthetic"}}
	result, err := (&AccountTestService{devin: d, accountRepo: repo}).SyncUpstreamModelCatalog(context.Background(), a)
	require.NoError(t, err)
	require.NotContains(t, result.Models, "opaque-harness")
	require.NotContains(t, result.Models, "fusion-lead-fast-sidekick-worker")
	require.Contains(t, repo.updates["devin_models"], "opaque-harness")
	require.Len(t, result.DevinRoutes, len(devinRoutingModels()))
	require.Equal(t, "devin_model_routes_blocked", result.Warnings[0].Code)
	snapshot := repo.updates[devinCatalogExtraKey].(*devin.CatalogSnapshot)
	require.Equal(t, []string{"fusion"}, devin.FindModel(snapshot.Models, "opaque-harness").ModelInfo.HarnessUids)
}

func TestDevinModelCacheReturnsIndependentRequestSnapshots(t *testing.T) {
	var calls atomic.Int32
	upstream := &devinHTTP{handle: func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		b, err := proto.Marshal(&pb.GetCliModelConfigsResponse{ClientModelConfigs: devinRoutingModels()})
		require.NoError(t, err)
		return devinReply(200, b), nil
	}}
	d := NewDevinService(nil, nil, upstream, &config.Config{}, nil)
	a := &Account{ID: 1, Platform: PlatformDevin, Credentials: map[string]any{"access_token": "synthetic"}}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			models, err := d.Models(context.Background(), a)
			require.NoError(t, err)
			models[0].ModelInfo.HarnessUids = []string{"request-local-only"}
			models[0].ModelUid = "local-mutation"
		})
	}
	wg.Wait()
	models, err := d.Models(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, "swe-2-max", models[0].ModelUid)
	require.Empty(t, models[0].ModelInfo.HarnessUids)
	require.Equal(t, int32(1), calls.Load())
}

func TestDevinRouteEvidenceReachesTheProtectedAdminLogIndex(t *testing.T) {
	d, _, _ := devinTestService(t)
	core, observed := observer.New(zap.DebugLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	a := &Account{ID: 1, Platform: PlatformDevin, Credentials: map[string]any{"access_token": "private-synthetic"}}
	result, err := (&OpenAIGatewayService{devin: d}).forwardDevin(ctx, c, a, devinRouteBody("responses", "swe-2-max", false), "responses", "private-session", "")
	require.NoError(t, err)
	entries := observed.FilterMessage("devin.route_trace").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, result.RequestID, fields["request_id"])
	require.Equal(t, "audit.devin.routing", fields["component"])
	sink := NewOpsSystemLogSink(nil)
	t.Cleanup(sink.Stop)
	require.True(t, sink.shouldIndex(&logger.LogEvent{Level: "debug", Fields: fields}), "debug events must not disappear at the admin sink's component filter")
	b, err := json.Marshal(fields)
	require.NoError(t, err)
	for _, secret := range []string{"private-synthetic", "private-request-content", "user-jwt", "private-session"} {
		require.NotContains(t, string(b), secret)
	}
	require.Contains(t, string(b), "usage_snapshots")
}

func TestDevinStreamRouteMismatchStopsWithoutReplayAndRetainsUsage(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			d, _, _ := devinTestService(t)
			upstream := d.upstream.(*devinHTTP)
			original := upstream.handle
			chats := 0
			upstream.handle = func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != devin.ChatPath {
					return original(r)
				}
				chats++
				var stream []byte
				for _, m := range []*pb.GetChatMessageResponse{
					{DeltaText: "partial", ActualModelUid: proto.String("swe-2-max"), Usage: &pb.ModelUsageStats{InputTokens: 3, OutputTokens: 1}},
					{ActualModelUid: proto.String("fusion"), Usage: &pb.ModelUsageStats{InputTokens: 3, OutputTokens: 2}},
				} {
					b, err := proto.Marshal(m)
					require.NoError(t, err)
					stream = append(stream, devinTestFrame(0, b)...)
				}
				return devinReply(200, append(stream, devinTestFrame(2, []byte(`{}`))...)), nil
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			a := &Account{ID: 1, Platform: PlatformDevin, Credentials: map[string]any{"access_token": "synthetic"}}
			result, err := (&OpenAIGatewayService{devin: d, accountRepo: &devinNoPenaltyRepo{}}).forwardDevin(context.Background(), c, a, devinRouteBody(protocol, "swe-2-max", true), protocol, "session", "")
			require.Error(t, err)
			require.NotNil(t, result)
			require.Equal(t, 1, chats)
			require.Equal(t, 3, result.Usage.InputTokens)
			require.Equal(t, 2, result.Usage.OutputTokens)
			if protocol == "responses" {
				require.Equal(t, 1, strings.Count(w.Body.String(), `"type":"response.output_text.delta"`), "must emit each content delta once")
			} else {
				require.Equal(t, 1, strings.Count(w.Body.String(), "partial"), "must not replay generation")
			}
			require.Contains(t, w.Body.String(), devin.CodeRouteMismatch)
			for _, success := range []string{"[DONE]", "response.completed", "message_stop"} {
				require.NotContains(t, w.Body.String(), success)
			}
		})
	}
}

func TestDevinRouterToolRoundTripsAcrossProtocols(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, streaming), func(t *testing.T) {
				var chats, assignments int
				var cascade string
				base := devinFakeUpstream(t, &atomic.Int32{}, &atomic.Int32{})
				upstream := &devinHTTP{handle: func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case devin.ModelsPath:
						b, err := proto.Marshal(&pb.GetCliModelConfigsResponse{ClientModelConfigs: devinRoutingModels()})
						require.NoError(t, err)
						return devinReply(200, b), nil
					case devin.AssignPath:
						assignments++
						b, err := proto.Marshal(&pb.AssignModelResponse{Assignment: &pb.ModelAssignment{ModelUid: "swe-2-max", AssignmentJwt: "synthetic-assignment"}})
						require.NoError(t, err)
						return devinReply(200, b), nil
					case devin.ChatPath:
						chats++
						_, b, err := devin.ReadFrame(r.Body)
						require.NoError(t, err)
						var q pb.GetChatMessageRequest
						require.NoError(t, proto.Unmarshal(b, &q))
						require.Equal(t, "swe-2-max", q.ChatModelUid)
						require.Equal(t, "synthetic-assignment", q.GetModelAssignmentJwt())
						require.Len(t, q.Tools, 1)
						m := &pb.GetChatMessageResponse{Usage: &pb.ModelUsageStats{InputTokens: 4, OutputTokens: 2}, ActualModelUid: proto.String("swe-2-max")}
						if chats == 1 {
							cascade = q.CascadeId
							m.DeltaToolCalls = []*pb.ChatToolCall{{Id: "call", Name: "lookup", ArgumentsJson: "{}"}}
							m.StopReason = pb.StopReason_STOP_REASON_FUNCTION_CALL
						} else {
							require.Equal(t, cascade, q.CascadeId)
							require.Len(t, q.ChatMessagePrompts, 3)
							require.Equal(t, "call", q.ChatMessagePrompts[1].ToolCalls[0].Id)
							require.Equal(t, "call", q.ChatMessagePrompts[2].ToolCallId)
							require.Equal(t, "answer", q.ChatMessagePrompts[2].Prompt)
							m.DeltaText, m.StopReason = "done", pb.StopReason_STOP_REASON_STOP_PATTERN
						}
						b, err = proto.Marshal(m)
						require.NoError(t, err)
						return devinReply(200, append(devinTestFrame(0, b), devinTestFrame(2, []byte(`{}`))...)), nil
					}
					return base.handle(r)
				}}
				d := NewDevinService(nil, nil, upstream, &config.Config{}, nil)
				a := &Account{ID: 1, Platform: PlatformDevin, Credentials: map[string]any{"access_token": "synthetic"}}
				for round := 0; round < 2; round++ {
					var body map[string]any
					switch protocol {
					case "responses":
						body = map[string]any{"input": json.RawMessage(`[{"role":"user","content":"question"}]`), "tools": json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]`)}
						if round > 0 {
							body["input"] = json.RawMessage(`[{"role":"user","content":"question"},{"type":"function_call","call_id":"call","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call","output":"answer"}]`)
						}
					case "chat":
						body = map[string]any{"messages": json.RawMessage(`[{"role":"user","content":"question"}]`), "tools": json.RawMessage(`[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]`)}
						if round > 0 {
							body["messages"] = json.RawMessage(`[{"role":"user","content":"question"},{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call","content":"answer"}]`)
						}
					case "anthropic":
						body = map[string]any{"max_tokens": 32, "messages": json.RawMessage(`[{"role":"user","content":"question"}]`), "tools": json.RawMessage(`[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]`)}
						if round > 0 {
							body["messages"] = json.RawMessage(`[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":"answer"}]}]`)
						}
					}
					body["model"], body["stream"] = "router", streaming
					payload, err := json.Marshal(body)
					require.NoError(t, err)
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
					result, err := (&OpenAIGatewayService{devin: d}).forwardDevin(context.Background(), c, a, payload, protocol, "same-tool-session", "")
					require.NoError(t, err, w.Body.String())
					require.Equal(t, 200, w.Code)
					require.Equal(t, "router", result.Model)
					require.Equal(t, "swe-2-max", result.UpstreamModel)
					if round == 0 {
						require.Contains(t, w.Body.String(), "lookup")
					} else {
						require.Contains(t, w.Body.String(), "done")
					}
				}
				require.Equal(t, 2, chats)
				require.Equal(t, 2, assignments)
			})
		}
	}
}
