//go:build devine2e

package devine2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientContractAndProjectAttemptBudgets(t *testing.T) {
	for _, limit := range []int{12, 40} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var calls atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer backend.Close()
			app := &application{base: backend.URL}
			var admission *agentAdmission
			if limit == 12 {
				admission = newAgentAdmission(t, app, fixtureModel, 40)
			} else {
				admission = newAgentAdmission(t, app, fixtureModel, 40, 40)
			}
			for i := 0; i <= limit; i++ {
				response, err := http.Post(admission.server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+fixtureModel+`","messages":[{"role":"user","content":"synthetic"}]}`))
				require.NoError(t, err)
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if i < limit {
					require.Equal(t, 200, response.StatusCode)
				} else {
					require.Equal(t, 402, response.StatusCode)
				}
			}
			require.EqualValues(t, limit, calls.Load(), "requests beyond the selected contract budget must not forward")
		})
	}
}

func TestProjectAdmissionSerializesClientCompactionRequests(t *testing.T) {
	var active, maximum, calls atomic.Int32
	entered, release := make(chan struct{}, 2), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		calls.Add(1)
		entered <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	admission := newJourneyAdmission(t, &application{base: backend.URL}, fixtureModel, 2)
	results := make(chan int, 2)
	client := &http.Client{Timeout: 5 * time.Second}
	post := func() {
		response, err := client.Post(admission.server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+fixtureModel+`"}`))
		if err != nil {
			results <- 0
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		results <- response.StatusCode
	}
	go post()
	<-entered
	go post()
	require.Eventually(t, func() bool { _, requests := admission.snapshot(); return len(requests) == 2 }, time.Second, 5*time.Millisecond)
	close(release)
	require.Equal(t, 200, <-results)
	require.Equal(t, 200, <-results)
	require.EqualValues(t, 1, maximum.Load(), "two client summary requests must never become parallel upstream work")
	require.EqualValues(t, 2, calls.Load())
	post()
	require.Equal(t, 402, <-results, "serialization must preserve the original request cap")
	require.EqualValues(t, 2, calls.Load())
}
