package core

import (
	"math/big"
	"strings"
)

// Amount is a non-negative quantity of an asset in its smallest unit ("base
// units": 1 USDT on BSC is 10^18 base units). It is never a float.
//
// Amount is an immutable value type. The zero value is zero. Two Amounts are
// equal exactly when a == b, so structs containing Amounts can be compared
// with == or reflect.DeepEqual.
//
// The range is 0 … 2^256-1, i.e. anything an ERC-20 transfer can carry.
//
// Persistence: use [Amount.String] / [ParseBaseUnits] (canonical decimal
// integer, "0" for zero) or the encoding.TextMarshaler methods. For columns
// that must sort numerically as text, use [Amount.SortKey].
//
// Boundaries: humans and wire formats use decimal strings in whole units
// ("149.00"); convert with [ParseAmount] and [Amount.Format], always passing
// the decimals of the asset — they are configuration, never assumed.
type Amount struct {
	// digits is the canonical base-10 representation without leading zeros;
	// the empty string means zero.
	digits string
}

// maxAmountDigits is the number of decimal digits of 2^256-1.
const maxAmountDigits = 78

// maxDecimals bounds asset decimals; it keeps every whole-unit amount the
// core accepts inside the Amount range.
const maxDecimals = 36

// maxUint256 is 2^256-1.
var maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// NewAmount returns the Amount of n base units.
func NewAmount(n uint64) Amount {
	return amountFromBig(new(big.Int).SetUint64(n))
}

// AmountFromBig converts a big integer of base units. It fails for nil,
// negative values and values above 2^256-1.
func AmountFromBig(n *big.Int) (Amount, error) {
	if n == nil || n.Sign() < 0 || n.Cmp(maxUint256) > 0 {
		return Amount{}, newError(ErrInvalid, "amount out of range")
	}
	return amountFromBig(n), nil
}

// amountFromBig converts an in-range big integer.
func amountFromBig(n *big.Int) Amount {
	if n.Sign() == 0 {
		return Amount{}
	}
	return Amount{digits: n.String()}
}

// ParseBaseUnits parses a canonical non-negative decimal integer of base
// units, as produced by [Amount.String]. Leading zeros (other than "0"
// itself), signs, spaces and anything non-digit are rejected.
func ParseBaseUnits(s string) (Amount, error) {
	if s == "" || len(s) > maxAmountDigits || !isDigits(s) || (len(s) > 1 && s[0] == '0') {
		return Amount{}, newError(ErrInvalid, "invalid base-unit amount")
	}
	if s == "0" {
		return Amount{}, nil
	}
	n, _ := new(big.Int).SetString(s, 10)
	if n.Cmp(maxUint256) > 0 {
		return Amount{}, newError(ErrInvalid, "amount out of range")
	}
	return Amount{digits: s}, nil
}

// ParseAmount parses a decimal string in whole units ("149", "149.00",
// "0.000000000000000001") into base units for an asset with the given
// decimals.
//
// The grammar is strict: digits, optionally followed by a dot and at least
// one digit. It rejects the empty string, signs, exponents, whitespace,
// thousands separators, a leading or trailing dot, leading zeros ("007",
// "00.5"), more fractional digits than decimals, and values above 2^256-1.
// Zero is accepted here; callers that need a positive amount check
// [Amount.IsZero].
func ParseAmount(s string, decimals int) (Amount, error) {
	if decimals < 0 || decimals > maxDecimals {
		return Amount{}, newError(ErrInvalid, "unsupported number of decimals")
	}
	if s == "" {
		return Amount{}, newError(ErrInvalid, "amount is required")
	}
	if len(s) > maxAmountDigits+1 {
		return Amount{}, newError(ErrInvalid, "amount is too long")
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || !isDigits(whole) {
		return Amount{}, newError(ErrInvalid, "amount must be a decimal number like 149.00")
	}
	if len(whole) > 1 && whole[0] == '0' {
		return Amount{}, newError(ErrInvalid, "amount must not have leading zeros")
	}
	if hasDot && (frac == "" || !isDigits(frac)) {
		return Amount{}, newError(ErrInvalid, "amount must be a decimal number like 149.00")
	}
	if len(frac) > decimals {
		return Amount{}, newError(ErrInvalid, "amount has more than %d decimal places", decimals)
	}
	digits := strings.TrimLeft(whole+frac+strings.Repeat("0", decimals-len(frac)), "0")
	if digits == "" {
		return Amount{}, nil
	}
	if len(digits) > maxAmountDigits {
		return Amount{}, newError(ErrInvalid, "amount is too large")
	}
	n, _ := new(big.Int).SetString(digits, 10)
	if n.Cmp(maxUint256) > 0 {
		return Amount{}, newError(ErrInvalid, "amount is too large")
	}
	return Amount{digits: digits}, nil
}

// MustParseAmount is like [ParseAmount] but panics on error. It is meant for
// tests and constants.
func MustParseAmount(s string, decimals int) Amount {
	a, err := ParseAmount(s, decimals)
	if err != nil {
		panic("core: MustParseAmount(" + s + "): " + err.Error())
	}
	return a
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// String returns the amount in base units as a canonical decimal integer.
func (a Amount) String() string {
	if a.digits == "" {
		return "0"
	}
	return a.digits
}

// Format renders the amount in whole units for an asset with the given
// decimals. Trailing fractional zeros are trimmed, but at least two
// fractional digits are kept (fewer if the asset has fewer decimals):
// "149.00", "0.50", "12.345678", "0.000000000000000001".
func (a Amount) Format(decimals int) string {
	if decimals < 0 {
		decimals = 0
	}
	digits := a.digits
	if len(digits) <= decimals {
		digits = strings.Repeat("0", decimals-len(digits)+1) + digits
	}
	whole, frac := digits[:len(digits)-decimals], digits[len(digits)-decimals:]
	keep := min(2, decimals)
	frac = strings.TrimRight(frac, "0")
	if len(frac) < keep {
		frac += strings.Repeat("0", keep-len(frac))
	}
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

// SortKey returns the base-unit value left-padded with zeros to 78 digits,
// so that lexicographic order of keys equals numeric order of amounts.
// Stores use it for "sort by amount" on text columns.
func (a Amount) SortKey() string {
	return strings.Repeat("0", maxAmountDigits-len(a.digits)) + a.digits
}

// Big returns the amount in base units as a new big integer.
func (a Amount) Big() *big.Int {
	n := new(big.Int)
	if a.digits != "" {
		n.SetString(a.digits, 10)
	}
	return n
}

// IsZero reports whether the amount is zero.
func (a Amount) IsZero() bool { return a.digits == "" }

// Cmp compares a and b: -1 if a < b, 0 if a == b, +1 if a > b.
func (a Amount) Cmp(b Amount) int {
	switch {
	case len(a.digits) != len(b.digits):
		if len(a.digits) < len(b.digits) {
			return -1
		}
		return 1
	default:
		return strings.Compare(a.digits, b.digits)
	}
}

// Add returns a+b. The sum saturates at 2^256-1, which no real token supply
// reaches.
func (a Amount) Add(b Amount) Amount {
	n := a.Big()
	n.Add(n, b.Big())
	if n.Cmp(maxUint256) > 0 {
		n.Set(maxUint256)
	}
	return amountFromBig(n)
}

// Sub returns a-b, or zero when b > a (saturating subtraction).
func (a Amount) Sub(b Amount) Amount {
	if a.Cmp(b) <= 0 {
		return Amount{}
	}
	n := a.Big()
	return amountFromBig(n.Sub(n, b.Big()))
}

// MarshalText implements encoding.TextMarshaler using the base-unit form.
func (a Amount) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler using the base-unit form.
func (a *Amount) UnmarshalText(text []byte) error {
	v, err := ParseBaseUnits(string(text))
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// effectiveTolerance is the underpayment tolerance that applies to a payment
// of the given amount: the configured tolerance, capped at
// amount/ToleranceCapDivisor (rounded down).
func effectiveTolerance(amount, tolerance Amount) Amount {
	limit := amount.Big()
	limit.Quo(limit, big.NewInt(ToleranceCapDivisor))
	if c := amountFromBig(limit); tolerance.Cmp(c) > 0 {
		return c
	}
	return tolerance
}

// dustLimit is the smallest amount that is not dust for a payment of the
// given amount: DustCapUnits whole units or amount/DustDivisor (rounded
// up), whichever is less, and never less than one base unit.
func dustLimit(amount Amount, decimals int) Amount {
	limit := ceilDiv(amount, DustDivisor)
	if c := wholeUnits(DustCapUnits, decimals); limit.Cmp(c) > 0 {
		limit = c
	}
	if limit.IsZero() {
		return amountFromBig(big.NewInt(1))
	}
	return limit
}

// fundedLimit is what a payment created through a public link must have
// received to count as funded: amount/FundedDivisor (rounded up), and at
// least FundedFloorUnits whole units.
func fundedLimit(amount Amount, decimals int) Amount {
	limit := ceilDiv(amount, FundedDivisor)
	if f := wholeUnits(FundedFloorUnits, decimals); limit.Cmp(f) < 0 {
		limit = f
	}
	return limit
}

// ceilDiv returns a/d rounded up.
func ceilDiv(a Amount, d int64) Amount {
	v := a.Big()
	v.Add(v, big.NewInt(d-1))
	return amountFromBig(v.Quo(v, big.NewInt(d)))
}

// wholeUnits returns n whole units of an asset in base units.
func wholeUnits(n uint64, decimals int) Amount {
	v := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return amountFromBig(v.Mul(v, new(big.Int).SetUint64(n)))
}
