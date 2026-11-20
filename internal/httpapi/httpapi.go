// Package httpapi is the HTTP adapter of hmnpay: the versioned REST API
// (/api/v1), the public hosted-checkout endpoints with their server-sent
// event stream, and the health probes.
//
// It owns transport only — routing, authentication extraction, decoding,
// encoding, limits, headers and streaming. Every business rule and every
// authorisation decision lives in [core.Service]: a handler authenticates
// the caller into a [core.Actor], attaches it with [core.WithActor] and
// calls exactly one Service method.
//
// One API serves every client. Merchant servers, bots and CLIs authenticate
// with "Authorization: Bearer hmn_…" (an API key, limited to its scopes by
// the core) or "Authorization: Bearer hmns_…" (an operator session token
// obtained from POST /api/v1/auth/login with "issue_token": true). The web
// dashboard uses the same routes with a session cookie, which is the only
// ambient credential and therefore the only one subject to CSRF checks.
//
// The handler serves /api/, /healthz and /readyz. Everything else goes to
// [Options.Fallback] (the web UI, mounted by the wiring) or gets a JSON 404.
package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// Options configures the HTTP adapter. The zero value is usable for local
// development; production wiring should at least set PublicURL, Logger and,
// behind a reverse proxy, TrustedProxies.
type Options struct {
	// PublicURL is the external base URL of the instance
	// ("https://pay.example.com"). It decides whether the session cookie is
	// Secure (and named "__Host-hmn_session"), its origin is trusted for
	// cookie-authenticated unsafe requests, and it is the default public URL
	// submitted by the setup wizard. When empty, the cookie is Secure only if
	// the request arrived over TLS (directly, or per X-Forwarded-Proto from a
	// trusted proxy), and CSRF checks compare Origin with Host.
	PublicURL string

	// AllowedHosts lists additional host names (without port) that requests
	// may carry in their Host header, e.g. an internal name a load
	// balancer's health check uses. The server always accepts the host of
	// PublicURL, "localhost" and IP-address literals, and rejects every
	// other Host with 421 host_not_allowed — also when PublicURL is unset,
	// and for the paths of the Fallback (the web interface) as for the API.
	// That closes DNS rebinding: a page on an attacker's domain that
	// re-resolves to this server still sends the attacker's host name. A
	// single "*" entry disables the check.
	AllowedHosts []string

	// DeviceKey signs the "known device" marker cookie that exempts a
	// browser which has logged in before from the shared login limits (see
	// [Limits.LoginGlobal]). At least 32 random bytes, stable across
	// restarts; when empty a key is generated at start, so markers do not
	// survive a restart (browsers then count as new devices once). The key
	// never needs rotating after a password reset: markers are bound to
	// the account's credentials and die with the old password.
	DeviceKey []byte

	// TrustedOrigins are additional origins ("http://localhost:3000") whose
	// browser pages may make cookie-authenticated unsafe requests, e.g. a
	// dashboard dev server on another port. Same-origin requests never need
	// to be listed.
	TrustedOrigins []string

	// CORSOrigins enables cross-origin access to the authenticated API for
	// these exact origins. Credentials (cookies) are never allowed
	// cross-origin: such clients must use bearer tokens. Off by default.
	CORSOrigins []string

	// PublicCORS sends "Access-Control-Allow-Origin: *" (without
	// credentials) on /api/v1/public/*, so a merchant can embed the checkout
	// data or the live stream in their own site. The data there is already
	// readable by anyone holding the unguessable payment id or link slug, so
	// this widens who can *script* against it, not what is disclosed. Off by
	// default.
	PublicCORS bool

	// TrustedProxies lists the reverse proxies (Caddy, nginx, Traefik) whose
	// X-Forwarded-For / X-Real-IP / X-Forwarded-Proto headers are believed.
	// Requests from any other peer use the socket address. Empty (the
	// default) trusts nobody. See [ParsePrefixes].
	TrustedProxies []netip.Prefix

	// Logger receives the access log and internal errors. Defaults to
	// slog.Default().
	Logger *slog.Logger

	// Fallback serves every request outside /api/, /healthz and /readyz —
	// the seam for the web UI (static files, SPA or a reverse proxy to
	// Next.js). Nil answers such requests with a JSON 404. The Host rule
	// (AllowedHosts) is applied before the Fallback is reached; the API's
	// security headers are not applied to its responses, it sets its own.
	Fallback http.Handler

	// Limits tunes rate limiting; zero fields take defaults.
	Limits Limits

	// Stream tunes the checkout event stream; zero fields take defaults.
	Stream StreamOptions

	// WriteTimeout bounds how long a non-streaming response may take to
	// write (set per request via http.ResponseController, so it does not
	// affect the event stream). Default 30s.
	WriteTimeout time.Duration

	// Version and Commit identify the build; GET /api/v1/instance reports
	// them to authenticated callers (the dashboard shows the version). No
	// endpoint of this handler sends them to anonymous callers. That keeps
	// the API from announcing its version; it does not make the version a
	// secret, because a binary with the web interface serves files whose
	// names are content hashes of a public source tree. Empty reads as
	// "dev" and "unknown".
	Version string
	Commit  string
}

// Default body limits.
const (
	defaultBodyLimit = 64 << 10  // ordinary JSON requests
	smallBodyLimit   = 8 << 10   // login, public link payments, empty-ish actions
	largeBodyLimit   = 768 << 10 // merchant identity (logo may be a 512 KB data: URL)
)

// Handler is the HTTP adapter. It implements http.Handler. Call Close when
// the server shuts down so open event streams end promptly (NewServer
// arranges that).
type Handler struct {
	svc  *core.Service
	opts Options
	log  *slog.Logger
	mux  *http.ServeMux

	cop          *http.CrossOriginProtection
	publicOrigin string // scheme://host of PublicURL, "" if unset
	publicHost   string // host name of PublicURL, lower case, "" if unset
	allowedHosts map[string]bool
	anyHost      bool
	deviceKey    []byte
	warnXFF      sync.Once
	secureFixed  *bool // cookie Secure decided by PublicURL; nil = per request
	corsOrigins  map[string]bool

	lim     *limiters
	streams *streamLimiter

	done      chan struct{}
	closeOnce sync.Once

	now func() time.Time
}

// New returns the HTTP adapter for svc.
func New(svc *core.Service, opts Options) (*Handler, error) {
	if svc == nil {
		return nil, errors.New("httpapi: nil service")
	}
	h := &Handler{
		svc:         svc,
		opts:        opts,
		log:         opts.Logger,
		mux:         http.NewServeMux(),
		cop:         http.NewCrossOriginProtection(),
		corsOrigins: map[string]bool{},
		done:        make(chan struct{}),
		now:         time.Now,
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.opts.WriteTimeout <= 0 {
		h.opts.WriteTimeout = 30 * time.Second
	}
	if opts.PublicURL != "" {
		u, err := url.Parse(opts.PublicURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("httpapi: PublicURL %q must be an absolute http(s) URL", opts.PublicURL)
		}
		h.publicOrigin = u.Scheme + "://" + u.Host
		h.publicHost = strings.ToLower(u.Hostname())
		secure := u.Scheme == "https"
		h.secureFixed = &secure
		if err := h.cop.AddTrustedOrigin(h.publicOrigin); err != nil {
			return nil, fmt.Errorf("httpapi: PublicURL: %w", err)
		}
	}
	for _, o := range opts.TrustedOrigins {
		if err := h.cop.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("httpapi: TrustedOrigins: %w", err)
		}
	}
	h.allowedHosts = map[string]bool{}
	for _, name := range opts.AllowedHosts {
		name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
		switch {
		case name == "*":
			h.anyHost = true
		case name == "" || strings.ContainsAny(name, "/:@ "):
			return nil, fmt.Errorf("httpapi: AllowedHosts entry %q must be a bare host name", name)
		default:
			h.allowedHosts[name] = true
		}
	}
	h.deviceKey = opts.DeviceKey
	switch {
	case len(h.deviceKey) == 0:
		h.deviceKey = make([]byte, 32)
		if _, err := rand.Read(h.deviceKey); err != nil {
			return nil, fmt.Errorf("httpapi: generate device key: %w", err)
		}
	case len(h.deviceKey) < 32:
		return nil, errors.New("httpapi: DeviceKey must be at least 32 bytes")
	}
	for _, o := range opts.CORSOrigins {
		u, err := url.Parse(o)
		if o == "*" || err != nil || u.Host == "" || u.Path != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("httpapi: CORSOrigins entry %q must be an origin like https://shop.example.com", o)
		}
		h.corsOrigins[u.Scheme+"://"+u.Host] = true
	}
	h.opts.Limits = opts.Limits.withDefaults()
	h.opts.Stream = opts.Stream.withDefaults()
	h.lim = newLimiters(h.opts.Limits, func() time.Time { return h.now() })
	h.streams = newStreamLimiter(h.opts.Stream.MaxPerIP, h.opts.Stream.MaxPerPayment, h.opts.Stream.MaxPerIPPerPayment, h.opts.Stream.MaxTotal)
	h.routes()
	return h, nil
}

// Close ends every open event stream. It is safe to call more than once.
func (h *Handler) Close() { h.closeOnce.Do(func() { close(h.done) }) }

// NewServer returns an *http.Server hardened for this API: header and body
// read timeouts, an idle timeout and a header size cap, but no global
// WriteTimeout (which would kill event streams; non-streaming routes get a
// per-request write deadline instead). root is the handler to serve — the
// wiring's top-level mux, or nil for h itself. Shutdown of the server also
// closes h's event streams.
func (h *Handler) NewServer(addr string, root http.Handler) *http.Server {
	if root == nil {
		root = h
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(h.log.Handler(), slog.LevelWarn),
	}
	srv.RegisterOnShutdown(h.Close)
	return srv
}

// ParsePrefixes parses IP addresses and CIDR prefixes ("10.0.0.0/8",
// "127.0.0.1", "::1") for [Options.TrustedProxies].
func ParsePrefixes(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if strings.Contains(s, "/") {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("httpapi: invalid proxy prefix %q: %w", s, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("httpapi: invalid proxy address %q: %w", s, err)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// ---- request bookkeeping ----

// reqInfo is per-request state shared between middleware and handlers.
type reqInfo struct {
	id      string
	route   string
	ip      netip.Addr
	actor   string // actor kind for the access log
	actorID string
	start   time.Time
}

type reqInfoKey struct{}

func info(r *http.Request) *reqInfo {
	if ri, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{}
}

// statusWriter records the status and size of a response. It forwards
// Flush and exposes Unwrap so http.ResponseController reaches the
// underlying connection.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/") || p == "/healthz" || p == "/readyz"
}

func isPublicPath(p string) bool { return strings.HasPrefix(p, "/api/v1/public/") }

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The Host rule comes first and covers everything the server answers:
	// a request for the interface under a host name this server does not
	// answer for is refused below exactly like an API request, and never
	// reaches the Fallback. (Its requests are otherwise outside this
	// handler's bookkeeping: no request id, no access-log line — they are
	// static files, or a development proxy.)
	if !isAPIPath(r.URL.Path) && h.opts.Fallback != nil && h.hostAllowed(r.Host) {
		h.opts.Fallback.ServeHTTP(w, r)
		return
	}
	ri := &reqInfo{id: requestID(r), ip: h.clientIP(r), start: time.Now(), route: "unmatched"}
	r = r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, ri))
	sw := &statusWriter{ResponseWriter: w}

	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			h.log.LogAttrs(r.Context(), slog.LevelError, "http panic",
				slog.String("request_id", ri.id), slog.String("route", ri.route),
				slog.Any("panic", v), slog.String("stack", string(debug.Stack())))
			if sw.status == 0 {
				writeErrorBody(sw, ri.id, &apiError{Status: http.StatusInternalServerError, Code: "internal", Message: "internal server error"})
			}
		}
		h.accessLog(r, ri, sw)
	}()

	hdr := sw.Header()
	hdr.Set("X-Request-Id", ri.id)
	h.securityHeaders(hdr, r)
	if !h.hostAllowed(r.Host) {
		ri.route = "host-not-allowed"
		writeErrorBody(sw, ri.id, errHostNotAllowed)
		return
	}
	if h.cors(sw, r) {
		return // preflight answered
	}

	if _, pattern := h.mux.Handler(r); pattern == "" {
		// Let the mux decide between 404 and 405, but answer in JSON.
		cw := &captureWriter{header: http.Header{}}
		hh, _ := h.mux.Handler(r)
		hh.ServeHTTP(cw, r)
		if allow := cw.header.Get("Allow"); allow != "" {
			hdr.Set("Allow", allow)
		}
		e := &apiError{Status: http.StatusNotFound, Code: "not_found", Message: "no such endpoint"}
		if cw.status == http.StatusMethodNotAllowed {
			e = &apiError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "method not allowed"}
		}
		writeErrorBody(sw, ri.id, e)
		return
	}
	h.mux.ServeHTTP(sw, r)
}

var errHostNotAllowed = &apiError{Status: http.StatusMisdirectedRequest, Code: "host_not_allowed",
	Message: "this server does not answer for the requested host name"}

// hostAllowed applies the Host header rule of [Options.AllowedHosts], to
// the API and to the Fallback alike. The server answers for: the host of PublicURL; "localhost"; any IP-address
// literal (a health check on 127.0.0.1, a container address, a LAN IP);
// the configured extra names; and requests without a Host (HTTP/1.0).
// Everything else — notably an arbitrary DNS name pointed at this server,
// which is what DNS rebinding relies on — is refused before any route runs.
// Ports are ignored.
func (h *Handler) hostAllowed(hostport string) bool {
	if h.anyHost || hostport == "" {
		return true
	}
	host := hostport
	if hst, _, err := net.SplitHostPort(hostport); err == nil {
		host = hst
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "localhost" || (h.publicHost != "" && host == h.publicHost) || h.allowedHosts[host] {
		return true
	}
	_, err := netip.ParseAddr(host)
	return err == nil
}

// captureWriter swallows the mux's plain-text 404/405 answer.
type captureWriter struct {
	header http.Header
	status int
}

func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return len(b), nil
}
func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

// accessLog writes one structured line per request. It deliberately logs
// the route pattern, never the raw path (payment ids are checkout
// capabilities) or the query string (search text, cursors), and never
// headers or bodies.
func (h *Handler) accessLog(r *http.Request, ri *reqInfo, sw *statusWriter) {
	status := sw.status
	if status == 0 {
		status = http.StatusOK
	}
	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.String("request_id", ri.id),
		slog.String("method", r.Method),
		slog.String("route", ri.route),
		slog.Int("status", status),
		slog.Int64("bytes", sw.bytes),
		slog.Float64("duration_ms", float64(time.Since(ri.start).Microseconds())/1000),
		slog.String("ip", ri.ip.String()),
	}
	if ri.actor != "" {
		attrs = append(attrs, slog.String("actor", ri.actor), slog.String("actor_id", ri.actorID))
	}
	h.log.LogAttrs(r.Context(), level, "http request", attrs...)
}

// securityHeaders sets the headers every API response carries.
func (h *Handler) securityHeaders(hdr http.Header, r *http.Request) {
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Cross-Origin-Opener-Policy", "same-origin")
	if h.opts.PublicCORS && isPublicPath(r.URL.Path) {
		hdr.Set("Cross-Origin-Resource-Policy", "cross-origin")
	} else {
		hdr.Set("Cross-Origin-Resource-Policy", "same-origin")
	}
}

// cors applies the CORS policy and answers preflight requests. It reports
// whether the request was fully handled.
func (h *Handler) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	hdr := w.Header()
	preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
	switch {
	case h.opts.PublicCORS && isPublicPath(r.URL.Path):
		hdr.Set("Access-Control-Allow-Origin", "*")
		hdr.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
		if preflight {
			hdr.Set("Access-Control-Allow-Methods", "GET, POST")
			hdr.Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key, Last-Event-ID")
			hdr.Set("Access-Control-Max-Age", "600")
		}
	case strings.HasPrefix(r.URL.Path, "/api/") && h.corsOrigins[origin]:
		hdr.Add("Vary", "Origin")
		hdr.Set("Access-Control-Allow-Origin", origin)
		hdr.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
		if preflight {
			hdr.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE")
			hdr.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-Id, Last-Event-ID")
			hdr.Set("Access-Control-Max-Age", "600")
		}
	default:
		return false
	}
	if preflight {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// ---- routing ----

// authMode says how a route treats credentials.
type authMode int

const (
	// authPublic ignores credentials entirely (checkout, health).
	authPublic authMode = iota
	// authOptional uses valid credentials and silently ignores invalid ones
	// (the setup wizard's helpers: public until setup, operator-only after —
	// the core decides).
	authOptional
	// authRequired rejects requests without valid credentials with 401
	// before doing any work; the core still authorises every call.
	authRequired
)

// limitClass selects the per-IP rate limit applied before a route runs.
type limitClass int

const (
	limitNone limitClass = iota
	limitSetup
	limitPublicRead
)

type apiFunc func(w http.ResponseWriter, r *http.Request) error

type route struct {
	pattern string
	mode    authMode
	limit   limitClass
	stream  bool // long-lived response: no write deadline
	fn      apiFunc
}

func (h *Handler) handle(rt route) {
	h.mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) {
		ri := info(r)
		ri.route = rt.pattern
		if !rt.stream {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(h.opts.WriteTimeout))
		}
		var err error
		switch rt.limit {
		case limitSetup:
			err = h.lim.setup.allow(h.rateKey(r))
		case limitPublicRead:
			err = h.lim.publicRead.allow(h.rateKey(r))
		}
		if err == nil && rt.mode != authPublic {
			var ctx context.Context
			ctx, err = h.authenticate(w, r, rt.mode)
			if err == nil {
				r = r.WithContext(ctx)
			}
		}
		if err == nil {
			err = rt.fn(w, r)
		}
		if err != nil {
			h.writeError(w, r, err)
		}
	})
}

// systemContext is the context for the adapter's own reads on behalf of
// the public stream (never exposed raw; see stream.go).
func systemContext(ctx context.Context) context.Context {
	return core.WithActor(ctx, core.SystemActor("httpapi"))
}
