package httpapi

import (
	"container/list"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Rate is a token bucket: Burst requests at once, refilled by one token
// every Every. A negative Burst disables the limit.
type Rate struct {
	Burst int
	Every time.Duration
}

func (r Rate) or(def Rate) Rate {
	if r.Burst == 0 && r.Every == 0 {
		return def
	}
	if r.Every <= 0 {
		r.Every = def.Every
	}
	return r
}

// Limits configures the in-memory rate limiters. Zero fields take the
// defaults documented on each field.
type Limits struct {
	// LoginPerIP limits login attempts per client IP. Default 10 burst,
	// one more every 12s (≈5/min sustained).
	LoginPerIP Rate
	// LoginPerAccount limits login attempts per submitted email, across all
	// IPs. Default 20 burst, one more every 30s (≈120/h). Successful
	// logins are refunded to both buckets.
	LoginPerAccount Rate
	// LoginGlobal limits login attempts instance-wide, across all IPs and
	// emails, for browsers that have not logged in here before. It bounds
	// what a distributed burst of made-up logins can cost (each attempt
	// runs the password KDF; the core additionally runs only a few at a
	// time). Default 30 burst, one more every 2s (≈30/min sustained).
	//
	// A browser that logged in successfully before carries a signed
	// "known device" cookie and is exempt from this bucket and from
	// LoginPerAccount — so neither a flood of anonymous attempts nor
	// someone hammering the operator's email can lock the operator out of
	// a browser they have used. It still counts against LoginPerIP and
	// LoginPerDevice.
	LoginGlobal Rate
	// LoginPerDevice limits login attempts per "known device" marker,
	// across all IPs. A known device skips LoginPerAccount and
	// LoginGlobal; this bucket is what it gets instead, so that somebody
	// holding one valid marker cannot spread attempts over many addresses
	// to keep the password KDF busy. Default 10 burst, one more every 30s
	// (≈120/h). Successful logins are refunded.
	LoginPerDevice Rate
	// AuthFailuresPerIP limits requests carrying invalid credentials (bad
	// API key or session token, wrong setup token) per IP. Default 30
	// burst, one every 2s.
	AuthFailuresPerIP Rate
	// SetupPerIP limits the setup wizard's endpoints per IP. Default 20
	// burst, one every 3s.
	SetupPerIP Rate
	// PublicReadPerIP limits public checkout reads and stream connections
	// per IP. Default 120 burst, one every 250ms.
	PublicReadPerIP Rate
	// LinkPaymentPerIP limits payments created from public payment links
	// per IP. Every one allocates a deposit address and joins the chain
	// watch set, so it is strict. Default 5 burst, one every 60s.
	LinkPaymentPerIP Rate
	// LinkPaymentPerIPPerLink limits payments one IP creates through one
	// link. It is what keeps a single client from holding several of a
	// limited link's uses at once (see the core's link reservation rule).
	// Default 2 burst, one every 5 minutes.
	LinkPaymentPerIPPerLink Rate
	// LinkPaymentPerLink caps payment creation per link, across all IPs: a
	// distributed flood on one link exhausts that link's budget, not every
	// link's. Default 60 burst, one every 5s (≈720/h).
	LinkPaymentPerLink Rate
	// LinkPaymentGlobal caps link payment creation instance-wide as a last
	// backstop. Default 300 burst, one every second (≈3600/h); the core
	// separately refuses link payments when the chain watch list is full.
	LinkPaymentGlobal Rate
	// MaxKeys bounds the number of keys each limiter remembers; the least
	// recently used key is evicted beyond it. Default 10000.
	MaxKeys int
}

func (l Limits) withDefaults() Limits {
	l.LoginPerIP = l.LoginPerIP.or(Rate{Burst: 10, Every: 12 * time.Second})
	l.LoginPerAccount = l.LoginPerAccount.or(Rate{Burst: 20, Every: 30 * time.Second})
	l.LoginGlobal = l.LoginGlobal.or(Rate{Burst: 30, Every: 2 * time.Second})
	l.LoginPerDevice = l.LoginPerDevice.or(Rate{Burst: 10, Every: 30 * time.Second})
	l.AuthFailuresPerIP = l.AuthFailuresPerIP.or(Rate{Burst: 30, Every: 2 * time.Second})
	l.SetupPerIP = l.SetupPerIP.or(Rate{Burst: 20, Every: 3 * time.Second})
	l.PublicReadPerIP = l.PublicReadPerIP.or(Rate{Burst: 120, Every: 250 * time.Millisecond})
	l.LinkPaymentPerIP = l.LinkPaymentPerIP.or(Rate{Burst: 5, Every: time.Minute})
	l.LinkPaymentPerIPPerLink = l.LinkPaymentPerIPPerLink.or(Rate{Burst: 2, Every: 5 * time.Minute})
	l.LinkPaymentPerLink = l.LinkPaymentPerLink.or(Rate{Burst: 60, Every: 5 * time.Second})
	l.LinkPaymentGlobal = l.LinkPaymentGlobal.or(Rate{Burst: 300, Every: time.Second})
	if l.MaxKeys <= 0 {
		l.MaxKeys = 10000
	}
	return l
}

type limiters struct {
	loginIP, loginAccount, loginGlobal, loginDevice *limiter
	authFail, setup, publicRead                     *limiter
	linkIP, linkIPLink, linkPerLink, linkGlobal     *limiter
}

func newLimiters(l Limits, now func() time.Time) *limiters {
	mk := func(r Rate) *limiter { return newLimiter(r, l.MaxKeys, now) }
	return &limiters{
		loginIP:      mk(l.LoginPerIP),
		loginAccount: mk(l.LoginPerAccount),
		loginGlobal:  mk(l.LoginGlobal),
		loginDevice:  mk(l.LoginPerDevice),
		authFail:     mk(l.AuthFailuresPerIP),
		setup:        mk(l.SetupPerIP),
		publicRead:   mk(l.PublicReadPerIP),
		linkIP:       mk(l.LinkPaymentPerIP),
		linkIPLink:   mk(l.LinkPaymentPerIPPerLink),
		linkPerLink:  mk(l.LinkPaymentPerLink),
		linkGlobal:   mk(l.LinkPaymentGlobal),
	}
}

// limiter is a set of token buckets keyed by string with bounded memory:
// an LRU list evicts the least recently used key when MaxKeys is reached.
// Evicting a key forgets its debt, so the bound must comfortably exceed
// the number of concurrently active clients; IPv6 clients are keyed by
// their /64 to keep one host from minting unlimited keys.
type limiter struct {
	mu   sync.Mutex
	rate Rate
	max  int
	now  func() time.Time
	ll   *list.List // front = most recently used
	m    map[string]*list.Element
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time
}

func newLimiter(r Rate, maxKeys int, now func() time.Time) *limiter {
	return &limiter{rate: r, max: maxKeys, now: now, ll: list.New(), m: map[string]*list.Element{}}
}

// get returns the refilled bucket for key, creating it (and evicting the
// LRU key if needed). Callers hold mu.
func (l *limiter) get(key string) *bucket {
	now := l.now()
	if el, ok := l.m[key]; ok {
		l.ll.MoveToFront(el)
		b := el.Value.(*bucket)
		if elapsed := now.Sub(b.last); elapsed > 0 {
			b.tokens = min(float64(l.rate.Burst), b.tokens+float64(elapsed)/float64(l.rate.Every))
			b.last = now
		}
		return b
	}
	for len(l.m) >= l.max {
		back := l.ll.Back()
		l.ll.Remove(back)
		delete(l.m, back.Value.(*bucket).key)
	}
	b := &bucket{key: key, tokens: float64(l.rate.Burst), last: now}
	l.m[key] = l.ll.PushFront(b)
	return b
}

// take consumes one token for key. It returns nil or a 429 error carrying
// the wait until the next token.
func (l *limiter) take(key string) error {
	if l.rate.Burst < 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(key)
	if b.tokens >= 1 {
		b.tokens--
		return nil
	}
	return errRateLimited(time.Duration((1 - b.tokens) * float64(l.rate.Every)))
}

// allow is take; the name reads better at call sites that only gate.
func (l *limiter) allow(key string) error { return l.take(key) }

// refund gives back one token (capped at the burst).
func (l *limiter) refund(key string) {
	if l.rate.Burst < 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.m[key]; ok {
		b := el.Value.(*bucket)
		b.tokens = min(float64(l.rate.Burst), b.tokens+1)
	}
}

func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// streamLimiter caps concurrent event streams per IP, per payment and in
// total. Its maps hold only IPs and payments with an open stream, so they
// are bounded by the total cap. The per-payment cap is what keeps one
// valid payment id, opened from many addresses, from occupying every slot;
// the per-IP share of it (perIPPayment) is what keeps a couple of
// addresses from occupying one payment's every slot and shutting its payer
// out of live updates.
type streamLimiter struct {
	mu           sync.Mutex
	perIP        int
	perPayment   int
	perIPPayment int
	maxTotal     int
	total        int
	byIP         map[string]int
	byPayment    map[string]int
	byIPPayment  map[string]int
}

func newStreamLimiter(perIP, perPayment, perIPPayment, maxTotal int) *streamLimiter {
	return &streamLimiter{
		perIP: perIP, perPayment: perPayment, perIPPayment: perIPPayment, maxTotal: maxTotal,
		byIP: map[string]int{}, byPayment: map[string]int{}, byIPPayment: map[string]int{},
	}
}

// acquire reserves a stream slot for the client key and the payment;
// release must be called once.
func (s *streamLimiter) acquire(key, paymentID string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pair := key + "\x00" + paymentID
	if s.total >= s.maxTotal || s.byIP[key] >= s.perIP ||
		s.byPayment[paymentID] >= s.perPayment || s.byIPPayment[pair] >= s.perIPPayment {
		return nil, false
	}
	s.total++
	s.byIP[key]++
	s.byPayment[paymentID]++
	s.byIPPayment[pair]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.total--
			if s.byIP[key]--; s.byIP[key] <= 0 {
				delete(s.byIP, key)
			}
			if s.byPayment[paymentID]--; s.byPayment[paymentID] <= 0 {
				delete(s.byPayment, paymentID)
			}
			if s.byIPPayment[pair]--; s.byIPPayment[pair] <= 0 {
				delete(s.byIPPayment, pair)
			}
		})
	}, true
}

func (s *streamLimiter) open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// ---- client identity ----

// clientIP returns the address of the client. The socket peer is used
// unless it is a trusted proxy; then X-Forwarded-For is walked from the
// right, skipping trusted proxies, and the first untrusted hop is the
// client (headers further left are client-controlled and ignored).
// X-Real-IP is consulted only from a trusted peer without X-Forwarded-For.
func (h *Handler) clientIP(r *http.Request) netip.Addr {
	peer := parseRemote(r.RemoteAddr)
	if !peer.IsValid() || !h.trusted(peer) {
		if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" {
			// A reverse proxy in front of us that is not listed as trusted:
			// every client then looks like the proxy, and all of them share
			// one rate-limit key. Say so once; the peer is the best guess
			// for which address to add to TrustedProxies.
			h.warnXFF.Do(func() {
				if h.log == nil {
					return
				}
				h.log.Warn("a request carried X-Forwarded-For / X-Real-IP from a peer that is not a trusted proxy; the header was ignored. "+
					"If hmnpay runs behind a reverse proxy, list the proxy in the trusted proxies setting — otherwise all clients share one rate-limit key",
					slog.String("peer", peer.String()))
			})
		}
		return peer
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		hops := strings.Split(strings.Join(xff, ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				return peer // malformed chain: do not guess
			}
			a = a.Unmap()
			if !h.trusted(a) {
				return a
			}
			peer = a
		}
		return peer
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		if a, err := netip.ParseAddr(xr); err == nil {
			return a.Unmap()
		}
	}
	return peer
}

func parseRemote(s string) netip.Addr {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap()
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func (h *Handler) trusted(a netip.Addr) bool {
	for _, p := range h.opts.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// rateKey is the limiter key of the request's client: the IPv4 address or
// the IPv6 /64.
func (h *Handler) rateKey(r *http.Request) string {
	return ipKey(info(r).ip)
}

func ipKey(a netip.Addr) string {
	if !a.IsValid() {
		return "unknown"
	}
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// requestIsHTTPS reports whether the client reached us over TLS.
func (h *Handler) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	peer := parseRemote(r.RemoteAddr)
	return peer.IsValid() && h.trusted(peer) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}
