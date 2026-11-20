package watcher_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// minesWhenAskedForItsHead makes a node's chain grow by n blocks in the
// moment the node is asked for its latest block — once, when armed: the
// blocks that are produced while a tick is asking one endpoint after the
// other where the chain ends.
func minesWhenAskedForItsHead(h *harness, n int, armed *bool) func(b *evmtest.Behavior) {
	return func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if tag, _, _ := blockParam(params); method == "eth_getBlockByNumber" && tag == "latest" && *armed {
				*armed = false
				h.chain.Mine(n)
			}
			return nil, nil, false
		}
	}
}

// TestPrimaryIsNotBlamedForBlocksMinedWhileTheOthersWereAsked is the cause of
// a test that failed now and then under load (TestManyPaymentsWhileRunning in
// package corebridge: an endpoint_fault alert nobody had earned), and it was
// the watcher's, not the test's.
//
// With three endpoints, when the primary's head and the verifier's are more
// than a settle depth apart, the third decides, and an endpoint that is more
// than a settle depth below the height two of them have reached is set aside
// with an incident. But the three heads are read one after the other, the
// primary's first — and the comparison took them as read in the same moment.
// Every block the chain produced while the verifier and the third endpoint
// were being asked was counted as the primary's lag. An endpoint that trails
// by less than the settle depth, which is what the settle depth is there to
// absorb, was declared at fault when two more blocks arrived in that time; a
// primary that is not behind at all could be, when another endpoint took a
// settle depth's worth of blocks to answer (on BSC: 7 seconds).
//
// The primary is now asked again before it is set aside, as the last of the
// three: only what it says then is measured against the others.
func TestPrimaryIsNotBlamedForBlocksMinedWhileTheOthersWereAsked(t *testing.T) {
	setup := func(t *testing.T, lag uint64) (*harness, *incidents, *bool) {
		in := &incidents{}
		h := newHarness(t, withNodes(3), withIncidents(in))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		if st := h.w.Status(); st.Endpoint != evm.RedactURL(h.urls[0]) {
			t.Fatalf("test setup: the primary is %s", st.Endpoint)
		}
		armed := false
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.Lag = lag })
		h.nodes[1].Set(minesWhenAskedForItsHead(h, 2, &armed))
		return h, in, &armed
	}

	// One block short of the settle depth behind: tolerated, and still
	// tolerated when two blocks are mined while the others are asked.
	t.Run("a primary that trails by less than the settle depth", func(t *testing.T) {
		h, in, armed := setup(t, testSettle-1)
		h.pay(addr(1), 12)
		h.chain.Mine(1)
		for _, n := range h.nodes {
			n.ResetCounters()
		}
		*armed = true
		if err := h.w.Tick(context.Background()); err != nil {
			t.Fatalf("the tick failed: %v", err)
		}
		if *armed {
			t.Fatal("test setup: the second endpoint was not asked for its head")
		}
		if n := in.count(watcher.IncidentHeightMismatch); n != 0 {
			t.Fatalf("%d height-mismatch incident(s) against an endpoint that is within the settle depth: %+v", n, in.list)
		}
		for i, es := range h.client.Status() {
			if es.State == evm.StateFailing || !es.Healthy && es.State != evm.StateLagging {
				t.Fatalf("endpoint %d was set aside: %+v", i, es)
			}
		}
		// It took the vote of the third endpoint, and one more question to
		// the primary; the tick went on from what the primary said then.
		st := h.w.Status()
		if st.Endpoint != evm.RedactURL(h.urls[0]) || h.nodes[2].TotalCalls() == 0 {
			t.Fatalf("primary %s, third endpoint asked %d times", st.Endpoint, h.nodes[2].TotalCalls())
		}
		if want := h.chain.Head().Number - (testSettle - 1); st.Tip.Number != want {
			t.Fatalf("the tick went on from block %d; the primary, asked again, is at %d", st.Tip.Number, want)
		}
		for i := 0; i < testSettle+2; i++ {
			h.chain.Mine(1)
			h.tick()
		}
		h.assertModel(startBlock)
	})

	// An endpoint that is behind is still behind when it is asked again, and
	// is set aside as before.
	t.Run("a primary that is behind by more than the settle depth", func(t *testing.T) {
		h, in, armed := setup(t, testSettle+3)
		h.chain.Mine(1)
		*armed = true
		err := h.w.Tick(context.Background())
		if !errors.Is(err, watcher.ErrInconsistentEndpoint) || !strings.Contains(err.Error(), "blocks behind") {
			t.Fatalf("tick: %v", err)
		}
		if blamed := in.blamed(watcher.IncidentHeightMismatch); len(blamed) != 1 || blamed[0] != evm.RedactURL(h.urls[0]) {
			t.Fatalf("height-mismatch incidents blame %v", blamed)
		}
		// The next tick starts from another endpoint.
		h.chain.Mine(1)
		h.tick()
		if st := h.w.Status(); st.Endpoint == evm.RedactURL(h.urls[0]) {
			t.Fatalf("the endpoint that is behind is still the primary: %+v", st)
		}
	})

	// Asking again is no way to hold the head with three endpoints
	// (invariant 4): a primary that claims a short chain and, asked again, the
	// real one is taken at its second word, and the head moves.
	t.Run("a primary that says low and then high", func(t *testing.T) {
		in := &incidents{}
		h := newHarness(t, withNodes(3), withIncidents(in))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		asked, lying := 0, false
		h.nodes[0].Set(func(b *evmtest.Behavior) {
			b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
				tag, _, numeric := blockParam(params)
				if method != "eth_getBlockByNumber" || numeric {
					return nil, nil, false
				}
				if tag == "latest" {
					asked++
					lying = asked%2 == 1 // the second time: the truth
				}
				if !lying {
					return nil, nil, false
				}
				n := h.chain.Head().Number - 50
				if tag == "finalized" {
					n -= 2
				}
				blk, _ := h.chain.BlockByNumber(n)
				return headerJSON(blk, blk.Time), nil, true
			}
		})
		for i := 0; i < 4; i++ {
			before := h.w.Status().Cursor
			h.chain.Mine(1)
			h.tick()
			if st := h.w.Status(); st.Cursor != before+1 || st.Hold != "" {
				t.Fatalf("tick %d: the head did not move (cursor %d -> %d, hold %q)", i, before, st.Cursor, st.Hold)
			}
		}
		if asked != 8 {
			t.Fatalf("the primary was asked for its head %d times in 4 ticks, want 8", asked)
		}
	})
}
