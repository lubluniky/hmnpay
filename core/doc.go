// Package core is the hmnpay engine: the domain model, the business rules
// and the use cases of a self-hosted, single-tenant, non-custodial crypto
// payment gateway — independent of any transport, database or chain
// library. It imports only the standard library and performs no I/O.
//
// # Architecture
//
//	  inbound adapters                    core                     outbound consumers
//	┌──────────────────┐        ┌──────────────────────┐        ┌────────────────────┐
//	│ REST API         │        │       Service        │        │ webhook dispatcher │
//	│ dashboard        │ calls  │  use cases + rules   │ event  │ SSE to checkout    │
//	│ hosted checkout  ├───────▶│  payment state       ├───────▶│ Telegram notifier  │
//	│ Telegram bot     │        │  machine (Evaluate)  │  log   │ polling clients    │
//	│ CLI / Go embed   │        │                      │        │ anything future    │
//	│ chain watcher    │        └──────────┬───────────┘        └────────────────────┘
//	└──────────────────┘                   │ ports
//	                      Store · AddressSource · PasswordHasher · ChainProber · Clock
//
// Every way into the system calls the same [Service]; there are no side
// doors. A transport's whole job is to decode a request, authenticate the
// caller into an [Actor] ([Service.AuthenticateAPIKey],
// [Service.AuthenticateSession], or [SystemActor] for trusted in-process
// code), put it on the context with [WithActor], call one Service method
// and encode the result. Authorisation happens inside the Service, so a
// rule cannot be forgotten by — or differ between — transports.
//
// Every way out consumes the same durable event log. Each state change
// appends its events in the same [Store] transaction that changes the
// state (a transactional outbox), so an event exists if and only if the
// change committed. Consumers read the log with a cursor
// ([Service.Events], at-least-once) and may use [Service.Subscribe] as a
// low-latency in-process nudge ([Service.SubscribePayment] for the events
// of one payment). The webhook dispatcher is just such a consumer whose
// cursor and fan-out rule live in the core
// ([Service.PlanWebhookDeliveries]). A dispatcher works on claims: each
// claimed delivery carries a fencing token, and only a result reported
// under the delivery's current claim is counted — a worker that outlives
// its lease can neither double-count an attempt nor overwrite a newer
// result ([Service.CompleteWebhookDelivery]). First attempts to one
// endpoint are handed out in event order, one at a time, and every webhook
// body carries the event's log position ([EventEnvelope.Seq]) for the
// receiver to order by — retries are not ordered.
//
// # Transactions
//
// All state lives behind the [Store] port. A use case is one
// Store.Update transaction — read, decide, write, append events — and
// Update transactions are serializable, so the core needs no locks of its
// own and is safe for concurrent use by construction. Deposit-address
// indexes, idempotency keys and payment transitions are all protected by
// that single property.
//
// # Money
//
// Amounts are integers of the asset's smallest unit ([Amount]), never
// floats. Decimal strings ("149.00") exist only at the boundaries
// ([ParseAmount], [Amount.Format]) and always use the decimals of the
// configured [Asset] — BSC-USD has 18.
//
// # The payment state machine
//
// The chain adapter reports facts; the core alone decides state. There are
// three facts — a transfer is in a block ([Service.ObserveTransfer]), a
// transfer left the canonical chain ([Service.RetractTransfer]), the chain
// has been processed up to a block ([Service.AdvanceHead]) — and one pure
// function, [Evaluate], that derives a payment's state from all of its
// transfers and the chain head. Because the state is derived from the full
// set of facts, replays, duplicates and reordering converge on the same
// result.
//
// Definitions. A transfer is "live" unless it was retracted (orphaned). It
// is "confirmed" — final, for good — once its block is at or below the
// head's final block ([ChainHead.Final]: the newest block the chain has
// finalised, as two endpoints attest it — on a network declared to have no
// finality, none of which is supported, the chain adapter's reorganisation
// window below the head) and the head gives it the payment's
// RequiredConfirmations, counting the including block as the first;
// otherwise it is "pending". The confirmations are a floor, never a
// substitute for finality: what is confirmed can be taken back only by a
// reorganisation that replaces a finalised block.
// Only the payments of the asset being watched are judged by the head. need = Amount − Tolerance, where Tolerance is
// the underpayment tolerance setting snapshotted at creation and capped at
// 5 % of Amount, so need is never less than 95 % of the amount.
//
// The "deadline" has passed when two clocks both read ExpiresAt + grace or
// later: the block timestamp of the processed chain head, and the wall
// clock of this server. Chain time is required — and deadlines are only
// judged while processing a new head — so that a watcher that is behind can
// never expire a payment that was paid in time. The wall clock is required
// so that a wrong block timestamp can never expire anything early: block
// time is whatever an RPC endpoint says it is, and with chain time alone
// one endpoint claiming a block from next month would expire every open
// payment at once. With both, a lying or lagging clock on either side can
// only delay an expiry. (A head whose block time is more than two minutes
// ahead of the wall clock is refused altogether: [ErrHeadTimeAhead].)
//
// The other half of it is when a deadline is set. ExpiresAt, and the end of
// every watch window ([Payment.WatchUntil]), are counted from the later of
// the two clocks: one counted from a clock that is behind the other would
// end early by the other's reckoning, and that is the one that has to pass
// too. A wall clock behind the chain by more than the tolerance for block
// times ([Config.MaxHeadTimeAhead]) makes the chain adapter refuse every
// block, so the processed head cannot show it; the newest block as the
// adapter's endpoints show it can ([ChainStatus.ClockSkew]). While either
// shows the clock that far behind, no payment is created
// ([ErrClockBehind]). Within the tolerance a deadline can be early by up to
// that much. A clock that is behind while the adapter reaches no endpoint
// shows nothing to compare with: what is created then is early by the
// clock's error. With the check switched off nothing is refused, and
// deadlines follow the chain's time wherever it is later.
//
// The same two clocks date a transfer. A live transfer is "late" when it
// provably entered the chain at or after the deadline: its block time is
// at or after it — the block time the chain adapter reported, or, when it
// reported none, the block time of the head the core had already processed
// when the transfer showed up in a later block ([Transfer.NotBefore]) —
// and the core first saw it at or after the deadline by the wall clock.
// Every other live transfer is "in time"; a transfer that cannot be dated
// gets the benefit of the doubt. Only funds in time can open, hold open or
// pay a payment. Late funds are late funds — recorded, flagged, never
// counted — whether or not a head had settled the payment by the time they
// arrived, so the outcome does not depend on how promptly the watcher was
// running.
//
// "Dust" is an amount below a payment's dust limit: one whole unit of the
// asset or 5 % of the amount due, whichever is less ([DustCapUnits],
// [DustDivisor]; 1 USDT for every invoice of 20 USDT and more). Dust is
// recorded on the payment and shown in its transfer list, and otherwise
// ignored: it changes no status, raises no attention, emits no event and
// keeps no address on the watch list. A payment that received only dust
// expires exactly like one that received nothing. Sending dust costs a
// network fee and nothing else, so nothing may hang on it.
//
// The rule is about sums, never about single transfers: what is compared
// with the dust limit is always the total of the funds in question — the
// funds that arrived in time while the payment is open, the confirmed funds
// nobody has been told about once it is settled. A hundred transfers of a
// cent are a unit, not a hundred times dust.
//
// Open statuses follow the funds that arrived in time:
//
//	awaiting    nothing but dust
//	detected    funds seen, but their total is below need
//	            (a partial payment; the payer may top up)
//	confirming  funds cover need; waiting for confirmations
//
// Settled statuses are terminal — the status never changes again:
//
//	paid        confirmed funds ≥ need and ≤ Amount
//	overpaid    confirmed funds > Amount
//	underpaid   deadline passed with confirmed funds short of need
//	            (and more than dust)
//	expired     the payment window closed with nothing paid in time: the
//	            deadline passed with no funds, or only dust, or only late
//	            funds — or the payment was cancelled (then CancelledAt is
//	            set)
//	failed      deadline passed and the funds that had been seen were
//	            orphaned
//
// Transitions (O = ObserveTransfer, R = RetractTransfer, H = AdvanceHead,
// C = CancelPayment):
//
//	from                  fact   guard                               to          event
//	─────────────────────────────────────────────────────────────────────────────────────────────
//	(none)                create                                     awaiting    payment.created
//	awaiting              O      dust ≤ live < need                  detected    payment.detected
//	awaiting              O      live < dust, or the transfer late   (unchanged) —
//	awaiting, detected    O      live ≥ need                         confirming  payment.confirming
//	any open              O, H   confirmed ≥ need, ≤ Amount          paid        payment.paid
//	any open              O, H   confirmed > Amount                  overpaid    payment.overpaid
//	confirming            R      dust ≤ live < need                  detected    payment.detected
//	detected, confirming  R      live < dust                         awaiting    payment.awaiting
//	any open              H      deadline, a transfer in time (not   (unchanged) —
//	                             dust) is pending
//	awaiting              C      nothing live but dust               expired     payment.expired
//	detected, confirming  C      (refused: ErrPaymentHasFunds)       (unchanged) —
//	any settled           C      (refused: ErrPaymentSettled; a      (unchanged) —
//	                             cancelled payment: no-op)
//	any open              H      chain time past the deadline but    (unchanged) —
//	                             the wall clock not, or vice versa
//	awaiting              H      deadline; nothing, dust or late     expired     payment.expired
//	                             funds only
//	awaiting              H      deadline, only orphaned funds       failed      payment.failed
//	detected              H      deadline, dust ≤ confirmed < need,  underpaid   payment.underpaid
//	                             nothing in time pending
//	settling, any of the  H      confirmed funds the outcome does    as above    + payment.late_funds
//	four rows above              not account for are not dust
//	any settled           O, H   the confirmed funds not yet         (unchanged) payment.late_funds
//	                             flagged are not dust (any more)
//	any settled           R      a pending transfer is orphaned      (unchanged) —
//	settled, with issue   ResolvePayment                             (unchanged) payment.resolved
//	settled, resolved     ReopenPayment                              (unchanged) payment.reopened
//
// A status can be skipped: a transfer first reported with enough
// confirmations takes the payment from awaiting straight to paid, emitting
// only payment.paid.
//
// In the table "live", "confirmed" and "pending" mean funds in time, and
// "deadline" means both clocks.
//
// The decisions behind the table:
//
//   - Several transfers to one address are summed. A transfer is identified
//     by (tx hash, log index, block hash); reporting it again is a no-op.
//   - Underpayment tolerance: confirmed funds within Tolerance below Amount
//     settle as paid. There is no tolerance above: any excess is overpaid.
//     The tolerance is an absolute amount meant to absorb a small shortfall
//     on a normal-sized invoice (an exchange deducting its withdrawal fee),
//     so it is never applied in full to a small one: each payment's
//     tolerance is the setting capped at 5 % of its amount
//     ([ToleranceCapDivisor]). A payment therefore always needs at least
//     95 % of its amount, whatever the setting and whoever chose the amount
//     (on a customer-chooses payment link the payer does) — dust never
//     settles anything.
//   - A short payment is not settled as underpaid while the payment is
//     still open; it sits in detected so that a top-up completes it. Only
//     when the deadline passes does it settle as underpaid. A top-up made
//     after the deadline does not complete it: it is late funds.
//   - A payment is settled as soon as its confirmed funds cover need, even
//     if another transfer is still pending — nobody can stall a settlement
//     by sending dust to the address.
//   - A payment never expires mid-confirmation: a pending transfer that
//     arrived in time holds it open past its deadline until that transfer
//     confirms or is orphaned. Dust and late transfers hold nothing open,
//     so nobody can keep a payment open by feeding it transfers.
//   - Dust never needs anybody's attention: a dusted invoice expires as
//     "expired", not "underpaid", and dust that arrives after settlement is
//     no late funds. A real partial payment — one unit and more on any
//     invoice of 20 units and more — always surfaces, as detected, then
//     underpaid with attention, or as late funds, in however many transfers
//     it arrives: after settlement the confirmed funds that have not been
//     flagged yet are added up ([Payment.LateFundsMark]), and they are
//     flagged the moment their total is not dust. What is added up starts
//     again from nothing with every flag and with every resolution by an
//     operator. When a payment expires or fails, whatever is confirmed on
//     it by then — dust that came in time, funds that came late — is one
//     such total.
//   - A reorganisation that removes a pending transfer simply stops it
//     counting. While open, the payment falls back to detected or awaiting
//     and can still be paid (typically the same transaction is mined again
//     and observed again). Deadline outcomes are only decided on the next
//     AdvanceHead, after the watcher has rescanned, so a transaction that
//     is re-included is not mistaken for a failure.
//   - Confirmed is final. A fact that contradicts a confirmed transfer is
//     refused with [ErrTransferFinal]; the operator chooses the finality
//     depth with the confirmation setting.
//   - Terminal means terminal. Funds that confirm after settlement — a late
//     payment on an expired invoice, a second payment, a late top-up — are
//     never lost and never silently change an outcome the merchant has
//     already been told about: they are added to AmountReceived and
//     AmountConfirmed, raise [AttentionLateFunds], re-open a resolved
//     issue and emit payment.late_funds.
//   - The deposit address stays on the watch list while the payment is open
//     and for a window afterwards, and the window depends on whether the
//     payment is funded ([Payment.Funded]). Funded: seven days after the
//     payment last changed ([Config.WatchWindow]). Not funded: one hour
//     after its status last changed ([Config.UnfundedWatchWindow]). A
//     payment the merchant created is funded by anything that is not dust.
//     A payment created through a public payment link is funded only by
//     half its amount and at least one whole unit, because anybody can
//     create those, in any number: keeping one of them on the list for a
//     week must cost real money, not a network fee. The consequence, on
//     purpose: money that reaches the address of a payment that closed
//     unfunded more than an hour after it closed is never noticed — an
//     expired invoice nobody paid, a link checkout that got less than half.
//     An hour catches the payer who is minutes late; it does not catch the
//     one who pays an old invoice the next day. (Late funds that are
//     noticed make the payment funded, and it is then watched for the long
//     window.) A window is a promise about blocks — money in a block stamped
//     before it ends is seen — and an address leaves the list on evidence
//     that those blocks have been scanned for it, never by a clock alone
//     ([Payment.RetiredAt], recorded in [Service.AdvanceHead]): a processed
//     head of the payment's network and asset whose final block's time
//     ([ChainHead.FinalTime]), less [WatchRetirementMargin], is past the end
//     of the window — a head that does not say when its final block was
//     mined retires nothing: unknown is not past; every block
//     stamped before the end scanned by two log endpoints
//     ([ChainHead.Checked]); and the wall clock past the end too. The chain
//     adapter scans a block some time after it was mined — after a pause,
//     an outage, or a while on another network or asset, during which no
//     head of this one is processed and nothing of it leaves the list.
//     Addresses kept only for a second log endpoint's scan are bounded
//     ([Config.MaxRetainedUnchecked]); those let go beyond the bound are
//     marked ([Payment.RetiredUnchecked]) and counted in the node status.
//   - What is on the watch list is not all watched with the same urgency.
//     An open payment has a payer, a deadline and perhaps a confirmation
//     count on a screen. A settled payment whose transfers are all final
//     has none of these: its address is "closed"
//     ([WatchedAddress.Closed]), and all the chain can still do to it is
//     late funds. While the list holds closed addresses only — and no
//     transfer anywhere is pending ([ChainState.Active] is false) — the
//     chain watcher scans at its idle interval (30 seconds by default)
//     instead of every 3: every block, for every address, as before, only
//     less often. Late funds on a closed payment are therefore noticed up
//     to that interval later; from the moment they are noticed the watcher
//     is at its normal pace until they are final. Nothing else is delayed:
//     creating a payment makes the list active and wakes the watcher in
//     the same moment, and no other operation — cancelling, resolving,
//     reopening a resolution — turns a settled payment back into one the
//     chain can decide anything about.
//   - Cancelling ([Service.CancelPayment]) is closing the payment window
//     early. It is allowed only while the payment is awaiting with nothing
//     but dust on its address — funds in flight are never cancelled under
//     the payer's feet — and settles the payment as expired with CancelledAt
//     set, emitting payment.expired. It is not a ninth status and not a
//     new event: a cancelled invoice needs exactly the handling of an
//     expired one (release the order, stop showing the address), every
//     existing payment.expired subscription and every exhaustive switch
//     over the eight statuses keeps working, and consumers that care can
//     tell the two apart by cancelled_at. Being settled, it follows the
//     rule above: a transfer that arrives afterwards — broadcast as the
//     payment was cancelled, or reorganised out before and mined again —
//     is late funds needing attention, never a silent loss.
//   - "Needs attention" ([Payment.NeedsAttention]) is an issue — underpaid,
//     overpaid, failed or late funds — that no operator has resolved.
//     [Service.ResolvePayment] records who closed it and why;
//     [Service.ReopenPayment] undoes that. Neither changes the status.
//
// # Payment links
//
// A payment link turns anonymous visitors into payments, so three limits
// surround [Service.CreatePaymentFromLink], the one unauthenticated way to
// create a payment.
//
// Uses. A link with MaxUses counts paid payments (Uses) and the checkouts
// that hold a use, and refuses a new checkout when the two reach MaxUses.
// A checkout holds a use for exactly as long as it can still become a paid
// use: from its creation until its deadline has passed by both clocks, and
// beyond that only while it is confirming (the full amount arrived in time
// and is waiting for confirmations). Because that would let one open
// checkout block a use for the whole payment TTL, a checkout of a limited
// link is short-lived: it expires after [Config.LinkReservation] (ten
// minutes) instead of the payment TTL — "you have ten minutes to pay; then
// the place is released and this checkout is closed", which the checkout
// view states as its ExpiresAt. A link without MaxUses is unaffected.
//
// The result is an invariant, not a compromise: Uses plus the checkouts
// holding a use never exceeds MaxUses, and a payment can only become paid
// while it holds one, so a limited link collects at most MaxUses paid
// payments. Whoever holds the address of a closed checkout can still send
// money to it — that cannot be prevented on a public chain — and it lands
// as late funds on an expired payment, flagged for the merchant to refund
// or honour, never as a use. What remains:
//
//   - Squatting. Someone who keeps opening MaxUses fresh checkouts every
//     ten minutes keeps a limited link at "limit reached" without paying.
//     Transports cap what one client may create per link.
//   - The watcher. Uses are released by chain time, so a limited link frees
//     nothing while the chain watcher is not processing heads; during such
//     an outage no payment would be detected either.
//   - Undatable transfers. A transfer that the adapter reports without a
//     block time in a block at or below the head the core has already
//     processed — which only a chain reorganisation produces — cannot be
//     proven late and is counted. An adapter that reports block times
//     ([TransferObservation.BlockTime]) closes this.
//
// With LinkReservation negative, checkouts of limited links keep the
// payment TTL and hold their use that long.
//
// Watch list. Every payment's address is watched while the payment is open
// and for a window afterwards (see above: an hour unless funded). The
// links' share of the list is capped ([Config.MaxWatchedAddresses]): when
// that many payments created through links are open or inside their
// window, further link payments are refused with [ErrWatchListFull] rather
// than created and silently not watched. Payments created by the merchant
// are never refused and do not count against the cap, so nothing done to
// the links can reach the merchant's own payments, and the merchant's
// volume cannot close the links. [NodeStatus.WatchListFull] reports the
// condition. Filling the share takes 5 000 link checkouts that are open or
// closed within the last hour, or half the amount, and at least one unit,
// paid to the merchant on each. (With the default 30-minute payment TTL the
// HTTP adapter's creation budget of one link payment per second gets there
// after some eighty minutes of a flood at that full rate — a flood that has
// taken the links' creation budget from real customers all along. The
// links are then closed until addresses age out, and the merchant's
// payments are not.)
//
// The cap is what it is because the watch list is not free: every 500
// watched addresses cost the chain watcher one eth_getLogs call per tick on
// each of two RPC endpoints. 5 000 addresses that anybody can create are
// ten log queries per tick and endpoint, about four requests a second;
// 20 000, the cap before, were fourteen a second — enough to make a free
// endpoint throttle, and a throttled endpoint delays the merchant's own
// payments too. The merchant's own payments are not capped: an operator
// who keeps tens of thousands of invoices open needs endpoints to match.
//
// Amounts. On a customer-chooses link the payer picks the amount, down to
// the link's minimum: [PaymentLink.MinAmount], or one whole unit of the
// asset if the link sets none — and a link cannot set less
// ([LinkMinAmountUnits]). Less is refused when the checkout is opened, with
// a validation error on "amount". The minimum is what makes a use of a
// limited link cost something: a use is a paid payment, and without it
// every use could be taken for one base unit and a network fee. With it a
// use costs at least the minimum less the capped tolerance, and that money
// is the merchant's. A fixed price is the merchant's own choice and has no
// minimum; a link payment under one whole unit — only a fixed price can
// produce one — is paid like any other, but is never "funded" and leaves
// the watch list an hour after its deadline.
//
// # First-run setup and sessions
//
// Whoever completes setup owns the instance, including the wallet every
// payment goes to. Server wiring therefore starts the service with a setup
// token ([Config.SetupToken]) that [Service.CompleteSetup] and the wizard's
// helpers demand from anonymous callers until setup is done. Recovering a
// lost password ([Service.ResetOperatorPassword]) changes the password, ends
// every session and nothing else; [Service.InstanceSummary] shows what an
// instance is really configured for and [Service.RevokeAllAPIKeys] ends
// every API key.
//
// "Ends" is meant for whatever is already on its way as well. A login that
// was verifying the old password while the reset committed does not get a
// session: the session is opened only for the credentials that are the
// account's at that moment. And every write checks, in its own transaction,
// that the session or API key the caller was authenticated with still holds
// — revoked, ended, or run out by the clock at the moment the transaction
// writes, not when it began to wait for the store — so a request that was
// authenticated a moment before its session was ended, its key revoked or
// either expired, changes nothing afterwards.
//
// Logins are throttled by the transports, and the throttles that stop
// distributed password guessing are necessarily shared, so anonymous
// traffic can exhaust them. Two things keep the operator from being locked
// out by that. Transports may remember a browser that has logged in and
// exempt it; what they remember must be bound to [Service.DeviceBinding],
// which moves with every password change and reset, so that old browsers
// are forgotten the moment the password is. And for a browser the instance
// has never seen there is the one-time recovery code
// ([Service.MintRecoveryCode], printed by a command run on the server —
// "hmnpay recovery-code", and "hmnpay reset-password" with the new
// password): presented with a login it lets the transport skip the shared
// throttles. It is no credential — the password is verified as
// always — and it is bounded by itself: fifteen minutes, one successful
// login, five attempts.
//
// A dashboard session ends at its absolute expiry or after an idle timeout
// without use, whichever comes first ([Config.SessionTTL],
// [Config.SessionIdleTimeout]). Password operations are slow by design; the
// service runs only a few at a time ([Config.MaxConcurrentKDF]) and answers
// [ErrBusy] beyond that.
package core
