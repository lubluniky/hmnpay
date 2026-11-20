package httpapi

// Regression tests for the operator's way back in: a login flood must not
// lock the operator out of every browser the instance has not seen (the
// recovery code gets past it), and the "known device" marker is bound to the
// credentials, so a password reset ends it.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

func (e *tenv) postLogin(remote string, body map[string]any, h hdr) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(req{method: "POST", path: "/api/v1/auth/login", remote: remote, body: body, h: h})
}

// exhaustSharedLoginBuckets plays the anonymous flood: wrong logins from
// many addresses until neither the per-account nor the instance-wide bucket
// has a token left.
func (e *tenv) exhaustSharedLoginBuckets() {
	e.t.Helper()
	for i := 0; i < 50; i++ {
		w := e.postLogin(fmt.Sprintf("198.51.100.%d:1", i+1), map[string]any{"email": e.Email, "password": "wrong password"}, nil)
		if w.Code == http.StatusTooManyRequests {
			return
		}
	}
	e.t.Fatal("the shared login buckets never ran out")
}

func deviceCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == deviceCookieSecure {
			return c
		}
	}
	return nil
}

// Anonymous traffic can keep the shared login buckets empty, and a
// browser the instance has never seen is subject to them: the operator who
// lost their browser (or just reset the password, which forgets every
// browser) could not log in at all. The way back in is a one-time recovery
// code printed on the server, presented in the login body.
func TestRecoveryCodeGetsPastTheSharedBuckets(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) {
		o.Limits.LoginPerAccount = Rate{Burst: 2, Every: time.Hour}
		o.Limits.LoginGlobal = Rate{Burst: 4, Every: time.Hour}
	}))
	creds := func(password, code string) map[string]any {
		b := map[string]any{"email": e.Email, "password": password}
		if code != "" {
			b["recovery_code"] = code
		}
		return b
	}
	e.exhaustSharedLoginBuckets()
	// Locked out: a new browser with the right password is told to wait.
	wantError(t, e.postLogin("203.0.113.7:1", creds(e.Password, ""), nil), http.StatusTooManyRequests, "rate_limited")

	code, err := e.Service.MintRecoveryCode(e.System())
	if err != nil {
		t.Fatal(err)
	}
	// A made-up code opens nothing and is refused before any password is
	// looked at — also with the right password.
	wantError(t, e.postLogin("203.0.113.7:1", creds(e.Password, "hmnr_00000000000000000000000000"), nil), http.StatusUnauthorized, "recovery_code_invalid")
	// The code is read from the body only. In the query string or in a
	// header it is ignored (and so not worth putting there, where it would
	// be logged): the attempt is an ordinary one, and throttled.
	for name, rq := range map[string]req{
		"query string": {method: "POST", path: "/api/v1/auth/login?recovery_code=" + code.Code, remote: "203.0.113.8:1", body: creds(e.Password, "")},
		"header": {method: "POST", path: "/api/v1/auth/login", remote: "203.0.113.9:1", body: creds(e.Password, ""),
			h: hdr{"X-Hmnpay-Recovery-Code": code.Code, "Authorization": "Bearer " + code.Code}},
	} {
		if w := e.do(rq); w.Code != http.StatusTooManyRequests && w.Code != http.StatusUnauthorized {
			t.Fatalf("recovery code in the %s: status %d", name, w.Code)
		} else if len(w.Result().Cookies()) != 0 {
			t.Fatalf("recovery code in the %s opened a session", name)
		}
	}
	// It replaces no password.
	wantError(t, e.postLogin("203.0.113.7:1", creds("wrong password", code.Code), nil), http.StatusUnauthorized, "invalid_credentials")
	// With the password it gets the operator in, on a browser the instance
	// has never seen, while the shared buckets are still empty.
	w := e.postLogin("203.0.113.7:1", creds(e.Password, code.Code), nil)
	wantStatus(t, w, http.StatusOK)
	marker := deviceCookie(t, w)
	if marker == nil {
		t.Fatalf("the recovered browser is not remembered: %v", w.Result().Cookies())
	}
	// Single use.
	wantError(t, e.postLogin("203.0.113.10:1", creds(e.Password, code.Code), nil), http.StatusUnauthorized, "recovery_code_invalid")
	// From now on that browser is a known device again.
	wantStatus(t, e.postLogin("203.0.113.11:1", creds(e.Password, ""), hdr{"Cookie": marker.Name + "=" + marker.Value}), http.StatusOK)
	wantError(t, e.postLogin("203.0.113.12:1", creds(e.Password, ""), nil), http.StatusTooManyRequests, "rate_limited")

	// Its own bound: a code is good for five attempts, whoever makes them
	// and from wherever, so it cannot be used to keep the KDF busy either.
	code, err = e.Service.MintRecoveryCode(e.System())
	if err != nil {
		t.Fatal(err)
	}
	for i := range core.RecoveryCodeMaxAttempts {
		wantError(t, e.postLogin(fmt.Sprintf("192.0.2.%d:1", i+100), creds("wrong password", code.Code), nil), http.StatusUnauthorized, "invalid_credentials")
	}
	wantError(t, e.postLogin("192.0.2.200:1", creds(e.Password, code.Code), nil), http.StatusUnauthorized, "recovery_code_invalid")

	// The per-address bucket applies to recovery attempts like to any other.
	e2 := newTenv(t, withOpts(func(o *Options) { o.Limits.LoginPerIP = Rate{Burst: 2, Every: time.Hour} }))
	code, err = e2.Service.MintRecoveryCode(e2.System())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		wantError(t, e2.postLogin("203.0.113.7:1", map[string]any{"email": e2.Email, "password": "wrong password", "recovery_code": code.Code}, nil),
			http.StatusUnauthorized, "invalid_credentials")
	}
	wantError(t, e2.postLogin("203.0.113.7:1", map[string]any{"email": e2.Email, "password": e2.Password, "recovery_code": code.Code}, nil),
		http.StatusTooManyRequests, "rate_limited")

	// No code ever reaches the log.
	for _, env := range []*tenv{e, e2} {
		if logs := env.logs.String(); strings.Contains(logs, "hmnr_") {
			t.Fatalf("a recovery code was logged:\n%s", logs)
		}
	}
}

// A known-device marker that were only HMAC(key, expiry) would be valid for
// 180 days for whoever held a copy, and still valid after `hmnpay
// reset-password` until the server restarted. It is bound to the account's
// credentials instead: a password reset or change kills every marker at once.
func TestDeviceMarkerDiesWithThePassword(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) {
		o.Limits.LoginPerAccount = Rate{Burst: 2, Every: time.Hour}
		o.Limits.LoginGlobal = Rate{Burst: 4, Every: time.Hour}
	}))
	creds := func(password string) map[string]any { return map[string]any{"email": e.Email, "password": password} }
	known := func(c *http.Cookie) hdr { return hdr{"Cookie": c.Name + "=" + c.Value} }

	w := e.postLogin("192.0.2.50:1", creds(e.Password), nil)
	wantStatus(t, w, http.StatusOK)
	old := deviceCookie(t, w)
	if old == nil {
		t.Fatal("no device marker")
	}
	e.exhaustSharedLoginBuckets()
	wantStatus(t, e.postLogin("192.0.2.51:1", creds(e.Password), known(old)), http.StatusOK)

	// reset-password on the server: no restart, no key rotation.
	const reset = "a brand new passphrase"
	if err := e.Service.ResetOperatorPassword(e.System(), reset); err != nil {
		t.Fatal(err)
	}
	wantError(t, e.postLogin("192.0.2.52:1", creds(reset), known(old)), http.StatusTooManyRequests, "rate_limited")
	// The owner comes back with the recovery code and is remembered anew.
	code, err := e.Service.MintRecoveryCode(e.System())
	if err != nil {
		t.Fatal(err)
	}
	w = e.postLogin("192.0.2.53:1", map[string]any{"email": e.Email, "password": reset, "recovery_code": code.Code}, nil)
	wantStatus(t, w, http.StatusOK)
	fresh := deviceCookie(t, w)
	if fresh == nil || fresh.Value == old.Value {
		t.Fatalf("no fresh marker after the reset: %v", w.Result().Cookies())
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieSecure {
			session = c
		}
	}
	wantStatus(t, e.postLogin("192.0.2.54:1", creds(reset), known(fresh)), http.StatusOK)

	// Changing the password in the dashboard kills the markers too — and
	// gives the browser that did it a new one in the same response.
	const changed = "yet another passphrase"
	w = e.do(req{method: "POST", path: "/api/v1/operator/password",
		h:    hdr{"Cookie": session.Name + "=" + session.Value, "Origin": testOrigin, "Sec-Fetch-Site": "same-origin"},
		body: map[string]any{"current_password": reset, "new_password": changed}})
	wantStatus(t, w, http.StatusNoContent)
	renewed := deviceCookie(t, w)
	if renewed == nil || renewed.Value == fresh.Value {
		t.Fatalf("changing the password did not renew this browser's marker: %v", w.Result().Cookies())
	}
	wantError(t, e.postLogin("192.0.2.55:1", creds(changed), known(fresh)), http.StatusTooManyRequests, "rate_limited")
	wantStatus(t, e.postLogin("192.0.2.56:1", creds(changed), known(renewed)), http.StatusOK)
	// An API client that changes the password with a bearer token gets no
	// cookie.
	tokenLogin := e.postLogin("192.0.2.57:1", map[string]any{"email": e.Email, "password": changed, "issue_token": true}, known(renewed))
	wantStatus(t, tokenLogin, http.StatusOK)
	token := decodeBody[struct {
		Token string `json:"token"`
	}](t, tokenLogin).Token
	w = e.do(req{method: "POST", path: "/api/v1/operator/password", h: bearer(token),
		body: map[string]any{"current_password": changed, "new_password": "and one more passphrase"}})
	wantStatus(t, w, http.StatusNoContent)
	if len(w.Result().Cookies()) != 0 {
		t.Fatalf("a bearer-token password change set cookies: %v", w.Result().Cookies())
	}
}

// A marker skips the shared buckets, so it needs a bound of its
// own: whoever holds one valid marker (a former operator, a copied cookie)
// could otherwise spread password attempts over any number of addresses.
func TestOneMarkerBuysABoundedNumberOfAttempts(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) { o.Limits.LoginPerDevice = Rate{Burst: 3, Every: time.Hour} }))
	creds := func(password string) map[string]any { return map[string]any{"email": e.Email, "password": password} }
	w := e.postLogin("192.0.2.50:1", creds(e.Password), nil)
	wantStatus(t, w, http.StatusOK)
	marker := deviceCookie(t, w)
	known := hdr{"Cookie": marker.Name + "=" + marker.Value}
	// A successful login costs the marker nothing.
	for i := range 5 {
		wantStatus(t, e.postLogin(fmt.Sprintf("192.0.2.%d:1", 60+i), creds(e.Password), known), http.StatusOK)
	}
	for i := range 3 {
		wantError(t, e.postLogin(fmt.Sprintf("198.51.100.%d:1", i+1), creds("wrong password"), known), http.StatusUnauthorized, "invalid_credentials")
	}
	// The fourth failure in a row, from a fourth address: the marker's own
	// bucket is empty.
	blocked := e.postLogin("198.51.100.4:1", creds("wrong password"), known)
	wantError(t, blocked, http.StatusTooManyRequests, "rate_limited")
	if blocked.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// The same browser without the marker is an unknown device and falls
	// under the shared buckets instead — which still have room here.
	wantStatus(t, e.postLogin("198.51.100.5:1", creds(e.Password), nil), http.StatusOK)
	// Another marker has its own bucket.
	other := deviceCookie(t, e.postLogin("198.51.100.6:1", creds(e.Password), nil))
	if other == nil {
		t.Fatal("no second marker")
	}
	if other.Value == marker.Value {
		// Two markers issued in the same second are the same marker.
		binding, err := e.Service.DeviceBinding(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		other.Value = e.h.deviceMarker(time.Now().Add(2*time.Hour), binding)
	}
	wantError(t, e.postLogin("198.51.100.7:1", creds("wrong password"), hdr{"Cookie": other.Name + "=" + other.Value}), http.StatusUnauthorized, "invalid_credentials")
}
