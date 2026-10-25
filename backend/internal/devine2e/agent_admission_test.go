//go:build devine2e

package devine2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
)

func TestAgentAdmissionClampsTokensAndCountsSDKRetries(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["max_tokens"] != float64(1024) || body["max_completion_tokens"] != nil || r.Header.Get("X-Session-Id") != "synthetic-session" {
			http.Error(w, "budget or header contract violated", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()
	gate := newAgentAdmission(t, &application{base: backend.URL}, "fixture", 1)
	for index, status := range []int{200, 402} {
		request, err := http.NewRequest("POST", gate.server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"fixture","stream":true,"max_tokens":30000,"max_completion_tokens":30000}`))
		require.NoError(t, err)
		request.Header.Set("X-Session-Id", "synthetic-session")
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		require.Equal(t, status, response.StatusCode, "request %d", index)
	}
	require.Equal(t, int32(1), calls.Load(), "a retry exceeded the actual forwarding ceiling")
}

func TestAgentAdmissionConfirmsCancelledRequestWasNeverSent(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer backend.Close()
	gate := newAgentAdmission(t, &application{base: backend.URL}, "fixture", 1)
	marker := filepath.Join(t.TempDir(), "not-forwarded")
	gate.interruptFile = marker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "POST", gate.server.URL+"/v1/chat/completions", bytes.NewBufferString(`{"model":"fixture","stream":true}`))
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		response, _ := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		close(done)
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled client did not exit")
	}
	forwarded, _ := gate.snapshot()
	require.Zero(t, forwarded)
	require.Zero(t, calls.Load())
}

func TestAgentModelSelectionRequiresGrantedToolsAndKnownContext(t *testing.T) {
	textOnly := &pb.ClientModelConfig{ModelUid: "text-only", MaxTokens: 200000, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096}}
	unknownContext := &pb.ClientModelConfig{ModelUid: "unknown-context", ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}}
	_, _, err := selectAgentModels([]*pb.ClientModelConfig{textOnly, unknownContext})
	require.Error(t, err)
	capable := &pb.ClientModelConfig{ModelUid: "granted-tool-model", MaxTokens: 200000, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}}
	lead, sidekick, err := selectAgentModels([]*pb.ClientModelConfig{textOnly, unknownContext, capable})
	require.NoError(t, err)
	require.Equal(t, "granted-tool-model", lead)
	require.Equal(t, lead, sidekick, "single-model grants must be honestly identified as a same-model pairing")
}
