package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/webui"
)

// The web UI seam: what answers every path outside /api/, /healthz and
// /readyz. Three cases, in this order:
//
//  1. HMNPAY_WEB_UPSTREAM is set: every such path is reverse-proxied to
//     that server. This is for developing the interface ("pnpm dev" next to
//     a running hmnpay): browser, API and checkout still share one origin
//     (same-origin cookies, no CORS). It is not a way to run hmnpay: see
//     newUpstreamProxy for what the upstream is trusted with.
//  2. The interface was built into the binary (internal/webui, "make ui"):
//     it is served from memory. This is the normal case — one binary, one
//     port.
//  3. Neither: a built-in status page says that this build has no
//     interface and how to get one. The API works all the same.
//
// In all three the Host rule of the API handler has already been applied
// when a request arrives here.

// parseUpstream accepts a bare http(s) origin: no credentials, path, query
// or fragment, so the proxy target is unambiguous. Unless allowRemote, the
// host must be this machine: "localhost" or a loopback IP address.
func parseUpstream(raw string, allowRemote bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, err
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("%q must start with http:// or https://", raw)
	case u.Host == "":
		return nil, fmt.Errorf("%q has no host", raw)
	case u.User != nil:
		return nil, errors.New("must not contain credentials")
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		return nil, fmt.Errorf("%q must be a bare origin like http://localhost:3000", raw)
	case !allowRemote && !loopbackHost(u.Hostname()):
		return nil, fmt.Errorf("%q is not on this machine. The upstream serves every page of this instance's origin, so by default only "+
			"localhost or a loopback address (127.0.0.1, [::1]) is accepted; set HMNPAY_WEB_UPSTREAM_ALLOW_REMOTE=true to use another host "+
			"(development only)", raw)
	}
	u.Path = ""
	return u, nil
}

// loopbackHost reports whether host names this machine without asking a
// resolver: "localhost" or a loopback IP address literal.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// Request headers that are not forwarded to the upstream: everything that
// authenticates a caller to this instance, and everything its API reads to
// recognise a same-origin browser request. A development server needs none
// of them to render a page.
var upstreamDropRequest = []string{
	"Cookie", "Authorization", "Proxy-Authorization", "X-Hmnpay-Setup-Token", "Idempotency-Key",
	"Origin", "Referer", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-Fetch-User",
}

// Response headers of the upstream that do not reach the browser: it must
// not set or clear cookies on the instance's origin (the session cookie
// lives there), nor open the origin to others. The security headers are
// replaced as well, see setUpstreamHeaders.
var upstreamDropResponse = []string{
	"Set-Cookie", "Clear-Site-Data",
	"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Allow-Methods",
	"Access-Control-Allow-Headers", "Access-Control-Expose-Headers", "Access-Control-Max-Age",
	"Content-Security-Policy-Report-Only", "Strict-Transport-Security",
}

type inboundHostKey struct{}

// newUpstreamProxy reverse-proxies to a web UI development server.
//
// What the upstream is trusted with: it serves the pages of the instance's
// origin, so a script it serves runs there and can use the operator's
// session through the API like the real interface does. That cannot be
// taken away from a development server, which is why the mode is for
// development, accepts only a loopback upstream unless told otherwise, and
// says so in the log at start.
//
// What is kept from it nevertheless:
//
//   - Credentials: Cookie, Authorization and the setup token are removed
//     from every forwarded request (upstreamDropRequest), so the upstream —
//     or anything on the way to it — never holds a session or an API key.
//     Client-supplied X-Forwarded-* are discarded and rebuilt from the
//     connection.
//   - Cookies and CORS: Set-Cookie and the like are removed from its
//     answers (upstreamDropResponse). Hop-by-hop headers are removed in
//     both directions by httputil.ReverseProxy itself.
//   - Policy: its answers are sent with the server's own security headers
//     and a development Content-Security-Policy (setUpstreamHeaders),
//     whatever it sent.
//
// Responses are flushed immediately (React streaming, hot reload).
func newUpstreamProxy(raw string, allowRemote bool, log *slog.Logger) (http.Handler, error) {
	target, err := parseUpstream(raw, allowRemote)
	if err != nil {
		return nil, err
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			for _, name := range upstreamDropRequest {
				pr.Out.Header.Del(name)
			}
		},
		ModifyResponse: func(res *http.Response) error {
			host, _ := res.Request.Context().Value(inboundHostKey{}).(string)
			setUpstreamHeaders(res.Header, host)
			return nil
		},
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("web UI upstream unavailable", "error", err)
			setUpstreamHeaders(w.Header(), r.Host)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintln(w, "The hmnpay web interface is not reachable right now. The API is running at /api/v1.")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The policy names the host the browser used (for the hot-reload
		// WebSocket), which the outgoing request no longer carries.
		proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), inboundHostKey{}, r.Host)))
	}), nil
}

// uiPermissionsPolicy turns off powerful browser features the dashboard
// and checkout never use (the same value the embedded interface sends).
const uiPermissionsPolicy = webui.PermissionsPolicy

// devPolicy is the Content-Security-Policy of pages served through
// HMNPAY_WEB_UPSTREAM. It is the policy of the built interface
// (webui.Policy) with what a Next.js development server cannot do without:
//
//   - script-src gets 'unsafe-inline' and 'unsafe-eval': "next dev" writes
//     inline scripts that change with every edit (no stable hash to allow)
//     and evaluates code for fast refresh and source maps. The built
//     interface gets neither.
//   - connect-src names the page's own host for ws: and wss: next to
//     'self', for the hot-reload WebSocket (not every browser reads 'self'
//     as covering it).
//   - worker-src allows this origin and blob:, which the development
//     tooling may use; the built interface allows no workers.
//   - font-src allows data:, for fonts the development server inlines.
//
// Everything else is as strict as in production: nothing loads from, or
// connects to, another origin (images excepted, as there: a merchant's logo
// may be a URL); no frames, objects or <base>; forms post to this origin
// only; and the pages cannot be framed.
func devPolicy(host string) string {
	connect := "connect-src 'self'"
	if host != "" && !strings.ContainsAny(host, " ;,'\"") {
		connect += " ws://" + host + " wss://" + host
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'unsafe-inline' 'unsafe-eval'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob: https: http:",
		"font-src 'self' data:",
		connect,
		"media-src 'none'",
		"object-src 'none'",
		"frame-src 'none'",
		"child-src 'none'",
		"worker-src 'self' blob:",
		"manifest-src 'self'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

// setUpstreamHeaders makes the headers of an answer that came from the
// development upstream the server's own: the headers the upstream must not
// send are removed, and the security headers are set — not merged, not
// defaulted: whatever the upstream sent for them is replaced. No framing
// (clickjacking of the dashboard; the checkout cannot be embedded in a
// merchant's iframe either), no Referer (the checkout URL is a capability
// and must not leak to an external logo or link host), no MIME sniffing,
// no powerful features, and the development policy.
func setUpstreamHeaders(h http.Header, host string) {
	for _, name := range upstreamDropResponse {
		h.Del(name)
	}
	h.Set("Content-Security-Policy", devPolicy(host))
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Permissions-Policy", uiPermissionsPolicy)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

var statusPage = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>hmnpay</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:3rem auto;padding:0 1rem;color:#1a1a1a;background:#fff}
@media (prefers-color-scheme:dark){body{color:#e8e8e8;background:#111}a{color:#8ab4ff}}
code{font-size:.9em}
.state{padding:.75rem 1rem;border-radius:.5rem;border:1px solid #8884}
</style>
</head>
<body>
<h1>hmnpay is running</h1>
<p>The API is served under <code>/api/v1</code>.</p>
<p class="state">{{if .Error}}Setup status is unavailable right now.{{else if .Completed}}Setup is complete.{{else}}<strong>Setup required.</strong>{{if .TokenRequired}} Claiming this instance needs the <strong>setup token</strong> printed in the server log when it started (for example <code>docker compose logs hmnpay</code>).{{end}} {{end}}</p>
<p><strong>This build has no web interface.</strong> The binary was compiled without it, so there is no dashboard, setup wizard or checkout page here; the API is complete and can do all of it. To get the interface, build it into the binary (<code>make build</code> in the source tree, which needs Node.js and pnpm) or use the Docker image, which always has it.</p>
<ul>
<li><a href="/api/v1/setup">/api/v1/setup</a> &mdash; setup status</li>
<li><a href="/healthz">/healthz</a> &mdash; health check</li>
<li><a href="/license.txt">/license.txt</a> &mdash; the licence of this program (GNU GPL 3.0 or later)</li>
<li><a href="/additional-terms.txt">/additional-terms.txt</a> &mdash; the additional terms of that licence (section 7)</li>
<li><a href="/third-party-notices.txt">/third-party-notices.txt</a> &mdash; licences of the third-party software in this program</li>
</ul>
</body>
</html>
`))

// statusHandler is the built-in page served when the binary has no web
// interface and none is proxied.
func statusHandler(svc *core.Service, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Cache-Control", "no-store")
		hdr.Set("Permissions-Policy", uiPermissionsPolicy)
		if r.URL.Path != "/" && r.URL.Path != "/setup" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			hdr.Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx := r.Context()
		st, err := svc.SetupStatus(ctx)
		if err != nil {
			log.Warn("status page: setup status", "error", err)
		}
		hdr.Set("Content-Type", "text/html; charset=utf-8")
		// Which version this is, the page does not say: it is open to
		// anybody, and the API tells the version to signed-in callers only.
		// It is in the log.
		_ = statusPage.Execute(w, struct {
			Completed     bool
			TokenRequired bool
			Error         bool
		}{st.Completed, st.TokenRequired, err != nil})
	})
}

// webMode says what serves the web interface.
type webMode string

const (
	webProxy    webMode = "proxy"    // HMNPAY_WEB_UPSTREAM
	webEmbedded webMode = "embedded" // built into the binary
	webNone     webMode = "none"     // status page only
)

// embeddedUI returns the interface built into the binary (tests replace
// it).
var embeddedUI = webui.Dist

// uiBuilt reports whether ui holds a built interface.
func uiBuilt(ui fs.FS) bool {
	_, err := fs.Stat(ui, "index.html")
	return err == nil
}

// webHandler returns the Fallback for the API handler and what it is. In
// every mode the licence, the additional terms and the third-party notices
// are served at their paths (legalTexts).
func webHandler(upstream string, allowRemote bool, svc *core.Service, log *slog.Logger) (http.Handler, webMode, error) {
	if upstream != "" {
		h, err := newUpstreamProxy(upstream, allowRemote, log)
		if err != nil {
			return nil, webProxy, err
		}
		return withNotices(h), webProxy, nil
	}
	if ui := embeddedUI(); uiBuilt(ui) {
		h, err := webui.New(ui)
		if err != nil {
			return nil, webNone, err
		}
		return withNotices(h), webEmbedded, nil
	}
	return withNotices(statusHandler(svc, log)), webNone, nil
}
