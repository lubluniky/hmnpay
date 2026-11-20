package corebridge

import (
	"errors"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// TestNoAddressBeforeTheFirstHead: a payment's address is scanned for from
// the processed head the core held at its creation. Before the core has one,
// nothing the instance knows bounds the block in which the payer may pay —
// a block found by the creation time is only as right as the server clock
// was then — so no address is handed out.
//
// The case that shows why: the first invoice is created while the server
// clock is five minutes fast and the watcher has not run yet. The clock is
// corrected, the invoice is paid, and when the watcher starts, the block
// "at the invoice's creation time" is five minutes of blocks above the
// payment. Both endpoints are honest and agree on every answer.
func TestNoAddressBeforeTheFirstHead(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.advance(5 * time.Minute) // the server clock runs fast
	p, _, err := w.env.Service.CreatePayment(w.env.System(), core.CreatePaymentInput{Amount: "25"})
	if err == nil {
		// What handing out the address would have cost.
		w.advance(-5 * time.Minute)
		w.pay(p, 25)
		w.mine(1)
		for range 145 {
			w.advance(time.Second)
			w.mine(1)
		}
		w.start()
		w.synced()
		t.Fatalf("an address was handed out before the core had processed a head; after the watcher started and processed block %d, the invoice shows %s received",
			w.coreHead().Number, w.env.Payment(p.ID).AmountReceived)
	}
	if !errors.Is(err, core.ErrChainNotReady) || core.KindOf(err) != core.KindUnavailable {
		t.Fatalf("payment before the first head: %v", err)
	}
	w.advance(-5 * time.Minute) // corrected

	// Once the watcher has processed a block, payments are created — and
	// the clock no longer matters to where they are looked for.
	w.start()
	w.firstHead()
	w.advance(5 * time.Minute) // fast again
	p = w.env.CreatePayment("25")
	w.advance(-5 * time.Minute)
	cs, err := w.env.Service.ChainState(w.env.System())
	if err != nil || len(cs.Addresses) != 1 || cs.Addresses[0].SinceBlock == 0 || cs.Addresses[0].SinceBlock > w.chain.Head().Number {
		t.Fatalf("start block of a payment created at head %d: %+v, %v", w.coreHead().Number, cs.Addresses, err)
	}
	w.pay(p, 25)
	w.mine(1)
	for range 145 {
		w.advance(time.Second)
		w.mine(1)
	}
	w.synced()
	if got := w.waitStatus(p, core.StatusPaid); got.AmountConfirmed != "25.00" {
		t.Fatalf("payment: %+v", got)
	}
}
