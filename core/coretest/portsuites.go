package coretest

import (
	"context"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/core"
)

// AddressVector is a known-good derivation for [RunAddressSourceSuite]:
// the address a real wallet shows for an xpub, path and index.
type AddressVector struct {
	XPub           string
	DerivationPath string
	Index          uint32
	// Address is the expected address; compared case-insensitively.
	Address string
}

// RunAddressSourceSuite checks a [core.AddressSource] against the port's
// contract. valid is a wallet configuration the source must accept;
// vectors are known-good derivations (supply at least one taken from a
// real wallet — the suite cannot know the right answers by itself).
func RunAddressSourceSuite(t *testing.T, src core.AddressSource, valid core.WalletSettings, vectors []AddressVector) {
	t.Helper()
	ctx := context.Background()

	t.Run("WellFormed", func(t *testing.T) {
		for _, i := range []uint32{0, 1, 2, 1000, 1<<31 - 1} {
			addr, err := src.DeriveAddress(ctx, valid, i)
			if err != nil {
				t.Fatalf("index %d: %v", i, err)
			}
			if _, err := core.NormalizeAddress(addr); err != nil {
				t.Fatalf("index %d: %q is not a 0x-prefixed 20-byte address", i, addr)
			}
		}
	})

	t.Run("Deterministic", func(t *testing.T) {
		for i := range uint32(5) {
			a, err1 := src.DeriveAddress(ctx, valid, i)
			b, err2 := src.DeriveAddress(ctx, valid, i)
			if err1 != nil || err2 != nil || a != b {
				t.Fatalf("index %d: %q (%v) vs %q (%v)", i, a, err1, b, err2)
			}
		}
	})

	t.Run("DistinctPerIndex", func(t *testing.T) {
		seen := map[string]uint32{}
		for i := range uint32(200) {
			addr, err := src.DeriveAddress(ctx, valid, i)
			if err != nil {
				t.Fatalf("index %d: %v", i, err)
			}
			key := strings.ToLower(addr)
			if j, dup := seen[key]; dup {
				t.Fatalf("indexes %d and %d derive the same address %s", j, i, addr)
			}
			seen[key] = i
		}
	})

	t.Run("IgnoresNextAddressIndex", func(t *testing.T) {
		other := valid
		other.NextAddressIndex += 12345
		a, _ := src.DeriveAddress(ctx, valid, 7)
		b, _ := src.DeriveAddress(ctx, other, 7)
		if a != b {
			t.Fatalf("the address must depend only on key, path and index: %s vs %s", a, b)
		}
	})

	t.Run("RejectsUnusableKeys", func(t *testing.T) {
		for name, xpub := range map[string]string{
			"empty":        "",
			"garbage":      "definitely not a key",
			"address":      "0x7a3f9c2e41b8d05a6e19c4f2b3d8a7e6c5f41b92",
			"truncated":    valid.XPub[:len(valid.XPub)/2],
			"bad checksum": valid.XPub[:len(valid.XPub)-1] + flip(valid.XPub[len(valid.XPub)-1]),
			// BIP-32 test vector 1 master private key: private keys must be refused.
			"private key": "xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi",
		} {
			w := valid
			w.XPub = xpub
			if addr, err := src.DeriveAddress(ctx, w, 0); err == nil {
				t.Errorf("%s: derived %s, want an error", name, addr)
			}
		}
	})

	t.Run("Vectors", func(t *testing.T) {
		if len(vectors) == 0 {
			t.Skip("no known-good vectors supplied")
		}
		for _, v := range vectors {
			got, err := src.DeriveAddress(ctx, core.WalletSettings{XPub: v.XPub, DerivationPath: v.DerivationPath}, v.Index)
			if err != nil {
				t.Errorf("%s/%d: %v", v.DerivationPath, v.Index, err)
				continue
			}
			if !strings.EqualFold(got, v.Address) {
				t.Errorf("%s/%d: got %s, want %s", v.DerivationPath, v.Index, got, v.Address)
			}
		}
	})
}

// flip returns a different base58 character.
func flip(c byte) string {
	if c == '2' {
		return "3"
	}
	return "2"
}

// RunPasswordHasherSuite checks a [core.PasswordHasher] against the port's
// contract.
func RunPasswordHasherSuite(t *testing.T, h core.PasswordHasher) {
	t.Helper()
	const password = "correct horse battery staple"

	hash, err := h.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if hash == "" || strings.Contains(hash, password) {
		t.Fatalf("hash %q is empty or contains the password", hash)
	}
	ok, err := h.Verify(hash, password)
	if err != nil || !ok {
		t.Fatalf("Verify(correct) = %v, %v", ok, err)
	}
	for _, wrong := range []string{"", "correct horse battery stapl", password + " ", strings.ToUpper(password)} {
		ok, err := h.Verify(hash, wrong)
		if err != nil || ok {
			t.Errorf("Verify(%q) = %v, %v; want false, nil", wrong, ok, err)
		}
	}

	// Salted: hashing twice gives different hashes that both verify.
	again, err := h.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if again == hash {
		t.Errorf("two hashes of one password are identical: missing salt")
	}
	if ok, err := h.Verify(again, password); err != nil || !ok {
		t.Errorf("Verify(second hash) = %v, %v", ok, err)
	}

	// Hashes in an unknown format are errors, not silent mismatches; a
	// damaged hash must at least never verify.
	for _, bad := range []string{"", "plaintext"} {
		if ok, err := h.Verify(bad, password); ok || err == nil {
			t.Errorf("Verify(hash %q) = %v, %v; want false and an error", bad, ok, err)
		}
	}
	if ok, _ := h.Verify(hash[:len(hash)/2], password); ok {
		t.Errorf("a truncated hash verifies")
	}

	// Long and non-ASCII passwords work and are not truncated.
	long := strings.Repeat("pässwörd-", 40)
	hash, err = h.Hash(long)
	if err != nil {
		t.Fatalf("Hash(long): %v", err)
	}
	if ok, _ := h.Verify(hash, long); !ok {
		t.Errorf("long password does not verify")
	}
	if ok, _ := h.Verify(hash, long[:len(long)-1]); ok {
		t.Errorf("long password is truncated: a shorter one verifies")
	}
}
