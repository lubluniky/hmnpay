package corebridge

import (
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// TestTheNodeStatusSaysWhatTheWatcherWaitsFor: the node status carries the
// watcher's reasons, as the dashboard shows them. Blocks settled on one
// endpoint's logs while the other has no log service are owed a second
// scan: `work` says which blocks and what that waits for, though nothing is
// held. A verifier far behind holds the head: `hold` says so.
func TestTheNodeStatusSaysWhatTheWatcherWaitsFor(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.nodes[1].Set(func(b *evmtest.Behavior) {
		b.LogsError = &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"}
	})
	w.start()
	w.firstHead()
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	w.mine(testSettle + 2)
	w.writeOffLogs()
	w.eventually("the node status to say what the blocks settled on one endpoint's logs wait for", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Relogin())
		return err == nil && strings.Contains(ns.Work, "still to be scanned by a second one")
	})

	w.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 4 * testSettle })
	w.eventually("the node status to say why the head is held", func() bool {
		w.mine(1)
		ns, err := w.env.Service.NodeStatus(w.env.Relogin())
		return err == nil && ns.Hold != "" && strings.Contains(ns.Work, "the processed head is waiting")
	})
}
