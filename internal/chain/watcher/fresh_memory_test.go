package watcher_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// A watcher that starts without its checkpoint, from the consumer's state, on
// endpoints that report no finalized block for the moment: the consumer hands
// it the block after its processed head and its final block, and lists the
// transfer it holds that is not final yet. Then a reorganisation a few blocks
// deeper than the settle depth — well inside the reorg window, above the final
// block — replaces the block the watcher settled on first, and with it the
// block of that transfer.
//
// The watcher used to remember only the block it settled on first; with that
// block gone, nothing it remembered was left on the chain, and it took an
// ordinary reorganisation for one that had replaced its whole window: after
// ConflictPatience ticks with one endpoint, at once with two, it raised a deep
// reorganisation — "contradicts finalized history" — and re-scanned a window
// below everything. It now remembers the block a window below, or the final
// block if that is higher, and the reorganisation is an ordinary one: the
// transfer is reverted, the head goes on, nothing deep is reported. A
// reorganisation that does replace the window still is.
func TestAWatcherStartedWithoutItsCheckpointTellsAnOrdinaryReorganisationFromADeepOne(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nodes int
		final uint64 // the consumer's final block, this far below its processed head (0: none)
		depth int    // of the reorganisation, below the settled block
		deep  bool
	}{
		{name: "final block a few blocks below, two endpoints", nodes: 2, final: 5, depth: 3},
		{name: "final block a few blocks below, one endpoint", nodes: 1, final: 5, depth: 3},
		{name: "the final block replaced", nodes: 2, final: 5, depth: 7, deep: true},
		{name: "final block more than a window below", nodes: 2, final: testWindow + 20, depth: 3},
		{name: "no final block from the consumer", nodes: 2, depth: 3},
		{name: "a window replaced", nodes: 2, final: testWindow + 30, depth: testWindow + 5, deep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withNodes(tc.nodes))
			h.watch.add(addr(1))
			h.mineTicking(testWindow + 40)
			h.pay(addr(1), 25)
			h.chain.Mine(1)
			h.mineTicking(testSettle + 1)
			var pending []watcher.Transfer
			for _, tr := range h.sink.liveKeys() {
				pending = append(pending, tr)
			}
			if len(pending) != 1 {
				t.Fatalf("test setup: %d transfers", len(pending))
			}
			for _, n := range h.nodes {
				n.Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
			}
			processed := h.w.Status().Cursor
			h.store.restore(nil)
			start := processed + 1
			h.cfg.StartBlock = &start
			if tc.final > 0 {
				h.cfg.FinalizedFloor = processed - tc.final
			}
			h.watch.setPending(pending...)
			h.restart()
			h.tick()
			settled := h.w.Status().Cursor
			if settled < pending[0].BlockNumber {
				t.Fatalf("test setup: settled on block %d, the transfer is in block %d", settled, pending[0].BlockNumber)
			}

			// The fork: everything above `settled - depth` replaced, the
			// transfer not mined again.
			tip := h.chain.Head().Number
			h.chain.Reorg(int(tip-settled) + tc.depth)
			h.chain.Mine(int(tip-h.chain.Head().Number) + 1)
			for range int(watcher.DefaultConflictPatience) + 2*testSettle {
				h.chain.Mine(1)
				_ = h.w.Tick(context.Background())
				if h.sink.count("reverted") > 0 {
					h.watch.setPending() // the consumer has it reverted
				}
			}
			st := h.w.Status()
			switch {
			case tc.deep && st.DeepReorgs != 1:
				t.Fatalf("a reorganisation that replaced the window: %d deep reorganisations reported", st.DeepReorgs)
			case !tc.deep && st.DeepReorgs != 0:
				t.Fatalf("an ordinary reorganisation %d blocks below the settled block %d taken for a deep one", tc.depth, settled)
			}
			if n := h.sink.count("reverted"); n != 1 {
				t.Fatalf("the transfer in a replaced block: %d reverts reported", n)
			}
			if head := h.chain.Head().Number; st.Cursor != head-testSettle {
				t.Fatalf("cursor %d, chain at %d", st.Cursor, head)
			}
		})
	}
}

// A first start, with the production settle depth and reorg window, on a
// chain whose finalized block is 40 below its tip and that is younger than
// the window: the endpoints cannot show the block a window below, so the
// watcher remembers the block it settles on, and the finalized block. A fork
// 20 blocks deep replaces the first and keeps the second: an ordinary
// reorganisation above a final block, inside the window — nothing deep is
// reported, and the head goes on. A fork that replaces the finalized block
// too is a contradiction of finalized history, and is reported as one.
func TestAShallowForkRightAfterTheStartIsAnOrdinaryReorganisation(t *testing.T) {
	for _, c := range []struct {
		name string
		fork int
		deep string
	}{
		{"the finalized block stands", 20, ""},
		{"the finalized block replaced", 45, "endpoints contradict finalized block"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var deep []watcher.DeepReorg
			h := newHarness(t, withNodes(2), withConfig(func(cfg *watcher.Config) {
				cfg.SettleDepth = watcher.DefaultSettleDepth
				cfg.ReorgWindow = watcher.DefaultReorgWindow
				cfg.OnDeepReorg = func(d watcher.DeepReorg) { deep = append(deep, d) }
			}))
			for _, n := range h.nodes {
				n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = 40 })
			}
			h.watch.addSince(addr(1), h.chain.Head().Number)
			h.tick()
			first := h.sink.lastHead()
			h.chain.Reorg(c.fork)
			h.chain.Mine(c.fork + 1)
			for range 3 {
				_ = h.w.Tick(context.Background())
			}
			switch {
			case c.deep == "" && len(deep) != 0:
				t.Fatalf("a %d-block fork above the finalized block %d reported as deep: %+v", c.fork, first.Finalized, deep)
			case c.deep != "" && (len(deep) != 1 || !strings.HasPrefix(deep[0].Reason, c.deep)):
				t.Fatalf("a fork replacing the finalized block %d: %+v", first.Finalized, deep)
			}
			if hd := h.sink.lastHead(); hd.Number != h.chain.Head().Number-watcher.DefaultSettleDepth {
				t.Fatalf("head %d, chain at %d", hd.Number, h.chain.Head().Number)
			}
		})
	}
}
