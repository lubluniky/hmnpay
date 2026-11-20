package corebridge

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// The instance of these tests is the one the quiet state is for: a payment
// was paid, it is settled and final, and its address stays on the watch list
// for a week. Nothing else is watched.

// quietWorld starts a world with two endpoints, has a payment of 25 paid and
// confirmed, lets its transfer leave the watcher's reorg window, and waits
// for the watcher to come to rest quiet. Unless an option says otherwise the
// watcher then pauses for an hour: whatever it does after that, it does
// because somebody woke it.
func quietWorld(t *testing.T, opts ...worldOption) (*world, core.PaymentView) {
	t.Helper()
	w := newWorld(t, append([]worldOption{withNodes(2), withOptions(func(o *Options) {
		o.IdlePollInterval = time.Hour
		o.HealthInterval = 5 * time.Millisecond
	})}, opts...)...)
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	w.mine(testSettle + testConfirmations + 1)
	p = w.waitStatus(p, core.StatusPaid)
	// Paid, closed — and not at rest yet: the transfer is inside the
	// watcher's reorg window, and stays there while the chain stands still.
	// (The core says "paid" from within a tick, so the status is waited
	// for: until that tick has ended it is the one before's.)
	w.eventually("the watcher to be at work for the transfer it tracks", func() bool {
		st := w.watcherStatus()
		return st.Tracked == 1 && !st.Idle && !st.Quiet
	})
	w.never("a watcher at rest with a transfer inside the reorg window", func() bool {
		st := w.watcherStatus()
		return st.Idle || st.Quiet
	})
	w.quiet()
	return w, p
}

// quiet lets whatever the watcher tracks leave its reorg window and waits
// for it to come to rest quiet.
func (w *world) quiet() {
	w.t.Helper()
	w.mine(watcher.DefaultReorgWindow + testSettle + 2)
	w.eventually("the watcher to be quiet", func() bool {
		st := w.watcherStatus()
		return st.Idle && st.Quiet
	})
}

// TestQuietWithOnlyClosedPaymentsWatched: the address of a paid payment is on
// the watch list — closed — and the watcher rests: blocks are mined and no
// tick looks at them, events that change nothing about the chain pass
// without a scan, and the node status says what is going on.
func TestQuietWithOnlyClosedPaymentsWatched(t *testing.T) {
	w, p := quietWorld(t)
	cs, err := w.env.Service.ChainState(w.env.System())
	if err != nil || len(cs.Addresses) != 1 || !cs.Addresses[0].Closed || cs.Addresses[0].PaymentID != p.ID || len(cs.PendingTransfers) != 0 || cs.Active() {
		t.Fatalf("chain state: %+v, %v", cs, err)
	}
	if st := w.watcherStatus(); st.WatchedAddresses != 1 || st.Tracked != 0 || st.Work != "" || st.Hold != "" {
		t.Fatalf("watcher status while quiet: %+v", st)
	}

	// Quiet means quiet: the chain moves on, the bridge re-reads the
	// core's state every 25 ms, and nothing wakes the watcher.
	before := w.scans()
	head := w.coreHead().Number
	w.mine(80)
	w.never("a tick of a quiet watcher that nobody woke", func() bool { return w.scans() != before })

	// The node status: idle, of the quiet kind, with one address watched —
	// and healthy, although the head is 80 blocks behind the chain.
	var ns core.NodeStatus
	w.eventually("the node status to show the chain ahead of a quiet watcher", func() bool {
		ns, err = w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && ns.LatestBlock == w.chain.Head().Number
	})
	if !ns.Idle || !ns.Quiet || !ns.Healthy || ns.Starting || ns.Head.Number != head || ns.LagBlocks != 80+testSettle || ns.WatchedAddresses != 1 {
		t.Fatalf("node status of a quiet instance, 80 blocks after its last tick: idle=%v quiet=%v healthy=%v head=%d (was %d) lag=%d watched=%d\n%+v",
			ns.Idle, ns.Quiet, ns.Healthy, ns.Head.Number, head, ns.LagBlocks, ns.WatchedAddresses, ns)
	}
	if ns.Hold != "" || len(ns.Warnings) != 0 || ns.Primary == "" || ns.Verifier == "" || ns.LogVerifier == "" || ns.LogsUnchecked {
		t.Fatalf("node status of a quiet instance: %+v", ns)
	}

	// Events that leave the chain state as it is do not wake the watcher.
	if _, err := w.env.Service.CreateLink(w.env.Operator(), core.LinkInput{Name: "Top-up", Active: true}); err != nil {
		t.Fatal(err)
	}
	w.never("a tick for a payment link that was created", func() bool { return w.scans() != before })
}

// TestQuietWatcherIsWokenByAPayment: with the address of a closed payment on
// the watch list and an hour between ticks, every way a payment comes into
// being wakes the watcher — and the new payment is found and settled as fast
// as on a watcher that never rested.
func TestQuietWatcherIsWokenByAPayment(t *testing.T) {
	create := map[string]func(w *world) core.PaymentView{
		"created by the API": func(w *world) core.PaymentView { return w.env.CreatePayment("40") },
		"created by a payment-link checkout": func(w *world) core.PaymentView {
			link, err := w.env.Service.CreateLink(w.env.Operator(), core.LinkInput{Name: "Top-up", Active: true})
			if err != nil {
				t.Fatal(err)
			}
			co, _, err := w.env.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "40"})
			if err != nil {
				t.Fatal(err)
			}
			return w.env.Payment(co.ID)
		},
	}
	for name, make := range create {
		t.Run(name, func(t *testing.T) {
			w, old := quietWorld(t)
			before := w.scans()
			w.mine(10)
			w.never("a tick of a quiet watcher that nobody woke", func() bool { return w.scans() != before })
			stale := w.coreHead().Final // the start block: the final block of the head at creation

			started := time.Now()
			p := make(w)
			w.eventually("the wake-up tick to see the new payment's address", func() bool {
				return w.watcherStatus().WatchedAddresses == 2
			})
			woke := time.Since(started)
			if woke > 5*time.Second {
				t.Errorf("the watcher saw the new address %s after the payment was created", woke)
			}
			t.Logf("wake-up tick completed %s after the payment was created (idle interval: an hour)", woke.Round(time.Millisecond))
			if st := w.watcherStatus(); st.Idle || st.Quiet || !strings.Contains(st.Work, "1 of the 2 watched address(es) are not closed") {
				t.Fatalf("at rest with an open payment: %+v", st)
			}

			// Its start block is the head the core held at its creation —
			// ten blocks old, lower than the chain, never higher.
			cs, err := w.env.Service.ChainState(w.env.System())
			if err != nil || len(cs.Addresses) != 2 || !cs.Active() {
				t.Fatalf("chain state: %+v, %v", cs, err)
			}
			for _, a := range cs.Addresses {
				switch a.PaymentID {
				case old.ID:
					if !a.Closed {
						t.Fatalf("the paid payment's address is not closed: %+v", a)
					}
				case p.ID:
					if a.Closed || a.SinceBlock != stale || a.SinceBlock > w.chain.Head().Number-testSettle-10 {
						t.Fatalf("the new payment's address: %+v; the core's head at creation was %d, the chain is at %d", a, stale, w.chain.Head().Number)
					}
				}
			}

			w.pay(p, 40)
			w.mine(testSettle + testConfirmations + 1)
			w.waitStatus(p, core.StatusPaid)
			ns, err := w.env.Service.NodeStatus(w.env.Operator())
			if err != nil || ns.Idle || ns.Quiet {
				t.Fatalf("node status says idle right after a payment was paid: %+v, %v", ns, err)
			}
			// Both payments closed, and the new transfer out of the reorg
			// window: quiet again, with two addresses.
			w.quiet()
			if st := w.watcherStatus(); st.WatchedAddresses != 2 {
				t.Fatalf("watcher status: %+v", st)
			}
		})
	}
}

// TestQuietWatcherIsWokenByARescan: Bridge.Rescan does not wait for the
// quiet pause to end.
func TestQuietWatcherIsWokenByARescan(t *testing.T) {
	w, _ := quietWorld(t)
	before := w.scans()
	w.never("a tick of a quiet watcher that nobody woke", func() bool { return w.scans() != before })
	if !w.bridge.Rescan(w.coreHead().Number - 10) {
		t.Fatal("no running watcher")
	}
	w.eventually("the re-scan", func() bool {
		return w.scans() != before && strings.Contains(w.logs.String(), "chain watcher re-scan requested")
	})
	w.eventually("the watcher to be quiet again", func() bool { return w.watcherStatus().Quiet })
}

// TestQuietWatcherIsNotWokenByBookkeeping: what an operator does to a settled
// payment — resolving its issue, reopening the resolution — and cancelling
// are the operations on payments besides creating one. Cancelling needs a
// payment that is still awaiting, which keeps the watcher at work anyway.
// The other two change neither a status nor a transfer: the chain state is
// as inactive after them as before, and the watcher is left alone.
func TestQuietWatcherIsNotWokenByBookkeeping(t *testing.T) {
	w := newWorld(t, withNodes(2), withOptions(func(o *Options) {
		o.IdlePollInterval = time.Hour
		o.HealthInterval = 5 * time.Millisecond
	}))
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	w.pay(p, 30) // overpaid: an issue to resolve
	w.mine(testSettle + testConfirmations + 1)
	p = w.waitStatus(p, core.StatusOverpaid)
	if !p.NeedsAttention {
		t.Fatalf("payment: %+v", p)
	}
	w.quiet()

	before := w.scans()
	if _, err := w.env.Service.ResolvePayment(w.env.Operator(), p.ID, "refunded 5"); err != nil {
		t.Fatal(err)
	}
	w.never("a tick for a resolution", func() bool { return w.scans() != before })
	if _, err := w.env.Service.ReopenPayment(w.env.Operator(), p.ID); err != nil {
		t.Fatal(err)
	}
	w.never("a tick for a reopened resolution", func() bool { return w.scans() != before })
	if got := w.events(p); !strings.HasSuffix(got, "resolved reopened") {
		t.Fatalf("events: %s", got)
	}
	cs, err := w.env.Service.ChainState(w.env.System())
	if err != nil || cs.Active() || len(cs.Addresses) != 1 || !cs.Addresses[0].Closed {
		t.Fatalf("chain state: %+v, %v", cs, err)
	}
	if st := w.watcherStatus(); !st.Quiet {
		t.Fatalf("not quiet any more: %+v", st)
	}

	// A payment that is cancelled was open until then: the watcher was at
	// work for it, and is quiet again once it is not.
	q := w.env.CreatePayment("10")
	w.eventually("the watcher to be at work for the new payment", func() bool {
		st := w.watcherStatus()
		return st.WatchedAddresses == 2 && !st.Idle
	})
	if _, err := w.env.Service.CancelPayment(w.env.Operator(), q.ID); err != nil {
		t.Fatal(err)
	}
	w.eventually("the watcher to be quiet with two closed payments", func() bool {
		st := w.watcherStatus()
		return st.Quiet && st.WatchedAddresses == 2
	})
}

// TestLateFundsOnAClosedPaymentWhileQuiet: money is sent to the address of
// the paid payment again.
//
// With an hour between ticks nothing notices: nobody tells the watcher that
// a block was mined, and that is the price of the quiet state — late funds on
// a payment that is already closed are noticed when the pause ends, not
// before.
//
// With a pause that does end (half a second here) the tick that ends it
// finds the transfer, and from that moment the instance is not at rest: the
// watcher tracks the transfer, the core holds it as pending, the node status
// is neither idle nor quiet. Its confirmations are followed tick by tick; the
// payment — still paid — is flagged for late funds; and when the transfer is
// final and out of the reorg window, the watcher is quiet again.
func TestLateFundsOnAClosedPaymentWhileQuiet(t *testing.T) {
	t.Run("not before the pause ends", func(t *testing.T) {
		w, p := quietWorld(t)
		before := w.scans()
		w.pay(p, 7)
		w.mine(testSettle + testConfirmations + 5)
		w.never("a tick of a quiet watcher that nobody woke", func() bool { return w.scans() != before })
		if got := w.env.Payment(p.ID); got.AmountReceived != "25.00" || got.NeedsAttention {
			t.Fatalf("payment: %+v", got)
		}
	})

	t.Run("by the tick that ends the pause, and followed from there", func(t *testing.T) {
		w, p := quietWorld(t, withOptions(func(o *Options) { o.IdlePollInterval = 500 * time.Millisecond }))
		w.pay(p, 7)
		w.mine(1)
		w.eventually("the late transfer to reach the core", func() bool { return w.env.Payment(p.ID).AmountReceived == "32.00" })
		got := w.env.Payment(p.ID)
		if got.Status != core.StatusPaid || got.AmountConfirmed != "25.00" || got.NeedsAttention {
			t.Fatalf("payment with a pending late transfer: %+v", got)
		}
		// Not at rest: the chain stands still, so this holds until the
		// test mines again.
		cs, err := w.env.Service.ChainState(w.env.System())
		if err != nil || !cs.Active() || len(cs.PendingTransfers) != 1 || cs.Addresses[0].Closed {
			t.Fatalf("chain state with a pending late transfer: %+v, %v", cs, err)
		}
		w.eventually("the watcher to be at work for the pending transfer", func() bool {
			st := w.watcherStatus()
			return !st.Idle && !st.Quiet && st.Tracked == 1 && strings.Contains(st.Work, "transfer(s)")
		})
		w.eventually("the node status to say so", func() bool {
			ns, err := w.env.Service.NodeStatus(w.env.Operator())
			return err == nil && !ns.Idle && !ns.Quiet && ns.Healthy
		})
		w.never("an idle pause while a late transfer is pending", func() bool { return w.watcherStatus().Idle })

		// Its confirmations: final, flagged, the payment still paid.
		w.mine(testSettle + testConfirmations)
		w.eventually("the late funds to be flagged", func() bool { return w.env.Payment(p.ID).NeedsAttention })
		got = w.env.Payment(p.ID)
		if got.Status != core.StatusPaid || got.Attention != core.AttentionLateFunds || got.AmountConfirmed != "32.00" {
			t.Fatalf("payment after late funds: %+v", got)
		}
		if ev := w.events(got); !strings.HasSuffix(ev, "paid late_funds") {
			t.Fatalf("events: %s", ev)
		}
		// Final for the core, but inside the watcher's reorg window: at
		// work. Out of it: quiet again.
		if st := w.watcherStatus(); st.Idle || st.Tracked != 1 {
			t.Fatalf("at rest with a transfer inside the reorg window: %+v", st)
		}
		w.quiet()
		if st := w.bridge.Status(); len(st.Alerts) != 0 {
			t.Fatalf("alerts: %+v", st.Alerts)
		}
	})
}

// TestNodeStatusQuietEndsWhenTheTickIsOverdue: like idle, quiet excuses the
// distance between the core's head and the chain only while the ticks keep
// coming. A watcher whose tick hangs still says what its last tick said; the
// node status does not go on calling that idle, or quiet.
func TestNodeStatusQuietEndsWhenTheTickIsOverdue(t *testing.T) {
	w, _ := quietWorld(t, withOptions(func(o *Options) { o.IdlePollInterval = 300 * time.Millisecond }))
	w.eventually("the node status to say quiet", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && ns.Idle && ns.Quiet && ns.Healthy
	})

	// The next tick's first request never returns.
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	t.Cleanup(open)
	for _, n := range w.nodes {
		n.Set(func(b *evmtest.Behavior) {
			b.FinalityDepth = w.finality
			b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
				if method == "eth_getBlockByNumber" {
					<-release
				}
				return nil, nil, false
			}
		})
	}
	w.mine(80)
	w.eventually("idle and quiet to end, and the lag to count, once the tick is overdue", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && !ns.Idle && !ns.Quiet && !ns.Healthy && ns.LagBlocks > 50
	})
	if st := w.watcherStatus(); !st.Quiet {
		t.Fatalf("test setup: the watcher's own status should still be that of its last tick: %+v", st)
	}

	open()
	w.eventually("the node status to say quiet again", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && ns.Idle && ns.Quiet && ns.Healthy
	})
}

// TestQuietSwitchedOffWithIdlePolling: HMNPAY_IDLE_POLL_INTERVAL=off is
// Options.IdlePollInterval < 0, and there is no quiet state then either.
func TestQuietSwitchedOffWithIdlePolling(t *testing.T) {
	w := newWorld(t, withNodes(2), withOptions(func(o *Options) { o.IdlePollInterval = -1 }))
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	w.mine(testSettle + testConfirmations + 1)
	w.waitStatus(p, core.StatusPaid)
	w.mine(watcher.DefaultReorgWindow + testSettle + 2)
	w.synced()
	before := w.scans()
	w.eventually("ticks at the poll interval", func() bool { return w.scans() >= before+20 })
	if st := w.watcherStatus(); st.Idle || st.Quiet {
		t.Fatalf("at rest with idle polling off: %+v", st)
	}
	ns, err := w.env.Service.NodeStatus(w.env.Operator())
	if err != nil || ns.Idle || ns.Quiet || !ns.Healthy {
		t.Fatalf("node status with idle polling off: %+v, %v", ns, err)
	}
}
