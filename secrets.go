package hmnpay

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"

	"github.com/lubluniky/hmnpay/internal/store/sqlite"
)

// tokenEncoding is lowercase Crockford-style base32: no padding, no
// look-alike characters, easy to copy from a terminal.
var tokenEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// generateSetupToken returns 160 random bits as 32 characters.
func generateSetupToken() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("hmnpay: generate setup token: %w", err)
	}
	return tokenEncoding.EncodeToString(b), nil
}

// deviceKeyName is the KV key of the HTTP API's device key.
const deviceKeyName = "httpapi.device_key"

// deviceKeyLen is the length of the device key.
const deviceKeyLen = 32

// loadDeviceKey returns the instance's device key, creating it on first
// use. The key signs the "known device" cookie, a low-value convenience
// (it only exempts a browser that logged in before from the shared login
// rate limits; the password is always required). It is kept in the
// database so that the marker survives restarts; anyone who can read the
// database could forge markers, which gains them nothing but a skipped
// rate-limit bucket.
func loadDeviceKey(ctx context.Context, s *sqlite.Store) ([]byte, error) {
	key, err := s.GetValue(ctx, deviceKeyName)
	if err != nil {
		return nil, err
	}
	if len(key) == deviceKeyLen {
		return key, nil
	}
	if err := rotateDeviceKey(ctx, s); err != nil {
		return nil, err
	}
	// Read back what is stored: if another process created a key at the
	// same moment, both end up using the one that won.
	return s.GetValue(ctx, deviceKeyName)
}

func rotateDeviceKey(ctx context.Context, s *sqlite.Store) error {
	key := make([]byte, deviceKeyLen)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	return s.PutValue(ctx, deviceKeyName, key)
}

// RotateDeviceKey replaces the stored device key, so that every browser
// counts as an unknown device again — from the next start of the server: a
// server that is running keeps the key it started with, and every marker it
// hands out until then dies at that restart.
//
// Nothing in hmnpay calls it. Forgetting every browser when the password
// changes needs no key change: the markers are bound to the account's
// credential generation (core.Service.DeviceBinding), which a password
// change or reset moves at once, in the running server too. Do not call
// this as part of a password reset (an earlier version did): the browser
// the operator then logs in with would be forgotten again at the next
// restart. It is for the case that the key itself may have been read by
// someone else — and then the server has to be restarted for it to matter.
func (e *Engine) RotateDeviceKey(ctx context.Context) error {
	if err := rotateDeviceKey(ctx, e.store); err != nil {
		return fmt.Errorf("hmnpay: rotate device key: %w", err)
	}
	return nil
}
