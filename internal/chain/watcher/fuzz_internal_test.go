package watcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

// FuzzCheckpointDecode: the checkpoint decoder against arbitrary bytes. A
// checkpoint is either rejected as a whole (ErrCheckpoint) or yields a state
// that satisfies the invariants the watcher computes with and that survives
// being written and read again unchanged.
//
//	go test ./internal/chain/watcher -run '^$' -fuzz FuzzCheckpointDecode -fuzztime 3m
func FuzzCheckpointDecode(f *testing.F) {
	token := ethaddr.MustParse("0x55d398326f99059fF775485246999027B3197955")
	const chainID = 56

	good := state{
		Initialized: true,
		Cursor:      1000,
		Anchors:     []blockRef{{Number: 900, Hash: evm.Hash{1}, Time: 1_700_000_000}, {Number: 1000, Hash: evm.Hash{2}, Time: 1_700_000_100}},
		Finalized:   blockRef{Number: 998, Hash: evm.Hash{3}},
		Tracked:     map[transferKey]*tracked{},
	}
	tr := Transfer{
		Token: token, From: ethaddr.Address{7}, To: ethaddr.Address{8}, Amount: big.NewInt(1_000_000),
		TxHash: evm.Hash{4}, LogIndex: 3, BlockNumber: 990, BlockHash: evm.Hash{5},
	}
	good.Tracked[keyOf(tr)] = &tracked{Transfer: tr}
	valid, err := good.encode(chainID, token)
	if err != nil {
		f.Fatal(err)
	}
	s := string(valid)
	seeds := []string{
		s, "", "null", "{}", "[]", `{"v":1}`, `{"v":2}`, s + s, s + " x", strings.Replace(s, `"v":1`, `"v":"1"`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":18446744073709551615`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":18446744073709551616`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":-1`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1e3`, 1),
		strings.Replace(s, `"chain_id":56`, `"chain_id":97`, 1),
		strings.Replace(s, `"amount":"1000000"`, `"amount":"-5"`, 1),
		strings.Replace(s, `"amount":"1000000"`, `"amount":"0"`, 1),
		strings.Replace(s, `"amount":"1000000"`, `"amount":"0x10"`, 1),
		strings.Replace(s, `"amount":"1000000"`, `"amount":"`+strings.Repeat("9", 100)+`"`, 1),
		strings.Replace(s, `"amount":"1000000"`, `"amount":1000000`, 1),
		strings.Replace(s, `"anchors":[`, `"anchors":[null,`, 1),
		strings.Replace(s, `"transfers":[`, `"transfers":[null,`, 1),
		strings.Replace(s, `"transfers":[`, `"transfers":[{},`, 1),
		strings.Replace(s, `"finalized":{`, `"finalized":null,"x":{`, 1),
		strings.Replace(s, `"token":"`, `"token":"0x00","t0":"`, 1),
		strings.Replace(s, `"transfers":`, `"transfers":`+strings.Repeat("[", 50)+strings.Repeat("]", 50)+`,"y":`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"log_servers":["https://b.example","https://a.example"],"unchecked_from":950`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"log_servers":["https://a.example","https://a.example"]`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"log_servers":[""],"unchecked_from":18446744073709551615`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"log_servers":[1,null]`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"unchecked":[{"from":900,"to":950,"by":["https://a.example"]},{"from":960,"to":990}]`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"unchecked":[{"from":900,"to":950},{"from":940,"to":990}]`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"unchecked":[{"from":900,"to":950,"by":["https://a.example","https://b.example"]}]`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"unchecked":[{"from":900,"to":950,"by":[""]}]`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"unchecked":[{"from":900,"to":950,"x":1}]`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"unchecked_from":900,"unchecked":[{"from":900,"to":950}]`, 1),
		strings.Replace(s, `"block":990`, `"block":990,"time":1700000050`, 1),
		strings.Replace(s, `"block":990`, `"block":990,"time":18446744073709551615`, 1),
		strings.Replace(s, `"block":990`, `"block":990,"time":-1`, 1),
		strings.Replace(s, `"block":990`, `"block":990,"time":"1700000050"`, 1),
	}
	// The same anchor and the same transfer twice; anchors out of order.
	if i, j := strings.Index(s, `"anchors":[`), strings.Index(s, `],"finalized"`); i > 0 && j > i {
		anchors := s[i+len(`"anchors":[`) : j]
		parts := strings.SplitN(anchors, "},{", 2)
		if len(parts) == 2 {
			seeds = append(seeds, s[:i]+`"anchors":[{`+parts[1]+`,`+parts[0]+`}`+s[j:], s[:i]+`"anchors":[`+parts[0]+`},`+parts[0]+`}`+s[j:])
		}
	}
	if i := strings.Index(s, `"transfers":[`); i > 0 {
		one := s[i+len(`"transfers":[`) : len(s)-2]
		seeds = append(seeds, s[:i]+`"transfers":[`+one+`,`+one+`]}`)
		// The same height and log index in a competing block (legitimate),
		// in the same block under another transaction (impossible), and the
		// same transfer with another amount.
		otherBlock := strings.Replace(one, `"block_hash":"0x05`, `"block_hash":"0x06`, 1)
		otherTx := strings.Replace(one, `"tx":"0x04`, `"tx":"0x09`, 1)
		otherAmount := strings.Replace(one, `"amount":"1000000"`, `"amount":"2"`, 1)
		for _, second := range []string{otherBlock, otherTx, otherAmount} {
			seeds = append(seeds, s[:i]+`"transfers":[`+one+`,`+second+`]}`, s[:i]+`"transfers":[`+second+`,`+one+`]}`)
		}
	}
	// Members in another case, twice, or unknown.
	seeds = append(seeds,
		strings.Replace(s, `"cursor":1000`, `"Cursor":1000`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1,"cursor":1000`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"CURSOR":1`, 1),
		strings.Replace(s, `"cursor":1000`, `"cursor":1000,"note":"x"`, 1),
		strings.Replace(s, `"chain_id":56`, `"Chain_id":56`, 1),
		strings.Replace(s, `"block":990`, `"bloCk":990`, 1),
		strings.Replace(s, `"block":990`, `"block":990,"block":991`, 1),
		strings.Replace(s, `"v":1`, `"v":1,"v":1`, 1),
		strings.Replace(s, `"v":1`, `"v":2,"future_member":true`, 1),
	)
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		st, err := decodeState(data, chainID, token)
		info, ierr := InspectCheckpoint(data)
		if err != nil {
			if !errors.Is(err, ErrCheckpoint) {
				t.Fatalf("decodeState: %v does not wrap ErrCheckpoint", err)
			}
			if ierr != nil && !errors.Is(ierr, ErrCheckpoint) {
				t.Fatalf("InspectCheckpoint: %v does not wrap ErrCheckpoint", ierr)
			}
			return
		}
		if ierr != nil || info.ChainID != chainID || info.Token != token || info.Cursor != st.Cursor {
			t.Fatalf("InspectCheckpoint = %+v, %v for a checkpoint the watcher accepts", info, ierr)
		}
		if !st.Initialized || st.Tracked == nil {
			t.Fatal("accepted state is not usable")
		}
		if len(st.LogServers) > maxLogServers || !slices.IsSorted(st.LogServers) || slices.Contains(st.LogServers, "") {
			t.Fatalf("log endpoints not sorted, bounded and non-empty: %q", st.LogServers)
		}
		for i, a := range st.Anchors {
			if a.Hash.IsZero() || (i > 0 && a.Number <= st.Anchors[i-1].Number) {
				t.Fatalf("anchors not ascending or without hash: %+v", st.Anchors)
			}
		}
		if st.Finalized.Number != 0 && st.Finalized.Hash.IsZero() {
			t.Fatalf("finalized block without hash: %+v", st.Finalized)
		}
		for k, tr := range st.Tracked {
			if k != keyOf(tr.Transfer) || tr.Token != token || tr.Amount == nil || tr.Amount.Sign() <= 0 || tr.Amount.BitLen() > 256 ||
				tr.BlockHash.IsZero() || tr.TxHash.IsZero() || tr.delivered {
				t.Fatalf("tracked transfer %+v", tr.Transfer)
			}
			// One log per position of a block.
			if other := st.occupant(tr.Transfer); other != nil {
				t.Fatalf("transfers %s and %s are both tracked at log index %d of block %s", tr.TxHash, other.TxHash, tr.LogIndex, tr.BlockHash)
			}
		}
		// What is accepted is what the watcher writes, member for member:
		// read leniently — any case, unknown members, the last of several —
		// the document says the same thing.
		var lenient struct {
			Cursor    uint64            `json:"cursor"`
			Transfers []json.RawMessage `json:"transfers"`
		}
		if err := json.Unmarshal(data, &lenient); err != nil || lenient.Cursor != st.Cursor || len(lenient.Transfers) != len(st.Tracked) {
			t.Fatalf("accepted checkpoint reads differently with encoding/json: cursor %d vs %d, %d vs %d transfers (%v)",
				lenient.Cursor, st.Cursor, len(lenient.Transfers), len(st.Tracked), err)
		}

		// Written and read again, it is the same state, and writing is a
		// fixed point.
		enc, err := st.encode(chainID, token)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		again, err := decodeState(enc, chainID, token)
		if err != nil {
			t.Fatalf("the watcher cannot read its own checkpoint: %v\n%s", err, enc)
		}
		if again.Cursor != st.Cursor || again.Finalized != st.Finalized || len(again.Anchors) != len(st.Anchors) || len(again.Tracked) != len(st.Tracked) ||
			!slices.EqualFunc(again.Unchecked, st.Unchecked, func(a, b uncheckedRange) bool {
				return a.From == b.From && a.To == b.To && slices.Equal(a.By, b.By)
			}) || !slices.Equal(again.LogServers, st.LogServers) {
			t.Fatalf("checkpoint changed in a round trip:\n%s", enc)
		}
		for i := range st.Anchors {
			if again.Anchors[i] != st.Anchors[i] {
				t.Fatalf("anchor %d changed in a round trip", i)
			}
		}
		for k, tr := range st.Tracked {
			o, ok := again.Tracked[k]
			if !ok || o.Amount.Cmp(tr.Amount) != 0 || o.From != tr.From || o.To != tr.To || o.BlockNumber != tr.BlockNumber || !o.BlockTime.Equal(tr.BlockTime) {
				t.Fatalf("transfer %+v changed in a round trip", tr.Transfer)
			}
		}
		// The encoding is canonical: the same state is the same bytes,
		// however often and from whichever copy it is written. (Go
		// randomises map iteration, so a repeat is a real test of that.)
		for i := 0; i < 8; i++ {
			enc2, err := again.encode(chainID, token)
			if err != nil || !bytes.Equal(enc, enc2) {
				t.Fatalf("encoding is not stable:\n%s\n%s", enc, enc2)
			}
			if enc3, err := st.encode(chainID, token); err != nil || !bytes.Equal(enc, enc3) {
				t.Fatalf("encoding is not stable:\n%s\n%s", enc, enc3)
			}
		}
	})
}
