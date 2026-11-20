package coretest

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// cancelled asserts that the payment is expired-by-cancellation.
func wantCancelled(t *testing.T, e *Env, id string) core.PaymentView {
	t.Helper()
	p := wantStatus(t, e, id, core.StatusExpired)
	if p.CancelledAt == nil || p.SettledAt == nil || p.PaidAt != nil {
		t.Fatalf("cancelled payment: %+v", p)
	}
	return p
}

var cancelCases = []serviceCase{
	{"cancel an awaiting payment: expired with cancelled_at, payment.expired", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		writer, _ := e.APIKey(core.ScopePaymentsWrite)
		p := e.CreatePayment("149")
		e.Head(50)
		e.Clock.Advance(time.Minute)
		got, err := e.Service.CancelPayment(writer, p.ID)
		noErr(t, err)
		if got.Status != core.StatusExpired || got.CancelledAt == nil || !got.CancelledAt.Equal(e.Clock.Now()) || got.CancelledBy != "test key" ||
			got.SettledAt == nil || !got.SettledAt.Equal(e.Clock.Now()) || got.PaidAt != nil || got.NeedsAttention || got.AmountReceived != "0.00" {
			t.Fatalf("cancelled payment: %+v", got)
		}
		wantEvents(t, e, p.ID, created, expired)
		events, err := e.Service.Events(e.System(), core.EventFilter{PaymentID: p.ID, Types: []core.EventType{expired}})
		noErr(t, err)
		snap, err := events[0].Payment()
		noErr(t, err)
		if snap.CancelledAt == nil || snap.Status != core.StatusExpired || snap.CancelledBy != "test key" {
			t.Fatalf("payment.expired payload of a cancellation: %+v", snap)
		}
		co, err := e.Service.GetCheckout(context.Background(), p.ID)
		noErr(t, err)
		if co.Status != core.StatusExpired || co.CancelledAt == nil {
			t.Fatalf("checkout of a cancelled payment: %+v", co)
		}

		// Safe to retry: same answer, nothing new happens.
		e.Clock.Advance(time.Hour)
		again, err := e.Service.CancelPayment(e.Operator(), p.ID)
		noErr(t, err)
		if !again.CancelledAt.Equal(*got.CancelledAt) || again.CancelledBy != "test key" || !again.UpdatedAt.Equal(got.UpdatedAt) {
			t.Fatalf("second cancel changed the payment: %+v", again)
		}
		// The deadline passing later changes nothing either.
		e.Expire(p)
		e.Head(700)
		e.Head(701)
		wantCancelled(t, e, p.ID)
		wantEvents(t, e, p.ID, created, expired)

		// It is an expired payment for lists and counts; a naturally expired
		// one has no cancelled_at.
		other := e.CreatePayment("1")
		e.Expire(other)
		e.Head(900)
		if q := wantStatus(t, e, other.ID, core.StatusExpired); q.CancelledAt != nil || q.CancelledBy != "" {
			t.Fatalf("naturally expired payment: %+v", q)
		}
		counts, err := e.Service.CountPayments(e.System(), core.PaymentFilter{})
		noErr(t, err)
		if counts != (core.PaymentCounts{All: 2, Expired: 2}) {
			t.Fatalf("counts: %+v", counts)
		}
	}},

	{"a payment with funds in sight cannot be cancelled", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx := e.Operator()
		refuse := func(id string, status core.PaymentStatus) {
			t.Helper()
			before := e.EventTypes(id)
			_, err := e.Service.CancelPayment(ctx, id)
			wantIs(t, err, core.ErrPaymentHasFunds)
			wantKind(t, err, core.KindConflict)
			if p := wantStatus(t, e, id, status); p.CancelledAt != nil {
				t.Fatalf("refused cancel marked the payment: %+v", p)
			}
			if !slices.Equal(e.EventTypes(id), before) {
				t.Fatalf("refused cancel emitted events")
			}
		}
		partial := e.CreatePayment("149")
		e.Pay(partial, "100", 100)
		refuse(partial.ID, core.StatusDetected) // pending, short
		full := e.CreatePayment("149")
		e.Pay(full, "149", 100)
		refuse(full.ID, core.StatusConfirming) // pending, enough
		e.Head(114)
		refuse(partial.ID, core.StatusDetected) // confirmed, short: the payer may still top up
		// The payments run their course.
		wantStatus(t, e, full.ID, core.StatusPaid)
		e.Pay(partial, "49", 120)
		e.Head(134)
		wantStatus(t, e, partial.ID, core.StatusPaid)
	}},

	{"a settled payment cannot be cancelled", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx := e.Operator()
		paidP, lapsed, short := e.CreatePayment("10"), e.CreatePayment("10"), e.CreatePayment("10")
		e.Pay(paidP, "10", 100)
		e.Pay(short, "4", 100)
		e.Head(114)
		e.Expire(lapsed)
		e.Head(115)
		for id, status := range map[string]core.PaymentStatus{paidP.ID: core.StatusPaid, lapsed.ID: core.StatusExpired, short.ID: core.StatusUnderpaid} {
			_, err := e.Service.CancelPayment(ctx, id)
			wantIs(t, err, core.ErrPaymentSettled)
			wantKind(t, err, core.KindConflict)
			if p := wantStatus(t, e, id, status); p.CancelledAt != nil {
				t.Fatalf("refused cancel marked the payment: %+v", p)
			}
		}
		wantEvents(t, e, lapsed.ID, created, expired)
		_, err := e.Service.CancelPayment(ctx, "pay_missing")
		wantIs(t, err, core.ErrNotFound)
	}},

	{"funds arriving after a cancellation are late funds, never lost", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p := e.CreatePayment("149")
		_, err := e.Service.CancelPayment(e.Operator(), p.ID)
		noErr(t, err)

		// The address is still watched.
		state, err := e.Service.ChainState(e.System())
		noErr(t, err)
		if len(state.Addresses) != 1 || state.Addresses[0].PaymentID != p.ID {
			t.Fatalf("a cancelled payment must stay on the watch list: %+v", state.Addresses)
		}

		tr := e.Pay(p, "149", 100)
		got := wantCancelled(t, e, p.ID)
		wantAmounts(t, got, "149.00", "0.00")
		if got.NeedsAttention {
			t.Fatal("unconfirmed funds must not raise attention yet")
		}
		e.Head(114)
		got = wantCancelled(t, e, p.ID)
		wantAmounts(t, got, "149.00", "149.00")
		if !got.NeedsAttention || got.Attention != core.AttentionLateFunds {
			t.Fatalf("funds on a cancelled payment must need attention: %+v", got)
		}
		wantEvents(t, e, p.ID, created, expired, lateFunds)
		// Replays change nothing.
		e.Observe(tr)
		e.Head(114)
		e.Head(200)
		_, err = e.Service.CancelPayment(e.Operator(), p.ID)
		noErr(t, err)
		wantAmounts(t, wantCancelled(t, e, p.ID), "149.00", "149.00")
		wantEvents(t, e, p.ID, created, expired, lateFunds)
		// The operator closes the issue as for any other late funds.
		resolved, err := e.Service.ResolvePayment(e.Operator(), p.ID, "Refunded.")
		noErr(t, err)
		if resolved.NeedsAttention || resolved.CancelledAt == nil {
			t.Fatalf("resolved: %+v", resolved)
		}
		// The watch window ends as for any settled payment.
		e.Clock.Advance(core.DefaultWatchWindow + time.Hour)
		e.Head(300)
		state, err = e.Service.ChainState(e.System())
		noErr(t, err)
		if len(state.Addresses) != 0 {
			t.Fatalf("still watched after the window: %+v", state.Addresses)
		}
	}},

	{"a transfer mined before the cancellation but reported after it", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		e.Head(500)
		p := e.CreatePayment("149")
		e.Head(520)
		_, err := e.Service.CancelPayment(e.Operator(), p.ID)
		noErr(t, err)
		// The watcher was behind: the payer's transfer sits in block 503.
		e.Pay(p, "149", 503)
		got := wantCancelled(t, e, p.ID)
		wantAmounts(t, got, "149.00", "149.00")
		if !got.NeedsAttention {
			t.Fatalf("must need attention: %+v", got)
		}
		wantEvents(t, e, p.ID, created, expired, lateFunds)
	}},

	{"reorg, cancel, re-inclusion: counted once, as late funds", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx := e.Operator()
		p := e.CreatePayment("149")
		tr := e.Pay(p, "149", 100)
		_, err := e.Service.CancelPayment(ctx, p.ID)
		wantIs(t, err, core.ErrPaymentHasFunds)
		e.Retract(tr)
		wantStatus(t, e, p.ID, core.StatusAwaiting)
		// Nothing live any more: now it can be cancelled. It is "expired",
		// not "failed" — the merchant closed it.
		_, err = e.Service.CancelPayment(ctx, p.ID)
		noErr(t, err)
		wantCancelled(t, e, p.ID)
		e.Retract(tr) // replayed retraction
		wantAmounts(t, wantCancelled(t, e, p.ID), "0.00", "0.00")

		// The transaction is mined again in another block …
		again := tr
		again.BlockNumber, again.BlockHash = 103, BlockHash(103, 1)
		e.Observe(again)
		// … and a confused watcher also replays the stale observation.
		e.Observe(tr)
		e.Observe(again)
		wantAmounts(t, wantCancelled(t, e, p.ID), "149.00", "0.00")
		e.Head(117)
		got := wantCancelled(t, e, p.ID)
		wantAmounts(t, got, "149.00", "149.00")
		if !got.NeedsAttention || got.Attention != core.AttentionLateFunds {
			t.Fatalf("re-included funds on a cancelled payment: %+v", got)
		}
		wantEvents(t, e, p.ID, created, confirming, awaiting, expired, lateFunds)
		// And it can be reorganised out again before it is final elsewhere.
		late := e.Pay(p, "1", 200)
		e.Retract(late)
		wantAmounts(t, wantCancelled(t, e, p.ID), "149.00", "149.00")
	}},

	{"cancelling frees the slot of a limited link", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		public := context.Background()
		_, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Slug: "one", Name: "Only one", Amount: "5", MaxUses: 1, Active: true})
		noErr(t, err)
		first, _, err := e.Service.CreatePaymentFromLink(public, "one", core.LinkPaymentInput{})
		noErr(t, err)
		_, _, err = e.Service.CreatePaymentFromLink(public, "one", core.LinkPaymentInput{})
		wantIs(t, err, core.ErrLinkLimitReached)
		_, err = e.Service.CancelPayment(e.Operator(), first.ID)
		noErr(t, err)
		_, _, err = e.Service.CreatePaymentFromLink(public, "one", core.LinkPaymentInput{})
		noErr(t, err)
	}},
}

var linkDeleteCases = []serviceCase{
	{"created flag of link payments", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		public := context.Background()
		_, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Slug: "ticket", Name: "Ticket", Amount: "5", Active: true})
		noErr(t, err)
		first, isNew, err := e.Service.CreatePaymentFromLink(public, "ticket", core.LinkPaymentInput{IdempotencyKey: "visit"})
		noErr(t, err)
		if !isNew {
			t.Fatal("first call must report created")
		}
		again, isNew, err := e.Service.CreatePaymentFromLink(public, "ticket", core.LinkPaymentInput{IdempotencyKey: "visit"})
		noErr(t, err)
		if isNew || again.ID != first.ID {
			t.Fatalf("replay: created %v, id %s vs %s", isNew, again.ID, first.ID)
		}
		other, isNew, err := e.Service.CreatePaymentFromLink(public, "ticket", core.LinkPaymentInput{})
		noErr(t, err)
		if !isNew || other.ID == first.ID {
			t.Fatal("a call without a key must create a new payment")
		}
	}},

	{"deleting a link keeps its payments", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx, public := e.Operator(), context.Background()
		l, err := e.Service.CreateLink(ctx, core.LinkInput{Slug: "promo", Name: "Promo", Amount: "20", Active: true})
		noErr(t, err)
		keep, err := e.Service.CreateLink(ctx, core.LinkInput{Slug: "keep", Name: "Keep", Amount: "1", Active: true})
		noErr(t, err)
		done, _, err := e.Service.CreatePaymentFromLink(public, "promo", core.LinkPaymentInput{})
		noErr(t, err)
		e.Pay(e.Payment(done.ID), "20", 100)
		e.Head(114)
		open, _, err := e.Service.CreatePaymentFromLink(public, "promo", core.LinkPaymentInput{IdempotencyKey: "k"})
		noErr(t, err)

		noErr(t, e.Service.DeleteLink(ctx, l.ID))
		wantIs(t, e.Service.DeleteLink(ctx, l.ID), core.ErrNotFound)
		_, err = e.Service.GetLink(ctx, l.ID)
		wantIs(t, err, core.ErrNotFound)
		_, err = e.Service.GetLinkCheckout(public, "promo")
		wantIs(t, err, core.ErrNotFound)
		_, _, err = e.Service.CreatePaymentFromLink(public, "promo", core.LinkPaymentInput{IdempotencyKey: "k"})
		wantIs(t, err, core.ErrNotFound)
		list, err := e.Service.ListLinks(ctx)
		noErr(t, err)
		if len(list) != 1 || list[0].ID != keep.ID {
			t.Fatalf("links after delete: %+v", list)
		}

		// Its payments are untouched: history stays, the open one can still
		// be paid, and both remain findable by the link's id.
		if p := wantStatus(t, e, done.ID, core.StatusPaid); p.LinkID == nil || *p.LinkID != l.ID {
			t.Fatalf("paid payment of a deleted link: %+v", p)
		}
		e.Pay(e.Payment(open.ID), "20", 200)
		e.Head(214)
		if p := wantStatus(t, e, open.ID, core.StatusPaid); p.LinkID == nil || *p.LinkID != l.ID {
			t.Fatalf("payment settled after its link was deleted: %+v", p)
		}
		page, err := e.Service.ListPayments(e.System(), core.ListPaymentsInput{Filter: core.PaymentFilter{LinkID: l.ID}})
		noErr(t, err)
		if page.Total != 2 {
			t.Fatalf("payments of the deleted link: %d", page.Total)
		}
		// The slug is free again, for a new link with its own statistics.
		fresh, err := e.Service.CreateLink(ctx, core.LinkInput{Slug: "promo", Name: "Promo 2", Amount: "20", Active: true})
		noErr(t, err)
		if fresh.ID == l.ID || fresh.Uses != 0 {
			t.Fatalf("new link on a reused slug: %+v", fresh)
		}
	}},
}

// oneDelivery sets up an endpoint and one planned delivery.
func oneDelivery(t *testing.T, e *Env) core.WebhookEndpoint {
	t.Helper()
	ep, err := e.Service.CreateWebhookEndpoint(e.Operator(), core.WebhookEndpointInput{
		URL: "https://a.example.com/hook", Events: []core.EventType{created}, Enabled: true,
	})
	noErr(t, err)
	e.CreatePayment("1")
	n, err := e.Service.PlanWebhookDeliveries(e.System())
	noErr(t, err)
	if n != 1 {
		t.Fatalf("planned %d deliveries, want 1", n)
	}
	return ep
}

func claimOne(t *testing.T, e *Env) core.DeliveryJob {
	t.Helper()
	jobs, err := e.Service.ClaimWebhookDeliveries(e.System(), 10)
	noErr(t, err)
	if len(jobs) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(jobs))
	}
	return jobs[0]
}

func delivery(t *testing.T, e *Env, id string) core.WebhookDelivery {
	t.Helper()
	d, err := e.Service.GetWebhookDelivery(e.Operator(), id)
	noErr(t, err)
	return d
}

var claimCases = []serviceCase{
	{"a job carries exactly the headers the core owns", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		oneDelivery(t, e)
		job := claimOne(t, e)
		var names []string
		for k := range job.Headers {
			names = append(names, k)
		}
		slices.Sort(names)
		if want := []string{"Content-Type", core.HeaderDeliveryID, core.HeaderEventID, core.HeaderSignature}; !slices.Equal(names, want) {
			t.Fatalf("job headers = %v, want %v (the User-Agent is the dispatcher's)", names, want)
		}
		if !strings.HasPrefix(job.ClaimID, "whc_") || job.Attempt != 1 || !job.LeaseExpiresAt.Equal(e.Clock.Now().Add(core.WebhookClaimLease)) {
			t.Fatalf("job: %+v", job)
		}
		if r := job.Result(); r.ClaimID != job.ClaimID || r.Attempt != 1 {
			t.Fatalf("Result() = %+v", r)
		}
		if d := delivery(t, e, job.DeliveryID); d.ClaimID != job.ClaimID {
			t.Fatalf("delivery claim = %q, job claim = %q", d.ClaimID, job.ClaimID)
		}
	}},

	{"stale completion after a re-claim is not counted and overwrites nothing", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ep := oneDelivery(t, e)
		slow := claimOne(t, e)
		// The slow worker's lease runs out; another worker claims the job.
		e.Clock.Advance(core.WebhookClaimLease)
		fast := claimOne(t, e)
		if fast.DeliveryID != slow.DeliveryID || fast.ClaimID == slow.ClaimID || fast.Attempt != 1 {
			t.Fatalf("re-claim: %+v vs %+v", fast, slow)
		}
		if out := complete(t, e, fast, core.AttemptResult{ResponseCode: 500, ResponseBody: "from the fast worker"}); out != core.CompletionCounted {
			t.Fatalf("current claim: outcome %s", out)
		}
		counted := delivery(t, e, fast.DeliveryID)
		e.Clock.Advance(5 * time.Second)
		// Now the slow worker reports.
		if out := complete(t, e, slow, core.AttemptResult{ResponseCode: 502, ResponseBody: "from the slow worker"}); out != core.CompletionLate {
			t.Fatalf("stale claim: outcome %s, want late", out)
		}
		d := delivery(t, e, fast.DeliveryID)
		if d.Attempt != 1 || d.Status != core.DeliveryRetrying || !d.NextAttemptAt.Equal(counted.NextAttemptAt) ||
			d.ResponseCode != 500 || d.ResponseBody != "from the fast worker" || d.ClaimID != "" {
			t.Fatalf("stale completion changed the delivery: %+v", d)
		}
		if len(d.Attempts) != 2 || d.Attempts[0].Late || d.Attempts[0].N != 1 || !d.Attempts[1].Late || d.Attempts[1].N != 1 || d.Attempts[1].ResponseCode != 502 {
			t.Fatalf("attempt history: %+v", d.Attempts)
		}
		sum, err := e.Service.GetWebhookEndpoint(e.Operator(), ep.ID)
		noErr(t, err)
		if sum.ConsecutiveFailures != 1 {
			t.Fatalf("endpoint failures = %d, want 1 (the stale attempt is not counted)", sum.ConsecutiveFailures)
		}
		// The schedule continues with attempt 2 exactly once.
		e.Clock.Set(d.NextAttemptAt)
		if next := claimOne(t, e); next.Attempt != 2 {
			t.Fatalf("next attempt = %d, want 2", next.Attempt)
		}
	}},

	{"stale completion that arrives first, then the current one", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		oneDelivery(t, e)
		slow := claimOne(t, e)
		e.Clock.Advance(core.WebhookClaimLease)
		fast := claimOne(t, e)
		if out := complete(t, e, slow, core.AttemptResult{ResponseCode: 503}); out != core.CompletionLate {
			t.Fatalf("stale claim: outcome %s", out)
		}
		d := delivery(t, e, fast.DeliveryID)
		if d.Attempt != 0 || d.Status != core.DeliveryPending || d.ClaimID != fast.ClaimID || len(d.Attempts) != 1 || !d.Attempts[0].Late {
			t.Fatalf("after the stale result: %+v", d)
		}
		if out := complete(t, e, fast, core.AttemptResult{ResponseCode: 500}); out != core.CompletionCounted {
			t.Fatalf("current claim: outcome %s", out)
		}
		d = delivery(t, e, fast.DeliveryID)
		if d.Attempt != 1 || d.Status != core.DeliveryRetrying || len(d.Attempts) != 2 || d.Attempts[1].Late {
			t.Fatalf("after the current result: %+v", d)
		}
	}},

	{"a claim stays good past its lease while nobody else took the job", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		oneDelivery(t, e)
		job := claimOne(t, e)
		e.Clock.Advance(10 * core.WebhookClaimLease)
		if out := complete(t, e, job, core.AttemptResult{ResponseCode: 200}); out != core.CompletionCounted {
			t.Fatalf("outcome %s, want counted", out)
		}
		if d := delivery(t, e, job.DeliveryID); d.Status != core.DeliverySucceeded || d.Attempt != 1 {
			t.Fatalf("delivery: %+v", d)
		}
	}},

	{"a late 2xx still marks the delivery succeeded", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ep := oneDelivery(t, e)
		slow := claimOne(t, e)
		e.Clock.Advance(core.WebhookClaimLease)
		fast := claimOne(t, e)
		// The slow worker's request did get through.
		if out := complete(t, e, slow, core.AttemptResult{ResponseCode: 200, ResponseBody: "thanks"}); out != core.CompletionLate {
			t.Fatalf("outcome %s, want late", out)
		}
		d := delivery(t, e, slow.DeliveryID)
		if d.Status != core.DeliverySucceeded || d.Attempt != 1 || d.ResponseCode != 200 || d.ResponseBody != "thanks" ||
			!d.NextAttemptAt.IsZero() || d.CompletedAt.IsZero() || d.ClaimID != "" || len(d.Attempts) != 1 || !d.Attempts[0].Late || !d.Attempts[0].Succeeded {
			t.Fatalf("delivery after a late success: %+v", d)
		}
		// The other worker's failure can no longer undo it.
		if out := complete(t, e, fast, core.AttemptResult{ResponseCode: 500}); out != core.CompletionLate {
			t.Fatalf("outcome %s, want late", out)
		}
		d = delivery(t, e, slow.DeliveryID)
		if d.Status != core.DeliverySucceeded || d.Attempt != 1 || d.ResponseCode != 200 || len(d.Attempts) != 2 {
			t.Fatalf("a failure after a success changed the delivery: %+v", d)
		}
		if jobs, _ := e.Service.ClaimWebhookDeliveries(e.System(), 10); len(jobs) != 0 {
			t.Fatalf("a succeeded delivery was claimed again")
		}
		sum, _ := e.Service.GetWebhookEndpoint(e.Operator(), ep.ID)
		if sum.ConsecutiveFailures != 0 || sum.LastSuccessAt.IsZero() || sum.Succeeded7d != 1 {
			t.Fatalf("endpoint: %+v", sum)
		}
	}},

	{"results that arrive after the endpoint was disabled are recorded", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx := e.Operator()
		ep := oneDelivery(t, e)
		// A second delivery that is on the wire at the same time: a test
		// event (a later event's first attempt would wait for the first
		// one's, see "first attempts in event order").
		_, err := e.Service.SendTestWebhook(ctx, ep.ID)
		noErr(t, err)
		jobs, err := e.Service.ClaimWebhookDeliveries(e.System(), 10)
		noErr(t, err)
		if len(jobs) != 2 {
			t.Fatalf("claimed %d jobs", len(jobs))
		}
		// Both requests are on the wire when the operator disables it.
		_, err = e.Service.UpdateWebhookEndpoint(ctx, ep.ID, core.WebhookEndpointInput{URL: ep.URL, Events: ep.Events, Enabled: false})
		noErr(t, err)
		for _, job := range jobs {
			if d := delivery(t, e, job.DeliveryID); d.Status != core.DeliveryFailed || d.Error != "endpoint disabled" || d.Attempt != 0 || d.ClaimID != "" {
				t.Fatalf("delivery of a disabled endpoint: %+v", d)
			}
		}
		e.Clock.Advance(time.Second)
		// One was received: the record says so.
		if out := complete(t, e, jobs[0], core.AttemptResult{ResponseCode: 200, Duration: 80 * time.Millisecond}); out != core.CompletionLate {
			t.Fatalf("outcome %s, want late", out)
		}
		d := delivery(t, e, jobs[0].DeliveryID)
		if d.Status != core.DeliverySucceeded || d.Attempt != 1 || d.Error != "" || d.ResponseCode != 200 || !d.CompletedAt.Equal(e.Clock.Now()) ||
			len(d.Attempts) != 1 || !d.Attempts[0].Succeeded || !d.Attempts[0].Late {
			t.Fatalf("received delivery of a disabled endpoint: %+v", d)
		}
		// The other failed on the wire: still failed, but the attempt shows.
		if out := complete(t, e, jobs[1], core.AttemptResult{Error: "timeout", Duration: 10 * time.Second}); out != core.CompletionLate {
			t.Fatalf("outcome %s, want late", out)
		}
		d = delivery(t, e, jobs[1].DeliveryID)
		if d.Status != core.DeliveryFailed || d.Error != "endpoint disabled" || d.Attempt != 0 || len(d.Attempts) != 1 || !d.Attempts[0].Late || d.Attempts[0].Error != "timeout" {
			t.Fatalf("failed delivery of a disabled endpoint: %+v", d)
		}
		// The endpoint stays disabled; only the success is noted.
		sum, err := e.Service.GetWebhookEndpoint(ctx, ep.ID)
		noErr(t, err)
		if sum.Enabled || sum.DisabledReason != "manual" || !sum.LastSuccessAt.Equal(e.Clock.Now()) || sum.ConsecutiveFailures != 0 {
			t.Fatalf("endpoint: %+v", sum)
		}
	}},

	{"first attempts in event order", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx, sys := e.Operator(), e.System()
		ep := oneDelivery(t, e) // event 1
		e.CreatePayment("2")    // event 2
		e.CreatePayment("3")    // event 3
		_, err := e.Service.PlanWebhookDeliveries(sys)
		noErr(t, err)
		seq := func(job core.DeliveryJob) int64 {
			var env core.EventEnvelope
			noErr(t, json.Unmarshal(job.Body, &env))
			return env.Seq
		}
		// One first attempt at a time, the earliest event first.
		first := claimOne(t, e)
		// Given back unsent, it is still the one that goes first.
		released, err := e.Service.ReleaseWebhookDelivery(sys, first.DeliveryID, first.ClaimID, 0)
		noErr(t, err)
		if !released {
			t.Fatal("release refused")
		}
		again := claimOne(t, e)
		if again.DeliveryID != first.DeliveryID {
			t.Fatal("a later event's first attempt overtook a released earlier one")
		}
		// A test event is not held up by the queue.
		_, err = e.Service.SendTestWebhook(ctx, ep.ID)
		noErr(t, err)
		test := claimOne(t, e)
		if seq(test) != 0 || test.Headers[core.HeaderEventID] == again.Headers[core.HeaderEventID] {
			t.Fatalf("test job: %s", test.Body)
		}
		complete(t, e, test, core.AttemptResult{ResponseCode: 200})
		// Success: the next event, in order.
		complete(t, e, again, core.AttemptResult{ResponseCode: 200})
		second := claimOne(t, e)
		if seq(second) <= seq(again) {
			t.Fatalf("seq %d after seq %d", seq(second), seq(again))
		}
		// A failure makes the endpoint failing: what is due then goes out
		// unordered, and nothing waits for the failed event's retry.
		complete(t, e, second, core.AttemptResult{ResponseCode: 503})
		third := claimOne(t, e)
		if seq(third) <= seq(second) {
			t.Fatalf("seq %d after seq %d", seq(third), seq(second))
		}
		e.CreatePayment("4")
		e.CreatePayment("5")
		_, err = e.Service.PlanWebhookDeliveries(sys)
		noErr(t, err)
		jobs, err := e.Service.ClaimWebhookDeliveries(sys, 10)
		noErr(t, err)
		if len(jobs) != 2 {
			t.Fatalf("a failing endpoint's due first attempts: %d jobs, want 2", len(jobs))
		}
	}},

	{"release gives a claim back without an attempt", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		sys := e.System()
		ep := oneDelivery(t, e)
		job := claimOne(t, e)
		sub := e.Service.Subscribe()
		defer sub.Close()

		_, err := e.Service.ReleaseWebhookDelivery(sys, job.DeliveryID, "", 0)
		wantField(t, err, "claim_id")
		_, err = e.Service.ReleaseWebhookDelivery(sys, "whd_missing", job.ClaimID, 0)
		wantIs(t, err, core.ErrNotFound)
		released, err := e.Service.ReleaseWebhookDelivery(sys, job.DeliveryID, "whc_someone_else", 0)
		noErr(t, err)
		if released {
			t.Fatal("a release with the wrong claim took effect")
		}
		if jobs, _ := e.Service.ClaimWebhookDeliveries(sys, 10); len(jobs) != 0 {
			t.Fatal("the delivery is still leased; it must not be claimable")
		}

		e.Clock.Advance(3 * time.Second)
		released, err = e.Service.ReleaseWebhookDelivery(sys, job.DeliveryID, job.ClaimID, 0)
		noErr(t, err)
		if !released {
			t.Fatal("release of the current claim did not take effect")
		}
		select {
		case <-sub.C():
		default:
			t.Fatal("an immediate release must wake dispatchers")
		}
		d := delivery(t, e, job.DeliveryID)
		if d.Status != core.DeliveryPending || d.Attempt != 0 || len(d.Attempts) != 0 || d.ClaimID != "" || !d.NextAttemptAt.Equal(e.Clock.Now()) {
			t.Fatalf("released delivery: %+v", d)
		}
		sum, _ := e.Service.GetWebhookEndpoint(e.Operator(), ep.ID)
		if sum.ConsecutiveFailures != 0 || !sum.LastDeliveryAt.IsZero() {
			t.Fatalf("release touched the endpoint: %+v", sum)
		}
		// Releasing twice, or completing under the released claim, is stale.
		released, err = e.Service.ReleaseWebhookDelivery(sys, job.DeliveryID, job.ClaimID, 0)
		noErr(t, err)
		if released {
			t.Fatal("second release took effect")
		}
		// It is due at once and gets a fresh claim, still for attempt 1.
		again := claimOne(t, e)
		if again.ClaimID == job.ClaimID || again.Attempt != 1 {
			t.Fatalf("claim after release: %+v", again)
		}
		if out := complete(t, e, job, core.AttemptResult{ResponseCode: 500}); out != core.CompletionLate {
			t.Fatalf("completion under a released claim: %s", out)
		}

		// With a delay the delivery comes back later, without waiting out
		// the lease.
		released, err = e.Service.ReleaseWebhookDelivery(sys, again.DeliveryID, again.ClaimID, 2*time.Second)
		noErr(t, err)
		if !released {
			t.Fatal("delayed release did not take effect")
		}
		e.Clock.Advance(1999 * time.Millisecond)
		if jobs, _ := e.Service.ClaimWebhookDeliveries(sys, 10); len(jobs) != 0 {
			t.Fatal("claimable before the release delay passed")
		}
		e.Clock.Advance(time.Millisecond)
		third := claimOne(t, e)
		// The delay is capped at the lease.
		released, err = e.Service.ReleaseWebhookDelivery(sys, third.DeliveryID, third.ClaimID, 24*time.Hour)
		noErr(t, err)
		if d := delivery(t, e, third.DeliveryID); !released || !d.NextAttemptAt.Equal(e.Clock.Now().Add(core.WebhookClaimLease)) {
			t.Fatalf("release with an excessive delay: %+v", d)
		}
		// A finished delivery cannot be released.
		e.Clock.Advance(core.WebhookClaimLease)
		last := claimOne(t, e)
		complete(t, e, last, core.AttemptResult{ResponseCode: 200})
		released, err = e.Service.ReleaseWebhookDelivery(sys, last.DeliveryID, last.ClaimID, 0)
		noErr(t, err)
		if d := delivery(t, e, last.DeliveryID); released || d.Status != core.DeliverySucceeded {
			t.Fatalf("release after completion: released %v, %+v", released, d)
		}
	}},

	{"a resend supersedes a claim that is still out", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		oneDelivery(t, e)
		out := claimOne(t, e)
		_, err := e.Service.ResendWebhookDelivery(e.Operator(), out.DeliveryID)
		noErr(t, err)
		// The attempt that was on the wire fails: the operator's resend has
		// precedence, so that failure neither counts nor delays the resend.
		if got := complete(t, e, out, core.AttemptResult{ResponseCode: 500}); got != core.CompletionLate {
			t.Fatalf("superseded claim: outcome %s", got)
		}
		if d := delivery(t, e, out.DeliveryID); d.Status != core.DeliveryPending || d.Attempt != 0 || !d.NextAttemptAt.Equal(e.Clock.Now()) || len(d.Attempts) != 1 || !d.Attempts[0].Late {
			t.Fatalf("delivery after the superseded attempt failed: %+v", d)
		}
		fresh := claimOne(t, e)
		if fresh.ClaimID == out.ClaimID || fresh.Attempt != 1 {
			t.Fatalf("claim after resend: %+v", fresh)
		}
		if got := complete(t, e, fresh, core.AttemptResult{ResponseCode: 200}); got != core.CompletionCounted {
			t.Fatalf("fresh claim: outcome %s", got)
		}
		if d := delivery(t, e, out.DeliveryID); d.Status != core.DeliverySucceeded || d.Attempt != 1 || len(d.Attempts) != 2 || d.Attempts[1].Late {
			t.Fatalf("delivery: %+v", d)
		}
	}},

	{"endpoint URLs: no credentials, and the wiring's policy applies everywhere", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		blocked := "internal"
		var asked []string
		e := newEnv(WithConfig(func(c *core.Config) {
			c.ValidateWebhookURL = func(u string) error {
				asked = append(asked, u)
				if blocked != "" && strings.Contains(u, blocked) {
					return errors.New("destination is not allowed by the egress policy")
				}
				return nil
			}
		}))
		ctx := e.Operator()
		in := func(u string, enabled bool) core.WebhookEndpointInput {
			return core.WebhookEndpointInput{URL: u, Events: []core.EventType{paid}, Enabled: enabled}
		}
		for _, u := range []string{"https://user:secret@shop.example.com/hook", "https://token@shop.example.com/hook", "http://:pw@shop.example.com/"} {
			_, err := e.Service.CreateWebhookEndpoint(ctx, in(u, true))
			wantField(t, err, "url")
			if f := core.FieldErrors(err); f[0].Code != "credentials" || strings.Contains(f[0].Message, "secret") {
				t.Fatalf("%s: %+v", u, f)
			}
		}
		if len(asked) != 0 {
			t.Fatalf("the policy hook saw URLs the core had already refused: %v", asked)
		}
		_, err := e.Service.CreateWebhookEndpoint(ctx, in("https://hooks.internal.example.com/x", true))
		wantField(t, err, "url")
		if f := core.FieldErrors(err); f[0].Code != "not_allowed" || f[0].Message != "destination is not allowed by the egress policy" {
			t.Fatalf("policy rejection: %+v", f)
		}
		_, err = e.Service.CreateWebhookEndpoint(ctx, in("https://hooks.internal.example.com/x", false))
		wantField(t, err, "url") // also when created disabled
		ep, err := e.Service.CreateWebhookEndpoint(ctx, in("https://shop.example.com/hook", true))
		noErr(t, err)
		if !slices.Contains(asked, "https://shop.example.com/hook") {
			t.Fatalf("the policy hook was not asked: %v", asked)
		}
		_, err = e.Service.UpdateWebhookEndpoint(ctx, ep.ID, in("https://hooks.internal.example.com/x", true))
		wantField(t, err, "url")
		_, err = e.Service.UpdateWebhookEndpoint(ctx, ep.ID, in("https://u:p@shop.example.com/hook", false))
		wantField(t, err, "url")

		// The policy got stricter after the endpoint was saved: it can still
		// be disabled as it is, but not saved enabled or moved elsewhere bad.
		blocked = "shop.example.com"
		_, err = e.Service.UpdateWebhookEndpoint(ctx, ep.ID, in(ep.URL, true))
		wantField(t, err, "url")
		off, err := e.Service.UpdateWebhookEndpoint(ctx, ep.ID, in(ep.URL, false))
		noErr(t, err)
		if off.Enabled || off.URL != ep.URL {
			t.Fatalf("disabled endpoint: %+v", off)
		}
		_, err = e.Service.UpdateWebhookEndpoint(ctx, ep.ID, in(ep.URL, true))
		wantField(t, err, "url")
	}},

	{"delivery list validates statuses", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		_, err := e.Service.ListWebhookDeliveries(e.Operator(), core.DeliveryFilter{Statuses: []core.DeliveryStatus{core.DeliveryFailed, "exploded"}}, core.Page{})
		wantField(t, err, "status")
		_, err = e.Service.ListWebhookDeliveries(e.Operator(), core.DeliveryFilter{Statuses: []core.DeliveryStatus{core.DeliveryPending, core.DeliveryRetrying, core.DeliverySucceeded, core.DeliveryFailed}}, core.Page{})
		noErr(t, err)
	}},
}

var sessionCases = []serviceCase{
	{"per-payment subscriptions", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		a, b := e.CreatePayment("1"), e.CreatePayment("2")
		global, subA, subB, none := e.Service.Subscribe(), e.Service.SubscribePayment(a.ID), e.Service.SubscribePayment(b.ID), e.Service.SubscribePayment("")
		second := e.Service.SubscribePayment(a.ID)
		defer global.Close()
		defer subB.Close()
		defer none.Close()
		fired := func(s *core.Subscription) bool {
			select {
			case <-s.C():
				return true
			default:
				return false
			}
		}
		check := func(what string, wantGlobal, wantA, wantB bool) {
			t.Helper()
			if g, x, x2, y, n := fired(global), fired(subA), fired(second), fired(subB), fired(none); g != wantGlobal || x != wantA || x2 != wantA || y != wantB || n {
				t.Fatalf("%s: global %v, A %v/%v, B %v, none %v; want %v %v %v false", what, g, x, x2, y, n, wantGlobal, wantA, wantB)
			}
		}
		check("nothing happened", false, false, false)
		e.Pay(a, "1", 100)
		check("event for A", true, true, false)
		e.CreatePayment("3")
		check("another payment created", true, false, false)
		e.Pay(b, "1", 100) // detected
		e.Pay(b, "1", 101) // confirming
		check("two events for B coalesce", true, false, true)
		e.Head(105) // confirmation counts move, no event
		check("head without events", false, false, false)
		e.Head(115) // both become final in one transaction
		check("one transaction, two payments", true, true, true)
		_, err := e.Service.UpdateMerchant(e.Operator(), core.UpdateMerchantInput{Merchant: core.MerchantSettings{Name: "N", SupportEmail: "a@b.co"}})
		noErr(t, err)
		check("instance event", true, false, false)
		// Closed subscriptions stop firing; closing twice is fine.
		subA.Close()
		subA.Close()
		second.Close()
		_, err = e.Service.ResolvePayment(e.Operator(), a.ID, "") // nothing to resolve: no event
		wantIs(t, err, core.ErrNothingToResolve)
		e.Pay(a, "5", 200)
		e.Head(214) // late funds on A
		if fired(subA) || fired(second) || !fired(global) {
			t.Fatal("closed subscription fired, or the global one did not")
		}
	}},

	{"session list and revocation", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		public := context.Background()
		e.Clock.Advance(time.Minute)
		laptop, err := e.Service.Login(public, core.LoginInput{Email: e.Email, Password: e.Password, UserAgent: "laptop", IP: "198.51.100.7"})
		noErr(t, err)
		e.Clock.Advance(time.Minute)
		phone, err := e.Service.Login(public, core.LoginInput{Email: e.Email, Password: e.Password, UserAgent: "phone"})
		noErr(t, err)
		actor, err := e.Service.AuthenticateSession(public, laptop.Token)
		noErr(t, err)
		ctx := core.WithActor(public, actor)

		list, err := e.Service.ListSessions(ctx)
		noErr(t, err)
		if len(list) != 3 || list[0].ID != phone.Session.ID || list[1].ID != laptop.Session.ID || list[1].UserAgent != "laptop" || list[1].IP != "198.51.100.7" {
			t.Fatalf("sessions: %+v", list)
		}
		for _, s := range list {
			if s.TokenHash != "" {
				t.Fatalf("session list leaks the token hash: %+v", s)
			}
		}
		if list[1].ID != actor.SessionID {
			t.Fatalf("the caller's session must be identifiable: %+v vs %+v", list[1], actor)
		}

		noErr(t, e.Service.RevokeSession(ctx, phone.Session.ID))
		_, err = e.Service.AuthenticateSession(public, phone.Token)
		wantIs(t, err, core.ErrInvalidCredentials)
		wantIs(t, e.Service.RevokeSession(ctx, phone.Session.ID), core.ErrNotFound)
		wantIs(t, e.Service.RevokeSession(ctx, "sess_missing"), core.ErrNotFound)
		_, err = e.Service.AuthenticateSession(public, laptop.Token)
		noErr(t, err)

		// Sessions that no longer work are not listed: the setup session,
		// unused since it was created, idles out first.
		e.Clock.Set(list[2].CreatedAt.Add(core.DefaultSessionIdleTimeout))
		list, err = e.Service.ListSessions(ctx)
		noErr(t, err)
		if len(list) != 1 || list[0].ID != laptop.Session.ID {
			t.Fatalf("sessions after one expired: %+v", list)
		}
		// Revoking the own session is a logout.
		noErr(t, e.Service.RevokeSession(ctx, laptop.Session.ID))
		_, err = e.Service.AuthenticateSession(public, laptop.Token)
		wantIs(t, err, core.ErrInvalidCredentials)
	}},
}
