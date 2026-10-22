//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/stretchr/testify/require"
)

type devinIdentityRepo struct {
	SettingRepository
	mu     sync.Mutex
	values map[string]string
}

func (r *devinIdentityRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	values := map[string]string{}
	for _, key := range keys {
		values[key] = r.values[key]
	}
	return values, nil
}

func (r *devinIdentityRepo) SetMultiple(_ context.Context, values map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = map[string]string{}
	}
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func (r *devinIdentityRepo) value(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.values[key]
}

func waitDevinIdentityIdle(t *testing.T, svc *SettingService) {
	t.Helper()
	require.Eventually(t, func() bool { return !svc.devinIdentitySyncing.Load() && !svc.devinIdentityRefreshing.Load() }, time.Second, time.Millisecond)
}

func expireDevinIdentityCache(svc *SettingService) {
	cached := *svc.devinIdentityCache.Load()
	cached.expiresAt, cached.nextSyncAt = time.Time{}, time.Time{}
	svc.devinIdentityCache.Store(&cached)
}

func TestDevinClientIdentityHotConfigurationAndRollback(t *testing.T) {
	ctx := context.Background()
	repo := &devinIdentityRepo{}
	svc := NewSettingService(repo, &config.Config{})
	require.Equal(t, devin.DefaultClientIdentity(), svc.GetDevinClientIdentity(ctx))
	remote := devin.DefaultClientIdentity()
	remote.Revision, remote.Chat.IDEVersion, remote.Chat.ExtensionVersion = "release-2", "4000.2.0", "4000.2.0"
	fetches := 0
	svc.devinIdentityFetch = func(context.Context, string) (devin.ClientIdentity, error) {
		fetches++
		return remote, nil
	}
	settings := DevinClientIdentitySettings{SourceURL: "https://example.com/devin.json"}
	_, err := svc.UpdateDevinClientIdentitySettings(ctx, settings)
	require.NoError(t, err)
	view, err := svc.GetDevinClientIdentitySettings(ctx, true)
	require.NoError(t, err)
	require.Equal(t, "remote", view.Source)
	require.Equal(t, remote, svc.GetDevinClientIdentity(ctx))
	require.Equal(t, 1, fetches)
	storedSync := repo.value(SettingKeyDevinClientIdentitySynced)
	manual := remote
	manual.Revision, manual.Chat.IDEVersion, manual.Chat.ExtensionVersion = "rollback", "3000.6.2", "3000.6.2"
	settings.Manual = &manual
	view, err = svc.UpdateDevinClientIdentitySettings(ctx, settings)
	require.NoError(t, err)
	require.Equal(t, "manual", view.Source)
	require.Equal(t, manual, svc.GetDevinClientIdentity(ctx), "manual rollback must take effect without waiting for a cache TTL")
	require.Equal(t, storedSync, repo.value(SettingKeyDevinClientIdentitySynced), "a manual save must not overwrite the synced profile")
	// Another instance observes the stored manual pin once its short cache expires.
	other := NewSettingService(repo, &config.Config{})
	require.Equal(t, manual, other.GetDevinClientIdentity(ctx))
	settings.Manual = nil
	settings.AutoSync = false
	_, err = svc.UpdateDevinClientIdentitySettings(ctx, settings)
	require.NoError(t, err)
	require.Equal(t, remote, svc.GetDevinClientIdentity(ctx))
	expireDevinIdentityCache(other)
	view, err = other.GetDevinClientIdentitySettings(ctx, false)
	require.NoError(t, err)
	require.Equal(t, remote, view.Effective)
	restarted := NewSettingService(repo, &config.Config{})
	require.Equal(t, remote, restarted.GetDevinClientIdentity(ctx), "remote state must survive a restart")
	settings.SourceURL = ""
	_, err = svc.UpdateDevinClientIdentitySettings(ctx, settings)
	require.NoError(t, err)
	require.Equal(t, devin.DefaultClientIdentity(), svc.GetDevinClientIdentity(ctx))
}

func TestDevinClientIdentitySyncFailurePreservesLastGoodAndRetries(t *testing.T) {
	ctx := context.Background()
	repo := &devinIdentityRepo{}
	svc := NewSettingService(repo, &config.Config{})
	profile := devin.DefaultClientIdentity()
	profile.Revision = "remote-good"
	var fetchErr error
	calls := 0
	svc.devinIdentityFetch = func(context.Context, string) (devin.ClientIdentity, error) {
		calls++
		return profile, fetchErr
	}
	settings := DevinClientIdentitySettings{SourceURL: "https://example.com/profile.json"}
	_, err := svc.UpdateDevinClientIdentitySettings(ctx, settings)
	require.NoError(t, err)
	_, err = svc.GetDevinClientIdentitySettings(ctx, true)
	require.NoError(t, err)
	stored := repo.value(SettingKeyDevinClientIdentitySynced)
	fetchErr = errors.New("source unavailable")
	view, err := svc.GetDevinClientIdentitySettings(ctx, true)
	require.NoError(t, err)
	require.NotEmpty(t, view.SyncError)
	require.Equal(t, profile, view.Effective)
	require.Equal(t, stored, repo.value(SettingKeyDevinClientIdentitySynced))
	settings.AutoSync = true
	_, err = svc.UpdateDevinClientIdentitySettings(ctx, settings)
	require.NoError(t, err)
	require.Equal(t, profile, svc.GetDevinClientIdentity(ctx))
	require.Equal(t, 2, calls, "failed automatic sync must be throttled too")
	fetchErr = nil
	profile.Revision = "remote-new"
	// The next incoming request picks up a newly released document when due.
	var state syncedDevinClientIdentity
	require.NoError(t, json.Unmarshal([]byte(stored), &state))
	state.SyncedAt = time.Now().Add(-2 * time.Minute)
	b, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, repo.SetMultiple(ctx, map[string]string{SettingKeyDevinClientIdentitySynced: string(b)}))
	expireDevinIdentityCache(svc)
	_ = svc.GetDevinClientIdentity(ctx)
	require.Eventually(t, func() bool { return svc.GetDevinClientIdentity(ctx).Revision == "remote-new" }, time.Second, time.Millisecond)
	waitDevinIdentityIdle(t, svc)
	view, err = svc.GetDevinClientIdentitySettings(ctx, false)
	require.NoError(t, err)
	require.Empty(t, view.SyncError)
	require.Equal(t, "remote-new", view.Effective.Revision)
	profile.Chat.IDEVersion = "invalid"
	view, err = svc.GetDevinClientIdentitySettings(ctx, true)
	require.NoError(t, err)
	require.NotEmpty(t, view.SyncError)
	require.Equal(t, "remote-new", view.Effective.Revision)
	require.NotEqual(t, "invalid", view.Effective.Chat.IDEVersion)
}

func TestDevinClientIdentitySourceValidationAndIsolation(t *testing.T) {
	ctx := context.Background()
	repo := &devinIdentityRepo{}
	svc := NewSettingService(repo, &config.Config{})
	for _, source := range []string{"http://example.com/profile.json", "https://localhost/profile.json", "https://169.254.169.254/profile", "https://user:secret@example.com/profile", "https://example.com/profile?token=secret"} {
		_, err := svc.UpdateDevinClientIdentitySettings(ctx, DevinClientIdentitySettings{SourceURL: source})
		require.Error(t, err, source)
	}
	_, err := svc.UpdateDevinClientIdentitySettings(ctx, DevinClientIdentitySettings{AutoSync: true})
	require.Error(t, err)
	invalid := devin.DefaultClientIdentity()
	invalid.Chat.IDEVersion = "latest"
	_, err = svc.UpdateDevinClientIdentitySettings(ctx, DevinClientIdentitySettings{Manual: &invalid})
	require.Error(t, err)
	_, err = svc.GetDevinClientIdentitySettings(ctx, true)
	require.Error(t, err)
	// A successful profile from source A must never be used for source B.
	profile := devin.DefaultClientIdentity()
	profile.Revision = "source-A"
	svc.devinIdentityFetch = func(context.Context, string) (devin.ClientIdentity, error) { return profile, nil }
	_, err = svc.UpdateDevinClientIdentitySettings(ctx, DevinClientIdentitySettings{SourceURL: "https://example.com/a"})
	require.NoError(t, err)
	_, err = svc.GetDevinClientIdentitySettings(ctx, true)
	require.NoError(t, err)
	svc.devinIdentityFetch = func(context.Context, string) (devin.ClientIdentity, error) {
		return devin.ClientIdentity{}, errors.New("offline")
	}
	_, err = svc.UpdateDevinClientIdentitySettings(ctx, DevinClientIdentitySettings{SourceURL: "https://example.com/b"})
	require.NoError(t, err)
	view, err := svc.GetDevinClientIdentitySettings(ctx, true)
	require.NoError(t, err)
	require.Equal(t, "builtin", view.Source)
	require.Equal(t, devin.DefaultClientIdentity(), view.Effective)
	require.NotEmpty(t, view.SyncError, "changing sources must check the new source immediately")
}

func TestDevinIdentityChangeInvalidatesCredentialRuntimeCaches(t *testing.T) {
	ctx := context.Background()
	svc := NewSettingService(&devinIdentityRepo{}, &config.Config{})
	devin.SetClientIdentityResolver(svc.GetDevinClientIdentity)
	t.Cleanup(func() { devin.SetClientIdentityResolver(nil) })
	d := NewDevinService(nil, nil, nil, &config.Config{}, nil)
	a := &Account{ID: 1, Credentials: map[string]any{"access_token": "session"}}
	before := d.runtimeKey(ctx, a)
	pinnedCtx := devin.WithClientIdentity(ctx)
	client, err := d.Client(ctx, a)
	require.NoError(t, err)
	profile := devin.DefaultClientIdentity()
	profile.Chat.IDEVersion, profile.Chat.ExtensionVersion = "4000.3.0", "4000.3.0"
	_, err = svc.UpdateDevinClientIdentitySettings(ctx, DevinClientIdentitySettings{Manual: &profile})
	require.NoError(t, err)
	require.NotEqual(t, before, d.runtimeKey(ctx, a))
	require.Equal(t, before, d.runtimeKey(pinnedCtx, a), "all lookups for an in-flight request must retain the same profile")
	require.Equal(t, devin.DefaultClientIdentity(), *client.Identity, "an in-flight client's snapshot must remain unchanged")
	updated, err := d.Client(ctx, a)
	require.NoError(t, err)
	require.Equal(t, profile, *updated.Identity)
}

func TestDevinIdentitySlowSyncDoesNotBlockRequestsOrConfiguration(t *testing.T) {
	for _, mode := range []string{"manual rollback", "source change"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			repo := &devinIdentityRepo{}
			svc := NewSettingService(repo, &config.Config{})
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var calls atomic.Int32
			remote := devin.DefaultClientIdentity()
			remote.Revision = "pending-remote"
			svc.devinIdentityFetch = func(ctx context.Context, _ string) (devin.ClientIdentity, error) {
				if calls.Add(1) == 1 {
					close(started)
				}
				select {
				case <-release:
					return remote, nil
				case <-ctx.Done():
					return devin.ClientIdentity{}, ctx.Err()
				}
			}
			settings := DevinClientIdentitySettings{SourceURL: "https://example.com/a", AutoSync: true}
			saved := make(chan error, 1)
			go func() { _, err := svc.UpdateDevinClientIdentitySettings(ctx, settings); saved <- err }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("automatic sync did not start")
			}
			select {
			case err := <-saved:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("saving configuration blocked on the remote source")
			}
			reads := make(chan devin.ClientIdentity, 16)
			for range 16 {
				go func() { reads <- svc.GetDevinClientIdentity(ctx) }()
			}
			for range 16 {
				select {
				case profile := <-reads:
					require.Equal(t, devin.DefaultClientIdentity(), profile)
				case <-time.After(time.Second):
					t.Fatal("a concurrent request waited for remote I/O")
				}
			}
			fixed := devin.DefaultClientIdentity()
			fixed.Revision = "manual-rollback"
			if mode == "manual rollback" {
				settings.Manual = &fixed
			} else {
				settings.SourceURL, settings.AutoSync = "https://example.com/b", false
			}
			go func() { _, err := svc.UpdateDevinClientIdentitySettings(ctx, settings); saved <- err }()
			select {
			case err := <-saved:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("configuration change waited for remote I/O")
			}
			require.Equal(t, int32(1), calls.Load(), "concurrent requests must share one automatic poll")
			unblock()
			waitDevinIdentityIdle(t, svc)
			view, err := svc.GetDevinClientIdentitySettings(ctx, false)
			require.NoError(t, err)
			require.Empty(t, repo.value(SettingKeyDevinClientIdentitySynced), "an obsolete sync result must not be persisted")
			if mode == "manual rollback" {
				require.Equal(t, fixed, view.Effective)
			} else {
				require.Equal(t, "https://example.com/b", view.SourceURL)
				require.Equal(t, "builtin", view.Source)
			}
		})
	}
}
