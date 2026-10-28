//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDevinHTTPIngressRejectsFusionBeforeScheduling(t *testing.T) {
	for _, protocol := range []string{"responses", "chat", "anthropic"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
				h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				body := fmt.Sprintf(`{"model":"fusion-lead-fast-sidekick-worker","stream":%t,"input":"hello","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`, stream)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+protocol, strings.NewReader(body))
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, Group: &service.Group{Platform: service.PlatformDevin}, User: &service.User{ID: 2}})
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 1})
				switch protocol {
				case "responses":
					h.Responses(c)
				case "chat":
					h.ChatCompletions(c)
				case "anthropic":
					h.Messages(c)
				}
				require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), devin.CodeFusionUnsupported)
				require.NotEmpty(t, w.Header().Get("X-Sub2API-Request-Id"))
				require.NotContains(t, w.Body.String(), "data:")
			})
		}
	}
}

func TestDevinGuardIsProviderScopedAndPreservesCompositeIntent(t *testing.T) {
	for _, platform := range []string{service.PlatformDevin, service.PlatformOpenAI} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		key := &service.APIKey{Group: &service.Group{Platform: platform}}
		require.Equal(t, platform == service.PlatformDevin, rejectUnsupportedDevinIntent(c, key, "fusion", "responses"))
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx := service.WithCompositeRouteDecision(c.Request.Context(), service.CompositeRouteDecision{Matched: true, TargetPlatform: service.PlatformDevin, PublicModel: "fusion-lead-sidekick-worker", UpstreamModel: "ordinary-lead"})
	c.Request = c.Request.WithContext(ctx)
	require.True(t, rejectUnsupportedDevinIntent(c, &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}, "ordinary-lead", "responses"))
	require.Contains(t, w.Body.String(), devin.CodeFusionUnsupported)
}

func TestInboundModelCapturePrecedesMapping(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	setOpsRequestContext(c, "public-alias", true)
	setOpsRequestContext(c, "channel-target", true)
	require.Equal(t, "public-alias", c.Request.Context().Value(ctxkey.InboundModel))
	require.Equal(t, "channel-target", c.Request.Context().Value(ctxkey.Model))
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestedPublicModel, "composite-public"))
	setOpsRequestContext(c, "other-mapped", true)
	require.Equal(t, "public-alias", c.Request.Context().Value(ctxkey.InboundModel))
}
