package core

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Scope is a permission an API key can hold.
type Scope string

// API key scopes.
const (
	// ScopePaymentsRead: list and read payments, events and statistics.
	ScopePaymentsRead Scope = "payments:read"
	// ScopePaymentsWrite: create and cancel payments.
	ScopePaymentsWrite Scope = "payments:write"
	// ScopeLinksWrite: read, create, edit, disable and delete payment
	// links.
	ScopeLinksWrite Scope = "links:write"
	// ScopeWebhooksManage: manage webhook endpoints and their deliveries.
	//
	// It is a powerful scope. A key holding it can register an endpoint of
	// its own and so receives every payment event: it effectively includes
	// read access to payment data ([ScopePaymentsRead]). It can also point
	// an existing endpoint somewhere else or disable it. What it cannot do
	// is read, reveal or rotate the signing secret of an endpoint — only
	// the operator can — so it cannot forge events that a receiver would
	// accept as coming from hmnpay.
	ScopeWebhooksManage Scope = "webhooks:manage"
)

// Scopes lists every scope, in display order.
var Scopes = []Scope{ScopePaymentsRead, ScopePaymentsWrite, ScopeLinksWrite, ScopeWebhooksManage}

// ActorKind says what kind of principal is calling the service.
type ActorKind string

// Actor kinds.
const (
	// ActorOperator is the signed-in instance operator: may do everything
	// except inject chain facts and run the delivery workflow.
	ActorOperator ActorKind = "operator"
	// ActorAPIKey is a merchant integration, limited to its key's scopes.
	ActorAPIKey ActorKind = "api_key"
	// ActorSystem is trusted in-process code: the chain watcher, the
	// webhook dispatcher, a Telegram bot run by the operator, a CLI, or a
	// program embedding hmnpay. It may do everything.
	ActorSystem ActorKind = "system"
)

// Actor is the authenticated caller of a [Service] method.
//
// Transports authenticate (with [Service.AuthenticateAPIKey],
// [Service.AuthenticateSession] or by construction via [SystemActor]) and
// attach the actor to the context with [WithActor]; the service authorises.
// A transport therefore cannot forget a permission check: without an actor
// in the context every non-public method fails with [ErrUnauthorized].
type Actor struct {
	Kind ActorKind
	// ID is the operator id, the API key id, or the system component name.
	ID string
	// Name is a display name used in audit fields such as [Resolution.By].
	Name string
	// Scopes are the API key's scopes; unused for other kinds.
	Scopes []Scope
	// SessionID is set for operators authenticated by a session.
	SessionID string
	// SessionExpiresAt is when that session stops working if it is not
	// used again: its absolute expiry or the idle timeout counted from its
	// last use, whichever is earlier. Zero for actors that are not
	// session-based.
	SessionExpiresAt time.Time
}

// SystemActor returns the actor for trusted in-process code. name
// identifies the component ("chain-watcher", "telegram-bot").
func SystemActor(name string) Actor {
	return Actor{Kind: ActorSystem, ID: name, Name: name}
}

type actorKey struct{}

// WithActor returns a context that carries the caller's identity.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor attached to ctx.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// permission is what a service method requires from its caller.
type permission struct {
	scope    Scope // satisfied by an API key holding it
	operator bool  // operator-level: no API key qualifies
	system   bool  // system only
}

var (
	permOperator = permission{operator: true}
	permSystem   = permission{system: true}
)

func permScope(s Scope) permission { return permission{scope: s} }

// authorize checks the actor in ctx against the required permission.
func authorize(ctx context.Context, need permission) (Actor, error) {
	a, ok := ActorFrom(ctx)
	if !ok {
		return Actor{}, newError(ErrUnauthorized, "")
	}
	switch a.Kind {
	case ActorSystem:
		return a, nil
	case ActorOperator:
		if !need.system {
			return a, nil
		}
	case ActorAPIKey:
		if need.scope != "" && slices.Contains(a.Scopes, need.scope) {
			return a, nil
		}
		if need.scope != "" {
			return a, &Error{Kind: KindForbidden, Code: "missing_scope", Message: fmt.Sprintf("this API key lacks the %q scope", need.scope)}
		}
	default:
		return Actor{}, newError(ErrUnauthorized, "")
	}
	return a, newError(ErrForbidden, "")
}

// APIKey is a merchant credential. The secret itself is returned once, by
// [Service.CreateAPIKey], and only its SHA-256 is stored.
type APIKey struct {
	// ID is "key_" plus 26 random characters.
	ID string
	// Name is the operator's label.
	Name string
	// Mode is "live" or "test": informational, derived from the instance
	// network when the key was created (test on a test network). It is not
	// a per-key switch.
	Mode string
	// Prefix is the beginning of the secret ("hmn_live_3fa9"), for display.
	Prefix string
	// Last4 is the end of the secret, for display.
	Last4 string
	// SecretHash is the lowercase hex SHA-256 of the full secret. Unique.
	SecretHash string
	// Scopes are the permissions the key holds.
	Scopes []Scope
	// CreatedAt is the creation time.
	CreatedAt time.Time
	// LastUsedAt is the last successful authentication, to the minute.
	LastUsedAt time.Time
	// ExpiresAt, when non-zero, is when the key stops working.
	ExpiresAt time.Time
	// RevokedAt, when non-zero, is when the key was revoked.
	RevokedAt time.Time
}

// Has reports whether the key holds scope.
func (k APIKey) Has(scope Scope) bool { return slices.Contains(k.Scopes, scope) }

// Operator is the single administrator account of the instance.
type Operator struct {
	// ID is "op_" plus 26 random characters.
	ID string
	// Email is the login, lowercase.
	Email string
	// PasswordHash is the output of the [PasswordHasher].
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// Generation counts the password changes and resets of the account.
	// Whatever a transport bound to the previous credentials — a "known
	// device" marker — stops working when it moves, see
	// [Service.DeviceBinding].
	Generation int64
	// RecoveryHash is the SHA-256 of the outstanding one-time recovery
	// code ([Service.MintRecoveryCode]), hex; empty when there is none.
	RecoveryHash string
	// RecoveryExpiresAt is when that code stops working.
	RecoveryExpiresAt time.Time
	// RecoveryAttempts counts the logins attempted with it.
	RecoveryAttempts int
}

// Session is a dashboard login. The token is returned once, by
// [Service.Login] or [Service.CompleteSetup]; only its SHA-256 is stored.
type Session struct {
	// ID is "sess_" plus 26 random characters.
	ID string
	// OperatorID is the account the session belongs to.
	OperatorID string
	// TokenHash is the lowercase hex SHA-256 of the session token. Unique.
	TokenHash string
	// UserAgent and IP describe the client that logged in (informational).
	UserAgent string
	IP        string
	CreatedAt time.Time
	// ExpiresAt is the absolute end of the session, however much it is
	// used.
	ExpiresAt time.Time
	// LastSeenAt is when the session last authenticated a request (to a
	// granularity of a few minutes). A session that is not used for
	// [Config.SessionIdleTimeout] ends early. Zero in sessions stored
	// before this field existed, which count from CreatedAt.
	LastSeenAt time.Time
}

// hashSecret returns the lowercase hex SHA-256 of a high-entropy secret
// (API key or session token). Such secrets need no slow KDF.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// secretsEqual compares two strings in constant time.
func secretsEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// PBKDF2Hasher is the [PasswordHasher] built on the standard library:
// PBKDF2-HMAC-SHA256 with a random 16-byte salt, encoded as
//
//	$pbkdf2-sha256$i=<iterations>$<salt b64>$<key b64>
//
// The zero value uses [DefaultPBKDF2Iterations] and crypto/rand. Verify
// honours the iteration count stored in the hash, so the default can be
// raised without invalidating existing passwords.
type PBKDF2Hasher struct {
	// Iterations overrides the work factor; 0 means the default. Values
	// below the default are only sensible in tests.
	Iterations int
}

// DefaultPBKDF2Iterations follows the OWASP recommendation for
// PBKDF2-HMAC-SHA256.
const DefaultPBKDF2Iterations = 600_000

const (
	pbkdf2Prefix  = "$pbkdf2-sha256$"
	pbkdf2SaltLen = 16
	pbkdf2KeyLen  = 32
)

// Hash implements [PasswordHasher].
func (h PBKDF2Hasher) Hash(password string) (string, error) {
	iter := h.Iterations
	if iter <= 0 {
		iter = DefaultPBKDF2Iterations
	}
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%si=%d$%s$%s", pbkdf2Prefix, iter, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// Verify implements [PasswordHasher].
func (h PBKDF2Hasher) Verify(hash, password string) (bool, error) {
	rest, ok := strings.CutPrefix(hash, pbkdf2Prefix)
	if !ok {
		return false, fmt.Errorf("core: unsupported password hash format")
	}
	parts := strings.Split(rest, "$")
	if len(parts) != 3 || !strings.HasPrefix(parts[0], "i=") {
		return false, fmt.Errorf("core: malformed password hash")
	}
	iter, err := strconv.Atoi(parts[0][2:])
	if err != nil || iter < 1 || iter > 100_000_000 {
		return false, fmt.Errorf("core: malformed password hash")
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[1])
	want, err2 := enc.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false, fmt.Errorf("core: malformed password hash")
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
