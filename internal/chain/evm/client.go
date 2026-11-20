package evm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
)

// Client defaults.
const (
	// DefaultMaxLag is how many blocks an endpoint may trail the chain's
	// height — the height at least two endpoints have reached, see
	// Client.Refresh — before failover prefers another one. On BSC (0.45 s
	// blocks) 30 blocks is about 13 seconds.
	DefaultMaxLag = 30
	// DefaultMonitorInterval is the period of the background health check
	// run by Monitor.
	DefaultMonitorInterval = 15 * time.Second
	// chainRecheckInterval is how often a verified chain id is re-verified.
	chainRecheckInterval = 10 * time.Minute
)

// Config configures a Client.
type Config struct {
	// ChainID is the chain the operator configured (56 for BNB Smart Chain,
	// 97 for its testnet). An endpoint is never used before it has
	// reported exactly this id, and is dropped as soon as it reports
	// another.
	ChainID uint64
	// Endpoints are the JSON-RPC URLs in order of preference: the first
	// usable one is the primary, the rest are fallbacks.
	Endpoints []string
	// MaxLag is the number of blocks an endpoint may trail the height two
	// endpoints have reached (as measured by Refresh/Monitor) and still be
	// preferred. Default: DefaultMaxLag.
	MaxLag uint64
	// Logger receives failover and health transitions. Default: discard.
	Logger *slog.Logger
	// Endpoint holds the per-endpoint options (HTTP client, timeout,
	// response size limit, clock, log scanner tuning). The same HTTP
	// client is shared by all endpoints.
	Endpoint EndpointOptions
}

// Client is an ordered set of JSON-RPC endpoints for one chain with health
// tracking and failover. It is safe for concurrent use.
//
// Selection rules (see Pick): endpoints are considered in configured order;
// one is usable if it has proven to be on the configured chain, is not in a
// failure cooldown, and is not lagging by more than MaxLag blocks behind the
// height at least two endpoints have reached (one endpoint's claim about the
// height of the chain makes nobody else "lagging": see Refresh). When the
// primary recovers it is used again automatically
// (fail-back). If no endpoint is fully healthy the least bad one is used —
// except that an endpoint on the wrong chain is never used, whatever the
// alternative.
//
// Health is updated passively by every call. For lag detection and prompt
// fail-back, run Monitor in a goroutine (or call Refresh periodically).
type Client struct {
	chainID uint64
	maxLag  uint64
	log     *slog.Logger
	now     func() time.Time
	eps     []*poolEndpoint

	mu      sync.Mutex
	current string // label of the endpoint handed out last, for transition logging
}

// poolEndpoint is an Endpoint plus what the pool knows about it.
type poolEndpoint struct {
	*Endpoint

	mu           sync.Mutex
	chainState   chainState
	reportedID   uint64
	chainChecked time.Time
	lag          uint64 // blocks behind the height two endpoints reached in the last Refresh
	lagKnown     bool
}

type chainState int

const (
	chainUnknown chainState = iota
	chainOK
	chainWrong
)

// NewClient validates the configuration and creates the client. It makes no
// network request; chain ids are verified lazily, before first use of each
// endpoint.
func NewClient(cfg Config) (*Client, error) {
	if cfg.ChainID == 0 {
		return nil, errors.New("evm: Config.ChainID is required")
	}
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("evm: at least one RPC endpoint is required")
	}
	if cfg.MaxLag == 0 {
		cfg.MaxLag = DefaultMaxLag
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	opts := cfg.Endpoint.withDefaults() // one shared HTTP client
	c := &Client{chainID: cfg.ChainID, maxLag: cfg.MaxLag, log: cfg.Logger, now: opts.Now}
	seen := make(map[string]int)
	for i, raw := range cfg.Endpoints {
		ep, err := NewEndpoint(raw, opts)
		if err != nil {
			return nil, fmt.Errorf("endpoint %d: %w", i+1, err)
		}
		// One endpoint under two URLs (another key in the path, say) would
		// be counted as two opinions, and confirm its own answers.
		id := Identity(raw)
		if j, dup := seen[id]; dup {
			return nil, fmt.Errorf("endpoint %d reaches the same endpoint as endpoint %d (%s): one endpoint cannot check its own answers", i+1, j, id)
		}
		seen[id] = i + 1
		c.eps = append(c.eps, &poolEndpoint{Endpoint: ep})
	}
	return c, nil
}

// ChainID returns the configured chain id.
func (c *Client) ChainID() uint64 { return c.chainID }

// Len returns the number of configured endpoints.
func (c *Client) Len() int { return len(c.eps) }

// Endpoints returns every configured endpoint, in configured order, whatever
// its state: for telling them what a caller remembers about them (for
// example Endpoint.MarkLogsAbsent). To choose one, use Pick.
func (c *Client) Endpoints() []*Endpoint {
	out := make([]*Endpoint, len(c.eps))
	for i, pe := range c.eps {
		out[i] = pe.Endpoint
	}
	return out
}

// PickOption narrows the choice made by Pick.
type PickOption func(*pickOpts)

type pickOpts struct {
	needLogs bool
	exclude  map[*Endpoint]bool
	strict   bool
}

// NeedLogs prefers endpoints on which eth_getLogs is not known to be
// unavailable — refused a moment ago, or held to have no log service at all
// (LogsHealth.NoService): such an endpoint is asked again when its pause
// ends, by whoever asks for a second log source, and its first answer ends
// the verdict; it is not the one to start from while another endpoint
// serves logs.
func NeedLogs() PickOption { return func(o *pickOpts) { o.needLogs = true } }

// Except excludes the given endpoints. It is used to obtain a second,
// independent endpoint for cross-checking.
func Except(eps ...*Endpoint) PickOption {
	return func(o *pickOpts) {
		if o.exclude == nil {
			o.exclude = make(map[*Endpoint]bool)
		}
		for _, e := range eps {
			o.exclude[e] = true
		}
	}
}

// OnlyHealthy makes Pick fail with ErrNoEndpoint rather than fall back to an
// endpoint that is cooling down or lagging.
func OnlyHealthy() PickOption { return func(o *pickOpts) { o.strict = true } }

// Pick returns the endpoint to use now: the first one, in configured order,
// that is on the right chain, not cooling down after failures, not lagging,
// and (with NeedLogs) not known to refuse eth_getLogs or to have no log
// service.
//
// If no endpoint satisfies everything, Pick relaxes the preferences in this
// order — log availability, lag, cooldown (taking the endpoint whose cooldown
// ends first) — unless OnlyHealthy is given. It never relaxes the chain id:
// an endpoint whose chain id is wrong, or could not be verified, is not
// returned. If that leaves nothing, the error wraps ErrNoEndpoint.
//
// Verifying a chain id may require a network call, hence the context.
func (c *Client) Pick(ctx context.Context, opts ...PickOption) (*Endpoint, error) {
	var o pickOpts
	for _, opt := range opts {
		opt(&o)
	}

	// Pass 0: fully healthy. Pass 1: an endpoint held to have no log
	// service whose pause has ended — it is due to be asked again, and
	// asking it as the endpoint in use is how it gets the chance (one still
	// in its pause would only refuse again, and a tick that starts from it
	// fails before any other endpoint is asked). Pass 2: ignore log
	// availability. Pass 3: also ignore lag. Pass 4: also ignore cooldowns.
	passes := 5
	if o.strict {
		passes = 3
	}
	var lastErr error
	for pass := 0; pass < passes; pass++ {
		var best *poolEndpoint
		var bestCooldown time.Time
		for _, pe := range c.eps {
			if o.exclude[pe.Endpoint] {
				continue
			}
			if pe.onWrongChain(c.now()) {
				continue
			}
			h := pe.Health()
			cooling := c.now().Before(h.CooldownUntil)
			if pass < 4 && cooling {
				continue
			}
			if pass < 3 && pe.lagging(c.maxLag) {
				continue
			}
			if pass < 1 && o.needLogs && (h.Logs.Unavailable || h.Logs.NoService) {
				continue
			}
			if pass == 1 && o.needLogs && h.Logs.Unavailable {
				continue
			}
			if pass == 4 {
				// Everything is cooling down: prefer the one that is
				// due back first.
				if best == nil || h.CooldownUntil.Before(bestCooldown) {
					best, bestCooldown = pe, h.CooldownUntil
				}
				continue
			}
			if err := c.ensureChain(ctx, pe); err != nil {
				if cerr := ctx.Err(); cerr != nil {
					return nil, cerr
				}
				lastErr = err
				continue
			}
			c.noteCurrent(pe, o)
			return pe.Endpoint, nil
		}
		if best != nil {
			if err := c.ensureChain(ctx, best); err != nil {
				if cerr := ctx.Err(); cerr != nil {
					return nil, cerr
				}
				lastErr = err
				continue
			}
			c.noteCurrent(best, o)
			return best.Endpoint, nil
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoEndpoint, lastErr)
	}
	return nil, ErrNoEndpoint
}

// noteCurrent logs a change of the endpoint in use (for unrestricted picks
// only, so cross-check picks do not make the log flap).
func (c *Client) noteCurrent(pe *poolEndpoint, o pickOpts) {
	if len(o.exclude) > 0 {
		return
	}
	c.mu.Lock()
	prev := c.current
	c.current = pe.Label()
	c.mu.Unlock()
	if prev != "" && prev != pe.Label() {
		c.log.Info("rpc endpoint switched", "from", prev, "to", pe.Label())
	}
}

// onWrongChain reports whether the endpoint is known to be on another chain.
// The verdict expires after chainRecheckInterval, so that an endpoint the
// operator has fixed is re-examined even if Monitor is not running.
func (pe *poolEndpoint) onWrongChain(now time.Time) bool {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	return pe.chainState == chainWrong && now.Sub(pe.chainChecked) < chainRecheckInterval
}

func (pe *poolEndpoint) lagging(maxLag uint64) bool {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	return pe.lagKnown && pe.lag > maxLag
}

// ensureChain makes sure the endpoint has reported the configured chain id
// recently enough, asking it if necessary.
func (c *Client) ensureChain(ctx context.Context, pe *poolEndpoint) error {
	pe.mu.Lock()
	state, checked := pe.chainState, pe.chainChecked
	pe.mu.Unlock()
	if state == chainOK && c.now().Sub(checked) < chainRecheckInterval {
		return nil
	}
	// Unknown, stale, or a wrong-chain verdict that is due for a re-check.
	return c.checkChain(ctx, pe)
}

// checkChain asks the endpoint for its chain id and records the verdict.
func (c *Client) checkChain(ctx context.Context, pe *poolEndpoint) error {
	id, err := pe.ChainID(ctx)
	if err != nil {
		return err // health already recorded by the call
	}
	pe.mu.Lock()
	prev := pe.chainState
	pe.reportedID = id
	pe.chainChecked = c.now()
	if id == c.chainID {
		pe.chainState = chainOK
	} else {
		pe.chainState = chainWrong
	}
	pe.mu.Unlock()

	if id != c.chainID {
		if prev != chainWrong {
			c.log.Error("rpc endpoint is on the wrong chain and will not be used",
				"endpoint", pe.Label(), "want_chain_id", c.chainID, "got_chain_id", id)
		}
		return &Error{Kind: KindUnavailable, Endpoint: pe.Label(), Method: "eth_chainId",
			Message: fmt.Sprintf("endpoint reports chain id %d, want %d", id, c.chainID)}
	}
	if prev == chainWrong {
		c.log.Info("rpc endpoint is back on the configured chain", "endpoint", pe.Label())
	}
	return nil
}

// Do runs fn against the best endpoint and, if fn fails with an error that is
// the endpoint's fault (transport, HTTP status, rate limit, malformed data,
// access denied), against the next ones, each at most once. Errors that
// another endpoint would repeat (a reverted call, an unknown transaction) are
// returned immediately.
func (c *Client) Do(ctx context.Context, fn func(ctx context.Context, e *Endpoint) error, opts ...PickOption) error {
	tried := make([]*Endpoint, 0, len(c.eps))
	var lastErr error
	for range c.eps {
		ep, err := c.Pick(ctx, append(opts[:len(opts):len(opts)], Except(tried...))...)
		if err != nil {
			if lastErr != nil {
				return lastErr
			}
			return err
		}
		err = fn(ctx, ep)
		if err == nil || !retryElsewhere(err) {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		c.log.Warn("rpc call failed, trying next endpoint", "endpoint", ep.Label(), "error", err)
		lastErr = err
		tried = append(tried, ep)
	}
	return lastErr
}

// BlockNumber returns the latest block number, with failover.
func (c *Client) BlockNumber(ctx context.Context) (n uint64, err error) {
	err = c.Do(ctx, func(ctx context.Context, e *Endpoint) error {
		n, err = e.BlockNumber(ctx)
		return err
	})
	return n, err
}

// Decimals reads decimals() of an ERC-20 token, with failover.
func (c *Client) Decimals(ctx context.Context, token ethaddr.Address) (d uint8, err error) {
	err = c.Do(ctx, func(ctx context.Context, e *Endpoint) error {
		d, err = e.Decimals(ctx, token)
		return err
	})
	return d, err
}

// Symbol reads symbol() of an ERC-20 token, with failover.
func (c *Client) Symbol(ctx context.Context, token ethaddr.Address) (s string, err error) {
	err = c.Do(ctx, func(ctx context.Context, e *Endpoint) error {
		s, err = e.Symbol(ctx, token)
		return err
	})
	return s, err
}

// BalanceOf reads balanceOf(owner) of an ERC-20 token, with failover.
func (c *Client) BalanceOf(ctx context.Context, token, owner ethaddr.Address) (b *big.Int, err error) {
	err = c.Do(ctx, func(ctx context.Context, e *Endpoint) error {
		b, err = e.BalanceOf(ctx, token, owner)
		return err
	})
	return b, err
}

// TransactionReceipt fetches a receipt, with failover. ErrNotFound from one
// endpoint is returned as is (another endpoint is not asked: use Pick and
// Except for deliberate cross-checks).
func (c *Client) TransactionReceipt(ctx context.Context, txHash Hash) (r *Receipt, err error) {
	err = c.Do(ctx, func(ctx context.Context, e *Endpoint) error {
		r, err = e.TransactionReceipt(ctx, txHash)
		return err
	})
	return r, err
}

// Refresh checks every endpoint once, concurrently: chain id and latest
// block. It updates health, detects wrong-chain endpoints, and computes each
// endpoint's lag. Endpoints in cooldown are checked too, which is what makes
// fail-back prompt.
//
// Lag is measured against the height of the chain, and the height of the
// chain is not what one endpoint says it is: it is the highest block number
// that at least two of the answering endpoints have reached — the
// second-highest answer. (With a single answer it is that answer.) Measured
// against the highest answer instead, one endpoint that adds a thousand to
// its eth_blockNumber would make every honest endpoint "lagging" and itself
// the one Pick prefers. The consequences:
//
//   - With three or more endpoints, one that is behind the other two by more
//     than MaxLag is lagging; one that claims to be ahead of everybody is
//     simply not believed about it, and nobody lags because of it.
//   - With exactly two endpoints neither is ever lagging: when they are far
//     apart there is no telling which of them is right, and "the one that
//     says less" is not a rule an endpoint could not exploit. The caller
//     sees the two heights and does not go beyond what both have (the
//     watcher holds its head and says so).
func (c *Client) Refresh(ctx context.Context) {
	type result struct {
		block uint64
		ok    bool
	}
	results := make([]result, len(c.eps))
	var wg sync.WaitGroup
	for i, pe := range c.eps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.checkChain(ctx, pe); err != nil {
				return
			}
			n, err := pe.BlockNumber(ctx)
			if err != nil {
				return
			}
			results[i] = result{block: n, ok: true}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	var answers []uint64
	for _, r := range results {
		if r.ok {
			answers = append(answers, r.block)
		}
	}
	best := agreedHeight(answers)
	for i, pe := range c.eps {
		pe.mu.Lock()
		wasLagging := pe.lagKnown && pe.lag > c.maxLag
		if results[i].ok {
			pe.lag, pe.lagKnown = best-min(best, results[i].block), true
		} else {
			pe.lagKnown = false
		}
		isLagging := pe.lagKnown && pe.lag > c.maxLag
		lag := pe.lag
		pe.mu.Unlock()
		switch {
		case isLagging && !wasLagging:
			c.log.Warn("rpc endpoint is lagging", "endpoint", pe.Label(), "blocks_behind", lag)
		case !isLagging && wasLagging && results[i].ok:
			c.log.Info("rpc endpoint caught up", "endpoint", pe.Label())
		}
	}
}

// agreedHeight returns the highest block number that at least two of the
// given heights reach — the second-highest of them — or the only one, or 0
// for none.
func agreedHeight(heights []uint64) uint64 {
	var first, second uint64
	for _, h := range heights {
		switch {
		case h > first:
			first, second = h, first
		case h > second:
			second = h
		}
	}
	if len(heights) == 1 {
		return first
	}
	return second
}

// OthersUnreachable reports whether every configured endpoint other than
// the given ones is unreachable right now: its most recent call — by anyone:
// the caller, Refresh — got no answer at all (see Health.Unreachable). It is
// false when there is no other endpoint.
//
// It is what a caller that normally insists on a second endpoint may use to
// tell "nobody else is there" from "somebody else is there and I do not like
// what it says": an endpoint that answers — with an error, a rate limit,
// another chain's id, a height nobody else has — is not unreachable, and
// nothing an endpoint says makes another one so.
func (c *Client) OthersUnreachable(except ...*Endpoint) bool {
	others := 0
	for _, pe := range c.eps {
		skip := false
		for _, e := range except {
			skip = skip || e == pe.Endpoint
		}
		if skip {
			continue
		}
		others++
		if !pe.Health().Unreachable {
			return false
		}
	}
	return others > 0
}

// Monitor calls Refresh immediately and then every interval until ctx is
// done. It returns ctx.Err(). A non-positive interval selects
// DefaultMonitorInterval.
func (c *Client) Monitor(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultMonitorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		c.Refresh(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// EndpointState summarises an endpoint for display.
type EndpointState string

const (
	// StateHealthy: verified chain id, responding, not lagging.
	StateHealthy EndpointState = "healthy"
	// StateUnverified: no successful chain id check yet.
	StateUnverified EndpointState = "unverified"
	// StateFailing: in a cooldown after consecutive failures.
	StateFailing EndpointState = "failing"
	// StateLagging: responding, but too far behind the height two other
	// endpoints have reached.
	StateLagging EndpointState = "lagging"
	// StateWrongChain: reports a chain id other than the configured one.
	StateWrongChain EndpointState = "wrong_chain"
)

// EndpointStatus is one row of the node-health view.
type EndpointStatus struct {
	// URL is the redacted endpoint URL (scheme and host; see RedactURL).
	URL string
	// Position is the 1-based position in the configured order.
	Position int
	// State is the overall verdict; Healthy is State == StateHealthy.
	State   EndpointState
	Healthy bool
	// Active is true for the endpoint Pick would hand out right now.
	Active bool
	// ChainID is the chain id the endpoint last reported (0 if never).
	ChainID uint64
	// Latency is a moving average of successful round trips.
	Latency time.Duration
	// BlockNumber is the highest block the endpoint has reported and
	// BlockSeenAt when it last reported one; Height is the block number it
	// reported then (see Health).
	BlockNumber uint64
	BlockSeenAt time.Time
	Height      uint64
	// Unreachable: the endpoint's most recent call got no answer at all
	// (see Health.Unreachable).
	Unreachable bool
	// Lag is the number of blocks behind the height at least two endpoints
	// have reached, measured by the last Refresh (0 for an endpoint at or
	// above it); LagKnown is false if it has not been measured.
	Lag      uint64
	LagKnown bool
	// ConsecutiveFailures, LastError and LastErrorAt describe failures.
	ConsecutiveFailures int
	LastError           string
	LastErrorAt         time.Time
	// LastSuccessAt is the time of the last successful call.
	LastSuccessAt time.Time
	// RetryAt is when a failing endpoint will be tried again.
	RetryAt time.Time
	// Logs describes eth_getLogs support as learned so far.
	Logs LogsHealth
}

// Status returns a snapshot of every endpoint in configured order. It makes
// no network call.
func (c *Client) Status() []EndpointStatus {
	now := c.now()
	out := make([]EndpointStatus, len(c.eps))
	activeSet := false
	for i, pe := range c.eps {
		h := pe.Health()
		pe.mu.Lock()
		state, reported, lag, lagKnown := pe.chainState, pe.reportedID, pe.lag, pe.lagKnown
		pe.mu.Unlock()

		st := EndpointStatus{
			URL:                 pe.Label(),
			Position:            i + 1,
			ChainID:             reported,
			Latency:             h.Latency,
			BlockNumber:         h.BlockNumber,
			BlockSeenAt:         h.BlockSeenAt,
			Height:              h.Height,
			Unreachable:         h.Unreachable,
			Lag:                 lag,
			LagKnown:            lagKnown,
			ConsecutiveFailures: h.ConsecutiveFailures,
			LastError:           h.LastError,
			LastErrorAt:         h.LastErrorAt,
			LastSuccessAt:       h.LastSuccessAt,
			Logs:                h.Logs,
		}
		switch {
		case state == chainWrong:
			st.State = StateWrongChain
			st.LastError = fmt.Sprintf("reports chain id %d, want %d", reported, c.chainID)
		case now.Before(h.CooldownUntil):
			st.State = StateFailing
			st.RetryAt = h.CooldownUntil
		case state == chainUnknown:
			st.State = StateUnverified
		case lagKnown && lag > c.maxLag:
			st.State = StateLagging
		default:
			st.State = StateHealthy
		}
		st.Healthy = st.State == StateHealthy
		if st.Healthy && !activeSet {
			st.Active, activeSet = true, true
		}
		out[i] = st
	}
	return out
}
