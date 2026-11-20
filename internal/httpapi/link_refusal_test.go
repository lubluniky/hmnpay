package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// A checkout the link refuses (used up: 409; below the minimum: 422) takes
// nothing, so it gives back the per-IP-and-link token: a customer told
// "used up" is not locked out of the link when a use frees up. The
// per-IP budget is still spent by refusals, so they cannot be made at will.
func TestRefusedLinkCheckoutKeepsThePerLinkBudget(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) {
		o.Limits.LinkPaymentPerIPPerLink = Rate{} // production default: 2 burst, one per 5 minutes
		o.Limits.LinkPaymentPerIP = Rate{Burst: 6, Every: time.Hour}
	}))
	e.Head(100)
	w := e.do(req{method: "POST", path: "/api/v1/payment-links", h: e.session(),
		body: map[string]any{"slug": "launch", "name": "Launch ticket", "amount": "25", "max_uses": 1}})
	wantStatus(t, w, http.StatusCreated)
	checkout := func(remote string, body map[string]any) int {
		t.Helper()
		return e.do(req{method: "POST", path: "/api/v1/public/links/launch/payments", remote: remote, body: body}).Code
	}
	if code := checkout("198.51.100.1:1", map[string]any{}); code != http.StatusCreated {
		t.Fatalf("squatter: %d", code)
	}
	const customer = "203.0.113.9:1"
	for i := range 3 {
		if code := checkout(customer, map[string]any{}); code != http.StatusConflict {
			t.Fatalf("refused checkout %d: %d, want 409", i+1, code)
		}
	}
	e.Clock.Advance(core.DefaultLinkReservation + core.DefaultExpiryGrace)
	e.Head(101)
	if code := checkout(customer, map[string]any{}); code != http.StatusCreated {
		t.Fatalf("after three refusals the customer gets %d when the use frees up, want 201", code)
	}
	// Two more from that address and its per-IP budget (6) is spent,
	// refusals included: they are not free.
	checkout(customer, map[string]any{})
	checkout(customer, map[string]any{})
	if code := checkout(customer, map[string]any{}); code != http.StatusTooManyRequests {
		t.Fatalf("seventh request from one address: %d, want 429", code)
	}
}

// Before the chain watcher has processed its first block no payment is
// created: 503 chain_not_ready with Retry-After, for the API and for a
// payment link alike. A link checkout refused that way takes nothing either,
// so it gives back the per-IP-and-link token: the checkout page retries by
// itself, and a customer must not be locked out of the link for minutes by
// a wait of seconds. Once there is a head, the same request goes through.
func TestChainNotReadyIsRetryableAndKeepsThePerLinkBudget(t *testing.T) {
	e := newTenv(t, withEnv(coretest.WithoutChainHead()), withOpts(func(o *Options) {
		o.Limits.LinkPaymentPerIPPerLink = Rate{} // production default: 2 burst, one per 5 minutes
		o.Limits.LinkPaymentPerIP = Rate{Burst: 10, Every: time.Hour}
	}))
	w := e.do(req{method: "POST", path: "/api/v1/payment-links", h: e.session(), body: map[string]any{"slug": "launch", "name": "Launch ticket", "amount": "25"}})
	wantStatus(t, w, http.StatusCreated) // links can be made; only payments wait for the chain
	for _, path := range []string{"/api/v1/payments", "/api/v1/public/links/launch/payments"} {
		body := map[string]any{}
		h := hdr{}
		if path == "/api/v1/payments" {
			body["amount"] = "25"
			h = e.session()
		}
		for i := range 4 {
			w := e.do(req{method: "POST", path: path, remote: "203.0.113.9:1", body: body, h: h})
			wantError(t, w, http.StatusServiceUnavailable, "chain_not_ready")
			if got := w.Header().Get("Retry-After"); got != "5" {
				t.Fatalf("%s attempt %d: Retry-After %q, want 5", path, i+1, got)
			}
		}
	}
	e.Head(100)
	w = e.do(req{method: "POST", path: "/api/v1/public/links/launch/payments", remote: "203.0.113.9:1", body: map[string]any{}})
	wantStatus(t, w, http.StatusCreated)
	w = e.do(req{method: "POST", path: "/api/v1/payments", body: map[string]any{"amount": "25"}, h: e.session()})
	wantStatus(t, w, http.StatusCreated)
}

// A client that retries exactly as the answer says — Retry-After, no
// sooner — is never answered 429 while the instance cannot take payments,
// with the production budgets of the public link route, however long the
// state lasts: chain_not_ready (no processed block yet), clock_behind (the
// server clock behind the chain), network_invalid (stored network settings
// that cannot be used) and watch_list_full (the links' share of the watch
// list used up; the API's own route is never refused by it). The same holds
// for the API's own route. When the state ends, the next retry goes through.
func TestRetryingAsToldIsNeverRateLimited(t *testing.T) {
	linkOnly := []string{"/api/v1/public/links/launch/payments"}
	both := []string{"/api/v1/public/links/launch/payments", "/api/v1/payments"}
	storedURLs := func(e *tenv, urls []string) {
		t.Helper()
		if err := e.Store.Update(t.Context(), func(tx core.Tx) error {
			st, err := tx.GetSettings(t.Context())
			if err != nil {
				return err
			}
			st.Network.RPCURLs = urls
			return tx.SaveSettings(t.Context(), st)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, code, after string
		paths             []string
		opts              []coretest.EnvOption
		enter, leave      func(e *tenv)
	}{
		{"chain_not_ready", "chain_not_ready", "5", both, []coretest.EnvOption{coretest.WithoutChainHead()},
			func(*tenv) {}, func(e *tenv) { e.Head(100) }},
		{"clock_behind", "clock_behind", "60", both, []coretest.EnvOption{coretest.WithConfig(func(c *core.Config) { c.MaxHeadTimeAhead = 2 * time.Minute })},
			func(e *tenv) { e.Head(100); e.Clock.Set(e.Clock.Now().Add(-24 * time.Hour)) }, func(e *tenv) { e.Clock.Set(e.Clock.Now().Add(24 * time.Hour)) }},
		{"network_invalid", "network_invalid", "60", both, nil,
			// Two URLs of one endpoint, as an earlier version could store them.
			func(e *tenv) { storedURLs(e, []string{"https://rpc.example.com/a", "https://rpc.example.com/b"}) },
			func(e *tenv) { storedURLs(e, []string{"https://rpc.example.com/a", "https://other.example.net"}) }},
		{"watch_list_full", "watch_list_full", "60", linkOnly, []coretest.EnvOption{coretest.WithConfig(func(c *core.Config) { c.MaxWatchedAddresses = 1 })},
			func(e *tenv) {
				w := e.do(req{method: "POST", path: "/api/v1/public/links/launch/payments", remote: "198.51.100.7:1", body: map[string]any{}})
				wantStatus(t, w, http.StatusCreated) // somebody else's checkout takes the one place
			},
			// (Its end — the other checkout leaving the list — needs a processed head with a final
			// block, which the core's own tests cover; here the retries are what is under test.)
			nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTenv(t, withEnv(c.opts...), withOpts(func(o *Options) {
				// The production defaults of every budget of the route.
				o.Limits.LinkPaymentPerIP, o.Limits.LinkPaymentPerIPPerLink = Rate{}, Rate{}
				o.Limits.LinkPaymentPerLink, o.Limits.LinkPaymentGlobal = Rate{}, Rate{}
			}))
			e.h.now = e.Clock.Now // the budgets refill on the test's clock
			w := e.do(req{method: "POST", path: "/api/v1/payment-links", h: e.session(), body: map[string]any{"slug": "launch", "name": "Launch ticket", "amount": "25"}})
			wantStatus(t, w, http.StatusCreated)
			c.enter(e)
			for _, path := range c.paths {
				body, h := map[string]any{}, hdr{"Idempotency-Key": "one-checkout-" + c.name}
				if path == "/api/v1/payments" {
					body["amount"], h = "25", e.session()
				}
				// An hour of retries, each as soon as allowed.
				for elapsed := time.Duration(0); elapsed < time.Hour; {
					w := e.do(req{method: "POST", path: path, remote: "203.0.113.9:1", body: body, h: h})
					if w.Code == http.StatusTooManyRequests {
						t.Fatalf("%s after %s of retrying as told: 429 %s", path, elapsed, w.Body.String())
					}
					wantError(t, w, http.StatusServiceUnavailable, c.code)
					if got := w.Header().Get("Retry-After"); got != c.after {
						t.Fatalf("%s: Retry-After %q, want %s", path, got, c.after)
					}
					wait, _ := time.ParseDuration(w.Header().Get("Retry-After") + "s")
					e.Clock.Advance(wait)
					elapsed += wait
				}
			}
			if c.leave == nil {
				return
			}
			c.leave(e)
			e.Head(101)
			w = e.do(req{method: "POST", path: "/api/v1/public/links/launch/payments", remote: "203.0.113.9:1", body: map[string]any{}, h: hdr{"Idempotency-Key": "one-checkout-" + c.name}})
			wantStatus(t, w, http.StatusCreated)
		})
	}
	if chainNotReadyRetry < (Limits{}).withDefaults().LinkPaymentPerLink.Every {
		t.Fatalf("chainNotReadyRetry %s is shorter than the per-link refill", chainNotReadyRetry)
	}
}
