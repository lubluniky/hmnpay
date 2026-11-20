package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/lubluniky/hmnpay"
	"github.com/lubluniky/hmnpay/core"
)

// resetPassword sets a new operator password directly in the data
// directory and ends every session. It works whether the server is running
// or not: SQLite serializes the single short write with the server's own
// transactions (the password is hashed before the write lock is taken).
//
// The password is read from the terminal without echo (twice), read from
// stdin with --stdin, or generated and printed once with --generate (the
// default when stdin is not a terminal).
func resetPassword(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", defaultDataDir(), "data directory of the instance ($HMNPAY_DATA_DIR)")
	generate := fs.Bool("generate", false, "generate a strong password and print it once")
	fromStdin := fs.Bool("stdin", false, "read the new password from the first line of stdin")
	revokeKeys := fs.Bool("revoke-api-keys", false, "also revoke every API key (do this if someone else may have had access)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: hmnpay reset-password [flags]\n\nSets a new operator password and signs out every session.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 || (*generate && *fromStdin) {
		fs.Usage()
		return exitUsage
	}
	db := filepath.Join(*dataDir, hmnpay.DatabaseFile)
	if _, err := os.Stat(db); err != nil {
		fmt.Fprintf(stderr, "hmnpay: no hmnpay database at %s (set --data-dir or HMNPAY_DATA_DIR): %v\n", db, err)
		return exitFail
	}

	tty := -1
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		tty = int(f.Fd())
	}
	var password string
	generated := false
	switch {
	case *fromStdin:
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintf(stderr, "hmnpay: read password: %v\n", err)
			return exitFail
		}
		password = strings.TrimRight(line, "\r\n")
	case *generate || tty < 0:
		p, err := generatePassword()
		if err != nil {
			fmt.Fprintf(stderr, "hmnpay: %v\n", err)
			return exitFail
		}
		password, generated = p, true
	default:
		p, err := promptPassword(tty, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "hmnpay: %v\n", err)
			return exitFail
		}
		password = p
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	eng, err := openTool(ctx, *dataDir, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "hmnpay: %v\n", err)
		return exitFail
	}
	defer eng.Close()
	sys := core.WithActor(ctx, core.SystemActor("cli-reset-password"))
	before, err := eng.Service().InstanceSummary(sys)
	if err != nil {
		fmt.Fprintf(stderr, "hmnpay: read instance: %v\n", err)
		return exitFail
	}
	err = eng.Service().ResetOperatorPassword(sys, password)
	switch {
	case errors.Is(err, core.ErrSetupRequired):
		fmt.Fprintln(stderr, "hmnpay: setup has not been completed yet; there is no operator account to reset. Open the web interface to run the setup wizard.")
		return exitFail
	case len(core.FieldErrors(err)) > 0:
		fmt.Fprintf(stderr, "hmnpay: %s\n", core.FieldErrors(err)[0].Message)
		return exitUsage
	case err != nil:
		fmt.Fprintf(stderr, "hmnpay: reset failed: %v\n", err)
		return exitFail
	}
	revoked := 0
	if *revokeKeys {
		if revoked, err = eng.Service().RevokeAllAPIKeys(sys); err != nil {
			fmt.Fprintf(stderr, "hmnpay: the password was changed, but revoking API keys failed: %v\n", err)
			return exitFail
		}
	}
	// The reset moved the operator's credential generation, and every
	// "known device" marker is bound to it: every browser is an unknown
	// device from this moment on, in the running server too. (The key that
	// signs the markers is deliberately left alone. A running server keeps
	// the key it started with, so rotating the stored one here would do
	// nothing now and something later: the marker the operator gets on the
	// first login after this reset would be forgotten at the next restart,
	// and under a login flood the operator would be locked out a second
	// time.)
	//
	// An unknown device shares the global login budget, which anonymous
	// traffic can keep empty. The one-time recovery code lets the operator
	// in regardless (it never replaces the password).
	recovery, recoveryErr := eng.Service().MintRecoveryCode(sys)
	after, err := eng.Service().InstanceSummary(sys)
	if err != nil {
		after = before
	}

	if generated {
		fmt.Fprintf(stdout, "New operator password (shown once, store it now):\n%s\n\n", password)
	}
	fmt.Fprintf(stdout, "Password changed for %s. %d dashboard session(s) signed out.\n", after.OperatorEmail, before.Sessions)
	if *revokeKeys {
		fmt.Fprintf(stdout, "%d API key(s) revoked.\n", revoked)
	}
	if recoveryErr != nil {
		fmt.Fprintf(stderr, "hmnpay: warning: could not create a recovery code: %v (run \"hmnpay recovery-code\" to try again)\n", recoveryErr)
	} else {
		fmt.Fprintln(stdout)
		printRecoveryCode(stdout, recovery, "the new password")
	}
	fmt.Fprintln(stdout)
	printSummary(stdout, after)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "CHECK THE WALLET ABOVE. If you did not configure this wallet, your instance was set up by someone else:")
	fmt.Fprintln(stdout, "payments go to that wallet. Stop the server, move the data directory aside and set the instance up again.")
	if !*revokeKeys && after.ActiveAPIKeys > 0 {
		fmt.Fprintf(stdout, "API keys were NOT revoked (%d active). If anyone else may have had access, run again with --revoke-api-keys.\n", after.ActiveAPIKeys)
	}
	return exitOK
}

// printRecoveryCode prints a freshly minted recovery code, in the one format
// both commands that mint one use.
func printRecoveryCode(w io.Writer, rc core.RecoveryCode, password string) {
	fmt.Fprintf(w, "Recovery code (valid until %s UTC, one login, shown once):\n%s\n", rc.ExpiresAt.UTC().Format("15:04"), rc.Code)
	fmt.Fprintf(w, "Enter it on the login page together with %s if the login says \"too many attempts\".\n", password)
}

// recoveryCode mints a one-time recovery code and prints it: the way back in
// for an operator who knows the password but is kept out by the login
// throttles on a browser the instance does not know (see
// core.Service.MintRecoveryCode). It changes nothing else — not the password,
// not the sessions, not the browsers the instance remembers — and works
// whether the server is running or not. An earlier code stops working.
func recoveryCode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("recovery-code", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", defaultDataDir(), "data directory of the instance ($HMNPAY_DATA_DIR)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: hmnpay recovery-code [flags]\n\nPrints a one-time code that lets one login past the login rate limits\n(the password is still required). Changes nothing else.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return exitUsage
	}
	db := filepath.Join(*dataDir, hmnpay.DatabaseFile)
	if _, err := os.Stat(db); err != nil {
		fmt.Fprintf(stderr, "hmnpay: no hmnpay database at %s (set --data-dir or HMNPAY_DATA_DIR): %v\n", db, err)
		return exitFail
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eng, err := openTool(ctx, *dataDir, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "hmnpay: %v\n", err)
		return exitFail
	}
	defer eng.Close()
	rc, err := eng.Service().MintRecoveryCode(core.WithActor(ctx, core.SystemActor("cli-recovery-code")))
	switch {
	case errors.Is(err, core.ErrSetupRequired):
		fmt.Fprintln(stderr, "hmnpay: setup has not been completed yet; there is no operator account to log in to. Open the web interface to run the setup wizard.")
		return exitFail
	case err != nil:
		fmt.Fprintf(stderr, "hmnpay: could not create a recovery code: %v\n", err)
		return exitFail
	}
	printRecoveryCode(stdout, rc, "your password")
	return exitOK
}

// openTool opens the data directory for an operator tool: no workers, no
// setup token (system actors need none), quiet logs. It does not take the
// server lock, so it works next to a running server.
func openTool(ctx context.Context, dataDir string, stderr io.Writer) (*hmnpay.Engine, error) {
	return hmnpay.Open(ctx, hmnpay.Config{
		DataDir:      dataDir,
		Logger:       slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Chain:        hmnpay.ChainConfig{Disabled: true},
		Webhooks:     hmnpay.WebhookConfig{Disabled: true},
		Prober:       hmnpay.ProberConfig{Disabled: true},
		Prune:        hmnpay.PruneConfig{Disabled: true},
		NoSetupToken: true, // nothing is served; the CLI acts as a system actor
	})
}

// printSummary prints who and what an instance is configured for.
func printSummary(w io.Writer, s core.InstanceSummary) {
	if !s.SetupCompleted {
		fmt.Fprintln(w, "Instance: setup has not been completed.")
		return
	}
	first := s.FirstAddress
	if first == "" {
		first = "(cannot derive: " + s.FirstAddressError + ")"
	}
	fmt.Fprintf(w, "Instance configuration:\n")
	fmt.Fprintf(w, "  Operator:           %s (since %s)\n", s.OperatorEmail, s.OperatorCreatedAt.UTC().Format("2006-01-02"))
	fmt.Fprintf(w, "  Network:            chain %d, %s %s\n", s.ChainID, s.Asset.Symbol, s.Asset.Contract)
	fmt.Fprintf(w, "  Wallet xpub:        %s\n", xpubFingerprint(s.XPub))
	fmt.Fprintf(w, "  Derivation path:    %s  (%d address(es) handed out)\n", s.DerivationPath, s.NextAddressIndex)
	fmt.Fprintf(w, "  First deposit addr: %s  <- compare with address #0 in YOUR wallet\n", first)
	fmt.Fprintf(w, "  RPC endpoints:      %s\n", strings.Join(s.RPCHosts, ", "))
	fmt.Fprintf(w, "  API keys:           %d active of %d\n", s.ActiveAPIKeys, s.APIKeys)
	fmt.Fprintf(w, "  Webhook endpoints:  %d enabled of %d\n", s.EnabledWebhookEndpoints, s.WebhookEndpoints)
	fmt.Fprintf(w, "  Active sessions:    %d\n", s.Sessions)
}

// xpubFingerprint shortens an xpub to its first and last characters: the
// xpub is not secret, but 111 characters are not comparable by eye.
func xpubFingerprint(x string) string {
	if len(x) <= 24 {
		return x
	}
	return x[:12] + "…" + x[len(x)-8:]
}

// inspect prints the instance summary without changing anything.
func inspect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", defaultDataDir(), "data directory of the instance ($HMNPAY_DATA_DIR)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	db := filepath.Join(*dataDir, hmnpay.DatabaseFile)
	if _, err := os.Stat(db); err != nil {
		fmt.Fprintf(stderr, "hmnpay: no hmnpay database at %s: %v\n", db, err)
		return exitFail
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eng, err := openTool(ctx, *dataDir, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "hmnpay: %v\n", err)
		return exitFail
	}
	defer eng.Close()
	sum, err := eng.Service().InstanceSummary(core.WithActor(ctx, core.SystemActor("cli-inspect")))
	if err != nil {
		fmt.Fprintf(stderr, "hmnpay: %v\n", err)
		return exitFail
	}
	printSummary(stdout, sum)
	return exitOK
}

// promptPassword reads a password twice from the terminal without echo.
func promptPassword(fd int, w io.Writer) (string, error) {
	fmt.Fprintf(w, "New operator password (at least %d characters): ", core.MinPasswordLength)
	a, err := term.ReadPassword(fd)
	fmt.Fprintln(w)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	fmt.Fprint(w, "Repeat the password: ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(w)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if string(a) != string(b) {
		return "", errors.New("the passwords do not match")
	}
	return string(a), nil
}

// generatePassword returns 24 characters from a 56-symbol alphabet without
// look-alikes: about 139 bits of entropy.
func generatePassword() (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	const n = 24
	out := make([]byte, 0, n)
	buf := make([]byte, 64)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		for _, b := range buf {
			// Rejection sampling keeps the distribution uniform.
			if int(b) < 256-256%len(alphabet) && len(out) < n {
				out = append(out, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	return string(out), nil
}

// healthcheck GETs the local server's /healthz: exit 0 on 200, 1
// otherwise. The container image has no shell or curl, so the image's
// HEALTHCHECK runs this.
func healthcheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := ":8080"
	if v, ok := lookup(os.Getenv, "HMNPAY_LISTEN"); ok {
		listen = v
	}
	target := fs.String("url", healthURL(listen), "health endpoint to check (default derived from $HMNPAY_LISTEN)")
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	client := &http.Client{Timeout: *timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(*target)
	if err != nil {
		fmt.Fprintf(stderr, "unhealthy: %v\n", err)
		return exitFail
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "unhealthy: %s answered %s\n", *target, resp.Status)
		return exitFail
	}
	fmt.Fprintln(stdout, "ok")
	return exitOK
}

// healthURL turns a listen address into a loopback URL of /healthz.
func healthURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1:8080/healthz"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}
