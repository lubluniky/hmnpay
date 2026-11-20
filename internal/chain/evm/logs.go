package evm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
)

// Defaults for the adaptive log scanner. They come from measurements against
// public BSC endpoints (see the package tests gated by HMNPAY_LIVE_RPC):
// providers that allow eth_getLogs at all cap the block range anywhere
// between 25 and 10 000 blocks, and answer a 1 000-block filtered query in
// 0.1–2.5 s but a 5 000-block one in 1–15 s.
const (
	// DefaultLogRange is the block span of the first eth_getLogs call to an
	// endpoint whose limits are not known yet.
	DefaultLogRange = 1000
	// DefaultMaxLogRange is the largest block span ever requested.
	DefaultMaxLogRange = 5000
	// DefaultMaxRecipients is the largest number of recipient addresses
	// OR-ed into one filter. go-ethereum based nodes reject more than 1000
	// alternatives per topic position; 500 keeps the request body near
	// 35 KiB and leaves headroom.
	DefaultMaxRecipients = 500
)

// LogOptions tunes the adaptive eth_getLogs scanner of an endpoint.
type LogOptions struct {
	// InitialRange is the block span tried first. Default: DefaultLogRange.
	InitialRange uint64
	// MaxRange caps the block span. Default: DefaultMaxLogRange.
	MaxRange uint64
	// MaxRecipients caps the recipients per filter. Default:
	// DefaultMaxRecipients.
	MaxRecipients int
}

func (o LogOptions) withDefaults() LogOptions {
	if o.MaxRange == 0 {
		o.MaxRange = DefaultMaxLogRange
	}
	if o.InitialRange == 0 {
		o.InitialRange = DefaultLogRange
	}
	if o.InitialRange > o.MaxRange {
		o.InitialRange = o.MaxRange
	}
	if o.MaxRecipients <= 0 {
		o.MaxRecipients = DefaultMaxRecipients
	}
	return o
}

// How long eth_getLogs is left alone on an endpoint after it refused even a
// minimal query: logsCooldownBase * 2^(strikes-1), capped. And how many such
// refusals in a row, with no log query answered in between, it takes before
// an endpoint that has never served logs is held to have no log service
// (LogsHealth.NoService): three, which with the cooldowns in between means it
// has refused consistently for three minutes.
const (
	logsCooldownBase  = time.Minute
	logsCooldownMax   = 30 * time.Minute
	logsAbsentStrikes = LogsAbsentRefusals
)

// LogsAbsentRefusals is the number of consecutive refusals of a minimal log
// query after which an endpoint that has never answered one is held to have
// no log service (LogsHealth.NoService).
const LogsAbsentRefusals = 3

// logState is what an endpoint has learned about its provider's eth_getLogs
// limits. Guarded by Endpoint.mu.
type logState struct {
	opts       LogOptions
	span       uint64 // block span to request next
	minFail    uint64 // smallest span the provider rejected as too large (0: none)
	recipients int    // recipients per filter
	known      bool   // at least one eth_getLogs call has succeeded
	strikes    int    // consecutive "does not serve logs" verdicts
	blockedTil time.Time
	lastErr    string
}

func newLogState(opts LogOptions) logState {
	return logState{opts: opts, span: opts.InitialRange, recipients: opts.MaxRecipients}
}

// LogsHealth describes what is known about eth_getLogs on an endpoint.
type LogsHealth struct {
	// Known is true once an eth_getLogs call has succeeded (or the caller
	// said so: MarkLogsKnown). An endpoint that is Known is never marked
	// Unavailable: when it refuses a log query it is throttling.
	Known bool
	// Unavailable is true while eth_getLogs is not to be asked of the
	// endpoint: it has never answered a log query and refused a minimal
	// one. UnavailableUntil is when it will be tried again.
	Unavailable      bool
	UnavailableUntil time.Time
	// NoService is the verdict "this endpoint has no log service": it has
	// never answered a log query and has refused a minimal one
	// logsAbsentStrikes times in a row (or the caller remembers that it
	// had: MarkLogsAbsent). One refusal is not that — a provider that
	// throttles answers the same way — and a caller that needs a second
	// log source must not write an endpoint off before the verdict stands.
	// The endpoint is asked again every time UnavailableUntil passes, and
	// the verdict ends with the first log query it answers.
	NoService bool
	// Refusals is the number of consecutive refusals behind Unavailable.
	Refusals int
	// Range is the block span the scanner currently uses per call.
	Range uint64
	// Recipients is the number of recipient addresses used per call.
	Recipients int
	// LastError is the most recent eth_getLogs refusal.
	LastError string
}

func (s *logState) snapshot(now time.Time) LogsHealth {
	return LogsHealth{
		Known:            s.known,
		Unavailable:      now.Before(s.blockedTil),
		UnavailableUntil: s.blockedTil,
		NoService:        !s.known && s.strikes >= logsAbsentStrikes,
		Refusals:         s.strikes,
		Range:            s.span,
		Recipients:       s.recipients,
		LastError:        s.lastErr,
	}
}

// observeLogs records the outcome of an eth_getLogs call in the general
// health record. Only failures of the endpoint as such count there; refusals
// of this particular query are handled by the scanner.
func (e *Endpoint) observeLogs(ctx context.Context, elapsed time.Duration, err error) {
	if err == nil {
		e.recordSuccess(elapsed)
		return
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	if !Unreachable(err) {
		e.mu.Lock()
		e.health.answers++ // a refusal is an answer (Health.Answers)
		e.mu.Unlock()
	}
	var ee *Error
	if !errors.As(err, &ee) {
		return
	}
	switch ee.Kind {
	case KindTransport, KindHTTP, KindBadResponse, KindRateLimited:
		e.recordFailure(err)
	}
}

// TransferQuery asks for the ERC-20 transfers of one token to a set of
// recipients within an inclusive block range.
type TransferQuery struct {
	Token     ethaddr.Address
	FromBlock uint64
	ToBlock   uint64
	// Recipients is the set of `to` addresses of interest. It must not be
	// empty: an unfiltered scan of a busy token returns thousands of logs
	// per block.
	Recipients []ethaddr.Address
}

// TransferScan is the result of TransferLogs.
type TransferScan struct {
	// Transfers are the matching transfers found in blocks
	// FromBlock..Next-1 — none from a block the scan does not vouch for —
	// sorted by block number and log index.
	Transfers []Transfer
	// Next is the first block the result does not cover: every matching
	// transfer in blocks FromBlock..Next-1 is in Transfers. After a
	// complete scan Next is ToBlock+1. It is lower when
	//
	//   - the endpoint revealed that the backend answering had not reached
	//     ToBlock yet (see Endpoint.Logs) — the scan then ends at that
	//     backend's height, without an error; or
	//   - the scan failed part-way — TransferLogs then returns the error
	//     together with what it had covered until then.
	//
	// Blocks from Next on must be treated as not scanned.
	Next uint64
	// BackendHead is the lowest block height reported by the backends that
	// answered the log queries of this scan (0 if the provider does not
	// accept batches, so that the height is unknown). A caller that leaves
	// a safety margin below the chain head — to let a log index catch up
	// with its node — must measure the margin from here, not from a head
	// obtained in a separate request.
	BackendHead uint64
}

// Complete reports whether the scan covered the whole range of q.
func (s TransferScan) Complete(q TransferQuery) bool { return s.Next > q.ToBlock }

// TransferLogs returns the ERC-20 Transfer events emitted by q.Token to any
// of q.Recipients in blocks q.FromBlock..q.ToBlock.
//
// The result never has holes: it is complete for the whole recipient set for
// all blocks below TransferScan.Next. Even when an error is returned, the
// scan describes the prefix of the range that was covered before the error
// (Next == FromBlock if nothing was), so a caller working through a long
// range can save that progress and retry from Next.
//
// To get there the scanner
// splits the block range and the recipient set into as many eth_getLogs calls
// as the provider requires, and adapts to its limits as it learns them:
//
//   - "block range too large" shrinks the span, using the limit stated in the
//     error message if there is one, otherwise halving; the span grows back
//     after successes, up to the smallest span known to fail.
//   - "too many results", an oversized response or a slow answer halves the
//     span.
//   - "request too large" (too many topics) halves the recipients per call.
//   - An error the scanner does not recognise triggers a minimal probe (one
//     block, then one block with one recipient). If that works, the original
//     failure is treated as a limit and the scanner shrinks; if not,
//     eth_getLogs is marked unavailable on this endpoint for a while and the
//     error is returned, so the caller fails over.
//   - Rate limiting, transport failures and malformed data end the scan
//     immediately; they count against the endpoint's health, so the caller
//     backs off or fails over.
//
// Every returned log is verified to match the query (block range, emitting
// contract, topic0, recipient). Logs that carry the Transfer topic but are
// not well-formed ERC-20 transfers, and logs flagged as removed, are dropped.
func (e *Endpoint) TransferLogs(ctx context.Context, q TransferQuery) (TransferScan, error) {
	scan := TransferScan{Next: q.FromBlock}
	if q.FromBlock > q.ToBlock {
		return scan, fmt.Errorf("evm: invalid block range %d..%d", q.FromBlock, q.ToBlock)
	}
	if len(q.Recipients) == 0 {
		return scan, errors.New("evm: TransferLogs needs at least one recipient")
	}
	topics := make([]Hash, len(q.Recipients))
	for i, a := range q.Recipients {
		topics[i] = addressTopic(a)
	}
	slices.SortFunc(topics, func(a, b Hash) int { return slices.Compare(a[:], b[:]) })
	topics = slices.Compact(topics)

	var search spanSearch
	var scanErr error
	for cur := q.FromBlock; ; {
		if scanErr = ctx.Err(); scanErr != nil {
			break
		}
		span := e.logSpan()
		if search.probing {
			span = 1
		}
		end := q.ToBlock
		if span-1 < q.ToBlock-cur {
			end = cur + span - 1
		}
		part, covered, backendHead, err := e.scanSpan(ctx, q.Token, cur, end, end-cur+1 == span, topics, &search)
		if backendHead != 0 && (scan.BackendHead == 0 || backendHead < scan.BackendHead) {
			scan.BackendHead = backendHead
		}
		if errors.Is(err, errAdjusted) {
			continue // limits were adjusted; ask again for the same blocks
		}
		if err != nil {
			scanErr = err
			break
		}
		scan.Transfers = append(scan.Transfers, part...)
		scan.Next = covered + 1
		if covered < end || end == q.ToBlock {
			break // the backend has not got further, or done
		}
		cur = end + 1
	}
	// Only the covered prefix is returned. A scan that asks in several
	// calls (many recipients) can have been answered by backends at
	// different heights: what an earlier call found above the height the
	// last one vouches for is not part of a result that is "complete below
	// Next", and is found again when those blocks are scanned.
	scan.Transfers = slices.DeleteFunc(scan.Transfers, func(t Transfer) bool { return t.BlockNumber >= scan.Next })
	slices.SortFunc(scan.Transfers, func(a, b Transfer) int {
		if a.BlockNumber != b.BlockNumber {
			if a.BlockNumber < b.BlockNumber {
				return -1
			}
			return 1
		}
		if a.LogIndex != b.LogIndex {
			if a.LogIndex < b.LogIndex {
				return -1
			}
			return 1
		}
		return 0
	})
	// A position in a block holds exactly one log. Two answers for the same
	// position — duplicates, or logs from two different blocks at one
	// height — mean the endpoint is not describing one chain.
	for i := 1; i < len(scan.Transfers); i++ {
		a, b := scan.Transfers[i-1], scan.Transfers[i]
		if a.BlockNumber == b.BlockNumber && (a.LogIndex == b.LogIndex || a.BlockHash != b.BlockHash) {
			return TransferScan{Next: q.FromBlock}, e.badResponse("eth_getLogs",
				fmt.Errorf("conflicting logs for block %d (log index %d and %d)", a.BlockNumber, a.LogIndex, b.LogIndex))
		}
	}
	return scan, scanErr
}

// errAdjusted is returned by scanSpan after it changed the span or the
// recipients per call in response to a refusal; the caller asks again.
var errAdjusted = errors.New("log scan limits adjusted")

// spanSearch is the state of the minimal-probe procedure that follows a
// refusal the scanner does not recognise.
type spanSearch struct {
	probing     bool   // the next span is a one-block probe
	probeFailed uint64 // the span whose refusal started the probe
	firstErr    error  // that refusal, reported if the endpoint is given up on
}

// scanSpan queries the blocks cur..end for all recipients, one eth_getLogs
// call per chunk of recipients. full says whether cur..end is a whole span of
// the current size (only then does success say anything about growing it).
// It returns the transfers and the last block covered, which is below end if
// the answering backend had not reached end.
func (e *Endpoint) scanSpan(ctx context.Context, token ethaddr.Address, cur, end uint64, full bool, recipients []Hash, search *spanSearch) (out []Transfer, covered, lowestHead uint64, err error) {
	covered = end
	asked := end - cur + 1
	single := asked == 1
	var slowest time.Duration

	for start := 0; start < len(recipients); {
		chunk := recipients[start:min(start+e.recipientChunk(), len(recipients))]
		began := time.Now()
		logs, backendHead, headKnown, err := e.logs(ctx, LogFilter{
			FromBlock: cur,
			ToBlock:   covered,
			Address:   token,
			Topics:    [][]Hash{{TransferTopic}, nil, chunk},
		})
		slowest = max(slowest, time.Since(began))

		if err == nil {
			if headKnown {
				lowestHead = minNonZero(lowestHead, backendHead)
			}
			if headKnown && backendHead < covered {
				// The backend that answered has not reached the end of
				// the range: it can only vouch for blocks up to its own
				// height.
				if backendHead < cur {
					lag := &Error{Kind: KindLagging, Endpoint: e.t.label, Method: "eth_getLogs",
						Message: fmt.Sprintf("backend is at block %d, below the requested range %d..%d", backendHead, cur, end)}
					e.recordFailure(lag)
					return nil, 0, lowestHead, lag
				}
				covered = backendHead
			}
			for _, l := range logs {
				t, derr := DecodeTransfer(l)
				if derr != nil {
					continue // not an ERC-20 transfer, or removed
				}
				out = append(out, t)
			}
			start += len(chunk)
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return nil, 0, lowestHead, cerr
		}

		switch kind, _ := KindOf(err); kind {
		case KindRangeTooLarge:
			if single {
				return nil, 0, lowestHead, e.logsRefused(err)
			}
			var ee *Error
			errors.As(err, &ee)
			e.logRangeRejected(asked, rangeHint(ee.Message, asked))
			return nil, 0, lowestHead, errAdjusted
		case KindTooManyResults:
			switch {
			case !single:
				e.logShrink(asked)
			case len(chunk) > 1:
				e.shrinkRecipients(len(chunk))
			default:
				return nil, 0, lowestHead, e.logsRefused(err)
			}
			return nil, 0, lowestHead, errAdjusted
		case KindRequestTooLarge:
			if len(chunk) == 1 {
				return nil, 0, lowestHead, e.logsRefused(err)
			}
			e.shrinkRecipients(len(chunk))
			return nil, 0, lowestHead, errAdjusted
		case KindTransport:
			// Most likely a timeout on a heavy query: ask for less next
			// time, but report the failure now — the caller fails over.
			if !single {
				e.logShrink(asked)
			}
			return nil, 0, lowestHead, err
		case KindLagging:
			// The node refused the range because it ends above its own
			// head. If it said where its head is (or the batched block
			// number did), ask again up to there: that is as far as it
			// can vouch for, and the scan ends there without an error.
			if headKnown && backendHead >= cur && backendHead < covered {
				lowestHead = minNonZero(lowestHead, backendHead)
				covered = backendHead
				continue
			}
			lag := &Error{Kind: KindLagging, Endpoint: e.t.label, Method: "eth_getLogs",
				Message: fmt.Sprintf("backend is behind the requested range %d..%d", cur, end)}
			if ee := (*Error)(nil); errors.As(err, &ee) {
				lag.Code, lag.Message = ee.Code, ee.Message
			}
			e.recordFailure(lag)
			return nil, 0, lowestHead, lag
		case KindHTTP, KindBadResponse, KindRateLimited:
			// The endpoint's problem as a whole (already counted against
			// its health): let the caller back off or fail over.
			return nil, 0, lowestHead, err
		default:
			// The node refused the query in words this client does not
			// recognise as a limit (or in words that are ambiguous, like
			// the bare "limit exceeded" of BSC's dataseed nodes). Find out
			// with minimal queries whether eth_getLogs works at all.
			if search.firstErr == nil {
				search.firstErr = err
			}
			if !single {
				search.probing, search.probeFailed = true, asked
				return nil, 0, lowestHead, errAdjusted
			}
			// A single block was refused. The recipient list may be what
			// the provider objects to: try the smallest possible request
			// before giving up on the endpoint.
			if len(chunk) > 1 {
				_, perr := e.Logs(ctx, LogFilter{
					FromBlock: cur, ToBlock: cur, Address: token,
					Topics: [][]Hash{{TransferTopic}, nil, chunk[:1]},
				})
				if cerr := ctx.Err(); cerr != nil {
					return nil, 0, lowestHead, cerr
				}
				if perr == nil {
					e.shrinkRecipients(len(chunk))
					return nil, 0, lowestHead, errAdjusted
				}
			}
			return nil, 0, lowestHead, e.logsRefused(search.firstErr)
		}
	}

	if search.probing {
		// A single block works, so the earlier refusal was about the size
		// of the request.
		e.logRangeRejected(search.probeFailed, 0)
	}
	e.logSucceeded(asked, full && !search.probing, slowest)
	search.probing, search.firstErr = false, nil
	return out, covered, lowestHead, nil
}

// minNonZero returns the smaller of a and b, treating 0 as "unknown".
func minNonZero(a, b uint64) uint64 {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

// logsRefused handles a refusal of eth_getLogs that is not about the size of
// the request.
//
// On an endpoint that has never answered a log query, it marks eth_getLogs as
// unavailable for a growing period and returns the refusal: this is what the
// providers without a log service look like (BSC's dataseed nodes answer
// every eth_getLogs with a bare -32005 "limit exceeded"). It is also what a
// provider looks like that throttles in those words, which is why the verdict
// "no log service" takes several refusals in a row (LogsHealth.NoService).
//
// On an endpoint that has served logs (in this process, or according to the
// caller: MarkLogsKnown), the same answer is throttling or a passing fault.
// The endpoint keeps counting as one that serves logs — a caller that relies
// on a second log source must wait for it, not carry on without it — and the
// refusal is counted against its health like a rate limit, so that failover
// moves on for a while.
func (e *Endpoint) logsRefused(err error) error {
	e.mu.Lock()
	s := &e.logState
	s.lastErr = failureText(err)
	if !s.known {
		s.strikes++
		cooldown := logsCooldownBase << min(s.strikes-1, 10)
		if cooldown > logsCooldownMax {
			cooldown = logsCooldownMax
		}
		s.blockedTil = e.opts.Now().Add(cooldown)
		e.mu.Unlock()
		return err
	}
	e.mu.Unlock()
	var ee *Error
	if errors.As(err, &ee) && (ee.Kind == KindRPC || ee.Kind == KindUnavailable) {
		limited := *ee
		limited.Kind = KindRateLimited
		err = &limited
	}
	e.recordFailure(err)
	return err
}

// MarkLogsKnown records that this endpoint is known to serve eth_getLogs —
// for a caller that remembers it across restarts (the watcher's checkpoint).
// From then on a refusal of eth_getLogs is treated as throttling, never as
// "no log service": see LogsHealth.Known.
func (e *Endpoint) MarkLogsKnown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logState.known = true
	e.logState.strikes = 0
	e.logState.blockedTil = time.Time{}
}

// MarkLogsAbsent records that this endpoint is remembered as one without a
// log service — for a caller that keeps the verdict across restarts (the
// watcher's checkpoint), so that it does not have to be reached again, by
// several refusals over several minutes, in every new process. The endpoint
// is still asked: the first log query it answers ends the verdict. It has no
// effect on an endpoint that is known to serve logs.
func (e *Endpoint) MarkLogsAbsent() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := &e.logState; !s.known && s.strikes < logsAbsentStrikes {
		s.strikes = logsAbsentStrikes
	}
}

// LogRange returns the block span the scanner currently uses per
// eth_getLogs call on this endpoint: the configured initial range until the
// provider's limit has been learned, then a span known to work. Callers
// catching up over long ranges should request this many blocks per
// TransferLogs call, so that each call is a single request and progress can
// be saved between calls.
func (e *Endpoint) LogRange() uint64 { return e.logSpan() }

func (e *Endpoint) logSpan() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.logState.span
}

func (e *Endpoint) recipientChunk() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.logState.recipients
}

// logSucceeded records a successful call that asked for `asked` blocks.
// fullSpan says whether the call used the whole current span (only then does
// success say anything about growing it).
func (e *Endpoint) logSucceeded(asked uint64, fullSpan bool, elapsed time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := &e.logState
	s.known = true
	s.strikes = 0
	s.blockedTil = time.Time{}
	switch {
	case elapsed > e.opts.Timeout/2 && asked > 1:
		// Close to timing out: back off before it does.
		s.span = max(asked/2, 1)
	case fullSpan && elapsed < e.opts.Timeout/4:
		// Double, but never up to a span the provider is known to
		// reject: the search for the limit is geometric, so the working
		// span ends up within a factor of two of it without probing the
		// limit again and again.
		grown := min(s.span*2, s.opts.MaxRange)
		if grown > s.span && (s.minFail == 0 || grown < s.minFail) {
			s.span = grown
		}
	}
}

// logRangeRejected records that the provider rejected a span of `failed`
// blocks as too large. hint is the limit stated by the provider, or 0.
func (e *Endpoint) logRangeRejected(failed, hint uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := &e.logState
	if s.minFail == 0 || failed < s.minFail {
		s.minFail = failed
	}
	next := failed / 2
	if hint > 0 && hint < failed {
		next = hint
	}
	s.span = max(min(next, s.span), 1)
}

// logShrink halves the span after a result-size or latency problem with a
// span of `failed` blocks. Unlike a range rejection this is not remembered as
// a hard limit: it depends on how busy the queried blocks were.
func (e *Endpoint) logShrink(failed uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := &e.logState
	s.span = max(min(failed/2, s.span), 1)
}

// shrinkRecipients halves the recipients-per-call after a request with
// `failed` recipients was refused.
func (e *Endpoint) shrinkRecipients(failed int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := &e.logState
	s.recipients = max(min(failed/2, s.recipients), 1)
}
