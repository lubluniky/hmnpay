package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/core"
)

// Scenario 5: the paying transaction is reorganised out and mined again in
// another block. The payment ends paid exactly once, the amount is counted
// once, and the dashboard keeps the orphaned record.
func TestReorgReincluded(t *testing.T) {
	t.Parallel()
	in := newInstance(t)
	key := in.setup()
	_, secret := in.endpoint(key)
	p := in.createPayment(key, "25.00")

	tx := in.pay(p.Address, 25)
	first := in.mine(1)
	in.waitStatus(key, p.ID, core.StatusConfirming)

	// The block loses to a competing fork; for a while the transaction is
	// in no block at all.
	dropped := in.chain.Reorg(1)
	in.mine(2)
	in.waitStatus(key, p.ID, core.StatusAwaiting)

	// It is mined again, in a different block.
	in.chain.Requeue(dropped...)
	second := in.mine(1)
	if second.Hash == first.Hash {
		t.Fatal("test setup: the transaction must land in another block")
	}
	in.waitStatus(key, p.ID, core.StatusConfirming)
	in.mine(settleDepth + confirmations)
	got := in.waitStatus(key, p.ID, core.StatusPaid)
	if got.AmountReceived != "25.00" || got.AmountConfirmed != "25.00" || got.TxHash == nil || *got.TxHash != tx.String() {
		t.Fatalf("paid after reorg = %+v (must be counted once)", got)
	}
	if tl := in.timeline(p.ID); tl != "created confirming awaiting confirming paid" {
		t.Fatalf("timeline = %s", tl)
	}
	d := in.detail(p.ID)
	var statuses []string
	for _, tr := range d.Transfers {
		statuses = append(statuses, string(tr.Status))
	}
	slices.Sort(statuses)
	if strings.Join(statuses, ",") != "confirmed,orphaned" {
		t.Fatalf("transfers = %v; want the orphaned and the confirmed record", statuses)
	}

	// Merchants are told "paid" once (one event id), and every message is
	// signed.
	in.eventually("the payment.paid webhook", func() bool {
		return slices.Contains(uniqueTypes(in.recv.forPayment(p.ID)), string(core.EventPaymentPaid))
	})
	ds := in.recv.forPayment(p.ID)
	if got := strings.Join(uniqueTypes(ds), " "); got != "payment.created payment.confirming payment.awaiting payment.confirming payment.paid" {
		t.Fatalf("webhooks = %s", got)
	}
	verifySignatures(t, in.clock, secret, ds)
	in.mine(10)
	in.synced()
	if got := in.payment(key, p.ID); got.Status != core.StatusPaid || got.AmountConfirmed != "25.00" {
		t.Fatalf("later = %+v", got)
	}
}

// Scenario 6: the process stops gracefully while a payment is confirming
// and starts again on the same data directory. Confirmation counting
// continues from the durable checkpoint, the payment converges to paid, and
// no transition is replayed: every webhook beyond at-least-once retries
// carries an event id already seen.
func TestRestartMidConfirmation(t *testing.T) {
	t.Parallel()
	in := newInstance(t)
	key := in.setup()
	in.must(200, "PUT", "/api/v1/settings/payments", map[string]any{"ttl_seconds": 3600, "required_confirmations": 10, "underpayment_tolerance": "0"})
	in.endpoint(key)
	in.synced()
	p := in.createPayment(key, "25.00")
	in.pay(p.Address, 25)
	in.mine(settleDepth + 4)
	in.synced()
	if got := in.payment(key, p.ID); got.Status != core.StatusConfirming || got.Confirmations != 4 {
		t.Fatalf("before restart = %+v", got)
	}
	in.eventually("the confirming webhook before the restart", func() bool {
		return slices.Contains(uniqueTypes(in.recv.forPayment(p.ID)), string(core.EventPaymentConfirming))
	})

	// The process goes away; the chain does not wait for it.
	in.stop()
	in.mine(3)

	in.start()
	in.must(200, "POST", "/api/v1/auth/login", map[string]any{"email": operatorEmail, "password": operatorPassword})
	in.synced()
	if got := in.payment(key, p.ID); got.Status != core.StatusConfirming || got.Confirmations != 7 {
		t.Fatalf("after restart = %+v (counting must continue: 4 + 3)", got)
	}
	in.mine(settleDepth + 10)
	got := in.waitStatus(key, p.ID, core.StatusPaid)
	if got.AmountConfirmed != "25.00" {
		t.Fatalf("paid = %+v", got)
	}
	if tl := in.timeline(p.ID); tl != "created confirming paid" {
		t.Fatalf("timeline = %s (a restart must not replay or lose a transition)", tl)
	}
	in.eventually("the paid webhook after the restart", func() bool {
		return slices.Contains(uniqueTypes(in.recv.forPayment(p.ID)), string(core.EventPaymentPaid))
	})
	ds := in.recv.forPayment(p.ID)
	if got := strings.Join(uniqueTypes(ds), " "); got != "payment.created payment.confirming payment.paid" {
		t.Fatalf("webhook event types by id = %s", got)
	}
	// Redeliveries, if any, are of the same event (same id, same type).
	byID := map[string]core.EventType{}
	for _, d := range ds {
		if typ, ok := byID[d.EventID]; ok && typ != d.Envelope.Type {
			t.Fatalf("event id %s delivered as %s and %s", d.EventID, typ, d.Envelope.Type)
		}
		byID[d.EventID] = d.Envelope.Type
	}
	// The checkpoint was persisted and reused: nothing re-scanned from
	// scratch, no checkpoint was discarded.
	logs := in.logs.String()
	if strings.Contains(logs, "checkpoint_discarded") || strings.Contains(logs, "checkpoint is unreadable") {
		t.Fatal("the checkpoint was discarded on restart")
	}
	if n := strings.Count(logs, "without checkpoint") + strings.Count(logs, "has no checkpoint"); n != 1 {
		t.Fatalf("the watcher started without a checkpoint %d times; want only the first start", n)
	}
}
