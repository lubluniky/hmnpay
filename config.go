package hmnpay

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/corebridge"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
)

// Version and Commit identify the build. The release build sets them with
//
//	-ldflags "-X github.com/lubluniky/hmnpay.Version=1.2.3 -X github.com/lubluniky/hmnpay.Commit=abc123"
//
// Version is also sent in the webhook User-Agent ("hmnpay-webhook/<Version>").
var (
	Version = "dev"
	Commit  = ""
)

// DatabaseFile is the name of the SQLite database inside [Config.DataDir].
// It runs in WAL mode, so "hmnpay.db-wal" and "hmnpay.db-shm" live next to
// it and belong to it.
const DatabaseFile = "hmnpay.db"

// Defaults.
const (
	// DefaultShutdownTimeout bounds how long [Engine.Run] drains its
	// workers after its context ends.
	DefaultShutdownTimeout = 30 * time.Second
	// DefaultPruneInterval is how often old webhook deliveries and expired
	// sessions are deleted.
	DefaultPruneInterval = time.Hour
)

// ErrInvalidConfig is matched (errors.Is) by the error [Open] returns for
// a [Config] that fails validation.
var ErrInvalidConfig = errors.New("hmnpay: invalid config")

// Config configures [Open]. Only DataDir is required; the zero value of
// every other field is the production default.
type Config struct {
	// DataDir is the directory holding all state (the SQLite database and
	// its WAL). It is created (mode 0700) if missing. Required.
	DataDir string

	// Logger receives the logs of every component. Default: slog.Default().
	Logger *slog.Logger

	// PublicURL is the external base URL of the instance
	// ("https://pay.example.com"); it is the default of
	// [HTTPOptions.PublicURL]. Optional; must be an absolute http(s) URL
	// without path, query or credentials when set.
	PublicURL string

	// Webhooks configures the webhook dispatcher and the URL policy every
	// transport applies when an endpoint is saved.
	Webhooks WebhookConfig

	// Chain configures the chain watcher (the bridge between the core and
	// the RPC endpoints the operator configured in the setup wizard).
	Chain ChainConfig

	// Prober configures the RPC endpoint probe used by the setup wizard and
	// the network settings page.
	Prober ProberConfig

	// Prune configures the periodic deletion of old webhook deliveries and
	// expired sessions ([core.Service.Prune]).
	Prune PruneConfig

	// ShutdownTimeout bounds how long [Engine.Run] waits for its workers to
	// drain after its context ends. Default [DefaultShutdownTimeout].
	ShutdownTimeout time.Duration

	// SetupToken guards first-run setup: until setup is completed, an
	// anonymous caller of the setup wizard's API must present it, so that
	// whoever merely reaches the port first cannot claim the instance
	// (system actors in the embedding program never need it).
	//
	// Empty (the default) generates a strong random token in Open; read it
	// with [Engine.SetupToken] and show it to the person installing. A
	// generated token is not stored: every Open makes a new one, which is
	// harmless because it only matters until setup is completed. A set
	// value must pass [ValidateSetupToken].
	SetupToken string
	// NoSetupToken opts out of the setup token entirely: anyone who
	// reaches the HTTP API before setup is completed can claim the
	// instance. Only for tests and embedders that never expose the setup
	// API. It cannot be combined with SetupToken.
	NoSetupToken bool

	// Policy tunes the core's limits and windows; zero fields take the
	// core's defaults.
	Policy PolicyConfig

	// Clock, Rand and PasswordHasher replace the core's defaults (system
	// clock, crypto/rand, [core.PBKDF2Hasher]). For tests: Rand must be
	// cryptographically secure in production.
	Clock          core.Clock
	Rand           io.Reader
	PasswordHasher core.PasswordHasher
}

// PolicyConfig passes the core's tunables through (see core.Config for the
// full semantics). Zero means the core's default; where noted, a negative
// value switches the mechanism off.
type PolicyConfig struct {
	// ExpiryGrace is added to a payment's deadline (default 1 min;
	// negative: none).
	ExpiryGrace time.Duration
	// WatchWindow / UnfundedWatchWindow: how long a settled payment's
	// address stays watched after funds were / were never seen (defaults
	// 7 days / 1 hour).
	WatchWindow         time.Duration
	UnfundedWatchWindow time.Duration
	// MaxWatchedAddresses caps the watch list; at the cap, anonymous link
	// payments are refused (default 5 000; negative: no cap). Every 500
	// watched addresses cost one eth_getLogs call per tick on each of two
	// RPC endpoints.
	MaxWatchedAddresses int
	// MaxHeadTimeAhead: chain heads whose block time is further ahead of
	// this server's clock are refused (default 2 min; negative: off). On a
	// host without time synchronisation whose clock is behind, nothing
	// confirms and the chain watcher reports refused heads — fix the clock
	// rather than switching this off.
	MaxHeadTimeAhead time.Duration
	// LinkReservation: how long an unfunded payment created from a limited
	// payment link holds one of its uses (default 10 min; negative: none).
	LinkReservation time.Duration
	// SessionTTL / SessionIdleTimeout: absolute and idle lifetime of a
	// dashboard session (defaults 14 days / 24 h; negative idle: none).
	SessionTTL         time.Duration
	SessionIdleTimeout time.Duration
	// MaxConcurrentKDF bounds concurrent password hashing (default 4).
	MaxConcurrentKDF int
}

// MinSetupTokenLength is the shortest setup token [ValidateSetupToken]
// accepts.
const MinSetupTokenLength = 20

// ValidateSetupToken rejects setup tokens that are too weak to stand
// between the internet and an unclaimed instance: shorter than
// [MinSetupTokenLength], with whitespace or control characters, or with
// fewer than 10 distinct characters ("aaaa…", "abababab…").
func ValidateSetupToken(token string) error {
	if len(token) < MinSetupTokenLength {
		return fmt.Errorf("setup token must be at least %d characters (generate one with: openssl rand -hex 16)", MinSetupTokenLength)
	}
	if len(token) > 256 {
		return errors.New("setup token must be at most 256 characters")
	}
	distinct := map[rune]bool{}
	for _, r := range token {
		if r <= ' ' || r == 0x7f {
			return errors.New("setup token must not contain whitespace or control characters")
		}
		distinct[r] = true
	}
	if len(distinct) < 10 {
		return errors.New("setup token is too repetitive (use a random value, e.g. openssl rand -hex 16)")
	}
	return nil
}

// WebhookConfig configures webhook delivery. The zero value is the strict
// default: deliveries to loopback and private networks are refused.
type WebhookConfig struct {
	// Disabled leaves the dispatcher out of [Engine.Run]. Deliveries are
	// still planned by the core and wait until a dispatcher runs.
	Disabled bool
	// AllowPrivateNetworks permits RFC 1918, CGNAT, unique-local and
	// similar destinations. Loopback is never allowed by this switch; list
	// it in AllowedPrefixes. Off by default: whoever holds an API key with
	// the webhooks:manage scope chooses webhook URLs and sees the responses.
	AllowPrivateNetworks bool
	// AllowedPrefixes are loopback or private networks permitted anyway
	// (e.g. 127.0.0.1/32 for a receiver on the same host). Link-local,
	// metadata and other always-blocked ranges cannot be unlocked.
	AllowedPrefixes []netip.Prefix
	// RequireHTTPS refuses plain-http endpoint URLs.
	RequireHTTPS bool
	// AllowedPorts, when non-empty, restricts destination ports.
	AllowedPorts []int
	// Concurrency is the number of deliveries in flight. Default 8.
	Concurrency int
	// PollInterval is the dispatcher's fallback timer for due retries.
	// Default 5 s.
	PollInterval time.Duration
}

// VerifyMode selects how the chain watcher cross-checks what one RPC
// endpoint reports before the core hears of it.
type VerifyMode int

// Verify modes.
const (
	// VerifyAuto (default): with two or more endpoints every new transfer
	// and every revert is confirmed by an endpoint other than the one that
	// reported it; with one endpoint it is re-asked (which catches
	// inconsistency, not lies).
	VerifyAuto VerifyMode = iota
	// VerifyBestEffort uses another endpoint when one is healthy and falls
	// back to the reporting endpoint otherwise: more available, weaker.
	VerifyBestEffort
	// VerifyStrict never acts on one endpoint's word; with a single
	// endpoint configured it degrades to VerifyAuto with a warning.
	VerifyStrict
)

// String returns the mode's configuration name.
func (m VerifyMode) String() string {
	switch m {
	case VerifyAuto:
		return "auto"
	case VerifyBestEffort:
		return "best-effort"
	case VerifyStrict:
		return "strict"
	}
	return fmt.Sprintf("VerifyMode(%d)", int(m))
}

// ParseVerifyMode parses "auto", "best-effort" or "strict" ("" is auto).
func ParseVerifyMode(s string) (VerifyMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return VerifyAuto, nil
	case "best-effort", "besteffort", "best_effort":
		return VerifyBestEffort, nil
	case "strict":
		return VerifyStrict, nil
	}
	return 0, fmt.Errorf("unknown verify mode %q (want auto, best-effort or strict)", s)
}

func (m VerifyMode) watcher() watcher.VerifyMode {
	switch m {
	case VerifyBestEffort:
		return watcher.VerifyBestEffort
	case VerifyStrict:
		return watcher.VerifyStrict
	}
	return watcher.VerifyAuto
}

// ChainAlert is a condition of the chain watcher that needs a human (a
// reorganisation that replaced a finalised block, finality that has stopped,
// a rejected fact, a discarded checkpoint).
type ChainAlert = corebridge.Alert

// ChainConfig configures the chain watcher. The network itself (chain,
// token, RPC endpoints, confirmations) is instance configuration, set in
// the setup wizard and the settings API, not here.
type ChainConfig struct {
	// Disabled leaves the chain watcher out of [Engine.Run]: no payment is
	// ever detected. For tests and for embedders running their own.
	Disabled bool
	// Verify selects the cross-check mode. Default VerifyAuto.
	Verify VerifyMode
	// PollInterval is the pause between scans. Default 3 s.
	PollInterval time.Duration
	// IdlePollInterval is the pause between scans while no payment is
	// open and no transfer is waiting to become final: the watch list is
	// empty, or holds only the addresses of closed payments — every block
	// is still scanned for those, and late funds on one of them are
	// noticed up to this much later. Creating a payment ends the pause at
	// once. Default 30 s. A negative value, or one that is not longer than
	// PollInterval, switches it off.
	IdlePollInterval time.Duration
	// SettleDepth is how many blocks the processed head trails the chain
	// tip. Default 16 (BSC). Lower it only for endpoints you run yourself.
	SettleDepth uint64
	// StatusInterval is how often endpoint health is reported to the core
	// (default 5 s); ReloadInterval how often configuration is re-read
	// without a change event (default 30 s); MaxBackoff caps retry delays
	// (default 30 s).
	StatusInterval time.Duration
	ReloadInterval time.Duration
	MaxBackoff     time.Duration
	// RequestTimeout bounds one RPC round trip. Default 10 s.
	RequestTimeout time.Duration
	// OnAlert, when set, is called (from the watcher's goroutines) for
	// every alert; alerts are logged either way.
	OnAlert func(ChainAlert)
}

// ProberConfig configures the RPC endpoint probe.
type ProberConfig struct {
	// Disabled removes the probe: the setup wizard's "test endpoint"
	// answers "unavailable".
	Disabled bool
	// DenyPrivateNetworks refuses to probe loopback and private addresses.
	// Until setup is completed anyone who can reach the instance can ask it
	// to probe a URL; turn this on when the instance is exposed before
	// setup and no local node is used.
	DenyPrivateNetworks bool
}

// PruneConfig configures the periodic clean-up.
type PruneConfig struct {
	// Disabled leaves the clean-up out of [Engine.Run].
	Disabled bool
	// Interval between runs (the first runs at start). Default
	// [DefaultPruneInterval].
	Interval time.Duration
}

// validate checks c and fills defaults.
func (c Config) validate() (Config, error) {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if strings.TrimSpace(c.DataDir) == "" {
		bad("DataDir is required")
	}
	if c.PublicURL != "" {
		if err := checkBaseURL(c.PublicURL); err != nil {
			bad("PublicURL: %v", err)
		}
	}
	nonNegative := func(name string, d time.Duration) {
		if d < 0 {
			bad("%s must not be negative", name)
		}
	}
	nonNegative("ShutdownTimeout", c.ShutdownTimeout)
	nonNegative("Prune.Interval", c.Prune.Interval)
	nonNegative("Webhooks.PollInterval", c.Webhooks.PollInterval)
	nonNegative("Chain.PollInterval", c.Chain.PollInterval)
	nonNegative("Chain.StatusInterval", c.Chain.StatusInterval)
	nonNegative("Chain.ReloadInterval", c.Chain.ReloadInterval)
	nonNegative("Chain.MaxBackoff", c.Chain.MaxBackoff)
	nonNegative("Chain.RequestTimeout", c.Chain.RequestTimeout)
	if c.Webhooks.Concurrency < 0 {
		bad("Webhooks.Concurrency must not be negative")
	}
	for _, p := range c.Webhooks.AllowedPrefixes {
		if !p.IsValid() {
			bad("Webhooks.AllowedPrefixes: invalid prefix %v", p)
		}
	}
	for _, port := range c.Webhooks.AllowedPorts {
		if port < 1 || port > 65535 {
			bad("Webhooks.AllowedPorts: invalid port %d", port)
		}
	}
	if c.Chain.Verify < VerifyAuto || c.Chain.Verify > VerifyStrict {
		bad("Chain.Verify: unknown mode %d", int(c.Chain.Verify))
	}
	if c.Chain.SettleDepth > 1024 {
		bad("Chain.SettleDepth %d is unreasonably deep (max 1024)", c.Chain.SettleDepth)
	}
	if c.SetupToken != "" && c.NoSetupToken {
		bad("SetupToken and NoSetupToken are mutually exclusive")
	}
	if c.SetupToken != "" {
		if err := ValidateSetupToken(c.SetupToken); err != nil {
			bad("SetupToken: %v", err)
		}
	}
	if c.Policy.MaxConcurrentKDF < 0 {
		bad("Policy.MaxConcurrentKDF must not be negative")
	}
	nonNegative("Policy.WatchWindow", c.Policy.WatchWindow)
	nonNegative("Policy.UnfundedWatchWindow", c.Policy.UnfundedWatchWindow)
	nonNegative("Policy.SessionTTL", c.Policy.SessionTTL)
	if len(errs) > 0 {
		return c, fmt.Errorf("%w: %w", ErrInvalidConfig, errors.Join(errs...))
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.Prune.Interval == 0 {
		c.Prune.Interval = DefaultPruneInterval
	}
	return c, nil
}

// checkBaseURL accepts an absolute http(s) URL with a host and nothing
// after it but an optional "/".
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return err
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("%q must start with http:// or https://", raw)
	case u.Host == "":
		return fmt.Errorf("%q has no host", raw)
	case u.User != nil:
		return fmt.Errorf("%q must not contain credentials", raw)
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("%q must be a bare origin (no path, query or fragment)", raw)
	}
	return nil
}
