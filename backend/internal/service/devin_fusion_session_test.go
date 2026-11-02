//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestDevinFusionOMPHeaderIdentityAndPrecedence(t *testing.T) {
	for _, test := range []struct {
		name     string
		headers  map[string]string
		want     string
		wantCode string
	}{
		{name: "OMP runtime session", headers: map[string]string{devinFusionOMPSessionHeader: "conversation-a"}, want: "conversation-a"},
		{name: "existing OpenCode session", headers: map[string]string{"X-OpenCode-Session": "conversation-a"}, want: "conversation-a"},
		{name: "matching runtime headers", headers: map[string]string{"X-Session-Id": "conversation-a", devinFusionOMPSessionHeader: "conversation-a"}, want: "conversation-a"},
		{name: "conflicting runtime headers", headers: map[string]string{"X-Session-Id": "conversation-a", devinFusionOMPSessionHeader: "conversation-b"}, wantCode: "devin_fusion_session_conflict"},
		{name: "explicit Fusion session overrides runtime", headers: map[string]string{fusion.HeaderSession: "returned-session", "X-Session-Id": "conversation-a", devinFusionOMPSessionHeader: "conversation-b"}, want: "returned-session"},
		{name: "explicit Fusion session ignores unused malformed OMP identity", headers: map[string]string{fusion.HeaderSession: "returned-session", devinFusionOMPSessionHeader: "malformed\nidentity"}, want: "returned-session"},
		{name: "no conversation header"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			for name, value := range test.headers {
				c.Request.Header.Set(name, value)
			}
			id, err := devinFusionRequestSessionID(c)
			if test.wantCode != "" {
				var typed *fusion.Error
				require.ErrorAs(t, err, &typed)
				require.Equal(t, test.wantCode, typed.Code)
				require.Empty(t, id)
				return
			}
			require.NoError(t, err)
			want := test.want
			if want != "" && test.headers[fusion.HeaderSession] == "" {
				want = "client_" + fusion.Digest(want)[:48]
			}
			require.Equal(t, want, id)
		})
	}
}

func TestDevinFusionOMPHeaderRejectsAmbiguousOrInvalidIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		values []string
	}{
		{name: "empty", values: []string{""}},
		{name: "spaces", values: []string{"   "}},
		{name: "embedded newline", values: []string{"conversation\nnext"}},
		{name: "leading tab", values: []string{"\tconversation"}},
		{name: "Unicode control", values: []string{"conversation\u0085next"}},
		{name: "invalid UTF-8", values: []string{"conversation\xff"}},
		{name: "over byte limit", values: []string{strings.Repeat("a", maxPersistedSessionIDLength+1)}},
		{name: "over byte limit with fewer runes", values: []string{strings.Repeat("界", 86)}},
		{name: "multiple values", values: []string{"conversation-a", "conversation-b"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Request.Header.Set("X-Session-Id", "otherwise-valid-session")
			for _, value := range test.values {
				c.Request.Header.Add(devinFusionOMPSessionHeader, value)
			}
			id, err := devinFusionRequestSessionID(c)
			var typed *fusion.Error
			require.ErrorAs(t, err, &typed)
			require.Equal(t, "devin_fusion_session_invalid", typed.Code)
			require.Equal(t, http.StatusBadRequest, typed.Status)
			require.Empty(t, id, "invalid runtime identity must not silently select a different session")
		})
	}
}

func TestDevinFusionOMPHeaderDoesNotChangeOrdinaryRouting(t *testing.T) {
	p := fusionTestProfile()
	a := &Account{ID: 7, Platform: PlatformDevin, Extra: map[string]any{fusion.ExtraProfiles: []fusion.Profile{p}}}
	s := &OpenAIGatewayService{devin: &DevinService{}, accountRepo: &fusionAccountRepo{account: a}}
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		for _, runtimeID := range []string{"conversation-a", "bad\nidentity"} {
			t.Run(protocol+"/"+runtimeID, func(t *testing.T) {
				body := []byte(strings.ReplaceAll(string(fusionProtocolBody(protocol, false, "call_ordinary_tool")), p.PublicModel, p.LeadModel))
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+protocol, nil)
				c.Request.Header.Set(devinFusionOMPSessionHeader, runtimeID)
				require.Empty(t, ExtractClientSessionID(c), "the new header must remain Fusion-specific")
				require.Empty(t, explicitOpenAIHeaderSessionID(c), "ordinary sticky scheduling must not adopt the new header")
				bound, err := s.BindDevinFusionRequest(c, &APIKey{ID: 2, UserID: 1}, p.LeadModel, body, protocol, nil)
				require.NoError(t, err)
				require.False(t, bound)
				require.False(t, DevinFusionRequest(c.Request.Context()))
			})
		}
	}
}

func TestDevinFusionOMPSessionRetryRestoresOnlyItsAuthenticatedScope(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			ctx := context.Background()
			p := fusionTestProfile()
			a := &Account{ID: 7, Platform: PlatformDevin, Extra: map[string]any{fusion.ExtraProfiles: []fusion.Profile{p}}}
			repo := &fusionAccountRepo{account: a}
			rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			store := devin.NewRedisSessionStore(rdb)
			s := &OpenAIGatewayService{devin: NewDevinService(repo, nil, nil, nil, store), accountRepo: repo}
			key := &APIKey{ID: 2, UserID: 1}
			bind := func(key *APIKey, clientID string) *devinFusionBinding {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+protocol, nil)
				c.Request.Header.Set(devinFusionOMPSessionHeader, clientID)
				c.Request.Header.Set("Idempotency-Key", "same-retried-request")
				bound, err := s.BindDevinFusionRequest(c, key, p.PublicModel, fusionProtocolBody(protocol, false, ""), protocol, nil)
				require.NoError(t, err)
				require.True(t, bound)
				binding, ok := c.Request.Context().Value(devinFusionBindingKey{}).(*devinFusionBinding)
				require.True(t, ok)
				require.Equal(t, binding.SessionID, c.Writer.Header().Get(fusion.HeaderSession))
				return binding
			}
			initial := bind(key, "conversation-a")
			require.Zero(t, initial.AccountID)
			session := &fusion.Session{ID: initial.SessionID, Scope: fusion.Scope(key.UserID, key.ID), AccountID: a.ID, Profile: p, ProfileRevision: p.Revision(), PublicModel: p.PublicModel, Role: fusion.Sidekick, State: fusion.Completed, ActiveRequest: -1, ExpiresAt: time.Now().Add(time.Hour)}
			payload, err := json.Marshal(session)
			require.NoError(t, err)
			require.NoError(t, store.Set(ctx, "devin:fusion:session:"+session.Scope+":"+fusion.Digest(session.ID), payload, time.Hour))
			retry := bind(key, "conversation-a")
			require.Equal(t, initial.SessionID, retry.SessionID)
			require.Equal(t, a.ID, retry.AccountID, "retry must restore the pinned account")
			require.NotEqual(t, retry.SessionID, bind(key, "conversation-b").SessionID)
			require.Zero(t, bind(&APIKey{ID: 3, UserID: key.UserID}, "conversation-a").AccountID, "another API key must not restore this account binding")
			require.Zero(t, bind(&APIKey{ID: key.ID, UserID: 9}, "conversation-a").AccountID, "another user must not restore this account binding")
		})
	}
}
