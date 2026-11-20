package corebridge

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

func TestPaymentLifecycle(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.synced()

	p := w.env.CreatePayment("25")
	if got := w.env.Payment(p.ID); got.Status != core.StatusAwaiting {
		t.Fatalf("status = %s", got.Status)
	}
	// The payment was created at the core's processed head: its final
	// block is the one at and before which its address cannot have been
	// paid — the head itself may still be replaced, and the payment mined
	// again at its height.
	cs, _ := w.env.Service.ChainState(w.env.System())
	if len(cs.Addresses) != 1 || cs.Addresses[0].SinceBlock != cs.Head.Final || cs.Head.Final == 0 || cs.Head.Final >= cs.Head.Number || cs.Head.Number != chainStart-testSettle {
		t.Fatalf("chain state = %+v", cs)
	}

	tx := w.pay(p, 25)
	paidIn := w.mine(1).Number

	// Seen within a tick, long before its block is settled: the checkout
	// shows "confirming" with zero confirmations.
	got := w.waitStatus(p, core.StatusConfirming)
	if got.TxHash == nil || *got.TxHash != tx.String() || got.AmountReceived != "25.00" || got.AmountConfirmed != "0.00" || got.Confirmations != 0 {
		t.Fatalf("payment while confirming = %+v", got)
	}
	if h := w.coreHead().Number; h >= paidIn {
		t.Fatalf("core head %d is already at the payment's block %d", h, paidIn)
	}

	// Confirmations count up, one block at a time, as the processed head
	// (SettleDepth below the tip) moves past the payment's block.
	var seen []int
	for i := 0; i < testSettle+testConfirmations; i++ {
		w.mine(1)
		w.synced()
		got = w.env.Payment(p.ID)
		seen = append(seen, got.Confirmations)
	}
	want := []int{0, 0, 0, 1, 2, 3, 3}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("confirmations per block = %v, want %v", seen, want)
		}
	}
	got = w.waitStatus(p, core.StatusPaid)
	if got.AmountConfirmed != "25.00" || got.PaidAt == nil || got.NeedsAttention {
		t.Fatalf("paid payment = %+v", got)
	}
	if ev := w.events(p); ev != "created confirming paid" {
		t.Fatalf("events = %s", ev)
	}

	// The dashboard's node status is fed and healthy.
	w.eventually("node status to be healthy", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && ns.Healthy && ns.LatestBlock == w.chain.Head().Number && len(ns.Endpoints) == 1 &&
			ns.Endpoints[0].InUse && ns.Endpoints[0].Healthy && ns.LagBlocks == testSettle
	})
	if st := w.bridge.Status(); st.State != StateRunning || st.Generation != 1 || st.ChainID != 56 || len(st.Alerts) != 0 {
		t.Fatalf("bridge status = %+v", st)
	}
}

func TestUnderpayThenTopUp(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")

	w.pay(p, 10)
	w.mine(1)
	got := w.waitStatus(p, core.StatusDetected)
	if got.AmountReceived != "10.00" || got.AmountDue != "15.00" {
		t.Fatalf("after partial payment = %+v", got)
	}
	// The partial payment becomes final; the payment stays open for the
	// rest.
	w.mine(testSettle + testConfirmations)
	w.synced()
	if got := w.env.Payment(p.ID); got.Status != core.StatusDetected || got.AmountConfirmed != "10.00" {
		t.Fatalf("after confirmations of the partial payment = %+v", got)
	}

	w.pay(p, 15)
	w.mine(1)
	w.waitStatus(p, core.StatusConfirming)
	w.mine(testSettle + testConfirmations)
	got = w.waitStatus(p, core.StatusPaid)
	if got.AmountConfirmed != "25.00" {
		t.Fatalf("paid = %+v", got)
	}
	if ev := w.events(p); ev != "created detected confirming paid" {
		t.Fatalf("events = %s", ev)
	}
}

func TestExpiresUnpaid(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")

	// Just before the deadline (TTL plus grace) nothing happens...
	w.advance(p.ExpiresAt.Sub(w.env.Clock.Now()) + core.DefaultExpiryGrace - time.Second)
	w.mine(testSettle + 2)
	w.synced()
	if got := w.status(p); got != core.StatusAwaiting {
		t.Fatalf("status before the deadline = %s", got)
	}
	// ...and the deadline is judged by the timestamp of the processed
	// head, not of the tip: blocks stamped after the deadline exist, but
	// the payment only expires when the processed head is one of them.
	w.advance(2 * time.Second)
	w.mine(testSettle)
	w.synced()
	if got := w.status(p); got != core.StatusAwaiting {
		t.Fatalf("expired although the processed head (%s) is before the deadline", w.coreHead().Time)
	}
	w.mine(1)
	w.waitStatus(p, core.StatusExpired)
	if ev := w.events(p); ev != "created expired" {
		t.Fatalf("events = %s", ev)
	}
}

func TestReorgedOutAndReincluded(t *testing.T) {
	// The deep reorganisation below keeps the finalised block.
	w := newWorld(t, withFinality(testSettle+4))
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")

	tx := w.pay(p, 25)
	first := w.mine(1)
	w.waitStatus(p, core.StatusConfirming)

	// The block is reorganised away; for a while the transaction is not in
	// any block.
	dropped := w.chain.Reorg(1)
	w.mine(2)
	w.waitStatus(p, core.StatusAwaiting)

	// It is mined again, further up.
	w.chain.Requeue(dropped...)
	second := w.mine(1)
	if second.Number == first.Number {
		t.Fatal("test setup: expected another block")
	}
	w.waitStatus(p, core.StatusConfirming)
	w.mine(testSettle + 4) // until the chain has finalised its block
	got := w.waitStatus(p, core.StatusPaid)
	if got.AmountConfirmed != "25.00" || got.AmountReceived != "25.00" || *got.TxHash != tx.String() {
		t.Fatalf("paid after reorg = %+v (must be counted once)", got)
	}
	if ev := w.events(p); ev != "created confirming awaiting confirming paid" {
		t.Fatalf("events = %s", ev)
	}
	detail, err := w.env.Service.GetPaymentDetail(w.env.Operator(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Transfers) != 2 {
		t.Fatalf("transfers on record = %d, want the orphaned and the confirmed one", len(detail.Transfers))
	}

	// A reorg deeper than the settle depth, of a transfer that is settled
	// for the watcher but not yet final for the core: the processed head
	// goes back, the transfer is retracted, and the payment can still be
	// paid.
	w.setConfirmations(20)
	q := w.env.CreatePayment("40")
	w.pay(q, 40)
	w.mine(1)
	w.mine(testSettle + 3)
	w.synced()
	if got := w.env.Payment(q.ID); got.Status != core.StatusConfirming || got.Confirmations != 4 {
		t.Fatalf("before deep reorg = %+v", got)
	}
	headBefore := w.coreHead()
	again := w.chain.Reorg(testSettle + 4)
	// While the new fork is shorter than what was already settled, there
	// is nothing to compare: the watcher waits (it cannot tell a shorter
	// fork from an endpoint that is behind) and tells the core nothing.
	w.mine(1)
	w.never("anything changes while the new fork is below the processed head", func() bool {
		return w.status(q) != core.StatusConfirming || w.coreHead() != headBefore
	})
	// The fork overtakes the old chain: the processed head goes back to
	// the fork point, the transfer is retracted.
	w.mine(testSettle + 6)
	w.waitStatus(q, core.StatusAwaiting)
	w.chain.Requeue(again...)
	w.mine(testSettle + 30)
	w.waitStatus(q, core.StatusPaid)
	if h := w.coreHead().Number; h <= headBefore.Number {
		t.Fatalf("core head = %d", h)
	}
	if st := w.watcherStatus(); st.Reorgs < 1 || st.DeepReorgs != 0 {
		t.Fatalf("watcher status = %+v", st)
	}
}

// TestPaidInTimeVerifiedLate is the seam bug this package exists to close.
//
// Two endpoints, default verification: a transfer needs a second opinion
// before it is reported. The second endpoint is down. The payer pays in
// time; the transfer is seen but cannot be delivered. Chain time passes the
// payment's deadline. If the head reported to the core ran ahead of the
// undelivered transfer, the core would expire the payment — terminally — and
// the money would arrive as "late funds" on an expired invoice.
func TestPaidInTimeVerifiedLate(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	other := w.env.CreatePayment("10") // nobody pays this one
	// The watcher takes the new addresses in — scanning again for them the
	// blocks above their start block, the head's final one — before the
	// second endpoint goes down. (Gone while it does, the head it reports is
	// the block it went back to.) A head above the one of the creation is
	// one of a tick that has done so.
	w.mine(1)
	w.synced()

	headBefore := w.coreHead().Number
	w.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
	// The payer pays one minute before the payment's TTL runs out.
	w.advance(p.ExpiresAt.Sub(w.env.Clock.Now()) - time.Minute)
	w.pay(p, 25)
	paidIn := w.mine(1).Number

	w.eventually("the watcher to see the unverified transfer", func() bool { return w.watcherStatus().Unverified == 1 })
	// The deadline passes — by an hour, on the chain's clock — while the
	// second endpoint is still down.
	w.advance(time.Hour)
	w.mine(testSettle + 10)
	w.never("the payment expires or the core's head passes the unverified transfer", func() bool {
		return w.status(p) != core.StatusAwaiting || w.coreHead().Number >= paidIn
	})
	// With the only other endpoint down nothing at all is settled: not the
	// transfer (nobody confirms its receipt) and no block either (nobody
	// confirms its header) — the head stays where two endpoints last agreed.
	if h := w.coreHead(); h.Number != headBefore {
		t.Fatalf("core head = %d, want it parked at %d, where both endpoints last agreed (the transfer is in block %d)", h.Number, headBefore, paidIn)
	}
	if hold := w.watcherStatus().Hold; !strings.Contains(hold, "no second endpoint") {
		t.Fatalf("hold = %q, want the status to say what the head is waiting for", hold)
	}
	// The unpaid payment is held open too: its fate is decided by the same
	// head. That is the price — slower, never wrong.
	if got := w.status(other); got != core.StatusAwaiting {
		t.Fatalf("unpaid payment = %s", got)
	}
	st := w.watcherStatus()
	if st.Unverified != 1 || st.Behind < testSettle+10 {
		t.Fatalf("watcher status = %+v", st)
	}

	// The second endpoint returns. The transfer is confirmed by it,
	// delivered, and the payment is paid — never expired, no late funds.
	w.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
	w.passCooldowns()
	got := w.waitStatus(p, core.StatusPaid)
	if got.NeedsAttention || got.AmountConfirmed != "25.00" {
		t.Fatalf("payment = %+v", got)
	}
	if ev := w.events(p); ev != "created confirming paid" && ev != "created paid" {
		t.Fatalf("events = %s", ev)
	}
	// And only now does the payment nobody paid expire.
	w.waitStatus(other, core.StatusExpired)
	w.synced()
}

// TestSecondEndpointDown: under the default verification mode with two
// endpoints, everything waits for the second one — and nothing is lost,
// reverted or wrongly settled meanwhile.
func TestSecondEndpointDown(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.start()
	w.synced()
	settled := w.env.CreatePayment("5")
	w.pay(settled, 5)
	w.mine(testSettle + testConfirmations + 1)
	w.waitStatus(settled, core.StatusPaid)

	w.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	w.mine(testSettle + testConfirmations + 5)
	w.eventually("unverified transfer", func() bool { return w.watcherStatus().Unverified == 1 })
	w.never("an unconfirmed observation reaches the core", func() bool { return w.status(p) != core.StatusAwaiting })

	// The dashboard shows which endpoint is the problem.
	w.eventually("node status to show the failing endpoint", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && len(ns.Endpoints) == 2 && ns.Endpoints[0].Healthy && ns.Endpoints[0].InUse &&
			!ns.Endpoints[1].Healthy && ns.Endpoints[1].Error != ""
	})
	// While the head is parked the instance falls behind the chain, and
	// once that exceeds the core's tolerance the node counts as unhealthy.
	w.mine(60)
	w.eventually("node status to turn unhealthy as the lag grows", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && !ns.Healthy && ns.LagBlocks > 50
	})

	w.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
	w.passCooldowns()
	w.waitStatus(p, core.StatusPaid)
	if got := w.status(settled); got != core.StatusPaid {
		t.Fatalf("earlier payment = %s", got)
	}
	// The receipt that released the observation came from the second
	// endpoint, not from the one that reported the log.
	if w.nodes[1].Calls("eth_getTransactionReceipt") == 0 {
		t.Fatal("the second endpoint was never asked")
	}
}

func TestRestartMidConfirmation(t *testing.T) {
	w := newWorld(t)
	w.setConfirmations(10)
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	w.mine(testSettle + 4)
	w.synced()
	if got := w.env.Payment(p.ID); got.Status != core.StatusConfirming || got.Confirmations != 4 {
		t.Fatalf("before restart = %+v", got)
	}

	// The process goes away; the chain does not wait.
	w.stop()
	w.mine(3)
	q := w.env.CreatePayment("7") // created while the watcher is down...
	w.pay(q, 7)                   // ...and paid while it is down
	w.mine(2)

	w.start()
	w.synced()
	// Five blocks on, the count continues where it was: 4 + 5.
	if got := w.env.Payment(p.ID); got.Status != core.StatusConfirming || got.Confirmations != 9 {
		t.Fatalf("after restart = %+v", got)
	}
	w.mine(testSettle + 10)
	got := w.waitStatus(p, core.StatusPaid)
	if got.AmountConfirmed != "25.00" {
		t.Fatalf("paid = %+v", got)
	}
	w.waitStatus(q, core.StatusPaid)
	if ev := w.events(p); ev != "created confirming paid" {
		t.Fatalf("events = %s (a restart must not replay or lose a transition)", ev)
	}
	if st := w.bridge.Status(); len(st.Alerts) != 0 {
		t.Fatalf("alerts after a clean restart: %+v", st.Alerts)
	}
}

func TestCheckpointLost(t *testing.T) {
	t.Run("core intact, chain unchanged", func(t *testing.T) {
		w := newWorld(t)
		w.setConfirmations(10)
		w.start()
		w.synced()
		p := w.env.CreatePayment("25")
		w.pay(p, 25)
		w.mine(testSettle + 3)
		w.synced()

		w.stop()
		w.store.set(nil) // the checkpoint table is gone
		w.mine(2)
		w.start()
		w.mine(testSettle + 10)
		got := w.waitStatus(p, core.StatusPaid)
		if got.AmountConfirmed != "25.00" || got.AmountReceived != "25.00" {
			t.Fatalf("paid = %+v (counted once)", got)
		}
		if ev := w.events(p); ev != "created confirming paid" {
			t.Fatalf("events = %s", ev)
		}
		if !strings.Contains(w.logs.String(), "has no checkpoint; starting from the core's state") {
			t.Error("resume from the core's state not logged")
		}
	})

	t.Run("core intact, pending transfer reorganised away during the outage", func(t *testing.T) {
		// The hard case. The core holds a pending transfer; the watcher
		// that reported it is gone together with its memory; the block
		// leaves the chain while nobody watches. A watcher starting "just
		// below the tip" would never look at that block again: the core
		// would count confirmations on a transfer that does not exist and
		// settle the payment as paid.
		w := newWorld(t)
		w.setConfirmations(10)
		w.start()
		w.synced()
		p := w.env.CreatePayment("25")
		w.pay(p, 25)
		w.mine(2)
		w.waitStatus(p, core.StatusConfirming)

		w.stop()
		w.store.set(nil)
		w.chain.Reorg(2) // the transfer's block is gone; the tx is not re-mined
		w.mine(testSettle + 30)
		w.start()
		w.waitStatus(p, core.StatusAwaiting)
		w.synced()
		if got := w.env.Payment(p.ID); got.Status != core.StatusAwaiting || got.AmountReceived != "0.00" {
			t.Fatalf("payment = %+v: a transfer that left the chain was confirmed", got)
		}
		if ev := w.events(p); ev != "created confirming awaiting" {
			t.Fatalf("events = %s", ev)
		}
		// The payer pays again; life goes on.
		w.pay(p, 25)
		w.mine(testSettle + 12)
		w.waitStatus(p, core.StatusPaid)
	})

	t.Run("checkpoint from an older backup", func(t *testing.T) {
		w := newWorld(t)
		w.start()
		w.synced()
		old := w.store.get()
		p := w.env.CreatePayment("25")
		w.pay(p, 25)
		w.mine(testSettle + testConfirmations + 2)
		w.waitStatus(p, core.StatusPaid)

		w.stop()
		w.store.set(old)
		w.start()
		q := w.env.CreatePayment("9")
		w.pay(q, 9)
		w.mine(testSettle + testConfirmations + 2)
		w.waitStatus(q, core.StatusPaid)
		// Replayed facts changed nothing.
		if got := w.env.Payment(p.ID); got.Status != core.StatusPaid || got.AmountConfirmed != "25.00" || got.NeedsAttention {
			t.Fatalf("payment = %+v", got)
		}
		if ev := w.events(p); ev != "created confirming paid" {
			t.Fatalf("events = %s", ev)
		}
	})

	t.Run("checkpoint ahead of the core (core restored from an older backup)", func(t *testing.T) {
		// The core's database is rolled back; the watcher's checkpoint is
		// not. Everything the watcher reported after the backup is unknown
		// to the core again, while the watcher believes it is done.
		w := newWorld(t)
		w.start()
		w.synced()
		p := w.env.CreatePayment("25")
		w.pay(p, 25)
		w.mine(2)
		w.waitStatus(p, core.StatusConfirming)
		w.synced() // the checkpoint's cursor is now two blocks past the "backup"
		w.stop()

		// "Restore": a second core on a fresh store, set up the same way,
		// with the same payment but none of the chain facts; the bridge
		// keeps its checkpoint.
		ahead := w.store.get()
		w2 := newWorld(t)
		w2.chain, w2.nodes, w2.urls = w.chain, w.nodes, w.urls
		if _, err := w2.env.Service.UpdateNetwork(w2.env.Operator(), core.UpdateNetworkInput{RPCURLs: w.urls}); err != nil {
			t.Fatal(err)
		}
		// The restored core's head is where the backup was taken.
		if err := w2.env.Service.AdvanceHead(w2.env.System(), core.ChainHead{
			ChainID: 56, Number: chainStart - testSettle, Hash: mustBlock(t, w.chain, chainStart-testSettle).Hash.String(), Time: w.env.Clock.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		p2 := w2.env.CreatePayment("25")
		if p2.Address != p.Address {
			t.Fatalf("test setup: restored payment has another address (%s vs %s)", p2.Address, p.Address)
		}
		w2.store.set(ahead)
		w2.start()
		w2.mine(testSettle + testConfirmations + 2)
		got := w2.waitStatus(p2, core.StatusPaid)
		if got.AmountConfirmed != "25.00" {
			t.Fatalf("payment = %+v", got)
		}
		if !strings.Contains(w2.logs.String(), "the core is behind the watcher's checkpoint") {
			t.Error("re-scan not logged")
		}
	})

	t.Run("unreadable checkpoint", func(t *testing.T) {
		w := newWorld(t)
		w.start()
		w.synced()
		p := w.env.CreatePayment("25")
		w.pay(p, 25)
		w.mine(1)
		w.waitStatus(p, core.StatusConfirming)
		w.stop()
		w.store.set([]byte(`{"v":1,"chain_id":56,"token":"0x55d3`)) // torn write
		w.start()
		w.mine(testSettle + testConfirmations + 2)
		w.waitStatus(p, core.StatusPaid)
		if st := w.bridge.Status(); st.Alerts[AlertCheckpointDiscarded] != 1 {
			t.Fatalf("alerts = %+v", st.Alerts)
		}
		select {
		case a := <-w.alerts:
			if a.Kind != AlertCheckpointDiscarded {
				t.Fatalf("alert = %+v", a)
			}
		default:
			t.Fatal("OnAlert not called")
		}
	})
}

func mustBlock(t *testing.T, c *evmtest.Chain, n uint64) evmtest.Block {
	t.Helper()
	b, ok := c.BlockByNumber(n)
	if !ok {
		t.Fatalf("no block %d", n)
	}
	return b
}

// TestFirstStartWithExistingPayments: the watcher has never run (the core has
// processed no head), but payments exist and one has been paid — created by
// an earlier version, which handed out addresses before the first head (this
// one does not: TestNoAddressBeforeTheFirstHead). Starting "just below the
// tip" would miss the payment and then expire it.
func TestFirstStartWithExistingPayments(t *testing.T) {
	w := newWorld(t)
	w.mine(5)
	p := w.legacyPayment("25")
	if cs, _ := w.env.Service.ChainState(w.env.System()); cs.Head.Number != 0 || cs.Addresses[0].SinceBlock != 0 {
		t.Fatalf("test setup: chain state = %+v", cs)
	}
	// Blocks are a second apart from here on, so that a block can be found
	// by its time.
	for i := 0; i < 20; i++ {
		w.advance(time.Second)
		w.mine(1)
	}
	w.pay(p, 25)
	paidIn := w.mine(1).Number
	for i := 0; i < 300; i++ { // long ago, by the standards of the settle depth
		w.advance(time.Second)
		w.mine(1)
	}
	// By now the deadline has not passed (5 minutes of a 30-minute TTL).

	w.start()
	got := w.waitStatus(p, core.StatusPaid)
	if got.AmountConfirmed != "25.00" || got.NeedsAttention {
		t.Fatalf("payment = %+v", got)
	}
	if !strings.Contains(w.logs.String(), "chain watcher starting without checkpoint") {
		t.Error("start point not logged")
	}
	// The scan started near the payment's creation, not at the beginning of
	// the chain and not at the tip.
	var lowest uint64 = 1 << 62
	for _, q := range w.nodes[0].LogQueries() {
		lowest = min(lowest, q.From)
	}
	if lowest > paidIn || lowest < chainStart-sinceMargin {
		t.Fatalf("scan started at block %d; the payment is in block %d", lowest, paidIn)
	}
}

func TestIdleUntilSetUp(t *testing.T) {
	w := newWorld(t, withoutSetup())
	w.start()
	w.eventually("bridge to idle", func() bool {
		st := w.bridge.Status()
		return st.State == StateIdle && st.Reason == "setup is not completed"
	})
	time.Sleep(50 * time.Millisecond)
	for _, n := range w.nodes {
		if n.Requests() != 0 {
			t.Fatal("RPC requests were made before setup")
		}
	}
	if ns, err := w.env.Service.NodeStatus(w.env.System()); err != nil || !ns.LastCheckedAt.IsZero() {
		t.Fatalf("node status before setup = %+v, %v", ns, err)
	}

	// The operator completes the wizard; the watcher starts by itself.
	in := w.env.SetupInput()
	in.RPCURLs, in.XPub = w.urls, testXpub
	res, err := w.env.Service.CompleteSetup(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	w.env.Token = res.Token
	w.setConfirmations(testConfirmations)
	w.eventually("bridge to run", func() bool { return w.bridge.Status().State == StateRunning })
	w.synced()
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	w.mine(testSettle + testConfirmations + 1)
	w.waitStatus(p, core.StatusPaid)
}

// lateTap stands in front of an endpoint that is about to be removed from the
// settings, and records what it is asked after the rebuild: whether the
// client still waited for the answer. A request of the replaced generation
// that was already on its way when the generation was stopped arrives with
// its client gone — the generation's context was cancelled, the connection
// closed — and is harmless. One that arrives with its client still waiting
// was started after the stop, by something of the old configuration still
// at work: that must never happen.
type lateTap struct {
	node *evmtest.Node

	mu      sync.Mutex
	rebuilt bool
	waited  []string // late requests whose client still waited: their bodies
	given   int      // late requests whose client had given up on them
}

// counts closes srv, the server in front of the tap — which waits until
// every request it has received has been answered and classified, and
// takes no more — and returns how many late requests came from a client
// that waited for the answer (their bodies) and how many from one that had
// given up.
func (l *lateTap) counts(srv *httptest.Server) (waited []string, given int) {
	srv.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.waited...), l.given
}

func (l *lateTap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	rebuilt := l.rebuilt
	l.mu.Unlock()
	if !rebuilt {
		l.node.ServeHTTP(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	l.node.ServeHTTP(w, r)
	// The answer is written; the request context ends when the client's
	// connection closes — at once, for a client that gave up.
	select {
	case <-r.Context().Done():
		l.mu.Lock()
		l.given++
		l.mu.Unlock()
	case <-time.After(time.Second):
		l.mu.Lock()
		l.waited = append(l.waited, string(body))
		l.mu.Unlock()
	}
}

func TestSettingsChangeRebuilds(t *testing.T) {
	t.Run("endpoints replaced", func(t *testing.T) {
		w := newWorld(t)
		tap := &lateTap{node: w.nodes[0]}
		srv := httptest.NewServer(tap)
		t.Cleanup(srv.Close)
		if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{RPCURLs: []string{srv.URL}}); err != nil {
			t.Fatal(err)
		}
		w.start()
		w.synced()
		p := w.env.CreatePayment("25")
		w.pay(p, 25)
		w.mine(1)
		w.waitStatus(p, core.StatusConfirming)

		// A second node; the operator replaces the endpoint list with it.
		second := evmtest.NewNode(w.chain)
		second.Set(func(b *evmtest.Behavior) { b.FinalityDepth = w.finality })
		url2 := second.Serve(t)
		if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{RPCURLs: []string{url2}}); err != nil {
			t.Fatal(err)
		}
		w.eventually("second generation on the new endpoint", func() bool {
			st := w.bridge.Status()
			return st.State == StateRunning && st.Generation == 2 && len(st.Endpoints) == 1 && st.Endpoints[0] == evm.RedactURL(url2)
		})
		// The first generation has stopped: everything it had started has
		// returned, its context is cancelled. From here on the removed
		// endpoint may only see requests that were on their way before.
		tap.mu.Lock()
		tap.rebuilt = true
		tap.mu.Unlock()
		w.mine(testSettle + testConfirmations + 1)
		w.waitStatus(p, core.StatusPaid)
		if ev := w.events(p); ev != "created confirming paid" {
			t.Fatalf("events = %s", ev)
		}
		time.Sleep(30 * time.Millisecond)
		w.stop() // and the second generation, so that nothing is pending
		waited, given := tap.counts(srv)
		if len(waited) > 0 {
			t.Fatalf("the removed endpoint was asked %d request(s) after the rebuild by a client that waited for the answer — started after the old generation stopped: %q", len(waited), waited)
		}
		if given > 0 {
			t.Logf("%d request(s) of the old generation, on their way when it stopped, arrived after the rebuild; their client had given up on them", given)
		}
		if second.Calls("eth_getLogs") == 0 {
			t.Fatal("the new endpoint is not used")
		}
		w.eventually("node status to list the new endpoint", func() bool {
			ns, err := w.env.Service.NodeStatus(w.env.Operator())
			return err == nil && len(ns.Endpoints) == 1 && ns.Endpoints[0].URL == evm.RedactURL(url2) && ns.Healthy
		})
	})

	t.Run("dead endpoints replaced by a working one", func(t *testing.T) {
		// The change that matters most: the watcher is failing before it
		// can even read the watch set, and the operator fixes the URL.
		//
		// It is also the case in which a watcher without a checkpoint must
		// not start "just below the tip": the payment below — one an
		// earlier version created before the first head — was paid and
		// buried far deeper than the settle depth while no endpoint could
		// be reached. The start position is chosen when the first tick
		// succeeds, from the payments that exist then.
		w := newWorld(t)
		w.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
		w.start()
		w.eventually("watcher to fail", func() bool { return w.watcherStatus().ConsecutiveErrors > 2 })
		w.advance(time.Minute)
		w.mine(3)
		p := w.legacyPayment("25")
		w.advance(time.Minute)
		w.pay(p, 25)
		w.mine(1)
		w.advance(time.Minute)
		w.mine(10 * testSettle)

		good := evmtest.NewNode(w.chain)
		good.Set(func(b *evmtest.Behavior) { b.FinalityDepth = w.finality })
		if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{RPCURLs: []string{good.Serve(t)}}); err != nil {
			t.Fatal(err)
		}
		w.mine(testSettle + testConfirmations + 1)
		got := w.waitStatus(p, core.StatusPaid)
		if got.NeedsAttention || got.AmountConfirmed != "25.00" {
			t.Fatalf("payment = %+v", got)
		}
		if g := w.bridge.Status().Generation; g != 2 {
			t.Fatalf("generation = %d", g)
		}
	})

	t.Run("confirmation setting changed", func(t *testing.T) {
		w := newWorld(t)
		w.start()
		w.synced()
		w.setConfirmations(7)
		w.eventually("rebuild", func() bool { return w.bridge.Status().Generation == 2 && w.bridge.Status().State == StateRunning })
		p := w.env.CreatePayment("25")
		w.pay(p, 25)
		w.mine(testSettle + 7)
		w.waitStatus(p, core.StatusPaid)
	})

	t.Run("only websocket endpoints", func(t *testing.T) {
		w := newWorld(t)
		w.start()
		w.synced()
		if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{RPCURLs: []string{"wss://bsc.example.org/ws/SECRETKEY"}}); err != nil {
			t.Fatal(err)
		}
		w.eventually("bridge to idle", func() bool {
			st := w.bridge.Status()
			return st.State == StateIdle && strings.Contains(st.Reason, "no usable RPC endpoint")
		})
		w.eventually("node status to explain", func() bool {
			ns, err := w.env.Service.NodeStatus(w.env.Operator())
			return err == nil && len(ns.Endpoints) == 1 && !ns.Endpoints[0].Healthy &&
				strings.Contains(ns.Endpoints[0].Error, "websocket") && !strings.Contains(ns.Endpoints[0].URL, "SECRETKEY")
		})
		// Mixed list: the websocket URL is reported as unusable, the http
		// one is used.
		if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{RPCURLs: []string{"wss://bsc.example.org/ws/SECRETKEY", w.urls[0]}}); err != nil {
			t.Fatal(err)
		}
		w.eventually("bridge to run again", func() bool { return w.bridge.Status().State == StateRunning })
		w.eventually("node status rows to line up with the settings", func() bool {
			ns, err := w.env.Service.NodeStatus(w.env.Operator())
			return err == nil && len(ns.Endpoints) == 2 && !ns.Endpoints[0].Healthy && ns.Endpoints[1].Healthy && ns.Endpoints[1].InUse
		})
	})

	t.Run("chain switched", func(t *testing.T) {
		// The core refuses while payments are open; once it allows it, the
		// old checkpoint must not be used for the new chain.
		w := newWorld(t)
		w.start()
		w.synced()
		p := w.env.CreatePayment("25")
		testnet := evmtest.NewChain(core.ChainBSCTestnet, 900)
		testToken, _ := parseAddress(core.USDTBSCTestnet.Contract)
		testnet.AddToken(testToken, "USDT", 18)
		testnet.Mine(100)
		tn := evmtest.NewNode(testnet)
		tnURL := tn.Serve(t)
		_, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{ChainID: core.ChainBSCTestnet, RPCURLs: []string{tnURL}})
		if err == nil {
			t.Fatal("the core switched chains with an open payment")
		}
		w.pay(p, 25)
		w.mine(testSettle + testConfirmations + 1)
		w.waitStatus(p, core.StatusPaid)

		if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{ChainID: core.ChainBSCTestnet, RPCURLs: []string{tnURL}}); err != nil {
			t.Fatal(err)
		}
		w.eventually("bridge to run on the testnet", func() bool {
			st := w.bridge.Status()
			return st.State == StateRunning && st.ChainID == core.ChainBSCTestnet && st.Token == testToken.String()
		})
		testnet.SetTime(w.env.Clock.Now())
		testnet.Mine(testSettle + 1)
		w.eventually("the core's first head of the testnet", func() bool {
			cs, err := w.env.Service.ChainState(w.env.System())
			return err == nil && cs.Head.ChainID == core.ChainBSCTestnet && cs.Head.Number != 0
		})
		q := w.env.CreatePayment("3")
		to, _ := parseAddress(q.Address)
		testnet.Transfer(testToken, payer, to, units(3))
		testnet.Mine(testSettle + testConfirmations + 1)
		w.waitStatus(q, core.StatusPaid)
		if h := w.coreHead(); h.ChainID != core.ChainBSCTestnet || h.Number > 1100 {
			t.Fatalf("core head = %+v", h)
		}
		if st := w.bridge.Status(); st.Alerts[AlertCheckpointDiscarded] != 1 {
			t.Fatalf("alerts = %+v (the mainnet checkpoint must have been discarded)", st.Alerts)
		}
		// Mainnet is not touched any more.
		before := w.nodes[0].Requests()
		time.Sleep(30 * time.Millisecond)
		if w.nodes[0].Requests() != before {
			t.Fatal("the mainnet endpoint is still queried")
		}
	})
}

// TestVerifyModes: the bridge's Verify option reaches the watcher.
func TestVerifyModes(t *testing.T) {
	t.Run("best effort keeps going on one endpoint", func(t *testing.T) {
		w := newWorld(t, withNodes(2), withOptions(func(o *Options) { o.Verify = watcher.VerifyBestEffort }))
		w.start()
		w.synced()
		w.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
		w.eventually("the second endpoint to be marked down", func() bool {
			ns, err := w.env.Service.NodeStatus(w.env.Operator())
			return err == nil && len(ns.Endpoints) == 2 && !ns.Endpoints[1].Healthy
		})
		p := w.env.CreatePayment("25")
		w.pay(p, 25)
		w.mine(testSettle + testConfirmations + 1)
		w.waitStatus(p, core.StatusPaid)
	})

	t.Run("strict with a single endpoint still runs, and says so", func(t *testing.T) {
		w := newWorld(t, withOptions(func(o *Options) { o.Verify = watcher.VerifyStrict }))
		w.start()
		w.synced()
		p := w.env.CreatePayment("25")
		w.pay(p, 25)
		w.mine(testSettle + testConfirmations + 1)
		w.waitStatus(p, core.StatusPaid)
		if !strings.Contains(w.logs.String(), "strict verification needs two RPC endpoints") {
			t.Error("the downgrade was not logged")
		}
	})

	t.Run("invalid options", func(t *testing.T) {
		w := newWorld(t)
		if _, err := New(nil, w.store, Options{}); err == nil {
			t.Error("nil service accepted")
		}
		if _, err := New(w.env.Service, nil, Options{}); err == nil {
			t.Error("nil store accepted")
		}
		if _, err := New(w.env.Service, w.store, Options{Verify: 99}); err == nil {
			t.Error("unknown verify mode accepted")
		}
		b, err := New(w.env.Service, w.store, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if b.Rescan(1) {
			t.Error("Rescan reported a running watcher")
		}
		if st := b.Status(); st.State != StateIdle {
			t.Errorf("status = %+v", st)
		}
	})
}

// TestLateTapTellsAbandonedFromWaiting: the instrument of the test above. A
// request whose client waits for the answer is recorded as waited for; one
// whose client gives up while it is on its way, as given up.
func TestLateTapTellsAbandonedFromWaiting(t *testing.T) {
	chain := evmtest.NewChain(core.ChainBSCMainnet, 100)
	node := evmtest.NewNode(chain)
	node.Set(func(b *evmtest.Behavior) { b.Delay = 300 * time.Millisecond })
	tap := &lateTap{node: node, rebuilt: true}
	srv := httptest.NewServer(tap)
	t.Cleanup(srv.Close)
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
	post := func(ctx context.Context) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	post(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	post(ctx) // gives up while the node is still answering
	cancel()
	if waited, given := tap.counts(srv); len(waited) != 1 || given != 1 {
		t.Fatalf("waited %d, given up %d; want one of each", len(waited), given)
	}
}
