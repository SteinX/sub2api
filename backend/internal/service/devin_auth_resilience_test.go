//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type devinAuthStoreProbe struct {
	devin.SessionStore
	locks        int
	afterAcquire func()
	failTerminal bool
}

func (s *devinAuthStoreProbe) Acquire(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	s.locks++
	unlock, ok, err := s.SessionStore.Acquire(ctx, key, ttl)
	if ok && s.afterAcquire != nil {
		s.afterAcquire()
	}
	return unlock, ok, err
}

func (s *devinAuthStoreProbe) Set(ctx context.Context, key string, data []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var v DevinAuthSession
	if s.failTerminal && json.Unmarshal(data, &v) == nil && v.Status == "failed" {
		return errors.New("private store failure")
	}
	return s.SessionStore.Set(ctx, key, data, ttl)
}

func TestDevinCallbackValidatesBeforeAndAfterLocking(t *testing.T) {
	d, _, exchanges := devinTestService(t)
	probe := &devinAuthStoreProbe{SessionStore: d.store}
	d.store = probe
	ctx := context.Background()
	for _, id := range []string{"unknown-a", "unknown-b", "unknown-c"} {
		if _, err := d.CompleteAuth(ctx, id, "code", "state", "ticket", -1); !infraerrors.IsNotFound(err) {
			t.Fatalf("unknown session was not rejected: %v", err)
		}
	}
	view, err := d.StartAuth(ctx, 7, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(view.AuthURL)
	state := u.Query().Get("state")
	if _, err = d.CompleteAuth(ctx, view.ID, "code", state, "wrong-ticket", -1); err == nil {
		t.Fatal("invalid pairing ticket was accepted")
	}
	if probe.locks != 0 || exchanges.Load() != 0 {
		t.Fatal("unauthenticated requests allocated locks or exchanged codes")
	}
	probe.afterAcquire = func() {
		if err := probe.SessionStore.Delete(ctx, devinSessionKey(view.ID)); err != nil {
			t.Error(err)
		}
	}
	if _, err = d.CompleteAuth(ctx, view.ID, "code", state, view.PairingTicket, -1); !infraerrors.IsNotFound(err) {
		t.Fatalf("session removed during locking was not revalidated: %v", err)
	}
	if probe.locks != 1 || exchanges.Load() != 0 {
		t.Fatal("removed session spent the authorization code")
	}
}

func TestDevinCancellationPersistsTerminalAuthorization(t *testing.T) {
	for _, phase := range []string{"exchange", "validation", "import"} {
		t.Run(phase, func(t *testing.T) {
			d, _, _ := devinTestService(t)
			d.store = &devinAuthStoreProbe{SessionStore: d.store}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			upstream, ok := d.upstream.(*devinHTTP)
			if !ok {
				t.Fatal("missing upstream test double")
			}
			original := upstream.handle
			upstream.handle = func(r *http.Request) (*http.Response, error) {
				if phase == "exchange" && r.URL.Path == "/auth/cli/token" || phase != "exchange" && r.URL.Path == devin.ModelsPath {
					cancel()
					return nil, ctx.Err()
				}
				return original(r)
			}
			var result DevinAuthView
			var err error
			if phase == "import" {
				result, err = d.ImportAuth(ctx, 7, "session-token", nil, 0)
			} else {
				view, startErr := d.StartAuth(ctx, 7, nil, 0)
				if startErr != nil {
					t.Fatal(startErr)
				}
				u, _ := url.Parse(view.AuthURL)
				result, err = d.CompleteAuth(ctx, view.ID, "code", u.Query().Get("state"), view.PairingTicket, -1)
			}
			if err != nil || result.Status != "failed" {
				t.Fatal("terminal failure was not returned", result, err)
			}
			persisted, err := d.LoadAuth(context.Background(), result.ID, 7)
			if err != nil || persisted.Status != "failed" || persisted.Credentials != nil || persisted.Verifier != "" || persisted.TicketHash != "" {
				t.Fatal("canceled request retained an active state or credentials", persisted, err)
			}
		})
	}
}

func TestDevinTerminalPersistenceFailureIsReported(t *testing.T) {
	d, _, _ := devinTestService(t)
	d.store = &devinAuthStoreProbe{SessionStore: d.store, failTerminal: true}
	upstream, ok := d.upstream.(*devinHTTP)
	if !ok {
		t.Fatal("missing upstream test double")
	}
	original := upstream.handle
	upstream.handle = func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/auth/cli/token" {
			return nil, errors.New("exchange failed")
		}
		return original(r)
	}
	ctx := context.Background()
	view, err := d.StartAuth(ctx, 7, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(view.AuthURL)
	if _, err = d.CompleteAuth(ctx, view.ID, "code", u.Query().Get("state"), view.PairingTicket, -1); !infraerrors.IsServiceUnavailable(err) {
		t.Fatalf("failed terminal write was ignored: %v", err)
	}
}

func TestDevinInterruptedSessionRecoversAfterStoreReturns(t *testing.T) {
	for _, phase := range []string{"exchange", "validation"} {
		t.Run(phase, func(t *testing.T) {
			for _, viaStatus := range []bool{false, true} {
				t.Run(map[bool]string{false: "callback retry", true: "status polling"}[viaStatus], func(t *testing.T) {
					d, _, _ := devinTestService(t)
					probe := &devinAuthStoreProbe{SessionStore: d.store, failTerminal: true}
					d.store = probe
					upstream, ok := d.upstream.(*devinHTTP)
					if !ok {
						t.Fatal("missing upstream test double")
					}
					original := upstream.handle
					exchanges := 0
					upstream.handle = func(r *http.Request) (*http.Response, error) {
						if r.URL.Path == "/auth/cli/token" {
							exchanges++
							if phase == "exchange" {
								return nil, errors.New("exchange failed")
							}
						}
						if phase == "validation" && r.URL.Path == devin.ModelsPath {
							return nil, errors.New("validation failed")
						}
						return original(r)
					}
					ctx := context.Background()
					view, err := d.StartAuth(ctx, 7, nil, 0)
					if err != nil {
						t.Fatal(err)
					}
					u, _ := url.Parse(view.AuthURL)
					state := u.Query().Get("state")
					if _, err = d.CompleteAuth(ctx, view.ID, "code", state, view.PairingTicket, -1); !infraerrors.IsServiceUnavailable(err) {
						t.Fatal("failed write should be retryable", err)
					}
					probe.failTerminal = false
					if viaStatus {
						var v *DevinAuthSession
						v, err = d.AuthStatus(ctx, view.ID, 7)
						if err != nil || v.Status != "failed" {
							t.Fatal("polling did not recover the orphaned state", v, err)
						}
					} else {
						var v DevinAuthView
						v, err = d.CompleteAuth(ctx, view.ID, "code", state, view.PairingTicket, -1)
						if err != nil || v.Status != "failed" {
							t.Fatal("callback retry did not recover the orphaned state", v, err)
						}
					}
					v, err := d.LoadAuth(ctx, view.ID, 7)
					if err != nil || v.Status != "failed" || v.Credentials != nil || v.Verifier != "" || v.TicketHash != "" || exchanges != 1 {
						t.Fatal("recovery retained secrets or replayed the code", v, err, exchanges)
					}
				})
			}
		})
	}
}

func TestDevinStatusPreservesActiveValidationAndRecoversAnOrphan(t *testing.T) {
	d, _, _ := devinTestService(t)
	ctx := context.Background()
	view, err := d.StartAuth(ctx, 7, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.LoadAuth(ctx, view.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	v.Status = "validating"
	v.Credentials = map[string]any{"access_token": "private-token"}
	if err = d.SaveAuth(ctx, v); err != nil {
		t.Fatal(err)
	}
	unlock, err := d.AuthLock(ctx, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	v, err = d.AuthStatus(ctx, view.ID, 7)
	unlock()
	if err != nil || v.Status != "validating" {
		t.Fatal("status polling interrupted a live authorization", v, err)
	}
	v, err = d.AuthStatus(ctx, view.ID, 7)
	if err != nil || v.Status != "failed" || v.Credentials != nil {
		t.Fatal("orphaned validation was not terminated", v, err)
	}
}

func TestDevinSuccessfulCallbackCanBeReadWithoutRepeatingTheExchange(t *testing.T) {
	d, _, exchanges := devinTestService(t)
	ctx := context.Background()
	view, err := d.StartAuth(ctx, 7, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(view.AuthURL)
	state := u.Query().Get("state")
	for range 2 {
		result, err := d.CompleteAuth(ctx, view.ID, "code", state, view.PairingTicket, -1)
		if err != nil || result.Status != "ready" || result.PairingTicket != "" {
			t.Fatal("successful callback replay did not return its existing outcome", result, err)
		}
	}
	v, err := d.LoadAuth(ctx, view.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	v.Status = "connected"
	v.AccountID = 12
	v.Credentials = nil
	if err = d.SaveAuth(ctx, v); err != nil {
		t.Fatal(err)
	}
	result, err := d.CompleteAuth(ctx, view.ID, "code", state, view.PairingTicket, -1)
	if err != nil || result.Status != "connected" || result.AccountID != 12 || exchanges.Load() != 1 {
		t.Fatal("saved callback outcome was not idempotent", result, err, exchanges.Load())
	}
	if _, err = d.CompleteAuth(ctx, view.ID, "code", state, "wrong-ticket", -1); err == nil {
		t.Fatal("terminal callback outcome was exposed without authentication")
	}
}
