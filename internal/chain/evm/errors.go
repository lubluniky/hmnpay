package evm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrorKind classifies a failed call. The classification drives failover
// (which errors mean "try another endpoint"), the adaptive log scanner (which
// errors mean "ask for less") and the setup wizard (what to tell the
// operator).
type ErrorKind int

const (
	// KindRPC is a JSON-RPC error returned by a working node that fits no
	// more specific kind, e.g. "execution reverted" or "invalid params".
	// Another endpoint would answer the same, so it is not retried.
	KindRPC ErrorKind = iota
	// KindTransport means no HTTP response was obtained: DNS failure,
	// connection refused, TLS failure, timeout.
	KindTransport
	// KindHTTP is a non-success HTTP status without a usable JSON-RPC body
	// (5xx from a gateway, an HTML error page, 403 from a WAF).
	KindHTTP
	// KindRateLimited means the provider throttled the request (HTTP 429,
	// or a JSON-RPC error saying so).
	KindRateLimited
	// KindBadResponse means the endpoint answered with something that is
	// not valid for the request: malformed JSON, a mismatched id, invalid
	// hex, a missing field, or data that contradicts the query (a log
	// outside the requested range). Treated as an unreliable endpoint.
	KindBadResponse
	// KindUnavailable means the provider refuses to serve this method or
	// this client: method not found/disabled, API key required, plan
	// restriction.
	KindUnavailable
	// KindRangeTooLarge means eth_getLogs was asked for more blocks than
	// the provider allows in one call.
	KindRangeTooLarge
	// KindTooManyResults means the query matched more logs (or produced a
	// larger response, or took longer) than the provider or this client
	// allows. Asking for less is the remedy.
	KindTooManyResults
	// KindRequestTooLarge means the provider rejected the request itself as
	// too big: too many topics or addresses in an eth_getLogs filter, or a
	// body over its size limit (HTTP 413). Splitting the address set is the
	// remedy.
	KindRequestTooLarge
	// KindLagging means the endpoint answered a query about blocks it has
	// not reached yet: the backend that served it is behind the requested
	// range. Another endpoint, or the same one a moment later, will do.
	KindLagging
)

// String returns a short, stable identifier for the kind.
func (k ErrorKind) String() string {
	switch k {
	case KindRPC:
		return "rpc_error"
	case KindTransport:
		return "transport"
	case KindHTTP:
		return "http"
	case KindRateLimited:
		return "rate_limited"
	case KindBadResponse:
		return "bad_response"
	case KindUnavailable:
		return "unavailable"
	case KindRangeTooLarge:
		return "range_too_large"
	case KindTooManyResults:
		return "too_many_results"
	case KindRequestTooLarge:
		return "request_too_large"
	case KindLagging:
		return "lagging"
	default:
		return "unknown"
	}
}

// Error is the error type returned by every call in this package (except for
// context cancellation by the caller, which is returned as the context's
// error). Its message never contains the endpoint's full URL — provider URLs
// routinely embed API keys — only the redacted label.
type Error struct {
	Kind ErrorKind
	// Endpoint is the redacted endpoint label (scheme and host).
	Endpoint string
	// Method is the JSON-RPC method, if the error belongs to one call.
	Method string
	// HTTPStatus is the HTTP status code, if a response was received.
	HTTPStatus int
	// Code is the JSON-RPC error code, if the node returned an error object.
	Code int
	// Message is the node's error message (truncated) or a description of
	// the local failure.
	Message string
	// RetryAfter is the delay requested by a Retry-After header, if any.
	RetryAfter time.Duration
	// Err is the underlying error, if any.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("evm: ")
	if e.Endpoint != "" {
		b.WriteString(e.Endpoint)
		b.WriteString(": ")
	}
	if e.Method != "" {
		b.WriteString(e.Method)
		b.WriteString(": ")
	}
	b.WriteString(e.Kind.String())
	if e.HTTPStatus != 0 && (e.Kind == KindHTTP || e.Kind == KindRateLimited) {
		fmt.Fprintf(&b, " (HTTP %d)", e.HTTPStatus)
	}
	if e.Code != 0 {
		fmt.Fprintf(&b, " (code %d)", e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	} else if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the ErrorKind of err and true if err is (or wraps) an
// *Error.
func KindOf(err error) (ErrorKind, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind, true
	}
	return 0, false
}

// IsKind reports whether err is (or wraps) an *Error of the given kind.
func IsKind(err error, kind ErrorKind) bool {
	k, ok := KindOf(err)
	return ok && k == kind
}

// ErrNotFound is returned by lookups (HeaderByNumber, TransactionReceipt)
// when the node answers null: it does not know the block or transaction
// (yet, or any more).
var ErrNotFound = errors.New("evm: not found")

// ErrNoEndpoint is returned by Client when no endpoint can be used: all of
// them report the wrong chain id, or none could be reached to verify it.
var ErrNoEndpoint = errors.New("evm: no usable RPC endpoint")

// retryElsewhere reports whether a failure is a property of the endpoint (so
// another endpoint may succeed) rather than of the request.
func retryElsewhere(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	kind, ok := KindOf(err)
	if !ok {
		return false
	}
	switch kind {
	case KindTransport, KindHTTP, KindRateLimited, KindBadResponse, KindUnavailable, KindLagging:
		return true
	default:
		return false
	}
}

const maxMessageLen = 300

// cleanMessage bounds and flattens a provider-supplied message so it is safe
// for logs and the dashboard.
func cleanMessage(s string) string {
	// The text comes from the endpoint and ends up in logs, errors and the
	// dashboard: no control characters (terminal escapes, NUL), no invalid
	// UTF-8, one line.
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxMessageLen {
		cut := maxMessageLen
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// transportMessage describes a net/http error without the request URL, which
// *url.Error would otherwise include verbatim (API key and all).
func transportMessage(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return cleanMessage(err.Error())
}

// classifyRPCError maps a JSON-RPC error object to a kind. Providers agree on
// neither codes nor wording, so this is a list of substrings observed in the
// wild (BSC public endpoints, Alchemy, Infura, QuickNode, Ankr, dRPC, NodeReal
// and friends). An unrecognised error stays KindRPC, which the log scanner
// treats as fatal for the call — it never guesses in the direction of
// skipping blocks.
func classifyRPCError(method string, code int, message string) ErrorKind {
	m := strings.ToLower(message)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(m, s) {
				return true
			}
		}
		return false
	}

	if has("request entity too large", "payload too large", "request too large", "body too large", "request body is too") {
		return KindRequestTooLarge
	}
	if method == "eth_getLogs" {
		switch {
		case has("beyond") && has("head"), has("after latest block", "after last accepted block", "block not found", "unknown block"):
			// Current BSC nodes refuse a range that ends above their head
			// ("block range extends beyond current head block: requested
			// N, head M") instead of silently answering for what they have.
			return KindLagging
		case has("topic", "addresses", "filter criteria") && has("too many", "exceed", "limit", "max", "at most"):
			return KindRequestTooLarge
		case has("range") && has("exceed", "limit", "too", "max", "over", "wide", "large", "up to"),
			has("too many blocks", "block span", "blocks at a time", "blocks per request"):
			return KindRangeTooLarge
		case has("more than") && has("result", "log"),
			has("too many results", "too many logs", "response size", "response too large",
				"response is too", "result set", "result size", "size exceeded", "exceeds max results",
				"query timeout", "timed out", "timeout", "took too long", "too much data"):
			return KindTooManyResults
		}
	}
	switch {
	case code == 429, code == -32029, code == -32097,
		has("rate limit", "rate-limit", "ratelimit", "too many requests", "throttl", "request limit",
			"requests per second", "request rate", "capacity", "quota", "credits"):
		return KindRateLimited
	case code == -32005 && has("limit exceeded") && method != "eth_getLogs":
		// EIP-1474 "limit exceeded": throttling. For eth_getLogs the bare
		// message stays unclassified (KindRPC) here on purpose: BSC's
		// public dataseed nodes answer *every* eth_getLogs with exactly
		// this, so there it means "not served". The log scanner decides:
		// on an endpoint that has served logs before it is a rate limit
		// (Endpoint.logsRefused); otherwise a probe tells.
		return KindRateLimited
	case code == -32601,
		has("method not found", "not supported", "not available", "unsupported", "not allowed",
			"not whitelisted", "disabled", "does not exist", "is not enabled", "forbidden",
			"unauthorized", "api key", "apikey", "authenticate", "paid", "upgrade", "subscription"):
		return KindUnavailable
	}
	return KindRPC
}

// headInMessage finds the node's own height in a "beyond current head" error
// ("... requested 134262128, head 134262118").
var headInMessage = regexp.MustCompile(`(?i)\bhead:?\s+(\d{1,18})\b`)

// laggingHead extracts the backend's height from a KindLagging message, or
// returns false.
func laggingHead(message string) (uint64, bool) {
	m := headInMessage.FindStringSubmatch(message)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	return n, err == nil
}

var decimalNumber = regexp.MustCompile(`\d[\d,_]*[kK]?`)

// rangeHint extracts the block-range limit a provider states in its error
// message ("limited to 0 - 50 blocks range", "must not exceed 25 blocks",
// "exceed maximum block range 5000", "max is 1k blocks"). It returns the
// largest number in the message that is at least 1 and below the span that
// just failed, or 0 if there is none. It is only a hint: the scanner falls
// back to halving if the hinted span fails too, and grows back if the hint
// was too pessimistic.
func rangeHint(message string, failedSpan uint64) uint64 {
	var best uint64
	// Hex literals are block numbers or hashes, never limits.
	for _, field := range strings.Fields(message) {
		if strings.Contains(strings.ToLower(field), "0x") {
			message = strings.Replace(message, field, " ", 1)
		}
	}
	for _, match := range decimalNumber.FindAllString(message, -1) {
		s := strings.TrimSpace(match)
		mult := uint64(1)
		if strings.HasSuffix(strings.ToLower(s), "k") {
			mult = 1000
			s = strings.TrimSpace(s[:len(s)-1])
		}
		s = strings.NewReplacer(",", "", "_", "").Replace(s)
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil || v == 0 || v > 1<<40 {
			continue
		}
		v *= mult
		if v < failedSpan && v > best {
			best = v
		}
	}
	return best
}
