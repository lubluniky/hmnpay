package evm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
)

// Fuzz tests for everything in this package that parses bytes an RPC
// endpoint controls. The seeds are in the code and in testdata/fuzz, so the
// normal test run executes them as regression tests; to fuzz, for example:
//
//	go test ./internal/chain/evm -run '^$' -fuzz FuzzRPCResponse -fuzztime 3m
//
// The properties are the ones the watcher relies on: no input makes the
// client panic or loop, and whatever it returns without an error is what was
// asked for — a header with the requested number, a receipt for the
// requested transaction, transfers of the requested token to the requested
// recipients inside the requested range.

func FuzzHexDecoders(f *testing.F) {
	for _, s := range []string{
		"0x0", "0x1", "0xff", "0x00ff", "0xFFFFFFFFFFFFFFFF", "0x10000000000000000", "0x", "", "0X1f", "1f", "0x1g", " 0x1",
		"0x55d398326f99059ff775485246999027b3197955", "0x55d398326f99059fF775485246999027B3197955",
		"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef", "0x\x00", "0x-1", "0x+1", "0x१",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if v, err := decodeUint64(s); err == nil {
			// Re-encoding gives the canonical form of the same number.
			if back, err := decodeUint64(encodeUint64(v)); err != nil || back != v {
				t.Fatalf("decodeUint64(%q) = %d does not survive re-encoding", s, v)
			}
			want, ok := new(big.Int).SetString(strings.ToLower(s[2:]), 16)
			if !ok || !want.IsUint64() || want.Uint64() != v {
				t.Fatalf("decodeUint64(%q) = %d", s, v)
			}
		}
		if b, err := decodeData(s, 64); err == nil {
			if len(b) > 64 || !strings.EqualFold(encodeData(b), s) {
				t.Fatalf("decodeData(%q) = %x", s, b)
			}
		}
		if h, err := decodeHash(s); err == nil && !strings.EqualFold(h.String(), s) {
			t.Fatalf("decodeHash(%q) = %s", s, h)
		}
		if a, err := decodeAddress(s); err == nil && !strings.EqualFold(a.Hex(), s) {
			t.Fatalf("decodeAddress(%q) = %s", s, a.Hex())
		}
	})
}

const (
	fuzzHashA = "0x1111111111111111111111111111111111111111111111111111111111111111"
	fuzzHashB = "0x2222222222222222222222222222222222222222222222222222222222222222"
	fuzzHashC = "0x3333333333333333333333333333333333333333333333333333333333333333"
	fuzzToken = "0x55d398326f99059ff775485246999027b3197955"
	fuzzTo    = "0x000000000000000000000000f39fd6e51aad88f6f4ce6ab8827279cfffb92266"
	fuzzFrom  = "0x0000000000000000000000008894e0a0c962cb723c1976a4421c95949be2d4e3"
	fuzzTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
)

func fuzzHeader(number string) string {
	return `{"number":"` + number + `","hash":"` + fuzzHashA + `","parentHash":"` + fuzzHashB + `","timestamp":"0x6553f100","transactions":[]}`
}

func fuzzLog(block, index string) string {
	return `{"address":"` + fuzzToken + `","topics":["` + fuzzTopic + `","` + fuzzFrom + `","` + fuzzTo + `"],` +
		`"data":"0x0000000000000000000000000000000000000000000000000de0b6b3a7640000","blockNumber":"` + block + `",` +
		`"blockHash":"` + fuzzHashA + `","transactionHash":"` + fuzzHashC + `","transactionIndex":"0x1","logIndex":"` + index + `","removed":false}`
}

func fuzzReceipt(status string) string {
	return `{"transactionHash":"` + fuzzHashC + `","blockHash":"` + fuzzHashA + `","blockNumber":"0xf","status":"` + status + `","logs":[` + fuzzLog("0xf", "0x0") + `]}`
}

func FuzzDecodeHeader(f *testing.F) {
	for _, s := range []string{
		fuzzHeader("0xa"), "null", "{}", `{"number":"0xa"}`, `{"number":null,"hash":null,"parentHash":null,"timestamp":null}`,
		`{"number":"0xa","hash":"0x00","parentHash":"` + fuzzHashB + `","timestamp":"0x1"}`,
		`{"number":"0xffffffffffffffffff","hash":"` + fuzzHashA + `","parentHash":"` + fuzzHashB + `","timestamp":"0x1"}`,
		`{"number":"0xa","hash":"0x0000000000000000000000000000000000000000000000000000000000000000","parentHash":"` + fuzzHashB + `","timestamp":"0x1"}`,
		`[]`, `"0xa"`, `{"number":10}`, ``, `{"number":"0xa","number":"0xb"}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		h, err := decodeHeader(raw)
		if err != nil {
			return
		}
		if h == nil || h.Hash.IsZero() {
			t.Fatalf("decodeHeader(%q) accepted a header without hash", raw)
		}
	})
}

func FuzzDecodeReceipt(f *testing.F) {
	for _, s := range []string{
		fuzzReceipt("0x1"), fuzzReceipt("0x0"), fuzzReceipt("0x2"), "null", "{}", `{"transactionHash":"` + fuzzHashA + `"}`,
		`{"transactionHash":"` + fuzzHashC + `","blockHash":"` + fuzzHashA + `","blockNumber":"0xf","status":"0x1","logs":[` + fuzzLog("0x10", "0x0") + `]}`,
		`{"transactionHash":"` + fuzzHashC + `","blockHash":"` + fuzzHashA + `","blockNumber":"0xf","status":"0x1","logs":null}`,
		`{"transactionHash":"` + fuzzHashC + `","blockHash":"` + fuzzHashA + `","blockNumber":"0xf","status":"0x1","logs":[{}]}`,
	} {
		f.Add([]byte(s))
	}
	want, _ := ParseHash(fuzzHashC)
	f.Fuzz(func(t *testing.T, raw []byte) {
		r, err := decodeReceipt(raw, want)
		if err != nil {
			return
		}
		if r == nil || r.TxHash != want || r.Status > 1 {
			t.Fatalf("decodeReceipt(%q) = %+v", raw, r)
		}
		for _, l := range r.Logs {
			if l.TxHash != want || l.BlockHash != r.BlockHash || l.BlockNumber != r.BlockNumber || len(l.Topics) > 4 {
				t.Fatalf("receipt log %+v does not belong to receipt %+v", l, r)
			}
			// What the watcher does with it.
			if tr, err := DecodeTransfer(l); err == nil && (tr.Amount == nil || tr.Amount.Sign() < 0 || tr.Amount.BitLen() > 256) {
				t.Fatalf("DecodeTransfer accepted amount %v", tr.Amount)
			}
		}
	})
}

func FuzzDecodeSymbol(f *testing.F) {
	word := func(b ...byte) []byte { return append(make([]byte, 32-len(b)), b...) }
	f.Add(append(append(word(32), word(4)...), append([]byte("USDT"), make([]byte, 28)...)...))
	f.Add(append([]byte("USDT"), make([]byte, 28)...))
	f.Add(append(append(word(32), word(0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)...), make([]byte, 32)...))
	f.Add(append(append(word(64), word(4)...), make([]byte, 32)...))
	f.Add([]byte{})
	f.Add(make([]byte, 31))
	f.Fuzz(func(t *testing.T, out []byte) {
		s, err := decodeSymbol(out)
		if err != nil {
			return
		}
		if s == "" || len(s) > maxSymbolLen || !utf8.ValidString(s) || strings.ContainsAny(s, "\x00\n\r\x1b") {
			t.Fatalf("decodeSymbol accepted %q", s)
		}
	})
}

func FuzzErrorMessages(f *testing.F) {
	for _, m := range []string{
		"block range extends beyond current head block: requested 134262128, head 134262118",
		"limited to 0 - 50 blocks range", "exceed maximum block range: 5000", "max is 1k blocks",
		"query returned more than 10000 results", "eth_getLogs is limited to a 10,000 range", "rate limit exceeded",
		"head: 99999999999999999999999", "0x1f4 blocks", "1_000 blocks", "limit exceeded", "",
		"range 18446744073709551615k", "head 0", strings.Repeat("9", 400), "head\t12 head 13",
		"\x1b[31mexecution reverted\x1b[0m\x00", "bad \xff\xfe utf-8", strings.Repeat("é", 400),
	} {
		f.Add("eth_getLogs", -32005, m, uint64(5000))
	}
	f.Fuzz(func(t *testing.T, method string, code int, message string, span uint64) {
		_ = classifyRPCError(method, code, message)
		_, _ = laggingHead(message)
		if hint := rangeHint(message, span); hint != 0 && hint >= span {
			t.Fatalf("rangeHint(%q, %d) = %d, not below the span that failed", message, span, hint)
		}
		// The message ends up in errors, logs and the dashboard.
		e := &Error{Kind: KindRPC, Endpoint: "http://example.invalid", Method: "eth_getLogs", Code: code, Message: cleanMessage(message)}
		if s := e.Error(); len(s) > 4096 || !utf8.ValidString(s) || strings.ContainsAny(s, "\n\r\x00\x1b") {
			t.Fatalf("error text is not fit for a log line: %q", s)
		}
	})
}

// stubTransport answers every HTTP request with the same status and body.
type stubTransport struct {
	status int
	body   []byte
	calls  int
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	if s.calls > 200 {
		return nil, errors.New("too many requests for one call")
	}
	return &http.Response{
		StatusCode: s.status, Status: http.StatusText(s.status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(s.body)),
		ContentLength: int64(len(s.body)), Request: req,
	}, nil
}

// FuzzRPCResponse feeds an arbitrary HTTP response to every call the watcher
// and the prober make. A fresh endpoint is used for each call, so the first
// request carries the JSON-RPC id 1 (batches: 1, 2, 3 …) and seeds that are
// well-formed answers get through the envelope to the decoders.
func FuzzRPCResponse(f *testing.F) {
	single := func(result string) string { return `{"jsonrpc":"2.0","id":1,"result":` + result + `}` }
	batch := func(results ...string) string {
		var b strings.Builder
		b.WriteString("[")
		for i, r := range results {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"jsonrpc":"2.0","id":` + string(rune('1'+i)) + `,"result":` + r + `}`)
		}
		b.WriteString("]")
		return b.String()
	}
	seeds := []string{
		single(`"0x38"`), single(`"0xa"`), single(fuzzHeader("0xa")), single(fuzzHeader("0xf")), single("null"),
		single("[" + fuzzLog("0xa", "0x0") + "," + fuzzLog("0xb", "0x1") + "]"), single("[]"),
		single(fuzzReceipt("0x1")), single(`"0x0000000000000000000000000000000000000000000000000000000000000012"`),
		batch(fuzzHeader("0x14"), fuzzHeader("0x12"), fuzzHeader("0xa")), batch(`"0x14"`, "["+fuzzLog("0xa", "0x0")+"]"),
		batch(fuzzReceipt("0x1"), "null"), batch(fuzzHeader("0x14"), "null", "null"),
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"limited to 0 - 50 blocks range"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"block range extends beyond current head block: requested 20, head 12"}}`,
		`[{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"batch not allowed"}}]`,
		`{"jsonrpc":"2.0","id":2,"result":"0x1"}`, `{"jsonrpc":"2.0","id":"1","result":"0x1"}`, `{"id":1}`, `[]`, `[[]]`, `null`, ``,
		`<html><body>502 Bad Gateway</body></html>`, `{"jsonrpc":"2.0","id":1,"result":"0x1"}{"jsonrpc":"2.0","id":1,"result":"0x2"}`,
		`[{"jsonrpc":"2.0","id":1,"result":"0x14"},{"jsonrpc":"2.0","id":1,"result":[]}]`,
		`{"jsonrpc":"2.0","id":1,"result":[{"address":"` + fuzzToken + `","topics":[],"data":"0x","blockNumber":"0xa","blockHash":"` + fuzzHashA + `","transactionHash":"` + fuzzHashC + `","logIndex":"0x0"}]}`,
	}
	for _, s := range seeds {
		f.Add(200, []byte(s))
	}
	f.Add(429, []byte(`{"error":"rate limited"}`))
	f.Add(500, []byte(single(`"0x1"`)))
	f.Add(301, []byte(``))

	token := ethaddr.MustParse(fuzzToken)
	recipient := ethaddr.MustParse("0x" + fuzzTo[26:])
	txA, _ := ParseHash(fuzzHashC)
	txB, _ := ParseHash(fuzzHashB)

	f.Fuzz(func(t *testing.T, status int, body []byte) {
		if status < 100 || status > 599 {
			status = 200
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		fresh := func() *Endpoint {
			e, err := NewEndpoint("http://rpc.invalid/key", EndpointOptions{
				HTTPClient: &http.Client{Transport: &stubTransport{status: status, body: body}},
				Timeout:    5 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			return e
		}

		if h, err := fresh().HeaderByNumber(ctx, 10); err == nil && (h.Number != 10 || h.Hash.IsZero()) {
			t.Fatalf("HeaderByNumber(10) = %+v", h)
		}
		if h, err := fresh().HeaderByTag(ctx, TagLatest); err == nil && h.Hash.IsZero() {
			t.Fatalf("HeaderByTag = %+v", h)
		}
		if hr, err := fresh().Head(ctx, HeadRequest{Finalized: true, Numbers: []uint64{10, 12}}); err == nil {
			if hr.Latest.Hash.IsZero() || (hr.Finalized != nil && hr.Finalized.Hash.IsZero()) {
				t.Fatalf("Head = %+v", hr)
			}
			for n, h := range hr.ByNumber {
				if (n != 10 && n != 12) || h.Number != n || h.Hash.IsZero() {
					t.Fatalf("Head returned header %+v for number %d", h, n)
				}
			}
		}
		_, _ = fresh().BlockNumber(ctx)
		_, _ = fresh().ChainID(ctx)
		_, _ = fresh().Decimals(ctx, token)
		_, _ = fresh().Code(ctx, token)
		if s, err := fresh().Symbol(ctx, token); err == nil && (s == "" || len(s) > maxSymbolLen) {
			t.Fatalf("Symbol = %q", s)
		}
		if b, err := fresh().BalanceOf(ctx, token, recipient); err == nil && (b == nil || b.Sign() < 0 || b.BitLen() > 256) {
			t.Fatalf("BalanceOf = %v", b)
		}
		if rs, err := fresh().TransactionReceipts(ctx, []Hash{txA, txB}); err == nil {
			if len(rs) != 2 {
				t.Fatalf("TransactionReceipts returned %d receipts for 2 hashes", len(rs))
			}
			for i, want := range []Hash{txA, txB} {
				if rs[i] != nil && rs[i].TxHash != want {
					t.Fatalf("receipt %d is for %s, asked for %s", i, rs[i].TxHash, want)
				}
			}
		}
		q := TransferQuery{Token: token, FromBlock: 10, ToBlock: 20, Recipients: []ethaddr.Address{recipient}}
		scan, err := fresh().TransferLogs(ctx, q)
		if scan.Next < q.FromBlock || scan.Next > q.ToBlock+1 {
			t.Fatalf("TransferLogs: next = %d, err = %v", scan.Next, err)
		}
		// Without an error the scan covers the whole range — or ends at
		// the height of the backend that answered, where the endpoint
		// stated one below the end of the range. That is a result, not a
		// failure (a node cannot have logs of blocks it has not got), and
		// it is the caller's business to go on from Next: the watcher does
		// so in all three places it scans (TestScanCallersHonourNext in
		// the watcher package). What must never happen is a short scan
		// that nothing the endpoint said accounts for.
		if err == nil && scan.Next != q.ToBlock+1 && (scan.BackendHead == 0 || scan.Next != scan.BackendHead+1) {
			t.Fatalf("TransferLogs: next = %d of %d..%d without an error, and the backend's height (%d) does not explain it",
				scan.Next, q.FromBlock, q.ToBlock, scan.BackendHead)
		}
		if err == nil && scan.BackendHead != 0 && scan.BackendHead < q.ToBlock && scan.Next > scan.BackendHead+1 {
			t.Fatalf("TransferLogs: next = %d, beyond the answering backend's height %d", scan.Next, scan.BackendHead)
		}
		for _, tr := range scan.Transfers {
			if tr.Token != token || tr.To != recipient || tr.BlockNumber < q.FromBlock || tr.BlockNumber >= scan.Next ||
				tr.BlockHash.IsZero() || tr.TxHash.IsZero() || tr.Amount == nil || tr.Amount.Sign() < 0 || tr.Amount.BitLen() > 256 {
				t.Fatalf("TransferLogs returned a transfer that was not asked for: %+v", tr)
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("the calls did not finish: %v", ctx.Err())
		}
	})
}
