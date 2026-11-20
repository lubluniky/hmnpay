package watcher_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// These tests are about what Run does between ticks: the pause is the poll
// interval while there is something to watch or to do, and the idle interval
// while there is not (package documentation, "Idle"). They run Run itself, in
// simulated time (see simulation).

const (
	simPoll = 3 * time.Second
	simIdle = watcher.DefaultIdleInterval
)

// idleWatcher returns a harness with two endpoints whose watcher has come to
// rest idle: nothing is watched, and two minutes have passed.
func idleWatcher(t *testing.T, opts ...option) (*harness, *simulation) {
	t.Helper()
	h := newHarness(t, append([]option{withNodes(2)}, opts...)...)
	// Reorganisations in these tests go deeper than the two blocks behind
	// the head at which the fake nodes otherwise report finality.
	for _, n := range h.nodes {
		n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = 64 })
	}
	s := newSimulation(h)
	s.run(2 * time.Minute)
	if st := h.w.Status(); !st.Idle {
		t.Fatalf("the watcher is not idle after two minutes with nothing to watch: %s\n%+v", st.Work, st)
	}
	s.reset()
	return h, s
}

// lastPause returns the pause the watcher is in (the one a run was stopped
// in), or took last.
func lastPause(t *testing.T, s *simulation) simPause {
	t.Helper()
	if len(s.pauses) == 0 {
		t.Fatal("the watcher has not paused")
	}
	return s.pauses[len(s.pauses)-1]
}

// TestIdleCallRate is the reason idle polling exists: what an instance with
// nothing to watch costs in RPC calls, per endpoint. Before, the watcher
// ticked every three seconds whatever there was to do — 20 ticks and a
// hundred calls a minute on each endpoint, 144 000 a day. Idle, it ticks
// every thirty.
//
// Simulated time has no tick duration in it (on a real network a tick takes
// some 0.4 s, so a minute holds 17 or 18 of the three-second ticks, not 20);
// the numbers here are the upper bound.
func TestIdleCallRate(t *testing.T) {
	const minutes = 5
	measure := func(t *testing.T, idle time.Duration) (calls [2]int, ticks int) {
		h := newHarness(t, withNodes(2), withConfig(func(c *watcher.Config) { c.IdleInterval = idle }))
		s := newSimulation(h)
		s.run(time.Minute)
		s.reset()
		for _, n := range h.nodes {
			n.ResetCounters()
		}
		s.run(minutes * time.Minute)
		for i, n := range h.nodes {
			calls[i] = n.TotalCalls()
		}
		// The head has followed the chain all the while.
		if st := h.w.Status(); st.Behind > testSettle+uint64(max(idle, simPoll)/time.Second) {
			t.Fatalf("the cursor is %d blocks behind the chain: %+v", st.Behind, st)
		}
		return calls, len(s.ticks)
	}

	t.Run("idle", func(t *testing.T) {
		calls, ticks := measure(t, 0) // the default: 30 s
		for i, c := range calls {
			perMinute := float64(c) / minutes
			t.Logf("endpoint %d, nothing watched: %d calls in %d simulated minutes (%d ticks): %.1f calls/tick, %.0f calls/minute, %.0f calls/day",
				i, c, minutes, ticks, float64(c)/float64(ticks), perMinute, perMinute*60*24)
			// Two ticks a minute at five calls each. (The old rate: 100.)
			if perMinute > 12 {
				t.Errorf("endpoint %d: %.0f calls a minute with nothing to watch; an idle watcher must stay below 12", i, perMinute)
			}
		}
		if want := minutes * int(time.Minute/simIdle); ticks != want {
			t.Errorf("%d ticks in %d idle minutes, want %d (one every %s)", ticks, minutes, want, simIdle)
		}
	})

	// With idle polling switched off the watcher behaves as it did before
	// there was any: this is the rate the test above would have measured.
	t.Run("idle polling off", func(t *testing.T) {
		calls, ticks := measure(t, -1)
		for i, c := range calls {
			t.Logf("endpoint %d, nothing watched, idle polling off: %d calls in %d simulated minutes (%d ticks): %.0f calls/minute, %.0f calls/day",
				i, c, minutes, ticks, float64(c)/minutes, float64(c)/minutes*60*24)
			if perMinute := float64(c) / minutes; perMinute < 80 {
				t.Errorf("endpoint %d: %.0f calls a minute with idle polling off; the poll interval alone gives about 100", i, perMinute)
			}
		}
		if want := minutes * int(time.Minute/simPoll); ticks != want {
			t.Errorf("%d ticks in %d minutes with idle polling off, want %d (one every %s)", ticks, minutes, want, simPoll)
		}
	})
}

// TestRPCUsage measures the calls one tick costs on each of two endpoints —
// idle, quiet (closed payments only), with an open payment, and with a watch
// list that needs a second eth_getLogs filter — and pins the table the README
// states ("RPC usage"). Calls are JSON-RPC calls: the members of a batch
// count one each, as providers bill them.
//
// A quiet tick costs what an active one costs — it is the same tick, with the
// same two log scans — and comes a tenth as often.
func TestRPCUsage(t *testing.T) {
	perTick := func(t *testing.T, addresses int, closed bool) (calls [2]float64, pause time.Duration) {
		h := newHarness(t, withNodes(2))
		since := h.chain.Head().Number
		for i := 0; i < addresses; i++ {
			h.watch.addSince(addr(2_000_000+i), since)
			if closed {
				h.watch.close(addr(2_000_000 + i))
			}
		}
		s := newSimulation(h)
		// Ten ticks to get going, ten to measure: half a minute each with
		// an open payment, five minutes each without.
		window := 10 * simPoll
		if addresses == 0 || closed {
			window = 10 * simIdle
		}
		s.run(window)
		s.reset()
		for _, n := range h.nodes {
			n.ResetCounters()
		}
		s.run(window)
		if len(s.ticks) != 10 {
			t.Fatalf("%d ticks in %s, want 10", len(s.ticks), window)
		}
		for i, n := range h.nodes {
			calls[i] = float64(n.TotalCalls()) / float64(len(s.ticks))
		}
		return calls, lastPause(t, s).want
	}
	for _, tc := range []struct {
		name      string
		addresses int
		closed    bool    // every address belongs to a closed payment
		want      float64 // calls per tick and endpoint
		pause     time.Duration
	}{
		{"nothing watched", 0, false, 5, simIdle},
		{"1 closed address", 1, true, 7, simIdle},
		{"500 closed addresses", evm.DefaultMaxRecipients, true, 7, simIdle},
		{"501 closed addresses", evm.DefaultMaxRecipients + 1, true, 9, simIdle},
		{"1 address", 1, false, 7, simPoll},
		{"500 addresses", evm.DefaultMaxRecipients, false, 7, simPoll},
		{"501 addresses", evm.DefaultMaxRecipients + 1, false, 9, simPoll},
		{"1000 addresses", 2 * evm.DefaultMaxRecipients, false, 9, simPoll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (raceDetector || testing.Short()) && tc.addresses > evm.DefaultMaxRecipients+1 {
				t.Skip("the largest list is measured without the race detector, and not in short mode (the fake node matches every filter against every address)")
			}
			calls, pause := perTick(t, tc.addresses, tc.closed)
			if pause != tc.pause {
				t.Errorf("pause between ticks = %s, want %s", pause, tc.pause)
			}
			perMinute := float64(time.Minute / pause)
			for i, c := range calls {
				t.Logf("%-20s endpoint %d: %.2f calls/tick, a tick every %s: %.0f calls/minute, %.0f calls/day",
					tc.name, i, c, pause, c*perMinute, c*perMinute*60*24)
				if c < tc.want-0.05 || c > tc.want+0.05 {
					t.Errorf("endpoint %d: %.2f calls per tick, want %.0f", i, c, tc.want)
				}
			}
		})
	}
}

// TestPaymentCreatedWhileIdleIsFoundOnTheWakeUpTick: a payment is created in
// the middle of an idle pause. The watcher is woken, ticks in that same
// moment, and the transfer — mined two blocks later — is reported by the
// next tick, a poll interval on: as fast as if the watcher had never idled,
// and not when the idle pause would have ended.
func TestPaymentCreatedWhileIdleIsFoundOnTheWakeUpTick(t *testing.T) {
	h, s := idleWatcher(t)
	var created, mined time.Duration
	var block uint64
	var tx evm.Hash
	s.in(47*time.Second, func() {
		// As the core does it: the address is in the watch set, with the
		// consumer's processed head as its start block, before anybody can
		// pay to it — and then the watcher is told.
		h.watch.addSince(addr(1), h.sink.lastHead().Number)
		created = s.now
		h.w.Wake()
	})
	s.in(48*time.Second, func() { tx = h.pay(addr(1), 25) })
	s.in(49*time.Second, func() { // the block of this second holds the transfer
		mined, block = s.now, h.chain.Head().Number
	})
	s.run(90 * time.Second)

	// The pause the watcher was in was cut short at the moment of the
	// wake-up, and a tick ran in that moment.
	var cut *simPause
	for i := range s.pauses {
		if p := &s.pauses[i]; p.cut {
			if cut != nil {
				t.Fatalf("two pauses were cut short: %+v and %+v", *cut, *p)
			}
			cut = p
		}
	}
	if cut == nil || !cut.idle || cut.want != simIdle || cut.at+cut.took != created {
		t.Fatalf("the idle pause was not ended by the wake-up at %s: %+v\npauses: %+v", created, cut, s.pauses)
	}
	wake, ok := s.tickAt(created)
	if !ok || wake.at != created || wake.st.WatchedAddresses != 1 || wake.st.Idle {
		t.Fatalf("no tick with the new address at the moment of the wake-up (%s): %+v", created, wake)
	}
	if natural := cut.at + cut.want; wake.at >= natural {
		t.Fatalf("the tick ran at %s, when the idle pause would have ended anyway (%s)", wake.at, natural)
	}

	// The transfer is reported by the first tick after its block.
	var observedAt time.Duration
	for _, tk := range s.ticks {
		if tk.at >= mined && observedAt == 0 && tk.st.Tracked == 1 {
			observedAt = tk.at
		}
	}
	if tr, found := findTransfer(h, tx); !found || tr.BlockNumber != block {
		t.Fatalf("the transfer was not reported in block %d: %+v", block, tr)
	}
	if observedAt == 0 || observedAt-mined > simPoll {
		t.Fatalf("the transfer was mined at %s and reported at %s: more than a poll interval (%s) later", mined, observedAt, simPoll)
	}
	t.Logf("payment created %s into an idle pause of %s; wake-up tick at +0s; transfer mined at +%s, reported at +%s",
		cut.took, cut.want, mined-created, observedAt-created)

	// From the wake-up on the watcher keeps its poll interval, for as long
	// as the address is watched.
	for _, p := range s.pauses {
		if p.at >= created && (p.idle || p.want != simPoll) {
			t.Fatalf("an idle pause while an address is watched: %+v", p)
		}
	}
	h.assertModel(startBlock)

	// The address leaves the watch set (its watch window is over). The
	// transfer the watcher reported is still inside the reorg window, and
	// for that long the watcher is not idle; then it is.
	h.watch.remove(addr(1))
	removed := s.now
	s.reset()
	s.run(3 * time.Minute)
	first := time.Duration(-1)
	for _, tk := range s.ticks {
		switch {
		case tk.st.Idle && first < 0:
			first = tk.at
		case !tk.st.Idle && first >= 0:
			t.Fatalf("active again at %s without anything to watch: %s", tk.at, tk.st.Work)
		case !tk.st.Idle && !strings.Contains(tk.st.Work, "inside the reorg window"):
			t.Fatalf("tick at %s: not idle because %q; want the tracked transfer named", tk.at, tk.st.Work)
		}
	}
	if first < 0 {
		t.Fatalf("the watcher did not return to idle after the address left the watch set")
	}
	// The transfer's block must be a reorg window (40 blocks, one a second)
	// below the chain before it is forgotten.
	if gone := mined + testWindow*time.Second; first < gone || first > gone+2*simPoll {
		t.Fatalf("idle again at %s; the transfer leaves the reorg window at %s (address removed at %s)", first, gone, removed)
	}
}

// findTransfer returns the transfer of tx the sink believes in.
func findTransfer(h *harness, tx evm.Hash) (watcher.Transfer, bool) {
	for _, tr := range h.sink.liveKeys() {
		if tr.TxHash == tx {
			return tr, true
		}
	}
	return watcher.Transfer{}, false
}

// TestWakeUpDuringATickIsNotLost: the address is added, and the watcher
// woken, after an idle tick has read the watch set and before it has ended.
// That tick ends idle — it saw nothing — and the next one must follow at
// once, not an idle pause later.
func TestWakeUpDuringATickIsNotLost(t *testing.T) {
	h, s := idleWatcher(t)
	armed, fired := false, time.Duration(-1)
	rule := h.sink.headRule
	h.sink.set(func(sk *sink) {
		sk.headRule = func(head watcher.Head, live map[key]watcher.Transfer) {
			rule(head, live)
			if armed && fired < 0 {
				// The head is reported at the end of a tick: its snapshot of
				// the watch set was taken long before.
				fired = s.now
				h.watch.addSince(addr(1), head.Number)
				h.w.Wake()
			}
		}
	})
	s.in(time.Second, func() { armed = true })
	s.run(40 * time.Second)
	if fired < 0 {
		t.Fatal("no idle tick reported a head")
	}
	var at []simTick
	for _, tk := range s.ticks {
		if tk.at == fired {
			at = append(at, tk)
		}
	}
	if len(at) != 2 || !at[0].st.Idle || at[0].st.WatchedAddresses != 0 || at[1].st.Idle || at[1].st.WatchedAddresses != 1 {
		t.Fatalf("want two ticks at %s — the idle one, and at once the one that sees the address; got %+v", fired, at)
	}
	for _, p := range s.pauses {
		if p.at >= fired && p.idle {
			t.Fatalf("an idle pause after the wake-up: %+v", p)
		}
	}
}

// TestIdleWatcherFindsAPaymentWithoutAWakeUp: the wake-up is an optimisation,
// not what correctness rests on. If nobody wakes the watcher, the payment is
// found by the tick that ends the idle pause: the watch set is read on every
// tick, idle or not.
func TestIdleWatcherFindsAPaymentWithoutAWakeUp(t *testing.T) {
	h, s := idleWatcher(t)
	var tx evm.Hash
	var created time.Duration
	s.in(5*time.Second, func() {
		h.watch.addSince(addr(1), h.sink.lastHead().Number)
		created = s.now
		tx = h.pay(addr(1), 40)
	})
	s.run(simIdle + 10*time.Second)
	tk, ok := s.tickAt(created)
	if !ok || tk.at-created >= simIdle || tk.st.Tracked != 1 {
		t.Fatalf("the first tick after the payment (created at %s) did not report it: %+v", created, tk)
	}
	if _, found := findTransfer(h, tx); !found {
		t.Fatal("the transfer was not reported")
	}
	for _, p := range s.pauses {
		if p.cut {
			t.Fatalf("a pause was cut short without a wake-up: %+v", p)
		}
	}
	h.assertModel(startBlock)
}

// TestStartBlockAfterIdle is about the start block of a payment created
// after an idle stretch. The consumer gives a new address the processed head
// it holds as the block at or below which the address cannot have been paid
// (WatchedAddress.Since), and while the watcher idles that head is up to an
// idle interval old. It is therefore lower than it would otherwise be —
// which is the safe direction, and the test shows both halves of that:
//
//   - a transfer to the address in a block above that head is reported,
//     although the block was mined during the idle pause, before the watcher
//     had heard of the address: the tick after the wake-up scans every block
//     above the cursor, and the cursor is that head;
//   - a transfer in a block at or below it is not looked for. (It cannot
//     exist: the address was created after that block. The test mines one to
//     show where the line is.)
func TestStartBlockAfterIdle(t *testing.T) {
	h, s := idleWatcher(t)
	blockOf := func(tx evm.Hash) uint64 {
		for n := h.chain.Head().Number; n > startBlock; n-- {
			blk, _ := h.chain.BlockByNumber(n)
			for _, x := range blk.Txs {
				if x.Hash == tx {
					return n
				}
			}
		}
		t.Fatalf("transaction %s is not on the chain", tx)
		return 0
	}
	// A transfer to the address-to-be in a block the watcher settles while
	// idle, with the address unknown: it will lie below the start block.
	early := h.pay(addr(1), 11)
	s.run(10 * time.Second)
	s.reset()

	// The run begins with a tick, which settles that block; then the idle
	// pause. Ten blocks into it a transfer is mined; ten blocks later the
	// address is created, with the consumer's head as its start block — a
	// head that is twenty blocks old by then.
	var during evm.Hash
	var since, tip uint64
	var created time.Duration
	s.in(10*time.Second, func() { during = h.pay(addr(1), 22) })
	s.in(20*time.Second, func() {
		since, tip = h.sink.lastHead().Number, h.chain.Head().Number
		// The head rule of the harness, from the start block on: every
		// transfer to a watched address above it must have been delivered
		// when a head is reported.
		h.ruleFrom = since + 1
		h.watch.addSince(addr(1), since)
		created = s.now
		h.w.Wake()
	})
	s.run(40 * time.Second)

	if head := s.ticks[0].st.Cursor; since != head || !s.ticks[0].st.Idle {
		t.Fatalf("start block %d; the idle tick before the pause left the head at %d (%+v)", since, head, s.ticks[0].st)
	}
	if b := blockOf(early); b > since {
		t.Fatalf("the early transfer is in block %d, above the start block %d", b, since)
	}
	// Lower than the head the consumer would hold had the watcher ticked
	// every three seconds (the tip minus the settle depth, give or take a
	// tick) — by the blocks of the pause so far, and never higher.
	if fresh := tip - testSettle; since >= fresh || fresh-since > uint64(simIdle/time.Second) {
		t.Fatalf("start block %d, chain at %d: want it below %d by at most an idle interval's blocks", since, tip, fresh)
	}
	b := blockOf(during)
	if b <= since || b > tip {
		t.Fatalf("the transfer mined during the pause is in block %d; start block %d, tip at creation %d", b, since, tip)
	}
	tr, found := findTransfer(h, during)
	if !found || tr.BlockNumber != b {
		t.Fatalf("the transfer mined during the idle pause, above the start block, was not reported: %+v", tr)
	}
	wake, ok := s.tickAt(created)
	if !ok || wake.at != created || wake.st.Tracked != 1 {
		t.Fatalf("it was not reported by the wake-up tick: %+v", wake)
	}
	if _, found := findTransfer(h, early); found {
		t.Fatal("a transfer at or below the start block was reported")
	}
	h.assertModel(since + 1)
	t.Logf("start block %d was %d blocks below the chain (%d with a three-second tick); transfer in block %d reported by the wake-up tick",
		since, tip-since, testSettle, b)
}

// TestReorgAcrossIdle: the chain is reorganised during an idle pause, deeper
// than the settle depth — the block the cursor rests on is replaced. The
// first tick after the pause must not scan on from it: like any tick it
// finds, with both endpoints, that the remembered block is gone, goes back
// to the newest block that is still there and settles again from there.
//
// And the wake-up on top of it: a payment is created after the
// reorganisation, while the consumer still holds the head of the old fork.
// Its start block is that head's number, and the new fork — shorter for a
// moment — includes the transfer at a height at or below it. The transfer is
// found all the same, because the watcher's position is put back before the
// scan, not after.
func TestReorgAcrossIdle(t *testing.T) {
	for _, idle := range []time.Duration{simIdle, 2 * time.Minute} {
		name := fmt.Sprintf("idle interval %s", idle)
		if idle > testWindow*time.Second {
			name += ", longer than the reorg window"
		}
		t.Run(name+"/nothing watched", func(t *testing.T) {
			h, s := idleWatcher(t, withConfig(func(c *watcher.Config) { c.IdleInterval = idle }))
			// A few idle ticks, so that the watcher remembers more than one
			// block. (With a single remembered block, and that one
			// replaced, nothing it knows is left on the chain: that is the
			// finality conflict, which is loud, not this test.)
			s.run(3 * idle)
			s.reset()
			var before watcher.Status
			var old evmtest.Block
			var reorgAt time.Duration
			// Five seconds into an idle pause.
			s.in(5*time.Second, func() {
				before = h.w.Status()
				old, _ = h.chain.BlockByNumber(before.Cursor)
				// The cursor is six blocks below the head of its tick, and
				// blocks have been mined since.
				h.chain.Reorg(int(h.chain.Head().Number-before.Cursor) + 4)
				reorgAt = s.now
			})
			s.run(idle + 10*time.Second)

			if blk, ok := h.chain.BlockByNumber(before.Cursor); !ok || blk.Hash == old.Hash {
				t.Fatalf("the test did not replace the cursor's block %d (chain at %d)", before.Cursor, h.chain.Head().Number)
			}
			st := h.w.Status()
			if st.Reorgs != before.Reorgs+1 || st.DeepReorgs != 0 {
				t.Fatalf("reorganisations counted: %d (deep: %d), want %d and none\n%s", st.Reorgs, st.DeepReorgs, before.Reorgs+1, tail(h.logs.String(), 3000))
			}
			if log := h.logs.String(); strings.Contains(log, "chain watcher incident") {
				t.Fatalf("an incident was raised:\n%s", tail(log, 3000))
			}
			// The head rule was checked on every head (harness). What the
			// sink holds now is a block of the new chain, and the cursor is
			// in step again.
			head := h.sink.lastHead()
			if blk, ok := h.chain.BlockByNumber(head.Number); !ok || blk.Hash != head.Hash || head.Number != st.Cursor {
				t.Fatalf("processed head %d (%s) is not the chain's block at the cursor (%d)", head.Number, head.Hash, st.Cursor)
			}
			// The tick that found it was the first one after the pause — the
			// pause was not cut short, nobody knew — and it did not leave
			// the watcher idle; the tick after it does.
			found, ok := s.tickAt(reorgAt)
			if !ok || found.at != s.ticks[0].at+idle || found.st.Idle || !strings.Contains(found.st.Work, "reorganisation") || found.st.Reorgs != before.Reorgs+1 {
				t.Fatalf("the first tick after the reorganisation (at %s): %+v", reorgAt, found)
			}
			if !st.Idle {
				t.Fatalf("not idle again: %s", st.Work)
			}
		})

		t.Run(name+"/payment created on the new fork", func(t *testing.T) {
			h, s := idleWatcher(t, withConfig(func(c *watcher.Config) { c.IdleInterval = idle }))
			s.run(3 * idle)
			s.reset()
			var before watcher.Status
			var tx evm.Hash
			var since uint64
			s.in(5*time.Second, func() {
				before = h.w.Status()
				h.chain.Reorg(int(h.chain.Head().Number-before.Cursor) + 4)
				// The consumer has not heard of the reorganisation: the
				// head it holds is the old fork's.
				since = h.sink.lastHead().Number
				h.watch.addSince(addr(1), since)
				tx = h.pay(addr(1), 33) // into the next block of the new fork
				h.w.Wake()
			})
			s.run(time.Minute)

			if since != before.Cursor {
				t.Fatalf("start block %d, want the old head %d", since, before.Cursor)
			}
			tr, found := findTransfer(h, tx)
			if !found {
				t.Fatalf("the transfer on the new fork was not reported: %+v", h.w.Status())
			}
			if tr.BlockNumber > since {
				t.Fatalf("the transfer is in block %d, above the start block %d: the test wants it at or below", tr.BlockNumber, since)
			}
			h.assertModel(startBlock)
			st := h.w.Status()
			if head := h.sink.lastHead(); head.Number != st.Cursor || st.Cursor+testSettle != st.Tip.Number {
				t.Fatalf("the watcher is not in step with the new fork: head %d, %+v", head.Number, st)
			}
			t.Logf("start block %d (the old fork's head); transfer in block %d of the new fork, reported", since, tr.BlockNumber)
		})
	}
}

// TestPendingWorkPreventsIdling: with nothing in the watch set the watcher
// still does not idle while it has business of its own — and says which.
func TestPendingWorkPreventsIdling(t *testing.T) {
	// noIdlePause fails if the watcher took an idle pause since `since`.
	noIdlePause := func(t *testing.T, s *simulation, since time.Duration) {
		t.Helper()
		for _, p := range s.pauses {
			if p.at >= since && (p.idle || p.want >= simIdle) {
				t.Fatalf("an idle pause with work pending: %+v", p)
			}
		}
	}
	// work fails unless every tick since `since` named a reason containing
	// want for not idling.
	work := func(t *testing.T, s *simulation, since time.Duration, want string) {
		t.Helper()
		n := 0
		for _, tk := range s.ticks {
			if tk.at < since {
				continue
			}
			n++
			if tk.st.Idle || !strings.Contains(tk.st.Work, want) {
				t.Fatalf("tick at %s: idle=%v work=%q; want work containing %q", tk.at, tk.st.Idle, tk.st.Work, want)
			}
		}
		if n == 0 {
			t.Fatalf("no tick since %s", since)
		}
	}

	t.Run("a requested re-scan wakes the watcher and is done before it idles again", func(t *testing.T) {
		h, s := idleWatcher(t)
		from := h.w.Status().Cursor - 20
		var asked time.Duration
		s.in(7*time.Second, func() {
			asked = s.now
			h.w.Rescan(from)
		})
		s.run(20 * time.Second)
		tk, ok := s.tickAt(asked)
		if !ok || tk.at != asked {
			t.Fatalf("the re-scan did not start when it was asked for (%s): next tick %+v", asked, tk)
		}
		if !strings.Contains(h.logs.String(), "re-scan requested") {
			t.Fatal("the re-scan was not carried out")
		}
		if st := h.w.Status(); !st.Idle {
			t.Fatalf("not idle after the re-scan: %s", st.Work)
		}
	})

	t.Run("a re-scan asked for during a tick", func(t *testing.T) {
		h, s := idleWatcher(t)
		armed, asked := false, time.Duration(-1)
		rule := h.sink.headRule
		h.sink.set(func(sk *sink) {
			sk.headRule = func(head watcher.Head, live map[key]watcher.Transfer) {
				rule(head, live)
				if armed && asked < 0 {
					asked = s.now
					h.w.Rescan(head.Number - 20)
				}
			}
		})
		s.in(time.Second, func() { armed = true })
		s.run(40 * time.Second)
		var at []simTick
		for _, tk := range s.ticks {
			if tk.at == asked {
				at = append(at, tk)
			}
		}
		// The tick in which it was asked for does not end idle, and says
		// why; the next one carries it out.
		if len(at) < 1 || at[0].st.Idle || !strings.Contains(at[0].st.Work, "re-scan") {
			t.Fatalf("ticks at %s: %+v", asked, at)
		}
		if !strings.Contains(h.logs.String(), "re-scan requested") {
			t.Fatal("the re-scan was not carried out")
		}
	})

	t.Run("a re-scan that is under way", func(t *testing.T) {
		h, s := idleWatcher(t)
		// The second endpoint goes away as the re-scan is asked for:
		// nothing can be settled, so the cursor stays where the re-scan put
		// it.
		var asked time.Duration
		s.in(4*time.Second, func() {
			asked = s.now
			h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
			h.w.Rescan(h.w.Status().Cursor - 20)
		})
		s.run(40 * time.Second)
		noIdlePause(t, s, asked)
		if st := h.w.Status(); st.Idle || st.Work == "" {
			t.Fatalf("idle with a re-scan under way and no second endpoint: %+v", st)
		}
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
		h.passCooldowns()
		s.run(time.Minute)
		if st := h.w.Status(); !st.Idle {
			t.Fatalf("not idle after the re-scan was completed: %s", st.Work)
		}
	})

	t.Run("a head that is held", func(t *testing.T) {
		h, s := idleWatcher(t)
		var down time.Duration
		s.in(4*time.Second, func() {
			down = s.now
			h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
		})
		s.run(3 * time.Minute)
		// The idle pause that was running ends by itself (nobody woke the
		// watcher); from the tick that finds the endpoint gone, the
		// watcher keeps its poll interval and says what it is waiting for.
		found, ok := s.tickAt(down)
		if !ok || found.at-down > simIdle {
			t.Fatalf("the outage at %s was not noticed within an idle interval: %+v", down, found)
		}
		noIdlePause(t, s, found.at)
		work(t, s, found.at, "second endpoint")
		if st := h.w.Status(); st.Hold == "" {
			t.Fatalf("the head is not held: %+v", st)
		}
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
		h.passCooldowns()
		s.run(time.Minute)
		if st := h.w.Status(); !st.Idle || st.Hold != "" {
			t.Fatalf("not idle after the endpoint came back: %+v", st)
		}
	})

	t.Run("single-source operation", func(t *testing.T) {
		h, s := idleWatcher(t, withConfig(func(c *watcher.Config) { c.Verify = watcher.VerifyBestEffort }))
		var down time.Duration
		s.in(4*time.Second, func() {
			down = s.now
			h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
		})
		s.run(3 * time.Minute)
		found, _ := s.tickAt(down)
		noIdlePause(t, s, found.at)
		st := h.w.Status()
		if !st.Solo || st.Idle || !strings.Contains(st.Work, "single-source") {
			t.Fatalf("want single-source operation, not idle: %+v", st)
		}
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
		h.passCooldowns()
		s.run(time.Minute)
		if st := h.w.Status(); !st.Idle || st.Solo {
			t.Fatalf("not idle after the second endpoint came back: %+v", st)
		}
	})

	t.Run("a transfer the consumer holds as not final", func(t *testing.T) {
		h, s := idleWatcher(t)
		h.watch.addSince(addr(1), h.sink.lastHead().Number)
		h.w.Wake()
		tx := h.pay(addr(1), 5)
		s.run(30 * time.Second)
		tr, found := findTransfer(h, tx)
		if !found {
			t.Fatal("the transfer was not reported")
		}
		// The payment's watch window ends; the consumer still lists the
		// transfer as pending (it has not got its confirmations, say).
		h.watch.remove(addr(1))
		h.watch.setPending(tr)
		s.reset()
		since := s.now
		s.run(3 * time.Minute) // far longer than the reorg window
		noIdlePause(t, s, since)
		if st := h.w.Status(); st.Idle || st.Tracked != 1 {
			t.Fatalf("idle while the consumer holds a pending transfer: %+v", st)
		}
		h.watch.setPending()
		s.run(time.Minute)
		if st := h.w.Status(); !st.Idle || st.Tracked != 0 {
			t.Fatalf("not idle after the consumer let the transfer go: %+v", st)
		}
	})

	// An idle tick asks nobody for a block number, so an endpoint that
	// inflates its eth_blockNumber and nothing else is not heard by it. The
	// pool's health check does ask — every endpoint, used or not — and what
	// it was told within the last minute counts: the watcher does not rest
	// while any endpoint claims that the chain goes on far above its cursor
	// (see the same case under TestWhatPreventsQuiet, where the tick itself
	// is told).
	t.Run("a longer chain an endpoint claimed to the pool's monitor", func(t *testing.T) {
		h, s := idleWatcher(t, withNodes(3))
		lie := func(b *evmtest.Behavior) {
			b.FinalityDepth = 64
			b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
				if method == "eth_blockNumber" {
					return hexUint(h.chain.Head().Number + 1000), nil, true
				}
				return nil, nil, false
			}
		}
		var since time.Duration
		// The health check runs every 15 seconds, also while the watcher
		// idles (the bridge: twice a minute then). The third endpoint, which
		// no tick has a reason to ask, lies to it.
		check := func() { h.client.Refresh(context.Background()) }
		s.in(4*time.Second, func() {
			since = s.now
			h.nodes[2].Set(lie)
		})
		for at := 5 * time.Second; at < 2*time.Minute; at += 15 * time.Second {
			s.in(at, check)
		}
		s.run(2 * time.Minute)
		found, ok := s.tickAt(since)
		if !ok || found.at-since > simIdle {
			t.Fatalf("no tick within an idle interval of %s: %+v", since, found)
		}
		noIdlePause(t, s, found.at)
		work(t, s, found.at, evm.RedactURL(h.urls[2])+" shows the chain at block")
		if st := h.w.Status(); st.Hold != "" || st.Cursor+testSettle != st.Tip.Number {
			t.Fatalf("status while an endpoint claims a longer chain: %+v", st)
		}
		// It stops. What it said is forgotten a minute after it said it.
		h.nodes[2].Set(func(b *evmtest.Behavior) { b.Intercept = nil })
		check()
		s.reset()
		s.run(10 * time.Second)
		if st := h.w.Status(); !st.Idle {
			t.Fatalf("not idle after the endpoint stopped: %s", st.Work)
		}
	})

	// Blocks settled on one endpoint's logs wait for a second endpoint to
	// scan them — as far as an address that is still watched can have been
	// paid in them. While there is such an address the watcher is at work,
	// and says so. When the watch set is empty nothing is left to scan in
	// those blocks, whatever range the checkpoint still records: the
	// watcher idles, and the record is closed, as before, by the next scan
	// a second endpoint confirms.
	t.Run("an unchecked range", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		h.watch.addSince(addr(1), h.chain.Head().Number)
		h.noLogService(1, &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"})
		h.mineTicking(3)
		recorded := h.w.Status()
		if recorded.UncheckedFrom == 0 || !recorded.LogsUnchecked {
			t.Fatalf("no blocks were settled on one endpoint's logs: %+v", recorded)
		}
		if recorded.Idle || !strings.Contains(recorded.Work, "settled on one endpoint's logs") {
			t.Fatalf("with an unchecked range and a watched address: idle=%v work=%q", recorded.Idle, recorded.Work)
		}

		s := newSimulation(h)
		s.run(30 * time.Second)
		noIdlePause(t, s, 0)
		work(t, s, 0, "settled on one endpoint's logs")

		// The watch set empties. Nothing in the range is left to scan.
		h.watch.remove(addr(1))
		s.reset()
		s.run(2 * time.Minute)
		st := h.w.Status()
		if !st.Idle {
			t.Fatalf("not idle with an empty watch set: %s", st.Work)
		}
		if st.UncheckedFrom != recorded.UncheckedFrom || st.UncheckedTo < recorded.UncheckedTo {
			t.Fatalf("the record of unchecked blocks changed while idle: %d..%d, was %d..%d",
				st.UncheckedFrom, st.UncheckedTo, recorded.UncheckedFrom, recorded.UncheckedTo)
		}

		// The second endpoint gets a log service, and a payment is created.
		// The new address cannot have been paid in the old range, so the
		// record is closed without a query into it.
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError = nil })
		h.passLogPause()
		h.nodes[1].ResetCounters()
		since := h.sink.lastHead().Number
		h.watch.addSince(addr(2), since)
		h.w.Wake()
		tx := h.pay(addr(2), 9)
		s.run(time.Minute)
		if _, found := findTransfer(h, tx); !found {
			t.Fatal("the payment after the idle stretch was not reported")
		}
		if st := h.w.Status(); st.UncheckedFrom != 0 || st.LogsUnchecked {
			t.Fatalf("the record of unchecked blocks was not closed: %+v", st)
		}
		for _, q := range h.nodes[1].LogQueries() {
			if q.From <= since {
				t.Fatalf("the second endpoint was asked for logs of blocks %d..%d, at or below the start block %d of the only watched address", q.From, q.To, since)
			}
		}
	})
}

// TestIdleStatus: what Status says while the watcher idles, and that a dead
// endpoint is still noticed within an idle interval.
func TestIdleStatus(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		_, s := idleWatcher(t)
		s.run(2 * time.Minute)
		for i, tk := range s.ticks {
			st := tk.st
			// Every idle tick is a full tick: it reached both endpoints,
			// settled what had become deep enough and reported the head.
			if !st.Idle || st.Work != "" || st.Hold != "" || st.LastError != "" || st.Verifier == "" {
				t.Fatalf("tick at %s: %+v", tk.at, st)
			}
			if st.Behind != testSettle || st.Processed.Number != st.Cursor || st.Tip.Number != st.Cursor+testSettle {
				t.Fatalf("tick at %s: cursor %d, processed %d, tip %d", tk.at, st.Cursor, st.Processed.Number, st.Tip.Number)
			}
			if !st.LastTickAt.Equal(st.LastSuccessAt) {
				t.Fatalf("tick at %s: last tick %s, last success %s", tk.at, st.LastTickAt, st.LastSuccessAt)
			}
			if i > 0 {
				prev := s.ticks[i-1]
				if gap := st.LastTickAt.Sub(prev.st.LastTickAt); gap != simIdle {
					t.Fatalf("ticks %s apart, want %s", gap, simIdle)
				}
				if moved := st.Cursor - prev.st.Cursor; moved != uint64(simIdle/time.Second) {
					t.Fatalf("the head moved %d blocks in an idle interval of %d", moved, simIdle/time.Second)
				}
			}
		}
		if len(s.ticks) != 4 {
			t.Fatalf("%d ticks in two idle minutes, want 4", len(s.ticks))
		}
	})

	t.Run("the first tick of a process", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		if st := h.w.Status(); st.Idle || !st.LastTickAt.IsZero() {
			t.Fatalf("before the first tick: %+v", st)
		}
		s := newSimulation(h)
		s.run(simIdle - time.Second)
		// Nothing to watch from the start: one tick, idle, and the status
		// says when it was.
		if len(s.ticks) != 1 || !s.ticks[0].st.Idle || s.ticks[0].st.LastTickAt.IsZero() {
			t.Fatalf("ticks: %+v", s.ticks)
		}
	})

	// A chain that is shorter than the settle depth has no block deep
	// enough to be the processed head. The consumer is waiting for its
	// first head: the watcher keeps its poll interval until it has one.
	t.Run("before the first processed head", func(t *testing.T) {
		// Block 2 of a new chain; the settle depth is 6.
		h := newHarness(t, withYoungChain(2), withNodes(2))
		s := newSimulation(h)
		s.run(3 * time.Second) // the chain: 5 blocks
		for _, p := range s.pauses {
			if p.idle || p.want != simPoll {
				t.Fatalf("an idle pause before anything was settled: %+v", p)
			}
		}
		if st := h.w.Status(); st.Idle || st.Cursor != 0 || st.Work != "nothing has been settled yet" || h.sink.lastHead().Number != 0 {
			t.Fatalf("status before the chain is a settle depth long: %+v", st)
		}
		s.reset()
		s.run(time.Minute)
		st := h.w.Status()
		if !st.Idle || st.Cursor == 0 || h.sink.lastHead().Number != st.Cursor {
			t.Fatalf("status after the first block was settled: %+v", st)
		}
		// The first head came with the first tick that could settle a
		// block (the chain is 7 blocks long two seconds into this run), not
		// an idle interval later.
		first := time.Duration(-1)
		for _, tk := range s.ticks {
			if tk.st.Cursor != 0 && first < 0 {
				first = tk.at - s.ticks[0].at
			}
		}
		if first < 0 || first > 2*simPoll {
			t.Fatalf("the first processed head came %s into the run", first)
		}
	})

	t.Run("an endpoint that dies is noticed within an idle interval", func(t *testing.T) {
		h, s := idleWatcher(t)
		var down time.Duration
		s.in(2*time.Second, func() {
			down = s.now
			for _, n := range h.nodes {
				n.Set(func(b *evmtest.Behavior) { b.Down = true })
			}
		})
		s.run(2 * time.Minute)
		tk, ok := s.tickAt(down)
		if !ok || tk.at-down > simIdle || tk.st.LastError == "" || tk.st.Idle {
			t.Fatalf("the outage at %s: first tick after it %+v", down, tk)
		}
		// From then on the watcher retries with its backoff, not at the
		// idle interval, and is not idle.
		st := h.w.Status()
		if st.Idle || st.ConsecutiveErrors < 3 || st.Work != "the last tick failed" {
			t.Fatalf("status during the outage: %+v", st)
		}
		for _, n := range h.nodes {
			n.Set(func(b *evmtest.Behavior) { b.Down = false })
		}
		h.passCooldowns()
		s.run(2 * time.Minute)
		if st := h.w.Status(); !st.Idle || st.ConsecutiveErrors != 0 {
			t.Fatalf("status after the outage: %+v", st)
		}
	})

	t.Run("idle polling off", func(t *testing.T) {
		for _, idle := range []time.Duration{-1, simPoll, time.Second} {
			h := newHarness(t, withNodes(2), withConfig(func(c *watcher.Config) { c.IdleInterval = idle }))
			s := newSimulation(h)
			s.run(time.Minute)
			for _, p := range s.pauses {
				if p.want != simPoll || p.idle {
					t.Fatalf("idle interval %s: pause %+v, want every pause to be the poll interval", idle, p)
				}
			}
			if st := h.w.Status(); st.Idle || st.Work != "" {
				t.Fatalf("idle interval %s: %+v", idle, st)
			}
		}
	})
}
