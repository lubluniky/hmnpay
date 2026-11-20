package corebridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// The core dates a transfer by the timestamp of its block: a transfer in a
// block at or after the payment's deadline, first seen at or after the
// deadline, is late — it never completes the payment or counts as a use of
// a limited link. These tests run the real core, bridge, watcher and RPC
// client; what they check is that every transfer reaches the core with the
// timestamp of its block, that the timestamp is one two endpoints agree on,
// and what the core then makes of it.

// deadlineOf is the instant from which the core counts a transfer as late.
func deadlineOf(p core.PaymentView) time.Time { return p.ExpiresAt.Add(core.DefaultExpiryGrace) }

// transferTimes returns the block times the core has recorded for a
// payment's live transfers.
func (w *world) transferTimes(p core.PaymentView) []time.Time {
	w.t.Helper()
	d, err := w.env.Service.GetPaymentDetail(w.env.Operator(), p.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []time.Time
	for _, tr := range d.Transfers {
		if tr.Status == core.TransferOrphaned {
			continue
		}
		if tr.BlockTime == nil {
			out = append(out, time.Time{})
		} else {
			out = append(out, *tr.BlockTime)
		}
	}
	return out
}

// shiftTimestamps makes a node add `shift` seconds to the timestamp of every
// block it is asked for by number; everything else it answers truthfully.
func shiftTimestamps(w *world, shift int64) func(b *evmtest.Behavior) {
	return func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			n, ok := blockNumberParam(params)
			if !ok {
				return nil, nil, false
			}
			blk, ok := w.chain.BlockByNumber(n)
			if !ok {
				return nil, nil, false
			}
			return map[string]any{
				"number": hexQuantity(blk.Number), "hash": blk.Hash.String(), "parentHash": blk.ParentHash.String(),
				"timestamp": hexQuantity(uint64(int64(blk.Time) + shift)), "transactions": []string{},
			}, nil, true
		}
	}
}

// inTimeSeenLate sets up a payment that is paid one minute before its
// deadline while the watcher is not running; the watcher comes back an hour
// later. The core first sees the transfer long after the deadline, and only
// the block's timestamp shows that it was in time.
func inTimeSeenLate(t *testing.T, nodes int) (*world, core.PaymentView, time.Time) {
	w := newWorld(t, withNodes(nodes))
	w.start()
	w.synced()
	w.stop()
	p := w.env.CreatePayment("25")
	w.advance(p.ExpiresAt.Sub(w.env.Clock.Now()) - time.Minute)
	w.pay(p, 25)
	paidAt := w.env.Clock.Now()
	w.mine(1)
	w.advance(time.Hour)
	w.mine(testSettle + testConfirmations + 2)
	return w, p, paidAt
}

// (1) A payment made in time is paid, however late the watcher gets to it —
// and the core has the block's own timestamp on record.
func TestBlockTime_InTimePaymentSeenAfterTheDeadlineIsPaid(t *testing.T) {
	w, p, paidAt := inTimeSeenLate(t, 2)
	w.start()
	got := w.waitStatus(p, core.StatusPaid)
	if got.NeedsAttention {
		t.Fatalf("payment = %+v", got)
	}
	if times := w.transferTimes(p); len(times) != 1 || !times[0].Equal(paidAt.Truncate(time.Second)) {
		t.Fatalf("block times recorded by the core = %v, the block was mined at %s", times, paidAt)
	}
}

// lateBelowTheHead sets up, on a limited link, the case the core cannot date
// by itself: a transfer in a block that is past the payment's deadline and
// at or below the head the core has already processed. (The chain's clock
// runs 90 seconds ahead of the server's — within tolerance — so the head
// passes the deadline while the server clock has not, and the payment is
// still open; the only endpoint hides the transfer until asked again.)
func lateBelowTheHead(t *testing.T) (*world, core.PaymentView, core.LinkView, uint64) {
	w := newWorld(t)
	w.start()
	w.synced()
	link, err := w.env.Service.CreateLink(w.env.Operator(), core.LinkInput{Name: "Early bird", Amount: "25", MaxUses: 1, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	co, _, err := w.env.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{})
	if err != nil {
		t.Fatal(err)
	}
	p := w.env.Payment(co.ID)
	deadline := deadlineOf(p)

	w.advance(deadline.Sub(w.env.Clock.Now()) - 80*time.Second)
	w.chain.SetTime(deadline.Add(10 * time.Second))
	w.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } })
	w.pay(p, 25)
	paidIn := w.chain.Mine(1).Number
	w.chain.Mine(testSettle + testConfirmations + 2)
	w.eventually("the head to pass the hidden transfer's block", func() bool { return w.coreHead().Number >= paidIn+testConfirmations })
	if got := w.status(p); got != core.StatusAwaiting {
		t.Fatalf("test setup: payment = %s, want it still open", got)
	}
	// The server clock passes the deadline; no block is mined, so the core
	// is given no new head and the payment stays open.
	w.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = nil })
	w.advance(3 * time.Minute)
	return w, p, link, paidIn
}

// (2) A transfer mined after the deadline and reported by a re-scan, below
// the head the core has processed, is late: it does not complete the
// payment, and the limited link keeps its use.
func TestBlockTime_LateTransferFoundByARescanIsLateFunds(t *testing.T) {
	w, p, link, paidIn := lateBelowTheHead(t)
	if !w.bridge.Rescan(paidIn) {
		t.Fatal("no watcher running")
	}
	w.eventually("the transfer to reach the core", func() bool { return len(w.transferTimes(p)) == 1 })
	if times := w.transferTimes(p); !times[0].Equal(deadlineOf(p).Add(10 * time.Second)) {
		t.Fatalf("block time recorded by the core = %v, the block was mined at %s", times, deadlineOf(p).Add(10*time.Second))
	}
	w.never("a transfer mined after the deadline completes the payment", func() bool {
		s := w.status(p)
		return s == core.StatusPaid || s == core.StatusConfirming || s == core.StatusOverpaid
	})
	// The next head settles it: expired, with late funds for the operator
	// to look at.
	w.mine(1)
	got := w.waitStatus(p, core.StatusExpired)
	if got.Attention != core.AttentionLateFunds || !got.NeedsAttention {
		t.Fatalf("payment = %+v", got)
	}
	lv, err := w.env.Service.GetLink(w.env.Operator(), link.ID)
	if err != nil || lv.Uses != 0 || lv.Status != core.LinkActive {
		t.Fatalf("link = %+v, %v: late funds must not count as a use", lv, err)
	}
}

// reincludedLate sets up a payment whose transfer is mined a minute before
// the deadline and seen by the core; then its block is reorganised away and
// the transaction is mined again, in a block 30 seconds after the deadline.
func reincludedLate(t *testing.T, nodes int, lie func(w *world)) (*world, core.PaymentView) {
	w := newWorld(t, withNodes(nodes))
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	deadline := deadlineOf(p)
	w.advance(deadline.Sub(w.env.Clock.Now()) - time.Minute)
	w.pay(p, 25)
	w.mine(1)
	w.eventually("the transfer to be seen in time", func() bool { return w.status(p) == core.StatusConfirming })

	if lie != nil {
		lie(w)
	}
	dropped := w.chain.Reorg(1)
	w.chain.Requeue(dropped...)
	w.advance(90 * time.Second)
	w.mine(testSettle + testConfirmations + 2)
	return w, p
}

// (3) The same transaction, mined again after a reorganisation in a block
// that is past the deadline, is late.
func TestBlockTime_ReinclusionAfterTheDeadlineIsLateFunds(t *testing.T) {
	w, p := reincludedLate(t, 2, nil)
	got := w.waitStatus(p, core.StatusExpired)
	if got.Attention != core.AttentionLateFunds || !got.NeedsAttention {
		t.Fatalf("payment = %+v (events %s)", got, w.events(p))
	}
	if times := w.transferTimes(p); len(times) != 1 || !times[0].Equal(deadlineOf(p).Add(30*time.Second)) {
		t.Fatalf("block times of the live transfer = %v, want the time of the block it was mined in again", times)
	}
	if strings.Contains(w.events(p), "paid") {
		t.Fatalf("events = %s", w.events(p))
	}
}

// (4) With two endpoints, one of them cannot move a transfer across the
// deadline by lying about the timestamp of its block — neither forwards (an
// in-time payment turned into late funds) nor backwards (late funds turned
// into a payment). While the two disagree the transfer is not reported at
// all; once the lie ends it is reported with the block's real time.
func TestBlockTime_OneEndpointCannotMoveATransferAcrossTheDeadline(t *testing.T) {
	for liar := 0; liar < 2; liar++ {
		t.Run(fmt.Sprintf("forward-dating, liar is endpoint %d", liar), func(t *testing.T) {
			w, p, paidAt := inTimeSeenLate(t, 2)
			w.nodes[liar].Set(shiftTimestamps(w, 2*3600))
			w.start()
			w.eventually("the watcher to hold the transfer back", func() bool {
				st := w.bridge.Status()
				return st.Watcher.Hold != "" && st.Alerts[AlertEndpointFault] > 0
			})
			w.never("a transfer is reported with a timestamp only one endpoint vouches for", func() bool {
				return len(w.transferTimes(p)) != 0 || w.status(p) != core.StatusAwaiting
			})
			w.nodes[liar].Set(func(b *evmtest.Behavior) { b.Intercept = nil })
			w.passCooldowns()
			if got := w.waitStatus(p, core.StatusPaid); got.NeedsAttention {
				t.Fatalf("payment = %+v", got)
			}
			if times := w.transferTimes(p); len(times) != 1 || !times[0].Equal(paidAt.Truncate(time.Second)) {
				t.Fatalf("block times = %v, want %s", times, paidAt)
			}
		})
		t.Run(fmt.Sprintf("back-dating, liar is endpoint %d", liar), func(t *testing.T) {
			w, p := reincludedLate(t, 2, func(w *world) { w.nodes[liar].Set(shiftTimestamps(w, -3600)) })
			w.eventually("the watcher to hold the transfer back", func() bool {
				st := w.bridge.Status()
				return st.Watcher.Hold != "" && st.Alerts[AlertEndpointFault] > 0
			})
			w.never("late funds complete a payment on one endpoint's timestamp", func() bool {
				s := w.status(p)
				return s == core.StatusPaid || s == core.StatusOverpaid
			})
			w.nodes[liar].Set(func(b *evmtest.Behavior) { b.Intercept = nil })
			w.passCooldowns()
			got := w.waitStatus(p, core.StatusExpired)
			if got.Attention != core.AttentionLateFunds {
				t.Fatalf("payment = %+v (events %s)", got, w.events(p))
			}
		})
	}
}
