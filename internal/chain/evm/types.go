// Package evm is a small, dependency-free JSON-RPC client for EVM chains,
// built for one job: letting hmnpay observe ERC-20 payments through
// operator-supplied RPC endpoints, many of which will be free public ones.
//
// The package has three layers:
//
//   - Endpoint is one JSON-RPC URL: typed calls, strict decoding, bounded
//     responses, timeouts, health bookkeeping, and an adaptive eth_getLogs
//     scanner that learns the provider's range limits.
//   - Client is an ordered list of endpoints for one chain with failover:
//     it never hands out an endpoint that reports the wrong chain id, skips
//     endpoints that are failing, rate-limited or lagging, fails back when
//     they recover, and exposes a status snapshot for the dashboard.
//   - Probe checks a single URL for the setup wizard, reporting separately
//     whether it is reachable, on the right chain, knows the token and
//     allows eth_getLogs.
//
// Token amounts are always *big.Int. Nothing in this package assumes token
// decimals; read them with Endpoint.Decimals.
package evm

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
)

// Hash is a 32-byte Keccak-256 hash: a block hash, a transaction hash or a
// log topic.
type Hash [32]byte

// ParseHash decodes "0x" followed by exactly 64 hex digits.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if len(s) != 66 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return h, errors.New("evm: hash must be 0x followed by 64 hex digits")
	}
	if _, err := hex.Decode(h[:], []byte(s[2:])); err != nil {
		return Hash{}, errors.New("evm: hash must be 0x followed by 64 hex digits")
	}
	return h, nil
}

// MustParseHash is ParseHash for constants and tests; it panics on error.
func MustParseHash(s string) Hash {
	h, err := ParseHash(s)
	if err != nil {
		panic(err)
	}
	return h
}

// String returns the lower-case 0x-prefixed hex form.
func (h Hash) String() string { return "0x" + hex.EncodeToString(h[:]) }

// IsZero reports whether h is all zero bytes.
func (h Hash) IsZero() bool { return h == Hash{} }

// MarshalText implements encoding.TextMarshaler.
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (h *Hash) UnmarshalText(text []byte) error {
	parsed, err := ParseHash(string(text))
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

// Header is the part of a block header the gateway needs.
type Header struct {
	Number     uint64
	Hash       Hash
	ParentHash Hash
	// Time is the block timestamp in Unix seconds.
	Time uint64
}

// Log is an EVM event log as returned by eth_getLogs or inside a receipt.
type Log struct {
	Address     ethaddr.Address
	Topics      []Hash
	Data        []byte
	BlockNumber uint64
	BlockHash   Hash
	TxHash      Hash
	TxIndex     uint64
	// LogIndex is the position of the log within its block.
	LogIndex uint64
	// Removed is true when the node reports the log as reverted by a chain
	// reorganisation.
	Removed bool
}

// Receipt is a transaction receipt.
type Receipt struct {
	TxHash      Hash
	BlockHash   Hash
	BlockNumber uint64
	// Status is 1 for a successful transaction and 0 for a reverted one.
	Status uint64
	Logs   []Log
}

// Transfer is a decoded ERC-20 Transfer(address,address,uint256) event.
type Transfer struct {
	// Token is the contract that emitted the event. Callers must compare
	// it with the token they expect: anyone can deploy a contract that
	// emits identical-looking Transfer events.
	Token ethaddr.Address
	From  ethaddr.Address
	To    ethaddr.Address
	// Amount is in the token's base units.
	Amount      *big.Int
	TxHash      Hash
	LogIndex    uint64
	BlockNumber uint64
	BlockHash   Hash
}

// --- strict hex decoding -------------------------------------------------
//
// JSON-RPC encodes numbers as "quantities" ("0x1b4") and byte strings as
// "data" ("0x00ff"). A node is untrusted input, so decoding insists on the
// 0x prefix, on hex digits only, on at least one digit for quantities, on an
// even digit count for data, and on the value fitting the destination type.
// The single leniency is that leading zeros in quantities are tolerated: they
// cannot change the value, and some non-geth nodes emit them.

var errHexSyntax = errors.New("invalid hex encoding")

func hasHexPrefix(s string) bool {
	return len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X')
}

func isHexDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// decodeUint64 decodes a quantity that must fit in 64 bits.
func decodeUint64(s string) (uint64, error) {
	if !hasHexPrefix(s) || len(s) == 2 || !isHexDigits(s[2:]) {
		return 0, fmt.Errorf("quantity: %w", errHexSyntax)
	}
	digits := trimLeadingZeros(s[2:])
	if len(digits) > 16 {
		return 0, errors.New("quantity: does not fit in 64 bits")
	}
	v, err := strconv.ParseUint(digits, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("quantity: %w", errHexSyntax)
	}
	return v, nil
}

func trimLeadingZeros(digits string) string {
	for len(digits) > 1 && digits[0] == '0' {
		digits = digits[1:]
	}
	return digits
}

// decodeData decodes a byte string of at most maxLen bytes.
func decodeData(s string, maxLen int) ([]byte, error) {
	if !hasHexPrefix(s) || len(s)%2 != 0 {
		return nil, fmt.Errorf("data: %w", errHexSyntax)
	}
	if (len(s)-2)/2 > maxLen {
		return nil, fmt.Errorf("data: longer than %d bytes", maxLen)
	}
	b, err := hex.DecodeString(s[2:])
	if err != nil {
		return nil, fmt.Errorf("data: %w", errHexSyntax)
	}
	return b, nil
}

func decodeHash(s string) (Hash, error) {
	h, err := ParseHash(s)
	if err != nil {
		return Hash{}, fmt.Errorf("hash: %w", errHexSyntax)
	}
	return h, nil
}

// decodeAddress decodes an address as nodes send it: 0x + 40 hex digits, any
// capitalisation, no checksum enforcement (nodes send lower case; some send
// EIP-55, and a few have sent wrong mixed case).
func decodeAddress(s string) (ethaddr.Address, error) {
	if len(s) != 42 || !hasHexPrefix(s) {
		return ethaddr.Address{}, fmt.Errorf("address: %w", errHexSyntax)
	}
	var a ethaddr.Address
	if _, err := hex.Decode(a[:], []byte(s[2:])); err != nil {
		return ethaddr.Address{}, fmt.Errorf("address: %w", errHexSyntax)
	}
	return a, nil
}

func encodeUint64(v uint64) string { return "0x" + strconv.FormatUint(v, 16) }

func encodeData(b []byte) string { return "0x" + hex.EncodeToString(b) }

// addressTopic returns the 32-byte, left-padded topic form of an address, as
// used for indexed address parameters.
func addressTopic(a ethaddr.Address) Hash {
	var h Hash
	copy(h[12:], a[:])
	return h
}

// topicAddress is the inverse of addressTopic. It fails if the upper 12 bytes
// are not zero: such a topic is not an address, and silently truncating it
// would make two different topics look like the same recipient.
func topicAddress(h Hash) (ethaddr.Address, error) {
	for _, b := range h[:12] {
		if b != 0 {
			return ethaddr.Address{}, errors.New("topic is not a left-padded address")
		}
	}
	var a ethaddr.Address
	copy(a[:], h[12:])
	return a, nil
}
