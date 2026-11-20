package watcher

import (
	"bytes"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

var checkpointToken = ethaddr.MustParse("0x55d398326f99059fF775485246999027B3197955")

func checkpointTransferAt(block uint64, blockHash, tx byte, log uint64, amount int64) Transfer {
	return Transfer{
		Token: checkpointToken, From: ethaddr.Address{7}, To: ethaddr.Address{8}, Amount: big.NewInt(amount),
		TxHash: evm.Hash{tx}, LogIndex: log, BlockNumber: block, BlockHash: evm.Hash{blockHash},
	}
}

// TestCheckpointEncodingIsCanonical: the same state is always written as the
// same bytes. The transfers used to be ordered by (block number, log index,
// block hash), and two with the same three — the input FuzzCheckpointDecode
// found — came out in the order of a map iteration: a different checkpoint
// every few saves, for the same state.
func TestCheckpointEncodingIsCanonical(t *testing.T) {
	st := state{Initialized: true, Cursor: 1000, Tracked: map[transferKey]*tracked{}}
	var want []transferKey
	add := func(tr Transfer) {
		st.Tracked[keyOf(tr)] = &tracked{Transfer: tr}
		want = append(want, keyOf(tr))
	}
	// In canonical order: block number, log index, block hash, transaction.
	add(checkpointTransferAt(989, 0x09, 0x01, 7, 1))
	add(checkpointTransferAt(990, 0x05, 0x02, 3, 1)) // one height and log index,
	add(checkpointTransferAt(990, 0x06, 0x01, 3, 1)) // two competing blocks:
	add(checkpointTransferAt(990, 0x07, 0x0a, 3, 1)) // each of them is tracked
	add(checkpointTransferAt(990, 0x04, 0x03, 4, 1))
	add(checkpointTransferAt(991, 0x01, 0x01, 0, 1))
	// The same transaction mined in two blocks (dropped, then mined again).
	add(checkpointTransferAt(992, 0x02, 0x55, 1, 1))
	add(checkpointTransferAt(993, 0x03, 0x55, 1, 1))

	first, err := st.encode(56, checkpointToken)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		// A fresh map each time, filled in another order.
		again := state{Initialized: true, Cursor: st.Cursor, Tracked: map[transferKey]*tracked{}}
		for j := range want {
			k := want[(j+i)%len(want)]
			again.Tracked[k] = st.Tracked[k]
		}
		enc, err := again.encode(56, checkpointToken)
		if err != nil || !bytes.Equal(enc, first) {
			t.Fatalf("encoding %d differs (%v):\n%s\n%s", i, err, first, enc)
		}
	}
	// The order is the documented one, and total: no two keys compare equal.
	decoded, err := decodeState(first, 56, checkpointToken)
	if err != nil || len(decoded.Tracked) != len(want) {
		t.Fatalf("decode: %d transfers, %v", len(decoded.Tracked), err)
	}
	at := 0
	for _, k := range want {
		i := bytes.Index(first[at:], []byte(k.Tx.String()+`","log":`))
		if i < 0 {
			t.Fatalf("transfers are not in canonical order (tx %s, log %d, block %s out of place):\n%s", k.Tx, k.Log, k.Block, first)
		}
		at += i + 1
	}
	// Even two transfers that cannot both exist — one position of one block,
	// two transactions — have an order, so that no state at all is written
	// in two ways. (The watcher does not track such a pair: occupant.)
	x, y := checkpointTransferAt(990, 0x05, 0x02, 3, 1), checkpointTransferAt(990, 0x05, 0x03, 3, 1)
	if compareTransfers(x, y) >= 0 || compareTransfers(y, x) <= 0 || compareTransfers(x, x) != 0 {
		t.Fatal("compareTransfers does not order two transactions at one position")
	}
	for i, a := range want {
		for j, b := range want {
			c := compareTransfers(st.Tracked[a].Transfer, st.Tracked[b].Transfer)
			if (i < j && c >= 0) || (i == j && c != 0) || (i > j && c <= 0) {
				t.Fatalf("compareTransfers(%d, %d) = %d", i, j, c)
			}
		}
	}
}

// TestCheckpointDecodingIsStrict: a checkpoint is accepted only if it is a
// document the watcher could have written.
func TestCheckpointDecodingIsStrict(t *testing.T) {
	st := state{Initialized: true, Cursor: 1000, Tracked: map[transferKey]*tracked{},
		Anchors: []blockRef{{Number: 1000, Hash: evm.Hash{2}, Time: 1_700_000_100}}, Finalized: blockRef{Number: 998, Hash: evm.Hash{3}}}
	tr := checkpointTransferAt(990, 0x05, 0x04, 3, 1_000_000)
	st.Tracked[keyOf(tr)] = &tracked{Transfer: tr}
	valid, err := st.encode(56, checkpointToken)
	if err != nil {
		t.Fatal(err)
	}
	s := string(valid)
	one := s[strings.Index(s, `"transfers":[`)+len(`"transfers":[`) : len(s)-2]
	withSecond := func(second string) string { return strings.Replace(s, one, one+","+second, 1) }

	accepted := map[string]string{
		"as written":                           s,
		"members in another order, whitespace": " " + strings.Replace(s, `{"v":1,`, `{`, 1)[:len(s)-len(`"v":1,`)-1] + ` , "v" : 1 }`,
		"a competing block at the same height": withSecond(strings.Replace(one, `"block_hash":"0x05`, `"block_hash":"0x06`, 1)),
		"the same transaction in a later block": withSecond(strings.NewReplacer(
			`"block_hash":"0x05`, `"block_hash":"0x06`, `"block":990`, `"block":991`).Replace(one)),
		"another transfer of the same transaction": withSecond(strings.Replace(one, `"log":3`, `"log":4`, 1)),
	}
	for name, doc := range accepted {
		got, err := decodeState([]byte(doc), 56, checkpointToken)
		if err != nil || got.Cursor != 1000 {
			t.Errorf("%s: %v\n%s", name, err, doc)
		}
		if _, err := InspectCheckpoint([]byte(doc)); err != nil {
			t.Errorf("%s: InspectCheckpoint: %v", name, err)
		}
	}

	refused := map[string]struct{ doc, why string }{
		"cursor in another case":    {strings.Replace(s, `"cursor":`, `"Cursor":`, 1), `is not "cursor"`},
		"cursor twice":              {strings.Replace(s, `"cursor":1000`, `"cursor":1,"cursor":1000`, 1), "more than once"},
		"chain id in another case":  {strings.Replace(s, `"chain_id":`, `"Chain_id":`, 1), `is not "chain_id"`},
		"an unknown member":         {strings.Replace(s, `"cursor":1000`, `"cursor":1000,"note":"x"`, 1), `unknown member "note"`},
		"transfers twice":           {strings.Replace(s, `"transfers":[`, `"transfers":[],"transfers":[`, 1), "more than once"},
		"transfer: block misspelt":  {strings.Replace(s, `"block":990`, `"bloCk":990`, 1), `is not "block"`},
		"transfer: amount twice":    {strings.Replace(s, `"amount":"1000000"`, `"amount":"1","amount":"1000000"`, 1), "more than once"},
		"transfer: unknown member":  {strings.Replace(s, `"block":990`, `"block":990,"2":"x"`, 1), `unknown member "2"`},
		"anchor: hash twice":        {strings.Replace(s, `"anchors":[{"n":1000,`, `"anchors":[{"n":1000,"h":"0x`+strings.Repeat("11", 32)+`",`, 1), "more than once"},
		"anchor: unknown member":    {strings.Replace(s, `"anchors":[{"n":1000,`, `"anchors":[{"n":1000,"x":1,`, 1), `unknown member "x"`},
		"finalized: number twice":   {strings.Replace(s, `"finalized":{"n":998,`, `"finalized":{"n":1,"n":998,`, 1), "more than once"},
		"the same transfer twice":   {withSecond(one), "listed twice"},
		"the same, another amount":  {withSecond(strings.Replace(one, `"amount":"1000000"`, `"amount":"2"`, 1)), "listed twice"},
		"two logs at one position":  {withSecond(strings.Replace(one, `"tx":"0x04`, `"tx":"0x09`, 1)), "both claim log index 3"},
		"data after the checkpoint": {s + s, "not a checkpoint"},
	}
	for name, tc := range refused {
		if !strings.Contains(tc.doc, `"v":1`) || tc.doc == s {
			t.Fatalf("%s: test setup: the replacement did not apply", name)
		}
		_, err := decodeState([]byte(tc.doc), 56, checkpointToken)
		if !errors.Is(err, ErrCheckpoint) || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("%s: err = %v, want ErrCheckpoint mentioning %q", name, err, tc.why)
		}
		if _, err := InspectCheckpoint([]byte(tc.doc)); !errors.Is(err, ErrCheckpoint) {
			t.Errorf("%s: InspectCheckpoint: %v", name, err)
		}
	}

	// A checkpoint of another version is refused for its version, whatever
	// else a newer build has put into it.
	future := strings.Replace(s, `"v":1`, `"v":2,"shards":[1,2]`, 1)
	if _, err := decodeState([]byte(future), 56, checkpointToken); !errors.Is(err, ErrCheckpoint) || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("newer version: %v", err)
	}
	if _, err := InspectCheckpoint([]byte(future)); !errors.Is(err, ErrCheckpoint) || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("newer version, InspectCheckpoint: %v", err)
	}

	// The input the fuzzer found: two transfers at log index 3 of one block,
	// under two transactions — and "Chain_id", "Block_hash", "bloCk" and a
	// member "2" besides, all of which used to be read as if they were right.
	raw, err := os.ReadFile("testdata/fuzz/FuzzCheckpointDecode/23e4f4a0550ba5ab")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], `[]byte("`) {
		t.Fatalf("unexpected corpus file: %q", raw)
	}
	found := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(lines[1], `[]byte("`), `")`), `\"`, `"`)
	if _, err := decodeState([]byte(found), 56, checkpointToken); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("the fuzzer's input: %v", err)
	}
	// With the member names put right it is still refused, for what it says.
	named := strings.NewReplacer(`"Chain_id"`, `"chain_id"`, `"Block_hash"`, `"block_hash"`, `"bloCk"`, `"block"`, `"2":"X2#x7",`, ``,
		`"amount":"8220"`, `"from":"0x0000000000000000000000000000000000000000","to":"0x0000000000000000000000000000000000000000","amount":"8220"`,
		`"amount":"1000"`, `"from":"0x0000000000000000000000000000000000000000","to":"0x0000000000000000000000000000000000000000","amount":"1000"`,
		`"0X05`, `"0x05`).Replace(found)
	if _, err := decodeState([]byte(named), 56, checkpointToken); !errors.Is(err, ErrCheckpoint) || !strings.Contains(err.Error(), "both claim log index 3") {
		t.Fatalf("the fuzzer's input with exact member names: %v\n%s", err, named)
	}
}
