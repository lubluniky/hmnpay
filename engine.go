package hmnpay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/corebridge"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/watcher"
	"github.com/lubluniky/hmnpay/internal/httpapi"
	"github.com/lubluniky/hmnpay/internal/store/sqlite"
	"github.com/lubluniky/hmnpay/internal/wallet"
	"github.com/lubluniky/hmnpay/internal/webhook"
)

var _ watcher.Store = (*sqlite.Checkpoints)(nil)

// pruneActor is the system actor of the periodic clean-up.
const pruneActor = "prune"

// Engine is one hmnpay instance: the core service on its SQLite store,
// wired to the real adapters, plus the background workers that make
// payments happen (chain watcher, webhook dispatcher, clean-up).
//
// Lifecycle: [Open] → [Engine.Run] (blocks until its context ends) →
// [Engine.Close]. [Engine.Service] may be used at any time between Open and
// Close, whether or not Run is active.
type Engine struct {
	cfg   Config
	log   *slog.Logger
	path  string
	store *sqlite.Store
	svc   *core.Service

	setupToken string // "" with NoSetupToken
	deviceKey  []byte

	bridge     *corebridge.Bridge  // nil when the chain watcher is disabled
	dispatcher *webhook.Dispatcher // nil when webhooks are disabled

	extra []worker // tests only: additional workers

	mu       sync.Mutex
	running  bool
	closed   bool
	handlers []*httpapi.Handler
}

// Open opens (creating if needed) the instance in cfg.DataDir and wires the
// core to its adapters. It starts no goroutines and makes no network
// request; call [Engine.Run] to start the workers.
//
// Several Engines may open the same data directory (SQLite serializes
// writers across processes; the command-line tools rely on this), but only
// one of them should Run its workers.
func Open(ctx context.Context, cfg Config) (*Engine, error) {
	cfg, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("hmnpay: data directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("hmnpay: create data directory: %w", err)
	}
	path := filepath.Join(dir, DatabaseFile)
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("hmnpay: open database %s: %w", path, err)
	}
	e := &Engine{cfg: cfg, log: cfg.Logger, path: path, store: store}
	switch {
	case cfg.NoSetupToken:
	case cfg.SetupToken != "":
		e.setupToken = cfg.SetupToken
	default:
		if e.setupToken, err = generateSetupToken(); err != nil {
			store.Close()
			return nil, err
		}
	}
	if e.deviceKey, err = loadDeviceKey(ctx, store); err != nil {
		store.Close()
		return nil, fmt.Errorf("hmnpay: device key: %w", err)
	}
	if err := e.wire(); err != nil {
		store.Close()
		return nil, err
	}
	return e, nil
}

func (e *Engine) wire() error {
	cfg := e.cfg
	policy := webhook.EgressPolicy{
		AllowPrivateNetworks: cfg.Webhooks.AllowPrivateNetworks,
		AllowedPrefixes:      cfg.Webhooks.AllowedPrefixes,
		RequireHTTPS:         cfg.Webhooks.RequireHTTPS,
		AllowedPorts:         cfg.Webhooks.AllowedPorts,
	}
	coreCfg := core.Config{
		Store:              e.store,
		Addresses:          wallet.NewAddressSource(),
		Hasher:             cfg.PasswordHasher,
		Clock:              cfg.Clock,
		Rand:               cfg.Rand,
		ValidateWebhookURL: policy.ValidateURL,
		SetupToken:         e.setupToken,

		ExpiryGrace:         cfg.Policy.ExpiryGrace,
		WatchWindow:         cfg.Policy.WatchWindow,
		UnfundedWatchWindow: cfg.Policy.UnfundedWatchWindow,
		MaxWatchedAddresses: cfg.Policy.MaxWatchedAddresses,
		MaxHeadTimeAhead:    cfg.Policy.MaxHeadTimeAhead,
		LinkReservation:     cfg.Policy.LinkReservation,
		SessionTTL:          cfg.Policy.SessionTTL,
		SessionIdleTimeout:  cfg.Policy.SessionIdleTimeout,
		MaxConcurrentKDF:    cfg.Policy.MaxConcurrentKDF,
	}
	if !cfg.Prober.Disabled {
		coreCfg.Prober = corebridge.NewProber(corebridge.ProberOptions{DenyPrivateNetworks: cfg.Prober.DenyPrivateNetworks})
	}
	svc, err := core.New(coreCfg)
	if err != nil {
		return fmt.Errorf("hmnpay: %w", err)
	}
	e.svc = svc

	if !cfg.Webhooks.Disabled {
		e.dispatcher, err = webhook.New(svc, webhook.Options{
			Concurrency:   cfg.Webhooks.Concurrency,
			PollInterval:  cfg.Webhooks.PollInterval,
			ShutdownGrace: min(webhook.DefaultShutdownGrace, cfg.ShutdownTimeout/2),
			Logger:        e.log,
			Egress:        policy,
			UserAgent:     "hmnpay-webhook/" + Version,
		})
		if err != nil {
			return fmt.Errorf("hmnpay: %w", err)
		}
	}
	if !cfg.Chain.Disabled {
		c := cfg.Chain
		// The bridge judges block timestamps against the same clock as the
		// core; with no injected clock it uses the system clock.
		var now func() time.Time
		if cfg.Clock != nil {
			now = cfg.Clock.Now
		}
		e.bridge, err = corebridge.New(svc, e.store.Checkpoints(), corebridge.Options{
			Now: now,
			// Same value as the core's check, so "off" switches off both.
			MaxHeadTimeAhead: cfg.Policy.MaxHeadTimeAhead,
			Logger:           e.log.With("component", "chain"),
			PollInterval:     c.PollInterval,
			IdlePollInterval: c.IdlePollInterval,
			SettleDepth:      c.SettleDepth,
			Verify:           c.Verify.watcher(),
			StatusInterval:   c.StatusInterval,
			ReloadInterval:   c.ReloadInterval,
			MaxBackoff:       c.MaxBackoff,
			Endpoint:         evm.EndpointOptions{Timeout: c.RequestTimeout},
			OnAlert:          c.OnAlert,
		})
		if err != nil {
			return fmt.Errorf("hmnpay: %w", err)
		}
	}
	return nil
}

// Service returns the core: every use case of the system. Authenticate the
// caller into a [core.Actor] (or use [core.SystemActor] for trusted
// in-process code) and attach it with [core.WithActor]; the core authorises.
func (e *Engine) Service() *core.Service { return e.svc }

// SetupToken returns the token the first-run setup requires from anonymous
// callers (see [Config.SetupToken]): the configured one, or the one
// generated by Open. It is "" with [Config.NoSetupToken]. Show it to the
// person installing only while [Engine.SetupPending] is true; it is
// meaningless afterwards.
func (e *Engine) SetupToken() string { return e.setupToken }

// SetupPending reports whether first-run setup is still to be completed.
func (e *Engine) SetupPending(ctx context.Context) (bool, error) {
	st, err := e.svc.SetupStatus(ctx)
	return !st.Completed, err
}

// DatabasePath returns the absolute path of the SQLite database.
func (e *Engine) DatabasePath() string { return e.path }

// worker is one background loop of Run.
type worker struct {
	name string
	run  func(ctx context.Context) error
}

func (e *Engine) workers() []worker {
	var ws []worker
	if e.bridge != nil {
		ws = append(ws, worker{"chain watcher", e.bridge.Run})
	}
	if e.dispatcher != nil {
		ws = append(ws, worker{"webhook dispatcher", e.dispatcher.Run})
	}
	if !e.cfg.Prune.Disabled {
		ws = append(ws, worker{"prune", e.pruneLoop})
	}
	return append(ws, e.extra...)
}

// Run runs the enabled background workers — the chain watcher, the webhook
// dispatcher and the hourly clean-up — until ctx ends, then stops them and
// waits up to [Config.ShutdownTimeout] for them to drain (in-flight webhook
// attempts finish or are recorded as aborted). It returns nil after a clean
// shutdown.
//
// A worker that stops on its own while ctx is still live (an error or a
// panic) is a failure of the instance: Run then stops the others and
// returns an error naming it, so the process supervisor can restart the
// whole thing rather than run half an engine. Run may be called again after
// it returned, but not concurrently, and not after Close.
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	switch {
	case e.closed:
		e.mu.Unlock()
		return errors.New("hmnpay: Run called after Close")
	case e.running:
		e.mu.Unlock()
		return errors.New("hmnpay: engine is already running")
	}
	e.running = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()

	ws := e.workers()
	names := make([]string, len(ws))
	for i, w := range ws {
		names[i] = w.name
	}
	e.log.Info("hmnpay engine started", "version", Version, "workers", names)

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(ws))
	for _, w := range ws {
		go func() {
			defer func() {
				if v := recover(); v != nil {
					e.log.Error("worker panicked", "worker", w.name, "panic", v, "stack", string(debug.Stack()))
					results <- result{w.name, fmt.Errorf("panic: %v", v)}
				}
			}()
			results <- result{w.name, w.run(wctx)}
		}()
	}

	var failure error
	pending := len(ws)
	select {
	case <-ctx.Done():
	case r := <-results:
		pending--
		if ctx.Err() != nil {
			// The shutdown was asked for at the same moment, and this worker
			// returned because of it (select picks either ready case): that is
			// the shutdown, not a failure — as for the workers drained below.
			if r.err != nil && !errors.Is(r.err, context.Canceled) {
				failure = fmt.Errorf("hmnpay: %s: %w", r.name, r.err)
			}
			break
		}
		err := r.err
		if err == nil {
			err = errors.New("returned without error")
		}
		failure = fmt.Errorf("hmnpay: %s stopped unexpectedly: %w", r.name, err)
		e.log.Error("worker stopped unexpectedly; shutting the engine down", "worker", r.name, "error", err)
	}
	cancel()

	deadline := time.NewTimer(e.cfg.ShutdownTimeout)
	defer deadline.Stop()
	for pending > 0 {
		select {
		case r := <-results:
			pending--
			if r.err != nil && !errors.Is(r.err, context.Canceled) && failure == nil {
				failure = fmt.Errorf("hmnpay: %s: %w", r.name, r.err)
			}
		case <-deadline.C:
			e.log.Error("workers did not stop in time", "pending", pending, "timeout", e.cfg.ShutdownTimeout)
			return errors.Join(failure, fmt.Errorf("hmnpay: %d worker(s) did not stop within %s", pending, e.cfg.ShutdownTimeout))
		}
	}
	e.log.Info("hmnpay engine stopped")
	return failure
}

// pruneLoop calls core.Service.Prune at start and then every interval.
// Failures are logged and retried at the next interval: the clean-up is
// housekeeping, not worth stopping the instance for.
func (e *Engine) pruneLoop(ctx context.Context) error {
	sys := core.WithActor(ctx, core.SystemActor(pruneActor))
	t := time.NewTicker(e.cfg.Prune.Interval)
	defer t.Stop()
	for {
		res, err := e.svc.Prune(sys)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			e.log.Warn("prune failed", "error", err)
		case res.Deliveries+res.Sessions > 0:
			e.log.Info("pruned", "deliveries", res.Deliveries, "sessions", res.Sessions)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Close ends the event streams of every HTTP handler made by this engine
// and closes the database. Call it after Run has returned (Close refuses to
// run while Run is active) and after the HTTP servers have shut down.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return errors.New("hmnpay: Close called while Run is active; cancel Run's context and wait for it first")
	}
	if e.closed {
		return nil
	}
	e.closed = true
	for _, h := range e.handlers {
		h.Close()
	}
	e.handlers = nil
	return e.store.Close()
}
