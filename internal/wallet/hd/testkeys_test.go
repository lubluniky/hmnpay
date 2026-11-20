package hd

// Test-only private-key side of BIP32/BIP39.
//
// The package under test is public-key only by design. To check it against
// real-world vectors (a seed or a mnemonic on one side, addresses on the
// other) the tests need the private derivation path too, including hardened
// steps. It lives here, in a _test file, deliberately independent of the
// production code: its own scalar arithmetic (math/big), its own Base58Check
// encoder. It is itself validated against the official BIP32 vectors in
// TestBIP32VectorsFromSeed before anything else relies on it.

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"math/big"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	//lint:ignore SA1019 BIP32 key fingerprints are defined with RIPEMD-160.
	"golang.org/x/crypto/ripemd160"
)

var curveN, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)

// testPriv is an extended private key.
type testPriv struct {
	key      *big.Int
	chain    [32]byte
	depth    uint8
	parentFP [4]byte
	childNum uint32
}

func testMaster(t testing.TB, seed []byte) testPriv {
	t.Helper()
	mac := hmac.New(sha512.New, []byte("Bitcoin seed"))
	mac.Write(seed)
	sum := mac.Sum(nil)
	p := testPriv{key: new(big.Int).SetBytes(sum[:32])}
	copy(p.chain[:], sum[32:])
	if p.key.Sign() == 0 || p.key.Cmp(curveN) >= 0 {
		t.Fatal("invalid master key")
	}
	return p
}

// bip39Seed turns a mnemonic into a seed: PBKDF2-HMAC-SHA512, 2048 rounds,
// salt "mnemonic" + passphrase (BIP39).
func bip39Seed(t testing.TB, mnemonic, passphrase string) []byte {
	t.Helper()
	seed, err := pbkdf2.Key(sha512.New, mnemonic, []byte("mnemonic"+passphrase), 2048, 64)
	if err != nil {
		t.Fatal(err)
	}
	return seed
}

func (p testPriv) keyBytes() []byte {
	var b [32]byte
	p.key.FillBytes(b[:])
	return b[:]
}

func (p testPriv) pubCompressed() []byte {
	return secp256k1.PrivKeyFromBytes(p.keyBytes()).PubKey().SerializeCompressed()
}

func (p testPriv) pubUncompressed() []byte {
	return secp256k1.PrivKeyFromBytes(p.keyBytes()).PubKey().SerializeUncompressed()
}

func (p testPriv) fingerprint() [4]byte {
	sha := sha256.Sum256(p.pubCompressed())
	r := ripemd160.New()
	r.Write(sha[:])
	return [4]byte(r.Sum(nil)[:4])
}

// child implements CKDpriv for hardened and non-hardened indexes.
func (p testPriv) child(t testing.TB, index uint32) testPriv {
	t.Helper()
	mac := hmac.New(sha512.New, p.chain[:])
	if index >= 1<<31 {
		mac.Write([]byte{0})
		mac.Write(p.keyBytes())
	} else {
		mac.Write(p.pubCompressed())
	}
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], index)
	mac.Write(idx[:])
	sum := mac.Sum(nil)
	il := new(big.Int).SetBytes(sum[:32])
	if il.Cmp(curveN) >= 0 {
		t.Fatal("invalid child (IL >= n)")
	}
	c := testPriv{
		key:      il.Add(il, p.key).Mod(il, curveN),
		depth:    p.depth + 1,
		parentFP: p.fingerprint(),
		childNum: index,
	}
	if c.key.Sign() == 0 {
		t.Fatal("invalid child (key is zero)")
	}
	copy(c.chain[:], sum[32:])
	return c
}

// derive follows an absolute path such as "m/44'/60'/0'".
func (p testPriv) derive(t testing.TB, path string) testPriv {
	t.Helper()
	cur := p
	for _, index := range parseTestPath(t, path) {
		cur = cur.child(t, index)
	}
	return cur
}

// parseTestPath parses "m/0'/1/2H" (and the empty path "m").
func parseTestPath(t testing.TB, path string) []uint32 {
	t.Helper()
	parts := strings.Split(path, "/")
	if parts[0] != "m" {
		t.Fatalf("path %q must start with m", path)
	}
	var out []uint32
	for _, part := range parts[1:] {
		hardened := strings.HasSuffix(part, "'") || strings.HasSuffix(part, "H")
		part = strings.TrimRight(part, "'H")
		v, ok := new(big.Int).SetString(part, 10)
		if !ok || v.BitLen() > 31 {
			t.Fatalf("bad path segment %q", part)
		}
		index := uint32(v.Uint64())
		if hardened {
			index += 1 << 31
		}
		out = append(out, index)
	}
	return out
}

// payload returns the 78-byte serialisation with the given version, either of
// the public key (private=false) or of the private key.
func (p testPriv) payload(version uint32, private bool) []byte {
	b := make([]byte, 0, 78)
	b = binary.BigEndian.AppendUint32(b, version)
	b = append(b, p.depth)
	b = append(b, p.parentFP[:]...)
	b = binary.BigEndian.AppendUint32(b, p.childNum)
	b = append(b, p.chain[:]...)
	if private {
		b = append(b, 0)
		b = append(b, p.keyBytes()...)
	} else {
		b = append(b, p.pubCompressed()...)
	}
	return b
}

func (p testPriv) xpub() string { return testBase58Check(p.payload(0x0488B21E, false)) }
func (p testPriv) xprv() string { return testBase58Check(p.payload(0x0488ADE4, true)) }

// testBase58Check is an independent Base58Check encoder (schoolbook long
// division on the byte string, no math/big, no production code).
func testBase58Check(payload []byte) string {
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	data := append(append([]byte{}, payload...), second[:4]...)

	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	zeros := 0
	for zeros < len(data) && data[zeros] == 0 {
		zeros++
	}
	num := append([]byte{}, data[zeros:]...)
	var digits []byte
	for len(num) > 0 {
		rem := 0
		var quotient []byte
		for _, b := range num {
			acc := rem*256 + int(b)
			q := acc / 58
			rem = acc % 58
			if len(quotient) > 0 || q > 0 {
				quotient = append(quotient, byte(q))
			}
		}
		digits = append(digits, alphabet[rem])
		num = quotient
	}
	for i := 0; i < zeros; i++ {
		digits = append(digits, '1')
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}
