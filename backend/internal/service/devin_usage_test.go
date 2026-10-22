//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type devinUsageRepo struct {
	AccountRepository
	accounts []*Account
}

func (r *devinUsageRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	for _, account := range r.accounts {
		if account.ID == id {
			return account, nil
		}
	}
	return nil, ErrAccountNotFound
}

func (r *devinUsageRepo) GetByIDs(context.Context, []int64) ([]*Account, error) {
	return r.accounts, nil
}

func TestDevinUsageSingleBatchCacheAndForce(t *testing.T) {
	var probes, mints atomic.Int32
	reset := time.Now().Add(time.Hour).Unix()
	upstream := &devinHTTP{handle: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == devin.MintPath {
			mints.Add(1)
			return devinReply(200, []byte(`{"sessionToken":"minted"}`)), nil
		}
		assert.Equal(t, devin.UserStatusPath, req.URL.Path)
		probes.Add(1)
		return devinReply(200, fmt.Appendf(nil, `{"userStatus":{"planStatus":{
			"dailyQuotaRemainingPercent":100,"weeklyQuotaRemainingPercent":0,
			"dailyQuotaResetAtUnix":"%d","weeklyQuotaResetAtUnix":"%d"}}}`, reset, reset+86400)), nil
	}}
	a := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "session"}}
	b := &Account{ID: 2, Platform: PlatformDevin, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "cog_test"}}
	repo := &devinUsageRepo{accounts: []*Account{a, b}}
	d := NewDevinService(repo, nil, upstream, &config.Config{}, nil)
	s := &AccountUsageService{accountRepo: repo, devinService: d}
	ctx := context.Background()
	usage, err := s.GetUsage(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, 0.0, usage.Daily.Utilization)
	require.Equal(t, 100.0, usage.SevenDay.Utilization)
	require.Equal(t, reset+86400, usage.SevenDay.ResetsAt.Unix())
	require.Greater(t, usage.Daily.RemainingSeconds, 0)
	require.NotNil(t, usage.UpdatedAt)
	require.Nil(t, usage.FiveHour, "daily quota must not be labeled as a five-hour window")
	_, err = s.GetUsage(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, int32(1), probes.Load())
	batch, failures, err := s.GetUsageBatch(ctx, []int64{1, 2}, true)
	require.NoError(t, err)
	require.Empty(t, failures)
	require.Len(t, batch, 2)
	require.Equal(t, int32(3), probes.Load())
	require.Equal(t, int32(1), mints.Load())
	a.Credentials["access_token"] = "replacement"
	_, err = s.GetUsage(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, int32(4), probes.Load(), "reauthorization must bypass the old credential's cached usage")
}

func TestDevinUsageRecoversAPIKeySession(t *testing.T) {
	var mints, probes atomic.Int32
	upstream := &devinHTTP{handle: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == devin.MintPath {
			return devinReply(200, fmt.Appendf(nil, `{"sessionToken":"token%d"}`, mints.Add(1))), nil
		}
		if probes.Add(1) == 1 {
			return devinReply(401, []byte(`{"code":"unauthenticated"}`)), nil
		}
		assert.Equal(t, "Bearer devin-session-token$token2", req.Header.Get("Authorization"))
		return devinReply(200, []byte(`{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":null,"weeklyQuotaRemainingPercent":50}}}`)), nil
	}}
	d := NewDevinService(nil, nil, upstream, &config.Config{}, nil)
	account := &Account{ID: 3, Credentials: map[string]any{"api_key": "cog_test"}}
	usage, err := d.Usage(context.Background(), account, false)
	require.NoError(t, err)
	require.Empty(t, usage.Error)
	require.Nil(t, usage.Daily)
	require.Equal(t, 50.0, usage.SevenDay.Utilization)
	require.Equal(t, int32(2), mints.Load())
	require.Equal(t, int32(2), probes.Load())
}

func TestDevinUsageCacheTracksClientIdentity(t *testing.T) {
	identity := devin.DefaultClientIdentity()
	devin.SetClientIdentityResolver(func(context.Context) devin.ClientIdentity { return identity })
	t.Cleanup(func() { devin.SetClientIdentityResolver(nil) })
	var probes atomic.Int32
	upstream := &devinHTTP{handle: func(req *http.Request) (*http.Response, error) {
		probes.Add(1)
		return devinReply(200, []byte(`{"userStatus":{}}`)), nil
	}}
	d := NewDevinService(nil, nil, upstream, &config.Config{}, nil)
	account := &Account{ID: 6, Credentials: map[string]any{"access_token": "session"}}
	ctx := context.Background()
	pinned := devin.WithClientIdentity(ctx)
	_, err := d.Usage(ctx, account, false)
	require.NoError(t, err)
	identity.Revision += ".next"
	_, err = d.Usage(pinned, account, false)
	require.NoError(t, err)
	require.Equal(t, int32(1), probes.Load(), "in-flight snapshots retain their existing usage cache")
	_, err = d.Usage(ctx, account, false)
	require.NoError(t, err)
	require.Equal(t, int32(2), probes.Load(), "new identity revisions must not reuse the old usage cache")
}

func TestDevinUsageErrorsAreSafeCachedAndRetryable(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		reauth bool
	}{{401, errorCodeUnauthenticated, true}, {403, errorCodeForbidden, false}, {429, errorCodeRateLimited, false}, {500, errorCodeNetworkError, false}, {200, errorCodeNetworkError, false}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			var calls atomic.Int32
			upstream := &devinHTTP{handle: func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return devinReply(tc.status, []byte(`{"message":"private diagnostic devin-session-token$secret"}`)), nil
			}}
			d := NewDevinService(nil, nil, upstream, &config.Config{}, nil)
			account := &Account{ID: 4, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "session"}}
			usage, err := d.Usage(context.Background(), account, false)
			require.NoError(t, err)
			require.Equal(t, tc.code, usage.ErrorCode)
			require.Equal(t, tc.reauth, usage.NeedsReauth)
			require.Nil(t, usage.SevenDay)
			raw, err := json.Marshal(usage)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private diagnostic")
			require.NotContains(t, string(raw), "secret")
			_, err = d.Usage(context.Background(), account, false)
			require.NoError(t, err)
			require.Equal(t, int32(1), calls.Load())
			_, err = d.Usage(context.Background(), account, true)
			require.NoError(t, err)
			require.Equal(t, int32(2), calls.Load())
			require.Equal(t, StatusActive, account.Status)
			require.True(t, account.Schedulable)
		})
	}
}

func TestDevinUsageSharedProbeSurvivesCallerCancellation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	upstream := &devinHTTP{handle: func(req *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-release:
			return devinReply(200, []byte(`{"userStatus":{}}`)), nil
		}
	}}
	d := NewDevinService(nil, nil, upstream, &config.Config{}, nil)
	account := &Account{ID: 5, Credentials: map[string]any{"access_token": "session"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := d.Usage(ctx, account, false); done <- err }()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	close(release)
	usage, err := d.Usage(context.Background(), account, false)
	require.NoError(t, err)
	require.Empty(t, usage.Error)
}
