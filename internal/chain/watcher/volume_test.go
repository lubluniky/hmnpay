package watcher_test

import (
	"fmt"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

// TestRequestVolumeByWatchListSize measures what the size of the watch list
// costs in RPC requests, in the steady state — one tick per new block, two
// endpoints that both serve logs — and pins the numbers the package
// documentation states ("What the watch list costs").
//
// An eth_getLogs filter takes at most evm.DefaultMaxRecipients (500)
// recipient addresses, so a scan of N addresses is ceil(N/500) calls: once
// per tick on the primary, for the blocks above the cursor, and once per
// tick on the second log endpoint, for the blocks being settled. Everything
// else a tick asks for — heads, the header to settle on — does not depend on
// N. The growth is linear and has no bound of its own: the bound is the size
// of the watch list.
func TestRequestVolumeByWatchListSize(t *testing.T) {
	for _, n := range []int{1, 500, 1000, 5000, 20000} {
		t.Run(fmt.Sprintf("%d addresses", n), func(t *testing.T) {
			if (testing.Short() || raceDetector) && n > 1000 {
				// Forty 35 kB filters per endpoint and tick, each matched
				// against 20 000 addresses by the fake node: ten seconds
				// under the race detector, for numbers the smaller lists
				// already pin (the count is ceil(n/500), whatever n).
				t.Skip("the large watch lists are measured without the race detector, and not in short mode")
			}
			h := newHarness(t, withNodes(2))
			since := h.chain.Head().Number
			for i := 0; i < n; i++ {
				h.watch.addSince(addr(1_000_000+i), since)
			}
			h.mineTicking(testSettle + 2)
			for _, node := range h.nodes {
				node.ResetCounters()
			}
			const ticks = 6
			h.pay(addr(1_000_000), 3) // a payment along the way costs a receipt, no more
			h.mineTicking(ticks)
			chunks := (n + evm.DefaultMaxRecipients - 1) / evm.DefaultMaxRecipients
			for i, node := range h.nodes {
				logs, requests := node.Calls("eth_getLogs"), node.Requests()
				t.Logf("%5d addresses, endpoint %d: %5.1f eth_getLogs and %5.1f HTTP requests per tick (%4.1f requests/s at the default 3 s tick)",
					n, i, float64(logs)/ticks, float64(requests)/ticks, float64(requests)/ticks/3)
				if logs != chunks*ticks {
					t.Errorf("endpoint %d: %d eth_getLogs calls in %d ticks, want %d per tick (one per %d addresses)",
						i, logs, ticks, chunks, evm.DefaultMaxRecipients)
				}
				// Besides the log queries: the head request, and for the
				// primary the header of the block the cursor moves to. A
				// little room for the receipt and block time of the payment.
				if other := requests - logs; other > 3*ticks+4 {
					t.Errorf("endpoint %d: %d requests other than eth_getLogs in %d ticks; they must not grow with the watch list", i, other, ticks)
				}
			}
			h.assertModel(startBlock)
		})
	}
}
