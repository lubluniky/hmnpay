package evm_test

// Reality check against real public BSC endpoints.
//
// These tests talk to the network, so they are skipped unless
// HMNPAY_LIVE_RPC=1 is set:
//
//	HMNPAY_LIVE_RPC=1 go test ./internal/chain/evm -run Live -v -count=1
//
// HMNPAY_LIVE_ENDPOINTS (comma-separated URLs) overrides the endpoint list.
// Everything is read-only, public data. The output is a table of what each
// endpoint allows (log range, recipients per call, latency); the package
// defaults were chosen from such measurements, not assumed.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

var liveEndpoints = []string{
	"https://bsc-rpc.publicnode.com",
	"https://bsc-dataseed.bnbchain.org",
	"https://bsc-dataseed1.binance.org",
	"https://bsc.drpc.org",
	"https://binance.llamarpc.com",
	"https://1rpc.io/bnb",
	"https://bsc.blockrazor.xyz",
	"https://public-bsc.nownodes.io",
	"https://rpc-bsc.48.club",
	"https://bsc.meowrpc.com",
	"https://bsc-mainnet.public.blastapi.io",
	"https://api.zan.top/bsc-mainnet",
	"https://bsc.rpc.blxrbdn.com",
}

func liveList(t *testing.T) []string {
	t.Helper()
	if os.Getenv("HMNPAY_LIVE_RPC") != "1" {
		t.Skip("network test; set HMNPAY_LIVE_RPC=1 to run")
	}
	if env := os.Getenv("HMNPAY_LIVE_ENDPOINTS"); env != "" {
		return strings.Split(env, ",")
	}
	return liveEndpoints
}

func TestLiveEndpointSurvey(t *testing.T) {
	urls := liveList(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	type row struct {
		url, verdict, chain, logs, maxRange, latency, head, finalLag, note string
	}
	var rows []row
	for _, u := range urls {
		r := evm.ProbeWith(ctx, u, 56, usdt, evm.ProbeOptions{
			Endpoint:  evm.EndpointOptions{Timeout: 20 * time.Second},
			LogRanges: []uint64{25, 50, 100, 500, 1000, 2000, 5000, 10000},
		})
		row := row{url: r.URL, verdict: string(r.Verdict), chain: "-", logs: "-", maxRange: "-", latency: "-", head: "-", finalLag: "-"}
		if u != r.URL {
			row.url = u // public URLs without credentials: show them in full
		}
		if r.Chain.Known {
			row.chain = fmt.Sprint(r.Chain.ID)
		}
		if !r.Reach.OK {
			row.note = r.Reach.Error
			rows = append(rows, row)
			continue
		}
		row.head = fmt.Sprintf("%d (age %s)", r.Head.Number, r.Head.Age.Round(100*time.Millisecond))
		if r.Token.Checked && r.Token.OK {
			row.note = fmt.Sprintf("%s/%d dec", r.Token.Symbol, r.Token.Decimals)
			if r.Token.Decimals != 18 || r.Token.Symbol != "USDT" {
				t.Errorf("%s: USDT on BSC reads as %s with %d decimals", u, r.Token.Symbol, r.Token.Decimals)
			}
		} else if r.Token.Checked {
			row.note = "token: " + r.Token.Error
		}
		switch {
		case r.Logs.OK:
			row.logs = "yes"
			row.maxRange = fmt.Sprint(r.Logs.LargestRangeOK)
			if r.Logs.SmallestRangeFailed != 0 {
				row.maxRange += fmt.Sprintf(" (%d refused: %s)", r.Logs.SmallestRangeFailed, r.Logs.Error)
			} else {
				row.maxRange = ">=" + row.maxRange
			}
		default:
			row.logs = "NO"
			row.maxRange = r.Logs.Error
		}

		// Latency: median of five eth_blockNumber round trips on a warm
		// connection, and the finality lag.
		ep, err := evm.NewEndpoint(u, evm.EndpointOptions{Timeout: 20 * time.Second})
		if err == nil {
			var samples []time.Duration
			for i := 0; i < 5; i++ {
				start := time.Now()
				if _, err := ep.BlockNumber(ctx); err == nil {
					samples = append(samples, time.Since(start))
				}
				time.Sleep(150 * time.Millisecond)
			}
			if len(samples) > 0 {
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				row.latency = samples[len(samples)/2].Round(time.Millisecond).String()
			}
			if hr, err := ep.Head(ctx, evm.HeadRequest{Finalized: true}); err == nil && hr.Finalized != nil {
				row.finalLag = fmt.Sprint(hr.Latest.Number - hr.Finalized.Number)
			} else if err != nil {
				row.finalLag = "err"
			} else {
				row.finalLag = "n/a"
			}
		}
		rows = append(rows, row)
	}

	t.Logf("\n%-42s | %-13s | %-5s | %-4s | %-8s | %-9s | %-30s | %s", "endpoint", "verdict", "chain", "logs", "latency", "final.lag", "head", "max log range / notes")
	for _, r := range rows {
		t.Logf("%-42s | %-13s | %-5s | %-4s | %-8s | %-9s | %-30s | %s | %s", r.url, r.verdict, r.chain, r.logs, r.latency, r.finalLag, r.head, r.maxRange, r.note)
	}
}

// TestLiveChainFacts measures the block time and verifies the USDT contract
// facts on chain, through the failover client.
func TestLiveChainFacts(t *testing.T) {
	urls := liveList(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := evm.NewClient(evm.Config{ChainID: 56, Endpoints: urls, Endpoint: evm.EndpointOptions{Timeout: 20 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	client.Refresh(ctx)
	for _, st := range client.Status() {
		t.Logf("status: %-42s state=%-11s block=%d lag=%d latency=%s err=%q", st.URL, st.State, st.BlockNumber, st.Lag, st.Latency.Round(time.Millisecond), st.LastError)
	}

	dec, err := client.Decimals(ctx, usdt)
	if err != nil {
		t.Fatal(err)
	}
	sym, err := client.Symbol(ctx, usdt)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("USDT %s: symbol=%q decimals=%d", usdt, sym, dec)
	if dec != 18 || sym != "USDT" {
		t.Fatalf("USDT on BSC: got %q / %d decimals, want USDT / 18", sym, dec)
	}

	err = client.Do(ctx, func(ctx context.Context, e *evm.Endpoint) error {
		head, err := e.Head(ctx, evm.HeadRequest{Finalized: true})
		if err != nil {
			return err
		}
		for _, span := range []uint64{1000, 100_000} {
			old, err := e.HeaderByNumber(ctx, head.Latest.Number-span)
			if err != nil {
				return err
			}
			t.Logf("block time over the last %d blocks: %.3f s (via %s)", span,
				float64(head.Latest.Time-old.Time)/float64(span), e.Label())
		}
		if head.Finalized != nil {
			t.Logf("latest=%d finalized=%d (lag %d blocks)", head.Latest.Number, head.Finalized.Number, head.Latest.Number-head.Finalized.Number)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLiveTransferScan scans real USDT transfers with the adaptive scanner on
// every endpoint that serves logs, cross-checks each endpoint's result
// against the others, and verifies a sample of transfers against receipts.
func TestLiveTransferScan(t *testing.T) {
	urls := liveList(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// Fix the window using the first endpoint that answers, and discover
	// real recipients: the addresses that received USDT in two recent
	// blocks (an unfiltered query; USDT is busy enough for that).
	var to uint64
	var recipients []ethaddr.Address
	for _, u := range urls {
		ep, err := evm.NewEndpoint(u, evm.EndpointOptions{Timeout: 20 * time.Second})
		if err != nil {
			continue
		}
		n, err := ep.BlockNumber(ctx)
		if err != nil {
			continue
		}
		to = n - 200 // well below the tip: identical on every endpoint
		logs, err := ep.Logs(ctx, evm.LogFilter{FromBlock: to - 1, ToBlock: to, Address: usdt, Topics: [][]evm.Hash{{evm.TransferTopic}}})
		if err != nil {
			t.Logf("%s: unfiltered query failed: %v", u, err)
			continue
		}
		seen := map[ethaddr.Address]bool{}
		for _, l := range logs {
			if tr, err := evm.DecodeTransfer(l); err == nil && !seen[tr.To] && len(recipients) < 40 {
				seen[tr.To] = true
				recipients = append(recipients, tr.To)
			}
		}
		t.Logf("%d USDT transfers in blocks %d..%d; watching %d of their recipients", len(logs), to-1, to, len(recipients))
		break
	}
	if to == 0 || len(recipients) == 0 {
		t.Fatal("could not discover recipients")
	}
	const window = 300
	from := to - window + 1

	type result struct {
		label     string
		transfers []evm.Transfer
	}
	var results []result
	for _, u := range urls {
		ep, err := evm.NewEndpoint(u, evm.EndpointOptions{Timeout: 20 * time.Second})
		if err != nil {
			continue
		}
		// Scan the way the watcher does: one LogRange-sized step at a
		// time, pausing between steps so as not to trip rate limiters.
		start := time.Now()
		var got []evm.Transfer
		var scanErr error
		calls := 0
		for cur := from; cur <= to; {
			end := min(cur+ep.LogRange()-1, to)
			part, err := scanAll(ctx, ep, evm.TransferQuery{Token: usdt, FromBlock: cur, ToBlock: end, Recipients: recipients})
			calls++
			if err != nil {
				scanErr = err
				break
			}
			got = append(got, part...)
			cur = end + 1
			time.Sleep(250 * time.Millisecond)
		}
		elapsed := time.Since(start).Round(time.Millisecond)
		if scanErr != nil {
			t.Logf("%-42s scan failed after %s (%d steps): %v", u, elapsed, calls, scanErr)
			continue
		}
		t.Logf("%-42s %d transfers in %d blocks, %d steps, %s, learned range=%d", u, len(got), window, calls, elapsed, ep.LogRange())
		results = append(results, result{label: u, transfers: got})
	}
	if len(results) == 0 {
		t.Fatal("no endpoint served logs")
	}
	ref := results[0]
	for _, r := range results[1:] {
		if len(r.transfers) != len(ref.transfers) {
			t.Errorf("%s returned %d transfers, %s returned %d", r.label, len(r.transfers), ref.label, len(ref.transfers))
			continue
		}
		for i := range r.transfers {
			a, b := r.transfers[i], ref.transfers[i]
			if a.TxHash != b.TxHash || a.LogIndex != b.LogIndex || a.BlockHash != b.BlockHash || a.Amount.Cmp(b.Amount) != 0 || a.From != b.From {
				t.Errorf("%s and %s disagree on transfer %d", r.label, ref.label, i)
				break
			}
		}
	}

	// Receipt cross-check of a sample on a different endpoint where
	// possible.
	verifier, err := evm.NewEndpoint(results[len(results)-1].label, evm.EndpointOptions{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for i, tr := range ref.transfers {
		if i >= 5 {
			break
		}
		if tr.Token != usdt || tr.Amount.Sign() < 0 || tr.BlockNumber < from || tr.BlockNumber > to {
			t.Fatalf("decoded transfer does not match the query: %+v", tr)
		}
		rcpt, err := verifier.TransactionReceipt(ctx, tr.TxHash)
		if err != nil {
			t.Logf("receipt %s: %v", tr.TxHash, err)
			continue
		}
		found := false
		for _, l := range rcpt.Logs {
			if l.LogIndex != tr.LogIndex {
				continue
			}
			dt, err := evm.DecodeTransfer(l)
			found = err == nil && dt.Amount.Cmp(tr.Amount) == 0 && dt.To == tr.To && dt.From == tr.From && l.Address == usdt
		}
		if !found || rcpt.BlockHash != tr.BlockHash || rcpt.Status != 1 {
			t.Errorf("transfer %s#%d is not confirmed by its receipt", tr.TxHash, tr.LogIndex)
		}
		checked++
	}
	t.Logf("verified %d transfers against receipts via %s", checked, verifier.Label())

	// How many recipients can one filter carry? (go-ethereum caps topic
	// alternatives; providers add their own limits.)
	for _, r := range results {
		ep, _ := evm.NewEndpoint(r.label, evm.EndpointOptions{Timeout: 20 * time.Second})
		var line []string
		for _, n := range []int{100, 500, 1000, 1001, 2000} {
			topics := make([]evm.Hash, n)
			for i := range topics {
				a := addr(i)
				copy(topics[i][12:], a[:])
			}
			_, err := ep.Logs(ctx, evm.LogFilter{FromBlock: to - 4, ToBlock: to, Address: usdt,
				Topics: [][]evm.Hash{{evm.TransferTopic}, nil, topics}})
			if err != nil {
				k, _ := evm.KindOf(err)
				line = append(line, fmt.Sprintf("%d:%s", n, k))
				t.Logf("  %s with %d recipients: %v", r.label, n, err)
				break
			}
			line = append(line, fmt.Sprintf("%d:ok", n))
			time.Sleep(300 * time.Millisecond)
		}
		t.Logf("%-42s recipients per filter: %s", r.label, strings.Join(line, " "))
	}
}

// TestLiveLogTipLag measures how far an endpoint's eth_getLogs answer trails
// the block height the same backend reports in the same batch — the "silent
// lag" that the watcher's settle depth has to cover. It asks for USDT
// transfers to a set of busy recipients over a range that ends above the
// head, and compares the newest block that produced a log with the batched
// eth_blockNumber. A lag of 0–1 blocks is noise (not every block has a
// transfer to the sampled recipients).
func TestLiveLogTipLag(t *testing.T) {
	urls := liveList(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var recipients []ethaddr.Address
	for _, u := range urls {
		ep, err := evm.NewEndpoint(u, evm.EndpointOptions{Timeout: 20 * time.Second})
		if err != nil {
			continue
		}
		n, err := ep.BlockNumber(ctx)
		if err != nil {
			continue
		}
		logs, err := ep.Logs(ctx, evm.LogFilter{FromBlock: n - 12, ToBlock: n - 10, Address: usdt, Topics: [][]evm.Hash{{evm.TransferTopic}}})
		if err != nil {
			continue
		}
		count := map[ethaddr.Address]int{}
		for _, l := range logs {
			if tr, err := evm.DecodeTransfer(l); err == nil {
				count[tr.To]++
			}
		}
		for a := range count {
			if len(recipients) < 200 {
				recipients = append(recipients, a)
			}
		}
		break
	}
	if len(recipients) == 0 {
		t.Fatal("could not discover recipients")
	}
	for _, u := range urls {
		ep, err := evm.NewEndpoint(u, evm.EndpointOptions{Timeout: 20 * time.Second})
		if err != nil {
			continue
		}
		var lags []uint64
		unknown := 0
		for i := 0; i < 15; i++ {
			n, err := ep.BlockNumber(ctx)
			if err != nil {
				break
			}
			scan, err := ep.TransferLogs(ctx, evm.TransferQuery{Token: usdt, FromBlock: n - 20, ToBlock: n + 20, Recipients: recipients})
			if err != nil {
				t.Logf("%-40s sample %d: %v", u, i, err)
				break
			}
			if scan.BackendHead == 0 {
				unknown++
				continue
			}
			var newest uint64
			for _, tr := range scan.Transfers {
				newest = max(newest, tr.BlockNumber)
			}
			if newest == 0 || newest > scan.BackendHead {
				lags = append(lags, 0)
			} else {
				lags = append(lags, scan.BackendHead-newest)
			}
			time.Sleep(700 * time.Millisecond)
		}
		sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
		if len(lags) == 0 {
			t.Logf("%-40s no samples (backend height unknown in %d)", u, unknown)
			continue
		}
		t.Logf("%-40s log tip lag over %d samples: min %d, median %d, max %d blocks", u, len(lags), lags[0], lags[len(lags)/2], lags[len(lags)-1])
	}
}
