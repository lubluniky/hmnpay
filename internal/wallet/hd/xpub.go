// Package hd derives EVM deposit addresses from a BIP32 extended public key
// (xpub).
//
// hmnpay is watch-only and non-custodial: the operator pastes an xpub during
// setup and the gateway derives a fresh address per payment. This package
// therefore implements only the public half of BIP32 (parsing, CKDpub) and
// goes out of its way to refuse secret material: extended private keys, seed
// phrases and raw private keys are rejected with dedicated errors, and no
// error produced here ever contains any part of the input.
//
// Typical use:
//
//	key, err := hd.ParseXpub(input)                   // validate what the operator pasted
//	scheme, err := hd.SchemeFor(hd.LayoutBIP44, key)  // "0/n" for an account-level xpub
//	d, err := hd.NewDeriver(key, scheme)
//	addr, err := d.Address(7)                         // deposit address #7
//	preview, err := d.DeriveRange(0, 5)               // "verify these against your wallet"
package hd

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	//lint:ignore SA1019 RIPEMD-160 is what BIP32 defines key fingerprints with; it is not used for anything else.
	"golang.org/x/crypto/ripemd160"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
)

// Errors returned by ParseXpub and the derivation functions. They are
// sentinels (compare with errors.Is) so the UI can show a precise message.
// None of them, nor any error wrapping them, contains input bytes.
var (
	// ErrPrivateKey is returned when the input is an extended PRIVATE key
	// (xprv, tprv, yprv, zprv, ...). The UI should tell the operator that
	// this key must never be pasted anywhere and that the wallet behind it
	// should be considered exposed if it left their machine.
	ErrPrivateKey = errors.New("hd: this is an extended private key; only the extended public key (xpub) may be used")

	// ErrSecret is returned when the input is not an extended key at all but
	// looks like other secret material: a seed phrase (12+ words) or a raw
	// 32-byte private key in hex.
	ErrSecret = errors.New("hd: input looks like a seed phrase or a raw private key, not an extended public key")

	// ErrEncoding is returned when the input is not Base58 text.
	ErrEncoding = errors.New("hd: extended key is not valid Base58")

	// ErrLength is returned when the decoded key is not exactly 78 bytes
	// plus a 4-byte checksum (truncated or padded input).
	ErrLength = errors.New("hd: extended key has the wrong length")

	// ErrChecksum is returned when the Base58Check checksum does not match
	// (typically a typo or an incomplete copy/paste).
	ErrChecksum = errors.New("hd: extended key checksum mismatch")

	// ErrVersion is returned for extended public keys with version bytes
	// other than xpub/tpub, e.g. Bitcoin SegWit ypub/zpub exports.
	ErrVersion = errors.New("hd: unsupported extended key version (expected xpub or tpub)")

	// ErrInvalidKey is returned when the fixed fields of the key are
	// inconsistent (a depth-0 key with a parent fingerprint or child index).
	ErrInvalidKey = errors.New("hd: malformed extended key")

	// ErrInvalidPublicKey is returned when the embedded public key is not a
	// compressed secp256k1 point on the curve.
	ErrInvalidPublicKey = errors.New("hd: extended key does not contain a valid secp256k1 public key")

	// ErrHardened is returned when a derivation step is hardened. Hardened
	// children can only be derived from a private key, which hmnpay never
	// has.
	ErrHardened = errors.New("hd: hardened derivation is impossible from an extended public key")

	// ErrInvalidChild is returned when BIP32 declares the child at an index
	// invalid (IL >= n or the resulting point is the point at infinity).
	// The probability is below 2^-127; per BIP32 the caller proceeds with
	// the next index.
	ErrInvalidChild = errors.New("hd: no valid child key exists at this index")

	// ErrDepth is returned when deriving would exceed the maximum BIP32
	// depth of 255.
	ErrDepth = errors.New("hd: maximum derivation depth exceeded")
)

// HardenedOffset is the first hardened child index (2^31). Indexes at or
// above it cannot be derived from a public key.
const HardenedOffset uint32 = 1 << 31

// Extended public key version bytes.
const (
	versionXpub uint32 = 0x0488B21E // mainnet public
	versionTpub uint32 = 0x043587CF // testnet public
	versionXprv uint32 = 0x0488ADE4 // mainnet private
	versionTprv uint32 = 0x04358394 // testnet private
)

const (
	payloadLen = 78             // version..key
	encodedLen = payloadLen + 4 // + checksum
	// An 82-byte value needs at most 113 Base58 digits; anything much
	// longer is not an extended key and is not worth decoding.
	maxInputLen = 120
)

// ExtendedKey is a parsed BIP32 extended public key. It is immutable and safe
// for concurrent use.
//
// An xpub is not a secret in the sense of a private key, but it reveals every
// address of the account and, combined with any single child private key,
// every private key of the account. ExtendedKey therefore does not implement
// fmt.Stringer with the full key: String and LogValue return a redacted
// description, and the full encoding is only available through Serialize.
type ExtendedKey struct {
	version   uint32
	depth     uint8
	parentFP  [4]byte
	childNum  uint32
	chainCode [32]byte
	pub       *secp256k1.PublicKey
}

// ParseXpub parses and fully validates a Base58Check-encoded extended public
// key as exported by a wallet. Leading and trailing whitespace is ignored.
//
// Validation covers the encoding, the length, the checksum, the version
// (xpub or tpub), the structural rules for depth-0 keys and that the public
// key is a point on secp256k1.
//
// Secret material is rejected with ErrPrivateKey (an extended private key) or
// ErrSecret (a seed phrase or raw private key). A private key is recognised
// by its text prefix and version before the checksum is even looked at, so a
// mistyped private key is still reported as a private key. Errors never
// contain any part of the input, so they are safe to log and to send to the
// browser.
func ParseXpub(input string) (*ExtendedKey, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return nil, ErrEncoding
	}
	if looksLikeSecret(s) {
		return nil, ErrSecret
	}
	// All standard private-key prefixes read "?prv" (xprv, tprv, yprv,
	// zprv, Yprv, Zprv, uprv, vprv, ...).
	// The input is not even decoded in that case.
	if len(s) >= 4 && s[1:4] == "prv" {
		return nil, ErrPrivateKey
	}
	if len(s) > maxInputLen {
		return nil, ErrLength
	}
	raw, ok := base58Decode(s)
	// Whatever happens next, do not leave decoded key bytes behind: the
	// buffer may still hold a private key under an unusual prefix.
	defer clear(raw)
	if !ok {
		return nil, ErrEncoding
	}
	if len(raw) != encodedLen {
		return nil, ErrLength
	}
	version := binary.BigEndian.Uint32(raw[0:4])
	if version == versionXprv || version == versionTprv {
		return nil, ErrPrivateKey
	}
	if sum := checksum(raw[:payloadLen]); !hmac.Equal(sum[:], raw[payloadLen:]) {
		return nil, ErrChecksum
	}
	keyData := raw[45:78]
	if keyData[0] == 0x00 {
		// "0x00 || 32-byte secret" is the private key serialisation, here
		// under a public or unknown version (BIP32 test vector 5).
		return nil, ErrPrivateKey
	}
	if version != versionXpub && version != versionTpub {
		return nil, ErrVersion
	}

	k := &ExtendedKey{
		version:  version,
		depth:    raw[4],
		childNum: binary.BigEndian.Uint32(raw[9:13]),
	}
	copy(k.parentFP[:], raw[5:9])
	copy(k.chainCode[:], raw[13:45])
	if k.depth == 0 && (k.parentFP != [4]byte{} || k.childNum != 0) {
		return nil, fmt.Errorf("%w: a master key cannot have a parent fingerprint or child index", ErrInvalidKey)
	}
	if keyData[0] != 0x02 && keyData[0] != 0x03 {
		return nil, ErrInvalidPublicKey
	}
	pub, err := secp256k1.ParsePubKey(keyData)
	if err != nil {
		// The library error may describe coordinates; do not propagate it.
		return nil, ErrInvalidPublicKey
	}
	k.pub = pub
	return k, nil
}

// looksLikeSecret recognises inputs that are not extended keys but are very
// likely secrets pasted into the wrong field: a BIP39-style phrase of twelve
// or more words, or 64 hex digits (a raw private key), optionally 0x-prefixed.
func looksLikeSecret(s string) bool {
	if fields := strings.Fields(s); len(fields) >= 12 {
		return true
	}
	h := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(h) == 64 {
		if _, err := hex.DecodeString(h); err == nil {
			return true
		}
	}
	return false
}

// Serialize returns the Base58Check encoding of the key ("xpub..."). The
// result parses back to an identical key. Treat it as sensitive: do not log
// it.
func (k *ExtendedKey) Serialize() string {
	var raw [encodedLen]byte
	binary.BigEndian.PutUint32(raw[0:4], k.version)
	raw[4] = k.depth
	copy(raw[5:9], k.parentFP[:])
	binary.BigEndian.PutUint32(raw[9:13], k.childNum)
	copy(raw[13:45], k.chainCode[:])
	copy(raw[45:78], k.pub.SerializeCompressed())
	sum := checksum(raw[:payloadLen])
	copy(raw[payloadLen:], sum[:])
	return base58Encode(raw[:])
}

// String returns a redacted, log-safe description such as
// "xpub(fingerprint=3442193e depth=3 child=0')". It deliberately does not
// return the key itself; use Serialize for that.
func (k *ExtendedKey) String() string {
	prefix := "xpub"
	if k.IsTestnet() {
		prefix = "tpub"
	}
	return fmt.Sprintf("%s(fingerprint=%s depth=%d child=%s)", prefix, k.FingerprintHex(), k.depth, formatIndex(k.childNum))
}

// GoString implements fmt.GoStringer so that even %#v does not dump the
// chain code and public key.
func (k *ExtendedKey) GoString() string { return k.String() }

// LogValue implements slog.LogValuer with the same redaction as String.
func (k *ExtendedKey) LogValue() slog.Value {
	return slog.StringValue(k.String())
}

// Depth is the number of derivation steps between the master key and this
// key: 0 for a master key, 3 for a BIP44 account key (m/44'/60'/0').
func (k *ExtendedKey) Depth() uint8 { return k.depth }

// ChildNumber is the index of this key under its parent. Values at or above
// HardenedOffset denote a hardened child (e.g. the account key 0').
func (k *ExtendedKey) ChildNumber() uint32 { return k.childNum }

// IsHardenedChild reports whether this key was derived as a hardened child.
func (k *ExtendedKey) IsHardenedChild() bool { return k.childNum >= HardenedOffset }

// ParentFingerprint is the fingerprint of the parent key (zero for a master
// key).
func (k *ExtendedKey) ParentFingerprint() [4]byte { return k.parentFP }

// IsTestnet reports whether the key carries the testnet version (tpub).
func (k *ExtendedKey) IsTestnet() bool { return k.version == versionTpub }

// PublicKey returns the 33-byte compressed SEC1 encoding of the public key.
func (k *ExtendedKey) PublicKey() []byte { return k.pub.SerializeCompressed() }

// Fingerprint returns the BIP32 key fingerprint: the first four bytes of
// RIPEMD160(SHA256(compressed public key)). It identifies the key in the
// settings UI without revealing it, and is the value wallets display as the
// "master/account fingerprint".
func (k *ExtendedKey) Fingerprint() [4]byte {
	sha := sha256.Sum256(k.pub.SerializeCompressed())
	r := ripemd160.New()
	r.Write(sha[:])
	return [4]byte(r.Sum(nil)[:4])
}

// FingerprintHex returns Fingerprint as eight lower-case hex digits.
func (k *ExtendedKey) FingerprintHex() string {
	fp := k.Fingerprint()
	return hex.EncodeToString(fp[:])
}

// Address returns the EVM address of this key's public key: the last 20
// bytes of Keccak-256 over the uncompressed point (without the 0x04 prefix).
func (k *ExtendedKey) Address() ethaddr.Address {
	addr, err := ethaddr.FromUncompressedPublicKey(k.pub.SerializeUncompressed())
	if err != nil {
		// SerializeUncompressed always returns 65 bytes starting with 0x04.
		panic("hd: unreachable: " + err.Error())
	}
	return addr
}

// Child derives the non-hardened child with the given index (BIP32 CKDpub).
//
// It returns ErrHardened if index >= 2^31, ErrDepth if the key is already at
// depth 255, and ErrInvalidChild in the two cases where BIP32 declares the
// child invalid: the left half of the HMAC is not below the curve order, or
// the resulting point is the point at infinity.
func (k *ExtendedKey) Child(index uint32) (*ExtendedKey, error) {
	if index >= HardenedOffset {
		return nil, ErrHardened
	}
	if k.depth == 255 {
		return nil, ErrDepth
	}

	// I = HMAC-SHA512(Key = c_par, Data = serP(K_par) || ser32(i))
	mac := hmac.New(sha512.New, k.chainCode[:])
	mac.Write(k.pub.SerializeCompressed())
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], index)
	mac.Write(idx[:])
	return k.childFromHMAC(index, [64]byte(mac.Sum(nil)))
}

// childFromHMAC completes CKDpub given I, the 64-byte HMAC output. It is
// split from Child so that tests can reach the two "invalid child" branches,
// which cannot be hit with real HMAC output (probability < 2^-127).
func (k *ExtendedKey) childFromHMAC(index uint32, sum [64]byte) (*ExtendedKey, error) {
	il, ir := sum[:32], sum[32:]

	// K_i = point(parse256(I_L)) + K_par
	var tweak secp256k1.ModNScalar
	if overflow := tweak.SetByteSlice(il); overflow {
		return nil, ErrInvalidChild // parse256(I_L) >= n
	}
	var tweakPoint, parentPoint, childPoint secp256k1.JacobianPoint
	secp256k1.ScalarBaseMultNonConst(&tweak, &tweakPoint)
	k.pub.AsJacobian(&parentPoint)
	secp256k1.AddNonConst(&tweakPoint, &parentPoint, &childPoint)
	if (childPoint.X.IsZero() && childPoint.Y.IsZero()) || childPoint.Z.IsZero() {
		return nil, ErrInvalidChild // K_i is the point at infinity
	}
	childPoint.ToAffine()

	child := &ExtendedKey{
		version:  k.version,
		depth:    k.depth + 1,
		parentFP: k.Fingerprint(),
		childNum: index,
		pub:      secp256k1.NewPublicKey(&childPoint.X, &childPoint.Y),
	}
	copy(child.chainCode[:], ir)
	return child, nil
}

// Derive applies Child for every element of path, in order.
func (k *ExtendedKey) Derive(path Path) (*ExtendedKey, error) {
	cur := k
	for i, index := range path {
		next, err := cur.Child(index)
		if err != nil {
			return nil, fmt.Errorf("derive step %d (index %s): %w", i, formatIndex(index), err)
		}
		cur = next
	}
	return cur, nil
}

// formatIndex renders a child index the way wallets do: 5, or 5' if hardened.
func formatIndex(i uint32) string {
	if i >= HardenedOffset {
		return fmt.Sprintf("%d'", i-HardenedOffset)
	}
	return fmt.Sprintf("%d", i)
}
