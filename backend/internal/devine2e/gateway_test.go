//go:build devine2e

package devine2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/coder/websocket"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type application struct {
	binary, base, dir, password string
	command                     *exec.Cmd
	done                        chan error
	log                         *os.File
	redactor                    *redactedLog
	trustFile                   string
	database                    string
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newApplication(t *testing.T, fixture *nativeFixture) *application {
	t.Helper()
	binary := os.Getenv("DEVIN_E2E_BINARY")
	require.NotEmpty(t, binary, "run bash deploy/devin-e2e.sh")
	for _, name := range []string{"DEVIN_E2E_PG_PORT", "DEVIN_E2E_REDIS_PORT", "DEVIN_E2E_DB_PASSWORD"} {
		require.NotEmpty(t, os.Getenv(name), "missing %s; use the isolated runner", name)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	base := "http://" + listener.Addr().String()
	require.NoError(t, listener.Close())
	dir := t.TempDir()
	a := &application{binary: binary, base: base, dir: dir, password: randomID()}
	// Each scenario owns a database inside this runner's disposable PostgreSQL.
	db, err := sql.Open("postgres", fmt.Sprintf("host=127.0.0.1 port=%s user=devin_e2e dbname=devin_e2e password=%s sslmode=disable", os.Getenv("DEVIN_E2E_PG_PORT"), os.Getenv("DEVIN_E2E_DB_PASSWORD")))
	require.NoError(t, err)
	a.database = "devin_e2e_" + randomID()
	_, err = db.Exec("CREATE DATABASE " + pq.QuoteIdentifier(a.database))
	_ = db.Close()
	require.NoError(t, err)
	if fixture != nil || os.Getenv("CODEX_PROXY_CERT") != "" {
		bundle, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt")
		require.NoError(t, err)
		if path := os.Getenv("CODEX_PROXY_CERT"); path != "" {
			certificate, err := os.ReadFile(path)
			require.NoError(t, err)
			bundle = append(append(bundle, '\n'), certificate...)
		}
		if fixture != nil {
			bundle = append(append(bundle, '\n'), fixture.ca...)
		}
		a.trustFile = filepath.Join(dir, "upstream-ca-bundle.pem")
		require.NoError(t, os.WriteFile(a.trustFile, bundle, 0600))
	}
	t.Cleanup(func() { a.stop(t) })
	a.start(t)
	return a
}

func (a *application) start(t *testing.T) {
	t.Helper()
	logDir := os.Getenv("DEVIN_E2E_ARTIFACTS")
	if logDir == "" {
		logDir = a.dir
	}
	var err error
	a.log, err = os.OpenFile(filepath.Join(logDir, "backend-private.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	require.NoError(t, err)
	port := strings.TrimPrefix(a.base, "http://127.0.0.1:")
	a.command = exec.Command(a.binary)
	a.command.Dir = a.dir
	// Keep the platform proxy and CA settings. All test services use loopback.
	var inherited []string
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		switch name {
		case "DEVIN_E2E_PAT", "DEVIN_E2E_CREDENTIAL", "SUB2API_API_KEY", "SUB2API_USER_TOKEN", "DEVIN_API_KEY":
			continue
		}
		inherited = append(inherited, variable)
	}
	a.command.Env = append(inherited,
		"DATA_DIR="+a.dir, "AUTO_SETUP=true", "RUN_MODE=standard",
		"CONFIG_FILE=",
		"SERVER_HOST=127.0.0.1", "SERVER_PORT="+port, "SERVER_MODE=release",
		"DATABASE_HOST=127.0.0.1", "DATABASE_PORT="+os.Getenv("DEVIN_E2E_PG_PORT"),
		"DATABASE_USER=devin_e2e", "DATABASE_DBNAME="+a.database, "DATABASE_SSLMODE=disable",
		"DATABASE_PASSWORD="+os.Getenv("DEVIN_E2E_DB_PASSWORD"),
		"REDIS_HOST=127.0.0.1", "REDIS_PORT="+os.Getenv("DEVIN_E2E_REDIS_PORT"), "REDIS_PASSWORD=", "REDIS_DB=0",
		"ADMIN_EMAIL=admin@devin-e2e.invalid", "ADMIN_PASSWORD="+a.password,
		"SECURITY_URL_ALLOWLIST_ENABLED=false", "SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS=true",
		"SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP=true", "BATCH_IMAGE_ENABLED=false",
		"GATEWAY_OPENAI_WS_MODE_ROUTER_V2_ENABLED=true",
	)
	if os.Getenv("DEVIN_E2E_MODE") == "live" {
		// The approved batch sends native credentials only to this hostname,
		// including when GetUserJwt supplies an account-specific API URL.
		a.command.Env = append(a.command.Env,
			"SECURITY_URL_ALLOWLIST_ENABLED=true",
			"SECURITY_URL_ALLOWLIST_UPSTREAM_HOSTS=server.codeium.com",
			// The managed proxy resolves the allowlisted hostname. Preserve the
			// runner's proxy-compatible setting instead of requiring local DNS.
			"SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS=true",
			"SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP=false",
		)
	}
	if a.trustFile != "" {
		a.command.Env = append(a.command.Env, "SSL_CERT_FILE="+a.trustFile)
	}
	if a.redactor == nil {
		a.redactor = &redactedLog{destination: a.log}
	} else {
		a.redactor.mu.Lock()
		a.redactor.destination = a.log
		a.redactor.mu.Unlock()
	}
	for _, secret := range []string{a.password, os.Getenv("DEVIN_E2E_DB_PASSWORD"), os.Getenv("DEVIN_E2E_PAT"), os.Getenv("DEVIN_E2E_CREDENTIAL")} {
		a.redactor.addSecret(secret)
	}
	a.command.Stdout, a.command.Stderr = a.redactor, a.redactor
	if err := a.command.Start(); err != nil {
		a.command = nil
		t.Fatal("could not start the test backend")
	}
	a.done = make(chan error, 1)
	go func() { a.done <- a.command.Wait() }()
	// The runner can also stop this exact binary if go test is interrupted.
	require.NoError(t, os.WriteFile(filepath.Join(logDir, "server.pid"), []byte(strconv.Itoa(a.command.Process.Pid)), 0600))
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-a.done:
			a.command = nil
			t.Fatal("backend exited during startup; inspect backend-private.log locally")
		default:
		}
		response, err := client.Get(a.base + "/health")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == 200 {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("backend not healthy after 90s; inspect backend-private.log locally")
}

func (a *application) stop(t *testing.T) {
	t.Helper()
	if a.command != nil {
		_ = a.command.Process.Signal(syscall.SIGTERM)
		select {
		case <-a.done:
		case <-time.After(10 * time.Second):
			_ = a.command.Process.Kill()
			<-a.done
		}
		a.command = nil
	}
	if a.log != nil {
		if a.redactor != nil {
			a.redactor.flush()
		}
		_ = a.log.Close()
		a.log = nil
	}
	if dir := os.Getenv("DEVIN_E2E_ARTIFACTS"); dir != "" {
		_ = os.Remove(filepath.Join(dir, "server.pid"))
	}
}

func request(t *testing.T, a *application, method, path, token string, body any, status int) []byte {
	t.Helper()
	a.redactor.addSecret(token)
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		require.NoError(t, err)
	}
	r, err := http.NewRequest(method, a.base+path, bytes.NewReader(encoded))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("anthropic-version", "2023-06-01")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := (&http.Client{Timeout: 90 * time.Second}).Do(r)
	require.NoError(t, err, "%s %s transport", method, path)
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	require.NoError(t, err)
	// Never print response bodies: auth/admin envelopes may contain credentials.
	require.Equal(t, status, response.StatusCode, "%s %s (body withheld)", method, path)
	return data
}

func api(t *testing.T, a *application, method, path, token string, body any) gjson.Result {
	t.Helper()
	data := request(t, a, method, "/api/v1"+path, token, body, 200)
	require.True(t, gjson.ValidBytes(data), "invalid API JSON at %s", path)
	require.Equal(t, int64(0), gjson.GetBytes(data, "code").Int(), "API failure at %s (body withheld)", path)
	return gjson.GetBytes(data, "data")
}

func login(t *testing.T, a *application, email, password string) string {
	t.Helper()
	a.redactor.addSecret(password)
	data := api(t, a, "POST", "/auth/login", "", map[string]any{"email": email, "password": password})
	token := data.Get("access_token").String()
	require.NotEmpty(t, token)
	a.redactor.addSecret(token)
	return token
}

func TestDevinCloudGateway(t *testing.T) {
	mode := os.Getenv("DEVIN_E2E_MODE")
	var fixture *nativeFixture
	if mode == "mock" {
		fixture = newNativeFixture(t)
	}
	runDevinCloudGateway(t, mode, fixture)
}

func TestPATPlanStopsBeforeInference(t *testing.T) {
	if os.Getenv("DEVIN_E2E_MODE") != "mock" {
		t.Skip("synthetic plan regression runs only in mock mode")
	}
	fixture := newNativeFixture(t)
	runDevinCloudGateway(t, "plan", fixture)
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	require.Empty(t, fixture.chats, "plan mode must not request inference")
	require.Positive(t, fixture.mints.Load(), "plan must still validate and onboard the PAT")
}

func TestOpaqueCredentialReferenceUsesKeyOnboarding(t *testing.T) {
	if os.Getenv("DEVIN_E2E_MODE") != "mock" {
		t.Skip("synthetic binding regression runs only in mock mode")
	}
	fixture := newNativeFixtureWithReference(t, "synthetic.runtime.reference")
	runDevinCloudGateway(t, "plan", fixture)
	require.Positive(t, fixture.referenceExchanges.Load(), "opaque reference must reach the native key exchange unchanged")
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	require.Empty(t, fixture.chats, "reference onboarding must not start inference")
}

func runDevinCloudGateway(t *testing.T, mode string, fixture *nativeFixture) {
	runDevinCloudGatewaySuite(t, mode, fixture, false)
}

func runDevinCloudGatewaySuite(t *testing.T, mode string, fixture *nativeFixture, agents bool) {
	t.Helper()
	require.Contains(t, []string{"mock", "plan", "live"}, mode, "select mock, plan or live explicitly")
	pat := "cog_e2e_fixture"
	if fixture != nil && fixture.credentialReference != "" {
		pat = fixture.credentialReference
	}
	maxOutputTokens := 1024
	if raw := os.Getenv("DEVIN_E2E_MAX_OUTPUT_TOKENS"); raw != "" {
		var err error
		maxOutputTokens, err = strconv.Atoi(raw)
		require.NoError(t, err)
		require.GreaterOrEqual(t, maxOutputTokens, 1)
		require.LessOrEqual(t, maxOutputTokens, 32768)
	}
	if fixture == nil {
		pat = os.Getenv("DEVIN_E2E_PAT")
		if pat == "" {
			pat = os.Getenv("DEVIN_E2E_CREDENTIAL")
		}
		require.NotEmpty(t, pat, "runtime credential reference unavailable; inspect the active binding/version without exporting the secret")
		require.False(t, strings.ContainsAny(pat, "\r\n"), "credential reference must not contain newline characters")
		if mode == "live" {
			require.Equal(t, "true", os.Getenv("DEVIN_E2E_ALLOW_PAID"), "explicit paid-test authorization is required before transmitting the PAT")
		}
	}
	t.Logf("mode=%s; production binary with isolated PostgreSQL/Redis; public group/key routing", mode)
	a := newApplication(t, fixture)
	admin := login(t, a, "admin@devin-e2e.invalid", a.password)
	// Exercise the first-run guard with this disposable synthetic administrator.
	compliance := api(t, a, "GET", "/admin/compliance", admin, nil)
	api(t, a, "POST", "/admin/compliance/accept", admin, map[string]string{
		"language": "en", "phrase": compliance.Get("ack_phrase_en").String(),
	})
	groupInput := map[string]any{
		"name": "devin-e2e", "platform": "devin", "rate_multiplier": 1,
		"subscription_type": "standard", "is_exclusive": true,
	}
	if fixture != nil {
		// Synthetic, explicit prices make reservation/billing checks independent
		// of remote price catalogs. Never override real prices in live mode.
		groupInput["model_pricing"] = []any{map[string]any{
			"platform": "devin", "models": []string{fixtureModel, fixtureSidekick}, "billing_mode": "token",
			"input_price": 0.000001, "output_price": 0.000002,
			"cache_write_price": 0.000001, "cache_read_price": 0.0000001,
		}}
	}
	group := api(t, a, "POST", "/admin/groups", admin, groupInput)
	groupID := group.Get("id").Int()
	require.Positive(t, groupID)
	var proxyID int64
	if fixture != nil {
		proxyID = fixture.registerProxy(t, a, admin)
	} else {
		proxyID = registerEnvironmentProxy(t, a, admin)
	}
	if mode == "mock" {
		for _, testCase := range []struct {
			name  string
			flag  *atomic.Bool
			stage string
		}{
			{"PAT_CLI_exchange_denied", &fixture.denyMint, "GetSelfDevinSessionToken"},
			{"PAT_model_discovery_denied", &fixture.denyModels, "GetCliModelConfigs"},
			{"PAT_empty_model_catalog", &fixture.emptyModels, "no available models"},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				testCase.flag.Store(true)
				defer testCase.flag.Store(false)
				id, err := addPATAccount(t, a, admin, "cog_e2e_denied_"+randomID(), groupID, proxyID)
				require.Zero(t, id)
				require.ErrorContains(t, err, testCase.stage)
				accounts := api(t, a, "GET", "/admin/accounts", admin, nil)
				require.Zero(t, accounts.Get("total").Int(), "failed authorization must not create an account")
				fixture.mu.Lock()
				defer fixture.mu.Unlock()
				require.Empty(t, fixture.chats)
			})
		}
	}
	accountID, err := addPATAccount(t, a, admin, pat, groupID, proxyID)
	require.NoError(t, err)
	account := api(t, a, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil)
	if mode == "mock" {
		t.Run("PAT_account_added_through_authorization_flow", func(t *testing.T) {
			require.Equal(t, "apikey", account.Get("type").String())
			require.Equal(t, "api_key", account.Get("extra.devin_auth_source").String())
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			require.Empty(t, fixture.chats)
		})
	}
	var catalog devin.CatalogSnapshot
	require.NoError(t, json.Unmarshal([]byte(account.Get("extra.devin_model_catalog").Raw), &catalog))
	selection, err := selectSmokeModel(catalog.Models, os.Getenv("DEVIN_E2E_MODEL"), maxOutputTokens)
	require.NoError(t, err)
	selection.PaidAllowed = mode == "live"
	selection.Mode = mode
	if !agents {
		saveSmokePlan(t, selection)
	}
	model := selection.Model
	userPassword := randomID()
	a.redactor.addSecret(userPassword)
	api(t, a, "POST", "/admin/users", admin, map[string]any{
		"email": "user@devin-e2e.invalid", "password": userPassword, "balance": 100,
		"concurrency": 4, "allowed_groups": []int64{groupID},
	})
	user := login(t, a, "user@devin-e2e.invalid", userPassword)
	keyInput := map[string]any{"name": "e2e-key", "group_id": groupID}
	if agents {
		quota := 0.25
		if mode == "mock" || os.Getenv("DEVIN_E2E_AGENT_BATCH") == "matrix" {
			quota = 1
		}
		keyInput["quota"] = quota
	}
	keyData := api(t, a, "POST", "/keys", user, keyInput)
	key := keyData.Get("key").String()
	require.NotEmpty(t, key, "public API key creation")
	a.redactor.addSecret(key)
	keyID := keyData.Get("id").Int()
	if mode == "mock" && !agents {
		t.Run("administrator_probe", func(t *testing.T) {
			body := request(t, a, "POST", fmt.Sprintf("/api/v1/admin/accounts/%d/test", accountID), admin,
				map[string]string{"model_id": model, "prompt": "Reply with E2E_OK only."}, 200)
			require.Contains(t, string(body), `"success":true`)
		})
	}

	if !t.Run("public_catalog_after_PAT_onboarding", func(t *testing.T) {
		body := request(t, a, "GET", "/v1/models", key, nil, 200)
		ids := gjson.GetBytes(body, "data.#.id").Array()
		found := false
		for _, id := range ids {
			if id.String() == model {
				found = true
			}
		}
		require.True(t, found, "selected Devin model absent from public key's group catalog")
	}) {
		t.Fatal("public model grant validation failed before inference")
	}
	if agents {
		runAgentCases(t, a, fixture, mode, admin, user, key, keyID, accountID, catalog.Models)
		return
	}
	if mode == "plan" {
		t.Log("PAT onboarding, account persistence and public model discovery completed; no inference requested")
		return
	}

	for _, protocol := range []string{"responses", "chat/completions", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				payload := map[string]any{"model": model, "stream": stream}
				if protocol == "responses" {
					payload["input"] = "Reply with E2E_OK only."
					payload["max_output_tokens"] = maxOutputTokens
				} else {
					payload["messages"] = []map[string]string{{"role": "user", "content": "Reply with E2E_OK only."}}
				}
				if protocol == "messages" {
					payload["max_tokens"] = maxOutputTokens
				} else if protocol == "chat/completions" {
					payload["max_completion_tokens"] = maxOutputTokens
				}
				body := request(t, a, "POST", "/v1/"+protocol, key, payload, 200)
				if stream {
					validateStream(t, body, protocol)
				} else {
					validateResponse(t, body, protocol)
				}
			})
		}
	}

	if mode == "mock" {
		t.Run("websocket_tool_continuation", func(t *testing.T) { websocketTools(t, a, key, model, fixture) })
		t.Run("expired_session_recovers_before_output", func(t *testing.T) {
			before := fixture.mints.Load()
			fixture.rejectJWT.Store(true)
			body := request(t, a, "POST", "/v1/responses", key, map[string]any{"model": model, "input": "hello"}, 200)
			validateResponse(t, body, "responses")
			require.Equal(t, before+1, fixture.mints.Load(), "recovery must mint exactly one replacement")
		})
		t.Run("public_key_authentication", func(t *testing.T) {
			request(t, a, "GET", "/v1/models", "e2e-invalid-key", nil, 401)
		})
		t.Run("exclusive_group_permission", func(t *testing.T) {
			other := api(t, a, "POST", "/admin/groups", admin, map[string]any{
				"name": "forbidden-e2e", "platform": "devin", "rate_multiplier": 1,
				"subscription_type": "standard", "is_exclusive": true,
			})
			request(t, a, "POST", "/api/v1/keys", user, map[string]any{"name": "forbidden-key", "group_id": other.Get("id").Int()}, 403)
		})
		t.Run("usage_persisted_for_public_key", func(t *testing.T) {
			require.Eventually(t, func() bool {
				data := api(t, a, "GET", "/usage?api_key_id="+strconv.FormatInt(keyID, 10), user, nil)
				for _, row := range data.Get("items").Array() {
					if row.Get("api_key_id").Int() == keyID && row.Get("account_id").Int() == accountID && row.Get("input_tokens").Int() > 0 && row.Get("output_tokens").Int() > 0 && row.Get("actual_cost").Float() > 0 {
						return true
					}
				}
				return false
			}, 15*time.Second, 250*time.Millisecond, "public gateway usage was not persisted")
		})
		t.Run("restart_preserves_public_scheduling", func(t *testing.T) {
			a.stop(t)
			a.start(t)
			body := request(t, a, "POST", "/v1/responses", key, map[string]any{"model": model, "input": "hello"}, 200)
			validateResponse(t, body, "responses")
		})
		t.Run("configured_and_inferred_fusion_routing", func(t *testing.T) {
			profile := fusion.DefaultProfile("fusion-e2e-configured", model, model)
			current := api(t, a, "GET", fmt.Sprintf("/admin/accounts/%d", accountID), admin, nil)
			var extra map[string]any
			require.NoError(t, json.Unmarshal([]byte(current.Get("extra").Raw), &extra))
			extra["devin_fusion_enabled"] = true
			extra["devin_fusion_profiles"] = []fusion.Profile{profile}
			api(t, a, "PUT", fmt.Sprintf("/admin/accounts/%d", accountID), admin, map[string]any{"extra": extra})
			for _, name := range []string{profile.PublicModel, "fusion-" + model + "-sidekick-" + model} {
				t.Run(name, func(t *testing.T) {
					fixture.mu.Lock()
					before := len(fixture.chats)
					fixture.mu.Unlock()
					body := request(t, a, "POST", "/v1/responses", key, map[string]any{"model": name, "input": "Reply with E2E_OK only."}, 200)
					validateResponse(t, body, "responses")
					fixture.mu.Lock()
					defer fixture.mu.Unlock()
					require.Len(t, fixture.chats, before+3, "Fusion must consult Sidekick and return to Lead for review")
					require.Equal(t, fixture.chats[before].CascadeId, fixture.chats[before+2].CascadeId)
					require.NotEqual(t, fixture.chats[before].CascadeId, fixture.chats[before+1].CascadeId, "Sidekick must have an independent native context")
				})
			}
		})
		t.Run("native_error_before_output", func(t *testing.T) {
			fixture.failChat.Store(true)
			body := request(t, a, "POST", "/v1/responses", key, map[string]any{"model": model, "input": "hello", "stream": true}, 400)
			require.True(t, gjson.GetBytes(body, "error").Exists(), "early native error must be an error envelope")
		})
		t.Run("native_error_after_output", func(t *testing.T) {
			fixture.failStream.Store(true)
			fixture.mu.Lock()
			before := len(fixture.chats)
			fixture.mu.Unlock()
			body := request(t, a, "POST", "/v1/responses", key, map[string]any{"model": model, "input": "hello", "stream": true}, 200)
			require.Contains(t, string(body), "partial")
			require.NotContains(t, string(body), "response.completed")
			require.True(t, bytes.Contains(body, []byte("response.failed")) || bytes.Contains(body, []byte(`"type":"error"`)), "native failure must produce a terminal failure event")
			fixture.mu.Lock()
			after := len(fixture.chats)
			fixture.mu.Unlock()
			require.Equal(t, before+1, after, "partially emitted generation must not be replayed")
		})
	}
}

func validateResponse(t *testing.T, body []byte, protocol string) {
	t.Helper()
	require.True(t, gjson.ValidBytes(body), "invalid response JSON")
	require.False(t, gjson.GetBytes(body, "error").Exists(), "gateway returned an error envelope")
	path := "output.#.content.#.text"
	switch protocol {
	case "responses":
		require.Equal(t, "completed", gjson.GetBytes(body, "status").String())
	case "chat/completions":
		path = "choices.0.message.content"
	case "messages":
		path = "content.#.text"
	}
	require.Contains(t, gjson.GetBytes(body, path).String(), "E2E_OK", "missing generated answer")
	usagePath := "usage.output_tokens"
	if protocol == "chat/completions" {
		usagePath = "usage.completion_tokens"
	}
	require.Positive(t, gjson.GetBytes(body, usagePath).Int(), "missing output token usage")
}

func validateStream(t *testing.T, body []byte, protocol string) {
	t.Helper()
	terminal := "[DONE]"
	if protocol == "responses" {
		terminal = "response.completed"
	}
	if protocol == "messages" {
		terminal = "message_stop"
	}
	require.Contains(t, string(body), terminal, "truncated stream")
	require.NotContains(t, string(body), "response.failed")
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var text strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		require.True(t, gjson.Valid(data), "invalid SSE JSON")
		v := gjson.Parse(data)
		require.False(t, v.Get("error").Exists(), "SSE error payload")
		switch protocol {
		case "responses":
			if v.Get("type").String() == "response.output_text.delta" {
				text.WriteString(v.Get("delta").String())
			}
		case "chat/completions":
			text.WriteString(v.Get("choices.0.delta.content").String())
		case "messages":
			text.WriteString(v.Get("delta.text").String())
		}
	}
	require.NoError(t, scanner.Err())
	require.Contains(t, text.String(), "E2E_OK", "stream emitted no generated answer")
}

func websocketTools(t *testing.T, a *application, key, model string, fixture *nativeFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(a.base, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	require.NoError(t, err)
	defer conn.CloseNow()
	readCompleted := func() gjson.Result {
		for {
			_, data, err := conn.Read(ctx)
			require.NoError(t, err)
			v := gjson.ParseBytes(data)
			require.NotContains(t, []string{"error", "response.failed"}, v.Get("type").String(), "WebSocket failure")
			if v.Get("type").String() == "response.completed" {
				return v.Get("response")
			}
		}
	}
	write := func(body any) {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		require.NoError(t, conn.Write(ctx, websocket.MessageText, data))
	}
	fixture.mu.Lock()
	before := len(fixture.chats)
	fixture.mu.Unlock()
	write(map[string]any{"type": "response.create", "model": model, "generate": false, "input": "warmup"})
	require.Empty(t, readCompleted().Get("output").Array())
	fixture.mu.Lock()
	after := len(fixture.chats)
	fixture.mu.Unlock()
	require.Equal(t, before, after, "warmup must not invoke native inference")
	write(map[string]any{"type": "response.create", "model": model, "input": "Use e2e_echo.", "tools": []any{map[string]any{
		"type": "function", "name": "e2e_echo", "parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]string{"type": "string"}}},
	}}, "tool_choice": map[string]string{"type": "function", "name": "e2e_echo"}})
	first := readCompleted()
	require.Equal(t, "function_call", first.Get("output.0.type").String())
	require.Equal(t, "call_e2e", first.Get("output.0.call_id").String())
	write(map[string]any{"type": "response.create", "previous_response_id": first.Get("id").String(), "input": []any{
		map[string]string{"type": "function_call_output", "call_id": "call_e2e", "output": "E2E_TOOL_RESULT"},
	}})
	second := readCompleted()
	require.Contains(t, second.Get("output.#.content.#.text").String(), "E2E_OK")
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	require.Len(t, fixture.chats, before+2)
	require.Equal(t, fixture.chats[before].CascadeId, fixture.chats[before+1].CascadeId, "native conversation changed across tool continuation")
}
