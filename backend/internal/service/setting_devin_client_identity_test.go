//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/stretchr/testify/require"
)

func TestDevinClientIdentityHotConfigurationAndRollback(t *testing.T) {
	ctx := context.Background()
	repo := &authSourceDefaultsRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})
	require.Equal(t, devin.DefaultClientIdentity(), svc.GetDevinClientIdentity(ctx))
	remote := devin.DefaultClientIdentity()
	remote.Revision, remote.Chat.IDEVersion, remote.Chat.ExtensionVersion = "release-2", "4000.2.0", "4000.2.0"
	fetches := 0
	svc.devinIdentityFetch = func(context.Context, string) (devin.ClientIdentity, error) {
		fetches++
		return remote, nil
	}
	settings := DevinClientIdentitySettings{SourceURL: "https://example.com/devin.json", AutoSync: true}
	view, err := svc.UpdateDevinClientIdentitySettings(ctx, settings)
	require.NoError(t, err)
	require.Equal(t, "remote", view.Source)
	require.Equal(t, remote, svc.GetDevinClientIdentity(ctx))
	require.Equal(t, 1, fetches)
	storedSync := repo.values[SettingKeyDevinClientIdentitySynced]
	manual := remote
	manual.Revision, manual.Chat.IDEVersion, manual.Chat.ExtensionVersion = "rollback", "3000.6.2", "3000.6.2"
	settings.Manual = &manual
	view, err = svc.UpdateDevinClientIdentitySettings(ctx, settings)
	require.NoError(t, err)
	require.Equal(t, "manual", view.Source)
	require.Equal(t, manual, svc.GetDevinClientIdentity(ctx), "manual rollback must take effect without waiting for a cache TTL")
	require.Equal(t, storedSync, repo.values[SettingKeyDevinClientIdentitySynced], "a manual save must not overwrite the synced profile")
	// Another instance observes the stored manual pin once its short cache expires.
	other := NewSettingService(repo, &config.Config{})
	require.Equal(t, manual, other.GetDevinClientIdentity(ctx))
	settings.Manual = nil
	settings.AutoSync = false
	_, err = svc.UpdateDevinClientIdentitySettings(ctx, settings)
	require.NoError(t, err)
	require.Equal(t, remote, svc.GetDevinClientIdentity(ctx))
	other.devinIdentityCache.expiresAt = time.Time{}
	require.Equal(t, remote, other.GetDevinClientIdentity(ctx))
	restarted := NewSettingService(repo, &config.Config{})
	require.Equal(t, remote, restarted.GetDevinClientIdentity(ctx), "remote state must survive a restart")
	settings.SourceURL = ""
	_, err = svc.UpdateDevinClientIdentitySettings(ctx, settings)
	require.NoError(t, err)
	require.Equal(t, devin.DefaultClientIdentity(), svc.GetDevinClientIdentity(ctx))
}

func TestDevinClientIdentitySyncFailurePreservesLastGoodAndRetries(t *testing.T) {
	ctx := context.Background()
	repo := &authSourceDefaultsRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})
	profile := devin.DefaultClientIdentity()
	profile.Revision = "remote-good"
	var fetchErr error
	calls := 0
	svc.devinIdentityFetch = func(context.Context, string) (devin.ClientIdentity, error) {
		calls++
		return profile, fetchErr
	}
	_, err := svc.UpdateDevinClientIdentitySettings(ctx, DevinClientIdentitySettings{SourceURL: "https://example.com/profile.json", AutoSync: true})
	require.NoError(t, err)
	stored := repo.values[SettingKeyDevinClientIdentitySynced]
	fetchErr = errors.New("source unavailable")
	view, err := svc.GetDevinClientIdentitySettings(ctx, true)
	require.NoError(t, err)
	require.NotEmpty(t, view.SyncError)
	require.Equal(t, profile, view.Effective)
	require.Equal(t, stored, repo.values[SettingKeyDevinClientIdentitySynced])
	svc.devinIdentityCache.expiresAt = time.Time{}
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
	repo.values[SettingKeyDevinClientIdentitySynced] = string(b)
	svc.devinIdentityCache.expiresAt, svc.devinIdentityCache.nextSyncAt = time.Time{}, time.Time{}
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
	repo := &authSourceDefaultsRepoStub{values: map[string]string{}}
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
	_, err = svc.UpdateDevinClientIdentitySettings(ctx, DevinClientIdentitySettings{SourceURL: "https://example.com/a", AutoSync: true})
	require.NoError(t, err)
	svc.devinIdentityFetch = func(context.Context, string) (devin.ClientIdentity, error) {
		return devin.ClientIdentity{}, errors.New("offline")
	}
	view, err := svc.UpdateDevinClientIdentitySettings(ctx, DevinClientIdentitySettings{SourceURL: "https://example.com/b", AutoSync: true})
	require.NoError(t, err)
	require.Equal(t, "builtin", view.Source)
	require.Equal(t, devin.DefaultClientIdentity(), view.Effective)
	require.NotEmpty(t, view.SyncError, "changing sources must check the new source immediately")
}

func TestDevinIdentityChangeInvalidatesCredentialRuntimeCaches(t *testing.T) {
	ctx := context.Background()
	svc := NewSettingService(&authSourceDefaultsRepoStub{values: map[string]string{}}, &config.Config{})
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
