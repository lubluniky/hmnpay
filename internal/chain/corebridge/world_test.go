package corebridge

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
	"github.com/lubluniky/hmnpay/internal/wallet"
)

// The end-to-end tests run the real core (on its in-memory store), the real
// bridge with its real supervisor loop, the real watcher and the real RPC
// client against the scripted chain of package evmtest. Nothing between the
// payer's transaction and the payment's status is faked.

const (
	// Account xpub (m/44'/60'/0') of the Hardhat development mnemonic.
	testXpub = "xpub6Ce9NcJvTk36xtLSrJLZqE7wtgA5deCeYs7rSQtreh4cj6ByPtrg9sD7V2FNFLPnf8heNP3FGkeV9qwfzvZNSd54JoNXVsXFYSYwHsnJxqP"

	testSettle        = 4
	testConfirmations = 3
	// testFinality is how far the nodes' finalized block trails their tip,
	// unless a test asks for another depth (withFinality): as deep as the
	// processed head and the confirmations together, so that a transfer
	// becomes final — finalised by the chain, with its confirmations — in
	// the block it would by its confirmations alone. Reorganisations up to
	// this deep keep every finalised block.
	testFinality = testSettle + testConfirmations - 1
	chainStart   = 50_000
	chainHistory = 200
)

var (
	usdt = ethaddr.MustParse("0x55d398326f99059fF775485246999027B3197955")
	// Not one of the wallet's own addresses: the first deposit address of
	// the Hardhat xpub is the well-known 0xf39F…2266, and a transfer from
	// an address to itself is (rightly) not a payment.
	payer = ethaddr.MustParse("0x8894E0a0c962CB723c1976a4421c95949bE2D4E3")
)

// checkpoints is the integrator's checkpoint table.
type checkpoints struct {
	mu   sync.Mutex
	data []byte
	// onSave, if set, is called before a checkpoint is written (a test
	// can let the process die there).
	onSave func()
}

func (c *checkpoints) LoadCheckpoint(context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.data), nil
}

func (c *checkpoints) SaveCheckpoint(_ context.Context, data []byte) error {
	if c.onSave != nil {
		c.onSave()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = bytes.Clone(data)
	return nil
}

func (c *checkpoints) set(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = bytes.Clone(data)
}

func (c *checkpoints) get() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.data)
}

// world is one hmnpay instance plus the chain it watches.
type world struct {
	t     *testing.T
	env   *coretest.Env
	chain *evmtest.Chain
	nodes []*evmtest.Node
	urls  []string
	store *checkpoints
	logs  *safeBuffer
	opts  Options
	// finality is how far the nodes' finalized block trails their tip.
	finality uint64
	alerts   chan Alert

	// skew is added to the endpoints' health clock, to let cooldowns pass.
	skew atomic.Int64
	// futureHeadsAllowed leaves the core's guard against heads from the
	// future switched off (HMNPAY_MAX_HEAD_TIME_AHEAD=off).
	futureHeadsAllowed bool

	bridge *Bridge
	stop   func()
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type worldOption func(w *world, envOpts *[]coretest.EnvOption)

func withNodes(n int) worldOption {
	return func(w *world, _ *[]coretest.EnvOption) {
		for len(w.nodes) < n {
			w.nodes = append(w.nodes, evmtest.NewNode(w.chain))
		}
	}
}

// withFinality has the nodes report the block depth below their tip as
// finalized.
func withFinality(depth uint64) worldOption {
	return func(w *world, _ *[]coretest.EnvOption) { w.finality = depth }
}

func withoutSetup() worldOption {
	return func(_ *world, envOpts *[]coretest.EnvOption) { *envOpts = append(*envOpts, coretest.WithoutSetup()) }
}

// withFutureHeadsAllowed switches the check on block timestamps off on both
// sides, as the operator's setting does.
func withFutureHeadsAllowed() worldOption {
	return func(w *world, _ *[]coretest.EnvOption) {
		w.futureHeadsAllowed = true
		w.opts.MaxHeadTimeAhead = -1
	}
}

func withOptions(f func(o *Options)) worldOption {
	return func(w *world, _ *[]coretest.EnvOption) { f(&w.opts) }
}

// newWorld builds the chain (head at chainStart, with history), its nodes,
// and a core instance set up for BSC mainnet USDT with those nodes as RPC
// endpoints, the real address source and the Hardhat xpub. The bridge is not
// started.
func newWorld(t *testing.T, opts ...worldOption) *world {
	t.Helper()
	w := &world{
		t:        t,
		chain:    evmtest.NewChain(core.ChainBSCMainnet, chainStart-chainHistory),
		store:    &checkpoints{},
		logs:     &safeBuffer{},
		alerts:   make(chan Alert, 64),
		finality: testFinality,
	}
	w.chain.AddToken(usdt, "USDT", 18)
	// Chain time is the core's clock (which starts at coretest.Epoch): the
	// watcher compares block timestamps with that clock.
	w.chain.SetTime(coretest.Epoch)
	w.chain.Mine(chainHistory)
	w.nodes = []*evmtest.Node{evmtest.NewNode(w.chain)}
	w.opts = Options{
		PollInterval: 3 * time.Millisecond,
		// Ten poll intervals, as in production (3 s and 30 s): the watcher
		// of these tests idles whenever the core lists nothing to watch,
		// and what a test expects of an idle watcher — a dead endpoint
		// shows in the status, the head follows the chain — it gets within
		// an idle interval.
		IdlePollInterval: 30 * time.Millisecond,
		SettleDepth:      testSettle,
		StatusInterval:   10 * time.Millisecond,
		ReloadInterval:   25 * time.Millisecond,
		MaxBackoff:       10 * time.Millisecond,
		Endpoint: evm.EndpointOptions{
			Timeout: 5 * time.Second,
			Now:     func() time.Time { return time.Now().Add(time.Duration(w.skew.Load())) },
		},
		OnAlert: func(a Alert) {
			select {
			case w.alerts <- a:
			default:
			}
		},
	}
	var envOpts []coretest.EnvOption
	for _, o := range opts {
		o(w, &envOpts)
	}
	for _, n := range w.nodes {
		n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = w.finality })
		w.urls = append(w.urls, n.Serve(t))
	}
	w.opts.Logger = slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	envOpts = append(envOpts,
		// The core's processed head is the bridge's to report.
		coretest.WithoutChainHead(),
		coretest.WithConfig(func(c *core.Config) {
			c.Addresses = wallet.NewAddressSource()
			c.Prober = NewProber(ProberOptions{})
			// The core's own guard against heads from the future, which
			// the test environment switches off, is on here as it is in
			// production: the chain of these tests keeps the core's time.
			if c.MaxHeadTimeAhead < 0 && !w.futureHeadsAllowed {
				c.MaxHeadTimeAhead = 0
			}
		}),
		coretest.WithSetup(func(in *core.SetupInput) {
			in.RPCURLs = w.urls
			in.XPub = testXpub
		}),
	)
	w.env = coretest.NewEnv(t, envOpts...)
	// Chain time follows the core's clock: a block is stamped with the
	// clock's reading when it is mined.
	w.chain.SetTime(w.env.Clock.Now())
	if st, _ := w.env.Service.SetupStatus(context.Background()); st.Completed {
		w.setConfirmations(testConfirmations)
	}
	t.Cleanup(func() {
		if w.stop != nil {
			w.stop()
		}
		if t.Failed() {
			t.Logf("log (info and above, tail):\n%s", tail(infoLines(w.logs.String()), 1500))
		}
	})
	return w
}

// infoLines drops the per-tick debug lines.
func infoLines(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.Contains(line, "level=DEBUG") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func (w *world) setConfirmations(n int) {
	w.t.Helper()
	st, err := w.env.Service.GetSettings(w.env.Operator())
	if err != nil {
		w.t.Fatal(err)
	}
	st.Payments.RequiredConfirmations = n
	if _, err := w.env.Service.UpdatePaymentDefaults(w.env.Operator(), st.Payments); err != nil {
		w.t.Fatal(err)
	}
}

// start runs a new Bridge (a new process, as far as the bridge is concerned)
// on the same core and checkpoint store.
func (w *world) start() {
	w.t.Helper()
	if w.stop != nil {
		w.t.Fatal("bridge already running")
	}
	b, err := New(w.env.Service, w.store, w.opts)
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	w.bridge = b
	w.stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			w.t.Error("bridge did not stop")
		}
		w.stop = nil
	}
}

// eventually waits until cond holds.
func (w *world) eventually(what string, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			st := w.bridge.Status()
			w.t.Fatalf("timed out waiting for: %s\nbridge: state=%s reason=%q generation=%d alerts=%v\nwatcher: cursor=%d tip=%d unverified=%d tracked=%d errors=%d last_error=%q",
				what, st.State, st.Reason, st.Generation, st.Alerts, st.Watcher.Cursor, st.Watcher.Tip.Number, st.Watcher.Unverified,
				st.Watcher.Tracked, st.Watcher.ConsecutiveErrors, st.Watcher.LastError)
		}
		time.Sleep(time.Millisecond)
	}
}

// never checks that cond does not become true for a while: long enough for
// many ticks of the 3 ms poll loop.
func (w *world) never(what string, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cond() {
			w.t.Fatalf("must not happen: %s (watcher cursor %d)", what, w.bridge.Status().Watcher.Cursor)
		}
		time.Sleep(time.Millisecond)
	}
}

// mine mines n blocks stamped with the core clock's current time.
func (w *world) mine(n int) evmtest.Block {
	w.chain.SetTime(w.env.Clock.Now())
	return w.chain.Mine(n)
}

// advance moves the clock (and so the timestamps of later blocks) forward.
func (w *world) advance(d time.Duration) { w.env.Clock.Advance(d) }

// coreHead returns the head the core has processed.
func (w *world) coreHead() core.ChainHead {
	w.t.Helper()
	cs, err := w.env.Service.ChainState(w.env.System())
	if err != nil {
		w.t.Fatal(err)
	}
	return cs.Head
}

// synced waits until the core's head is the block SettleDepth below the
// chain's tip — everything the chain has is processed.
func (w *world) synced() {
	w.t.Helper()
	w.eventually("core head to reach tip - settle depth", func() bool {
		tip := w.chain.Head().Number
		blk, ok := w.chain.BlockByNumber(tip - testSettle)
		h := w.coreHead()
		return ok && h.Number == blk.Number && h.Hash == blk.Hash.String()
	})
}

// pay sends amount (whole units) of USDT to a payment's address; the
// transaction is mined with the next block.
func (w *world) pay(p core.PaymentView, amount int64) evm.Hash {
	w.t.Helper()
	to, err := parseAddress(p.Address)
	if err != nil {
		w.t.Fatal(err)
	}
	return w.chain.Transfer(usdt, payer, to, units(amount))
}

func units(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}

func (w *world) status(p core.PaymentView) core.PaymentStatus {
	return w.env.Payment(p.ID).Status
}

func (w *world) waitStatus(p core.PaymentView, want core.PaymentStatus) core.PaymentView {
	w.t.Helper()
	w.eventually("payment "+p.ID+" to become "+string(want)+" (it was "+string(w.status(p))+" when the wait began)", func() bool {
		return w.status(p) == want
	})
	return w.env.Payment(p.ID)
}

// events returns the payment's event types, without the leading
// payment.created.
func (w *world) events(p core.PaymentView) string {
	types := w.env.EventTypes(p.ID)
	names := make([]string, 0, len(types))
	for _, typ := range types {
		names = append(names, strings.TrimPrefix(string(typ), "payment."))
	}
	return strings.Join(names, " ")
}

// writeOffLogs lets the watcher conclude that an endpoint which refuses
// eth_getLogs has no log service. That takes three refusals in a row, a
// minute and then two minutes apart on the endpoints' clock; until the third
// the head waits for the endpoint, and the status says so — which is what
// this waits for before it moves that clock on.
func (w *world) writeOffLogs() {
	w.t.Helper()
	for round := 1; round < evm.LogsAbsentRefusals; round++ {
		want := fmt.Sprintf("has refused eth_getLogs %d time(s)", round)
		w.eventually("the head to wait for the endpoint that refused a log query: "+want, func() bool {
			return strings.Contains(w.watcherStatus().Hold, want)
		})
		w.skew.Add(int64(time.Minute<<(round-1) + time.Second))
	}
}

// passCooldowns lets every endpoint cooldown expire.
func (w *world) passCooldowns() { w.skew.Add(int64(10 * time.Minute)) }

// watcherStatus returns the running watcher's status.
func (w *world) watcherStatus() watcher.Status { return w.bridge.Status().Watcher }

// legacyPayment creates a payment the way versions before core.ErrChainNotReady
// could: while the core had not processed a head of the chain, so that it has
// no start block (CreatedAtBlock). The core no longer creates such payments;
// they exist in databases those versions wrote, and the bridge derives their
// start block from their creation time.
func (w *world) legacyPayment(amount string) core.PaymentView {
	w.t.Helper()
	if h := w.coreHead(); h.Number != 0 {
		w.t.Fatalf("legacyPayment: the core has processed head %d", h.Number)
	}
	sys := w.env.System()
	if err := w.env.Service.AdvanceHead(sys, core.ChainHead{ChainID: core.ChainBSCMainnet, Number: 1, Hash: coretest.BlockHash(1, 0), Time: w.env.Clock.Now(), Final: 1}); err != nil {
		w.t.Fatal(err)
	}
	p := w.env.CreatePayment(amount)
	err := w.env.Store.Update(context.Background(), func(tx core.Tx) error {
		stored, err := tx.GetPayment(sys, p.ID)
		if err != nil {
			return err
		}
		stored.CreatedAtBlock = 0
		if err := tx.UpdatePayment(sys, stored); err != nil {
			return err
		}
		return tx.SaveChainHead(sys, core.ChainHead{ChainID: core.ChainBSCMainnet})
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return w.env.Payment(p.ID)
}

// firstHead waits until the core has processed a head: from then on payments
// can be created.
func (w *world) firstHead() {
	w.t.Helper()
	w.eventually("the core's first processed head", func() bool { return w.coreHead().Number != 0 })
}
