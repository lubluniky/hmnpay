package core_test

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/core"
)

const maxUint256 = "115792089237316195423570985008687907853269984665640564039457584007913129639935"

func TestParseAmount(t *testing.T) {
	tests := []struct {
		in       string
		decimals int
		want     string // base units; "" means an error is expected
	}{
		// 18 decimals: BSC-USD.
		{"149", 18, "149000000000000000000"},
		{"149.00", 18, "149000000000000000000"},
		{"149.5", 18, "149500000000000000000"},
		{"0.5", 18, "500000000000000000"},
		{"0.000000000000000001", 18, "1"},
		{"1.000000000000000001", 18, "1000000000000000001"},
		{"0", 18, "0"},
		{"0.0", 18, "0"},
		{"0.000000000000000000", 18, "0"},
		{"1000000000", 18, "1000000000000000000000000000"},
		{"123456789012345678901234567890.123456789012345678", 18, "123456789012345678901234567890123456789012345678"},
		// Other decimals: never assumed.
		{"149.00", 6, "149000000"},
		{"0.000001", 6, "1"},
		{"12.345678", 8, "1234567800"},
		{"7", 0, "7"},
		{"1.5", 1, "15"},
		// The very top of the range.
		{maxUint256, 0, maxUint256},
		{maxUint256[:60] + "." + maxUint256[60:], 18, maxUint256},

		// Garbage.
		{"", 18, ""},
		{" ", 18, ""},
		{" 1", 18, ""},
		{"1 ", 18, ""},
		{"1\n", 18, ""},
		{"abc", 18, ""},
		{"1a", 18, ""},
		{"0x10", 18, ""},
		{"１２", 18, ""}, // full-width digits
		{"1e3", 18, ""},
		{"1E3", 18, ""},
		{"1,000", 18, ""},
		{"1,5", 18, ""},
		{"1_000", 18, ""},
		{"1.2.3", 18, ""},
		{".", 18, ""},
		{".5", 18, ""},
		{"5.", 18, ""},
		{"1..5", 18, ""},
		{"NaN", 18, ""},
		{"Inf", 18, ""},
		{"$5", 18, ""},
		// Signs.
		{"-1", 18, ""},
		{"+1", 18, ""},
		{"-0", 18, ""},
		{"1.-5", 18, ""},
		// Leading zeros.
		{"007", 18, ""},
		{"00", 18, ""},
		{"00.5", 18, ""},
		{"01.5", 18, ""},
		// Too many decimals.
		{"0.0000000000000000001", 18, ""},
		{"1.0000000000000000000", 18, ""}, // 19 places, even though they are zeros
		{"0.0000001", 6, ""},
		{"1.0", 0, ""},
		// Absurd sizes.
		{"115792089237316195423570985008687907853269984665640564039457584007913129639936", 0, ""}, // 2^256
		{maxUint256, 18, ""},
		{strings.Repeat("9", 500), 18, ""},
		{"1" + strings.Repeat("0", 78), 0, ""},
		// Unsupported decimals.
		{"1", -1, ""},
		{"1", 37, ""},
	}
	for _, tc := range tests {
		got, err := core.ParseAmount(tc.in, tc.decimals)
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("ParseAmount(%q, %d) = %s, want an error", tc.in, tc.decimals, got)
		case tc.want == "":
			if core.KindOf(err) != core.KindInvalid {
				t.Errorf("ParseAmount(%q, %d): error kind %q, want invalid", tc.in, tc.decimals, core.KindOf(err))
			}
		case err != nil:
			t.Errorf("ParseAmount(%q, %d): %v", tc.in, tc.decimals, err)
		case got.String() != tc.want:
			t.Errorf("ParseAmount(%q, %d) = %s, want %s", tc.in, tc.decimals, got, tc.want)
		}
	}
}

func TestAmountFormat(t *testing.T) {
	tests := []struct {
		base     string
		decimals int
		want     string
	}{
		{"0", 18, "0.00"},
		{"1", 18, "0.000000000000000001"},
		{"10", 18, "0.00000000000000001"},
		{"500000000000000000", 18, "0.50"},
		{"149000000000000000000", 18, "149.00"},
		{"149500000000000000000", 18, "149.50"},
		{"149990000000000000000", 18, "149.99"},
		{"149123000000000000000", 18, "149.123"},
		{"1000000000000000001", 18, "1.000000000000000001"},
		{"999999999999999999", 18, "0.999999999999999999"},
		{"1000000000000000000000000000", 18, "1000000000.00"},
		{maxUint256, 18, "115792089237316195423570985008687907853269984665640564039457.584007913129639935"},
		{"149000000", 6, "149.00"},
		{"1", 6, "0.000001"},
		{"1234567800", 8, "12.345678"},
		{"7", 0, "7"},
		{"0", 0, "0"},
		{"15", 1, "1.5"},
		{"10", 1, "1.0"},
		{"5", 2, "0.05"},
		{"100", 2, "1.00"},
	}
	for _, tc := range tests {
		a, err := core.ParseBaseUnits(tc.base)
		if err != nil {
			t.Fatalf("ParseBaseUnits(%q): %v", tc.base, err)
		}
		if got := a.Format(tc.decimals); got != tc.want {
			t.Errorf("Amount(%s).Format(%d) = %q, want %q", tc.base, tc.decimals, got, tc.want)
		}
		// Whatever Format prints, ParseAmount reads back exactly.
		back, err := core.ParseAmount(a.Format(tc.decimals), tc.decimals)
		if err != nil || back != a {
			t.Errorf("round trip of %s at %d decimals: %s, %v", tc.base, tc.decimals, back, err)
		}
	}
}

func TestParseBaseUnits(t *testing.T) {
	for _, ok := range []string{"0", "1", "149000000000000000000", maxUint256} {
		a, err := core.ParseBaseUnits(ok)
		if err != nil || a.String() != ok {
			t.Errorf("ParseBaseUnits(%q) = %s, %v", ok, a, err)
		}
	}
	for _, bad := range []string{"", "00", "01", "-1", "+1", "1.0", " 1", "1e3", "0x1", maxUint256[:77] + "6", maxUint256 + "0"} {
		if a, err := core.ParseBaseUnits(bad); err == nil {
			t.Errorf("ParseBaseUnits(%q) = %s, want an error", bad, a)
		}
	}
}

func TestAmountArithmetic(t *testing.T) {
	amt := func(s string) core.Amount {
		a, err := core.ParseBaseUnits(s)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	var zero core.Amount
	if !zero.IsZero() || zero.String() != "0" || zero != amt("0") || zero != core.NewAmount(0) {
		t.Error("the zero value must be the amount zero")
	}
	if core.NewAmount(1<<63).String() != "9223372036854775808" {
		t.Error("NewAmount")
	}
	if got := amt("999999999999999999").Add(amt("1")); got != amt("1000000000000000000") {
		t.Errorf("Add carried wrong: %s", got)
	}
	if got := amt("5").Add(zero); got != amt("5") {
		t.Errorf("Add zero: %s", got)
	}
	if got := amt(maxUint256).Add(amt("1")); got != amt(maxUint256) {
		t.Errorf("Add must saturate, got %s", got)
	}
	if got := amt("1000000000000000000").Sub(amt("1")); got != amt("999999999999999999") {
		t.Errorf("Sub borrowed wrong: %s", got)
	}
	if got := amt("5").Sub(amt("5")); !got.IsZero() || got != zero {
		t.Errorf("Sub to zero must be the canonical zero: %#v", got)
	}
	if got := amt("5").Sub(amt("6")); !got.IsZero() {
		t.Errorf("Sub must saturate at zero, got %s", got)
	}
	cmp := []struct {
		a, b string
		want int
	}{
		{"0", "0", 0}, {"1", "0", 1}, {"0", "1", -1}, {"9", "10", -1}, {"10", "9", 1},
		{"100", "99", 1}, {"123", "124", -1}, {maxUint256, "1", 1}, {"1000000000000000000000000000000", "999999999999999999999999999999", 1},
	}
	for _, c := range cmp {
		if got := amt(c.a).Cmp(amt(c.b)); got != c.want {
			t.Errorf("Cmp(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
		// SortKey orders text the way Cmp orders numbers.
		if got := strings.Compare(amt(c.a).SortKey(), amt(c.b).SortKey()); got != c.want {
			t.Errorf("SortKey order of (%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if k := amt("9").SortKey(); len(k) != 78 || !strings.HasSuffix(k, "9") || len(amt(maxUint256).SortKey()) != 78 {
		t.Errorf("SortKey must be 78 digits wide: %q", k)
	}

	if a, err := core.AmountFromBig(big.NewInt(42)); err != nil || a != amt("42") {
		t.Errorf("AmountFromBig(42) = %s, %v", a, err)
	}
	over := new(big.Int).Lsh(big.NewInt(1), 256)
	for _, bad := range []*big.Int{nil, big.NewInt(-1), over} {
		if _, err := core.AmountFromBig(bad); err == nil {
			t.Errorf("AmountFromBig(%v) succeeded", bad)
		}
	}
	b := amt("42").Big()
	b.SetInt64(7)
	if amt("42").Big().Int64() != 42 {
		t.Error("Big must return a copy")
	}
}

func TestAmountText(t *testing.T) {
	type doc struct {
		A core.Amount            `json:"a"`
		M map[string]core.Amount `json:"m"`
	}
	in := doc{A: core.MustParseAmount("149.5", 18), M: map[string]core.Amount{"zero": {}}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"a":"149500000000000000000","m":{"zero":"0"}}` {
		t.Fatalf("JSON form: %s", raw)
	}
	var out doc
	if err := json.Unmarshal(raw, &out); err != nil || out.A != in.A || out.M["zero"] != (core.Amount{}) {
		t.Fatalf("JSON round trip: %+v, %v", out, err)
	}
	for _, bad := range []string{`{"a":"1.5"}`, `{"a":"-1"}`, `{"a":"01"}`, `{"a":""}`} {
		if err := json.Unmarshal([]byte(bad), &out); err == nil {
			t.Errorf("Unmarshal(%s) succeeded", bad)
		}
	}
}

func TestMustParseAmountPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustParseAmount did not panic on garbage")
		}
	}()
	core.MustParseAmount("garbage", 18)
}
