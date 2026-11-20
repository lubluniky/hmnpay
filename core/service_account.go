package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

// ---- API keys ----

// CreateAPIKeyInput describes a new API key.
type CreateAPIKeyInput struct {
	// Name is the operator's label (≤ 80 characters). Required.
	Name string
	// Scopes are the permissions to grant; at least one.
	Scopes []Scope
	// ExpiresAt, when non-zero, is when the key stops working; it must be
	// in the future.
	ExpiresAt time.Time
}

// CreateAPIKey creates an API key and returns it together with its secret
// ("hmn_live_…", or "hmn_test_…" on a test network). The secret is shown
// exactly once: only its hash is stored. Operator only.
func (s *Service) CreateAPIKey(ctx context.Context, in CreateAPIKeyInput) (APIKey, string, error) {
	var key APIKey
	if _, err := authorize(ctx, permOperator); err != nil {
		return key, "", err
	}
	now := s.now()
	var v validation
	key.Name = cleanText(&v, "name", in.Name, 80, true)
	for _, sc := range in.Scopes {
		if !slices.Contains(Scopes, sc) {
			v.add("scopes", "invalid", "unknown scope %q", sc)
		} else if !slices.Contains(key.Scopes, sc) {
			key.Scopes = append(key.Scopes, sc)
		}
	}
	if len(key.Scopes) == 0 && v.ok() {
		v.add("scopes", "required", "grant at least one scope")
	}
	if !in.ExpiresAt.IsZero() {
		key.ExpiresAt = in.ExpiresAt.UTC().Truncate(time.Millisecond)
		if !key.ExpiresAt.After(now) {
			v.add("expires_at", "invalid", "expiry must be in the future")
		}
	}
	if err := v.errOrNil(); err != nil {
		return key, "", err
	}
	var secret string
	err := s.update(ctx, func(u *unit) error {
		st, err := u.loadSettings(ctx)
		if err != nil {
			return err
		}
		key.Mode = "live"
		if IsTestnet(st.Network.ChainID) {
			key.Mode = "test"
		}
		random, err := s.randomString(25)
		if err != nil {
			return err
		}
		secret = "hmn_" + key.Mode + "_" + random
		if key.ID, err = s.newID(prefixAPIKey); err != nil {
			return err
		}
		key.Prefix = secret[:len("hmn_live_")+4]
		key.Last4 = secret[len(secret)-4:]
		key.SecretHash = hashSecret(secret)
		key.CreatedAt = u.now
		return u.InsertAPIKey(ctx, key)
	})
	if err != nil {
		return APIKey{}, "", err
	}
	return key, secret, nil
}

// ListAPIKeys returns all API keys, newest first, revoked ones included.
// Operator only.
func (s *Service) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	if _, err := authorize(ctx, permOperator); err != nil {
		return nil, err
	}
	var keys []APIKey
	err := s.store.View(ctx, func(r Reader) error {
		var err error
		keys, err = r.ListAPIKeys(ctx)
		return err
	})
	return keys, err
}

// RevokeAPIKey revokes a key immediately and permanently. Revoking a
// revoked key is a no-op. Operator only.
func (s *Service) RevokeAPIKey(ctx context.Context, id string) (APIKey, error) {
	var key APIKey
	if _, err := authorize(ctx, permOperator); err != nil {
		return key, err
	}
	err := s.update(ctx, func(u *unit) error {
		var err error
		key, err = u.GetAPIKey(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return notFound("api_key", id)
		}
		if err != nil || !key.RevokedAt.IsZero() {
			return err
		}
		key.RevokedAt = u.now
		return u.UpdateAPIKey(ctx, key)
	})
	return key, err
}

// lastUsedGranularity limits how often authentication writes LastUsedAt.
const lastUsedGranularity = time.Minute

// AuthenticateAPIKey resolves an API key secret to an [Actor] holding the
// key's scopes. It fails with [ErrInvalidCredentials] for unknown secrets,
// [ErrCredentialsRevoked] and [ErrCredentialsExpired] otherwise (all match
// [ErrUnauthorized]). The key's LastUsedAt is refreshed at most once a
// minute. Public.
//
// The secret is looked up by its SHA-256, so the comparison of secret
// material is constant-time by construction.
func (s *Service) AuthenticateAPIKey(ctx context.Context, secret string) (Actor, error) {
	secret = strings.TrimSpace(secret)
	if !strings.HasPrefix(secret, "hmn_") || len(secret) > 128 {
		return Actor{}, newError(ErrInvalidCredentials, "")
	}
	hash := hashSecret(secret)
	now := s.now()
	check := func(ctx context.Context, r APIKeyReader, now time.Time) (APIKey, error) {
		key, err := r.GetAPIKeyByHash(ctx, hash)
		switch {
		case errors.Is(err, ErrNotFound):
			return key, newError(ErrInvalidCredentials, "")
		case err != nil:
			return key, err
		case !secretsEqual(key.SecretHash, hash):
			return key, newError(ErrInvalidCredentials, "")
		case !key.RevokedAt.IsZero():
			return key, newError(ErrCredentialsRevoked, "")
		case !key.ExpiresAt.IsZero() && !now.Before(key.ExpiresAt):
			return key, newError(ErrCredentialsExpired, "")
		}
		return key, nil
	}
	var key APIKey
	err := s.store.View(ctx, func(r Reader) error {
		var err error
		key, err = check(ctx, r, now)
		return err
	})
	if err != nil {
		return Actor{}, err
	}
	if now.Sub(key.LastUsedAt) >= lastUsedGranularity {
		err = s.update(ctx, func(u *unit) error {
			// Checked again by the time of the write: a key that expired
			// while this waited for the store is not marked as used.
			k, err := check(ctx, u, u.now)
			if err != nil {
				return err
			}
			k.LastUsedAt = u.now
			key = k
			return u.UpdateAPIKey(ctx, k)
		})
		if err != nil {
			return Actor{}, err
		}
	}
	return Actor{Kind: ActorAPIKey, ID: key.ID, Name: key.Name, Scopes: slices.Clone(key.Scopes)}, nil
}

// ---- operator account and sessions ----

// LoginInput carries the operator's credentials.
type LoginInput struct {
	Email    string
	Password string
	// UserAgent and IP describe the client; stored with the session.
	UserAgent string
	IP        string
	// RecoveryCode is the one-time code from [Service.MintRecoveryCode],
	// if the operator presented one. It never replaces the password. A
	// wrong, expired or used-up code fails the login with
	// [ErrRecoveryCodeInvalid] before the password is looked at; a valid
	// one is used up by a successful login.
	RecoveryCode string
}

// LoginResult is a fresh session.
type LoginResult struct {
	// Token is the session secret ("hmns_…"), shown exactly once. The
	// transport puts it in a cookie (or hands it to the client) and passes
	// it to [Service.AuthenticateSession] on every request.
	Token string
	// Session describes the session; its ExpiresAt is the cookie lifetime.
	Session Session
	// Operator is the account that logged in.
	Operator Operator
}

// Login verifies the operator's email and password and opens a session.
// Wrong email and wrong password are indistinguishable — the password is
// verified either way, so both take the same time — and fail with
// [ErrInvalidCredentials]. Before setup it fails with [ErrSetupRequired].
//
// Password verification is slow by design and this method is
// unauthenticated, so the service runs at most [Config.MaxConcurrentKDF]
// of them at a time (4 by default); a caller that finds every slot taken
// for two seconds gets [ErrBusy]. Public — transports must still
// rate-limit it.
//
// With [LoginInput.RecoveryCode] the login is the same, plus: the code is
// checked first and one of its [RecoveryCodeMaxAttempts] attempts is spent
// before the password is verified, and a successful login uses the code
// up. Transports use [Service.RecoveryCodeValid] to let such a login past
// the throttles that anonymous traffic can exhaust.
func (s *Service) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	var res LoginResult
	email := strings.ToLower(strings.TrimSpace(in.Email))
	var op Operator
	err := s.store.View(ctx, func(r Reader) error {
		var err error
		op, err = r.GetOperator(ctx)
		if errors.Is(err, ErrNotFound) {
			return newError(ErrSetupRequired, "")
		}
		return err
	})
	if err != nil {
		return res, err
	}
	recovery := strings.TrimSpace(in.RecoveryCode)
	if recovery != "" {
		if err := s.spendRecoveryAttempt(ctx, recovery); err != nil {
			return res, err
		}
	}
	if len(in.Password) > maxPasswordLength {
		return res, newError(ErrInvalidCredentials, "")
	}
	ok, err := s.verifyPassword(ctx, op.PasswordHash, in.Password)
	if err != nil {
		return res, err
	}
	if !ok || !secretsEqual(op.Email, email) {
		return res, newError(ErrInvalidCredentials, "")
	}
	err = s.update(ctx, func(u *unit) error {
		// The password was verified outside any transaction, against the
		// account as it was read before the KDF ran — hundreds of
		// milliseconds ago. A session is opened only for the credentials
		// that are the account's now: a password change or reset that
		// committed in between ends this login like every session, and the
		// reset's "every session is gone" holds for the
		// one that was about to be created.
		cur, err := u.GetOperator(ctx)
		if errors.Is(err, ErrNotFound) {
			return newError(ErrSetupRequired, "")
		}
		if err != nil {
			return err
		}
		if !sameCredentials(cur, op) {
			return newError(ErrInvalidCredentials, "")
		}
		if recovery != "" {
			// Single use: whoever gets here first takes the code.
			if cur.RecoveryHash == "" || !secretsEqual(cur.RecoveryHash, hashSecret(recovery)) {
				return newError(ErrRecoveryCodeInvalid, "")
			}
			cur.clearRecovery()
			if err := u.SaveOperator(ctx, cur); err != nil {
				return err
			}
		}
		res, err = s.openSession(ctx, u, cur.redacted(), in.UserAgent, in.IP)
		return err
	})
	return res, err
}

// sameCredentials reports whether the account read now (cur) still has the
// credentials of the account a password was verified against (seen): the
// same account, password hash and credential generation. The generation
// alone would do — it moves with every change and reset — and the hash is
// compared as well so that the check does not rest on one counter.
func sameCredentials(cur, seen Operator) bool {
	return cur.ID == seen.ID && cur.Generation == seen.Generation && secretsEqual(cur.PasswordHash, seen.PasswordHash)
}

// openSession creates and stores a session for op.
func (s *Service) openSession(ctx context.Context, u *unit, op Operator, userAgent, ip string) (LoginResult, error) {
	random, err := s.randomString(32)
	if err != nil {
		return LoginResult{}, err
	}
	token := "hmns_" + random
	id, err := s.newID(prefixSession)
	if err != nil {
		return LoginResult{}, err
	}
	sess := Session{
		ID:         id,
		OperatorID: op.ID,
		TokenHash:  hashSecret(token),
		UserAgent:  truncate(userAgent, 300),
		IP:         truncate(ip, 64),
		CreatedAt:  u.now,
		ExpiresAt:  u.now.Add(s.sessionTTL),
		LastSeenAt: u.now,
	}
	if err := u.InsertSession(ctx, sess); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Token: token, Session: sess, Operator: op}, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// sessionTouchEvery limits how often authentication writes LastSeenAt.
const sessionTouchEvery = 5 * time.Minute

// sessionDeadline returns when a session stops working if it is not used
// again: its absolute expiry, or the idle timeout counted from its last
// use, whichever comes first.
func (s *Service) sessionDeadline(sess Session) time.Time {
	deadline := sess.ExpiresAt
	if s.sessionIdle > 0 {
		last := sess.LastSeenAt
		if last.IsZero() {
			last = sess.CreatedAt
		}
		if idle := last.Add(s.sessionIdle); idle.Before(deadline) {
			deadline = idle
		}
	}
	return deadline
}

// AuthenticateSession resolves a session token to the operator [Actor],
// which carries the session's id and when it will stop working if it is
// not used again ([Actor.SessionID], [Actor.SessionExpiresAt]). It fails
// with [ErrInvalidCredentials] for unknown tokens and
// [ErrCredentialsExpired] for expired sessions. Public.
//
// A session ends at its absolute expiry ([Config.SessionTTL], 14 days) or
// after [Config.SessionIdleTimeout] (24 hours) without use, whichever comes
// first. Every successful authentication counts as use; it is recorded at
// most once every five minutes, so the idle timeout is exact to that
// granularity.
func (s *Service) AuthenticateSession(ctx context.Context, token string) (Actor, error) {
	if !strings.HasPrefix(token, "hmns_") || len(token) > 128 {
		return Actor{}, newError(ErrInvalidCredentials, "")
	}
	hash := hashSecret(token)
	now := s.now()
	check := func(ctx context.Context, r AccountReader, now time.Time) (Session, Operator, error) {
		sess, err := r.GetSessionByTokenHash(ctx, hash)
		switch {
		case errors.Is(err, ErrNotFound):
			return sess, Operator{}, newError(ErrInvalidCredentials, "")
		case err != nil:
			return sess, Operator{}, err
		case !secretsEqual(sess.TokenHash, hash):
			return sess, Operator{}, newError(ErrInvalidCredentials, "")
		case !now.Before(s.sessionDeadline(sess)):
			return sess, Operator{}, newError(ErrCredentialsExpired, "")
		}
		op, err := r.GetOperator(ctx)
		if errors.Is(err, ErrNotFound) || (err == nil && op.ID != sess.OperatorID) {
			return sess, op, newError(ErrInvalidCredentials, "")
		}
		return sess, op, err
	}
	var sess Session
	var op Operator
	err := s.store.View(ctx, func(r Reader) error {
		var err error
		sess, op, err = check(ctx, r, now)
		return err
	})
	if err != nil {
		return Actor{}, err
	}
	touch := sessionTouchEvery
	if s.sessionIdle > 0 {
		touch = min(touch, s.sessionIdle/10)
	}
	if now.Sub(sess.LastSeenAt) >= touch {
		err = s.update(ctx, func(u *unit) error {
			// Checked again by the time of the write: a session that ran
			// out while this waited for the store is not revived by being
			// marked as used.
			var err error
			if sess, op, err = check(ctx, u, u.now); err != nil {
				return err
			}
			sess.LastSeenAt = u.now
			return u.UpdateSession(ctx, sess)
		})
		if err != nil {
			return Actor{}, err
		}
	}
	return Actor{Kind: ActorOperator, ID: op.ID, Name: op.Email, SessionID: sess.ID, SessionExpiresAt: s.sessionDeadline(sess)}, nil
}

// Logout ends the session identified by token. Unknown tokens are ignored
// without a write: the session is looked up in a read first, so a stream
// of logout calls with made-up tokens costs no write transactions. Public.
func (s *Service) Logout(ctx context.Context, token string) error {
	if !strings.HasPrefix(token, "hmns_") || len(token) > 128 {
		return nil
	}
	hash := hashSecret(token)
	var id string
	err := s.store.View(ctx, func(r Reader) error {
		sess, err := r.GetSessionByTokenHash(ctx, hash)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		id = sess.ID
		return err
	})
	if err != nil || id == "" {
		return err
	}
	return s.update(ctx, func(u *unit) error { return u.DeleteSession(ctx, id) })
}

// GetOperator returns the operator account, without the password hash and
// the recovery code state.
// Operator only.
func (s *Service) GetOperator(ctx context.Context) (Operator, error) {
	var op Operator
	if _, err := authorize(ctx, permOperator); err != nil {
		return op, err
	}
	err := s.store.View(ctx, func(r Reader) error {
		var err error
		op, err = r.GetOperator(ctx)
		if errors.Is(err, ErrNotFound) {
			return newError(ErrSetupRequired, "")
		}
		return err
	})
	return op.redacted(), err
}

// maxPasswordLength bounds the work a password can cause.
const maxPasswordLength = 1024

func validatePassword(v *validation, field, password string) {
	switch {
	case len(password) < MinPasswordLength:
		v.add(field, "too_short", "password must be at least %d characters", MinPasswordLength)
	case len(password) > maxPasswordLength:
		v.add(field, "too_long", "password must be at most %d bytes", maxPasswordLength)
	}
}

// ChangePassword replaces the operator's password after verifying the
// current one, and ends every other session (the caller's own session, if
// the actor carries one, stays). Like a reset it moves the credential
// generation ([Service.DeviceBinding]) and cancels an outstanding recovery
// code. Operator only.
func (s *Service) ChangePassword(ctx context.Context, current, next string) error {
	actor, err := authorize(ctx, permOperator)
	if err != nil {
		return err
	}
	var v validation
	validatePassword(&v, "new_password", next)
	if err := v.errOrNil(); err != nil {
		return err
	}
	var op Operator
	err = s.store.View(ctx, func(r Reader) error {
		var err error
		op, err = r.GetOperator(ctx)
		if errors.Is(err, ErrNotFound) {
			return newError(ErrSetupRequired, "")
		}
		return err
	})
	if err != nil {
		return err
	}
	ok, err := s.verifyPassword(ctx, op.PasswordHash, current)
	if err != nil {
		return err
	}
	if !ok {
		return invalidField("current_password", "invalid", "current password is wrong")
	}
	hash, err := s.hashPassword(ctx, next)
	if err != nil {
		return err
	}
	return s.update(ctx, func(u *unit) error {
		cur, err := u.GetOperator(ctx)
		if err != nil {
			return err
		}
		if !sameCredentials(cur, op) {
			return conflict("password_changed", "the password was changed concurrently")
		}
		cur.PasswordHash = hash
		cur.UpdatedAt = u.now
		cur.Generation++
		cur.clearRecovery()
		if err := u.SaveOperator(ctx, cur); err != nil {
			return err
		}
		_, err = u.DeleteSessionsExcept(ctx, actor.SessionID)
		return err
	})
}

// ResetOperatorPassword replaces the operator's password without the
// current one and ends every session, the recovery path for an operator
// who is locked out ("hmnpay reset-password" on the server). It fails with
// [ErrSetupRequired] before setup and with a validation error on
// "new_password" for a password the policy refuses. System only: no
// transport may expose it, because it bypasses the knowledge of the current
// password — holding the system actor already means running code with
// access to the data directory.
//
// It also moves the account's credential generation, which makes every
// browser the transports remembered as a known device unknown again
// ([Service.DeviceBinding]), and cancels any outstanding recovery code. An
// owner who resets the password because they are locked out by the login
// throttles needs a fresh one: call [Service.MintRecoveryCode] next and
// show the code with the new password.
//
// Otherwise it replaces the password and nothing else. If somebody else had access to
// the instance, their API keys, webhook endpoints and — most importantly —
// the wallet they may have configured are all still in place: show the
// owner [Service.InstanceSummary] and offer [Service.RevokeAllAPIKeys].
func (s *Service) ResetOperatorPassword(ctx context.Context, next string) error {
	if _, err := authorize(ctx, permSystem); err != nil {
		return err
	}
	var v validation
	validatePassword(&v, "new_password", next)
	if err := v.errOrNil(); err != nil {
		return err
	}
	// Hash outside the transaction: the KDF is slow on purpose and must not
	// hold the store's write lock.
	hash, err := s.hashPassword(ctx, next)
	if err != nil {
		return err
	}
	return s.update(ctx, func(u *unit) error {
		op, err := u.GetOperator(ctx)
		if errors.Is(err, ErrNotFound) {
			return newError(ErrSetupRequired, "")
		}
		if err != nil {
			return err
		}
		op.PasswordHash = hash
		op.UpdatedAt = u.now
		op.Generation++
		op.clearRecovery()
		if err := u.SaveOperator(ctx, op); err != nil {
			return err
		}
		_, err = u.DeleteSessionsExcept(ctx, "")
		return err
	})
}

// ListSessions returns the operator's sessions that still work (neither
// past their absolute expiry nor idle for too long), newest first, with
// TokenHash cleared. The caller's own session is the
// one whose ID equals [Actor.SessionID]. Operator only.
func (s *Service) ListSessions(ctx context.Context) ([]Session, error) {
	if _, err := authorize(ctx, permOperator); err != nil {
		return nil, err
	}
	now := s.now()
	var out []Session
	err := s.store.View(ctx, func(r Reader) error {
		all, err := r.ListSessions(ctx)
		if err != nil {
			return err
		}
		out = make([]Session, 0, len(all))
		for _, sess := range all {
			if !now.Before(s.sessionDeadline(sess)) {
				continue
			}
			sess.TokenHash = ""
			out = append(out, sess)
		}
		return nil
	})
	return out, err
}

// RevokeSession ends one session by id — any of the operator's sessions,
// including the caller's own (which is then a logout). It fails with a
// not-found error for an unknown id. Operator only.
func (s *Service) RevokeSession(ctx context.Context, id string) error {
	if _, err := authorize(ctx, permOperator); err != nil {
		return err
	}
	return s.update(ctx, func(u *unit) error {
		all, err := u.ListSessions(ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(all, func(sess Session) bool { return sess.ID == id }) {
			return notFound("session", id)
		}
		return u.DeleteSession(ctx, id)
	})
}

// RevokeAllAPIKeys revokes every API key that is not revoked yet and
// returns how many it revoked. It is the blunt instrument for an instance
// whose keys can no longer be trusted — for example one that somebody else
// had access to: resetting the operator's password does not touch API
// keys. Operator only (which includes system actors).
func (s *Service) RevokeAllAPIKeys(ctx context.Context) (int, error) {
	if _, err := authorize(ctx, permOperator); err != nil {
		return 0, err
	}
	revoked := 0
	err := s.update(ctx, func(u *unit) error {
		revoked = 0
		keys, err := u.ListAPIKeys(ctx)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if !k.RevokedAt.IsZero() {
				continue
			}
			k.RevokedAt = u.now
			if err := u.UpdateAPIKey(ctx, k); err != nil {
				return err
			}
			revoked++
		}
		return nil
	})
	return revoked, err
}
