package corebridge

import (
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// A payment's watch window is a promise about blocks: money in a block mined
// before the window ends is seen. The watcher looks at those blocks later —
// a quiet watcher up to its idle interval later, any watcher after an outage
// by however long that lasted — so the address has to stay on the watch list
// until the chain the watcher has processed is past the window, not merely
// the wall clock.

// TestLateFundsMinedJustBeforeTheWatchWindowEndsAreFound: the paid payment of
// a quiet instance is watched for a week. Ten seconds before the week is up
// 5 USDT more arrive; the watcher, pausing for an hour, does not look before
// the week is over by the wall clock. When it does look, the address is still
// on the list, and the 5 USDT are late funds on the payment.
func TestLateFundsMinedJustBeforeTheWatchWindowEndsAreFound(t *testing.T) {
	w, p := quietWorld(t)
	w.advance(core.DefaultWatchWindow - 10*time.Second)
	w.pay(p, 5)
	paidIn := w.mine(testSettle+testConfirmations+2).Number - uint64(testSettle+testConfirmations+1)
	w.advance(20 * time.Second) // the window has ended by the wall clock

	cs, err := w.env.Service.ChainState(w.env.System())
	if err != nil || len(cs.Addresses) != 1 || cs.Addresses[0].PaymentID != p.ID {
		t.Fatalf("the address left the watch list before the watcher scanned the end of its window: %+v, %v", cs, err)
	}

	// Something unrelated wakes the watcher.
	w.env.CreatePayment("1")
	w.synced()
	w.eventually("the 5 USDT mined inside the window to be credited as late funds", func() bool {
		got := w.env.Payment(p.ID)
		return got.AmountReceived == "30.00" && got.AmountConfirmed == "30.00" && got.NeedsAttention
	})
	if got := w.events(p); !strings.HasSuffix(got, " paid late_funds") {
		t.Fatalf("events of the payment: %q (the late transfer was mined in block %d)", got, paidIn)
	}
}

// TestLateFundsOnAnExpiredInvoiceInsideItsHourAreFound: the same for the
// short window of an invoice that expired unpaid — an hour. The payer is 59
// minutes and 50 seconds late; the watcher looks after the hour is over.
func TestLateFundsOnAnExpiredInvoiceInsideItsHourAreFound(t *testing.T) {
	w := newWorld(t, withNodes(2), withOptions(func(o *Options) {
		o.IdlePollInterval = time.Hour
		o.HealthInterval = 5 * time.Millisecond
	}))
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	w.advance(core.DefaultPaymentTTL + core.DefaultExpiryGrace + time.Second)
	w.mine(testSettle + 2)
	p = w.waitStatus(p, core.StatusExpired)
	w.quiet()
	expired := w.env.Payment(p.ID)

	w.advance(core.DefaultUnfundedWatchWindow - 10*time.Second)
	w.pay(p, 25)
	w.mine(testSettle + testConfirmations + 2)
	w.advance(20 * time.Second)

	w.env.CreatePayment("1")
	w.synced()
	w.eventually("the late payment to be recorded on the expired invoice", func() bool {
		got := w.env.Payment(p.ID)
		return got.AmountConfirmed == "25.00" && got.NeedsAttention
	})
	if got := w.env.Payment(p.ID); got.Status != core.StatusExpired || got.SettledAt == nil || expired.SettledAt == nil || !got.SettledAt.Equal(*expired.SettledAt) {
		t.Fatalf("the late payment changed the outcome: %+v", got)
	}
}

// TestAddressLeavesTheWatchListOnceTheChainHasPassedItsWindow: what keeps an
// address on the list is the processed chain, and only until it has passed
// the window (and the margin for a reorganisation below it): then the address
// leaves, and money sent to it is not looked for any more.
func TestAddressLeavesTheWatchListOnceTheChainHasPassedItsWindow(t *testing.T) {
	w, p := quietWorld(t)
	w.advance(core.DefaultWatchWindow + time.Minute)
	listed := func() bool {
		cs, err := w.env.Service.ChainState(w.env.System())
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range cs.Addresses {
			if a.PaymentID == p.ID {
				return true
			}
		}
		return false
	}
	if !listed() {
		t.Fatal("the address left the watch list while the processed head is a week old")
	}
	// The chain catches up with the clock, but not yet with the margin.
	w.env.CreatePayment("1")
	w.synced()
	if !listed() {
		t.Fatalf("the address left the watch list %s after its window by the chain's clock (margin %s)", time.Minute, core.WatchRetirementMargin)
	}
	w.advance(core.WatchRetirementMargin)
	w.mine(testFinality + 1) // the final block, too, is past the window then
	w.synced()
	w.eventually("the address to leave the watch list", func() bool { return !listed() })
}
