// Command hmnpay is the self-hosted hmnpay server: the payment engine, the
// REST API with the hosted-checkout stream, and the operator tools, in one
// static binary.
//
//	hmnpay [serve]          run the server (default)
//	hmnpay reset-password   set a new operator password, end all sessions
//	hmnpay inspect          show the instance's configuration (read-only)
//	hmnpay healthcheck      exit 0 when the local server answers /healthz
//	hmnpay version          print the version
//
// Deployment settings come from HMNPAY_* environment variables, overridable
// by flags (hmnpay serve -h lists them). Everything else — the operator
// account, the network, RPC endpoints, the wallet — is configured in the
// first-run setup wizard over HTTP.
//
// Exit codes: 0 success or clean shutdown; 1 runtime failure (the database
// cannot be opened, a worker died, the listener failed); 2 invalid usage or
// configuration; 3 forced exit (second signal, or shutdown did not finish
// in time).
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/lubluniky/hmnpay"
)

const (
	exitOK     = 0
	exitFail   = 1
	exitUsage  = 2
	exitForced = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches a command line and returns the exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(args, stdout, stderr)
	case "reset-password":
		return resetPassword(args, stdin, stdout, stderr)
	case "recovery-code":
		return recoveryCode(args, stdout, stderr)
	case "inspect":
		return inspect(args, stdout, stderr)
	case "healthcheck":
		return healthcheck(args, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, versionString())
		return exitOK
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	}
	fmt.Fprintf(stderr, "hmnpay: unknown command %q\n\n", cmd)
	usage(stderr)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: hmnpay [command] [flags]

Commands:
  serve            run the server (default)
  reset-password   set a new operator password and end all sessions
  recovery-code    print a one-time code that gets one login past the rate limits
  inspect          show who and what the instance is configured for (read-only)
  healthcheck      check the local server's /healthz (for container health checks)
  version          print the version

Run "hmnpay <command> -h" for the flags of a command.
`)
}

func versionString() string {
	commit := hmnpay.Commit
	if commit == "" {
		commit = "unknown"
	}
	return fmt.Sprintf("hmnpay %s (commit %s, %s, %s/%s)", hmnpay.Version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
