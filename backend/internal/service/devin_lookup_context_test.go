//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
)

func TestDevinSharedLookupsSurviveCallerCancellation(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, want string }{
		{"token", devin.MintPath, "devin-session-token$minted"},
		{"models", devin.ModelsPath, "swe-2-max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := devinTestService(t)
			a := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "cog_test"}}
			waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelWaiter()
			leaderCtx, cancelLeader := context.WithCancel(context.Background())
			defer cancelLeader()
			resume := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(resume) }) }
			defer release()
			started := make(chan context.Context, 1)
			var startedOnce sync.Once
			var calls atomic.Int32
			upstream := d.upstream.(*devinHTTP)
			original := upstream.handle
			upstream.handle = func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != tc.endpoint {
					return original(r)
				}
				calls.Add(1)
				startedOnce.Do(func() { started <- r.Context() })
				select {
				case <-resume:
					return original(r)
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			lookup := func(ctx context.Context) (string, error) {
				if tc.name == "token" {
					return d.Token(ctx, a, false)
				}
				models, err := d.Models(ctx, a)
				if err != nil {
					return "", err
				}
				if len(models) != 1 {
					return "", errors.New("unexpected model catalog")
				}
				return devin.ModelID(models[0]), nil
			}
			type outcome struct {
				value string
				err   error
			}
			startLookup := func(ctx context.Context) <-chan outcome {
				done := make(chan outcome, 1)
				go func() {
					value, err := lookup(ctx)
					done <- outcome{value, err}
				}()
				return done
			}
			leaderDone := startLookup(leaderCtx)
			var requestCtx context.Context
			select {
			case requestCtx = <-started:
			case result := <-leaderDone:
				t.Fatalf("lookup finished before upstream was blocked: %+v", result)
			case <-waiterCtx.Done():
				t.Fatal("lookup did not reach upstream")
			}
			waiterDone := startLookup(waiterCtx)
			cancelLeader()
			select {
			case result := <-leaderDone:
				if !errors.Is(result.err, context.Canceled) {
					t.Fatalf("canceled caller did not stop waiting: %+v", result)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("canceled caller remained blocked on shared lookup")
			}
			if requestCtx.Err() != nil {
				t.Fatal("caller cancellation stopped the shared upstream operation")
			}
			if deadline, ok := requestCtx.Deadline(); !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 31*time.Second {
				t.Fatal("shared lookup has no bounded deadline")
			}
			release()
			select {
			case result := <-waiterDone:
				if result.err != nil || result.value != tc.want {
					t.Fatalf("live waiter did not receive the shared result: %+v", result)
				}
			case <-waiterCtx.Done():
				t.Fatal("live waiter did not finish")
			}
			if value, err := lookup(waiterCtx); err != nil || value != tc.want || calls.Load() != 1 {
				t.Fatalf("shared result was not cached: value=%q err=%v upstream calls=%d", value, err, calls.Load())
			}
			if _, err := lookup(leaderCtx); !errors.Is(err, context.Canceled) {
				t.Fatalf("cache hit ignored caller cancellation: %v", err)
			}
		})
	}
}
