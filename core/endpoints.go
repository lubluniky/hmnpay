package core

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"github.com/lubluniky/hmnpay/core/endpointid"
)

// endpointIdentity returns what makes two RPC URLs one endpoint
// (endpointid.Identity: the canonical scheme, host and port — the same
// definition the chain adapter labels endpoints by). The path and the query —
// where providers put their keys — do not tell endpoints apart: two URLs
// that differ only there reach the same server, and would confirm each
// other's answers.
func endpointIdentity(raw string) string { return endpointid.Identity(raw) }

// providerDomain returns the part of a host name that names whoever runs it,
// as far as can be told from the name alone: its registrable domain —
// "binance.org" for bsc-dataseed1.binance.org and bsc-dataseed2.binance.org.
// It is a heuristic (no public suffix list is consulted): the last two labels,
// or three under a country code with a common second level ("example.co.uk").
// An IP address is its own domain. "" for an address of this machine
// (localhost, loopback), which is not compared: it is the operator's own.
func providerDomain(host string) string {
	host = endpointid.Host(host)
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return ""
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.IsLoopback() {
			return ""
		}
		return ip.String()
	}
	labels := strings.Split(host, ".")
	n := len(labels)
	if n < 2 {
		return host
	}
	if n >= 3 && len(labels[n-1]) == 2 && commonSecondLevel[labels[n-2]] {
		return strings.Join(labels[n-3:], ".")
	}
	return strings.Join(labels[n-2:], ".")
}

// commonSecondLevel are the second-level labels under which country-code
// domains are registered (co.uk, com.au, ne.jp, ...).
var commonSecondLevel = map[string]bool{"co": true, "com": true, "net": true, "org": true, "gov": true, "edu": true, "ac": true, "or": true, "ne": true, "go": true}

// SameProvider returns the configured RPC endpoints that look like one
// provider — whose hosts share a registrable domain (providerDomain) — in
// groups of two or more, each endpoint as scheme and host (no path, no key).
// hmnpay cannot know who runs an endpoint; it can say when two names look
// like one. Two endpoints of one provider are one opinion: what one of them
// says, the other is no independent check of. Empty when nothing looks so.
func (n NetworkSettings) SameProvider() [][]string {
	byDomain := map[string][]string{}
	var order []string
	for _, raw := range n.RPCURLs {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		d := providerDomain(u.Hostname())
		if d == "" {
			continue
		}
		if _, seen := byDomain[d]; !seen {
			order = append(order, d)
		}
		byDomain[d] = append(byDomain[d], endpointid.Identity(raw))
	}
	var out [][]string
	for _, d := range order {
		if len(byDomain[d]) > 1 {
			out = append(out, byDomain[d])
		}
	}
	return out
}

// Warnings returns what the operator should know about the network
// settings although they are valid: endpoints that look like one provider
// (SameProvider), in words.
func (n NetworkSettings) Warnings() []string {
	var out []string
	for _, g := range n.SameProvider() {
		out = append(out, fmt.Sprintf("%s look like the same provider: the second opinion they give each other is worth less. Configure endpoints of independent providers.", strings.Join(g, " and ")))
	}
	return out
}
