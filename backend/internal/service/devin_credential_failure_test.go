//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type devinCredentialFailureRepo struct {
	AccountRepository
	errors, limited []int64
}

func (r *devinCredentialFailureRepo) SetError(_ context.Context, id int64, _ string) error {
	r.errors = append(r.errors, id)
	return nil
}
func (r *devinCredentialFailureRepo) SetRateLimited(_ context.Context, id int64, _ time.Time) error {
	r.limited = append(r.limited, id)
	return nil
}

func TestDevinRequestCredentialFailuresPermitAccountFailover(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			d, _, _ := devinTestService(t)
			upstream, ok := d.upstream.(*devinHTTP)
			require.True(t, ok)
			original := upstream.handle
			upstream.handle = func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == devin.MintPath && r.Header.Get("X-Api-Key") == "bad-key" {
					return devinReply(status, []byte(`{"message":"private upstream detail"}`)), nil
				}
				return original(r)
			}
			repo := &devinCredentialFailureRepo{}
			gateway := &OpenAIGatewayService{devin: d, accountRepo: repo}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			bad := &Account{ID: 1, Platform: PlatformDevin, Credentials: map[string]any{"api_key": "bad-key"}}
			_, _, err := gateway.GetRequestCredential(c.Request.Context(), c, bad)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Equal(t, status, failover.StatusCode)
			require.True(t, failover.ShouldRetryNextAccount())
			require.NotContains(t, string(failover.ResponseBody), "private upstream detail")
			require.False(t, c.Writer.Written(), "credential errors must not write HTTP after a WS upgrade")
			require.Empty(t, w.Body.String())
			if status == 401 {
				require.Equal(t, []int64{bad.ID}, repo.errors)
			} else {
				require.Empty(t, repo.errors)
			}
			if status == 429 {
				require.Equal(t, []int64{bad.ID}, repo.limited)
			} else {
				require.Empty(t, repo.limited)
			}
			healthy := &Account{ID: 2, Platform: PlatformDevin, Credentials: map[string]any{"api_key": "healthy-key"}}
			token, kind, err := gateway.GetRequestCredential(c.Request.Context(), c, healthy)
			require.NoError(t, err)
			require.NotEmpty(t, token)
			require.Equal(t, "devin", kind)
		})
	}
}

func TestDevinRequestCredentialCancellationPreservesAccountHealth(t *testing.T) {
	d, mints, _ := devinTestService(t)
	repo := &devinCredentialFailureRepo{}
	gateway := &OpenAIGatewayService{devin: d, accountRepo: repo}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := gateway.GetRequestCredential(ctx, nil, &Account{ID: 1, Platform: PlatformDevin, Credentials: map[string]any{"api_key": "key"}})
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, repo.errors)
	require.Empty(t, repo.limited)
	require.Zero(t, mints.Load())
}

func TestDevinHTTPInvalidArgumentStillReturnsClientError(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	upstream := &devin.Error{Code: "invalid_argument", Status: http.StatusBadRequest}
	err := (&OpenAIGatewayService{}).devinForwardFailure(c, &Account{ID: 1}, upstream)
	require.Same(t, upstream, err)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "invalid_request_error")
}
