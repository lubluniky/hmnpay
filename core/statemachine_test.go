package core_test

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// The state machine as a table: one row per edge of the transition table in
// the package documentation, exercised on the pure function.
func TestEvaluate(t *testing.T) {
	const (
		grace    = time.Minute
		window   = 7 * 24 * time.Hour // watch window once a transfer was seen
		unfunded = time.Hour          // watch window of a payment nobody ever paid
	)
	var (
		t0       = coretest.Epoch
		expires  = t0.Add(30 * time.Minute)
		inTime   = t0.Add(10 * time.Minute) // chain time before the deadline
		tooLate  = expires.Add(grace)       // first chain time past the deadline
		wall     = t0.Add(3 * time.Hour)    // default wall clock of the evaluation: past the deadline
		noChange = []core.EventType(nil)
	)
	usdt := func(s string) core.Amount { return core.MustParseAmount(s, 18) }

	type transfer struct {
		amount string
		block  uint64
		status core.TransferStatus
		// What dates the transfer: the reported block time, the core's own
		// lower bound for it, and when the core first saw it (zero = t0,
		// before the deadline).
		blockTime, notBefore, seenAt time.Time
	}
	pending := func(amount string, block uint64) transfer {
		return transfer{amount: amount, block: block, status: core.TransferPending}
	}
	confirmed := func(amount string, block uint64) transfer {
		return transfer{amount: amount, block: block, status: core.TransferConfirmed}
	}
	orphaned := func(amount string, block uint64) transfer {
		return transfer{amount: amount, block: block, status: core.TransferOrphaned}
	}
	// late: in a block at the deadline, first seen at the deadline.
	late := func(tr transfer) transfer { tr.blockTime, tr.seenAt = tooLate, tooLate; return tr }

	type row struct {
		name string
		// given
		status    core.PaymentStatus
		tolerance string
		attention core.Attention
		resolved  bool
		cancelled bool // the payment was cancelled (status expired, CancelledAt set)
		transfers []transfer
		head      uint64
		// unfinal is how far the head's final block lies below it
		// (ChainHead.Final = head - unfinal): 0, the final block is the
		// head itself, as on BSC, where the chain finalises blocks long
		// before the processed head reaches them.
		unfinal    uint64
		chainTime  time.Time // head block time; zero value = inTime
		wallClock  time.Time // wall clock (EvalInput.Now); zero value = wall, which is past the deadline
		otherChain bool      // the head belongs to another chain
		noDeadline bool      // evaluate outside AdvanceHead
		link       bool      // the payment was created through a public link
		// mark is the settled payment's LateFundsMark: how much of what is
		// confirmed has been accounted for (by its outcome, a raised issue
		// or a resolution). Empty = everything the row lists as confirmed,
		// which is what a payment that has been told about all of it holds.
		mark string
		// then
		want          core.PaymentStatus
		events        []core.EventType
		received      string
		confirmed     string
		promoted      int
		wantAttention core.Attention
		wantResolved  bool
		// watch, when set, is what must happen to WatchUntil: "long" (now +
		// WatchWindow), "short" (now + UnfundedWatchWindow) or "same".
		watch string
	}
	ev := func(types ...core.EventType) []core.EventType { return types }

	rows := []row{
		// ── open: the status follows the funds ──
		{name: "awaiting, nothing seen", status: core.StatusAwaiting, head: 100,
			want: core.StatusAwaiting, events: noChange, received: "0", confirmed: "0"},
		{name: "awaiting → confirming: full amount seen, zero confirmations", status: core.StatusAwaiting,
			transfers: []transfer{pending("149", 100)}, head: 99,
			want: core.StatusConfirming, events: ev(core.EventPaymentConfirming), received: "149", confirmed: "0"},
		{name: "awaiting → detected: partial amount seen", status: core.StatusAwaiting,
			transfers: []transfer{pending("100", 100)}, head: 100,
			want: core.StatusDetected, events: ev(core.EventPaymentDetected), received: "100", confirmed: "0"},
		{name: "detected → confirming: transfers are summed", status: core.StatusDetected,
			transfers: []transfer{pending("100", 100), pending("49", 101)}, head: 101,
			want: core.StatusConfirming, events: ev(core.EventPaymentConfirming), received: "149", confirmed: "0"},
		{name: "confirming stays: 14 of 15 confirmations", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100)}, head: 113,
			want: core.StatusConfirming, events: noChange, received: "149", confirmed: "0"},
		{name: "confirming → paid: 15th confirmation", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100)}, head: 114,
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "149", confirmed: "149", promoted: 1},
		// ── finality: the confirmations are a floor, never a substitute ──
		{name: "confirming stays: 15 confirmations, but the block is above the final block", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100)}, head: 114, unfinal: 15,
			want: core.StatusConfirming, events: noChange, received: "149", confirmed: "0"},
		{name: "confirming stays: a thousand blocks deep, the chain has not finalised it", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100)}, head: 1100, unfinal: 1001,
			want: core.StatusConfirming, events: noChange, received: "149", confirmed: "0"},
		{name: "confirming → paid: the chain finalises the block", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100)}, head: 1100, unfinal: 1000,
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "149", confirmed: "149", promoted: 1},
		{name: "confirming stays: the block is final but has 14 of 15 confirmations", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100)}, head: 113, unfinal: 13,
			want: core.StatusConfirming, events: noChange, received: "149", confirmed: "0"},
		{name: "final block reached by one of two transfers: confirming, not paid", status: core.StatusConfirming,
			transfers: []transfer{pending("100", 100), pending("49", 120)}, head: 140, unfinal: 30,
			want: core.StatusConfirming, events: noChange, received: "149", confirmed: "100", promoted: 1},
		{name: "paid: late funds wait for finality like any transfer", status: core.StatusPaid,
			transfers: []transfer{confirmed("149", 100), late(pending("60", 150))}, head: 200, unfinal: 51,
			want: core.StatusPaid, events: noChange, received: "209", confirmed: "149"},
		{name: "paid: late funds final → late funds", status: core.StatusPaid,
			transfers: []transfer{confirmed("149", 100), late(pending("60", 150))}, head: 200, unfinal: 50,
			want: core.StatusPaid, events: ev(core.EventPaymentLateFunds), received: "209", confirmed: "209", promoted: 1,
			wantAttention: core.AttentionLateFunds},
		{name: "deadline: a payment waiting for finality is not expired", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100)}, head: 500, unfinal: 401, chainTime: tooLate,
			want: core.StatusConfirming, events: noChange, received: "149", confirmed: "0"},
		{name: "awaiting → paid: first reported deep under the head, no intermediate event", status: core.StatusAwaiting,
			transfers: []transfer{pending("149", 100)}, head: 5000,
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "149", confirmed: "149", promoted: 1},
		{name: "confirming → overpaid: one base unit too many", status: core.StatusConfirming,
			transfers: []transfer{pending("149.000000000000000001", 100)}, head: 114,
			want: core.StatusOverpaid, events: ev(core.EventPaymentOverpaid), received: "149.000000000000000001", confirmed: "149.000000000000000001",
			promoted: 1, wantAttention: core.AttentionOverpaid},
		{name: "two transfers final in one step are judged together: overpaid", status: core.StatusConfirming,
			transfers: []transfer{pending("100", 100), pending("100", 105)}, head: 200,
			want: core.StatusOverpaid, events: ev(core.EventPaymentOverpaid), received: "200", confirmed: "200", promoted: 2, wantAttention: core.AttentionOverpaid},
		{name: "one base unit short without tolerance is not paid", status: core.StatusConfirming,
			transfers: []transfer{pending("148.999999999999999999", 100)}, head: 114,
			want: core.StatusDetected, events: ev(core.EventPaymentDetected), received: "148.999999999999999999", confirmed: "148.999999999999999999", promoted: 1},

		// ── underpayment tolerance ──
		{name: "tolerance: short by exactly the tolerance is paid", status: core.StatusConfirming, tolerance: "0.5",
			transfers: []transfer{pending("148.5", 100)}, head: 114,
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "148.5", confirmed: "148.5", promoted: 1},
		{name: "tolerance: the tolerance also decides confirming", status: core.StatusAwaiting, tolerance: "0.5",
			transfers: []transfer{pending("148.5", 100)}, head: 100,
			want: core.StatusConfirming, events: ev(core.EventPaymentConfirming), received: "148.5", confirmed: "0"},
		{name: "tolerance: one base unit beyond it is short", status: core.StatusDetected, tolerance: "0.5",
			transfers: []transfer{pending("148.499999999999999999", 100)}, head: 114,
			want: core.StatusDetected, events: noChange, received: "148.499999999999999999", confirmed: "148.499999999999999999", promoted: 1},
		{name: "tolerance: no tolerance on the upper side", status: core.StatusConfirming, tolerance: "0.5",
			transfers: []transfer{pending("149.01", 100)}, head: 114,
			want: core.StatusOverpaid, events: ev(core.EventPaymentOverpaid), received: "149.01", confirmed: "149.01", promoted: 1, wantAttention: core.AttentionOverpaid},
		// The tolerance is capped at 5 % of the amount (149 → 7.45),
		// so a tolerance at or above the amount cannot let dust settle it.
		{name: "tolerance ≥ amount never makes an unpaid payment paid", status: core.StatusAwaiting, tolerance: "500", head: 100,
			want: core.StatusAwaiting, events: noChange, received: "0", confirmed: "0"},
		{name: "tolerance ≥ amount: dust does not pay", status: core.StatusAwaiting, tolerance: "500",
			transfers: []transfer{confirmed("0.000000000000000001", 100)}, head: 200,
			want: core.StatusAwaiting, events: noChange, received: "0.000000000000000001", confirmed: "0.000000000000000001", watch: "same"},
		{name: "tolerance ≥ amount: half the amount does not pay", status: core.StatusDetected, tolerance: "149",
			transfers: []transfer{confirmed("74.5", 100)}, head: 200,
			want: core.StatusDetected, events: noChange, received: "74.5", confirmed: "74.5"},
		{name: "tolerance cap: 95 % of the amount is the least that pays", status: core.StatusDetected, tolerance: "500",
			transfers: []transfer{confirmed("141.55", 100)}, head: 200,
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "141.55", confirmed: "141.55"},
		{name: "tolerance cap: one base unit under 95 % is short", status: core.StatusDetected, tolerance: "500",
			transfers: []transfer{confirmed("141.549999999999999999", 100)}, head: 200,
			want: core.StatusDetected, events: noChange, received: "141.549999999999999999", confirmed: "141.549999999999999999"},
		{name: "tolerance cap also decides confirming", status: core.StatusAwaiting, tolerance: "500",
			transfers: []transfer{pending("141.54", 100)}, head: 100,
			want: core.StatusDetected, events: ev(core.EventPaymentDetected), received: "141.54", confirmed: "0"},

		// ── partial payments and top-ups ──
		{name: "partial, confirmed, deadline not reached: stays open", status: core.StatusDetected,
			transfers: []transfer{confirmed("100", 100)}, head: 200,
			want: core.StatusDetected, events: noChange, received: "100", confirmed: "100"},
		{name: "partial confirmed, top-up pending: confirming", status: core.StatusDetected,
			transfers: []transfer{confirmed("100", 100), pending("49", 200)}, head: 205,
			want: core.StatusConfirming, events: ev(core.EventPaymentConfirming), received: "149", confirmed: "100"},
		{name: "top-up becomes final: paid", status: core.StatusConfirming,
			transfers: []transfer{confirmed("100", 100), pending("49", 200)}, head: 214,
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "149", confirmed: "149", promoted: 1},
		{name: "settles as soon as confirmed funds suffice, despite a pending extra", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100), pending("10", 110)}, head: 114,
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "159", confirmed: "149", promoted: 1},

		// ── the deadline ──
		{name: "awaiting → expired: deadline passed, nothing ever seen", status: core.StatusAwaiting, head: 900, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentExpired), received: "0", confirmed: "0"},
		{name: "not expired one millisecond before deadline + grace", status: core.StatusAwaiting, head: 900, chainTime: tooLate.Add(-time.Millisecond),
			want: core.StatusAwaiting, events: noChange, received: "0", confirmed: "0"},
		{name: "not expired at the bare deadline: the grace applies", status: core.StatusAwaiting, head: 900, chainTime: expires,
			want: core.StatusAwaiting, events: noChange, received: "0", confirmed: "0"},
		{name: "not expired outside AdvanceHead, however late", status: core.StatusAwaiting, head: 900, chainTime: tooLate.Add(time.Hour), noDeadline: true,
			want: core.StatusAwaiting, events: noChange, received: "0", confirmed: "0"},
		{name: "not expired by a head of another chain", status: core.StatusAwaiting, head: 900, chainTime: tooLate, otherChain: true,
			want: core.StatusAwaiting, events: noChange, received: "0", confirmed: "0"},
		// A deadline needs BOTH clocks. A block timestamp from the
		// future (one lying RPC endpoint) cannot expire anything early; a
		// wall clock that ran ahead of the processed chain cannot either.
		{name: "chain time 40 days ahead, wall clock before the deadline: stays awaiting", status: core.StatusAwaiting, head: 900,
			chainTime: expires.Add(40 * 24 * time.Hour), wallClock: expires.Add(-10 * time.Minute),
			want: core.StatusAwaiting, events: noChange, received: "0", confirmed: "0"},
		{name: "chain time ahead, wall clock inside the grace: stays awaiting", status: core.StatusAwaiting, head: 900,
			chainTime: expires.Add(40 * 24 * time.Hour), wallClock: tooLate.Add(-time.Millisecond),
			want: core.StatusAwaiting, events: noChange, received: "0", confirmed: "0"},
		{name: "chain time ahead, wall clock reaches deadline + grace: expires", status: core.StatusAwaiting, head: 900,
			chainTime: expires.Add(40 * 24 * time.Hour), wallClock: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentExpired), received: "0", confirmed: "0"},
		{name: "wall clock far past, chain time one millisecond short: stays awaiting", status: core.StatusAwaiting, head: 900,
			chainTime: tooLate.Add(-time.Millisecond), wallClock: expires.Add(40 * 24 * time.Hour),
			want: core.StatusAwaiting, events: noChange, received: "0", confirmed: "0"},
		{name: "chain time ahead, wall clock before the deadline: a short payment is not underpaid yet", status: core.StatusDetected,
			transfers: []transfer{confirmed("100", 100)}, head: 900, chainTime: expires.Add(40 * 24 * time.Hour), wallClock: inTime,
			want: core.StatusDetected, events: noChange, received: "100", confirmed: "100"},
		{name: "chain time ahead, wall clock before the deadline: an orphaned transfer is not a failure yet", status: core.StatusAwaiting,
			transfers: []transfer{orphaned("149", 100)}, head: 900, chainTime: expires.Add(40 * 24 * time.Hour), wallClock: inTime,
			want: core.StatusAwaiting, events: noChange, received: "0", confirmed: "0"},
		{name: "chain time ahead, wall clock before the deadline: paying still works", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100)}, head: 114, chainTime: expires.Add(40 * 24 * time.Hour), wallClock: inTime,
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "149", confirmed: "149", promoted: 1},

		{name: "detected → underpaid: deadline passed, confirmed funds short", status: core.StatusDetected,
			transfers: []transfer{confirmed("100", 100)}, head: 900, chainTime: tooLate,
			want: core.StatusUnderpaid, events: ev(core.EventPaymentUnderpaid), received: "100", confirmed: "100", wantAttention: core.AttentionUnderpaid},
		{name: "mid-confirmation at the deadline: does not expire", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100)}, head: 105, chainTime: tooLate.Add(time.Hour),
			want: core.StatusConfirming, events: noChange, received: "149", confirmed: "0"},
		{name: "partial, still pending at the deadline: held open", status: core.StatusDetected,
			transfers: []transfer{confirmed("100", 100), pending("20", 300)}, head: 305, chainTime: tooLate,
			want: core.StatusDetected, events: noChange, received: "120", confirmed: "100"},
		{name: "partial, last transfer final after the deadline: underpaid", status: core.StatusDetected,
			transfers: []transfer{confirmed("100", 100), pending("20", 300)}, head: 314, chainTime: tooLate,
			want: core.StatusUnderpaid, events: ev(core.EventPaymentUnderpaid), received: "120", confirmed: "120", promoted: 1, wantAttention: core.AttentionUnderpaid},
		{name: "paid after the deadline when the pending transfer completes it", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100)}, head: 114, chainTime: tooLate.Add(time.Hour),
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "149", confirmed: "149", promoted: 1},

		// ── reorganisations ──
		{name: "confirming → awaiting: the only transfer was orphaned", status: core.StatusConfirming,
			transfers: []transfer{orphaned("149", 100)}, head: 101,
			want: core.StatusAwaiting, events: ev(core.EventPaymentAwaiting), received: "0", confirmed: "0"},
		{name: "confirming → detected: one of two transfers orphaned", status: core.StatusConfirming,
			transfers: []transfer{pending("100", 100), orphaned("49", 101)}, head: 102,
			want: core.StatusDetected, events: ev(core.EventPaymentDetected), received: "100", confirmed: "0"},
		{name: "an orphaned transfer counts for nothing", status: core.StatusAwaiting,
			transfers: []transfer{orphaned("149", 100), pending("149", 102)}, head: 102,
			want: core.StatusConfirming, events: ev(core.EventPaymentConfirming), received: "149", confirmed: "0"},
		{name: "an orphaned transfer is never promoted", status: core.StatusAwaiting,
			transfers: []transfer{orphaned("149", 100)}, head: 5000,
			want: core.StatusAwaiting, events: noChange, received: "0", confirmed: "0"},
		{name: "awaiting → failed: deadline passed, only orphaned transfers", status: core.StatusAwaiting,
			transfers: []transfer{orphaned("149", 100)}, head: 900, chainTime: tooLate,
			want: core.StatusFailed, events: ev(core.EventPaymentFailed), received: "0", confirmed: "0", wantAttention: core.AttentionFailed},
		{name: "orphaned after the deadline is not settled by the retraction itself", status: core.StatusConfirming,
			transfers: []transfer{orphaned("149", 100)}, head: 900, chainTime: tooLate, noDeadline: true,
			want: core.StatusAwaiting, events: ev(core.EventPaymentAwaiting), received: "0", confirmed: "0"},
		{name: "a head of another chain confirms nothing", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100)}, head: 5000, otherChain: true,
			want: core.StatusConfirming, events: noChange, received: "149", confirmed: "0"},

		// ── settled: the status is frozen ──
		{name: "paid: an unconfirmed second payment only moves the sums", status: core.StatusPaid,
			transfers: []transfer{confirmed("149", 100), pending("149", 200)}, head: 205,
			want: core.StatusPaid, events: noChange, received: "298", confirmed: "149"},
		{name: "paid: a second payment becomes final → late funds", status: core.StatusPaid,
			transfers: []transfer{confirmed("149", 100), pending("149", 200)}, head: 214,
			want: core.StatusPaid, events: ev(core.EventPaymentLateFunds), received: "298", confirmed: "298", promoted: 1, wantAttention: core.AttentionLateFunds},
		{name: "expired: late funds become final → late funds, still expired", status: core.StatusExpired,
			transfers: []transfer{pending("149", 200)}, head: 214, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentLateFunds), received: "149", confirmed: "149", promoted: 1, wantAttention: core.AttentionLateFunds},
		{name: "underpaid and resolved: a late top-up reopens the issue", status: core.StatusUnderpaid, attention: core.AttentionUnderpaid, resolved: true,
			transfers: []transfer{confirmed("100", 100), pending("49", 200)}, head: 214, chainTime: tooLate,
			want: core.StatusUnderpaid, events: ev(core.EventPaymentLateFunds), received: "149", confirmed: "149", promoted: 1, wantAttention: core.AttentionUnderpaid},
		{name: "underpaid and resolved: an unconfirmed top-up keeps the resolution", status: core.StatusUnderpaid, attention: core.AttentionUnderpaid, resolved: true,
			transfers: []transfer{confirmed("100", 100), pending("49", 200)}, head: 205, chainTime: tooLate,
			want: core.StatusUnderpaid, events: noChange, received: "149", confirmed: "100", wantAttention: core.AttentionUnderpaid, wantResolved: true},
		{name: "failed: the transaction is mined after all → late funds, still failed", status: core.StatusFailed, attention: core.AttentionFailed,
			transfers: []transfer{orphaned("149", 100), pending("149", 300)}, head: 314, chainTime: tooLate,
			want: core.StatusFailed, events: ev(core.EventPaymentLateFunds), received: "149", confirmed: "149", promoted: 1, wantAttention: core.AttentionFailed},
		{name: "paid: an extra transfer orphaned after settlement leaves no trace", status: core.StatusPaid,
			transfers: []transfer{confirmed("149", 100), orphaned("5", 110)}, head: 300,
			want: core.StatusPaid, events: noChange, received: "149", confirmed: "149"},
		{name: "expired stays expired", status: core.StatusExpired, head: 9000, chainTime: tooLate.Add(24 * time.Hour),
			want: core.StatusExpired, events: noChange, received: "0", confirmed: "0"},

		// ── cancelled: an expired payment like any other ──
		{name: "cancelled stays cancelled when the deadline passes", status: core.StatusExpired, cancelled: true, head: 9000, chainTime: tooLate,
			want: core.StatusExpired, events: noChange, received: "0", confirmed: "0"},
		{name: "cancelled: a transfer seen afterwards only moves the sums", status: core.StatusExpired, cancelled: true,
			transfers: []transfer{pending("149", 200)}, head: 205,
			want: core.StatusExpired, events: noChange, received: "149", confirmed: "0"},
		{name: "cancelled: the transfer becomes final → late funds, still cancelled", status: core.StatusExpired, cancelled: true,
			transfers: []transfer{pending("149", 200)}, head: 214,
			want: core.StatusExpired, events: ev(core.EventPaymentLateFunds), received: "149", confirmed: "149", promoted: 1, wantAttention: core.AttentionLateFunds},
		{name: "cancelled after a reorg: the orphaned transfer counts for nothing", status: core.StatusExpired, cancelled: true,
			transfers: []transfer{orphaned("149", 100)}, head: 9000, chainTime: tooLate,
			want: core.StatusExpired, events: noChange, received: "0", confirmed: "0"},
		{name: "cancelled after a reorg: the re-included transfer is late funds, counted once", status: core.StatusExpired, cancelled: true,
			transfers: []transfer{orphaned("149", 100), pending("149", 103)}, head: 117,
			want: core.StatusExpired, events: ev(core.EventPaymentLateFunds), received: "149", confirmed: "149", promoted: 1, wantAttention: core.AttentionLateFunds},
		{name: "cancelled and resolved: more late funds reopen the issue", status: core.StatusExpired, cancelled: true, attention: core.AttentionLateFunds, resolved: true,
			transfers: []transfer{confirmed("149", 200), pending("1", 300)}, head: 314,
			want: core.StatusExpired, events: ev(core.EventPaymentLateFunds), received: "150", confirmed: "150", promoted: 1, wantAttention: core.AttentionLateFunds},
		{name: "overpaid stays overpaid", status: core.StatusOverpaid, attention: core.AttentionOverpaid,
			transfers: []transfer{confirmed("150", 100)}, head: 9000, chainTime: tooLate,
			want: core.StatusOverpaid, events: noChange, received: "150", confirmed: "150", wantAttention: core.AttentionOverpaid},

		// ── Funds after the deadline are late funds, whether or not
		// a head had settled the payment by then ──
		{name: "late transfer, no head since the deadline: it neither opens nor pays the payment", status: core.StatusAwaiting,
			transfers: []transfer{late(pending("149", 300))}, head: 314, noDeadline: true,
			want: core.StatusAwaiting, events: noChange, received: "149", confirmed: "149", promoted: 1, watch: "long"},
		{name: "late transfer, already final when the deadline is processed: expired with late funds", status: core.StatusAwaiting,
			transfers: []transfer{late(pending("149", 300))}, head: 314, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentExpired, core.EventPaymentLateFunds), received: "149", confirmed: "149", promoted: 1,
			wantAttention: core.AttentionLateFunds, watch: "long"},
		{name: "late transfer, still pending when the deadline is processed: it does not hold the payment open", status: core.StatusAwaiting,
			transfers: []transfer{late(pending("149", 300))}, head: 305, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentExpired), received: "149", confirmed: "0", watch: "long"},
		{name: "partial in time, top-up late: underpaid, and the top-up is late funds", status: core.StatusDetected,
			transfers: []transfer{confirmed("100", 100), late(pending("49", 300))}, head: 314, chainTime: tooLate,
			want: core.StatusUnderpaid, events: ev(core.EventPaymentUnderpaid, core.EventPaymentLateFunds), received: "149", confirmed: "149", promoted: 1,
			wantAttention: core.AttentionUnderpaid, watch: "long"},
		{name: "paid in time plus a late transfer already final: paid with late funds, not overpaid", status: core.StatusConfirming,
			transfers: []transfer{pending("149", 100), late(pending("149", 101))}, head: 115, chainTime: tooLate,
			want: core.StatusPaid, events: ev(core.EventPaymentPaid, core.EventPaymentLateFunds), received: "298", confirmed: "298", promoted: 2,
			wantAttention: core.AttentionLateFunds, watch: "long"},
		{name: "dated by the core's own lower bound only: late", status: core.StatusAwaiting,
			transfers: []transfer{{amount: "149", block: 300, status: core.TransferPending, notBefore: tooLate, seenAt: tooLate}}, head: 314, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentExpired, core.EventPaymentLateFunds), received: "149", confirmed: "149", promoted: 1,
			wantAttention: core.AttentionLateFunds, watch: "long"},
		{name: "one millisecond before the deadline by chain time: in time", status: core.StatusAwaiting,
			transfers: []transfer{{amount: "149", block: 300, status: core.TransferPending, blockTime: tooLate.Add(-time.Millisecond), seenAt: tooLate.Add(time.Hour)}},
			head:      314, chainTime: tooLate.Add(time.Hour),
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "149", confirmed: "149", promoted: 1, watch: "long"},
		{name: "a block timestamp past the deadline on a transfer seen before it: in time (one clock is not enough)", status: core.StatusAwaiting,
			transfers: []transfer{{amount: "149", block: 300, status: core.TransferPending, blockTime: tooLate.Add(40 * 24 * time.Hour), seenAt: inTime}},
			head:      314, chainTime: tooLate,
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "149", confirmed: "149", promoted: 1, watch: "long"},
		{name: "undated transfer first seen after the deadline: benefit of the doubt, in time", status: core.StatusAwaiting,
			transfers: []transfer{{amount: "149", block: 300, status: core.TransferPending, seenAt: tooLate.Add(48 * time.Hour)}},
			head:      314, chainTime: tooLate,
			want: core.StatusPaid, events: ev(core.EventPaymentPaid), received: "149", confirmed: "149", promoted: 1, watch: "long"},

		// ── Dust (below min(1 unit, 5 % of the amount)) is recorded
		// and otherwise ignored ──
		{name: "dust pending: still awaiting, no event, no longer watch", status: core.StatusAwaiting,
			transfers: []transfer{pending("0.999999999999999999", 100)}, head: 100,
			want: core.StatusAwaiting, events: noChange, received: "0.999999999999999999", confirmed: "0", watch: "same"},
		{name: "the dust limit itself is not dust: detected", status: core.StatusAwaiting,
			transfers: []transfer{pending("1", 100)}, head: 100,
			want: core.StatusDetected, events: ev(core.EventPaymentDetected), received: "1", confirmed: "0", watch: "long"},
		{name: "dust pending at the deadline does not hold the payment open", status: core.StatusAwaiting,
			transfers: []transfer{pending("0.5", 300)}, head: 305, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentExpired), received: "0.5", confirmed: "0", watch: "short"},
		{name: "dust confirmed at the deadline: expired, not underpaid, no attention", status: core.StatusAwaiting,
			transfers: []transfer{confirmed("0.000000000000000001", 100)}, head: 900, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentExpired), received: "0.000000000000000001", confirmed: "0.000000000000000001", watch: "short"},
		{name: "two dust transfers that add up are not dust: underpaid", status: core.StatusAwaiting,
			transfers: []transfer{confirmed("0.5", 100), confirmed("0.5", 101)}, head: 900, chainTime: tooLate,
			want: core.StatusUnderpaid, events: ev(core.EventPaymentUnderpaid), received: "1", confirmed: "1", wantAttention: core.AttentionUnderpaid, watch: "long"},
		{name: "only orphaned dust at the deadline: expired, not failed", status: core.StatusAwaiting,
			transfers: []transfer{orphaned("0.5", 100)}, head: 900, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentExpired), received: "0", confirmed: "0", watch: "short"},
		{name: "expired: dust becoming final later is no late funds", status: core.StatusExpired,
			transfers: []transfer{pending("0.5", 300)}, head: 314, chainTime: tooLate,
			want: core.StatusExpired, events: noChange, received: "0.5", confirmed: "0.5", promoted: 1, watch: "same"},
		{name: "paid: dust on top is recorded, not flagged", status: core.StatusPaid,
			transfers: []transfer{confirmed("149", 100), pending("0.01", 300)}, head: 314,
			want: core.StatusPaid, events: noChange, received: "149.01", confirmed: "149.01", promoted: 1, watch: "long"},
		{name: "expired: a real amount becoming final later is late funds", status: core.StatusExpired,
			transfers: []transfer{pending("1", 300)}, head: 314, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentLateFunds), received: "1", confirmed: "1", promoted: 1,
			wantAttention: core.AttentionLateFunds, watch: "long"},

		// ── A link payment is funded by half the amount, not by any
		// transfer ──
		{name: "link payment, a partial payment under half: detected, but only the short watch window", status: core.StatusAwaiting, link: true,
			transfers: []transfer{pending("74.499999999999999999", 100)}, head: 100,
			want: core.StatusDetected, events: ev(core.EventPaymentDetected), received: "74.499999999999999999", confirmed: "0", watch: "short"},
		{name: "link payment, half the amount: funded, the long watch window", status: core.StatusAwaiting, link: true,
			transfers: []transfer{pending("74.5", 100)}, head: 100,
			want: core.StatusDetected, events: ev(core.EventPaymentDetected), received: "74.5", confirmed: "0", watch: "long"},
		{name: "link payment, underpaid below half: flagged, short watch window", status: core.StatusDetected, link: true,
			transfers: []transfer{confirmed("30", 100)}, head: 900, chainTime: tooLate,
			want: core.StatusUnderpaid, events: ev(core.EventPaymentUnderpaid), received: "30", confirmed: "30", wantAttention: core.AttentionUnderpaid, watch: "short"},
		{name: "link payment, dust then expiry: short watch window", status: core.StatusAwaiting, link: true,
			transfers: []transfer{confirmed("0.000000000000000001", 100)}, head: 900, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentExpired), received: "0.000000000000000001", confirmed: "0.000000000000000001", watch: "short"},
		{name: "link payment, expired: late dust does not extend the watch window", status: core.StatusExpired, link: true,
			transfers: []transfer{pending("0.5", 300)}, head: 305,
			want: core.StatusExpired, events: noChange, received: "0.5", confirmed: "0", watch: "same"},
		// ── Late funds are judged as a sum, not transfer by transfer ──
		{name: "expired: a late piece below the dust limit is recorded, not flagged", status: core.StatusExpired,
			transfers: []transfer{late(pending("0.75", 300))}, head: 314, chainTime: tooLate,
			want: core.StatusExpired, events: noChange, received: "0.75", confirmed: "0.75", promoted: 1, watch: "same"},
		{name: "expired: the second piece takes the unflagged total over the limit → late funds", status: core.StatusExpired, mark: "0",
			transfers: []transfer{late(confirmed("0.75", 300)), late(pending("0.75", 301))}, head: 315, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentLateFunds), received: "1.5", confirmed: "1.5", promoted: 1,
			wantAttention: core.AttentionLateFunds, watch: "long"},
		{name: "expired: the pieces already flagged are not flagged again (replay)", status: core.StatusExpired, attention: core.AttentionLateFunds, mark: "1.5",
			transfers: []transfer{late(confirmed("0.75", 300)), late(confirmed("0.75", 301))}, head: 9000, chainTime: tooLate,
			want: core.StatusExpired, events: noChange, received: "1.5", confirmed: "1.5", wantAttention: core.AttentionLateFunds},
		{name: "expired: after a flag the count starts again; one more piece is not enough", status: core.StatusExpired, attention: core.AttentionLateFunds, mark: "1.5",
			transfers: []transfer{late(confirmed("0.75", 300)), late(confirmed("0.75", 301)), late(pending("0.75", 302))}, head: 316, chainTime: tooLate,
			want: core.StatusExpired, events: noChange, received: "2.25", confirmed: "2.25", promoted: 1, wantAttention: core.AttentionLateFunds},
		{name: "expired and resolved: pieces since the resolution add up → the issue reopens", status: core.StatusExpired, attention: core.AttentionLateFunds, resolved: true, mark: "1.5",
			transfers: []transfer{late(confirmed("0.75", 300)), late(confirmed("0.75", 301)), late(confirmed("0.75", 302)), late(pending("0.75", 303))}, head: 317, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentLateFunds), received: "3", confirmed: "3", promoted: 1, wantAttention: core.AttentionLateFunds},
		{name: "paid: sub-dust extras after settlement add up → late funds", status: core.StatusPaid, mark: "149",
			transfers: []transfer{confirmed("149", 100), confirmed("0.5", 200), pending("0.5", 201)}, head: 215,
			want: core.StatusPaid, events: ev(core.EventPaymentLateFunds), received: "150", confirmed: "150", promoted: 1, wantAttention: core.AttentionLateFunds},
		{name: "deadline: dust in time plus dust late, final, together not dust: expired with late funds", status: core.StatusAwaiting,
			transfers: []transfer{confirmed("0.6", 100), late(pending("0.6", 300))}, head: 314, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentExpired, core.EventPaymentLateFunds), received: "1.2", confirmed: "1.2", promoted: 1,
			wantAttention: core.AttentionLateFunds, watch: "long"},
		{name: "deadline: dust in time plus dust late that is still dust together: plain expired", status: core.StatusAwaiting,
			transfers: []transfer{confirmed("0.3", 100), late(pending("0.3", 300))}, head: 314, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentExpired), received: "0.6", confirmed: "0.6", promoted: 1, watch: "short"},
		{name: "link payment, expired: a late full payment is noticed and watched for long", status: core.StatusExpired, link: true,
			transfers: []transfer{late(pending("149", 300))}, head: 314, chainTime: tooLate,
			want: core.StatusExpired, events: ev(core.EventPaymentLateFunds), received: "149", confirmed: "149", promoted: 1,
			wantAttention: core.AttentionLateFunds, watch: "long"},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p := core.Payment{
				ID: "pay_1", Status: r.status, Amount: usdt("149"), Asset: core.USDTBSC,
				Address: coretest.Address("deposit"), RequiredConfirmations: 15,
				Metadata:  map[string]string{},
				Attention: r.attention,
				CreatedAt: t0, ExpiresAt: expires, UpdatedAt: t0, WatchUntil: expires.Add(grace + unfunded),
			}
			if r.tolerance != "" {
				p.Tolerance = usdt(r.tolerance)
			}
			if r.link {
				p.LinkID = "link_1"
			}
			if r.resolved {
				p.Resolution = &core.Resolution{At: t0.Add(time.Hour), By: "owner", Note: "done"}
			}
			if r.status.Settled() {
				p.SettledAt = t0.Add(40 * time.Minute)
			}
			if r.cancelled {
				p.CancelledAt, p.CancelledBy = p.SettledAt, "owner@example.com"
			}
			if r.status.Settled() {
				for _, tr := range r.transfers {
					if tr.status == core.TransferConfirmed {
						p.LateFundsMark = p.LateFundsMark.Add(usdt(tr.amount))
					}
				}
				if r.mark != "" {
					p.LateFundsMark = usdt(r.mark)
				}
			}
			var transfers []core.Transfer
			for i, tr := range r.transfers {
				transfers = append(transfers, core.Transfer{
					TxHash: coretest.Hash("tx", i), LogIndex: uint(i), BlockHash: coretest.BlockHash(tr.block, 0),
					PaymentID: p.ID, BlockNumber: tr.block, From: coretest.Address("payer", i), To: p.Address,
					Amount: usdt(tr.amount), Status: tr.status, SeenAt: t0,
					BlockTime: tr.blockTime, NotBefore: tr.notBefore,
				})
				if !tr.seenAt.IsZero() {
					transfers[i].SeenAt = tr.seenAt
				}
			}
			now := wall
			if !r.wallClock.IsZero() {
				now = r.wallClock
			}
			in := core.EvalInput{
				Head:     core.ChainHead{ChainID: 56, Number: r.head, Hash: coretest.BlockHash(r.head, 0), Time: inTime, Final: r.head - r.unfinal},
				Now:      now,
				Deadline: !r.noDeadline,
				Grace:    grace, WatchWindow: window, UnfundedWatchWindow: unfunded,
			}
			if !r.chainTime.IsZero() {
				in.Head.Time = r.chainTime
			}
			if r.otherChain {
				in.Head.ChainID = 97
			}
			before := fmt.Sprintf("%+v %+v", p, transfers)

			got := core.Evaluate(p, transfers, in)

			if after := fmt.Sprintf("%+v %+v", p, transfers); after != before {
				t.Fatalf("Evaluate modified its inputs")
			}
			q := got.Payment
			if q.Status != r.want {
				t.Errorf("status = %s, want %s", q.Status, r.want)
			}
			if !slices.Equal(got.Events, r.events) {
				t.Errorf("events = %v, want %v", got.Events, r.events)
			}
			if q.AmountReceived != usdt(r.received) || q.AmountConfirmed != usdt(r.confirmed) {
				t.Errorf("received %s confirmed %s, want %s and %s", q.AmountReceived.Format(18), q.AmountConfirmed.Format(18), r.received, r.confirmed)
			}
			if len(got.Transfers) != r.promoted {
				t.Errorf("%d transfers promoted, want %d", len(got.Transfers), r.promoted)
			}
			for _, tr := range got.Transfers {
				if tr.Status != core.TransferConfirmed || !tr.ConfirmedAt.Equal(now) {
					t.Errorf("promoted transfer: %+v", tr)
				}
			}
			if q.Attention != r.wantAttention {
				t.Errorf("attention = %q, want %q", q.Attention, r.wantAttention)
			}
			if (q.Resolution != nil) != r.wantResolved {
				t.Errorf("resolution = %+v, want resolved %v", q.Resolution, r.wantResolved)
			}
			if q.NeedsAttention() != (r.wantAttention != "" && !r.wantResolved) {
				t.Errorf("NeedsAttention = %v", q.NeedsAttention())
			}
			if !q.CancelledAt.Equal(p.CancelledAt) || q.CancelledBy != p.CancelledBy {
				t.Errorf("the state machine touched the cancellation: %v by %q", q.CancelledAt, q.CancelledBy)
			}

			// Bookkeeping that follows from the outcome.
			changedStatus := q.Status != r.status
			if changedStatus != (len(r.events) > 0 && r.events[0] != core.EventPaymentLateFunds) {
				t.Errorf("status changed = %v but events = %v", changedStatus, r.events)
			}
			if q.Status.Settled() == q.SettledAt.IsZero() {
				t.Errorf("SettledAt = %v for status %s", q.SettledAt, q.Status)
			}
			if changedStatus && q.Status.Settled() && !q.SettledAt.Equal(now) {
				t.Errorf("SettledAt = %v, want %v", q.SettledAt, now)
			}
			wantPaidAt := changedStatus && (q.Status == core.StatusPaid || q.Status == core.StatusOverpaid)
			if wantPaidAt != q.PaidAt.Equal(now) {
				t.Errorf("PaidAt = %v for status %s", q.PaidAt, q.Status)
			}
			if got.Changed != !reflect.DeepEqual(clearBookkeeping(q), clearBookkeeping(p)) {
				t.Errorf("Changed = %v does not match the difference", got.Changed)
			}
			// A changed payment stays watched — for the
			// long window if it is funded (orphaned funds count), for the
			// short one if its status changed, and no longer otherwise.
			var orphanedSum core.Amount
			for _, tr := range r.transfers {
				if tr.status == core.TransferOrphaned {
					orphanedSum = orphanedSum.Add(usdt(tr.amount))
				}
			}
			watch := r.watch
			if watch == "" {
				switch {
				case q.Funded() || (!r.link && orphanedSum.Cmp(usdt("1")) >= 0):
					watch = "long"
				case len(got.Events) > 0:
					watch = "short"
				default:
					watch = "same"
				}
			}
			// Counted from the later of the two clocks: a window is judged
			// by both, and one counted from a clock that is behind the
			// other would end early.
			from := now
			if in.Head.ChainID == p.Asset.ChainID && in.Head.Time.After(now) {
				from = in.Head.Time
			}
			wantWatch := p.WatchUntil
			switch watch {
			case "long":
				wantWatch = from.Add(window)
			case "short":
				wantWatch = from.Add(unfunded)
			}
			if p.WatchUntil.After(wantWatch) {
				wantWatch = p.WatchUntil
			}
			if got.Changed && (!q.UpdatedAt.Equal(now) || !q.WatchUntil.Equal(wantWatch)) {
				t.Errorf("a changed payment must be touched and watched until %v: updated %v, watch until %v", wantWatch, q.UpdatedAt, q.WatchUntil)
			}
			if !got.Changed && (len(got.Events) > 0 || !reflect.DeepEqual(q, withMetadata(p))) {
				t.Errorf("an unchanged payment must be returned as is, without events")
			}
			wantPending := uint64(0)
			live := 0
			for _, tr := range r.transfers {
				if tr.status == core.TransferOrphaned {
					continue
				}
				live++
				if tr.status == core.TransferPending && !promotedBlock(got.Transfers, tr.block) {
					wantPending = max(wantPending, tr.block)
				}
			}
			if q.PendingBlock != wantPending {
				t.Errorf("PendingBlock = %d, want %d", q.PendingBlock, wantPending)
			}
			if (q.TxHash != "") != (live > 0) || (q.FromAddress != "") != (live > 0) {
				t.Errorf("TxHash %q / FromAddress %q with %d live transfers", q.TxHash, q.FromAddress, live)
			}

			// Replays converge: evaluating the result again changes nothing.
			after := slices.Clone(transfers)
			for _, promoted := range got.Transfers {
				for i := range after {
					if after[i].Key() == promoted.Key() {
						after[i] = promoted
					}
				}
			}
			again := core.Evaluate(q, after, in)
			if again.Changed || len(again.Events) != 0 || len(again.Transfers) != 0 || !reflect.DeepEqual(again.Payment, q) {
				t.Errorf("re-evaluation is not a no-op: %+v", again)
			}
			// The order facts arrive in does not matter.
			reversed := slices.Clone(transfers)
			slices.Reverse(reversed)
			if other := core.Evaluate(p, reversed, in); !reflect.DeepEqual(other.Payment, q) || !slices.Equal(other.Events, got.Events) {
				t.Errorf("the order of transfers changed the outcome: %+v", other)
			}
		})
	}
}

func promotedBlock(promoted []core.Transfer, block uint64) bool {
	for _, t := range promoted {
		if t.BlockNumber == block {
			return true
		}
	}
	return false
}

// clearBookkeeping removes the fields Evaluate only sets because something
// else changed.
func clearBookkeeping(p core.Payment) core.Payment {
	p = withMetadata(p)
	p.UpdatedAt, p.WatchUntil = time.Time{}, time.Time{}
	return p
}

func withMetadata(p core.Payment) core.Payment {
	if p.Metadata == nil {
		p.Metadata = map[string]string{}
	}
	return p
}

func TestPaymentConfirmations(t *testing.T) {
	p := core.Payment{RequiredConfirmations: 15}
	if got := p.Confirmations(1000); got != 0 {
		t.Errorf("nothing received: %d", got)
	}
	p.PendingBlock = 100
	for head, want := range map[uint64]int{0: 0, 99: 0, 100: 1, 101: 2, 113: 14, 114: 15, 115: 15, 1 << 40: 15} {
		if got := p.Confirmations(head); got != want {
			t.Errorf("pending in block 100 under head %d: %d confirmations, want %d", head, got, want)
		}
	}
	p.PendingBlock, p.AmountConfirmed = 0, core.NewAmount(1)
	if got := p.Confirmations(0); got != 15 {
		t.Errorf("everything final: %d", got)
	}
	p = core.Payment{Amount: core.NewAmount(10), AmountReceived: core.NewAmount(4)}
	if p.AmountDue() != core.NewAmount(6) {
		t.Errorf("AmountDue = %s", p.AmountDue())
	}
	p.AmountReceived = core.NewAmount(40)
	if !p.AmountDue().IsZero() {
		t.Errorf("AmountDue of an overpaid payment = %s", p.AmountDue())
	}
}
