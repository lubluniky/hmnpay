package evm

import "github.com/lubluniky/hmnpay/internal/chain/ethaddr"

// BNB Smart Chain identifiers.
const (
	// BSCMainnetChainID is the chain id of BNB Smart Chain.
	BSCMainnetChainID uint64 = 56
	// BSCTestnetChainID is the chain id of the BNB Smart Chain testnet
	// (Chapel).
	BSCTestnetChainID uint64 = 97
)

// USDTBSCMainnet is the BSC-USD ("Binance-Peg USDT") token contract on BNB
// Smart Chain. Its decimals() is 18 — unlike USDT on Ethereum or Tron, which
// have 6. The live test suite re-verifies symbol and decimals on chain; code
// must still read them rather than assume.
var USDTBSCMainnet = ethaddr.MustParse("0x55d398326f99059fF775485246999027B3197955")

// BSCMainnetPublicEndpoints is a starting list of free, keyless BSC endpoints
// for the setup wizard, in order of preference. It is a convenience, not a
// guarantee: free endpoints change their limits without notice, and an
// operator who cares about availability should add an endpoint of their own.
//
// The order and the comments come from a survey made with this package's live
// tests (TestLiveEndpointSurvey, TestLiveTransferScan) in November 2026:
//
//	endpoint                      eth_getLogs   max range    topics/filter  latency
//	bsc-rpc.publicnode.com        yes           >= 10 000    >= 2 000       ~50 ms
//	rpc-bsc.48.club               yes           ~2 000 (1)   1 000          ~50 ms
//	bsc-dataseed.bnbchain.org     no (2)        -            -              ~50 ms
//	bsc-dataseed1.binance.org     no (2)        -            -              ~90 ms
//	1rpc.io/bnb                   yes           50           1 000          ~250 ms
//
//	(1) 5 000 blocks are accepted but time out; 1 000 take 1–3 s.
//	(2) Every eth_getLogs is answered with -32005 "limit exceeded".
//
// 1rpc.io has a usage quota per client that a day of testing exhausted; it is
// last on the list, as a reserve for when everything else is down.
//
// Also surveyed and left out: bsc.blockrazor.xyz (25-block ranges and an
// aggressive burst limit), bsc.drpc.org, public-bsc.nownodes.io,
// bsc.meowrpc.com and bsc-mainnet.public.blastapi.io (rate-limited after a
// handful of requests), api.zan.top and bsc.rpc.blxrbdn.com (eth_getLogs
// and/or eth_call refused), binance.llamarpc.com (no longer resolves).
//
// Two endpoints without eth_getLogs are on the list on purpose. They are the
// chain's own, they are fast and reliable for headers and receipts, and the
// watcher needs a second, independent endpoint to confirm what the first one
// reports. Placing one of them second makes it the default cross-check for
// the log-serving primary.
var BSCMainnetPublicEndpoints = []string{
	"https://bsc-rpc.publicnode.com",
	"https://bsc-dataseed.bnbchain.org",
	"https://rpc-bsc.48.club",
	"https://bsc-dataseed1.binance.org",
	"https://1rpc.io/bnb",
}
