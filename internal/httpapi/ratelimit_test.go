package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeNow) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeNow) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func TestLimiterBucket(t *testing.T) {
	clk := &fakeNow{t: time.Unix(1000, 0)}
	l := newLimiter(Rate{Burst: 2, Every: 10 * time.Second}, 100, clk.now)
	if l.take("a") != nil || l.take("a") != nil {
		t.Fatal("burst refused")
	}
	err := l.take("a")
	ae, ok := err.(*apiError)
	if !ok || ae.Status != http.StatusTooManyRequests || ae.RetryAfter != 10*time.Second {
		t.Fatalf("third take: %v", err)
	}
	if l.take("b") != nil {
		t.Fatal("keys are not independent")
	}
	clk.advance(5 * time.Second)
	if err := l.take("a"); err == nil || err.(*apiError).RetryAfter != 5*time.Second {
		t.Fatalf("half refill: %v", err)
	}
	clk.advance(5 * time.Second)
	if l.take("a") != nil {
		t.Fatal("refill not applied")
	}
	l.refund("a")
	if l.take("a") != nil || l.take("a") == nil {
		t.Fatal("refund/check broken")
	}
	clk.advance(time.Hour)
	if l.take("a") != nil || l.take("a") != nil || l.take("a") == nil {
		t.Fatal("refill exceeded the burst")
	}
	off := newLimiter(Rate{Burst: -1}, 10, clk.now)
	for i := 0; i < 100; i++ {
		if off.take("x") != nil {
			t.Fatal("disabled limiter limited")
		}
	}
}

func TestLimiterEviction(t *testing.T) {
	clk := &fakeNow{t: time.Unix(1000, 0)}
	l := newLimiter(Rate{Burst: 1, Every: time.Hour}, 3, clk.now)
	for _, k := range []string{"a", "b", "c"} {
		if l.take(k) != nil {
			t.Fatal(k)
		}
	}
	// Touch a so b is the least recently used, then add d.
	if l.take("a") == nil {
		t.Fatal("a not limited")
	}
	_ = l.take("d")
	if l.size() != 3 {
		t.Fatalf("size %d, want 3", l.size())
	}
	if l.take("a") == nil || l.take("c") == nil {
		t.Fatal("recent keys were evicted")
	}
	if l.take("b") != nil {
		t.Fatal("LRU key b was not evicted (its debt survived)")
	}
	// Memory stays bounded under a flood of unique keys.
	for i := 0; i < 1000; i++ {
		_ = l.take(strconv.Itoa(i))
	}
	if l.size() != 3 {
		t.Fatalf("size %d after flood", l.size())
	}
}

func TestClientIP(t *testing.T) {
	proxies, err := ParsePrefixes([]string{"10.0.0.0/8", "fd00::/8"})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{opts: Options{TrustedProxies: proxies}}
	none := &Handler{}
	cases := []struct {
		name   string
		h      *Handler
		remote string
		hdr    hdr
		want   string
	}{
		{"socket by default", none, "203.0.113.7:5000", hdr{"X-Forwarded-For": "1.2.3.4", "X-Real-IP": "5.6.7.8"}, "203.0.113.7"},
		{"untrusted peer ignores XFF", h, "203.0.113.7:5000", hdr{"X-Forwarded-For": "1.2.3.4"}, "203.0.113.7"},
		{"trusted proxy, single hop", h, "10.0.0.5:80", hdr{"X-Forwarded-For": "198.51.100.9"}, "198.51.100.9"},
		{"rightmost untrusted wins over spoofed left", h, "10.0.0.5:80", hdr{"X-Forwarded-For": "6.6.6.6, 198.51.100.9"}, "198.51.100.9"},
		{"chain of trusted proxies", h, "10.0.0.5:80", hdr{"X-Forwarded-For": "198.51.100.9, 10.0.0.7, 10.0.0.8"}, "198.51.100.9"},
		{"all trusted: leftmost", h, "10.0.0.5:80", hdr{"X-Forwarded-For": "10.0.0.9"}, "10.0.0.9"},
		{"malformed hop: use peer", h, "10.0.0.5:80", hdr{"X-Forwarded-For": "198.51.100.9, junk"}, "10.0.0.5"},
		{"X-Real-IP from trusted proxy", h, "10.0.0.5:80", hdr{"X-Real-IP": "198.51.100.10"}, "198.51.100.10"},
		{"X-Real-IP from stranger", h, "203.0.113.7:5000", hdr{"X-Real-IP": "198.51.100.10"}, "203.0.113.7"},
		{"ipv4-mapped peer", h, "[::ffff:10.0.0.5]:80", hdr{"X-Forwarded-For": "198.51.100.9"}, "198.51.100.9"},
		{"ipv6 trusted proxy", h, "[fd00::1]:80", hdr{"X-Forwarded-For": "2001:db8::1"}, "2001:db8::1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		if got := c.h.clientIP(r); got.String() != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	if k := ipKey(netip.MustParseAddr("2001:db8:1:2:3:4:5:6")); k != "2001:db8:1:2::/64" {
		t.Errorf("ipv6 key %q", k)
	}
	if k := ipKey(netip.MustParseAddr("198.51.100.9")); k != "198.51.100.9" {
		t.Errorf("ipv4 key %q", k)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) {
		o.Limits.LoginPerIP = Rate{Burst: 3, Every: time.Hour}
		o.Limits.LoginPerAccount = Rate{Burst: 5, Every: time.Hour}
	}))
	wrong := map[string]any{"email": e.Email, "password": "not the password"}
	right := map[string]any{"email": e.Email, "password": e.Password}
	ipA, ipB, ipC := "198.51.100.1:1", "198.51.100.2:1", "198.51.100.3:1"

	// Successful logins are refunded: they never use up the budget.
	for i := 0; i < 5; i++ {
		wantStatus(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: right, remote: ipA}), http.StatusOK)
	}
	for i := 0; i < 3; i++ {
		wantStatus(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: wrong, remote: ipA}), http.StatusUnauthorized)
	}
	w := wantError(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: right, remote: ipA}), http.StatusTooManyRequests, "rate_limited")
	_ = w
	if ra := e.do(req{method: "POST", path: "/api/v1/auth/login", body: right, remote: ipA}).Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After %q", ra)
	}
	// The account bucket spans IPs: 3 failures so far, 2 more from B, then
	// C is refused although C itself is fresh.
	for i := 0; i < 2; i++ {
		wantStatus(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: wrong, remote: ipB}), http.StatusUnauthorized)
	}
	wantError(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: wrong, remote: ipC}), http.StatusTooManyRequests, "rate_limited")
	// Another account identifier from C is unaffected (keys are per email).
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"email": "x@example.com", "password": "y"}, remote: ipC}), http.StatusUnauthorized)
	// An existing session keeps working throughout.
	wantStatus(t, e.do(req{method: "GET", path: "/api/v1/auth/me", h: e.session()}), http.StatusOK)
}

func TestTrustedProxyRateKeys(t *testing.T) {
	proxies, _ := ParsePrefixes([]string{"10.0.0.0/8"})
	e := newTenv(t, withOpts(func(o *Options) {
		o.TrustedProxies = proxies
		o.Limits.PublicReadPerIP = Rate{Burst: 2, Every: time.Hour}
	}))
	p := e.CreatePayment("1")
	get := func(remote, xff string) int {
		return e.do(req{method: "GET", path: "/api/v1/public/payments/" + p.ID, remote: remote, h: hdr{"X-Forwarded-For": xff}}).Code
	}
	// Two clients behind the same proxy have separate budgets.
	for i := 0; i < 2; i++ {
		if get("10.0.0.1:1", "198.51.100.1") != 200 || get("10.0.0.1:1", "198.51.100.2") != 200 {
			t.Fatal("refused within budget")
		}
	}
	if get("10.0.0.1:1", "198.51.100.1") != http.StatusTooManyRequests {
		t.Fatal("client 1 not limited")
	}
	// A client cannot escape by spoofing X-Forwarded-For when not behind a
	// trusted proxy.
	if get("203.0.113.5:1", "1.1.1.1") != 200 || get("203.0.113.5:1", "2.2.2.2") != 200 {
		t.Fatal("refused within budget")
	}
	if get("203.0.113.5:1", "3.3.3.3") != http.StatusTooManyRequests {
		t.Fatal("spoofed XFF escaped the limit")
	}
	// Nor by prepending fake hops in front of a trusted proxy.
	if get("10.0.0.1:1", "9.9.9.9, 198.51.100.1") != http.StatusTooManyRequests {
		t.Fatal("prepended hop escaped the limit")
	}
}

func TestLinkPaymentLimits(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) {
		o.Limits.LinkPaymentPerIP = Rate{Burst: 2, Every: time.Hour}
		o.Limits.LinkPaymentGlobal = Rate{Burst: 3, Every: time.Hour}
	}))
	if _, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Slug: "shop", Name: "Shop", Amount: "1", Active: true}); err != nil {
		t.Fatal(err)
	}
	post := func(remote string) int {
		return e.do(req{method: "POST", path: "/api/v1/public/links/shop/payments", body: map[string]any{}, remote: remote}).Code
	}
	if post("198.51.100.1:1") != 201 || post("198.51.100.1:1") != 201 {
		t.Fatal("refused within budget")
	}
	if post("198.51.100.1:1") != 429 {
		t.Fatal("per-IP limit not applied")
	}
	if post("198.51.100.2:1") != 201 {
		t.Fatal("other IP refused")
	}
	if post("198.51.100.3:1") != 429 {
		t.Fatal("global limit not applied")
	}
}

func TestSetupAndAuthFailureLimits(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) {
		o.Limits.SetupPerIP = Rate{Burst: 2, Every: time.Hour}
		o.Limits.AuthFailuresPerIP = Rate{Burst: 2, Every: time.Hour}
	}))
	op := e.session()
	for i := 0; i < 2; i++ {
		wantStatus(t, e.do(req{method: "POST", path: "/api/v1/setup/preview-addresses", body: map[string]any{"xpub": xpub}, h: op}), 200)
	}
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/preview-addresses", body: map[string]any{"xpub": xpub}, h: op}), 429, "rate_limited")

	for i := 0; i < 2; i++ {
		wantError(t, e.do(req{method: "GET", path: "/api/v1/payments", h: bearer("hmn_live_guess" + strconv.Itoa(i))}), 401, "invalid_credentials")
	}
	// Further invalid credentials from this IP get 429 instead of 401…
	wantError(t, e.do(req{method: "GET", path: "/api/v1/payments", h: bearer("hmn_live_guess9")}), 429, "rate_limited")
	wantError(t, e.do(req{method: "GET", path: "/api/v1/payments", h: hdr{"Authorization": "Token x"}}), 429, "rate_limited")
	// …but valid credentials from the same IP are never locked out.
	wantStatus(t, e.do(req{method: "GET", path: "/api/v1/settings", h: op}), 200)
	// Other IPs still get the plain 401.
	wantError(t, e.do(req{method: "GET", path: "/api/v1/payments", h: bearer("hmn_live_guess9"), remote: "198.51.100.77:1"}), 401, "invalid_credentials")
}
