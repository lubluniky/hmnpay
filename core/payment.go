package core

import (
	"maps"
	"time"
)

// PaymentStatus is the state of a [Payment]. The vocabulary is shared with
// every transport and with webhook event names ("payment." + status).
type PaymentStatus string

// Payment statuses. See the package documentation for the state machine.
const (
	// StatusAwaiting: open; no funds seen on the deposit address.
	StatusAwaiting PaymentStatus = "awaiting"
	// StatusDetected: open; funds were seen, but the total seen so far is
	// short of the amount due. The payer may still top up.
	StatusDetected PaymentStatus = "detected"
	// StatusConfirming: open; the funds seen cover the amount due and are
	// collecting confirmations.
	StatusConfirming PaymentStatus = "confirming"
	// StatusPaid: settled; confirmed funds cover the amount (within the
	// underpayment tolerance) and do not exceed it.
	StatusPaid PaymentStatus = "paid"
	// StatusUnderpaid: settled; the deadline passed with confirmed funds
	// short of the amount due (and more than dust).
	StatusUnderpaid PaymentStatus = "underpaid"
	// StatusOverpaid: settled; confirmed funds exceed the amount.
	StatusOverpaid PaymentStatus = "overpaid"
	// StatusExpired: settled; the payment window closed with nothing paid
	// in time (dust does not count, see [DustDivisor]) — because the
	// deadline passed, or because the payment was cancelled (then
	// [Payment.CancelledAt] is set).
	StatusExpired PaymentStatus = "expired"
	// StatusFailed: settled; the deadline passed and the funds that had
	// been seen were removed from the chain by a reorganisation.
	StatusFailed PaymentStatus = "failed"
)

// PaymentStatuses lists every status, in lifecycle order.
var PaymentStatuses = []PaymentStatus{
	StatusAwaiting, StatusDetected, StatusConfirming,
	StatusPaid, StatusUnderpaid, StatusOverpaid, StatusExpired, StatusFailed,
}

// OpenStatuses are the statuses in which a payment still follows the chain.
var OpenStatuses = []PaymentStatus{StatusAwaiting, StatusDetected, StatusConfirming}

// Valid reports whether s is a known status.
func (s PaymentStatus) Valid() bool {
	switch s {
	case StatusAwaiting, StatusDetected, StatusConfirming, StatusPaid,
		StatusUnderpaid, StatusOverpaid, StatusExpired, StatusFailed:
		return true
	}
	return false
}

// Open reports whether the payment can still change status.
func (s PaymentStatus) Open() bool {
	return s == StatusAwaiting || s == StatusDetected || s == StatusConfirming
}

// Settled reports whether the status is terminal. A settled payment never
// changes status again; later chain activity is recorded on it and raises
// [AttentionLateFunds] instead.
func (s PaymentStatus) Settled() bool { return s.Valid() && !s.Open() }

// Attention says why a human should look at a settled payment. It is set by
// the core's rules and never cleared; whether the issue is still open is
// [Payment.NeedsAttention].
type Attention string

// Attention reasons.
const (
	// AttentionNone: nothing to look at.
	AttentionNone Attention = ""
	// AttentionUnderpaid: settled short of the amount due.
	AttentionUnderpaid Attention = "underpaid"
	// AttentionOverpaid: settled with more than the amount.
	AttentionOverpaid Attention = "overpaid"
	// AttentionFailed: the transfer was reorganised out and never returned.
	AttentionFailed Attention = "failed"
	// AttentionLateFunds: funds were confirmed after the payment settled
	// (late payment on an expired invoice, a second payment, a late top-up).
	AttentionLateFunds Attention = "late_funds"
)

// Resolution records an operator's decision to close an issue.
type Resolution struct {
	// At is when the payment was marked resolved.
	At time.Time
	// By names who resolved it (the actor's display name).
	By string
	// Note is the operator's free-text explanation, possibly empty.
	Note string
}

// Payment is one invoice: an amount due on a dedicated deposit address.
//
// Store implementations persist every field verbatim. Fields in the
// "derived" group are recomputed by the core from the payment's transfers
// and written back; stores must not derive them themselves.
type Payment struct {
	// ID is "pay_" plus 26 random characters. It is a capability: the hosted
	// checkout URL contains it.
	ID string
	// Status is the current state; only the core changes it.
	Status PaymentStatus

	// Amount is the amount due, in base units of Asset.
	Amount Amount
	// Asset is a snapshot of the instance asset at creation.
	Asset Asset
	// Address is the deposit address in canonical lowercase form. Unique
	// across all payments.
	Address string
	// AddressIndex is the index the address was allocated at.
	AddressIndex uint32

	// OrderID is the merchant's reference. Not unique: a merchant may issue
	// a second payment for the same order after the first one expired.
	OrderID string
	// Description is shown on the checkout page.
	Description string
	// CustomerEmail is optional.
	CustomerEmail string
	// RedirectURL is where the checkout sends the customer afterwards.
	RedirectURL string
	// Metadata is the merchant's own key/value data; never nil after a read.
	Metadata map[string]string
	// LinkID is the payment link this payment was created from, if any.
	LinkID string
	// IdempotencyKey deduplicates creation. Unique across payments when
	// non-empty; any number of payments may have an empty key.
	IdempotencyKey string

	// RequiredConfirmations is the snapshot of the confirmation setting.
	RequiredConfirmations int
	// Tolerance is the underpayment tolerance that applies to this payment,
	// in base units: confirmed funds of at least Amount-Tolerance count as
	// paid. It is the setting at creation, capped at 5 % of Amount
	// ([ToleranceCapDivisor]); [Evaluate] applies the cap again, so a
	// payment stored before the cap existed is judged by the same rule.
	Tolerance Amount

	// Derived from transfers (see [Evaluate]):

	// AmountReceived is the sum of all live (not orphaned) transfers,
	// confirmed or not.
	AmountReceived Amount
	// AmountConfirmed is the sum of confirmed transfers.
	AmountConfirmed Amount
	// TxHash and FromAddress describe the earliest live transfer, for
	// display and search; empty when there is none.
	TxHash      string
	FromAddress string
	// PendingBlock is the highest block number among live unconfirmed
	// transfers, or 0 when nothing is pending. It lets list views compute
	// confirmations without loading transfers. OldestPendingBlock is the
	// lowest: the pending transfer with the most confirmations, the first
	// to be final ([PaymentView.AwaitingFinality]).
	PendingBlock       uint64
	OldestPendingBlock uint64

	// CreatedAtBlock is the start block of the address's scan: the highest
	// block the chain adapter treated as beyond reorganisation when the
	// payment was created ([ChainHead.Final]; block 1 if that was 0). No
	// transfer to Address can be in
	// it or an earlier block — the address did not exist when it was
	// mined, and no reorganisation the adapter accepts replaces it — so it
	// is the watcher's safe lower bound for back-scanning, on every path:
	// at a restart with or without its checkpoint, and in the re-scan of a
	// reorganisation. Every payment has one: none is created before the
	// core has processed a head of its network ([ErrChainNotReady]). 0
	// only on payments that earlier versions created before the first head.
	CreatedAtBlock uint64

	// LateFundsMark is the part of AmountConfirmed that has been accounted
	// for on a settled payment: by the outcome it settled with (the funds
	// that were confirmed in time when it settled as paid, overpaid or
	// underpaid), by a late-funds issue that was raised for it, or by an
	// operator's resolution. Confirmed funds beyond the mark are funds
	// nobody has been told about yet; they are flagged — [AttentionLateFunds],
	// payment.late_funds — as soon as they add up to the dust limit, however
	// many transfers they arrived in. Zero and unused while the payment is
	// open.
	LateFundsMark Amount

	// Attention is why the payment needs a human, if it does.
	Attention Attention
	// Resolution is set while an operator has marked the issue resolved.
	Resolution *Resolution

	// CreatedAt is the creation time.
	CreatedAt time.Time
	// ExpiresAt is the payment deadline, judged in chain time.
	ExpiresAt time.Time
	// UpdatedAt changes whenever any field changes.
	UpdatedAt time.Time
	// PaidAt is when the payment settled as paid or overpaid; zero
	// otherwise.
	PaidAt time.Time
	// SettledAt is when the payment reached a terminal status; zero while
	// open.
	SettledAt time.Time
	// CancelledAt is when the payment was cancelled with
	// [Service.CancelPayment]; zero otherwise. A cancelled payment has
	// status expired — cancelling is closing the payment window early —
	// and CancelledAt is what tells the two apart.
	CancelledAt time.Time
	// CancelledBy names who cancelled it (the actor's display name).
	CancelledBy string
	// WatchUntil is how long the watcher keeps scanning Address after the
	// payment stops being open. Open payments are always watched. A
	// payment that is not funded gets the short window
	// ([DefaultUnfundedWatchWindow]) from its last change of status; a
	// funded one ([Payment.Funded]; funds that were reorganised out count)
	// gets the long one ([DefaultWatchWindow]) from its last change.
	WatchUntil time.Time
	// RetiredAt is when the address left the chain watch list for good;
	// zero while it is on the list, or may come back to it. It is set only
	// on evidence from the chain: when a processed head of the payment's
	// network is past WatchUntil (by [WatchRetirementMargin]) and every
	// block of the window has been scanned by two log endpoints (see
	// [ChainHead.Checked]) — never by a clock alone, and never while no
	// head of that network is processed.
	RetiredAt time.Time
	// RetiredUnchecked is set when the address was let go although blocks
	// of its window had not been scanned by a second log endpoint yet: the
	// number of addresses kept for that reason had reached its limit
	// ([Config.MaxRetainedUnchecked]).
	RetiredUnchecked bool
}

// startBlock returns the start block of the scan of an address created now
// ([Payment.CreatedAtBlock]): the head's final block, or the head itself if
// the final block is above it (the head went back) — never 0, which means
// "no start block".
func (h ChainHead) startBlock() uint64 { return max(min(h.Final, h.Number), 1) }

// final reports whether a transfer in block is final under the head with the
// required confirmations: the block is at or below the head's final block,
// and the head gives it that many confirmations (the including block counts
// as the first).
func (h ChainHead) final(block uint64, required int) bool {
	return block != 0 && block <= h.Final && confirmations(block, h.Number, required) >= required
}

// awaitingFinality reports whether a transfer in block has the required
// confirmations under the head but is not final, because the chain has not
// finalised its block yet.
func (h ChainHead) awaitingFinality(block uint64, required int) bool {
	return block > h.Final && confirmations(block, h.Number, required) >= required
}

// NeedsAttention reports whether the payment has an issue that no operator
// has resolved yet. Stores index this value for the "needs attention"
// filter ([PaymentFilter.NeedsAttention]).
func (p Payment) NeedsAttention() bool {
	return p.Attention != AttentionNone && p.Resolution == nil
}

// Funded reports whether the payment has received enough live funds
// (confirmed or not, in time or late) to be treated as a real payment
// rather than an empty or dusted one. It decides how long the deposit
// address stays on the chain watch list after the payment last changed
// ([Config.WatchWindow] when funded, [Config.UnfundedWatchWindow] when
// not).
//
// A payment the merchant created is funded by anything that is not dust
// ([DustDivisor]). A payment created through a public payment link — which
// anybody can create, in any number, for free — is funded only by half the
// amount due and at least one whole unit of the asset ([FundedDivisor],
// [FundedFloorUnits]), so that keeping a link payment's address on the
// watch list always costs real money, which goes to the merchant.
func (p Payment) Funded() bool { return p.fundedBy(p.AmountReceived) }

// fundedBy reports whether a would fund the payment.
func (p Payment) fundedBy(a Amount) bool {
	limit := dustLimit(p.Amount, p.Asset.Decimals)
	if p.LinkID != "" {
		limit = fundedLimit(p.Amount, p.Asset.Decimals)
	}
	return a.Cmp(limit) >= 0
}

// AmountDue is what a payer still has to send for the payment to be
// accepted as paid in full: Amount minus everything received so far.
func (p Payment) AmountDue() Amount { return p.Amount.Sub(p.AmountReceived) }

// Confirmations reports the confirmation count to show for the payment
// given the chain head: the confirmations of its least-confirmed live
// transfer, capped at RequiredConfirmations. It is 0 when nothing was
// received.
func (p Payment) Confirmations(head uint64) int {
	if p.PendingBlock == 0 {
		if p.AmountConfirmed.IsZero() {
			return 0
		}
		return p.RequiredConfirmations
	}
	return confirmations(p.PendingBlock, head, p.RequiredConfirmations)
}

// clone returns a deep copy.
func (p Payment) clone() Payment {
	p.Metadata = maps.Clone(p.Metadata)
	if p.Metadata == nil {
		p.Metadata = map[string]string{}
	}
	if p.Resolution != nil {
		r := *p.Resolution
		p.Resolution = &r
	}
	return p
}

// confirmations counts confirmations of a transfer in block under head: the
// including block is the first confirmation. The result is capped at limit.
func confirmations(block, head uint64, limit int) int {
	if block == 0 || head < block {
		return 0
	}
	n := head - block + 1
	if n > uint64(limit) {
		return limit
	}
	return int(n)
}

// TransferStatus is the state of an observed [Transfer].
type TransferStatus string

// Transfer statuses.
const (
	// TransferPending: on the canonical chain as far as the core knows, with
	// fewer than the required confirmations.
	TransferPending TransferStatus = "pending"
	// TransferConfirmed: reached the required confirmations. Final: the core
	// never un-confirms a transfer.
	TransferConfirmed TransferStatus = "confirmed"
	// TransferOrphaned: reported as no longer on the canonical chain. An
	// orphaned transfer counts for nothing, but is kept for the record and
	// comes back to life if the same block becomes canonical again.
	TransferOrphaned TransferStatus = "orphaned"
)

// TransferKey identifies one observed token transfer: a log in a
// transaction in a specific block. The block hash is part of the identity
// because the same transaction can be included again, at another log index,
// after a reorganisation.
type TransferKey struct {
	// TxHash is the transaction hash, canonical lowercase.
	TxHash string
	// LogIndex is the index of the Transfer log within its block.
	LogIndex uint
	// BlockHash is the hash of the including block, canonical lowercase.
	BlockHash string
}

// Transfer is a token transfer to a payment's deposit address, as reported
// by the chain adapter. The triple (TxHash, LogIndex, BlockHash) is unique.
type Transfer struct {
	TxHash    string
	LogIndex  uint
	BlockHash string

	// PaymentID is the payment that owns the destination address.
	PaymentID string
	// BlockNumber is the height of the including block.
	BlockNumber uint64
	// BlockTime is the including block's timestamp (zero if not reported).
	BlockTime time.Time
	// From and To are canonical lowercase addresses.
	From string
	To   string
	// Amount is the transferred value in base units.
	Amount Amount

	// Status is maintained by the core.
	Status TransferStatus
	// SeenAt is when the core first learned about the transfer.
	SeenAt time.Time
	// NotBefore is a lower bound for the including block's timestamp that
	// the core worked out itself when it first saw the transfer: the block
	// time of the head it had processed by then, if that head is below
	// BlockNumber (block times never decrease along a chain). Zero when no
	// such head was known. It dates a transfer when the chain adapter does
	// not report BlockTime.
	NotBefore time.Time
	// ConfirmedAt is when it reached finality; zero otherwise.
	ConfirmedAt time.Time
	// OrphanedAt is when it was last reported reorganised out; zero while
	// live.
	OrphanedAt time.Time
}

// Key returns the transfer's identity.
func (t Transfer) Key() TransferKey {
	return TransferKey{TxHash: t.TxHash, LogIndex: t.LogIndex, BlockHash: t.BlockHash}
}

// Live reports whether the transfer counts towards its payment.
func (t Transfer) Live() bool { return t.Status != TransferOrphaned }

// After reports whether the transfer provably entered the chain at or after
// deadline. Like a payment's deadline itself, that takes both clocks: chain
// time — BlockTime, or NotBefore when the adapter did not report one — and
// the wall clock at which the core first saw the transfer. A transfer that
// cannot be dated, or that the core had already seen before the deadline,
// is never "after": the payer gets the benefit of the doubt, and a wrong
// block timestamp alone cannot turn a payment made in time into a late one.
func (t Transfer) After(deadline time.Time) bool {
	chain := t.BlockTime
	if chain.IsZero() {
		chain = t.NotBefore
	}
	return !chain.IsZero() && !chain.Before(deadline) && !t.SeenAt.Before(deadline)
}

// ChainHead is the newest block the chain adapter has fully processed.
type ChainHead struct {
	// ChainID is the chain the head belongs to.
	ChainID uint64
	// Number is the block height.
	Number uint64
	// Hash is the block hash, canonical lowercase.
	Hash string
	// Time is the block's timestamp. Payment deadlines are judged against
	// it, so a lagging watcher can never expire a payment that was paid in
	// time.
	Time time.Time
	// Final is the highest block that the chain adapter treats as beyond
	// reorganisation: the newest block the chain has finalised, as two
	// endpoints attest it — or, on a network declared to have no finality
	// ([HasFinality]; none is, in this version), the block the adapter's
	// reorganisation window below Number. Two things rest on it. A transfer
	// is final, and counts towards paid, only in a block at or below it (and
	// with the payment's confirmations: [Evaluate], rule 1). And a payment's
	// address is scanned for from the block after it, or after Number if
	// that is lower ([Payment.CreatedAtBlock]): a block above it can still
	// be replaced, and the payer's transaction mined again in a block at or
	// below the head the address was handed out at. 0 is a block like any
	// other (a chain younger than the window); every head carries it —
	// heads stored by versions that did not, or that could take it from
	// depth alone on a network with finality, are dropped when the database
	// is upgraded, and the core then waits for the adapter's first head
	// ([ErrChainNotReady]).
	//
	// The core never lowers it: a head that reports a lower final block
	// (one that went back, or an adapter that lost its records) keeps the
	// stored one, which can therefore be above Number for a while. The
	// adapter is given it when it starts, so that what two endpoints
	// attested once stays final across the loss of its own records.
	Final uint64
	// FinalTime is the timestamp of the final block; zero if not known.
	// An address leaves the watch list only once it, too, is past the
	// address's window ([Payment.RetiredAt]): a reorganisation that keeps
	// every final block cannot move money into a window that ended before
	// the final block was mined.
	FinalTime time.Time
	// Checked is the block time before which the chain adapter has had
	// every block scanned by two log endpoints (or by the only configured
	// endpoint): no later than Time, and earlier while blocks are on record
	// as settled on a single endpoint's logs and not scanned by a second
	// one yet (the chain watcher's unchecked blocks). Zero means Time:
	// nothing outstanding. A settled payment's address leaves the watch
	// list only once Checked, too, is past its window (see
	// [Payment.RetiredAt]): a payment hidden in those blocks is found when
	// they are scanned again, which can only find it while the address is
	// watched.
	Checked time.Time
	// ObservedAt is when the core recorded this head (set by the core).
	ObservedAt time.Time
}
