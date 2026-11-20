// Package watcher turns an EVM chain, seen through untrusted JSON-RPC
// endpoints, into three kinds of facts about one ERC-20 token:
//
//   - a transfer to a watched address was observed,
//   - a previously reported transfer is no longer on the canonical chain,
//   - the chain head advanced.
//
// The watcher reports facts and nothing else. It never decides that a payment
// is "paid": counting confirmations, matching amounts and expiring payments
// belong to the consumer of the facts (the core).
//
// # Interfaces
//
// The watcher consumes three small interfaces, defined here so that the core
// can be adapted to them: Sink (receives the facts), WatchSet (which
// addresses to watch) and Store (durable checkpoint). Their documentation is
// the contract between the two sides.
//
// # Algorithm
//
// Each Tick:
//
//  1. Pick one endpoint (the "primary") and fetch, in one request, its latest
//     header, the finalized header, and the headers at the numbers of the two
//     blocks the watcher remembers best: its newest anchor and its finalized
//     anchor.
//  2. Pick a second endpoint (the "verifier") and fetch, in one request, its
//     latest header, its finalized header and the header of the block the
//     cursor is expected to reach. From here on the height of the chain is
//     the lower of the two endpoints' heads: a block only one endpoint claims
//     to have does not count. If the two heads are more than SettleDepth
//     apart and there is a third endpoint, it is asked for its head too, and
//     the one of the three that is behind the other two is set aside (see
//     "The height of the chain"). A tip stamped more than
//     Config.MaxFutureDrift ahead of the local clock is refused. (With a
//     single configured endpoint the primary is its own verifier; see "Trust
//     model".)
//  3. Is this the chain we know? If the checkpoint claims more than the
//     chain has, see "A checkpoint above the chain". Then the reorg check: an
//     anchor is a (number, hash, time) triple of a block both endpoints
//     agreed on in an earlier tick. A block hash commits to all its
//     ancestors, so if the newest anchor is still canonical nothing below it
//     has changed. If it is not, walk back to the newest anchor that is:
//     everything above it is scanned again.
//  4. Only now take a snapshot of the watch set. Every block at or below the
//     head existed before the snapshot, which is what closes the race with
//     newly created payment addresses (see WatchSet).
//  5. Fetch the header of the block the cursor should reach in this tick
//     (SettleDepth below the chain's height) from both endpoints. Number,
//     hash, parent hash and timestamp must be identical. This happens before
//     the scan, so that the header vouches for everything the scan is about
//     to read.
//  6. Scan the blocks above the cursor with eth_getLogs on the primary for
//     transfers of the token to the watched addresses, in steps the provider
//     accepts, persisting progress after each step. Diff against the
//     transfers already reported for those blocks. New ones are cross-checked
//     against their transaction receipt from another endpoint, recorded in
//     the checkpoint, and then reported as observed. Missing ones are
//     reported as reverted only with proof: the canonical block at that
//     height has a different hash than the one the transfer was reported
//     under, according to both endpoints. A log missing from a block that is
//     still canonical is not a reorg but a faulty endpoint: the tick fails
//     and the endpoint is set aside.
//  7. Settle. The log scan of the blocks about to be settled is repeated on
//     a second endpoint that serves logs. A transfer found only there goes
//     through the same receipt check and is reported; a reported transfer
//     that is not there is attested again, or taken back, or holds the head
//     (see "Invariants"). Then the cursor — the processed head — moves onto
//     the block agreed on in step 5, which becomes the newest anchor. It
//     never moves to or past a block holding a transfer that could not be
//     verified or a revert that could not be proven, never above SettleDepth
//     below the lowest height any endpoint involved has reached, and (inside
//     the reorg window) never onto a block whose header the two endpoints do
//     not agree on. Blocks near the tip are thus scanned again on every tick
//     until they are SettleDepth deep; the last scan of a block is the one
//     that counts.
//  8. Report the processed head — the cursor's block, with the agreed hash
//     and timestamp — to the sink.
//
// Transfers are reported as soon as they are seen and verified, typically
// within one tick of being mined. The head is reported only for what is
// settled. A consumer that counts confirmations from the reported head, and
// lets payments expire against its timestamp, therefore never acts on a
// block whose scan could still change: when HeadAdvanced(h) is called,
// every transfer to a watched address in any block up to h has been
// delivered. (One exception, in configurations where a single endpoint
// serves logs: invariant 3.) The price is latency: the reported head trails
// the chain by SettleDepth blocks.
//
// The cursor only moves after the sink accepted everything below it and the
// checkpoint was saved, so after a crash or downtime the watcher resumes
// exactly where it was and re-reports rather than skips (at-least-once).
//
// A block the cursor is put on without anything being settled — the start
// position of a watcher without a checkpoint, the position a checkpoint far
// below everything is moved up to — is remembered as well, once both
// endpoints have agreed on its header (which they must before it is reported
// as the processed head): the newest anchor is never below the cursor for
// longer than it takes the endpoints to agree on the cursor's block.
// Otherwise a reorganisation below such a cursor, before the next block is
// settled, would go unnoticed, and the scan would go on above a block that
// has been replaced.
//
// # The height of the chain
//
// Where the chain ends decides what is scanned, what is settled, where a
// watcher without a checkpoint starts and which endpoint counts as behind. It
// is never taken from one endpoint:
//
//   - The tick's height is the lower of the primary's and the verifier's
//     head. Nothing above it is settled, and nothing more than two settle
//     depths above it is scanned.
//   - When the two are more than SettleDepth apart, a third endpoint decides
//     if there is one: the height is then the highest that two of the three
//     have reached. An endpoint in use that is more than SettleDepth below
//     it is set aside — the verifier is replaced in the same tick, a primary
//     costs the tick — and an endpoint that claims more than the other two
//     is simply not followed. With two endpoints there is nobody to ask; the
//     head waits at what both have, and says which endpoint is behind which.
//   - The height an endpoint reports together with its log answers (its log
//     index) can only lower what is settled on its logs. If it stays behind
//     the chain the endpoints agree on, nothing above it can be scanned:
//     that is named, and after holdAlertTicks ticks the endpoint is set
//     aside as the source of logs.
//   - A position is derived from the heads — the start of a watcher without
//     a checkpoint, the place a checkpoint that lies above the chain is put
//     back to — only when the two heads are within maxStartSpread (64)
//     blocks of each other, or a third endpoint has been asked. The lower of
//     two heads that are further apart is wherever the endpoint that is
//     behind, or says it is, happens to be.
//   - The pool's idea of who is lagging (evm.Client.Refresh) is measured
//     against the second-highest of the endpoints' block numbers, for the
//     same reason, and never decides whether an endpoint is asked.
//
// # A checkpoint below the consumer
//
// The cursor normally sits at, or just below, the consumer's processed head.
// A checkpoint whose position lies far below everything that still has to be
// scanned is not scanned from. Before the scan of a tick, if no requested
// re-scan is under way, the cursor is moved up to the higher of
//
//   - a reorg window below the consumer's processed head (Snapshot.Processed):
//     the consumer holds every transfer up to its head, and what lies more
//     than a reorg window below it does not change any more;
//   - the lowest Since of the watched addresses, if every address has one
//     (with no address at all: the settled height of the chain);
//
// but never above the settled height of the chain the endpoints agree on,
// nor onto or past the block of a transfer that was reported ahead of the
// head — and only if that skips more than a reorg window
// (IncidentCheckpointBehind). A re-scan somebody asked for — Rescan, a
// rewind for a new address's Since or for a pending transfer of the
// consumer's — or one that follows an accepted finality conflict is
// recorded in the checkpoint and is not skipped, also not after a restart.
//
// A cursor that is far below the chain because the watcher idled there is
// not such a checkpoint: the watcher settled its way to it in this process,
// with both endpoints, and found nothing to watch. The tick after an idle
// pause — however long: Config.IdleInterval may cover more blocks than the
// reorg window — is an ordinary tick: it compares the remembered blocks (and
// goes back to the newest one that is still there, if the chain was
// reorganised meanwhile), scans on from the cursor (which costs nothing while
// nothing is watched) and settles on a header both endpoints agree on, which
// becomes the next remembered block. No incident, and no gap in what is
// remembered.
//
// This is what keeps a wrong position from outliving its cause. What it
// cannot shorten: if a watched address is as old as the wrong position — the
// consumer created it while its own head was down there — the blocks from
// there on are scanned, because the address can have been paid in them.
//
// # What the watch list costs
//
// An eth_getLogs filter takes at most 500 recipient addresses
// (evm.DefaultMaxRecipients), so one scan of N watched addresses is
// ceil(N/500) calls. In the steady state a tick makes one scan on the
// primary (the blocks above the cursor) and one on the second log endpoint
// (the blocks being settled); nothing else it asks for depends on N.
// Measured (TestRequestVolumeByWatchListSize; HTTP requests per tick on the
// primary / on the second log endpoint, and per second at the default 3 s
// tick):
//
//	 1 000 addresses    4.2 /  3.2 per tick    1.4 / 1.1 per second
//	 5 000 addresses   12.2 / 11.2 per tick    4.1 / 3.7 per second
//	20 000 addresses   42.2 / 41.2 per tick   14.1 / 13.7 per second
//
// The growth is linear and has no bound of its own: its bound is the size of
// the watch list. Each request of a large list carries a 35 kB filter, and
// while catching up the same number of calls is made per scan step, with
// CatchUpPause between the steps only. Free public endpoints throttle in the
// range of ten requests a second and count eth_getLogs dearly, and a
// throttled endpoint delays every payment, not only those of the addresses
// that filled the list. The consumer therefore has to bound the part of the
// list that strangers can fill (the core caps the share of its public
// payment links at 5 000 addresses: 10 log queries per tick and endpoint),
// and an operator who watches tens of thousands of addresses of their own
// needs endpoints that allow that many requests.
//
// # Idle
//
// A tick costs requests whether or not there is anything to find, and most of
// the time a small instance has nothing to find. Run therefore pauses
// Config.IdleInterval (30 s) instead of Config.PollInterval (3 s) after a
// tick that left the watcher idle. Nothing else changes: an idle tick is a
// tick like any other — it compares the remembered blocks with both
// endpoints, settles the blocks that have become deep enough and reports the
// processed head — and so is the first tick after an idle pause, which
// confirms that the cursor's block is still on the chain the endpoints agree
// on before it scans on from it. A reorganisation during the pause is handled
// by that tick exactly as one between two ticks three seconds apart, and one
// during a pause longer than the reorg window exactly as one that happened
// while the process was down.
//
// The watcher is idle after a tick (Status.Idle) only if all of this holds;
// Status.Work names the first thing that does not:
//
//   - the tick succeeded, with a second endpoint (or the only one) vouching
//     for the head, and named no reason for holding it (Status.Hold);
//   - the watch set held nothing the consumer is waiting on: no address, or
//     none but closed ones (WatchedAddress.Closed: see "Quiet"), and no
//     transfer the consumer holds as not yet final (Snapshot.Pending);
//   - the tick reported no transfer: one that did is followed by a tick a
//     poll interval later, which asks the consumer what it holds as not
//     final now;
//   - the watcher tracks no transfer of its own (the reorg window has passed
//     over everything it reported), and none is waiting for confirmation;
//   - no re-scan is under way or has been asked for (Rescan, a rewind for an
//     address or a pending transfer, the re-scan after an accepted finality
//     conflict);
//   - no block settled on one endpoint's logs is waiting for a second
//     endpoint to scan it. Those are the blocks in which an address that is
//     still watched — closed or not — can have been paid (invariant 3), so
//     with an empty watch set there are none, whatever range the checkpoint
//     still records. With a single endpoint configured there is no second
//     one to wait for either: the range is recorded for the day one is
//     added, and keeps nothing waiting until then. (With several endpoints
//     of which one serves logs, the others may begin to at any time, and a
//     watcher with addresses to scan for stays at its poll interval);
//   - the watcher is not in, or counting towards, single-source operation,
//     the endpoints do not disagree about where the chain ends, no
//     reorganisation was dealt with in this tick, and none of the counters
//     that lead to an incident (a held head, a log index that is behind, a
//     checkpoint above the chain, a finality conflict) is running;
//   - the cursor is where a watcher in step is — SettleDepth below the height
//     the endpoints agree on — the sink has that head (so there is one: on a
//     chain shorter than the settle depth the watcher keeps its poll
//     interval until the first block is settled), and the consumer's own
//     processed head (Snapshot.Processed) is not behind it;
//   - no endpoint shows a chain that reaches more than two settle depths
//     above the cursor: not in any answer it gave this tick — a head, the
//     height that comes with a log answer — and not to the pool's monitor
//     within the last minute. Such a claim settles nothing; it is enough
//     to keep the watcher from resting.
//
// An idle watcher is woken by Wake, and by Rescan: the pause ends, the next
// tick runs at once, and PollInterval applies again for as long as there is
// anything to do. The consumer calls Wake when it has added to the watch set
// — an address that is not closed, a pending transfer — or when an address
// that was closed is not closed any more (the WatchSet contract is unchanged:
// the address is durably in the set before Wake is called, and the tick that
// follows reads the set after it has fetched the chain's head). A Wake that
// arrives while a tick is running is not lost: if that tick ends idle, the
// next one follows immediately.
//
// What a consumer has to know: while the watcher idles, the processed head it
// was given is up to IdleInterval old. A start block derived from it
// (WatchedAddress.Since) is therefore lower than it would otherwise be, never
// higher — the tick that follows the wake-up scans every block above the
// cursor with the new address, and the cursor is that head.
//
// # Quiet
//
// An address usually stays in the watch set long after the consumer has
// stopped waiting for anything on it: the core keeps the address of a payment
// that was paid for a week, so that money sent to it again is noticed. Such
// an address is closed (WatchedAddress.Closed). Nobody is looking at a
// checkout for it, no deadline runs on it, nothing about it is unconfirmed —
// and ticking every three seconds for its sake costs what an open payment
// costs, around the clock.
//
// A watcher whose watch set holds closed addresses and nothing else is quiet
// (Status.Quiet, with Status.Idle): it is idle in every respect — the same
// conditions, the same IdleInterval, the same Wake — with one difference, and
// that one is not a shortcut: there is something to scan for. A quiet tick is
// a full tick. It scans every block above the cursor for every watched
// address on the primary, repeats the scan of the blocks it settles on a
// second endpoint, and confirms what it finds by receipt, exactly as a tick
// three seconds after the last one would; it only covers IdleInterval's worth
// of blocks in one go instead of PollInterval's. So nothing is missed that
// would otherwise be found: a transfer to a closed address is reported by the
// tick that ends the pause it was mined in — up to IdleInterval later than by
// a watcher that never rests, and that delay is the whole price. A
// reorganisation during the pause is found by the same comparison of
// remembered blocks as ever, before anything is scanned.
//
// Quiet ends like idle ends, and in one more way: by what the watcher finds.
// A tick that reports a transfer is never idle, so the next tick follows a
// poll interval later; by then the consumer lists the transfer as pending
// (or the watcher still tracks it), and the watcher stays at its poll
// interval until the transfer is final for the consumer and has left the
// reorg window. The confirmations of money that arrives late are therefore
// followed as closely as those of any payment.
//
// What "closed" has to mean is the consumer's to say, and the rule is this:
// an address may be closed only if a delay of IdleInterval in learning of a
// transfer to it breaks no promise the consumer has made. An address
// somebody may be paying to on time is not closed.
//
// # Finality conflicts
//
// BSC finalizes blocks one or two blocks behind the head. The watcher
// remembers the highest block the primary reported as finalized and the
// verifier confirmed — the same block at that height, at or below the
// verifier's own finalized block — and, for chains or endpoints without
// finality, a window of anchors. (A watcher that remembers nothing yet — it
// started without a checkpoint — remembers, with the first block it settles
// on, the block a reorg window below that one, or the consumer's final block
// if that is higher: a reorganisation a few blocks deep that replaces the
// block it started on is an ordinary one, not one that replaced the window.)
// An endpoint whose chain contradicts that
// finalized block, or on which no remembered anchor survives, is either
// witnessing a consensus failure or — overwhelmingly more likely — stuck on a
// stale or foreign fork. It is not believed on its own: with a second
// endpoint available, the second one decides (if it confirms the known
// chain, the first is set aside; if it agrees with the first, the new chain
// is accepted). With a single endpoint the contradiction has to persist for
// Config.ConflictPatience consecutive ticks. Accepting is loud: an error is
// logged, Status.DeepReorgs is incremented and Config.OnDeepReorg is called,
// because transfers older than the reorg window cannot be re-examined.
//
// # A checkpoint above the chain
//
// If the checkpoint's cursor (or its newest remembered block) lies above the
// primary's head, the primary is normally just behind and the tick fails.
// If the verifier's chain ends below the checkpoint as well, for three ticks
// in a row, the checkpoint is the one that is wrong: it was written by a
// version that took a single endpoint's word for the height of the chain, or
// the chain was rolled back. The watcher then moves its cursor back to
// SettleDepth below the lower of the two heads, drops every remembered block
// above that, raises IncidentCheckpointAhead and scans again — provided the
// two heads are a height the endpoints agree on (within 64 blocks of each
// other, or put to a third endpoint: "The height of the chain"); otherwise
// the tick fails and says so. Nothing is
// credited by that; transfers it still tracks from blocks that turn out not
// to exist are reverted by the scan like any other vanished transfer. With a
// single endpoint the same happens after Config.ConflictPatience ticks.
//
// # Invariants
//
// This is what the verification code is there to uphold under VerifyAuto
// (the default) and VerifyStrict with more than one endpoint configured. The
// code refers to them by number, and the randomised model test asserts them
// on every fact.
//
//  1. No fact reaches the sink on one endpoint's word, and none stays there
//     on the strength of having been confirmed once.
//     A transfer is reported only if one endpoint's logs show it and another
//     endpoint's receipt for that transaction contains the identical log in
//     the same block — also when it is reported again after a restart. It
//     is reported with the timestamp of its block (Transfer.BlockTime), and
//     that is the timestamp of a header with the transfer's block hash that
//     two endpoints report identically; until they do, the transfer is not
//     reported. The timestamp is recorded with the transfer and is the same
//     every time the transfer is reported — but like the transfer itself it
//     is not taken from the checkpoint on trust: in a new process the
//     endpoints confirm it before the transfer is reported with it again,
//     and if they report another time for that block, theirs is used. When
//     its block is settled, it is attested again: a second endpoint's logs
//     show it in that block, or (if they do not, or no second endpoint
//     serves logs) another endpoint's receipt still confirms it there. If
//     neither, the header at its height is fetched from the primary and the
//     verifier: if they agree on another block, the transfer is reported as
//     reverted; otherwise it holds the head. A revert is reported only if
//     the primary and the verifier both show another block at the
//     transfer's height. Whenever the block the endpoints agree on at a
//     settled height changes — a reorganisation below the cursor, which the
//     primary and the verifier are both asked about on every tick — the
//     cursor goes back and the blocks above are settled, and their transfers
//     attested, again. How far back is not the primary's to say either: the
//     block a reorganisation is said to begin above is one the verifier has
//     to confirm, or nothing is settled.
//  2. The processed head is a block whose number, hash, parent hash and
//     timestamp the primary and the verifier report identically, at most
//     SettleDepth below the lower of their two heads and not more than
//     Config.MaxFutureDrift ahead of the local clock. They report it so in
//     the tick in which it is handed to the sink: a head that could not be
//     delivered when its block was settled, or that is reported again
//     because finality moved on, is agreed on again first, and it has to be
//     the same block. The cursor never advances beyond SettleDepth below
//     the verifier's head, so no checkpoint is written above the chain.
//  3. No block is passed by the processed head unless its logs were scanned
//     by two endpoints — if two serve logs — and each of the two scans was
//     answered by a backend that had got at least SettleDepth blocks beyond
//     it: the margin that lets a log index trail its node is kept for the
//     second scan as for the first, so the head follows the slower of the
//     two log backends. "Serves logs" is every endpoint
//     that has ever answered a log query (the checkpoint remembers which
//     have), has not been tried, or has refused log queries without having
//     been found to have no log service: if such an endpoint is
//     unreachable, throttled, behind, or its logs lack a transfer that a
//     receipt confirms, and no other can take its place, the head waits —
//     for as long as it takes. Only if no other endpoint serves logs at all
//     is a range settled on one endpoint's logs. "No log service" is a
//     verdict that takes time: an endpoint that has never answered a log
//     query and has refused a minimal one three times in a row, a minute
//     and then two minutes apart (evm.LogsHealth.NoService; the checkpoint
//     remembers the verdict too, so that a new process does not wait for it
//     again). The endpoint is asked again whenever its pause ends — at most
//     half an hour — and its first answer makes it a log source for good.
//     What is settled on one endpoint's logs is recorded, first block to
//     last (Status.LogsUnchecked, Status.UncheckedFrom and UncheckedTo, in
//     the checkpoint), together with that endpoint's label, and when a
//     second endpoint that serves logs appears, the unchecked blocks in
//     which an address that is still watched can have been paid are
//     scanned by it; a transfer found then is reported late, below the
//     head (IncidentLateTransfer). "A second endpoint" is one other than
//     the endpoint on record for the blocks — whatever roles the two have
//     by then: a new endpoint put first in the list makes the old one the
//     second log source, and the old one's scan of its own blocks checks
//     nothing. Endpoints are named by label (scheme, host and port), and a
//     name on record counts only while an endpoint of that label is
//     configured: after the URL of the endpoint that settled the blocks has
//     changed, the provider may be configured still under the new one, and
//     the blocks take the scans of two endpoints configured now. (So do the
//     blocks of a checkpoint written before the endpoint was recorded.)
//     The record is closed when all of them have been, never in a tick
//     that settled blocks unchecked itself.
//  4. A head that is held is never held silently, and with three or more
//     endpoints one endpoint cannot hold it. Status.Hold names what the
//     head is waiting for; after three ticks IncidentHeadHeld is raised, and
//     again every minute. "Never silently" is not left to the places that
//     hold the head: whenever a tick succeeds without moving the head and
//     without a reason while any endpoint — one in use, or one the pool's
//     monitor has heard from in the last minute — shows a chain that
//     reaches more than two settle depths above the cursor, Tick names that
//     as the reason. Disagreements about a header — when a block is
//     settled, when a revert is to be proven, when a remembered block is
//     checked, when the block of a transfer is dated —, about the height of
//     the chain and about a transfer's receipt are put to a third endpoint
//     where there is one (a transfer's block that one of the two lacks, or
//     names another block at its height, is dated by the next endpoint
//     that has it), and the
//     endpoint that stands alone is set aside
//     (evm.Endpoint.ReportFault), as is one that does not know a block below
//     its own head, and one whose log index stays behind the chain. A
//     disagreement about a log scan needs a third endpoint
//     that serves logs to be resolved that way. With two endpoints the
//     watcher cannot tell which one lies, unless the lie is impossible on
//     its own (a timestamp in the future, or one that runs backwards against
//     remembered blocks), and waits.
//
// If no second endpoint answers, nothing is reported and nothing settles:
// the watcher fails closed.
//
// # Trust model
//
// Header hashes are not recomputed locally. The watcher compares what two
// endpoints say; it does not verify that a header hashes to its hash or was
// sealed by a validator. Recomputing the hash would not show that a block is
// canonical (anyone can produce a self-consistent header), it would only
// catch an endpoint forging fields of a real block — which the comparison
// with the second endpoint catches as well — and it would tie the watcher
// to the exact header encoding of every BSC hard fork. The consequence: two
// endpoints that return the same wrong answer are believed. Two URLs of one
// provider are one endpoint for this purpose.
//
// What one faulty or malicious endpoint can do, the other configured
// endpoints being honest and reachable (VerifyAuto or VerifyStrict):
//
//	                        | forge a   | revert a  | hide an      | expire an    | confirm     | stall the
//	                        | payment   | reported  | incoming     | open invoice | with too    | watcher
//	                        |           | payment   | payment      | early        | few blocks  |
//	------------------------+-----------+-----------+--------------+--------------+-------------+-------------
//	one endpoint            | possible  | possible  | possible     | possible (1) | possible    | possible
//	two or more, only one   | prevented | prevented | possible (2) | prevented    | prevented   | possible (4)
//	  of them serves logs   |           |           |              |   (3)        |             |
//	two or more, at least   | prevented | prevented | delayed only | prevented    | prevented   | possible (4)
//	  two serve logs        |           |           |   (5)        |              |             |
//
// The time a transfer is reported with — by which the consumer decides
// whether a payment was made before its deadline — goes with the first
// column: with two or more endpoints one of them cannot put a transfer on
// the other side of a deadline (while they disagree about the block's
// timestamp the transfer is not reported), with one endpoint it can.
//
// (1) A single endpoint is trusted with everything it says consistently.
// What is checked even then: its answers against each other (a transfer
// against its receipt, a missing log against the block's hash), a block
// timestamp against the local clock (not more than MaxFutureDrift ahead) and
// against the blocks around it (never decreasing), and a contradiction of
// finalized history has to persist for ConflictPatience ticks and is
// reported. Within those bounds it can stamp a block with any time between
// its parent's and now, which is enough to expire an invoice early when the
// watcher is behind — and to date a transfer on the wrong side of a
// payment's deadline.
//
// (2) If the lying endpoint is the one that serves logs, nobody can show
// what it left out: the payment is not seen, and the invoice later expires
// (which is why (3) holds only for the other ways of expiring an invoice: a
// forged timestamp or a head beyond the chain). The omission itself is not
// detected; the configuration is reported every time it begins and for as
// long as it lasts (Status.LogsUnchecked; the bridge repeats its alert every
// hour), and if a second endpoint that serves logs is added
// while the address is still watched, the payment is found and reported
// late. If the lying endpoint is one that does not serve logs, it has no say
// in what is found. What the log endpoint cannot do is keep a transfer on
// the books whose block is gone: a reported transfer needs a current receipt
// from the other endpoint when its block is settled.
//
// (4) With exactly two endpoints, the lying one can keep the processed head
// from moving for as long as it keeps contradicting the other — another
// header, another height (a chain that ends lower, a latest block or a log
// index that stands still), transfers nobody else knows, refusing receipts,
// logs that lack what a receipt shows — or simply does not answer: nothing
// is settled on the remaining endpoint's word. The same goes for a watcher
// that has no checkpoint yet: it does not start while the two are more than
// 64 blocks apart. That is never silent (invariant 4), nothing is lost, and
// everything proceeds when the endpoint stops or is removed. With three or
// more endpoints the lying one is outvoted and set aside, and costs a tick
// now and then — about headers, receipts and the height of the chain alike.
// For disputes about log scans this takes three endpoints that serve logs;
// with two log endpoints among three, one of them can hold the head by
// serving incomplete logs or none, or by a log index that stays behind. And
// in the second row the one endpoint that serves logs can always hold it, by
// not serving them.
//
// (5) The second log scan covers the blocks as they are settled, so a
// transfer the primary hides is found SettleDepth blocks later than usual,
// reported, and the primary is set aside (IncidentLogOmission). While the
// second log endpoint cannot be asked — throttled, unreachable, behind — the
// blocks are not settled at all: the payment is delayed further, it is not
// lost, and nothing expires in the meantime. The exception is an endpoint
// that has never answered a log query (since the checkpoint was first
// written) and refuses them: after three refusals in a row, over three
// minutes, it cannot be told from one without a log service any longer, and
// row two applies until it answers one — the head waits for it only during
// those three minutes. It is asked again at least every half hour; when it
// answers, the blocks settled meanwhile are scanned by it, and a payment the
// primary hid in them is reported then: late, which for a payment whose
// invoice has expired means late funds, and for one whose address is no
// longer watched (an unpaid invoice leaves the watch list an hour after it
// expired) means never. An endpoint that serves logs and is throttled, in
// those words, for the first three minutes of its life in this
// configuration — or after the checkpoint was lost — is the case this
// leaves open.
//
// VerifyBestEffort is the default mode with one exception: when every other
// endpoint has been unreachable for five consecutive ticks — it did not
// answer at all: no connection, a timeout, a gateway's 5xx —, the first row
// applies until one of them answers again (Status.Solo;
// IncidentSingleSource and IncidentSingleSourceEnded mark the beginning and
// the end). More available, and a malicious endpoint only has to wait for
// the others to be down; it cannot bring that about by anything it says,
// because an endpoint that answers — a rate limit, an error, a height or a
// block nobody shares — is never passed over, whatever the pool makes of it.
// (Blocks settled that way are recorded as unchecked and scanned by a second
// endpoint later, like those of row two; transfers, reverts and heads taken
// on one endpoint's word in the meantime are not checked again.)
//
// Further limits, in every configuration. An endpoint whose log index
// silently trails its head by more than SettleDepth blocks omits transfers
// without knowing it; where the provider answers batches the lag is measured
// (eth_blockNumber in the same batch), otherwise SettleDepth is the margin
// — for each of the two scans — and with two log-serving endpoints both
// would have to trail. Finality is
// taken from the endpoints' "finalized" tag, when two of them agree on it.
// The local clock is trusted: if it is behind the chain by more than
// MaxFutureDrift every block is refused (ErrFutureBlock) and nothing settles
// until it is corrected or the check is switched off; Status.ClockSkew shows
// the difference. And a checkpoint is believed about which blocks have been
// scanned: whoever can write the checkpoint store can make the watcher skip
// blocks.
package watcher

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

// Transfer is an ERC-20 transfer of the configured token to a watched
// address, as found in a specific block.
//
// (TxHash, LogIndex) identifies the transfer within its block and BlockHash
// identifies the block; the triple (BlockHash, TxHash, LogIndex) is the
// idempotency key of the fact. The same transaction mined again in another
// block after a reorg is a different fact: the old one is reported as
// reverted and the new one as observed (its LogIndex may differ).
type Transfer struct {
	// Token is the contract that emitted the event; always the configured
	// token.
	Token ethaddr.Address
	From  ethaddr.Address
	To    ethaddr.Address
	// Amount is in base units of the token. Never zero.
	Amount      *big.Int
	TxHash      evm.Hash
	LogIndex    uint64
	BlockNumber uint64
	BlockHash   evm.Hash
	// BlockTime is the timestamp of the block the transfer is in: the
	// timestamp of the header with that BlockHash, as reported identically
	// by two endpoints (with a single endpoint: by that endpoint). It is
	// set on every transfer passed to Sink.TransferObserved — a transfer
	// whose block cannot be dated that way yet is not reported yet — and
	// is the same every time the transfer is reported. Not part of the
	// transfer's identity; ignored in WatchSet snapshots unless set.
	BlockTime time.Time
}

// Head is a block of the canonical chain: in Sink.HeadAdvanced the processed
// head, in Status.Tip the newest block seen.
type Head struct {
	Number uint64
	Hash   evm.Hash
	// Time is the block timestamp.
	Time time.Time
	// Finalized is the number of the highest block the chain has reported
	// as finalized (BSC fast finality) — reported by the primary and
	// confirmed by the verifier — or 0 if the endpoints do not report
	// finality. A
	// transfer in a block at or below Finalized cannot be
	// reverted short of a consensus failure. It is usually above Number,
	// because the processed head trails the chain.
	//
	// The value only grows while the watcher runs. It can be lower than
	// one reported before after a restart from an older checkpoint, and
	// after an accepted finality conflict (Config.OnDeepReorg), which
	// resets it.
	Finalized uint64
	// Final is the highest block at or below Number that the watcher
	// treats as beyond reorganisation: Finalized — the finalized block two
	// endpoints attest (or two of three), or the consumer's floor
	// (Config.FinalizedFloor) — and, only on a network declared to have no
	// finality (Config.NoFinality), the block ReorgWindow below Number (the
	// watcher remembers that many blocks; a chain that replaces older ones
	// is accepted only as a deep reorganisation, OnDeepReorg). A consumer
	// that has an address scanned for from above Final misses nothing a
	// reorganisation the watcher accepts can move below it — which is not
	// so for Number: blocks above Finalized may be replaced, and a
	// transaction sent after the address was handed out mined again in
	// one of them, at or below the head it was handed out at.
	Final uint64
	// FinalTime is the timestamp of block Final (zero if not known).
	FinalTime time.Time
	// Checked is zero while every block up to Number has been scanned by
	// two log endpoints, or only one endpoint is configured — every block in
	// which a watched address can have been paid: not those below the
	// lowest WatchedAddress.Since, when every address has one. Otherwise
	// some blocks were settled on one endpoint's logs and are still to be
	// scanned by a second (invariant 3; Status.UncheckedFrom): Checked is
	// then a time no later than the timestamp of any of them (the Unix
	// epoch if that is not known). A consumer that stops watching an
	// address whose watch window ended after Checked gives up the payment
	// that second scan would have found. The head is reported again when
	// Checked changes.
	Checked time.Time
}

// Sink receives the facts. Implementations must be idempotent: every method
// may be called again with a fact it has already accepted (after an error,
// after a restart, after a re-scan), and must then succeed without side
// effects.
//
// Guarantees made by the watcher:
//
//   - At-least-once. A fact is considered delivered only when the method
//     returned nil. If it returns an error the watcher stops the tick, keeps
//     its cursor where it is and delivers the fact again later. An error
//     wrapping ErrHalt stops Run instead (see ErrHalt).
//   - No concurrent calls. All methods are called from the goroutine running
//     Tick/Run.
//   - The head rule. HeadAdvanced(h) is called only when, for every block up
//     to and including h.Number: the block has had its final scan, with a
//     watch-set snapshot taken after the block existed; every transfer found
//     in it has been verified and accepted by TransferObserved; and no
//     transfer reported in it is in doubt (seen missing but not yet proven
//     reverted). A transfer that is waiting for verification holds the head
//     below its block for as long as it waits. A consumer may therefore
//     treat "no transfer by head h" as final knowledge about blocks <= h —
//     for example to expire a payment against h.Time.
//   - Order within a tick: reverted transfers first, then observed transfers
//     in ascending (block number, log index), then at most one HeadAdvanced.
//   - Every transfer is reported with the timestamp of its block
//     (Transfer.BlockTime), agreed on by two endpoints like every other fact;
//     the same transfer is always reported with the same timestamp.
//   - Transfers run ahead of the head: a transfer is reported as soon as it
//     is seen and verified, usually SettleDepth blocks before the head
//     reaches its block. Such a report is tentative in the sense that a
//     later scan may revert it; it is never withheld to wait for the head.
//   - The head can move backwards: after a reorganisation deeper than
//     SettleDepth, after Rescan, or when an address or pending transfer
//     appears that requires blocks to be scanned again. Everything it
//     guaranteed before still holds.
//   - TransferReverted is called for a transfer the watcher reported with the
//     same BlockHash, TxHash and LogIndex, and only once the canonical block
//     at that height is known to be a different one. Because the fact carries
//     the BlockHash, a consumer can and should ignore a revert whose
//     BlockHash is not the one it has on record. In one situation the sink
//     can see a revert for a transfer it never accepted: the watcher records
//     its intent to report a transfer before calling TransferObserved, so if
//     the process stopped (or the sink kept refusing) in between and the
//     block was reorganised away meanwhile, the revert arrives alone. The
//     sink must treat that as a no-op.
type Sink interface {
	// TransferObserved reports a transfer found on the canonical chain.
	TransferObserved(ctx context.Context, t Transfer) error
	// TransferReverted reports that a transfer reported earlier is not on
	// the canonical chain any more.
	TransferReverted(ctx context.Context, t Transfer) error
	// HeadAdvanced reports the processed head: see "the head rule" above.
	// It is called at most once per tick and only when the processed head
	// changed. h.Time is the block's own timestamp.
	HeadAdvanced(ctx context.Context, h Head) error
}

// ErrHalt, wrapped in an error returned by a Sink or WatchSet method, makes
// Run return that error at once instead of retrying. It is how the consumer
// says "stop: what you are configured for is no longer what I run" — the
// caller then builds a new Watcher from fresh configuration. Tick returns
// the error like any other.
var ErrHalt = errors.New("watcher: halted by the consumer")

// WatchedAddress is one address of the watch set.
type WatchedAddress struct {
	Address ethaddr.Address
	// Since, if non-zero, is a block number below or at which no transfer
	// to the address needs to be looked for (typically the consumer's
	// processed head when the address was created). When an address shows
	// up that the watcher has not seen in an earlier snapshot of this
	// process and Since is below the cursor, the cursor is rewound to
	// Since, so that the blocks in between are scanned again with the
	// address. Zero means "no information": nothing is rewound.
	Since uint64
	// Closed says that the consumer is not waiting for anything on this
	// address: it is watched so that a transfer which arrives all the same
	// is noticed, and noticing it up to Config.IdleInterval later than at
	// once breaks no promise. A watch set that holds closed addresses only
	// lets the watcher go quiet (see "Quiet" in the package documentation);
	// one address that is not closed keeps it at its poll interval. The
	// zero value is the safe one: an address is open unless the consumer
	// says otherwise. Closed changes nothing about what is scanned or
	// reported, only how long the watcher may pause between two scans.
	Closed bool
}

// Snapshot is the consumer's side of the conversation, read once per tick.
type Snapshot struct {
	// Addresses is the watch set.
	Addresses []WatchedAddress
	// Pending are the transfers the consumer has accepted and does not yet
	// regard as final. The watcher keeps tracking every one of them until
	// the consumer stops listing it, however old it gets, and reports its
	// revert if its block leaves the chain. A listed transfer the watcher
	// has no record of (its checkpoint was lost or is older than the
	// consumer's data) is adopted: the cursor is rewound below its block so
	// that the next scan either finds it or reports it reverted. Token is
	// ignored (it is the configured token).
	Pending []Transfer
	// Processed, if non-zero, is the consumer's processed head: the number
	// of the last head it accepted from HeadAdvanced, in this process or an
	// earlier one — the block up to which it holds every transfer to the
	// watched addresses. A watcher whose cursor lies more than a reorg
	// window below it (its checkpoint is older than the consumer's data, or
	// was written from a position nobody needed) does not scan the blocks
	// in between again: see "A checkpoint below the consumer" in the
	// package documentation. Zero means "no information".
	Processed uint64
}

// WatchSet answers "what should I watch?".
//
// Contract required from the implementation: an address must be part of the
// returned set from the moment it is durably created and before it is
// disclosed to anyone who could pay to it, and the read must observe all
// such completed writes (no stale cache). The watcher takes its snapshot
// after it has fetched the chain head, so under this contract a block it
// scans can never contain a transfer to an address missing from the
// snapshot: the address would have had to be paid before it existed.
//
// That argument covers every address created while the watcher is running
// or stopped. It does not cover a watcher that starts from a position it did
// not scan its way to:
//
//   - No checkpoint. The start position (Config.StartBlock, or just below the
//     chain tip) is chosen in the first tick in which two endpoints agree on
//     where the chain ends (their heads within 64 blocks of each other, or a
//     third endpoint asked) — which may be long after New, if the endpoints
//     were down or apart. It is then lowered
//     to the smallest Since of the addresses in that tick's snapshot, so
//     that an address which already exists cannot have been paid below it.
//     Addresses without Since are not protected: a consumer that has
//     addresses must provide it.
//   - A checkpoint that is ahead of what the consumer has processed (the
//     consumer's data was restored from an older copy). The caller handles
//     that before Run, with Rescan.
//
// With a checkpoint, the addresses present in the first snapshot of a process
// are covered by it; Since is honoured for addresses that appear later.
//
// An address should stay in the set for as long as transfers to it matter.
// A consumer that watches an address for a span of time has to measure that
// span against the chain the watcher has processed (the time of the last
// head it was given), not only against its own clock: the watcher scans a
// block for the last time some time after it was mined — up to IdleInterval
// later while it rests, longer after an outage — and an address removed by
// the clock alone misses the blocks of its last moments. Removing an address
// stops new transfers to it from being reported;
// transfers already reported keep being tracked for reorgs until they leave
// the reorg window (or the consumer's Pending list, whichever is later).
//
// Hundreds to a few thousand addresses are expected; the call is made once
// per tick, and every 500 addresses cost one eth_getLogs call per tick on
// each of two endpoints (see "What the watch list costs"). An error
// wrapping ErrHalt stops Run.
type WatchSet interface {
	Watched(ctx context.Context) (Snapshot, error)
}

// Store persists the watcher's checkpoint: an opaque, versioned blob of a
// few kilobytes (cursor, recent block hashes, and the transfers reported
// within the reorg window).
//
// SaveCheckpoint must replace the previous value atomically and durably
// before returning nil. Ideally the store lives in the same database as the
// sink's data; it does not have to be transactional with it, because the
// watcher saves only after the sink accepted the corresponding facts, and
// re-delivers them if the save is lost.
//
// One Store belongs to one (chain, token) pair. The checkpoint records both
// and the watcher refuses to start from a checkpoint made for another pair.
//
// The encoding is canonical — the same state is always the same bytes, so a
// store may compare checkpoints — and it is read strictly: a checkpoint with
// a member this build does not know, a member twice or under another
// spelling, the same transfer twice, or two transfers at one log index of one
// block is refused as a whole (ErrCheckpoint), never read as something close
// to what it says.
type Store interface {
	// LoadCheckpoint returns the last saved checkpoint, or nil if there is
	// none yet.
	LoadCheckpoint(ctx context.Context) ([]byte, error)
	// SaveCheckpoint durably replaces the checkpoint.
	SaveCheckpoint(ctx context.Context, data []byte) error
}

// VerifyMode selects how an observation by the primary endpoint is
// cross-checked before it is reported.
type VerifyMode int

const (
	// VerifyAuto (the default): if more than one endpoint is configured,
	// every new transfer, every revert and every processed head must be
	// confirmed by an endpoint other than the one that produced it, and if
	// none can be reached nothing is reported; with a single configured
	// endpoint the same endpoint is asked (which catches inconsistent
	// answers, not lies).
	VerifyAuto VerifyMode = iota
	// VerifyBestEffort is VerifyAuto with one exception: when every other
	// endpoint has been unreachable — no answer at all: no connection, a
	// timeout, a gateway's 5xx — for five consecutive ticks of the same
	// remaining endpoint (another one left begins the count again), and has
	// answered nothing in between either (evm.Health.Answers: a health check
	// it answered between two ticks begins the count again), that endpoint
	// is believed on its own, for transfers, reverts, heads and
	// log scans alike, until another endpoint answers again (Status.Solo;
	// IncidentSingleSource when it begins, IncidentSingleSourceEnded when
	// it ends). An endpoint that answers is never passed over, whatever it
	// answers and whatever the pool thinks of it (lagging, rate-limited,
	// set aside): then this mode waits exactly like VerifyAuto. More
	// available than VerifyAuto, and a malicious endpoint only has to wait
	// for the others to be down — but it cannot bring that about by
	// anything it says.
	VerifyBestEffort
	// VerifyStrict is VerifyAuto, except that New refuses a client with a
	// single endpoint: nothing is ever reported on one endpoint's word.
	VerifyStrict
)

// Defaults. The numbers are for BNB Smart Chain as measured in November 2026
// (0.45 s blocks, fast finality 1–2 blocks behind the head, validator turns
// of 16 consecutive blocks, 21 active validators).
const (
	// DefaultPollInterval is the pause between ticks: about 7 blocks, so a
	// payment shows up within ~3 s and a 15-confirmation counter moves in
	// two or three steps, at under one request per second.
	DefaultPollInterval = 3 * time.Second
	// DefaultIdleInterval is the pause between ticks while there is nothing
	// to watch (see "Idle"): about 67 blocks. An idle instance then costs a
	// tenth of the requests, the processed head the consumer holds is at
	// most that old, and a failing endpoint is still noticed within half a
	// minute.
	DefaultIdleInterval = 30 * time.Second
	// DefaultSettleDepth is how far the processed head trails the chain:
	// 16 blocks, one validator turn, about 7 s. Blocks above it are scanned
	// again on every tick, which absorbs shallow reorgs without special
	// handling and gives an endpoint whose log index trails its head time
	// to catch up before a block is scanned for the last time.
	//
	// It is pure latency for the consumer (confirmations are counted from
	// the processed head), so it is no deeper than the evidence asks for:
	// on the public BSC endpoints that serve logs, the answer to a log
	// query always included the answering backend's own newest block
	// (TestLiveLogTipLag: lag 0 in every sample), and those endpoints
	// refuse a query that reaches above their head rather than truncating
	// it. The depth is the margin for providers that are not so careful.
	DefaultSettleDepth = 16
	// DefaultReorgWindow is how long block hashes and reported transfers
	// are remembered: 256 blocks (~2 min), just above the 15 x 16 = 240
	// blocks after which BSC blocks are final even without fast finality.
	DefaultReorgWindow = 256
	// DefaultCatchUpPause is the pause between scan steps while catching
	// up, to stay below the burst limits of free endpoints.
	DefaultCatchUpPause = 200 * time.Millisecond
	// DefaultMaxBackoff caps the retry delay after failed ticks.
	DefaultMaxBackoff = 30 * time.Second
	// DefaultConflictPatience is the number of consecutive ticks a finality
	// conflict must persist, with no second endpoint to consult, before the
	// watcher accepts the conflicting chain.
	DefaultConflictPatience = 20
	// holdAlertTicks is the number of consecutive ticks the processed head
	// may be held before IncidentHeadHeld is raised — and the number of
	// ticks a primary's log index may stay behind the chain before the
	// primary is set aside.
	holdAlertTicks = 3
	// DefaultMaxFutureDrift is how far ahead of the local clock a block
	// timestamp may be. BSC validators reject blocks from the future
	// outright, and endpoints serve the tip a second or two late, so an
	// honest block is never ahead at all; two minutes is room for a badly
	// kept server clock, and far too little to expire a payment early.
	DefaultMaxFutureDrift = 2 * time.Minute
)

// Config configures a Watcher. Client, Token, Sink, Watch and Store are
// required.
type Config struct {
	// Client is the RPC client. The caller should also run
	// Client.Monitor so that lagging endpoints are detected and failed
	// endpoints are brought back promptly.
	Client *evm.Client
	// Token is the ERC-20 contract whose transfers are reported. Transfer
	// events of any other contract are ignored, whatever they look like.
	Token ethaddr.Address
	Sink  Sink
	Watch WatchSet
	Store Store

	// Logger receives structured logs. Default: discard.
	Logger *slog.Logger

	// PollInterval is the pause between ticks in Run.
	PollInterval time.Duration
	// IdleInterval is the pause between ticks in Run while the watcher is
	// idle (see "Idle" in the package documentation). Zero means
	// DefaultIdleInterval; a negative value, or one that is not longer than
	// PollInterval, switches idle polling off: Run then pauses PollInterval
	// after every tick.
	IdleInterval time.Duration
	// SettleDepth is the number of blocks the cursor trails the head.
	SettleDepth uint64
	// ReorgWindow is the number of blocks below the head for which reorgs
	// are detected and reported. It must be at least SettleDepth.
	ReorgWindow uint64
	// StartBlock is the first block to scan when there is no checkpoint.
	// If nil, scanning starts SettleDepth blocks below the current head:
	// on a first start no deposit address has existed before, so there is
	// nothing older to find. Set it when restoring from a backup.
	StartBlock *uint64
	// MinAmount, if set, makes the watcher ignore transfers below this
	// amount (base units). Zero-value transfers are always ignored.
	MinAmount *big.Int
	// Verify selects the cross-check mode.
	Verify VerifyMode
	// NoFinality declares that the network has no finality: the
	// "finalized" block tag is not used, and a block is final
	// (Head.Final) once it is ReorgWindow deep. Whether a network has
	// finality is the network's to say, never the endpoints': without
	// this, no block is final for being deep — when the endpoints attest
	// no finalized block, nothing new is final, and the wait is named.
	NoFinality bool
	// FinalizedFloor is the consumer's final block (the Final of the last
	// head it accepted, in this process or an earlier one; 0 if none): a
	// block two endpoints attested as finalized. A watcher that knows no
	// finalized block as high — it lost its checkpoint, or it is older —
	// takes it as its finalized block once the primary and the verifier
	// show the very same block at that height, and reports no head until
	// they do. So what was final stays final, whatever the endpoints
	// answer now.
	FinalizedFloor uint64
	// CatchUpPause is the pause between scan steps of one tick.
	CatchUpPause time.Duration
	// MaxBackoff caps the retry delay after failed ticks in Run.
	MaxBackoff time.Duration
	// ConflictPatience: see DefaultConflictPatience. It is also the number
	// of ticks a single endpoint has to insist that the chain ends below
	// the checkpoint before the checkpoint is repaired.
	ConflictPatience int
	// OnDeepReorg, if set, is called when the watcher accepts a chain that
	// contradicts a finalized block or its whole reorg window. Transfers
	// older than the window cannot be re-examined by the watcher; the
	// operator should be alerted.
	OnDeepReorg func(DeepReorg)
	// OnIncident, if set, is called (from the goroutine running Tick) when
	// an endpoint is caught contradicting another, or the watcher repairs
	// its checkpoint: see Incident. The same incident is raised at most
	// once a minute.
	OnIncident func(Incident)
	// OnTick, if set, is called by Run after every tick, successful or
	// not, from Run's goroutine, once Status describes that tick. It is
	// how a consumer publishes the watcher's state as soon as it changes
	// rather than on a timer of its own. It must not call Tick.
	OnTick func()
	// MaxFutureDrift is how far ahead of Now a block timestamp may be
	// before the block is refused. Zero means DefaultMaxFutureDrift; a
	// negative value switches the check off (for a server whose clock
	// cannot be trusted — at the price of the protection).
	MaxFutureDrift time.Duration

	// Now and Sleep are the clock; defaults are time.Now and a
	// context-aware sleep.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// DeepReorg describes an accepted finality conflict.
type DeepReorg struct {
	// Endpoint is the redacted URL of the endpoint whose chain was
	// accepted.
	Endpoint string
	// RescanFrom is the first block that is scanned again.
	RescanFrom uint64
	// Reason says what was contradicted.
	Reason string
}

// uncheckedWaiting returns Status.UncheckedWaiting.
func (w *Watcher) uncheckedWaiting() string {
	if len(w.st.Unchecked) == 0 {
		return ""
	}
	r := w.st.Unchecked[0]
	var named []string
	for _, l := range r.By {
		for _, ep := range w.cfg.Client.Endpoints() {
			if ep.Label() == l {
				named = append(named, l)
			}
		}
	}
	could := 0
	for _, ep := range w.cfg.Client.Endpoints() {
		if !slices.Contains(named, ep.Label()) && !ep.Health().Logs.NoService {
			could++
		}
	}
	want := "scans by two configured log endpoints"
	if len(named) == 1 {
		want = "a scan by a configured log endpoint other than " + named[0]
	}
	switch {
	case could == 0:
		return fmt.Sprintf("blocks %d..%d wait for %s; no configured endpoint that could make it serves logs", r.From, r.To, want)
	case could == 1 && len(named) == 0:
		return fmt.Sprintf("blocks %d..%d wait for %s; only one configured endpoint that could make them serves logs", r.From, r.To, want)
	}
	return fmt.Sprintf("blocks %d..%d wait for %s", r.From, r.To, want)
}

// clockDisputeThreshold is how far one endpoint's newest block has to put
// the clock on the other side of the other's for Status.ClockDispute.
const clockDisputeThreshold = 30 * time.Second

// clockSkew returns Status.ClockSkew and Status.ClockDispute for a tick.
func (w *Watcher) clockSkew(t *tick, now time.Time) (time.Duration, string) {
	primary := now.Sub(time.Unix(int64(min(t.head.Time, 1<<62)), 0))
	if w.cfg.Client.Len() < 2 {
		return primary, ""
	}
	if !t.independent {
		return 0, ""
	}
	verifier := now.Sub(time.Unix(int64(min(t.verTip.Time, 1<<62)), 0))
	if (primary < 0) != (verifier < 0) {
		if max(primary.Abs(), verifier.Abs()) < clockDisputeThreshold {
			return 0, "" // seconds either way: no news
		}
		return 0, fmt.Sprintf("the newest block of %s puts this server's clock %s, that of %s %s",
			t.primary.Label(), skewWords(primary), t.ver.Label(), skewWords(verifier))
	}
	if verifier.Abs() < primary.Abs() {
		return verifier, ""
	}
	return primary, ""
}

func skewWords(d time.Duration) string {
	if d < 0 {
		return fmt.Sprintf("%s behind", (-d).Round(time.Second))
	}
	return fmt.Sprintf("%s ahead", d.Round(time.Second))
}

// Status is a snapshot of the watcher for the dashboard.
type Status struct {
	// Cursor is the processed head: everything at or below it has been
	// scanned for the last time and delivered.
	Cursor uint64
	// Processed is the processed head as last accepted by the sink in this
	// process (zero until the first HeadAdvanced succeeded).
	Processed Head
	// Tip is the newest block of the chain seen in the last tick that
	// reached an endpoint: with two endpoints, the newest block both have.
	Tip Head
	// Behind is Tip.Number - Cursor: SettleDepth when the watcher is in
	// sync, more while it is catching up or holding the head below an
	// unverified transfer.
	Behind uint64
	// Endpoint is the redacted URL of the endpoint used in the last tick.
	Endpoint string
	// WatchedAddresses is the size of the last watch-set snapshot.
	WatchedAddresses int
	// Tracked is the number of reported transfers the watcher is keeping
	// an eye on (inside the reorg window, or pending for the consumer).
	Tracked int
	// Unverified is the number of transfers one endpoint reported that are
	// waiting for confirmation by another. While it is non-zero the
	// processed head cannot pass their blocks.
	Unverified int
	// Verifier is the redacted URL of the endpoint that cross-checked the
	// primary in the last tick: "" if there is only one endpoint, or if
	// none was available (then nothing is settled; see Hold — unless Solo).
	Verifier string
	// Solo is true while the watcher, under VerifyBestEffort with several
	// endpoints configured, reports what one endpoint says without
	// confirmation: every other endpoint has been unreachable for several
	// ticks (see VerifyBestEffort). Never true in the other modes.
	Solo bool
	// LogVerifier is the redacted URL of the second endpoint whose log scan
	// confirmed the blocks settled most recently. LogsUnchecked is true if
	// the most recent blocks were settled on a single endpoint's logs
	// instead — because no other configured endpoint serves eth_getLogs
	// (or, under VerifyBestEffort, every other endpoint was unreachable:
	// Solo) — which leaves that endpoint able to hide a transfer.
	LogVerifier   string
	LogsUnchecked bool
	// UncheckedFrom and UncheckedTo are the lowest and the highest block
	// that were settled on a single endpoint's logs and have not been
	// scanned by a second one since (0: none). When a second endpoint that
	// serves logs appears, the blocks in that range — as far as addresses
	// that are still watched can have been paid in them — are scanned
	// again, by an endpoint other than the one whose logs settled them.
	UncheckedFrom uint64
	UncheckedTo   uint64
	// UncheckedWaiting says, while there are such blocks, what the first of
	// them wait for: the scan of a configured log endpoint other than the
	// one on record, or of two; and whether one is configured that could
	// make it.
	UncheckedWaiting string
	// Hold says why the processed head did not move in the last tick, if
	// there was something to settle and it was not settled: a transfer
	// waiting for confirmation, endpoints that disagree — about a block, or
	// about where the chain ends —, an endpoint whose log index is behind,
	// no second endpoint. "" otherwise: whenever a tick succeeds without
	// moving the head while an endpoint shows a chain with more to settle,
	// Hold says why (invariant 4).
	Hold string
	// ClockSkew is the local clock minus the timestamp of the chain's
	// newest block, as of the last tick that reached an endpoint: a few
	// seconds when all is well. A large positive value means the endpoints
	// serve an old head or the local clock runs ahead; a negative one means
	// the local clock is behind. It is what two endpoints agree on, so that
	// one endpoint alone cannot fake it: with several configured, the
	// smaller of the primary's and the verifier's differences when both are
	// on the same side of the clock, and zero when they are on opposite
	// sides or no independent verifier answered in the tick (ClockDispute
	// then says so). Only a single configured endpoint's word stands alone —
	// it is trusted with everything.
	ClockSkew time.Duration
	// ClockDispute is non-empty when the endpoints' newest blocks put the
	// local clock on opposite sides of their timestamps — one ahead of it,
	// one behind — so that no ClockSkew is reported; it says which said
	// what.
	ClockDispute string
	// FinalityDispute is non-empty when the endpoints do not agree on the
	// chain's finalized block in a way that holds finality back: one
	// reports none, or a block the other does not have, or a finalized
	// block more than SettleDepth behind the other's (the lower one is the
	// one that counts, Head.Final). It says which said what.
	FinalityDispute string
	// LastTickAt and LastSuccessAt are the times of the last tick and the
	// last successful one.
	LastTickAt    time.Time
	LastSuccessAt time.Time
	// LastError is the error of the last tick ("" if it succeeded) and
	// ConsecutiveErrors the number of failed ticks in a row.
	LastError         string
	ConsecutiveErrors int
	// Reorgs counts ticks that detected a reorganisation (a reported
	// transfer reverted, or settled blocks replaced) and DeepReorgs
	// accepted finality conflicts, since the process started.
	Reorgs     uint64
	DeepReorgs uint64
	// Idle is true if the last tick left the watcher with nothing the
	// consumer is waiting on and nothing to do, and idle polling is on: Run
	// pauses Config.IdleInterval before the next tick, unless Wake or Rescan
	// ends the pause. Work says what keeps the watcher at its poll interval
	// — the first of the conditions listed under "Idle" in the package
	// documentation that does not hold — and is "" when none does (also
	// with idle polling switched off).
	//
	// Quiet says which kind of idle it is: true if the watch set is not
	// empty — every address in it is closed (WatchedAddress.Closed), and the
	// ticks, IdleInterval apart, go on scanning for all of them — and false
	// if there is nothing to scan for at all. Quiet is never true without
	// Idle.
	Idle  bool
	Quiet bool
	Work  string
}

// Watcher scans the chain and feeds a Sink. Create one with New, then call
// Run (or Tick, for manual stepping). A Watcher is safe for concurrent use,
// though only one Tick runs at a time.
type Watcher struct {
	cfg Config
	log *slog.Logger

	tickMu sync.Mutex // serialises Tick; guards everything below it
	loaded bool
	st     state
	// headSent is the last processed head accepted by the sink in this
	// process (zero if none yet).
	headSent Head
	// conflictStreak counts consecutive ticks with an unconfirmed finality
	// conflict, raised by the endpoint conflictBy (a single source has to
	// insist on its own: another endpoint begins the count again).
	conflictStreak int
	conflictBy     string
	// seen is the set of addresses of the previous snapshot (nil before
	// the first one).
	seen map[ethaddr.Address]struct{}
	// verifiedHead is the header of the block the cursor rests on, as the
	// endpoints agreed on it in this process (zero if they have not).
	verifiedHead blockRef
	// aheadStreak counts consecutive ticks in which the endpoints claimed
	// the chain ends below the checkpoint; heldTicks those in which the
	// processed head did not move although something held it.
	aheadStreak int
	aheadBy     string
	heldTicks   int
	// unsaved is set while the last attempt to write the checkpoint
	// failed: the next tick writes it, changed or not (save).
	unsaved bool
	// lastShown is the highest chain height an endpoint had shown by the
	// end of the last tick, and lastShownBy who (tick.settle); shownBefore
	// is set once there was a last tick.
	lastShown   uint64
	lastShownBy string
	shownBefore bool
	// soloStreak counts consecutive ticks in which every endpoint other
	// than soloFor (a label) was unreachable (VerifyBestEffort); solo is
	// set while soloFor is believed on its own because of it. The count is
	// that endpoint's: when another endpoint is the one left, it starts
	// again — the endpoint left now was among those that did not answer.
	soloStreak int
	soloFor    string
	solo       bool
	// soloAnswers are the answer counts (evm.Health.Answers) of the other
	// endpoints, by label, when soloStreak began: an endpoint that has
	// answered anything since — between two ticks as well — ends the
	// count.
	soloAnswers map[string]uint64
	// stalledLog is the label of a primary whose log index kept the head
	// from moving, and stalledTicks for how many of its ticks in a row.
	stalledLog   string
	stalledTicks int
	// incidents remembers when each incident was last raised.
	incidents map[string]time.Time
	// rested is set when the last tick left the watcher with nothing the
	// consumer waits on and nothing to do (tick.work), idle or quiet. The
	// cursor is then a position the
	// watcher settled its way to, with both endpoints, however far the
	// chain has moved on by the next tick (skipUnneeded).
	rested bool

	mu         sync.Mutex // guards status, rescanFrom, soloNow and the wake-up state
	status     Status
	rescanFrom *uint64
	soloNow    string // the label of the endpoint believed alone (solo), readable while a tick runs
	// woken is set by Wake and Rescan and cleared by Run before each tick:
	// when it is set after a tick that ended idle, something may have been
	// added to the watch set after that tick read it. cancelIdle, while Run
	// is in an idle pause, ends that pause.
	woken      bool
	cancelIdle context.CancelFunc
}

// setSolo begins single-source operation on the endpoint labelled label, or
// ends it ("").
func (w *Watcher) setSolo(label string) {
	w.solo = label != ""
	w.mu.Lock()
	w.soloNow = label
	w.mu.Unlock()
}

// Solo reports whether the watcher is, at this moment, reporting what a
// single endpoint says without confirmation although several are configured
// (Status.Solo). Unlike Status it is up to date while a tick is running.
func (w *Watcher) Solo() bool {
	return w.SoloEndpoint() != ""
}

// SoloEndpoint returns the label (evm.Endpoint.Label) of the endpoint whose
// word the watcher takes alone at this moment, or "" if it takes nobody's
// (see Solo). A Sink or WatchSet may call it from inside a callback to decide
// what it accepts on one endpoint's word itself: the word of this endpoint,
// and of no other — it is the one every other endpoint has been unreachable
// beside for long enough.
func (w *Watcher) SoloEndpoint() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.soloNow
}

// New validates cfg and creates a Watcher. It performs no I/O.
func New(cfg Config) (*Watcher, error) {
	switch {
	case cfg.Client == nil:
		return nil, errors.New("watcher: Config.Client is required")
	case cfg.Token.IsZero():
		return nil, errors.New("watcher: Config.Token is required")
	case cfg.Sink == nil:
		return nil, errors.New("watcher: Config.Sink is required")
	case cfg.Watch == nil:
		return nil, errors.New("watcher: Config.Watch is required")
	case cfg.Store == nil:
		return nil, errors.New("watcher: Config.Store is required")
	}
	if cfg.Verify == VerifyStrict && cfg.Client.Len() < 2 {
		return nil, errors.New("watcher: VerifyStrict needs at least two RPC endpoints")
	}
	if cfg.Verify < VerifyAuto || cfg.Verify > VerifyStrict {
		return nil, fmt.Errorf("watcher: unknown VerifyMode %d", cfg.Verify)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.IdleInterval == 0 {
		cfg.IdleInterval = DefaultIdleInterval
	}
	if cfg.IdleInterval <= cfg.PollInterval {
		cfg.IdleInterval = 0 // off
	}
	if cfg.SettleDepth == 0 {
		cfg.SettleDepth = DefaultSettleDepth
	}
	if cfg.ReorgWindow == 0 {
		cfg.ReorgWindow = max(DefaultReorgWindow, cfg.SettleDepth)
	}
	if cfg.ReorgWindow < cfg.SettleDepth {
		return nil, fmt.Errorf("watcher: ReorgWindow (%d) must be at least SettleDepth (%d)", cfg.ReorgWindow, cfg.SettleDepth)
	}
	if cfg.MinAmount != nil && cfg.MinAmount.Sign() < 0 {
		return nil, errors.New("watcher: MinAmount must not be negative")
	}
	if cfg.CatchUpPause == 0 {
		cfg.CatchUpPause = DefaultCatchUpPause
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.ConflictPatience <= 0 {
		cfg.ConflictPatience = DefaultConflictPatience
	}
	if cfg.MaxFutureDrift == 0 {
		cfg.MaxFutureDrift = DefaultMaxFutureDrift
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	return &Watcher{cfg: cfg, log: cfg.Logger}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Status returns a snapshot of the watcher's progress.
func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

// Rescan asks the watcher to scan again from block `from` on its next tick.
// Transfers found are reported like any others (the sink is idempotent).
//
// It is the remedy for an address that was added to the watch set after
// transfers to it could already have been mined — an import, a restore from
// backup — which the normal flow never produces. Requests are merged: the
// lowest block wins.
//
// A watcher that is idling is woken: the re-scan starts at once.
func (w *Watcher) Rescan(from uint64) {
	w.mu.Lock()
	if w.rescanFrom == nil || from < *w.rescanFrom {
		w.rescanFrom = &from
	}
	w.mu.Unlock()
	w.Wake()
}

// Wake ends the pause of a watcher that is idling in Run (see "Idle" in the
// package documentation), quiet or not: the next tick runs at once. The
// consumer calls it after it has added to the watch set — an address that is
// not closed, a transfer it holds as pending — or has stopped calling an
// address closed, so that what was added is looked for now and not when the
// idle pause ends. It never blocks, may be called from any goroutine and at any
// time, and costs nothing when the watcher is not idle: a watcher that is
// ticking at its poll interval, or backing off after a failed tick, is not
// hurried by it.
func (w *Watcher) Wake() {
	w.mu.Lock()
	w.woken = true
	cancel := w.cancelIdle
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// pause is Run's wait after a successful tick: PollInterval, or — if the
// tick left the watcher idle — IdleInterval, cut short by Wake. It returns
// an error only if Run is to end.
func (w *Watcher) pause(ctx context.Context) error {
	w.mu.Lock()
	if !w.status.Idle {
		w.mu.Unlock()
		return w.cfg.Sleep(ctx, w.cfg.PollInterval)
	}
	if w.woken {
		// Woken while the tick was running: what was added may have missed
		// that tick's snapshot of the watch set. Look again now.
		w.woken = false
		w.mu.Unlock()
		return ctx.Err()
	}
	idleCtx, cancel := context.WithCancel(ctx)
	w.cancelIdle = cancel
	w.mu.Unlock()

	err := w.cfg.Sleep(idleCtx, w.cfg.IdleInterval)

	w.mu.Lock()
	w.cancelIdle = nil
	woken := w.woken
	w.mu.Unlock()
	cancel()
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if woken {
		return nil // the pause was ended, not broken
	}
	return err
}

// Run calls Tick every PollInterval until ctx is done — every IdleInterval
// while the watcher is idle (see "Idle" in the package documentation; Wake
// and Rescan end an idle pause at once) — backing off exponentially (1 s up
// to MaxBackoff) while ticks fail. It returns ctx.Err() when the context
// ends, or immediately with the error if the checkpoint cannot be used
// (ErrCheckpoint) or the consumer halted it (ErrHalt).
func (w *Watcher) Run(ctx context.Context) error {
	failures := 0
	for {
		// Whatever was added to the watch set before this moment is in the
		// snapshot the tick takes: only a wake-up from here on can concern
		// something the tick did not see.
		w.mu.Lock()
		w.woken = false
		w.mu.Unlock()
		err := w.Tick(ctx)
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if w.cfg.OnTick != nil {
			w.cfg.OnTick()
		}
		switch {
		case errors.Is(err, ErrCheckpoint), errors.Is(err, ErrHalt):
			return err
		case err != nil:
			failures++
			wait := min(time.Second<<min(failures-1, 10), w.cfg.MaxBackoff)
			w.log.Warn("chain watcher tick failed", "error", err, "consecutive_errors", failures, "retry_in", wait)
			if err := w.cfg.Sleep(ctx, wait); err != nil {
				return err
			}
		default:
			failures = 0
			if err := w.pause(ctx); err != nil {
				return err
			}
		}
	}
}

// Tick performs one scan iteration: see the package documentation. It
// returns nil if the chain was scanned up to the endpoint's head. Any error
// leaves the watcher in a state from which the next Tick retries; nothing is
// skipped.
func (w *Watcher) Tick(ctx context.Context) error {
	w.tickMu.Lock()
	defer w.tickMu.Unlock()

	now := w.cfg.Now()
	t := &tick{w: w, ctx: ctx, now: now}
	err := t.run()
	// Where the tick began (tick.began): in the first tick of a process the
	// cursor was not zero before it, it was where the checkpoint has it, or
	// the start position chosen without one. (Measured against zero, a head
	// that is held would count as having moved, and this one tick would
	// hold it without saying so.)
	before := t.began
	if err != nil && t.initialised && !t.started {
		// The start position was chosen in this tick, but the tick failed
		// before the watch set had its say about it (and before anything
		// was saved). Choose again next time: a start position that was
		// not lowered to the Since of the addresses that already exist
		// could lie above a payment.
		w.st.Initialized, w.st.Cursor = false, 0
		w.verifiedHead = blockRef{}
	}

	// Invariant 4: a head that is held is never held silently. Every place
	// that keeps the head where it is names its reason (holdf); what follows
	// is the net under all of them. A tick that succeeded without moving
	// the head, and without a reason, is acceptable only if there was
	// nothing to settle — and whether there was is not for the endpoints in
	// use to say alone: if any endpoint shows a chain that reaches more than
	// a settle depth beyond what the cursor accounts for, the head is being
	// held, and the status says by what. A tick that rewound the cursor (for
	// the watch set, or below the start it chose) and has not settled the
	// blocks again up to where it began is catching up: what it is busy
	// with is the reason (tick.work) — the head is below where it was, and
	// the status says why. One that has, and got no further, is held like
	// any other. No hold goes unnamed.
	catchingUp := (t.rewound || t.initialised) && w.st.Cursor < before
	if err == nil && t.started && t.holding == "" && w.st.Cursor <= before {
		top, by := t.highestShown()
		if t.shownTaken {
			// As the endpoints stood when the tick settled: a block mined
			// while it ran is the next tick's to settle.
			top, by = t.shownTop, t.shownBy
		}
		if top > w.st.Cursor+2*w.cfg.SettleDepth {
			if catchingUp {
				t.holding = fmt.Sprintf("settling again the blocks below where the head was (block %d; the cursor is at block %d): %s",
					before, w.st.Cursor, cmp.Or(t.work(nil), "the watch set asked for them"))
			} else {
				t.holding = fmt.Sprintf("%s reports the chain at block %d, but nothing above block %d could be settled: the endpoints in use do not show those blocks",
					by, top, w.st.Cursor)
			}
		}
	}
	w.lastShown, w.lastShownBy = t.highestShown()
	w.shownBefore = true
	switch {
	case err != nil:
		// A failing tick is reported as such (Status.LastError).
	case t.holding != "" && w.st.Cursor <= before:
		w.heldTicks++
		if w.heldTicks >= holdAlertTicks {
			t.incident(IncidentHeadHeld, nil, w.st.Cursor+1, t.holding)
		}
	default:
		w.heldTicks = 0
	}

	work := t.work(err)

	w.mu.Lock()
	defer w.mu.Unlock()
	if work == "" && w.rescanFrom != nil {
		work = fmt.Sprintf("a re-scan from block %d has been asked for", *w.rescanFrom)
	}
	w.rested = work == ""
	s := &w.status
	idle := work == "" && w.cfg.IdleInterval > 0
	// Quiet: at rest with closed addresses to scan for (tick.work lets no
	// tick rest that was given an address which is not closed).
	quiet := idle && t.watched > 0
	if idle != s.Idle || quiet != s.Quiet {
		switch {
		case quiet:
			w.log.Info("chain watcher quiet: every watched address is closed", "watched_addresses", t.watched,
				"next_tick_in", w.cfg.IdleInterval, "cursor", w.st.Cursor)
		case idle:
			w.log.Info("chain watcher idle: nothing to watch", "next_tick_in", w.cfg.IdleInterval, "cursor", w.st.Cursor)
		default:
			w.log.Info("chain watcher active again", "because", work, "poll_interval", w.cfg.PollInterval)
		}
		s.Idle, s.Quiet = idle, quiet
	}
	s.Work = work
	s.LastTickAt = now
	s.Cursor = w.st.Cursor
	s.Tracked = len(w.st.Tracked)
	s.Unverified = len(t.doubted)
	s.Hold = t.holding
	if t.primary != nil {
		s.Endpoint = t.primary.Label()
		s.Verifier = ""
		if t.independent {
			s.Verifier = t.ver.Label()
		}
	}
	s.Solo = w.solo
	if t.confirmedTo != 0 || t.logsUnchecked {
		s.LogVerifier, s.LogsUnchecked = t.logVerifier, t.logsUnchecked
	}
	s.UncheckedFrom, s.UncheckedTo = w.st.uncheckedBounds()
	s.UncheckedWaiting = w.uncheckedWaiting()
	s.Processed = w.headSent
	if t.haveHead {
		tip := t.head
		if t.independent && t.verTip.Number < tip.Number {
			tip = t.verTip // a height only one endpoint claims is not the tip
		}
		s.Tip = Head{
			Number:    tip.Number,
			Hash:      tip.Hash,
			Time:      time.Unix(int64(min(tip.Time, 1<<62)), 0).UTC(),
			Finalized: w.st.Finalized.Number,
		}
		s.WatchedAddresses = t.watched
		s.ClockSkew, s.ClockDispute = w.clockSkew(t, now)
		s.FinalityDispute = t.finalityDispute
	}
	s.Behind = 0
	if s.Tip.Number >= s.Cursor {
		s.Behind = s.Tip.Number - s.Cursor
	}
	if t.reverted {
		s.Reorgs++
	}
	if err != nil {
		if ctx.Err() == nil {
			s.LastError = err.Error()
			s.ConsecutiveErrors++
		}
		return err
	}
	s.LastError = ""
	s.ConsecutiveErrors = 0
	s.LastSuccessAt = now
	w.log.Debug("chain watcher tick", "endpoint", s.Endpoint, "tip", s.Tip.Number, "scanned_to", t.scannedTo,
		"cursor", s.Cursor, "watched", s.WatchedAddresses, "tracked", s.Tracked, "unverified", s.Unverified)
	return nil
}
