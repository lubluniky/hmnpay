package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/core"
)

// A payment link where the customer chooses the amount
// has a minimum — its own (min_amount) or one whole unit — which both link
// views state and the public checkout enforces with a field error the
// checkout page can show.
func TestLinkMinimumAmountAPI(t *testing.T) {
	e := newTenv(t)
	lk := bearer(e.key(core.ScopeLinksWrite))

	// No min_amount: the default, shown as "1.00".
	w := e.do(req{method: "POST", path: "/api/v1/payment-links", body: map[string]any{"slug": "tips", "name": "Tips", "max_uses": 3}, h: lk})
	wantStatus(t, w, http.StatusCreated)
	tips := decodeBody[core.LinkView](t, w)
	if tips.Amount != nil || tips.MinAmount == nil || *tips.MinAmount != "1.00" {
		t.Fatalf("link without a price: amount %v, min_amount %v", tips.Amount, tips.MinAmount)
	}
	if !strings.Contains(w.Body.String(), `"min_amount":"1.00"`) {
		t.Fatalf("min_amount is not in the response: %s", w.Body.String())
	}
	w = e.do(req{method: "GET", path: "/api/v1/public/links/tips"})
	wantStatus(t, w, http.StatusOK)
	if lc := decodeBody[core.LinkCheckoutView](t, w); lc.MinAmount == nil || *lc.MinAmount != "1.00" || lc.Amount != nil {
		t.Fatalf("public link view: %+v", lc)
	}
	for _, amount := range []string{"0.000000000000000001", "0.99"} {
		er := wantError(t, e.do(req{method: "POST", path: "/api/v1/public/links/tips/payments", body: map[string]any{"amount": amount}}),
			http.StatusUnprocessableEntity, "validation_failed")
		if len(er.Error.Fields) != 1 || er.Error.Fields[0].Field != "amount" || er.Error.Fields[0].Code != "too_small" ||
			er.Error.Fields[0].Message != "the minimum amount is 1.00 USDT" {
			t.Fatalf("amount %s: fields %+v", amount, er.Error.Fields)
		}
	}
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/public/links/tips/payments", body: map[string]any{"amount": "1"}}), http.StatusCreated)

	// A minimum of the link's own, set, shown, enforced and changed.
	w = e.do(req{method: "POST", path: "/api/v1/payment-links", body: map[string]any{"slug": "donate", "name": "Donate", "min_amount": "5"}, h: lk})
	wantStatus(t, w, http.StatusCreated)
	donate := decodeBody[core.LinkView](t, w)
	if donate.MinAmount == nil || *donate.MinAmount != "5.00" {
		t.Fatalf("min_amount %v", donate.MinAmount)
	}
	er := wantError(t, e.do(req{method: "POST", path: "/api/v1/public/links/donate/payments", body: map[string]any{"amount": "4.99"}}),
		http.StatusUnprocessableEntity, "validation_failed")
	if er.Error.Fields[0].Field != "amount" || er.Error.Fields[0].Message != "the minimum amount is 5.00 USDT" {
		t.Fatalf("fields %+v", er.Error.Fields)
	}
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/public/links/donate/payments", body: map[string]any{"amount": "5.00"}}), http.StatusCreated)
	w = e.do(req{method: "PUT", path: "/api/v1/payment-links/" + donate.ID, body: map[string]any{"name": "Donate", "min_amount": nil, "active": true}, h: lk})
	wantStatus(t, w, http.StatusOK)
	if l := decodeBody[core.LinkView](t, w); l.MinAmount == nil || *l.MinAmount != "1.00" {
		t.Fatalf("min_amount after removing the link's own: %v", l.MinAmount)
	}

	// What is refused when a link is made.
	for name, body := range map[string]map[string]any{
		"below one unit":        {"name": "X", "min_amount": "0.5"},
		"zero":                  {"name": "X", "min_amount": "0"},
		"not a number":          {"name": "X", "min_amount": "lots"},
		"with a fixed price":    {"name": "X", "amount": "25", "min_amount": "5"},
		"above the maximum":     {"name": "X", "min_amount": "1000000000000"},
		"more decimals than 18": {"name": "X", "min_amount": "1.0000000000000000001"},
	} {
		er := wantError(t, e.do(req{method: "POST", path: "/api/v1/payment-links", body: body, h: lk}), http.StatusUnprocessableEntity, "validation_failed")
		if len(er.Error.Fields) == 0 || er.Error.Fields[0].Field != "min_amount" {
			t.Fatalf("%s: fields %+v", name, er.Error.Fields)
		}
	}

	// A fixed price has no minimum: null in both views, any price accepted.
	w = e.do(req{method: "POST", path: "/api/v1/payment-links", body: map[string]any{"slug": "sticker", "name": "Sticker", "amount": "0.50"}, h: lk})
	wantStatus(t, w, http.StatusCreated)
	if l := decodeBody[core.LinkView](t, w); l.MinAmount != nil || !strings.Contains(w.Body.String(), `"min_amount":null`) {
		t.Fatalf("fixed-price link: %s", w.Body.String())
	}
	w = e.do(req{method: "GET", path: "/api/v1/public/links/sticker"})
	if lc := decodeBody[core.LinkCheckoutView](t, w); lc.MinAmount != nil || lc.Amount == nil || *lc.Amount != "0.50" {
		t.Fatalf("public view of a fixed-price link: %+v", lc)
	}
	wantStatus(t, e.do(req{method: "POST", path: "/api/v1/public/links/sticker/payments", body: map[string]any{}}), http.StatusCreated)
}
