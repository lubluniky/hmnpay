package hd

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
)

// hardhatMnemonic is the well-known development mnemonic of Hardhat, Anvil
// and Foundry. Its first accounts are public knowledge, which makes it a good
// end-to-end vector. It must obviously never hold real funds.
const hardhatMnemonic = "test test test test test test test test test test test junk"

// hardhatAccounts are accounts #0..#2 of that mnemonic at m/44'/60'/0'/0/n,
// as printed by `npx hardhat node` / `anvil`.
var hardhatAccounts = []string{
	"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
	"0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
	"0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",
}

// TestBIP32VectorsFromSeed validates the test-only private derivation helper
// against the official vectors (seed -> every xpub and xprv in the chain).
// Everything below that uses the helper rests on this.
func TestBIP32VectorsFromSeed(t *testing.T) {
	for _, v := range bip32Vectors {
		seed, err := hex.DecodeString(v.seed)
		if err != nil {
			t.Fatal(err)
		}
		master := testMaster(t, seed)
		for _, step := range v.chain {
			k := master.derive(t, strings.ReplaceAll(step.path, "H", "'"))
			if got := k.xpub(); got != step.xpub {
				t.Errorf("%s %s: xpub\n got %s\nwant %s", v.name, step.path, got, step.xpub)
			}
			if got := k.xprv(); got != step.xprv {
				t.Errorf("%s %s: xprv mismatch", v.name, step.path)
			}
		}
	}
}

// TestBIP32VectorsParseAndSerialize: every official xpub parses, exposes the
// expected fixed fields and serialises back byte-for-byte (vectors 3 and 4
// exist specifically to catch dropped leading zeros).
func TestBIP32VectorsParseAndSerialize(t *testing.T) {
	for _, v := range bip32Vectors {
		for _, step := range v.chain {
			k, err := ParseXpub(step.xpub)
			if err != nil {
				t.Fatalf("%s %s: ParseXpub: %v", v.name, step.path, err)
			}
			if got := k.Serialize(); got != step.xpub {
				t.Errorf("%s %s: Serialize\n got %s\nwant %s", v.name, step.path, got, step.xpub)
			}
			indexes := parseTestPath(t, strings.ReplaceAll(step.path, "H", "'"))
			if int(k.Depth()) != len(indexes) {
				t.Errorf("%s %s: depth = %d", v.name, step.path, k.Depth())
			}
			wantChild := uint32(0)
			if len(indexes) > 0 {
				wantChild = indexes[len(indexes)-1]
			}
			if k.ChildNumber() != wantChild {
				t.Errorf("%s %s: child number = %d, want %d", v.name, step.path, k.ChildNumber(), wantChild)
			}
			if k.IsTestnet() {
				t.Errorf("%s %s: reported as testnet", v.name, step.path)
			}
		}
	}
}

// TestBIP32VectorsPublicDerivation: for every non-hardened step in the
// official chains, CKDpub of the parent xpub must produce exactly the child
// xpub of the vector (key, chain code, fingerprint, depth, index). For every
// hardened step, derivation from the xpub must be refused.
func TestBIP32VectorsPublicDerivation(t *testing.T) {
	publicSteps, hardenedSteps := 0, 0
	for _, v := range bip32Vectors {
		for i := 1; i < len(v.chain); i++ {
			parent, child := v.chain[i-1], v.chain[i]
			indexes := parseTestPath(t, strings.ReplaceAll(child.path, "H", "'"))
			index := indexes[len(indexes)-1]

			pk, err := ParseXpub(parent.xpub)
			if err != nil {
				t.Fatal(err)
			}
			got, err := pk.Child(index)
			if index >= HardenedOffset {
				hardenedSteps++
				if !errors.Is(err, ErrHardened) {
					t.Errorf("%s %s: hardened child err = %v, want ErrHardened", v.name, child.path, err)
				}
				continue
			}
			publicSteps++
			if err != nil {
				t.Fatalf("%s %s: Child: %v", v.name, child.path, err)
			}
			if got.Serialize() != child.xpub {
				t.Errorf("%s %s: CKDpub\n got %s\nwant %s", v.name, child.path, got.Serialize(), child.xpub)
			}
			ck, _ := ParseXpub(child.xpub)
			if ck.ParentFingerprint() != pk.Fingerprint() {
				t.Errorf("%s %s: parent fingerprint mismatch", v.name, child.path)
			}
		}
	}
	// Guard against the table silently shrinking.
	if publicSteps != 6 || hardenedSteps != 7 {
		t.Fatalf("covered %d public and %d hardened steps, want 6 and 7", publicSteps, hardenedSteps)
	}

	// Multi-step Derive across the two consecutive non-hardened steps of
	// vector 1: m/0H/1/2H -> /2/1000000000.
	base, _ := ParseXpub(bip32Vectors[0].chain[3].xpub)
	leaf, err := base.Derive(Path{2, 1000000000})
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Serialize() != bip32Vectors[0].chain[5].xpub {
		t.Fatal("Derive over two steps does not match vector 1")
	}
	if _, err := base.Derive(Path{2, HardenedOffset}); !errors.Is(err, ErrHardened) {
		t.Fatalf("Derive with hardened step: %v", err)
	}
}

// TestBIP32InvalidKeys is test vector 5.
func TestBIP32InvalidKeys(t *testing.T) {
	for _, tc := range bip32InvalidKeys {
		_, err := ParseXpub(tc.key)
		if err == nil {
			t.Errorf("accepted invalid key (%s)", tc.reason)
			continue
		}
		var want error
		switch {
		case strings.HasPrefix(tc.key, "xprv"):
			// Whatever else is wrong with it, a private key is reported
			// as a private key.
			want = ErrPrivateKey
		case strings.Contains(tc.reason, "pubkey version / prvkey mismatch"):
			want = ErrPrivateKey // private key bytes under the xpub version
		case strings.Contains(tc.reason, "invalid pubkey"):
			want = ErrInvalidPublicKey
		case strings.Contains(tc.reason, "zero depth"):
			want = ErrInvalidKey
		case strings.Contains(tc.reason, "unknown extended key version"):
			// One of the two is a private-key payload under an unknown
			// version, the other a public key: both must fail, each as
			// ErrPrivateKey or ErrVersion.
			if !errors.Is(err, ErrVersion) && !errors.Is(err, ErrPrivateKey) {
				t.Errorf("%s: err = %v", tc.reason, err)
			}
			continue
		default:
			t.Fatalf("unclassified vector: %s", tc.reason)
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", tc.reason, err, want)
		}
	}
}

// TestHardhatMnemonicEndToEnd: mnemonic -> BIP39 seed -> account xpub (test
// code, private derivation) -> the package derives the publicly known
// addresses from that xpub alone.
func TestHardhatMnemonicEndToEnd(t *testing.T) {
	master := testMaster(t, bip39Seed(t, hardhatMnemonic, ""))

	t.Run("account xpub, standard layout", func(t *testing.T) {
		key, err := ParseXpub(master.derive(t, "m/44'/60'/0'").xpub())
		if err != nil {
			t.Fatal(err)
		}
		if key.Depth() != 3 || key.ChildNumber() != HardenedOffset {
			t.Fatalf("unexpected account key: depth %d child %d", key.Depth(), key.ChildNumber())
		}
		scheme, err := SchemeFor(LayoutBIP44, key)
		if err != nil {
			t.Fatal(err)
		}
		if scheme.String() != "0/n" {
			t.Fatalf("scheme = %s, want 0/n", scheme)
		}
		d, err := NewDeriver(key, scheme)
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range hardhatAccounts {
			got, err := d.Address(uint32(i))
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != want {
				t.Errorf("address %d = %s, want %s", i, got, want)
			}
		}
		rng, err := d.DeriveRange(0, 3)
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range hardhatAccounts {
			if rng[i].Address.String() != want || rng[i].Index != uint32(i) || rng[i].Path != fmt.Sprintf("0/%d", i) {
				t.Errorf("DeriveRange[%d] = %+v", i, rng[i])
			}
		}
	})

	t.Run("external-chain xpub, standard layout", func(t *testing.T) {
		// Some tools export m/44'/60'/0'/0 ("BIP32 extended public key").
		key, err := ParseXpub(master.derive(t, "m/44'/60'/0'/0").xpub())
		if err != nil {
			t.Fatal(err)
		}
		scheme, err := SchemeFor(LayoutBIP44, key)
		if err != nil {
			t.Fatal(err)
		}
		if scheme != SchemeDirect {
			t.Fatalf("scheme = %s, want n", scheme)
		}
		d, _ := NewDeriver(key, scheme)
		for i, want := range hardhatAccounts {
			if got, _ := d.Address(uint32(i)); got.String() != want {
				t.Errorf("address %d = %s, want %s", i, got, want)
			}
		}
	})

	t.Run("ledger legacy layout", func(t *testing.T) {
		// m/44'/60'/0'/n. Expected values come from the independent
		// private derivation: private key -> public key -> address.
		account := master.derive(t, "m/44'/60'/0'")
		key, err := ParseXpub(account.xpub())
		if err != nil {
			t.Fatal(err)
		}
		scheme, err := SchemeFor(LayoutLedgerLegacy, key)
		if err != nil {
			t.Fatal(err)
		}
		if scheme.String() != "n" {
			t.Fatalf("scheme = %s, want n", scheme)
		}
		d, _ := NewDeriver(key, scheme)
		seen := map[ethaddr.Address]bool{}
		for i := uint32(0); i < 5; i++ {
			want, err := ethaddr.FromUncompressedPublicKey(account.child(t, i).pubUncompressed())
			if err != nil {
				t.Fatal(err)
			}
			got, err := d.Address(i)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("legacy address %d = %s, want %s", i, got, want)
			}
			seen[got] = true
		}
		// m/44'/60'/0'/0 is the *parent* of the standard addresses, so the
		// two layouts must not collide on these indexes.
		for _, a := range hardhatAccounts {
			if seen[ethaddr.MustParse(a)] {
				t.Errorf("layouts collide on %s", a)
			}
		}
	})
}

// TestPublicDerivationMatchesPrivate is a property test over many indexes and
// both layouts: deriving the child public key from the xpub must give the
// public key of the privately derived child (N(CKDpriv) == CKDpub(N)).
func TestPublicDerivationMatchesPrivate(t *testing.T) {
	master := testMaster(t, bip39Seed(t, hardhatMnemonic, "some passphrase"))
	account := master.derive(t, "m/44'/60'/7'")
	key, err := ParseXpub(account.xpub())
	if err != nil {
		t.Fatal(err)
	}
	chain := account.child(t, 0)
	std, _ := NewDeriver(key, SchemeExternalChain)
	for _, i := range []uint32{0, 1, 2, 9, 255, 256, 65535, 1 << 20, HardenedOffset - 1} {
		want, _ := ethaddr.FromUncompressedPublicKey(chain.child(t, i).pubUncompressed())
		got, err := std.Address(i)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("index %d: %s != %s", i, got, want)
		}
		child, err := key.Derive(Path{0, i})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(child.PublicKey(), chain.child(t, i).pubCompressed()) {
			t.Errorf("index %d: public key mismatch", i)
		}
		if child.Serialize() != chain.child(t, i).xpub() {
			t.Errorf("index %d: serialised child differs", i)
		}
	}
}

func TestParseRejectsSecrets(t *testing.T) {
	master := testMaster(t, bip39Seed(t, hardhatMnemonic, ""))
	account := master.derive(t, "m/44'/60'/0'")
	xprv := account.xprv()
	tprv := testBase58Check(account.payload(0x04358394, true))
	zprv := testBase58Check(account.payload(0x04B2430C, true))
	if !strings.HasPrefix(xprv, "xprv") || !strings.HasPrefix(tprv, "tprv") || !strings.HasPrefix(zprv, "zprv") {
		t.Fatalf("test encoder produced unexpected prefixes: %s %s %s", xprv[:4], tprv[:4], zprv[:4])
	}
	// A private payload hiding behind the xpub version bytes.
	disguised := testBase58Check(account.payload(0x0488B21E, true))
	if !strings.HasPrefix(disguised, "xpub") {
		t.Fatal("disguised key should carry the xpub prefix")
	}

	private := map[string]string{
		"xprv":                    xprv,
		"xprv with whitespace":    "  " + xprv + "\n",
		"xprv with a typo":        xprv[:50] + flipBase58(xprv[50]) + xprv[51:],
		"xprv truncated":          xprv[:90],
		"xprv with garbage added": xprv + "0OIl",
		"tprv":                    tprv,
		"zprv (SLIP-132)":         zprv,
		"private key as xpub":     disguised,
		"very long xprv-like":     "xprv" + strings.Repeat("A", 500),
	}
	for name, in := range private {
		_, err := ParseXpub(in)
		if !errors.Is(err, ErrPrivateKey) {
			t.Errorf("%s: err = %v, want ErrPrivateKey", name, err)
		}
		assertNoLeak(t, name, in, err)
	}

	rawKey := hex.EncodeToString(account.keyBytes())
	secrets := map[string]string{
		"mnemonic":            hardhatMnemonic,
		"mnemonic multi-line": strings.ReplaceAll(hardhatMnemonic, " ", "\n"),
		"24 words":            strings.TrimSpace(strings.Repeat("abandon ", 23)) + " art",
		"raw hex key":         rawKey,
		"raw hex key 0x":      "0x" + rawKey,
		"raw hex key upper":   strings.ToUpper(rawKey),
	}
	for name, in := range secrets {
		_, err := ParseXpub(in)
		if !errors.Is(err, ErrSecret) {
			t.Errorf("%s: err = %v, want ErrSecret", name, err)
		}
		if errors.Is(err, ErrPrivateKey) {
			t.Errorf("%s: also matches ErrPrivateKey", name)
		}
		assertNoLeak(t, name, in, err)
	}
}

func TestParseMalformed(t *testing.T) {
	master := testMaster(t, bip39Seed(t, hardhatMnemonic, ""))
	account := master.derive(t, "m/44'/60'/0'")
	good := account.xpub()
	if _, err := ParseXpub(good); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseXpub("\t " + good + " \r\n"); err != nil {
		t.Fatalf("surrounding whitespace: %v", err)
	}

	// A syntactically perfect key whose X coordinate is not on the curve.
	// x = 5 has no square root for y^2 = x^3 + 7 on secp256k1.
	offCurve := account.payload(0x0488B21E, false)
	copy(offCurve[45:], append([]byte{0x02}, make([]byte, 31)...))
	offCurve[77] = 5
	// X coordinate >= field prime.
	overflow := account.payload(0x0488B21E, false)
	copy(overflow[46:], bytes.Repeat([]byte{0xff}, 32))
	// Uncompressed-style prefix.
	prefix04 := account.payload(0x0488B21E, false)
	prefix04[45] = 0x04

	payload := account.payload(0x0488B21E, false)
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", ErrEncoding},
		{"whitespace only", " \n ", ErrEncoding},
		{"bad checksum (last char)", good[:len(good)-1] + flipBase58(good[len(good)-1]), ErrChecksum},
		{"bad checksum (middle char)", good[:60] + flipBase58(good[60]) + good[61:], ErrChecksum},
		{"truncated", good[:len(good)-1], ErrLength},
		{"truncated a lot", good[:20], ErrLength},
		{"one char too many", good + "1", ErrLength},
		{"payload one byte short", testBase58Check(payload[:77]), ErrLength},
		{"payload one byte long", testBase58Check(append(append([]byte{}, payload...), 0)), ErrLength},
		{"absurdly long", strings.Repeat("x", 5000), ErrLength},
		{"character 0", good[:30] + "0" + good[31:], ErrEncoding},
		{"character O", good[:30] + "O" + good[31:], ErrEncoding},
		{"character I", good[:30] + "I" + good[31:], ErrEncoding},
		{"character l", good[:30] + "l" + good[31:], ErrEncoding},
		{"non-ASCII", good[:30] + "é" + good[31:], ErrEncoding},
		{"inner space", good[:30] + " " + good[30:], ErrEncoding},
		{"an address", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266", ErrEncoding},
		{"point not on curve", testBase58Check(offCurve), ErrInvalidPublicKey},
		{"x coordinate out of range", testBase58Check(overflow), ErrInvalidPublicKey},
		{"uncompressed prefix", testBase58Check(prefix04), ErrInvalidPublicKey},
		{"ypub (SLIP-132)", testBase58Check(account.payload(0x049D7CB2, false)), ErrVersion},
		{"zpub (SLIP-132)", testBase58Check(account.payload(0x04B24746, false)), ErrVersion},
		{"zero version", testBase58Check(account.payload(0, false)), ErrVersion},
	}
	for _, tc := range tests {
		k, err := ParseXpub(tc.in)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if k != nil {
			t.Errorf("%s: returned a key together with an error", tc.name)
		}
		assertNoLeak(t, tc.name, tc.in, err)
	}

	// tpub is accepted and flagged.
	tk, err := ParseXpub(testBase58Check(account.payload(0x043587CF, false)))
	if err != nil || !tk.IsTestnet() {
		t.Fatalf("tpub: %v", err)
	}
	if !strings.HasPrefix(tk.Serialize(), "tpub") {
		t.Fatal("tpub does not serialise as tpub")
	}
}

// TestInvalidChildBranches drives the two BIP32 "invalid child" cases through
// childFromHMAC, since no real HMAC output reaches them.
func TestInvalidChildBranches(t *testing.T) {
	master := testMaster(t, bip39Seed(t, hardhatMnemonic, ""))
	account := master.derive(t, "m/44'/60'/0'")
	key, err := ParseXpub(account.xpub())
	if err != nil {
		t.Fatal(err)
	}
	fill := func(il *big.Int) (sum [64]byte) {
		il.FillBytes(sum[:32])
		for i := 32; i < 64; i++ {
			sum[i] = byte(i)
		}
		return sum
	}

	// Case 1: parse256(IL) >= n.
	for _, il := range []*big.Int{
		new(big.Int).Set(curveN),
		new(big.Int).Add(curveN, big.NewInt(1)),
		new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)),
	} {
		if _, err := key.childFromHMAC(0, fill(il)); !errors.Is(err, ErrInvalidChild) {
			t.Errorf("IL = %x: err = %v, want ErrInvalidChild", il, err)
		}
	}

	// Case 2: point(IL) + Kpar is the point at infinity, i.e. IL = n - kpar.
	il := new(big.Int).Sub(curveN, account.key)
	if _, err := key.childFromHMAC(0, fill(il)); !errors.Is(err, ErrInvalidChild) {
		t.Errorf("IL = n - k: err = %v, want ErrInvalidChild", err)
	}

	// Boundary: IL = n - 1 is valid, and equals private child k - 1.
	child, err := key.childFromHMAC(3, fill(new(big.Int).Sub(curveN, big.NewInt(1))))
	if err != nil {
		t.Fatalf("IL = n-1 rejected: %v", err)
	}
	want := testPriv{key: new(big.Int).Sub(account.key, big.NewInt(1))}
	if !bytes.Equal(child.PublicKey(), want.pubCompressed()) {
		t.Error("IL = n-1: wrong child key")
	}
	if child.Depth() != 4 || child.ChildNumber() != 3 || child.ParentFingerprint() != key.Fingerprint() {
		t.Error("IL = n-1: wrong child metadata")
	}

	// IL = 0 is valid per BIP32 (only IL >= n and infinity are excluded):
	// the child key equals the parent key.
	same, err := key.childFromHMAC(0, fill(new(big.Int)))
	if err != nil {
		t.Fatalf("IL = 0 rejected: %v", err)
	}
	if !bytes.Equal(same.PublicKey(), key.PublicKey()) {
		t.Error("IL = 0: child key differs from parent key")
	}
}

func TestSchemeFor(t *testing.T) {
	master := testMaster(t, bip39Seed(t, hardhatMnemonic, ""))
	keyAt := func(path string) *ExtendedKey {
		k, err := ParseXpub(master.derive(t, path).xpub())
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	tests := []struct {
		path     string
		layout   Layout
		want     string // scheme, or "" for an error
		hardened bool   // error must also match ErrHardened
	}{
		{"m", LayoutBIP44, "", true},
		{"m/44'", LayoutBIP44, "", true},
		{"m/44'/60'", LayoutBIP44, "", true},
		{"m/44'/60'/0'", LayoutBIP44, "0/n", false},
		{"m/44'/60'/1'", LayoutBIP44, "0/n", false}, // a different account works the same way
		{"m/44'/60'/0'/0", LayoutBIP44, "n", false},
		{"m/44'/60'/0'/1", LayoutBIP44, "", false}, // change chain
		{"m/44'/60'/0'/0'", LayoutBIP44, "", false},
		{"m/44'/60'/0'/0/0", LayoutBIP44, "", false}, // an address key
		{"m/44'/60'/0'/0/0/0", LayoutBIP44, "", false},
		{"m/44'/60'/0", LayoutBIP44, "", false}, // depth 3 but not hardened

		{"m", LayoutLedgerLegacy, "", true},
		{"m/44'/60'", LayoutLedgerLegacy, "", true},
		{"m/44'/60'/0'", LayoutLedgerLegacy, "n", false},
		{"m/44'/60'/0'/0", LayoutLedgerLegacy, "", false}, // that is address 0 itself
		{"m/44'/60'/0'/0/0", LayoutLedgerLegacy, "", false},
		{"m/44'/60'/0", LayoutLedgerLegacy, "", false},
	}
	for _, tc := range tests {
		got, err := SchemeFor(tc.layout, keyAt(tc.path))
		if tc.want == "" {
			if !errors.Is(err, ErrLayoutDepth) {
				t.Errorf("%s under %s: err = %v, want ErrLayoutDepth", tc.path, tc.layout, err)
			}
			if errors.Is(err, ErrHardened) != tc.hardened {
				t.Errorf("%s under %s: ErrHardened match = %v, want %v", tc.path, tc.layout, !tc.hardened, tc.hardened)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s under %s: %v", tc.path, tc.layout, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("%s under %s: scheme = %s, want %s", tc.path, tc.layout, got, tc.want)
		}
	}
	if _, err := SchemeFor(Layout(0), keyAt("m/44'/60'/0'")); err == nil {
		t.Error("unknown layout accepted")
	}
	if LayoutBIP44.String() != "m/44'/60'/0'/0/n" || LayoutLedgerLegacy.String() != "m/44'/60'/0'/n" {
		t.Error("Layout.String")
	}
}

func TestParseScheme(t *testing.T) {
	good := map[string]Path{
		"n":                                   nil,
		"0/n":                                 {0},
		"1/n":                                 {1},
		"0/0/n":                               {0, 0},
		"12/34/n":                             {12, 34},
		fmt.Sprintf("%d/n", HardenedOffset-1): {HardenedOffset - 1},
	}
	for in, branch := range good {
		s, err := ParseScheme(in)
		if err != nil {
			t.Errorf("ParseScheme(%q): %v", in, err)
			continue
		}
		if s.String() != in {
			t.Errorf("ParseScheme(%q).String() = %q", in, s)
		}
		if fmt.Sprint(s.Branch()) != fmt.Sprint(branch) {
			t.Errorf("ParseScheme(%q).Branch() = %v, want %v", in, s.Branch(), branch)
		}
		text, _ := s.MarshalText()
		var back Scheme
		if err := back.UnmarshalText(text); err != nil || back != s {
			t.Errorf("text round trip of %q failed", in)
		}
	}
	if s, _ := ParseScheme("n"); s != SchemeDirect {
		t.Error(`"n" is not SchemeDirect`)
	}
	if s, _ := ParseScheme("0/n"); s != SchemeExternalChain {
		t.Error(`"0/n" is not SchemeExternalChain`)
	}

	hardened := []string{"0'/n", "0h/n", "0H/n", "44'/60'/0'/0/n", "0/1'/n", "2147483648/n", "4294967295/n"}
	for _, in := range hardened {
		if _, err := ParseScheme(in); !errors.Is(err, ErrHardened) {
			t.Errorf("ParseScheme(%q) err = %v, want ErrHardened", in, err)
		}
	}
	invalid := []string{"", "0", "0/", "/n", "0//n", "n/0", "N", "0/N", "m/0/n", "m/44'/60'/0'/0/n", "m", "a/n", "-1/n", "+1/n",
		"0x10/n", "1.5/n", " 0/n", "0/n ", "4294967296/n", "99999999999999999999/n", "0/n/n", "1/2/3/4/5/6/7/8/9/n", "n'"}
	for _, in := range invalid {
		if _, err := ParseScheme(in); !errors.Is(err, ErrScheme) {
			t.Errorf("ParseScheme(%q) err = %v, want ErrScheme", in, err)
		}
	}
}

func TestDeriverBounds(t *testing.T) {
	master := testMaster(t, bip39Seed(t, hardhatMnemonic, ""))
	key, _ := ParseXpub(master.derive(t, "m/44'/60'/0'").xpub())
	d, err := NewDeriver(key, SchemeExternalChain)
	if err != nil {
		t.Fatal(err)
	}
	if d.Key() != key || d.Scheme() != SchemeExternalChain {
		t.Error("accessors")
	}
	if got := d.Path(7).String(); got != "0/7" {
		t.Errorf("Path(7) = %q", got)
	}
	if _, err := d.Address(HardenedOffset); !errors.Is(err, ErrIndexRange) {
		t.Errorf("Address(2^31): %v", err)
	}
	if _, err := d.Address(^uint32(0)); !errors.Is(err, ErrIndexRange) {
		t.Errorf("Address(2^32-1): %v", err)
	}
	if _, err := d.Address(HardenedOffset - 1); err != nil {
		t.Errorf("Address(2^31-1): %v", err)
	}
	if out, err := d.DeriveRange(5, 0); err != nil || len(out) != 0 {
		t.Errorf("empty range: %v %v", out, err)
	}
	if _, err := d.DeriveRange(0, MaxRange+1); err == nil {
		t.Error("range above MaxRange accepted")
	}
	if _, err := d.DeriveRange(HardenedOffset-2, 3); !errors.Is(err, ErrIndexRange) {
		t.Errorf("range crossing 2^31: %v", err)
	}
	if _, err := d.DeriveRange(HardenedOffset, 1); !errors.Is(err, ErrIndexRange) {
		t.Errorf("range starting at 2^31: %v", err)
	}
	last, err := d.DeriveRange(HardenedOffset-2, 2)
	if err != nil || len(last) != 2 || last[1].Index != HardenedOffset-1 {
		t.Errorf("last valid range: %v %v", last, err)
	}
	// Addresses within a range are distinct and match Address.
	rng, err := d.DeriveRange(10, 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[ethaddr.Address]bool{}
	for _, e := range rng {
		single, _ := d.Address(e.Index)
		if single != e.Address || seen[e.Address] {
			t.Fatalf("index %d inconsistent or duplicate", e.Index)
		}
		seen[e.Address] = true
	}
	if _, err := NewDeriver(nil, SchemeDirect); err == nil {
		t.Error("nil key accepted")
	}

	// Depth limit.
	deep := *key
	deep.depth = 255
	if _, err := deep.Child(0); !errors.Is(err, ErrDepth) {
		t.Errorf("child of depth-255 key: %v", err)
	}
}

// TestKeyIsNotPrintedByAccident: formatting or logging an ExtendedKey must
// not reveal the xpub, the chain code or the public key.
func TestKeyIsNotPrintedByAccident(t *testing.T) {
	master := testMaster(t, bip39Seed(t, hardhatMnemonic, ""))
	account := master.derive(t, "m/44'/60'/0'")
	xpub := account.xpub()
	key, _ := ParseXpub(xpub)

	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.Info("configured", "key", key)
	outputs := []string{
		fmt.Sprint(key), fmt.Sprintf("%v", key), fmt.Sprintf("%+v", key), fmt.Sprintf("%#v", key),
		key.String(), fmt.Sprintf("%q", key), fmt.Sprintf("%x", key), logged.String(),
	}
	chainHex := hex.EncodeToString(account.chain[:])
	pubHex := hex.EncodeToString(account.pubCompressed())
	for _, out := range outputs {
		if strings.Contains(out, xpub) || strings.Contains(out, xpub[:20]) ||
			strings.Contains(out, chainHex[:16]) || strings.Contains(out, pubHex[:16]) {
			t.Errorf("key material leaked into %q", out)
		}
	}
	want := fmt.Sprintf("xpub(fingerprint=%s depth=3 child=0')", key.FingerprintHex())
	if key.String() != want {
		t.Errorf("String() = %q, want %q", key.String(), want)
	}
	fp := account.fingerprint()
	if key.Fingerprint() != fp || key.FingerprintHex() != hex.EncodeToString(fp[:]) {
		t.Error("fingerprint differs from the independent implementation")
	}
}

// TestFingerprintVector: the fingerprint of BIP32 vector 1's master key is
// 3442193e (it appears as the parent fingerprint in every published
// decomposition of the vector's m/0H key).
func TestFingerprintVector(t *testing.T) {
	master, _ := ParseXpub(bip32Vectors[0].chain[0].xpub)
	if got := master.FingerprintHex(); got != "3442193e" {
		t.Fatalf("fingerprint = %s, want 3442193e", got)
	}
	child, _ := ParseXpub(bip32Vectors[0].chain[1].xpub)
	if child.ParentFingerprint() != master.Fingerprint() {
		t.Fatal("child's parent fingerprint differs from master fingerprint")
	}
	if master.ParentFingerprint() != [4]byte{} {
		t.Fatal("master has a parent fingerprint")
	}
}

func FuzzParseXpub(f *testing.F) {
	for _, v := range bip32Vectors {
		for _, step := range v.chain {
			f.Add(step.xpub)
			f.Add(step.xprv)
		}
	}
	for _, tc := range bip32InvalidKeys {
		f.Add(tc.key)
	}
	f.Add(hardhatMnemonic)
	f.Add("")
	f.Add("1111111111111111111111111111111111111111111111111111111111111111111111111111111111")
	f.Fuzz(func(t *testing.T, s string) {
		k, err := ParseXpub(s)
		if err != nil {
			if k != nil {
				t.Fatal("key returned with error")
			}
			if trimmed := strings.TrimSpace(s); len(trimmed) >= 12 && strings.Contains(err.Error(), trimmed) {
				t.Fatalf("error echoes the input: %v", err)
			}
			return
		}
		// Anything accepted must be a canonical extended public key.
		trimmed := strings.TrimSpace(s)
		if got := k.Serialize(); got != trimmed {
			t.Fatalf("accepted non-canonical encoding: %q -> %q", trimmed, got)
		}
		if strings.Contains(trimmed, "prv") && trimmed[1:4] == "prv" {
			t.Fatal("accepted a private key prefix")
		}
		if _, err := ParseXpub(k.Serialize()); err != nil {
			t.Fatalf("re-parse failed: %v", err)
		}
		// Derivation from any accepted key must not panic.
		if c, err := k.Child(0); err == nil {
			_ = c.Address()
		}
	})
}

func FuzzParseScheme(f *testing.F) {
	for _, s := range []string{"n", "0/n", "0'/n", "m/0/n", "1/2/3/n", "4294967296/n", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		sc, err := ParseScheme(s)
		if err != nil {
			return
		}
		if sc.String() != s {
			// Leading zeros are the only accepted non-canonical spelling.
			again, err := ParseScheme(sc.String())
			if err != nil || again != sc {
				t.Fatalf("ParseScheme(%q) = %q does not round-trip", s, sc)
			}
		}
		for _, index := range sc.Branch() {
			if index >= HardenedOffset {
				t.Fatalf("ParseScheme(%q) produced a hardened segment", s)
			}
		}
	})
}

// assertNoLeak fails if the error text contains any 8-character window of the
// rejected input (which may be a private key or a seed phrase).
func assertNoLeak(t *testing.T, name, input string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	in := strings.TrimSpace(input)
	for i := 0; i+8 <= len(in); i++ {
		if strings.Contains(msg, in[i:i+8]) {
			t.Errorf("%s: error text contains input fragment %q: %s", name, in[i:i+8], msg)
			return
		}
	}
}

// flipBase58 returns a different Base58 character.
func flipBase58(c byte) string {
	if c == 'a' {
		return "b"
	}
	return "a"
}
