package core

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// Config wires a [Service] to its ports.
type Config struct {
	// Store is the persistence port. Required.
	Store Store
	// Addresses allocates deposit addresses. Required.
	Addresses AddressSource
	// Hasher hashes operator passwords. Defaults to [PBKDF2Hasher].
	Hasher PasswordHasher
	// Prober checks RPC endpoints. Optional; without it
	// [Service.ProbeRPC] returns [ErrUnavailable].
	Prober ChainProber
	// Clock tells the time. Defaults to the system clock.
	Clock Clock
	// Rand is the source of randomness for ids, API keys, session tokens
	// and webhook secrets. Defaults to crypto/rand. It must be
	// cryptographically secure outside tests.
	Rand io.Reader
	// ExpiryGrace is added to a payment's deadline before it settles as
	// expired. Defaults to [DefaultExpiryGrace]; negative disables it.
	ExpiryGrace time.Duration
	// WatchWindow is how long a deposit address stays on the watch list
	// after its payment last changed, for funded payments
	// ([Payment.Funded]). Defaults to [DefaultWatchWindow].
	WatchWindow time.Duration
	// UnfundedWatchWindow is the same for payments that are not funded —
	// nothing arrived, or only dust, or (on a payment created through a
	// public link) less than half the amount — counted from the payment's
	// last change of status. Funds that reach such an address later than
	// this after the payment closed are not noticed. Defaults to
	// [DefaultUnfundedWatchWindow].
	UnfundedWatchWindow time.Duration
	// MaxWatchedAddresses caps the public payment links' share of the
	// watch list: the payments created through links that are open or
	// still inside their watch window. At the cap,
	// [Service.CreatePaymentFromLink] — the one unauthenticated way to
	// create a payment — fails with [ErrWatchListFull]. Payments created
	// by the merchant are never refused and do not count against the cap.
	// Defaults to [DefaultMaxWatchedAddresses]; negative means no cap.
	MaxWatchedAddresses int
	// MaxRetainedUnchecked bounds how many addresses stay on the watch list
	// after their window only because blocks of it are still to be
	// scanned by a second log endpoint ([ChainHead.Checked]). Beyond it the
	// oldest are let go, those of payments through public links first
	// ([Payment.RetiredUnchecked]; counted in
	// [NodeStatus.ReleasedUnchecked]): an instance that runs on a single
	// log source for good would otherwise watch every address it ever had.
	// Defaults to [DefaultMaxRetainedUnchecked]; negative means no limit.
	MaxRetainedUnchecked int
	// MaxHeadTimeAhead is how far ahead of the wall clock a chain head's
	// block timestamp may be before [Service.AdvanceHead] refuses the head
	// with [ErrHeadTimeAhead]. Defaults to [DefaultMaxHeadTimeAhead];
	// negative disables the check (deadlines stay safe without it, see
	// [Evaluate]).
	MaxHeadTimeAhead time.Duration
	// LinkReservation is the lifetime of a checkout opened through a
	// payment link that has MaxUses: such a checkout holds one of the
	// link's uses until it settles, so it expires after this long instead
	// of the instance's payment TTL (see [Service.CreatePaymentFromLink]).
	// It is capped at the payment TTL and never shorter than
	// [MinPaymentTTL]. Defaults to [DefaultLinkReservation]; negative
	// means the payment TTL (a use is then held that long).
	LinkReservation time.Duration
	// SessionTTL is the absolute lifetime of a dashboard session. Defaults
	// to [DefaultSessionTTL].
	SessionTTL time.Duration
	// SessionIdleTimeout ends a session that has not been used for this
	// long. Defaults to [DefaultSessionIdleTimeout]; negative disables the
	// idle timeout.
	SessionIdleTimeout time.Duration
	// MaxConcurrentKDF bounds how many password hashes (logins, password
	// changes, setup) are computed at the same time, so that
	// unauthenticated login attempts cannot occupy every CPU. A caller that
	// finds all slots taken waits up to two seconds and then fails with
	// [ErrBusy]. Defaults to [DefaultMaxConcurrentKDF].
	MaxConcurrentKDF int
	// SetupToken, when non-empty, must be presented by anonymous callers of
	// the first-run setup methods ([Service.CompleteSetup],
	// [Service.ProbeRPC], [Service.PreviewAddresses]) while setup is not
	// completed — in SetupInput.SetupToken and friends — so that whoever
	// merely reaches the instance first cannot claim it. Server wiring
	// should always set it (generate it, show it to the person installing).
	// Empty means no token is required, which is meant for tests and for
	// programs embedding the engine, which call setup as a system actor
	// anyway. System actors never need the token.
	SetupToken string
	// ValidateWebhookURL, when set, is the wiring's egress policy for
	// webhook endpoint URLs (point it at the dispatcher's static URL
	// check). The core calls it — after its own syntax and no-credentials
	// checks — whenever an endpoint is created, or updated with a changed
	// URL or left enabled, so every transport rejects the same URLs with
	// the same validation error on "url" (code "not_allowed"). The
	// returned error's text is shown to the caller; it must be safe to
	// show. It must be pure and fast (no DNS, no I/O): it runs inside a
	// store transaction. It is a create-time courtesy, not the security
	// boundary — the dispatcher still enforces its policy on every
	// connection.
	ValidateWebhookURL func(url string) error
}

// Service is the hmnpay engine: every use case of the system, independent
// of any transport. REST handlers, the dashboard, the hosted checkout, a
// Telegram bot, a CLI and a program embedding hmnpay all call the same
// Service; the chain watcher and the webhook dispatcher are its only other
// clients. See the package documentation.
//
// A Service is safe for concurrent use. Its methods fall into two groups:
// public ones (setup, login, checkout, authentication) and privileged ones,
// which require an [Actor] in the context (see [WithActor]).
type Service struct {
	store       Store
	addresses   AddressSource
	hasher      PasswordHasher
	prober      ChainProber
	clock       Clock
	grace       time.Duration
	watchWindow time.Duration
	sessionTTL  time.Duration

	unfundedWatchWindow time.Duration
	maxWatched          int
	maxRetained         int
	maxHeadAhead        time.Duration
	linkReservation     time.Duration
	sessionIdle         time.Duration
	setupToken          string
	kdfSlots            chan struct{}

	validateWebhookURL func(url string) error

	randMu sync.Mutex
	rand   io.Reader

	hub hub

	statusMu    sync.Mutex
	chainStatus *ChainStatus
}

// New returns a Service wired to the given ports.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("core: Config.Store is required")
	}
	if cfg.Addresses == nil {
		return nil, errors.New("core: Config.Addresses is required")
	}
	s := &Service{
		store:       cfg.Store,
		addresses:   cfg.Addresses,
		hasher:      cfg.Hasher,
		prober:      cfg.Prober,
		clock:       cfg.Clock,
		rand:        cfg.Rand,
		grace:       cfg.ExpiryGrace,
		watchWindow: cfg.WatchWindow,
		sessionTTL:  cfg.SessionTTL,

		unfundedWatchWindow: cfg.UnfundedWatchWindow,
		maxWatched:          cfg.MaxWatchedAddresses,
		maxRetained:         cfg.MaxRetainedUnchecked,
		maxHeadAhead:        cfg.MaxHeadTimeAhead,
		linkReservation:     cfg.LinkReservation,
		sessionIdle:         cfg.SessionIdleTimeout,
		setupToken:          cfg.SetupToken,

		validateWebhookURL: cfg.ValidateWebhookURL,
	}
	if s.hasher == nil {
		s.hasher = PBKDF2Hasher{}
	}
	if s.clock == nil {
		s.clock = systemClock{}
	}
	if s.rand == nil {
		s.rand = rand.Reader
	}
	switch {
	case s.grace == 0:
		s.grace = DefaultExpiryGrace
	case s.grace < 0:
		s.grace = 0
	}
	if s.watchWindow <= 0 {
		s.watchWindow = DefaultWatchWindow
	}
	if s.sessionTTL <= 0 {
		s.sessionTTL = DefaultSessionTTL
	}
	if s.unfundedWatchWindow <= 0 {
		s.unfundedWatchWindow = DefaultUnfundedWatchWindow
	}
	if s.maxWatched == 0 {
		s.maxWatched = DefaultMaxWatchedAddresses
	}
	if s.maxRetained == 0 {
		s.maxRetained = DefaultMaxRetainedUnchecked
	}
	if s.maxHeadAhead == 0 {
		s.maxHeadAhead = DefaultMaxHeadTimeAhead
	}
	switch {
	case s.linkReservation == 0:
		s.linkReservation = DefaultLinkReservation
	case s.linkReservation < 0:
		s.linkReservation = 0
	}
	switch {
	case s.sessionIdle == 0:
		s.sessionIdle = DefaultSessionIdleTimeout
	case s.sessionIdle < 0:
		s.sessionIdle = 0
	}
	slots := cfg.MaxConcurrentKDF
	if slots <= 0 {
		slots = DefaultMaxConcurrentKDF
	}
	s.kdfSlots = make(chan struct{}, slots)
	return s, nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// now returns the current time in UTC, truncated to milliseconds.
func (s *Service) now() time.Time {
	return s.clock.Now().UTC().Truncate(time.Millisecond)
}

// kdfMaxWait is how long a password operation waits for a free KDF slot
// before giving up with ErrBusy.
const kdfMaxWait = 2 * time.Second

// withKDF runs fn — one password hash or verification — in one of the
// service's few KDF slots. The KDF is slow on purpose, and logins are
// unauthenticated: without a bound, a burst of login attempts would run as
// many of them in parallel as there are requests. Callers that find every
// slot taken wait briefly, then fail with ErrBusy.
func (s *Service) withKDF(ctx context.Context, fn func() error) error {
	select {
	case s.kdfSlots <- struct{}{}:
	default:
		wait := time.NewTimer(kdfMaxWait)
		defer wait.Stop()
		select {
		case s.kdfSlots <- struct{}{}:
		case <-wait.C:
			return newError(ErrBusy, "")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() { <-s.kdfSlots }()
	return fn()
}

// verifyPassword runs the hasher's Verify in a KDF slot.
func (s *Service) verifyPassword(ctx context.Context, hash, password string) (ok bool, err error) {
	err = s.withKDF(ctx, func() error {
		var verr error
		if ok, verr = s.hasher.Verify(hash, password); verr != nil {
			return internal("verify password", verr)
		}
		return nil
	})
	return ok, err
}

// hashPassword runs the hasher's Hash in a KDF slot.
func (s *Service) hashPassword(ctx context.Context, password string) (hash string, err error) {
	err = s.withKDF(ctx, func() error {
		var herr error
		if hash, herr = s.hasher.Hash(password); herr != nil {
			return internal("hash password", herr)
		}
		return nil
	})
	return hash, err
}

// ID prefixes. IDs are "<prefix>_" plus 26 characters of lowercase
// Crockford base32 encoding 128 random bits: URL-safe and unguessable.
const (
	prefixPayment  = "pay"
	prefixLink     = "plink"
	prefixEndpoint = "we"
	prefixDelivery = "whd"
	prefixAPIKey   = "key"
	prefixEvent    = "evt"
	prefixSession  = "sess"
	prefixOperator = "op"
	prefixClaim    = "whc"
)

var idEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// randomString returns n random bytes in the id encoding.
func (s *Service) randomString(n int) (string, error) {
	buf := make([]byte, n)
	s.randMu.Lock()
	_, err := io.ReadFull(s.rand, buf)
	s.randMu.Unlock()
	if err != nil {
		return "", internal("read randomness", err)
	}
	return idEncoding.EncodeToString(buf), nil
}

// newID returns a fresh id with the given prefix.
func (s *Service) newID(prefix string) (string, error) {
	r, err := s.randomString(16)
	if err != nil {
		return "", err
	}
	return prefix + "_" + r, nil
}

// unit is one read-write transaction of the service, with the helpers that
// keep "state change + event" in a single commit.
type unit struct {
	Tx
	s *Service
	// now is the clock's reading once the transaction has begun — after the
	// store's write lock was acquired, which can take a while: everything
	// the transaction decides by time (a credential's expiry above all,
	// stillAuthenticated) is decided by the moment it writes, not by the
	// moment it queued.
	now      time.Time
	emitted  int
	payments []string // payments that got an event in this transaction
	settings *Settings
	head     *ChainHead
}

// update runs fn in a store transaction and wakes event subscribers after
// a commit that appended events.
func (s *Service) update(ctx context.Context, fn func(u *unit) error) error {
	u := &unit{s: s}
	err := s.store.Update(ctx, func(tx Tx) error {
		u.Tx, u.now = tx, s.now()
		if err := u.stillAuthenticated(ctx); err != nil {
			return err
		}
		return fn(u)
	})
	if err == nil && u.emitted > 0 {
		s.hub.wake(u.payments)
	}
	return err
}

// stillAuthenticated checks, inside the transaction that is about to write,
// that the credential the caller was authenticated with is still good: the
// operator's session still exists and has not run out, the API key has not
// been revoked or expired.
//
// A transport authenticates a request once, before it calls the service, and
// a use case may then spend time before it writes (hashing a password,
// waiting for the store's write lock). Without this check, whatever was
// revoked in between — "end every session" by a password reset, one session
// by [Service.RevokeSession], every key by [Service.RevokeAllAPIKeys] — would
// not stop a request that was already on its way: a session purged by a
// reset could still create an API key, change the password or move the
// wallet. With it, a revocation is final for every write that commits after
// it. (A read that was authenticated before the revocation still completes;
// it changes nothing.)
//
// System actors, and actors built by trusted in-process code without a
// session or key behind them, are not checked: there is nothing to revoke.
func (u *unit) stillAuthenticated(ctx context.Context) error {
	a, ok := ActorFrom(ctx)
	if !ok {
		return nil
	}
	switch {
	case a.Kind == ActorOperator && a.SessionID != "":
		sessions, err := u.ListSessions(ctx)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(sessions, func(sess Session) bool { return sess.ID == a.SessionID })
		if i < 0 || sessions[i].OperatorID != a.ID {
			return newError(ErrInvalidCredentials, "")
		}
		if !u.now.Before(u.s.sessionDeadline(sessions[i])) {
			return newError(ErrCredentialsExpired, "")
		}
	case a.Kind == ActorAPIKey && a.ID != "":
		key, err := u.GetAPIKey(ctx, a.ID)
		switch {
		case errors.Is(err, ErrNotFound):
			// Keys are revoked, never deleted: an actor naming a key the
			// store does not have was not made by AuthenticateAPIKey.
			return nil
		case err != nil:
			return err
		case !key.RevokedAt.IsZero():
			return newError(ErrCredentialsRevoked, "")
		case !key.ExpiresAt.IsZero() && !u.now.Before(key.ExpiresAt):
			return newError(ErrCredentialsExpired, "")
		}
	}
	return nil
}

// loadSettings returns the stored settings, or the defaults of a fresh
// instance.
func loadSettings(ctx context.Context, r SettingsReader) (Settings, error) {
	st, err := r.GetSettings(ctx)
	if errors.Is(err, ErrNotFound) {
		return DefaultSettings(), nil
	}
	return st, err
}

// loadHead returns the stored chain head, or the zero head.
func loadHead(ctx context.Context, r SettingsReader) (ChainHead, error) {
	h, err := r.GetChainHead(ctx)
	if errors.Is(err, ErrNotFound) {
		return ChainHead{}, nil
	}
	return h, err
}

// requireSetup returns the settings of a set-up instance.
func requireSetup(ctx context.Context, r SettingsReader) (Settings, error) {
	st, err := loadSettings(ctx, r)
	if err != nil {
		return st, err
	}
	if !st.SetupCompleted {
		return st, newError(ErrSetupRequired, "")
	}
	return st, nil
}

func (u *unit) loadSettings(ctx context.Context) (Settings, error) {
	if u.settings == nil {
		st, err := loadSettings(ctx, u.Tx)
		if err != nil {
			return st, err
		}
		u.settings = &st
	}
	return u.settings.clone(), nil
}

func (u *unit) saveSettings(ctx context.Context, st Settings) error {
	st.UpdatedAt = u.now
	prev, err := u.loadSettings(ctx)
	if err != nil {
		return err
	}
	st.Network.Revision = prev.Network.Revision
	if chainConfigChanged(prev, st) {
		st.Network.Revision++
	}
	if err := u.SaveSettings(ctx, st); err != nil {
		return err
	}
	st = st.clone()
	u.settings = &st
	return nil
}

func (u *unit) loadHead(ctx context.Context) (ChainHead, error) {
	if u.head == nil {
		h, err := loadHead(ctx, u.Tx)
		if err != nil {
			return h, err
		}
		u.head = &h
	}
	return *u.head, nil
}

// emit appends an event with a JSON payload.
func (u *unit) emit(ctx context.Context, typ EventType, paymentID string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return internal("encode event", err)
	}
	id, err := u.s.newID(prefixEvent)
	if err != nil {
		return err
	}
	e := Event{ID: id, Type: typ, PaymentID: paymentID, CreatedAt: u.now, Data: raw}
	if err := u.AppendEvent(ctx, &e); err != nil {
		return err
	}
	u.emitted++
	if paymentID != "" && !slices.Contains(u.payments, paymentID) {
		u.payments = append(u.payments, paymentID)
	}
	return nil
}

// emitPayment appends a payment event carrying the payment's current view.
func (u *unit) emitPayment(ctx context.Context, typ EventType, p Payment) error {
	st, err := u.loadSettings(ctx)
	if err != nil {
		return err
	}
	head, err := u.loadHead(ctx)
	if err != nil {
		return err
	}
	return u.emit(ctx, typ, p.ID, NewPaymentView(p, headFor(head, st, p), st.PublicURL))
}

// headFor returns the head usable for p's confirmations and finality: the
// zero head when it is not a head of p's asset (another chain, or a token the
// instance does not watch now: see [Service.reevaluate]).
func headFor(h ChainHead, st Settings, p Payment) ChainHead {
	if h.ChainID != p.Asset.ChainID || !st.watches(p.Asset) {
		return ChainHead{}
	}
	return h
}

// Subscribe returns a [Subscription] that is signalled after events are
// appended to the log (and after a webhook delivery becomes due out of
// schedule: a resend, a test event, or a claim released for immediate
// redelivery).
// Callers must Close it. Subscribe is public: the signal carries no data.
func (s *Service) Subscribe() *Subscription { return s.hub.subscribe("") }

// SubscribePayment returns a [Subscription] that is signalled only after
// an event of the given payment is appended ("payment.*": status changes,
// late funds, resolve, reopen). It is what a per-payment stream (the
// checkout's server-sent events) should use: with [Service.Subscribe]
// every open stream would wake on every event in the system.
//
// The signal is the same coalescing, data-free nudge. It does not fire
// when only the confirmation count changes — that is not an event; poll
// for it while the payment is detected or confirming. Callers must Close
// it. Public: the payment id is the capability, exactly as for
// [Service.GetCheckout], and the id is not checked to exist.
func (s *Service) SubscribePayment(paymentID string) *Subscription {
	if paymentID == "" {
		paymentID = "\x00" // matches no payment; never the global topic
	}
	return s.hub.subscribe(paymentID)
}

// Events reads the event log in ascending Seq order. It is the outbound
// half of the system: webhooks, server-sent events, notifiers and polling
// integrations all consume this one stream.
//
// Consumers keep a cursor — the Seq of the last event they processed — and
// pass it as filter.After. Delivery is at-least-once: process an event,
// then advance the cursor. A Limit of 0 is replaced by 100; the maximum is
// 1000. Requires [ScopePaymentsRead]; API keys see "payment.*" events
// only, operators and system actors see everything.
func (s *Service) Events(ctx context.Context, filter EventFilter) ([]Event, error) {
	actor, err := authorize(ctx, permScope(ScopePaymentsRead))
	if err != nil {
		return nil, err
	}
	if actor.Kind == ActorAPIKey {
		// Instance events (settings, endpoint health) are not a merchant
		// integration's business.
		types := slices.DeleteFunc(slices.Clone(filter.Types), func(t EventType) bool { return !t.IsPayment() })
		if len(filter.Types) > 0 && len(types) == 0 {
			return nil, nil
		}
		if len(types) == 0 {
			types = slices.Clone(WebhookEventTypes)
		}
		filter.Types = types
	}
	filter.Limit = clampLimit(filter.Limit, 100, 1000)
	var out []Event
	err = s.store.View(ctx, func(r Reader) error {
		var err error
		out, err = r.ListEvents(ctx, filter)
		return err
	})
	return out, err
}

// Cursor returns the durable position saved by a named consumer with
// [Service.SaveCursor], or 0. Consumer names are free-form ("telegram");
// names starting with "core." are reserved. System only.
func (s *Service) Cursor(ctx context.Context, consumer string) (int64, error) {
	if _, err := authorize(ctx, permSystem); err != nil {
		return 0, err
	}
	var seq int64
	err := s.store.View(ctx, func(r Reader) error {
		var err error
		seq, err = r.GetCursor(ctx, consumer)
		return err
	})
	return seq, err
}

// SaveCursor durably records that a named consumer has processed every
// event up to and including seq. System only.
func (s *Service) SaveCursor(ctx context.Context, consumer string, seq int64) error {
	if _, err := authorize(ctx, permSystem); err != nil {
		return err
	}
	if consumer == "" || len(consumer) > 64 || strings.HasPrefix(consumer, "core.") {
		return invalidField("consumer", "invalid", "consumer name must be 1 to 64 characters and must not start with \"core.\"")
	}
	if seq < 0 {
		return invalidField("seq", "invalid", "seq must not be negative")
	}
	return s.update(ctx, func(u *unit) error { return u.SetCursor(ctx, consumer, seq) })
}

// PruneResult reports what [Service.Prune] removed.
type PruneResult struct {
	Deliveries int
	Sessions   int
}

// Prune deletes webhook deliveries older than [WebhookRetention] and
// sessions that no longer work (expired or idle for too long). Wiring should call it periodically (hourly is plenty).
// The event log is never pruned: it is the audit trail of every payment.
// System only.
func (s *Service) Prune(ctx context.Context) (PruneResult, error) {
	var res PruneResult
	if _, err := authorize(ctx, permSystem); err != nil {
		return res, err
	}
	err := s.update(ctx, func(u *unit) error {
		var err error
		if res.Deliveries, err = u.DeleteDeliveriesBefore(ctx, u.now.Add(-WebhookRetention)); err != nil {
			return err
		}
		if res.Sessions, err = u.DeleteSessionsExpiredBefore(ctx, u.now); err != nil {
			return err
		}
		// Sessions that idled out before their absolute expiry.
		rest, err := u.ListSessions(ctx)
		if err != nil {
			return err
		}
		for _, sess := range rest {
			if !u.now.Before(s.sessionDeadline(sess)) {
				if err := u.DeleteSession(ctx, sess.ID); err != nil {
					return err
				}
				res.Sessions++
			}
		}
		return nil
	})
	return res, err
}

func clampLimit(limit, def, maxLimit int) int {
	switch {
	case limit <= 0:
		return def
	case limit > maxLimit:
		return maxLimit
	}
	return limit
}
