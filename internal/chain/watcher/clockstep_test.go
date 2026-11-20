package watcher_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// TestClockSteppedForwardUnderATickIsNoBlockFromTheFuture: a tick reads the
// clock when it begins. If the clock is stepped forward a moment later — time
// synchronisation correcting a clock that was slow — the newest block, mined
// after the step by a chain that keeps real time, is ahead of that reading,
// and not of the clock. It is not a block from the future: no incident, no
// endpoint set aside, and the tick goes on.
func TestClockSteppedForwardUnderATickIsNoBlockFromTheFuture(t *testing.T) {
	var in incidents
	var h *harness
	step := false
	h = newHarness(t, withNodes(2), withIncidents(&in), withConfig(func(c *watcher.Config) {
		c.Now = func() time.Time {
			now := h.clock.Now()
			if step {
				// Read by the tick as it begins; the step comes right
				// after, and a block with it.
				step = false
				h.clock.Advance(5 * time.Minute)
				h.chain.SetTime(h.clock.Now())
				h.chain.Mine(1)
			}
			return now
		}
	}))
	h.chain.SetTime(h.clock.Now())
	h.mineTicking(testSettle + 2)
	step = true
	if err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("a tick across a step of the clock: %v", err)
	}
	if n := in.count(watcher.IncidentFutureBlock); n != 0 {
		t.Fatalf("%d future-block incident(s) for a block mined after the clock was stepped forward", n)
	}
	for _, s := range h.client.Status() {
		if s.LastError != "" && s.State != "healthy" {
			t.Fatalf("an endpoint was set aside: %+v", s)
		}
	}
	h.mineTicking(2)
}

// TestClockSkewIsWhatTwoEndpointsAgreeOn: one endpoint stamps its newest
// block three minutes ahead of the clock, the other's newest block is older
// than the clock. The skew is not taken on the first one's word — it drives
// the core's refusal of payments while the clock is behind — so none is
// reported, and the status names the dispute. When both are ahead, the
// smaller of the two is the skew.
func TestClockSkewIsWhatTwoEndpointsAgreeOn(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.tick()
	h.clock.Advance(12 * time.Minute)
	h.chain.SetTime(h.clock.Now().Add(3 * time.Minute))
	h.chain.Mine(1)
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 1 })
	_ = h.w.Tick(t.Context())
	s := h.w.Status()
	if s.ClockSkew != 0 || !strings.Contains(s.ClockDispute, "3m0s behind") {
		t.Fatalf("one endpoint 3 minutes ahead, the other behind: skew %s, dispute %q", s.ClockSkew, s.ClockDispute)
	}
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 0 })
	h.chain.SetTime(h.clock.Now().Add(5 * time.Minute))
	h.chain.Mine(1)
	_ = h.w.Tick(t.Context())
	if s := h.w.Status(); s.ClockSkew > -2*time.Minute || s.ClockDispute != "" {
		t.Fatalf("both endpoints ahead: skew %s, dispute %q", s.ClockSkew, s.ClockDispute)
	}
}
