package watcher

import "fmt"

// work says what keeps the watcher from idling after this tick, or "" if
// nothing does: see "Idle" in the package documentation, which lists the
// conditions in the words of the contract. err is the tick's error. A tick
// that leaves nothing to do leaves the watcher idle, or — if its watch set
// held closed addresses — quiet: the conditions are the same for both.
//
// The watcher's own business comes first, the watch set last, so that the
// reason given is the one an operator would not have guessed. Several of the
// conditions imply each other (a tick without a verifier names a hold, a
// counter that runs belongs to a tick that failed or held the head); each is
// asked on its own all the same, because the price of being wrong here is not
// a wasted tick but a payment that is looked for ten times later.
func (t *tick) work(err error) string {
	w := t.w
	st := &w.st
	depth := w.cfg.SettleDepth
	switch {
	case err != nil:
		return "the last tick failed"
	case !t.started || !st.Initialized:
		return "the watcher has not started yet"
	case t.ver == nil:
		return "no second endpoint confirmed the chain in the last tick"
	case t.holding != "":
		return "the processed head is waiting: " + t.holding
	case len(t.doubted) > 0:
		return fmt.Sprintf("%d transfer(s) are waiting to be confirmed by another endpoint", len(t.doubted))
	case w.solo || t.solo:
		return "single-source operation: every other endpoint is unreachable"
	case w.soloStreak > 0:
		return fmt.Sprintf("every other endpoint has been unreachable for %d tick(s)", w.soloStreak)
	case w.heldTicks > 0 || t.logStalled || w.aheadStreak > 0 || w.conflictStreak > 0:
		// (Not Watcher.stalledTicks: that count is an endpoint's, and
		// stands until the endpoint is the primary again — which may be
		// never, while another one is preferred.)
		return "a disagreement with the endpoints is being resolved"
	case t.voted:
		return "the endpoints disagree about where the chain ends"
	case t.reverted:
		return "a reorganisation was dealt with in the last tick"
	case st.RescanTo > st.Cursor:
		return fmt.Sprintf("a re-scan is under way: block %d of %d", st.Cursor, st.RescanTo)
	}
	if t.independent {
		if spread := max(t.head.Number, t.verHead) - min(t.head.Number, t.verHead); spread > depth {
			return fmt.Sprintf("the endpoints are %d blocks apart about where the chain ends", spread)
		}
	}
	if from, to, left := t.uncheckedLeft(); left && w.cfg.Client.Len() > 1 {
		// (With a single endpoint configured there is no second one that
		// could scan them: the record stands for the day an endpoint is
		// added — which builds a new watcher — and waits for nothing this
		// watcher can do. It would otherwise keep a single-endpoint
		// instance from ever resting while an address is watched.)
		return fmt.Sprintf("blocks %d..%d were settled on one endpoint's logs and are still to be scanned by a second one (%s)", from, to, t.w.uncheckedWaiting())
	}
	// Where the chain ends is not for the endpoints of this tick to say
	// alone when it comes to resting on it, any more than when it comes to
	// holding the head in silence (Tick, invariant 4): the highest chain any
	// endpoint has shown — to this tick, in whatever answer, or to the
	// pool's monitor within the last minute — counts.
	top, by := t.highestShown()
	switch tip := t.chainTip(); {
	case st.Cursor+depth < tip:
		return fmt.Sprintf("catching up: the cursor is at block %d, the chain at block %d", st.Cursor, tip)
	case top > st.Cursor+2*depth:
		// One endpoint's claim, and nothing is settled on it. But a watcher
		// that rests is one that has satisfied itself that there is nothing
		// above its cursor to look at — and here somebody says there is.
		// (An endpoint that is merely ahead by the blocks of a slow tick
		// costs one poll interval; one that keeps claiming a longer chain
		// keeps the watcher at its poll interval, and this says who.)
		return fmt.Sprintf("%s shows the chain at block %d, more than two settle depths above the cursor (block %d)", by, top, st.Cursor)
	case st.Cursor == 0:
		// A chain shorter than the settle depth: no block is deep enough to
		// be the processed head yet, and the consumer is waiting for its
		// first one.
		return "nothing has been settled yet"
	case w.headSent.Number != st.Cursor:
		return fmt.Sprintf("the processed head (block %d) has not been reported yet", st.Cursor)
	case t.consumerBehind != 0:
		return fmt.Sprintf("the consumer's processed head is at block %d, behind the head it was given", t.consumerBehind)
	case len(st.Tracked) > 0:
		return fmt.Sprintf("%d reported transfer(s) are still inside the reorg window", len(st.Tracked))
	case t.pendingListed > 0:
		return fmt.Sprintf("the consumer holds %d transfer(s) that are not final yet", t.pendingListed)
	case t.observed > 0:
		// Reported, and out of the reorg window already (the pause before
		// this tick was longer than the window). What the consumer makes of
		// it — final at once, or pending — it says in its next snapshot,
		// and that one is asked for a poll interval from now.
		return fmt.Sprintf("%d transfer(s) were reported in the last tick", t.observed)
	case t.open == t.watched && t.watched > 0:
		return fmt.Sprintf("%d address(es) are watched", t.watched)
	case t.open > 0:
		return fmt.Sprintf("%d of the %d watched address(es) are not closed", t.open, t.watched)
	}
	// Nothing is watched, or closed addresses only: at rest — idle, or
	// quiet (package documentation, "Quiet").
	return ""
}

// uncheckedLeft reports whether blocks that were settled on one endpoint's
// logs are still to be scanned by a second one, and which: the recorded range
// (state.Unchecked), as far as an address that is still
// watched can have been paid in it. It is the rule recheck applies — and with
// it the answer for an empty watch set is no, whatever the record says:
// nobody is left who could have been paid in those blocks, and an address
// that is created later cannot have been paid below the cursor it is created
// at. (The record itself is closed, as before, by the next tick in which a
// second endpoint confirms a log scan.)
func (t *tick) uncheckedLeft() (from, to uint64, left bool) {
	st := &t.w.st
	from, to = st.uncheckedBounds()
	if from == 0 || t.watched == 0 {
		return 0, 0, false
	}
	if t.sinceKnown {
		from = max(from, t.minSince+1)
	}
	return from, to, from <= to
}
