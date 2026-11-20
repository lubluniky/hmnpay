package wallet_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
	"github.com/lubluniky/hmnpay/internal/wallet"
	"github.com/lubluniky/hmnpay/internal/wallet/hd"
)

// Extended public keys of the Hardhat/Anvil development mnemonic
// "test test test test test test test test test test test junk", as derived
// (and cross-checked against the official BIP32 vectors) in package hd's
// tests. Public test material; never fund it.
const (
	hardhatAccountXpub = "xpub6Ce9NcJvTk36xtLSrJLZqE7wtgA5deCeYs7rSQtreh4cj6ByPtrg9sD7V2FNFLPnf8heNP3FGkeV9qwfzvZNSd54JoNXVsXFYSYwHsnJxqP" // m/44'/60'/0'
	hardhatChainXpub   = "xpub6DyUKdwoLWmUJ4Tn9Bbsdtx7B5Ws18mEN19e5HT52ikE53FiUheSQXrZUNPovqfyKmw4579A1Mm3GXXKM39N64uooBfJ4tNAzFsEbodRTx4" // m/44'/60'/0'/0
	hardhatMasterXpub  = "xpub661MyMwAqRbcGCHqYL6cunEAC4sjobr4oEsADYNVSpvM1oCFfV98EVtMuHVmKomD5EWqhYwUPCkQdrti7hUGbmxaoTGLSkzhtBmR5tk9Jtu" // m
)

// The accounts every `hardhat node` and `anvil` prints: m/44'/60'/0'/0/n.
var hardhatAccounts = []string{
	"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
	"0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
	"0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",
}

// The same seed under the Ledger-legacy layout m/44'/60'/0'/n (checked in
// package hd against an independent private-key derivation).
var hardhatLegacy = []string{
	"0x1e59ce931B4CFea3fe4B875411e280e173cB7A9C",
	"0xc89D42189f0450C2b2c3c61f58Ec5d628176A1E7",
	"0x318b469BBa396AEc2C60342F9441be36A1945174",
}

func TestAddressSourceSuite(t *testing.T) {
	var vectors []coretest.AddressVector
	for i, want := range hardhatAccounts {
		for _, path := range []string{"m/44'/60'/0'/0", "m/44'/60'/0'/0/n", "m/44h/60h/0h/0", "m/44H/60H/0H/0/n", " m/44'/60'/0'/0 "} {
			vectors = append(vectors, coretest.AddressVector{XPub: hardhatAccountXpub, DerivationPath: path, Index: uint32(i), Address: want})
			// A wallet that exports the external-chain key instead of the
			// account key leads to the same addresses.
			vectors = append(vectors, coretest.AddressVector{XPub: hardhatChainXpub, DerivationPath: path, Index: uint32(i), Address: want})
		}
	}
	for i, want := range hardhatLegacy {
		for _, path := range []string{"m/44'/60'/0'", "m/44'/60'/0'/n", "m/44h/60h/0h/n"} {
			vectors = append(vectors, coretest.AddressVector{XPub: hardhatAccountXpub, DerivationPath: path, Index: uint32(i), Address: want})
		}
	}
	for _, valid := range []core.WalletSettings{
		{XPub: hardhatAccountXpub, DerivationPath: core.DefaultDerivationPath},
		{XPub: hardhatAccountXpub, DerivationPath: wallet.PathBIP44},
		{XPub: hardhatAccountXpub, DerivationPath: wallet.PathLedgerLegacy},
		{XPub: hardhatChainXpub, DerivationPath: core.DefaultDerivationPath},
	} {
		t.Run(valid.DerivationPath, func(t *testing.T) {
			coretest.RunAddressSourceSuite(t, wallet.NewAddressSource(), valid, vectors)
		})
	}
}

func TestParseDerivationPath(t *testing.T) {
	good := map[string]hd.Layout{
		"m/44'/60'/0'/0":   hd.LayoutBIP44,
		"m/44'/60'/0'/0/n": hd.LayoutBIP44,
		"m/44'/60'/0'":     hd.LayoutLedgerLegacy,
		"m/44'/60'/0'/n":   hd.LayoutLedgerLegacy,
		"m/44h/60H/0'/0":   hd.LayoutBIP44,
		"m/44'/60'/7'/0/n": hd.LayoutBIP44, // another account of the same wallet
		"m/44'/60'/7'":     hd.LayoutLedgerLegacy,
	}
	for path, want := range good {
		got, err := wallet.ParseDerivationPath(path)
		if err != nil || got != want {
			t.Errorf("ParseDerivationPath(%q) = %v, %v; want %v", path, got, err, want)
		}
	}
	if wallet.PathBIP44 != hd.LayoutBIP44.String() || wallet.PathLedgerLegacy != hd.LayoutLedgerLegacy.String() {
		t.Error("path constants drifted from the hd layouts")
	}
	bad := []string{
		"", "m", "n", "m/n", "44'/60'/0'/0", "/44'/60'/0'/0",
		"m/44'/60'", "m/44'/60'/n",
		"m/44'/60'/0'/0/0",   // an address path, not a template
		"m/44'/60'/0'/0/0/n", // too deep
		"m/44'/60'/0'/1",     // the change chain
		"m/44'/60'/0'/1/n",
		"m/44'/60'/0'/0'",   // hardened below the account
		"m/44'/60'/0'/0'/n", // hardened below the account
		"m/44'/60'/n'/0/0",  // Ledger Live: one hardened account per address
		"m/44'/60'/0/0",     // account not hardened
		"m/44/60'/0'/0",     // purpose not hardened
		"m/84'/60'/0'/0",    // other purpose
		"m/44'/0'/0'/0",     // Bitcoin's coin type
		"m/44'/61'/0'/0",    // Ethereum Classic
		"m/44'/60'/0'/0/n/n",
		"m/44'/60'/0'//n", "m/44'/60'/+0'/0", "m/44'/60'/-0'/0", "m/44'/60'/0x0'/0",
		"m/44'/60'/4294967296'/0", "m/44'/60'/2147483648'/0",
		"M/44'/60'/0'/0", "m\\44'\\60'\\0'\\0", "m/44'/60'/0'/0 /n",
	}
	for _, path := range bad {
		if l, err := wallet.ParseDerivationPath(path); !errors.Is(err, wallet.ErrUnsupportedPath) {
			t.Errorf("ParseDerivationPath(%q) = %v, %v; want ErrUnsupportedPath", path, l, err)
		}
	}
}

func TestDeriveAddressErrors(t *testing.T) {
	ctx := context.Background()
	src := wallet.NewAddressSource()
	// BIP-32 test vector 1 master private key.
	const xprv = "xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi"
	tests := []struct {
		name  string
		w     core.WalletSettings
		index uint32
		want  error
	}{
		{"private key", core.WalletSettings{XPub: xprv, DerivationPath: wallet.PathBIP44}, 0, hd.ErrPrivateKey},
		{"seed phrase", core.WalletSettings{XPub: "test test test test test test test test test test test junk", DerivationPath: wallet.PathBIP44}, 0, hd.ErrSecret},
		{"bad checksum", core.WalletSettings{XPub: hardhatAccountXpub[:len(hardhatAccountXpub)-1] + "2", DerivationPath: wallet.PathBIP44}, 0, hd.ErrChecksum},
		{"master key (needs hardened steps)", core.WalletSettings{XPub: hardhatMasterXpub, DerivationPath: wallet.PathBIP44}, 0, hd.ErrLayoutDepth},
		{"chain key under ledger legacy", core.WalletSettings{XPub: hardhatChainXpub, DerivationPath: wallet.PathLedgerLegacy}, 0, hd.ErrLayoutDepth},
		{"unsupported path", core.WalletSettings{XPub: hardhatAccountXpub, DerivationPath: "m/84'/0'/0'/0"}, 0, wallet.ErrUnsupportedPath},
		{"empty path", core.WalletSettings{XPub: hardhatAccountXpub}, 0, wallet.ErrUnsupportedPath},
		{"index 2^31", core.WalletSettings{XPub: hardhatAccountXpub, DerivationPath: wallet.PathBIP44}, 1 << 31, hd.ErrIndexRange},
	}
	for _, tc := range tests {
		addr, err := src.DeriveAddress(ctx, tc.w, tc.index)
		if !errors.Is(err, tc.want) || addr != "" {
			t.Errorf("%s: got %q, %v; want %v", tc.name, addr, err, tc.want)
			continue
		}
		// Nothing of the key may be echoed.
		for i := 0; i+8 <= len(tc.w.XPub); i++ {
			if strings.Contains(err.Error(), tc.w.XPub[i:i+8]) {
				t.Errorf("%s: error contains key material: %v", tc.name, err)
				break
			}
		}
	}
	// A master xpub is refused with both reasons, so the UI can explain it.
	_, err := src.DeriveAddress(ctx, core.WalletSettings{XPub: hardhatMasterXpub, DerivationPath: wallet.PathBIP44}, 0)
	if !errors.Is(err, hd.ErrHardened) {
		t.Errorf("master key: %v", err)
	}
}

// TestCoreUsesTheSource runs the real core on the real address source: the
// setup wizard's preview and the first payments show the wallet's addresses,
// and a private key is refused before anything is stored.
func TestCoreUsesTheSource(t *testing.T) {
	env := coretest.NewEnv(t, coretest.WithoutSetup(), coretest.WithConfig(func(c *core.Config) {
		c.Addresses = wallet.NewAddressSource()
	}))
	ctx := context.Background()

	preview, err := env.Service.PreviewAddresses(ctx, core.PreviewAddressesInput{XPub: hardhatAccountXpub, DerivationPath: "m/44'/60'/0'/0", Count: 3})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range hardhatAccounts {
		if !strings.EqualFold(preview[i].Address, want) {
			t.Fatalf("preview[%d] = %s, want %s", i, preview[i].Address, want)
		}
	}
	if _, err := env.Service.PreviewAddresses(ctx, core.PreviewAddressesInput{XPub: hardhatMasterXpub}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("master xpub in preview: %v", err)
	}

	in := env.SetupInput()
	in.XPub, in.DerivationPath = hardhatAccountXpub, ""
	if _, err := env.Service.CompleteSetup(ctx, in); err != nil {
		t.Fatal(err)
	}
	env.Head(1) // the chain watcher's first processed block
	for i, want := range hardhatAccounts {
		p := env.CreatePayment("10")
		if !strings.EqualFold(p.Address, want) {
			t.Fatalf("payment %d address = %s, want %s", i, p.Address, want)
		}
	}
}

func TestConcurrentUseAndCache(t *testing.T) {
	ctx := context.Background()
	src := wallet.NewAddressSource()
	var wg sync.WaitGroup
	results := make([][]string, 16)
	for g := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := uint32(0); i < 50; i++ {
				// Rotate through more wallets than the cache holds.
				path := wallet.PathBIP44
				if (int(i)+g)%3 == 0 {
					path = "m/44'/60'/" + string(rune('0'+(int(i)+g)%10)) + "'/0/n"
				}
				a, err := src.DeriveAddress(ctx, core.WalletSettings{XPub: hardhatAccountXpub, DerivationPath: path}, i%5)
				if err != nil {
					t.Error(err)
					return
				}
				results[g] = append(results[g], a)
			}
		}()
	}
	wg.Wait()
	for g := range results {
		for i, a := range results[g] {
			if want := strings.ToLower([]string{hardhatAccounts[0], hardhatAccounts[1], hardhatAccounts[2], "", ""}[i%5]); want != "" && a != want {
				t.Fatalf("goroutine %d call %d: %s, want %s", g, i, a, want)
			}
		}
	}
}

func BenchmarkDeriveAddress(b *testing.B) {
	ctx := context.Background()
	src := wallet.NewAddressSource()
	w := core.WalletSettings{XPub: hardhatAccountXpub, DerivationPath: wallet.PathBIP44}
	for i := 0; b.Loop(); i++ {
		if _, err := src.DeriveAddress(ctx, w, uint32(i)&0xffff); err != nil {
			b.Fatal(err)
		}
	}
}
