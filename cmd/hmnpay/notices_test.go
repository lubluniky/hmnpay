package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay"
)

// The binary carries the notices of the third-party software compiled into
// it and serves them at one stable path, as a plain text file that can only
// be read.
func TestThirdPartyNoticesAreServed(t *testing.T) {
	rest := 0
	h := withNotices(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest++
		io.WriteString(w, "everything else")
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/third-party-notices.txt", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Header().Get("X-Content-Type-Options") != "nosniff" ||
		w.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("GET: %d %v", w.Code, w.Header())
	}
	if w.Body.String() != hmnpay.ThirdPartyNotices || len(hmnpay.ThirdPartyNotices) < 10_000 {
		t.Errorf("the served text is not the embedded notices (%d bytes served, %d embedded)", w.Body.Len(), len(hmnpay.ThirdPartyNotices))
	}
	// Conditional and HEAD requests, and nothing but reading.
	again := httptest.NewRequest("GET", "/third-party-notices.txt", nil)
	again.Header.Set("If-None-Match", w.Header().Get("ETag"))
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, again)
	if w.Header().Get("ETag") == "" || w2.Code != http.StatusNotModified {
		t.Errorf("revalidation = %d (ETag %q)", w2.Code, w.Header().Get("ETag"))
	}
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, httptest.NewRequest("HEAD", "/third-party-notices.txt", nil))
	if w3.Code != http.StatusOK || w3.Body.Len() != 0 {
		t.Errorf("HEAD = %d with %d bytes", w3.Code, w3.Body.Len())
	}
	w4 := httptest.NewRecorder()
	h.ServeHTTP(w4, httptest.NewRequest("POST", "/third-party-notices.txt", strings.NewReader("x")))
	if w4.Code != http.StatusMethodNotAllowed || w4.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST = %d", w4.Code)
	}
	// Every other path is not its business.
	w5 := httptest.NewRecorder()
	h.ServeHTTP(w5, httptest.NewRequest("GET", "/third-party-notices.txt/x", nil))
	if rest != 1 || w5.Body.String() != "everything else" {
		t.Errorf("another path was not handed on (%d, %q)", rest, w5.Body.String())
	}
}

// The licence and its additional terms travel with the binary too, at
// paths of their own, served like the notices.
func TestLicenseAndAdditionalTermsAreServed(t *testing.T) {
	h := withNotices(http.NotFoundHandler())
	for path, want := range map[string]struct{ text, mark string }{
		licensePath:         {hmnpay.License, "GNU GENERAL PUBLIC LICENSE"},
		additionalTermsPath: {hmnpay.AdditionalTerms, "Powered by hmnpay"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Header().Get("X-Content-Type-Options") != "nosniff" ||
			!strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") || w.Header().Get("ETag") == "" {
			t.Fatalf("GET %s: %d %v", path, w.Code, w.Header())
		}
		if w.Body.String() != want.text || !strings.Contains(want.text, want.mark) {
			t.Errorf("GET %s did not serve the embedded text (%d bytes)", path, w.Body.Len())
		}
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, httptest.NewRequest("PUT", path, strings.NewReader("x")))
		if w2.Code != http.StatusMethodNotAllowed {
			t.Errorf("PUT %s = %d", path, w2.Code)
		}
	}
	// The embedded texts are the files of the source tree, byte for byte.
	for file, text := range map[string]string{"../../LICENSE": hmnpay.License, "../../ADDITIONAL_TERMS.md": hmnpay.AdditionalTerms} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != text {
			t.Errorf("%s differs from the embedded text", file)
		}
	}
	if !strings.Contains(hmnpay.License, "Version 3, 29 June 2007") {
		t.Error("LICENSE is not the GPL version 3 text")
	}
}

// What the notices must name, checked against what the build really uses:
// every Go module of go.mod and every package the web interface declares as
// a dependency, the fonts and the vendored code, each with a licence.
func TestThirdPartyNoticesCoverTheBuild(t *testing.T) {
	notices := hmnpay.ThirdPartyNotices
	for _, want := range []string{
		"SIL OPEN FONT LICENSE", "The Geist Project Authors",
		"distortion-waves", "Jakub Antalik",
		"shadcn", "MIT License", "Apache License", "BSD 3-Clause", "ISC License",
		"The Go Authors",
	} {
		if !strings.Contains(notices, want) {
			t.Errorf("the notices do not contain %q", want)
		}
	}
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(gomod), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.Contains(f[0], ".") || !strings.HasPrefix(f[1], "v") {
			continue
		}
		if !strings.Contains(notices, f[0]+" "+f[1]) {
			t.Errorf("Go module %s %s is not in the notices: run scripts/third-party-notices.mjs", f[0], f[1])
		}
	}
	raw, err := os.ReadFile("../../apps/web/package.json")
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatal(err)
	}
	// A dependency that is only a front for other packages is in the bundle
	// under their names: the umbrella packages of Radix and Shiki, and React,
	// which Next.js ships in builds of its own.
	under := map[string]string{
		"radix-ui": "\n- @radix-ui/react-dialog ", "shiki": "\n- @shikijs/core ",
		"react": "\n- react-builtin ", "react-dom": "\n- react-dom-builtin ",
	}
	for name := range pkg.Dependencies {
		want := "\n- " + name + " "
		if alias, ok := under[name]; ok {
			want = alias
		}
		if !strings.Contains(notices, want) {
			t.Errorf("npm package %s is not in the notices (looked for %q): run scripts/third-party-notices.mjs", name, strings.TrimSpace(want))
		}
	}
}
