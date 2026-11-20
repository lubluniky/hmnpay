package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/strictjson"

	"github.com/lubluniky/hmnpay/core/endpointid"
)

// Transport defaults.
const (
	// DefaultTimeout bounds one HTTP round trip.
	DefaultTimeout = 10 * time.Second
	// DefaultMaxResponseBytes bounds the size of one HTTP response body. A
	// filtered eth_getLogs answer for a payment gateway is a few kilobytes;
	// a header with transaction hashes is tens of kilobytes. 8 MiB leaves
	// two orders of magnitude of headroom while keeping a hostile or broken
	// endpoint from exhausting memory.
	DefaultMaxResponseBytes = 8 << 20

	userAgent = "hmnpay-evm/1"
)

// transport is JSON-RPC 2.0 over HTTP POST for one URL.
type transport struct {
	url     string // full URL, may contain credentials; never logged
	label   string // redacted form for errors, logs and status
	http    *http.Client
	timeout time.Duration
	maxBody int64
	quiet   bool // do not quote non-JSON response bodies in errors
	nextID  atomic.Uint64
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Everything an endpoint sends is decoded strictly (see strictjson): a member
// counts only under its exact name and only once. encoding/json would also
// read "RESULT" as the result and let the second of two "result" members
// win, so that what this client makes of a response would depend on the order
// of its members and not only on what it says. No JSON-RPC server produces
// such a response — the public BSC endpoints all answer with exactly "id",
// "jsonrpc" and "result" or "error", and with the member names of the
// execution API in a block, a log and a receipt — so one that does is
// treated like any other malformed answer. Members this client does not read
// are ignored: nodes add new ones with every release ("blockTimestamp" in
// logs, for one).

// UnmarshalJSON implements json.Unmarshaler.
func (r *rpcResponse) UnmarshalJSON(data []byte) error {
	return strictjson.Object(data, strictjson.Fields{
		{Name: "jsonrpc", Into: &r.JSONRPC},
		{Name: "id", Into: &r.ID},
		{Name: "result", Into: &r.Result},
		{Name: "error", Into: &r.Error},
	}, strictjson.IgnoreUnknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (e *rpcError) UnmarshalJSON(data []byte) error {
	return strictjson.Object(data, strictjson.Fields{
		{Name: "code", Into: &e.Code},
		{Name: "message", Into: &e.Message},
	}, strictjson.IgnoreUnknown)
}

// parseEndpointURL validates an operator-supplied endpoint URL and returns it
// together with its redacted label.
func parseEndpointURL(raw string) (full, label string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// url.Parse errors quote the input; do not propagate them.
		return "", "", errors.New("evm: endpoint URL is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", errors.New("evm: endpoint URL must use http or https")
	}
	if u.Host == "" {
		return "", "", errors.New("evm: endpoint URL has no host")
	}
	return u.String(), RedactURL(u.String()), nil
}

// Identity returns what makes two endpoint URLs one endpoint: the canonical
// scheme, host and port (endpointid.Identity, the one definition shared
// with the core's settings validation).
func Identity(raw string) string { return endpointid.Identity(raw) }

// RedactURL returns a form of an endpoint URL that is safe to log and
// display, and that is the endpoint's label — the name the watcher remembers
// it by: its identity (Identity), scheme, canonical host and port only.
// Provider URLs commonly carry the API key in the path
// (https://host/v1/KEY), the query (?apikey=KEY) or the user info, so all of
// those are dropped — and a change of key is not a change of endpoint.
func RedactURL(raw string) string { return endpointid.Identity(raw) }

// call performs one JSON-RPC call and returns the raw result.
func (t *transport) call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	req := rpcRequest{JSONRPC: "2.0", ID: t.nextID.Add(1), Method: method, Params: params}
	body, status, retryAfter, err := t.post(ctx, method, req)
	if err != nil {
		return nil, err
	}
	var resp rpcResponse
	if jerr := resp.UnmarshalJSON(body); jerr != nil || (resp.Error == nil && resp.Result == nil && len(resp.ID) == 0) {
		return nil, t.httpOrBadResponse(method, status, retryAfter, body)
	}
	return t.result(method, req.ID, status, retryAfter, &resp)
}

// batchElem is one call of a batch. After batch returns nil, each element has
// either Result or Err set.
type batchElem struct {
	Method string
	Params []any
	Result json.RawMessage
	Err    error
}

// batch performs several calls in one HTTP request. A non-nil return value
// means the batch as a whole failed (transport, HTTP status, or a single
// error object in place of the array — the way providers reject batches).
// Responses are matched to requests by id, never by position.
func (t *transport) batch(ctx context.Context, elems []batchElem) error {
	if len(elems) == 0 {
		return nil
	}
	reqs := make([]rpcRequest, len(elems))
	index := make(map[uint64]int, len(elems))
	for i := range elems {
		params := elems[i].Params
		if params == nil {
			params = []any{}
		}
		reqs[i] = rpcRequest{JSONRPC: "2.0", ID: t.nextID.Add(1), Method: elems[i].Method, Params: params}
		index[reqs[i].ID] = i
	}
	const method = "batch"
	body, status, retryAfter, err := t.post(ctx, method, reqs)
	if err != nil {
		return err
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '[' {
		// One object instead of an array: an error for the whole batch.
		var single rpcResponse
		if jerr := single.UnmarshalJSON(body); jerr == nil && single.Error != nil {
			return t.rpcFailure(method, status, retryAfter, single.Error)
		}
		return t.httpOrBadResponse(method, status, retryAfter, body)
	}
	var resps []rpcResponse
	if jerr := json.Unmarshal(body, &resps); jerr != nil {
		return t.httpOrBadResponse(method, status, retryAfter, body)
	}
	if status == http.StatusTooManyRequests {
		return &Error{Kind: KindRateLimited, Endpoint: t.label, Method: method, HTTPStatus: status, RetryAfter: retryAfter}
	}
	seen := make([]bool, len(elems))
	for i := range resps {
		id, perr := strconv.ParseUint(string(bytes.TrimSpace(resps[i].ID)), 10, 64)
		if perr != nil {
			continue // null or foreign id: cannot be attributed
		}
		pos, ok := index[id]
		if !ok || seen[pos] {
			continue
		}
		seen[pos] = true
		elems[pos].Result, elems[pos].Err = t.result(elems[pos].Method, id, status, retryAfter, &resps[i])
	}
	answered := false
	for i := range elems {
		if seen[i] {
			answered = true
			continue
		}
		elems[i].Err = &Error{Kind: KindBadResponse, Endpoint: t.label, Method: elems[i].Method,
			HTTPStatus: status, Message: "no response for this call in the batch reply"}
	}
	if !answered {
		// Nothing in the reply belongs to a request. If it carries an
		// error object (with a null id), that error is the provider's
		// answer to the batch as a whole — BSC's dataseed nodes reject any
		// batch containing eth_getLogs this way.
		for i := range resps {
			if resps[i].Error != nil {
				return t.rpcFailure(method, status, retryAfter, resps[i].Error)
			}
		}
	}
	return nil
}

// result validates one response object against its request.
func (t *transport) result(method string, wantID uint64, status int, retryAfter time.Duration, resp *rpcResponse) (json.RawMessage, error) {
	if resp.Error != nil {
		return nil, t.rpcFailure(method, status, retryAfter, resp.Error)
	}
	if status == http.StatusTooManyRequests {
		return nil, &Error{Kind: KindRateLimited, Endpoint: t.label, Method: method, HTTPStatus: status, RetryAfter: retryAfter}
	}
	if status < 200 || status > 299 {
		return nil, &Error{Kind: KindHTTP, Endpoint: t.label, Method: method, HTTPStatus: status,
			Message: http.StatusText(status)}
	}
	// A response to a different request must never be taken for ours: a
	// mixed-up proxy would otherwise feed us someone else's data.
	if id, err := strconv.ParseUint(string(bytes.TrimSpace(resp.ID)), 10, 64); err != nil || id != wantID {
		return nil, &Error{Kind: KindBadResponse, Endpoint: t.label, Method: method, HTTPStatus: status,
			Message: "response id does not match request id"}
	}
	if resp.Result == nil {
		return nil, &Error{Kind: KindBadResponse, Endpoint: t.label, Method: method, HTTPStatus: status,
			Message: "response has neither result nor error"}
	}
	// A result is taken only from a JSON-RPC 2.0 response object, which
	// says that it is one. (An error is taken from anything that carries
	// one: gateways reject requests in all sorts of shapes, and a refusal
	// cannot make the client believe anything about the chain.)
	if resp.JSONRPC != "2.0" {
		return nil, &Error{Kind: KindBadResponse, Endpoint: t.label, Method: method, HTTPStatus: status,
			Message: `response is not a JSON-RPC 2.0 object (no "jsonrpc":"2.0")`}
	}
	return resp.Result, nil
}

func (t *transport) rpcFailure(method string, status int, retryAfter time.Duration, e *rpcError) error {
	kind := classifyRPCError(method, e.Code, e.Message)
	if status == http.StatusTooManyRequests {
		kind = KindRateLimited
	}
	return &Error{Kind: kind, Endpoint: t.label, Method: method, HTTPStatus: status, Code: e.Code,
		Message: cleanMessage(e.Message), RetryAfter: retryAfter}
}

// httpOrBadResponse builds the error for a body that is not JSON-RPC.
func (t *transport) httpOrBadResponse(method string, status int, retryAfter time.Duration, body []byte) error {
	switch {
	case status == http.StatusTooManyRequests:
		return &Error{Kind: KindRateLimited, Endpoint: t.label, Method: method, HTTPStatus: status, RetryAfter: retryAfter}
	case status == http.StatusRequestEntityTooLarge:
		return &Error{Kind: KindRequestTooLarge, Endpoint: t.label, Method: method, HTTPStatus: status,
			Message: "request body rejected as too large"}
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusPaymentRequired:
		return &Error{Kind: KindUnavailable, Endpoint: t.label, Method: method, HTTPStatus: status,
			Message: strings.TrimSuffix("access denied: "+t.excerpt(body), ": ")}
	case status < 200 || status > 299:
		return &Error{Kind: KindHTTP, Endpoint: t.label, Method: method, HTTPStatus: status,
			Message: t.excerpt(body)}
	default:
		return &Error{Kind: KindBadResponse, Endpoint: t.label, Method: method, HTTPStatus: status,
			Message: "response is not valid JSON-RPC"}
	}
}

// excerpt returns bodyExcerpt(body), or the status text alone in quiet mode.
func (t *transport) excerpt(body []byte) string {
	if t.quiet {
		return ""
	}
	return bodyExcerpt(body)
}

// bodyExcerpt returns a short, single-line excerpt of a non-JSON body (often
// an HTML error page) for diagnostics.
func bodyExcerpt(body []byte) string {
	const max = 120
	if len(body) > max {
		body = body[:max]
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, string(body))
	return cleanMessage(s)
}

// post sends one request body and returns the (bounded) response body.
func (t *transport) post(ctx context.Context, method string, payload any) (body []byte, status int, retryAfter time.Duration, err error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("evm: encode %s request: %w", method, err)
	}
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, t.url, bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, 0, &Error{Kind: KindTransport, Endpoint: t.label, Method: method, Message: "cannot build request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := t.http.Do(req)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, 0, 0, cerr // the caller gave up; not the endpoint's fault
		}
		return nil, 0, 0, &Error{Kind: KindTransport, Endpoint: t.label, Method: method, Message: transportMessage(err)}
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(io.LimitReader(resp.Body, t.maxBody+1))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, 0, 0, cerr
		}
		return nil, 0, 0, &Error{Kind: KindTransport, Endpoint: t.label, Method: method, HTTPStatus: resp.StatusCode,
			Message: "reading response: " + transportMessage(err)}
	}
	if int64(len(body)) > t.maxBody {
		return nil, 0, 0, &Error{Kind: KindTooManyResults, Endpoint: t.label, Method: method, HTTPStatus: resp.StatusCode,
			Message: fmt.Sprintf("response larger than %d bytes", t.maxBody)}
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, perr := strconv.Atoi(strings.TrimSpace(ra)); perr == nil && secs > 0 {
			retryAfter = min(time.Duration(secs)*time.Second, 10*time.Minute)
		}
	}
	return body, resp.StatusCode, retryAfter, nil
}
