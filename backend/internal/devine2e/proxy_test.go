//go:build devine2e

package devine2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http/httpproxy"
)

// The production transport uses the account's explicit proxy rather than
// ProxyFromEnvironment. Bind the inherited platform proxy before PAT validation.
// This record exists only in the runner's disposable database.
func registerEnvironmentProxy(t *testing.T, a *application, admin string) int64 {
	t.Helper()
	target, _ := url.Parse("https://server.codeium.com")
	u, err := httpproxy.FromEnvironment().ProxyFunc()(target)
	if err != nil {
		t.Fatal("invalid inherited HTTPS proxy; no PAT exchange requested")
	}
	if u == nil {
		if _, err := os.Stat("/etc/codex/network-policy.json"); err == nil {
			t.Fatal("managed environment requires its inherited proxy for Devin; no PAT exchange requested")
		}
		return 0
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		t.Fatal("unsupported inherited HTTPS proxy; no PAT exchange requested")
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			t.Fatal("invalid inherited HTTPS proxy port; no PAT exchange requested")
		}
	}
	body := map[string]any{"name": "platform Devin upstream", "protocol": u.Scheme, "host": u.Hostname(), "port": port, "fallback_mode": "none"}
	if u.User != nil {
		username := u.User.Username()
		password, _ := u.User.Password()
		a.redactor.addSecret(username)
		a.redactor.addSecret(password)
		body["username"], body["password"] = username, password
	}
	proxy := api(t, a, "POST", "/admin/proxies", admin, body)
	id := proxy.Get("id").Int()
	require.Positive(t, id)
	return id
}

func TestEnvironmentProxyRoutesPATOnboarding(t *testing.T) {
	if os.Getenv("DEVIN_E2E_MODE") != "mock" {
		t.Skip("inherited proxy regression uses a synthetic upstream")
	}
	fixture := newNativeFixture(t)
	t.Setenv("HTTPS_PROXY", fixture.proxy.URL)
	t.Setenv("https_proxy", fixture.proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("DEVIN_E2E_MODE", "live")
	a := newApplication(t, fixture)
	admin := login(t, a, "admin@devin-e2e.invalid", a.password)
	compliance := api(t, a, "GET", "/admin/compliance", admin, nil)
	api(t, a, "POST", "/admin/compliance/accept", admin, map[string]string{"language": "en", "phrase": compliance.Get("ack_phrase_en").String()})
	group := api(t, a, "POST", "/admin/groups", admin, map[string]any{"name": "proxy-e2e", "platform": "devin", "subscription_type": "standard", "rate_multiplier": 1})
	proxyID := registerEnvironmentProxy(t, a, admin)
	accountID, err := addPATAccount(t, a, admin, "cog_e2e_fixture", group.Get("id").Int(), proxyID)
	require.Positive(t, fixture.mints.Load(), "PAT exchange must traverse the inherited proxy")
	require.NoError(t, err)
	account := api(t, a, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil)
	require.Equal(t, proxyID, account.Get("proxy_id").Int())
	before := fixture.mints.Load()
	api(t, a, "PUT", fmt.Sprintf("/admin/accounts/%d", accountID), admin, map[string]any{
		"credentials": map[string]any{"base_url": "https://unapproved.invalid"},
	})
	rejected := api(t, a, "POST", "/admin/devin/auth-sessions", admin, map[string]any{
		"mode": "key", "content": "cog_e2e_fixture", "account_id": accountID,
	})
	require.Equal(t, "failed", rejected.Get("status").String())
	require.Contains(t, rejected.Get("error").String(), "host is not allowed")
	require.Equal(t, before, fixture.mints.Load(), "unapproved host must be rejected before credential transmission")
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	require.Empty(t, fixture.chats, "proxy regression must not generate content")
}

func TestVerifiedPricingCacheSurvivesUnavailableRemoteHash(t *testing.T) {
	if os.Getenv("DEVIN_E2E_MODE") != "mock" {
		t.Skip("verified cache regression uses synthetic prices")
	}
	data := []byte(`{"synthetic-priced-card":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}}`)
	path := filepath.Join(t.TempDir(), "prices.json")
	require.NoError(t, os.WriteFile(path, data, 0600))
	t.Setenv("DEVIN_E2E_PRICING_CATALOG", path)
	t.Setenv("DEVIN_E2E_PRICING_SHA256", fmt.Sprintf("%x", sha256.Sum256(data)))
	fixture := newNativeFixture(t)
	// Keep this test independent of Internet availability: the fixture proxy
	// rejects ordinary GETs, so a hash refresh cannot replace the seeded card.
	t.Setenv("PRICING_HASH_URL", fixture.proxy.URL+"/unavailable-pricing-hash")
	a := newApplication(t, fixture)
	admin := login(t, a, "admin@devin-e2e.invalid", a.password)
	compliance := api(t, a, "GET", "/admin/compliance", admin, nil)
	api(t, a, "POST", "/admin/compliance/accept", admin, map[string]string{"language": "en", "phrase": compliance.Get("ack_phrase_en").String()})
	pricing := api(t, a, "GET", "/admin/channels/model-pricing?model=synthetic-priced-card", admin, nil)
	require.True(t, pricing.Get("found").Bool())
	require.True(t, pricing.Get("identified").Bool(), "verified catalog card must satisfy the Fusion pricing gate")
	require.Equal(t, .000001, pricing.Get("input_price").Float())
	require.Equal(t, .000002, pricing.Get("output_price").Float())
}

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
