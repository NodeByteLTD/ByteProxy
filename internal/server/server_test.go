package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NodeByteLTD/ByteProxy/internal/config"
	"github.com/NodeByteLTD/ByteProxy/internal/registry"
)

const proxyKey = "test-proxy-key-123"
const manageKey = "test-manage-key-456"

var png = []byte{0x89, 0x50, 0x4e, 0x47, 0, 1, 2, 3, 255, 254}

type seen struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

type upstream struct {
	*httptest.Server
	mu      sync.Mutex
	last    seen
	hits    atomic.Int32
	handler func(w http.ResponseWriter, r *http.Request) bool
}

func (u *upstream) lastSeen() seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.last = seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), body}
		h := u.handler
		u.mu.Unlock()
		u.hits.Add(1)
		if h != nil && h(w, r) {
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/gzip"):
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			gz.Write([]byte(`{"hello":"compressed"}`))
			gz.Close()
		case strings.HasSuffix(r.URL.Path, "/png"):
			w.Header().Set("Content-Type", "image/png")
			w.Write(png)
		case strings.HasSuffix(r.URL.Path, "/ratelimited"):
			w.Header().Set("X-RateLimit-Remaining", "5")
			w.Header().Set("X-RateLimit-Reset-After", "1")
			w.Header().Set("X-RateLimit-Bucket", "abc")
			w.Header().Set("Access-Control-Allow-Origin", "https://evil.example")
			w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/slow"):
			time.Sleep(300 * time.Millisecond)
			w.Write([]byte(`{}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
		}
	}))
	t.Cleanup(u.Close)
	return u
}

type env struct {
	up       *upstream
	proxy    *httptest.Server
	reg      *registry.Registry
	file     string
	settings config.Settings
}

func baseSettings() config.Settings {
	return config.Settings{
		Port:                  0,
		CORSEnabled:           true,
		CORSOrigins:           []string{"*"},
		LogLevel:              slog.LevelError,
		Timeout:               2 * time.Second,
		StrictTLS:             true,
		ProxyAPIKey:           proxyKey,
		ManagementAPIKey:      manageKey,
		RequireProxyAuth:      true,
		RequireManagementAuth: true,
		DiscordGlobalRPS:      50,
		ServicesFile:          "",
		MaxBodyBytes:          1 << 20,
		Environment:           "local",
	}
}

func newEnv(t *testing.T, mutate ...func(*config.Settings)) *env {
	t.Helper()
	up := newUpstream(t)
	settings := baseSettings()
	for _, m := range mutate {
		m(&settings)
	}
	file := filepath.Join(t.TempDir(), "services.json")
	reg, err := registry.New(file, registry.Builtins("test", up.URL+"/api/", up.URL+"/gh/"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add("fake", registry.Service{Name: "Fake", BaseURL: up.URL + "/fake/", Headers: map[string]string{"User-Agent": "test"}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add("dead", registry.Service{Name: "Dead", BaseURL: "http://127.0.0.1:1/"}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(settings, reg, "2.0.0", logger)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &env{up: up, proxy: ts, reg: reg, file: file, settings: settings}
}

var client = &http.Client{Transport: &http.Transport{DisableCompression: true}}

func (e *env) do(t *testing.T, method, path string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.proxy.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func key(extra map[string]string) map[string]string {
	h := map[string]string{"X-Api-Key": proxyKey}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMultipartForwardedIntact(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("payload_json", `{"content":"hi"}`)
	fw, _ := mw.CreateFormFile("files[0]", "a.png")
	fw.Write(png)
	mw.Close()

	resp := e.do(t, "POST", "/proxy/fake/upload", &buf, key(map[string]string{"Content-Type": mw.FormDataContentType()}))
	got := e.up.lastSeen()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := got.Header.Values("Content-Type"); len(ct) != 1 || ct[0] != mw.FormDataContentType() {
		t.Fatalf("content-type %q", ct)
	}
	if !bytes.Contains(got.Body, png) {
		t.Fatalf("file bytes missing from %d-byte body", len(got.Body))
	}
}

func TestJSONBodyAndMethod(t *testing.T) {
	e := newEnv(t)
	e.do(t, "PATCH", "/proxy/fake/json", strings.NewReader(`{"a":1}`), key(map[string]string{"Content-Type": "application/json"}))
	got := e.up.lastSeen()
	if got.Method != "PATCH" || string(got.Body) != `{"a":1}` {
		t.Fatalf("got %s %q", got.Method, got.Body)
	}
}

func TestBodyWithoutContentTypeDefaultsToJSON(t *testing.T) {
	e := newEnv(t)
	e.do(t, "POST", "/proxy/fake/json", strings.NewReader(`{"a":1}`), key(nil))
	if ct := e.up.lastSeen().Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
}

func TestGzipDecodedAndHeaderStripped(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "GET", "/proxy/fake/gzip", nil, key(nil))
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("content-encoding %q leaked", ce)
	}
	if body := readAll(t, resp.Body); string(body) != `{"hello":"compressed"}` {
		t.Fatalf("body %q", body)
	}
}

func TestBinaryIntact(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "GET", "/proxy/fake/png", nil, key(nil))
	if body := readAll(t, resp.Body); !bytes.Equal(body, png) {
		t.Fatalf("binary changed: %v", body)
	}
	if resp.Header.Get("Content-Type") != "image/png" {
		t.Fatal("content-type lost")
	}
	if resp.Header.Get("Content-Length") != "10" {
		t.Fatalf("content-length %q", resp.Header.Get("Content-Length"))
	}
}

func TestProxySecretsNotForwarded(t *testing.T) {
	e := newEnv(t)
	e.do(t, "GET", "/proxy/fake/q?api_key="+proxyKey+"&x=1&y=a%20b", nil, key(nil))
	got := e.up.lastSeen()
	if got.Query != "x=1&y=a%20b" {
		t.Fatalf("query %q", got.Query)
	}
	if got.Header.Get("X-Api-Key") != "" {
		t.Fatal("x-api-key forwarded")
	}

	e.do(t, "GET", "/proxy/fake/q", nil, map[string]string{"Authorization": "Bearer " + proxyKey})
	if a := e.up.lastSeen().Header.Get("Authorization"); a != "" {
		t.Fatalf("proxy bearer key forwarded as %q", a)
	}
}

func TestCallerAuthorizationForwarded(t *testing.T) {
	e := newEnv(t)
	e.do(t, "GET", "/proxy/fake/me", nil, key(map[string]string{"Authorization": "Bot caller-token"}))
	if a := e.up.lastSeen().Header.Get("Authorization"); a != "Bot caller-token" {
		t.Fatalf("authorization %q", a)
	}
}

func TestUpstreamAuthorizationWins(t *testing.T) {
	e := newEnv(t)
	e.do(t, "GET", "/proxy/fake/me", nil, map[string]string{"Authorization": "Bearer " + proxyKey, "X-Upstream-Authorization": "Bot popplio"})
	got := e.up.lastSeen()
	if got.Header.Get("Authorization") != "Bot popplio" || got.Header.Get("X-Upstream-Authorization") != "" {
		t.Fatalf("headers %v", got.Header)
	}
}

func TestCookiesAndForwardingHeadersStripped(t *testing.T) {
	e := newEnv(t)
	e.do(t, "GET", "/proxy/fake/x", nil, key(map[string]string{"Cookie": "a=b", "X-Forwarded-For": "1.2.3.4", "X-Audit-Log-Reason": "kept"}))
	got := e.up.lastSeen()
	if got.Header.Get("Cookie") != "" || got.Header.Get("X-Forwarded-For") != "" {
		t.Fatalf("leaked headers %v", got.Header)
	}
	if got.Header.Get("X-Audit-Log-Reason") != "kept" {
		t.Fatal("regular header dropped")
	}
}

func TestGETSendsNoBody(t *testing.T) {
	e := newEnv(t)
	e.do(t, "GET", "/proxy/fake/get", nil, key(nil))
	got := e.up.lastSeen()
	if len(got.Body) != 0 || got.Header.Get("Content-Type") != "" {
		t.Fatalf("body %q content-type %q", got.Body, got.Header.Get("Content-Type"))
	}
}

func TestRateLimitHeadersPassThroughButNotUpstreamCORS(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "GET", "/proxy/fake/ratelimited", nil, key(nil))
	if resp.Header.Get("X-RateLimit-Bucket") != "abc" {
		t.Fatal("rate limit header dropped")
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS header %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	if resp.Header.Get("X-Proxy-Service") != "fake" || resp.Header.Get("X-Proxy-Duration") == "" {
		t.Fatal("proxy headers missing")
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	if resp := e.do(t, "GET", "/proxy/fake/x", nil, map[string]string{"X-Api-Key": "wrong"}); resp.StatusCode != 401 {
		t.Fatalf("wrong key: %d", resp.StatusCode)
	}
	if resp := e.do(t, "GET", "/proxy/fake/x", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
	if resp := e.do(t, "GET", "/proxy/fake/x?api_key="+proxyKey, nil, nil); resp.StatusCode != 200 {
		t.Fatalf("query key: %d", resp.StatusCode)
	}
	if resp := e.do(t, "GET", "/manage/services", nil, key(nil)); resp.StatusCode != 401 {
		t.Fatalf("proxy key on manage: %d", resp.StatusCode)
	}
	if resp := e.do(t, "GET", "/health", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("health: %d", resp.StatusCode)
	}
}

func TestAuthDisabled(t *testing.T) {
	e := newEnv(t, func(s *config.Settings) { s.RequireProxyAuth = false })
	if resp := e.do(t, "GET", "/proxy/fake/x", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestErrors(t *testing.T) {
	e := newEnv(t, func(s *config.Settings) { s.Timeout = 100 * time.Millisecond })
	cases := []struct {
		path   string
		status int
	}{
		{"/proxy/nope/x", 404},
		{"/proxy/dead/x", 502},
		{"/proxy/fake/slow", 504},
		{"/nothing-here", 404},
	}
	for _, c := range cases {
		resp := e.do(t, "GET", c.path, nil, key(nil))
		if resp.StatusCode != c.status {
			t.Errorf("%s: got %d want %d", c.path, resp.StatusCode, c.status)
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("%s: not JSON", c.path)
		}
	}
}

func TestBodyTooLarge(t *testing.T) {
	e := newEnv(t, func(s *config.Settings) { s.MaxBodyBytes = 16 })
	resp := e.do(t, "POST", "/proxy/fake/x", strings.NewReader(strings.Repeat("a", 64)), key(nil))
	if resp.StatusCode != 413 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestKeyDebugHasNoKeyMaterial(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/proxy/key-debug", "/manage/key-debug", "/manage/diagnostics"} {
		headers := key(nil)
		if strings.HasPrefix(p, "/manage") {
			headers = map[string]string{"X-Api-Key": manageKey}
		}
		body := string(readAll(t, e.do(t, "GET", p, nil, headers).Body))
		if strings.Contains(body, "test-") || strings.Contains(body, "123") || strings.Contains(body, "456") {
			t.Errorf("%s leaks key material: %s", p, body)
		}
	}
	body := string(readAll(t, e.do(t, "GET", "/auth-status?api_key=secret-value", nil, nil).Body))
	if strings.Contains(body, "secret-value") {
		t.Errorf("auth-status echoes api_key: %s", body)
	}
}

func TestDiscordPathShaping(t *testing.T) {
	e := newEnv(t)
	for path, want := range map[string]string{
		"/proxy/discord/v10/users/@me":                                    "/api/v10/users/@me",
		"/proxy/discord/api/v10/users/@me":                                "/api/v10/users/@me",
		"/proxy/discord/v9/channels/1/messages":                           "/api/v9/channels/1/messages",
		"/proxy/discord/channels/1/messages/2/reactions/%F0%9F%91%8D/@me": "/api/channels/1/messages/2/reactions/%F0%9F%91%8D/@me",
	} {
		e.do(t, "GET", path, nil, key(map[string]string{"Authorization": "Bot x"}))
		if got := e.up.lastSeen().Path; got != want {
			t.Errorf("%s -> %s, want %s", path, got, want)
		}
	}
	if ua := e.up.lastSeen().Header.Get("User-Agent"); !strings.HasPrefix(ua, "DiscordBot (") {
		t.Errorf("user-agent %q", ua)
	}
}

func TestDiscord429Retried(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	e.up.handler = func(w http.ResponseWriter, r *http.Request) bool {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0.05")
			w.WriteHeader(429)
			w.Write([]byte(`{"retry_after":0.05,"global":false}`))
			return true
		}
		return false
	}
	resp := e.do(t, "POST", "/proxy/discord/v10/channels/1/messages", strings.NewReader(`{"content":"x"}`), key(map[string]string{"Authorization": "Bot x"}))
	if resp.StatusCode != 200 || calls.Load() != 2 {
		t.Fatalf("status %d after %d calls", resp.StatusCode, calls.Load())
	}
	if string(e.up.lastSeen().Body) != `{"content":"x"}` {
		t.Fatal("retry lost the body")
	}
}

func TestDiscordLong429ReturnedToCaller(t *testing.T) {
	e := newEnv(t)
	e.up.handler = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
		w.Write([]byte(`{"retry_after":3600}`))
		return true
	}
	start := time.Now()
	resp := e.do(t, "GET", "/proxy/discord/v10/users/@me", nil, key(map[string]string{"Authorization": "Bot x"}))
	if resp.StatusCode != 429 || time.Since(start) > 5*time.Second {
		t.Fatalf("status %d after %v", resp.StatusCode, time.Since(start))
	}
	if body := readAll(t, resp.Body); !strings.Contains(string(body), "3600") {
		t.Fatalf("body %q", body)
	}
}

func TestGitHubOrgHint(t *testing.T) {
	e := newEnv(t)
	if resp := e.do(t, "GET", "/proxy/github/org/foo", nil, key(nil)); resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestWindowRateLimit(t *testing.T) {
	e := newEnv(t)
	if err := e.reg.Add("limited", registry.Service{Name: "Limited", BaseURL: e.up.URL + "/", RateLimit: &registry.RateLimit{MaxRequests: 2, WindowMs: 60000}}); err != nil {
		t.Fatal(err)
	}
	statuses := []int{}
	for i := 0; i < 3; i++ {
		statuses = append(statuses, e.do(t, "GET", "/proxy/limited/x", nil, key(nil)).StatusCode)
	}
	if statuses[0] != 200 || statuses[1] != 200 || statuses[2] != 429 {
		t.Fatalf("statuses %v", statuses)
	}
	resp := e.do(t, "GET", "/proxy/services/limited/rate-limit", nil, key(nil))
	var body map[string]any
	json.Unmarshal(readAll(t, resp.Body), &body)
	if body["remaining"] != float64(0) {
		t.Fatalf("status %v", body)
	}
}

func TestManagementLifecycleAndPersistence(t *testing.T) {
	e := newEnv(t)
	h := map[string]string{"X-Api-Key": manageKey, "Content-Type": "application/json"}

	bad := `{"key":"steal","config":{"name":"x","baseUrl":"https://evil.example/","auth":{"type":"bearer","tokenEnvVar":"DATABASE_URL"}}}`
	if resp := e.do(t, "POST", "/manage/services", strings.NewReader(bad), h); resp.StatusCode != 400 {
		t.Fatalf("disallowed env var accepted: %d", resp.StatusCode)
	}
	badHeader := `{"key":"hdr","config":{"name":"x","baseUrl":"https://example.com/","headers":{"Authorization":"Bearer x"}}}`
	if resp := e.do(t, "POST", "/manage/services", strings.NewReader(badHeader), h); resp.StatusCode != 400 {
		t.Fatalf("static Authorization accepted: %d", resp.StatusCode)
	}

	t.Setenv("BYTEPROXY_TOKEN_STATUS", "server-side-token")
	good := `{"key":"status","config":{"name":"Status","baseUrl":"` + e.up.URL + `/status/","auth":{"type":"bearer","tokenEnvVar":"BYTEPROXY_TOKEN_STATUS"}}}`
	if resp := e.do(t, "POST", "/manage/services", strings.NewReader(good), h); resp.StatusCode != 200 {
		t.Fatalf("add: %d %s", resp.StatusCode, readAll(t, resp.Body))
	}
	if resp := e.do(t, "POST", "/manage/services", strings.NewReader(good), h); resp.StatusCode != 409 {
		t.Fatalf("duplicate: %d", resp.StatusCode)
	}

	e.do(t, "GET", "/proxy/status/ping", nil, key(nil))
	if a := e.up.lastSeen().Header.Get("Authorization"); a != "Bearer server-side-token" {
		t.Fatalf("server token not applied: %q", a)
	}
	e.do(t, "GET", "/proxy/status/ping", nil, key(map[string]string{"X-Upstream-Authorization": "Bearer caller"}))
	if a := e.up.lastSeen().Header.Get("Authorization"); a != "Bearer caller" {
		t.Fatalf("caller credential should win: %q", a)
	}

	data, err := os.ReadFile(e.file)
	if err != nil || !strings.Contains(string(data), `"status"`) {
		t.Fatalf("not persisted: %v %s", err, data)
	}
	reloaded, err := registry.New(e.file, registry.Builtins("test", "https://a/", "https://b/"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Get("status"); !ok {
		t.Fatal("service not reloaded from disk")
	}

	if resp := e.do(t, "DELETE", "/manage/services/discord", nil, h); resp.StatusCode != 400 {
		t.Fatalf("builtin removal: %d", resp.StatusCode)
	}
	if resp := e.do(t, "DELETE", "/manage/services/status", nil, h); resp.StatusCode != 200 {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	if resp := e.do(t, "DELETE", "/manage/services/status", nil, h); resp.StatusCode != 404 {
		t.Fatalf("remove again: %d", resp.StatusCode)
	}
}

func TestCORS(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "OPTIONS", "/proxy/fake/x", nil, map[string]string{"Origin": "https://app.example", "Access-Control-Request-Method": "POST"})
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight %d %v", resp.StatusCode, resp.Header)
	}
	if resp.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("wildcard origin must not allow credentials")
	}
	if !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "X-Upstream-Authorization") {
		t.Fatal("X-Upstream-Authorization not allowed")
	}

	e2 := newEnv(t, func(s *config.Settings) { s.CORSOrigins = []string{"https://app.example"} })
	resp = e2.do(t, "GET", "/health", nil, map[string]string{"Origin": "https://app.example"})
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example" || resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("allowlisted origin %v", resp.Header)
	}
	resp = e2.do(t, "GET", "/health", nil, map[string]string{"Origin": "https://evil.example"})
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unlisted origin allowed")
	}
}

func TestOpenAPICoversEveryRoute(t *testing.T) {
	var spec struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal([]byte(openAPISpec), &spec); err != nil {
		t.Fatalf("openapi.json is invalid JSON: %v", err)
	}
	if spec.OpenAPI != "3.1.0" {
		t.Fatalf("openapi version %q", spec.OpenAPI)
	}
	s := New(baseSettings(), mustRegistry(t), "2.0.0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, r := range s.routeTable() {
		method, path, hasMethod := strings.Cut(r.pattern, " ")
		if !hasMethod {
			method, path = "", r.pattern
		}
		if path == "/" || strings.HasSuffix(path, "/{$}") && path != "/{$}" {
			continue
		}
		path = strings.ReplaceAll(strings.ReplaceAll(path, "{$}", ""), "...}", "}")
		if path == "" {
			path = "/"
		}
		ops, ok := spec.Paths[path]
		if !ok {
			t.Errorf("route %q is not documented in openapi.json", r.pattern)
			continue
		}
		if method != "" {
			if _, ok := ops[strings.ToLower(method)]; !ok {
				t.Errorf("route %q: method %s not documented", r.pattern, method)
			}
		}
	}
}

func mustRegistry(t *testing.T) *registry.Registry {
	reg, err := registry.New("", registry.Builtins("test", "https://discord.com/api/", "https://api.github.com/"))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestDocsServed(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "GET", "/openapi.json", nil, nil)
	body := readAll(t, resp.Body)
	if resp.StatusCode != 200 || !json.Valid(body) || !bytes.Contains(body, []byte(`"version": "2.0.0"`)) {
		t.Fatalf("openapi.json %d", resp.StatusCode)
	}
	if resp := e.do(t, "GET", "/docs", nil, nil); resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("docs %d", resp.StatusCode)
	}
}
