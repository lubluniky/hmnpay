//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// lockFile is the name of the server lock inside the data directory.
const lockFile = "hmnpay.lock"

// errLocked means another server holds the data directory.
var errLocked = errors.New("another hmnpay server is already running on this data directory")

// lockDataDir takes an exclusive, non-blocking advisory lock (flock) on
// <dir>/hmnpay.lock, so that only one server runs per data directory. The
// kernel releases it when the process exits, however it exits, so a crash
// never leaves a stale lock. Operator tools (reset-password, inspect) do
// not take it: they make short transactions that SQLite serializes with
// the server's. flock is advisory and local: it does not protect a data
// directory shared over NFS between hosts.
func lockDataDir(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, lockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			holder, _ := os.ReadFile(path)
			if pid := string(holder); pid != "" {
				return nil, fmt.Errorf("%w (pid %s, lock %s)", errLocked, pid, path)
			}
			return nil, fmt.Errorf("%w (lock %s)", errLocked, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Informational only: the lock is the flock, not the content.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

const lockSupported = true
