package strictjson_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lubluniky/hmnpay/internal/chain/strictjson"
)

type inner struct {
	N uint64
}

func (i *inner) UnmarshalJSON(data []byte) error {
	return strictjson.Object(data, strictjson.Fields{{Name: "n", Into: &i.N}}, strictjson.RejectUnknown)
}

type doc struct {
	Number *string
	Text   string
	Count  uint64
	Raw    json.RawMessage
	List   []inner
	Ptr    *inner
}

func (d *doc) fields() strictjson.Fields {
	return strictjson.Fields{
		{Name: "blockNumber", Into: &d.Number},
		{Name: "count", Into: &d.Count},
		{Name: "raw", Into: &d.Raw},
		{Name: "list", Into: &d.List},
		{Name: "ptr", Into: &d.Ptr},
		{Name: "text", Into: &d.Text},
	}
}

func TestObject(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		unknown strictjson.Unknown
		wantErr string // "" for success
		check   func(t *testing.T, d doc)
	}{
		{name: "exact names", in: `{"blockNumber":"0xa","count":3,"raw":[1, 2],"list":[{"n":1},{"n":2}],"ptr":{"n":9}}`,
			check: func(t *testing.T, d doc) {
				if d.Number == nil || *d.Number != "0xa" || d.Count != 3 || string(d.Raw) != "[1, 2]" || len(d.List) != 2 || d.List[1].N != 2 || d.Ptr == nil || d.Ptr.N != 9 {
					t.Fatalf("decoded %+v", d)
				}
			}},
		{name: "absent members stay untouched", in: ` { } `, check: func(t *testing.T, d doc) {
			if d.Number != nil || d.Count != 0 || d.Raw != nil || d.Ptr != nil {
				t.Fatalf("decoded %+v", d)
			}
		}},
		{name: "null members", in: `{"blockNumber":null,"raw":null,"ptr":null,"list":null}`, check: func(t *testing.T, d doc) {
			if d.Number != nil || string(d.Raw) != "null" || d.Ptr != nil || d.List != nil {
				t.Fatalf("decoded %+v", d)
			}
		}},
		{name: "null instead of the object", in: `null`, check: func(t *testing.T, d doc) {}},
		{name: "unknown members are skipped when allowed", in: `{"blockTimestamp":"0x1","count":1,"extra":{"count":7,"count":8}}`,
			check: func(t *testing.T, d doc) {
				if d.Count != 1 {
					t.Fatalf("count = %d", d.Count)
				}
			}},
		{name: "unknown members are refused when not", in: `{"count":1,"extra":true}`, unknown: strictjson.RejectUnknown, wantErr: `unknown member "extra"`},
		{name: "another case is not the same name", in: `{"BloCkNumBer":"0xa"}`, wantErr: `is not "blockNumber"`},
		{name: "another case next to the exact name", in: `{"blockNumber":"0xa","BLOCKNUMBER":"0xb"}`, wantErr: `is not "blockNumber"`},
		{name: "unicode case folding", in: `{"blocKNumber":"0xa"}`, wantErr: `is not "blockNumber"`}, // U+212A KELVIN SIGN
		{name: "a member twice", in: `{"count":1,"count":2}`, wantErr: `"count" occurs more than once`},
		{name: "a member twice with the same value", in: `{"count":1,"count":1}`, wantErr: `occurs more than once`},
		{name: "strictness follows nested types", in: `{"list":[{"N":1}]}`, wantErr: `is not "n"`},
		{name: "nested unknown member", in: `{"ptr":{"n":1,"m":2}}`, wantErr: `unknown member "m"`},
		{name: "wrong type", in: `{"count":"3"}`, wantErr: `member "count"`},
		{name: "an array", in: `[]`, wantErr: "not a JSON object"},
		{name: "a string", in: `"x"`, wantErr: "not a JSON object"},
		{name: "empty", in: ``, wantErr: "not valid JSON"},
		{name: "truncated", in: `{"count":1`, wantErr: "not valid JSON"},
		{name: "data after the object", in: `{"count":1} {"count":2}`, wantErr: "not valid JSON"},
		{name: "data after null", in: `null 1`, wantErr: "not valid JSON"},
		{name: "null with whitespace", in: " null\n", check: func(t *testing.T, d doc) {}},
		{name: "an escaped name is the name it denotes", in: `{"\u0063ount":4}`, check: func(t *testing.T, d doc) {
			if d.Count != 4 {
				t.Fatalf("count = %d", d.Count)
			}
		}},
		{name: "an escaped name twice", in: `{"count":1,"\u0063ount":4}`, wantErr: "more than once"},
		{name: "an escaped name in another case", in: `{"\u0043ount":4}`, wantErr: `is not "count"`},
		{name: "braces, brackets, quotes and escapes inside values", in: `{"x":"}]\"\\","y":[{"a":"]"},["}"]],"raw":{"k":"\\\""},"count":2,"z":-1.5e3,"t":true}`,
			check: func(t *testing.T, d doc) {
				if d.Count != 2 || string(d.Raw) != `{"k":"\\\""}` {
					t.Fatalf("decoded %+v (raw %s)", d, d.Raw)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d doc
			err := strictjson.Object([]byte(tc.in), d.fields(), tc.unknown)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Object(%s): %v", tc.in, err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("Object(%s) = %v, want an error containing %q", tc.in, err, tc.wantErr)
			case tc.wantErr == "":
				tc.check(t, d)
			}
		})
	}
}

// TestAgainstEncodingJSON pins down what the package is for: documents that
// encoding/json reads without complaint — as something they do not say.
func TestAgainstEncodingJSON(t *testing.T) {
	type lenient struct {
		Count uint64 `json:"count"`
	}
	for _, in := range []string{`{"COUNT":5}`, `{"count":1,"count":5}`, `{"count":1,"Count":5}`} {
		var l lenient
		if err := json.Unmarshal([]byte(in), &l); err != nil || l.Count != 5 {
			t.Fatalf("encoding/json: %s -> %+v, %v (the premise of this package no longer holds)", in, l, err)
		}
		var d doc
		if err := strictjson.Object([]byte(in), d.fields(), strictjson.IgnoreUnknown); err == nil {
			t.Fatalf("Object accepted %s as count=%d", in, d.Count)
		}
	}
}

// A long or hostile member name does not end up in an error message whole.
func TestErrorsAreBounded(t *testing.T) {
	name := strings.Repeat("A", 10_000) + "\n\x1b[31m"
	in, _ := json.Marshal(map[string]int{name: 1})
	var d doc
	err := strictjson.Object(in, d.fields(), strictjson.RejectUnknown)
	if err == nil || len(err.Error()) > 200 || strings.ContainsAny(err.Error(), "\n\x1b") {
		t.Fatalf("error = %q", err)
	}
}

// FuzzObject: the object is taken apart by hand (after json.Valid has passed
// it), so the walker is held against encoding/json on arbitrary input. It
// must not panic or read out of bounds, and whatever it accepts must mean the
// same to encoding/json — which is more lenient, never different.
//
//	go test ./internal/chain/strictjson -run '^$' -fuzz FuzzObject -fuzztime 3m
func FuzzObject(f *testing.F) {
	for _, s := range []string{
		`{"blockNumber":"0xa","count":3,"raw":[1, 2],"list":[{"n":1},{"n":2}],"ptr":{"n":9}}`, `{}`, ` { } `, `null`, `[]`, `"x"`, ``, `{`, `{"count":1`,
		`{"count":1,"count":2}`, `{"COUNT":1}`, `{"\u0063ount":4}`, `{"x":"}]\"\\","y":[{"a":"]"},["}"]],"count":2}`, `{"count":1} x`,
		`{"raw":{"a":{"b":[[[]]]}},"count":7}`, `{"count":-0}`, `{"count":1e2}`, `{"raw":"\ud83d\ude00","blockNumber":"\u0000"}`, "{\"raw\":\"\xff\"}", "{\"\xff\":1}",
		`{"":1,"count":2}`, `{"list":[null,{"n":3}]}`, `{"ptr":null,"list":[]}`, `{"count":18446744073709551615}`, `{"count":18446744073709551616}`,
	} {
		f.Add([]byte(s), false)
		f.Add([]byte(s), true)
	}
	f.Fuzz(func(t *testing.T, data []byte, reject bool) {
		var d doc
		if err := strictjson.Object(data, d.fields(), strictjson.Unknown(reject)); err != nil {
			return
		}
		if !json.Valid(data) {
			t.Fatalf("Object accepted invalid JSON: %q", data)
		}
		var lenient struct {
			Number *string          `json:"blockNumber"`
			Count  uint64           `json:"count"`
			Raw    json.RawMessage  `json:"raw"`
			List   []map[string]any `json:"list"`
			Ptr    map[string]any   `json:"ptr"`
		}
		if err := json.Unmarshal(data, &lenient); err != nil {
			t.Fatalf("Object accepted %q, encoding/json does not: %v", data, err)
		}
		if (d.Number == nil) != (lenient.Number == nil) || (d.Number != nil && *d.Number != *lenient.Number) || d.Count != lenient.Count ||
			!bytes.Equal(d.Raw, lenient.Raw) || len(d.List) != len(lenient.List) || (d.Ptr == nil) != (lenient.Ptr == nil) {
			t.Fatalf("Object and encoding/json read %q differently:\n%+v\n%+v", data, d, lenient)
		}
	})
}
