package corebridge

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// TestHeadOfAReplacedConfigurationIsRefused: a generation's tick is in
// flight when the operator switches the token — away and back, so that
// network, endpoints and token look as they did. The head that tick goes on
// to report is a fact of a configuration that has been replaced: the core
// refuses it, nothing is retired or expired on it, the generation halts, and
// the generation built for the current configuration finds the cancelled
// invoice's in-window transfer, as late funds.
func TestHeadOfAReplacedConfigurationIsRefused(t *testing.T) {
	w := newWorld(t, withNodes(2))
	g := newGeneration(t, w)
	head := func(g *generation, n uint64) error {
		t.Helper()
		blk := mustBlock(t, w.chain, n)
		return g.HeadAdvanced(t.Context(), watcher.Head{Number: n, Hash: blk.Hash, Time: time.Unix(int64(blk.Time), 0), Final: n})
	}
	if err := head(g, w.chain.Head().Number-testSettle); err != nil {
		t.Fatal(err)
	}
	p := w.env.CreatePayment("25")
	if _, err := w.env.Service.CancelPayment(w.env.Operator(), p.ID); err != nil {
		t.Fatal(err)
	}
	change := func(a core.Asset) {
		t.Helper()
		if _, err := w.env.Service.UpdateNetwork(w.env.Relogin(), core.UpdateNetworkInput{Asset: &a, RPCURLs: w.urls}); err != nil {
			t.Fatal(err)
		}
	}
	other := core.USDTBSC
	other.Contract = "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d"
	change(other)
	g = newGeneration(t, w) // the other token's generation
	w.advance(time.Minute)
	w.pay(p, 25)
	paid := w.mine(1)
	w.advance(24 * time.Hour)
	w.mine(testSettle + 2)
	late := w.chain.Head().Number - testSettle
	if _, err := g.Watched(t.Context()); err != nil {
		t.Fatal(err)
	}
	change(core.USDTBSC) // and back, while the other token's tick is in flight

	err := head(g, late)
	if !errors.Is(err, core.ErrConfigurationReplaced) || !errors.Is(err, watcher.ErrHalt) {
		t.Fatalf("the in-flight head of the replaced configuration: %v, want it refused and the generation halted", err)
	}
	if !w.listed(p) {
		t.Fatal("the in-flight head of the replaced configuration retired the cancelled invoice's address")
	}
	if h := w.coreHead(); h.Number != 0 {
		t.Fatalf("the in-flight head of the replaced configuration was recorded: %+v", h)
	}
	w.start()
	w.mine(testSettle + 2)
	w.synced()
	w.eventually("the in-window transfer to be credited as late funds", func() bool {
		got := w.env.Payment(p.ID)
		return got.AmountReceived == "25.00" && got.NeedsAttention
	})
	_ = paid
}

// TestStoredNetworkSettingsThatWouldBeRefusedRunNothing: settings an
// earlier version stored — two URLs of one endpoint, which are refused now.
// The instance comes up: no chain watcher runs on them, no payment is
// created on them (the head the core holds is from before), and the node
// status says what is wrong and where to fix it. Repaired in the network
// settings, the watcher starts and payments work.
func TestStoredNetworkSettingsThatWouldBeRefusedRunNothing(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.synced()
	good := w.urls
	err := w.env.Store.Update(t.Context(), func(tx core.Tx) error {
		st, err := tx.GetSettings(t.Context())
		if err != nil {
			return err
		}
		st.Network.RPCURLs = []string{w.urls[0] + "/a", w.urls[0] + "/b"}
		return tx.SaveSettings(t.Context(), st)
	})
	if err != nil {
		t.Fatal(err)
	}
	w.stop()
	w.start()
	w.eventually("the bridge to run nothing, and say why", func() bool {
		st := w.bridge.Status()
		return st.State == StateIdle && strings.Contains(st.Reason, "network settings cannot be used") && strings.Contains(st.Reason, "same endpoint")
	})
	if _, _, err := w.env.Service.CreatePayment(w.env.System(), core.CreatePaymentInput{Amount: "5"}); !errors.Is(err, core.ErrNetworkInvalid) {
		t.Fatalf("a payment on settings that would be refused: %v", err)
	}
	ns, err := w.env.Service.NodeStatus(w.env.Relogin())
	if err != nil || ns.Healthy || !strings.Contains(ns.NetworkProblem, "same endpoint") || len(ns.Warnings) == 0 || !strings.Contains(ns.Warnings[0], "network settings cannot be used") {
		t.Fatalf("node status: %+v, %v", ns, err)
	}
	if _, err := w.env.Service.UpdateNetwork(w.env.Relogin(), core.UpdateNetworkInput{RPCURLs: good}); err != nil {
		t.Fatalf("repairing the settings: %v", err)
	}
	w.mine(testSettle + 2)
	w.synced()
	if _, _, err := w.env.Service.CreatePayment(w.env.System(), core.CreatePaymentInput{Amount: "5"}); err != nil {
		t.Fatalf("a payment after the repair: %v", err)
	}
}

// TestReleasedAlertComparesWithinOneConfiguration: the alert about addresses
// let go before a second log scan compares the count with the last one read
// under the same configuration. Another network counts other payments: its
// first count is a baseline, not an increase.
func TestReleasedAlertComparesWithinOneConfiguration(t *testing.T) {
	w := newWorld(t)
	b, err := New(w.env.Service, w.store, w.opts)
	if err != nil {
		t.Fatal(err)
	}
	alerts := func() uint64 { return b.Status().Alerts[AlertUncheckedReleased] }
	b.noteReleased(3, 1) // the first reading: a baseline
	b.noteReleased(3, 1)
	if alerts() != 0 {
		t.Fatalf("alerts without an increase: %d", alerts())
	}
	b.noteReleased(5, 1)
	if alerts() != 1 {
		t.Fatalf("an increase under one configuration: %d alerts", alerts())
	}
	b.noteReleased(9, 2) // another network, other payments
	if alerts() != 1 {
		t.Fatalf("a change of configuration taken for an increase: %d alerts", alerts())
	}
	b.noteReleased(10, 2)
	if alerts() != 2 {
		t.Fatalf("an increase under the new configuration: %d alerts", alerts())
	}
}
