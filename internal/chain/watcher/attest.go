package watcher

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

// This file holds what decides whether a transfer is reported, kept or taken
// back, and whether the blocks below the head have been looked at by two
// endpoints. The invariants it upholds are listed under "Invariants" in the
// package documentation; the comments below refer to them by number.

// logVerifiers returns the endpoints, other than the primary, that serve
// eth_getLogs and must therefore repeat the log scan before blocks are
// settled: every endpoint that is not held to lack a log service. The header
// verifier comes first.
//
// "Held to lack a log service" is a verdict, and it takes more than one
// refusal (evm.LogsHealth.NoService): an endpoint that has never answered a
// log query and has refused a minimal one three times in a row, over three
// minutes — or is remembered by the checkpoint as one that had. Until the
// verdict stands the endpoint is a second log source that cannot be asked,
// and the head waits for it like for any other. Once it stands, the
// endpoint is left out while it is not to be asked, and asked again each
// time that pause ends (it grows to half an hour): if it answers, it is a
// log source from then on. An endpoint that has ever answered a log query
// never comes under the verdict, whatever it answers now
// (evm.Endpoint.MarkLogsKnown).
func (t *tick) logVerifiers() []*evm.Endpoint {
	var out []*evm.Endpoint
	add := func(ep *evm.Endpoint) {
		if ep == nil || ep == t.primary || slices.Contains(out, ep) {
			return
		}
		if h := ep.Health().Logs; h.NoService {
			t.noteNoLogServer(ep)
			if h.Unavailable {
				return
			}
		}
		out = append(out, ep)
	}
	if t.independent {
		add(t.ver)
	}
	for _, ep := range t.others() {
		add(ep)
	}
	return out
}

// noteLogServer remembers, durably, that an endpoint has answered a log
// query. After a restart such an endpoint is still one that must confirm log
// scans, even if the first thing it does is refuse.
func (t *tick) noteLogServer(ep *evm.Endpoint) {
	st := &t.w.st
	label := ep.Label()
	if i := slices.Index(st.NoLogServers, label); i >= 0 {
		st.NoLogServers = slices.Delete(st.NoLogServers, i, i+1)
		t.dirty = true
	}
	if !slices.Contains(st.LogServers, label) && len(st.LogServers) < maxLogServers {
		st.LogServers = append(st.LogServers, label)
		sort.Strings(st.LogServers)
		t.dirty = true
	}
}

// noteNoLogServer remembers, durably, that an endpoint has been found to
// have no log service, so that the next process starts from that verdict
// instead of holding the head until it has been reached again.
func (t *tick) noteNoLogServer(ep *evm.Endpoint) {
	st := &t.w.st
	label := ep.Label()
	if slices.Contains(st.LogServers, label) || slices.Contains(st.NoLogServers, label) || len(st.NoLogServers) >= maxLogServers {
		return
	}
	st.NoLogServers = append(st.NoLogServers, label)
	sort.Strings(st.NoLogServers)
	t.dirty = true
}

// markLogServers tells the configured endpoints what the checkpoint
// remembers about them. It runs before the primary is picked: an endpoint
// remembered to have no log service is not the one a tick starts from
// (evm.NeedLogs), and the pool of a new process learns that only from here.
func (t *tick) markLogServers() {
	known, absent := t.w.st.LogServers, t.w.st.NoLogServers
	if len(known) == 0 && len(absent) == 0 {
		return
	}
	for _, ep := range t.w.cfg.Client.Endpoints() {
		switch h := ep.Health().Logs; {
		case slices.Contains(known, ep.Label()):
			if !h.Known {
				ep.MarkLogsKnown()
			}
		case slices.Contains(absent, ep.Label()):
			if !h.Known && !h.NoService {
				ep.MarkLogsAbsent()
			}
		}
	}
}

// trackedIn returns the tracked transfers in blocks from..to, in block order.
func (t *tick) trackedIn(from, to uint64) []*tracked {
	var out []*tracked
	for _, tr := range t.w.st.Tracked {
		if tr.BlockNumber >= from && tr.BlockNumber <= to {
			out = append(out, tr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessTransfer(out[i].Transfer, out[j].Transfer) })
	return out
}

// confirmRange is the last check before the blocks from..to are settled
// (invariants 2 and 3). It returns false if they must not be settled in this
// tick; the reason is recorded with holdf.
//
// The log scan of the range is repeated on a second endpoint that serves
// logs. Then:
//
//   - A transfer the second endpoint has and the primary did not report is a
//     candidate like any other: verified against a receipt from an endpoint
//     other than the one that produced it, and reported. The primary, having
//     left it out, is set aside.
//   - A transfer the watcher has reported and the second endpoint does not
//     have is attested again (reattest): it stays only if an endpoint other
//     than the primary still confirms it by receipt — and then it is the
//     second endpoint whose logs are incomplete, and another one is asked.
//     Otherwise it is taken back, if the block it was reported in is not the
//     block the endpoints now agree on, or it holds the head.
//
// If a second endpoint that serves logs exists and none can confirm the
// range — unreachable, throttled, behind, with logs that lack attested
// transfers, or refusing log queries without having been found to lack a log
// service (logVerifiers) — the range is not settled, for as long as that
// lasts, in every verification mode. Only if no other endpoint serves logs
// at all — or, under VerifyBestEffort, every other endpoint has been
// unreachable for long enough that the primary is believed on its own
// (tick.solo) — does the primary's scan stand alone: the transfers reported
// in the range are attested again by receipt where there is somebody to ask,
// and the range is remembered as unchecked, from its first block to its last
// (state.UncheckedFrom, UncheckedTo), to be scanned by a second endpoint
// when one appears (recheck).
//
// The second scan vouches for a block only if that block lies SettleDepth
// below the height of the backend that answered it — the same margin the
// primary's scan is given (safeHead), and for the same reason: a log index
// may trail its own node. A second endpoint that is behind therefore confirms
// a prefix of the range, and the returned block is the last one of it: the
// caller settles no further. (When ok is true, it is at least `from`.)
func (t *tick) confirmRange(from, to uint64) (upTo uint64, ok bool, err error) {
	w := t.w
	st := &w.st
	if from > to {
		return to, true, nil
	}
	if t.secondLogsTo != 0 && to > t.secondLogsTo {
		// The second endpoint has already shown, in this tick, how far its
		// logs can be relied on. It is not asked again for more.
		if to = t.secondLogsTo; from > to {
			return 0, false, nil
		}
	}
	if t.confirmedTo >= to {
		return to, true, nil
	}
	from = max(from, t.confirmedTo+1)
	if len(t.recipients) == 0 {
		t.confirmedTo = to
		return to, true, nil
	}

	candidates := t.logVerifiers()
	if t.solo {
		// Every other endpoint has just been found unreachable, again.
		candidates = nil
	}
	var problem string
	for _, lv := range candidates {
		if h := lv.Health().Logs; h.Unavailable && !h.Known {
			// It refused a log query a moment ago, in the words of a
			// provider without a log service — and of one that throttles.
			// It is not asked again before its pause ends, and not written
			// off before it has refused often enough.
			problem = fmt.Sprintf("%s has refused eth_getLogs %d time(s) in a row (%s) and is asked again at %s; it counts as an endpoint without a log service after %d refusals",
				lv.Label(), h.Refusals, h.LastError, h.UnavailableUntil.UTC().Format("15:04:05"), evm.LogsAbsentRefusals)
			continue
		}
		// How far has this endpoint got? Asked before its logs are, so that
		// the answer is a height the log query has seen. (For the header
		// verifier that is the head it reported at the start of the tick.)
		height := t.verHead
		if lv != t.ver || !t.independent {
			n, err := lv.BlockNumber(t.ctx)
			if cerr := t.ctx.Err(); cerr != nil {
				return 0, false, cerr
			}
			if err != nil {
				problem = fmt.Sprintf("%s could not be asked for its height before repeating the log scan of blocks %d..%d: %v", lv.Label(), from, to, err)
				continue
			}
			height = n
			t.sawHeight(lv, n)
		}
		noService := lv.Health().Logs.NoService
		res, err := lv.TransferLogs(t.ctx, evm.TransferQuery{Token: w.cfg.Token, FromBlock: from, ToBlock: to, Recipients: t.recipients})
		if cerr := t.ctx.Err(); cerr != nil {
			return 0, false, cerr
		}
		if err != nil {
			if noService && lv.Health().Logs.NoService {
				// Held to have no log service, asked again after its
				// pause, and still without one: it is not waited for, as
				// between two such questions.
				continue
			}
			problem = fmt.Sprintf("%s could not repeat the log scan of blocks %d..%d: %v", lv.Label(), from, to, err)
			continue
		}
		if res.BackendHead != 0 {
			// Whatever height came with the answer is one this endpoint
			// showed (tick.work: nobody rests while an endpoint shows a
			// longer chain). It can only lower what its logs vouch for.
			t.sawHeight(lv, res.BackendHead)
		}
		if res.BackendHead != 0 && res.BackendHead < height {
			// The backend that answered the log query is behind the one
			// that reported the head.
			height = res.BackendHead
		}
		// What its logs vouch for: the blocks it covered that lie a settle
		// depth below its own height. Above that its log index may simply
		// not have caught up with its node yet, and "no transfer there"
		// would mean nothing.
		limit := height - min(height, w.cfg.SettleDepth)
		upTo := min(to, limit, res.Next-min(res.Next, 1))
		if upTo < from {
			if limit < from {
				problem = fmt.Sprintf("the logs of %s are at block %d, not yet the settle depth (%d blocks) above block %d",
					lv.Label(), height, w.cfg.SettleDepth, from)
			} else {
				problem = fmt.Sprintf("%s has no logs for blocks %d..%d yet", lv.Label(), from, to)
			}
			continue
		}
		t.noteLogServer(lv)

		theirs := make(map[transferKey]Transfer, len(res.Transfers))
		for _, f := range res.Transfers {
			if !t.relevant(f, from, upTo, t.set) {
				continue
			}
			tr := Transfer{
				Token: f.Token, From: f.From, To: f.To, Amount: f.Amount,
				TxHash: f.TxHash, LogIndex: f.LogIndex, BlockNumber: f.BlockNumber, BlockHash: f.BlockHash,
			}
			theirs[keyOf(tr)] = tr
		}

		// Reported, and not in the second endpoint's logs.
		var missing []*tracked
		for _, tr := range t.trackedIn(from, upTo) {
			if _, has := theirs[keyOf(tr.Transfer)]; !has {
				missing = append(missing, tr)
			}
		}
		if len(missing) > 0 {
			attested, err := t.reattest(missing)
			if err != nil {
				return 0, false, err
			}
			if len(attested) > 0 {
				// A third party's — or its own — receipt shows what its
				// logs lack.
				m := attested[0]
				lv.ReportFault(fmt.Sprintf("its logs lack a transfer in block %d that a receipt confirms", m.BlockNumber))
				t.incident(IncidentLogOmission, lv, m.BlockNumber, fmt.Sprintf(
					"%s returned logs for blocks %d..%d without transfer %s#%d, which a receipt from another endpoint confirms in that block",
					lv.Label(), from, upTo, m.TxHash, m.LogIndex))
				problem = fmt.Sprintf("the logs of %s lack a transfer in block %d that a receipt confirms", lv.Label(), m.BlockNumber)
				continue
			}
		}

		// In the second endpoint's logs, and not reported.
		var extra []Transfer
		hidden := make(map[transferKey]bool)
		for k, tr := range theirs {
			// Reported in this process: nothing to do. On record without
			// that — the sink refused it, or the process restarted since —
			// it is reported (again) like any other: the record of a
			// transfer is not its report.
			if old, known := st.Tracked[k]; known && old.delivered {
				continue
			} else if !known && w.cfg.MinAmount != nil && tr.Amount.Cmp(w.cfg.MinAmount) < 0 {
				continue
			}
			if _, seen := t.doubted[k]; !seen {
				hidden[k] = true // the primary did not show it at all
			}
			extra = append(extra, tr)
		}
		// In the order of the chain, not of a map: which transfer an
		// incident names must not differ from run to run.
		sort.Slice(extra, func(i, j int) bool { return lessTransfer(extra[i], extra[j]) })
		if len(extra) > 0 {
			admitted, err := t.admit(extra, lv)
			if err != nil {
				return 0, false, err
			}
			caught := uint64(0)
			for _, a := range admitted {
				if hidden[keyOf(a)] {
					caught = a.BlockNumber
					t.incident(IncidentLogOmission, t.primary, a.BlockNumber, fmt.Sprintf(
						"%s returned logs for blocks %d..%d without transfer %s#%d, which %s has and a receipt confirms",
						t.primary.Label(), from, upTo, a.TxHash, a.LogIndex, lv.Label()))
				}
			}
			if caught != 0 {
				t.primary.ReportFault(fmt.Sprintf("its logs lack a transfer in block %d that other endpoints confirm", caught))
			}
			for _, x := range extra {
				if block, still := t.doubted[keyOf(x)]; still && hidden[keyOf(x)] {
					t.incident(IncidentLogMismatch, nil, block, fmt.Sprintf(
						"%s reports a transfer in block %d that %s does not, and no other endpoint confirms it",
						lv.Label(), block, t.primary.Label()))
				}
			}
			if err := t.deliver(nil, admitted); err != nil {
				return 0, false, err
			}
		}
		t.confirmedTo = upTo
		if upTo < to {
			t.secondLogsTo = upTo
			if upTo == limit {
				// Not a page of a longer scan: its logs end there.
				tip := t.chainTip()
				t.secondLogsBehind = fmt.Sprintf("the logs of %s are at block %d, %d blocks behind the chain (block %d): nothing above block %d is confirmed by a second endpoint",
					lv.Label(), height, tip-min(tip, height), tip, upTo)
			}
		}
		t.logVerifier = lv.Label()
		return upTo, true, t.recheck(lv, limit)
	}

	if problem == "" {
		// Nobody to ask right now (or only endpoints held to have no log
		// service, which still have none). But an endpoint that cannot even be
		// reached — its chain id could not be verified, so the pool does not
		// offer it — is still a second log source unless it is held to have
		// no log service or is known to serve another chain: one the
		// checkpoint knows as a log server (after a restart, say), and just
		// as much one that has never been asked for logs at all (a new
		// endpoint that is down or throttled from the start). "Serves logs"
		// includes "has not been tried" (invariant 3).
		for _, es := range w.cfg.Client.Status() {
			if es.URL == t.primary.Label() || es.State == evm.StateWrongChain || es.Logs.NoService || slices.Contains(st.NoLogServers, es.URL) {
				continue
			}
			problem = es.URL + ", which has not been asked for logs yet, cannot be reached"
			if slices.Contains(st.LogServers, es.URL) {
				problem = es.URL + ", which has served logs before, cannot be reached"
			}
			break
		}
	}
	if problem != "" && !t.solo {
		// There is a second log source and it cannot vouch for these
		// blocks. Settling them on the primary's logs alone would let the
		// primary hide a payment for good.
		t.holdf("log verification unavailable — blocks %d..%d are not settled and payments are not confirming: %s", from, to, problem)
		return 0, false, nil
	}

	// No other endpoint serves logs (or, in best-effort mode, every other
	// endpoint is unreachable). What was reported in the range must at
	// least still be attested by receipt.
	if _, err := t.reattest(t.trackedIn(from, to)); err != nil {
		return 0, false, err
	}
	t.logsUnchecked = true
	// Recorded with the endpoint whose logs settled them: a scan by that
	// endpoint is no second scan, wherever it comes in the list of
	// endpoints later on (recheck).
	st.addUnchecked(from, to, t.primary.Label())
	t.dirty = true
	t.confirmedTo = to
	return to, true, nil
}

// reattest asks whether transfers the watcher has reported — and that the
// primary's logs still show — are attested, now, by an endpoint other than
// the primary (invariant 1). It returns those that are: confirmed by a
// current receipt, in the block they were reported in.
//
// The others get no benefit of the doubt for having been confirmed once.
// The header of the transfer's block is fetched from the primary and the
// verifier:
//
//   - they agree on another block at that height: the transfer was
//     reorganised away. It is reported as reverted, and the primary, whose
//     logs outlived the block, is set aside.
//   - they agree on the transfer's block, or do not agree: the transfer
//     holds the head below its block until an endpoint attests it or the
//     chain moves on. If two other endpoints deny it the primary is set aside.
//
// With a single source of truth (one endpoint, or best-effort without a
// healthy second one) there is nobody to ask and nothing is returned.
func (t *tick) reattest(list []*tracked) ([]*tracked, error) {
	if len(list) == 0 || !t.independent {
		return nil, nil
	}
	w := t.w
	transfers := make([]Transfer, len(list))
	for i, tr := range list {
		transfers[i] = tr.Transfer
	}
	confirmed, denied, err := t.receipts(transfers, t.primary)
	if err != nil {
		return nil, err
	}
	var attested, reverts []*tracked
	for i, tr := range list {
		if confirmed[i] {
			attested = append(attested, tr)
			continue
		}
		h, agreed, err := t.agreedHeader(tr.BlockNumber)
		if err != nil {
			return nil, err
		}
		switch {
		case !agreed:
			t.hold(tr.BlockNumber)
		case h.Hash != tr.BlockHash:
			reverts = append(reverts, tr)
		default:
			t.hold(tr.BlockNumber)
			detail := fmt.Sprintf("transfer %s#%d in block %d is in the logs of %s only: no other endpoint's logs or receipt confirm it any more",
				tr.TxHash, tr.LogIndex, tr.BlockNumber, t.primary.Label())
			t.holdf("%s", detail)
			if denied[i] >= 2 {
				t.incident(IncidentStaleLog, t.primary, tr.BlockNumber, detail)
				return nil, t.fault(fmt.Sprintf("reports a transfer in block %d that no other endpoint confirms any more", tr.BlockNumber))
			}
			t.incident(IncidentLogMismatch, nil, tr.BlockNumber, detail)
		}
	}
	if len(reverts) > 0 {
		r := reverts[0]
		t.primary.ReportFault(fmt.Sprintf("its logs show a transfer in a block at height %d that is not on the chain", r.BlockNumber))
		t.incident(IncidentStaleLog, t.primary, r.BlockNumber, fmt.Sprintf(
			"%s still returns the log of transfer %s#%d from block %s at height %d, which is not on the chain any more; the transfer is reported as reverted",
			t.primary.Label(), r.TxHash, r.LogIndex, r.BlockHash, r.BlockNumber))
		w.log.Warn("a reported transfer is not on the chain the endpoints agree on", "tx", r.TxHash.String(), "block", r.BlockNumber,
			"count", len(reverts), "still_reported_by", t.primary.Label())
		if err := t.deliver(reverts, nil); err != nil {
			return nil, err
		}
	}
	return attested, nil
}

// recheck lets a second log endpoint scan blocks that were settled while
// there was none (invariant 3: unchecked blocks are remembered). One step of
// at most the endpoint's log range per tick, from the oldest unchecked block
// in which an address that is still watched can have been paid, up to the
// last block that was settled unchecked — which is recorded
// (state.Unchecked), not taken from where the cursor stood when the tick
// began: blocks can be settled unchecked in the very tick in which the
// second endpoint comes back, and they are part of the record. A
// transfer found this way was hidden from the scan that settled its block;
// it is verified and reported like any other — late: the head has passed it.
//
// Who scans them matters, not in which role. The record names, by label,
// the endpoint whose logs alone settled each run of blocks; a scan by that
// endpoint would only repeat what it said the first time — and an endpoint
// that omitted a payment then omits it again. So the endpoint that has just
// confirmed this tick's range (lv) scans them if it is not on record for
// them, otherwise the primary, which has just served logs too, and if
// neither can (both on record, or the scan is refused, or does not reach
// that far back) the next endpoint that serves logs. That is what keeps a
// change of the endpoints' order — a new endpoint put first, the old one
// serving as the second log source — from passing the old endpoint off as
// its own witness. Blocks are checked once two different endpoints that are
// configured now are on record for them (state.checkedBy): one more scan for
// blocks whose first scan is recorded under a label that is still
// configured; two — by endpoints configured now — when that label is gone
// (the URL was changed: the provider may be configured still, under the new
// one) and for the runs of a checkpoint written before the endpoints were
// recorded.
//
// Nothing is rechecked in a tick that has itself settled blocks unchecked:
// such a tick has just shown that the second log source cannot be counted
// on yet.
//
// limit is the highest block lv's logs vouch for in this tick (confirmRange):
// like the scan that settles a block, the one that makes up for it counts
// only a settle depth below the height of the backend that answered.
func (t *tick) recheck(lv *evm.Endpoint, limit uint64) error {
	w := t.w
	st := &w.st
	if len(st.Unchecked) == 0 || t.rechecked || t.logsUnchecked {
		return nil
	}
	t.rechecked = true
	if t.watched == 0 {
		// Nobody is left who could have been paid in those blocks, and an
		// address that is created later cannot have been paid below the
		// cursor it is created at.
		st.Unchecked = nil
		t.dirty = true
		return nil
	}
	if t.sinceKnown && st.dropUncheckedBelow(t.minSince+1) {
		t.dirty = true
	}
	type scanner struct {
		ep    *evm.Endpoint
		limit uint64
	}
	settled := t.safeHead() - min(t.safeHead(), w.cfg.SettleDepth)
	scanners := []scanner{{lv, limit}}
	if t.primary != lv {
		scanners = append(scanners, scanner{t.primary, settled})
	}
	for _, ep := range t.logVerifiers() {
		if ep != lv {
			scanners = append(scanners, scanner{ep, settled})
		}
	}
	for _, r := range st.Unchecked {
		for _, sc := range scanners {
			if slices.Contains(r.By, sc.ep.Label()) {
				continue // on record for these blocks already
			}
			done, err := t.recheckStep(sc.ep, r.From, min(r.To, sc.limit))
			if err != nil || done {
				return err
			}
		}
	}
	return nil
}

// configuredLabel reports whether an endpoint of that label is configured.
func (t *tick) configuredLabel(label string) bool {
	for _, ep := range t.w.cfg.Client.Endpoints() {
		if ep.Label() == label {
			return true
		}
	}
	return false
}

// recheckStep scans blocks lower..upper (at most one log range of them) on
// ep for the watched addresses, reports what the scan that settled them did
// not, and records ep's scan of the blocks it covered. done is false if
// nothing could be scanned.
func (t *tick) recheckStep(ep *evm.Endpoint, lower, upper uint64) (done bool, err error) {
	w := t.w
	st := &w.st
	end := min(upper, lower+max(ep.LogRange(), 1)-1)
	if end < lower {
		return false, nil // its logs do not reach that far yet
	}
	res, err := ep.TransferLogs(t.ctx, evm.TransferQuery{Token: w.cfg.Token, FromBlock: lower, ToBlock: end, Recipients: t.recipients})
	if cerr := t.ctx.Err(); cerr != nil {
		return false, cerr
	}
	if err != nil || res.Next <= lower {
		return false, nil // not now; the unchecked blocks stay on the books
	}
	end = min(end, res.Next-1)
	if h := res.BackendHead; h != 0 {
		t.sawHeight(ep, h)
		end = min(end, h-min(h, w.cfg.SettleDepth))
	}
	if end < lower {
		return false, nil
	}
	var extra []Transfer
	for _, f := range res.Transfers {
		if !t.relevant(f, lower, end, t.set) {
			continue
		}
		tr := Transfer{
			Token: f.Token, From: f.From, To: f.To, Amount: f.Amount,
			TxHash: f.TxHash, LogIndex: f.LogIndex, BlockNumber: f.BlockNumber, BlockHash: f.BlockHash,
		}
		// As in confirmRange: a transfer on record that was not reported in
		// this process — the sink refused it when this scan found it before
		// — is reported now. Nothing else would: its block is below the
		// cursor, and no other scan looks there.
		old, known := st.Tracked[keyOf(tr)]
		if (known && old.delivered) || (!known && w.cfg.MinAmount != nil && tr.Amount.Cmp(w.cfg.MinAmount) < 0) {
			continue
		}
		extra = append(extra, tr)
	}
	next := end + 1
	if len(extra) > 0 {
		admitted, err := t.admit(extra, ep)
		if err != nil {
			return false, err
		}
		for _, a := range admitted {
			if st.Tracked[keyOf(a)] != nil {
				continue // found before: its incident was raised then
			}
			t.incident(IncidentLateTransfer, nil, a.BlockNumber, fmt.Sprintf(
				"transfer %s#%d in block %d was not in the log scan that settled that block, at a time when no second endpoint could confirm it; %s has it and a receipt confirms it. It is reported now, below the processed head",
				a.TxHash, a.LogIndex, a.BlockNumber, ep.Label()))
		}
		if err := t.deliver(nil, admitted); err != nil {
			return false, err
		}
		for _, x := range extra {
			if block, doubted := t.doubted[keyOf(x)]; doubted {
				next = min(next, block) // not resolved: look again
			}
		}
	}
	if next > lower {
		st.checkedBy(lower, next-1, ep.Label(), t.configuredLabel)
		t.dirty = true
	}
	return true, nil
}

// receipts asks endpoints other than `reporter` for the receipts of the
// given transfers. confirmed[i] is true if one of them returned a receipt
// showing a successful transaction in the same block with the identical log
// at the same index; denied[i] counts the endpoints that answered and did
// not.
func (t *tick) receipts(list []Transfer, reporter *evm.Endpoint) (confirmed []bool, denied []int, err error) {
	w := t.w
	confirmed = make([]bool, len(list))
	denied = make([]int, len(list))
	confirmers, independent := t.confirmers(reporter)
	if len(confirmers) == 0 {
		if cerr := t.ctx.Err(); cerr != nil {
			return nil, nil, cerr
		}
		w.log.Warn("transfers not confirmed: no second endpoint to ask", "count", len(list), "error", t.verErr)
	}
	open := len(list)
	for _, ep := range confirmers {
		index := make(map[evm.Hash]int)
		var hashes []evm.Hash
		for i, f := range list {
			if confirmed[i] {
				continue
			}
			if _, seen := index[f.TxHash]; !seen {
				index[f.TxHash] = len(hashes)
				hashes = append(hashes, f.TxHash)
			}
		}
		got, err := ep.TransactionReceipts(t.ctx, hashes)
		if err != nil {
			if cerr := t.ctx.Err(); cerr != nil {
				return nil, nil, cerr
			}
			if !independent {
				return nil, nil, fmt.Errorf("watcher: fetch receipts: %w", err)
			}
			// The confirming endpoint is in trouble, not the reporter.
			w.log.Warn("transfers not confirmed: asking for receipts failed", "count", open, "endpoint", ep.Label(), "error", err)
			continue
		}
		for i, f := range list {
			if confirmed[i] {
				continue
			}
			reason := receiptMismatch(got[index[f.TxHash]], f)
			if reason == "" {
				confirmed[i] = true
				open--
				continue
			}
			denied[i]++
			w.log.Warn("transfer not confirmed: receipt does not confirm it", "tx", f.TxHash.String(), "log_index", f.LogIndex,
				"block", f.BlockNumber, "reason", reason, "reported_by", reporter.Label(), "asked", ep.Label())
		}
		if open == 0 {
			break
		}
	}
	return confirmed, denied, nil
}

// admit decides which of the transfers that `reporter` produced are reported
// to the sink (invariant 1): those confirmed by the transaction receipt from
// an endpoint other than the reporter. The others are in doubt:
//
//   - If the block a transfer claims to be in lies at settle depth and is
//     not the block the primary and the verifier agree on at that height, the
//     transfer is not on the chain: it is dropped and the reporter set aside.
//   - Otherwise it holds the cursor below its block until an endpoint
//     confirms it or a later scan no longer shows it. If at least two other
//     endpoints know nothing of it at settle depth, the reporter is set
//     aside as well, so that the next tick starts from another endpoint's
//     logs.
//
// With a single configured endpoint the receipt comes from that endpoint,
// which catches inconsistency, not lies.
func (t *tick) admit(fresh []Transfer, reporter *evm.Endpoint) ([]Transfer, error) {
	if len(fresh) == 0 {
		return nil, nil
	}
	w := t.w
	confirmed, denied, err := t.receipts(fresh, reporter)
	if err != nil {
		return nil, err
	}
	var out []Transfer
	deep := t.safeHead()
	deep -= min(deep, w.cfg.SettleDepth)
	for i, f := range fresh {
		k := keyOf(f)
		if confirmed[i] {
			delete(t.doubted, k)
			out = append(out, f)
			continue
		}
		if t.independent && f.BlockNumber <= deep {
			h, agreed, err := t.agreedHeader(f.BlockNumber)
			if err != nil {
				return nil, err
			}
			if agreed && h.Hash != f.BlockHash {
				delete(t.doubted, k)
				reporter.ReportFault(fmt.Sprintf("its logs show a transfer in a block at height %d that is not on the chain", f.BlockNumber))
				t.incident(IncidentStaleLog, reporter, f.BlockNumber, fmt.Sprintf(
					"%s returns the log of transfer %s#%d from block %s at height %d, which is not the block the endpoints agree on; it is not reported",
					reporter.Label(), f.TxHash, f.LogIndex, f.BlockHash, f.BlockNumber))
				continue
			}
		}
		if t.doubted == nil {
			t.doubted = make(map[transferKey]uint64)
		}
		t.doubted[k] = f.BlockNumber
		t.hold(f.BlockNumber)
		if t.independent && denied[i] >= 2 && f.BlockNumber <= deep {
			// Two other endpoints have no such transfer at a depth where
			// they should. The transfer still holds the head in this tick
			// — an absent receipt proves nothing — but the reporter is set
			// aside, so that the next tick works from another endpoint's
			// logs.
			reporter.ReportFault(fmt.Sprintf("reports a transfer in block %d that no other endpoint knows", f.BlockNumber))
			t.incident(IncidentUnknownTransfer, reporter, f.BlockNumber, fmt.Sprintf(
				"%s reports transfer %s#%d in block %d; %d other endpoints have no such transfer",
				reporter.Label(), f.TxHash, f.LogIndex, f.BlockNumber, denied[i]))
		}
	}
	return out, nil
}

// maxConfirmers bounds how many endpoints are asked about one batch of
// transfers.
const maxConfirmers = 3

// confirmers returns the endpoints to ask for receipts of what `reporter`
// said, and whether they are independent of it.
func (t *tick) confirmers(reporter *evm.Endpoint) ([]*evm.Endpoint, bool) {
	cfg := &t.w.cfg
	if cfg.Client.Len() == 1 {
		return []*evm.Endpoint{t.primary}, false
	}
	if t.solo {
		// Best effort, and every other endpoint has been unreachable for
		// long enough: the reporter vouches for itself.
		return []*evm.Endpoint{reporter}, false
	}
	var out []*evm.Endpoint
	add := func(ep *evm.Endpoint) {
		if ep == nil || ep == reporter || len(out) >= maxConfirmers || slices.Contains(out, ep) {
			return
		}
		out = append(out, ep)
	}
	if t.independent {
		add(t.ver)
	}
	if reporter != t.primary {
		add(t.primary)
	}
	for _, ep := range t.others() {
		add(ep)
	}
	return out, true
}

// proveReverted decides whether a tracked transfer that the primary's scan
// no longer returns has been reorganised away. Proof is a canonical block at
// the transfer's height with another hash, according to the primary and to
// the verifier (invariant 1, for reverts). Without proof the transfer stays
// as it is and the cursor stays below it — and a primary that has no good
// reason for leaving the transfer out is set aside (invariant 4):
//
//   - it still reports the transfer's block as canonical, or does not know a
//     block below its own head: its logs are incomplete;
//   - it reports another block there while the verifier still has the
//     transfer's block: at settle depth that is a disagreement like any other
//     about a header, and a third endpoint decides (disagree). Nearer the
//     tip it may be a reorganisation one endpoint has seen before the other,
//     and the watcher waits.
func (t *tick) proveReverted(tr *tracked) (bool, error) {
	w := t.w
	number := tr.BlockNumber
	omitted := func(what string) error {
		t.incident(IncidentLogOmission, t.primary, number, fmt.Sprintf(
			"%s no longer returns the log of transfer %s#%d and %s", t.primary.Label(), tr.TxHash, tr.LogIndex, what))
		return t.fault(fmt.Sprintf("omitted a transfer (tx %s) from block %d and %s", tr.TxHash, number, what))
	}
	// Ask now, not from the start-of-tick cache: the scan ran in between.
	h, err := t.primary.HeaderByNumber(t.ctx, number)
	switch {
	case errors.Is(err, evm.ErrNotFound):
		if number <= t.head.Number {
			return false, omitted(fmt.Sprintf("does not know block %d, which is below its own head %d", number, t.head.Number))
		}
		// Its chain does not reach that height (any more). Not proof.
		t.holdf("%s does not have block %d, in which a reported transfer was found, any more", t.primary.Label(), number)
		return false, nil
	case err != nil:
		return false, fmt.Errorf("watcher: fetch header %d: %w", number, err)
	case h.Hash == tr.BlockHash:
		return false, omitted(fmt.Sprintf("still reports block %d as canonical", number))
	}

	ver, independent, verr := t.verifier()
	if verr != nil {
		w.log.Warn("revert not reported: no second endpoint to confirm it", "tx", tr.TxHash.String(), "block", number, "error", verr)
		t.holdf("the revert of a transfer in block %d cannot be confirmed: no second endpoint: %v", number, verr)
		return false, nil
	}
	if !independent {
		return true, nil
	}
	vh, err := ver.HeaderByNumber(t.ctx, number)
	switch {
	case errors.Is(err, evm.ErrNotFound) && number <= t.verHead:
		ver.ReportFault(fmt.Sprintf("does not know block %d, below its own head %d", number, t.verHead))
		t.holdf("%s does not know block %d, which is below its own head", ver.Label(), number)
		return false, nil
	case err != nil:
		if cerr := t.ctx.Err(); cerr != nil {
			return false, cerr
		}
		w.log.Warn("revert not reported: verifying endpoint could not confirm it", "tx", tr.TxHash.String(),
			"block", number, "verifier", ver.Label(), "error", err)
		t.holdf("the revert of a transfer in block %d cannot be confirmed: asking %s failed: %v", number, ver.Label(), err)
		return false, nil
	case vh.Hash == tr.BlockHash:
		deep := t.safeHead()
		if number+w.cfg.SettleDepth > deep {
			// Near the tip: one endpoint may simply have seen a
			// reorganisation before the other.
			w.log.Warn("endpoints disagree about a block; revert not reported", "tx", tr.TxHash.String(),
				"block", number, "primary", t.primary.Label(), "verifier", ver.Label())
			return false, nil
		}
		w.log.Warn("endpoints disagree about a block; revert not reported", "tx", tr.TxHash.String(),
			"block", number, "primary", t.primary.Label(), "verifier", ver.Label())
		return false, t.disagree(number, *h, *vh)
	}
	return true, nil
}

// stamp sets BlockTime on the transfers that are about to be reported and
// returns those that could be dated. The others are held back like any
// other fact a second endpoint has not confirmed: they keep the cursor below
// their block and are tried again on the next tick. (The consumer may record
// the block time on the first report only, so a transfer is not reported
// without it.) A timestamp once recorded with a transfer is used again —
// once the endpoints have agreed on it in this process: what a checkpoint
// says the timestamp of a block is, is no more a fact than what it says
// about the transfer itself, which gets a receipt before it is reported
// again (invariant 1). A recorded time the endpoints do not confirm is
// replaced by the one they agree on; until they agree, the transfer waits.
func (t *tick) stamp(observe []Transfer) ([]Transfer, error) {
	w := t.w
	st := &w.st
	var need []blockRef
	for i := range observe {
		o := &observe[i]
		// Whatever time the transfer arrives here with — it is a copy of
		// the tracked record, checkpoint and all — does not count.
		o.BlockTime = time.Time{}
		if tr := st.Tracked[keyOf(*o)]; tr != nil && tr.timed && !tr.BlockTime.IsZero() {
			o.BlockTime = tr.BlockTime
		}
		if _, known := t.times[o.BlockHash]; o.BlockTime.IsZero() && !known {
			need = append(need, blockRef{Number: o.BlockNumber, Hash: o.BlockHash})
		}
	}
	if err := t.blockTimes(need); err != nil {
		return nil, err
	}
	out := observe[:0:0]
	for _, o := range observe {
		k := keyOf(o)
		if o.BlockTime.IsZero() {
			sec, ok := t.times[o.BlockHash]
			if !ok {
				if t.doubted == nil {
					t.doubted = make(map[transferKey]uint64)
				}
				t.doubted[k] = o.BlockNumber
				t.hold(o.BlockNumber)
				t.holdf("the block of a transfer (block %d) cannot be dated yet: the endpoints do not both have it", o.BlockNumber)
				continue
			}
			o.BlockTime = unixTime(sec)
			if tr := st.Tracked[k]; tr != nil {
				if !tr.BlockTime.IsZero() && !tr.BlockTime.Equal(o.BlockTime) {
					w.log.Error("the checkpoint recorded another block time for a transfer than the endpoints report; the endpoints' is used",
						"tx", o.TxHash.String(), "log_index", o.LogIndex, "block", o.BlockNumber,
						"recorded", tr.BlockTime, "block_time", o.BlockTime)
				}
				if !tr.BlockTime.Equal(o.BlockTime) {
					tr.BlockTime = o.BlockTime
					t.dirty = true
				}
				tr.timed = true
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// maxHeaderBatch bounds the headers asked for in one request.
const maxHeaderBatch = 50

// blockTimes works out the timestamps of the given blocks (number and hash)
// and records them in t.times (invariant 1, for the block time of a
// transfer): the timestamp of a header with that hash that two endpoints
// report identically — the primary and the verifier, as a rule — and that is
// plausible on its own. A block for which there is no such header right now
// gets no entry.
//
// The headers are fetched in one batch per endpoint, however many blocks
// there are. Two endpoints may honestly be on different blocks near the tip
// for a moment — one does not have the block yet, or has another one at that
// height — and the block's time is then asked of the next endpoint, for the
// blocks still undated only: any two endpoints that report the block
// identically date it (invariant 4: where a third endpoint exists, one
// endpoint that lacks a block, or names another, does not keep a transfer
// from being dated). The same hash with another timestamp, though, is a lie
// wherever it occurs, and is put to the vote at once (disagree).
func (t *tick) blockTimes(blocks []blockRef) error {
	if len(blocks) == 0 || t.ver == nil {
		return nil
	}
	fetch := func(ep *evm.Endpoint, have map[uint64]evm.Header, numbers []uint64) (map[uint64]evm.Header, error) {
		got := make(map[uint64]evm.Header, len(numbers))
		var ask []uint64
		for _, n := range numbers {
			if h, ok := have[n]; ok {
				got[n] = h
			} else {
				ask = append(ask, n)
			}
		}
		for len(ask) > 0 {
			chunk := ask[:min(len(ask), maxHeaderBatch)]
			ask = ask[len(chunk):]
			hr, err := ep.Head(t.ctx, evm.HeadRequest{Numbers: chunk})
			if err != nil {
				return nil, err
			}
			for n, h := range hr.ByNumber {
				got[n] = h
			}
		}
		return got, nil
	}
	// Two endpoints have to report a block identically: the primary and
	// the verifier, as a rule. One that fails right now (the primary may
	// have died in the middle of a scan whose results are being reported),
	// or that does not have a block, is followed by the next endpoint —
	// any two will do, the rule is agreement.
	sources := []*evm.Endpoint{t.primary}
	want := 1
	if t.independent {
		want = 2
		sources = append(sources, t.ver)
		for _, ep := range t.others() {
			if ep != t.ver {
				sources = append(sources, ep)
			}
		}
	}
	type vote struct {
		ep *evm.Endpoint
		h  evm.Header
	}
	votes := make(map[evm.Hash][]vote, len(blocks)) // headers with the block's hash, by block hash
	var open []blockRef                             // blocks without a verdict yet, each once
	for _, b := range blocks {
		if !slices.ContainsFunc(open, func(o blockRef) bool { return o.Hash == b.Hash }) {
			open = append(open, b)
		}
	}
	answered := 0
	for _, ep := range sources {
		if len(open) == 0 {
			break
		}
		var numbers []uint64
		for _, b := range open {
			if !slices.Contains(numbers, b.Number) {
				numbers = append(numbers, b.Number)
			}
		}
		var have map[uint64]evm.Header
		if ep == t.ver {
			have = t.verHeaders
		}
		got, err := fetch(ep, have, numbers)
		if err != nil {
			if cerr := t.ctx.Err(); cerr != nil {
				return cerr
			}
			if !t.independent {
				return fmt.Errorf("watcher: fetch headers of blocks with transfers: %w", err)
			}
			t.w.log.Warn("asking for the headers of blocks with transfers failed", "endpoint", ep.Label(), "error", err)
			continue
		}
		answered++
		var still []blockRef
		for _, b := range open {
			if h, ok := got[b.Number]; ok && h.Hash == b.Hash {
				votes[b.Hash] = append(votes[b.Hash], vote{ep, h})
			}
			vs := votes[b.Hash]
			if answered < want || len(vs) < want {
				still = append(still, b) // not on enough endpoints' chains yet
				continue
			}
			agreed, conflict := vs[0].h, false
			if want == 2 {
				// The newest vote against every earlier one: two that are
				// identical date the block; two that differ are a lie.
				last := vs[len(vs)-1]
				matched := false
				for _, v := range vs[:len(vs)-1] {
					if v.h == last.h {
						agreed, matched = last.h, true
						break
					}
				}
				if !matched {
					conflict = true
					if a, c := vs[0], last; len(vs) == 2 && a.ep == t.primary && c.ep == t.ver {
						if err := t.disagree(b.Number, a.h, c.h); err != nil {
							return err
						}
					} else {
						t.w.log.Warn("endpoints report the same block with another header; the transfer's block time waits",
							"block", b.Number, "hash", b.Hash.String(), "endpoint", c.ep.Label(), "other", a.ep.Label())
					}
				}
			}
			if conflict {
				continue // no time for this block in this tick
			}
			if reason := t.implausible(agreed); reason != "" {
				if !t.independent {
					t.primary.ReportFault("reports an impossible block: " + reason)
					return fmt.Errorf("%w: %s reports block %d %s", ErrInconsistentEndpoint, t.primary.Label(), b.Number, reason)
				}
				t.holdf("block %d as reported by two endpoints is %s", b.Number, reason)
				continue
			}
			if t.times == nil {
				t.times = make(map[evm.Hash]uint64)
			}
			t.times[b.Hash] = agreed.Time
		}
		open = still
	}
	return nil
}
