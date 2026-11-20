package core_test

import (
	"slices"
	"testing"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// TestPendingWorkOfAnotherTokenWaitsForIt: money to a settled payment is seen
// and not final yet when the operator switches to another token on the same
// chain. Nobody scans the first token's logs now, so nobody would notice that
// transfer leave the chain: it is not in what the chain watcher of the second
// token is given, and that watcher's heads do not judge it — it is not
// confirmed however deep and final its block gets. When the first token is
// configured again it is followed, and confirmed, as before.
func TestPendingWorkOfAnotherTokenWaitsForIt(t *testing.T) {
	e := coretest.NewEnv(t)
	e.Head(100)
	p := e.CreatePayment("25")
	e.Pay(p, "25", 101)
	e.Head(120)
	if got := e.Payment(p.ID); got.Status != core.StatusPaid {
		t.Fatalf("test setup: %s", got.Status)
	}
	late := e.Pay(p, "5", 121)
	pending := func() []core.Transfer {
		t.Helper()
		cs, err := e.Service.ChainState(e.System())
		if err != nil {
			t.Fatal(err)
		}
		return cs.PendingTransfers
	}
	if n := len(pending()); n != 1 {
		t.Fatalf("test setup: %d pending", n)
	}

	urls := []string{"https://bsc-a.example.org", "https://bsc-b.example.net"}
	usdc := core.Asset{Symbol: "USDC", Network: "BEP20", ChainID: core.ChainBSCMainnet, Contract: "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d", Decimals: 18}
	if _, err := e.Service.UpdateNetwork(e.Operator(), core.UpdateNetworkInput{Asset: &usdc, RPCURLs: urls}); err != nil {
		t.Fatal(err)
	}
	if got := pending(); len(got) != 0 {
		t.Fatalf("the watch snapshot for USDC holds USDT's pending transfers: %+v", got)
	}
	e.Head(500)
	got := e.Payment(p.ID)
	if got.AmountConfirmed != "25.00" || got.NeedsAttention || got.Confirmations != 0 || got.AwaitingFinality != nil {
		t.Fatalf("under the USDC watcher's head: confirmed %s, attention %v, %d confirmations, awaiting %+v",
			got.AmountConfirmed, got.NeedsAttention, got.Confirmations, got.AwaitingFinality)
	}
	if types := e.EventTypes(p.ID); slices.Contains(types, core.EventPaymentLateFunds) {
		t.Fatalf("events under the USDC watcher's head: %v", types)
	}

	usdt := core.USDTBSC
	if _, err := e.Service.UpdateNetwork(e.Operator(), core.UpdateNetworkInput{Asset: &usdt, RPCURLs: urls}); err != nil {
		t.Fatal(err)
	}
	if got := pending(); len(got) != 1 || got[0].TxHash != late.TxHash {
		t.Fatalf("USDT again: pending %+v", got)
	}
	e.Head(501)
	if got := e.Payment(p.ID); got.AmountConfirmed != "30.00" || got.Attention != core.AttentionLateFunds {
		t.Fatalf("USDT again: confirmed %s, attention %q", got.AmountConfirmed, got.Attention)
	}
}
