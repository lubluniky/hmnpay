package httpapi

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

func TestStrictDecoding(t *testing.T) {
	e := newTenv(t)
	w := bearer(e.key(core.ScopePaymentsWrite))
	cases := []struct {
		name   string
		body   string
		ct     string
		status int
		code   string
		field  string
	}{
		{"wrong content type", `{"amount":"1"}`, "text/plain", 415, "unsupported_media_type", ""},
		{"form content type", `amount=1`, "application/x-www-form-urlencoded", 415, "unsupported_media_type", ""},
		{"latin1 charset", `{"amount":"1"}`, "application/json; charset=latin1", 415, "unsupported_media_type", ""},
		{"empty body", ``, "application/json", 400, "empty_body", ""},
		{"not json", `{"amount":`, "application/json", 400, "invalid_json", ""},
		{"syntax error", `{amount:1}`, "application/json", 400, "invalid_json", ""},
		{"two values", `{"amount":"1"}{"amount":"2"}`, "application/json", 400, "invalid_json", ""},
		{"trailing garbage", `{"amount":"1"} x`, "application/json", 400, "invalid_json", ""},
		{"array", `[1]`, "application/json", 400, "invalid_json", ""},
		{"unknown field", `{"amount":"1","amout":"2"}`, "application/json", 400, "unknown_field", "amout"},
		{"amount as number", `{"amount":149.5}`, "application/json", 400, "invalid_type", "amount"},
		{"metadata value number", `{"amount":"1","metadata":{"a":1}}`, "application/json", 400, "invalid_type", "metadata.a"},
		{"too large", `{"amount":"1","description":"` + strings.Repeat("x", 70<<10) + `"}`, "application/json", 413, "body_too_large", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := e.do(req{method: "POST", path: "/api/v1/payments", body: c.body, h: merge(w, hdr{"Content-Type": c.ct})})
			if c.body == "" {
				r := httptest.NewRequest("POST", "/api/v1/payments", nil)
				r.Host = "pay.example.com"
				r.Header.Set("Content-Type", c.ct)
				r.Header.Set("Authorization", w["Authorization"])
				resp = httptest.NewRecorder()
				e.h.ServeHTTP(resp, r)
			}
			er := wantError(t, resp, c.status, c.code)
			if c.field != "" && (len(er.Error.Fields) != 1 || er.Error.Fields[0].Field != c.field) {
				t.Fatalf("fields %+v", er.Error.Fields)
			}
		})
	}
	// Trailing whitespace is fine; JSON with explicit utf-8 charset is fine.
	resp := e.do(req{method: "POST", path: "/api/v1/payments", body: "{\"amount\":\"1\"}\n \n", h: merge(w, hdr{"Content-Type": "application/json; charset=UTF-8"})})
	wantStatus(t, resp, http.StatusCreated)
	// Bodyless actions accept no body, an empty JSON body, or {} — but not
	// a non-JSON body.
	op := e.session()
	p := e.CreatePayment("1")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/payments/" + p.ID + "/reopen", body: "x", h: merge(op, hdr{"Content-Type": "text/plain"})}), 415, "unsupported_media_type")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/payments/" + p.ID + "/reopen", body: "{}", h: op}), 409, "nothing_to_resolve")
}

func TestErrorMapping(t *testing.T) {
	// invalid → 422, not_found → 404, conflict → 409, unauthorized → 401,
	// forbidden → 403, setup_required → 503 are covered by the use-case
	// tests; here: unavailable → 503 and internal → 500 (never echoed).
	t.Run("unavailable", func(t *testing.T) {
		e := newTenv(t, withEnv(coretest.WithConfig(func(c *core.Config) { c.Prober = nil })))
		w := e.do(req{method: "POST", path: "/api/v1/setup/probe-rpc", body: map[string]any{"url": "https://rpc.example.org"}, h: e.session()})
		wantStatus(t, w, http.StatusServiceUnavailable)
		if er := decodeBody[errResp](t, w); er.Error.Code != "unavailable" {
			t.Fatalf("code %q", er.Error.Code)
		}
	})
	t.Run("internal", func(t *testing.T) {
		e := newTenv(t)
		e.Prober.Err = errors.New("dial tcp 10.1.2.3:8545: secret internal detail")
		w := e.do(req{method: "POST", path: "/api/v1/setup/probe-rpc", body: map[string]any{"url": "https://rpc.example.org"}, h: e.session()})
		er := wantError(t, w, http.StatusInternalServerError, "internal")
		if strings.Contains(w.Body.String(), "secret internal detail") || er.Error.Message != "internal server error" {
			t.Fatalf("internal error echoed: %s", w.Body.String())
		}
		logs := e.logs.String()
		if !strings.Contains(logs, "secret internal detail") || !strings.Contains(logs, er.RequestID) {
			t.Fatalf("internal error not logged with request id: %s", logs)
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		e := newTenv(t)
		wantError(t, e.do(req{method: "GET", path: "/api/v1/settings", h: bearer(e.key(core.ScopePaymentsRead))}), http.StatusForbidden, "forbidden")
	})
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	e := newTenv(t)
	w := e.do(req{method: "GET", path: "/api/v1/nope"})
	wantError(t, w, http.StatusNotFound, "not_found")
	w = e.do(req{method: "DELETE", path: "/api/v1/payments"})
	wantError(t, w, http.StatusMethodNotAllowed, "method_not_allowed")
	if !strings.Contains(w.Header().Get("Allow"), "GET") {
		t.Fatalf("Allow %q", w.Header().Get("Allow"))
	}
	// Without a fallback, non-API paths are a JSON 404 as well.
	wantError(t, e.do(req{method: "GET", path: "/dashboard"}), http.StatusNotFound, "not_found")
}

func TestFallback(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) {
		o.Fallback = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>ui</html>"))
		})
	}))
	for _, p := range []string{"/", "/pay/pay_x", "/settings/webhooks", "/apiary"} {
		w := e.do(req{method: "GET", path: p})
		if w.Code != 200 || w.Body.String() != "<html>ui</html>" || w.Header().Get("Content-Security-Policy") != "" {
			t.Fatalf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
	wantError(t, e.do(req{method: "GET", path: "/api/v1/nope"}), http.StatusNotFound, "not_found")
	wantStatus(t, e.do(req{method: "GET", path: "/healthz"}), http.StatusOK)
}

func TestSecurityHeaders(t *testing.T) {
	e := newTenv(t)
	p := e.CreatePayment("1")
	for _, rq := range []req{
		{method: "GET", path: "/healthz"},
		{method: "GET", path: "/readyz"},
		{method: "GET", path: "/api/v1/settings", h: e.session()},
		{method: "GET", path: "/api/v1/public/payments/" + p.ID},
		{method: "GET", path: "/api/v1/nope"},
		{method: "GET", path: "/api/v1/payments"},
	} {
		w := e.do(rq)
		for k, want := range map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"Referrer-Policy":              "no-referrer",
			"Cache-Control":                "no-store",
			"X-Frame-Options":              "DENY",
			"Cross-Origin-Resource-Policy": "same-origin",
		} {
			if got := w.Header().Get(k); got != want {
				t.Errorf("%s %s: %s = %q, want %q", rq.method, rq.path, k, got, want)
			}
		}
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", rq.path, csp)
		}
		if w.Header().Get("X-Request-Id") == "" || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: request id / CORS headers wrong: %v", rq.path, w.Header())
		}
	}
	w := e.do(req{method: "GET", path: "/readyz"})
	if body := w.Body.String(); !strings.Contains(body, `"setup_completed":true`) || !strings.Contains(body, `"chain_healthy":false`) {
		t.Fatalf("readyz %s", body)
	}
}

func TestRequestID(t *testing.T) {
	e := newTenv(t)
	w := e.do(req{method: "GET", path: "/healthz", h: hdr{"X-Request-Id": "trace-123.abc_X"}})
	if got := w.Header().Get("X-Request-Id"); got != "trace-123.abc_X" {
		t.Fatalf("sane id not kept: %q", got)
	}
	w = e.do(req{method: "GET", path: "/healthz", h: hdr{"X-Request-Id": "bad id\" injected"}})
	if got := w.Header().Get("X-Request-Id"); got == "" || strings.Contains(got, " ") {
		t.Fatalf("insane id kept: %q", got)
	}
}

func TestCORS(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		e := newTenv(t)
		w := e.do(req{method: "OPTIONS", path: "/api/v1/payments", h: hdr{"Origin": "https://shop.example", "Access-Control-Request-Method": "POST"}})
		if w.Header().Get("Access-Control-Allow-Origin") != "" || w.Code == http.StatusNoContent {
			t.Fatalf("preflight answered: %d %v", w.Code, w.Header())
		}
	})
	t.Run("allow-list", func(t *testing.T) {
		e := newTenv(t, withOpts(func(o *Options) { o.CORSOrigins = []string{"https://shop.example"} }))
		w := e.do(req{method: "OPTIONS", path: "/api/v1/payments", h: hdr{"Origin": "https://shop.example", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization,content-type,idempotency-key"}})
		wantStatus(t, w, http.StatusNoContent)
		if w.Header().Get("Access-Control-Allow-Origin") != "https://shop.example" || w.Header().Get("Access-Control-Allow-Credentials") != "" ||
			!strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key") {
			t.Fatalf("preflight headers %v", w.Header())
		}
		w = e.do(req{method: "GET", path: "/api/v1/payments", h: merge(bearer(e.key(core.ScopePaymentsRead)), hdr{"Origin": "https://shop.example"})})
		wantStatus(t, w, http.StatusOK)
		if w.Header().Get("Access-Control-Allow-Origin") != "https://shop.example" || w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("headers %v", w.Header())
		}
		w = e.do(req{method: "GET", path: "/api/v1/payments", h: hdr{"Origin": "https://evil.example"}})
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("unlisted origin allowed")
		}
		// A CORS origin does not get cookie-CSRF rights.
		w = e.do(req{method: "POST", path: "/api/v1/payment-links", body: map[string]any{"name": "x"}, h: hdr{"Cookie": cookieSecure + "=" + e.Token, "Origin": "https://shop.example"}})
		wantError(t, w, http.StatusForbidden, "csrf_rejected")
		if _, err := New(e.Service, Options{CORSOrigins: []string{"*"}}); err == nil {
			t.Fatal("wildcard CORS origin accepted")
		}
	})
	t.Run("public", func(t *testing.T) {
		e := newTenv(t, withOpts(func(o *Options) { o.PublicCORS = true }))
		p := e.CreatePayment("1")
		w := e.do(req{method: "GET", path: "/api/v1/public/payments/" + p.ID, h: hdr{"Origin": "https://merchant.example"}})
		wantStatus(t, w, http.StatusOK)
		if w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" || w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("headers %v", w.Header())
		}
		w = e.do(req{method: "GET", path: "/api/v1/payments", h: hdr{"Origin": "https://merchant.example"}})
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("public CORS leaked onto the private API")
		}
	})
}

func TestCSRF(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) { o.TrustedOrigins = []string{"http://localhost:3000"} }))
	cookie := hdr{"Cookie": cookieSecure + "=" + e.Token}
	body := map[string]any{"name": "Link"}
	post := func(h hdr) int {
		return e.do(req{method: "POST", path: "/api/v1/payment-links", body: body, h: merge(cookie, h)}).Code
	}
	cases := []struct {
		name string
		h    hdr
		want int
	}{
		{"same origin", hdr{"Origin": testOrigin}, 201},
		{"sec-fetch-site same-origin", hdr{"Sec-Fetch-Site": "same-origin"}, 201},
		{"trusted dev origin", hdr{"Origin": "http://localhost:3000", "Sec-Fetch-Site": "cross-site"}, 201},
		{"non-browser (no headers)", hdr{}, 201},
		{"cross-site", hdr{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},
		{"same-site sibling", hdr{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.pay.example.com"}, 403},
		{"foreign origin, no fetch metadata", hdr{"Origin": "https://evil.example"}, 403},
		{"null origin", hdr{"Origin": "null"}, 403},
	}
	for _, c := range cases {
		if got := post(c.h); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	// Safe methods are not CSRF-checked; bearer tokens are exempt.
	wantStatus(t, e.do(req{method: "GET", path: "/api/v1/settings", h: merge(cookie, hdr{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"})}), 200)
	w := e.do(req{method: "POST", path: "/api/v1/payment-links", body: body, h: merge(bearer(e.key(core.ScopeLinksWrite)), hdr{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"})})
	wantStatus(t, w, http.StatusCreated)
	// When both are present the bearer token is used and the cookie ignored.
	w = e.do(req{method: "POST", path: "/api/v1/payment-links", body: body, h: merge(cookie, bearer(e.key(core.ScopeLinksWrite)), hdr{"Origin": "https://evil.example"})})
	wantStatus(t, w, http.StatusCreated)
	// Login CSRF and forced logout from a foreign page are refused.
	wantError(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"email": e.Email, "password": e.Password}, h: hdr{"Origin": "https://evil.example"}}), 403, "csrf_rejected")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/auth/logout", h: merge(cookie, hdr{"Sec-Fetch-Site": "cross-site"})}), 403, "csrf_rejected")
	wantStatus(t, e.do(req{method: "GET", path: "/api/v1/settings", h: cookie}), 200)
}

func TestSessionCookie(t *testing.T) {
	login := func(t *testing.T, e *tenv, rq req) *httptest.ResponseRecorder {
		t.Helper()
		rq.method, rq.path = "POST", "/api/v1/auth/login"
		rq.body = map[string]any{"email": e.Email, "password": e.Password}
		w := e.do(rq)
		wantStatus(t, w, http.StatusOK)
		return w
	}
	t.Run("https public URL", func(t *testing.T) {
		e := newTenv(t)
		w := login(t, e, req{})
		c := findCookie(w, cookieSecure)
		if c == nil {
			t.Fatalf("cookies %v", w.Result().Cookies())
		}
		if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
			t.Fatalf("cookie attributes %+v", c)
		}
		if c.MaxAge != int(core.DefaultSessionTTL/time.Second) {
			t.Fatalf("max-age %d", c.MaxAge)
		}
		if strings.Contains(w.Body.String(), c.Value) {
			t.Fatal("token in body of a cookie login")
		}
		// The token works as the cookie…
		sess := hdr{"Cookie": cookieSecure + "=" + c.Value}
		wantStatus(t, e.do(req{method: "GET", path: "/api/v1/auth/me", h: sess}), 200)
		// …but not under the non-prefixed name (cookie tossing over HTTP),
		wantError(t, e.do(req{method: "GET", path: "/api/v1/auth/me", h: hdr{"Cookie": cookieInsecure + "=" + c.Value}}), 401, "unauthorized")
		// …and never from the query string.
		wantError(t, e.do(req{method: "GET", path: "/api/v1/auth/me?token=" + c.Value + "&session=" + c.Value}), 401, "unauthorized")

		// Logout ends the session and clears the cookie.
		w = e.do(req{method: "POST", path: "/api/v1/auth/logout", h: merge(sess, hdr{"Origin": testOrigin})})
		wantStatus(t, w, http.StatusNoContent)
		if cl := findCookie(w, cookieSecure); cl == nil || cl.MaxAge >= 0 || cl.Value != "" {
			t.Fatalf("logout cookie %+v", cl)
		}
		w = e.do(req{method: "GET", path: "/api/v1/auth/me", h: sess})
		wantError(t, w, 401, "invalid_credentials")
		if cl := findCookie(w, cookieSecure); cl == nil || cl.MaxAge >= 0 {
			t.Fatal("stale cookie not cleared")
		}
		wantStatus(t, e.do(req{method: "POST", path: "/api/v1/auth/logout", h: sess}), http.StatusNoContent)
	})
	t.Run("http public URL", func(t *testing.T) {
		e := newTenv(t, withOpts(func(o *Options) { o.PublicURL = "http://pay.local:8080" }))
		c := findCookie(login(t, e, req{}), cookieInsecure)
		if c == nil || c.Secure || !c.HttpOnly {
			t.Fatalf("cookie %+v", c)
		}
	})
	t.Run("no public URL", func(t *testing.T) {
		proxies, _ := ParsePrefixes([]string{"10.0.0.0/8"})
		e := newTenv(t, withOpts(func(o *Options) { o.PublicURL = ""; o.TrustedProxies = proxies }))
		if c := findCookie(login(t, e, req{}), cookieInsecure); c == nil || c.Secure {
			t.Fatalf("plain http: %+v", c)
		}
		if c := findCookie(login(t, e, req{remote: "10.0.0.2:1", h: hdr{"X-Forwarded-Proto": "https"}}), cookieSecure); c == nil || !c.Secure {
			t.Fatal("trusted proxy https not honoured")
		}
		if c := findCookie(login(t, e, req{h: hdr{"X-Forwarded-Proto": "https"}}), cookieInsecure); c == nil || c.Secure {
			t.Fatal("untrusted X-Forwarded-Proto honoured")
		}
		r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"email":"`+e.Email+`","password":"`+e.Password+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Host = "localhost"
		r.TLS = &tls.ConnectionState{}
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if c := findCookie(w, cookieSecure); c == nil || !c.Secure {
			t.Fatal("direct TLS not secure")
		}
	})
	t.Run("bearer session token", func(t *testing.T) {
		e := newTenv(t)
		w := login(t, e, req{})
		_ = w
		w = e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"email": e.Email, "password": e.Password, "issue_token": true}})
		wantStatus(t, w, http.StatusOK)
		lr := decodeBody[loginResponse](t, w)
		if !strings.HasPrefix(lr.Token, "hmns_") || len(w.Result().Cookies()) != 0 {
			t.Fatalf("token login %+v cookies %v", lr, w.Result().Cookies())
		}
		// Full operator capabilities, no CSRF requirements (not ambient).
		wantStatus(t, e.do(req{method: "POST", path: "/api/v1/api-keys", body: map[string]any{"name": "bot", "scopes": []string{"payments:read"}}, h: merge(bearer(lr.Token), hdr{"Origin": "https://evil.example"})}), 201)
		wantStatus(t, e.do(req{method: "POST", path: "/api/v1/auth/logout", h: bearer(lr.Token)}), http.StatusNoContent)
		wantError(t, e.do(req{method: "GET", path: "/api/v1/settings", h: bearer(lr.Token)}), 401, "invalid_credentials")
	})
	t.Run("uniform failures", func(t *testing.T) {
		e := newTenv(t)
		a := wantError(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"email": e.Email, "password": "wrong password!"}}), 401, "invalid_credentials")
		b := wantError(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"email": "nobody@example.com", "password": e.Password}}), 401, "invalid_credentials")
		if a.Error.Message != b.Error.Message {
			t.Fatalf("distinguishable: %q vs %q", a.Error.Message, b.Error.Message)
		}
	})
}

func TestNoSecretsInLogs(t *testing.T) {
	e := newTenv(t)
	w := e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"email": e.Email, "password": e.Password, "issue_token": true}})
	token := decodeBody[loginResponse](t, w).Token
	w = e.do(req{method: "POST", path: "/api/v1/api-keys", body: map[string]any{"name": "k", "scopes": []string{"payments:read", "webhooks:manage"}}, h: bearer(token)})
	key := decodeBody[apiKeyDTO](t, w).Secret
	w = e.do(req{method: "POST", path: "/api/v1/webhook-endpoints", body: map[string]any{"url": "https://hooks.example.com/?token=abc", "events": []string{"payment.paid"}}, h: bearer(key)})
	ep := decodeBody[endpointDTO](t, w)
	e.do(req{method: "POST", path: "/api/v1/webhook-endpoints/" + ep.ID + "/reveal-secret", h: bearer(key)})
	p := e.CreatePayment("1")
	e.do(req{method: "GET", path: "/api/v1/payments?q=sensitive-search-text", h: bearer(key)})
	e.do(req{method: "GET", path: "/api/v1/public/payments/" + p.ID})
	e.do(req{method: "GET", path: "/api/v1/payments", h: bearer("hmn_live_wrongwrongwrong")})
	e.do(req{method: "GET", path: "/api/v1/payments", h: hdr{"Cookie": cookieSecure + "=" + e.Token}})

	logs := e.logs.String()
	for name, secret := range map[string]string{
		"session token": token, "operator token": e.Token, "api key": key, "webhook secret": ep.Secret,
		"wrong key": "hmn_live_wrongwrongwrong", "password": e.Password, "search": "sensitive-search-text",
		"payment id (checkout capability)": p.ID, "webhook url query": "token=abc",
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("%s found in logs", name)
		}
	}
	if !strings.Contains(logs, `"route":"GET /api/v1/public/payments/{id}"`) || !strings.Contains(logs, `"actor":"api_key"`) {
		t.Fatalf("access log lacks route/actor: %s", logs)
	}
}

func TestPanicRecovery(t *testing.T) {
	e := newTenv(t)
	e.h.handle(route{pattern: "GET /api/v1/test-panic", mode: authPublic, fn: func(http.ResponseWriter, *http.Request) error {
		panic("boom: secret detail")
	}})
	w := e.do(req{method: "GET", path: "/api/v1/test-panic"})
	er := wantError(t, w, http.StatusInternalServerError, "internal")
	if strings.Contains(w.Body.String(), "secret detail") {
		t.Fatal("panic value echoed")
	}
	if logs := e.logs.String(); !strings.Contains(logs, "http panic") || !strings.Contains(logs, er.RequestID) {
		t.Fatalf("panic not logged: %s", logs)
	}
}

func TestOptionsValidation(t *testing.T) {
	e := newTenv(t)
	for _, o := range []Options{
		{PublicURL: "pay.example.com"},
		{PublicURL: "ftp://pay.example.com"},
		{TrustedOrigins: []string{"not an origin"}},
		{CORSOrigins: []string{"https://shop.example/path"}},
	} {
		if _, err := New(e.Service, o); err == nil {
			t.Errorf("accepted %+v", o)
		}
	}
	if _, err := New(nil, Options{}); err == nil {
		t.Error("nil service accepted")
	}
	if _, err := ParsePrefixes([]string{"10.0.0.0/33"}); err == nil {
		t.Error("bad prefix accepted")
	}
	got, err := ParsePrefixes([]string{"127.0.0.1", " ::1 ", "10.1.2.3/8", ""})
	if err != nil || len(got) != 3 || got[2] != netip.MustParsePrefix("10.0.0.0/8") {
		t.Fatalf("ParsePrefixes %v %v", got, err)
	}
}
