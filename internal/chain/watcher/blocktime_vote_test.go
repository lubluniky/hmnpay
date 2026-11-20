package watcher_test

import (
	"encoding/json"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// withoutBlockInBatches makes a node answer a request for block n by number,
// made as a member of a batch, with nothing — or, with otherHash, with
// another block. Asked on its own it answers truthfully, and so it does
// everything else.
func withoutBlockInBatches(chain *evmtest.Chain, n uint64, otherHash bool) func(b *evmtest.Behavior) {
	return func(b *evmtest.Behavior) {
		b.InterceptBatch = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			if _, m, numeric := blockParam(params); !numeric || m != n {
				return nil, nil, false
			}
			blk, ok := chain.BlockByNumber(n)
			if ok && otherHash {
				blk.Hash[0] ^= 0xff
				return headerJSON(blk, blk.Time), nil, true
			}
			return nil, nil, true
		}
	}
}

// TestThirdEndpointDatesATransferTheVerifierCannot: a transfer is reported
// with the time of its block, which two endpoints have to report identically.
// With three endpoints one that does not have the block — or has another one
// at that height — does not keep the transfer from being dated: the third is
// asked. Here the verifier answers so only when asked in a batch (as the
// headers of blocks with transfers are asked for), and truthfully when asked
// for the block alone — so nothing else ever finds it wrong, and with the two
// answers all there was to go by, the transfer held the head below its block
// for as long as the verifier kept answering so.
func TestThirdEndpointDatesATransferTheVerifierCannot(t *testing.T) {
	for _, otherHash := range []bool{false, true} {
		name := "the verifier lacks the block"
		if otherHash {
			name = "the verifier has another block at that height"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, withNodes(3))
			h.watch.addSince(addr(1), h.chain.Head().Number)
			h.mineTicking(2)
			if st := h.w.Status(); st.Endpoint != evm.RedactURL(h.urls[0]) || st.Verifier != evm.RedactURL(h.urls[1]) {
				t.Fatalf("test setup: primary %s, verifier %s", st.Endpoint, st.Verifier)
			}
			tx := h.pay(addr(1), 40)
			paid := h.chain.Mine(1)
			h.nodes[1].Set(withoutBlockInBatches(h.chain, paid.Number, otherHash))
			h.chain.Mine(1)
			h.tick()
			if _, found := findTransfer(h, tx); !found {
				st := h.w.Status()
				t.Errorf("the transfer in block %d was not reported in the tick that found it: %d unverified, hold %q", paid.Number, st.Unverified, st.Hold)
			}
			for i := 0; i < 2*testSettle; i++ {
				h.chain.Mine(1)
				h.tick()
			}
			if _, found := findTransfer(h, tx); !found {
				st := h.w.Status()
				t.Fatalf("the transfer in block %d was not reported: %d unverified, hold %q (primary %s, verifier %s)",
					paid.Number, st.Unverified, st.Hold, st.Endpoint, st.Verifier)
			}
			if st := h.w.Status(); st.Cursor <= paid.Number || st.Unverified != 0 {
				t.Fatalf("the head did not pass block %d: %+v", paid.Number, st)
			}
			h.assertModel(startBlock)
		})
	}
}
