//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestContextManualRetrySettlesKnownUsageWithoutInferenceAdmission(t *testing.T) {
	cfg, key := contextEnabledFixture()
	models := []*pb.ClientModelConfig{{ModelUid: "gpt-5.5", MaxTokens: 65536, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}}}
	a := &Account{ID: 7, Platform: PlatformDevin, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{9}, Credentials: map[string]any{"access_token": "synthetic-token"}, Extra: devinCatalogExtra(models, "fixture")}
	a.Extra[DevinContextEnabledExtra], a.Extra[DevinContextAutoExtra] = true, false
	calls := 0
	upstream := &devinHTTP{handle: func(r *http.Request) (*http.Response, error) {
		var reply proto.Message
		switch r.URL.Path {
		case devin.ModelsPath:
			reply = &pb.GetCliModelConfigsResponse{ClientModelConfigs: models}
		case devin.UserJWTPath:
			reply = &pb.GetUserJwtResponse{UserJwt: "synthetic-jwt"}
		case devin.ChatPath:
			calls++
			text := "ready"
			if calls == 1 {
				text = strings.Repeat("old evidence ", 3300)
			} else if calls == 3 {
				text = `{"goal":"Keep the API","current_state":"Completed evidence summarized","decisions":[],"changes":[],"evidence":[],"checks":[],"open_questions":[],"next_steps":[],"files":[]}`
			}
			require.LessOrEqual(t, calls, 3, "recovery must not issue another provider call")
			message := &pb.GetChatMessageResponse{ActualModelUid: proto.String("gpt-5.5"), MessageId: "physical", StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN, DeltaText: text, Usage: &pb.ModelUsageStats{InputTokens: 100, OutputTokens: 20, BillingModelUid: "gpt-5.5"}}
			b, err := proto.Marshal(message)
			require.NoError(t, err)
			return devinReply(200, append(devinTestFrame(0, b), devinTestFrame(2, []byte(`{}`))...)), nil
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
		}
		b, err := proto.Marshal(reply)
		require.NoError(t, err)
		return devinReply(200, b), nil
	}}
	repo := &fusionAccountRepo{account: a}
	billing := &fusionBillingRepo{}
	s := newOpenAIRecordUsageServiceWithBillingRepoForTest(&openAIRecordUsageLogRepoStub{inserted: true}, billing, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	s.cfg, s.accountRepo = cfg, repo
	cfg.Billing.InflightReservation.Enabled = true
	s.billingCacheService = NewBillingCacheService(&fusionTestBillingCache{newMemInflightCache(100)}, nil, nil, nil, nil, nil, cfg, nil)
	defer s.billingCacheService.Stop()
	s.concurrencyService = NewConcurrencyService(&stubConcurrencyCacheForTest{acquireResult: true})
	s.devin = NewDevinService(repo, nil, upstream, cfg, nil)
	s.devin.contextStore = newContextGatewayStore()
	defer func() { require.NoError(t, s.devin.managedEngine().Close(context.Background())) }()
	keys := &APIKeyService{apiKeyRepo: &apiKeyRepoStub{apiKey: key}}
	run := func(id, mode, session string, body []byte, revision int64) (*ContextRunResult, error) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, "shared-client-id"))
		c.Request.Header.Set(mc.HeaderMode, "managed")
		c.Request.Header.Set("Idempotency-Key", id)
		return s.RunContext(c, key, keys, body, mode, session, revision, 0)
	}
	first, err := run("first", "responses", "", []byte(`{"model":"gpt-5.5","input":"Keep the API","max_output_tokens":128}`), 0)
	require.NoError(t, err)
	body, _ := json.Marshal(map[string]any{"model": "gpt-5.5", "input": "Continue", "previous_response_id": first.Response.ID, "max_output_tokens": 128})
	second, err := run("second", "responses", first.SessionID, body, 0)
	require.NoError(t, err)
	body, _ = json.Marshal(map[string]any{"mode": "internal", "session_id": second.SessionID, "lane": "main", "expected_revision": second.Revision})
	billing.fail = true
	_, err = run("compact", "internal", second.SessionID, body, second.Revision)
	require.Error(t, err)
	require.Equal(t, 3, calls)
	require.Len(t, billing.commands, 2)
	billing.fail = false
	a.Schedulable, a.Extra[DevinContextEnabledExtra] = false, false
	s.concurrencyService = nil
	result, err := run("compact", "internal", second.SessionID, body, second.Revision)
	require.NoError(t, err)
	require.True(t, result.Replayed)
	require.True(t, result.Job.Accounted)
	require.Len(t, billing.commands, 3, "the known summary is charged once during the retry")
	repo.lookupErr = errors.New("account no longer available")
	_, err = run("compact", "internal", second.SessionID, body, second.Revision)
	require.NoError(t, err, "fully settled replay does not need account metadata")
	require.Equal(t, 3, calls)
	require.Len(t, billing.commands, 3)
}
