package watcher

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

func rangesString(rs []uncheckedRange) string {
	var parts []string
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%d-%d%v", r.From, r.To, r.By))
	}
	return strings.Join(parts, " ")
}

// TestUncheckedRecordKeepsWhoScanned: the record of blocks settled on one
// endpoint's logs says, for every run of blocks, whose scans are on record;
// a block leaves it when two different endpoints have scanned it.
func TestUncheckedRecordKeepsWhoScanned(t *testing.T) {
	all := func(string) bool { return true }
	var s state
	s.addUnchecked(100, 199, "a")
	s.addUnchecked(200, 249, "a") // joins the run of the same endpoint
	s.addUnchecked(300, 349, "b")
	if got := rangesString(s.Unchecked); got != "100-249[a] 300-349[b]" {
		t.Fatalf("record: %s", got)
	}
	// Settled again (a rewind), by another endpoint: its scan is the one
	// that counts for these blocks now.
	s.addUnchecked(150, 159, "b")
	if got := rangesString(s.Unchecked); got != "100-149[a] 150-159[b] 160-249[a] 300-349[b]" {
		t.Fatalf("record after a re-settle: %s", got)
	}
	// "a" scanning its own blocks checks nothing — but it checks the ones
	// "b" settled; "b" scanning a's blocks checks those.
	s.checkedBy(100, 249, "a", all)
	if got := rangesString(s.Unchecked); got != "100-149[a] 160-249[a] 300-349[b]" {
		t.Fatalf("record after the endpoint that settled them scanned them again: %s", got)
	}
	s.checkedBy(100, 200, "b", all)
	if got := rangesString(s.Unchecked); got != "201-249[a] 300-349[b]" {
		t.Fatalf("record after another endpoint scanned part of them: %s", got)
	}
	if from, to := s.uncheckedBounds(); from != 201 || to != 349 {
		t.Fatalf("bounds %d..%d", from, to)
	}
	// Blocks nobody is on record for (an old checkpoint) take two.
	var old state
	old.Unchecked = []uncheckedRange{{From: 10, To: 20}}
	old.checkedBy(10, 20, "a", all)
	if got := rangesString(old.Unchecked); got != "10-20[a]" {
		t.Fatalf("record of an old checkpoint after one scan: %s", got)
	}
	old.checkedBy(10, 20, "a", all)
	old.checkedBy(10, 15, "b", all)
	if got := rangesString(old.Unchecked); got != "16-20[a]" {
		t.Fatalf("record of an old checkpoint after two: %s", got)
	}
	if !old.dropUncheckedBelow(21) || len(old.Unchecked) != 0 || old.Unchecked != nil {
		t.Fatalf("dropped below 21: %s", rangesString(old.Unchecked))
	}

	// A label on record that is no longer configured counts for nobody:
	// its provider may be configured still, under the new URL ("a2"). That
	// one scan does not make two; a second endpoint configured now does.
	var renamed state
	renamed.addUnchecked(10, 20, "a")
	now := func(l string) bool { return l == "a2" || l == "b" }
	renamed.checkedBy(10, 20, "a2", now)
	if got := rangesString(renamed.Unchecked); got != "10-20[a a2]" {
		t.Fatalf("after a scan by the endpoint under a new URL: %s", got)
	}
	renamed.checkedBy(10, 20, "b", now)
	if len(renamed.Unchecked) != 0 {
		t.Fatalf("after a second endpoint configured now: %s", rangesString(renamed.Unchecked))
	}
}

// TestUncheckedRecordIsBounded: past maxUncheckedRanges runs, neighbours are
// merged — the blocks between them included, and of the endpoints on record
// only those both name: more is scanned again, never less.
func TestUncheckedRecordIsBounded(t *testing.T) {
	var s state
	for i := range maxUncheckedRanges + 5 {
		by := "a"
		if i%2 == 1 {
			by = "b"
		}
		from := uint64(1000 + 10*i)
		s.addUnchecked(from, from+4, by)
	}
	if len(s.Unchecked) != maxUncheckedRanges {
		t.Fatalf("%d runs recorded", len(s.Unchecked))
	}
	covered := func(n uint64) (uncheckedRange, bool) {
		for _, r := range s.Unchecked {
			if r.From <= n && n <= r.To {
				return r, true
			}
		}
		return uncheckedRange{}, false
	}
	for i := range maxUncheckedRanges + 5 {
		n := uint64(1000 + 10*i)
		r, ok := covered(n)
		want := "a"
		if i%2 == 1 {
			want = "b"
		}
		if !ok || len(r.By) > 1 || (len(r.By) == 1 && r.By[0] != want) {
			t.Fatalf("block %d after merging: %v %+v", n, ok, r)
		}
	}
}

// TestUncheckedRecordInTheCheckpoint: the record is written with its
// endpoints, read back the same, and a checkpoint of the earlier form — the
// first and the last block only — is read as a run nobody is on record for.
func TestUncheckedRecordInTheCheckpoint(t *testing.T) {
	token := ethaddr.MustParse("0x55d398326f99059fF775485246999027B3197955")
	s := state{Initialized: true, Cursor: 500, Tracked: map[transferKey]*tracked{}}
	s.addUnchecked(100, 199, "https://a.example")
	s.addUnchecked(300, 310, "https://b.example")
	data, err := s.encode(56, token)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"unchecked":[{"from":100,"to":199,"by":["https://a.example"]},{"from":300,"to":310,"by":["https://b.example"]}]`) ||
		strings.Contains(string(data), "unchecked_from") {
		t.Fatalf("checkpoint: %s", data)
	}
	back, err := decodeState(data, 56, token)
	if err != nil || rangesString(back.Unchecked) != rangesString(s.Unchecked) {
		t.Fatalf("read back: %s, %v", rangesString(back.Unchecked), err)
	}

	old := strings.Replace(string(data), `"unchecked":[{"from":100,"to":199,"by":["https://a.example"]},{"from":300,"to":310,"by":["https://b.example"]}]`,
		`"unchecked_from":100,"unchecked_to":310`, 1)
	back, err = decodeState([]byte(old), 56, token)
	if err != nil || rangesString(back.Unchecked) != "100-310[]" {
		t.Fatalf("an old checkpoint: %s, %v", rangesString(back.Unchecked), err)
	}
	older := strings.Replace(old, `,"unchecked_to":310`, "", 1)
	back, err = decodeState([]byte(older), 56, token)
	if err != nil || rangesString(back.Unchecked) != "100-500[]" {
		t.Fatalf("a checkpoint from before the end was recorded: %s, %v", rangesString(back.Unchecked), err)
	}
	for _, bad := range []string{
		strings.Replace(string(data), `"unchecked":[`, `"unchecked_from":5,"unchecked":[`, 1),
		strings.Replace(string(data), `{"from":300,"to":310`, `{"from":150,"to":310`, 1),
		strings.Replace(string(data), `{"from":300,"to":310`, `{"from":300,"to":299`, 1),
		strings.Replace(string(data), `"by":["https://b.example"]`, `"by":["https://b.example","https://b.example"]`, 1),
		strings.Replace(string(data), `"by":["https://b.example"]`, `"by":[""]`, 1),
		strings.Replace(string(data), `"by":["https://b.example"]`, `"by":["https://b.example"],"x":1`, 1),
	} {
		if _, err := decodeState([]byte(bad), 56, token); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	_ = slices.Contains[[]string]
}

// TestUncheckedRecordKnowsHowOldItsBlocksAre: every run carries a block time
// no later than any of its blocks — that of the block below it the cursor
// rested on — through splits, joins and merges (the lowest wins), and in the
// checkpoint. A run of an older checkpoint has none (0: as old as can be).
func TestUncheckedRecordKnowsHowOldItsBlocksAre(t *testing.T) {
	all := func(string) bool { return true }
	var s state
	s.Anchors = []blockRef{{Number: 99, Hash: evm.Hash{99}, Time: 1_000}, {Number: 149, Hash: evm.Hash{149}, Time: 1_150}}
	s.addUnchecked(100, 149, "a")
	s.Anchors = append(s.Anchors, blockRef{Number: 199, Hash: evm.Hash{199}, Time: 1_200})
	s.addUnchecked(150, 199, "a") // joins the run before it
	s.addUnchecked(200, 249, "b")
	if len(s.Unchecked) != 2 || s.Unchecked[0].Since != 1_000 || s.Unchecked[1].Since != 1_200 {
		t.Fatalf("record: %+v", s.Unchecked)
	}
	s.checkedBy(120, 130, "b", all) // splits the first run: both parts keep its time
	if len(s.Unchecked) != 3 || s.Unchecked[0].Since != 1_000 || s.Unchecked[1].Since != 1_000 {
		t.Fatalf("after a split: %+v", s.Unchecked)
	}
	if since, ok := s.uncheckedSince(); !ok || since != 1_000 {
		t.Fatalf("oldest: %d, %v", since, ok)
	}
	s.checkedBy(100, 199, "b", all)
	if since, ok := s.uncheckedSince(); !ok || since != 1_200 {
		t.Fatalf("oldest after the older runs were checked: %d, %v (%+v)", since, ok, s.Unchecked)
	}

	token := ethaddr.MustParse("0x55d398326f99059fF775485246999027B3197955")
	s.Initialized, s.Cursor, s.Tracked = true, 300, map[transferKey]*tracked{}
	data, err := s.encode(56, token)
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodeState(data, 56, token)
	if err != nil || len(back.Unchecked) != 1 || back.Unchecked[0].Since != 1_200 {
		t.Fatalf("read back: %+v, %v (%s)", back.Unchecked, err, data)
	}
	var none state
	none.addUnchecked(10, 20, "a")
	if since, ok := none.uncheckedSince(); !ok || since != 0 {
		t.Fatalf("a run with no block below it on record: %d, %v", since, ok)
	}
	var empty state
	if _, ok := empty.uncheckedSince(); ok {
		t.Fatal("an empty record has an oldest run")
	}
}
