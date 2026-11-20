package watcher_test

// End-to-end reality check: the watcher against real public BSC endpoints.
//
// Skipped unless HMNPAY_LIVE_RPC=1:
//
//	HMNPAY_LIVE_RPC=1 go test ./internal/chain/watcher -run Live -v -count=1
//
// HMNPAY_LIVE_ENDPOINTS (comma-separated) overrides the endpoint list and
// HMNPAY_LIVE_SECONDS the duration (default 30). Read-only, public data.

import (
	"context"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

func TestLiveWatcher(t *testing.T) {
	if os.Getenv("HMNPAY_LIVE_RPC") != "1" {
		t.Skip("network test; set HMNPAY_LIVE_RPC=1 to run")
	}
	urls := evm.BSCMainnetPublicEndpoints
	if env := os.Getenv("HMNPAY_LIVE_ENDPOINTS"); env != "" {
		urls = strings.Split(env, ",")
	}
	seconds := 30
	if env := os.Getenv("HMNPAY_LIVE_SECONDS"); env != "" {
		seconds, _ = strconv.Atoi(env)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds+60)*time.Second)
	defer cancel()

	counter := &countingTransport{byHost: map[string]int{}}
	client, err := evm.NewClient(evm.Config{ChainID: evm.BSCMainnetChainID, Endpoints: urls,
		Endpoint: evm.EndpointOptions{HTTPClient: &http.Client{Transport: counter, Timeout: 15 * time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	client.Refresh(ctx)

	// Watch addresses that are receiving USDT right now: the recipients of
	// the transfers in two recent blocks.
	watch := newWatchSet()
	err = client.Do(ctx, func(ctx context.Context, e *evm.Endpoint) error {
		n, err := e.BlockNumber(ctx)
		if err != nil {
			return err
		}
		logs, err := e.Logs(ctx, evm.LogFilter{FromBlock: n - 12, ToBlock: n - 10, Address: evm.USDTBSCMainnet, Topics: [][]evm.Hash{{evm.TransferTopic}}})
		if err != nil {
			return err
		}
		seen := map[ethaddr.Address]bool{}
		for _, l := range logs {
			if tr, err := evm.DecodeTransfer(l); err == nil && !seen[tr.To] && len(seen) < 60 {
				seen[tr.To] = true
				watch.add(tr.To)
			}
		}
		t.Logf("%d USDT transfers in 3 blocks via %s; watching %d recipients", len(logs), e.Label(), len(seen))
		return nil
	}, evm.NeedLogs())
	if err != nil {
		t.Fatalf("could not discover recipients: %v", err)
	}

	sk := newSink(t)
	// How old a block is when its transfers are reported, and how old the
	// processed head is when it is reported (block timestamps have a
	// resolution of one second).
	var mu sync.Mutex
	var headAge []time.Duration
	var in incidents
	timed := &timedSink{sink: sk, onHead: func(h watcher.Head) {
		mu.Lock()
		defer mu.Unlock()
		headAge = append(headAge, time.Since(h.Time))
	}}
	w, err := watcher.New(watcher.Config{
		Client:     client,
		Token:      evm.USDTBSCMainnet,
		Sink:       timed,
		Watch:      watch,
		Store:      &store{},
		OnIncident: in.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	counter.reset()

	runCtx, stop := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer stop()
	go func() { _ = client.Monitor(runCtx, 0) }()
	started := time.Now()
	_ = w.Run(runCtx)
	elapsed := time.Since(started)

	st := w.Status()
	requests, perHost := counter.snapshot()
	ticks := max(int(elapsed/watcher.DefaultPollInterval), 1)
	t.Logf("requests: %d HTTP requests in %s (%.1f per tick at the default %s poll interval, %.2f per second)",
		requests, elapsed.Round(time.Second), float64(requests)/float64(ticks), watcher.DefaultPollInterval, float64(requests)/elapsed.Seconds())
	for host, n := range perHost {
		t.Logf("  %-40s %d requests (%.1f per tick)", host, n, float64(n)/float64(ticks))
	}
	t.Logf("cross-checks: verifier=%s log verifier=%s logs unchecked=%v hold=%q clock skew=%s incidents=%d",
		st.Verifier, st.LogVerifier, st.LogsUnchecked, st.Hold, st.ClockSkew.Round(time.Millisecond), len(in.list))
	for _, i := range in.list {
		t.Logf("  incident %s endpoint=%s block=%d: %s", i.Kind, i.Endpoint, i.Block, i.Detail)
	}
	mu.Lock()
	if len(headAge) > 0 {
		sort.Slice(headAge, func(i, j int) bool { return headAge[i] < headAge[j] })
		t.Logf("age of the processed head when reported: median %s, max %s (%d heads; SettleDepth %d blocks)",
			headAge[len(headAge)/2].Round(100*time.Millisecond), headAge[len(headAge)-1].Round(100*time.Millisecond), len(headAge), watcher.DefaultSettleDepth)
	}
	mu.Unlock()
	events := sk.take()
	var heads, observed, reverted int
	var firstHead, lastHead watcher.Head
	for _, e := range events {
		switch e.kind {
		case "head":
			if heads == 0 {
				firstHead = e.head
			}
			lastHead = e.head
			heads++
		case "observed":
			observed++
			if e.tr.Token != evm.USDTBSCMainnet || !watch.has(e.tr.To) || e.tr.Amount.Sign() <= 0 {
				t.Errorf("bad transfer fact: %+v", e.tr)
			}
		case "reverted":
			reverted++
		}
	}
	t.Logf("ran %s: %d heads (%d -> %d, finalized %d), %d transfers observed, %d reverted",
		elapsed.Round(time.Second), heads, firstHead.Number, lastHead.Number, lastHead.Finalized, observed, reverted)
	t.Logf("status: cursor=%d head=%d behind=%d endpoint=%s tracked=%d unverified=%d reorgs=%d errors_in_a_row=%d last_error=%q",
		st.Cursor, st.Tip.Number, st.Behind, st.Endpoint, st.Tracked, st.Unverified, st.Reorgs, st.ConsecutiveErrors, st.LastError)
	for _, es := range client.Status() {
		t.Logf("endpoint %-36s %-10s block=%d lag=%d latency=%-6s logs(unavailable=%v range=%d) err=%q",
			es.URL, es.State, es.BlockNumber, es.Lag, es.Latency.Round(time.Millisecond), es.Logs.Unavailable, es.Logs.Range, es.LastError)
	}
	if heads < 2 {
		t.Errorf("only %d heads in %s", heads, elapsed)
	}
	if lastHead.Number <= firstHead.Number {
		t.Errorf("head did not advance: %d -> %d", firstHead.Number, lastHead.Number)
	}
	if blocks := lastHead.Number - firstHead.Number; heads > 1 {
		t.Logf("observed block time: %.3f s", lastHead.Time.Sub(firstHead.Time).Seconds()/float64(blocks))
	}
	if observed == 0 {
		t.Errorf("no transfer observed to %d addresses that were receiving USDT a moment ago", st.WatchedAddresses)
	}
}

// countingTransport counts HTTP requests per host.
type countingTransport struct {
	mu     sync.Mutex
	byHost map[string]int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.byHost[r.URL.Host]++
	c.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

func (c *countingTransport) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byHost = map[string]int{}
}

func (c *countingTransport) snapshot() (total int, byHost map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	byHost = make(map[string]int, len(c.byHost))
	for h, n := range c.byHost {
		byHost[h] = n
		total += n
	}
	return total, byHost
}

// timedSink is a sink that also tells when a head arrives.
type timedSink struct {
	*sink
	onHead func(watcher.Head)
}

func (s *timedSink) HeadAdvanced(ctx context.Context, h watcher.Head) error {
	s.onHead(h)
	return s.sink.HeadAdvanced(ctx, h)
}
