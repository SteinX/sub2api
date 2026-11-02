//go:build devine2e

package devine2e

import (
	"bytes"
	"encoding/base64"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/stretchr/testify/require"
)

// Buffer complete lines so a credential split across process writes is redacted.
type redactedLog struct {
	mu          sync.Mutex
	destination io.Writer
	pending     []byte
	secrets     []string
}

func (w *redactedLog) addSecret(secret string) {
	if secret == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.secrets = append(w.secrets, secret, base64.StdEncoding.EncodeToString([]byte(secret+":"+secret)))
}

func (w *redactedLog) line(data []byte) error {
	value := string(data)
	for _, secret := range w.secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	value = logredact.RedactText(value, "api_key", "apikey", "authorization", "x-api-key", "key", "token", "sessionToken", "pairing_ticket")
	_, err := io.WriteString(w.destination, value+"\n")
	return err
}

func (w *redactedLog) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, data...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		if err := w.line(w.pending[:i]); err != nil {
			return 0, err
		}
		w.pending = w.pending[i+1:]
	}
	return len(data), nil
}

func (w *redactedLog) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 {
		_ = w.line(w.pending)
		w.pending = nil
	}
}

func TestBackendLogRedactsSplitPATAndDynamicKeys(t *testing.T) {
	var destination bytes.Buffer
	w := &redactedLog{destination: &destination}
	w.addSecret("cog_synthetic_pat")
	_, err := w.Write([]byte("diagnostic cog_synt"))
	require.NoError(t, err)
	_, err = w.Write([]byte("hetic_pat\n{\"api_key\":\"synthetic-key\",\"access_token\":\"synthetic-session\"}\n"))
	require.NoError(t, err)
	w.addSecret("dynamic-public-key")
	_, err = w.Write([]byte("diagnostic dynamic-public-key"))
	require.NoError(t, err)
	w.flush()
	for _, value := range []string{"cog_synthetic_pat", "synthetic-key", "synthetic-session", "dynamic-public-key"} {
		require.NotContains(t, destination.String(), value)
	}
	require.Contains(t, destination.String(), "REDACTED")
}
