package corebridge

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// TestPaidWaitsForTheChainsFinality: production depths — the processed head
// 16 blocks behind the tip, 15 confirmations — and two honest endpoints that
// serve logs and report the block 40 below their tip as finalized. The payment
// has its confirmations 30 blocks after its block, and is not paid then: its
// block is above what the chain has finalised, and a reorganisation that keeps
// every finalised block could still take it. It stays confirming and says so;
// when finality stops moving while the chain goes on, finality_stalled is
// raised; when the chain finalises the block, it is paid — and only then is
// payment.paid sent.
func TestPaidWaitsForTheChainsFinality(t *testing.T) {
	w := newWorld(t, withNodes(2), withFinality(40), withOptions(func(o *Options) {
		o.SettleDepth = watcher.DefaultSettleDepth
		o.FinalityStallAfter = 50 * time.Millisecond
	}))
	w.setConfirmations(core.DefaultRequiredConfirmations)
	w.start()
	w.eventually("the first head", func() bool { return w.coreHead().Number == w.chain.Head().Number-w.opts.SettleDepth })
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	paid := w.mine(1)
	w.mine(int(w.opts.SettleDepth) + core.DefaultRequiredConfirmations)
	w.eventually("the payment has its confirmations", func() bool {
		got := w.env.Payment(p.ID)
		return got.Confirmations == core.DefaultRequiredConfirmations && got.AwaitingFinality != nil
	})
	got := w.env.Payment(p.ID)
	if got.Status != core.StatusConfirming || got.AmountConfirmed != "0.00" || got.AwaitingFinality.Block != paid.Number ||
		got.AwaitingFinality.Final >= paid.Number {
		t.Fatalf("confirmations reached, block %d above the finalised block: %s, confirmed %s, awaiting %+v",
			paid.Number, got.Status, got.AmountConfirmed, got.AwaitingFinality)
	}
	ns, err := w.env.Service.NodeStatus(w.env.Relogin())
	if err != nil {
		t.Fatal(err)
	}
	if fw := ns.AwaitingFinality; fw == nil || fw.Payments != 1 || fw.Block != paid.Number {
		t.Fatalf("node status: %+v", fw)
	}

	// Finality stalls: the chain moves on, the finalised block does not.
	stuck := w.chain.Head().Number - 40
	for range 5 {
		blk := w.mine(1)
		for _, n := range w.nodes {
			n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = blk.Number - stuck })
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for stalled := false; !stalled; {
		select {
		case a := <-w.alerts:
			stalled = a.Kind == AlertFinalityStalled
			if stalled && !strings.Contains(a.Message, "stay confirming") {
				t.Fatalf("alert: %+v", a)
			}
		case <-time.After(10 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatalf("no finality_stalled alert; alerts %v", w.bridge.Status().Alerts)
			}
			w.mine(1)
			for _, n := range w.nodes {
				n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = w.chain.Head().Number - stuck })
			}
		}
	}
	if s := w.status(p); s != core.StatusConfirming {
		t.Fatalf("while finality stalls: %s", s)
	}
	// The node status names the stall for as long as it lasts, not only in
	// the alert.
	stalledWarning := func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Relogin())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range ns.Warnings {
			if strings.HasPrefix(s, "finality stalled: ") && strings.Contains(s, "stay confirming") {
				return true
			}
		}
		return false
	}
	w.eventually("the stall in the node status", stalledWarning)

	// The chain finalises the block.
	for _, n := range w.nodes {
		n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = 2 })
	}
	w.mine(1)
	got = w.waitStatus(p, core.StatusPaid)
	if got.AmountConfirmed != "25.00" || got.AwaitingFinality != nil {
		t.Fatalf("finalised: %+v", got)
	}
	w.eventually("the stall to leave the node status", func() bool { return !stalledWarning() })
	if ev := w.events(p); ev != "created confirming paid" {
		t.Fatalf("events = %s", ev)
	}
	if h := w.coreHead(); h.Final < paid.Number {
		t.Fatalf("paid with the core's head final at %d, the transfer in block %d", h.Final, paid.Number)
	}
}

// TestFinalBlockSurvivesTheLossOfTheCheckpoint: two honest endpoints agreed on
// a final block, then both stopped reporting finalized blocks while the chain
// went on. Restarted with the checkpoint, and restarted without it, the final
// block is the one they agreed on — not one the depth of the chain would give
// (BSC has finality: no block is final for being deep): the watcher takes it
// from the core's head. A payment in a later block, with every confirmation,
// stays confirming and says why; when finality is reported again, it is paid.
func TestFinalBlockSurvivesTheLossOfTheCheckpoint(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.start()
	w.synced()
	prior := w.coreHead().Final
	for _, n := range w.nodes {
		n.Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
	}
	w.mine(270)
	w.synced()
	if h := w.coreHead(); h.Final != prior {
		t.Fatalf("endpoints stopped reporting finality: final %d, was %d", h.Final, prior)
	}
	w.stop()
	w.mine(1)
	w.start()
	w.synced()
	if h := w.coreHead(); h.Final != prior {
		t.Fatalf("restart with the checkpoint: final %d, was %d", h.Final, prior)
	}
	w.stop()
	w.store.set(nil)
	w.mine(1)
	w.start()
	w.synced()
	if h := w.coreHead(); h.Final != prior {
		t.Fatalf("restart without the checkpoint: head %d final %d, the endpoints last agreed on %d", h.Number, h.Final, prior)
	}
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	w.mine(1)
	w.mine(testSettle + testConfirmations + 20)
	w.synced()
	got := w.env.Payment(p.ID)
	if got.Status != core.StatusConfirming || got.AwaitingFinality == nil || got.AwaitingFinality.Final != prior {
		t.Fatalf("payment above the last agreed final block: %s, awaiting %+v", got.Status, got.AwaitingFinality)
	}
	ns, err := w.env.Service.NodeStatus(w.env.Relogin())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(ns.Warnings, func(s string) bool { return strings.Contains(s, "finality dispute") }) {
		t.Fatalf("node status warnings: %q", ns.Warnings)
	}
	for _, n := range w.nodes {
		n.Set(func(b *evmtest.Behavior) { b.NoFinalized = false })
	}
	w.passCooldowns()
	w.mine(2)
	w.waitStatus(p, core.StatusPaid)
}

// TestNothingIsFinalBeforeTwoEndpointsAttestIt: a first start on endpoints
// that report no finalized block. Nothing is final: no head is reported, no
// payment can be created (chain_not_ready), the node status holds and says
// why, and finality_stalled is raised while the chain moves on — before there
// is any processed head, whatever the scan does. When they report finality,
// the first head follows.
func TestNothingIsFinalBeforeTwoEndpointsAttestIt(t *testing.T) {
	w := newWorld(t, withNodes(2), withOptions(func(o *Options) { o.FinalityStallAfter = 50 * time.Millisecond }))
	for _, n := range w.nodes {
		n.Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
	}
	w.start()
	w.eventually("the hold", func() bool { return strings.Contains(w.watcherStatus().Hold, "agree on a finalized block") })
	deadline := time.Now().Add(15 * time.Second)
	for stalled := false; !stalled; {
		select {
		case a := <-w.alerts:
			stalled = a.Kind == AlertFinalityStalled
		case <-time.After(10 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatalf("no finality_stalled alert; alerts %v", w.bridge.Status().Alerts)
			}
			w.mine(1)
		}
	}
	if h := w.coreHead(); h.Number != 0 {
		t.Fatalf("a head without an attested final block: %+v", h)
	}
	if _, _, err := w.env.Service.CreatePayment(w.env.System(), core.CreatePaymentInput{Amount: "25"}); !errors.Is(err, core.ErrChainNotReady) {
		t.Fatalf("payment before the first head: %v", err)
	}
	for _, n := range w.nodes {
		n.Set(func(b *evmtest.Behavior) { b.NoFinalized = false })
	}
	w.passCooldowns()
	w.mine(1)
	w.synced()
}

// TestStartupFinalityDisputeIsSettledByAThirdEndpoint: of three endpoints,
// the one in use reports no finalized block. The other two attest the same
// one: that is final, from the first tick, and the dissenter is named.
func TestStartupFinalityDisputeIsSettledByAThirdEndpoint(t *testing.T) {
	w := newWorld(t, withNodes(3))
	w.nodes[0].Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
	w.start()
	w.synced()
	if d := w.watcherStatus().FinalityDispute; !strings.Contains(d, "two of three") {
		t.Fatalf("dispute %q", d)
	}
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	w.mine(1)
	w.mine(testSettle + testConfirmations + 1)
	w.waitStatus(p, core.StatusPaid)
}

// TestNoAddressRetiresAboveTheFinalizedChain: a payment expires unpaid; its
// window ends; the endpoints stop reporting finality, so the final block stays
// where it was when the payment was created, while the processed head and the
// clocks go far past the window. The address stays watched: a reorganisation
// that keeps every finalised block can still replace everything above the
// final block — and does, with money to the address in a block stamped
// inside the window. It is found.
func TestNoAddressRetiresAboveTheFinalizedChain(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	if _, err := w.env.Service.CancelPayment(w.env.Operator(), p.ID); err != nil {
		t.Fatal(err)
	}
	last := w.coreHead().Final
	for _, n := range w.nodes {
		n.Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
	}
	w.advance(core.DefaultPaymentTTL + core.DefaultExpiryGrace + core.DefaultUnfundedWatchWindow + core.WatchRetirementMargin + time.Hour)
	w.mine(testSettle + 2)
	w.synced()
	if !w.listed(p) || w.coreHead().Final != last {
		t.Fatalf("window over by the head and the clock, the final block %d (was %d) before it: listed %v", w.coreHead().Final, last, w.listed(p))
	}
	// The fork: everything above the final block replaced, the first block
	// of the new branch stamped inside the window, with the money.
	base := mustBlock(t, w.chain, last)
	dropped := w.chain.Reorg(int(w.chain.Head().Number - last))
	_ = dropped
	w.chain.SetTime(time.Unix(int64(base.Time)+1, 0))
	w.pay(p, 1)
	w.chain.Mine(1)
	w.mine(testSettle + 20)
	w.eventually("the money in the window found", func() bool { return w.env.Payment(p.ID).AmountReceived == "1.00" })
}

// TestTheCoresFinalBlockStaysWhenTheWatchersGoesBack: for a while both
// endpoints serve a chain 300 blocks longer, and attest finalized blocks in
// it; then they serve the real chain again. The watcher repairs its
// checkpoint (checkpoint_repaired): it gives up the blocks neither chain
// reaches and the finalized block among them, and from then on reports the
// lower final block the endpoints attest on the real chain. The core keeps
// the one two endpoints attested: its processed head goes back with the
// watcher's, its final block does not — it stays above the processed head
// (head.final) until the chain grows past it. It cannot tell, any more than
// the watcher can, endpoints that are behind for a while from a chain that is
// not there any more; what was attested stays final, and what was paid on it
// stays paid.
func TestTheCoresFinalBlockStaysWhenTheWatchersGoesBack(t *testing.T) {
	w := newWorld(t, withNodes(2))
	long := w.chain.Clone()
	long.Mine(300)
	for _, n := range w.nodes {
		n.SetChain(long)
	}
	w.start()
	w.eventually("a final block on the longer chain", func() bool { return w.coreHead().Final > w.chain.Head().Number+200 })
	final := w.coreHead().Final
	w.stop()

	for _, n := range w.nodes {
		n.SetChain(w.chain)
	}
	w.start()
	w.mine(1)
	w.eventually("the checkpoint to be repaired", func() bool { return w.bridge.Status().Alerts[AlertCheckpointRepaired] > 0 })
	w.mine(testSettle + 2)
	w.synced()
	if h := w.coreHead(); h.Final != final || h.Number >= final {
		t.Fatalf("after the repair: head %d final %d; before it, final %d", h.Number, h.Final, final)
	}
}

// TestPaidAtTheHighestBlockTwoEndpointsFinalize: three endpoints serve the
// chain; the one in use finalizes a block behind the other two, by a block.
// The invoice's block is finalized by the other two: it is final — the
// highest block two configured endpoints attest — and paid with its
// confirmations, not a block later; the endpoint that is behind is named
// while it costs that block. With two endpoints the lower one's finalized
// block is the highest two attest: the payment waits for it, and the node
// status says which endpoint it waits for.
func TestPaidAtTheHighestBlockTwoEndpointsFinalize(t *testing.T) {
	for _, nodes := range []int{3, 2} {
		t.Run(fmt.Sprintf("%d endpoints", nodes), func(t *testing.T) {
			w := newWorld(t, withNodes(nodes))
			for i, d := range []uint64{12, 11, 11}[:nodes] {
				w.nodes[i].Set(func(b *evmtest.Behavior) { b.FinalityDepth = d })
			}
			w.start()
			w.synced()
			p := w.env.CreatePayment("25")
			w.pay(p, 25)
			paid := w.mine(1)
			w.mine(11) // the block is now finalized by the endpoints 11 blocks behind
			w.synced()
			got := w.env.Payment(p.ID)
			dispute := w.watcherStatus().FinalityDispute
			if nodes == 3 {
				if got.Status != core.StatusPaid || w.coreHead().Final < paid.Number {
					t.Fatalf("two of three endpoints finalize block %d: %s, final %d, dispute %q", paid.Number, got.Status, w.coreHead().Final, dispute)
				}
				if !strings.Contains(dispute, "two of three") || !strings.Contains(dispute, evm.RedactURL(w.urls[0])+" dissents") {
					t.Fatalf("dispute %q", dispute)
				}
				return
			}
			if got.Status != core.StatusConfirming || got.AwaitingFinality == nil {
				t.Fatalf("two endpoints, one finalizing a block behind: %s, awaiting %+v", got.Status, got.AwaitingFinality)
			}
			if !strings.Contains(dispute, fmt.Sprintf("the transfer in block %d waits for %s", paid.Number, evm.RedactURL(w.urls[0]))) {
				t.Fatalf("dispute %q", dispute)
			}
			w.mine(1)
			w.waitStatus(p, core.StatusPaid)
		})
	}
}
