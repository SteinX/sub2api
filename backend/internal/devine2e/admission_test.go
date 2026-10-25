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
