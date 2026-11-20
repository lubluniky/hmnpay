package watcher_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// FuzzCheckpointTick runs a watcher on a checkpoint an attacker (or a broken
// disk) wrote. Whatever is in it:
//
//   - nothing panics;
//   - a checkpoint that is not usable is refused as a whole — every tick
//     fails with ErrCheckpoint, nothing is reported and the stored bytes are
//     left alone (never half-applied, never silently overwritten);
//   - a usable one cannot make the watcher report a head that is not a block
//     of the chain with its real timestamp, or a transfer that is not on the
//     chain. (It can make the watcher believe blocks were already scanned;
//     that is what a checkpoint is.)
//
// Seeds: real checkpoints of this harness, and mutations of them.
//
//	go test ./internal/chain/watcher -run '^$' -fuzz FuzzCheckpointTick -fuzztime 3m
func FuzzCheckpointTick(f *testing.F) {
	// A real checkpoint: a watcher that has seen two payments and a reorg.
	seed := func(nodes int) []byte {
		h := newHarness(f, withNodes(nodes))
		h.watch.add(addr(1), addr(2))
		h.pay(addr(1), 10)
		h.mineTicking(testSettle + 2)
		h.pay(addr(2), 20)
		h.mineTicking(2)
		return h.store.snapshot()
	}
	real := string(seed(1))
	f.Add([]byte(real), uint8(1))
	f.Add([]byte(real), uint8(2))
	for _, mutate := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"cursor":`, `"cursor":9`, 1) },       // cursor far above the chain
		func(s string) string { return strings.Replace(s, `"cursor":1`, `"cursor":`, 1) },       // cursor far below
		func(s string) string { return strings.Replace(s, `"h":"0x`, `"h":"0xff`, 1)[:len(s)] }, // truncated
		func(s string) string { return strings.Replace(s, `"h":"0x`, `"h":"0x00`, 1) },
		func(s string) string { return strings.ReplaceAll(s, `"n":100`, `"n":101`) },         // anchors at other heights
		func(s string) string { return strings.ReplaceAll(s, `"block":100`, `"block":900`) }, // transfers in blocks that do not exist
		func(s string) string { return strings.Replace(s, `"amount":"`, `"amount":"99999`, 1) },
		func(s string) string { return strings.Replace(s, `"chain_id":56`, `"chain_id":97`, 1) },
		func(s string) string { return strings.Replace(s, `"v":1`, `"v":7`, 1) },
		func(s string) string { return strings.Replace(s, `"finalized":{"n":1`, `"finalized":{"n":9`, 1) },
		func(s string) string {
			return `{"v":1,"chain_id":56,"token":"0x55d398326f99059fF775485246999027B3197955","cursor":18446744073709551615}`
		},
		func(s string) string {
			return `{"v":1,"chain_id":56,"token":"0x55d398326f99059fF775485246999027B3197955","cursor":0}`
		},
		func(s string) string { return "{" },
		// Found by this target: a transfer whose recorded block time is not
		// its block's, in a checkpoint whose position lies below it — the
		// transfer is found again and was reported with the recorded time.
		func(s string) string {
			return strings.Replace(strings.ReplaceAll(s, `"time":17`, `"time":1`), `"cursor":1`, `"cursor":`, 1)
		},
		func(s string) string {
			return `{"v":1,"chain_id":56,"token":"0X55d398326f99059fF775485246999027B3197955","cursor":0,"transfers":[{"from":"0Xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266","to":"0XA97FA33978cF7670d4F2C8656A075981C138b7b3","amount":"20000000000000000000","tx":"0Xdd0Bf07A3e3e31e7B0f18355f3f2dfBB9efBC7512e2e85eB73dA1215ABC1f34e","block":10009,"block_hash":"0X33087d059eA1B8C9dCBdf0fdB5656A09294AA46d91C67421fdfddAf84f62516A","time":1}]}`
		},
	} {
		f.Add([]byte(mutate(real)), uint8(1))
		f.Add([]byte(mutate(real)), uint8(2))
	}

	f.Fuzz(func(t *testing.T, data []byte, nodes uint8) {
		h := newHarness(t, withNodes(1+int(nodes%2)))
		h.watch.add(addr(1), addr(2))
		h.pay(addr(1), 10)
		h.chain.Mine(testSettle + 2)
		h.pay(addr(2), 20)
		h.chain.Mine(3)

		// The harness's rule also demands that every transfer below the
		// head was delivered. A forged checkpoint can claim blocks as
		// scanned; what it must not be able to do is make the watcher say
		// something false about the chain.
		h.sink.set(func(s *sink) {
			s.lenientReverts = true
			s.headRule = func(head watcher.Head, _ map[key]watcher.Transfer) {
				blk, ok := h.chain.BlockByNumber(head.Number)
				if !ok || blk.Hash != head.Hash || head.Time.Unix() != int64(blk.Time) {
					t.Errorf("head %d (%s, %s) is not a block of the chain", head.Number, head.Hash, head.Time)
				}
			}
		})
		h.store.restore(data)
		h.restart()

		_, inspectErr := watcher.InspectCheckpoint(data)
		refused := false
		for i := 0; i < 5; i++ {
			err := h.w.Tick(context.Background())
			if errors.Is(err, watcher.ErrCheckpoint) {
				refused = true
			}
			if refused && !errors.Is(err, watcher.ErrCheckpoint) {
				t.Fatalf("tick %d: a refused checkpoint was used after all (err = %v)", i, err)
			}
			h.chain.Mine(2)
			h.passCooldowns()
		}
		if refused {
			if events := h.sink.take(); len(events) != 0 {
				t.Fatalf("facts reported from a refused checkpoint: %v", kinds(events))
			}
			if !bytes.Equal(h.store.snapshot(), data) {
				t.Fatal("a refused checkpoint was overwritten")
			}
			return
		}
		if len(data) != 0 && inspectErr != nil {
			t.Fatalf("the watcher ran on a checkpoint InspectCheckpoint rejects: %v", inspectErr)
		}
		truth := h.truth(0, h.watch.has)
		for k, tr := range h.sink.liveKeys() {
			if _, ok := truth[k]; !ok {
				t.Fatalf("a transfer that is not on the chain was reported: %+v", tr)
			}
		}
	})
}

// TestCheckpointBlockTimeIsConfirmed is what FuzzCheckpointTick found, as a
// scenario. A transfer the checkpoint remembers is reported again after a
// restart when its block is scanned again — with a fresh receipt, never on
// the strength of having been confirmed once. The same has to go for the
// timestamp it is reported with: the consumer dates the transfer by it
// against the payment's deadline, and the checkpoint's copy was used as it
// stood. It is now confirmed by the endpoints first; if they report another
// time for that block, theirs is used and recorded.
func TestCheckpointBlockTimeIsConfirmed(t *testing.T) {
	for _, nodes := range []int{1, 2, 3} {
		h := newHarness(t, withNodes(nodes))
		h.watch.add(addr(1))
		h.pay(addr(1), 10)
		paid := h.chain.Mine(1)
		h.mineTicking(testSettle + 2)
		if len(h.sink.liveKeys()) != 1 {
			t.Fatalf("%d endpoints, test setup: %d transfers", nodes, len(h.sink.liveKeys()))
		}

		// The checkpoint, with the transfer dated to 1970 and the position
		// put back below its block.
		var cp map[string]any
		if err := json.Unmarshal(h.store.snapshot(), &cp); err != nil {
			t.Fatal(err)
		}
		transfers, _ := cp["transfers"].([]any)
		if len(transfers) != 1 {
			t.Fatalf("test setup: checkpoint %s", h.store.snapshot())
		}
		transfers[0].(map[string]any)["time"] = 1
		cp["cursor"] = paid.Number - 2
		delete(cp, "anchors")
		delete(cp, "finalized")
		doctored, err := json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		h.store.restore(doctored)
		h.restart()
		h.chain.Mine(1)
		h.tick() // the harness checks the time of every transfer it is given
		h.sink.mu.Lock()
		dups := h.sink.dups
		h.sink.mu.Unlock()
		if dups == 0 {
			t.Fatalf("%d endpoints, test setup: the transfer was not reported again", nodes)
		}
		h.assertModel(startBlock)
		// And the record is put right.
		if err := json.Unmarshal(h.store.snapshot(), &cp); err != nil {
			t.Fatal(err)
		}
		transfers, _ = cp["transfers"].([]any)
		if len(transfers) != 1 || uint64(transfers[0].(map[string]any)["time"].(float64)) != paid.Time {
			t.Fatalf("%d endpoints: the checkpoint still dates the transfer wrongly: %s", nodes, h.store.snapshot())
		}
	}
}
