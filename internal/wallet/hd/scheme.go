package hd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
)

// Path is a relative derivation path below an extended public key: a list of
// non-hardened child indexes. The empty path denotes the key itself.
type Path []uint32

// String renders the path as "0/7". The empty path renders as "".
func (p Path) String() string {
	parts := make([]string, len(p))
	for i, index := range p {
		parts[i] = formatIndex(index)
	}
	return strings.Join(parts, "/")
}

// Layout names a full derivation layout offered in the setup UI. A Layout is
// an absolute statement ("addresses live at m/44'/60'/0'/0/n"); what has to be
// derived from a given xpub depends on the depth the wallet exported it at,
// which SchemeFor resolves.
type Layout uint8

const (
	// LayoutBIP44 is the standard layout m/44'/60'/0'/0/n used by MetaMask,
	// Trezor, Rabby, Trust Wallet and most others: one account, an external
	// chain (0), then the address index.
	LayoutBIP44 Layout = iota + 1
	// LayoutLedgerLegacy is m/44'/60'/0'/n, the layout of the Ledger
	// Chrome app and MyEtherWallet's "Ledger (legacy)" option: the address
	// index sits directly below the account key.
	LayoutLedgerLegacy
)

// String returns the absolute path template of the layout.
func (l Layout) String() string {
	switch l {
	case LayoutBIP44:
		return "m/44'/60'/0'/0/n"
	case LayoutLedgerLegacy:
		return "m/44'/60'/0'/n"
	default:
		return fmt.Sprintf("Layout(%d)", uint8(l))
	}
}

// ErrLayoutDepth is returned by SchemeFor when the depth (or child index) of
// the supplied xpub does not fit the chosen layout, so the two cannot be
// combined without guessing.
var ErrLayoutDepth = errors.New("hd: extended key depth does not match the chosen derivation layout")

// ErrScheme is returned by ParseScheme for syntactically invalid schemes.
var ErrScheme = errors.New("hd: invalid derivation scheme")

// accountDepth is the depth of a BIP44 account key m/44'/60'/0'.
const accountDepth = 3

// Scheme states where the address with index n lives relative to the
// configured extended public key:
//
//	address(n) = xpub / branch[0] / ... / branch[k-1] / n
//
// Its text form is the relative path with a literal n as the last segment:
// "0/n" (an account-level xpub under the standard layout) or "n" (the address
// index directly below the xpub). All segments are non-hardened by
// construction — a scheme that would need a hardened step cannot be
// expressed.
//
// The zero value is the scheme "n".
type Scheme struct {
	branch string // encoded Path, kept as a string so Scheme stays comparable
}

// SchemeDirect is the scheme "n": addresses are the direct children of the
// xpub.
var SchemeDirect = Scheme{}

// SchemeExternalChain is the scheme "0/n": addresses are the children of the
// xpub's child 0 (the BIP44 external chain).
var SchemeExternalChain = schemeFromBranch(Path{0})

func schemeFromBranch(p Path) Scheme { return Scheme{branch: p.String()} }

// ParseScheme parses the text form of a scheme: zero or more decimal,
// non-hardened child indexes separated by "/", followed by the literal "n".
// Examples: "n", "0/n", "0/0/n".
//
// A hardened segment (5', 5h, 5H or an index >= 2^31) is refused with
// ErrHardened. An absolute path (one starting with "m") is refused with
// ErrScheme: a scheme is always relative to the xpub; use SchemeFor to
// translate an absolute Layout.
//
// ParseScheme is the explicit escape hatch for wallets whose exports do not
// follow the depth conventions SchemeFor relies on. It performs no depth
// check: the operator asserts the relative path, and the "verify the first
// addresses against your wallet" step is the safeguard.
func ParseScheme(s string) (Scheme, error) {
	segments := strings.Split(s, "/")
	last := len(segments) - 1
	if segments[last] != "n" {
		if segments[0] == "m" || segments[0] == "M" {
			return Scheme{}, fmt.Errorf("%w: must be relative to the extended key, not start with m", ErrScheme)
		}
		return Scheme{}, fmt.Errorf("%w: the last segment must be the literal n", ErrScheme)
	}
	if len(segments) > 1 && (segments[0] == "m" || segments[0] == "M") {
		return Scheme{}, fmt.Errorf("%w: must be relative to the extended key, not start with m", ErrScheme)
	}
	branch := make(Path, 0, last)
	for i, seg := range segments[:last] {
		index, err := parseSegment(seg)
		if err != nil {
			return Scheme{}, fmt.Errorf("segment %d: %w", i, err)
		}
		branch = append(branch, index)
	}
	if len(branch) > 8 {
		return Scheme{}, fmt.Errorf("%w: too many segments", ErrScheme)
	}
	return schemeFromBranch(branch), nil
}

// parseSegment parses one non-hardened decimal child index.
func parseSegment(seg string) (uint32, error) {
	if seg == "" {
		return 0, fmt.Errorf("%w: empty segment", ErrScheme)
	}
	switch seg[len(seg)-1] {
	case '\'', 'h', 'H':
		return 0, ErrHardened
	}
	for i := 0; i < len(seg); i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return 0, fmt.Errorf("%w: segments must be decimal numbers", ErrScheme)
		}
	}
	v, err := strconv.ParseUint(seg, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: segment out of range", ErrScheme)
	}
	if uint32(v) >= HardenedOffset {
		return 0, ErrHardened
	}
	return uint32(v), nil
}

// Branch returns the fixed path between the xpub and the parent of the
// addresses.
func (s Scheme) Branch() Path {
	if s.branch == "" {
		return nil
	}
	parts := strings.Split(s.branch, "/")
	p := make(Path, len(parts))
	for i, part := range parts {
		v, _ := strconv.ParseUint(part, 10, 32) // encoded by schemeFromBranch
		p[i] = uint32(v)
	}
	return p
}

// String returns the text form accepted by ParseScheme, e.g. "0/n".
func (s Scheme) String() string {
	if s.branch == "" {
		return "n"
	}
	return s.branch + "/n"
}

// MarshalText implements encoding.TextMarshaler.
func (s Scheme) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Scheme) UnmarshalText(text []byte) error {
	parsed, err := ParseScheme(string(text))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// SchemeFor resolves an absolute Layout against the depth at which the
// wallet exported key, and returns the relative scheme to use with it.
//
// Both layouts hang off the hardened account key m/44'/60'/0' (depth 3),
// which is the key practically every wallet exports:
//
//	layout               key depth 3 (account)   key depth 4 (m/44'/60'/0'/0)
//	LayoutBIP44          "0/n"                   "n"
//	LayoutLedgerLegacy   "n"                     ErrLayoutDepth
//
// Everything else is refused rather than guessed:
//
//   - Depth 0–2 (master, purpose or coin-type key): the remaining steps down
//     to the account are hardened and cannot be derived from a public key.
//     The error wraps both ErrLayoutDepth and ErrHardened; the operator must
//     export the account-level xpub instead.
//   - Depth 3 whose own child index is not hardened: not an account key.
//   - Depth 4 under LayoutBIP44 whose child index is not 0: that is the
//     internal (change) chain or something else, not m/44'/60'/0'/0.
//   - Depth 4 under LayoutLedgerLegacy, and depth 5 or more under either
//     layout: the key is already an address-level key (or deeper).
//
// The depth byte is the only evidence an xpub carries about its position; it
// cannot prove that the purpose and coin type were 44' and 60'. The setup
// step that shows DeriveRange(0, n) for comparison with the wallet remains
// the authoritative check. For an export that violates these conventions use
// ParseScheme.
func SchemeFor(layout Layout, key *ExtendedKey) (Scheme, error) {
	if layout != LayoutBIP44 && layout != LayoutLedgerLegacy {
		return Scheme{}, fmt.Errorf("hd: unknown layout %d", uint8(layout))
	}
	depth := key.Depth()
	switch {
	case depth < accountDepth:
		return Scheme{}, fmt.Errorf("%w: key is at depth %d, above the account level m/44'/60'/0' (depth %d); reaching %s from it needs hardened steps (%w); export the account-level key instead",
			ErrLayoutDepth, depth, accountDepth, layout, ErrHardened)
	case depth == accountDepth:
		if !key.IsHardenedChild() {
			return Scheme{}, fmt.Errorf("%w: key is at depth %d but is not a hardened child, so it is not an account key m/44'/60'/0'", ErrLayoutDepth, depth)
		}
		if layout == LayoutBIP44 {
			return SchemeExternalChain, nil
		}
		return SchemeDirect, nil
	case depth == accountDepth+1 && layout == LayoutBIP44:
		if key.ChildNumber() != 0 {
			return Scheme{}, fmt.Errorf("%w: key is at depth %d with child index %s; %s needs the external chain key (child index 0) or the account key", ErrLayoutDepth, depth, formatIndex(key.ChildNumber()), layout)
		}
		return SchemeDirect, nil
	default:
		return Scheme{}, fmt.Errorf("%w: key is at depth %d, at or below the address level of %s; export the account-level key (depth %d)", ErrLayoutDepth, depth, layout, accountDepth)
	}
}

// MaxRange is the largest count DeriveRange accepts. It bounds the work a
// single (API-triggered) call can cause.
const MaxRange = 1000

// ErrIndexRange is returned when an address index is not below 2^31.
var ErrIndexRange = errors.New("hd: address index must be below 2^31")

// Deriver derives deposit addresses from one extended public key under one
// Scheme. It caches the parent key of the addresses, so each address costs a
// single child derivation. A Deriver is immutable and safe for concurrent
// use.
type Deriver struct {
	root   *ExtendedKey // the key as supplied by the operator
	parent *ExtendedKey // root / scheme.Branch()
	scheme Scheme
}

// NewDeriver prepares address derivation for key under scheme.
func NewDeriver(key *ExtendedKey, scheme Scheme) (*Deriver, error) {
	if key == nil {
		return nil, errors.New("hd: nil extended key")
	}
	parent, err := key.Derive(scheme.Branch())
	if err != nil {
		return nil, fmt.Errorf("hd: derive scheme branch %q: %w", scheme.Branch(), err)
	}
	return &Deriver{root: key, parent: parent, scheme: scheme}, nil
}

// Key returns the extended public key the Deriver was created with.
func (d *Deriver) Key() *ExtendedKey { return d.root }

// Scheme returns the scheme the Deriver was created with.
func (d *Deriver) Scheme() Scheme { return d.scheme }

// Path returns the relative path of address index, e.g. Path{0, 7}.
func (d *Deriver) Path(index uint32) Path {
	return append(d.scheme.Branch(), index)
}

// Address returns the deposit address with the given index.
//
// It returns ErrIndexRange if index >= 2^31. It returns ErrInvalidChild in
// the (probability < 2^-127) case that BIP32 defines no key at this index;
// the caller must then skip the index, exactly as a wallet would.
func (d *Deriver) Address(index uint32) (ethaddr.Address, error) {
	if index >= HardenedOffset {
		return ethaddr.Address{}, ErrIndexRange
	}
	child, err := d.parent.Child(index)
	if err != nil {
		return ethaddr.Address{}, fmt.Errorf("hd: address index %d: %w", index, err)
	}
	return child.Address(), nil
}

// DerivedAddress is one entry of DeriveRange.
type DerivedAddress struct {
	// Index is the address index n.
	Index uint32
	// Path is the derivation path relative to the xpub, e.g. "0/7".
	Path string
	// Address is the deposit address.
	Address ethaddr.Address
}

// DeriveRange returns the addresses with indexes first, first+1, ...,
// first+count-1. It backs the setup step in which the operator compares the
// first addresses with the ones their wallet shows.
//
// count must not exceed MaxRange and the range must stay below 2^31. If BIP32
// defines no key at one of the indexes the call fails with ErrInvalidChild
// (see Address).
func (d *Deriver) DeriveRange(first, count uint32) ([]DerivedAddress, error) {
	if count > MaxRange {
		return nil, fmt.Errorf("hd: range of %d addresses exceeds the maximum of %d", count, MaxRange)
	}
	if first >= HardenedOffset || count > HardenedOffset-first {
		return nil, ErrIndexRange
	}
	out := make([]DerivedAddress, 0, count)
	for i := uint32(0); i < count; i++ {
		index := first + i
		addr, err := d.Address(index)
		if err != nil {
			return nil, err
		}
		out = append(out, DerivedAddress{Index: index, Path: d.Path(index).String(), Address: addr})
	}
	return out, nil
}
