package watcher_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// TestScanCallersHonourNext: evm.TransferLogs may return, without an error,
// a scan that ends below the block it was asked to reach — at the height of
// the backend that answered (TransferScan.Next). That is a result, and a
// caller that took it for the whole range would skip the blocks from Next on
// for good. The watcher scans in three places; in each of them the blocks a
// short scan did not cover stay unsettled (or, for the re-check, on the
// books), and a payment in them is found when they are covered.
func TestScanCallersHonourNext(t *testing.T) {
	// freeze keeps a node's log backend where the chain is now, however far
	// the chain grows: the lag is restated before every tick.
	freeze := func(h *harness, node int, refuse bool) (at uint64, restate func()) {
		at = h.chain.Head().Number
		return at, func() {
			h.nodes[node].Set(func(b *evmtest.Behavior) {
				b.LogsBackendLag = h.chain.Head().Number - at
				b.RefuseBeyondHead = refuse
			})
		}
	}

	for _, refuse := range []bool{false, true} {
		name := "the node truncates silently and the batch gives its height away"
		if refuse {
			name = "the node refuses the range and states its head"
		}
		t.Run("the scan above the cursor/"+name, func(t *testing.T) {
			h := newHarness(t)
			h.watch.add(addr(1))
			h.mineTicking(testSettle + 2)
			at, restate := freeze(h, 0, refuse)
			h.pay(addr(1), 10)
			paid := h.chain.Mine(1).Number // one above the log backend
			for i := 0; i < testSettle+3; i++ {
				h.chain.Mine(1)
				restate()
				h.tick()
				st := h.w.Status()
				if st.Cursor > at-testSettle || len(h.sink.liveKeys()) != 0 {
					t.Fatalf("log backend at %d, payment in %d: cursor %d, %d transfers reported", at, paid, st.Cursor, len(h.sink.liveKeys()))
				}
			}
			h.nodes[0].Set(func(b *evmtest.Behavior) { b.LogsBackendLag = 0 })
			h.tick()
			h.assertModel(startBlock)
			if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle || len(h.sink.liveKeys()) != 1 {
				t.Fatalf("after the backend caught up: %+v", st)
			}
		})

		t.Run("the second scan before settling/"+name, func(t *testing.T) {
			h := newHarness(t, withNodes(2))
			h.watch.add(addr(1))
			h.mineTicking(testSettle + 2)
			at, restate := freeze(h, 1, refuse)
			// The primary hides the payment: only the second scan can find
			// it, and the second endpoint's logs end below its block.
			h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } })
			h.pay(addr(1), 10)
			paid := h.chain.Mine(1).Number
			for i := 0; i < testSettle+3; i++ {
				h.chain.Mine(1)
				restate()
				_ = h.w.Tick(context.Background())
				if st := h.w.Status(); st.Cursor >= paid || st.Cursor > at-testSettle {
					t.Fatalf("second endpoint's logs end at %d, payment in %d: cursor %d", at, paid, st.Cursor)
				}
				h.passCooldowns()
			}
			h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsBackendLag = 0 })
			for i := 0; i < 3; i++ {
				_ = h.w.Tick(context.Background())
				h.passCooldowns()
			}
			h.assertModel(startBlock)
			if st := h.w.Status(); st.Cursor < paid || len(h.sink.liveKeys()) != 1 {
				t.Fatalf("after the backend caught up: %+v", st)
			}
		})
	}

	t.Run("the re-check of blocks settled on one endpoint's logs", func(t *testing.T) {
		// The unchecked range is worked off one provider-sized step per
		// tick. What a step did not cover stays on the books.
		var in incidents
		h := newHarness(t, withNodes(2), withIncidents(&in))
		h.watch.addSince(addr(1), h.chain.Head().Number)
		h.noLogService(1, &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"})
		h.mineTicking(testSettle + 2)
		first := h.w.Status().UncheckedFrom
		h.mineTicking(8)
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } })
		h.pay(addr(1), 100)
		paid := h.chain.Mine(1).Number
		h.sink.set(func(s *sink) { s.headRule = nil }) // one log endpoint: the head passes what it hides
		h.mineTicking(testSettle + 4)
		if got := h.w.Status().UncheckedFrom; got != first || first == 0 || len(h.sink.liveKeys()) != 0 {
			t.Fatalf("test setup: unchecked from %d (was %d), %d transfers", got, first, len(h.sink.liveKeys()))
		}
		// The second endpoint starts serving logs, two blocks per call —
		// which it makes known when it is asked for the five blocks that
		// are waiting to be settled.
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError, b.MaxLogRange = nil, 2 })
		h.passLogPause()
		h.chain.Mine(4)
		steps := 0
		for ; len(h.sink.liveKeys()) == 0; steps++ {
			if steps > 40 {
				t.Fatalf("the hidden payment was not found: %+v", h.w.Status())
			}
			h.chain.Mine(1)
			_ = h.w.Tick(context.Background())
			if st := h.w.Status(); len(h.sink.liveKeys()) == 0 && (st.UncheckedFrom == 0 || st.UncheckedFrom > paid) {
				t.Fatalf("step %d: the unchecked range begins at %d now; the payment in block %d was skipped", steps, st.UncheckedFrom, paid)
			}
		}
		if steps < 3 {
			t.Fatalf("test setup: the re-check took %d steps; it was meant to be worked off in several", steps)
		}
		if in.count(watcher.IncidentLateTransfer) != 1 {
			t.Fatalf("incidents = %+v", in.list)
		}
	})
}

// TestTwoTransfersAtOnePosition: a block has one log per index. If a transfer
// passes verification although the watcher tracks another transaction's
// transfer at that very position of that very block — which takes two
// endpoints that lie, here: one whose logs show an invented transfer in place
// of the real one, one whose receipt confirms the invention — the watcher
// does not report it and does not record it: the checkpoint never holds a
// pair that cannot both be true (and that decodeState refuses), and the head
// waits below the block.
func TestTwoTransfersAtOnePosition(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(2), withIncidents(&in))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)
	h.pay(addr(1), 10)
	paid := h.chain.Mine(1)
	h.tick()
	real := h.sink.liveKeys()
	if len(real) != 1 {
		t.Fatalf("test setup: %d transfers observed", len(real))
	}
	var genuine watcher.Transfer
	for _, tr := range real {
		genuine = tr
	}

	// The invention: another transaction, a hundred times the amount, at the
	// same log index of the same block.
	fake := fabricated(h, paid.Number, 1, 1000)
	fake.LogIndex = genuine.LogIndex
	fakeReceipt := map[string]any{
		"transactionHash": fake.TxHash.String(), "blockHash": fake.BlockHash.String(), "blockNumber": hexUint(fake.BlockNumber), "status": "0x1",
		"logs": []any{map[string]any{
			"address": fake.Address.Hex(), "topics": []string{fake.Topics[0].String(), fake.Topics[1].String(), fake.Topics[2].String()},
			"data": "0x" + hexBytes(fake.Data), "blockNumber": hexUint(fake.BlockNumber), "blockHash": fake.BlockHash.String(),
			"transactionHash": fake.TxHash.String(), "transactionIndex": "0x0", "logIndex": hexUint(fake.LogIndex), "removed": false,
		}},
	}
	h.nodes[1].Set(func(b *evmtest.Behavior) {
		b.DropLogs = func(l evm.Log) bool { return l.TxHash == genuine.TxHash }
		b.ExtraLogs = []evm.Log{fake}
		b.HideReceipts = true
	})
	h.nodes[0].Set(func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			var tx string
			if method == "eth_getTransactionReceipt" && len(params) == 1 && json.Unmarshal(params[0], &tx) == nil && tx == fake.TxHash.String() {
				return fakeReceipt, nil, true
			}
			return nil, nil, false
		}
	})

	for i := 0; i < testSettle+4; i++ {
		h.chain.Mine(1)
		if err := h.w.Tick(context.Background()); errors.Is(err, watcher.ErrCheckpoint) {
			t.Fatalf("tick %d: %v", i, err)
		}
		for k, tr := range h.sink.liveKeys() {
			if k != keyOf(genuine) {
				t.Fatalf("tick %d: a second transfer was reported at log index %d of block %d: tx %s, amount %s", i, tr.LogIndex, tr.BlockNumber, tr.TxHash, tr.Amount)
			}
		}
		if _, err := watcher.InspectCheckpoint(h.store.snapshot()); err != nil {
			t.Fatalf("tick %d: the watcher wrote a checkpoint it will not read: %v", i, err)
		}
		if got := h.sink.lastHead().Number; got >= paid.Number {
			t.Fatalf("tick %d: head %d passed block %d, about which the endpoints contradict each other", i, got, paid.Number)
		}
		h.passCooldowns()
	}
	if st := h.w.Status(); st.Hold == "" || in.count(watcher.IncidentLogMismatch) == 0 || in.count(watcher.IncidentHeadHeld) == 0 {
		t.Fatalf("hold %q, incidents %+v", st.Hold, in.list)
	}
	// A restart reads the checkpoint; and when the endpoints tell the truth
	// again, everything is as the chain has it.
	h.restart()
	h.nodes[0].Set(honest)
	h.nodes[1].Set(func(b *evmtest.Behavior) { honest(b); b.HideReceipts = false })
	h.passCooldowns()
	h.mineTicking(2)
	h.assertModel(startBlock)
	if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle {
		t.Fatalf("status = %+v", st)
	}
}

func hexBytes(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}
