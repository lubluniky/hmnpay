package corebridge

// Regression tests for a malicious RPC endpoint: one that lies about block
// times, hides logs, claims a longer or shorter chain, or is throttled, and
// a server clock that is off. Each test describes the attack and asserts the
// safe behaviour.
//
// Setting for all of them: TWO endpoints, the default verify mode (auto),
// i.e. the configuration the README describes as "a single lying or
// compromised endpoint can neither create a payment that did not happen nor
// revert one that did — at worst it delays detection". Endpoint 0 (the one
// the watcher prefers) lies; endpoint 1 is honest and healthy throughout.

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

func hexQuantity(v uint64) string { return "0x" + strconv.FormatUint(v, 16) }

// blockNumberParam parses the block parameter of eth_getBlockByNumber when
// it is a number (not a tag).
func blockNumberParam(params []json.RawMessage) (uint64, bool) {
	if len(params) == 0 {
		return 0, false
	}
	var tag string
	if json.Unmarshal(params[0], &tag) != nil || !strings.HasPrefix(tag, "0x") {
		return 0, false
	}
	n, err := strconv.ParseUint(tag[2:], 16, 64)
	return n, err == nil
}

// The core expires payments, terminally, against the processed head's
// timestamp. Were the head — number, hash and TIMESTAMP — taken from one
// endpoint and never compared with another, one endpoint that answers every
// header request truthfully except for the "timestamp" field would expire
// every open invoice at once; nothing would contradict any remembered block
// hash, so no check would fire. The head is cross-checked, so it does not.
func TestOneEndpointLyingAboutBlockTimeExpiresNothing(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.start()
	w.synced()

	unpaid := w.env.CreatePayment("10")
	willPay := w.env.CreatePayment("25") // its payer has 30 minutes
	if primary := w.watcherStatus().Endpoint; !strings.Contains(w.urls[0], strings.TrimPrefix(primary, "http://")) {
		t.Fatalf("test assumption: endpoint 0 is the primary (primary is %s)", primary)
	}

	// Endpoint 0 starts lying: real numbers, real hashes, timestamps 40
	// days ahead (longer than the maximum payment TTL of 30 days).
	w.nodes[0].Set(func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			n, ok := blockNumberParam(params)
			if !ok {
				return nil, nil, false
			}
			blk, ok := w.chain.BlockByNumber(n)
			if !ok {
				return nil, nil, false
			}
			return map[string]any{
				"number": hexQuantity(blk.Number), "hash": blk.Hash.String(), "parentHash": blk.ParentHash.String(),
				"timestamp": hexQuantity(blk.Time + 40*24*3600), "transactions": []string{},
			}, nil, true
		}
	})
	w.mine(testSettle + 2) // ordinary blocks, a few seconds of chain time

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && w.status(unpaid) == core.StatusAwaiting {
		time.Sleep(2 * time.Millisecond)
	}
	// The payer of the second invoice pays well inside their window.
	w.pay(willPay, 25)
	w.mine(testSettle + testConfirmations + 2)
	time.Sleep(300 * time.Millisecond)

	ns, _ := w.env.Service.NodeStatus(w.env.Operator())
	a, b := w.env.Payment(unpaid.ID), w.env.Payment(willPay.ID)
	t.Logf("real chain/wall clock: %s; invoices expire at %s", w.env.Clock.Now().Format(time.RFC3339), unpaid.ExpiresAt.Format(time.RFC3339))
	t.Logf("core head time after the lie: %s", w.coreHead().Time.Format(time.RFC3339))
	t.Logf("unpaid invoice: %s (events: %s)", a.Status, w.events(unpaid))
	t.Logf("invoice paid in time with 25 USDT: status=%s attention=%q confirmed=%s (events: %s)", b.Status, b.Attention, b.AmountConfirmed, w.events(willPay))
	if len(ns.Endpoints) == 2 {
		t.Logf("lying endpoint reported healthy=%v in_use=%v; honest endpoint healthy=%v", ns.Endpoints[0].Healthy, ns.Endpoints[0].InUse, ns.Endpoints[1].Healthy)
	}
	if a.Status == core.StatusExpired || b.Status == core.StatusExpired {
		t.Fatalf("one of two endpoints lied about a block timestamp and open invoices were expired %s before their deadline; an in-time 25 USDT payment ended as %q/%q",
			unpaid.ExpiresAt.Sub(w.env.Clock.Now()).Round(time.Second), b.Status, b.Attention)
	}

	// More than that: not one head with a forged timestamp
	// reached the core: its head time never left the real clock.
	if ahead := w.coreHead().Time.Sub(w.env.Clock.Now()); ahead > time.Minute {
		t.Fatalf("core head time is %s ahead of the clock", ahead)
	}
	// The lie was noticed and attributed: the forged header is impossible
	// on its own (40 days in the future), so the watcher knows which of the
	// two endpoints is wrong, says so, and the dashboard shows why the head
	// is waiting.
	w.eventually("an endpoint_fault alert and a status warning", func() bool {
		st := w.bridge.Status()
		return st.Alerts[AlertEndpointFault] > 0 && len(st.Warnings) > 0
	})
	if !strings.Contains(w.logs.String(), "ahead of this server's clock") {
		t.Error("the log does not say what was wrong with the endpoint's blocks")
	}
	// With one liar among two endpoints nothing can be settled — the price
	// of not taking one endpoint's word. The payment that was made is not
	// lost: once the operator removes the lying endpoint it is found paid,
	// in time, and the unpaid invoice is still open.
	if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{RPCURLs: []string{w.urls[1]}}); err != nil {
		t.Fatal(err)
	}
	got := w.waitStatus(willPay, core.StatusPaid)
	if got.NeedsAttention || w.status(unpaid) != core.StatusAwaiting {
		t.Fatalf("after removing the liar: paid invoice %+v, unpaid invoice %s", got, w.status(unpaid))
	}
}

// Were log scans made on one endpoint only, with the second endpoint asked
// for receipts of what the first one reported but never for what it may have
// left out, an endpoint that omits a transfer from eth_getLogs from the
// start would hide the payment although the other configured endpoint serves
// logs and would have shown it — and the invoice would then expire on the
// (correctly reported) chain clock. The second endpoint scans the logs too.
func TestOneEndpointHidingLogsDoesNotLoseThePayment(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")

	w.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } })
	before := w.nodes[1].Calls("eth_getLogs")
	w.pay(p, 25)
	w.mine(testSettle + testConfirmations + 2)
	time.Sleep(300 * time.Millisecond)
	if got := w.status(p); got != core.StatusAwaiting {
		t.Logf("payment detected (%s) although endpoint 0 hid it", got)
		// And further: the payment completes, the
		// receipt that confirmed the honest endpoint's log came from the
		// other endpoint, and the omission is reported.
		done := w.waitStatus(p, core.StatusPaid)
		if done.NeedsAttention || done.AmountConfirmed != "25.00" {
			t.Fatalf("payment = %+v", done)
		}
		if w.nodes[1].Calls("eth_getLogs") == before {
			t.Fatal("the honest endpoint was never asked for logs")
		}
		if n := w.bridge.Status().Alerts[AlertEndpointFault]; n == 0 || !strings.Contains(w.logs.String(), "log_omission") {
			t.Fatalf("the omission was not reported: alerts %+v", w.bridge.Status().Alerts)
		}
		return
	}
	t.Logf("25 USDT on-chain for 9+ blocks; status still %q; eth_getLogs calls to the honest endpoint meanwhile: %d",
		w.status(p), w.nodes[1].Calls("eth_getLogs")-before)

	w.advance(p.ExpiresAt.Sub(w.env.Clock.Now()) + core.DefaultExpiryGrace + time.Second)
	w.mine(testSettle + 2)
	w.waitStatus(p, core.StatusExpired)
	got := w.env.Payment(p.ID)
	t.Fatalf("the payer sent 25 USDT in time; one of two log-serving endpoints hid the log and the invoice is %q with %s received",
		got.Status, got.AmountReceived)
}

// The head NUMBER must be verified as well. An endpoint that claims the
// chain is a few hundred blocks longer than it is would otherwise make the
// core count confirmations that do not exist (a transfer one block deep
// settled as final), and leave the persisted cursor above the real chain:
// from then on every honest endpoint would be "behind the settled cursor"
// and the watcher would make no progress — payments made afterwards would
// not be detected.
func TestOneEndpointLyingAboutHeadNumberSettlesNothingEarly(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.setConfirmations(12)
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	paidIn := w.mine(1).Number // exactly one confirmation exists

	const jump = 300
	fake := func(n uint64) map[string]any {
		h := make([]byte, 32)
		h[0], h[24], h[25], h[26], h[27], h[28], h[29], h[30], h[31] = 0xfa, byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32), byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
		ph := make([]byte, 32)
		copy(ph, h)
		ph[1] = 0x01
		return map[string]any{
			"number": hexQuantity(n), "hash": "0x" + hexString(h), "parentHash": "0x" + hexString(ph),
			"timestamp": hexQuantity(uint64(w.env.Clock.Now().Unix())), "transactions": []string{},
		}
	}
	w.nodes[0].Set(func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			real := w.chain.Head().Number
			switch method {
			case "eth_blockNumber":
				return hexQuantity(real + jump), nil, true
			case "eth_getBlockByNumber":
				var tag string
				if len(params) > 0 {
					_ = json.Unmarshal(params[0], &tag)
				}
				if tag == "latest" {
					return fake(real + jump), nil, true
				}
				if n, ok := blockNumberParam(params); ok && n > real {
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
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && w.status(p) != core.StatusPaid {
		time.Sleep(2 * time.Millisecond)
	}
	real := w.chain.Head().Number
	got := w.env.Payment(p.ID)
	t.Logf("required confirmations: 12; real confirmations of the transfer: %d; core head: %d (real tip %d); status: %s",
		real-paidIn+1, w.coreHead().Number, real, got.Status)
	premature := got.Status == core.StatusPaid
	if premature {
		t.Fatalf("premature settlement: paid with %d of 12 confirmations", real-paidIn+1)
	}
	// Nothing above the real chain was ever
	// persisted or passed on.
	if st := w.watcherStatus(); st.Cursor > real || w.coreHead().Number > real {
		t.Fatalf("watcher cursor %d / core head %d above the real tip %d", st.Cursor, w.coreHead().Number, real)
	}
	if info, err := watcher.InspectCheckpoint(w.store.get()); err != nil || info.Cursor > real {
		t.Fatalf("checkpoint cursor %d above the real tip %d (err %v)", info.Cursor, real, err)
	}
	// And the lie does not stop the payment either: while endpoint 0 keeps
	// claiming a longer chain, the watcher settles what both endpoints
	// have, and the payment is paid when its confirmations really exist.
	w.mine(testSettle + 12)
	w.waitStatus(p, core.StatusPaid)
	if h := w.coreHead().Number; h < paidIn+11 || h > w.chain.Head().Number {
		t.Fatalf("paid at core head %d; transfer in block %d, real tip %d", h, paidIn, w.chain.Head().Number)
	}

	// The liar goes quiet: only the honest endpoint answers now.
	//
	// One might expect a payment made now to be detected and settled by the
	// one endpoint that is left. Under the default mode, with two endpoints
	// configured, that would be taking one endpoint's word — for the
	// transfer and for the head — which is exactly what this test forbids:
	// an attacker who controls one endpoint and can make the other one
	// unreachable would be back in business. The watcher fails closed
	// instead: it is not wedged (its cursor is on the real chain, it sees
	// the transfer, it says what it is waiting for), and the payment
	// completes as soon as there is a second opinion again — or as soon as
	// the operator removes the dead endpoint, or runs with
	// VerifyBestEffort, which trades this guarantee for availability.
	w.nodes[0].Set(func(b *evmtest.Behavior) { b.Intercept = nil; b.Down = true })
	w.passCooldowns()
	p2 := w.env.CreatePayment("40")
	w.pay(p2, 40)
	w.mine(testSettle + 12 + 4)
	w.eventually("the watcher to see the transfer and wait for a second endpoint", func() bool {
		st := w.watcherStatus()
		return st.Unverified == 1 && strings.Contains(st.Hold, "no second endpoint")
	})
	st := w.watcherStatus()
	t.Logf("honest endpoint only: a new 40 USDT payment is %q; watcher cursor=%d real tip=%d hold=%q",
		w.status(p2), st.Cursor, w.chain.Head().Number, st.Hold)
	if st.Cursor > w.chain.Head().Number {
		t.Fatalf("watcher wedged above the real chain: cursor %d, real tip %d", st.Cursor, w.chain.Head().Number)
	}
	w.never("a payment is settled on the word of a single endpoint", func() bool { return w.status(p2) != core.StatusAwaiting })
	// The operator removes the dead endpoint: the honest one is now the
	// only one, and the payment — 16+ blocks deep — is paid at once.
	if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{RPCURLs: []string{w.urls[1]}}); err != nil {
		t.Fatal(err)
	}
	w.waitStatus(p2, core.StatusPaid)
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// The case of a lying block time the core cannot close by itself. The core settles a
// deadline only when block time AND its own clock are past it, so a block
// from the future no longer expires anything. But a watcher that is behind
// processes old blocks while the wall clock is already past a deadline — and
// then an endpoint that stamps an OLD block with the current time (and
// withholds the newer blocks) satisfies both clocks, and the core would
// expire, terminally, a payment whose in-time transfer sits in a block the
// watcher has not processed yet. Only the rule "no head from a single
// endpoint" prevents it: the forged timestamp is not the one the second
// endpoint reports for that block.
func TestLaggingWatcherIsNotFooledByAnOldBlockStampedNow(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.start()
	w.synced()
	w.stop() // the watcher falls behind

	p := w.env.CreatePayment("25")
	other := w.env.CreatePayment("10") // nobody pays this one
	w.mine(3)
	// The payer pays one minute before the deadline.
	w.advance(p.ExpiresAt.Sub(w.env.Clock.Now()) - time.Minute)
	w.pay(p, 25)
	paidIn := w.mine(1).Number
	// Time passes; the deadline is long gone by the wall clock.
	w.advance(time.Hour)
	w.mine(testSettle + testConfirmations + 4)

	// Endpoint 0: its chain "ends" right below the payment's block, and
	// every block it serves is stamped with the current time.
	limit := paidIn - 1
	forged := func(n uint64) (any, *evmtest.RPCError, bool) {
		blk, ok := w.chain.BlockByNumber(n)
		if !ok || n > limit {
			return nil, nil, true // null: no such block
		}
		return map[string]any{
			"number": hexQuantity(blk.Number), "hash": blk.Hash.String(), "parentHash": blk.ParentHash.String(),
			"timestamp": hexQuantity(uint64(w.env.Clock.Now().Unix())), "transactions": []string{},
		}, nil, true
	}
	w.nodes[0].Set(func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			switch method {
			case "eth_blockNumber":
				return hexQuantity(limit), nil, true
			case "eth_getBlockByNumber":
				var tag string
				if len(params) > 0 {
					_ = json.Unmarshal(params[0], &tag)
				}
				switch tag {
				case "latest":
					return forged(limit)
				case "finalized", "safe":
					return forged(limit - 2)
				}
				if n, ok := blockNumberParam(params); ok {
					return forged(n)
				}
			}
			return nil, nil, false
		}
	})

	w.start()
	w.eventually("the watcher to notice that the endpoints disagree", func() bool {
		return w.bridge.Status().Alerts[AlertEndpointFault] > 0 && w.watcherStatus().Hold != ""
	})
	w.never("an open payment is expired by a head only one endpoint vouches for", func() bool {
		return w.status(p) == core.StatusExpired || w.status(other) == core.StatusExpired ||
			w.coreHead().Time.After(p.ExpiresAt)
	})

	// The endpoint stops lying. The transfer is processed before any head
	// past the deadline, and the payment is paid — while the invoice that
	// really was not paid expires, as it should.
	w.nodes[0].Set(func(b *evmtest.Behavior) { b.Intercept = nil })
	w.passCooldowns()
	got := w.waitStatus(p, core.StatusPaid)
	if got.NeedsAttention || got.AmountConfirmed != "25.00" {
		t.Fatalf("payment = %+v", got)
	}
	w.waitStatus(other, core.StatusExpired)
}

// A checkpoint (and a core head) that is already above the real
// chain — as an older version could leave it after an endpoint lied about
// the chain's height — does not wedge the watcher. Once two endpoints agree
// that the chain ends below the checkpoint, the watcher goes back to it,
// says so, and carries on; the core's head moves back with it.
func TestCheckpointAboveTheChainHeals(t *testing.T) {
	w := newWorld(t, withNodes(2))
	long := w.chain.Clone()
	long.Mine(300) // blocks that do not exist
	for _, n := range w.nodes {
		n.SetChain(long)
	}
	w.start()
	w.eventually("the poisoned state: core head 300 blocks above the real chain", func() bool {
		return w.coreHead().Number == long.Head().Number-testSettle
	})
	w.stop()
	if info, err := watcher.InspectCheckpoint(w.store.get()); err != nil || info.Cursor <= w.chain.Head().Number {
		t.Fatalf("test setup: checkpoint cursor %d is not above the real tip %d (err %v)", info.Cursor, w.chain.Head().Number, err)
	}

	for _, n := range w.nodes {
		n.SetChain(w.chain)
	}
	p := w.env.CreatePayment("25")
	w.start()
	w.pay(p, 25)
	w.mine(1)
	w.eventually("the checkpoint to be repaired", func() bool { return w.bridge.Status().Alerts[AlertCheckpointRepaired] > 0 })
	w.mine(testSettle + testConfirmations)
	w.waitStatus(p, core.StatusPaid)
	w.synced()
	if info, err := watcher.InspectCheckpoint(w.store.get()); err != nil || info.Cursor > w.chain.Head().Number {
		t.Fatalf("checkpoint cursor %d above the real tip %d (err %v)", info.Cursor, w.chain.Head().Number, err)
	}
}

// A server clock that is BEHIND the chain. Blocks then look as if
// they came from the future; they are refused (by the watcher, and the core
// would refuse them too), so nothing confirms and nothing expires — and the
// operator is told that it is the clock, in the log, as an alert and on the
// dashboard's node status.
func TestServerClockBehindIsReported(t *testing.T) {
	for _, nodes := range []int{1, 2} {
		t.Run(strconv.Itoa(nodes)+" endpoint(s)", func(t *testing.T) {
			w := newWorld(t, withNodes(nodes))
			w.start()
			w.synced()
			p := w.env.CreatePayment("25")
			unpaid := w.env.CreatePayment("10")
			headBefore := w.coreHead()

			// The chain's time runs ten minutes ahead of the server's.
			w.chain.SetTime(w.env.Clock.Now().Add(10 * time.Minute))
			w.pay(p, 25)
			w.chain.Mine(testSettle + testConfirmations + 2)

			w.eventually("a clock_skew alert", func() bool { return w.bridge.Status().Alerts[AlertClockSkew] > 0 })
			st := w.bridge.Status()
			if len(st.Warnings) == 0 || !strings.Contains(st.Warnings[0], "the clock is behind") {
				t.Fatalf("warnings = %q", st.Warnings)
			}
			if !strings.Contains(st.Watcher.LastError, "ahead") || st.Watcher.ClockSkew > -9*time.Minute {
				t.Fatalf("watcher status = %+v", st.Watcher)
			}
			if !strings.Contains(w.logs.String(), "server clock skew") {
				t.Error("clock skew not logged")
			}
			w.eventually("the node status to show the clock skew", func() bool {
				ns, err := w.env.Service.NodeStatus(w.env.Operator())
				if err != nil {
					return false
				}
				for _, e := range ns.Endpoints {
					if strings.Contains(e.Error, "server clock skew") {
						return true
					}
				}
				return false
			})
			w.never("anything is settled or expired on blocks from the future", func() bool {
				return w.coreHead() != headBefore || w.status(p) != core.StatusAwaiting || w.status(unpaid) != core.StatusAwaiting
			})

			// The clock is corrected: everything proceeds.
			w.advance(10 * time.Minute)
			w.passCooldowns()
			w.waitStatus(p, core.StatusPaid)
			if got := w.status(unpaid); got != core.StatusAwaiting {
				t.Fatalf("unpaid invoice = %s", got)
			}
			w.eventually("the warning to clear", func() bool { return len(w.bridge.Status().Warnings) == 0 })
		})
	}
}

// A server clock that is AHEAD of the chain (or endpoints that all
// serve a stale head) raises the same alert. Payments are not blocked by it.
func TestServerClockAheadDoesNotBlockPayments(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")

	// The server clock jumps five minutes ahead; the chain keeps its time.
	chainTime := w.env.Clock.Now()
	w.advance(5 * time.Minute)
	w.chain.SetTime(chainTime)
	w.pay(p, 25)
	w.chain.Mine(testSettle + testConfirmations + 2)

	w.eventually("a clock_skew alert", func() bool { return w.bridge.Status().Alerts[AlertClockSkew] > 0 })
	st := w.bridge.Status()
	if len(st.Warnings) == 0 || !strings.Contains(st.Warnings[0], "the clock is ahead") || st.Watcher.ClockSkew < 4*time.Minute {
		t.Fatalf("warnings = %q, skew = %s", st.Warnings, st.Watcher.ClockSkew)
	}
	w.waitStatus(p, core.StatusPaid)
	if n := st.Alerts[AlertClockSkew]; n != 1 {
		t.Fatalf("clock_skew alerts = %d, want one for one episode", n)
	}
}

// The log-scan configuration that cannot be protected: two endpoints of
// which only one serves eth_getLogs. Nobody can confirm that the one shows
// every transfer. Payments work; the operator is told that log scans are
// not cross-checked. (That the second endpoint has no log service is not
// concluded from its first refusal — a provider that throttles answers the
// same — but from three, over three minutes; until then the head waits and
// says what for.)
func TestOnlyOneEndpointServesLogs(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.nodes[1].Set(func(b *evmtest.Behavior) {
		b.LogsError = &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"}
	})
	w.start()
	w.firstHead()
	p := w.env.CreatePayment("25")
	w.pay(p, 25)
	w.mine(testSettle + testConfirmations + 2)
	w.never("blocks are settled on one endpoint's logs after a single refusal of the other", func() bool {
		return w.watcherStatus().LogsUnchecked || w.status(p) == core.StatusPaid
	})
	w.writeOffLogs()
	w.waitStatus(p, core.StatusPaid)
	w.eventually("a logs_unchecked alert and warning", func() bool {
		st := w.bridge.Status()
		return st.Alerts[AlertLogsUnchecked] == 1 && len(st.Warnings) == 1 && strings.Contains(st.Warnings[0], "not cross-checked") &&
			st.Watcher.LogsUnchecked && st.Watcher.LogVerifier == ""
	})
	// Headers and receipts are still cross-checked by the second endpoint.
	if v := w.watcherStatus().Verifier; v != evm.RedactURL(w.urls[1]) {
		t.Fatalf("verifier = %q", v)
	}
}

// futureTimestamps makes a node stamp every block it is asked for by number
// a year ahead ("every block is newer than anything you ask about"); its
// latest block keeps its real time.
func futureTimestamps(w *world) func(b *evmtest.Behavior) {
	return func(b *evmtest.Behavior) {
		b.Intercept = func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
			if method != "eth_getBlockByNumber" {
				return nil, nil, false
			}
			n, ok := blockNumberParam(params)
			if !ok {
				return nil, nil, false
			}
			blk, ok := w.chain.BlockByNumber(n)
			if !ok {
				return nil, nil, false
			}
			return map[string]any{
				"number": hexQuantity(blk.Number), "hash": blk.Hash.String(), "parentHash": blk.ParentHash.String(),
				"timestamp": hexQuantity(blk.Time + 365*24*3600), "transactions": []string{},
			}, nil, true
		}
	}
}

// lowestLogQuery returns the lowest block any node was asked to scan.
func lowestLogQuery(w *world) uint64 {
	lowest := ^uint64(0)
	for _, n := range w.nodes {
		for _, q := range n.LogQueries() {
			lowest = min(lowest, q.From)
		}
	}
	return lowest
}

// For payments created before the core had a head, the block to start
// scanning from is found by a binary search over block timestamps. Taking
// the lower of two endpoints' answers would let one endpoint stamping every
// block "newer than the payment" give block 1, and the watcher would set out
// to scan the whole chain. So two endpoints have to agree on the answer: with two endpoints the watcher does not start on a
// disputed answer and says so; with three the liar is outvoted.
func TestOneEndpointCannotSendTheScanToBlockOne(t *testing.T) {
	setup := func(t *testing.T, nodes int) (*world, core.PaymentView) {
		w := newWorld(t, withNodes(nodes))
		p := w.legacyPayment("25") // created before the core had a head: no SinceBlock
		w.advance(time.Minute)
		w.pay(p, 25)
		w.mine(testSettle + testConfirmations + 2)
		if cs, _ := w.env.Service.ChainState(w.env.System()); cs.Head.Number != 0 || cs.Addresses[0].SinceBlock != 0 {
			t.Fatalf("test setup: %+v", cs)
		}
		w.nodes[0].Set(futureTimestamps(w))
		return w, p
	}

	t.Run("two endpoints: no start on a disputed answer", func(t *testing.T) {
		w, p := setup(t, 2)
		w.start()
		w.eventually("an alert about the disputed block time", func() bool {
			st := w.bridge.Status()
			return st.Alerts[AlertEndpointFault] > 0 && strings.Contains(st.LastAlert.Message+st.Watcher.LastError, "no two endpoints agree")
		})
		w.never("a scan from the beginning of the chain", func() bool { return lowestLogQuery(w) < chainStart-chainHistory })
		if got := w.status(p); got != core.StatusAwaiting {
			t.Fatalf("payment = %s", got)
		}
		// The endpoint is fixed (or removed): the watcher starts where
		// it should and finds the payment.
		w.nodes[0].Set(func(b *evmtest.Behavior) { b.Intercept = nil })
		w.passCooldowns()
		w.waitStatus(p, core.StatusPaid)
		if low := lowestLogQuery(w); low < chainStart-sinceMargin-1 {
			t.Fatalf("a scan started at block %d; the payment was created at block %d", low, chainStart)
		}
	})

	t.Run("three endpoints: the liar is outvoted", func(t *testing.T) {
		w, p := setup(t, 3)
		w.start()
		w.waitStatus(p, core.StatusPaid)
		if low := lowestLogQuery(w); low < chainStart-sinceMargin-1 {
			t.Fatalf("a scan started at block %d; the payment was created at block %d", low, chainStart)
		}
		st := w.bridge.Status()
		if st.Alerts[AlertEndpointFault] == 0 || !strings.Contains(w.logs.String(), "reports an impossible block") && !strings.Contains(w.logs.String(), "places the block") && !strings.Contains(w.logs.String(), "ahead of this server's clock") {
			t.Fatalf("the lying endpoint was not reported: alerts %+v", st.Alerts)
		}
	})
}

// With the check on block timestamps switched off on both sides
// (the core's MaxHeadTimeAhead and the bridge's), a server whose clock is
// five minutes behind the chain keeps working. The clock skew is still
// reported.
func TestTimeCheckOffLetsASlowClockThrough(t *testing.T) {
	w := newWorld(t, withNodes(2), withFutureHeadsAllowed())
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	w.chain.SetTime(w.env.Clock.Now().Add(5 * time.Minute))
	w.pay(p, 25)
	w.chain.Mine(testSettle + testConfirmations + 2)
	w.waitStatus(p, core.StatusPaid)
	if ahead := w.coreHead().Time.Sub(w.env.Clock.Now()); ahead < 4*time.Minute {
		t.Fatalf("core head is %s ahead of the clock; expected the head from the future to be accepted", ahead)
	}
	w.eventually("the clock skew to be reported all the same", func() bool {
		st := w.bridge.Status()
		return st.Alerts[AlertClockSkew] > 0 && len(st.Warnings) > 0
	})
}

// A throttled log endpoint, at the level of the payment: while the second log endpoint is
// throttled, payments do not confirm (and nothing expires), the operator is
// told what the head is waiting for, and no block is settled on the first
// endpoint's logs — which is hiding a payment. When the throttling ends the
// payment is found and paid.
func TestThrottledSecondLogEndpointHoldsPayments(t *testing.T) {
	w := newWorld(t, withNodes(2))
	w.start()
	w.synced()
	p := w.env.CreatePayment("25")
	w.eventually("the second endpoint to be confirming log scans", func() bool {
		w.mine(1)
		return w.watcherStatus().LogVerifier != ""
	})

	w.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"} })
	w.nodes[0].Set(func(b *evmtest.Behavior) { b.DropLogs = func(evm.Log) bool { return true } })
	w.pay(p, 25)
	paidIn := w.mine(1).Number
	w.mine(testSettle + testConfirmations + 2)

	w.eventually("a head_held alert that names the cause", func() bool {
		st := w.bridge.Status()
		return st.Alerts[AlertHeadHeld] > 0 && strings.Contains(st.LastAlert.Message, "log verification unavailable") &&
			len(st.Warnings) > 0 && strings.Contains(st.Warnings[0], "log verification unavailable")
	})
	w.never("blocks are settled on the first endpoint's logs", func() bool {
		st := w.bridge.Status()
		return w.coreHead().Number >= paidIn || st.Watcher.LogsUnchecked || st.Alerts[AlertLogsUnchecked] > 0
	})

	w.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError = nil })
	w.passCooldowns()
	got := w.waitStatus(p, core.StatusPaid)
	if got.NeedsAttention {
		t.Fatalf("payment = %+v", got)
	}
}
