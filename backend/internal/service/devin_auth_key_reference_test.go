//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDevinKeyModePreservesOpaqueCredentialReference(t *testing.T) {
	service, mints, exchanges := devinTestService(t)
	upstream := service.upstream.(*devinHTTP)
	original := upstream.handle
	const reference = "synthetic.runtime.reference"
	upstream.handle = func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == devin.MintPath {
			var body struct {
				Metadata struct {
					APIKey string `json:"api_key"`
				} `json:"metadata"`
			}
			assert.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			assert.Equal(t, reference, request.Header.Get("X-Api-Key"))
			assert.Equal(t, reference, body.Metadata.APIKey)
		}
		return original(request)
	}
	view, err := service.ImportAPIKey(context.Background(), 7, reference, nil, 0)
	require.NoError(t, err)
	require.Equal(t, "ready", view.Status)
	require.True(t, view.AutoRecover)
	require.Equal(t, int32(1), mints.Load())
	require.Zero(t, exchanges.Load())
	saved, err := service.LoadAuth(context.Background(), view.ID, 7)
	require.NoError(t, err)
	require.Equal(t, reference, saved.Credentials["api_key"])
	require.Nil(t, saved.Credentials["access_token"])
	// Automatic credential-file imports retain their existing inference rules.
	imported, err := ParseDevinCredential(reference)
	require.NoError(t, err)
	require.Nil(t, imported["api_key"])
	require.Equal(t, "devin-session-token$"+reference, imported["access_token"])
}

func TestDevinKeyModeRejectsEmptyOrHeaderInjection(t *testing.T) {
	service, mints, _ := devinTestService(t)
	for _, value := range []string{"", "  ", "opaque\r\nInjected: value", "opaque\n", "\ropaque"} {
		_, err := service.ImportAPIKey(context.Background(), 7, value, nil, 0)
		require.Error(t, err)
	}
	require.Zero(t, mints.Load())
}
