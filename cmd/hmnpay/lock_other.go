//go:build !(linux || darwin || freebsd || openbsd || netbsd || dragonfly)

package main

import "errors"

var errLocked = errors.New("another hmnpay server is already running on this data directory")

// lockDataDir is a no-op on platforms without flock (Windows, Plan 9,
// …): nothing stops a second server on the same data directory there.
// serve logs a warning.
func lockDataDir(string) (func(), error) { return func() {}, nil }

const lockSupported = false
