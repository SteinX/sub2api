package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sync/singleflight"
)

type devinRuntime struct {
	token                   string
	models                  []*pb.ClientModelConfig
	tokenUntil, modelsUntil time.Time
}

// DevinService owns credential-scoped runtime caches; account scheduling stays in the gateway.
type DevinService struct {
	repo      AccountRepository
	proxyRepo ProxyRepository
	upstream  HTTPUpstream
	cfg       *config.Config
	store     devin.SessionStore
	mu        sync.Mutex
	runtime   map[string]devinRuntime
	flights   singleflight.Group
}

func NewDevinService(repo AccountRepository, proxyRepo ProxyRepository, upstream HTTPUpstream, cfg *config.Config, store devin.SessionStore) *DevinService {
	return &DevinService{repo: repo, proxyRepo: proxyRepo, upstream: upstream, cfg: cfg, store: store, runtime: map[string]devinRuntime{}}
}

func (a *Account) devinModelIDs() []string {
	switch ids := a.Extra["devin_models"].(type) {
	case []string:
		return ids
	case []any:
		out := make([]string, 0, len(ids))
		for _, value := range ids {
			if id, ok := value.(string); ok && id != "" {
				out = append(out, id)
			}
		}
		return out
	}
	return nil
}

// Both scheduler implementations call the ordinary account model gate. Apply
// the account's credential catalog after resolving any explicit public alias.
func (a *Account) supportsDevinModel(requested string) bool {
	if mapping := a.GetModelMapping(); len(mapping) > 0 && !mappingSupportsRequestedModel(mapping, requested) {
		return false
	}
	return slices.Contains(a.devinModelIDs(), a.GetMappedModel(requested))
}

func (s *DevinService) runtimeKey(a *Account) string {
	proxy := ""
	if a.ProxyID != nil {
		proxy = strconv.FormatInt(*a.ProxyID, 10)
	}
	return devin.CredentialFingerprint(strconv.FormatInt(a.ID, 10), a.GetCredential("api_key"), a.GetCredential("access_token"), a.GetCredential("base_url"), proxy)
}
func (s *DevinService) ValidateURL(raw string) (string, error) {
	return (&AccountTestService{cfg: s.cfg}).validateUpstreamBaseURL(raw)
}
func (s *DevinService) Client(ctx context.Context, a *Account) (*devin.Client, error) {
	base := a.GetCredential("base_url")
	if base == "" {
		base = devin.DefaultBaseURL
	}
	base, err := s.ValidateURL(base)
	if err != nil {
		return nil, err
	}
	proxy := upstreamModelsProxyURL(a)
	if a.ProxyID != nil && a.Proxy == nil {
		if s.proxyRepo == nil {
			return nil, errors.New("devin proxy is unavailable")
		}
		p, err := s.proxyRepo.GetByID(ctx, *a.ProxyID)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, errors.New("devin proxy is unavailable")
		}
		proxy = p.URL()
	}
	baseURL, _ := url.Parse(base)
	return &devin.Client{BaseURL: base, ValidateURL: s.ValidateURL, Do: func(req *http.Request) (*http.Response, error) {
		requestCtx := WithHTTPUpstreamRedirectsDisabled(req.Context())
		if req.URL.Host != baseURL.Host {
			requestCtx = WithHTTPUpstreamPublicHostsOnly(requestCtx)
		}
		return s.upstream.Do(req.WithContext(requestCtx), proxy, a.ID, a.Concurrency)
	}}, nil
}
func (s *DevinService) Token(ctx context.Context, a *Account, force bool) (string, error) {
	failed := ""
	if force {
		s.mu.Lock()
		failed = s.runtime[s.runtimeKey(a)].token
		s.mu.Unlock()
	}
	return s.token(ctx, a, force, failed)
}

func (s *DevinService) token(ctx context.Context, a *Account, force bool, failed string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key := s.runtimeKey(a)
	if a.GetCredential("api_key") == "" {
		token := a.GetCredential("access_token")
		if token == "" {
			return "", errors.New("devin requires reauthorization")
		}
		if force {
			return "", &devin.Error{Code: "unauthenticated", Status: 401}
		}
		return devin.NormalizeToken(token), nil
	}
	s.mu.Lock()
	entry := s.runtime[key]
	if entry.token != "" && time.Now().Before(entry.tokenUntil) && (!force || failed != "" && entry.token != failed) {
		s.mu.Unlock()
		return entry.token, nil
	}
	s.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		pending := s.flights.DoChan("token:"+key, func() (any, error) {
			sharedCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			return s.loadOrMintToken(sharedCtx, a, key, force, failed)
		})
		var result singleflight.Result
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case result = <-pending:
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if result.Err != nil {
			return "", result.Err
		}
		token, ok := result.Val.(string)
		if !ok {
			return "", errors.New("invalid Devin token cache result")
		}
		if !force || failed == "" || token != failed {
			return token, nil
		}
		// A normal lookup may already have been in flight when recovery began.
		// Its result must satisfy this caller's rejection too.
	}
}

func (s *DevinService) loadOrMintToken(ctx context.Context, a *Account, key string, force bool, failed string) (string, error) {
	s.mu.Lock()
	e := s.runtime[key]
	if e.token != "" && time.Now().Before(e.tokenUntil) && (!force || failed != "" && e.token != failed) {
		s.mu.Unlock()
		return e.token, nil
	}
	if force {
		e.token = ""
		e.tokenUntil = time.Time{}
		s.runtime[key] = e
	}
	s.mu.Unlock()
	client, err := s.Client(ctx, a)
	if err != nil {
		return "", err
	}
	mintCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.store != nil {
		cacheKey := "devin:token:" + key
		cachedToken := func() (string, error) {
			cached, cacheErr := s.store.Get(mintCtx, cacheKey)
			if errors.Is(cacheErr, devin.ErrSessionNotFound) {
				return "", nil
			}
			if cacheErr != nil {
				return "", cacheErr
			}
			if len(cached) == 0 || force && (failed == "" || string(cached) == failed) {
				return "", nil
			}
			s.mu.Lock()
			e := s.runtime[key]
			e.token = string(cached)
			e.tokenUntil = time.Now().Add(5 * time.Minute)
			s.runtime[key] = e
			s.mu.Unlock()
			return string(cached), nil
		}
		for {
			if cached, _ := cachedToken(); cached != "" {
				return cached, nil
			}
			unlock, ok, lockErr := s.store.Acquire(mintCtx, cacheKey+":lock", 40*time.Second)
			if lockErr != nil {
				return "", lockErr
			}
			if ok {
				defer unlock()
				// Another instance may have published a token between Get and Acquire.
				cached, cacheErr := cachedToken()
				if cacheErr != nil {
					return "", cacheErr
				}
				if cached != "" {
					return cached, nil
				}
				break
			}
			select {
			case <-mintCtx.Done():
				return "", mintCtx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	token, err := client.Mint(mintCtx, a.GetCredential("api_key"))
	if err != nil {
		return "", err
	}
	if force && failed != "" && token == failed {
		return "", &devin.Error{Code: "unauthenticated", Status: 401}
	}
	if s.store != nil {
		if err = s.store.Set(mintCtx, "devin:token:"+key, []byte(token), 30*time.Minute); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runtime) > 2048 {
		for k, v := range s.runtime {
			if time.Now().After(v.tokenUntil) && time.Now().After(v.modelsUntil) {
				delete(s.runtime, k)
			}
		}
	}
	e = s.runtime[key]
	e.token = token
	e.tokenUntil = time.Now().Add(30 * time.Minute)
	s.runtime[key] = e
	return token, nil
}

// RecoverToken reuses a credential another request has already recovered.
func (s *DevinService) RecoverToken(ctx context.Context, a *Account, failed string) (string, error) {
	return s.token(ctx, a, true, devin.NormalizeToken(failed))
}

func (s *DevinService) Models(ctx context.Context, a *Account) ([]*pb.ClientModelConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := s.runtimeKey(a)
	s.mu.Lock()
	e := s.runtime[key]
	s.mu.Unlock()
	if e.models != nil && time.Now().Before(e.modelsUntil) {
		return e.models, nil
	}
	pending := s.flights.DoChan("models:"+key, func() (any, error) {
		sharedCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		token, err := s.Token(sharedCtx, a, false)
		if err != nil {
			return nil, err
		}
		client, err := s.Client(sharedCtx, a)
		if err != nil {
			return nil, err
		}
		models, err := client.Models(sharedCtx, token)
		if devin.IsUnauthenticated(err) && a.GetCredential("api_key") != "" {
			token, err = s.RecoverToken(sharedCtx, a, token)
			if err == nil {
				models, err = client.Models(sharedCtx, token)
			}
		}
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		e := s.runtime[key]
		e.models = models
		e.modelsUntil = time.Now().Add(2 * time.Minute)
		s.runtime[key] = e
		s.mu.Unlock()
		return models, nil
	})
	var result singleflight.Result
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result = <-pending:
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Err != nil {
		return nil, result.Err
	}
	models, ok := result.Val.([]*pb.ClientModelConfig)
	if !ok {
		return nil, errors.New("invalid Devin model cache result")
	}
	return models, nil
}

func DevinModelsBody(models []*pb.ClientModelConfig) ([]byte, error) {
	rows := make([]map[string]any, 0, len(models))
	for _, m := range models {
		f := m.GetModelInfo().GetModelFeatures()
		rows = append(rows, map[string]any{"id": devin.ModelID(m), "object": "model", "owned_by": "devin", "name": m.Label, "context_window": m.MaxTokens, "max_output_tokens": m.GetModelInfo().GetMaxOutputTokens(), "supports_images": m.SupportsImages || f.GetSupportsImages(), "supports_tool_calls": f.GetSupportsToolCalls(), "supports_thinking": f.GetSupportsThinking(), "is_model_router": m.GetModelInfo().GetIsModelRouter()})
	}
	return json.Marshal(map[string]any{"object": "list", "data": rows})
}

// ParseDevinCredential accepts a key, token or explicitly supplied CLI file contents.
func ParseDevinCredential(input string) (map[string]any, error) {
	input = strings.TrimSpace(input)
	if input == "" || len(input) > 1<<20 {
		return nil, fmt.Errorf("provide a Devin API key, session token or CLI credential file")
	}
	value := input
	if strings.HasPrefix(input, "{") || strings.Contains(input, "=") && !strings.HasPrefix(input, "devin-session-token$") {
		var data map[string]any
		if strings.HasPrefix(input, "{") {
			if json.Unmarshal([]byte(input), &data) != nil {
				return nil, fmt.Errorf("invalid credential JSON")
			}
		} else if toml.Unmarshal([]byte(input), &data) != nil {
			return nil, fmt.Errorf("invalid CLI credential TOML")
		}
		value = ""
		for _, k := range []string{"api_key", "windsurf_api_key", "access_token", "token"} {
			if v, ok := data[k].(string); ok && strings.TrimSpace(v) != "" {
				value = strings.TrimSpace(v)
				break
			}
		}
		if value == "" {
			return nil, fmt.Errorf("credential file has no supported token field")
		}
	}
	if strings.ContainsAny(value, "\r\n\t ") {
		return nil, fmt.Errorf("invalid credential format")
	}
	if strings.HasPrefix(value, "cog_") {
		return map[string]any{"api_key": value, "auth_mode": "api_key"}, nil
	}
	return map[string]any{"access_token": devin.NormalizeToken(value), "auth_mode": "cli"}, nil
}
