package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lubluniky/hmnpay"
)

// serverConfig is the deployment-level configuration of "hmnpay serve".
// Everything about the instance itself (network, endpoints, wallet,
// merchant) lives in the database and is set through the setup wizard.
type serverConfig struct {
	Listen          string
	DataDir         string
	PublicURL       string
	TrustedProxies  []string
	TrustedOrigins  []string
	CORSOrigins     []string
	PublicCORS      bool
	LogLevel        string
	LogFormat       string
	WebhookPrivate  bool
	WebhookPrefixes []string
	WebhookHTTPS    bool
	RPCDenyPrivate  bool
	ChainVerify     string
	WebUpstream     string
	// WebUpstreamRemote allows WebUpstream to be something else than a
	// loopback address.
	WebUpstreamRemote bool
	ShutdownTimeout   time.Duration
	SetupToken        string
	AllowedHosts      []string
	// MaxHeadTimeAhead: 0 = core default, negative = off.
	MaxHeadTimeAhead time.Duration
	// PollInterval is the chain watcher's pause between scans;
	// IdlePollInterval the pause while no payment is open — nothing is
	// watched, or closed payments only (negative: idle polling is off).
	PollInterval     time.Duration
	IdlePollInterval time.Duration
}

// Bounds of the poll intervals. A scan every second is as fast as two blocks
// of latency can use; one every 30 seconds is as slow as a checkout page can
// bear. Five minutes of idle pause is some 670 blocks: the first scan after it
// is still a handful of log queries, the processed head a new payment starts
// from is not older than that, and money sent to a payment that is already
// closed is noticed no later than that.
const (
	minPollInterval     = time.Second
	maxPollInterval     = 30 * time.Second
	maxIdlePollInterval = 5 * time.Minute
)

// envVar documents one setting: its variable, flag, default and meaning.
// The table is the single source for flags, -h output and .env.example.
type envVar struct {
	env, flag, def, help string
}

var settings = []envVar{
	{"HMNPAY_LISTEN", "listen", ":8080", "address to listen on"},
	{"HMNPAY_DATA_DIR", "data-dir", "./data", "directory holding the database (back it up)"},
	{"HMNPAY_PUBLIC_URL", "public-url", "", "external base URL, e.g. https://pay.example.com (cookie security, CSRF origin, allowed Host, setup default)"},
	{"HMNPAY_ALLOWED_HOSTS", "allowed-hosts", "", "comma-separated extra Host names the API answers for (the public URL's host, localhost and IPs always work; others get 421)"},
	{"HMNPAY_SETUP_TOKEN", "setup-token", "", "first-run setup token (at least 20 random characters); empty generates one at each start and prints it to stderr"},
	{"HMNPAY_TRUSTED_PROXIES", "trusted-proxies", "", "comma-separated IPs/CIDRs of reverse proxies whose X-Forwarded-* headers are believed"},
	{"HMNPAY_TRUSTED_ORIGINS", "trusted-origins", "", "comma-separated extra origins allowed to make cookie-authenticated writes"},
	{"HMNPAY_CORS_ORIGINS", "cors-origins", "", "comma-separated origins allowed cross-origin bearer-token API access"},
	{"HMNPAY_PUBLIC_CORS", "public-cors", "false", "allow any origin to read the public checkout endpoints"},
	{"HMNPAY_LOG_LEVEL", "log-level", "info", "debug, info, warn or error"},
	{"HMNPAY_LOG_FORMAT", "log-format", "text", "text or json"},
	{"HMNPAY_WEBHOOK_ALLOW_PRIVATE", "webhook-allow-private", "false", "allow webhook delivery to private networks (RFC 1918 etc.; not loopback)"},
	{"HMNPAY_WEBHOOK_ALLOWED_PREFIXES", "webhook-allowed-prefixes", "", "comma-separated loopback/private CIDRs webhooks may reach anyway, e.g. 127.0.0.1/32"},
	{"HMNPAY_WEBHOOK_REQUIRE_HTTPS", "webhook-require-https", "false", "refuse plain-http webhook URLs"},
	{"HMNPAY_RPC_DENY_PRIVATE", "rpc-deny-private", "false", "refuse to probe RPC endpoints on loopback/private networks (setup wizard)"},
	{"HMNPAY_CHAIN_VERIFY", "chain-verify", "auto", "cross-check mode of the chain watcher: auto, best-effort or strict"},
	{"HMNPAY_WEB_UPSTREAM", "web-upstream", "", "DEVELOPMENT ONLY: origin of a web UI dev server (\"pnpm dev\", http://localhost:3000) to reverse-proxy non-API paths to instead of the interface built into the binary; must be a loopback address unless the next setting allows otherwise; never use in production"},
	{"HMNPAY_WEB_UPSTREAM_ALLOW_REMOTE", "web-upstream-allow-remote", "false", "allow HMNPAY_WEB_UPSTREAM to be a host other than this machine (the upstream then serves every page of this origin; development only)"},
	{"HMNPAY_SHUTDOWN_TIMEOUT", "shutdown-timeout", "30s", "how long a graceful shutdown may take"},
	{"HMNPAY_MAX_HEAD_TIME_AHEAD", "max-head-time-ahead", "2m", "refuse chain heads whose block time is this far ahead of the server clock; \"off\" disables (escape hatch for hosts without time sync)"},
	{"HMNPAY_POLL_INTERVAL", "poll-interval", "3s", "pause between chain scans while a payment is open or a transfer is not final yet, 1s to 30s; a payment is detected half of it later on average, and every scan costs RPC calls"},
	{"HMNPAY_IDLE_POLL_INTERVAL", "idle-poll-interval", "30s", "pause between chain scans while no payment is open: nothing is watched, or only the addresses of closed payments (for late funds, which are then noticed up to this much later); from the poll interval to 5m; creating a payment ends the pause at once; \"off\" or the poll interval disables it"},
}

// parseServeConfig reads the environment, then flags, and validates.
func parseServeConfig(args []string, getenv func(string) string, stderr io.Writer) (serverConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	values := make(map[string]*string, len(settings))
	for _, s := range settings {
		def := s.def
		if v, ok := lookup(getenv, s.env); ok {
			def = v
		}
		values[s.env] = fs.String(s.flag, def, s.help+" ($"+s.env+")")
	}
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: hmnpay serve [flags]\n\nEvery flag can also be set with the environment variable shown; flags win.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return serverConfig{}, err
	}
	if fs.NArg() > 0 {
		return serverConfig{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	get := func(env string) string { return strings.TrimSpace(*values[env]) }
	var c serverConfig
	var errs []string
	boolean := func(env string) bool {
		v := get(env)
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %q is not a boolean", env, v))
		}
		return b
	}
	c.Listen = get("HMNPAY_LISTEN")
	c.DataDir = get("HMNPAY_DATA_DIR")
	c.PublicURL = strings.TrimRight(get("HMNPAY_PUBLIC_URL"), "/")
	c.TrustedProxies = list(get("HMNPAY_TRUSTED_PROXIES"))
	c.TrustedOrigins = list(get("HMNPAY_TRUSTED_ORIGINS"))
	c.CORSOrigins = list(get("HMNPAY_CORS_ORIGINS"))
	c.PublicCORS = boolean("HMNPAY_PUBLIC_CORS")
	c.LogLevel = strings.ToLower(get("HMNPAY_LOG_LEVEL"))
	c.LogFormat = strings.ToLower(get("HMNPAY_LOG_FORMAT"))
	c.WebhookPrivate = boolean("HMNPAY_WEBHOOK_ALLOW_PRIVATE")
	c.WebhookPrefixes = list(get("HMNPAY_WEBHOOK_ALLOWED_PREFIXES"))
	c.WebhookHTTPS = boolean("HMNPAY_WEBHOOK_REQUIRE_HTTPS")
	c.RPCDenyPrivate = boolean("HMNPAY_RPC_DENY_PRIVATE")
	c.ChainVerify = get("HMNPAY_CHAIN_VERIFY")
	c.WebUpstream = get("HMNPAY_WEB_UPSTREAM")
	c.WebUpstreamRemote = boolean("HMNPAY_WEB_UPSTREAM_ALLOW_REMOTE")
	if d, err := time.ParseDuration(get("HMNPAY_SHUTDOWN_TIMEOUT")); err != nil || d <= 0 {
		errs = append(errs, fmt.Sprintf("HMNPAY_SHUTDOWN_TIMEOUT: %q is not a positive duration", get("HMNPAY_SHUTDOWN_TIMEOUT")))
	} else {
		c.ShutdownTimeout = d
	}

	c.SetupToken = get("HMNPAY_SETUP_TOKEN")
	if c.SetupToken != "" {
		if err := hmnpay.ValidateSetupToken(c.SetupToken); err != nil {
			errs = append(errs, "HMNPAY_SETUP_TOKEN: "+err.Error())
		}
	}
	c.AllowedHosts = list(get("HMNPAY_ALLOWED_HOSTS"))
	for _, h := range c.AllowedHosts {
		if strings.ContainsAny(h, "/:@ ") && h != "*" {
			errs = append(errs, fmt.Sprintf("HMNPAY_ALLOWED_HOSTS: %q must be a bare host name (no scheme, port or path)", h))
		}
	}
	switch v := strings.ToLower(get("HMNPAY_MAX_HEAD_TIME_AHEAD")); v {
	case "off", "disabled", "none":
		c.MaxHeadTimeAhead = -1
	default:
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf("HMNPAY_MAX_HEAD_TIME_AHEAD: %q must be a positive duration or \"off\"", v))
		}
		c.MaxHeadTimeAhead = d
	}

	if d, err := time.ParseDuration(get("HMNPAY_POLL_INTERVAL")); err != nil || d < minPollInterval || d > maxPollInterval {
		errs = append(errs, fmt.Sprintf("HMNPAY_POLL_INTERVAL: %q must be a duration from %s to %s", get("HMNPAY_POLL_INTERVAL"), minPollInterval, maxPollInterval))
	} else {
		c.PollInterval = d
	}
	switch v := strings.ToLower(get("HMNPAY_IDLE_POLL_INTERVAL")); v {
	case "off", "disabled", "none":
		c.IdlePollInterval = -1
	default:
		d, err := time.ParseDuration(v)
		switch {
		case err != nil || d <= 0 || d > maxIdlePollInterval:
			errs = append(errs, fmt.Sprintf("HMNPAY_IDLE_POLL_INTERVAL: %q must be a duration up to %s, or \"off\"", v, maxIdlePollInterval))
		case c.PollInterval != 0 && d < c.PollInterval:
			errs = append(errs, fmt.Sprintf("HMNPAY_IDLE_POLL_INTERVAL: %s is shorter than HMNPAY_POLL_INTERVAL (%s); an idle watcher does not scan more often than a busy one (the same value, or \"off\", disables idle polling)", d, c.PollInterval))
		case d == c.PollInterval:
			c.IdlePollInterval = -1 // the same pause either way: off
		default:
			c.IdlePollInterval = d
		}
	}

	if c.Listen == "" {
		errs = append(errs, "HMNPAY_LISTEN must not be empty")
	}
	if c.DataDir == "" {
		errs = append(errs, "HMNPAY_DATA_DIR must not be empty")
	}
	if _, err := parseLevel(c.LogLevel); err != nil {
		errs = append(errs, "HMNPAY_LOG_LEVEL: "+err.Error())
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		errs = append(errs, fmt.Sprintf("HMNPAY_LOG_FORMAT: %q must be text or json", c.LogFormat))
	}
	if _, err := hmnpay.ParseVerifyMode(c.ChainVerify); err != nil {
		errs = append(errs, "HMNPAY_CHAIN_VERIFY: "+err.Error())
	}
	if _, err := hmnpay.ParsePrefixes(c.TrustedProxies); err != nil {
		errs = append(errs, "HMNPAY_TRUSTED_PROXIES: "+err.Error())
	}
	if _, err := hmnpay.ParsePrefixes(c.WebhookPrefixes); err != nil {
		errs = append(errs, "HMNPAY_WEBHOOK_ALLOWED_PREFIXES: "+err.Error())
	}
	if c.WebUpstream != "" {
		if _, err := parseUpstream(c.WebUpstream, c.WebUpstreamRemote); err != nil {
			errs = append(errs, "HMNPAY_WEB_UPSTREAM: "+err.Error())
		}
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("invalid configuration:\n  %s", strings.Join(errs, "\n  "))
	}
	return c, nil
}

// lookup returns an environment variable that is set and not blank.
func lookup(getenv func(string) string, key string) (string, bool) {
	v := strings.TrimSpace(getenv(key))
	return v, v != ""
}

// list splits a comma-separated list, dropping blanks.
func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%q must be debug, info, warn or error", s)
}

func newLogger(w io.Writer, level, format string) *slog.Logger {
	lvl, _ := parseLevel(level)
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// redactURL keeps scheme, host and path of a URL and drops credentials,
// query and fragment (they may carry tokens).
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(invalid URL)"
	}
	out := u.Scheme + "://" + u.Host + u.EscapedPath()
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		out += " (credentials/query redacted)"
	}
	return out
}

// logEffective prints the effective configuration. Nothing here is a
// secret, but URLs are redacted anyway in case one carries a token.
func logEffective(log *slog.Logger, c serverConfig) {
	none := func(ss []string) string {
		if len(ss) == 0 {
			return "(none)"
		}
		return strings.Join(ss, ",")
	}
	web := "status page only (this binary was built without the web interface)"
	switch {
	case c.WebUpstream != "":
		web = "DEVELOPMENT proxy to " + redactURL(c.WebUpstream)
	case uiBuilt(embeddedUI()):
		web = "built in"
	}
	publicURL := redactURL(c.PublicURL)
	if publicURL == "" {
		publicURL = "(unset: derived per request)"
	}
	log.Info("effective configuration",
		"listen", c.Listen,
		"data_dir", c.DataDir,
		"public_url", publicURL,
		"trusted_proxies", none(c.TrustedProxies),
		"trusted_origins", none(c.TrustedOrigins),
		"cors_origins", none(c.CORSOrigins),
		"public_cors", c.PublicCORS,
		"log_level", c.LogLevel,
		"log_format", c.LogFormat,
		"webhook_allow_private", c.WebhookPrivate,
		"webhook_allowed_prefixes", none(c.WebhookPrefixes),
		"webhook_require_https", c.WebhookHTTPS,
		"rpc_deny_private", c.RPCDenyPrivate,
		"chain_verify", c.ChainVerify,
		"web", web,
		"shutdown_timeout", c.ShutdownTimeout,
		"allowed_hosts", none(c.AllowedHosts),
		"setup_token", tokenSource(c.SetupToken),
		"max_head_time_ahead", headAhead(c.MaxHeadTimeAhead),
		"poll_interval", c.PollInterval,
		"idle_poll_interval", headAhead(c.IdlePollInterval),
	)
}

// defaultDataDir is HMNPAY_DATA_DIR or the default, for the operator tools.
func defaultDataDir() string {
	if v, ok := lookup(os.Getenv, "HMNPAY_DATA_DIR"); ok {
		return v
	}
	return "./data"
}

func tokenSource(t string) string {
	if t == "" {
		return "generated at start (printed to stderr while setup is pending)"
	}
	return "from HMNPAY_SETUP_TOKEN (redacted)"
}

func headAhead(d time.Duration) string {
	if d < 0 {
		return "off"
	}
	return d.String()
}

// loopbackListen reports whether a listen address only accepts local
// connections.
func loopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && ip.IsLoopback()
}
