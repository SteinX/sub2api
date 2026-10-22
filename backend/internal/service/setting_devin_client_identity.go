package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

const (
	SettingKeyDevinClientIdentity       = "devin_client_identity"
	SettingKeyDevinClientIdentitySynced = "devin_client_identity_synced"
	devinIdentityCacheTTL               = 5 * time.Second
	devinIdentitySyncInterval           = time.Minute
)

type DevinClientIdentitySettings struct {
	Manual    *devin.ClientIdentity `json:"manual"`
	SourceURL string                `json:"source_url"`
	AutoSync  bool                  `json:"auto_sync"`
}

type DevinClientIdentityView struct {
	DevinClientIdentitySettings
	Effective devin.ClientIdentity `json:"effective"`
	Source    string               `json:"source"`
	SyncedAt  *time.Time           `json:"synced_at,omitempty"`
	SyncError string               `json:"sync_error,omitempty"`
}

type syncedDevinClientIdentity struct {
	SourceURL string               `json:"source_url"`
	Identity  devin.ClientIdentity `json:"identity"`
	SyncedAt  time.Time            `json:"synced_at"`
}

type cachedDevinClientIdentity struct {
	view       DevinClientIdentityView
	expiresAt  time.Time
	nextSyncAt time.Time
}

func (s *SettingService) validateDevinIdentitySource(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	options := urlvalidator.ValidationOptions{}
	if s.cfg != nil && s.cfg.Security.URLAllowlist.Enabled {
		options.RequireAllowlist = true
		options.AllowedHosts = s.cfg.Security.URLAllowlist.UpstreamHosts
	}
	normalized, err := urlvalidator.ValidateHTTPSURL(raw, options)
	if err != nil {
		return "", errors.New("Devin identity source must be an allowed public HTTPS URL")
	}
	u, err := url.Parse(normalized)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("Devin identity source must not contain credentials, a query or a fragment")
	}
	return normalized, nil
}

// GetDevinClientIdentity resolves one immutable profile. Settings are observed
// across instances within five seconds; remote documents are polled on demand,
// at most once a minute. Remote failure retains the persisted last good profile.
func (s *SettingService) GetDevinClientIdentity(ctx context.Context) devin.ClientIdentity {
	if s == nil || s.settingRepo == nil {
		return devin.DefaultClientIdentity()
	}
	s.devinIdentityMu.Lock()
	defer s.devinIdentityMu.Unlock()
	view, err := s.readDevinClientIdentityLocked(ctx, false)
	if err == nil {
		return view.Effective
	}
	if s.devinIdentityCache != nil {
		return s.devinIdentityCache.view.Effective
	}
	return devin.DefaultClientIdentity()
}

func (s *SettingService) GetDevinClientIdentitySettings(ctx context.Context, syncNow bool) (DevinClientIdentityView, error) {
	s.devinIdentityMu.Lock()
	defer s.devinIdentityMu.Unlock()
	return s.readDevinClientIdentityLocked(ctx, syncNow)
}

func (s *SettingService) UpdateDevinClientIdentitySettings(ctx context.Context, settings DevinClientIdentitySettings) (DevinClientIdentityView, error) {
	if settings.Manual != nil {
		if err := settings.Manual.Validate(); err != nil {
			return DevinClientIdentityView{}, infraerrors.BadRequest("INVALID_DEVIN_CLIENT_IDENTITY", err.Error())
		}
	}
	source, err := s.validateDevinIdentitySource(settings.SourceURL)
	if err != nil {
		return DevinClientIdentityView{}, infraerrors.BadRequest("INVALID_DEVIN_IDENTITY_SOURCE", err.Error())
	}
	settings.SourceURL = source
	if settings.AutoSync && source == "" {
		return DevinClientIdentityView{}, infraerrors.BadRequest("INVALID_DEVIN_IDENTITY_SOURCE", "Configure a source URL before enabling automatic sync")
	}
	b, _ := json.Marshal(settings)
	s.devinIdentityMu.Lock()
	defer s.devinIdentityMu.Unlock()
	if err := s.settingRepo.SetMultiple(ctx, map[string]string{SettingKeyDevinClientIdentity: string(b)}); err != nil {
		return DevinClientIdentityView{}, err
	}
	s.devinIdentityCache = nil
	return s.readDevinClientIdentityLocked(ctx, false)
}

func (s *SettingService) readDevinClientIdentityLocked(ctx context.Context, syncNow bool) (DevinClientIdentityView, error) {
	now := time.Now()
	previous := s.devinIdentityCache
	if !syncNow && previous != nil && now.Before(previous.expiresAt) {
		return previous.view, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	values, err := s.settingRepo.GetMultiple(ctx, []string{SettingKeyDevinClientIdentity, SettingKeyDevinClientIdentitySynced})
	if err != nil {
		return DevinClientIdentityView{}, err
	}
	view := DevinClientIdentityView{Effective: devin.DefaultClientIdentity(), Source: "builtin"}
	if raw := values[SettingKeyDevinClientIdentity]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &view.DevinClientIdentitySettings); err != nil {
			return view, errors.New("invalid stored Devin client identity settings")
		}
	}
	var synced syncedDevinClientIdentity
	if json.Unmarshal([]byte(values[SettingKeyDevinClientIdentitySynced]), &synced) == nil &&
		synced.SourceURL == view.SourceURL && synced.Identity.Validate() == nil {
		view.Effective, view.Source, view.SyncedAt = synced.Identity, "remote", &synced.SyncedAt
	}
	if view.Manual != nil {
		if err := view.Manual.Validate(); err != nil {
			return view, err
		}
		view.Effective, view.Source = *view.Manual, "manual"
	}
	nextSync := time.Time{}
	if view.SyncedAt != nil {
		nextSync = view.SyncedAt.Add(devinIdentitySyncInterval)
	}
	if previous != nil && previous.view.SourceURL == view.SourceURL {
		if previous.nextSyncAt.After(nextSync) {
			nextSync = previous.nextSyncAt
		}
		view.SyncError = previous.view.SyncError
	}
	if syncNow && view.SourceURL == "" {
		return view, infraerrors.BadRequest("INVALID_DEVIN_IDENTITY_SOURCE", "Configure and save a source URL before syncing")
	}
	if syncNow || view.AutoSync && view.Manual == nil && view.SourceURL != "" && !now.Before(nextSync) {
		nextSync = now.Add(devinIdentitySyncInterval)
		fetch := s.devinIdentityFetch
		if fetch == nil {
			fetch = s.fetchDevinClientIdentity
		}
		identity, fetchErr := fetch(ctx, view.SourceURL)
		if fetchErr == nil {
			fetchErr = identity.Validate()
		}
		if fetchErr == nil {
			state := syncedDevinClientIdentity{view.SourceURL, identity, time.Now().UTC()}
			b, _ := json.Marshal(state)
			fetchErr = s.settingRepo.SetMultiple(ctx, map[string]string{SettingKeyDevinClientIdentitySynced: string(b)})
			if fetchErr == nil {
				view.SyncedAt, view.SyncError = &state.SyncedAt, ""
				if view.Manual == nil {
					view.Effective, view.Source = identity, "remote"
				}
			}
		}
		if fetchErr != nil {
			view.SyncError = "Devin identity sync failed; the last valid profile remains active"
		}
	}
	s.devinIdentityCache = &cachedDevinClientIdentity{view, now.Add(devinIdentityCacheTTL), nextSync}
	return view, nil
}

func (s *SettingService) fetchDevinClientIdentity(ctx context.Context, source string) (devin.ClientIdentity, error) {
	var empty devin.ClientIdentity
	source, err := s.validateDevinIdentitySource(source)
	if err != nil || source == "" {
		return empty, errors.New("invalid Devin identity source")
	}
	proxy := firstNonEmpty(os.Getenv("HTTPS_PROXY"), os.Getenv("HTTP_PROXY"))
	shared, err := httpclient.GetClient(httpclient.Options{Timeout: 5 * time.Second, ProxyURL: proxy, ValidateResolvedIP: true})
	if err != nil {
		return empty, err
	}
	client := *shared
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many Devin identity redirects")
		}
		_, err := s.validateDevinIdentitySource(req.URL.String())
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return empty, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return empty, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return empty, fmt.Errorf("Devin identity source returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, devin.MaxClientIdentitySize+1))
	if err != nil {
		return empty, err
	}
	return devin.ParseClientIdentity(b)
}
