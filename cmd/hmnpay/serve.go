package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lubluniky/hmnpay"
)

// serve runs the server until SIGINT/SIGTERM, then shuts down in order:
//
//  1. stop accepting connections and finish in-flight requests
//     (http.Server.Shutdown), which also ends open checkout event streams;
//  2. stop the workers and drain them (chain watcher, webhook dispatcher
//     with its in-flight attempts, clean-up);
//  3. close the database.
//
// A second signal during shutdown exits at once with code 3.
func serve(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseServeConfig(args, os.Getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "hmnpay: %v\n", err)
		return exitUsage
	}
	log := newLogger(stderr, cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(log)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	return serveWith(cfg, log, stderr, sigs)
}

// serveWith is serve after configuration, with the signal channel injected
// (tests send on it). console receives the human-readable first-run
// banner (stderr in production), never the structured log.
func serveWith(cfg serverConfig, log *slog.Logger, console io.Writer, sigs <-chan os.Signal) int {
	log.Info("starting hmnpay", "version", hmnpay.Version, "commit", hmnpay.Commit)
	logEffective(log, cfg)
	if cfg.PublicURL == "" && !loopbackListen(cfg.Listen) {
		log.Warn("HMNPAY_PUBLIC_URL is not set while listening on a non-loopback address: session cookies are not marked Secure " +
			"unless the request arrived over TLS, and the API only answers for localhost and IP-address Host headers " +
			"(plus HMNPAY_ALLOWED_HOSTS). Behind a TLS reverse proxy set HMNPAY_PUBLIC_URL and HMNPAY_TRUSTED_PROXIES.")
	}

	release, err := lockDataDir(cfg.DataDir)
	if err != nil {
		log.Error("cannot lock the data directory", "data_dir", cfg.DataDir, "error", err)
		if errors.Is(err, errLocked) {
			fmt.Fprintf(console, "hmnpay: %v\nStop the other server first. (reset-password, recovery-code and inspect work while it runs.)\n", err)
		}
		return exitFail
	}
	defer release()
	if !lockSupported {
		log.Warn("this platform has no flock: nothing prevents a second server on the same data directory")
	}

	verify, _ := hmnpay.ParseVerifyMode(cfg.ChainVerify)
	webhookPrefixes, _ := hmnpay.ParsePrefixes(cfg.WebhookPrefixes)
	proxies, _ := hmnpay.ParsePrefixes(cfg.TrustedProxies)

	openCtx, cancelOpen := context.WithTimeout(context.Background(), time.Minute)
	eng, err := hmnpay.Open(openCtx, hmnpay.Config{
		DataDir:   cfg.DataDir,
		Logger:    log,
		PublicURL: cfg.PublicURL,
		Webhooks: hmnpay.WebhookConfig{
			AllowPrivateNetworks: cfg.WebhookPrivate,
			AllowedPrefixes:      webhookPrefixes,
			RequireHTTPS:         cfg.WebhookHTTPS,
		},
		Chain:           hmnpay.ChainConfig{Verify: verify, PollInterval: cfg.PollInterval, IdlePollInterval: cfg.IdlePollInterval},
		Prober:          hmnpay.ProberConfig{DenyPrivateNetworks: cfg.RPCDenyPrivate},
		ShutdownTimeout: cfg.ShutdownTimeout,
		SetupToken:      cfg.SetupToken, // empty: generated
		Policy:          hmnpay.PolicyConfig{MaxHeadTimeAhead: cfg.MaxHeadTimeAhead},
	})
	cancelOpen()
	if err != nil {
		log.Error("cannot open the engine", "error", err)
		return exitUsageOrFail(err)
	}
	closeEngine := func() int {
		if err := eng.Close(); err != nil {
			log.Error("closing the database failed", "error", err)
			return exitFail
		}
		return exitOK
	}
	log.Info("database opened", "path", eng.DatabasePath())

	fallback, mode, err := webHandler(cfg.WebUpstream, cfg.WebUpstreamRemote, eng.Service(), log)
	if err != nil {
		log.Error("web UI configuration", "error", err)
		closeEngine()
		return exitUsage
	}
	switch mode {
	case webEmbedded:
		log.Info("web interface: built in")
	case webProxy:
		log.Warn("HMNPAY_WEB_UPSTREAM is set: this is a development mode and must not be used in production. "+
			"Every page of this origin — dashboard, setup, checkout — is served by the upstream, not by the interface built into this binary, "+
			"so the upstream is trusted completely: whatever script it serves runs with the operator's session. "+
			"It is not sent cookies or API credentials, cannot set cookies, and its pages get a relaxed development Content-Security-Policy (inline scripts and eval are allowed). "+
			"Unset HMNPAY_WEB_UPSTREAM to serve the built-in interface.",
			"upstream", redactURL(cfg.WebUpstream))
	default:
		log.Warn("this binary was built without the web interface: only the API and a status page are served. " +
			"Build with \"make build\" (needs Node.js and pnpm) or use the Docker image to get the dashboard, setup wizard and checkout.")
	}
	h, err := eng.HTTPHandler(hmnpay.HTTPOptions{
		AllowedHosts:   cfg.AllowedHosts,
		TrustedProxies: proxies,
		TrustedOrigins: cfg.TrustedOrigins,
		CORSOrigins:    cfg.CORSOrigins,
		PublicCORS:     cfg.PublicCORS,
		Fallback:       fallback,
	})
	if err != nil {
		log.Error("HTTP configuration", "error", err)
		closeEngine()
		return exitUsage
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("cannot listen", "address", cfg.Listen, "error", err)
		closeEngine()
		return exitFail
	}
	srv := h.NewServer(cfg.Listen)

	runCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	runDone := make(chan error, 1)
	go func() { runDone <- eng.Run(runCtx) }()
	// The first-run banner is printed before the server answers anything: whoever sees the
	// instance up (a health check, a log follower, a person opening the page) can find the
	// setup token already. The listener is bound, so the banner's URL is the real one.
	pending, perr := eng.SetupPending(context.Background())
	if perr == nil && pending {
		printSetupBanner(console, setupURL(cfg, ln.Addr()), eng.SetupToken(), cfg.SetupToken != "")
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ln) }()
	log.Info("listening", "address", ln.Addr().String())
	if perr == nil && pending {
		log.Warn("first-run setup is pending: the setup token is required to claim this instance; see the banner on stderr (the token is never written to the structured log)")
	}

	code := exitOK
	runFinished := false
	select {
	case sig := <-sigs:
		log.Info("shutting down", "signal", sig.String())
	case err := <-serveDone:
		log.Error("HTTP server stopped", "error", err)
		code = exitFail
	case err := <-runDone:
		runFinished = true
		log.Error("engine stopped", "error", err)
		code = exitFail
	}

	// Ordered, bounded shutdown; a second signal forces the exit.
	forced := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		select {
		case sig := <-sigs:
			log.Warn("second signal; exiting immediately", "signal", sig.String())
			close(forced)
		case <-finished:
		}
	}()
	result := make(chan int, 1)
	go func() {
		defer close(finished)
		c := code
		httpCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		if err := srv.Shutdown(httpCtx); err != nil {
			log.Warn("HTTP shutdown incomplete; closing connections", "error", err)
			_ = srv.Close()
		}
		cancel()
		log.Info("HTTP server stopped")
		stopWorkers()
		if !runFinished {
			if err := <-runDone; err != nil {
				log.Error("workers stopped with an error", "error", err)
				c = exitFail
			}
		}
		if cc := closeEngine(); cc != exitOK {
			c = cc
		}
		result <- c
	}()
	// The engine bounds its own drain by ShutdownTimeout; allow both
	// phases plus a margin before giving up.
	deadline := time.NewTimer(2*cfg.ShutdownTimeout + 5*time.Second)
	defer deadline.Stop()
	select {
	case c := <-result:
		log.Info("hmnpay stopped", "exit_code", c)
		return c
	case <-forced:
		return exitForced
	case <-deadline.C:
		log.Error("shutdown did not finish in time; exiting")
		return exitForced
	}
}

// printSetupBanner writes the first-run instructions as plain text, the
// same in text and JSON log mode: it must be easy to spot in
// "docker compose logs", and the token must never enter structured logs
// that may be shipped elsewhere. A token the operator configured is not
// echoed back.
func printSetupBanner(w io.Writer, url, token string, configured bool) {
	line := strings.Repeat("=", 72)
	fmt.Fprintf(w, "\n%s\n  hmnpay: FIRST-RUN SETUP REQUIRED\n\n  This instance has not been set up yet. To claim it, open\n\n      %s\n\n", line, url)
	if configured {
		fmt.Fprint(w, "  and enter the setup token you configured in HMNPAY_SETUP_TOKEN.\n")
	} else {
		fmt.Fprintf(w, "  and enter this setup token:\n\n      %s\n\n  The token is new on every start until setup is completed.\n", token)
	}
	fmt.Fprintf(w, "  Nobody can complete setup without it.\n%s\n\n", line)
}

// setupURL is where the person installing finds the wizard.
func setupURL(cfg serverConfig, addr net.Addr) string {
	if cfg.PublicURL != "" {
		return cfg.PublicURL + "/setup"
	}
	host := "localhost"
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		port = "8080"
	}
	if h, _, err := net.SplitHostPort(cfg.Listen); err == nil && h != "" && h != "0.0.0.0" && h != "::" {
		host = h
	}
	return "http://" + net.JoinHostPort(host, port) + "/setup"
}

// exitUsageOrFail maps an Open error: an invalid configuration is a usage
// error, everything else (permissions, a corrupt database) a failure.
func exitUsageOrFail(err error) int {
	if errors.Is(err, hmnpay.ErrInvalidConfig) {
		return exitUsage
	}
	return exitFail
}
