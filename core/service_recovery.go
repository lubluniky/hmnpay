package core

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Recovery code policy.
const (
	// RecoveryCodeTTL is how long a recovery code works after it was
	// minted.
	RecoveryCodeTTL = 15 * time.Minute
	// RecoveryCodeMaxAttempts is how many logins may be attempted with one
	// recovery code. Every attempt counts, whatever its outcome, so one
	// code can never cause more than this many password verifications.
	RecoveryCodeMaxAttempts = 5

	recoveryCodePrefix = "hmnr_"
)

// RecoveryCode is a freshly minted one-time recovery code.
type RecoveryCode struct {
	// Code is the secret ("hmnr_" plus 26 random characters, 128 bits),
	// shown exactly once; only its SHA-256 is stored.
	Code string
	// ExpiresAt is when it stops working.
	ExpiresAt time.Time
}

// MintRecoveryCode creates a one-time recovery code for the operator and
// returns it; any earlier code stops working. It is the way back in for an
// operator whom the transports' shared login throttles keep out — those
// throttles exist to stop anonymous password guessing, and anonymous
// traffic can exhaust them for everybody on a browser the instance has not
// seen before. A login that presents the code ([LoginInput.RecoveryCode])
// is one the transports may let past those shared throttles.
//
// The code replaces nothing: the password is still required and still
// verified. It works for [RecoveryCodeTTL] (15 minutes), for one
// successful login, and for at most [RecoveryCodeMaxAttempts] (5) attempts
// in total. Changing or resetting the password cancels it.
//
// It fails with [ErrSetupRequired] before setup. System only, like
// [Service.ResetOperatorPassword]: it is meant to be printed by a command
// run on the server ("hmnpay recovery-code", and "hmnpay reset-password"
// together with the new password), and no transport may expose it.
func (s *Service) MintRecoveryCode(ctx context.Context) (RecoveryCode, error) {
	var out RecoveryCode
	if _, err := authorize(ctx, permSystem); err != nil {
		return out, err
	}
	random, err := s.randomString(16)
	if err != nil {
		return out, err
	}
	code := recoveryCodePrefix + random
	err = s.update(ctx, func(u *unit) error {
		op, err := u.GetOperator(ctx)
		if errors.Is(err, ErrNotFound) {
			return newError(ErrSetupRequired, "")
		}
		if err != nil {
			return err
		}
		op.RecoveryHash = hashSecret(code)
		op.RecoveryExpiresAt = u.now.Add(RecoveryCodeTTL)
		op.RecoveryAttempts = 0
		out = RecoveryCode{Code: code, ExpiresAt: op.RecoveryExpiresAt}
		return u.SaveOperator(ctx, op)
	})
	if err != nil {
		return RecoveryCode{}, err
	}
	return out, nil
}

// RecoveryCodeValid reports whether code is the operator's outstanding
// recovery code: minted, not expired, not used and with attempts left. It
// reads one row and compares hashes — no password verification — so a
// transport can call it before deciding which throttles a login attempt is
// subject to. It consumes nothing. Public; the code is 128 random bits, so
// answering "no" tells a guesser nothing useful.
func (s *Service) RecoveryCodeValid(ctx context.Context, code string) (bool, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return false, nil
	}
	now := s.now()
	valid := false
	err := s.store.View(ctx, func(r Reader) error {
		op, err := r.GetOperator(ctx)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		valid = op.recoveryMatches(code, now)
		return err
	})
	return valid && err == nil, err
}

// recoveryMatches reports whether code is the outstanding recovery code.
func (op Operator) recoveryMatches(code string, now time.Time) bool {
	return op.RecoveryHash != "" &&
		now.Before(op.RecoveryExpiresAt) &&
		op.RecoveryAttempts < RecoveryCodeMaxAttempts &&
		secretsEqual(op.RecoveryHash, hashSecret(code))
}

// clearRecovery cancels the outstanding recovery code, if any.
func (op *Operator) clearRecovery() {
	op.RecoveryHash, op.RecoveryExpiresAt, op.RecoveryAttempts = "", time.Time{}, 0
}

// redacted returns the operator without anything secret-derived.
func (op Operator) redacted() Operator {
	op.PasswordHash = ""
	op.clearRecovery()
	return op
}

// spendRecoveryAttempt checks a recovery code and counts one attempt
// against it, before the password is verified.
func (s *Service) spendRecoveryAttempt(ctx context.Context, code string) error {
	return s.update(ctx, func(u *unit) error {
		op, err := u.GetOperator(ctx)
		if errors.Is(err, ErrNotFound) {
			return newError(ErrSetupRequired, "")
		}
		if err != nil {
			return err
		}
		if !op.recoveryMatches(code, u.now) {
			return newError(ErrRecoveryCodeInvalid, "")
		}
		op.RecoveryAttempts++
		return u.SaveOperator(ctx, op)
	})
}

// DeviceBinding returns an opaque value that identifies the operator
// account and its current credentials: it changes whenever the password is
// changed or reset, and never otherwise. A transport that remembers a
// browser as a "known device" binds its marker to this value, so that a
// password reset forgets every remembered browser at once.
//
// The value is a hash and reveals nothing. It fails with
// [ErrSetupRequired] before setup. Public.
func (s *Service) DeviceBinding(ctx context.Context) (string, error) {
	var binding string
	err := s.store.View(ctx, func(r Reader) error {
		op, err := r.GetOperator(ctx)
		if errors.Is(err, ErrNotFound) {
			return newError(ErrSetupRequired, "")
		}
		if err != nil {
			return err
		}
		binding = hashSecret("hmnpay device binding v1\x00" + op.ID + "\x00" + strconv.FormatInt(op.Generation, 10))[:32]
		return nil
	})
	return binding, err
}
