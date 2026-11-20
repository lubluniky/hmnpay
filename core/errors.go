package core

import (
	"errors"
	"fmt"
	"strings"
)

// Kind classifies an [Error]. Adapters map kinds, not individual errors, onto
// their transport (HTTP status, bot reply, exit code).
type Kind string

// Error kinds. See [Error] for the recommended HTTP mapping.
const (
	// KindInvalid: the input is malformed or breaks a business rule that the
	// caller can fix by changing the request. Carries [Error.Fields].
	KindInvalid Kind = "invalid"
	// KindNotFound: the referenced object does not exist.
	KindNotFound Kind = "not_found"
	// KindConflict: the request is well-formed but contradicts current state
	// (duplicate slug, idempotency key reused with other parameters, link no
	// longer available, setup already done, …).
	KindConflict Kind = "conflict"
	// KindUnauthorized: missing, unknown, expired or revoked credentials.
	KindUnauthorized Kind = "unauthorized"
	// KindForbidden: the caller is authenticated but may not do this.
	KindForbidden Kind = "forbidden"
	// KindSetupRequired: the instance has not completed first-run setup.
	KindSetupRequired Kind = "setup_required"
	// KindUnavailable: a dependency the operation needs is not configured or
	// is failing (for example no chain prober is wired in).
	KindUnavailable Kind = "unavailable"
	// KindInternal: everything else; never produced by the core on purpose.
	// [KindOf] reports it for errors that are not an [*Error].
	KindInternal Kind = "internal"
)

// FieldError describes one invalid input field.
type FieldError struct {
	// Field is the input field in snake_case, as transports spell it
	// ("amount", "redirect_url", "metadata.plan").
	Field string
	// Code is a stable machine-readable reason ("required", "invalid",
	// "too_long", "too_small", "too_large", "taken", …).
	Code string
	// Message is a human-readable sentence, safe to show to the caller.
	Message string
}

// Error is the single error type the core returns for expected failures.
//
// Match errors with [errors.Is] against the sentinels below, or switch on
// [KindOf]. Recommended HTTP mapping:
//
//	KindInvalid        422 (400 for syntactically broken bodies)
//	KindNotFound       404
//	KindConflict       409
//	KindUnauthorized   401
//	KindForbidden      403
//	KindSetupRequired  503
//	KindUnavailable    503
//	KindInternal       500 (log it; do not echo the message)
//
// Code and Message are stable and safe to expose; Err is the internal cause
// and must not be shown to callers.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Fields  []FieldError
	Err     error
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Kind))
	if e.Code != "" && e.Code != string(e.Kind) {
		b.WriteString(" (" + e.Code + ")")
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	for i, f := range e.Fields {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(f.Field + ": " + f.Message)
	}
	if e.Err != nil {
		b.WriteString(": " + e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the underlying cause, if any.
func (e *Error) Unwrap() error { return e.Err }

// Is reports whether e matches target. A target without a Code matches every
// error of the same Kind; a target with a Code must match both.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return e.Kind == t.Kind && (t.Code == "" || t.Code == e.Code)
}

// Sentinel errors. The first group matches by kind; the second group names
// specific conditions (and still matches its kind's sentinel).
//
// Store implementations must return ErrNotFound and ErrConflict (or errors
// wrapping them) as specified on the [Store] interfaces.
var (
	ErrInvalid       = &Error{Kind: KindInvalid, Message: "invalid input"}
	ErrNotFound      = &Error{Kind: KindNotFound, Message: "not found"}
	ErrConflict      = &Error{Kind: KindConflict, Message: "conflict"}
	ErrUnauthorized  = &Error{Kind: KindUnauthorized, Message: "authentication required"}
	ErrForbidden     = &Error{Kind: KindForbidden, Message: "not allowed"}
	ErrSetupRequired = &Error{Kind: KindSetupRequired, Code: "setup_required", Message: "first-run setup has not been completed"}
	ErrUnavailable   = &Error{Kind: KindUnavailable, Message: "temporarily unavailable"}

	// ErrSetupCompleted: a setup-only operation was called after setup.
	ErrSetupCompleted = &Error{Kind: KindConflict, Code: "setup_completed", Message: "setup has already been completed"}
	// ErrIdempotencyMismatch: an idempotency key was reused with different
	// parameters than the payment it first created.
	ErrIdempotencyMismatch = &Error{Kind: KindConflict, Code: "idempotency_key_reused", Message: "idempotency key was already used with different parameters"}
	// ErrLinkDisabled, ErrLinkExpired, ErrLinkLimitReached: a payment link
	// cannot take a new payment.
	ErrLinkDisabled     = &Error{Kind: KindConflict, Code: "link_disabled", Message: "this payment link is disabled"}
	ErrLinkExpired      = &Error{Kind: KindConflict, Code: "link_expired", Message: "this payment link has expired"}
	ErrLinkLimitReached = &Error{Kind: KindConflict, Code: "link_limit_reached", Message: "this payment link has reached its payment limit"}
	// ErrTransferFinal: a chain fact contradicts a transfer that already
	// reached finality — its block was finalised by the chain ([Evaluate],
	// rule 1), so this takes a reorganisation that replaced a finalised
	// block. State is left unchanged; the watcher should log it.
	ErrTransferFinal = &Error{Kind: KindConflict, Code: "transfer_final", Message: "transfer is already final"}
	// ErrChainMismatch: a chain fact refers to another chain or token
	// contract than the payment (or the instance) uses.
	ErrChainMismatch = &Error{Kind: KindInvalid, Code: "chain_mismatch", Message: "fact does not belong to the expected chain and token contract"}
	// ErrHeadTimeAhead: [Service.AdvanceHead] was given a head whose block
	// timestamp is implausibly far ahead of the server's wall clock. The
	// head is not recorded. The chain adapter should treat it as a lying
	// endpoint (or a wrong server clock) and alert.
	ErrHeadTimeAhead = &Error{Kind: KindInvalid, Code: "head_time_ahead", Message: "block timestamp is ahead of the server clock"}
	// ErrWatchListFull: the public payment links' share of the chain watch
	// list is used up ([Config.MaxWatchedAddresses]); no payment can be
	// created through a public link until addresses age out.
	ErrWatchListFull = &Error{Kind: KindUnavailable, Code: "watch_list_full", Message: "too many payments are open right now; try again later"}
	// ErrChainNotReady: no payment can be created yet, because the chain
	// adapter has not processed a block of the configured network: right
	// after first-run setup or a change of the network, for the moment it
	// takes the chain watcher to agree with its endpoints on where the
	// chain ends — or for as long as it cannot. A payment records the
	// processed head as the block from which its address is scanned
	// ([Payment.CreatedAtBlock]); before there is one, nothing the
	// instance knows bounds the block in which the payer may pay, so it
	// hands out no address. Retry in a moment; if it lasts, the node status
	// says what the chain watcher is waiting for.
	ErrChainNotReady = &Error{Kind: KindUnavailable, Code: "chain_not_ready", Message: "the chain watcher has not processed a block of this network yet; try again in a moment"}
	// ErrClockBehind: no payment can be created while the server clock is
	// behind the chain by more than the tolerance for block times
	// ([Config.MaxHeadTimeAhead]): by the processed head's block time, or
	// by the newest block's as the chain adapter's endpoints show it. Its
	// deadline and the window in which late funds are looked for would be
	// counted from a clock that is behind, and end early. Fix the server's
	// time synchronisation; the node status warns about clock skew.
	ErrClockBehind = &Error{Kind: KindUnavailable, Code: "clock_behind", Message: "the server clock is behind the chain; no payment can be created until it is right"}
	// ErrNetworkInvalid: no payment can be created while the stored network
	// settings fail the rules new ones are held to ([NetworkSettings.Problems];
	// settings an earlier version accepted). The message says what is
	// wrong; repair it in the network settings.
	ErrNetworkInvalid = &Error{Kind: KindUnavailable, Code: "network_invalid", Message: "the network settings cannot be used; fix them in the network settings"}
	// ErrConfigurationReplaced: a chain adapter reported a fact made under a
	// chain configuration that has been replaced since ([WithChainRevision]).
	// Nothing of it is applied; the adapter builds a generation for the
	// current configuration.
	ErrConfigurationReplaced = &Error{Kind: KindConflict, Code: "configuration_replaced", Message: "the chain configuration the fact was made under has been replaced"}
	// ErrBusy: too many password operations are in progress; retry
	// shortly.
	ErrBusy = &Error{Kind: KindUnavailable, Code: "busy", Message: "busy; retry in a moment"}
	// ErrRecoveryCodeInvalid: the one-time recovery code presented with a
	// login ([LoginInput.RecoveryCode]) is wrong, expired, used up or was
	// never minted.
	ErrRecoveryCodeInvalid = &Error{Kind: KindUnauthorized, Code: "recovery_code_invalid", Message: "the recovery code is wrong, has expired or was already used"}
	// ErrSetupTokenRequired / ErrSetupTokenInvalid: first-run setup needs
	// the instance's setup token ([Config.SetupToken]) and it is missing or
	// wrong.
	ErrSetupTokenRequired = &Error{Kind: KindUnauthorized, Code: "setup_token_required", Message: "the setup token is required"}
	ErrSetupTokenInvalid  = &Error{Kind: KindForbidden, Code: "setup_token_invalid", Message: "the setup token is not valid"}
	// ErrNoWebhookSecret: [VerifyWebhookSignature] was called without a
	// usable signing secret. That is a configuration error on the
	// receiver's side, not a bad signature.
	ErrNoWebhookSecret = &Error{Kind: KindUnavailable, Code: "webhook_secret_missing", Message: "no webhook signing secret is configured; refusing to verify"}
	// ErrUnknownAddress: a transfer was reported for an address that no
	// payment owns. Watchers may ignore it.
	ErrUnknownAddress = &Error{Kind: KindNotFound, Code: "unknown_address", Message: "no payment uses this address"}
	// ErrInvalidCredentials: wrong email/password, unknown API key or
	// session token. Deliberately unspecific.
	ErrInvalidCredentials = &Error{Kind: KindUnauthorized, Code: "invalid_credentials", Message: "invalid credentials"}
	// ErrCredentialsExpired: the API key or session is past its expiry.
	ErrCredentialsExpired = &Error{Kind: KindUnauthorized, Code: "credentials_expired", Message: "credentials have expired"}
	// ErrCredentialsRevoked: the API key was revoked.
	ErrCredentialsRevoked = &Error{Kind: KindUnauthorized, Code: "credentials_revoked", Message: "credentials have been revoked"}
	// ErrNothingToResolve: resolve/reopen was called on a payment that is
	// not in the matching state.
	ErrNothingToResolve = &Error{Kind: KindConflict, Code: "nothing_to_resolve", Message: "payment has no open issue in that state"}
	// ErrPaymentHasFunds: a payment cannot be cancelled because funds have
	// been seen on its address (it is detected or confirming, or late
	// funds are waiting to be recorded). Dust is not funds.
	ErrPaymentHasFunds = &Error{Kind: KindConflict, Code: "payment_has_funds", Message: "funds have been seen for this payment; it can no longer be cancelled"}
	// ErrPaymentSettled: a payment cannot be cancelled because it already
	// reached a terminal status by itself.
	ErrPaymentSettled = &Error{Kind: KindConflict, Code: "payment_settled", Message: "this payment is already settled"}
	// ErrEndpointDisabled: the webhook endpoint is disabled.
	ErrEndpointDisabled = &Error{Kind: KindConflict, Code: "endpoint_disabled", Message: "webhook endpoint is disabled"}
	// ErrOpenPayments: the network cannot be switched while payments are
	// still open on the current one.
	ErrOpenPayments = &Error{Kind: KindConflict, Code: "open_payments", Message: "payments are still open on the current network"}
)

// KindOf returns the [Kind] of err, or [KindInternal] if err is not (and does
// not wrap) an [*Error]. KindOf(nil) is the empty string.
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// FieldErrors returns the field errors carried by err, if any.
func FieldErrors(err error) []FieldError {
	var e *Error
	if errors.As(err, &e) {
		return e.Fields
	}
	return nil
}

// newError copies a sentinel and attaches a message.
func newError(base *Error, format string, args ...any) *Error {
	e := *base
	if format != "" {
		e.Message = fmt.Sprintf(format, args...)
	}
	return &e
}

// notFound builds a not-found error for a named object.
func notFound(what, id string) *Error {
	return &Error{Kind: KindNotFound, Code: what + "_not_found", Message: fmt.Sprintf("%s %q not found", strings.ReplaceAll(what, "_", " "), id)}
}

// conflict builds a conflict error with a specific code.
func conflict(code, format string, args ...any) *Error {
	return &Error{Kind: KindConflict, Code: code, Message: fmt.Sprintf(format, args...)}
}

// invalidField builds a validation error for a single field.
func invalidField(field, code, format string, args ...any) *Error {
	var v validation
	v.add(field, code, format, args...)
	return v.err()
}

// validation collects field errors.
type validation struct{ fields []FieldError }

func (v *validation) add(field, code, format string, args ...any) {
	v.fields = append(v.fields, FieldError{Field: field, Code: code, Message: fmt.Sprintf(format, args...)})
}

func (v *validation) ok() bool { return len(v.fields) == 0 }

// err returns nil when no field error was recorded.
func (v *validation) err() *Error {
	if len(v.fields) == 0 {
		return nil
	}
	return &Error{Kind: KindInvalid, Code: "validation_failed", Fields: v.fields}
}

// errOrNil converts the typed nil pitfall away: it returns a plain nil error
// when there is nothing to report.
func (v *validation) errOrNil() error {
	if e := v.err(); e != nil {
		return e
	}
	return nil
}

// internal wraps an unexpected error from a port with context.
func internal(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("core: %s: %w", op, err)
}
