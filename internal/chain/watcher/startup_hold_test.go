package watcher_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// The first tick of a process that starts without a checkpoint chooses a
// start position and settles from it. If nothing above that position can be
// settled — here an endpoint claims a chain a thousand blocks longer than
// anybody else has, which only the net under the named holds catches — the
// head is held where the process started, and the status says so, as it does
// after a restart with the checkpoint. Measured from a cursor of zero, that
// tick counted as the head having moved: a process restarted without its
// checkpoint again and again (the randomised model's restart without the
// checkpoint, while one endpoint lagged) held its head without a reason, tick
// after tick. The same when the first tick rewinds below its start for an
// address that can have been paid lower and settles back up to it, and when
// a running watcher does that for an address that has just appeared: the
// rewind was work, but once it is back where it began and goes no further,
// the head is held.
func TestTheFirstTickWithoutACheckpointNamesWhatHoldsTheHead(t *testing.T) {
	const reason = "reports the chain at block"
	for _, tc := range []struct {
		name          string
		consumerStart bool // the consumer names the block after its processed head
		lowSince      bool // an address that can have been paid below the start
	}{
		{name: "first start"},
		{name: "the consumer's start block", consumerStart: true},
		{name: "an address paid below the start", lowSince: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withNodes(2))
			h.watch.add(addr(1))
			h.mineTicking(testSettle + 4)
			if tc.lowSince {
				h.watch.addSince(addr(2), h.w.Status().Cursor-3)
				h.tick()
			}
			h.nodes[0].Set(func(b *evmtest.Behavior) {
				b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
					if method == "eth_blockNumber" {
						return hexUint(h.chain.Head().Number + 1000), nil, true
					}
					return nil, nil, false
				}
			})
			h.tick()
			held := h.w.Status().Cursor
			if hold := h.w.Status().Hold; !strings.Contains(hold, reason) {
				t.Fatalf("test setup: hold %q", hold)
			}
			for i := 0; i < 3; i++ {
				h.store.restore(nil)
				if tc.consumerStart {
					start := held + 1
					h.cfg.StartBlock = &start
				}
				h.restart()
				h.tick()
				st := h.w.Status()
				if st.Cursor != held {
					t.Fatalf("restart %d without the checkpoint: cursor %d, want %d", i+1, st.Cursor, held)
				}
				if !strings.Contains(st.Hold, reason) {
					t.Fatalf("restart %d without the checkpoint: the first tick held the head at block %d without a reason (hold %q)", i+1, st.Cursor, st.Hold)
				}
			}
			// Running: an address appears that can have been paid three
			// blocks down. The tick settles them again, and is then where
			// it began.
			h.watch.addSince(addr(3), held-3)
			h.tick()
			if st := h.w.Status(); st.Cursor != held || !strings.Contains(st.Hold, reason) {
				t.Fatalf("after the rewind for a new address: cursor %d (was %d), hold %q", st.Cursor, held, st.Hold)
			}
		})
	}
}

// A process restarted without its checkpoint starts where the consumer's
// processed head is; an address can have been paid below that, so its first
// tick rewinds to the address's Since and settles the blocks again. On the
// way up it meets the transfer in the very block it started from, which the
// other endpoint cannot confirm now (its receipts are gone). The cursor
// moved in that tick — from the rewind up to the block below the transfer —
// but it ended below where the process started, and stays there: the status
// says it is the transfer waiting for its confirmation, from the first tick
// on.
func TestTheFirstTickWithoutACheckpointNamesATransferThatHoldsTheHeadBelowItsStart(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.mineTicking(testSettle + 4)
	h.watch.addSince(addr(1), h.w.Status().Cursor)
	h.tick()
	h.pay(addr(1), 25)
	paid := h.chain.Mine(1)
	h.mineTicking(testSettle + 4)
	if got := h.sink.count("observed"); got != 1 {
		t.Fatalf("test setup: %d transfers observed", got)
	}
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.HideReceipts = true })
	for i := 0; i < 2; i++ {
		h.store.restore(nil)
		start := paid.Number + 1
		h.cfg.StartBlock = &start
		h.restart()
		h.tick()
		st := h.w.Status()
		if st.Cursor != paid.Number-1 {
			t.Fatalf("restart %d: cursor %d, want %d (below the unconfirmed transfer)", i+1, st.Cursor, paid.Number-1)
		}
		if !strings.Contains(st.Hold, "waiting to be confirmed") {
			t.Fatalf("restart %d: the first tick held the head at block %d, below the block %d it started from, without a reason (hold %q)",
				i+1, st.Cursor, paid.Number, st.Hold)
		}
	}
}

// The same first tick, and the second endpoint's log index trails its node:
// its logs vouch for the blocks up to a settle depth below the height its log
// backend has reached, which lies below the block the process started from.
// The rewound tick settles up to there and no further, and says why — as a
// running watcher does when the second endpoint's logs do not reach the
// blocks it is to settle.
func TestTheFirstTickWithoutACheckpointNamesASecondLogIndexThatHoldsTheHeadBelowItsStart(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.mineTicking(testSettle + 20)
	processed := h.w.Status().Cursor
	const lag = testSettle + 3
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsBackendLag = lag })
	for i := 0; i < 2; i++ {
		h.store.restore(nil)
		start := processed + 1
		h.cfg.StartBlock = &start
		h.restart()
		h.tick()
		st := h.w.Status()
		if want := h.chain.Head().Number - lag - testSettle; st.Cursor != want {
			t.Fatalf("restart %d: cursor %d, want %d (a settle depth below the second endpoint's logs)", i+1, st.Cursor, want)
		}
		if !strings.Contains(st.Hold, "confirmed by a second endpoint") {
			t.Fatalf("restart %d: the first tick held the head at block %d, below the block %d it started from, without a reason (hold %q)",
				i+1, st.Cursor, processed, st.Hold)
		}
	}
}

// A tick that has been rewound for a new address settles the blocks again up
// to where it began, as far as the endpoints showed the chain when it came to
// settle. While it repeats the log scan on the second endpoint, a burst of
// blocks is mined, and that endpoint's answer shows them. They are for the
// next tick to settle; this one held nothing, and says nothing is held. (The
// net under the named holds had counted them, and named a hold for one tick:
// "nothing above block … could be settled".)
func TestBlocksMinedWhileATickSettlesAreTheNextTicks(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 4)
	armed := false
	burst := func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
		if method == "eth_getLogs" && armed {
			armed = false
			h.chain.Mine(3 * testSettle)
			// And the pool's monitor sees them on the endpoint in use, as
			// it does whenever it looks.
			_, _ = h.client.Endpoints()[0].BlockNumber(context.Background())
		}
		return nil, nil, false
	}
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Intercept, b.InterceptBatch = burst, burst })
	was := h.w.Status().Cursor
	h.watch.addSince(addr(2), was-2)
	armed = true
	h.tick()
	if armed {
		t.Fatal("test setup: the second endpoint was not asked for logs")
	}
	if st := h.w.Status(); st.Cursor != was || st.Hold != "" {
		t.Fatalf("cursor %d (began at %d), hold %q", st.Cursor, was, st.Hold)
	}
	h.tick()
	if st := h.w.Status(); st.Cursor <= was {
		t.Fatalf("the next tick did not settle the blocks mined meanwhile: cursor %d", st.Cursor)
	}
}
