package corebridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// generation is one running configuration. It is the watcher's Sink and
// WatchSet: every fact the watcher produces goes to the core through it, and
// everything the watcher needs to know about the core comes from it.
type generation struct {
	b      *Bridge
	plan   plan
	client *evm.Client
	// ctx ends with the generation; w is its watcher.
	ctx context.Context
	w   *watcher.Watcher

	// clockOffset is the core's clock minus the system clock, in
	// nanoseconds, as last measured (see now): written by the watcher's
	// goroutine, read by whoever reports. Only the watcher's goroutine uses
	// lastRefused, the time of the last AlertHeadRefused.
	clockOffset atomic.Int64
	// finalityStalled says, while the final block has stood still for
	// Options.FinalityStallAfter as the processed head moved on, what
	// AlertFinalityStalled said; nil otherwise. Written by the report loop,
	// read by whoever reports: the node status names the stall for as long
	// as it lasts, not only in the alert.
	finalityStalled atomic.Pointer[string]
	lastRefused     time.Time
	lastSinceAlert  time.Time

	// sinceAt and sinceBlock cache the block found for the creation time
	// of the oldest watched payment that the core has no SinceBlock for
	// (see since). Only the watcher's goroutine uses them.
	sinceAt    time.Time
	sinceBlock uint64

	// reportMu serialises report: it is called from the report loop, from
	// the watcher's clock and after every tick, and a report put together
	// from an older reading must not land after a newer one.
	reportMu sync.Mutex
}

var (
	_ watcher.Sink     = (*generation)(nil)
	_ watcher.WatchSet = (*generation)(nil)
)

// halt wraps err so that the watcher stops and the bridge rebuilds from
// fresh configuration.
func halt(err error) error {
	return fmt.Errorf("%w: %w", watcher.ErrHalt, err)
}

// now is the watcher's clock: Options.Now, or else the core's clock.
//
// The core does not hand out its clock, but it stamps every status report
// with it. So, once per tick, the bridge sends its report and reads the
// stamp back: the difference to the system clock is what the core's clock
// is ahead by. In production the two are the same clock and the difference
// is nil; an embedder (or a test) that gives the core another clock gets a
// watcher that judges block timestamps by that clock without having to be
// told.
func (g *generation) now() time.Time {
	b := g.b
	if b.opts.Now != nil {
		return b.opts.Now()
	}
	g.report(g.ctx)
	local := time.Now()
	if ns, err := b.svc.NodeStatus(sys(g.ctx)); err == nil && !ns.LastCheckedAt.IsZero() {
		offset := ns.LastCheckedAt.Sub(local)
		if offset.Abs() < time.Second {
			offset = 0 // the same clock, read a moment apart
		}
		g.clockOffset.Store(int64(offset))
	}
	return local.Add(time.Duration(g.clockOffset.Load()))
}

// incident passes a watcher incident on as an alert.
func (g *generation) incident(in watcher.Incident) {
	kind := AlertEndpointFault
	switch in.Kind {
	case watcher.IncidentCheckpointAhead, watcher.IncidentCheckpointBehind:
		kind = AlertCheckpointRepaired
	case watcher.IncidentHeadHeld:
		kind = AlertHeadHeld
	case watcher.IncidentLateTransfer:
		kind = AlertLateTransfer
	case watcher.IncidentSingleSource:
		kind = AlertSingleSource
	case watcher.IncidentSingleSourceEnded:
		kind = AlertSingleSourceEnded
	}
	g.b.alert(Alert{Kind: kind, Message: string(in.Kind) + ": " + in.Detail, BlockNumber: in.Block})
}

// Watched implements watcher.WatchSet from core.Service.ChainState. It is a
// direct read of the store on every tick — never a cache — because the
// watcher's guarantee rests on the snapshot containing every address that
// existed when the tick's head was fetched.
func (g *generation) Watched(ctx context.Context) (watcher.Snapshot, error) {
	cs, err := g.b.svc.ChainState(sys(ctx))
	if err != nil {
		return watcher.Snapshot{}, fmt.Errorf("corebridge: read chain state: %w", err)
	}
	// The configuration this watcher was built for must still be the
	// core's: a watcher for another chain or token must not report a
	// single fact more.
	if p, idle := planFor(cs); idle != "" || !p.equal(g.plan) {
		return watcher.Snapshot{}, halt(errReconfigure)
	}
	g.b.noteReleased(cs.ReleasedUnchecked, cs.Revision)
	snap := watcher.Snapshot{
		Addresses: make([]watcher.WatchedAddress, 0, len(cs.Addresses)),
		Pending:   make([]watcher.Transfer, 0, len(cs.PendingTransfers)),
		// What the core has processed. A checkpoint that lies far below
		// it — written from a position that was wrong, say — is not
		// scanned from: the watcher continues a reorg window below the
		// core's head, and the core's head is never dragged down to the
		// checkpoint (watcher: "A checkpoint below the consumer").
		Processed: cs.Head.Number,
	}
	unknownSince, err := g.since(ctx, cs.Addresses)
	if err != nil {
		return watcher.Snapshot{}, err
	}
	for _, a := range cs.Addresses {
		addr, err := parseAddress(a.Address)
		if err != nil {
			g.b.log.Error("the core lists a deposit address that is not an address; it cannot be watched", "payment", a.PaymentID)
			continue
		}
		since := a.SinceBlock
		if since == 0 {
			since = unknownSince
		}
		// Closed: the payment is settled and nothing on it is pending — the
		// core's verdict (core.WatchedAddress.Closed), passed on as it is. A
		// watch list of closed addresses lets the watcher go quiet.
		snap.Addresses = append(snap.Addresses, watcher.WatchedAddress{Address: addr, Since: since, Closed: a.Closed})
	}
	for _, t := range cs.PendingTransfers {
		tr, err := fromCoreTransfer(t)
		if err != nil {
			g.b.log.Error("the core lists a pending transfer that cannot be decoded; it cannot be tracked", "payment", t.PaymentID, "error", err)
			continue
		}
		snap.Pending = append(snap.Pending, tr)
	}
	return snap, nil
}

// since returns the block to use as Since for the addresses the core has no
// SinceBlock for: payments created before the core had processed any head of
// this chain (the watcher had never run — a bad endpoint since setup, say).
// The core no longer creates such payments (core.ErrChainNotReady); earlier
// versions did, and their databases still hold them. As the core prescribes,
// it is derived from their creation time: the newest block not after the
// creation of the oldest such payment, minus a margin. That is only as right
// as the server clock was when the payment was created — a clock that was
// ahead then puts the block above the payment — which is why no payment is
// created that way any more.
// The lookup (a binary search over block timestamps, some 27 header
// requests per endpoint asked) is made once per oldest payment and cached.
//
// It is what keeps a watcher that starts without a checkpoint from starting
// "just below the tip" above a payment that was made while nothing was
// watching.
//
// Block timestamps are an endpoint's word, and the answer moves the start of
// the scan in both directions: too high skips a payment, too low — one
// endpoint claiming that every block is newer than the payment gives block 1
// — sends the watcher through the whole chain. So with more than one
// endpoint the answer has to be given by two of them, within sinceMargin
// blocks of each other (the lower of the two is used); a third endpoint
// outvotes one that disagrees. If no two agree, the watcher does not start,
// and says why. Best-effort verification makes no difference here unless
// the watcher is, at this moment, believing one endpoint for everything
// else as well (watcher.Watcher.SoloEndpoint: every other endpoint has been
// unreachable for several ticks) and that endpoint is the only one that
// answered here; merely being configured for best effort does not make one
// endpoint's answer do, and neither does single-source operation make the
// answer of another endpoint, or one of two answers that disagree.
func (g *generation) since(ctx context.Context, addrs []core.WatchedAddress) (uint64, error) {
	var earliest time.Time
	for _, a := range addrs {
		if a.SinceBlock == 0 && (earliest.IsZero() || a.CreatedAt.Before(earliest)) {
			earliest = a.CreatedAt
		}
	}
	if earliest.IsZero() {
		return 0, nil
	}
	if g.sinceBlock != 0 && earliest.Equal(g.sinceAt) {
		return g.sinceBlock, nil
	}
	at := earliest.UTC().Format(time.RFC3339)

	type answer struct {
		ep    *evm.Endpoint
		block uint64
	}
	var answers []answer
	var asked []*evm.Endpoint
	var lastErr error
	for len(asked) < min(g.client.Len(), 3) {
		ep, err := g.client.Pick(ctx, evm.Except(asked...))
		if err != nil {
			lastErr = err
			break
		}
		asked = append(asked, ep)
		block, err := blockAtOrBefore(ctx, ep, earliest)
		if cerr := ctx.Err(); cerr != nil {
			return 0, cerr
		}
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", ep.Label(), err)
			continue
		}
		answers = append(answers, answer{ep, block})
	}
	if len(answers) == 0 {
		return 0, fmt.Errorf("corebridge: find the block at %s (creation of the oldest watched payment): %w", at, lastErr)
	}

	// One answer is enough with a single endpoint configured — and under
	// best effort while the watcher believes that very endpoint alone for
	// everything else: when it was the only one to answer here too. An
	// answer of another endpoint, or two answers, are weighed as always.
	block, agreed := answers[0].block, g.client.Len() == 1
	if g.w != nil && len(answers) == 1 && answers[0].ep.Label() == g.w.SoloEndpoint() {
		agreed = true
	}
	for i := 0; i < len(answers) && !agreed; i++ {
		for j := i + 1; j < len(answers); j++ {
			lo, hi := min(answers[i].block, answers[j].block), max(answers[i].block, answers[j].block)
			if hi-lo <= sinceMargin {
				block, agreed = lo, true
				// Whoever is far from the two that agree is wrong.
				for _, a := range answers {
					if d := max(a.block, lo) - min(a.block, lo); d > 2*sinceMargin {
						a.ep.ReportFault(fmt.Sprintf("places the block mined at %s at %d; two other endpoints place it at %d", at, a.block, lo))
						g.b.alert(Alert{Kind: AlertEndpointFault, BlockNumber: a.block, Message: fmt.Sprintf(
							"block_time: %s says block %d was the newest at %s; two other endpoints say block %d", a.ep.Label(), a.block, at, lo)})
					}
				}
				break
			}
		}
	}
	if !agreed {
		var said []string
		for _, a := range answers {
			said = append(said, fmt.Sprintf("%s says %d", a.ep.Label(), a.block))
		}
		msg := fmt.Sprintf("cannot tell where to start scanning: no two endpoints agree on the newest block at %s, the creation of the oldest watched payment (%s)",
			at, strings.Join(said, ", "))
		if lastErr != nil {
			msg += "; " + lastErr.Error()
		}
		if now := time.Now(); now.Sub(g.lastSinceAlert) >= time.Minute {
			g.lastSinceAlert = now
			g.b.log.Error(msg)
			g.b.alert(Alert{Kind: AlertEndpointFault, Message: "block_time: " + msg})
		}
		return 0, errors.New("corebridge: " + msg)
	}
	g.sinceAt, g.sinceBlock = earliest, max(block-min(block, sinceMargin), 1)
	return g.sinceBlock, nil
}

// inFlight calls Options.inFlight, if set.
func (g *generation) inFlight(kind string) {
	if f := g.b.opts.inFlight; f != nil {
		f(kind)
	}
}

// TransferObserved implements watcher.Sink with core.Service.ObserveTransfer.
//
//   - core.ErrUnknownAddress: no payment owns the address (it left the
//     core's books between the snapshot and now). Nothing to do.
//   - core.ErrTransferFinal: the transaction is already final in another
//     block — the chain contradicts a settlement. The core keeps what it
//     decided; the operator is alerted; the fact is not retried.
//   - core.ErrChainMismatch: the core no longer runs the chain or token this
//     watcher was built for. The watcher is halted and rebuilt.
//   - any other rejection of the fact as invalid: it can never be accepted,
//     so retrying would stall every other payment. Alert and drop.
//   - everything else (the store is down, the context ended): returned, so
//     the watcher keeps the fact and delivers it again.
func (g *generation) TransferObserved(ctx context.Context, t watcher.Transfer) error {
	amount, err := core.AmountFromBig(t.Amount)
	if err != nil {
		g.rejected("observed transfer", t, err)
		return nil
	}
	g.inFlight("transfer")
	err = g.b.svc.ObserveTransfer(g.plan.fenced(ctx), core.TransferObservation{
		ChainID:     g.plan.chainID,
		Contract:    t.Token.Hex(),
		TxHash:      t.TxHash.String(),
		LogIndex:    uint(t.LogIndex),
		BlockHash:   t.BlockHash.String(),
		BlockNumber: t.BlockNumber,
		// The block's timestamp, as two endpoints agree on it: the core
		// dates the transfer by it against the payment's deadline.
		BlockTime: t.BlockTime,
		From:      t.From.Hex(),
		To:        t.To.Hex(),
		Amount:    amount,
	})
	return g.outcome("observed transfer", t, err)
}

// TransferReverted implements watcher.Sink with core.Service.RetractTransfer.
// Errors are handled as in TransferObserved; core.ErrTransferFinal here means
// a transfer the core already counts as final has left the chain.
func (g *generation) TransferReverted(ctx context.Context, t watcher.Transfer) error {
	g.inFlight("revert")
	err := g.b.svc.RetractTransfer(g.plan.fenced(ctx), core.TransferKey{
		TxHash:    t.TxHash.String(),
		LogIndex:  uint(t.LogIndex),
		BlockHash: t.BlockHash.String(),
	})
	return g.outcome("reverted transfer", t, err)
}

// outcome maps the core's answer to a transfer fact to what the watcher
// should do with it: nil (done), an error (keep and retry) or a halt.
func (g *generation) outcome(what string, t watcher.Transfer, err error) error {
	switch {
	case err == nil:
		return nil
	case isContextErr(err):
		return err
	case errors.Is(err, core.ErrUnknownAddress):
		g.b.log.Debug("the core has no payment for the address of an "+what, "to", t.To.String(), "tx", t.TxHash.String())
		return nil
	case errors.Is(err, core.ErrTransferFinal):
		msg := fmt.Sprintf("the chain contradicts a transfer the core regards as final (%s in block %d): a reorganisation that replaced a block the chain had finalised; the settlement stands, check the payment by hand", what, t.BlockNumber)
		g.b.log.Error(msg, "tx", t.TxHash.String(), "log_index", t.LogIndex, "block", t.BlockNumber,
			"block_hash", t.BlockHash.String(), "to", t.To.String(), "amount", t.Amount.String())
		g.b.alert(Alert{Kind: AlertFinalityConflict, Message: msg, TxHash: t.TxHash.String(), BlockNumber: t.BlockNumber})
		return nil
	case errors.Is(err, core.ErrChainMismatch), errors.Is(err, core.ErrConfigurationReplaced):
		g.b.log.Warn("the core runs another configuration than this watcher; rebuilding", "error", err)
		return halt(err)
	case core.KindOf(err) == core.KindInvalid:
		g.rejected(what, t, err)
		return nil
	default:
		return fmt.Errorf("corebridge: report %s to the core: %w", what, err)
	}
}

// isContextErr reports whether err is a context error (the caller gave up).
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (g *generation) rejected(what string, t watcher.Transfer, err error) {
	msg := fmt.Sprintf("the core rejected an %s as malformed; it is dropped — this is a bug: %v", what, err)
	g.b.log.Error(msg, "tx", t.TxHash.String(), "log_index", t.LogIndex, "block", t.BlockNumber, "to", t.To.String())
	g.b.alert(Alert{Kind: AlertFactRejected, Message: msg, TxHash: t.TxHash.String(), BlockNumber: t.BlockNumber})
}

// HeadAdvanced implements watcher.Sink with core.Service.AdvanceHead. The
// head it receives is the watcher's processed head — the only head for which
// the core's requirement ("every transfer to a watched address up to this
// block has been reported") holds.
//
// A head the core rejects is never dropped: without a head nothing confirms
// and nothing expires, and that must be visible as a failing watcher, not
// hidden. core.ErrChainMismatch halts the watcher for a rebuild.
func (g *generation) HeadAdvanced(ctx context.Context, h watcher.Head) error {
	g.inFlight("head")
	head := core.ChainHead{
		ChainID:   g.plan.chainID,
		Number:    h.Number,
		Hash:      h.Hash.String(),
		Time:      h.Time,
		Final:     h.Final,
		FinalTime: h.FinalTime,
		Checked:   h.Checked,
	}
	err := g.b.svc.AdvanceHead(g.plan.fenced(ctx), head)
	switch {
	case err == nil:
		if f := g.b.opts.headAccepted; f != nil {
			f(head)
		}
		return nil
	case errors.Is(err, core.ErrChainMismatch), errors.Is(err, core.ErrConfigurationReplaced):
		g.b.log.Warn("the core runs another configuration than this watcher; rebuilding", "error", err)
		return halt(err)
	case errors.Is(err, core.ErrHeadTimeAhead):
		g.headRefused(ctx, h, err)
		return nil
	default:
		return fmt.Errorf("corebridge: advance the core's head to %d: %w", h.Number, err)
	}
}

// headRefused handles core.ErrHeadTimeAhead: the core's clock is behind the
// block time of a processed head, by more than the core tolerates.
//
// The watcher refuses blocks from the future itself, by the same clock (see
// generation.now), so this is the second line of defence: it is reached when
// the core is configured to tolerate less than the watcher, or was given
// another clock. Offering the same head again would not help and would keep
// the watcher from doing anything else, so the head is let go — nothing is
// recorded, nothing confirms or expires — and the next processed head is
// offered when there is one. The operator is told which of the two possible
// causes it is:
//
//   - the head was agreed on by two endpoints, or the chain's tip as a whole
//     is ahead of the clock: the server clock is behind (AlertClockSkew);
//   - a single endpoint supplied it and its tip is not ahead: that endpoint
//     reports wrong block times. It is marked faulty, so that another one is
//     used if there is one.
//
// Either way AlertHeadRefused is raised (at most once a minute).
func (g *generation) headRefused(ctx context.Context, h watcher.Head, err error) {
	b := g.b
	ws := g.w.Status()
	clock := ws.Verifier != "" || ws.ClockSkew < -b.opts.ClockSkewThreshold
	cause := "the server clock is behind the chain: check time synchronisation (NTP)"
	if !clock {
		cause = "the RPC endpoint in use reports block times ahead of the server clock (or the clock is behind and no second endpoint can tell)"
		if ep, perr := g.client.Pick(ctx, evm.NeedLogs()); perr == nil {
			ep.ReportFault("supplied a head stamped ahead of the server clock")
		}
	}
	now := time.Now()
	if now.Sub(g.lastRefused) < time.Minute {
		return
	}
	g.lastRefused = now
	msg := fmt.Sprintf("the core refused processed head %d: %v. Nothing confirms or expires until block time and the server clock agree; %s", h.Number, err, cause)
	b.log.Error(msg, "block", h.Number, "block_time", h.Time, "clock_skew", ws.ClockSkew.Round(time.Second))
	b.alert(Alert{Kind: AlertHeadRefused, Message: msg, BlockNumber: h.Number})
	if clock {
		b.alert(Alert{Kind: AlertClockSkew, Message: skewMessage(min(ws.ClockSkew, -time.Second)), BlockNumber: h.Number})
	}
}

// parseAddress parses an address as the core stores it (lower-case hex).
func parseAddress(s string) (ethaddr.Address, error) {
	return ethaddr.Parse(strings.ToLower(s))
}

// fromCoreTransfer converts a transfer from the core's books.
func fromCoreTransfer(t core.Transfer) (watcher.Transfer, error) {
	var out watcher.Transfer
	var err error
	if out.TxHash, err = evm.ParseHash(t.TxHash); err != nil {
		return out, fmt.Errorf("tx hash: %w", err)
	}
	if out.BlockHash, err = evm.ParseHash(t.BlockHash); err != nil {
		return out, fmt.Errorf("block hash: %w", err)
	}
	if out.From, err = parseAddress(t.From); err != nil {
		return out, fmt.Errorf("from: %w", err)
	}
	if out.To, err = parseAddress(t.To); err != nil {
		return out, fmt.Errorf("to: %w", err)
	}
	out.LogIndex = uint64(t.LogIndex)
	out.BlockNumber = t.BlockNumber
	out.BlockTime = t.BlockTime
	out.Amount = t.Amount.Big()
	return out, nil
}

// reportLoop reports endpoint health to the core until ctx is done, and
// raises the alerts that follow from the watcher's status.
func (g *generation) reportLoop(ctx context.Context) {
	b := g.b
	ticker := time.NewTicker(b.opts.StatusInterval)
	defer ticker.Stop()
	var (
		lastTick    time.Time
		skewStreak  int
		skewAlerted bool
		logsAlerted time.Time
		failAlerted time.Time
		// The final block as it was when it last moved, the processed
		// head then, and when that was.
		finalAt, finalHead uint64
		finalSince         time.Time
		finalAlerted       time.Time
	)
	for {
		ws := g.report(ctx)
		if !ws.LastTickAt.Equal(lastTick) {
			lastTick = ws.LastTickAt
			// Finality: the final block stays where it is while the chain
			// moves on — behind the processed head, or with no head at
			// all because the endpoints have not agreed on a finalized
			// block yet, or while they are in dispute about it. Measured
			// on the chain's tip, not on the scan's progress. (A chain
			// that does not move, finality included, is not stalled.)
			h := ws.Processed
			waiting := (h.Number != 0 && h.Final < h.Number) || ws.FinalityDispute != "" ||
				(h.Number == 0 && strings.Contains(ws.Hold, "finalized block"))
			if !waiting || h.Final != finalAt || finalSince.IsZero() {
				finalAt, finalHead, finalSince, finalAlerted = h.Final, ws.Tip.Number, time.Now(), time.Time{}
				g.finalityStalled.Store(nil)
			} else if ws.Tip.Number > finalHead && time.Since(finalSince) >= b.opts.FinalityStallAfter {
				var msg string
				if h.Number == 0 {
					msg = fmt.Sprintf("no block is final after %s: the endpoints have not agreed on a finalized block (the chain is at block %d); no payment can be created, and the watcher reports nothing, until they do",
						time.Since(finalSince).Round(time.Second), ws.Tip.Number)
				} else {
					msg = fmt.Sprintf("the chain has not finalised a block for %s: the newest final block is %d, the processed head %d, the chain at block %d; payments in later blocks have their confirmations but stay confirming until it does",
						time.Since(finalSince).Round(time.Second), h.Final, h.Number, ws.Tip.Number)
				}
				if ws.FinalityDispute != "" {
					msg += " — " + ws.FinalityDispute
				} else if ws.Hold != "" {
					msg += " — " + ws.Hold
				}
				// Said in the node status while it lasts; alerted once per repeat interval.
				g.finalityStalled.Store(&msg)
				if time.Since(finalAlerted) >= b.opts.LogsUncheckedRepeat {
					finalAlerted = time.Now()
					b.log.Error(msg, "final", h.Final, "head", h.Number, "tip", ws.Tip.Number)
					b.alert(Alert{Kind: AlertFinalityStalled, Message: msg, BlockNumber: h.Final + 1})
				}
			}
			// Clock skew: the tip's timestamp against the clock, on every
			// tick; reported when it has persisted for three.
			if ws.Tip.Number != 0 && ws.ClockSkew.Abs() > b.opts.ClockSkewThreshold {
				skewStreak++
			} else {
				skewStreak, skewAlerted = 0, false
			}
			if skewStreak >= 3 && !skewAlerted {
				skewAlerted = true
				msg := skewMessage(ws.ClockSkew)
				b.log.Error(msg, "skew", ws.ClockSkew.Round(time.Second), "tip", ws.Tip.Number, "tip_time", ws.Tip.Time)
				b.alert(Alert{Kind: AlertClockSkew, Message: msg, BlockNumber: ws.Tip.Number})
			}
			// A watcher whose scans keep failing settles nothing either;
			// that is alerted like a held head, once a minute.
			if ws.ConsecutiveErrors >= failingTicks && time.Since(failAlerted) >= time.Minute {
				failAlerted = time.Now()
				b.alert(Alert{Kind: AlertHeadHeld, BlockNumber: ws.Cursor + 1,
					Message: fmt.Sprintf("the last %d scans failed, nothing is being settled: %s", ws.ConsecutiveErrors, ws.LastError)})
			}
			switch {
			case !ws.LogsUnchecked || len(g.plan.urls) < 2:
				logsAlerted = time.Time{}
			case time.Since(logsAlerted) >= b.opts.LogsUncheckedRepeat:
				// Every time it begins, not once per process — and again
				// and again for as long as it lasts: a configuration in
				// which one endpoint decides what is seen is never a
				// state to get used to.
				logsAlerted = time.Now()
				msg := b.logsUncheckedMessage(ws)
				b.log.Warn(msg, "endpoint", ws.Endpoint, "unchecked_from_block", ws.UncheckedFrom)
				b.alert(Alert{Kind: AlertLogsUnchecked, Message: msg, BlockNumber: ws.UncheckedFrom})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// failingTicks is the number of consecutive failed scans after which the
// operator is told.
const failingTicks = 3

// logsUncheckedMessage says why blocks are being settled on one endpoint's
// logs. That happens only when no other configured endpoint serves logs —
// each has never answered a log query and has refused one again and again,
// for minutes — or, under best-effort verification, while every other
// endpoint is unreachable (ws.Solo). An endpoint that has served logs, and
// stops, holds the head instead (AlertHeadHeld).
func (b *Bridge) logsUncheckedMessage(ws watcher.Status) string {
	if ws.Solo {
		return "log scans are not cross-checked: every other endpoint is unreachable, and best-effort verification settles blocks on one endpoint's logs then; that endpoint alone decides which transfers are seen (the blocks settled meanwhile are scanned again when another endpoint that serves logs answers)"
	}
	return "log scans are not cross-checked: no other configured endpoint serves eth_getLogs, so one endpoint alone decides which transfers are seen — a payment it leaves out is not detected, and its invoice expires; add a second endpoint that serves logs (the blocks settled meanwhile are scanned again when one appears, and a payment found then is reported late)"
}

func skewMessage(skew time.Duration) string {
	d := skew.Abs().Round(time.Second)
	if skew < 0 {
		return fmt.Sprintf("server clock skew: the newest block is stamped %s ahead of this server's clock; the clock is behind — check time synchronisation (NTP)", d)
	}
	return fmt.Sprintf("server clock skew: the newest block is %s older than this server's clock; the clock is ahead, or every endpoint serves a stale chain head — check time synchronisation (NTP) and the endpoints", d)
}

// warnings lists what the operator should look at right now.
func (b *Bridge) warnings(ws watcher.Status, p plan) []string {
	var out []string
	if ws.Tip.Number != 0 && ws.ClockSkew.Abs() > b.opts.ClockSkewThreshold {
		out = append(out, skewMessage(ws.ClockSkew))
	}
	if ws.ClockDispute != "" {
		// Not taken as a fault of the clock — payments are not refused on
		// one endpoint's word — but one of the endpoints is wrong.
		out = append(out, "clock dispute: the endpoints disagree about the time of the chain's newest block — "+ws.ClockDispute)
	}
	if ws.FinalityDispute != "" {
		// A transfer is final only in a block both endpoints call
		// finalized: while they disagree, payments wait.
		out = append(out, "finality dispute: "+ws.FinalityDispute)
	}
	if ws.Solo {
		out = append(out, singleSourceMessage)
	}
	if ws.Hold != "" {
		out = append(out, "the processed head is waiting: "+ws.Hold)
	}
	if ws.ConsecutiveErrors >= failingTicks {
		out = append(out, fmt.Sprintf("the last %d scans failed: %s", ws.ConsecutiveErrors, ws.LastError))
	}
	if ws.LogsUnchecked && len(p.urls) > 1 {
		out = append(out, b.logsUncheckedMessage(ws))
	}
	return out
}

// singleSourceMessage is the standing warning of single-source operation.
const singleSourceMessage = "best-effort verification: every other endpoint is unreachable, so transfers, reverts and the processed head are taken on one endpoint's word until another answers again"

// report sends one health report: core.Service.ReportChainStatus.
//
// URLs are reported in their redacted form (scheme and host): the report is
// served to every API key with read access, and provider URLs carry keys.
// Rows are in the configured order, including endpoints the client cannot
// use, so that they line up with the settings page.
//
// Until the watcher's first tick has completed the report says Starting:
// nothing has been asked of the chain yet that it could report, and an
// endpoint nobody has asked anything is not a failed one (it is
// core.EndpointNotChecked, without an error).
//
// The warnings about the watcher as a whole (clock skew, a processed head
// that is waiting, unchecked log scans) are reported in their own field,
// and also in the Error column of the endpoint in use, where earlier
// versions of the node status put them.
func (g *generation) report(ctx context.Context) watcher.Status {
	g.reportMu.Lock()
	defer g.reportMu.Unlock()
	b, client, p := g.b, g.client, g.plan
	ws := g.w.Status()
	warnings := b.warnings(ws, p)
	if stalled := g.finalityStalled.Load(); stalled != nil {
		warnings = append(warnings, "finality stalled: "+*stalled)
	}
	b.mu.Lock()
	b.status.Warnings = warnings
	b.mu.Unlock()
	rows := client.Status()
	idle := g.idle(ws)
	status := core.ChainStatus{
		Starting:      ws.LastTickAt.IsZero(),
		Idle:          idle,
		Quiet:         idle && ws.Quiet,
		LatestBlock:   ws.Tip.Number,
		Primary:       ws.Endpoint,
		Verifier:      ws.Verifier,
		LogVerifier:   ws.LogVerifier,
		Solo:          ws.Solo,
		LogsUnchecked: ws.LogsUnchecked && len(p.urls) > 1,
		Hold:          ws.Hold,
		Work:          ws.Work,
		Warnings:      warnings,
	}
	if ws.Tip.Number != 0 {
		status.ClockSkew = ws.ClockSkew
	}
	var heights []uint64
	inUse := ws.Endpoint
	next := 0
	for _, u := range p.all {
		if !usableURL(u) {
			status.Endpoints = append(status.Endpoints, core.RPCEndpointStatus{URL: evm.RedactURL(u), State: core.EndpointUnsupported, Error: unsupportedScheme})
			continue
		}
		if next >= len(rows) || p.urls[next] != u {
			continue // a duplicate of an earlier endpoint
		}
		row := rows[next]
		next++
		es := core.RPCEndpointStatus{
			URL:         row.URL,
			Healthy:     row.Healthy,
			State:       endpointState(row),
			LatencyMs:   int(row.Latency / time.Millisecond),
			BlockNumber: row.BlockNumber,
		}
		if inUse != "" {
			es.InUse = row.URL == inUse
		} else {
			es.InUse = row.Active
		}
		switch {
		case es.State == core.EndpointNotChecked:
			// Not asked yet: nothing to report, and nothing wrong.
		case !row.Healthy:
			es.Error = row.LastError
			if es.Error == "" {
				es.Error = string(row.State)
			}
		case row.Logs.Unavailable:
			// Healthy for headers and receipts, but it cannot find
			// payments. The core's status has no field for that, and an
			// operator whose only endpoint is like this must be told why
			// nothing is ever detected.
			es.Error = "does not serve eth_getLogs (" + row.Logs.LastError + "): usable as an additional endpoint only"
		}
		if es.InUse && len(warnings) > 0 {
			es.Error = strings.TrimPrefix(es.Error+"; "+strings.Join(warnings, "; "), "; ")
		}
		if row.Healthy {
			heights = append(heights, row.Height)
		}
		status.Endpoints = append(status.Endpoints, es)
	}
	// The newest block "seen on the network" is not what one endpoint says
	// it is either: it is the height two healthy endpoints have reached
	// (with one endpoint, that endpoint's). The dashboard's lag, and with
	// it "is the node healthy", follow from this number.
	status.LatestBlock = max(status.LatestBlock, agreedHeight(heights))
	g.inFlight("status")
	if err := b.svc.ReportChainStatus(p.fenced(ctx), status); err != nil && ctx.Err() == nil {
		b.log.Debug("could not report chain status to the core", "error", err)
	}
	return ws
}

// idle reports whether the watcher is idling — quiet included — as far as
// the node status is concerned: its last tick found nothing the core is
// waiting on, and that tick is not overdue. The watcher's own word is not
// enough for the second part — a watcher that has stopped ticking still says
// what its last tick said — and "idle" is what excuses the distance between
// the core's head and the chain (core.ChainStatus.Idle). The clock is the
// watcher's own (generation.now stamps LastSuccessAt), read without a report
// of its own.
func (g *generation) idle(ws watcher.Status) bool {
	if !ws.Idle || ws.LastSuccessAt.IsZero() {
		return false
	}
	every := g.b.idleInterval()
	now := time.Now().Add(time.Duration(g.clockOffset.Load()))
	if g.b.opts.Now != nil {
		now = g.b.opts.Now()
	}
	return now.Sub(ws.LastSuccessAt) <= every+every/2
}

// monitor is the pool's health check (evm.Client.Refresh: chain id and block
// number of every endpoint, those that are cooling down included), run until
// ctx is done: every Options.HealthInterval while the watcher works, and half
// as often — never less often than the watcher's own idle ticks — while it
// idles. What the check feeds stays timely at that pace: the endpoint rows of
// the node status, the pool's idea of who is lagging, and the heights the
// watcher's "is the head held?" net looks at, which it accepts for a minute.
// What alerts the operator does not come from here at all: a dead or lying
// endpoint is found by the watcher's ticks.
func (g *generation) monitor(ctx context.Context) {
	for {
		g.client.Refresh(ctx)
		if !pause(ctx, g.healthInterval(g.w.Status().Idle)) {
			return
		}
	}
}

// healthInterval is the pause between two health checks.
func (g *generation) healthInterval(idle bool) time.Duration {
	every := g.b.opts.HealthInterval
	if idle {
		every = max(every, min(2*every, g.b.idleInterval()))
	}
	return every
}

// idleInterval is the watcher's pause between ticks while it idles.
func (b *Bridge) idleInterval() time.Duration {
	if b.opts.IdlePollInterval > 0 {
		return b.opts.IdlePollInterval
	}
	return watcher.DefaultIdleInterval
}

// endpointState is the one-word verdict on an endpoint for the node status.
// An endpoint whose chain id is unverified because nobody has asked it
// anything yet — no answer, no failure — is "not checked", not a fault.
func endpointState(row evm.EndpointStatus) string {
	switch row.State {
	case evm.StateHealthy:
		return core.EndpointHealthy
	case evm.StateUnverified:
		if row.LastSuccessAt.IsZero() && row.LastErrorAt.IsZero() {
			return core.EndpointNotChecked
		}
		return core.EndpointUnverified
	case evm.StateFailing:
		return core.EndpointFailing
	case evm.StateLagging:
		return core.EndpointLagging
	case evm.StateWrongChain:
		return core.EndpointWrongChain
	}
	return string(row.State)
}

// agreedHeight returns the highest block number that at least two of the
// given heights reach (the only one, if there is one; 0 for none).
func agreedHeight(heights []uint64) uint64 {
	var first, second uint64
	for _, h := range heights {
		switch {
		case h > first:
			first, second = h, first
		case h > second:
			second = h
		}
	}
	if len(heights) == 1 {
		return first
	}
	return second
}

// reportUnusable tells the core why nothing is running although setup is
// done: none of the configured endpoints can be used.
func (b *Bridge) reportUnusable(ctx context.Context, p plan) {
	var status core.ChainStatus
	for _, u := range p.all {
		es := core.RPCEndpointStatus{URL: evm.RedactURL(u), State: core.EndpointIdle, Error: "not in use"}
		if !usableURL(u) {
			es.State, es.Error = core.EndpointUnsupported, unsupportedScheme
		}
		status.Endpoints = append(status.Endpoints, es)
	}
	if err := b.svc.ReportChainStatus(p.fenced(ctx), status); err != nil && ctx.Err() == nil {
		b.log.Debug("could not report chain status to the core", "error", err)
	}
}
