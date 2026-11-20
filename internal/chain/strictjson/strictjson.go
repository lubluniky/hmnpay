// Package strictjson decodes JSON objects the way a payment system should
// read bytes it did not write: a member is recognised only under its exact
// name, and at most once.
//
// encoding/json is more generous than that. It matches member names to struct
// fields case-insensitively ("BLOCKNUMBER" sets the field tagged
// "blockNumber") and, when a name occurs several times, silently lets the
// last occurrence win. Both are harmless for trusted input and both mean, for
// untrusted input, that two readers of the same bytes can see two different
// documents — and that a document can say two things at once. An RPC endpoint
// that answers with such an object is not a JSON-RPC node, and a checkpoint
// that contains one was not written by the watcher.
//
// Types opt in by implementing json.Unmarshaler with Object, so the rule
// follows them wherever they are decoded (in slices, nested, by
// json.Unmarshal).
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Field names one member of a JSON object — exactly — and where its value is
// decoded into (a pointer, as for json.Unmarshal).
type Field struct {
	Name string
	Into any
}

// Fields are the members of an object that are asked for. (A slice, not a
// map: a handful of names is found faster by comparing than by hashing, and
// the list costs no allocation per decoded object.)
type Fields []Field

func (f Fields) find(name string) (any, bool) {
	for i := range f {
		if f[i].Name == name {
			return f[i].Into, true
		}
	}
	return nil, false
}

// Unknown says what to do with a member that is not in Fields.
type Unknown bool

const (
	// IgnoreUnknown skips members that are not asked for. This is for
	// formats that other parties extend: a JSON-RPC node sends many members
	// this client does not read, and new ones with every release.
	IgnoreUnknown Unknown = false
	// RejectUnknown refuses them. This is for formats this program writes
	// itself: a member it does not know was written by something else.
	RejectUnknown Unknown = true
)

// Object decodes the JSON object in data into fields.
//
// It fails if data is not exactly one JSON object; if a member of Fields
// occurs more than once; if a member's name differs from a name in Fields
// only by case (under either mode: such a member is either a mistake or an
// attempt to be read differently by different readers); or if a member is
// unknown and unknown is RejectUnknown. Members that do not occur leave their
// destination untouched. A JSON null in place of the object decodes into
// nothing, as with encoding/json.
func Object(data []byte, fields Fields, unknown Unknown) error {
	// Well-formedness first, by the standard library: after that the
	// object can be taken apart knowing where every value ends. (Taking it
	// apart by hand rather than with json.Decoder's token stream is what
	// keeps this as cheap as a plain json.Unmarshal: every answer of every
	// endpoint goes through here.)
	if !json.Valid(data) {
		return errors.New("strictjson: not valid JSON")
	}
	i := skipSpace(data, 0)
	if data[i] != '{' {
		if string(bytes.TrimSpace(data)) == "null" {
			return nil
		}
		return errors.New("strictjson: not a JSON object")
	}
	// Bounded by len(fields): only names that are asked for are remembered.
	var seen [16]string
	found := seen[:0]
	for i = skipSpace(data, i+1); data[i] != '}'; {
		// "name" : value
		end := skipString(data, i)
		name, err := memberName(data[i:end])
		if err != nil {
			return err
		}
		i = skipSpace(data, end) // at the colon
		i = skipSpace(data, i+1) // at the value
		end = skipValue(data, i)
		value := data[i:end]
		if i = skipSpace(data, end); data[i] == ',' {
			i = skipSpace(data, i+1)
		}

		dst, known := fields.find(name)
		if !known {
			for _, f := range fields {
				if strings.EqualFold(f.Name, name) {
					return fmt.Errorf("strictjson: member %q is not %q", clip(name), f.Name)
				}
			}
			if unknown == RejectUnknown {
				return fmt.Errorf("strictjson: unknown member %q", clip(name))
			}
			continue
		}
		if slices.Contains(found, name) {
			return fmt.Errorf("strictjson: member %q occurs more than once", name)
		}
		found = append(found, name)
		if err := decode(value, dst); err != nil {
			return fmt.Errorf("strictjson: member %q: %w", name, err)
		}
	}
	return nil
}

// decode is json.Unmarshal(value, dst) for a value that is known to be valid
// JSON. The destinations nearly every member of an RPC answer has — a string,
// an optional string, a raw value — are filled in directly when the value is
// plain (a string without escapes, null); everything else, and every doubt,
// goes to json.Unmarshal, so the result is the same either way.
func decode(value []byte, dst any) error {
	switch d := dst.(type) {
	case *json.RawMessage:
		*d = append((*d)[:0], value...)
		return nil
	case **string:
		if s, ok := plainString(value); ok {
			*d = &s
			return nil
		}
		if string(value) == "null" {
			*d = nil
			return nil
		}
	case *string:
		if s, ok := plainString(value); ok {
			*d = s
			return nil
		}
	}
	return json.Unmarshal(value, dst)
}

// plainString returns the string a JSON string denotes if it needs no
// decoding: no escapes, valid UTF-8.
func plainString(value []byte) (string, bool) {
	if len(value) < 2 || value[0] != '"' || bytes.IndexByte(value, '\\') >= 0 || !utf8.Valid(value) {
		return "", false
	}
	return string(value[1 : len(value)-1]), true
}

// The helpers below walk over JSON that json.Valid has accepted: they find
// where things end, they do not check them again.

func skipSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index after the string that starts at data[i].
func skipString(data []byte, i int) int {
	for i++; data[i] != '"'; i++ {
		if data[i] == '\\' {
			i++ // the escaped character is not the end, whatever it is
		}
	}
	return i + 1
}

// skipValue returns the index after the value that starts at data[i].
func skipValue(data []byte, i int) int {
	switch data[i] {
	case '"':
		return skipString(data, i)
	case '{', '[':
		for depth := 0; ; {
			switch data[i] {
			case '"':
				i = skipString(data, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
			i++
		}
	}
	// A number or a literal: it ends where the syntax around it resumes.
	for i < len(data) && !strings.ContainsRune(",}] \t\n\r", rune(data[i])) {
		i++
	}
	return i
}

// memberName decodes the quoted name of an object member. A name is compared
// as the string it denotes, however it is written ("\u0062lock" is "block").
func memberName(quoted []byte) (string, error) {
	if bytes.IndexByte(quoted, '\\') < 0 && utf8.Valid(quoted) {
		return string(quoted[1 : len(quoted)-1]), nil
	}
	var name string
	if err := json.Unmarshal(quoted, &name); err != nil {
		return "", err
	}
	return name, nil
}

// clip bounds a name taken from the input before it is quoted in an error.
func clip(s string) string {
	const max = 40
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
