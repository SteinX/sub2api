package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/oauth"
)

const devinAuthTTL = 10 * time.Minute

const devinAuthOperationTimeout = 75 * time.Second

func devinAuthStoreUnavailable() error {
	return infraerrors.ServiceUnavailable("DEVIN_AUTH_STORE_UNAVAILABLE", "devin authorization store is temporarily unavailable")
}

// DevinAuthSession is an internal Redis record, never an HTTP response.
type DevinAuthSession struct {
	ID              string                 `json:"id"`
	Owner           int64                  `json:"owner"`
	Status          string                 `json:"status"`
	State           string                 `json:"state"`
	Verifier        string                 `json:"verifier,omitempty"`
	TicketHash      string                 `json:"ticket_hash,omitempty"`
	Credentials     map[string]any         `json:"credentials,omitempty"`
	ProxyID         *int64                 `json:"proxy_id,omitempty"`
	TargetAccountID int64                  `json:"target_account_id,omitempty"`
	Models          []string               `json:"models,omitempty"`
	Catalog         *devin.CatalogSnapshot `json:"catalog,omitempty"`
	Error           string                 `json:"error,omitempty"`
	AccountID       int64                  `json:"account_id,omitempty"`
	AutoRecover     bool                   `json:"auto_recover,omitempty"`
	ExpiresAt       int64                  `json:"expires_at"`
}
type DevinAuthView struct {
	ID            string                 `json:"id"`
	Status        string                 `json:"status"`
	AuthURL       string                 `json:"auth_url,omitempty"`
	PairingTicket string                 `json:"pairing_ticket,omitempty"`
	Models        []string               `json:"models"`
	ModelRoutes   []devin.ModelRoutePlan `json:"model_routes,omitempty"`
	AutoRecover   bool                   `json:"auto_recover"`
	Error         string                 `json:"error,omitempty"`
	AccountID     int64                  `json:"account_id,omitempty"`
	ExpiresAt     int64                  `json:"expires_at"`
}

func DevinAuthSummary(session *DevinAuthSession) DevinAuthView {
	key, _ := session.Credentials["api_key"].(string)
	view := DevinAuthView{ID: session.ID, Status: session.Status, Models: session.Models, AutoRecover: key != "" || session.AutoRecover, Error: session.Error, AccountID: session.AccountID, ExpiresAt: session.ExpiresAt}
	if session.Catalog != nil {
		view.Models = devinEffectiveModelIDs(session.Catalog.Models, nil)
		view.ModelRoutes = devinEffectiveModelRoutes(session.Catalog.Models, nil)
	}
	return view
}
func devinSessionKey(id string) string { return "devin:auth:" + id }

// Reserve negative transport IDs for temporary authorization sessions so they
// never share a pool with persisted accounts or other administrators' sessions.
func (v *DevinAuthSession) transportID() int64 {
	hash := sha256.Sum256([]byte("devin:auth:transport:" + v.ID))
	return -int64(binary.BigEndian.Uint64(hash[:8])>>1) - 1
}

func (s *DevinService) SaveAuth(ctx context.Context, session *DevinAuthSession) error {
	if s.store == nil {
		return devinAuthStoreUnavailable()
	}
	ttl := time.Until(time.Unix(session.ExpiresAt, 0))
	if ttl <= 0 {
		return errors.New("devin authorization expired")
	}
	b, err := json.Marshal(session)
	if err != nil {
		return err
	}
	if err = s.store.Set(ctx, devinSessionKey(session.ID), b, ttl); err != nil {
		return devinAuthStoreUnavailable()
	}
	return nil
}
func (s *DevinService) LoadAuth(ctx context.Context, id string, owner int64) (*DevinAuthSession, error) {
	if s.store == nil {
		return nil, devinAuthStoreUnavailable()
	}
	b, err := s.store.Get(ctx, devinSessionKey(id))
	if errors.Is(err, devin.ErrSessionNotFound) {
		return nil, infraerrors.NotFound("DEVIN_AUTH_SESSION_NOT_FOUND", "devin authorization expired or unavailable")
	}
	if err != nil {
		return nil, devinAuthStoreUnavailable()
	}
	var v DevinAuthSession
	if json.Unmarshal(b, &v) != nil {
		return nil, errors.New("invalid authorization session")
	}
	if owner != -1 && owner != v.Owner {
		return nil, infraerrors.Forbidden("DEVIN_AUTH_SESSION_OWNER", "authorization session belongs to another administrator")
	}
	return &v, nil
}
func (s *DevinService) AuthLock(ctx context.Context, id string) (func(), error) {
	if s.store == nil {
		return nil, devinAuthStoreUnavailable()
	}
	unlock, ok, err := s.store.Acquire(ctx, devinSessionKey(id)+":lock", 90*time.Second)
	if err != nil {
		return nil, devinAuthStoreUnavailable()
	}
	if !ok {
		return nil, infraerrors.Conflict("DEVIN_AUTH_BUSY", "authorization operation is already in progress")
	}
	return unlock, nil
}

// A disconnected caller must not leave a persisted exchange/validation active.
func (s *DevinService) failAuth(ctx context.Context, v *DevinAuthSession, message string) error {
	v.Status = "failed"
	v.Error = message
	v.Credentials = nil
	v.Verifier = ""
	v.TicketHash = ""
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.SaveAuth(cleanup, v)
}

func (s *DevinService) recoverInterruptedAuth(ctx context.Context, v *DevinAuthSession) error {
	if v.Status != "exchanging" && v.Status != "validating" {
		return nil
	}
	return s.failAuth(ctx, v, "Devin authorization was interrupted; start a new connection")
}

// AuthStatus recovers orphaned intermediate records once no worker holds their
// lock. Authorization codes are never replayed after an uncertain exchange.
func (s *DevinService) AuthStatus(ctx context.Context, id string, owner int64) (*DevinAuthSession, error) {
	v, err := s.LoadAuth(ctx, id, owner)
	if err != nil || v.Status != "exchanging" && v.Status != "validating" {
		return v, err
	}
	unlock, err := s.AuthLock(ctx, id)
	if infraerrors.IsConflict(err) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	v, err = s.LoadAuth(ctx, id, owner)
	if err != nil {
		return nil, err
	}
	if err = s.recoverInterruptedAuth(ctx, v); err != nil {
		return nil, err
	}
	return v, nil
}
func (s *DevinService) StartAuth(ctx context.Context, owner int64, proxyID *int64, target int64) (DevinAuthView, error) {
	id, err := oauth.GenerateSessionID()
	if err != nil {
		return DevinAuthView{}, err
	}
	state, err := oauth.GenerateState()
	if err != nil {
		return DevinAuthView{}, err
	}
	verifier, err := oauth.GenerateCodeVerifier()
	if err != nil {
		return DevinAuthView{}, err
	}
	ticket, err := oauth.GenerateState()
	if err != nil {
		return DevinAuthView{}, err
	}
	v := &DevinAuthSession{ID: id, Owner: owner, Status: "waiting", State: state, Verifier: verifier, TicketHash: devin.CredentialFingerprint(ticket), ProxyID: proxyID, TargetAccountID: target, ExpiresAt: time.Now().Add(devinAuthTTL).Unix()}
	if err = s.SaveAuth(ctx, v); err != nil {
		return DevinAuthView{}, err
	}
	q := url.Values{"response_type": {"code"}, "redirect_uri": {devin.CallbackURI}, "code_challenge": {oauth.GenerateCodeChallenge(verifier)}, "code_challenge_method": {"S256"}, "state": {state}, "prompt": {"select_account"}}
	out := DevinAuthSummary(v)
	out.AuthURL = "https://app.devin.ai/auth/cli/continue?" + q.Encode()
	out.PairingTicket = ticket
	return out, nil
}
func (s *DevinService) ValidateAuth(ctx context.Context, v *DevinAuthSession) error {
	ctx = devin.WithClientIdentity(ctx)
	v.AutoRecover = v.Credentials["api_key"] != nil
	a := &Account{ID: v.transportID(), Platform: PlatformDevin, Type: AccountTypeOAuth, Credentials: v.Credentials, ProxyID: v.ProxyID, Concurrency: 1}
	if v.TargetAccountID != 0 {
		if s.repo == nil {
			return errors.New("devin account repository is unavailable")
		}
		target, err := s.repo.GetByID(ctx, v.TargetAccountID)
		if err != nil {
			return err
		}
		if target == nil || target.Platform != PlatformDevin {
			return errors.New("target account is not a Devin account")
		}
		// Reauthorization retains the target's endpoint and proxy at commit time.
		// Validate the new identity through that same configuration.
		a.Credentials = MergeCredentials(v.Credentials, map[string]any{"base_url": target.GetCredential("base_url")})
		a.ProxyID, a.Proxy = target.ProxyID, target.Proxy
	}
	models, err := s.Models(ctx, a)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return errors.New("devin account returned no available models")
	}
	v.Models = make([]string, 0, len(models))
	v.Catalog = devin.NewCatalogSnapshot(models, devin.ResolveClientIdentity(ctx).Revision)
	for _, m := range models {
		v.Models = append(v.Models, devin.ModelID(m))
	}
	v.Status = "ready"
	v.Error = ""
	v.Verifier = ""
	// The ticket permits idempotent outcome reads until this short-lived session expires.
	return s.SaveAuth(ctx, v)
}
func (s *DevinService) ImportAuth(ctx context.Context, owner int64, content string, proxyID *int64, target int64) (DevinAuthView, error) {
	creds, err := ParseDevinCredential(content)
	if err != nil {
		return DevinAuthView{}, err
	}
	return s.importCredentials(ctx, owner, creds, proxyID, target)
}

// ImportAPIKey honors the explicitly selected key mode. A domain-scoped proxy
// may supply an opaque credential reference instead of exposing the actual key;
// do not infer its type or normalize it as a CLI JWT from its prefix or dots.
// The native session exchange still validates access before any account commit.
func (s *DevinService) ImportAPIKey(ctx context.Context, owner int64, content string, proxyID *int64, target int64) (DevinAuthView, error) {
	key := strings.TrimSpace(content)
	if key == "" || len(key) > 1<<20 || strings.ContainsAny(content, "\r\n") {
		return DevinAuthView{}, errors.New("provide a Devin API key or configured credential reference")
	}
	return s.importCredentials(ctx, owner, map[string]any{"api_key": key, "auth_mode": "api_key"}, proxyID, target)
}

func (s *DevinService) importCredentials(ctx context.Context, owner int64, creds map[string]any, proxyID *int64, target int64) (DevinAuthView, error) {
	id, err := oauth.GenerateSessionID()
	if err != nil {
		return DevinAuthView{}, err
	}
	v := &DevinAuthSession{ID: id, Owner: owner, Status: "validating", Credentials: creds, ProxyID: proxyID, TargetAccountID: target, ExpiresAt: time.Now().Add(devinAuthTTL).Unix()}
	unlock, err := s.AuthLock(ctx, id)
	if err != nil {
		return DevinAuthView{}, err
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, devinAuthOperationTimeout)
	defer cancel()
	if err = s.SaveAuth(ctx, v); err != nil {
		return DevinAuthView{}, err
	}
	if err = s.ValidateAuth(ctx, v); err != nil {
		if saveErr := s.failAuth(ctx, v, "Credential validation failed: "+err.Error()); saveErr != nil {
			return DevinAuthView{}, saveErr
		}
		return DevinAuthSummary(v), nil
	}
	return DevinAuthSummary(v), nil
}
func (s *DevinService) CompleteAuth(ctx context.Context, id, code, state, ticket string, owner int64) (DevinAuthView, error) {
	loadAuthorized := func() (*DevinAuthSession, error) {
		v, err := s.LoadAuth(ctx, id, owner)
		if err != nil {
			return nil, err
		}
		if owner < 0 && subtle.ConstantTimeCompare([]byte(v.TicketHash), []byte(devin.CredentialFingerprint(ticket))) != 1 {
			return nil, errors.New("invalid pairing ticket")
		}
		return v, nil
	}
	// Reject unknown sessions and unauthenticated tickets without allocating locks.
	if _, err := loadAuthorized(); err != nil {
		return DevinAuthView{}, err
	}
	unlock, err := s.AuthLock(ctx, id)
	if err != nil {
		return DevinAuthView{}, err
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, devinAuthOperationTimeout)
	defer cancel()
	// Cancellation or another callback may have changed the record while locking.
	v, err := loadAuthorized()
	if err != nil {
		return DevinAuthView{}, err
	}
	if v.Status != "waiting" {
		if err = s.recoverInterruptedAuth(ctx, v); err != nil {
			return DevinAuthView{}, err
		}
		return DevinAuthSummary(v), nil
	}
	denied := false
	if strings.HasPrefix(strings.TrimSpace(code), "http") {
		u, err := url.Parse(strings.TrimSpace(code))
		if err != nil {
			return DevinAuthView{}, err
		}
		if u.Scheme != "http" || u.Host != "127.0.0.1:59653" || u.Path != "/callback" {
			return DevinAuthView{}, errors.New("unexpected Devin callback address")
		}
		state = u.Query().Get("state")
		code = u.Query().Get("code")
		denied = u.Query().Get("error") != ""
	}
	if state == "" || subtle.ConstantTimeCompare([]byte(v.State), []byte(state)) != 1 {
		return DevinAuthView{}, errors.New("authorization code or state mismatch")
	}
	if denied {
		if err = s.failAuth(ctx, v, "Devin sign-in was cancelled or denied"); err != nil {
			return DevinAuthView{}, err
		}
		return DevinAuthSummary(v), nil
	}
	if code == "" {
		return DevinAuthView{}, errors.New("authorization code is missing")
	}
	v.Status = "exchanging"
	if err = s.SaveAuth(ctx, v); err == nil {
		a := &Account{ID: v.transportID(), ProxyID: v.ProxyID, Concurrency: 1, Credentials: map[string]any{}}
		var client *devin.Client
		client, err = s.Client(ctx, a)
		if err == nil {
			var token string
			token, err = client.Exchange(ctx, code, v.Verifier)
			if err == nil {
				v.Credentials = map[string]any{"access_token": token, "auth_mode": "cli"}
				v.Status = "validating"
				v.Verifier = ""
				// Keep authenticating callback retries until a terminal state is saved.
				if err = s.SaveAuth(ctx, v); err == nil {
					err = s.ValidateAuth(ctx, v)
				}
			}
		}
	}
	if err != nil {
		if saveErr := s.failAuth(ctx, v, fmt.Sprintf("Devin authorization failed: %v", err)); saveErr != nil {
			return DevinAuthView{}, saveErr
		}
	}
	return DevinAuthSummary(v), nil
}
func (s *DevinService) CancelAuth(ctx context.Context, id string, owner int64) error {
	unlock, err := s.AuthLock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	v, err := s.LoadAuth(ctx, id, owner)
	if err != nil {
		return err
	}
	if v.Status == "connected" {
		return nil
	}
	return s.store.Delete(ctx, devinSessionKey(id))
}
