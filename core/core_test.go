package core_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// The in-memory store is the reference implementation of the store
// contract; it has to pass the same suites as every real store.
func TestMemStore(t *testing.T) {
	coretest.RunStoreSuite(t, func(*testing.T) core.Store { return coretest.NewMemStore() })
}

// The business rules, end to end.
func TestService(t *testing.T) {
	coretest.RunServiceSuite(t, func(*testing.T) core.Store { return coretest.NewMemStore() })
}

func TestPBKDF2Hasher(t *testing.T) {
	// A low work factor keeps the suite fast; the format is the same.
	coretest.RunPasswordHasherSuite(t, core.PBKDF2Hasher{Iterations: 1000})

	h := core.PBKDF2Hasher{Iterations: 1000}
	hash, err := h.Hash("some long password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$pbkdf2-sha256$i=1000$") || strings.Count(hash, "$") != 4 {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	// Verification honours the parameters stored in the hash, so hashes
	// survive a change of the default work factor.
	if ok, err := (core.PBKDF2Hasher{}).Verify(hash, "some long password"); err != nil || !ok {
		t.Fatalf("default hasher cannot verify a hash with other parameters: %v, %v", ok, err)
	}
	for _, bad := range []string{"$pbkdf2-sha256$i=0$c2FsdA$aGFzaA", "$pbkdf2-sha256$i=x$c2FsdA$aGFzaA", "$pbkdf2-sha256$i=10$!!$aGFzaA", "$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA"} {
		if ok, err := h.Verify(bad, "x"); ok || err == nil {
			t.Errorf("Verify(%q) = %v, %v; want an error", bad, ok, err)
		}
	}
}

func TestDefaultHasherWorkFactor(t *testing.T) {
	if testing.Short() {
		t.Skip("slow KDF")
	}
	hash, err := core.PBKDF2Hasher{}.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$pbkdf2-sha256$i=600000$") {
		t.Fatalf("default work factor changed: %s", hash)
	}
}

func TestNew(t *testing.T) {
	if _, err := core.New(core.Config{}); err == nil {
		t.Error("New without a store must fail")
	}
	if _, err := core.New(core.Config{Store: coretest.NewMemStore()}); err == nil {
		t.Error("New without an address source must fail")
	}
	// Everything else has a production default.
	svc, err := core.New(core.Config{Store: coretest.NewMemStore(), Addresses: &coretest.Addresses{}})
	if err != nil {
		t.Fatal(err)
	}
	status, err := svc.SetupStatus(context.Background())
	if err != nil || status.Completed {
		t.Fatalf("fresh service: %+v, %v", status, err)
	}
}

// IDs are capabilities: with real randomness they must be unique, have the
// documented shape and be URL-safe.
func TestIDs(t *testing.T) {
	e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.Rand = nil }))
	seen := map[string]bool{}
	for range 200 {
		p := e.CreatePayment("1")
		if seen[p.ID] {
			t.Fatalf("duplicate id %s", p.ID)
		}
		seen[p.ID] = true
		if len(p.ID) != 30 || !strings.HasPrefix(p.ID, "pay_") || strings.Trim(p.ID[4:], "0123456789abcdefghjkmnpqrstvwxyz") != "" {
			t.Fatalf("id %q is not pay_ + 26 base32 characters", p.ID)
		}
	}
}

type failingRand struct{}

func (failingRand) Read([]byte) (int, error) { return 0, errors.New("entropy exhausted") }

func TestRandomnessFailureIsAnError(t *testing.T) {
	e := coretest.NewEnv(t, coretest.WithoutSetup(), coretest.WithConfig(func(c *core.Config) { c.Rand = failingRand{} }))
	_, err := e.Service.CompleteSetup(context.Background(), e.SetupInput())
	if err == nil || core.KindOf(err) != core.KindInternal {
		t.Fatalf("setup with a failing random source: %v", err)
	}
	if status, _ := e.Service.SetupStatus(context.Background()); status.Completed {
		t.Fatal("a failed setup must not be stored")
	}
}

func TestErrors(t *testing.T) {
	e := coretest.NewEnv(t)
	_, err := e.Service.GetPayment(e.System(), "pay_missing")
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Kind != core.KindNotFound || ce.Code != "payment_not_found" || !errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrConflict) {
		t.Fatalf("not found error: %#v", err)
	}
	// Specific sentinels match their kind's sentinel, not each other.
	if !errors.Is(core.ErrLinkExpired, core.ErrConflict) || errors.Is(core.ErrLinkExpired, core.ErrLinkDisabled) || errors.Is(core.ErrConflict, core.ErrLinkExpired) {
		t.Error("sentinel matching is wrong")
	}
	if !errors.Is(core.ErrInvalidCredentials, core.ErrUnauthorized) || errors.Is(core.ErrUnauthorized, core.ErrForbidden) {
		t.Error("sentinel matching is wrong")
	}
	kinds := map[error]core.Kind{
		nil:                         "",
		errors.New("disk on fire"):  core.KindInternal,
		context.Canceled:            core.KindInternal,
		core.ErrSetupRequired:       core.KindSetupRequired,
		core.ErrIdempotencyMismatch: core.KindConflict,
		core.ErrChainMismatch:       core.KindInvalid,
		core.ErrUnknownAddress:      core.KindNotFound,
		core.ErrCredentialsExpired:  core.KindUnauthorized,
		core.ErrPaymentHasFunds:     core.KindConflict,
		core.ErrPaymentSettled:      core.KindConflict,
	}
	for err, want := range kinds {
		if got := core.KindOf(err); got != want {
			t.Errorf("KindOf(%v) = %q, want %q", err, got, want)
		}
	}
	// Wrapping keeps the kind and the field errors.
	_, _, err = e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "x"})
	wrapped := errors.Join(errors.New("handler"), err)
	if core.KindOf(wrapped) != core.KindInvalid || len(core.FieldErrors(wrapped)) != 1 || core.FieldErrors(wrapped)[0].Field != "amount" {
		t.Fatalf("wrapped validation error: %v", wrapped)
	}
	if !strings.Contains(err.Error(), "amount") {
		t.Errorf("error text should name the field: %q", err.Error())
	}
}

func TestNormalizeAddressAndHash(t *testing.T) {
	addr, err := core.NormalizeAddress("0x55d398326f99059fF775485246999027B3197955")
	if err != nil || addr != "0x55d398326f99059ff775485246999027b3197955" {
		t.Fatalf("NormalizeAddress = %q, %v", addr, err)
	}
	for _, bad := range []string{"", "0x", "55d398326f99059fF775485246999027B3197955", "0x55d398326f99059fF775485246999027B319795", "0x55d398326f99059fF775485246999027B31979555", "0xZZd398326f99059fF775485246999027B3197955", " 0x55d398326f99059fF775485246999027B3197955"} {
		if _, err := core.NormalizeAddress(bad); err == nil {
			t.Errorf("NormalizeAddress(%q) succeeded", bad)
		}
	}
	h := coretest.Hash("x")
	if got, err := core.NormalizeHash(strings.ToUpper(h[2:])); err == nil {
		t.Errorf("NormalizeHash without 0x succeeded: %q", got)
	}
	if got, err := core.NormalizeHash("0x" + strings.ToUpper(h[2:])); err != nil || got != h {
		t.Errorf("NormalizeHash = %q, %v", got, err)
	}
	if _, err := core.NormalizeHash(addr); err == nil {
		t.Error("NormalizeHash accepted an address")
	}
	if a, ok := core.DefaultAsset(56); !ok || a.Decimals != 18 || a.Contract != addr {
		t.Errorf("BSC USDT preset: %+v", a)
	}
	if _, ok := core.DefaultAsset(1); ok {
		t.Error("unexpected preset for chain 1")
	}
}

// ResetOperatorPassword is the locked-out operator's recovery path: system
// actors only, no current password, every session ends.
func TestResetOperatorPassword(t *testing.T) {
	ctx := context.Background()
	t.Run("before setup", func(t *testing.T) {
		e := coretest.NewEnv(t, coretest.WithoutSetup())
		err := e.Service.ResetOperatorPassword(e.System(), "a brand new passphrase")
		if !errors.Is(err, core.ErrSetupRequired) {
			t.Fatalf("got %v, want ErrSetupRequired", err)
		}
	})
	t.Run("authorization", func(t *testing.T) {
		e := coretest.NewEnv(t)
		apiKey, _ := e.APIKey(core.Scopes...)
		for name, c := range map[string]context.Context{"anonymous": ctx, "operator": e.Operator(), "api key": apiKey} {
			if err := e.Service.ResetOperatorPassword(c, "a brand new passphrase"); err == nil {
				t.Errorf("%s may reset the password", name)
			} else if !errors.Is(err, core.ErrUnauthorized) && !errors.Is(err, core.ErrForbidden) {
				t.Errorf("%s: got %v, want unauthorized or forbidden", name, err)
			}
		}
		// Nothing changed: the old password still works.
		if _, err := e.Service.Login(ctx, core.LoginInput{Email: e.Email, Password: e.Password}); err != nil {
			t.Fatalf("old password stopped working: %v", err)
		}
	})
	t.Run("policy", func(t *testing.T) {
		e := coretest.NewEnv(t)
		err := e.Service.ResetOperatorPassword(e.System(), "short")
		if fe := core.FieldErrors(err); len(fe) != 1 || fe[0].Field != "new_password" {
			t.Fatalf("got %v, want a validation error on new_password", err)
		}
	})
	t.Run("reset", func(t *testing.T) {
		e := coretest.NewEnv(t)
		other, err := e.Service.Login(ctx, core.LoginInput{Email: e.Email, Password: e.Password})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Service.ResetOperatorPassword(e.System(), "a brand new passphrase"); err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{e.Token, other.Token} {
			if _, err := e.Service.AuthenticateSession(ctx, tok); !errors.Is(err, core.ErrUnauthorized) {
				t.Errorf("a session survived the reset: %v", err)
			}
		}
		if _, err := e.Service.Login(ctx, core.LoginInput{Email: e.Email, Password: e.Password}); err == nil {
			t.Error("the old password still works")
		}
		if _, err := e.Service.Login(ctx, core.LoginInput{Email: e.Email, Password: "a brand new passphrase"}); err != nil {
			t.Errorf("the new password does not work: %v", err)
		}
	})
}
