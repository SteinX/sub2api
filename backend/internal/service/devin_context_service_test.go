//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// Small protocol double: SQL transactions/fencing are exercised separately
// against PostgreSQL. Unused read APIs intentionally retain the nil interface.
type contextGatewayStore struct {
	mc.Store
	mu      sync.Mutex
	records map[string]mc.Record
	blobs   map[string][]byte
	leases  map[string]mc.Lease
}

func newContextGatewayStore() *contextGatewayStore {
	return &contextGatewayStore{records: map[string]mc.Record{}, blobs: map[string][]byte{}, leases: map[string]mc.Lease{}}
}
func contextRecordKey(scope, kind, id string) string { return scope + ":" + kind + ":" + id }
func (s *contextGatewayStore) Read(_ context.Context, scope, kind, id string) (*mc.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[contextRecordKey(scope, kind, id)]
	if !ok {
		return nil, mc.ErrNotFound
	}
	r.Data = append([]byte(nil), r.Data...)
	return &r, nil
}
func (s *contextGatewayStore) Create(_ context.Context, r *mc.Record, _ *mc.Policy) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := contextRecordKey(r.Scope, r.Kind, r.ID)
	if _, ok := s.records[k]; ok {
		return false, nil
	}
	r.Revision = 1
	v := *r
	v.Data = append([]byte(nil), r.Data...)
	s.records[k] = v
	return true, nil
}
func (s *contextGatewayStore) CAS(_ context.Context, r *mc.Record, l *mc.Lease, blobs []mc.Blob, _ *mc.Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := contextRecordKey(r.Scope, r.Kind, r.ID)
	old, ok := s.records[k]
	if !ok || old.Revision != r.Revision || old.Cancelled {
		return fmt.Errorf("CAS conflict")
	}
	if l != nil && s.leases[k] != *l {
		return fmt.Errorf("lease conflict")
	}
	r.Revision++
	v := *r
	v.Data = append([]byte(nil), r.Data...)
	s.records[k] = v
	for _, b := range blobs {
		s.blobs[contextRecordKey(r.Scope, "blob", b.ID)] = append([]byte(nil), b.Data...)
	}
	return nil
}
func (s *contextGatewayStore) Acquire(_ context.Context, scope, kind, id, owner string, _ time.Duration) (mc.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := contextRecordKey(scope, kind, id)
	if _, ok := s.leases[k]; ok {
		return mc.Lease{}, fmt.Errorf("busy")
	}
	l := mc.Lease{Scope: scope, Kind: kind, ID: id, Owner: owner, Token: 1}
	s.leases[k] = l
	return l, nil
}
func (s *contextGatewayStore) Renew(context.Context, mc.Lease, time.Duration) error { return nil }
func (s *contextGatewayStore) Release(_ context.Context, l mc.Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.leases, contextRecordKey(l.Scope, l.Kind, l.ID))
	return nil
}
func (s *contextGatewayStore) Blob(_ context.Context, scope, id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[contextRecordKey(scope, "blob", id)]
	if !ok {
		return nil, mc.ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func (s *contextGatewayStore) Pending(_ context.Context, limit int) ([]*mc.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*mc.Record
	for _, r := range s.records {
		if r.Kind == "attempt" {
			var a mc.Attempt
			_ = json.Unmarshal(r.Data, &a)
			if !a.Settled {
				v := r
				v.Data = append([]byte(nil), r.Data...)
				out = append(out, &v)
			}
		}
	}
	return out, nil
}
func (s *contextGatewayStore) Cleanup(context.Context, int) (int64, error) { return 0, nil }
func contextEnabledFixture() (*config.Config, *APIKey) {
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.DevinContext = config.DevinContextConfig{Enabled: true, ManualCompact: true, AutoCompaction: true, APIKeyIDs: []int64{22}, ModelUIDs: []string{"gpt-5.5"}}
	id := int64(9)
	key := &APIKey{ID: 22, UserID: 33, Status: StatusActive, GroupID: &id, Group: &Group{ID: 9, Platform: PlatformDevin, RateMultiplier: 1}, User: &User{ID: 33, Status: StatusActive, Balance: 100, Concurrency: 1}}
	return cfg, key
}
func TestContextAccessGatesAndFreshAuthorization(t *testing.T) {
	cfg, key := contextEnabledFixture()
	a := &Account{ID: 7, Platform: PlatformDevin, Status: StatusActive, Schedulable: true, GroupIDs: []int64{9}, Credentials: map[string]any{"devin_model_uids": []string{"gpt-5.5"}}}
	s := &OpenAIGatewayService{cfg: cfg, accountRepo: &fusionAccountRepo{account: a}}
	require.NoError(t, contextAccountAccessError(s, key, a, "gpt-5.5", true))
	cfg.Gateway.DevinContext.Enabled = false
	require.Error(t, contextAccountAccessError(s, key, a, "gpt-5.5", false))
	cfg.Gateway.DevinContext.Enabled = true
	cfg.Gateway.DevinContext.APIKeyIDs = nil
	require.Error(t, contextAccountAccessError(s, key, a, "gpt-5.5", false))
	cfg.Gateway.DevinContext.APIKeyIDs = []int64{22}
	require.Error(t, contextAccountAccessError(s, key, a, "another-model", false))
	cfg.Gateway.DevinContext.ManualCompact = false
	require.Error(t, contextAccountAccessError(s, key, a, "gpt-5.5", true))
	cfg.Gateway.DevinContext.ManualCompact = true
	// Use a concrete catalog grant and a freshly loaded key for each paid phase.
	a.Extra = devinCatalogExtra([]*pb.ClientModelConfig{{ModelUid: "gpt-5.5"}}, "fixture")
	repo := &apiKeyRepoStub{apiKey: key}
	keys := &APIKeyService{apiKeyRepo: repo}
	x := &contextExecutor{physical: &devinFusionExecutor{service: s, publicModel: "gpt-5.5"}, keys: keys, owner: mc.Owner{UserID: 33, KeyID: 22, AccountID: 7, GroupID: 9}}
	require.NoError(t, x.check(context.Background(), "inference"))
	key.Status = "disabled"
	require.Error(t, x.check(context.Background(), "inference"))
	key.Status = StatusActive
	cfg.Gateway.DevinContext.AccountIDs = []int64{8}
	require.Error(t, x.check(context.Background(), "inference"))
	cfg.Gateway.DevinContext.AccountIDs = nil
	other := int64(10)
	key.GroupID = &other
	require.Error(t, x.check(context.Background(), "inference"))
	key.GroupID = &key.Group.ID
	a.Schedulable = false
	require.Error(t, x.check(context.Background(), "inference"))
	mapped := &Account{ID: 7, Platform: PlatformDevin, Status: StatusActive, Schedulable: true, GroupIDs: []int64{9}, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5.5": "gpt-5.4"}}, Extra: devinCatalogExtra([]*pb.ClientModelConfig{{ModelUid: "gpt-5.5"}, {ModelUid: "gpt-5.4"}}, "fixture")}
	require.True(t, mapped.supportsDevinModel("gpt-5.5"), "ordinary forwarding may honor an explicit alias")
	s.accountRepo = &fusionAccountRepo{account: mapped}
	require.Error(t, x.check(context.Background(), "inference"), "managed concrete models must not bypass or silently apply an account remapping")
}
func TestContextGatewayToolContinuationBillingRecoveryAndSSE(t *testing.T) {
	cfg, key := contextEnabledFixture()
	cfg.Gateway.DevinContext.Enabled = false
	cfg.Gateway.DevinContext.APIKeyIDs = nil
	cfg.Gateway.DevinContext.ModelUIDs = nil
	models := []*pb.ClientModelConfig{{ModelUid: "gpt-5.5", MaxTokens: 65536, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}}}
	a := &Account{ID: 7, Platform: PlatformDevin, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{9}, Credentials: map[string]any{"access_token": "synthetic-token"}, Extra: devinCatalogExtra(models, "fixture")}
	a.Extra[DevinContextEnabledExtra] = true
	calls := 0
	upstream := &devinHTTP{handle: func(r *http.Request) (*http.Response, error) {
		var reply proto.Message
		switch r.URL.Path {
		case devin.ModelsPath:
			reply = &pb.GetCliModelConfigsResponse{ClientModelConfigs: models}
		case devin.UserJWTPath:
			reply = &pb.GetUserJwtResponse{UserJwt: "synthetic-jwt"}
		case devin.ChatPath:
			_, b, err := devin.ReadFrame(r.Body)
			require.NoError(t, err)
			wire := &pb.GetChatMessageRequest{}
			require.NoError(t, proto.Unmarshal(b, wire))
			calls++
			message := &pb.GetChatMessageResponse{ActualModelUid: proto.String(wire.ChatModelUid), MessageId: fmt.Sprint(calls), StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN, Usage: &pb.ModelUsageStats{InputTokens: 100, OutputTokens: 20, BillingModelUid: wire.ChatModelUid}}
			if calls == 1 {
				message.DeltaToolCalls = []*pb.ChatToolCall{{Id: "provider-call", Name: "read_file", ArgumentsJson: `{"path":"main.go"}`}}
			} else {
				require.Contains(t, fmt.Sprint(wire.ChatMessagePrompts), "actual tool evidence")
				message.DeltaText = "Verified the change."
			}
			b, err = proto.Marshal(message)
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
	billing := &fusionBillingRepo{fail: true}
	s := newOpenAIRecordUsageServiceWithBillingRepoForTest(&openAIRecordUsageLogRepoStub{inserted: true}, billing, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	s.cfg = cfg
	cfg.Billing.InflightReservation.Enabled = true
	cache := &fusionTestBillingCache{newMemInflightCache(100)}
	s.billingCacheService = NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
	defer s.billingCacheService.Stop()
	s.accountRepo = repo
	s.concurrencyService = NewConcurrencyService(&stubConcurrencyCacheForTest{acquireResult: true})
	s.devin = NewDevinService(repo, nil, upstream, cfg, nil)
	s.devin.contextStore = newContextGatewayStore()
	keys := &APIKeyService{apiKeyRepo: &apiKeyRepoStub{apiKey: key}}
	request := func(keyID, session string, body []byte) (*ContextRunResult, error) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Request.Header.Set(mc.HeaderMode, "managed")
		c.Request.Header.Set("Idempotency-Key", keyID)
		return s.RunContext(c, key, keys, body, "responses", session, 0, 0)
	}
	body := []byte(`{"model":"gpt-5.5","input":"Preserve API compatibility","max_output_tokens":128,"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`)
	_, err := request("first", "", body)
	require.Error(t, err, "known usage cannot publish before settlement")
	require.Equal(t, 1, calls)
	billing.fail = false
	a.Extra[DevinContextEnabledExtra] = false
	a.Schedulable = false
	contextMaintenancePass(context.Background(), s, s.devin, keys)
	require.Equal(t, 1, calls)
	require.Len(t, billing.commands, 1, "recovery settles known usage after rollout/account disable")
	a.Extra[DevinContextEnabledExtra] = true
	a.Schedulable = true
	first, err := request("first", "", body)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Len(t, billing.commands, 1)
	require.True(t, strings.HasPrefix(billing.commands[0].RequestID, "context:"))
	require.NotEmpty(t, first.SessionID)
	again, err := request("first", "", body)
	require.NoError(t, err)
	require.True(t, again.Replayed)
	require.Equal(t, first.Response.ID, again.Response.ID)
	require.Positive(t, again.Revision)
	require.Equal(t, 1, calls)
	require.Len(t, billing.commands, 1)
	var callID string
	for _, o := range first.Response.Output {
		if o.Type == "function_call" {
			callID = o.CallID
		}
	}
	require.NotEmpty(t, callID)
	nextBody, _ := json.Marshal(map[string]any{"model": "gpt-5.5", "previous_response_id": first.Response.ID, "input": []any{map[string]any{"type": "function_call_output", "call_id": callID, "output": "actual tool evidence"}}, "max_output_tokens": 128})
	a.Extra[DevinContextEnabledExtra] = false
	_, err = request("blocked-second", first.SessionID, nextBody)
	require.Error(t, err, "disabling the account blocks an existing session")
	require.Equal(t, 1, calls, "disabled accounts must not dispatch or charge")
	require.Len(t, billing.commands, 1)
	a.Extra[DevinContextEnabledExtra] = true
	next, err := request("second", first.SessionID, nextBody)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Len(t, billing.commands, 2)
	for _, stream := range []bool{false, true} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		require.NoError(t, WriteContextResponse(c, next.Response, stream))
		require.Equal(t, 200, w.Code)
		require.Contains(t, w.Body.String(), "Verified the change")
		if stream {
			require.Contains(t, w.Header().Get("Content-Type"), "text/event-stream")
			require.Contains(t, w.Body.String(), "response.completed")
		} else {
			require.Contains(t, w.Header().Get("Content-Type"), "application/json")
		}
	}
	require.NoError(t, s.devin.managedEngine().Close(context.Background()))
}
func TestContextModelBudgetAppliesConcreteOutputConfiguration(t *testing.T) {
	x := &devinFusionExecutor{attemptPrefix: "context", profile: fusionTestProfile(), models: []*pb.ClientModelConfig{{ModelUid: "gpt-5.5", MaxTokens: 8192, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 512}}}}
	n := 128
	r := &apicompat.ResponsesRequest{Model: "gpt-5.5", Input: json.RawMessage(`"hello"`), MaxOutputTokens: &n}
	size, safe, err := x.MeasureContext(r)
	require.NoError(t, err)
	require.Greater(t, size, 0)
	require.Less(t, safe, 8192-128)
	n = 1000
	_, _, err = x.MeasureContext(r)
	require.Error(t, err, "output above the concrete capability is rejected before payment")
}
