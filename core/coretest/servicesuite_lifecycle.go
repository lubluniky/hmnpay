package coretest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// RunServiceSuite drives a complete [core.Service] — payment lifecycle,
// creation and idempotency, links, webhooks, authentication, setup,
// settings and read models — on top of the given store. It proves that the
// business rules hold end to end on that store, which the narrower
// [RunStoreSuite] cannot. newStore must return a new, empty store on every
// call.
func RunServiceSuite(t *testing.T, newStore func(t *testing.T) core.Store) {
	t.Helper()
	groups := []struct {
		name  string
		cases []serviceCase
	}{
		{"Lifecycle", lifecycleCases},
		{"Cancel", cancelCases},
		{"Payments", paymentCases},
		{"Links", slices.Concat(linkCases, linkDeleteCases)},
		{"Webhooks", webhookCases},
		{"Claims", claimCases},
		{"Access", slices.Concat(accessCases, sessionCases)},
		{"Instance", instanceCases},
		{"Security", securityCases},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			for _, c := range g.cases {
				t.Run(c.name, func(t *testing.T) {
					c.run(t, func(opts ...EnvOption) *Env {
						return NewEnv(t, append([]EnvOption{WithStore(newStore(t))}, opts...)...)
					})
				})
			}
		})
	}
}

// serviceCase is one scenario; newEnv builds a fresh service on a fresh
// store.
type serviceCase struct {
	name string
	run  func(t *testing.T, newEnv func(opts ...EnvOption) *Env)
}

// ---- assertions ----

func wantStatus(t *testing.T, e *Env, id string, want core.PaymentStatus) core.PaymentView {
	t.Helper()
	p := e.Payment(id)
	if p.Status != want {
		t.Fatalf("payment status = %s, want %s", p.Status, want)
	}
	return p
}

func wantEvents(t *testing.T, e *Env, id string, want ...core.EventType) {
	t.Helper()
	if got := e.EventTypes(id); !slices.Equal(got, want) {
		t.Fatalf("events:\n got  %v\n want %v", got, want)
	}
}

func wantAmounts(t *testing.T, p core.PaymentView, received, confirmed string) {
	t.Helper()
	if p.AmountReceived != received || p.AmountConfirmed != confirmed {
		t.Fatalf("amounts: received %s confirmed %s, want received %s confirmed %s", p.AmountReceived, p.AmountConfirmed, received, confirmed)
	}
}

func wantIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want one matching %v", err, target)
	}
}

func wantKind(t *testing.T, err error, kind core.Kind) {
	t.Helper()
	if core.KindOf(err) != kind {
		t.Fatalf("error = %v (kind %q), want kind %q", err, core.KindOf(err), kind)
	}
}

// wantField checks for a validation error on a field.
func wantField(t *testing.T, err error, field string) {
	t.Helper()
	wantKind(t, err, core.KindInvalid)
	for _, f := range core.FieldErrors(err) {
		if f.Field == field {
			return
		}
	}
	t.Fatalf("error %v has no field error for %q", err, field)
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func setTolerance(t *testing.T, e *Env, tolerance string) {
	t.Helper()
	_, err := e.Service.UpdatePaymentDefaults(e.Operator(), core.PaymentSettings{
		TTL: core.DefaultPaymentTTL, RequiredConfirmations: core.DefaultRequiredConfirmations, UnderpaymentTolerance: tolerance,
	})
	noErr(t, err)
}

// setPaymentDefaults sets the confirmation depth and the tolerance.
func setPaymentDefaults(t *testing.T, e *Env, confirmations int, tolerance string) {
	t.Helper()
	_, err := e.Service.UpdatePaymentDefaults(e.Operator(), core.PaymentSettings{
		TTL: core.DefaultPaymentTTL, RequiredConfirmations: confirmations, UnderpaymentTolerance: tolerance,
	})
	noErr(t, err)
}

const (
	created    = core.EventPaymentCreated
	awaiting   = core.EventPaymentAwaiting
	detected   = core.EventPaymentDetected
	confirming = core.EventPaymentConfirming
	paid       = core.EventPaymentPaid
	underpaid  = core.EventPaymentUnderpaid
	overpaid   = core.EventPaymentOverpaid
	expired    = core.EventPaymentExpired
	failed     = core.EventPaymentFailed
	lateFunds  = core.EventPaymentLateFunds
)

// Blocks used by the scenarios: with 15 required confirmations a transfer
// in block 100 becomes final when the head reaches block 114.
var lifecycleCases = []serviceCase{
	{"exact payment: awaiting → confirming → paid", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149.00")
		if p.Status != core.StatusAwaiting || p.Confirmations != 0 || p.AmountDue != "149.00" {
			t.Fatalf("new payment: %+v", p)
		}
		e.Head(99)
		tr := e.Pay(p, "149", 100)
		got := wantStatus(t, e, p.ID, core.StatusConfirming)
		wantAmounts(t, got, "149.00", "0.00")
		if got.Confirmations != 0 || got.TxHash == nil || *got.TxHash != tr.TxHash || *got.FromAddress != tr.From {
			t.Fatalf("after first sight: %+v", got)
		}
		e.Head(100)
		if c := e.Payment(p.ID).Confirmations; c != 1 {
			t.Fatalf("confirmations at the including block = %d, want 1", c)
		}
		e.Head(113)
		if got := wantStatus(t, e, p.ID, core.StatusConfirming); got.Confirmations != 14 {
			t.Fatalf("confirmations = %d, want 14", got.Confirmations)
		}
		e.Clock.Advance(40 * time.Second)
		e.Head(114)
		got = wantStatus(t, e, p.ID, core.StatusPaid)
		wantAmounts(t, got, "149.00", "149.00")
		if got.Confirmations != 15 || got.PaidAt == nil || !got.PaidAt.Equal(e.Clock.Now()) || got.SettledAt == nil || got.NeedsAttention || got.AmountDue != "0.00" {
			t.Fatalf("paid payment: %+v", got)
		}
		wantEvents(t, e, p.ID, created, confirming, paid)
	}},

	{"replayed and duplicated facts change nothing", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		tr := e.Pay(p, "149", 100)
		e.Observe(tr)
		e.Observe(tr)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusConfirming), "149.00", "0.00")
		e.Head(114)
		e.Head(114)
		e.Observe(tr)
		e.Head(120)
		e.Observe(tr)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusPaid), "149.00", "149.00")
		wantEvents(t, e, p.ID, created, confirming, paid)
	}},

	{"several transfers to one address are summed", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "100", 100)
		got := wantStatus(t, e, p.ID, core.StatusDetected)
		wantAmounts(t, got, "100.00", "0.00")
		if got.AmountDue != "49.00" {
			t.Fatalf("amount due = %s, want 49.00", got.AmountDue)
		}
		e.Pay(p, "48.5", 102)
		e.Pay(p, "0.5", 104)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusConfirming), "149.00", "0.00")
		e.Head(116) // first two final, third still pending
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusConfirming), "149.00", "148.50")
		e.Head(118)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusPaid), "149.00", "149.00")
		wantEvents(t, e, p.ID, created, detected, confirming, paid)
	}},

	{"two logs of one transaction are two transfers", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("100")
		a := e.Transfer(p, "60", 100)
		b := a
		b.LogIndex++
		b.Amount = core.MustParseAmount("40", 18)
		e.Observe(a)
		e.Observe(b)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusConfirming), "100.00", "0.00")
	}},

	{"underpayment within tolerance counts as paid", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		setTolerance(t, e, "0.50")
		p := e.CreatePayment("149")
		e.Pay(p, "148.50", 100)
		wantStatus(t, e, p.ID, core.StatusConfirming)
		e.Head(114)
		got := wantStatus(t, e, p.ID, core.StatusPaid)
		wantAmounts(t, got, "148.50", "148.50")
		if got.NeedsAttention {
			t.Fatal("a payment within tolerance must not need attention")
		}
		wantEvents(t, e, p.ID, created, confirming, paid)
	}},

	{"underpayment beyond tolerance settles as underpaid at the deadline", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		setTolerance(t, e, "0.50")
		p := e.CreatePayment("149")
		e.Pay(p, "148.49", 100)
		wantStatus(t, e, p.ID, core.StatusDetected)
		e.Head(114)
		// Confirmed but short: still open, the payer may top up.
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusDetected), "148.49", "148.49")
		e.Expire(p)
		e.Head(700)
		got := wantStatus(t, e, p.ID, core.StatusUnderpaid)
		if !got.NeedsAttention || got.Attention != core.AttentionUnderpaid || got.PaidAt != nil || got.SettledAt == nil || got.AmountDue != "0.51" {
			t.Fatalf("underpaid payment: %+v", got)
		}
		wantEvents(t, e, p.ID, created, detected, underpaid)
	}},

	{"the tolerance is snapshotted at creation", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		setTolerance(t, e, "5")
		e.Pay(p, "148", 100)
		wantStatus(t, e, p.ID, core.StatusDetected)
		q := e.CreatePayment("149")
		e.Pay(q, "148", 100)
		wantStatus(t, e, q.ID, core.StatusConfirming)
	}},

	{"a short payment that is topped up gets paid", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "100", 100)
		e.Head(120)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusDetected), "100.00", "100.00")
		e.Pay(p, "49", 121)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusConfirming), "149.00", "100.00")
		e.Head(135)
		wantStatus(t, e, p.ID, core.StatusPaid)
		wantEvents(t, e, p.ID, created, detected, confirming, paid)
	}},

	{"overpayment settles as overpaid and needs attention", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "149.000000000000000001", 100)
		wantStatus(t, e, p.ID, core.StatusConfirming)
		e.Head(114)
		got := wantStatus(t, e, p.ID, core.StatusOverpaid)
		if !got.NeedsAttention || got.Attention != core.AttentionOverpaid || got.PaidAt == nil {
			t.Fatalf("overpaid payment: %+v", got)
		}
		wantEvents(t, e, p.ID, created, confirming, overpaid)
	}},

	{"a top-up that overshoots settles as overpaid", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "100", 100)
		e.Pay(p, "100", 100)
		e.Head(114)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusOverpaid), "200.00", "200.00")
	}},

	{"nothing received: expires at the deadline, in chain time, after the grace", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Clock.Set(p.ExpiresAt.Add(-time.Second))
		e.Head(500)
		wantStatus(t, e, p.ID, core.StatusAwaiting)
		e.Clock.Set(p.ExpiresAt.Add(core.DefaultExpiryGrace - time.Second))
		e.Head(520)
		wantStatus(t, e, p.ID, core.StatusAwaiting)
		e.Clock.Set(p.ExpiresAt.Add(core.DefaultExpiryGrace))
		e.Head(521)
		got := wantStatus(t, e, p.ID, core.StatusExpired)
		if got.NeedsAttention || got.SettledAt == nil || got.PaidAt != nil {
			t.Fatalf("expired payment: %+v", got)
		}
		e.Head(522)
		wantEvents(t, e, p.ID, created, expired)
	}},

	{"wall-clock time alone never expires a payment", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		// The watcher is down: the clock runs far past the deadline, but no
		// head arrives, so nothing may be declared expired. When the watcher
		// catches up it first reports the transfer that was mined in time.
		e := newEnv()
		p := e.CreatePayment("149")
		inTime := p.ExpiresAt.Add(-10 * time.Minute)
		e.Clock.Set(inTime)
		transfer := e.Transfer(p, "149", 100) // mined in time
		e.Clock.Advance(48 * time.Hour)
		wantStatus(t, e, p.ID, core.StatusAwaiting)
		e.Observe(transfer)
		noErr(t, e.Service.AdvanceHead(e.System(), core.ChainHead{ChainID: 56, Number: 100, Hash: BlockHash(100, 0), Time: inTime, Final: 100}))
		wantStatus(t, e, p.ID, core.StatusConfirming)
		e.Head(200)
		wantStatus(t, e, p.ID, core.StatusPaid)
		wantEvents(t, e, p.ID, created, confirming, paid)
	}},

	{"funds arriving after expiry are recorded and flagged, the status stays", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Expire(p)
		e.Head(700)
		wantStatus(t, e, p.ID, core.StatusExpired)

		e.Pay(p, "149", 800)
		got := wantStatus(t, e, p.ID, core.StatusExpired)
		wantAmounts(t, got, "149.00", "0.00")
		if got.NeedsAttention {
			t.Fatal("unconfirmed late funds must not raise attention yet")
		}
		e.Head(814)
		got = wantStatus(t, e, p.ID, core.StatusExpired)
		wantAmounts(t, got, "149.00", "149.00")
		if !got.NeedsAttention || got.Attention != core.AttentionLateFunds {
			t.Fatalf("late funds must need attention: %+v", got)
		}
		wantEvents(t, e, p.ID, created, expired, lateFunds)

		// The operator deals with it …
		resolved, err := e.Service.ResolvePayment(e.Operator(), p.ID, "Refunded to sender.")
		noErr(t, err)
		if resolved.NeedsAttention || resolved.Resolution == nil || resolved.Resolution.By != e.Email || resolved.Resolution.Note != "Refunded to sender." {
			t.Fatalf("resolved payment: %+v", resolved)
		}
		// … and more money shows up: the issue is open again.
		e.Pay(p, "10", 900)
		e.Head(914)
		got = wantStatus(t, e, p.ID, core.StatusExpired)
		wantAmounts(t, got, "159.00", "159.00")
		if !got.NeedsAttention || got.Resolution != nil {
			t.Fatalf("new late funds must reopen the issue: %+v", got)
		}
		wantEvents(t, e, p.ID, created, expired, lateFunds, core.EventPaymentResolved, lateFunds)
	}},

	{"a payment mid-confirmation does not expire under the payer's feet", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Clock.Set(p.ExpiresAt.Add(-5 * time.Second))
		e.Pay(p, "149", 100)
		e.Head(100)
		e.Expire(p)
		e.Head(105)
		wantStatus(t, e, p.ID, core.StatusConfirming)
		e.Clock.Advance(time.Hour)
		e.Head(113)
		wantStatus(t, e, p.ID, core.StatusConfirming)
		e.Head(114)
		wantStatus(t, e, p.ID, core.StatusPaid)
		wantEvents(t, e, p.ID, created, confirming, paid)
	}},

	{"a partial payment pending at the deadline settles once it confirms", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "100", 100)
		e.Expire(p)
		e.Head(105)
		wantStatus(t, e, p.ID, core.StatusDetected) // held open by the pending transfer
		// A top-up now is too late: it neither counts nor keeps the
		// payment open any longer.
		e.Pay(p, "20", 106)
		e.Head(113)
		wantStatus(t, e, p.ID, core.StatusDetected) // the 100 is one block short of final
		e.Head(114)
		got := wantStatus(t, e, p.ID, core.StatusUnderpaid)
		wantAmounts(t, got, "120.00", "100.00")
		e.Head(120)
		got = wantStatus(t, e, p.ID, core.StatusUnderpaid)
		wantAmounts(t, got, "120.00", "120.00")
		if got.Attention != core.AttentionUnderpaid || !got.NeedsAttention {
			t.Fatalf("underpaid with a late top-up: %+v", got)
		}
		wantEvents(t, e, p.ID, created, detected, underpaid, lateFunds)
	}},

	{"a top-up mined in time counts even when it is reported after the deadline", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "100", 100)
		topUp := e.Transfer(p, "49", 101) // mined now, reported late
		e.Expire(p)
		e.Observe(topUp)
		e.Head(115)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusPaid), "149.00", "149.00")
		wantEvents(t, e, p.ID, created, detected, confirming, paid)
	}},

	{"reorg before the deadline: back to awaiting, then paid when re-included", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		tr := e.Pay(p, "149", 100)
		e.Head(101)
		wantStatus(t, e, p.ID, core.StatusConfirming)
		e.Retract(tr)
		got := wantStatus(t, e, p.ID, core.StatusAwaiting)
		wantAmounts(t, got, "0.00", "0.00")
		if got.TxHash != nil || got.Confirmations != 0 {
			t.Fatalf("after reorg: %+v", got)
		}
		e.Retract(tr) // replay
		// The same transaction is mined again in another block.
		again := tr
		again.BlockNumber, again.BlockHash, again.LogIndex = 102, BlockHash(102, 1), tr.LogIndex+3
		e.Observe(again)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusConfirming), "149.00", "0.00")
		e.Head(116)
		wantStatus(t, e, p.ID, core.StatusPaid)
		wantEvents(t, e, p.ID, created, confirming, awaiting, confirming, paid)

		d, err := e.Service.GetPaymentDetail(e.System(), p.ID)
		noErr(t, err)
		if len(d.Transfers) != 2 || d.Transfers[0].Status != core.TransferOrphaned || d.Transfers[1].Status != core.TransferConfirmed {
			t.Fatalf("transfers after reorg: %+v", d.Transfers)
		}
	}},

	{"re-inclusion without a retraction is never counted twice", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		tr := e.Pay(p, "149", 100)
		again := tr
		again.BlockNumber, again.BlockHash = 101, BlockHash(101, 1)
		e.Observe(again)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusConfirming), "149.00", "0.00")
		// Reporting the stale one again revives it and orphans the other:
		// whichever block the watcher reported last is the canonical one.
		e.Observe(tr)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusConfirming), "149.00", "0.00")
		e.Head(114)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusPaid), "149.00", "149.00")
		wantEvents(t, e, p.ID, created, confirming, paid)
	}},

	{"a retracted transfer comes back when its block is canonical again", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		tr := e.Pay(p, "149", 100)
		e.Retract(tr)
		wantStatus(t, e, p.ID, core.StatusAwaiting)
		e.Observe(tr)
		wantStatus(t, e, p.ID, core.StatusConfirming)
		e.Head(114)
		wantStatus(t, e, p.ID, core.StatusPaid)
	}},

	{"reorg of one of two transfers: confirming → detected", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "100", 100)
		second := e.Pay(p, "49", 101)
		wantStatus(t, e, p.ID, core.StatusConfirming)
		e.Retract(second)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusDetected), "100.00", "0.00")
		wantEvents(t, e, p.ID, created, detected, confirming, detected)
	}},

	{"reorg after the deadline with no re-inclusion: failed", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		tr := e.Pay(p, "149", 100)
		e.Expire(p)
		e.Head(102)
		wantStatus(t, e, p.ID, core.StatusConfirming)
		e.Retract(tr)
		// Not settled by the retraction itself: the watcher has not finished
		// rescanning, the transaction may be re-included.
		wantStatus(t, e, p.ID, core.StatusAwaiting)
		e.Head(103)
		got := wantStatus(t, e, p.ID, core.StatusFailed)
		if !got.NeedsAttention || got.Attention != core.AttentionFailed {
			t.Fatalf("failed payment: %+v", got)
		}
		wantEvents(t, e, p.ID, created, confirming, awaiting, failed)

		// If it is mined after all, that is late funds on a failed payment.
		again := tr
		again.BlockNumber, again.BlockHash = 110, BlockHash(110, 0)
		e.Observe(again)
		e.Head(124)
		got = wantStatus(t, e, p.ID, core.StatusFailed)
		wantAmounts(t, got, "149.00", "149.00")
		wantEvents(t, e, p.ID, created, confirming, awaiting, failed, lateFunds)
	}},

	{"reorg after the deadline, re-included before the next head: paid", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		tr := e.Pay(p, "149", 100)
		e.Expire(p)
		e.Head(102)
		e.Retract(tr)
		again := tr
		again.BlockNumber, again.BlockHash = 103, BlockHash(103, 1)
		e.Observe(again)
		e.Head(103)
		wantStatus(t, e, p.ID, core.StatusConfirming)
		e.Head(117)
		wantStatus(t, e, p.ID, core.StatusPaid)
	}},

	{"confirmed is final: later contradictions are refused", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		tr := e.Pay(p, "149", 100)
		e.Head(114)
		wantStatus(t, e, p.ID, core.StatusPaid)
		key := core.TransferKey{TxHash: tr.TxHash, LogIndex: tr.LogIndex, BlockHash: tr.BlockHash}
		wantIs(t, e.Service.RetractTransfer(e.System(), key), core.ErrTransferFinal)
		elsewhere := tr
		elsewhere.BlockNumber, elsewhere.BlockHash = 101, BlockHash(101, 7)
		wantIs(t, e.Service.ObserveTransfer(e.System(), elsewhere), core.ErrTransferFinal)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusPaid), "149.00", "149.00")
		wantEvents(t, e, p.ID, created, confirming, paid)
		// A head that moves backwards un-confirms nothing.
		e.Head(90)
		got := wantStatus(t, e, p.ID, core.StatusPaid)
		if got.Confirmations != 15 {
			t.Fatalf("confirmations of a final payment = %d, want 15", got.Confirmations)
		}
	}},

	{"a head that moves backwards only lowers pending confirmations", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "149", 100)
		e.Head(110)
		if c := e.Payment(p.ID).Confirmations; c != 11 {
			t.Fatalf("confirmations = %d, want 11", c)
		}
		e.Head(104)
		if got := wantStatus(t, e, p.ID, core.StatusConfirming); got.Confirmations != 5 {
			t.Fatalf("confirmations = %d, want 5", got.Confirmations)
		}
		e.Head(98)
		if got := wantStatus(t, e, p.ID, core.StatusConfirming); got.Confirmations != 0 {
			t.Fatalf("confirmations = %d, want 0", got.Confirmations)
		}
		e.Head(114)
		wantStatus(t, e, p.ID, core.StatusPaid)
	}},

	{"out-of-order facts: a transfer reported under an old head settles at once", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		e.Head(500)
		p := e.CreatePayment("149")
		e.Head(540)
		e.Pay(p, "149", 510) // discovered late, e.g. by a back-scan
		wantStatus(t, e, p.ID, core.StatusPaid)
		wantEvents(t, e, p.ID, created, paid)
	}},

	{"transfers reported in reverse order converge", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "49", 105)
		e.Pay(p, "100", 100)
		got := wantStatus(t, e, p.ID, core.StatusConfirming)
		d, err := e.Service.GetPaymentDetail(e.System(), p.ID)
		noErr(t, err)
		if *got.TxHash != d.Transfers[0].TxHash || d.Transfers[0].BlockNumber != 100 {
			t.Fatalf("the payment's tx hash must be the earliest transfer: %+v", d.Transfers)
		}
		e.Head(119)
		wantStatus(t, e, p.ID, core.StatusPaid)
	}},

	{"a second payment to a paid invoice is late funds", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "149", 100)
		e.Head(114)
		e.Pay(p, "149", 200)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusPaid), "298.00", "149.00")
		e.Head(214)
		got := wantStatus(t, e, p.ID, core.StatusPaid)
		wantAmounts(t, got, "298.00", "298.00")
		if !got.NeedsAttention || got.Attention != core.AttentionLateFunds {
			t.Fatalf("double payment must need attention: %+v", got)
		}
		wantEvents(t, e, p.ID, created, confirming, paid, lateFunds)
	}},

	{"settlement does not wait for an extra pending transfer", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "149", 100)
		e.Pay(p, "5", 110)
		e.Head(114)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusPaid), "154.00", "149.00")
		// The straggler confirms later: late funds, the status stays paid.
		e.Head(124)
		got := wantStatus(t, e, p.ID, core.StatusPaid)
		if !got.NeedsAttention {
			t.Fatalf("extra funds confirmed after settlement must need attention: %+v", got)
		}
		wantEvents(t, e, p.ID, created, confirming, paid, lateFunds)
	}},

	{"dust on top of a paid payment is recorded and not flagged", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "149", 100)
		e.Head(114)
		wantStatus(t, e, p.ID, core.StatusPaid)
		e.Pay(p, "0.01", 115)
		e.Head(130)
		got := wantStatus(t, e, p.ID, core.StatusPaid)
		wantAmounts(t, got, "149.01", "149.01")
		if got.NeedsAttention || got.Attention != "" {
			t.Fatalf("a cent on top of a paid invoice needs nobody's attention: %+v", got)
		}
		wantEvents(t, e, p.ID, created, confirming, paid)
	}},

	{"an extra transfer that is reorganised out after settlement leaves no issue", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		e.Pay(p, "149", 100)
		extra := e.Pay(p, "5", 110)
		e.Head(114)
		e.Retract(extra)
		got := wantStatus(t, e, p.ID, core.StatusPaid)
		wantAmounts(t, got, "149.00", "149.00")
		if got.NeedsAttention {
			t.Fatalf("no issue expected: %+v", got)
		}
		wantEvents(t, e, p.ID, created, confirming, paid)
	}},

	{"facts for the wrong chain, token or address are rejected", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		good := e.Transfer(p, "149", 100)

		wrongToken := good
		wrongToken.Contract = Address("some other token")
		wantIs(t, e.Service.ObserveTransfer(e.System(), wrongToken), core.ErrChainMismatch)
		wrongChain := good
		wrongChain.ChainID = core.ChainBSCTestnet
		wantIs(t, e.Service.ObserveTransfer(e.System(), wrongChain), core.ErrChainMismatch)
		stranger := good
		stranger.To = Address("not a deposit address")
		wantIs(t, e.Service.ObserveTransfer(e.System(), stranger), core.ErrUnknownAddress)
		zero := good
		zero.Amount = core.Amount{}
		noErr(t, e.Service.ObserveTransfer(e.System(), zero))
		malformed := good
		malformed.TxHash = "0x1234"
		wantField(t, e.Service.ObserveTransfer(e.System(), malformed), "tx_hash")
		wantIs(t, e.Service.AdvanceHead(e.System(), core.ChainHead{ChainID: 97, Number: 5, Hash: BlockHash(5, 0), Time: e.Clock.Now(), Final: 5}), core.ErrChainMismatch)
		wantField(t, e.Service.AdvanceHead(e.System(), core.ChainHead{ChainID: 56, Number: 5, Hash: BlockHash(5, 0), Final: 5}), "time")

		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusAwaiting), "0.00", "0.00")
		wantEvents(t, e, p.ID, created)

		// Addresses and hashes are matched case-insensitively.
		shouting := good
		shouting.To = "0x" + upper(good.To[2:])
		shouting.TxHash = "0X" + upper(good.TxHash[2:])
		e.Observe(shouting)
		e.Observe(good)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusConfirming), "149.00", "0.00")
	}},

	{"resolve and reopen", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		_, err := e.Service.ResolvePayment(e.Operator(), p.ID, "")
		wantIs(t, err, core.ErrNothingToResolve)
		e.Pay(p, "150", 100)
		e.Head(114)
		wantStatus(t, e, p.ID, core.StatusOverpaid)
		_, err = e.Service.ReopenPayment(e.Operator(), p.ID)
		wantIs(t, err, core.ErrNothingToResolve)

		got, err := e.Service.ResolvePayment(e.Operator(), p.ID, "  Kept as a tip.  ")
		noErr(t, err)
		if got.Status != core.StatusOverpaid || got.NeedsAttention || got.Attention != core.AttentionOverpaid || got.Resolution.Note != "Kept as a tip." {
			t.Fatalf("resolved: %+v", got)
		}
		_, err = e.Service.ResolvePayment(e.Operator(), p.ID, "again")
		wantIs(t, err, core.ErrNothingToResolve)
		got, err = e.Service.ReopenPayment(e.Operator(), p.ID)
		noErr(t, err)
		if !got.NeedsAttention || got.Resolution != nil {
			t.Fatalf("reopened: %+v", got)
		}
		wantEvents(t, e, p.ID, created, confirming, overpaid, core.EventPaymentResolved, core.EventPaymentReopened)
		_, err = e.Service.ResolvePayment(e.Operator(), "pay_missing", "")
		wantIs(t, err, core.ErrNotFound)
	}},

	{"the watch list covers open payments and recently settled ones", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		e.Head(1000)
		open := e.CreatePayment("10")
		e.Clock.Advance(time.Second)
		stale := e.CreatePayment("20")
		pending := e.Pay(open, "10", 1001)

		state, err := e.Service.ChainState(e.System())
		noErr(t, err)
		if !state.SetupCompleted || state.Asset != core.USDTBSC || state.Head.Number != 1000 || state.RequiredConfirmations != 15 || len(state.Network.RPCURLs) != 1 {
			t.Fatalf("chain state: %+v", state)
		}
		if len(state.Addresses) != 2 || state.Addresses[0].Address != open.Address || state.Addresses[0].SinceBlock != 1000 || state.Addresses[0].PaymentID != open.ID {
			t.Fatalf("watched addresses: %+v", state.Addresses)
		}
		if len(state.PendingTransfers) != 1 || state.PendingTransfers[0].TxHash != pending.TxHash {
			t.Fatalf("pending transfers: %+v", state.PendingTransfers)
		}

		e.Expire(stale)
		e.Head(1015)
		wantStatus(t, e, open.ID, core.StatusPaid)
		wantStatus(t, e, stale.ID, core.StatusExpired)
		state, err = e.Service.ChainState(e.System())
		noErr(t, err)
		if len(state.Addresses) != 2 || len(state.PendingTransfers) != 0 {
			t.Fatalf("settled payments stay watched for the watch window: %+v", state)
		}
		e.Clock.Advance(core.DefaultWatchWindow + time.Hour)
		e.Head(1100) // the processed chain is past the window as well
		state, err = e.Service.ChainState(e.System())
		noErr(t, err)
		if len(state.Addresses) != 0 {
			t.Fatalf("watch window is over, still watching: %+v", state.Addresses)
		}
		// An open payment is watched no matter how old it is.
		forgotten := e.CreatePayment("30")
		e.Clock.Advance(90 * 24 * time.Hour)
		state, err = e.Service.ChainState(e.System())
		noErr(t, err)
		if len(state.Addresses) != 1 || state.Addresses[0].PaymentID != forgotten.ID {
			t.Fatalf("open payment must be watched: %+v", state.Addresses)
		}
	}},

	// What the chain adapter may look for less often: the addresses of
	// payments that are settled and have nothing pending. Every open status
	// keeps an address — and with it the watch list — active; so does a
	// transfer that is not final, on whatever payment.
	{"an address is closed once its payment is settled and nothing on it is pending", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		e.Head(1000)
		// state returns which payments' addresses are listed as closed, and
		// the chain state they were read from.
		state := func() (map[string]bool, core.ChainState) {
			t.Helper()
			cs, err := e.Service.ChainState(e.System())
			noErr(t, err)
			closed := make(map[string]bool, len(cs.Addresses))
			for _, a := range cs.Addresses {
				closed[a.PaymentID] = a.Closed
			}
			return closed, cs
		}
		want := func(what string, wantClosed []core.PaymentView, wantOpen []core.PaymentView, active bool) {
			t.Helper()
			closed, cs := state()
			for _, p := range wantClosed {
				if c, listed := closed[p.ID]; !listed || !c {
					t.Fatalf("%s: the address of the %s payment %s must be listed as closed (listed=%v closed=%v)", what, e.Payment(p.ID).Status, p.Amount, listed, c)
				}
			}
			for _, p := range wantOpen {
				if c, listed := closed[p.ID]; !listed || c {
					t.Fatalf("%s: the address of the %s payment %s must be listed and not closed (listed=%v closed=%v)", what, e.Payment(p.ID).Status, p.Amount, listed, c)
				}
			}
			if cs.Active() != active {
				t.Fatalf("%s: chain state active = %v, want %v (%d addresses, %d pending transfers)", what, cs.Active(), active, len(cs.Addresses), len(cs.PendingTransfers))
			}
		}
		if _, cs := state(); cs.Active() || len(cs.Addresses) != 0 {
			t.Fatalf("an instance without payments: %+v", cs)
		}

		awaiting := e.CreatePayment("10")
		short := e.CreatePayment("20")
		full := e.CreatePayment("30")
		over := e.CreatePayment("40")
		orphaned := e.CreatePayment("50")
		unpaid := e.CreatePayment("60")
		all := []core.PaymentView{awaiting, short, full, over, orphaned, unpaid}
		want("awaiting", nil, all, true)

		// Open statuses: detected, confirming.
		e.Pay(short, "5", 1001)
		e.Pay(full, "30", 1001)
		e.Pay(over, "45", 1001)
		gone := e.Pay(orphaned, "50", 1001)
		wantStatus(t, e, short.ID, core.StatusDetected)
		wantStatus(t, e, full.ID, core.StatusConfirming)
		want("detected and confirming", nil, all, true)
		e.Retract(gone)
		wantStatus(t, e, orphaned.ID, core.StatusAwaiting)

		// Paid and overpaid, confirmed: closed. A partial payment that is
		// confirmed is still open — the payer may top up.
		e.Head(1015)
		wantStatus(t, e, full.ID, core.StatusPaid)
		wantStatus(t, e, over.ID, core.StatusOverpaid)
		wantStatus(t, e, short.ID, core.StatusDetected)
		want("paid and overpaid", []core.PaymentView{full, over}, []core.PaymentView{awaiting, short, orphaned, unpaid}, true)

		// Late funds on the paid one: while their transfer is pending the
		// address is not closed — its confirmations are counted, it can be
		// retracted. Final, it is closed again, and the payment is flagged.
		e.Pay(full, "3", 1016)
		want("a pending transfer on a paid payment", []core.PaymentView{over}, []core.PaymentView{full, awaiting}, true)
		e.Head(1030)
		if p := e.Payment(full.ID); p.Status != core.StatusPaid || !p.NeedsAttention || p.Attention != core.AttentionLateFunds {
			t.Fatalf("late funds on a paid payment: %+v", p)
		}
		want("late funds, flagged and final", []core.PaymentView{full, over}, []core.PaymentView{awaiting}, true)
		// And a late transfer that is reorganised out before it is final.
		again := e.Pay(full, "2", 1031)
		want("another pending transfer", nil, []core.PaymentView{full}, true)
		e.Retract(again)
		want("the transfer was retracted", []core.PaymentView{full, over}, nil, true)

		// The operator's bookkeeping changes nothing about the chain.
		_, err := e.Service.ResolvePayment(e.Operator(), over.ID, "refunded the difference")
		noErr(t, err)
		want("resolved", []core.PaymentView{full, over}, nil, true)
		_, err = e.Service.ReopenPayment(e.Operator(), over.ID)
		noErr(t, err)
		want("resolution reopened", []core.PaymentView{full, over}, nil, true)

		// Cancelled: expired, closed.
		_, err = e.Service.CancelPayment(e.Operator(), awaiting.ID)
		noErr(t, err)
		want("cancelled", []core.PaymentView{awaiting, full, over}, []core.PaymentView{short, orphaned, unpaid}, true)

		// The deadline passes: underpaid, failed, expired — all closed, and
		// the watch list, which still holds all six, is not active.
		e.Expire(unpaid)
		e.Head(1040)
		wantStatus(t, e, short.ID, core.StatusUnderpaid)
		wantStatus(t, e, orphaned.ID, core.StatusFailed)
		wantStatus(t, e, unpaid.ID, core.StatusExpired)
		want("everything is settled", all, nil, false)
		if _, cs := state(); len(cs.Addresses) != len(all) || len(cs.PendingTransfers) != 0 {
			t.Fatalf("settled payments stay on the watch list for their window: %+v", cs)
		}

		// A top-up of the underpaid one does not reopen it: late funds,
		// pending — which makes the list active — and then final.
		e.Pay(short, "15", 1041)
		wantStatus(t, e, short.ID, core.StatusUnderpaid)
		want("a late top-up, pending", []core.PaymentView{full, over, awaiting, orphaned, unpaid}, []core.PaymentView{short}, true)
		e.Head(1055)
		if p := e.Payment(short.ID); p.Status != core.StatusUnderpaid || p.AmountConfirmed != "20.00" {
			t.Fatalf("a late top-up of an underpaid payment: %+v", p)
		}
		want("a late top-up, final", all, nil, false)

		// A payment is created: active again.
		fresh := e.CreatePayment("70")
		want("a new payment", all, []core.PaymentView{fresh}, true)
	}},
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}
