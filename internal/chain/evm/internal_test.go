package evm

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
)

func TestDecodeUint64(t *testing.T) {
	good := map[string]uint64{
		"0x0":                0,
		"0x1":                1,
		"0x38":               56,
		"0X38":               56,
		"0xff":               255,
		"0xFF":               255,
		"0x8007abc":          134249148,
		"0xffffffffffffffff": 1<<64 - 1,
		"0x00":               0, // leading zeros tolerated
		"0x0000000000000000000000000000000000000000000000000000000000000038": 56,
	}
	for in, want := range good {
		got, err := decodeUint64(in)
		if err != nil || got != want {
			t.Errorf("decodeUint64(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	bad := []string{"", "0x", "38", "56", "x38", "0x3g", "0x 38", " 0x38", "0x38 ", "-0x1", "0x-1", "0x+1",
		"0x10000000000000000", "0x1ffffffffffffffff", "0x0x1", "0x1.0", "1e3", "0x१"}
	for _, in := range bad {
		if v, err := decodeUint64(in); err == nil {
			t.Errorf("decodeUint64(%q) = %d, want error", in, v)
		}
	}
}

func TestDecodeDataAndHash(t *testing.T) {
	if b, err := decodeData("0x", 10); err != nil || len(b) != 0 {
		t.Errorf("empty data: %v %v", b, err)
	}
	if b, err := decodeData("0x00ff", 10); err != nil || !bytes.Equal(b, []byte{0, 0xff}) {
		t.Errorf("data: %v %v", b, err)
	}
	for _, in := range []string{"", "0", "00ff", "0x0", "0xf", "0xgg", "0x00 ", "0x0011223344556677889900"} {
		if _, err := decodeData(in, 10); err == nil {
			t.Errorf("decodeData(%q) accepted", in)
		}
	}
	h := "0x" + strings.Repeat("ab", 32)
	if got, err := ParseHash(h); err != nil || got.String() != h {
		t.Errorf("ParseHash: %v %v", got, err)
	}
	for _, in := range []string{"", "0x", h[:65], h + "0", h[2:], "0x" + strings.Repeat("zz", 32)} {
		if _, err := ParseHash(in); err == nil {
			t.Errorf("ParseHash(%q) accepted", in)
		}
	}
	var th Hash
	if err := th.UnmarshalText([]byte(h)); err != nil || th.IsZero() {
		t.Error("UnmarshalText")
	}
	if _, err := decodeAddress("0x55d398326f99059fF775485246999027B3197955"); err != nil {
		t.Errorf("checksummed address from node rejected: %v", err)
	}
	for _, in := range []string{"", "0x55d3", "55d398326f99059ff775485246999027b3197955", "0x55d398326f99059ff775485246999027b319795g"} {
		if _, err := decodeAddress(in); err == nil {
			t.Errorf("decodeAddress(%q) accepted", in)
		}
	}
}

func TestSelectorsAndTopic(t *testing.T) {
	if got := TransferTopic.String(); got != "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef" {
		t.Errorf("TransferTopic = %s", got)
	}
	for name, tc := range map[string]struct {
		got  []byte
		want string
	}{
		"decimals":  {selectorDecimals, "313ce567"},
		"symbol":    {selectorSymbol, "95d89b41"},
		"balanceOf": {selectorBalanceOf, "70a08231"},
	} {
		if hex.EncodeToString(tc.got) != tc.want {
			t.Errorf("%s selector = %x, want %s", name, tc.got, tc.want)
		}
	}
}

// TestClassifyRPCError uses error bodies recorded from real public BSC
// endpoints (November 2026) plus well-known messages of the large providers.
func TestClassifyRPCError(t *testing.T) {
	tests := []struct {
		method  string
		code    int
		message string
		want    ErrorKind
	}{
		// eth_getLogs range limits.
		{"eth_getLogs", -32602, "eth_getLogs is limited to 0 - 50 blocks range", KindRangeTooLarge},                  // 1rpc.io
		{"eth_getLogs", -32000, "log query range must not exceed 25 blocks", KindRangeTooLarge},                      // blockrazor
		{"eth_getLogs", -32602, "exceed maximum block range 5000", KindRangeTooLarge},                                // nownodes, 48.club
		{"eth_getLogs", 35, "ranges over 10000 blocks are not supported on free plan", KindRangeTooLarge},            // dRPC
		{"eth_getLogs", -32600, "You can make eth_getLogs requests with up to a 10 block range.", KindRangeTooLarge}, // Alchemy free
		{"eth_getLogs", -32614, "eth_getLogs is limited to a 10,000 range", KindRangeTooLarge},                       // QuickNode
		{"eth_getLogs", -32000, "block range is too wide", KindRangeTooLarge},                                        // Ankr
		{"eth_getLogs", -32000, "requested too many blocks from 1 to 100000, maximum is set to 2000", KindRangeTooLarge},
		{"eth_getLogs", -32005, "Exceeded max range limit for eth_getLogs: 2000", KindRangeTooLarge},
		// eth_getLogs result limits.
		{"eth_getLogs", -32005, "query returned more than 10000 results. Try with this block range [0x1, 0x2].", KindTooManyResults}, // Infura
		{"eth_getLogs", -32602, "Log response size exceeded. You can make eth_getLogs requests with ...", KindTooManyResults},
		{"eth_getLogs", -32000, "query timeout exceeded", KindTooManyResults},
		{"eth_getLogs", -32002, "request timed out", KindTooManyResults},
		// eth_getLogs filter size.
		{"eth_getLogs", -32602, "exceed max topics", KindRequestTooLarge},
		{"eth_getLogs", -32000, "too many topics in filter", KindRequestTooLarge},
		{"eth_getLogs", -32000, "filter criteria: at most 1000 addresses allowed", KindRequestTooLarge},
		// Not served.
		{"eth_getLogs", -32000, "The method eth_getLogs is not supported.", KindUnavailable}, // meowrpc
		{"eth_getLogs", -32012, `Method "eth_getLogs" is not available for unregistered accounts. Please visit https://zan.top/ and register to unlock more.`, KindUnavailable},
		{"eth_getLogs", -32601, "the method eth_getLogs does not exist/is not available", KindUnavailable},
		{"eth_chainId", -32000, "Unauthorized: You must authenticate your request with an API key.", KindUnavailable}, // Ankr
		// Throttling.
		{"eth_getLogs", -32005, "limit exceeded", KindRPC},         // bsc-dataseed answers every eth_getLogs so: ambiguous, probed
		{"eth_call", -32005, "limit exceeded", KindRateLimited},    // the same words elsewhere are throttling
		{"eth_getLogs", 429, "too many requests", KindRateLimited}, // blockrazor, HTTP 200
		{"eth_getLogs", 15, "You reached Public endpoint rate limit, please upgrade to paid plan", KindRateLimited},
		{"eth_getLogs", -32000, "Your request has been rate-limited due to unusually high traffic on the Alchemy public API.", KindRateLimited},
		{"eth_blockNumber", -32029, "Too Many Requests, Please apply an OnFinality API key", KindRateLimited},
		{"eth_call", 429, "whatever", KindRateLimited},
		{"eth_call", -32000, "exceeded its compute units per second capacity", KindRateLimited},
		// The range ends above the node's head (recorded from publicnode and 48.club).
		{"eth_getLogs", -32602, "block range extends beyond current head block: requested 134262128, head 134262118", KindLagging},
		{"eth_getLogs", -32602, "block range extends beyond current head block", KindLagging},
		// Ordinary node errors stay KindRPC.
		{"eth_call", 3, "execution reverted", KindRPC},
		{"eth_getLogs", -32602, "invalid block range params", KindRPC},
		{"eth_getLogs", -32000, "something nobody has seen before", KindRPC},
		{"eth_getBlockByNumber", -32000, "finalized block not found", KindRPC},
		{"eth_getBlockByNumber", -32602, "invalid argument 0: hex string without 0x prefix", KindRPC},
		// A range-sounding message on another method is not a log range limit.
		{"eth_call", -32000, "block range exceeds limit", KindRPC},
	}
	for _, tc := range tests {
		if got := classifyRPCError(tc.method, tc.code, tc.message); got != tc.want {
			t.Errorf("classify(%s, %d, %q) = %v, want %v", tc.method, tc.code, tc.message, got, tc.want)
		}
	}
}

func TestRangeHint(t *testing.T) {
	tests := []struct {
		message string
		failed  uint64
		want    uint64
	}{
		{"eth_getLogs is limited to 0 - 50 blocks range", 1000, 50},
		{"log query range must not exceed 25 blocks", 1000, 25},
		{"exceed maximum block range 5000", 50000, 5000},
		{"exceed maximum block range: 2000", 5000, 2000},
		{"ranges over 10000 blocks are not supported on free plan", 20000, 10000},
		{"eth_getLogs is limited to a 10,000 range", 20000, 10000},
		{"eth_getLogs range is too large, max is 1k blocks", 5000, 1000},
		{"You can make eth_getLogs requests with up to a 10 block range.", 1000, 10},
		// The stated limit is not below what failed: no usable hint.
		{"exceed maximum block range 5000", 1000, 0},
		{"block range is too wide", 1000, 0},
		// Hex block numbers and hashes are not limits.
		{"query returned more than 10000 results. Try with this block range [0x7ffb1c0, 0x7ffb1c5].", 500, 0},
		// Decimal block numbers far above the span are ignored.
		{"block range 134249000-134250000 exceeds limit of 800", 1001, 800},
		{"limited to 0 blocks", 1000, 0},
	}
	for _, tc := range tests {
		if got := rangeHint(tc.message, tc.failed); got != tc.want {
			t.Errorf("rangeHint(%q, %d) = %d, want %d", tc.message, tc.failed, got, tc.want)
		}
	}
}

func TestLaggingHead(t *testing.T) {
	for msg, want := range map[string]uint64{
		"block range extends beyond current head block: requested 134262128, head 134262118": 134262118,
		"toBlock is above head: 77":                     77,
		"block range extends beyond current head block": 0,
		"header not found":                              0,
		"head 0x10":                                     0,
	} {
		got, ok := laggingHead(msg)
		if got != want || ok != (want != 0) {
			t.Errorf("laggingHead(%q) = %d, %v; want %d", msg, got, ok, want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	tests := map[string]string{
		"https://bsc-dataseed.bnbchain.org":                         "https://bsc-dataseed.bnbchain.org",
		"https://bsc-dataseed.bnbchain.org/":                        "https://bsc-dataseed.bnbchain.org",
		"https://bsc-mainnet.nodereal.io/v1/0123456789abcdefSECRET": "https://bsc-mainnet.nodereal.io",
		"https://rpc.example.com?apikey=SECRET":                     "https://rpc.example.com",
		"https://user:SECRET@RPC.example.com.:443/path":             "https://rpc.example.com",
		"http://127.0.0.1:8545":                                     "http://127.0.0.1:8545",
		"::not a url::":                                             "invalid-url",
		"":                                                          "invalid-url",
	}
	for in, want := range tests {
		got := RedactURL(in)
		if got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "SECRET") {
			t.Errorf("RedactURL(%q) leaks the secret: %q", in, got)
		}
	}
	for _, bad := range []string{"", "ftp://host", "ws://host", "//host", "https://", "host:8545", "javascript:alert(1)", "file:///etc/passwd"} {
		if _, err := NewEndpoint(bad, EndpointOptions{}); err == nil {
			t.Errorf("NewEndpoint(%q) accepted", bad)
		}
	}
	if _, err := NewEndpoint("https://user:SECRET@host/%zz", EndpointOptions{}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("invalid URL error leaks or is nil: %v", err)
	}
}

func transferLog(token, from, to ethaddr.Address, amount *big.Int) Log {
	data := make([]byte, 32)
	amount.FillBytes(data)
	return Log{
		Address:     token,
		Topics:      []Hash{TransferTopic, addressTopic(from), addressTopic(to)},
		Data:        data,
		BlockNumber: 100,
		BlockHash:   Hash{1},
		TxHash:      Hash{2},
		LogIndex:    7,
	}
}

func TestDecodeTransfer(t *testing.T) {
	token := ethaddr.MustParse("0x55d398326f99059fF775485246999027B3197955")
	from := ethaddr.MustParse("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	to := ethaddr.MustParse("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	// 25.5 USDT at 18 decimals: does not fit in uint64-with-headroom
	// arithmetic of naive implementations once amounts grow, so check a
	// value above 2^64 as well.
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)

	for _, amount := range []*big.Int{big.NewInt(0), big.NewInt(1), huge,
		new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))} {
		tr, err := DecodeTransfer(transferLog(token, from, to, amount))
		if err != nil {
			t.Fatal(err)
		}
		if tr.Token != token || tr.From != from || tr.To != to || tr.Amount.Cmp(amount) != 0 ||
			tr.BlockNumber != 100 || tr.LogIndex != 7 || tr.BlockHash != (Hash{1}) || tr.TxHash != (Hash{2}) {
			t.Errorf("decoded %+v", tr)
		}
	}

	good := transferLog(token, from, to, big.NewInt(5))
	mutate := func(f func(l *Log)) Log {
		l := good
		l.Topics = append([]Hash(nil), good.Topics...)
		l.Data = append([]byte(nil), good.Data...)
		f(&l)
		return l
	}
	notTransfer := map[string]Log{
		"wrong topic0":         mutate(func(l *Log) { l.Topics[0][0] ^= 1 }),
		"approval topic":       mutate(func(l *Log) { l.Topics[0] = Hash(ethaddr.Keccak256([]byte("Approval(address,address,uint256)"))) }),
		"no topics":            mutate(func(l *Log) { l.Topics = nil }),
		"two topics":           mutate(func(l *Log) { l.Topics = l.Topics[:2] }),
		"four topics (ERC721)": mutate(func(l *Log) { l.Topics = append(l.Topics, Hash{}) }),
		"empty data":           mutate(func(l *Log) { l.Data = nil }),
		"31 bytes of data":     mutate(func(l *Log) { l.Data = l.Data[:31] }),
		"64 bytes of data":     mutate(func(l *Log) { l.Data = append(l.Data, make([]byte, 32)...) }),
		"dirty from padding":   mutate(func(l *Log) { l.Topics[1][0] = 1 }),
		"dirty to padding":     mutate(func(l *Log) { l.Topics[2][11] = 1 }),
	}
	for name, l := range notTransfer {
		if _, err := DecodeTransfer(l); !errors.Is(err, ErrNotTransfer) {
			t.Errorf("%s: err = %v, want ErrNotTransfer", name, err)
		}
	}
	if _, err := DecodeTransfer(mutate(func(l *Log) { l.Removed = true })); !errors.Is(err, ErrRemovedLog) {
		t.Errorf("removed log: %v", err)
	}
	// The decoder reports the emitting contract; it is not its job to
	// reject other tokens.
	other := ethaddr.MustParse("0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC")
	if tr, err := DecodeTransfer(mutate(func(l *Log) { l.Address = other })); err != nil || tr.Token != other {
		t.Errorf("other token: %v %v", tr, err)
	}
}

func TestDecodeSymbol(t *testing.T) {
	abiString := func(s string) []byte {
		out := make([]byte, 64)
		out[31] = 32
		out[63] = byte(len(s))
		padded := make([]byte, (len(s)+31)/32*32)
		copy(padded, s)
		return append(out, padded...)
	}
	if s, err := decodeSymbol(abiString("USDT")); err != nil || s != "USDT" {
		t.Errorf("abi string: %q %v", s, err)
	}
	b32 := make([]byte, 32)
	copy(b32, "MKR")
	if s, err := decodeSymbol(b32); err != nil || s != "MKR" {
		t.Errorf("bytes32: %q %v", s, err)
	}
	badLen := abiString("USDT")
	badLen[63] = 200
	badOffset := abiString("USDT")
	badOffset[31] = 64
	for name, in := range map[string][]byte{
		"empty":           nil,
		"odd length":      make([]byte, 33),
		"empty string":    abiString(""),
		"length overflow": badLen,
		"bad offset":      badOffset,
		"control chars":   abiString("US\x00DT"),
		"newline":         abiString("USDT\n<script>"),
		"invalid utf8":    abiString("\xff\xfe"),
		"too long":        abiString(strings.Repeat("A", 33)),
		"zero bytes32":    make([]byte, 32),
	} {
		if s, err := decodeSymbol(in); err == nil {
			t.Errorf("%s: accepted as %q", name, s)
		}
	}
}

func TestErrorFormatting(t *testing.T) {
	e := &Error{Kind: KindRateLimited, Endpoint: "https://host/…", Method: "eth_getLogs", HTTPStatus: 429, Code: 15, Message: "slow down"}
	want := "evm: https://host/…: eth_getLogs: rate_limited (HTTP 429) (code 15): slow down"
	if e.Error() != want {
		t.Errorf("Error() = %q\n          want %q", e.Error(), want)
	}
	if got := failureText(e); got != "eth_getLogs: rate_limited (HTTP 429) (code 15): slow down" {
		t.Errorf("failureText = %q", got)
	}
	if !IsKind(e, KindRateLimited) || IsKind(e, KindRPC) || IsKind(errors.New("x"), KindRPC) {
		t.Error("IsKind")
	}
	long := cleanMessage(strings.Repeat("a\n", 1000))
	if len(long) > maxMessageLen+8 || strings.Contains(long, "\n") {
		t.Errorf("cleanMessage did not bound/flatten: %d bytes", len(long))
	}
	for k := KindRPC; k <= KindRequestTooLarge; k++ {
		if k.String() == "unknown" {
			t.Errorf("kind %d has no name", k)
		}
	}
}
