package watcher_test

import (
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// A transfer is recorded before it is reported (write-ahead), so that a
// crash between the two cannot leave the sink with a fact the watcher has
// forgotten. When the sink refuses the report, the record stays — and a
// transfer that only a second endpoint's logs show, or only the scan that
// makes up for blocks settled on one endpoint's logs, must still be reported
// the next time that scan shows it: being on record is not being reported.

// TestTransferTheSinkRefusedIsReportedWhenTheSecondScanShowsItAgain: the
// primary leaves a payment out of its logs; the second endpoint's scan of
// the blocks being settled has it. The sink refuses it once. The head does
// not pass its block before the sink has it.
func TestTransferTheSinkRefusedIsReportedWhenTheSecondScanShowsItAgain(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.mineTicking(2)
	tx := h.pay(addr(1), 30)
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(l evm.Log) bool { return l.TxHash == tx } })
	paid := h.chain.Mine(1)
	refused := false
	for i := 0; i < 4*testSettle; i++ {
		h.chain.Mine(1)
		if !refused && h.w.Status().Cursor+testSettle+1 >= paid.Number {
			// The tick that settles the block reports the transfer the
			// second endpoint found: the sink refuses it.
			h.sink.set(func(s *sink) { s.failObserved = 1 })
			refused = true
		}
		_ = h.w.Tick(t.Context())
	}
	if _, found := findTransfer(h, tx); !found {
		t.Fatalf("the transfer in block %d, which the second endpoint's logs show and the sink refused once, was never reported (cursor %d)", paid.Number, h.w.Status().Cursor)
	}
}

// TestTransferTheSinkRefusedIsReportedWhenTheMakeUpScanShowsItAgain: blocks
// settled on a lone endpoint's logs, which left a payment out; a second
// endpoint is added and makes up for them, and finds the payment. The sink
// refuses it once. It is reported all the same, below the head.
func TestTransferTheSinkRefusedIsReportedWhenTheMakeUpScanShowsItAgain(t *testing.T) {
	h := newHarness(t)
	h.sink.set(func(s *sink) { s.headRule = nil }) // a lone endpoint is trusted with what it hides
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.mineTicking(3)
	tx := h.pay(addr(1), 70)
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(l evm.Log) bool { return l.TxHash == tx } })
	paid := h.chain.Mine(1)
	h.mineTicking(testSettle + 4)
	if st := h.w.Status(); st.Cursor <= paid.Number || len(h.sink.liveKeys()) != 0 {
		t.Fatalf("test setup: %+v", st)
	}
	fresh := evmtest.NewNode(h.chain)
	h.urls = append(h.urls, fresh.Serve(t))
	h.restartWithNewClient()
	h.sink.set(func(s *sink) { s.failObserved = 1 })
	for i := 0; i == 0 || i < 20 && h.w.Status().UncheckedFrom != 0; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(t.Context())
	}
	for range 3 {
		h.chain.Mine(1)
		_ = h.w.Tick(t.Context())
	}
	if _, found := findTransfer(h, tx); !found {
		t.Fatalf("the payment the lone endpoint hid in block %d was found by the make-up scan, refused by the sink once, and never reported (status %+v)", paid.Number, h.w.Status())
	}
}
