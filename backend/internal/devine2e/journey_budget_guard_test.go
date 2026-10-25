//go:build devine2e

package devine2e

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type syntheticJourneyBudget struct {
	mu         sync.Mutex
	calls, max int
	stopped    bool
	outcomes   []string
}

func (b *syntheticJourneyBudget) Admit() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped || b.calls >= b.max {
		return 0, errors.New("closed")
	}
	b.calls++
	return b.calls, nil
}
func (b *syntheticJourneyBudget) Complete(_ int, outcome string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.outcomes = append(b.outcomes, outcome)
	b.stopped = b.stopped || outcome != "success"
	return nil
}

func TestApprovedJourneyRequiresProtocolCompletionAndStopsAcrossCases(t *testing.T) {
	for _, test := range []struct {
		name, body string
		stream     bool
		status     int
		expected   string
	}{
		{"text", `{"choices":[{"finish_reason":"stop"}]}`, false, 200, "success"},
		{"tool", `data: {"choices":[{"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n", true, 200, "success"},
		{"http-error", `{"error":{"code":"synthetic"}}`, false, 400, "failed"},
		{"sse-error", `data: {"error":{"message":"synthetic"}}` + "\n\ndata: [DONE]\n\n", true, 200, "failed"},
		{"no-terminal", `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n", true, 200, "failed"},
		{"missing-done", `data: {"choices":[{"finish_reason":"stop"}]}` + "\n\n", true, 200, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstream atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstream.Add(1)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer backend.Close()
			budget := &syntheticJourneyBudget{max: 1}
			for i := 0; i < 2; i++ {
				p := newJourneyAdmission(t, &application{base: backend.URL}, fixtureModel, 40)
				p.runBudget = budget
				response, err := http.Post(p.server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+fixtureModel+`","stream":`+map[bool]string{true: "true", false: "false"}[test.stream]+`}`))
				require.NoError(t, err)
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if i == 0 {
					require.Equal(t, test.status, response.StatusCode)
				} else {
					require.Equal(t, 402, response.StatusCode)
				}
			}
			require.EqualValues(t, 1, upstream.Load())
			require.Equal(t, []string{test.expected}, budget.outcomes)
		})
	}
}

func TestApprovedJourneyTransportUncertaintyStopsWithoutRetry(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	defer backend.Close()
	budget := &syntheticJourneyBudget{max: 5}
	p := newJourneyAdmission(t, &application{base: backend.URL}, fixtureModel, 40)
	p.runBudget = budget
	for i := 0; i < 2; i++ {
		response, err := http.Post(p.server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+fixtureModel+`"}`))
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if i == 0 {
			require.Equal(t, 502, response.StatusCode)
		} else {
			require.Equal(t, 402, response.StatusCode)
		}
	}
	require.Equal(t, 1, budget.calls)
	require.Equal(t, []string{"uncertain"}, budget.outcomes)
}
