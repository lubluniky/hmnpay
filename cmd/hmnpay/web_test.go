package main

import (
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lubluniky/hmnpay"
	"github.com/lubluniky/hmnpay/core/coretest"
	"github.com/lubluniky/hmnpay/internal/webui"
)

// withUI makes the server under test believe ui is what was built into
// the binary.
func withUI(t *testing.T, ui fs.FS) {
	t.Helper()
	old := embeddedUI
	embeddedUI = func() fs.FS { return ui }
	t.Cleanup(func() { embeddedUI = old })
}

const testInline = `self.__next_f=[];`

func testUI() fstest.MapFS {
	page := func(title string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(`<!DOCTYPE html><html><body><h1>` + title + `</h1><script>` + testInline + `</script><script src="/_next/static/chunks/app.js"></script></body></html>`)}
	}
	return fstest.MapFS{
		"index.html":                 page("dashboard"),
		"404.html":                   page("no such page"),
		"setup.html":                 page("setup wizard"),
		"login.html":                 page("sign in"),
		"pay/_.html":                 page("checkout"),
		"l/_.html":                   page("payment link"),
		"payments/_.html":            page("payment detail"),
		"api.html":                   page("a page that must never be served"),
		"healthz.html":               page("a page that must never be served"),
		"_next/static/chunks/app.js": {Data: []byte("console.log(1)")},
	}
}

// The binary with the interface built in: one port serves the pages, the
// hashed assets and the API; API paths never reach the interface, and the
// interface never weakens what the API answers.
func TestServeEmbeddedUI(t *testing.T) {
	withUI(t, testUI())
	addr := freeAddr(t)
	cfg, err := parseServeConfig([]string{"--listen", addr, "--data-dir", t.TempDir(), "--shutdown-timeout", "10s"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var logs, console safeBuffer
	sigs := make(chan os.Signal, 2)
	done := make(chan int, 1)
	go func() { done <- serveWith(cfg, slog.New(slog.NewTextHandler(&logs, nil)), &console, sigs) }()
	base := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for run([]string{"healthcheck", "--url", base + "/healthz", "--timeout", "500ms"}, nil, io.Discard, io.Discard) != exitOK {
		if time.Now().After(deadline) {
			t.Fatalf("server did not become healthy; log:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "web interface: built in") {
		t.Errorf("the log does not say the interface is built in:\n%s", logs.String())
	}

	fetch := func(method, path string, hdr ...string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			if hdr[i] == "Host" {
				req.Host = hdr[i+1]
				continue
			}
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, string(body)
	}

	// Pages, with the dynamic routes falling back to their one export.
	for path, want := range map[string]string{
		"/":                                   "dashboard",
		"/setup":                              "setup wizard",
		"/login":                              "sign in",
		"/pay/pay_0123456789abcdefghjkmnpqrs": "checkout",
		"/l/spring-sale":                      "payment link",
		"/payments/pay_0123456789abcdefghjkmnpqrs": "payment detail",
	} {
		res, body := fetch("GET", path)
		if res.StatusCode != 200 || !strings.Contains(body, "<h1>"+want+"</h1>") {
			t.Errorf("GET %s = %d %q", path, res.StatusCode, body)
		}
		if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s: Cache-Control %q", path, cc)
		}
		csp := res.Header.Get("Content-Security-Policy")
		if hashes := webui.ScriptHashes([]byte(body)); len(hashes) != 1 || !strings.Contains(csp, "'"+hashes[0]+"'") ||
			strings.Contains(csp, "unsafe-eval") || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "connect-src 'self'") {
			t.Errorf("GET %s: policy %q does not match the page's inline script", path, csp)
		}
		if res.Header.Get("X-Frame-Options") != "DENY" || res.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("GET %s lacks the UI security headers: %v", path, res.Header)
		}
	}
	if res, _ := fetch("GET", "/_next/static/chunks/app.js"); res.StatusCode != 200 || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("asset = %d, Cache-Control %q", res.StatusCode, res.Header.Get("Cache-Control"))
	}
	if res, body := fetch("GET", "/no/such/page"); res.StatusCode != 404 || !strings.Contains(body, "no such page") {
		t.Errorf("unknown page = %d %q", res.StatusCode, body)
	}

	// API paths never fall through to the interface: known ones answer as
	// before, unknown ones are a JSON 404 with the API's own headers.
	for _, path := range []string{"/api", "/api/", "/api/v1", "/api/v1/nope", "/api/v2/payments", "/api/v1/payments/pay_x/nope", "/healthz/x"} {
		res, body := fetch("GET", path)
		if path == "/healthz/x" {
			// Not an API path: it is the interface's to refuse.
			if res.StatusCode != 404 {
				t.Errorf("GET %s = %d", path, res.StatusCode)
			}
			continue
		}
		var e struct {
			Error struct{ Code string } `json:"error"`
		}
		if res.StatusCode != 404 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") ||
			json.Unmarshal([]byte(body), &e) != nil || e.Error.Code != "not_found" {
			t.Errorf("GET %s = %d %s %q; want a JSON 404", path, res.StatusCode, res.Header.Get("Content-Type"), body)
		}
		if csp := res.Header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'none'") {
			t.Errorf("GET %s: API policy %q", path, csp)
		}
	}
	if res, body := fetch("GET", "/healthz"); res.StatusCode != 200 || !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("/healthz = %d %q", res.StatusCode, body)
	}
	if res, body := fetch("GET", "/api/v1/setup"); res.StatusCode != 200 || !strings.Contains(body, `"setup_token_required":true`) {
		t.Errorf("/api/v1/setup = %d %q", res.StatusCode, body)
	}
	if res, body := fetch("GET", "/api/v1/payments"); res.StatusCode != 401 || !strings.Contains(body, `"unauthorized"`) {
		t.Errorf("/api/v1/payments without a session = %d %q", res.StatusCode, body)
	}
	if res, _ := fetch("DELETE", "/api/v1/setup"); res.StatusCode != 405 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		t.Errorf("DELETE /api/v1/setup = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	// Host validation still guards the API.
	if res, _ := fetch("GET", "/api/v1/setup", "Host", "rebind.attacker.example"); res.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("foreign Host on the API = %d; want 421", res.StatusCode)
	}
	// The interface takes no writes.
	if res, _ := fetch("POST", "/login"); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /login = %d", res.StatusCode)
	}

	sigs <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("exit code %d; log:\n%s", code, logs.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestWebHandlerModes(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	withUI(t, fstest.MapFS{".placeholder": {Data: []byte("x")}})
	if _, mode, err := webHandler("", false, nil, log); err != nil || mode != webNone {
		t.Errorf("placeholder only: mode %q, %v", mode, err)
	}
	withUI(t, testUI())
	if _, mode, err := webHandler("", false, nil, log); err != nil || mode != webEmbedded {
		t.Errorf("built interface: mode %q, %v", mode, err)
	}
	// The development proxy wins over a built-in interface.
	if _, mode, err := webHandler("http://127.0.0.1:3000", false, nil, log); err != nil || mode != webProxy {
		t.Errorf("upstream: mode %q, %v", mode, err)
	}
}

// HMNPAY_WEB_UPSTREAM is a development switch. What it forwards and what it
// lets back is bounded: the upstream gets no credential of the instance,
// cannot set a cookie on its origin, and its pages are sent with the
// server's own security headers.
func TestUpstreamProxyKeepsCredentialsAndSetsThePolicy(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Add("Set-Cookie", "hmn_session=planted-by-upstream; Path=/")
		h.Add("Set-Cookie", "__Host-hmn_session=planted; Path=/; Secure")
		h.Set("Content-Security-Policy", "default-src *")
		h.Set("X-Frame-Options", "ALLOWALL")
		h.Set("Referrer-Policy", "unsafe-url")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Clear-Site-Data", `"cookies"`)
		h.Set("X-Upstream", "kept")
		io.WriteString(w, "<script>document.title='upstream'</script>")
	}))
	defer upstream.Close()
	p, err := newUpstreamProxy(upstream.URL, false, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	defer front.Close()

	req, _ := http.NewRequest("POST", front.URL+"/payments?x=1", strings.NewReader("body"))
	for k, v := range map[string]string{
		"Cookie":               "hmn_session=hmns_THE_OPERATOR_SESSION_TOKEN; hmn_device=marker; theme=dark",
		"Authorization":        "Bearer hmn_AN_API_KEY",
		"Proxy-Authorization":  "Basic eDp5",
		"X-Hmnpay-Setup-Token": "the-setup-token",
		"Idempotency-Key":      "key-1",
		"Origin":               "https://pay.example.com",
		"Referer":              "https://pay.example.com/pay/pay_01hzy0000000000000000000000",
		"Sec-Fetch-Site":       "same-origin",
		"Sec-Fetch-Mode":       "cors",
		"Sec-Fetch-Dest":       "empty",
		"Accept":               "text/html",
		"X-Requested-With":     "kept",
	} {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "upstream") {
		t.Fatalf("proxied answer: %d %q", res.StatusCode, body)
	}

	// Nothing that authenticates to this instance, or that its API reads to
	// tell a same-origin browser request, goes to the upstream.
	for _, k := range []string{"Cookie", "Authorization", "Proxy-Authorization", "X-Hmnpay-Setup-Token", "Idempotency-Key", "Origin", "Referer", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest"} {
		if v := seen.Values(k); len(v) != 0 {
			t.Errorf("the upstream received %s: %q", k, v)
		}
	}
	if seen.Get("Accept") != "text/html" || seen.Get("X-Requested-With") != "kept" {
		t.Errorf("ordinary request headers were not forwarded: %v", seen)
	}
	// The upstream cannot set or clear cookies on this origin, open it to
	// other origins, or choose its policy.
	h := res.Header
	for _, k := range []string{"Set-Cookie", "Clear-Site-Data", "Access-Control-Allow-Origin", "Access-Control-Allow-Credentials"} {
		if v := h.Values(k); len(v) != 0 {
			t.Errorf("the upstream's %s reached the browser: %q", k, v)
		}
	}
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'", "form-action 'self'", "frame-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("development policy lacks %q: %s", want, csp)
		}
	}
	if len(h.Values("Content-Security-Policy")) != 1 || strings.Contains(csp, "*") || strings.Contains(csp, "https:") && !strings.Contains(csp, "img-src") {
		t.Errorf("the upstream's policy was kept or merged: %q", h.Values("Content-Security-Policy"))
	}
	// Scripts and connections stay on this origin (the development server
	// needs inline scripts and eval, which the built interface does not get).
	for _, directive := range strings.Split(csp, ";") {
		directive = strings.TrimSpace(directive)
		if strings.HasPrefix(directive, "script-src") || strings.HasPrefix(directive, "connect-src") || strings.HasPrefix(directive, "default-src") {
			for _, source := range strings.Fields(directive)[1:] {
				ok := source == "'self'" || source == "'unsafe-inline'" || source == "'unsafe-eval'" ||
					strings.HasPrefix(source, "ws://"+req.Host) || strings.HasPrefix(source, "wss://"+req.Host)
				if !ok {
					t.Errorf("%s allows %q", strings.Fields(directive)[0], source)
				}
			}
		}
	}
	for k, want := range map[string]string{
		"X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff",
		"Cross-Origin-Opener-Policy": "same-origin", "Permissions-Policy": webui.PermissionsPolicy,
	} {
		if got := h.Values(k); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if h.Get("X-Upstream") != "kept" {
		t.Error("an ordinary response header of the upstream was dropped")
	}
}

// Only a loopback upstream is taken without saying more: the upstream is
// trusted with every page of the instance's origin.
func TestUpstreamMustBeLoopbackUnlessAllowed(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	for _, ok := range []string{"http://127.0.0.1:3000", "http://localhost:3000", "http://[::1]:3000", "http://127.9.9.9:3000", "http://LOCALHOST:3000", "https://localhost"} {
		if _, err := newUpstreamProxy(ok, false, log); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, remote := range []string{"http://web:3000", "http://192.168.1.20:3000", "http://10.0.0.5:3000", "https://dev.example.com", "http://localhost.example.com:3000", "http://127.0.0.1.example.com:3000", "http://0.0.0.0:3000", "http://[::]:3000"} {
		if _, err := newUpstreamProxy(remote, false, log); err == nil || !strings.Contains(err.Error(), "HMNPAY_WEB_UPSTREAM_ALLOW_REMOTE") {
			t.Errorf("%s accepted without the second switch (error: %v)", remote, err)
		}
		if _, err := newUpstreamProxy(remote, true, log); err != nil {
			t.Errorf("%s refused although remote upstreams were allowed: %v", remote, err)
		}
	}
	// The configuration says so before anything starts.
	if _, err := parseServeConfig([]string{"--data-dir", t.TempDir()}, env(map[string]string{"HMNPAY_WEB_UPSTREAM": "http://web:3000"}), io.Discard); err == nil || !strings.Contains(err.Error(), "HMNPAY_WEB_UPSTREAM_ALLOW_REMOTE") {
		t.Errorf("a remote upstream passed the configuration check: %v", err)
	}
	cfg, err := parseServeConfig([]string{"--data-dir", t.TempDir()}, env(map[string]string{"HMNPAY_WEB_UPSTREAM": "http://web:3000", "HMNPAY_WEB_UPSTREAM_ALLOW_REMOTE": "true"}), io.Discard)
	if err != nil || !cfg.WebUpstreamRemote {
		t.Errorf("remote upstream with the second switch: %v", err)
	}
}

// The server with HMNPAY_WEB_UPSTREAM: it says at start that this is a
// development mode, and the Host rule covers the proxied pages.
func TestServeWithUpstreamWarnsAndChecksHost(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		io.WriteString(w, "dev server")
	}))
	defer upstream.Close()
	addr := freeAddr(t)
	cfg, err := parseServeConfig([]string{"--listen", addr, "--data-dir", t.TempDir(), "--shutdown-timeout", "10s", "--web-upstream", upstream.URL}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var logs, console safeBuffer
	sigs := make(chan os.Signal, 2)
	done := make(chan int, 1)
	go func() { done <- serveWith(cfg, slog.New(slog.NewTextHandler(&logs, nil)), &console, sigs) }()
	base := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for run([]string{"healthcheck", "--url", base + "/healthz", "--timeout", "500ms"}, nil, io.Discard, io.Discard) != exitOK {
		if time.Now().After(deadline) {
			t.Fatalf("server did not become healthy; log:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() {
		sigs <- syscall.SIGTERM
		<-done
	}()
	for _, want := range []string{"level=WARN", "HMNPAY_WEB_UPSTREAM", "development", "must not be used in production"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the start-up log does not contain %q:\n%s", want, logs.String())
		}
	}
	get := func(path, host string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", base+path, nil)
		if host != "" {
			req.Host = host
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := get("/payments", ""); code != http.StatusOK || hits != 1 {
		t.Errorf("proxied page: %d, upstream hits %d", code, hits)
	}
	if code := get("/payments", "rebind.attacker.example"); code != http.StatusMisdirectedRequest || hits != 1 {
		t.Errorf("proxied page under a foreign Host: %d, upstream hits %d (want 421 and no hit)", code, hits)
	}
}

// A binary without the interface tells anonymous visitors that it runs and
// how to set it up, not which version it is (the API does not either).
func TestStatusPageDoesNotTellTheVersion(t *testing.T) {
	old := hmnpay.Version
	hmnpay.Version = "9.9.9-test"
	t.Cleanup(func() { hmnpay.Version = old })
	e := coretest.NewEnv(t)
	w := httptest.NewRecorder()
	statusHandler(e.Service, slog.New(slog.DiscardHandler)).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "hmnpay is running") {
		t.Fatalf("status page: %d %q", w.Code, body)
	}
	if strings.Contains(body, "9.9.9-test") || strings.Contains(strings.ToLower(body), "version") {
		t.Errorf("the status page tells the version:\n%s", body)
	}
}

// Whatever serves the interface, the licence, the additional terms and the
// third-party notices are served next to it.
func TestEveryWebModeServesTheNotices(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "dev server") }))
	defer upstream.Close()
	env := coretest.NewEnv(t)
	modes := map[string]func() (http.Handler, webMode, error){
		"built in": func() (http.Handler, webMode, error) {
			withUI(t, testUI())
			return webHandler("", false, env.Service, log)
		},
		"none": func() (http.Handler, webMode, error) {
			withUI(t, fstest.MapFS{})
			return webHandler("", false, env.Service, log)
		},
		"proxy": func() (http.Handler, webMode, error) { return webHandler(upstream.URL, false, env.Service, log) },
	}
	for name, build := range modes {
		h, _, err := build()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for path, text := range map[string]string{noticesPath: hmnpay.ThirdPartyNotices, licensePath: hmnpay.License, additionalTermsPath: hmnpay.AdditionalTerms} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != http.StatusOK || w.Body.String() != text {
				t.Errorf("%s: GET %s = %d, %d bytes", name, path, w.Code, w.Body.Len())
			}
		}
	}
}
