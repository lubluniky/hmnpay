package watcher_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// deepFinality makes the nodes report finality far enough behind the head
// that the multi-block reorgs of a scenario do not contradict it. (On BSC,
// blocks are final one or two blocks behind the head, and a deeper reorg is
// by definition a finality violation — tested separately.)
func deepFinality(h *harness) {
	for _, n := range h.nodes {
		n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = 30 })
	}
}

func TestStraightPayment(t *testing.T) {
	h := newHarness(t)
	deposit := addr(1)
	h.watch.add(deposit)

	// First tick: nothing to report but the head. Scanning starts just
	// below the tip.
	h.tick()
	if got := kinds(h.sink.take()); !reflect.DeepEqual(got, []string{"head"}) {
		t.Fatalf("first tick events = %v", got)
	}
	st := h.w.Status()
	if st.Cursor != startBlock-testSettle || st.Tip.Number != startBlock || st.Behind != testSettle || st.WatchedAddresses != 1 {
		t.Fatalf("status after first tick = %+v", st)
	}

	// The payer sends 25 USDT; it is mined in the next block.
	tx := h.pay(deposit, 25)
	blk := h.chain.Mine(1)
	h.tick()
	events := h.sink.take()
	if got := kinds(events); !reflect.DeepEqual(got, []string{"observed", "head"}) {
		t.Fatalf("events = %v", got)
	}
	tr := events[0].tr
	if tr.TxHash != tx || tr.To != deposit || tr.From != payer || tr.Token != usdt || tr.Amount.Cmp(units(25)) != 0 ||
		tr.BlockNumber != blk.Number || tr.BlockHash != blk.Hash || tr.LogIndex != 0 {
		t.Fatalf("transfer = %+v", tr)
	}
	// The head that is reported is the processed one: SettleDepth below the
	// tip, with that block's own hash and timestamp.
	settled, _ := h.chain.BlockByNumber(blk.Number - testSettle)
	if got := events[1].head; got.Number != settled.Number || got.Hash != settled.Hash || got.Time.Unix() != int64(settled.Time) {
		t.Fatalf("head = %+v, want block %d", got, settled.Number)
	}
	// The receipt was checked before reporting.
	if h.nodes[0].Calls("eth_getTransactionReceipt") != 1 {
		t.Errorf("receipt lookups = %d, want 1", h.nodes[0].Calls("eth_getTransactionReceipt"))
	}

	// Confirmations accumulate: one head per tick, the transfer is not
	// repeated, and each tick costs exactly one eth_getLogs.
	for i := 0; i < 10; i++ {
		before := h.nodes[0].Calls("eth_getLogs")
		h.chain.Mine(3)
		h.tick()
		if got := kinds(h.sink.take()); !reflect.DeepEqual(got, []string{"head"}) {
			t.Fatalf("tick %d events = %v", i, got)
		}
		if n := h.nodes[0].Calls("eth_getLogs") - before; n != 1 {
			t.Fatalf("tick %d used %d eth_getLogs calls, want 1", i, n)
		}
	}
	if h.nodes[0].Calls("eth_getTransactionReceipt") != 1 {
		t.Errorf("transfer was re-verified: %d receipt lookups", h.nodes[0].Calls("eth_getTransactionReceipt"))
	}
	head := h.chain.Head().Number
	st = h.w.Status()
	if st.Cursor != head-testSettle || st.Tracked != 1 || st.LastError != "" || st.Unverified != 0 {
		t.Fatalf("status = %+v", st)
	}
	if got := h.sink.lastHead(); got.Number != head-testSettle || got.Finalized != head-2 {
		t.Fatalf("last head = %+v (processed %d below the tip, finalized 2 below)", got, testSettle)
	}
	if st.Processed.Number != head-testSettle || st.Tip.Number != head {
		t.Fatalf("status = %+v", st)
	}
	h.assertModel(startBlock)

	// A tick without a new block reports nothing at all.
	h.tick()
	if events := h.sink.take(); len(events) != 0 {
		t.Fatalf("idle tick produced %v", events)
	}

	// Once the transfer leaves the reorg window it is forgotten.
	h.chain.Mine(testWindow)
	h.tick()
	if st := h.w.Status(); st.Tracked != 0 {
		t.Fatalf("tracked = %d after the window passed", st.Tracked)
	}
}

func TestManyAddresses(t *testing.T) {
	h := newHarness(t)
	const n = 2300
	for i := 0; i < n; i++ {
		h.watch.add(addr(i))
	}
	h.tick()
	h.sink.take()

	// Payments to a spread of addresses over several blocks, some in the
	// same block and the same transaction.
	for i := 0; i < 60; i++ {
		h.pay(addr((i*37)%n), int64(1+i))
		if i%4 == 0 {
			h.chain.Submit(
				evmtest.TransferLog(usdt, payer, addr((i*91)%n), units(2)),
				evmtest.TransferLog(usdt, payer, addr((i*53)%n), units(3)),
			)
		}
		if i%3 == 0 {
			h.chain.Mine(1)
		}
	}
	h.chain.Mine(1)
	h.nodes[0].ResetCounters()
	h.tick()
	h.assertModel(startBlock)
	if got := len(h.sink.liveKeys()); got != 60+15*2 {
		t.Fatalf("%d transfers reported, want %d", got, 60+15*2)
	}
	// One eth_getLogs per 500 addresses — not one per address — and the
	// receipts are fetched in batches.
	if calls := h.nodes[0].Calls("eth_getLogs"); calls != 5 {
		t.Errorf("eth_getLogs calls for %d addresses = %d, want 5", n, calls)
	}
	if reqs := h.nodes[0].Requests(); reqs > 5+1+10 {
		t.Errorf("%d HTTP requests in one tick", reqs)
	}
	// Events arrive in chain order.
	var last watcher.Transfer
	for _, e := range h.sink.take() {
		if e.kind != "observed" {
			continue
		}
		if e.tr.BlockNumber < last.BlockNumber || (e.tr.BlockNumber == last.BlockNumber && e.tr.LogIndex <= last.LogIndex && last.Amount != nil) {
			t.Fatalf("out of order: block %d log %d after block %d log %d", e.tr.BlockNumber, e.tr.LogIndex, last.BlockNumber, last.LogIndex)
		}
		last = e.tr
	}

	// With an empty watch set no log query is made at all.
	h2 := newHarness(t)
	h2.tick()
	h2.mineTicking(testSettle + 4)
	if calls := h2.nodes[0].Calls("eth_getLogs"); calls != 0 {
		t.Errorf("eth_getLogs calls with an empty watch set = %d", calls)
	}
	if st := h2.w.Status(); st.Cursor != h2.chain.Head().Number-testSettle {
		t.Errorf("cursor did not advance with an empty watch set: %+v", st)
	}
}

func TestCatchUpAfterDowntime(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 20; i++ {
		h.watch.add(addr(i))
	}
	h.tick()
	h.pay(addr(0), 1)
	h.chain.Mine(2)
	h.tick()

	// The process is down while 3 000 blocks go by, with payments all over
	// them, some right at the edges.
	h.pay(addr(1), 2)
	h.chain.Mine(1)
	for i := 0; i < 3000; i++ {
		if i%41 == 0 {
			h.pay(addr(i%20), int64(3+i))
		}
		if i%500 == 0 {
			h.chain.Transfer(fakeToken, payer, addr(i%20), units(999))
		}
		h.chain.Mine(1)
	}
	h.pay(addr(2), 4)
	h.chain.Mine(1)

	// It comes back against a provider that allows 100 blocks per query.
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.MaxLogRange = 100 })
	h.restart()
	saves := h.store.saves
	h.sink.take()
	h.tick()
	h.assertModel(startBlock)
	head := h.chain.Head().Number
	if st := h.w.Status(); st.Cursor != head-testSettle || st.Behind != testSettle {
		t.Fatalf("status after catch-up = %+v", st)
	}
	// Progress was checkpointed along the way, not only at the end.
	if n := h.store.saves - saves; n < 15 {
		t.Errorf("only %d checkpoint saves during a 3000-block catch-up", n)
	}
	// One head for the whole catch-up, after all its transfers.
	events := h.sink.take()
	heads := 0
	for i, e := range events {
		if e.kind == "head" {
			heads++
			if i != len(events)-1 || e.head.Number != head-testSettle {
				t.Errorf("head event at position %d of %d: %+v", i, len(events), e.head)
			}
		}
	}
	if heads < 1 {
		t.Error("no head reported after catch-up")
	}
	h.mineTicking(testSettle + 1)
	if st := h.w.Status(); st.Behind != testSettle {
		t.Fatalf("status once in sync = %+v", st)
	}
	head = h.chain.Head().Number

	// Crash in the middle of the next catch-up: the sink dies after a few
	// facts. After the restart nothing is skipped; some facts repeat.
	for i := 0; i < 1000; i++ {
		if i%13 == 0 {
			h.pay(addr(i%20), int64(1+i))
		}
		h.chain.Mine(1)
	}
	h.restart()
	accepted := 0
	h.sink.set(func(s *sink) {
		s.onObserved = func(watcher.Transfer) {
			accepted++
			if accepted == 30 {
				s.failObserved = 1_000_000 // the database goes away
			}
		}
	})
	err := h.tickErr()
	if !errors.Is(err, errSink) {
		t.Fatalf("err = %v", err)
	}
	cursorAtCrash := h.w.Status().Cursor
	if cursorAtCrash >= h.chain.Head().Number-testWindow || cursorAtCrash <= head-testSettle {
		t.Fatalf("cursor at crash = %d: expected partial progress", cursorAtCrash)
	}
	h.sink.set(func(s *sink) { s.failObserved = 0; s.onObserved = nil })
	h.restart()
	h.tick()
	h.assertModel(startBlock)
}

func TestFirstStartAndStartBlock(t *testing.T) {
	// Without a checkpoint, history before the start is not scanned. That
	// is right only if nothing could have been paid before; a caller that
	// knows better (the core bridge does: it has payments and a processed
	// head) must say so with StartBlock. The head rule is switched off here
	// because this watcher was, on purpose, not told.
	h := newHarness(t)
	h.sink.set(func(s *sink) { s.headRule = nil })
	h.watch.add(addr(1))
	h.pay(addr(1), 5)
	h.chain.Mine(50)
	h.tick()
	if n := len(h.sink.liveKeys()); n != 0 {
		t.Fatalf("a transfer %d blocks before the first start was reported", 50)
	}
	// ...unless StartBlock says so.
	from := uint64(startBlock)
	h2 := newHarness(t, withConfig(func(c *watcher.Config) { c.StartBlock = &from }))
	h2.watch.add(addr(1))
	h2.pay(addr(1), 5)
	h2.chain.Mine(50)
	h2.tick()
	h2.assertModel(startBlock)
	if n := len(h2.sink.liveKeys()); n != 1 {
		t.Fatalf("StartBlock: %d transfers", n)
	}
	// The start position is chosen when the first tick succeeds — which is
	// not when the watcher is created. Here the endpoint is down for a
	// while, and in that time an address is created, paid, and buried
	// deeper than the settle depth. Since tells the watcher where it must
	// not start above.
	h4 := newHarness(t)
	h4.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
	for i := 0; i < 3; i++ {
		h4.tickErr()
		h4.passCooldowns()
	}
	created := h4.chain.Head().Number
	h4.watch.addSince(addr(1), created)
	h4.pay(addr(1), 5)
	h4.chain.Mine(5 * testSettle)
	h4.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = false })
	h4.passCooldowns()
	h4.tick()
	h4.assertModel(startBlock)
	if n := len(h4.sink.liveKeys()); n != 1 {
		t.Fatalf("a payment made before the first successful tick was missed (%d transfers)", n)
	}
	// With StartBlock given as well, the lower of the two wins.
	above := created + 3
	h5 := newHarness(t, withConfig(func(c *watcher.Config) { c.StartBlock = &above }))
	h5.watch.addSince(addr(1), startBlock)
	h5.pay(addr(1), 5)
	h5.chain.Mine(5 * testSettle)
	h5.tick()
	h5.assertModel(startBlock)
	if n := len(h5.sink.liveKeys()); n != 1 {
		t.Fatalf("StartBlock overrode Since (%d transfers)", n)
	}

	// A StartBlock in the future is clamped to the head instead of
	// stalling.
	future := uint64(startBlock + 1_000_000)
	h3 := newHarness(t, withConfig(func(c *watcher.Config) { c.StartBlock = &future }))
	h3.tick()
	h3.mineTicking(testSettle + 3)
	if st := h3.w.Status(); st.Cursor != h3.chain.Head().Number-testSettle {
		t.Fatalf("future StartBlock: %+v", st)
	}
}

// TestAddressAddedMidFlight covers the window between "address created" and
// "payer pays", from every angle.
func TestAddressAddedMidFlight(t *testing.T) {
	t.Run("added between ticks, paid later", func(t *testing.T) {
		h := newHarness(t)
		h.tick()
		h.chain.Mine(3)
		h.tick()
		h.watch.add(addr(1))
		h.pay(addr(1), 10)
		h.chain.Mine(1)
		h.tick()
		h.assertModel(startBlock)
		if len(h.sink.liveKeys()) != 1 {
			t.Fatal("payment not observed")
		}
	})

	t.Run("address created and paid while a tick is running", func(t *testing.T) {
		// The worst interleaving the normal flow can produce: after the
		// watcher has fetched the head (and, in the second variant, also
		// taken its snapshot), the core creates an address and the payer's
		// transaction is mined immediately.
		for _, when := range []string{"before snapshot", "after snapshot"} {
			h := newHarness(t)
			h.watch.add(addr(99))
			h.tick()
			h.chain.Mine(2)

			fired := false
			inject := func() {
				if fired {
					return
				}
				fired = true
				h.watch.add(addr(1))
				h.pay(addr(1), 10)
				h.chain.Mine(1)
			}
			if when == "before snapshot" {
				h.watch.mu.Lock()
				h.watch.onRead = inject
				h.watch.mu.Unlock()
			} else {
				h.nodes[0].Set(func(b *evmtest.Behavior) {
					b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
						if method == "eth_getLogs" {
							inject()
						}
						return nil, nil, false
					}
				})
			}
			h.tick() // the tick during which it all happens
			if !fired {
				t.Fatalf("%s: injection did not fire", when)
			}
			h.tick()
			h.chain.Mine(testSettle * 3)
			h.tick()
			h.assertModel(startBlock)
			if len(h.sink.liveKeys()) != 1 {
				t.Fatalf("%s: payment slipped through", when)
			}
		}
	})

	t.Run("transfer mined before the address was watched", func(t *testing.T) {
		// Outside the contract (the address must be watched before it is
		// disclosed), but the tentative zone still catches it while the
		// block is less than SettleDepth deep...
		h := newHarness(t)
		h.sink.set(func(s *sink) { s.headRule = nil }) // the premise of the rule is broken on purpose
		h.tick()
		h.pay(addr(1), 10)
		paidIn := h.chain.Mine(1).Number
		h.chain.Mine(testSettle - 2)
		h.tick() // scanned without the address
		h.watch.add(addr(1))
		h.chain.Mine(1)
		h.tick()
		h.assertModel(startBlock)
		if len(h.sink.liveKeys()) != 1 {
			t.Fatal("transfer in the tentative zone not picked up after the address was added")
		}

		// ...and beyond that only an explicit re-scan finds it.
		h.pay(addr(2), 20)
		deep := h.chain.Mine(1).Number
		h.mineTicking(testSettle + 5)
		if c := h.w.Status().Cursor; c < deep {
			t.Fatalf("test setup: cursor %d has not passed block %d", c, deep)
		}
		h.watch.add(addr(2))
		h.chain.Mine(1)
		h.tick()
		if len(h.sink.liveKeys()) != 1 {
			t.Fatal("a settled block was re-scanned without being asked")
		}
		h.w.Rescan(deep)
		h.tick()
		h.assertModel(startBlock)
		if len(h.sink.liveKeys()) != 2 {
			t.Fatal("re-scan did not find the transfer")
		}
		// Rescan requests merge to the lowest block, and already reported
		// transfers in the range are reported again (idempotent sink).
		h.sink.take()
		h.w.Rescan(deep + 3)
		h.w.Rescan(paidIn)
		h.tick()
		if n := h.sink.count("observed"); n != 2 {
			t.Fatalf("re-scan re-reported %d transfers, want 2", n)
		}
		h.assertModel(startBlock)
	})

	t.Run("address learned late, with the block since which it may have been paid", func(t *testing.T) {
		// A consumer that can say "nothing was paid to this address up to
		// block S" gets the blocks after S scanned again when the address
		// first shows up — even if they were settled long ago.
		h := newHarness(t)
		h.watch.add(addr(9))
		h.tick()
		since := h.chain.Head().Number
		h.pay(addr(1), 10)
		paidIn := h.chain.Mine(1).Number
		h.mineTicking(testSettle + 8) // settled without the address
		if c := h.w.Status().Cursor; c < paidIn {
			t.Fatalf("test setup: cursor %d has not passed block %d", c, paidIn)
		}
		headBefore := h.sink.lastHead().Number
		h.sink.take()
		h.sink.set(func(s *sink) { s.headRule = nil }) // re-armed below, once the address is known
		h.watch.addSince(addr(1), since)
		h.tick()
		events := h.sink.take()
		// The transfer is delivered before any head is reported again (the
		// processed head ends the tick where it was, so none is).
		if len(events) == 0 || events[0].kind != "observed" || events[0].tr.BlockNumber != paidIn {
			t.Fatalf("events = %v", events)
		}
		h.sink.set(func(s *sink) { s.headRule = h.checkHeadRule })
		h.mineTicking(2)
		h.assertModel(startBlock)
		if got := h.sink.lastHead().Number; got < headBefore {
			t.Fatalf("head = %d, was %d", got, headBefore)
		}
		// An address without that information (Since 0) rewinds nothing,
		// and neither does one whose Since is not below the cursor.
		cursor := h.w.Status().Cursor
		h.watch.add(addr(2))
		h.watch.addSince(addr(3), cursor+5)
		before := h.nodes[0].LogQueries()
		h.tick()
		for _, q := range h.nodes[0].LogQueries()[len(before):] {
			if q.From <= cursor {
				t.Fatalf("blocks from %d were scanned again (cursor was %d)", q.From, cursor)
			}
		}
	})

	t.Run("address removed while its transfer can still be reverted", func(t *testing.T) {
		h := newHarness(t)
		deepFinality(h)
		h.watch.add(addr(1))
		h.tick()
		h.pay(addr(1), 10)
		h.chain.Mine(2)
		h.tick()
		// The core is done with the payment and drops the address...
		h.watch.remove(addr(1))
		h.chain.Mine(1)
		h.tick()
		// ...then the block with the payment is reorganised away.
		h.chain.Reorg(3)
		h.chain.Mine(4)
		h.tick()
		if n := len(h.sink.liveKeys()); n != 0 {
			t.Fatalf("revert of a transfer to a no-longer-watched address was not reported (%d live)", n)
		}
	})
}

func TestReorgs(t *testing.T) {
	t.Run("one block, transfer dropped", func(t *testing.T) {
		h := newHarness(t)
		h.watch.add(addr(1))
		h.tick()
		tx := h.pay(addr(1), 10)
		old := h.chain.Mine(1)
		h.tick()
		h.sink.take()

		h.chain.Reorg(1) // the block is orphaned; the tx is not re-mined
		h.chain.Mine(2)
		h.tick()
		events := h.sink.take()
		if got := kinds(events); !reflect.DeepEqual(got, []string{"reverted", "head"}) {
			t.Fatalf("events = %v", got)
		}
		if r := events[0].tr; r.TxHash != tx || r.BlockHash != old.Hash || r.BlockNumber != old.Number {
			t.Fatalf("revert = %+v", r)
		}
		h.assertModel(startBlock)
		if st := h.w.Status(); st.Reorgs != 1 || st.Tracked != 0 {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("one block, same height, transfer re-mined", func(t *testing.T) {
		h := newHarness(t)
		h.watch.add(addr(1))
		h.tick()
		tx := h.pay(addr(1), 10)
		old := h.chain.Mine(1)
		h.tick()
		h.sink.take()

		h.chain.Requeue(h.chain.Reorg(1)...)
		replacement := h.chain.Mine(1) // same number, same tx, different hash
		if replacement.Number != old.Number || replacement.Hash == old.Hash {
			t.Fatal("test setup: expected a different block at the same height")
		}
		h.tick()
		events := h.sink.take()
		// Revert of the old fact first, then the new one. The processed
		// head is far below the replaced block and does not change.
		if got := kinds(events); !reflect.DeepEqual(got, []string{"reverted", "observed"}) {
			t.Fatalf("events = %v", got)
		}
		if events[0].tr.BlockHash != old.Hash || events[1].tr.BlockHash != replacement.Hash || events[1].tr.TxHash != tx {
			t.Fatalf("events = %+v", events)
		}
		h.assertModel(startBlock)
	})

	t.Run("several blocks, transfer moves to another block", func(t *testing.T) {
		h := newHarness(t)
		deepFinality(h)
		h.watch.add(addr(1), addr(2))
		h.tick()
		moved := h.pay(addr(1), 10)
		h.chain.Mine(1)
		dropped := h.pay(addr(2), 20)
		h.chain.Mine(2)
		h.tick()
		h.sink.take()

		txs := h.chain.Reorg(3)
		h.chain.Mine(2) // two empty blocks first
		for _, tx := range txs {
			if tx.Hash == moved {
				h.chain.Requeue(tx)
			}
		}
		h.pay(addr(2), 7) // and a transfer that only exists on the new fork
		h.chain.Mine(3)
		h.tick()
		events := h.sink.take()
		if got := kinds(events); !reflect.DeepEqual(got, []string{"reverted", "reverted", "observed", "observed", "head"}) {
			t.Fatalf("events = %v", events)
		}
		if events[0].tr.TxHash != moved || events[1].tr.TxHash != dropped || events[2].tr.TxHash != moved {
			t.Fatalf("events = %v", events)
		}
		if events[2].tr.BlockNumber == events[0].tr.BlockNumber {
			t.Fatal("the moved transfer should be in another block")
		}
		h.assertModel(startBlock)
	})

	t.Run("deeper than the settle depth, within the window", func(t *testing.T) {
		h := newHarness(t)
		deepFinality(h)
		h.watch.add(addr(1), addr(2))
		h.tick()
		h.pay(addr(1), 10)
		h.chain.Mine(3)
		h.tick()
		h.chain.Mine(testSettle + 4) // the transfer's block is settled now
		h.tick()
		cursor := h.w.Status().Cursor
		h.sink.take()

		depth := testSettle + 7
		txs := h.chain.Reorg(depth) // back to before the payment, far below the cursor
		h.chain.Mine(2)
		h.chain.Requeue(txs...)
		h.pay(addr(2), 5)
		h.chain.Mine(depth)
		h.tick()
		if got := h.w.Status(); got.Reorgs != 1 {
			t.Fatalf("reorgs = %d", got.Reorgs)
		}
		events := h.sink.take()
		if got := kinds(events); !reflect.DeepEqual(got, []string{"reverted", "observed", "observed", "head"}) {
			t.Fatalf("events = %v (cursor was %d)", events, cursor)
		}
		h.assertModel(startBlock)
	})

	t.Run("the chain gets shorter", func(t *testing.T) {
		h := newHarness(t)
		deepFinality(h)
		h.watch.add(addr(1))
		h.tick()
		h.pay(addr(1), 10)
		h.chain.Mine(5)
		h.tick()
		before := h.sink.lastHead()
		h.sink.take()

		h.chain.Reorg(5) // drops the transfer's block and four more
		h.chain.Mine(1)  // the new chain is shorter for now
		h.tick()
		events := h.sink.take()
		// The revert is reported at once. The processed head was below the
		// fork and stays where it is — it never follows the tip down.
		if got := kinds(events); !reflect.DeepEqual(got, []string{"reverted"}) {
			t.Fatalf("events = %v", events)
		}
		if got := h.sink.lastHead(); got != before {
			t.Fatalf("processed head changed from %d to %d", before.Number, got.Number)
		}
		h.assertModel(startBlock)
		if st := h.w.Status(); st.Reorgs != 1 {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("revert rejected by the sink is retried", func(t *testing.T) {
		h := newHarness(t)
		h.watch.add(addr(1))
		h.tick()
		h.pay(addr(1), 10)
		h.chain.Mine(1)
		h.tick()
		h.chain.Reorg(1)
		h.chain.Mine(2)
		h.sink.set(func(s *sink) { s.failReverted = 1 })
		if err := h.tickErr(); !errors.Is(err, errSink) {
			t.Fatalf("err = %v", err)
		}
		if len(h.sink.liveKeys()) != 1 {
			t.Fatal("model changed although the sink rejected the revert")
		}
		h.tick()
		h.assertModel(startBlock)
	})
}

// TestFinalityConflicts: an endpoint whose chain contradicts a finalized
// block, or under which nothing of the reorg window survives, is not believed
// lightly.
func TestFinalityConflicts(t *testing.T) {
	// setup reports a payment, lets it become final, then replaces the last
	// `depth` blocks of the chain (the payment's block included) by a fork
	// without the payment.
	setup := func(h *harness, depth int) {
		h.watch.add(addr(1), addr(2))
		h.tick()
		h.pay(addr(1), 10)
		h.chain.Mine(1)
		h.chain.Mine(depth - 2)
		h.tick()
		h.chain.Mine(1)
		h.tick()
		h.sink.take()
	}

	t.Run("single endpoint: believed only if it persists", func(t *testing.T) {
		var reported []watcher.DeepReorg
		h := newHarness(t, withConfig(func(c *watcher.Config) {
			c.ConflictPatience = 4
			c.OnDeepReorg = func(d watcher.DeepReorg) { reported = append(reported, d) }
		}))
		setup(h, 8)
		if f := h.sink.lastHead().Finalized; f == 0 {
			t.Fatal("test setup: no finalized block known")
		}
		h.chain.Reorg(8) // far below the finalized block
		h.pay(addr(2), 3)
		h.chain.Mine(10)

		for i := 1; i < 4; i++ {
			err := h.tickErr()
			if !errors.Is(err, watcher.ErrInconsistentEndpoint) {
				t.Fatalf("tick %d: err = %v", i, err)
			}
			if events := h.sink.take(); len(events) != 0 {
				t.Fatalf("tick %d: facts reported during an unconfirmed conflict: %v", i, events)
			}
			h.passCooldowns()
		}
		if len(h.sink.liveKeys()) != 1 {
			t.Fatal("transfer reverted before the conflict was accepted")
		}
		h.tick() // fourth consecutive tick: accepted
		events := h.sink.take()
		if got := kinds(events); !reflect.DeepEqual(got, []string{"reverted", "observed", "head"}) {
			t.Fatalf("events = %v", events)
		}
		h.assertModel(startBlock)
		if st := h.w.Status(); st.DeepReorgs != 1 {
			t.Fatalf("status = %+v", st)
		}
		if len(reported) != 1 || reported[0].RescanFrom == 0 || !strings.Contains(reported[0].Reason, "finalized") {
			t.Fatalf("OnDeepReorg = %+v", reported)
		}
		if !strings.Contains(h.logs.String(), "contradicts finalized history") {
			t.Error("deep reorg not logged")
		}
	})

	t.Run("single endpoint: a transient glitch is ignored", func(t *testing.T) {
		h := newHarness(t, withConfig(func(c *watcher.Config) { c.ConflictPatience = 4 }))
		setup(h, 8)
		// For two ticks the endpoint serves a stale fork (one bad backend
		// behind its load balancer), then it is itself again.
		stale := h.chain.Clone()
		stale.Reorg(8)
		stale.Mine(9)
		h.nodes[0].SetChain(stale)
		for i := 0; i < 2; i++ {
			if err := h.tickErr(); !errors.Is(err, watcher.ErrInconsistentEndpoint) {
				t.Fatalf("err = %v", err)
			}
			h.passCooldowns()
		}
		h.nodes[0].SetChain(h.chain)
		h.chain.Mine(2)
		h.tick()
		if events := h.sink.take(); h.sink.count("reverted") != 0 || len(h.sink.liveKeys()) != 1 {
			t.Fatalf("events after glitch = %v", events)
		}
		h.assertModel(startBlock)
		if st := h.w.Status(); st.DeepReorgs != 0 || st.Reorgs != 0 {
			t.Fatalf("status = %+v", st)
		}
		// The streak was reset: the next glitch starts counting from one.
		h.nodes[0].SetChain(stale)
		for i := 0; i < 3; i++ {
			h.tickErr()
			h.passCooldowns()
		}
		h.nodes[0].SetChain(h.chain)
		h.tick()
		h.assertModel(startBlock)
	})

	t.Run("two endpoints: the one on the stale fork is rejected", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		setup(h, 8)
		// The primary falls onto a fork that contradicts finalized
		// history; the second endpoint stays on the real chain.
		stale := h.chain.Clone()
		stale.Reorg(8)
		stale.Mine(12)
		h.nodes[0].SetChain(stale)
		h.chain.Mine(2)

		err := h.tickErr()
		if !errors.Is(err, watcher.ErrInconsistentEndpoint) || !strings.Contains(err.Error(), "confirms the known chain") {
			t.Fatalf("err = %v", err)
		}
		if events := h.sink.take(); len(events) != 0 {
			t.Fatalf("facts reported from a stale fork: %v", events)
		}
		// The primary was marked; the next tick uses the good endpoint.
		h.tick()
		if st := h.w.Status(); st.Endpoint != evm.RedactURL(h.urls[1]) || st.DeepReorgs != 0 {
			t.Fatalf("status = %+v", st)
		}
		h.assertModel(startBlock)
		if len(h.sink.liveKeys()) != 1 {
			t.Fatal("the reported transfer was lost")
		}
	})

	t.Run("two endpoints agree: accepted at once", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		setup(h, 8)
		h.chain.Reorg(8)
		h.pay(addr(2), 3)
		h.chain.Mine(10)
		h.tick()
		events := h.sink.take()
		if got := kinds(events); !reflect.DeepEqual(got, []string{"reverted", "observed", "head"}) {
			t.Fatalf("events = %v", events)
		}
		h.assertModel(startBlock)
		if st := h.w.Status(); st.DeepReorgs != 1 {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("reorg deeper than the window, no finality information", func(t *testing.T) {
		h := newHarness(t, withConfig(func(c *watcher.Config) { c.NoFinality = true; c.ConflictPatience = 3 }))
		h.watch.add(addr(1), addr(2))
		h.chain.Mine(60) // history older than anything the watcher will remember
		h.tick()
		for i := 0; i < 6; i++ { // a tick every few blocks: the window fills with anchors
			h.chain.Mine(4)
			h.tick()
		}
		recent := h.pay(addr(1), 10)
		h.chain.Mine(3)
		h.tick()
		h.sink.take()

		// Everything the watcher remembers is replaced.
		txs := h.chain.Reorg(testWindow + 20)
		h.chain.Mine(2)
		for _, tx := range txs {
			if tx.Hash == recent {
				h.chain.Requeue(tx)
			}
		}
		h.pay(addr(2), 3)
		h.chain.Mine(testWindow + 20)

		for i := 1; i < 3; i++ {
			if err := h.tickErr(); !errors.Is(err, watcher.ErrInconsistentEndpoint) {
				t.Fatalf("err = %v", err)
			}
			h.passCooldowns()
		}
		h.tick()
		h.assertModel(startBlock)
		if st := h.w.Status(); st.DeepReorgs != 1 {
			t.Fatalf("status = %+v", st)
		}
		// Old fact reverted, the re-mined transfer and the new one observed.
		if r, o := h.sink.count("reverted"), h.sink.count("observed"); r != 1 || o != 2 {
			t.Fatalf("reverted=%d observed=%d: %v", r, o, h.sink.take())
		}
	})
}

func TestEndpointFailoverMidScan(t *testing.T) {
	// Three endpoints: when the primary dies, the second takes over the
	// scan and the third confirms what it finds.
	h := newHarness(t, withNodes(3))
	for i := 0; i < 10; i++ {
		h.watch.add(addr(i))
	}
	h.tick()
	for i := 0; i < 2000; i++ {
		if i%17 == 0 {
			h.pay(addr(i%10), int64(1+i))
		}
		h.chain.Mine(1)
	}
	// Both providers cap queries at 100 blocks; the primary dies after a
	// few of them.
	for _, n := range h.nodes {
		n.Set(func(b *evmtest.Behavior) { b.MaxLogRange = 100 })
	}
	h.nodes[0].Set(func(b *evmtest.Behavior) {
		served := 0
		b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method != "eth_getLogs" {
				return nil, nil, false
			}
			served++
			if served > 8 {
				h.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
			}
			return nil, nil, false
		}
	})
	err := h.tickErr()
	if k, _ := evm.KindOf(err); k != evm.KindHTTP {
		t.Fatalf("err = %v", err)
	}
	partial := h.w.Status()
	if partial.Cursor <= startBlock || partial.Cursor >= h.chain.Head().Number-testSettle {
		t.Fatalf("no partial progress kept: %+v", partial)
	}
	if partial.LastError == "" || partial.ConsecutiveErrors != 1 {
		t.Fatalf("status = %+v", partial)
	}

	// The next tick fails over and continues from the cursor.
	h.nodes[1].ResetCounters()
	h.tick()
	st := h.w.Status()
	if st.Endpoint != evm.RedactURL(h.urls[1]) || st.Cursor != h.chain.Head().Number-testSettle || st.LastError != "" {
		t.Fatalf("status after failover = %+v", st)
	}
	for _, q := range h.nodes[1].LogQueries() {
		if !q.Refused && q.From <= partial.Cursor-200 {
			t.Fatalf("fallback endpoint re-scanned from %d although the cursor was %d", q.From, partial.Cursor)
		}
	}
	h.assertModel(startBlock)

	// The primary comes back; after its cooldown the watcher fails back.
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = false; b.Intercept = nil })
	h.passCooldowns()
	h.pay(addr(3), 5)
	h.chain.Mine(2)
	h.tick()
	if st := h.w.Status(); st.Endpoint != evm.RedactURL(h.urls[0]) {
		t.Fatalf("no fail-back: %+v", st)
	}
	h.assertModel(startBlock)
}

func TestProviderLimits(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 700; i++ {
		h.watch.add(addr(i))
	}
	// A stingy free endpoint: 10 blocks per query, 120 topics per filter,
	// no batches, no finality tag.
	h.nodes[0].Set(func(b *evmtest.Behavior) {
		b.MaxLogRange = 10
		b.RangeError = func(uint64) evmtest.RPCError {
			return evmtest.RPCError{Code: -32000, Message: "log query range must not exceed 10 blocks"}
		}
		b.MaxTopics = 120
		b.NoBatch = true
		b.NoFinalized = true
	})
	h.tick()
	for i := 0; i < 300; i++ {
		if i%7 == 0 {
			h.pay(addr((i*13)%700), int64(1+i))
		}
		h.chain.Mine(1)
	}
	h.tick()
	h.assertModel(startBlock)
	if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle {
		t.Fatalf("status = %+v", st)
	}
	if f := h.sink.lastHead().Finalized; f != 0 {
		t.Fatalf("finalized = %d from an endpoint without finality", f)
	}
	// Steady state stays within the limits without refusals.
	h.nodes[0].ResetCounters()
	for i := 0; i < 5; i++ {
		h.pay(addr(i), 1)
		h.chain.Mine(2)
		h.tick()
	}
	for _, q := range h.nodes[0].LogQueries() {
		if q.Refused {
			t.Fatalf("query %+v refused in steady state", q)
		}
		if q.To-q.From+1 > 10 || q.Recipients > 120 {
			t.Fatalf("query %+v exceeds the provider's limits", q)
		}
	}
	h.assertModel(startBlock)

	// An endpoint that rate-limits is backed off from, and nothing is lost.
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.RateLimited = true; b.RetryAfter = 20 })
	h.pay(addr(5), 9)
	h.chain.Mine(1)
	err := h.tickErr()
	if !evm.IsKind(err, evm.KindRateLimited) {
		t.Fatalf("err = %v", err)
	}
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.RateLimited = false })
	h.passCooldowns()
	h.tick()
	h.assertModel(startBlock)
}

// TestNoiseIsIgnored: look-alike tokens, zero-value transfers, dust,
// self-transfers, other events, failed transactions.
func TestNoiseIsIgnored(t *testing.T) {
	h := newHarness(t, withConfig(func(c *watcher.Config) { c.MinAmount = units(1) }))
	deposit := addr(1)
	h.watch.add(deposit)
	h.tick()
	h.sink.take()

	// A contract with a near-identical address emits a perfectly formed
	// Transfer to the deposit address.
	h.chain.Transfer(fakeToken, payer, deposit, units(1_000_000))
	// Address poisoning: zero-value transfers to and "from" the deposit
	// address.
	h.chain.Transfer(usdt, addr(500), deposit, units(0))
	h.chain.Transfer(usdt, deposit, addr(501), units(0))
	// Dust below the configured minimum.
	h.chain.Transfer(usdt, addr(502), deposit, units(1).Rsh(units(1), 1))
	// A self-transfer does not add funds.
	h.chain.Transfer(usdt, deposit, deposit, units(50))
	// Outgoing transfer from the deposit address (a sweep).
	h.chain.Transfer(usdt, deposit, addr(503), units(5))
	// Someone else's payment.
	h.chain.Transfer(usdt, payer, addr(504), units(5))
	// Other events of the real token mentioning the address.
	approval := evm.Hash(ethaddr.Keccak256([]byte("Approval(address,address,uint256)")))
	real := evmtest.TransferLog(usdt, payer, deposit, units(5))
	h.chain.Submit(evmtest.RawLog{Address: usdt, Topics: []evm.Hash{approval, real.Topics[1], real.Topics[2]}, Data: real.Data})
	// A Transfer-topic log of the real token that is not ERC-20 shaped.
	h.chain.Submit(evmtest.RawLog{Address: usdt, Topics: append(append([]evm.Hash{}, real.Topics...), evm.Hash{1}), Data: real.Data})
	h.chain.Submit(evmtest.RawLog{Address: usdt, Topics: real.Topics, Data: append(append([]byte{}, real.Data...), make([]byte, 32)...)})
	// A transaction that reverted.
	h.chain.SubmitFailed()
	h.chain.Mine(1)
	h.tick()
	if events := h.sink.take(); !reflect.DeepEqual(kinds(events), []string{"head"}) {
		t.Fatalf("noise produced facts: %v", events)
	}
	if n := h.nodes[0].Calls("eth_getTransactionReceipt"); n != 0 {
		t.Errorf("noise caused %d receipt lookups", n)
	}

	// The real thing, in the same block as more noise, still gets through
	// — exactly once, with the right log index.
	h.chain.Transfer(fakeToken, payer, deposit, units(25))
	tx := h.chain.Submit(
		evmtest.TransferLog(fakeToken, payer, deposit, units(25)),
		evmtest.TransferLog(usdt, payer, deposit, units(25)),
		evmtest.TransferLog(usdt, payer, deposit, units(0)),
	)
	h.chain.Mine(1)
	h.tick()
	events := h.sink.take()
	if !reflect.DeepEqual(kinds(events), []string{"observed", "head"}) {
		t.Fatalf("events = %v", events)
	}
	if tr := events[0].tr; tr.TxHash != tx || tr.LogIndex != 2 || tr.Amount.Cmp(units(25)) != 0 {
		t.Fatalf("transfer = %+v", tr)
	}

	// Raising the dust threshold later must not make an already reported
	// transfer look reverted.
	h.cfg.MinAmount = units(100)
	h.restart()
	h.chain.Mine(1)
	h.tick()
	if n := h.sink.count("reverted"); n != 0 || len(h.sink.liveKeys()) != 1 {
		t.Fatalf("threshold change caused %d reverts", n)
	}
}

func TestAtLeastOnce(t *testing.T) {
	t.Run("sink rejects a transfer", func(t *testing.T) {
		h := newHarness(t)
		h.watch.add(addr(1), addr(2))
		h.tick()
		h.pay(addr(1), 10)
		h.pay(addr(2), 20)
		h.chain.Mine(testSettle + 3) // deep enough to be settled in one go
		cursor := h.w.Status().Cursor
		h.sink.take()

		// The first fact is accepted, the second is refused.
		h.sink.set(func(s *sink) {
			s.onObserved = func(watcher.Transfer) { s.failObserved = 1 }
		})
		if err := h.tickErr(); !errors.Is(err, errSink) {
			t.Fatalf("err = %v", err)
		}
		h.sink.set(func(s *sink) { s.onObserved = nil })
		if got := h.w.Status().Cursor; got != cursor {
			t.Fatalf("cursor moved from %d to %d past an undelivered fact", cursor, got)
		}
		if n := h.sink.count("head"); n != 0 {
			t.Fatal("head reported although a transfer below it was not delivered")
		}
		// Retry: the refused fact arrives; the accepted one is not sent a
		// second time within the same process.
		h.tick()
		if n := h.sink.count("observed"); n != 2 {
			t.Fatalf("observed %d times, want 2", n)
		}
		h.assertModel(startBlock)
	})

	t.Run("sink rejects the head", func(t *testing.T) {
		h := newHarness(t)
		h.tick()
		h.chain.Mine(2)
		h.sink.take()
		h.sink.set(func(s *sink) { s.failHead = 1 })
		if err := h.tickErr(); !errors.Is(err, errSink) {
			t.Fatalf("err = %v", err)
		}
		h.tick() // same head, delivered now
		if got := h.sink.lastHead().Number; got != h.chain.Head().Number-testSettle {
			t.Fatalf("head = %d", got)
		}
	})

	t.Run("checkpoint lost: facts are delivered again after restart", func(t *testing.T) {
		h := newHarness(t)
		h.watch.add(addr(1))
		h.tick()
		before := h.store.snapshot()
		h.pay(addr(1), 10)
		h.chain.Mine(testSettle + 3)
		h.tick()
		if h.sink.dups != 0 {
			t.Fatal("unexpected duplicate")
		}
		// The process crashes and the last checkpoint writes never made
		// it to disk.
		h.store.restore(before)
		h.restart()
		h.tick()
		if h.sink.dups != 1 {
			t.Fatalf("duplicates = %d, want the transfer to be delivered again", h.sink.dups)
		}
		h.assertModel(startBlock)
	})

	t.Run("checkpoint save fails before delivery: nothing is reported", func(t *testing.T) {
		// The intent to report is recorded first. If that fails, the sink
		// is not told: it must never know a transfer the watcher has no
		// durable memory of.
		h := newHarness(t)
		h.watch.add(addr(1))
		h.tick()
		h.pay(addr(1), 10)
		h.chain.Mine(testSettle + 3)
		h.store.mu.Lock()
		h.store.failSave = 1
		h.store.mu.Unlock()
		if err := h.tickErr(); !strings.Contains(err.Error(), "save checkpoint") {
			t.Fatalf("err = %v", err)
		}
		if n := len(h.sink.liveKeys()); n != 0 {
			t.Fatalf("%d transfers reported without a recorded intent", n)
		}
		h.tick() // the same process retries
		if h.sink.dups != 0 || len(h.sink.liveKeys()) != 1 {
			t.Fatalf("dups = %d, live = %d", h.sink.dups, len(h.sink.liveKeys()))
		}
		h.assertModel(startBlock)
	})

	t.Run("checkpoint save fails after delivery: facts repeat after restart", func(t *testing.T) {
		h := newHarness(t)
		h.watch.add(addr(1))
		h.tick()
		h.pay(addr(1), 10)
		h.chain.Mine(testSettle + 3)
		h.store.mu.Lock()
		h.store.passSaves, h.store.failSave = 1, 100 // the intent is saved, nothing after it
		h.store.mu.Unlock()
		if err := h.tickErr(); !strings.Contains(err.Error(), "save checkpoint") {
			t.Fatalf("err = %v", err)
		}
		if len(h.sink.liveKeys()) != 1 {
			t.Fatal("the transfer should have been delivered")
		}
		h.store.mu.Lock()
		h.store.failSave = 0
		h.store.mu.Unlock()
		h.restart() // crash before any later save succeeded
		h.tick()
		if h.sink.dups != 1 {
			t.Fatalf("duplicates = %d, want the transfer to be delivered again", h.sink.dups)
		}
		h.assertModel(startBlock)
	})

	t.Run("crash between intent and delivery, then a reorg", func(t *testing.T) {
		// The worst place to stop: the intent is on disk, the sink has not
		// been told (or has — the watcher cannot know), and while the
		// process is down the block is reorganised away. The revert must
		// still go out; the sink ignores it if it never saw the transfer.
		for _, delivered := range []bool{false, true} {
			h := newHarness(t)
			h.sink.set(func(s *sink) { s.lenientReverts = true })
			h.watch.add(addr(1))
			h.tick()
			h.pay(addr(1), 10)
			h.chain.Mine(1)
			if delivered {
				h.store.mu.Lock()
				h.store.passSaves, h.store.failSave = 1, 100
				h.store.mu.Unlock()
			} else {
				h.sink.set(func(s *sink) { s.failObserved = 100 })
			}
			h.tickErr()
			if got := len(h.sink.liveKeys()); (got == 1) != delivered {
				t.Fatalf("delivered=%v: %d live transfers before the crash", delivered, got)
			}
			h.store.mu.Lock()
			h.store.failSave = 0
			h.store.mu.Unlock()
			h.sink.set(func(s *sink) { s.failObserved = 0 })
			h.sink.take()

			h.chain.Reorg(1) // while the process is down
			h.chain.Mine(2)
			h.restart()
			h.tick()
			if n := h.sink.count("reverted"); n != 1 {
				t.Fatalf("delivered=%v: %d reverts after restart, want 1", delivered, n)
			}
			h.assertModel(startBlock)
		}
	})

	t.Run("restart re-reports the unsettled zone", func(t *testing.T) {
		h := newHarness(t)
		h.watch.add(addr(1), addr(2))
		h.tick()
		h.pay(addr(1), 10)
		h.mineTicking(testSettle + 5) // settled
		h.pay(addr(2), 20)
		h.chain.Mine(2) // not settled yet
		h.tick()
		h.sink.take()
		h.restart()
		h.chain.Mine(1)
		h.tick()
		events := h.sink.take()
		if n := h.sink.count("observed"); n != 0 { // take() cleared; count afterwards is 0
			t.Fatal("unexpected state")
		}
		observed := 0
		for _, e := range events {
			if e.kind == "observed" {
				observed++
				if e.tr.To != addr(2) {
					t.Fatalf("settled transfer re-reported after restart: %+v", e.tr)
				}
			}
		}
		if observed != 1 {
			t.Fatalf("unsettled transfer re-reported %d times after restart, want 1", observed)
		}
		// And it was not verified again: it had been before it was
		// checkpointed.
		h.assertModel(startBlock)
	})

	t.Run("watch set or store unavailable", func(t *testing.T) {
		h := newHarness(t)
		h.watch.add(addr(1))
		h.store.mu.Lock()
		h.store.failLoad = 1
		h.store.mu.Unlock()
		if err := h.tickErr(); !strings.Contains(err.Error(), "load checkpoint") {
			t.Fatalf("err = %v", err)
		}
		h.tick()
		h.pay(addr(1), 10)
		h.chain.Mine(testSettle + 2)
		h.watch.mu.Lock()
		h.watch.fail = 1
		h.watch.mu.Unlock()
		cursor := h.w.Status().Cursor
		if err := h.tickErr(); !strings.Contains(err.Error(), "watch set") {
			t.Fatalf("err = %v", err)
		}
		if got := h.w.Status().Cursor; got != cursor {
			t.Fatal("cursor moved without a watch set")
		}
		h.tick()
		h.assertModel(startBlock)
	})
}

func TestCheckpointValidation(t *testing.T) {
	h := newHarness(t)
	h.watch.add(addr(1))
	h.tick()
	h.pay(addr(1), 10)
	h.chain.Mine(2)
	h.tick()
	good := h.store.snapshot()

	// The checkpoint is self-describing JSON.
	var cp map[string]any
	if err := json.Unmarshal(good, &cp); err != nil {
		t.Fatal(err)
	}
	if cp["v"].(float64) != 1 || cp["chain_id"].(float64) != 56 || cp["token"] != usdt.String() {
		t.Fatalf("checkpoint = %s", good)
	}
	if transfers := cp["transfers"].([]any); len(transfers) != 1 ||
		transfers[0].(map[string]any)["amount"] != "10000000000000000000" {
		t.Fatalf("checkpoint transfers = %v", cp["transfers"])
	}

	bad := map[string]string{
		"garbage":       "{not json",
		"other version": strings.Replace(string(good), `"v":1`, `"v":2`, 1),
		"other chain":   strings.Replace(string(good), `"chain_id":56`, `"chain_id":97`, 1),
		"other token":   strings.Replace(string(good), usdt.String(), fakeToken.String(), 1),
		"bad amount":    strings.Replace(string(good), `"10000000000000000000"`, `"-5"`, 1),
		"float amount":  strings.Replace(string(good), `"10000000000000000000"`, `"1e19"`, 1),
	}
	for name, data := range bad {
		if data == string(good) {
			t.Fatalf("%s: mutation did not apply", name)
		}
		h.store.restore([]byte(data))
		h.restart()
		err := h.w.Tick(context.Background())
		if !errors.Is(err, watcher.ErrCheckpoint) {
			t.Errorf("%s: err = %v, want ErrCheckpoint", name, err)
		}
		// Run gives up immediately instead of looping on it.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := h.w.Run(ctx); !errors.Is(err, watcher.ErrCheckpoint) {
			t.Errorf("%s: Run returned %v", name, err)
		}
		cancel()
		if got := h.store.snapshot(); string(got) != data {
			t.Errorf("%s: the unusable checkpoint was overwritten", name)
		}
	}

	// A caller can look into a checkpoint before deciding what to do.
	info, err := watcher.InspectCheckpoint(good)
	if err != nil || info.ChainID != 56 || info.Token != usdt || info.Transfers != 1 || info.Cursor != startBlock+2-testSettle {
		t.Fatalf("InspectCheckpoint = %+v, %v", info, err)
	}
	for name, data := range bad {
		if name == "other chain" || name == "other token" {
			if info, err := watcher.InspectCheckpoint([]byte(data)); err != nil || (info.ChainID == 56 && info.Token == usdt) {
				t.Errorf("%s: InspectCheckpoint = %+v, %v", name, info, err)
			}
			continue
		}
		if _, err := watcher.InspectCheckpoint([]byte(data)); !errors.Is(err, watcher.ErrCheckpoint) {
			t.Errorf("%s: InspectCheckpoint err = %v", name, err)
		}
	}

	// The good one resumes exactly.
	h.store.restore(good)
	h.restart()
	h.chain.Mine(1)
	h.tick()
	h.assertModel(startBlock)
}

func TestNewValidation(t *testing.T) {
	h := newHarness(t)
	base := h.cfg
	for name, mutate := range map[string]func(c *watcher.Config){
		"no client":          func(c *watcher.Config) { c.Client = nil },
		"no token":           func(c *watcher.Config) { c.Token = ethaddr.Address{} },
		"no sink":            func(c *watcher.Config) { c.Sink = nil },
		"no watch set":       func(c *watcher.Config) { c.Watch = nil },
		"no store":           func(c *watcher.Config) { c.Store = nil },
		"strict, 1 endpoint": func(c *watcher.Config) { c.Verify = watcher.VerifyStrict },
		"bad verify mode":    func(c *watcher.Config) { c.Verify = 99 },
		"window < settle":    func(c *watcher.Config) { c.SettleDepth = 50; c.ReorgWindow = 10 },
		"negative minimum":   func(c *watcher.Config) { c.MinAmount = units(-1) },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := watcher.New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cfg := base
	cfg.SettleDepth, cfg.ReorgWindow, cfg.PollInterval = 0, 0, 0
	if _, err := watcher.New(cfg); err != nil {
		t.Fatalf("defaults: %v", err)
	}
}

func TestRunLoop(t *testing.T) {
	var sleeps []time.Duration
	h := newHarness(t, withConfig(func(c *watcher.Config) {
		c.PollInterval = 3 * time.Second
		c.MaxBackoff = 8 * time.Second
		c.CatchUpPause = time.Nanosecond
	}))
	h.watch.add(addr(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The injected sleep scripts the scenario: each call is the pause
	// after one tick.
	step := 0
	h.cfg.Sleep = func(ctx context.Context, d time.Duration) error {
		if d == time.Nanosecond {
			return ctx.Err() // catch-up pause, not a loop pause
		}
		sleeps = append(sleeps, d)
		step++
		switch step {
		case 1:
			h.pay(addr(1), 10)
			h.chain.Mine(1)
		case 2:
			h.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
		case 7:
			h.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = false })
			h.chain.Mine(1)
		case 9:
			cancel()
		}
		h.passCooldowns()
		return ctx.Err()
	}
	h.restart()
	err := h.w.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	want := []time.Duration{
		3 * time.Second, 3 * time.Second, // two good ticks
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, // outage: exponential, capped
		3 * time.Second, 3 * time.Second, // recovered: back to the poll interval
	}
	if !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("pauses = %v\n   want %v", sleeps, want)
	}
	h.assertModel(startBlock)
	if st := h.w.Status(); st.ConsecutiveErrors != 0 || st.LastSuccessAt.IsZero() {
		t.Fatalf("status = %+v", st)
	}

	// Cancellation during a tick stops promptly and is not an error state.
	h2 := newHarness(t)
	h2.nodes[0].Set(func(b *evmtest.Behavior) { b.Delay = 10 * time.Second })
	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h2.w.Run(ctx2) }()
	time.Sleep(50 * time.Millisecond)
	cancel2()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
	if st := h2.w.Status(); st.LastError != "" {
		t.Fatalf("cancellation recorded as an error: %+v", st)
	}
}

// TestPendingTransfersOfTheConsumer: the consumer's list of not-yet-final
// transfers is the second source of truth. Whatever happened to the
// checkpoint, a transfer on that list is tracked until the consumer drops it,
// and reverted if its block is gone.
func TestPendingTransfersOfTheConsumer(t *testing.T) {
	// asPending turns what the sink was told into what a consumer would
	// list.
	asPending := func(h *harness) []watcher.Transfer {
		var out []watcher.Transfer
		for _, tr := range h.sink.liveKeys() {
			out = append(out, tr)
		}
		return out
	}

	t.Run("checkpoint lost, transfer reorganised away meanwhile", func(t *testing.T) {
		h := newHarness(t)
		deepFinality(h)
		h.watch.add(addr(1), addr(2))
		h.tick()
		h.pay(addr(1), 10) // will be reorganised away
		h.chain.Mine(1)
		h.pay(addr(2), 20) // stays
		h.chain.Mine(2)
		h.tick()
		if len(h.sink.liveKeys()) != 2 {
			t.Fatal("test setup")
		}
		// The watcher goes down; its checkpoint is lost; the chain
		// reorganises the first payment away.
		h.watch.setPending(asPending(h)...)
		txs := h.chain.Reorg(3)
		for _, tx := range txs {
			if len(tx.Logs) > 0 && tx.Logs[0].Topics[2] == evm.Hash(append(make([]byte, 12), addr(2).Bytes()...)) {
				h.chain.Requeue(tx) // the second payment is mined again, in another block
			}
		}
		h.chain.Mine(testSettle + 6)
		h.store.restore(nil)
		// A watcher without a checkpoint would start below the tip, above
		// both payments, and would never look at their blocks again.
		h.sink.take()
		h.restart()
		h.tick()
		h.assertModel(startBlock)
		if r, o := h.sink.count("reverted"), h.sink.count("observed"); r != 2 || o != 1 {
			t.Fatalf("reverted = %d, observed = %d; want both stale facts reverted and the re-mined payment observed", r, o)
		}
		if !strings.Contains(h.logs.String(), "adopting a pending transfer") {
			t.Error("adoption not logged")
		}
	})

	t.Run("checkpoint lost, transfers still on chain", func(t *testing.T) {
		h := newHarness(t)
		h.watch.add(addr(1))
		h.tick()
		h.pay(addr(1), 10)
		h.chain.Mine(2)
		h.tick()
		h.watch.setPending(asPending(h)...)
		h.chain.Mine(testSettle + 3)
		h.store.restore(nil)
		h.sink.take()
		h.restart()
		h.tick()
		// Nothing to report: the consumer already has it, and it is where
		// it was.
		if r, o := h.sink.count("reverted"), h.sink.count("observed"); r != 0 || o != 0 {
			t.Fatalf("reverted = %d, observed = %d", r, o)
		}
		if st := h.w.Status(); st.Tracked != 1 {
			t.Fatalf("status = %+v", st)
		}
		// It stays tracked far beyond the reorg window, for as long as the
		// consumer lists it — a consumer may need thousands of
		// confirmations.
		h.chain.Mine(3 * testWindow)
		h.tick()
		queries := len(h.nodes[0].LogQueries())
		h.chain.Mine(2)
		h.tick()
		if st := h.w.Status(); st.Tracked != 1 {
			t.Fatalf("pending transfer forgotten: %+v", st)
		}
		// ...without re-scanning its block on every tick.
		for _, q := range h.nodes[0].LogQueries()[queries:] {
			if q.From < h.chain.Head().Number-testSettle-10 {
				t.Fatalf("old blocks scanned again: %+v", q)
			}
		}
		// Once the consumer regards it as final it is let go.
		h.watch.setPending()
		h.chain.Mine(1)
		h.tick()
		if st := h.w.Status(); st.Tracked != 0 {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("consumer lists a transfer below the cursor that the checkpoint does not know", func(t *testing.T) {
		// The checkpoint is older than the consumer's data in one respect:
		// it does not track a transfer the consumer has. The cursor is
		// rewound to examine its block.
		h := newHarness(t)
		deepFinality(h)
		h.watch.add(addr(1))
		h.tick()
		h.pay(addr(1), 10)
		paid := h.chain.Mine(1)
		h.mineTicking(testSettle + 4)
		pending := asPending(h)
		// Forge the situation: same cursor, but no memory of the transfer.
		h.store.restore(withoutTransfers(t, h.store.snapshot()))
		h.restart()
		h.watch.setPending(pending...)
		// Its block is then replaced.
		h.chain.Reorg(testSettle + 5)
		h.chain.Mine(testSettle + 8)
		h.sink.take()
		h.sink.set(func(s *sink) { s.lenientReverts = false })
		for i := 0; i < 3; i++ {
			if err := h.w.Tick(t.Context()); err == nil {
				break
			}
			h.passCooldowns()
		}
		events := h.sink.take()
		reverted := 0
		for _, e := range events {
			if e.kind == "reverted" && e.tr.BlockHash == paid.Hash {
				reverted++
			}
		}
		if reverted != 1 {
			t.Fatalf("events = %v", events)
		}
		h.assertModel(startBlock)
	})
}

// TestHaltStopsRun: a consumer can stop the watcher for good by wrapping
// ErrHalt, from the sink or from the watch set.
func TestHaltStopsRun(t *testing.T) {
	for _, from := range []string{"sink", "watch set"} {
		h := newHarness(t)
		h.watch.add(addr(1))
		halt := fmt.Errorf("network settings changed: %w", watcher.ErrHalt)
		sleeps := 0
		h.cfg.Sleep = func(ctx context.Context, d time.Duration) error {
			if d > time.Millisecond {
				sleeps++
				if sleeps == 1 && from == "sink" {
					h.pay(addr(1), 5)
					h.chain.Mine(1)
				}
			}
			return ctx.Err()
		}
		if from == "sink" {
			h.cfg.Sink = haltingSink{sink: h.sink, err: halt}
		} else {
			h.cfg.Watch = haltingWatch{WatchSet: h.watch, after: 2, err: halt}
		}
		h.restart()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := h.w.Run(ctx)
		cancel()
		if !errors.Is(err, watcher.ErrHalt) || !strings.Contains(err.Error(), "network settings changed") {
			t.Fatalf("%s: Run returned %v", from, err)
		}
		if sleeps > 3 {
			t.Fatalf("%s: Run kept retrying (%d pauses)", from, sleeps)
		}
	}
}

type haltingSink struct {
	*sink
	err error
}

func (s haltingSink) TransferObserved(context.Context, watcher.Transfer) error { return s.err }

type haltingWatch struct {
	watcher.WatchSet
	after int
	err   error
}

func (w haltingWatch) Watched(ctx context.Context) (watcher.Snapshot, error) {
	if calls.Add(1) > int32(w.after) {
		return watcher.Snapshot{}, w.err
	}
	return w.WatchSet.Watched(ctx)
}

var calls atomic.Int32

// TestStateStaysBounded: over a long chain with a steady trickle of payments
// (and the occasional reorg), what the watcher keeps — the transfers it
// tracks, the checkpoint it writes — does not grow: everything that leaves
// the reorg window is forgotten. A payment server runs for months.
func TestStateStaysBounded(t *testing.T) {
	h := newHarness(t, withNodes(2))
	const addresses = 30
	for i := 0; i < addresses; i++ {
		h.watch.add(addr(i))
	}
	h.tick()

	// A hundred reorg windows — or forty under the race detector, where
	// every one of the thousand ticks costs ten times as much and this test
	// alone was half of the package's run time. What it measures is how
	// much the watcher remembers, which the race detector has no part in.
	blocks := 4000
	if raceDetector {
		blocks = 1600
	}
	maxTracked, maxCheckpoint := 0, 0
	var early int // the largest checkpoint of the first quarter
	paid := 0
	for n := 0; n < blocks; n += 4 {
		h.pay(addr(paid%addresses), int64(1+paid%500))
		paid++
		h.chain.Mine(2)
		h.pay(addr(paid%addresses), int64(1+paid%500))
		paid++
		h.chain.Mine(2)
		if n%400 == 200 {
			for _, tx := range h.chain.Reorg(3) {
				h.chain.Requeue(tx)
			}
			h.chain.Mine(4)
		}
		h.tick()
		st := h.w.Status()
		maxTracked = max(maxTracked, st.Tracked)
		size := len(h.store.snapshot())
		maxCheckpoint = max(maxCheckpoint, size)
		if n < blocks/4 {
			early = maxCheckpoint
		}
	}
	h.assertModel(startBlock)

	// One payment every two blocks: the window holds at most
	// (ReorgWindow + SettleDepth)/2 of them, plus a tick's worth.
	if limit := (testWindow+testSettle)/2 + 8; maxTracked > limit {
		t.Fatalf("tracked transfers peaked at %d, want at most %d (reorg window %d blocks)", maxTracked, limit, testWindow)
	}
	if maxCheckpoint > early+early/10 {
		t.Fatalf("the checkpoint grew from %d bytes (first quarter) to %d bytes", early, maxCheckpoint)
	}
	info, err := watcher.InspectCheckpoint(h.store.snapshot())
	if err != nil || info.Transfers > maxTracked {
		t.Fatalf("checkpoint = %+v, %v", info, err)
	}
	t.Logf("%d blocks, %d payments: at most %d transfers tracked, checkpoint at most %d bytes", blocks, paid, maxTracked, maxCheckpoint)
}

// TestStartPositionSurvivesAFailedFirstTick: without a checkpoint the start
// position is chosen just below the tip and then lowered to the Since of the
// addresses that already exist. If the very first tick fails between the
// two — the watch set cannot be read — the choice must be made again, not
// kept half-made: a payment between Since and the tip would be skipped.
func TestStartPositionSurvivesAFailedFirstTick(t *testing.T) {
	for _, nodes := range []int{1, 2} {
		h := newHarness(t, withNodes(nodes))
		since := h.chain.Head().Number
		h.watch.addSince(addr(1), since)
		h.pay(addr(1), 10)
		h.chain.Mine(3 * testSettle) // the payment is far below tip - SettleDepth
		h.watch.mu.Lock()
		h.watch.fail = 1
		h.watch.mu.Unlock()
		h.tickErr()
		if st := h.w.Status(); st.Cursor != 0 {
			t.Fatalf("%d endpoints: cursor = %d after a first tick that failed before the watch set was read", nodes, st.Cursor)
		}
		h.sink.set(func(s *sink) { s.headRule = nil }) // ruleFrom is above the payment's block here
		h.tick()
		if n := len(h.sink.liveKeys()); n != 1 {
			t.Fatalf("%d endpoints: %d transfers observed, want the one paid before the watcher's first successful tick", nodes, n)
		}
	}
}

// withoutTransfers returns a checkpoint without its tracked transfers: the
// same cursor and anchors, no memory of what was reported.
func withoutTransfers(t *testing.T, checkpoint []byte) []byte {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(checkpoint, &doc); err != nil {
		t.Fatal(err)
	}
	if _, has := doc["transfers"]; !has {
		t.Fatalf("checkpoint tracks no transfers: %s", checkpoint)
	}
	delete(doc, "transfers")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
