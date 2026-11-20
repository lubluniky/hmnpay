package corebridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// An address leaves the watch list once the blocks of its window have been
// scanned for it — for the last time, by the endpoints that are meant to
// scan them. Two ways those blocks can still be unscanned while every clock
// says the window is over: the operator configured another asset for a
// while, and nothing scanned the old one's blocks meanwhile; or the blocks
// were settled on one endpoint's logs and the second endpoint's scan of them
// is still to come.

// listed reports whether the core lists the payment's address for the
// watcher.
func (w *world) listed(p core.PaymentView) bool {
	w.t.Helper()
	cs, err := w.env.Service.ChainState(w.env.System())
	if err != nil {
		w.t.Fatal(err)
	}
	for _, a := range cs.Addresses {
		if a.PaymentID == p.ID {
			return true
		}
	}
	return false
}

// TestPaymentMinedWhileAnotherAssetWasConfiguredIsFoundOnTheWayBack: an
// invoice is cancelled, and its address is watched for late funds until an
// hour after its deadline. The operator switches to another token for a while; inside the hour
// the payer sends the first token anyway. By the time the operator switches
// back the hour is over by every clock — but no head of that token has been
// processed since the switch, so nothing has scanned those blocks for the
// address. It is still listed, the watcher scans from where it was created,
// and the money shows up as late funds on the cancelled invoice.
func TestPaymentMinedWhileAnotherAssetWasConfiguredIsFoundOnTheWayBack(t *testing.T) {
	other := ethaddr.MustParse("0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d")
	w := newWorld(t, withNodes(2))
	w.chain.AddToken(other, "USDC", 18)
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	if _, err := w.env.Service.CancelPayment(w.env.Operator(), p.ID); err != nil {
		t.Fatal(err)
	}
	switchTo := func(a core.Asset) {
		t.Helper()
		if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{Asset: &a, RPCURLs: w.urls}); err != nil {
			t.Fatal(err)
		}
	}
	usdc := core.Asset{Symbol: "USDC", Network: "BEP20", ChainID: core.ChainBSCMainnet, Contract: "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d", Decimals: 18}
	switchTo(usdc)
	w.mine(testSettle + 2)
	w.synced() // the other token's watcher runs, on the same chain

	w.advance(10 * time.Minute)
	w.pay(p, 25) // inside the hour: the first token, to the cancelled invoice
	paid := w.mine(1)
	// The window of an invoice cancelled before its deadline runs to an
	// hour after the deadline.
	w.advance(core.DefaultPaymentTTL + core.DefaultExpiryGrace + core.DefaultUnfundedWatchWindow + core.WatchRetirementMargin)
	w.mine(testSettle + 2)
	w.synced()

	switchTo(core.USDTBSC)
	w.mine(testSettle + 2)
	w.synced()
	w.eventually("the payment mined while another token was configured to be found", func() bool {
		got := w.env.Payment(p.ID)
		return got.AmountReceived == "25.00" && got.NeedsAttention
	})
	if got := w.events(p); got == "" {
		t.Fatalf("no events on the payment paid in block %d", paid.Number)
	}
}

// TestPaymentHiddenByTheOnlyLogSourceIsFoundAfterTheWindowWhenASecondArrives:
// of two endpoints, one has no log service; blocks are settled on the other's
// logs alone, and it leaves a payment out. The invoice expires unpaid; its
// hour of late-funds watching ends by every clock. Its address stays listed
// — blocks of that hour are still to be scanned by a second log endpoint —
// and when the operator adds one, its scan of those blocks finds the payment,
// as late funds on the expired invoice.
func TestPaymentHiddenByTheOnlyLogSourceIsFoundAfterTheWindowWhenASecondArrives(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.nodes[1].Set(func(b *evmtest.Behavior) {
		b.LogsError = &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"}
	})
	w.start()
	w.firstHead()
	p := w.env.CreatePayment("25")
	tx := w.pay(p, 25)
	w.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(l evm.Log) bool { return l.TxHash == tx } })
	w.mine(testSettle + 2)
	w.writeOffLogs()
	w.eventually("blocks settled on one endpoint's logs", func() bool { return w.watcherStatus().LogsUnchecked })
	w.synced()
	w.advance(core.DefaultPaymentTTL + core.DefaultExpiryGrace + time.Second)
	w.mine(testSettle + 2)
	p = w.waitStatus(p, core.StatusExpired)
	w.advance(core.DefaultUnfundedWatchWindow + core.WatchRetirementMargin + time.Minute)
	w.mine(testFinality + 1) // the final block, too, is past the window then
	w.synced()
	if !w.listed(p) {
		t.Fatal("the expired invoice's address left the watch list while blocks of its window, settled on one endpoint's logs, were still to be scanned by a second")
	}
	retained := func() int {
		ns, err := w.env.Service.NodeStatus(w.env.Relogin())
		if err != nil {
			t.Fatal(err)
		}
		return ns.RetainedUnchecked
	}
	if n := retained(); n != 1 {
		t.Fatalf("node status counts %d address(es) kept for unchecked blocks, want 1", n)
	}

	honest := evmtest.NewNode(w.chain)
	honest.Set(func(b *evmtest.Behavior) { b.FinalityDepth = w.finality })
	w.nodes = append(w.nodes, honest)
	w.urls = append(w.urls, honest.Serve(t))
	if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{RPCURLs: w.urls}); err != nil {
		t.Fatal(err)
	}
	w.mine(testSettle + 2)
	w.eventually("the payment the only log source hid to be found", func() bool {
		got := w.env.Payment(p.ID)
		return got.AmountReceived == "25.00" && got.NeedsAttention
	})
	if got := w.env.Payment(p.ID); got.Status != core.StatusExpired {
		t.Fatalf("the late payment changed the outcome: %+v", got)
	}
	// Funded now, it is watched for the long window from here on; it is no
	// longer kept for the blocks of the old one.
	w.eventually("the node status to count no address kept for unchecked blocks", func() bool {
		w.mine(1)
		return retained() == 0
	})
}

// TestNoPaymentWhileTheEndpointsShowTheClockIsBehind: the chain runs ten
// minutes ahead of the server clock. The watcher refuses those blocks — they
// look as if they came from the future — so the processed head stays behind;
// what shows the clock is behind is the newest block both endpoints have. The
// bridge reports it, and the core refuses payments until the clock is right.
func TestNoPaymentWhileTheEndpointsShowTheClockIsBehind(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.start()
	w.synced()
	create := func() error {
		_, _, err := w.env.Service.CreatePayment(w.env.System(), core.CreatePaymentInput{Amount: "1"})
		return err
	}
	if err := create(); err != nil {
		t.Fatal(err)
	}
	w.chain.SetTime(w.env.Clock.Now().Add(10 * time.Minute))
	w.chain.Mine(testSettle + 2)
	w.eventually("payments to be refused while the clock is ten minutes behind the chain", func() bool {
		return errors.Is(create(), core.ErrClockBehind)
	})
	w.advance(10 * time.Minute) // the clock is put right
	w.mine(testSettle + 2)
	w.eventually("payments to be accepted again", func() bool { return create() == nil })
}

// TestPaymentMovedBelowItsHeadByAReorganisationAboveFinalityIsFound: with
// the production settle depth, an invoice is handed out at the processed
// head, sixteen blocks below the tip; the finalized block is further below
// still (finality lags). The payer pays; a reorganisation above the
// finalized block — the finalized prefix stands — replaces the blocks from
// the head the invoice was handed out at, and the transaction is mined again
// at exactly that height. The payment is found — by a watcher that restarts
// without its checkpoint, by one that restarts with it, and by one that
// keeps running: the start block of the address's scan is the head's final
// block, below which no reorganisation the watcher accepts reaches.
func TestPaymentMovedBelowItsHeadByAReorganisationAboveFinalityIsFound(t *testing.T) {
	for _, path := range []string{"restart without the checkpoint", "restart with the checkpoint", "running"} {
		t.Run(path, func(t *testing.T) {
			w := newWorld(t, withNodes(2), withFinality(40))
			w.opts.SettleDepth = watcher.DefaultSettleDepth
			w.start()
			tip := w.chain.Head().Number
			start := tip - w.opts.SettleDepth
			w.eventually("the processed head", func() bool { return w.coreHead().Number == start })
			p := w.env.CreatePayment("25")
			if path != "running" {
				w.stop()
			}
			final := tip - 40 // the nodes report finality 40 blocks back
			anchor := mustBlock(t, w.chain, final)
			w.pay(p, 25)
			w.mine(1)
			dropped := w.chain.Reorg(int(w.opts.SettleDepth) + 2)
			w.chain.Requeue(dropped...)
			moved := w.mine(1)
			if moved.Number != start || len(dropped) != 1 || moved.Number <= final {
				t.Fatalf("test setup: moved to %d, start %d, %d dropped, final %d", moved.Number, start, len(dropped), final)
			}
			if path == "restart without the checkpoint" {
				w.store.set(nil)
			}
			w.mine(50)
			if path != "running" {
				w.start()
			}
			w.eventually("the replacement chain to be processed", func() bool {
				return w.coreHead().Number == w.chain.Head().Number-w.opts.SettleDepth
			})
			if mustBlock(t, w.chain, final).Hash != anchor.Hash {
				t.Fatal("test setup: the finalized block was replaced")
			}
			w.eventually("the payment mined again at the height of the head it was handed out at to be credited", func() bool {
				return w.env.Payment(p.ID).AmountReceived == "25.00"
			})
		})
	}
}

// TestAnAddressStaysAcrossACheckpointWithoutTheFinalBlocksTime: the watcher's
// checkpoint records its finalized block without the block's time, as one
// written before the time was recorded does. Restarted from it, with the
// final block where it was, the watcher reads that block again and reports
// its time; until it has, the head's final block has no time, and no address
// leaves the watch list on it — unknown is not "past". A cancelled invoice's
// address is still watched, and money paid inside its window is found. The
// same with the current checkpoint.
func TestAnAddressStaysAcrossACheckpointWithoutTheFinalBlocksTime(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("time on record %v", !legacy), func(t *testing.T) {
			w := newWorld(t, withNodes(2))
			w.start()
			w.synced()
			w.stop()
			p := w.env.CreatePayment("25")
			if _, err := w.env.Service.CancelPayment(w.env.Operator(), p.ID); err != nil {
				t.Fatal(err)
			}
			if legacy {
				var cp map[string]json.RawMessage
				var fin map[string]json.RawMessage
				if err := json.Unmarshal(w.store.get(), &cp); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(cp["finalized"], &fin); err != nil {
					t.Fatal(err)
				}
				if _, ok := fin["t"]; !ok {
					t.Fatal("test setup: the checkpoint has no time of the finalized block to remove")
				}
				delete(fin, "t")
				cp["finalized"], _ = json.Marshal(fin)
				data, _ := json.Marshal(cp)
				w.store.set(data)
			}
			// One block more, and finality one block further behind: the
			// final block stays the one on record.
			depth := w.chain.Head().Number - w.coreHead().Final + 1
			for _, n := range w.nodes {
				n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = depth })
			}
			w.mine(1)
			w.start()
			w.synced()
			if !w.listed(p) {
				t.Fatalf("the cancelled invoice's address left the watch list at head %d, final %d (time %v)", w.coreHead().Number, w.coreHead().Final, w.coreHead().FinalTime)
			}
			if w.coreHead().FinalTime.IsZero() {
				t.Fatalf("the final block's time was not read again: head %+v", w.coreHead())
			}
			w.pay(p, 25)
			paid := w.mine(1)
			w.mine(int(depth) + testSettle)
			w.synced()
			if w.coreHead().Final < paid.Number {
				t.Fatalf("test setup: block %d not final", paid.Number)
			}
			w.eventually("the money paid inside the window to be found", func() bool { return w.env.Payment(p.ID).AmountReceived == "25.00" })
		})
	}
}

// TestAMissingCheckpointThatTookTheSecondScanRecordIsAlerted: of two
// endpoints one has no log service, so blocks are settled on the other's
// logs alone and are on record — in the watcher's checkpoint — as still to
// be scanned by a second log endpoint; the core's head says so (Checked).
// The checkpoint is then lost. That record goes with it, like with one that
// is unreadable: checkpoint_discarded is raised and says what was lost. A
// checkpoint lost while nothing waits for a second scan loses nothing the
// core does not have: no alert.
func TestAMissingCheckpointThatTookTheSecondScanRecordIsAlerted(t *testing.T) {
	for _, outstanding := range []bool{true, false} {
		t.Run(fmt.Sprintf("blocks waiting for a second scan %v", outstanding), func(t *testing.T) {
			w := newWorld(t, withNodes(2))
			if outstanding {
				w.nodes[1].Set(func(b *evmtest.Behavior) {
					b.LogsError = &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"}
				})
			}
			w.start()
			w.firstHead()
			p := w.env.CreatePayment("25")
			w.pay(p, 25)
			w.mine(testSettle + 2)
			if outstanding {
				w.writeOffLogs()
				w.eventually("blocks settled on one endpoint's logs", func() bool { return !w.coreHead().Checked.IsZero() })
			}
			w.synced()
			w.stop()
			w.store.set(nil)
			before := w.bridge.Status().Alerts[AlertCheckpointDiscarded]
			w.start()
			w.mine(1)
			if outstanding {
				w.writeOffLogs() // that, too, was on record in the checkpoint
			}
			w.synced()
			got := w.bridge.Status().Alerts[AlertCheckpointDiscarded] - before
			switch {
			case outstanding && got != 1:
				t.Fatalf("%d checkpoint_discarded alert(s) after the record of blocks to scan again was lost with the checkpoint", got)
			case !outstanding && got != 0:
				t.Fatalf("%d checkpoint_discarded alert(s) for a lost checkpoint that took nothing with it", got)
			}
			if outstanding && !strings.Contains(w.logs.String(), "no checkpoint: the record of the blocks settled on one endpoint's logs") {
				t.Fatal("the alert does not say what was lost")
			}
		})
	}
}
