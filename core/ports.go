package core

import (
	"context"
	"time"
)

// This file defines every port: the interfaces the core needs the outside
// world to implement. The core itself performs no I/O.

// Clock tells the time. The service converts every reading to UTC and
// truncates it to milliseconds before using it.
type Clock interface {
	Now() time.Time
}

// PasswordHasher hashes operator passwords with a slow, salted KDF.
//
// The core ships [PBKDF2Hasher] (standard library, PBKDF2-HMAC-SHA256,
// 600 000 iterations), which [New] uses by default. Wiring that accepts a
// dependency on golang.org/x/crypto may substitute argon2id; hashes must be
// self-describing so that Verify keeps working for hashes made earlier.
type PasswordHasher interface {
	// Hash returns an encoded hash of password, including salt and
	// parameters.
	Hash(password string) (string, error)
	// Verify reports whether password matches hash. A mismatch is
	// (false, nil); an error means the hash itself is unusable.
	Verify(hash, password string) (bool, error)
}

// AddressSource turns an address index into a deposit address. It is the
// seam that keeps deposit-address allocation replaceable: the v1
// implementation derives child keys of the operator's xpub (BIP-32,
// non-hardened <path>/<index>); a CREATE2 forwarder strategy can implement
// the same interface later without touching the payment rules.
//
// Implemented by internal/wallet.
type AddressSource interface {
	// DeriveAddress returns the deposit address for index under the given
	// wallet configuration, as a 0x-prefixed 20-byte hex address (any
	// casing; the core lowercases it).
	//
	// It must be deterministic (same wallet and index → same address),
	// injective over indexes, and pure: no I/O, no blocking, no calls back
	// into the service — the core calls it while holding a store
	// transaction. It must reject private keys (xprv), malformed keys and
	// unsupported paths with an error; the core reports such an error as a
	// validation failure of the xpub during setup and wallet updates.
	DeriveAddress(ctx context.Context, wallet WalletSettings, index uint32) (string, error)
}

// ChainProber checks a JSON-RPC endpoint for the setup wizard and the
// network settings page. Implemented by internal/chain. Optional: without
// one, [Service.ProbeRPC] returns [ErrUnavailable].
type ChainProber interface {
	// Probe connects to rpcURL and reports plain facts: whether it
	// answered, its chain id, latest block, round-trip latency, and what
	// the token contract of asset reports. It must not judge them — the
	// core derives the verdict. Unreachable endpoints are a result
	// (Reachable false, Error set), not an error; return an error only for
	// failures of the prober itself or a cancelled context. It must apply
	// its own timeout (a few seconds).
	Probe(ctx context.Context, rpcURL string, asset Asset) (ProbeFacts, error)
}

// ProbeFacts is what a [ChainProber] observed.
type ProbeFacts struct {
	// Reachable: the endpoint answered a JSON-RPC request.
	Reachable bool
	// ChainID is the endpoint's eth_chainId.
	ChainID uint64
	// BlockNumber is the endpoint's latest block.
	BlockNumber uint64
	// LatencyMs is the round-trip time of one request.
	LatencyMs int
	// TokenFound: a contract exists at asset.Contract and answered
	// decimals().
	TokenFound bool
	// TokenDecimals and TokenSymbol are what the contract reports.
	TokenDecimals int
	TokenSymbol   string
	// Error is a short human-readable reason when something failed.
	Error string
}

// Store is the persistence port: one transactional unit of work over all
// of the core's state. Implemented by internal/store/sqlite; a reference
// in-memory implementation and a conformance suite live in core/coretest.
//
// # Transactions
//
// Update runs fn inside a read-write transaction and commits if and only if
// fn returns nil; on a non-nil error (which Update returns unchanged) or a
// panic (which Update re-raises after rolling back) nothing fn wrote
// becomes visible. fn is called exactly once — implementations must not
// retry it.
//
// Update transactions must be serializable with respect to each other. The
// simplest correct implementation runs one writer at a time (SQLite:
// BEGIN IMMEDIATE on a single write connection). The core relies on this
// for every invariant: "read payment → decide → write payment + append
// event", idempotency keys, and address-index allocation (which is a plain
// read-modify-write of [Settings] inside Update).
//
// View runs fn against a consistent snapshot: it never observes a
// partially committed Update. fn must not retain the Reader or Tx after
// returning, and must not call Update or View on the same store from
// inside fn.
//
// # Values
//
// Everything returned is a private copy the caller may modify; everything
// passed in is copied before Update returns. Timestamps are stored with at
// least millisecond precision and returned in UTC with the zero time
// preserved; nil and empty slices/maps are interchangeable on input, and
// [Payment.Metadata] is returned non-nil. All other fields round-trip
// exactly.
//
// # Errors
//
// Lookups of a missing object return an error matching [ErrNotFound];
// Insert of a duplicate (primary key or any unique field named on the
// method) returns an error matching [ErrConflict]; Update of a missing
// object returns [ErrNotFound]. Any other error is treated as an
// infrastructure failure and passed through to the caller.
type Store interface {
	Update(ctx context.Context, fn func(tx Tx) error) error
	View(ctx context.Context, fn func(r Reader) error) error
}

// Reader is the read side of the store.
type Reader interface {
	SettingsReader
	PaymentReader
	TransferReader
	LinkReader
	EventReader
	WebhookReader
	APIKeyReader
	AccountReader
}

// Tx is a read-write transaction: every read sees the transaction's own
// writes.
type Tx interface {
	Reader
	SettingsWriter
	PaymentWriter
	TransferWriter
	LinkWriter
	EventWriter
	WebhookWriter
	APIKeyWriter
	AccountWriter
}

// SettingsReader reads the singleton rows.
type SettingsReader interface {
	// GetSettings returns the instance settings, or ErrNotFound if none
	// were ever saved.
	GetSettings(ctx context.Context) (Settings, error)
	// GetChainHead returns the last saved chain head, or ErrNotFound.
	GetChainHead(ctx context.Context) (ChainHead, error)
}

// SettingsWriter writes the singleton rows.
type SettingsWriter interface {
	// SaveSettings creates or replaces the instance settings.
	SaveSettings(ctx context.Context, s Settings) error
	// SaveChainHead creates or replaces the chain head.
	SaveChainHead(ctx context.Context, h ChainHead) error
}

// PaymentReader reads payments.
type PaymentReader interface {
	// GetPayment returns the payment with the given id.
	GetPayment(ctx context.Context, id string) (Payment, error)
	// GetPaymentByAddress returns the payment owning a deposit address
	// (canonical lowercase; exact match).
	GetPaymentByAddress(ctx context.Context, address string) (Payment, error)
	// GetPaymentByIdempotencyKey returns the payment created with a
	// non-empty idempotency key.
	GetPaymentByIdempotencyKey(ctx context.Context, key string) (Payment, error)
	// ListPayments returns the payments matching filter, ordered by sort,
	// restricted to page.
	ListPayments(ctx context.Context, filter PaymentFilter, sort PaymentSort, page Page) ([]Payment, error)
	// CountPayments returns the number of payments matching filter.
	CountPayments(ctx context.Context, filter PaymentFilter) (int, error)
}

// PaymentWriter writes payments.
type PaymentWriter interface {
	// InsertPayment stores a new payment. Unique: ID, Address, and
	// IdempotencyKey when non-empty.
	InsertPayment(ctx context.Context, p Payment) error
	// UpdatePayment replaces the payment with the same ID.
	UpdatePayment(ctx context.Context, p Payment) error
}

// PaymentFilter selects payments. All set fields must match (logical AND);
// the zero value matches every payment.
type PaymentFilter struct {
	// Statuses, when non-empty, matches payments in any listed status.
	Statuses []PaymentStatus
	// NeedsAttention, when non-nil, matches payments whose
	// [Payment.NeedsAttention] equals the value.
	NeedsAttention *bool
	// Search, when non-empty, is a case-insensitive substring match
	// against the payment's ID, OrderID, Description, CustomerEmail and
	// Address, and against the TxHash and From address of any of its
	// transfers (orphaned ones included). Leading and trailing spaces are
	// already trimmed by the service.
	Search string
	// OrderID, when non-empty, matches the order id exactly.
	OrderID string
	// LinkID, when non-empty, matches payments created from that link.
	LinkID string
	// FromLink, when non-nil, matches payments created through a payment
	// link (LinkID != "") when true, and payments the merchant created
	// (LinkID == "") when false.
	FromLink *bool
	// ChainID, when non-zero, matches payments whose Asset.ChainID equals
	// it.
	ChainID uint64
	// CreatedFrom / CreatedTo bound CreatedAt: CreatedFrom <= CreatedAt <
	// CreatedTo. A zero bound is open.
	CreatedFrom time.Time
	CreatedTo   time.Time
	// PaidFrom / PaidTo bound PaidAt the same way; when either is set,
	// payments with a zero PaidAt never match.
	PaidFrom time.Time
	PaidTo   time.Time
	// ExpiresBefore, when non-zero, matches ExpiresAt <= ExpiresBefore.
	ExpiresBefore time.Time
	// WatchedAfter, when non-zero, matches WatchUntil > WatchedAfter.
	WatchedAfter time.Time
	// WatchEndsBy, when non-zero, matches WatchUntil <= WatchEndsBy.
	WatchEndsBy time.Time
	// Retired, when non-nil, matches payments whose address has left the
	// chain watch list ([Payment.RetiredAt] non-zero) when true, and those
	// whose address has not when false.
	Retired *bool
	// Contract, when non-empty, matches Asset.Contract (canonical
	// lowercase form).
	Contract string
	// RetiredUnchecked, when true, matches only payments whose address was
	// let go while blocks of its window were still to be scanned by a
	// second log endpoint ([Payment.RetiredUnchecked]).
	RetiredUnchecked bool
}

// PaymentSortKey names a sort order for payment lists.
type PaymentSortKey string

// Payment sort keys.
const (
	// SortByCreated orders by CreatedAt. The default.
	SortByCreated PaymentSortKey = "created"
	// SortByAmount orders by Amount, numerically (see [Amount.SortKey]).
	SortByAmount PaymentSortKey = "amount"
)

// PaymentSort is the ordering of a payment list. Ties are broken by ID in
// the same direction, so the order is total and pagination is stable.
type PaymentSort struct {
	// By is the sort key; empty means [SortByCreated].
	By PaymentSortKey
	// Desc reverses the order (newest / largest first).
	Desc bool
}

// Page restricts a list to a window.
type Page struct {
	// Limit is the maximum number of items; 0 means no limit.
	Limit int
	// Offset is the number of items to skip.
	Offset int
}

// TransferReader reads observed transfers.
type TransferReader interface {
	// GetTransfer returns the transfer with the given key.
	GetTransfer(ctx context.Context, key TransferKey) (Transfer, error)
	// ListTransfers returns all transfers of a payment ordered by
	// BlockNumber, LogIndex, then BlockHash.
	ListTransfers(ctx context.Context, paymentID string) ([]Transfer, error)
	// ListTransfersByTxHash returns every transfer recorded for a
	// transaction hash, in any block, in the same order.
	ListTransfersByTxHash(ctx context.Context, txHash string) ([]Transfer, error)
	// ListPendingTransfers returns every transfer in status pending, of
	// all payments, in the same order.
	ListPendingTransfers(ctx context.Context) ([]Transfer, error)
}

// TransferWriter writes observed transfers.
type TransferWriter interface {
	// InsertTransfer stores a new transfer. Unique: the key
	// (TxHash, LogIndex, BlockHash).
	InsertTransfer(ctx context.Context, t Transfer) error
	// UpdateTransfer replaces the transfer with the same key.
	UpdateTransfer(ctx context.Context, t Transfer) error
}

// LinkReader reads payment links.
type LinkReader interface {
	// GetLink returns the link with the given id.
	GetLink(ctx context.Context, id string) (PaymentLink, error)
	// GetLinkBySlug returns the link with the given slug (exact match).
	GetLinkBySlug(ctx context.Context, slug string) (PaymentLink, error)
	// ListLinks returns all links, newest first (CreatedAt descending,
	// then ID descending).
	ListLinks(ctx context.Context) ([]PaymentLink, error)
}

// LinkWriter writes payment links.
type LinkWriter interface {
	// InsertLink stores a new link. Unique: ID, Slug.
	InsertLink(ctx context.Context, l PaymentLink) error
	// UpdateLink replaces the link with the same ID.
	UpdateLink(ctx context.Context, l PaymentLink) error
	// DeleteLink removes a link. Payments created from it are not touched
	// and keep their LinkID. Deleting a missing link returns ErrNotFound.
	// The slug becomes available again.
	DeleteLink(ctx context.Context, id string) error
}

// EventReader reads the event log and consumer cursors.
type EventReader interface {
	// ListEvents returns the events matching filter in ascending Seq.
	ListEvents(ctx context.Context, filter EventFilter) ([]Event, error)
	// GetCursor returns the saved position of a named consumer, or 0 if it
	// never saved one.
	GetCursor(ctx context.Context, consumer string) (int64, error)
}

// EventWriter appends to the event log and saves consumer cursors.
type EventWriter interface {
	// AppendEvent appends e to the log, assigns e.Seq and returns it.
	//
	// Seq must be positive and strictly greater than the Seq of every
	// event committed before this transaction commits, so that a consumer
	// that has read up to Seq n and later asks for "After: n" can never
	// miss an event. With serializable (single-writer) transactions a
	// monotonically increasing counter that is assigned inside the
	// transaction satisfies this; values consumed by rolled-back
	// transactions may be skipped or reused. Unique: e.ID.
	AppendEvent(ctx context.Context, e *Event) error
	// SetCursor saves the position of a named consumer.
	SetCursor(ctx context.Context, consumer string, seq int64) error
}

// WebhookReader reads webhook endpoints and deliveries.
type WebhookReader interface {
	// GetWebhookEndpoint returns the endpoint with the given id.
	GetWebhookEndpoint(ctx context.Context, id string) (WebhookEndpoint, error)
	// ListWebhookEndpoints returns all endpoints, oldest first (CreatedAt
	// ascending, then ID).
	ListWebhookEndpoints(ctx context.Context) ([]WebhookEndpoint, error)
	// GetDelivery returns the delivery with the given id.
	GetDelivery(ctx context.Context, id string) (WebhookDelivery, error)
	// ListDeliveries returns the deliveries matching filter in the order
	// documented on [DeliveryFilter], restricted to page.
	ListDeliveries(ctx context.Context, filter DeliveryFilter, page Page) ([]WebhookDelivery, error)
	// CountDeliveries returns the number of deliveries matching filter.
	CountDeliveries(ctx context.Context, filter DeliveryFilter) (int, error)
}

// WebhookWriter writes webhook endpoints and deliveries.
type WebhookWriter interface {
	// InsertWebhookEndpoint stores a new endpoint. Unique: ID.
	InsertWebhookEndpoint(ctx context.Context, e WebhookEndpoint) error
	// UpdateWebhookEndpoint replaces the endpoint with the same ID.
	UpdateWebhookEndpoint(ctx context.Context, e WebhookEndpoint) error
	// DeleteWebhookEndpoint removes an endpoint and all of its deliveries.
	// Deleting a missing endpoint returns ErrNotFound.
	DeleteWebhookEndpoint(ctx context.Context, id string) error
	// InsertDelivery stores a new delivery. Unique: ID.
	InsertDelivery(ctx context.Context, d WebhookDelivery) error
	// UpdateDelivery replaces the delivery with the same ID.
	UpdateDelivery(ctx context.Context, d WebhookDelivery) error
	// DeleteDeliveriesBefore removes deliveries with CreatedAt before t
	// and returns how many were removed.
	DeleteDeliveriesBefore(ctx context.Context, t time.Time) (int, error)
}

// APIKeyReader reads API keys.
type APIKeyReader interface {
	// GetAPIKey returns the key with the given id.
	GetAPIKey(ctx context.Context, id string) (APIKey, error)
	// GetAPIKeyByHash returns the key with the given SecretHash.
	GetAPIKeyByHash(ctx context.Context, secretHash string) (APIKey, error)
	// ListAPIKeys returns all keys, newest first (CreatedAt descending,
	// then ID descending), revoked ones included.
	ListAPIKeys(ctx context.Context) ([]APIKey, error)
}

// APIKeyWriter writes API keys.
type APIKeyWriter interface {
	// InsertAPIKey stores a new key. Unique: ID, SecretHash.
	InsertAPIKey(ctx context.Context, k APIKey) error
	// UpdateAPIKey replaces the key with the same ID.
	UpdateAPIKey(ctx context.Context, k APIKey) error
}

// AccountReader reads the operator account and its sessions.
type AccountReader interface {
	// GetOperator returns the operator account, or ErrNotFound before
	// setup.
	GetOperator(ctx context.Context) (Operator, error)
	// GetSessionByTokenHash returns the session with the given TokenHash.
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (Session, error)
	// ListSessions returns all sessions, expired ones included, newest
	// first (CreatedAt descending, then ID descending).
	ListSessions(ctx context.Context) ([]Session, error)
}

// AccountWriter writes the operator account and its sessions.
type AccountWriter interface {
	// SaveOperator creates or replaces the single operator account.
	SaveOperator(ctx context.Context, o Operator) error
	// InsertSession stores a new session. Unique: ID, TokenHash.
	InsertSession(ctx context.Context, s Session) error
	// UpdateSession replaces the session with the same ID.
	UpdateSession(ctx context.Context, s Session) error
	// DeleteSession removes a session; removing a missing one is not an
	// error.
	DeleteSession(ctx context.Context, id string) error
	// DeleteSessionsExcept removes every session except the one with id
	// keepID (pass "" to remove all) and returns how many were removed.
	DeleteSessionsExcept(ctx context.Context, keepID string) (int, error)
	// DeleteSessionsExpiredBefore removes sessions with ExpiresAt before t
	// and returns how many were removed.
	DeleteSessionsExpiredBefore(ctx context.Context, t time.Time) (int, error)
}
