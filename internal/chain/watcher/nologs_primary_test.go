package watcher_test

import (
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// TestEndpointWithoutLogServiceIsNotTheOneATickStartsFrom: one of three
// endpoints has no log service, and is held to have none. It is asked again
// whenever its pause ends — as a second log source, whose refusal costs
// nothing. Before, it was picked as the endpoint the tick starts from once
// its pause had ended, the tick's scan was refused, and the tick failed: a
// payment made then (a wake-up after a quiet stretch, say) was reported only
// after the retry.
func TestEndpointWithoutLogServiceIsNotTheOneATickStartsFrom(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.addSince(addr(1), h.chain.Head().Number)
	h.noLogService(1, &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"})
	h.mineTicking(1) // the checkpoint records the verdict
	if !strings.Contains(string(h.store.snapshot()), `"no_log_servers":["`+evm.RedactURL(h.urls[1])) {
		t.Fatalf("test setup: the checkpoint does not record the verdict: %s", h.store.snapshot())
	}
	// A third endpoint is added, and the operator puts the one without a
	// log service first in the list; the checkpoint remembers the verdict.
	third := evmtest.NewNode(h.chain)
	h.urls = []string{h.urls[1], h.urls[0], third.Serve(t)}
	h.nodes = []*evmtest.Node{h.nodes[1], h.nodes[0], third}
	h.restartWithNewClient()
	for round, why := range []string{"right after the restart", "when its pause has ended"} {
		if round == 1 {
			h.passLogPause() // it is to be asked again
		}
		tx := h.pay(addr(1), int64(12+round))
		h.chain.Mine(1)
		if err := h.w.Tick(t.Context()); err != nil {
			t.Fatalf("%s: the tick failed: %v", why, err)
		}
		if _, found := findTransfer(h, tx); !found {
			t.Fatalf("%s: the payment was not reported by the first tick (primary %s)", why, h.w.Status().Endpoint)
		}
		if st := h.w.Status(); st.Endpoint == evm.RedactURL(h.urls[0]) {
			t.Fatalf("%s: the tick started from the endpoint without a log service: %+v", why, st)
		}
	}
	if !h.client.Status()[0].Logs.NoService {
		t.Fatalf("the endpoint without a log service: %+v", h.client.Status()[0].Logs)
	}
}
