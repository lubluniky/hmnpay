// Package endpointid says what makes two RPC endpoint URLs one endpoint. It
// is the one definition used by the core's settings validation (two URLs of
// one endpoint are refused), the same-provider warning, the RPC client, and
// the chain watcher's labels — the names under which it remembers what an
// endpoint has done (scanned blocks, served logs) across restarts.
package endpointid

import (
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Identity returns the canonical form of an endpoint URL's identity:
// scheme://host, with :port unless it is the scheme's default. The path, the
// query and any user information — where providers put their keys — are not
// part of it: URLs that differ only there reach the same server. Spellings
// of one host are one: letter case, trailing dots, an internationalised
// name and its ASCII (punycode) form, the forms of one IP address (an
// IPv4-mapped IPv6 address is its IPv4 address), a port written with
// leading zeros. It returns "invalid-url" for a URL without a host.
//
// What it does not unify: names that differ in more than case and
// encoding (Unicode compatibility forms, a name and a CNAME of it, two
// names of one provider) — that is what the same-provider warning is for,
// as far as it can tell.
func Identity(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "invalid-url"
	}
	scheme := strings.ToLower(u.Scheme)
	host := Host(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if n, err := strconv.Atoi(port); err == nil {
		port = strconv.Itoa(n)
	}
	switch {
	case port == "", (scheme == "https" || scheme == "wss") && port == "443", (scheme == "http" || scheme == "ws") && port == "80":
		return scheme + "://" + host
	}
	return scheme + "://" + host + ":" + port
}

// Host returns the canonical form of a host name or IP address (without
// brackets): an IP address in its standard form, a name in lower case,
// without trailing dots, each label that is not ASCII in its punycode form
// ("xn--…").
func Host(h string) string {
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.Unmap().String()
	}
	h = strings.TrimRight(strings.ToLower(h), ".")
	labels := strings.Split(h, ".")
	for i, l := range labels {
		if !isASCII(l) {
			if enc, ok := punycode(l); ok {
				labels[i] = "xn--" + enc
			}
		}
	}
	return strings.Join(labels, ".")
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// punycode encodes a label by RFC 3492 (the Bootstring parameters of
// punycode).
func punycode(label string) (string, bool) {
	const (
		base, tmin, tmax, skew, damp = 36, 1, 26, 38, 700
		initialBias, initialN        = 72, 128
	)
	runes := []rune(label)
	var out []byte
	for _, r := range runes {
		if r < 0x80 {
			out = append(out, byte(r))
		}
	}
	b := len(out)
	h := b
	if b > 0 {
		out = append(out, '-')
	}
	digit := func(d int) byte {
		if d < 26 {
			return byte('a' + d)
		}
		return byte('0' + d - 26)
	}
	adapt := func(delta, numPoints int, first bool) int {
		if first {
			delta /= damp
		} else {
			delta /= 2
		}
		delta += delta / numPoints
		k := 0
		for delta > ((base-tmin)*tmax)/2 {
			delta /= base - tmin
			k += base
		}
		return k + (base-tmin+1)*delta/(delta+skew)
	}
	n, delta, bias := initialN, 0, initialBias
	for h < len(runes) {
		m := int(^uint(0) >> 1)
		for _, r := range runes {
			if int(r) >= n && int(r) < m {
				m = int(r)
			}
		}
		if (m-n)*(h+1) < 0 {
			return "", false // overflow
		}
		delta += (m - n) * (h + 1)
		n = m
		for _, r := range runes {
			if int(r) < n {
				delta++
			}
			if int(r) == n {
				q := delta
				for k := base; ; k += base {
					t := k - bias
					switch {
					case t < tmin:
						t = tmin
					case t > tmax:
						t = tmax
					}
					if q < t {
						break
					}
					out = append(out, digit(t+(q-t)%(base-t)))
					q = (q - t) / (base - t)
				}
				out = append(out, digit(q))
				bias = adapt(delta, h+1, h == b)
				delta = 0
				h++
			}
		}
		delta++
		n++
	}
	return string(out), true
}
