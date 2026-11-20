package evm

import (
	"context"
	"errors"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
)

// ProbeVerdict is the one-line summary of a probe, ordered by how early in
// the checklist the endpoint failed.
type ProbeVerdict string

const (
	// ProbeOK: reachable, right chain, token found, eth_getLogs works. The
	// endpoint can serve as a primary.
	ProbeOK ProbeVerdict = "ok"
	// ProbeUnreachable: no valid JSON-RPC answer at all (cannot connect,
	// TLS/DNS failure, timeout, HTTP error, access denied, not an RPC
	// endpoint). See Reach.Error.
	ProbeUnreachable ProbeVerdict = "unreachable"
	// ProbeWrongChain: the endpoint works but serves another network.
	ProbeWrongChain ProbeVerdict = "wrong_chain"
	// ProbeTokenMissing: right chain, but the token contract has no code
	// there or does not answer symbol()/decimals() — a wrong token address
	// or a node that is not synced.
	ProbeTokenMissing ProbeVerdict = "token_missing"
	// ProbeNoLogs: everything works except eth_getLogs, which this provider
	// does not serve (common on free endpoints). The endpoint cannot detect
	// payments on its own, but is still useful as a secondary endpoint for
	// cross-checking and for head tracking.
	ProbeNoLogs ProbeVerdict = "no_logs"
)

// ProbeResult is the outcome of Probe. Each step is reported separately so
// the UI can render a checklist; Verdict summarises them.
type ProbeResult struct {
	// URL is the redacted endpoint URL.
	URL     string
	Verdict ProbeVerdict

	// Reach: does the endpoint answer JSON-RPC, and how fast?
	Reach struct {
		OK bool
		// Latency is the faster of the first two round trips (the second
		// one runs on an established connection).
		Latency time.Duration
		// Kind and Error describe the failure when OK is false. Kind
		// distinguishes "cannot connect" (KindTransport) from "answered,
		// but not usable" (KindHTTP, KindUnavailable, KindRateLimited,
		// KindBadResponse).
		Kind  ErrorKind
		Error string
	}

	// Chain: which network is it?
	Chain struct {
		// Known is true if eth_chainId returned a value.
		Known bool
		// ID is the reported chain id; Expected the one asked for.
		ID       uint64
		Expected uint64
		Matches  bool
	}

	// Head: the latest block.
	Head struct {
		OK     bool
		Number uint64
		// Time is the block timestamp, and Age how far it lies behind the
		// local clock. A large age means the node is not synced.
		Time  time.Time
		Age   time.Duration
		Error string
	}

	// Token: is the token contract there?
	Token struct {
		// Checked is false if the step was skipped (zero token address,
		// or an earlier step failed).
		Checked bool
		// CodePresent is true if there is contract code at the address.
		CodePresent bool
		Symbol      string
		// Decimals is what decimals() returned; DecimalsRead says whether
		// it answered (independently of symbol()).
		Decimals     uint8
		DecimalsRead bool
		// OK is true if code is present and both symbol() and decimals()
		// could be read.
		OK    bool
		Error string
	}

	// Logs: does eth_getLogs work on a small recent range?
	Logs struct {
		// Checked is false if the step was skipped.
		Checked bool
		OK      bool
		// Blocks is the size of the range that was queried.
		Blocks uint64
		// LargestRangeOK is the largest block range that succeeded when a
		// range ladder was requested (ProbeOptions.LogRanges), and
		// SmallestRangeFailed the smallest that was refused (0 if none).
		LargestRangeOK      uint64
		SmallestRangeFailed uint64
		// Kind and Error describe the refusal when OK is false.
		Kind  ErrorKind
		Error string
	}
}

// ProbeOptions tunes Probe.
type ProbeOptions struct {
	// Endpoint holds transport options (timeout, HTTP client, ...).
	Endpoint EndpointOptions
	// LogBlocks is the size of the recent block range used for the
	// eth_getLogs check. Default: 5 — small enough for the strictest public
	// providers (some allow only 25 blocks).
	LogBlocks uint64
	// LogRanges, if set, makes the probe additionally try eth_getLogs over
	// these block ranges (ascending), stopping at the first refusal, to
	// measure the provider's range limit. Used by diagnostics, not by the
	// setup wizard.
	LogRanges []uint64
	// Now is the clock used for Head.Age. Default: time.Now.
	Now func() time.Time
}

// Probe examines a single RPC URL for the setup wizard and the "add
// endpoint" dialog. It never returns an error: every problem is part of the
// result. It performs, in order:
//
//  1. eth_chainId — reachable at all? how fast?
//  2. compares the chain id with expectedChainID
//  3. the latest block (number and age)
//  4. the token: contract code present, symbol() and decimals() readable
//     (skipped if tokenContract is the zero address)
//  5. eth_getLogs for the token's Transfer events over a few recent blocks —
//     many free endpoints disable it
//
// Steps 3–5 are skipped if the endpoint is unreachable or on the wrong
// chain: results from another network would only mislead.
func Probe(ctx context.Context, rawURL string, expectedChainID uint64, tokenContract ethaddr.Address) ProbeResult {
	return ProbeWith(ctx, rawURL, expectedChainID, tokenContract, ProbeOptions{})
}

// ProbeWith is Probe with options.
func ProbeWith(ctx context.Context, rawURL string, expectedChainID uint64, token ethaddr.Address, opts ProbeOptions) ProbeResult {
	var res ProbeResult
	res.URL = RedactURL(rawURL)
	res.Verdict = ProbeUnreachable
	res.Chain.Expected = expectedChainID
	if opts.LogBlocks == 0 {
		opts.LogBlocks = 5
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	ep, err := NewEndpoint(rawURL, opts.Endpoint)
	if err != nil {
		res.Reach.Kind = KindTransport
		res.Reach.Error = err.Error()
		return res
	}

	// 1. Reachability and chain id. The probe makes as few requests as it
	// can: free endpoints throttle bursts, and a probe that trips the rate
	// limiter would report a working endpoint as broken.
	started := time.Now()
	id, err := ep.ChainID(ctx)
	res.Reach.Latency = time.Since(started)
	if err != nil {
		res.Reach.Kind, _ = KindOf(err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			res.Reach.Kind = KindTransport
		}
		res.Reach.Error = failureText(err)
		return res
	}
	res.Reach.OK = true

	// 2. Chain id.
	res.Chain.Known = true
	res.Chain.ID = id
	res.Chain.Matches = id == expectedChainID
	if !res.Chain.Matches {
		res.Verdict = ProbeWrongChain
		return res
	}

	// 3. Latest block. This request reuses the connection, so its round
	// trip (without DNS and TLS setup) is the better latency figure.
	started = time.Now()
	head, err := ep.HeaderByTag(ctx, TagLatest)
	if warm := time.Since(started); err == nil && warm < res.Reach.Latency {
		res.Reach.Latency = warm
	}
	if err != nil {
		res.Head.Error = failureText(err)
		res.Reach.OK = false
		res.Reach.Kind, _ = KindOf(err)
		res.Reach.Error = res.Head.Error
		return res
	}
	res.Head.OK = true
	res.Head.Number = head.Number
	res.Head.Time = time.Unix(int64(min(head.Time, 1<<62)), 0).UTC()
	res.Head.Age = now().Sub(res.Head.Time)

	// 4. Token contract.
	tokenOK := true
	if !token.IsZero() {
		res.Token.Checked = true
		tokenOK = false
		code, err := ep.Code(ctx, token)
		switch {
		case err != nil:
			res.Token.Error = failureText(err)
		case len(code) == 0:
			res.Token.Error = "no contract code at the token address on this chain"
		default:
			res.Token.CodePresent = true
			sym, serr := ep.Symbol(ctx, token)
			dec, derr := ep.Decimals(ctx, token)
			if derr == nil {
				res.Token.Decimals, res.Token.DecimalsRead = dec, true
			}
			if serr == nil {
				res.Token.Symbol = sym
			}
			switch {
			case serr != nil:
				res.Token.Error = "symbol(): " + failureText(serr)
			case derr != nil:
				res.Token.Error = "decimals(): " + failureText(derr)
			default:
				res.Token.Symbol, res.Token.Decimals, res.Token.OK = sym, dec, true
				tokenOK = true
			}
		}
	}

	// 5. eth_getLogs on a small, recent range. The filter asks for
	// transfers to an address nobody sends to, so the answer is small no
	// matter how busy the token is; what matters is whether the node
	// accepts the query.
	logToken := token
	res.Logs.Checked = true
	res.Logs.Blocks = min(opts.LogBlocks, head.Number+1)
	probeLogs := func(blocks uint64) error {
		// Stay a few blocks below the tip: load-balanced providers
		// reject or truncate ranges their backend has not seen yet.
		to := head.Number - min(head.Number, 3)
		from := to - min(to, blocks-1)
		_, err := ep.Logs(ctx, LogFilter{
			FromBlock: from, ToBlock: to, Address: logToken,
			Topics: [][]Hash{{TransferTopic}, nil, {addressTopic(probeRecipient)}},
		})
		return err
	}
	if err := probeLogs(res.Logs.Blocks); err != nil {
		res.Logs.Kind, _ = KindOf(err)
		res.Logs.Error = failureText(err)
	} else {
		res.Logs.OK = true
		res.Logs.LargestRangeOK = res.Logs.Blocks
		for _, blocks := range opts.LogRanges {
			if blocks <= res.Logs.LargestRangeOK || blocks > head.Number {
				continue
			}
			if err := probeLogs(blocks); err != nil {
				res.Logs.SmallestRangeFailed = blocks
				res.Logs.Kind, _ = KindOf(err)
				res.Logs.Error = failureText(err)
				break
			}
			res.Logs.LargestRangeOK = blocks
		}
	}

	switch {
	case !tokenOK:
		res.Verdict = ProbeTokenMissing
	case !res.Logs.OK:
		res.Verdict = ProbeNoLogs
	default:
		res.Verdict = ProbeOK
	}
	return res
}

// probeRecipient is the recipient used in probe filters: an address nobody
// transfers to, which keeps probe answers empty however busy the token is.
var probeRecipient = ethaddr.Address{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe}
