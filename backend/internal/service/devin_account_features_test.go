//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestDevinAccountTestRecoversSessionBeforeOutput(t *testing.T) {
	for _, phase := range []string{"prepare", "chat", "after-content", "session-only"} {
		t.Run(phase, func(t *testing.T) {
			var mints, failures atomic.Int32
			base := devinFakeUpstream(t, &atomic.Int32{}, &atomic.Int32{})
			upstream := &devinHTTP{handle: func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == devin.MintPath {
					return devinReply(200, fmt.Appendf(nil, `{"sessionToken":"minted%d"}`, mints.Add(1))), nil
				}
				if (phase == "prepare" || phase == "session-only") && req.URL.Path == devin.UserJWTPath && failures.Add(1) == 1 {
					return devinReply(401, []byte(`{"code":"unauthenticated"}`)), nil
				}
				if (phase == "chat" || phase == "after-content") && req.URL.Path == devin.ChatPath && failures.Add(1) == 1 {
					if phase == "chat" {
						return devinReply(401, []byte(`{"code":"unauthenticated"}`)), nil
					}
					content, _ := proto.Marshal(&pb.GetChatMessageResponse{DeltaText: "partial"})
					return devinReply(200, append(devinTestFrame(0, content), devinTestFrame(2, []byte(`{"error":{"code":"unauthenticated"}}`))...)), nil
				}
				return base.handle(req)
			}}
			creds := map[string]any{"api_key": "cog_key", "model_mapping": map[string]any{"friendly": "swe-2-max"}}
			if phase == "session-only" {
				delete(creds, "api_key")
				creds["access_token"] = "session"
			}
			account := &Account{ID: 99, Platform: PlatformDevin, Credentials: creds}
			repo := &devinUsageRepo{accounts: []*Account{account}}
			d := NewDevinService(repo, nil, upstream, &config.Config{}, nil)
			s := &AccountTestService{accountRepo: repo, devin: d}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
			err := s.TestAccountConnection(c, account.ID, "friendly", "", "")
			if phase == "after-content" || phase == "session-only" {
				require.Error(t, err)
				require.NotContains(t, w.Body.String(), `"success":true`)
				if phase == "after-content" {
					require.Equal(t, 1, strings.Count(w.Body.String(), `"text":"partial"`))
					require.Equal(t, int32(1), mints.Load(), "must not replay a partially emitted test")
				} else {
					require.Zero(t, mints.Load())
				}
			} else {
				require.NoError(t, err)
				require.Contains(t, w.Body.String(), `"success":true`)
				require.Equal(t, int32(2), mints.Load())
			}
		})
	}
}

type devinFeatureRepo struct {
	devinUsageRepo
	updates map[string]any
}

func (r *devinFeatureRepo) UpdateExtra(_ context.Context, _ int64, values map[string]any) error {
	r.updates = values
	return nil
}

func TestDevinManualModelSyncRefreshesTheSharedCatalog(t *testing.T) {
	var probes atomic.Int32
	upstream := &devinHTTP{handle: func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, devin.ModelsPath, req.URL.Path)
		body, _ := proto.Marshal(&pb.GetCliModelConfigsResponse{ClientModelConfigs: []*pb.ClientModelConfig{{ModelUid: fmt.Sprintf("swe-%d", probes.Add(1))}}})
		return devinReply(200, body), nil
	}}
	account := &Account{ID: 1, Platform: PlatformDevin, Credentials: map[string]any{"access_token": "session"}}
	repo := &devinFeatureRepo{}
	d := NewDevinService(repo, nil, upstream, &config.Config{}, nil)
	s := &AccountTestService{accountRepo: repo, devin: d}
	_, err := d.Models(context.Background(), account)
	require.NoError(t, err)
	catalog, err := s.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"swe-2"}, catalog.Models)
	require.Equal(t, []string{"swe-2"}, repo.updates["devin_models"])
	ids, err := s.FetchUpstreamSupportedModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, catalog.Models, ids)
	require.Equal(t, int32(2), probes.Load())
}

func TestDevinManualModelSyncPreservesCatalogWithoutUsableModels(t *testing.T) {
	for _, models := range [][]*pb.ClientModelConfig{nil, {{ModelUid: "swe-2-max", Disabled: true}}} {
		var probes atomic.Int32
		upstream := &devinHTTP{handle: func(*http.Request) (*http.Response, error) {
			catalog := models
			if probes.Add(1) == 1 {
				catalog = []*pb.ClientModelConfig{{ModelUid: "swe-2-max"}}
			}
			body, err := proto.Marshal(&pb.GetCliModelConfigsResponse{ClientModelConfigs: catalog})
			assert.NoError(t, err)
			return devinReply(200, body), nil
		}}
		account := &Account{ID: 1, Platform: PlatformDevin, Credentials: map[string]any{"access_token": "session"}, Extra: map[string]any{"devin_models": []string{"swe-2-max"}}}
		repo := &devinFeatureRepo{}
		s := &AccountTestService{accountRepo: repo, devin: NewDevinService(repo, nil, upstream, &config.Config{}, nil)}
		_, err := s.devin.Models(context.Background(), account)
		require.NoError(t, err)
		_, err = s.SyncUpstreamModelCatalog(context.Background(), account)
		require.ErrorContains(t, err, "no usable models")
		require.Nil(t, repo.updates, "must not overwrite the last working scheduling catalog")
		require.True(t, account.supportsDevinModel("swe-2-max"))
		cached, err := s.devin.Models(context.Background(), account)
		require.NoError(t, err)
		require.Len(t, cached, 1, "forwarding must still see the previous shared runtime catalog")
		require.Equal(t, "swe-2-max", devin.ModelID(cached[0]))
		require.Equal(t, int32(2), probes.Load(), "the failed sync must not replace the good cache entry")
	}
}

func TestDevinReauthorizationValidatesTheExistingEndpointAndProxy(t *testing.T) {
	d, _, _ := devinTestService(t)
	proxyID := int64(3)
	proxy := &Proxy{ID: proxyID, Protocol: "http", Host: "proxy.example", Port: 8080}
	target := &Account{ID: 9, Platform: PlatformDevin, Credentials: map[string]any{"base_url": "https://cascade.example"}, ProxyID: &proxyID, Proxy: proxy}
	d.repo = &devinUsageRepo{accounts: []*Account{target}}
	upstream := d.upstream.(*devinHTTP)
	upstream.observe = func(req *http.Request, _ int64) {
		assert.Equal(t, "cascade.example", req.URL.Host)
	}
	session := &DevinAuthSession{ID: "reconnect", Owner: 7, TargetAccountID: 9, Credentials: map[string]any{"access_token": "new-session"}, ExpiresAt: time.Now().Add(time.Minute).Unix()}
	require.NoError(t, d.ValidateAuth(context.Background(), session))
	require.Equal(t, "ready", session.Status)
	require.Equal(t, "new-session", session.Credentials["access_token"])
	require.NotContains(t, session.Credentials, "base_url", "validation must not replace the account's saved settings")
}
