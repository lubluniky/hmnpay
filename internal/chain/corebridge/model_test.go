package corebridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// TestRandomisedCoreModel drives the real core, the real bridge generation —
// built by the function production builds it with — and the real watcher
// against the scripted chain, tick by tick, through random histories, and
// checks on every tick the property the watch list exists for:
//
//	when the core has processed block H, every transfer in a block up to
//	H to the deposit address of a payment — mined after the payment was
//	created, while the payment was open or before its watch window ended
//	(by the block's own time) — has been credited to that payment.
//
// And on every tick, the property "paid" rests on:
//
//	no transfer is confirmed in a block above the one the chain has
//	finalised (as the endpoints report it), nor above the final block of
//	the core's head; and no reorganisation that keeps every finalised block
//	removes a transfer the core has confirmed.
//
// The forks are therefore as deep as the chain allows — up to its finalised
// block — whatever the core holds in the blocks they replace.
//
// What a payment is owed is the model's own account, not the core's: from
// the chain's time when the payment was handed out, the lifetime asked for
// (its TTL), and the windows that follow from the transfers the chain holds
// — an hour after the deadline, a week after each transfer it is owed (the
// payment is funded then) — less what the server clock may be behind (the
// model keeps it within 90 seconds, beyond an episode in which it is far
// behind). The core's status and WatchUntil decide nothing. Credited means
// the very transfers: those owed are among the payment's live transfers, by
// transaction, log, block and amount, and every live transfer the core
// holds at or below its head is one the chain holds, to that address. A payment paid
// in full in time is paid; a transfer mined after the latest deadline the
// payment can have, once confirmed, has raised payment.late_funds.
//
// What the histories are made of is what decides where a payment is looked
// for, and for how long:
//
//   - the chain keeps real time, and the server's clock does not: it is put
//     forwards and backwards (within what keeps blocks from looking like
//     ones from the future) — around the first start, around the creation
//     of payments, around their deadlines;
//   - a first start that happens long after the first payments were
//     created (the endpoints were down, or the operator had not finished):
//     the chain moves on, payments are paid, the clock is corrected, and
//     only then does the watcher run;
//   - pauses of every length in which nobody ticks — a quiet watcher's idle
//     interval, an outage of an hour, of days, of the week a paid payment
//     is watched for — with money mined in them;
//   - money sent to settled payments seconds before their watch window ends
//     (one hour for an unpaid invoice, a week for a paid one), followed by a
//     pause that takes the clock past the end before the next tick;
//   - endpoint outages, and restarts of the generation from its checkpoint;
//   - competing forks: the chain's last blocks replaced — as deep as its
//     finalised block, which runs two blocks behind the tip (as on BSC) or
//     forty (the processed head and the payments' confirmations long past
//     it) — most of their transactions mined again later, some never;
//   - transactions that pay several times: one to two payments, or twice to
//     one (a batch payout), each log its own transfer;
//   - the operator switching to another token for a while and back, while
//     money is mined to the first token's addresses inside their windows —
//     also right after money to a settled payment was seen and is not
//     final yet: nothing of the first token may be in what the watcher of
//     the second is given, nor be judged by its head;
//   - crashes between the watcher's checkpoint write and the core's
//     commit of a fact, in both orders;
//   - the server clock far behind (hours) at the moment a payment with a
//     long lifetime is asked for, put right at once, and the payer paying
//     inside that lifetime;
//   - in pools of two, one endpoint without a log service — for good, or
//     until a step of the history, the other one hiding payments from its
//     logs meanwhile: those are owed once the first has a log service.
//
// HMNPAY_CORE_MODEL_SEEDS sets the number of histories (default 40) and
// HMNPAY_CORE_MODEL_FIRST_SEED the first of them (default 0);
// HMNPAY_CORE_MODEL_DEBUG_SEED replays one with a trace.
func TestRandomisedCoreModel(t *testing.T) {
	seeds := 40
	if env := os.Getenv("HMNPAY_CORE_MODEL_SEEDS"); env != "" {
		n, err := strconv.Atoi(env)
		if err != nil {
			t.Fatalf("HMNPAY_CORE_MODEL_SEEDS: %v", err)
		}
		seeds = n
	}
	if testing.Short() {
		seeds = min(seeds, 8)
	}
	first := 0
	if env := os.Getenv("HMNPAY_CORE_MODEL_FIRST_SEED"); env != "" {
		n, err := strconv.Atoi(env)
		if err != nil {
			t.Fatalf("HMNPAY_CORE_MODEL_FIRST_SEED: %v", err)
		}
		first = n
	}
	if env := os.Getenv("HMNPAY_CORE_MODEL_DEBUG_SEED"); env != "" {
		n, err := strconv.Atoi(env)
		if err != nil {
			t.Fatalf("HMNPAY_CORE_MODEL_DEBUG_SEED: %v", err)
		}
		first, seeds = n, 1
	}
	const group = 50
	for lo := first; lo < first+seeds; lo += group {
		hi := min(lo+group, first+seeds) - 1
		t.Run(fmt.Sprintf("seeds=%d-%d", lo, hi), func(t *testing.T) {
			for seed := lo; seed <= hi; seed++ {
				t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
					t.Parallel()
					runCoreHistory(t, uint64(seed), os.Getenv("HMNPAY_CORE_MODEL_DEBUG_SEED") != "")
				})
			}
		})
	}
}

// coreModel is one history.
type coreModel struct {
	t     *testing.T
	w     *world
	rng   *rand.Rand
	debug bool
	ctx   context.Context
	b     *Bridge
	g     *generation
	// real is the chain's clock: the time the next block is stamped with.
	// The server's clock (w.env.Clock) moves with it, plus whatever it has
	// been put forwards or backwards by (skew).
	real time.Time
	skew time.Duration

	payments []*modelPayment
	sent     []*sentTransfer
	queued   []*sentTransfer // sent, not mined yet
	down     []int           // endpoints that are down until the next heal
	healIn   int             // steps until they are back
	ticks    int
	trace    []string

	rng3 *rand.Rand // the generators added later draw from a stream of their own
	rng4 *rand.Rand // and the finality generators from another
	mu   sync.Mutex // hiding and hidden: the endpoints read them
	// away is set while another token is configured, since awayAt (by the
	// server's clock).
	away   bool
	awayAt time.Time
	// logless is the endpoint without a log service (-1: none); for good,
	// or until it recovers. hiding is set while the other one leaves the
	// payments in hidden out of its logs.
	logless   int
	forGood   bool
	recovered bool
	hiding    bool
	hidden    map[evm.Hash]bool
	// network is set while another network is configured (with away).
	network bool
	// episode is set while the server clock is hours behind (a payment
	// refused as clock_behind is right then, and only then).
	episode bool
	// inFlight is the kind of fact ("head", "transfer", "revert",
	// "status") at which the settings are changed next while it is in
	// flight ("": none pending).
	inFlight string
	// finality is how far the chain's finalised block trails its tip, as
	// the endpoints report it (it changes within a history). finalised is
	// the chain's finalised block: the highest the endpoints have reported
	// — while they report none, it stays where it was. stopIn counts the
	// steps until the endpoints report finality again (0: they do); quiet
	// is the endpoint that alone reports none (-1: none).
	finality  uint64
	finalised uint64
	stopIn    int
	quiet     int
	// coreFinal is the highest final block of the core's head seen in the
	// current configuration of the asset; retired, the payments seen
	// retired.
	coreFinal uint64
	retired   map[string]bool
	// confirmed holds the transfers the core has been seen to confirm;
	// maxFinal is the highest final block of a head the core accepted.
	confirmed map[string]bool
	maxFinal  uint64
	// crash is the point at which the process dies next ("": none armed):
	// "after checkpoint" — the next fact the core is to commit after the
	// watcher saved its checkpoint is not committed; "before checkpoint" —
	// the next checkpoint after a fact was handed to the core is not
	// written. saved and handed say what happened in the current tick.
	crash         string
	saved, handed bool
	inTick        bool
	// disputeIn counts the steps until the endpoint that stamps its newest
	// block in the future stops (0: none does).
	disputeIn int
	disputer  int
	// off names generators switched off (HMNPAY_CORE_MODEL_OFF, a comma-
	// separated list): their random numbers are drawn all the same, so a
	// history without one is the same history otherwise — to tell which
	// generator a failing seed needs.
	off map[string]bool
}

type modelPayment struct {
	view core.PaymentView
	to   ethaddr.Address
	// created is the chain's time when it was handed out, clock the
	// server's (which its deadline is counted from, at the earliest), ttl
	// the lifetime asked for, amount its amount in whole units.
	created time.Time
	clock   time.Time
	ttl     time.Duration
	amount  int64
	// headAtCreation is the core's processed head then: for the generator
	// that forks the chain at it, never for the model's account.
	headAtCreation uint64
}

type sentTransfer struct {
	tx evm.Hash
	// log is the position of its log among the transaction's logs (a
	// transaction can pay several times).
	log     int
	p       *modelPayment
	amount  int64
	block   uint64
	time    uint64 // the block's timestamp
	dropped bool   // reorganised away, and never mined again
}

// Skew bounds: further behind than this the chain's blocks look like ones
// from the future (refused, by design, and nothing would happen at all);
// ahead is unbounded in production, ten minutes is plenty here.
const (
	modelMaxBehind = 90 * time.Second
	modelMaxAhead  = 10 * time.Minute
)

func runCoreHistory(t *testing.T, seed uint64, debug bool) {
	rng := rand.New(rand.NewPCG(seed, 0xc0de))
	nodes := 1 + rng.IntN(3)
	if nodes == 1 && rng.IntN(2) == 0 {
		nodes = 2 // two endpoints, the recommended configuration, more often
	}
	w := newWorld(t, withNodes(nodes))
	w.chain.AddToken(modelOtherToken, "USDC", 18)
	m := &coreModel{t: t, w: w, rng: rng, debug: debug, ctx: context.Background(), real: w.env.Clock.Now(),
		rng3: rand.New(rand.NewPCG(seed, 0x5eed)), rng4: rand.New(rand.NewPCG(seed, 0xf1a2)), logless: -1, hidden: map[evm.Hash]bool{}, off: map[string]bool{},
		confirmed: map[string]bool{}, retired: map[string]bool{}, quiet: -1}
	// The finalised block: two blocks behind the tip, as on BSC (ahead of
	// the processed head), or forty (behind it, and behind the payments'
	// confirmations).
	m.finality = []uint64{2, 6, 40}[rand.New(rand.NewPCG(seed, 0xf1a1)).IntN(3)]
	m.setFinality(m.finality)
	m.observeFinality()
	for _, g := range strings.Split(os.Getenv("HMNPAY_CORE_MODEL_OFF"), ",") {
		m.off[strings.TrimSpace(g)] = true
	}
	// One of two endpoints without a log service: for good, or until a
	// step of the history, the other hiding payments meanwhile.
	recoverAt := 30 + m.rng3.IntN(90)
	if nodes == 2 && m.rng3.IntN(3) == 0 {
		m.logless, m.forGood = m.rng3.IntN(2), m.rng3.IntN(2) == 0
		m.hiding = !m.forGood
		w.nodes[m.logless].Set(func(b *evmtest.Behavior) {
			b.LogsError = &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"}
		})
		w.nodes[1-m.logless].Set(func(b *evmtest.Behavior) { b.DropLogs = func(l evm.Log) bool { return m.isHidden(l.TxHash) } })
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("seed %d, %d endpoint(s); history (tail):\n%s", seed, nodes, strings.Join(m.trace[max(0, len(m.trace)-80):], "\n"))
			if debug {
				t.Logf("bridge and watcher log:\n%s", w.logs.String())
			}
		}
	})
	m.logf("seed=%d nodes=%d logless=%d forGood=%v recoverAt=%d", seed, nodes, m.logless, m.forGood, recoverAt)
	opts := w.opts
	opts.Now = w.env.Clock.Now
	opts.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	opts.inFlight = func(kind string) {
		if m.crash == "after checkpoint" && m.saved && m.inTick {
			m.crash = ""
			m.logf("crash: the checkpoint is written, the %s is not committed", kind)
			panic(crashed{})
		}
		m.handed = true
		if m.inFlight == "" || m.inFlight != kind {
			return
		}
		m.inFlight = ""
		m.logf("the settings change while a %s is in flight", kind)
		m.changeSettings()
	}
	opts.headAccepted = func(h core.ChainHead) { m.maxFinal = max(m.maxFinal, min(h.Final, h.Number)) }
	w.store.onSave = func() {
		if m.crash == "before checkpoint" && m.handed && m.inTick {
			m.crash = ""
			m.logf("crash: a fact is committed, the checkpoint after it is not written")
			panic(crashed{})
		}
		m.saved = true
	}
	b, err := New(w.env.Service, w.store, opts)
	if err != nil {
		t.Fatal(err)
	}
	m.b = b

	// A first start long after the first payments, sometimes with the
	// server clock off while they were created, and corrected since.
	if rng.IntN(2) == 0 {
		off := time.Duration(rng.IntN(4)) * 3 * time.Minute // 0, 3, 6 or 9 minutes ahead
		m.shift(off)
		for range 1 + rng.IntN(2) {
			if p := m.create(); p != nil && rng.IntN(4) > 0 {
				m.pay(p, int64(1+rng.IntN(30)))
				m.mine(1, time.Second)
			}
		}
		m.mine(70+rng.IntN(150), time.Second)
		m.shift(-m.skew + time.Duration(rng.IntN(3)-1)*time.Minute)
	}
	m.start()

	for step := range 140 {
		if m.stopIn > 0 {
			if m.stopIn--; m.stopIn == 0 {
				m.resumeFinality()
			}
		}
		if len(m.down) > 0 {
			if m.healIn--; m.healIn <= 0 {
				m.heal()
			}
		}
		if m.logless >= 0 && !m.forGood && step == recoverAt {
			m.recover()
		}
		switch r := m.rng3.IntN(100); {
		case r < 3:
			m.switchAsset()
		case r < 7:
			m.fork()
		case r < 9:
			m.clockBehindEpisode()
		case r < 12:
			// The settings change while the next fact of a kind is in
			// flight: between the snapshot a tick works from and what it
			// reports.
			if kind := []string{"head", "head", "transfer", "status", "revert"}[m.rng3.IntN(5)]; !m.off["inflight"] {
				m.inFlight = kind
			}
		case r < 14:
			if !m.off["forkatcreation"] {
				m.forkAtCreation()
			}
		case r < 16 && nodes > 1 && m.disputeIn == 0:
			disputer, steps := m.rng3.IntN(nodes+2), 2+m.rng3.IntN(8)
			ahead := time.Duration(3+m.rng3.IntN(10)) * time.Minute
			// Half of the time the chain stands still a while first: the
			// other endpoints' newest block is old by then.
			still := time.Duration(m.rng3.IntN(2)*(5+m.rng3.IntN(20))) * time.Minute
			if m.off["dispute"] {
				break
			}
			// The endpoint in use or the verifier: an endpoint whose
			// newest block nobody looks at says nothing.
			if st := m.g.w.Status(); disputer < 3 {
				role := []string{st.Endpoint, st.Endpoint, st.Verifier}[disputer]
				for i, u := range m.w.urls {
					if evm.RedactURL(u) == role {
						disputer = i
					}
				}
			}
			disputer %= len(m.w.nodes)
			m.disputer, m.disputeIn = disputer, steps
			if still > 0 {
				m.real = m.real.Add(still)
				m.w.env.Clock.Advance(still)
				m.logf("no block for %s", still)
			}
			m.w.nodes[m.disputer].Set(func(b *evmtest.Behavior) { b.Intercept = futureNewest(m.w, ahead) })
			m.logf("endpoint %d stamps its newest block %s ahead", m.disputer, ahead)
			// A tick reports what the endpoints say, and a payment is asked
			// for: it must not be refused on one endpoint's word.
			m.tick()
			m.create()
		case r < 17:
			m.switchNetwork()
		case r < 19:
			if !m.off["batch"] {
				m.payBatch()
			}
		case r < 21:
			if !m.off["lateswitch"] {
				m.lateSwitch()
			}
		case r < 23:
			if !m.off["crash"] {
				m.crash = []string{"after checkpoint", "before checkpoint"}[m.rng3.IntN(2)]
			}
		}
		// Finality, from a stream of its own: it changes depth, stops for a
		// while (on every endpoint, or on one), and a restart without the
		// checkpoint falls into such a stretch.
		switch r := m.rng4.IntN(100); {
		case m.off["finality"]:
		case r < 3:
			m.setFinality([]uint64{2, 6, 20, 40}[m.rng4.IntN(4)])
			m.logf("the endpoints report finality %d blocks behind the tip", m.finality)
		case r < 6 && m.stopIn == 0:
			m.stopFinality(-1, 3+m.rng4.IntN(30))
		case r < 8 && m.stopIn == 0 && len(m.w.nodes) > 1:
			m.stopFinality(m.rng4.IntN(len(m.w.nodes)), 3+m.rng4.IntN(30))
		case r < 9 && m.stopIn == 0 && !(m.logless >= 0 && !m.recovered):
			// A long stretch without finality, longer than the watcher's
			// reorganisation window, and a restart without the checkpoint
			// in it. (Not while the only log source hides payments: the
			// record of the blocks it settled alone, which a second log
			// endpoint has to scan again, goes with the checkpoint — a
			// documented limit.)
			m.stopFinality(-1, 2)
			for range 10 {
				m.mine(30, time.Second)
				m.tick()
			}
			m.logf("restart without the checkpoint while no endpoint reports finality")
			m.w.store.set(nil)
			m.start()
			m.tick()
		}
		switch op := rng.IntN(100); {
		case op < 22:
			m.mine(1+rng.IntN(4), time.Duration(1+rng.IntN(3))*time.Second)
		case op < 45:
			for range 1 + rng.IntN(3) {
				m.tick()
			}
		case op < 55:
			// A payment, now and then while the clock is off.
			off := time.Duration(0)
			if rng.IntN(3) == 0 {
				off = time.Duration(rng.IntN(7)-2) * time.Minute
			}
			m.shift(off)
			m.create()
			m.shift(-off)
		case op < 67:
			if p := m.anyPayment(); p != nil {
				m.pay(p, int64(1+rng.IntN(40)))
				if rng.IntN(2) == 0 {
					m.mine(1, time.Second)
				}
			}
		case op < 74:
			// Nobody ticks for a while: an idle pause, an outage, a week.
			pauses := []time.Duration{20 * time.Second, 2 * time.Minute, 31 * time.Minute, 62 * time.Minute, 3 * time.Hour, 2 * 24 * time.Hour, 7*24*time.Hour + time.Hour}
			d := pauses[rng.IntN(len(pauses))]
			var p *modelPayment
			if rng.IntN(2) == 0 {
				p = m.anyPayment()
			}
			m.pause(d, p)
		case op < 84:
			m.windowEdge()
		case op < 89:
			// The server clock is corrected (or goes wrong).
			shifts := []time.Duration{-time.Minute, -5 * time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, -10 * time.Minute}
			m.shift(shifts[rng.IntN(len(shifts))])
		case op < 93:
			m.restart()
		case nodes > 1:
			// An outage of one endpoint, for a few steps.
			i := rng.IntN(nodes)
			m.logf("endpoint %d down", i)
			w.nodes[i].Set(func(b *evmtest.Behavior) { b.Down = true })
			m.down = append(m.down, i)
			m.healIn = 2 + rng.IntN(10)
		}
	}
	m.converge()
}

func (m *coreModel) logf(format string, args ...any) {
	line := fmt.Sprintf("[%s skew %s] ", m.real.UTC().Format("01-02 15:04:05"), m.skew) + fmt.Sprintf(format, args...)
	m.trace = append(m.trace, line)
	if m.debug {
		m.t.Log(line)
	}
}

// shift puts the server clock forwards (d > 0) or backwards, within bounds.
func (m *coreModel) shift(d time.Duration) {
	skew := min(max(m.skew+d, -modelMaxBehind), modelMaxAhead)
	if d = skew - m.skew; d == 0 {
		return
	}
	m.skew = skew
	m.w.env.Clock.Advance(d)
	m.logf("server clock moved by %s", d)
}

// mine mines n blocks, each `step` after the one before, by the chain's
// clock (the server's moves on with it).
func (m *coreModel) mine(n int, step time.Duration) {
	for range n {
		m.real = m.real.Add(step)
		m.w.env.Clock.Advance(step)
		m.w.chain.SetTime(m.real)
		blk := m.w.chain.Mine(1)
		m.observeFinality()
		var still []*sentTransfer
		for _, s := range m.queued {
			mined := false
			for _, tx := range blk.Txs {
				if tx.Hash == s.tx {
					s.block, s.time, mined = blk.Number, blk.Time, true
				}
			}
			if !mined {
				still = append(still, s)
			}
		}
		m.queued = still
	}
}

// pause lets d go by in which nobody ticks, with a few blocks in it, and
// (if p is set) a payment to p mined in the middle of it.
func (m *coreModel) pause(d time.Duration, p *modelPayment) {
	m.logf("pause of %s", d)
	blocks := 1 + m.rng.IntN(3)
	for i := range blocks {
		m.mine(1, d/time.Duration(blocks))
		if p != nil && i == 0 {
			m.pay(p, int64(1+m.rng.IntN(20)))
		}
	}
	m.mine(1, time.Second)
}

func (m *coreModel) create() *modelPayment {
	ttl := time.Duration(0)
	if m.rng3.IntN(5) == 0 {
		ttl = []time.Duration{10 * time.Minute, 2 * time.Hour, 26 * time.Hour}[m.rng3.IntN(3)]
	}
	return m.createTTL(ttl)
}

// createTTL creates a payment with the lifetime ttl (0: the default).
func (m *coreModel) createTTL(ttl time.Duration) *modelPayment {
	if m.away {
		return nil // the payments of the model are in the first token
	}
	amount := 10 + m.rng.IntN(90)
	v, _, err := m.w.env.Service.CreatePayment(m.w.env.System(), core.CreatePaymentInput{Amount: strconv.Itoa(amount), TTL: ttl})
	if err != nil {
		if errors.Is(err, core.ErrClockBehind) && !m.episode {
			// The server clock is never more than 90 seconds behind the
			// chain here, and the honest endpoints' blocks are never ahead
			// of it: nothing justifies this but one endpoint's word.
			m.t.Fatalf("CLOCK GATE: a payment was refused with the server clock %s off the chain's: %v", m.skew, err)
		}
		if core.KindOf(err) == core.KindUnavailable {
			m.logf("payment refused: %v", err)
			return nil
		}
		m.t.Fatalf("create payment: %v", err)
	}
	to, err := parseAddress(v.Address)
	if err != nil {
		m.t.Fatal(err)
	}
	if ttl == 0 {
		ttl = core.DefaultPaymentTTL
	}
	p := &modelPayment{view: v, to: to, created: m.real, clock: m.w.env.Clock.Now(), ttl: ttl, amount: int64(amount), headAtCreation: m.coreHead().Number}
	m.payments = append(m.payments, p)
	m.logf("payment %s of %d created, lifetime %s (core head %d)", v.ID, amount, ttl, m.coreHead().Number)
	return p
}

func (m *coreModel) anyPayment() *modelPayment {
	if len(m.payments) == 0 {
		return nil
	}
	return m.payments[m.rng.IntN(len(m.payments))]
}

func (m *coreModel) pay(p *modelPayment, amount int64) {
	tx := m.w.chain.Transfer(usdt, payer, p.to, units(amount))
	s := &sentTransfer{tx: tx, p: p, amount: amount}
	m.sent = append(m.sent, s)
	m.queued = append(m.queued, s)
	m.logf("pay %d to %s (tx %s)", amount, p.view.ID, tx.String()[:10])
	m.hide(tx)
}

// payBatch sends one transaction that pays twice: two payments, or one of
// them twice (a batch payout). Each of its logs is a transfer of its own.
func (m *coreModel) payBatch() {
	a, b := m.anyPayment(), m.anyPayment()
	if a == nil {
		return
	}
	if m.rng3.IntN(2) == 0 {
		b = a
	}
	x, y := int64(1+m.rng3.IntN(30)), int64(1+m.rng3.IntN(30))
	tx := m.w.chain.Submit(evmtest.TransferLog(usdt, payer, a.to, units(x)), evmtest.TransferLog(usdt, payer, b.to, units(y)))
	for i, s := range []*sentTransfer{{tx: tx, log: 0, p: a, amount: x}, {tx: tx, log: 1, p: b, amount: y}} {
		m.sent = append(m.sent, s)
		m.queued = append(m.queued, s)
		m.logf("pay %d to %s (tx %s, log %d)", s.amount, s.p.view.ID, tx.String()[:10], i)
	}
	m.hide(tx)
	if m.rng3.IntN(2) == 0 {
		m.mine(1, time.Second)
	}
}

// lateSwitch: money to a settled payment is seen, and the operator switches
// to the other token before it is final.
func (m *coreModel) lateSwitch() {
	if m.away || m.network || len(m.down) > 0 {
		return
	}
	for _, p := range m.payments {
		if !m.record(p).Status.Settled() {
			continue
		}
		m.pay(p, int64(1+m.rng3.IntN(20)))
		m.mine(1, time.Second)
		m.tick()
		m.logf("money to settled %s is seen; now the token changes", p.view.ID)
		m.switchAsset()
		return
	}
}

// hide has the only log source leave tx out of its logs, half of the time,
// while it hides payments.
func (m *coreModel) hide(tx evm.Hash) {
	m.mu.Lock()
	if m.hiding && !m.recovered && m.rng3.IntN(2) == 0 {
		m.hidden[tx] = true
		m.logf("  the only log source hides it")
	}
	m.mu.Unlock()
}

// isHidden reports whether the endpoint that serves logs leaves tx out.
func (m *coreModel) isHidden(tx evm.Hash) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hiding && m.hidden[tx]
}

// wasHidden reports whether tx was hidden at some point.
func (m *coreModel) wasHidden(tx evm.Hash) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hidden[tx]
}

// recover gives the endpoint without a log service one.
func (m *coreModel) recover() {
	m.logf("the endpoint without a log service gets one")
	m.w.nodes[m.logless].Set(func(b *evmtest.Behavior) { b.LogsError = nil })
	m.recovered = true
}

var (
	modelOtherToken = ethaddr.MustParse("0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d")
	modelOtherAsset = core.Asset{Symbol: "USDC", Network: "BEP20", ChainID: core.ChainBSCMainnet, Contract: "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d", Decimals: 18}
)

// switchAsset switches the instance to the other token, or back. It can
// only while no payment is open.
func (m *coreModel) switchAsset() {
	if m.setAsset() {
		m.start()
	}
}

// setAsset changes the token (away, or back) in the settings, and reports
// whether it did; the generation is the caller's to rebuild.
func (m *coreModel) setAsset() bool {
	if m.network {
		return false
	}
	a := modelOtherAsset
	if m.away {
		a = core.USDTBSC
	}
	_, err := m.w.env.Service.UpdateNetwork(m.w.env.Relogin(), core.UpdateNetworkInput{Asset: &a, RPCURLs: m.w.urls})
	if errors.Is(err, core.ErrOpenPayments) {
		m.logf("no switch to %s: payments are open", a.Symbol)
		return false
	}
	if err != nil {
		m.t.Fatalf("switch to %s: %v", a.Symbol, err)
	}
	m.away = !m.away
	m.awayAt = m.w.env.Clock.Now()
	m.coreFinal = 0 // the core's head starts anew
	m.logf("the operator switches to %s", a.Symbol)
	return true
}

// switchNetwork switches the instance to another network (which the
// endpoints do not serve), or back.
func (m *coreModel) switchNetwork() {
	if m.away && !m.network {
		return
	}
	in := core.UpdateNetworkInput{ChainID: core.ChainBSCTestnet, RPCURLs: m.w.urls}
	if m.network {
		in = core.UpdateNetworkInput{ChainID: core.ChainBSCMainnet, Asset: &core.USDTBSC, RPCURLs: m.w.urls}
	}
	_, err := m.w.env.Service.UpdateNetwork(m.w.env.Relogin(), in)
	if errors.Is(err, core.ErrOpenPayments) {
		return
	}
	if err != nil {
		m.t.Fatalf("switch to chain %d: %v", in.ChainID, err)
	}
	m.network = !m.network
	m.away = m.network
	m.awayAt = m.w.env.Clock.Now()
	m.coreFinal = 0
	m.logf("the operator switches to chain %d", in.ChainID)
	m.start()
}

// changeSettings changes the chain configuration — the token, the
// endpoints or the finality depth — without rebuilding the generation: it
// is called while a fact of the running one is in flight.
func (m *coreModel) changeSettings() {
	switch m.rng3.IntN(3) {
	case 0:
		if m.setAsset() {
			return
		}
		fallthrough
	case 1:
		urls := slices.Clone(m.w.urls)
		if len(urls) > 1 {
			urls = append(urls[1:], urls[0])
		} else {
			urls[0] = m.w.nodes[0].Serve(m.t) // the same endpoint under another URL
		}
		if _, err := m.w.env.Service.UpdateNetwork(m.w.env.Relogin(), core.UpdateNetworkInput{RPCURLs: urls}); err != nil {
			if m.away {
				return // the other network's settings are left as they are
			}
			m.t.Fatalf("change the endpoints: %v", err)
		}
		if len(urls) > 1 {
			m.w.nodes = append(m.w.nodes[1:], m.w.nodes[0])
			if m.logless >= 0 {
				m.logless = (m.logless + len(m.w.nodes) - 1) % len(m.w.nodes)
			}
			if m.disputeIn > 0 {
				m.disputer = (m.disputer + len(m.w.nodes) - 1) % len(m.w.nodes)
			}
			for i, d := range m.down {
				m.down[i] = (d + len(m.w.nodes) - 1) % len(m.w.nodes)
			}
		}
		m.w.urls = urls
		m.logf("the endpoints change: %v", urls)
	default:
		st, err := m.w.env.Service.GetSettings(m.w.env.Relogin())
		if err != nil {
			m.t.Fatal(err)
		}
		ps := st.Payments
		ps.RequiredConfirmations = 7 - ps.RequiredConfirmations // 3 and 4
		if _, err := m.w.env.Service.UpdatePaymentDefaults(m.w.env.Relogin(), ps); err != nil {
			m.t.Fatalf("change the finality depth: %v", err)
		}
		m.logf("the finality depth changes to %d", ps.RequiredConfirmations)
	}
}

// futureNewest makes a node stamp its newest block ahead of the server
// clock (asked for by number or by the "latest" tag); older blocks keep
// their time.
func futureNewest(w *world, ahead time.Duration) func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
	return func(method string, params []json.RawMessage) (any, *evmtest.RPCError, bool) {
		if method != "eth_getBlockByNumber" || len(params) == 0 {
			return nil, nil, false
		}
		head := w.chain.Head()
		var tag string
		n, numeric := blockNumberParam(params)
		if !numeric && (json.Unmarshal(params[0], &tag) != nil || tag != "latest") || numeric && n != head.Number {
			return nil, nil, false
		}
		return map[string]any{
			"number": hexQuantity(head.Number), "hash": head.Hash.String(), "parentHash": head.ParentHash.String(),
			"timestamp": hexQuantity(uint64(w.env.Clock.Now().Add(ahead).Unix())), "transactions": []string{},
		}, nil, true
	}
}

// setFinality has the endpoints report the block depth below the tip as
// finalized.
func (m *coreModel) setFinality(depth uint64) {
	m.finality = depth
	for _, n := range m.w.nodes {
		n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = depth })
	}
	m.observeFinality()
}

// stopFinality has every endpoint (who < 0) or one stop reporting a
// finalized block for steps steps. While none does, the chain finalises
// nothing new; while one does not, the chain goes on finalising.
func (m *coreModel) stopFinality(who, steps int) {
	m.stopIn, m.quiet = steps, who
	for i, n := range m.w.nodes {
		if who < 0 || i == who {
			n.Set(func(b *evmtest.Behavior) { b.NoFinalized = true })
		}
	}
	if who < 0 {
		m.logf("no endpoint reports a finalized block for %d steps (the chain finalises nothing; finalised at %d)", steps, m.finalised)
	} else {
		m.logf("endpoint %d reports no finalized block for %d steps", who, steps)
	}
}

// resumeFinality has every endpoint report finality again.
func (m *coreModel) resumeFinality() {
	for _, n := range m.w.nodes {
		n.Set(func(b *evmtest.Behavior) { b.NoFinalized = false })
	}
	m.stopIn, m.quiet = 0, -1
	m.w.passCooldowns() // asked for the tag again
	m.observeFinality()
	m.logf("the endpoints report finality again")
}

// observeFinality moves the chain's finalised block up to what the endpoints
// report, unless none reports one.
func (m *coreModel) observeFinality() {
	if m.stopIn > 0 && m.quiet < 0 {
		return
	}
	tip := m.w.chain.Head().Number
	if f := tip - min(tip, m.finality); f > m.finalised {
		m.finalised = f
	}
}

// fork replaces the chain's last blocks by a competing fork: most of their
// transactions are mined again in it, some never. A third of them reach as
// deep as the chain's finalised block allows.
func (m *coreModel) fork() {
	depth := 1 + m.rng3.IntN(6)
	if m.rng3.IntN(3) == 0 {
		tip := m.w.chain.Head().Number
		// (Within the watcher's reorganisation window: a deeper one is
		// taken as a deep reorganisation, a documented limit.)
		depth = 1 + m.rng3.IntN(int(min(max(tip-min(tip, m.finalised), 1), 200)))
	}
	m.forkDepth(depth)
}

// forkAtCreation replaces the blocks from about the head the core had
// processed when the newest payment was created: a reorganisation above the
// finalized block (40 blocks back here), at and around the payment's start.
func (m *coreModel) forkAtCreation() {
	if len(m.payments) == 0 {
		return
	}
	p := m.payments[len(m.payments)-1]
	tip := m.w.chain.Head().Number
	if p.headAtCreation == 0 || p.headAtCreation+2 > tip {
		return
	}
	depth := int(tip-p.headAtCreation) + m.rng3.IntN(3)
	m.forkDepth(depth)
}

func (m *coreModel) forkDepth(depth int) {
	tip := m.w.chain.Head().Number
	if tip < chainStart+uint64(depth)+2 {
		return
	}
	// Every block the chain has finalised stays: a fork as deep as that is
	// one the chain may make, whatever the core holds in the blocks it
	// replaces — and nothing in them may be confirmed.
	// (And within the watcher's reorganisation window: a deeper fork is a
	// deep reorganisation, a documented limit.)
	if uint64(depth) > tip-min(tip, m.finalised) || depth > 200 {
		return
	}
	for _, s := range m.sent {
		if s.block <= tip-uint64(depth) {
			continue
		}
		if tr, ok := m.liveTransfers(s.p)[m.idOf(s)]; ok && tr.Status == core.TransferConfirmed && tr.BlockNumber == s.block {
			m.t.Fatalf("REORGANISED AWAY: a fork that keeps every finalised block (the chain's is %d) replaces block %d, in which payment %s has the confirmed transfer %s",
				m.finalised, s.block, s.p.view.ID, s.tx.String()[:10])
		}
	}
	dropped := m.w.chain.Reorg(depth)
	byHash := map[evm.Hash][]*sentTransfer{}
	for _, s := range m.sent {
		byHash[s.tx] = append(byHash[s.tx], s)
	}
	var again []evmtest.Tx
	for _, tx := range dropped {
		ss := byHash[tx.Hash]
		for _, s := range ss {
			s.block, s.time = 0, 0
		}
		if m.rng3.IntN(4) > 0 {
			again = append(again, tx)
			m.queued = append(m.queued, ss...)
		} else {
			for _, s := range ss {
				s.dropped = true
			}
		}
	}
	m.w.chain.Requeue(again...)
	m.logf("a fork replaces the last %d blocks (%d transactions, %d mined again)", depth, len(dropped), len(again))
	m.mine(depth+1, time.Second)
}

// clockBehindEpisode: the server clock is hours behind at the moment a
// payment with a long lifetime is asked for, and is put right at once; in
// half of them the payer pays the full amount ten minutes before the
// lifetime asked for is over.
func (m *coreModel) clockBehindEpisode() {
	if m.away || len(m.down) > 0 {
		return
	}
	// The watcher at work, as in production: the processed head is fresh.
	m.tick()
	m.tick()
	if h := m.coreHead(); h.Number == 0 || m.real.Sub(h.Time) > 10*time.Minute {
		return
	}
	behind := []time.Duration{30 * time.Minute, 2 * time.Hour, 6 * time.Hour}[m.rng3.IntN(3)]
	ttl := behind + []time.Duration{time.Hour, 3 * time.Hour}[m.rng3.IntN(2)]
	m.logf("the server clock is %s behind for a moment: a payment with a lifetime of %s", behind, ttl)
	m.w.env.Clock.Advance(-behind)
	m.episode = true
	p := m.createTTL(ttl)
	m.episode = false
	m.w.env.Clock.Advance(behind)
	if p == nil || m.rng3.IntN(2) == 0 {
		return
	}
	if at := p.created.Add(ttl - 10*time.Minute); at.After(m.real) {
		m.mine(1, at.Sub(m.real))
	}
	m.pay(p, p.amount)
	m.mine(1, time.Second)
}

// windowEdge sends money to a settled payment seconds before its watch
// window ends — by the block's time — and lets the clock run past the end
// before anybody ticks.
func (m *coreModel) windowEdge() {
	// The payments past their deadline by the model's account, whose
	// window by the model's account is not over yet.
	var settled []*modelPayment
	for _, p := range m.payments {
		_, end := m.account(p, false)
		if m.real.After(p.clock.Add(p.ttl+core.DefaultExpiryGrace+modelMaxAhead)) && end.After(m.real) {
			settled = append(settled, p)
		}
	}
	if len(settled) == 0 {
		return
	}
	p := settled[m.rng.IntN(len(settled))]
	_, until := m.account(p, false)
	before := time.Duration(1+m.rng.IntN(60)) * time.Second
	at := until.Add(-before)
	if !at.After(m.real) {
		return
	}
	m.logf("to the end of %s's watch window by the model's account (%s)", p.view.ID, until.UTC().Format("01-02 15:04:05"))
	m.mine(1, at.Sub(m.real))
	m.pay(p, int64(1+m.rng.IntN(20)))
	m.mine(1, 0)
	past := before + time.Duration(1+m.rng.IntN(1200))*time.Second
	m.mine(1+m.rng.IntN(2), past)
}

func (m *coreModel) start() {
	cs, err := m.w.env.Service.ChainState(m.w.env.System())
	if err != nil {
		m.t.Fatal(err)
	}
	p, idle := planFor(cs)
	if idle != "" {
		m.t.Fatalf("plan: %s", idle)
	}
	if m.g, err = m.b.newGeneration(m.ctx, p, cs); err != nil {
		m.t.Fatalf("build the generation: %v", err)
	}
	m.logf("generation started")
}

func (m *coreModel) restart() {
	switch r := m.rng3.IntN(6); {
	case r == 0 && !m.off["nocheckpoint"]:
		m.logf("restart without the checkpoint")
		m.w.store.set(nil)
	case r == 1:
		// Another verification mode: a restart of the process.
		modes := []watcher.VerifyMode{watcher.VerifyAuto, watcher.VerifyStrict, watcher.VerifyBestEffort}
		opts := m.b.opts
		opts.Verify = modes[m.rng3.IntN(len(modes))]
		b, err := New(m.w.env.Service, m.w.store, opts)
		if err != nil {
			m.t.Fatal(err)
		}
		m.b = b
		m.logf("restart with verification mode %d", opts.Verify)
	default:
		m.logf("restart")
	}
	m.start()
}

func (m *coreModel) heal() {
	for _, i := range m.down {
		m.w.nodes[i].Set(func(b *evmtest.Behavior) { b.Down = false })
	}
	if len(m.down) > 0 {
		m.logf("endpoints back")
		m.w.passCooldowns()
	}
	m.down = nil
}

func (m *coreModel) tick() {
	if m.logless >= 0 {
		// The endpoint without a log service is written off, and asked
		// again, on the endpoints' clock.
		m.w.passCooldowns()
	}
	if m.disputeIn > 0 {
		if m.disputeIn--; m.disputeIn == 0 {
			m.w.nodes[m.disputer].Set(func(b *evmtest.Behavior) { b.Intercept = nil })
			m.logf("endpoint %d stamps its newest block right again", m.disputer)
		}
	}
	err := m.tickOnce()
	m.ticks++
	// The status as the bridge's report loop sends it every few seconds,
	// whether the tick succeeded or not.
	m.g.report(m.ctx)
	if m.debug {
		st := m.g.w.Status()
		m.logf("tick: err=%v cursor=%d watched=%d core head=%d skew=%s", err, st.Cursor, st.WatchedAddresses, m.coreHead().Number, st.ClockSkew)
	}
	if errors.Is(err, watcher.ErrHalt) {
		// The configuration changed (in flight, or before the tick read
		// the watch set): like the bridge, build a generation for the
		// current one.
		m.logf("generation halted: %v", err)
		m.start()
	}
	m.check()
}

// crashed is what a crash panics with.
type crashed struct{}

// tickOnce runs one tick of the generation; if the process "crashes" in it
// (crash), the generation is dropped where it stands and a new one starts
// from whatever was saved: the checkpoint and the core's books.
func (m *coreModel) tickOnce() (err error) {
	m.saved, m.handed, m.inTick = false, false, true
	defer func() {
		m.inTick = false
		if r := recover(); r != nil {
			if _, ok := r.(crashed); !ok {
				panic(r)
			}
			m.logf("the process restarts after the crash")
			m.start()
			err = nil
		}
	}()
	return m.g.w.Tick(m.ctx)
}

func (m *coreModel) coreHead() core.ChainHead {
	cs, err := m.w.env.Service.ChainState(m.w.env.System())
	if err != nil {
		m.t.Fatal(err)
	}
	return cs.Head
}

func (m *coreModel) record(p *modelPayment) core.Payment {
	var rec core.Payment
	err := m.w.env.Store.View(context.Background(), func(r core.Reader) error {
		var err error
		rec, err = r.GetPayment(context.Background(), p.view.ID)
		return err
	})
	if err != nil {
		m.t.Fatal(err)
	}
	return rec
}

// owed returns, of the transfers to p that the chain holds, those the
// payment is owed — by the model's own account (see TestRandomisedCoreModel):
// mined before the end of its window, which is an hour after the deadline
// its lifetime gives it in the chain's time, and a week after each transfer
// it is owed — less the 90 seconds the server clock may be behind when it
// sets them. Hidden ones (left out of the only log source's logs) are owed
// only once final is set: when a second log endpoint has had its chance.
// Every transaction to the address was sent after it was handed out, so
// whatever block a fork moves one into is owed — the model knows nothing of
// start blocks.
func (m *coreModel) owed(p *modelPayment, final bool) []*sentTransfer {
	owed, _ := m.account(p, final)
	return owed
}

// account returns owed and the end of the payment's window by the model's
// account: from the server clock at its creation (a deadline is counted
// from it at the earliest), the lifetime, the grace and the hour of an
// unfunded invoice, then a week after each transfer it is owed.
func (m *coreModel) account(p *modelPayment, final bool) ([]*sentTransfer, time.Time) {
	var mined []*sentTransfer
	for _, s := range m.sent {
		if s.p == p && s.block != 0 && !s.dropped {
			mined = append(mined, s)
		}
	}
	sort.Slice(mined, func(i, j int) bool { return mined[i].block < mined[j].block })
	end := p.clock.Add(p.ttl + core.DefaultExpiryGrace + core.DefaultUnfundedWatchWindow)
	var owed []*sentTransfer
	for _, s := range mined {
		at := time.Unix(int64(s.time), 0)
		if !at.Before(end) {
			continue
		}
		if final || !m.wasHidden(s.tx) {
			owed = append(owed, s)
		}
		if until := at.Add(core.DefaultWatchWindow - modelMaxBehind); until.After(end) {
			end = until
		}
	}
	return owed, end
}

// logIndexOf returns the index of the transfer's log in its block, as the
// chain has it.
func (m *coreModel) logIndexOf(s *sentTransfer) uint {
	blk, ok := m.w.chain.BlockByNumber(s.block)
	if !ok {
		return ^uint(0)
	}
	i := uint(0)
	for _, tx := range blk.Txs {
		if tx.Failed {
			continue
		}
		if tx.Hash == s.tx {
			return i + uint(s.log)
		}
		i += uint(len(tx.Logs))
	}
	return ^uint(0)
}

// transferID identifies a transfer: its transaction and the index of its log
// in the block.
func transferID(tx string, logIndex uint) string {
	return strings.ToLower(tx) + "#" + strconv.FormatUint(uint64(logIndex), 10)
}

// idOf returns the transferID of a mined transfer, as the chain holds it.
func (m *coreModel) idOf(s *sentTransfer) string { return transferID(s.tx.String(), m.logIndexOf(s)) }

// liveTransfers returns the core's live transfers of p, by transferID.
func (m *coreModel) liveTransfers(p *modelPayment) map[string]core.Transfer {
	out := map[string]core.Transfer{}
	err := m.w.env.Store.View(context.Background(), func(r core.Reader) error {
		ts, err := r.ListTransfers(context.Background(), p.view.ID)
		for _, t := range ts {
			if t.Live() {
				out[transferID(t.TxHash, t.LogIndex)] = t
			}
		}
		return err
	})
	if err != nil {
		m.t.Fatal(err)
	}
	return out
}

// checkFinality is the property "paid" rests on, judged as soon as the core
// is seen to confirm a transfer (right after the tick that did): it is in a
// block the chain had finalised — the endpoints report the block finality
// trails the tip by as finalised — and at or below the final block of a head
// the core processed; and while another token is configured, no transfer of
// the first one is confirmed at all: nobody looks at that token's logs then,
// so nobody would see such a transfer leave the chain.
func (m *coreModel) checkFinality() {
	tip := m.w.chain.Head().Number
	finalised := m.finalised
	// The core's final block: never above what the chain finalised, and
	// never back (within one configuration of the asset: a token switch
	// starts the core's head anew).
	if h := m.coreHead(); h.ChainID == core.ChainBSCMainnet && h.Number != 0 {
		if h.Final > finalised {
			m.t.Fatalf("FINALITY: the core's head %d is final at block %d, above the block the chain has finalised (%d, the tip %d)", h.Number, h.Final, finalised, tip)
		}
		if h.Final < m.coreFinal {
			m.t.Fatalf("FINALITY: the core's final block went back from %d to %d", m.coreFinal, h.Final)
		}
		m.coreFinal = h.Final
	}
	m.checkRetirement()
	for _, p := range m.payments {
		for id, tr := range m.liveTransfers(p) {
			if tr.Status != core.TransferConfirmed || m.confirmed[id] {
				continue
			}
			m.confirmed[id] = true
			switch {
			case tr.BlockNumber > finalised:
				m.t.Fatalf("FINALITY: payment %s has transfer %s in block %d confirmed, above the block the chain has finalised (%d, the tip %d)",
					p.view.ID, id[:10], tr.BlockNumber, finalised, tip)
			case tr.BlockNumber > m.maxFinal:
				m.t.Fatalf("FINALITY: payment %s has transfer %s in block %d confirmed, above the final block of every head the core processed (at most %d)",
					p.view.ID, id[:10], tr.BlockNumber, m.maxFinal)
			case m.away && tr.ConfirmedAt.After(m.awayAt):
				m.t.Fatalf("ISOLATION: payment %s has transfer %s in block %d confirmed at %s, while another token is configured (since %s)",
					p.view.ID, id[:10], tr.BlockNumber, tr.ConfirmedAt.UTC().Format("01-02 15:04:05"), m.awayAt.UTC().Format("01-02 15:04:05"))
			}
		}
	}
}

// checkRetirement: no address leaves the watch list before the chain has
// finalised a block stamped at or after the end of its window — a
// reorganisation that keeps every finalised block could otherwise move money
// into the window of an address nobody watches.
func (m *coreModel) checkRetirement() {
	fin, ok := m.w.chain.BlockByNumber(m.finalised)
	if !ok {
		return
	}
	for _, p := range m.payments {
		if m.retired[p.view.ID] {
			continue
		}
		rec := m.record(p)
		if rec.RetiredAt.IsZero() {
			continue
		}
		m.retired[p.view.ID] = true
		if int64(fin.Time) < rec.WatchUntil.Unix() {
			m.t.Fatalf("RETIRED: payment %s left the watch list (window until %s), and the chain has finalised only up to block %d, stamped %s",
				p.view.ID, rec.WatchUntil.UTC().Format("01-02 15:04:05"), m.finalised, time.Unix(int64(fin.Time), 0).UTC().Format("01-02 15:04:05"))
		}
	}
}

// checkIsolation: what the watcher is given — the addresses to scan for and
// the transfers to follow — is the configured asset's alone.
func (m *coreModel) checkIsolation() {
	cs, err := m.w.env.Service.ChainState(m.w.env.System())
	if err != nil {
		m.t.Fatal(err)
	}
	asset := func(id string) core.Asset {
		var p core.Payment
		if err := m.w.env.Store.View(context.Background(), func(r core.Reader) error {
			var err error
			p, err = r.GetPayment(context.Background(), id)
			return err
		}); err != nil {
			m.t.Fatal(err)
		}
		return p.Asset
	}
	for _, t := range cs.PendingTransfers {
		if a := asset(t.PaymentID); a.ChainID != cs.Network.ChainID || a.Contract != cs.Asset.Contract {
			m.t.Fatalf("ISOLATION: the watch snapshot for %s on chain %d holds a pending transfer (tx %s) of payment %s in %s on chain %d",
				cs.Asset.Symbol, cs.Network.ChainID, t.TxHash[:10], t.PaymentID, a.Symbol, a.ChainID)
		}
	}
	for _, w := range cs.Addresses {
		if a := asset(w.PaymentID); a.ChainID != cs.Network.ChainID || a.Contract != cs.Asset.Contract {
			m.t.Fatalf("ISOLATION: the watch snapshot for %s on chain %d holds the address of payment %s in %s on chain %d",
				cs.Asset.Symbol, cs.Network.ChainID, w.PaymentID, a.Symbol, a.ChainID)
		}
	}
}

// check is the property: every transfer at or below the core's head that a
// payment is owed has been credited to it — that very transfer — and every
// live transfer the core holds at or below its head is one the chain holds,
// to that payment's address. It is judged while the core's head is a block
// of the canonical chain and the first token is configured: a head on a
// fork that has been replaced, or of another token, says nothing about the
// blocks of this chain for these addresses.
func (m *coreModel) check() { m.checkAll(false) }

func (m *coreModel) checkAll(final bool) {
	m.checkFinality()
	m.checkIsolation()
	if m.away {
		return
	}
	h := m.coreHead()
	head := h.Number
	if blk, ok := m.w.chain.BlockByNumber(head); head == 0 || !ok || blk.Hash.String() != h.Hash {
		return
	}
	byID := map[string]*sentTransfer{}
	for _, s := range m.sent {
		if s.block != 0 {
			byID[m.idOf(s)] = s
		}
	}
	for _, p := range m.payments {
		live := m.liveTransfers(p)
		var missing []string
		owedSum := int64(0)
		for _, s := range m.owed(p, final) {
			if s.block > head {
				continue
			}
			owedSum += s.amount
			tr, ok := live[m.idOf(s)]
			if !ok || tr.BlockNumber != s.block || tr.LogIndex != m.logIndexOf(s) || tr.Amount.Cmp(mustAmount(m.t, s.amount)) != 0 {
				missing = append(missing, fmt.Sprintf("%d in block %d at %s (tx %s; the core has %+v)", s.amount, s.block,
					time.Unix(int64(s.time), 0).UTC().Format("01-02 15:04:05"), s.tx.String()[:10], tr))
			}
		}
		if len(missing) > 0 {
			rec := m.record(p)
			rule := "WATCH RULE (window end)"
			if rec.CreatedAtBlock == 0 {
				rule = "WATCH RULE (start block)"
			}
			m.t.Fatalf("%s: the core has processed block %d, and payment %s (created at %s with a lifetime of %s, start block %d; status %s, watched until %s) lacks transfers it is owed in blocks up to there:\n  %s",
				rule, head, p.view.ID, p.created.UTC().Format("01-02 15:04:05"), p.ttl, rec.CreatedAtBlock, rec.Status,
				rec.WatchUntil.UTC().Format("01-02 15:04:05"), strings.Join(missing, "\n  "))
		}
		for id, tr := range live {
			if tr.BlockNumber > head {
				continue
			}
			s := byID[id]
			blk, ok := m.w.chain.BlockByNumber(tr.BlockNumber)
			if s == nil || s.p != p || s.dropped || s.block != tr.BlockNumber || !ok || blk.Hash.String() != tr.BlockHash ||
				tr.Amount.Cmp(mustAmount(m.t, s.amount)) != 0 {
				m.t.Fatalf("IDENTITY: payment %s holds transfer %s in block %d (%s) at or below the core's head %d, which the chain does not hold to its address there",
					p.view.ID, id[:10]+id[strings.IndexByte(id, '#'):], tr.BlockNumber, tr.BlockHash, head)
			}
		}
		if rec := m.record(p); rec.AmountReceived.Cmp(mustAmount(m.t, owedSum)) < 0 {
			m.t.Fatalf("payment %s has %s received, less than the %d it is owed in blocks up to %d", p.view.ID, rec.AmountReceived.Format(18), owedSum, head)
		}
	}
	if final {
		m.checkOutcomes()
	}
}

// checkOutcomes, once everything has been processed and confirmed, by the
// model's account alone:
//
//   - a payment paid in full in time — in blocks stamped before the earliest
//     deadline its lifetime can give it (from the server clock at its
//     creation) — is paid;
//   - its balances are exactly what it holds: received is the sum of its
//     live transfers (each of them a transfer the chain holds to its
//     address: checkAll), all of them confirmed, and confirmed is received;
//   - when the payment is owed transfers mined after the latest deadline it
//     can have (its lifetime from a clock up to ten minutes ahead, the
//     grace, and the 90 seconds the clock may be behind when one is seen),
//     the last payment.late_funds told the merchant of all of them: its
//     payment confirmed at least everything owed up to the last of them.
func (m *coreModel) checkOutcomes() {
	for _, p := range m.payments {
		rec := m.record(p)
		inTime := int64(0)
		for _, s := range m.sent {
			if s.p == p && s.block != 0 && !s.dropped && !m.wasHidden(s.tx) &&
				int64(s.time) < p.clock.Add(p.ttl).Unix() {
				inTime += s.amount
			}
		}
		if inTime >= p.amount && rec.Status != core.StatusPaid && rec.Status != core.StatusOverpaid {
			m.t.Fatalf("DEADLINE: payment %s of %d (created at %s by the server clock, with a lifetime of %s) was paid %d in time, and is %s (due at %s)",
				p.view.ID, p.amount, p.clock.UTC().Format("01-02 15:04:05"), p.ttl, inTime, rec.Status, rec.ExpiresAt.UTC().Format("01-02 15:04:05"))
		}
		live := m.liveTransfers(p)
		var received core.Amount
		// What the chain holds for it, by the model's account, log by log
		// (a transaction can pay several times).
		var held core.Amount
		for _, s := range m.owed(p, true) {
			held = held.Add(mustAmount(m.t, s.amount))
		}
		for id, tr := range live {
			if tr.Status != core.TransferConfirmed {
				m.t.Fatalf("BALANCE: payment %s holds transfer %s in block %d %s after everything was processed and confirmed", p.view.ID, id[:10], tr.BlockNumber, tr.Status)
			}
			received = received.Add(tr.Amount)
		}
		if received.Cmp(held) < 0 {
			m.t.Fatalf("BALANCE: payment %s holds %s in live transfers, less than the %s the chain holds for it", p.view.ID, received.Format(18), held.Format(18))
		}
		if rec.AmountReceived.Cmp(received) != 0 || rec.AmountConfirmed.Cmp(received) != 0 {
			m.t.Fatalf("BALANCE: payment %s has %s received and %s confirmed; its live transfers sum to %s",
				p.view.ID, rec.AmountReceived.Format(18), rec.AmountConfirmed.Format(18), received.Format(18))
		}
		latest := p.created.Add(p.ttl + modelMaxAhead + core.DefaultExpiryGrace + modelMaxBehind + time.Minute)
		var told int64
		var lastLate *sentTransfer
		for _, s := range m.owed(p, true) {
			if int64(s.time) >= latest.Unix() {
				lastLate = s
			}
		}
		if lastLate == nil {
			continue
		}
		for _, s := range m.owed(p, true) {
			if s.block <= lastLate.block {
				told += s.amount
			}
		}
		events, err := m.w.env.Service.Events(m.w.env.System(), core.EventFilter{PaymentID: p.view.ID, Limit: 1000})
		if err != nil {
			m.t.Fatal(err)
		}
		var last *core.Event
		for i := range events {
			if events[i].Type == core.EventPaymentLateFunds {
				last = &events[i]
			}
		}
		var snap struct {
			AmountConfirmed string `json:"amount_confirmed"`
		}
		if last != nil {
			if err := json.Unmarshal(last.Data, &snap); err != nil {
				m.t.Fatal(err)
			}
		}
		confirmed, err := core.ParseAmount(snap.AmountConfirmed, 18)
		if last == nil || err != nil || confirmed.Cmp(mustAmount(m.t, told)) < 0 {
			m.t.Fatalf("LATE FUNDS: payment %s (%s) is owed the late transfer %s of %d in block %d, and the last payment.late_funds (%v) told of %s confirmed, not the %d owed up to there: %s",
				p.view.ID, rec.Status, lastLate.tx.String()[:10], lastLate.amount, lastLate.block, last != nil, snap.AmountConfirmed, told, m.w.events(p.view))
		}
	}
}

func mustAmount(t *testing.T, whole int64) core.Amount {
	a, err := core.AmountFromBig(units(whole))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// converge: the first token configured again, endpoints healthy (the one
// without a log service, unless for good, with one), the chain grows past
// everything sent, and the watcher ticks until the core has processed it all
// — once while the only log source still hides what it hid, so that the
// other endpoint's scan of the blocks it settled is what finds it; then the
// lie ends.
func (m *coreModel) converge() {
	m.inFlight = "" // the settings stay as they are from here on
	switch {
	case m.network:
		m.switchNetwork()
	case m.away:
		m.switchAsset()
	}
	if m.disputeIn > 0 {
		m.disputeIn = 1 // ends with the next tick
	}
	if m.stopIn > 0 {
		m.resumeFinality()
	}
	if m.logless >= 0 && !m.forGood && !m.recovered {
		m.recover()
	}
	m.heal()
	m.w.passCooldowns()
	if m.hiding {
		// (With two endpoints a transfer one of them leaves out of blocks
		// that are not settled yet holds the head: that is not caught up
		// with until the lie ends.)
		m.mine(testSettle+testConfirmations+2, time.Second)
		for i := 0; i < 60 && m.g.w.Status().UncheckedFrom != 0; i++ {
			m.mine(1, time.Second)
			m.tick()
		}
		m.mu.Lock()
		m.hiding = false
		m.mu.Unlock()
		m.logf("the only log source hides nothing any more")
	}
	m.catchUp()
	m.checkAll(true)
	m.logf("converged after %d ticks: %d payments, %d transfers", m.ticks, len(m.payments), len(m.sent))
}

func (m *coreModel) catchUp() {
	// Past the finalised block, too: a transfer is final only at or below
	// it.
	m.mine(int(m.finality)+testSettle+testConfirmations+2, time.Second)
	tip := m.w.chain.Head().Number
	for i := 0; i < 60 && m.coreHead().Number < tip-testSettle; i++ {
		m.tick()
		m.w.passCooldowns()
	}
	if got := m.coreHead().Number; got < tip-testSettle {
		st := m.g.w.Status()
		m.t.Fatalf("the core did not catch up: head %d, chain %d; watcher %+v", got, tip, st)
	}
}
