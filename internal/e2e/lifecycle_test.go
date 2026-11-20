package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/core"
)

// Scenarios 1–3: a fresh instance is set up through the wizard's API, a
// merchant integration creates a payment idempotently, the payer pays, and
// checkout stream, webhooks and dashboard all tell the same story.
func TestFirstRunToPaid(t *testing.T) {
	t.Parallel()
	in := newInstance(t)

	// 1. Fresh instance: ready, setup required.
	ready := decode[struct {
		Status         string `json:"status"`
		SetupCompleted bool   `json:"setup_completed"`
	}](t, in.must(200, "GET", "/readyz", nil))
	if ready.Status != "ready" || ready.SetupCompleted {
		t.Fatalf("readyz on a fresh instance = %+v", ready)
	}
	type setupStatus struct {
		Completed     bool `json:"completed"`
		TokenRequired bool `json:"setup_token_required"`
	}
	setup := decode[setupStatus](t, in.must(200, "GET", "/api/v1/setup", nil))
	if setup.Completed || !setup.TokenRequired {
		t.Fatalf("fresh instance setup status = %+v", setup)
	}
	token := in.eng.SetupToken()

	// An unclaimed instance cannot be claimed — or even used as a
	// probe — without the setup token printed at start.
	probeBody := func(extra map[string]any) map[string]any {
		b := map[string]any{"url": in.urls[0], "chain_id": core.ChainBSCMainnet}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	previewBody := map[string]any{"xpub": hardhatXpub, "count": 3}
	for _, c := range []struct {
		path string
		body map[string]any
	}{{"/api/v1/setup/probe-rpc", probeBody(nil)}, {"/api/v1/setup/preview-addresses", previewBody}, {"/api/v1/setup/complete", in.setupBody("")}} {
		if r := in.must(401, "POST", c.path, c.body); r.errCode() != "setup_token_required" {
			t.Fatalf("%s without token = %s", c.path, r)
		}
		if r := in.must(403, "POST", c.path, c.body, header("X-Hmnpay-Setup-Token", "not-the-token-not-the-token")); r.errCode() != "setup_token_invalid" {
			t.Fatalf("%s with a wrong token = %s", c.path, r)
		}
	}
	// Never in the query string.
	if r := in.call("POST", "/api/v1/setup/probe-rpc?setup_token="+token, probeBody(nil)); r.status != 401 {
		t.Fatalf("token in the query string accepted: %s", r)
	}
	// DNS rebinding: a foreign Host name is refused before any route runs.
	if r := in.call("GET", "/api/v1/setup", nil, func(r *http.Request) { r.Host = "rebind.attacker.example" }); r.status != http.StatusMisdirectedRequest || r.errCode() != "host_not_allowed" {
		t.Fatalf("foreign Host = %s", r)
	}
	// Payments are refused until setup.
	if r := in.call("POST", "/api/v1/payments", map[string]any{"amount": "1"}); r.status != 401 {
		t.Fatalf("payment before setup = %s", r)
	}

	// The wizard probes each RPC endpoint (anonymously: no operator yet).
	for _, u := range in.urls {
		probe := decode[struct {
			Outcome core.ProbeOutcome `json:"outcome"`
			Facts   struct {
				ChainID     uint64 `json:"chain_id"`
				TokenSymbol string `json:"token_symbol"`
			} `json:"facts"`
		}](t, in.must(200, "POST", "/api/v1/setup/probe-rpc", map[string]any{"url": u, "chain_id": core.ChainBSCMainnet}, header("X-Hmnpay-Setup-Token", token)))
		if probe.Outcome != core.ProbeOK || probe.Facts.ChainID != core.ChainBSCMainnet || probe.Facts.TokenSymbol != "USDT" {
			t.Fatalf("probe %s = %+v", u, probe)
		}
	}
	// …and previews the deposit addresses of the xpub.
	preview := decode[list[struct {
		Index   uint32 `json:"index"`
		Address string `json:"address"`
	}]](t, in.must(200, "POST", "/api/v1/setup/preview-addresses", map[string]any{"xpub": hardhatXpub, "count": 3, "setup_token": token}))
	if len(preview.Data) != 3 || !strings.EqualFold(preview.Data[0].Address, hardhatAddr0) || !strings.EqualFold(preview.Data[1].Address, hardhatAddr1) {
		t.Fatalf("preview = %+v", preview.Data)
	}

	in.completeSetup()
	// A second setup — even with the token — is refused, and the token
	// is no longer asked for.
	if r := in.call("POST", "/api/v1/setup/complete", in.setupBody(token), header("Cookie", "")); r.status < 400 || r.status == 422 {
		t.Fatalf("second setup = %s; want it refused", r)
	}
	if st := decode[setupStatus](t, in.must(200, "GET", "/api/v1/setup", nil)); !st.Completed || st.TokenRequired {
		t.Fatalf("setup status after setup = %+v", st)
	}
	me := decode[struct {
		Kind core.ActorKind `json:"kind"`
	}](t, in.must(200, "GET", "/api/v1/auth/me", nil))
	if me.Kind != core.ActorOperator {
		t.Fatalf("session after setup = %+v", me)
	}
	in.must(200, "PUT", "/api/v1/settings/payments", map[string]any{"ttl_seconds": 3600, "required_confirmations": confirmations, "underpayment_tolerance": "0"})
	in.synced()
	in.eventually("node status to report both endpoints healthy", func() bool {
		ns := decode[nodeStatus](t, in.must(200, "GET", "/api/v1/node-status", nil))
		return ns.Healthy && len(ns.Endpoints) == 2 && ns.Endpoints[0].Healthy && ns.Endpoints[1].Healthy
	})

	// 2. A merchant integration with an API key.
	key := in.createAPIKey("shop backend", core.Scopes...)
	_, secret := in.endpoint(key)
	order := map[string]any{"amount": "25.00", "order_id": "ORD-1001", "description": "Blue mug"}
	first := in.must(201, "POST", "/api/v1/payments", order, bearer(key), header("Idempotency-Key", "ord-1001"))
	p := decode[core.PaymentView](t, first)
	if p.Status != core.StatusAwaiting || p.Amount != "25.00" || !strings.EqualFold(p.Address, hardhatAddr0) {
		t.Fatalf("created = %+v", p)
	}
	replay := decode[core.PaymentView](t, in.must(200, "POST", "/api/v1/payments", order, bearer(key), header("Idempotency-Key", "ord-1001")))
	if replay.ID != p.ID || replay.Address != p.Address {
		t.Fatalf("replay returned another payment: %s vs %s", replay.ID, p.ID)
	}
	mismatch := in.must(409, "POST", "/api/v1/payments", map[string]any{"amount": "26.00", "order_id": "ORD-1001"}, bearer(key), header("Idempotency-Key", "ord-1001"))
	if mismatch.errCode() != "idempotency_key_reused" {
		t.Fatalf("mismatch = %s", mismatch)
	}

	// 3. The payer opens the checkout and pays.
	stream := in.openStream(p.ID)
	if v := stream.next(t); v.Status != core.StatusAwaiting || v.AmountDue != "25.00" || v.Address != p.Address || v.Merchant.Name != "Northwind" {
		t.Fatalf("first checkout view = %+v", v)
	}
	// SSE ids count this payment's events only (1: created), not the
	// instance's log position, which would disclose activity of others.
	if stream.lastID != "1" {
		t.Fatalf("first SSE id = %q; want 1", stream.lastID)
	}
	tx := in.pay(p.Address, 25)
	in.mine(1)
	confirming := stream.until(t, core.StatusConfirming)
	in.mine(settleDepth + confirmations)
	seen := append(confirming, stream.until(t, core.StatusPaid)...)
	var counts []int
	for _, v := range seen {
		if v.Status == core.StatusConfirming {
			counts = append(counts, v.Confirmations)
		}
	}
	if len(counts) == 0 || !slices.IsSorted(counts) || counts[len(counts)-1] >= confirmations {
		t.Fatalf("confirmations while confirming = %v (want counting up below %d)", counts, confirmations)
	}
	if stream.lastID != "3" {
		t.Fatalf("SSE id at paid = %q; want 3 (created, confirming, paid)", stream.lastID)
	}
	last := seen[len(seen)-1]
	if last.Status != core.StatusPaid || last.TxHash == nil || *last.TxHash != tx.String() || last.PaidAt == nil || last.Confirmations != confirmations {
		t.Fatalf("paid checkout view = %+v", last)
	}

	// Webhooks: every transition, in order, signed.
	in.eventually("the payment.paid webhook", func() bool {
		return slices.Contains(uniqueTypes(in.recv.forPayment(p.ID)), string(core.EventPaymentPaid))
	})
	ds := in.recv.forPayment(p.ID)
	if got := strings.Join(uniqueTypes(ds), " "); got != "payment.created payment.confirming payment.paid" {
		t.Fatalf("webhooks = %s", got)
	}
	verifySignatures(t, in.clock, secret, ds)
	var paid core.PaymentView
	for _, d := range ds {
		if d.Envelope.Type == core.EventPaymentPaid {
			paid = decode[core.PaymentView](t, response{body: d.Envelope.Data})
		}
	}
	if paid.ID != p.ID || paid.Status != core.StatusPaid || paid.AmountConfirmed != "25.00" || paid.OrderID != "ORD-1001" {
		t.Fatalf("payment.paid payload = %+v", paid)
	}

	// The dashboard's payment page: transfer, timeline, deliveries.
	in.eventually("every delivery to be recorded as succeeded", func() bool {
		d := in.detail(p.ID)
		if len(d.Deliveries) != 3 {
			return false
		}
		for _, dl := range d.Deliveries {
			if dl.Status != core.DeliverySucceeded {
				return false
			}
		}
		return true
	})
	d := in.detail(p.ID)
	if len(d.Transfers) != 1 || d.Transfers[0].TxHash != tx.String() || d.Transfers[0].Amount != "25.00" || d.Transfers[0].Status != core.TransferConfirmed {
		t.Fatalf("transfers = %+v", d.Transfers)
	}
	if got := in.timeline(p.ID); got != "created confirming paid" {
		t.Fatalf("timeline = %s", got)
	}
	if d.Payment.Status != core.StatusPaid || d.Payment.NeedsAttention {
		t.Fatalf("detail payment = %+v", d.Payment)
	}
	// The public checkout without the stream agrees.
	co := decode[core.CheckoutView](t, in.must(200, "GET", "/api/v1/public/payments/"+p.ID, nil))
	if co.Status != core.StatusPaid {
		t.Fatalf("public checkout = %+v", co)
	}
}
