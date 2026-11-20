package watcher_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

var (
	usdt      = ethaddr.MustParse("0x55d398326f99059fF775485246999027B3197955")
	fakeToken = ethaddr.MustParse("0x55d398326f99059ff775485246999027b3197966") // look-alike contract
	payer     = ethaddr.MustParse("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
)

func addr(i int) ethaddr.Address {
	sum := ethaddr.Keccak256([]byte(fmt.Sprintf("deposit-%d", i)))
	a, _ := ethaddr.FromBytes(sum[:20])
	return a
}

func units(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}

// --- sink -----------------------------------------------------------------

type key struct {
	block evm.Hash
	tx    evm.Hash
	log   uint64
}

func keyOf(t watcher.Transfer) key { return key{t.BlockHash, t.TxHash, t.LogIndex} }

type event struct {
	kind string // "observed", "reverted", "head"
	tr   watcher.Transfer
	head watcher.Head
}

func (e event) String() string {
	if e.kind == "head" {
		return fmt.Sprintf("head(%d)", e.head.Number)
	}
	return fmt.Sprintf("%s(tx %x block %d)", e.kind, e.tr.TxHash[:3], e.tr.BlockNumber)
}

// sink records facts and keeps the model a real consumer would keep: the set
// of transfers currently believed to be on chain. It is idempotent, and it
// checks the guarantees the watcher documents.
type sink struct {
	t  testing.TB
	mu sync.Mutex

	events []event
	live   map[key]watcher.Transfer // observed and not reverted
	ever   map[key]bool             // ever observed
	head   watcher.Head
	dups   int
	// observations counts the transfers accepted, ever (events is emptied
	// by take).
	observations int

	failObserved, failReverted, failHead int // fail the next N calls
	onObserved, onReverted               func(watcher.Transfer)
	// lenientReverts accepts reverts of transfers the sink never accepted
	// (legitimate after a crash or a refused TransferObserved).
	lenientReverts bool
	// headRule, if set, is called with every accepted head and the
	// transfers the sink believes in at that moment. The harness uses it to
	// check the rule a consumer like the core relies on.
	headRule func(h watcher.Head, live map[key]watcher.Transfer)
	timeRule func(tr watcher.Transfer)
}

func newSink(t testing.TB) *sink {
	return &sink{t: t, live: make(map[key]watcher.Transfer), ever: make(map[key]bool)}
}

var errSink = errors.New("sink unavailable")

func (s *sink) TransferObserved(_ context.Context, tr watcher.Transfer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failObserved > 0 {
		s.failObserved--
		return errSink
	}
	if tr.Token != usdt {
		s.t.Errorf("observed a transfer of another token: %s", tr.Token)
	}
	if tr.Amount == nil || tr.Amount.Sign() <= 0 {
		s.t.Errorf("observed a transfer with amount %v", tr.Amount)
	}
	if tr.BlockHash.IsZero() || tr.TxHash.IsZero() {
		s.t.Errorf("observed a transfer without hashes: %+v", tr)
	}
	if tr.BlockTime.IsZero() {
		s.t.Errorf("observed a transfer without block time: tx %x block %d", tr.TxHash[:4], tr.BlockNumber)
	}
	if prev, again := s.live[keyOf(tr)]; again && !prev.BlockTime.Equal(tr.BlockTime) {
		s.t.Errorf("transfer %x reported again with another block time: %s, was %s", tr.TxHash[:4], tr.BlockTime, prev.BlockTime)
	}
	if s.timeRule != nil {
		s.timeRule(tr)
	}
	k := keyOf(tr)
	if _, dup := s.live[k]; dup {
		s.dups++
	}
	s.live[k] = tr
	s.ever[k] = true
	s.events = append(s.events, event{kind: "observed", tr: tr})
	s.observations++
	if s.onObserved != nil {
		s.onObserved(tr)
	}
	return nil
}

func (s *sink) TransferReverted(_ context.Context, tr watcher.Transfer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failReverted > 0 {
		s.failReverted--
		return errSink
	}
	k := keyOf(tr)
	if !s.ever[k] && !s.lenientReverts {
		s.t.Errorf("revert of a transfer that was never reported as observed: tx %s block %d", tr.TxHash, tr.BlockNumber)
	}
	delete(s.live, k)
	s.events = append(s.events, event{kind: "reverted", tr: tr})
	if s.onReverted != nil {
		s.onReverted(tr)
	}
	return nil
}

func (s *sink) HeadAdvanced(_ context.Context, h watcher.Head) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failHead > 0 {
		s.failHead--
		return errSink
	}
	if h.Hash.IsZero() {
		s.t.Errorf("head without hash")
	}
	if s.headRule != nil {
		s.headRule(h, s.live)
	}
	s.head = h
	s.events = append(s.events, event{kind: "head", head: h})
	return nil
}

func (s *sink) set(change func(s *sink)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(s)
}

// take returns the events recorded since the last call.
func (s *sink) take() []event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.events
	s.events = nil
	return out
}

func (s *sink) count(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.kind == kind {
			n++
		}
	}
	return n
}

func (s *sink) liveKeys() map[key]watcher.Transfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[key]watcher.Transfer, len(s.live))
	for k, v := range s.live {
		out[k] = v
	}
	return out
}

func (s *sink) lastHead() watcher.Head {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.head
}

// --- watch set and store --------------------------------------------------

type watchSet struct {
	mu  sync.Mutex
	set map[ethaddr.Address]bool
	// was holds every address that has ever been in the set.
	was   map[ethaddr.Address]bool
	since map[ethaddr.Address]uint64
	// closed are the addresses the consumer reports as closed
	// (WatchedAddress.Closed): watched, with nothing expected on them.
	closed  map[ethaddr.Address]bool
	pending []watcher.Transfer
	// processed is the consumer's processed head (Snapshot.Processed).
	processed uint64
	fail      int
	onRead    func()
	// readAddresses and readPending are the sizes of the snapshot the
	// watcher was given last, readOpen the number of its addresses that
	// were not closed.
	readAddresses, readPending, readOpen int
}

func newWatchSet(addrs ...ethaddr.Address) *watchSet {
	w := &watchSet{set: make(map[ethaddr.Address]bool), was: make(map[ethaddr.Address]bool), since: make(map[ethaddr.Address]uint64),
		closed: make(map[ethaddr.Address]bool)}
	w.add(addrs...)
	return w
}

func (w *watchSet) add(addrs ...ethaddr.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range addrs {
		w.set[a] = true
		w.was[a] = true
	}
}

// addSince adds an address together with the block before which it cannot
// have been paid.
func (w *watchSet) addSince(a ethaddr.Address, since uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.set[a] = true
	w.was[a] = true
	w.since[a] = since
}

func (w *watchSet) setPending(p ...watcher.Transfer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = p
}

// setProcessed sets what the watch set reports as the consumer's processed
// head.
func (w *watchSet) setProcessed(n uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.processed = n
}

func (w *watchSet) remove(a ethaddr.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.set, a)
	delete(w.closed, a)
}

// close marks addresses of the set as closed: their payments are settled,
// and they stay watched for whatever arrives late. reopen takes that back.
func (w *watchSet) close(addrs ...ethaddr.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range addrs {
		w.closed[a] = true
	}
}

func (w *watchSet) reopen(addrs ...ethaddr.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range addrs {
		delete(w.closed, a)
	}
}

func (w *watchSet) has(a ethaddr.Address) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.set[a]
}

// had reports whether a is or ever was in the set.
func (w *watchSet) had(a ethaddr.Address) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.was[a]
}

func (w *watchSet) Watched(context.Context) (watcher.Snapshot, error) {
	w.mu.Lock()
	hook := w.onRead
	w.mu.Unlock()
	if hook != nil {
		hook() // something happens just before the snapshot is taken
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail > 0 {
		w.fail--
		return watcher.Snapshot{}, errors.New("database unavailable")
	}
	snap := watcher.Snapshot{Pending: append([]watcher.Transfer(nil), w.pending...), Processed: w.processed}
	open := 0
	for a := range w.set {
		snap.Addresses = append(snap.Addresses, watcher.WatchedAddress{Address: a, Since: w.since[a], Closed: w.closed[a]})
		if !w.closed[a] {
			open++
		}
	}
	w.readAddresses, w.readPending, w.readOpen = len(snap.Addresses), len(snap.Pending), open
	return snap, nil
}

type store struct {
	mu        sync.Mutex
	data      []byte
	saves     int
	passSaves int // let this many saves through before failSave applies
	failSave  int // then fail this many
	failLoad  int
}

func (s *store) LoadCheckpoint(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failLoad > 0 {
		s.failLoad--
		return nil, errors.New("database unavailable")
	}
	return bytes.Clone(s.data), nil
}

func (s *store) SaveCheckpoint(_ context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.passSaves > 0 {
		s.passSaves--
	} else if s.failSave > 0 {
		s.failSave--
		return errors.New("disk full")
	}
	s.data = bytes.Clone(data)
	s.saves++
	return nil
}

func (s *store) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.data)
}

func (s *store) restore(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = bytes.Clone(data)
}

// --- harness --------------------------------------------------------------

const (
	testSettle = 6
	testWindow = 40
	startBlock = 10_000
	history    = 100 // blocks that exist below startBlock
)

type harness struct {
	t      testing.TB
	chain  *evmtest.Chain
	nodes  []*evmtest.Node
	urls   []string
	client *evm.Client
	clock  *fakeClock
	sink   *sink
	watch  *watchSet
	store  *store
	logs   *bytes.Buffer
	cfg    watcher.Config
	w      *watchdog
	// stalled counts consecutive ticks the watchdog found silently stalled;
	// lastCursor is the cursor after the previous tick, across restarts;
	// tickFrom is where the tick that is running started.
	stalled    int
	lastCursor uint64
	tickFrom   uint64
	// ruleFrom is the first block the head rule is checked from (payments
	// in earlier blocks predate the watcher's start).
	ruleFrom uint64
	// observedBefore is the number of transfers the sink had recorded when
	// the last tick ended (the watchdog counts what a tick reported).
	observedBefore int
	// onTickEnd, if set, is called after every tick the watchdog sees —
	// ticked by hand or by Run — with the tick's outcome.
	onTickEnd func(ok bool)
	// exempt, if set, names transfers the head rule and the model do not
	// demand: in histories where a lone log source hides them by design
	// until a second one makes up for its blocks, and those mined to an
	// address after its watch window ended.
	exempt func(tr watcher.Transfer) bool
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type option func(h *harness)

func withNodes(n int) option {
	return func(h *harness) {
		for len(h.nodes) < n {
			h.nodes = append(h.nodes, evmtest.NewNode(h.chain))
		}
	}
}

// withYoungChain replaces the harness's chain (whose head is startBlock) by
// one that begins at block 0 and is height blocks long. It must come before
// withNodes.
func withYoungChain(height int) option {
	return func(h *harness) {
		h.chain = evmtest.NewChain(56, 0)
		h.chain.AddToken(usdt, "USDT", 18)
		h.chain.AddToken(fakeToken, "USDT", 18)
		h.chain.Mine(height)
		h.nodes = []*evmtest.Node{evmtest.NewNode(h.chain)}
		h.ruleFrom = 1
	}
}

func withConfig(change func(c *watcher.Config)) option {
	return func(h *harness) { change(&h.cfg) }
}

// newHarness builds a chain whose head is startBlock with USDT deployed, one node (more
// with withNodes), and a watcher with small depths so scenarios stay short.
func newHarness(t testing.TB, opts ...option) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		chain: evmtest.NewChain(56, startBlock-history),
		clock: &fakeClock{now: time.Unix(1_800_000_000, 0)},
		sink:  newSink(t),
		// Nothing a test pays is mined at or below startBlock.
		ruleFrom: startBlock + 1,
		watch:    newWatchSet(),
		store:    &store{},
		logs:     &bytes.Buffer{},
	}
	h.chain.AddToken(usdt, "USDT", 18)
	h.chain.AddToken(fakeToken, "USDT", 18)
	h.chain.Mine(history) // the chain has a past: its head is startBlock
	h.nodes = []*evmtest.Node{evmtest.NewNode(h.chain)}
	h.cfg = watcher.Config{
		Token:        usdt,
		SettleDepth:  testSettle,
		ReorgWindow:  testWindow,
		CatchUpPause: time.Nanosecond,
		Sleep:        func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		Now:          h.clock.Now,
	}
	for _, opt := range opts {
		opt(h)
	}
	for _, n := range h.nodes {
		h.urls = append(h.urls, n.Serve(t))
	}
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: h.logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client, err := evm.NewClient(evm.Config{
		ChainID:   56,
		Endpoints: h.urls,
		Logger:    logger,
		Endpoint:  evm.EndpointOptions{Now: h.clock.Now, Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.client = client
	h.cfg.Client = client
	h.cfg.Sink = h.sink
	h.cfg.Watch = h.watch
	h.cfg.Store = h.store
	h.cfg.Logger = logger
	h.sink.headRule = h.checkHeadRule
	h.sink.timeRule = h.checkBlockTime
	h.restart()
	return h
}

// checkBlockTime: every transfer is reported with the timestamp of the block
// it is in. It runs on every transfer of every test.
func (h *harness) checkBlockTime(tr watcher.Transfer) {
	h.t.Helper()
	if blk, ok := h.chain.BlockByNumber(tr.BlockNumber); ok && blk.Hash == tr.BlockHash && tr.BlockTime.Unix() != int64(blk.Time) {
		h.t.Errorf("BLOCK TIME: transfer %x in block %d reported with time %s, the block's is %d", tr.TxHash[:4], tr.BlockNumber, tr.BlockTime, blk.Time)
	}
}

// checkHeadRule is the property the core builds on: when the watcher reports
// a processed head, that block is on the canonical chain with its real
// timestamp, every transfer to a watched address in any block up to it has
// been delivered, and nothing the sink still believes in at or below it has
// left the chain. It runs on every head of every test.
func (h *harness) checkHeadRule(head watcher.Head, live map[key]watcher.Transfer) {
	h.t.Helper()
	blk, ok := h.chain.BlockByNumber(head.Number)
	if !ok || blk.Hash != head.Hash {
		h.t.Errorf("HEAD RULE: processed head %d (%s) is not a block of the canonical chain", head.Number, head.Hash)
		return
	}
	if head.Time.Unix() != int64(blk.Time) {
		h.t.Errorf("HEAD RULE: head %d carries time %s, the block's is %d", head.Number, head.Time, blk.Time)
	}
	want := h.truthUpTo(h.ruleFrom, head.Number, h.watch.has)
	for k, tr := range want {
		if h.exempt != nil && h.exempt(tr) {
			continue
		}
		if _, delivered := live[k]; !delivered {
			h.t.Errorf("HEAD RULE: head %d reported, but the transfer %x in block %d to %s was never delivered",
				head.Number, tr.TxHash[:4], tr.BlockNumber, tr.To)
		}
	}
	// What the sink holds may be a transfer to an address that has left the
	// watch set since (nobody has to deliver those any more, but one that
	// was delivered is as true as it was).
	onChain := h.truthUpTo(h.ruleFrom, head.Number, h.watch.had)
	for k, tr := range live {
		if tr.BlockNumber > head.Number {
			continue
		}
		if _, canonical := onChain[k]; !canonical {
			h.t.Errorf("HEAD RULE: head %d reported while the sink still holds transfer %x of block %d (hash %x), which is not on the chain",
				head.Number, tr.TxHash[:4], tr.BlockNumber, k.block[:4])
		}
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// restart replaces the watcher with a fresh one reading the same store, as
// after a process restart.
func (h *harness) restart() {
	h.t.Helper()
	w, err := watcher.New(h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	h.w = &watchdog{Watcher: w, h: h}
}

// watchdog is the watcher of a harness, with two assertions added to every
// Tick of every test (and of the randomised model). No silent stall:
//
//	There is no tick that succeeds, leaves the processed head where it
//	was and gives no reason (Status.Hold), while an endpoint shows a chain
//	with more to settle.
//
// "Shows a chain with more to settle": during the tick, a node answered
// eth_blockNumber, or returned a "latest" block, more than two settle depths
// above the cursor — one depth is where the cursor rests when it is in step,
// the second is the distance at which two endpoints stop agreeing on where
// the chain ends. The assertion fires when that is so in two ticks in a row:
// a test may mine blocks in the middle of a tick, after the watcher has
// looked, and the next tick has to deal with them.
//
// And no idling on the job. A tick that declares the watcher idle
// (Status.Idle) is followed by a pause ten times as long, so the verdict
// has to be right in the tick that gives it:
//
//	There is no tick that leaves the watcher idle after it failed, held
//	the head, was given anything the consumer waits on (an address that
//	is not closed, a pending transfer), reported a transfer to the sink,
//	kept a transfer tracked or unconfirmed, or while an endpoint showed a
//	chain with more to settle — in that very tick, not two in a row. And
//	a tick that does not leave it idle says why (Status.Work).
//
// The same goes for quiet, which is idle with closed addresses in the watch
// set, and for telling the two apart:
//
//	A tick leaves the watcher quiet (Status.Quiet) exactly if it leaves
//	it idle and was given at least one address — all of them closed.
//
// Ticks run by Watcher.Run are checked through Config.OnTick (see
// simulation), which is why the checks live in the harness.
type watchdog struct {
	*watcher.Watcher
	h *harness
}

func (w *watchdog) Tick(ctx context.Context) error {
	w.h.beforeTick()
	err := w.Watcher.Tick(ctx)
	w.h.afterTick(err == nil)
	return err
}

// idleOn reports whether the harness's watcher idles at all (Config.
// IdleInterval: zero is the default, 30 s; off if not above PollInterval).
func (h *harness) idleOn() bool {
	poll := h.cfg.PollInterval
	if poll <= 0 {
		poll = watcher.DefaultPollInterval
	}
	return h.cfg.IdleInterval == 0 || h.cfg.IdleInterval > poll
}

// beforeTick prepares the watchdog's assertions for the next tick.
func (h *harness) beforeTick() {
	for _, n := range h.nodes {
		n.ResetShown()
	}
	// Where the tick starts: the cursor of the watcher; for one that has
	// not ticked yet, where its start puts it — the checkpoint's cursor,
	// or without one the block before Config.StartBlock (a start that
	// rewinds below where an earlier process got is a start, not a stall;
	// one that does not move from there is one). Not known: where the last
	// tick ended.
	h.tickFrom = h.w.Watcher.Status().Cursor
	if h.tickFrom == 0 {
		h.tickFrom = h.lastCursor
		if cp := h.store.snapshot(); len(cp) > 0 {
			if info, err := watcher.InspectCheckpoint(cp); err == nil {
				h.tickFrom = info.Cursor
			}
		} else if sb := h.cfg.StartBlock; sb != nil && *sb > 0 {
			h.tickFrom = *sb - 1
		}
	}
}

// afterTick makes the watchdog's assertions about the tick that has just
// ended (ok: without error).
func (h *harness) afterTick(ok bool) {
	if h.onTickEnd != nil {
		defer h.onTickEnd(ok)
	}
	st := h.w.Watcher.Status()
	var top uint64
	who := -1
	for i, n := range h.nodes {
		if s := n.Shown(); s > top {
			top, who = s, i
		}
	}
	depth := h.cfg.SettleDepth
	if depth == 0 {
		depth = watcher.DefaultSettleDepth
	}
	if ok && st.Hold == "" && st.Cursor != 0 && st.Cursor <= h.tickFrom && top > st.Cursor+2*depth {
		h.stalled++
	} else {
		h.stalled = 0
	}
	if h.stalled >= 2 {
		h.t.Errorf("WATCHDOG: the processed head has stayed at block %d for %d ticks that succeeded and named no reason (Status.Hold is empty), while endpoint %d shows the chain at block %d",
			st.Cursor, h.stalled, who, top)
	}
	h.lastCursor = st.Cursor

	h.watch.mu.Lock()
	addresses, pending, open := h.watch.readAddresses, h.watch.readPending, h.watch.readOpen
	h.watch.mu.Unlock()
	h.sink.mu.Lock()
	observed := h.sink.observations
	h.sink.mu.Unlock()
	reported := observed - h.observedBefore
	h.observedBefore = observed
	switch {
	case !st.Idle:
		if st.Work == "" && h.idleOn() {
			h.t.Errorf("IDLE WATCHDOG: the watcher is not idle after the tick at block %d and does not say why (Status.Work is empty)", st.Cursor)
		}
		if st.Quiet {
			h.t.Errorf("QUIET WATCHDOG: quiet without being idle after the tick at block %d (%s)", st.Cursor, st.Work)
		}
	case !ok || st.LastError != "":
		h.t.Errorf("IDLE WATCHDOG: idle after a tick that failed: %s", st.LastError)
	case st.Work != "":
		h.t.Errorf("IDLE WATCHDOG: idle, and at work: %s", st.Work)
	case st.Hold != "":
		h.t.Errorf("IDLE WATCHDOG: idle while the head is held: %s", st.Hold)
	case open != 0 || pending != 0:
		h.t.Errorf("IDLE WATCHDOG: idle after a tick that was given %d address(es) that are not closed (of %d) and %d pending transfer(s) to watch", open, addresses, pending)
	case reported != 0:
		h.t.Errorf("IDLE WATCHDOG: idle after a tick that reported %d transfer(s)", reported)
	case st.Quiet != (addresses != 0):
		h.t.Errorf("QUIET WATCHDOG: quiet=%v after an idle tick that was given %d address(es), all closed", st.Quiet, addresses)
	case st.Tracked != 0 || st.Unverified != 0:
		h.t.Errorf("IDLE WATCHDOG: idle with %d transfer(s) tracked and %d unconfirmed", st.Tracked, st.Unverified)
	case st.Solo:
		h.t.Errorf("IDLE WATCHDOG: idle in single-source operation")
	case top > st.Cursor+2*depth:
		h.t.Errorf("IDLE WATCHDOG: idle at block %d while endpoint %d shows the chain at block %d", st.Cursor, who, top)
	}
}

// restartWithNewClient is a restart of the whole process: the RPC client is
// new as well, so everything it had learnt about its endpoints (health, log
// ranges, whether they serve logs) is gone. Only the store survives.
func (h *harness) restartWithNewClient() {
	h.t.Helper()
	client, err := evm.NewClient(evm.Config{
		ChainID:   56,
		Endpoints: h.urls,
		Logger:    h.cfg.Logger,
		Endpoint:  evm.EndpointOptions{Now: h.clock.Now, Timeout: 5 * time.Second},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.client = client
	h.cfg.Client = client
	h.restart()
}

// tick runs one tick that is expected to succeed.
func (h *harness) tick() {
	h.t.Helper()
	if err := h.w.Tick(context.Background()); err != nil {
		h.t.Fatalf("Tick: %v\n%s", err, h.logs.String())
	}
}

// tickErr runs one tick that is expected to fail and returns the error.
func (h *harness) tickErr() error {
	h.t.Helper()
	err := h.w.Tick(context.Background())
	if err == nil {
		h.t.Fatalf("Tick succeeded, expected an error")
	}
	return err
}

// mineTicking mines n blocks one at a time with a tick after each. Every
// block is then a block the watcher has seen as a head, which is what lets
// the cursor come to rest exactly SettleDepth below the head.
func (h *harness) mineTicking(n int) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		h.chain.Mine(1)
		h.tick()
	}
}

// pay queues a USDT transfer to a deposit address.
func (h *harness) pay(to ethaddr.Address, amount int64) evm.Hash {
	return h.chain.Transfer(usdt, payer, to, units(amount))
}

// passCooldowns moves the clock past any endpoint cooldown.
func (h *harness) passCooldowns() { h.clock.Advance(2 * time.Minute) }

// passLogPause moves the clock past the longest pause after which an
// endpoint that is held to have no log service is asked for logs again.
func (h *harness) passLogPause() { h.clock.Advance(31 * time.Minute) }

// noLogService makes node i a provider without a log service — it answers
// every eth_getLogs with the given refusal — and takes the watcher through
// what it takes to conclude that: three refusals in a row, a minute and then
// two minutes apart (evm.LogsHealth.NoService). Until the third, the
// endpoint is a second log source that cannot be asked, and the head waits
// for it; the helper checks that, mining a block per tick. When it returns
// the verdict stands, and blocks are settled on the other endpoint's logs.
func (h *harness) noLogService(i int, refusal *evmtest.RPCError) {
	h.t.Helper()
	h.nodes[i].Set(func(b *evmtest.Behavior) { b.LogsError = refusal })
	for round := 1; round <= evm.LogsAbsentRefusals; round++ {
		for tries := 0; h.client.Status()[i].Logs.Refusals < round; tries++ {
			if tries > 3*testSettle {
				h.t.Fatalf("endpoint %d was not asked for logs a %d. time: %+v", i, round, h.w.Status())
			}
			h.chain.Mine(1)
			h.tick()
			if st, n := h.w.Status(), h.client.Status()[i].Logs.Refusals; n < evm.LogsAbsentRefusals && (st.LogsUnchecked || st.UncheckedFrom != 0) {
				h.t.Fatalf("blocks were settled on one endpoint's logs after %d refusal(s) of the other: %+v", n, st)
			}
		}
		if st := h.w.Status(); round < evm.LogsAbsentRefusals && !strings.Contains(st.Hold, "log verification unavailable") {
			h.t.Fatalf("after refusal %d the head must wait and say why: hold %q", round, st.Hold)
		}
		if logs := h.client.Status()[i].Logs; logs.NoService != (round == evm.LogsAbsentRefusals) {
			h.t.Fatalf("after refusal %d: no-service verdict = %v", round, logs.NoService)
		}
		if round < evm.LogsAbsentRefusals {
			h.clock.Advance(time.Minute<<(round-1) + time.Second)
		}
	}
}

// truth computes, from the canonical chain, the transfers a correct watcher
// must have reported and not reverted: well-formed non-zero USDT transfers
// to addresses in `watched`, in blocks from..head.
func (h *harness) truth(from uint64, watched func(ethaddr.Address) bool) map[key]watcher.Transfer {
	return h.truthUpTo(from, h.chain.Head().Number, watched)
}

// truthUpTo is truth restricted to blocks from..to.
func (h *harness) truthUpTo(from, to uint64, watched func(ethaddr.Address) bool) map[key]watcher.Transfer {
	out := make(map[key]watcher.Transfer)
	for n := from; n <= to; n++ {
		blk, ok := h.chain.BlockByNumber(n)
		if !ok {
			continue
		}
		logIndex := uint64(0)
		for _, tx := range blk.Txs {
			if tx.Failed {
				continue
			}
			for _, l := range tx.Logs {
				idx := logIndex
				logIndex++
				if l.Address != usdt || len(l.Topics) != 3 || l.Topics[0] != evm.TransferTopic || len(l.Data) != 32 {
					continue
				}
				var from, to ethaddr.Address
				copy(from[:], l.Topics[1][12:])
				copy(to[:], l.Topics[2][12:])
				amount := new(big.Int).SetBytes(l.Data)
				if amount.Sign() == 0 || from == to || !watched(to) {
					continue
				}
				tr := watcher.Transfer{Token: usdt, From: from, To: to, Amount: amount,
					TxHash: tx.Hash, LogIndex: idx, BlockNumber: blk.Number, BlockHash: blk.Hash}
				out[keyOf(tr)] = tr
			}
		}
	}
	return out
}

// assertModel checks that the sink's model equals the chain's truth.
func (h *harness) assertModel(from uint64) {
	h.t.Helper()
	h.assertModelFor(from, h.watch.has)
}

func (h *harness) assertModelFor(from uint64, watched func(ethaddr.Address) bool) {
	h.t.Helper()
	want := h.truth(from, watched)
	got := h.sink.liveKeys()
	var problems []string
	for k, tr := range want {
		if _, delivered := got[k]; !delivered && h.exempt != nil && h.exempt(tr) {
			continue // not demanded yet (harness.exempt)
		}
		g, ok := got[k]
		if !ok {
			problems = append(problems, fmt.Sprintf("MISSED transfer tx %x in block %d to %s", tr.TxHash[:4], tr.BlockNumber, tr.To))
			continue
		}
		if g.Amount.Cmp(tr.Amount) != 0 || g.From != tr.From || g.To != tr.To || g.BlockNumber != tr.BlockNumber || g.Token != tr.Token {
			problems = append(problems, fmt.Sprintf("transfer tx %x reported with wrong fields: %+v", tr.TxHash[:4], g))
		}
	}
	for k, tr := range got {
		if _, ok := want[k]; !ok {
			problems = append(problems, fmt.Sprintf("PHANTOM transfer tx %x in block %d (hash %x) is not on the canonical chain", tr.TxHash[:4], tr.BlockNumber, k.block[:4]))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		h.t.Fatalf("sink model differs from the chain (%d live, %d expected):\n  %s", len(got), len(want), joinLines(problems))
	}
}

func joinLines(lines []string) string {
	if len(lines) > 12 {
		lines = append(lines[:12], fmt.Sprintf("... and %d more", len(lines)-12))
	}
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n  "
		}
		out += l
	}
	return out
}

func kinds(events []event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.kind
	}
	return out
}
