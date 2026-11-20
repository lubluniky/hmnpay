package core_test

// A payment link where the customer chooses the amount has a minimum, so
// that a use of a limited link cannot be had for one base unit: the attack,
// and the tests of the rule.

import (
	"context"
	"errors"
	"testing"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// fieldError returns the field error of err for the given field, if any.
func fieldError(err error, field string) (core.FieldError, bool) {
	var ce *core.Error
	if !errors.As(err, &ce) {
		return core.FieldError{}, false
	}
	for _, f := range ce.Fields {
		if f.Field == field {
			return f, true
		}
	}
	return core.FieldError{}, false
}

// A customer-chooses link had no minimum amount, and a use of a
// limited link is "a paid payment", whatever was paid: anybody could use up
// every use of such a link for one base unit each (10^-18 USDT plus the
// network fee), each payment emitting payment.paid, and uses that are counted
// are never released.
//
// The rule (one could also have created such checkouts and not counted
// them; the rule is a minimum instead, so the assertion is about that): an
// amount the customer chooses is at least the link's minimum
// — one whole unit unless the link sets more — and a checkout below it is
// not created. A use therefore costs at least the minimum less the capped
// tolerance, paid to the merchant.
func TestLimitedCustomerAmountLinkIsNotUsedUpForOneBaseUnitEach(t *testing.T) {
	const maxUses = 3
	e := coretest.NewEnv(t)
	requireOneConfirmation(t, e)
	e.Head(100)
	link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "First 3 supporters", MaxUses: maxUses, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if link.MinAmount == nil || *link.MinAmount != "1.00" {
		t.Fatalf("the link's minimum is not shown: %+v", link.MinAmount)
	}
	public, err := e.Service.GetLinkCheckout(context.Background(), link.Slug)
	if err != nil || public.MinAmount == nil || *public.MinAmount != "1.00" {
		t.Fatalf("the public link view does not state the minimum: %+v, %v", public.MinAmount, err)
	}

	for _, amount := range []string{"0.000000000000000001", "0.50", "0.999999999999999999"} {
		_, _, err := e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: amount})
		f, ok := fieldError(err, "amount")
		if core.KindOf(err) != core.KindInvalid || !ok || f.Code != "too_small" || f.Message != "the minimum amount is 1.00 USDT" {
			t.Fatalf("a checkout for %s: err = %v (field error %+v); want a validation error on amount: too_small, the minimum amount is 1.00 USDT", amount, err, f)
		}
	}
	// One base unit sent to a checkout for the minimum buys nothing either.
	co, _, err := e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "1"})
	if err != nil {
		t.Fatal(err)
	}
	e.Pay(e.Payment(co.ID), "0.000000000000000001", 101)
	e.Head(101)
	if p := e.Payment(co.ID); p.Status != core.StatusAwaiting {
		t.Fatalf("one base unit on a checkout for 1.00: status %s", p.Status)
	}
	for _, typ := range e.EventTypes(co.ID) {
		if typ == core.EventPaymentPaid {
			t.Fatal("payment.paid for one base unit")
		}
	}
	lv, err := e.Service.GetLink(e.Operator(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "25"})
	t.Logf("link: uses %d of %d, total collected %s; a customer who wants to pay 25: %v", lv.Uses, maxUses, lv.TotalCollected, err)
	if lv.Uses != 0 || err != nil {
		t.Fatalf("uses %d of %d taken without a payment of the minimum; a real customer gets: %v", lv.Uses, maxUses, err)
	}
}

// The link's own minimum, and where a minimum does not apply.
func TestLinkMinimumAmount(t *testing.T) {
	e := coretest.NewEnv(t)
	requireOneConfirmation(t, e)
	e.Head(100)
	op := e.Operator()
	public := context.Background()

	// Below one unit is not a minimum the merchant can set ...
	for _, low := range []string{"0.99", "0.000000000000000001"} {
		_, err := e.Service.CreateLink(op, core.LinkInput{Name: "Tips", MinAmount: low, Active: true})
		if f, ok := fieldError(err, "min_amount"); !ok || f.Code != "too_small" {
			t.Fatalf("min_amount %s: err = %v, want a too_small validation error on min_amount", low, err)
		}
	}
	// ... nor is "0" a way to switch it off, nor does a fixed price have one.
	if _, err := e.Service.CreateLink(op, core.LinkInput{Name: "Tips", MinAmount: "0", Active: true}); core.KindOf(err) != core.KindInvalid {
		t.Fatalf("min_amount 0: err = %v", err)
	}
	if _, err := e.Service.CreateLink(op, core.LinkInput{Name: "Ticket", Amount: "25", MinAmount: "5", Active: true}); core.KindOf(err) != core.KindInvalid {
		t.Fatalf("min_amount on a fixed-price link: err = %v", err)
	}

	link, err := e.Service.CreateLink(op, core.LinkInput{Name: "Donate", MinAmount: "5", MaxUses: 2, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if link.MinAmount == nil || *link.MinAmount != "5.00" || link.Amount != nil {
		t.Fatalf("link view: min %v amount %v", link.MinAmount, link.Amount)
	}
	_, _, err = e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{Amount: "4.99"})
	if f, ok := fieldError(err, "amount"); !ok || f.Code != "too_small" || f.Message != "the minimum amount is 5.00 USDT" {
		t.Fatalf("4.99 on a link with a minimum of 5: %v", err)
	}
	co, _, err := e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{Amount: "5", IdempotencyKey: "visit-1"})
	if err != nil {
		t.Fatal(err)
	}
	// The minimum can be raised; a checkout that exists is not taken back,
	// and its idempotent retry still returns it.
	if _, err := e.Service.UpdateLink(op, link.ID, core.LinkInput{Name: "Donate", MinAmount: "10", MaxUses: 2, Active: true}); err != nil {
		t.Fatal(err)
	}
	again, created, err := e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{Amount: "5", IdempotencyKey: "visit-1"})
	if err != nil || created || again.ID != co.ID {
		t.Fatalf("idempotent retry after the minimum was raised: %v (created %v)", err, created)
	}
	if _, _, err := e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{Amount: "5"}); core.KindOf(err) != core.KindInvalid {
		t.Fatalf("5 on a link with a minimum of 10: %v", err)
	}
	// Removing the link's own minimum brings back the default, not zero.
	back, err := e.Service.UpdateLink(op, link.ID, core.LinkInput{Name: "Donate", MaxUses: 2, Active: true})
	if err != nil || back.MinAmount == nil || *back.MinAmount != "1.00" {
		t.Fatalf("default minimum: %+v, %v", back.MinAmount, err)
	}

	// A fixed price is the merchant's own choice, however small: no minimum
	// applies, and the views show none.
	micro, err := e.Service.CreateLink(op, core.LinkInput{Name: "Micro", Amount: "0.10", Active: true})
	if err != nil || micro.MinAmount != nil {
		t.Fatalf("fixed-price link: min %v, %v", micro.MinAmount, err)
	}
	if v, err := e.Service.GetLinkCheckout(public, micro.Slug); err != nil || v.MinAmount != nil {
		t.Fatalf("fixed-price link, public view: min %v, %v", v.MinAmount, err)
	}
	if _, _, err := e.Service.CreatePaymentFromLink(public, micro.Slug, core.LinkPaymentInput{}); err != nil {
		t.Fatalf("checkout of a fixed-price link below one unit: %v", err)
	}
}
