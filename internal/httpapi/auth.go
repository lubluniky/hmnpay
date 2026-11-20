package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// Session cookie names. Over HTTPS the "__Host-" prefix makes browsers
// insist on Secure, Path=/ and no Domain, so a sibling subdomain or a
// plain-HTTP response cannot plant or overwrite it.
const (
	cookieSecure   = "__Host-hmn_session"
	cookieInsecure = "hmn_session"
)

func (h *Handler) secure(r *http.Request) bool {
	if h.secureFixed != nil {
		return *h.secureFixed
	}
	return h.requestIsHTTPS(r)
}

func (h *Handler) cookieName(r *http.Request) string {
	if h.secure(r) {
		return cookieSecure
	}
	return cookieInsecure
}

// sessionCookie returns the session token from the cookie, if any. Only
// the cookie matching the current scheme is read: on an HTTPS instance a
// non-prefixed cookie (which a network attacker could plant over plain
// HTTP) is ignored.
func (h *Handler) sessionCookie(r *http.Request) string {
	c, err := r.Cookie(h.cookieName(r))
	if err != nil {
		return ""
	}
	return c.Value
}

// setSessionCookie sets the session cookie. Its lifetime is the core
// session's own lifetime (ExpiresAt − CreatedAt), so it does not depend on
// agreement between the core's clock and the wall clock.
func (h *Handler) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, sess core.Session) {
	maxAge := int(sess.ExpiresAt.Sub(sess.CreatedAt) / time.Second)
	if maxAge < 1 {
		maxAge = 1
	}
	expires := time.Now().Add(time.Duration(maxAge) * time.Second)
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookieName(r),
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		Expires:  expires.UTC(),
		HttpOnly: true,
		Secure:   h.secure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookieName(r),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// credential is what a request presented.
type credential struct {
	token  string
	cookie bool // ambient (cookie) rather than an explicit bearer token
}

// credentials extracts the request's credential. An Authorization header
// wins over the cookie; a malformed Authorization header is an error, not
// a fallback to the cookie. Tokens are never read from the query string.
func (h *Handler) credentials(r *http.Request) (credential, error) {
	if authz := r.Header.Get("Authorization"); authz != "" {
		scheme, token, ok := strings.Cut(authz, " ")
		token = strings.TrimSpace(token)
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
			return credential{}, &apiError{Status: http.StatusUnauthorized, Code: "invalid_credentials", Message: "Authorization must be \"Bearer <token>\""}
		}
		return credential{token: token}, nil
	}
	if t := h.sessionCookie(r); t != "" {
		return credential{token: t, cookie: true}, nil
	}
	return credential{}, nil
}

// resolve turns a credential into an actor via the core.
func (h *Handler) resolve(ctx context.Context, c credential) (core.Actor, error) {
	if strings.HasPrefix(c.token, "hmns_") {
		return h.svc.AuthenticateSession(ctx, c.token)
	}
	if c.cookie {
		return core.Actor{}, core.ErrInvalidCredentials
	}
	return h.svc.AuthenticateAPIKey(ctx, c.token)
}

// authenticate attaches the caller's actor to the request context.
//
// Required mode: no credential → 401; an invalid credential → 401, and it
// is charged to the IP's auth-failure bucket — once that is empty, further
// invalid credentials from the IP get 429 instead. Valid credentials are
// never throttled here (a broken integration with a revoked key must not
// lock out a healthy one behind the same address); guessing is defeated
// by token entropy (≥125 bits), the bucket only bounds the noise. A
// cookie-authenticated unsafe request must pass the cross-origin check
// (403 otherwise). Optional mode uses a valid credential and proceeds
// anonymously otherwise. Either way the core makes every authorisation
// decision.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, mode authMode) (context.Context, error) {
	ctx := r.Context()
	ri := info(r)
	key := h.rateKey(r)
	c, err := h.credentials(r)
	if err != nil {
		if mode == authOptional {
			return ctx, nil
		}
		return nil, h.authFailure(key, err)
	}
	if c.token == "" {
		if mode == authOptional {
			return ctx, nil
		}
		return nil, &apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "authentication required"}
	}
	actor, err := h.resolve(ctx, c)
	if err != nil {
		if mode == authOptional && core.KindOf(err) == core.KindUnauthorized {
			return ctx, nil
		}
		if core.KindOf(err) == core.KindUnauthorized {
			if c.cookie {
				h.clearSessionCookie(w, r)
			}
			return nil, h.authFailure(key, err)
		}
		return nil, err
	}
	if c.cookie && unsafeMethod(r.Method) {
		if err := h.cop.Check(r); err != nil {
			return nil, errCSRF
		}
	}
	ri.actor, ri.actorID = string(actor.Kind), actor.ID
	return core.WithActor(ctx, actor), nil
}

// authFailure charges an invalid credential to the IP's bucket and turns
// the 401 into a 429 once the bucket is empty.
func (h *Handler) authFailure(key string, err error) error {
	if lerr := h.lim.authFail.take(key); lerr != nil {
		return lerr
	}
	return err
}

var errCSRF = &apiError{Status: http.StatusForbidden, Code: "csrf_rejected", Message: "cross-origin request rejected"}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// crossOriginCheck guards unauthenticated browser-facing unsafe endpoints
// (login, logout, setup) against login-CSRF. Non-browser clients send
// neither Origin nor Sec-Fetch-Site and pass.
func (h *Handler) crossOriginCheck(r *http.Request) error {
	if err := h.cop.Check(r); err != nil {
		return errCSRF
	}
	return nil
}

// ---- known-device marker ----

// A browser that has logged in successfully gets a signed marker cookie.
// It is not a credential: it grants nothing but exemption from the shared
// login buckets (per account, instance-wide), so that nobody can keep the
// operator out of a browser they have used by exhausting those buckets.
//
// The marker is bound to the operator account and its current credentials
// (core.Service.DeviceBinding): changing or resetting the password makes
// every marker issued before worthless at once, with no restart and no key
// rotation. Each marker is also its own rate-limit key
// ([Limits.LoginPerDevice]), so holding one — a former operator, a copied
// cookie — buys a bounded number of password attempts, not a way around
// the bounds.
const (
	deviceCookieSecure   = "__Host-hmn_device"
	deviceCookieInsecure = "hmn_device"
	deviceMarkerLifetime = 180 * 24 * time.Hour
	deviceMarkerContext  = "hmnpay known-device marker v2"
)

func (h *Handler) deviceCookieName(r *http.Request) string {
	if h.secure(r) {
		return deviceCookieSecure
	}
	return deviceCookieInsecure
}

// deviceMarker returns "<expiry unix>.<base64url HMAC>" for the given
// account binding.
func (h *Handler) deviceMarker(expires time.Time, binding string) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, h.deviceKey)
	mac.Write([]byte(deviceMarkerContext))
	mac.Write([]byte{0})
	mac.Write([]byte(binding))
	mac.Write([]byte{0})
	mac.Write([]byte(exp))
	return exp + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// setDeviceCookie marks the browser as a known device for the account's
// current credentials. Without a binding (the core is unreachable) it sets
// nothing: the browser is then simply unknown next time.
func (h *Handler) setDeviceCookie(w http.ResponseWriter, r *http.Request) {
	binding, err := h.svc.DeviceBinding(r.Context())
	if err != nil {
		return
	}
	expires := h.now().Add(deviceMarkerLifetime)
	http.SetCookie(w, &http.Cookie{
		Name:     h.deviceCookieName(r),
		Value:    h.deviceMarker(expires, binding),
		Path:     "/",
		MaxAge:   int(deviceMarkerLifetime / time.Second),
		Expires:  expires.UTC(),
		HttpOnly: true,
		Secure:   h.secure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// knownDevice reports whether the request carries a marker that is valid,
// unexpired and bound to the account's current credentials. key is the
// rate-limit key of that marker (a digest of it), empty otherwise.
func (h *Handler) knownDevice(r *http.Request) (key string, ok bool) {
	c, err := r.Cookie(h.deviceCookieName(r))
	if err != nil || len(c.Value) > 128 {
		return "", false
	}
	exp, _, cut := strings.Cut(c.Value, ".")
	if !cut {
		return "", false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || !h.now().Before(time.Unix(unix, 0)) {
		return "", false
	}
	binding, err := h.svc.DeviceBinding(r.Context())
	if err != nil {
		return "", false
	}
	if !hmac.Equal([]byte(c.Value), []byte(h.deviceMarker(time.Unix(unix, 0), binding))) {
		return "", false
	}
	sum := sha256.Sum256([]byte(c.Value))
	return "dev:" + base64.RawURLEncoding.EncodeToString(sum[:12]), true
}
