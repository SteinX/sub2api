package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestContextCompactValidationAndDefaultDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, body string
		auth       bool
		want       int
		code       string
	}{
		{"unauthenticated", `{}`, false, 401, ""},
		{"bad_json", `{`, true, 400, "context_input_invalid"},
		{"bad_mode", `{"mode":"native"}`, true, 400, "context_input_invalid"},
		{"sidekick_lane", `{"mode":"internal","session_id":"ctx_x","lane":"sidekick","expected_revision":1}`, true, 400, "context_input_invalid"},
		{"missing_revision", `{"mode":"internal","session_id":"ctx_x","lane":"main"}`, true, 400, "context_input_invalid"},
		{"disabled", `{"mode":"stateless","model":"concrete","input":"a"}`, true, 404, "context_disabled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/context/compact", strings.NewReader(tt.body))
			if tt.auth {
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 1, User: &service.User{ID: 1}, Group: &service.Group{Platform: service.PlatformDevin}})
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
			}
			h := &OpenAIGatewayHandler{cfg: &config.Config{}, gatewayService: &service.OpenAIGatewayService{}}
			h.ContextCompact(c)
			require.Equal(t, tt.want, w.Code, w.Body.String())
			if tt.code != "" {
				require.Contains(t, w.Body.String(), tt.code)
			}
		})
	}
}

func TestManagedResponsesPreserveCompositeEndpointGate(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"claude-opus-4-5","input":"hello"}`))
	c.Request.Header.Set(mc.HeaderMode, "managed")
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 1, User: &service.User{ID: 1}, Group: &service.Group{Platform: service.PlatformComposite}})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
	h := &OpenAIGatewayHandler{
		cfg:                 &config.Config{},
		gatewayService:      &service.OpenAIGatewayService{},
		billingCacheService: &service.BillingCacheService{},
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(nil), SSEPingFormatNone, 0),
	}
	h.Responses(c)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "composite groups")
}
func TestContextErrorsAreBoundedAndHideStorageDetails(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want int
		code string
	}{{mc.ErrNotFound, 404, "context_not_found"}, {mc.E("attempt_uncertain", "outcome unknown", 409), 409, "context_attempt_uncertain"}, {errors.New("postgres: credential and private query details"), 503, "context_unavailable"}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		(&OpenAIGatewayHandler{}).contextError(c, tt.err)
		require.Equal(t, tt.want, w.Code)
		require.Contains(t, w.Body.String(), tt.code)
		require.NotContains(t, w.Body.String(), "credential")
	}
	require.Equal(t, http.StatusRequestEntityTooLarge, boundedContextStatus(&http.MaxBytesError{Limit: 1}))
}
func TestContextUnavailableControlResourcesFailClosed(t *testing.T) {
	for _, invoke := range []func(*OpenAIGatewayHandler, *gin.Context){(*OpenAIGatewayHandler).ContextSession, (*OpenAIGatewayHandler).ContextJob} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/v1/context/sessions/missing", nil)
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 1})
		invoke(&OpenAIGatewayHandler{}, c)
		require.Equal(t, 503, w.Code)
	}
}

func TestContextCompactBodyLimit(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/context/compact", strings.NewReader(strings.Repeat("x", 1025)))
	c.Request.Body = http.MaxBytesReader(w, c.Request.Body, 1024)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 1})
	h := &OpenAIGatewayHandler{cfg: &config.Config{}, gatewayService: &service.OpenAIGatewayService{}}
	h.ContextCompact(c)
	require.Equal(t, 413, w.Code)
}
