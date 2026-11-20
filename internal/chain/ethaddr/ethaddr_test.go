package ethaddr

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// eip55Vectors are the test cases listed in EIP-55
// (https://eips.ethereum.org/EIPS/eip-55#test-cases).
var eip55Vectors = []string{
	// All caps
	"0x52908400098527886E0F7030069857D2E4169EE7",
	"0x8617E340B3D01FA5F11F306F4090FD50E238070D",
	// All lower
	"0xde709f2102306220921060314715629080e2fb77",
	"0x27b1fdb04752bbc536007a920d24acb045561c26",
	// Normal
	"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
	"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
	"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
	"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
}

func TestEIP55Vectors(t *testing.T) {
	for _, want := range eip55Vectors {
		// Parsing the checksummed form must succeed...
		a, err := Parse(want)
		if err != nil {
			t.Errorf("Parse(%s): %v", want, err)
			continue
		}
		// ...and so must the checksum-less lower-case form, which must
		// re-encode to exactly the vector.
		lower, err := Parse(strings.ToLower(want))
		if err != nil {
			t.Errorf("Parse(lower %s): %v", want, err)
			continue
		}
		if lower != a {
			t.Errorf("%s: lower-case parse differs", want)
		}
		if got := lower.String(); got != want {
			t.Errorf("String() = %s, want %s", got, want)
		}
		if got := a.Hex(); got != strings.ToLower(want) {
			t.Errorf("Hex() = %s, want %s", got, strings.ToLower(want))
		}
	}
}

func TestParseRejectsBadChecksum(t *testing.T) {
	// Flip the case of one letter in each mixed-case vector: every such
	// mutation must be rejected with ErrChecksum.
	for _, v := range eip55Vectors[4:] {
		for i := 2; i < len(v); i++ {
			c := v[i]
			var flipped byte
			switch {
			case c >= 'a' && c <= 'f':
				flipped = c - 'a' + 'A'
			case c >= 'A' && c <= 'F':
				flipped = c - 'A' + 'a'
			default:
				continue
			}
			mutated := v[:i] + string(flipped) + v[i+1:]
			if isSingleCase(mutated[2:]) {
				continue // became checksum-less, which is legitimately accepted
			}
			if _, err := Parse(mutated); !errors.Is(err, ErrChecksum) {
				t.Fatalf("Parse(%s) err = %v, want ErrChecksum", mutated, err)
			}
		}
	}
}

func TestParseDetectsSingleCharacterTypos(t *testing.T) {
	// Replacing a digit changes the address; with the original
	// capitalisation kept, the checksum must (overwhelmingly) fail. For
	// this fixed vector and substitution every position must be caught or
	// yield a syntactically different, checksum-less string.
	const v = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"
	caught := 0
	for i := 2; i < len(v); i++ {
		repl := byte('1')
		if v[i] == '1' {
			repl = '2'
		}
		mutated := v[:i] + string(repl) + v[i+1:]
		if _, err := Parse(mutated); errors.Is(err, ErrChecksum) {
			caught++
		}
	}
	if caught < 38 {
		t.Fatalf("only %d/40 single-character typos detected", caught)
	}
}

func TestParseSyntax(t *testing.T) {
	bad := []string{
		"",
		"0x",
		"5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",     // no prefix
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAe",    // 39 digits
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAedd",  // 41 digits
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeg",   // 39 + non-hex
		"0xZaAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",   // non-hex
		" 0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",  // leading space
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed\n", // trailing newline
		"1x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
	}
	for _, s := range bad {
		if _, err := Parse(s); !errors.Is(err, ErrSyntax) {
			t.Errorf("Parse(%q) err = %v, want ErrSyntax", s, err)
		}
	}
	if _, err := Parse("0X52908400098527886E0F7030069857D2E4169EE7"); err != nil {
		t.Errorf("upper-case 0X prefix rejected: %v", err)
	}
}

func TestFromUncompressedPublicKey(t *testing.T) {
	// Public key of the secp256k1 private key 1 (the generator point G).
	// Its Ethereum address is a widely published constant.
	g, _ := hex.DecodeString("0479be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798" +
		"483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8")
	const want = "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"
	a, err := FromUncompressedPublicKey(g)
	if err != nil {
		t.Fatal(err)
	}
	if a.String() != want {
		t.Fatalf("address of G = %s, want %s", a, want)
	}
	b, err := FromUncompressedPublicKey(g[1:])
	if err != nil || b != a {
		t.Fatalf("64-byte form: %v %s", err, b)
	}
	for _, bad := range [][]byte{nil, g[:63], g[:33], append([]byte{0x02}, g[1:]...), append(append([]byte{}, g...), 0)} {
		if _, err := FromUncompressedPublicKey(bad); err == nil {
			t.Errorf("accepted %d-byte key with prefix %x", len(bad), bad[:min(1, len(bad))])
		}
	}
}

func TestKeccak256(t *testing.T) {
	// Keccak-256("") — distinguishes legacy Keccak from NIST SHA3-256.
	got := Keccak256()
	const want = "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"
	if hex.EncodeToString(got[:]) != want {
		t.Fatalf("Keccak256(\"\") = %x", got)
	}
	split := Keccak256([]byte("ab"), []byte("c"))
	whole := Keccak256([]byte("abc"))
	if split != whole {
		t.Fatal("multi-part hashing differs from single-part")
	}
}

func TestTextRoundTrip(t *testing.T) {
	a := MustParse("0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359")
	raw, err := json.Marshal(map[string]Address{"a": a})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"a":"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359"}` {
		t.Fatalf("json = %s", raw)
	}
	var back map[string]Address
	if err := json.Unmarshal(raw, &back); err != nil || back["a"] != a {
		t.Fatalf("round trip: %v %v", err, back)
	}
	if err := json.Unmarshal([]byte(`{"a":"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d358"}`), &back); err == nil {
		t.Fatal("bad checksum accepted through UnmarshalText")
	}
	if !(Address{}).IsZero() || a.IsZero() {
		t.Fatal("IsZero")
	}
	if b, err := FromBytes(a.Bytes()); err != nil || b != a {
		t.Fatal("FromBytes round trip")
	}
	if _, err := FromBytes(a[:19]); err == nil {
		t.Fatal("FromBytes accepted 19 bytes")
	}
}

func FuzzParse(f *testing.F) {
	for _, v := range eip55Vectors {
		f.Add(v)
	}
	f.Add("0x")
	f.Add("0x0000000000000000000000000000000000000000")
	f.Fuzz(func(t *testing.T, s string) {
		a, err := Parse(s)
		if err != nil {
			return
		}
		// Anything accepted must round-trip through both encodings.
		for _, enc := range []string{a.String(), a.Hex()} {
			b, err := Parse(enc)
			if err != nil || b != a {
				t.Fatalf("round trip of %q via %q failed: %v", s, enc, err)
			}
		}
		if !strings.EqualFold(a.Hex(), s) {
			t.Fatalf("Parse(%q) = %s: digits changed", s, a.Hex())
		}
	})
}

func isSingleCase(s string) bool {
	return s == strings.ToLower(s) || s == strings.ToUpper(s)
}
