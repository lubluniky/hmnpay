package watcher_test

// Endpoints that misstate the chain's height — a log backend that trails its
// node, a "latest" block that stops, a block number inflated by a thousand,
// a node that is far behind at the start — and blocks settled on one
// endpoint's logs. Each test first describes the attack as it worked against
// an earlier version of the watcher, then asserts the safe behaviour.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// Three endpoints, two of them honest. The primary answers every
// header request truthfully, but the eth_blockNumber that travels in the same
// batch as its eth_getLogs — the "height of the backend that answered the log
// query" — is frozen a couple of blocks above the cursor. The scan then covers
// nothing beyond that height, safeHead stays below cursor+SettleDepth, and
// settle() returns without a reason: the tick SUCCEEDS, Status.Hold is empty,
// no incident is raised, the endpoint is never set aside, and no payment in a
// newer block is ever looked at. Invariant 4 says a held head is never held
// silently and that one of three endpoints cannot hold it.
func TestAStuckLogBackendOnThePrimaryDoesNotHoldTheHeadSilently(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(3), withIncidents(&in))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)

	frozen := h.w.Status().Cursor + 2
	h.nodes[0].Set(func(b *evmtest.Behavior) {
		b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method == "eth_blockNumber" {
				return hexUint(frozen), nil, true
			}
			return nil, nil, false
		}
	})

	h.pay(addr(1), 100)
	paid := h.chain.Mine(1)
	failed := 0
	for i := 0; i < 8*testSettle; i++ {
		h.chain.Mine(1)
		if err := h.w.Tick(context.Background()); err != nil {
			failed++
		}
		h.clock.Advance(3 * time.Second) // a poll interval: 2.4 minutes in all
	}
	st := h.w.Status()
	head := h.sink.lastHead().Number
	t.Logf("paid in block %d, tip %d, processed head %d, cursor %d, failed ticks %d, primary %s, hold %q, last error %q, incidents %d, endpoint 0 healthy %v, payments seen %d",
		paid.Number, h.chain.Head().Number, head, st.Cursor, failed, st.Endpoint, st.Hold, st.LastError, len(in.list),
		h.client.Status()[0].Healthy, len(h.sink.liveKeys()))
	if head >= paid.Number && len(h.sink.liveKeys()) == 1 {
		return // the endpoint was set aside and the watcher went on: safe
	}
	if st.Hold == "" && in.count(watcher.IncidentHeadHeld) == 0 && failed == 0 {
		t.Fatalf("one of three endpoints has held the processed head at %d for %d ticks (%d blocks behind the tip, payment in block %d never seen) and nothing says so: every tick succeeded, Status.Hold is empty, no incident was raised",
			head, 8*testSettle, h.chain.Head().Number-head, paid.Number)
	}
	if head < paid.Number {
		t.Errorf("with three endpoints, one endpoint held the head below block %d for %d ticks (hold %q)", paid.Number, 8*testSettle, st.Hold)
	}
}

// VerifyBestEffort, two endpoints, both up and answering. The second
// one answers one method with a lie: eth_blockNumber is a thousand blocks too
// high. Client.Refresh measures every endpoint's lag against the highest
// number any endpoint reports, so the honest endpoint is now "lagging": the
// liar becomes the primary although it is listed second, and — because
// best-effort verification only consults healthy endpoints — it has no
// verifier at all. A payment it invents, with a receipt it invents, is
// reported. The documented price of best effort is "a malicious primary only
// has to wait for the others to be down"; it does not have to wait.
func TestOneInflatedBlockNumberDoesNotMakeTheHonestEndpointLagging(t *testing.T) {
	h := newHarness(t, withNodes(2), withConfig(func(c *watcher.Config) { c.Verify = watcher.VerifyBestEffort }))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)

	h.chain.Mine(3)
	fake := fabricated(h, h.chain.Head().Number, 1, 1000)
	topics := make([]string, len(fake.Topics))
	for i, tp := range fake.Topics {
		topics[i] = tp.String()
	}
	logJSON := map[string]any{
		"address": fake.Address.Hex(), "topics": topics, "data": "0x" + hex.EncodeToString(fake.Data),
		"blockNumber": hexUint(fake.BlockNumber), "blockHash": fake.BlockHash.String(),
		"transactionHash": fake.TxHash.String(), "transactionIndex": "0x0", "logIndex": "0x0", "removed": false,
	}
	h.nodes[1].Set(func(b *evmtest.Behavior) {
		b.ExtraLogs = []evm.Log{fake}
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			switch {
			case method == "eth_blockNumber":
				return hexUint(h.chain.Head().Number + 1000), nil, true
			case method == "eth_getTransactionReceipt" && strings.Contains(string(params[0]), fake.TxHash.String()):
				return map[string]any{
					"transactionHash": fake.TxHash.String(), "blockHash": fake.BlockHash.String(),
					"blockNumber": hexUint(fake.BlockNumber), "status": "0x1", "logs": []any{logJSON},
				}, nil, true
			}
			return nil, nil, false
		}
	})
	h.client.Refresh(t.Context()) // what Client.Monitor does every few seconds

	h.sink.set(func(s *sink) { s.headRule = nil }) // reported below as the test's own failure
	for i := 0; i < testSettle+4; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
	}
	cs := h.client.Status()
	st := h.w.Status()
	t.Logf("endpoint 0 (honest): state %s, lag %d; endpoint 1 (liar): state %s; primary %s, verifier %q; transfers reported %d; head %d",
		cs[0].State, cs[0].Lag, cs[1].State, st.Endpoint, st.Verifier, len(h.sink.liveKeys()), h.sink.lastHead().Number)
	if n := len(h.sink.liveKeys()); n != 0 {
		t.Fatalf("best effort: a payment invented by one endpoint was reported (%d transfers) although the honest endpoint was up and answering the whole time — it was merely declared lagging on the liar's eth_blockNumber", n)
	}
}

// A watcher without a checkpoint chooses its start position from the
// lower of the two endpoints' heads, however far apart they are. One endpoint
// that is far behind at that moment — a node that is still syncing, or one
// that lies — puts the cursor that far down, the position is saved, and the
// processed head is reported to the consumer from there: the watcher then has
// the whole distance to scan before it sees a new payment, also after the
// endpoint has been removed. (Here the distance is 80 blocks; on BSC it is
// tens of millions, more than public endpoints serve logs for.)
func TestStartPositionDoesNotFollowOneEndpointDown(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.add(addr(1))
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 80 })
	tip := h.chain.Head().Number

	err := h.w.Tick(context.Background())
	st := h.w.Status()
	t.Logf("tip %d; first tick: err %v; cursor %d; head reported to the sink %d; hold %q", tip, err, st.Cursor, h.sink.lastHead().Number, st.Hold)
	if st.Cursor == 0 {
		return // it did not start on such an answer: safe
	}
	if st.Cursor+testWindow < tip-testSettle {
		t.Fatalf("the start position was put at block %d, %d blocks below the tip (%d), because one of two endpoints claimed the chain ends at %d; it is in the checkpoint now",
			st.Cursor, tip-st.Cursor, tip, tip-80)
	}
}

// Unchecked ranges were forgotten when the second log endpoint came
// back in the middle of a tick. Two endpoints; the second has never answered
// a log query and refuses them (the documented exception: it counts as an
// endpoint without a log service, blocks are settled unchecked and
// remembered). The watcher falls behind and catches up in steps. During one
// catch-up tick the first step is still settled unchecked; then the pause
// after which such an endpoint is asked again ends, and the second endpoint
// answers the next step. recheck() ran with upper = the cursor before this
// tick, finished, and set UncheckedFrom to zero — although the blocks of the
// first step had been settled unchecked a moment before and lay above that
// bound. A payment the primary hid in them was never looked for again.
//
// (A single refusal used to be enough to get into the unchecked state. That
// is no longer enough — an endpoint is held to have no log service after
// three refusals over three minutes, and the head waits until then — so the
// setup goes through those; the catch-up tick and what is asserted about it
// are unchanged, and stricter: the payment has to be found.)
func TestUncheckedBlocksOfTheSameTickAreNotForgotten(t *testing.T) {
	var in incidents
	var h *harness
	var steps atomic.Int32
	var comeBack atomic.Bool
	h = newHarness(t, withNodes(2), withIncidents(&in), withConfig(func(c *watcher.Config) {
		c.Sleep = func(ctx context.Context, _ time.Duration) error {
			// Between two scan steps of the catch-up tick: the pause of the
			// "no log service" verdict runs out and the endpoint works.
			if comeBack.Load() && steps.Add(1) == 1 {
				h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError = nil })
				h.passLogPause()
			}
			return ctx.Err()
		}
	}))
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.MaxLogRange = 10 })
	// The address is 50 blocks old: the first scan asks the primary for more
	// than its 10-block limit, so later ticks scan in steps of that size.
	h.watch.addSince(addr(1), h.chain.Head().Number-50)
	h.sink.set(func(s *sink) { s.headRule = nil }) // unchecked settling passes hidden payments by design
	h.noLogService(1, &evmtest.RPCError{Code: -32005, Message: "limit exceeded"})
	for i := 0; i < testSettle+4; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
	}
	st := h.w.Status()
	if !st.LogsUnchecked || st.UncheckedFrom == 0 || st.UncheckedTo != st.Cursor {
		t.Fatalf("test setup: expected unchecked settling, status %+v", st)
	}

	// The watcher is down for a while. The primary hides a payment made in
	// the first block after the cursor.
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } })
	h.pay(addr(1), 100)
	paid := h.chain.Mine(1)
	h.chain.Mine(3 * testWindow)
	comeBack.Store(true)
	_ = h.w.Tick(context.Background())
	after := h.w.Status()
	t.Logf("payment hidden in block %d; after the catch-up tick: cursor %d, unchecked %d..%d, logs unchecked %v, log verifier %q, hold %q, last error %q",
		paid.Number, after.Cursor, after.UncheckedFrom, after.UncheckedTo, after.LogsUnchecked, after.LogVerifier, after.Hold, after.LastError)
	if after.Cursor < paid.Number || !after.LogsUnchecked || after.LogVerifier == "" {
		t.Fatalf("test setup: block %d was to be settled unchecked and a later step confirmed by the second endpoint, in one tick: %+v", paid.Number, after)
	}
	if after.UncheckedFrom == 0 && len(h.sink.liveKeys()) == 0 {
		t.Fatalf("block %d was settled on one endpoint's logs in the tick in which the second log endpoint came back, and the record of unchecked blocks was closed in that same tick without that block having been scanned by the second endpoint", paid.Number)
	}

	// The second endpoint serves logs from now on. Give the re-check time.
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = nil })
	for i := 0; i < 12; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
		h.clock.Advance(3 * time.Second)
	}
	final := h.w.Status()
	if len(h.sink.liveKeys()) != 1 {
		t.Fatalf("block %d was settled on one endpoint's logs in the tick in which the second log endpoint came back; the payment in it was never found (unchecked %d..%d, late-transfer incidents: %d)",
			paid.Number, final.UncheckedFrom, final.UncheckedTo, in.count(watcher.IncidentLateTransfer))
	}
	if in.count(watcher.IncidentLateTransfer) != 1 || final.UncheckedFrom != 0 || final.UncheckedTo != 0 || final.LogsUnchecked {
		t.Fatalf("after the re-check: late-transfer incidents %d, status %+v", in.count(watcher.IncidentLateTransfer), final)
	}
	h.sink.set(func(s *sink) { s.headRule = h.checkHeadRule })
	h.mineTicking(2)
	h.assertModel(startBlock)
}

// staleLatest makes a node answer "latest" (and "finalized") with a block
// `behind()` blocks below the real head, and everything else truthfully —
// including eth_blockNumber, which is what the pool's lag monitor reads.
func staleLatest(chain *evmtest.Chain, at func() uint64) func(b *evmtest.Behavior) {
	return func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			tag, _, numeric := blockParam(params)
			if numeric {
				return nil, nil, false
			}
			n := at()
			if tag == "finalized" && n > 2 {
				n -= 2
			}
			blk, ok := chain.BlockByNumber(n)
			if !ok {
				return nil, nil, false
			}
			return headerJSON(blk, blk.Time), nil, true
		}
	}
}

// A stuck log backend, second form: the primary's "latest" block stops a few blocks above
// the cursor while its eth_blockNumber keeps up (so the pool does not call it
// lagging). Nothing above that block is scanned, nothing is settled, and
// again the tick succeeds without a word: settle() names a hold only when the
// VERIFIER is behind the primary, not when the primary is behind the verifier.
func TestAStalePrimaryHeadDoesNotHoldTheHeadSilently(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(3), withIncidents(&in))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)

	frozen := h.chain.Head().Number
	h.nodes[0].Set(staleLatest(h.chain, func() uint64 { return frozen }))
	h.pay(addr(1), 100)
	paid := h.chain.Mine(1)
	failed := 0
	for i := 0; i < 8*testSettle; i++ {
		h.chain.Mine(1)
		h.client.Refresh(t.Context()) // Client.Monitor
		if err := h.w.Tick(context.Background()); err != nil {
			failed++
		}
		h.clock.Advance(3 * time.Second)
	}
	st := h.w.Status()
	head := h.sink.lastHead().Number
	t.Logf("paid in block %d, tip %d, processed head %d, failed ticks %d, primary %s (endpoint 0 state %s), hold %q, incidents %d, payments seen %d",
		paid.Number, h.chain.Head().Number, head, failed, st.Endpoint, h.client.Status()[0].State, st.Hold, len(in.list), len(h.sink.liveKeys()))
	if head >= paid.Number && len(h.sink.liveKeys()) == 1 {
		return
	}
	if st.Hold == "" && in.count(watcher.IncidentHeadHeld) == 0 && failed == 0 {
		t.Fatalf("one of three endpoints has held the processed head at %d for %d ticks (payment in block %d never seen) and nothing says so", head, 8*testSettle, paid.Number)
	}
	if head < paid.Number {
		t.Errorf("with three endpoints, one endpoint held the head below block %d for %d ticks (hold %q)", paid.Number, 8*testSettle, st.Hold)
	}
}

// Three endpoints. The second one — the verifier, by its place in the
// list — claims that the chain ends at a block it stopped at, permanently, and
// is truthful about everything else (its eth_blockNumber keeps up, so the pool
// does not call it lagging). "The height of the chain is the lower of
// the two endpoints' heads", so nothing is settled; the hold is named and
// alerted, but the third endpoint is never asked who is right about the
// height, the endpoint is never set aside, and the head stays held for as
// long as it likes. Invariant 4: "with three or more endpoints one endpoint
// cannot hold it".
func TestOneOfThreeCannotHoldTheHeadByClaimingALowHeight(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(3), withIncidents(&in))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)

	stuck := h.chain.Head().Number
	h.nodes[1].Set(staleLatest(h.chain, func() uint64 { return stuck }))
	h.pay(addr(1), 100)
	paid := h.chain.Mine(1)
	for i := 0; i < 12*testSettle; i++ {
		h.chain.Mine(1)
		h.client.Refresh(t.Context()) // Client.Monitor
		_ = h.w.Tick(context.Background())
		h.clock.Advance(3 * time.Second)
	}
	st := h.w.Status()
	head, tip := h.sink.lastHead().Number, h.chain.Head().Number
	t.Logf("tip %d, processed head %d (%d behind), verifier %s (endpoint 1 state %s), hold %q, head-held incidents %d",
		tip, head, tip-head, st.Verifier, h.client.Status()[1].State, st.Hold, in.count(watcher.IncidentHeadHeld))
	if tip-head > 3*testSettle {
		t.Fatalf("with three endpoints, one endpoint keeps the processed head %d blocks behind the tip (payment in block %d, head %d) by claiming a lower height; it is never outvoted or set aside (hold %q)",
			tip-head, paid.Number, head, st.Hold)
	}
}

// ---- beyond the proofs of concept ----

// frozenBlockNumber makes a node answer every eth_blockNumber — the one the
// pool's monitor asks and the one that travels with its eth_getLogs answers —
// with a fixed height.
func frozenBlockNumber(at uint64) func(b *evmtest.Behavior) {
	return func(b *evmtest.Behavior) {
		b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method == "eth_blockNumber" {
				return hexUint(at), nil, true
			}
			return nil, nil, false
		}
	}
}

// A stuck log backend with two endpoints. There is nobody to take over, so the head waits
// — but it says what for in every tick, the operator is alerted, the endpoint
// whose log index is stuck is set aside as the source of logs, and nothing is
// lost: when the endpoint recovers, the payment is there and the head moves.
func TestHeldHeadIsNamedWithTwoEndpoints(t *testing.T) {
	for name, lie := range map[string]func(h *harness) func(b *evmtest.Behavior){
		"the height that travels with its log answers is frozen": func(h *harness) func(b *evmtest.Behavior) {
			return frozenBlockNumber(h.w.Status().Cursor + 2)
		},
		"its latest block is frozen": func(h *harness) func(b *evmtest.Behavior) {
			frozen := h.chain.Head().Number
			return staleLatest(h.chain, func() uint64 { return frozen })
		},
	} {
		t.Run(name, func(t *testing.T) {
			var in incidents
			h := newHarness(t, withNodes(2), withIncidents(&in))
			h.watch.add(addr(1))
			h.mineTicking(testSettle + 2)
			h.nodes[0].Set(lie(h))
			h.pay(addr(1), 100)
			paid := h.chain.Mine(1)
			silent := 0
			for i := 0; i < 6*testSettle; i++ {
				h.chain.Mine(1)
				err := h.w.Tick(context.Background())
				if st := h.w.Status(); err == nil && st.Hold == "" && st.Cursor < paid.Number && i > 2*testSettle {
					silent++
				}
				h.clock.Advance(3 * time.Second)
			}
			st := h.w.Status()
			if silent != 0 || in.count(watcher.IncidentHeadHeld) == 0 {
				t.Fatalf("%d ticks succeeded without moving the head or saying why; head-held incidents %d; status %+v", silent, in.count(watcher.IncidentHeadHeld), st)
			}
			if st.Cursor >= paid.Number {
				t.Fatalf("with two endpoints the head passed block %d on one endpoint's view of the chain (cursor %d)", paid.Number, st.Cursor)
			}
			// Nothing is lost.
			h.nodes[0].Set(honest)
			h.passCooldowns()
			for i := 0; i < 4 && h.w.Status().Cursor != h.chain.Head().Number-testSettle; i++ {
				h.chain.Mine(1)
				_ = h.w.Tick(context.Background())
				h.passCooldowns()
			}
			h.assertModel(startBlock)
			if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle || st.Hold != "" {
				t.Fatalf("after the endpoint recovered: %+v", st)
			}
		})
	}

	t.Run("a log index that is stuck gets its endpoint set aside", func(t *testing.T) {
		var in incidents
		h := newHarness(t, withNodes(2), withIncidents(&in))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		h.nodes[0].Set(frozenBlockNumber(h.w.Status().Cursor + 2))
		for i := 1; i <= watcher.HoldAlertTicks; i++ {
			h.chain.Mine(1)
			h.tick()
			st := h.w.Status()
			if !strings.Contains(st.Hold, "the log index of "+evm.RedactURL(h.urls[0])) {
				t.Fatalf("tick %d: hold %q", i, st.Hold)
			}
			if got := in.count(watcher.IncidentLogIndexBehind); (got == 1) != (i == watcher.HoldAlertTicks) {
				t.Fatalf("tick %d: %d log-index incidents", i, got)
			}
		}
		if blamed := in.blamed(watcher.IncidentLogIndexBehind); blamed[0] != evm.RedactURL(h.urls[0]) {
			t.Fatalf("blamed %v", blamed)
		}
		if h.client.Status()[0].State != evm.StateFailing {
			t.Fatalf("the endpoint was not set aside: %s", h.client.Status()[0].State)
		}
		// The other endpoint's logs are used now; the stuck one cannot
		// vouch for them, and the hold says that instead.
		h.chain.Mine(1)
		h.tick()
		if st := h.w.Status(); st.Endpoint != evm.RedactURL(h.urls[1]) || !strings.Contains(st.Hold, "log verification unavailable") {
			t.Fatalf("after the primary was set aside: %+v", st)
		}
	})
}

// forge makes a node serve a payment that was never made, with a receipt to
// match: everything a single endpoint can do to have it believed.
func forge(h *harness, node int) {
	fake := fabricated(h, h.chain.Head().Number, 1, 1000)
	topics := make([]string, len(fake.Topics))
	for i, tp := range fake.Topics {
		topics[i] = tp.String()
	}
	logJSON := map[string]any{
		"address": fake.Address.Hex(), "topics": topics, "data": "0x" + hex.EncodeToString(fake.Data),
		"blockNumber": hexUint(fake.BlockNumber), "blockHash": fake.BlockHash.String(),
		"transactionHash": fake.TxHash.String(), "transactionIndex": "0x0", "logIndex": "0x0", "removed": false,
	}
	h.nodes[node].Set(func(b *evmtest.Behavior) {
		b.ExtraLogs = []evm.Log{fake}
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method == "eth_getTransactionReceipt" && strings.Contains(string(params[0]), fake.TxHash.String()) {
				return map[string]any{
					"transactionHash": fake.TxHash.String(), "blockHash": fake.BlockHash.String(),
					"blockNumber": hexUint(fake.BlockNumber), "status": "0x1", "logs": []any{logJSON},
				}, nil, true
			}
			return nil, nil, false
		}
	})
}

// An inflated block number in the default mode: an endpoint that adds a thousand to its block number
// changes nothing. The honest endpoints are not called lagging, the endpoint
// listed first stays the primary, and with three endpoints the head moves on
// as if nothing had happened.
func TestDefaultModeTheHonestEndpointStaysPrimary(t *testing.T) {
	for _, nodes := range []int{2, 3} {
		for liar := 1; liar < nodes; liar++ {
			t.Run(fmt.Sprintf("%d endpoints, endpoint %d lies", nodes, liar), func(t *testing.T) {
				h := newHarness(t, withNodes(nodes))
				h.watch.add(addr(1))
				h.mineTicking(testSettle + 2)
				h.nodes[liar].Set(func(b *evmtest.Behavior) {
					b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
						if method == "eth_blockNumber" {
							return hexUint(h.chain.Head().Number + 1000), nil, true
						}
						return nil, nil, false
					}
				})
				h.pay(addr(1), 7)
				for i := 0; i < testSettle+4; i++ {
					h.client.Refresh(t.Context())
					h.chain.Mine(1)
					h.tick()
					for j, es := range h.client.Status() {
						if es.State != evm.StateHealthy {
							t.Fatalf("tick %d: endpoint %d is %s (lag %d) on the word of endpoint %d", i, j, es.State, es.Lag, liar)
						}
					}
					if st := h.w.Status(); st.Endpoint != evm.RedactURL(h.urls[0]) {
						t.Fatalf("tick %d: the primary is %s, not the endpoint listed first", i, st.Endpoint)
					}
				}
				h.assertModel(startBlock)
				if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle || st.Hold != "" {
					t.Fatalf("the head did not keep up: %+v", st)
				}
			})
		}
	}
}

// An inflated block number under best effort as a whole: the watcher takes one endpoint's word in
// exactly one situation — every other endpoint has been unreachable for
// SoloPatience ticks in a row — and nothing an endpoint SAYS brings that
// situation about. Each of the answers below used to make the other endpoint
// "not healthy", and the primary its own verifier.
func TestBestEffortTakesNoAnswerForAnAbsence(t *testing.T) {
	answers := map[string]func(h *harness) func(b *evmtest.Behavior){
		"a height far below the primary's": func(*harness) func(b *evmtest.Behavior) {
			return func(b *evmtest.Behavior) { b.Lag = 300 }
		},
		"a rate limit": func(*harness) func(b *evmtest.Behavior) {
			return func(b *evmtest.Behavior) { b.RateLimited = true }
		},
		"another chain's id": func(*harness) func(b *evmtest.Behavior) {
			return func(b *evmtest.Behavior) { b.ChainID = 97 }
		},
		"blocks from the future": func(h *harness) func(b *evmtest.Behavior) { return lieAboutTime(h.chain, 3600) },
		"another hash for every block": func(h *harness) func(b *evmtest.Behavior) {
			return lieAboutHash(h.chain, h.chain.Head().Number-3*testSettle)
		},
		"no receipts": func(*harness) func(b *evmtest.Behavior) {
			return func(b *evmtest.Behavior) { b.HideReceipts = true }
		},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			var in incidents
			h := newHarness(t, withNodes(2), withIncidents(&in), withConfig(func(c *watcher.Config) { c.Verify = watcher.VerifyBestEffort }))
			h.watch.add(addr(1))
			h.mineTicking(testSettle + 2)
			h.chain.Mine(2)
			forge(h, 0)
			h.nodes[1].Set(answer(h))
			for i := 0; i < 3*watcher.SoloPatience; i++ {
				h.client.Refresh(t.Context()) // the pool forms its opinion of the endpoint
				h.chain.Mine(1)
				_ = h.w.Tick(context.Background())
				h.clock.Advance(3 * time.Second)
				if st := h.w.Status(); st.Solo || len(h.sink.liveKeys()) != 0 {
					t.Fatalf("tick %d: an endpoint that answers was passed over (solo %v), %d transfers reported on the primary's word; endpoint 1 is %s",
						i, st.Solo, len(h.sink.liveKeys()), h.client.Status()[1].State)
				}
			}
			if in.count(watcher.IncidentSingleSource) != 0 {
				t.Fatalf("incidents: %+v", in.list)
			}
		})
	}

	t.Run("no answer at all", func(t *testing.T) {
		var in incidents
		h := newHarness(t, withNodes(3), withIncidents(&in), withConfig(func(c *watcher.Config) { c.Verify = watcher.VerifyBestEffort }))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		h.chain.Mine(2)
		forge(h, 0)
		// One of the two others is down: that is not "every other".
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
		for i := 0; i < 2*watcher.SoloPatience; i++ {
			h.chain.Mine(1)
			_ = h.w.Tick(context.Background())
			if st := h.w.Status(); st.Solo || len(h.sink.liveKeys()) != 0 {
				t.Fatalf("tick %d with one of two other endpoints down: solo %v, %d transfers", i, st.Solo, len(h.sink.liveKeys()))
			}
		}
		// Both are down: after SoloPatience ticks, and not one earlier, the
		// primary is on its own — and the forgery goes through. That is the
		// price of this mode, and it is announced.
		h.sink.set(func(s *sink) { s.headRule = nil })
		h.nodes[2].Set(func(b *evmtest.Behavior) { b.Down = true })
		for i := 1; i <= watcher.SoloPatience; i++ {
			h.chain.Mine(1)
			_ = h.w.Tick(context.Background())
			st := h.w.Status()
			if want := i == watcher.SoloPatience; st.Solo != want || (len(h.sink.liveKeys()) != 0) != want {
				t.Fatalf("tick %d with every other endpoint down: solo %v, %d transfers", i, st.Solo, len(h.sink.liveKeys()))
			}
			if !st.Solo && !strings.Contains(st.Hold, "best effort") {
				t.Fatalf("tick %d: hold %q", i, st.Hold)
			}
		}
		if in.count(watcher.IncidentSingleSource) != 1 {
			t.Fatalf("incidents: %+v", in.list)
		}
	})
}

// A start with a third endpoint: the one that is far behind is outvoted, the
// watcher starts where the other two are, and says which endpoint it was.
func TestThreeEndpointsOutvoteTheOneThatIsBehind(t *testing.T) {
	for behind := 0; behind < 3; behind++ {
		t.Run(fmt.Sprintf("endpoint %d is 80 blocks behind", behind), func(t *testing.T) {
			var in incidents
			h := newHarness(t, withNodes(3), withIncidents(&in))
			h.watch.add(addr(1))
			h.nodes[behind].Set(func(b *evmtest.Behavior) { b.Lag = 80 })
			tip := h.chain.Head().Number
			for i := 0; i < 3 && h.w.Status().Cursor == 0; i++ {
				_ = h.w.Tick(context.Background())
			}
			if st := h.w.Status(); st.Cursor != tip-testSettle {
				t.Fatalf("start cursor %d, tip %d (status %+v)", st.Cursor, tip, st)
			}
			if h.sink.lastHead().Number != tip-testSettle {
				t.Fatalf("head reported to the consumer: %d", h.sink.lastHead().Number)
			}
			if blamed := in.blamed(watcher.IncidentHeightMismatch); behind < 2 && (len(blamed) == 0 || blamed[0] != evm.RedactURL(h.urls[behind])) {
				// (The endpoint listed third is never asked when the first
				// two agree.)
				t.Fatalf("height-mismatch incidents blame %v", blamed)
			}
			h.pay(addr(1), 5)
			for i := 0; i < testSettle+3; i++ {
				h.chain.Mine(1)
				_ = h.w.Tick(context.Background())
			}
			h.assertModel(startBlock)
		})
	}
}

// The same hole one door down: a checkpoint that lies above the
// chain is put back to the lower of the two heads — which must not be a
// place only one endpoint is at.
func TestACheckpointIsNotPutBackOnOneEndpointsHeight(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)
	cursor := h.w.Status().Cursor
	// Both endpoints are below what the checkpoint remembers: one by a few
	// blocks (it will be back in a moment), one by a hundred.
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.Lag = testSettle + 3 })
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 100 })
	for i := 0; i < 6; i++ {
		err := h.tickErr()
		if !strings.Contains(err.Error(), "blocks apart") {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if got := h.w.Status().Cursor; got != cursor {
		t.Fatalf("the cursor went from %d to %d on the word of the endpoint that is behind", cursor, got)
	}
	info, err := watcher.InspectCheckpoint(h.store.snapshot())
	if err != nil || info.Cursor != cursor {
		t.Fatalf("checkpoint cursor %d (%v), want %d", info.Cursor, err, cursor)
	}
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.Lag = 0 })
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 0 })
	h.mineTicking(2)
	h.assertModel(startBlock)
}

// lowCheckpoint replaces the stored checkpoint by one whose position is the
// given block and that remembers nothing else: what a watcher wrote that had
// started far below the chain (by an older version, before the start
// position was bounded).
func lowCheckpoint(t *testing.T, h *harness, cursor uint64) {
	t.Helper()
	var cp map[string]any
	if err := json.Unmarshal(h.store.snapshot(), &cp); err != nil {
		t.Fatal(err)
	}
	low, err := json.Marshal(map[string]any{"v": cp["v"], "chain_id": cp["chain_id"], "token": cp["token"], "cursor": cursor})
	if err != nil {
		t.Fatal(err)
	}
	h.store.restore(low)
}

// The damage does not outlive its cause. A checkpoint whose position
// lies far below the chain is not scanned from, block by block, with the
// consumer's head dragged down to it — unless somebody needs those blocks.
func TestAPoisonedCheckpointDoesNotHaveToBeScannedFrom(t *testing.T) {
	const low = startBlock - history + 2 // 98 blocks below the tip: more than two reorg windows
	setup := func(t *testing.T, in *incidents) *harness {
		h := newHarness(t, withNodes(2), withIncidents(in))
		h.mineTicking(testSettle + 2)
		lowCheckpoint(t, h, low)
		h.sink.take()
		return h
	}
	heads := func(h *harness) (lowest uint64) {
		lowest = ^uint64(0)
		for _, e := range h.sink.take() {
			if e.kind == "head" {
				lowest = min(lowest, e.head.Number)
			}
		}
		return lowest
	}

	t.Run("nothing is watched: it continues at the chain's height", func(t *testing.T) {
		var in incidents
		h := setup(t, &in)
		h.restart()
		h.chain.Mine(1)
		h.tick()
		tip := h.chain.Head().Number
		if st := h.w.Status(); st.Cursor != tip-testSettle {
			t.Fatalf("cursor %d, tip %d", st.Cursor, tip)
		}
		if lowest := heads(h); lowest != tip-testSettle {
			t.Fatalf("lowest head reported to the consumer: %d (tip %d)", lowest, tip)
		}
		if in.count(watcher.IncidentCheckpointBehind) != 1 {
			t.Fatalf("incidents: %+v", in.list)
		}
		// An address created now is watched from now.
		h.watch.addSince(addr(1), tip-testSettle)
		h.pay(addr(1), 3)
		h.mineTicking(testSettle + 2)
		h.assertModel(startBlock)
	})

	t.Run("the consumer has processed more: it continues a reorg window below that", func(t *testing.T) {
		var in incidents
		h := setup(t, &in)
		processed := h.sink.lastHead().Number
		// An address that has been there for long, with no start block the
		// watcher could go by — and the consumer says how far it has got.
		h.watch.add(addr(1))
		h.watch.setProcessed(processed)
		h.restart()
		h.chain.Mine(1)
		h.tick()
		if lowest := heads(h); lowest+testWindow < processed {
			t.Fatalf("a head %d blocks below the consumer's processed head (%d) was reported", processed-lowest, processed)
		}
		if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle || in.count(watcher.IncidentCheckpointBehind) != 1 {
			t.Fatalf("status %+v, incidents %+v", st, in.list)
		}
	})

	t.Run("every watched address is younger: it continues where the oldest begins", func(t *testing.T) {
		var in incidents
		h := setup(t, &in)
		h.chain.Mine(3 * testSettle)
		since := h.chain.Head().Number - 2*testSettle
		h.watch.addSince(addr(1), since)
		h.pay(addr(1), 9)
		h.restart()
		h.chain.Mine(1)
		h.tick()
		if lowest := heads(h); lowest < since {
			t.Fatalf("a head below block %d was reported: %d", since, lowest)
		}
		if in.count(watcher.IncidentCheckpointBehind) != 1 {
			t.Fatalf("incidents: %+v", in.list)
		}
		h.mineTicking(testSettle + 2)
		h.assertModel(startBlock)
	})

	t.Run("an address needs the blocks: they are scanned", func(t *testing.T) {
		var in incidents
		h := newHarness(t, withNodes(2), withIncidents(&in))
		h.watch.addSince(addr(1), low)
		h.pay(addr(1), 11)
		paid := h.chain.Mine(1).Number
		h.chain.Mine(2 * testWindow)
		h.mineTicking(testSettle + 2)
		h.assertModelFor(paid, h.watch.has)
		// The same position again, as a checkpoint: nothing may be skipped,
		// the address is as old as it.
		h.sink.set(func(s *sink) { // the consumer has lost what it had as well
			s.live, s.ever, s.events, s.head = make(map[key]watcher.Transfer), make(map[key]bool), nil, watcher.Head{}
		})
		lowCheckpoint(t, h, paid-1)
		h.restart()
		h.chain.Mine(1)
		h.tick()
		if len(h.sink.liveKeys()) != 1 || in.count(watcher.IncidentCheckpointBehind) != 0 {
			t.Fatalf("the payment in block %d was skipped: %d transfers, incidents %+v", paid, len(h.sink.liveKeys()), in.list)
		}
	})

	t.Run("a re-scan somebody asked for is not skipped, also after a restart", func(t *testing.T) {
		var in incidents
		h := newHarness(t, withNodes(2), withIncidents(&in))
		// A payment to an address the watcher learns of too late (an
		// import): its Since says "now", the payment is 90 blocks old.
		h.pay(addr(1), 13)
		paid := h.chain.Mine(1).Number
		h.chain.Mine(2 * testWindow)
		h.mineTicking(testSettle + 2)
		h.watch.addSince(addr(1), h.chain.Head().Number)
		h.watch.setProcessed(h.sink.lastHead().Number)
		h.w.Rescan(paid)
		// The process dies right after the rewind was saved.
		h.watch.mu.Lock()
		h.watch.fail = 1
		h.watch.mu.Unlock()
		h.tickErr()
		if info, err := watcher.InspectCheckpoint(h.store.snapshot()); err != nil || info.Cursor != paid-1 {
			t.Fatalf("checkpoint cursor %d (%v), want %d", info.Cursor, err, paid-1)
		}
		h.restart()
		h.chain.Mine(1)
		h.tick()
		if len(h.sink.liveKeys()) != 1 || in.count(watcher.IncidentCheckpointBehind) != 0 {
			t.Fatalf("the re-scan was dropped: %d transfers, incidents %+v, status %+v", len(h.sink.liveKeys()), in.list, h.w.Status())
		}
	})
}

// The exception to settling blocks unchecked: an endpoint is not written off as "without a
// log service" on one refusal (noLogService checks the three it takes, and
// that the head waits meanwhile). Once written off, the verdict is in the
// checkpoint: a new process does not hold the head for three minutes again,
// it asks the endpoint once and carries on. And the endpoint is asked again
// whenever its pause ends: the first answer makes it a log source for good.
func TestTheNoLogServiceVerdict(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(2), withIncidents(&in))
	h.watch.addSince(addr(1), h.chain.Head().Number)
	refusal := &evmtest.RPCError{Code: -32005, Message: "limit exceeded"}
	h.noLogService(1, refusal)
	h.mineTicking(testSettle)
	if st := h.w.Status(); !st.LogsUnchecked || st.UncheckedFrom == 0 {
		t.Fatalf("after the verdict: %+v", st)
	}

	// A new process, a new RPC client: nothing is known about the endpoints
	// but what the checkpoint says.
	began := h.clock.Now()
	h.restartWithNewClient()
	for i := 0; i < 3; i++ {
		h.chain.Mine(1)
		h.tick()
	}
	st := h.w.Status()
	if !st.LogsUnchecked || st.Cursor != h.chain.Head().Number-testSettle || !h.client.Status()[1].Logs.NoService {
		t.Fatalf("after a restart the verdict had to be reached again: %+v (endpoint 1 logs: %+v)", st, h.client.Status()[1].Logs)
	}
	if waited := h.clock.Now().Sub(began); waited != 0 {
		t.Fatalf("test setup: the clock moved by %s", waited)
	}
	if n := h.nodes[1].Calls("eth_getLogs"); n == 0 {
		t.Fatal("the endpoint was not asked for logs at all after the restart")
	}

	// The endpoint starts to serve logs. It is asked when its pause ends —
	// and from its first answer on it is a second log source: the blocks
	// that were settled without it are scanned by it, and a refusal after
	// that holds the head instead of being taken for "no log service".
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError = nil })
	h.passLogPause()
	for i := 0; i < 4; i++ {
		h.chain.Mine(1)
		h.tick()
	}
	st = h.w.Status()
	if st.LogsUnchecked || st.UncheckedFrom != 0 || st.LogVerifier != evm.RedactURL(h.urls[1]) || !h.client.Status()[1].Logs.Known {
		t.Fatalf("after the endpoint answered a log query: %+v", st)
	}
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError = refusal })
	h.restartWithNewClient()
	for i := 0; i < 2*testSettle; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
		h.clock.Advance(time.Minute)
		if st := h.w.Status(); st.LogsUnchecked || st.UncheckedFrom != 0 {
			t.Fatalf("an endpoint that has served logs was written off after a restart: %+v", st)
		}
	}
	if st := h.w.Status(); !strings.Contains(st.Hold, "log verification unavailable") || in.count(watcher.IncidentHeadHeld) == 0 {
		t.Fatalf("the head must wait for it, and say so: %+v", st)
	}
}

// Found by the model's watchdog during the soak: the first tick of a new
// process measured "did the head move?" against a cursor of zero — the
// checkpoint was not loaded yet when the question was put — so whatever held
// the head in that tick, and was not named where it held it, went unnamed
// for that one tick. Two restarts in a row made two silent ticks. Here an
// endpoint claims a chain a thousand blocks longer than anybody has, which
// only the net under the named holds catches.
func TestTheFirstTickAfterARestartIsNotSilentEither(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)
	h.nodes[0].Set(func(b *evmtest.Behavior) {
		b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method == "eth_blockNumber" {
				return hexUint(h.chain.Head().Number + 1000), nil, true
			}
			return nil, nil, false
		}
	})
	h.tick()
	if hold := h.w.Status().Hold; !strings.Contains(hold, "reports the chain at block") {
		t.Fatalf("test setup: hold %q", hold)
	}
	for i := 0; i < 3; i++ {
		h.restart()
		h.tick()
		if hold := h.w.Status().Hold; !strings.Contains(hold, "reports the chain at block") {
			t.Fatalf("restart %d: the first tick held the head at block %d without a reason (hold %q)", i+1, h.w.Status().Cursor, hold)
		}
	}
}
