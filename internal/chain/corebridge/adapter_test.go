package corebridge

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// newGeneration returns the sink/watch-set adapter of a world whose bridge
// is not running, for calling the adapter directly.
func newGeneration(t *testing.T, w *world) *generation {
	t.Helper()
	b, err := New(w.env.Service, w.store, w.opts)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := w.env.Service.ChainState(w.env.System())
	if err != nil {
		t.Fatal(err)
	}
	p, idle := planFor(cs)
	if idle != "" {
		t.Fatalf("plan: %s", idle)
	}
	client, err := evm.NewClient(evm.Config{ChainID: p.chainID, Endpoints: p.urls, Endpoint: w.opts.Endpoint})
	if err != nil {
		t.Fatal(err)
	}
	w.bridge = b
	g := &generation{b: b, plan: p, client: client, ctx: context.Background()}
	if g.w, err = watcher.New(watcher.Config{Client: client, Token: p.token, Sink: g, Watch: g, Store: w.store}); err != nil {
		t.Fatal(err)
	}
	return g
}

func transferTo(t *testing.T, p core.PaymentView, amount int64, block uint64, tag byte) watcher.Transfer {
	t.Helper()
	to, err := parseAddress(p.Address)
	if err != nil {
		t.Fatal(err)
	}
	return watcher.Transfer{
		Token: usdt, From: payer, To: to, Amount: units(amount),
		TxHash: evm.Hash{0xaa, tag}, LogIndex: 3, BlockNumber: block, BlockHash: evm.Hash{0xbb, tag, byte(block)},
	}
}

// TestErrorMapping: each answer of the core to a fact leads to the right
// behaviour of the watcher — ignore, alert, halt or retry — and none to an
// endless retry of something that can never succeed.
func TestErrorMapping(t *testing.T) {
	ctx := context.Background()
	head := func(w *world, n uint64) {
		t.Helper()
		if err := w.env.Service.AdvanceHead(w.env.System(), core.ChainHead{ChainID: 56, Number: n, Hash: evm.Hash{0xcc, byte(n)}.String(), Time: w.env.Clock.Now(), Final: n}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("accepted, replayed", func(t *testing.T) {
		w := newWorld(t)
		g := newGeneration(t, w)
		head(w, 99)
		p := w.env.CreatePayment("25")
		tr := transferTo(t, p, 25, 100, 1)
		for i := 0; i < 3; i++ { // idempotent
			if err := g.TransferObserved(ctx, tr); err != nil {
				t.Fatal(err)
			}
		}
		if got := w.env.Payment(p.ID); got.Status != core.StatusConfirming || got.AmountReceived != "25.00" {
			t.Fatalf("payment = %+v", got)
		}
		if err := g.TransferReverted(ctx, tr); err != nil {
			t.Fatal(err)
		}
		if err := g.TransferReverted(ctx, tr); err != nil { // idempotent
			t.Fatal(err)
		}
		if got := w.status(p); got != core.StatusAwaiting {
			t.Fatalf("status = %s", got)
		}
		// A revert of something the core never saw is a no-op.
		if err := g.TransferReverted(ctx, transferTo(t, p, 1, 101, 2)); err != nil {
			t.Fatal(err)
		}
		if len(w.bridge.Status().Alerts) != 0 {
			t.Fatalf("alerts = %+v", w.bridge.Status().Alerts)
		}
	})

	t.Run("unknown address is ignored", func(t *testing.T) {
		w := newWorld(t)
		g := newGeneration(t, w)
		tr := watcher.Transfer{Token: usdt, From: payer, To: usdt, Amount: units(1), TxHash: evm.Hash{1}, BlockNumber: 5, BlockHash: evm.Hash{2}}
		if err := g.TransferObserved(ctx, tr); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(w.bridge.Status().Alerts) != 0 {
			t.Fatal("an unknown address raised an alert")
		}
	})

	t.Run("contradiction of a final transfer: alert, no retry", func(t *testing.T) {
		w := newWorld(t)
		g := newGeneration(t, w)
		head(w, 99)
		p := w.env.CreatePayment("25")
		final := transferTo(t, p, 25, 100, 1)
		if err := g.TransferObserved(ctx, final); err != nil {
			t.Fatal(err)
		}
		head(w, 100+testConfirmations) // final now
		if got := w.status(p); got != core.StatusPaid {
			t.Fatalf("status = %s", got)
		}
		// The chain "takes it back": a revert of the final transfer, and
		// the same transaction showing up in another block.
		if err := g.TransferReverted(ctx, final); err != nil {
			t.Fatalf("revert of a final transfer must not be retried: %v", err)
		}
		moved := final
		moved.BlockNumber, moved.BlockHash = 104, evm.Hash{0xdd}
		if err := g.TransferObserved(ctx, moved); err != nil {
			t.Fatalf("observation contradicting a final transfer must not be retried: %v", err)
		}
		if got := w.env.Payment(p.ID); got.Status != core.StatusPaid || got.AmountConfirmed != "25.00" {
			t.Fatalf("the settlement changed: %+v", got)
		}
		st := w.bridge.Status()
		if st.Alerts[AlertFinalityConflict] != 2 || st.LastAlert.TxHash != final.TxHash.String() || st.LastAlert.BlockNumber != 104 {
			t.Fatalf("alerts = %+v, last = %+v", st.Alerts, st.LastAlert)
		}
		if !strings.Contains(w.logs.String(), "level=ERROR") || !strings.Contains(w.logs.String(), "regards as final") {
			t.Error("not logged as an error")
		}
		for i := 0; i < 2; i++ {
			select {
			case a := <-w.alerts:
				if a.Kind != AlertFinalityConflict {
					t.Fatalf("alert = %+v", a)
				}
			default:
				t.Fatal("OnAlert not called")
			}
		}
	})

	t.Run("chain or token mismatch halts the watcher", func(t *testing.T) {
		w := newWorld(t)
		g := newGeneration(t, w)
		head(w, 99)
		p := w.env.CreatePayment("25")
		// This watcher still believes in another chain.
		g.plan.chainID = 97
		err := g.TransferObserved(ctx, transferTo(t, p, 25, 100, 1))
		if !errors.Is(err, watcher.ErrHalt) || !errors.Is(err, core.ErrChainMismatch) {
			t.Fatalf("observe: %v", err)
		}
		// (Block times are read against the core's clock, which is the fake
		// one of the test world: a head stamped with the wall clock is, from
		// two minutes past coretest.Epoch on, a head from the future, which
		// the core refuses before it looks at the chain id.)
		err = g.HeadAdvanced(ctx, watcher.Head{Number: 100, Hash: evm.Hash{1}, Time: w.env.Clock.Now(), Final: 100})
		if !errors.Is(err, watcher.ErrHalt) {
			t.Fatalf("head: %v", err)
		}
		if got := w.status(p); got != core.StatusAwaiting {
			t.Fatalf("a fact for another chain was credited: %s", got)
		}
		// The watch set of a watcher built for an older configuration
		// halts it as well.
		if _, err := g.Watched(ctx); !errors.Is(err, watcher.ErrHalt) {
			t.Fatalf("watch set: %v", err)
		}
		// Another token on the right chain is refused by the core too.
		g.plan.chainID = 56
		other := transferTo(t, p, 25, 100, 2)
		other.Token = payer
		if err := g.TransferObserved(ctx, other); !errors.Is(err, watcher.ErrHalt) {
			t.Fatalf("other token: %v", err)
		}
	})

	t.Run("malformed fact: alert and drop", func(t *testing.T) {
		w := newWorld(t)
		g := newGeneration(t, w)
		head(w, 99)
		p := w.env.CreatePayment("25")
		bad := transferTo(t, p, 25, 0, 1) // block number 0 is invalid for the core
		if err := g.TransferObserved(ctx, bad); err != nil {
			t.Fatalf("err = %v", err)
		}
		huge := transferTo(t, p, 1, 100, 2)
		huge.Amount = new(big.Int).Lsh(big.NewInt(1), 300) // cannot be an ERC-20 amount
		if err := g.TransferObserved(ctx, huge); err != nil {
			t.Fatalf("err = %v", err)
		}
		if n := w.bridge.Status().Alerts[AlertFactRejected]; n != 2 {
			t.Fatalf("alerts = %+v", w.bridge.Status().Alerts)
		}
		if got := w.status(p); got != core.StatusAwaiting {
			t.Fatalf("status = %s", got)
		}
	})

	t.Run("a rejected head is never dropped", func(t *testing.T) {
		w := newWorld(t)
		g := newGeneration(t, w)
		err := g.HeadAdvanced(ctx, watcher.Head{Number: 0, Hash: evm.Hash{1}, Time: w.env.Clock.Now(), Final: 0})
		if err == nil || errors.Is(err, watcher.ErrHalt) {
			t.Fatalf("err = %v, want a retryable error", err)
		}
		at := w.env.Clock.Now().Add(-3 * time.Second).Truncate(time.Second)
		if err := g.HeadAdvanced(ctx, watcher.Head{Number: 7, Hash: evm.Hash{1}, Time: at, Final: 7}); err != nil {
			t.Fatal(err)
		}
		if h := w.coreHead(); h.Number != 7 || h.ChainID != 56 || !h.Time.Equal(at) {
			t.Fatalf("core head = %+v", h)
		}
	})

	// The core refuses a head whose block time is ahead
	// of its clock (core.ErrHeadTimeAhead). The watcher refuses such blocks
	// itself, so this is the second line; if it is ever reached, the head
	// is let go with an alert rather than retried for ever, and the core's
	// head does not move.
	t.Run("a head from the future is refused, alerted and not retried", func(t *testing.T) {
		w := newWorld(t)
		g := newGeneration(t, w)
		at := w.env.Clock.Now().Add(-3 * time.Second).Truncate(time.Second)
		if err := g.HeadAdvanced(ctx, watcher.Head{Number: 7, Hash: evm.Hash{1}, Time: at, Final: 7}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			future := watcher.Head{Number: 8 + uint64(i), Hash: evm.Hash{2}, Time: w.env.Clock.Now().Add(40 * 24 * time.Hour), Final: 8 + uint64(i)}
			if err := g.HeadAdvanced(ctx, future); err != nil {
				t.Fatalf("err = %v, want the head to be let go", err)
			}
		}
		if h := w.coreHead(); h.Number != 7 || !h.Time.Equal(at) {
			t.Fatalf("core head = %+v: a head from the future was recorded", h)
		}
		if n := w.bridge.Status().Alerts[AlertHeadRefused]; n != 1 {
			t.Fatalf("alerts = %+v, want one head_refused (repeats within a minute are not alerted again)", w.bridge.Status().Alerts)
		}
		// A single endpoint supplied it: that endpoint is set aside.
		if st := g.client.Status(); st[0].Healthy || !strings.Contains(st[0].LastError, "ahead of the server clock") {
			t.Fatalf("endpoint status = %+v", st[0])
		}
		if !strings.Contains(w.logs.String(), "the core refused processed head 8") {
			t.Error("refusal not logged")
		}
	})

	t.Run("caller gave up: the fact is kept", func(t *testing.T) {
		w := newWorld(t)
		g := newGeneration(t, w)
		head(w, 99)
		p := w.env.CreatePayment("25")
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		// The in-memory store does not look at the context; what matters
		// is that a context error, if the core returns one, is passed on.
		if err := g.outcome("observed transfer", transferTo(t, p, 25, 100, 1), cctx.Err()); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if err := g.outcome("observed transfer", transferTo(t, p, 25, 100, 1), errors.New("database is locked")); err == nil || errors.Is(err, watcher.ErrHalt) {
			t.Fatalf("a store failure must be retried: %v", err)
		}
	})
}

// TestWatchSetFromCore: what the watcher is told to watch is the core's
// chain state, read afresh every time.
func TestWatchSetFromCore(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	g := newGeneration(t, w)
	snap, err := g.Watched(ctx)
	if err != nil || len(snap.Addresses) != 0 || len(snap.Pending) != 0 {
		t.Fatalf("empty instance: %+v, %v", snap, err)
	}
	p := w.legacyPayment("25")
	if err := g.HeadAdvanced(ctx, watcher.Head{Number: 500, Hash: evm.Hash{5}, Time: w.env.Clock.Now(), Final: 500}); err != nil {
		t.Fatal(err)
	}
	q := w.env.CreatePayment("10")
	tr := transferTo(t, q, 10, 501, 1)
	if err := g.TransferObserved(ctx, tr); err != nil {
		t.Fatal(err)
	}
	snap, err = g.Watched(ctx)
	if err != nil {
		t.Fatal(err)
	}
	since := map[string]uint64{}
	for _, a := range snap.Addresses {
		since[a.Address.Hex()] = a.Since
	}
	// The first payment was created (by an earlier version) before the core
	// knew any head: its Since is derived from its creation time (the chain's newest block is
	// older than the payment, so that block minus the margin). The second
	// was created at head 500.
	if len(since) != 2 || since[p.Address] != chainStart-sinceMargin || since[q.Address] != 500 {
		t.Fatalf("addresses = %+v", snap.Addresses)
	}
	// The lookup is made once.
	before := w.nodes[0].Requests()
	if _, err := g.Watched(ctx); err != nil || w.nodes[0].Requests() != before {
		t.Fatalf("second snapshot made %d RPC requests (err %v)", w.nodes[0].Requests()-before, err)
	}
	if len(snap.Pending) != 1 {
		t.Fatalf("pending = %+v", snap.Pending)
	}
	got := snap.Pending[0]
	if got.TxHash != tr.TxHash || got.BlockHash != tr.BlockHash || got.LogIndex != tr.LogIndex || got.BlockNumber != 501 ||
		got.From != tr.From || got.To != tr.To || got.Amount.Cmp(tr.Amount) != 0 {
		t.Fatalf("pending transfer = %+v, want %+v", got, tr)
	}
	// Once it is final for the core it is no longer the watcher's concern.
	if err := g.HeadAdvanced(ctx, watcher.Head{Number: 501 + testConfirmations, Hash: evm.Hash{6}, Time: w.env.Clock.Now(), Final: 501 + testConfirmations}); err != nil {
		t.Fatal(err)
	}
	snap, _ = g.Watched(ctx)
	if len(snap.Pending) != 0 || len(snap.Addresses) != 2 {
		t.Fatalf("after finality: %+v", snap)
	}
	// And its payment is paid: the address stays in the watch set, closed.
	// The other payment is still awaiting, and its address is not.
	for _, a := range snap.Addresses {
		if want := a.Address.Hex() == q.Address; a.Closed != want {
			t.Fatalf("address %s: closed = %v, want %v (the paid payment's is %s)", a.Address.Hex(), a.Closed, want, q.Address)
		}
	}
}

// TestManyPaymentsWhileRunning: payments are created and paid while the
// watcher runs, blocks keep coming, an endpoint flaps — and every payment
// ends exactly as it should: paid ones paid, unpaid ones expired, nothing
// expired that was paid in time, no late funds.
func TestManyPaymentsWhileRunning(t *testing.T) {
	w := newWorld(t, withNodes(3))
	w.start()
	w.synced()

	rng := rand.New(rand.NewPCG(42, 42))
	var mu sync.Mutex // serialises chain and clock changes
	stopMining := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the chain produces blocks on its own
		defer wg.Done()
		for {
			select {
			case <-stopMining:
				return
			case <-time.After(2 * time.Millisecond):
				mu.Lock()
				// The clock of this test is the miner's: every block moves
				// it on by half a second, 250 times as fast as the wall
				// clock. A watcher that does not get to run for half a
				// second of a busy machine's time would find two minutes
				// gone and the newest block "from the future" — so the
				// chain does not run more than a hundred blocks (50 s)
				// ahead of what the core has processed.
				if cs, err := w.env.Service.ChainState(w.env.System()); err == nil && w.chain.Head().Number < cs.Head.Number+100 {
					w.advance(500 * time.Millisecond)
					w.mine(1)
				}
				mu.Unlock()
			}
		}
	}()

	type order struct {
		p    core.PaymentView
		paid bool
	}
	var orders []order
	for i := 0; i < 60; i++ {
		p := w.env.CreatePayment(fmt.Sprintf("%d", 5+i))
		o := order{p: p, paid: rng.IntN(4) > 0}
		if o.paid {
			mu.Lock()
			w.pay(p, int64(5+i))
			mu.Unlock()
		}
		orders = append(orders, o)
		switch i {
		case 15:
			w.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true }) // the primary dies
		case 30:
			w.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 3 })
		case 40:
			w.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = false })
			w.passCooldowns()
		}
		time.Sleep(time.Duration(rng.IntN(4)) * time.Millisecond)
	}
	for _, o := range orders {
		if o.paid {
			w.waitStatus(o.p, core.StatusPaid)
		}
	}
	// Let the unpaid ones run out: the clock jumps half an hour. Not under
	// a tick's feet, though. A tick reads the clock once, when it begins;
	// one that was running across the jump would see the next block stamped
	// half an hour ahead of its clock and take the endpoint that showed it
	// for one that serves blocks from the future (an endpoint_fault alert,
	// and this test's second way of failing under load: 1 of 10 race runs).
	// No clock does that to a running process. So no block is mined until a
	// tick that began after the jump has ended — ticks do not overlap, so
	// none that began before it is running then.
	mu.Lock()
	w.advance(core.DefaultPaymentTTL + 2*core.DefaultExpiryGrace)
	jumped := w.env.Clock.Now()
	w.eventually("a tick that began after the clock was moved on", func() bool {
		return !w.watcherStatus().LastTickAt.Before(jumped)
	})
	mu.Unlock()
	for _, o := range orders {
		if !o.paid {
			w.waitStatus(o.p, core.StatusExpired)
		}
	}
	close(stopMining)
	wg.Wait()

	for _, o := range orders {
		got := w.env.Payment(o.p.ID)
		ev := w.events(o.p)
		switch {
		case o.paid && (got.Status != core.StatusPaid || got.NeedsAttention || got.AmountConfirmed != got.Amount):
			t.Errorf("paid order: %+v (events: %s)", got, ev)
		case !o.paid && (got.Status != core.StatusExpired || got.AmountReceived != "0.00"):
			t.Errorf("unpaid order: %+v (events: %s)", got, ev)
		}
		if strings.Contains(ev, "late_funds") || strings.Contains(ev, "failed") || strings.Contains(ev, "underpaid") {
			t.Errorf("order %s: events = %s", o.p.ID, ev)
		}
	}
	if st := w.bridge.Status(); len(st.Alerts) != 0 {
		// The alert itself, not only its kind: the log tail a failed test
		// prints is too short to hold what raised it.
		t.Fatalf("alerts = %+v; the last one: %s: %s", st.Alerts, st.LastAlert.Kind, st.LastAlert.Message)
	}
}
