package watcher_test

import (
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// A process restarted without its checkpoint is started the way the bridge
// starts it: from the block after the consumer's final block
// (Config.StartBlock). Here the consumer has processed less than the chain
// has mined — the endpoints were down while four payments were made — and
// finality has stopped, so its final block is further down still. The new
// process scans from there and reports every one of the four before a head
// above them. (Without a start block it would begin at the settle point of
// the chain, as on a first start, when no deposit address has existed
// before; a consumer that has processed blocks always names one.)
func TestAProcessRestartedWithoutItsCheckpointScansFromTheConsumersFinalBlock(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.add(addr(1), addr(2))
	h.mineTicking(testSettle + 4)
	final := h.sink.lastHead().Final
	for _, n := range h.nodes {
		n.Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
	}
	h.chain.Mine(5)
	for i := 0; i < 4; i++ {
		h.pay(addr(1+i%2), int64(10+i))
		h.chain.Mine(3)
	}
	h.chain.Mine(testSettle)

	h.store.restore(nil)
	start := final + 1
	h.cfg.StartBlock = &start
	h.cfg.FinalizedFloor = final
	h.restart()
	h.tick() // the head rule of the harness: no head above a transfer not reported
	if n := h.sink.count("observed"); n != 4 {
		t.Fatalf("%d of the 4 transfers reported; head %d", n, h.sink.lastHead().Number)
	}
	if hd := h.sink.lastHead(); hd.Number != h.chain.Head().Number-testSettle || hd.Final != final {
		t.Fatalf("head %+v", hd)
	}
}
