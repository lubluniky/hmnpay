package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// fixtures creates one of everything the routes refer to.
type fixtures struct {
	payment, link, endpoint, delivery, keyID string
}

func (e *tenv) fixtures() fixtures {
	e.t.Helper()
	op := e.Operator()
	p := e.CreatePayment("10")
	l, err := e.Service.CreateLink(op, core.LinkInput{Name: "Tip jar", Active: true})
	if err != nil {
		e.t.Fatal(err)
	}
	ep, err := e.Service.CreateWebhookEndpoint(op, core.WebhookEndpointInput{URL: "https://hooks.example.com/x", Events: []core.EventType{core.EventPaymentPaid}, Enabled: true})
	if err != nil {
		e.t.Fatal(err)
	}
	d, err := e.Service.SendTestWebhook(op, ep.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	k, _, err := e.Service.CreateAPIKey(op, core.CreateAPIKeyInput{Name: "fixture", Scopes: []core.Scope{core.ScopePaymentsRead}})
	if err != nil {
		e.t.Fatal(err)
	}
	return fixtures{payment: p.ID, link: l.ID, endpoint: ep.ID, delivery: d.ID, keyID: k.ID}
}

func TestAuthMatrix(t *testing.T) {
	e := newTenv(t)
	f := e.fixtures()
	creds := map[string]hdr{
		"read":  bearer(e.key(core.ScopePaymentsRead)),
		"write": bearer(e.key(core.ScopePaymentsWrite)),
		"links": bearer(e.key(core.ScopeLinksWrite)),
		"hooks": bearer(e.key(core.ScopeWebhooksManage)),
		"op":    e.session(),
	}
	linkBody := map[string]any{"name": "Updated", "active": true}
	epBody := map[string]any{"url": "https://hooks.example.com/y", "events": []string{"payment.paid"}, "enabled": true}
	cases := []struct {
		method, path string
		body         any
		allowed      string // space-separated credential names
	}{
		{"GET", "/api/v1/auth/me", nil, "read write links hooks op"},
		{"GET", "/api/v1/instance", nil, "read write links hooks op"},
		{"POST", "/api/v1/payments", map[string]any{"amount": "1.50"}, "write op"},
		{"GET", "/api/v1/payments", nil, "read op"},
		{"GET", "/api/v1/payments/" + f.payment, nil, "read op"},
		{"GET", "/api/v1/payments/" + f.payment + "/detail", nil, "read op"},
		{"POST", "/api/v1/payments/" + f.payment + "/resolve", map[string]any{"note": "x"}, "op"},
		{"POST", "/api/v1/payments/" + f.payment + "/reopen", nil, "op"},
		{"POST", "/api/v1/payments/" + f.payment + "/cancel", nil, "write op"},
		{"GET", "/api/v1/overview?days=7", nil, "read op"},
		{"GET", "/api/v1/node-status", nil, "read op"},
		{"GET", "/api/v1/events", nil, "read op"},
		{"POST", "/api/v1/payment-links", map[string]any{"name": "New"}, "links op"},
		{"GET", "/api/v1/payment-links", nil, "links op"},
		{"GET", "/api/v1/payment-links/" + f.link, nil, "links op"},
		{"PUT", "/api/v1/payment-links/" + f.link, linkBody, "links op"},
		{"POST", "/api/v1/payment-links/" + f.link + "/deactivate", nil, "links op"},
		{"POST", "/api/v1/payment-links/" + f.link + "/activate", nil, "links op"},
		{"DELETE", "/api/v1/payment-links/plink_missing", nil, "links op"},
		{"POST", "/api/v1/webhook-endpoints", epBody, "hooks op"},
		{"GET", "/api/v1/webhook-endpoints", nil, "hooks op"},
		{"GET", "/api/v1/webhook-endpoints/" + f.endpoint, nil, "hooks op"},
		{"PUT", "/api/v1/webhook-endpoints/" + f.endpoint, epBody, "hooks op"},
		// Rotating and revealing hand out a signing secret: operator only.
		{"POST", "/api/v1/webhook-endpoints/" + f.endpoint + "/rotate-secret", nil, "op"},
		{"POST", "/api/v1/webhook-endpoints/" + f.endpoint + "/reveal-secret", nil, "op"},
		{"POST", "/api/v1/webhook-endpoints/" + f.endpoint + "/test", nil, "hooks op"},
		{"DELETE", "/api/v1/webhook-endpoints/we_missing", nil, "hooks op"},
		{"GET", "/api/v1/webhook-deliveries", nil, "hooks op"},
		{"GET", "/api/v1/webhook-deliveries/" + f.delivery, nil, "hooks op"},
		{"POST", "/api/v1/webhook-deliveries/" + f.delivery + "/resend", nil, "hooks op"},
		{"POST", "/api/v1/api-keys", map[string]any{"name": "k", "scopes": []string{"payments:read"}}, "op"},
		{"GET", "/api/v1/api-keys", nil, "op"},
		{"POST", "/api/v1/api-keys/key_missing/revoke", nil, "op"},
		{"GET", "/api/v1/settings", nil, "op"},
		{"PUT", "/api/v1/settings/merchant", map[string]any{"merchant_name": "N", "support_email": "a@b.co"}, "op"},
		{"PUT", "/api/v1/settings/payments", map[string]any{"ttl_seconds": 1800, "required_confirmations": 15}, "op"},
		{"PUT", "/api/v1/settings/network", map[string]any{"rpc_urls": []string{"https://rpc.example.org"}}, "op"},
		{"PUT", "/api/v1/settings/wallet", map[string]any{"xpub": "xpub6CUGRUonZSQ4TWtTMmzXdrXDtypWKiKrhko4egpiMZbpiaQL2jkwSB1icqYh2cfDfVxdx4df189oLKnC5fSwqPfgyP3hooxujYzAu3fDVmz"}, "op"},
		{"GET", "/api/v1/operator", nil, "op"},
		{"GET", "/api/v1/operator/sessions", nil, "op"},
		{"DELETE", "/api/v1/operator/sessions/sess_missing", nil, "op"},
		{"POST", "/api/v1/setup/probe-rpc", map[string]any{"url": "https://rpc.example.org"}, "op"},
		{"POST", "/api/v1/setup/preview-addresses", map[string]any{"xpub": "xpub6CUGRUonZSQ4TWtTMmzXdrXDtypWKiKrhko4egpiMZbpiaQL2jkwSB1icqYh2cfDfVxdx4df189oLKnC5fSwqPfgyP3hooxujYzAu3fDVmz"}, "op"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			w := e.do(req{method: c.method, path: c.path, body: c.body})
			wantError(t, w, http.StatusUnauthorized, "unauthorized")
			if w.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without WWW-Authenticate")
			}
			allowed := " " + c.allowed + " "
			for name, h := range creds {
				w := e.do(req{method: c.method, path: c.path, body: c.body, h: h})
				ok := strings.Contains(allowed, " "+name+" ")
				switch {
				case ok && (w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden || w.Code >= 500):
					t.Errorf("%s: status %d, want access; body %s", name, w.Code, w.Body.String())
				case !ok && w.Code != http.StatusForbidden:
					t.Errorf("%s: status %d, want 403; body %s", name, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestSetupFlow(t *testing.T) {
	e := newTenv(t, withEnv(coretest.WithoutSetup()))
	w := e.do(req{method: "GET", path: "/api/v1/setup"})
	wantStatus(t, w, http.StatusOK)
	st := decodeBody[setupStatusDTO](t, w)
	if st.Completed || st.MinPasswordLength != core.MinPasswordLength || len(st.SupportedChains) != 2 || st.Defaults.Payments.TTLSeconds != 1800 {
		t.Fatalf("setup status %+v", st)
	}

	// Wizard helpers are public before setup.
	w = e.do(req{method: "POST", path: "/api/v1/setup/probe-rpc", body: map[string]any{"url": "https://rpc.example.org", "chain_id": 97}})
	wantStatus(t, w, http.StatusOK)
	if pr := decodeBody[probeResultDTO](t, w); pr.Outcome != core.ProbeOK || pr.Expected.ChainID != 97 {
		t.Fatalf("probe %+v", pr)
	}
	w = e.do(req{method: "POST", path: "/api/v1/setup/preview-addresses", body: map[string]any{"xpub": xpub, "count": 2}})
	wantStatus(t, w, http.StatusOK)
	if pv := decodeBody[listBody[derivedAddressDTO]](t, w); len(pv.Data) != 2 || pv.Data[1].Path != "m/44'/60'/0'/0/1" {
		t.Fatalf("preview %+v", pv)
	}
	// A private key is refused as a field error on xpub.
	w = e.do(req{method: "POST", path: "/api/v1/setup/preview-addresses", body: map[string]any{"xpub": "xprv9s21ZrQH143K"}})
	if er := wantError(t, w, http.StatusUnprocessableEntity, "validation_failed"); er.Error.Fields[0].Field != "xpub" {
		t.Fatalf("fields %+v", er.Error.Fields)
	}

	// Nothing else works yet.
	wantError(t, e.do(req{method: "GET", path: "/api/v1/payments"}), http.StatusUnauthorized, "unauthorized")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"email": "a@b.co", "password": "x"}}), http.StatusServiceUnavailable, "setup_required")
	// The public checkout surface never reports setup state: nothing exists
	// yet, so every id and slug is simply not found.
	wantError(t, e.do(req{method: "GET", path: "/api/v1/public/links/abc"}), http.StatusNotFound, "not_found")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/public/links/abc/payments", body: map[string]any{}}), http.StatusNotFound, "not_found")
	wantError(t, e.do(req{method: "GET", path: "/api/v1/public/payments/pay_0000000000000000000000000z"}), http.StatusNotFound, "not_found")

	// Invalid setup reports request keys.
	w = e.do(req{method: "POST", path: "/api/v1/setup/complete", body: map[string]any{"email": "owner@example.com"}})
	er := wantError(t, w, http.StatusUnprocessableEntity, "validation_failed")
	fields := map[string]bool{}
	for _, f := range er.Error.Fields {
		fields[f.Field] = true
	}
	for _, want := range []string{"password", "merchant_name", "support_email", "rpc_urls", "xpub"} {
		if !fields[want] {
			t.Errorf("missing field error %q in %+v", want, er.Error.Fields)
		}
	}

	body := map[string]any{
		"email": "owner@example.com", "password": "correct horse battery staple",
		"chain_id": 56, "rpc_urls": []string{"https://rpc.example.org"}, "xpub": xpub,
		"merchant_name": "Northwind", "support_email": "help@example.com",
	}
	w = e.do(req{method: "POST", path: "/api/v1/setup/complete", body: body, h: hdr{"Origin": testOrigin}})
	wantStatus(t, w, http.StatusCreated)
	c := findCookie(w, cookieSecure)
	if c == nil || !strings.HasPrefix(c.Value, "hmns_") {
		t.Fatalf("no session cookie: %v", w.Result().Cookies())
	}
	if lr := decodeBody[loginResponse](t, w); lr.Operator.Email != "owner@example.com" || lr.Token != "" {
		t.Fatalf("login response %+v", lr)
	}
	sess := hdr{"Cookie": cookieSecure + "=" + c.Value, "Origin": testOrigin}

	// The public URL defaulted to Options.PublicURL.
	w = e.do(req{method: "GET", path: "/api/v1/settings", h: sess})
	wantStatus(t, w, http.StatusOK)
	if s := decodeBody[settingsDTO](t, w); s.PublicURL != testOrigin || !s.SetupCompleted {
		t.Fatalf("settings %+v", s)
	}

	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/complete", body: body}), http.StatusConflict, "setup_completed")
	// After setup the helpers are operator-only.
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/probe-rpc", body: map[string]any{"url": "https://rpc.example.org"}}), http.StatusUnauthorized, "unauthorized")
	// …and an invalid cookie does not make them public again.
	wantError(t, e.do(req{method: "POST", path: "/api/v1/setup/probe-rpc", body: map[string]any{"url": "https://rpc.example.org"},
		h: hdr{"Cookie": cookieSecure + "=hmns_bogus"}}), http.StatusUnauthorized, "unauthorized")
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/setup/probe-rpc", body: map[string]any{"url": "https://rpc.example.org"}, h: sess}), http.StatusOK)
	w = e.do(req{method: "GET", path: "/api/v1/setup"})
	if !decodeBody[setupStatusDTO](t, w).Completed {
		t.Fatal("setup not reported complete")
	}
}

const xpub = "xpub6CUGRUonZSQ4TWtTMmzXdrXDtypWKiKrhko4egpiMZbpiaQL2jkwSB1icqYh2cfDfVxdx4df189oLKnC5fSwqPfgyP3hooxujYzAu3fDVmz"

func TestPayments(t *testing.T) {
	e := newTenv(t)
	write := bearer(e.key(core.ScopePaymentsWrite))
	read := bearer(e.key(core.ScopePaymentsRead))
	body := map[string]any{"amount": "149.00", "order_id": "ORD-1", "metadata": map[string]string{"plan": "pro"}, "ttl_seconds": 3600}

	w := e.do(req{method: "POST", path: "/api/v1/payments", body: body, h: merge(write, hdr{"Idempotency-Key": "k-1"})})
	wantStatus(t, w, http.StatusCreated)
	p := decodeBody[core.PaymentView](t, w)
	if p.Status != core.StatusAwaiting || p.Amount != "149.00" || p.Metadata["plan"] != "pro" || w.Header().Get("Location") != "/api/v1/payments/"+p.ID {
		t.Fatalf("created %+v", p)
	}
	if p.ExpiresAt.Sub(p.CreatedAt).Seconds() != 3600 {
		t.Fatalf("ttl not applied: %v", p.ExpiresAt.Sub(p.CreatedAt))
	}
	// Same key, same body: the same payment, 200.
	w = e.do(req{method: "POST", path: "/api/v1/payments", body: body, h: merge(write, hdr{"Idempotency-Key": "k-1"})})
	wantStatus(t, w, http.StatusOK)
	if decodeBody[core.PaymentView](t, w).ID != p.ID {
		t.Fatal("idempotent retry created a new payment")
	}
	// Same key, other body: 409.
	body2 := map[string]any{"amount": "150.00", "order_id": "ORD-1"}
	wantError(t, e.do(req{method: "POST", path: "/api/v1/payments", body: body2, h: merge(write, hdr{"Idempotency-Key": "k-1"})}), http.StatusConflict, "idempotency_key_reused")
	// A bad key is a field error named after the header.
	er := wantError(t, e.do(req{method: "POST", path: "/api/v1/payments", body: body2, h: merge(write, hdr{"Idempotency-Key": "has space"})}), http.StatusUnprocessableEntity, "validation_failed")
	if er.Error.Fields[0].Field != "Idempotency-Key" {
		t.Fatalf("fields %+v", er.Error.Fields)
	}
	// Validation: field names are request keys.
	er = wantError(t, e.do(req{method: "POST", path: "/api/v1/payments", body: map[string]any{"amount": "0", "ttl_seconds": 5}, h: write}), http.StatusUnprocessableEntity, "validation_failed")
	got := map[string]string{}
	for _, f := range er.Error.Fields {
		got[f.Field] = f.Code
	}
	if got["amount"] != "too_small" || got["ttl_seconds"] != "invalid" {
		t.Fatalf("fields %+v", er.Error.Fields)
	}

	// More payments for listing.
	small := e.CreatePayment("5")
	e.Clock.Advance(1e9)
	big := e.CreatePayment("500")

	w = e.do(req{method: "GET", path: "/api/v1/payments/" + p.ID, h: read})
	wantStatus(t, w, http.StatusOK)
	wantError(t, e.do(req{method: "GET", path: "/api/v1/payments/pay_missing", h: read}), http.StatusNotFound, "payment_not_found")

	type listResp struct {
		Data   []core.PaymentView `json:"data"`
		Page   pageDTO            `json:"page"`
		Counts *countsDTO         `json:"counts"`
	}
	w = e.do(req{method: "GET", path: "/api/v1/payments?sort=amount&dir=asc&limit=2&counts=true", h: read})
	wantStatus(t, w, http.StatusOK)
	lr := decodeBody[listResp](t, w)
	if len(lr.Data) != 2 || lr.Data[0].ID != small.ID || lr.Page.Total != 3 || !lr.Page.HasMore || lr.Page.Limit != 2 {
		t.Fatalf("list %+v", lr)
	}
	if lr.Counts == nil || lr.Counts.All != 3 || lr.Counts.Open != 3 || lr.Counts.Paid != 0 {
		t.Fatalf("counts %+v", lr.Counts)
	}
	w = e.do(req{method: "GET", path: "/api/v1/payments?sort=amount&dir=asc&limit=2&offset=2", h: read})
	if lr := decodeBody[listResp](t, w); len(lr.Data) != 1 || lr.Data[0].ID != big.ID || lr.Page.HasMore || lr.Counts != nil {
		t.Fatalf("page 2 %+v", lr)
	}
	w = e.do(req{method: "GET", path: "/api/v1/payments?q=ORD-1", h: read})
	if lr := decodeBody[listResp](t, w); len(lr.Data) != 1 || lr.Data[0].ID != p.ID {
		t.Fatalf("search %+v", lr)
	}
	w = e.do(req{method: "GET", path: "/api/v1/payments?view=paid", h: read})
	if lr := decodeBody[listResp](t, w); len(lr.Data) != 0 || lr.Page.Total != 0 {
		t.Fatalf("paid view %+v", lr)
	}
	w = e.do(req{method: "GET", path: "/api/v1/payments?status=awaiting,detected", h: read})
	if lr := decodeBody[listResp](t, w); lr.Page.Total != 3 {
		t.Fatalf("status filter %+v", lr)
	}
	day := e.Clock.Now().Format("2006-01-02")
	w = e.do(req{method: "GET", path: "/api/v1/payments?created_from=" + day + "&created_to=" + day, h: read})
	if lr := decodeBody[listResp](t, w); lr.Page.Total != 3 {
		t.Fatalf("date range %+v", lr)
	}
	w = e.do(req{method: "GET", path: "/api/v1/payments?created_to=2020-01-01", h: read})
	if lr := decodeBody[listResp](t, w); lr.Page.Total != 0 {
		t.Fatalf("date range past %+v", lr)
	}
	for _, bad := range []string{"view=nope", "status=nope", "view=open&status=paid", "dir=up", "sort=name", "limit=x", "created_from=yesterday", "offset=-1"} {
		w := e.do(req{method: "GET", path: "/api/v1/payments?" + bad, h: read})
		wantError(t, w, http.StatusUnprocessableEntity, "validation_failed")
	}

	// Detail, resolve and reopen through an overpayment.
	e.Pay(e.Payment(small.ID), "6", 100)
	e.Head(120)
	op := e.session()
	w = e.do(req{method: "GET", path: "/api/v1/payments/" + small.ID + "/detail", h: read})
	wantStatus(t, w, http.StatusOK)
	d := decodeBody[paymentDetailDTO](t, w)
	if d.Payment.Status != core.StatusOverpaid || len(d.Transfers) != 1 || len(d.Timeline) < 2 || d.Timeline[0].Type != core.EventPaymentCreated || d.Deliveries == nil {
		t.Fatalf("detail %+v", d)
	}
	w = e.do(req{method: "POST", path: "/api/v1/payments/" + small.ID + "/resolve", body: map[string]any{"note": "refunded"}, h: op})
	wantStatus(t, w, http.StatusOK)
	if v := decodeBody[core.PaymentView](t, w); v.NeedsAttention || v.Resolution == nil || v.Resolution.Note != "refunded" {
		t.Fatalf("resolved %+v", v)
	}
	wantError(t, e.do(req{method: "POST", path: "/api/v1/payments/" + small.ID + "/resolve", h: op}), http.StatusConflict, "nothing_to_resolve")
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/payments/" + small.ID + "/reopen", h: op}), http.StatusOK)
	wantError(t, e.do(req{method: "POST", path: "/api/v1/payments/" + small.ID + "/reopen", h: op}), http.StatusConflict, "nothing_to_resolve")

	// Overview, node status, events.
	w = e.do(req{method: "GET", path: "/api/v1/overview?days=7", h: read})
	wantStatus(t, w, http.StatusOK)
	if ov := decodeBody[core.Overview](t, w); ov.Days != 7 || len(ov.Series) != 7 || ov.Current.Paid != 1 {
		t.Fatalf("overview %+v", ov)
	}
	wantError(t, e.do(req{method: "GET", path: "/api/v1/overview?days=999", h: read}), http.StatusUnprocessableEntity, "validation_failed")
	w = e.do(req{method: "GET", path: "/api/v1/node-status", h: read})
	wantStatus(t, w, http.StatusOK)
	if ns := decodeBody[nodeStatusDTO](t, w); ns.Head == nil || ns.Head.Number != 120 || ns.Endpoints == nil {
		t.Fatalf("node status %+v", ns)
	}
	w = e.do(req{method: "GET", path: "/api/v1/events?limit=2", h: read})
	wantStatus(t, w, http.StatusOK)
	ev := decodeBody[eventsBody](t, w)
	if len(ev.Data) != 2 || ev.Next != ev.Data[1].Seq {
		t.Fatalf("events %+v", ev)
	}
	w = e.do(req{method: "GET", path: fmt.Sprintf("/api/v1/events?after=%d&payment_id=%s", ev.Next, small.ID), h: read})
	for _, x := range decodeBody[eventsBody](t, w).Data {
		if x.Seq <= ev.Next || x.PaymentID == nil || *x.PaymentID != small.ID {
			t.Fatalf("cursor/filter broken: %+v", x)
		}
	}
	wantError(t, e.do(req{method: "GET", path: "/api/v1/events?after=-3", h: read}), http.StatusUnprocessableEntity, "validation_failed")
}

func TestLinks(t *testing.T) {
	e := newTenv(t)
	lk := bearer(e.key(core.ScopeLinksWrite))
	w := e.do(req{method: "POST", path: "/api/v1/payment-links", body: map[string]any{"slug": "tip-jar", "name": "Tip jar", "amount": "25.00", "max_uses": 10}, h: lk})
	wantStatus(t, w, http.StatusCreated)
	l := decodeBody[core.LinkView](t, w)
	if !l.Active || l.Amount == nil || *l.Amount != "25.00" || l.MaxUses == nil || *l.MaxUses != 10 || l.URL != "https://pay.example.com/l/tip-jar" {
		t.Fatalf("link %+v", l)
	}
	er := wantError(t, e.do(req{method: "POST", path: "/api/v1/payment-links", body: map[string]any{"slug": "tip-jar", "name": "Again"}, h: lk}), http.StatusUnprocessableEntity, "validation_failed")
	if er.Error.Fields[0].Field != "slug" || er.Error.Fields[0].Code != "taken" {
		t.Fatalf("fields %+v", er.Error.Fields)
	}
	// PUT requires active.
	wantError(t, e.do(req{method: "PUT", path: "/api/v1/payment-links/" + l.ID, body: map[string]any{"name": "X"}, h: lk}), http.StatusUnprocessableEntity, "validation_failed")
	w = e.do(req{method: "PUT", path: "/api/v1/payment-links/" + l.ID, body: map[string]any{"name": "Tips", "amount": nil, "active": true}, h: lk})
	wantStatus(t, w, http.StatusOK)
	if l2 := decodeBody[core.LinkView](t, w); l2.Name != "Tips" || l2.Amount != nil || l2.MaxUses != nil {
		t.Fatalf("updated %+v", l2)
	}
	w = e.do(req{method: "GET", path: "/api/v1/payment-links", h: lk})
	if lb := decodeBody[listBody[core.LinkView]](t, w); len(lb.Data) != 1 {
		t.Fatalf("list %+v", lb)
	}

	// Public side.
	w = e.do(req{method: "GET", path: "/api/v1/public/links/tip-jar"})
	wantStatus(t, w, http.StatusOK)
	if lc := decodeBody[core.LinkCheckoutView](t, w); lc.Status != core.LinkActive || lc.Merchant.Name == "" {
		t.Fatalf("link checkout %+v", lc)
	}
	idem := hdr{"Idempotency-Key": "visit-1"}
	w = e.do(req{method: "POST", path: "/api/v1/public/links/tip-jar/payments", body: map[string]any{"amount": "3.00"}, h: idem})
	wantStatus(t, w, http.StatusCreated)
	c1 := decodeBody[core.CheckoutView](t, w)
	// The same visit again: the same payment, and 200 rather than 201.
	w = e.do(req{method: "POST", path: "/api/v1/public/links/tip-jar/payments", body: map[string]any{"amount": "3.00"}, h: idem})
	wantStatus(t, w, http.StatusOK)
	if c2 := decodeBody[core.CheckoutView](t, w); c2.ID != c1.ID || c1.Amount != "3.00" {
		t.Fatalf("idempotent link payment: %s vs %s", c1.ID, c2.ID)
	}
	if strings.Contains(w.Body.String(), "metadata") || strings.Contains(w.Body.String(), "customer_email") {
		t.Fatalf("checkout leaks private fields: %s", w.Body.String())
	}
	wantError(t, e.do(req{method: "POST", path: "/api/v1/public/links/tip-jar/payments", body: map[string]any{}}), http.StatusUnprocessableEntity, "validation_failed")

	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/payment-links/" + l.ID + "/deactivate", h: lk}), http.StatusOK)
	wantError(t, e.do(req{method: "POST", path: "/api/v1/public/links/tip-jar/payments", body: map[string]any{"amount": "1"}}), http.StatusConflict, "link_disabled")
	w = e.do(req{method: "GET", path: "/api/v1/public/links/tip-jar"})
	if decodeBody[core.LinkCheckoutView](t, w).Status != core.LinkDisabled {
		t.Fatal("link still active")
	}

	// Unknown and malformed slugs are indistinguishable.
	a := wantError(t, e.do(req{method: "GET", path: "/api/v1/public/links/nope"}), http.StatusNotFound, "not_found")
	b := wantError(t, e.do(req{method: "GET", path: "/api/v1/public/links/NOT_A_SLUG"}), http.StatusNotFound, "not_found")
	if a.Error.Message != b.Error.Message || strings.Contains(a.Error.Message, "nope") {
		t.Fatalf("public 404s differ or echo input: %q / %q", a.Error.Message, b.Error.Message)
	}
	wantError(t, e.do(req{method: "POST", path: "/api/v1/public/links/nope/payments", body: map[string]any{"amount": "1"}}), http.StatusNotFound, "not_found")
}

func TestPublicCheckout(t *testing.T) {
	e := newTenv(t)
	p := e.CreatePayment("12.5")
	w := e.do(req{method: "GET", path: "/api/v1/public/payments/" + p.ID})
	wantStatus(t, w, http.StatusOK)
	c := decodeBody[core.CheckoutView](t, w)
	if c.ID != p.ID || c.Amount != "12.50" || !strings.HasPrefix(c.PaymentURI, "ethereum:") || c.Merchant.Name != "Northwind Digital" {
		t.Fatalf("checkout %+v", c)
	}
	// Unknown vs malformed vs well-formed-but-missing: same answer.
	a := e.do(req{method: "GET", path: "/api/v1/public/payments/pay_0000000000000000000000000z"})
	b := e.do(req{method: "GET", path: "/api/v1/public/payments/garbage"})
	ea, eb := wantError(t, a, http.StatusNotFound, "not_found"), wantError(t, b, http.StatusNotFound, "not_found")
	if ea.Error.Message != eb.Error.Message {
		t.Fatalf("distinguishable 404s: %q vs %q", ea.Error.Message, eb.Error.Message)
	}
	// Credentials are ignored on public routes (no 401 for a bad one).
	wantStatus(t, e.do(req{method: "GET", path: "/api/v1/public/payments/" + p.ID, h: bearer("hmn_bogus")}), http.StatusOK)
}

func TestWebhooks(t *testing.T) {
	e := newTenv(t)
	hk := bearer(e.key(core.ScopeWebhooksManage))
	w := e.do(req{method: "POST", path: "/api/v1/webhook-endpoints", body: map[string]any{"url": "https://hooks.example.com/a", "events": []string{"payment.paid", "payment.expired"}}, h: hk})
	wantStatus(t, w, http.StatusCreated)
	ep := decodeBody[endpointDTO](t, w)
	if ep.SecretMasked || !strings.HasPrefix(ep.Secret, "whsec_") || len(ep.Secret) < 20 || !ep.Enabled {
		t.Fatalf("created endpoint %+v", ep)
	}
	secret := ep.Secret
	op := e.session()

	for _, path := range []string{"/api/v1/webhook-endpoints", "/api/v1/webhook-endpoints/" + ep.ID} {
		// The operator sees a mask with the last four characters …
		w := e.do(req{method: "GET", path: path, h: op})
		wantStatus(t, w, http.StatusOK)
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("%s leaks the secret: %s", path, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"secret_masked":true`) || !strings.Contains(w.Body.String(), secret[len(secret)-4:]) {
			t.Fatalf("%s: masked secret missing: %s", path, w.Body.String())
		}
		// … an API key not even that.
		w = e.do(req{method: "GET", path: path, h: hk})
		wantStatus(t, w, http.StatusOK)
		if strings.Contains(w.Body.String(), secret[len(secret)-4:]) || !strings.Contains(w.Body.String(), `"secret":"whsec_…"`) {
			t.Fatalf("%s shows secret material to an API key: %s", path, w.Body.String())
		}
	}
	// Reveal and rotate are the operator's.
	wantError(t, e.do(req{method: "POST", path: "/api/v1/webhook-endpoints/" + ep.ID + "/reveal-secret", h: hk}), http.StatusForbidden, "forbidden")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/webhook-endpoints/" + ep.ID + "/rotate-secret", h: hk}), http.StatusForbidden, "forbidden")
	w = e.do(req{method: "POST", path: "/api/v1/webhook-endpoints/" + ep.ID + "/reveal-secret", h: op})
	wantStatus(t, w, http.StatusOK)
	if s := decodeBody[secretDTO](t, w); s.Secret != secret || s.PreviousSecret != nil {
		t.Fatalf("reveal %+v", s)
	}
	w = e.do(req{method: "POST", path: "/api/v1/webhook-endpoints/" + ep.ID + "/rotate-secret", h: op})
	wantStatus(t, w, http.StatusOK)
	rot := decodeBody[endpointDTO](t, w)
	if rot.Secret == secret || rot.SecretMasked || rot.PreviousSecretExpiresAt == nil {
		t.Fatalf("rotate %+v", rot)
	}
	w = e.do(req{method: "POST", path: "/api/v1/webhook-endpoints/" + ep.ID + "/reveal-secret", h: op})
	if s := decodeBody[secretDTO](t, w); s.Secret != rot.Secret || s.PreviousSecret == nil || *s.PreviousSecret != secret {
		t.Fatalf("reveal after rotate %+v", s)
	}
	w = e.do(req{method: "PUT", path: "/api/v1/webhook-endpoints/" + ep.ID, body: map[string]any{"url": "https://hooks.example.com/b", "events": []string{"payment.paid"}}, h: hk})
	wantError(t, w, http.StatusUnprocessableEntity, "validation_failed")
	w = e.do(req{method: "PUT", path: "/api/v1/webhook-endpoints/" + ep.ID, body: map[string]any{"url": "https://hooks.example.com/b", "events": []string{"payment.paid"}, "enabled": true}, h: hk})
	wantStatus(t, w, http.StatusOK)
	if strings.Contains(w.Body.String(), rot.Secret) {
		t.Fatal("update leaks secret")
	}
	er := wantError(t, e.do(req{method: "POST", path: "/api/v1/webhook-endpoints", body: map[string]any{"url": "ftp://x", "events": []string{"nope"}}, h: hk}), http.StatusUnprocessableEntity, "validation_failed")
	if len(er.Error.Fields) != 2 {
		t.Fatalf("fields %+v", er.Error.Fields)
	}

	w = e.do(req{method: "POST", path: "/api/v1/webhook-endpoints/" + ep.ID + "/test", h: hk})
	wantStatus(t, w, http.StatusAccepted)
	d := decodeBody[deliveryDTO](t, w)
	if d.EventType != core.EventWebhookTest || d.Status != core.DeliveryPending || d.MaxAttempts != 1 {
		t.Fatalf("test delivery %+v", d)
	}
	w = e.do(req{method: "GET", path: "/api/v1/webhook-deliveries?endpoint_id=" + ep.ID + "&status=pending,failed", h: hk})
	wantStatus(t, w, http.StatusOK)
	if lb := decodeBody[listBody[deliveryDTO]](t, w); len(lb.Data) != 1 || lb.Page == nil || lb.Page.Total != 1 || lb.Page.Limit != 50 {
		t.Fatalf("deliveries %+v", lb)
	}
	wantError(t, e.do(req{method: "GET", path: "/api/v1/webhook-deliveries?status=lost", h: hk}), http.StatusUnprocessableEntity, "validation_failed")
	wantStatus(t, e.do(req{method: "GET", path: "/api/v1/webhook-deliveries/" + d.ID, h: hk}), http.StatusOK)
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/webhook-deliveries/" + d.ID + "/resend", h: hk}), http.StatusAccepted)
	wantError(t, e.do(req{method: "GET", path: "/api/v1/webhook-deliveries/whd_missing", h: hk}), http.StatusNotFound, "webhook_delivery_not_found")

	// Disabled endpoint: test is a conflict.
	e.do(req{method: "PUT", path: "/api/v1/webhook-endpoints/" + ep.ID, body: map[string]any{"url": "https://hooks.example.com/b", "events": []string{"payment.paid"}, "enabled": false}, h: hk})
	wantError(t, e.do(req{method: "POST", path: "/api/v1/webhook-endpoints/" + ep.ID + "/test", h: hk}), http.StatusConflict, "endpoint_disabled")

	w = e.do(req{method: "DELETE", path: "/api/v1/webhook-endpoints/" + ep.ID, h: hk})
	wantStatus(t, w, http.StatusNoContent)
	wantError(t, e.do(req{method: "DELETE", path: "/api/v1/webhook-endpoints/" + ep.ID, h: hk}), http.StatusNotFound, "webhook_endpoint_not_found")

	for _, s := range []string{secret, rot.Secret} {
		if strings.Contains(e.logs.String(), s) {
			t.Fatal("webhook secret in logs")
		}
	}
}

func TestAPIKeysAndAccount(t *testing.T) {
	e := newTenv(t)
	op := e.session()
	w := e.do(req{method: "POST", path: "/api/v1/api-keys", body: map[string]any{"name": "Shop", "scopes": []string{"payments:read", "payments:write"}}, h: op})
	wantStatus(t, w, http.StatusCreated)
	k := decodeBody[apiKeyDTO](t, w)
	if !strings.HasPrefix(k.Secret, "hmn_live_") || k.Mode != "live" || len(k.Scopes) != 2 || !strings.HasPrefix(k.Secret, k.Prefix) {
		t.Fatalf("key %+v", k)
	}
	er := wantError(t, e.do(req{method: "POST", path: "/api/v1/api-keys", body: map[string]any{"name": "", "scopes": []string{"root"}}, h: op}), http.StatusUnprocessableEntity, "validation_failed")
	if len(er.Error.Fields) != 2 {
		t.Fatalf("fields %+v", er.Error.Fields)
	}

	w = e.do(req{method: "GET", path: "/api/v1/api-keys", h: op})
	wantStatus(t, w, http.StatusOK)
	if strings.Contains(w.Body.String(), k.Secret) || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "hash") {
		t.Fatalf("list leaks secret material: %s", w.Body.String())
	}

	// The key works, sees its own identity, and is limited by scope.
	w = e.do(req{method: "GET", path: "/api/v1/auth/me", h: bearer(k.Secret)})
	wantStatus(t, w, http.StatusOK)
	if me := decodeBody[meResponse](t, w); me.Kind != core.ActorAPIKey || me.APIKey == nil || me.APIKey.ID != k.ID {
		t.Fatalf("me %+v", me)
	}
	er = wantError(t, e.do(req{method: "GET", path: "/api/v1/payment-links", h: bearer(k.Secret)}), http.StatusForbidden, "missing_scope")
	if !strings.Contains(er.Error.Message, "links:write") {
		t.Fatalf("message %q", er.Error.Message)
	}
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/api-keys/" + k.ID + "/revoke", h: op}), http.StatusOK)
	wantError(t, e.do(req{method: "GET", path: "/api/v1/payments", h: bearer(k.Secret)}), http.StatusUnauthorized, "credentials_revoked")
	wantError(t, e.do(req{method: "GET", path: "/api/v1/payments", h: bearer("hmn_live_nope")}), http.StatusUnauthorized, "invalid_credentials")
	wantError(t, e.do(req{method: "GET", path: "/api/v1/payments", h: hdr{"Authorization": "Basic Zm9vOmJhcg=="}}), http.StatusUnauthorized, "invalid_credentials")

	// Operator and me.
	w = e.do(req{method: "GET", path: "/api/v1/auth/me", h: op})
	if me := decodeBody[meResponse](t, w); me.Kind != core.ActorOperator || me.Operator.Email != e.Email {
		t.Fatalf("me %+v", me)
	}
	w = e.do(req{method: "GET", path: "/api/v1/operator", h: op})
	if o := decodeBody[operatorDTO](t, w); o.Email != e.Email || o.CreatedAt == nil || strings.Contains(w.Body.String(), "fake$") {
		t.Fatalf("operator %s", w.Body.String())
	}

	// Settings.
	w = e.do(req{method: "GET", path: "/api/v1/settings", h: op})
	wantStatus(t, w, http.StatusOK)
	st := decodeBody[settingsDTO](t, w)
	if st.Asset.Decimals != 18 || st.Network.ChainID != 56 || st.Payments.TTLSeconds != 1800 || st.Wallet.XPub == "" {
		t.Fatalf("settings %+v", st)
	}
	w = e.do(req{method: "PUT", path: "/api/v1/settings/merchant", body: map[string]any{"merchant_name": "Acme", "support_email": "help@acme.example", "public_url": "https://acme.example"}, h: op})
	wantStatus(t, w, http.StatusOK)
	if s := decodeBody[settingsDTO](t, w); s.Merchant.Name != "Acme" || s.PublicURL != "https://acme.example" {
		t.Fatalf("merchant %+v", s)
	}
	er = wantError(t, e.do(req{method: "PUT", path: "/api/v1/settings/payments", body: map[string]any{"ttl_seconds": 1, "required_confirmations": 0, "underpayment_tolerance": "x"}, h: op}), http.StatusUnprocessableEntity, "validation_failed")
	names := map[string]bool{}
	for _, f := range er.Error.Fields {
		names[f.Field] = true
	}
	if !names["ttl_seconds"] || !names["required_confirmations"] || !names["underpayment_tolerance"] {
		t.Fatalf("fields %+v", er.Error.Fields)
	}
	w = e.do(req{method: "PUT", path: "/api/v1/settings/payments", body: map[string]any{"ttl_seconds": 600, "required_confirmations": 3, "underpayment_tolerance": "0.5"}, h: op})
	if s := decodeBody[settingsDTO](t, w); s.Payments.TTLSeconds != 600 || s.Payments.RequiredConfirmations != 3 {
		t.Fatalf("payments %+v", s)
	}
	// Network switch refused while payments are open.
	e.CreatePayment("1")
	wantError(t, e.do(req{method: "PUT", path: "/api/v1/settings/network", body: map[string]any{"chain_id": 97, "rpc_urls": []string{"https://rpc.example.org"}}, h: op}), http.StatusConflict, "open_payments")
	w = e.do(req{method: "PUT", path: "/api/v1/settings/network", body: map[string]any{"rpc_urls": []string{"https://a.example.org", "wss://b.example.org"}}, h: op})
	if s := decodeBody[settingsDTO](t, w); len(s.Network.RPCURLs) != 2 {
		t.Fatalf("network %+v", s)
	}
	idx := uint32(42)
	w = e.do(req{method: "PUT", path: "/api/v1/settings/wallet", body: map[string]any{"xpub": xpub, "next_address_index": idx}, h: op})
	if s := decodeBody[settingsDTO](t, w); s.Wallet.NextAddressIndex != 42 {
		t.Fatalf("wallet %+v", s)
	}

	// Password change keeps this session and ends the others.
	w = e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"email": e.Email, "password": e.Password, "issue_token": true}})
	wantStatus(t, w, http.StatusOK)
	other := decodeBody[loginResponse](t, w).Token
	er = wantError(t, e.do(req{method: "POST", path: "/api/v1/operator/password", body: map[string]any{"current_password": "wrong", "new_password": "another long password"}, h: op}), http.StatusUnprocessableEntity, "validation_failed")
	if er.Error.Fields[0].Field != "current_password" {
		t.Fatalf("fields %+v", er.Error.Fields)
	}
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/operator/password", body: map[string]any{"current_password": e.Password, "new_password": "another long password"}, h: op}), http.StatusNoContent)
	wantStatus(t, e.do(req{method: "GET", path: "/api/v1/settings", h: op}), http.StatusOK)
	wantError(t, e.do(req{method: "GET", path: "/api/v1/settings", h: bearer(other)}), http.StatusUnauthorized, "invalid_credentials")
	wantError(t, e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"email": e.Email, "password": e.Password}}), http.StatusUnauthorized, "invalid_credentials")
}

// GET /api/v1/instance tells an authenticated caller which build runs, and
// an anonymous one nothing.
func TestInstanceVersion(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) { o.Version, o.Commit = "1.2.3", "abc1234" }))
	wantError(t, e.do(req{method: "GET", path: "/api/v1/instance"}), http.StatusUnauthorized, "unauthorized")
	w := e.do(req{method: "GET", path: "/api/v1/instance", h: e.session()})
	wantStatus(t, w, http.StatusOK)
	got := decodeBody[map[string]string](t, w)
	if len(got) != 2 || got["version"] != "1.2.3" || got["commit"] != "abc1234" {
		t.Fatalf("instance = %v", got)
	}
	// The public endpoints never mention the version.
	for _, path := range []string{"/healthz", "/readyz", "/api/v1/setup"} {
		if w := e.do(req{method: "GET", path: path}); strings.Contains(w.Body.String(), "1.2.3") {
			t.Errorf("%s leaks the version: %s", path, w.Body.String())
		}
	}
	// A build that was not stamped still answers.
	e2 := newTenv(t)
	got = decodeBody[map[string]string](t, e2.do(req{method: "GET", path: "/api/v1/instance", h: e2.session()}))
	if got["version"] != "dev" || got["commit"] != "unknown" {
		t.Fatalf("unstamped instance = %v", got)
	}
}
