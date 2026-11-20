// Package ethaddr implements the 20-byte EVM account address: derivation from
// a secp256k1 public key, strict parsing, and EIP-55 checksum encoding.
//
// It is a leaf package (standard library + Keccak only) shared by the wallet
// side (address derivation) and the chain side (log decoding), so that neither
// has to import the other.
package ethaddr

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/crypto/sha3"
)

// Length is the size of an address in bytes.
const Length = 20

// Address is an EVM account address. The zero value is the zero address.
//
// Address is comparable and can be used as a map key. Its text form is always
// the EIP-55 mixed-case checksum encoding.
type Address [Length]byte

// Errors returned by Parse. They are sentinel values so callers (the API
// layer, the setup wizard) can map them to precise user-facing messages.
var (
	// ErrSyntax is returned when the input is not "0x" followed by exactly
	// 40 hexadecimal digits.
	ErrSyntax = errors.New("ethaddr: address must be 0x followed by 40 hex digits")
	// ErrChecksum is returned when the input is mixed-case but its
	// capitalisation is not the valid EIP-55 checksum for the address.
	ErrChecksum = errors.New("ethaddr: invalid EIP-55 checksum")
)

// Keccak256 returns the legacy Keccak-256 digest (the hash used by Ethereum,
// not NIST SHA3-256) of the concatenation of data.
func Keccak256(data ...[]byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	for _, d := range data {
		h.Write(d)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// FromUncompressedPublicKey returns the address of a secp256k1 public key
// given in SEC1 uncompressed form: either 65 bytes (0x04 || X || Y) or the
// bare 64-byte X || Y. The address is the last 20 bytes of Keccak-256(X || Y).
//
// The function does not check that the point is on the curve; callers obtain
// the encoding from a validated key.
func FromUncompressedPublicKey(pub []byte) (Address, error) {
	switch {
	case len(pub) == 65 && pub[0] == 0x04:
		pub = pub[1:]
	case len(pub) == 64:
	default:
		return Address{}, fmt.Errorf("ethaddr: uncompressed public key must be 64 or 65 bytes, got %d", len(pub))
	}
	sum := Keccak256(pub)
	var a Address
	copy(a[:], sum[12:])
	return a, nil
}

// FromBytes converts a 20-byte slice into an Address.
func FromBytes(b []byte) (Address, error) {
	if len(b) != Length {
		return Address{}, fmt.Errorf("ethaddr: address must be %d bytes, got %d", Length, len(b))
	}
	var a Address
	copy(a[:], b)
	return a, nil
}

// Parse decodes a textual address.
//
// The input must be "0x" (or "0X") followed by exactly 40 hex digits. If the
// digits are all lower-case or all upper-case the address carries no checksum
// and is accepted as is. If they are mixed-case, the capitalisation must be
// the valid EIP-55 checksum, otherwise ErrChecksum is returned: a single
// mistyped character in a checksummed address is detected instead of silently
// naming a different account.
//
// Surrounding whitespace is not trimmed; that is the caller's decision.
func Parse(s string) (Address, error) {
	if len(s) != 2+2*Length || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return Address{}, ErrSyntax
	}
	digits := s[2:]
	var a Address
	if _, err := hex.Decode(a[:], []byte(digits)); err != nil {
		return Address{}, ErrSyntax
	}
	hasLower, hasUpper := false, false
	for i := 0; i < len(digits); i++ {
		switch c := digits[i]; {
		case c >= 'a' && c <= 'f':
			hasLower = true
		case c >= 'A' && c <= 'F':
			hasUpper = true
		}
	}
	if hasLower && hasUpper {
		var want [2 * Length]byte
		a.checksumHex(&want)
		if !bytes.Equal(want[:], []byte(digits)) {
			return Address{}, ErrChecksum
		}
	}
	return a, nil
}

// MustParse is like Parse but panics on error. It is intended for constants
// and tests.
func MustParse(s string) Address {
	a, err := Parse(s)
	if err != nil {
		panic(fmt.Sprintf("ethaddr.MustParse(%q): %v", s, err))
	}
	return a
}

// String returns the EIP-55 checksummed form, e.g.
// "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed".
func (a Address) String() string {
	var buf [2 + 2*Length]byte
	buf[0], buf[1] = '0', 'x'
	a.checksumHex((*[2 * Length]byte)(buf[2:]))
	return string(buf[:])
}

// Hex returns the lower-case, non-checksummed form with a 0x prefix. This is
// the form to use in JSON-RPC requests and for case-insensitive storage keys.
func (a Address) Hex() string {
	return "0x" + hex.EncodeToString(a[:])
}

// Bytes returns a copy of the address bytes.
func (a Address) Bytes() []byte {
	return bytes.Clone(a[:])
}

// IsZero reports whether a is the zero address.
func (a Address) IsZero() bool {
	return a == Address{}
}

// MarshalText implements encoding.TextMarshaler using the EIP-55 form.
func (a Address) MarshalText() ([]byte, error) {
	return []byte(a.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler with the rules of Parse.
func (a *Address) UnmarshalText(text []byte) error {
	parsed, err := Parse(string(text))
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

// checksumHex writes the 40 EIP-55 checksummed hex digits of a into dst.
//
// EIP-55: hex-encode the address in lower case, hash those 40 ASCII
// characters with Keccak-256, and upper-case every letter whose corresponding
// hash nibble is >= 8.
func (a Address) checksumHex(dst *[2 * Length]byte) {
	hex.Encode(dst[:], a[:])
	sum := Keccak256(dst[:])
	for i := range dst {
		c := dst[i]
		if c < 'a' || c > 'f' {
			continue
		}
		nibble := sum[i/2]
		if i%2 == 0 {
			nibble >>= 4
		}
		if nibble&0x0f >= 8 {
			dst[i] = c - 'a' + 'A'
		}
	}
}
