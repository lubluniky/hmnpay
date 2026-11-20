package evm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/strictjson"
)

// EndpointOptions tunes a single endpoint. The zero value selects the
// defaults.
type EndpointOptions struct {
	// HTTPClient is the client used for requests. If nil, a client with
	// conservative connection limits is created. Timeouts are applied per
	// call through the context, so the client needs no Timeout of its own.
	HTTPClient *http.Client
	// Timeout bounds one HTTP round trip. Default: DefaultTimeout.
	Timeout time.Duration
	// MaxResponseBytes bounds one response body. Default:
	// DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// OmitResponseExcerpts keeps excerpts of non-JSON response bodies (the
	// first characters of an HTML error page, say) out of error messages.
	// Set it when error messages reach someone who may not be allowed to
	// see what the probed host answers — the setup wizard's endpoint probe
	// runs before any operator account exists.
	OmitResponseExcerpts bool
	// Now is the clock used for health bookkeeping. Default: time.Now.
	Now func() time.Time
	// Logs tunes the adaptive eth_getLogs scanner.
	Logs LogOptions
}

func (o EndpointOptions) withDefaults() EndpointOptions {
	if o.HTTPClient == nil {
		o.HTTPClient = newHTTPClient()
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	o.Logs = o.Logs.withDefaults()
	return o
}

// newHTTPClient returns an HTTP client suited to talking to a handful of RPC
// hosts: keep-alives on, few idle connections, bounded dial/TLS/header
// timeouts, and no following of redirects (a JSON-RPC endpoint that redirects
// a POST is misconfigured at best; following it would send requests, and
// possibly credentials, to a host the operator did not configure).
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 16
	tr.MaxIdleConnsPerHost = 4
	tr.IdleConnTimeout = 60 * time.Second
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Endpoint is one JSON-RPC URL. It is safe for concurrent use.
//
// Every call updates the endpoint's health record (see Health), which the
// Client uses for failover. An Endpoint can also be used on its own, without
// a Client.
type Endpoint struct {
	t    *transport
	opts EndpointOptions

	mu          sync.Mutex
	health      health
	logState    logState
	noBatch     bool      // the provider rejected a batch request
	noLogBatch  bool      // the provider rejected a batch containing eth_getLogs
	noFinalized time.Time // until when not to ask for the "finalized" tag
}

// NewEndpoint creates an Endpoint for rawURL, which must be an http or https
// URL. No request is made.
func NewEndpoint(rawURL string, opts EndpointOptions) (*Endpoint, error) {
	full, label, err := parseEndpointURL(rawURL)
	if err != nil {
		return nil, err
	}
	opts = opts.withDefaults()
	return &Endpoint{
		t: &transport{
			url:     full,
			label:   label,
			http:    opts.HTTPClient,
			timeout: opts.Timeout,
			maxBody: opts.MaxResponseBytes,
			quiet:   opts.OmitResponseExcerpts,
		},
		opts:     opts,
		logState: newLogState(opts.Logs),
	}, nil
}

// Label returns the redacted URL of the endpoint (see RedactURL). It is the
// identifier used in errors, logs and status snapshots.
func (e *Endpoint) Label() string { return e.t.label }

// URL returns the full URL as configured. It may contain an API key: do not
// log it.
func (e *Endpoint) URL() string { return e.t.url }

// call performs one JSON-RPC call, records the outcome in the health record
// and decodes the result into out (if out is non-nil).
func (e *Endpoint) call(ctx context.Context, out any, method string, params ...any) error {
	start := time.Now()
	raw, err := e.t.call(ctx, method, params...)
	e.observe(ctx, method, time.Since(start), err)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return e.badResponse(method, err)
	}
	return nil
}

// badResponse records and returns a KindBadResponse error for a result that
// could not be decoded or contradicts the request.
func (e *Endpoint) badResponse(method string, cause error) error {
	err := &Error{Kind: KindBadResponse, Endpoint: e.t.label, Method: method, Message: cleanMessage(cause.Error())}
	e.recordFailure(err)
	return err
}

// ChainID returns the chain id reported by eth_chainId.
func (e *Endpoint) ChainID(ctx context.Context) (uint64, error) {
	var s string
	if err := e.call(ctx, &s, "eth_chainId"); err != nil {
		return 0, err
	}
	id, err := decodeUint64(s)
	if err != nil {
		return 0, e.badResponse("eth_chainId", err)
	}
	return id, nil
}

// BlockNumber returns the number of the most recent block known to the node.
func (e *Endpoint) BlockNumber(ctx context.Context) (uint64, error) {
	var s string
	if err := e.call(ctx, &s, "eth_blockNumber"); err != nil {
		return 0, err
	}
	n, err := decodeUint64(s)
	if err != nil {
		return 0, e.badResponse("eth_blockNumber", err)
	}
	e.sawBlock(n)
	return n, nil
}

// Block tags accepted by HeaderByTag.
const (
	TagLatest    = "latest"
	TagSafe      = "safe"
	TagFinalized = "finalized"
)

// rawHeader is the JSON shape of a block (without transaction bodies).
type rawHeader struct {
	Number     *string `json:"number"`
	Hash       *string `json:"hash"`
	ParentHash *string `json:"parentHash"`
	Timestamp  *string `json:"timestamp"`
}

// UnmarshalJSON implements json.Unmarshaler: strictly, see rpcResponse.
func (rh *rawHeader) UnmarshalJSON(data []byte) error {
	return strictjson.Object(data, strictjson.Fields{
		{Name: "number", Into: &rh.Number},
		{Name: "hash", Into: &rh.Hash},
		{Name: "parentHash", Into: &rh.ParentHash},
		{Name: "timestamp", Into: &rh.Timestamp},
	}, strictjson.IgnoreUnknown)
}

func decodeHeader(raw json.RawMessage) (*Header, error) {
	if isJSONNull(raw) {
		return nil, ErrNotFound
	}
	var rh rawHeader
	if err := rh.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	if rh.Number == nil || rh.Hash == nil || rh.ParentHash == nil || rh.Timestamp == nil {
		return nil, errors.New("block is missing number, hash, parentHash or timestamp")
	}
	var h Header
	var err error
	if h.Number, err = decodeUint64(*rh.Number); err != nil {
		return nil, fmt.Errorf("block number: %w", err)
	}
	if h.Hash, err = decodeHash(*rh.Hash); err != nil {
		return nil, fmt.Errorf("block hash: %w", err)
	}
	if h.ParentHash, err = decodeHash(*rh.ParentHash); err != nil {
		return nil, fmt.Errorf("parent hash: %w", err)
	}
	if h.Time, err = decodeUint64(*rh.Timestamp); err != nil {
		return nil, fmt.Errorf("timestamp: %w", err)
	}
	if h.Hash.IsZero() {
		return nil, errors.New("block hash is zero")
	}
	return &h, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return string(raw) == "null"
}

// HeaderByNumber returns the header of the canonical block with the given
// number, or ErrNotFound if the node does not know such a block (yet). The
// returned header is checked to carry the requested number.
func (e *Endpoint) HeaderByNumber(ctx context.Context, number uint64) (*Header, error) {
	var raw json.RawMessage
	if err := e.call(ctx, &raw, "eth_getBlockByNumber", encodeUint64(number), false); err != nil {
		return nil, err
	}
	h, err := decodeHeader(raw)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, e.badResponse("eth_getBlockByNumber", err)
	}
	if h.Number != number {
		return nil, e.badResponse("eth_getBlockByNumber", fmt.Errorf("asked for block %d, got block %d", number, h.Number))
	}
	return h, nil
}

// HeaderByTag returns the header for a block tag: TagLatest, TagSafe or
// TagFinalized. It returns ErrNotFound if the node answers null.
func (e *Endpoint) HeaderByTag(ctx context.Context, tag string) (*Header, error) {
	var raw json.RawMessage
	if err := e.call(ctx, &raw, "eth_getBlockByNumber", tag, false); err != nil {
		return nil, err
	}
	h, err := decodeHeader(raw)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, e.badResponse("eth_getBlockByNumber", err)
	}
	if tag == TagLatest {
		e.sawBlock(h.Number)
	}
	return h, nil
}

// HeadRequest describes what Head should fetch besides the latest header.
type HeadRequest struct {
	// Finalized also requests the header of the "finalized" block tag.
	// Nodes that do not support the tag are remembered and not asked
	// again for a while.
	Finalized bool
	// Numbers lists block numbers whose canonical headers are wanted too
	// (the watcher uses this to check that blocks it has already seen are
	// still canonical).
	Numbers []uint64
}

// HeadResult is the answer to Head.
type HeadResult struct {
	// Latest is the node's most recent block.
	Latest Header
	// Finalized is the latest finalized block, or nil if it was not
	// requested or the node does not report one.
	Finalized *Header
	// ByNumber holds the canonical header for each requested number that
	// the node knows. A missing entry means the node answered null.
	ByNumber map[uint64]Header
}

// Head fetches the latest header and, in the same HTTP request (a JSON-RPC
// batch) where the provider allows it, the finalized header and a set of
// headers by number. One round trip gives the caller a view of the chain
// that is as close to a single point in time as an RPC endpoint can offer.
//
// If the provider rejects batch requests the calls are made one by one, and
// the endpoint remembers not to batch again.
func (e *Endpoint) Head(ctx context.Context, req HeadRequest) (*HeadResult, error) {
	wantFinalized := req.Finalized && e.finalizedSupported()

	elems := make([]batchElem, 0, 2+len(req.Numbers))
	elems = append(elems, batchElem{Method: "eth_getBlockByNumber", Params: []any{TagLatest, false}})
	if wantFinalized {
		elems = append(elems, batchElem{Method: "eth_getBlockByNumber", Params: []any{TagFinalized, false}})
	}
	for _, n := range req.Numbers {
		elems = append(elems, batchElem{Method: "eth_getBlockByNumber", Params: []any{encodeUint64(n), false}})
	}
	if err := e.batch(ctx, elems); err != nil {
		return nil, err
	}

	res := &HeadResult{ByNumber: make(map[uint64]Header, len(req.Numbers))}
	if elems[0].Err != nil {
		return nil, elems[0].Err
	}
	latest, err := decodeHeader(elems[0].Result)
	if err != nil {
		return nil, e.badResponse("eth_getBlockByNumber", fmt.Errorf("latest block: %w", err))
	}
	res.Latest = *latest
	e.sawBlock(latest.Number)

	next := 1
	if wantFinalized {
		el := elems[next]
		next++
		switch {
		case el.Err != nil:
			// Typically "finalized block not found" or an unknown tag on
			// chains or nodes without finality. Not fatal: finality is
			// extra information.
			if errors.Is(el.Err, context.Canceled) || errors.Is(el.Err, context.DeadlineExceeded) {
				return nil, el.Err
			}
			if retryElsewhere(el.Err) && !IsKind(el.Err, KindUnavailable) {
				return nil, el.Err
			}
			e.markNoFinalized()
		default:
			fin, derr := decodeHeader(el.Result)
			switch {
			case errors.Is(derr, ErrNotFound):
				e.markNoFinalized()
			case derr != nil:
				return nil, e.badResponse("eth_getBlockByNumber", fmt.Errorf("finalized block: %w", derr))
			case fin.Number > latest.Number:
				return nil, e.badResponse("eth_getBlockByNumber",
					fmt.Errorf("finalized block %d is above latest block %d", fin.Number, latest.Number))
			default:
				res.Finalized = fin
			}
		}
	}
	for i, n := range req.Numbers {
		el := elems[next+i]
		if el.Err != nil {
			return nil, el.Err
		}
		h, derr := decodeHeader(el.Result)
		if errors.Is(derr, ErrNotFound) {
			continue
		}
		if derr != nil {
			return nil, e.badResponse("eth_getBlockByNumber", derr)
		}
		if h.Number != n {
			return nil, e.badResponse("eth_getBlockByNumber", fmt.Errorf("asked for block %d, got block %d", n, h.Number))
		}
		res.ByNumber[n] = *h
	}
	return res, nil
}

// finalizedRetryInterval is how long an endpoint that failed the "finalized"
// tag is not asked for it again. Short: a transfer is final only in a block
// two endpoints report as finalized, so while one is not asked, nothing new
// becomes final.
const finalizedRetryInterval = time.Minute

func (e *Endpoint) finalizedSupported() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.opts.Now().Before(e.noFinalized)
}

func (e *Endpoint) markNoFinalized() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.noFinalized = e.opts.Now().Add(finalizedRetryInterval)
}

// batch runs elems as one JSON-RPC batch, or one by one if the provider does
// not accept batches. On a nil return every element has Result or Err set.
func (e *Endpoint) batch(ctx context.Context, elems []batchElem) error {
	e.mu.Lock()
	sequential := e.noBatch || len(elems) == 1
	e.mu.Unlock()

	if !sequential {
		start := time.Now()
		err := e.t.batch(ctx, elems)
		if err == nil {
			// The batch as a whole succeeded; judge health by its members.
			var firstErr error
			for i := range elems {
				if elems[i].Err != nil && firstErr == nil {
					firstErr = elems[i].Err
				}
			}
			e.observe(ctx, "batch", time.Since(start), firstErr)
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		kind, _ := KindOf(err)
		if kind == KindTransport || kind == KindRateLimited || kind == KindHTTP {
			e.observe(ctx, "batch", time.Since(start), err)
			return err
		}
		// The provider answered but would not take a batch (an error
		// object, "batch not supported", a non-array body). Fall back to
		// single calls and remember.
		e.mu.Lock()
		e.noBatch = true
		e.mu.Unlock()
	}
	for i := range elems {
		start := time.Now()
		elems[i].Result, elems[i].Err = e.t.call(ctx, elems[i].Method, elems[i].Params...)
		e.observe(ctx, elems[i].Method, time.Since(start), elems[i].Err)
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if elems[i].Err != nil && retryElsewhere(elems[i].Err) && i == 0 {
			// The first call already shows the endpoint is not usable;
			// do not burn the remaining timeouts.
			return elems[i].Err
		}
	}
	return nil
}

// rawLog is the JSON shape of a log.
type rawLog struct {
	Address          *string  `json:"address"`
	Topics           []string `json:"topics"`
	Data             *string  `json:"data"`
	BlockNumber      *string  `json:"blockNumber"`
	BlockHash        *string  `json:"blockHash"`
	TransactionHash  *string  `json:"transactionHash"`
	TransactionIndex *string  `json:"transactionIndex"`
	LogIndex         *string  `json:"logIndex"`
	Removed          bool     `json:"removed"`
}

// UnmarshalJSON implements json.Unmarshaler: strictly, see rpcResponse.
func (rl *rawLog) UnmarshalJSON(data []byte) error {
	return strictjson.Object(data, strictjson.Fields{
		{Name: "address", Into: &rl.Address},
		{Name: "topics", Into: &rl.Topics},
		{Name: "data", Into: &rl.Data},
		{Name: "blockNumber", Into: &rl.BlockNumber},
		{Name: "blockHash", Into: &rl.BlockHash},
		{Name: "transactionHash", Into: &rl.TransactionHash},
		{Name: "transactionIndex", Into: &rl.TransactionIndex},
		{Name: "logIndex", Into: &rl.LogIndex},
		{Name: "removed", Into: &rl.Removed},
	}, strictjson.IgnoreUnknown)
}

// maxLogData bounds the data field of a single log. ERC-20 Transfer data is
// 32 bytes; this only keeps absurd payloads out.
const maxLogData = 64 << 10

func (rl *rawLog) decode() (Log, error) {
	var l Log
	var err error
	if rl.Address == nil || rl.Data == nil || rl.BlockNumber == nil || rl.BlockHash == nil ||
		rl.TransactionHash == nil || rl.LogIndex == nil {
		// Pending logs have null block fields; they are of no use here.
		return l, errors.New("log is missing address, data, blockNumber, blockHash, transactionHash or logIndex")
	}
	if l.Address, err = decodeAddress(*rl.Address); err != nil {
		return l, fmt.Errorf("log address: %w", err)
	}
	if len(rl.Topics) > 4 {
		return l, errors.New("log has more than 4 topics")
	}
	l.Topics = make([]Hash, len(rl.Topics))
	for i, t := range rl.Topics {
		if l.Topics[i], err = decodeHash(t); err != nil {
			return l, fmt.Errorf("log topic %d: %w", i, err)
		}
	}
	if l.Data, err = decodeData(*rl.Data, maxLogData); err != nil {
		return l, fmt.Errorf("log data: %w", err)
	}
	if l.BlockNumber, err = decodeUint64(*rl.BlockNumber); err != nil {
		return l, fmt.Errorf("log block number: %w", err)
	}
	if l.BlockHash, err = decodeHash(*rl.BlockHash); err != nil {
		return l, fmt.Errorf("log block hash: %w", err)
	}
	if l.TxHash, err = decodeHash(*rl.TransactionHash); err != nil {
		return l, fmt.Errorf("log transaction hash: %w", err)
	}
	if l.LogIndex, err = decodeUint64(*rl.LogIndex); err != nil {
		return l, fmt.Errorf("log index: %w", err)
	}
	if rl.TransactionIndex != nil {
		if l.TxIndex, err = decodeUint64(*rl.TransactionIndex); err != nil {
			return l, fmt.Errorf("log transaction index: %w", err)
		}
	}
	if l.BlockHash.IsZero() || l.TxHash.IsZero() {
		return l, errors.New("log has a zero block or transaction hash")
	}
	l.Removed = rl.Removed
	return l, nil
}

// rawReceipt is the JSON shape of a transaction receipt.
type rawReceipt struct {
	TransactionHash *string  `json:"transactionHash"`
	BlockHash       *string  `json:"blockHash"`
	BlockNumber     *string  `json:"blockNumber"`
	Status          *string  `json:"status"`
	Logs            []rawLog `json:"logs"`
}

// UnmarshalJSON implements json.Unmarshaler: strictly, see rpcResponse.
func (rr *rawReceipt) UnmarshalJSON(data []byte) error {
	return strictjson.Object(data, strictjson.Fields{
		{Name: "transactionHash", Into: &rr.TransactionHash},
		{Name: "blockHash", Into: &rr.BlockHash},
		{Name: "blockNumber", Into: &rr.BlockNumber},
		{Name: "status", Into: &rr.Status},
		{Name: "logs", Into: &rr.Logs},
	}, strictjson.IgnoreUnknown)
}

func decodeReceipt(raw json.RawMessage, want Hash) (*Receipt, error) {
	if isJSONNull(raw) {
		return nil, ErrNotFound
	}
	var rr rawReceipt
	if err := rr.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	if rr.TransactionHash == nil || rr.BlockHash == nil || rr.BlockNumber == nil || rr.Status == nil {
		return nil, errors.New("receipt is missing transactionHash, blockHash, blockNumber or status")
	}
	var r Receipt
	var err error
	if r.TxHash, err = decodeHash(*rr.TransactionHash); err != nil {
		return nil, fmt.Errorf("receipt transaction hash: %w", err)
	}
	if r.TxHash != want {
		return nil, errors.New("receipt is for a different transaction")
	}
	if r.BlockHash, err = decodeHash(*rr.BlockHash); err != nil {
		return nil, fmt.Errorf("receipt block hash: %w", err)
	}
	if r.BlockNumber, err = decodeUint64(*rr.BlockNumber); err != nil {
		return nil, fmt.Errorf("receipt block number: %w", err)
	}
	if r.Status, err = decodeUint64(*rr.Status); err != nil {
		return nil, fmt.Errorf("receipt status: %w", err)
	}
	if r.Status > 1 {
		return nil, errors.New("receipt status is neither 0 nor 1")
	}
	r.Logs = make([]Log, len(rr.Logs))
	for i := range rr.Logs {
		if r.Logs[i], err = rr.Logs[i].decode(); err != nil {
			return nil, fmt.Errorf("receipt log %d: %w", i, err)
		}
		if r.Logs[i].BlockHash != r.BlockHash || r.Logs[i].TxHash != r.TxHash || r.Logs[i].BlockNumber != r.BlockNumber {
			return nil, fmt.Errorf("receipt log %d belongs to a different block or transaction", i)
		}
	}
	return &r, nil
}

// TransactionReceipt returns the receipt of a mined transaction, or
// ErrNotFound if the node does not know the transaction as mined on its
// canonical chain. The receipt and all its logs are checked to refer to the
// requested transaction and to one block.
func (e *Endpoint) TransactionReceipt(ctx context.Context, txHash Hash) (*Receipt, error) {
	var raw json.RawMessage
	if err := e.call(ctx, &raw, "eth_getTransactionReceipt", txHash.String()); err != nil {
		return nil, err
	}
	r, err := decodeReceipt(raw, txHash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, e.badResponse("eth_getTransactionReceipt", err)
	}
	return r, nil
}

// maxBatchReceipts is the number of receipts requested per HTTP request.
// Public providers cap batch sizes (often at 10–100 items) and count every
// item against rate limits.
const maxBatchReceipts = 10

// TransactionReceipts returns the receipts for several transactions. The
// result has one entry per input hash, in order; an entry is nil if the node
// does not know the transaction. Requests are batched where the provider
// allows it.
func (e *Endpoint) TransactionReceipts(ctx context.Context, txHashes []Hash) ([]*Receipt, error) {
	out := make([]*Receipt, len(txHashes))
	for start := 0; start < len(txHashes); start += maxBatchReceipts {
		end := min(start+maxBatchReceipts, len(txHashes))
		elems := make([]batchElem, end-start)
		for i := range elems {
			elems[i] = batchElem{Method: "eth_getTransactionReceipt", Params: []any{txHashes[start+i].String()}}
		}
		if err := e.batch(ctx, elems); err != nil {
			return nil, err
		}
		for i := range elems {
			if elems[i].Err != nil {
				return nil, elems[i].Err
			}
			r, err := decodeReceipt(elems[i].Result, txHashes[start+i])
			switch {
			case errors.Is(err, ErrNotFound):
			case err != nil:
				return nil, e.badResponse("eth_getTransactionReceipt", err)
			default:
				out[start+i] = r
			}
		}
	}
	return out, nil
}

// maxCallData bounds the return data of eth_call and eth_getCode (the EVM
// contract size limit is 24 KiB; ERC-20 view functions return a few words).
const maxCallData = 1 << 20

// Call executes a read-only contract call (eth_call) against the latest
// block and returns the raw return data.
func (e *Endpoint) Call(ctx context.Context, to ethaddr.Address, data []byte) ([]byte, error) {
	var s string
	arg := map[string]string{"to": to.Hex(), "data": encodeData(data)}
	if err := e.call(ctx, &s, "eth_call", arg, TagLatest); err != nil {
		return nil, err
	}
	out, err := decodeData(s, maxCallData)
	if err != nil {
		return nil, e.badResponse("eth_call", err)
	}
	return out, nil
}

// Code returns the contract bytecode at addr in the latest block; it is empty
// for an address without code.
func (e *Endpoint) Code(ctx context.Context, addr ethaddr.Address) ([]byte, error) {
	var s string
	if err := e.call(ctx, &s, "eth_getCode", addr.Hex(), TagLatest); err != nil {
		return nil, err
	}
	out, err := decodeData(s, maxCallData)
	if err != nil {
		return nil, e.badResponse("eth_getCode", err)
	}
	return out, nil
}

// LogFilter is an eth_getLogs query over a block range.
type LogFilter struct {
	FromBlock uint64
	ToBlock   uint64
	// Address restricts the emitting contract.
	Address ethaddr.Address
	// Topics is the positional topic filter: each position is a list of
	// alternatives (OR); an empty list matches anything.
	Topics [][]Hash
}

// Logs performs a single eth_getLogs query exactly as specified, without any
// chunking or retries, and decodes the result strictly. Most callers want
// TransferLogs instead.
//
// The outcome of this call is not recorded in the general health record when
// the node itself rejects it (range limits, method disabled): such answers
// say something about eth_getLogs on this provider, not about whether the
// endpoint is alive.
func (e *Endpoint) Logs(ctx context.Context, f LogFilter) ([]Log, error) {
	logs, _, _, err := e.logs(ctx, f)
	return logs, err
}

// logs is Logs, additionally reporting the block height of the backend that
// answered, when the provider accepts batches.
//
// go-ethereum based nodes answer an eth_getLogs query that reaches beyond
// their head with the logs they have, without an error. Behind a load
// balancer the backend answering eth_getLogs can be several blocks behind the
// one that reported the head a moment ago, so a query up to that head may
// silently miss its last blocks. To detect this, the query is sent as a
// JSON-RPC batch [eth_blockNumber, eth_getLogs]: the batch is one HTTP
// request, which load balancers hand to one backend, and the block number —
// evaluated first — is a height the log query is guaranteed to have seen.
func (e *Endpoint) logs(ctx context.Context, f LogFilter) (logs []Log, backendHead uint64, headKnown bool, err error) {
	if f.FromBlock > f.ToBlock {
		return nil, 0, false, fmt.Errorf("evm: invalid log range %d..%d", f.FromBlock, f.ToBlock)
	}
	topics := make([]any, len(f.Topics))
	for i, alts := range f.Topics {
		switch len(alts) {
		case 0:
			topics[i] = nil
		case 1:
			topics[i] = alts[0].String()
		default:
			list := make([]string, len(alts))
			for j, t := range alts {
				list[j] = t.String()
			}
			topics[i] = list
		}
	}
	arg := map[string]any{
		"fromBlock": encodeUint64(f.FromBlock),
		"toBlock":   encodeUint64(f.ToBlock),
		"address":   f.Address.Hex(),
		"topics":    topics,
	}

	e.mu.Lock()
	batching := !e.noBatch && !e.noLogBatch
	e.mu.Unlock()

	start := time.Now()
	var raw json.RawMessage
	if batching {
		elems := []batchElem{
			{Method: "eth_blockNumber"},
			{Method: "eth_getLogs", Params: []any{arg}},
		}
		berr := e.t.batch(ctx, elems)
		var be *Error
		switch {
		case berr == nil:
			raw, err = elems[1].Result, elems[1].Err
			if elems[0].Err == nil {
				var s string
				if json.Unmarshal(elems[0].Result, &s) == nil {
					if n, derr := decodeUint64(s); derr == nil {
						backendHead, headKnown = n, true
					}
				}
			}
			if err != nil && headKnown && IsKind(err, KindBadResponse) {
				// The batch was answered, but without an entry for the
				// log query (1rpc.io drops a failing member instead of
				// reporting its error). Ask plainly to learn why.
				raw, err = e.t.call(ctx, "eth_getLogs", arg)
			}
		case ctx.Err() != nil:
			return nil, 0, false, ctx.Err()
		case errors.As(berr, &be) && be.Kind != KindTransport && be.Kind != KindRequestTooLarge && be.Kind != KindTooManyResults &&
			(be.HTTPStatus == 0 || (be.HTTPStatus >= 200 && be.HTTPStatus <= 299)):
			// The provider answered, but not to this batch: it does not
			// take batches, or not ones with eth_getLogs in them (BSC's
			// dataseed nodes: "method eth_getLogs in batch triggered rate
			// limit"). Remember, and ask plainly — without the height
			// check; the plain answer decides what the endpoint can do.
			e.mu.Lock()
			e.noLogBatch = true
			e.mu.Unlock()
			raw, err = e.t.call(ctx, "eth_getLogs", arg)
		default:
			err = berr
		}
	} else {
		raw, err = e.t.call(ctx, "eth_getLogs", arg)
	}
	e.observeLogs(ctx, time.Since(start), err)
	if err != nil {
		// A node that refuses the range because it ends above its head
		// usually says where its head is; pass that on.
		var le *Error
		if errors.As(err, &le) && le.Kind == KindLagging {
			if n, ok := laggingHead(le.Message); ok {
				backendHead, headKnown = n, true
			}
		}
		return nil, backendHead, headKnown, err
	}
	var raws []rawLog
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, 0, false, e.badResponse("eth_getLogs", err)
	}
	logs = make([]Log, 0, len(raws))
	for i := range raws {
		l, err := raws[i].decode()
		if err != nil {
			return nil, 0, false, e.badResponse("eth_getLogs", err)
		}
		// The answer must satisfy the question. A node that returns logs
		// from other blocks or contracts has a broken index or filter and
		// cannot be trusted not to omit logs either.
		if l.BlockNumber < f.FromBlock || l.BlockNumber > f.ToBlock {
			return nil, 0, false, e.badResponse("eth_getLogs",
				fmt.Errorf("log in block %d is outside the requested range %d..%d", l.BlockNumber, f.FromBlock, f.ToBlock))
		}
		if l.Address != f.Address {
			return nil, 0, false, e.badResponse("eth_getLogs", errors.New("log was emitted by a contract other than the requested one"))
		}
		for pos, alts := range f.Topics {
			if len(alts) == 0 {
				continue
			}
			if pos >= len(l.Topics) || !containsHash(alts, l.Topics[pos]) {
				return nil, 0, false, e.badResponse("eth_getLogs", fmt.Errorf("log does not match the topic filter at position %d", pos))
			}
		}
		logs = append(logs, l)
	}
	return logs, backendHead, headKnown, nil
}

func containsHash(list []Hash, h Hash) bool {
	for _, x := range list {
		if x == h {
			return true
		}
	}
	return false
}
