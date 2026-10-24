//go:build devine2e

package devine2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Use the existing account proxy option to exercise new-account onboarding at
// the production default hostname. TLS stays verified with a temporary test CA;
// no production endpoint override or TLS bypass is added to the application.
func (f *nativeFixture) startTLSProxy(t *testing.T) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Devin E2E synthetic CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"server.codeium.com"},
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, public, private)
	require.NoError(t, err)
	f.ca = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serveHTTP))
	f.server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}, MinVersion: tls.VersionTLS12}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	f.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "server.codeium.com:443" {
			http.Error(w, "fixture destination denied", 403)
			return
		}
		upstream, err := net.DialTimeout("tcp", f.server.Listener.Addr().String(), 3*time.Second)
		if err != nil {
			http.Error(w, "fixture unavailable", 502)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		defer client.Close()
		defer upstream.Close()
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if buffer.Flush() != nil {
			return
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, buffer); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-done
	}))
	t.Cleanup(f.proxy.Close)
}

func (f *nativeFixture) registerProxy(t *testing.T, a *application, admin string) int64 {
	t.Helper()
	u, err := url.Parse(f.proxy.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	proxy := api(t, a, "POST", "/admin/proxies", admin, map[string]any{"name": "synthetic Devin upstream", "protocol": "http", "host": u.Hostname(), "port": port})
	id := proxy.Get("id").Int()
	require.Positive(t, id)
	return id
}
