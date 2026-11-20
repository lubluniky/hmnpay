package evm

import (
	"context"
	"errors"
	"time"
)

// Cooldown bounds: after the n-th consecutive failure an endpoint is left
// alone for min(cooldownBase * 2^(n-1), cooldownMax).
const (
	cooldownBase = 2 * time.Second
	cooldownMax  = 60 * time.Second
)

// health is the per-endpoint record behind failover decisions and the status
// snapshot. It is guarded by Endpoint.mu.
type health struct {
	latency       time.Duration // exponentially weighted moving average
	failures      int           // consecutive failures
	cooldownUntil time.Time     // do not use before this instant
	faults        int           // faults reported within faultMemory
	faultUntil    time.Time     // end of the cooldown imposed by ReportFault
	lastFaultAt   time.Time
	lastErr       string
	lastErrAt     time.Time
	lastOKAt      time.Time
	block         uint64 // highest block number seen from this endpoint
	blockAt       time.Time
	height        uint64 // block number seen most recently (at blockAt)
	// unreachable is set while the most recent call got no answer at all
	// (see Unreachable) and nothing has been heard from the endpoint since.
	unreachable bool
	// answers counts the calls the endpoint answered (see Health.Answers).
	answers uint64
}

// Health is a point-in-time copy of an endpoint's health record.
type Health struct {
	// Latency is a moving average of successful round-trip times.
	Latency time.Duration
	// ConsecutiveFailures counts failures since the last success.
	ConsecutiveFailures int
	// CooldownUntil is the instant before which failover skips the
	// endpoint; zero if it is not cooling down.
	CooldownUntil time.Time
	// LastError and LastErrorAt describe the most recent failure.
	LastError   string
	LastErrorAt time.Time
	// LastSuccessAt is the time of the most recent successful call.
	LastSuccessAt time.Time
	// BlockNumber is the highest block number this endpoint has reported,
	// and BlockSeenAt when it last reported one. Height is the one it
	// reported then: what it says the chain's height is, as of BlockSeenAt
	// (BlockNumber never goes down; Height does when the endpoint's answer
	// does).
	BlockNumber uint64
	BlockSeenAt time.Time
	Height      uint64
	// Unreachable is true while the endpoint cannot be reached at all: its
	// most recent call ended without an answer — no connection, a timeout,
	// or a gateway answering 5xx in its place — and nothing has been heard
	// from it since. An endpoint that answers, whatever it answers (an
	// error, a rate limit, wrong data), is not unreachable.
	Unreachable bool
	// Answers is the number of calls the endpoint has answered — with a
	// result or with anything that is an answer (an error, a rate limit,
	// wrong data), not with silence. It only grows: a caller that has seen
	// it unchanged between two moments knows the endpoint answered nothing
	// in between, whoever asked (the pool's health check included).
	Answers uint64
	// Logs describes what is known about eth_getLogs on this endpoint.
	Logs LogsHealth
}

// Health returns the endpoint's current health record.
func (e *Endpoint) Health() Health {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.health
	return Health{
		Latency:             h.latency,
		ConsecutiveFailures: h.failures,
		CooldownUntil:       h.cooldownUntil,
		LastError:           h.lastErr,
		LastErrorAt:         h.lastErrAt,
		LastSuccessAt:       h.lastOKAt,
		BlockNumber:         h.block,
		BlockSeenAt:         h.blockAt,
		Height:              h.height,
		Unreachable:         h.unreachable,
		Answers:             h.answers,
		Logs:                e.logState.snapshot(e.opts.Now()),
	}
}

// observe records the outcome of a general call.
//
//   - A failure that is a property of the endpoint (transport, HTTP status,
//     rate limit, garbage, access denied) counts against it.
//   - A JSON-RPC error from a working node (KindRPC, range limits) is
//     neutral: the endpoint is alive, but nothing is learned about latency.
//   - Cancellation by the caller is ignored entirely.
func (e *Endpoint) observe(ctx context.Context, _ string, elapsed time.Duration, err error) {
	if err == nil {
		e.recordSuccess(elapsed)
		return
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	if !Unreachable(err) {
		e.mu.Lock()
		e.health.answers++
		e.mu.Unlock()
	}
	if retryElsewhere(err) {
		e.recordFailure(err)
	}
}

func (e *Endpoint) recordSuccess(elapsed time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := &e.health
	if h.latency == 0 {
		h.latency = elapsed
	} else {
		h.latency = (h.latency*7 + elapsed*3) / 10
	}
	h.failures = 0
	h.unreachable = false
	h.answers++
	// A request that succeeds ends a cooldown caused by requests that
	// failed. It does not end one imposed by ReportFault: an endpoint that
	// was caught returning wrong data answers promptly all the same.
	h.cooldownUntil = time.Time{}
	h.lastOKAt = e.opts.Now()
	if h.lastOKAt.Before(h.faultUntil) {
		h.cooldownUntil = h.faultUntil
	}
}

func (e *Endpoint) recordFailure(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := &e.health
	now := e.opts.Now()
	h.failures++
	h.lastErr = failureText(err)
	h.lastErrAt = now
	h.unreachable = Unreachable(err)
	cooldown := cooldownBase << min(h.failures-1, 10)
	if cooldown > cooldownMax {
		cooldown = cooldownMax
	}
	var ee *Error
	if errors.As(err, &ee) && ee.RetryAfter > cooldown {
		cooldown = ee.RetryAfter
	}
	h.cooldownUntil = now.Add(cooldown)
}

// faultMemory is how long a fault is held against an endpoint when the
// cooldown for the next one is computed.
const faultMemory = 10 * time.Minute

// ReportFault lets a caller that cross-checks results (the watcher) tell the
// endpoint that it returned data which turned out to be wrong or
// inconsistent — a block header another endpoint contradicts, logs that are
// missing from a block. The endpoint is set aside for a cooldown that
// doubles with every fault reported within faultMemory (2 s up to a minute),
// so that failover moves on to another endpoint. Unlike the cooldown after a
// failed request, this one is not ended by the next request that succeeds:
// an endpoint that lies answers promptly.
//
// Callers that must have a second endpoint still use one that is set aside
// if there is no other (see Pick).
func (e *Endpoint) ReportFault(reason string) {
	err := &Error{Kind: KindBadResponse, Endpoint: e.t.label, Message: cleanMessage(reason)}
	e.mu.Lock()
	defer e.mu.Unlock()
	h := &e.health
	now := e.opts.Now()
	if now.Sub(h.lastFaultAt) > faultMemory {
		h.faults = 0
	}
	h.faults++
	h.lastFaultAt = now
	h.unreachable = false // it answered: that is what the fault is about
	h.failures++
	h.lastErr = failureText(err)
	h.lastErrAt = now
	cooldown := cooldownBase << min(h.faults-1, 10)
	if cooldown > cooldownMax {
		cooldown = cooldownMax
	}
	if until := now.Add(cooldown); until.After(h.faultUntil) {
		h.faultUntil = until
	}
	if h.faultUntil.After(h.cooldownUntil) {
		h.cooldownUntil = h.faultUntil
	}
}

// failureText renders an error for the status snapshot without the endpoint
// label (the snapshot already names the endpoint).
func failureText(err error) string {
	var ee *Error
	if errors.As(err, &ee) {
		c := *ee
		c.Endpoint = ""
		s := c.Error()
		return s[len("evm: "):]
	}
	return cleanMessage(err.Error())
}

// sawBlock records a block height reported by the endpoint.
func (e *Endpoint) sawBlock(n uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n >= e.health.block {
		e.health.block = n
	}
	e.health.height = n
	e.health.blockAt = e.opts.Now()
}

// Unreachable reports whether err — the error of a call to an endpoint, or of
// Pick — says that an endpoint could not be reached at all: no HTTP response
// was obtained (DNS failure, connection refused or reset, TLS failure,
// timeout), or what answered was a gateway reporting a server-side failure in
// the endpoint's place (HTTP 5xx). Everything else is an answer: a JSON-RPC
// error, a rate limit (HTTP 429), a refusal (401, 403), malformed data.
func Unreachable(err error) bool {
	var ee *Error
	if !errors.As(err, &ee) {
		return false
	}
	switch ee.Kind {
	case KindTransport:
		return true
	case KindHTTP:
		return ee.HTTPStatus >= 500
	}
	return false
}
