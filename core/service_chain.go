package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// This file is the chain adapter's side of the service. The adapter
// (internal/chain) only observes the chain and reports facts; the core
// alone decides what they mean. Every method here is system-only.
//
// The contract in one place:
//
//   - Learn what to watch from [Service.ChainState]; refresh it when a
//     payment.created or settings.updated event appears (see
//     [Service.Subscribe]) and periodically. An adapter that looks at the
//     chain less often while the state lists nothing open (see
//     [WatchedAddress.Closed]) must look at once when it does again.
//   - Report every Transfer log of the asset contract whose recipient is a
//     watched address with [Service.ObserveTransfer].
//   - Report transfers that left the canonical chain with
//     [Service.RetractTransfer].
//   - After — and only after — everything up to block N has been reported,
//     call [Service.AdvanceHead] with block N.
//   - Pass every fact, and every status report, with the revision of the
//     configuration it was made under ([ChainState.Revision],
//     [WithChainRevision]). Once the configuration has been replaced —
//     network, endpoints, token or finality depth changed, also away and
//     back — nothing of the old one is applied: the facts are refused with
//     [ErrConfigurationReplaced] in the transaction that would apply them
//     (revision.go). Rebuild from the current ChainState.
//
// All three fact methods are idempotent and may be replayed in any order;
// the only ordering rule is the one about AdvanceHead.

// TransferObservation is the fact "a token transfer to a watched address
// is in a block".
type TransferObservation struct {
	// ChainID and Contract identify the chain and token the log was read
	// from. The core checks them against the payment's asset and rejects
	// the fact with [ErrChainMismatch] otherwise, so that a transfer of
	// another token, or on another chain, can never be credited.
	ChainID  uint64
	Contract string
	// TxHash, LogIndex and BlockHash identify the log.
	TxHash    string
	LogIndex  uint
	BlockHash string
	// BlockNumber is the including block's height (must be > 0).
	BlockNumber uint64
	// BlockTime is the including block's timestamp. Optional, but worth
	// reporting: it is shown with the transfer, and it dates the transfer
	// for the deadline rule (a transfer in a block at or after the
	// payment's deadline is late funds, see [Evaluate]). Without it the
	// core dates the transfer by the heads it has processed
	// ([Transfer.NotBefore]), which is exact enough except for a transfer
	// reported in a block at or below the processed head.
	BlockTime time.Time
	// From is the sender; To the recipient (a deposit address).
	From string
	To   string
	// Amount is the transferred value in base units.
	Amount Amount
}

// ObserveTransfer records a transfer and re-evaluates the payment that
// owns the destination address.
//
// Idempotent: reporting the same (TxHash, LogIndex, BlockHash) again
// changes nothing and emits nothing. A transfer that was retracted comes
// back to life when it is observed again. Observing a transaction in a new
// block automatically orphans the records of the same transaction in other
// blocks (a transaction is in at most one canonical block), so a
// re-included transfer is never counted twice even if the watcher never
// retracts the stale one.
//
// The transfer counts from the moment it is observed; it becomes final
// when the head gives it the payment's required confirmations, which may
// be immediately if the head is already far enough.
//
// Errors: [ErrUnknownAddress] if no payment owns To (safe to ignore),
// [ErrChainMismatch] if the fact is for another chain or token,
// [ErrTransferFinal] if it contradicts a transfer that is already final.
// Zero-value transfers are ignored. System only.
func (s *Service) ObserveTransfer(ctx context.Context, obs TransferObservation) error {
	if _, err := authorize(ctx, permSystem); err != nil {
		return err
	}
	t, contract, err := normalizeObservation(obs)
	if err != nil {
		return err
	}
	if t.Amount.IsZero() {
		return nil
	}
	return s.update(ctx, func(u *unit) error {
		st, err := u.loadSettings(ctx)
		if err != nil {
			return err
		}
		if err := fence(ctx, st); err != nil {
			return err
		}
		p, err := u.GetPaymentByAddress(ctx, t.To)
		if errors.Is(err, ErrNotFound) {
			return newError(ErrUnknownAddress, "")
		}
		if err != nil {
			return err
		}
		if p.Asset.ChainID != obs.ChainID || p.Asset.Contract != contract {
			return newError(ErrChainMismatch, "")
		}
		affected := []string{p.ID}

		existing, err := u.GetTransfer(ctx, t.Key())
		switch {
		case err == nil:
			if existing.Status != TransferOrphaned {
				return nil // replay
			}
			existing.Status = TransferPending
			existing.OrphanedAt = time.Time{}
			if err := u.UpdateTransfer(ctx, existing); err != nil {
				return err
			}
			affected = appendUnique(affected, existing.PaymentID)
		case errors.Is(err, ErrNotFound):
			t.PaymentID = p.ID
			t.Status = TransferPending
			t.SeenAt = u.now
			// Everything up to the processed head had been reported
			// before that head was (the AdvanceHead contract), so a
			// transfer above it is in a later block, and no later block
			// is older than the head.
			if head, err := u.loadHead(ctx); err != nil {
				return err
			} else if head.ChainID == obs.ChainID && head.Number < t.BlockNumber {
				t.NotBefore = head.Time
			}
			if err := u.InsertTransfer(ctx, t); err != nil {
				return err
			}
		default:
			return err
		}

		// The same transaction in any other block is a stale fork.
		siblings, err := u.ListTransfersByTxHash(ctx, t.TxHash)
		if err != nil {
			return err
		}
		for _, sib := range siblings {
			if sib.BlockHash == t.BlockHash {
				continue
			}
			switch sib.Status {
			case TransferConfirmed:
				return newError(ErrTransferFinal, "transaction %s is already final in block %s", t.TxHash, sib.BlockHash)
			case TransferPending:
				sib.Status = TransferOrphaned
				sib.OrphanedAt = u.now
				if err := u.UpdateTransfer(ctx, sib); err != nil {
					return err
				}
				affected = appendUnique(affected, sib.PaymentID)
			}
		}
		return s.reevaluate(ctx, u, affected, false)
	})
}

func normalizeObservation(obs TransferObservation) (Transfer, string, error) {
	var v validation
	t := Transfer{
		LogIndex:    obs.LogIndex,
		BlockNumber: obs.BlockNumber,
		Amount:      obs.Amount,
	}
	if !obs.BlockTime.IsZero() {
		t.BlockTime = obs.BlockTime.UTC().Truncate(time.Millisecond)
	}
	var err error
	if t.TxHash, err = NormalizeHash(obs.TxHash); err != nil {
		v.add("tx_hash", "invalid", "tx hash must be a 0x-prefixed 32-byte hash")
	}
	if t.BlockHash, err = NormalizeHash(obs.BlockHash); err != nil {
		v.add("block_hash", "invalid", "block hash must be a 0x-prefixed 32-byte hash")
	}
	if t.From, err = NormalizeAddress(obs.From); err != nil {
		v.add("from", "invalid", "from must be a 0x-prefixed 20-byte address")
	}
	if t.To, err = NormalizeAddress(obs.To); err != nil {
		v.add("to", "invalid", "to must be a 0x-prefixed 20-byte address")
	}
	contract, err := NormalizeAddress(obs.Contract)
	if err != nil {
		v.add("contract", "invalid", "contract must be a 0x-prefixed 20-byte address")
	}
	if obs.BlockNumber == 0 {
		v.add("block_number", "required", "block number is required")
	}
	if obs.ChainID == 0 {
		v.add("chain_id", "required", "chain id is required")
	}
	return t, contract, v.errOrNil()
}

// RetractTransfer records that a previously observed transfer is no longer
// on the canonical chain (its block was reorganised out) and re-evaluates
// its payment: the funds stop counting, and an open payment falls back to
// detected or awaiting.
//
// Idempotent: retracting an unknown or already retracted transfer is a
// no-op. Retracting a transfer that is already final fails with
// [ErrTransferFinal] and changes nothing — finality is the operator's
// chosen confirmation depth, and the core never takes back a settlement.
// System only.
func (s *Service) RetractTransfer(ctx context.Context, key TransferKey) error {
	if _, err := authorize(ctx, permSystem); err != nil {
		return err
	}
	var err error
	if key.TxHash, err = NormalizeHash(key.TxHash); err != nil {
		return invalidField("tx_hash", "invalid", "tx hash must be a 0x-prefixed 32-byte hash")
	}
	if key.BlockHash, err = NormalizeHash(key.BlockHash); err != nil {
		return invalidField("block_hash", "invalid", "block hash must be a 0x-prefixed 32-byte hash")
	}
	return s.update(ctx, func(u *unit) error {
		st, err := u.loadSettings(ctx)
		if err != nil {
			return err
		}
		if err := fence(ctx, st); err != nil {
			return err
		}
		t, err := u.GetTransfer(ctx, key)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		switch t.Status {
		case TransferOrphaned:
			return nil
		case TransferConfirmed:
			return newError(ErrTransferFinal, "")
		}
		t.Status = TransferOrphaned
		t.OrphanedAt = u.now
		if err := u.UpdateTransfer(ctx, t); err != nil {
			return err
		}
		return s.reevaluate(ctx, u, []string{t.PaymentID}, false)
	})
}

// AdvanceHead records the newest block the chain adapter has fully
// processed and applies everything that follows from it: transfers that
// reached their required confirmations become final (settling payments as
// paid or overpaid), and open payments whose deadline has passed settle as
// expired, underpaid or failed.
//
// A deadline has passed only when both clocks say so: the head's block
// time and this server's wall clock (see [Evaluate]). A head whose block
// time is more than [Config.MaxHeadTimeAhead] (2 minutes by default) ahead
// of the wall clock is refused outright with [ErrHeadTimeAhead] and
// nothing is recorded: no honest block is from the future, so either the
// RPC endpoint is lying or the server clock is wrong, and the adapter
// should alert and switch endpoints rather than carry on.
//
// Guarantee required from the caller: every transfer to a watched address
// in any block up to and including head.Number has been reported with
// ObserveTransfer before this call. That is what makes it safe to expire
// payments here, and why a lagging watcher can never expire a payment that
// was paid in time.
//
// head.Time must be the block's timestamp, head.Checked the adapter's
// account of the blocks still to be scanned by a second log endpoint (zero
// if none). head.ChainID must equal the instance's chain id
// ([ErrChainMismatch] otherwise). Settled payments whose window the head has
// passed are retired here (see [ChainState.Addresses]). The head may move
// backwards (after a reorganisation, or when the adapter fails over to a
// node that is behind); that never un-confirms anything. Calling it again
// with the same head is a no-op. System only.
func (s *Service) AdvanceHead(ctx context.Context, head ChainHead) error {
	if _, err := authorize(ctx, permSystem); err != nil {
		return err
	}
	var v validation
	var err error
	if head.Hash, err = NormalizeHash(head.Hash); err != nil {
		v.add("hash", "invalid", "hash must be a 0x-prefixed 32-byte hash")
	}
	if head.Number == 0 {
		v.add("number", "required", "block number is required")
	}
	if head.Time.IsZero() {
		v.add("time", "required", "block time is required")
	}
	if err := v.errOrNil(); err != nil {
		return err
	}
	head.Time = head.Time.UTC().Truncate(time.Millisecond)
	head.Final = min(head.Final, head.Number)
	switch {
	case head.Final == head.Number:
		head.FinalTime = head.Time
	case head.FinalTime.After(head.Time):
		head.FinalTime = time.Time{} // not the final block's: not known
	default:
		head.FinalTime = head.FinalTime.UTC().Truncate(time.Millisecond)
	}
	if head.Checked = head.Checked.UTC().Truncate(time.Millisecond); !head.Checked.Before(head.Time) {
		head.Checked = time.Time{} // nothing outstanding
	}
	if now := s.now(); s.maxHeadAhead > 0 && head.Time.Sub(now) > s.maxHeadAhead {
		return newError(ErrHeadTimeAhead, "block %d carries a timestamp %s ahead of this server's clock; either the RPC endpoint is lying about block time or the server clock is wrong",
			head.Number, head.Time.Sub(now).Round(time.Second))
	}
	return s.update(ctx, func(u *unit) error {
		st, err := u.loadSettings(ctx)
		if err != nil {
			return err
		}
		if err := fence(ctx, st); err != nil {
			return err
		}
		if head.ChainID != st.Network.ChainID {
			return newError(ErrChainMismatch, "head is for chain %d, the instance runs chain %d", head.ChainID, st.Network.ChainID)
		}
		head.ObservedAt = u.now
		// What was final stays final: a head that says less (it went
		// back, or the adapter lost its records) does not lower it.
		if prev, err := u.loadHead(ctx); err != nil {
			return err
		} else if prev.ChainID == head.ChainID && prev.Number != 0 && prev.Final > head.Final {
			head.Final, head.FinalTime = prev.Final, prev.FinalTime
		}
		if err := u.SaveChainHead(ctx, head); err != nil {
			return err
		}
		u.head = &head

		var ids []string
		pending, err := u.ListPendingTransfers(ctx)
		if err != nil {
			return err
		}
		for _, t := range pending {
			ids = appendUnique(ids, t.PaymentID)
		}
		due, err := u.ListPayments(ctx, PaymentFilter{
			Statuses:      OpenStatuses,
			ChainID:       head.ChainID,
			Contract:      st.Asset.Contract,
			ExpiresBefore: earlier(head.Time, u.now).Add(-s.grace),
		}, PaymentSort{By: SortByCreated}, Page{})
		if err != nil {
			return err
		}
		for _, p := range due {
			ids = appendUnique(ids, p.ID)
		}
		if err := s.reevaluate(ctx, u, ids, true); err != nil {
			return err
		}
		return s.retire(ctx, u, head)
	})
}

// reevaluate runs the state machine for each payment and persists the
// outcome: promoted transfers, the payment, link statistics and events.
//
// A payment is judged by the processed head only if its asset is the one
// being watched — the network's chain and the configured token. The head
// says nothing about any other: a payment in a token the instance watched
// before (the token can be changed while settled payments still have
// transfers on the way) is not scanned for, so nobody would notice its
// transfer leave the chain, and its deadline and confirmations wait — like
// its address and its pending transfers ([ChainState]) — until that token is
// watched again.
func (s *Service) reevaluate(ctx context.Context, u *unit, paymentIDs []string, deadline bool) error {
	current, err := u.loadHead(ctx)
	if err != nil {
		return err
	}
	st, err := u.loadSettings(ctx)
	if err != nil {
		return err
	}
	for _, id := range paymentIDs {
		p, err := u.GetPayment(ctx, id)
		if err != nil {
			return err
		}
		transfers, err := u.ListTransfers(ctx, id)
		if err != nil {
			return err
		}
		head := current
		if !st.watches(p.Asset) {
			head = ChainHead{}
		}
		ev := Evaluate(p, transfers, EvalInput{
			Head:                head,
			Now:                 u.now,
			Deadline:            deadline,
			Grace:               s.grace,
			WatchWindow:         s.watchWindow,
			UnfundedWatchWindow: s.unfundedWatchWindow,
		})
		for _, t := range ev.Transfers {
			if err := u.UpdateTransfer(ctx, t); err != nil {
				return err
			}
		}
		if !ev.Changed {
			continue
		}
		if err := u.UpdatePayment(ctx, ev.Payment); err != nil {
			return err
		}
		if p.Status.Open() && !ev.Payment.PaidAt.IsZero() && ev.Payment.LinkID != "" {
			if err := creditLink(ctx, u, ev.Payment); err != nil {
				return err
			}
		}
		for _, typ := range ev.Events {
			if err := u.emitPayment(ctx, typ, ev.Payment); err != nil {
				return err
			}
		}
	}
	return nil
}

// creditLink adds a freshly paid payment to its link's statistics.
func creditLink(ctx context.Context, u *unit, p Payment) error {
	l, err := u.GetLink(ctx, p.LinkID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	l.Uses++
	l.TotalCollected = l.TotalCollected.Add(p.AmountConfirmed)
	l.LastPaidAt = u.now
	l.UpdatedAt = u.now
	return u.UpdateLink(ctx, l)
}

// earlier returns the earlier of two times.
func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// windowStart returns the moment a deadline or a watch window set now is
// counted from: the wall clock, or the processed head's block time if that
// is later. A deadline is judged by both clocks (the earlier of the two has
// to be past it), so one counted from the earlier of them would end early,
// in the chain's terms, by however far the clock is behind. How far that
// can be while payments are created is bounded by [Service.clockBehind].
func windowStart(now time.Time, head ChainHead) time.Time {
	if head.Time.After(now) {
		return head.Time
	}
	return now
}

// clockBehind returns how far the wall clock is behind the chain, when that
// is more than the tolerance for block times ahead of it
// ([Config.MaxHeadTimeAhead]); zero otherwise, and always zero while that
// check is switched off. The chain's time is the processed head's block
// time, or the newest block's by the chain adapter's latest report (as two
// endpoints show it: [ChainStatus.ClockSkew]).
func (s *Service) clockBehind(now time.Time, head ChainHead, st Settings) time.Duration {
	if s.maxHeadAhead <= 0 {
		return 0
	}
	behind := head.Time.Sub(now)
	if report := s.currentReport(st.Network.Revision); report != nil && report.ClockSkew < 0 && now.Sub(report.ReportedAt) < nodeStaleAfter {
		behind = max(behind, -report.ClockSkew)
	}
	if behind <= s.maxHeadAhead {
		return 0
	}
	return behind
}

func appendUnique(ids []string, id string) []string {
	if slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

// WatchedAddress is a deposit address the chain adapter must monitor.
type WatchedAddress struct {
	// Address is the deposit address, canonical lowercase.
	Address string
	// PaymentID is the payment that owns it.
	PaymentID string
	// SinceBlock is a block number before which no transfer to Address
	// can exist: the head the core knew when the payment was created. When
	// the adapter first learns of an address it must scan from SinceBlock
	// (not from its current position) so that a transfer mined between
	// creation and discovery is not missed. Every payment has one — none is
	// created before the core has processed a head ([ErrChainNotReady]) —
	// except payments earlier versions created before the first head: for
	// those it is 0, and the adapter falls back to the block at CreatedAt,
	// which is only as right as the server clock was at that moment.
	SinceBlock uint64
	// CreatedAt is when the payment was created.
	CreatedAt time.Time
	// Closed is true when the core is waiting for nothing at this address:
	// its payment is settled, and none of the transfers recorded on it is
	// pending. The address is on the list for one reason only — so that
	// money which arrives after the fact is seen (late funds).
	//
	// It tells the adapter how promptly it has to look, and nothing else:
	// a closed address is scanned for like any other, in every block, but
	// an adapter whose list holds closed addresses only may look at the
	// chain at a longer interval (the chain watcher: its idle interval,
	// 30 seconds by default, instead of every 3). What that delays is the
	// moment late funds on a payment that is already closed are noticed.
	// It cannot delay anything else, because of what "settled" means in
	// this state machine: a settled status never changes again, and
	// everything the core still does to a settled payment it does when a
	// transfer is reported or becomes final.
	//
	//   - paid, overpaid: the confirmed funds decided the outcome and are
	//     final. Money that arrives now is a second payment: late funds.
	//   - underpaid: the deadline has passed (that is what settled it); a
	//     top-up from here on does not complete the payment, it is late
	//     funds. While a short payment can still be topped up it is
	//     "detected", which is open.
	//   - expired, cancelled included: the payment window is closed.
	//     Whoever pays now pays late, by definition.
	//   - failed: the funds that had been seen were reorganised out and the
	//     deadline has passed. If their transaction is mined again it is
	//     late funds.
	//
	// An unresolved issue (needs attention), a resolution and a reopened
	// resolution are the operator's bookkeeping on a settled payment and
	// change nothing about what the chain can do to it. Late funds that
	// have been flagged are confirmed transfers: final.
	//
	// Never closed: a payment in an open status — awaiting (a payer may be
	// looking at its checkout, and its deadline is judged by the processed
	// head), detected, confirming (a confirmation count is on screen) —
	// and a settled payment for as long as a transfer on it is pending:
	// its confirmations are being counted, and it can still be retracted.
	Closed bool
}

// closed reports whether nothing but late funds can happen at a payment's
// address any more: see [WatchedAddress.Closed].
func closed(p Payment) bool {
	return p.Status.Settled() && p.PendingBlock == 0
}

// ChainState is everything the chain adapter needs to know from the core.
type ChainState struct {
	// Problem, when not empty, says why the network settings cannot be used
	// (NetworkSettings.Problems): the adapter runs nothing on them.
	Problem string
	// Revision identifies the chain configuration this state is of
	// (NetworkSettings.Revision): the adapter builds its generation for it
	// and passes it with every fact (WithChainRevision); facts of another
	// revision are refused (revision.go).
	Revision uint64
	// Network is the chain id and the ordered RPC endpoints to use.
	Network NetworkSettings
	// Asset is the token to watch: Transfer logs of Asset.Contract.
	Asset Asset
	// SetupCompleted is false until the instance is configured; there is
	// nothing to watch before that.
	SetupCompleted bool
	// RequiredConfirmations is the current finality depth, a lower bound
	// for how many recent blocks the adapter should keep re-checking.
	RequiredConfirmations int
	// Head is the last head passed to AdvanceHead: where to resume. Zero
	// on a fresh instance (start from the current chain tip).
	Head ChainHead
	// Addresses are the deposit addresses to watch: every open payment,
	// plus settled payments whose address has not been retired (so that
	// late funds are seen). The latter are marked [WatchedAddress.Closed]
	// once nothing on them is pending.
	//
	// A settled payment's address is retired — leaves the list for good —
	// only on evidence from the chain, in [Service.AdvanceHead]: once a
	// processed head of the payment's network and asset is past the end
	// of its window ([Payment.WatchUntil]) by [WatchRetirementMargin] of
	// block time, every block stamped before the end has been scanned by
	// two log endpoints ([ChainHead.Checked]), and the wall clock is past
	// it too. The window is a promise about blocks — a transfer in a block
	// stamped before it ends is seen — and the adapter looks at blocks
	// after they were mined: by up to its idle interval, after an outage
	// by however long that lasted, and after a change of network and back
	// not at all for the time another one was configured. A clock alone
	// never retires an address; with no processed head of the network,
	// nothing leaves the list.
	Addresses []WatchedAddress
	// PendingTransfers are the observed transfers that are not final yet.
	// The adapter must keep verifying that each is still in the block it
	// was reported in (BlockHash at BlockNumber) and retract it otherwise.
	PendingTransfers []Transfer
	// ReleasedUnchecked counts the payments of this network whose address
	// was retired although blocks of its window had not been scanned by a
	// second log endpoint ([Payment.RetiredUnchecked]); the adapter raises
	// an alert when it grows.
	ReleasedUnchecked int
}

// settledStatuses are the terminal statuses.
var settledStatuses = []PaymentStatus{StatusPaid, StatusUnderpaid, StatusOverpaid, StatusExpired, StatusFailed}

// retirementCutoffs returns, for a processed head and the wall clock, the
// moment a settled payment's window ([Payment.WatchUntil]) must lie at or
// before for its address to leave the watch list (retire), and the moment it
// would have to by the chain's progress alone (due) — later than retire
// while blocks are on record as settled on one endpoint's logs
// ([ChainHead.Checked]).
//
// The rule they implement: no address leaves the list while a block stamped
// before the end of its window may still be scanned — for the last time, or
// by the second log endpoint that makes up for blocks one endpoint settled
// alone. Block times do not decrease along the chain, so every block above
// the processed head is stamped at or after the head's time, and every block
// at or after Checked is too; an address whose window ended before both is
// owed nothing that is still to be scanned. Both clocks have to be past the
// window, the wall clock and the chain's, like for a deadline; and the
// chain's by [WatchRetirementMargin]. The chain's is the final block's
// ([ChainHead.FinalTime]), not the head's: a reorganisation that keeps
// every final block can replace the blocks above it, and move money into
// the window of an address that nobody would watch any more.
//
// known is false when a fact the rule needs is not known — the head, its
// time, the time of its final block (a head from a record that predates
// it, a final block whose header nobody has read yet): then nothing is past
// anything, and no address leaves the list. (A zero cutoff is not "no
// bound": callers must not use one.)
func retirementCutoffs(now time.Time, head ChainHead) (retire, due time.Time, known bool) {
	if head.Number == 0 || head.Time.IsZero() || now.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	// The chain's time an address's window is judged by: the final
	// block's, as far as the processed head has got — a block above the
	// final one can still be replaced, and money moved into the window by
	// a reorganisation that keeps every final block.
	final := head.FinalTime
	switch {
	case head.Final >= head.Number:
		final = head.Time // the head itself is final
	case head.Final == 0 || final.IsZero() || final.After(head.Time):
		return time.Time{}, time.Time{}, false
	}
	// Checked is zero when nothing is outstanding (ChainHead.Checked).
	checked := head.Checked
	if checked.IsZero() || checked.After(final) {
		checked = final
	}
	return earlier(now, checked.Add(-WatchRetirementMargin)), earlier(now, final.Add(-WatchRetirementMargin)), true
}

// retire records, for a processed head, which settled payments' addresses
// leave the watch list: those of the head's network and asset whose window
// has ended by every clock that has to say so (retirementCutoffs) and on
// which nothing is pending. It runs only in [Service.AdvanceHead]: an
// address leaves the list on evidence from a processed head of its network,
// never by a clock alone — with no head of that network (a fresh instance,
// another network configured for a while) nothing leaves — and that it has
// left is remembered ([Payment.RetiredAt]), so that a head that goes back, a
// restart or a change of network does not decide it again.
//
// Addresses kept only because blocks of their window are still to be
// scanned by a second log endpoint are bounded ([Config.MaxRetainedUnchecked]):
// beyond the limit the oldest are let go, those of payments through public
// links first, marked RetiredUnchecked.
func (s *Service) retire(ctx context.Context, u *unit, head ChainHead) error {
	st, err := u.loadSettings(ctx)
	if err != nil {
		return err
	}
	if head.ChainID != st.Asset.ChainID {
		return nil
	}
	retireBy, dueBy, known := retirementCutoffs(u.now, head)
	if !known {
		return nil // unknown is not "past": every address stays
	}
	notRetired := false
	leave := func(p Payment, unchecked bool) error {
		p.RetiredAt, p.RetiredUnchecked = u.now, unchecked
		return u.UpdatePayment(ctx, p)
	}
	settled := PaymentFilter{Statuses: settledStatuses, ChainID: head.ChainID, Contract: st.Asset.Contract, Retired: &notRetired}
	due := settled
	due.WatchEndsBy = retireBy
	list, err := u.ListPayments(ctx, due, PaymentSort{By: SortByCreated}, Page{})
	if err != nil {
		return err
	}
	for _, p := range list {
		if closed(p) {
			if err := leave(p, false); err != nil {
				return err
			}
		}
	}
	if s.maxRetained < 0 || !dueBy.After(retireBy) {
		return nil
	}
	retained := settled
	retained.WatchEndsBy = dueBy
	n, err := u.CountPayments(ctx, retained)
	if err != nil || n <= s.maxRetained {
		return err
	}
	kept, err := u.ListPayments(ctx, retained, PaymentSort{By: SortByCreated}, Page{})
	if err != nil {
		return err
	}
	kept = slices.DeleteFunc(kept, func(p Payment) bool { return !closed(p) })
	// Payments through public links go first, oldest first: anybody can
	// create those, in any number, and a flood of them must not take the
	// merchant's own invoices' coverage.
	slices.SortStableFunc(kept, func(a, b Payment) int {
		if fa, fb := a.LinkID != "", b.LinkID != ""; fa != fb {
			if fa {
				return -1
			}
			return 1
		}
		return a.WatchUntil.Compare(b.WatchUntil)
	})
	for len(kept) > s.maxRetained {
		if err := leave(kept[0], true); err != nil {
			return err
		}
		kept = kept[1:]
	}
	return nil
}

// Active reports whether the core is waiting for anything from the chain
// right now: an address that is not closed ([WatchedAddress.Closed]: an open
// payment, or a settled one with a transfer that is not final), or a pending
// transfer. While it is false, everything on the watch list is there for
// late funds only, and the adapter may look at the chain at its idle
// interval; the moment it is true the adapter has to look at once.
func (cs ChainState) Active() bool {
	if len(cs.PendingTransfers) > 0 {
		return true
	}
	for _, a := range cs.Addresses {
		if !a.Closed {
			return true
		}
	}
	return false
}

// ChainState returns what the chain adapter must watch and where to
// resume. Only payments of the instance's current chain are included.
// System only.
func (s *Service) ChainState(ctx context.Context) (ChainState, error) {
	var cs ChainState
	if _, err := authorize(ctx, permSystem); err != nil {
		return cs, err
	}
	err := s.store.View(ctx, func(r Reader) error {
		st, err := loadSettings(ctx, r)
		if err != nil {
			return err
		}
		cs.Revision = st.Network.Revision
		if st.SetupCompleted {
			cs.Problem = st.Network.problem()
		}
		cs.Network = st.Network
		cs.Asset = st.Asset
		cs.SetupCompleted = st.SetupCompleted
		cs.RequiredConfirmations = st.Payments.RequiredConfirmations
		if cs.Head, err = loadHead(ctx, r); err != nil {
			return err
		}
		if cs.Head.ChainID != st.Network.ChainID {
			cs.Head = ChainHead{}
		}
		open, err := r.ListPayments(ctx, PaymentFilter{Statuses: OpenStatuses, ChainID: st.Network.ChainID}, PaymentSort{By: SortByCreated}, Page{})
		if err != nil {
			return err
		}
		// Settled payments stay on the list until their address has been
		// retired — on evidence from a processed head of this network
		// (Service.retire): never by the clock, here.
		notRetired := false
		watched, err := r.ListPayments(ctx, PaymentFilter{Statuses: settledStatuses, Retired: &notRetired, ChainID: st.Network.ChainID, Contract: st.Asset.Contract}, PaymentSort{By: SortByCreated}, Page{})
		if err != nil {
			return err
		}
		seen := make(map[string]bool, len(open)+len(watched))
		for _, p := range append(open, watched...) {
			if seen[p.ID] || p.Asset.Contract != st.Asset.Contract {
				continue
			}
			seen[p.ID] = true
			cs.Addresses = append(cs.Addresses, WatchedAddress{
				Address: p.Address, PaymentID: p.ID, SinceBlock: p.CreatedAtBlock, CreatedAt: p.CreatedAt,
				Closed: closed(p),
			})
		}
		pending, err := r.ListPendingTransfers(ctx)
		if err != nil {
			return err
		}
		for _, t := range pending {
			p, err := r.GetPayment(ctx, t.PaymentID)
			if err != nil {
				return err
			}
			// Like the addresses: what is pending in another token (or on
			// another chain) waits for that one to be watched again.
			if st.watches(p.Asset) {
				cs.PendingTransfers = append(cs.PendingTransfers, t)
			}
		}
		cs.ReleasedUnchecked, err = r.CountPayments(ctx, PaymentFilter{ChainID: st.Network.ChainID, Contract: st.Asset.Contract, RetiredUnchecked: true})
		return err
	})
	return cs, err
}

// ChainStatus is the chain adapter's health report.
//
// Every endpoint named in it — Endpoints[].URL, Primary, Verifier,
// LogVerifier — is in a form fit to be shown to any reader of the node
// status: the adapter redacts URLs (scheme and host), because provider URLs
// carry keys.
type ChainStatus struct {
	// Starting is true while the adapter has started (or restarted) and
	// has not completed its first pass over the chain yet: it does not
	// know yet what it would report. The report says so instead of
	// reporting a fault, and the node is not healthy meanwhile.
	Starting bool
	// Idle is true while the core is waiting for nothing from the chain —
	// no payment is open, no transfer is not final ([ChainState.Active] is
	// false) — and the adapter therefore looks at the chain at its idle
	// interval instead of every few seconds. The processed head then trails
	// LatestBlock by up to that interval's worth of blocks: that distance is
	// the pause, not lag, and [Service.NodeStatus] does not count it against
	// the node's health. The adapter must say Idle only while its idle
	// passes keep succeeding on schedule; one that has stopped is lagging.
	Idle bool
	// Quiet says which kind of idle it is. False: the watch list is empty.
	// True: the list holds addresses, all of them of closed payments
	// ([WatchedAddress.Closed]), and every idle pass scans for them — late
	// funds are noticed, up to an idle interval later than they would be by
	// an adapter at work. Only meaningful together with Idle.
	Quiet bool
	// LatestBlock is the newest block number seen on the network, which
	// may be ahead of the processed head. With several endpoints it should
	// be a block at least two of them have: what a single endpoint claims
	// about the height of the chain is that endpoint's claim.
	LatestBlock uint64
	// Endpoints is the state of each configured RPC endpoint.
	Endpoints []RPCEndpointStatus
	// Primary is the endpoint the last pass read the chain from, Verifier
	// the one that cross-checked it ("" if none did: a single endpoint, or
	// no second one available), and LogVerifier the one whose log scan
	// confirmed the blocks settled most recently ("" if no log scan needed
	// confirming yet, or nobody confirmed it: see LogsUnchecked).
	Primary     string
	Verifier    string
	LogVerifier string
	// Solo is true while several endpoints are configured and one is
	// believed on its own word, because every other one is unreachable
	// (best-effort verification only).
	Solo bool
	// LogsUnchecked is true when the most recent blocks were settled on a
	// single endpoint's logs although several endpoints are configured.
	LogsUnchecked bool
	// Hold says why the processed head is not moving although there are
	// blocks to settle ("" if it is not held).
	Hold string
	// Work says what the chain adapter is busy with when it is not at rest
	// — a re-scan under way, transfers waiting for a second endpoint,
	// blocks still to be scanned by a second log endpoint and what that
	// waits for — ("" at rest). Unlike Hold it is not a fault.
	Work string
	// Warnings are the conditions the operator should look at right now
	// (clock skew, a held head, unchecked log scans, single-source
	// operation, failing scans), in words.
	Warnings []string
	// ClockSkew is the server clock minus the timestamp of the chain's
	// newest block, with several endpoints the smaller difference of two
	// of them (zero if not known). A clock behind the chain by more than
	// [Config.MaxHeadTimeAhead] makes payment creation fail with
	// [ErrClockBehind]: deadlines would be set early.
	ClockSkew time.Duration
	// ReportedAt is when the core received the report (set by the core).
	ReportedAt time.Time
	// The chain configuration the report was made under (set by the core
	// from the reporter's context: WithChainRevision).
	revision uint64
	fenced   bool
}

// RPCEndpointStatus is the health of one RPC endpoint.
type RPCEndpointStatus struct {
	URL string
	// Healthy: the endpoint answers and is not lagging.
	Healthy bool
	// InUse: the adapter is currently reading from this endpoint.
	InUse bool
	// State is the adapter's verdict in one word: EndpointHealthy,
	// EndpointNotChecked, EndpointUnverified, EndpointFailing,
	// EndpointLagging, EndpointWrongChain, EndpointUnsupported or
	// EndpointIdle.
	State       string
	LatencyMs   int
	BlockNumber uint64
	// Error is the last error, if the endpoint is unhealthy, or what
	// limits it (it serves no logs, say). Empty for an endpoint that has
	// simply not been asked anything yet.
	Error string
}

// Values of [RPCEndpointStatus.State].
const (
	// EndpointHealthy: verified chain id, answering, not lagging.
	EndpointHealthy = "healthy"
	// EndpointNotChecked: not asked anything yet (the adapter is
	// starting). Not a fault.
	EndpointNotChecked = "not_checked"
	// EndpointUnverified: asked, but its chain id has not been verified:
	// it has not answered.
	EndpointUnverified = "unverified"
	// EndpointFailing: paused after consecutive failures.
	EndpointFailing = "failing"
	// EndpointLagging: answering, but too far behind the others.
	EndpointLagging = "lagging"
	// EndpointWrongChain: reports another chain id.
	EndpointWrongChain = "wrong_chain"
	// EndpointUnsupported: a URL the adapter cannot use (a websocket URL).
	EndpointUnsupported = "unsupported"
	// EndpointIdle: configured, but nothing runs on it: the network
	// configuration as a whole cannot be used (Error says why).
	EndpointIdle = "idle"
)

// ReportChainStatus stores the chain adapter's latest health report, which
// [Service.NodeStatus] serves to the dashboard. The report is kept in
// memory only: it describes a running process, and after a restart "no
// report yet" is the truthful answer. The adapter should report every few
// seconds. System only.
func (s *Service) ReportChainStatus(ctx context.Context, status ChainStatus) error {
	if _, err := authorize(ctx, permSystem); err != nil {
		return err
	}
	status.Endpoints = slices.Clone(status.Endpoints)
	status.Warnings = slices.Clone(status.Warnings)
	status.ReportedAt = s.now()
	status.revision, status.fenced = chainRevision(ctx)
	s.statusMu.Lock()
	s.chainStatus = &status
	s.statusMu.Unlock()
	return nil
}

// currentReport returns the chain adapter's latest status report if it was
// made under the chain configuration of the given revision (or not fenced
// at all); nil otherwise: a report of a replaced configuration says nothing
// about this one.
func (s *Service) currentReport(revision uint64) *ChainStatus {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if r := s.chainStatus; r != nil && (!r.fenced || r.revision == revision) {
		return r
	}
	return nil
}

// nodeStaleAfter is how old a chain status report may be before the node
// counts as unhealthy; nodeMaxLag is how many blocks the processed head
// may trail the network tip.
const (
	nodeStaleAfter = time.Minute
	nodeMaxLag     = 50
)

// NodeStatus is the sync state shown in the dashboard.
type NodeStatus struct {
	// Healthy: a fresh report exists, the chain adapter is not starting,
	// the endpoint in use is healthy and the watcher is not lagging — or is
	// idle (see Idle, of which Quiet is a kind), which is when the lag is
	// not a fault.
	Healthy bool
	// Starting: the chain adapter has (re)started and not completed its
	// first pass over the chain yet — normally a second or two after the
	// server starts or the network settings change. Healthy is false
	// meanwhile, but nothing is known to be wrong; the endpoints show what
	// they have been asked so far. A Starting that lasts much longer than
	// a minute is a fault (the first pass hangs). False when the last
	// report is stale.
	Starting bool
	// Idle: the core is waiting for nothing from the chain (no payment is
	// open, no transfer is not final) and the chain adapter looks at the
	// chain at its idle interval — every 30 seconds by default — until a
	// payment is created, which wakes it at once. Head and LagBlocks then
	// show that pause: Head is up to an idle interval old, LagBlocks up to
	// that many blocks, and neither makes the node unhealthy. The endpoints
	// are still checked, and an idle pass that fails or does not come ends
	// Idle. False when the last report is stale.
	Idle bool
	// Quiet: idle with a watch list that is not empty. Every address on it
	// belongs to a closed payment — settled, nothing pending — and is
	// scanned for by every idle pass, so that late funds are noticed (up to
	// an idle interval after they are mined; the adapter then follows their
	// confirmations at its normal pace, and Idle and Quiet are false until
	// they are final). False while Idle is false, and while the watch list
	// is empty: Idle without Quiet means there is nothing to scan for.
	Quiet bool
	// Head is the last block the core processed.
	Head ChainHead
	// LatestBlock is the network tip from the last report (0 if none): the
	// newest block two endpoints have, as of their last health check or the
	// adapter's last pass. It keeps moving while the adapter idles.
	LatestBlock uint64
	// LagBlocks is how far Head trails LatestBlock. While Idle it is the
	// number of blocks since the adapter's last pass and is not counted
	// against Healthy.
	LagBlocks uint64
	// RPCLatencyMs is the latency of the endpoint in use.
	RPCLatencyMs int
	// LastCheckedAt is when the last report arrived; zero if none.
	LastCheckedAt time.Time
	// Endpoints is the per-endpoint detail from the last report.
	Endpoints []RPCEndpointStatus
	// Primary, Verifier, LogVerifier, Solo, LogsUnchecked, Hold, Work and
	// Warnings are those of the last report: which endpoint is read from,
	// which one verifies it, which one confirms its log scans, whether one
	// endpoint is believed alone, what the adapter is busy with, and what is
	// wrong right now (see [ChainStatus]). Endpoints appear redacted, as in
	// Endpoints.
	Primary       string
	Verifier      string
	LogVerifier   string
	Solo          bool
	LogsUnchecked bool
	Hold          string
	Work          string
	Warnings      []string
	// WatchedAddresses is the current size of the chain watch list.
	// WatchedLinkAddresses is the part of it that belongs to payments
	// created through public payment links, and MaxWatchedAddresses the
	// cap on that part (0 = none). WatchListFull is true at the cap:
	// public payment links then refuse new payments ([ErrWatchListFull])
	// until addresses age out — worth an alert. Payments the merchant
	// creates are not capped and do not count against the cap.
	WatchedAddresses     int
	WatchedLinkAddresses int
	MaxWatchedAddresses  int
	WatchListFull        bool
	// SameProvider lists the configured RPC endpoints that look like one
	// provider ([NetworkSettings.SameProvider]: their hosts share a
	// registrable domain), in groups, as scheme and host. Two endpoints of
	// one provider are one opinion; each group is also a sentence in
	// Warnings. Empty when nothing looks so.
	SameProvider [][]string
	// NetworkProblem, when not empty, says why the stored network settings
	// cannot be used (NetworkSettings.Problems): no chain watcher runs on
	// them and no payment is created until they are repaired. It is also
	// the first of Warnings, and the node is not healthy.
	NetworkProblem string
	// RetainedUnchecked is the number of settled payments' addresses that
	// stay on the watch list past their window only because blocks of it
	// are still to be scanned by a second log endpoint
	// ([ChainHead.Checked]); ReleasedUnchecked the number that were let go
	// all the same, because the number kept had reached its limit
	// ([Config.MaxRetainedUnchecked], [Payment.RetiredUnchecked]). A
	// payment hidden in those blocks for one of the latter is not found
	// when the blocks are scanned again.
	RetainedUnchecked int
	ReleasedUnchecked int
	// AwaitingFinality is set while payments have the confirmations they
	// need but wait for the chain to finalise the block of a transfer
	// ([PaymentView.AwaitingFinality]): a transfer counts towards paid
	// only at or below Head.Final. It is also a sentence in Warnings. Nil
	// while no payment waits for that.
	AwaitingFinality *FinalityWait
}

// FinalityWait is [NodeStatus.AwaitingFinality].
type FinalityWait struct {
	// Payments is the number of payments that wait.
	Payments int
	// Block is the lowest block they wait for: the next one whose
	// finalisation confirms something.
	Block uint64
	// Final is the newest final block ([ChainHead.Final]).
	Final uint64
}

// finalityWait returns NodeStatus.AwaitingFinality.
func finalityWait(ctx context.Context, r Reader, st Settings, head ChainHead) (*FinalityWait, error) {
	if head.Number == 0 {
		return nil, nil
	}
	pending, err := r.ListPendingTransfers(ctx)
	if err != nil {
		return nil, err
	}
	var w FinalityWait
	counted := map[string]bool{}
	for _, t := range pending {
		if t.BlockNumber <= head.Final || counted[t.PaymentID] {
			continue
		}
		p, err := r.GetPayment(ctx, t.PaymentID)
		if err != nil {
			return nil, err
		}
		if !st.watches(p.Asset) || !head.awaitingFinality(t.BlockNumber, p.RequiredConfirmations) {
			continue
		}
		counted[t.PaymentID] = true
		w.Payments++
		if w.Block == 0 || t.BlockNumber < w.Block {
			w.Block = t.BlockNumber
		}
	}
	if w.Payments == 0 {
		return nil, nil
	}
	w.Final = head.Final
	return &w, nil
}

// uncheckedCounts returns NodeStatus.RetainedUnchecked and
// ReleasedUnchecked.
func uncheckedCounts(ctx context.Context, r PaymentReader, st Settings, head ChainHead, now time.Time) (retained, released int, err error) {
	if released, err = r.CountPayments(ctx, PaymentFilter{ChainID: st.Network.ChainID, Contract: st.Asset.Contract, RetiredUnchecked: true}); err != nil {
		return 0, 0, err
	}
	no := false
	if head.ChainID != st.Network.ChainID || head.Number == 0 {
		return 0, released, nil
	}
	retireBy, dueBy, known := retirementCutoffs(now, head)
	if !known || !dueBy.After(retireBy) {
		return 0, released, nil
	}
	kept, err := r.ListPayments(ctx, PaymentFilter{Statuses: settledStatuses, ChainID: st.Network.ChainID, Contract: st.Asset.Contract, Retired: &no, WatchEndsBy: dueBy}, PaymentSort{By: SortByCreated}, Page{})
	if err != nil {
		return 0, 0, err
	}
	for _, p := range kept {
		if p.WatchUntil.After(retireBy) && closed(p) {
			retained++
		}
	}
	return retained, released, nil
}

// NodeStatus reports how the chain watcher is doing. Requires
// [ScopePaymentsRead].
func (s *Service) NodeStatus(ctx context.Context) (NodeStatus, error) {
	var ns NodeStatus
	if _, err := authorize(ctx, permScope(ScopePaymentsRead)); err != nil {
		return ns, err
	}
	var providerWarnings []string
	var revision uint64
	err := s.store.View(ctx, func(r Reader) error {
		st, err := loadSettings(ctx, r)
		if err != nil {
			return err
		}
		if ns.Head, err = loadHead(ctx, r); err != nil {
			return err
		}
		if ns.Head.ChainID != st.Network.ChainID {
			ns.Head = ChainHead{}
		}
		ns.SameProvider = st.Network.SameProvider()
		if st.SetupCompleted {
			ns.NetworkProblem = st.Network.problem()
		}
		providerWarnings = st.Network.Warnings()
		revision = st.Network.Revision
		fromLink := true
		if ns.WatchedAddresses, err = watchedCount(ctx, r, st.Asset, nil); err != nil {
			return err
		}
		if ns.WatchedLinkAddresses, err = watchedCount(ctx, r, st.Asset, &fromLink); err != nil {
			return err
		}
		ns.RetainedUnchecked, ns.ReleasedUnchecked, err = uncheckedCounts(ctx, r, st, ns.Head, s.now())
		if err != nil {
			return err
		}
		ns.AwaitingFinality, err = finalityWait(ctx, r, st, ns.Head)
		return err
	})
	if err != nil {
		return ns, err
	}
	if w := ns.AwaitingFinality; w != nil {
		providerWarnings = append(providerWarnings, fmt.Sprintf(
			"%d payment(s) have their confirmations and are waiting for the chain to finalise block %d (the newest final block is %d); they confirm when it does, and stay confirming while it does not",
			w.Payments, w.Block, w.Final))
	}
	if ns.RetainedUnchecked > 0 {
		limit := "no limit"
		if s.maxRetained > 0 {
			limit = fmt.Sprintf("at most %d are kept", s.maxRetained)
		}
		providerWarnings = append(providerWarnings, fmt.Sprintf(
			"%d settled payment address(es) stay on the watch list past their window until blocks settled on one endpoint's logs have been scanned by a second log endpoint (%s)",
			ns.RetainedUnchecked, limit))
	}
	if ns.WatchedAddresses >= WatchListWarning {
		providerWarnings = append(providerWarnings, fmt.Sprintf(
			"%d addresses are on the chain watch list: each scan asks every endpoint about them, %d to a query; payments the merchant creates have no cap, and no address leaves the list while the processed head does not move",
			ns.WatchedAddresses, logAddressesPerQuery))
	}
	if s.maxWatched > 0 {
		ns.MaxWatchedAddresses = s.maxWatched
		ns.WatchListFull = ns.WatchedLinkAddresses >= s.maxWatched
	}
	if ns.NetworkProblem != "" {
		providerWarnings = append([]string{"the network settings cannot be used: " + ns.NetworkProblem +
			" — no chain watcher runs and no payment can be created until they are fixed in the network settings"}, providerWarnings...)
	}
	report := s.currentReport(revision)
	if report == nil {
		ns.Warnings = providerWarnings
		return ns, nil
	}
	ns.LatestBlock = report.LatestBlock
	ns.LastCheckedAt = report.ReportedAt
	ns.Endpoints = slices.Clone(report.Endpoints)
	ns.Primary, ns.Verifier, ns.LogVerifier = report.Primary, report.Verifier, report.LogVerifier
	ns.Solo, ns.LogsUnchecked, ns.Hold, ns.Work = report.Solo, report.LogsUnchecked, report.Hold, report.Work
	ns.Warnings = append(slices.Clone(report.Warnings), providerWarnings...)
	if ns.LatestBlock > ns.Head.Number {
		ns.LagBlocks = ns.LatestBlock - ns.Head.Number
	}
	inUseHealthy := false
	for _, e := range report.Endpoints {
		if e.InUse {
			ns.RPCLatencyMs = e.LatencyMs
			inUseHealthy = e.Healthy
		}
	}
	fresh := s.now().Sub(report.ReportedAt) <= nodeStaleAfter
	ns.Starting = report.Starting && fresh
	ns.Idle = report.Idle && !report.Starting && fresh
	ns.Quiet = ns.Idle && report.Quiet
	// An idle adapter is not lagging: the distance to the tip is its pause
	// between passes. Everything else is asked of it as of a busy one.
	ns.Healthy = !report.Starting && inUseHealthy && fresh && (ns.LagBlocks <= nodeMaxLag || ns.Idle) && ns.NetworkProblem == ""
	return ns, nil
}
