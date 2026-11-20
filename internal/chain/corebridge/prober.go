package corebridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
)

// Prober defaults.
const (
	// DefaultProbeTimeout bounds a whole probe (at most six requests).
	DefaultProbeTimeout = 8 * time.Second
	// DefaultProbeRequestTimeout bounds each request of a probe.
	DefaultProbeRequestTimeout = 4 * time.Second
	// DefaultProbeMaxResponseBytes bounds each response body. A probe only
	// reads a chain id, a header, a few words of contract data and an
	// (empty) log list.
	DefaultProbeMaxResponseBytes = 256 << 10
	// DefaultProbeConcurrency is how many probes may run at once.
	DefaultProbeConcurrency = 4
)

// ProberOptions tunes a Prober. The zero value is ready for use.
type ProberOptions struct {
	// Timeout, RequestTimeout, MaxResponseBytes, Concurrency: see the
	// defaults.
	Timeout          time.Duration
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	Concurrency      int
	// DenyPrivateNetworks refuses to connect to loopback, private,
	// link-local, carrier-grade-NAT, multicast and unspecified addresses.
	// The check is made on the address actually dialled, after DNS
	// resolution, so a name that resolves to an internal address is
	// refused too. Off by default, because a node on localhost or on the
	// LAN is a legitimate — and common — endpoint for a self-hosted
	// gateway. Turn it on when the instance is reachable from the internet
	// before it is set up and no local node is used.
	DenyPrivateNetworks bool
	// HTTPClient replaces the HTTP client (tests). DenyPrivateNetworks is
	// then the caller's business.
	HTTPClient *http.Client
}

// Prober implements core.ChainProber with evm.Probe.
//
// # Exposure
//
// The core lets anyone call ProbeRPC until first-run setup is completed, so
// until then a Prober is an unauthenticated way to make this server send
// HTTP requests to a URL of the caller's choosing (server-side request
// forgery). What that is limited to:
//
//   - http and https only; no redirects are followed.
//   - The request is always a POST of a fixed JSON-RPC body (eth_chainId and
//     a few read-only calls). The caller controls the URL, nothing else: no
//     headers, no body, no method.
//   - At most six requests per probe, 4 s each, 8 s in total; responses are
//     read up to 256 KiB; at most four probes run at a time (further ones
//     are answered "busy" at once).
//   - Nothing of a response body that is not JSON-RPC is returned. What the
//     caller learns about an arbitrary host is: whether something accepted
//     the connection and how fast, the HTTP status class, and — if the host
//     answers in JSON-RPC shape — its chain id, block number, token facts
//     and JSON-RPC error messages (truncated).
//
// What remains: an attacker who reaches an instance before its owner has
// completed setup can use it to probe which hosts and ports are reachable
// from the server (including localhost and the internal network, unless
// DenyPrivateNetworks is set) and to send that fixed POST to them. They
// cannot read non-JSON-RPC responses, send chosen payloads, or reach
// non-HTTP services meaningfully. Completing setup closes the window: the
// probe is operator-only from then on.
type Prober struct {
	opts ProberOptions
	hc   *http.Client
	sem  chan struct{}
}

var _ core.ChainProber = (*Prober)(nil)

// NewProber creates a Prober.
func NewProber(opts ProberOptions) *Prober {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultProbeTimeout
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultProbeRequestTimeout
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = DefaultProbeMaxResponseBytes
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultProbeConcurrency
	}
	p := &Prober{opts: opts, hc: opts.HTTPClient, sem: make(chan struct{}, opts.Concurrency)}
	if p.hc == nil {
		p.hc = probeHTTPClient(opts)
	}
	return p
}

// probeHTTPClient builds the client probes use: no redirects, no connection
// reuse between probes of different hosts worth keeping, bounded handshakes,
// and — if requested — a dialer that refuses internal addresses.
func probeHTTPClient(opts ProberOptions) *http.Client {
	dialer := &net.Dialer{Timeout: opts.RequestTimeout}
	if opts.DenyPrivateNetworks {
		dialer.Control = denyPrivate
	}
	tr := &http.Transport{
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    opts.RequestTimeout,
		ResponseHeaderTimeout:  opts.RequestTimeout,
		MaxIdleConns:           4,
		IdleConnTimeout:        10 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
	}
	if !opts.DenyPrivateNetworks {
		// With the guard on, an environment proxy would be the address
		// dialled and hide the real destination from the check.
		tr.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// errPrivateAddress is the dial error for a refused destination.
var errPrivateAddress = errors.New("destination is a private or local address")

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// denyPrivate is a net.Dialer control function: it sees the literal IP
// address about to be connected to.
func denyPrivate(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return errPrivateAddress
	}
	ip := ap.Addr().Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip) {
		return errPrivateAddress
	}
	return nil
}

// Probe implements core.ChainProber. It reports plain facts; an endpoint
// that cannot be reached, is not an RPC endpoint, or is refused by policy is
// a result (Reachable false, Error set). The only error it returns is the
// caller's cancelled context.
//
// Error is also set when the endpoint works but does not serve eth_getLogs
// (common on free endpoints): the core's ProbeFacts has no field for that,
// and an operator needs to know that such an endpoint cannot detect payments
// on its own.
func (p *Prober) Probe(ctx context.Context, rpcURL string, asset core.Asset) (core.ProbeFacts, error) {
	var facts core.ProbeFacts
	switch u := strings.ToLower(strings.TrimSpace(rpcURL)); {
	case strings.HasPrefix(u, "ws://"), strings.HasPrefix(u, "wss://"):
		facts.Error = unsupportedScheme
		return facts, nil
	case !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://"):
		facts.Error = "the endpoint URL must start with http:// or https://"
		return facts, nil
	}
	token, err := parseAddress(asset.Contract)
	if err != nil {
		facts.Error = "the asset has no valid contract address"
		return facts, nil
	}

	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	default:
		facts.Error = "too many endpoint checks are running; try again in a moment"
		return facts, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()
	res := evm.ProbeWith(probeCtx, rpcURL, asset.ChainID, token, evm.ProbeOptions{
		Endpoint: evm.EndpointOptions{
			HTTPClient:           p.hc,
			Timeout:              p.opts.RequestTimeout,
			MaxResponseBytes:     p.opts.MaxResponseBytes,
			OmitResponseExcerpts: true,
		},
	})
	if err := ctx.Err(); err != nil {
		return core.ProbeFacts{}, err
	}

	facts.Reachable = res.Reach.OK
	facts.ChainID = res.Chain.ID
	facts.BlockNumber = res.Head.Number
	if res.Reach.Latency > 0 {
		facts.LatencyMs = max(int(res.Reach.Latency/time.Millisecond), 1)
	}
	facts.TokenFound = res.Token.CodePresent && res.Token.DecimalsRead
	facts.TokenDecimals = int(res.Token.Decimals)
	facts.TokenSymbol = res.Token.Symbol
	switch {
	case !res.Reach.OK:
		facts.Error = reachError(res)
	case !res.Chain.Matches:
		// Reported as a fact (ChainID); the core words the verdict.
	case res.Token.Checked && !facts.TokenFound:
		facts.Error = short(res.Token.Error)
	case res.Logs.Checked && !res.Logs.OK:
		facts.Error = "this endpoint does not serve eth_getLogs (" + short(res.Logs.Error) +
			"): it cannot detect payments by itself, but can be used as an additional endpoint"
	}
	return facts, nil
}

// reachError words why an endpoint could not be used at all.
func reachError(res evm.ProbeResult) string {
	msg := res.Reach.Error
	if strings.Contains(msg, errPrivateAddress.Error()) {
		return "this server does not probe private or local addresses"
	}
	switch res.Reach.Kind {
	case evm.KindTransport:
		return "cannot connect: " + short(strings.TrimPrefix(msg, "eth_chainId: transport: "))
	case evm.KindRateLimited:
		return "the endpoint is rate-limiting requests: " + short(msg)
	case evm.KindUnavailable:
		return "the endpoint refuses access (an API key may be required): " + short(msg)
	case evm.KindBadResponse:
		return "this is not a JSON-RPC endpoint"
	default:
		return short(msg)
	}
}

// short bounds a message for display.
func short(s string) string {
	const limit = 200
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	if s == "" {
		return "no further detail"
	}
	return s
}

// String makes accidental logging of a Prober harmless.
func (p *Prober) String() string {
	return fmt.Sprintf("corebridge.Prober(timeout=%s)", p.opts.Timeout)
}
