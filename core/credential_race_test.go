package core_test

// A credential that was reset or revoked does not act any more, not even for
// a request that verified it just before: the race of a login with a
// password reset, and the other paths with the same race.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// racingHasher lets a test run something at the moment a password is being
// verified — the window in which the real KDF spends its hundreds of
// milliseconds.
type racingHasher struct {
	coretest.Hasher
	during func()
}

func (h *racingHasher) Verify(hash, password string) (bool, error) {
	if f := h.during; f != nil {
		h.during = nil
		f()
	}
	return h.Hasher.Verify(hash, password)
}

// Login read the operator, verified the password against the hash it
// had read (slow, outside any transaction) and then opened a session without
// looking at the operator again. A password reset — the owner's remedy when
// somebody else knows the password, documented as "changes the password,
// ends every session" — that committed while such a login was inside the KDF
// did not stop it: the login completed with the OLD password and its session
// was created after the reset had deleted "every" session.
func TestLoginWithTheOldPasswordDoesNotSurviveAReset(t *testing.T) {
	hasher := &racingHasher{}
	e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.Hasher = hasher }))
	old := e.Password

	hasher.during = func() {
		// The owner runs `hmnpay reset-password` while the intruder's
		// login is being verified.
		if err := e.Service.ResetOperatorPassword(e.System(), "a brand new password 2026"); err != nil {
			t.Fatalf("reset: %v", err)
		}
	}
	res, err := e.Service.Login(context.Background(), core.LoginInput{Email: e.Email, Password: old})
	if !errors.Is(err, core.ErrInvalidCredentials) {
		t.Errorf("a login with the old password, in flight during the reset: err = %v, want invalid credentials", err)
	}
	if err == nil {
		if _, aerr := e.Service.AuthenticateSession(context.Background(), res.Token); aerr == nil {
			t.Fatalf("a login with the OLD password, in flight while the password was reset, returned a session that still authenticates after the reset (session %s): the reset did not end every session", res.Session.ID)
		}
	}
	// Nothing is left behind, and the new password works.
	if _, err := e.Service.Login(context.Background(), core.LoginInput{Email: e.Email, Password: "a brand new password 2026"}); err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
	sessions, err := e.Service.ListSessions(e.System())
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions after the reset and one login: %d (%v), want 1", len(sessions), err)
	}
}

// The same race on the other paths that verify a password outside a
// transaction and write afterwards.
func TestOtherPasswordPathsInFlightDuringAReset(t *testing.T) {
	const fresh = "a brand new password 2026"
	t.Run("a login with a recovery code", func(t *testing.T) {
		hasher := &racingHasher{}
		e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.Hasher = hasher }))
		code, err := e.Service.MintRecoveryCode(e.System())
		if err != nil {
			t.Fatal(err)
		}
		hasher.during = func() {
			if err := e.Service.ResetOperatorPassword(e.System(), fresh); err != nil {
				t.Fatalf("reset: %v", err)
			}
		}
		_, err = e.Service.Login(context.Background(), core.LoginInput{Email: e.Email, Password: e.Password, RecoveryCode: code.Code})
		if !errors.Is(err, core.ErrInvalidCredentials) {
			t.Fatalf("err = %v, want invalid credentials", err)
		}
		if sessions, _ := e.Service.ListSessions(e.System()); len(sessions) != 0 {
			t.Fatalf("%d sessions survive the reset", len(sessions))
		}
	})
	t.Run("a password change by a session the reset ended", func(t *testing.T) {
		hasher := &racingHasher{}
		e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.Hasher = hasher }))
		intruder := e.Operator() // authenticated before the reset
		hasher.during = func() {
			if err := e.Service.ResetOperatorPassword(e.System(), fresh); err != nil {
				t.Fatalf("reset: %v", err)
			}
		}
		if err := e.Service.ChangePassword(intruder, e.Password, "the intruder's password 1"); err == nil {
			t.Fatal("a password change that was in flight during the reset went through")
		}
		if _, err := e.Service.Login(context.Background(), core.LoginInput{Email: e.Email, Password: fresh}); err != nil {
			t.Fatalf("the owner's new password does not work: %v", err)
		}
		if _, err := e.Service.Login(context.Background(), core.LoginInput{Email: e.Email, Password: "the intruder's password 1"}); !errors.Is(err, core.ErrInvalidCredentials) {
			t.Fatalf("the intruder's password works: %v", err)
		}
	})
}

// Generalised: a credential that was revoked does not write any
// more, also for a request that was authenticated before the revocation.
// "Ends every session" and "revokes every key" are final for every
// transaction that commits after them.
func TestARevokedCredentialDoesNotWrite(t *testing.T) {
	t.Run("a session ended by a password reset cannot create an API key", func(t *testing.T) {
		e := coretest.NewEnv(t)
		intruder := e.Operator()
		if err := e.Service.ResetOperatorPassword(e.System(), "a brand new password 2026"); err != nil {
			t.Fatal(err)
		}
		if n, err := e.Service.RevokeAllAPIKeys(e.System()); err != nil || n != 0 {
			t.Fatalf("revoke all: %d, %v", n, err)
		}
		_, _, err := e.Service.CreateAPIKey(intruder, core.CreateAPIKeyInput{Name: "left behind", Scopes: []core.Scope{core.ScopePaymentsWrite}})
		if !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("err = %v, want unauthorized", err)
		}
		if keys, _ := e.Service.ListAPIKeys(e.System()); len(keys) != 0 {
			t.Fatalf("%d API keys exist after the reset", len(keys))
		}
		// Nor move the wallet, nor anything else that writes.
		if _, err := e.Service.CreateLink(intruder, core.LinkInput{Name: "x", Active: true}); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("create link: err = %v, want unauthorized", err)
		}
	})
	t.Run("a revoked session", func(t *testing.T) {
		e := coretest.NewEnv(t)
		res, err := e.Service.Login(context.Background(), core.LoginInput{Email: e.Email, Password: e.Password})
		if err != nil {
			t.Fatal(err)
		}
		actor, err := e.Service.AuthenticateSession(context.Background(), res.Token)
		if err != nil {
			t.Fatal(err)
		}
		other := core.WithActor(context.Background(), actor)
		if err := e.Service.RevokeSession(e.Operator(), res.Session.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.Service.CreateAPIKey(other, core.CreateAPIKeyInput{Name: "k", Scopes: []core.Scope{core.ScopePaymentsRead}}); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("err = %v, want unauthorized", err)
		}
		// The session that revoked it works as before.
		if _, _, err := e.Service.CreateAPIKey(e.Operator(), core.CreateAPIKeyInput{Name: "k", Scopes: []core.Scope{core.ScopePaymentsRead}}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a session that ran out", func(t *testing.T) {
		e := coretest.NewEnv(t)
		stale := e.Operator()
		e.Clock.Advance(core.DefaultSessionIdleTimeout + time.Minute)
		if _, err := e.Service.CreateLink(stale, core.LinkInput{Name: "x", Active: true}); !errors.Is(err, core.ErrCredentialsExpired) {
			t.Fatalf("err = %v, want expired credentials", err)
		}
	})
	t.Run("a revoked API key", func(t *testing.T) {
		e := coretest.NewEnv(t)
		key, _ := e.APIKey(core.ScopePaymentsWrite, core.ScopeLinksWrite)
		if _, _, err := e.Service.CreatePayment(key, core.CreatePaymentInput{Amount: "10"}); err != nil {
			t.Fatal(err)
		}
		if n, err := e.Service.RevokeAllAPIKeys(e.Operator()); err != nil || n != 1 {
			t.Fatalf("revoke all: %d, %v", n, err)
		}
		if _, _, err := e.Service.CreatePayment(key, core.CreatePaymentInput{Amount: "10"}); !errors.Is(err, core.ErrCredentialsRevoked) {
			t.Fatalf("err = %v, want revoked credentials", err)
		}
	})
}
