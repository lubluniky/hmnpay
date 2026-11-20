// Package wallet implements the core's AddressSource port: deposit addresses
// derived from the operator's extended public key (see package hd).
//
// hmnpay is watch-only. Nothing in this package can hold, derive or accept a
// private key: an xprv, a seed phrase or a raw key in place of the xpub is
// refused with an error that says so and never contains the input.
package wallet

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/wallet/hd"
)

// Derivation path templates offered by the setup wizard.
const (
	// PathBIP44 is the standard layout (MetaMask, Trezor, Rabby, …):
	// addresses at m/44'/60'/0'/0/n.
	PathBIP44 = "m/44'/60'/0'/0/n"
	// PathLedgerLegacy is the layout of the old Ledger Chrome app and
	// MyEtherWallet's "Ledger (legacy)": addresses at m/44'/60'/0'/n.
	PathLedgerLegacy = "m/44'/60'/0'/n"
)

// ErrUnsupportedPath is returned for a derivation path other than the two
// supported layouts.
var ErrUnsupportedPath = errors.New("wallet: unsupported derivation path; supported are " + PathBIP44 + " (standard) and " + PathLedgerLegacy + " (Ledger legacy)")

// ParseDerivationPath maps a derivation path string, as the core stores it
// and the frontend offers it, to a layout.
//
// The core defines the path as the parent of the addresses ("addresses are
// derived at <path>/<index>", default m/44'/60'/0'/0); the frontend shows it
// with the index placeholder (m/44'/60'/0'/0/n). Both spellings are accepted:
//
//	m/44'/60'/a'/0   m/44'/60'/a'/0/n   → hd.LayoutBIP44
//	m/44'/60'/a'     m/44'/60'/a'/n     → hd.LayoutLedgerLegacy
//
// Hardened segments may be marked with ' or h or H. The account a is usually
// 0; any account number is accepted, because the xpub — not the path string —
// determines the keys: the path only says how the addresses hang below the
// account key the wallet exported. Purpose 44 and coin type 60 are required.
// Everything else — other purposes, a non-zero change segment, hardened
// address levels, Ledger Live's m/44'/60'/n'/0/0 (one hardened account per
// address, which no single xpub can cover) — is ErrUnsupportedPath.
func ParseDerivationPath(path string) (hd.Layout, error) {
	segments := strings.Split(strings.TrimSpace(path), "/")
	if n := len(segments); n > 1 && segments[n-1] == "n" {
		segments = segments[:n-1]
	}
	if len(segments) < 4 || len(segments) > 5 || segments[0] != "m" {
		return 0, ErrUnsupportedPath
	}
	hardened := func(seg string) (uint32, bool) {
		if seg == "" {
			return 0, false
		}
		switch seg[len(seg)-1] {
		case '\'', 'h', 'H':
		default:
			return 0, false
		}
		v, err := strconv.ParseUint(seg[:len(seg)-1], 10, 31)
		if err != nil || strings.HasPrefix(seg, "+") {
			return 0, false
		}
		return uint32(v), true
	}
	if v, ok := hardened(segments[1]); !ok || v != 44 {
		return 0, ErrUnsupportedPath
	}
	if v, ok := hardened(segments[2]); !ok || v != 60 {
		return 0, ErrUnsupportedPath
	}
	if _, ok := hardened(segments[3]); !ok {
		return 0, ErrUnsupportedPath
	}
	switch {
	case len(segments) == 4:
		return hd.LayoutLedgerLegacy, nil
	case segments[4] == "0":
		return hd.LayoutBIP44, nil
	default:
		return 0, ErrUnsupportedPath
	}
}

// maxCached bounds the number of prepared wallets kept. An instance has one
// wallet; the setup wizard and a wallet change add a few more.
const maxCached = 8

type cacheKey struct{ xpub, path string }

// AddressSource derives deposit addresses for the core. It implements
// core.AddressSource: deterministic, injective over indexes, pure (no I/O, no
// blocking), and safe for concurrent use and for use inside a store
// transaction.
//
// Preparing a wallet (decoding the key, checking it, deriving the branch the
// addresses hang off) is done once per (xpub, path) and cached; each address
// then costs a single public child derivation, a fraction of a millisecond.
type AddressSource struct {
	mu    sync.Mutex
	cache map[cacheKey]*hd.Deriver
}

// NewAddressSource returns an AddressSource.
func NewAddressSource() *AddressSource {
	return &AddressSource{cache: make(map[cacheKey]*hd.Deriver)}
}

var _ core.AddressSource = (*AddressSource)(nil)

// DeriveAddress returns the deposit address with the given index for the
// wallet's extended public key and derivation path, as lower-case 0x-prefixed
// hex. NextAddressIndex is ignored.
//
// Errors (all safe to show; none contains key material): the hd package's
// sentinels for an unusable key — hd.ErrPrivateKey and hd.ErrSecret for
// secret material, hd.ErrChecksum, hd.ErrLength, hd.ErrEncoding,
// hd.ErrVersion, hd.ErrInvalidPublicKey; ErrUnsupportedPath;
// hd.ErrLayoutDepth when the key was exported at a depth that does not fit
// the path (for example a master xpub); hd.ErrIndexRange for an index of
// 2^31 or more.
func (s *AddressSource) DeriveAddress(_ context.Context, w core.WalletSettings, index uint32) (string, error) {
	d, err := s.deriver(w.XPub, w.DerivationPath)
	if err != nil {
		return "", err
	}
	addr, err := d.Address(index)
	if err != nil {
		return "", err
	}
	return addr.Hex(), nil
}

// deriver returns the prepared wallet for an xpub and path.
func (s *AddressSource) deriver(xpub, path string) (*hd.Deriver, error) {
	key := cacheKey{xpub: xpub, path: path}
	s.mu.Lock()
	d, ok := s.cache[key]
	s.mu.Unlock()
	if ok {
		return d, nil
	}

	layout, err := ParseDerivationPath(path)
	if err != nil {
		return nil, err
	}
	parsed, err := hd.ParseXpub(xpub)
	if err != nil {
		return nil, err
	}
	scheme, err := hd.SchemeFor(layout, parsed)
	if err != nil {
		return nil, err
	}
	d, err = hd.NewDeriver(parsed, scheme)
	if err != nil {
		return nil, fmt.Errorf("wallet: %w", err)
	}

	// Only valid wallets are cached, so the cache cannot be filled with
	// garbage by failed attempts.
	s.mu.Lock()
	if len(s.cache) >= maxCached {
		clear(s.cache)
	}
	s.cache[key] = d
	s.mu.Unlock()
	return d, nil
}
