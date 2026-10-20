package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDevinAuthorizationBodiesAreNotAudited(t *testing.T) {
	for _, route := range []string{"/api/v1/admin/devin/auth-sessions", "/api/v1/admin/devin/auth-sessions/:id/complete"} {
		t.Run(route, func(t *testing.T) {
			repo := &auditCaptureRepository{}
			audit := service.NewAuditLogService(repo, nil)
			audit.Start()
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set(string(ContextKeyUser), AuthSubject{UserID: 77})
				c.Set(string(ContextKeyUserRole), "admin")
				c.Next()
			})
			r.Use(gin.HandlerFunc(NewAuditLogMiddleware(audit)))
			r.POST(route, func(c *gin.Context) { c.Status(http.StatusOK) })
			target := route
			if route != "/api/v1/admin/devin/auth-sessions" {
				target = "/api/v1/admin/devin/auth-sessions/session/complete"
			}
			req := httptest.NewRequest("POST", target, bytes.NewBufferString(`{"mode":"import","content":"windsurf_api_key = \"canary-secret\"","code":"callback-with-canary-secret"}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code)
			audit.Stop()
			repo.mu.Lock()
			defer repo.mu.Unlock()
			require.Len(t, repo.logs, 1)
			require.Equal(t, "<credential-bearing body omitted>", repo.logs[0].RequestBody)
		})
	}
}
