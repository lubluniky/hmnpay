package watcher

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

// This file holds the cross-checks: everything that keeps a single endpoint
// from deciding what the watcher reports. See "Trust model" in the package
// documentation.

// ErrFutureBlock is returned (wrapped) by Tick when the chain's newest block
// carries a timestamp further ahead of the local clock than
// Config.MaxFutureDrift, according to every endpoint asked. No honest block
// is from the future, so the local clock is behind (or every endpoint lies);
// nothing is settled until that is resolved.
var ErrFutureBlock = errors.New("watcher: block timestamps are ahead of the local clock")

// IncidentKind classifies an Incident.
type IncidentKind string

// Incident kinds.
const (
	// IncidentHeaderMismatch: two endpoints returned different headers
	// (hash, parent or timestamp) for the same block number, deep enough
	// below the tip that it is not an ordinary reorganisation in progress.
	// The processed head waits until they agree.
	IncidentHeaderMismatch IncidentKind = "header_mismatch"
	// IncidentFutureBlock: an endpoint returned a block whose timestamp is
	// ahead of the local clock by more than Config.MaxFutureDrift.
	IncidentFutureBlock IncidentKind = "future_block"
	// IncidentLogOmission: an endpoint's log scan lacked a transfer that
	// another endpoint's logs and a second endpoint's receipt show. The
	// transfer was reported; the endpoint is marked faulty.
	IncidentLogOmission IncidentKind = "log_omission"
	// IncidentLogMismatch: two log-serving endpoints disagree about the
	// transfers in a range and nobody could break the tie. The processed
	// head waits.
	IncidentLogMismatch IncidentKind = "log_mismatch"
	// IncidentUnknownTransfer: an endpoint reported a transfer, deep
	// enough to be settled, that at least two other endpoints know nothing
	// of. It is not reported, and the endpoint is set aside.
	IncidentUnknownTransfer IncidentKind = "unknown_transfer"
	// IncidentStaleLog: an endpoint's logs show a transfer in a block that
	// is not the block the endpoints agree on at that height — a block
	// that was reorganised away, or never existed. A transfer reported
	// from it is taken back; the endpoint is set aside.
	IncidentStaleLog IncidentKind = "stale_log"
	// IncidentLateTransfer: a transfer was found, by a later scan on a
	// second endpoint, in blocks that had been settled on a single
	// endpoint's logs. It is reported below the processed head; the
	// payment it belongs to may have expired in the meantime.
	IncidentLateTransfer IncidentKind = "late_transfer"
	// IncidentHeadHeld: the processed head has not moved for several ticks
	// although there are blocks to settle. Detail says what it is waiting
	// for (Status.Hold). Raised again every minute for as long as it lasts.
	IncidentHeadHeld IncidentKind = "head_held"
	// IncidentCheckpointAhead: the checkpoint claimed blocks as settled
	// that no reachable endpoint has. The watcher went back to the chain
	// the endpoints agree on and scans again from there.
	IncidentCheckpointAhead IncidentKind = "checkpoint_ahead"
	// IncidentHeightMismatch: an endpoint's chain ended more than the
	// settle depth below the height two other endpoints had reached. It is
	// set aside; the other two decide where the chain ends.
	IncidentHeightMismatch IncidentKind = "height_mismatch"
	// IncidentLogIndexBehind: the height an endpoint reports together with
	// its eth_getLogs answers stayed behind the chain for several ticks, so
	// that nothing above it could be scanned. The endpoint is set aside.
	IncidentLogIndexBehind IncidentKind = "log_index_behind"
	// IncidentCheckpointBehind: the checkpoint's position lay more than a
	// reorg window below anything that still has to be scanned — below the
	// consumer's processed head and below the first block in which a
	// watched address can have been paid. The watcher continues from
	// there instead of scanning the blocks in between.
	IncidentCheckpointBehind IncidentKind = "checkpoint_behind"
	// IncidentSingleSource: VerifyBestEffort only. Every other endpoint
	// has been unreachable for soloPatience ticks in a row, and from now on
	// what the remaining endpoint says is reported without confirmation:
	// transfers, reverts, heads. Raised every time that begins.
	IncidentSingleSource IncidentKind = "single_source"
	// IncidentSingleSourceEnded: another endpoint answered again; facts
	// need two endpoints again.
	IncidentSingleSourceEnded IncidentKind = "single_source_ended"
)

// Incident is something the operator should know about: an endpoint caught
// contradicting another, or the watcher repairing its own checkpoint.
// Incidents are informational — the watcher has already done what is safe.
type Incident struct {
	Kind IncidentKind
	// Endpoint is the redacted URL of the endpoint held responsible, or ""
	// if the watcher cannot tell which of two is wrong.
	Endpoint string
	// Block is the block number concerned, if any.
	Block uint64
	// Detail is a human-readable description (URLs redacted).
	Detail string
}

// incidentRepeat is how long the same incident (kind and endpoint) is not
// raised again.
const incidentRepeat = time.Minute

// incident logs and reports an incident, at most once per incidentRepeat for
// the same kind and endpoint.
func (t *tick) incident(kind IncidentKind, ep *evm.Endpoint, block uint64, detail string) {
	w := t.w
	label := ""
	if ep != nil {
		label = ep.Label()
	}
	key := string(kind) + " " + label
	// The beginning and the end of single-source operation are reported
	// every time, however close together.
	always := kind == IncidentSingleSource || kind == IncidentSingleSourceEnded
	if last, seen := w.incidents[key]; seen && !always && t.now.Sub(last) < incidentRepeat && !t.now.Before(last) {
		return
	}
	if w.incidents == nil {
		w.incidents = make(map[string]time.Time)
	}
	w.incidents[key] = t.now
	w.log.Error("chain watcher incident", "kind", string(kind), "endpoint", label, "block", block, "detail", detail)
	if w.cfg.OnIncident != nil {
		w.cfg.OnIncident(Incident{Kind: kind, Endpoint: label, Block: block, Detail: detail})
	}
}

// unixTime converts a block timestamp.
func unixTime(sec uint64) time.Time {
	return time.Unix(int64(min(sec, math.MaxInt64/2)), 0).UTC()
}

// holdf records why the processed head is not moving in this tick. The first
// reason given is the one kept.
func (t *tick) holdf(format string, args ...any) {
	if t.holding == "" {
		t.holding = fmt.Sprintf(format, args...)
	}
}

// future reports whether a block timestamp is further ahead of the local
// clock than tolerated.
//
// Ahead of the clock as it read when the tick began is not yet ahead: the
// clock may have been stepped forward since (time synchronisation correcting
// a clock that was slow), and a block mined after the step, by a chain that
// keeps real time, is not from the future. So before a block is called that,
// the clock is read again, and that reading counts from then on in this tick.
func (t *tick) future(sec uint64) bool {
	if t.w.cfg.MaxFutureDrift < 0 {
		return false // the check is switched off
	}
	if sec > math.MaxInt64/2 {
		return true
	}
	at := time.Unix(int64(sec), 0)
	if t.clock.IsZero() {
		t.clock = t.now
	}
	if at.Sub(t.clock) <= t.w.cfg.MaxFutureDrift {
		return false
	}
	if now := t.w.cfg.Now(); now.After(t.clock) {
		t.clock = now
	}
	return at.Sub(t.clock) > t.w.cfg.MaxFutureDrift
}

// ahead formats how far a block timestamp is ahead of the local clock.
func (t *tick) ahead(sec uint64) string {
	if sec > math.MaxInt64/2 {
		return "absurdly far"
	}
	now := t.clock
	if now.IsZero() {
		now = t.now
	}
	return time.Unix(int64(sec), 0).Sub(now).Round(time.Second).String()
}

// others returns the usable endpoints other than the primary, in the order
// the client prefers them (healthy ones first, then lagging ones, then those
// that are cooling down) — in every verification mode. What the pool thinks
// of an endpoint decides when it is asked, never whether: "lagging" in
// particular is an estimate that rests on what endpoints say about the
// chain's height, which an endpoint can misstate.
func (t *tick) others() []*evm.Endpoint {
	if t.othersDone {
		return t.otherEPs
	}
	t.othersDone = true
	c := t.w.cfg.Client
	skip := []*evm.Endpoint{t.primary}
	for len(skip) < c.Len() {
		ep, err := c.Pick(t.ctx, evm.Except(skip...))
		if err != nil {
			break
		}
		t.otherEPs = append(t.otherEPs, ep)
		skip = append(skip, ep)
	}
	return t.otherEPs
}

// soloPatience is the number of consecutive ticks in which every other
// endpoint must have been unreachable before VerifyBestEffort accepts the
// remaining endpoint's word.
const soloPatience = 5

// maxStartSpread is how far apart the heads of the primary and the verifier
// may be when a position is derived from them: the start position of a
// watcher without a checkpoint, and the position a checkpoint that lies
// above the chain is put back to. Both follow the lower of the two heads,
// and following it without a bound lets one endpoint that is far behind — or
// says so — put the cursor, and with it the consumer's processed head,
// millions of blocks down, where it stays after the endpoint is gone.
// Honest endpoints are seconds apart; 64 blocks is half a minute
// on BSC.
const maxStartSpread = 64

// sawHeight records a chain height an endpoint showed in this tick.
func (t *tick) sawHeight(ep *evm.Endpoint, height uint64) {
	if height > t.topHeight {
		t.topHeight, t.topBy = height, ep.Label()
	}
}

// rereadHead asks the primary for its view of the chain again — the request
// the tick began with — and replaces what the tick holds of it: its head, its
// finalized block, the remembered blocks as it sees them.
func (t *tick) rereadHead() error {
	hr, err := t.primary.Head(t.ctx, t.headReq)
	if err != nil {
		return fmt.Errorf("watcher: fetch head: %w", err)
	}
	t.head = hr.Latest
	t.priFinalized = hr.Finalized
	t.logHead = hr.Latest.Number
	t.startHeaders = hr.ByNumber
	t.startHeaders[hr.Latest.Number] = hr.Latest
	t.sawHeight(t.primary, hr.Latest.Number)
	return nil
}

// secondHighest returns the second-highest of three numbers: the highest
// height that two of three endpoints have reached.
func secondHighest(a, b, c uint64) uint64 {
	return max(min(a, b), min(max(a, b), c))
}

// resolveVerifier chooses the endpoint that cross-checks the primary in this
// tick and asks it for its own view of the chain: its latest block, its
// finalized block and the header at `guess`, the block the cursor will most
// likely move to. With a single configured endpoint the "verifier" is the
// primary itself.
//
// # The height of the chain
//
// The verifier is the first of the other endpoints that answers. If its head
// and the primary's are more than SettleDepth apart, the two do not agree on
// where the chain ends, and where there is a third endpoint it is asked. The
// height of the chain is then the highest one that two of the three have
// reached — the second-highest of the three heads:
//
//   - An endpoint in use that is more than SettleDepth below it stands
//     alone and is set aside (invariant 4): the verifier is replaced by the
//     third endpoint in this very tick; if it is the primary, the tick fails
//     and the next one starts from another endpoint. The three heads are
//     not read in the same moment, and the comparison has to allow for
//     that: an endpoint is behind only if it was asked after the height it
//     is measured against was read. The verifier is (it is asked after the
//     primary, and only the primary's head can condemn it); the primary,
//     which is asked first, is asked again before it is set aside.
//   - An endpoint that claims more than the other two is not believed about
//     it: nothing is scanned or settled beyond what two endpoints have.
//
// With two endpoints there is nobody to ask. The height of the chain is the
// lower of the two heads, nothing is settled above it, and the disagreement
// is named (settle) for as long as it holds the head.
//
// # Best effort
//
// If no other endpoint answers, nothing can be confirmed and nothing is
// settled (t.ver stays nil) — in every mode, with one exception that is the
// whole of VerifyBestEffort: the remaining endpoint is believed on its own
// once every other endpoint has been unreachable for soloPatience
// consecutive ticks. Unreachable means that it did not answer at all
// (evm.Health.Unreachable: no connection, a timeout, a gateway's 5xx); an
// endpoint that answers — with a rate limit, an error, another chain's id, a
// block from the future, a height or a header nobody shares — is there, and
// what it says is weighed like in the default mode. So nothing an endpoint
// says, and nothing the pool concludes from what endpoints say (lag, a fault
// report), brings this state about. It begins and ends with an incident
// (IncidentSingleSource, IncidentSingleSourceEnded) and shows as
// Status.Solo.
//
// It returns an error if the primary must not be used for this tick.
func (t *tick) resolveVerifier(guess uint64) error {
	w := t.w
	cfg := &w.cfg
	if cfg.Client.Len() == 1 {
		t.ver = t.primary
		return nil
	}
	req := evm.HeadRequest{Finalized: !cfg.NoFinality, Numbers: []uint64{guess}}
	if f := t.priFinalized; f != nil && f.Number != guess {
		// What the primary calls finalized: does the verifier have the
		// same block there?
		req.Numbers = append(req.Numbers, f.Number)
	}
	if a, ok := w.st.newestAnchor(); ok && !slices.Contains(req.Numbers, a.Number) {
		// And the newest block the watcher has settled on (checkChain).
		req.Numbers = append(req.Numbers, a.Number)
	}
	var last error
	// ask returns ep's view of the chain, or nil if it cannot serve as the
	// verifier in this tick (last says why).
	ask := func(ep *evm.Endpoint) *evm.HeadResult {
		hr, err := ep.Head(t.ctx, req)
		if err != nil {
			last = fmt.Errorf("%s: %w", ep.Label(), err)
			return nil
		}
		if t.future(hr.Latest.Time) && !t.future(t.head.Time) {
			// Only this endpoint's chain is in the future.
			ep.ReportFault("reports a block from the future")
			t.incident(IncidentFutureBlock, ep, hr.Latest.Number, fmt.Sprintf("block %d is stamped %s ahead of this server's clock",
				hr.Latest.Number, t.ahead(hr.Latest.Time)))
			last = fmt.Errorf("%s: reports a block from the future", ep.Label())
			return nil
		}
		t.sawHeight(ep, hr.Latest.Number)
		return hr
	}
	use := func(ep *evm.Endpoint, hr *evm.HeadResult) {
		t.ver, t.independent = ep, true
		t.verTip = hr.Latest
		t.verHead = hr.Latest.Number
		t.verHeaders = hr.ByNumber
		t.verHeaders[hr.Latest.Number] = hr.Latest
		t.verFinalized = hr.Finalized
	}

	others := t.others()
	var ver *evm.Endpoint
	var verView *evm.HeadResult
	for len(others) > 0 && ver == nil {
		ep := others[0]
		others = others[1:]
		if hr := ask(ep); hr != nil {
			ver, verView = ep, hr
		}
		if cerr := t.ctx.Err(); cerr != nil {
			t.verErr = cerr
			return nil
		}
	}
	if ver == nil {
		if cfg.Verify == VerifyBestEffort && t.aloneFor(last) {
			t.ver, t.solo = t.primary, true
			return nil
		}
		if last == nil {
			last = evm.ErrNoEndpoint
		}
		if t.verErr == nil {
			t.verErr = last
		}
		return nil
	}
	t.notAlone(ver)

	// Do the two agree on where the chain ends? If not, a third decides.
	depth := cfg.SettleDepth
	p, v := t.head.Number, verView.Latest.Number
	if max(p, v)-min(p, v) > depth {
		for len(others) > 0 {
			third := others[0]
			others = others[1:]
			thirdView := ask(third)
			if cerr := t.ctx.Err(); cerr != nil {
				t.verErr = cerr
				return nil
			}
			if thirdView == nil {
				continue
			}
			n := thirdView.Latest.Number
			agreed := secondHighest(p, v, n)
			if p+depth < agreed {
				// The primary looks behind — but its head was read first,
				// and the two it is measured against after it: every block
				// the chain produced while the other two were being asked
				// counts against it, and so does every second one of them
				// took to answer. Before it is set aside it is asked again,
				// now that it is the last to answer; the tick goes on from
				// what it says then. (What it says is its claim either way:
				// an endpoint that is behind is still behind, and one that
				// answers low and then high has only spent a request.)
				if err := t.rereadHead(); err != nil {
					return err
				}
				p = t.head.Number
			}
			if p+depth < agreed {
				detail := fmt.Sprintf("%s is at block %d, %d blocks behind what %s (block %d) and %s (block %d) have reached",
					t.primary.Label(), p, agreed-p, ver.Label(), v, third.Label(), n)
				t.primary.ReportFault(fmt.Sprintf("its chain ends at block %d, %d blocks below the height two other endpoints have reached", p, agreed-p))
				t.incident(IncidentHeightMismatch, t.primary, p, detail)
				return fmt.Errorf("%w: %s", ErrInconsistentEndpoint, detail)
			}
			if n > v {
				if v+depth < agreed {
					detail := fmt.Sprintf("%s is at block %d, %d blocks behind what %s (block %d) and %s (block %d) have reached; %s cross-checks in its place",
						ver.Label(), v, agreed-v, t.primary.Label(), p, third.Label(), n, third.Label())
					ver.ReportFault(fmt.Sprintf("its chain ends at block %d, %d blocks below the height two other endpoints have reached", v, agreed-v))
					t.incident(IncidentHeightMismatch, ver, v, detail)
				}
				ver, verView = third, thirdView
			}
			// From here on the lower of the primary's and the verifier's
			// head is a height two of three endpoints have reached, or
			// within a settle depth below it.
			t.voted = true
			break
		}
	}
	use(ver, verView)
	return nil
}

// headsAgree reports whether the heads of the primary and the verifier are
// close enough together — or have been put to a third endpoint — for a
// position to be derived from the lower of them (maxStartSpread).
func (t *tick) headsAgree() (spread uint64, ok bool) {
	if !t.independent {
		return 0, true
	}
	spread = max(t.head.Number, t.verHead) - min(t.head.Number, t.verHead)
	return spread, t.voted || spread <= maxStartSpread
}

// aloneFor is VerifyBestEffort's one question, asked in a tick in which no
// other endpoint could be used as the verifier: has every other endpoint
// been unreachable for soloPatience ticks in a row? Only then is the primary
// believed on its own. An endpoint that answered — whatever it answered —
// ends the count.
func (t *tick) aloneFor(last error) bool {
	w := t.w
	if last == nil {
		last = evm.ErrNoEndpoint
	}
	if !w.cfg.Client.OthersUnreachable(t.primary) {
		t.notAlone(nil)
		t.verErr = fmt.Errorf("%w (best effort: another endpoint answers, so nothing is taken on one endpoint's word)", last)
		return false
	}
	if label := t.primary.Label(); w.soloFor != label {
		// The ticks counted so far — and single-source operation, if it
		// had begun — were about another endpoint, the one left then; this
		// one was among those that did not answer. Its patience starts now.
		if w.solo {
			t.endSolo(fmt.Sprintf("%s, which was believed on its own, is not the endpoint in use any more (%s is)", w.soloFor, label))
		}
		w.soloFor, w.soloStreak = label, 0
	}
	// Ticks in a row in which every other endpoint was unreachable — and
	// silent in between as well: one that came back between two ticks,
	// answered the pool's health check and was down again by the next tick
	// has answered all the same. Then the count, and single-source
	// operation if it had begun, start again.
	answers := make(map[string]uint64)
	for _, ep := range w.cfg.Client.Endpoints() {
		if ep != t.primary {
			answers[ep.Label()] = ep.Health().Answers
		}
	}
	if w.soloStreak > 0 {
		for label, n := range answers {
			if before, known := w.soloAnswers[label]; !known || n > before {
				if w.solo {
					t.endSolo(label + " answered since the last tick")
				}
				w.soloStreak = 0
				break
			}
		}
	}
	if w.soloStreak == 0 {
		w.soloAnswers = answers
	}
	w.soloStreak++
	if w.soloStreak < soloPatience {
		t.verErr = fmt.Errorf("%w (best effort: every other endpoint is unreachable, tick %d of %d before this one is used alone)",
			last, w.soloStreak, soloPatience)
		return false
	}
	if !w.solo {
		w.setSolo(w.soloFor)
		t.incident(IncidentSingleSource, t.primary, w.st.Cursor+1, fmt.Sprintf(
			"best-effort verification: every other endpoint has been unreachable for %d ticks; from block %d on, transfers, reverts and the processed head are taken on the word of %s alone, until another endpoint answers again",
			soloPatience, w.st.Cursor+1, t.primary.Label()))
	}
	return true
}

// notAlone ends single-source operation: another endpoint (ep, if known)
// answered.
func (t *tick) notAlone(ep *evm.Endpoint) {
	w := t.w
	w.soloStreak, w.soloFor, w.soloAnswers = 0, "", nil
	if !w.solo {
		return
	}
	who := "another endpoint"
	if ep != nil {
		who = ep.Label()
	}
	t.endSolo(who + " answers again")
}

// endSolo ends single-source operation, saying why.
func (t *tick) endSolo(why string) {
	w := t.w
	w.setSolo("")
	t.incident(IncidentSingleSourceEnded, nil, w.st.Cursor+1, fmt.Sprintf(
		"best-effort verification: %s; from block %d on, nothing is reported on one endpoint's word", why, w.st.Cursor+1))
}

// chainTip is the highest block both the primary and the verifier claim to
// have: a head only one endpoint claims does not count.
func (t *tick) chainTip() uint64 {
	if t.independent {
		return min(t.head.Number, t.verHead)
	}
	return t.head.Number
}

// agreeFinality takes in what the endpoints say about finality in this tick:
// it moves the remembered finalized block (state.Finalized) up to the one
// they agree on, and records in the tick whether the finalized block is
// known (finalKnown) and what the endpoints disagree about
// (finalityDispute).
//
// The block the primary and the verifier agree on is the lower of their two
// finalized blocks, provided the other endpoint has that very block at that
// height — the same header: number, hash, parent hash and timestamp (the
// timestamp of the final block decides when an address may leave the
// consumer's watch list; a hash both report, with a time one endpoint
// decoded, is one endpoint's word for the time). So one endpoint that claims
// a higher finalized block is not believed about it, and one whose finality
// lags holds the watcher's at its own — unless a third endpoint is
// configured: when the two do not agree, are more than the settle depth
// apart, or the lower of the two keeps a transfer the watcher follows from
// being final, it is asked, and the finalized block is the highest block
// that two of the three attest as finalized (each endpoint that does not is
// named). What holds finality back is named
// (Status.FinalityDispute): an endpoint that reports no finalized block, or
// one the others do not have, or one far behind theirs.
//
// Whether the chain has finality is the network's, not the endpoints' to
// say (Config.NoFinality): on a network with finality no block is final for
// being deep. When the endpoints attest nothing, nothing new is final; what
// they attested before stays final — also across the loss of the
// checkpoint, from the consumer's own record (Config.FinalizedFloor) — and
// the wait is named.
//
// With a single configured endpoint (or one believed alone: Solo) its word is
// taken, as for everything else.
func (t *tick) agreeFinality() error {
	w := t.w
	st := &w.st
	t.finalKnown, t.finalityDispute = false, ""
	if w.cfg.NoFinality || t.ver == nil {
		return nil // no finality to agree on; or nobody to agree with
	}
	if err := t.takeFloor(); err != nil {
		return err
	}
	if fb := st.Finalized; fb.Number != 0 && fb.Time == 0 && t.independent {
		// The remembered finalized block comes without its time (a
		// checkpoint written before the time was recorded). It is read
		// again: if the two endpoints show that very block, its time is
		// theirs. Until then the head's final block has no time, and the
		// consumer retires no address on it.
		if h, ok, err := t.headerIfAgreed(fb.Number); err != nil {
			return err
		} else if ok && h.Hash == fb.Hash {
			st.Finalized.Time = h.Time
			t.dirty = true
		}
	}
	f, v := t.priFinalized, t.verFinalized
	if !t.independent {
		if f != nil {
			t.finalize(*f)
		} else if st.Finalized.Number != 0 {
			t.finalityDispute = fmt.Sprintf("%s reports no finalized block; nothing above block %d is final until it does", t.primary.Label(), st.Finalized.Number)
		}
		return nil
	}
	// The pair's view: the lower finalized block, if the other has it.
	var agreed *evm.Header
	var dispute string
	switch {
	case f == nil && v == nil:
		dispute = fmt.Sprintf("%s and %s report no finalized block", t.primary.Label(), t.ver.Label())
	case f == nil || v == nil:
		has, hasnt, n := t.primary, t.ver, uint64(0)
		if f != nil {
			n = f.Number
		} else {
			has, hasnt, n = t.ver, t.primary, v.Number
		}
		dispute = fmt.Sprintf("%s reports block %d as finalized, %s reports no finalized block", has.Label(), n, hasnt.Label())
	default:
		lo, hi, loBy, hiBy := *f, *v, t.primary, t.ver
		if v.Number < f.Number {
			lo, hi, loBy, hiBy = *v, *f, t.ver, t.primary
		}
		there, ok, err := t.headerOf(hiBy, lo.Number)
		if err != nil {
			return err
		}
		switch {
		case !ok || there != lo:
			dispute = fmt.Sprintf("%s reports block %d as finalized, and %s has another block at that height (or none)", loBy.Label(), lo.Number, hiBy.Label())
		case hi.Number-lo.Number > w.cfg.SettleDepth:
			agreed = &lo
			dispute = fmt.Sprintf("%s reports block %d as finalized, %s block %d", hiBy.Label(), hi.Number, loBy.Label(), lo.Number)
		default:
			agreed = &lo
			if b := t.waitsAbove(lo.Number, hi.Number); b != 0 {
				// Close together, as they usually are — but the lower one
				// keeps a transfer from being final that the other already
				// has final. That costs a block: a third endpoint may have
				// it final as well, and if not, the one that is behind is
				// named.
				dispute = fmt.Sprintf("%s reports block %d as finalized, %s block %d, and the transfer in block %d waits for %s",
					hiBy.Label(), hi.Number, loBy.Label(), lo.Number, b, loBy.Label())
			}
		}
	}
	if dispute != "" {
		// A third endpoint settles it, if there is one.
		// (Not above where the primary and the verifier both have the
		// chain: the watcher follows the primary's chain, and a finalized
		// block above its head is one it cannot hold the primary to.)
		if b, why, err := t.twoOfThree(); err != nil {
			return err
		} else if b != nil && (agreed == nil || b.Number > agreed.Number) && b.Number <= t.chainTip() {
			agreed, dispute = b, why
		} else if b != nil && b.Number > t.chainTip() {
			dispute += fmt.Sprintf("; two of three endpoints attest block %d as finalized, above block %d, where the chain of %s and %s ends",
				b.Number, t.chainTip(), t.primary.Label(), t.ver.Label())
		} else if why != "" {
			dispute += "; " + why
		}
	}
	if agreed != nil {
		t.finalize(*agreed)
	}
	if dispute != "" {
		if agreed != nil && st.Finalized.Number == agreed.Number && !strings.Contains(dispute, "settled by") {
			t.finalityDispute = dispute + fmt.Sprintf(": a block is final when two endpoints say so, so payments wait for the lower (block %d)", agreed.Number)
		} else if agreed == nil {
			t.finalityDispute = dispute + fmt.Sprintf("; nothing above block %d is final until two endpoints agree", st.Finalized.Number)
		} else {
			t.finalityDispute = dispute
		}
	}
	return nil
}

// headerOf returns ep's header at number: from what it answered in this tick,
// or asked now. The boolean is false if it has no such block.
func (t *tick) headerOf(ep *evm.Endpoint, number uint64) (evm.Header, bool, error) {
	switch ep {
	case t.primary:
		return t.startHeader(number)
	case t.ver:
		if h, ok := t.verHeaders[number]; ok {
			return h, true, nil
		}
	}
	h, err := ep.HeaderByNumber(t.ctx, number)
	switch {
	case errors.Is(err, evm.ErrNotFound):
		return evm.Header{}, false, nil
	case err != nil:
		if cerr := t.ctx.Err(); cerr != nil {
			return evm.Header{}, false, cerr
		}
		return evm.Header{}, false, nil // it cannot say: it does not attest it
	}
	if ep == t.ver {
		t.verHeaders[number] = *h
	}
	return *h, true, nil
}

// twoOfThree asks a third configured endpoint, if there is one, for its
// finalized block and returns the highest block that two of the three — the
// primary, the verifier and it — attest as finalized: a block at or below
// each one's finalized block, the very block each has at that height. It
// returns nil if there is none (or no third endpoint answers), and in why
// what the third endpoint said, and who dissents.
func (t *tick) twoOfThree() (*evm.Header, string, error) {
	var third *evm.Endpoint
	var hr *evm.HeadResult
	for _, ep := range t.others() {
		if ep == t.ver {
			continue
		}
		r, err := ep.Head(t.ctx, evm.HeadRequest{Finalized: true})
		if cerr := t.ctx.Err(); cerr != nil {
			return nil, "", cerr
		}
		if err == nil {
			t.sawHeight(ep, r.Latest.Number) // what it shows counts like what the others show
			third, hr = ep, r
			break
		}
	}
	if third == nil {
		return nil, "", nil
	}
	if hr.Finalized == nil {
		return nil, fmt.Sprintf("%s, asked as well, reports no finalized block either", third.Label()), nil
	}
	finalized := map[*evm.Endpoint]*evm.Header{t.primary: t.priFinalized, t.ver: t.verFinalized, third: hr.Finalized}
	eps := []*evm.Endpoint{t.primary, t.ver, third}
	var best *evm.Header
	var bestBy []*evm.Endpoint
	for _, by := range eps {
		cand := finalized[by]
		if cand == nil || (best != nil && cand.Number <= best.Number) {
			continue
		}
		support := []*evm.Endpoint{by}
		for _, ep := range eps {
			if ep == by || finalized[ep] == nil || finalized[ep].Number < cand.Number {
				continue
			}
			h, ok, err := t.headerOf(ep, cand.Number)
			if err != nil {
				return nil, "", err
			}
			if ok && h == *cand { // the same header, its timestamp included
				support = append(support, ep)
			}
		}
		if len(support) >= 2 {
			c := *cand
			best, bestBy = &c, support
		}
	}
	if best == nil {
		return nil, fmt.Sprintf("%s, asked as well, reports block %d as finalized, and no two of the three attest the same finalized block", third.Label(), hr.Finalized.Number), nil
	}
	var dissent []string
	for _, ep := range eps {
		if !slices.Contains(bestBy, ep) {
			dissent = append(dissent, ep.Label())
		}
	}
	return best, fmt.Sprintf("settled by %s: block %d is finalized by two of three endpoints; %s dissents", third.Label(), best.Number, strings.Join(dissent, ", ")), nil
}

// takeFloor makes sure the remembered finalized block is at least the one the
// consumer holds as final (Config.FinalizedFloor): two endpoints attested it
// once, and a watcher that lost its checkpoint must not know less. It is
// taken once the primary and the verifier show the very same block at that
// height; until then nothing is reported (final is not known).
func (t *tick) takeFloor() error {
	st := &t.w.st
	floor := t.w.cfg.FinalizedFloor
	if floor == 0 || st.Finalized.Number >= floor {
		return nil
	}
	h, ok, err := t.agreedHeader(floor)
	if err != nil || !ok {
		return err
	}
	st.Finalized = blockRef{Number: h.Number, Hash: h.Hash, Time: h.Time}
	t.dirty = true
	return nil
}

// finalize records h as finalized by the endpoints in this tick.
func (t *tick) finalize(h evm.Header) {
	st := &t.w.st
	t.finalKnown = true
	switch {
	case h.Number > st.Finalized.Number:
		st.Finalized = blockRef{Number: h.Number, Hash: h.Hash, Time: h.Time}
		t.dirty = true
	case h.Number == st.Finalized.Number && h.Hash == st.Finalized.Hash && st.Finalized.Time == 0:
		// A checkpoint written before the time of the finalized block was
		// recorded: the block the endpoints agree on now is that very
		// block, and its time is known again.
		st.Finalized.Time = h.Time
		t.dirty = true
	}
}

// waitsAbove returns the block of the lowest transfer the watcher follows —
// reported, and still inside the reorg window or pending for the consumer —
// that a finalized block of lo keeps from being final and one of hi would
// not (0 if there is none). The head's final block is the finalized block
// only as far as the processed head (tick.final), so that is a transfer above
// lo and at or below both hi and the cursor: a finalized block above the
// processed head — on BSC, where the chain finalises a block or two behind
// its tip, the usual case — costs nobody anything, however the endpoints'
// finalized blocks differ, and above the processed head nothing is paid yet.
func (t *tick) waitsAbove(lo, hi uint64) uint64 {
	hi = min(hi, t.w.st.Cursor)
	var b uint64
	for _, tr := range t.w.st.Tracked {
		if tr.delivered && tr.BlockNumber > lo && tr.BlockNumber <= hi && (b == 0 || tr.BlockNumber < b) {
			b = tr.BlockNumber
		}
	}
	return b
}

// final returns Head.Final for a processed head at number, and false if it is
// not known: the endpoints have never agreed on a finalized block (since the
// watcher started without a checkpoint and without a floor from the
// consumer, or since an accepted contradiction of finalized history reset
// it). Nothing is final then, and no head is reported (Status.Hold). On a
// network without finality (Config.NoFinality) it is the reorganisation
// window below number: what the watcher can still re-examine.
func (t *tick) final(number uint64) (uint64, bool) {
	w := t.w
	switch {
	case w.cfg.NoFinality:
		return number - min(number, w.cfg.ReorgWindow), true
	case w.cfg.FinalizedFloor > w.st.Finalized.Number:
		// The consumer holds a higher block as final than this watcher
		// knows (it started from an older checkpoint, or none): a head
		// final below it would say less than was attested.
		return 0, false
	case w.st.Finalized.Number != 0 || t.finalKnown:
		return min(number, w.st.Finalized.Number), true
	}
	return 0, false
}

// checkClock rejects a chain tip that lies in the future.
func (t *tick) checkClock() error {
	if !t.future(t.head.Time) {
		return nil
	}
	if t.independent && t.future(t.verTip.Time) {
		// Both endpoints: it is this server's clock that is off.
		return fmt.Errorf("%w: block %d is stamped %s ahead according to %s and %s; check the server clock",
			ErrFutureBlock, t.head.Number, t.ahead(t.head.Time), t.primary.Label(), t.ver.Label())
	}
	t.primary.ReportFault("reports a block from the future")
	t.incident(IncidentFutureBlock, t.primary, t.head.Number, fmt.Sprintf("block %d is stamped %s ahead of this server's clock",
		t.head.Number, t.ahead(t.head.Time)))
	if !t.independent {
		return fmt.Errorf("%w: %s reports block %d stamped %s ahead; either the endpoint lies about block time or the server clock is wrong",
			ErrFutureBlock, t.primary.Label(), t.head.Number, t.ahead(t.head.Time))
	}
	return fmt.Errorf("%w: %s reports block %d stamped %s ahead of this server's clock; %s does not",
		ErrInconsistentEndpoint, t.primary.Label(), t.head.Number, t.ahead(t.head.Time), t.ver.Label())
}

// quorumPatience is the number of consecutive ticks for which two endpoints
// must both report a chain that ends below the checkpoint before the
// checkpoint is taken to be the one that is wrong. Two honest endpoints that
// are merely a few blocks behind catch up within that time.
const quorumPatience = 3

// checkpointAhead deals with a checkpoint that claims more than the chain
// has: the cursor, the finalized block or the newest remembered block lies
// above the primary's head.
//
// Usually the primary is merely behind, and the tick fails so that another
// endpoint is tried: an endpoint that has not reached the newest block the
// watcher remembers cannot show whether the chain below it is still the one
// that was scanned. But if the verifier's chain ends below the checkpoint as
// well, and stays there, it is the checkpoint that is wrong — it was written
// while an endpoint lied about the height of the chain, or the chain was
// rolled back — and waiting would wedge the watcher for good. The watcher
// then goes back to the settle depth below the lower of the two heads,
// forgets every block hash it remembers above that, and scans again;
// whatever it reported from blocks that do not exist is reverted by that
// scan like any other vanished transfer. With a single source of truth the
// same happens once the endpoint has insisted for Config.ConflictPatience
// ticks.
func (t *tick) checkpointAhead() error {
	w := t.w
	st := &w.st
	top := max(st.Cursor, st.Finalized.Number)
	if a, ok := st.newestAnchor(); ok {
		top = max(top, a.Number)
	}
	if t.head.Number >= top {
		w.aheadStreak = 0
		return nil
	}
	behind := fmt.Sprintf("watcher: endpoint %s is at block %d, behind block %d, which has already been seen", t.primary.Label(), t.head.Number, top)
	patience := w.cfg.ConflictPatience
	switch {
	case t.ver == nil:
		return fmt.Errorf("%s; no second endpoint available to tell which is wrong: %v", behind, t.verErr)
	case t.independent:
		if t.verHead >= top {
			w.aheadStreak = 0
			return errors.New(behind)
		}
		patience = quorumPatience
		behind += fmt.Sprintf(", and so is %s (at block %d)", t.ver.Label(), t.verHead)
		if spread, ok := t.headsAgree(); !ok {
			// Both are below the checkpoint, but not at the same place:
			// where the chain ends is not something the two agree on, and
			// the lower of the two heads is not a position to go back to.
			w.aheadStreak = 0
			return fmt.Errorf("%s; the two are %d blocks apart, so the checkpoint is not put back on their word", behind, spread)
		}
	}
	// A single source has to insist on its own (under best effort another
	// endpoint may be the one left from one tick to the next).
	by := "the primary and the verifier"
	if !t.independent {
		by = t.primary.Label()
	}
	if w.aheadBy != by {
		w.aheadBy, w.aheadStreak = by, 0
	}
	w.aheadStreak++
	if w.aheadStreak < patience {
		return fmt.Errorf("%s; waiting for it to persist (%d/%d)", behind, w.aheadStreak, patience)
	}
	w.aheadStreak = 0

	tip := t.chainTip()
	target := tip - min(tip, w.cfg.SettleDepth)
	kept := st.Anchors[:0:0]
	for _, a := range st.Anchors {
		if a.Number <= target {
			kept = append(kept, a)
		}
	}
	st.Anchors = kept
	// A finalized block the chain still has stays final: only one above
	// where both endpoints' chains end is given up (as is the consumer's
	// floor, then).
	if st.Finalized.Number > tip {
		st.Finalized = blockRef{}
	}
	if w.cfg.FinalizedFloor > tip {
		w.cfg.FinalizedFloor = 0
	}
	t.setCursor(min(st.Cursor, target))
	if err := w.save(t.ctx); err != nil {
		return err
	}
	t.dirty = false
	t.incident(IncidentCheckpointAhead, nil, top, fmt.Sprintf(
		"the checkpoint claimed block %d as settled but the chain ends at block %d; scanning again from block %d",
		top, tip, st.Cursor+1))
	return nil
}

// setCursor moves the cursor to a block whose header has not been agreed on
// by the endpoints in this process (a rewind, a catch-up step, a repair).
func (t *tick) setCursor(n uint64) {
	t.w.st.Cursor = n
	t.w.verifiedHead = blockRef{}
	t.headAgreed = false
	t.dirty = true
}

// agreedHeader returns the header of block `number` as the primary and the
// verifier both report it: number, hash, parent hash and timestamp must be
// identical, and the timestamp must be plausible. The boolean is false — and
// the reason recorded with holdf — if there is no such agreement right now.
// An error means the tick cannot continue (the primary failed or was caught
// lying).
func (t *tick) agreedHeader(number uint64) (evm.Header, bool, error) {
	if t.ver == nil {
		t.holdf("no second endpoint available to confirm block %d: %v", number, t.verErr)
		return evm.Header{}, false, nil
	}
	if h, done := t.agreed[number]; done {
		return h, true, nil
	}
	ph, err := t.primary.HeaderByNumber(t.ctx, number)
	if errors.Is(err, evm.ErrNotFound) {
		if number <= t.head.Number {
			return evm.Header{}, false, t.fault(fmt.Sprintf("does not know block %d, below its own head %d", number, t.head.Number))
		}
		t.holdf("%s does not have block %d yet", t.primary.Label(), number)
		return evm.Header{}, false, nil
	}
	if err != nil {
		return evm.Header{}, false, fmt.Errorf("watcher: fetch header %d: %w", number, err)
	}
	h := *ph
	if t.independent {
		vh, cached := t.verHeaders[number]
		if !cached {
			p, err := t.ver.HeaderByNumber(t.ctx, number)
			switch {
			case errors.Is(err, evm.ErrNotFound):
				if number <= t.verHead {
					t.ver.ReportFault(fmt.Sprintf("does not know block %d, below its own head %d", number, t.verHead))
				}
				t.holdf("%s does not have block %d", t.ver.Label(), number)
				return evm.Header{}, false, nil
			case err != nil:
				if cerr := t.ctx.Err(); cerr != nil {
					return evm.Header{}, false, cerr
				}
				t.holdf("asking %s for block %d failed: %v", t.ver.Label(), number, err)
				return evm.Header{}, false, nil
			}
			vh = *p
			t.verHeaders[number] = vh
		}
		if vh != h {
			return evm.Header{}, false, t.disagree(number, h, vh)
		}
	}
	if reason := t.implausible(h); reason != "" {
		if !t.independent {
			t.primary.ReportFault("reports an impossible block: " + reason)
			return evm.Header{}, false, fmt.Errorf("%w: %s reports block %d %s", ErrInconsistentEndpoint, t.primary.Label(), number, reason)
		}
		// Two endpoints agree on something that cannot be. Nothing is
		// settled on it.
		t.holdf("block %d as reported by %s and %s is %s", number, t.primary.Label(), t.ver.Label(), reason)
		return evm.Header{}, false, nil
	}
	if t.agreed == nil {
		t.agreed = make(map[uint64]evm.Header)
	}
	t.agreed[number] = h
	return h, true, nil
}

// implausible returns "" for a header that fits what the watcher knows, or
// what is wrong with it: a timestamp in the future, or one that runs against
// the remembered blocks around it (block time never decreases with height).
func (t *tick) implausible(h evm.Header) string {
	if t.future(h.Time) {
		return "stamped " + t.ahead(h.Time) + " ahead of this server's clock"
	}
	for _, a := range t.w.st.Anchors {
		if a.Time == 0 {
			continue
		}
		switch {
		case a.Number < h.Number && a.Time > h.Time:
			return fmt.Sprintf("older than block %d below it", a.Number)
		case a.Number > h.Number && a.Time < h.Time:
			return fmt.Sprintf("newer than block %d above it", a.Number)
		case a.Number+1 == h.Number && a.Hash != h.ParentHash && t.anchorCanonical(a):
			return fmt.Sprintf("not a child of block %d", a.Number)
		}
	}
	return ""
}

// anchorCanonical reports whether the primary confirmed the anchor at the
// start of this tick.
func (t *tick) anchorCanonical(a blockRef) bool {
	h, ok := t.startHeaders[a.Number]
	return ok && h.Hash == a.Hash
}

// disagree handles two different headers for the same settled-depth block.
// One of the two endpoints is wrong. A header that is impossible on its own
// gives its endpoint away; otherwise a third endpoint, if there is one,
// breaks the tie. The endpoint found wrong is marked faulty. Without a
// verdict nothing is settled and both endpoints stay in use — one liar among
// two endpoints can hold the head, not move it.
func (t *tick) disagree(number uint64, ph, vh evm.Header) error {
	what := "hash"
	switch {
	case ph.Hash == vh.Hash && ph.Time != vh.Time:
		what = "timestamp"
	case ph.Hash == vh.Hash:
		what = "parent hash"
	}
	detail := fmt.Sprintf("%s and %s report a different %s for block %d", t.primary.Label(), t.ver.Label(), what, number)

	pBad, vBad := t.implausible(ph), t.implausible(vh)
	verdict := 0 // 1: the primary is wrong, 2: the verifier is wrong
	switch {
	case pBad != "" && vBad == "":
		verdict, detail = 1, detail+"; the former is "+pBad
	case vBad != "" && pBad == "":
		verdict, detail = 2, detail+"; the latter is "+vBad
	default:
		for _, ep := range t.others() {
			if ep == t.ver {
				continue
			}
			th, err := ep.HeaderByNumber(t.ctx, number)
			if err != nil {
				continue
			}
			switch *th {
			case ph:
				verdict, detail = 2, detail+"; "+ep.Label()+" agrees with the former"
			case vh:
				verdict, detail = 1, detail+"; "+ep.Label()+" agrees with the latter"
			}
			if verdict != 0 {
				break
			}
		}
	}
	switch verdict {
	case 1:
		t.primary.ReportFault("reports another block " + fmt.Sprint(number) + " than the other endpoints")
		t.incident(IncidentHeaderMismatch, t.primary, number, detail)
		return fmt.Errorf("%w: %s", ErrInconsistentEndpoint, detail)
	case 2:
		t.ver.ReportFault("reports another block " + fmt.Sprint(number) + " than the other endpoints")
		t.incident(IncidentHeaderMismatch, t.ver, number, detail)
	default:
		t.incident(IncidentHeaderMismatch, nil, number, detail+"; no third endpoint to tell which is right")
	}
	t.holdf("%s", detail)
	return nil
}
