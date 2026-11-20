package watcher_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// Tests for lying endpoints at the level of the watcher — a processed head
// taken from a single endpoint, a log scan made on a single endpoint, and
// clock skew: whatever the consumer does with the
// facts, a head that one endpoint invented is never reported.
//
// The harness checks "the head rule" on every head reported by every test:
// the head is a block of the canonical chain with its real number, hash and
// timestamp, and every transfer at or below it has been delivered. A lying
// endpoint that got anything past the watcher fails that check.

// incidents collects Config.OnIncident calls.
type incidents struct {
	mu   sync.Mutex
	list []watcher.Incident
}

func (in *incidents) add(i watcher.Incident) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.list = append(in.list, i)
}

func (in *incidents) count(kind watcher.IncidentKind) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	n := 0
	for _, i := range in.list {
		if i.Kind == kind {
			n++
		}
	}
	return n
}

func (in *incidents) blamed(kind watcher.IncidentKind) []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []string
	for _, i := range in.list {
		if i.Kind == kind {
			out = append(out, i.Endpoint)
		}
	}
	return out
}

func withIncidents(in *incidents) option {
	return withConfig(func(c *watcher.Config) { c.OnIncident = in.add })
}

func hexUint(v uint64) string { return "0x" + strconv.FormatUint(v, 16) }

// blockParam parses the block parameter of eth_getBlockByNumber.
func blockParam(params []json.RawMessage) (tag string, number uint64, numeric bool) {
	if len(params) == 0 || json.Unmarshal(params[0], &tag) != nil {
		return "", 0, false
	}
	if !strings.HasPrefix(tag, "0x") {
		return tag, 0, false
	}
	n, err := strconv.ParseUint(tag[2:], 16, 64)
	return tag, n, err == nil
}

func headerJSON(blk evmtest.Block, time uint64) map[string]any {
	return map[string]any{
		"number": hexUint(blk.Number), "hash": blk.Hash.String(), "parentHash": blk.ParentHash.String(),
		"timestamp": hexUint(time), "transactions": []string{},
	}
}

// lieAboutTime makes a node answer header requests by number truthfully
// except for the timestamp, which is shifted.
func lieAboutTime(chain *evmtest.Chain, shift int64) func(b *evmtest.Behavior) {
	return func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			_, n, numeric := blockParam(params)
			if !numeric {
				return nil, nil, false
			}
			blk, ok := chain.BlockByNumber(n)
			if !ok {
				return nil, nil, false
			}
			return headerJSON(blk, uint64(int64(blk.Time)+shift)), nil, true
		}
	}
}

// lieAboutHash makes a node return another hash for every block at or above
// `from` that is asked for by number.
func lieAboutHash(chain *evmtest.Chain, from uint64) func(b *evmtest.Behavior) {
	return func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			_, n, numeric := blockParam(params)
			blk, ok := chain.BlockByNumber(n)
			if !numeric || !ok || n < from {
				return nil, nil, false
			}
			blk.Hash[0] ^= 0xff
			return headerJSON(blk, blk.Time), nil, true
		}
	}
}

// lieAboutHeight makes a node claim that the chain is `jump` blocks longer
// than it is, with invented blocks (and no logs) above the real head.
func lieAboutHeight(chain *evmtest.Chain, jump uint64) func(b *evmtest.Behavior) {
	fake := func(n uint64) map[string]any {
		var h, ph evm.Hash
		h[0], ph[0], ph[1] = 0xfa, 0xfa, 0x01
		for i := 0; i < 8; i++ {
			h[31-i], ph[31-i] = byte(n>>(8*i)), byte((n-1)>>(8*i))
		}
		tip := chain.Head()
		return map[string]any{
			"number": hexUint(n), "hash": h.String(), "parentHash": ph.String(),
			"timestamp": hexUint(tip.Time + (n - tip.Number)), "transactions": []string{},
		}
	}
	return func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			real := chain.Head().Number
			switch method {
			case "eth_blockNumber":
				return hexUint(real + jump), nil, true
			case "eth_getBlockByNumber":
				tag, n, numeric := blockParam(params)
				switch {
				case tag == "latest":
					return fake(real + jump), nil, true
				case tag == "finalized":
					return fake(real + jump - 2), nil, true
				case numeric && n > real && n <= real+jump:
					return fake(n), nil, true
				}
			case "eth_getLogs":
				var f struct {
					FromBlock string `json:"fromBlock"`
				}
				if len(params) > 0 && json.Unmarshal(params[0], &f) == nil && strings.HasPrefix(f.FromBlock, "0x") {
					if n, err := strconv.ParseUint(f.FromBlock[2:], 16, 64); err == nil && n > real {
						return []any{}, nil, true
					}
				}
			}
			return nil, nil, false
		}
	}
}

func honest(b *evmtest.Behavior) { b.Intercept = nil; b.DropLogs = nil; b.ExtraLogs = nil }

// headsOtherThan returns the reported heads that are not the block `at`.
// (The same head is reported again when the finalized block it carries has
// moved; that is not a new head.)
func headsOtherThan(events []event, at watcher.Head) []uint64 {
	var out []uint64
	for _, e := range events {
		if e.kind == "head" && (e.head.Number != at.Number || e.head.Hash != at.Hash || !e.head.Time.Equal(at.Time)) {
			out = append(out, e.head.Number)
		}
	}
	return out
}

// TestHeadTimestampLie: one of two endpoints lies about block
// timestamps. No head with a forged timestamp is reported — in fact no head
// at all while the two disagree — whichever of the two the liar is, and
// whatever the size of the lie. Payments are still observed; when the
// endpoint stops lying the head catches up.
func TestHeadTimestampLie(t *testing.T) {
	shifts := map[string]int64{
		"40 days ahead": 40 * 24 * 3600, "90 seconds ahead": 90, "one second ahead": 1,
		"an hour back": -3600, "one second back": -1,
	}
	for name, shift := range shifts {
		for liar := 0; liar < 2; liar++ {
			t.Run(fmt.Sprintf("%s, liar is endpoint %d", name, liar), func(t *testing.T) {
				var in incidents
				h := newHarness(t, withNodes(2), withIncidents(&in))
				h.watch.add(addr(1))
				h.mineTicking(testSettle + 2)
				before := h.sink.lastHead()
				h.sink.take()

				h.nodes[liar].Set(lieAboutTime(h.chain, shift))
				h.pay(addr(1), 10)
				for i := 0; i < 12; i++ {
					h.chain.Mine(2)
					// The tick may fail (the liar found out and set
					// aside) or succeed without settling anything.
					_ = h.w.Tick(context.Background())
				}
				events := h.sink.take()
				if hs := headsOtherThan(events, before); len(hs) != 0 {
					t.Fatalf("heads reported while the endpoints disagree about block time: %v", hs)
				}
				if st := h.w.Status(); st.Cursor != before.Number {
					t.Fatalf("cursor moved from %d to %d", before.Number, st.Cursor)
				}
				if in.count(watcher.IncidentHeaderMismatch) == 0 {
					t.Fatal("no incident raised")
				}
				// Neither is the transfer: it is reported with the
				// timestamp of its block, and that is a fact the two
				// endpoints do not agree on.
				if n := len(h.sink.liveKeys()); n != 0 {
					t.Fatalf("%d transfers observed while the endpoints disagree about the time of its block", n)
				}
				if st := h.w.Status(); st.Unverified != 1 {
					t.Fatalf("status = %+v", st)
				}

				h.nodes[liar].Set(honest)
				h.passCooldowns()
				h.mineTicking(testSettle + 1)
				h.assertModel(startBlock)
				if got := h.sink.lastHead().Number; got != h.chain.Head().Number-testSettle {
					t.Fatalf("head = %d after the lie ended, tip = %d", got, h.chain.Head().Number)
				}
			})
		}
	}
}

// TestHeadLieWithThirdEndpoint: with three endpoints the liar is
// identified by the other two, set aside, and the head keeps moving.
func TestHeadLieWithThirdEndpoint(t *testing.T) {
	lies := map[string]func(h *harness) func(*evmtest.Behavior){
		"timestamp": func(h *harness) func(*evmtest.Behavior) { return lieAboutTime(h.chain, 1) },
		"hash":      func(h *harness) func(*evmtest.Behavior) { return lieAboutHash(h.chain, startBlock) },
	}
	for name, lie := range lies {
		for liar := 0; liar < 2; liar++ {
			t.Run(fmt.Sprintf("%s, liar is endpoint %d", name, liar), func(t *testing.T) {
				var in incidents
				h := newHarness(t, withNodes(3), withIncidents(&in))
				h.watch.add(addr(1))
				h.mineTicking(testSettle + 2)
				h.nodes[liar].Set(lie(h))
				h.pay(addr(1), 10)
				for i := 0; i < 10; i++ {
					h.chain.Mine(2)
					_ = h.w.Tick(context.Background())
				}
				if got, want := h.sink.lastHead().Number, h.chain.Head().Number-testSettle; got != want {
					t.Fatalf("head = %d, want %d: one liar among three endpoints must not stop the head", got, want)
				}
				h.assertModel(startBlock)
				// The liar is set aside: by the header comparison, or — a
				// primary whose blocks contradict the remembered ones —
				// by the fork check.
				for _, ep := range in.blamed(watcher.IncidentHeaderMismatch) {
					if ep != evm.RedactURL(h.urls[liar]) {
						t.Fatalf("blamed %q, the liar is %q", ep, evm.RedactURL(h.urls[liar]))
					}
				}
				for i, s := range h.client.Status() {
					if s.Healthy == (i == liar) {
						t.Fatalf("endpoint %d: healthy = %v; the liar is endpoint %d", i, s.Healthy, liar)
					}
				}
			})
		}
	}
}

// TestHeadHashLie: the verifier, or the primary, reports another
// block at the height that is about to be settled. Two endpoints cannot tell
// who is right: nothing is settled.
func TestHeadHashLie(t *testing.T) {
	for liar := 0; liar < 2; liar++ {
		t.Run(fmt.Sprintf("liar is endpoint %d", liar), func(t *testing.T) {
			var in incidents
			h := newHarness(t, withNodes(2), withIncidents(&in))
			h.watch.add(addr(1))
			h.mineTicking(testSettle + 2)
			before := h.sink.lastHead()
			h.sink.take()
			h.nodes[liar].Set(lieAboutHash(h.chain, before.Number+1))
			for i := 0; i < 8; i++ {
				h.chain.Mine(2)
				_ = h.w.Tick(context.Background())
			}
			if hs := headsOtherThan(h.sink.take(), before); len(hs) != 0 {
				t.Fatalf("heads reported: %v", hs)
			}
			st := h.w.Status()
			if st.Cursor != before.Number || st.Hold == "" {
				t.Fatalf("status = %+v", st)
			}
			if blamed := in.blamed(watcher.IncidentHeaderMismatch); len(blamed) == 0 || blamed[0] != "" {
				t.Fatalf("incidents blame %q; with two endpoints nobody can be blamed", blamed)
			}
			h.nodes[liar].Set(honest)
			h.passCooldowns()
			h.mineTicking(2)
			h.assertModel(startBlock)
		})
	}
}

// TestHeadHeightLie: one endpoint claims the chain is hundreds of
// blocks longer than it is. The cursor never passes what the other endpoint
// has, nothing above the real chain is persisted, the head keeps moving at
// the honest endpoint's pace — and a transfer is never ahead of its
// confirmations: the reported head stays SettleDepth below the real tip.
func TestHeadHeightLie(t *testing.T) {
	for liar := 0; liar < 2; liar++ {
		t.Run(fmt.Sprintf("liar is endpoint %d", liar), func(t *testing.T) {
			h := newHarness(t, withNodes(2))
			h.watch.add(addr(1), addr(2))
			h.mineTicking(testSettle + 2)
			h.nodes[liar].Set(lieAboutHeight(h.chain, 300))
			h.client.Refresh(t.Context()) // the pool now believes the liar is ahead
			h.pay(addr(1), 10)
			for i := 0; i < 10; i++ {
				h.chain.Mine(2)
				if i == 5 {
					h.pay(addr(2), 20)
				}
				_ = h.w.Tick(context.Background())
				real := h.chain.Head().Number
				if got := h.sink.lastHead().Number; got > real-testSettle {
					t.Fatalf("head %d reported with the real tip at %d: confirmations that do not exist", got, real)
				}
				info, err := watcher.InspectCheckpoint(h.store.snapshot())
				if err != nil || info.Cursor > real-testSettle {
					t.Fatalf("checkpoint cursor %d with the real tip at %d (err %v)", info.Cursor, real, err)
				}
				if tip := h.w.Status().Tip.Number; tip > real {
					t.Fatalf("status tip %d above the real tip %d", tip, real)
				}
			}
			if got, want := h.sink.lastHead().Number, h.chain.Head().Number-testSettle; got != want {
				t.Fatalf("head = %d, want %d: the lie must not stop the head either", got, want)
			}
			h.assertModel(startBlock)
			// A restart in the middle of it changes nothing.
			h.restart()
			h.mineTicking(3)
			h.assertModel(startBlock)
		})
	}

	t.Run("first start", func(t *testing.T) {
		// Without a checkpoint the start position is derived from the
		// chain's height, and that has to be a height the endpoints agree
		// on. Two endpoints that are 300 blocks apart agree on nothing: one
		// of them is wrong, and there is no telling which. (An older version
		// started from the lower of the two heads, which is the
		// right one here — and wherever a lagging or lying endpoint puts it
		// when the lie goes the other way.) So it does not start, says why,
		// and starts — at the real height — when the lie is over.
		for liar := 0; liar < 2; liar++ {
			h := newHarness(t, withNodes(2))
			h.nodes[liar].Set(lieAboutHeight(h.chain, 300))
			h.watch.add(addr(1))
			for i := 0; i < 3; i++ {
				err := h.tickErr()
				if !strings.Contains(err.Error(), "cannot choose a start position") || !strings.Contains(err.Error(), "300 blocks apart") {
					t.Fatalf("liar %d: err = %v", liar, err)
				}
				h.chain.Mine(1)
			}
			if st := h.w.Status(); st.Cursor != 0 || h.sink.lastHead().Number != 0 || len(h.store.snapshot()) != 0 {
				t.Fatalf("liar %d: started on a height only one endpoint has: cursor %d, head %d, checkpoint %d bytes",
					liar, st.Cursor, h.sink.lastHead().Number, len(h.store.snapshot()))
			}
			h.nodes[liar].Set(honest)
			h.tick()
			if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle {
				t.Fatalf("liar %d: start cursor = %d, real tip = %d", liar, st.Cursor, h.chain.Head().Number)
			}
			h.pay(addr(1), 5)
			h.mineTicking(testSettle + 2)
			h.assertModel(startBlock)
		}
	})

	t.Run("first start, three endpoints", func(t *testing.T) {
		// With a third endpoint the height is not in doubt: it is the one
		// two of the three have. The watcher starts there at once, whichever
		// endpoint lies, and never above the real chain.
		for liar := 0; liar < 3; liar++ {
			h := newHarness(t, withNodes(3))
			h.nodes[liar].Set(lieAboutHeight(h.chain, 300))
			h.watch.add(addr(1))
			h.tick()
			if st := h.w.Status(); st.Cursor != h.chain.Head().Number-testSettle {
				t.Fatalf("liar %d: start cursor = %d, real tip = %d", liar, st.Cursor, h.chain.Head().Number)
			}
			h.pay(addr(1), 5)
			h.mineTicking(testSettle + 2)
			h.assertModel(startBlock)
		}
	})
}

// TestBlocksFromTheFuture: a block stamped ahead of the local clock
// is refused whoever vouches for it — also with a single endpoint, where
// nothing else can be cross-checked.
func TestBlocksFromTheFuture(t *testing.T) {
	t.Run("single endpoint", func(t *testing.T) {
		h := newHarness(t)
		h.watch.add(addr(1))
		h.chain.SetTime(h.clock.Now())
		h.mineTicking(testSettle + 2)
		before := h.sink.lastHead()
		h.sink.take()

		// Within tolerance: a clock that is a minute off is not an attack.
		h.chain.SetTime(h.clock.Now().Add(time.Minute))
		h.mineTicking(testSettle + 1)
		if got := h.sink.lastHead(); got.Number <= before.Number {
			t.Fatalf("head did not move with blocks one minute ahead")
		}
		before = h.sink.lastHead()
		h.sink.take()

		h.chain.SetTime(h.clock.Now().Add(10 * time.Minute))
		h.pay(addr(1), 10)
		for i := 0; i < 5; i++ {
			h.chain.Mine(3)
			if err := h.tickErr(); !errors.Is(err, watcher.ErrFutureBlock) {
				t.Fatalf("err = %v", err)
			}
		}
		if events := h.sink.take(); len(events) != 0 {
			t.Fatalf("facts from blocks of the future: %v", kinds(events))
		}
		if st := h.w.Status(); st.Cursor != before.Number || st.ClockSkew > -9*time.Minute {
			t.Fatalf("status = %+v", st)
		}
		// The clock catches up: the same blocks are fine now.
		h.clock.Advance(10 * time.Minute)
		h.tick()
		h.assertModel(startBlock)
	})

	t.Run("settled block ahead of the clock, tip not", func(t *testing.T) {
		// The endpoint keeps its tip plausible and forges the timestamps
		// of the blocks that are settled.
		h := newHarness(t)
		h.chain.SetTime(h.clock.Now())
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		before := h.sink.lastHead()
		h.nodes[0].Set(lieAboutTime(h.chain, 40*24*3600))
		for i := 0; i < 5; i++ {
			h.chain.Mine(2)
			if err := h.tickErr(); !errors.Is(err, watcher.ErrInconsistentEndpoint) {
				t.Fatalf("err = %v", err)
			}
		}
		if got := h.sink.lastHead(); got != before {
			t.Fatalf("head moved to %d (%s)", got.Number, got.Time)
		}
	})

	t.Run("time running backwards", func(t *testing.T) {
		// A single endpoint that stamps a block older than the blocks
		// below it is caught by itself: block time never decreases.
		h := newHarness(t)
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		before := h.sink.lastHead()
		h.nodes[0].Set(lieAboutTime(h.chain, -3600))
		for i := 0; i < 5; i++ {
			h.chain.Mine(2)
			if err := h.tickErr(); !errors.Is(err, watcher.ErrInconsistentEndpoint) {
				t.Fatalf("err = %v", err)
			}
		}
		if got := h.sink.lastHead(); got != before {
			t.Fatalf("head moved to %d (%s)", got.Number, got.Time)
		}
	})

	t.Run("two endpoints agree: it is the clock", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		h.chain.SetTime(h.clock.Now().Add(time.Hour))
		h.chain.Mine(3)
		err := h.tickErr()
		if !errors.Is(err, watcher.ErrFutureBlock) || !strings.Contains(err.Error(), "check the server clock") {
			t.Fatalf("err = %v", err)
		}
		for _, s := range h.client.Status() {
			if !s.Healthy {
				t.Fatalf("endpoint %s marked unhealthy for the server's clock", s.URL)
			}
		}
	})
}

// TestCheckpointAboveTheChain: a checkpoint written while an
// endpoint lied about the chain's height — cursor, remembered hashes,
// finalized block and reported transfers all above the real chain — does not
// wedge the watcher. Once the endpoints agree that the chain is shorter, it
// goes back, credits nothing, and reverts what was reported from blocks that
// do not exist.
func TestCheckpointAboveTheChain(t *testing.T) {
	poison := func(t *testing.T, nodes int, in *incidents) (*harness, key) {
		h := newHarness(t, withNodes(nodes), withIncidents(in))
		h.watch.add(addr(1), addr(2))
		h.pay(addr(1), 10)
		h.mineTicking(testSettle + 2)

		// Every endpoint serves a chain that is 300 blocks longer and
		// contains a payment that never happened: the state an older
		// version could be brought into by one endpoint.
		long := h.chain.Clone()
		long.Transfer(usdt, payer, addr(2), units(1000))
		long.Mine(300)
		for _, n := range h.nodes {
			n.SetChain(long)
		}
		h.sink.set(func(s *sink) { s.headRule = nil })
		h.tick()
		h.tick()
		if c := h.w.Status().Cursor; c != long.Head().Number-testSettle {
			t.Fatalf("test setup: cursor = %d", c)
		}
		var phantom key
		for k, tr := range h.sink.liveKeys() {
			if tr.To == addr(2) {
				phantom = k
				// The consumer counts on it (it is not final there), so
				// the watcher keeps tracking it however old it gets.
				h.watch.setPending(tr)
			}
		}
		if phantom == (key{}) {
			t.Fatal("test setup: the phantom payment was not reported")
		}
		h.tick()
		for _, n := range h.nodes {
			n.SetChain(h.chain)
		}
		h.passCooldowns()
		h.client.Refresh(t.Context())
		h.sink.set(func(s *sink) { s.headRule = h.checkHeadRule })
		h.sink.take()
		h.restart()
		return h, phantom
	}

	check := func(t *testing.T, h *harness, phantom key, in *incidents) {
		t.Helper()
		real := h.chain.Head().Number
		st := h.w.Status()
		if st.Cursor > real-testSettle {
			t.Fatalf("cursor = %d, real tip = %d", st.Cursor, real)
		}
		if in.count(watcher.IncidentCheckpointAhead) != 1 {
			t.Fatalf("incidents = %+v", in.list)
		}
		for _, e := range h.sink.take() {
			if e.kind == "observed" {
				t.Fatalf("a transfer was credited during the repair: %+v", e.tr)
			}
		}
		// The chain grows past the height of the phantom payment's block:
		// the block there is another one, and the payment is reverted.
		h.pay(addr(1), 7)
		h.mineTicking(testSettle + 3)
		if _, still := h.sink.liveKeys()[phantom]; still {
			t.Fatal("the payment from a block that never existed was not reverted")
		}
		h.assertModel(startBlock)
		if got, want := h.sink.lastHead().Number, h.chain.Head().Number-testSettle; got != want {
			t.Fatalf("head = %d, want %d", got, want)
		}
	}

	t.Run("two endpoints", func(t *testing.T) {
		var in incidents
		h, phantom := poison(t, 2, &in)
		// The endpoints may simply be behind: for two ticks the watcher
		// waits, as it would for a lagging endpoint.
		for i := 0; i < 2; i++ {
			if err := h.tickErr(); !strings.Contains(err.Error(), "behind") {
				t.Fatalf("err = %v", err)
			}
			if len(h.sink.take()) != 0 {
				t.Fatal("facts reported while the checkpoint is ahead of the chain")
			}
		}
		h.tick() // the third time the checkpoint is taken to be wrong
		check(t, h, phantom, &in)
	})

	t.Run("single endpoint", func(t *testing.T) {
		var in incidents
		h, phantom := poison(t, 1, &in)
		for i := 0; i < watcher.DefaultConflictPatience-1; i++ {
			if err := h.tickErr(); !strings.Contains(err.Error(), "behind") {
				t.Fatalf("err = %v", err)
			}
		}
		h.tick()
		check(t, h, phantom, &in)
	})

	t.Run("an endpoint that is merely behind changes nothing", func(t *testing.T) {
		var in incidents
		h := newHarness(t, withNodes(2), withIncidents(&in))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		cursor := h.w.Status().Cursor
		h.nodes[0].Set(func(b *evmtest.Behavior) { b.Lag = 3 * testSettle })
		for i := 0; i < 6; i++ {
			if err := h.tickErr(); !strings.Contains(err.Error(), "behind") {
				t.Fatalf("err = %v", err)
			}
		}
		if got := h.w.Status().Cursor; got != cursor || in.count(watcher.IncidentCheckpointAhead) != 0 {
			t.Fatalf("cursor went from %d to %d; incidents %+v", cursor, got, in.list)
		}
	})
}

// TestLogOmission: the log scan of the blocks about to be settled
// is repeated on a second endpoint that serves logs.
func TestLogOmission(t *testing.T) {
	hideAll := func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } }

	t.Run("the primary hides a payment", func(t *testing.T) {
		var in incidents
		h := newHarness(t, withNodes(2), withIncidents(&in))
		h.watch.add(addr(1), addr(2))
		h.mineTicking(testSettle + 2)
		h.nodes[0].Set(hideAll)
		h.pay(addr(1), 10)
		h.pay(addr(2), 20)
		paid := h.chain.Mine(1).Number
		h.sink.take()
		// The payment is found when its block is about to be settled, not
		// before: a hidden transfer costs SettleDepth blocks of delay.
		for h.chain.Head().Number < paid+testSettle {
			if n := len(h.sink.liveKeys()); n != 0 {
				t.Fatalf("%d transfers observed with the block %d deep", n, h.chain.Head().Number-paid)
			}
			h.chain.Mine(1)
			_ = h.w.Tick(context.Background())
		}
		if n := len(h.sink.liveKeys()); n != 2 {
			t.Fatalf("%d transfers observed, want the 2 the primary hid", n)
		}
		if blamed := in.blamed(watcher.IncidentLogOmission); len(blamed) == 0 || blamed[0] != evm.RedactURL(h.urls[0]) {
			t.Fatalf("incidents = %+v", in.list)
		}
		if s := h.client.Status()[0]; s.Healthy || !strings.Contains(s.LastError, "lack a transfer") {
			t.Fatalf("the hiding endpoint's status = %+v", s)
		}
		// From here the honest endpoint is the primary and the liar the
		// one whose logs lack things.
		h.pay(addr(1), 30)
		h.mineTicking(testSettle + 2)
		h.assertModel(startBlock)
	})

	t.Run("the primary hides one of two payments in a block", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		h.watch.add(addr(1), addr(2))
		h.mineTicking(testSettle + 2)
		victim := addr(2)
		h.nodes[0].Set(func(b *evmtest.Behavior) {
			b.DropLogs = func(l evm.Log) bool { return len(l.Topics) == 3 && string(l.Topics[2][12:]) == string(victim[:]) }
		})
		h.pay(addr(1), 10)
		h.pay(addr(2), 20)
		h.chain.Mine(1)
		for i := 0; i < testSettle+2; i++ {
			h.chain.Mine(1)
			_ = h.w.Tick(context.Background())
		}
		h.assertModel(startBlock)
	})

	t.Run("the primary hides a payment during catch-up", func(t *testing.T) {
		// Far below the head the cursor follows the scan step by step
		// without fetching headers; the logs are confirmed all the same.
		h := newHarness(t, withNodes(2))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		h.nodes[0].Set(hideAll)
		h.pay(addr(1), 10)
		h.chain.Mine(3 * testWindow)
		for i := 0; i < 4; i++ {
			_ = h.w.Tick(context.Background())
		}
		h.assertModel(startBlock)
	})

	t.Run("the second log endpoint hides payments", func(t *testing.T) {
		// The primary's logs agree with the other endpoint's receipts, so
		// the payment is reported at once. The second endpoint's logs lack
		// it although its own receipt shows it: its scan is not accepted
		// as confirmation, and with no other log endpoint the head waits —
		// for as long as that lasts (settling on one endpoint's
		// logs after a while is exactly what would let a primary hide a
		// payment). The operator is told, again and again.
		var in incidents
		h := newHarness(t, withNodes(2), withIncidents(&in), withConfig(func(c *watcher.Config) { c.ConflictPatience = 4 }))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		h.nodes[1].Set(hideAll)
		h.pay(addr(1), 10)
		paid := h.chain.Mine(1).Number
		h.tick()
		if n := len(h.sink.liveKeys()); n != 1 {
			t.Fatalf("%d transfers observed, want 1", n)
		}
		h.chain.Mine(testSettle + 3)
		for i := 0; i < 12; i++ {
			h.tick()
			if got := h.sink.lastHead().Number; got >= paid {
				t.Fatalf("tick %d: head %d passed block %d although no second endpoint confirmed its logs", i, got, paid)
			}
			h.passCooldowns()
		}
		if blamed := in.blamed(watcher.IncidentLogOmission); len(blamed) == 0 || blamed[0] != evm.RedactURL(h.urls[1]) {
			t.Fatalf("incidents = %+v", in.list)
		}
		st := h.w.Status()
		if st.LogsUnchecked || !strings.Contains(st.Hold, "log verification unavailable") || in.count(watcher.IncidentHeadHeld) < 2 {
			t.Fatalf("status = %+v, head_held incidents = %d: the hold must be named and alerted repeatedly", st, in.count(watcher.IncidentHeadHeld))
		}
		// The endpoint's logs are complete again: the head moves on.
		h.nodes[1].Set(honest)
		h.passCooldowns()
		h.tick()
		if st := h.w.Status(); st.Cursor < paid || st.LogsUnchecked || st.Hold != "" {
			t.Fatalf("status = %+v", st)
		}
		h.assertModel(startBlock)
	})

	t.Run("the second log endpoint invents a payment", func(t *testing.T) {
		for _, nodes := range []int{2, 3} {
			var in incidents
			h := newHarness(t, withNodes(nodes), withIncidents(&in))
			h.watch.add(addr(1))
			h.mineTicking(testSettle + 2)
			at := h.chain.Mine(1)
			fake := fabricated(h, at.Number, 1, 1_000_000)
			h.nodes[1].Set(func(b *evmtest.Behavior) { b.ExtraLogs = []evm.Log{fake} })
			for i := 0; i < 2*testSettle; i++ {
				h.chain.Mine(1)
				_ = h.w.Tick(context.Background())
				if nodes == 2 {
					h.passCooldowns() // however long it goes on
				}
			}
			if n := len(h.sink.liveKeys()); n != 0 {
				t.Fatalf("%d endpoints: a payment only one endpoint knows was reported", nodes)
			}
			head := h.sink.lastHead().Number
			switch nodes {
			case 2:
				// Two endpoints: one says there is a transfer, one says
				// there is not. The head waits below it.
				if head >= at.Number || h.w.Status().Unverified != 1 {
					t.Fatalf("2 endpoints: head = %d, disputed block = %d, status %+v", head, at.Number, h.w.Status())
				}
			case 3:
				// Three: the other two outvote it, and the head moves.
				if head != h.chain.Head().Number-testSettle || in.count(watcher.IncidentUnknownTransfer) == 0 {
					t.Fatalf("3 endpoints: head = %d, tip = %d, incidents %+v", head, h.chain.Head().Number, in.list)
				}
			}
		}
	})

	t.Run("only one endpoint serves logs", func(t *testing.T) {
		// Nothing to compare with: the scan stands alone, and the status
		// says so. Headers and receipts are still cross-checked. (That an
		// endpoint has no log service is concluded from three refusals over
		// three minutes, not from one; until then the head waits.)
		h := newHarness(t, withNodes(2))
		h.watch.add(addr(1))
		h.noLogService(1, &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"})
		h.pay(addr(1), 10)
		for i := 0; i < testSettle+4; i++ {
			h.chain.Mine(1)
			_ = h.w.Tick(context.Background())
		}
		h.assertModel(startBlock)
		st := h.w.Status()
		if !st.LogsUnchecked || st.LogVerifier != "" || st.Verifier != evm.RedactURL(h.urls[1]) || st.Cursor != h.chain.Head().Number-testSettle {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("the cost: one log query per tick on the second endpoint", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		for _, n := range h.nodes {
			n.ResetCounters()
		}
		const ticks = 20
		h.mineTicking(ticks)
		for i, n := range h.nodes {
			t.Logf("endpoint %d: %d requests in %d ticks (%d eth_getLogs, %d eth_getBlockByNumber)", i, n.Requests(), ticks,
				n.Calls("eth_getLogs"), n.Calls("eth_getBlockByNumber"))
		}
		if got := h.nodes[1].Calls("eth_getLogs"); got != ticks {
			t.Fatalf("second endpoint: %d eth_getLogs calls in %d ticks", got, ticks)
		}
		if got := h.nodes[0].Requests() + h.nodes[1].Requests(); got > 5*ticks {
			t.Fatalf("%d HTTP requests in %d ticks, want at most 5 per tick", got, ticks)
		}
		if st := h.w.Status(); st.LogVerifier != evm.RedactURL(h.urls[1]) || st.LogsUnchecked {
			t.Fatalf("status = %+v", st)
		}
	})
}

// TestClockSkewStatus: the status carries the difference between
// the local clock and the chain tip's timestamp.
func TestClockSkewStatus(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.chain.SetTime(h.clock.Now().Add(-2 * time.Second))
	h.chain.Mine(1)
	h.tick()
	if skew := h.w.Status().ClockSkew; skew != 2*time.Second {
		t.Fatalf("skew = %s, want 2s", skew)
	}
	h.clock.Advance(5 * time.Minute)
	h.tick()
	if skew := h.w.Status().ClockSkew; skew != 5*time.Minute+2*time.Second {
		t.Fatalf("skew = %s", skew)
	}
	// One endpoint alone cannot fake a skew: the smaller difference of the
	// two tips counts.
	h.chain.SetTime(h.clock.Now())
	h.chain.Mine(1)
	h.nodes[0].Set(func(b *evmtest.Behavior) { b.Lag = 1 }) // its tip is the five-minute-old block
	h.tick()
	if skew := h.w.Status().ClockSkew; skew != 0 {
		t.Fatalf("skew = %s with one fresh endpoint, want 0", skew)
	}
}

// TestTransferBlockTime: every transfer is reported with the timestamp of
// its block, and that timestamp is a fact like the transfer itself — the
// consumer dates the payment by it. (That it is the block's real timestamp
// is checked by the harness on every transfer of every test.)
func TestTransferBlockTime(t *testing.T) {
	t.Run("the same after a restart, without asking again", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		h.pay(addr(1), 10)
		paid := h.chain.Mine(1)
		h.tick()
		var first watcher.Transfer
		for _, tr := range h.sink.liveKeys() {
			first = tr
		}
		if first.BlockTime.Unix() != int64(paid.Time) {
			t.Fatalf("block time = %s, the block's is %d", first.BlockTime, paid.Time)
		}
		// A new process reports the transfer again (its block is not
		// settled yet): with the time from the checkpoint. The sink fails
		// the test if a transfer comes back with another time. It costs a
		// batch of receipts (the transfer is confirmed again) and no
		// header for dating it: at most 6 HTTP requests, where a quiet
		// tick makes 5.
		h.restart()
		for _, n := range h.nodes {
			n.ResetCounters()
		}
		h.sink.take()
		h.tick()
		if got := kinds(h.sink.take()); len(got) == 0 || got[0] != "observed" {
			t.Fatalf("events after the restart = %v", got)
		}
		if got := h.nodes[0].Requests() + h.nodes[1].Requests(); got > 6 {
			t.Fatalf("%d HTTP requests in the tick after the restart, want at most 6", got)
		}
		// And with a client that knows nothing about its endpoints.
		h.restartWithNewClient()
		h.tick()
	})

	t.Run("the cost: one batch of headers per endpoint when transfers arrive", func(t *testing.T) {
		h := newHarness(t, withNodes(2))
		for i := 1; i <= 5; i++ {
			h.watch.add(addr(i))
		}
		h.mineTicking(testSettle + 2)
		// Five payments in three blocks.
		h.pay(addr(1), 10)
		h.pay(addr(2), 10)
		h.chain.Mine(1)
		h.pay(addr(3), 10)
		h.chain.Mine(1)
		h.pay(addr(4), 10)
		h.pay(addr(5), 10)
		h.chain.Mine(1)
		for _, n := range h.nodes {
			n.ResetCounters()
		}
		h.tick()
		quiet := 5 // HTTP requests of a tick without transfers
		if got := h.nodes[0].Requests() + h.nodes[1].Requests(); got != quiet+3 {
			t.Fatalf("%d HTTP requests in a tick that reports 5 transfers in 3 blocks, want %d: the usual %d, one batch of receipts, one batch of headers per endpoint",
				got, quiet+3, quiet)
		}
		if n := len(h.sink.liveKeys()); n != 5 {
			t.Fatalf("%d transfers observed", n)
		}
	})

	t.Run("three endpoints: a lie about the block's time is outvoted", func(t *testing.T) {
		for liar := 0; liar < 2; liar++ {
			var in incidents
			h := newHarness(t, withNodes(3), withIncidents(&in))
			h.watch.add(addr(1))
			h.mineTicking(testSettle + 2)
			h.nodes[liar].Set(lieAboutTime(h.chain, -3600))
			h.pay(addr(1), 10)
			for i := 0; i < 4; i++ {
				h.chain.Mine(1)
				_ = h.w.Tick(context.Background())
			}
			if n := len(h.sink.liveKeys()); n != 1 {
				t.Fatalf("liar %d: %d transfers observed, want 1 (with the block's real time)", liar, n)
			}
			if h.client.Status()[liar].Healthy {
				t.Fatalf("liar %d was not set aside", liar)
			}
		}
	})

	t.Run("single endpoint: its word is taken", func(t *testing.T) {
		// The documented limit of one endpoint: within what is plausible
		// (not in the future, not older than the blocks below) the
		// timestamp it gives for a block is the one that is reported.
		h := newHarness(t)
		h.watch.add(addr(1))
		h.mineTicking(testSettle + 2)
		h.sink.set(func(s *sink) { s.timeRule, s.headRule = nil, nil })
		h.nodes[0].Set(func(b *evmtest.Behavior) {
			b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
				if method != "eth_getBlockByNumber" {
					return nil, nil, false
				}
				tag, n, numeric := blockParam(params)
				blk, ok := h.chain.BlockByNumber(n)
				if !ok || (!numeric && tag != "latest") {
					return nil, nil, false
				}
				if !numeric {
					blk = h.chain.Head()
				}
				return headerJSON(blk, blk.Time+90), nil, true // every block 90 s younger than it is
			}
		})
		h.clock.Advance(0)
		h.pay(addr(1), 10)
		paid := h.chain.Mine(1)
		h.tick()
		for _, tr := range h.sink.liveKeys() {
			if tr.BlockTime.Unix() != int64(paid.Time)+90 {
				t.Fatalf("block time = %s; expected the single endpoint's (forged) answer %d to be taken", tr.BlockTime, paid.Time+90)
			}
		}
		if len(h.sink.liveKeys()) != 1 {
			t.Fatal("transfer not reported")
		}
	})
}
