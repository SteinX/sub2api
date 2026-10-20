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
	"github.com/Wei-Shaw/sub2api/internal/pkg/oauth"
)

const devinAuthTTL = 10 * time.Minute

// DevinAuthSession is an internal Redis record, never an HTTP response.
type DevinAuthSession struct {
	ID              string         `json:"id"`
	Owner           int64          `json:"owner"`
	Status          string         `json:"status"`
	State           string         `json:"state"`
	Verifier        string         `json:"verifier,omitempty"`
	TicketHash      string         `json:"ticket_hash,omitempty"`
	Credentials     map[string]any `json:"credentials,omitempty"`
	ProxyID         *int64         `json:"proxy_id,omitempty"`
	TargetAccountID int64          `json:"target_account_id,omitempty"`
	Models          []string       `json:"models,omitempty"`
	Error           string         `json:"error,omitempty"`
	AccountID       int64          `json:"account_id,omitempty"`
	AutoRecover     bool           `json:"auto_recover,omitempty"`
	ExpiresAt       int64          `json:"expires_at"`
}
type DevinAuthView struct {
	ID            string   `json:"id"`
	Status        string   `json:"status"`
	AuthURL       string   `json:"auth_url,omitempty"`
	PairingTicket string   `json:"pairing_ticket,omitempty"`
	Models        []string `json:"models"`
	AutoRecover   bool     `json:"auto_recover"`
	Error         string   `json:"error,omitempty"`
	AccountID     int64    `json:"account_id,omitempty"`
	ExpiresAt     int64    `json:"expires_at"`
}

func DevinAuthSummary(session *DevinAuthSession) DevinAuthView {
	key, _ := session.Credentials["api_key"].(string)
	return DevinAuthView{ID: session.ID, Status: session.Status, Models: session.Models, AutoRecover: key != "" || session.AutoRecover, Error: session.Error, AccountID: session.AccountID, ExpiresAt: session.ExpiresAt}
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
		return errors.New("devin authorization requires Redis")
	}
	ttl := time.Until(time.Unix(session.ExpiresAt, 0))
	if ttl <= 0 {
		return errors.New("devin authorization expired")
	}
	b, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return s.store.Set(ctx, devinSessionKey(session.ID), b, ttl)
}
func (s *DevinService) LoadAuth(ctx context.Context, id string, owner int64) (*DevinAuthSession, error) {
	if s.store == nil {
		return nil, errors.New("devin authorization requires Redis")
	}
	b, err := s.store.Get(ctx, devinSessionKey(id))
	if err != nil {
		return nil, errors.New("devin authorization expired or unavailable")
	}
	var v DevinAuthSession
	if json.Unmarshal(b, &v) != nil {
		return nil, errors.New("invalid authorization session")
	}
	if owner != -1 && owner != v.Owner {
		return nil, errors.New("authorization session belongs to another administrator")
	}
	return &v, nil
}
func (s *DevinService) AuthLock(ctx context.Context, id string) (func(), error) {
	if s.store == nil {
		return nil, errors.New("devin authorization requires Redis")
	}
	unlock, ok, err := s.store.Acquire(ctx, devinSessionKey(id)+":lock", 90*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("authorization operation is already in progress")
	}
	return unlock, nil
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
	v.AutoRecover = v.Credentials["api_key"] != nil
	a := &Account{ID: v.transportID(), Platform: PlatformDevin, Type: AccountTypeOAuth, Credentials: v.Credentials, ProxyID: v.ProxyID, Concurrency: 1}
	models, err := s.Models(ctx, a)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return errors.New("devin account returned no available models")
	}
	v.Models = make([]string, 0, len(models))
	for _, m := range models {
		v.Models = append(v.Models, devin.ModelID(m))
	}
	v.Status = "ready"
	v.Error = ""
	v.Verifier = ""
	v.TicketHash = ""
	return s.SaveAuth(ctx, v)
}
func (s *DevinService) ImportAuth(ctx context.Context, owner int64, content string, proxyID *int64, target int64) (DevinAuthView, error) {
	creds, err := ParseDevinCredential(content)
	if err != nil {
		return DevinAuthView{}, err
	}
	id, err := oauth.GenerateSessionID()
	if err != nil {
		return DevinAuthView{}, err
	}
	v := &DevinAuthSession{ID: id, Owner: owner, Status: "validating", Credentials: creds, ProxyID: proxyID, TargetAccountID: target, ExpiresAt: time.Now().Add(devinAuthTTL).Unix()}
	if err = s.SaveAuth(ctx, v); err != nil {
		return DevinAuthView{}, err
	}
	if err = s.ValidateAuth(ctx, v); err != nil {
		v.Status = "failed"
		v.Error = "Credential validation failed: " + err.Error()
		v.Credentials = nil
		_ = s.SaveAuth(ctx, v)
		return DevinAuthSummary(v), nil
	}
	return DevinAuthSummary(v), nil
}
func (s *DevinService) CompleteAuth(ctx context.Context, id, code, state, ticket string, owner int64) (DevinAuthView, error) {
	unlock, err := s.AuthLock(ctx, id)
	if err != nil {
		return DevinAuthView{}, err
	}
	defer unlock()
	v, err := s.LoadAuth(ctx, id, owner)
	if err != nil {
		return DevinAuthView{}, err
	}
	if owner < 0 && subtle.ConstantTimeCompare([]byte(v.TicketHash), []byte(devin.CredentialFingerprint(ticket))) != 1 {
		return DevinAuthView{}, errors.New("invalid pairing ticket")
	}
	if v.Status != "waiting" {
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
		v.Status = "failed"
		v.Error = "Devin sign-in was cancelled or denied"
		v.Verifier = ""
		v.TicketHash = ""
		v.Credentials = nil
		if err = s.SaveAuth(ctx, v); err != nil {
			return DevinAuthView{}, err
		}
		return DevinAuthSummary(v), nil
	}
	if code == "" {
		return DevinAuthView{}, errors.New("authorization code is missing")
	}
	v.Status = "exchanging"
	if err = s.SaveAuth(ctx, v); err != nil {
		return DevinAuthView{}, err
	}
	a := &Account{ID: v.transportID(), ProxyID: v.ProxyID, Concurrency: 1, Credentials: map[string]any{}}
	client, err := s.Client(ctx, a)
	if err == nil {
		var token string
		token, err = client.Exchange(ctx, code, v.Verifier)
		if err == nil {
			v.Credentials = map[string]any{"access_token": token, "auth_mode": "cli"}
			v.Status = "validating"
			v.Verifier = ""
			v.TicketHash = ""
			_ = s.SaveAuth(ctx, v)
			err = s.ValidateAuth(ctx, v)
		}
	}
	if err != nil {
		v.Status = "failed"
		v.Error = fmt.Sprintf("Devin authorization failed: %v", err)
		v.Credentials = nil
		v.Verifier = ""
		v.TicketHash = ""
		_ = s.SaveAuth(ctx, v)
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
