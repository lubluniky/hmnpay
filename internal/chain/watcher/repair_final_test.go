package watcher_test

import (
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// The final block a watcher reports goes back in one case only, and only
// once: the watcher gives up blocks of its checkpoint that both endpoints'
// chains no longer reach (IncidentCheckpointAhead), and a finalized block
// among them — the endpoints are behind it for good, or the chain it was on
// is not there any more. Its own record of what is final goes with them; it
// reports no head until two endpoints attest a finalized block again, and
// that one is lower. From there on it never goes back again. (The consumer
// does not follow it down: TestTheCoresFinalBlockStaysWhenTheWatchersGoesBack
// in package corebridge.)
func TestTheFinalBlockGoesBackOnlyOnceWhenACheckpointAboveTheChainIsRepaired(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(2), withIncidents(&in))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)

	// The endpoints serve a chain 300 blocks longer, and finalize in it.
	long := h.chain.Clone()
	long.Mine(300)
	for _, n := range h.nodes {
		n.SetChain(long)
	}
	h.sink.set(func(s *sink) { s.headRule = nil })
	h.tick()
	h.tick()
	before := h.sink.lastHead()
	if before.Final <= h.chain.Head().Number {
		t.Fatalf("test setup: final block %d, the real chain at %d", before.Final, h.chain.Head().Number)
	}

	// Then they serve the real chain again, for good.
	for _, n := range h.nodes {
		n.SetChain(h.chain)
	}
	h.passCooldowns()
	h.client.Refresh(t.Context())
	h.sink.set(func(s *sink) { s.headRule = h.checkHeadRule })
	h.sink.take()
	h.restart()
	for i := 0; i < 2; i++ {
		if err := h.tickErr(); err == nil || !strings.Contains(err.Error(), "behind") {
			t.Fatalf("err = %v", err)
		}
	}
	h.tick() // the repair
	if in.count(watcher.IncidentCheckpointAhead) != 1 {
		t.Fatalf("incidents = %+v", in.list)
	}
	h.mineTicking(3 * testSettle)

	var finals []uint64
	for _, e := range h.sink.take() {
		if e.kind == "head" {
			finals = append(finals, e.head.Final)
		}
	}
	if len(finals) == 0 {
		t.Fatal("no head after the repair")
	}
	if finals[0] >= before.Final || finals[0] > h.chain.Head().Number {
		t.Fatalf("the first head after the repair is final at block %d; before, %d; the real chain at %d", finals[0], before.Final, h.chain.Head().Number)
	}
	for i := 1; i < len(finals); i++ {
		if finals[i] < finals[i-1] {
			t.Fatalf("after the repair the final block went back again: %v", finals)
		}
	}
}
