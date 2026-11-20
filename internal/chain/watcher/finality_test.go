package watcher_test

import (
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// finalityHarness is a pool of two endpoints whose finalized blocks trail
// the tip by depths[0] and depths[1] (-1: the endpoint reports none).
func finalityHarness(t *testing.T, depths ...int) *harness {
	t.Helper()
	h := newHarness(t, withNodes(2))
	for i, d := range depths {
		h.nodes[i].Set(func(b *evmtest.Behavior) {
			if d < 0 {
				b.NoFinalized = true
			} else {
				b.FinalityDepth = uint64(d)
			}
		})
	}
	h.watch.addSince(addr(1), h.chain.Head().Number)
	return h
}

// TestFinalizedBlockIsTheLowerOfTwo: two endpoints whose finality runs at
// different depths. The finalized block the watcher goes by — and Head.Final,
// what a payment is final at — is the lower of the two, whichever of them is
// the primary: one endpoint that says a block is finalized is not enough.
// The two being further apart than the settle depth is named.
func TestFinalizedBlockIsTheLowerOfTwo(t *testing.T) {
	for _, depths := range [][]int{{3, 30}, {30, 3}} {
		h := finalityHarness(t, depths...)
		h.mineTicking(testWindow)
		hd := h.sink.lastHead()
		tip := h.chain.Head().Number
		// The lower endpoint's finalized block when the tick read it.
		if hd.Finalized == 0 || hd.Finalized > tip-30 || hd.Finalized+3 < tip-30 {
			t.Fatalf("depths %v: finalized %d with the tip at %d; want the lower of the two (about %d)", depths, hd.Finalized, tip, tip-30)
		}
		if hd.Final != min(hd.Number, hd.Finalized) {
			t.Fatalf("depths %v: head %+v", depths, hd)
		}
		if d := h.w.Status().FinalityDispute; !strings.Contains(d, "payments wait for") {
			t.Fatalf("depths %v: endpoints 27 blocks apart in their finality, and the dispute is %q", depths, d)
		}
	}
	// Close together, as two honest BSC endpoints are: nothing to say.
	h := finalityHarness(t, 2, 3)
	h.mineTicking(testWindow)
	if d := h.w.Status().FinalityDispute; d != "" {
		t.Fatalf("finality a block apart: dispute %q", d)
	}
	if hd, tip := h.sink.lastHead(), h.chain.Head().Number; hd.Final != hd.Number || hd.Finalized < tip-3 {
		t.Fatalf("finality ahead of the processed head: head %+v, tip %d", hd, tip)
	}
}

// TestNoHeadWhileFinalityIsUnknown: one endpoint reports a finalized block,
// the other none. Nothing is final on one endpoint's word, so the watcher,
// which has never known a finalized block, reports no head at all (a head
// says what is final), holds, and says why; the moment both report one, the
// head follows, final at the lower of the two.
func TestNoHeadWhileFinalityIsUnknown(t *testing.T) {
	h := finalityHarness(t, 2, -1)
	h.mineTicking(testSettle + 5)
	if n := h.sink.count("head"); n != 0 {
		t.Fatalf("%d head(s) reported with one endpoint reporting no finalized block: %+v", n, h.sink.lastHead())
	}
	st := h.w.Status()
	if !strings.Contains(st.Hold, "agree on a finalized block") || !strings.Contains(st.FinalityDispute, "reports no finalized block") {
		t.Fatalf("hold %q, dispute %q", st.Hold, st.FinalityDispute)
	}
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.NoFinalized, b.FinalityDepth = false, 5 })
	h.passCooldowns() // the endpoint is asked for the tag again
	h.mineTicking(2)
	hd := h.sink.lastHead()
	if tip := h.chain.Head().Number; hd.Number == 0 || hd.Finalized > tip-5 || hd.Final != min(hd.Number, hd.Finalized) {
		t.Fatalf("both endpoints report finality: head %+v, tip %d", hd, tip)
	}
	if st := h.w.Status(); st.FinalityDispute != "" || st.Hold != "" {
		t.Fatalf("after agreement: hold %q, dispute %q", st.Hold, st.FinalityDispute)
	}
}

// TestFinalityStopsWhereAgreementEnds: a finalized block the two agreed on
// stays what is final while one of them stops reporting finality — it does
// not move up on the other's word, and the head keeps being reported (the
// final block is known) with the dispute named.
func TestFinalityStopsWhereAgreementEnds(t *testing.T) {
	h := finalityHarness(t, 2, 2)
	h.mineTicking(testWindow)
	agreed := h.sink.lastHead().Finalized
	if agreed == 0 {
		t.Fatal("test setup: no finalized block")
	}
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
	h.mineTicking(testWindow)
	hd := h.sink.lastHead()
	if hd.Finalized != agreed || hd.Final != agreed || hd.Number <= agreed {
		t.Fatalf("finality reported by neither any more: head %+v, the agreed finalized block %d", hd, agreed)
	}
	if d := h.w.Status().FinalityDispute; !strings.Contains(d, "report no finalized block") {
		t.Fatalf("dispute %q", d)
	}
}

// TestEndpointsWithoutFinalityMakeNothingFinal: on a network with finality,
// two endpoints that report no finalized block make no block final, however
// deep: no head is reported, and the hold and the dispute say why. Only a
// network declared to have no finality (Config.NoFinality) is final a
// reorganisation window deep.
func TestEndpointsWithoutFinalityMakeNothingFinal(t *testing.T) {
	h := finalityHarness(t, -1, -1)
	h.mineTicking(testWindow + 10)
	if n := h.sink.count("head"); n != 0 {
		t.Fatalf("%d head(s) reported on a network with finality whose endpoints report none: %+v", n, h.sink.lastHead())
	}
	if st := h.w.Status(); !strings.Contains(st.Hold, "agree on a finalized block") || !strings.Contains(st.FinalityDispute, "report no finalized block") {
		t.Fatalf("hold %q, dispute %q", st.Hold, st.FinalityDispute)
	}
	n := newHarness(t, withNodes(2), withConfig(func(c *watcher.Config) { c.NoFinality = true }))
	n.watch.addSince(addr(1), n.chain.Head().Number)
	n.mineTicking(testWindow + 10)
	if hd := n.sink.lastHead(); hd.Number == 0 || hd.Final != hd.Number-testWindow {
		t.Fatalf("declared without finality: head %+v (window %d)", hd, testWindow)
	}
}

// TestFinalityFloorSurvivesTheCheckpoint: two endpoints agreed on a
// finalized block, then stopped reporting finality; the watcher loses its
// checkpoint. Started again with the consumer's final block as its floor, it
// takes that block once both endpoints show it, reports heads final there —
// never higher, however deep the chain grows, and never lower — and names
// the wait. Without a floor (a consumer that holds nothing final) it reports
// no head at all.
func TestFinalityFloorSurvivesTheCheckpoint(t *testing.T) {
	h := finalityHarness(t, 3, 3)
	h.mineTicking(testWindow)
	agreed := h.sink.lastHead().Final
	if agreed == 0 {
		t.Fatal("test setup: no final block")
	}
	for _, n := range h.nodes {
		n.Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
	}
	h.passCooldowns()
	h.mineTicking(testWindow + 20)
	h.store.restore(nil)
	h.cfg.FinalizedFloor = agreed
	h.restart()
	before := h.sink.count("head")
	h.mineTicking(testWindow + 20)
	hd := h.sink.lastHead()
	if h.sink.count("head") == before || hd.Final != agreed || hd.Finalized != agreed || hd.Number <= agreed+testWindow {
		t.Fatalf("restarted without checkpoint, floor %d: head %+v", agreed, hd)
	}
	if d := h.w.Status().FinalityDispute; !strings.Contains(d, "report no finalized block") {
		t.Fatalf("dispute %q", d)
	}
	// No floor: nothing is final.
	h.store.restore(nil)
	h.cfg.FinalizedFloor = 0
	h.restart()
	before = h.sink.count("head")
	h.mineTicking(10)
	if n := h.sink.count("head"); n != before {
		t.Fatalf("no checkpoint, no floor, no finality reported: %d head(s): %+v", n-before, h.sink.lastHead())
	}
}

// TestThirdEndpointSettlesAFinalityDispute: of three endpoints, the primary
// and the verifier disagree about finality — one reports none — and the
// third, asked, attests the same finalized block as the one that does: that
// block is final, the dissenter is named, and nobody waits.
func TestThirdEndpointSettlesAFinalityDispute(t *testing.T) {
	h := newHarness(t, withNodes(3))
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.mineTicking(testWindow)
	hd := h.sink.lastHead()
	if hd.Number == 0 || hd.Final != hd.Number {
		t.Fatalf("two of three report finality two blocks behind the tip: head %+v", hd)
	}
	st := h.w.Status()
	if !strings.Contains(st.FinalityDispute, "two of three") || !strings.Contains(st.FinalityDispute, h.urls[0][7:]) || st.Hold != "" {
		t.Fatalf("primary %s, verifier %s: dispute %q, hold %q", st.Endpoint, st.Verifier, st.FinalityDispute, st.Hold)
	}
}
