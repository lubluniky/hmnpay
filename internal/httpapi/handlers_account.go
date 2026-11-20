package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// ---- health ----

func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports readiness without details: 200 when the store answers
// (the instance can serve the setup wizard or the dashboard), 503
// otherwise. The booleans tell an operator's monitor whether setup is done
// and whether the chain watcher looks healthy; they do not gate readiness,
// because checkouts and the dashboard keep working while the node lags.
func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) error {
	st, err := h.svc.SetupStatus(r.Context())
	if err != nil {
		h.toAPIError(r, err) // logs internal errors
		return writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable"})
	}
	chain, starting := false, false
	if st.Completed {
		if ns, err := h.svc.NodeStatus(systemContext(r.Context())); err == nil {
			chain, starting = ns.Healthy, ns.Starting
		}
	}
	return writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "setup_completed": st.Completed,
		"chain_healthy": chain, "chain_starting": starting})
}

// ---- session ----

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// IssueToken asks for the session token in the response body (for
	// bots and CLIs that send it as a bearer token) instead of a cookie.
	IssueToken bool `json:"issue_token"`
	// RecoveryCode is the one-time code printed by `hmnpay recovery-code`
	// (and by `hmnpay reset-password`, with the new password) on the
	// server, for an operator the shared login throttles keep out.
	// It is accepted here, in the request body, and nowhere else: never
	// in the query string or a header, which end up in logs and histories.
	RecoveryCode string `json:"recovery_code"`
}

// login opens an operator session.
//
// Throttling. Every attempt runs the password KDF, so attempts are bounded
// at four levels before the password is checked (tokens are taken first,
// so parallel guesses cannot outrun a limit, and refunded when the login
// succeeds):
//
//   - per client IP, always;
//   - per submitted account and instance-wide ([Limits.LoginGlobal]), for
//     browsers that have not logged in here before. The per-account bucket
//     stops a distributed brute force against the single operator account;
//     the global one bounds a distributed burst of made-up logins;
//   - in the core, which runs only a few KDFs at a time and answers 503
//     busy (Retry-After) beyond that.
//
// The shared buckets would let anyone slow the real operator's logins down
// by hammering them, so two kinds of attempt skip both:
//
//   - A browser that has logged in successfully before: a successful login
//     sets a signed, long-lived "known device" cookie ([Options.DeviceKey])
//     bound to the account's current credentials. It proves nothing but an
//     earlier successful login with the current password. Instead of the
//     shared buckets it has a bucket of its own ([Limits.LoginPerDevice]),
//     so one marker used from any number of addresses still buys a bounded
//     number of attempts.
//   - An attempt that presents the one-time recovery code ("recovery_code"
//     in the body), which `hmnpay recovery-code` prints on the server:
//     the way in on a browser the instance has never seen while anonymous
//     traffic keeps the shared buckets empty. The core bounds it: at most
//     core.RecoveryCodeMaxAttempts attempts per code, one success, fifteen
//     minutes. A wrong code is refused (401 recovery_code_invalid) before
//     any password is verified.
//
// The per-IP bucket, the KDF bound and of course the password apply to
// every attempt. Failures are uniform: the core verifies a password whether
// or not the email matches.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) error {
	if err := h.crossOriginCheck(r); err != nil {
		return err
	}
	ipKey := h.rateKey(r)
	if err := h.lim.loginIP.take(ipKey); err != nil {
		return err
	}
	var in loginRequest
	if err := decodeJSON(w, r, &in, smallBodyLimit); err != nil {
		return err
	}
	acct := strings.ToLower(strings.TrimSpace(in.Email))
	if len(acct) > 254 {
		acct = acct[:254]
	}
	acctKey := "acct:" + acct
	recovery := strings.TrimSpace(in.RecoveryCode)
	deviceKey, known := "", false
	if recovery != "" {
		valid, err := h.svc.RecoveryCodeValid(r.Context(), recovery)
		if err != nil {
			return err
		}
		if !valid {
			return core.ErrRecoveryCodeInvalid // no password was verified
		}
	} else if deviceKey, known = h.knownDevice(r); known {
		if err := h.lim.loginDevice.take(deviceKey); err != nil {
			return err
		}
	} else {
		if err := h.lim.loginGlobal.take("global"); err != nil {
			return err
		}
		if err := h.lim.loginAccount.take(acctKey); err != nil {
			h.lim.loginGlobal.refund("global")
			return err
		}
	}
	res, err := h.svc.Login(r.Context(), core.LoginInput{
		Email: in.Email, Password: in.Password,
		UserAgent: r.UserAgent(), IP: info(r).ip.String(),
		RecoveryCode: recovery,
	})
	if err != nil {
		return err
	}
	h.lim.loginIP.refund(ipKey)
	switch {
	case recovery != "":
	case known:
		h.lim.loginDevice.refund(deviceKey)
	default:
		h.lim.loginGlobal.refund("global")
		h.lim.loginAccount.refund(acctKey)
	}
	body := newLoginResponse(res)
	if in.IssueToken {
		body.Token = res.Token
	} else {
		h.setSessionCookie(w, r, res.Token, res.Session)
		h.setDeviceCookie(w, r) // browsers only: a bot asking for a token keeps no cookies
	}
	ri := info(r)
	ri.actor, ri.actorID = string(core.ActorOperator), res.Operator.ID
	return writeJSON(w, http.StatusOK, body)
}

// logout ends the session presented as a bearer token or cookie and clears
// the cookie. It always answers 204: logging out twice is not an error. It
// is unauthenticated, so it is rate-limited per IP, and the core looks the
// session up in a read before it writes anything.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) error {
	if err := h.crossOriginCheck(r); err != nil {
		return err
	}
	c, err := h.credentials(r)
	if err == nil && strings.HasPrefix(c.token, "hmns_") {
		if err := h.svc.Logout(r.Context(), c.token); err != nil {
			return err
		}
	}
	h.clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

type meResponse struct {
	Kind     core.ActorKind `json:"kind"`
	Operator *operatorDTO   `json:"operator,omitempty"`
	// Session is the operator's current session: its id and when it stops
	// working (the dashboard uses it to warn before the session runs out).
	Session *sessionDTO `json:"session,omitempty"`
	APIKey  *meKeyDTO   `json:"api_key,omitempty"`
}

type meKeyDTO struct {
	ID     string       `json:"id"`
	Name   string       `json:"name"`
	Scopes []core.Scope `json:"scopes"`
}

// me describes the authenticated caller (from the actor the core
// returned; no extra lookup).
func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	a, _ := core.ActorFrom(r.Context())
	out := meResponse{Kind: a.Kind}
	switch a.Kind {
	case core.ActorOperator:
		out.Operator = &operatorDTO{ID: a.ID, Email: a.Name}
		if a.SessionID != "" {
			out.Session = &sessionDTO{ID: a.SessionID, ExpiresAt: a.SessionExpiresAt}
		}
	case core.ActorAPIKey:
		out.APIKey = &meKeyDTO{ID: a.ID, Name: a.Name, Scopes: nonNil(a.Scopes)}
	}
	return writeJSON(w, http.StatusOK, out)
}

// instanceDTO says which build is running.
type instanceDTO struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// instance reports the running build to any authenticated caller (an
// operator session or an API key of any scope). It is not public: the
// exact version of a server is nobody else's business.
func (h *Handler) instance(w http.ResponseWriter, _ *http.Request) error {
	out := instanceDTO{Version: h.opts.Version, Commit: h.opts.Commit}
	if out.Version == "" {
		out.Version = "dev"
	}
	if out.Commit == "" {
		out.Commit = "unknown"
	}
	return writeJSON(w, http.StatusOK, out)
}

// ---- setup ----

func (h *Handler) setupStatus(w http.ResponseWriter, r *http.Request) error {
	st, err := h.svc.SetupStatus(r.Context())
	if err != nil {
		return err
	}
	d := st.Defaults
	out := setupStatusDTO{
		Completed:          st.Completed,
		SetupTokenRequired: st.TokenRequired,
		MinPasswordLength:  st.MinPasswordLength,
		Defaults: setupDefaultsDTO{
			ChainID:        d.Network.ChainID,
			Asset:          newAssetDTO(d.Asset),
			DerivationPath: d.Wallet.DerivationPath,
			Payments:       newSettingsDTO(d).Payments,
		},
	}
	for _, id := range []uint64{core.ChainBSCMainnet, core.ChainBSCTestnet} {
		if a, ok := core.DefaultAsset(id); ok {
			out.SupportedChains = append(out.SupportedChains, chainDTO{ChainID: id, Testnet: core.IsTestnet(id), Asset: newAssetDTO(a)})
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

// setupTokenHeader carries the one-time setup token while the instance is
// not set up. The token is accepted in this header or in the request
// body's "setup_token" field — never in the query string, which ends up in
// logs, histories and Referer headers.
const setupTokenHeader = "X-Hmnpay-Setup-Token"

// setupToken returns the setup token a request presents.
func setupToken(r *http.Request, bodyField string) string {
	if t := strings.TrimSpace(r.Header.Get(setupTokenHeader)); t != "" {
		return t
	}
	return strings.TrimSpace(bodyField)
}

// setupError post-processes an error of a setup call: a wrong setup token
// is a failed credential and is charged to the IP's auth-failure bucket,
// like a bad API key (429 once the bucket is empty).
func (h *Handler) setupError(r *http.Request, err error) error {
	if errors.Is(err, core.ErrSetupTokenInvalid) {
		return h.authFailure(h.rateKey(r), err)
	}
	return err
}

type probeRequest struct {
	URL        string    `json:"url"`
	ChainID    uint64    `json:"chain_id"`
	Asset      *assetDTO `json:"asset"`
	SetupToken string    `json:"setup_token"`
}

// probeRPC is shared by the setup wizard and the network settings page.
// Before setup it needs the setup token (401 setup_token_required, 403
// setup_token_invalid); afterwards the operator.
func (h *Handler) probeRPC(w http.ResponseWriter, r *http.Request) error {
	var in probeRequest
	if err := decodeJSON(w, r, &in, smallBodyLimit); err != nil {
		return err
	}
	res, err := h.svc.ProbeRPC(r.Context(), core.ProbeRPCInput{URL: in.URL, ChainID: in.ChainID, Asset: in.Asset.core(), SetupToken: setupToken(r, in.SetupToken)})
	if err != nil {
		return h.setupError(r, err)
	}
	f := res.Facts
	return writeJSON(w, http.StatusOK, probeResultDTO{
		Outcome: res.Outcome, Expected: newAssetDTO(res.Expected),
		Facts: probeFactsDTO{Reachable: f.Reachable, ChainID: f.ChainID, BlockNumber: f.BlockNumber, LatencyMs: f.LatencyMs,
			TokenFound: f.TokenFound, TokenDecimals: f.TokenDecimals, TokenSymbol: f.TokenSymbol, Error: f.Error},
	})
}

type previewRequest struct {
	XPub           string `json:"xpub"`
	DerivationPath string `json:"derivation_path"`
	Count          int    `json:"count"`
	SetupToken     string `json:"setup_token"`
}

func (h *Handler) previewAddresses(w http.ResponseWriter, r *http.Request) error {
	var in previewRequest
	if err := decodeJSON(w, r, &in, smallBodyLimit); err != nil {
		return err
	}
	addrs, err := h.svc.PreviewAddresses(r.Context(), core.PreviewAddressesInput{XPub: in.XPub, DerivationPath: in.DerivationPath, Count: in.Count, SetupToken: setupToken(r, in.SetupToken)})
	if err != nil {
		return h.setupError(r, err)
	}
	out := make([]derivedAddressDTO, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, derivedAddressDTO{Index: a.Index, Path: a.Path, Address: a.Address})
	}
	return writeJSON(w, http.StatusOK, listBody[derivedAddressDTO]{Data: out})
}

// setupRequest is flat so that every field error the core reports
// ("merchant_name", "rpc_urls.0", "xpub", …) names a key of this body.
type setupRequest struct {
	Email          string    `json:"email"`
	Password       string    `json:"password"`
	ChainID        uint64    `json:"chain_id"`
	Asset          *assetDTO `json:"asset"`
	RPCURLs        []string  `json:"rpc_urls"`
	XPub           string    `json:"xpub"`
	DerivationPath string    `json:"derivation_path"`
	MerchantName   string    `json:"merchant_name"`
	LogoURL        string    `json:"logo_url"`
	SupportEmail   string    `json:"support_email"`
	SupportURL     string    `json:"support_url"`
	// PublicURL defaults to Options.PublicURL, else the request's origin.
	PublicURL string `json:"public_url"`
	// SetupToken is the instance's setup token (or send it in the
	// X-Hmnpay-Setup-Token header).
	SetupToken string `json:"setup_token"`
}

// completeSetup runs the wizard's final step and signs the new operator in
// with a session cookie. When the instance was started with a setup token
// it answers 401 setup_token_required / 403 setup_token_invalid without
// it, whatever else the body contains.
func (h *Handler) completeSetup(w http.ResponseWriter, r *http.Request) error {
	if err := h.crossOriginCheck(r); err != nil {
		return err
	}
	var in setupRequest
	if err := decodeJSON(w, r, &in, largeBodyLimit); err != nil {
		return err
	}
	publicURL := strings.TrimSpace(in.PublicURL)
	if publicURL == "" {
		publicURL = h.opts.PublicURL
	}
	if publicURL == "" && r.Host != "" {
		scheme := "http"
		if h.requestIsHTTPS(r) {
			scheme = "https"
		}
		publicURL = scheme + "://" + r.Host
	}
	res, err := h.svc.CompleteSetup(r.Context(), core.SetupInput{
		Email: in.Email, Password: in.Password,
		ChainID: in.ChainID, Asset: in.Asset.core(), RPCURLs: in.RPCURLs,
		XPub: in.XPub, DerivationPath: in.DerivationPath,
		Merchant:  core.MerchantSettings{Name: in.MerchantName, LogoURL: in.LogoURL, SupportEmail: in.SupportEmail, SupportURL: in.SupportURL},
		PublicURL: publicURL,
		UserAgent: r.UserAgent(), IP: info(r).ip.String(),
		SetupToken: setupToken(r, in.SetupToken),
	})
	if err != nil {
		return h.setupError(r, err)
	}
	h.setSessionCookie(w, r, res.Token, res.Session)
	h.setDeviceCookie(w, r)
	ri := info(r)
	ri.actor, ri.actorID = string(core.ActorOperator), res.Operator.ID
	return writeJSON(w, http.StatusCreated, newLoginResponse(res))
}

// ---- settings ----

func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) error {
	st, err := h.svc.GetSettings(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, newSettingsDTO(st))
}

type merchantRequest struct {
	MerchantName string `json:"merchant_name"`
	LogoURL      string `json:"logo_url"`
	SupportEmail string `json:"support_email"`
	SupportURL   string `json:"support_url"`
	PublicURL    string `json:"public_url"`
}

func (h *Handler) updateMerchant(w http.ResponseWriter, r *http.Request) error {
	var in merchantRequest
	if err := decodeJSON(w, r, &in, largeBodyLimit); err != nil {
		return err
	}
	st, err := h.svc.UpdateMerchant(r.Context(), core.UpdateMerchantInput{
		Merchant:  core.MerchantSettings{Name: in.MerchantName, LogoURL: in.LogoURL, SupportEmail: in.SupportEmail, SupportURL: in.SupportURL},
		PublicURL: in.PublicURL,
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, newSettingsDTO(st))
}

type paymentDefaultsRequest struct {
	TTLSeconds            int64  `json:"ttl_seconds"`
	RequiredConfirmations int    `json:"required_confirmations"`
	UnderpaymentTolerance string `json:"underpayment_tolerance"`
	DefaultRedirectURL    string `json:"default_redirect_url"`
}

func (h *Handler) updatePaymentDefaults(w http.ResponseWriter, r *http.Request) error {
	var in paymentDefaultsRequest
	if err := decodeJSON(w, r, &in, defaultBodyLimit); err != nil {
		return err
	}
	ttl, err := seconds("ttl_seconds", in.TTLSeconds)
	if err != nil {
		return err
	}
	st, err := h.svc.UpdatePaymentDefaults(r.Context(), core.PaymentSettings{
		TTL: ttl, RequiredConfirmations: in.RequiredConfirmations,
		UnderpaymentTolerance: in.UnderpaymentTolerance, DefaultRedirectURL: in.DefaultRedirectURL,
	})
	if err != nil {
		return withFieldNames(err, map[string]string{"ttl": "ttl_seconds"})
	}
	return writeJSON(w, http.StatusOK, newSettingsDTO(st))
}

// seconds converts a seconds field, rejecting values that would overflow.
func seconds(field string, n int64) (time.Duration, error) {
	if n < 0 || n > int64((1<<62)/time.Second) {
		return 0, errQuery(field, field+" is out of range")
	}
	return time.Duration(n) * time.Second, nil
}

type networkRequest struct {
	ChainID uint64    `json:"chain_id"`
	Asset   *assetDTO `json:"asset"`
	RPCURLs []string  `json:"rpc_urls"`
}

func (h *Handler) updateNetwork(w http.ResponseWriter, r *http.Request) error {
	var in networkRequest
	if err := decodeJSON(w, r, &in, defaultBodyLimit); err != nil {
		return err
	}
	st, err := h.svc.UpdateNetwork(r.Context(), core.UpdateNetworkInput{ChainID: in.ChainID, Asset: in.Asset.core(), RPCURLs: in.RPCURLs})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, newSettingsDTO(st))
}

type walletRequest struct {
	XPub             string  `json:"xpub"`
	DerivationPath   string  `json:"derivation_path"`
	NextAddressIndex *uint32 `json:"next_address_index"`
}

func (h *Handler) updateWallet(w http.ResponseWriter, r *http.Request) error {
	var in walletRequest
	if err := decodeJSON(w, r, &in, defaultBodyLimit); err != nil {
		return err
	}
	st, err := h.svc.UpdateWallet(r.Context(), core.UpdateWalletInput{XPub: in.XPub, DerivationPath: in.DerivationPath, NextAddressIndex: in.NextAddressIndex})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, newSettingsDTO(st))
}

func (h *Handler) getOperator(w http.ResponseWriter, r *http.Request) error {
	op, err := h.svc.GetOperator(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, operatorDTO{ID: op.ID, Email: op.Email, CreatedAt: optTime(op.CreatedAt), UpdatedAt: optTime(op.UpdatedAt)})
}

type passwordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// changePassword replaces the password; the core ends every other session
// and keeps the caller's.
func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) error {
	var in passwordRequest
	if err := decodeJSON(w, r, &in, smallBodyLimit); err != nil {
		return err
	}
	if err := h.svc.ChangePassword(r.Context(), in.CurrentPassword, in.NewPassword); err != nil {
		return err
	}
	// The new password made every known-device marker stale, this
	// browser's included; a browser session gets a fresh one.
	if h.sessionCookie(r) != "" {
		h.setDeviceCookie(w, r)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// listSessions lists the operator's live sessions, newest first; "current"
// marks the one this request used.
func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) error {
	sessions, err := h.svc.ListSessions(r.Context())
	if err != nil {
		return err
	}
	a, _ := core.ActorFrom(r.Context())
	out := make([]sessionInfoDTO, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, sessionInfoDTO{
			ID: s.ID, UserAgent: s.UserAgent, IP: s.IP, CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt, LastSeenAt: optTime(s.LastSeenAt),
			Current: a.SessionID != "" && s.ID == a.SessionID,
		})
	}
	return writeJSON(w, http.StatusOK, listBody[sessionInfoDTO]{Data: out})
}

// revokeSession ends one session by id. Revoking the session the request
// itself used is a logout: the cookie is cleared too.
func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if err := h.svc.RevokeSession(r.Context(), id); err != nil {
		return err
	}
	if a, _ := core.ActorFrom(r.Context()); a.SessionID == id {
		h.clearSessionCookie(w, r)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- API keys ----

type apiKeyRequest struct {
	Name      string       `json:"name"`
	Scopes    []core.Scope `json:"scopes"`
	ExpiresAt *time.Time   `json:"expires_at"`
}

// createAPIKey returns the key with its secret — the only time the secret
// is ever shown.
func (h *Handler) createAPIKey(w http.ResponseWriter, r *http.Request) error {
	var in apiKeyRequest
	if err := decodeJSON(w, r, &in, smallBodyLimit); err != nil {
		return err
	}
	var exp time.Time
	if in.ExpiresAt != nil {
		exp = *in.ExpiresAt
	}
	key, secret, err := h.svc.CreateAPIKey(r.Context(), core.CreateAPIKeyInput{Name: in.Name, Scopes: in.Scopes, ExpiresAt: exp})
	if err != nil {
		return err
	}
	out := newAPIKeyDTO(key)
	out.Secret = secret
	return writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) listAPIKeys(w http.ResponseWriter, r *http.Request) error {
	keys, err := h.svc.ListAPIKeys(r.Context())
	if err != nil {
		return err
	}
	out := make([]apiKeyDTO, 0, len(keys))
	for _, k := range keys {
		out = append(out, newAPIKeyDTO(k))
	}
	return writeJSON(w, http.StatusOK, listBody[apiKeyDTO]{Data: out})
}

func (h *Handler) revokeAPIKey(w http.ResponseWriter, r *http.Request) error {
	if err := decodeOptionalJSON(w, r, &struct{}{}, smallBodyLimit); err != nil {
		return err
	}
	key, err := h.svc.RevokeAPIKey(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, newAPIKeyDTO(key))
}
