package corebridge

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// The watcher of these tests idles for an hour once it has nothing to watch:
// whatever it does after that, it does because somebody woke it.

// idleWorld starts a world with two endpoints and waits for its watcher to
// come to rest idle.
func idleWorld(t *testing.T, opts ...worldOption) *world {
	t.Helper()
	w := newWorld(t, append([]worldOption{withNodes(2), withOptions(func(o *Options) {
		o.IdlePollInterval = time.Hour
		// The health check often, so that the endpoints' heights in the
		// node status follow the chain within the test.
		o.HealthInterval = 5 * time.Millisecond
	})}, opts...)...)
	w.start()
	w.synced()
	w.eventually("the watcher to idle", func() bool { return w.watcherStatus().Idle })
	return w
}

// scans counts the header requests the endpoints have received. Every tick
// of the watcher begins with one on each of two endpoints, and nothing else
// asks for headers while nothing is watched (the health check asks for the
// chain id and the block number): the count moves exactly when the watcher
// has ticked. (Not Status.LastTickAt: it is stamped with the core's clock,
// which stands still in these tests.)
func (w *world) scans() int {
	total := 0
	for _, n := range w.nodes {
		total += n.Calls("eth_getBlockByNumber")
	}
	return total
}

// TestIdleWatcherIsWokenByAPayment: every way a payment comes into being puts
// its address on the watch list and wakes the watcher — the API's
// CreatePayment and the public checkout of a payment link — and the payment
// is then found and settled as fast as on a watcher that never idled.
func TestIdleWatcherIsWokenByAPayment(t *testing.T) {
	create := map[string]func(w *world) core.PaymentView{
		"created by the API": func(w *world) core.PaymentView { return w.env.CreatePayment("25") },
		"created by a payment-link checkout": func(w *world) core.PaymentView {
			link, err := w.env.Service.CreateLink(w.env.Operator(), core.LinkInput{Name: "Top-up", Active: true})
			if err != nil {
				t.Fatal(err)
			}
			// Creating the link is an event too — it must not wake the
			// watcher: there is still nothing to watch.
			before := w.scans()
			w.never("a tick for an event that added nothing to the watch list", func() bool { return w.scans() != before })
			co, _, err := w.env.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "25"})
			if err != nil {
				t.Fatal(err)
			}
			return w.env.Payment(co.ID)
		},
	}
	for name, make := range create {
		t.Run(name, func(t *testing.T) {
			w := idleWorld(t)
			// Idle means idle: blocks are mined and no tick looks at them.
			before := w.scans()
			w.mine(10)
			w.never("a tick of an idle watcher that nobody woke", func() bool { return w.scans() != before })
			stale := w.coreHead().Final // the start block: the final block of the head at creation

			started := time.Now()
			p := make(w)
			w.eventually("the wake-up tick to see the payment's address", func() bool {
				return w.watcherStatus().WatchedAddresses == 1
			})
			woke := time.Since(started)
			if woke > 5*time.Second {
				t.Errorf("the watcher saw the new address %s after the payment was created", woke)
			}
			t.Logf("wake-up tick completed %s after the payment was created (idle interval: an hour)", woke.Round(time.Millisecond))

			// Its start block is the final block of the head the core held
			// at its creation — older still, lower than the chain, never
			// higher.
			cs, err := w.env.Service.ChainState(w.env.System())
			if err != nil || len(cs.Addresses) != 1 {
				t.Fatalf("chain state: %+v, %v", cs, err)
			}
			if since := cs.Addresses[0].SinceBlock; since != stale || since > w.chain.Head().Number-testSettle-10 {
				t.Fatalf("start block %d; the core's head at creation was %d, the chain is at %d", since, stale, w.chain.Head().Number)
			}

			w.pay(p, 25)
			w.mine(testSettle + testConfirmations + 1)
			w.waitStatus(p, core.StatusPaid)
			// Paid, and its transfer still inside the watcher's reorg
			// window: not at rest yet. (When it has left the window the
			// watcher is quiet: see quiet_test.go.)
			if st := w.watcherStatus(); st.Idle || st.Quiet {
				t.Fatalf("at rest with a transfer inside the reorg window: %+v", st)
			}
			ns, err := w.env.Service.NodeStatus(w.env.Operator())
			if err != nil || ns.Idle || ns.Quiet {
				t.Fatalf("node status says idle with a transfer inside the reorg window: %+v, %v", ns, err)
			}
		})
	}
}

// TestIdleWatcherIsWokenByARescan: Bridge.Rescan does not wait for the idle
// pause to end.
func TestIdleWatcherIsWokenByARescan(t *testing.T) {
	w := idleWorld(t)
	before := w.scans()
	w.never("a tick of an idle watcher that nobody woke", func() bool { return w.scans() != before })
	if !w.bridge.Rescan(w.coreHead().Number - 10) {
		t.Fatal("no running watcher")
	}
	w.eventually("the re-scan", func() bool {
		return w.scans() != before && strings.Contains(w.logs.String(), "chain watcher re-scan requested")
	})
	w.eventually("the watcher to idle again", func() bool { return w.watcherStatus().Idle })
}

// TestNodeStatusWhileIdle: an instance with nothing to watch is not a sick
// one. The node status says idle; the newest block keeps following the chain
// (the health check sees it), the core's head stays where the last idle tick
// left it, and that distance — which would count as lag on a watcher that is
// at work — does not make the node unhealthy. A dead endpoint does.
func TestNodeStatusWhileIdle(t *testing.T) {
	w := idleWorld(t)
	head := w.coreHead().Number
	w.mine(80) // more than the 50 blocks of lag a busy watcher is allowed
	var ns core.NodeStatus
	w.eventually("the node status to show the chain ahead of an idle watcher", func() bool {
		var err error
		ns, err = w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && ns.LatestBlock == w.chain.Head().Number
	})
	if !ns.Idle || !ns.Healthy || ns.Starting || ns.Head.Number != head || ns.LagBlocks != 80+testSettle {
		t.Fatalf("node status of an idle instance, 80 blocks after its last tick: idle=%v healthy=%v starting=%v head=%d (was %d) lag=%d\n%+v",
			ns.Idle, ns.Healthy, ns.Starting, ns.Head.Number, head, ns.LagBlocks, ns)
	}
	if ns.Hold != "" || len(ns.Warnings) != 0 || ns.Primary == "" || ns.Verifier == "" {
		t.Fatalf("node status of an idle instance: %+v", ns)
	}
	for _, e := range ns.Endpoints {
		if e.State != core.EndpointHealthy || e.Error != "" {
			t.Fatalf("endpoint of an idle instance: %+v", e)
		}
	}

	// The endpoint in use dies. No tick is due for an hour; the health
	// check finds it, and the node is not healthy any more.
	for i, e := range ns.Endpoints {
		if e.InUse {
			w.nodes[i].Set(func(b *evmtest.Behavior) { b.Down = true })
		}
	}
	w.eventually("the dead endpoint to show in the node status", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		if err != nil || ns.Healthy {
			return false
		}
		for _, e := range ns.Endpoints {
			if !e.Healthy && e.Error != "" {
				return true
			}
		}
		return false
	})
}

// TestNodeStatusIdleEndsWhenTheIdleTickIsOverdue: "idle" excuses the distance
// between the core's head and the chain only while the idle ticks keep
// coming. A watcher whose tick hangs still says what its last tick said; the
// node status does not go on calling that idle.
func TestNodeStatusIdleEndsWhenTheIdleTickIsOverdue(t *testing.T) {
	w := newWorld(t, withNodes(2), withOptions(func(o *Options) {
		o.IdlePollInterval = 300 * time.Millisecond
		o.HealthInterval = 5 * time.Millisecond
	}))
	w.start()
	w.synced()
	w.eventually("the node status to say idle", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && ns.Idle && ns.Healthy
	})

	// The next tick's first request never returns.
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	t.Cleanup(open)
	for _, n := range w.nodes {
		n.Set(func(b *evmtest.Behavior) {
			b.FinalityDepth = w.finality
			b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
				if method == "eth_getBlockByNumber" {
					<-release
				}
				return nil, nil, false
			}
		})
	}
	w.mine(80)
	w.eventually("idle to end, and the lag to count, once the idle tick is overdue", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && !ns.Idle && !ns.Healthy && ns.LagBlocks > 50
	})
	if st := w.watcherStatus(); !st.Idle {
		t.Fatalf("test setup: the watcher's own status should still be that of its last tick: %+v", st)
	}

	// The endpoints answer again: the tick completes, and the instance is
	// idle and healthy as before.
	open()
	w.eventually("the node status to say idle again", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && ns.Idle && ns.Healthy
	})
}

// TestHealthCheckSlowsDownWhileIdle: the pool's health check costs two calls
// per endpoint, and runs half as often while the watcher idles — but never
// less often than the watcher's idle ticks.
func TestHealthCheckSlowsDownWhileIdle(t *testing.T) {
	g := &generation{b: &Bridge{opts: Options{HealthInterval: 15 * time.Second}}}
	for _, tc := range []struct {
		idlePoll   time.Duration
		idle       bool
		want       time.Duration
		idlePollIs string
	}{
		{0, false, 15 * time.Second, "the default"},
		{0, true, 30 * time.Second, "the default"},
		{5 * time.Minute, true, 30 * time.Second, "longer than two health intervals"},
		{20 * time.Second, true, 20 * time.Second, "shorter than two health intervals"},
		{10 * time.Second, true, 15 * time.Second, "shorter than the health interval"},
		{5 * time.Minute, false, 15 * time.Second, "irrelevant"},
	} {
		g.b.opts.IdlePollInterval = tc.idlePoll
		if got := g.healthInterval(tc.idle); got != tc.want {
			t.Errorf("idle=%v, idle poll interval %s (%s): health check every %s, want %s", tc.idle, tc.idlePoll, tc.idlePollIs, got, tc.want)
		}
	}

	// And in a running bridge: while the watcher idles, each endpoint is
	// asked for its block number by the health check and by nobody else.
	w := idleWorld(t, withOptions(func(o *Options) { o.HealthInterval = 40 * time.Millisecond }))
	for _, n := range w.nodes {
		n.ResetCounters()
	}
	const window = 800 * time.Millisecond
	time.Sleep(window)
	for i, n := range w.nodes {
		// One check every 80 ms while idle (every 40 ms otherwise). A slow
		// machine makes fewer, never more.
		checks, limit := n.Calls("eth_blockNumber"), int(window/(80*time.Millisecond))+2
		if checks > limit {
			t.Errorf("endpoint %d: %d health checks in %s while idle, want at most %d (one every 80 ms)", i, checks, window, limit)
		}
		if other := n.TotalCalls() - checks - n.Calls("eth_chainId"); other != 0 {
			t.Errorf("endpoint %d: %d calls besides the health check from an idle watcher with an hour between ticks", i, other)
		}
	}
}
