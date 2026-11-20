package core

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Defaults and limits of the payment rules.
const (
	// DefaultPaymentTTL is how long a payment stays payable by default.
	DefaultPaymentTTL = 30 * time.Minute
	// MinPaymentTTL and MaxPaymentTTL bound custom TTLs.
	MinPaymentTTL = time.Minute
	MaxPaymentTTL = 30 * 24 * time.Hour
	// DefaultRequiredConfirmations is the default finality depth.
	DefaultRequiredConfirmations = 15
	// MaxRequiredConfirmations bounds the confirmation setting.
	MaxRequiredConfirmations = 10_000
	// DefaultDerivationPath is the BIP-44 path for EVM accounts; deposit
	// addresses are derived at <path>/<index>.
	DefaultDerivationPath = "m/44'/60'/0'/0"
	// MaxPaymentUnits caps a payment's amount, in whole units of the asset.
	MaxPaymentUnits = 1_000_000_000
	// DefaultExpiryGrace is added to a payment's deadline before it settles
	// as expired; it absorbs block-inclusion delay for last-second payers.
	DefaultExpiryGrace = time.Minute
	// DefaultWatchWindow is how long a deposit address stays on the watch
	// list after its payment last changed, so late funds are still seen.
	DefaultWatchWindow = 7 * 24 * time.Hour
	// DefaultUnfundedWatchWindow is how long the deposit address of a
	// payment that is not funded ([Payment.Funded]) stays on the watch
	// list after the payment closed. It is deliberately short: such
	// payments are free to create through a public link, and each one
	// costs the chain watcher a log filter entry. It still catches the
	// realistic late payer, who is minutes late.
	DefaultUnfundedWatchWindow = time.Hour
	// WatchRetirementMargin is how long a settled payment's address stays
	// on the watch list after the chain itself — the block time of the
	// processed head — has passed the end of its watch window. A watch
	// window is a promise about blocks: every transfer in a block stamped
	// before the window ends is seen. The chain watcher scans those blocks
	// some time after they were mined (up to its idle interval later, or
	// much later after an outage), so the address has to be in the list
	// it scans for until the processed head has passed them — and a few
	// minutes beyond, so that a reorganisation that replaces blocks below
	// the processed head is scanned for the address as well. See
	// [ChainState.Addresses].
	WatchRetirementMargin = 10 * time.Minute
	// DefaultMaxRetainedUnchecked is the default of
	// [Config.MaxRetainedUnchecked]: as many addresses as the payment links'
	// share of the watch list may have — ten more eth_getLogs calls per
	// tick and endpoint at most.
	DefaultMaxRetainedUnchecked = 5_000
	// WatchListWarning is the size of the chain watch list from which the
	// node status warns about it ([NodeStatus.Warnings]). Payments through
	// public links have a cap ([Config.MaxWatchedAddresses]); those the
	// merchant creates have none, and nothing leaves the list while the
	// processed head does not move.
	WatchListWarning = 20_000
	// logAddressesPerQuery is how many addresses the chain adapter puts in
	// one eth_getLogs query (evm's chunk size), for the warning's sake.
	logAddressesPerQuery = 500
	// DefaultMaxWatchedAddresses caps the public payment links' share of
	// the watch list. At the cap, payments through public links are
	// refused (see [ErrWatchListFull]); payments created by the merchant
	// are neither refused nor counted.
	//
	// The number is what the chain watcher pays for the list: every 500
	// watched addresses cost one eth_getLogs call per tick on each of two
	// RPC endpoints, so 5 000 addresses that anybody may create are ten log
	// queries per tick and endpoint — some four requests a second at the
	// default tick, which free public endpoints still serve. (It was
	// 20 000: forty queries per tick, fourteen requests a second, enough
	// to have a free endpoint throttle everything, the merchant's own
	// payments included.)
	DefaultMaxWatchedAddresses = 5_000
	// DefaultMaxHeadTimeAhead is how far ahead of the wall clock a chain
	// head's block timestamp may be before [Service.AdvanceHead] refuses
	// it with [ErrHeadTimeAhead].
	DefaultMaxHeadTimeAhead = 2 * time.Minute
	// DefaultLinkReservation is how long a customer has to pay a checkout
	// opened through a payment link with MaxUses; the checkout holds one
	// of the link's uses for that long.
	DefaultLinkReservation = 10 * time.Minute
	// DefaultMaxConcurrentKDF bounds how many password hashes are computed
	// at the same time.
	DefaultMaxConcurrentKDF = 4
	// ToleranceCapDivisor caps the underpayment tolerance of a payment at
	// Amount/ToleranceCapDivisor, i.e. 5 % of the amount due.
	ToleranceCapDivisor = 20
	// DustDivisor and DustCapUnits define a payment's dust limit: funds
	// below Amount/DustDivisor (5 % of the amount due), and in any case
	// below DustCapUnits whole units of the asset, are dust. Dust is
	// recorded on the payment and shown in its detail, and otherwise
	// ignored: it never changes a status, raises attention, emits an event
	// or keeps an address on the watch list (see [Evaluate]).
	DustDivisor  = 20
	DustCapUnits = 1
	// FundedDivisor and FundedFloorUnits define when a payment created
	// through a public link counts as funded ([Payment.Funded]): live funds
	// of at least Amount/FundedDivisor (half the amount due) and at least
	// FundedFloorUnits whole units of the asset.
	FundedDivisor    = 2
	FundedFloorUnits = 1
	// LinkMinAmountUnits is the least a customer may choose to pay through
	// a payment link without a fixed price, in whole units of the asset,
	// and the floor for a link's own minimum ([PaymentLink.MinAmount]). It
	// equals DustCapUnits on purpose: a checkout opened through such a link
	// is never for an amount whose every payment would be dust, and taking
	// one use of a limited link costs at least this much, paid to the
	// merchant.
	LinkMinAmountUnits = DustCapUnits
	// DefaultSessionTTL is the absolute lifetime of a dashboard session.
	DefaultSessionTTL = 14 * 24 * time.Hour
	// DefaultSessionIdleTimeout ends a dashboard session that has not been
	// used for this long, even before its absolute lifetime is over.
	DefaultSessionIdleTimeout = 24 * time.Hour
	// MinPasswordLength is the minimum operator password length.
	MinPasswordLength = 12
)

// Settings is the instance configuration. There is exactly one.
type Settings struct {
	// SetupCompleted is false until [Service.CompleteSetup] succeeded.
	SetupCompleted bool
	// PublicURL is the base URL the instance is reachable at, used to build
	// checkout and payment-link URLs ("https://pay.example.com").
	PublicURL string
	// Merchant is the identity shown on public pages.
	Merchant MerchantSettings
	// Payments holds the defaults applied to new payments.
	Payments PaymentSettings
	// Network selects the chain and how to reach it.
	Network NetworkSettings
	// Asset is the token the instance accepts; Asset.ChainID always equals
	// Network.ChainID.
	Asset Asset
	// Wallet configures deposit-address allocation.
	Wallet WalletSettings
	// UpdatedAt is the last modification time.
	UpdatedAt time.Time
}

// MerchantSettings is the merchant identity shown on checkout.
type MerchantSettings struct {
	// Name is the business name. Required.
	Name string
	// LogoURL is an http(s) URL, or an inline PNG, JPEG, WebP or GIF as a
	// base64 data: URL of at most 512 KB (never SVG); optional.
	LogoURL string
	// SupportEmail is where payers can ask for help. Required.
	SupportEmail string
	// SupportURL is an optional help page.
	SupportURL string
}

// PaymentSettings holds the defaults for new payments. Each payment
// snapshots them at creation; changing them never affects existing
// payments.
type PaymentSettings struct {
	// TTL is the default time a payment stays payable.
	TTL time.Duration
	// RequiredConfirmations is the finality depth.
	RequiredConfirmations int
	// UnderpaymentTolerance is a decimal string in whole units ("0.50"):
	// confirmed funds short of the amount by at most this much still count
	// as paid. "0" disables the tolerance.
	//
	// It exists to absorb a small absolute shortfall on a normal-sized
	// invoice (an exchange deducting its withdrawal fee). It is therefore
	// never applied in full to a small invoice: each payment's tolerance
	// is capped at 5 % of its amount ([ToleranceCapDivisor]), so a payment
	// always needs at least 95 % of its amount to count as paid and dust
	// can never settle anything. With "0.50", invoices of 10.00 and more
	// forgive 0.50; an invoice of 4.00 forgives 0.20.
	UnderpaymentTolerance string
	// DefaultRedirectURL is used for payments created without their own.
	DefaultRedirectURL string
}

// NetworkSettings selects the chain. One network per instance.
type NetworkSettings struct {
	// ChainID is the EVM chain id (56 mainnet, 97 testnet).
	ChainID uint64
	// RPCURLs are the JSON-RPC endpoints in order of preference.
	RPCURLs []string
	// Revision identifies the chain configuration — this, the asset and
	// the finality depth — as saved: the core increments it whenever any
	// of them changes (see revision.go), and ignores what is given here.
	Revision uint64
}

// WalletSettings configures deposit-address allocation. hmnpay is
// non-custodial and watch-only: it holds no private keys.
type WalletSettings struct {
	// XPub is the operator's extended public key.
	XPub string
	// DerivationPath is the path of XPub; addresses are derived at
	// <path>/<index>.
	DerivationPath string
	// NextAddressIndex is the index the next payment will use. Only the
	// core advances it, inside the transaction that creates the payment.
	NextAddressIndex uint32
}

// DefaultSettings returns the settings of a fresh instance: BSC mainnet
// USDT, 30-minute TTL, 15 confirmations, no underpayment tolerance.
func DefaultSettings() Settings {
	return Settings{
		Payments: PaymentSettings{
			TTL:                   DefaultPaymentTTL,
			RequiredConfirmations: DefaultRequiredConfirmations,
			UnderpaymentTolerance: "0",
		},
		Network: NetworkSettings{ChainID: ChainBSCMainnet},
		Asset:   USDTBSC,
		Wallet:  WalletSettings{DerivationPath: DefaultDerivationPath},
	}
}

func (s Settings) clone() Settings {
	s.Network.RPCURLs = slices.Clone(s.Network.RPCURLs)
	return s
}

// watches reports whether a is the asset the chain adapter watches: the
// network's chain and the configured token. Everything the core keeps per
// payment — its address on the watch list, its pending transfers, its
// judgement by the processed head — is in force for the watched asset only.
func (s Settings) watches(a Asset) bool {
	return a.ChainID == s.Network.ChainID && a.Contract == s.Asset.Contract
}

// ---- validation helpers shared by all use cases ----

// cleanText trims a single-line text field and validates it: valid UTF-8,
// at most maxLen characters, no control characters at all. Everything else
// — any script, emoji, punctuation — is ordinary text.
func cleanText(v *validation, field, s string, maxLen int, required bool) string {
	return cleanTextWith(v, field, s, maxLen, required, isControl)
}

// cleanMultiline is cleanText for free-form texts that may span lines
// (descriptions, notes): line feeds and tabs are allowed, every other
// control character is not. Carriage returns are dropped.
func cleanMultiline(v *validation, field, s string, maxLen int, required bool) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return cleanTextWith(v, field, s, maxLen, required, func(r rune) bool { return isControl(r) && r != '\n' && r != '\t' })
}

func cleanTextWith(v *validation, field, s string, maxLen int, required bool, bad func(rune) bool) string {
	s = strings.TrimSpace(s)
	switch {
	case s == "" && required:
		v.add(field, "required", "%s is required", fieldLabel(field))
	case utf8.RuneCountInString(s) > maxLen:
		v.add(field, "too_long", "%s must be at most %d characters", fieldLabel(field), maxLen)
	case !utf8.ValidString(s) || strings.ContainsFunc(s, bad):
		v.add(field, "invalid", "%s contains invalid characters", fieldLabel(field))
	}
	return s
}

// isControl reports the characters no text field accepts: the C0 controls
// (U+0000–U+001F), DEL (U+007F) and the C1 controls (U+0080–U+009F). They
// have no place in an order id or a name and are the raw material of log
// and terminal injection.
func isControl(r rune) bool { return r < 0x20 || (r >= 0x7f && r <= 0x9f) }

func fieldLabel(field string) string { return strings.ReplaceAll(field, "_", " ") }

// cleanEmail validates an email address loosely (one "@", a dotted domain,
// no spaces) and lowercases it.
func cleanEmail(v *validation, field, s string, required bool) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		if required {
			v.add(field, "required", "%s is required", fieldLabel(field))
		}
		return ""
	}
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || len(s) > 254 || strings.ContainsAny(s, " \t\r\n<>(),;:\\\"") ||
		!utf8.ValidString(s) || strings.ContainsFunc(s, isControl) ||
		strings.Contains(domain, "@") || !strings.Contains(domain, ".") ||
		strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		v.add(field, "invalid", "%s must be a valid email address", fieldLabel(field))
	}
	return s
}

// cleanURL validates an absolute URL with one of the given schemes.
func cleanURL(v *validation, field, s string, required bool, schemes ...string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		if required {
			v.add(field, "required", "%s is required", fieldLabel(field))
		}
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || !slices.Contains(schemes, strings.ToLower(u.Scheme)) || len(s) > 2048 {
		v.add(field, "invalid", "%s must be an absolute %s URL", fieldLabel(field), strings.Join(schemes, "/"))
	}
	return s
}

// cleanWebURL validates a URL that a browser will follow or show (redirect
// targets, support and logo links, the public URL): absolute http or https
// only — so no javascript:, data: or other scheme can be smuggled into a
// link — and without embedded credentials ("https://bank.com@evil.test").
func cleanWebURL(v *validation, field, s string, required bool) string {
	before := len(v.fields)
	s = cleanURL(v, field, s, required, "http", "https")
	if s != "" && len(v.fields) == before {
		if u, err := url.Parse(s); err == nil && u.User != nil {
			v.add(field, "invalid", "%s must not contain a user name or password", fieldLabel(field))
		}
	}
	return s
}

// maxLogoBytes bounds an inline logo (the length of the whole data: URL).
const maxLogoBytes = 512 * 1024

// logoDataPrefixes are the inline image types a logo may have. SVG is not
// among them on purpose: it is a document format that can carry script.
var logoDataPrefixes = []string{
	"data:image/png;base64,", "data:image/jpeg;base64,", "data:image/webp;base64,", "data:image/gif;base64,",
}

// cleanLogo validates a logo: an http(s) URL, or an inline PNG, JPEG, WebP
// or GIF as a base64 data: URL of at most 512 KB.
func cleanLogo(v *validation, field, s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(strings.ToLower(s), "data:") {
		return cleanWebURL(v, field, s, false)
	}
	if len(s) > maxLogoBytes {
		v.add(field, "too_long", "logo must be at most 512 KB")
		return s
	}
	for _, prefix := range logoDataPrefixes {
		if payload, ok := strings.CutPrefix(s, prefix); ok {
			if payload == "" || strings.ContainsFunc(payload, func(r rune) bool {
				return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=')
			}) {
				v.add(field, "invalid", "logo is not valid base64 image data")
			}
			return s
		}
	}
	v.add(field, "invalid", "an inline logo must be a PNG, JPEG, WebP or GIF image (data:image/png;base64,…)")
	return s
}

func (m MerchantSettings) normalize(v *validation) MerchantSettings {
	m.Name = cleanText(v, "merchant_name", m.Name, 80, true)
	m.SupportEmail = cleanEmail(v, "support_email", m.SupportEmail, true)
	m.SupportURL = cleanWebURL(v, "support_url", m.SupportURL, false)
	m.LogoURL = cleanLogo(v, "logo_url", m.LogoURL)
	return m
}

func (p PaymentSettings) normalize(v *validation, asset Asset) PaymentSettings {
	if p.TTL < MinPaymentTTL || p.TTL > MaxPaymentTTL {
		v.add("ttl", "invalid", "ttl must be between %s and %s", MinPaymentTTL, MaxPaymentTTL)
	}
	if p.RequiredConfirmations < 1 || p.RequiredConfirmations > MaxRequiredConfirmations {
		v.add("required_confirmations", "invalid", "required confirmations must be between 1 and %d", MaxRequiredConfirmations)
	}
	p.UnderpaymentTolerance = strings.TrimSpace(p.UnderpaymentTolerance)
	if p.UnderpaymentTolerance == "" {
		p.UnderpaymentTolerance = "0"
	}
	tol, err := ParseAmount(p.UnderpaymentTolerance, asset.Decimals)
	switch {
	case err != nil:
		v.add("underpayment_tolerance", "invalid", "underpayment tolerance: %s", errMessage(err))
	case tol.Cmp(wholeUnits(MaxPaymentUnits, asset.Decimals)) > 0:
		v.add("underpayment_tolerance", "too_large", "underpayment tolerance is too large")
	}
	p.DefaultRedirectURL = cleanWebURL(v, "default_redirect_url", p.DefaultRedirectURL, false)
	return p
}

func (n NetworkSettings) normalize(v *validation) NetworkSettings {
	if n.ChainID == 0 {
		v.add("chain_id", "required", "chain id is required")
	}
	urls := make([]string, 0, len(n.RPCURLs))
	for i, u := range n.RPCURLs {
		u = cleanURL(v, "rpc_urls."+strconv.Itoa(i), u, true, "http", "https", "ws", "wss")
		if slices.Contains(urls, u) {
			continue
		}
		// Two URLs that differ only in their path or query (a key) reach
		// one endpoint: it would confirm its own answers, as two.
		if j := slices.IndexFunc(urls, func(o string) bool { return endpointIdentity(o) == endpointIdentity(u) }); j >= 0 {
			v.add("rpc_urls."+strconv.Itoa(i), "same_endpoint", "this URL reaches the same endpoint (%s) as another one in the list: one endpoint cannot check its own answers", endpointIdentity(u))
			continue
		}
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		v.add("rpc_urls", "required", "at least one RPC endpoint is required")
	}
	if len(urls) > 10 {
		v.add("rpc_urls", "too_many", "at most 10 RPC endpoints")
	}
	n.RPCURLs = urls
	return n
}

// Problems returns what is wrong with network settings by the rules
// [Service.UpdateNetwork] applies to new ones — empty when they would be
// accepted. Settings stored by an earlier version can fail rules added
// since (two URLs of one endpoint, say). An instance with such settings
// starts all the same, so that the operator can see what is wrong and
// repair it, but runs no chain watcher on them and creates no payment
// ([ErrNetworkInvalid]); [NodeStatus.NetworkProblem] and the chain adapter's
// idle reason say what is wrong.
func (n NetworkSettings) Problems() []FieldError {
	var v validation
	n.normalize(&v)
	return v.fields
}

// problem returns Problems in words ("" if none).
func (n NetworkSettings) problem() string {
	var parts []string
	for _, f := range n.Problems() {
		parts = append(parts, f.Field+": "+f.Message)
	}
	return strings.Join(parts, "; ")
}

// errMessage returns the caller-safe message of an error.
func errMessage(err error) string {
	if e, ok := err.(*Error); ok && e.Message != "" {
		return e.Message
	}
	return err.Error()
}
