package watcher_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// Blocks settled on one endpoint's logs are recorded, and a second endpoint
// that serves logs scans them later. "Second" means another endpoint than the
// one whose logs settled them — not the one that happens to have the role of
// the second log source when the scan is made. A new endpoint put first in
// the list makes the old one the second log source; the old one's scan of its
// own blocks would only repeat what it said the first time.

// coveredBy reports whether one of the node's log queries covered block n.
func coveredBy(n *evmtest.Node, block uint64) bool {
	for _, q := range n.LogQueries() {
		if q.From <= block && block <= q.To && !q.Refused {
			return true
		}
	}
	return false
}

// settledAlone builds a single-endpoint harness whose blocks are settled on
// that endpoint's logs (it is the only one), with an address that is watched
// throughout, and returns it with the recorded unchecked range and the first
// block of it in which the address can have been paid (above its Since).
func settledAlone(t *testing.T, opts ...option) (*harness, watcher.Status, uint64) {
	t.Helper()
	h := newHarness(t, opts...)
	since := h.chain.Head().Number
	h.watch.addSince(addr(1), since)
	h.mineTicking(12)
	st := h.w.Status()
	if st.UncheckedFrom == 0 || st.UncheckedTo <= since || !st.LogsUnchecked {
		t.Fatalf("test setup: a single endpoint settles on its own logs, and records it: %+v", st)
	}
	return h, st, max(st.UncheckedFrom, since+1)
}

// TestUncheckedBlocksAreNotCheckedByTheEndpointThatSettledThem: an endpoint
// is added in front of the one that settled the blocks alone. The record is
// closed only after the new endpoint has scanned the blocks.
func TestUncheckedBlocksAreNotCheckedByTheEndpointThatSettledThem(t *testing.T) {
	h, before, owed := settledAlone(t)
	fresh := evmtest.NewNode(h.chain)
	h.urls = append([]string{fresh.Serve(t)}, h.urls...)
	h.restartWithNewClient()
	for i := 0; i == 0 || i < 20 && h.w.Status().UncheckedFrom != 0; i++ {
		h.chain.Mine(1)
		h.tick()
	}
	if st := h.w.Status(); st.UncheckedFrom != 0 || st.LogsUnchecked {
		t.Fatalf("the unchecked blocks %d..%d were never checked: %+v", before.UncheckedFrom, before.UncheckedTo, st)
	}
	for b := owed; b <= before.UncheckedTo; b++ {
		if !coveredBy(fresh, b) {
			t.Fatalf("the record of blocks %d..%d settled on one endpoint's logs was closed, and the new endpoint never scanned block %d; its log queries: %+v",
				before.UncheckedFrom, before.UncheckedTo, b, fresh.LogQueries())
		}
	}
}

// TestPaymentHiddenByTheOnlyLogSourceIsFoundAfterAReordering: the endpoint
// that settled the blocks alone left a payment out of its logs, and keeps
// leaving it out. An honest endpoint is added in front of it: the payment is
// found, late, by the honest endpoint's scan of the recorded blocks.
func TestPaymentHiddenByTheOnlyLogSourceIsFoundAfterAReordering(t *testing.T) {
	var in incidents
	h := newHarness(t, withIncidents(&in))
	h.sink.set(func(s *sink) { s.headRule = nil }) // a lone endpoint is trusted with what it hides
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.mineTicking(3)
	tx := h.pay(addr(1), 70)
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(l evm.Log) bool { return l.TxHash == tx } })
	paid := h.chain.Mine(1)
	h.mineTicking(testSettle + 4)
	if st := h.w.Status(); st.Cursor <= paid.Number || st.UncheckedFrom > paid.Number || len(h.sink.liveKeys()) != 0 {
		t.Fatalf("test setup: block %d settled on the lone endpoint's logs without the payment: %+v", paid.Number, st)
	}

	fresh := evmtest.NewNode(h.chain)
	h.urls = append([]string{fresh.Serve(t)}, h.urls...)
	h.restartWithNewClient()
	for i := 0; i == 0 || i < 20 && h.w.Status().UncheckedFrom != 0; i++ {
		h.chain.Mine(1)
		h.tick()
	}
	if _, found := findTransfer(h, tx); !found {
		t.Fatalf("the payment the lone endpoint hid in block %d was never found: the unchecked blocks were checked by the endpoint that hid it (status %+v)", paid.Number, h.w.Status())
	}
	if in.count(watcher.IncidentLateTransfer) != 1 {
		t.Fatalf("late-transfer incidents: %d", in.count(watcher.IncidentLateTransfer))
	}
}

// TestUncheckedBlocksOfAnOldCheckpointTakeTwoEndpoints: a checkpoint written
// before the endpoint that settled the blocks was recorded names nobody. Any
// one endpoint may have been the one; so the record is closed only when two
// different endpoints have scanned the blocks.
func TestUncheckedBlocksOfAnOldCheckpointTakeTwoEndpoints(t *testing.T) {
	h, before, owed := settledAlone(t)
	cp := string(h.store.snapshot())
	i := strings.Index(cp, `"unchecked":[`)
	j := i + strings.Index(cp[i:], `}]`)
	if i < 0 || j < i {
		t.Fatalf("test setup: no unchecked record in %s", cp)
	}
	if cp[i-1] == ',' {
		i--
	}
	legacy := cp[:i] + `,"unchecked_from":` + uitoa(before.UncheckedFrom) + `,"unchecked_to":` + uitoa(before.UncheckedTo) + cp[j+2:]
	h.store.restore([]byte(legacy))

	second := evmtest.NewNode(h.chain)
	h.urls = append(h.urls, second.Serve(t))
	h.restartWithNewClient()
	first := h.nodes[0]
	firstBefore := len(first.LogQueries())
	for i := 0; i == 0 || i < 30 && h.w.Status().UncheckedFrom != 0; i++ {
		h.chain.Mine(1)
		h.tick()
	}
	if st := h.w.Status(); st.UncheckedFrom != 0 {
		t.Fatalf("the unchecked blocks of the old checkpoint were never checked: %+v", st)
	}
	again := false
	for _, q := range first.LogQueries()[firstBefore:] {
		if q.From <= owed && owed <= q.To {
			again = true
		}
	}
	if !coveredBy(second, owed) || !again {
		t.Fatalf("the record of an old checkpoint (blocks %d..%d, settled by nobody on record) was closed after the scan of one endpoint only (second: %v, first again: %v)",
			before.UncheckedFrom, before.UncheckedTo, coveredBy(second, owed), again)
	}
}

func uitoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

var _ = context.Background

// TestEndpointUnderANewURLCannotCheckItsOwnBlocks: the lone log source
// settled blocks on its own logs, leaving a payment out, and goes on leaving
// it out. The operator changes its URL — the same provider under another host,
// so another label — and adds an honest endpoint in front of it. The record
// names the old label, which is no longer configured: nobody can tell whether
// the new label is the endpoint that settled the blocks. So its scan alone
// cannot make up for them; the honest endpoint has to scan them as well, and
// finds the payment.
func TestEndpointUnderANewURLCannotCheckItsOwnBlocks(t *testing.T) {
	var in incidents
	h := newHarness(t, withIncidents(&in))
	h.sink.set(func(s *sink) { s.headRule = nil }) // a lone endpoint is trusted with what it hides
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.mineTicking(3)
	tx := h.pay(addr(1), 70)
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(l evm.Log) bool { return l.TxHash == tx } })
	paid := h.chain.Mine(1)
	h.mineTicking(testSettle + 4)
	if st := h.w.Status(); st.Cursor <= paid.Number || st.UncheckedFrom == 0 || st.UncheckedFrom > paid.Number {
		t.Fatalf("test setup: block %d settled on the lone endpoint's logs: %+v", paid.Number, st)
	}

	honest := evmtest.NewNode(h.chain)
	renamed := h.nodes[0].Serve(t) // the same provider, under another URL
	h.urls = []string{honest.Serve(t), renamed}
	h.nodes = []*evmtest.Node{honest, h.nodes[0]}
	h.restartWithNewClient()
	for i := 0; i == 0 || i < 30 && h.w.Status().UncheckedFrom != 0; i++ {
		h.chain.Mine(1)
		h.tick()
	}
	if _, found := findTransfer(h, tx); !found {
		t.Fatalf("the payment the lone endpoint hid in block %d was never found: the endpoint that hid it, under its new URL, made up for its own blocks (status %+v; the honest endpoint scanned them: %v)",
			paid.Number, h.w.Status(), coveredBy(honest, paid.Number))
	}
	if st := h.w.Status(); st.UncheckedFrom != 0 {
		t.Fatalf("the record of unchecked blocks is still open: %+v", st)
	}
}

// TestHeadSaysHowFarBlocksHaveBeenScannedByTwoEndpoints: while blocks settled
// on one endpoint's logs wait for a second endpoint's scan, the processed
// head carries a time no later than the oldest of them (Head.Checked) — the
// consumer must not stop watching an address whose window ends after it. With
// a single endpoint configured there is nobody to make that scan, and the
// head says nothing is outstanding. Once the second endpoint has scanned the
// blocks, the head is reported again, without it.
func TestHeadSaysHowFarBlocksHaveBeenScannedByTwoEndpoints(t *testing.T) {
	h, st, _ := settledAlone(t)
	if c := h.sink.lastHead().Checked; !c.IsZero() {
		t.Fatalf("a single endpoint configured: the head says blocks are owed a second scan (%s)", c)
	}
	first, ok := h.chain.BlockByNumber(st.UncheckedFrom)
	if !ok {
		t.Fatalf("no block %d", st.UncheckedFrom)
	}

	// A second endpoint, which is down: best effort settles on the first
	// one's logs alone, and the blocks wait.
	second := evmtest.NewNode(h.chain)
	second.Set(func(b *evmtest.Behavior) { b.Down = true })
	h.urls = append(h.urls, second.Serve(t))
	h.nodes = append(h.nodes, second)
	h.cfg.Verify = watcher.VerifyBestEffort
	h.restartWithNewClient()
	for range watcher.SoloPatience + 2 {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
	}
	head := h.sink.lastHead()
	if head.Checked.IsZero() || head.Checked.Unix() > int64(first.Time) {
		t.Fatalf("blocks from %d (stamped %d) are owed a second scan, and the head says checked %s (head %+v, status %+v)",
			first.Number, first.Time, head.Checked, head, h.w.Status())
	}

	second.Set(func(b *evmtest.Behavior) { b.Down = false })
	h.client.Refresh(context.Background())
	for i := 0; i < 20 && (h.w.Status().UncheckedFrom != 0 || !h.sink.lastHead().Checked.IsZero()); i++ {
		h.chain.Mine(1)
		_ = h.w.Tick(context.Background())
	}
	if st, c := h.w.Status(), h.sink.lastHead().Checked; st.UncheckedFrom != 0 || !c.IsZero() {
		t.Fatalf("after the second endpoint scanned the blocks: status %+v, head checked %s", st, c)
	}
}

// TestHeadSaysWhichBlockIsBeyondReorganisation: the head carries the
// highest block the watcher treats as beyond reorganisation — the finalized
// block when the endpoints report finality (the head itself when finality
// is ahead of it), else the block a reorganisation window below the head.
func TestHeadSaysWhichBlockIsBeyondReorganisation(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.mineTicking(testWindow + 10)
	hd := h.sink.lastHead()
	if hd.Finalized == 0 || hd.Final != min(hd.Number, hd.Finalized) {
		t.Fatalf("with finality: head %+v", hd)
	}
	n := newHarness(t, withNodes(2), withConfig(func(c *watcher.Config) { c.NoFinality = true }))
	n.watch.addSince(addr(1), n.chain.Head().Number)
	n.mineTicking(testWindow + 10)
	if hd := n.sink.lastHead(); hd.Finalized != 0 || hd.Final != hd.Number-testWindow {
		t.Fatalf("without finality: head %+v (reorg window %d)", hd, testWindow)
	}
}

// TestUncheckedBlocksScannedByAReturningPairAreChecked: blocks settled on
// one endpoint's logs are scanned by one of two others during a
// configuration that does not include the first. Then the first and that
// witness are configured together again: both are on record for the
// blocks, both are configured, so the blocks have been checked — the record
// is closed, without waiting for anything, in the first tick.
func TestUncheckedBlocksScannedByAReturningPairAreChecked(t *testing.T) {
	h, _, owed := settledAlone(t)
	original := h.urls[0]
	b, c := evmtest.NewNode(h.chain), evmtest.NewNode(h.chain)
	bURL, cURL := b.Serve(t), c.Serve(t)
	h.urls = []string{bURL, cURL}
	h.restartWithNewClient()
	for i := 0; i < 10 && !coveredBy(b, owed) && !coveredBy(c, owed); i++ {
		h.chain.Mine(1)
		h.tick()
	}
	if coveredBy(b, owed) == coveredBy(c, owed) {
		t.Fatal("test setup: one historical scan, by one of the two")
	}
	witness := bURL
	if coveredBy(c, owed) {
		witness = cURL
	}
	h.urls = []string{original, witness}
	h.restartWithNewClient()
	h.chain.Mine(1)
	h.tick()
	if s := h.w.Status(); s.UncheckedFrom != 0 || s.UncheckedWaiting != "" {
		t.Fatalf("both endpoints on record for block %d are configured again, and the record is open: %+v", owed, s)
	}
}

// TestUncheckedRecordStaysBoundedThroughEndpointChurn: blocks settled on
// one endpoint's logs; then a hundred configurations of two new endpoints
// each, every one replaced after a single tick — in which one of the two has
// scanned the blocks and the other has not yet. Only endpoints configured
// now count, and only they stay on record: the record stays within what the
// checkpoint can hold, every restart reads it back, and the last pair, left
// in place, closes it.
func TestUncheckedRecordStaysBoundedThroughEndpointChurn(t *testing.T) {
	h, _, _ := settledAlone(t)
	labels := func() int {
		cp := string(h.store.snapshot())
		i := strings.Index(cp, `"unchecked":[`)
		if i < 0 {
			return 0
		}
		return strings.Count(cp[i:i+strings.Index(cp[i:], "}]")], "http://")
	}
	for i := range 100 {
		a, b := evmtest.NewNode(h.chain), evmtest.NewNode(h.chain)
		h.urls = []string{a.Serve(t), b.Serve(t)}
		h.restartWithNewClient()
		h.chain.Mine(1)
		h.tick()
		if n := labels(); n > 2 {
			t.Fatalf("after %d changes of configuration the record names %d endpoints:\n%s", i+1, n, h.store.snapshot())
		}
		if h.w.Status().UncheckedFrom == 0 {
			t.Fatalf("test setup: configuration %d closed the record in one tick", i+1)
		}
		if i%10 == 9 {
			h.restart() // the checkpoint is read back
		}
	}
	h.mineTicking(3)
	if s := h.w.Status(); s.UncheckedFrom != 0 {
		t.Fatalf("the last pair, left in place, did not close the record: %+v", s)
	}
}

// TestPaymentHiddenByTheOnlyLogSourceIsFoundWhileTheChainStandsStill: as
// above, but after the honest endpoint is added no block is mined, so no
// tick has anything new to settle. What a second endpoint is owed does not
// wait for new blocks: the recorded blocks are scanned all the same, and the
// payment is found. (It used to be made up for only in a tick that settled
// blocks; while the chain stood still, or the head was held, the blocks stayed
// on the record, the payment unreported.)
func TestPaymentHiddenByTheOnlyLogSourceIsFoundWhileTheChainStandsStill(t *testing.T) {
	h := newHarness(t)
	h.sink.set(func(s *sink) { s.headRule = nil }) // a lone endpoint is trusted with what it hides
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.mineTicking(3)
	tx := h.pay(addr(1), 70)
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(l evm.Log) bool { return l.TxHash == tx } })
	paid := h.chain.Mine(1)
	h.mineTicking(testSettle + 4)
	if st := h.w.Status(); st.Cursor <= paid.Number || st.UncheckedFrom > paid.Number || len(h.sink.liveKeys()) != 0 {
		t.Fatalf("test setup: block %d settled on the lone endpoint's logs without the payment: %+v", paid.Number, st)
	}
	fresh := evmtest.NewNode(h.chain)
	h.urls = append([]string{fresh.Serve(t)}, h.urls...)
	h.restartWithNewClient()
	for i := 0; i == 0 || i < 10 && h.w.Status().UncheckedFrom != 0; i++ {
		h.tick() // no new block
	}
	if _, found := findTransfer(h, tx); !found {
		t.Fatalf("the payment the lone endpoint hid in block %d was not found while the chain stood still (status %+v)", paid.Number, h.w.Status())
	}
	if st := h.w.Status(); st.UncheckedFrom != 0 {
		t.Fatalf("blocks %d..%d still unchecked", st.UncheckedFrom, st.UncheckedTo)
	}
}
