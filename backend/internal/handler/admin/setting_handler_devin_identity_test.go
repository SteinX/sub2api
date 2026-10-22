//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type devinIdentitySettingsRepo struct {
	service.SettingRepository
	values map[string]string
}

func (r *devinIdentitySettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	return r.values, nil
}
func (r *devinIdentitySettingsRepo) SetMultiple(_ context.Context, values map[string]string) error {
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func TestDevinIdentitySettingsHandlerRoundTripAndValidation(t *testing.T) {
	repo := &devinIdentitySettingsRepo{values: map[string]string{}}
	h := &SettingHandler{settingService: service.NewSettingService(repo, &config.Config{})}
	request := func(method, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, "/settings/devin-client-identity", strings.NewReader(body))
		handler(c)
		return w
	}
	profile := devin.DefaultClientIdentity()
	profile.Chat.IDEVersion, profile.Chat.ExtensionVersion = "4000.1.0", "4000.1.0"
	body, err := json.Marshal(service.DevinClientIdentitySettings{Manual: &profile})
	require.NoError(t, err)
	w := request(http.MethodPut, string(body), h.UpdateDevinClientIdentity)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "4000.1.0")
	w = request(http.MethodGet, "", h.GetDevinClientIdentity)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"source":"manual"`)
	stored := repo.values[service.SettingKeyDevinClientIdentity]
	for _, invalid := range []string{
		`null`,
		`{"manual":{"schema_version":2}}`,
		`{"source_url":"https://localhost/profile"}`,
		`{"auto_sync":true}`,
		`{"unknown":"typo"}`,
		string(body) + `{}`,
		strings.Replace(string(body), `"ide_version":"4000.1.0"`, `"api_key":"secret","ide_version":"4000.1.0"`, 1),
	} {
		w = request(http.MethodPut, invalid, h.UpdateDevinClientIdentity)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, stored, repo.values[service.SettingKeyDevinClientIdentity])
	}
	w = request(http.MethodPost, "", h.SyncDevinClientIdentity)
	require.Equal(t, http.StatusBadRequest, w.Code)
}
