package watcher_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// The time of the final block decides when an address may leave the
// consumer's watch list, for good. It is taken only from a header two
// endpoints report alike — not from a block whose hash two endpoints agree
// on, with a timestamp one of them decoded. Here the endpoint in use reports
// its finalized block with the right hash and a timestamp two seconds late:
// no head carries that time, and the endpoints are named as disagreeing.
func TestTheFinalBlocksTimeIsOneTwoEndpointsReport(t *testing.T) {
	const depth = testSettle + 4 // finality behind the processed head
	h := newHarness(t, withNodes(2))
	for _, n := range h.nodes {
		n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = depth })
	}
	h.watch.add(addr(1))
	h.mineTicking(depth + 2)
	lie := func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
		if method != "eth_getBlockByNumber" {
			return nil, nil, false
		}
		tag, n, numeric := blockParam(params)
		final := h.chain.Head().Number - depth // the nodes' finalized block
		if tag == "finalized" {
			n, numeric = final, true
		}
		blk, ok := h.chain.BlockByNumber(n)
		if !numeric || !ok || n != final {
			return nil, nil, false
		}
		return headerJSON(blk, blk.Time+2), nil, true
	}
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.Intercept, b.InterceptBatch = lie, lie })
	before := h.sink.lastHead()
	h.sink.take()
	for i := 0; i < 4; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
	}
	var heads []watcher.Head
	for _, e := range h.sink.take() {
		if e.kind == "head" {
			heads = append(heads, e.head)
		}
	}
	if len(heads) == 0 || before.Final >= before.Number {
		t.Fatalf("test setup: %d heads; before the lie, head %d final %d", len(heads), before.Number, before.Final)
	}
	for _, hd := range heads {
		blk, _ := h.chain.BlockByNumber(hd.Final)
		if !hd.FinalTime.IsZero() && hd.FinalTime.Unix() != int64(blk.Time) {
			t.Fatalf("head %d: final block %d at %s, the block's time is %d", hd.Number, hd.Final, hd.FinalTime, blk.Time)
		}
	}
	if d := h.w.Status().FinalityDispute; !strings.Contains(d, "another block at that height") {
		t.Fatalf("finality dispute %q", d)
	}
}

// On BSC the chain finalises a block or two behind its tip, long before the
// processed head (a settle depth behind the tip) gets there, and providers
// differ by a block in what they call finalized. A transfer in the block
// between their two finalized blocks — near the tip, above the processed
// head — is kept from being final by nothing: the head's final block is the
// processed head either way. So that difference is no finality dispute, names
// nobody, and a third endpoint is not asked about it. (It was, whenever a
// transfer the watcher followed lay between the two: on busy addresses on
// mainnet, in one status sample in five.)
func TestAFinalizedBlockAboveTheHeadCostsNothing(t *testing.T) {
	for _, nodes := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d endpoints", nodes), func(t *testing.T) {
			h := newHarness(t, withNodes(nodes))
			for i, d := range []uint64{3, 2, 2}[:nodes] {
				h.nodes[i].Set(func(b *evmtest.Behavior) { b.FinalityDepth = d })
			}
			h.watch.add(addr(1))
			h.mineTicking(testSettle + 4)
			for i := 0; i < 6; i++ {
				h.pay(addr(1), int64(1+i))
				h.chain.Mine(1)
				for _, n := range h.nodes {
					n.ResetCounters()
				}
				h.tick()
				st := h.w.Status()
				if st.FinalityDispute != "" {
					t.Fatalf("finality a block or two behind the tip, above the head: dispute %q", st.FinalityDispute)
				}
				if nodes == 3 && h.nodes[2].Calls("eth_getBlockByNumber") > 0 {
					t.Fatalf("the third endpoint was asked about finality: %d header request(s)", h.nodes[2].Calls("eth_getBlockByNumber"))
				}
			}
			if hd := h.sink.lastHead(); hd.Final != hd.Number || h.sink.count("observed") == 0 {
				t.Fatalf("test setup: head %d final %d, %d transfers", hd.Number, hd.Final, h.sink.count("observed"))
			}
		})
	}
}
