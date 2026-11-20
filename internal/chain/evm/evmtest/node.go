package evmtest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Behavior is the set of knobs that make a Node misbehave. The zero value is
// a well-behaved, unlimited node.
type Behavior struct {
	// ChainID overrides the chain id reported by eth_chainId (0: the
	// chain's own).
	ChainID uint64
	// Lag makes the node serve the chain as it was Lag blocks ago.
	Lag uint64
	// LogsLag makes eth_getLogs (only) see a chain that is LogsLag blocks
	// further behind: queries reaching beyond it are silently truncated,
	// like a load balancer answering eth_blockNumber from one backend and
	// eth_getLogs from a slower one.
	LogsLag uint64
	// LogsBackendLag models a load balancer that hands requests containing
	// eth_getLogs to a backend this many blocks behind the one answering
	// everything else. Unlike LogsLag the lag is honest: an eth_blockNumber
	// in the same batch reports the slower backend's height.
	LogsBackendLag uint64
	// RefuseBeyondHead makes eth_getLogs fail, as current BSC nodes do,
	// when the range ends above the answering backend's head ("block range
	// extends beyond current head block: requested N, head M") instead of
	// silently truncating the answer.
	RefuseBeyondHead bool
	// Down makes every HTTP request fail with 503.
	Down bool
	// FailRequests makes the next N HTTP requests fail with 503.
	FailRequests int
	// RateLimited makes every HTTP request fail with 429.
	RateLimited bool
	// RetryAfter is sent as the Retry-After header (seconds) with 429.
	RetryAfter int
	// Delay is added to every request.
	Delay time.Duration

	// LogsError, if set, is returned for every eth_getLogs call (a
	// provider that does not serve logs).
	LogsError *RPCError
	// LogsBatchError, if set, is the whole answer to any batch containing
	// eth_getLogs: a one-element array with a null id, the way BSC's
	// dataseed nodes reject such batches.
	LogsBatchError *RPCError
	// MaxLogRange caps the number of blocks per eth_getLogs call
	// (0: unlimited). RangeError formats the refusal; the default mimics
	// "exceed maximum block range: N".
	MaxLogRange uint64
	RangeError  func(limit uint64) RPCError
	// MaxLogResults caps the number of logs per eth_getLogs answer.
	MaxLogResults int
	// MaxTopics caps the number of alternatives per topic position.
	MaxTopics int
	// TopicsError formats the MaxTopics refusal (default: a recognisable
	// "too many topics" message).
	TopicsError func(limit int) RPCError
	// DropLogs makes eth_getLogs silently omit matching logs (partial
	// data from a broken index).
	DropLogs func(evm.Log) bool
	// ExtraLogs are appended to every eth_getLogs answer whose block range
	// covers them, regardless of the chain (a lying endpoint).
	ExtraLogs []evm.Log

	// HideReceipts makes eth_getTransactionReceipt answer null.
	HideReceipts bool
	// NoBatch makes the node reject JSON-RPC batch requests.
	NoBatch bool
	// NoFinalized makes the node reject the "finalized" block tag.
	NoFinalized bool
	// FinalityDepth is how far the finalized block trails the head
	// (default 2).
	FinalityDepth uint64

	// Intercept, if set, is consulted first for every call. Returning
	// handled=true short-circuits normal processing with the given result
	// or error.
	Intercept func(method string, params []json.RawMessage) (result any, rpcErr *RPCError, handled bool)
	// InterceptBatch is Intercept for the members of a JSON-RPC batch
	// only, consulted before it: a provider whose batch answers differ
	// from its answers to single requests.
	InterceptBatch func(method string, params []json.RawMessage) (result any, rpcErr *RPCError, handled bool)
}

// LogQuery records one eth_getLogs call received by a node.
type LogQuery struct {
	From, To   uint64
	Recipients int // alternatives at topic position 2
	Refused    bool
}

// Node is a JSON-RPC endpoint (an http.Handler) serving a Chain.
type Node struct {
	mu       sync.Mutex
	chain    *Chain
	b        Behavior
	calls    map[string]int
	requests int
	queries  []LogQuery
	shown    uint64
	answered int
}

// NewNode returns a well-behaved node serving c.
func NewNode(c *Chain) *Node {
	return &Node{chain: c, calls: make(map[string]int)}
}

// Serve starts an HTTP server for the node and returns its URL. The server is
// closed when the test ends.
func (n *Node) Serve(t testing.TB) string {
	t.Helper()
	srv := httptest.NewServer(n)
	t.Cleanup(srv.Close)
	return srv.URL
}

// Set changes the node's behaviour.
func (n *Node) Set(change func(b *Behavior)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	change(&n.b)
}

// SetChain points the node at another chain (for example a Clone that stays
// on a stale fork).
func (n *Node) SetChain(c *Chain) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.chain = c
}

// Calls returns how many times a JSON-RPC method was called (batch members
// count individually).
func (n *Node) Calls(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[method]
}

// TotalCalls returns how many JSON-RPC calls of any method the node has
// received (batch members count individually): what a provider that bills by
// the call would bill.
func (n *Node) TotalCalls() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	total := 0
	for _, c := range n.calls {
		total += c
	}
	return total
}

// Requests returns the number of HTTP requests received.
func (n *Node) Requests() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.requests
}

// LogQueries returns the eth_getLogs calls received so far.
func (n *Node) LogQueries() []LogQuery {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]LogQuery(nil), n.queries...)
}

// Shown returns the highest chain height the node has claimed since the last
// ResetShown: the highest eth_blockNumber it answered, or number of a block
// it returned as "latest" — whatever its Behavior made it say.
func (n *Node) Shown() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.shown
}

// Answered returns the number of HTTP requests the node has answered since
// the last ResetShown with anything but a server failure: a JSON-RPC result
// or error, or a rate limit. (Down and FailRequests answer 503, which is not
// an answer.)
func (n *Node) Answered() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.answered
}

// ResetShown clears Shown and Answered.
func (n *Node) ResetShown() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.shown, n.answered = 0, 0
}

// noteShown records the height a response claims for the chain, if it is an
// answer to eth_blockNumber or to eth_getBlockByNumber("latest").
func (n *Node) noteShown(req request, result any) {
	var claimed string
	switch req.Method {
	case "eth_blockNumber":
		claimed, _ = result.(string)
	case "eth_getBlockByNumber":
		var tag string
		if len(req.Params) == 0 || json.Unmarshal(req.Params[0], &tag) != nil || tag != "latest" {
			return
		}
		if blk, ok := result.(map[string]any); ok {
			claimed, _ = blk["number"].(string)
		}
	}
	height, err := strconv.ParseUint(strings.TrimPrefix(claimed, "0x"), 16, 64)
	if claimed == "" || err != nil {
		return
	}
	n.mu.Lock()
	n.shown = max(n.shown, height)
	n.mu.Unlock()
}

// ResetCounters clears Calls, Requests and LogQueries.
func (n *Node) ResetCounters() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = make(map[string]int)
	n.requests = 0
	n.queries = nil
}

type request struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// ServeHTTP implements http.Handler.
func (n *Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	n.requests++
	b := n.b
	failing := false
	if n.b.FailRequests > 0 {
		n.b.FailRequests--
		failing = true
	}
	chain := n.chain
	n.mu.Unlock()

	// Read the body first: only then does the server notice a client that
	// hangs up while the handler is still waiting.
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if b.Delay > 0 {
		select {
		case <-time.After(b.Delay):
		case <-r.Context().Done():
			return
		}
	}
	switch {
	case b.Down || failing:
		http.Error(w, "<html><body>503 Service Unavailable</body></html>", http.StatusServiceUnavailable)
		return
	}
	n.mu.Lock()
	n.answered++
	n.mu.Unlock()
	switch {
	case b.RateLimited:
		if b.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(b.RetryAfter))
		}
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if b.NoBatch {
			_ = json.NewEncoder(w).Encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &RPCError{Code: -32600, Message: "batch requests are not supported"}})
			return
		}
		var reqs []request
		if err := json.Unmarshal(body, &reqs); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		for _, req := range reqs {
			if req.Method == "eth_getLogs" {
				if b.LogsBatchError != nil {
					_ = json.NewEncoder(w).Encode([]response{{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: b.LogsBatchError}})
					return
				}
				b.Lag += b.LogsBackendLag // the whole batch goes to one backend
				break
			}
		}
		resps := make([]response, len(reqs))
		for i, req := range reqs {
			resps[i] = n.handle(chain, b, req, true)
		}
		// Answer in reverse order: clients must match by id.
		for i, j := 0, len(resps)-1; i < j; i, j = i+1, j-1 {
			resps[i], resps[j] = resps[j], resps[i]
		}
		_ = json.NewEncoder(w).Encode(resps)
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Method == "eth_getLogs" {
		b.Lag += b.LogsBackendLag
	}
	_ = json.NewEncoder(w).Encode(n.handle(chain, b, req, false))
}

func (n *Node) handle(chain *Chain, b Behavior, req request, inBatch bool) response {
	n.mu.Lock()
	n.calls[req.Method]++
	n.mu.Unlock()

	resp := response{JSONRPC: "2.0", ID: req.ID}
	intercepts := []func(string, []json.RawMessage) (any, *RPCError, bool){b.Intercept}
	if inBatch {
		intercepts = append([]func(string, []json.RawMessage) (any, *RPCError, bool){b.InterceptBatch}, intercepts...)
	}
	for _, intercept := range intercepts {
		if intercept == nil {
			continue
		}
		if result, rpcErr, handled := intercept(req.Method, req.Params); handled {
			resp.Result, resp.Error = result, rpcErr
			if rpcErr == nil && result == nil {
				resp.Result = json.RawMessage("null")
			}
			n.noteShown(req, result)
			return resp
		}
	}
	result, rpcErr := n.dispatch(chain, b, req)
	if rpcErr != nil {
		resp.Error = rpcErr
		return resp
	}
	n.noteShown(req, result)
	if result == nil {
		result = json.RawMessage("null")
	}
	resp.Result = result
	return resp
}

func invalidParams(format string, args ...any) *RPCError {
	return &RPCError{Code: -32602, Message: "invalid params: " + fmt.Sprintf(format, args...)}
}

func (n *Node) dispatch(chain *Chain, b Behavior, req request) (any, *RPCError) {
	base, blocks := chain.view(b.Lag)
	head := blocks[len(blocks)-1]

	switch req.Method {
	case "eth_chainId":
		id := chain.chainID
		if b.ChainID != 0 {
			id = b.ChainID
		}
		return quantity(id), nil

	case "eth_blockNumber":
		return quantity(head.Number), nil

	case "eth_getBlockByNumber":
		if len(req.Params) < 1 {
			return nil, invalidParams("missing block")
		}
		var tag string
		if err := json.Unmarshal(req.Params[0], &tag); err != nil {
			return nil, invalidParams("block must be a string")
		}
		number, rpcErr := resolveTag(tag, base, head.Number, b)
		if rpcErr != nil {
			return nil, rpcErr
		}
		if number < base || number > head.Number {
			return nil, nil
		}
		return blockJSON(blocks[number-base]), nil

	case "eth_getLogs":
		return n.getLogs(base, blocks, b, req)

	case "eth_getTransactionReceipt":
		if len(req.Params) < 1 {
			return nil, invalidParams("missing hash")
		}
		var hs string
		if err := json.Unmarshal(req.Params[0], &hs); err != nil {
			return nil, invalidParams("hash must be a string")
		}
		want, err := evm.ParseHash(hs)
		if err != nil {
			return nil, invalidParams("bad hash")
		}
		if b.HideReceipts {
			return nil, nil
		}
		for i := len(blocks) - 1; i >= 0; i-- {
			blk := blocks[i]
			logIndex := uint64(0)
			for txIndex, tx := range blk.Txs {
				logs := txLogs(blk, tx, uint64(txIndex), &logIndex)
				if tx.Hash != want {
					continue
				}
				status := "0x1"
				if tx.Failed {
					status = "0x0"
				}
				out := make([]map[string]any, len(logs))
				for j, l := range logs {
					out[j] = logJSON(l)
				}
				return map[string]any{
					"transactionHash":  tx.Hash.String(),
					"transactionIndex": quantity(uint64(txIndex)),
					"blockHash":        blk.Hash.String(),
					"blockNumber":      quantity(blk.Number),
					"status":           status,
					"logs":             out,
				}, nil
			}
		}
		return nil, nil

	case "eth_getCode":
		addr, rpcErr := addressParam(req.Params, 0)
		if rpcErr != nil {
			return nil, rpcErr
		}
		if _, _, ok := chain.tokenInfo(addr); ok {
			return "0x6080604052600436106100", nil
		}
		return "0x", nil

	case "eth_call":
		if len(req.Params) < 1 {
			return nil, invalidParams("missing call object")
		}
		var call struct {
			To   string `json:"to"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal(req.Params[0], &call); err != nil {
			return nil, invalidParams("bad call object")
		}
		to, err := ethaddr.Parse(strings.ToLower(call.To))
		if err != nil {
			return nil, invalidParams("bad to address")
		}
		data, err := hex.DecodeString(strings.TrimPrefix(call.Data, "0x"))
		if err != nil || len(data) < 4 {
			return nil, invalidParams("bad call data")
		}
		symbol, decimals, ok := chain.tokenInfo(to)
		if !ok {
			return "0x", nil // calling an address without code returns empty data
		}
		switch hex.EncodeToString(data[:4]) {
		case "313ce567": // decimals()
			return word(new(big.Int).SetUint64(uint64(decimals))), nil
		case "95d89b41": // symbol()
			padded := make([]byte, (len(symbol)+31)/32*32)
			copy(padded, symbol)
			return word(big.NewInt(32)) + hex.EncodeToString(leftPad(big.NewInt(int64(len(symbol))))) + hex.EncodeToString(padded), nil
		case "70a08231": // balanceOf(address)
			if len(data) != 36 {
				return nil, &RPCError{Code: 3, Message: "execution reverted"}
			}
			var owner ethaddr.Address
			copy(owner[:], data[16:36])
			return word(chain.balanceOf(to, owner)), nil
		default:
			return nil, &RPCError{Code: 3, Message: "execution reverted"}
		}

	default:
		return nil, &RPCError{Code: -32601, Message: "the method " + req.Method + " does not exist/is not available"}
	}
}

func resolveTag(tag string, base, head uint64, b Behavior) (uint64, *RPCError) {
	switch tag {
	case "latest", "pending":
		return head, nil
	case "earliest":
		return base, nil
	case "finalized", "safe":
		if b.NoFinalized {
			return 0, &RPCError{Code: -32000, Message: "finalized block not found"}
		}
		depth := b.FinalityDepth
		if depth == 0 {
			depth = 2
		}
		if head-base < depth {
			return base, nil
		}
		return head - depth, nil
	}
	if !strings.HasPrefix(tag, "0x") {
		return 0, invalidParams("bad block tag %q", tag)
	}
	v, err := strconv.ParseUint(tag[2:], 16, 64)
	if err != nil {
		return 0, invalidParams("bad block number %q", tag)
	}
	return v, nil
}

func (n *Node) getLogs(base uint64, blocks []Block, b Behavior, req request) (any, *RPCError) {
	if len(req.Params) < 1 {
		return nil, invalidParams("missing filter")
	}
	var f struct {
		FromBlock string            `json:"fromBlock"`
		ToBlock   string            `json:"toBlock"`
		Address   string            `json:"address"`
		Topics    []json.RawMessage `json:"topics"`
	}
	if err := json.Unmarshal(req.Params[0], &f); err != nil {
		return nil, invalidParams("bad filter: %v", err)
	}
	head := blocks[len(blocks)-1].Number
	from, rpcErr := resolveTag(f.FromBlock, base, head, b)
	if rpcErr != nil {
		return nil, rpcErr
	}
	to, rpcErr := resolveTag(f.ToBlock, base, head, b)
	if rpcErr != nil {
		return nil, rpcErr
	}
	topics := make([][]evm.Hash, len(f.Topics))
	for i, raw := range f.Topics {
		switch {
		case string(raw) == "null":
		case len(raw) > 0 && raw[0] == '[':
			var list []string
			if err := json.Unmarshal(raw, &list); err != nil {
				return nil, invalidParams("bad topic list")
			}
			for _, s := range list {
				h, err := evm.ParseHash(s)
				if err != nil {
					return nil, invalidParams("bad topic")
				}
				topics[i] = append(topics[i], h)
			}
		default:
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, invalidParams("bad topic")
			}
			h, err := evm.ParseHash(s)
			if err != nil {
				return nil, invalidParams("bad topic")
			}
			topics[i] = []evm.Hash{h}
		}
	}
	query := LogQuery{From: from, To: to}
	if len(topics) > 2 {
		query.Recipients = len(topics[2])
	}
	refuse := func(e RPCError) (any, *RPCError) {
		query.Refused = true
		n.mu.Lock()
		n.queries = append(n.queries, query)
		n.mu.Unlock()
		return nil, &e
	}

	if b.LogsError != nil {
		return refuse(*b.LogsError)
	}
	if b.RefuseBeyondHead && to > head {
		return refuse(RPCError{Code: -32602, Message: fmt.Sprintf("block range extends beyond current head block: requested %d, head %d", to, head)})
	}
	if from > to {
		return refuse(RPCError{Code: -32602, Message: "invalid block range params"})
	}
	if b.MaxTopics > 0 {
		for _, alts := range topics {
			if len(alts) > b.MaxTopics {
				if b.TopicsError != nil {
					return refuse(b.TopicsError(b.MaxTopics))
				}
				return refuse(RPCError{Code: -32602, Message: fmt.Sprintf("exceed max topics: at most %d topics per position", b.MaxTopics)})
			}
		}
	}
	if b.MaxLogRange > 0 && to-from+1 > b.MaxLogRange {
		if b.RangeError != nil {
			return refuse(b.RangeError(b.MaxLogRange))
		}
		return refuse(RPCError{Code: -32602, Message: fmt.Sprintf("exceed maximum block range: %d", b.MaxLogRange)})
	}

	var address *ethaddr.Address
	if f.Address != "" {
		a, err := ethaddr.Parse(strings.ToLower(f.Address))
		if err != nil {
			return nil, invalidParams("bad address")
		}
		address = &a
	}

	// The log index may lag further behind than the head.
	visible := blocks
	if b.LogsLag > 0 {
		cut := uint64(len(blocks))
		if b.LogsLag >= cut {
			cut = 1
		} else {
			cut -= b.LogsLag
		}
		visible = blocks[:cut]
	}
	out := []map[string]any{}
	count := 0
	for _, blk := range visible {
		if blk.Number < from || blk.Number > to {
			continue
		}
		logIndex := uint64(0)
		for txIndex, tx := range blk.Txs {
			for _, l := range txLogs(blk, tx, uint64(txIndex), &logIndex) {
				if address != nil && l.Address != *address {
					continue
				}
				if !matchTopics(l.Topics, topics) {
					continue
				}
				if b.DropLogs != nil && b.DropLogs(l) {
					continue
				}
				out = append(out, logJSON(l))
				count++
			}
		}
	}
	for _, l := range b.ExtraLogs {
		if l.BlockNumber >= from && l.BlockNumber <= to {
			out = append(out, logJSON(l))
			count++
		}
	}
	if b.MaxLogResults > 0 && count > b.MaxLogResults {
		return refuse(RPCError{Code: -32005, Message: fmt.Sprintf("query returned more than %d results", b.MaxLogResults)})
	}
	n.mu.Lock()
	n.queries = append(n.queries, query)
	n.mu.Unlock()
	return out, nil
}

// txLogs returns the logs of tx as they appear in blk; logIndex is the
// running per-block log counter.
func txLogs(blk Block, tx Tx, txIndex uint64, logIndex *uint64) []evm.Log {
	if tx.Failed {
		return nil
	}
	out := make([]evm.Log, len(tx.Logs))
	for i, l := range tx.Logs {
		out[i] = evm.Log{
			Address:     l.Address,
			Topics:      l.Topics,
			Data:        l.Data,
			BlockNumber: blk.Number,
			BlockHash:   blk.Hash,
			TxHash:      tx.Hash,
			TxIndex:     txIndex,
			LogIndex:    *logIndex,
		}
		*logIndex++
	}
	return out
}

func matchTopics(have []evm.Hash, filter [][]evm.Hash) bool {
	for pos, alts := range filter {
		if len(alts) == 0 {
			continue
		}
		if pos >= len(have) {
			return false
		}
		found := false
		for _, a := range alts {
			if a == have[pos] {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func blockJSON(b Block) map[string]any {
	txs := make([]string, len(b.Txs))
	for i, tx := range b.Txs {
		txs[i] = tx.Hash.String()
	}
	return map[string]any{
		"number":       quantity(b.Number),
		"hash":         b.Hash.String(),
		"parentHash":   b.ParentHash.String(),
		"timestamp":    quantity(b.Time),
		"transactions": txs,
	}
}

func logJSON(l evm.Log) map[string]any {
	topics := make([]string, len(l.Topics))
	for i, t := range l.Topics {
		topics[i] = t.String()
	}
	return map[string]any{
		"address":          l.Address.Hex(),
		"topics":           topics,
		"data":             "0x" + hex.EncodeToString(l.Data),
		"blockNumber":      quantity(l.BlockNumber),
		"blockHash":        l.BlockHash.String(),
		"transactionHash":  l.TxHash.String(),
		"transactionIndex": quantity(l.TxIndex),
		"logIndex":         quantity(l.LogIndex),
		"removed":          l.Removed,
	}
}

func addressParam(params []json.RawMessage, i int) (ethaddr.Address, *RPCError) {
	if len(params) <= i {
		return ethaddr.Address{}, invalidParams("missing address")
	}
	var s string
	if err := json.Unmarshal(params[i], &s); err != nil {
		return ethaddr.Address{}, invalidParams("address must be a string")
	}
	a, err := ethaddr.Parse(strings.ToLower(s))
	if err != nil {
		return ethaddr.Address{}, invalidParams("bad address")
	}
	return a, nil
}

func quantity(v uint64) string { return "0x" + strconv.FormatUint(v, 16) }

func leftPad(v *big.Int) []byte {
	out := make([]byte, 32)
	v.FillBytes(out)
	return out
}

func word(v *big.Int) string { return "0x" + hex.EncodeToString(leftPad(v)) }
