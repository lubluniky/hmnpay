package watcher_test

import (
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// TestNewEndpointThatCannotBeReachedIsStillASecondLogSource: two endpoints,
// one of which has no log service, so blocks are settled on the other's logs
// (recorded as unchecked). A third endpoint is added and cannot be reached
// from the start — down, or throttled before it has answered anything, so
// its chain id is not verified yet. It has never been asked for logs, so it
// counts as one that serves them, and the head waits for it (invariant 3).
// Before, an endpoint whose chain id was not verified was not among the
// endpoints the second log scan was asked of at all, and the blocks were
// settled on the first endpoint's logs alone as if there were no one else.
func TestNewEndpointThatCannotBeReachedIsStillASecondLogSource(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.noLogService(1, &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"})
	h.mineTicking(2)
	settled := h.w.Status()
	if !settled.LogsUnchecked {
		t.Fatalf("test setup: %+v", settled)
	}

	third := evmtest.NewNode(h.chain)
	third.Set(func(b *evmtest.Behavior) { b.Down = true })
	h.urls = append(h.urls, third.Serve(t))
	h.nodes = append(h.nodes, third)
	h.restartWithNewClient()
	for range testSettle + 2 {
		h.chain.Mine(1)
		_ = h.w.Tick(t.Context())
	}
	st := h.w.Status()
	if st.Cursor > settled.Cursor+1 || st.LogsUnchecked {
		t.Fatalf("blocks %d..%d were settled on one endpoint's logs while a third endpoint, never asked for logs, could not be reached (logs unchecked: %v, hold %q)",
			settled.Cursor+1, st.Cursor, st.LogsUnchecked, st.Hold)
	}
	if !strings.Contains(st.Hold, evm.RedactURL(h.urls[2])) {
		t.Fatalf("the head waits without naming the endpoint it waits for: %q", st.Hold)
	}

	// It comes up: its logs confirm the blocks, and the head moves on.
	third.Set(func(b *evmtest.Behavior) { b.Down = false })
	h.passCooldowns()
	h.mineTicking(testSettle + 2)
	if st := h.w.Status(); st.Cursor <= settled.Cursor+1 || st.LogsUnchecked || st.LogVerifier != evm.RedactURL(h.urls[2]) {
		t.Fatalf("after the third endpoint came up: %+v", st)
	}
}

// TestEndpointWithoutLogsAskedAgainDoesNotHoldTheHead: of two endpoints, one
// is held to have no log service, and is asked for logs again every so often
// in case it has one now. When it still refuses, nothing has changed: the
// blocks are settled on the other's logs, as between two such questions —
// the head is not held for the tick in which it was asked.
func TestEndpointWithoutLogsAskedAgainDoesNotHoldTheHead(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.noLogService(1, &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"})
	h.mineTicking(2)
	if !h.w.Status().LogsUnchecked {
		t.Fatalf("test setup: %+v", h.w.Status())
	}
	for round := range 3 {
		h.passLogPause()
		asked := len(h.nodes[1].LogQueries())
		before := h.w.Status().Cursor
		h.chain.Mine(1)
		_ = h.w.Tick(t.Context())
		st := h.w.Status()
		if len(h.nodes[1].LogQueries()) == asked {
			t.Fatalf("round %d: the endpoint without a log service was not asked again after its pause", round)
		}
		if st.Cursor <= before || st.Hold != "" {
			t.Fatalf("round %d: asking it again held the head at %d: %q", round, st.Cursor, st.Hold)
		}
	}
}
