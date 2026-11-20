// Package webui serves the hmnpay web interface — the dashboard, the setup
// wizard, the login page, the hosted checkout and the public payment-link
// page — from files embedded in the binary.
//
// The interface is a static export of the Next.js app in apps/web: plain
// HTML, JavaScript, CSS and fonts with no server-side rendering, so the Go
// server needs nothing but these files. They are built by "make ui" (or
// scripts/build-ui.sh, or the Dockerfile's Node stage) into the dist
// directory next to this file and embedded with go:embed. The repository
// carries only a placeholder there: a binary built without that step has no
// interface ([Built] reports false) and the wiring serves a status page
// that says so.
//
// What the handler does with a request path:
//
//   - /_next/static/… are content-hashed assets: served as they are, cached
//     for a year, immutable.
//   - A page is a file "<path>.html" ("/payments" is payments.html, "/" is
//     index.html). The app's dynamic routes are exported once, for the
//     placeholder parameter "_" (pay/_.html, payments/_.html, l/_.html);
//     a path segment that names no file falls back to "_", so
//     /pay/pay_0123… gets pay/_.html and the page reads the real id from
//     the address bar. The same holds for the route data Next.js fetches
//     on client-side navigation (….txt files).
//   - Everything else is a 404 with the app's own not-found page.
//
// Pages and route data are sent with "Cache-Control: no-store": they name
// the hashed assets of this build, and a stale copy would point at files a
// new binary no longer has.
//
// Every response carries the security headers of the UI (no framing, no
// referrer, no MIME sniffing, no powerful features) and a
// Content-Security-Policy. The policy of a page allows scripts from this
// origin and, by SHA-256 hash, exactly the inline scripts that page's HTML
// contains (Next.js emits a few to bootstrap itself); the hashes are
// computed from the embedded bytes when the handler is made, so the policy
// cannot drift from what is served. See [Policy].
package webui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
)

// dist holds the built interface. "all:" because Next.js names directories
// and files with a leading underscore (_next, _not-found, __next.….txt).
//
//go:embed all:dist
var dist embed.FS

// Placeholder is the parameter value the app's dynamic routes are exported
// for (generateStaticParams in apps/web).
const Placeholder = "_"

// Dist returns the embedded interface: the contents of the dist directory.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the directory is part of the package
	}
	return sub
}

// Built reports whether an interface was built into this binary, that is,
// whether the embedded directory holds more than the placeholder.
func Built() bool {
	_, err := fs.Stat(Dist(), "index.html")
	return err == nil
}

// PermissionsPolicy turns off powerful browser features the dashboard and
// the checkout never use.
const PermissionsPolicy = "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=(), interest-cohort=()"

// Policy returns the Content-Security-Policy of a page whose inline scripts
// have the given hashes ("sha256-…", as [ScriptHashes] returns them).
//
//   - script-src: this origin and the page's own inline scripts by hash. No
//     'unsafe-inline', no 'unsafe-eval', no other origin.
//   - connect-src 'self': fetch, XMLHttpRequest, EventSource and WebSocket
//     reach this origin only. That is the API — and the vendored wave
//     effect, which does call fetch: to draw a snapshot of the page it
//     reads again, from this origin, the font files the page already
//     uses. The one image on a page that somebody else chooses, a
//     merchant's logo on another host, is left out of the snapshot by the
//     app and never fetched; and if that exclusion were ever lost, this
//     directive is what refuses the request.
//   - frame-src, child-src, object-src 'none': the pages embed nothing from
//     anywhere. The wave effect does create one frame: a hidden, empty
//     <iframe> without a source, which it writes a blank document into to
//     read the browser's default styles. Such a frame makes no request and
//     is not a navigation, so frame-src does not stand in its way; it has
//     this page's origin and this policy. A frame with a source — any
//     source — is what the directive forbids.
//   - worker-src 'none': no workers.
//   - style-src allows inline styles: React writes style attributes into
//     the exported HTML (positions, animation values) and the toast
//     library injects its stylesheet. Styles cannot run script.
//   - img-src allows https/http images because a merchant's logo may be a
//     URL on their own site, data: for an inline logo and the QR code, and
//     blob: for the canvas snapshots of the wave effect.
//   - frame-ancestors 'none': the pages cannot be framed (clickjacking of
//     the dashboard; the checkout too).
func Policy(scriptHashes []string) string {
	script := "script-src 'self'"
	for _, h := range scriptHashes {
		script += " '" + h + "'"
	}
	return strings.Join([]string{
		"default-src 'self'",
		script,
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob: https: http:",
		"font-src 'self'",
		"connect-src 'self'",
		"media-src 'none'",
		"object-src 'none'",
		"frame-src 'none'",
		"child-src 'none'",
		"worker-src 'none'",
		"manifest-src 'self'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

// assetPolicy is the policy of everything that is not a page (scripts,
// styles, fonts, route data): such a response is never a document, and if a
// browser is made to treat it as one it may load and run nothing.
const assetPolicy = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// ScriptHashes returns the CSP hash sources ("sha256-<base64>") of the
// inline scripts in an HTML document, in document order without
// duplicates. A script is inline when its start tag has no src attribute;
// its content runs from the end of the start tag to the next "</script",
// which is how browsers delimit it.
func ScriptHashes(doc []byte) []string {
	var out []string
	seen := map[string]bool{}
	lower := bytes.ToLower(doc)
	for pos := 0; ; {
		i := bytes.Index(lower[pos:], []byte("<script"))
		if i < 0 {
			return out
		}
		start := pos + i + len("<script")
		pos = start
		// "<scripts>" is some other element.
		if start >= len(doc) || !strings.ContainsRune(" \t\n\f\r/>", rune(doc[start])) {
			continue
		}
		tagEnd, hasSrc := scanStartTag(lower, start)
		if tagEnd < 0 {
			return out
		}
		body := tagEnd + 1
		end := bytes.Index(lower[body:], []byte("</script"))
		if end < 0 {
			return out
		}
		pos = body + end
		if hasSrc {
			continue
		}
		sum := sha256.Sum256(doc[body : body+end])
		h := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
}

// scanStartTag walks the attributes of a start tag beginning at from (just
// after the tag name) and returns the index of its closing '>' and whether
// it has a src attribute. Quoted attribute values may contain '>'.
func scanStartTag(lower []byte, from int) (end int, hasSrc bool) {
	i := from
	for i < len(lower) {
		switch c := lower[i]; {
		case c == '>':
			return i, hasSrc
		case c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == '/':
			i++
		default:
			nameStart := i
			for i < len(lower) && !strings.ContainsRune(" \t\n\f\r/>=", rune(lower[i])) {
				i++
			}
			if string(lower[nameStart:i]) == "src" {
				hasSrc = true
			}
			for i < len(lower) && strings.ContainsRune(" \t\n\f\r", rune(lower[i])) {
				i++
			}
			if i >= len(lower) || lower[i] != '=' {
				continue
			}
			i++
			for i < len(lower) && strings.ContainsRune(" \t\n\f\r", rune(lower[i])) {
				i++
			}
			if i >= len(lower) {
				return -1, hasSrc
			}
			if q := lower[i]; q == '"' || q == '\'' {
				j := bytes.IndexByte(lower[i+1:], q)
				if j < 0 {
					return -1, hasSrc
				}
				i += j + 2
				continue
			}
			for i < len(lower) && !strings.ContainsRune(" \t\n\f\r>", rune(lower[i])) {
				i++
			}
		}
	}
	return -1, hasSrc
}

// file is one embedded file, read once when the handler is made.
type file struct {
	name        string
	data        []byte
	contentType string
	etag        string
	// csp is the Content-Security-Policy of the response.
	csp string
	// cache is the Cache-Control of the response.
	cache string

	gzipOnce sync.Once
	gzipped  []byte // nil when compression does not pay
}

// Handler serves the interface. It implements http.Handler.
type Handler struct {
	files map[string]*file
	dirs  map[string]bool
	// notFound is the app's 404 page, nil if the export has none.
	notFound *file
}

// contentTypes is the fixed table of what the export contains. It is not
// taken from the operating system's MIME database, so the same binary
// answers the same everywhere.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".txt":         "text/plain; charset=utf-8", // route data; Next.js accepts text/plain from a static export
	".json":        "application/json; charset=utf-8",
	".map":         "application/json; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".webp":        "image/webp",
	".gif":         "image/gif",
	".ico":         "image/x-icon",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".wasm":        "application/wasm",
	".webmanifest": "application/manifest+json",
}

// compressible lists the extensions worth compressing (text formats).
var compressible = map[string]bool{
	".html": true, ".js": true, ".mjs": true, ".css": true, ".txt": true, ".json": true, ".map": true, ".svg": true, ".webmanifest": true,
}

// Cache-Control values.
const (
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheNoStore   = "no-store"
	cacheShort     = "public, max-age=300"
)

// immutablePrefix marks the content-hashed assets of the export.
const immutablePrefix = "_next/static/"

// New returns the handler for the interface in fsys (see [Dist]). It reads
// every file once. It fails when fsys holds no index.html — an interface
// that was never built.
func New(fsys fs.FS) (*Handler, error) {
	h := &Handler{files: map[string]*file{}, dirs: map[string]bool{"": true}}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		// Dot files are never part of an export (the placeholder that keeps
		// the directory in the repository is one).
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			h.dirs[name] = true
			return nil
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		ext := strings.ToLower(path.Ext(name))
		ct, ok := contentTypes[ext]
		if !ok {
			ct = "application/octet-stream"
		}
		sum := sha256.Sum256(data)
		f := &file{name: name, data: data, contentType: ct, etag: `"` + hex.EncodeToString(sum[:12]) + `"`, csp: assetPolicy}
		switch {
		case strings.HasPrefix(name, immutablePrefix):
			f.cache = cacheImmutable
		case ext == ".html":
			f.cache = cacheNoStore
			f.csp = Policy(ScriptHashes(data))
		case ext == ".txt":
			f.cache = cacheNoStore
		default:
			f.cache = cacheShort
		}
		h.files[name] = f
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("webui: reading the interface: %w", err)
	}
	if h.files["index.html"] == nil {
		return nil, errors.New("webui: the interface has no index.html (it was not built)")
	}
	h.notFound = h.files["404.html"]
	return h, nil
}

// Pages returns the names of the HTML documents of the interface, sorted
// (for tests and diagnostics).
func (h *Handler) Pages() []string {
	var out []string
	for name := range h.files {
		if strings.HasSuffix(name, ".html") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// resolve maps a cleaned request path (no leading or trailing slash, ""
// for the root) onto an embedded file.
func (h *Handler) resolve(p string) *file {
	if p == "" {
		return h.files["index.html"]
	}
	if strings.HasPrefix(p, immutablePrefix) {
		return h.files[p] // hashed assets: exact names only
	}
	return h.walk("", strings.Split(p, "/"))
}

// walk resolves segs below dir. A segment that names nothing falls back to
// the placeholder entry, which is how one exported page serves every value
// of a dynamic route.
func (h *Handler) walk(dir string, segs []string) *file {
	join := func(name string) string {
		if dir == "" {
			return name
		}
		return dir + "/" + name
	}
	seg := segs[0]
	if len(segs) > 1 {
		if h.dirs[join(seg)] {
			if f := h.walk(join(seg), segs[1:]); f != nil {
				return f
			}
		}
		if seg != Placeholder && h.dirs[join(Placeholder)] {
			return h.walk(join(Placeholder), segs[1:])
		}
		return nil
	}
	// The last segment: a file by its own name, a page, or the placeholder
	// page (route data keeps its .txt extension).
	if f := h.files[join(seg)]; f != nil {
		return f
	}
	ext := path.Ext(seg)
	if ext == "" {
		if f := h.files[join(seg+".html")]; f != nil {
			return f
		}
		if f := h.files[join(seg+"/index.html")]; f != nil {
			return f
		}
		return h.files[join(Placeholder+".html")]
	}
	if ext == ".txt" && !strings.HasPrefix(seg, "__next.") {
		return h.files[join(Placeholder+".txt")]
	}
	return nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("Permissions-Policy", PermissionsPolicy)
	hdr.Set("Cross-Origin-Opener-Policy", "same-origin")
	hdr.Set("Cross-Origin-Resource-Policy", "same-origin")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		hdr.Set("Allow", "GET, HEAD")
		hdr.Set("Content-Security-Policy", assetPolicy)
		hdr.Set("Cache-Control", cacheNoStore)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if !validPath(p) {
		h.serveNotFound(w, r)
		return
	}
	// One spelling per page, as Next.js itself has it: no trailing slash.
	// The target starts with exactly one slash (validPath refuses "//"), so
	// this cannot redirect to another site.
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		target := strings.TrimRight(p, "/")
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		hdr.Set("Content-Security-Policy", assetPolicy)
		hdr.Set("Cache-Control", cacheNoStore)
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
		return
	}
	f := h.resolve(strings.TrimPrefix(p, "/"))
	if f == nil {
		h.serveNotFound(w, r)
		return
	}
	h.serveFile(w, r, f, http.StatusOK)
}

// validPath refuses anything that is not a plain absolute path: empty
// segments ("//"), dot segments, hidden files, backslashes and control
// characters. Nothing in the export needs them, and the handler never
// touches a file system at request time anyway.
func validPath(p string) bool {
	if !strings.HasPrefix(p, "/") || len(p) > 2048 {
		return false
	}
	if p == "/" {
		return true
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p[1:], "/"), "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return false
		}
		for i := 0; i < len(seg); i++ {
			if c := seg[i]; c < 0x20 || c == 0x7f || c == '\\' {
				return false
			}
		}
	}
	return true
}

func (h *Handler) serveNotFound(w http.ResponseWriter, r *http.Request) {
	if h.notFound == nil {
		w.Header().Set("Content-Security-Policy", assetPolicy)
		w.Header().Set("Cache-Control", cacheNoStore)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.serveFile(w, r, h.notFound, http.StatusNotFound)
}

func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, f *file, status int) {
	hdr := w.Header()
	hdr.Set("Content-Type", f.contentType)
	hdr.Set("Content-Security-Policy", f.csp)
	hdr.Set("Cache-Control", f.cache)
	if status == http.StatusNotFound {
		hdr.Set("Cache-Control", cacheNoStore)
	}
	body := f.data
	if compressible[strings.ToLower(path.Ext(f.name))] {
		hdr.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			if gz := f.gz(); gz != nil {
				hdr.Set("Content-Encoding", "gzip")
				body = gz
			}
		}
	}
	// Validators only where a cache may keep the response.
	if status == http.StatusOK && f.cache != cacheNoStore {
		hdr.Set("ETag", f.etag)
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, f.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	hdr.Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// gz returns the gzip form of the file, made on first use, or nil when it
// is not smaller.
func (f *file) gz() []byte {
	f.gzipOnce.Do(func() {
		if len(f.data) < 1024 {
			return
		}
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return
		}
		if _, err := zw.Write(f.data); err != nil {
			return
		}
		if err := zw.Close(); err != nil {
			return
		}
		if buf.Len() < len(f.data) {
			f.gzipped = buf.Bytes()
		}
	})
	return f.gzipped
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q := strings.ReplaceAll(strings.ToLower(params), " ", "")
		return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	return false
}

func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == etag {
			return true
		}
	}
	return false
}
