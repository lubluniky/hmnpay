package hd

import (
	"crypto/sha256"
	"math/big"
)

// Base58 (Bitcoin alphabet) and Base58Check, implemented here because the
// only thing this package ever encodes is a fixed 78-byte extended key; a
// dependency would be larger than the code.

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Index maps an ASCII byte to its digit value, or -1.
var base58Index = func() (t [256]int8) {
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(base58Alphabet); i++ {
		t[base58Alphabet[i]] = int8(i)
	}
	return t
}()

var big58 = big.NewInt(58)

// base58Decode decodes s. It reports ok=false if s contains a character
// outside the alphabet. The caller bounds len(s).
func base58Decode(s string) (out []byte, ok bool) {
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	n := new(big.Int)
	for i := zeros; i < len(s); i++ {
		d := base58Index[s[i]]
		if d < 0 {
			return nil, false
		}
		n.Mul(n, big58)
		n.Add(n, big.NewInt(int64(d)))
	}
	body := n.Bytes()
	out = make([]byte, zeros+len(body))
	copy(out[zeros:], body)
	return out, true
}

// base58Encode encodes b.
func base58Encode(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}
	n := new(big.Int).SetBytes(b)
	mod := new(big.Int)
	// 138/100 > log(256)/log(58): upper bound on the encoded length.
	digits := make([]byte, 0, len(b)*138/100+1)
	for n.Sign() > 0 {
		n.DivMod(n, big58, mod)
		digits = append(digits, base58Alphabet[mod.Int64()])
	}
	for i := 0; i < zeros; i++ {
		digits = append(digits, '1')
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}

// checksum returns the first four bytes of SHA-256(SHA-256(payload)).
func checksum(payload []byte) [4]byte {
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	return [4]byte(second[:4])
}
