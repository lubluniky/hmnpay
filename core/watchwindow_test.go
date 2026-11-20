package core_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// watched reports whether the chain state lists the payment's address.
func watched(t *testing.T, e *coretest.Env, paymentID string) bool {
	t.Helper()
	cs, err := e.Service.ChainState(e.System())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cs.Addresses {
		if a.PaymentID == paymentID {
			return true
		}
	}
	return false
}

// stored returns the payment as the store has it.
func stored(t *testing.T, e *coretest.Env, id string) core.Payment {
	t.Helper()
	var p core.Payment
	if err := e.Store.View(context.Background(), func(r core.Reader) error {
		var err error
		p, err = r.GetPayment(context.Background(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestWatchWindowEndsByBothClocks: a settled payment's address leaves the
// watch list when the wall clock has passed the end of its window and the
// processed chain has too — by its block time, less the margin for a
// reorganisation below the head. A watcher that is behind (paused, or down)
// has not scanned the blocks of the window's last moments yet, and is owed
// the address for them. The count the node status and the payment links' cap
// work with is the same list.
func TestWatchWindowEndsByBothClocks(t *testing.T) {
	e := coretest.NewEnv(t)
	e.Head(100)
	p := e.CreatePayment("25")
	e.Pay(p, "25", 101)
	e.Head(101 + uint64(core.DefaultRequiredConfirmations))
	if got := e.Payment(p.ID); got.Status != core.StatusPaid {
		t.Fatalf("payment: %+v", got)
	}
	count := func() int {
		ns, err := e.Service.NodeStatus(e.Relogin())
		if err != nil {
			t.Fatal(err)
		}
		return ns.WatchedAddresses
	}

	// The week is over by the wall clock; the processed head is a week old.
	e.Clock.Advance(core.DefaultWatchWindow + time.Second)
	if !watched(t, e, p.ID) || count() != 1 {
		t.Fatalf("the address left the watch list while the processed head is a week behind its window (listed %v, counted %d)", watched(t, e, p.ID), count())
	}
	// The chain is past the window too, but not by the margin yet.
	e.Head(200)
	if !watched(t, e, p.ID) {
		t.Fatalf("the address left the watch list %s after its window by the chain, margin %s", time.Second, core.WatchRetirementMargin)
	}
	// Past the margin: gone.
	e.Clock.Advance(core.WatchRetirementMargin)
	e.Head(300)
	if watched(t, e, p.ID) || count() != 0 {
		t.Fatalf("the address is still listed (counted %d) with both clocks past its window", count())
	}
	// A chain head stamped ahead of the wall clock does not end it early:
	// the wall clock has to be past the window as well.
	q := e.CreatePayment("25")
	e.Pay(q, "25", 301)
	e.Head(301 + uint64(core.DefaultRequiredConfirmations))
	settled := e.Payment(q.ID)
	if settled.Status != core.StatusPaid {
		t.Fatalf("payment: %+v", settled)
	}
	future := core.ChainHead{ChainID: core.ChainBSCMainnet, Number: 400, Hash: coretest.BlockHash(400, 0),
		Time: e.Clock.Now().Add(core.DefaultWatchWindow + time.Hour), Final: 400}
	if err := e.Service.AdvanceHead(e.System(), future); err != nil {
		t.Fatal(err)
	}
	if !watched(t, e, q.ID) {
		t.Fatal("a head stamped in the future ended a watch window the wall clock has not finished")
	}
}

// TestNoAddressLeavesTheWatchListWithoutAHeadOfItsNetwork: an address
// leaves the watch list on evidence from a processed head of its own network,
// never by the wall clock alone. A network switched away and back has no head
// for the time in between — the blocks of that time have not been scanned for
// the address — and the address is listed until a head of its network has
// passed its window. A head of another network retires nothing of this one;
// a head that goes back does not bring a retired address back, and a head
// that is not past the window does not retire it.
func TestNoAddressLeavesTheWatchListWithoutAHeadOfItsNetwork(t *testing.T) {
	e := coretest.NewEnv(t)
	e.Head(100)
	p := e.CreatePayment("25")
	if _, err := e.Service.CancelPayment(e.Operator(), p.ID); err != nil {
		t.Fatal(err)
	}
	network := func(chain uint64, url string) {
		t.Helper()
		if _, err := e.Service.UpdateNetwork(e.Operator(), core.UpdateNetworkInput{ChainID: chain, RPCURLs: []string{url}}); err != nil {
			t.Fatal(err)
		}
	}
	network(core.ChainBSCTestnet, "https://testnet.example.org")
	e.Clock.Advance(core.DefaultUnfundedWatchWindow + core.DefaultPaymentTTL + time.Hour)
	// The other network's chain moves on, past the window.
	other := core.ChainHead{ChainID: core.ChainBSCTestnet, Number: 5_000, Hash: coretest.BlockHash(5_000, 1), Time: e.Clock.Now(), Final: 5_000}
	if err := e.Service.AdvanceHead(e.System(), other); err != nil {
		t.Fatal(err)
	}
	network(core.ChainBSCMainnet, "https://bsc.example.org")
	if !watched(t, e, p.ID) {
		t.Fatal("after a switch away and back, the address left the watch list by the wall clock: the blocks of its window mined while another network was configured have not been scanned for it")
	}
	// A head of its network that is not past the window by the margin yet.
	early := core.ChainHead{ChainID: core.ChainBSCMainnet, Number: 900, Hash: coretest.BlockHash(900, 0),
		Time: stored(t, e, p.ID).WatchUntil.Add(core.WatchRetirementMargin - time.Second), Final: 900}
	if err := e.Service.AdvanceHead(e.System(), early); err != nil {
		t.Fatal(err)
	}
	if !watched(t, e, p.ID) {
		t.Fatal("a head of the network that is not past the window by the margin retired the address")
	}
	e.Head(1_000)
	if watched(t, e, p.ID) {
		t.Fatal("a head of the network past the window did not retire the address")
	}
	// Remembered: a head that goes back, or another switch away and back,
	// does not bring it back.
	if err := e.Service.AdvanceHead(e.System(), early); err != nil {
		t.Fatal(err)
	}
	network(core.ChainBSCTestnet, "https://testnet.example.org")
	network(core.ChainBSCMainnet, "https://bsc.example.org")
	if watched(t, e, p.ID) {
		t.Fatal("a retired address came back to the watch list")
	}
}

// TestAddressStaysWatchedWhileBlocksOfItsWindowAreUnchecked: blocks the
// adapter settled on one log endpoint's answers are scanned again by a
// second, later; that second scan can only find a payment to an address that
// is still watched. So while the head says blocks stamped before the end of a
// window are still to be scanned (ChainHead.Checked), the address stays — and
// the node status counts it. The number kept for that reason is bounded:
// beyond the limit the oldest are let go, marked, and counted.
func TestAddressStaysWatchedWhileBlocksOfItsWindowAreUnchecked(t *testing.T) {
	e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.MaxRetainedUnchecked = 1 }))
	e.Head(100)
	p := e.CreatePayment("25")
	e.Clock.Advance(time.Minute)
	q := e.CreatePayment("25")
	for _, x := range []core.PaymentView{p, q} {
		if _, err := e.Service.CancelPayment(e.Operator(), x.ID); err != nil {
			t.Fatal(err)
		}
	}
	since := e.Clock.Now().Add(-time.Hour) // blocks from before both were created are unchecked
	e.Clock.Advance(core.DefaultUnfundedWatchWindow + core.DefaultPaymentTTL + time.Hour)
	head := func(n uint64, checked time.Time) {
		t.Helper()
		h := core.ChainHead{ChainID: core.ChainBSCMainnet, Number: n, Hash: coretest.BlockHash(n, 0), Time: e.Clock.Now(), Checked: checked, Final: n}
		if err := e.Service.AdvanceHead(e.System(), h); err != nil {
			t.Fatal(err)
		}
	}
	status := func() core.NodeStatus {
		t.Helper()
		ns, err := e.Service.NodeStatus(e.Relogin())
		if err != nil {
			t.Fatal(err)
		}
		return ns
	}
	head(1_000, since)
	if watched(t, e, p.ID) || !watched(t, e, q.ID) {
		t.Fatalf("with a limit of one, the older address is let go and the newer one kept (p listed %v, q listed %v)", watched(t, e, p.ID), watched(t, e, q.ID))
	}
	ns := status()
	cs, err := e.Service.ChainState(e.System())
	if err != nil {
		t.Fatal(err)
	}
	if ns.RetainedUnchecked != 1 || ns.ReleasedUnchecked != 1 || cs.ReleasedUnchecked != 1 || ns.WatchedAddresses != 1 {
		t.Fatalf("node status: retained %d, released %d, watched %d; chain state released %d", ns.RetainedUnchecked, ns.ReleasedUnchecked, ns.WatchedAddresses, cs.ReleasedUnchecked)
	}
	// The blocks are checked: the kept one goes, normally.
	head(1_001, time.Time{})
	if watched(t, e, q.ID) {
		t.Fatal("the address is still listed once every block of its window was scanned by two endpoints")
	}
	if ns := status(); ns.RetainedUnchecked != 0 || ns.ReleasedUnchecked != 1 || ns.WatchedAddresses != 0 {
		t.Fatalf("node status: retained %d, released %d, watched %d", ns.RetainedUnchecked, ns.ReleasedUnchecked, ns.WatchedAddresses)
	}
}

// TestWatchListIsTheCurrentAssets: the settled payments of an asset that is
// not configured any more stay unretired — they are owed the blocks mined
// meanwhile if it is configured again — but they are not watched, and not
// counted: not in the node status, not against the payment links' share.
func TestWatchListIsTheCurrentAssets(t *testing.T) {
	e := coretest.NewEnv(t)
	e.Head(100)
	p := e.CreatePayment("25")
	if _, err := e.Service.CancelPayment(e.Operator(), p.ID); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		ns, err := e.Service.NodeStatus(e.Relogin())
		if err != nil {
			t.Fatal(err)
		}
		return ns.WatchedAddresses
	}
	switchTo := func(a core.Asset) {
		t.Helper()
		if _, err := e.Service.UpdateNetwork(e.Operator(), core.UpdateNetworkInput{Asset: &a, RPCURLs: []string{"https://bsc.example.org"}}); err != nil {
			t.Fatal(err)
		}
	}
	usdc := core.Asset{Symbol: "USDC", Network: "BEP20", ChainID: core.ChainBSCMainnet, Contract: "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d", Decimals: 18}
	switchTo(usdc)
	e.Clock.Advance(core.DefaultPaymentTTL + core.DefaultExpiryGrace + core.DefaultUnfundedWatchWindow + core.WatchRetirementMargin)
	e.Head(200) // a head of the chain, for the other asset
	if watched(t, e, p.ID) || count() != 0 {
		t.Fatalf("a payment of the asset configured before is watched (%v) or counted (%d)", watched(t, e, p.ID), count())
	}
	switchTo(core.USDTBSC)
	if !watched(t, e, p.ID) || count() != 1 {
		t.Fatalf("back on its asset, the payment is not watched (%v) or not counted (%d): the other asset's head retired it", watched(t, e, p.ID), count())
	}
}

// TestPublicLinkPaymentsAreLetGoBeforeTheMerchants: of the addresses kept
// for a second log endpoint's scan, beyond the limit those of payments
// through public links go first — anybody can create those — and the
// merchant's own invoice, older, stays covered.
func TestPublicLinkPaymentsAreLetGoBeforeTheMerchants(t *testing.T) {
	e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.MaxRetainedUnchecked = 1 }))
	e.Head(100)
	merchant := e.CreatePayment("25")
	link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Tickets", Amount: "5", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	e.Clock.Advance(time.Minute)
	co, _, err := e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Service.CancelPayment(e.Operator(), merchant.ID); err != nil {
		t.Fatal(err)
	}
	since := e.Clock.Now().Add(-time.Hour)
	e.Clock.Advance(core.DefaultPaymentTTL + core.DefaultExpiryGrace + time.Minute)
	e.Head(200) // the link's checkout expires
	e.Clock.Advance(core.DefaultUnfundedWatchWindow + core.WatchRetirementMargin + time.Hour)
	h := core.ChainHead{ChainID: core.ChainBSCMainnet, Number: 1_000, Hash: coretest.BlockHash(1_000, 0), Time: e.Clock.Now(), Checked: since, Final: 1_000}
	if err := e.Service.AdvanceHead(e.System(), h); err != nil {
		t.Fatal(err)
	}
	if !watched(t, e, merchant.ID) || watched(t, e, co.ID) {
		t.Fatalf("with room for one, the merchant's invoice is kept (%v) and the link's checkout let go (%v)", watched(t, e, merchant.ID), !watched(t, e, co.ID))
	}
	if !stored(t, e, co.ID).RetiredUnchecked {
		t.Fatal("the link's checkout let go is not marked")
	}
}

// TestNodeStatusWarnsOfALargeWatchList: from WatchListWarning watched
// addresses on, the node status says so (payments the merchant creates have
// no cap); one fewer, it does not.
func TestNodeStatusWarnsOfALargeWatchList(t *testing.T) {
	e := coretest.NewEnv(t)
	e.Head(100)
	warned := func() bool {
		t.Helper()
		ns, err := e.Service.NodeStatus(e.Relogin())
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range ns.Warnings {
			if strings.Contains(w, "addresses are on the chain watch list") {
				return true
			}
		}
		return false
	}
	insert := func(from, to int) {
		t.Helper()
		err := e.Store.Update(context.Background(), func(tx core.Tx) error {
			for i := from; i < to; i++ {
				p := core.Payment{ID: fmt.Sprintf("pay_%06d", i), Status: core.StatusAwaiting, Amount: core.NewAmount(1), Asset: core.USDTBSC,
					Address: fmt.Sprintf("0x%040x", i+1), CreatedAt: e.Clock.Now(), ExpiresAt: e.Clock.Now().Add(time.Hour), WatchUntil: e.Clock.Now().Add(2 * time.Hour)}
				if err := tx.InsertPayment(context.Background(), p); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(0, core.WatchListWarning-1)
	if warned() {
		t.Fatalf("warned with %d watched addresses", core.WatchListWarning-1)
	}
	insert(core.WatchListWarning-1, core.WatchListWarning)
	if !warned() {
		t.Fatalf("no warning with %d watched addresses", core.WatchListWarning)
	}
}
