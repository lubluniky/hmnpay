package hmnpay

import (
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/lubluniky/hmnpay/internal/httpapi"
)

// HTTPLimits tunes the API's rate limits; zero fields take defaults.
type HTTPLimits = httpapi.Limits

// Rate is one token-bucket limit: Burst requests, one more every Every.
type Rate = httpapi.Rate

// StreamOptions tunes the checkout's server-sent event stream; zero fields
// take defaults.
type StreamOptions = httpapi.StreamOptions

// HTTPOptions configures [Engine.HTTPHandler]. The zero value is usable;
// behind a reverse proxy set TrustedProxies.
type HTTPOptions struct {
	// PublicURL defaults to [Config.PublicURL]. It decides the session
	// cookie's Secure flag and the origin trusted for cookie-authenticated
	// writes.
	PublicURL string
	// AllowedHosts are extra Host names (without port) the API answers
	// for. It always answers for the host of PublicURL, "localhost" and IP
	// literals; any other Host gets 421 host_not_allowed (DNS rebinding
	// protection). "*" disables the check.
	AllowedHosts []string
	// DeviceKey signs the "known device" cookie; it defaults to the key
	// the engine keeps in its database, so markers survive restarts.
	DeviceKey []byte
	// TrustedProxies are the reverse proxies whose X-Forwarded-* headers
	// are believed. Empty trusts nobody. See [ParsePrefixes].
	TrustedProxies []netip.Prefix
	// TrustedOrigins are additional origins allowed to make
	// cookie-authenticated writes (e.g. a dashboard dev server).
	TrustedOrigins []string
	// CORSOrigins allows cross-origin bearer-token API access from these
	// exact origins (never with cookies).
	CORSOrigins []string
	// PublicCORS opens /api/v1/public/* (checkout data and stream) to any
	// origin, without credentials.
	PublicCORS bool
	// Fallback serves every path outside /api/, /healthz and /readyz (the
	// web UI). Nil answers them with a JSON 404.
	Fallback http.Handler
	// Limits and Stream tune rate limiting and the event stream.
	Limits HTTPLimits
	Stream StreamOptions
	// WriteTimeout bounds non-streaming responses. Default 30 s.
	WriteTimeout time.Duration
	// Logger defaults to [Config.Logger].
	Logger *slog.Logger
}

// HTTPHandler serves the REST API, the public checkout endpoints with their
// event stream, and the health probes. It implements http.Handler.
type HTTPHandler struct{ h *httpapi.Handler }

// HTTPHandler returns the HTTP adapter for this engine. Several handlers
// may be made (for different listeners); [Engine.Close] closes them all.
func (e *Engine) HTTPHandler(opts HTTPOptions) (*HTTPHandler, error) {
	if opts.PublicURL == "" {
		opts.PublicURL = e.cfg.PublicURL
	}
	if opts.Logger == nil {
		opts.Logger = e.log
	}
	if opts.DeviceKey == nil {
		opts.DeviceKey = e.deviceKey
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("hmnpay: engine is closed")
	}
	h, err := httpapi.New(e.svc, httpapi.Options{
		PublicURL:      opts.PublicURL,
		AllowedHosts:   opts.AllowedHosts,
		DeviceKey:      opts.DeviceKey,
		TrustedOrigins: opts.TrustedOrigins,
		CORSOrigins:    opts.CORSOrigins,
		PublicCORS:     opts.PublicCORS,
		TrustedProxies: opts.TrustedProxies,
		Logger:         opts.Logger,
		Fallback:       opts.Fallback,
		Limits:         opts.Limits,
		Stream:         opts.Stream,
		WriteTimeout:   opts.WriteTimeout,
		Version:        Version,
		Commit:         Commit,
	})
	if err != nil {
		return nil, err
	}
	e.handlers = append(e.handlers, h)
	return &HTTPHandler{h: h}, nil
}

// ServeHTTP implements http.Handler.
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.h.ServeHTTP(w, r) }

// Close ends every open event stream. Safe to call more than once.
func (h *HTTPHandler) Close() { h.h.Close() }

// NewServer returns an *http.Server for this handler with hardened
// timeouts (header and body read timeouts, idle timeout, header size cap,
// per-request write deadlines that spare the event stream). Shutting the
// server down also ends the handler's event streams, which would otherwise
// keep Shutdown waiting.
func (h *HTTPHandler) NewServer(addr string) *http.Server { return h.h.NewServer(addr, nil) }

// ParsePrefixes parses IP addresses and CIDR prefixes ("10.0.0.0/8",
// "127.0.0.1") for [HTTPOptions.TrustedProxies] and
// [WebhookConfig.AllowedPrefixes].
func ParsePrefixes(list []string) ([]netip.Prefix, error) { return httpapi.ParsePrefixes(list) }
