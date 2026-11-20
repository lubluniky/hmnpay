package corebridge

// At the seam with the core: what an endpoint that misstates the chain's
// height, or is far behind, or serves no logs, means for the processed head
// the core is given and for what the operator is told.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// lowestCoreHead waits for cond while recording the lowest head the core
// held in the meantime (a head of 0 is "none yet").
func (w *world) lowestCoreHead(what string, cond func() bool) uint64 {
	w.t.Helper()
	lowest := ^uint64(0)
	w.eventually(what, func() bool {
		if h := w.coreHead().Number; h != 0 {
			lowest = min(lowest, h)
		}
		return cond()
	})
	return lowest
}

// At the bridge: the checkpoint is lost (or discarded) and one of two
// endpoints is far behind when the watcher starts again. The start position
// used to follow that endpoint down — and the processed head the core is
// given with it, so that the core's own head, which is where a later start
// resumes from, was poisoned as well. Now the watcher does not start on two
// heads that far apart, the core's head stays where it was, and the operator
// is told why nothing moves.
func TestTheCoresHeadIsNotMovedBackByAnEndpointThatIsBehind(t *testing.T) {
	t.Run("two endpoints: no start until they agree", func(t *testing.T) {
		w := newWorld(t, withNodes(2))
		w.start()
		w.synced()
		head := w.coreHead().Number
		w.stop()
		w.store.set(nil) // the checkpoint is gone
		w.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 150 })
		p := w.env.CreatePayment("25")
		w.pay(p, 25)
		w.mine(3)

		w.start()
		lowest := w.lowestCoreHead("the watcher to say why it does not start", func() bool {
			st := w.bridge.Status()
			return strings.Contains(st.Watcher.LastError, "cannot choose a start position") && st.Alerts[AlertHeadHeld] > 0
		})
		w.never("the core's head moves back", func() bool { return w.coreHead().Number < head })
		if lowest < head {
			t.Fatalf("the core's head went from %d back to %d on the word of the endpoint that is behind", head, lowest)
		}
		if got := w.status(p); got != core.StatusAwaiting {
			t.Fatalf("payment is %s while nothing could be scanned", got)
		}
		// The endpoint catches up (or is replaced): the watcher starts where
		// the core left off, and the payment made in the meantime is there.
		w.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 0 })
		w.mine(testSettle + testConfirmations + 1)
		lowest = w.lowestCoreHead("the payment to be paid", func() bool { return w.status(p) == core.StatusPaid })
		if lowest < head {
			t.Fatalf("the core's head went from %d back to %d", head, lowest)
		}
	})

	t.Run("three endpoints: the one that is behind is outvoted", func(t *testing.T) {
		for behind := 0; behind < 3; behind++ {
			w := newWorld(t, withNodes(3))
			w.start()
			w.synced()
			head := w.coreHead().Number
			w.stop()
			w.store.set(nil)
			w.nodes[behind].Set(func(b *evmtest.Behavior) { b.Lag = 150 })
			p := w.env.CreatePayment("25")
			w.pay(p, 25)
			w.mine(testSettle + testConfirmations + 1)
			w.start()
			lowest := w.lowestCoreHead("the payment to be paid", func() bool { return w.status(p) == core.StatusPaid })
			if lowest < head {
				t.Fatalf("endpoint %d behind: the core's head went from %d back to %d", behind, head, lowest)
			}
			w.stop()
		}
	})
}

// poison leaves the instance the way a start that followed one endpoint down
// left it in an older version, before the start position was bounded: a
// checkpoint far below the chain, and —
// with core — the core's own head down there with it.
func (w *world) poison(low uint64, coreToo bool) {
	w.t.Helper()
	var cp map[string]any
	if err := json.Unmarshal(w.store.get(), &cp); err != nil {
		w.t.Fatalf("no checkpoint to take the chain and token from: %v", err)
	}
	data, err := json.Marshal(map[string]any{"v": cp["v"], "chain_id": cp["chain_id"], "token": cp["token"], "cursor": low})
	if err != nil {
		w.t.Fatal(err)
	}
	w.store.set(data)
	if !coreToo {
		return
	}
	blk, ok := w.chain.BlockByNumber(low)
	if !ok {
		w.t.Fatalf("no block %d", low)
	}
	if err := w.env.Service.AdvanceHead(w.env.System(), core.ChainHead{
		ChainID: core.ChainBSCMainnet, Number: blk.Number, Hash: blk.Hash.String(), Time: time.Unix(int64(blk.Time), 0),
	}); err != nil {
		w.t.Fatal(err)
	}
}

// The damage that outlived its cause. An instance whose checkpoint —
// and whose core head — were put far below the chain by such a start used to
// have the whole distance to scan before it saw a payment again, also after
// the endpoint was gone, and deleting the checkpoint did not help. It
// recovers by itself now: blocks nobody needs are not scanned.
func TestAPoisonedInstanceRecovers(t *testing.T) {
	const low = chainStart - chainHistory + 5

	t.Run("checkpoint and core head are both far below the chain", func(t *testing.T) {
		w := newWorld(t, withNodes(2))
		w.start()
		w.synced()
		w.stop()
		w.mine(2 * watcher.DefaultReorgWindow) // the chain has gone on
		w.poison(low, true)
		if h := w.coreHead().Number; h != low {
			t.Fatalf("test setup: core head %d", h)
		}
		for _, n := range w.nodes {
			n.ResetCounters()
		}

		w.start()
		w.synced() // the core's head is back at the chain's
		if n := w.bridge.Status().Alerts[AlertCheckpointRepaired]; n != 1 {
			t.Fatalf("checkpoint_repaired alerts = %d", n)
		}
		// A payment made now is seen at once, and nothing was — or is —
		// scanned down there.
		p := w.env.CreatePayment("25")
		w.pay(p, 25)
		w.mine(testSettle + testConfirmations + 1)
		w.waitStatus(p, core.StatusPaid)
		for i, n := range w.nodes {
			for _, q := range n.LogQueries() {
				if q.From < chainStart {
					t.Fatalf("endpoint %d was asked for the logs of blocks %d..%d: the poisoned range was scanned", i, q.From, q.To)
				}
			}
		}
	})

	t.Run("only the checkpoint is: the core's head is never dragged down to it", func(t *testing.T) {
		w := newWorld(t, withNodes(2))
		w.start()
		w.synced()
		w.stop()
		w.mine(2 * watcher.DefaultReorgWindow)
		p := w.env.CreatePayment("25") // an open payment: its address is watched, from the core's head on
		head := w.coreHead().Number
		w.poison(low, false)
		w.start()
		w.pay(p, 25)
		w.mine(testSettle + testConfirmations + 1)
		lowest := w.lowestCoreHead("the payment to be paid", func() bool { return w.status(p) == core.StatusPaid })
		if lowest+watcher.DefaultReorgWindow < head {
			t.Fatalf("the core's head went from %d back to %d, more than a reorg window", head, lowest)
		}
	})

	t.Run("a payment created while the head was down there is still found", func(t *testing.T) {
		w := newWorld(t, withNodes(2))
		w.start()
		w.synced()
		w.stop()
		w.mine(2 * watcher.DefaultReorgWindow)
		w.poison(low, true)
		p := w.env.CreatePayment("25") // start block: the poisoned head
		w.pay(p, 25)
		w.mine(testSettle + testConfirmations + 1)
		w.start()
		// Its address can have been paid from the poisoned head on, as far
		// as the core knows: those blocks are scanned, and the payment is
		// found. (This is the case the watcher cannot shorten: the address
		// is as old as the damage.)
		w.waitStatus(p, core.StatusPaid)
	})
}

// At the bridge: best-effort verification says so when it starts to
// take one endpoint's word, and when it stops; and what is settled on one
// endpoint's logs is said again and again, not once.
func TestSingleSourceIsAnnounced(t *testing.T) {
	w := newWorld(t, withNodes(2), withOptions(func(o *Options) {
		o.Verify = watcher.VerifyBestEffort
		o.LogsUncheckedRepeat = 20 * time.Millisecond
	}))
	w.start()
	w.synced()
	if st := w.bridge.Status(); st.Alerts[AlertSingleSource] != 0 || st.Watcher.Solo {
		t.Fatalf("status %+v", st)
	}
	// The second endpoint throttles: it is there. Nothing is taken on the
	// first one's word, and the head waits.
	w.nodes[1].Set(func(b *evmtest.Behavior) { b.RateLimited = true })
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	w.mine(testSettle + testConfirmations + 1)
	w.eventually("the head to wait for the throttled endpoint", func() bool { return w.bridge.Status().Alerts[AlertHeadHeld] > 0 })
	w.never("a payment is accepted on one endpoint's word while the other answers", func() bool {
		return w.status(p) != core.StatusAwaiting || w.watcherStatus().Solo
	})
	// It goes away altogether: after a few ticks the first is on its own.
	w.nodes[1].Set(func(b *evmtest.Behavior) { b.RateLimited = false; b.Down = true })
	w.waitStatus(p, core.StatusPaid)
	w.eventually("single-source operation to be announced", func() bool {
		st := w.bridge.Status()
		return st.Alerts[AlertSingleSource] == 1 && st.Watcher.Solo && len(st.Warnings) > 0 && strings.Contains(strings.Join(st.Warnings, "\n"), "one endpoint's word")
	})
	w.eventually("the reminder that log scans are not cross-checked to repeat", func() bool {
		return w.bridge.Status().Alerts[AlertLogsUnchecked] >= 3
	})
	// And back.
	w.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
	w.passCooldowns()
	w.mine(1)
	w.eventually("the end of single-source operation to be announced", func() bool {
		st := w.bridge.Status()
		return st.Alerts[AlertSingleSourceEnded] == 1 && !st.Watcher.Solo
	})
}
