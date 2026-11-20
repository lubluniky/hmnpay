package corebridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

// TestProbeThroughCore: the setup wizard's "test this endpoint" button, from
// the core's ProbeRPC down to a (scripted) node and back.
func TestProbeThroughCore(t *testing.T) {
	// A fresh, not yet set up instance: the call is unauthenticated.
	w := newWorld(t, withoutSetup())
	ctx := context.Background()
	probe := func(url string) core.ProbeResult {
		t.Helper()
		res, err := w.env.Service.ProbeRPC(ctx, core.ProbeRPCInput{URL: url})
		if err != nil {
			t.Fatalf("ProbeRPC(%s): %v", url, err)
		}
		return res
	}

	res := probe(w.urls[0])
	if res.Outcome != core.ProbeOK {
		t.Fatalf("outcome = %s: %+v", res.Outcome, res.Facts)
	}
	f := res.Facts
	if !f.Reachable || f.ChainID != 56 || f.BlockNumber != chainStart || f.LatencyMs < 1 ||
		!f.TokenFound || f.TokenDecimals != 18 || f.TokenSymbol != "USDT" || f.Error != "" {
		t.Fatalf("facts = %+v", f)
	}

	// Another network.
	testnet := evmtest.NewNode(evmtest.NewChain(core.ChainBSCTestnet, 77))
	res = probe(testnet.Serve(t))
	if res.Outcome != core.ProbeWrongChain || !res.Facts.Reachable || res.Facts.ChainID != 97 {
		t.Fatalf("wrong chain: %s %+v", res.Outcome, res.Facts)
	}
	if testnet.Calls("eth_call")+testnet.Calls("eth_getLogs") != 0 {
		t.Error("a node of another network was asked about the token")
	}

	// Right network, but the token is not there (a node that is not
	// synced, or a wrong contract address).
	bare := evmtest.NewNode(evmtest.NewChain(core.ChainBSCMainnet, 1234))
	res = probe(bare.Serve(t))
	if res.Outcome != core.ProbeTokenMismatch || res.Facts.TokenFound || res.Facts.Error == "" || res.Facts.BlockNumber != 1234 {
		t.Fatalf("token missing: %s %+v", res.Outcome, res.Facts)
	}

	// Works, but serves no logs (bsc-dataseed style). The core has no
	// field for that; the fact travels in Error while the outcome is ok.
	nologs := evmtest.NewNode(w.chain)
	nologs.Set(func(b *evmtest.Behavior) {
		b.LogsBatchError = &evmtest.RPCError{Code: -32005, Message: "method eth_getLogs in batch triggered rate limit"}
		b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"}
	})
	res = probe(nologs.Serve(t))
	if res.Outcome != core.ProbeOK || !res.Facts.TokenFound || !strings.Contains(res.Facts.Error, "eth_getLogs") || !strings.Contains(res.Facts.Error, "limit exceeded") {
		t.Fatalf("no logs: %s %+v", res.Outcome, res.Facts)
	}

	// Nothing listening.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	res = probe(deadURL)
	if res.Outcome != core.ProbeUnreachable || res.Facts.Reachable || !strings.HasPrefix(res.Facts.Error, "cannot connect") {
		t.Fatalf("dead: %s %+v", res.Outcome, res.Facts)
	}

	// The core accepts websocket URLs; the client does not speak them.
	res = probe("wss://bsc.example.org/ws")
	if res.Outcome != core.ProbeUnreachable || !strings.Contains(res.Facts.Error, "websocket") {
		t.Fatalf("ws: %s %+v", res.Outcome, res.Facts)
	}

	// A throttling endpoint and one that wants an API key are told apart.
	limited := evmtest.NewNode(w.chain)
	limited.Set(func(b *evmtest.Behavior) { b.RateLimited = true })
	if res = probe(limited.Serve(t)); res.Outcome != core.ProbeUnreachable || !strings.Contains(res.Facts.Error, "rate-limiting") {
		t.Fatalf("rate limited: %+v", res.Facts)
	}
	keyed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Unauthorized: You must authenticate your request with an API key."},"id":null}`)
	}))
	defer keyed.Close()
	if res = probe(keyed.URL); res.Outcome != core.ProbeUnreachable || !strings.Contains(res.Facts.Error, "API key") {
		t.Fatalf("api key: %+v", res.Facts)
	}

	// After setup the probe is the operator's only.
	in := w.env.SetupInput()
	in.RPCURLs, in.XPub = w.urls, testXpub
	if _, err := w.env.Service.CompleteSetup(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := w.env.Service.ProbeRPC(ctx, core.ProbeRPCInput{URL: w.urls[0]}); !errors.Is(err, core.ErrUnauthorized) && !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("anonymous probe after setup: %v", err)
	}
}

// TestProbeIsBounded: what an anonymous caller can make a not-yet-set-up
// instance do with the probe.
func TestProbeIsBounded(t *testing.T) {
	ctx := context.Background()
	asset := core.USDTBSC
	const secret = "INTERNAL-SECRET-DO-NOT-LEAK"

	t.Run("no response content is echoed", func(t *testing.T) {
		for name, handler := range map[string]http.HandlerFunc{
			"html 200":  func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>"+secret+"</html>") },
			"html 403":  func(w http.ResponseWriter, r *http.Request) { http.Error(w, secret, http.StatusForbidden) },
			"html 500":  func(w http.ResponseWriter, r *http.Request) { http.Error(w, secret, http.StatusInternalServerError) },
			"json blob": func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"password":%q}`, secret) },
			"json array": func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `[{"jsonrpc":"2.0","id":1,"result":%q}]`, secret)
			},
			"result of the wrong type": func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"secret":%q}}`, secret)
			},
		} {
			srv := httptest.NewServer(handler)
			facts, err := NewProber(ProberOptions{}).Probe(ctx, srv.URL, asset)
			srv.Close()
			if err != nil || facts.Reachable {
				t.Errorf("%s: facts = %+v, err = %v", name, facts, err)
			}
			if s := fmt.Sprintf("%+v", facts); strings.Contains(s, secret) || strings.Contains(s, "INTERNAL") {
				t.Errorf("%s: response content leaked: %s", name, s)
			}
		}
	})

	t.Run("request is a fixed POST", func(t *testing.T) {
		var methods, bodies []string
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			mu.Lock()
			methods = append(methods, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Content-Type"))
			bodies = append(bodies, string(buf[:n]))
			mu.Unlock()
			http.Error(w, "no", http.StatusNotFound)
		}))
		defer srv.Close()
		if _, err := NewProber(ProberOptions{}).Probe(ctx, srv.URL+"/admin/delete?id=1", asset); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(methods) != 1 || methods[0] != "POST /admin/delete?id=1 application/json" {
			t.Fatalf("requests = %v", methods)
		}
		if !strings.Contains(bodies[0], `"method":"eth_chainId"`) || strings.Contains(bodies[0], "admin") {
			t.Fatalf("body = %s", bodies[0])
		}
	})

	t.Run("redirects are not followed", func(t *testing.T) {
		var hits atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		defer target.Close()
		src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/internal", http.StatusTemporaryRedirect)
		}))
		defer src.Close()
		facts, _ := NewProber(ProberOptions{}).Probe(ctx, src.URL, asset)
		if facts.Reachable || hits.Load() != 0 {
			t.Fatalf("facts = %+v, redirect target hits = %d", facts, hits.Load())
		}
	})

	t.Run("only http and https", func(t *testing.T) {
		for _, u := range []string{"file:///etc/passwd", "ftp://example.org", "gopher://127.0.0.1:6379/_INFO", "javascript:alert(1)", "//example.org", "example.org:8545", ""} {
			facts, err := NewProber(ProberOptions{}).Probe(ctx, u, asset)
			if err != nil || facts.Reachable || facts.Error == "" {
				t.Errorf("%q: facts = %+v, err = %v", u, facts, err)
			}
		}
	})

	t.Run("time and size", func(t *testing.T) {
		release := make(chan struct{})
		var requests atomic.Int32
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			<-release
		}))
		defer slow.Close()
		defer close(release) // runs first: lets the handler return so the server can close
		p := NewProber(ProberOptions{Timeout: 150 * time.Millisecond, RequestTimeout: 100 * time.Millisecond})
		start := time.Now()
		facts, err := p.Probe(ctx, slow.URL, asset)
		if err != nil || facts.Reachable || time.Since(start) > 2*time.Second {
			t.Fatalf("slow endpoint: facts = %+v, err = %v, took %s", facts, err, time.Since(start))
		}
		if requests.Load() != 1 {
			t.Fatalf("%d requests to an endpoint that never answered the first", requests.Load())
		}

		big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"0x%s"}`, strings.Repeat("00", 2<<20))
		}))
		defer big.Close()
		facts, err = NewProber(ProberOptions{MaxResponseBytes: 64 << 10}).Probe(ctx, big.URL, asset)
		if err != nil || facts.Reachable {
			t.Fatalf("oversized answer: facts = %+v, err = %v", facts, err)
		}
	})

	t.Run("concurrency", func(t *testing.T) {
		release := make(chan struct{})
		var inFlight atomic.Int32
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inFlight.Add(1)
			<-release
		}))
		defer slow.Close()
		p := NewProber(ProberOptions{Concurrency: 2, Timeout: 5 * time.Second, RequestTimeout: 5 * time.Second})
		var wg sync.WaitGroup
		results := make([]core.ProbeFacts, 6)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i >= 2 { // let the first two take the slots
					for inFlight.Load() < 2 {
						time.Sleep(time.Millisecond)
					}
				}
				results[i], _ = p.Probe(ctx, slow.URL, asset)
			}()
		}
		for inFlight.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		close(release)
		wg.Wait()
		busy := 0
		for _, f := range results {
			if strings.Contains(f.Error, "too many endpoint checks") {
				busy++
			}
		}
		if busy != 4 || inFlight.Load() != 2 {
			t.Fatalf("%d of 6 probes were turned away, %d reached the server; want 4 and 2", busy, inFlight.Load())
		}
	})

	t.Run("private networks can be refused", func(t *testing.T) {
		node := evmtest.NewNode(evmtest.NewChain(56, 100))
		url := node.Serve(t) // on 127.0.0.1
		facts, err := NewProber(ProberOptions{DenyPrivateNetworks: true}).Probe(ctx, url, asset)
		if err != nil || facts.Reachable || facts.Error != "this server does not probe private or local addresses" {
			t.Fatalf("facts = %+v, err = %v", facts, err)
		}
		if node.Requests() != 0 {
			t.Fatal("a request reached the local node")
		}
		// A name that resolves to a local address is refused as well (the
		// check is on the address dialled).
		facts, _ = NewProber(ProberOptions{DenyPrivateNetworks: true}).Probe(ctx, strings.Replace(url, "127.0.0.1", "localhost", 1), asset)
		if facts.Reachable || node.Requests() != 0 {
			t.Fatalf("localhost: facts = %+v", facts)
		}
		// Without the option a local node is a perfectly good endpoint.
		facts, _ = NewProber(ProberOptions{}).Probe(ctx, url, asset)
		if !facts.Reachable || facts.ChainID != 56 {
			t.Fatalf("default: facts = %+v", facts)
		}
		for addr, denied := range map[string]bool{
			"127.0.0.1:80": true, "10.1.2.3:8545": true, "172.16.0.1:80": true, "192.168.1.10:80": true,
			"169.254.169.254:80": true, "100.64.0.1:80": true, "0.0.0.0:80": true, "[::1]:80": true,
			"[fe80::1]:80": true, "[fc00::1]:80": true, "[::ffff:127.0.0.1]:80": true, "224.0.0.1:80": true,
			"8.8.8.8:443": false, "[2606:4700:4700::1111]:443": false, "not-an-address": true,
		} {
			err := denyPrivate("tcp", addr, syscall.RawConn(nil))
			if (err != nil) != denied {
				t.Errorf("denyPrivate(%s) = %v, want denied=%v", addr, err, denied)
			}
		}
	})

	t.Run("cancellation is the only error", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		node := evmtest.NewNode(evmtest.NewChain(56, 100))
		if _, err := NewProber(ProberOptions{}).Probe(cctx, node.Serve(t), asset); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestStatusReport: what the dashboard's node status is fed.
func TestStatusReport(t *testing.T) {
	w := newWorld(t, withNodes(2))
	// The operator's URLs carry provider keys in the path.
	keyed := []string{w.urls[0] + "/v1/SUPERSECRETKEY", w.urls[1] + "/?apikey=SUPERSECRETKEY"}
	if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{RPCURLs: keyed}); err != nil {
		t.Fatal(err)
	}
	w.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 2 })
	w.start()
	// The second endpoint is two blocks behind. The processed head is
	// settled on what both endpoints have, so it trails by those two blocks
	// as well — and so does the "latest block" of the report: the newest
	// block of the network is the newest one two endpoints have, not what
	// one of them says it is.
	const lag = 2
	var ns core.NodeStatus
	// The report is awaited in the state it comes to rest in: both endpoints
	// verified and the first — the preferred one — in use. Under load the
	// second endpoint's chain id can be verified before the first one's; the
	// watcher then starts on the second, with the same processed head, and
	// moves to the first a tick later. (Asserting on whichever report
	// arrived first failed about once in twenty runs of a loaded machine.)
	w.eventually("status report", func() bool {
		var err error
		ns, err = w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && ns.Healthy && len(ns.Endpoints) == 2 && ns.Endpoints[1].BlockNumber != 0 &&
			ns.Head.Number == chainStart-lag-testSettle &&
			ns.Endpoints[0].Healthy && ns.Endpoints[0].InUse && ns.LatestBlock == chainStart-lag
	})
	if ns.LatestBlock != chainStart-lag || ns.LagBlocks != testSettle || ns.LastCheckedAt.IsZero() {
		t.Fatalf("node status = %+v", ns)
	}
	first, second := ns.Endpoints[0], ns.Endpoints[1]
	if !first.Healthy || !first.InUse || first.BlockNumber != chainStart || first.Error != "" || first.URL != evm.RedactURL(keyed[0]) {
		t.Fatalf("first endpoint = %+v", first)
	}
	if second.InUse || second.BlockNumber != chainStart-2 {
		t.Fatalf("second endpoint = %+v", second)
	}
	if s := fmt.Sprintf("%+v %+v", ns, w.bridge.Status()); strings.Contains(s, "SUPERSECRETKEY") {
		t.Fatalf("an API key from an endpoint URL is in the status: %s", s)
	}
	if strings.Contains(w.logs.String(), "SUPERSECRETKEY") {
		t.Fatal("an API key from an endpoint URL is in the log")
	}

	// A dead primary shows up as such, with the fallback in use.
	w.nodes[0].Set(func(b *evmtest.Behavior) { b.Down = true })
	w.nodes[1].Set(func(b *evmtest.Behavior) { b.Lag = 0 })
	w.mine(3)
	w.eventually("failover to show in the status", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && len(ns.Endpoints) == 2 && !ns.Endpoints[0].Healthy && ns.Endpoints[0].Error != "" &&
			!ns.Endpoints[0].InUse && ns.Endpoints[1].InUse && ns.Endpoints[1].Healthy
	})
	_ = coretest.Epoch
}

// TestOnlyEndpointServesNoLogs: the operator configured nothing but an
// endpoint that refuses eth_getLogs (the official dataseed nodes do). Nothing
// can be detected; that must be visible, with the reason.
func TestOnlyEndpointServesNoLogs(t *testing.T) {
	w := newWorld(t)
	w.nodes[0].Set(func(b *evmtest.Behavior) {
		b.LogsBatchError = &evmtest.RPCError{Code: -32005, Message: "method eth_getLogs in batch triggered rate limit"}
		b.LogsError = &evmtest.RPCError{Code: -32005, Message: "limit exceeded"}
	})
	w.start()
	w.firstHead() // nothing is watched yet: no log query needed
	first := w.coreHead()
	p := w.env.CreatePayment("25")
	w.eventually("the watcher to report the refusal", func() bool {
		return strings.Contains(w.watcherStatus().LastError, "limit exceeded")
	})
	w.eventually("the node status to explain", func() bool {
		ns, err := w.env.Service.NodeStatus(w.env.Operator())
		return err == nil && len(ns.Endpoints) == 1 && strings.Contains(ns.Endpoints[0].Error, "eth_getLogs")
	})
	// The payer pays, in time; nobody sees it.
	w.pay(p, 25)
	w.mine(1)
	// No head is reported for blocks that could not be scanned: the
	// payment does not expire behind the operator's back.
	w.advance(2 * time.Hour)
	w.mine(testSettle + 5)
	w.never("the unscanned payment expires", func() bool { return w.status(p) != core.StatusAwaiting })
	if h := w.coreHead(); h != first {
		t.Fatalf("core head = %+v (was %+v when the payment was created) although nothing could be scanned", h, first)
	}

	// The operator adds an endpoint that serves logs, and the payment is
	// found. The core first hears of it two hours after the deadline — but
	// the transfer comes with the timestamp of its block, which is before
	// the deadline: an outage of the watcher is never held against the
	// payer.
	good := evmtest.NewNode(w.chain)
	good.Set(func(b *evmtest.Behavior) { b.FinalityDepth = w.finality })
	if _, err := w.env.Service.UpdateNetwork(w.env.Operator(), core.UpdateNetworkInput{RPCURLs: []string{w.urls[0], good.Serve(t)}}); err != nil {
		t.Fatal(err)
	}
	w.mine(testSettle + testConfirmations + 1)
	// (The first endpoint is now the second log source, and it refuses: it
	// takes three refusals before it is held to have no log service.)
	w.writeOffLogs()
	got := w.waitStatus(p, core.StatusPaid)
	if got.NeedsAttention {
		t.Fatalf("payment = %+v", got)
	}
}
