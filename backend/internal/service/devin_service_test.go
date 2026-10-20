//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

type devinHTTP struct {
	handle  func(*http.Request) (*http.Response, error)
	observe func(*http.Request, int64)
}

func (h *devinHTTP) Do(r *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	if h.observe != nil {
		h.observe(r, id)
	}
	return h.handle(r)
}
func (h *devinHTTP) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(r, p, id, n)
}
func devinReply(status int, b []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b))}
}
func devinTestFrame(flags byte, b []byte) []byte {
	h := make([]byte, 5)
	h[0] = flags
	binary.BigEndian.PutUint32(h[1:], uint32(len(b)))
	return append(h, b...)
}
func devinFakeUpstream(t *testing.T, mints, exchanges *atomic.Int32) *devinHTTP {
	t.Helper()
	return &devinHTTP{handle: func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case devin.MintPath:
			mints.Add(1)
			return devinReply(200, []byte(`{"sessionToken":"devin-session-token$minted"}`)), nil
		case "/auth/cli/token":
			exchanges.Add(1)
			return devinReply(200, []byte(`{"token":"login-token"}`)), nil
		case devin.ModelsPath:
			b, _ := proto.Marshal(&pb.GetCliModelConfigsResponse{ClientModelConfigs: []*pb.ClientModelConfig{{ModelUid: "swe-2-max", Label: "SWE-2 Max", MaxTokens: 262000, ModelInfo: &pb.ModelInfo{ModelUid: "swe-2-max", MaxOutputTokens: 64000, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}}}})
			return devinReply(200, b), nil
		case devin.UserJWTPath:
			b, _ := proto.Marshal(&pb.GetUserJwtResponse{UserJwt: "user-jwt"})
			return devinReply(200, b), nil
		case devin.ChatPath:
			_, b, err := devin.ReadFrame(r.Body)
			if err != nil {
				t.Error(err)
			}
			q := &pb.GetChatMessageRequest{}
			_ = proto.Unmarshal(b, q)
			if q.Metadata.UserJwt != "user-jwt" {
				t.Error("missing user JWT")
			}
			b, _ = proto.Marshal(&pb.GetChatMessageResponse{MessageId: "message", DeltaText: "hello", Usage: &pb.ModelUsageStats{InputTokens: 11, OutputTokens: 3, CacheReadTokens: 5, CacheWriteTokens: 2, BillingModelUid: "billed-swe-model"}, StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN})
			data := append(devinTestFrame(0, b), devinTestFrame(2, []byte(`{}`))...)
			return devinReply(200, data), nil
		}
		t.Errorf("unexpected endpoint %s", r.URL.Path)
		return devinReply(404, nil), nil
	}}
}
func devinTestService(t *testing.T) (*DevinService, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mints, exchanges := &atomic.Int32{}, &atomic.Int32{}
	cfg := &config.Config{}
	d := NewDevinService(nil, nil, devinFakeUpstream(t, mints, exchanges), cfg, devin.NewRedisSessionStore(rdb))
	return d, mints, exchanges
}

func TestDevinBrowserAuthorizationOwnershipAndState(t *testing.T) {
	d, _, exchanges := devinTestService(t)
	ctx := context.Background()
	view, err := d.StartAuth(ctx, 7, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.LoadAuth(ctx, view.ID, 8); err == nil {
		t.Fatal("another admin could read the session")
	}
	u, _ := url.Parse(view.AuthURL)
	state := u.Query().Get("state")
	if _, err = d.CompleteAuth(ctx, view.ID, "code", "wrong", view.PairingTicket, -1); err == nil {
		t.Fatal("bad state accepted")
	}
	if exchanges.Load() != 0 {
		t.Fatal("bad state spent authorization code")
	}
	if _, err = d.CompleteAuth(ctx, view.ID, "code", state, "wrong", -1); err == nil {
		t.Fatal("bad pairing ticket accepted")
	}
	ready, err := d.CompleteAuth(ctx, view.ID, "code", state, view.PairingTicket, -1)
	if err != nil || ready.Status != "ready" {
		t.Fatal(err, ready)
	}
	if exchanges.Load() != 1 {
		t.Fatal("exchange count", exchanges.Load())
	}
	raw, _ := json.Marshal(ready)
	for _, secret := range []string{"login-token", "verifier", "access_token", view.PairingTicket} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("public summary leaked %s", secret)
		}
	}
	if err = d.CancelAuth(ctx, view.ID, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = d.LoadAuth(ctx, view.ID, 7); err == nil {
		t.Fatal("cancelled session retained")
	}
}

func TestDevinAuthorizationTransportIsolation(t *testing.T) {
	d, _, _ := devinTestService(t)
	upstream, ok := d.upstream.(*devinHTTP)
	if !ok {
		t.Fatal("missing upstream test double")
	}
	var ids []int64
	upstream.observe = func(_ *http.Request, id int64) { ids = append(ids, id) }
	ctx := context.Background()
	seen := map[int64]bool{}
	for _, owner := range []int64{7, 8} {
		ids = nil
		view, err := d.StartAuth(ctx, owner, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(view.AuthURL)
		ready, err := d.CompleteAuth(ctx, view.ID, "code", u.Query().Get("state"), view.PairingTicket, -1)
		if err != nil || ready.Status != "ready" {
			t.Fatal(err, ready)
		}
		if len(ids) != 2 || ids[0] >= 0 || ids[0] != ids[1] || seen[ids[0]] {
			t.Fatalf("exchange/discovery transport IDs were not stable and isolated: %v", ids)
		}
		seen[ids[0]] = true
	}
	ids = nil
	view, err := d.ImportAuth(ctx, 9, "login-token", nil, 0)
	if err != nil || view.Status != "ready" {
		t.Fatal(err, view)
	}
	if len(ids) != 1 || ids[0] >= 0 || seen[ids[0]] {
		t.Fatalf("import reused another authorization's transport: %v", ids)
	}
}

func TestDevinAuthorizationDenialCompletesThePendingFlow(t *testing.T) {
	d, _, exchanges := devinTestService(t)
	ctx := context.Background()
	view, err := d.StartAuth(ctx, 7, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(view.AuthURL)
	state := u.Query().Get("state")
	callback := "http://127.0.0.1:59653/callback?error=access_denied&state=" + url.QueryEscape(state)
	result, err := d.CompleteAuth(ctx, view.ID, callback, "", view.PairingTicket, -1)
	if err != nil || result.Status != "failed" {
		t.Fatal(err, result)
	}
	if exchanges.Load() != 0 {
		t.Fatal("denied login attempted token exchange")
	}
}
func TestDevinCredentialsAndMintSingleflight(t *testing.T) {
	d, mints, _ := devinTestService(t)
	ctx := context.Background()
	key, err := ParseDevinCredential("cog_test")
	if err != nil {
		t.Fatal(err)
	}
	a := &Account{Platform: PlatformDevin, Type: AccountTypeAPIKey, Credentials: key}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := d.Token(ctx, a, false)
			if err != nil || token != "devin-session-token$minted" {
				t.Errorf("token: %q %v", token, err)
			}
		}()
	}
	wg.Wait()
	if mints.Load() != 1 {
		t.Fatal("concurrent requests minted", mints.Load())
	}
	view, err := d.ImportAuth(ctx, 1, "windsurf_api_key = \"opaque\"", nil, 0)
	if err != nil || view.Status != "ready" || view.AutoRecover {
		t.Fatal(err, view)
	}
	if len(view.Models) != 1 || view.Models[0] != "swe-2-max" {
		t.Fatal(view.Models)
	}
	a = &Account{Platform: PlatformDevin, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "opaque"}}
	if a.UsesOpenAICodexProtocol() {
		t.Fatal("Devin login was classified as Codex")
	}
	if _, err = d.Token(ctx, a, true); !devin.IsUnauthenticated(err) {
		t.Fatalf("CLI token should require reauthorization: %v", err)
	}
}
func TestDevinGatewayProtocolsAndAccounting(t *testing.T) {
	cases := []struct{ protocol, body, contains string }{{"responses", `{"model":"swe-2-max","input":"hello"}`, `"object":"response"`}, {"chat", `{"model":"swe-2-max","messages":[{"role":"user","content":"hello"}]}`, `"object":"chat.completion"`}, {"anthropic", `{"model":"swe-2-max","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`, `"type":"message"`}}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			name := tc.protocol
			if stream {
				name += " stream"
			}
			t.Run(name, func(t *testing.T) {
				d, _, _ := devinTestService(t)
				a := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "test"}}
				s := &OpenAIGatewayService{devin: d}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				body := tc.body
				if stream {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				result, err := s.forwardDevin(context.Background(), c, a, []byte(body), tc.protocol, "session", "")
				if err != nil {
					t.Fatal(err, w.Body.String())
				}
				if result.Usage.InputTokens != 18 || result.Usage.CacheCreationInputTokens != 2 || result.Usage.CacheReadInputTokens != 5 || result.Usage.OutputTokens != 3 {
					t.Fatal(result.Usage)
				}
				if result.BillingModel != "billed-swe-model" || result.Model != "swe-2-max" {
					t.Fatalf("upstream billing model was not propagated: %+v", result)
				}
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				if !stream && !strings.Contains(w.Body.String(), tc.contains) {
					t.Fatal(w.Body.String())
				}
				if stream && !strings.Contains(w.Body.String(), "data:") {
					t.Fatal("not streamed", w.Body.String())
				}
				if !strings.Contains(w.Body.String(), "hello") {
					t.Fatal("text dropped", w.Body.String())
				}
			})
		}
	}
}

func TestDevinGatewayBillingModelFallback(t *testing.T) {
	d, _, _ := devinTestService(t)
	upstream, ok := d.upstream.(*devinHTTP)
	if !ok {
		t.Fatal("missing upstream test double")
	}
	original := upstream.handle
	upstream.handle = func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != devin.ChatPath {
			return original(r)
		}
		b, _ := proto.Marshal(&pb.GetChatMessageResponse{DeltaText: "hello", Usage: &pb.ModelUsageStats{InputTokens: 1, OutputTokens: 1}, StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN})
		return devinReply(200, append(devinTestFrame(0, b), devinTestFrame(2, []byte(`{}`))...)), nil
	}
	a := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "test"}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	result, err := (&OpenAIGatewayService{devin: d}).forwardDevin(context.Background(), c, a, []byte(`{"model":"swe-2-max","input":"hello"}`), "responses", "session", "")
	if err != nil || result == nil || result.BillingModel != "swe-2-max" {
		t.Fatalf("missing billing UID did not fall back to the requested model: %v, %v", result, err)
	}
}
