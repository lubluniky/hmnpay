// Package corebridge connects the chain side of hmnpay (internal/chain/evm
// and internal/chain/watcher) to the core: it runs the payment watcher for
// whatever network the core is configured for, feeds the core the facts the
// watcher produces, and implements the core's ChainProber port.
//
// # What the integrator starts
//
//	bridge, err := corebridge.New(service, checkpointStore, corebridge.Options{Logger: logger})
//	go bridge.Run(ctx) // until ctx is cancelled
//
// That is all. The bridge reads core.Service.ChainState, and
//
//   - idles while first-run setup is not completed or no usable RPC endpoint
//     is configured, and starts by itself when that changes;
//   - builds an evm.Client and a watcher.Watcher for the configured chain,
//     token and endpoints, runs them, and reports endpoint health to the core
//     after every tick of the watcher and every few seconds besides
//     (core.Service.ReportChainStatus) — "starting" until the first tick
//     of a watcher has completed;
//   - lets the watcher idle while the core is waiting for nothing — no
//     payment is open, no transfer is not final; payments that are closed
//     and still in their watch window are scanned for at the idle pace —
//     and wakes it the moment that changes (see "Idle and quiet");
//   - tears all of it down and builds it anew when the network settings
//     change (RPC endpoints, chain, asset, required confirmations) — it
//     learns of changes through the core's event subscription and by
//     re-reading the state periodically;
//   - never gives up: any failure of a generation is logged and retried with
//     backoff.
//
// # The seam with the core
//
// The core makes one demand of its chain adapter that everything else
// follows from: AdvanceHead(N) may be called only after every transfer to a
// watched address in blocks up to N has been reported, because the core
// expires payments — terminally — against the timestamp of that head. The
// watcher's processed head has exactly this meaning (see watcher.Sink, "the
// head rule"), and it is the only head the bridge passes on.
//
// Every transfer is passed on with the timestamp of its block
// (core.TransferObservation.BlockTime), by which the core decides whether it
// arrived before the payment's deadline. The core records that time on the
// first report of a transfer only, so the watcher does not report a transfer
// before two endpoints agree on the timestamp of its block.
//
// The core and the watcher each keep a record of transfers that are not final
// yet. The core's is authoritative: on every tick the bridge hands the
// watcher the core's pending transfers, and the watcher adopts any it does
// not know. At start the bridge reconciles positions the same way: the
// watcher resumes from the lower of its own checkpoint and the core's
// processed head, and a checkpoint that is unreadable or belongs to another
// chain or token is discarded and rebuilt from the core's state.
//
// The core's head is the authority in the other direction as well. On every
// tick the bridge tells the watcher how far the core has got
// (watcher.Snapshot.Processed), and a checkpoint that lies more than a reorg
// window below that — and below every block in which a watched address can
// have been paid — is not scanned from: the watcher continues from where
// the core and the addresses need it to (AlertCheckpointRepaired). So the
// processed head the core is given is never far below the one it has,
// unless two endpoints agree that the chain is shorter, a deep
// reorganisation was accepted, or a re-scan was asked for (Bridge.Rescan) —
// and a watcher without a checkpoint does not start at all on two endpoints
// that are far apart about where the chain ends.
//
// Losing the checkpoint therefore costs a re-scan of the blocks the core has
// not made final, never a payment that the core knows of. It does lose what
// only the checkpoint knew: which endpoints have served logs and which have
// none (learnt again: an endpoint that refuses its first log queries is
// waited for, three refusals over three minutes, before it is held to have
// no log service), and which blocks were settled on one endpoint's logs and
// not scanned by a second one yet — those are not scanned again.
//
// # Idle and quiet
//
// Every tick of the watcher costs requests, and RPC providers bill by the
// request. While the core is waiting for nothing from the chain, the watcher
// pauses Options.IdlePollInterval (30 s) between ticks instead of
// Options.PollInterval (3 s), and the endpoint health check runs half as
// often (every 30 s instead of every 15). "Waiting for nothing" is
// core.ChainState.Active being false, and it comes in two kinds:
//
//   - idle: the watch list is empty. A tick has nothing to scan for; it
//     follows the chain and reports the processed head.
//   - quiet: the watch list holds addresses, every one of them closed
//     (core.WatchedAddress.Closed) — its payment is settled and none of its
//     transfers is pending, so the only thing the chain can still do to it
//     is late funds. A paid payment stays on the list like that for seven
//     days. A quiet tick is a full tick: every block since the last one is
//     scanned for every address, on two endpoints, as always; there are
//     just ten times fewer of them.
//
// Either way it is the watcher's verdict, not the bridge's (watcher: "Idle",
// "Quiet"): besides what the core lists, the watcher must have no business
// of its own left. Deadlines are settled against the processed head, and a
// payment with a deadline is an open payment, so nothing the core does by
// the head's block time ever waits for an idle pause.
//
// What the pause can delay, and it is the one thing: money that arrives at
// the address of a payment that is already closed is noticed by the tick
// that ends the pause — up to Options.IdlePollInterval after it was mined
// instead of up to Options.PollInterval. A tick that reports a transfer
// does not leave the watcher at rest: from there on it ticks at its poll
// interval until the transfer is final for the core and out of the reorg
// window, so the confirmations of late funds are followed like any others.
//
// The watcher is woken — its pause ends and it ticks at once — whenever the
// core appends an event while its chain state is active: the bridge already
// reads that state after every event to notice a change of configuration.
// What makes it active, and where each comes from:
//
//   - a payment is created: its address is put on the watch list, open, in
//     the transaction that emits payment.created. The API's CreatePayment
//     and the payment-link checkout share that code, and nothing else adds
//     an address;
//   - a transfer is pending: only the watcher reports transfers, and the
//     tick that reported one is followed by the next a poll interval later
//     in any case;
//   - nothing else. No operation turns a settled payment back into an open
//     one: cancelling settles, resolving an issue and reopening a
//     resolution leave status and transfers alone. Those events find the
//     chain state as inactive as it was, and do not wake the watcher.
//
// Bridge.Rescan wakes the watcher itself, and so does a chain state that
// cannot be read (unknown is not "nothing"). A change of the network
// settings ends the generation and starts a watcher that ticks at once.
// And the wake-up is an optimisation, not what finding a payment rests on:
// the watch list is read by every tick, and the bridge re-reads the chain
// state every Options.ReloadInterval as well.
//
// So the first payment after an idle stretch is looked for as fast as any
// other; what the pause costs it is the scan of the blocks the watcher has
// not settled yet, one eth_getLogs call for some 80 blocks.
//
// Its start block: a payment records the core's processed head at its
// creation (WatchedAddress.SinceBlock), and while idle that head is up to an
// idle interval old. It is therefore lower than it would otherwise be —
// never higher: the head is where the watcher's cursor is, and the tick
// after the wake-up scans every block above the cursor with the new address.
//
// The node status says Idle while this lasts (core.ChainStatus.Idle), with
// Quiet if the watch list is not empty, and the core then does not count the
// distance between its head and the newest block as lag: that distance is
// the idle pause. The bridge says Idle only while the watcher's idle ticks
// keep succeeding — a tick overdue by more than half an idle interval ends
// it, and Quiet with it — so a watcher that stopped is reported as lagging
// like before, and a failing endpoint as failing.
//
// # What an RPC endpoint can do
//
// The endpoints are not trusted. With two or more configured and the default
// verification mode, every fact passed to the core — a transfer, a revert,
// and the processed head with its number, hash and timestamp — has been
// reported identically by two endpoints, and the blocks below the head have
// been scanned for logs on two endpoints if two of them serve eth_getLogs.
// One lying endpoint can then delay payments; it cannot create one, take one
// back, expire an invoice early or have one confirmed with fewer
// confirmations than required. The full account, including what a single
// endpoint and an endpoint that alone serves logs can still do, is the
// "Invariants" and "Trust model" sections of package watcher. The price of
// the rule: when the only other endpoint is unreachable or throttled, or the
// two disagree, nothing is settled until that is resolved — and the operator
// is told what the head is waiting for (AlertHeadHeld, once a minute). With
// a third endpoint, disagreements about a block, a receipt or the height of
// the chain are settled by it and the endpoint that stands alone is set
// aside.
//
// Options.Verify = VerifyBestEffort trades the guarantee for availability in
// exactly one situation: when every other endpoint has been unreachable — no
// answer at all — for five ticks in a row, what the remaining endpoint says
// is passed on unconfirmed until another answers again (AlertSingleSource
// when that begins, AlertSingleSourceEnded when it ends, a standing warning
// in between). An endpoint that answers is never passed over, whatever it
// answers; then this mode waits like the default.
//
// The same holds for the one thing the bridge itself asks the endpoints:
// which block was the newest when the oldest payment without a start block
// was created (generation.since: payments earlier versions created before
// the first head; the core now refuses to create a payment before it has
// one). Two endpoints have to give the same answer.
//
// # Clocks
//
// The core settles a deadline when the processed head's block time and its
// own clock are both past it, and refuses a head whose block time is ahead
// of its clock (core.ErrHeadTimeAhead). The watcher applies the same test
// one step earlier, to every block it would settle on, with the same clock
// (Options.Now, or the core's clock read back from its status reports), so a
// block from the future never becomes a head. Should the core refuse a head
// all the same, the head is let go (it is not offered again, and the core's
// head does not move), AlertHeadRefused is raised, and a single endpoint
// that supplied it is set aside.
//
// A server clock that is wrong shows up as the difference between the clock
// and the timestamp of the chain's newest block: watcher.Status.ClockSkew.
// Beyond Options.ClockSkewThreshold (30 s) for three ticks the bridge raises
// AlertClockSkew. A clock that runs ahead does not stop payments; a clock
// more than two minutes behind stops everything — every block looks as if
// it came from the future — and the alert says that it is the clock. An
// operator who cannot keep the clock right can switch the test off, here
// (Options.MaxHeadTimeAhead) and in the core (core.Config.MaxHeadTimeAhead)
// alike; both take the same value.
//
// # What the operator sees
//
// Alerts (Options.OnAlert, Status.Alerts, the log at error or warning level)
// for what needs a human: see the AlertKind constants. Status.Warnings lists
// what is wrong right now — clock skew, a processed head that is waiting and
// why, log scans that nobody cross-checks — and the same text is put into
// the Error column of the endpoint in use in the report the core serves as
// the dashboard's node status (and in core.ChainStatus.Warnings).
package corebridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// ActorName is the name of the system actor the bridge acts as.
const ActorName = "chain-watcher"

// Defaults.
const (
	// DefaultStatusInterval is how often endpoint health is reported to
	// the core (which considers a report stale after a minute).
	DefaultStatusInterval = 5 * time.Second
	// DefaultReloadInterval is how often the configuration is re-read even
	// if no event announced a change.
	DefaultReloadInterval = 30 * time.Second
	// DefaultMaxBackoff caps the delay between retries.
	DefaultMaxBackoff = 30 * time.Second
	// DefaultLogsUncheckedRepeat is how often the operator is reminded
	// that log scans are not cross-checked, for as long as that lasts.
	DefaultLogsUncheckedRepeat = time.Hour
	// DefaultClockSkewThreshold is the difference between the server's
	// clock and the timestamp of the chain's newest block above which the
	// operator is warned. An honest tip is a few seconds old.
	DefaultClockSkewThreshold = 30 * time.Second
	// DefaultFinalityStallAfter is how long the final block may stay where
	// it is, while the processed head moves on above it, before
	// AlertFinalityStalled is raised. BSC finalises a block or two behind
	// its tip, within seconds; two minutes is a hundred blocks and more.
	DefaultFinalityStallAfter = 2 * time.Minute
)

// Options configures a Bridge. The zero value is ready for production.
type Options struct {
	// Logger receives the logs of the bridge, the watcher and the RPC
	// client. Default: discard.
	Logger *slog.Logger
	// PollInterval is the watcher's pause between ticks. Default:
	// watcher.DefaultPollInterval.
	PollInterval time.Duration
	// IdlePollInterval is the watcher's pause between ticks while the core
	// is waiting for nothing from the chain: the watch list is empty, or
	// holds closed payments only (see "Idle and quiet"). Default:
	// watcher.DefaultIdleInterval (30 s). A negative value, or one that is
	// not longer than PollInterval, switches idle polling off, and the
	// quiet state with it.
	IdlePollInterval time.Duration
	// SettleDepth is how many blocks the processed head trails the chain
	// (see watcher.DefaultSettleDepth). It is the latency added to every
	// confirmation; lower it only for endpoints you run yourself.
	SettleDepth uint64
	// Verify selects how what one endpoint says is cross-checked with
	// another: transfers, reverts, the processed head and the log scans.
	// Default: watcher.VerifyAuto — with two or more endpoints nothing is
	// reported on one endpoint's word, and nothing at all while no second
	// endpoint can be reached.
	Verify watcher.VerifyMode
	// StatusInterval and ReloadInterval: see the defaults.
	StatusInterval time.Duration
	ReloadInterval time.Duration
	// HealthInterval is how often every endpoint is asked for its chain id
	// and block number, whether the watcher uses it or not (the pool's
	// health check, evm.Client.Refresh). Default:
	// evm.DefaultMonitorInterval (15 s). While the watcher idles the check
	// runs half as often, but not less often than the watcher's idle ticks.
	HealthInterval time.Duration
	// MaxBackoff caps the delay before a failed tick or a failed start is
	// retried. Default: 30 s.
	MaxBackoff time.Duration
	// Endpoint tunes the RPC transport (timeouts, HTTP client).
	Endpoint evm.EndpointOptions
	// OnAlert, if set, is called (from the bridge's goroutines) for
	// conditions that need the operator: see Alert.
	OnAlert func(Alert)
	// Now is the clock block timestamps are judged against: a block stamped
	// more than two minutes ahead of it is refused, and a chain tip that
	// differs from it by more than ClockSkewThreshold raises
	// AlertClockSkew. It must be the clock the core uses (core.Config.Clock),
	// because the core judges payment deadlines by its own clock and block
	// time together. If nil, the bridge follows the core's clock by reading
	// back the time stamp the core puts on each status report — the system
	// clock in production.
	Now func() time.Time
	// ClockSkewThreshold: see DefaultClockSkewThreshold.
	ClockSkewThreshold time.Duration
	// LogsUncheckedRepeat is how often AlertLogsUnchecked is raised again
	// while blocks keep being settled on one endpoint's logs. Default: one
	// hour. AlertFinalityStalled is repeated as often.
	LogsUncheckedRepeat time.Duration
	// FinalityStallAfter: see DefaultFinalityStallAfter.
	FinalityStallAfter time.Duration
	// MaxHeadTimeAhead is how far ahead of the clock a block timestamp may
	// be before the watcher refuses the block. It has the meaning and the
	// values of core.Config.MaxHeadTimeAhead and should be given the same
	// value: zero is the default (two minutes), a negative value switches
	// the check off — for a server whose clock cannot be kept right, at
	// the price of the protection against blocks from the future.
	MaxHeadTimeAhead time.Duration

	// sleep, if set, is the watcher's pause (watcher.Config.Sleep): for
	// tests that tick a generation by hand.
	sleep func(ctx context.Context, d time.Duration) error
	// inFlight, if set, is called as a generation is about to hand a fact
	// to the core ("head", "transfer", "revert", "status"): for tests that
	// change the settings while a fact is in flight.
	inFlight func(kind string)
	// headAccepted, if set, is called with every head the core accepted:
	// for tests that check what the core went by.
	headAccepted func(core.ChainHead)
}

// AlertKind classifies an Alert.
type AlertKind string

// Alert kinds.
const (
	// AlertFinalityConflict: the chain contradicted a transfer the core
	// regards as final (core.ErrTransferFinal). A transfer is final only in
	// a block the chain has finalised, as two endpoints attest it: so this
	// is a reorganisation that replaced a finalised block — a consensus
	// failure, or endpoints that both lied about finality. The core keeps
	// its settlement; a human has to look.
	AlertFinalityConflict AlertKind = "finality_conflict"
	// AlertFinalityStalled: the final block (core.ChainHead.Final) has not
	// moved for Options.FinalityStallAfter while the chain moved on — the
	// chain does not finalise blocks, or the endpoints do not agree on what
	// it has finalised (the message says which), also before the first
	// processed head (no payment can be created then) and whatever the scan
	// does meanwhile. A
	// transfer counts towards paid only at or below the final block, so
	// payments in later blocks stay confirming, without a deadline
	// passing, until it moves. Raised when that begins and again every
	// Options.LogsUncheckedRepeat for as long as it lasts.
	AlertFinalityStalled AlertKind = "finality_stalled"
	// AlertFactRejected: the core rejected a fact as malformed. It is
	// dropped (it could never be accepted); this is a bug to report.
	AlertFactRejected AlertKind = "fact_rejected"
	// AlertDeepReorg: the watcher accepted a chain that contradicts
	// finalized history (watcher.Config.OnDeepReorg).
	AlertDeepReorg AlertKind = "deep_reorg"
	// AlertCheckpointDiscarded: the stored checkpoint was unreadable or
	// belonged to another chain or token, and was rebuilt from the core's
	// state; or there was none, while the core's head had blocks on record
	// as settled on one endpoint's logs (core.ChainHead.Checked) — that
	// record was the checkpoint's.
	AlertCheckpointDiscarded AlertKind = "checkpoint_discarded"
	// AlertEndpointFault: an RPC endpoint was caught contradicting another
	// one — a different block header, a block from the future, a transfer
	// missing from its logs, a transfer nobody else knows. Nothing was
	// taken on its word; if it keeps happening, remove the endpoint. With
	// only two endpoints the watcher cannot always tell which of them is
	// wrong and the processed head waits until they agree.
	AlertEndpointFault AlertKind = "endpoint_fault"
	// AlertCheckpointRepaired: the watcher's checkpoint was not where the
	// chain and the core are. Either it claimed blocks as settled that no
	// endpoint has (an endpoint lied about the height of the chain before
	// this was checked, or the chain was rolled back): the watcher went
	// back and scans again. Or it lay far below everything that still has
	// to be scanned (it was written from a start position that followed
	// one endpoint down): the watcher continues from where the core and
	// the watched addresses need it to, instead of scanning its way up.
	AlertCheckpointRepaired AlertKind = "checkpoint_repaired"
	// AlertClockSkew: the server's clock and the timestamp of the chain's
	// newest block differ by more than Options.ClockSkewThreshold. Deadlines
	// are judged by this clock: fix the clock (NTP). While it is behind the
	// chain by more than the core's tolerance, the core creates no payment
	// (core.ErrClockBehind). The same alert is raised when every endpoint
	// serves a stale chain head.
	AlertClockSkew AlertKind = "clock_skew"
	// AlertHeadRefused: the core refused a processed head because its
	// block time is ahead of the core's clock (core.ErrHeadTimeAhead). The
	// head is not recorded; confirmations and expiries wait.
	AlertHeadRefused AlertKind = "head_refused"
	// AlertLogsUnchecked: several endpoints are configured but blocks are
	// being settled on one endpoint's logs — no other endpoint serves
	// eth_getLogs (or, in best-effort mode, every other endpoint is
	// unreachable) — so nobody can confirm that it shows every transfer: a
	// payment it leaves out is not seen. Raised every time that begins,
	// and again every Options.LogsUncheckedRepeat (an hour) for as long as
	// it lasts. Add a second endpoint that serves logs.
	AlertLogsUnchecked AlertKind = "logs_unchecked"
	// AlertSingleSource: best-effort verification only. Every other
	// endpoint has been unreachable for several ticks, and from now on
	// what the remaining endpoint says is passed to the core without
	// confirmation — transfers, reverts, the processed head. Raised every
	// time that begins; AlertSingleSourceEnded when another endpoint
	// answers again.
	AlertSingleSource      AlertKind = "single_source"
	AlertSingleSourceEnded AlertKind = "single_source_ended"
	// AlertHeadHeld: the processed head has not moved for several ticks
	// although there are blocks to settle; the message says what it is
	// waiting for (two endpoints that disagree, a second endpoint that is
	// unreachable, log verification that is unavailable, a transfer nobody
	// else confirms). Payments are not confirming and nothing expires
	// while it lasts. Raised again every minute.
	AlertHeadHeld AlertKind = "head_held"
	// AlertLateTransfer: a transfer was found by a second endpoint in
	// blocks that had been settled on one endpoint's logs (see
	// AlertLogsUnchecked). It has been reported to the core, below its
	// head; if the payment has expired meanwhile it shows up there as late
	// funds. Look at the payment.
	AlertLateTransfer AlertKind = "late_transfer"
	// AlertUncheckedReleased: addresses left the watch list although
	// blocks of their watch window had been settled on one endpoint's logs
	// and not scanned by a second yet — more addresses were waiting for
	// that scan than the core keeps for it (core.Config.MaxRetainedUnchecked;
	// node status released_unchecked). A payment that endpoint left out of
	// its logs for one of them will not be found. Raised whenever the
	// number grows. Add a second endpoint that serves logs, or bring back
	// the one that does not answer.
	AlertUncheckedReleased AlertKind = "unchecked_released"
)

// Alert is a condition the operator should know about. Every alert is also
// logged at error or warning level and counted in Status.
type Alert struct {
	Kind    AlertKind
	Message string
	// TxHash and BlockNumber identify the transfer concerned, if any.
	TxHash      string
	BlockNumber uint64
	At          time.Time
}

// State is what the bridge is doing.
type State string

// States.
const (
	// StateIdle: nothing to watch — setup is not completed, or no usable
	// endpoint is configured. Status.Reason says which.
	StateIdle State = "idle"
	// StateRunning: a watcher is running.
	StateRunning State = "running"
	// StateRetrying: the last attempt to run failed; the bridge is waiting
	// to try again. Status.Reason holds the error.
	StateRetrying State = "retrying"
)

// Status is a snapshot of the bridge.
type Status struct {
	State  State
	Reason string
	// Generation counts how many times a watcher was built.
	Generation int
	// ChainID, Token and Endpoints (redacted URLs) describe the running
	// configuration.
	ChainID   uint64
	Token     string
	Endpoints []string
	// Watcher is the running watcher's status.
	Watcher watcher.Status
	// Warnings are the conditions the operator should look at right now
	// (clock skew, a processed head that is waiting, log scans that nobody
	// cross-checks), as shown on the dashboard's node status. Empty when
	// all is well.
	Warnings []string
	// Alerts counts alerts by kind since the process started; LastAlert is
	// the most recent one.
	Alerts    map[AlertKind]uint64
	LastAlert Alert
}

// Bridge runs the chain watcher for a core.Service. Create it with New and
// call Run.
type Bridge struct {
	svc   *core.Service
	store watcher.Store
	opts  Options
	log   *slog.Logger

	mu     sync.Mutex
	status Status
	w      *watcher.Watcher // the running watcher, if any
	// released is core.ChainState.ReleasedUnchecked as last read under the
	// configuration releasedRev, once releasedSeen (see noteReleased).
	released     int
	releasedRev  uint64
	releasedSeen bool
}

// noteReleased raises AlertUncheckedReleased when the number of addresses
// let go unchecked has grown since it was last read under the same chain
// configuration (revision). The first reading of a process, and the first
// after a change of configuration — another network counts other payments —
// is the baseline: what was released before it is in the node status
// (released_unchecked), which is the durable record; the alert is the
// running process's, and one released in the moment the process ends is
// counted there but not alerted.
func (b *Bridge) noteReleased(n int, revision uint64) {
	b.mu.Lock()
	grown := b.releasedSeen && b.releasedRev == revision && n > b.released
	before := b.released
	b.released, b.releasedRev, b.releasedSeen = n, revision, true
	b.mu.Unlock()
	if grown {
		b.log.Error("addresses left the watch list before blocks of their window were scanned by a second log endpoint",
			"released", n-before, "released_in_all", n)
		b.alert(Alert{Kind: AlertUncheckedReleased, Message: fmt.Sprintf(
			"%d address(es) left the watch list although blocks of their window were settled on one endpoint's logs and not scanned by a second yet (%d in all): a payment hidden in those blocks for them will not be found; add a second endpoint that serves eth_getLogs",
			n-before, n)})
	}
}

// New creates a Bridge. checkpoints is the durable store of the watcher's
// checkpoint (see watcher.Store): one opaque value, replaced atomically.
// Saving an empty value must make LoadCheckpoint return an empty value. It
// performs no I/O.
func New(svc *core.Service, checkpoints watcher.Store, opts Options) (*Bridge, error) {
	if svc == nil {
		return nil, errors.New("corebridge: core service is required")
	}
	if checkpoints == nil {
		return nil, errors.New("corebridge: checkpoint store is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.StatusInterval <= 0 {
		opts.StatusInterval = DefaultStatusInterval
	}
	if opts.ReloadInterval <= 0 {
		opts.ReloadInterval = DefaultReloadInterval
	}
	if opts.HealthInterval <= 0 {
		opts.HealthInterval = evm.DefaultMonitorInterval
	}
	if opts.SettleDepth == 0 {
		opts.SettleDepth = watcher.DefaultSettleDepth
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultMaxBackoff
	}
	if opts.ClockSkewThreshold <= 0 {
		opts.ClockSkewThreshold = DefaultClockSkewThreshold
	}
	if opts.LogsUncheckedRepeat <= 0 {
		opts.LogsUncheckedRepeat = DefaultLogsUncheckedRepeat
	}
	if opts.FinalityStallAfter <= 0 {
		opts.FinalityStallAfter = DefaultFinalityStallAfter
	}
	if opts.Verify < watcher.VerifyAuto || opts.Verify > watcher.VerifyStrict {
		return nil, fmt.Errorf("corebridge: unknown verify mode %d", opts.Verify)
	}
	return &Bridge{
		svc:    svc,
		store:  checkpoints,
		opts:   opts,
		log:    opts.Logger,
		status: Status{State: StateIdle, Reason: "not started", Alerts: make(map[AlertKind]uint64)},
	}, nil
}

// Status returns a snapshot of the bridge.
func (b *Bridge) Status() Status {
	b.mu.Lock()
	st := b.status
	w := b.w
	st.Alerts = make(map[AlertKind]uint64, len(b.status.Alerts))
	for k, v := range b.status.Alerts {
		st.Alerts[k] = v
	}
	st.Endpoints = slices.Clone(b.status.Endpoints)
	st.Warnings = slices.Clone(b.status.Warnings)
	b.mu.Unlock()
	if w != nil {
		st.Watcher = w.Status()
	}
	return st
}

// Rescan asks the running watcher to scan again from block `from` (see
// watcher.Watcher.Rescan). It reports whether a watcher is running.
func (b *Bridge) Rescan(from uint64) bool {
	b.mu.Lock()
	w := b.w
	b.mu.Unlock()
	if w == nil {
		return false
	}
	w.Rescan(from)
	return true
}

func (b *Bridge) setState(state State, reason string) {
	b.mu.Lock()
	changed := b.status.State != state || b.status.Reason != reason
	b.status.State, b.status.Reason = state, reason
	b.mu.Unlock()
	if changed && state == StateIdle {
		b.log.Info("chain watcher idle", "reason", reason)
	}
}

func (b *Bridge) alert(a Alert) {
	a.At = time.Now()
	b.mu.Lock()
	b.status.Alerts[a.Kind]++
	b.status.LastAlert = a
	b.mu.Unlock()
	if b.opts.OnAlert != nil {
		b.opts.OnAlert(a)
	}
}

// sys returns ctx acting as the chain watcher's system actor.
func sys(ctx context.Context) context.Context {
	return core.WithActor(ctx, core.SystemActor(ActorName))
}

// plan is the configuration one generation runs with: everything that, when
// it changes, requires the watcher to be rebuilt.
type plan struct {
	// revision identifies the configuration (core.ChainState.Revision):
	// everything the generation reports carries it, and the core refuses
	// it once the configuration has been replaced. A plan equals another
	// only for the same revision — a configuration switched away and back
	// is another one.
	revision uint64
	chainID  uint64
	token    ethaddr.Address
	urls     []string // usable endpoints, in order of preference
	all      []string // endpoints as configured, usable or not
	required int      // the core's current finality depth
}

func (p plan) equal(o plan) bool {
	return p.revision == o.revision && p.chainID == o.chainID && p.token == o.token && p.required == o.required &&
		slices.Equal(p.urls, o.urls) && slices.Equal(p.all, o.all)
}

// fenced returns ctx acting as the chain watcher, for facts made under the
// configuration of p (core.WithChainRevision).
func (p plan) fenced(ctx context.Context) context.Context {
	return core.WithChainRevision(sys(ctx), p.revision)
}

// usableURL reports whether the RPC client can use an endpoint URL. The core
// also accepts ws:// and wss:// URLs; the client speaks JSON-RPC over HTTP
// only.
func usableURL(u string) bool {
	u = strings.ToLower(strings.TrimSpace(u))
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

const unsupportedScheme = "websocket endpoints are not supported; enter the endpoint's http(s) URL"

// planFor derives the plan from the core's state. idle is non-empty when
// there is nothing to run, and says why.
func planFor(cs core.ChainState) (p plan, idle string) {
	if !cs.SetupCompleted {
		return p, "setup is not completed"
	}
	if cs.Problem != "" {
		// Settings an earlier version stored, which the core would refuse
		// now: nothing runs on them until they are repaired.
		p.revision = cs.Revision
		return p, "the network settings cannot be used: " + cs.Problem + " — fix them in the network settings"
	}
	p.revision = cs.Revision
	p.chainID = cs.Network.ChainID
	p.required = cs.RequiredConfirmations
	p.all = slices.Clone(cs.Network.RPCURLs)
	for _, u := range p.all {
		if usableURL(u) && !slices.Contains(p.urls, u) {
			p.urls = append(p.urls, u)
		}
	}
	token, err := ethaddr.Parse(strings.ToLower(cs.Asset.Contract))
	switch {
	case p.chainID == 0:
		return p, "no chain configured"
	case err != nil || token.IsZero():
		return p, "the configured asset has no valid contract address"
	case len(p.urls) == 0:
		return p, "no usable RPC endpoint configured (http or https)"
	}
	p.token = token
	return p, ""
}

// errReconfigure ends a generation because the configuration changed.
var errReconfigure = errors.New("corebridge: configuration changed")

// Run supervises the watcher until ctx is done and returns ctx.Err(). It
// never returns for any other reason.
func (b *Bridge) Run(ctx context.Context) error {
	sub := b.svc.Subscribe()
	defer sub.Close()

	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			b.setState(StateIdle, "stopped")
			return err
		}
		cs, err := b.svc.ChainState(sys(ctx))
		if err != nil && ctx.Err() != nil {
			continue // shutting down, not a failure: the check above returns
		}
		if err != nil {
			failures++
			b.setState(StateRetrying, "read chain state: "+err.Error())
			b.log.Error("chain watcher cannot read the core's chain state", "error", err)
			pause(ctx, b.backoff(failures))
			continue
		}
		p, idle := planFor(cs)
		if idle != "" {
			failures = 0
			b.setState(StateIdle, idle)
			if cs.SetupCompleted {
				b.reportUnusable(ctx, p)
			}
			// Wait for a change: an event, or the periodic re-read.
			select {
			case <-ctx.Done():
			case <-sub.C():
			case <-time.After(b.opts.ReloadInterval):
			}
			continue
		}

		started := time.Now()
		err = b.runGeneration(ctx, p, cs, sub)
		switch {
		case ctx.Err() != nil:
		case errors.Is(err, errReconfigure):
			failures = 0
			b.log.Info("chain watcher restarting with new configuration")
		default:
			if time.Since(started) > time.Minute {
				failures = 0
			}
			failures++
			wait := b.backoff(failures)
			b.setState(StateRetrying, err.Error())
			b.log.Error("chain watcher stopped; retrying", "error", err, "retry_in", wait)
			pause(ctx, wait)
		}
	}
}

func (b *Bridge) backoff(failures int) time.Duration {
	return min(time.Second<<min(failures-1, 10), b.opts.MaxBackoff)
}

// pause waits for d or until ctx is done; it reports whether d elapsed.
func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// runGeneration builds a client and a watcher for p and runs them until the
// context ends, the configuration changes (errReconfigure) or the watcher
// stops with an error.
func (b *Bridge) runGeneration(ctx context.Context, p plan, cs core.ChainState, sub *core.Subscription) error {
	genCtx, cancel := context.WithCancel(ctx)
	g, err := b.newGeneration(genCtx, p, cs)
	if err != nil {
		cancel()
		return err
	}
	w := g.w

	labels := make([]string, len(p.urls))
	for i, u := range p.urls {
		labels[i] = evm.RedactURL(u)
	}
	b.mu.Lock()
	b.w = w
	b.status.Generation++
	b.status.State, b.status.Reason = StateRunning, ""
	b.status.ChainID, b.status.Token, b.status.Endpoints = p.chainID, p.token.String(), labels
	generation := b.status.Generation
	b.mu.Unlock()
	b.log.Info("chain watcher started", "generation", generation, "chain_id", p.chainID, "token", p.token.String(),
		"endpoints", labels, "settle_depth", b.opts.SettleDepth, "required_confirmations", p.required)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		g.monitor(genCtx)
	}()
	go func() {
		defer wg.Done()
		g.reportLoop(genCtx)
	}()
	done := make(chan error, 1)
	go func() { done <- w.Run(genCtx) }()

	stop := func() {
		cancel()
		<-done
		wg.Wait()
		b.mu.Lock()
		b.w = nil
		b.status.Warnings = nil
		b.mu.Unlock()
	}
	reload := time.NewTicker(b.opts.ReloadInterval)
	defer reload.Stop()
	for {
		select {
		case err := <-done:
			cancel()
			wg.Wait()
			b.mu.Lock()
			b.w = nil
			b.status.Warnings = nil
			b.mu.Unlock()
			if errors.Is(err, watcher.ErrHalt) {
				return errReconfigure
			}
			if err == nil {
				err = errors.New("watcher stopped")
			}
			return err
		case <-ctx.Done():
			stop()
			return ctx.Err()
		case <-sub.C():
		case <-reload.C:
		}
		// An event was appended (or it is time for the periodic check):
		// has the configuration changed? The watcher notices on its own
		// when a tick gets as far as reading the watch set — but a watcher
		// whose endpoints are all down never gets that far, and replacing
		// the endpoints is exactly the change that has to get through.
		now, err := b.svc.ChainState(sys(ctx))
		if err != nil {
			// Unknown is not "nothing": a watcher that idles looks now.
			w.Wake()
			continue
		}
		if np, _ := planFor(now); !np.equal(p) {
			stop()
			return errReconfigure
		}
		// The core is waiting for something: a payment that is open, a
		// transfer that is not final. A watcher that is idling — quiet or
		// not — must not finish its pause first (see "Idle and quiet"). The
		// address is in the core's store by now — the event was appended in
		// the transaction that created it — so the tick this causes reads it.
		// Wake does nothing to a watcher that is not idle.
		//
		// Not "the watch list is not empty": while it holds the addresses of
		// closed payments only, the watcher is meant to rest, and an event
		// that changes nothing about that — a resolution, a webhook endpoint
		// edited, the periodic re-read itself — must not cost a scan.
		if now.Active() {
			w.Wake()
		}
	}
}

// newGeneration builds what runGeneration runs for p: the RPC client, the
// watcher with its checkpoint reconciled against the core's state (resume),
// and the generation that is the watcher's sink and watch set. ctx is the
// generation's: it ends with it.
func (b *Bridge) newGeneration(ctx context.Context, p plan, cs core.ChainState) (*generation, error) {
	client, err := evm.NewClient(evm.Config{
		ChainID:   p.chainID,
		Endpoints: p.urls,
		Logger:    b.log,
		Endpoint:  b.opts.Endpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("corebridge: build RPC client: %w", err)
	}
	start, rescan, err := b.resume(ctx, p, cs)
	if err != nil {
		return nil, err
	}
	g := &generation{b: b, plan: p, client: client, ctx: ctx}
	w, err := watcher.New(watcher.Config{
		Client:       client,
		Token:        p.token,
		Sink:         g,
		Watch:        g,
		Store:        b.store,
		Logger:       b.log,
		PollInterval: b.opts.PollInterval,
		IdleInterval: b.opts.IdlePollInterval,
		SettleDepth:  b.opts.SettleDepth,
		MaxBackoff:   b.opts.MaxBackoff,
		// Remember at least as much history as the core can still take
		// back: a transfer is not final for the core until the processed
		// head is `required` blocks past it.
		ReorgWindow: max(watcher.DefaultReorgWindow, 2*(uint64(max(p.required, 0))+b.opts.SettleDepth)),
		StartBlock:  start,
		// Whether the chain has finality is the network's to say, not the
		// endpoints': on a network with finality, a block is final only
		// when two endpoints attest it — and what the core holds as final
		// (two endpoints attested it once) stays final, also for a
		// watcher that lost its checkpoint.
		NoFinality:     !core.HasFinality(p.chainID),
		FinalizedFloor: finalFloor(cs.Head, p.chainID),
		Verify:         b.verifyMode(p),
		OnDeepReorg: func(d watcher.DeepReorg) {
			b.alert(Alert{Kind: AlertDeepReorg, Message: d.Reason + "; re-scanning from block " + fmt.Sprint(d.RescanFrom)})
		},
		OnIncident: g.incident,
		// The node status is published the moment a tick has changed
		// it, not when the report loop next comes round: the first tick
		// of a process in particular ends the "starting" report.
		OnTick:         func() { g.report(ctx) },
		Now:            g.now,
		MaxFutureDrift: b.opts.MaxHeadTimeAhead,
		Sleep:          b.opts.sleep,
	})
	if err != nil {
		return nil, fmt.Errorf("corebridge: build watcher: %w", err)
	}
	g.w = w
	if rescan != nil {
		w.Rescan(*rescan)
	}
	return g, nil
}

// finalFloor returns the core's final block, if its head is one of the
// planned chain (0 otherwise: nothing is final yet that the watcher has to
// know of).
func finalFloor(h core.ChainHead, chainID uint64) uint64 {
	if h.ChainID != chainID || h.Number == 0 {
		return 0
	}
	return h.Final
}

// verifyMode returns the watcher's verification mode for a plan. VerifyStrict
// needs two endpoints; with a single one configured it cannot be honoured
// and VerifyAuto (same endpoint) is the only way to run at all.
func (b *Bridge) verifyMode(p plan) watcher.VerifyMode {
	if b.opts.Verify == watcher.VerifyStrict && len(p.urls) < 2 {
		b.log.Warn("strict verification needs two RPC endpoints; only one is configured, so its answers are checked against itself")
		return watcher.VerifyAuto
	}
	return b.opts.Verify
}

// resume reconciles the watcher's checkpoint with the core's state and
// decides where scanning starts: start is the first block to scan when there
// is no usable checkpoint (nil: just below the chain tip), rescan the block
// to rewind to when there is one but the core is behind it.
//
// The rule: never start above anything the core has not fully processed.
// The lower of the checkpoint's cursor and the core's processed head is
// safe — without a checkpoint, the head's final block (core.ChainHead.Final):
// what lies above it may have been reorganised since; so is the block below
// the oldest transfer the core still regards as pending.
//
// What this cannot decide is where a watcher without a checkpoint starts when
// the core has no processed head either: that depends on the payments that
// exist when the watcher's first tick succeeds, not on those that exist now.
// The watcher decides it then, from the Since of the watched addresses (see
// generation.Watched).
func (b *Bridge) resume(ctx context.Context, p plan, cs core.ChainState) (start, rescan *uint64, err error) {
	data, err := b.store.LoadCheckpoint(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("corebridge: load checkpoint: %w", err)
	}
	var cursor uint64
	have := false
	if len(data) > 0 {
		info, ierr := watcher.InspectCheckpoint(data)
		switch {
		case ierr != nil:
			b.log.Error("chain watcher checkpoint is unreadable; discarding it and rebuilding from the core's state", "error", ierr)
			b.alert(Alert{Kind: AlertCheckpointDiscarded, Message: "unreadable checkpoint: " + ierr.Error()})
		case info.ChainID != p.chainID || info.Token != p.token:
			b.log.Warn("chain watcher checkpoint belongs to another chain or token; discarding it",
				"checkpoint_chain_id", info.ChainID, "checkpoint_token", info.Token.String(), "chain_id", p.chainID, "token", p.token.String())
			b.alert(Alert{Kind: AlertCheckpointDiscarded, Message: fmt.Sprintf("checkpoint was for chain %d token %s", info.ChainID, info.Token)})
		default:
			cursor, have = info.Cursor, true
		}
		if !have {
			if err := b.store.SaveCheckpoint(ctx, nil); err != nil {
				return nil, nil, fmt.Errorf("corebridge: discard checkpoint: %w", err)
			}
		}
	}

	if len(data) == 0 && !cs.Head.Checked.IsZero() {
		// No checkpoint at all, while the core's head says blocks are on
		// record as settled on one endpoint's logs and still to be scanned
		// by a second one: that record was the checkpoint's, and is lost
		// with it (as with one that is discarded).
		msg := fmt.Sprintf("no checkpoint: the record of the blocks settled on one endpoint's logs since %s, still to be scanned by a second log endpoint, is lost with it; those blocks are not scanned again",
			cs.Head.Checked.UTC().Format(time.RFC3339))
		b.log.Error("chain watcher checkpoint is missing; " + msg)
		b.alert(Alert{Kind: AlertCheckpointDiscarded, Message: msg})
	}

	const unknown = ^uint64(0)
	resume := unknown
	if have {
		resume = cursor
	}
	if h := cs.Head.Number; h > 0 {
		if !have {
			// Without a checkpoint nothing remembers the blocks above the
			// head's final block, and a reorganisation may have replaced
			// them — the head itself included — since the core processed
			// them: they are scanned again.
			h = min(h, cs.Head.Final)
		}
		resume = min(resume, h)
	}
	oldestPending := unknown
	for _, t := range cs.PendingTransfers {
		if t.BlockNumber > 0 {
			oldestPending = min(oldestPending, t.BlockNumber)
			resume = min(resume, t.BlockNumber-1)
		}
	}
	switch {
	case resume == unknown:
		return nil, nil, nil // no position known: the watcher starts from the watched addresses, or the tip
	case !have:
		first := resume + 1
		b.log.Info("chain watcher has no checkpoint; starting from the core's state", "first_block", first,
			"core_head", cs.Head.Number, "watched_addresses", len(cs.Addresses), "pending_transfers", len(cs.PendingTransfers))
		return &first, nil, nil
	case resume < cursor:
		first := resume + 1
		if h := cs.Head.Number; h > 0 && h < cursor {
			b.log.Warn("the core is behind the watcher's checkpoint; scanning again", "from_block", first,
				"checkpoint_cursor", cursor, "core_head", h)
		} else {
			// The usual restart: the core holds transfers that are not
			// final yet, and their blocks are looked at again.
			b.log.Info("re-examining the blocks of transfers that are not final yet", "from_block", first,
				"checkpoint_cursor", cursor, "oldest_pending_transfer_block", oldestPending, "pending_transfers", len(cs.PendingTransfers))
		}
		return nil, &first, nil
	}
	return nil, nil, nil
}

// sinceMargin is subtracted from a block number derived from a timestamp.
const sinceMargin = 64

// blockAtOrBefore returns the number of the newest block whose timestamp is
// not after t, by binary search over block timestamps (about 27 header
// requests on a chain of 10^8 blocks).
func blockAtOrBefore(ctx context.Context, e *evm.Endpoint, t time.Time) (uint64, error) {
	head, err := e.HeaderByTag(ctx, evm.TagLatest)
	if err != nil {
		return 0, err
	}
	target := uint64(max(t.Unix(), 0))
	if head.Time <= target {
		return head.Number, nil
	}
	lo, hi := uint64(0), head.Number // time(lo) <= target < time(hi); block 0 counts as "before everything"
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		h, err := e.HeaderByNumber(ctx, mid)
		switch {
		case errors.Is(err, evm.ErrNotFound):
			lo = mid // pruned: older than anything of interest
		case err != nil:
			return 0, err
		case h.Time <= target:
			lo = mid
		default:
			hi = mid
		}
	}
	return lo, nil
}
