package httpapi

import (
	"encoding/json"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// Wire shapes for core types that carry no JSON tags. PaymentView,
// CheckoutView, TransferView, LinkView, LinkCheckoutView and Overview are
// already wire shapes and are encoded as they are.

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optInt(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// ---- events ----

type eventDTO struct {
	Seq       int64           `json:"seq"`
	ID        string          `json:"id"`
	Type      core.EventType  `json:"type"`
	PaymentID *string         `json:"payment_id"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

func newEventDTO(e core.Event) eventDTO {
	env := e.Envelope()
	return eventDTO{Seq: e.Seq, ID: e.ID, Type: e.Type, PaymentID: optString(e.PaymentID), CreatedAt: e.CreatedAt, Data: env.Data}
}

func newEventDTOs(es []core.Event) []eventDTO {
	out := make([]eventDTO, 0, len(es))
	for _, e := range es {
		out = append(out, newEventDTO(e))
	}
	return out
}

// ---- webhooks ----

type attemptDTO struct {
	N            int       `json:"n"`
	At           time.Time `json:"at"`
	ResponseCode *int      `json:"response_code"`
	DurationMs   int       `json:"duration_ms"`
	Error        string    `json:"error"`
	Succeeded    bool      `json:"succeeded"`
	// Late marks an attempt whose result arrived after its claim on the
	// delivery was lost: it really happened, but it is not counted in
	// "attempt" and did not move the retry schedule.
	Late bool `json:"late"`
}

type deliveryDTO struct {
	ID            string              `json:"id"`
	EndpointID    string              `json:"endpoint_id"`
	EventID       string              `json:"event_id"`
	EventType     core.EventType      `json:"event_type"`
	PaymentID     *string             `json:"payment_id"`
	URL           string              `json:"url"`
	Status        core.DeliveryStatus `json:"status"`
	Attempt       int                 `json:"attempt"`
	MaxAttempts   int                 `json:"max_attempts"`
	NextAttemptAt *time.Time          `json:"next_attempt_at"`
	RequestBody   string              `json:"request_body"`
	ResponseCode  *int                `json:"response_code"`
	ResponseBody  string              `json:"response_body"`
	DurationMs    int                 `json:"duration_ms"`
	Error         string              `json:"error"`
	Attempts      []attemptDTO        `json:"attempts"`
	CreatedAt     time.Time           `json:"created_at"`
	UpdatedAt     time.Time           `json:"updated_at"`
	CompletedAt   *time.Time          `json:"completed_at"`
}

func newDeliveryDTO(d core.WebhookDelivery) deliveryDTO {
	out := deliveryDTO{
		ID: d.ID, EndpointID: d.EndpointID, EventID: d.EventID, EventType: d.EventType,
		PaymentID: optString(d.PaymentID), URL: d.URL, Status: d.Status,
		Attempt: d.Attempt, MaxAttempts: d.MaxAttempts, NextAttemptAt: optTime(d.NextAttemptAt),
		RequestBody: d.RequestBody, ResponseCode: optInt(d.ResponseCode), ResponseBody: d.ResponseBody,
		DurationMs: d.DurationMs, Error: d.Error, Attempts: make([]attemptDTO, 0, len(d.Attempts)),
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, CompletedAt: optTime(d.CompletedAt),
	}
	for _, a := range d.Attempts {
		out.Attempts = append(out.Attempts, attemptDTO{N: a.N, At: a.At, ResponseCode: optInt(a.ResponseCode), DurationMs: a.DurationMs, Error: a.Error, Succeeded: a.Succeeded, Late: a.Late})
	}
	return out
}

func newDeliveryDTOs(ds []core.WebhookDelivery) []deliveryDTO {
	out := make([]deliveryDTO, 0, len(ds))
	for _, d := range ds {
		out = append(out, newDeliveryDTO(d))
	}
	return out
}

// endpointDTO is a webhook endpoint. Secret is masked ("whsec_…ab12")
// except in the responses of create, rotate and reveal.
type endpointDTO struct {
	ID                      string           `json:"id"`
	URL                     string           `json:"url"`
	Description             string           `json:"description"`
	Events                  []core.EventType `json:"events"`
	Enabled                 bool             `json:"enabled"`
	Secret                  string           `json:"secret"`
	SecretMasked            bool             `json:"secret_masked"`
	PreviousSecretExpiresAt *time.Time       `json:"previous_secret_expires_at"`
	DisabledAt              *time.Time       `json:"disabled_at"`
	DisabledReason          string           `json:"disabled_reason"`
	FailingSince            *time.Time       `json:"failing_since"`
	ConsecutiveFailures     int              `json:"consecutive_failures"`
	DisableAt               *time.Time       `json:"disable_at"`
	LastDeliveryAt          *time.Time       `json:"last_delivery_at"`
	LastSuccessAt           *time.Time       `json:"last_success_at"`
	Succeeded7d             *int             `json:"succeeded_7d,omitempty"`
	Failed7d                *int             `json:"failed_7d,omitempty"`
	SuccessRate             *float64         `json:"success_rate,omitempty"`
	CreatedAt               time.Time        `json:"created_at"`
	UpdatedAt               time.Time        `json:"updated_at"`
}

func maskSecret(s string) string {
	if len(s) <= 10 {
		return "whsec_…"
	}
	return "whsec_…" + s[len(s)-4:]
}

func newEndpointDTO(e core.WebhookEndpoint, reveal bool) endpointDTO {
	out := endpointDTO{
		ID: e.ID, URL: e.URL, Description: e.Description, Events: nonNil(e.Events), Enabled: e.Enabled,
		Secret: maskSecret(e.Secret), SecretMasked: true,
		DisabledAt: optTime(e.DisabledAt), DisabledReason: e.DisabledReason,
		FailingSince: optTime(e.FailingSince), ConsecutiveFailures: e.ConsecutiveFailures,
		DisableAt: optTime(e.DisableAt()), LastDeliveryAt: optTime(e.LastDeliveryAt), LastSuccessAt: optTime(e.LastSuccessAt),
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
	if e.PreviousSecret != "" {
		out.PreviousSecretExpiresAt = optTime(e.PreviousSecretExpiresAt)
	}
	if reveal {
		out.Secret, out.SecretMasked = e.Secret, false
	}
	return out
}

func newEndpointSummaryDTO(s core.WebhookEndpointSummary) endpointDTO {
	out := newEndpointDTO(s.WebhookEndpoint, false)
	ok, failed := s.Succeeded7d, s.Failed7d
	out.Succeeded7d, out.Failed7d = &ok, &failed
	if ok+failed > 0 {
		rate := float64(ok) / float64(ok+failed)
		out.SuccessRate = &rate
	}
	return out
}

// secretDTO is the response of the explicit reveal call.
type secretDTO struct {
	Secret                  string     `json:"secret"`
	PreviousSecret          *string    `json:"previous_secret"`
	PreviousSecretExpiresAt *time.Time `json:"previous_secret_expires_at"`
}

// ---- API keys ----

type apiKeyDTO struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Mode       string       `json:"mode"`
	Prefix     string       `json:"prefix"`
	Last4      string       `json:"last4"`
	Scopes     []core.Scope `json:"scopes"`
	CreatedAt  time.Time    `json:"created_at"`
	LastUsedAt *time.Time   `json:"last_used_at"`
	ExpiresAt  *time.Time   `json:"expires_at"`
	RevokedAt  *time.Time   `json:"revoked_at"`
	// Secret is present only in the response that created the key.
	Secret string `json:"secret,omitempty"`
}

func newAPIKeyDTO(k core.APIKey) apiKeyDTO {
	return apiKeyDTO{
		ID: k.ID, Name: k.Name, Mode: k.Mode, Prefix: k.Prefix, Last4: k.Last4, Scopes: nonNil(k.Scopes),
		CreatedAt: k.CreatedAt, LastUsedAt: optTime(k.LastUsedAt), ExpiresAt: optTime(k.ExpiresAt), RevokedAt: optTime(k.RevokedAt),
	}
}

// ---- settings ----

type assetDTO struct {
	Symbol   string `json:"symbol"`
	Network  string `json:"network"`
	ChainID  uint64 `json:"chain_id"`
	Contract string `json:"contract"`
	Decimals int    `json:"decimals"`
}

func newAssetDTO(a core.Asset) assetDTO {
	return assetDTO{Symbol: a.Symbol, Network: a.Network, ChainID: a.ChainID, Contract: a.Contract, Decimals: a.Decimals}
}

func (a *assetDTO) core() *core.Asset {
	if a == nil {
		return nil
	}
	return &core.Asset{Symbol: a.Symbol, Network: a.Network, ChainID: a.ChainID, Contract: a.Contract, Decimals: a.Decimals}
}

type merchantDTO struct {
	Name         string `json:"name"`
	LogoURL      string `json:"logo_url"`
	SupportEmail string `json:"support_email"`
	SupportURL   string `json:"support_url"`
}

type paymentSettingsDTO struct {
	TTLSeconds            int64  `json:"ttl_seconds"`
	RequiredConfirmations int    `json:"required_confirmations"`
	UnderpaymentTolerance string `json:"underpayment_tolerance"`
	DefaultRedirectURL    string `json:"default_redirect_url"`
}

type networkDTO struct {
	ChainID uint64   `json:"chain_id"`
	Testnet bool     `json:"testnet"`
	RPCURLs []string `json:"rpc_urls"`
	// Warnings are what the operator should know although the settings are
	// valid: endpoints that look like one provider (core.NetworkSettings.
	// Warnings). Never null.
	Warnings []string `json:"warnings"`
	// Problems are what makes stored settings unusable by the rules new
	// ones are held to (core.NetworkSettings.Problems: settings an earlier
	// version accepted), each with the field it concerns — the same as a
	// validation error of saving them. Empty when they are valid. Never
	// null.
	Problems []fieldError `json:"problems"`
}

// networkProblems returns NetworkSettings.Problems of set-up settings for
// the API.
func networkProblems(s core.Settings) []fieldError {
	out := []fieldError{}
	if !s.SetupCompleted {
		return out
	}
	for _, f := range s.Network.Problems() {
		out = append(out, fieldError{Field: f.Field, Code: f.Code, Message: f.Message})
	}
	return out
}

type walletDTO struct {
	XPub             string `json:"xpub"`
	DerivationPath   string `json:"derivation_path"`
	NextAddressIndex uint32 `json:"next_address_index"`
}

type settingsDTO struct {
	SetupCompleted bool               `json:"setup_completed"`
	PublicURL      string             `json:"public_url"`
	Merchant       merchantDTO        `json:"merchant"`
	Payments       paymentSettingsDTO `json:"payments"`
	Network        networkDTO         `json:"network"`
	Asset          assetDTO           `json:"asset"`
	Wallet         walletDTO          `json:"wallet"`
	UpdatedAt      *time.Time         `json:"updated_at"`
}

func newSettingsDTO(s core.Settings) settingsDTO {
	return settingsDTO{
		SetupCompleted: s.SetupCompleted,
		PublicURL:      s.PublicURL,
		Merchant:       merchantDTO{Name: s.Merchant.Name, LogoURL: s.Merchant.LogoURL, SupportEmail: s.Merchant.SupportEmail, SupportURL: s.Merchant.SupportURL},
		Payments: paymentSettingsDTO{
			TTLSeconds:            int64(s.Payments.TTL / time.Second),
			RequiredConfirmations: s.Payments.RequiredConfirmations,
			UnderpaymentTolerance: s.Payments.UnderpaymentTolerance,
			DefaultRedirectURL:    s.Payments.DefaultRedirectURL,
		},
		Network:   networkDTO{ChainID: s.Network.ChainID, Testnet: core.IsTestnet(s.Network.ChainID), RPCURLs: nonNil(s.Network.RPCURLs), Warnings: nonNil(s.Network.Warnings()), Problems: networkProblems(s)},
		Asset:     newAssetDTO(s.Asset),
		Wallet:    walletDTO{XPub: s.Wallet.XPub, DerivationPath: s.Wallet.DerivationPath, NextAddressIndex: s.Wallet.NextAddressIndex},
		UpdatedAt: optTime(s.UpdatedAt),
	}
}

// ---- account ----

type operatorDTO struct {
	ID        string     `json:"id"`
	Email     string     `json:"email"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

type sessionDTO struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// sessionInfoDTO is one entry of the operator's session list. The token
// (or its hash) is never included.
type sessionInfoDTO struct {
	ID        string    `json:"id"`
	UserAgent string    `json:"user_agent"`
	IP        string    `json:"ip"`
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt is the absolute end of the session; LastSeenAt is its last
	// use (to a few minutes), null if it was never used after login. A
	// session also ends when it is not used for the idle timeout.
	ExpiresAt  time.Time  `json:"expires_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	// Current marks the session this request was authenticated with.
	Current bool `json:"current"`
}

type loginResponse struct {
	Operator operatorDTO `json:"operator"`
	Session  sessionDTO  `json:"session"`
	// Token is returned only when the client asked for a bearer token
	// ("issue_token": true); browser logins get the cookie instead.
	Token string `json:"token,omitempty"`
}

func newLoginResponse(res core.LoginResult) loginResponse {
	return loginResponse{
		Operator: operatorDTO{ID: res.Operator.ID, Email: res.Operator.Email},
		Session:  sessionDTO{ID: res.Session.ID, ExpiresAt: res.Session.ExpiresAt},
	}
}

// ---- chain ----

type headDTO struct {
	ChainID uint64     `json:"chain_id"`
	Number  uint64     `json:"number"`
	Hash    string     `json:"hash"`
	Time    *time.Time `json:"time"`
	// Checked is null while every block up to the head has been scanned by
	// two log endpoints; otherwise a time no later than the oldest block
	// still to be scanned by a second one (core.ChainHead.Checked).
	Checked *time.Time `json:"checked"`
	// Final is the newest block that is final (core.ChainHead.Final): the
	// chain has finalised it, as two endpoints attest. It never goes back,
	// and can be above number for a while (the processed head went back).
	// A transfer counts towards paid only at or below it.
	Final      uint64     `json:"final"`
	ObservedAt *time.Time `json:"observed_at"`
}

// finalityWaitDTO is core.FinalityWait: payments that have their
// confirmations and wait for the chain to finalise block Block (the lowest
// block they wait for); Final is the newest final block.
type finalityWaitDTO struct {
	Payments int    `json:"payments"`
	Block    uint64 `json:"block"`
	Final    uint64 `json:"final"`
}

type endpointStatusDTO struct {
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
	InUse   bool   `json:"in_use"`
	// State is the verdict in one word: "healthy", "not_checked" (not
	// asked anything yet — the watcher is starting; not a fault),
	// "unverified", "failing", "lagging", "wrong_chain", "unsupported"
	// or "idle".
	State       string `json:"state"`
	LatencyMs   int    `json:"latency_ms"`
	BlockNumber uint64 `json:"block_number"`
	Error       string `json:"error"`
}

type nodeStatusDTO struct {
	Healthy bool `json:"healthy"`
	// Starting is true while the chain watcher has (re)started and its
	// first pass over the chain has not completed: healthy is false, but
	// nothing is known to be wrong. Normally a second or two.
	Starting bool `json:"starting"`
	// Idle is true while nothing is expected from the chain (no payment is
	// open, no transfer is waiting for confirmations) and the watcher
	// looks at the chain at its idle interval instead of every few
	// seconds. head and lag_blocks then show that pause and do not make
	// the node unhealthy; creating a payment wakes the watcher at once.
	//
	// Quiet says which kind of idle: true if the watch list is not empty —
	// watched_addresses addresses of closed payments, scanned for late
	// funds on every idle pass — and false if there is nothing to scan
	// for. Never true while idle is false.
	Idle          bool                `json:"idle"`
	Quiet         bool                `json:"quiet"`
	Head          *headDTO            `json:"head"`
	LatestBlock   uint64              `json:"latest_block"`
	LagBlocks     uint64              `json:"lag_blocks"`
	RPCLatencyMs  int                 `json:"rpc_latency_ms"`
	LastCheckedAt *time.Time          `json:"last_checked_at"`
	Endpoints     []endpointStatusDTO `json:"endpoints"`
	// Primary is the endpoint the chain is read from, Verifier the one
	// that cross-checks it, LogVerifier the one whose log scan confirmed
	// the most recently settled blocks ("" for none of each). Solo: one
	// endpoint is believed alone (best-effort, every other unreachable).
	// LogsUnchecked: recent blocks were settled on one endpoint's logs.
	// Hold says why the processed head is waiting; Work what the watcher
	// is busy with when it is not at rest (a re-scan, transfers or blocks
	// waiting for a second endpoint, and what that waits for); Warnings
	// lists what is wrong right now. Endpoints appear as scheme and host
	// only.
	Primary       string   `json:"primary"`
	Verifier      string   `json:"verifier"`
	LogVerifier   string   `json:"log_verifier"`
	Solo          bool     `json:"solo"`
	LogsUnchecked bool     `json:"logs_unchecked"`
	Hold          string   `json:"hold"`
	Work          string   `json:"work"`
	Warnings      []string `json:"warnings"`
	// WatchedAddresses is the size of the chain watch list;
	// WatchedLinkAddresses the part that belongs to payments created
	// through public links and MaxWatchedAddresses the cap on that part
	// (0 = none). WatchListFull means public payment links refuse new
	// payments until addresses age out.
	WatchedAddresses     int  `json:"watched_addresses"`
	WatchedLinkAddresses int  `json:"watched_link_addresses"`
	MaxWatchedAddresses  int  `json:"max_watched_addresses"`
	WatchListFull        bool `json:"watch_list_full"`
	// SameProvider lists the configured endpoints that look like one
	// provider (hosts with a shared registrable domain), in groups of
	// scheme and host. Never null.
	SameProvider [][]string `json:"same_provider"`
	// NetworkProblem, when not empty, says why the stored network settings
	// cannot be used: no chain watcher runs and no payment is created until
	// they are repaired (core.NodeStatus.NetworkProblem).
	NetworkProblem string `json:"network_problem"`
	// RetainedUnchecked counts the settled payments' addresses watched past
	// their window until blocks of it settled on one endpoint's logs are
	// scanned by a second; ReleasedUnchecked those let go before that scan
	// because the number kept had reached its limit — a payment hidden in
	// those blocks for one of them is not found.
	RetainedUnchecked int `json:"retained_unchecked"`
	ReleasedUnchecked int `json:"released_unchecked"`
	// AwaitingFinality is set while payments have their confirmations but
	// wait for the chain to finalise a block (also a sentence in
	// warnings); null otherwise.
	AwaitingFinality *finalityWaitDTO `json:"awaiting_finality"`
}

func newNodeStatusDTO(ns core.NodeStatus) nodeStatusDTO {
	out := nodeStatusDTO{
		Healthy: ns.Healthy, Starting: ns.Starting, Idle: ns.Idle, Quiet: ns.Quiet, LatestBlock: ns.LatestBlock, LagBlocks: ns.LagBlocks,
		Primary: ns.Primary, Verifier: ns.Verifier, LogVerifier: ns.LogVerifier,
		Solo: ns.Solo, LogsUnchecked: ns.LogsUnchecked, Hold: ns.Hold, Work: ns.Work, Warnings: nonNil(ns.Warnings),
		RPCLatencyMs: ns.RPCLatencyMs, LastCheckedAt: optTime(ns.LastCheckedAt),
		Endpoints:        make([]endpointStatusDTO, 0, len(ns.Endpoints)),
		WatchedAddresses: ns.WatchedAddresses, WatchedLinkAddresses: ns.WatchedLinkAddresses,
		MaxWatchedAddresses: ns.MaxWatchedAddresses, WatchListFull: ns.WatchListFull,
		SameProvider:      nonNil(ns.SameProvider),
		RetainedUnchecked: ns.RetainedUnchecked, ReleasedUnchecked: ns.ReleasedUnchecked,
		NetworkProblem: ns.NetworkProblem,
	}
	if ns.Head.Number != 0 {
		out.Head = &headDTO{ChainID: ns.Head.ChainID, Number: ns.Head.Number, Hash: ns.Head.Hash, Time: optTime(ns.Head.Time), Checked: optTime(ns.Head.Checked), Final: ns.Head.Final, ObservedAt: optTime(ns.Head.ObservedAt)}
	}
	if w := ns.AwaitingFinality; w != nil {
		out.AwaitingFinality = &finalityWaitDTO{Payments: w.Payments, Block: w.Block, Final: w.Final}
	}
	for _, e := range ns.Endpoints {
		out.Endpoints = append(out.Endpoints, endpointStatusDTO{URL: e.URL, Healthy: e.Healthy, InUse: e.InUse, State: e.State, LatencyMs: e.LatencyMs, BlockNumber: e.BlockNumber, Error: e.Error})
	}
	return out
}

// ---- setup ----

type setupStatusDTO struct {
	Completed bool `json:"completed"`
	// SetupTokenRequired tells the wizard to ask for the setup token (shown
	// to whoever installed the server) and to send it with every setup
	// call, in the X-Hmnpay-Setup-Token header or the "setup_token" field.
	SetupTokenRequired bool             `json:"setup_token_required"`
	MinPasswordLength  int              `json:"min_password_length"`
	Defaults           setupDefaultsDTO `json:"defaults"`
	SupportedChains    []chainDTO       `json:"supported_chains"`
}

type setupDefaultsDTO struct {
	ChainID        uint64             `json:"chain_id"`
	Asset          assetDTO           `json:"asset"`
	DerivationPath string             `json:"derivation_path"`
	Payments       paymentSettingsDTO `json:"payments"`
}

type chainDTO struct {
	ChainID uint64   `json:"chain_id"`
	Testnet bool     `json:"testnet"`
	Asset   assetDTO `json:"asset"`
}

type probeResultDTO struct {
	Outcome  core.ProbeOutcome `json:"outcome"`
	Expected assetDTO          `json:"expected"`
	Facts    probeFactsDTO     `json:"facts"`
}

type probeFactsDTO struct {
	Reachable     bool   `json:"reachable"`
	ChainID       uint64 `json:"chain_id"`
	BlockNumber   uint64 `json:"block_number"`
	LatencyMs     int    `json:"latency_ms"`
	TokenFound    bool   `json:"token_found"`
	TokenDecimals int    `json:"token_decimals"`
	TokenSymbol   string `json:"token_symbol"`
	Error         string `json:"error"`
}

type derivedAddressDTO struct {
	Index   uint32 `json:"index"`
	Path    string `json:"path"`
	Address string `json:"address"`
}

// ---- payments ----

type countsDTO struct {
	All       int `json:"all"`
	Open      int `json:"open"`
	Paid      int `json:"paid"`
	Attention int `json:"attention"`
	Expired   int `json:"expired"`
}

type paymentListBody struct {
	Data   []core.PaymentView `json:"data"`
	Page   *pageDTO           `json:"page"`
	Counts *countsDTO         `json:"counts,omitempty"`
}

type paymentDetailDTO struct {
	Payment    core.PaymentView    `json:"payment"`
	Transfers  []core.TransferView `json:"transfers"`
	Timeline   []eventDTO          `json:"timeline"`
	Deliveries []deliveryDTO       `json:"deliveries"`
}

// checkoutDTO is the public view of a payment as the API sends it: the
// core's view and its version.
//
// Version is the number of events the payment has had. It only grows, and
// every change of the payment's state — status, amounts, transaction,
// cancellation — raises it, so of two views of one payment the one with
// the higher version is the newer. A checkout page gets views from two
// places (this endpoint and the event stream, whose event id is the same
// number) and their answers can arrive in either order; with the version
// it never puts an older view over a newer one. Two views with the same
// version differ at most in the confirmation count, which changes with
// the chain's head and without an event (and in the merchant's name and
// logo, if the operator edits them at that moment).
type checkoutDTO struct {
	core.CheckoutView
	Version int64 `json:"version"`
}
