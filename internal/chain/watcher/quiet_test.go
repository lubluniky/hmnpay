package watcher_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// These tests are about the quiet watcher (package documentation, "Quiet"):
// one whose watch set holds closed addresses and nothing else. It pauses the
// idle interval between ticks like a watcher with nothing to watch — and
// every one of its ticks scans every block for every address, on two
// endpoints. Like the idle tests they run Run itself, in simulated time: a
// block a second, three seconds between ticks, thirty while at rest.

// quietWatcher returns a harness with two endpoints whose watch set holds n
// closed addresses, and whose watcher has come to rest quiet.
func quietWatcher(t *testing.T, n int, opts ...option) (*harness, *simulation, []ethaddr.Address) {
	t.Helper()
	h := newHarness(t, append([]option{withNodes(2)}, opts...)...)
	for _, node := range h.nodes {
		node.Set(func(b *evmtest.Behavior) { b.FinalityDepth = 64 })
	}
	closed := closedAddresses(h, n)
	s := newSimulation(h)
	s.run(2 * time.Minute)
	if st := h.w.Status(); !st.Idle || !st.Quiet || st.WatchedAddresses != n {
		t.Fatalf("the watcher is not quiet after two minutes with %d closed address(es) to watch: idle=%v quiet=%v work=%q\n%+v", n, st.Idle, st.Quiet, st.Work, st)
	}
	s.reset()
	return h, s, closed
}

// closedAddresses puts n addresses into the watch set as the core lists the
// addresses of settled payments: with a start block, and closed.
func closedAddresses(h *harness, n int) []ethaddr.Address {
	since := h.chain.Head().Number - testSettle
	closed := make([]ethaddr.Address, n)
	for i := range closed {
		closed[i] = addr(500_000 + i)
		h.watch.addSince(closed[i], since)
	}
	h.watch.close(closed...)
	return closed
}

// holdPending makes the harness's consumer do what the core does with a
// transfer it is given: list it as pending (Snapshot.Pending) until the
// processed head has given it `confirmations` blocks, or it is reverted.
func holdPending(h *harness, confirmations uint64) {
	rule := h.sink.headRule
	h.sink.set(func(sk *sink) {
		sk.headRule = func(head watcher.Head, live map[key]watcher.Transfer) {
			rule(head, live)
			var pending []watcher.Transfer
			for _, tr := range live {
				if tr.BlockNumber+confirmations > head.Number+1 {
					pending = append(pending, tr)
				}
			}
			h.watch.setPending(pending...)
		}
		sk.onObserved = func(tr watcher.Transfer) {
			h.watch.mu.Lock()
			defer h.watch.mu.Unlock()
			if head := sk.head.Number; tr.BlockNumber+confirmations > head+1 {
				h.watch.pending = append(h.watch.pending, tr)
			}
		}
		sk.onReverted = func(tr watcher.Transfer) {
			h.watch.mu.Lock()
			defer h.watch.mu.Unlock()
			kept := h.watch.pending[:0:0]
			for _, p := range h.watch.pending {
				if keyOf(p) != keyOf(tr) {
					kept = append(kept, p)
				}
			}
			h.watch.pending = kept
		}
	})
}

// covers reports whether the log queries a node answered cover every block
// from..to, each with all of `recipients` addresses in its filter.
func covers(queries []evmtest.LogQuery, from, to uint64, recipients int) bool {
	for n := from; n <= to; n++ {
		found := false
		for _, q := range queries {
			if !q.Refused && q.From <= n && n <= q.To && q.Recipients == recipients {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestQuietCallRate is the reason the quiet state exists: what an instance
// costs in RPC calls, per endpoint, whose watch list holds the addresses of
// closed payments and nothing else — the shop with a paid order now and then,
// whose every payment stays on the list for a week. Before, an address was an
// address: the watcher ticked every three seconds for it, 20 ticks and some
// 140 calls a minute on each endpoint. Quiet, it ticks every thirty.
//
// (Simulated time has no tick duration in it: the numbers are the upper
// bound, as in TestIdleCallRate.)
func TestQuietCallRate(t *testing.T) {
	const minutes = 5
	measure := func(t *testing.T, idle time.Duration, open int) (calls [2]int, ticks int, st watcher.Status) {
		h := newHarness(t, withNodes(2), withConfig(func(c *watcher.Config) { c.IdleInterval = idle }))
		closedAddresses(h, 3)
		for i := 0; i < open; i++ {
			h.watch.addSince(addr(1+i), h.chain.Head().Number-testSettle)
		}
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
		st = h.w.Status()
		if st.Behind > testSettle+uint64(simIdle/time.Second) {
			t.Fatalf("the cursor is %d blocks behind the chain: %+v", st.Behind, st)
		}
		return calls, len(s.ticks), st
	}

	t.Run("closed payments only", func(t *testing.T) {
		calls, ticks, st := measure(t, 0, 0)
		if !st.Quiet || !st.Idle {
			t.Fatalf("not quiet with closed addresses only: %s", st.Work)
		}
		for i, c := range calls {
			perMinute := float64(c) / minutes
			t.Logf("endpoint %d, 3 closed addresses: %d calls in %d simulated minutes (%d ticks): %.1f calls/tick, %.0f calls/minute, %.0f calls/day",
				i, c, minutes, ticks, float64(c)/float64(ticks), perMinute, perMinute*60*24)
			// Two ticks a minute at seven calls each. (The old rate: 140.)
			if perMinute > 16 {
				t.Errorf("endpoint %d: %.0f calls a minute with closed payments only; a quiet watcher must stay below 16", i, perMinute)
			}
		}
		if want := minutes * int(time.Minute/simIdle); ticks != want {
			t.Errorf("%d ticks in %d quiet minutes, want %d (one every %s)", ticks, minutes, want, simIdle)
		}
	})

	// One payment that is still open among the closed ones, and the watcher
	// ticks as it always did: this is the rate the test above would have
	// measured before there was a quiet state.
	t.Run("one open payment among them", func(t *testing.T) {
		calls, ticks, st := measure(t, 0, 1)
		if st.Quiet || st.Idle {
			t.Fatalf("at rest with an open address in the watch set: %+v", st)
		}
		for i, c := range calls {
			t.Logf("endpoint %d, 3 closed addresses and an open one: %d calls in %d simulated minutes (%d ticks): %.0f calls/minute, %.0f calls/day",
				i, c, minutes, ticks, float64(c)/minutes, float64(c)/minutes*60*24)
			if perMinute := float64(c) / minutes; perMinute < 120 {
				t.Errorf("endpoint %d: %.0f calls a minute with an open payment; the poll interval gives about 140", i, perMinute)
			}
		}
		if want := minutes * int(time.Minute/simPoll); ticks != want {
			t.Errorf("%d ticks in %d minutes with an open payment, want %d (one every %s)", ticks, minutes, want, simPoll)
		}
	})

	// HMNPAY_IDLE_POLL_INTERVAL=off switches the quiet state off with the
	// idle one.
	t.Run("idle polling off", func(t *testing.T) {
		_, ticks, st := measure(t, -1, 0)
		if st.Quiet || st.Idle || st.Work != "" {
			t.Fatalf("with idle polling off: %+v", st)
		}
		if want := minutes * int(time.Minute/simPoll); ticks != want {
			t.Errorf("%d ticks in %d minutes with idle polling off, want %d (one every %s)", ticks, minutes, want, simPoll)
		}
	})
}

// TestLateFundsDuringQuiet: money arrives at the addresses of closed payments
// in the middle of a quiet pause — one transfer early in the pause, one near
// its end. Nobody wakes the watcher — nobody knows — and the pause runs its
// course; the tick that ends it is a full tick, scans every block of the
// pause for every watched address on both endpoints, and reports both: the
// early one 24 seconds after it was mined, which is what quiet costs, and
// never more than the idle interval. From that tick on the watcher keeps its
// poll interval, so the consumer sees the confirmations of the transfer that
// is not final yet as closely as those of any payment; when it is final and
// has left the reorg window, the watcher is quiet again.
func TestLateFundsDuringQuiet(t *testing.T) {
	const confirmations = 12
	h, s, closed := quietWatcher(t, 3)
	holdPending(h, confirmations)

	var early, late evm.Hash
	var earlyMined, lateMined time.Duration
	var earlyBlock, lateBlock uint64
	s.in(5*time.Second, func() {
		for _, n := range h.nodes {
			n.ResetCounters()
		}
		early = h.pay(closed[1], 40)
	})
	s.in(6*time.Second, func() { earlyMined, earlyBlock = s.now, h.chain.Head().Number })
	s.in(26*time.Second, func() { late = h.pay(closed[2], 15) })
	s.in(27*time.Second, func() { lateMined, lateBlock = s.now, h.chain.Head().Number })
	// What each endpoint was asked for logs by the tick that found them.
	var asked [2][]evmtest.LogQuery
	var found *simTick
	s.onTick = func(st watcher.Status) {
		if found == nil && st.Tracked > 0 {
			tk := s.ticks[len(s.ticks)-1]
			found = &tk
			for i, n := range h.nodes {
				asked[i] = n.LogQueries()
			}
		}
	}
	s.run(simIdle + 3*time.Minute)

	if found == nil || found.st.Tracked != 2 {
		t.Fatalf("the transfers to closed addresses were not reported by one tick: %+v", h.w.Status())
	}
	for tx, block := range map[evm.Hash]uint64{early: earlyBlock, late: lateBlock} {
		if tr, ok := findTransfer(h, tx); !ok || tr.BlockNumber != block {
			t.Fatalf("reported transfer %+v, want the one in block %d", tr, block)
		}
	}
	// The pause they were mined in was not cut short, and the tick that
	// ended it is the one that reported them.
	for _, p := range s.pauses {
		if p.cut {
			t.Fatalf("a pause was cut short, and nobody woke the watcher: %+v", p)
		}
	}
	start := s.ticks[0] // the tick the quiet pause followed
	if found.at != start.at+simIdle || found.at-earlyMined > simIdle {
		t.Fatalf("mined at %s, reported at %s; the quiet pause began at %s and lasts %s", earlyMined, found.at, start.at, simIdle)
	}
	t.Logf("late transfers mined %s and %s into a quiet pause of %s, reported %s and %s after they were mined",
		earlyMined-start.at, lateMined-start.at, simIdle, found.at-earlyMined, found.at-lateMined)

	// A full tick: both endpoints were asked for the logs of every block
	// from the old cursor to the new one, with every watched address.
	if found.st.Cursor <= start.st.Cursor || earlyBlock <= start.st.Cursor {
		t.Fatalf("test setup: cursor %d -> %d, transfer in block %d", start.st.Cursor, found.st.Cursor, earlyBlock)
	}
	for i := range asked {
		if !covers(asked[i], start.st.Cursor+1, found.st.Cursor, len(closed)) {
			t.Errorf("endpoint %d was not asked for the logs of every block %d..%d with all %d addresses: %+v",
				i, start.st.Cursor+1, found.st.Cursor, len(closed), asked[i])
		}
	}
	if found.st.Verifier == "" || found.st.LogVerifier == "" || found.st.LogsUnchecked {
		t.Errorf("the tick that found them was not cross-checked: %+v", found.st)
	}

	// Followed at the normal interval: from that tick until the transfers
	// are final for the consumer and out of the reorg window, every pause is
	// the poll interval — and the reason is named.
	quietAgain := time.Duration(-1)
	for _, tk := range s.ticks {
		if tk.at < found.at {
			continue
		}
		switch {
		case tk.st.Idle && quietAgain < 0:
			quietAgain = tk.at
			if !tk.st.Quiet {
				t.Fatalf("idle but not quiet with closed addresses watched: %+v", tk.st)
			}
		case !tk.st.Idle && quietAgain >= 0:
			t.Fatalf("at work again at %s without anything new: %s", tk.at, tk.st.Work)
		case !tk.st.Idle && !strings.Contains(tk.st.Work, "transfer(s)"):
			t.Fatalf("tick at %s: at work because %q; want the transfers named", tk.at, tk.st.Work)
		}
	}
	if quietAgain < 0 {
		t.Fatalf("the watcher did not become quiet again: %+v", h.w.Status())
	}
	for _, p := range s.pauses {
		if p.at >= found.at && p.at < quietAgain && (p.idle || p.want != simPoll) {
			t.Fatalf("pause %+v while the late transfers are not final: want the poll interval", p)
		}
	}
	// The consumer had the confirmations of the transfer that was not final
	// yet as fast as the chain produced them: the processed head reached
	// block+confirmations-1 within a poll interval of that block being a
	// settle depth deep.
	final := lateMined + (confirmations-1+testSettle)*time.Second
	if final <= found.at {
		t.Fatalf("test setup: the late transfer was final (%s) when it was found (%s)", final, found.at)
	}
	var confirmedAt time.Duration
	for _, tk := range s.ticks {
		if confirmedAt == 0 && tk.st.Processed.Number+1 >= lateBlock+confirmations {
			confirmedAt = tk.at
		}
	}
	if confirmedAt < final || confirmedAt > final+simPoll {
		t.Fatalf("the transfer had its %d confirmations at %s; the chain gave them at %s", confirmations, confirmedAt, final)
	}
	// Out of the reorg window (40 blocks, one a second), and quiet.
	if gone := lateMined + testWindow*time.Second; quietAgain < gone || quietAgain > gone+2*simPoll {
		t.Fatalf("quiet again at %s; the last transfer leaves the reorg window at %s", quietAgain, gone)
	}
	h.assertModel(startBlock)
}

// TestLateFundsAfterAPauseLongerThanTheReorgWindow: with an idle interval of
// two minutes (the setting goes up to five) a quiet pause covers more blocks
// than the watcher remembers transfers for. A transfer mined early in such a
// pause is reported by the tick that ends it — and is, in that tick,
// already out of the reorg window: nothing is tracked when the tick ends.
// The watcher must not go back to sleep on that. A tick that reported a
// transfer is followed by one a poll interval later, which asks the consumer
// whether it holds the transfer as final.
func TestLateFundsAfterAPauseLongerThanTheReorgWindow(t *testing.T) {
	const idle = 2 * time.Minute
	h, s, closed := quietWatcher(t, 2, withConfig(func(c *watcher.Config) { c.IdleInterval = idle }))
	s.run(idle) // into a pause
	s.reset()
	var tx evm.Hash
	var block uint64
	s.in(4*time.Second, func() { tx = h.pay(closed[0], 7) })
	s.in(5*time.Second, func() { block = h.chain.Head().Number })
	s.run(idle + 20*time.Second)

	tr, ok := findTransfer(h, tx)
	if !ok || tr.BlockNumber != block {
		t.Fatalf("the transfer mined early in a long quiet pause was not reported: %+v", h.w.Status())
	}
	if len(s.ticks) < 3 {
		t.Fatalf("ticks: %+v", s.ticks)
	}
	start, found := s.ticks[0], s.ticks[1] // the ticks before and after the pause
	if found.at != start.at+idle || found.st.Cursor-start.st.Cursor != uint64(idle/time.Second) || block <= start.st.Cursor {
		t.Fatalf("the pause did not run its course: ticks at %s and %s, cursor %d -> %d, transfer in block %d",
			start.at, found.at, start.st.Cursor, found.st.Cursor, block)
	}
	if block+testWindow >= found.st.Tip.Number {
		t.Fatalf("test setup: the transfer in block %d is not a reorg window below the tip %d", block, found.st.Tip.Number)
	}
	if found.st.Tracked != 0 {
		t.Fatalf("test setup: the transfer is still tracked: %+v", found.st)
	}
	if found.st.Idle || !strings.Contains(found.st.Work, "reported in the last tick") {
		t.Fatalf("the tick that reported a transfer left the watcher at rest: idle=%v quiet=%v work=%q", found.st.Idle, found.st.Quiet, found.st.Work)
	}
	if next := s.ticks[2]; next.at != found.at+simPoll || !next.st.Quiet {
		t.Fatalf("the tick after it: at %s (want %s later), %+v", next.at-found.at, simPoll, next.st)
	}
	h.assertModel(startBlock)
}

// TestQuietWatcherIsWoken: what ends a quiet pause at once.
func TestQuietWatcherIsWoken(t *testing.T) {
	// woken asserts that the pause running at `at` was cut short at that
	// moment and that a tick ran then; it returns that tick.
	woken := func(t *testing.T, s *simulation, at time.Duration) simTick {
		t.Helper()
		var cut *simPause
		for i := range s.pauses {
			if p := &s.pauses[i]; p.cut {
				if cut != nil {
					t.Fatalf("two pauses were cut short: %+v and %+v", *cut, *p)
				}
				cut = p
			}
		}
		if cut == nil || !cut.idle || cut.want != simIdle || cut.at+cut.took != at {
			t.Fatalf("the quiet pause was not ended by the wake-up at %s: %+v\npauses: %+v", at, cut, s.pauses)
		}
		tk, ok := s.tickAt(at)
		if !ok || tk.at != at {
			t.Fatalf("no tick at the moment of the wake-up (%s): %+v", at, tk)
		}
		return tk
	}

	// A payment is created: its address — not closed — is in the watch set,
	// with the consumer's head as its start block, and the watcher is told.
	t.Run("by a payment that is created", func(t *testing.T) {
		h, s, closed := quietWatcher(t, 3)
		var created, mined time.Duration
		var tx evm.Hash
		s.in(11*time.Second, func() {
			h.watch.addSince(addr(1), h.sink.lastHead().Number)
			created = s.now
			h.w.Wake()
		})
		s.in(12*time.Second, func() { tx = h.pay(addr(1), 25) })
		s.in(13*time.Second, func() { mined = s.now })
		s.run(time.Minute)
		tk := woken(t, s, created)
		if tk.st.Idle || tk.st.Quiet || tk.st.WatchedAddresses != len(closed)+1 || !strings.Contains(tk.st.Work, "1 of the 4 watched address(es) are not closed") {
			t.Fatalf("the wake-up tick: %+v", tk.st)
		}
		var observedAt time.Duration
		for _, tk := range s.ticks {
			if tk.at >= mined && observedAt == 0 && tk.st.Tracked == 1 {
				observedAt = tk.at
			}
		}
		if _, ok := findTransfer(h, tx); !ok || observedAt == 0 || observedAt-mined > simPoll {
			t.Fatalf("the payment was mined at %s and reported at %s: more than a poll interval (%s) later", mined, observedAt, simPoll)
		}
		for _, p := range s.pauses {
			if p.at >= created && (p.idle || p.want != simPoll) {
				t.Fatalf("a quiet pause while a payment is open: %+v", p)
			}
		}
		h.assertModel(startBlock)
	})

	// An address that was closed is not closed any more.
	t.Run("by a closed address that is open again", func(t *testing.T) {
		h, s, closed := quietWatcher(t, 3)
		var at time.Duration
		s.in(9*time.Second, func() {
			h.watch.reopen(closed[2])
			at = s.now
			h.w.Wake()
		})
		s.run(40 * time.Second)
		if tk := woken(t, s, at); tk.st.Idle || tk.st.Quiet || !strings.Contains(tk.st.Work, "not closed") {
			t.Fatalf("the wake-up tick: %+v", tk.st)
		}
		for _, p := range s.pauses {
			if p.at >= at && p.idle {
				t.Fatalf("a quiet pause while an address is open: %+v", p)
			}
		}
		// Closed again, and quiet again — by the next tick: no wake-up is
		// needed to go to rest.
		h.watch.close(closed[2])
		s.run(10 * time.Second)
		if st := h.w.Status(); !st.Quiet {
			t.Fatalf("not quiet after the address was closed again: %s", st.Work)
		}
	})

	t.Run("by a re-scan", func(t *testing.T) {
		h, s, _ := quietWatcher(t, 2)
		from := h.w.Status().Cursor - 20
		var at time.Duration
		s.in(7*time.Second, func() {
			at = s.now
			h.w.Rescan(from)
		})
		s.run(20 * time.Second)
		woken(t, s, at)
		if !strings.Contains(h.logs.String(), "re-scan requested") {
			t.Fatal("the re-scan was not carried out")
		}
		if st := h.w.Status(); !st.Quiet {
			t.Fatalf("not quiet after the re-scan: %s", st.Work)
		}
	})

	// The wake-up arrives while a quiet tick is running: after the tick has
	// read the watch set, before it has ended. That tick ends quiet — it saw
	// closed addresses only — and the next one must follow at once.
	t.Run("during a quiet tick", func(t *testing.T) {
		h, s, closed := quietWatcher(t, 2)
		armed, fired := false, time.Duration(-1)
		rule := h.sink.headRule
		h.sink.set(func(sk *sink) {
			sk.headRule = func(head watcher.Head, live map[key]watcher.Transfer) {
				rule(head, live)
				if armed && fired < 0 {
					fired = s.now
					h.watch.addSince(addr(1), head.Number)
					h.w.Wake()
				}
			}
		})
		s.in(time.Second, func() { armed = true })
		s.run(40 * time.Second)
		if fired < 0 {
			t.Fatal("no quiet tick reported a head")
		}
		var at []simTick
		for _, tk := range s.ticks {
			if tk.at == fired {
				at = append(at, tk)
			}
		}
		if len(at) != 2 || !at[0].st.Quiet || at[0].st.WatchedAddresses != len(closed) || at[1].st.Idle || at[1].st.WatchedAddresses != len(closed)+1 {
			t.Fatalf("want two ticks at %s — the quiet one, and at once the one that sees the open address; got %+v", fired, at)
		}
		for _, p := range s.pauses {
			if p.at >= fired && p.idle {
				t.Fatalf("a quiet pause after the wake-up: %+v", p)
			}
		}
	})

	// The wake-up is an optimisation here as it is for the idle watcher:
	// the watch set is read on every tick, and a payment nobody woke the
	// watcher for is found by the tick that ends the pause.
	t.Run("by nobody: the payment is found when the pause ends", func(t *testing.T) {
		h, s, closed := quietWatcher(t, 2)
		var tx evm.Hash
		var created time.Duration
		s.in(5*time.Second, func() {
			h.watch.addSince(addr(1), h.sink.lastHead().Number)
			created = s.now
			tx = h.pay(addr(1), 40)
		})
		s.run(simIdle + 10*time.Second)
		tk, ok := s.tickAt(created)
		if !ok || tk.at-created >= simIdle || tk.st.Tracked != 1 || tk.st.WatchedAddresses != len(closed)+1 {
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
	})
}

// TestWhatPreventsQuiet: closed addresses in the watch set do not make the
// watcher quiet while anything else is going on — and the status says what.
func TestWhatPreventsQuiet(t *testing.T) {
	noRest := func(t *testing.T, s *simulation, since time.Duration, want string) {
		t.Helper()
		n := 0
		for _, tk := range s.ticks {
			if tk.at < since {
				continue
			}
			n++
			if tk.st.Idle || tk.st.Quiet || !strings.Contains(tk.st.Work, want) {
				t.Fatalf("tick at %s: idle=%v quiet=%v work=%q; want work containing %q", tk.at, tk.st.Idle, tk.st.Quiet, tk.st.Work, want)
			}
		}
		if n == 0 {
			t.Fatalf("no tick since %s", since)
		}
		for _, p := range s.pauses {
			if p.at >= since && (p.idle || p.want >= simIdle) {
				t.Fatalf("a quiet pause with work pending: %+v", p)
			}
		}
	}

	t.Run("an open payment among closed ones", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		closedAddresses(h, 5)
		h.watch.addSince(addr(1), h.chain.Head().Number-testSettle)
		s := newSimulation(h)
		s.run(2 * time.Minute)
		noRest(t, s, 0, "1 of the 6 watched address(es) are not closed")
		// It closes (paid long ago, say): quiet from the next tick.
		h.watch.close(addr(1))
		s.reset()
		s.run(10 * time.Second)
		if st := h.w.Status(); !st.Quiet || st.WatchedAddresses != 6 {
			t.Fatalf("not quiet with six closed addresses: %+v", st)
		}
	})

	t.Run("a transfer the consumer holds as not final", func(t *testing.T) {
		h, s, closed := quietWatcher(t, 2)
		tx := h.pay(closed[0], 5)
		s.run(simIdle + time.Second)
		tr, found := findTransfer(h, tx)
		if !found {
			t.Fatal("the late transfer was not reported")
		}
		// The consumer wants many confirmations: it lists the transfer as
		// pending long after the watcher's own reorg window has passed.
		h.watch.setPending(tr)
		s.reset()
		since := s.now
		s.run(3 * time.Minute)
		noRest(t, s, since, "transfer(s)")
		if st := h.w.Status(); st.Tracked != 1 {
			t.Fatalf("the pending transfer is not tracked: %+v", st)
		}
		h.watch.setPending()
		s.run(time.Minute)
		if st := h.w.Status(); !st.Quiet || st.Tracked != 0 {
			t.Fatalf("not quiet after the consumer let the transfer go: %+v", st)
		}
	})

	t.Run("a transfer inside the reorg window", func(t *testing.T) {
		h, s, closed := quietWatcher(t, 2)
		h.pay(closed[0], 5)
		s.run(simIdle + time.Second) // found by the tick that ends the pause
		s.reset()
		since := s.now
		s.run(testWindow*time.Second - simIdle)
		noRest(t, s, since, "inside the reorg window")
	})

	t.Run("a re-scan that is under way", func(t *testing.T) {
		h, s, _ := quietWatcher(t, 2)
		var asked time.Duration
		s.in(4*time.Second, func() {
			asked = s.now
			h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
			h.w.Rescan(h.w.Status().Cursor - 20)
		})
		s.run(40 * time.Second)
		for _, p := range s.pauses {
			if p.at >= asked && (p.idle || p.want >= simIdle) {
				t.Fatalf("a quiet pause with a re-scan under way: %+v", p)
			}
		}
		if st := h.w.Status(); st.Idle || st.Quiet || st.Work == "" {
			t.Fatalf("at rest with a re-scan under way and no second endpoint: %+v", st)
		}
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
		h.passCooldowns()
		s.run(time.Minute)
		if st := h.w.Status(); !st.Quiet {
			t.Fatalf("not quiet after the re-scan was completed: %s", st.Work)
		}
	})

	t.Run("a head that is held", func(t *testing.T) {
		h, s, _ := quietWatcher(t, 2)
		var down time.Duration
		s.in(4*time.Second, func() {
			down = s.now
			h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
		})
		s.run(3 * time.Minute)
		// Nobody woke the watcher: the pause that was running ends by
		// itself, and from the tick that finds the endpoint gone the
		// watcher keeps its poll interval and says what it is waiting for.
		found, ok := s.tickAt(down)
		if !ok || found.at-down > simIdle {
			t.Fatalf("the outage at %s was not noticed within an idle interval: %+v", down, found)
		}
		noRest(t, s, found.at, "second endpoint")
		if st := h.w.Status(); st.Hold == "" {
			t.Fatalf("the head is not held: %+v", st)
		}
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
		h.passCooldowns()
		s.run(time.Minute)
		if st := h.w.Status(); !st.Quiet || st.Hold != "" {
			t.Fatalf("not quiet after the endpoint came back: %+v", st)
		}
	})

	t.Run("single-source operation", func(t *testing.T) {
		h, s, _ := quietWatcher(t, 2, withConfig(func(c *watcher.Config) { c.Verify = watcher.VerifyBestEffort }))
		var down time.Duration
		s.in(4*time.Second, func() {
			down = s.now
			h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
		})
		s.run(3 * time.Minute)
		found, _ := s.tickAt(down)
		for _, p := range s.pauses {
			if p.at >= found.at && (p.idle || p.want >= simIdle) {
				t.Fatalf("a quiet pause on one endpoint's word: %+v", p)
			}
		}
		if st := h.w.Status(); !st.Solo || st.Idle || st.Quiet || !strings.Contains(st.Work, "single-source") {
			t.Fatalf("want single-source operation, not quiet: %+v", st)
		}
	})

	// Seed 826 of the randomised model, in the first soak with quiet
	// stretches (30 000 seeds on 22ca568), which ended with
	//
	//	IDLE WATCHDOG: idle at block 10218 while endpoint 1 shows the chain at block 11224
	//
	// An endpoint adds a thousand to its eth_blockNumber and tells the truth
	// in everything else. The two heads agree, the cursor is in
	// step with them, and the claim settles nothing — rightly. But nobody
	// asks an endpoint for its block number while nothing is watched; a
	// quiet tick does, with every log query, and then rested on a chain of
	// which one endpoint had just said that it goes on for a thousand
	// blocks. The watcher does not rest on that: whoever is right, there is
	// a disagreement about where the chain ends, and thirty seconds are a
	// long time to wait for the next look.
	for i, who := range []string{"the primary", "the endpoint that confirms the log scans"} {
		t.Run("a longer chain claimed by "+who+" with its log answers", func(t *testing.T) {
			h, s, _ := quietWatcher(t, 2)
			var since time.Duration
			s.in(4*time.Second, func() {
				since = s.now
				h.nodes[i].Set(func(b *evmtest.Behavior) {
					b.FinalityDepth = 64
					b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
						if method == "eth_blockNumber" {
							return hexUint(h.chain.Head().Number + 1000), nil, true
						}
						return nil, nil, false
					}
				})
			})
			s.run(3 * time.Minute)
			// The quiet pause that was running ends by itself; the tick
			// that ends it is shown the longer chain, and from there the
			// watcher keeps its poll interval and names the endpoint.
			found, ok := s.tickAt(since)
			if !ok || found.at-since > simIdle {
				t.Fatalf("no tick within an idle interval of %s: %+v", since, found)
			}
			noRest(t, s, found.at, evm.RedactURL(h.urls[i])+" shows the chain at block")
			// Nothing was settled on the claim, and the head is not held:
			// the cursor follows the chain both endpoints show.
			if st := h.w.Status(); st.Hold != "" || st.Cursor+testSettle != st.Tip.Number || st.LastError != "" {
				t.Fatalf("status while one endpoint claims a longer chain: %+v", st)
			}
			h.nodes[i].Set(func(b *evmtest.Behavior) { b.Intercept = nil })
			s.reset()
			s.run(10 * time.Second)
			if st := h.w.Status(); !st.Quiet {
				t.Fatalf("not quiet after the endpoint stopped: %s", st.Work)
			}
		})
	}

	// Blocks settled on one endpoint's logs wait for a second endpoint to
	// scan them, as far as a watched address can have been paid in them —
	// and a closed address can. With two endpoints of which one serves no
	// logs, a watcher with closed addresses is therefore never quiet: the
	// other endpoint may begin to serve logs at any moment. With a single
	// endpoint configured there is no second one to wait for, and the
	// watcher is quiet.
	t.Run("an unchecked range, with a second endpoint that may come to serve logs", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		closedAddresses(h, 2)
		h.noLogService(1, &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"})
		h.mineTicking(3)
		if st := h.w.Status(); st.UncheckedFrom == 0 || !st.LogsUnchecked {
			t.Fatalf("test setup: no blocks were settled on one endpoint's logs: %+v", st)
		}
		s := newSimulation(h)
		s.run(2 * time.Minute)
		noRest(t, s, 0, "settled on one endpoint's logs")

		// The second endpoint gets a log service: the unchecked blocks are
		// scanned by it, the record is closed, and the watcher is quiet.
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError = nil })
		h.passLogPause()
		s.reset()
		s.run(2 * time.Minute)
		if st := h.w.Status(); !st.Quiet || st.UncheckedFrom != 0 || st.LogsUnchecked {
			t.Fatalf("not quiet after a second endpoint scanned the unchecked blocks: %+v", st)
		}
	})

	t.Run("an unchecked range with a single endpoint does not", func(t *testing.T) {
		h := newHarness(t)
		closed := closedAddresses(h, 2)
		s := newSimulation(h)
		s.run(2 * time.Minute)
		st := h.w.Status()
		if !st.Quiet || st.UncheckedFrom == 0 || !st.LogsUnchecked {
			t.Fatalf("a single-endpoint watcher with closed addresses only: quiet=%v work=%q unchecked=%d..%d", st.Quiet, st.Work, st.UncheckedFrom, st.UncheckedTo)
		}
		// Late funds are found by its quiet ticks all the same.
		s.reset()
		tx := h.pay(closed[1], 3)
		s.run(simIdle + time.Second)
		if _, ok := findTransfer(h, tx); !ok {
			t.Fatalf("the late transfer was not reported by a single-endpoint watcher: %+v", h.w.Status())
		}
		h.assertModel(startBlock)
	})
}

// TestReorgAcrossQuiet: the chain is reorganised during a quiet pause.
func TestReorgAcrossQuiet(t *testing.T) {
	for _, idle := range []time.Duration{simIdle, 2 * time.Minute} {
		name := fmt.Sprintf("idle interval %s", idle)
		if idle > testWindow*time.Second {
			name += ", longer than the reorg window"
		}
		// Deeper than the settle depth: the block the cursor rests on is
		// replaced, and the new fork includes a transfer to a closed address
		// at a height the watcher had already settled. The first tick after
		// the pause must not scan on from the cursor: it finds, with both
		// endpoints, that the remembered block is gone, goes back to the
		// newest block that is still there, and scans from there — with the
		// closed addresses, like every scan.
		t.Run(name+"/late funds on the new fork, at a height already settled", func(t *testing.T) {
			h, s, closed := quietWatcher(t, 2, withConfig(func(c *watcher.Config) { c.IdleInterval = idle }))
			// A few quiet ticks, so that more than one block is remembered.
			s.run(3 * idle)
			s.reset()
			var before watcher.Status
			var old evmtest.Block
			var tx evm.Hash
			var reorgAt time.Duration
			s.in(5*time.Second, func() {
				before = h.w.Status()
				old, _ = h.chain.BlockByNumber(before.Cursor)
				h.chain.Reorg(int(h.chain.Head().Number-before.Cursor) + 4)
				tx = h.pay(closed[0], 33) // into the next block of the new fork
				reorgAt = s.now
			})
			s.run(idle + 10*time.Second)

			if blk, ok := h.chain.BlockByNumber(before.Cursor); !ok || blk.Hash == old.Hash {
				t.Fatalf("the test did not replace the cursor's block %d (chain at %d)", before.Cursor, h.chain.Head().Number)
			}
			tr, found := findTransfer(h, tx)
			if !found {
				t.Fatalf("the transfer on the new fork was not reported: %+v\n%s", h.w.Status(), tail(h.logs.String(), 3000))
			}
			if tr.BlockNumber > before.Cursor {
				t.Fatalf("test setup: the transfer is in block %d, above the old cursor %d", tr.BlockNumber, before.Cursor)
			}
			st := h.w.Status()
			if st.Reorgs != before.Reorgs+1 || st.DeepReorgs != 0 {
				t.Fatalf("reorganisations counted: %d (deep: %d), want %d and none", st.Reorgs, st.DeepReorgs, before.Reorgs+1)
			}
			if log := h.logs.String(); strings.Contains(log, "chain watcher incident") {
				t.Fatalf("an incident was raised:\n%s", tail(log, 3000))
			}
			// Found by the first tick after the pause — which nobody cut
			// short — and that tick did not leave the watcher at rest.
			tk, ok := s.tickAt(reorgAt)
			if !ok || tk.at != s.ticks[0].at+idle || tk.st.Idle || tk.st.Quiet || tk.st.Reorgs != before.Reorgs+1 {
				t.Fatalf("the first tick after the reorganisation (at %s): %+v", reorgAt, tk)
			}
			if head := h.sink.lastHead(); head.Number != st.Cursor || st.Cursor+testSettle != st.Tip.Number {
				t.Fatalf("the watcher is not in step with the new fork: head %d, %+v", head.Number, st)
			}
			h.assertModel(startBlock)
		})

		// A late transfer is mined during the pause, and its block is
		// reorganised away before the watcher has looked. What the tick
		// after the pause reports is what the chain holds then: nothing, if
		// the transaction did not come back — the transfer in the new block,
		// once, if it did.
		for _, remined := range []bool{false, true} {
			sub := "/a late transfer whose block is replaced before the next tick"
			if remined {
				sub += ", and that is mined again"
			}
			t.Run(name+sub, func(t *testing.T) {
				h, s, closed := quietWatcher(t, 2, withConfig(func(c *watcher.Config) { c.IdleInterval = idle }))
				var tx evm.Hash
				var first evmtest.Block
				s.in(5*time.Second, func() { tx = h.pay(closed[1], 21) })
				s.in(6*time.Second, func() { first = h.chain.Head() })
				s.in(9*time.Second, func() {
					dropped := h.chain.Reorg(int(h.chain.Head().Number-first.Number) + 1)
					if len(dropped) != 1 || dropped[0].Hash != tx {
						t.Fatalf("test setup: dropped %+v", dropped)
					}
					if remined {
						h.chain.Requeue(dropped...)
					}
				})
				s.run(idle + 10*time.Second)
				tr, found := findTransfer(h, tx)
				if found != remined {
					t.Fatalf("reported=%v, mined again=%v: %+v", found, remined, tr)
				}
				if remined && tr.BlockHash == first.Hash {
					t.Fatalf("test setup: the transfer was reported in its first block %d (%s), which was replaced", first.Number, first.Hash)
				}
				if n := h.sink.count("observed"); (remined && n != 1) || (!remined && n != 0) {
					t.Fatalf("%d transfers were reported", n)
				}
				if n := h.sink.count("reverted"); n != 0 {
					t.Fatalf("%d reverts were reported for a block the watcher never looked at", n)
				}
				h.assertModel(startBlock)
			})
		}
	}
}

// TestCursorBlockIsRememberedAcrossQuiet is TestCursorBlockIsRememberedHowever
// TheCursorGotThere with the watcher at rest on that block for a quiet pause:
// the cursor was put on a block without anything being settled — the start
// position of a watcher without a checkpoint, the position a checkpoint below
// everything is moved up to — the tick that put it there left the watcher
// quiet, and during the pause the chain is reorganised below it. The block
// that replaces the cursor's holds a transfer to a closed address. Had the
// cursor's block not been remembered, the tick after the pause would scan on
// above a block that is gone, and pass that transfer by.
func TestCursorBlockIsRememberedAcrossQuiet(t *testing.T) {
	// duringThePause schedules, five seconds into the quiet pause that
	// follows the first tick, a reorganisation below the cursor's block and
	// a transfer to a closed address into the new fork.
	duringThePause := func(t *testing.T, h *harness, s *simulation, to ethaddr.Address) (tx *evm.Hash, cursor *uint64) {
		t.Helper()
		tx, cursor = new(evm.Hash), new(uint64)
		s.in(5*time.Second, func() {
			st := h.w.Status()
			if !st.Quiet || st.Cursor == 0 || h.sink.lastHead().Number != st.Cursor {
				t.Fatalf("test setup: the first tick did not leave the watcher quiet on a reported head: %+v", st)
			}
			*cursor = st.Cursor
			h.chain.Reorg(int(h.chain.Head().Number-st.Cursor) + 2)
			*tx = h.pay(to, 77)
		})
		return tx, cursor
	}
	found := func(t *testing.T, h *harness, tx evm.Hash, cursor uint64) {
		t.Helper()
		tr, ok := findTransfer(h, tx)
		if !ok {
			t.Fatalf("the transfer in the block that replaced the cursor's was never reported: %+v\n%s", h.w.Status(), tail(h.logs.String(), 2500))
		}
		if tr.BlockNumber > cursor {
			t.Fatalf("test setup: the transfer is in block %d, above the old cursor %d", tr.BlockNumber, cursor)
		}
		h.assertModel(startBlock)
		if head, st := h.sink.lastHead(), h.w.Status(); head.Number != st.Cursor || st.Cursor+testSettle != st.Tip.Number {
			t.Fatalf("the watcher is not in step with the new fork: head %d, cursor %d, tip %d", head.Number, st.Cursor, st.Tip.Number)
		}
	}

	t.Run("the start position of a watcher without a checkpoint", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		for _, n := range h.nodes {
			n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = 64 })
		}
		h.chain.Mine(20)
		closed := closedAddresses(h, 2)
		s := newSimulation(h)
		tx, cursor := duringThePause(t, h, s, closed[0])
		s.run(simIdle + 20*time.Second)
		if s.ticks[1].at != s.ticks[0].at+simIdle {
			t.Fatalf("the first tick was not followed by a quiet pause: %+v", s.ticks[:2])
		}
		found(t, h, *tx, *cursor)
		// The one block the watcher remembered is gone, and nothing below it
		// is remembered: that is accepted from two endpoints, loudly.
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
		// window. When it comes back the watch set holds closed addresses
		// that cannot have been paid below the settled height: the new
		// process does not scan its way up to them.
		h.chain.Mine(testWindow + 20)
		closed := closedAddresses(h, 2)
		s := newSimulation(h) // a new watcher on the old checkpoint
		tx, cursor := duringThePause(t, h, s, closed[1])
		s.run(simIdle + 20*time.Second)
		if in.count(watcher.IncidentCheckpointBehind) == 0 {
			t.Fatalf("test setup: the checkpoint was not moved up: %+v", h.w.Status())
		}
		if s.ticks[1].at != s.ticks[0].at+simIdle {
			t.Fatalf("the first tick was not followed by a quiet pause: %+v", s.ticks[:2])
		}
		found(t, h, *tx, *cursor)
		// Blocks below the new position are still remembered, so this is an
		// ordinary reorganisation.
		if st := h.w.Status(); st.DeepReorgs != 0 || st.Reorgs == 0 {
			t.Fatalf("reorganisations = %d, deep = %d; want an ordinary one", st.Reorgs, st.DeepReorgs)
		}
	})
}

// TestQuietStatus: what Status says while the watcher is quiet, how quiet and
// idle are told apart, and that a dead endpoint is noticed within an idle
// interval.
func TestQuietStatus(t *testing.T) {
	t.Run("quiet", func(t *testing.T) {
		h, s, closed := quietWatcher(t, 4)
		s.run(2 * time.Minute)
		for i, tk := range s.ticks {
			st := tk.st
			// Every quiet tick is a full tick: it reached both endpoints,
			// had its log scan confirmed by the second, settled what had
			// become deep enough and reported the head.
			if !st.Idle || !st.Quiet || st.Work != "" || st.Hold != "" || st.LastError != "" || st.Verifier == "" || st.LogVerifier == "" || st.LogsUnchecked {
				t.Fatalf("tick at %s: %+v", tk.at, st)
			}
			if st.WatchedAddresses != len(closed) || st.Tracked != 0 || st.Unverified != 0 {
				t.Fatalf("tick at %s: %+v", tk.at, st)
			}
			if st.Behind != testSettle || st.Processed.Number != st.Cursor || st.Tip.Number != st.Cursor+testSettle {
				t.Fatalf("tick at %s: cursor %d, processed %d, tip %d", tk.at, st.Cursor, st.Processed.Number, st.Tip.Number)
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
			t.Fatalf("%d ticks in two quiet minutes, want 4", len(s.ticks))
		}
		if !strings.Contains(h.logs.String(), "chain watcher quiet: every watched address is closed") {
			t.Fatal("going quiet was not logged")
		}
	})

	// The watch windows end: the list empties, and the watcher is idle and
	// no longer quiet — the same pause, with nothing to scan for.
	t.Run("quiet becomes idle when the watch set empties, and the other way round", func(t *testing.T) {
		h, s, closed := quietWatcher(t, 2)
		for _, a := range closed {
			h.watch.remove(a)
		}
		s.run(simIdle + time.Second)
		for _, p := range s.pauses {
			if !p.idle || p.want != simIdle {
				t.Fatalf("pause %+v: want the idle interval throughout", p)
			}
		}
		if st := h.w.Status(); !st.Idle || st.Quiet || st.WatchedAddresses != 0 {
			t.Fatalf("after the watch set emptied: %+v", st)
		}
		// A closed address appears (a restored backup, say): it needs no
		// wake-up, and makes the idle watcher a quiet one.
		h.watch.addSince(closed[0], h.sink.lastHead().Number)
		h.watch.close(closed[0])
		s.reset()
		s.run(simIdle + time.Second)
		for _, p := range s.pauses {
			if !p.idle || p.want != simIdle || p.cut {
				t.Fatalf("pause %+v: want the idle interval throughout", p)
			}
		}
		if st := h.w.Status(); !st.Idle || !st.Quiet || st.WatchedAddresses != 1 {
			t.Fatalf("with a closed address in the watch set: %+v", st)
		}
	})

	t.Run("the first tick of a process", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		closedAddresses(h, 3)
		s := newSimulation(h)
		s.run(simIdle - time.Second)
		if len(s.ticks) != 1 || !s.ticks[0].st.Quiet || !s.ticks[0].st.Idle {
			t.Fatalf("ticks: %+v", s.ticks)
		}
	})

	t.Run("an endpoint that dies is noticed within an idle interval", func(t *testing.T) {
		h, s, _ := quietWatcher(t, 2)
		var down time.Duration
		s.in(2*time.Second, func() {
			down = s.now
			for _, n := range h.nodes {
				n.Set(func(b *evmtest.Behavior) { b.Down = true })
			}
		})
		s.run(2 * time.Minute)
		tk, ok := s.tickAt(down)
		if !ok || tk.at-down > simIdle || tk.st.LastError == "" || tk.st.Idle || tk.st.Quiet {
			t.Fatalf("the outage at %s: first tick after it %+v", down, tk)
		}
		if st := h.w.Status(); st.Idle || st.Quiet || st.ConsecutiveErrors < 3 || st.Work != "the last tick failed" {
			t.Fatalf("status during the outage: %+v", st)
		}
		for _, n := range h.nodes {
			n.Set(func(b *evmtest.Behavior) { b.Down = false })
		}
		h.passCooldowns()
		s.run(2 * time.Minute)
		if st := h.w.Status(); !st.Quiet || st.ConsecutiveErrors != 0 {
			t.Fatalf("status after the outage: %+v", st)
		}
	})
}
