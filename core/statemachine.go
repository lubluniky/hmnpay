package core

import (
	"reflect"
	"time"
)

// EvalInput is the context [Evaluate] judges a payment in.
type EvalInput struct {
	// Head is the newest block the chain adapter has fully processed, if
	// the payment's asset is the one being watched (else the zero head).
	// Confirmations are counted from it, finality is its Final; a zero
	// head confirms nothing.
	Head ChainHead
	// Now is the wall-clock time. It stamps what changes, and it is the
	// second of the two clocks a deadline is judged by (see Evaluate).
	Now time.Time
	// Deadline enables deadline-based settlement (expired, underpaid,
	// failed). The service sets it only while processing a new chain head,
	// because only then is it known that every transfer up to Head has been
	// reported.
	Deadline bool
	// Grace is added to ExpiresAt before the deadline counts as passed.
	Grace time.Duration
	// WatchWindow is how long the deposit address stays on the watch list
	// after the payment last changed, while the payment is funded
	// ([Payment.Funded]).
	WatchWindow time.Duration
	// UnfundedWatchWindow is the same for a payment that is not funded,
	// counted from its last change of status.
	UnfundedWatchWindow time.Duration
}

// Evaluation is the outcome of [Evaluate].
type Evaluation struct {
	// Payment is the payment after applying the rules.
	Payment Payment
	// Changed reports whether Payment differs from the input and must be
	// saved.
	Changed bool
	// Transfers holds the transfers that were promoted from pending to
	// confirmed and must be saved.
	Transfers []Transfer
	// Events lists the events to append, in order.
	Events []EventType
}

// Evaluate is the payment state machine. It is a pure function: given a
// payment, all of its transfers and the chain head, it returns the payment's
// new state, the transfers that just became final and the events to emit.
//
// Because the result is derived from the complete set of facts rather than
// from the fact that triggered the call, replayed, duplicated and
// out-of-order facts all converge on the same state, and re-evaluating an
// unchanged payment is a no-op (Changed == false, no events).
//
// It is pure in the strict sense: Now is an input like any other. The same
// facts evaluated again at the same Now give the same result; evaluated at
// a later Now they can only differ in what the wall clock is allowed to
// decide, which is whether a deadline has passed.
//
// Vocabulary. need = Amount − min(Tolerance, Amount/20). "Live" = not
// orphaned. The deadline is ExpiresAt + Grace. A live transfer is "late"
// when it provably entered the chain at or after the deadline (see
// [Transfer.After]: by chain time AND by the wall clock at which it was
// first seen); every other live transfer is "in time" — a transfer the
// core cannot date gets the benefit of the doubt. "Dust" is an amount below
// the payment's dust limit, min(1 whole unit, 5 % of Amount), see
// [DustDivisor]: dust is recorded and shown, and otherwise ignored.
//
// Rules:
//
//  1. A live pending transfer becomes confirmed (final) once its block is
//     at or below the head's final block ([ChainHead.Final]: finalised by
//     the chain as two endpoints attest it; on a network declared to have
//     no finality, beyond the adapter's reorganisation window) AND the head gives
//     it RequiredConfirmations; the including block counts as the first.
//     The confirmations are a floor, never a substitute for finality:
//     while the chain does not finalise the block, the transfer stays
//     pending however deep it is (the payment shows why:
//     [PaymentView.AwaitingFinality]). Nothing else confirms a transfer.
//  2. While the payment is open, the status follows the funds that arrived
//     in time:
//     confirmed ≥ need            → paid (or overpaid when confirmed > Amount)
//     else a transfer is pending  → confirming if the live total ≥ need,
//     else detected (only if the live total is not dust)
//     else deadline passed        → underpaid if confirmed funds are not dust,
//     failed if all that ever arrived was orphaned, else expired
//     else confirmed, not dust    → detected (partial payment, payer may top up)
//     else                        → awaiting
//     The deadline has passed when in.Deadline is set and BOTH clocks are
//     at or after it: the head's block time and Now. Chain time is required
//     so that a watcher that is behind cannot expire a payment that was
//     paid in time; the wall clock is required so that a wrong block
//     timestamp — one lying RPC endpoint is enough to produce one — can at
//     worst delay an expiry, never cause one early. A pending in-time
//     transfer that is not dust holds the payment open, so a payment never
//     expires mid-confirmation. Late transfers and dust never do, and never
//     pay anything: funds that arrive after the deadline are late funds
//     whether or not the payment had been settled by then, so the outcome
//     does not depend on how promptly heads were processed.
//  3. Once settled, the status is frozen. Sums keep following the chain.
//     Confirmed funds that the outcome does not account for are late funds:
//     whatever was confirmed too late to count, or in time but too little
//     to count, when the payment settled, and whatever becomes confirmed
//     afterwards. They raise [AttentionLateFunds], clear any resolution and
//     emit [EventPaymentLateFunds] as soon as their total — since the
//     payment settled, since late funds were last flagged, or since an
//     operator last resolved the issue, whichever is latest
//     ([Payment.LateFundsMark]) — is not dust. The total decides, not the
//     single transfer: sixty units that arrive in eighty pieces are flagged
//     like sixty units in one.
//  4. Dust never changes the status, raises attention or emits an event: a
//     payment that received only dust expires as if it had received
//     nothing (the amounts stay on the payment and its transfer list).
//     "Dust" is always a sum — the funds in question taken together — never
//     a single transfer.
//  5. Whenever the payment changes, its watch window is extended: by
//     WatchWindow if it is funded ([Payment.Funded]; funds that were
//     orphaned count too, they are usually mined again), else by
//     UnfundedWatchWindow and only when the status changed. Dust therefore
//     never keeps an address on the watch list.
//
// Every status change emits exactly one "payment.<status>" event; statuses
// can be skipped (awaiting → paid when a transfer is first reported with
// enough confirmations), so consumers must not expect intermediate events.
func Evaluate(p Payment, transfers []Transfer, in EvalInput) Evaluation {
	before := p.clone()
	p = p.clone()
	var out Evaluation

	sameChain := in.Head.ChainID == p.Asset.ChainID
	deadline := p.ExpiresAt.Add(in.Grace)
	// All live funds, for the record ...
	var received, confirmed Amount
	// ... and what decides: funds in time, late funds, orphaned funds.
	var inTime, inTimeConfirmed, lateConfirmed, orphaned Amount
	var pendingBlock, oldestPending uint64
	var first *Transfer
	pendingInTime := false

	for _, t := range transfers {
		if !t.Live() {
			orphaned = orphaned.Add(t.Amount)
			continue
		}
		if t.Status == TransferPending && sameChain && in.Head.final(t.BlockNumber, p.RequiredConfirmations) {
			t.Status = TransferConfirmed
			t.ConfirmedAt = in.Now
			out.Transfers = append(out.Transfers, t)
		}
		late := t.After(deadline)
		received = received.Add(t.Amount)
		if !late {
			inTime = inTime.Add(t.Amount)
		}
		if t.Status == TransferConfirmed {
			confirmed = confirmed.Add(t.Amount)
			if late {
				lateConfirmed = lateConfirmed.Add(t.Amount)
			} else {
				inTimeConfirmed = inTimeConfirmed.Add(t.Amount)
			}
		} else {
			pendingBlock = max(pendingBlock, t.BlockNumber)
			if oldestPending == 0 || t.BlockNumber < oldestPending {
				oldestPending = t.BlockNumber
			}
			if !late {
				pendingInTime = true
			}
		}
		if first == nil || t.BlockNumber < first.BlockNumber ||
			(t.BlockNumber == first.BlockNumber && t.LogIndex < first.LogIndex) {
			first = &t
		}
	}

	p.AmountReceived, p.AmountConfirmed, p.PendingBlock, p.OldestPendingBlock = received, confirmed, pendingBlock, oldestPending
	p.TxHash, p.FromAddress = "", ""
	if first != nil {
		p.TxHash, p.FromAddress = first.TxHash, first.From
	}

	need := p.Amount.Sub(effectiveTolerance(p.Amount, p.Tolerance))
	covers := func(a Amount) bool { return !a.IsZero() && a.Cmp(need) >= 0 }
	dust := dustLimit(p.Amount, p.Asset.Decimals)
	counts := func(a Amount) bool { return a.Cmp(dust) >= 0 }

	if before.Status.Open() {
		status := StatusAwaiting
		switch {
		case covers(inTimeConfirmed):
			status = StatusPaid
			if inTimeConfirmed.Cmp(p.Amount) > 0 {
				status = StatusOverpaid
			}
		case pendingInTime && counts(inTime):
			status = StatusDetected
			if covers(inTime) {
				status = StatusConfirming
			}
		case in.Deadline && sameChain && !in.Head.Time.IsZero() && !in.Head.Time.Before(deadline) && !in.Now.Before(deadline):
			switch {
			case counts(inTimeConfirmed):
				status = StatusUnderpaid
			case !counts(received) && counts(orphaned):
				status = StatusFailed
			default:
				status = StatusExpired
			}
		case counts(inTimeConfirmed):
			status = StatusDetected
		}
		if status != before.Status {
			p.Status = status
			out.Events = append(out.Events, statusEvent(status))
			if status.Settled() {
				p.SettledAt = in.Now
				switch status {
				case StatusPaid:
					p.PaidAt = in.Now
				case StatusOverpaid:
					p.PaidAt = in.Now
					p.Attention = AttentionOverpaid
				case StatusUnderpaid:
					p.Attention = AttentionUnderpaid
				case StatusFailed:
					p.Attention = AttentionFailed
				}
				// What the outcome accounts for: the funds confirmed in
				// time, if they decided it. A payment that expired or
				// failed was decided by nothing — whatever is confirmed on
				// it, in time (then it is dust) or late, is unaccounted
				// for, like everything that confirms from here on.
				p.LateFundsMark = inTimeConfirmed
				if status == StatusExpired || status == StatusFailed {
					p.LateFundsMark = Amount{}
				}
				if counts(confirmed.Sub(p.LateFundsMark)) {
					if p.Attention == AttentionNone {
						p.Attention = AttentionLateFunds
					}
					p.LateFundsMark = confirmed
					out.Events = append(out.Events, EventPaymentLateFunds)
				}
			}
		}
	} else if counts(confirmed.Sub(p.LateFundsMark)) {
		// Judged as a sum: what has been confirmed since the mark
		// was last set, however it was split. A replay finds nothing above
		// the mark and changes nothing.
		if p.Attention == AttentionNone {
			p.Attention = AttentionLateFunds
		}
		p.Resolution = nil
		p.LateFundsMark = confirmed
		out.Events = append(out.Events, EventPaymentLateFunds)
	}

	if !reflect.DeepEqual(before, p) {
		out.Changed = true
		p.UpdatedAt = in.Now
		window := time.Duration(0)
		switch {
		case p.Funded() || p.fundedBy(orphaned):
			// Orphaned funds count here: a transaction that a
			// reorganisation dropped is usually mined again.
			window = in.WatchWindow
		case len(out.Events) > 0:
			window = in.UnfundedWatchWindow
		}
		head := in.Head
		if head.ChainID != p.Asset.ChainID {
			head = ChainHead{}
		}
		if until := windowStart(in.Now, head).Add(window); window > 0 && until.After(p.WatchUntil) {
			p.WatchUntil = until
		}
	}
	out.Payment = p
	return out
}
