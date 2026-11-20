package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lubluniky/hmnpay"
	"github.com/lubluniky/hmnpay/core"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseServeConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		c, err := parseServeConfig(nil, env(nil), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if c.Listen != ":8080" || c.DataDir != "./data" || c.LogLevel != "info" || c.LogFormat != "text" ||
			c.WebhookPrivate || c.ChainVerify != "auto" || c.ShutdownTimeout != 30*time.Second || c.WebUpstream != "" ||
			c.PollInterval != 3*time.Second || c.IdlePollInterval != 30*time.Second {
			t.Fatalf("defaults = %+v", c)
		}
	})
	t.Run("poll intervals", func(t *testing.T) {
		for _, tc := range []struct {
			poll, idle         string
			wantPoll, wantIdle time.Duration // wantIdle < 0: idle polling off
		}{
			{"", "", 3 * time.Second, 30 * time.Second},
			{"1s", "5m", time.Second, 5 * time.Minute},
			{"30s", "45s", 30 * time.Second, 45 * time.Second},
			{"6s", "off", 6 * time.Second, -1},
			{"", "OFF", 3 * time.Second, -1},
			{"5s", "5s", 5 * time.Second, -1}, // the same pause: off
			{"", "3s", 3 * time.Second, -1},
			{"2500ms", "1m", 2500 * time.Millisecond, time.Minute},
		} {
			e := map[string]string{}
			if tc.poll != "" {
				e["HMNPAY_POLL_INTERVAL"] = tc.poll
			}
			if tc.idle != "" {
				e["HMNPAY_IDLE_POLL_INTERVAL"] = tc.idle
			}
			c, err := parseServeConfig(nil, env(e), io.Discard)
			if err != nil || c.PollInterval != tc.wantPoll || (tc.wantIdle < 0) != (c.IdlePollInterval < 0) || (tc.wantIdle >= 0 && c.IdlePollInterval != tc.wantIdle) {
				t.Errorf("poll %q, idle %q: poll %s, idle %s, err %v; want %s and %s", tc.poll, tc.idle, c.PollInterval, c.IdlePollInterval, err, tc.wantPoll, tc.wantIdle)
			}
		}
		// Flags win over the environment.
		c, err := parseServeConfig([]string{"--poll-interval", "2s", "--idle-poll-interval", "off"},
			env(map[string]string{"HMNPAY_POLL_INTERVAL": "10s", "HMNPAY_IDLE_POLL_INTERVAL": "1m"}), io.Discard)
		if err != nil || c.PollInterval != 2*time.Second || c.IdlePollInterval >= 0 {
			t.Errorf("flags: poll %s, idle %s, err %v", c.PollInterval, c.IdlePollInterval, err)
		}
		for _, bad := range [][2]string{
			{"HMNPAY_POLL_INTERVAL", "999ms"}, {"HMNPAY_POLL_INTERVAL", "31s"}, {"HMNPAY_POLL_INTERVAL", "0"},
			{"HMNPAY_POLL_INTERVAL", "off"}, {"HMNPAY_POLL_INTERVAL", "fast"}, {"HMNPAY_POLL_INTERVAL", "-3s"},
			{"HMNPAY_IDLE_POLL_INTERVAL", "2s"}, // shorter than the default poll interval
			{"HMNPAY_IDLE_POLL_INTERVAL", "301s"}, {"HMNPAY_IDLE_POLL_INTERVAL", "0"},
			{"HMNPAY_IDLE_POLL_INTERVAL", "-30s"}, {"HMNPAY_IDLE_POLL_INTERVAL", "never"},
		} {
			if _, err := parseServeConfig(nil, env(map[string]string{bad[0]: bad[1]}), io.Discard); err == nil || !strings.Contains(err.Error(), bad[0]) {
				t.Errorf("%s=%s: err = %v; want one naming the variable", bad[0], bad[1], err)
			}
		}
		// An idle interval below a poll interval that was raised.
		if _, err := parseServeConfig(nil, env(map[string]string{"HMNPAY_POLL_INTERVAL": "20s", "HMNPAY_IDLE_POLL_INTERVAL": "10s"}), io.Discard); err == nil ||
			!strings.Contains(err.Error(), "HMNPAY_IDLE_POLL_INTERVAL") {
			t.Errorf("idle below poll: err = %v", err)
		}
		// The default idle interval (30s) with the poll interval at its
		// maximum (30s): the same pause, so idle polling is off, not an
		// error.
		if c, err := parseServeConfig(nil, env(map[string]string{"HMNPAY_POLL_INTERVAL": "30s"}), io.Discard); err != nil || c.IdlePollInterval >= 0 {
			t.Errorf("poll 30s with the default idle interval: idle %s, err %v", c.IdlePollInterval, err)
		}
	})
	t.Run("environment, then flags", func(t *testing.T) {
		e := env(map[string]string{
			"HMNPAY_LISTEN": "127.0.0.1:9000", "HMNPAY_DATA_DIR": "/srv/hmnpay", "HMNPAY_PUBLIC_URL": "https://pay.example.com/",
			"HMNPAY_TRUSTED_PROXIES": "10.0.0.0/8, 127.0.0.1", "HMNPAY_WEBHOOK_ALLOW_PRIVATE": "true",
			"HMNPAY_WEBHOOK_ALLOWED_PREFIXES": "127.0.0.1/32", "HMNPAY_LOG_FORMAT": "json", "HMNPAY_CHAIN_VERIFY": "strict",
			"HMNPAY_WEB_UPSTREAM": "http://localhost:3000", "HMNPAY_CORS_ORIGINS": "https://shop.example.com",
		})
		c, err := parseServeConfig([]string{"--listen", ":7000", "--webhook-allow-private=false"}, e, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if c.Listen != ":7000" || c.DataDir != "/srv/hmnpay" || c.PublicURL != "https://pay.example.com" || c.WebhookPrivate ||
			len(c.TrustedProxies) != 2 || c.LogFormat != "json" || c.ChainVerify != "strict" || c.WebUpstream != "http://localhost:3000" || c.WebUpstreamRemote ||
			len(c.CORSOrigins) != 1 || len(c.WebhookPrefixes) != 1 {
			t.Fatalf("config = %+v", c)
		}
		c, err = parseServeConfig([]string{"--allowed-hosts", "pay.internal, lb-check", "--max-head-time-ahead", "off",
			"--setup-token", "k3J9-qZ7x-Lm2P-vR8t-Yw4N"}, env(nil), io.Discard)
		if err != nil || len(c.AllowedHosts) != 2 || c.MaxHeadTimeAhead >= 0 || c.SetupToken != "k3J9-qZ7x-Lm2P-vR8t-Yw4N" {
			t.Fatalf("config = %+v, %v", c, err)
		}
	})
	for name, kv := range map[string][2]string{
		"bool":     {"HMNPAY_PUBLIC_CORS", "maybe"},
		"level":    {"HMNPAY_LOG_LEVEL", "loud"},
		"format":   {"HMNPAY_LOG_FORMAT", "xml"},
		"verify":   {"HMNPAY_CHAIN_VERIFY", "paranoid"},
		"proxies":  {"HMNPAY_TRUSTED_PROXIES", "not-an-ip"},
		"prefixes": {"HMNPAY_WEBHOOK_ALLOWED_PREFIXES", "10.0.0.0/33"},
		"upstream": {"HMNPAY_WEB_UPSTREAM", "http://user:pw@web:3000/app"},
		"timeout":  {"HMNPAY_SHUTDOWN_TIMEOUT", "soon"},
		"token":    {"HMNPAY_SETUP_TOKEN", "hunter2"},
		"hosts":    {"HMNPAY_ALLOWED_HOSTS", "https://pay.example.com"},
		"ahead":    {"HMNPAY_MAX_HEAD_TIME_AHEAD", "0"},
	} {
		t.Run("invalid "+name, func(t *testing.T) {
			if _, err := parseServeConfig(nil, env(map[string]string{kv[0]: kv[1]}), io.Discard); err == nil || !strings.Contains(err.Error(), kv[0]) {
				t.Fatalf("err = %v; want one naming %s", err, kv[0])
			}
		})
	}
	if code := run([]string{"serve", "--log-level", "loud"}, nil, io.Discard, io.Discard); code != exitUsage {
		t.Fatalf("invalid flag exit code = %d", code)
	}
	if code := run([]string{"frobnicate"}, nil, io.Discard, io.Discard); code != exitUsage {
		t.Fatalf("unknown command exit code = %d", code)
	}
}

func TestHelpers(t *testing.T) {
	for listen, want := range map[string]string{
		":8080": "http://127.0.0.1:8080/healthz", "0.0.0.0:9000": "http://127.0.0.1:9000/healthz",
		"[::]:9000": "http://127.0.0.1:9000/healthz", "10.1.2.3:80": "http://10.1.2.3:80/healthz",
	} {
		if got := healthURL(listen); got != want {
			t.Errorf("healthURL(%q) = %s", listen, got)
		}
	}
	if got := redactURL("https://user:secret@rpc.example.com/v1/KEY?token=abc"); strings.Contains(got, "secret") || strings.Contains(got, "abc") {
		t.Errorf("redactURL leaked: %s", got)
	}
	p, err := generatePassword()
	if err != nil || len(p) != 24 || len(p) < core.MinPasswordLength {
		t.Fatalf("generated %q, %v", p, err)
	}
	if q, _ := generatePassword(); q == p {
		t.Fatal("two generated passwords are equal")
	}
	var out bytes.Buffer
	if run([]string{"version"}, nil, &out, io.Discard) != exitOK || !strings.HasPrefix(out.String(), "hmnpay ") {
		t.Fatalf("version = %q", out.String())
	}
}

func TestWebUpstreamProxy(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		io.WriteString(w, "next.js page "+r.URL.Path)
	}))
	defer upstream.Close()
	p, err := newUpstreamProxy(upstream.URL, false, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	defer front.Close()
	req, _ := http.NewRequest("GET", front.URL+"/dashboard/payments", nil)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "next.js page /dashboard/payments" {
		t.Fatalf("proxied body = %q", body)
	}
	if xff := seen.Get("X-Forwarded-For"); xff != "127.0.0.1" {
		t.Errorf("upstream saw X-Forwarded-For %q; client-supplied values must be replaced", xff)
	}
	if h := seen.Get("X-Forwarded-Host"); h == "evil.example" {
		t.Errorf("upstream saw a spoofed X-Forwarded-Host")
	}
	if res.Header.Get("X-Frame-Options") != "DENY" || res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("proxied response lacks the UI security headers: %v", res.Header)
	}

	upstream.Close()
	res, err = http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream down = %d", res.StatusCode)
	}
}

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// The whole binary: start, serve the status page and the API, pass its own
// health check, shut down on SIGTERM with exit code 0.
func TestServeLifecycle(t *testing.T) {
	// This test is about the binary without an interface, whatever the
	// test binary happens to have embedded.
	withUI(t, fstest.MapFS{})
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
	res, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(page), "Setup required") || !strings.Contains(string(page), "setup token") ||
		!strings.Contains(string(page), "This build has no web interface") ||
		!strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("status page = %s", page)
	}
	// The first-run banner went to the console, with the token — before the
	// server answered anything, so it is there once the health check passes;
	// the structured log mentions setup but never the token, and neither does
	// the status page.
	token := bannerToken(t, console.String())
	if !strings.Contains(console.String(), "http://"+addr+"/setup") {
		t.Errorf("banner lacks the setup URL:\n%s", console.String())
	}
	if strings.Contains(logs.String(), token) || strings.Contains(string(page), token) {
		t.Fatal("the setup token leaked into the log or the status page")
	}
	// Setup over HTTP: no token 401, wrong token 403, the printed one works.
	setupBody := func(tok string) string {
		return `{"email":"owner@example.com","password":"correct horse battery staple","chain_id":56,` +
			`"rpc_urls":["https://bsc-rpc.publicnode.com"],"xpub":"xpub6Ce9NcJvTk36xtLSrJLZqE7wtgA5deCeYs7rSQtreh4cj6ByPtrg9sD7V2FNFLPnf8heNP3FGkeV9qwfzvZNSd54JoNXVsXFYSYwHsnJxqP",` +
			`"merchant_name":"N","support_email":"s@example.com","setup_token":"` + tok + `"}`
	}
	for _, c := range []struct {
		tok  string
		want int
	}{{"", 401}, {"wrong-token-wrong-token-1234", 403}, {token, 201}} {
		tok, want := c.tok, c.want
		res, err := http.Post(base+"/api/v1/setup/complete", "application/json", strings.NewReader(setupBody(tok)))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("setup with token %q = %d; want %d", tok, res.StatusCode, want)
		}
	}
	// Host validation: a foreign Host is refused, the health check (an IP
	// literal) is not.
	req, _ := http.NewRequest("GET", base+"/api/v1/setup", nil)
	req.Host = "rebind.attacker.example"
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host = %v, %v; want 421", res, err)
	}
	// A second server on the same data directory is refused while the
	// first holds the lock.
	cfg2 := cfg
	cfg2.Listen = freeAddr(t)
	var console2 safeBuffer
	if code := serveWith(cfg2, slog.New(slog.DiscardHandler), &console2, make(chan os.Signal)); code != exitFail ||
		!strings.Contains(console2.String(), "already running") {
		t.Fatalf("second server: exit %d, %q", code, console2.String())
	}
	res, err = http.Get(base + "/api/v1/setup")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("setup status = %d", res.StatusCode)
	}
	if code := run([]string{"healthcheck", "--url", base + "/nope"}, nil, io.Discard, io.Discard); code != exitFail {
		t.Fatalf("healthcheck of a 404 = %d", code)
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
	for _, want := range []string{"effective configuration", "shutting down", "HTTP server stopped", "hmnpay engine stopped", "hmnpay stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q", want)
		}
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("errors logged during a clean run:\n%s", logs.String())
	}
	// The lock was released: the data directory can be served again.
	release, err := lockDataDir(cfg.DataDir)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	release()
}

// bannerToken extracts the setup token from the first-run banner.
func bannerToken(t *testing.T, banner string) string {
	t.Helper()
	lines := strings.Split(banner, "\n")
	for i, l := range lines {
		if strings.Contains(l, "enter this setup token") {
			for _, next := range lines[i+1:] {
				if tok := strings.TrimSpace(next); tok != "" {
					return tok
				}
			}
		}
	}
	t.Fatalf("no setup token in the banner:\n%s", banner)
	return ""
}

func TestSetupBannerAndWarnings(t *testing.T) {
	var b bytes.Buffer
	printSetupBanner(&b, "https://pay.example.com/setup", "", true)
	if !strings.Contains(b.String(), "HMNPAY_SETUP_TOKEN") || !strings.Contains(b.String(), "https://pay.example.com/setup") {
		t.Fatalf("banner for a configured token = %s", b.String())
	}
	for listen, want := range map[string]bool{":8080": false, "0.0.0.0:80": false, "127.0.0.1:8080": true, "[::1]:8080": true, "localhost:1": true, "10.0.0.2:80": false} {
		if got := loopbackListen(listen); got != want {
			t.Errorf("loopbackListen(%q) = %v", listen, got)
		}
	}
	var logs bytes.Buffer
	cfg, _ := parseServeConfig([]string{"--listen", "0.0.0.0:0", "--data-dir", t.TempDir()}, env(nil), io.Discard)
	cfg.Listen = "256.0.0.1:1" // fails at listen, after the warning
	serveWith(cfg, slog.New(slog.NewTextHandler(&logs, nil)), io.Discard, make(chan os.Signal))
	if !strings.Contains(logs.String(), "HMNPAY_PUBLIC_URL is not set") {
		t.Fatalf("no warning about a missing public URL:\n%s", logs.String())
	}
}

func TestUpstreamHeaders(t *testing.T) {
	h := http.Header{}
	setUpstreamHeaders(h, "localhost:8080")
	for k, v := range map[string]string{
		"X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff",
		"Cross-Origin-Opener-Policy": "same-origin", "Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := h.Values(k); len(got) != 1 || got[0] != v {
			t.Errorf("%s = %v", k, got)
		}
	}
	if h.Get("Permissions-Policy") == "" {
		t.Error("no Permissions-Policy")
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "connect-src 'self' ws://localhost:8080 wss://localhost:8080") {
		t.Errorf("development policy = %q", csp)
	}
	// Whatever the upstream sent for a security header is replaced, never
	// kept or merged: it does not choose the policy of this origin.
	h = http.Header{}
	h.Set("Content-Security-Policy", "frame-ancestors https://shop.example; script-src *")
	h.Set("Referrer-Policy", "unsafe-url")
	h.Set("X-Frame-Options", "ALLOWALL")
	setUpstreamHeaders(h, "")
	if got := h.Values("Content-Security-Policy"); len(got) != 1 || strings.Contains(got[0], "shop.example") || strings.Contains(got[0], "*") || !strings.Contains(got[0], "frame-ancestors 'none'") {
		t.Errorf("the upstream's policy survived: %q", got)
	}
	if h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("the upstream's headers survived: %v", h)
	}
	// A host that cannot be written into a policy is left out of it.
	if csp := devPolicy("evil; script-src *"); strings.Contains(csp, "evil") {
		t.Errorf("an odd Host got into the policy: %q", csp)
	}
}

func TestServeFailsOnBusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg, _ := parseServeConfig([]string{"--listen", ln.Addr().String(), "--data-dir", t.TempDir()}, env(nil), io.Discard)
	if code := serveWith(cfg, slog.New(slog.DiscardHandler), io.Discard, make(chan os.Signal)); code != exitFail {
		t.Fatalf("exit code %d; want %d", code, exitFail)
	}
}

// reset-password against the data directory of a running instance.
func TestResetPassword(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if code := run([]string{"reset-password", "--data-dir", dir, "--generate"}, nil, io.Discard, io.Discard); code != exitFail {
		t.Fatalf("without a database: exit %d", code)
	}

	// The "server": an engine with its workers running on the same data
	// directory.
	eng, err := hmnpay.Open(ctx, hmnpay.Config{
		DataDir: dir, Logger: slog.New(slog.DiscardHandler),
		Chain: hmnpay.ChainConfig{Disabled: true}, PasswordHasher: core.PBKDF2Hasher{Iterations: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- eng.Run(runCtx) }()
	defer func() {
		stop()
		<-runDone
		eng.Close()
	}()
	svc := eng.Service()

	if code := run([]string{"reset-password", "--data-dir", dir, "--generate"}, nil, io.Discard, io.Discard); code != exitFail {
		t.Fatalf("before setup: exit %d", code)
	}
	setup, err := svc.CompleteSetup(core.WithActor(ctx, core.SystemActor("test")), core.SetupInput{
		Email: "owner@example.com", Password: "correct horse battery staple",
		RPCURLs:  []string{"https://bsc-rpc.publicnode.com"},
		XPub:     "xpub6Ce9NcJvTk36xtLSrJLZqE7wtgA5deCeYs7rSQtreh4cj6ByPtrg9sD7V2FNFLPnf8heNP3FGkeV9qwfzvZNSd54JoNXVsXFYSYwHsnJxqP",
		Merchant: core.MerchantSettings{Name: "N", SupportEmail: "s@example.com"}, PublicURL: "https://pay.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	op, err := svc.AuthenticateSession(ctx, setup.Token)
	if err != nil {
		t.Fatal(err)
	}
	opCtx := core.WithActor(ctx, op)
	_, keySecret, err := svc.CreateAPIKey(opCtx, core.CreateAPIKeyInput{Name: "intruder", Scopes: core.Scopes})
	if err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	if code := run([]string{"reset-password", "--data-dir", dir, "--stdin"}, strings.NewReader("short\n"), io.Discard, &stderr); code != exitUsage {
		t.Fatalf("short password: exit %d (%s)", code, stderr.String())
	}
	stderr.Reset()
	var summary bytes.Buffer
	if code := run([]string{"reset-password", "--data-dir", dir, "--stdin"}, strings.NewReader("a much better passphrase\n"), &summary, &stderr); code != exitOK {
		t.Fatalf("reset: exit %d (%s)", code, stderr.String())
	}
	out := summary.String()
	for _, want := range []string{"owner@example.com", "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266", "xpub6Ce9NcJv…", "https://bsc-rpc.publicnode.com",
		"1 dashboard session(s) signed out", "1 active of 1", "If you did not configure this wallet", "--revoke-api-keys"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("reset output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "a much better passphrase") {
		t.Error("a password given on stdin was echoed")
	}
	// Without --revoke-api-keys the intruder's key still works…
	if _, err := svc.AuthenticateAPIKey(ctx, keySecret); err != nil {
		t.Fatalf("key revoked without --revoke-api-keys: %v", err)
	}
	// …with it, it does not.
	summary.Reset()
	if code := run([]string{"reset-password", "--data-dir", dir, "--stdin", "--revoke-api-keys"}, strings.NewReader("a much better passphrase\n"), &summary, io.Discard); code != exitOK {
		t.Fatalf("reset --revoke-api-keys: exit %d", code)
	}
	if !strings.Contains(summary.String(), "1 API key(s) revoked.") || !strings.Contains(summary.String(), "0 active of 1") {
		t.Errorf("revoke output:\n%s", summary.String())
	}
	if _, err := svc.AuthenticateAPIKey(ctx, keySecret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked key still authenticates: %v", err)
	}
	var inspected bytes.Buffer
	if code := run([]string{"inspect", "--data-dir", dir}, nil, &inspected, io.Discard); code != exitOK || !strings.Contains(inspected.String(), "First deposit addr") {
		t.Fatalf("inspect: exit %d\n%s", code, inspected.String())
	}
	if _, err := svc.AuthenticateSession(ctx, setup.Token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("the setup session survived the reset: %v", err)
	}
	if _, err := svc.Login(ctx, core.LoginInput{Email: "owner@example.com", Password: "a much better passphrase"}); err != nil {
		t.Fatalf("new password: %v", err)
	}

	// Non-interactive without flags: a generated password, printed once.
	var stdout bytes.Buffer
	if code := run([]string{"reset-password", "--data-dir", dir}, strings.NewReader(""), &stdout, io.Discard); code != exitOK {
		t.Fatalf("generate: exit %d", code)
	}
	lines := strings.Split(stdout.String(), "\n")
	generated, recovery := "", ""
	for i, l := range lines {
		if i+1 >= len(lines) {
			break
		}
		switch {
		case strings.HasPrefix(l, "New operator password"):
			generated = lines[i+1]
		case strings.HasPrefix(l, "Recovery code"):
			recovery = lines[i+1]
		}
	}
	// The reset also prints a one-time recovery code for an operator who is
	// rate-limited out of the login; it works together with the password.
	if !strings.HasPrefix(recovery, "hmnr_") {
		t.Fatalf("no recovery code in the output:\n%s", stdout.String())
	}
	if _, err := svc.Login(ctx, core.LoginInput{Email: "owner@example.com", Password: generated, RecoveryCode: recovery}); err != nil {
		t.Fatalf("generated password %q with the recovery code does not work: %v", generated, err)
	}
	if _, err := svc.Login(ctx, core.LoginInput{Email: "owner@example.com", Password: generated}); err != nil {
		t.Fatalf("generated password %q does not work: %v", generated, err)
	}
	if _, err := os.Stat(filepath.Join(dir, hmnpay.DatabaseFile)); err != nil {
		t.Fatal(err)
	}
}

// toolServer is a "running server" for the tests of the operator tools: an
// engine with its workers running on a data directory, and its HTTP API on a
// test listener. stop ends both, as stopping the server does.
type toolServer struct {
	eng  *hmnpay.Engine
	http *httptest.Server
	stop func()
}

func startToolServer(t *testing.T, dir string, limits hmnpay.HTTPLimits) *toolServer {
	t.Helper()
	ctx := context.Background()
	eng, err := hmnpay.Open(ctx, hmnpay.Config{
		DataDir: dir, Logger: slog.New(slog.DiscardHandler),
		Chain: hmnpay.ChainConfig{Disabled: true}, PasswordHasher: core.PBKDF2Hasher{Iterations: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	// After a CLI reset-password the account's hash is the real one (600 000
	// rounds of PBKDF2), and each login verifies it: half a second under the
	// race detector on an idle machine, more than the default 30-second
	// response deadline on one that is busy (seen: 68 s for the test, and a
	// login cut off with EOF). The deadline is not what these tests are
	// about.
	handler, err := eng.HTTPHandler(hmnpay.HTTPOptions{Limits: limits, WriteTimeout: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- eng.Run(runCtx) }()
	var once sync.Once
	s := &toolServer{eng: eng, http: srv}
	s.stop = func() {
		once.Do(func() {
			srv.Close()
			cancel()
			<-runDone
			eng.Close()
		})
	}
	t.Cleanup(s.stop)
	return s
}

// login posts a login from the given browser (a cookie jar, or nil for a
// client without cookies) and returns the HTTP status.
func (s *toolServer) login(t *testing.T, jar http.CookieJar, password, recovery string) int {
	t.Helper()
	body := map[string]string{"email": "owner@example.com", "password": password}
	if recovery != "" {
		body["recovery_code"] = recovery
	}
	raw, _ := json.Marshal(body)
	client := &http.Client{Jar: jar}
	res, err := client.Post(s.http.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}

// outputLine returns the line that follows the first line of out starting
// with prefix (the tools print secrets on a line of their own).
func outputLine(out, prefix string) string {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	return ""
}

// Reset-password used to rotate the stored device key as well. The
// running server kept the key it had started with, so the "known device"
// marker the operator received on the first login after the reset was signed
// with a key that was gone at the next restart: the browser was forgotten a
// second time, and under a sustained login flood the operator was locked out
// again — with no way to get a recovery code but another password reset.
//
// The reset forgets every browser at once through the credential generation
// alone; the marker of the login that follows must still be known after the
// server is restarted. And `hmnpay recovery-code` mints a code by itself.
func TestTheMarkerOfTheLoginAfterAResetSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	const oldPassword, newPassword = "correct horse battery staple", "a much better passphrase"
	// One wrong guess empties the shared buckets: what a login flood does.
	limits := hmnpay.HTTPLimits{
		LoginPerAccount: hmnpay.Rate{Burst: 1, Every: time.Hour},
		LoginPerIP:      hmnpay.Rate{Burst: 1000, Every: time.Second},
	}
	srv := startToolServer(t, dir, limits)
	if _, err := srv.eng.Service().CompleteSetup(core.WithActor(context.Background(), core.SystemActor("test")), core.SetupInput{
		Email: "owner@example.com", Password: oldPassword,
		RPCURLs:  []string{"https://bsc-rpc.publicnode.com"},
		XPub:     "xpub6Ce9NcJvTk36xtLSrJLZqE7wtgA5deCeYs7rSQtreh4cj6ByPtrg9sD7V2FNFLPnf8heNP3FGkeV9qwfzvZNSd54JoNXVsXFYSYwHsnJxqP",
		Merchant: core.MerchantSettings{Name: "N", SupportEmail: "s@example.com"}, PublicURL: "https://pay.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	// (The test keeps password verifications few: after the reset each one
	// runs the real KDF, which is slow by design and slower under the race
	// detector.)
	flood := func(s *toolServer) {
		t.Helper()
		if code := s.login(t, nil, "a wrong guess, one of many", ""); code != http.StatusUnauthorized {
			t.Fatalf("a wrong guess: %d", code)
		}
		// From here on a browser the instance does not know is turned away
		// before its password is looked at.
		if code := s.login(t, nil, newPassword, ""); code != http.StatusTooManyRequests {
			t.Fatalf("an unknown browser during the flood: %d, want 429", code)
		}
	}

	// A browser the instance knows from before the reset; then the flood
	// begins.
	before, _ := cookiejar.New(nil)
	if code := srv.login(t, before, oldPassword, ""); code != http.StatusOK {
		t.Fatalf("login before the reset: %d", code)
	}
	flood(srv)

	// The reset, while the server runs.
	var out, stderr bytes.Buffer
	if code := run([]string{"reset-password", "--data-dir", dir, "--stdin"}, strings.NewReader(newPassword+"\n"), &out, &stderr); code != exitOK {
		t.Fatalf("reset: exit %d (%s)", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "device key") {
		t.Errorf("reset-password still speaks of the device key: %s", stderr.String())
	}
	recovery := outputLine(out.String(), "Recovery code")
	if !strings.HasPrefix(recovery, "hmnr_") {
		t.Fatalf("no recovery code in the output:\n%s", out.String())
	}
	// The old browser is forgotten at once, in the running server: it is an
	// unknown device like any other, and the flood keeps those out.
	if code := srv.login(t, before, newPassword, ""); code != http.StatusTooManyRequests {
		t.Fatalf("a browser known before the reset, after it: %d, want 429 (it must be forgotten immediately)", code)
	}
	// The operator gets in with the recovery code, on a new browser, and
	// that browser is known from then on.
	browser, _ := cookiejar.New(nil)
	if code := srv.login(t, browser, newPassword, recovery); code != http.StatusOK {
		t.Fatalf("login with the recovery code: %d", code)
	}

	// The server is restarted. The flood goes on.
	srv.stop()
	again := startToolServer(t, dir, limits)
	u, _ := url.Parse(again.http.URL)
	old, _ := url.Parse(srv.http.URL)
	cookies, _ := cookiejar.New(nil)
	cookies.SetCookies(u, browser.Cookies(old)) // the same browser; the test server got another port
	if len(cookies.Cookies(u)) == 0 {
		t.Fatal("test setup: the browser holds no cookies")
	}
	flood(again)
	if code := again.login(t, cookies, newPassword, ""); code != http.StatusOK {
		t.Fatalf("after the restart the browser the operator logged in with after the reset is an unknown device again (%d): the operator is locked out a second time", code)
	}

	// hmnpay recovery-code: a code and nothing else, while the server runs.
	sessions := func() int {
		sum, err := again.eng.Service().InstanceSummary(core.WithActor(context.Background(), core.SystemActor("test")))
		if err != nil {
			t.Fatal(err)
		}
		return sum.Sessions
	}
	had := sessions()
	out.Reset()
	stderr.Reset()
	if code := run([]string{"recovery-code", "--data-dir", dir}, nil, &out, &stderr); code != exitOK {
		t.Fatalf("recovery-code: exit %d (%s)", code, stderr.String())
	}
	code2 := outputLine(out.String(), "Recovery code")
	if !strings.HasPrefix(code2, "hmnr_") || code2 == recovery || !strings.Contains(out.String(), "one login, shown once") ||
		!strings.Contains(out.String(), "too many attempts") {
		t.Fatalf("recovery-code output:\n%s", out.String())
	}
	if strings.Count(out.String(), "\n") != 3 {
		t.Errorf("recovery-code prints more than the code and how to use it:\n%s", out.String())
	}
	if sessions() != had {
		t.Fatalf("recovery-code ended sessions: %d, had %d", sessions(), had)
	}
	// It lets a browser the instance has never seen past the flood, with
	// the password.
	fresh, _ := cookiejar.New(nil)
	if code := again.login(t, fresh, newPassword, code2); code != http.StatusOK {
		t.Fatalf("recovery code with the password: %d", code)
	}
	// One login per code.
	if code := again.login(t, nil, newPassword, code2); code != http.StatusUnauthorized {
		t.Fatalf("a used recovery code: %d", code)
	}
}

// recovery-code refuses where there is nothing to recover.
func TestRecoveryCodeCommand(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	if code := run([]string{"recovery-code", "--data-dir", dir}, nil, io.Discard, &stderr); code != exitFail || !strings.Contains(stderr.String(), "no hmnpay database") {
		t.Fatalf("without a database: exit %d (%s)", code, stderr.String())
	}
	srv := startToolServer(t, dir, hmnpay.HTTPLimits{})
	stderr.Reset()
	var out bytes.Buffer
	if code := run([]string{"recovery-code", "--data-dir", dir}, nil, &out, &stderr); code != exitFail ||
		!strings.Contains(stderr.String(), "setup has not been completed") || out.Len() != 0 {
		t.Fatalf("before setup: exit %d, stdout %q, stderr %q", code, out.String(), stderr.String())
	}
	if code := run([]string{"recovery-code", "--data-dir", dir, "extra"}, nil, io.Discard, io.Discard); code != exitUsage {
		t.Fatalf("with an argument: exit %d", code)
	}
	if code := run([]string{"recovery-code", "-h"}, nil, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("-h: exit %d", code)
	}
	var help bytes.Buffer
	if run([]string{"help"}, nil, &help, io.Discard) != exitOK || !strings.Contains(help.String(), "recovery-code") {
		t.Fatalf("help does not list recovery-code:\n%s", help.String())
	}
	srv.stop()
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// .env.example documents every setting of the binary, with its default.
func TestEnvExampleDocumentsEverySetting(t *testing.T) {
	b, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, s := range settings {
		line := s.env + "=" + s.def
		if !strings.Contains(doc, "\n"+line+"\n") {
			t.Errorf(".env.example lacks %q", line)
		}
	}
}
