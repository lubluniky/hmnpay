package watcher

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

// ErrInconsistentEndpoint is returned (wrapped) by Tick when the primary
// endpoint contradicted itself or the chain the watcher knows to be final,
// in a way that cannot be explained by a reorg: logs missing from a block it
// still reports as canonical, a block below its own head that it does not
// know, a conflict with a finalized block. The endpoint is marked faulty so
// that the next tick prefers another one.
var ErrInconsistentEndpoint = errors.New("watcher: inconsistent RPC endpoint")

// tick is the working state of one Tick.
type tick struct {
	w   *Watcher
	ctx context.Context
	// now is the local clock's reading at the start of the tick; clock the
	// latest reading the check for blocks from the future has taken (see
	// future), zero until it has taken one.
	now   time.Time
	clock time.Time

	primary *evm.Endpoint
	// head is the primary's latest block: how far this tick scans at most.
	// headReq is the request it was read with (see rereadHead).
	head     evm.Header
	headReq  evm.HeadRequest
	haveHead bool
	// priFinalized is the block the primary reports as finalized (nil if it
	// reports none).
	priFinalized *evm.Header
	// scannedTo is the last block covered by this tick's scan.
	scannedTo uint64
	// logHead is the height of the backend that answered the log queries:
	// the head, unless that backend revealed itself to be behind.
	logHead uint64
	watched int
	// settleLimit is the lowest block with an unresolved observation or
	// revert; the cursor — and with it the head reported to the sink —
	// must stay below it.
	settleLimit uint64
	// pending are the keys of the transfers the consumer holds as not yet
	// final (from the watch-set snapshot); they are never forgotten.
	pending map[transferKey]struct{}
	// dirty is true when the durable state changed since the last save.
	dirty bool
	// initialised is set in the tick that chose the start position because
	// there was no checkpoint; started once that position — lowered to
	// what the watch set demands — has been saved.
	initialised bool
	started     bool
	// reverted is set when this tick saw a reorganisation: a reported
	// transfer was reverted, or settled blocks were replaced.
	reverted bool

	// startHeaders are the canonical headers fetched together with the
	// head (by number), as seen by the primary at the start of the tick.
	startHeaders map[uint64]evm.Header

	// ver is the endpoint that cross-checks the primary in this tick, and
	// independent whether it is another endpoint than the primary. ver is
	// nil when the verification mode demands an independent endpoint and
	// none answered (verErr says why): then nothing can be settled.
	ver         *evm.Endpoint
	independent bool
	verErr      error
	// solo is set when, under VerifyBestEffort, the primary is believed on
	// its own in this tick because every other endpoint has been
	// unreachable for long enough (resolveVerifier). ver is then the
	// primary, as with a single configured endpoint.
	solo bool
	// topHeight is the highest chain height any endpoint showed in this
	// tick, and topBy the label of the endpoint that showed it.
	topHeight uint64
	topBy     string
	// base is the agreed block a reorg window below pre (or the consumer's
	// final block, if higher), remembered with pre when nothing is
	// remembered yet (prepare).
	base *blockRef
	// shownTop is the highest chain height an endpoint had shown when the
	// tick came to settle (highestShown), and shownBy who showed it;
	// shownTaken is set then.
	shownTop   uint64
	shownBy    string
	shownTaken bool
	// began is the position this tick is measured from — whether the head
	// moved in it, or was held: the cursor as the tick found it; in the
	// first tick of a process, the cursor of the checkpoint it loaded or,
	// without one, the start position it chose (before the watch set
	// rewound it).
	began uint64
	// voted is set when the primary and the verifier were more than a
	// settle depth apart and a third endpoint was asked where the chain
	// ends (resolveVerifier).
	voted bool
	// logStalled is set when the head could not move in this tick for the
	// one reason that the primary's log index is behind the chain.
	logStalled bool
	// verTip is the verifier's own latest block, verHead its number,
	// verFinalized the block it reports as finalized and verHeaders the
	// headers it has returned in this tick (independent verifier only).
	verTip       evm.Header
	verHead      uint64
	verFinalized *evm.Header
	verHeaders   map[uint64]evm.Header
	// finalKnown, noFinality and finalityDispute are what agreeFinality
	// found in this tick.
	finalKnown      bool
	noFinality      bool
	finalityDispute string
	// otherEPs caches others().
	otherEPs   []*evm.Endpoint
	othersDone bool

	// pre is the header of the block the cursor is expected to move to, as
	// both endpoints reported it before the scan began (nil if there was
	// no agreement, or nothing to settle).
	pre *blockRef

	// set and recipients are the addresses this tick scans for.
	set        map[ethaddr.Address]struct{}
	recipients []ethaddr.Address
	// doubted are the transfers (with their block numbers) that one
	// endpoint reported and no other confirmed in this tick.
	doubted map[transferKey]uint64
	// holding says why the processed head did not move, if it did not.
	holding string
	// confirmedTo is the last block whose log scan a second endpoint
	// confirmed in this tick; logVerifier is that endpoint. logsUnchecked
	// is set when a range was settled on the primary's logs alone.
	confirmedTo   uint64
	logVerifier   string
	logsUnchecked bool
	// headAgreed is set once the primary and the verifier have agreed, in
	// this tick, on the header of the block the cursor rests on
	// (Watcher.verifiedHead). Only then is that block reported as the
	// processed head: see cursorHeader.
	headAgreed bool
	// secondLogsTo, if not zero, is the last block the second endpoint's
	// logs vouched for when it could not vouch for everything it was asked
	// about in this tick: nothing above it is settled before the next tick.
	secondLogsTo uint64
	// secondLogsBehind, if not empty, says why: the second endpoint's logs
	// end there (not a page of a longer scan).
	secondLogsBehind string
	// agreed caches the headers the primary and the verifier agreed on in
	// this tick; times the block timestamps worked out for transfers.
	agreed map[uint64]evm.Header
	times  map[evm.Hash]uint64
	// settledBefore is the cursor before this tick's scan; minSince the
	// lowest Since of the watched addresses, if every one has one
	// (sinceKnown); rechecked is set once recheck has run in this tick.
	settledBefore uint64
	minSince      uint64
	sinceKnown    bool
	rechecked     bool
	// pendingListed is the number of transfers the consumer listed as not
	// yet final, decodable or not; consumerBehind, if not zero, is the
	// consumer's processed head when it lies below the head the sink
	// accepted last; open is the number of watched addresses that are not
	// closed (WatchedAddress.Closed) and observed the number of transfers
	// the sink accepted in this tick. Each keeps the watcher from idling
	// (tick.work).
	pendingListed  int
	consumerBehind uint64
	open           int
	observed       int
	// rewound is set when the tick moved the cursor back for the watch set
	// (a new address that can have been paid lower, a transfer the consumer
	// holds): what it settled again below where it began was work, not a
	// head held in silence.
	rewound bool
}

func (t *tick) run() error {
	w := t.w
	ctx := t.ctx
	t.settleLimit = math.MaxUint64

	if !w.loaded {
		if err := w.load(ctx); err != nil {
			return err
		}
	}
	st := &w.st
	t.began = st.Cursor
	if w.unsaved {
		t.dirty = true // the last checkpoint could not be written (save)
	}
	t.markLogServers()

	primary, err := w.cfg.Client.Pick(ctx, evm.NeedLogs())
	if err != nil {
		return fmt.Errorf("watcher: no RPC endpoint: %w", err)
	}
	t.primary = primary

	// 1. Head, finalized block, and the blocks we remember best — in one
	// request.
	req := evm.HeadRequest{Finalized: !w.cfg.NoFinality}
	if a, ok := st.newestAnchor(); ok {
		req.Numbers = append(req.Numbers, a.Number)
	}
	if f := st.Finalized; f.Number != 0 && (len(req.Numbers) == 0 || req.Numbers[0] != f.Number) {
		req.Numbers = append(req.Numbers, f.Number)
	}
	t.headReq = req
	hr, err := primary.Head(ctx, req)
	if err != nil {
		return fmt.Errorf("watcher: fetch head: %w", err)
	}
	t.head = hr.Latest
	t.priFinalized = hr.Finalized
	t.logHead = hr.Latest.Number
	t.haveHead = true
	t.startHeaders = hr.ByNumber
	t.startHeaders[hr.Latest.Number] = hr.Latest
	t.sawHeight(primary, hr.Latest.Number)

	// Who cross-checks the primary in this tick? The verifier is asked for
	// its own head and, in the same request, for the header of the block
	// the cursor will most likely move to. Where the two do not agree on
	// the height of the chain and a third endpoint exists, it decides.
	guess := t.head.Number - min(t.head.Number, w.cfg.SettleDepth)
	if err := t.resolveVerifier(guess); err != nil {
		return err
	}
	if err := t.ctx.Err(); err != nil {
		return err
	}
	// A block cannot come from the future, whoever vouches for it.
	if err := t.checkClock(); err != nil {
		return err
	}

	// First start: choose where to begin. The height of the chain is what
	// both endpoints say it is, not what one of them claims — and "both" is
	// meant: if their heads are further apart than honest endpoints ever
	// are, they do not say the same thing, and the lower of two heads is
	// then no better a start than the higher (it is wherever the endpoint
	// that is behind, or says it is, happens to be, and the position would
	// be saved and reported to the consumer from there). A
	// third endpoint has had its say by now (resolveVerifier); without one
	// the watcher does not start until the two agree.
	if !st.Initialized {
		if t.ver == nil {
			return fmt.Errorf("watcher: cannot choose a start position: no second endpoint to confirm the chain head: %w", t.verErr)
		}
		tip := t.chainTip()
		settled := tip - min(tip, w.cfg.SettleDepth)
		// fromHeads: the position is where the chain ends, not one the
		// caller named below that.
		start, fromHeads := settled, true
		switch {
		case w.cfg.StartBlock != nil && *w.cfg.StartBlock > 0:
			start, fromHeads = min(*w.cfg.StartBlock-1, settled), *w.cfg.StartBlock-1 > settled
		case w.cfg.StartBlock != nil:
			start, fromHeads = 0, false
		}
		if spread, ok := t.headsAgree(); fromHeads && !ok {
			return fmt.Errorf("watcher: cannot choose a start position: %s is at block %d and %s at block %d, %d blocks apart; the watcher starts when they agree on where the chain ends (or a third endpoint settles it)",
				t.primary.Label(), t.head.Number, t.ver.Label(), t.verHead, spread)
		}
		st.Cursor = start
		st.Initialized = true
		t.initialised, t.began = true, start
		t.dirty = true
	}

	// A requested re-scan rewinds the cursor. The rewind is saved at once,
	// so that the request survives a crash during the scan it causes.
	if from, ok := w.takeRescan(); ok {
		if target := from - min(from, 1); target < st.Cursor {
			w.log.Info("chain watcher re-scan requested", "from", from, "cursor", st.Cursor)
			// Remembered with the rewind: these blocks are to be scanned
			// because somebody asked, whatever else says they need not be.
			st.RescanTo = max(st.RescanTo, st.Cursor)
			t.setCursor(target)
			if err := w.save(ctx); err != nil {
				w.Rescan(from) // not recorded: keep the request
				return err
			}
		}
		for _, tr := range st.Tracked {
			if tr.BlockNumber >= from {
				tr.delivered = false // report again what is found again
			}
		}
	}

	// 2. Is this endpoint's chain the one we know? First: does it reach as
	// far as the checkpoint says the chain has been settled; then: are the
	// blocks we remember still there.
	if err := t.checkpointAhead(); err != nil {
		return err
	}
	if err := t.checkChain(); err != nil {
		return err
	}

	// 3. Watch-set snapshot. It must be taken after the head was fetched:
	// see WatchSet.
	snap, err := w.cfg.Watch.Watched(ctx)
	if err != nil {
		return fmt.Errorf("watcher: read watch set: %w", err)
	}
	set, err := t.applySnapshot(snap)
	if err != nil {
		return err
	}
	t.set = set
	t.started = true
	t.scannedTo = st.Cursor
	t.settledBefore = st.Cursor

	// 4. The block the cursor should reach in this tick, as both endpoints
	// see it now — before the scan. See settle for why "before".
	if err := t.prepare(); err != nil {
		return err
	}

	// 5–6. Scan, diff, deliver; then settle.
	if err := t.scan(); err != nil {
		return err
	}
	if err := t.settle(); err != nil {
		return err
	}
	if err := t.recheckOwed(); err != nil {
		return err
	}

	// Finality: remember the highest finalized block the endpoints agree
	// on.
	if err := t.agreeFinality(); err != nil {
		return err
	}

	// The block the cursor rests on, as agreed between the endpoints — if
	// there is a head to report (which depends on the finalized block, so
	// that comes first).
	head, agreed, err := t.cursorHeader()
	if err != nil {
		return err
	}
	// Forget what left the window — measured from the height the endpoints
	// agree on, so that no endpoint can talk the watcher into forgetting a
	// transfer early.
	tip := t.safeHead()
	if t.ver == nil {
		tip = 0 // one endpoint's claim about the chain's height forgets nothing
	}
	for k, tr := range st.Tracked {
		if _, held := t.pending[k]; held {
			continue // the consumer still counts on it
		}
		if tr.BlockNumber <= st.Cursor && tr.BlockNumber+w.cfg.ReorgWindow < tip {
			delete(st.Tracked, k)
			t.dirty = true
		}
	}
	if t.dirty {
		if err := w.save(ctx); err != nil {
			return err
		}
		t.dirty = false
	}

	// 7. Report the processed head — the cursor — if it changed.
	if !agreed {
		return nil
	}
	return t.reportHead(head)
}

func (w *Watcher) takeRescan() (uint64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rescanFrom == nil {
		return 0, false
	}
	from := *w.rescanFrom
	w.rescanFrom = nil
	return from, true
}

// applySnapshot folds the consumer's view into the tick: it returns the set
// of addresses to scan for, rewinds the cursor for addresses that are new
// and may already have been paid, and adopts transfers the consumer holds as
// pending but the watcher does not track.
func (t *tick) applySnapshot(snap Snapshot) (map[ethaddr.Address]struct{}, error) {
	w := t.w
	st := &w.st
	t.watched = len(snap.Addresses)
	t.pendingListed = len(snap.Pending)
	if sent := w.headSent.Number; snap.Processed != 0 && snap.Processed < sent {
		t.consumerBehind = snap.Processed
	}

	// The record of unchecked blocks is held against the configuration
	// this watcher runs with (state.reconcileUnchecked).
	if st.reconcileUnchecked(t.configuredLabel) {
		t.dirty = true
	}

	rewind := st.Cursor
	first := w.seen == nil
	t.sinceKnown = len(snap.Addresses) > 0
	for _, a := range snap.Addresses {
		if !a.Closed {
			t.open++
		}
		switch {
		case a.Since == 0:
			t.sinceKnown = false
		case t.minSince == 0 || a.Since < t.minSince:
			t.minSince = a.Since
		}
	}
	t.skipUnneeded(snap)
	rewind = st.Cursor
	seen := make(map[ethaddr.Address]struct{}, len(snap.Addresses))
	set := make(map[ethaddr.Address]struct{}, len(snap.Addresses)+len(st.Tracked))
	for _, a := range snap.Addresses {
		seen[a.Address] = struct{}{}
		set[a.Address] = struct{}{}
		_, known := w.seen[a.Address]
		switch {
		case a.Since == 0 || a.Since >= rewind:
			// Nothing to scan again for this address.
		case t.initialised:
			// The start position was chosen in this very tick, without
			// having scanned anything: it must not lie above a block in
			// which an address that already exists may have been paid.
			// (The start is chosen when the first tick succeeds, which can
			// be long after the watcher was created — its endpoints may
			// have been down while payments were made.)
			rewind = a.Since
		case !first && !known:
			// A new address whose payment may have been made in blocks
			// that were settled without it: scan them again.
			rewind = a.Since
		}
		// Addresses that were already there when a watcher with a
		// checkpoint started are covered by that checkpoint (see WatchSet).
	}
	w.seen = seen

	t.pending = make(map[transferKey]struct{}, len(snap.Pending))
	for _, p := range snap.Pending {
		if p.Amount == nil || p.Amount.Sign() <= 0 || p.BlockHash.IsZero() || p.TxHash.IsZero() {
			continue
		}
		p.Token = w.cfg.Token
		k := keyOf(p)
		t.pending[k] = struct{}{}
		if _, tracked := st.Tracked[k]; tracked {
			continue
		}
		// The consumer counts on a transfer the watcher has no record of
		// (its checkpoint was lost, or restored from an older copy). Track
		// it, and make sure its block is examined: if it is not on the
		// chain any more, the scan reports the revert.
		if other := st.occupant(p); other != nil {
			// Not if the watcher tracks another transaction's transfer at
			// that very position of that very block: they cannot both be
			// true, and the watcher has verified its own.
			w.log.Error("not adopting a pending transfer of the consumer: another transfer is tracked at the same log index of the same block",
				"tx", p.TxHash.String(), "log_index", p.LogIndex, "block", p.BlockNumber, "block_hash", p.BlockHash.String(),
				"tracked_tx", other.TxHash.String())
			continue
		}
		st.Tracked[k] = &tracked{Transfer: p, delivered: true}
		t.dirty = true
		if p.BlockNumber > 0 && p.BlockNumber-1 < rewind {
			rewind = p.BlockNumber - 1
		}
		w.log.Warn("adopting a pending transfer known to the consumer but not to the watcher", "tx", p.TxHash.String(),
			"log_index", p.LogIndex, "block", p.BlockNumber)
	}
	if rewind < st.Cursor {
		if !t.initialised {
			t.rewound = true
			w.log.Info("chain watcher rewinding", "from_cursor", st.Cursor, "to", rewind)
			// The consumer asked for these blocks — an address that can
			// have been paid in them, a transfer it holds — so they are
			// scanned whatever its processed head says (skipUnneeded),
			// also if this tick does not get through them.
			st.RescanTo = max(st.RescanTo, st.Cursor)
		}
		t.setCursor(rewind)
	}
	if t.initialised {
		w.log.Info("chain watcher starting without checkpoint", "first_block", st.Cursor+1, "head", t.head.Number,
			"watched_addresses", len(snap.Addresses))
	}
	if t.dirty {
		// Recorded before anything else happens, so that a crash cannot
		// lose the rewind.
		if err := w.save(t.ctx); err != nil {
			w.seen = nil // examine every address again next time
			return nil, err
		}
		t.dirty = false
	}

	// The filter covers the watch set and, additionally, the recipients of
	// transfers still being tracked: a transfer must stay visible for as
	// long as its revert would have to be reported, even if its address
	// has left the watch set in the meantime.
	for _, tr := range st.Tracked {
		set[tr.To] = struct{}{}
	}
	return set, nil
}

// skipUnneeded moves the cursor up, past blocks that nobody needs scanned,
// when it lies more than a reorg window below the first block that somebody
// does: see "A checkpoint below the consumer" in the package documentation.
//
// What is needed is not for an endpoint to say. Two things bound it from
// below, and both come from the consumer:
//
//   - its processed head (Snapshot.Processed): the consumer holds every
//     transfer to the watched addresses up to that block, and what lies more
//     than a reorg window below it cannot change any more;
//   - the Since of the watched addresses, when every address has one: no
//     address can have been paid at or below the lowest of them. (With no
//     address at all, nothing below the chain's settled height is needed.)
//
// The cursor goes to the higher of the two — never above the settled height
// of the chain the endpoints agree on, never onto or past the block of a
// transfer the watcher or the consumer still tracks, not while a re-scan
// that was asked for is under way (state.RescanTo), and not in the tick
// after one that left this watcher at rest (an idle pause is not a wrong
// position, however many blocks it lasted).
//
// This is what keeps a wrong position from outliving its cause: a checkpoint
// written far below the chain (a start that followed one endpoint down,
// as an older version without the bound could write) would otherwise be scanned from, block by
// block, with the consumer's head dragged back to it.
func (t *tick) skipUnneeded(snap Snapshot) {
	w := t.w
	st := &w.st
	if t.ver == nil || t.initialised || st.RescanTo > st.Cursor {
		return
	}
	if w.rested {
		// The last tick left this watcher at rest: in step with the chain
		// both endpoints showed, with nothing to watch, or with closed
		// addresses only. The cursor is where that tick settled it — or
		// where this one has just put it back to, because the chain was
		// reorganised or turned out shorter — and not where a checkpoint
		// claims it to be. The chain may have moved on by more than a reorg
		// window since (an idle pause can be that long), but nothing is
		// wrong with the position and nothing is to be repaired: the scan
		// goes on from it like after any other pause — through every block
		// of the pause, for every address that is watched — and the
		// remembered blocks stay a chain without a gap.
		return
	}
	window := w.cfg.ReorgWindow
	tip := t.chainTip()
	settled := tip - min(tip, w.cfg.SettleDepth)
	target := snap.Processed - min(snap.Processed, window)
	switch {
	case len(snap.Addresses) == 0:
		target = settled
	case t.sinceKnown:
		target = max(target, t.minSince)
	}
	target = min(target, settled)
	// What was reported ahead of the head has not had its last look yet.
	// (What lies at or below the cursor has, and blocks that a second log
	// endpoint still has to make up for are found by their own record,
	// state.Unchecked, wherever the cursor is.)
	for _, tr := range st.Tracked {
		if tr.BlockNumber > st.Cursor {
			target = min(target, tr.BlockNumber-1)
		}
	}
	for _, p := range snap.Pending {
		if p.BlockNumber > st.Cursor {
			target = min(target, p.BlockNumber-1)
		}
	}
	if target <= st.Cursor || target-st.Cursor <= window {
		return
	}
	from := st.Cursor
	t.setCursor(target)
	w.log.Warn("chain watcher checkpoint lies below everything that has to be scanned; continuing from there",
		"checkpoint_cursor", from, "cursor", target, "consumer_head", snap.Processed, "watched_addresses", len(snap.Addresses), "chain", tip)
	t.incident(IncidentCheckpointBehind, nil, target+1, fmt.Sprintf(
		"the checkpoint's position (block %d) lay %d blocks below anything that has to be scanned: the consumer has processed block %d, and no watched address can have been paid below block %d; continuing from block %d",
		from, target-from, snap.Processed, target+1, target+1))
}

// prepare fetches, before the scan, the header of the block the cursor is
// expected to move to in this tick, and requires the primary and the
// verifier to agree on it.
func (t *tick) prepare() error {
	st := &t.w.st
	safe := t.safeHead()
	p := safe - min(safe, t.w.cfg.SettleDepth)
	if p <= st.Cursor || t.ver == nil {
		return nil
	}
	h, ok, err := t.agreedHeader(p)
	if err != nil || !ok {
		return err
	}
	t.pre = &blockRef{Number: h.Number, Hash: h.Hash, Time: h.Time}
	if len(st.Anchors) == 0 {
		// Nothing is remembered yet: the watcher started without a
		// checkpoint (or an accepted contradiction of history cleared what
		// it remembered). A running watcher remembers blocks a whole reorg
		// window down; one that remembered only the block it is about to
		// settle on would take any reorganisation that replaces that block
		// — an ordinary one, a few blocks deep — for one that replaced its
		// whole window. So the block a window below is remembered too, or
		// the consumer's final block if that is higher (below it nothing
		// changes): a reorganisation that leaves it standing is an ordinary
		// one, and everything above it is scanned again.
		// (Wanted, not needed: if the two endpoints cannot show that
		// block, the same as before — only the block settled on.)
		base := max(t.w.cfg.FinalizedFloor, p-min(p, t.w.cfg.ReorgWindow))
		if base > 0 && base < p {
			bh, ok, err := t.headerIfAgreed(base)
			if err != nil {
				return err
			}
			if ok {
				t.base = &blockRef{Number: bh.Number, Hash: bh.Hash, Time: bh.Time}
			}
		}
	}
	return nil
}

// headerIfAgreed returns the header of block `number` if the primary and the
// verifier both report it, identical, and nothing otherwise: for a block the
// watcher would like to remember, not one it has to have. A failure to answer
// is no fault of the endpoint here, and holds nothing; the error is only that
// of the tick's context.
func (t *tick) headerIfAgreed(number uint64) (evm.Header, bool, error) {
	if h, done := t.agreed[number]; done {
		return h, true, nil
	}
	ph, err := t.primary.HeaderByNumber(t.ctx, number)
	if cerr := t.ctx.Err(); cerr != nil {
		return evm.Header{}, false, cerr
	}
	if err != nil {
		return evm.Header{}, false, nil
	}
	if t.independent {
		vh, err := t.ver.HeaderByNumber(t.ctx, number)
		if cerr := t.ctx.Err(); cerr != nil {
			return evm.Header{}, false, cerr
		}
		if err != nil || *vh != *ph {
			return evm.Header{}, false, nil
		}
	}
	if t.implausible(*ph) != "" {
		return evm.Header{}, false, nil
	}
	return *ph, true, nil
}

// settle advances the cursor — the processed head — as far as this tick's
// scan allows: to SettleDepth below the lowest height any involved backend
// has reached, and never to or past a block with an unresolved observation
// or revert.
//
// Nothing that a single endpoint says becomes the processed head (unless
// there is only one endpoint to ask). Before the cursor moves:
//
//   - the log scan of the blocks being settled is repeated on a second
//     log-serving endpoint, if there is one, and whatever that endpoint has
//     and the primary did not report goes through verification like any
//     other transfer (confirmRange);
//   - the header of the block the cursor moves onto is fetched from the
//     primary and from the verifier, and its number, hash, parent hash and
//     timestamp must be identical (agreedHeader).
//
// The header agreed on before the scan (prepare) becomes an anchor: a block
// hash commits to all its ancestors, so if that block is still canonical on
// the next tick, nothing at or below it has changed since before the scan —
// neither during the scan nor between the scans of the two endpoints. A
// header fetched after the scan could not promise that, which is why the
// anchor is taken first, and why, when the cursor ends up below the prepared
// block (a transfer waiting for confirmation holds it, or the log backend is
// behind), the prepared block is remembered all the same.
func (t *tick) settle() error {
	// What the endpoints had shown when the tick decided where to settle —
	// in this tick so far, or by the end of the last one: what Tick holds
	// the head to (invariant 4). A block mined while the tick runs is for
	// the next one to settle; a longer chain an endpoint shows only late in
	// every tick (asked for its finalized block after the others, say) is
	// held against in the next. (The first tick of a process has no last
	// one: it is held to everything it was shown.)
	if w := t.w; w.shownBefore {
		t.shownTop, t.shownBy = t.highestShown()
		if w.lastShown > t.shownTop {
			t.shownTop, t.shownBy = w.lastShown, w.lastShownBy
		}
		t.shownTaken = true
	}
	err := t.settleCursor()
	if err == nil && t.holding == "" && t.w.st.Cursor <= t.began {
		// The cursor moved in this tick, but not above where the tick
		// began: it was rewound (for the watch set, or below the start
		// chosen in the first tick of a process) and settled back up no
		// further than that. Whatever stopped it there holds the head, and
		// is named as when nothing is settled at all.
		t.nameHold()
	}
	return err
}

func (t *tick) settleCursor() error {
	w := t.w
	st := &w.st
	target := t.settleTarget(t.scannedTo)
	if target <= st.Cursor {
		// Nothing is settled in this tick. That is in order when there is
		// nothing to settle — the chain has not moved a block that is deep
		// enough — and has to be named in every other case (invariant 4;
		// what is not named here is named by Tick, from the heights the
		// endpoints showed).
		t.nameHold()
		t.logIndexVerdict()
		return nil
	}
	t.logIndexVerdict()
	if t.ver == nil {
		t.holdf("no second endpoint available to confirm the head: %v", t.verErr)
		return nil
	}
	confirmed, ok, err := t.confirmRange(st.Cursor+1, target)
	if err != nil || !ok {
		return err
	}
	// No further than the second endpoint's logs vouch for; and the second
	// scan may have turned up transfers that now hold the head.
	if target = min(target, confirmed, t.settleTarget(t.scannedTo)); target <= st.Cursor {
		t.holdf("a transfer in block %d is waiting to be confirmed by another endpoint", t.settleLimit)
		return nil
	}
	if t.pre == nil {
		t.holdf("the endpoints have not agreed on a block to settle on")
		return nil
	}
	head := *t.pre
	if target < t.pre.Number {
		h, ok, err := t.agreedHeader(target)
		if err != nil || !ok {
			return err
		}
		head = blockRef{Number: h.Number, Hash: h.Hash, Time: h.Time}
	}
	if t.base != nil && len(st.Anchors) == 0 {
		st.addAnchor(*t.base, w.cfg.ReorgWindow, max(w.cfg.ReorgWindow/64, 1))
	}
	if a, ok := st.newestAnchor(); !ok || a.Number < t.pre.Number {
		st.addAnchor(*t.pre, w.cfg.ReorgWindow, max(w.cfg.ReorgWindow/64, 1))
	}
	// (An anchor at or above the prepared block is one the primary has
	// confirmed at the start of this tick; it keeps vouching.)
	st.Cursor = target
	w.verifiedHead = head
	t.headAgreed = true
	t.dirty = true
	return nil
}

// recheckOwed makes up for blocks settled on one endpoint's logs (recheck)
// in a tick that has not done so on the way: one that settled nothing — the
// chain did not move, or the head is held — or settled on the primary's logs
// alone. What a second endpoint is owed does not wait for new blocks: while
// it is not made up for, a payment the only log source left out stays
// unreported, and the addresses kept for it stay on the consumer's list.
func (t *tick) recheckOwed() error {
	if t.rechecked || t.logsUnchecked || t.ver == nil || len(t.w.st.Unchecked) == 0 {
		return nil
	}
	settled := t.safeHead() - min(t.safeHead(), t.w.cfg.SettleDepth)
	for _, lv := range t.logVerifiers() {
		if h := lv.Health().Logs; h.Unavailable && !h.Known {
			continue // not asked again before its pause ends (confirmRange)
		}
		return t.recheck(lv, settled)
	}
	return nil
}

// nameHold names what keeps the head from moving above the cursor, of the
// reasons this tick has seen: a transfer waiting for its confirmation right
// above it, the two endpoints more than a settle depth apart, the primary's
// log index behind the chain. (What is not named here is named by Tick, from
// the heights the endpoints showed.)
func (t *tick) nameHold() {
	w := t.w
	st := &w.st
	depth := w.cfg.SettleDepth
	tip := t.chainTip()
	switch behind, ahead := t.head.Number-min(t.head.Number, t.verHead), t.verHead-min(t.verHead, t.head.Number); {
	case t.settleLimit <= st.Cursor+1:
		t.holdf("a transfer in block %d is waiting to be confirmed by another endpoint", t.settleLimit)
	case t.secondLogsBehind != "" && t.secondLogsTo <= st.Cursor:
		t.holdf("%s", t.secondLogsBehind)
	case t.independent && behind > depth:
		// Not an error — the head is settled on what both endpoints
		// have — but if it lasts, the operator should know why the
		// head does not move. (With a third endpoint the two would have
		// been told apart: resolveVerifier.)
		t.holdf("%s is at block %d, %d blocks behind %s; nothing is settled beyond what both have",
			t.ver.Label(), t.verHead, behind, t.primary.Label())
	case t.independent && ahead > depth:
		// The same disagreement the other way round: the primary's
		// chain ends below the verifier's. Nothing above it is scanned.
		t.holdf("%s is at block %d, %d blocks behind %s; nothing is scanned or settled beyond what both have",
			t.primary.Label(), t.head.Number, ahead, t.ver.Label())
	case t.ver != nil && tip-min(tip, depth) > st.Cursor && t.logHead < tip:
		// The chain the endpoints agree on has blocks to settle, and
		// the scan did not get to them: the backend that answers the
		// primary's log queries reports a height below the primary's
		// own head. A log index may trail its node by a few
		// blocks for a moment; one that stays behind hides every
		// payment above it, so after a few ticks the primary is set
		// aside and another endpoint's logs are used.
		t.logStalled = true
		t.holdf("the log index of %s is at block %d, %d blocks behind the chain (block %d): nothing above block %d can be scanned on it",
			t.primary.Label(), t.logHead, tip-t.logHead, tip, t.logHead)
	}
}

// logIndexVerdict keeps count of the ticks in which the primary's log index
// alone held the head (tick.logStalled), per endpoint, and sets the primary
// aside when it has done so holdAlertTicks times running. The count is the
// endpoint's: a tick with another primary in between does not clear it, a
// tick in which its log index was where it should be does. So an endpoint
// whose log index is stuck costs one tick each time its cooldown ends, not
// three.
func (t *tick) logIndexVerdict() {
	w := t.w
	label := t.primary.Label()
	if !t.logStalled {
		if w.stalledLog == label {
			w.stalledLog, w.stalledTicks = "", 0
		}
		return
	}
	if w.stalledLog != label {
		w.stalledLog, w.stalledTicks = label, 0
	}
	w.stalledTicks++
	if w.stalledTicks < holdAlertTicks {
		return
	}
	tip := t.chainTip()
	t.primary.ReportFault(fmt.Sprintf("its log index is at block %d, %d blocks behind its own head", t.logHead, t.head.Number-min(t.head.Number, t.logHead)))
	t.incident(IncidentLogIndexBehind, t.primary, t.logHead, fmt.Sprintf(
		"the height %s reports together with its eth_getLogs answers has stayed at block %d for %d ticks while the chain is at block %d; nothing above it could be scanned, so another endpoint's logs are used",
		t.primary.Label(), t.logHead, w.stalledTicks, tip))
}

// highestShown returns the highest chain height an endpoint has shown: in
// this tick, to the watcher, or — for endpoints the tick had no reason to
// ask — to the pool's monitor within the last minute. Endpoints the pool
// has set aside or cannot use (a failure cooldown, another chain) are not
// counted: what they say is not weighed anywhere else either.
func (t *tick) highestShown() (uint64, string) {
	top, by := t.topHeight, t.topBy
	for _, es := range t.w.cfg.Client.Status() {
		if es.State != evm.StateHealthy && es.State != evm.StateLagging {
			continue
		}
		if age := t.now.Sub(es.BlockSeenAt); es.BlockSeenAt.IsZero() || age > time.Minute || age < -time.Minute {
			continue
		}
		if es.Height > top {
			top, by = es.Height, es.URL
		}
	}
	return top, by
}

// advanceBelowWindow moves the cursor behind the scan while it is still a
// whole reorg window below the chain's head (catching up), so that progress
// survives a failure. No header is needed there — blocks that deep do not
// change — but the log scan is confirmed like any other.
func (t *tick) advanceBelowWindow() error {
	w := t.w
	st := &w.st
	if t.ver == nil {
		return nil
	}
	safe := t.safeHead()
	target := min(t.settleTarget(t.scannedTo), safe-min(safe, w.cfg.ReorgWindow))
	if target <= st.Cursor {
		return nil
	}
	confirmed, ok, err := t.confirmRange(st.Cursor+1, target)
	if err != nil || !ok {
		return err
	}
	if target = min(target, confirmed, t.settleTarget(t.scannedTo)); target > st.Cursor {
		t.setCursor(target)
	}
	return nil
}

// safeHead is the lowest chain height reported by the backends this tick
// relies on: the one that answered the log queries and, if there is an
// independent verifier, the verifier's own head. A head that only one
// endpoint claims does not move anything.
func (t *tick) safeHead() uint64 {
	return min(t.logHead, t.chainTip())
}

// settleTarget returns the highest block at or below `to` that may be
// settled.
func (t *tick) settleTarget(to uint64) uint64 {
	safe := t.safeHead()
	target := min(to, safe-min(safe, t.w.cfg.SettleDepth))
	if t.settleLimit != math.MaxUint64 {
		target = min(target, t.settleLimit-min(t.settleLimit, 1))
	}
	return target
}

// cursorHeader returns the header of the block the cursor rests on, as the
// primary and the verifier agree on it in this tick (invariant 2). If the
// cursor was not put there by settle in this tick — it rests where an
// earlier tick left it, or comes from the checkpoint, a rewind, a catch-up
// step or the configuration — the header is fetched from both and compared
// now. The boolean is false if there is no agreed header (yet), or no need
// for one: the sink already has the head and there is nothing to add.
//
// "In this tick" matters. The agreement of an earlier tick says what the
// chain was then. A head that could not be delivered at the time (the sink
// failed), or that is reported again because finality moved on, would
// otherwise be reported on the strength of it — after a reorganisation that
// replaced the block, if the primary keeps quiet about that reorganisation
// and the verifier, which contradicts it, has been set aside for this tick:
// one endpoint's word, and a head that is not a block of the chain. So the
// remembered header is good for one thing only: what the endpoints agree on
// now must be the same block.
func (t *tick) cursorHeader() (blockRef, bool, error) {
	w := t.w
	st := &w.st
	if st.Cursor == 0 {
		return blockRef{}, false, nil
	}
	known := w.verifiedHead
	remembered := known.Number == st.Cursor && !known.Hash.IsZero()
	if remembered && t.headAgreed {
		return known, true, nil
	}
	if sent := w.headSent; remembered && sent.Number == known.Number && sent.Hash == known.Hash && sent.Finalized >= st.Finalized.Number && sent.Checked.Equal(w.checked()) {
		// The sink has this very head, and nothing about it has changed:
		// there is nothing to report, so nothing to ask the endpoints for.
		// (A tick in which the chain has not moved costs no more than it
		// did before.)
		return blockRef{}, false, nil
	}
	h, ok, err := t.agreedHeader(st.Cursor)
	if err != nil || !ok {
		return blockRef{}, false, err
	}
	a, anchored := st.anchorAt(st.Cursor)
	if (anchored && a.Hash != h.Hash) || (remembered && known.Hash != h.Hash) {
		// The watcher's idea of the cursor block is not the chain's (any
		// more). checkChain deals with that on the next tick; until then
		// nothing is reported.
		t.holdf("the remembered block %d is not the chain's any more", st.Cursor)
		return blockRef{}, false, nil
	}
	ref := blockRef{Number: h.Number, Hash: h.Hash, Time: h.Time}
	w.verifiedHead = ref
	t.headAgreed = true
	if a, ok := st.newestAnchor(); !ok || a.Number < ref.Number {
		// The cursor rests on a block that nothing the watcher remembers
		// covers: it was not settled onto it (settle remembers the block it
		// settles on) but put there — the start position of a watcher
		// without a checkpoint, a checkpoint moved up to where scanning has
		// to go on (skipUnneeded), a catch-up step. From here on everything
		// at or below this block counts as dealt with, so this block is the
		// one that has to be found missing if the chain is reorganised
		// below it: it is remembered like a block that was settled on. (A
		// header fetched after a scan vouches for less than one fetched
		// before it — see settle — but nothing at or below this block was
		// scanned in this tick on the strength of it; and the next block
		// that is settled supersedes it.)
		st.addAnchor(ref, w.cfg.ReorgWindow, max(w.cfg.ReorgWindow/64, 1))
		t.dirty = true
	}
	return ref, true, nil
}

// checked returns Head.Checked.
func (w *Watcher) checked() time.Time {
	since, ok := w.st.uncheckedSince()
	if !ok || len(w.cfg.Client.Endpoints()) < 2 {
		// Nothing waits for a second endpoint's scan; or there is nobody
		// to make it, and nothing is owed to it.
		return time.Time{}
	}
	return time.Unix(int64(min(since, math.MaxInt64/2)), 0).UTC()
}

// reportHead tells the sink the processed head — the block the cursor rests
// on — if it differs from the one reported last.
func (t *tick) reportHead(ref blockRef) error {
	w := t.w
	fact := Head{
		Number:    ref.Number,
		Hash:      ref.Hash,
		Time:      time.Unix(int64(min(ref.Time, math.MaxInt64/2)), 0).UTC(),
		Finalized: w.st.Finalized.Number,
	}
	final, known := t.final(fact.Number)
	if !known {
		if floor := w.cfg.FinalizedFloor; floor > w.st.Finalized.Number {
			t.holdf("waiting for two endpoints to show block %d, which the consumer holds as final", floor)
		} else {
			t.holdf("waiting for the endpoints to agree on a finalized block: %s", cmp.Or(t.finalityDispute, "they have not reported the same one yet"))
		}
		return nil
	}
	fact.Final = final
	switch {
	case final == fact.Number:
		fact.FinalTime = fact.Time
	case final == w.st.Finalized.Number && w.st.Finalized.Time != 0:
		fact.FinalTime = time.Unix(int64(min(w.st.Finalized.Time, math.MaxInt64/2)), 0).UTC()
	}
	fact.Checked = w.checked()
	if sent := w.headSent; sent.Number == fact.Number && sent.Hash == fact.Hash && sent.Finalized >= fact.Finalized && sent.Final == fact.Final && sent.FinalTime.Equal(fact.FinalTime) && sent.Checked.Equal(fact.Checked) {
		return nil
	}
	if err := w.cfg.Sink.HeadAdvanced(t.ctx, fact); err != nil {
		return fmt.Errorf("watcher: sink rejected head %d: %w", fact.Number, err)
	}
	w.headSent = fact
	return nil
}

// startHeader returns the canonical header at number as the primary saw it
// at the start of the tick, fetching it if it was not part of the head
// request. The boolean is false if the endpoint has no such block.
func (t *tick) startHeader(number uint64) (evm.Header, bool, error) {
	if h, ok := t.startHeaders[number]; ok {
		return h, true, nil
	}
	h, err := t.primary.HeaderByNumber(t.ctx, number)
	if errors.Is(err, evm.ErrNotFound) {
		return evm.Header{}, false, nil
	}
	if err != nil {
		return evm.Header{}, false, fmt.Errorf("watcher: fetch header %d: %w", number, err)
	}
	t.startHeaders[number] = *h
	return *h, true, nil
}

// checkChain compares the primary's chain with the blocks the watcher
// remembers and handles reorgs. It returns an error if the endpoint must not
// be used for this tick.
func (t *tick) checkChain() error {
	w := t.w
	st := &w.st

	// Finalized anchor: a block the chain declared final.
	finConflict := false
	if f := st.Finalized; f.Number != 0 {
		if f.Number > t.head.Number {
			return fmt.Errorf("watcher: endpoint %s is at block %d, behind the finalized block %d already seen",
				t.primary.Label(), t.head.Number, f.Number)
		}
		h, known, err := t.startHeader(f.Number)
		if err != nil {
			return err
		}
		if !known {
			return t.fault(fmt.Sprintf("does not know block %d, below its own head %d", f.Number, t.head.Number))
		}
		finConflict = h.Hash != f.Hash
	}

	// Anchors, newest first: find the newest one that is still canonical.
	canonical, lowestBad := -1, -1
	for i := len(st.Anchors) - 1; i >= 0; i-- {
		a := st.Anchors[i]
		if a.Number > t.head.Number {
			continue // the endpoint has not got that far; nothing to compare
		}
		h, known, err := t.startHeader(a.Number)
		if err != nil {
			return err
		}
		if !known {
			return t.fault(fmt.Sprintf("does not know block %d, below its own head %d", a.Number, t.head.Number))
		}
		if h.Hash == a.Hash {
			canonical = i
			break
		}
		lowestBad = i
	}
	// Every remembered block that can be compared is gone, but the
	// finalized block stands, below all of them and less than a reorg
	// window under the newest: the reorganisation forked above a final
	// block, inside the window — an ordinary one. (A watcher that started
	// without its checkpoint remembers a window's worth when the two
	// endpoints could show it the block a window down — see prepare; if
	// they could not, it remembers little more than the block it started
	// on. One that remembers a whole window has a block at or below the
	// finalized one among them, gone only if the finalized block is.) The
	// finalized block is then where the reorganisation ends, and the
	// verifier is asked about it like about any other.
	if f := st.Finalized; canonical == -1 && lowestBad != -1 && !finConflict && f.Number != 0 &&
		f.Number < st.Anchors[0].Number && f.Number+w.cfg.ReorgWindow >= st.Anchors[len(st.Anchors)-1].Number {
		st.Anchors = append([]blockRef{f}, st.Anchors...)
		canonical, lowestBad = 0, lowestBad+1
	}

	// The primary's word that a remembered block is still there is not
	// enough either: a reorganisation deeper than the settle depth that the
	// primary keeps quiet about would leave transfers of blocks that are
	// gone on the books. The verifier was asked for the newest anchor too.
	//
	// The same goes for where a reorganisation ends. If the primary reports
	// one — remembered blocks replaced, down to one that is still there —
	// everything above that block is scanned again and everything at or
	// below it is taken to be unchanged. That block is the newest anchor only
	// when nothing was replaced, so the verifier has not been asked about it
	// yet: it is asked now. A primary that admits to a reorganisation but
	// not to its depth would otherwise have the blocks in between passed
	// over, with whatever was paid in them.
	if canonical >= 0 && t.independent {
		a := st.Anchors[canonical]
		vh, asked := t.verHeaders[a.Number]
		if !asked && lowestBad != -1 {
			p, err := t.ver.HeaderByNumber(t.ctx, a.Number)
			if cerr := t.ctx.Err(); cerr != nil {
				return cerr
			}
			if err != nil {
				// It cannot say. Then it cannot vouch for anything that
				// rests on that block either: nothing is settled with it
				// in this tick.
				t.verErr = fmt.Errorf("%s could not confirm block %d, above which %s reports a reorganisation: %v",
					t.ver.Label(), a.Number, t.primary.Label(), err)
				t.ver, t.independent = nil, false
			} else {
				vh, asked = *p, true
				t.verHeaders[a.Number] = vh
			}
		}
		if asked && vh.Hash != a.Hash {
			// A third endpoint decides, if there is one (and if it sides
			// with the verifier, this tick ends here). Either way this
			// verifier's view is not the one the watcher settled on:
			// nothing is settled with it in this tick.
			if err := t.disagree(a.Number, t.startHeaders[a.Number], vh); err != nil {
				return err
			}
			t.verErr = fmt.Errorf("%s has another block %d than the one the watcher settled on", t.ver.Label(), a.Number)
			t.ver, t.independent = nil, false
		}
	}

	windowLost := lowestBad != -1 && canonical == -1
	if finConflict || windowLost {
		// The endpoint contradicts a finalized block, or nothing we
		// remember survives on its chain. That is either a consensus
		// failure or — far more likely — an endpoint on a stale or foreign
		// fork. Do not take one endpoint's word for it.
		ref, reason := st.Finalized, "finalized block"
		if !finConflict {
			ref, reason = st.Anchors[len(st.Anchors)-1], "every remembered block of the reorg window"
			if f := st.Finalized; f.Number != 0 {
				reason += fmt.Sprintf(" (the finalized block %d stands)", f.Number)
			}
			for i := len(st.Anchors) - 1; i >= 0; i-- {
				if st.Anchors[i].Number <= t.head.Number {
					ref = st.Anchors[i]
					break
				}
			}
		}
		if err := t.confirmConflict(ref, reason); err != nil {
			return err
		}
		// Accepted. Re-scan from the newest surviving anchor, or from a
		// full window below everything we remember.
		rescanFrom := uint64(0)
		if canonical >= 0 {
			rescanFrom = st.Anchors[canonical].Number
			st.Anchors = st.Anchors[:canonical+1]
		} else {
			lowest := st.Cursor
			if len(st.Anchors) > 0 {
				lowest = min(lowest, st.Anchors[0].Number)
			}
			if st.Finalized.Number != 0 {
				lowest = min(lowest, st.Finalized.Number)
			}
			rescanFrom = lowest - min(lowest, w.cfg.ReorgWindow+1)
			st.Anchors = nil
		}
		if finConflict {
			// The finalized block itself is contradicted, and with it
			// the consumer's floor: what is finalized now is for the
			// endpoints to attest again. (When only the remembered
			// window is lost, the finalized block is still there, and
			// stays final.)
			st.Finalized = blockRef{}
			w.cfg.FinalizedFloor = 0
		}
		if rescanFrom < st.Cursor {
			// A re-scan that must happen whatever the consumer has
			// processed: its facts are what is in question.
			st.RescanTo = max(st.RescanTo, st.Cursor)
			t.setCursor(rescanFrom)
		}
		t.dirty = true
		w.conflictStreak = 0
		w.bumpDeepReorgs()
		if finConflict {
			w.log.Error("chain watcher accepted a chain that contradicts finalized history; transfers older than the reorg window cannot be re-examined",
				"endpoint", t.primary.Label(), "contradicted", reason, "block", ref.Number, "rescan_from", st.Cursor+1)
		} else {
			// Not a contradiction of anything final: every block the
			// watcher remembers was replaced, and how deep the
			// reorganisation went is not known — up to the whole window.
			w.log.Error("chain watcher accepted a chain that replaced every block it remembers of the reorg window; the reorganisation may go deeper than the window, and transfers older than it cannot be re-examined",
				"endpoint", t.primary.Label(), "replaced", reason, "block", ref.Number, "finalized", st.Finalized.Number, "rescan_from", st.Cursor+1)
		}
		if w.cfg.OnDeepReorg != nil {
			why := "endpoints contradict " + reason
			if !finConflict {
				why = "endpoints replaced " + reason
			}
			w.cfg.OnDeepReorg(DeepReorg{Endpoint: t.primary.Label(), RescanFrom: st.Cursor + 1, Reason: why})
		}
		return nil
	}
	w.conflictStreak = 0

	if lowestBad != -1 {
		// An ordinary reorg above the fork point st.Anchors[canonical].
		forkBase := st.Anchors[canonical].Number
		depth := st.Anchors[len(st.Anchors)-1].Number - forkBase
		st.Anchors = st.Anchors[:canonical+1]
		if forkBase < st.Cursor {
			t.setCursor(forkBase)
		}
		t.dirty = true
		t.reverted = true // counted as a reorganisation, once per tick
		w.log.Warn("chain reorganisation detected", "common_ancestor_at_or_above", forkBase, "max_depth", depth,
			"head", t.head.Number, "rescan_from", st.Cursor+1)
	}
	return nil
}

// confirmConflict decides whether the primary's contradiction of ref (a block
// the watcher holds to be final or at least long settled) is to be believed.
// It returns nil if it is, and an error if the tick must be abandoned.
func (t *tick) confirmConflict(ref blockRef, reason string) error {
	w := t.w
	theirs, _, err := t.startHeader(ref.Number) // the primary's block at that height
	if err != nil {
		return err
	}
	ver, independent, verr := t.verifier()
	if verr != nil {
		if cerr := t.ctx.Err(); cerr != nil {
			return cerr
		}
		t.primary.ReportFault("contradicts " + reason + "; no second endpoint available to confirm")
		return fmt.Errorf("%w: %s contradicts %s (block %d) and no second endpoint could confirm it: %v",
			ErrInconsistentEndpoint, t.primary.Label(), reason, ref.Number, verr)
	}
	if independent {
		vh, err := ver.HeaderByNumber(t.ctx, ref.Number)
		switch {
		case errors.Is(err, evm.ErrNotFound):
			return fmt.Errorf("%w: %s contradicts %s (block %d); %s cannot confirm it yet",
				ErrInconsistentEndpoint, t.primary.Label(), reason, ref.Number, ver.Label())
		case err != nil:
			if cerr := t.ctx.Err(); cerr != nil {
				return cerr
			}
			return fmt.Errorf("%w: %s contradicts %s (block %d); asking %s failed: %v",
				ErrInconsistentEndpoint, t.primary.Label(), reason, ref.Number, ver.Label(), err)
		case vh.Hash == ref.Hash:
			// The second endpoint sides with our record: the primary is
			// on another fork.
			t.primary.ReportFault("is on a fork that contradicts " + reason)
			return fmt.Errorf("%w: %s is on a fork that contradicts %s (block %d); %s confirms the known chain",
				ErrInconsistentEndpoint, t.primary.Label(), reason, ref.Number, ver.Label())
		case vh.Hash == theirs.Hash:
			return nil // two endpoints agree against our record
		default:
			return fmt.Errorf("%w: %s and %s both contradict %s (block %d) and each other",
				ErrInconsistentEndpoint, t.primary.Label(), ver.Label(), reason, ref.Number)
		}
	}
	// A single source. Believe it only if it insists for a while: a
	// load-balanced endpoint with one stale backend flips back and forth,
	// a real change of history stays. It has to be the same source all the
	// while (under best effort another endpoint may be the one left).
	if label := t.primary.Label(); w.conflictBy != label {
		w.conflictBy, w.conflictStreak = label, 0
	}
	w.conflictStreak++
	if w.conflictStreak < w.cfg.ConflictPatience {
		t.primary.ReportFault("contradicts " + reason)
		return fmt.Errorf("%w: %s contradicts %s (block %d); waiting for it to persist (%d/%d)",
			ErrInconsistentEndpoint, t.primary.Label(), reason, ref.Number, w.conflictStreak, w.cfg.ConflictPatience)
	}
	return nil
}

// fault marks the primary as faulty and returns the corresponding error.
func (t *tick) fault(what string) error {
	t.primary.ReportFault(what)
	return fmt.Errorf("%w: %s %s", ErrInconsistentEndpoint, t.primary.Label(), what)
}

func (w *Watcher) bumpDeepReorgs() {
	w.mu.Lock()
	w.status.DeepReorgs++
	w.mu.Unlock()
}

// verifier returns the endpoint that cross-checks the primary in this tick,
// and whether it is a different endpoint. An error means the configured mode
// demands an independent endpoint and none can be had right now.
func (t *tick) verifier() (*evm.Endpoint, bool, error) {
	if t.ver == nil {
		err := t.verErr
		if err == nil {
			err = evm.ErrNoEndpoint
		}
		return nil, false, err
	}
	return t.ver, t.independent, nil
}

// scan covers the blocks cursor+1..head for the addresses of this tick.
func (t *tick) scan() error {
	w := t.w
	st := &w.st
	set := t.set
	recipients := make([]ethaddr.Address, 0, len(set))
	for a := range set {
		recipients = append(recipients, a)
	}
	t.recipients = recipients

	// How far to look. The primary's head is only its claim: with an
	// independent verifier, nothing is scanned further than two settle
	// depths above the verifier's own head, so that an endpoint inventing
	// a chain millions of blocks long cannot keep the watcher busy with
	// blocks that do not exist.
	scanTo := t.head.Number
	if t.independent {
		scanTo = min(scanTo, t.verHead+2*w.cfg.SettleDepth)
	}
	if t.ver == nil {
		// Nothing can be reported or settled without a second endpoint.
		// Look far enough to show what is waiting, no further: all of it
		// will have to be scanned again.
		t.holdf("no second endpoint available to confirm anything: %v", t.verErr)
		scanTo = min(scanTo, st.Cursor+w.cfg.ReorgWindow)
	}

	for cur := st.Cursor + 1; cur <= scanTo; {
		end := scanTo
		next := end + 1 // first block not covered by this step
		var found []evm.Transfer
		var scanErr error
		var res evm.TransferScan
		if len(recipients) > 0 {
			// One provider-sized step at a time, so that progress is
			// saved between requests.
			if span := max(t.primary.LogRange(), 1); span-1 < end-cur {
				end = cur + span - 1
			}
			var err error
			res, err = t.primary.TransferLogs(t.ctx, evm.TransferQuery{
				Token: w.cfg.Token, FromBlock: cur, ToBlock: end, Recipients: recipients,
			})
			found, next, scanErr = res.Transfers, res.Next, err
		}
		if len(recipients) > 0 && scanErr == nil {
			t.noteLogServer(t.primary)
		}
		if res.BackendHead != 0 {
			t.sawHeight(t.primary, res.BackendHead)
		}
		if res.BackendHead != 0 && res.BackendHead < t.logHead {
			// The backend answering log queries is behind the head. The
			// settle depth exists to give a log index time to catch up
			// with its own node, so it has to be measured from there.
			t.logHead = res.BackendHead
		}
		if next > cur {
			// Whatever prefix was covered is processed and saved, even
			// if the scan then failed.
			if err := t.step(cur, next-1, found, set); err != nil {
				return err
			}
			t.scannedTo = next - 1
			// Far below the head (catching up), the cursor follows the
			// scan step by step, so that progress survives a failure.
			// Near the head it is settled once, after the scan.
			if err := t.advanceBelowWindow(); err != nil {
				return err
			}
			if t.dirty {
				if err := w.save(t.ctx); err != nil {
					return err
				}
				t.dirty = false
			}
		}
		if scanErr != nil {
			if evm.IsKind(scanErr, evm.KindLagging) {
				// The backend answering the primary's log queries is below
				// the blocks that are to be scanned, although the primary's
				// own head is not. Once, that is a backend catching up; when
				// it keeps happening the endpoint is set aside like one
				// whose log index is stuck just above the cursor (settle).
				t.logStalled = true
				t.logIndexVerdict()
			}
			return fmt.Errorf("watcher: scan blocks %d..%d: %w", next, end, scanErr)
		}
		if next <= cur {
			// Cannot happen (a scan without error covers at least one
			// block); guards the loop against ever spinning in place.
			return fmt.Errorf("watcher: scan of blocks %d..%d made no progress", cur, end)
		}
		if next <= end || end == scanTo {
			// Done — or the backend answering log queries has not reached
			// the head yet, and the next tick continues from there.
			break
		}
		cur = end + 1
		if err := w.cfg.Sleep(t.ctx, w.cfg.CatchUpPause); err != nil {
			return err
		}
	}
	return nil
}

// step processes the complete scan result for blocks from..to: diff, verify,
// record, deliver.
func (t *tick) step(from, to uint64, found []evm.Transfer, set map[ethaddr.Address]struct{}) error {
	w := t.w
	st := &w.st

	// What the chain has now, reduced to what counts as a payment-relevant
	// transfer.
	scanned := make(map[transferKey]Transfer, len(found))
	order := make([]transferKey, 0, len(found))
	for _, f := range found {
		if !t.relevant(f, from, to, set) {
			continue
		}
		tr := Transfer{
			Token: f.Token, From: f.From, To: f.To, Amount: f.Amount,
			TxHash: f.TxHash, LogIndex: f.LogIndex, BlockNumber: f.BlockNumber, BlockHash: f.BlockHash,
		}
		k := keyOf(tr)
		if _, dup := scanned[k]; dup {
			continue
		}
		// The dust threshold is policy for new observations only. A
		// transfer that was already reported must stay visible even if
		// the threshold was raised since, or it would look reverted.
		if _, known := st.Tracked[k]; !known && w.cfg.MinAmount != nil && f.Amount.Cmp(w.cfg.MinAmount) < 0 {
			continue
		}
		scanned[k] = tr
		order = append(order, k)
	}

	// Reported earlier, not there now.
	var gone []*tracked
	for k, tr := range st.Tracked {
		if tr.BlockNumber < from || tr.BlockNumber > to {
			continue
		}
		if _, still := scanned[k]; !still {
			gone = append(gone, tr)
		}
	}
	sort.Slice(gone, func(i, j int) bool { return lessTransfer(gone[i].Transfer, gone[j].Transfer) })

	// There now, not reported yet — or not yet in this process: what the
	// checkpoint remembers is reported again after a restart, but not on
	// the strength of having been confirmed once. It needs a receipt now,
	// like a new transfer (invariant 1); without one it waits, and is
	// attested or taken back when its block is settled.
	var fresh, again []Transfer
	for _, k := range order {
		switch tr, known := st.Tracked[k]; {
		case !known:
			fresh = append(fresh, scanned[k])
		case !tr.delivered:
			again = append(again, tr.Transfer)
		}
	}
	var observe []Transfer
	var stale []*tracked
	if len(again) > 0 {
		confirmed, _, err := t.receipts(again, t.primary)
		if err != nil {
			return err
		}
		deep := t.safeHead()
		deep -= min(deep, w.cfg.SettleDepth)
		for i, tr := range again {
			if confirmed[i] {
				observe = append(observe, tr)
				continue
			}
			if t.independent && tr.BlockNumber <= deep {
				h, agreed, err := t.agreedHeader(tr.BlockNumber)
				if err != nil {
					return err
				}
				if agreed && h.Hash != tr.BlockHash {
					// Its block is not the one the endpoints agree on:
					// reorganised away while the watcher was not looking.
					stale = append(stale, st.Tracked[keyOf(tr)])
					continue
				}
			}
			if t.doubted == nil {
				t.doubted = make(map[transferKey]uint64)
			}
			t.doubted[keyOf(tr)] = tr.BlockNumber
			t.hold(tr.BlockNumber)
		}
		if len(stale) > 0 {
			t.primary.ReportFault(fmt.Sprintf("its logs show a transfer in a block at height %d that is not on the chain", stale[0].BlockNumber))
			t.incident(IncidentStaleLog, t.primary, stale[0].BlockNumber, fmt.Sprintf(
				"%s still returns the log of transfer %s#%d from block %s at height %d, which is not on the chain any more; the transfer is reported as reverted",
				t.primary.Label(), stale[0].TxHash, stale[0].LogIndex, stale[0].BlockHash, stale[0].BlockNumber))
		}
	}

	// Reverts need proof; observations need confirmation.
	var reverts []*tracked
	for _, g := range gone {
		proven, err := t.proveReverted(g)
		if err != nil {
			return err
		}
		if proven {
			reverts = append(reverts, g)
		} else {
			t.hold(g.BlockNumber)
		}
	}
	reverts = append(reverts, stale...)
	admitted, err := t.admit(fresh, t.primary)
	if err != nil {
		return err
	}
	observe = append(observe, admitted...)
	return t.deliver(reverts, observe)
}

// deliver reports reverts and observations to the sink, in that order.
func (t *tick) deliver(reverts []*tracked, observe []Transfer) error {
	w := t.w
	st := &w.st
	ctx := t.ctx
	sort.Slice(observe, func(i, j int) bool { return lessTransfer(observe[i], observe[j]) })

	// Every transfer is reported with the timestamp of its block, and the
	// timestamp is a fact like the transfer itself: two endpoints have to
	// agree on it. A transfer whose block cannot be dated yet waits.
	observe, err := t.stamp(observe)
	if err != nil {
		return err
	}

	// Write-ahead: record the transfers that are about to be reported
	// before reporting them. Otherwise a crash between the sink accepting a
	// transfer and the next checkpoint would leave the sink with a fact the
	// watcher has no memory of — and if that block were then reorganised
	// away, nobody would ever report the revert.
	var intents []transferKey
	uncontested := observe[:0:0]
	for _, o := range observe {
		k := keyOf(o)
		if st.Tracked[k] == nil {
			if other := st.occupant(o); other != nil {
				t.contested(o, other)
				continue
			}
			st.Tracked[k] = &tracked{Transfer: o, timed: true} // stamped a moment ago
			intents = append(intents, k)
		}
		uncontested = append(uncontested, o)
	}
	observe = uncontested
	if len(intents) > 0 {
		if err := w.save(ctx); err != nil {
			// Not recorded, so not to be reported: forget the intent
			// again, or a later tick would take it for a recorded one.
			for _, k := range intents {
				delete(st.Tracked, k)
			}
			return err
		}
		t.dirty = false
	}

	// Deliver. State changes follow each accepted fact, so a sink error
	// part-way leaves exactly the undelivered facts to be retried.
	for _, r := range reverts {
		if err := w.cfg.Sink.TransferReverted(ctx, r.Transfer); err != nil {
			return fmt.Errorf("watcher: sink rejected revert of %s#%d: %w", r.TxHash, r.LogIndex, err)
		}
		delete(st.Tracked, keyOf(r.Transfer))
		t.dirty = true
		t.reverted = true
		w.log.Warn("transfer reverted by reorganisation", "tx", r.TxHash.String(), "log_index", r.LogIndex,
			"block", r.BlockNumber, "block_hash", r.BlockHash.String(), "to", r.To.String())
	}
	for _, o := range observe {
		if err := w.cfg.Sink.TransferObserved(ctx, o); err != nil {
			return fmt.Errorf("watcher: sink rejected transfer %s#%d: %w", o.TxHash, o.LogIndex, err)
		}
		st.Tracked[keyOf(o)].delivered = true
		t.observed++
		w.log.Info("transfer observed", "tx", o.TxHash.String(), "log_index", o.LogIndex, "block", o.BlockNumber,
			"block_hash", o.BlockHash.String(), "to", o.To.String(), "amount", o.Amount.String())
	}
	return nil
}

// contested deals with a transfer that passed verification although the
// watcher tracks another transaction's transfer at the same log index of the
// same block. A block has one log per index, so one of the two is false — and
// both were confirmed by a receipt, which takes an endpoint that contradicts
// itself over time, or two that lie (outside what the trust model covers, but
// not outside what can happen). The watcher does not pick one: the new
// transfer is not reported and not recorded, and it holds the head below its
// block like any other transfer in doubt, until the endpoints make up their
// minds. The one already reported stays what it is — reported — until a scan
// shows its block gone.
func (t *tick) contested(o Transfer, other *tracked) {
	if t.doubted == nil {
		t.doubted = make(map[transferKey]uint64)
	}
	t.doubted[keyOf(o)] = o.BlockNumber
	t.hold(o.BlockNumber)
	detail := fmt.Sprintf("transfers %s and %s both claim log index %d of block %s (height %d); the first was reported, the second is not, and the head waits below that block",
		other.TxHash, o.TxHash, o.LogIndex, o.BlockHash, o.BlockNumber)
	t.holdf("%s", detail)
	t.incident(IncidentLogMismatch, nil, o.BlockNumber, detail)
}

// hold keeps the cursor below block.
func (t *tick) hold(block uint64) {
	if block < t.settleLimit {
		t.settleLimit = block
	}
}

// relevant reports whether a transfer returned by the scan is one the
// watcher reports. The evm package already guarantees the contract, the
// range and the recipient filter; they are checked again here because this
// is the line of defence the rest of the system relies on.
func (t *tick) relevant(f evm.Transfer, from, to uint64, set map[ethaddr.Address]struct{}) bool {
	cfg := &t.w.cfg
	if f.Token != cfg.Token {
		return false // another contract's event, however similar
	}
	if f.BlockNumber < from || f.BlockNumber > to || f.BlockHash.IsZero() {
		return false
	}
	if _, watched := set[f.To]; !watched {
		return false
	}
	if f.Amount == nil || f.Amount.Sign() <= 0 {
		return false // zero-value transfers move nothing (address poisoning)
	}
	if f.From == f.To {
		return false // a self-transfer does not change the balance
	}
	return true
}

// lessTransfer orders transfers for delivery: see compareTransfers.
func lessTransfer(a, b Transfer) bool { return compareTransfers(a, b) < 0 }

// receiptMismatch returns "" if the receipt confirms the transfer, or the
// reason why it does not.
func receiptMismatch(r *evm.Receipt, f Transfer) string {
	switch {
	case r == nil:
		return "transaction not found"
	case r.Status != 1:
		return "transaction failed"
	case r.BlockHash != f.BlockHash || r.BlockNumber != f.BlockNumber:
		return "transaction is in a different block"
	}
	for _, l := range r.Logs {
		if l.LogIndex != f.LogIndex {
			continue
		}
		d, err := evm.DecodeTransfer(l)
		if err != nil {
			return "log at that index is not a transfer"
		}
		if d.Token != f.Token || d.From != f.From || d.To != f.To || d.Amount.Cmp(f.Amount) != 0 {
			return "log at that index differs"
		}
		return ""
	}
	return "no log at that index"
}
