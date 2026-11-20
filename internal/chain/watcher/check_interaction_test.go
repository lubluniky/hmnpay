package watcher_test

// Regression tests for holes in the interaction of the watcher's checks: a
// primary that keeps serving the logs of an orphaned block, one of three
// endpoints that stalls the head, a log verifier that is throttled, and a
// deep reorganisation the primary keeps quiet. Each test first describes the
// attack as it worked against an earlier version, then asserts the safe
// behaviour.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// staleLog rebuilds the eth_getLogs entry of a transfer the watcher reported,
// exactly as the endpoint returned it at the time (same block hash).
func staleLog(tr watcher.Transfer) evm.Log {
	raw := evmtest.TransferLog(tr.Token, tr.From, tr.To, tr.Amount)
	return evm.Log{
		Address: raw.Address, Topics: raw.Topics, Data: raw.Data,
		BlockNumber: tr.BlockNumber, BlockHash: tr.BlockHash,
		TxHash: tr.TxHash, LogIndex: tr.LogIndex,
	}
}

// Two endpoints, both serve logs. A payment is mined at the tip and
// reported (primary's log + the other endpoint's receipt). Then an ordinary
// one-block reorganisation drops the transaction and it is not mined again
// (the payer double-spent it). An honest primary would stop showing the log
// and the watcher would report the revert. A lying primary keeps returning
// the log of the orphaned block from eth_getLogs while answering every header
// request truthfully.
//
// The second log scan (confirmRange) notices that the honest endpoint's logs
// lack the transfer — and blames the honest endpoint ("its logs lack a
// transfer ... that other endpoints confirm"). After ConflictPatience ticks
// the watcher gives up on the honest endpoint and settles the range on the
// liar's logs alone: the processed head passes the block while the consumer
// still holds a transfer that is not on the chain. The core counts
// confirmations from that head, so the payment is credited.
//
// The harness's head rule ("HEAD RULE: head N reported while the sink still
// holds transfer ... which is not on the chain") fails the test by itself; the
// explicit assertion below says the same in the test's own words.
func TestStaleLogsDoNotKeepAnOrphanedTransferAlive(t *testing.T) {
	for _, nodes := range []int{2, 3} {
		var in incidents
		h := newHarness(t, withNodes(nodes), withIncidents(&in), withConfig(func(c *watcher.Config) { c.ConflictPatience = 4 }))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)

		h.pay(addr(1), 100)
		paid := h.chain.Mine(1)
		h.tick()
		live := h.sink.liveKeys()
		if len(live) != 1 {
			t.Fatalf("%d endpoints: %d transfers observed, want 1", nodes, len(live))
		}
		var reported watcher.Transfer
		for _, tr := range live {
			reported = tr
		}

		// The block is reorganised away; the transaction is not re-mined.
		h.chain.Reorg(1)
		h.chain.Mine(1)
		if blk, _ := h.chain.BlockByNumber(paid.Number); blk.Hash == paid.Hash {
			t.Fatal("test setup: the reorg did not replace the block")
		}
		// The primary keeps serving the orphaned block's log.
		stale := staleLog(reported)
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.ExtraLogs = []evm.Log{stale} })

		for i := 0; i < 3*testSettle+8; i++ {
			h.chain.Mine(1)
			_ = h.w.Tick(context.Background())
		}

		head := h.sink.lastHead().Number
		_, stillLive := h.sink.liveKeys()[keyOf(reported)]
		t.Logf("%d endpoints: orphaned block %d, processed head %d, orphaned transfer still live in the consumer: %v, endpoints blamed for log omission: %v",
			nodes, paid.Number, head, stillLive, in.blamed(watcher.IncidentLogOmission))
		if stillLive && head >= paid.Number {
			t.Errorf("%d endpoints: the processed head (%d) passed block %d while the consumer still counts a transfer from the orphaned block %x — one endpoint's stale logs outlived a reorganisation",
				nodes, head, paid.Number, paid.Hash[:4])
		}
		for _, blamed := range in.blamed(watcher.IncidentLogOmission) {
			if blamed != evm.RedactURL(h.urls[0]) {
				t.Errorf("%d endpoints: an honest endpoint (%s) was blamed for a log omission; the liar is %s", nodes, blamed, evm.RedactURL(h.urls[0]))
				break
			}
		}
		// Further: the transfer is taken back (not
		// merely held), the endpoint whose logs outlived the block is the
		// one blamed, no honest endpoint is set aside, and the head keeps
		// up — the stale log, which the liar goes on serving, is thrown out
		// on sight because its block is not the one the endpoints agree on.
		if stillLive {
			t.Errorf("%d endpoints: the orphaned transfer was never reported as reverted", nodes)
		}
		if blamed := in.blamed(watcher.IncidentStaleLog); len(blamed) == 0 || blamed[0] != evm.RedactURL(h.urls[0]) {
			t.Errorf("%d endpoints: stale_log incidents blame %q, want the liar", nodes, blamed)
		}
		for i, es := range h.client.Status() {
			if i > 0 && !es.Healthy {
				t.Errorf("%d endpoints: honest endpoint %d was set aside: %s", nodes, i, es.LastError)
			}
		}
		if want := h.chain.Head().Number - testSettle; head != want {
			t.Errorf("%d endpoints: processed head %d, want %d", nodes, head, want)
		}
		h.assertModel(startBlock)
	}
}

// Three endpoints, one faulty: README says "with 3+ the faulty one is
// outvoted". A reported (still pending) transfer exists in block B. The
// primary then (a) leaves that transfer out of its logs and (b) answers null
// for the header of block B when it is asked about it right after a log scan
// (that is the question proveReverted asks; in the head batch at the start of
// a tick and in the pre-scan header agreement it answers truthfully, so the
// anchor and finality checks have nothing to object to). proveReverted treats
// "the primary does not have that block" as "no proof of anything": the head
// is held below B, the primary is neither faulted nor outvoted, no incident
// is raised, and because it stays healthy it is the primary again on the next
// tick. The processed head never passes B: nothing confirms and nothing
// expires for as long as the endpoint keeps it up. (Answering with another
// hash for B instead of null takes the same path: "endpoints disagree about a
// block; revert not reported" is a log line, not a fault.)
func TestOneOfThreeEndpointsCannotStallTheHeadWithoutBeingSetAside(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(3), withIncidents(&in))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)

	txHash := h.pay(addr(1), 100)
	paid := h.chain.Mine(1)
	h.tick()
	if n := len(h.sink.liveKeys()); n != 1 {
		t.Fatalf("%d transfers observed, want 1", n)
	}

	// From now on endpoint 0 hides that transfer, and does not know block B
	// when asked after a log scan.
	var mu sync.Mutex
	afterLogs := false
	h.nodes[0].Set(func(b *evmtest.Behavior) {
		b.DropLogs = func(l evm.Log) bool { return l.TxHash == txHash }
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			mu.Lock()
			defer mu.Unlock()
			if method == "eth_getLogs" {
				afterLogs = true
				return nil, nil, false
			}
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			tag, n, numeric := blockParam(params)
			if tag == "latest" {
				afterLogs = false // a new tick begins
			}
			if numeric && n == paid.Number && afterLogs {
				return nil, nil, true // null: "no such block"
			}
			return nil, nil, false
		}
	})

	// Generous: 6 settle depths of blocks, one tick each. The clock is not
	// advanced, so an endpoint that is set aside once stays aside.
	for i := 0; i < 6*testSettle; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
	}
	head := h.sink.lastHead().Number
	st := h.w.Status()
	t.Logf("endpoint 0 last error: %q", h.client.Status()[0].LastError)
	t.Logf("block with the pending transfer: %d; tip: %d; processed head after %d ticks: %d; primary: %s; hold: %q; incidents: %d; endpoint 0 healthy: %v",
		paid.Number, h.chain.Head().Number, 6*testSettle, head, st.Endpoint, st.Hold, len(in.list), h.client.Status()[0].Healthy)
	if head < paid.Number {
		t.Errorf("with three endpoints, one faulty endpoint held the processed head at %d (below block %d) for %d ticks and was never set aside (still primary: %v, incidents raised: %d)",
			head, paid.Number, 6*testSettle, st.Endpoint == evm.RedactURL(h.urls[0]), len(in.list))
	}
}

// Two endpoints that both serve eth_getLogs (README table, last row:
// hiding a payment is "delayed by a few seconds, alerted"; and "two endpoints
// fail closed"). The honest second endpoint throttles eth_getLogs for a
// moment with the standard JSON-RPC answer {-32005 "limit exceeded"}. The
// scanner cannot tell that from "does not serve logs", marks eth_getLogs
// unavailable on that endpoint for a minute (doubling up to 30), and
// confirmRange then finds no log verifier at all — and settles the blocks on
// the primary's scan alone (Status.LogsUnchecked) instead of waiting. A
// primary that hides a payment during that window hides it for good: the
// blocks are settled and never scanned again, and the invoice expires.
func TestAThrottledLogVerifierIsNotDroppedSilently(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)
	if st := h.w.Status(); st.LogVerifier != evm.RedactURL(h.urls[1]) || st.LogsUnchecked {
		t.Fatalf("test setup: the second endpoint should be confirming log scans: %+v", st)
	}

	// The honest endpoint starts throttling eth_getLogs; the primary starts
	// hiding logs. A payment is mined.
	h.nodes[1].Set(func(b *evmtest.Behavior) {
		b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"}
	})
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } })
	h.pay(addr(1), 100)
	paid := h.chain.Mine(1)

	// The harness's head rule reports any head at or above the paid block
	// that is announced without the transfer; collect instead of failing
	// inside the callback so that the message below is the one shown.
	violated := uint64(0)
	h.sink.set(func(s *sink) {
		s.headRule = func(head watcher.Head, live map[key]watcher.Transfer) {
			if head.Number >= paid.Number && len(live) == 0 && violated == 0 {
				violated = head.Number
			}
		}
	})
	for i := 0; i < testSettle+4; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
	}
	// The throttling ends; everything is honest again. Too late.
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError = nil })
	h.nodes[0].Set(honest)
	h.passCooldowns()
	for i := 0; i < testSettle+4; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
	}

	st := h.w.Status()
	t.Logf("payment in block %d; processed head %d; transfers delivered: %d; first head announced without it: %d; LogsUnchecked=%v",
		paid.Number, h.sink.lastHead().Number, len(h.sink.liveKeys()), violated, st.LogsUnchecked)
	if violated != 0 {
		t.Errorf("head %d was reported although the payment in block %d was never delivered: while the second log endpoint was throttled, blocks were settled on the primary's logs alone", violated, paid.Number)
	}
	if len(h.sink.liveKeys()) != 1 {
		t.Errorf("the payment in block %d was never reported (%d transfers delivered), even after both endpoints were honest again", paid.Number, len(h.sink.liveKeys()))
	}
}

// Stale logs, where only the lying endpoint serves logs. Nobody can repeat its
// log scan, but the transfers it reports are attested again, by receipt from
// the other endpoint, when their block is settled — and a transfer whose
// block is gone is taken back.
func TestStaleLogsWhereOnlyOneEndpointServesLogs(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(2), withIncidents(&in))
	h.nodes[1].Set(func(b *evmtest.Behavior) {
		b.LogsError = &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"}
	})
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)
	h.pay(addr(1), 100)
	paid := h.chain.Mine(1)
	h.tick()
	var reported watcher.Transfer
	for _, tr := range h.sink.liveKeys() {
		reported = tr
	}
	if reported.BlockHash != paid.Hash {
		t.Fatalf("test setup: transfer not reported")
	}
	h.chain.Reorg(1)
	h.chain.Mine(1)
	stale := staleLog(reported)
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.ExtraLogs = []evm.Log{stale} })
	for i := 0; i < 2*testSettle; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
		h.passCooldowns()
	}
	if _, still := h.sink.liveKeys()[keyOf(reported)]; still {
		t.Fatal("the orphaned transfer was not reported as reverted")
	}
	if in.count(watcher.IncidentStaleLog) == 0 {
		t.Fatalf("incidents = %+v", in.list)
	}
}

// A stalled head, the other answer: instead of "no such block" the primary returns
// another hash for the block of the transfer it hides. At settle depth that
// is a header disagreement like any other: the third endpoint decides.
func TestAnotherHashInsteadOfNull(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(3), withIncidents(&in))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)
	txHash := h.pay(addr(1), 100)
	paid := h.chain.Mine(1)
	h.tick()

	var mu sync.Mutex
	afterLogs := false
	h.nodes[0].Set(func(b *evmtest.Behavior) {
		b.DropLogs = func(l evm.Log) bool { return l.TxHash == txHash }
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			mu.Lock()
			defer mu.Unlock()
			if method == "eth_getLogs" {
				afterLogs = true
				return nil, nil, false
			}
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			tag, n, numeric := blockParam(params)
			if tag == "latest" {
				afterLogs = false
			}
			if numeric && n == paid.Number && afterLogs {
				blk, _ := h.chain.BlockByNumber(n)
				blk.Hash[0] ^= 0xff
				return headerJSON(blk, blk.Time), nil, true
			}
			return nil, nil, false
		}
	})
	for i := 0; i < 4*testSettle; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
	}
	if head := h.sink.lastHead().Number; head != h.chain.Head().Number-testSettle {
		t.Fatalf("head = %d, tip = %d: the liar was not outvoted (hold %q)", head, h.chain.Head().Number, h.w.Status().Hold)
	}
	if h.client.Status()[0].Healthy || len(in.list) == 0 {
		t.Fatalf("endpoint 0 healthy = %v, incidents = %+v", h.client.Status()[0].Healthy, in.list)
	}
	for _, i := range in.list {
		if i.Endpoint != "" && i.Endpoint != evm.RedactURL(h.urls[0]) {
			t.Fatalf("an honest endpoint was blamed: %+v", i)
		}
	}
	h.assertModel(startBlock)
}

// A stalled head with two endpoints. Nobody can outvote the liar, so the head can be
// held — but never silently: the hold names its cause and an incident is
// raised, again and again.
func TestHeldHeadIsAlertedWithTwoEndpoints(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(2), withIncidents(&in))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)
	h.pay(addr(1), 100)
	paid := h.chain.Mine(1)
	h.tick()
	// Endpoint 1 now claims another block where the payment is, for good.
	h.nodes[1].Set(lieAboutHash(h.chain, paid.Number))
	for i := 0; i < 3*testSettle; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
		h.passCooldowns()
	}
	st := h.w.Status()
	if head := h.sink.lastHead().Number; head >= paid.Number {
		t.Fatalf("head %d passed the disputed block %d", head, paid.Number)
	}
	if st.Hold == "" || in.count(watcher.IncidentHeadHeld) < 2 || in.count(watcher.IncidentHeaderMismatch) == 0 {
		t.Fatalf("hold = %q, incidents = %+v: a held head must say why and be alerted repeatedly", st.Hold, in.list)
	}
	if len(h.sink.liveKeys()) != 1 {
		t.Fatal("the payment was reverted on one endpoint's word")
	}
}

// A throttled log endpoint across a restart. What the watcher has learnt about an endpoint —
// that it serves logs — is in the checkpoint, so an endpoint that throttles
// from the first request of a new process is still one that has to confirm
// log scans, not one "without a log service".
func TestThrottledFromTheStartOfANewProcess(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(2), withIncidents(&in))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)

	h.nodes[1].Set(func(b *evmtest.Behavior) {
		b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"}
	})
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } })
	h.restartWithNewClient()
	h.pay(addr(1), 100)
	paid := h.chain.Mine(1)
	for i := 0; i < testSettle+6; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
		h.passCooldowns()
	}
	st := h.w.Status()
	if head := h.sink.lastHead().Number; head >= paid.Number || st.LogsUnchecked {
		t.Fatalf("head %d (payment hidden in block %d), status %+v: blocks were settled on the primary's logs alone", head, paid.Number, st)
	}
	if !strings.Contains(st.Hold, "log verification unavailable") || in.count(watcher.IncidentHeadHeld) == 0 {
		t.Fatalf("hold = %q, incidents = %+v", st.Hold, in.list)
	}
	if es := h.client.Status()[1]; es.Logs.Unavailable || !es.Logs.Known {
		t.Fatalf("second endpoint: logs %+v — a known log endpoint must not be written off", es.Logs)
	}
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError = nil })
	h.passCooldowns()
	h.mineTicking(2)
	h.assertModel(startBlock)
}

// The configuration that really has one log endpoint: blocks are
// settled unchecked (and the status says so), the range is remembered, and
// when a second endpoint that serves logs appears, the blocks in which an
// address that is still watched can have been paid are scanned again. A
// payment the first endpoint hid is then reported — late, below the head.
func TestUncheckedBlocksAreScannedAgainWhenASecondLogEndpointAppears(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(2), withIncidents(&in))
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.noLogService(1, &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"})
	h.mineTicking(testSettle + 2)
	if st := h.w.Status(); !st.LogsUnchecked || st.UncheckedFrom == 0 || st.UncheckedTo != st.Cursor {
		t.Fatalf("status = %+v", st)
	}
	// The only log endpoint hides a payment; its block is settled.
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } })
	h.pay(addr(1), 100)
	paid := h.chain.Mine(1)
	h.sink.set(func(s *sink) { s.headRule = nil }) // the head passes the hidden payment: that is this configuration's limit
	h.mineTicking(testSettle + 4)
	if len(h.sink.liveKeys()) != 0 || h.sink.lastHead().Number < paid.Number {
		t.Fatalf("test setup: the payment should be hidden and its block settled")
	}
	unchecked := h.w.Status().UncheckedFrom
	if unchecked == 0 || unchecked > paid.Number {
		t.Fatalf("unchecked from %d, payment in %d", unchecked, paid.Number)
	}
	// A restart does not forget it.
	h.restart()
	h.mineTicking(1)
	if got := h.w.Status().UncheckedFrom; got != unchecked {
		t.Fatalf("unchecked from %d after a restart, was %d", got, unchecked)
	}
	// The second endpoint starts serving logs (the operator replaced it).
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError = nil })
	h.passLogPause() // the endpoint is asked again
	for i := 0; i < 4; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
	}
	if len(h.sink.liveKeys()) != 1 {
		t.Fatalf("the hidden payment was not found by the re-check (status %+v)", h.w.Status())
	}
	if in.count(watcher.IncidentLateTransfer) != 1 {
		t.Fatalf("incidents = %+v", in.list)
	}
	if st := h.w.Status(); st.UncheckedFrom != 0 || st.UncheckedTo != 0 || st.LogsUnchecked {
		t.Fatalf("status = %+v", st)
	}
	h.sink.set(func(s *sink) { s.headRule = h.checkHeadRule })
	h.mineTicking(3)
	h.assertModel(startBlock)
}

// A reorganisation deeper than the settle depth, kept quiet by the primary:
// it answers truthfully for every new block and keeps returning the old
// headers for the blocks the watcher has settled on, so the reorg check —
// "is the newest remembered block still there?" — would pass on its word,
// and a payment in a block that is gone would stay on the books (the same
// class as stale logs keeping an orphaned transfer alive, one level down). The verifier is asked about the newest
// remembered block as well; the third endpoint breaks the tie.
func TestADeepReorgHiddenByThePrimaryIsNoticed(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(3), withIncidents(&in), withConfig(func(c *watcher.Config) { c.NoFinality = true }))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 2)
	h.pay(addr(1), 100)
	paid := h.chain.Mine(1)
	h.mineTicking(testSettle + 2) // the head has passed the payment's block
	if h.sink.lastHead().Number < paid.Number || len(h.sink.liveKeys()) != 1 {
		t.Fatal("test setup: the payment should be reported and settled")
	}

	old := h.chain.Clone()
	h.chain.Reorg(testSettle + 4) // the payment's block is replaced; the payment is not mined again
	h.chain.Mine(testSettle + 6)
	if blk, _ := h.chain.BlockByNumber(paid.Number); blk.Hash == paid.Hash {
		t.Fatal("test setup: the reorg did not replace the block")
	}
	// Old headers for everything up to the block the watcher's cursor rests
	// on; the truth above it, so that every new block is agreed on.
	top := h.w.Status().Cursor
	h.nodes[0].Set(func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			_, n, numeric := blockParam(params)
			if blk, ok := old.BlockByNumber(n); numeric && ok && n <= top {
				return headerJSON(blk, blk.Time), nil, true
			}
			return nil, nil, false
		}
	})
	for i := 0; i < 2*testSettle; i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
	}
	if len(h.sink.liveKeys()) != 0 {
		t.Fatalf("the payment of a block that was reorganised away is still on the books (head %d, hold %q)", h.sink.lastHead().Number, h.w.Status().Hold)
	}
	if h.client.Status()[0].Healthy {
		t.Fatal("the endpoint that hid the reorganisation was not set aside")
	}
	for _, i := range in.list {
		if i.Endpoint != "" && i.Endpoint != evm.RedactURL(h.urls[0]) {
			t.Fatalf("an honest endpoint was blamed: %+v", i)
		}
	}
	h.assertModel(startBlock)
}
