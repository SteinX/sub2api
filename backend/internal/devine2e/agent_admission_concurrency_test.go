//go:build devine2e

package devine2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAgentAdmissionBoundsConcurrentForwarding(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	gate := newAgentAdmission(t, &application{base: target.URL}, "synthetic", 2)
	client := &http.Client{Timeout: 5 * time.Second}
	request := func() int {
		response, err := client.Post(gate.server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"synthetic","messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			return 0
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	start := make(chan struct{})
	statuses := make(chan int, 8)
	var workers sync.WaitGroup
	for n := 0; n < 8; n++ {
		workers.Add(1)
		go func() { defer workers.Done(); <-start; statuses <- request() }()
	}
	close(start)
	workers.Wait()
	close(statuses)
	for status := range statuses {
		require.Contains(t, []int{200, 402, 409}, status)
	}
	forwarded, _ := gate.snapshot()
	require.LessOrEqual(t, forwarded, 2)
	for forwarded < 2 {
		require.Equal(t, http.StatusOK, request())
		forwarded, _ = gate.snapshot()
	}
	require.Equal(t, http.StatusPaymentRequired, request())
	require.Equal(t, int32(2), calls.Load())
	forwarded, _ = gate.snapshot()
	require.Equal(t, 2, forwarded)
}
