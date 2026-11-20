package watcher

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"sort"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/strictjson"
)

// ErrCheckpoint is returned (wrapped) when the stored checkpoint cannot be
// used: it is corrupt, was written by a newer version, or belongs to another
// chain or token. The watcher does not guess in that situation — silently
// starting over would skip blocks, silently continuing would mix chains. The
// operator has to decide (typically: delete the checkpoint and set
// Config.StartBlock).
var ErrCheckpoint = errors.New("watcher: unusable checkpoint")

const checkpointVersion = 1

// maxLogServers bounds the list of endpoints a checkpoint remembers.
const maxLogServers = 64

// maxUncheckedRanges bounds the number of separate runs of unchecked blocks a
// checkpoint records (state.Unchecked). Beyond it two neighbouring runs are
// merged, the blocks between them included, and only the endpoints both had
// on record are kept: more is scanned again, nothing less.
const maxUncheckedRanges = 64

// blockRef identifies a block the watcher has seen.
type blockRef struct {
	Number uint64   `json:"n"`
	Hash   evm.Hash `json:"h"`
	// Time is the block timestamp (Unix seconds); 0 if not recorded.
	Time uint64 `json:"t,omitempty"`
}

// unixSeconds returns t as a block timestamp; 0 for the zero time.
func unixSeconds(t time.Time) uint64 {
	if t.IsZero() || t.Unix() <= 0 {
		return 0
	}
	return uint64(t.Unix())
}

// transferKey is the idempotency key of a transfer fact.
type transferKey struct {
	Block evm.Hash
	Tx    evm.Hash
	Log   uint64
}

func keyOf(t Transfer) transferKey {
	return transferKey{Block: t.BlockHash, Tx: t.TxHash, Log: t.LogIndex}
}

// compareTransfers is the one order transfers are put in, wherever the
// watcher puts them in an order: the facts it delivers, the incidents it
// raises, the checkpoint it writes. Ascending block number, then log index —
// the order of the chain — and then block hash and transaction hash, so that
// the order is total on the idempotency key: two transfers compare equal only
// if they are the same fact. (Two different transfers at one height and log
// index are a fact of life — one in each of two competing blocks, the old one
// tracked until its revert is proven. Without the hashes their order would be
// whatever the iteration order of a map happened to be.)
func compareTransfers(a, b Transfer) int {
	if c := cmp.Compare(a.BlockNumber, b.BlockNumber); c != 0 {
		return c
	}
	if c := cmp.Compare(a.LogIndex, b.LogIndex); c != 0 {
		return c
	}
	if c := bytes.Compare(a.BlockHash[:], b.BlockHash[:]); c != 0 {
		return c
	}
	return bytes.Compare(a.TxHash[:], b.TxHash[:])
}

// position is a place in a block: where exactly one log is.
type position struct {
	Block evm.Hash
	Log   uint64
}

// occupant returns the tracked transfer that sits at the position of t in
// the same block but belongs to another transaction, or nil. Two such
// transfers cannot both be true — a block has one log per index — and the
// watcher never tracks both: see deliver and applySnapshot, and decodeState,
// which refuses a checkpoint that says otherwise.
func (s *state) occupant(t Transfer) *tracked {
	for _, o := range s.Tracked {
		if o.BlockHash == t.BlockHash && o.LogIndex == t.LogIndex && o.TxHash != t.TxHash {
			return o
		}
	}
	return nil
}

// tracked is a transfer that was reported as observed and not reverted, and
// is still inside the reorg window.
type tracked struct {
	Transfer
	// delivered is true once the sink accepted the transfer in this
	// process. It is not persisted: after a restart, transfers in blocks
	// that are scanned again are reported again.
	delivered bool
	// timed is true once the endpoints have agreed, in this process, on
	// the timestamp of the transfer's block (BlockTime). Not persisted
	// either: a block time that comes from the checkpoint is confirmed
	// before a transfer is reported with it again (tick.stamp).
	timed bool
}

// state is the watcher's durable state.
type state struct {
	// Initialized is false until the first head was seen and the start
	// cursor chosen.
	Initialized bool
	// Cursor is the highest settled block.
	Cursor uint64
	// Anchors are blocks the cursor rested on, ascending by number,
	// spanning the reorg window. The newest is the cursor's own block
	// unless the cursor was rewound or is catching up.
	Anchors []blockRef
	// Finalized is the highest block reported as finalized (Number 0 if
	// none).
	Finalized blockRef
	// Tracked are the reported transfers inside the reorg window.
	Tracked map[transferKey]*tracked
	// LogServers are the labels (redacted URLs) of the endpoints that have
	// answered a log query, sorted. An endpoint listed here is one whose
	// log scan is required before blocks are settled, even if it refuses
	// log queries after a restart.
	LogServers []string
	// Unchecked are the blocks settled on a single endpoint's logs and not
	// scanned by a second endpoint since: what a second log endpoint has
	// to make up for when one appears (recheck). Each run records whose
	// log scans of its blocks are on record (uncheckedRange.By), so that
	// the endpoint whose logs alone settled them cannot make up for them
	// itself — whatever place it has in the list of endpoints by then.
	// Ascending, disjoint. The upper end of the blocks is recorded, not
	// derived from where the cursor happens to be: blocks can be settled
	// unchecked in the very tick in which the second endpoint comes back.
	Unchecked []uncheckedRange
	// NoLogServers are the labels of the endpoints found to have no log
	// service (evm.LogsHealth.NoService), sorted: endpoints that never
	// answered a log query and refused one again and again, over minutes.
	// Remembered so that a new process does not have to hold the head for
	// those minutes again before it settles on one endpoint's logs — and
	// only that: such an endpoint is still asked, and the first log query
	// it answers moves it to LogServers.
	NoLogServers []string
	// RescanTo, if above Cursor, says that the cursor was put back on
	// purpose — a requested re-scan (Rescan), a rewind for an address or a
	// pending transfer of the consumer's, or the re-scan that follows an
	// accepted finality conflict — and has not come back to this block yet.
	// Until it has, the blocks in between are scanned whatever the
	// consumer's position says (see tick.skipUnneeded).
	RescanTo uint64
}

// uncheckedRange is a run of blocks that was settled on one endpoint's logs
// and has not been scanned by a second endpoint since (invariant 3).
//
// By names the endpoints whose log scans of all of these blocks are on
// record: the one whose logs settled them, and any that has scanned them
// since. An endpoint is named by its label (evm.Endpoint.Label: scheme, host
// and port), which is what survives a reordering of the endpoints and a
// restart, and shows no provider key.
//
// A name is worth what the configuration says it is worth. While the label
// is configured it is that endpoint, and the operator's endpoints are
// independent providers (the premise of the trust model). A label that is no
// longer configured tells nothing about the endpoints that are: the provider
// behind it may be configured still, under another URL. So the blocks are
// checked once two different endpoints that are configured now are named.
// A scan by a named endpoint adds nothing, configured or not — it is the
// scan that has to be checked — and a named label that is no longer
// configured counts for nobody: after the URL of the endpoint that settled
// the blocks has changed, they take the scans of two endpoints configured
// now, as do the runs of a checkpoint written before the endpoints were
// recorded (which name nobody).
//
// Since is a block time (Unix seconds) no later than that of any block of
// the run — the time of a block below it that the cursor rested on — or 0 if
// none is known (a run of an older checkpoint). The consumer is told the
// lowest (Head.Checked): an address whose watch window ends after it may
// have been paid in blocks that are still to be scanned again, which can
// only find the payment while the address is watched.
type uncheckedRange struct {
	From  uint64   `json:"from"`
	To    uint64   `json:"to"`
	By    []string `json:"by,omitempty"`
	Since uint64   `json:"since,omitempty"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *uncheckedRange) UnmarshalJSON(data []byte) error {
	return strictjson.Object(data, strictjson.Fields{
		{Name: "from", Into: &r.From},
		{Name: "to", Into: &r.To},
		{Name: "by", Into: &r.By},
		{Name: "since", Into: &r.Since},
	}, strictjson.RejectUnknown)
}

// uncheckedSince returns the lowest Since of the unchecked runs (0 if one
// is not known), and false if there are none.
func (s *state) uncheckedSince() (uint64, bool) {
	if len(s.Unchecked) == 0 {
		return 0, false
	}
	since := s.Unchecked[0].Since
	for _, r := range s.Unchecked[1:] {
		since = min(since, r.Since)
	}
	return since, true
}

// timeBelow returns the block time of the newest block below n that the
// cursor rested on and whose time is on record (0 if there is none): no
// block from n on is stamped earlier.
func (s *state) timeBelow(n uint64) uint64 {
	for i := len(s.Anchors) - 1; i >= 0; i-- {
		if a := s.Anchors[i]; a.Number < n && a.Time != 0 {
			return a.Time
		}
	}
	return 0
}

// uncheckedBounds returns the lowest and the highest unchecked block (0 and
// 0 if there is none).
func (s *state) uncheckedBounds() (from, to uint64) {
	if len(s.Unchecked) == 0 {
		return 0, 0
	}
	return s.Unchecked[0].From, s.Unchecked[len(s.Unchecked)-1].To
}

// addUnchecked records that blocks from..to were settled on the logs of
// `by` alone. Whatever was on record for any of them is replaced: the scan
// that settled them now was made for the addresses watched now, and an
// earlier one, by whomever, may not have been.
func (s *state) addUnchecked(from, to uint64, by string) {
	if from == 0 || from > to {
		return
	}
	s.Unchecked = append(cutRanges(s.Unchecked, from, to), uncheckedRange{From: from, To: to, By: []string{by}, Since: s.timeBelow(from)})
	s.normaliseUnchecked()
}

// checkedBy records that `by` has scanned blocks from..to, of those that
// are unchecked: blocks that two different endpoints configured now
// (configured) have scanned leave the record (see uncheckedRange).
func (s *state) checkedBy(from, to uint64, by string, configured func(label string) bool) {
	if from > to {
		return
	}
	var out []uncheckedRange
	for _, r := range s.Unchecked {
		if r.To < from || r.From > to {
			out = append(out, r)
			continue
		}
		if r.From < from {
			out = append(out, uncheckedRange{From: r.From, To: from - 1, By: r.By, Since: r.Since})
		}
		inside := uncheckedRange{From: max(r.From, from), To: min(r.To, to), By: slices.Clone(r.By), Since: r.Since}
		if !slices.Contains(inside.By, by) {
			inside.By = append(inside.By, by)
		}
		counted := 0
		for _, l := range inside.By {
			if configured(l) {
				counted++
			}
		}
		if counted < 2 {
			out = append(out, inside)
		}
		if r.To > to {
			out = append(out, uncheckedRange{From: to + 1, To: r.To, By: r.By, Since: r.Since})
		}
	}
	s.Unchecked = out
	s.normaliseUnchecked()
}

// reconcileUnchecked applies the configuration (configured) to the record.
// Of the endpoints on record for a run only those configured now count
// (uncheckedRange), so the others are dropped: that keeps the record bounded
// by the configuration, however often it changed, and loses nothing — the
// blocks of a run that names nobody configured take the scans of two
// endpoints configured now, which is never less than the rule asks (one of
// them, at least, is not the endpoint whose logs settled the blocks). And a
// run on record for two endpoints configured now has been checked already:
// it leaves the record, without a scan — the returning pair of endpoints
// that both scanned it has nothing left to do for it. It reports whether the
// record changed.
func (s *state) reconcileUnchecked(configured func(label string) bool) bool {
	changed := false
	out := s.Unchecked[:0:0]
	for _, r := range s.Unchecked {
		by := slices.DeleteFunc(slices.Clone(r.By), func(l string) bool { return !configured(l) })
		if len(by) != len(r.By) {
			changed = true
		}
		if len(by) >= 2 {
			changed = true
			continue
		}
		r.By = by
		out = append(out, r)
	}
	if changed {
		s.Unchecked = out
		s.normaliseUnchecked()
	}
	return changed
}

// dropUncheckedBelow forgets the unchecked blocks below n: nobody who is
// watched can have been paid in them.
func (s *state) dropUncheckedBelow(n uint64) bool {
	if from, _ := s.uncheckedBounds(); from == 0 || from >= n {
		return false
	}
	s.Unchecked = cutRanges(s.Unchecked, 1, n-1)
	return true
}

// cutRanges returns rs without the blocks from..to.
func cutRanges(rs []uncheckedRange, from, to uint64) []uncheckedRange {
	var out []uncheckedRange
	for _, r := range rs {
		if r.To < from || r.From > to {
			out = append(out, r)
			continue
		}
		if r.From < from {
			out = append(out, uncheckedRange{From: r.From, To: from - 1, By: r.By, Since: r.Since})
		}
		if r.To > to {
			out = append(out, uncheckedRange{From: to + 1, To: r.To, By: r.By, Since: r.Since})
		}
	}
	return out
}

// normaliseUnchecked sorts the record, joins neighbouring runs on the record
// of the same endpoints, and keeps the record within maxUncheckedRanges.
func (s *state) normaliseUnchecked() {
	rs := s.Unchecked
	for i := range rs {
		rs[i].By = slices.Compact(slices.Sorted(slices.Values(rs[i].By)))
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].From < rs[j].From })
	var out []uncheckedRange
	for _, r := range rs {
		if n := len(out); n > 0 && out[n-1].To+1 == r.From && slices.Equal(out[n-1].By, r.By) {
			out[n-1].To, out[n-1].Since = r.To, min(out[n-1].Since, r.Since)
			continue
		}
		out = append(out, r)
	}
	for len(out) > maxUncheckedRanges {
		// Merge the two neighbours closest to each other. The blocks
		// between them are scanned again; of the endpoints on record only
		// those both name are kept.
		best := 0
		for i := 1; i+1 < len(out); i++ {
			if out[i+1].From-out[i].To < out[best+1].From-out[best].To {
				best = i
			}
		}
		a, b := out[best], out[best+1]
		var by []string
		for _, l := range a.By {
			if slices.Contains(b.By, l) {
				by = append(by, l)
			}
		}
		out[best] = uncheckedRange{From: a.From, To: b.To, By: by, Since: min(a.Since, b.Since)}
		out = append(out[:best+1], out[best+2:]...)
	}
	if len(out) == 0 {
		out = nil
	}
	s.Unchecked = out
}

type checkpointJSON struct {
	Version   int                  `json:"v"`
	ChainID   uint64               `json:"chain_id"`
	Token     ethaddr.Address      `json:"token"`
	Cursor    uint64               `json:"cursor"`
	Anchors   []blockRef           `json:"anchors,omitempty"`
	Finalized *blockRef            `json:"finalized,omitempty"`
	Transfers []checkpointTransfer `json:"transfers,omitempty"`
	// Added later, optional: a checkpoint without them is a valid one.
	LogServers []string `json:"log_servers,omitempty"`
	// UncheckedFrom and UncheckedTo are how checkpoints recorded the
	// unchecked blocks before the endpoints that had scanned them were
	// recorded with them (Unchecked): read, never written.
	UncheckedFrom uint64           `json:"unchecked_from,omitempty"`
	UncheckedTo   uint64           `json:"unchecked_to,omitempty"`
	Unchecked     []uncheckedRange `json:"unchecked,omitempty"`
	NoLogServers  []string         `json:"no_log_servers,omitempty"`
	RescanTo      uint64           `json:"rescan_to,omitempty"`
}

type checkpointTransfer struct {
	From      ethaddr.Address `json:"from"`
	To        ethaddr.Address `json:"to"`
	Amount    string          `json:"amount"` // decimal, base units
	TxHash    evm.Hash        `json:"tx"`
	LogIndex  uint64          `json:"log"`
	Block     uint64          `json:"block"`
	BlockHash evm.Hash        `json:"block_hash"`
	// Time is the block's timestamp (Unix seconds); absent in checkpoints
	// written before it was recorded.
	Time uint64 `json:"time,omitempty"`
}

// A checkpoint is read as strictly as it is written: every member under its
// exact name, once, and no member this build does not know (strictjson).
// encoding/json alone would accept "Cursor" for "cursor", let the second of
// two "cursor" members win and skip anything it does not recognise — so a
// damaged or doctored checkpoint could be read as something other than what
// it says, and a member written by a newer build (which may be a duty: blocks
// still to be scanned by a second endpoint) would be dropped without a word.
// The watcher writes none of that, so none of it is accepted.

// UnmarshalJSON implements json.Unmarshaler.
func (b *blockRef) UnmarshalJSON(data []byte) error {
	return strictjson.Object(data, strictjson.Fields{
		{Name: "n", Into: &b.Number},
		{Name: "h", Into: &b.Hash},
		{Name: "t", Into: &b.Time},
	}, strictjson.RejectUnknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *checkpointJSON) UnmarshalJSON(data []byte) error {
	return strictjson.Object(data, strictjson.Fields{
		{Name: "v", Into: &c.Version},
		{Name: "chain_id", Into: &c.ChainID},
		{Name: "token", Into: &c.Token},
		{Name: "cursor", Into: &c.Cursor},
		{Name: "anchors", Into: &c.Anchors},
		{Name: "finalized", Into: &c.Finalized},
		{Name: "transfers", Into: &c.Transfers},
		{Name: "log_servers", Into: &c.LogServers},
		{Name: "unchecked_from", Into: &c.UncheckedFrom},
		{Name: "unchecked_to", Into: &c.UncheckedTo},
		{Name: "unchecked", Into: &c.Unchecked},
		{Name: "no_log_servers", Into: &c.NoLogServers},
		{Name: "rescan_to", Into: &c.RescanTo},
	}, strictjson.RejectUnknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *checkpointTransfer) UnmarshalJSON(data []byte) error {
	return strictjson.Object(data, strictjson.Fields{
		{Name: "from", Into: &c.From},
		{Name: "to", Into: &c.To},
		{Name: "amount", Into: &c.Amount},
		{Name: "tx", Into: &c.TxHash},
		{Name: "log", Into: &c.LogIndex},
		{Name: "block", Into: &c.Block},
		{Name: "block_hash", Into: &c.BlockHash},
		{Name: "time", Into: &c.Time},
	}, strictjson.RejectUnknown)
}

// checkpointVersionOf reads the version of a checkpoint, and nothing else:
// a checkpoint of another version is refused for being of another version,
// whatever else is in it.
func checkpointVersionOf(data []byte) (int, error) {
	version := 0
	if err := strictjson.Object(data, strictjson.Fields{{Name: "v", Into: &version}}, strictjson.IgnoreUnknown); err != nil {
		return 0, fmt.Errorf("%w: not a checkpoint: %v", ErrCheckpoint, err)
	}
	if version != checkpointVersion {
		return 0, fmt.Errorf("%w: version %d, this build understands version %d", ErrCheckpoint, version, checkpointVersion)
	}
	return version, nil
}

// encode serialises the state.
func (s *state) encode(chainID uint64, token ethaddr.Address) ([]byte, error) {
	cp := checkpointJSON{
		Version: checkpointVersion,
		ChainID: chainID,
		Token:   token,
		Cursor:  s.Cursor,
		Anchors: s.Anchors,

		LogServers:   s.LogServers,
		Unchecked:    s.Unchecked,
		NoLogServers: s.NoLogServers,
	}
	if s.RescanTo > s.Cursor {
		cp.RescanTo = s.RescanTo // below or at the cursor it says nothing
	}
	if s.Finalized.Number != 0 {
		f := s.Finalized
		cp.Finalized = &f
	}
	// Canonical output: the same state is always the same bytes. The order
	// is total (compareTransfers), so it does not depend on the iteration
	// order of the map.
	list := make([]*tracked, 0, len(s.Tracked))
	for _, t := range s.Tracked {
		list = append(list, t)
	}
	slices.SortFunc(list, func(a, b *tracked) int { return compareTransfers(a.Transfer, b.Transfer) })
	cp.Transfers = make([]checkpointTransfer, 0, len(list))
	for _, t := range list {
		cp.Transfers = append(cp.Transfers, checkpointTransfer{
			From: t.From, To: t.To, Amount: t.Amount.String(),
			TxHash: t.TxHash, LogIndex: t.LogIndex, Block: t.BlockNumber, BlockHash: t.BlockHash,
			Time: unixSeconds(t.BlockTime),
		})
	}
	return json.Marshal(cp)
}

// decodeState parses and validates a checkpoint.
func decodeState(data []byte, chainID uint64, token ethaddr.Address) (state, error) {
	if _, err := checkpointVersionOf(data); err != nil {
		return state{}, err
	}
	var cp checkpointJSON
	if err := json.Unmarshal(data, &cp); err != nil {
		return state{}, fmt.Errorf("%w: not a valid checkpoint: %v", ErrCheckpoint, err)
	}
	if cp.ChainID != chainID {
		return state{}, fmt.Errorf("%w: written for chain id %d, configured chain id is %d", ErrCheckpoint, cp.ChainID, chainID)
	}
	if cp.Token != token {
		return state{}, fmt.Errorf("%w: written for token %s, configured token is %s", ErrCheckpoint, cp.Token, token)
	}
	s := state{
		Initialized: true,
		Cursor:      cp.Cursor,
		Anchors:     cp.Anchors,
		Tracked:     make(map[transferKey]*tracked, len(cp.Transfers)),

		RescanTo: cp.RescanTo,
	}
	if len(cp.LogServers) > maxLogServers || len(cp.NoLogServers) > maxLogServers {
		return state{}, fmt.Errorf("%w: more than %d log endpoints", ErrCheckpoint, maxLogServers)
	}
	for _, label := range cp.LogServers {
		if label == "" || len(label) > 512 || slices.Contains(s.LogServers, label) {
			return state{}, fmt.Errorf("%w: invalid log endpoint entry", ErrCheckpoint)
		}
		s.LogServers = append(s.LogServers, label)
	}
	sort.Strings(s.LogServers)
	for _, label := range cp.NoLogServers {
		// An endpoint either has answered a log query or is held to have no
		// log service: a checkpoint that says both says nothing.
		if label == "" || len(label) > 512 || slices.Contains(s.NoLogServers, label) || slices.Contains(s.LogServers, label) {
			return state{}, fmt.Errorf("%w: invalid entry in the list of endpoints without a log service", ErrCheckpoint)
		}
		s.NoLogServers = append(s.NoLogServers, label)
	}
	sort.Strings(s.NoLogServers)
	switch from, to := cp.UncheckedFrom, cp.UncheckedTo; {
	case (from != 0 || to != 0) && len(cp.Unchecked) != 0:
		return state{}, fmt.Errorf("%w: unchecked blocks recorded twice", ErrCheckpoint)
	case from == 0 && to != 0:
		return state{}, fmt.Errorf("%w: an unchecked range with an end and no beginning", ErrCheckpoint)
	case from != 0:
		// Written before the endpoints that had scanned the blocks were
		// recorded (and, without an end, before the end was): nobody is on
		// record, and the blocks take the scans of two endpoints.
		if to == 0 {
			to = max(s.Cursor, from)
		}
		if to < from {
			return state{}, fmt.Errorf("%w: an unchecked range that ends before it begins", ErrCheckpoint)
		}
		s.Unchecked = []uncheckedRange{{From: from, To: to}}
	}
	if len(cp.Unchecked) > maxUncheckedRanges {
		return state{}, fmt.Errorf("%w: more than %d unchecked ranges", ErrCheckpoint, maxUncheckedRanges)
	}
	for i, r := range cp.Unchecked {
		switch {
		case r.From == 0 || r.To < r.From:
			return state{}, fmt.Errorf("%w: an unchecked range without blocks", ErrCheckpoint)
		case i > 0 && r.From <= cp.Unchecked[i-1].To:
			return state{}, fmt.Errorf("%w: unchecked ranges that are not ascending and disjoint", ErrCheckpoint)
		case len(r.By) > maxLogServers:
			return state{}, fmt.Errorf("%w: an unchecked range with more than %d endpoints", ErrCheckpoint, maxLogServers)
		}
		for j, label := range r.By {
			if label == "" || len(label) > 512 || slices.Contains(r.By[:j], label) {
				return state{}, fmt.Errorf("%w: invalid endpoint of an unchecked range", ErrCheckpoint)
			}
		}
		s.Unchecked = append(s.Unchecked, uncheckedRange{From: r.From, To: r.To, By: slices.Clone(r.By), Since: r.Since})
	}
	if s.RescanTo <= s.Cursor {
		s.RescanTo = 0
	}
	for i := 1; i < len(s.Anchors); i++ {
		if s.Anchors[i].Number <= s.Anchors[i-1].Number {
			return state{}, fmt.Errorf("%w: anchors are not ascending", ErrCheckpoint)
		}
	}
	for _, a := range s.Anchors {
		if a.Hash.IsZero() {
			return state{}, fmt.Errorf("%w: anchor without hash", ErrCheckpoint)
		}
	}
	if cp.Finalized != nil {
		if cp.Finalized.Hash.IsZero() || cp.Finalized.Number == 0 {
			return state{}, fmt.Errorf("%w: finalized block without number or hash", ErrCheckpoint)
		}
		s.Finalized = *cp.Finalized
	}
	positions := make(map[position]evm.Hash, len(cp.Transfers))
	for _, ct := range cp.Transfers {
		amount, ok := new(big.Int).SetString(ct.Amount, 10)
		if !ok || amount.Sign() <= 0 || amount.BitLen() > 256 {
			return state{}, fmt.Errorf("%w: invalid transfer amount", ErrCheckpoint)
		}
		if ct.BlockHash.IsZero() || ct.TxHash.IsZero() {
			return state{}, fmt.Errorf("%w: transfer without block or transaction hash", ErrCheckpoint)
		}
		t := Transfer{
			Token: token, From: ct.From, To: ct.To, Amount: amount,
			TxHash: ct.TxHash, LogIndex: ct.LogIndex, BlockNumber: ct.Block, BlockHash: ct.BlockHash,
		}
		if ct.Time > math.MaxInt64/2 {
			return state{}, fmt.Errorf("%w: invalid block time", ErrCheckpoint)
		}
		if ct.Time != 0 {
			t.BlockTime = time.Unix(int64(ct.Time), 0).UTC()
		}
		// The watcher writes every transfer once, and never two at one
		// position of one block. A checkpoint that has either is damaged,
		// and which of the two entries is the true one — they may differ in
		// amount or recipient — is not for the decoder to guess. (The same
		// height and log index in two different blocks is another matter:
		// competing blocks, both tracked until one is proven gone.)
		k := keyOf(t)
		if _, dup := s.Tracked[k]; dup {
			return state{}, fmt.Errorf("%w: transfer %s#%d of block %s is listed twice", ErrCheckpoint, t.TxHash, t.LogIndex, t.BlockHash)
		}
		at := position{Block: t.BlockHash, Log: t.LogIndex}
		if other, taken := positions[at]; taken {
			return state{}, fmt.Errorf("%w: transfers %s and %s both claim log index %d of block %s", ErrCheckpoint,
				other, t.TxHash, t.LogIndex, t.BlockHash)
		}
		positions[at] = t.TxHash
		s.Tracked[k] = &tracked{Transfer: t}
	}
	return s, nil
}

// CheckpointInfo is what InspectCheckpoint reads from a checkpoint.
type CheckpointInfo struct {
	// ChainID and Token say which chain and token the checkpoint is for.
	ChainID uint64
	Token   ethaddr.Address
	// Cursor is the processed head recorded in it.
	Cursor uint64
	// Transfers is the number of transfers it tracks.
	Transfers int
}

// InspectCheckpoint parses a checkpoint written by a Watcher without using
// it, so that a caller can decide what to do with it before New: resume from
// it, rewind it (Rescan), or discard it because it belongs to another chain
// or token. It returns an error wrapping ErrCheckpoint if the data is not a
// checkpoint this version understands.
func InspectCheckpoint(data []byte) (CheckpointInfo, error) {
	if _, err := checkpointVersionOf(data); err != nil {
		return CheckpointInfo{}, err
	}
	var cp checkpointJSON
	if err := json.Unmarshal(data, &cp); err != nil {
		return CheckpointInfo{}, fmt.Errorf("%w: not a valid checkpoint: %v", ErrCheckpoint, err)
	}
	if _, err := decodeState(data, cp.ChainID, cp.Token); err != nil {
		return CheckpointInfo{}, err
	}
	return CheckpointInfo{ChainID: cp.ChainID, Token: cp.Token, Cursor: cp.Cursor, Transfers: len(cp.Transfers)}, nil
}

// load reads the checkpoint from the store.
func (w *Watcher) load(ctx context.Context) error {
	data, err := w.cfg.Store.LoadCheckpoint(ctx)
	if err != nil {
		return fmt.Errorf("watcher: load checkpoint: %w", err)
	}
	if len(data) == 0 {
		w.st = state{Tracked: make(map[transferKey]*tracked)}
		w.loaded = true
		return nil
	}
	st, err := decodeState(data, w.cfg.Client.ChainID(), w.cfg.Token)
	if err != nil {
		return err
	}
	// Endpoints that are not configured any more are forgotten.
	configured := make([]string, 0, w.cfg.Client.Len())
	for _, es := range w.cfg.Client.Status() {
		configured = append(configured, es.URL)
	}
	st.LogServers = slices.DeleteFunc(st.LogServers, func(label string) bool { return !slices.Contains(configured, label) })
	st.NoLogServers = slices.DeleteFunc(st.NoLogServers, func(label string) bool { return !slices.Contains(configured, label) })
	w.st = st
	w.loaded = true
	w.log.Info("chain watcher resuming from checkpoint",
		"cursor", st.Cursor, "tracked_transfers", len(st.Tracked), "finalized", st.Finalized.Number)
	return nil
}

// save writes the checkpoint to the store.
func (w *Watcher) save(ctx context.Context) error {
	data, err := w.st.encode(w.cfg.Client.ChainID(), w.cfg.Token)
	if err != nil {
		return fmt.Errorf("watcher: encode checkpoint: %w", err)
	}
	if err := w.cfg.Store.SaveCheckpoint(ctx, data); err != nil {
		// What is in memory is not what is on disk now: it is written in
		// the next tick, whether or not anything else changes by then. (A
		// rewind for an address that can have been paid lower, kept in
		// memory and never written because nothing else changed, was lost
		// with a restart: the process resumed from the checkpoint before
		// it, and the blocks were not scanned again for the address.)
		w.unsaved = true
		return fmt.Errorf("watcher: save checkpoint: %w", err)
	}
	w.unsaved = false
	return nil
}

// newestAnchor returns the most recent anchor.
func (s *state) newestAnchor() (blockRef, bool) {
	if len(s.Anchors) == 0 {
		return blockRef{}, false
	}
	return s.Anchors[len(s.Anchors)-1], true
}

// anchorAt returns the anchor at exactly the given block number.
func (s *state) anchorAt(number uint64) (blockRef, bool) {
	for i := len(s.Anchors) - 1; i >= 0; i-- {
		switch a := s.Anchors[i]; {
		case a.Number == number:
			return a, true
		case a.Number < number:
			return blockRef{}, false
		}
	}
	return blockRef{}, false
}

// addAnchor records the block the cursor is about to rest on and prunes the
// list.
//
// Anchors are kept at least `spacing` blocks apart, except for the newest
// one: if the previous newest anchor is closer than spacing to the one before
// it, it is replaced rather than kept (the cursor has left it). The list is
// cut below the newest anchor that is at least `window` blocks under the new
// one, so it always spans the whole reorg window with a bounded number of
// entries.
func (s *state) addAnchor(ref blockRef, window, spacing uint64) {
	// Anything at or above the new anchor is superseded.
	for len(s.Anchors) > 0 && s.Anchors[len(s.Anchors)-1].Number >= ref.Number {
		s.Anchors = s.Anchors[:len(s.Anchors)-1]
	}
	if n := len(s.Anchors); n >= 2 && s.Anchors[n-1].Number-s.Anchors[n-2].Number < spacing {
		s.Anchors[n-1] = ref
	} else {
		s.Anchors = append(s.Anchors, ref)
	}
	if ref.Number > window {
		horizon := ref.Number - window
		floor := -1
		for i, a := range s.Anchors {
			if a.Number <= horizon {
				floor = i
			}
		}
		if floor > 0 {
			s.Anchors = append([]blockRef(nil), s.Anchors[floor:]...)
		}
	}
}
