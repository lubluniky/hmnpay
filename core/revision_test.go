package core_test

import (
	"errors"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// TestFactsOfAReplacedConfigurationAreRefused: every change of the chain
// configuration — endpoints, token, finality depth — gives it a new
// revision, also a change away and back; settings that are not part of it
// do not. A fact made under another revision is refused in the transaction
// that would apply it — heads, transfers, retractions — and a status report
// of another revision is not taken into account (it would decide the clock
// gate and the node status).
func TestFactsOfAReplacedConfigurationAreRefused(t *testing.T) {
	e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.MaxHeadTimeAhead = 2 * time.Minute }))
	e.Head(100)
	p := e.CreatePayment("25")
	state := func() core.ChainState {
		t.Helper()
		cs, err := e.Service.ChainState(e.System())
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}
	first := state().Revision
	if first == 0 {
		t.Fatal("a set-up instance has no chain configuration revision")
	}
	urls := e.SetupInput().RPCURLs
	network := func(in core.UpdateNetworkInput) uint64 {
		t.Helper()
		if _, err := e.Service.UpdateNetwork(e.Relogin(), in); err != nil {
			t.Fatal(err)
		}
		return state().Revision
	}
	if _, err := e.Service.UpdateMerchant(e.Relogin(), core.UpdateMerchantInput{Merchant: core.MerchantSettings{Name: "Other", SupportEmail: "help@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if r := state().Revision; r != first {
		t.Fatalf("a change of the merchant name changed the chain configuration revision: %d -> %d", first, r)
	}
	if _, err := e.Service.CancelPayment(e.Operator(), p.ID); err != nil {
		t.Fatal(err)
	}
	other := core.USDTBSC
	other.Contract = "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d"
	away := network(core.UpdateNetworkInput{Asset: &other, RPCURLs: urls})
	back := network(core.UpdateNetworkInput{Asset: &core.USDTBSC, RPCURLs: urls})
	reordered := network(core.UpdateNetworkInput{RPCURLs: append([]string{"https://second.example.net"}, urls...)})
	if !(first < away && away < back && back < reordered) {
		t.Fatalf("revisions %d, %d, %d, %d: every change, also back to what it was, gives a new one", first, away, back, reordered)
	}
	// The finality depth is part of it; the payment TTL is not.
	defaults := func(change func(*core.PaymentSettings)) uint64 {
		t.Helper()
		cur, err := e.Service.GetSettings(e.Relogin())
		if err != nil {
			t.Fatal(err)
		}
		ps := cur.Payments
		change(&ps)
		if _, err := e.Service.UpdatePaymentDefaults(e.Relogin(), ps); err != nil {
			t.Fatal(err)
		}
		return state().Revision
	}
	if r := defaults(func(p *core.PaymentSettings) { p.TTL += time.Minute }); r != reordered {
		t.Fatalf("a change of the payment TTL changed the chain configuration revision: %d -> %d", reordered, r)
	}
	deeper := defaults(func(p *core.PaymentSettings) { p.RequiredConfirmations++ })
	if deeper <= reordered {
		t.Fatalf("a change of the finality depth kept the revision %d", deeper)
	}
	testnet := network(core.UpdateNetworkInput{ChainID: core.ChainBSCTestnet, RPCURLs: []string{"https://testnet.example.org"}})
	mainnet := network(core.UpdateNetworkInput{ChainID: core.ChainBSCMainnet, RPCURLs: append([]string{"https://second.example.net"}, urls...)})
	if !(deeper < testnet && testnet < mainnet) {
		t.Fatalf("a change of network and back: revisions %d, %d, %d", deeper, testnet, mainnet)
	}
	reordered = mainnet

	stale := core.WithChainRevision(e.System(), back)
	head := core.ChainHead{ChainID: core.ChainBSCMainnet, Number: 200, Hash: coretest.BlockHash(200, 0), Time: e.Clock.Now(), Final: 200}
	if err := e.Service.AdvanceHead(stale, head); !errors.Is(err, core.ErrConfigurationReplaced) {
		t.Fatalf("a head of a replaced configuration: %v", err)
	}
	if h := state().Head; h.Number == 200 {
		t.Fatalf("the head of a replaced configuration was recorded: %+v", h)
	}
	obs := e.Transfer(p, "25", 150)
	if err := e.Service.ObserveTransfer(stale, obs); !errors.Is(err, core.ErrConfigurationReplaced) {
		t.Fatalf("a transfer of a replaced configuration: %v", err)
	}
	if err := e.Service.RetractTransfer(stale, core.TransferKey{TxHash: obs.TxHash, LogIndex: obs.LogIndex, BlockHash: obs.BlockHash}); !errors.Is(err, core.ErrConfigurationReplaced) {
		t.Fatalf("a retraction of a replaced configuration: %v", err)
	}
	current := core.WithChainRevision(e.System(), reordered)
	if err := e.Service.ObserveTransfer(current, obs); err != nil {
		t.Fatalf("a transfer of the current configuration: %v", err)
	}

	if err := e.Service.AdvanceHead(current, core.ChainHead{ChainID: core.ChainBSCMainnet, Number: 160, Hash: coretest.BlockHash(160, 0), Time: e.Clock.Now(), Final: 160}); err != nil {
		t.Fatalf("a head of the current configuration: %v", err)
	}

	// A status report of a replaced configuration does not count.
	if err := e.Service.ReportChainStatus(stale, core.ChainStatus{LatestBlock: 100, ClockSkew: -time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "5"}); err != nil {
		t.Fatalf("a payment was refused on the strength of a replaced configuration's report: %v", err)
	}
	ns, err := e.Service.NodeStatus(e.Relogin())
	if err != nil || !ns.LastCheckedAt.IsZero() {
		t.Fatalf("the node status shows a replaced configuration's report: %+v, %v", ns, err)
	}
	if err := e.Service.ReportChainStatus(current, core.ChainStatus{LatestBlock: 100, ClockSkew: -time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "5"}); !errors.Is(err, core.ErrClockBehind) {
		t.Fatalf("the current configuration's report: %v", err)
	}
}
