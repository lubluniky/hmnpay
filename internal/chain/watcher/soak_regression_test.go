package watcher_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// Regression tests for histories the randomised model found. Each one is the
// sequence of endpoint behaviours that broke an invariant, written out by
// hand: none of them depends on the random generator.

// TestSecondLogScanKeepsTheSettleMargin is seed 36780 of the randomised model
// (40 000-seed soak on fc290c4), which ended with
//
//	HEAD RULE: head 10083 reported, but the transfer … in block 10083 … was never delivered
//
// Two endpoints. The primary hides transfers from its logs (the malicious
// one). The other is honest but load-balanced: requests containing
// eth_getLogs go to a backend a few blocks behind — which the block number in
// the same batch gives away — and that backend's log index trails its own
// head by a few blocks more, silently. Neither lag reaches the settle depth,
// which exists precisely to absorb a log index that trails its node; the sum
// does. The watcher measured the margin for its own scan from the height of
// the backend that answered it, but accepted the second endpoint's scan for
// any block that backend had merely reached: the block was settled on logs
// that did not contain it yet, and the payment the primary hid was passed by
// the head.
func TestSecondLogScanKeepsTheSettleMargin(t *testing.T) {
	hideAll := func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } }
	const backendLag, indexLag = 3, 4 // each below testSettle, together above

	for _, tc := range []struct {
		name string
		// catchUp: the paid block is a whole reorg window below the head
		// when it is settled (advanceBelowWindow), not at settle depth.
		catchUp bool
	}{
		{name: "at settle depth"},
		{name: "during catch-up", catchUp: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in incidents
			h := newHarness(t, withNodes(2), withIncidents(&in))
			h.watch.add(addr(1), addr(2))
			h.mineTicking(testSettle + 2)

			h.nodes[0].Set(hideAll)
			h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsBackendLag, b.LogsLag = backendLag, indexLag })
			h.pay(addr(1), 10)
			h.pay(addr(2), 20)
			paid := h.chain.Mine(1).Number

			// The head rule is checked by the harness on every head the
			// watcher reports; here it is spelled out as well.
			passed := func(when string) {
				t.Helper()
				if head := h.sink.lastHead().Number; head >= paid && len(h.sink.liveKeys()) != 2 {
					t.Fatalf("%s: head %d reported, block %d holds 2 payments and %d were delivered",
						when, head, paid, len(h.sink.liveKeys()))
				}
			}
			if tc.catchUp {
				h.chain.Mine(testWindow + testSettle)
				_ = h.w.Tick(context.Background())
				passed("catching up")
			}
			// Block by block, until the second endpoint's log index has
			// the paid block for certain.
			for h.chain.Head().Number < paid+backendLag+indexLag+testSettle {
				h.chain.Mine(1)
				_ = h.w.Tick(context.Background())
				passed("head at " + hexUint(h.chain.Head().Number))
				h.passCooldowns()
			}
			if n := len(h.sink.liveKeys()); n != 2 {
				t.Fatalf("%d transfers observed, want the 2 the primary hid; status %+v", n, h.w.Status())
			}
			if blamed := in.blamed(watcher.IncidentLogOmission); len(blamed) == 0 || blamed[0] != evm.RedactURL(h.urls[0]) {
				t.Fatalf("the hiding primary was not named: incidents = %+v", in.list)
			}

			// The lag stays, the liar stops: the head follows the slower
			// log backend at the settle depth, without holding and without
			// settling anything on one endpoint's logs.
			h.nodes[0].Set(honest)
			h.passCooldowns()
			h.pay(addr(1), 30)
			h.mineTicking(2 * testSettle)
			h.assertModel(startBlock)
			st := h.w.Status()
			if want := h.chain.Head().Number - backendLag - testSettle; st.Cursor != want {
				t.Fatalf("cursor = %d, want %d: the settle depth below the second endpoint's log backend; status %+v", st.Cursor, want, st)
			}
			if st.LogsUnchecked || st.UncheckedFrom != 0 {
				t.Fatalf("blocks settled on one endpoint's logs: %+v", st)
			}
		})
	}

	t.Run("a log backend too far behind holds the head, and says so", func(t *testing.T) {
		// The second endpoint's log backend has not come within the settle
		// depth of the blocks to settle and does not move: nothing is
		// settled, and the hold names it.
		var in incidents
		h := newHarness(t, withNodes(2), withIncidents(&in))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		cursor := h.w.Status().Cursor
		frozen := h.chain.Clone()
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsBackendLag = 1 })
		h.pay(addr(1), 10)
		h.chain.Mine(1)
		for i := 0; i < 6; i++ {
			h.chain.Mine(2)
			h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsBackendLag = h.chain.Head().Number - frozen.Head().Number })
			h.tick()
		}
		st := h.w.Status()
		if st.Cursor != cursor || !strings.Contains(st.Hold, "log verification unavailable") || !strings.Contains(st.Hold, "settle depth") {
			t.Fatalf("cursor %d -> %d, hold %q", cursor, st.Cursor, st.Hold)
		}
		if in.count(watcher.IncidentHeadHeld) == 0 {
			t.Fatalf("the hold was not alerted: %+v", in.list)
		}
		if n := len(h.sink.liveKeys()); n != 1 {
			t.Fatalf("%d transfers observed, want 1 (reported at once, on the primary's logs and the other's receipt)", n)
		}
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsBackendLag = 0 })
		h.tick()
		if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle || st.Hold != "" {
			t.Fatalf("status after the backend caught up: %+v", st)
		}
		h.assertModel(startBlock)
	})
}

// oldHeadersUpTo makes a node keep a reorganisation quiet where it matters:
// asked by number for a block at or below top, it returns the header that
// block had on the chain `old`; everything else it answers truthfully.
func oldHeadersUpTo(old *evmtest.Chain, top uint64) func(b *evmtest.Behavior) {
	return func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			_, n, numeric := blockParam(params)
			if blk, ok := old.BlockByNumber(n); numeric && ok && n <= top {
				return headerJSON(blk, blk.Time), nil, true
			}
			return nil, nil, false
		}
	}
}

// TestHeadNeedsAgreementInTheTickThatReportsIt is seed 1351 (and 25 more) of
// the 40 000-seed soak on 59cae92:
//
//	HEAD RULE: processed head 10139 (0xd736…) is not a block of the canonical chain
//
// The cursor is settled on a block the two endpoints agree on, and saved; the
// sink is down, so the head is not delivered. Then that block is reorganised
// away, and the primary — the malicious one — keeps returning its old header.
// The verifier contradicts it and is set aside for the tick (nobody can tell
// which of two is right), which is meant to mean that nothing is settled or
// reported. But the head still owed to the sink was taken from the agreement
// of the earlier tick and reported: a block that is not on the chain, on what
// had become one endpoint's word.
func TestHeadNeedsAgreementInTheTickThatReportsIt(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(2), withIncidents(&in), withConfig(func(c *watcher.Config) { c.NoFinality = true }))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)
	delivered := h.sink.lastHead()

	h.chain.Mine(3)
	h.sink.set(func(s *sink) { s.failHead = 1 })
	if err := h.tickErr(); !strings.Contains(err.Error(), "sink rejected head") {
		t.Fatalf("tick: %v", err)
	}
	cursor := h.w.Status().Cursor
	if cursor != delivered.Number+3 || h.sink.lastHead() != delivered {
		t.Fatalf("test setup: cursor %d, delivered head %d", cursor, h.sink.lastHead().Number)
	}

	old := h.chain.Clone()
	h.chain.Reorg(testSettle + 2) // replaces the cursor's block
	h.pay(addr(1), 10)
	h.chain.Mine(testSettle + 4)
	if now, _ := h.chain.BlockByNumber(cursor); now.Hash == mustBlock(t, old, cursor).Hash {
		t.Fatal("test setup: the reorg did not replace the cursor's block")
	}
	h.nodes[0].Set(oldHeadersUpTo(old, cursor))
	for i := 0; i < 5; i++ {
		_ = h.w.Tick(context.Background())
		// (That a reported head is a block of the chain is checked by the
		// harness on every head.)
		if got := h.sink.lastHead(); got != delivered {
			t.Fatalf("tick %d: head %d (%s) reported while the endpoints disagree about the settled blocks", i, got.Number, got.Hash)
		}
		h.passCooldowns()
	}
	if st := h.w.Status(); st.Hold == "" || in.count(watcher.IncidentHeadHeld) == 0 {
		t.Fatalf("the hold is not named or not alerted: hold %q, incidents %+v", st.Hold, in.list)
	}

	// The endpoint stops lying: the reorganisation is seen for what it is.
	h.nodes[0].Set(honest)
	h.passCooldowns()
	h.mineTicking(2)
	h.assertModel(startBlock)
	if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle || h.sink.lastHead().Number != st.Cursor {
		t.Fatalf("after the lie: cursor %d, sink head %d, chain head %d", st.Cursor, h.sink.lastHead().Number, h.chain.Head().Number)
	}
}

// TestIdleTickAsksForNothingMore: agreeing on the cursor's block again is
// the price of reporting a head that an earlier tick settled. A tick that has
// no head to report — the chain has not moved, the sink has the head — pays
// nothing for it: three requests, as before (the primary's head and its scan
// of the blocks above the cursor, the verifier's head).
func TestIdleTickAsksForNothingMore(t *testing.T) {
	h := newHarness(t, withNodes(2), withConfig(func(c *watcher.Config) { c.NoFinality = true }))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)
	head := h.sink.lastHead()
	for _, n := range h.nodes {
		n.ResetCounters()
	}
	const ticks = 10
	for i := 0; i < ticks; i++ {
		h.tick()
	}
	if got := h.nodes[0].Requests() + h.nodes[1].Requests(); got != 3*ticks {
		t.Fatalf("%d HTTP requests in %d ticks without a new block, want %d (endpoint 0: %d, endpoint 1: %d)",
			got, ticks, 3*ticks, h.nodes[0].Requests(), h.nodes[1].Requests())
	}
	if h.sink.lastHead() != head {
		t.Fatalf("head changed without a new block: %+v", h.sink.lastHead())
	}
}

func mustBlock(t *testing.T, c *evmtest.Chain, n uint64) evmtest.Block {
	t.Helper()
	blk, ok := c.BlockByNumber(n)
	if !ok {
		t.Fatalf("no block %d", n)
	}
	return blk
}

// TestReorgDepthNeedsTheVerifier is seeds 350, 4549, 8171 and 23700 of the
// same soak:
//
//	HEAD RULE: head 10141 reported, but the transfer … in block 10139 … was never delivered
//
// A reorganisation replaces the block the cursor rests on and blocks below
// it. The primary — the malicious one — admits to the reorganisation but not
// to its depth: for the cursor's block it returns the new header, for the
// ones below the old ones. The watcher asked the verifier about the newest
// block it remembers only; where the reorganisation ends it took from the
// primary. So it went back one block instead of two, scanned from there, and
// the payment mined into the replaced block below was passed by the head.
func TestReorgDepthNeedsTheVerifier(t *testing.T) {
	for _, nodes := range []int{2, 3} {
		var in incidents
		h := newHarness(t, withNodes(nodes), withIncidents(&in), withConfig(func(c *watcher.Config) { c.NoFinality = true }))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 4)
		cursor := h.w.Status().Cursor

		old := h.chain.Clone()
		h.chain.Reorg(testSettle + 2) // replaces the cursor's block and the one below it
		h.pay(addr(1), 10)
		paid := h.chain.Mine(1)
		if paid.Number != cursor-1 {
			t.Fatalf("test setup: payment in block %d, cursor %d", paid.Number, cursor)
		}
		h.chain.Mine(testSettle + 4)
		h.nodes[0].Set(oldHeadersUpTo(old, cursor-1))

		check := func(when string) {
			t.Helper()
			head := h.sink.lastHead()
			was, existed := old.BlockByNumber(head.Number)
			if onNewChain := !existed || was.Hash != head.Hash; onNewChain && head.Number >= paid.Number && len(h.sink.liveKeys()) != 1 {
				t.Fatalf("%d endpoints, %s: head %d of the new chain reported, and the payment in its block %d was not delivered",
					nodes, when, head.Number, paid.Number)
			}
		}
		for i := 0; i < 6; i++ {
			h.chain.Mine(1)
			_ = h.w.Tick(context.Background())
			check("while the primary lies")
		}
		switch nodes {
		case 2:
			// One says the block below the cursor is still there, one says
			// it is not: nothing is settled, and the operator is told.
			if st := h.w.Status(); st.Hold == "" || in.count(watcher.IncidentHeadHeld) == 0 || in.count(watcher.IncidentHeaderMismatch) == 0 {
				t.Fatalf("2 endpoints: hold %q, incidents %+v", st.Hold, in.list)
			}
			h.nodes[0].Set(honest)
			h.passCooldowns()
			h.mineTicking(2)
		case 3:
			// The third endpoint settles it: the primary is set aside, and
			// the watcher carries on from another endpoint's view.
			if h.client.Status()[0].Healthy {
				t.Fatalf("3 endpoints: the endpoint that hid the depth of the reorganisation was not set aside: %+v", in.list)
			}
			for _, i := range in.list {
				if i.Endpoint != "" && i.Endpoint != evm.RedactURL(h.urls[0]) {
					t.Fatalf("an honest endpoint was blamed: %+v", i)
				}
			}
		}
		check("afterwards")
		h.assertModel(startBlock)
		if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle || h.sink.lastHead().Number != st.Cursor {
			t.Fatalf("%d endpoints: cursor %d, sink head %d, chain head %d; hold %q", nodes, st.Cursor, h.sink.lastHead().Number, h.chain.Head().Number, st.Hold)
		}
	}
}

// TestCursorBlockIsRememberedHoweverTheCursorGotThere is seed 2253 of the
// randomised model, in the first soak after the model learnt to empty the
// watch set (3 000 seeds on c3bed82), which ended with
//
//	HEAD RULE: head 10225 reported, but the transfer … in block 10217 … was never delivered
//
// No endpoint has to lie for it. The watcher remembers the blocks it settles
// on (the anchors) and compares the newest with both endpoints at the start
// of every tick: that is how it notices a reorganisation deeper than the
// settle depth. But the cursor also comes to rest on a block in two ways that
// settle nothing — the start position of a watcher without a checkpoint, and
// the position a checkpoint far below everything is moved up to
// (skipUnneeded) — and there the block's header was agreed on, reported as
// the processed head, and not remembered. Until the next block was settled,
// the watcher had no record of the block its cursor stood on. If the chain
// was reorganised below it in that time, nothing noticed: the scan went on
// from the block above, and a transfer in the block that replaced the
// cursor's was passed by — to an address created in between, whose start
// block (the consumer's processed head, by number) says nothing about a
// block of another fork.
//
// With a watcher that ticks every three seconds that takes a reorganisation
// of more than the settle depth within one poll interval. Idle polling makes
// the interval ten times as long, which is why this is fixed with it: the
// block the endpoints agree on for the cursor is remembered whenever no
// remembered block covers it.
func TestCursorBlockIsRememberedHoweverTheCursorGotThere(t *testing.T) {
	// reorgBelowCursor replaces the cursor's block and creates a payment —
	// address first, with the consumer's head as its start block — whose
	// transfer the new fork includes at a height at or below that head.
	reorgBelowCursor := func(t *testing.T, h *harness) (tx evm.Hash, since uint64) {
		t.Helper()
		st := h.w.Status()
		since = h.sink.lastHead().Number
		if since != st.Cursor || st.Cursor == 0 {
			t.Fatalf("test setup: sink head %d, cursor %d", since, st.Cursor)
		}
		h.chain.Reorg(int(h.chain.Head().Number-st.Cursor) + 2)
		h.watch.addSince(addr(1), since)
		tx = h.pay(addr(1), 77)
		h.chain.Mine(testSettle + 6) // the transfer is in the first of them
		return tx, since
	}
	found := func(t *testing.T, h *harness, tx evm.Hash, since uint64) {
		t.Helper()
		// The head rule is checked by the harness on every head.
		for i := 0; i < 6; i++ {
			_ = h.w.Tick(context.Background())
			h.passCooldowns()
		}
		tr, ok := findTransfer(h, tx)
		if !ok {
			t.Fatalf("the transfer in the block that replaced the cursor's was never reported: %+v\n%s", h.w.Status(), tail(h.logs.String(), 2500))
		}
		if tr.BlockNumber > since {
			t.Fatalf("test setup: the transfer is in block %d, above the start block %d", tr.BlockNumber, since)
		}
		h.assertModel(startBlock)
		if head, st := h.sink.lastHead(), h.w.Status(); head.Number != st.Cursor || st.Cursor != h.chain.Head().Number-testSettle {
			t.Fatalf("the watcher is not in step with the new fork: head %d, cursor %d, chain %d", head.Number, st.Cursor, h.chain.Head().Number)
		}
	}

	t.Run("the start position of a watcher without a checkpoint", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		for _, n := range h.nodes {
			n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = 64 })
		}
		h.chain.Mine(20)
		h.tick() // chooses the start position, reports its block as the head
		tx, since := reorgBelowCursor(t, h)
		found(t, h, tx, since)
		// The one block the watcher remembered is gone, and nothing below
		// it is remembered: that is accepted from two endpoints, loudly.
		if st := h.w.Status(); st.DeepReorgs != 1 {
			t.Fatalf("deep reorganisations = %d, want 1", st.DeepReorgs)
		}
	})

	t.Run("the position a checkpoint below everything is moved up to", func(t *testing.T) {
		var in incidents
		h := newHarness(t, withNodes(2), withIncidents(&in))
		for _, n := range h.nodes {
			n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = 64 })
		}
		h.chain.Mine(20)
		h.mineTicking(4)
		// The process is down while the chain moves on by more than a reorg
		// window; nothing is watched. The new process does not scan its way
		// up: it continues from the settled height of the chain.
		h.chain.Mine(testWindow + 20)
		h.restart()
		h.tick()
		if in.count(watcher.IncidentCheckpointBehind) == 0 {
			t.Fatalf("test setup: the checkpoint was not moved up: %+v", h.w.Status())
		}
		tx, since := reorgBelowCursor(t, h)
		found(t, h, tx, since)
		// Blocks below the new position are still remembered, so this is an
		// ordinary reorganisation.
		if st := h.w.Status(); st.DeepReorgs != 0 || st.Reorgs == 0 {
			t.Fatalf("reorganisations = %d, deep = %d; want an ordinary one", st.Reorgs, st.DeepReorgs)
		}
	})
}
