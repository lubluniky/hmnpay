package core_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// TestPaidWaitsForFinality: a transfer with every confirmation it needs is
// not final while its block is above the head's final block. The payment
// stays confirming, says what it waits for — in the payment view, the
// transfer view, the public checkout view and the node status — and is paid
// the moment the chain finalises the block; no event goes out before that.
func TestPaidWaitsForFinality(t *testing.T) {
	e := coretest.NewEnv(t)
	e.Head(100)
	p := e.CreatePayment("25")
	e.Pay(p, "25", 101)
	// 40 confirmations, and the chain has finalised block 100.
	e.HeadFinal(140, 100)
	got := e.Payment(p.ID)
	if got.Status != core.StatusConfirming || got.AmountConfirmed != "0.00" || got.Confirmations != got.RequiredConfirmations {
		t.Fatalf("block 101 with every confirmation, finalised up to 100: %s, confirmed %s, %d/%d confirmations",
			got.Status, got.AmountConfirmed, got.Confirmations, got.RequiredConfirmations)
	}
	if w := got.AwaitingFinality; w == nil || w.Block != 101 || w.Final != 100 {
		t.Fatalf("awaiting finality: %+v", w)
	}
	detail, err := e.Service.GetPaymentDetail(e.System(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Transfers) != 1 || !detail.Transfers[0].AwaitingFinality || detail.Transfers[0].Status != core.TransferPending {
		t.Fatalf("transfer view: %+v", detail.Transfers)
	}
	checkout, err := e.Service.GetCheckout(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w := checkout.AwaitingFinality; w == nil || w.Block != 101 {
		t.Fatalf("checkout: awaiting finality %+v", w)
	}
	ns, err := e.Service.NodeStatus(e.Relogin())
	if err != nil {
		t.Fatal(err)
	}
	if w := ns.AwaitingFinality; w == nil || w.Payments != 1 || w.Block != 101 || w.Final != 100 {
		t.Fatalf("node status: awaiting finality %+v", w)
	}
	if !slices.ContainsFunc(ns.Warnings, func(s string) bool { return strings.Contains(s, "waiting for the chain to finalise block 101") }) {
		t.Fatalf("node status warnings: %q", ns.Warnings)
	}
	if types := e.EventTypes(p.ID); slices.Contains(types, core.EventPaymentPaid) {
		t.Fatalf("events before finality: %v", types)
	}

	e.HeadFinal(141, 101)
	got = e.Payment(p.ID)
	if got.Status != core.StatusPaid || got.AmountConfirmed != "25.00" || got.AwaitingFinality != nil {
		t.Fatalf("block 101 finalised: %s, confirmed %s, awaiting %+v", got.Status, got.AmountConfirmed, got.AwaitingFinality)
	}
	if ns, err := e.Service.NodeStatus(e.Relogin()); err != nil || ns.AwaitingFinality != nil {
		t.Fatalf("node status after finality: %+v, %v", ns.AwaitingFinality, err)
	}
}

// TestFinalityIsNotEnoughWithoutTheConfirmations: the confirmations are a
// floor — a block the chain has finalised is final only with them.
func TestFinalityIsNotEnoughWithoutTheConfirmations(t *testing.T) {
	e := coretest.NewEnv(t)
	e.Head(100)
	p := e.CreatePayment("25")
	e.Pay(p, "25", 101)
	e.HeadFinal(110, 110) // 10 of 15 confirmations, finalised
	if got := e.Payment(p.ID); got.Status != core.StatusConfirming || got.AwaitingFinality != nil {
		t.Fatalf("finalised with 10 of 15 confirmations: %s, awaiting %+v", got.Status, got.AwaitingFinality)
	}
	e.HeadFinal(115, 115)
	if got := e.Payment(p.ID); got.Status != core.StatusPaid {
		t.Fatalf("finalised with 15 confirmations: %s", got.Status)
	}
}

// TestNewPaymentStartsAtTheFinalBlock: the start block of a new payment's
// scan is the head's final block, also when that is 0 (block 1 then: no
// start block is 0) — never the processed head.
func TestNewPaymentStartsAtTheFinalBlock(t *testing.T) {
	for _, c := range []struct{ head, final, want uint64 }{{50_016, 49_992, 49_992}, {200, 0, 1}, {120, 120, 120}} {
		e := coretest.NewEnv(t)
		e.HeadFinal(c.head, c.final)
		p := e.CreatePayment("25")
		cs, err := e.Service.ChainState(e.System())
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range cs.Addresses {
			if a.PaymentID == p.ID && a.SinceBlock != c.want {
				t.Fatalf("head %d final %d: start block %d, want %d", c.head, c.final, a.SinceBlock, c.want)
			}
		}
	}
}

// TestFinalityWaitIsShownForAllTheMoney: a payment's full price has its
// confirmations and waits for finality; a small second transfer arrives on
// top. The payment still says what it waits for — the block of the oldest
// pending transfer, the first to be final — as the node status does.
func TestFinalityWaitIsShownForAllTheMoney(t *testing.T) {
	e := coretest.NewEnv(t)
	e.Head(100)
	p := e.CreatePayment("25")
	e.Pay(p, "25", 101)
	e.HeadFinal(130, 100)
	e.Pay(p, "1", 131)
	got := e.Payment(p.ID)
	if w := got.AwaitingFinality; w == nil || w.Block != 101 || w.Final != 100 {
		t.Fatalf("with a second transfer on top: awaiting %+v", w)
	}
	ns, err := e.Service.NodeStatus(e.Relogin())
	if err != nil || ns.AwaitingFinality == nil || ns.AwaitingFinality.Block != 101 {
		t.Fatalf("node status: %+v, %v", ns.AwaitingFinality, err)
	}
}

// TestFinalBlockNeverGoesBack: a head that reports a lower final block than
// the one stored (the processed head went back, or the chain adapter lost its
// records) keeps the stored one; a new payment's scan starts at the lower of
// the final block and the head.
func TestFinalBlockNeverGoesBack(t *testing.T) {
	e := coretest.NewEnv(t)
	e.HeadFinal(200, 190)
	e.HeadFinal(201, 150)
	cs, err := e.Service.ChainState(e.System())
	if err != nil || cs.Head.Final != 190 {
		t.Fatalf("final after a head that says less: %d, %v", cs.Head.Final, err)
	}
	e.HeadFinal(180, 170) // the head itself went back, below the final block
	p := e.CreatePayment("25")
	cs, err = e.Service.ChainState(e.System())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cs.Addresses {
		if a.PaymentID == p.ID && (a.SinceBlock != 180 || cs.Head.Final != 190) {
			t.Fatalf("start block %d, final %d; want the head 180 and the final block kept at 190", a.SinceBlock, cs.Head.Final)
		}
	}
}
