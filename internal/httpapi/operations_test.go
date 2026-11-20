package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

func TestCancelPayment(t *testing.T) {
	e := newTenv(t)
	write := bearer(e.key(core.ScopePaymentsWrite))
	read := bearer(e.key(core.ScopePaymentsRead))
	p := e.CreatePayment("20")

	wantError(t, e.do(req{method: "POST", path: "/api/v1/payments/" + p.ID + "/cancel", h: read}), http.StatusForbidden, "missing_scope")
	w := e.do(req{method: "POST", path: "/api/v1/payments/" + p.ID + "/cancel", h: write})
	wantStatus(t, w, http.StatusOK)
	got := decodeBody[core.PaymentView](t, w)
	if got.Status != core.StatusExpired || got.CancelledAt == nil || got.CancelledBy != "test key" {
		t.Fatalf("cancelled %+v", got)
	}
	if !strings.Contains(w.Body.String(), `"cancelled_at":"`) {
		t.Fatalf("wire shape: %s", w.Body.String())
	}
	// Safe to retry.
	w = e.do(req{method: "POST", path: "/api/v1/payments/" + p.ID + "/cancel", h: write})
	wantStatus(t, w, http.StatusOK)
	if again := decodeBody[core.PaymentView](t, w); !again.CancelledAt.Equal(*got.CancelledAt) {
		t.Fatalf("second cancel %+v", again)
	}
	// The public checkout shows it; the event log says payment.expired.
	w = e.do(req{method: "GET", path: "/api/v1/public/payments/" + p.ID})
	if c := decodeBody[core.CheckoutView](t, w); c.Status != core.StatusExpired || c.CancelledAt == nil {
		t.Fatalf("checkout %+v", c)
	}
	w = e.do(req{method: "GET", path: "/api/v1/events?payment_id=" + p.ID + "&types=payment.expired", h: read})
	if evs := decodeBody[eventsBody](t, w); len(evs.Data) != 1 || !strings.Contains(string(evs.Data[0].Data), `"cancelled_at":"`) {
		t.Fatalf("events %s", w.Body.String())
	}
	// A naturally open payment has a null cancelled_at.
	q := e.CreatePayment("5")
	w = e.do(req{method: "GET", path: "/api/v1/payments/" + q.ID, h: read})
	if !strings.Contains(w.Body.String(), `"cancelled_at":null`) {
		t.Fatalf("wire shape: %s", w.Body.String())
	}

	// Funds in sight: conflict. Settled: conflict. Unknown: not found.
	e.Pay(q, "5", 100)
	wantError(t, e.do(req{method: "POST", path: "/api/v1/payments/" + q.ID + "/cancel", h: write}), http.StatusConflict, "payment_has_funds")
	e.Head(114)
	wantError(t, e.do(req{method: "POST", path: "/api/v1/payments/" + q.ID + "/cancel", h: write}), http.StatusConflict, "payment_settled")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/payments/pay_missing/cancel", h: write}), http.StatusNotFound, "payment_not_found")
	// Unknown body fields are rejected like everywhere else.
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/payments/" + p.ID + "/cancel", body: map[string]any{"reason": "x"}, h: write}), http.StatusBadRequest)
}

func TestDeleteLink(t *testing.T) {
	e := newTenv(t)
	lk := bearer(e.key(core.ScopeLinksWrite))
	w := e.do(req{method: "POST", path: "/api/v1/payment-links", body: map[string]any{"slug": "promo", "name": "Promo", "amount": "9"}, h: lk})
	wantStatus(t, w, http.StatusCreated)
	l := decodeBody[core.LinkView](t, w)
	w = e.do(req{method: "POST", path: "/api/v1/public/links/promo/payments", body: map[string]any{}})
	wantStatus(t, w, http.StatusCreated)
	pay := decodeBody[core.CheckoutView](t, w)

	wantError(t, e.do(req{method: "DELETE", path: "/api/v1/payment-links/" + l.ID, h: bearer(e.key(core.ScopePaymentsWrite))}), http.StatusForbidden, "missing_scope")
	w = e.do(req{method: "DELETE", path: "/api/v1/payment-links/" + l.ID, h: lk})
	wantStatus(t, w, http.StatusNoContent)
	if w.Body.Len() != 0 {
		t.Fatalf("204 with a body: %s", w.Body.String())
	}
	wantError(t, e.do(req{method: "DELETE", path: "/api/v1/payment-links/" + l.ID, h: lk}), http.StatusNotFound, "payment_link_not_found")
	wantError(t, e.do(req{method: "GET", path: "/api/v1/payment-links/" + l.ID, h: lk}), http.StatusNotFound, "payment_link_not_found")
	wantError(t, e.do(req{method: "GET", path: "/api/v1/public/links/promo"}), http.StatusNotFound, "not_found")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/public/links/promo/payments", body: map[string]any{}}), http.StatusNotFound, "not_found")
	// The payment made through it is still there, still payable, still linked.
	w = e.do(req{method: "GET", path: "/api/v1/public/payments/" + pay.ID})
	wantStatus(t, w, http.StatusOK)
	w = e.do(req{method: "GET", path: "/api/v1/payments?link_id=" + l.ID, h: bearer(e.key(core.ScopePaymentsRead))})
	wantStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), pay.ID) || !strings.Contains(w.Body.String(), `"link_id":"`+l.ID+`"`) {
		t.Fatalf("payments of a deleted link: %s", w.Body.String())
	}
	// The slug can be used again.
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/payment-links", body: map[string]any{"slug": "promo", "name": "Promo 2"}, h: lk}), http.StatusCreated)
}

func TestSessionsAndMe(t *testing.T) {
	e := newTenv(t)
	op := e.session()

	// me exposes the current session and its expiry.
	w := e.do(req{method: "GET", path: "/api/v1/auth/me", h: op})
	wantStatus(t, w, http.StatusOK)
	me := decodeBody[meResponse](t, w)
	// expires_at is when the session ends if it is not used again: the
	// idle timeout, counted from its last use.
	if me.Session == nil || !strings.HasPrefix(me.Session.ID, "sess_") || !me.Session.ExpiresAt.Equal(e.Clock.Now().Add(core.DefaultSessionIdleTimeout)) {
		t.Fatalf("me %+v", me)
	}
	// An API key has no session.
	w = e.do(req{method: "GET", path: "/api/v1/auth/me", h: bearer(e.key(core.ScopePaymentsRead))})
	if strings.Contains(w.Body.String(), `"session"`) {
		t.Fatalf("me of an API key: %s", w.Body.String())
	}

	// A second login, from elsewhere.
	e.Clock.Advance(time.Minute)
	w = e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"email": e.Email, "password": e.Password, "issue_token": true},
		h: hdr{"User-Agent": "hmn-cli/1.0"}, remote: "198.51.100.77:999"})
	wantStatus(t, w, http.StatusOK)
	cli := decodeBody[loginResponse](t, w)

	w = e.do(req{method: "GET", path: "/api/v1/operator/sessions", h: op})
	wantStatus(t, w, http.StatusOK)
	list := decodeBody[listBody[sessionInfoDTO]](t, w).Data
	if len(list) != 2 || list[0].ID != cli.Session.ID || list[0].Current || list[0].UserAgent != "hmn-cli/1.0" || list[0].IP != "198.51.100.77" ||
		list[1].ID != me.Session.ID || !list[1].Current {
		t.Fatalf("sessions %+v", list)
	}
	for _, leak := range []string{"token", "hash", cli.Token} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("session list contains %q: %s", leak, w.Body.String())
		}
	}
	// API keys cannot see or end sessions.
	wantError(t, e.do(req{method: "GET", path: "/api/v1/operator/sessions", h: bearer(e.key(core.ScopePaymentsRead))}), http.StatusForbidden, "forbidden")

	// Revoke the other session: its token stops working at once.
	wantStatus(t, e.do(req{method: "DELETE", path: "/api/v1/operator/sessions/" + cli.Session.ID, h: op}), http.StatusNoContent)
	wantError(t, e.do(req{method: "GET", path: "/api/v1/auth/me", h: bearer(cli.Token)}), http.StatusUnauthorized, "invalid_credentials")
	wantError(t, e.do(req{method: "DELETE", path: "/api/v1/operator/sessions/" + cli.Session.ID, h: op}), http.StatusNotFound, "session_not_found")
	// A cross-site request cannot revoke sessions with the cookie.
	evil := merge(op, hdr{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"})
	wantError(t, e.do(req{method: "DELETE", path: "/api/v1/operator/sessions/" + me.Session.ID, h: evil}), http.StatusForbidden, "csrf_rejected")
	// Revoking the own session logs out and clears the cookie.
	w = e.do(req{method: "DELETE", path: "/api/v1/operator/sessions/" + me.Session.ID, h: op})
	wantStatus(t, w, http.StatusNoContent)
	if c := findCookie(w, cookieSecure); c == nil || c.MaxAge >= 0 {
		t.Fatalf("cookie not cleared: %v", w.Result().Cookies())
	}
	wantError(t, e.do(req{method: "GET", path: "/api/v1/auth/me", h: op}), http.StatusUnauthorized, "invalid_credentials")
}

func TestWebhookEndpointURLAndLateAttempts(t *testing.T) {
	// The wiring's egress policy is enforced by the core for every
	// transport; the HTTP layer adds nothing of its own.
	e := newTenv(t, withEnv(coretest.WithConfig(func(c *core.Config) {
		c.ValidateWebhookURL = func(u string) error {
			if strings.Contains(u, "169.254.") {
				return &core.Error{Kind: core.KindInvalid, Message: "destination is not allowed"}
			}
			return nil
		}
	})))
	hk := bearer(e.key(core.ScopeWebhooksManage))
	post := func(url string) errResp {
		t.Helper()
		w := e.do(req{method: "POST", path: "/api/v1/webhook-endpoints", body: map[string]any{"url": url, "events": []string{"payment.paid"}}, h: hk})
		return wantError(t, w, http.StatusUnprocessableEntity, "validation_failed")
	}
	if er := post("https://user:hunter2@hooks.example.com/a"); er.Error.Fields[0].Field != "url" || er.Error.Fields[0].Code != "credentials" {
		t.Fatalf("fields %+v", er.Error.Fields)
	}
	if strings.Contains(e.logs.String(), "hunter2") {
		t.Fatal("URL credentials in logs")
	}
	if er := post("http://169.254.169.254/latest/meta-data"); er.Error.Fields[0].Field != "url" || er.Error.Fields[0].Code != "not_allowed" || er.Error.Fields[0].Message != "destination is not allowed" {
		t.Fatalf("fields %+v", er.Error.Fields)
	}

	// A late attempt is visible as such on the wire.
	w := e.do(req{method: "POST", path: "/api/v1/webhook-endpoints", body: map[string]any{"url": "https://hooks.example.com/a", "events": []string{"payment.created"}}, h: hk})
	wantStatus(t, w, http.StatusCreated)
	e.CreatePayment("1")
	sys := e.System()
	if _, err := e.Service.PlanWebhookDeliveries(sys); err != nil {
		t.Fatal(err)
	}
	jobs, err := e.Service.ClaimWebhookDeliveries(sys, 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v %v", jobs, err)
	}
	if _, err := e.Service.ResendWebhookDelivery(e.Operator(), jobs[0].DeliveryID); err != nil { // supersedes the claim
		t.Fatal(err)
	}
	res := jobs[0].Result()
	res.ResponseCode = 200
	if out, err := e.Service.CompleteWebhookDelivery(sys, jobs[0].DeliveryID, res); err != nil || out != core.CompletionLate {
		t.Fatalf("complete: %s %v", out, err)
	}
	w = e.do(req{method: "GET", path: "/api/v1/webhook-deliveries/" + jobs[0].DeliveryID, h: hk})
	wantStatus(t, w, http.StatusOK)
	d := decodeBody[deliveryDTO](t, w)
	if d.Status != core.DeliverySucceeded || d.Attempt != 1 || len(d.Attempts) != 1 || !d.Attempts[0].Late || !d.Attempts[0].Succeeded {
		t.Fatalf("delivery %+v", d)
	}
	// The fencing token is internal.
	if strings.Contains(w.Body.String(), "claim") || strings.Contains(w.Body.String(), jobs[0].ClaimID) {
		t.Fatalf("delivery exposes its claim: %s", w.Body.String())
	}
}

// viewCounter counts read transactions.
type viewCounter struct {
	core.Store
	views atomic.Int64
}

func (s *viewCounter) View(ctx context.Context, fn func(core.Reader) error) error {
	s.views.Add(1)
	return s.Store.View(ctx, fn)
}

// A checkout stream wakes only for events of its own payment: activity
// elsewhere in the system must not make every open stream query the store.
func TestStreamWakesOnlyForItsPayment(t *testing.T) {
	store := &viewCounter{Store: coretest.NewMemStore()}
	e := newTenv(t, withEnv(coretest.WithStore(store)), withOpts(func(o *Options) {
		// Polling off (in effect): only nudges can cause reads.
		o.Stream = StreamOptions{Heartbeat: 40 * time.Millisecond, ActivePoll: time.Hour, IdlePoll: time.Hour, Debounce: 5 * time.Millisecond, MaxLifetime: time.Minute}
	}))
	srv := startServer(t, e.h)
	p := e.CreatePayment("10")
	c := openStream(t, srv.URL+"/api/v1/public/payments/"+p.ID+"/stream", nil)
	if _, v := c.next(nil); v.Status != core.StatusAwaiting {
		t.Fatalf("initial %+v", v)
	}
	time.Sleep(50 * time.Millisecond)
	idle := store.views.Load()

	// Plenty happens to other payments.
	for range 25 {
		other := e.CreatePayment("1")
		e.Pay(other, "1", 100)
	}
	c.quiet(200 * time.Millisecond)
	if n := store.views.Load() - idle; n != 0 {
		t.Fatalf("the stream read the store %d times for events of other payments", n)
	}
	// Its own payment still gets through at once.
	e.Pay(p, "10", 100)
	if _, v := c.next(nil); v.Status != core.StatusConfirming {
		t.Fatalf("own event %+v", v)
	}
	if store.views.Load() == idle {
		t.Fatal("no read for the stream's own event")
	}
}
