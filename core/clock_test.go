package core_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// A deadline is judged by two clocks, the wall clock and the processed
// chain's block time: the earlier of them has to be past it. One that was
// set from a wall clock behind the chain therefore ends early in the chain's
// terms — and with it the hour in which late funds on an unpaid invoice are
// looked for. A payer who pays inside the deadline the merchant asked for
// can pay after the address has left the watch list.

// TestNoPaymentWhileTheClockIsBehindTheChain: the server clock steps back a
// day after the chain watcher's last head. A payment with a two-day deadline
// is refused — it would be due a day early, and its address watched an hour
// after that — until the clock is right again; then its deadline is two days
// from now. A payment link refuses the same way.
func TestNoPaymentWhileTheClockIsBehindTheChain(t *testing.T) {
	e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.MaxHeadTimeAhead = 2 * time.Minute }))
	right := e.Clock.Now()
	e.Head(100)
	link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Tickets", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	e.Clock.Set(right.Add(-24 * time.Hour))

	p, _, err := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "25", TTL: 48 * time.Hour})
	if err == nil {
		t.Fatalf("a payment was created with the clock a day behind the chain: due %s, a day before the two days asked for (%s)",
			p.ExpiresAt, right.Add(48*time.Hour))
	}
	if !errors.Is(err, core.ErrClockBehind) || core.KindOf(err) != core.KindUnavailable {
		t.Fatalf("err = %v, want clock_behind", err)
	}
	if _, _, err := e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "25"}); !errors.Is(err, core.ErrClockBehind) {
		t.Fatalf("a payment link: err = %v, want clock_behind", err)
	}

	e.Clock.Set(right.Add(time.Minute))
	p, _, err = e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "25", TTL: 48 * time.Hour})
	if err != nil || p.ExpiresAt.Before(e.Clock.Now().Add(48*time.Hour)) {
		t.Fatalf("with the clock right: %+v, %v", p, err)
	}
}

// TestNoPaymentWhileTheEndpointsShowTheClockIsBehind: the chain watcher
// refuses blocks from the future itself, so a clock that is behind from the
// start never gets a head past it: the processed head stays where it was.
// What shows the clock is behind is the chain's newest block, as the
// adapter's endpoints show it (ChainStatus.ClockSkew). Beyond the tolerance
// that refuses payments; within it, or once the report is stale, it does
// not.
func TestNoPaymentWhileTheEndpointsShowTheClockIsBehind(t *testing.T) {
	e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.MaxHeadTimeAhead = 2 * time.Minute }))
	e.Head(100)
	report := func(skew time.Duration) {
		t.Helper()
		if err := e.Service.ReportChainStatus(e.System(), core.ChainStatus{LatestBlock: 100, ClockSkew: skew}); err != nil {
			t.Fatal(err)
		}
	}
	create := func() error {
		_, _, err := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "25"})
		return err
	}
	report(-10 * time.Minute)
	if err := create(); !errors.Is(err, core.ErrClockBehind) {
		t.Fatalf("the newest block is ten minutes ahead of the clock: err = %v, want clock_behind", err)
	}
	report(-time.Minute)
	if err := create(); err != nil {
		t.Fatalf("a minute behind, inside the tolerance: %v", err)
	}
	report(-10 * time.Minute)
	e.Clock.Advance(2 * time.Minute) // the report is older than a fresh one can be
	e.Head(101)
	if err := create(); err != nil {
		t.Fatalf("a stale report: %v", err)
	}
}

// TestDeadlinesCountFromTheLaterClock: with the check on block times
// switched off (for a server whose clock cannot be kept right) heads ahead
// of the clock are accepted, and payments are not refused; their deadline,
// and the hour of watching after it, count from the chain's time when it is
// the later one. The same holds for a cancellation.
func TestDeadlinesCountFromTheLaterClock(t *testing.T) {
	e := coretest.NewEnv(t) // the check is off in the test environment
	ahead := e.Clock.Now().Add(6 * time.Hour)
	if err := e.Service.AdvanceHead(e.System(), core.ChainHead{ChainID: core.ChainBSCMainnet, Number: 100, Hash: coretest.BlockHash(100, 0), Time: ahead}); err != nil {
		t.Fatal(err)
	}
	p := e.CreatePayment("25")
	if want := ahead.Add(core.DefaultPaymentTTL); !p.ExpiresAt.Equal(want) {
		t.Fatalf("due %s, want %s (the chain's time plus the TTL)", p.ExpiresAt, want)
	}
	if _, err := e.Service.CancelPayment(e.Operator(), p.ID); err != nil {
		t.Fatal(err)
	}
	if got, want := stored(t, e, p.ID).WatchUntil, ahead.Add(core.DefaultPaymentTTL+core.DefaultExpiryGrace+core.DefaultUnfundedWatchWindow); got.Before(want) {
		t.Fatalf("watched until %s, want at least %s", got, want)
	}
}
