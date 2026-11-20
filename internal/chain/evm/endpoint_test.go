package evm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

var (
	usdt      = ethaddr.MustParse("0x55d398326f99059fF775485246999027B3197955")
	fakeToken = ethaddr.MustParse("0x55d398326f99059ff775485246999027b3197966") // look-alike
	payer     = ethaddr.MustParse("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
)

// addr returns a deterministic test address.
func addr(i int) ethaddr.Address {
	sum := ethaddr.Keccak256([]byte(fmt.Sprintf("address-%d", i)))
	a, _ := ethaddr.FromBytes(sum[:20])
	return a
}

func units(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newChain(t *testing.T) (*evmtest.Chain, *evmtest.Node, *evm.Endpoint) {
	t.Helper()
	chain := evmtest.NewChain(56, 1000)
	chain.AddToken(usdt, "USDT", 18)
	node := evmtest.NewNode(chain)
	ep, err := evm.NewEndpoint(node.Serve(t), evm.EndpointOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return chain, node, ep
}

func TestEndpointBasics(t *testing.T) {
	ctx := context.Background()
	chain, node, ep := newChain(t)
	chain.Mine(5)

	if id, err := ep.ChainID(ctx); err != nil || id != 56 {
		t.Fatalf("ChainID = %d, %v", id, err)
	}
	if n, err := ep.BlockNumber(ctx); err != nil || n != 1005 {
		t.Fatalf("BlockNumber = %d, %v", n, err)
	}
	want, _ := chain.BlockByNumber(1003)
	h, err := ep.HeaderByNumber(ctx, 1003)
	if err != nil {
		t.Fatal(err)
	}
	if h.Number != 1003 || h.Hash != want.Hash || h.ParentHash != want.ParentHash || h.Time != want.Time {
		t.Fatalf("header = %+v, want %+v", h, want)
	}
	if _, err := ep.HeaderByNumber(ctx, 2000); !errors.Is(err, evm.ErrNotFound) {
		t.Fatalf("future block: %v", err)
	}
	latest, err := ep.HeaderByTag(ctx, evm.TagLatest)
	if err != nil || latest.Number != 1005 {
		t.Fatalf("latest: %+v %v", latest, err)
	}
	fin, err := ep.HeaderByTag(ctx, evm.TagFinalized)
	if err != nil || fin.Number != 1003 {
		t.Fatalf("finalized: %+v %v", fin, err)
	}

	// Head: one HTTP request for latest + finalized + two numbers.
	node.ResetCounters()
	res, err := ep.Head(ctx, evm.HeadRequest{Finalized: true, Numbers: []uint64{1001, 1004, 5000}})
	if err != nil {
		t.Fatal(err)
	}
	if node.Requests() != 1 || node.Calls("eth_getBlockByNumber") != 5 {
		t.Errorf("Head used %d requests / %d calls, want 1 / 5", node.Requests(), node.Calls("eth_getBlockByNumber"))
	}
	if res.Latest.Number != 1005 || res.Finalized == nil || res.Finalized.Number != 1003 {
		t.Errorf("Head = %+v", res)
	}
	if len(res.ByNumber) != 2 || res.ByNumber[1001].Number != 1001 || res.ByNumber[1004].Hash == (evm.Hash{}) {
		t.Errorf("ByNumber = %+v", res.ByNumber)
	}
	if _, ok := res.ByNumber[5000]; ok {
		t.Error("unknown block reported")
	}

	health := ep.Health()
	if health.ConsecutiveFailures != 0 || health.BlockNumber != 1005 || health.LastSuccessAt.IsZero() {
		t.Errorf("health = %+v", health)
	}
}

func TestHeadWithoutBatchOrFinality(t *testing.T) {
	ctx := context.Background()
	chain, node, ep := newChain(t)
	chain.Mine(3)
	node.Set(func(b *evmtest.Behavior) { b.NoBatch = true; b.NoFinalized = true })

	res, err := ep.Head(ctx, evm.HeadRequest{Finalized: true, Numbers: []uint64{1001}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Latest.Number != 1003 || res.Finalized != nil || len(res.ByNumber) != 1 {
		t.Fatalf("Head = %+v", res)
	}
	// The endpoint remembers both facts: no further batch attempt, no
	// further "finalized" request.
	node.ResetCounters()
	if _, err := ep.Head(ctx, evm.HeadRequest{Finalized: true, Numbers: []uint64{1001}}); err != nil {
		t.Fatal(err)
	}
	if node.Requests() != 2 || node.Calls("eth_getBlockByNumber") != 2 {
		t.Errorf("second Head used %d requests / %d calls, want 2 / 2", node.Requests(), node.Calls("eth_getBlockByNumber"))
	}
	if f := ep.Health().ConsecutiveFailures; f != 0 {
		t.Errorf("batch/finality refusal counted as %d failures", f)
	}
}

func TestReceiptAndERC20(t *testing.T) {
	ctx := context.Background()
	chain, _, ep := newChain(t)
	to := addr(1)
	txHash := chain.Submit(
		evmtest.TransferLog(usdt, payer, addr(9), units(1)),
		evmtest.TransferLog(usdt, payer, to, units(25)),
	)
	failed := chain.SubmitFailed()
	blk := chain.Mine(1)

	r, err := ep.TransactionReceipt(ctx, txHash)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != 1 || r.BlockHash != blk.Hash || r.BlockNumber != blk.Number || len(r.Logs) != 2 {
		t.Fatalf("receipt = %+v", r)
	}
	tr, err := evm.DecodeTransfer(r.Logs[1])
	if err != nil || tr.To != to || tr.Amount.Cmp(units(25)) != 0 || tr.LogIndex != 1 || tr.Token != usdt {
		t.Fatalf("transfer = %+v, %v", tr, err)
	}
	fr, err := ep.TransactionReceipt(ctx, failed)
	if err != nil || fr.Status != 0 || len(fr.Logs) != 0 {
		t.Fatalf("failed receipt = %+v, %v", fr, err)
	}
	if _, err := ep.TransactionReceipt(ctx, evm.Hash{0xde, 0xad}); !errors.Is(err, evm.ErrNotFound) {
		t.Fatalf("unknown tx: %v", err)
	}
	rs, err := ep.TransactionReceipts(ctx, []evm.Hash{txHash, {0xde, 0xad}, failed})
	if err != nil || len(rs) != 3 || rs[0] == nil || rs[1] != nil || rs[2] == nil || rs[2].Status != 0 {
		t.Fatalf("receipts = %+v, %v", rs, err)
	}

	if d, err := ep.Decimals(ctx, usdt); err != nil || d != 18 {
		t.Fatalf("decimals = %d, %v", d, err)
	}
	if s, err := ep.Symbol(ctx, usdt); err != nil || s != "USDT" {
		t.Fatalf("symbol = %q, %v", s, err)
	}
	if b, err := ep.BalanceOf(ctx, usdt, to); err != nil || b.Cmp(units(25)) != 0 {
		t.Fatalf("balance = %v, %v", b, err)
	}
	if code, err := ep.Code(ctx, usdt); err != nil || len(code) == 0 {
		t.Fatalf("code: %x %v", code, err)
	}
	if code, err := ep.Code(ctx, addr(5)); err != nil || len(code) != 0 {
		t.Fatalf("code of EOA: %x %v", code, err)
	}
	// A contract-less address answers eth_call with empty data: that must
	// be an error, not "0 decimals".
	if _, err := ep.Decimals(ctx, addr(5)); !evm.IsKind(err, evm.KindBadResponse) {
		t.Fatalf("decimals of EOA: %v", err)
	}
}

// scanAll runs TransferLogs and insists on full coverage of the range.
func scanAll(ctx context.Context, ep *evm.Endpoint, q evm.TransferQuery) ([]evm.Transfer, error) {
	scan, err := ep.TransferLogs(ctx, q)
	if err != nil {
		return nil, err
	}
	if !scan.Complete(q) {
		return nil, fmt.Errorf("partial coverage: next block %d of %d..%d", scan.Next, q.FromBlock, q.ToBlock)
	}
	return scan.Transfers, nil
}

// scripted returns an endpoint whose every call is answered by fn.
func scripted(t *testing.T, opts evm.EndpointOptions, fn func(w http.ResponseWriter, method string, id json.RawMessage)) *evm.Endpoint {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		fn(w, req.Method, req.ID)
	}))
	t.Cleanup(srv.Close)
	ep, err := evm.NewEndpoint(srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func reply(w http.ResponseWriter, id json.RawMessage, result string) {
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
}

// TestHostileResponses feeds the client the kinds of garbage a broken or
// malicious endpoint can produce. Each must surface as an error of the right
// kind — never as a value.
func TestHostileResponses(t *testing.T) {
	ctx := context.Background()
	hash := `"0x` + strings.Repeat("ab", 32) + `"`
	block := func(number string) string {
		return `{"number":` + number + `,"hash":` + hash + `,"parentHash":` + hash + `,"timestamp":"0x1"}`
	}
	tests := []struct {
		name string
		body func(id json.RawMessage) string
		call func(ep *evm.Endpoint) error
		want evm.ErrorKind
	}{
		{"html with status 200", func(json.RawMessage) string { return "<html>hello</html>" },
			func(ep *evm.Endpoint) error { _, err := ep.ChainID(ctx); return err }, evm.KindBadResponse},
		{"empty body", func(json.RawMessage) string { return "" },
			func(ep *evm.Endpoint) error { _, err := ep.ChainID(ctx); return err }, evm.KindBadResponse},
		{"json that is not json-rpc", func(json.RawMessage) string { return `{"message":"hi"}` },
			func(ep *evm.Endpoint) error { _, err := ep.ChainID(ctx); return err }, evm.KindBadResponse},
		{"wrong id", func(json.RawMessage) string { return `{"jsonrpc":"2.0","id":424242,"result":"0x38"}` },
			func(ep *evm.Endpoint) error { _, err := ep.ChainID(ctx); return err }, evm.KindBadResponse},
		{"null id", func(json.RawMessage) string { return `{"jsonrpc":"2.0","id":null,"result":"0x38"}` },
			func(ep *evm.Endpoint) error { _, err := ep.ChainID(ctx); return err }, evm.KindBadResponse},
		{"chain id as number", func(id json.RawMessage) string { return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":56}`, id) },
			func(ep *evm.Endpoint) error { _, err := ep.ChainID(ctx); return err }, evm.KindBadResponse},
		{"chain id without 0x", func(id json.RawMessage) string { return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":"38"}`, id) },
			func(ep *evm.Endpoint) error { _, err := ep.ChainID(ctx); return err }, evm.KindBadResponse},
		{"chain id null", func(id json.RawMessage) string { return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, id) },
			func(ep *evm.Endpoint) error { _, err := ep.ChainID(ctx); return err }, evm.KindBadResponse},
		{"block number overflow", func(id json.RawMessage) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":"0x1ffffffffffffffff"}`, id)
		}, func(ep *evm.Endpoint) error { _, err := ep.BlockNumber(ctx); return err }, evm.KindBadResponse},
		{"header for another number", func(id json.RawMessage) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, block(`"0x7"`))
		}, func(ep *evm.Endpoint) error { _, err := ep.HeaderByNumber(ctx, 5); return err }, evm.KindBadResponse},
		{"header missing hash", func(id json.RawMessage) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"number":"0x5","parentHash":%s,"timestamp":"0x1"}}`, id, hash)
		}, func(ep *evm.Endpoint) error { _, err := ep.HeaderByNumber(ctx, 5); return err }, evm.KindBadResponse},
		{"header with short hash", func(id json.RawMessage) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"number":"0x5","hash":"0xabcd","parentHash":%s,"timestamp":"0x1"}}`, id, hash)
		}, func(ep *evm.Endpoint) error { _, err := ep.HeaderByNumber(ctx, 5); return err }, evm.KindBadResponse},
		{"header with zero hash", func(id json.RawMessage) string {
			zero := `"0x` + strings.Repeat("00", 32) + `"`
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"number":"0x5","hash":%s,"parentHash":%s,"timestamp":"0x1"}}`, id, zero, hash)
		}, func(ep *evm.Endpoint) error { _, err := ep.HeaderByNumber(ctx, 5); return err }, evm.KindBadResponse},
		{"receipt for another tx", func(id json.RawMessage) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"transactionHash":%s,"blockHash":%s,"blockNumber":"0x5","status":"0x1","logs":[]}}`, id, hash, hash)
		}, func(ep *evm.Endpoint) error { _, err := ep.TransactionReceipt(ctx, evm.Hash{1}); return err }, evm.KindBadResponse},
		{"receipt with status 2", func(id json.RawMessage) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"transactionHash":%s,"blockHash":%s,"blockNumber":"0x5","status":"0x2","logs":[]}}`, id, hash, hash)
		}, func(ep *evm.Endpoint) error {
			_, err := ep.TransactionReceipt(ctx, evm.MustParseHash(strings.Trim(hash, `"`)))
			return err
		}, evm.KindBadResponse},
		{"receipt without status", func(id json.RawMessage) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"transactionHash":%s,"blockHash":%s,"blockNumber":"0x5","logs":[]}}`, id, hash, hash)
		}, func(ep *evm.Endpoint) error {
			_, err := ep.TransactionReceipt(ctx, evm.MustParseHash(strings.Trim(hash, `"`)))
			return err
		}, evm.KindBadResponse},
		{"decimals too wide", func(id json.RawMessage) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":"0x%s"}`, id, strings.Repeat("00", 30)+"0100")
		}, func(ep *evm.Endpoint) error { _, err := ep.Decimals(ctx, usdt); return err }, evm.KindBadResponse},
		{"decimals 64 bytes", func(id json.RawMessage) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":"0x%s"}`, id, strings.Repeat("00", 64))
		}, func(ep *evm.Endpoint) error { _, err := ep.Decimals(ctx, usdt); return err }, evm.KindBadResponse},
		{"odd-length call data", func(id json.RawMessage) string { return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":"0x123"}`, id) },
			func(ep *evm.Endpoint) error { _, err := ep.Call(ctx, usdt, []byte{1, 2, 3, 4}); return err }, evm.KindBadResponse},
		{"execution reverted", func(id json.RawMessage) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":3,"message":"execution reverted"}}`, id)
		}, func(ep *evm.Endpoint) error { _, err := ep.Call(ctx, usdt, []byte{1, 2, 3, 4}); return err }, evm.KindRPC},
		{"error with null id", func(json.RawMessage) string {
			return `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Unauthorized: You must authenticate your request with an API key."}}`
		}, func(ep *evm.Endpoint) error { _, err := ep.ChainID(ctx); return err }, evm.KindUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := scripted(t, evm.EndpointOptions{}, func(w http.ResponseWriter, _ string, id json.RawMessage) {
				io.WriteString(w, tc.body(id))
			})
			err := tc.call(ep)
			if !evm.IsKind(err, tc.want) {
				t.Fatalf("err = %v, want kind %v", err, tc.want)
			}
		})
	}
}

func TestHostileLogs(t *testing.T) {
	ctx := context.Background()
	hash := "0x" + strings.Repeat("ab", 32)
	recipient := addr(1)
	logJSON := func(mutate func(m map[string]any)) string {
		m := map[string]any{
			"address":          usdt.Hex(),
			"topics":           []string{evm.TransferTopic.String(), "0x" + strings.Repeat("00", 32), "0x" + strings.Repeat("00", 12) + recipient.Hex()[2:]},
			"data":             "0x" + strings.Repeat("00", 31) + "05",
			"blockNumber":      "0x64",
			"blockHash":        hash,
			"transactionHash":  hash,
			"transactionIndex": "0x0",
			"logIndex":         "0x0",
			"removed":          false,
		}
		mutate(m)
		raw, _ := json.Marshal([]any{m})
		return string(raw)
	}
	query := evm.TransferQuery{Token: usdt, FromBlock: 100, ToBlock: 100, Recipients: []ethaddr.Address{recipient}}

	bad := map[string]func(m map[string]any){
		"block below range": func(m map[string]any) { m["blockNumber"] = "0x63" },
		"block above range": func(m map[string]any) { m["blockNumber"] = "0x65" },
		"other contract":    func(m map[string]any) { m["address"] = fakeToken.Hex() },
		"other recipient": func(m map[string]any) {
			m["topics"].([]string)[2] = "0x" + strings.Repeat("00", 12) + addr(2).Hex()[2:]
		},
		"other event":          func(m map[string]any) { m["topics"].([]string)[0] = hash },
		"pending (null block)": func(m map[string]any) { m["blockHash"] = nil },
		"missing tx hash":      func(m map[string]any) { delete(m, "transactionHash") },
		"zero block hash":      func(m map[string]any) { m["blockHash"] = "0x" + strings.Repeat("00", 32) },
		"bad data hex":         func(m map[string]any) { m["data"] = "0xzz" },
		"five topics":          func(m map[string]any) { m["topics"] = append(m["topics"].([]string), hash, hash) },
		"short topic":          func(m map[string]any) { m["topics"].([]string)[1] = "0x00" },
		"log index overflow":   func(m map[string]any) { m["logIndex"] = "0x1" + strings.Repeat("0", 16) },
		"address not hex":      func(m map[string]any) { m["address"] = "USDT" },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			ep := scripted(t, evm.EndpointOptions{}, func(w http.ResponseWriter, _ string, id json.RawMessage) {
				reply(w, id, logJSON(mutate))
			})
			got, err := scanAll(ctx, ep, query)
			if !evm.IsKind(err, evm.KindBadResponse) {
				t.Fatalf("got %v, err = %v; want KindBadResponse", got, err)
			}
			if ep.Health().ConsecutiveFailures == 0 {
				t.Error("garbage did not count against the endpoint")
			}
		})
	}

	// Logs that are valid answers to the query but not ERC-20 transfers
	// are dropped, not errors.
	dropped := map[string]func(m map[string]any){
		"removed":           func(m map[string]any) { m["removed"] = true },
		"ERC-721 (4 topic)": func(m map[string]any) { m["topics"] = append(m["topics"].([]string), hash) },
		"64-byte data":      func(m map[string]any) { m["data"] = "0x" + strings.Repeat("00", 64) },
		"dirty sender word": func(m map[string]any) { m["topics"].([]string)[1] = hash },
	}
	for name, mutate := range dropped {
		t.Run(name, func(t *testing.T) {
			ep := scripted(t, evm.EndpointOptions{}, func(w http.ResponseWriter, _ string, id json.RawMessage) {
				reply(w, id, logJSON(mutate))
			})
			got, err := scanAll(ctx, ep, query)
			if err != nil || len(got) != 0 {
				t.Fatalf("got %v, err = %v; want no transfers and no error", got, err)
			}
		})
	}

	// And the unmodified log decodes.
	ep := scripted(t, evm.EndpointOptions{}, func(w http.ResponseWriter, _ string, id json.RawMessage) {
		reply(w, id, logJSON(func(map[string]any) {}))
	})
	got, err := scanAll(ctx, ep, query)
	if err != nil || len(got) != 1 || got[0].To != recipient || got[0].Amount.Int64() != 5 {
		t.Fatalf("got %+v, err = %v", got, err)
	}
}

func TestResponseSizeTimeoutAndCancellation(t *testing.T) {
	ctx := context.Background()

	t.Run("oversized response", func(t *testing.T) {
		ep := scripted(t, evm.EndpointOptions{MaxResponseBytes: 1024}, func(w http.ResponseWriter, _ string, id json.RawMessage) {
			reply(w, id, `"0x`+strings.Repeat("00", 4096)+`"`)
		})
		_, err := ep.Call(ctx, usdt, []byte{1, 2, 3, 4})
		if !evm.IsKind(err, evm.KindTooManyResults) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("timeout counts against the endpoint", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		ep := scripted(t, evm.EndpointOptions{Timeout: 50 * time.Millisecond}, func(http.ResponseWriter, string, json.RawMessage) {
			<-release
		})
		start := time.Now()
		_, err := ep.ChainID(ctx)
		if !evm.IsKind(err, evm.KindTransport) || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("err = %v", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("timeout not enforced")
		}
		if ep.Health().ConsecutiveFailures != 1 {
			t.Errorf("failures = %d", ep.Health().ConsecutiveFailures)
		}
	})

	t.Run("caller cancellation is not the endpoint's fault", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		ep := scripted(t, evm.EndpointOptions{}, func(http.ResponseWriter, string, json.RawMessage) {
			<-release
		})
		cctx, cancel := context.WithCancel(ctx)
		go func() { time.Sleep(30 * time.Millisecond); cancel() }()
		_, err := ep.ChainID(cctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if h := ep.Health(); h.ConsecutiveFailures != 0 || h.LastError != "" {
			t.Errorf("cancellation recorded as failure: %+v", h)
		}
	})

	t.Run("redirects are not followed", func(t *testing.T) {
		var hits int
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
		defer target.Close()
		src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		defer src.Close()
		ep, _ := evm.NewEndpoint(src.URL, evm.EndpointOptions{})
		if _, err := ep.ChainID(ctx); !evm.IsKind(err, evm.KindHTTP) {
			t.Fatalf("err = %v", err)
		}
		if hits != 0 {
			t.Fatal("redirect was followed")
		}
	})
}

// TestErrorsDoNotLeakURL: provider URLs embed API keys; neither errors nor
// the health record may contain them.
func TestErrorsDoNotLeakURL(t *testing.T) {
	ctx := context.Background()
	const secret = "SUPERSECRETAPIKEY"

	// Unreachable host (closed local port).
	closed := httptest.NewServer(http.NotFoundHandler())
	base := closed.URL
	closed.Close()
	ep, err := evm.NewEndpoint(base+"/v1/"+secret+"?token="+secret, evm.EndpointOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ep.ChainID(ctx)
	if !evm.IsKind(err, evm.KindTransport) {
		t.Fatalf("err = %v", err)
	}
	// Reachable host answering 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ep2, _ := evm.NewEndpoint(srv.URL+"/v1/"+secret, evm.EndpointOptions{})
	_, err2 := ep2.ChainID(ctx)
	if !evm.IsKind(err2, evm.KindHTTP) {
		t.Fatalf("err = %v", err2)
	}
	for _, s := range []string{err.Error(), err2.Error(), ep.Health().LastError, ep2.Health().LastError, ep.Label(), ep2.Label(),
		fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", ep.Health())} {
		if strings.Contains(s, secret) {
			t.Errorf("API key leaked: %s", s)
		}
	}
	if !strings.Contains(ep.URL(), secret) {
		t.Error("URL() should return the full URL")
	}
	probe := evm.ProbeWith(ctx, base+"/v1/"+secret, 56, usdt, evm.ProbeOptions{Endpoint: evm.EndpointOptions{Timeout: 2 * time.Second}})
	if s := fmt.Sprintf("%+v", probe); strings.Contains(s, secret) {
		t.Errorf("API key leaked through Probe: %s", s)
	}
}

func TestHTTPStatusClassification(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		status int
		header string
		body   string
		want   evm.ErrorKind
	}{
		{429, "7", "Too Many Requests", evm.KindRateLimited},
		{429, "", `{"id":1,"jsonrpc":"2.0","error":{"message":"You reached Public endpoint rate limit","code":15}}`, evm.KindRateLimited},
		{403, "", "<html><body><h1>403 Forbidden</h1></body></html>", evm.KindUnavailable},
		{401, "", "unauthorized", evm.KindUnavailable},
		{502, "", "<html>bad gateway</html>", evm.KindHTTP},
		{521, "", "error code: 521", evm.KindHTTP},
		{404, "", "not found", evm.KindHTTP},
		{413, "", "too big", evm.KindRequestTooLarge},
		{400, "", `{"id":1,"jsonrpc":"2.0","error":{"message":"execution reverted","code":3}}`, evm.KindRPC},
	}
	for _, tc := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tc.header != "" {
				w.Header().Set("Retry-After", tc.header)
			}
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		ep, _ := evm.NewEndpoint(srv.URL, evm.EndpointOptions{})
		_, err := ep.ChainID(ctx)
		if !evm.IsKind(err, tc.want) {
			t.Errorf("HTTP %d %q: err = %v, want %v", tc.status, tc.body, err, tc.want)
		}
		var ee *evm.Error
		if errors.As(err, &ee) && tc.header != "" && ee.RetryAfter != 7*time.Second {
			t.Errorf("Retry-After = %v", ee.RetryAfter)
		}
		srv.Close()
	}
}

// --- TransferLogs ---------------------------------------------------------

type wantTransfer struct {
	tx evm.Hash
	to ethaddr.Address
}

// populate mines `blocks` blocks with transfers to the given recipients
// sprinkled over them, plus noise that must not be returned. It returns the
// expected transfers in chain order.
func populate(chain *evmtest.Chain, rng *rand.Rand, blocks int, recipients []ethaddr.Address) []wantTransfer {
	var want []wantTransfer
	for i := 0; i < blocks; i++ {
		if rng.IntN(3) == 0 {
			to := recipients[rng.IntN(len(recipients))]
			tx := chain.Transfer(usdt, payer, to, units(int64(1+rng.IntN(100))))
			want = append(want, wantTransfer{tx, to})
		}
		if rng.IntN(4) == 0 {
			chain.Transfer(usdt, payer, addr(900_000+i), units(1)) // someone else's payment
		}
		if rng.IntN(5) == 0 {
			chain.Transfer(fakeToken, payer, recipients[rng.IntN(len(recipients))], units(1_000_000)) // look-alike token
		}
		chain.Mine(1)
	}
	return want
}

func checkTransfers(t *testing.T, got []evm.Transfer, want []wantTransfer) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d transfers, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].TxHash != want[i].tx || got[i].To != want[i].to || got[i].Token != usdt {
			t.Fatalf("transfer %d = %+v, want tx %s to %s", i, got[i], want[i].tx, want[i].to)
		}
		if i > 0 && (got[i].BlockNumber < got[i-1].BlockNumber ||
			(got[i].BlockNumber == got[i-1].BlockNumber && got[i].LogIndex <= got[i-1].LogIndex)) {
			t.Fatalf("transfers not sorted at %d", i)
		}
	}
}

// checkCoverage asserts that the successful eth_getLogs calls cover
// [from, to] without a gap for every recipient chunk.
func checkCoverage(t *testing.T, queries []evmtest.LogQuery, from, to uint64) {
	t.Helper()
	covered := make(map[uint64]bool)
	for _, q := range queries {
		if q.Refused {
			continue
		}
		for b := q.From; b <= q.To; b++ {
			covered[b] = true
		}
	}
	for b := from; b <= to; b++ {
		if !covered[b] {
			t.Fatalf("block %d was never queried successfully", b)
		}
	}
}

func TestTransferLogsProviderLimits(t *testing.T) {
	ctx := context.Background()
	recipients := []ethaddr.Address{addr(1), addr(2), addr(3)}

	styles := map[string]func(limit uint64) evmtest.RPCError{
		"nownodes": func(l uint64) evmtest.RPCError {
			return evmtest.RPCError{Code: -32602, Message: fmt.Sprintf("exceed maximum block range %d", l)}
		},
		"1rpc": func(l uint64) evmtest.RPCError {
			return evmtest.RPCError{Code: -32602, Message: fmt.Sprintf("eth_getLogs is limited to 0 - %d blocks range", l)}
		},
		"blockrazor": func(l uint64) evmtest.RPCError {
			return evmtest.RPCError{Code: -32000, Message: fmt.Sprintf("log query range must not exceed %d blocks", l)}
		},
		"drpc": func(l uint64) evmtest.RPCError {
			return evmtest.RPCError{Code: 35, Message: fmt.Sprintf("ranges over %d blocks are not supported on free plan", l)}
		},
		"no number": func(uint64) evmtest.RPCError {
			return evmtest.RPCError{Code: -32000, Message: "block range is too wide"}
		},
		// A refusal this client has never seen: it must still converge
		// (via the one-block probe) instead of failing or skipping.
		"unrecognised wording": func(uint64) evmtest.RPCError {
			return evmtest.RPCError{Code: -32000, Message: "nope"}
		},
		// BSC dataseed wording, here used as an actual range limit.
		"bare limit exceeded": func(uint64) evmtest.RPCError {
			return evmtest.RPCError{Code: -32005, Message: "limit exceeded"}
		},
	}
	for name, style := range styles {
		for _, limit := range []uint64{1, 7, 25, 50, 333} {
			t.Run(fmt.Sprintf("%s/limit=%d", name, limit), func(t *testing.T) {
				chain, node, ep := newChain(t)
				rng := rand.New(rand.NewPCG(1, limit))
				want := populate(chain, rng, 700, recipients)
				node.Set(func(b *evmtest.Behavior) { b.MaxLogRange = limit; b.RangeError = style })

				from, to := uint64(1001), chain.Head().Number
				got, err := scanAll(ctx, ep, evm.TransferQuery{Token: usdt, FromBlock: from, ToBlock: to, Recipients: recipients})
				if err != nil {
					t.Fatal(err)
				}
				checkTransfers(t, got, want)
				checkCoverage(t, node.LogQueries(), from, to)

				// The limit has been learned: a second scan wastes at
				// most one call on probing upwards.
				node.ResetCounters()
				if _, err := scanAll(ctx, ep, evm.TransferQuery{Token: usdt, FromBlock: from, ToBlock: to, Recipients: recipients}); err != nil {
					t.Fatal(err)
				}
				refused := 0
				for _, q := range node.LogQueries() {
					if q.Refused {
						refused++
					}
				}
				if refused > 1 {
					t.Errorf("second scan was refused %d times; limit not learned", refused)
				}
				if h := ep.Health(); h.ConsecutiveFailures != 0 || h.Logs.Unavailable || h.Logs.Range > limit {
					t.Errorf("health after scan: %+v", h)
				}
			})
		}
	}
}

func TestTransferLogsTooManyResults(t *testing.T) {
	ctx := context.Background()
	chain, node, ep := newChain(t)
	recipients := []ethaddr.Address{addr(1), addr(2)}
	var want []wantTransfer
	for i := 0; i < 300; i++ {
		to := recipients[i%2]
		want = append(want, wantTransfer{chain.Transfer(usdt, payer, to, units(1)), to})
		chain.Mine(1)
	}
	node.Set(func(b *evmtest.Behavior) { b.MaxLogResults = 20 })
	got, err := scanAll(ctx, ep, evm.TransferQuery{Token: usdt, FromBlock: 1001, ToBlock: chain.Head().Number, Recipients: recipients})
	if err != nil {
		t.Fatal(err)
	}
	checkTransfers(t, got, want)
	checkCoverage(t, node.LogQueries(), 1001, chain.Head().Number)
}

func TestTransferLogsRecipientChunks(t *testing.T) {
	ctx := context.Background()
	chain, node, ep := newChain(t)
	recipients := make([]ethaddr.Address, 1700)
	for i := range recipients {
		recipients[i] = addr(i)
	}
	var want []wantTransfer
	for i := 0; i < 40; i++ {
		to := recipients[(i*97)%len(recipients)]
		want = append(want, wantTransfer{chain.Transfer(usdt, payer, to, units(3)), to})
		chain.Mine(1)
	}
	q := evm.TransferQuery{Token: usdt, FromBlock: 1001, ToBlock: chain.Head().Number,
		// Duplicates in the recipient list must not matter.
		Recipients: append(append([]ethaddr.Address{}, recipients...), recipients[:50]...)}

	// Default: 500 recipients per call -> 4 calls for 1700 addresses.
	got, err := scanAll(ctx, ep, q)
	if err != nil {
		t.Fatal(err)
	}
	checkTransfers(t, got, want)
	queries := node.LogQueries()
	if len(queries) != 4 {
		t.Fatalf("%d eth_getLogs calls, want 4", len(queries))
	}
	total := 0
	for _, lq := range queries {
		if lq.Recipients > evm.DefaultMaxRecipients {
			t.Fatalf("a call carried %d recipients", lq.Recipients)
		}
		total += lq.Recipients
	}
	if total != 1700 {
		t.Fatalf("calls carried %d recipients in total, want 1700 (deduplicated)", total)
	}

	// A provider that takes at most 120 topics per position, saying so in
	// three different ways (the last one unrecognisable).
	for name, topicsErr := range map[string]func(int) evmtest.RPCError{
		"recognised": nil,
		"http-ish":   func(int) evmtest.RPCError { return evmtest.RPCError{Code: -32000, Message: "request entity too large"} },
		"opaque":     func(int) evmtest.RPCError { return evmtest.RPCError{Code: -32000, Message: "bad filter"} },
	} {
		t.Run(name, func(t *testing.T) {
			_, node, ep := newChain(t)
			node.SetChain(chain)
			node.Set(func(b *evmtest.Behavior) { b.MaxTopics = 120; b.TopicsError = topicsErr })
			got, err := scanAll(ctx, ep, q)
			if err != nil {
				t.Fatal(err)
			}
			checkTransfers(t, got, want)
			if r := ep.Health().Logs.Recipients; r > 120 {
				t.Errorf("recipients per call = %d after learning", r)
			}
		})
	}
}

func TestTransferLogsUnavailable(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	chain := evmtest.NewChain(56, 1000)
	chain.AddToken(usdt, "USDT", 18)
	chain.Mine(50)
	node := evmtest.NewNode(chain)
	// The exact answer of bsc-dataseed.bnbchain.org to any eth_getLogs.
	node.Set(func(b *evmtest.Behavior) { b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"} })
	ep, _ := evm.NewEndpoint(node.Serve(t), evm.EndpointOptions{Now: clock.Now})
	q := evm.TransferQuery{Token: usdt, FromBlock: 1001, ToBlock: 1050, Recipients: []ethaddr.Address{addr(1), addr(2)}}

	_, err := scanAll(ctx, ep, q)
	if !evm.IsKind(err, evm.KindRPC) || !strings.Contains(err.Error(), "limit exceeded") {
		t.Fatalf("err = %v", err)
	}
	// Found out with a handful of calls, not a long halving sequence.
	if n := node.Calls("eth_getLogs"); n > 3 {
		t.Errorf("%d eth_getLogs calls to discover that logs are not served", n)
	}
	h := ep.Health()
	if !h.Logs.Unavailable || h.Logs.LastError == "" || h.Logs.Known {
		t.Errorf("logs health = %+v", h.Logs)
	}
	// The endpoint as such is fine: it still answers everything else.
	if h.ConsecutiveFailures != 0 || !h.CooldownUntil.IsZero() {
		t.Errorf("log refusal counted against general health: %+v", h)
	}
	if _, err := ep.BlockNumber(ctx); err != nil {
		t.Fatal(err)
	}

	// After the cooldown it is tried again, and recovers if the provider
	// changed its mind.
	clock.Advance(2 * time.Minute)
	if ep.Health().Logs.Unavailable {
		t.Fatal("still marked unavailable after the cooldown")
	}
	node.Set(func(b *evmtest.Behavior) { b.LogsError = nil })
	if _, err := scanAll(ctx, ep, q); err != nil {
		t.Fatal(err)
	}
	if h := ep.Health().Logs; !h.Known || h.Unavailable {
		t.Errorf("logs health after recovery = %+v", h)
	}

	// Rate limiting by HTTP status is the endpoint's problem as a whole.
	node.Set(func(b *evmtest.Behavior) { b.RateLimited = true; b.RetryAfter = 30 })
	_, err = scanAll(ctx, ep, q)
	if !evm.IsKind(err, evm.KindRateLimited) {
		t.Fatalf("err = %v", err)
	}
	h = ep.Health()
	if h.ConsecutiveFailures != 1 || h.CooldownUntil.Sub(clock.Now()) != 30*time.Second {
		t.Errorf("health after 429 = %+v", h)
	}

	// Invalid queries are rejected locally.
	if _, err := scanAll(ctx, ep, evm.TransferQuery{Token: usdt, FromBlock: 5, ToBlock: 4, Recipients: q.Recipients}); err == nil {
		t.Error("inverted range accepted")
	}
	if _, err := scanAll(ctx, ep, evm.TransferQuery{Token: usdt, FromBlock: 4, ToBlock: 5}); err == nil {
		t.Error("empty recipient set accepted")
	}
}

// TestTransferLogsRandomised: random chains, random provider limits, random
// query windows — the scanner must always return exactly the ground truth.
func TestTransferLogsRandomised(t *testing.T) {
	ctx := context.Background()
	for seed := uint64(0); seed < 30; seed++ {
		rng := rand.New(rand.NewPCG(seed, 99))
		chain := evmtest.NewChain(56, 1000)
		chain.AddToken(usdt, "USDT", 18)
		recipients := make([]ethaddr.Address, 1+rng.IntN(40))
		for i := range recipients {
			recipients[i] = addr(i)
		}
		want := populate(chain, rng, 100+rng.IntN(400), recipients)
		node := evmtest.NewNode(chain)
		node.Set(func(b *evmtest.Behavior) {
			if rng.IntN(2) == 0 {
				b.MaxLogRange = uint64(1 + rng.IntN(200))
			}
			if rng.IntN(3) == 0 {
				b.MaxLogResults = 1 + rng.IntN(30)
			}
			if rng.IntN(3) == 0 {
				b.MaxTopics = 1 + rng.IntN(len(recipients))
			}
			b.NoBatch = rng.IntN(2) == 0
		})
		ep, _ := evm.NewEndpoint(node.Serve(t), evm.EndpointOptions{
			Logs: evm.LogOptions{InitialRange: uint64(1 + rng.IntN(600)), MaxRecipients: 1 + rng.IntN(60)},
		})
		got, err := scanAll(ctx, ep, evm.TransferQuery{Token: usdt, FromBlock: 1000, ToBlock: chain.Head().Number, Recipients: recipients})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		checkTransfers(t, got, want)
	}
}

// TestTransferLogsCoverage: the scanner reports how far its answer can be
// trusted when the backend answering log queries is behind.
func TestTransferLogsCoverage(t *testing.T) {
	ctx := context.Background()
	chain, node, ep := newChain(t)
	recipients := []ethaddr.Address{addr(1), addr(2)}
	rng := rand.New(rand.NewPCG(7, 7))
	want := populate(chain, rng, 60, recipients)
	head := chain.Head().Number
	q := evm.TransferQuery{Token: usdt, FromBlock: 1001, ToBlock: head, Recipients: recipients}

	// Healthy: complete, and the backend's height is known.
	scan, err := ep.TransferLogs(ctx, q)
	if err != nil || !scan.Complete(q) || scan.Next != head+1 || scan.BackendHead != head {
		t.Fatalf("scan = next %d backend %d, err %v", scan.Next, scan.BackendHead, err)
	}
	checkTransfers(t, scan.Transfers, want)

	// The log backend is 7 blocks behind the head the caller knows.
	node.Set(func(b *evmtest.Behavior) { b.LogsBackendLag = 7 })
	scan, err = ep.TransferLogs(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if scan.Complete(q) || scan.Next != head-7+1 || scan.BackendHead != head-7 {
		t.Fatalf("scan = next %d backend %d; want next %d backend %d", scan.Next, scan.BackendHead, head-6, head-7)
	}
	var prefix []wantTransfer
	for i, tr := range scan.Transfers {
		if tr.BlockNumber >= scan.Next {
			t.Fatalf("transfer in block %d returned beyond the covered range", tr.BlockNumber)
		}
		prefix = append(prefix, want[i])
	}
	checkTransfers(t, scan.Transfers, prefix)
	if ep.Health().ConsecutiveFailures != 0 {
		t.Error("a lagging log backend within the range counted as a failure")
	}

	// A range entirely above the log backend is an error, not an empty
	// answer.
	scan, err = ep.TransferLogs(ctx, evm.TransferQuery{Token: usdt, FromBlock: head - 3, ToBlock: head, Recipients: recipients})
	if !evm.IsKind(err, evm.KindLagging) || scan.Next != head-3 || len(scan.Transfers) != 0 {
		t.Fatalf("scan = %+v, err = %v", scan, err)
	}
	if ep.Health().ConsecutiveFailures != 1 {
		t.Error("an endpoint behind the requested range was not marked")
	}

	// Without batches the height cannot be observed: the documented limit
	// of the check. The answer then looks complete; BackendHead says that
	// nobody vouched for it.
	node.Set(func(b *evmtest.Behavior) { b.NoBatch = true })
	scan, err = ep.TransferLogs(ctx, q)
	if err != nil || !scan.Complete(q) || scan.BackendHead != 0 {
		t.Fatalf("scan = next %d backend %d, err %v", scan.Next, scan.BackendHead, err)
	}
}

// TestTransferLogsKeepsPrefixOnError: a scan that fails part-way returns what
// it had covered, so the caller can save progress.
func TestTransferLogsKeepsPrefixOnError(t *testing.T) {
	ctx := context.Background()
	chain, node, ep := newChain(t)
	recipients := []ethaddr.Address{addr(1), addr(2)}
	rng := rand.New(rand.NewPCG(3, 3))
	want := populate(chain, rng, 200, recipients)
	served := 0
	node.Set(func(b *evmtest.Behavior) {
		b.MaxLogRange = 10
		b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method == "eth_getLogs" {
				if served++; served > 8 {
					return nil, &evmtest.RPCError{Code: 429, Message: "too many requests"}, true
				}
			}
			return nil, nil, false
		}
	})
	q := evm.TransferQuery{Token: usdt, FromBlock: 1001, ToBlock: chain.Head().Number, Recipients: recipients}
	scan, err := ep.TransferLogs(ctx, q)
	if !evm.IsKind(err, evm.KindRateLimited) {
		t.Fatalf("err = %v", err)
	}
	if scan.Next <= q.FromBlock || scan.Complete(q) {
		t.Fatalf("no usable prefix: next = %d", scan.Next)
	}
	var prefix []wantTransfer
	for i := range scan.Transfers {
		prefix = append(prefix, want[i])
	}
	checkTransfers(t, scan.Transfers, prefix)
	for _, tr := range scan.Transfers {
		if tr.BlockNumber >= scan.Next {
			t.Fatalf("transfer in block %d beyond the covered prefix (next %d)", tr.BlockNumber, scan.Next)
		}
	}
	// Every expected transfer below Next is there: the prefix has no holes.
	blk := func(i int) uint64 {
		r, err := ep.TransactionReceipt(ctx, want[i].tx)
		if err != nil {
			t.Fatal(err)
		}
		return r.BlockNumber
	}
	node.Set(func(b *evmtest.Behavior) { b.Intercept = nil })
	if len(scan.Transfers) < len(want) && blk(len(scan.Transfers)) < scan.Next {
		t.Fatalf("transfer %d (block %d) is missing from a prefix that claims to reach block %d", len(scan.Transfers), blk(len(scan.Transfers)), scan.Next-1)
	}
	// Continuing from Next completes the picture.
	rest, err := ep.TransferLogs(ctx, evm.TransferQuery{Token: usdt, FromBlock: scan.Next, ToBlock: q.ToBlock, Recipients: recipients})
	if err != nil || !rest.Complete(q) {
		t.Fatalf("continuation: %v", err)
	}
	checkTransfers(t, append(scan.Transfers, rest.Transfers...), want)
}

// TestTransferLogsRateLimitedInBand: a throttling message inside a 200
// response (bsc.blockrazor.xyz does this) ends the scan at once, counts
// against the endpoint, and is not mistaken for "eth_getLogs unsupported".
func TestTransferLogsRateLimitedInBand(t *testing.T) {
	ctx := context.Background()
	chain, node, ep := newChain(t)
	chain.Mine(100)
	node.Set(func(b *evmtest.Behavior) { b.LogsError = &evmtest.RPCError{Code: 429, Message: "too many requests"} })
	_, err := ep.TransferLogs(ctx, evm.TransferQuery{Token: usdt, FromBlock: 1001, ToBlock: 1100, Recipients: []ethaddr.Address{addr(1), addr(2)}})
	if !evm.IsKind(err, evm.KindRateLimited) {
		t.Fatalf("err = %v", err)
	}
	if n := node.Calls("eth_getLogs"); n != 1 {
		t.Errorf("%d calls after being told to slow down", n)
	}
	h := ep.Health()
	if h.ConsecutiveFailures != 1 || h.CooldownUntil.IsZero() || h.Logs.Unavailable {
		t.Errorf("health = %+v", h)
	}
}

// TestTransferLogsConflictingAnswers: two logs for one position in a block,
// or logs from two blocks at one height, are not a description of one chain.
func TestTransferLogsConflictingAnswers(t *testing.T) {
	ctx := context.Background()
	recipient := addr(1)
	hashA, hashB := "0x"+strings.Repeat("aa", 32), "0x"+strings.Repeat("bb", 32)
	logJSON := func(blockHash, txHash, logIndex, amount string) string {
		return fmt.Sprintf(`{"address":%q,"topics":[%q,%q,%q],"data":"0x%064s","blockNumber":"0x64","blockHash":%q,"transactionHash":%q,"transactionIndex":"0x0","logIndex":%q,"removed":false}`,
			usdt.Hex(), evm.TransferTopic.String(), "0x"+strings.Repeat("00", 32), "0x"+strings.Repeat("00", 12)+recipient.Hex()[2:],
			amount, blockHash, txHash, logIndex)
	}
	cases := map[string]string{
		"same position twice":           "[" + logJSON(hashA, hashA, "0x0", "5") + "," + logJSON(hashA, hashA, "0x0", "6") + "]",
		"exact duplicate":               "[" + logJSON(hashA, hashA, "0x0", "5") + "," + logJSON(hashA, hashA, "0x0", "5") + "]",
		"two blocks at the same height": "[" + logJSON(hashA, hashA, "0x0", "5") + "," + logJSON(hashB, hashB, "0x1", "5") + "]",
	}
	for name, body := range cases {
		ep := scripted(t, evm.EndpointOptions{}, func(w http.ResponseWriter, method string, id json.RawMessage) {
			if method == "eth_blockNumber" {
				reply(w, id, `"0x100"`)
				return
			}
			reply(w, id, body)
		})
		scan, err := ep.TransferLogs(ctx, evm.TransferQuery{Token: usdt, FromBlock: 100, ToBlock: 100, Recipients: []ethaddr.Address{recipient}})
		if !evm.IsKind(err, evm.KindBadResponse) || len(scan.Transfers) != 0 || scan.Next != 100 {
			t.Errorf("%s: scan = %+v, err = %v", name, scan, err)
		}
	}
	// Two logs of one transaction at different positions are fine.
	ep := scripted(t, evm.EndpointOptions{}, func(w http.ResponseWriter, _ string, id json.RawMessage) {
		reply(w, id, "["+logJSON(hashA, hashA, "0x0", "5")+","+logJSON(hashA, hashA, "0x1", "5")+"]")
	})
	scan, err := ep.TransferLogs(ctx, evm.TransferQuery{Token: usdt, FromBlock: 100, ToBlock: 100, Recipients: []ethaddr.Address{recipient}})
	if err != nil || len(scan.Transfers) != 2 {
		t.Fatalf("scan = %+v, err = %v", scan, err)
	}
}

// TestDataseedBehaviour reproduces bsc-dataseed.bnbchain.org exactly as
// recorded: a batch containing eth_getLogs is answered with a single
// unattributable error, a plain eth_getLogs with -32005 "limit exceeded", and
// everything else works. The endpoint must end up "healthy, without logs" —
// it is the natural cross-check endpoint and must not be penalised for what
// it does not offer.
func TestDataseedBehaviour(t *testing.T) {
	ctx := context.Background()
	chain, node, ep := newChain(t)
	chain.Mine(50)
	tx := chain.Transfer(usdt, payer, addr(1), units(5))
	chain.Mine(1)
	node.Set(func(b *evmtest.Behavior) {
		b.LogsBatchError = &evmtest.RPCError{Code: -32005, Message: "method eth_getLogs in batch triggered rate limit"}
		b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"}
	})
	q := evm.TransferQuery{Token: usdt, FromBlock: 1001, ToBlock: chain.Head().Number, Recipients: []ethaddr.Address{addr(1), addr(2)}}
	_, err := ep.TransferLogs(ctx, q)
	if !evm.IsKind(err, evm.KindRPC) || !strings.Contains(err.Error(), "limit exceeded") {
		t.Fatalf("err = %v", err)
	}
	h := ep.Health()
	if h.ConsecutiveFailures != 0 || !h.CooldownUntil.IsZero() || !h.Logs.Unavailable {
		t.Fatalf("health = %+v", h)
	}
	// Headers and receipts still go out as batches.
	node.ResetCounters()
	if _, err := ep.Head(ctx, evm.HeadRequest{Finalized: true, Numbers: []uint64{1001}}); err != nil {
		t.Fatal(err)
	}
	rs, err := ep.TransactionReceipts(ctx, []evm.Hash{tx, {1}, {2}})
	if err != nil || rs[0] == nil || rs[1] != nil {
		t.Fatalf("receipts = %v, %v", rs, err)
	}
	if node.Requests() != 2 {
		t.Errorf("%d HTTP requests for one head batch and one receipt batch", node.Requests())
	}
	// The setup wizard sees: works, but no log access.
	probe := evm.Probe(ctx, ep.URL(), 56, usdt)
	if probe.Verdict != evm.ProbeNoLogs || !strings.Contains(probe.Logs.Error, "limit exceeded") {
		t.Fatalf("probe = %+v", probe)
	}
}

// TestTransferLogsBeyondHead: current BSC nodes refuse a range that ends
// above the answering backend's head. That is the normal state of affairs at
// the tip of a load-balanced endpoint, so it must end the scan at that
// backend's height — without an error, without marking the endpoint, and
// without "learning" a smaller range.
func TestTransferLogsBeyondHead(t *testing.T) {
	ctx := context.Background()
	for _, batches := range []bool{true, false} {
		chain, node, ep := newChain(t)
		recipients := []ethaddr.Address{addr(1), addr(2)}
		rng := rand.New(rand.NewPCG(11, 11))
		want := populate(chain, rng, 60, recipients)
		head := chain.Head().Number
		node.Set(func(b *evmtest.Behavior) {
			b.RefuseBeyondHead = true
			b.LogsBackendLag = 4
			b.NoBatch = !batches
		})
		q := evm.TransferQuery{Token: usdt, FromBlock: 1001, ToBlock: head, Recipients: recipients}
		scan, err := ep.TransferLogs(ctx, q)
		if err != nil {
			t.Fatalf("batches=%v: %v", batches, err)
		}
		if scan.Next != head-4+1 || scan.BackendHead != head-4 {
			t.Fatalf("batches=%v: next %d backend %d; want %d and %d", batches, scan.Next, scan.BackendHead, head-3, head-4)
		}
		for i, tr := range scan.Transfers {
			if tr.TxHash != want[i].tx || tr.BlockNumber >= scan.Next {
				t.Fatalf("batches=%v: transfer %d wrong or beyond coverage", batches, i)
			}
		}
		h := ep.Health()
		if h.ConsecutiveFailures != 0 || h.Logs.Unavailable || h.Logs.Range != evm.DefaultLogRange {
			t.Fatalf("batches=%v: health = %+v", batches, h)
		}
		// Entirely above the backend's head: an error of kind lagging.
		_, err = ep.TransferLogs(ctx, evm.TransferQuery{Token: usdt, FromBlock: head - 1, ToBlock: head, Recipients: recipients})
		if !evm.IsKind(err, evm.KindLagging) {
			t.Fatalf("batches=%v: err = %v", batches, err)
		}
		if got := ep.Health().Logs.Range; got != evm.DefaultLogRange {
			t.Fatalf("batches=%v: range shrank to %d", batches, got)
		}
	}
}

// TestKnownLogEndpointIsThrottledNotWrittenOff: BSC's nodes
// without a log service answer eth_getLogs with a bare -32005 "limit
// exceeded" — and so does a provider that throttles. On an endpoint that has
// never answered a log query the scanner concludes "no log service"; on one
// that has (or that the caller knows to have: MarkLogsKnown), the same answer
// is a rate limit: the endpoint keeps counting as a log endpoint and cools
// down like any throttled one.
func TestKnownLogEndpointIsThrottledNotWrittenOff(t *testing.T) {
	throttle := func(b *evmtest.Behavior) { b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"} }
	query := func(c *evmtest.Chain) evm.TransferQuery {
		return evm.TransferQuery{Token: usdt, FromBlock: c.Head().Number - 5, ToBlock: c.Head().Number, Recipients: []ethaddr.Address{addr(1)}}
	}
	setup := func(t *testing.T) (*evmtest.Chain, *evmtest.Node, *evm.Endpoint) {
		chain := evmtest.NewChain(56, 1000)
		chain.AddToken(usdt, "USDT", 18)
		chain.Mine(20)
		node := evmtest.NewNode(chain)
		ep, err := evm.NewEndpoint(node.Serve(t), evm.EndpointOptions{Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return chain, node, ep
	}

	t.Run("has served logs", func(t *testing.T) {
		chain, node, ep := setup(t)
		if _, err := ep.TransferLogs(t.Context(), query(chain)); err != nil {
			t.Fatal(err)
		}
		node.Set(throttle)
		_, err := ep.TransferLogs(t.Context(), query(chain))
		if !evm.IsKind(err, evm.KindRateLimited) {
			t.Fatalf("err = %v, want a rate limit", err)
		}
		h := ep.Health()
		if h.Logs.Unavailable || !h.Logs.Known || h.ConsecutiveFailures == 0 || h.CooldownUntil.IsZero() || h.Logs.LastError == "" {
			t.Fatalf("health = %+v", h)
		}
		node.Set(func(b *evmtest.Behavior) { b.LogsError = nil })
		if _, err := ep.TransferLogs(t.Context(), query(chain)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("never has", func(t *testing.T) {
		chain, node, ep := setup(t)
		node.Set(throttle)
		if _, err := ep.TransferLogs(t.Context(), query(chain)); err == nil || evm.IsKind(err, evm.KindRateLimited) {
			t.Fatalf("err = %v", err)
		}
		if h := ep.Health(); !h.Logs.Unavailable || h.Logs.Known {
			t.Fatalf("health = %+v", h)
		}
	})

	t.Run("known to the caller", func(t *testing.T) {
		chain, node, ep := setup(t)
		node.Set(throttle)
		ep.MarkLogsKnown()
		if _, err := ep.TransferLogs(t.Context(), query(chain)); !evm.IsKind(err, evm.KindRateLimited) {
			t.Fatalf("err = %v, want a rate limit", err)
		}
		if h := ep.Health(); h.Logs.Unavailable || !h.Logs.Known {
			t.Fatalf("health = %+v", h)
		}
	})
}
