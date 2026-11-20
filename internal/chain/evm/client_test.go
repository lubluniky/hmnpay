package evm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

type pool struct {
	chain  *evmtest.Chain
	nodes  []*evmtest.Node
	urls   []string
	client *evm.Client
	clock  *fakeClock
	logs   *bytes.Buffer
}

func newPool(t *testing.T, n int) *pool {
	t.Helper()
	p := &pool{chain: evmtest.NewChain(56, 1000), clock: newFakeClock(), logs: &bytes.Buffer{}}
	p.chain.AddToken(usdt, "USDT", 18)
	p.chain.Mine(100)
	for i := 0; i < n; i++ {
		node := evmtest.NewNode(p.chain)
		p.nodes = append(p.nodes, node)
		p.urls = append(p.urls, node.Serve(t))
	}
	client, err := evm.NewClient(evm.Config{
		ChainID:   56,
		Endpoints: p.urls,
		Logger:    slog.New(slog.NewTextHandler(p.logs, nil)),
		Endpoint:  evm.EndpointOptions{Now: p.clock.Now, Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.client = client
	return p
}

// pick returns the index of the endpoint Pick hands out.
func (p *pool) pick(t *testing.T, opts ...evm.PickOption) int {
	t.Helper()
	ep, err := p.client.Pick(context.Background(), opts...)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	for i, u := range p.urls {
		if ep.URL() == u {
			return i
		}
	}
	t.Fatalf("Pick returned unknown endpoint %s", ep.Label())
	return -1
}

func TestNewClientValidation(t *testing.T) {
	for name, cfg := range map[string]evm.Config{
		"no chain id":  {Endpoints: []string{"http://a"}},
		"no endpoints": {ChainID: 56},
		"bad url":      {ChainID: 56, Endpoints: []string{"ftp://a"}},
		"duplicate":    {ChainID: 56, Endpoints: []string{"http://a", "http://b", "http://a"}},
	} {
		if _, err := evm.NewClient(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c, err := evm.NewClient(evm.Config{ChainID: 56, Endpoints: []string{"http://a", "http://b"}})
	if err != nil || c.Len() != 2 || c.ChainID() != 56 {
		t.Fatalf("valid config: %v", err)
	}
}

func TestFailoverAndFailback(t *testing.T) {
	ctx := context.Background()
	p := newPool(t, 3)

	if got := p.pick(t); got != 0 {
		t.Fatalf("initial pick = %d, want primary", got)
	}
	// Primary goes down: the call fails over within the same Do.
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
	n, err := p.client.BlockNumber(ctx)
	if err != nil || n != 1100 {
		t.Fatalf("BlockNumber during outage = %d, %v", n, err)
	}
	if got := p.pick(t); got != 1 {
		t.Fatalf("pick during outage = %d, want 1", got)
	}
	st := p.client.Status()
	if st[0].State != evm.StateFailing || st[0].Healthy || st[0].ConsecutiveFailures != 1 || st[0].LastError == "" || st[0].RetryAt.IsZero() {
		t.Errorf("primary status = %+v", st[0])
	}
	if !st[1].Healthy || !st[1].Active || st[0].Active {
		t.Errorf("active flags wrong: %+v / %+v", st[0], st[1])
	}

	// Second goes down too.
	p.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
	if _, err := p.client.BlockNumber(ctx); err != nil {
		t.Fatal(err)
	}
	if got := p.pick(t); got != 2 {
		t.Fatalf("pick = %d, want 2", got)
	}

	// Primary recovers. Until its cooldown passes it is left alone...
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = false })
	if got := p.pick(t); got != 2 {
		t.Fatalf("pick during cooldown = %d, want 2", got)
	}
	// ...then it is used again (fail-back), without any monitor running.
	p.clock.Advance(3 * time.Second)
	if got := p.pick(t); got != 0 {
		t.Fatalf("pick after cooldown = %d, want 0 (fail-back)", got)
	}
	if _, err := p.client.BlockNumber(ctx); err != nil {
		t.Fatal(err)
	}
	if st := p.client.Status(); !st[0].Healthy || st[0].ConsecutiveFailures != 0 {
		t.Errorf("primary after recovery = %+v", st[0])
	}
	if !strings.Contains(p.logs.String(), "rpc endpoint switched") {
		t.Error("endpoint switch was not logged")
	}

}

// TestCooldownBackoff: consecutive failures back off exponentially up to a
// cap, and a single success clears the record.
func TestCooldownBackoff(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	chain := evmtest.NewChain(56, 1000)
	node := evmtest.NewNode(chain)
	node.Set(func(b *evmtest.Behavior) { b.Down = true })
	ep, _ := evm.NewEndpoint(node.Serve(t), evm.EndpointOptions{Now: clock.Now})

	var got []time.Duration
	for i := 0; i < 8; i++ {
		if _, err := ep.BlockNumber(ctx); err == nil {
			t.Fatal("expected failure")
		}
		h := ep.Health()
		if h.ConsecutiveFailures != i+1 {
			t.Fatalf("failures = %d, want %d", h.ConsecutiveFailures, i+1)
		}
		cooldown := h.CooldownUntil.Sub(clock.Now())
		got = append(got, cooldown)
		clock.Advance(cooldown)
	}
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("cooldowns = %v, want %v seconds", got, want)
		}
	}
	node.Set(func(b *evmtest.Behavior) { b.Down = false })
	if _, err := ep.BlockNumber(ctx); err != nil {
		t.Fatal(err)
	}
	if h := ep.Health(); h.ConsecutiveFailures != 0 || !h.CooldownUntil.IsZero() || h.LastError == "" {
		t.Errorf("health after success = %+v (LastError is kept for display)", h)
	}
	// A fault reported by a cross-checking caller counts like a failure.
	ep.ReportFault("logs missing from a canonical block")
	if h := ep.Health(); h.ConsecutiveFailures != 1 || !strings.Contains(h.LastError, "logs missing") {
		t.Errorf("health after ReportFault = %+v", h)
	}
}

func TestAllEndpointsDown(t *testing.T) {
	ctx := context.Background()
	p := newPool(t, 2)
	p.pick(t) // verify chain ids while healthy
	p.client.Refresh(ctx)
	for _, n := range p.nodes {
		n.Set(func(b *evmtest.Behavior) { b.Down = true })
	}
	_, err := p.client.BlockNumber(ctx)
	if !evm.IsKind(err, evm.KindHTTP) {
		t.Fatalf("err = %v", err)
	}
	// Everything is cooling down, but Pick still returns something to try
	// (the one due back first) rather than giving up for good.
	if _, err := p.client.Pick(ctx); err != nil {
		t.Fatalf("Pick with all endpoints cooling down: %v", err)
	}
	// With OnlyHealthy it refuses.
	if _, err := p.client.Pick(ctx, evm.OnlyHealthy()); !errors.Is(err, evm.ErrNoEndpoint) {
		t.Fatalf("OnlyHealthy: %v", err)
	}
	// Recovery.
	p.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
	p.clock.Advance(time.Minute)
	if n, err := p.client.BlockNumber(ctx); err != nil || n != 1100 {
		t.Fatalf("after recovery: %d %v", n, err)
	}
}

func TestWrongChainIsNeverUsed(t *testing.T) {
	ctx := context.Background()
	p := newPool(t, 2)
	// The primary is a testnet node (chain id 97).
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.ChainID = 97 })

	if got := p.pick(t); got != 1 {
		t.Fatalf("pick = %d, want 1", got)
	}
	if p.nodes[0].Calls("eth_blockNumber")+p.nodes[0].Calls("eth_getLogs") != 0 {
		t.Fatal("data was requested from a wrong-chain endpoint")
	}
	st := p.client.Status()
	if st[0].State != evm.StateWrongChain || st[0].Healthy || st[0].ChainID != 97 || !strings.Contains(st[0].LastError, "97") {
		t.Errorf("status = %+v", st[0])
	}
	if !strings.Contains(p.logs.String(), "wrong chain") {
		t.Error("wrong chain was not logged")
	}

	// Even if it is the only one left, it is not used.
	p.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
	if _, err := p.client.BlockNumber(ctx); err == nil {
		t.Fatal("call succeeded with only a wrong-chain endpoint alive")
	}
	p.clock.Advance(time.Minute)
	_, err := p.client.Pick(ctx)
	if err == nil {
		// The pick may return the failing endpoint 1 for a retry; it must
		// never be endpoint 0.
		if got := p.pick(t); got == 0 {
			t.Fatal("wrong-chain endpoint handed out")
		}
	}
	before := p.nodes[0].Calls("eth_blockNumber")
	_, _ = p.client.BlockNumber(ctx)
	if p.nodes[0].Calls("eth_blockNumber") != before {
		t.Fatal("wrong-chain endpoint was queried")
	}

	// All endpoints on the wrong chain: ErrNoEndpoint.
	p.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false; b.ChainID = 97 })
	p.clock.Advance(time.Minute)
	p.client.Refresh(ctx)
	if _, err := p.client.Pick(ctx); !errors.Is(err, evm.ErrNoEndpoint) {
		t.Fatalf("all wrong chain: %v", err)
	}

	// An endpoint that starts out right and later switches networks is
	// dropped at the next check, and re-admitted when it is fixed.
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.ChainID = 0 })
	p.client.Refresh(ctx)
	if got := p.pick(t); got != 0 {
		t.Fatalf("fixed endpoint not re-admitted: pick = %d", got)
	}
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.ChainID = 1 })
	p.clock.Advance(11 * time.Minute) // past the re-verification interval, no monitor
	if _, err := p.client.Pick(ctx); !errors.Is(err, evm.ErrNoEndpoint) {
		t.Fatalf("endpoint that switched chains is still used: %v", err)
	}
}

func TestLaggingEndpointIsSkipped(t *testing.T) {
	ctx := context.Background()
	p := newPool(t, 3)
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.Lag = 50 })

	// Without a measurement nobody knows the primary lags.
	if got := p.pick(t); got != 0 {
		t.Fatalf("pick before refresh = %d", got)
	}
	p.client.Refresh(ctx)
	if got := p.pick(t); got != 1 {
		t.Fatalf("pick after refresh = %d, want 1 (primary lags 50 blocks behind two others)", got)
	}
	st := p.client.Status()
	if st[0].State != evm.StateLagging || st[0].Lag != 50 || !st[0].LagKnown || st[0].BlockNumber != 1050 {
		t.Errorf("status = %+v", st[0])
	}
	if st[1].Lag != 0 || !st[1].Healthy || st[1].BlockNumber != 1100 || st[1].Latency <= 0 {
		t.Errorf("status = %+v", st[1])
	}
	// A small lag is tolerated.
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.Lag = 5 })
	p.client.Refresh(ctx)
	if got := p.pick(t); got != 0 {
		t.Fatalf("pick with small lag = %d, want 0", got)
	}
	// A lagging endpoint is still better than none.
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.Lag = 50 })
	p.client.Refresh(ctx)
	p.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = true })
	p.nodes[2].Set(func(b *evmtest.Behavior) { b.Down = true })
	p.client.Refresh(ctx)
	if got := p.pick(t); got != 0 {
		t.Fatalf("pick with the only alternatives down = %d, want 0", got)
	}
}

// The height lag is measured against is one that two endpoints have
// reached, not the highest number any single endpoint reports.
func TestLagIsNotMeasuredAgainstOneEndpointsClaim(t *testing.T) {
	ctx := context.Background()
	inflate := func(by uint64) func(b *evmtest.Behavior) {
		return func(b *evmtest.Behavior) {
			b.Intercept = func(method string, _ []json.RawMessage) (any, *evmtest.RPCError, bool) {
				if method == "eth_blockNumber" {
					return fmt.Sprintf("0x%x", 1100+by), nil, true
				}
				return nil, nil, false
			}
		}
	}
	for _, liar := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("three endpoints, endpoint %d adds a thousand to its block number", liar), func(t *testing.T) {
			p := newPool(t, 3)
			p.nodes[liar].Set(inflate(1000))
			p.client.Refresh(ctx)
			for i, st := range p.client.Status() {
				if st.State != evm.StateHealthy || st.Lag != 0 || !st.LagKnown {
					t.Errorf("endpoint %d: state %s, lag %d: one endpoint's block number made it lag", i, st.State, st.Lag)
				}
			}
			if got := p.pick(t); got != 0 {
				t.Fatalf("pick = %d, want 0: the configured order stands", got)
			}
		})
	}
	t.Run("two endpoints: a large gap makes neither lagging", func(t *testing.T) {
		for _, liar := range []int{0, 1} {
			p := newPool(t, 2)
			p.nodes[liar].Set(inflate(1000))
			p.client.Refresh(ctx)
			for i, st := range p.client.Status() {
				if st.State != evm.StateHealthy || st.Lag != 0 {
					t.Errorf("liar %d, endpoint %d: state %s, lag %d", liar, i, st.State, st.Lag)
				}
			}
			if got := p.pick(t); got != 0 {
				t.Fatalf("liar %d: pick = %d, want 0", liar, got)
			}
			// The same when one of them is honestly far behind: with two
			// there is no telling which.
			p.nodes[liar].Set(func(b *evmtest.Behavior) { b.Intercept = nil; b.Lag = 400 })
			p.client.Refresh(ctx)
			if st := p.client.Status(); st[0].State != evm.StateHealthy || st[1].State != evm.StateHealthy {
				t.Errorf("an endpoint 400 blocks behind the only other one: %s, %s", st[0].State, st[1].State)
			}
		}
	})
	t.Run("three endpoints: the one behind the other two lags, whatever the third claims", func(t *testing.T) {
		p := newPool(t, 3)
		p.nodes[0].Set(func(b *evmtest.Behavior) { b.Lag = 50 })
		p.nodes[2].Set(inflate(100000))
		p.client.Refresh(ctx)
		st := p.client.Status()
		if st[0].State != evm.StateLagging || st[0].Lag != 50 {
			t.Errorf("endpoint 0: state %s, lag %d; want lagging by 50 (behind the height two endpoints reach)", st[0].State, st[0].Lag)
		}
		if st[1].State != evm.StateHealthy || st[2].State != evm.StateHealthy {
			t.Errorf("endpoints 1 and 2: %s, %s", st[1].State, st[2].State)
		}
		// Health.Height is what an endpoint says now; BlockNumber the most
		// it ever said.
		p.nodes[2].Set(func(b *evmtest.Behavior) { b.Intercept = nil })
		p.client.Refresh(ctx)
		if st := p.client.Status(); st[2].BlockNumber != 101100 {
			t.Errorf("highest block reported by endpoint 2 = %d", st[2].BlockNumber)
		}
	})
}

// OthersUnreachable: only an endpoint that does not answer at all counts.
func TestOthersUnreachable(t *testing.T) {
	ctx := context.Background()
	p := newPool(t, 3)
	p.client.Refresh(ctx)
	first, err := p.client.Pick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.client.OthersUnreachable(first) {
		t.Fatal("healthy endpoints reported unreachable")
	}
	// Answers of every unhelpful kind: a rate limit, another chain, a wrong
	// height. None of them is "unreachable".
	p.nodes[1].Set(func(b *evmtest.Behavior) { b.RateLimited = true })
	p.nodes[2].Set(func(b *evmtest.Behavior) { b.ChainID = 97 })
	p.client.Refresh(ctx)
	if p.client.OthersUnreachable(first) {
		t.Fatal("a throttling endpoint and one on another chain count as unreachable")
	}
	p.nodes[2].Set(func(b *evmtest.Behavior) { b.ChainID = 0; b.Down = true })
	p.client.Refresh(ctx)
	if p.client.OthersUnreachable(first) {
		t.Fatal("one endpoint is down, the other throttles: not all are unreachable")
	}
	p.nodes[1].Set(func(b *evmtest.Behavior) { b.RateLimited = false; b.Down = true })
	p.client.Refresh(ctx)
	if !p.client.OthersUnreachable(first) {
		t.Fatal("both other endpoints are down")
	}
	st := p.client.Status()
	if st[0].Unreachable || !st[1].Unreachable || !st[2].Unreachable {
		t.Fatalf("status: %v %v %v", st[0].Unreachable, st[1].Unreachable, st[2].Unreachable)
	}
	// A fault report is about an answer.
	second, _ := p.client.Pick(ctx, evm.Except(first))
	second.ReportFault("lied")
	if p.client.OthersUnreachable(first) {
		t.Fatal("an endpoint that was caught lying counts as unreachable")
	}
	// Back up: reachable at once.
	p.nodes[1].Set(func(b *evmtest.Behavior) { b.Down = false })
	p.nodes[2].Set(func(b *evmtest.Behavior) { b.Down = false })
	p.client.Refresh(ctx)
	if p.client.OthersUnreachable(first) {
		t.Fatal("endpoints that answer again are still unreachable")
	}
	// No other endpoint at all is not "every other endpoint is down".
	single := newPool(t, 1)
	only, _ := single.client.Pick(ctx)
	if single.client.OthersUnreachable(only) {
		t.Fatal("a single endpoint has no others")
	}
}

func TestPickOptions(t *testing.T) {
	ctx := context.Background()
	p := newPool(t, 3)
	// The primary does not serve logs (like bsc-dataseed).
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"} })
	first, _ := p.client.Pick(ctx, evm.NeedLogs())
	if first.URL() != p.urls[0] {
		t.Fatal("nothing known yet: primary expected")
	}
	if _, err := scanAll(ctx, first, evm.TransferQuery{Token: usdt, FromBlock: 1090, ToBlock: 1100, Recipients: []ethaddr.Address{addr(1)}}); err == nil {
		t.Fatal("expected the log query to be refused")
	}
	if got := p.pick(t, evm.NeedLogs()); got != 1 {
		t.Fatalf("NeedLogs pick = %d, want 1", got)
	}
	// For everything else the primary is still first choice.
	if got := p.pick(t); got != 0 {
		t.Fatalf("plain pick = %d, want 0", got)
	}
	st := p.client.Status()
	if !st[0].Healthy || !st[0].Logs.Unavailable || st[0].Logs.LastError == "" {
		t.Errorf("status = %+v", st[0])
	}
	// If no endpoint serves logs, NeedLogs degrades to "any" rather than
	// failing.
	for _, n := range p.nodes[1:] {
		n.Set(func(b *evmtest.Behavior) { b.Down = true })
	}
	p.client.Refresh(ctx)
	if got := p.pick(t, evm.NeedLogs()); got != 0 {
		t.Fatalf("NeedLogs with no alternative = %d", got)
	}
	for _, n := range p.nodes[1:] {
		n.Set(func(b *evmtest.Behavior) { b.Down = false })
	}
	p.clock.Advance(time.Minute)

	// Except yields an independent endpoint, or ErrNoEndpoint.
	a, _ := p.client.Pick(ctx)
	b, err := p.client.Pick(ctx, evm.Except(a))
	if err != nil || b == a {
		t.Fatalf("Except: %v", err)
	}
	c, err := p.client.Pick(ctx, evm.Except(a, b))
	if err != nil || c == a || c == b {
		t.Fatalf("Except two: %v", err)
	}
	if _, err := p.client.Pick(ctx, evm.Except(a, b, c)); !errors.Is(err, evm.ErrNoEndpoint) {
		t.Fatalf("Except all: %v", err)
	}
}

func TestDoRetriesOnlyEndpointFaults(t *testing.T) {
	ctx := context.Background()
	p := newPool(t, 2)

	// A reverted call is not retried on the second endpoint.
	calls := 0
	doErr := p.client.Do(ctx, func(ctx context.Context, e *evm.Endpoint) error {
		calls++
		_, err := e.Call(ctx, usdt, []byte{0xde, 0xad, 0xbe, 0xef})
		return err
	})
	if !evm.IsKind(doErr, evm.KindRPC) || calls != 1 {
		t.Fatalf("revert: err = %v after %d attempts", doErr, calls)
	}
	// ErrNotFound is an answer, not a fault.
	_, rerr := p.client.TransactionReceipt(ctx, evm.Hash{1})
	if !errors.Is(rerr, evm.ErrNotFound) {
		t.Fatalf("receipt: %v", rerr)
	}
	// A rate-limited primary is skipped and cooled down for Retry-After.
	p.nodes[0].ResetCounters()
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.RateLimited = true; b.RetryAfter = 45 })
	if d, err := p.client.Decimals(ctx, usdt); err != nil || d != 18 {
		t.Fatalf("decimals = %d, %v", d, err)
	}
	if s, err := p.client.Symbol(ctx, usdt); err != nil || s != "USDT" {
		t.Fatalf("symbol = %q, %v", s, err)
	}
	if b, err := p.client.BalanceOf(ctx, usdt, addr(1)); err != nil || b.Sign() != 0 {
		t.Fatalf("balance = %v, %v", b, err)
	}
	st := p.client.Status()[0]
	if got := st.RetryAt.Sub(p.clock.Now()); got != 45*time.Second {
		t.Errorf("cooldown = %v, want the provider's Retry-After (45s)", got)
	}
	if p.nodes[0].Requests() != 1 {
		t.Errorf("rate-limited endpoint was hit %d times", p.nodes[0].Requests())
	}
	// Caller cancellation stops the failover loop.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.client.BlockNumber(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestMonitor(t *testing.T) {
	p := newPool(t, 2)
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.client.Monitor(ctx, 10*time.Millisecond) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		st := p.client.Status()
		if st[0].State == evm.StateFailing && st[1].Healthy && st[1].LagKnown {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("monitor did not converge: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Fail-back is driven by the monitor even while the endpoint is in
	// cooldown (the fake clock never advances here).
	p.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = false })
	for {
		if st := p.client.Status(); st[0].Healthy && st[0].Active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no fail-back: %+v", p.client.Status()[0])
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Monitor returned %v", err)
	}
}

func TestProbe(t *testing.T) {
	ctx := context.Background()
	chain := evmtest.NewChain(56, 5000)
	chain.AddToken(usdt, "USDT", 18)
	chain.Mine(50)
	opts := evm.ProbeOptions{
		Endpoint: evm.EndpointOptions{Timeout: 2 * time.Second},
		Now:      func() time.Time { return time.Unix(int64(chain.Head().Time)+4, 0) },
	}

	t.Run("ok", func(t *testing.T) {
		node := evmtest.NewNode(chain)
		r := evm.ProbeWith(ctx, node.Serve(t), 56, usdt, opts)
		if r.Verdict != evm.ProbeOK {
			t.Fatalf("verdict = %s: %+v", r.Verdict, r)
		}
		if !r.Reach.OK || r.Reach.Latency <= 0 || !r.Chain.Known || !r.Chain.Matches || r.Chain.ID != 56 {
			t.Errorf("reach/chain: %+v %+v", r.Reach, r.Chain)
		}
		if !r.Head.OK || r.Head.Number != 5050 || r.Head.Age != 4*time.Second {
			t.Errorf("head: %+v", r.Head)
		}
		if !r.Token.Checked || !r.Token.OK || !r.Token.CodePresent || r.Token.Symbol != "USDT" || r.Token.Decimals != 18 {
			t.Errorf("token: %+v", r.Token)
		}
		if !r.Logs.Checked || !r.Logs.OK || r.Logs.Blocks != 5 {
			t.Errorf("logs: %+v", r.Logs)
		}
		// The log probe stays within a small range.
		for _, q := range node.LogQueries() {
			if q.To-q.From+1 > 5 {
				t.Errorf("probe queried %d blocks", q.To-q.From+1)
			}
		}
	})

	t.Run("cannot connect", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		r := evm.ProbeWith(ctx, url, 56, usdt, opts)
		if r.Verdict != evm.ProbeUnreachable || r.Reach.OK || r.Reach.Kind != evm.KindTransport || r.Reach.Error == "" {
			t.Fatalf("%+v", r)
		}
		if r.Chain.Known || r.Head.OK || r.Token.Checked || r.Logs.Checked {
			t.Errorf("later steps ran: %+v", r)
		}
	})

	t.Run("not an rpc endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html>welcome</html>"))
		}))
		defer srv.Close()
		r := evm.ProbeWith(ctx, srv.URL, 56, usdt, opts)
		if r.Verdict != evm.ProbeUnreachable || r.Reach.Kind != evm.KindBadResponse {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("api key required", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"Unauthorized: You must authenticate your request with an API key."},"id":null}`))
		}))
		defer srv.Close()
		r := evm.ProbeWith(ctx, srv.URL, 56, usdt, opts)
		if r.Verdict != evm.ProbeUnreachable || r.Reach.Kind != evm.KindUnavailable || !strings.Contains(r.Reach.Error, "API key") {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("invalid url", func(t *testing.T) {
		r := evm.ProbeWith(ctx, "ftp://example.com", 56, usdt, opts)
		if r.Verdict != evm.ProbeUnreachable || r.Reach.OK || r.Reach.Error == "" {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("wrong network", func(t *testing.T) {
		node := evmtest.NewNode(chain)
		node.Set(func(b *evmtest.Behavior) { b.ChainID = 97 })
		r := evm.ProbeWith(ctx, node.Serve(t), 56, usdt, opts)
		if r.Verdict != evm.ProbeWrongChain || !r.Reach.OK || !r.Chain.Known || r.Chain.Matches || r.Chain.ID != 97 || r.Chain.Expected != 56 {
			t.Fatalf("%+v", r)
		}
		if r.Head.OK || r.Token.Checked || r.Logs.Checked {
			t.Errorf("later steps ran against the wrong network: %+v", r)
		}
		if node.Calls("eth_getLogs")+node.Calls("eth_call") != 0 {
			t.Error("wrong-network node was queried further")
		}
	})

	t.Run("works but no log access", func(t *testing.T) {
		for _, e := range []evmtest.RPCError{
			{Code: -32005, Message: "limit exceeded"},
			{Code: -32000, Message: "The method eth_getLogs is not supported."},
			{Code: -32012, Message: `Method "eth_getLogs" is not available for unregistered accounts.`},
		} {
			node := evmtest.NewNode(chain)
			node.Set(func(b *evmtest.Behavior) { b.LogsError = &e })
			r := evm.ProbeWith(ctx, node.Serve(t), 56, usdt, opts)
			if r.Verdict != evm.ProbeNoLogs || !r.Reach.OK || !r.Chain.Matches || !r.Token.OK || r.Logs.OK || !r.Logs.Checked {
				t.Fatalf("%+v", r)
			}
			if !strings.Contains(r.Logs.Error, e.Message[:10]) {
				t.Errorf("logs error = %q", r.Logs.Error)
			}
		}
	})

	t.Run("token missing", func(t *testing.T) {
		node := evmtest.NewNode(chain)
		r := evm.ProbeWith(ctx, node.Serve(t), 56, addr(77), opts)
		if r.Verdict != evm.ProbeTokenMissing || r.Token.CodePresent || r.Token.OK || r.Token.Error == "" {
			t.Fatalf("%+v", r)
		}
		// The log check is still reported, independently.
		if !r.Logs.OK {
			t.Errorf("logs: %+v", r.Logs)
		}
	})

	t.Run("no token requested", func(t *testing.T) {
		node := evmtest.NewNode(chain)
		r := evm.Probe(ctx, node.Serve(t), 56, ethaddr.Address{})
		if r.Verdict != evm.ProbeOK || r.Token.Checked {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("range ladder", func(t *testing.T) {
		node := evmtest.NewNode(chain)
		node.Set(func(b *evmtest.Behavior) { b.MaxLogRange = 30 })
		o := opts
		o.LogRanges = []uint64{10, 25, 40, 50}
		r := evm.ProbeWith(ctx, node.Serve(t), 56, usdt, o)
		if r.Verdict != evm.ProbeOK || r.Logs.LargestRangeOK != 25 || r.Logs.SmallestRangeFailed != 40 || r.Logs.Kind != evm.KindRangeTooLarge {
			t.Fatalf("%+v", r.Logs)
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		node := evmtest.NewNode(chain)
		node.Set(func(b *evmtest.Behavior) { b.RateLimited = true })
		r := evm.ProbeWith(ctx, node.Serve(t), 56, usdt, opts)
		if r.Verdict != evm.ProbeUnreachable || r.Reach.Kind != evm.KindRateLimited {
			t.Fatalf("%+v", r)
		}
	})
}

// TestClientRefusesTwoURLsOfOneEndpoint: the pool counts endpoints as
// opinions; two URLs that differ only in their path or query are one.
func TestClientRefusesTwoURLsOfOneEndpoint(t *testing.T) {
	_, err := evm.NewClient(evm.Config{ChainID: 56, Endpoints: []string{"https://bsc.example.org/v1/a", "https://BSC.example.org:443/v1/b"}})
	if err == nil || !strings.Contains(err.Error(), "same endpoint") {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := evm.NewClient(evm.Config{ChainID: 56, Endpoints: []string{"http://127.0.0.1:8545", "http://127.0.0.1:8546"}}); err != nil {
		t.Fatal(err)
	}
}

// TestPickAsksAgainAnEndpointWhoseLogPauseIsOver: both endpoints are held
// to have no log service — the first truly has none, the second refused
// while it was throttled. The first refused again a moment ago and is in
// its pause; the second's pause is over. The endpoint in use is the second:
// it is due to be asked again, and a tick that started from the first would
// fail before anybody else was asked — every tick, for as long as the first
// stays first.
func TestPickAsksAgainAnEndpointWhoseLogPauseIsOver(t *testing.T) {
	ctx := context.Background()
	p := newPool(t, 2)
	q := evm.TransferQuery{Token: usdt, FromBlock: 1090, ToBlock: 1100, Recipients: []ethaddr.Address{addr(1)}}
	refuse := func(b *evmtest.Behavior) {
		b.LogsError = &evmtest.RPCError{Code: -32601, Message: "the method eth_getLogs does not exist/is not available"}
	}
	for i, n := range p.nodes {
		n.Set(refuse)
		if _, err := scanAll(ctx, p.client.Endpoints()[i], q); err == nil {
			t.Fatal("test setup: logs served")
		}
		p.client.Endpoints()[i].MarkLogsAbsent()
	}
	p.nodes[1].Set(func(b *evmtest.Behavior) { b.LogsError = nil }) // it has logs again
	p.clock.Advance(time.Hour)                                      // every pause is over
	if _, err := scanAll(ctx, p.client.Endpoints()[0], q); err == nil {
		t.Fatal("test setup: the first endpoint served logs")
	}
	st := p.client.Status()
	if !st[0].Logs.NoService || !st[0].Logs.Unavailable || !st[1].Logs.NoService || st[1].Logs.Unavailable {
		t.Fatalf("test setup: %+v / %+v", st[0].Logs, st[1].Logs)
	}
	if got := p.pick(t, evm.NeedLogs()); got != 1 {
		t.Fatalf("NeedLogs pick = %d, want the endpoint whose pause is over (1)", got)
	}
}
