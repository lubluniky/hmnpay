package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/lubluniky/hmnpay/core"
)

// The public views carry a version, so that a checkout page can tell which
// of two answers about a payment is the newer one.

// versionOf reads the "version" of a public view.
func versionOf(t *testing.T, raw []byte) int64 {
	t.Helper()
	var v struct {
		Version *int64 `json:"version"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("bad view %q: %v", raw, err)
	}
	if v.Version == nil {
		t.Fatalf("the public view has no version: %s", raw)
	}
	return *v.Version
}

// A checkout page learns a payment's state from two places, the plain view
// and the stream, and their answers can arrive out of order. Both carry
// the same monotonic "version" — the number of events the payment has had,
// which is also the stream's event id — so a page can tell which is newer
// and never show a paid payment as unpaid again.
func TestPublicViewCarriesTheVersionTheStreamCounts(t *testing.T) {
	e := newTenv(t, withOpts(fastStreams))
	srv := startServer(t, e.h)
	p := e.CreatePayment("10")
	path := "/api/v1/public/payments/" + p.ID
	plain := func() (int64, core.CheckoutView) {
		t.Helper()
		w := e.do(req{method: "GET", path: path})
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
		}
		var v core.CheckoutView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return versionOf(t, w.Body.Bytes()), v
	}
	events := func() int64 {
		t.Helper()
		evs, err := e.Service.Events(systemContext(t.Context()), core.EventFilter{PaymentID: p.ID, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		return int64(len(evs))
	}

	c := openStream(t, srv.URL+path+"/stream", nil)
	first, _ := c.next(nil)
	v0, view := plain()
	if v0 < 1 || v0 != events() || view.Status != core.StatusAwaiting {
		t.Fatalf("new payment: version %d, events %d, status %s", v0, events(), view.Status)
	}
	// The stream's id and the version inside its payload are the same
	// number as the plain view's.
	if first.id != strconv.FormatInt(v0, 10) || versionOf(t, []byte(first.data)) != v0 {
		t.Fatalf("stream id %q, payload version %d, plain version %d", first.id, versionOf(t, []byte(first.data)), v0)
	}

	// Every change of the payment moves the version forward, in both.
	e.Pay(e.Payment(p.ID), "4", 100)
	det, _ := c.next(func(v core.CheckoutView) bool { return v.Status == core.StatusDetected })
	v1, view := plain()
	if v1 <= v0 || v1 != events() || view.Status != core.StatusDetected || versionOf(t, []byte(det.data)) != v1 || det.id != strconv.FormatInt(v1, 10) {
		t.Fatalf("detected: version %d (was %d), events %d, stream id %q payload %d", v1, v0, events(), det.id, versionOf(t, []byte(det.data)))
	}
	e.Pay(e.Payment(p.ID), "6", 100)
	e.Head(100)
	conf, _ := c.next(func(v core.CheckoutView) bool { return v.Status == core.StatusConfirming })
	v2, _ := plain()
	if v2 <= v1 || versionOf(t, []byte(conf.data)) != v2 {
		t.Fatalf("confirming: version %d (was %d), stream payload %d", v2, v1, versionOf(t, []byte(conf.data)))
	}
	// Confirmations tick without an event: the version stays, the counter
	// only grows.
	e.Head(103)
	more, tick := c.next(func(v core.CheckoutView) bool { return v.Confirmations == 4 })
	v3, view := plain()
	if v3 != v2 || versionOf(t, []byte(more.data)) != v2 || view.Confirmations != tick.Confirmations {
		t.Fatalf("a confirmation tick moved the version: %d -> %d (stream %d)", v2, v3, versionOf(t, []byte(more.data)))
	}
	e.Head(120)
	paid, _ := c.next(func(v core.CheckoutView) bool { return v.Status == core.StatusPaid })
	v4, view := plain()
	if v4 <= v3 || v4 != events() || view.Status != core.StatusPaid || versionOf(t, []byte(paid.data)) != v4 {
		t.Fatalf("paid: version %d (was %d), events %d, stream payload %d", v4, v3, events(), versionOf(t, []byte(paid.data)))
	}
}

// A payment made through a payment link answers with the same view, so it
// carries a version too.
func TestLinkPaymentViewHasAVersion(t *testing.T) {
	e := newTenv(t)
	w := e.do(req{method: "POST", path: "/api/v1/payment-links", body: map[string]any{"name": "Pro", "slug": "pro", "amount": "5"}, h: e.session()})
	if w.Code != http.StatusCreated {
		t.Fatalf("create link: %d %s", w.Code, w.Body)
	}
	w = e.do(req{method: "POST", path: "/api/v1/public/links/pro/payments", body: map[string]any{}, h: hdr{"Idempotency-Key": "version-1"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("link payment: %d %s", w.Code, w.Body)
	}
	created := versionOf(t, w.Body.Bytes())
	var v core.CheckoutView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	plain := e.do(req{method: "GET", path: "/api/v1/public/payments/" + v.ID})
	if created < 1 || created != versionOf(t, plain.Body.Bytes()) {
		t.Fatalf("link payment version %d, plain view version %d", created, versionOf(t, plain.Body.Bytes()))
	}
}
