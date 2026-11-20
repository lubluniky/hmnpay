package core_test

// Regression tests for two ways to abuse a limited payment link: overselling
// it by timing checkouts, and holding its uses (and the watch list) with
// dust. The same rules are pinned in more detail, and for every store, by
// the "Security" group of coretest.RunServiceSuite and by TestEvaluate.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

func requireOneConfirmation(t *testing.T, e *coretest.Env) {
	t.Helper()
	if _, err := e.Service.UpdatePaymentDefaults(e.Operator(), core.PaymentSettings{
		TTL: core.DefaultPaymentTTL, RequiredConfirmations: 1, UnderpaymentTolerance: "0",
	}); err != nil {
		t.Fatal(err)
	}
}

// A limited link used to free every slot held by
// an unfunded checkout after the 10-minute reservation while the checkout
// itself stayed payable for the payment TTL: one buyer who opened MaxUses
// checkouts every 10 minutes and paid them all got ceil(TTL / reservation)
// x MaxUses uses.
//
// The rule now: a checkout of a limited link lives for the reservation
// window and holds its use until it closes; money sent to it afterwards is
// late funds on an expired payment, never a use. The buyer of that attack
// ends up with no paid payment at all, six payments
// flagged for the merchant, and a link that still has both its uses.
//
// The subtests are the two ways time can pass — heads keep arriving (the
// checkouts expire on schedule), or the chain watcher is stalled (no head
// is processed between the rounds of checkouts) —
// times the two ways a transfer can be dated: by the block time the chain
// adapter reports, or, when it reports none, by the heads the core has
// processed. In the one combination where nothing dates the transfers (a
// stalled watcher that reports no block times) the payments get the benefit
// of the doubt and are paid — but then their checkouts never gave up their
// uses either, so the link is full, not oversold.
func TestLimitedLinkIsNeverOversold(t *testing.T) {
	const maxUses = 2
	type variant struct {
		name              string
		heads, blockTimes bool
		wantPaid          int
	}
	for _, v := range []variant{
		{"heads keep arriving, block times reported", true, true, 0},
		{"heads keep arriving, no block times", true, false, 0},
		{"the watcher is stalled, block times reported", false, true, 0},
		{"the watcher is stalled, no block times", false, false, maxUses},
	} {
		t.Run(v.name, func(t *testing.T) {
			e := coretest.NewEnv(t)
			requireOneConfirmation(t, e)
			e.Head(100)
			link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Early bird", Amount: "10", MaxUses: maxUses, Active: true})
			if err != nil {
				t.Fatal(err)
			}
			checkout := func() (core.CheckoutView, error) {
				co, _, err := e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{})
				return co, err
			}
			pay := func(id string, block uint64) {
				obs := e.Transfer(e.Payment(id), "10", block)
				if !v.blockTimes {
					obs.BlockTime = time.Time{}
				}
				e.Observe(obs)
			}
			var mine []core.CheckoutView
			block := uint64(100)
			for round := 0; round < 3; round++ { // t = 0, 11 and 22 minutes
				opened := 0
				for i := 0; i < maxUses; i++ {
					co, err := checkout()
					switch {
					case err == nil:
						mine = append(mine, co)
						opened++
					case !errors.Is(err, core.ErrLinkLimitReached):
						t.Fatalf("round %d, checkout %d: %v", round, i, err)
					}
				}
				// With heads, the previous round's checkouts have expired
				// and their uses are free again. Without, nothing proves
				// that they were not paid in time, so they still hold them.
				if want := map[bool]int{true: maxUses, false: 0}[v.heads || round == 0]; opened != want {
					t.Fatalf("round %d: %d checkouts opened, want %d", round, opened, want)
				}
				if _, err := checkout(); !errors.Is(err, core.ErrLinkLimitReached) {
					t.Fatalf("round %d: the link took more than MaxUses open checkouts: %v", round, err)
				}
				e.Clock.Advance(core.DefaultLinkReservation + time.Minute)
				if v.heads {
					block++
					e.Head(block)
				}
			}
			// Every checkout's deadline has passed. Pay every one of them.
			for _, co := range mine {
				pay(co.ID, block+1)
			}
			e.Head(block + 1)

			lv, err := e.Service.GetLink(e.Operator(), link.ID)
			if err != nil {
				t.Fatal(err)
			}
			paid, late := 0, 0
			for _, co := range mine {
				switch p := e.Payment(co.ID); {
				case p.Status == core.StatusPaid:
					paid++
				case p.Status == core.StatusExpired && p.Attention == core.AttentionLateFunds && p.NeedsAttention:
					late++
				default:
					t.Errorf("payment %s: status %s, attention %q", p.ID, p.Status, p.Attention)
				}
			}
			t.Logf("link with max_uses=%d: %d checkouts paid after their deadline: %d settled as paid, %d as late funds, link reports uses=%d",
				maxUses, len(mine), paid, late, lv.Uses)
			if paid > maxUses || lv.Uses != paid || paid+late != len(mine) {
				t.Fatalf("a link limited to %d uses: %d paid, %d late of %d, uses=%d", maxUses, paid, late, len(mine), lv.Uses)
			}
			if paid != v.wantPaid {
				t.Fatalf("%d payments to closed checkouts settled as paid, want %d", paid, v.wantPaid)
			}
			// The link has exactly the uses left that nobody paid for.
			for i := paid; i < maxUses; i++ {
				co, err := checkout()
				if err != nil {
					t.Fatalf("the link lost a use to a late payment: %v", err)
				}
				pay(co.ID, block+2)
			}
			if _, err := checkout(); !errors.Is(err, core.ErrLinkLimitReached) {
				t.Fatalf("more than MaxUses checkouts: %v", err)
			}
			e.Head(block + 2)
			if lv, err = e.Service.GetLink(e.Operator(), link.ID); err != nil || lv.Uses != maxUses || lv.Status != core.LinkLimit {
				t.Fatalf("link after MaxUses payments in time: %+v, %v", lv, err)
			}
		})
	}
}

// "Funded" used to mean "any non-zero
// transfer was observed", so one base unit of the token (10^-18 USDT; the
// cost is the network fee) was enough to
//
//	(a) hold one of a limited link's uses until the payment's own deadline,
//	(b) keep the deposit address on the chain watch list for the 7-day
//	    window after it settled instead of one hour — 20 000 dusted link
//	    checkouts kept every public payment link at watch_list_full for a
//	    week — and
//	(c) end as "underpaid": an item needing the operator's attention, with
//	    its webhooks.
//
// Now dust (below min(1 unit, 5 % of the amount)) is recorded and otherwise
// ignored, a link payment is funded only by half its amount and at least
// one whole unit, and a use is held only while the checkout can still be
// paid in time.
func TestDustDoesNotMakeACheckoutFunded(t *testing.T) {
	const cap = 5
	e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.MaxWatchedAddresses = cap }))
	requireOneConfirmation(t, e)
	e.Head(100)

	limited, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Limited", Amount: "100", MaxUses: 1, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	open, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Donate", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	const dust = "0.000000000000000001"

	// (a) One dusted checkout must not hold the limited link's only use
	// beyond the reservation window.
	squat, _, err := e.Service.CreatePaymentFromLink(context.Background(), limited.Slug, core.LinkPaymentInput{})
	if err != nil {
		t.Fatal(err)
	}
	e.Pay(e.Payment(squat.ID), dust, 101)
	e.Head(101)
	e.Clock.Advance(core.DefaultLinkReservation + 5*time.Minute) // 15 minutes: the reservation is long over
	e.Head(102)                                                  // ... and the chain has got there too
	_, _, errLimited := e.Service.CreatePaymentFromLink(context.Background(), limited.Slug, core.LinkPaymentInput{})
	t.Logf("(a) 15 minutes after a checkout that received %s USDT: a real customer gets %v", dust, errLimited)

	// (b) Fill the watch list with dusted checkouts and let them all expire.
	var last core.CheckoutView
	for i := 0; i < cap-2; i++ { // the two limited-link checkouts are on the list as well
		co, _, err := e.Service.CreatePaymentFromLink(context.Background(), open.Slug, core.LinkPaymentInput{Amount: "100"})
		if err != nil {
			t.Fatal(err)
		}
		e.Pay(e.Payment(co.ID), dust, 103)
		last = co
	}
	e.Head(103)
	if _, _, err := e.Service.CreatePaymentFromLink(context.Background(), open.Slug, core.LinkPaymentInput{Amount: "100"}); !errors.Is(err, core.ErrWatchListFull) {
		t.Fatalf("the links' share of the watch list (%d) is used up, yet: %v", cap, err)
	}
	e.Expire(e.Payment(last.ID))
	e.Head(104) // all of them settle
	settled := e.Payment(last.ID)
	e.Clock.Advance(6 * 24 * time.Hour)
	e.Head(105)
	_, _, errFull := e.Service.CreatePaymentFromLink(context.Background(), open.Slug, core.LinkPaymentInput{Amount: "100"})
	cs, err := e.Service.ChainState(e.System())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("(b) 6 days after %d dusted checkouts settled (status of the last: %q): %d addresses still watched (cap %d); a real customer gets %v",
		cap, settled.Status, len(cs.Addresses), cap, errFull)

	if errLimited != nil {
		t.Errorf("(a) a transfer of one base unit held a limited link's use past the reservation window: %v", errLimited)
	}
	if errFull != nil {
		t.Errorf("(b) %d checkouts that each received one base unit keep the watch list full 6 days after they settled; link payments are refused: %v", cap, errFull)
	}
	if len(cs.Addresses) != 1 { // the checkout just created
		t.Errorf("(b) %d addresses watched, want only the new checkout's", len(cs.Addresses))
	}
	// (c) A dusted checkout is an expired checkout: no attention, no
	// "underpaid" webhook.
	for _, id := range []string{squat.ID, last.ID} {
		p := e.Payment(id)
		if p.Status != core.StatusExpired || p.NeedsAttention || p.Attention != "" {
			t.Errorf("(c) dusted checkout %s: status %s, attention %q", id, p.Status, p.Attention)
		}
		if p.AmountReceived != dust {
			t.Errorf("(c) the dust must stay on record: received %q", p.AmountReceived)
		}
		for _, typ := range e.EventTypes(id) {
			if typ != core.EventPaymentCreated && typ != core.EventPaymentExpired {
				t.Errorf("(c) dusted checkout %s emitted %s", id, typ)
			}
		}
	}
}
