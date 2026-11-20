package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// writeJSON encodes v with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

// listBody is the envelope of every list response.
type listBody[T any] struct {
	Data []T      `json:"data"`
	Page *pageDTO `json:"page,omitempty"`
}

type pageDTO struct {
	Limit   int  `json:"limit"`
	Offset  int  `json:"offset"`
	Total   int  `json:"total"`
	HasMore bool `json:"has_more"`
}

func newPage(limit, offset, total int) *pageDTO {
	return &pageDTO{Limit: limit, Offset: offset, Total: total, HasMore: offset+limit < total}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// decodeJSON strictly decodes a JSON request body into dst: the content
// type must be application/json, the body at most limit bytes, exactly one
// JSON value, and no unknown fields. Failures are 400 (415, 413).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	return decode(w, r, dst, limit, false)
}

// decodeOptionalJSON is decodeJSON for endpoints whose body may be empty
// (an empty body leaves dst untouched).
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	return decode(w, r, dst, limit, true)
}

func decode(w http.ResponseWriter, r *http.Request, dst any, limit int64, optional bool) error {
	if r.ContentLength > limit {
		return &apiError{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", Message: "request body is too large"}
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close()
	if optional && r.ContentLength == 0 && r.Header.Get("Content-Type") == "" {
		// Nothing declared; make sure nothing is actually sent either.
		var one [1]byte
		if n, _ := io.ReadFull(body, one[:]); n == 0 {
			return nil
		}
		return &apiError{Status: http.StatusUnsupportedMediaType, Code: "unsupported_media_type", Message: "Content-Type must be application/json"}
	}
	ct := r.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || mt != "application/json" {
		return &apiError{Status: http.StatusUnsupportedMediaType, Code: "unsupported_media_type", Message: "Content-Type must be application/json"}
	}
	if cs, ok := params["charset"]; ok && !strings.EqualFold(cs, "utf-8") {
		return &apiError{Status: http.StatusUnsupportedMediaType, Code: "unsupported_media_type", Message: "request body must be UTF-8"}
	}
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) && optional {
			return nil
		}
		return decodeError(err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return decodeError(err)
		}
		return errBadRequest("invalid_json", "request body must contain a single JSON value")
	}
	return nil
}

func decodeError(err error) error {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		return &apiError{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", Message: "request body is too large"}
	case errors.Is(err, io.EOF):
		return errBadRequest("empty_body", "request body is required")
	case errors.As(err, &syn), errors.Is(err, io.ErrUnexpectedEOF):
		return errBadRequest("invalid_json", "request body is not valid JSON")
	case errors.As(err, &typ):
		field := typ.Field
		if field == "" {
			return errBadRequest("invalid_json", "request body must be a JSON object")
		}
		return errBadRequest("invalid_type", "a field has the wrong type",
			fieldError{Field: field, Code: "invalid_type", Message: field + " must be a " + jsonKind(typ.Type.Kind().String())})
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return errBadRequest("unknown_field", "the request contains an unknown field",
			fieldError{Field: name, Code: "unknown", Message: "unknown field " + strconv.Quote(name)})
	}
	return errBadRequest("invalid_json", "request body could not be decoded")
}

func jsonKind(goKind string) string {
	switch goKind {
	case "string":
		return "string"
	case "bool":
		return "boolean"
	case "slice", "array":
		return "array"
	case "map", "struct", "ptr":
		return "object"
	}
	return "number"
}

// ---- query parameters ----

func queryInt(q url.Values, name string, def int) (int, error) {
	s := strings.TrimSpace(q.Get(name))
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, errQuery(name, name+" must be an integer")
	}
	return n, nil
}

func queryInt64(q url.Values, name string) (int64, error) {
	s := strings.TrimSpace(q.Get(name))
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, errQuery(name, name+" must be a non-negative integer")
	}
	return n, nil
}

// queryList returns a comma-separated and/or repeated parameter.
func queryList(q url.Values, name string) []string {
	var out []string
	for _, v := range q[name] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// queryTime parses an RFC 3339 timestamp or a YYYY-MM-DD day (UTC). With
// endOfDay a bare day means the start of the following day, so a "to" day
// is inclusive.
func queryTime(q url.Values, name string, endOfDay bool) (time.Time, error) {
	s := strings.TrimSpace(q.Get(name))
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if d, err := time.Parse("2006-01-02", s); err == nil {
		if endOfDay {
			d = d.AddDate(0, 0, 1)
		}
		return d, nil
	}
	return time.Time{}, errQuery(name, name+" must be an RFC 3339 timestamp or a YYYY-MM-DD date")
}

func clamp(limit, def, maxLimit int) int {
	switch {
	case limit <= 0:
		return def
	case limit > maxLimit:
		return maxLimit
	}
	return limit
}

// ---- ids ----

// requestID returns a sane inbound X-Request-Id or a fresh one.
func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); id != "" && len(id) <= 64 && saneID(id) {
		return id
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func saneID(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// validPaymentID reports whether s has the shape of a payment id:
// "pay_" plus 26 characters of the core's id alphabet.
func validPaymentID(s string) bool {
	rest, ok := strings.CutPrefix(s, "pay_")
	if !ok || len(rest) != 26 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if !strings.ContainsRune("0123456789abcdefghjkmnpqrstvwxyz", rune(rest[i])) {
			return false
		}
	}
	return true
}

// validSlug mirrors the core's slug shape (1–64 lowercase letters, digits
// and inner hyphens).
func validSlug(s string) bool {
	if len(s) == 0 || len(s) > 64 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
