//go:build devine2e

package devine2e

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Model an already-running scheduler writer holding the old decoded row.
// Use the real repository publisher, not raw Redis mutation or a timing sleep.
// This helper is installed only in the synthetic protocol fixture.
func delayedDevinCachePublication(t *testing.T, app *application, key string, accountID int64, realModel, publicModel string) func() map[string]any {
	t.Helper()
	request(t, app, "GET", "/v1/models", key, nil, 200)                                                                                                          // caller-visible model grant precondition
	validateResponse(t, request(t, app, "POST", "/v1/responses", key, map[string]any{"model": realModel, "input": "Reply with E2E_OK only."}, 200), "responses") // one synthetic ordinary request warms the actual scheduler
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT"), Password: os.Getenv("DEVIN_E2E_REDIS_PASSWORD")})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	cache := repository.NewSchedulerCache(rdb)
	old, err := cache.GetAccount(context.Background(), accountID)
	require.NoError(t, err)
	require.NotNil(t, old)
	require.False(t, old.IsModelSupported(publicModel), "the retained row must predate the new configured pair")
	return func() map[string]any {
		current, err := cache.GetAccount(context.Background(), accountID)
		require.NoError(t, err)
		require.NotNil(t, current)
		require.True(t, current.IsModelSupported(publicModel), "prove configured caller-route prerequisites before resuming the stale writer")
		require.True(t, current.UpdatedAt.After(old.UpdatedAt), "the authoritative row must have a later database version")
		err = cache.SetAccount(context.Background(), old)
		require.True(t, err == nil || errors.Is(err, service.ErrSchedulerBucketWriteFenced), "unexpected cache publication failure; details withheld")
		after, readErr := cache.GetAccount(context.Background(), accountID)
		require.NoError(t, readErr)
		require.NotNil(t, after)
		return map[string]any{"synthetic_only": true, "configured_route_proved_before_publication": true, "newer_row_version_proved": true, "stale_write_rejected": errors.Is(err, service.ErrSchedulerBucketWriteFenced), "configured_route_preserved": after.IsModelSupported(publicModel)}
	}
}
