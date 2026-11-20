package watcher_test

import (
	"context"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// An address appears that can have been paid below the cursor: the watcher
// goes back for it, and writes that down at once — but the disk is full.
// The second endpoint is down, so the ticks that follow settle nothing and
// change nothing else. The checkpoint is written all the same, in the next
// tick: a process that restarts then goes on from the rewind, and the blocks
// are scanned again for the address. (The rewind used to stay in memory
// until something else changed; restarted before that, the process resumed
// from the checkpoint before it, took the address for one the checkpoint had
// covered, and never scanned those blocks for it.)
func TestARewindThatCouldNotBeWrittenIsWrittenInTheNextTick(t *testing.T) {
	h := newHarness(t, withNodes(2))
	h.watch.add(addr(1))
	h.mineTicking(testSettle + 6)
	cursor := h.w.Status().Cursor
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
	h.store.mu.Lock()
	h.store.failSave = 1
	h.store.mu.Unlock()
	h.watch.addSince(addr(2), cursor-4)
	if err := h.w.Tick(context.Background()); err == nil {
		t.Fatal("test setup: the rewind was written")
	}
	_ = h.w.Tick(context.Background()) // nothing else changes
	info, err := watcher.InspectCheckpoint(h.store.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if info.Cursor != cursor-4 {
		t.Fatalf("checkpoint cursor %d after the tick that followed the failed write, want the rewind to %d", info.Cursor, cursor-4)
	}
	h.restart()
	h.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
	h.passCooldowns()
	for _, n := range h.nodes {
		n.ResetCounters()
	}
	h.tick()
	if st := h.w.Status(); st.Cursor < cursor {
		t.Fatalf("after the restart the rewound blocks were not settled again: cursor %d", st.Cursor)
	}
	covered := false
	for _, q := range h.nodes[0].LogQueries() {
		if q.From <= cursor-3 && cursor-3 <= q.To && !q.Refused {
			covered = true
		}
	}
	if !covered {
		t.Fatalf("block %d was never scanned again for the new address", cursor-3)
	}
}
