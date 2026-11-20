package httpapi

// Regression tests for abuse of the HTTP surface: claiming an instance that
// is not yours, DNS rebinding, keys that read more than their scope, link
// checkouts that hold or flood a link, login floods against the password KDF,
// and streams that leak or exhaust. Each describes the attack and asserts the
// safe behaviour.

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

const (
	attackerXPub   = "xpubATTACKER6CUGRUonZSQ4TWtTMmzXdrXDtypWKiKrhko4egpiMZbpiaQL2jkwSB1icqYh2cfDfVxdx4df189oLKnC5fSwqPfgyP3h"
	testSetupToken = "st_2f9c4e81a7b3d05c6e19f4a2b3c8d7e6"
)

func withSetupToken(token string) func(*setup) {
	return withEnv(coretest.WithoutSetup(), coretest.WithConfig(func(c *core.Config) { c.SetupToken = token }))
}

// First-run setup used to be first-come-first-served for anyone who
// could reach the port. A server is now started with a setup token, and
// nothing of the wizard works without it.
func TestSetupNeedsTheSetupToken(t *testing.T) {
	e := newTenv(t, withSetupToken(testSetupToken), withOpts(func(o *Options) { o.PublicURL = "" }))
	attacker := map[string]any{
		"email": "attacker@evil.example", "password": "attacker-password-1",
		"chain_id": 56, "rpc_urls": []string{"https://rpc.evil.example"}, "xpub": attackerXPub,
		"merchant_name": "Shop", "support_email": "help@evil.example",
	}
	const remote = "203.0.113.66:40000"

	// The wizard learns that it has to ask for the token — not the token.
	w := e.do(req{method: "GET", path: "/api/v1/setup"})
	wantStatus(t, w, http.StatusOK)
	if st := decodeBody[setupStatusDTO](t, w); st.Completed || !st.SetupTokenRequired {
		t.Fatalf("setup status %+v", st)
	}
	if strings.Contains(w.Body.String(), testSetupToken) {
		t.Fatal("GET /setup reveals the setup token")
	}

	// A stranger on the internet, no credentials, no token, not a browser.
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/complete", remote: remote, body: attacker}), http.StatusUnauthorized, "setup_token_required")
	// The token is checked before anything else: garbage gets the same answer.
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/complete", remote: remote, body: map[string]any{"email": "x"}}), http.StatusUnauthorized, "setup_token_required")
	// A wrong token, in the header or in the body.
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/complete", remote: remote, body: attacker, h: hdr{setupTokenHeader: "st_guess"}}), http.StatusForbidden, "setup_token_invalid")
	withBody := map[string]any{"setup_token": "st_guess"}
	for k, v := range attacker {
		withBody[k] = v
	}
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/complete", remote: remote, body: withBody}), http.StatusForbidden, "setup_token_invalid")
	// The query string is never a place for it — not even the right token.
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/complete?setup_token=" + testSetupToken, remote: remote, body: attacker}), http.StatusUnauthorized, "setup_token_required")
	// The helpers are no anonymous request primitive either.
	probe := map[string]any{"url": "http://10.0.0.1:6379/"}
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/probe-rpc", remote: remote, body: probe}), http.StatusUnauthorized, "setup_token_required")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/preview-addresses", remote: remote, body: map[string]any{"xpub": xpub}}), http.StatusUnauthorized, "setup_token_required")
	if st := decodeBody[setupStatusDTO](t, e.do(req{method: "GET", path: "/api/v1/setup"})); st.Completed {
		t.Fatal("an anonymous caller completed setup")
	}

	// The person who installed the server has the token: header or body.
	tok := hdr{setupTokenHeader: testSetupToken}
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/setup/probe-rpc", body: map[string]any{"url": "https://rpc.example.org"}, h: tok}), http.StatusOK)
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/setup/preview-addresses", body: map[string]any{"xpub": xpub, "setup_token": testSetupToken}}), http.StatusOK)
	owner := map[string]any{
		"email": "owner@example.com", "password": "correct horse battery staple",
		"chain_id": 56, "rpc_urls": []string{"https://rpc.example.org"}, "xpub": xpub,
		"merchant_name": "Northwind", "support_email": "help@example.com",
	}
	w = e.do(req{method: "POST", path: "/api/v1/setup/complete", body: owner, h: tok})
	wantStatus(t, w, http.StatusCreated)
	if findCookie(w, cookieInsecure) == nil {
		t.Fatal("no session cookie after setup")
	}
	st, err := e.Service.GetSettings(e.System())
	if err != nil || st.Wallet.XPub != xpub {
		t.Fatalf("wallet after setup: %+v, %v", st.Wallet, err)
	}
	// Afterwards the token is spent: no flag, and it opens nothing.
	if st := decodeBody[setupStatusDTO](t, e.do(req{method: "GET", path: "/api/v1/setup"})); !st.Completed || st.SetupTokenRequired {
		t.Fatalf("setup status after setup %+v", st)
	}
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/complete", body: attacker, h: tok}), http.StatusConflict, "setup_completed")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/probe-rpc", body: probe, h: tok}), http.StatusUnauthorized, "unauthorized")
	// The token never reaches the logs.
	if strings.Contains(e.logs.String(), testSetupToken) || strings.Contains(e.logs.String(), "st_guess") {
		t.Fatal("a setup token was logged")
	}
}

// Guessing the setup token is charged like any other failed
// credential.
func TestWrongSetupTokensAreRateLimited(t *testing.T) {
	e := newTenv(t, withSetupToken(testSetupToken), withOpts(func(o *Options) {
		o.Limits.AuthFailuresPerIP = Rate{Burst: 3, Every: time.Hour}
	}))
	guess := func(remote string) int {
		return e.do(req{method: "POST", path: "/api/v1/setup/preview-addresses", remote: remote,
			body: map[string]any{"xpub": xpub}, h: hdr{setupTokenHeader: "st_wrong"}}).Code
	}
	for i := range 3 {
		if code := guess("198.51.100.5:1"); code != http.StatusForbidden {
			t.Fatalf("guess %d: status %d", i+1, code)
		}
	}
	if code := guess("198.51.100.5:1"); code != http.StatusTooManyRequests {
		t.Fatalf("fourth wrong token: status %d, want 429", code)
	}
	if code := guess("198.51.100.6:1"); code != http.StatusForbidden {
		t.Fatalf("another address: status %d", code)
	}
	// The right token still works from the throttled address (no lockout
	// of the legitimate installer by a guesser behind the same NAT).
	w := e.do(req{method: "POST", path: "/api/v1/setup/preview-addresses", remote: "198.51.100.5:1",
		body: map[string]any{"xpub": xpub}, h: hdr{setupTokenHeader: testSetupToken}})
	wantStatus(t, w, http.StatusOK)
}

// The API answers only for its own host names. A DNS-rebinding page
// ("attacker.test" re-resolved to 127.0.0.1) reaches the socket but sends
// the attacker's Host, and to the CSRF check it looks same-origin.
func TestHostValidation(t *testing.T) {
	check := func(t *testing.T, e *tenv, host string, allowed bool) {
		t.Helper()
		w := e.do(req{method: "GET", path: "/healthz", h: hdr{"Host": host}})
		switch {
		case allowed && w.Code != http.StatusOK:
			t.Errorf("Host %q: status %d, want 200", host, w.Code)
		case !allowed:
			if er := wantError(t, w, http.StatusMisdirectedRequest, "host_not_allowed"); strings.Contains(er.Error.Message, host) {
				t.Errorf("Host %q is echoed in the error", host)
			}
		}
	}
	always := []string{"localhost", "localhost:8080", "LOCALHOST", "127.0.0.1", "127.0.0.1:8080", "[::1]:8080", "::1", "10.1.2.3:8080", "192.168.1.20", ""}
	never := []string{"attacker.test", "attacker.test:8080", "pay.example.com.attacker.test", "example.com", "localhost.attacker.test", "127.0.0.1.nip.io", "a b", "evil.example."}

	t.Run("public URL set", func(t *testing.T) {
		e := newTenv(t)
		for _, h := range append([]string{"pay.example.com", "pay.example.com:443", "PAY.EXAMPLE.COM", "pay.example.com."}, always...) {
			check(t, e, h, true)
		}
		for _, h := range never {
			check(t, e, h, false)
		}
		// Every API route is covered, before authentication and limits.
		w := e.do(req{method: "GET", path: "/api/v1/payments", h: merge(e.session(), hdr{"Host": "attacker.test"})})
		wantError(t, w, http.StatusMisdirectedRequest, "host_not_allowed")
	})
	t.Run("no public URL", func(t *testing.T) {
		e := newTenv(t, withOpts(func(o *Options) { o.PublicURL = "" }))
		for _, h := range always {
			check(t, e, h, true)
		}
		for _, h := range append([]string{"pay.example.com", "hmnpay.lan"}, never...) {
			check(t, e, h, false)
		}
	})
	t.Run("extra allowed hosts", func(t *testing.T) {
		e := newTenv(t, withOpts(func(o *Options) { o.AllowedHosts = []string{"hmnpay.lan", "Internal-LB."} }))
		for _, h := range []string{"hmnpay.lan:8080", "internal-lb", "pay.example.com"} {
			check(t, e, h, true)
		}
		check(t, e, "other.lan", false)
	})
	t.Run("wildcard turns the check off", func(t *testing.T) {
		e := newTenv(t, withOpts(func(o *Options) { o.AllowedHosts = []string{"*"} }))
		check(t, e, "anything.example", true)
	})
	t.Run("bad configuration is refused", func(t *testing.T) {
		env := coretest.NewEnv(t)
		for _, bad := range []string{"https://pay.example.com", "host:8080", "a/b"} {
			if _, err := New(env.Service, Options{AllowedHosts: []string{bad}}); err == nil {
				t.Errorf("AllowedHosts entry %q accepted", bad)
			}
		}
		if _, err := New(env.Service, Options{DeviceKey: []byte("short")}); err == nil {
			t.Error("a short DeviceKey was accepted")
		}
	})
	t.Run("DNS rebinding cannot complete setup", func(t *testing.T) {
		// No setup token here on purpose: the Host rule alone must stop it.
		e := newTenv(t, withEnv(coretest.WithoutSetup()), withOpts(func(o *Options) { o.PublicURL = "" }))
		body := map[string]any{
			"email": "attacker@evil.example", "password": "attacker-password-1", "chain_id": 56,
			"rpc_urls": []string{"https://rpc.evil.example"}, "xpub": attackerXPub,
			"merchant_name": "Shop", "support_email": "help@evil.example",
		}
		rebind := hdr{"Host": "attacker.test:8080", "Origin": "http://attacker.test:8080", "Sec-Fetch-Site": "same-origin"}
		w := e.do(req{method: "POST", path: "/api/v1/setup/complete", remote: "127.0.0.1:50000", body: body, h: rebind})
		wantError(t, w, http.StatusMisdirectedRequest, "host_not_allowed")
		wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/probe-rpc", remote: "127.0.0.1:50000", body: map[string]any{"url": "http://10.0.0.1/"}, h: rebind}),
			http.StatusMisdirectedRequest, "host_not_allowed")
		if st := decodeBody[setupStatusDTO](t, e.do(req{method: "GET", path: "/api/v1/setup"})); st.Completed {
			t.Fatal("a rebinding page completed setup")
		}
	})
}

// An API key limited to webhooks:manage must not read the signing secret
// of any endpoint: with it, it could forge payment.paid deliveries.
func TestWebhooksManageKeyCannotReadSigningSecrets(t *testing.T) {
	e := newTenv(t)
	w := e.do(req{method: "POST", path: "/api/v1/webhook-endpoints", h: e.session(),
		body: map[string]any{"url": "https://shop.example.com/hooks/hmnpay", "events": []string{"payment.paid"}}})
	wantStatus(t, w, http.StatusCreated)
	ep := decodeBody[endpointDTO](t, w)

	limited := bearer(e.key(core.ScopeWebhooksManage))
	wantError(t, e.do(req{method: "POST", path: "/api/v1/webhook-endpoints/" + ep.ID + "/reveal-secret", h: limited}), http.StatusForbidden, "forbidden")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/webhook-endpoints/" + ep.ID + "/rotate-secret", h: limited}), http.StatusForbidden, "forbidden")
	// Nothing the key can read contains the secret or any part of it.
	for _, rq := range []req{
		{method: "GET", path: "/api/v1/webhook-endpoints"},
		{method: "GET", path: "/api/v1/webhook-endpoints/" + ep.ID},
		{method: "PUT", path: "/api/v1/webhook-endpoints/" + ep.ID, body: map[string]any{"url": "https://shop.example.com/hooks/hmnpay", "events": []string{"payment.paid"}, "enabled": true}},
	} {
		rq.h = limited
		w := e.do(rq)
		wantStatus(t, w, http.StatusOK)
		if strings.Contains(w.Body.String(), ep.Secret) || strings.Contains(w.Body.String(), ep.Secret[len(ep.Secret)-4:]) {
			t.Fatalf("%s %s shows secret material to a webhooks:manage key: %s", rq.method, rq.path, w.Body.String())
		}
	}
	// The key still gets the secret of an endpoint it creates itself, once.
	w = e.do(req{method: "POST", path: "/api/v1/webhook-endpoints", h: limited,
		body: map[string]any{"url": "https://integration.example.com/hook", "events": []string{"payment.paid"}}})
	wantStatus(t, w, http.StatusCreated)
	if own := decodeBody[endpointDTO](t, w); !strings.HasPrefix(own.Secret, "whsec_") || own.SecretMasked {
		t.Fatalf("creator did not receive its endpoint's secret: %+v", own)
	}
	// The operator can still reveal.
	w = e.do(req{method: "POST", path: "/api/v1/webhook-endpoints/" + ep.ID + "/reveal-secret", h: e.session()})
	wantStatus(t, w, http.StatusOK)
	if s := decodeBody[secretDTO](t, w); s.Secret != ep.Secret {
		t.Fatalf("operator reveal %+v", s)
	}
}

// Every unpaid, still-open payment through a limited link
// used to reserve one of its uses for the payment's whole lifetime, so a
// stranger could hold a link at "limit reached" for free. The rule now: a
// checkout of a limited link lives for the reservation window (10 minutes,
// shown to the customer as expires_at) and holds its use until it closes,
// and one client cannot take more than two.
func TestUnfundedCheckoutsHoldALimitedLinkOnlyBriefly(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) { o.Limits.LinkPaymentPerIPPerLink = Rate{} })) // production default
	e.Head(100)
	w := e.do(req{method: "POST", path: "/api/v1/payment-links", h: e.session(),
		body: map[string]any{"slug": "launch", "name": "Launch ticket", "amount": "25", "max_uses": 3}})
	wantStatus(t, w, http.StatusCreated)
	checkout := func(remote string) *http.Response {
		t.Helper()
		return e.do(req{method: "POST", path: "/api/v1/public/links/launch/payments", remote: remote, body: map[string]any{}}).Result()
	}
	status := func() core.LinkStatus {
		t.Helper()
		return decodeBody[core.LinkCheckoutView](t, e.do(req{method: "GET", path: "/api/v1/public/links/launch"})).Status
	}

	// One address cannot take all three uses: its third checkout is refused
	// by the per-client budget, and it does not use up the link's.
	first := e.do(req{method: "POST", path: "/api/v1/public/links/launch/payments", remote: "198.51.100.1:5000", body: map[string]any{}})
	wantStatus(t, first, http.StatusCreated)
	// The checkout page is told how long the customer has: the reservation
	// window, not the instance's 30-minute payment TTL.
	if co := decodeBody[core.CheckoutView](t, first); co.ExpiresAt.Sub(co.CreatedAt) != core.DefaultLinkReservation {
		t.Fatalf("a limited link's checkout expires after %s, want %s", co.ExpiresAt.Sub(co.CreatedAt), core.DefaultLinkReservation)
	}
	if resp := checkout("198.51.100.1:5000"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("second checkout from one address: %d", resp.StatusCode)
	}
	if resp := checkout("198.51.100.1:5000"); resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("third checkout from one address: %d", resp.StatusCode)
	}
	// With a second address the squatter fills the link.
	if resp := checkout("198.51.100.2:5000"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("checkout from a second address: %d", resp.StatusCode)
	}
	// While those checkouts are open a real customer is turned away — that
	// is the price of not overselling three customers who check out
	// together …
	if resp := checkout("203.0.113.9:5000"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("customer inside the reservation window: %d, want 409", resp.StatusCode)
	}
	if status() != core.LinkLimit {
		t.Fatalf("link status while reserved: %s", status())
	}
	// … but ten minutes later the squatter's checkouts have expired and
	// the customer is served.
	e.Clock.Advance(core.DefaultLinkReservation + core.DefaultExpiryGrace)
	e.Head(101)
	if status() != core.LinkActive {
		t.Fatalf("link status after the reservation window: %s", status())
	}
	if resp := checkout("203.0.113.9:5000"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("a link with 3 uses and zero paid payments refuses a real customer after the reservation window (HTTP %d)", resp.StatusCode)
	}
}

// One flooded link used to exhaust the single instance-wide budget
// for link payments, turning every link's customers away.
func TestFloodingOneLinkDoesNotStarveTheOthers(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) { o.Limits.LinkPaymentPerLink = Rate{Burst: 3, Every: time.Hour} }))
	for _, slug := range []string{"flooded", "quiet"} {
		if _, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Slug: slug, Name: slug, Amount: "1", Active: true}); err != nil {
			t.Fatal(err)
		}
	}
	post := func(slug string, i int) int {
		return e.do(req{method: "POST", path: "/api/v1/public/links/" + slug + "/payments", remote: fmt.Sprintf("198.51.100.%d:1", i), body: map[string]any{}}).Code
	}
	for i := 1; i <= 3; i++ {
		if code := post("flooded", i); code != http.StatusCreated {
			t.Fatalf("flooded link, request %d: %d", i, code)
		}
	}
	if code := post("flooded", 4); code != http.StatusTooManyRequests {
		t.Fatalf("flooded link beyond its budget: %d, want 429", code)
	}
	if code := post("quiet", 5); code != http.StatusCreated {
		t.Fatalf("another link while one is flooded: %d, want 201", code)
	}
	// A refused request gave its per-IP token back.
	if code := post("quiet", 4); code != http.StatusCreated {
		t.Fatalf("the address refused on the flooded link, on another link: %d", code)
	}
}

// slowHasher stands in for PBKDF2 (≈0.2–0.5 CPU-seconds per call) and
// records how many verifications run at the same time.
type slowHasher struct {
	running, peak, calls atomic.Int64
	delay                time.Duration
}

func (h *slowHasher) Hash(p string) (string, error) { return "slow$" + p, nil }
func (h *slowHasher) Verify(hash, p string) (bool, error) {
	h.calls.Add(1)
	n := h.running.Add(1)
	defer h.running.Add(-1)
	for {
		old := h.peak.Load()
		if n <= old || h.peak.CompareAndSwap(old, n) {
			break
		}
	}
	time.Sleep(h.delay)
	return hash == "slow$"+p, nil
}

// Every login attempt runs the password KDF, wrong emails included. Without
// a bound on how many run at once or in total, 200 clients (or one IPv6 /48)
// with 200 made-up emails would get 200 concurrent KDF runs.
func TestAnonymousLoginsCannotMonopoliseTheKDF(t *testing.T) {
	slow := &slowHasher{delay: 50 * time.Millisecond}
	e := newTenv(t,
		withEnv(coretest.WithConfig(func(c *core.Config) { c.Hasher = slow })),
		withOpts(func(o *Options) { o.Limits = Limits{} }), // production defaults
	)
	const clients = 200
	var wg sync.WaitGroup
	var reached, limited atomic.Int64
	for i := range clients {
		wg.Go(func() {
			w := e.do(req{method: "POST", path: "/api/v1/auth/login",
				remote: fmt.Sprintf("[2001:db8:%x::1]:4000", i+1), // one /64 each, all inside one /48
				body:   map[string]any{"email": fmt.Sprintf("nobody%d@example.com", i), "password": "x"}})
			switch w.Code {
			case http.StatusUnauthorized:
				reached.Add(1)
			case http.StatusTooManyRequests:
				limited.Add(1)
			}
		})
	}
	wg.Wait()
	t.Logf("%d anonymous logins from one IPv6 /48: %d reached the KDF, %d were rate-limited, peak %d running concurrently",
		clients, reached.Load(), limited.Load(), slow.peak.Load())
	if peak := slow.peak.Load(); peak > core.DefaultMaxConcurrentKDF {
		t.Fatalf("%d password-KDF computations ran concurrently for unauthenticated requests", peak)
	}
	// The instance-wide bucket (30 burst) is what most of them hit.
	if n := slow.calls.Load(); n > 31 || reached.Load()+limited.Load() != clients {
		t.Fatalf("%d of %d anonymous logins ran the KDF (limited %d, reached %d)", n, clients, limited.Load(), reached.Load())
	}
}

// The shared login buckets must not become a way to lock the
// operator out. A browser that has logged in before is exempt from them.
func TestKnownDeviceIsNotLockedOutBySharedBuckets(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) {
		o.Limits.LoginPerAccount = Rate{Burst: 2, Every: time.Hour}
		o.Limits.LoginGlobal = Rate{Burst: 4, Every: time.Hour}
	}))
	login := func(remote, email, password string, h hdr) *http.Response {
		t.Helper()
		return e.do(req{method: "POST", path: "/api/v1/auth/login", remote: remote, body: map[string]any{"email": email, "password": password}, h: h}).Result()
	}
	// The operator logs in from their browser and gets the device marker.
	resp := login("192.0.2.50:1", e.Email, e.Password, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("operator login: %d", resp.StatusCode)
	}
	var marker *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == deviceCookieSecure {
			marker = c
		}
	}
	if marker == nil || !marker.HttpOnly || !marker.Secure || marker.MaxAge < 86400 {
		t.Fatalf("no usable device marker cookie: %v", resp.Cookies())
	}
	known := hdr{"Cookie": marker.Name + "=" + marker.Value}

	// Someone hammers the operator's email and a few made-up ones until the
	// per-account and the instance-wide buckets are empty.
	for i := range 2 {
		if resp := login(fmt.Sprintf("198.51.100.%d:1", i+1), e.Email, "wrong password", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attacker attempt: %d", resp.StatusCode)
		}
	}
	for i := range 2 {
		login(fmt.Sprintf("198.51.100.%d:1", i+10), fmt.Sprintf("nobody%d@example.com", i), "x", nil)
	}
	// A new device is now told to wait — for the operator's email and for
	// any other …
	if resp := login("203.0.113.7:1", e.Email, e.Password, nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("unknown device with both shared buckets empty: %d, want 429", resp.StatusCode)
	}
	// … but the operator's own browser gets in, from any address.
	if resp := login("203.0.113.8:1", e.Email, e.Password, known); resp.StatusCode != http.StatusOK {
		t.Fatalf("known device was locked out by the shared buckets: %d", resp.StatusCode)
	}
	// The marker is no credential: the password is still checked.
	if resp := login("203.0.113.8:1", e.Email, "wrong password", known); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("known device with a wrong password: %d", resp.StatusCode)
	}
	// A forged, tampered or expired marker exempts nobody.
	binding, err := e.Service.DeviceBinding(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	exp, mac, _ := strings.Cut(marker.Value, ".")
	for name, value := range map[string]string{
		"garbage":       "nonsense",
		"later expiry":  "9999999999." + mac,
		"flipped mac":   exp + "." + strings.ToUpper(mac[:1]) + mac[1:] + "A",
		"other key":     (&Handler{deviceKey: make([]byte, 32), now: time.Now}).deviceMarker(time.Now().Add(time.Hour), binding),
		"other account": e.h.deviceMarker(time.Now().Add(time.Hour), strings.Repeat("0", 32)),
		"unbound (v1)":  e.h.deviceMarker(time.Now().Add(time.Hour), ""),
		"empty":         "",
		"expired (own)": e.h.deviceMarker(time.Now().Add(-time.Minute), binding),
	} {
		if resp := login("203.0.113.9:1", e.Email, e.Password, hdr{"Cookie": marker.Name + "=" + value}); resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("%s marker: status %d, want 429", name, resp.StatusCode)
		}
	}
	// A bot that asks for a bearer token gets no cookie.
	e2 := newTenv(t)
	w := e2.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"email": e2.Email, "password": e2.Password, "issue_token": true}})
	wantStatus(t, w, http.StatusOK)
	if len(w.Result().Cookies()) != 0 {
		t.Fatalf("token login set cookies: %v", w.Result().Cookies())
	}
}

// When the core's KDF slots are all taken the caller is told to
// come back, with Retry-After, instead of queueing without bound.
func TestBusyIsServiceUnavailableWithRetryAfter(t *testing.T) {
	slow := &slowHasher{delay: 3 * time.Second}
	e := newTenv(t, withEnv(coretest.WithConfig(func(c *core.Config) { c.Hasher = slow; c.MaxConcurrentKDF = 1 })))
	first := make(chan int, 1)
	go func() {
		first <- e.do(req{method: "POST", path: "/api/v1/auth/login", remote: "192.0.2.1:1", body: map[string]any{"email": e.Email, "password": e.Password}}).Code
	}()
	for slow.running.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	w := e.do(req{method: "POST", path: "/api/v1/auth/login", remote: "192.0.2.2:1", body: map[string]any{"email": e.Email, "password": e.Password}})
	wantError(t, w, http.StatusServiceUnavailable, "busy")
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("503 busy without Retry-After")
	}
	if code := <-first; code != http.StatusOK {
		t.Fatalf("the login that held the slot: %d", code)
	}
}

// Forwarding headers from a peer that is not a trusted proxy mean
// every client shares one limiter key; that is worth one loud warning.
func TestUntrustedForwardedHeaderWarnsOnce(t *testing.T) {
	e := newTenv(t)
	for range 3 {
		e.do(req{method: "GET", path: "/healthz", remote: "172.17.0.1:4000", h: hdr{"X-Forwarded-For": "203.0.113.5"}})
	}
	logs := e.logs.String()
	if n := strings.Count(logs, "not a trusted proxy"); n != 1 {
		t.Fatalf("warning logged %d times, want once:\n%s", n, logs)
	}
	if !strings.Contains(logs, "172.17.0.1") {
		t.Fatalf("the warning must name the peer to trust:\n%s", logs)
	}
	// From a trusted proxy there is nothing to warn about.
	proxies, _ := ParsePrefixes([]string{"172.17.0.0/16"})
	e2 := newTenv(t, withOpts(func(o *Options) { o.TrustedProxies = proxies }))
	e2.do(req{method: "GET", path: "/healthz", remote: "172.17.0.1:4000", h: hdr{"X-Forwarded-For": "203.0.113.5"}})
	if strings.Contains(e2.logs.String(), "not a trusted proxy") {
		t.Fatal("warned about a trusted proxy")
	}
}

// GET /payments/{id}/detail must not embed the payment's webhook
// deliveries — endpoint URL, signed request body, response body — for a key
// that only holds payments:read: the delivery log requires webhooks:manage.
func TestPaymentsReadKeySeesNoWebhookDeliveries(t *testing.T) {
	e := newTenv(t)
	_, err := e.Service.CreateWebhookEndpoint(e.Operator(), core.WebhookEndpointInput{
		URL: "https://shop.example.com/hooks/hmnpay?token=s3cr3t-in-query", Events: []core.EventType{core.EventPaymentCreated}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	p := e.CreatePayment("10")
	if _, err := e.Service.PlanWebhookDeliveries(e.System()); err != nil {
		t.Fatal(err)
	}
	jobs, err := e.Service.ClaimWebhookDeliveries(e.System(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v, %d jobs", err, len(jobs))
	}
	res := jobs[0].Result()
	res.ResponseCode, res.ResponseBody = 500, "internal: db password=hunter2"
	if _, err := e.Service.CompleteWebhookDelivery(e.System(), jobs[0].DeliveryID, res); err != nil {
		t.Fatal(err)
	}

	reader := bearer(e.key(core.ScopePaymentsRead))
	wantError(t, e.do(req{method: "GET", path: "/api/v1/webhook-deliveries", h: reader}), http.StatusForbidden, "missing_scope")
	w := e.do(req{method: "GET", path: "/api/v1/payments/" + p.ID + "/detail", h: reader})
	wantStatus(t, w, http.StatusOK)
	d := decodeBody[paymentDetailDTO](t, w)
	if len(d.Deliveries) > 0 || strings.Contains(w.Body.String(), "s3cr3t-in-query") || strings.Contains(w.Body.String(), "hunter2") {
		t.Fatalf("a payments:read key read webhook delivery data: %s", w.Body.String())
	}
	if len(d.Timeline) == 0 || d.Payment.ID != p.ID {
		t.Fatalf("the rest of the detail must still be there: %s", w.Body.String())
	}
	// The operator, and a key that may read the delivery log, see them.
	for who, h := range map[string]hdr{"operator": e.session(), "key with webhooks:manage": bearer(e.key(core.ScopePaymentsRead, core.ScopeWebhooksManage))} {
		w := e.do(req{method: "GET", path: "/api/v1/payments/" + p.ID + "/detail", h: h})
		wantStatus(t, w, http.StatusOK)
		if d := decodeBody[paymentDetailDTO](t, w); len(d.Deliveries) != 1 {
			t.Fatalf("%s sees %d deliveries, want 1", who, len(d.Deliveries))
		}
	}
}

// The checkout stream's SSE "id:" must not be the instance-wide event
// sequence number: any payer could read from it how many events (≈ payments
// × transitions) the merchant had processed, and sample it over time.
func TestPublicStreamIDsArePerPayment(t *testing.T) {
	e := newTenv(t, withOpts(fastStreams))
	for range 40 { // other customers' business
		e.CreatePayment("5")
	}
	p := e.CreatePayment("10")
	srv := startServer(t, e.h)
	url := srv.URL + "/api/v1/public/payments/" + p.ID + "/stream"
	c := openStream(t, url, nil)
	first, _ := c.next(nil)
	if first.id != "1" {
		t.Fatalf("public stream id %s reveals the global event counter (41 payments created on this instance)", first.id)
	}
	// Other payments' events do not move this stream's ids.
	for range 5 {
		e.CreatePayment("5")
	}
	e.Pay(e.Payment(p.ID), "4", 100)
	second, v := c.next(func(v core.CheckoutView) bool { return v.Status == core.StatusDetected })
	if second.id != "2" || v.AmountReceived != "4.00" {
		t.Fatalf("second event: id %s, %+v", second.id, v)
	}
	// Last-Event-ID resume keeps working with the per-payment ids.
	current := openStream(t, url, hdr{"Last-Event-ID": "2"})
	current.quiet(150 * time.Millisecond)
	stale := openStream(t, url, hdr{"Last-Event-ID": "1"})
	if ev, v := stale.next(nil); ev.id != "2" || v.Status != core.StatusDetected {
		t.Fatalf("stale resume: id %s, %+v", ev.id, v)
	}
	e.Pay(e.Payment(p.ID), "6", 100)
	if ev, v := current.next(nil); ev.id != "3" || v.Status != core.StatusConfirming {
		t.Fatalf("after resume: id %s, %+v", ev.id, v)
	}
}

// Logout is unauthenticated; it is rate-limited, and one payment id
// cannot be used to occupy every stream slot.
func TestLogoutAndStreamsAreBounded(t *testing.T) {
	t.Run("logout", func(t *testing.T) {
		e := newTenv(t, withOpts(func(o *Options) { o.Limits.PublicReadPerIP = Rate{Burst: 3, Every: time.Hour} }))
		spam := bearer("hmns_0000000000000000000000000000000000000000000000000000")
		for i := range 3 {
			if code := e.do(req{method: "POST", path: "/api/v1/auth/logout", h: spam}).Code; code != http.StatusNoContent {
				t.Fatalf("logout %d: %d", i+1, code)
			}
		}
		wantError(t, e.do(req{method: "POST", path: "/api/v1/auth/logout", h: spam}), http.StatusTooManyRequests, "rate_limited")
		wantStatus(t, e.do(req{method: "POST", path: "/api/v1/auth/logout", h: spam, remote: "198.51.100.9:1"}), http.StatusNoContent)
	})
	t.Run("streams per payment", func(t *testing.T) {
		e := newTenv(t, withOpts(func(o *Options) {
			fastStreams(o)
			o.Stream.MaxPerPayment, o.Stream.MaxPerIP = 2, 100
		}))
		srv := startServer(t, e.h)
		a, b := e.CreatePayment("1"), e.CreatePayment("2")
		url := func(id string) string { return srv.URL + "/api/v1/public/payments/" + id + "/stream" }
		s1, s2 := openStream(t, url(a.ID), nil), openStream(t, url(a.ID), nil)
		s1.next(nil)
		s2.next(nil)
		if third := openStream(t, url(a.ID), nil); third.resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("third stream on one payment: %d, want 429", third.resp.StatusCode)
		}
		// Another payment is unaffected, and a closed stream frees its slot.
		if other := openStream(t, url(b.ID), nil); other.resp.StatusCode != http.StatusOK {
			t.Fatalf("stream on another payment: %d", other.resp.StatusCode)
		}
		s1.close()
		waitStreams(t, e.h, 2)
		if again := openStream(t, url(a.ID), nil); again.resp.StatusCode != http.StatusOK {
			t.Fatalf("stream after one closed: %d", again.resp.StatusCode)
		}
	})
	// Three addresses used to be enough
	// to hold all of one payment's stream slots (8 + 8 + 4 of 20) and leave
	// its payer without live updates. One address now gets a share of a
	// payment's slots, so that takes seven.
	t.Run("streams per address per payment", func(t *testing.T) {
		if d := (StreamOptions{}).withDefaults(); d.MaxPerIPPerPayment != 3 || d.MaxPerPayment/d.MaxPerIPPerPayment < 6 {
			t.Fatalf("default stream caps: %+v", d)
		}
		e := newTenv(t, withOpts(func(o *Options) {
			fastStreams(o)
			o.Stream.MaxPerPayment, o.Stream.MaxPerIP, o.Stream.MaxPerIPPerPayment = 10, 100, 2
			o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
		}))
		srv := startServer(t, e.h)
		a, b := e.CreatePayment("1"), e.CreatePayment("2")
		url := func(id string) string { return srv.URL + "/api/v1/public/payments/" + id + "/stream" }
		from := func(ip string) hdr { return hdr{"X-Forwarded-For": ip} }
		for range 2 {
			if s := openStream(t, url(a.ID), from("198.51.100.1")); s.resp.StatusCode != http.StatusOK {
				t.Fatalf("stream within one address's share: %d", s.resp.StatusCode)
			}
		}
		third := openStream(t, url(a.ID), from("198.51.100.1"))
		if third.resp.StatusCode != http.StatusTooManyRequests || third.resp.Header.Get("Retry-After") == "" {
			t.Fatalf("third stream of one address on one payment: %d, want 429 with Retry-After", third.resp.StatusCode)
		}
		// The payer, on another address, still gets a stream — and the
		// first address can watch another payment.
		if s := openStream(t, url(a.ID), from("203.0.113.9")); s.resp.StatusCode != http.StatusOK {
			t.Fatalf("the payer's stream: %d", s.resp.StatusCode)
		}
		if s := openStream(t, url(b.ID), from("198.51.100.1")); s.resp.StatusCode != http.StatusOK {
			t.Fatalf("the first address on another payment: %d", s.resp.StatusCode)
		}
	})
}

func TestStreamLimiterReleasesEveryCounter(t *testing.T) {
	l := newStreamLimiter(3, 4, 2, 5)
	r1, ok1 := l.acquire("ip1", "pay1")
	r2, ok2 := l.acquire("ip1", "pay1")
	if _, ok := l.acquire("ip1", "pay1"); !ok1 || !ok2 || ok {
		t.Fatal("per address per payment share not enforced")
	}
	r3, ok := l.acquire("ip1", "pay2")
	if !ok {
		t.Fatal("another payment refused")
	}
	if _, ok := l.acquire("ip1", "pay3"); ok {
		t.Fatal("per address cap not enforced")
	}
	r1()
	r1() // releasing twice is harmless
	r2()
	r3()
	if l.open() != 0 || len(l.byIP) != 0 || len(l.byPayment) != 0 || len(l.byIPPayment) != 0 {
		t.Fatalf("limiter not empty after every release: %+v", l)
	}
}

// The HTTP side of the watch-list cap: the dashboard can see the watch list fill up, and a
// full list answers public link checkouts with 503 and Retry-After.
func TestWatchListCapOverHTTP(t *testing.T) {
	e := newTenv(t, withEnv(coretest.WithConfig(func(c *core.Config) { c.MaxWatchedAddresses = 2 })))
	if _, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Slug: "donate", Name: "Donate", Amount: "1", Active: true}); err != nil {
		t.Fatal(err)
	}
	post := func(i int) *http.Response {
		return e.do(req{method: "POST", path: "/api/v1/public/links/donate/payments", remote: fmt.Sprintf("198.51.100.%d:1", i), body: map[string]any{}}).Result()
	}
	for i := 1; i <= 2; i++ {
		if resp := post(i); resp.StatusCode != http.StatusCreated {
			t.Fatalf("checkout %d: %d", i, resp.StatusCode)
		}
	}
	w := e.do(req{method: "POST", path: "/api/v1/public/links/donate/payments", remote: "198.51.100.3:1", body: map[string]any{}})
	wantError(t, w, http.StatusServiceUnavailable, "watch_list_full")
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("503 watch_list_full without Retry-After")
	}
	if !strings.Contains(e.logs.String(), "watch list is at its cap") {
		t.Fatal("hitting the watch-list cap was not logged")
	}
	w = e.do(req{method: "GET", path: "/api/v1/node-status", h: e.session()})
	wantStatus(t, w, http.StatusOK)
	if ns := decodeBody[nodeStatusDTO](t, w); ns.WatchedAddresses != 2 || ns.MaxWatchedAddresses != 2 || !ns.WatchListFull {
		t.Fatalf("node status %+v", ns)
	}
	// The merchant's own API is not affected.
	w = e.do(req{method: "POST", path: "/api/v1/payments", h: bearer(e.key(core.ScopePaymentsWrite)), body: map[string]any{"amount": "5"}})
	wantStatus(t, w, http.StatusCreated)
}

// A payment is refused while the server clock is behind the chain (its
// deadline would be set early): 503 clock_behind with Retry-After, for the
// merchant's API and payment links alike.
func TestClockBehindIsServiceUnavailableWithRetryAfter(t *testing.T) {
	e := newTenv(t, withEnv(coretest.WithConfig(func(c *core.Config) { c.MaxHeadTimeAhead = 2 * time.Minute })))
	if _, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Slug: "donate", Name: "Donate", Amount: "1", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.Service.ReportChainStatus(e.System(), core.ChainStatus{ClockSkew: -time.Hour}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []req{
		{method: "POST", path: "/api/v1/payments", h: bearer(e.key(core.ScopePaymentsWrite)), body: map[string]any{"amount": "5"}},
		{method: "POST", path: "/api/v1/public/links/donate/payments", remote: "198.51.100.1:1", body: map[string]any{}},
	} {
		w := e.do(r)
		wantError(t, w, http.StatusServiceUnavailable, "clock_behind")
		if got := w.Header().Get("Retry-After"); got != "60" {
			t.Fatalf("%s: Retry-After %q", r.path, got)
		}
	}
}
