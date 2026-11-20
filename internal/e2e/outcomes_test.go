package e2e

import (
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// Scenario 4: every way a payment can end, driven through the API and the
// chain alone.
func TestPaymentOutcomes(t *testing.T) {
	t.Parallel()
	in := newInstance(t)
	key := in.setup()
	in.endpoint(key)

	t.Run("underpaid then topped up", func(t *testing.T) {
		p := in.createPayment(key, "30.00")
		in.pay(p.Address, 10)
		in.mine(1)
		got := in.waitStatus(key, p.ID, core.StatusDetected)
		if got.AmountReceived != "10.00" || got.AmountDue != "20.00" {
			t.Fatalf("partial = %+v", got)
		}
		in.pay(p.Address, 20)
		in.mine(1)
		in.waitStatus(key, p.ID, core.StatusConfirming)
		in.mine(settleDepth + confirmations)
		got = in.waitStatus(key, p.ID, core.StatusPaid)
		if got.AmountConfirmed != "30.00" || got.NeedsAttention {
			t.Fatalf("topped up = %+v", got)
		}
		if tl := in.timeline(p.ID); tl != "created detected confirming paid" {
			t.Fatalf("timeline = %s", tl)
		}
	})

	t.Run("overpaid, needs attention, resolved", func(t *testing.T) {
		p := in.createPayment(key, "10.00")
		in.pay(p.Address, 15)
		in.mine(1)
		in.mine(settleDepth + confirmations)
		got := in.waitStatus(key, p.ID, core.StatusOverpaid)
		if !got.NeedsAttention || got.Attention != core.AttentionOverpaid || got.AmountConfirmed != "15.00" {
			t.Fatalf("overpaid = %+v", got)
		}
		counts := decode[struct {
			Counts struct {
				Attention int `json:"attention"`
			} `json:"counts"`
		}](t, in.must(200, "GET", "/api/v1/payments?view=attention&counts=true", nil, bearer(key)))
		if counts.Counts.Attention < 1 {
			t.Fatalf("attention tab count = %d", counts.Counts.Attention)
		}
		res := decode[core.PaymentView](t, in.must(200, "POST", "/api/v1/payments/"+p.ID+"/resolve", map[string]any{"note": "refunded 5 USDT"}))
		if res.NeedsAttention || res.Resolution == nil || res.Resolution.Note != "refunded 5 USDT" || res.Status != core.StatusOverpaid {
			t.Fatalf("resolved = %+v", res)
		}
		if tl := in.timeline(p.ID); tl != "created confirming overpaid resolved" {
			t.Fatalf("timeline = %s", tl)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		p := in.createPayment(key, "12.00")
		c := decode[core.PaymentView](t, in.must(200, "POST", "/api/v1/payments/"+p.ID+"/cancel", nil, bearer(key)))
		if c.Status != core.StatusExpired || c.CancelledAt == nil {
			t.Fatalf("cancelled = %+v", c)
		}
		// Once funds are seen, cancelling is refused.
		q := in.createPayment(key, "12.00")
		in.pay(q.Address, 5)
		in.mine(1)
		in.waitStatus(key, q.ID, core.StatusDetected)
		r := in.must(409, "POST", "/api/v1/payments/"+q.ID+"/cancel", nil, bearer(key))
		if r.errCode() != "payment_has_funds" {
			t.Fatalf("cancel with funds = %s", r)
		}
		if got := in.payment(key, q.ID); got.Status != core.StatusDetected {
			t.Fatalf("after refused cancel = %+v", got)
		}
	})

	t.Run("underpayment tolerance is capped at 5% of the amount", func(t *testing.T) {
		// The operator allows up to 1.00 short — but on a 10.00 invoice
		// at most 5% (0.50) can be waived.
		in.must(200, "PUT", "/api/v1/settings/payments", map[string]any{"ttl_seconds": 3600, "required_confirmations": confirmations, "underpayment_tolerance": "1.00"})
		defer in.must(200, "PUT", "/api/v1/settings/payments", map[string]any{"ttl_seconds": 3600, "required_confirmations": confirmations, "underpayment_tolerance": "0"})
		short := in.createPayment(key, "10.00")
		within := in.createPayment(key, "10.00")
		in.payUnits(short.Address, units("9.40"))
		in.payUnits(within.Address, units("9.60"))
		in.mine(1)
		in.mine(settleDepth + confirmations)
		in.waitStatus(key, within.ID, core.StatusPaid)
		in.synced()
		if got := in.payment(key, short.ID); got.Status != core.StatusDetected || got.AmountDue != "0.60" {
			t.Fatalf("9.40 of 10.00 with a 1.00 tolerance = %+v; the cap (0.50) must keep it open", got)
		}
	})

	// The rest moves chain time past deadlines, so it runs last.
	t.Run("expires, then late funds", func(t *testing.T) {
		p := in.createPayment(key, "8.00", map[string]any{"ttl_seconds": 60})
		// Just before TTL + grace nothing happens.
		in.clock.Advance(p.ExpiresAt.Add(core.DefaultExpiryGrace - 5*time.Second).Sub(in.clock.Now()))
		in.mine(settleDepth + 2)
		in.synced()
		if got := in.payment(key, p.ID); got.Status != core.StatusAwaiting {
			t.Fatalf("before the deadline = %+v", got)
		}
		in.clock.Advance(10 * time.Second)
		in.mine(settleDepth + 1)
		in.waitStatus(key, p.ID, core.StatusExpired)
		if tl := in.timeline(p.ID); tl != "created expired" {
			t.Fatalf("timeline = %s", tl)
		}

		// The payer pays anyway: never lost, never silently changes the
		// outcome — late funds, needing attention.
		in.pay(p.Address, 8)
		in.mine(1)
		in.mine(settleDepth + confirmations)
		in.eventually("late funds to be recorded", func() bool {
			got := in.payment(key, p.ID)
			return got.Attention == core.AttentionLateFunds
		})
		got := in.payment(key, p.ID)
		if got.Status != core.StatusExpired || !got.NeedsAttention || got.AmountConfirmed != "8.00" {
			t.Fatalf("late funds = %+v", got)
		}
		if tl := in.timeline(p.ID); !strings.HasSuffix(tl, "late_funds") {
			t.Fatalf("timeline = %s", tl)
		}
		in.eventually("the late_funds webhook", func() bool {
			return slices.Contains(uniqueTypes(in.recv.forPayment(p.ID)), string(core.EventPaymentLateFunds))
		})
	})
}

// units converts a decimal USDT amount ("9.40") to base units (18 decimals).
func units(s string) *big.Int {
	a, err := core.ParseAmount(s, 18)
	if err != nil {
		panic(err)
	}
	return a.Big()
}
