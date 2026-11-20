package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// apiError is a transport-level error with a fixed HTTP rendering.
type apiError struct {
	Status     int
	Code       string
	Message    string
	Fields     []fieldError
	RetryAfter time.Duration
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

type fieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorBody struct {
	Error     errorDetail `json:"error"`
	RequestID string      `json:"request_id"`
}

type errorDetail struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Fields  []fieldError `json:"fields,omitempty"`
}

// renamed maps field names reported by the core onto request keys that
// differ ("ttl" → "ttl_seconds"), so every field error names a request key.
type renamed struct {
	err   error
	names map[string]string
}

func (r *renamed) Error() string { return r.err.Error() }
func (r *renamed) Unwrap() error { return r.err }

func withFieldNames(err error, names map[string]string) error {
	if err == nil {
		return nil
	}
	return &renamed{err: err, names: names}
}

// publicError marks an error from an unauthenticated public endpoint: a
// not-found is rendered generically, without echoing the identifier.
type publicError struct{ err error }

func (p *publicError) Error() string { return p.err.Error() }
func (p *publicError) Unwrap() error { return p.err }

func asPublic(err error) error {
	if err == nil {
		return nil
	}
	return &publicError{err: err}
}

var errPublicNotFound = &apiError{Status: http.StatusNotFound, Code: "not_found", Message: "not found"}

// statusFor maps a core error kind onto an HTTP status.
func statusFor(k core.Kind) int {
	switch k {
	case core.KindInvalid:
		return http.StatusUnprocessableEntity
	case core.KindNotFound:
		return http.StatusNotFound
	case core.KindConflict:
		return http.StatusConflict
	case core.KindUnauthorized:
		return http.StatusUnauthorized
	case core.KindForbidden:
		return http.StatusForbidden
	case core.KindSetupRequired, core.KindUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// toAPIError renders any error for the wire. Only the Code, Message and
// Fields of a core error are exposed; its wrapped cause never is.
func (h *Handler) toAPIError(r *http.Request, err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return &apiError{Status: 499, Code: "client_closed_request", Message: "request canceled"}
	}
	kind := core.KindOf(err)
	var ce *core.Error
	if kind == core.KindInternal || !errors.As(err, &ce) {
		ri := info(r)
		h.log.LogAttrs(r.Context(), slog.LevelError, "http internal error",
			slog.String("request_id", ri.id), slog.String("route", ri.route), slog.String("error", err.Error()))
		return &apiError{Status: http.StatusInternalServerError, Code: "internal", Message: "internal server error"}
	}
	var pe *publicError
	if kind == core.KindNotFound && errors.As(err, &pe) {
		return errPublicNotFound
	}
	out := &apiError{Status: statusFor(kind), Code: ce.Code, Message: ce.Message}
	if out.Code == "" {
		out.Code = string(kind)
	}
	switch {
	case errors.Is(err, core.ErrBusy):
		// Too many password operations at once: shed load, ask to retry.
		out.Status, out.RetryAfter = http.StatusServiceUnavailable, 2*time.Second
	case errors.Is(err, core.ErrChainNotReady):
		// Normally a few seconds after setup or a change of network; long
		// enough apart that a client retrying as told stays inside the
		// public link route's own budgets (see publicLinkPayment).
		out.RetryAfter = chainNotReadyRetry
	case errors.Is(err, core.ErrNetworkInvalid):
		// Waiting does not fix it either: the operator has to repair the
		// network settings.
		out.RetryAfter = time.Minute
	case errors.Is(err, core.ErrClockBehind):
		// Waiting does not fix it: the server clock must be corrected.
		out.RetryAfter = time.Minute
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "a payment was refused: the server clock is behind the chain",
			slog.String("request_id", info(r).id))
	case errors.Is(err, core.ErrWatchListFull):
		out.RetryAfter = time.Minute
		h.log.LogAttrs(r.Context(), slog.LevelWarn, "payment link refused a payment: the chain watch list is at its cap",
			slog.String("request_id", info(r).id))
	}
	if out.Message == "" {
		out.Message = defaultMessage(kind)
	}
	var rn *renamed
	errors.As(err, &rn)
	for _, f := range ce.Fields {
		name := f.Field
		if rn != nil {
			if n, ok := rn.names[name]; ok {
				name = n
			}
		}
		out.Fields = append(out.Fields, fieldError{Field: name, Code: f.Code, Message: f.Message})
	}
	if kind == core.KindInvalid && len(out.Fields) > 0 && ce.Message == "" {
		out.Message = "the request contains invalid fields"
	}
	return out
}

func defaultMessage(k core.Kind) string {
	switch k {
	case core.KindInvalid:
		return "invalid input"
	case core.KindNotFound:
		return "not found"
	case core.KindConflict:
		return "conflict"
	case core.KindUnauthorized:
		return "authentication required"
	case core.KindForbidden:
		return "not allowed"
	case core.KindSetupRequired:
		return "first-run setup has not been completed"
	case core.KindUnavailable:
		return "temporarily unavailable"
	}
	return "internal server error"
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	ae := h.toAPIError(r, err)
	if ae.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hmnpay"`)
	}
	if ae.RetryAfter > 0 {
		secs := int((ae.RetryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	writeErrorBody(w, info(r).id, ae)
}

func writeErrorBody(w http.ResponseWriter, requestID string, ae *apiError) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(ae.Status)
	_ = json.NewEncoder(w).Encode(errorBody{
		Error:     errorDetail{Code: ae.Code, Message: ae.Message, Fields: ae.Fields},
		RequestID: requestID,
	})
}

// Transport errors.
func errBadRequest(code, msg string, fields ...fieldError) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: code, Message: msg, Fields: fields}
}

func errQuery(field, msg string) *apiError {
	return &apiError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: "the request contains invalid fields",
		Fields: []fieldError{{Field: field, Code: "invalid", Message: msg}}}
}

func errRequired(field string) *apiError {
	return &apiError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: "the request contains invalid fields",
		Fields: []fieldError{{Field: field, Code: "required", Message: field + " is required"}}}
}

func errRateLimited(retry time.Duration) *apiError {
	return &apiError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "too many requests; retry later", RetryAfter: retry}
}
