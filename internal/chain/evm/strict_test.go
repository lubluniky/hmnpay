package evm

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
)

// stubEndpoint returns an endpoint whose every HTTP request is answered with
// the given body.
func stubEndpoint(t *testing.T, body string) *Endpoint {
	t.Helper()
	e, err := NewEndpoint("http://rpc.invalid/key", EndpointOptions{
		HTTPClient: &http.Client{Transport: &stubTransport{status: 200, body: []byte(body)}},
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestStrictResponses: what an endpoint sends counts only under the exact
// member names of JSON-RPC and the execution API, each at most once, and a
// result only in a response that says it is JSON-RPC 2.0.
//
// encoding/json reads `{"RESULT":…}` as a result and lets the second of two
// "hash" members win. No node answers like that (see the comment on
// rpcResponse for what the public BSC endpoints send), so nothing is lost by
// refusing — and a response can no longer mean two things.
func TestStrictResponses(t *testing.T) {
	ctx := context.Background()
	header := fuzzHeader("0xa")
	envelope := func(members string) string { return `{` + members + `}` }

	accepted := map[string]string{
		"plain":                        envelope(`"jsonrpc":"2.0","id":1,"result":` + header),
		"member order":                 envelope(`"result":` + header + `,"id":1,"jsonrpc":"2.0"`),
		"members this client ignores":  envelope(`"jsonrpc":"2.0","id":1,"result":` + strings.Replace(header, `{`, `{"blockTimestamp":"0x1","miner":null,"size":"0x10",`, 1) + `,"x-served-by":"node-7"`),
		"an explicit null error":       envelope(`"jsonrpc":"2.0","id":1,"error":null,"result":` + header),
		"duplicates of ignored fields": envelope(`"jsonrpc":"2.0","id":1,"result":` + strings.Replace(header, `{`, `{"miner":"a","miner":"b",`, 1)),
	}
	for name, body := range accepted {
		if h, err := stubEndpoint(t, body).HeaderByNumber(ctx, 10); err != nil || h.Number != 10 {
			t.Errorf("%s: HeaderByNumber = %+v, %v; want block 10\n%s", name, h, err, body)
		}
	}

	refused := map[string]string{
		"no jsonrpc member":             envelope(`"id":1,"result":` + header),
		"jsonrpc 1.0":                   envelope(`"jsonrpc":"1.0","id":1,"result":` + header),
		"jsonrpc as a number":           envelope(`"jsonrpc":2.0,"id":1,"result":` + header),
		"jsonrpc in another case":       envelope(`"JSONRPC":"2.0","id":1,"result":` + header),
		"result in another case":        envelope(`"jsonrpc":"2.0","id":1,"Result":` + header),
		"id in another case":            envelope(`"jsonrpc":"2.0","ID":1,"result":` + header),
		"two results":                   envelope(`"jsonrpc":"2.0","id":1,"result":null,"result":` + header),
		"two ids":                       envelope(`"jsonrpc":"2.0","id":2,"id":1,"result":` + header),
		"result and another-case twin":  envelope(`"jsonrpc":"2.0","id":1,"result":null,"RESULT":` + header),
		"header: hash in another case":  envelope(`"jsonrpc":"2.0","id":1,"result":` + strings.Replace(header, `"hash"`, `"Hash"`, 1)),
		"header: number twice":          envelope(`"jsonrpc":"2.0","id":1,"result":` + strings.Replace(header, `{`, `{"number":"0xb",`, 1)),
		"header: timestamp upper-cased": envelope(`"jsonrpc":"2.0","id":1,"result":` + strings.Replace(header, `"timestamp"`, `"TIMESTAMP"`, 1)),
	}
	for name, body := range refused {
		h, err := stubEndpoint(t, body).HeaderByNumber(ctx, 10)
		if err == nil || !IsKind(err, KindBadResponse) {
			t.Errorf("%s: HeaderByNumber = %+v, %v; want a bad-response error\n%s", name, h, err, body)
		}
	}

	// An error is taken from anything that carries one: gateways refuse
	// requests in many shapes, and a refusal makes no claim about the chain.
	for name, body := range map[string]string{
		"error without jsonrpc":     `{"id":1,"error":{"code":-32005,"message":"rate limit exceeded"}}`,
		"error with a data member":  `{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"rate limit exceeded","data":{"see":"https://example.invalid"}}}`,
		"error with a null id":      `{"jsonrpc":"2.0","id":null,"error":{"code":-32005,"message":"rate limit exceeded"}}`,
		"error and extra top-level": `{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"rate limit exceeded"},"requestId":"abc"}`,
	} {
		_, err := stubEndpoint(t, body).HeaderByNumber(ctx, 10)
		if err == nil || IsKind(err, KindBadResponse) || !strings.Contains(err.Error(), "rate limit exceeded") {
			t.Errorf("%s: err = %v; want the provider's refusal", name, err)
		}
	}
	// But not from an object that is not one.
	for name, body := range map[string]string{
		"error twice":             `{"jsonrpc":"2.0","id":1,"error":null,"error":{"code":-32005,"message":"x"}}`,
		"error code twice":        `{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"code":-32000,"message":"x"}}`,
		"message in another case": `{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"Message":"x"}}`,
	} {
		if _, err := stubEndpoint(t, body).HeaderByNumber(ctx, 10); err == nil || !IsKind(err, KindBadResponse) {
			t.Errorf("%s: err = %v; want a bad-response error", name, err)
		}
	}

	// Receipts and the logs in them.
	tx, _ := ParseHash(fuzzHashC)
	single := func(result string) string { return `{"jsonrpc":"2.0","id":1,"result":` + result + `}` }
	if r, err := stubEndpoint(t, single(fuzzReceipt("0x1"))).TransactionReceipt(ctx, tx); err != nil || len(r.Logs) != 1 {
		t.Fatalf("TransactionReceipt = %+v, %v", r, err)
	}
	for name, receipt := range map[string]string{
		"status twice":              strings.Replace(fuzzReceipt("0x1"), `"status":"0x1"`, `"status":"0x0","status":"0x1"`, 1),
		"status in another case":    strings.Replace(fuzzReceipt("0x1"), `"status"`, `"Status"`, 1),
		"log: logIndex twice":       strings.Replace(fuzzReceipt("0x1"), `"logIndex":"0x0"`, `"logIndex":"0x5","logIndex":"0x0"`, 1),
		"log: data in another case": strings.Replace(fuzzReceipt("0x1"), `"data"`, `"Data"`, 1),
		"log: topics twice":         strings.Replace(fuzzReceipt("0x1"), `"topics":[`, `"topics":[],"topics":[`, 1),
	} {
		if r, err := stubEndpoint(t, single(receipt)).TransactionReceipt(ctx, tx); err == nil || !IsKind(err, KindBadResponse) {
			t.Errorf("receipt, %s: %+v, %v; want a bad-response error", name, r, err)
		}
	}
}

// TestTransferLogsEndsAtTheStatedHead is the input FuzzRPCResponse stopped
// at (048afdf5248a0f7d): a batch answer [block number 16, logs] to a scan of
// blocks 10..20.
//
// Two things were in it. Its second element had no "jsonrpc" member and
// member names in the wrong case ("BloCkNumBer", "logIndeX"), all of which
// encoding/json read as if they were right: that is now refused. And with
// the names put right, the scan returns Next = 17 without an error — the
// property the fuzz target checked ("no error means the whole range") was
// older than the behaviour, which is deliberate and documented on
// TransferScan.Next: the backend that answered is at block 16, so blocks
// 10..16 are what it can vouch for, and the caller goes on from 17.
func TestTransferLogsEndsAtTheStatedHead(t *testing.T) {
	ctx := context.Background()
	token := ethaddr.MustParse(fuzzToken)
	q := TransferQuery{Token: token, FromBlock: 10, ToBlock: 20, Recipients: []ethaddr.Address{ethaddr.MustParse("0x" + fuzzTo[26:])}}

	raw, err := os.ReadFile("testdata/fuzz/FuzzRPCResponse/048afdf5248a0f7d")
	if err != nil {
		t.Fatal(err)
	}
	// The corpus file holds the body as a Go string literal on its third line.
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[2], `[]byte("`) {
		t.Fatalf("unexpected corpus file: %q", raw)
	}
	body := strings.NewReplacer(`\"`, `"`).Replace(strings.TrimSuffix(strings.TrimPrefix(lines[2], `[]byte("`), `")`))
	scan, err := stubEndpoint(t, body).TransferLogs(ctx, q)
	if err == nil || scan.Next != q.FromBlock || len(scan.Transfers) != 0 {
		t.Fatalf("the saved input: next = %d, %d transfers, err = %v; want nothing covered and an error", scan.Next, len(scan.Transfers), err)
	}

	wellFormed := `[{"jsonrpc":"2.0","id":1,"result":"0x10"},{"jsonrpc":"2.0","id":2,"result":[` + fuzzLog("0xa", "0x0") + `,` + fuzzLog("0x10", "0x3") + `]}]`
	scan, err = stubEndpoint(t, wellFormed).TransferLogs(ctx, q)
	if err != nil || scan.Next != 17 || scan.BackendHead != 16 || scan.Complete(q) || len(scan.Transfers) != 2 {
		t.Fatalf("next = %d, backend head = %d, %d transfers, err = %v; want the scan to end at the backend's block 16", scan.Next, scan.BackendHead, len(scan.Transfers), err)
	}
	// A log from above the height the backend stated contradicts it.
	beyond := `[{"jsonrpc":"2.0","id":1,"result":"0x10"},{"jsonrpc":"2.0","id":2,"result":[` + fuzzLog("0x12", "0x0") + `]}]`
	scan, err = stubEndpoint(t, beyond).TransferLogs(ctx, q)
	for _, tr := range scan.Transfers {
		if tr.BlockNumber >= scan.Next {
			t.Fatalf("transfer in block %d returned by a scan that covers blocks below %d (err = %v)", tr.BlockNumber, scan.Next, err)
		}
	}
}
