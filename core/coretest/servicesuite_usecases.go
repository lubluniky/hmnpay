package coretest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

var paymentCases = []serviceCase{
	{"defaults of a new payment", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		p, isNew, err := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{
			Amount: "49.99", OrderID: " ORD-1 ", Description: "Pro plan", CustomerEmail: "Maria@Example.com",
			RedirectURL: "https://shop.example.com/thanks", Metadata: map[string]string{"plan": "pro"},
		})
		noErr(t, err)
		if !isNew || p.Status != core.StatusAwaiting || p.Amount != "49.99" || p.AmountReceived != "0.00" ||
			p.Asset != "USDT" || p.Network != "BEP20" || p.ChainID != 56 || p.Decimals != 18 ||
			p.OrderID != "ORD-1" || p.CustomerEmail != "maria@example.com" || p.Metadata["plan"] != "pro" ||
			p.RequiredConfirmations != 15 || !p.ExpiresAt.Equal(p.CreatedAt.Add(30*time.Minute)) ||
			p.CheckoutURL != "https://pay.example.com/pay/"+p.ID || p.TxHash != nil || p.LinkID != nil {
			t.Fatalf("new payment: %+v", p)
		}
		if !strings.HasPrefix(p.ID, "pay_") || len(p.ID) != 30 {
			t.Fatalf("payment id %q is not pay_ + 26 characters", p.ID)
		}
		if p.Address != Address(XPub, core.DefaultDerivationPath, uint32(0)) {
			t.Fatalf("first payment must use address index 0, got %s", p.Address)
		}
		q, _, err := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "1", TTL: 2 * time.Hour})
		noErr(t, err)
		if q.Address != Address(XPub, core.DefaultDerivationPath, uint32(1)) || !q.ExpiresAt.Equal(q.CreatedAt.Add(2*time.Hour)) || q.Metadata == nil {
			t.Fatalf("second payment: %+v", q)
		}
		wantEvents(t, e, p.ID, created)

		// The event carries the payment as its data, in the wire shape.
		events, err := e.Service.Events(e.System(), core.EventFilter{PaymentID: p.ID})
		noErr(t, err)
		snap, err := events[0].Payment()
		noErr(t, err)
		if snap.ID != p.ID || snap.Amount != "49.99" || snap.Status != core.StatusAwaiting || !strings.HasPrefix(events[0].ID, "evt_") {
			t.Fatalf("event payload: %+v", snap)
		}
	}},

	{"input validation", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		long := strings.Repeat("x", 600)
		many := map[string]string{}
		for i := range 21 {
			many[string(rune('a'+i))] = "v"
		}
		tests := []struct {
			field string
			in    core.CreatePaymentInput
		}{
			{"amount", core.CreatePaymentInput{}},
			{"amount", core.CreatePaymentInput{Amount: "0"}},
			{"amount", core.CreatePaymentInput{Amount: "0.00"}},
			{"amount", core.CreatePaymentInput{Amount: "-5"}},
			{"amount", core.CreatePaymentInput{Amount: "ten"}},
			{"amount", core.CreatePaymentInput{Amount: "1e3"}},
			{"amount", core.CreatePaymentInput{Amount: "1,000.00"}},
			{"amount", core.CreatePaymentInput{Amount: "1.0000000000000000001"}},
			{"amount", core.CreatePaymentInput{Amount: "1000000000.01"}},
			{"amount", core.CreatePaymentInput{Amount: strings.Repeat("9", 70)}},
			{"order_id", core.CreatePaymentInput{Amount: "1", OrderID: long}},
			{"description", core.CreatePaymentInput{Amount: "1", Description: long}},
			{"customer_email", core.CreatePaymentInput{Amount: "1", CustomerEmail: "not-an-email"}},
			{"redirect_url", core.CreatePaymentInput{Amount: "1", RedirectURL: "javascript:alert(1)"}},
			{"redirect_url", core.CreatePaymentInput{Amount: "1", RedirectURL: "/relative"}},
			{"ttl", core.CreatePaymentInput{Amount: "1", TTL: time.Second}},
			{"ttl", core.CreatePaymentInput{Amount: "1", TTL: 365 * 24 * time.Hour}},
			{"metadata", core.CreatePaymentInput{Amount: "1", Metadata: many}},
			{"metadata.k", core.CreatePaymentInput{Amount: "1", Metadata: map[string]string{"k": long}}},
			{"idempotency_key", core.CreatePaymentInput{Amount: "1", IdempotencyKey: "has space"}},
		}
		for _, tc := range tests {
			_, _, err := e.Service.CreatePayment(e.System(), tc.in)
			if core.KindOf(err) != core.KindInvalid {
				t.Errorf("%+v: error %v, want a validation error", tc.in, err)
				continue
			}
			found := false
			for _, f := range core.FieldErrors(err) {
				found = found || f.Field == tc.field
			}
			if !found {
				t.Errorf("%+v: error %v, want field %q", tc.in, err, tc.field)
			}
		}
		// Several problems are reported together.
		_, _, err := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "x", CustomerEmail: "y", RedirectURL: "z"})
		if n := len(core.FieldErrors(err)); n != 3 {
			t.Errorf("want 3 field errors, got %d: %v", n, err)
		}
		// Nothing was created and no address index was burned.
		page, err := e.Service.ListPayments(e.System(), core.ListPaymentsInput{})
		noErr(t, err)
		if page.Total != 0 {
			t.Errorf("invalid requests created %d payments", page.Total)
		}
		p := e.CreatePayment("1")
		if p.Address != Address(XPub, core.DefaultDerivationPath, uint32(0)) {
			t.Errorf("a failed create consumed an address index")
		}
		// The largest and the smallest amounts are fine.
		e.CreatePayment("1000000000")
		e.CreatePayment("0.000000000000000001")
	}},

	{"idempotency key returns the same payment", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		in := core.CreatePaymentInput{Amount: "149.00", OrderID: "ORD-7", Metadata: map[string]string{"a": "b"}, IdempotencyKey: "req-1"}
		first, isNew, err := e.Service.CreatePayment(e.System(), in)
		noErr(t, err)
		if !isNew {
			t.Fatal("first create must report created")
		}
		e.Clock.Advance(10 * time.Minute)
		again, isNew, err := e.Service.CreatePayment(e.System(), in)
		noErr(t, err)
		if isNew || again.ID != first.ID || again.Address != first.Address || !again.ExpiresAt.Equal(first.ExpiresAt) {
			t.Fatalf("retry returned another payment: %+v vs %+v", again, first)
		}
		// Equivalent spellings of the same request are the same request.
		spelled := in
		spelled.Amount, spelled.OrderID = "149", " ORD-7"
		again, isNew, err = e.Service.CreatePayment(e.System(), spelled)
		noErr(t, err)
		if isNew || again.ID != first.ID {
			t.Fatal("equivalent retry must return the first payment")
		}
		// The retry reflects the payment's current state.
		e.Pay(first, "149", 100)
		again, _, err = e.Service.CreatePayment(e.System(), in)
		noErr(t, err)
		if again.Status != core.StatusConfirming {
			t.Fatalf("retry must return current state, got %s", again.Status)
		}

		for _, change := range []func(*core.CreatePaymentInput){
			func(c *core.CreatePaymentInput) { c.Amount = "150" },
			func(c *core.CreatePaymentInput) { c.OrderID = "ORD-8" },
			func(c *core.CreatePaymentInput) { c.Metadata = map[string]string{"a": "c"} },
			func(c *core.CreatePaymentInput) { c.TTL = time.Hour },
			func(c *core.CreatePaymentInput) { c.CustomerEmail = "x@example.com" },
		} {
			other := in
			change(&other)
			_, _, err := e.Service.CreatePayment(e.System(), other)
			wantIs(t, err, core.ErrIdempotencyMismatch)
			wantKind(t, err, core.KindConflict)
		}
		page, err := e.Service.ListPayments(e.System(), core.ListPaymentsInput{})
		noErr(t, err)
		if page.Total != 1 {
			t.Fatalf("%d payments exist for one idempotency key", page.Total)
		}
		wantEvents(t, e, first.ID, created, confirming)

		// Without a key, identical requests are separate payments, and the
		// order id is not unique.
		a, _, _ := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "1", OrderID: "ORD-7"})
		b, _, _ := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "1", OrderID: "ORD-7"})
		if a.ID == b.ID || a.Address == b.Address {
			t.Fatal("requests without a key must create separate payments")
		}
	}},

	{"concurrent creates: unique addresses, one payment per key", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		const n = 40
		views := make([]core.PaymentView, n)
		keyed := make([]core.PaymentView, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				var err error
				if views[i], _, err = e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "5"}); err != nil {
					t.Errorf("create: %v", err)
				}
				if keyed[i], _, err = e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "7", IdempotencyKey: "same"}); err != nil {
					t.Errorf("keyed create: %v", err)
				}
			})
		}
		wg.Wait()
		addresses := map[string]bool{}
		for _, v := range views {
			addresses[v.Address] = true
		}
		if len(addresses) != n {
			t.Fatalf("%d payments share %d addresses", n, len(addresses))
		}
		for _, v := range keyed {
			if v.ID != keyed[0].ID {
				t.Fatal("concurrent creates with one idempotency key produced different payments")
			}
		}
		st, err := e.Service.GetSettings(e.Operator())
		noErr(t, err)
		if st.Wallet.NextAddressIndex != n+1 {
			t.Fatalf("next address index = %d, want %d", st.Wallet.NextAddressIndex, n+1)
		}
	}},

	{"an address is never reused, even if the index is rewound", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		a, b := e.CreatePayment("1"), e.CreatePayment("2")
		zero := uint32(0)
		st, err := e.Service.UpdateWallet(e.Operator(), core.UpdateWalletInput{XPub: XPub, NextAddressIndex: &zero})
		noErr(t, err)
		if st.Wallet.NextAddressIndex != 0 {
			t.Fatalf("index not rewound: %+v", st.Wallet)
		}
		c := e.CreatePayment("3")
		if c.Address == a.Address || c.Address == b.Address || c.Address != Address(XPub, core.DefaultDerivationPath, uint32(2)) {
			t.Fatalf("address reused or wrong: %s", c.Address)
		}
		// A different key starts at index 0 again, with its own addresses.
		other := XPub[:len(XPub)-4] + "zzzz"
		_, err = e.Service.UpdateWallet(e.Operator(), core.UpdateWalletInput{XPub: other})
		noErr(t, err)
		d := e.CreatePayment("4")
		if d.Address != Address(other, core.DefaultDerivationPath, uint32(0)) {
			t.Fatalf("new wallet must start at index 0, got %s", d.Address)
		}
		// Payments of the old wallet are still watched and payable.
		e.Pay(a, "1", 100)
		wantStatus(t, e, a.ID, core.StatusConfirming)
	}},

	{"list, groups, counts, search, detail", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		mk := func(amount, order string) core.PaymentView {
			e.Clock.Advance(time.Minute)
			p, _, err := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: amount, OrderID: order, CustomerEmail: order + "@example.com"})
			noErr(t, err)
			return p
		}
		pPaid, pOver, pOpen, pExpired := mk("10", "paid"), mk("20", "over"), mk("30", "open"), mk("5", "expired")
		paidTx := e.Pay(pPaid, "10", 100)
		e.Pay(pOver, "25", 100)
		e.Head(114)
		e.Expire(pExpired)
		e.Head(115)
		pFresh := mk("7", "fresh")
		wantStatus(t, e, pOpen.ID, core.StatusExpired) // same TTL as pExpired
		_ = pOpen

		ids := func(in core.ListPaymentsInput) []string {
			page, err := e.Service.ListPayments(e.System(), in)
			noErr(t, err)
			var out []string
			for _, p := range page.Items {
				out = append(out, p.OrderID)
			}
			return out
		}
		eq := func(got []string, want ...string) {
			t.Helper()
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
		eq(ids(core.ListPaymentsInput{}), "fresh", "expired", "open", "over", "paid")
		eq(ids(core.ListPaymentsInput{Sort: &core.PaymentSort{By: core.SortByAmount}}), "expired", "fresh", "paid", "over", "open")
		eq(ids(core.ListPaymentsInput{Filter: core.GroupPaid.Apply(core.PaymentFilter{})}), "paid")
		eq(ids(core.ListPaymentsInput{Filter: core.GroupAttention.Apply(core.PaymentFilter{})}), "over")
		eq(ids(core.ListPaymentsInput{Filter: core.GroupOpen.Apply(core.PaymentFilter{})}), "fresh")
		eq(ids(core.ListPaymentsInput{Filter: core.GroupExpired.Apply(core.PaymentFilter{})}), "expired", "open")
		eq(ids(core.ListPaymentsInput{Filter: core.PaymentFilter{Search: "  " + paidTx.TxHash[:20] + " "}}), "paid")
		eq(ids(core.ListPaymentsInput{Filter: core.PaymentFilter{Search: "OVER@EXAMPLE"}}), "over")
		eq(ids(core.ListPaymentsInput{Filter: core.PaymentFilter{Search: pFresh.Address}}), "fresh")
		eq(ids(core.ListPaymentsInput{Page: core.Page{Limit: 2, Offset: 1}}), "expired", "open")

		page, err := e.Service.ListPayments(e.System(), core.ListPaymentsInput{Page: core.Page{Limit: 2}})
		noErr(t, err)
		if page.Total != 5 || len(page.Items) != 2 {
			t.Fatalf("page: total %d, %d items", page.Total, len(page.Items))
		}
		counts, err := e.Service.CountPayments(e.System(), core.PaymentFilter{})
		noErr(t, err)
		if counts != (core.PaymentCounts{All: 5, Open: 1, Paid: 1, Attention: 1, Expired: 2}) {
			t.Fatalf("counts: %+v", counts)
		}
		_, err = e.Service.ListPayments(e.System(), core.ListPaymentsInput{Sort: &core.PaymentSort{By: "price"}})
		wantField(t, err, "sort")
		_, err = e.Service.ListPayments(e.System(), core.ListPaymentsInput{Filter: core.PaymentFilter{Statuses: []core.PaymentStatus{"nope"}}})
		wantField(t, err, "status")

		d, err := e.Service.GetPaymentDetail(e.System(), pPaid.ID)
		noErr(t, err)
		if d.Payment.Status != core.StatusPaid || len(d.Transfers) != 1 || d.Transfers[0].Status != core.TransferConfirmed ||
			d.Transfers[0].Amount != "10.00" || d.Transfers[0].Confirmations != 15 || len(d.Events) != 3 {
			t.Fatalf("detail: %+v", d)
		}
		_, err = e.Service.GetPayment(e.System(), "pay_missing")
		wantIs(t, err, core.ErrNotFound)

		// The public checkout view hides merchant-private data.
		co, err := e.Service.GetCheckout(context.Background(), pFresh.ID)
		noErr(t, err)
		raw, _ := json.Marshal(co)
		if co.Status != core.StatusAwaiting || co.Merchant.Name != "Northwind Digital" || co.AmountDue != "7.00" ||
			co.PaymentURI != "ethereum:"+core.USDTBSC.Contract+"@56/transfer?address="+pFresh.Address+"&uint256=7000000000000000000" ||
			strings.Contains(string(raw), "fresh@example.com") || strings.Contains(string(raw), "metadata") {
			t.Fatalf("checkout view: %s", raw)
		}
		_, err = e.Service.GetCheckout(context.Background(), "pay_guess")
		wantIs(t, err, core.ErrNotFound)
	}},

	{"overview", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		// Day 0 (today is Epoch's day): two created, one paid 100.
		a := e.CreatePayment("100")
		e.CreatePayment("1")
		e.Pay(a, "100", 100)
		e.Head(114)
		// Day 1: one created and overpaid (60 for 50); one created, unpaid.
		e.Clock.Advance(24 * time.Hour)
		b := e.CreatePayment("50")
		e.CreatePayment("2")
		e.Pay(b, "60", 200)
		e.Head(214)
		// Day 2: nothing. Day 3: one created, paid 40.
		e.Clock.Advance(48 * time.Hour)
		c := e.CreatePayment("40")
		e.Pay(c, "40", 300)
		e.Head(314)

		ov, err := e.Service.Overview(e.System(), 2)
		noErr(t, err)
		if ov.Days != 2 || ov.Asset != "USDT" || len(ov.Series) != 2 {
			t.Fatalf("overview shape: %+v", ov)
		}
		// Current period = days 2–3; previous = days 0–1.
		if ov.Current != (core.PeriodTotals{Volume: "40.00", Paid: 1, Created: 1, ConversionRate: 1, AverageTicket: "40.00"}) {
			t.Fatalf("current: %+v", ov.Current)
		}
		if ov.Previous != (core.PeriodTotals{Volume: "160.00", Paid: 2, Created: 4, ConversionRate: 0.5, AverageTicket: "80.00"}) {
			t.Fatalf("previous: %+v", ov.Previous)
		}
		day3 := Epoch.Add(72 * time.Hour).Format("2006-01-02")
		if ov.Series[0] != (core.DayPoint{Date: Epoch.Add(48 * time.Hour).Format("2006-01-02"), Volume: "0.00"}) ||
			ov.Series[1] != (core.DayPoint{Date: day3, Volume: "40.00", Paid: 1, Created: 1}) {
			t.Fatalf("series: %+v", ov.Series)
		}
		// Two unpaid payments are past their deadline by chain time.
		if ov.OpenPayments != 0 || ov.NeedsAttention != 1 {
			t.Fatalf("open %d, attention %d", ov.OpenPayments, ov.NeedsAttention)
		}
		ov, err = e.Service.Overview(e.System(), 0)
		noErr(t, err)
		if ov.Days != 30 || ov.Current.Volume != "200.00" || ov.Current.Paid != 3 || ov.Current.Created != 5 || ov.Previous.Paid != 0 || ov.Previous.AverageTicket != "0.00" {
			t.Fatalf("30-day overview: %+v", ov)
		}
		_, err = e.Service.Overview(e.System(), 1000)
		wantField(t, err, "days")
	}},

	{"events: order, cursor, nudge", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		sub := e.Service.Subscribe()
		defer sub.Close()
		select {
		case <-sub.C():
			t.Fatal("subscription fired before anything happened")
		default:
		}
		a := e.CreatePayment("1")
		select {
		case <-sub.C():
		case <-time.After(5 * time.Second):
			t.Fatal("subscription did not fire after an event was appended")
		}
		b := e.CreatePayment("2")
		e.Pay(a, "1", 100)
		e.Head(114)

		all, err := e.Service.Events(e.System(), core.EventFilter{Types: []core.EventType{created, confirming, paid}})
		noErr(t, err)
		if len(all) != 4 || all[0].PaymentID != a.ID || all[1].PaymentID != b.ID || all[2].Type != confirming || all[3].Type != paid {
			t.Fatalf("events: %+v", all)
		}
		for i := 1; i < len(all); i++ {
			if all[i].Seq <= all[i-1].Seq {
				t.Fatalf("events out of order: %+v", all)
			}
		}
		rest, err := e.Service.Events(e.System(), core.EventFilter{After: all[1].Seq, PaymentID: a.ID})
		noErr(t, err)
		if len(rest) != 2 || rest[0].ID != all[2].ID {
			t.Fatalf("events after cursor: %+v", rest)
		}
		// The envelope is the wire form.
		raw, _ := json.Marshal(all[3].Envelope())
		var env struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Data struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Amount string `json:"amount"`
			} `json:"data"`
		}
		noErr(t, json.Unmarshal(raw, &env))
		if env.ID != all[3].ID || env.Type != "payment.paid" || env.Data.ID != a.ID || env.Data.Status != "paid" || env.Data.Amount != "1.00" {
			t.Fatalf("envelope: %s", raw)
		}

		// Durable named cursors.
		seq, err := e.Service.Cursor(e.System(), "telegram")
		noErr(t, err)
		if seq != 0 {
			t.Fatalf("fresh cursor = %d", seq)
		}
		noErr(t, e.Service.SaveCursor(e.System(), "telegram", all[2].Seq))
		seq, err = e.Service.Cursor(e.System(), "telegram")
		noErr(t, err)
		if seq != all[2].Seq {
			t.Fatalf("cursor = %d, want %d", seq, all[2].Seq)
		}
		wantField(t, e.Service.SaveCursor(e.System(), "core.webhooks", 1), "consumer")
		// An API key sees payment events only; instance events are internal.
		reader, _ := e.APIKey(core.ScopePaymentsRead)
		mine, err := e.Service.Events(reader, core.EventFilter{})
		noErr(t, err)
		everything, err := e.Service.Events(e.System(), core.EventFilter{})
		noErr(t, err)
		if len(mine) != 4 || len(everything) != 5 || everything[0].Type != core.EventSettingsUpdated {
			t.Fatalf("API key sees %d events, system sees %d", len(mine), len(everything))
		}
		hidden, err := e.Service.Events(reader, core.EventFilter{Types: []core.EventType{core.EventSettingsUpdated}})
		noErr(t, err)
		if len(hidden) != 0 {
			t.Fatalf("API key read instance events: %+v", hidden)
		}
		// A failed use case appends nothing and wakes nobody.
		for len(sub.C()) > 0 {
			<-sub.C()
		}
		_, _, err = e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "bad"})
		wantKind(t, err, core.KindInvalid)
		select {
		case <-sub.C():
			t.Fatal("subscription fired although nothing was appended")
		default:
		}
	}},
}

var linkCases = []serviceCase{
	{"create, validate, update", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx := e.Operator()
		l, err := e.Service.CreateLink(ctx, core.LinkInput{
			Slug: "pro-annual", Name: " Pro plan — annual ", Description: "12 months", Amount: "149.00",
			MaxUses: 50, ExpiresAt: e.Clock.Now().Add(24 * time.Hour), RedirectURL: "https://example.com/welcome", Active: true,
		})
		noErr(t, err)
		if !strings.HasPrefix(l.ID, "plink_") || l.Slug != "pro-annual" || l.Name != "Pro plan — annual" || l.Amount == nil || *l.Amount != "149.00" ||
			l.Status != core.LinkActive || l.Uses != 0 || l.TotalCollected != "0.00" || l.MaxUses == nil || *l.MaxUses != 50 ||
			l.URL != "https://pay.example.com/l/pro-annual" || l.LastPaidAt != nil {
			t.Fatalf("link: %+v", l)
		}
		_, err = e.Service.CreateLink(ctx, core.LinkInput{Slug: "pro-annual", Name: "Other", Active: true})
		wantField(t, err, "slug")
		for field, in := range map[string]core.LinkInput{
			"name":         {Slug: "a"},
			"slug":         {Slug: "Not A Slug", Name: "x"},
			"amount":       {Slug: "b", Name: "x", Amount: "-1"},
			"max_uses":     {Slug: "c", Name: "x", MaxUses: -1},
			"redirect_url": {Slug: "d", Name: "x", RedirectURL: "nope"},
		} {
			_, err := e.Service.CreateLink(ctx, in)
			wantField(t, err, field)
		}
		auto, err := e.Service.CreateLink(ctx, core.LinkInput{Name: "Donate", Active: true})
		noErr(t, err)
		if auto.Slug == "" || auto.Amount != nil || auto.MaxUses != nil || auto.ExpiresAt != nil {
			t.Fatalf("open-amount link with generated slug: %+v", auto)
		}

		e.Clock.Advance(time.Minute)
		up, err := e.Service.UpdateLink(ctx, l.ID, core.LinkInput{Slug: "ignored", Name: "Pro", Amount: "99", Active: true})
		noErr(t, err)
		if up.Slug != "pro-annual" || up.Name != "Pro" || *up.Amount != "99.00" || up.MaxUses != nil || up.ExpiresAt != nil || up.RedirectURL != "" {
			t.Fatalf("updated link: %+v", up)
		}
		off, err := e.Service.SetLinkActive(ctx, l.ID, false)
		noErr(t, err)
		if off.Active || off.Status != core.LinkDisabled {
			t.Fatalf("disabled link: %+v", off)
		}
		list, err := e.Service.ListLinks(ctx)
		noErr(t, err)
		if len(list) != 2 {
			t.Fatalf("links: %+v", list)
		}
		got, err := e.Service.GetLink(ctx, l.ID)
		noErr(t, err)
		if got.Name != "Pro" {
			t.Fatalf("GetLink: %+v", got)
		}
		_, err = e.Service.GetLink(ctx, "plink_missing")
		wantIs(t, err, core.ErrNotFound)
	}},

	{"status derivation order: expired, limit, disabled, active", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		now := Epoch
		l := core.PaymentLink{Active: true}
		check := func(want core.LinkStatus) {
			t.Helper()
			if got := l.Status(now); got != want {
				t.Fatalf("status = %s, want %s (%+v)", got, want, l)
			}
		}
		check(core.LinkActive)
		l.Active = false
		check(core.LinkDisabled)
		l.MaxUses, l.Uses = 5, 5
		check(core.LinkLimit)
		l.ExpiresAt = now
		check(core.LinkExpired)
		l.Active = true
		check(core.LinkExpired)
		l.ExpiresAt = now.Add(time.Second)
		check(core.LinkLimit)
		l.Uses = 4
		check(core.LinkActive)
	}},

	{"paying through a fixed-price link", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		l, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Slug: "ticket", Name: "Conference ticket", Amount: "450", RedirectURL: "https://example.com/ok", Active: true})
		noErr(t, err)
		public := context.Background()
		page, err := e.Service.GetLinkCheckout(public, "ticket")
		noErr(t, err)
		if page.Name != "Conference ticket" || page.Amount == nil || *page.Amount != "450.00" || page.Status != core.LinkActive || page.Merchant.Name == "" {
			t.Fatalf("link page: %+v", page)
		}
		co, _, err := e.Service.CreatePaymentFromLink(public, "ticket", core.LinkPaymentInput{CustomerEmail: "ann@example.com", IdempotencyKey: "visit-1"})
		noErr(t, err)
		if co.Amount != "450.00" || co.Description != "Conference ticket" || co.RedirectURL != "https://example.com/ok" || co.Status != core.StatusAwaiting {
			t.Fatalf("checkout from link: %+v", co)
		}
		// A reload with the same key returns the same payment.
		again, _, err := e.Service.CreatePaymentFromLink(public, "ticket", core.LinkPaymentInput{CustomerEmail: "ann@example.com", IdempotencyKey: "visit-1"})
		noErr(t, err)
		if again.ID != co.ID {
			t.Fatal("link idempotency key must return the same payment")
		}
		_, _, err = e.Service.CreatePaymentFromLink(public, "ticket", core.LinkPaymentInput{Amount: "1"})
		wantField(t, err, "amount")
		_, _, err = e.Service.CreatePaymentFromLink(public, "ticket", core.LinkPaymentInput{Amount: "450.0"})
		noErr(t, err)
		_, _, err = e.Service.CreatePaymentFromLink(public, "nope", core.LinkPaymentInput{})
		wantIs(t, err, core.ErrNotFound)

		p := e.Payment(co.ID)
		if p.LinkID == nil || *p.LinkID != l.ID || p.CustomerEmail != "ann@example.com" {
			t.Fatalf("payment from link: %+v", p)
		}
		// Statistics follow settlement.
		e.Pay(p, "450", 100)
		e.Clock.Advance(time.Minute)
		e.Head(114)
		got, err := e.Service.GetLink(e.Operator(), l.ID)
		noErr(t, err)
		if got.Uses != 1 || got.TotalCollected != "450.00" || got.LastPaidAt == nil || !got.LastPaidAt.Equal(e.Clock.Now()) {
			t.Fatalf("link statistics: %+v", got)
		}
		// Late funds do not count as another use.
		e.Pay(p, "450", 200)
		e.Head(214)
		got, _ = e.Service.GetLink(e.Operator(), l.ID)
		if got.Uses != 1 || got.TotalCollected != "450.00" {
			t.Fatalf("link statistics after late funds: %+v", got)
		}
		page2, err := e.Service.ListPayments(e.System(), core.ListPaymentsInput{Filter: core.PaymentFilter{LinkID: l.ID}})
		noErr(t, err)
		if page2.Total != 2 {
			t.Fatalf("payments of the link: %d", page2.Total)
		}
	}},

	{"customer-chooses link", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		_, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Slug: "donate", Name: "Donate", Active: true})
		noErr(t, err)
		public := context.Background()
		page, err := e.Service.GetLinkCheckout(public, "donate")
		noErr(t, err)
		if page.Amount != nil {
			t.Fatalf("open link must have no amount: %+v", page)
		}
		_, _, err = e.Service.CreatePaymentFromLink(public, "donate", core.LinkPaymentInput{})
		wantField(t, err, "amount")
		_, _, err = e.Service.CreatePaymentFromLink(public, "donate", core.LinkPaymentInput{Amount: "0"})
		wantField(t, err, "amount")
		co, _, err := e.Service.CreatePaymentFromLink(public, "donate", core.LinkPaymentInput{Amount: "12.5"})
		noErr(t, err)
		if co.Amount != "12.50" {
			t.Fatalf("amount: %s", co.Amount)
		}
	}},

	{"disabled, expired and limited links refuse payments; slots are reserved", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx, public := e.Operator(), context.Background()
		off, err := e.Service.CreateLink(ctx, core.LinkInput{Slug: "off", Name: "Off", Amount: "1"})
		noErr(t, err)
		_, _, err = e.Service.CreatePaymentFromLink(public, off.Slug, core.LinkPaymentInput{})
		wantIs(t, err, core.ErrLinkDisabled)
		wantKind(t, err, core.KindConflict)

		_, err = e.Service.CreateLink(ctx, core.LinkInput{Slug: "soon", Name: "Soon over", Amount: "1", Active: true, ExpiresAt: e.Clock.Now().Add(time.Hour)})
		noErr(t, err)
		_, _, err = e.Service.CreatePaymentFromLink(public, "soon", core.LinkPaymentInput{})
		noErr(t, err)
		e.Clock.Advance(time.Hour)
		_, _, err = e.Service.CreatePaymentFromLink(public, "soon", core.LinkPaymentInput{})
		wantIs(t, err, core.ErrLinkExpired)
		page, _ := e.Service.GetLinkCheckout(public, "soon")
		if page.Status != core.LinkExpired {
			t.Fatalf("expired link page: %+v", page)
		}

		// Two tickets only.
		_, err = e.Service.CreateLink(ctx, core.LinkInput{Slug: "vip", Name: "VIP", Amount: "10", MaxUses: 2, Active: true})
		noErr(t, err)
		first, _, err := e.Service.CreatePaymentFromLink(public, "vip", core.LinkPaymentInput{IdempotencyKey: "a"})
		noErr(t, err)
		second, _, err := e.Service.CreatePaymentFromLink(public, "vip", core.LinkPaymentInput{})
		noErr(t, err)
		// Both slots are reserved by payments in flight.
		_, _, err = e.Service.CreatePaymentFromLink(public, "vip", core.LinkPaymentInput{})
		wantIs(t, err, core.ErrLinkLimitReached)
		page, _ = e.Service.GetLinkCheckout(public, "vip")
		if page.Status != core.LinkLimit {
			t.Fatalf("fully reserved link page: %+v", page)
		}
		// … but a customer who already holds a slot gets their payment back.
		again, _, err := e.Service.CreatePaymentFromLink(public, "vip", core.LinkPaymentInput{IdempotencyKey: "a"})
		noErr(t, err)
		if again.ID != first.ID {
			t.Fatal("idempotent retry on a full link must return the held payment")
		}
		// The first pays; the second lets the invoice expire, freeing a slot.
		e.Pay(e.Payment(first.ID), "10", 100)
		e.Head(114)
		e.Expire(e.Payment(second.ID))
		e.Head(115)
		wantStatus(t, e, second.ID, core.StatusExpired)
		third, _, err := e.Service.CreatePaymentFromLink(public, "vip", core.LinkPaymentInput{})
		noErr(t, err)
		e.Pay(e.Payment(third.ID), "10", 200)
		e.Head(214)
		_, _, err = e.Service.CreatePaymentFromLink(public, "vip", core.LinkPaymentInput{})
		wantIs(t, err, core.ErrLinkLimitReached)
		got, err := e.Service.GetLink(ctx, *e.Payment(third.ID).LinkID)
		noErr(t, err)
		if got.Status != core.LinkLimit || got.Uses != 2 || got.TotalCollected != "20.00" {
			t.Fatalf("sold-out link: %+v", got)
		}
	}},
}

// complete reports the result of job under the job's claim.
func complete(t *testing.T, e *Env, job core.DeliveryJob, res core.AttemptResult) core.CompletionOutcome {
	t.Helper()
	res.ClaimID, res.Attempt = job.ClaimID, job.Attempt
	out, err := e.Service.CompleteWebhookDelivery(e.System(), job.DeliveryID, res)
	noErr(t, err)
	return out
}

var webhookCases = []serviceCase{
	{"endpoint management", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx := e.Operator()
		ep, err := e.Service.CreateWebhookEndpoint(ctx, core.WebhookEndpointInput{
			URL: "https://shop.example.com/hook", Description: "Production",
			Events: []core.EventType{paid, expired, paid}, Enabled: true,
		})
		noErr(t, err)
		if !strings.HasPrefix(ep.ID, "we_") || !strings.HasPrefix(ep.Secret, "whsec_") || len(ep.Secret) != 38 || len(ep.Events) != 2 || !ep.Enabled {
			t.Fatalf("endpoint: %+v", ep)
		}
		for field, in := range map[string]core.WebhookEndpointInput{
			"url":    {URL: "ftp://example.com", Events: []core.EventType{paid}},
			"events": {URL: "https://example.com"},
		} {
			_, err := e.Service.CreateWebhookEndpoint(ctx, in)
			wantField(t, err, field)
		}
		_, err = e.Service.CreateWebhookEndpoint(ctx, core.WebhookEndpointInput{URL: "https://example.com", Events: []core.EventType{"settings.updated"}})
		wantField(t, err, "events")

		up, err := e.Service.UpdateWebhookEndpoint(ctx, ep.ID, core.WebhookEndpointInput{URL: "http://localhost:3000/hook", Events: []core.EventType{created}, Enabled: false})
		noErr(t, err)
		if up.URL != "http://localhost:3000/hook" || up.Enabled || up.DisabledReason != "manual" || up.Secret != ep.Secret {
			t.Fatalf("updated endpoint: %+v", up)
		}
		list, err := e.Service.ListWebhookEndpoints(ctx)
		noErr(t, err)
		if len(list) != 1 || list[0].ID != ep.ID {
			t.Fatalf("endpoints: %+v", list)
		}
		_, err = e.Service.SendTestWebhook(ctx, ep.ID)
		wantIs(t, err, core.ErrEndpointDisabled)
		noErr(t, e.Service.DeleteWebhookEndpoint(ctx, ep.ID))
		_, err = e.Service.GetWebhookEndpoint(ctx, ep.ID)
		wantIs(t, err, core.ErrNotFound)
		wantIs(t, e.Service.DeleteWebhookEndpoint(ctx, ep.ID), core.ErrNotFound)
	}},

	{"fan-out, claim, sign, complete", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx, sys := e.Operator(), e.System()
		before := e.CreatePayment("1") // predates every endpoint
		e.Clock.Advance(time.Second)
		all, err := e.Service.CreateWebhookEndpoint(ctx, core.WebhookEndpointInput{URL: "https://a.example.com/hook", Events: []core.EventType{created, confirming, paid}, Enabled: true})
		noErr(t, err)
		onlyPaid, err := e.Service.CreateWebhookEndpoint(ctx, core.WebhookEndpointInput{URL: "https://b.example.com/hook", Events: []core.EventType{paid}, Enabled: true})
		noErr(t, err)
		_, err = e.Service.CreateWebhookEndpoint(ctx, core.WebhookEndpointInput{URL: "https://c.example.com/hook", Events: []core.EventType{paid}, Enabled: false})
		noErr(t, err)

		p := e.CreatePayment("149")
		e.Pay(p, "149", 100)
		e.Head(114)
		_ = before

		n, err := e.Service.PlanWebhookDeliveries(sys)
		noErr(t, err)
		if n != 4 { // created, confirming, paid → A; paid → B; nothing for the old payment or the disabled endpoint
			t.Fatalf("planned %d deliveries, want 4", n)
		}
		n, err = e.Service.PlanWebhookDeliveries(sys)
		noErr(t, err)
		if n != 0 {
			t.Fatalf("second plan created %d deliveries, want 0", n)
		}

		// First attempts to one endpoint go out in event order, one at a
		// time: A gets payment.created now, B its payment.paid.
		jobs, err := e.Service.ClaimWebhookDeliveries(sys, 10)
		noErr(t, err)
		if len(jobs) != 2 {
			t.Fatalf("claimed %d jobs, want 2 (the first event of each endpoint)", len(jobs))
		}
		events, err := e.Service.Events(sys, core.EventFilter{PaymentID: p.ID})
		noErr(t, err)
		seqOf := map[string]int64{}
		for _, ev := range events {
			seqOf[ev.ID] = ev.Seq
		}
		none, err := e.Service.ClaimWebhookDeliveries(sys, 10)
		noErr(t, err)
		if len(none) != 0 {
			t.Fatalf("leased deliveries were claimed twice: %d", len(none))
		}
		secrets := map[string]string{all.ID: all.Secret, onlyPaid.ID: onlyPaid.Secret}
		urls := map[string]string{all.ID: all.URL, onlyPaid.ID: onlyPaid.URL}
		var paidJob core.DeliveryJob
		for _, job := range jobs {
			if job.URL != urls[job.EndpointID] || job.Attempt != 1 || job.Timeout != core.WebhookTimeout ||
				job.Headers["Content-Type"] != "application/json" || job.Headers[core.HeaderDeliveryID] != job.DeliveryID {
				t.Fatalf("job: %+v", job)
			}
			noErr(t, core.VerifyWebhookSignature(job.Headers[core.HeaderSignature], job.Body, []string{secrets[job.EndpointID]}, e.Clock.Now(), 0))
			if err := core.VerifyWebhookSignature(job.Headers[core.HeaderSignature], job.Body, []string{"whsec_wrong"}, e.Clock.Now(), 0); err == nil {
				t.Fatal("signature verified with the wrong secret")
			}
			var env core.EventEnvelope
			noErr(t, json.Unmarshal(job.Body, &env))
			if env.ID != job.Headers[core.HeaderEventID] || !env.Type.IsPayment() || env.Seq == 0 || env.Seq != seqOf[env.ID] {
				t.Fatalf("job body: %s", job.Body)
			}
			if job.EndpointID == all.ID && env.Type != created {
				t.Fatalf("endpoint A's first job is %s, not its first event", env.Type)
			}
			if env.Type == paid && job.EndpointID == onlyPaid.ID {
				paidJob = job
				var view core.PaymentView
				noErr(t, json.Unmarshal(env.Data, &view))
				if view.ID != p.ID || view.Status != core.StatusPaid || view.AmountReceived != "149.00" {
					t.Fatalf("payment in webhook body: %+v", view)
				}
			}
		}

		// Success.
		e.Clock.Advance(time.Second)
		if paidJob.ClaimID == "" || !paidJob.LeaseExpiresAt.Equal(e.Clock.Now().Add(core.WebhookClaimLease-time.Second)) {
			t.Fatalf("job claim: %+v", paidJob)
		}
		if out := complete(t, e, paidJob, core.AttemptResult{ResponseCode: 204, Duration: 120 * time.Millisecond, ResponseBody: "ok"}); out != core.CompletionCounted {
			t.Fatalf("outcome = %s, want counted", out)
		}
		d, err := e.Service.GetWebhookDelivery(ctx, paidJob.DeliveryID)
		noErr(t, err)
		if d.Status != core.DeliverySucceeded || d.Attempt != 1 || d.ResponseCode != 204 || d.DurationMs != 120 || !d.NextAttemptAt.IsZero() || d.ClaimID != "" ||
			d.CompletedAt.IsZero() || len(d.Attempts) != 1 || !d.Attempts[0].Succeeded || d.Attempts[0].Late || d.PaymentID != p.ID || d.EventType != paid {
			t.Fatalf("succeeded delivery: %+v", d)
		}
		// A duplicate report under the same, now spent, claim is not counted
		// again and cannot overwrite the result; it is only noted.
		if out := complete(t, e, paidJob, core.AttemptResult{ResponseCode: 500}); out != core.CompletionLate {
			t.Fatalf("outcome of a duplicate report = %s, want late", out)
		}
		d, _ = e.Service.GetWebhookDelivery(ctx, paidJob.DeliveryID)
		if d.Status != core.DeliverySucceeded || d.Attempt != 1 || d.ResponseCode != 204 || len(d.Attempts) != 2 || !d.Attempts[1].Late {
			t.Fatalf("duplicate completion changed the delivery: %+v", d)
		}
		// A result must name its claim.
		_, err = e.Service.CompleteWebhookDelivery(sys, paidJob.DeliveryID, core.AttemptResult{ResponseCode: 200})
		wantField(t, err, "claim_id")
		_, err = e.Service.CompleteWebhookDelivery(sys, "whd_missing", paidJob.Result())
		wantIs(t, err, core.ErrNotFound)
		sum, err := e.Service.GetWebhookEndpoint(ctx, onlyPaid.ID)
		noErr(t, err)
		if sum.Succeeded7d != 1 || sum.Failed7d != 0 || sum.LastSuccessAt.IsZero() || sum.ConsecutiveFailures != 0 {
			t.Fatalf("endpoint summary: %+v", sum)
		}

		// A worker that dies: its jobs come back after the lease — and
		// A's later events still wait for its first one.
		e.Clock.Advance(core.WebhookClaimLease)
		jobs, err = e.Service.ClaimWebhookDeliveries(sys, 10)
		noErr(t, err)
		if len(jobs) != 1 || jobs[0].Attempt != 1 || jobs[0].EndpointID != all.ID {
			t.Fatalf("after the lease: %d jobs", len(jobs))
		}
		// Each finished first attempt releases the next event's, in order;
		// a failed one holds nothing up.
		var order []core.EventType
		for i, code := range []int{200, 500, 200} {
			var env core.EventEnvelope
			noErr(t, json.Unmarshal(jobs[0].Body, &env))
			order = append(order, env.Type)
			complete(t, e, jobs[0], core.AttemptResult{ResponseCode: code})
			jobs, err = e.Service.ClaimWebhookDeliveries(sys, 10)
			noErr(t, err)
			if want := []int{1, 1, 0}[i]; len(jobs) != want {
				t.Fatalf("after A's first attempt of %s: %d jobs, want %d", env.Type, len(jobs), want)
			}
		}
		if fmt.Sprint(order) != fmt.Sprint([]core.EventType{created, confirming, paid}) {
			t.Fatalf("first attempts to A in the order %v", order)
		}
		detail, err := e.Service.GetPaymentDetail(sys, p.ID)
		noErr(t, err)
		if len(detail.Deliveries) != 4 {
			t.Fatalf("deliveries on the payment detail: %d", len(detail.Deliveries))
		}
		list, err := e.Service.ListWebhookDeliveries(ctx, core.DeliveryFilter{EndpointID: all.ID}, core.Page{Limit: 2})
		noErr(t, err)
		if list.Total != 3 || len(list.Items) != 2 {
			t.Fatalf("delivery list: total %d, %d items", list.Total, len(list.Items))
		}
	}},

	{"retry schedule, exhaustion and resend", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx, sys := e.Operator(), e.System()
		ep, err := e.Service.CreateWebhookEndpoint(ctx, core.WebhookEndpointInput{URL: "https://a.example.com/hook", Events: []core.EventType{created}, Enabled: true})
		noErr(t, err)
		e.CreatePayment("1")
		_, err = e.Service.PlanWebhookDeliveries(sys)
		noErr(t, err)

		waits := []time.Duration{1, 5, 15, 60, 180, 360, 720}
		var id, eventID string
		for attempt := 1; attempt <= core.WebhookMaxAttempts; attempt++ {
			jobs, err := e.Service.ClaimWebhookDeliveries(sys, 10)
			noErr(t, err)
			if len(jobs) != 1 || jobs[0].Attempt != attempt {
				t.Fatalf("attempt %d: claimed %+v", attempt, jobs)
			}
			if id == "" {
				id, eventID = jobs[0].DeliveryID, jobs[0].Headers[core.HeaderEventID]
			}
			if jobs[0].Headers[core.HeaderEventID] != eventID {
				t.Fatal("event id changed between attempts")
			}
			complete(t, e, jobs[0], core.AttemptResult{ResponseCode: 503, ResponseBody: strings.Repeat("x", 10_000), Duration: time.Second})
			d, err := e.Service.GetWebhookDelivery(ctx, id)
			noErr(t, err)
			if attempt == core.WebhookMaxAttempts {
				if d.Status != core.DeliveryFailed || !d.NextAttemptAt.IsZero() || d.CompletedAt.IsZero() || len(d.Attempts) != 8 {
					t.Fatalf("exhausted delivery: %+v", d)
				}
				break
			}
			want := e.Clock.Now().Add(waits[attempt-1] * time.Minute)
			if d.Status != core.DeliveryRetrying || !d.NextAttemptAt.Equal(want) || d.Attempt != attempt || len(d.ResponseBody) != core.WebhookMaxResponseBody {
				t.Fatalf("after attempt %d: status %s next %v (want %v) body %d", attempt, d.Status, d.NextAttemptAt, want, len(d.ResponseBody))
			}
			// Not due a second early.
			e.Clock.Set(want.Add(-time.Second))
			early, _ := e.Service.ClaimWebhookDeliveries(sys, 10)
			if len(early) != 0 {
				t.Fatalf("delivery claimed before it was due")
			}
			e.Clock.Set(want)
		}
		sum, _ := e.Service.GetWebhookEndpoint(ctx, ep.ID)
		if sum.ConsecutiveFailures != 8 || sum.FailingSince.IsZero() || sum.Failed7d != 1 || !sum.Enabled {
			t.Fatalf("failing endpoint: %+v", sum)
		}
		if !sum.DisableAt().Equal(sum.FailingSince.Add(72 * time.Hour)) {
			t.Fatalf("DisableAt: %v", sum.DisableAt())
		}

		// Resend: one more attempt, same event id.
		d, err := e.Service.ResendWebhookDelivery(ctx, id)
		noErr(t, err)
		if d.Status != core.DeliveryPending || d.MaxAttempts != 9 {
			t.Fatalf("resent delivery: %+v", d)
		}
		jobs, err := e.Service.ClaimWebhookDeliveries(sys, 10)
		noErr(t, err)
		if len(jobs) != 1 || jobs[0].Attempt != 9 || jobs[0].Headers[core.HeaderEventID] != eventID {
			t.Fatalf("resend job: %+v", jobs)
		}
		complete(t, e, jobs[0], core.AttemptResult{Error: "dial tcp: connection refused", Duration: 10 * time.Second})
		d, _ = e.Service.GetWebhookDelivery(ctx, id)
		if d.Status != core.DeliveryFailed || d.Attempt != 9 || d.ResponseCode != 0 || d.Error == "" {
			t.Fatalf("failed resend: %+v", d)
		}
		_, err = e.Service.ResendWebhookDelivery(ctx, id)
		noErr(t, err)
		jobs, _ = e.Service.ClaimWebhookDeliveries(sys, 10)
		complete(t, e, jobs[0], core.AttemptResult{ResponseCode: 200})
		d, _ = e.Service.GetWebhookDelivery(ctx, id)
		sum, _ = e.Service.GetWebhookEndpoint(ctx, ep.ID)
		if d.Status != core.DeliverySucceeded || d.Attempt != 10 || sum.ConsecutiveFailures != 0 || !sum.FailingSince.IsZero() {
			t.Fatalf("successful resend: %+v / %+v", d, sum)
		}
		_, err = e.Service.ResendWebhookDelivery(ctx, "whd_missing")
		wantIs(t, err, core.ErrNotFound)
	}},

	{"secret rotation overlap", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx, sys := e.Operator(), e.System()
		ep, err := e.Service.CreateWebhookEndpoint(ctx, core.WebhookEndpointInput{URL: "https://a.example.com/hook", Events: []core.EventType{created}, Enabled: true})
		noErr(t, err)
		rotated, err := e.Service.RotateWebhookSecret(ctx, ep.ID)
		noErr(t, err)
		if rotated.Secret == ep.Secret || rotated.PreviousSecret != ep.Secret || !rotated.PreviousSecretExpiresAt.Equal(e.Clock.Now().Add(24*time.Hour)) {
			t.Fatalf("rotated endpoint: %+v", rotated)
		}
		claim := func() core.DeliveryJob {
			t.Helper()
			e.CreatePayment("1")
			_, err := e.Service.PlanWebhookDeliveries(sys)
			noErr(t, err)
			jobs, err := e.Service.ClaimWebhookDeliveries(sys, 1)
			noErr(t, err)
			if len(jobs) != 1 {
				t.Fatalf("claimed %d jobs", len(jobs))
			}
			complete(t, e, jobs[0], core.AttemptResult{ResponseCode: 200})
			return jobs[0]
		}
		verify := func(job core.DeliveryJob, secret string) error {
			return core.VerifyWebhookSignature(job.Headers[core.HeaderSignature], job.Body, []string{secret}, e.Clock.Now(), 0)
		}
		during := claim()
		noErr(t, verify(during, rotated.Secret))
		noErr(t, verify(during, ep.Secret))
		e.Clock.Advance(24 * time.Hour)
		after := claim()
		noErr(t, verify(after, rotated.Secret))
		if verify(after, ep.Secret) == nil {
			t.Fatal("the previous secret still signs after the overlap")
		}
	}},

	{"auto-disable after 72 hours of continuous failure", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx, sys := e.Operator(), e.System()
		ep, err := e.Service.CreateWebhookEndpoint(ctx, core.WebhookEndpointInput{URL: "https://a.example.com/hook", Events: []core.EventType{created}, Enabled: true})
		noErr(t, err)
		fail := func() {
			t.Helper()
			_, err := e.Service.PlanWebhookDeliveries(sys)
			noErr(t, err)
			jobs, err := e.Service.ClaimWebhookDeliveries(sys, 100)
			noErr(t, err)
			for _, job := range jobs {
				complete(t, e, job, core.AttemptResult{ResponseCode: 500})
			}
		}
		e.CreatePayment("1")
		fail()
		e.Clock.Advance(71 * time.Hour)
		e.CreatePayment("2")
		fail()
		sum, _ := e.Service.GetWebhookEndpoint(ctx, ep.ID)
		if !sum.Enabled {
			t.Fatal("endpoint disabled before 72 hours")
		}
		e.Clock.Advance(time.Hour)
		e.CreatePayment("3")
		fail()
		sum, _ = e.Service.GetWebhookEndpoint(ctx, ep.ID)
		if sum.Enabled || sum.DisabledReason != "failing" || sum.DisabledAt.IsZero() {
			t.Fatalf("endpoint must be auto-disabled: %+v", sum)
		}
		open, err := e.Service.ListWebhookDeliveries(ctx, core.DeliveryFilter{Statuses: []core.DeliveryStatus{core.DeliveryPending, core.DeliveryRetrying}}, core.Page{})
		noErr(t, err)
		if open.Total != 0 {
			t.Fatalf("%d deliveries still scheduled for a disabled endpoint", open.Total)
		}
		events, err := e.Service.Events(sys, core.EventFilter{Types: []core.EventType{core.EventWebhookEndpointDisabled}})
		noErr(t, err)
		var data core.EndpointDisabledData
		if len(events) != 1 || json.Unmarshal(events[0].Data, &data) != nil || data.EndpointID != ep.ID || data.Reason != "failing" {
			t.Fatalf("disabled event: %+v", events)
		}
		// Nothing new is planned for a disabled endpoint.
		e.CreatePayment("4")
		n, err := e.Service.PlanWebhookDeliveries(sys)
		noErr(t, err)
		if n != 0 {
			t.Fatalf("planned %d deliveries for a disabled endpoint", n)
		}
		// Re-enabling clears the streak; a single success in between would
		// have reset it too. (Three days on, the session this test started
		// with has idled out, and a session that no longer works cannot
		// write: the operator logs in again.)
		ctx = e.Relogin()
		up, err := e.Service.UpdateWebhookEndpoint(ctx, ep.ID, core.WebhookEndpointInput{URL: ep.URL, Events: ep.Events, Enabled: true})
		noErr(t, err)
		if !up.Enabled || !up.FailingSince.IsZero() || up.ConsecutiveFailures != 0 || up.DisabledReason != "" {
			t.Fatalf("re-enabled endpoint: %+v", up)
		}
	}},

	{"a success resets the failure streak", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx, sys := e.Operator(), e.System()
		ep, err := e.Service.CreateWebhookEndpoint(ctx, core.WebhookEndpointInput{URL: "https://a.example.com/hook", Events: []core.EventType{created}, Enabled: true})
		noErr(t, err)
		round := func(code int) {
			t.Helper()
			e.CreatePayment("1")
			_, err := e.Service.PlanWebhookDeliveries(sys)
			noErr(t, err)
			jobs, err := e.Service.ClaimWebhookDeliveries(sys, 1)
			noErr(t, err)
			complete(t, e, jobs[0], core.AttemptResult{ResponseCode: code})
		}
		round(500)
		e.Clock.Advance(60 * time.Hour)
		round(200)
		e.Clock.Advance(60 * time.Hour)
		round(500)
		sum, _ := e.Service.GetWebhookEndpoint(ctx, ep.ID)
		if !sum.Enabled || sum.ConsecutiveFailures != 1 {
			t.Fatalf("endpoint: %+v", sum)
		}
	}},

	{"disabling an endpoint fails what is outstanding; test events", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx, sys := e.Operator(), e.System()
		ep, err := e.Service.CreateWebhookEndpoint(ctx, core.WebhookEndpointInput{URL: "https://a.example.com/hook", Events: []core.EventType{created}, Enabled: true})
		noErr(t, err)

		test, err := e.Service.SendTestWebhook(ctx, ep.ID)
		noErr(t, err)
		if test.EventType != core.EventWebhookTest || test.MaxAttempts != 1 || test.Status != core.DeliveryPending || !strings.Contains(test.RequestBody, `"type":"webhook.test"`) {
			t.Fatalf("test delivery: %+v", test)
		}
		jobs, err := e.Service.ClaimWebhookDeliveries(sys, 10)
		noErr(t, err)
		if len(jobs) != 1 || jobs[0].DeliveryID != test.ID {
			t.Fatalf("test job: %+v", jobs)
		}
		complete(t, e, jobs[0], core.AttemptResult{ResponseCode: 500})
		d, _ := e.Service.GetWebhookDelivery(ctx, test.ID)
		if d.Status != core.DeliveryFailed {
			t.Fatalf("a failed test event must not be retried: %+v", d)
		}
		// The test event is not part of the log.
		events, _ := e.Service.Events(sys, core.EventFilter{Types: []core.EventType{core.EventWebhookTest}})
		if len(events) != 0 {
			t.Fatalf("test event leaked into the log: %+v", events)
		}

		e.Clock.Advance(time.Second)
		e.CreatePayment("1")
		_, err = e.Service.PlanWebhookDeliveries(sys)
		noErr(t, err)
		_, err = e.Service.UpdateWebhookEndpoint(ctx, ep.ID, core.WebhookEndpointInput{URL: ep.URL, Events: ep.Events, Enabled: false})
		noErr(t, err)
		jobs, _ = e.Service.ClaimWebhookDeliveries(sys, 10)
		if len(jobs) != 0 {
			t.Fatalf("claimed %d jobs for a disabled endpoint", len(jobs))
		}
		list, _ := e.Service.ListWebhookDeliveries(ctx, core.DeliveryFilter{Statuses: []core.DeliveryStatus{core.DeliveryFailed}}, core.Page{})
		if list.Total != 2 || list.Items[0].Error != "endpoint disabled" {
			t.Fatalf("deliveries after disabling: %+v", list)
		}
		_, err = e.Service.ResendWebhookDelivery(ctx, list.Items[0].ID)
		wantIs(t, err, core.ErrEndpointDisabled)

		// Retention.
		e.Clock.Advance(core.WebhookRetention + time.Hour)
		res, err := e.Service.Prune(sys)
		noErr(t, err)
		if res.Deliveries != 2 || res.Sessions != 1 {
			t.Fatalf("prune: %+v", res)
		}
	}},
}
