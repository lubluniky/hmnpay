package e2e

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay"
	"github.com/lubluniky/hmnpay/core"
)

// Scenario 7: a payment link — public link page, a payment created from it
// by the payer, paid, and the link's statistics.
func TestPaymentLink(t *testing.T) {
	t.Parallel()
	in := newInstance(t)
	key := in.setup()

	link := decode[core.LinkView](t, in.must(201, "POST", "/api/v1/payment-links", map[string]any{
		"slug": "coffee-beans", "name": "Coffee beans", "amount": "10.00", "max_uses": 5,
	}, bearer(key)))
	if link.Slug != "coffee-beans" || link.Status != "active" || link.Uses != 0 {
		t.Fatalf("link = %+v", link)
	}

	// The payer's browser, anonymously.
	page := decode[struct {
		Slug   string  `json:"slug"`
		Name   string  `json:"name"`
		Amount *string `json:"amount"`
	}](t, in.must(200, "GET", "/api/v1/public/links/coffee-beans", nil, header("Cookie", "")))
	if page.Name != "Coffee beans" || page.Amount == nil || *page.Amount != "10.00" {
		t.Fatalf("public link = %+v", page)
	}
	body := map[string]any{"customer_email": "payer@example.net"}
	co := decode[core.CheckoutView](t, in.must(201, "POST", "/api/v1/public/links/coffee-beans/payments", body, header("Idempotency-Key", "visit-1")))
	again := decode[core.CheckoutView](t, in.must(200, "POST", "/api/v1/public/links/coffee-beans/payments", body, header("Idempotency-Key", "visit-1")))
	if again.ID != co.ID || co.Amount != "10.00" || co.Status != core.StatusAwaiting {
		t.Fatalf("link payment = %+v, replay %s", co, again.ID)
	}

	in.pay(co.Address, 10)
	in.mine(1)
	in.mine(settleDepth + confirmations)
	in.waitStatus(key, co.ID, core.StatusPaid)
	got := in.payment(key, co.ID)
	if got.LinkID == nil || *got.LinkID != link.ID || got.CustomerEmail != "payer@example.net" {
		t.Fatalf("payment from link = %+v", got)
	}

	stats := decode[core.LinkView](t, in.must(200, "GET", "/api/v1/payment-links/"+link.ID, nil, bearer(key)))
	if stats.Uses != 1 || stats.TotalCollected != "10.00" || stats.LastPaidAt == nil {
		t.Fatalf("link stats = %+v", stats)
	}
	byLink := decode[list[core.PaymentView]](t, in.must(200, "GET", "/api/v1/payments?link_id="+link.ID, nil, bearer(key)))
	if len(byLink.Data) != 1 || byLink.Data[0].ID != co.ID {
		t.Fatalf("payments of the link = %+v", byLink.Data)
	}
}

// Scenario 8: authorisation and hardening at the integration level.
func TestAuthAndHardening(t *testing.T) {
	t.Parallel()
	in := newInstance(t, func(o *instanceOptions) {
		o.limits = hmnpay.HTTPLimits{LoginPerIP: hmnpay.Rate{Burst: 3, Every: time.Hour}}
	})
	key := in.setup()
	_, whSecret := in.endpoint(key)
	// A bot's bearer session (before the login bucket is exhausted below).
	tok := decode[struct {
		Token string `json:"token"`
	}](t, in.must(200, "POST", "/api/v1/auth/login", map[string]any{"email": operatorEmail, "password": operatorPassword, "issue_token": true}))
	if !strings.HasPrefix(tok.Token, "hmns_") {
		t.Fatalf("session token %q", tok.Token)
	}

	t.Run("API key scopes", func(t *testing.T) {
		readOnly := in.createAPIKey("reporting", core.ScopePaymentsRead)
		r := in.must(403, "POST", "/api/v1/payments", map[string]any{"amount": "1.00"}, bearer(readOnly))
		if r.errCode() != "missing_scope" {
			t.Fatalf("create with a read-only key = %s", r)
		}
		in.must(200, "GET", "/api/v1/payments", nil, bearer(readOnly))
		// Operator-only areas are closed to every API key.
		in.must(403, "GET", "/api/v1/api-keys", nil, bearer(key))
		in.must(403, "GET", "/api/v1/settings", nil, bearer(key))
		in.must(401, "GET", "/api/v1/payments", nil, bearer("hmn_live_notakey"))
	})

	t.Run("webhook secrets are for the operator only", func(t *testing.T) {
		eps := decode[list[struct {
			ID     string `json:"id"`
			Secret string `json:"secret"`
		}]](t, in.must(200, "GET", "/api/v1/webhook-endpoints", nil, bearer(key)))
		if len(eps.Data) == 0 {
			t.Fatal("no endpoint")
		}
		id := eps.Data[0].ID
		if strings.Contains(eps.Data[0].Secret, whSecret[len(whSecret)-6:]) {
			t.Fatalf("an API key sees (part of) the signing secret: %q", eps.Data[0].Secret)
		}
		// Even a key holding webhooks:manage cannot read or rotate it…
		in.must(403, "POST", "/api/v1/webhook-endpoints/"+id+"/reveal-secret", nil, bearer(key))
		in.must(403, "POST", "/api/v1/webhook-endpoints/"+id+"/rotate-secret", nil, bearer(key))
		// …the operator can read it.
		got := decode[struct {
			Secret string `json:"secret"`
		}](t, in.must(200, "POST", "/api/v1/webhook-endpoints/"+id+"/reveal-secret", nil))
		if got.Secret != whSecret {
			t.Fatal("the operator's reveal returned another secret")
		}
	})

	t.Run("CSRF", func(t *testing.T) {
		// A cookie-authenticated write from another site's page.
		r := in.must(403, "POST", "/api/v1/api-keys", map[string]any{"name": "evil", "scopes": []string{"payments:read"}},
			header("Origin", "https://evil.example"), header("Sec-Fetch-Site", "cross-site"))
		if r.errCode() != "csrf_rejected" {
			t.Fatalf("cross-origin cookie POST = %s", r)
		}
		// The same request same-origin works.
		in.must(201, "POST", "/api/v1/api-keys", map[string]any{"name": "fine", "scopes": []string{"payments:read"}},
			header("Origin", in.srv.URL), header("Sec-Fetch-Site", "same-origin"))
		// A bearer token is not an ambient credential: no CSRF check.
		in.must(201, "POST", "/api/v1/payments", map[string]any{"amount": "1.00"}, bearer(key),
			header("Origin", "https://evil.example"), header("Sec-Fetch-Site", "cross-site"))
	})

	t.Run("login rate limit", func(t *testing.T) {
		for i := range 3 {
			r := in.call("POST", "/api/v1/auth/login", map[string]any{"email": operatorEmail, "password": "wrong password " + string(rune('a'+i))})
			if r.status != 401 {
				t.Fatalf("wrong password #%d = %s", i+1, r)
			}
		}
		r := in.call("POST", "/api/v1/auth/login", map[string]any{"email": operatorEmail, "password": operatorPassword})
		if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" {
			t.Fatalf("login after the burst = %s; want 429 with Retry-After", r)
		}
	})

	// Secrets stay out of the logs: after everything above, plus a bearer
	// session and a paid payment with webhooks, none of them was logged.
	in.must(200, "GET", "/api/v1/auth/me", nil, bearer(tok.Token))
	p := in.createPayment(key, "3.00")
	in.pay(p.Address, 3)
	in.mine(1)
	in.mine(settleDepth + confirmations)
	in.waitStatus(key, p.ID, core.StatusPaid)
	in.eventually("webhooks for the payment", func() bool { return len(in.recv.forPayment(p.ID)) >= 3 })
	var cookie string
	for _, c := range in.http.Jar.Cookies(mustURL(in.srv.URL)) {
		cookie = c.Value
	}
	logs := in.logs.String()
	for name, secret := range map[string]string{
		"operator password": operatorPassword, "API key": key, "webhook secret": whSecret, "session cookie": cookie,
		"session token": tok.Token, "xpub": hardhatXpub, "setup token": in.eng.SetupToken(),
	} {
		if secret == "" {
			t.Fatalf("test setup: no %s", name)
		}
		if strings.Contains(logs, secret) {
			t.Errorf("the %s appears in the logs", name)
		}
	}
	if strings.Contains(logs, p.ID) {
		t.Error("a payment id (a checkout capability) appears in the access log")
	}
}

// Scenario 9: a webhook endpoint answering 500 is retried on the core's
// schedule (1 min, then 5 min, …), with the same event id, until it
// succeeds. Time is advanced through the engine's injected clock.
func TestWebhookRetries(t *testing.T) {
	t.Parallel()
	in := newInstance(t)
	key := in.setup()
	endpointID, secret := in.endpoint(key)
	in.recv.setStatus(http.StatusInternalServerError)

	p := in.createPayment(key, "4.00")
	type deliveryView struct {
		ID            string              `json:"id"`
		EventID       string              `json:"event_id"`
		EventType     core.EventType      `json:"event_type"`
		Status        core.DeliveryStatus `json:"status"`
		Attempt       int                 `json:"attempt"`
		NextAttemptAt *time.Time          `json:"next_attempt_at"`
		ResponseCode  *int                `json:"response_code"`
	}
	deliveryAfter := func(attempts int) deliveryView {
		var d deliveryView
		in.eventually("the created delivery to have "+string(rune('0'+attempts))+" attempt(s)", func() bool {
			ds := decode[list[deliveryView]](t, in.must(200, "GET", "/api/v1/webhook-deliveries?endpoint_id="+endpointID+"&payment_id="+p.ID, nil, bearer(key)))
			for _, x := range ds.Data {
				if x.EventType == core.EventPaymentCreated && x.Attempt == attempts {
					d = x
					return true
				}
			}
			return false
		})
		return d
	}

	d := deliveryAfter(1)
	if d.Status != core.DeliveryRetrying || d.ResponseCode == nil || *d.ResponseCode != 500 || d.NextAttemptAt == nil {
		t.Fatalf("after the first failure = %+v", d)
	}
	if wait := d.NextAttemptAt.Sub(in.clock.Now()); wait < 50*time.Second || wait > 61*time.Second {
		t.Fatalf("next attempt in %s; want about 1 minute", wait)
	}
	// Nothing happens before it is due.
	time.Sleep(100 * time.Millisecond) // five dispatcher polls
	if n := len(in.recv.forPayment(p.ID)); n != 1 {
		t.Fatalf("%d requests before the retry was due", n)
	}

	// The chain's time goes with the clock, as a live chain's does: a clock
	// that runs a minute ahead of the newest block is clock skew, and the
	// chain bridge says so (an ERROR in the log, which fails any scenario
	// here) once it has seen it in three status reports — which this test
	// used to outrun, except on a busy machine.
	in.clock.Advance(61 * time.Second)
	in.mine(1)
	d = deliveryAfter(2)
	if wait := d.NextAttemptAt.Sub(in.clock.Now()); wait < 4*time.Minute || wait > 5*time.Minute+time.Second {
		t.Fatalf("second retry in %s; want about 5 minutes", wait)
	}

	in.recv.setStatus(http.StatusOK)
	in.clock.Advance(5*time.Minute + time.Second)
	in.mine(1)
	d = deliveryAfter(3)
	in.eventually("the delivery to succeed", func() bool {
		d = deliveryAfter(3)
		return d.Status == core.DeliverySucceeded
	})
	got := in.recv.forPayment(p.ID)
	if len(got) != 3 {
		t.Fatalf("receiver saw %d requests; want 3", len(got))
	}
	for _, x := range got {
		if x.EventID != d.EventID || x.DeliveryID != d.ID {
			t.Fatalf("retry changed identity: %+v vs delivery %s event %s", x, d.ID, d.EventID)
		}
	}
	verifySignatures(t, in.clock, secret, got)
	ep := decode[struct {
		ConsecutiveFailures int        `json:"consecutive_failures"`
		LastSuccessAt       *time.Time `json:"last_success_at"`
	}](t, in.must(200, "GET", "/api/v1/webhook-endpoints/"+endpointID, nil, bearer(key)))
	if ep.ConsecutiveFailures != 0 || ep.LastSuccessAt == nil {
		t.Fatalf("endpoint after recovery = %+v", ep)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// The known-device marker is signed with a key kept in the data directory,
// so a browser that logged in before is still exempt from the shared login
// buckets after a restart.
func TestDeviceKeySurvivesRestart(t *testing.T) {
	t.Parallel()
	in := newInstance(t, func(o *instanceOptions) {
		o.limits = hmnpay.HTTPLimits{
			LoginPerIP:      hmnpay.Rate{Burst: 100, Every: time.Second},
			LoginPerAccount: hmnpay.Rate{Burst: 2, Every: time.Hour},
		}
	})
	in.setup() // the browser now carries a known-device marker
	in.stop()
	in.start()

	// Someone else (no cookies) exhausts the operator account's bucket.
	stranger := &http.Client{Timeout: waitFor}
	statuses := []int{}
	for range 3 {
		req, _ := http.NewRequest("POST", in.srv.URL+"/api/v1/auth/login", strings.NewReader(`{"email":"`+operatorEmail+`","password":"guess guess guess"}`))
		req.Header.Set("Content-Type", "application/json")
		res, err := stranger.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		statuses = append(statuses, res.StatusCode)
	}
	if statuses[2] != http.StatusTooManyRequests {
		t.Fatalf("stranger's attempts = %v; want the account bucket exhausted", statuses)
	}
	// The owner's browser, known from before the restart, still gets in.
	in.must(200, "POST", "/api/v1/auth/login", map[string]any{"email": operatorEmail, "password": operatorPassword})
}
