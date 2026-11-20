package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/lubluniky/hmnpay"
)

// The paths of the texts the binary carries, whatever serves the rest of
// the interface. The dashboard's About block links to each.
const (
	licensePath         = "/license.txt"
	additionalTermsPath = "/additional-terms.txt"
	noticesPath         = "/third-party-notices.txt"
)

// legalTexts maps each path to the text built into the binary. The binary
// travels without the source tree: the GPL asks that its text accompanies
// the program, the additional terms are part of the licence, and the
// licences of what is compiled into it ask that their text travels with it.
// This is where a person who has only the running product can read them.
var legalTexts = map[string]string{
	licensePath:         hmnpay.License,
	additionalTermsPath: hmnpay.AdditionalTerms,
	noticesPath:         hmnpay.ThirdPartyNotices,
}

// withNotices serves the texts of legalTexts at their paths, as plain text
// that can only be read, and hands every other request to next.
func withNotices(next http.Handler) http.Handler {
	type doc struct {
		body []byte
		etag string
	}
	docs := make(map[string]doc, len(legalTexts))
	for path, text := range legalTexts {
		body := []byte(text)
		sum := sha256.Sum256(body)
		docs[path] = doc{body: body, etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, ok := docs[r.URL.Path]
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Permissions-Policy", uiPermissionsPolicy)
		hdr.Set("Cross-Origin-Opener-Policy", "same-origin")
		hdr.Set("Cross-Origin-Resource-Policy", "same-origin")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			hdr.Set("Allow", "GET, HEAD")
			hdr.Set("Cache-Control", "no-store")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		hdr.Set("Cache-Control", "no-cache")
		hdr.Set("ETag", d.etag)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(d.body))
	})
}
