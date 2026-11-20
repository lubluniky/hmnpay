package core_test

// Late funds are judged as a sum, however the payer split them: money sent
// in pieces below the dust limit still surfaces.

import (
	"testing"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// lateFundsEvents counts the payment.late_funds events of a payment.
func lateFundsEvents(e *coretest.Env, paymentID string) int {
	n := 0
	for _, typ := range e.EventTypes(paymentID) {
		if typ == core.EventPaymentLateFunds {
			n++
		}
	}
	return n
}

// Late funds were judged against the dust limit one evaluation at a
// time: on a settled payment a transfer that confirmed raised
// AttentionLateFunds only if what became final in that very evaluation was
// not dust — so money that arrived after settlement in pieces below one
// unit, each confirmed by a different head, added up on the payment
// (AmountReceived) without ever raising attention or emitting
// payment.late_funds. The same pieces in one block were flagged. The rule is
// about amounts, not about how the payer's wallet or exchange split them.
func TestLateFundsInSubDustPiecesSurface(t *testing.T) {
	e := coretest.NewEnv(t)
	requireOneConfirmation(t, e)
	e.Head(100)
	p := e.CreatePayment("100")
	e.Expire(p)
	e.Head(999)
	if got := e.Payment(p.ID); got.Status != core.StatusExpired {
		t.Fatalf("test setup: status %s", got.Status)
	}
	// 60 USDT arrive a few minutes late, in 80 transfers of 0.75 USDT, one
	// per block.
	block := uint64(1000)
	for i := 0; i < 80; i++ {
		block++
		e.Pay(e.Payment(p.ID), "0.75", block)
		e.Head(block)
		if i == 0 {
			// One piece is dust and stays dust.
			if got := e.Payment(p.ID); got.Attention != core.AttentionNone || lateFundsEvents(e, p.ID) != 0 {
				t.Fatalf("0.75 on an expired payment of 100 was flagged: attention %q", got.Attention)
			}
		}
		if i == 1 {
			// Two are 1.50: not dust any more.
			if got := e.Payment(p.ID); got.Attention != core.AttentionLateFunds || !got.NeedsAttention || lateFundsEvents(e, p.ID) != 1 {
				t.Fatalf("1.50 arrived in two pieces: attention %q, needs attention %v, %d late-funds events", got.Attention, got.NeedsAttention, lateFundsEvents(e, p.ID))
			}
		}
	}
	got := e.Payment(p.ID)
	late := lateFundsEvents(e, p.ID)
	t.Logf("expired payment of 100: received %s after it closed; attention %q, needs attention %v, payment.late_funds events %d",
		got.AmountReceived, got.Attention, got.NeedsAttention, late)
	if got.Attention == core.AttentionNone || late == 0 {
		t.Fatalf("%s arrived on an expired payment and nothing flags it: no attention, no payment.late_funds event", got.AmountReceived)
	}
	if got.Status != core.StatusExpired || got.AmountReceived != "60.00" {
		t.Fatalf("status %s, received %s", got.Status, got.AmountReceived)
	}
	// Every 1.50 is told once: 40 events for 80 pieces, not 80 and not 1.
	if late != 40 {
		t.Fatalf("%d payment.late_funds events for 80 pieces of 0.75, want 40", late)
	}
	// Replays change nothing.
	e.Head(block)
	e.Head(block + 50)
	if n := lateFundsEvents(e, p.ID); n != late {
		t.Fatalf("replayed heads emitted %d more late-funds events", n-late)
	}
}

// The sum that is judged starts again when the operator resolves the
// issue, and the pieces before that are not counted twice.
func TestLateFundsAfterAResolution(t *testing.T) {
	e := coretest.NewEnv(t)
	requireOneConfirmation(t, e)
	e.Head(100)
	p := e.CreatePayment("100")
	e.Expire(p)
	e.Head(999)
	e.Pay(e.Payment(p.ID), "30", 1000)
	e.Head(1000)
	if got := e.Payment(p.ID); got.Attention != core.AttentionLateFunds || lateFundsEvents(e, p.ID) != 1 {
		t.Fatalf("late funds of 30: attention %q", got.Attention)
	}
	// One more piece of dust, then the operator refunds and resolves.
	e.Pay(e.Payment(p.ID), "0.75", 1001)
	e.Head(1001)
	if _, err := e.Service.ResolvePayment(e.Operator(), p.ID, "refunded 30.75"); err != nil {
		t.Fatal(err)
	}
	// Dust after the resolution is dust: the 0.75 from before it does not
	// count towards the next flag.
	e.Pay(e.Payment(p.ID), "0.75", 1002)
	e.Head(1002)
	if got := e.Payment(p.ID); got.NeedsAttention || lateFundsEvents(e, p.ID) != 1 {
		t.Fatalf("0.75 after the resolution reopened the issue (needs attention %v, %d events)", got.NeedsAttention, lateFundsEvents(e, p.ID))
	}
	// A second piece makes it 1.50 since the resolution: attention again.
	e.Pay(e.Payment(p.ID), "0.75", 1003)
	e.Head(1003)
	got := e.Payment(p.ID)
	if !got.NeedsAttention || got.Attention != core.AttentionLateFunds || lateFundsEvents(e, p.ID) != 2 {
		t.Fatalf("1.50 since the resolution: needs attention %v, attention %q, %d late-funds events", got.NeedsAttention, got.Attention, lateFundsEvents(e, p.ID))
	}
}

// A payment that is paid is told about extras the same way.
func TestExtrasOnAPaidPaymentAddUp(t *testing.T) {
	e := coretest.NewEnv(t)
	requireOneConfirmation(t, e)
	e.Head(100)
	p := e.CreatePayment("100")
	e.Pay(p, "100", 101)
	e.Head(101)
	if got := e.Payment(p.ID); got.Status != core.StatusPaid || got.Attention != core.AttentionNone {
		t.Fatalf("status %s attention %q", got.Status, got.Attention)
	}
	for i, block := range []uint64{102, 103, 104} {
		e.Pay(e.Payment(p.ID), "0.4", block)
		e.Head(block)
		got := e.Payment(p.ID)
		if flagged := got.Attention == core.AttentionLateFunds; flagged != (i == 2) {
			t.Fatalf("after %d extras of 0.40: attention %q", i+1, got.Attention)
		}
	}
	if got := e.Payment(p.ID); got.Status != core.StatusPaid || lateFundsEvents(e, p.ID) != 1 {
		t.Fatalf("status %s, %d late-funds events", got.Status, lateFundsEvents(e, p.ID))
	}
}
