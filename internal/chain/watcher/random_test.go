package watcher_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// TestRandomisedModel drives the watcher through random histories — payments,
// noise, reorgs, endpoint outages, lag, provider limits, rate limits, sink
// and store failures, restarts in the middle of all that — and checks the one
// property everything else exists for:
//
//	once the endpoints are healthy again, the set of transfers the sink
//	believes in is exactly the set of transfers on the canonical chain.
//
// Not one missed, not one phantom. The same check runs after every quiet tick
// along the way.
//
// In half of the histories with two or more endpoints one endpoint is
// malicious (see runRandomHistory); the head rule, checked on every reported
// head, must hold there as everywhere.
//
// And one more, on every tick of every history (it is part of the harness:
// see watchdog): the processed head is never held silently. No tick succeeds,
// leaves the head where it was and names no reason while an endpoint shows a
// chain with more to settle.
//
// Idle stretches. Up to twice in a history every watch window ends: the
// watch set empties, and from there the history is driven by Watcher.Run in
// simulated time instead of tick by tick (idleStretch). The watcher has to
// come to rest idle; it sits through idle pauses during which the chain is
// reorganised and endpoints fail; then a payment is created — with the
// consumer's stale head as its start block — and the watcher is woken, or
// not woken. Asserted there: every pause is the one the status calls for (the
// idle interval only after a tick that left the watcher idle, the poll
// interval otherwise, a backoff after a failure), a watcher with nothing to
// watch and healthy endpoints does idle, the tick that follows a wake-up
// runs at that moment and reports the payment, and a payment nobody woke the
// watcher for is reported by the tick that ends the pause. The watchdog's
// second assertion — no tick leaves the watcher idle while it has anything
// to watch or to do, and one that does not says why — runs on every tick of
// the history, driven either way.
//
// Quiet stretches. Every other time the watch set is not emptied: a few of
// its addresses stay, as closed ones (WatchedAddress.Closed) — the payments
// are settled, the addresses are watched for whatever arrives late — and the
// watcher has to come to rest quiet instead. During its pauses, besides the
// reorganisations and the endpoint failures, transfers to the closed
// addresses are mined (and reorganised away, and mined again). Nothing wakes
// the watcher for them: each has to be reported by the tick that ends the
// pause it was mined in, that tick must not leave the watcher at rest, and
// the model — the sink holds exactly what the chain holds — is asserted when
// the watcher has come to rest again. The closed addresses stay in the watch
// set for the remainder of the history, among the open ones of the payments
// that follow, and are paid to like any other. The watchdog checks every
// verdict: quiet exactly when at rest with addresses, all of them closed.
//
// The list of endpoints changes. Restarts with a new client come with the
// endpoints reordered, or one of them replaced by a new endpoint put
// anywhere in the list. Some histories begin with a single endpoint that
// leaves payments out of its logs, and goes on leaving them out: the blocks
// it settles alone are recorded as unchecked, and when an honest endpoint is
// added — in front of it or behind it — the payments have to be found by an
// endpoint other than the one that hid them, whatever the order (invariant
// 3). In some pools of three, one endpoint never serves logs. Under best
// effort, outages hand over: the endpoint that was down comes back as
// another goes down, and on every tick, by hand or by Run, an endpoint
// believed alone must have had every other endpoint silent beside it for
// SoloPatience ticks of its own. And the malicious endpoint may lie about
// blocks only inside batches — as the blocks of transfers are asked for, to
// date them — leaving them out or naming others: with three endpoints that
// must not keep a transfer's block undated (invariant 4).
//
// Labels and windows. The lone log source that hid payments may stay, under
// another URL, when the honest endpoint is added: the blocks it settled name
// its old label, and its scan under the new one must not count as the second
// (only endpoints configured now count). Honest endpoints are sometimes
// configured twice, under two URLs. In some pools of two, one endpoint never
// gets a log service: blocks are settled on the other's logs for good, and
// on every tick Head.Checked must be no later than the first block still to
// be scanned by a second log endpoint (and zero when none is, or with a
// single endpoint). In others it gets one at a step of the history, and the
// other endpoint hides payments until then; the addresses they were sent to
// have a watch window that ends soon after, and leave the watch set as the
// consumer retires them — once the reported head's time and Head.Checked are
// past it — so the payments have to be found before that.
//
// The rules that decide for how long and from where an address is looked
// for — a payment's watch window, its start block — are the consumer's, and
// are under test where the consumer is real: TestRandomisedCoreModel in
// package corebridge.
//
// HMNPAY_MODEL_SEEDS sets the number of histories (default 60), and
// HMNPAY_MODEL_FIRST_SEED the first of them (default 0). A soak:
//
//	HMNPAY_MODEL_SEEDS=40000 go test ./internal/chain/watcher -run 'TestRandomisedModel$' -count=1 -timeout 2h
func TestRandomisedModel(t *testing.T) {
	seeds := 60
	if env := os.Getenv("HMNPAY_MODEL_SEEDS"); env != "" {
		n, err := strconv.Atoi(env)
		if err != nil {
			t.Fatalf("HMNPAY_MODEL_SEEDS: %v", err)
		}
		seeds = n
	}
	if testing.Short() {
		seeds = min(seeds, 10)
	}
	// HMNPAY_MODEL_FIRST_SEED starts the histories at another seed, to split
	// a soak into parts.
	offset := 0
	if env := os.Getenv("HMNPAY_MODEL_FIRST_SEED"); env != "" {
		n, err := strconv.Atoi(env)
		if err != nil {
			t.Fatalf("HMNPAY_MODEL_FIRST_SEED: %v", err)
		}
		offset = n
	}
	// Histories run in parallel, in groups. The testing package keeps every
	// parallel subtest of a test — and what its cleanup functions hold on
	// to: here the whole harness of a history, some 170 kB — until the
	// parent test is done. With all histories under one parent, memory grew
	// with their number (2.8 GB for a long soak); a group is released when
	// it completes.
	const group = 50
	for first := offset; first < offset+seeds; first += group {
		last := min(first+group, offset+seeds) - 1
		t.Run(fmt.Sprintf("seeds=%d-%d", first, last), func(t *testing.T) {
			for seed := first; seed <= last; seed++ {
				t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
					t.Parallel()
					runRandomHistory(t, uint64(seed))
				})
			}
		})
	}
}

const maxReorgDepth = 9

// TestRandomisedModelDebug replays the single history named by
// HMNPAY_MODEL_DEBUG_SEED and prints the watcher's log if it fails.
func TestRandomisedModelDebug(t *testing.T) {
	env := os.Getenv("HMNPAY_MODEL_DEBUG_SEED")
	if env == "" {
		t.Skip("set HMNPAY_MODEL_DEBUG_SEED to replay one history with logs")
	}
	seed, err := strconv.ParseUint(env, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	debugLogs = true
	runRandomHistory(t, seed)
}

var debugLogs bool

func runRandomHistory(t *testing.T, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 0xfeed))
	nodes := 1 + rng.IntN(3)
	noFinality := rng.IntN(3) == 0
	mode := watcher.VerifyAuto
	if rng.IntN(4) == 0 {
		mode = watcher.VerifyBestEffort
	}
	// Incidents, stamped with the harness clock.
	var incMu sync.Mutex
	lastHeld := time.Time{}
	// repairs counts the times the watcher gave up blocks of its
	// checkpoint that both endpoints' chains no longer reached
	// (IncidentCheckpointAhead): a finalized block among them goes with
	// them.
	repairs := 0
	var clock func() time.Time
	h := newHarness(t, withNodes(nodes), withConfig(func(c *watcher.Config) {
		c.NoFinality = noFinality
		c.Verify = mode
		c.OnIncident = func(i watcher.Incident) {
			if i.Kind == watcher.IncidentCheckpointAhead {
				incMu.Lock()
				repairs++
				incMu.Unlock()
			}
			if i.Kind == watcher.IncidentHeadHeld {
				incMu.Lock()
				lastHeld = clock()
				incMu.Unlock()
			}
		}
	}))
	clock = h.clock.Now
	// The watcher's pauses are the simulation's, for the stretches of the
	// history that are driven by Run (idleStretch). Ticks driven one by one
	// are not affected: a tick pauses only between scan steps.
	sim := newSimulation(h)
	// In half of the histories with two or more endpoints and the default
	// mode, one endpoint is malicious: every now and then it lies about the
	// chain's height, about block hashes or timestamps, hides transfers
	// from its logs or invents one, keeps serving the logs of blocks that
	// were reorganised away, or forgets a block when asked about it after a
	// log scan. That is the configuration in which one lying endpoint must
	// be able to delay facts and nothing else. The invariants of the
	// package documentation are asserted directly, on every fact:
	//
	//  1. every transfer reported is on the canonical chain at that moment
	//     (onObserved below), and no head is reported while the consumer
	//     holds a transfer at or below it that is not (the head rule);
	//  2. every head is a block of the canonical chain with its real
	//     timestamp (the head rule);
	//  3. every transfer at or below a reported head has been reported (the
	//     head rule), and with two or more endpoints that serve logs no
	//     block is ever settled on one endpoint's logs (LogsUnchecked);
	//  4. a head that is held for more than a few ticks has been alerted.
	liar := -1
	if nodes >= 2 && mode == watcher.VerifyAuto && rng.IntN(2) == 0 {
		liar = rng.IntN(nodes)
	}
	// With HMNPAY_MODEL_DEBUG_SEED the history is written into the watcher's
	// log as it unfolds — what the chain did, what each endpoint was made to
	// do, how each tick ended — so that a failing seed can be read, and
	// rewritten as a test that does not need the generator. (trace draws no
	// random numbers: a history is the same with and without it.)
	trace := func(format string, args ...any) {
		if debugLogs {
			fmt.Fprintf(&lockedWriter{w: h.logs}, "TRACE "+format+"\n", args...)
		}
	}
	trace("seed=%d nodes=%d noFinality=%v mode=%d liar=%d urls=%v", seed, nodes, noFinality, mode, liar, h.urls)
	// In some of the other histories one endpoint has no log service when
	// the watcher first meets it: it refuses eth_getLogs, in the words of a
	// provider that has none, until things get better for the first time.
	// It has never answered a log query, so after three refusals over three
	// minutes — not before — blocks are settled on the other endpoint's
	// logs; and once it serves logs, every block settled without it is
	// scanned by it (the record of unchecked blocks is closed at the end).
	noLogsAtFirst := -1
	if nodes >= 2 && liar < 0 && mode == watcher.VerifyAuto && rng.IntN(5) == 0 {
		noLogsAtFirst = rng.IntN(nodes)
	}
	// logsException is set once any endpoint has been held to have no log
	// service (see the tick's check of invariant 3).
	logsException := false
	// What the model generates besides is drawn from a stream of its own,
	// so that a history in which none of it happens is the one it was.
	rng2 := rand.New(rand.NewPCG(seed, 0xbeef))
	// A lone log source that hides payments. The history begins with a
	// single endpoint, trusted with everything: it leaves some payments out
	// of its logs (and goes on leaving them out), and the blocks it settles
	// are recorded as unchecked. Some way into the history an honest
	// endpoint is added — in front of it or behind it in the list — and the
	// payments have to be found: by the new endpoint's scan of the recorded
	// blocks, whatever the order of the two, never by the one that hid them
	// (invariant 3). Until then the head rule does not demand them.
	soleSource := nodes == 1 && mode == watcher.VerifyAuto && rng2.IntN(2) == 0
	aloneUntil := 40 + rng2.IntN(140)
	var hiddenMu sync.Mutex
	hidden := map[evm.Hash]bool{}
	hiding := true // until the convergence: then it lies no more, like every liar
	isHidden := func(tx evm.Hash) bool {
		hiddenMu.Lock()
		defer hiddenMu.Unlock()
		return hidden[tx]
	}
	hides := func(tx evm.Hash) bool {
		hiddenMu.Lock()
		defer hiddenMu.Unlock()
		return hiding && hidden[tx]
	}
	// A pool in which one of three endpoints never serves logs: two that do
	// remain, and nothing is settled on one endpoint's logs.
	noLogsEver := -1
	if nodes == 3 && liar < 0 && noLogsAtFirst < 0 && rng2.IntN(3) == 0 {
		noLogsEver = rng2.IntN(3)
	}
	trace("soleSource=%v aloneUntil=%d noLogsEver=%d", soleSource, aloneUntil, noLogsEver)
	// Further generators, from a stream of their own (histories without
	// them stay what they were).
	rng3 := rand.New(rand.NewPCG(seed, 0x5eed))
	// A relabelled lone log source: when the honest endpoint is added, the
	// one that hid payments stays, under another URL — the same provider,
	// another label. The blocks it settled name its old label, which is not
	// configured any more; its scan under the new one must not count as the
	// second.
	relabelHider := soleSource && rng3.IntN(2) == 0
	// A returning pair: instead of the honest endpoint joining the lone
	// log source, two new ones replace it for a while — one of them scans
	// the blocks it settled — and then the lone source comes back together
	// with that witness. Both are on record for the blocks and both are
	// configured: they have been checked, and must not wait for a scan
	// that nobody will make.
	returningPair := soleSource && !relabelHider && rng3.IntN(2) == 0
	// Two endpoints of which one has no log service: for good (blocks are
	// settled on the other's logs for the rest of the history; nobody can
	// check them), or — the endpoint that had none at first — until a step
	// of the history, with the other one hiding payments meanwhile. The
	// addresses paid by a hidden payment have a watch window that ends soon
	// after it, and leave the watch set — retired by the processed head, as
	// the consumer does it — once the head is past the window and
	// Head.Checked is too: the blocks are still to be scanned by the endpoint
	// that had no log service, and that scan can only find the payment
	// while the address is watched.
	noLogsForGood := nodes == 2 && noLogsAtFirst >= 0 && rng3.IntN(3) == 0
	loglessHider := nodes == 2 && noLogsAtFirst >= 0 && !noLogsForGood && rng3.IntN(2) == 0
	loglessUntil := 40 + rng3.IntN(120)
	loglessOn := loglessHider
	trace("relabelHider=%v noLogsForGood=%v loglessHider=%v loglessUntil=%d", relabelHider, noLogsForGood, loglessHider, loglessUntil)
	// Watch windows that end (loglessHider): an address with one has left
	// the watch set once retired, and what is mined to it after its window
	// is nobody's to deliver.
	var windowMu sync.Mutex
	windowEnd := map[ethaddr.Address]time.Time{}
	retired := map[ethaddr.Address]bool{}
	lateToRetired := func(tr watcher.Transfer) bool {
		windowMu.Lock()
		end, ok := windowEnd[tr.To]
		gone := retired[tr.To]
		windowMu.Unlock()
		if !ok || !gone {
			return false
		}
		blk, found := h.chain.BlockByNumber(tr.BlockNumber)
		return found && int64(blk.Time) >= end.Unix()
	}
	h.exempt = func(tr watcher.Transfer) bool {
		return ((soleSource || loglessHider) && isHidden(tr.TxHash)) || lateToRetired(tr)
	}

	// In some histories of several endpoints one of them calls a block
	// finalized one block below its tip — more than the chain has
	// finalised (the others: a reorganisation's depth and more). What is
	// final for the consumer (Head.Final) never goes by its word. It is the
	// malicious endpoint, where the history has one: one faulty endpoint
	// is what the guarantees are made against. Like the malicious endpoint,
	// it is not part of best-effort histories: best effort believes the
	// endpoint left alone, finality included.
	eager := -1
	if nodes >= 2 && mode != watcher.VerifyBestEffort && rand.New(rand.NewPCG(seed, 0xea9e)).IntN(4) == 0 {
		eager = rand.New(rand.NewPCG(seed, 0xea9f)).IntN(nodes)
		if liar >= 0 {
			eager = liar
		}
		trace("node %d reports the block below its tip as finalized", eager)
	}
	const honestFinality = maxReorgDepth + 3
	var highestTip uint64
	// finStopped is set while no endpoint reports a finalized block (the
	// chain finalises nothing new then: finalised, the highest block the
	// honest endpoints have called finalized, stays where it is);
	// finStopIn counts the steps until they report it again.
	finStopped := false
	finStopIn := 0
	var finalised, lastFinal uint64
	repairsSeen := 0
	rng5 := rand.New(rand.NewPCG(seed, 0xf1a3))
	// restartWithoutCheckpoint: the process starts again without its
	// checkpoint, as the bridge starts it — the consumer's final block as
	// the floor, the transfers the consumer still lists as pending (what it
	// holds above that block), and the first block to scan the one after.
	restartWithoutCheckpoint := func() {
		trace("  RESTART without the checkpoint while no endpoint reports finality (floor %d)", h.cfg.FinalizedFloor)
		h.store.restore(nil)
		var pend []watcher.Transfer
		for _, tr := range h.sink.liveKeys() {
			if tr.BlockNumber > h.cfg.FinalizedFloor {
				pend = append(pend, tr)
			}
		}
		h.watch.setPending(pend...)
		trace("    pending listed for the new watcher: %d", len(pend))
		h.restart()
	}

	// Standing provider limits per node (these are not glitches: they stay).
	limits := make([]func(b *evmtest.Behavior), nodes)
	batches := make([]bool, nodes) // whether the node accepts JSON-RPC batches
	for i := range limits {
		maxRange := uint64(0)
		if rng.IntN(2) == 0 {
			maxRange = uint64(2 + rng.IntN(40))
		}
		maxTopics := 0
		if rng.IntN(3) == 0 {
			maxTopics = 1 + rng.IntN(30)
		}
		noBatch := rng.IntN(4) == 0
		batches[i] = !noBatch
		trace("node %d: maxLogRange=%d maxTopics=%d noBatch=%v", i, maxRange, maxTopics, noBatch)
		noLogs := i == noLogsEver || (noLogsForGood && i == noLogsAtFirst)
		hider := (soleSource && i == 0) || (loglessHider && i == 1-noLogsAtFirst)
		finality := uint64(honestFinality)
		if i == eager {
			finality = 1
		}
		limits[i] = func(b *evmtest.Behavior) {
			*b = evmtest.Behavior{
				MaxLogRange: maxRange, MaxTopics: maxTopics, NoBatch: noBatch,
				// Finality far enough behind the head that the random
				// reorgs below never contradict it.
				FinalityDepth: finality,
				NoFinalized:   finStopped,
			}
			if noLogs {
				b.LogsError = &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"}
			}
			if hider {
				b.DropLogs = func(l evm.Log) bool { return hides(l.TxHash) }
			}
		}
		h.nodes[i].Set(limits[i])
	}
	if noLogsAtFirst >= 0 {
		trace("node %d has no log service at first", noLogsAtFirst)
		h.nodes[noLogsAtFirst].Set(func(b *evmtest.Behavior) {
			b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"}
		})
	}
	glitched := make([]bool, nodes)
	heal := func() {
		for i, n := range h.nodes {
			n.Set(limits[i])
			glitched[i] = false
		}
		h.sink.set(func(s *sink) { s.failObserved, s.failReverted, s.failHead = 0, 0, 0 })
		h.store.mu.Lock()
		h.store.failSave, h.store.failLoad = 0, 0
		h.store.mu.Unlock()
		h.watch.mu.Lock()
		h.watch.fail = 0
		h.watch.mu.Unlock()
		h.passCooldowns()
	}
	quiet := func() bool {
		for _, g := range glitched {
			if g {
				return false
			}
		}
		return true
	}
	// down are the endpoints a glitch has taken down (until heal).
	down := map[*evmtest.Node]bool{}
	// liesNow are the kinds of lie the malicious endpoint tells at the
	// moment (until heal).
	liesNow := map[int]bool{}
	healAll := heal
	heal = func() {
		healAll()
		clear(down)
		clear(liesNow)
		if loglessOn {
			h.nodes[noLogsAtFirst].Set(func(b *evmtest.Behavior) {
				b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"}
			})
		}
	}

	// The list of endpoints changes: reordered, an endpoint added, one
	// removed. order[k] is the index the endpoint at position k had.
	rearrange := func(order []int) {
		pos := func(old int) int {
			if old < 0 {
				return -1
			}
			return slices.Index(order, old)
		}
		var ns []*evmtest.Node
		var us []string
		var ls []func(*evmtest.Behavior)
		var bs, gs []bool
		// A glitch is the node's, whatever URL it was set through: an
		// alias that leaves the list leaves the node glitched.
		nodeGlitched := map[*evmtest.Node]bool{}
		for i, n := range h.nodes {
			nodeGlitched[n] = nodeGlitched[n] || glitched[i]
		}
		for _, o := range order {
			ns, us, ls = append(ns, h.nodes[o]), append(us, h.urls[o]), append(ls, limits[o])
			bs, gs = append(bs, batches[o]), append(gs, nodeGlitched[h.nodes[o]])
		}
		liar, noLogsAtFirst, noLogsEver, eager = pos(liar), pos(noLogsAtFirst), pos(noLogsEver), pos(eager)
		h.nodes, h.urls, limits, batches, glitched = ns, us, ls, bs, gs
		nodes = len(ns)
	}
	// addNode adds an honest endpoint at position `at`.
	addNode := func(at int) {
		n := evmtest.NewNode(h.chain)
		u := n.Serve(t)
		lim := func(b *evmtest.Behavior) {
			*b = evmtest.Behavior{FinalityDepth: maxReorgDepth + 3, NoFinalized: finStopped}
		}
		n.Set(lim)
		h.nodes, h.urls, limits = append(h.nodes, n), append(h.urls, u), append(limits, lim)
		batches, glitched = append(batches, true), append(glitched, false)
		order := make([]int, 0, len(h.nodes))
		for i := range len(h.nodes) - 1 {
			order = append(order, i)
		}
		at = min(at, len(order))
		order = slices.Insert(order, at, len(h.nodes)-1)
		rearrange(order)
	}
	// special reports whether the endpoint at i plays a part of its own in
	// the history (and is therefore never removed).
	var hider *evmtest.Node
	if soleSource {
		hider = h.nodes[0]
	}
	if loglessHider {
		hider = h.nodes[1-noLogsAtFirst]
	}
	special := func(i int) bool {
		return i == liar || i == noLogsAtFirst || i == noLogsEver || i == eager || h.nodes[i] == hider
	}

	// Best effort believes an endpoint alone only after every other one has
	// been silent beside it for SoloPatience ticks in a row — that
	// endpoint's ticks, whichever endpoint it is and whatever happened
	// before (invariant: an endpoint that did not answer cannot have been
	// the one left all along). Checked on every tick, by Run or by hand.
	quietFor := map[*evmtest.Node]int{}
	h.onTickEnd = func(ok bool) {
		for _, es := range h.client.Status() {
			if es.Logs.NoService {
				logsException = true
			}
		}
		for _, n := range h.nodes {
			silent := true
			for _, o := range h.nodes {
				if o != n && o.Answered() > 0 {
					silent = false
				}
			}
			if silent {
				quietFor[n]++
			} else {
				quietFor[n] = 0
			}
		}
		st := h.w.Status()
		if !ok || !st.Solo {
			return
		}
		for i, n := range h.nodes {
			if evm.RedactURL(h.urls[i]) == st.Endpoint && quietFor[n] < watcher.SoloPatience {
				t.Errorf("BEST EFFORT: %s is believed alone after %d tick(s) in which every other endpoint was silent; it takes %d of its own",
					st.Endpoint, quietFor[n], watcher.SoloPatience)
			}
		}
	}

	// watched are the addresses in the watch set, watchedNo their numbers
	// (addr); created counts every address of the history.
	var watched []ethaddr.Address
	var watchedNo []int
	created := 0
	everWatched := make(map[ethaddr.Address]bool)
	nextAddress := func() ethaddr.Address {
		no := int(seed)*100_000 + created
		created++
		a := addr(no)
		watched, watchedNo = append(watched, a), append(watchedNo, no)
		everWatched[a] = true
		return a
	}
	newAddress := func() ethaddr.Address {
		a := nextAddress()
		// Always before anyone can pay to it, and with the block the core
		// gives an address as the start of its scan: the processed head's
		// final block, or the head if that is lower (core's startBlock) —
		// nothing is paid to it before, and no reorganisation the watcher
		// accepts moves a payment below it. (None before the first head:
		// nothing is known then.)
		if hd := h.sink.lastHead(); hd.Number != 0 {
			h.watch.addSince(a, max(min(hd.Final, hd.Number), 1))
		} else {
			h.watch.add(a)
		}
		return a
	}
	isWatched := func(a ethaddr.Address) bool { return everWatched[a] }

	// retireDue retires the addresses whose watch window the processed head
	// is past — by its block time and by Head.Checked, less a margin for a
	// reorganisation below it — as the consumer does.
	const retireMargin = (maxReorgDepth + 1) * time.Second
	retireDue := func() {
		windowMu.Lock()
		defer windowMu.Unlock()
		if len(windowEnd) == 0 {
			return
		}
		hd := h.sink.lastHead()
		if hd.Number == 0 {
			return
		}
		passed := hd.Time
		if c, _ := headChecked(hd); !c.IsZero() && c.Before(passed) {
			passed = c
		}
		for a, end := range windowEnd {
			if !retired[a] && !passed.Add(-retireMargin).Before(end) {
				retired[a] = true
				h.watch.remove(a)
				trace("  RETIRED %s: window ended at %d, head %d at %d (checked %s)", a, end.Unix(), hd.Number, hd.Time.Unix(), passed)
			}
		}
	}
	// Head.Checked, on every tick: while blocks wait for a second log
	// endpoint's scan and more than one endpoint is configured, the earlier
	// of it and the head's time is no later than the first of them;
	// otherwise there is no Checked.
	// What the endpoints themselves have scanned, from their own log
	// queries — not from the watcher's record: per node, the blocks a query
	// that was answered covered, forgotten from the first block of a
	// reorganisation on (the blocks there are others now).
	coverage := map[*evmtest.Node]map[uint64]bool{}
	queriesSeen := map[*evmtest.Node]int{}
	updateCoverage := func() {
		for _, n := range h.nodes {
			qs := n.LogQueries()
			if queriesSeen[n] > len(qs) {
				queriesSeen[n] = 0
			}
			if coverage[n] == nil {
				coverage[n] = map[uint64]bool{}
			}
			for _, q := range qs[queriesSeen[n]:] {
				if q.Refused || q.Recipients == 0 || q.To < q.From || q.To-q.From > 10_000 {
					continue
				}
				for b := q.From; b <= q.To; b++ {
					coverage[n][b] = true
				}
			}
			queriesSeen[n] = len(qs)
		}
	}
	forgetCoverage := func(from uint64) {
		updateCoverage()
		for _, c := range coverage {
			for b := range c {
				if b >= from {
					delete(c, b)
				}
			}
		}
	}
	independentChecked := loglessHider || noLogsForGood
	var checkFrom uint64 // the first block of the truth, once the warm-up is over

	// (Only a head this process has reported: what the sink has from an
	// earlier one says what that one's configuration was.)
	tickEnd := h.onTickEnd
	var process *watchdog
	headsBefore := 0
	headsLastTick := 0 // heads reported up to the end of the previous tick
	heads := func() (n int) {
		h.sink.mu.Lock()
		defer h.sink.mu.Unlock()
		for _, e := range h.sink.events {
			if e.kind == "head" {
				n++
			}
		}
		return n
	}
	h.onTickEnd = func(ok bool) {
		tickEnd(ok)
		retireDue()
		if process != h.w {
			process, headsBefore = h.w, heads()
		}
		hd, st := h.sink.lastHead(), h.w.Status()
		// FINALITY: what the consumer is told is final is final for the
		// chain — at most the block the honest endpoints call finalized —
		// whatever one endpoint says; a single configured endpoint is
		// trusted with it, as with everything.
		// (By the highest tip so far: a fork shortens the chain for a
		// while, and the blocks it keeps that were finalized stay so —
		// no fork here is that deep.)
		highestTip = max(highestTip, h.chain.Head().Number)
		if !finStopped && highestTip > honestFinality {
			finalised = max(finalised, highestTip-honestFinality)
		}
		if !noFinality && len(h.urls) > 1 && hd.Number != 0 {
			if hd.Final > finalised {
				t.Errorf("FINALITY: head %d is final at block %d (finalized %d), above block %d, which the honest endpoints have called finalized (no endpoint reports finality now: %v; dispute %q)",
					hd.Number, hd.Final, hd.Finalized, finalised, finStopped, st.FinalityDispute)
			}
			// The final block never goes back — with one exception: the
			// watcher gave up blocks of its checkpoint that both
			// endpoints' chains no longer reached, and a finalized block
			// among them (IncidentCheckpointAhead). It reports no head
			// until a finalized block is attested again (or the
			// consumer's, if the chains reach that), and that one can be
			// lower: the first head after a repair may have a lower final
			// block, and from there on it never goes back again. The
			// exception stays even when the given-up block survives on the
			// real chain (two URLs of one stale provider): at the repair
			// neither endpoint in use shows a block at that height, so
			// the watcher cannot tell; holding it final anyway would be
			// the blind trust the repair is there to end. It is a known
			// limit (README, "A repaired checkpoint can lower the
			// watcher's own final block"); the consumer never lowers its
			// own — the core model checks that.)
			incMu.Lock()
			r := repairs
			incMu.Unlock()
			switch {
			case r != repairsSeen:
				repairsSeen = r
				lastFinal = hd.Final
			case hd.Final < lastFinal && hd.Number >= lastFinal:
				t.Errorf("FINALITY: the final block went back from %d to %d (finalized %d), the head at %d; solo %v, dispute %q", lastFinal, hd.Final, hd.Finalized, hd.Number, st.Solo, st.FinalityDispute)
			}
			lastFinal = max(lastFinal, hd.Final)
		}
		// The consumer's final block, given to every watcher it starts
		// (the bridge has it from the core, which never lowers it); and
		// what it still lists as pending for a watcher that lost its
		// checkpoint, as long as it is not final and still on record.
		h.cfg.FinalizedFloor = max(h.cfg.FinalizedFloor, hd.Final)
		// And, like the bridge on every start, where to begin if there
		// is no checkpoint (the watcher uses it only then): the block
		// after the consumer's final one.
		if hd.Number != 0 {
			start := h.cfg.FinalizedFloor + 1
			h.cfg.StartBlock = &start
		}
		live := h.sink.liveKeys()
		h.watch.mu.Lock()
		kept := h.watch.pending[:0:0]
		for _, p := range h.watch.pending {
			if _, ok := live[keyOf(p)]; ok && p.BlockNumber > hd.Final {
				kept = append(kept, p)
			}
		}
		if len(kept) != len(h.watch.pending) {
			trace("    the consumer lists %d pending transfer(s), was %d (final %d)", len(kept), len(h.watch.pending), hd.Final)
		}
		h.watch.pending = kept
		h.watch.mu.Unlock()
		c, reported := headChecked(hd)
		now := heads()
		reportedNow := now != headsLastTick
		headsLastTick = now
		if !ok || !reported || hd.Number == 0 || hd.Number != st.Cursor || now == headsBefore {
			return
		}
		switch {
		case len(h.urls) < 2 || st.UncheckedFrom == 0:
			// (Of a head reported in this tick: the record can be made up
			// for in a tick that reports no head — the endpoints cannot
			// show the cursor's block in it — and the last head then says
			// more is owed than is, until the next head: the safe way.)
			if !c.IsZero() && reportedNow {
				t.Errorf("CHECKED: head %d says blocks are owed a second scan (%s), with %d endpoint(s) and unchecked blocks %d..%d", hd.Number, c, len(h.urls), st.UncheckedFrom, st.UncheckedTo)
			}
		default:
			// What the consumer goes by is the earlier of the head's time
			// and Checked: blocks above the head (a rewind left them on
			// record) are later than the head anyway.
			passed := hd.Time
			if !c.IsZero() && c.Before(passed) {
				passed = c
			}
			blk, found := h.chain.BlockByNumber(st.UncheckedFrom)
			if found && passed.Unix() > int64(blk.Time) {
				t.Errorf("CHECKED: head %d (stamped %d) says checked %s, and block %d (stamped %d) is still to be scanned by a second log endpoint", hd.Number, hd.Time.Unix(), c, st.UncheckedFrom, blk.Time)
			}
		}
		if !independentChecked || checkFrom == 0 {
			return
		}
		// And by the endpoints' own account: every block at or below the
		// head stamped before what the head says (the earlier of its time
		// and Checked) that anybody scanned has been scanned by two nodes.
		updateCoverage()
		passed := hd.Time
		if !c.IsZero() && c.Before(passed) {
			passed = c
		}
		// (Not below every watched address's start block: nobody watched
		// can have been paid there, and the watcher owes those blocks no
		// second scan — Head.Checked.)
		from := checkFrom
		h.watch.mu.Lock()
		lowest, known := uint64(0), len(h.watch.set) > 0
		for a := range h.watch.set {
			sb := h.watch.since[a]
			if sb == 0 {
				known = false
				break
			}
			if lowest == 0 || sb < lowest {
				lowest = sb
			}
		}
		h.watch.mu.Unlock()
		if known {
			from = max(from, lowest+1)
		}
		for b := from; b <= hd.Number; b++ {
			blk, found := h.chain.BlockByNumber(b)
			if !found || int64(blk.Time) >= passed.Unix() {
				continue
			}
			by := 0
			for _, cov := range coverage {
				if cov[b] {
					by++
				}
			}
			if by == 1 {
				t.Errorf("CHECKED: head %d says every block stamped before %s has been scanned by two log endpoints; block %d (stamped %d) has been scanned by one", hd.Number, passed, b, blk.Time)
				return
			}
		}
	}
	// forget takes an address out of those payments go to.
	forget := func(a ethaddr.Address) {
		if i := slices.Index(watched, a); i >= 0 {
			watched, watchedNo = slices.Delete(watched, i, i+1), slices.Delete(watchedNo, i, i+1)
		}
	}

	if debugLogs {
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("watcher log:\n%s", tail(h.logs.String(), 600000))
			}
		})
	}

	// Invariant 1, at the moment of the report.
	h.sink.set(func(s *sink) {
		s.onObserved = func(tr watcher.Transfer) {
			if blk, ok := h.chain.BlockByNumber(tr.BlockNumber); !ok || blk.Hash != tr.BlockHash {
				t.Errorf("INVARIANT 1: transfer %x reported in block %d (%x), which is not on the chain at that moment",
					tr.TxHash[:4], tr.BlockNumber, tr.BlockHash[:4])
			}
		}
	})
	// In some histories one endpoint is far behind when the watcher
	// starts for the first time. With two endpoints it must not start at
	// all while that lasts — the lower of two heads is no start position —
	// and with three it starts where the other two are; either way the
	// position it starts from is never far below the chain.
	if nodes >= 2 && rng.IntN(5) == 0 {
		far := rng.IntN(nodes)
		lag := uint64(watcher.MaxStartSpread + 1 + rng.IntN(history-watcher.MaxStartSpread-5))
		trace("node %d is %d blocks behind at the first start", far, lag)
		h.nodes[far].Set(func(b *evmtest.Behavior) { b.Lag = lag })
		tip := h.chain.Head().Number
		for i := 0; i < 3; i++ {
			_ = h.w.Tick(context.Background())
		}
		switch st := h.w.Status(); {
		case nodes == 2 && st.Cursor != 0:
			t.Errorf("START: with two endpoints %d blocks apart the watcher started, at block %d (tip %d)", lag, st.Cursor, tip)
		case nodes > 2 && st.Cursor != tip-testSettle:
			t.Errorf("START: with one of three endpoints %d blocks behind the watcher is at block %d after three ticks, tip %d", lag, st.Cursor, tip)
		}
		h.nodes[far].Set(limits[far])
		if far == noLogsAtFirst {
			h.nodes[far].Set(func(b *evmtest.Behavior) { b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"} })
		}
		h.passCooldowns()
	}

	// An address is watched from the start, so that the endpoints are asked
	// for logs during the warm-up: an endpoint that has never answered a log
	// query and refuses one cannot be told from one without a log service.
	newAddress()

	// Warm-up: some history and a few ticks, so that the watcher has
	// anchors well below anything the random reorgs can reach.
	h.chain.Mine(20)
	for i := 0; i < 8; i++ {
		h.chain.Mine(3)
		if noLogsEver >= 0 {
			// A tick that starts from the endpoint without logs fails,
			// and the next one starts from another.
			_ = h.w.Tick(context.Background())
			h.clock.Advance(time.Duration(20+rng2.IntN(60)) * time.Second)
			continue
		}
		if noLogsAtFirst < 0 {
			h.tick()
			continue
		}
		// The endpoint without a log service: until it has refused three
		// times the head waits for it (and a tick that starts from it
		// fails), and nothing is settled on one endpoint's logs.
		_ = h.w.Tick(context.Background())
		if st, logs := h.w.Status(), h.client.Status()[noLogsAtFirst].Logs; (st.LogsUnchecked || st.UncheckedFrom != 0) && !logs.NoService {
			t.Errorf("INVARIANT 3: blocks settled on one endpoint's logs (unchecked from %d) after %d refusal(s) of the other", st.UncheckedFrom, logs.Refusals)
		}
		h.clock.Advance(time.Duration(20+rng.IntN(60)) * time.Second)
	}
	if noLogsAtFirst >= 0 || noLogsEver >= 0 {
		// Whatever it took: from here on the history is like any other.
		for i := 0; i < 4*testSettle && h.w.Status().Cursor != h.chain.Head().Number-testSettle; i++ {
			h.chain.Mine(1)
			_ = h.w.Tick(context.Background())
			h.clock.Advance(time.Minute)
		}
		if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle {
			t.Fatalf("with an endpoint that has no log service the watcher did not get going: %+v", st)
		}
	}
	// Nothing is paid during the warm-up, and a reorg can move later
	// payments at most maxReorgDepth blocks below the highest head — never
	// below the block the watcher started from. So the truth is "every
	// payment on the canonical chain", full stop.
	first := uint64(startBlock)
	checkFrom = first
	maxHead := h.chain.Head().Number
	ticksOK, checks, heldStreak := 0, 0, 0
	h.sink.set(func(s *sink) { s.lenientReverts = true })

	// idleStretch: every watch window ends, and the history goes on under
	// Watcher.Run in simulated time (a block a second) — see the test's
	// documentation.
	stretches := 0
	if soleSource || loglessHider {
		// Addresses that leave the watch set are not looked for again — not
		// even for the blocks of a lone log source (documented) — so these
		// histories keep every address.
		stretches = 2
	}
	if noLogsForGood {
		// Blocks wait for a second log endpoint's scan for good, and a
		// watcher with addresses to scan them for does not rest (it is
		// ready for the moment the other endpoint serves logs): these
		// histories have no stretch at rest.
		stretches = 2
	}
	idleStretch := func() {
		stretches++
		// The ticks of the stretch are not the model's: what it has counted
		// of a held head starts again afterwards.
		defer func() { heldStreak = 0 }()
		ctx := context.Background()
		mineUp := func(extra int) {
			h.chain.Mine(int(maxHead-min(maxHead, h.chain.Head().Number)) + extra)
			maxHead = max(maxHead, h.chain.Head().Number)
		}
		// A watcher that cannot report a head (no endpoint reports a
		// finalized block) is not at rest: finality comes back first.
		// (What the consumer lists as pending stays listed until it is
		// final or reverted, as a consumer's does.)
		if finStopped {
			finStopped, finStopIn = false, 0
			for _, n := range h.nodes {
				n.Set(func(b *evmtest.Behavior) { b.NoFinalized = false })
			}
			h.passCooldowns()
			trace("  FINALITY: the endpoints report finalized blocks again (before an idle stretch)")
		}
		// The truth about the addresses that are about to leave the watch
		// set has to be final first: everything paid so far is mined, deeper
		// than any reorganisation of this model reaches, and reported.
		heal()
		mineUp(maxReorgDepth + 1)
		inStep := false
		for i := 0; i < 60 && !inStep; i++ {
			err := h.w.Tick(ctx)
			st := h.w.Status()
			inStep = err == nil && st.Unverified == 0 && st.Hold == "" && st.Tip.Number == h.chain.Head().Number && st.Cursor == st.Tip.Number-testSettle
			h.passCooldowns()
			if noLogsAtFirst >= 0 || (logsException && noLogsEver < 0) {
				h.passLogPause()
			}
		}
		if !inStep {
			t.Fatalf("IDLE STRETCH %d: the watcher did not get in step with healthy endpoints: %+v\n%s", stretches, h.w.Status(), tail(h.logs.String(), 3000))
		}
		h.assertModelFor(first, isWatched)
		// Every other stretch is a quiet one: a few addresses stay in the
		// watch set, closed. All others leave it.
		var kept []ethaddr.Address
		var keptNo []int
		if rng.IntN(2) == 0 {
			for _, i := range rng.Perm(len(watched))[:min(len(watched), 1+rng.IntN(4))] {
				kept, keptNo = append(kept, watched[i]), append(keptNo, watchedNo[i])
			}
		}
		for _, a := range watched {
			if !slices.Contains(kept, a) {
				h.watch.remove(a)
			}
		}
		h.watch.close(kept...)
		trace("IDLE STRETCH %d: %d addresses leave the watch set at head %d, %d stay in it as closed ones", stretches, len(watched)-len(kept), h.chain.Head().Number, len(kept))
		watched, watchedNo = kept, keptNo
		quietStretch := len(kept) > 0

		// Every pause is the one the status of the tick before it calls for.
		sim.onPause = func(d time.Duration, st watcher.Status) {
			trace("  PAUSE %s at %s: idle=%v work=%q error=%q cursor=%d", d, sim.now, st.Idle, st.Work, st.LastError, st.Cursor)
			switch {
			case st.LastError != "":
				if d < time.Second || d > watcher.DefaultMaxBackoff {
					t.Errorf("PAUSE: %s after a failed tick, want a backoff of 1 s to %s", d, watcher.DefaultMaxBackoff)
				}
			case st.Idle && d != simIdle:
				t.Errorf("PAUSE: %s after a tick that left the watcher idle, want the idle interval (%s)", d, simIdle)
			case !st.Idle && d != simPoll:
				t.Errorf("PAUSE: %s after a tick that left the watcher at work (%s), want the poll interval (%s)", d, st.Work, simPoll)
			}
		}
		// comeToRest runs until an idle pause begins. With nothing to watch
		// and healthy endpoints it has to: a watcher that never idles is
		// safe, and costs ten times what it should without anybody noticing.
		comeToRest := func(why string) {
			resting := false
			keep := sim.onPause
			sim.onPause = func(d time.Duration, st watcher.Status) {
				keep(d, st)
				if st.Idle && st.LastError == "" {
					resting = true
					sim.stop()
				}
			}
			sim.run(4 * time.Minute)
			sim.onPause = keep
			maxHead = max(maxHead, h.chain.Head().Number)
			if !resting {
				t.Fatalf("IDLE STRETCH %d: %s the watcher did not come to rest in four minutes: %+v\n%s", stretches, why, h.w.Status(), tail(h.logs.String(), 3000))
			}
			if st := h.w.Status(); st.Quiet != quietStretch || st.WatchedAddresses != len(kept) {
				t.Errorf("IDLE STRETCH %d: at rest with %d closed address(es) in the watch set: quiet=%v, %d watched", stretches, len(kept), st.Quiet, st.WatchedAddresses)
			}
		}
		if quietStretch {
			comeToRest("with closed addresses only and healthy endpoints")
		} else {
			comeToRest("with an empty watch set and healthy endpoints")
		}

		// One to three idle pauses, with whatever happens meanwhile.
		chaos := time.Duration(1+rng.IntN(3)) * simIdle
		for at := time.Duration(1+rng.IntN(20)) * time.Second; at < chaos; at += time.Duration(5+rng.IntN(40)) * time.Second {
			switch rng.IntN(4) {
			case 0: // a reorganisation, possibly below the cursor's block
				sim.in(at, func() {
					head := h.chain.Head().Number
					maxHead = max(maxHead, head)
					if maxHead < maxReorgDepth || head <= maxHead-maxReorgDepth {
						return
					}
					depth := 1 + rng.IntN(int(min(uint64(maxReorgDepth), head-(maxHead-maxReorgDepth))))
					dropped := h.chain.Reorg(depth)
					for _, tx := range dropped {
						if rng.IntN(4) > 0 { // most late transfers come back, some do not
							h.chain.Requeue(tx)
						}
					}
					trace("  reorg during the idle stretch: %d blocks dropped (%d transactions), head now %d (cursor %d)", depth, len(dropped), h.chain.Head().Number, h.w.Status().Cursor)
				})
			case 2, 3: // money arrives at a closed address; nobody tells the watcher
				if !quietStretch {
					break
				}
				to, amount := kept[rng.IntN(len(kept))], int64(1+rng.IntN(500))
				sim.in(at, func() {
					tx := h.pay(to, amount)
					trace("  late transfer %s to the closed address %s at %s, head %d (cursor %d)", tx.String()[:10], to, sim.now, h.chain.Head().Number, h.w.Status().Cursor)
				})
			case 1: // an endpoint misbehaves for a while
				i := rng.IntN(nodes)
				kind, lag, fails := rng.IntN(3), uint64(1+rng.IntN(12)), 1+rng.IntN(4)
				sim.in(at, func() {
					trace("  GLITCH during the idle stretch: node %d kind %d (0 down, 1 lag %d, 2 fails %d requests)", i, kind, lag, fails)
					glitched[i] = true
					h.nodes[i].Set(func(b *evmtest.Behavior) {
						switch kind {
						case 0:
							b.Down = true
						case 1:
							b.Lag = lag
						case 2:
							b.FailRequests = fails
						}
					})
				})
				if rng.IntN(2) == 0 {
					sim.in(at+time.Duration(1+rng.IntN(60))*time.Second, func() {
						trace("  node %d recovers", i)
						h.nodes[i].Set(limits[i])
					})
				}
			}
		}
		sim.run(chaos)
		maxHead = max(maxHead, h.chain.Head().Number)

		// Things get better, and the chain grows past its former maximum.
		// Then, in three stretches out of four, a payment is created in the
		// middle of an idle pause.
		heal()
		mineUp(1)
		kind := rng.IntN(4)
		if kind == 0 {
			trace("IDLE STRETCH %d ends with nothing watched but %d closed address(es)", stretches, len(kept))
			sim.onPause = nil
			return
		}
		comeToRest("after the endpoints recovered,")
		// Whatever arrived late, was reorganised away or came back while the
		// watcher sat through its pauses: at rest, in step with a chain that
		// has not moved since its last tick, the sink holds what the chain
		// holds.
		h.assertModelFor(first, isWatched)
		woken := kind != 3
		offset := time.Duration(1+rng.IntN(int(simIdle/time.Second)-2)) * time.Second
		var tx evm.Hash
		var createdAt time.Duration
		sim.in(offset, func() {
			// As the core does it: the processed head's final block (or
			// the head, if lower) is the address's start block, the address
			// is in the watch set before
			// anybody can pay to it, and then the watcher is told — or, in
			// one stretch out of four, is not.
			hd := h.sink.lastHead()
			since := max(min(hd.Final, hd.Number), 1)
			a := nextAddress()
			h.watch.addSince(a, since)
			tx = h.pay(a, int64(1+rng.IntN(500)))
			h.chain.Mine(1)
			maxHead = max(maxHead, h.chain.Head().Number)
			createdAt = sim.now
			trace("  payment %s to a new address at %s into an idle pause: start block %d, head %d, woken=%v", tx.String()[:10], offset, since, h.chain.Head().Number, woken)
			if woken {
				h.w.Wake()
			}
		})
		sim.reset()
		sim.run(simIdle + 3*simPoll)
		maxHead = max(maxHead, h.chain.Head().Number)
		sim.onPause = nil

		var pause simPause // the idle pause the payment was created in
		for _, p := range sim.pauses {
			if p.idle && p.at <= createdAt && createdAt <= p.at+p.took {
				pause = p
			}
		}
		tk, ticked := sim.tickAt(createdAt)
		_, reported := findTransfer(h, tx)
		switch {
		case !ticked || !reported:
			t.Errorf("WAKE-UP: the payment created at %s (woken=%v) was not reported: next tick %+v", createdAt, woken, tk)
		case tk.st.Tracked == 0 || tk.st.WatchedAddresses != len(kept)+1:
			t.Errorf("WAKE-UP: the first tick after the payment (at %s, created at %s) did not report it: %+v", tk.at, createdAt, tk.st)
		case woken && (tk.at != createdAt || !pause.cut):
			t.Errorf("WAKE-UP: woken at %s, the next tick ran at %s (pause %+v)", createdAt, tk.at, pause)
		case !woken && (pause.cut || tk.at != pause.at+simIdle):
			t.Errorf("WAKE-UP: nobody woke the watcher, and the idle pause %+v did not run its course (next tick at %s)", pause, tk.at)
		}
	}

	for step := 0; step < 260; step++ {
		if soleSource && step == aloneUntil {
			// What it hides was mined, and settled, while it was alone:
			// the blocks are recorded as unchecked, by it.
			heal()
			h.chain.Mine(1)
			for range testSettle + 2 {
				h.chain.Mine(1)
				_ = h.w.Tick(context.Background())
			}
			at := rng2.IntN(2)
			if relabelHider {
				h.urls[0] = h.nodes[0].Serve(t)
				trace("  the lone log source stays under another URL: %s", h.urls[0])
			}
			if returningPair {
				st := h.w.Status()
				owed := st.UncheckedFrom
				lone, loneURL, loneLimit := h.nodes[0], h.urls[0], limits[0]
				b, c := evmtest.NewNode(h.chain), evmtest.NewNode(h.chain)
				lim := func(be *evmtest.Behavior) {
					*be = evmtest.Behavior{FinalityDepth: maxReorgDepth + 3, NoFinalized: finStopped}
				}
				b.Set(lim)
				c.Set(lim)
				h.nodes, h.urls = []*evmtest.Node{b, c}, []string{b.Serve(t), c.Serve(t)}
				limits, batches, glitched = []func(*evmtest.Behavior){lim, lim}, []bool{true, true}, []bool{false, false}
				nodes = 2
				h.restartWithNewClient()
				trace("  two new endpoints replace the lone log source (unchecked %d..%d)", st.UncheckedFrom, st.UncheckedTo)
				for i := 0; i < 10 && owed != 0 && !coveredBy(b, owed) && !coveredBy(c, owed); i++ {
					h.chain.Mine(1)
					_ = h.w.Tick(context.Background())
				}
				witness := 0
				if coveredBy(c, owed) && !coveredBy(b, owed) {
					witness = 1
				}
				h.nodes, h.urls = []*evmtest.Node{lone, h.nodes[witness]}, []string{loneURL, h.urls[witness]}
				limits = []func(*evmtest.Behavior){loneLimit, limits[witness]}
				trace("  the lone log source comes back with the witness %s", h.urls[1])
			} else {
				trace("  an honest endpoint is added, at position %d of the list (unchecked %d..%d)", at, h.w.Status().UncheckedFrom, h.w.Status().UncheckedTo)
				addNode(at)
			}
			h.restartWithNewClient()
			heldStreak = 0 // a new process: its count of held ticks starts again
		}
		if loglessOn && step == loglessUntil {
			// What it hides was mined, and settled, on the other's logs
			// alone; then the endpoint gets a log service.
			heal()
			h.chain.Mine(1)
			for range testSettle + 2 {
				h.chain.Mine(1)
				_ = h.w.Tick(context.Background())
			}
			loglessOn = false
			trace("  the endpoint without a log service gets one (unchecked %d..%d)", h.w.Status().UncheckedFrom, h.w.Status().UncheckedTo)
			heal()
		}
		if mode == watcher.VerifyBestEffort && nodes >= 2 && len(down) > 0 && rng2.IntN(4) == 0 {
			// A hand-off: the endpoint that was down comes back as another
			// goes down — the endpoint left is now one that did not answer
			// until a moment ago.
			for i, n := range h.nodes {
				if !down[n] {
					continue
				}
				others := []int{}
				for j := range h.nodes {
					if j != i && !down[h.nodes[j]] {
						others = append(others, j)
					}
				}
				if len(others) == 0 {
					break
				}
				j := others[rng2.IntN(len(others))]
				trace("  HAND-OFF: node %d comes back, node %d goes down", i, j)
				n.Set(limits[i])
				delete(down, n)
				glitched[i] = false
				h.nodes[j].Set(func(b *evmtest.Behavior) { b.Down = true })
				down[h.nodes[j]], glitched[j] = true, true
				h.client.Refresh(context.Background())
				break
			}
		}
		// Finality stops on every endpoint for a while (from a stream of
		// its own), and the process may lose its checkpoint meanwhile and
		// start again from the consumer's state, as the bridge starts it.
		if finStopIn > 0 {
			if finStopIn--; finStopIn == 0 {
				finStopped = false
				for _, n := range h.nodes {
					n.Set(func(b *evmtest.Behavior) { b.NoFinalized = false })
				}
				h.passCooldowns()
				trace("  FINALITY: the endpoints report finalized blocks again")
			}
		} else if !noFinality && rng5.IntN(25) == 0 {
			finStopped, finStopIn = true, 5+rng5.IntN(40)
			for _, n := range h.nodes {
				n.Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
			}
			trace("  FINALITY: no endpoint reports a finalized block for %d steps", finStopIn)
		}
		// (Not where the model checks which blocks two log endpoints
		// scanned, nor where the only log source hides payments to be
		// found by a second one's scan of the blocks it settled alone: a
		// lost checkpoint takes that record with it, a documented limit.)
		if finStopped && !independentChecked && !soleSource && !relabelHider && rng5.IntN(8) == 0 {
			restartWithoutCheckpoint()
			heldStreak = 0 // a new process: its count of held ticks starts again
		}
		op := rng.IntN(100)
		if head := h.chain.Head(); debugLogs {
			trace("step %d op=%d: head %d (%s) cursor %d", step, op, head.Number, head.Hash.String()[:10], h.w.Status().Cursor)
		}
		switch {
		case op < 22: // blocks
			h.chain.Mine(1 + rng.IntN(4))

		case op < 40: // a payment to a new or an existing deposit address
			var to ethaddr.Address
			if len(watched) == 0 || rng.IntN(3) > 0 {
				to = newAddress()
			} else {
				to = watched[rng.IntN(len(watched))]
			}
			tx := h.pay(to, int64(1+rng.IntN(500)))
			trace("  pay %s to %s", tx.String()[:10], to)
			if soleSource && step < aloneUntil-1 && rng2.IntN(2) == 0 {
				trace("  the lone log source hides %s", tx.String()[:10])
				hiddenMu.Lock()
				hidden[tx] = true
				hiddenMu.Unlock()
			}
			if loglessOn && step < loglessUntil-1 && rng3.IntN(2) == 0 {
				// Settled on its logs alone, and its window ends soon.
				end := time.Unix(int64(h.chain.Head().Time)+2+int64(rng3.IntN(8)), 0)
				trace("  the only log source hides %s; the window of %s ends at %d", tx.String()[:10], to, end.Unix())
				hiddenMu.Lock()
				hidden[tx] = true
				hiddenMu.Unlock()
				windowMu.Lock()
				windowEnd[to] = end
				windowMu.Unlock()
				h.watch.close(to)
				forget(to)
			}
			if rng.IntN(2) == 0 {
				h.chain.Mine(1)
			}

		case op < 46: // noise
			if len(watched) > 0 {
				victim := watched[rng.IntN(len(watched))]
				switch rng.IntN(4) {
				case 0:
					h.chain.Transfer(fakeToken, payer, victim, units(1000))
				case 1:
					h.chain.Transfer(usdt, payer, victim, units(0))
				case 2:
					h.chain.Transfer(usdt, victim, victim, units(5))
				case 3:
					h.chain.Transfer(usdt, payer, addr(-1-step), units(5))
				}
			}

		case op < 54: // reorg
			head := h.chain.Head().Number
			if maxHead < maxReorgDepth || head <= maxHead-maxReorgDepth {
				break
			}
			depth := 1 + rng.IntN(int(min(uint64(maxReorgDepth), head-(maxHead-maxReorgDepth))))
			believed := h.sink.liveKeys()
			before := h.chain.Clone()
			dropped := h.chain.Reorg(depth)
			forgetCoverage(h.chain.Head().Number + 1)
			trace("  reorg: %d blocks dropped (%d transactions), head now %d", depth, len(dropped), h.chain.Head().Number)
			if liar >= 0 && rng.IntN(4) == 0 {
				trace("  LIE: node %d keeps the old headers for blocks <= %d", liar, h.w.Status().Cursor)
				// The malicious endpoint keeps the reorganisation quiet
				// where it matters: for the blocks the watcher has
				// settled on it goes on returning the old headers.
				settled := h.w.Status().Cursor
				glitched[liar] = true
				h.nodes[liar].Set(func(b *evmtest.Behavior) {
					b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
						if method != "eth_getBlockByNumber" {
							return nil, nil, false
						}
						_, n, numeric := blockParam(params)
						if blk, ok := before.BlockByNumber(n); numeric && ok && n <= settled {
							return headerJSON(blk, blk.Time), nil, true
						}
						return nil, nil, false
					}
				})
			} else if liar >= 0 && rng.IntN(2) == 0 {
				// The malicious endpoint keeps serving the logs
				// of the blocks that were just reorganised away, for the
				// transfers the consumer believes in.
				var stale []evm.Log
				for _, tr := range believed {
					if tr.BlockNumber > h.chain.Head().Number {
						stale = append(stale, staleLog(tr))
					}
				}
				if len(stale) > 0 {
					trace("  LIE: node %d keeps serving %d logs of the dropped blocks", liar, len(stale))
					glitched[liar] = true
					h.nodes[liar].Set(func(b *evmtest.Behavior) { b.ExtraLogs = append(b.ExtraLogs, stale...) })
				}
			}
			if rng.IntN(3) > 0 {
				h.chain.Mine(rng.IntN(2)) // sometimes re-mined later, sometimes at once
			}
			for _, tx := range dropped {
				if rng.IntN(4) > 0 { // most transactions come back, some do not
					h.chain.Requeue(tx)
				}
			}
			h.chain.Mine(rng.IntN(depth + 3)) // the new fork may be shorter for a while
			trace("  after the reorg: head %d (%s)", h.chain.Head().Number, h.chain.Head().Hash.String()[:10])

		case op >= 78 && op < 80 && stretches < 2:
			idleStretch()

		case op < 80: // tick
			if rng.IntN(8) == 0 {
				// The pool's own health check: with a liar claiming a
				// longer chain it now takes the honest endpoints to lag.
				h.client.Refresh(context.Background())
			}
			cursorBefore := h.w.Status().Cursor
			err := h.w.Tick(context.Background())
			if st := h.w.Status(); debugLogs {
				h.watch.mu.Lock()
				readPending, listed := h.watch.readPending, len(h.watch.pending)
				h.watch.mu.Unlock()
				trace("  TICK: err=%v cursor %d -> %d tip=%d primary=%s verifier=%s logVerifier=%s unverified=%d hold=%q sink head=%d pending read %d listed %d",
					err, cursorBefore, st.Cursor, st.Tip.Number, st.Endpoint, st.Verifier, st.LogVerifier, st.Unverified, st.Hold, h.sink.lastHead().Number, readPending, listed)
			}
			if err == nil {
				st := h.w.Status()
				// Invariant 3: with two or more endpoints, all of which
				// serve logs, nothing is settled on one endpoint's logs.
				// (In the histories with an endpoint that had no log
				// service at first, blocks were — and what the status
				// still lists is what that endpoint has not made up for.)
				// The documented exception: an endpoint that has never
				// answered a log query and refused one three times, over
				// three minutes, is held to have no log service — a new
				// endpoint that refuses from its first query, say. Only
				// while every other endpoint is held so may blocks be
				// settled on one endpoint's logs.
				pool := h.client.Status()
				for _, es := range pool {
					if es.Logs.NoService {
						logsException = true
					}
				}
				// (LogsUnchecked describes the blocks settled last: what
				// this tick settled, if it moved the head.)
				if nodes >= 2 && mode == watcher.VerifyAuto && !soleSource && st.LogsUnchecked && st.Cursor > cursorBefore {
					for _, es := range pool {
						if es.URL != st.Endpoint && !es.Logs.NoService {
							t.Errorf("INVARIANT 3: blocks settled on the logs of %s alone while %s, not held to lack a log service, is configured (%+v)", st.Endpoint, es.URL, es.Logs)
						}
					}
				}
				if nodes >= 2 && mode == watcher.VerifyAuto && noLogsAtFirst < 0 && !logsException && !soleSource && (st.LogsUnchecked || st.UncheckedFrom != 0) {
					t.Errorf("INVARIANT 3: blocks settled on one endpoint's logs (unchecked from %d)", st.UncheckedFrom)
				}
				// Invariant 4, for the time of a transfer's block: with
				// three endpoints, one whose header batches lack the block
				// or name another does not keep it from being dated.
				if nodes == 3 && liar >= 0 && len(liesNow) == 1 && liesNow[11] && strings.Contains(st.Hold, "cannot be dated") {
					others := true
					for i, g := range glitched {
						if g && i != liar {
							others = false
						}
					}
					if others {
						t.Errorf("INVARIANT 4: one of three endpoints keeps a transfer's block from being dated: %q", st.Hold)
					}
				}
				// Best effort: one endpoint's word is taken only while
				// every other endpoint is unreachable. An endpoint that
				// answered anything in this tick — a result, an error, a
				// rate limit — ends that, in this tick.
				if st.Solo {
					for i, n := range h.nodes {
						if evm.RedactURL(h.urls[i]) != st.Endpoint && n.Answered() > 0 {
							t.Errorf("BEST EFFORT: single-source operation in a tick in which endpoint %d answered %d request(s)", i, n.Answered())
						}
					}
				}
				// (Invariant 4, a held head carries its reason: the harness's
				// watchdog, on every tick — no two in a row hold the head in
				// silence while an endpoint shows a longer chain. One is
				// allowed: the watcher holds the head to what the endpoints
				// had shown when it settled, and what one shows later in the
				// tick — a block mined meanwhile, a liar's later answer — is
				// the next tick's.)
				// Invariant 4: a held head is alerted.
				if st.Hold != "" && st.Cursor <= cursorBefore {
					heldStreak++
				} else {
					heldStreak = 0
				}
				incMu.Lock()
				alerted := !lastHeld.IsZero() && h.clock.Now().Sub(lastHeld) <= time.Minute
				incMu.Unlock()
				if heldStreak > 3 && !alerted {
					t.Errorf("INVARIANT 4: the head has been held for %d ticks (%q) and no incident was raised in the last minute", heldStreak, st.Hold)
				}
				ticksOK++
				// While the canonical chain is shorter than it once was
				// (a reorg whose fork has not caught up yet), a transfer
				// in a block above the current head can be neither
				// confirmed gone nor found: the check waits, as the
				// watcher does.
				if quiet() && h.w.Status().Unverified == 0 && h.chain.Head().Number >= maxHead &&
					h.w.Status().Tip.Number == h.chain.Head().Number {
					h.assertModelFor(first, isWatched)
					checks++
				}
			}

		case op < 88 && liar >= 0 && rng.IntN(2) == 0: // the malicious endpoint lies for a while
			glitched[liar] = true
			var lie func(b *evmtest.Behavior)
			kind := rng.IntN(11)
			if rng2.IntN(6) == 0 {
				kind = 11
			}
			trace("  LIE: node %d, kind %d (0 height, 1 time, 2 hash, 3 no logs, 4 some logs, 5 invented log, 6/7 forgets blocks after a log scan, 8 frozen log height, 9 frozen latest block, 10 inflated block number)", liar, kind)
			switch kind {
			case 8:
				// The height that travels with its log answers
				// stops just above the cursor.
				lie = frozenBlockNumber(h.w.Status().Cursor + uint64(rng.IntN(2*testSettle)))
			case 9:
				// Its latest block stops where it is,
				// while its eth_blockNumber keeps up.
				frozen := h.chain.Head().Number - uint64(rng.IntN(testSettle))
				lie = staleLatest(h.chain, func() uint64 { return frozen })
			case 10:
				// A thousand added to its eth_blockNumber, and
				// nothing else.
				lie = func(b *evmtest.Behavior) {
					b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
						if method == "eth_blockNumber" {
							return hexUint(h.chain.Head().Number + 1000), nil, true
						}
						return nil, nil, false
					}
				}
			case 0:
				lie = lieAboutHeight(h.chain, uint64(1+rng.IntN(400)))
			case 1:
				shifts := []int64{1, -1, 60, -60, 3600, 40 * 24 * 3600, -40 * 24 * 3600}
				lie = lieAboutTime(h.chain, shifts[rng.IntN(len(shifts))])
			case 2:
				head := h.chain.Head().Number
				lie = lieAboutHash(h.chain, head-min(head, uint64(rng.IntN(3*testSettle))))
			case 3:
				lie = func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } }
			case 4:
				bit := byte(1 << rng.IntN(3))
				lie = func(b *evmtest.Behavior) { b.DropLogs = func(l evm.Log) bool { return l.TxHash[5]&bit != 0 } }
			case 5:
				if len(watched) == 0 {
					break
				}
				head := h.chain.Head().Number
				fake := fabricated(h, head-min(head, uint64(rng.IntN(2*testSettle))), watchedNo[rng.IntN(len(watched))], int64(1+rng.IntN(500)))
				lie = func(b *evmtest.Behavior) { b.ExtraLogs = append(b.ExtraLogs, fake) }
			case 6, 7:
				// Hide the transfers the consumer believes in,
				// and when asked about their blocks after a log scan,
				// know nothing (6) or name another block (7).
				lie = forgetAfterLogs(h.chain, h.sink.liveKeys(), rng.IntN(2) == 0)
			case 11:
				// Blocks asked for by number inside a batch — as the
				// blocks of transfers are, to date them — are missing, or
				// another block; asked for alone, the same blocks are
				// what they are.
				other := rng2.IntN(2) == 0
				lie = func(b *evmtest.Behavior) {
					b.InterceptBatch = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
						if method != "eth_getBlockByNumber" {
							return nil, nil, false
						}
						_, n, numeric := blockParam(params)
						if !numeric {
							return nil, nil, false
						}
						if blk, ok := h.chain.BlockByNumber(n); ok && other {
							blk.Hash[0] ^= 0xff
							return headerJSON(blk, blk.Time), nil, true
						}
						return nil, nil, true
					}
				}
			}
			if lie != nil {
				h.nodes[liar].Set(lie)
				liesNow[kind] = true
			}

		case op < 88: // an endpoint misbehaves for a while
			i := rng.IntN(nodes)
			glitched[i] = true
			wentDown := false
			h.nodes[i].Set(func(b *evmtest.Behavior) {
				defer func() {
					trace("  GLITCH: node %d: down=%v lag=%d logsLag=%d logsBackendLag=%d failRequests=%d rateLimited=%v hideReceipts=%v logsError=%v",
						i, b.Down, b.Lag, b.LogsLag, b.LogsBackendLag, b.FailRequests, b.RateLimited, b.HideReceipts, b.LogsError != nil)
				}()
				switch rng.IntN(8) {
				case 0:
					b.Down, wentDown = true, true
				case 1:
					b.Lag = uint64(1 + rng.IntN(12))
				case 2:
					// The log index silently trails the node's head. That
					// is survivable as long as the total silent lag stays
					// below the settle depth.
					b.LogsLag = uint64(1 + rng.IntN(testSettle-1))
					if !batches[i] {
						b.LogsBackendLag = 0
					}
				case 3:
					// A log backend behind the head backend. Where the
					// batched block number can expose it, any lag is
					// survivable; a provider without batches hides it,
					// and then it counts towards the silent lag.
					if batches[i] {
						b.LogsBackendLag = uint64(1 + rng.IntN(15))
					} else {
						b.LogsBackendLag = uint64(1 + rng.IntN(testSettle-1))
						b.LogsLag = 0
					}
				case 4:
					b.FailRequests = 1 + rng.IntN(4)
				case 5:
					b.RateLimited = true
					b.RetryAfter = rng.IntN(3)
				case 6:
					b.HideReceipts = true
				case 7:
					// Eth_getLogs is throttled, in the words of a
					// provider that has no log service at all.
					b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"}
				}
			})
			if wentDown {
				down[h.nodes[i]] = true
			}

		case op < 92: // the consumer side misbehaves
			trace("  the consumer misbehaves")
			switch rng.IntN(4) {
			case 0:
				h.sink.set(func(s *sink) { s.failObserved = 1 + rng.IntN(2) })
			case 1:
				h.sink.set(func(s *sink) { s.failHead = 1 })
			case 2:
				h.store.mu.Lock()
				h.store.failSave = 1 + rng.IntN(2)
				h.store.mu.Unlock()
			case 3:
				h.watch.mu.Lock()
				h.watch.fail = 1
				h.watch.mu.Unlock()
			}

		case op < 96: // things get better
			trace("  HEAL")
			heal()

		default: // process restart (possibly in the middle of trouble)
			heldStreak = 0
			if rng.IntN(2) == 0 {
				// The configuration may have changed with the restart: the
				// endpoints in another order, or one replaced by another.
				switch r := rng2.IntN(6); {
				case r < 2 && nodes >= 2:
					order := rng2.Perm(nodes)
					trace("  the endpoints are reordered: %v", order)
					rearrange(order)
				case r == 2 && nodes >= 2:
					var ordinary []int
					for i := range nodes {
						if !special(i) {
							ordinary = append(ordinary, i)
						}
					}
					if len(ordinary) == 0 {
						break
					}
					gone := ordinary[rng2.IntN(len(ordinary))]
					var order []int
					for i := range nodes {
						if i != gone {
							order = append(order, i)
						}
					}
					rearrange(order)
					at := rng2.IntN(nodes + 1)
					trace("  endpoint %d is replaced by a new one, at position %d", gone, at)
					addNode(at)
				case r == 3 && nodes >= 2 && nodes < 4 && !independentChecked:
					// An alias: an endpoint configured a second time,
					// under another URL (another port: the same provider,
					// as far as anybody can tell from outside). An alias of
					// an honest endpoint must cost nothing. (One of a lying
					// endpoint is two opinions from one liar: the trust
					// model's premise of independent providers excludes it.)
					var ordinary []int
					for i := range nodes {
						if !special(i) {
							ordinary = append(ordinary, i)
						}
					}
					if len(ordinary) == 0 {
						break
					}
					o := ordinary[rng3.IntN(len(ordinary))]
					h.nodes, h.urls, limits = append(h.nodes, h.nodes[o]), append(h.urls, h.nodes[o].Serve(t)), append(limits, limits[o])
					batches, glitched = append(batches, batches[o]), append(glitched, glitched[o])
					order := make([]int, 0, len(h.nodes))
					for i := range len(h.nodes) - 1 {
						order = append(order, i)
					}
					at := rng3.IntN(len(order) + 1)
					order = slices.Insert(order, at, len(h.nodes)-1)
					rearrange(order)
					trace("  endpoint %d is configured a second time, under %s, at position %d", o, h.urls[at], at)
				}
				trace("  RESTART with a new RPC client")
				h.restartWithNewClient() // everything learnt about the endpoints is gone
			} else {
				trace("  RESTART")
				h.restart()
			}
		}
		maxHead = max(maxHead, h.chain.Head().Number)
		if rng.IntN(5) == 0 {
			h.clock.Advance(randomDuration(rng))
		}
	}

	// The endpoints report finality again (it had stopped, or not).
	finStopped, finStopIn = false, 0
	for _, n := range h.nodes {
		n.Set(func(b *evmtest.Behavior) { b.NoFinalized = false })
	}
	h.passCooldowns()
	// Calm down and let the watcher converge. The chain grows past its
	// former maximum once more (so every replaced block has a successor at
	// its height), then stands still: the truth is fixed.
	heal()
	if (noLogsAtFirst >= 0 || (logsException && noLogsEver < 0)) && !noLogsForGood {
		// Every endpoint serves logs now. The one that did not is asked
		// again when its pause ends, and makes up for the blocks that were
		// settled without it as further blocks are settled — one step of
		// its log range per tick.
		for i := 0; i < 400; i++ {
			if st := h.w.Status(); st.UncheckedFrom == 0 && !st.LogsUnchecked && i > 0 {
				break
			}
			h.passLogPause()
			h.chain.Mine(1)
			_ = h.w.Tick(context.Background())
		}
		maxHead = max(maxHead, h.chain.Head().Number)
	}
	if loglessHider {
		// The endpoint that had no log service has made up for the blocks
		// settled without it, while the other still hid what it hid; now
		// that lie ends too.
		hiddenMu.Lock()
		hiding = false
		hiddenMu.Unlock()
	}
	if soleSource {
		// The added endpoint makes up for the blocks the lone one settled —
		// one step of its log range per tick — while the lone one still
		// hides what it hid; then the lie ends, like every other here.
		for phase := range 2 {
			for i := 0; i < 400; i++ {
				if st := h.w.Status(); st.UncheckedFrom == 0 && !st.LogsUnchecked && i > 0 {
					break
				}
				h.chain.Mine(1)
				_ = h.w.Tick(context.Background())
				h.passCooldowns()
			}
			if phase == 0 {
				hiddenMu.Lock()
				hiding = false
				hiddenMu.Unlock()
			}
		}
		maxHead = max(maxHead, h.chain.Head().Number)
	}
	h.chain.Mine(int(maxHead-min(maxHead, h.chain.Head().Number)) + 2)
	trace("CONVERGE: head %d", h.chain.Head().Number)
	var lastErr error
	for i := 0; i < 40; i++ {
		lastErr = h.w.Tick(context.Background())
		st := h.w.Status()
		trace("  CONVERGE TICK %d: err=%v cursor=%d unchecked=%d..%d logsUnchecked=%v primary=%s verifier=%s logVerifier=%s hold=%q",
			i, lastErr, st.Cursor, st.UncheckedFrom, st.UncheckedTo, st.LogsUnchecked, st.Endpoint, st.Verifier, st.LogVerifier, st.Hold)
		if debugLogs {
			for _, es := range h.client.Status() {
				trace("    endpoint %s: state=%s logs=%+v", es.URL, es.State, es.Logs)
			}
		}
		if lastErr == nil && st.Unverified == 0 && st.Tip.Number == h.chain.Head().Number && (st.UncheckedFrom == 0 || noLogsForGood) {
			break
		}
		h.passCooldowns()
		if (noLogsAtFirst >= 0 || (logsException && noLogsEver < 0)) && !noLogsForGood {
			// The endpoint is asked for logs again. (Not one that has none
			// for good: its pause would end every tick, and it would be
			// asked as the endpoint in use every tick — once in half an hour
			// in production.)
			h.passLogPause()
		}
	}
	if lastErr != nil {
		t.Fatalf("watcher did not recover: %v\n%s", lastErr, tail(h.logs.String(), 3000))
	}
	h.exempt = lateToRetired // what a lone log source hid has been made up for by now
	h.assertModelFor(first, isWatched)

	st := h.w.Status()
	if st.DeepReorgs != 0 {
		t.Errorf("deep reorg reported in a history without one")
	}
	if nodes >= 2 && mode == watcher.VerifyAuto && !noLogsForGood && (st.UncheckedFrom != 0 || st.UncheckedTo != 0 || st.LogsUnchecked) {
		// Every endpoint serves logs by now: whatever was settled on one
		// endpoint's logs has been scanned by a second.
		t.Errorf("INVARIANT 3: blocks %d..%d are still unchecked after convergence (logs unchecked: %v)", st.UncheckedFrom, st.UncheckedTo, st.LogsUnchecked)
	}
	if head := h.chain.Head().Number; st.Cursor != head-testSettle {
		t.Errorf("cursor = %d after convergence, head = %d", st.Cursor, head)
	}
	// The processed head is the cursor's block, on the canonical chain.
	if blk, ok := h.chain.BlockByNumber(st.Cursor); !ok || h.sink.lastHead().Number != blk.Number || h.sink.lastHead().Hash != blk.Hash {
		t.Errorf("sink head = %d (%s), cursor = %d", h.sink.lastHead().Number, h.sink.lastHead().Hash, st.Cursor)
	}
	_, _ = ticksOK, checks
	for _, s := range h.client.Status() {
		if s.State == evm.StateWrongChain {
			t.Errorf("endpoint %s marked wrong-chain", s.URL)
		}
	}
}

func randomDuration(rng *rand.Rand) time.Duration {
	return time.Duration(1+rng.IntN(20)) * time.Second
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// forgetAfterLogs makes a node hide the given transfers from its logs and,
// when asked by number for one of their blocks after it has answered a log
// query in the same tick, answer null — or, with otherHash, another block.
// In the head batch at the start of a tick it answers truthfully.
func forgetAfterLogs(chain *evmtest.Chain, transfers map[key]watcher.Transfer, otherHash bool) func(b *evmtest.Behavior) {
	hide := make(map[evm.Hash]bool)
	blocks := make(map[uint64]bool)
	for _, tr := range transfers {
		hide[tr.TxHash] = true
		blocks[tr.BlockNumber] = true
	}
	var mu sync.Mutex
	afterLogs := false
	return func(b *evmtest.Behavior) {
		b.DropLogs = func(l evm.Log) bool { return hide[l.TxHash] }
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			mu.Lock()
			defer mu.Unlock()
			if method == "eth_getLogs" {
				afterLogs = true
				return nil, nil, false
			}
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			tag, n, numeric := blockParam(params)
			if tag == "latest" {
				afterLogs = false
			}
			if !numeric || !afterLogs || !blocks[n] {
				return nil, nil, false
			}
			if blk, ok := chain.BlockByNumber(n); ok && otherHash {
				blk.Hash[0] ^= 0xff
				return headerJSON(blk, blk.Time), nil, true
			}
			return nil, nil, true
		}
	}
}
