package watcher_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// fabricated builds the log of a transfer that never happened, placed in a
// real block.
func fabricated(h *harness, block uint64, to int, amount int64) evm.Log {
	blk, ok := h.chain.BlockByNumber(block)
	if !ok {
		h.t.Fatalf("no block %d", block)
	}
	raw := evmtest.TransferLog(usdt, payer, addr(to), units(amount))
	return evm.Log{
		Address: raw.Address, Topics: raw.Topics, Data: raw.Data,
		BlockNumber: blk.Number, BlockHash: blk.Hash,
		TxHash: evm.Hash{0xfa, 0xce, byte(to)}, LogIndex: 0,
	}
}

// TestPartialLogs: an endpoint that silently omits logs.
func TestPartialLogs(t *testing.T) {
	t.Run("a reported transfer vanishes from a canonical block", func(t *testing.T) {
		// This is what a reorg looks like to a naive watcher. It is not
		// one: the block is still there.
		h := newHarness(t, withNodes(2))
		h.watch.add(addr(1))
		h.tick()
		tx := h.pay(addr(1), 10)
		h.chain.Mine(2)
		h.tick()
		h.sink.take()

		h.nodes[0].Set(func(b *evmtest.Behavior) {
			b.DropLogs = func(l evm.Log) bool { return l.TxHash == tx }
		})
		h.chain.Mine(1)
		err := h.tickErr()
		if !errors.Is(err, watcher.ErrInconsistentEndpoint) || !strings.Contains(err.Error(), "omitted a transfer") {
			t.Fatalf("err = %v", err)
		}
		if events := h.sink.take(); len(events) != 0 {
			t.Fatalf("facts from a faulty endpoint: %v", events)
		}
		// The endpoint is marked and the next tick uses the other one.
		st := h.client.Status()
		if st[0].Healthy || !strings.Contains(st[0].LastError, "omitted a transfer") {
			t.Fatalf("faulty endpoint status = %+v", st[0])
		}
		h.tick()
		if got := h.w.Status().Endpoint; got != evm.RedactURL(h.urls[1]) {
			t.Fatalf("still using %s", got)
		}
		if n := h.sink.count("reverted"); n != 0 {
			t.Fatal("spurious revert")
		}
		h.assertModel(startBlock)
	})

	t.Run("single endpoint: never a revert, never settled past", func(t *testing.T) {
		h := newHarness(t)
		h.watch.add(addr(1))
		h.tick()
		tx := h.pay(addr(1), 10)
		h.chain.Mine(2)
		h.tick()
		cursor := h.w.Status().Cursor
		h.nodes[0].Set(func(b *evmtest.Behavior) {
			b.DropLogs = func(l evm.Log) bool { return l.TxHash == tx }
		})
		for i := 0; i < 5; i++ {
			h.chain.Mine(3)
			h.tickErr()
			h.passCooldowns()
		}
		if n := h.sink.count("reverted"); n != 0 || len(h.sink.liveKeys()) != 1 {
			t.Fatal("spurious revert")
		}
		if got := h.w.Status().Cursor; got != cursor {
			t.Fatalf("cursor moved from %d to %d while the endpoint was inconsistent", cursor, got)
		}
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = nil })
		h.tick()
		h.assertModel(startBlock)
	})

	t.Run("log index trails the head silently", func(t *testing.T) {
		// eth_getLogs answers from a state a few blocks behind the head it
		// advertises, without saying so. Blocks are scanned again until
		// they are SettleDepth deep, so nothing is missed — it only shows
		// up later.
		h := newHarness(t)
		h.watch.add(addr(1), addr(2))
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.LogsLag = testSettle - 2 })
		h.tick()
		for i := 0; i < 30; i++ {
			if i%3 == 0 {
				h.pay(addr(1+i%2), int64(1+i))
			}
			h.chain.Mine(1)
			h.tick()
		}
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.LogsLag = 0 })
		h.tick()
		h.assertModel(startBlock)
		if n := h.sink.count("reverted"); n != 0 {
			t.Fatalf("%d spurious reverts", n)
		}
	})

	t.Run("log backend behind the head backend, visibly", func(t *testing.T) {
		// A load balancer: the head comes from a backend that is ahead of
		// the one answering log queries. The batched block number exposes
		// it; the watcher settles nothing beyond that height and does not
		// tell the sink about a head it has not scanned up to.
		h := newHarness(t)
		h.watch.add(addr(1))
		h.tick()
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.LogsBackendLag = 3 })
		h.pay(addr(1), 10)
		h.chain.Mine(1) // payment in the newest block
		h.chain.Mine(1)
		h.sink.take()
		h.tick()
		head := h.chain.Head().Number
		events := h.sink.take()
		for _, e := range events {
			if e.kind == "observed" {
				t.Fatal("transfer reported from blocks the log backend had not reached")
			}
			if e.kind == "head" && e.head.Number > head-3 {
				t.Fatalf("head %d reported, but logs were only scanned up to %d", e.head.Number, head-3)
			}
		}
		// A lag larger than the settle depth must not let the cursor run
		// past unscanned blocks either.
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.LogsBackendLag = testSettle + 10 })
		h.chain.Mine(testSettle + 12)
		h.tick()
		if st := h.w.Status(); st.Cursor > h.chain.Head().Number-(testSettle+10) {
			t.Fatalf("cursor %d is beyond what the log backend has seen (head %d, lag %d)", st.Cursor, h.chain.Head().Number, testSettle+10)
		}
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.LogsBackendLag = 0 })
		h.tick()
		h.assertModel(startBlock)
		if got := h.sink.lastHead().Number; got != h.chain.Head().Number-testSettle {
			t.Fatalf("head = %d", got)
		}
		// The backend being entirely below the range is an error, not an
		// empty answer.
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.LogsBackendLag = 500 })
		h.chain.Mine(2)
		if err := h.tickErr(); !evm.IsKind(err, evm.KindLagging) {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestFabricatedTransfers: an endpoint that invents a payment.
func TestFabricatedTransfers(t *testing.T) {
	t.Run("invented log, no such transaction", func(t *testing.T) {
		for _, nodes := range []int{1, 2} {
			h := newHarness(t, withNodes(nodes))
			h.watch.add(addr(1), addr(2))
			h.tick()
			h.chain.Mine(3)
			fake := fabricated(h, h.chain.Head().Number-1, 1, 1000)
			h.nodes[0].Set(func(b *evmtest.Behavior) { b.ExtraLogs = []evm.Log{fake} })
			h.pay(addr(2), 5) // a real payment later on
			h.chain.Mine(testSettle + 4)
			h.sink.take()
			h.tick()
			// The real payment is reported, the invented one is not — and
			// the cursor waits below the disputed block.
			h.assertModel(startBlock)
			if n := len(h.sink.liveKeys()); n != 1 {
				t.Fatalf("%d endpoints: %d transfers reported, want only the real one", nodes, n)
			}
			st := h.w.Status()
			if st.Unverified != 1 || st.Cursor >= fake.BlockNumber {
				t.Fatalf("%d endpoints: status = %+v (fake in block %d)", nodes, st, fake.BlockNumber)
			}
			if !strings.Contains(h.logs.String(), "receipt does not confirm it") {
				t.Error("the disagreement was not logged")
			}
			// When the endpoint stops lying, everything settles.
			h.nodes[0].Set(func(b *evmtest.Behavior) { b.ExtraLogs = nil })
			h.mineTicking(testSettle + 2)
			if st := h.w.Status(); st.Unverified != 0 || st.Cursor != h.chain.Head().Number-testSettle {
				t.Fatalf("%d endpoints: status after recovery = %+v", nodes, st)
			}
		}
	})

	t.Run("receipt disagrees with the log", func(t *testing.T) {
		// A real transaction, but the log claims a different amount, a
		// different recipient, or a different block.
		h := newHarness(t)
		h.watch.add(addr(1), addr(2))
		h.tick()
		tx := h.pay(addr(1), 5)
		blk := h.chain.Mine(1)
		real := evmtest.TransferLog(usdt, payer, addr(1), units(5))
		inflated := evmtest.TransferLog(usdt, payer, addr(1), units(5000))
		redirected := evmtest.TransferLog(usdt, payer, addr(2), units(5))
		prev, _ := h.chain.BlockByNumber(blk.Number - 1)
		h.nodes[0].Set(func(b *evmtest.Behavior) {
			b.DropLogs = func(l evm.Log) bool { return l.TxHash == tx } // hide the truth...
			b.ExtraLogs = []evm.Log{                                    // ...and serve variations of it
				{Address: usdt, Topics: inflated.Topics, Data: inflated.Data, BlockNumber: blk.Number, BlockHash: blk.Hash, TxHash: tx, LogIndex: 0},
				{Address: usdt, Topics: redirected.Topics, Data: redirected.Data, BlockNumber: blk.Number, BlockHash: blk.Hash, TxHash: tx, LogIndex: 1},
				{Address: usdt, Topics: real.Topics, Data: real.Data, BlockNumber: blk.Number, BlockHash: blk.Hash, TxHash: tx, LogIndex: 4},
				{Address: usdt, Topics: real.Topics, Data: real.Data, BlockNumber: prev.Number, BlockHash: prev.Hash, TxHash: tx, LogIndex: 0},
			}
		})
		h.sink.take()
		h.tick()
		if n := len(h.sink.liveKeys()); n != 0 {
			t.Fatalf("%d forged variants were reported", n)
		}
		if st := h.w.Status(); st.Unverified != 4 {
			t.Fatalf("status = %+v", st)
		}
		// Two different logs at the same position of the same block are
		// not even looked at: the endpoint is rejected.
		h.nodes[0].Set(func(b *evmtest.Behavior) {
			b.ExtraLogs = append(b.ExtraLogs, evm.Log{Address: usdt, Topics: redirected.Topics, Data: redirected.Data,
				BlockNumber: blk.Number, BlockHash: blk.Hash, TxHash: tx, LogIndex: 0})
		})
		if err := h.tickErr(); !evm.IsKind(err, evm.KindBadResponse) {
			t.Fatalf("err = %v", err)
		}
		// A transaction that reverted on chain does not count either.
		h2 := newHarness(t)
		h2.watch.add(addr(1))
		h2.tick()
		failed := h2.chain.SubmitFailed()
		blk2 := h2.chain.Mine(1)
		h2.nodes[0].Set(func(b *evmtest.Behavior) {
			b.ExtraLogs = []evm.Log{{Address: usdt, Topics: real.Topics, Data: real.Data, BlockNumber: blk2.Number, BlockHash: blk2.Hash, TxHash: failed, LogIndex: 0}}
		})
		h2.tick()
		if n := len(h2.sink.liveKeys()); n != 0 {
			t.Fatal("a transfer of a reverted transaction was reported")
		}
	})

	t.Run("a lying primary needs an accomplice", func(t *testing.T) {
		// The primary invents both the log and a matching receipt. With a
		// second, honest endpoint configured the payment is never
		// reported; with the primary alone it is — that is the documented
		// limit of a single-endpoint setup.
		lie := func(h *harness) evm.Log {
			h.chain.Mine(3)
			fake := fabricated(h, h.chain.Head().Number, 1, 1000)
			topics := make([]string, len(fake.Topics))
			for i, tp := range fake.Topics {
				topics[i] = tp.String()
			}
			logJSON := map[string]any{
				"address": fake.Address.Hex(), "topics": topics, "data": "0x" + hex.EncodeToString(fake.Data),
				"blockNumber": "0x" + strconvHex(fake.BlockNumber), "blockHash": fake.BlockHash.String(),
				"transactionHash": fake.TxHash.String(), "transactionIndex": "0x0", "logIndex": "0x0", "removed": false,
			}
			h.nodes[0].Set(func(b *evmtest.Behavior) {
				b.ExtraLogs = []evm.Log{fake}
				b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
					if method != "eth_getTransactionReceipt" || !strings.Contains(string(params[0]), fake.TxHash.String()) {
						return nil, nil, false
					}
					return map[string]any{
						"transactionHash": fake.TxHash.String(), "blockHash": fake.BlockHash.String(),
						"blockNumber": "0x" + strconvHex(fake.BlockNumber), "status": "0x1", "logs": []any{logJSON},
					}, nil, true
				}
			})
			return fake
		}

		alone := newHarness(t)
		alone.watch.add(addr(1))
		alone.tick()
		lie(alone)
		alone.tick()
		if n := len(alone.sink.liveKeys()); n != 1 {
			t.Fatalf("single endpoint: expected the (documented) acceptance of the lie, got %d transfers", n)
		}

		for _, mode := range []watcher.VerifyMode{watcher.VerifyAuto, watcher.VerifyStrict} {
			guarded := newHarness(t, withNodes(2), withConfig(func(c *watcher.Config) { c.Verify = mode }))
			guarded.watch.add(addr(1))
			guarded.tick()
			lie(guarded)
			for i := 0; i < 5; i++ {
				guarded.chain.Mine(2)
				guarded.tick()
			}
			if n := len(guarded.sink.liveKeys()); n != 0 {
				t.Fatalf("mode %d: a payment vouched for by one endpoint only was reported", mode)
			}
			// Not even while the honest endpoint is unreachable.
			guarded.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
			for i := 0; i < 5; i++ {
				guarded.chain.Mine(2)
				guarded.tick()
				guarded.passCooldowns()
			}
			if n := len(guarded.sink.liveKeys()); n != 0 {
				t.Fatalf("mode %d: the lie was accepted while the second endpoint was down", mode)
			}
			if st := guarded.w.Status(); st.Unverified != 1 {
				t.Fatalf("mode %d: status = %+v", mode, st)
			}
		}

		// VerifyBestEffort trades exactly that away.
		be := newHarness(t, withNodes(2), withConfig(func(c *watcher.Config) { c.Verify = watcher.VerifyBestEffort }))
		be.watch.add(addr(1))
		be.tick()
		lie(be)
		be.chain.Mine(1)
		be.tick()
		if n := len(be.sink.liveKeys()); n != 0 {
			t.Fatal("best effort: the lie was accepted although the honest endpoint was up")
		}
		// Not for being called lagging, rate-limited or faulty either: an
		// endpoint that answers is never passed over ...
		be.nodes[1].Set(func(b *evmtest.Behavior) { b.RateLimited = true })
		for i := 0; i < 2*watcher.SoloPatience; i++ {
			be.client.Refresh(t.Context())
			be.chain.Mine(1)
			be.tick()
			if n, st := len(be.sink.liveKeys()), be.w.Status(); n != 0 || st.Solo || st.Hold == "" {
				t.Fatalf("best effort: the lie was accepted while the honest endpoint was throttling (%d transfers, status %+v)", n, st)
			}
		}
		// ... only when it has not answered at all for a number of ticks in
		// a row.
		be.nodes[1].Set(func(b *evmtest.Behavior) { b.RateLimited = false; b.Down = true })
		for i := 1; i < watcher.SoloPatience; i++ {
			be.client.Refresh(t.Context())
			be.chain.Mine(1)
			be.tick()
			if n, st := len(be.sink.liveKeys()), be.w.Status(); n != 0 || st.Solo || st.Hold == "" {
				t.Fatalf("best effort: the lie was accepted after %d tick(s) without the other endpoint (%d transfers, status %+v)", i, n, st)
			}
		}
		// From here on a head can be reported over a transfer that is not
		// on the chain: that is what this mode gives up, and the harness's
		// head rule would say so.
		be.sink.set(func(s *sink) { s.headRule = nil })
		be.chain.Mine(1)
		be.tick()
		if n := len(be.sink.liveKeys()); n != 1 || !be.w.Status().Solo {
			t.Fatalf("best effort: expected the fallback to the primary's own word once every other endpoint has been unreachable for %d ticks (status %+v)",
				watcher.SoloPatience, be.w.Status())
		}
	})
}

func strconvHex(v uint64) string {
	const digits = "0123456789abcdef"
	if v == 0 {
		return "0"
	}
	var out []byte
	for ; v > 0; v >>= 4 {
		out = append([]byte{digits[v&0xf]}, out...)
	}
	return string(out)
}

// TestVerificationAvailability: what the cross-check costs when endpoints go
// away.
func TestVerificationAvailability(t *testing.T) {
	t.Run("auto, two endpoints: observations wait for the second one", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		h.watch.add(addr(1))
		h.tick()
		before := h.w.Status().Cursor
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
		h.pay(addr(1), 10)
		paid := h.chain.Mine(1).Number
		h.chain.Mine(testSettle + 5)
		h.sink.take()
		h.tick() // not an error: the chain was scanned
		st := h.w.Status()
		if len(h.sink.liveKeys()) != 0 || st.Unverified != 1 || st.Cursor != before || st.Hold == "" || st.Verifier != "" {
			t.Fatalf("status = %+v (payment in %d)", st, paid)
		}
		// Without a second endpoint nothing is settled at all — neither the
		// transfer nor any block: the processed head stays where the two
		// endpoints last agreed. A consumer that expires payments by the
		// head's time cannot be led to expire this one while its payment
		// waits for a second opinion, and no head comes from one endpoint.
		if events := h.sink.take(); len(events) != 0 {
			t.Fatalf("facts reported on one endpoint's word: %v", kinds(events))
		}
		for i := 0; i < 5; i++ {
			h.chain.Mine(3)
			h.tick()
			h.passCooldowns()
		}
		if events := h.sink.take(); len(events) != 0 {
			t.Fatalf("facts reported on one endpoint's word: %v", kinds(events))
		}
		if st := h.w.Status(); st.Unverified != 1 || st.Behind < 15 || st.Cursor != before {
			t.Fatalf("status = %+v", st)
		}
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
		h.passCooldowns()
		h.tick()
		h.assertModel(startBlock)
		h.mineTicking(testSettle + 1)
		if st := h.w.Status(); st.Unverified != 0 || st.Cursor < paid {
			t.Fatalf("status = %+v", st)
		}
		// The receipt came from the other endpoint.
		if h.nodes[1].Calls("eth_getTransactionReceipt") != 1 || h.nodes[0].Calls("eth_getTransactionReceipt") != 0 {
			t.Fatalf("receipt lookups: primary %d, verifier %d", h.nodes[0].Calls("eth_getTransactionReceipt"), h.nodes[1].Calls("eth_getTransactionReceipt"))
		}
	})

	t.Run("the verifier lags: observation is delayed, not lost", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		h.watch.add(addr(1))
		h.tick()
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 4 })
		h.pay(addr(1), 10)
		h.chain.Mine(1)
		h.tick()
		if len(h.sink.liveKeys()) != 0 {
			t.Fatal("reported before the verifier had the block")
		}
		h.chain.Mine(4)
		h.tick()
		h.assertModel(startBlock)
		if len(h.sink.liveKeys()) != 1 {
			t.Fatal("not reported after the verifier caught up")
		}
	})

	t.Run("best effort, two endpoints: falls back to the primary", func(t *testing.T) {
		var in incidents
		h := newHarness(t, withNodes(2), withIncidents(&in), withConfig(func(c *watcher.Config) { c.Verify = watcher.VerifyBestEffort }))
		h.watch.add(addr(1))
		h.tick()
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
		h.client.Refresh(t.Context())
		h.pay(addr(1), 10)
		// Not at once: the other endpoint has to have been unreachable for
		// a number of ticks in a row. Until then this mode waits like the
		// default one, and says what for.
		for i := 1; i < watcher.SoloPatience; i++ {
			h.chain.Mine(1)
			h.tick()
			if st := h.w.Status(); len(h.sink.liveKeys()) != 0 || st.Solo || !strings.Contains(st.Hold, "best effort") {
				t.Fatalf("tick %d without the second endpoint: %d transfers, status %+v", i, len(h.sink.liveKeys()), st)
			}
		}
		if in.count(watcher.IncidentSingleSource) != 0 {
			t.Fatalf("incidents = %+v", in.list)
		}
		h.chain.Mine(1)
		h.tick()
		h.assertModel(startBlock)
		if st := h.w.Status(); len(h.sink.liveKeys()) != 1 || !st.Solo || st.Verifier != "" {
			t.Fatalf("not reported after %d ticks: status %+v", watcher.SoloPatience, st)
		}
		if in.count(watcher.IncidentSingleSource) != 1 || in.blamed(watcher.IncidentSingleSource)[0] != evm.RedactURL(h.urls[0]) {
			t.Fatalf("the beginning of single-source operation was not reported: %+v", in.list)
		}
		// It lasts until the other endpoint answers again, and ends at once
		// — with an incident of its own, also seconds after the first.
		h.mineTicking(testSettle + 2)
		if st := h.w.Status(); !st.Solo || !st.LogsUnchecked || st.UncheckedFrom == 0 {
			t.Fatalf("single-source operation: status %+v", st)
		}
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
		h.passCooldowns()
		h.chain.Mine(1)
		h.tick()
		if st := h.w.Status(); st.Solo || st.Verifier != evm.RedactURL(h.urls[1]) {
			t.Fatalf("after the second endpoint came back: status %+v", st)
		}
		if in.count(watcher.IncidentSingleSourceEnded) != 1 || in.count(watcher.IncidentSingleSource) != 1 {
			t.Fatalf("incidents = %+v", in.list)
		}
		// A new outage is counted from the beginning.
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
		h.chain.Mine(1)
		h.tick()
		if st := h.w.Status(); st.Solo || st.Hold == "" {
			t.Fatalf("one tick into a new outage: status %+v", st)
		}
	})

	t.Run("a revert also needs the second endpoint", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		h.watch.add(addr(1))
		h.tick()
		h.pay(addr(1), 10)
		h.chain.Mine(1)
		h.tick()
		// The primary alone moves to a fork without the payment (the
		// second endpoint still serves the old chain).
		old := h.chain.Clone()
		h.nodes[1].SetChain(old)
		h.chain.Reorg(1)
		h.chain.Mine(2)
		old.Mine(1)
		h.sink.take()
		h.sink.set(func(s *sink) { s.headRule = nil }) // the harness's "truth" is the primary's fork here
		h.tick()
		if n := h.sink.count("reverted"); n != 0 || len(h.sink.liveKeys()) != 1 {
			t.Fatal("revert reported on one endpoint's word")
		}
		// And while the endpoints disagree about that block, the processed
		// head does not move past it: the consumer must not come to regard
		// a transfer as final that one endpoint says is gone.
		disputed := h.sink.liveKeys()
		for _, tr := range disputed {
			for i := 0; i < 4; i++ {
				h.chain.Mine(3)
				old.Mine(3)
				h.tick()
			}
			if got := h.sink.lastHead().Number; got >= tr.BlockNumber {
				t.Fatalf("processed head %d reached the disputed block %d", got, tr.BlockNumber)
			}
		}
		if !strings.Contains(h.logs.String(), "endpoints disagree about a block") {
			t.Error("disagreement not logged")
		}
		// Once the second endpoint follows, the revert goes out.
		h.nodes[1].SetChain(h.chain)
		h.sink.set(func(s *sink) { s.headRule = h.checkHeadRule })
		h.chain.Mine(1)
		h.sink.take()
		h.tick()
		if got := kinds(h.sink.take()); !reflect.DeepEqual(got, []string{"reverted", "head"}) {
			t.Fatalf("events = %v", got)
		}
		h.assertModel(startBlock)
	})
}

// TestEndpointFlipsBetweenForks: the pool fails over to an endpoint stuck on
// a stale fork and back.
func TestEndpointFlipsBetweenForks(t *testing.T) {
	h := newHarness(t, withNodes(3), withConfig(func(c *watcher.Config) { c.Verify = watcher.VerifyBestEffort }))
	h.watch.add(addr(1), addr(2))
	h.tick()
	h.pay(addr(1), 10)
	h.chain.Mine(3)
	h.tick()

	// Endpoint 2 got stuck some blocks ago on a fork of its own, on which
	// the payment never happened and another one did.
	stale := h.chain.Clone()
	stale.Reorg(3)
	stale.Transfer(usdt, payer, addr(2), units(77))
	stale.Mine(9)
	h.nodes[1].SetChain(stale)
	h.chain.Mine(5)
	h.tick()
	h.sink.take()

	// The primary and the third endpoint go down: only the stale endpoint
	// is left.
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
	h.nodes[2].Set(func(b *evmtest.Behavior) { b.Down = true })
	surfaced := false
	for i := 0; i < 8; i++ {
		h.chain.Mine(2)
		stale.Mine(2)
		err := h.w.Tick(t.Context())
		if err == nil {
			t.Fatalf("tick %d succeeded on a stale fork", i)
		}
		surfaced = surfaced || strings.Contains(err.Error(), "contradicts")
	}
	if h.nodes[1].Calls("eth_getBlockByNumber") == 0 {
		t.Fatal("test setup: the stale endpoint was never consulted")
	}
	if !surfaced {
		t.Errorf("the fork conflict was not surfaced; last error: %s", h.w.Status().LastError)
	}
	if events := h.sink.take(); len(events) != 0 {
		t.Fatalf("facts taken from the stale fork: %v", events)
	}
	if len(h.sink.liveKeys()) != 1 {
		t.Fatal("the real payment was reverted on the word of a stale endpoint")
	}

	// The good endpoints return.
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = false })
	h.nodes[2].Set(func(b *evmtest.Behavior) { b.Down = false })
	h.passCooldowns()
	h.tick()
	h.assertModel(startBlock)
	if st := h.w.Status(); st.DeepReorgs != 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestLaggingAndWrongChainEndpoints(t *testing.T) {
	t.Run("endpoint behind the cursor", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		h.watch.add(addr(1))
		h.tick()
		h.pay(addr(1), 10)
		h.chain.Mine(testSettle * 4)
		h.tick()
		h.sink.take()
		cursor := h.w.Status().Cursor

		// The primary dies; the fallback is far behind (it has not even
		// reached blocks that are already settled).
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = uint64(testSettle * 3) })
		h.tickErr() // primary fails
		err := h.tickErr()
		if !strings.Contains(err.Error(), "behind") {
			t.Fatalf("err = %v", err)
		}
		if events := h.sink.take(); len(events) != 0 {
			t.Fatalf("facts from a lagging endpoint: %v", events)
		}
		if got := h.w.Status().Cursor; got != cursor {
			t.Fatalf("cursor went from %d to %d", cursor, got)
		}
		// A fallback that lags only a little is used; the head it reports
		// is lower than the one already reported, so no head goes out.
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 2 })
		h.passCooldowns()
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
		if err := h.w.Tick(t.Context()); err != nil {
			// The primary may be picked first once its cooldown is over.
			h.tick()
		}
		for _, e := range h.sink.take() {
			if e.kind == "head" {
				t.Fatalf("head %d reported from a lagging endpoint (already at %d)", e.head.Number, h.chain.Head().Number)
			}
			if e.kind == "reverted" {
				t.Fatal("revert from a lagging endpoint")
			}
		}
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = false })
		h.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 0 })
		h.passCooldowns()
		h.chain.Mine(1)
		h.tick()
		h.assertModel(startBlock)
	})

	t.Run("wrong chain", func(t *testing.T) {
		// The primary is a testnet node whose chain happens to contain a
		// "payment" to the deposit address.
		h := newHarness(t, withNodes(3))
		testnet := evmtest.NewChain(97, startBlock)
		testnet.AddToken(usdt, "USDT", 18)
		testnet.Transfer(usdt, payer, addr(1), units(1_000_000))
		testnet.Mine(5)
		h.nodes[0].SetChain(testnet)
		h.watch.add(addr(1))
		for i := 0; i < 5; i++ {
			h.chain.Mine(2)
			testnet.Mine(2)
			h.tick()
		}
		if n := len(h.sink.liveKeys()); n != 0 {
			t.Fatal("a testnet transfer was reported")
		}
		if n := h.nodes[0].Calls("eth_getLogs") + h.nodes[0].Calls("eth_getBlockByNumber"); n != 0 {
			t.Fatalf("the wrong-chain endpoint was asked for chain data %d times", n)
		}
		if st := h.w.Status(); st.Endpoint != evm.RedactURL(h.urls[1]) {
			t.Fatalf("status = %+v", st)
		}
		// With a single endpoint left on the right chain, nothing is
		// settled: there is nobody to confirm what it says.
		h.sink.take()
		h.nodes[2].SetChain(testnet)
		h.passCooldowns()
		h.client.Refresh(t.Context())
		h.chain.Mine(testSettle)
		testnet.Mine(testSettle)
		h.tick()
		if events := h.sink.take(); len(events) != 0 || h.w.Status().Hold == "" {
			t.Fatalf("facts on the word of the only endpoint on the right chain: %v (status %+v)", kinds(events), h.w.Status())
		}
		// With nothing but wrong-chain endpoints the watcher stands still.
		h.nodes[1].SetChain(testnet)
		h.passCooldowns()
		h.client.Refresh(t.Context())
		if err := h.tickErr(); !errors.Is(err, evm.ErrNoEndpoint) {
			t.Fatalf("err = %v", err)
		}
	})
}
