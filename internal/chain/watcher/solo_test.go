package watcher_test

import (
	"context"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// Best effort believes one endpoint alone once every other endpoint has been
// unreachable beside it for SoloPatience ticks in a row. The ticks are that
// endpoint's: when another endpoint becomes the one that is left, it was
// among those that did not answer, and its patience starts from nothing.

// TestBestEffortPatienceBelongsToTheEndpointLeft: two endpoints. The second
// is down for all but one of the ticks it takes to go solo; then the two swap
// — the first goes down, the second comes back. The second is not believed
// alone on its first tick (it was the one that was down), but after as many
// ticks of its own as anybody.
func TestBestEffortPatienceBelongsToTheEndpointLeft(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(2), withIncidents(&in), withConfig(func(c *watcher.Config) { c.Verify = watcher.VerifyBestEffort }))
	h.tick()
	ctx := context.Background()
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
	for range watcher.SoloPatience - 1 {
		_ = h.w.Tick(ctx)
	}
	before := h.w.Status()
	if before.Solo || before.Endpoint != evm.RedactURL(h.urls[0]) {
		t.Fatalf("test setup: %+v", before)
	}

	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
	h.client.Refresh(ctx)
	for i := 1; i < watcher.SoloPatience; i++ {
		_ = h.w.Tick(ctx)
		st := h.w.Status()
		if st.Endpoint != evm.RedactURL(h.urls[1]) {
			t.Fatalf("tick %d after the swap: the primary is %s", i, st.Endpoint)
		}
		if st.Solo || h.w.Solo() {
			t.Fatalf("tick %d after the swap: %s is believed alone, although it was the endpoint that did not answer until a moment ago (%d ticks of patience)",
				i, st.Endpoint, watcher.SoloPatience)
		}
	}
	_ = h.w.Tick(ctx)
	if st := h.w.Status(); !st.Solo || h.w.SoloEndpoint() != evm.RedactURL(h.urls[1]) {
		t.Fatalf("after %d ticks of its own the endpoint left is not believed alone: %+v", watcher.SoloPatience, st)
	}
	if in.count(watcher.IncidentSingleSource) != 1 {
		t.Fatalf("single-source incidents: %d", in.count(watcher.IncidentSingleSource))
	}
}

// TestBestEffortSoloEndsWhenAnotherEndpointIsTheOneLeft: one endpoint is
// believed alone; then it goes down and the other comes back. That ends
// single-source operation — the endpoint believed alone is not the one in use
// any more — and the other has to wait its own ticks.
func TestBestEffortSoloEndsWhenAnotherEndpointIsTheOneLeft(t *testing.T) {
	var in incidents
	h := newHarness(t, withNodes(2), withIncidents(&in), withConfig(func(c *watcher.Config) { c.Verify = watcher.VerifyBestEffort }))
	h.tick()
	ctx := context.Background()
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
	for range watcher.SoloPatience {
		_ = h.w.Tick(ctx)
	}
	if st := h.w.Status(); !st.Solo || h.w.SoloEndpoint() != evm.RedactURL(h.urls[0]) {
		t.Fatalf("test setup: %+v", st)
	}
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
	h.client.Refresh(ctx)
	_ = h.w.Tick(ctx)
	st := h.w.Status()
	if st.Solo || h.w.Solo() || st.Endpoint != evm.RedactURL(h.urls[1]) {
		t.Fatalf("the first tick on the other endpoint: %+v (believed alone: %q)", st, h.w.SoloEndpoint())
	}
	if in.count(watcher.IncidentSingleSourceEnded) != 1 {
		t.Fatalf("single-source operation ended without an incident (%d)", in.count(watcher.IncidentSingleSourceEnded))
	}
}

// TestBestEffortPatienceStartsAgainWhenAnotherEndpointAnsweredBetweenTicks:
// the ticks counted towards believing one endpoint alone have to be ticks in
// a row in which every other endpoint was silent. One that comes back
// between two ticks — it answers the pool's health check — and is down again
// by the next tick has answered all the same, and the count starts again.
// Before, nothing looked at what had happened between the ticks.
func TestBestEffortPatienceStartsAgainWhenAnotherEndpointAnsweredBetweenTicks(t *testing.T) {
	h := newHarness(t, withNodes(2), withConfig(func(c *watcher.Config) { c.Verify = watcher.VerifyBestEffort }))
	h.tick()
	ctx := context.Background()
	down := func(on bool) { h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = on }) }
	down(true)
	for range watcher.SoloPatience - 2 {
		_ = h.w.Tick(ctx)
	}
	// Back for a moment, between two ticks: it answers the health check.
	down(false)
	h.client.Refresh(ctx)
	down(true)
	h.client.Refresh(ctx)
	for i := 1; i < watcher.SoloPatience; i++ {
		_ = h.w.Tick(ctx)
		if st := h.w.Status(); st.Solo {
			t.Fatalf("believed alone %d tick(s) after the other endpoint last answered (it takes %d)", i, watcher.SoloPatience)
		}
	}
	_ = h.w.Tick(ctx)
	if st := h.w.Status(); !st.Solo {
		t.Fatalf("not believed alone after %d ticks in which the other endpoint was silent: %+v", watcher.SoloPatience, st)
	}
}
