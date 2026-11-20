package corebridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// TestNodeStatusRightAfterRestart reproduces what live tests on BSC mainnet
// found: after a restart the node status kept saying healthy:false, every
// endpoint "unverified" and latest_block 0 for seconds after the watcher
// had scanned the chain (and a payment had become paid), because the report
// the core serves was only refreshed by the report loop's timer and at the
// start of the next tick — never at the end of a tick. Here both timers are
// an hour long: whatever the node status says after the first tick must
// come from that tick.
func TestNodeStatusRightAfterRestart(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.start()
	w.synced()
	w.stop()

	// The restart: a payment exists (so log scans have something to
	// confirm), and neither the report loop nor the next tick come round
	// during the test.
	w.env.CreatePayment("25")
	w.mine(testSettle + 2) // blocks the first tick scans for it
	w.opts.StatusInterval = time.Hour
	w.opts.PollInterval = time.Hour
	w.start()

	w.eventually("the first tick of the new process", func() bool {
		return !w.watcherStatus().LastTickAt.IsZero()
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		if err != nil {
			t.Fatal(err)
		}
		ok := ns.Healthy && !ns.Starting && ns.LatestBlock == w.chain.Head().Number &&
			ns.Primary != "" && ns.Verifier != "" && ns.Primary != ns.Verifier && ns.LogVerifier != "" && !ns.Solo
		for _, e := range ns.Endpoints {
			ok = ok && e.State == core.EndpointHealthy && e.Error == ""
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first tick has completed, but the node status does not say so:\n%+v\nwatcher: %+v", ns, w.watcherStatus())
		}
		time.Sleep(time.Millisecond)
	}
}

// TestNodeStatusWhileStarting: before the watcher's first tick has
// completed, the node status says "starting" — not healthy, but no endpoint
// is reported as failed for not having been asked yet — and it ends with
// that tick.
func TestNodeStatusWhileStarting(t *testing.T) {
	w := newWorld(t, withNodes(2))
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	t.Cleanup(open)
	for _, n := range w.nodes {
		// Headers wait (the watcher's first request of a tick); chain id
		// and block number, which is all the client's health check
		// asks, do not.
		n.Set(func(b *evmtest.Behavior) {
			b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
				if method == "eth_getBlockByNumber" {
					<-release
				}
				return nil, nil, false
			}
		})
	}
	w.start()

	var ns core.NodeStatus
	w.eventually("a report while the first tick waits for the endpoints", func() bool {
		var err error
		ns, err = w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && !ns.LastCheckedAt.IsZero()
	})
	if !ns.Starting || ns.Healthy || ns.Primary != "" || len(ns.Warnings) != 0 || len(ns.Endpoints) != 2 {
		t.Fatalf("node status while starting = %+v", ns)
	}
	for _, e := range ns.Endpoints {
		if e.Error != "" || (e.State != core.EndpointNotChecked && e.State != core.EndpointHealthy) {
			t.Fatalf("an endpoint that has merely not been asked yet is reported as a fault: %+v", e)
		}
	}
	if !w.watcherStatus().LastTickAt.IsZero() {
		t.Fatal("test setup: the first tick has completed already")
	}

	open()
	w.eventually("the node status to be healthy after the first tick", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && ns.Healthy && !ns.Starting && ns.Primary != "" && ns.Verifier != ""
	})
}

// TestNodeStatusStartingEndsWithAFailedTick: a first tick that fails is a
// fault, not a start: "starting" ends with it.
func TestNodeStatusStartingEndsWithAFailedTick(t *testing.T) {
	w := newWorld(t)
	w.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
	w.start()
	w.eventually("a failed first tick to be reported", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && !ns.Starting && !ns.LastCheckedAt.IsZero() && len(ns.Endpoints) == 1 &&
			ns.Endpoints[0].Error != "" && ns.Endpoints[0].State != core.EndpointNotChecked
	})
	if ns, _ := w.env.Service.NodeStatus(w.env.Operator()); ns.Healthy {
		t.Fatalf("node status = %+v", ns)
	}
}

// TestNodeStatusNamesTheVerifiers: the node status says which endpoint is
// read from, which one verifies it and which one confirms its log scans,
// with URLs redacted like the endpoint rows: provider keys do not leak.
func TestNodeStatusNamesTheVerifiers(t *testing.T) {
	w := newWorld(t, withNodes(2))
	keyed := []string{w.urls[0] + "/v1/SUPERSECRETKEY", w.urls[1] + "/?apikey=SUPERSECRETKEY"}
	if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{RPCURLs: keyed}); err != nil {
		t.Fatal(err)
	}
	w.start()
	w.synced()
	w.env.CreatePayment("25")
	w.mine(testSettle + 2)
	var ns core.NodeStatus
	w.eventually("primary, verifier and log verifier in the node status", func() bool {
		var err error
		ns, err = w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && ns.Healthy && ns.Primary != "" && ns.Verifier != "" && ns.LogVerifier != ""
	})
	redacted := []string{evm.RedactURL(keyed[0]), evm.RedactURL(keyed[1])}
	if ns.Primary == ns.Verifier || !contains(redacted, ns.Primary) || !contains(redacted, ns.Verifier) ||
		ns.LogVerifier != ns.Verifier || ns.Solo || ns.LogsUnchecked || ns.Hold != "" || len(ns.Warnings) != 0 {
		t.Fatalf("node status = %+v (endpoints %v)", ns, redacted)
	}
	for _, e := range ns.Endpoints {
		if e.InUse != (e.URL == ns.Primary) {
			t.Fatalf("the row in use is not the primary: %+v", ns)
		}
	}
	if s := fmt.Sprintf("%+v", ns); strings.Contains(s, "SUPERSECRETKEY") {
		t.Fatalf("an API key from an endpoint URL is in the node status: %s", s)
	}

	// A held head shows as Hold and as a warning: the verifier goes down.
	w.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
	w.mine(testSettle + 2)
	w.eventually("the hold in the node status", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		if err != nil || ns.Hold == "" {
			return false
		}
		for _, warning := range ns.Warnings {
			if strings.Contains(warning, ns.Hold) {
				return true
			}
		}
		return false
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestEndpointState(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		row  evm.EndpointStatus
		want string
	}{
		{evm.EndpointStatus{State: evm.StateUnverified}, core.EndpointNotChecked},
		{evm.EndpointStatus{State: evm.StateUnverified, LastErrorAt: now, LastError: "timeout"}, core.EndpointUnverified},
		{evm.EndpointStatus{State: evm.StateHealthy, LastSuccessAt: now}, core.EndpointHealthy},
		{evm.EndpointStatus{State: evm.StateFailing, LastErrorAt: now}, core.EndpointFailing},
		{evm.EndpointStatus{State: evm.StateLagging}, core.EndpointLagging},
		{evm.EndpointStatus{State: evm.StateWrongChain}, core.EndpointWrongChain},
	} {
		if got := endpointState(c.row); got != c.want {
			t.Errorf("endpointState(%+v) = %s, want %s", c.row, got, c.want)
		}
	}
}
