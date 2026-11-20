package hmnpay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

func quietConfig(t *testing.T) Config {
	return Config{
		DataDir:        t.TempDir(),
		Logger:         slog.New(slog.DiscardHandler),
		Chain:          ChainConfig{Disabled: true},
		Webhooks:       WebhookConfig{Disabled: true},
		Prune:          PruneConfig{Disabled: true},
		PasswordHasher: core.PBKDF2Hasher{Iterations: 1000},
	}
}

func openQuiet(t *testing.T, mutate ...func(*Config)) *Engine {
	t.Helper()
	cfg := quietConfig(t)
	for _, m := range mutate {
		m(&cfg)
	}
	e, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func TestConfigValidation(t *testing.T) {
	bad := map[string]func(*Config){
		"no data dir":       func(c *Config) { c.DataDir = " " },
		"public url path":   func(c *Config) { c.PublicURL = "https://pay.example.com/x" },
		"public url scheme": func(c *Config) { c.PublicURL = "ftp://pay.example.com" },
		"public url creds":  func(c *Config) { c.PublicURL = "https://u:p@pay.example.com" },
		"negative timeout":  func(c *Config) { c.ShutdownTimeout = -time.Second },
		"negative poll":     func(c *Config) { c.Chain.PollInterval = -1 },
		"verify mode":       func(c *Config) { c.Chain.Verify = 7 },
		"settle depth":      func(c *Config) { c.Chain.SettleDepth = 5000 },
		"bad prefix":        func(c *Config) { c.Webhooks.AllowedPrefixes = []netip.Prefix{{}} },
		"bad port":          func(c *Config) { c.Webhooks.AllowedPorts = []int{70000} },
		"negative workers":  func(c *Config) { c.Webhooks.Concurrency = -1 },
		"negative prune":    func(c *Config) { c.Prune.Interval = -time.Minute },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			cfg := quietConfig(t)
			mutate(&cfg)
			if _, err := Open(t.Context(), cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Open = %v; want ErrInvalidConfig", err)
			}
		})
	}
	good := quietConfig(t)
	good.PublicURL = "https://pay.example.com/"
	e, err := Open(t.Context(), good)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(e.DatabasePath(), DatabaseFile) {
		t.Fatalf("database path %s", e.DatabasePath())
	}
	_ = e.Close()
}

func TestParseVerifyMode(t *testing.T) {
	for in, want := range map[string]VerifyMode{"": VerifyAuto, "auto": VerifyAuto, "best-effort": VerifyBestEffort, "STRICT": VerifyStrict} {
		if got, err := ParseVerifyMode(in); err != nil || got != want {
			t.Errorf("ParseVerifyMode(%q) = %v, %v", in, got, err)
		}
		if m, _ := ParseVerifyMode(want.String()); m != want {
			t.Errorf("%v does not round-trip", want)
		}
	}
	if _, err := ParseVerifyMode("paranoid"); err == nil {
		t.Error("unknown mode accepted")
	}
}

func runAsync(e *Engine, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	return done
}

func waitErr(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	return nil
}

func steady(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

// A worker that returns, without an error, because the engine is being shut
// down — as the prune loop does — is part of a clean shutdown, however its
// result and the cancellation race: Run used to call it "stopped
// unexpectedly" when the select took the result first (TestRunCleanShutdown
// failed so on Linux). Many shutdowns in a row, so that both orders occur.
func TestShutdownIsNotAFailureWhenAWorkerStopsWithIt(t *testing.T) {
	for i := range 200 {
		e := openQuiet(t)
		// Many of them, so that some have returned while Run is still starting the others.
		for w := range 50 {
			e.extra = append(e.extra, worker{fmt.Sprintf("quick %d", w), func(ctx context.Context) error { <-ctx.Done(); return nil }})
		}
		ctx, cancel := context.WithCancel(t.Context())
		if i%2 == 0 {
			cancel() // asked to stop before it has started: both cases of the race are ready at once
		}
		done := runAsync(e, ctx)
		cancel()
		if err := waitErr(t, done); err != nil {
			t.Fatalf("shutdown %d returned %v", i+1, err)
		}
	}
	// A worker that stops by itself, without a shutdown, is still a failure.
	e := openQuiet(t)
	e.extra = []worker{{"quitter", func(context.Context) error { return nil }}}
	if err := waitErr(t, runAsync(e, t.Context())); err == nil || !strings.Contains(err.Error(), "quitter stopped unexpectedly") {
		t.Fatalf("a worker that returned by itself: %v", err)
	}
}

func TestRunCleanShutdown(t *testing.T) {
	e := openQuiet(t, func(c *Config) { c.Prune.Disabled = false })
	stopped := make(chan struct{})
	e.extra = []worker{{"steady", func(ctx context.Context) error { defer close(stopped); return steady(ctx) }}}
	ctx, cancel := context.WithCancel(t.Context())
	done := runAsync(e, ctx)
	cancel()
	if err := waitErr(t, done); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
	<-stopped
}

func TestRunSurfacesWorkerFailure(t *testing.T) {
	for name, w := range map[string]func(context.Context) error{
		"error": func(context.Context) error { return errors.New("boom") },
		"nil":   func(context.Context) error { return nil },
		"panic": func(context.Context) error { panic("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			e := openQuiet(t)
			otherStopped := make(chan struct{})
			e.extra = []worker{
				{"other", func(ctx context.Context) error { defer close(otherStopped); return steady(ctx) }},
				{"flaky", w},
			}
			err := waitErr(t, runAsync(e, t.Context()))
			if err == nil || !strings.Contains(err.Error(), "flaky stopped unexpectedly") {
				t.Fatalf("Run = %v; want the failure of flaky", err)
			}
			select {
			case <-otherStopped:
			default:
				t.Fatal("the other workers were not stopped")
			}
		})
	}
}

func TestRunBoundedDrain(t *testing.T) {
	e := openQuiet(t, func(c *Config) { c.ShutdownTimeout = 50 * time.Millisecond })
	release := make(chan struct{})
	defer close(release)
	e.extra = []worker{{"stuck", func(context.Context) error { <-release; return nil }}}
	ctx, cancel := context.WithCancel(t.Context())
	done := runAsync(e, ctx)
	cancel()
	err := waitErr(t, done)
	if err == nil || !strings.Contains(err.Error(), "did not stop within") {
		t.Fatalf("Run = %v; want a drain timeout", err)
	}
}

func TestLifecycleMisuse(t *testing.T) {
	e := openQuiet(t)
	ctx, cancel := context.WithCancel(t.Context())
	e.extra = []worker{{"steady", steady}}
	done := runAsync(e, ctx)
	// Wait until running.
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.mu.Lock()
		r := e.running
		e.mu.Unlock()
		if r || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := e.Run(ctx); err == nil {
		t.Error("a second concurrent Run was accepted")
	}
	if err := e.Close(); err == nil {
		t.Error("Close while running was accepted")
	}
	cancel()
	if err := waitErr(t, done); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HTTPHandler(HTTPOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if err := e.Run(t.Context()); err == nil {
		t.Error("Run after Close was accepted")
	}
	if _, err := e.HTTPHandler(HTTPOptions{}); err == nil {
		t.Error("HTTPHandler after Close was accepted")
	}
}

// The webhook URL policy of Config reaches the core: every transport
// rejects a private destination when the endpoint is saved.
func TestWebhookPolicyWiredIntoCore(t *testing.T) {
	ctx := context.Background()
	e := openQuiet(t)
	svc := e.Service()
	if _, err := svc.CompleteSetup(core.WithActor(ctx, core.SystemActor("test")), core.SetupInput{
		Email: "owner@example.com", Password: "correct horse battery staple",
		RPCURLs:  []string{"https://bsc-rpc.publicnode.com"},
		XPub:     "xpub6Ce9NcJvTk36xtLSrJLZqE7wtgA5deCeYs7rSQtreh4cj6ByPtrg9sD7V2FNFLPnf8heNP3FGkeV9qwfzvZNSd54JoNXVsXFYSYwHsnJxqP",
		Merchant: core.MerchantSettings{Name: "N", SupportEmail: "s@example.com"}, PublicURL: "https://pay.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	sys := core.WithActor(ctx, core.SystemActor("test"))
	for _, u := range []string{"http://127.0.0.1:9000/hook", "http://10.0.0.5/hook", "http://169.254.169.254/latest"} {
		_, err := svc.CreateWebhookEndpoint(sys, core.WebhookEndpointInput{URL: u, Events: core.WebhookEventTypes, Enabled: true})
		if fe := core.FieldErrors(err); len(fe) == 0 || fe[0].Code != "not_allowed" {
			t.Errorf("%s: %v; want not_allowed", u, err)
		}
	}
	if _, err := svc.CreateWebhookEndpoint(sys, core.WebhookEndpointInput{URL: "https://shop.example.com/hook", Events: core.WebhookEventTypes, Enabled: true}); err != nil {
		t.Errorf("public URL refused: %v", err)
	}
}

func TestSetupTokenModes(t *testing.T) {
	ctx := context.Background()
	in := core.SetupInput{
		Email: "owner@example.com", Password: "correct horse battery staple",
		RPCURLs:  []string{"https://bsc-rpc.publicnode.com"},
		XPub:     "xpub6Ce9NcJvTk36xtLSrJLZqE7wtgA5deCeYs7rSQtreh4cj6ByPtrg9sD7V2FNFLPnf8heNP3FGkeV9qwfzvZNSd54JoNXVsXFYSYwHsnJxqP",
		Merchant: core.MerchantSettings{Name: "N", SupportEmail: "s@example.com"}, PublicURL: "https://pay.example.com",
	}
	t.Run("generated by default", func(t *testing.T) {
		e := openQuiet(t)
		tok := e.SetupToken()
		if ValidateSetupToken(tok) != nil || len(tok) != 32 {
			t.Fatalf("generated token %q", tok)
		}
		if again := openQuiet(t); again.SetupToken() == tok {
			t.Fatal("two engines generated the same token")
		}
		if _, err := e.Service().CompleteSetup(ctx, in); !errors.Is(err, core.ErrSetupTokenRequired) {
			t.Fatalf("anonymous setup without token = %v", err)
		}
		bad := in
		bad.SetupToken = strings.Repeat("x", 31) + "y"
		if _, err := e.Service().CompleteSetup(ctx, bad); !errors.Is(err, core.ErrSetupTokenInvalid) {
			t.Fatalf("wrong token = %v", err)
		}
		good := in
		good.SetupToken = tok
		if _, err := e.Service().CompleteSetup(ctx, good); err != nil {
			t.Fatal(err)
		}
		if pending, err := e.SetupPending(ctx); err != nil || pending {
			t.Fatalf("pending after setup = %v, %v", pending, err)
		}
	})
	t.Run("explicit", func(t *testing.T) {
		e := openQuiet(t, func(c *Config) { c.SetupToken = "k3J9-qZ7x-Lm2P-vR8t-Yw4N" })
		if e.SetupToken() != "k3J9-qZ7x-Lm2P-vR8t-Yw4N" {
			t.Fatal("explicit token not used")
		}
	})
	t.Run("opt out", func(t *testing.T) {
		e := openQuiet(t, func(c *Config) { c.NoSetupToken = true })
		if e.SetupToken() != "" {
			t.Fatal("token with NoSetupToken")
		}
		if _, err := e.Service().CompleteSetup(ctx, in); err != nil {
			t.Fatalf("setup without token on an opted-out engine: %v", err)
		}
	})
	for name, tok := range map[string]string{"short": "abc123", "repetitive": strings.Repeat("ab", 20), "space": "abcdefghij klmnopqrstuvwxyz"} {
		t.Run("weak "+name, func(t *testing.T) {
			cfg := quietConfig(t)
			cfg.SetupToken = tok
			if _, err := Open(ctx, cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Open = %v", err)
			}
		})
	}
	cfg := quietConfig(t)
	cfg.SetupToken, cfg.NoSetupToken = "k3J9-qZ7x-Lm2P-vR8t-Yw4N", true
	if _, err := Open(ctx, cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("SetupToken with NoSetupToken accepted")
	}
}

// The device key is created once, persists across Open, and rotates.
func TestDeviceKeyPersistence(t *testing.T) {
	ctx := context.Background()
	cfg := quietConfig(t)
	a, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := a.deviceKey
	if len(key) != 32 {
		t.Fatalf("device key length %d", len(key))
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(b.deviceKey) != string(key) {
		t.Fatal("device key changed across restarts")
	}
	if err := b.RotateDeviceKey(ctx); err != nil {
		t.Fatal(err)
	}
	b.Close()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if string(c.deviceKey) == string(key) || len(c.deviceKey) != 32 {
		t.Fatal("device key not rotated")
	}
}
