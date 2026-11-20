package core

import "strings"

// Chain IDs the core ships presets for.
const (
	ChainBSCMainnet uint64 = 56
	ChainBSCTestnet uint64 = 97
)

// Asset describes a token on a network. It is data, not code: an instance
// runs one asset (see [Settings.Asset]) and every [Payment] keeps a snapshot
// of the asset it was created for, so amounts are never reinterpreted when
// settings change.
type Asset struct {
	// Symbol is the ticker shown to people, e.g. "USDT".
	Symbol string
	// Network is the token standard / network label, e.g. "BEP20".
	Network string
	// ChainID is the EVM chain id (56 BSC mainnet, 97 BSC testnet).
	ChainID uint64
	// Contract is the token contract address in canonical (lowercase) form.
	Contract string
	// Decimals is the token's decimals. BSC-USD uses 18, not 6.
	Decimals int
}

// Built-in assets. Addresses are in canonical lowercase form.
var (
	// USDTBSC is Binance-Peg BSC-USD (USDT) on BNB Smart Chain mainnet.
	USDTBSC = Asset{Symbol: "USDT", Network: "BEP20", ChainID: ChainBSCMainnet, Contract: "0x55d398326f99059ff775485246999027b3197955", Decimals: 18}
	// USDTBSCTestnet is the commonly used USDT test token on BSC testnet.
	USDTBSCTestnet = Asset{Symbol: "USDT", Network: "BEP20", ChainID: ChainBSCTestnet, Contract: "0x337610d27c682e347c9cd60bd4b3b107c9d34ddd", Decimals: 18}
)

// DefaultAsset returns the built-in asset for a chain id.
func DefaultAsset(chainID uint64) (Asset, bool) {
	switch chainID {
	case ChainBSCMainnet:
		return USDTBSC, true
	case ChainBSCTestnet:
		return USDTBSCTestnet, true
	}
	return Asset{}, false
}

// noFinality lists the networks declared to have no finality: on them a
// block is beyond reorganisation once it is the chain adapter's
// reorganisation window deep. None, in this version: BNB Smart Chain,
// mainnet and testnet, finalises blocks (fast finality).
var noFinality = map[uint64]bool{}

// HasFinality reports whether the network finalises blocks. On such a
// network a block is final ([ChainHead.Final]) only when two endpoints
// attest it as finalized — never because it is deep, whatever the endpoints
// answer today; while they attest nothing new, the last block they agreed
// on stays the final block.
func HasFinality(chainID uint64) bool { return !noFinality[chainID] }

// IsTestnet reports whether chainID is a known test network.
func IsTestnet(chainID uint64) bool { return chainID == ChainBSCTestnet }

// normalize validates the asset and returns it in canonical form.
func (a Asset) normalize(field string, v *validation) Asset {
	a.Symbol = strings.TrimSpace(a.Symbol)
	a.Network = strings.TrimSpace(a.Network)
	if a.Symbol == "" || len(a.Symbol) > 16 {
		v.add(field+".symbol", "invalid", "symbol must be 1 to 16 characters")
	}
	if a.Network == "" || len(a.Network) > 16 {
		v.add(field+".network", "invalid", "network must be 1 to 16 characters")
	}
	if a.ChainID == 0 {
		v.add(field+".chain_id", "required", "chain id is required")
	}
	if a.Decimals < 0 || a.Decimals > maxDecimals {
		v.add(field+".decimals", "invalid", "decimals must be between 0 and %d", maxDecimals)
	}
	c, err := NormalizeAddress(a.Contract)
	if err != nil {
		v.add(field+".contract", "invalid", "contract must be a 0x-prefixed 20-byte address")
	}
	a.Contract = c
	return a
}

// NormalizeAddress validates an EVM address (0x followed by 40 hex digits)
// and returns its canonical form: lowercase.
//
// The core stores and compares addresses only in canonical form; every
// address crossing a port — in either direction — is normalized. EIP-55
// checksum casing is a presentation concern (it needs Keccak-256, which the
// standard library does not provide); adapters that display addresses may
// re-case them.
func NormalizeAddress(s string) (string, error) {
	return normalizeHex(s, 40, "address")
}

// NormalizeHash validates a 32-byte hash (0x followed by 64 hex digits; a
// transaction or block hash) and returns it in lowercase.
func NormalizeHash(s string) (string, error) {
	return normalizeHex(s, 64, "hash")
}

func normalizeHex(s string, digits int, what string) (string, error) {
	if len(s) != digits+2 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return "", newError(ErrInvalid, "invalid %s", what)
	}
	for i := 2; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return "", newError(ErrInvalid, "invalid %s", what)
		}
	}
	return strings.ToLower(s), nil
}
