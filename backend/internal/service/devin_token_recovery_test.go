//go:build unit

package service

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
)

func TestDevinTokenRecoveryExcludesRejectedTokenAcrossConcurrentRequests(t *testing.T) {
	const failed = "devin-session-token$rejected"
	const recovered = "devin-session-token$minted"
	for _, tc := range []struct {
		name, memory, shared string
		mints                int32
	}{
		{"memory already cleared by another request", "", failed, 1},
		{"rejected token in both caches", failed, failed, 1},
		{"another request already recovered", recovered, failed, 0},
		{"another instance already recovered", "", recovered, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mints, _ := devinTestService(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "cog_test"}}
			key := d.runtimeKey(a)
			d.runtime[key] = devinRuntime{token: tc.memory, tokenUntil: time.Now().Add(time.Minute)}
			if err := d.store.Set(ctx, "devin:token:"+key, []byte(tc.shared), time.Minute); err != nil {
				t.Fatal(err)
			}
			const requests = 12
			start := make(chan struct{})
			type outcome struct {
				token string
				err   error
			}
			results := make(chan outcome, requests)
			var workers sync.WaitGroup
			for range requests {
				workers.Go(func() {
					<-start
					token, err := d.RecoverToken(ctx, a, failed)
					results <- outcome{token, err}
				})
			}
			close(start)
			workers.Wait()
			close(results)
			for result := range results {
				if result.err != nil || result.token != recovered {
					t.Fatalf("recovery reused a rejected credential: token=%q err=%v", result.token, result.err)
				}
			}
			if got := mints.Load(); got != tc.mints {
				t.Fatalf("recovery was not shared: minted %d times, want %d", got, tc.mints)
			}
		})
	}
}

func TestDevinTokenRecoveryRejectsAnUnchangedMintResult(t *testing.T) {
	d, mints, _ := devinTestService(t)
	upstream := d.upstream.(*devinHTTP)
	original := upstream.handle
	upstream.handle = func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != devin.MintPath {
			return original(r)
		}
		mints.Add(1)
		return devinReply(200, []byte(`{"sessionToken":"devin-session-token$rejected"}`)), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "cog_test"}}
	_, err := d.RecoverToken(ctx, a, "devin-session-token$rejected")
	if !devin.IsUnauthenticated(err) || mints.Load() != 1 {
		t.Fatalf("unchanged mint result should fail once: err=%v mints=%d", err, mints.Load())
	}
}

type devinTokenAcquireBarrier struct {
	devin.SessionStore
	arrived chan struct{}
	resume  <-chan struct{}
	once    sync.Once
}

func (s *devinTokenAcquireBarrier) Acquire(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	s.once.Do(func() {
		close(s.arrived)
		select {
		case <-s.resume:
		case <-ctx.Done():
		}
	})
	return s.SessionStore.Acquire(ctx, key, ttl)
}

func TestDevinTokenMintRechecksSharedCacheAfterLockAcquisition(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "cold cache"
		if recovery {
			name = "rejected token"
		}
		t.Run(name, func(t *testing.T) {
			winner, mints, _ := devinTestService(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "cog_test"}}
			const failed = "devin-session-token$rejected"
			const recovered = "devin-session-token$minted"
			if recovery {
				if err := winner.store.Set(ctx, "devin:token:"+winner.runtimeKey(a), []byte(failed), time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			resume := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(resume) }) }
			defer release()
			barrier := &devinTokenAcquireBarrier{SessionStore: winner.store, arrived: make(chan struct{}), resume: resume}
			loser := NewDevinService(nil, nil, winner.upstream, winner.cfg, barrier)
			lookup := func(s *DevinService) (string, error) {
				if recovery {
					return s.RecoverToken(ctx, a, failed)
				}
				return s.Token(ctx, a, false)
			}
			type outcome struct {
				token string
				err   error
			}
			loserDone := make(chan outcome, 1)
			go func() {
				token, err := lookup(loser)
				loserDone <- outcome{token, err}
			}()
			select {
			case <-barrier.arrived:
			case <-ctx.Done():
				t.Fatal("losing instance never reached the lock")
			}
			if token, err := lookup(winner); err != nil || token != recovered {
				t.Fatalf("winning instance failed: token=%q err=%v", token, err)
			}
			release()
			select {
			case result := <-loserDone:
				if result.err != nil || result.token != recovered {
					t.Fatalf("losing instance did not reuse the published token: token=%q err=%v", result.token, result.err)
				}
			case <-ctx.Done():
				t.Fatal("losing instance did not finish")
			}
			if got := mints.Load(); got != 1 {
				t.Fatalf("instances minted %d times, want one shared mint", got)
			}
		})
	}
}
