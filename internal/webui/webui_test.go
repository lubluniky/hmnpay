package webui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const (
	inlineA = `(self.__next_f=self.__next_f||[]).push([0])`
	inlineB = `self.__next_f.push([1,"a > b </div> \"quoted\""])`
)

func page(title string, inline ...string) []byte {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><script src="/_next/static/chunks/main.js" async=""></script></head><body><h1>` + title + `</h1>`)
	for _, s := range inline {
		b.WriteString("<script>" + s + "</script>")
	}
	b.WriteString(`</body></html>`)
	return []byte(b.String())
}

// export mirrors the layout of a Next.js static export with three dynamic
// routes exported for the placeholder parameter.
func export() fstest.MapFS {
	big := bytes.Repeat([]byte("console.log('hmnpay');\n"), 400)
	return fstest.MapFS{
		".placeholder":                         {Data: []byte("placeholder")},
		"index.html":                           {Data: page("overview", inlineA, inlineB)},
		"index.txt":                            {Data: []byte("rsc index")},
		"__next._tree.txt":                     {Data: []byte("rsc tree root")},
		"404.html":                             {Data: page("not found", inlineA)},
		"icon.svg":                             {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		"login.html":                           {Data: page("login", inlineA)},
		"payments.html":                        {Data: page("payments", inlineA)},
		"payments.txt":                         {Data: []byte("rsc payments")},
		"payments/__next._tree.txt":            {Data: []byte("rsc tree payments")},
		"payments/_.html":                      {Data: page("payment detail", inlineB)},
		"payments/_.txt":                       {Data: []byte("rsc payment detail")},
		"payments/_/__next._tree.txt":          {Data: []byte("rsc tree payment detail")},
		"pay/_.html":                           {Data: page("checkout", inlineA)},
		"pay/_.txt":                            {Data: []byte("rsc checkout")},
		"l/_.html":                             {Data: page("link", inlineA)},
		"settings.html":                        {Data: page("settings", inlineA)},
		"settings/wallet.html":                 {Data: page("wallet", inlineA)},
		"_next/static/chunks/main.js":          {Data: big},
		"_next/static/chunks/app.css":          {Data: []byte("body{}")},
		"_next/static/media/geist.woff2":       {Data: []byte{0, 1, 2, 3}},
		"_next/static/chunks/unknown.weirdext": {Data: []byte("x")},
	}
}

func newHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := New(export())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(t *testing.T, h http.Handler, method, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://pay.example.com"+target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func TestPagesAndFallbacks(t *testing.T) {
	h := newHandler(t)
	cases := []struct {
		path   string
		status int
		want   string // substring of the body
	}{
		{"/", 200, "overview"},
		{"/login", 200, "login"},
		{"/payments", 200, "<h1>payments</h1>"},
		{"/settings", 200, "<h1>settings</h1>"},
		{"/settings/wallet", 200, "wallet"},
		// Dynamic routes: any id gets the page exported for the placeholder.
		{"/payments/pay_01hzy3v9k2m4n6p8q0r2s4t6v8", 200, "payment detail"},
		{"/pay/pay_01hzy3v9k2m4n6p8q0r2s4t6v8", 200, "checkout"},
		{"/l/spring-sale", 200, "<h1>link</h1>"},
		{"/pay/_", 200, "checkout"},
		// Route data for client-side navigation follows the same rule.
		{"/index.txt", 200, "rsc index"},
		{"/payments.txt", 200, "rsc payments"},
		{"/__next._tree.txt", 200, "rsc tree root"},
		{"/payments/__next._tree.txt", 200, "rsc tree payments"},
		{"/payments/pay_abc.txt", 200, "rsc payment detail"},
		{"/payments/pay_abc/__next._tree.txt", 200, "rsc tree payment detail"},
		{"/pay/pay_abc.txt", 200, "rsc checkout"},
		// Unknown paths get the app's 404 page with status 404.
		{"/nope", 404, "not found"},
		{"/settings/nope", 404, "not found"},
		{"/pay", 404, "not found"},
		{"/pay/a/b", 404, "not found"},
		{"/payments/pay_abc/__next.nope.txt", 404, "not found"},
		{"/payments/pay_abc/extra", 404, "not found"},
		{"/l/x.js", 404, "not found"},
		{"/_next/static/chunks/missing.js", 404, "not found"},
		// No fallback inside the hashed assets.
		{"/_next/static/chunks/_", 404, "not found"},
		// Never: dot files, dot segments, empty segments, backslashes.
		{"/.placeholder", 404, "not found"},
		{"/.env", 404, "not found"},
		{"/payments/../index.html", 404, "not found"},
		{"/..%2f..%2fetc/passwd", 404, "not found"},
		{"//evil.example/", 404, "not found"},
		{"/payments//x", 404, "not found"},
		{`/pay\x`, 404, "not found"},
	}
	for _, c := range cases {
		w := get(t, h, "GET", c.path)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("GET %s = %d %q, want %d containing %q", c.path, w.Code, w.Body.String(), c.status, c.want)
		}
	}
}

func TestCachingHeaders(t *testing.T) {
	h := newHandler(t)
	for path, want := range map[string]string{
		"/":                               "no-store",
		"/login":                          "no-store",
		"/pay/pay_abc":                    "no-store",
		"/payments.txt":                   "no-store",
		"/payments/pay_abc.txt":           "no-store",
		"/nope":                           "no-store",
		"/_next/static/chunks/main.js":    "public, max-age=31536000, immutable",
		"/_next/static/chunks/app.css":    "public, max-age=31536000, immutable",
		"/_next/static/media/geist.woff2": "public, max-age=31536000, immutable",
		"/icon.svg":                       "public, max-age=300",
	} {
		if got := get(t, h, "GET", path).Header().Get("Cache-Control"); got != want {
			t.Errorf("Cache-Control of %s = %q, want %q", path, got, want)
		}
	}
	// A missing hashed asset must not be cached as "immutable".
	if got := get(t, h, "GET", "/_next/static/chunks/missing.js").Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("missing asset Cache-Control = %q", got)
	}
}

func TestContentTypes(t *testing.T) {
	h := newHandler(t)
	for path, want := range map[string]string{
		"/":                                     "text/html; charset=utf-8",
		"/_next/static/chunks/main.js":          "text/javascript; charset=utf-8",
		"/_next/static/chunks/app.css":          "text/css; charset=utf-8",
		"/_next/static/media/geist.woff2":       "font/woff2",
		"/payments.txt":                         "text/plain; charset=utf-8",
		"/icon.svg":                             "image/svg+xml",
		"/_next/static/chunks/unknown.weirdext": "application/octet-stream",
	} {
		w := get(t, h, "GET", path)
		if got := w.Header().Get("Content-Type"); got != want {
			t.Errorf("Content-Type of %s = %q, want %q", path, got, want)
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", path, got)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHandler(t)
	for _, path := range []string{"/", "/pay/pay_abc", "/nope", "/_next/static/chunks/main.js", "/payments.txt", "/payments/"} {
		hdr := get(t, h, "GET", path).Header()
		for name, want := range map[string]string{
			"X-Frame-Options":              "DENY",
			"Referrer-Policy":              "no-referrer",
			"X-Content-Type-Options":       "nosniff",
			"Permissions-Policy":           PermissionsPolicy,
			"Cross-Origin-Opener-Policy":   "same-origin",
			"Cross-Origin-Resource-Policy": "same-origin",
		} {
			if got := hdr.Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", path, name, got, want)
			}
		}
		if csp := hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q does not forbid framing", path, csp)
		}
	}
}

func TestPagePolicyAllowsExactlyItsInlineScripts(t *testing.T) {
	h := newHandler(t)
	csp := get(t, h, "GET", "/").Header().Get("Content-Security-Policy")
	for _, s := range []string{inlineA, inlineB} {
		if !strings.Contains(csp, "'"+hashOf(s)+"'") {
			t.Errorf("policy of / lacks the hash of %q: %s", s, csp)
		}
	}
	login := get(t, h, "GET", "/login").Header().Get("Content-Security-Policy")
	if !strings.Contains(login, "'"+hashOf(inlineA)+"'") || strings.Contains(login, hashOf(inlineB)) {
		t.Errorf("policy of /login must hash its own scripts only: %s", login)
	}
	for _, directive := range []string{
		"default-src 'self'", "connect-src 'self'", "frame-src 'none'", "child-src 'none'", "worker-src 'none'",
		"object-src 'none'", "base-uri 'none'", "form-action 'self'", "frame-ancestors 'none'", "font-src 'self'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("policy lacks %q: %s", directive, csp)
		}
	}
	// The script directive names this origin and hashes, nothing looser.
	var script string
	for _, d := range strings.Split(csp, "; ") {
		if strings.HasPrefix(d, "script-src ") {
			script = d
		}
	}
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "*", "data:", "blob:"} {
		if strings.Contains(script, bad) {
			t.Errorf("script-src contains %q: %s", bad, script)
		}
	}
	// Anything that is not a page may load nothing at all.
	for _, path := range []string{"/_next/static/chunks/main.js", "/payments.txt", "/icon.svg"} {
		if got := get(t, h, "GET", path).Header().Get("Content-Security-Policy"); got != assetPolicy {
			t.Errorf("policy of %s = %q", path, got)
		}
	}
}

func TestScriptHashes(t *testing.T) {
	doc := []byte(`<html><head>
<script src="/a.js"></script>
<SCRIPT SRC='/b.js' async></SCRIPT>
<script data-x="a>b" id=one>first()</script>
<script type="application/json">{"a":1}</script>
<script
  nonce="x">
second("</div>")
</script >
<scripts>not a script</scripts>
<script>first()</script>
<script data-src="/not-src.js">third()</script>
</head></html>`)
	want := []string{hashOf("first()"), hashOf(`{"a":1}`), hashOf("\nsecond(\"</div>\")\n"), hashOf("third()")}
	got := ScriptHashes(doc)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("ScriptHashes:\n got %v\nwant %v", got, want)
	}
	if got := ScriptHashes([]byte(`<p>no scripts</p><script src=x>`)); len(got) != 0 {
		t.Errorf("unterminated script produced hashes: %v", got)
	}
}

func TestMethodsAndRedirects(t *testing.T) {
	h := newHandler(t)
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		w := get(t, h, m, "/login")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s /login = %d Allow %q", m, w.Code, w.Header().Get("Allow"))
		}
	}
	w := get(t, h, "HEAD", "/pay/pay_abc")
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD = %d, %d body bytes, Content-Length %q", w.Code, w.Body.Len(), w.Header().Get("Content-Length"))
	}
	w = get(t, h, "GET", "/payments/?view=open")
	if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "/payments?view=open" {
		t.Errorf("trailing slash = %d Location %q", w.Code, w.Header().Get("Location"))
	}
	// A trailing-slash redirect can never leave the site.
	for _, p := range []string{"//evil.example/", "/\\evil.example/", "///evil.example/"} {
		w := get(t, h, "GET", p)
		if w.Code != 404 || w.Header().Get("Location") != "" {
			t.Errorf("GET %s = %d Location %q, want 404", p, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestCompressionAndValidators(t *testing.T) {
	h := newHandler(t)
	plain := get(t, h, "GET", "/_next/static/chunks/main.js")
	if plain.Header().Get("Content-Encoding") != "" {
		t.Fatal("compressed without Accept-Encoding")
	}
	w := get(t, h, "GET", "/_next/static/chunks/main.js", "Accept-Encoding", "br, gzip;q=0.8")
	if w.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("Content-Encoding %q Vary %q", w.Header().Get("Content-Encoding"), w.Header().Get("Vary"))
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if !bytes.Equal(got, plain.Body.Bytes()) {
		t.Error("gzip body does not decode to the file")
	}
	if w.Body.Len() >= plain.Body.Len() {
		t.Error("gzip body is not smaller")
	}
	if get(t, h, "GET", "/_next/static/chunks/main.js", "Accept-Encoding", "gzip;q=0").Header().Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 was ignored")
	}
	// Binary formats and tiny files are sent as they are.
	if get(t, h, "GET", "/_next/static/media/geist.woff2", "Accept-Encoding", "gzip").Header().Get("Content-Encoding") != "" {
		t.Error("a font was compressed")
	}
	etag := plain.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on a cacheable asset")
	}
	if w := get(t, h, "GET", "/_next/static/chunks/main.js", "If-None-Match", etag); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Errorf("If-None-Match = %d with %d bytes", w.Code, w.Body.Len())
	}
	if get(t, h, "GET", "/").Header().Get("ETag") != "" {
		t.Error("a no-store page carries an ETag")
	}
}

func TestNewRefusesAnUnbuiltInterface(t *testing.T) {
	if _, err := New(fstest.MapFS{".placeholder": {Data: []byte("x")}}); err == nil {
		t.Fatal("New accepted a directory without index.html")
	}
}

// TestEmbeddedDist checks whatever is embedded in this build: the
// repository's placeholder (not built) or a real export, which must then
// be servable and must contain the pages the Go side links to.
func TestEmbeddedDist(t *testing.T) {
	if !Built() {
		if _, err := New(Dist()); err == nil {
			t.Fatal("New accepted the placeholder directory")
		}
		t.Skip("the web interface is not built into this test binary (make ui)")
	}
	h, err := New(Dist())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/login", "/setup", "/payments", "/payment-links", "/settings", "/settings/network",
		"/settings/wallet", "/settings/webhooks", "/settings/api-keys",
		"/pay/pay_0123456789abcdefghjkmnpqrs", "/payments/pay_0123456789abcdefghjkmnpqrs", "/l/some-link"} {
		w := get(t, h, "GET", path)
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Errorf("GET %s = %d %s", path, w.Code, w.Header().Get("Content-Type"))
			continue
		}
		// Every inline script of the page is allowed by the page's policy.
		csp := w.Header().Get("Content-Security-Policy")
		for _, hash := range ScriptHashes(w.Body.Bytes()) {
			if !strings.Contains(csp, "'"+hash+"'") {
				t.Errorf("%s: an inline script is not covered by the policy", path)
			}
		}
	}
}
