package core

import (
	"context"
	"errors"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---- first-run setup ----

// SetupStatus tells a client whether the setup wizard has to run, and what
// to prefill it with.
type SetupStatus struct {
	// Completed is true once [Service.CompleteSetup] succeeded.
	Completed bool
	// Defaults are the settings a fresh instance starts from (network,
	// asset, derivation path, payment defaults).
	Defaults Settings
	// MinPasswordLength is the minimum operator password length.
	MinPasswordLength int
	// TokenRequired is true while setup is not completed and the instance
	// was started with a setup token ([Config.SetupToken]): the wizard must
	// ask for it and send it with every setup call. The token itself is
	// never reported.
	TokenRequired bool
}

// SetupStatus reports whether first-run setup is done. Public.
func (s *Service) SetupStatus(ctx context.Context) (SetupStatus, error) {
	status := SetupStatus{Defaults: DefaultSettings(), MinPasswordLength: MinPasswordLength}
	err := s.store.View(ctx, func(r Reader) error {
		st, err := loadSettings(ctx, r)
		status.Completed = st.SetupCompleted
		return err
	})
	status.TokenRequired = !status.Completed && s.setupToken != ""
	return status, err
}

// checkSetupToken guards first-run setup. While the instance is not set up
// and a setup token is configured, a caller must present it; trusted
// in-process code (a system actor) is exempt. The comparison is
// constant-time, and a missing and a wrong token are told apart only by
// their error code.
func (s *Service) checkSetupToken(ctx context.Context, token string) error {
	if s.setupToken == "" {
		return nil
	}
	if a, ok := ActorFrom(ctx); ok && a.Kind == ActorSystem {
		return nil
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return newError(ErrSetupTokenRequired, "")
	}
	if !secretsEqual(token, s.setupToken) {
		return newError(ErrSetupTokenInvalid, "")
	}
	return nil
}

// setupGate authorises the wizard's helper calls: until setup is completed
// they need the setup token (if one is configured); afterwards only the
// operator may use them.
func (s *Service) setupGate(ctx context.Context, token string) error {
	var completed bool
	err := s.store.View(ctx, func(r Reader) error {
		st, err := loadSettings(ctx, r)
		completed = st.SetupCompleted
		return err
	})
	if err != nil {
		return err
	}
	if !completed {
		return s.checkSetupToken(ctx, token)
	}
	_, err = authorize(ctx, permOperator)
	return err
}

// ProbeOutcome is the core's verdict on an RPC endpoint.
type ProbeOutcome string

// Probe outcomes, in the order the checks run.
const (
	// ProbeOK: reachable, right chain, token contract as expected.
	ProbeOK ProbeOutcome = "ok"
	// ProbeUnreachable: the endpoint did not answer.
	ProbeUnreachable ProbeOutcome = "unreachable"
	// ProbeWrongChain: it answered with another chain id.
	ProbeWrongChain ProbeOutcome = "wrong_chain"
	// ProbeTokenMismatch: the token contract is missing or reports other
	// decimals than the configured asset.
	ProbeTokenMismatch ProbeOutcome = "token_mismatch"
)

// ProbeRPCInput selects what to probe.
type ProbeRPCInput struct {
	// URL is the JSON-RPC endpoint (http, https, ws or wss).
	URL string
	// ChainID is the chain the endpoint is expected to serve; 0 means the
	// instance's configured chain.
	ChainID uint64
	// Asset overrides the token to check; nil means the built-in asset of
	// ChainID (or the configured asset when the chain is the configured
	// one).
	Asset *Asset
	// SetupToken is the instance's setup token; needed only before setup
	// is completed, and only if one is configured ([Config.SetupToken]).
	SetupToken string
}

// ProbeResult is the outcome of [Service.ProbeRPC].
type ProbeResult struct {
	Outcome ProbeOutcome
	// Expected is the asset the endpoint was checked against.
	Expected Asset
	// Facts is what the prober observed.
	Facts ProbeFacts
}

// ProbeRPC tests an RPC endpoint for the setup wizard's network step and
// the network settings page: does it answer, is it the expected chain,
// what is its latest block, and does the token contract look right.
//
// Until setup is completed it is open to callers presenting the setup
// token (in.SetupToken; [ErrSetupTokenRequired] / [ErrSetupTokenInvalid]
// otherwise) — to anyone if no token is configured; afterwards it is
// operator-only. It makes this server send a request to a URL of the
// caller's choice, which is why it is not simply public. Returns
// [ErrUnavailable] when no [ChainProber] is wired in.
func (s *Service) ProbeRPC(ctx context.Context, in ProbeRPCInput) (ProbeResult, error) {
	var res ProbeResult
	if err := s.setupGate(ctx, in.SetupToken); err != nil {
		return res, err
	}
	if s.prober == nil {
		return res, newError(ErrUnavailable, "no chain prober is configured")
	}
	var v validation
	url := cleanURL(&v, "url", in.URL, true, "http", "https", "ws", "wss")
	var current Settings
	if err := s.store.View(ctx, func(r Reader) error {
		var err error
		current, err = loadSettings(ctx, r)
		return err
	}); err != nil {
		return res, err
	}
	asset, err := resolveAsset(&v, in.ChainID, in.Asset, current)
	if err != nil {
		return res, err
	}
	if err := v.errOrNil(); err != nil {
		return res, err
	}
	facts, err := s.prober.Probe(ctx, url, asset)
	if err != nil {
		return res, internal("probe rpc endpoint", err)
	}
	res.Expected, res.Facts = asset, facts
	switch {
	case !facts.Reachable:
		res.Outcome = ProbeUnreachable
	case facts.ChainID != asset.ChainID:
		res.Outcome = ProbeWrongChain
	case !facts.TokenFound || facts.TokenDecimals != asset.Decimals:
		res.Outcome = ProbeTokenMismatch
	default:
		res.Outcome = ProbeOK
	}
	return res, nil
}

// resolveAsset picks the asset for a chain: an explicit one, the
// configured one if the chain is unchanged, or the built-in preset.
func resolveAsset(v *validation, chainID uint64, explicit *Asset, current Settings) (Asset, error) {
	if chainID == 0 {
		chainID = current.Network.ChainID
	}
	if explicit != nil {
		a := explicit.normalize("asset", v)
		if a.ChainID != chainID {
			v.add("asset.chain_id", "invalid", "asset chain id must equal the network chain id")
		}
		return a, nil
	}
	if chainID == current.Asset.ChainID {
		return current.Asset, nil
	}
	if a, ok := DefaultAsset(chainID); ok {
		return a, nil
	}
	v.add("chain_id", "unsupported", "no built-in asset for chain %d; supply the asset explicitly", chainID)
	return Asset{}, nil
}

// PreviewAddressesInput selects the addresses to preview.
type PreviewAddressesInput struct {
	// XPub is the extended public key the operator pasted.
	XPub string
	// DerivationPath is its path; empty means [DefaultDerivationPath].
	DerivationPath string
	// Count is how many addresses to derive, starting at index 0; 0 means
	// 3, the maximum is 20.
	Count int
	// SetupToken is the instance's setup token; see
	// [ProbeRPCInput.SetupToken].
	SetupToken string
}

// DerivedAddress is one address of a wallet preview.
type DerivedAddress struct {
	// Index is the address index.
	Index uint32
	// Path is the full derivation path, "<path>/<index>".
	Path string
	// Address is the derived address exactly as the address source
	// returned it (typically EIP-55 checksummed, for comparison with the
	// operator's wallet).
	Address string
}

// PreviewAddresses derives the first addresses of an xpub so the operator
// can compare them with their wallet before trusting hmnpay with it — the
// setup wizard's wallet step. Nothing is stored. An unusable key is a
// validation error on "xpub". Access is as for [Service.ProbeRPC]: the
// setup token before setup is completed, the operator afterwards.
func (s *Service) PreviewAddresses(ctx context.Context, in PreviewAddressesInput) ([]DerivedAddress, error) {
	if err := s.setupGate(ctx, in.SetupToken); err != nil {
		return nil, err
	}
	var v validation
	w := normalizeWallet(&v, in.XPub, in.DerivationPath)
	count := clampLimit(in.Count, 3, 20)
	if err := v.errOrNil(); err != nil {
		return nil, err
	}
	out := make([]DerivedAddress, 0, count)
	for i := uint32(0); i < uint32(count); i++ {
		addr, err := s.deriveChecked(ctx, w, i)
		if err != nil {
			return nil, err
		}
		out = append(out, DerivedAddress{Index: i, Path: w.DerivationPath + "/" + strconv.FormatUint(uint64(i), 10), Address: addr})
	}
	return out, nil
}

// normalizeWallet cleans the operator's wallet input (shape only; the
// address source verifies the key itself).
func normalizeWallet(v *validation, xpub, path string) WalletSettings {
	w := WalletSettings{
		XPub:           strings.Join(strings.Fields(xpub), ""),
		DerivationPath: strings.TrimSpace(path),
	}
	if w.DerivationPath == "" {
		w.DerivationPath = DefaultDerivationPath
	}
	switch {
	case w.XPub == "":
		v.add("xpub", "required", "extended public key is required")
	case len(w.XPub) > 256:
		v.add("xpub", "invalid", "extended public key is too long")
	case looksLikePrivateKey(w.XPub):
		v.add("xpub", "private_key", "that is a private key; hmnpay only ever needs the extended public key")
	}
	if len(w.DerivationPath) > 100 || !strings.HasPrefix(w.DerivationPath, "m") {
		v.add("derivation_path", "invalid", "derivation path must look like %s", DefaultDerivationPath)
	}
	return w
}

// looksLikePrivateKey reports whether s starts with the version prefix of a
// serialized extended private key (xprv, yprv, zprv, tprv, uprv, vprv and
// their multisig variants Yprv, Zprv, Uprv, Vprv). It is only a courtesy
// that gives a clearer message for the common mistake: the letters "prv"
// elsewhere in a key mean nothing (Base58 payloads contain them by chance),
// and the address source is what actually rejects private keys.
func looksLikePrivateKey(s string) bool {
	return len(s) > 4 && s[1:4] == "prv" && strings.ContainsRune("xyztuvYZUV", rune(s[0]))
}

// deriveChecked derives one address and reports an address-source failure
// as a validation error on the xpub.
func (s *Service) deriveChecked(ctx context.Context, w WalletSettings, index uint32) (string, error) {
	addr, err := s.addresses.DeriveAddress(ctx, w, index)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &Error{Kind: KindInvalid, Code: "validation_failed", Err: err,
			Fields: []FieldError{{Field: "xpub", Code: "invalid", Message: "this key cannot be used to derive addresses: " + errMessage(err)}}}
	}
	if _, err := NormalizeAddress(addr); err != nil {
		return "", internal("derive deposit address", err)
	}
	return addr, nil
}

// SetupInput is everything the setup wizard collects.
type SetupInput struct {
	// Email and Password create the operator account.
	Email    string
	Password string

	// ChainID selects the network (56 or 97); 0 means BSC mainnet.
	ChainID uint64
	// Asset overrides the built-in asset of ChainID; usually nil.
	Asset *Asset
	// RPCURLs are the JSON-RPC endpoints in order of preference; at least
	// one.
	RPCURLs []string

	// XPub and DerivationPath configure deposit addresses. The wizard
	// must have shown [Service.PreviewAddresses] and obtained the
	// operator's confirmation before submitting.
	XPub           string
	DerivationPath string

	// Merchant is the business identity shown on checkout.
	Merchant MerchantSettings
	// PublicURL is the base URL of the instance; the transport usually
	// derives it from the request.
	PublicURL string

	// UserAgent and IP describe the client, for the session that is
	// opened.
	UserAgent string
	IP        string

	// SetupToken is the instance's setup token ([Config.SetupToken]).
	// Required when one is configured, unless the caller is a system
	// actor.
	SetupToken string
}

// CompleteSetup performs first-run setup in one atomic step: it validates
// everything the wizard collected, creates the operator account, stores
// the settings with SetupCompleted set, and opens a session so the
// operator lands signed in. Nothing is stored unless everything is valid,
// so an abandoned or failed wizard leaves no half-configured instance.
//
// It can succeed only once: afterwards it fails with [ErrSetupCompleted].
//
// Whoever completes setup owns the instance — the account, and the wallet
// every payment goes to. When the instance was started with a setup token
// ([Config.SetupToken], which server wiring always sets), the caller must
// present it in in.SetupToken: it fails with [ErrSetupTokenRequired] or
// [ErrSetupTokenInvalid] before looking at anything else in the input, so
// reaching the port first is not enough to claim an instance. Without a
// configured token it is public, which is only appropriate for tests and
// for programs embedding the engine.
func (s *Service) CompleteSetup(ctx context.Context, in SetupInput) (LoginResult, error) {
	var res LoginResult
	if status, err := s.SetupStatus(ctx); err != nil {
		return res, err
	} else if status.Completed {
		return res, newError(ErrSetupCompleted, "")
	}
	if err := s.checkSetupToken(ctx, in.SetupToken); err != nil {
		return res, err
	}
	var v validation
	email := cleanEmail(&v, "email", in.Email, true)
	validatePassword(&v, "password", in.Password)

	st := DefaultSettings()
	st.Merchant = in.Merchant.normalize(&v)
	st.PublicURL = strings.TrimRight(cleanWebURL(&v, "public_url", in.PublicURL, false), "/")
	chainID := in.ChainID
	if chainID == 0 {
		chainID = ChainBSCMainnet
	}
	st.Network = NetworkSettings{ChainID: chainID, RPCURLs: in.RPCURLs}.normalize(&v)
	asset, _ := resolveAsset(&v, chainID, in.Asset, st)
	st.Asset = asset
	st.Wallet = normalizeWallet(&v, in.XPub, in.DerivationPath)
	if err := v.errOrNil(); err != nil {
		return res, err
	}
	if _, err := s.deriveChecked(ctx, st.Wallet, 0); err != nil {
		return res, err
	}
	hash, err := s.hashPassword(ctx, in.Password)
	if err != nil {
		return res, err
	}
	err = s.update(ctx, func(u *unit) error {
		cur, err := u.loadSettings(ctx)
		if err != nil {
			return err
		}
		if cur.SetupCompleted {
			return newError(ErrSetupCompleted, "")
		}
		st.SetupCompleted = true
		if err := u.saveSettings(ctx, st); err != nil {
			return err
		}
		id, err := s.newID(prefixOperator)
		if err != nil {
			return err
		}
		op := Operator{ID: id, Email: email, PasswordHash: hash, CreatedAt: u.now, UpdatedAt: u.now}
		if err := u.SaveOperator(ctx, op); err != nil {
			return err
		}
		if res, err = s.openSession(ctx, u, op, in.UserAgent, in.IP); err != nil {
			return err
		}
		return u.emit(ctx, EventSettingsUpdated, "", SettingsUpdatedData{Section: "setup"})
	})
	res.Operator = res.Operator.redacted()
	return res, err
}

// ---- settings ----

// GetSettings returns the instance settings. Operator only.
func (s *Service) GetSettings(ctx context.Context) (Settings, error) {
	var st Settings
	if _, err := authorize(ctx, permOperator); err != nil {
		return st, err
	}
	err := s.store.View(ctx, func(r Reader) error {
		var err error
		st, err = loadSettings(ctx, r)
		return err
	})
	return st, err
}

// updateSettings runs one settings change and announces it.
func (s *Service) updateSettings(ctx context.Context, section string, change func(ctx context.Context, u *unit, st *Settings) error) (Settings, error) {
	var out Settings
	if _, err := authorize(ctx, permOperator); err != nil {
		return out, err
	}
	err := s.update(ctx, func(u *unit) error {
		st, err := requireSetup(ctx, u)
		if err != nil {
			return err
		}
		if err := change(ctx, u, &st); err != nil {
			return err
		}
		if err := u.saveSettings(ctx, st); err != nil {
			return err
		}
		out = st.clone()
		out.UpdatedAt = u.now
		return u.emit(ctx, EventSettingsUpdated, "", SettingsUpdatedData{Section: section})
	})
	return out, err
}

// UpdateMerchantInput is the editable merchant identity.
type UpdateMerchantInput struct {
	Merchant MerchantSettings
	// PublicURL is the base URL of the instance.
	PublicURL string
}

// UpdateMerchant replaces the merchant identity and public URL. Operator
// only.
func (s *Service) UpdateMerchant(ctx context.Context, in UpdateMerchantInput) (Settings, error) {
	return s.updateSettings(ctx, "merchant", func(_ context.Context, _ *unit, st *Settings) error {
		var v validation
		st.Merchant = in.Merchant.normalize(&v)
		st.PublicURL = strings.TrimRight(cleanWebURL(&v, "public_url", in.PublicURL, false), "/")
		return v.errOrNil()
	})
}

// UpdatePaymentDefaults replaces the defaults for new payments (TTL,
// confirmations, underpayment tolerance, default redirect). Existing
// payments keep the values they were created with. Operator only.
func (s *Service) UpdatePaymentDefaults(ctx context.Context, in PaymentSettings) (Settings, error) {
	return s.updateSettings(ctx, "payments", func(_ context.Context, _ *unit, st *Settings) error {
		var v validation
		st.Payments = in.normalize(&v, st.Asset)
		return v.errOrNil()
	})
}

// UpdateNetworkInput changes how the instance reaches the chain.
type UpdateNetworkInput struct {
	// ChainID selects the network; 0 keeps the current one.
	ChainID uint64
	// Asset overrides the asset; nil keeps the current asset when the
	// chain is unchanged and uses the built-in one otherwise.
	Asset *Asset
	// RPCURLs are the JSON-RPC endpoints in order of preference.
	RPCURLs []string
}

// UpdateNetwork replaces the RPC endpoints and, optionally, switches the
// chain or asset. Switching is refused with [ErrOpenPayments] while any
// payment of the current network is still open, because its deposit
// address would stop being watched; the stored chain head is reset on a
// switch. Operator only.
func (s *Service) UpdateNetwork(ctx context.Context, in UpdateNetworkInput) (Settings, error) {
	return s.updateSettings(ctx, "network", func(ctx context.Context, u *unit, st *Settings) error {
		var v validation
		chainID := in.ChainID
		if chainID == 0 {
			chainID = st.Network.ChainID
		}
		network := NetworkSettings{ChainID: chainID, RPCURLs: in.RPCURLs}.normalize(&v)
		asset, _ := resolveAsset(&v, chainID, in.Asset, *st)
		if err := v.errOrNil(); err != nil {
			return err
		}
		if asset != st.Asset {
			open, err := u.CountPayments(ctx, PaymentFilter{Statuses: OpenStatuses})
			if err != nil {
				return err
			}
			if open > 0 {
				return newError(ErrOpenPayments, "")
			}
			if err := u.SaveChainHead(ctx, ChainHead{ChainID: chainID}); err != nil {
				return err
			}
			u.head = nil
			// The tolerance is a decimal string; make sure it still parses
			// under the new asset's decimals.
			if _, err := ParseAmount(st.Payments.UnderpaymentTolerance, asset.Decimals); err != nil {
				st.Payments.UnderpaymentTolerance = "0"
			}
		}
		st.Network, st.Asset = network, asset
		return nil
	})
}

// UpdateWalletInput changes deposit-address allocation.
type UpdateWalletInput struct {
	XPub           string
	DerivationPath string
	// NextAddressIndex sets the index the next payment uses. Nil keeps the
	// current index when the key and path are unchanged, and restarts at 0
	// for a new key.
	NextAddressIndex *uint32
}

// UpdateWallet replaces the xpub and derivation path. Existing payments
// keep their addresses and stay watched; new payments use the new wallet.
// The key is verified by deriving an address. Addresses already used by a
// payment are never handed out again, even if the index is rewound.
// Operator only.
func (s *Service) UpdateWallet(ctx context.Context, in UpdateWalletInput) (Settings, error) {
	return s.updateSettings(ctx, "wallet", func(ctx context.Context, _ *unit, st *Settings) error {
		var v validation
		w := normalizeWallet(&v, in.XPub, in.DerivationPath)
		if in.NextAddressIndex != nil && *in.NextAddressIndex >= 1<<31 {
			v.add("next_address_index", "too_large", "address index must be below 2^31")
		}
		if err := v.errOrNil(); err != nil {
			return err
		}
		if _, err := s.deriveChecked(ctx, w, 0); err != nil {
			return err
		}
		switch {
		case in.NextAddressIndex != nil:
			w.NextAddressIndex = *in.NextAddressIndex
		case w.XPub == st.Wallet.XPub && w.DerivationPath == st.Wallet.DerivationPath:
			w.NextAddressIndex = st.Wallet.NextAddressIndex
		}
		st.Wallet = w
		return nil
	})
}

// ---- overview ----

// PeriodTotals summarises one period of the overview.
type PeriodTotals struct {
	// Volume is the confirmed amount of payments that settled as paid or
	// overpaid in the period, as a decimal string.
	Volume string `json:"volume"`
	// Paid is the number of those payments.
	Paid int `json:"paid"`
	// Created is the number of payments created in the period.
	Created int `json:"created"`
	// ConversionRate is Paid / Created, 0 when nothing was created.
	ConversionRate float64 `json:"conversion_rate"`
	// AverageTicket is Volume / Paid as a decimal string ("0.00" when
	// nothing was paid).
	AverageTicket string `json:"average_ticket"`
}

// DayPoint is one UTC day of the overview series.
type DayPoint struct {
	// Date is the UTC day, YYYY-MM-DD.
	Date    string `json:"date"`
	Volume  string `json:"volume"`
	Paid    int    `json:"paid"`
	Created int    `json:"created"`
}

// Overview is the dashboard's home-page read model.
type Overview struct {
	// Days is the length of the period.
	Days int `json:"days"`
	// Asset and Decimals say what the amounts are in.
	Asset    string `json:"asset"`
	Decimals int    `json:"decimals"`
	// Current covers the last Days UTC days including today; Previous the
	// Days days before that.
	Current  PeriodTotals `json:"current"`
	Previous PeriodTotals `json:"previous"`
	// Series has one point per day of the current period, oldest first.
	Series []DayPoint `json:"series"`
	// OpenPayments counts payments that are awaiting, detected or
	// confirming right now.
	OpenPayments int `json:"open_payments"`
	// NeedsAttention counts payments with an unresolved issue.
	NeedsAttention int `json:"needs_attention"`
}

// Overview returns volume, paid count, conversion and average ticket for
// the last `days` UTC days (1–366; 0 means 30) next to the period before,
// a per-day series, and the counts of open payments and payments needing
// attention. Only payments in the instance's current asset are counted.
// Requires [ScopePaymentsRead].
//
// A payment counts towards the day it settled as paid or overpaid, with
// its confirmed amount; it counts as created on the day it was created.
func (s *Service) Overview(ctx context.Context, days int) (Overview, error) {
	var ov Overview
	if _, err := authorize(ctx, permScope(ScopePaymentsRead)); err != nil {
		return ov, err
	}
	if days == 0 {
		days = 30
	}
	if days < 1 || days > 366 {
		return ov, invalidField("days", "invalid", "days must be between 1 and 366")
	}
	now := s.now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	start := today.AddDate(0, 0, -(days - 1))
	prevStart := start.AddDate(0, 0, -days)
	end := today.AddDate(0, 0, 1)

	type bucket struct {
		volume        Amount
		paid, created int
	}
	series := make([]bucket, days)
	var cur, prev bucket
	add := func(t time.Time, f func(b *bucket)) {
		if t.Before(start) {
			f(&prev)
			return
		}
		f(&cur)
		if i := int(t.Sub(start) / (24 * time.Hour)); i >= 0 && i < days {
			f(&series[i])
		}
	}

	var asset Asset
	err := s.store.View(ctx, func(r Reader) error {
		st, err := loadSettings(ctx, r)
		if err != nil {
			return err
		}
		asset = st.Asset
		created, err := r.ListPayments(ctx, PaymentFilter{CreatedFrom: prevStart, CreatedTo: end, ChainID: asset.ChainID}, PaymentSort{}, Page{})
		if err != nil {
			return err
		}
		for _, p := range created {
			if p.Asset == asset {
				add(p.CreatedAt, func(b *bucket) { b.created++ })
			}
		}
		paid, err := r.ListPayments(ctx, PaymentFilter{PaidFrom: prevStart, PaidTo: end, ChainID: asset.ChainID}, PaymentSort{}, Page{})
		if err != nil {
			return err
		}
		for _, p := range paid {
			if p.Asset == asset {
				add(p.PaidAt, func(b *bucket) {
					b.paid++
					b.volume = b.volume.Add(p.AmountConfirmed)
				})
			}
		}
		if ov.OpenPayments, err = r.CountPayments(ctx, GroupOpen.Apply(PaymentFilter{})); err != nil {
			return err
		}
		ov.NeedsAttention, err = r.CountPayments(ctx, GroupAttention.Apply(PaymentFilter{}))
		return err
	})
	if err != nil {
		return ov, err
	}

	totals := func(b bucket) PeriodTotals {
		t := PeriodTotals{Volume: b.volume.Format(asset.Decimals), Paid: b.paid, Created: b.created, AverageTicket: Amount{}.Format(asset.Decimals)}
		if b.created > 0 {
			t.ConversionRate = float64(b.paid) / float64(b.created)
		}
		if b.paid > 0 {
			avg := new(big.Int).Quo(b.volume.Big(), big.NewInt(int64(b.paid)))
			t.AverageTicket = amountFromBig(avg).Format(asset.Decimals)
		}
		return t
	}
	ov.Days, ov.Asset, ov.Decimals = days, asset.Symbol, asset.Decimals
	ov.Current, ov.Previous = totals(cur), totals(prev)
	ov.Series = make([]DayPoint, days)
	for i, b := range series {
		ov.Series[i] = DayPoint{
			Date:    start.AddDate(0, 0, i).Format("2006-01-02"),
			Volume:  b.volume.Format(asset.Decimals),
			Paid:    b.paid,
			Created: b.created,
		}
	}
	return ov, nil
}

// InstanceSummary is what the person who owns the server needs to see to
// judge whether an instance is really theirs — in particular after
// recovering access with [Service.ResetOperatorPassword], which replaces
// the password and nothing else: where payments go, which nodes are
// trusted, and which credentials exist.
type InstanceSummary struct {
	// SetupCompleted is false on a fresh instance; everything else is
	// then empty.
	SetupCompleted bool
	// OperatorEmail is the operator account's login.
	OperatorEmail     string
	OperatorCreatedAt time.Time
	// ChainID and Asset are the network and token in use.
	ChainID uint64
	Asset   Asset
	// RPCHosts are the configured RPC endpoints reduced to scheme and
	// host: paths, query strings and credentials — where API keys live —
	// are left out.
	RPCHosts []string
	// XPub and DerivationPath are the wallet every payment is paid to;
	// NextAddressIndex is how many addresses it has handed out.
	XPub             string
	DerivationPath   string
	NextAddressIndex uint32
	// FirstAddress is the wallet's address at index 0, the one the setup
	// wizard showed for comparison with the owner's own wallet. Empty if
	// it could not be derived (FirstAddressError says why).
	FirstAddress      string
	FirstAddressError string
	// APIKeys counts all API keys ever created, ActiveAPIKeys those that
	// still work (neither revoked nor expired).
	APIKeys       int
	ActiveAPIKeys int
	// WebhookEndpoints counts registered endpoints, EnabledWebhookEndpoints
	// those that receive events.
	WebhookEndpoints        int
	EnabledWebhookEndpoints int
	// Sessions counts the dashboard sessions that still work.
	Sessions int
}

// InstanceSummary reports who and what an instance is configured for (see
// [InstanceSummary]). It reads only. Operator only (which includes system
// actors: the server's own command line).
func (s *Service) InstanceSummary(ctx context.Context) (InstanceSummary, error) {
	var sum InstanceSummary
	if _, err := authorize(ctx, permOperator); err != nil {
		return sum, err
	}
	now := s.now()
	var wallet WalletSettings
	err := s.store.View(ctx, func(r Reader) error {
		st, err := loadSettings(ctx, r)
		if err != nil || !st.SetupCompleted {
			return err
		}
		sum.SetupCompleted = true
		sum.ChainID, sum.Asset = st.Network.ChainID, st.Asset
		for _, raw := range st.Network.RPCURLs {
			host := "(unparsable)"
			if u, err := url.Parse(raw); err == nil && u.Host != "" {
				host = u.Scheme + "://" + u.Host
			}
			sum.RPCHosts = append(sum.RPCHosts, host)
		}
		wallet = st.Wallet
		sum.XPub, sum.DerivationPath, sum.NextAddressIndex = wallet.XPub, wallet.DerivationPath, wallet.NextAddressIndex
		op, err := r.GetOperator(ctx)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		sum.OperatorEmail, sum.OperatorCreatedAt = op.Email, op.CreatedAt
		keys, err := r.ListAPIKeys(ctx)
		if err != nil {
			return err
		}
		sum.APIKeys = len(keys)
		for _, k := range keys {
			if k.RevokedAt.IsZero() && (k.ExpiresAt.IsZero() || now.Before(k.ExpiresAt)) {
				sum.ActiveAPIKeys++
			}
		}
		endpoints, err := r.ListWebhookEndpoints(ctx)
		if err != nil {
			return err
		}
		sum.WebhookEndpoints = len(endpoints)
		for _, e := range endpoints {
			if e.Enabled {
				sum.EnabledWebhookEndpoints++
			}
		}
		sessions, err := r.ListSessions(ctx)
		if err != nil {
			return err
		}
		for _, sess := range sessions {
			if now.Before(s.sessionDeadline(sess)) {
				sum.Sessions++
			}
		}
		return nil
	})
	if err != nil || !sum.SetupCompleted {
		return sum, err
	}
	// Outside the read transaction: derivation is the address source's.
	if addr, err := s.addresses.DeriveAddress(ctx, wallet, 0); err != nil {
		sum.FirstAddressError = errMessage(err)
	} else {
		sum.FirstAddress = addr
	}
	return sum, nil
}
