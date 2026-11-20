package webhook

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrInvalidURL is matched (with errors.Is) by every error [ValidateURL]
// returns for a URL that is malformed or uses a forbidden form (scheme,
// credentials, port, ambiguous host).
var ErrInvalidURL = errors.New("webhook: invalid endpoint URL")

// ErrBlockedDestination is matched (with errors.Is) by every error that
// refuses a destination address under the [EgressPolicy], both from
// [ValidateURL] and from the dial-time guard.
var ErrBlockedDestination = errors.New("webhook: destination blocked by egress policy")

// PolicyError explains why a URL or an address was refused. It matches
// [ErrInvalidURL] or [ErrBlockedDestination] with errors.Is. Its message
// is safe to show to the operator: it never contains the full URL, only
// the offending part.
type PolicyError struct {
	// Category is a short stable identifier: "scheme", "credentials",
	// "port", "host", "insecure_scheme", or an address category such as
	// "loopback", "link_local", "private", "metadata", "multicast".
	Category string
	// Msg is a human-readable explanation.
	Msg     string
	blocked bool
}

func (e *PolicyError) Error() string { return "webhook: " + e.Msg }

// Is makes PolicyError match its sentinel.
func (e *PolicyError) Is(target error) bool {
	if e.blocked {
		return target == ErrBlockedDestination
	}
	return target == ErrInvalidURL
}

func invalidURL(category, format string, args ...any) error {
	return &PolicyError{Category: category, Msg: fmt.Sprintf(format, args...)}
}

func blockedAddr(category, format string, args ...any) error {
	return &PolicyError{Category: category, Msg: fmt.Sprintf(format, args...), blocked: true}
}

// Resolver resolves host names for the dial-time guard. *net.Resolver
// implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// EgressPolicy decides which destinations the dispatcher may connect to.
//
// The webhook URL is chosen by whoever holds the webhooks:manage scope —
// the operator, but also any API key carrying that scope, which lives in a
// merchant integration and can leak. And the attempt is not blind: status
// code and the first 4 KiB of the response are stored and shown in the
// delivery log. An unrestricted dispatcher would therefore be a full-read
// SSRF primitive into the gateway's own network (cloud metadata, admin
// panels, the RPC node, databases with HTTP interfaces, the gateway's own
// API on localhost). The policy is enforced at dial time on every
// resolved address; [EgressPolicy.ValidateURL] applies the same rules
// statically so obviously bad URLs can be refused up front.
//
// Always blocked, whatever the options (see [EgressPolicy.CheckAddr]):
// unspecified (0.0.0.0/8, ::), multicast, broadcast and reserved IPv4
// (240.0.0.0/4), link-local (169.254.0.0/16, fe80::/10, which includes
// the 169.254.169.254 metadata service), the well-known cloud metadata
// addresses outside link-local (fd00:ec2::254, 100.100.100.200,
// 168.63.129.16), documentation, benchmarking-adjacent and IETF special
// ranges, IPv4-compatible IPv6 (::/96), Teredo, discard-only and ORCHID
// ranges. Addresses that embed an IPv4 address (IPv4-mapped, SIIT, NAT64
// 64:ff9b::/96, 6to4) are judged by the embedded address.
//
// Blocked unless allowed: loopback (127.0.0.0/8, ::1) and private networks
// (see AllowPrivateNetworks).
type EgressPolicy struct {
	// AllowPrivateNetworks permits RFC 1918 (10/8, 172.16/12,
	// 192.168/16), carrier-grade NAT (100.64/10), benchmarking
	// (198.18/15), IPv6 unique-local (fc00::/7), deprecated site-local
	// (fec0::/10) and local-use NAT64 (64:ff9b:1::/48) destinations.
	//
	// The default is false (fail closed). Self-hosters do deliver to
	// internal services, but the people who can set an endpoint URL are
	// not only the operator (see the type documentation), and the
	// response is reflected back to them. Delivering into the private
	// network is therefore a decision the operator must take explicitly
	// in configuration, where an API caller cannot reach it. Operators who
	// only need one internal service should prefer AllowedPrefixes.
	AllowPrivateNetworks bool

	// AllowedPrefixes are operator-configured networks that are permitted
	// even though they are loopback or private — for example
	// 127.0.0.1/32 when the merchant application runs on the same host,
	// or 10.0.4.0/24 for one internal subnet. It cannot permit the
	// categories that are always blocked (unspecified, multicast,
	// broadcast/reserved, link-local, cloud metadata).
	AllowedPrefixes []netip.Prefix

	// RequireHTTPS refuses plain-http endpoints. The default (false)
	// allows http: deliveries are authenticated by the HMAC signature, so
	// an on-path attacker can read but not forge them, and internal or
	// development receivers often have no certificate. Operators who
	// deliver over the public internet should enable it.
	RequireHTTPS bool

	// AllowedPorts, when non-empty, restricts destination ports (the
	// URL's explicit port, or 80/443 by scheme). Empty allows every port
	// from 1 to 65535: receivers commonly listen on 8080, 8443, 3000 …,
	// and the address rules above are what keeps the dispatcher out of
	// internal services.
	AllowedPorts []int

	// Resolver resolves host names at dial time. Nil uses
	// net.DefaultResolver.
	Resolver Resolver
}

// Address categories that no option can unlock.
var (
	alwaysBlocked = []struct {
		category string
		prefixes []netip.Prefix
	}{
		{"metadata", prefixes(
			"fd00:ec2::254/128",   // AWS IMDS over IPv6 (inside ULA)
			"100.100.100.200/32",  // Alibaba Cloud metadata (inside CGNAT)
			"168.63.129.16/32",    // Azure wire server (public address)
			"169.254.169.254/32",  // AWS, GCP, Azure, OCI, DO … (link-local anyway)
			"169.254.170.2/32",    // ECS task metadata (link-local anyway)
			"fd20:ce::254/128",    // GCP metadata over IPv6
			"fe80::a9fe:a9fe/128", // OpenStack/GCP link-local alias
		)},
		{"unspecified", prefixes("0.0.0.0/8", "::/128")},
		{"link_local", prefixes("169.254.0.0/16", "fe80::/10")},
		{"multicast", prefixes("224.0.0.0/4", "ff00::/8")},
		{"broadcast", prefixes("255.255.255.255/32")},
		{"reserved", prefixes(
			"240.0.0.0/4",     // future use (Linux may route it)
			"192.0.0.0/24",    // IETF protocol assignments (NAT64 discovery …)
			"192.0.2.0/24",    // TEST-NET-1
			"198.51.100.0/24", // TEST-NET-2
			"203.0.113.0/24",  // TEST-NET-3
			"192.88.99.0/24",  // deprecated 6to4 relay anycast
			"::/96",           // IPv4-compatible IPv6 (deprecated); also hides ::1-like forms
			"100::/64",        // discard-only
			"2001::/32",       // Teredo: tunnels to an obfuscated IPv4 address
			"2001:10::/28",    // ORCHID
			"2001:20::/28",    // ORCHIDv2
			"2001:db8::/32",   // documentation
			"3fff::/20",       // documentation (RFC 9637)
		)},
	}
	loopbackPrefixes = prefixes("127.0.0.0/8", "::1/128")
	privatePrefixes  = prefixes(
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"100.64.0.0/10",  // carrier-grade NAT
		"198.18.0.0/15",  // benchmarking, used inside some datacenters
		"fc00::/7",       // unique local
		"fec0::/10",      // site-local (deprecated)
		"64:ff9b:1::/48", // local-use NAT64
	)
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	siitPrefix  = netip.MustParsePrefix("::ffff:0:0:0/96") // IPv4-translated (SIIT)
	sixToFour   = netip.MustParsePrefix("2002::/16")
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func containsAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// CheckAddr reports whether the dispatcher may connect to addr. It returns
// nil or an error matching [ErrBlockedDestination].
func (p EgressPolicy) CheckAddr(addr netip.Addr) error {
	return p.checkAddr(addr, 0)
}

func (p EgressPolicy) checkAddr(addr netip.Addr, depth int) error {
	if !addr.IsValid() {
		return blockedAddr("invalid", "invalid address")
	}
	if addr.Zone() != "" {
		return blockedAddr("link_local", "scoped (zoned) addresses are not allowed")
	}
	a := addr.Unmap()                        // ::ffff:a.b.c.d is a.b.c.d
	isLoopback6 := a == netip.IPv6Loopback() // inside ::/96, but it is plain loopback
	for _, c := range alwaysBlocked {
		if isLoopback6 {
			break
		}
		if containsAny(c.prefixes, a) {
			return blockedAddr(c.category, "%s is a %s address", a, strings.ReplaceAll(c.category, "_", "-"))
		}
	}
	// Addresses that carry an IPv4 address are judged by it: a translator
	// on the path would deliver to the embedded address.
	if embedded, ok := embeddedIPv4(a); ok && depth == 0 {
		if err := p.checkAddr(embedded, depth+1); err != nil {
			pe := err.(*PolicyError)
			return blockedAddr(pe.Category, "%s embeds %s: %s", a, embedded, pe.Msg)
		}
		return nil
	}
	allowed := containsAny(p.AllowedPrefixes, a)
	if containsAny(loopbackPrefixes, a) && !allowed {
		return blockedAddr("loopback", "%s is a loopback address", a)
	}
	if containsAny(privatePrefixes, a) && !allowed && !p.AllowPrivateNetworks {
		return blockedAddr("private", "%s is a private-network address (set AllowPrivateNetworks or AllowedPrefixes to permit it)", a)
	}
	return nil
}

// embeddedIPv4 extracts the IPv4 address carried by SIIT (::ffff:0:a.b.c.d),
// well-known NAT64 (64:ff9b::a.b.c.d) and 6to4 (2002:aabb:ccdd::/48)
// addresses.
func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	switch {
	case nat64Prefix.Contains(a), siitPrefix.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case sixToFour.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

func (p EgressPolicy) checkPort(port int) error {
	if port < 1 || port > 65535 {
		return invalidURL("port", "port %d is out of range", port)
	}
	if len(p.AllowedPorts) > 0 && !slices.Contains(p.AllowedPorts, port) {
		return blockedAddr("port", "port %d is not allowed by the egress policy", port)
	}
	return nil
}

// ValidateURL checks an endpoint URL against the default policy
// ([EgressPolicy] zero value). See [EgressPolicy.ValidateURL].
func ValidateURL(raw string) error { return EgressPolicy{}.ValidateURL(raw) }

// ValidateURL checks an endpoint URL statically, without resolving it: the
// scheme must be http or https (https only with RequireHTTPS), the URL
// must not carry credentials, the port must be allowed, an IP-literal host
// must pass [EgressPolicy.CheckAddr], and host names must be plain ASCII
// (punycode for IDNs), not "localhost", and not a numeric form that some
// resolvers read as an IPv4 address ("2130706433", "0x7f.1", "0177.0.0.1").
//
// It is meant for the API layer, to reject obviously bad URLs with a clear
// message when an endpoint is created. It cannot catch a name that
// resolves to a blocked address — the dial-time guard does that on every
// connection.
func (p EgressPolicy) ValidateURL(raw string) error {
	_, _, err := p.parseURL(raw)
	return err
}

// parseURL validates raw and returns it parsed with its numeric port.
func (p EgressPolicy) parseURL(raw string) (*url.URL, int, error) {
	if len(raw) > 2048 {
		return nil, 0, invalidURL("url", "URL is longer than 2048 characters")
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return r <= 0x20 || r == 0x7f }) {
		return nil, 0, invalidURL("url", "URL must not contain spaces or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, 0, invalidURL("url", "URL cannot be parsed")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if p.RequireHTTPS {
			return nil, 0, invalidURL("insecure_scheme", "plain http is not allowed; use https")
		}
	default:
		return nil, 0, invalidURL("scheme", "URL scheme must be http or https")
	}
	if u.Opaque != "" || u.Host == "" {
		return nil, 0, invalidURL("url", "URL must be absolute, like https://example.com/webhooks")
	}
	if u.User != nil {
		return nil, 0, invalidURL("credentials", "URL must not contain a user name or password")
	}
	port := 443
	if strings.EqualFold(u.Scheme, "http") {
		port = 80
	}
	if ps := u.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil {
			return nil, 0, invalidURL("port", "port is not a number")
		}
		port = n
	}
	if err := p.checkPort(port); err != nil {
		return nil, 0, err
	}
	if err := p.checkHost(u.Hostname()); err != nil {
		return nil, 0, err
	}
	return u, port, nil
}

// checkHost applies the static host rules.
func (p EgressPolicy) checkHost(host string) error {
	if host == "" {
		return invalidURL("host", "URL has no host")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return p.CheckAddr(addr)
	}
	if strings.Contains(host, ":") || strings.Contains(host, "%") {
		return invalidURL("host", "host is not a valid IP address or name")
	}
	for _, r := range host {
		if r >= 0x80 {
			return invalidURL("host", "host must be ASCII; use the punycode (xn--) form of international names")
		}
	}
	name := strings.TrimSuffix(strings.ToLower(host), ".")
	if looksNumeric(name) {
		return invalidURL("host", "host %q is a non-canonical numeric address; write the address as a dotted quad", host)
	}
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		// RFC 6761: always loopback. Allowed only when loopback is.
		if p.CheckAddr(netip.MustParseAddr("127.0.0.1")) != nil && p.CheckAddr(netip.IPv6Loopback()) != nil {
			return blockedAddr("loopback", "localhost is a loopback address")
		}
	}
	return nil
}

// looksNumeric reports whether a host name could be read as an IPv4
// address by inet_aton-style parsers (decimal, octal or hex parts, fewer
// than four parts). A real DNS name never has an all-numeric last label.
func looksNumeric(name string) bool {
	labels := strings.Split(name, ".")
	last := labels[len(labels)-1]
	if last == "" {
		return false
	}
	if l := strings.TrimPrefix(last, "0x"); l != last {
		return l == "" || strings.Trim(l, "0123456789abcdef") == ""
	}
	return strings.Trim(last, "0123456789") == ""
}

// ---- dial-time guard ----

// dialFunc dials one already-checked address.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// guard is the DialContext of the dispatcher's transport. It resolves the
// host itself, checks every resolved address and dials the checked IP —
// never the name — so the address that is checked is the address that is
// connected to (no DNS-rebinding window). The net.Dialer it wraps checks
// the socket address once more in Control.
type guard struct {
	policy   EgressPolicy
	resolver Resolver
	dial     dialFunc
}

func newGuard(p EgressPolicy, dial dialFunc) *guard {
	g := &guard{policy: p, resolver: p.Resolver, dial: dial}
	if g.resolver == nil {
		g.resolver = net.DefaultResolver
	}
	if g.dial == nil {
		d := &net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
			Control:   controlFunc(p),
		}
		g.dial = d.DialContext
	}
	return g
}

// dialTimeout bounds connect and TLS handshake; the attempt deadline
// usually ends them sooner.
const dialTimeout = 10 * time.Second

// controlFunc re-checks the address of every socket right before
// connect(2): defence in depth should any path reach the dialer with an
// unchecked address.
func controlFunc(p EgressPolicy) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return blockedAddr("invalid", "cannot parse socket address")
		}
		if err := p.checkPort(int(ap.Port())); err != nil {
			return err
		}
		return p.CheckAddr(ap.Addr())
	}
}

func (g *guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, invalidURL("host", "cannot split host and port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, invalidURL("port", "port is not a number")
	}
	if err := g.policy.checkPort(port); err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		if err := g.policy.checkHost(host); err != nil {
			return nil, err
		}
		lookupNet := "ip"
		switch network {
		case "tcp4":
			lookupNet = "ip4"
		case "tcp6":
			lookupNet = "ip6"
		}
		addrs, err = g.resolver.LookupNetIP(ctx, lookupNet, host)
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
		}
	}
	// A name that resolves to any blocked address is refused as a whole:
	// mixing a public and an internal answer is a rebinding pattern, and
	// dialling "the good one" would make the outcome depend on ordering.
	for _, a := range addrs {
		if err := g.policy.CheckAddr(a); err != nil {
			return nil, err
		}
	}
	var lastErr error
	for i, a := range addrs {
		dctx := ctx
		// Give each remaining address a fair share of the time left, as
		// net.Dialer does, so one black-holed address cannot eat it all.
		if dl, ok := ctx.Deadline(); ok && len(addrs)-i > 1 {
			share := time.Until(dl) / time.Duration(len(addrs)-i)
			if share < 2*time.Second {
				share = 2 * time.Second
			}
			var cancel context.CancelFunc
			dctx, cancel = context.WithTimeout(ctx, share)
			defer cancel()
		}
		conn, err := g.dial(dctx, network, netip.AddrPortFrom(a.Unmap(), uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// newTransport returns the dispatcher's HTTP transport: every connection
// goes through the egress guard; proxies from the environment are ignored
// (a proxy would make the guard check the proxy's address, not the
// destination's); TLS is verified with TLS 1.2 as the minimum; response
// headers are bounded; idle connections are kept briefly for reuse.
func newTransport(p EgressPolicy, dial dialFunc) *http.Transport {
	g := newGuard(p, dial)
	return &http.Transport{
		Proxy:       nil,
		DialContext: g.DialContext,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    dialTimeout,
		ResponseHeaderTimeout:  dialTimeout,
		ExpectContinueTimeout:  0,
		MaxResponseHeaderBytes: 32 << 10,
		DisableCompression:     true, // keep the stored body raw; no decompression bombs
		MaxIdleConns:           64,
		MaxIdleConnsPerHost:    4,
		MaxConnsPerHost:        0,
		IdleConnTimeout:        60 * time.Second,
	}
}
