package coretest

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// MemStore is an in-memory [core.Store]: the reference implementation of
// the store contract, good enough to run the whole service in tests.
//
// Update takes an exclusive lock and works on a private copy of the state
// that replaces the committed state only if fn returns nil, which gives
// serializable transactions and rollback (including on panic) for free.
// View takes a shared lock. It is not built for large data sets: every
// Update copies everything.
type MemStore struct {
	mu    sync.RWMutex
	state *memState
}

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{state: newMemState()}
}

type memState struct {
	settings *core.Settings
	head     *core.ChainHead
	operator *core.Operator
	payments map[string]core.Payment
	// byAddress and byIdempotencyKey index payments (by ID), so that
	// inserting one costs no scan of all the others.
	byAddress        map[string]string
	byIdempotencyKey map[string]string
	transfers        map[core.TransferKey]core.Transfer
	links            map[string]core.PaymentLink
	events           []core.Event
	seq              int64
	cursors          map[string]int64
	endpoints        map[string]core.WebhookEndpoint
	deliveries       map[string]core.WebhookDelivery
	keys             map[string]core.APIKey
	sessions         map[string]core.Session
}

func newMemState() *memState {
	return &memState{
		payments:         map[string]core.Payment{},
		byAddress:        map[string]string{},
		byIdempotencyKey: map[string]string{},
		transfers:        map[core.TransferKey]core.Transfer{},
		links:            map[string]core.PaymentLink{},
		cursors:          map[string]int64{},
		endpoints:        map[string]core.WebhookEndpoint{},
		deliveries:       map[string]core.WebhookDelivery{},
		keys:             map[string]core.APIKey{},
		sessions:         map[string]core.Session{},
	}
}

// clone copies the containers. Stored values are never mutated in place
// (every write stores a fresh deep copy), so sharing them is safe.
func (s *memState) clone() *memState {
	c := *s
	c.payments = maps.Clone(s.payments)
	c.byAddress = maps.Clone(s.byAddress)
	c.byIdempotencyKey = maps.Clone(s.byIdempotencyKey)
	c.transfers = maps.Clone(s.transfers)
	c.links = maps.Clone(s.links)
	c.events = slices.Clone(s.events)
	c.cursors = maps.Clone(s.cursors)
	c.endpoints = maps.Clone(s.endpoints)
	c.deliveries = maps.Clone(s.deliveries)
	c.keys = maps.Clone(s.keys)
	c.sessions = maps.Clone(s.sessions)
	return &c
}

// Update implements [core.Store].
func (m *MemStore) Update(ctx context.Context, fn func(tx core.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tx := &memTx{st: m.state.clone()}
	defer func() { tx.done = true }()
	if err := fn(tx); err != nil {
		return err
	}
	m.state = tx.st
	return nil
}

// View implements [core.Store].
func (m *MemStore) View(ctx context.Context, fn func(r core.Reader) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	tx := &memTx{st: m.state}
	defer func() { tx.done = true }()
	return fn(tx)
}

// memTx implements core.Tx over one state. Views only ever call the read
// methods, so sharing the committed state with them is safe.
type memTx struct {
	st   *memState
	done bool
}

var _ core.Tx = (*memTx)(nil)

// check panics on use after the transaction function returned — a bug in
// the caller that a real database would also punish.
func (t *memTx) check() {
	if t.done {
		panic("coretest: store transaction used after it ended")
	}
}

func notFound(what, id string) error {
	return fmt.Errorf("coretest: %s %q: %w", what, id, core.ErrNotFound)
}

func duplicate(what, id string) error {
	return fmt.Errorf("coretest: duplicate %s %q: %w", what, id, core.ErrConflict)
}

// ---- deep copies ----

func clonePayment(p core.Payment) core.Payment {
	p.Metadata = maps.Clone(p.Metadata)
	if p.Metadata == nil {
		p.Metadata = map[string]string{}
	}
	if p.Resolution != nil {
		r := *p.Resolution
		p.Resolution = &r
	}
	return p
}

func cloneSettings(s core.Settings) core.Settings {
	s.Network.RPCURLs = slices.Clone(s.Network.RPCURLs)
	return s
}

func cloneEndpoint(e core.WebhookEndpoint) core.WebhookEndpoint {
	e.Events = slices.Clone(e.Events)
	return e
}

func cloneDelivery(d core.WebhookDelivery) core.WebhookDelivery {
	d.Attempts = slices.Clone(d.Attempts)
	return d
}

func cloneKey(k core.APIKey) core.APIKey {
	k.Scopes = slices.Clone(k.Scopes)
	return k
}

func cloneEvent(e core.Event) core.Event {
	e.Data = slices.Clone(e.Data)
	return e
}

// ---- settings ----

func (t *memTx) GetSettings(context.Context) (core.Settings, error) {
	t.check()
	if t.st.settings == nil {
		return core.Settings{}, notFound("settings", "")
	}
	return cloneSettings(*t.st.settings), nil
}

func (t *memTx) SaveSettings(_ context.Context, s core.Settings) error {
	t.check()
	s = cloneSettings(s)
	t.st.settings = &s
	return nil
}

func (t *memTx) GetChainHead(context.Context) (core.ChainHead, error) {
	t.check()
	if t.st.head == nil {
		return core.ChainHead{}, notFound("chain head", "")
	}
	return *t.st.head, nil
}

func (t *memTx) SaveChainHead(_ context.Context, h core.ChainHead) error {
	t.check()
	t.st.head = &h
	return nil
}

// ---- payments ----

func (t *memTx) GetPayment(_ context.Context, id string) (core.Payment, error) {
	t.check()
	p, ok := t.st.payments[id]
	if !ok {
		return core.Payment{}, notFound("payment", id)
	}
	return clonePayment(p), nil
}

func (t *memTx) GetPaymentByAddress(_ context.Context, address string) (core.Payment, error) {
	t.check()
	if id, ok := t.st.byAddress[address]; ok {
		return clonePayment(t.st.payments[id]), nil
	}
	return core.Payment{}, notFound("payment at address", address)
}

func (t *memTx) GetPaymentByIdempotencyKey(_ context.Context, key string) (core.Payment, error) {
	t.check()
	if id, ok := t.st.byIdempotencyKey[key]; ok && key != "" {
		return clonePayment(t.st.payments[id]), nil
	}
	return core.Payment{}, notFound("payment with idempotency key", key)
}

func (t *memTx) InsertPayment(_ context.Context, p core.Payment) error {
	t.check()
	if _, ok := t.st.payments[p.ID]; ok {
		return duplicate("payment", p.ID)
	}
	if _, ok := t.st.byAddress[p.Address]; ok {
		return duplicate("payment address", p.Address)
	}
	if _, ok := t.st.byIdempotencyKey[p.IdempotencyKey]; ok && p.IdempotencyKey != "" {
		return duplicate("idempotency key", p.IdempotencyKey)
	}
	t.st.payments[p.ID] = clonePayment(p)
	t.index(core.Payment{}, p)
	return nil
}

// index moves the payment's index entries from what it was to what it is.
func (t *memTx) index(was, is core.Payment) {
	if was.Address != is.Address {
		delete(t.st.byAddress, was.Address)
	}
	if was.IdempotencyKey != is.IdempotencyKey {
		delete(t.st.byIdempotencyKey, was.IdempotencyKey)
	}
	t.st.byAddress[is.Address] = is.ID
	if is.IdempotencyKey != "" {
		t.st.byIdempotencyKey[is.IdempotencyKey] = is.ID
	}
}

func (t *memTx) UpdatePayment(_ context.Context, p core.Payment) error {
	t.check()
	was, ok := t.st.payments[p.ID]
	if !ok {
		return notFound("payment", p.ID)
	}
	t.st.payments[p.ID] = clonePayment(p)
	t.index(was, p)
	return nil
}

func (t *memTx) matchPayment(p core.Payment, f core.PaymentFilter) bool {
	if len(f.Statuses) > 0 && !slices.Contains(f.Statuses, p.Status) {
		return false
	}
	if f.NeedsAttention != nil && p.NeedsAttention() != *f.NeedsAttention {
		return false
	}
	if f.OrderID != "" && p.OrderID != f.OrderID {
		return false
	}
	if f.LinkID != "" && p.LinkID != f.LinkID {
		return false
	}
	if f.FromLink != nil && (p.LinkID != "") != *f.FromLink {
		return false
	}
	if f.ChainID != 0 && p.Asset.ChainID != f.ChainID {
		return false
	}
	if !f.CreatedFrom.IsZero() && p.CreatedAt.Before(f.CreatedFrom) {
		return false
	}
	if !f.CreatedTo.IsZero() && !p.CreatedAt.Before(f.CreatedTo) {
		return false
	}
	if !f.PaidFrom.IsZero() || !f.PaidTo.IsZero() {
		if p.PaidAt.IsZero() {
			return false
		}
		if !f.PaidFrom.IsZero() && p.PaidAt.Before(f.PaidFrom) {
			return false
		}
		if !f.PaidTo.IsZero() && !p.PaidAt.Before(f.PaidTo) {
			return false
		}
	}
	if !f.ExpiresBefore.IsZero() && p.ExpiresAt.After(f.ExpiresBefore) {
		return false
	}
	if !f.WatchedAfter.IsZero() && !p.WatchUntil.After(f.WatchedAfter) {
		return false
	}
	if !f.WatchEndsBy.IsZero() && p.WatchUntil.After(f.WatchEndsBy) {
		return false
	}
	if f.Retired != nil && p.RetiredAt.IsZero() == *f.Retired {
		return false
	}
	if f.RetiredUnchecked && !p.RetiredUnchecked {
		return false
	}
	if f.Contract != "" && p.Asset.Contract != f.Contract {
		return false
	}
	if f.Search != "" {
		needle := strings.ToLower(f.Search)
		has := func(s string) bool { return strings.Contains(strings.ToLower(s), needle) }
		found := has(p.ID) || has(p.OrderID) || has(p.Description) || has(p.CustomerEmail) || has(p.Address)
		if !found {
			for _, tr := range t.st.transfers {
				if tr.PaymentID == p.ID && (has(tr.TxHash) || has(tr.From)) {
					found = true
					break
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (t *memTx) ListPayments(_ context.Context, f core.PaymentFilter, sort core.PaymentSort, page core.Page) ([]core.Payment, error) {
	t.check()
	var out []core.Payment
	for _, p := range t.st.payments {
		if t.matchPayment(p, f) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b core.Payment) int {
		var c int
		if sort.By == core.SortByAmount {
			c = a.Amount.Cmp(b.Amount)
		} else {
			c = a.CreatedAt.Compare(b.CreatedAt)
		}
		if c == 0 {
			c = cmp.Compare(a.ID, b.ID)
		}
		if sort.Desc {
			c = -c
		}
		return c
	})
	out = window(out, page)
	for i := range out {
		out[i] = clonePayment(out[i])
	}
	return out, nil
}

func (t *memTx) CountPayments(_ context.Context, f core.PaymentFilter) (int, error) {
	t.check()
	n := 0
	for _, p := range t.st.payments {
		if t.matchPayment(p, f) {
			n++
		}
	}
	return n, nil
}

func window[T any](items []T, page core.Page) []T {
	if page.Offset > 0 {
		if page.Offset >= len(items) {
			return nil
		}
		items = items[page.Offset:]
	}
	if page.Limit > 0 && page.Limit < len(items) {
		items = items[:page.Limit]
	}
	return items
}

// ---- transfers ----

func (t *memTx) GetTransfer(_ context.Context, key core.TransferKey) (core.Transfer, error) {
	t.check()
	tr, ok := t.st.transfers[key]
	if !ok {
		return core.Transfer{}, notFound("transfer", key.TxHash)
	}
	return tr, nil
}

func (t *memTx) listTransfers(match func(core.Transfer) bool) []core.Transfer {
	var out []core.Transfer
	for _, tr := range t.st.transfers {
		if match(tr) {
			out = append(out, tr)
		}
	}
	slices.SortFunc(out, func(a, b core.Transfer) int {
		return cmp.Or(
			cmp.Compare(a.BlockNumber, b.BlockNumber),
			cmp.Compare(a.LogIndex, b.LogIndex),
			cmp.Compare(a.BlockHash, b.BlockHash),
			cmp.Compare(a.TxHash, b.TxHash),
		)
	})
	return out
}

func (t *memTx) ListTransfers(_ context.Context, paymentID string) ([]core.Transfer, error) {
	t.check()
	return t.listTransfers(func(tr core.Transfer) bool { return tr.PaymentID == paymentID }), nil
}

func (t *memTx) ListTransfersByTxHash(_ context.Context, txHash string) ([]core.Transfer, error) {
	t.check()
	return t.listTransfers(func(tr core.Transfer) bool { return tr.TxHash == txHash }), nil
}

func (t *memTx) ListPendingTransfers(context.Context) ([]core.Transfer, error) {
	t.check()
	return t.listTransfers(func(tr core.Transfer) bool { return tr.Status == core.TransferPending }), nil
}

func (t *memTx) InsertTransfer(_ context.Context, tr core.Transfer) error {
	t.check()
	if _, ok := t.st.transfers[tr.Key()]; ok {
		return duplicate("transfer", tr.TxHash)
	}
	t.st.transfers[tr.Key()] = tr
	return nil
}

func (t *memTx) UpdateTransfer(_ context.Context, tr core.Transfer) error {
	t.check()
	if _, ok := t.st.transfers[tr.Key()]; !ok {
		return notFound("transfer", tr.TxHash)
	}
	t.st.transfers[tr.Key()] = tr
	return nil
}

// ---- links ----

func (t *memTx) GetLink(_ context.Context, id string) (core.PaymentLink, error) {
	t.check()
	l, ok := t.st.links[id]
	if !ok {
		return core.PaymentLink{}, notFound("link", id)
	}
	return l, nil
}

func (t *memTx) GetLinkBySlug(_ context.Context, slug string) (core.PaymentLink, error) {
	t.check()
	for _, l := range t.st.links {
		if l.Slug == slug {
			return l, nil
		}
	}
	return core.PaymentLink{}, notFound("link with slug", slug)
}

func (t *memTx) ListLinks(context.Context) ([]core.PaymentLink, error) {
	t.check()
	out := slices.Collect(maps.Values(t.st.links))
	slices.SortFunc(out, func(a, b core.PaymentLink) int {
		return -cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func (t *memTx) InsertLink(_ context.Context, l core.PaymentLink) error {
	t.check()
	if _, ok := t.st.links[l.ID]; ok {
		return duplicate("link", l.ID)
	}
	for _, q := range t.st.links {
		if q.Slug == l.Slug {
			return duplicate("link slug", l.Slug)
		}
	}
	t.st.links[l.ID] = l
	return nil
}

func (t *memTx) UpdateLink(_ context.Context, l core.PaymentLink) error {
	t.check()
	if _, ok := t.st.links[l.ID]; !ok {
		return notFound("link", l.ID)
	}
	t.st.links[l.ID] = l
	return nil
}

func (t *memTx) DeleteLink(_ context.Context, id string) error {
	t.check()
	if _, ok := t.st.links[id]; !ok {
		return notFound("link", id)
	}
	delete(t.st.links, id)
	return nil
}

// ---- events ----

func (t *memTx) ListEvents(_ context.Context, f core.EventFilter) ([]core.Event, error) {
	t.check()
	var out []core.Event
	for _, e := range t.st.events {
		if e.Seq <= f.After ||
			(f.PaymentID != "" && e.PaymentID != f.PaymentID) ||
			(len(f.Types) > 0 && !slices.Contains(f.Types, e.Type)) {
			continue
		}
		out = append(out, cloneEvent(e))
		if f.Limit > 0 && len(out) == f.Limit {
			break
		}
	}
	return out, nil
}

func (t *memTx) AppendEvent(_ context.Context, e *core.Event) error {
	t.check()
	for _, q := range t.st.events {
		if q.ID == e.ID {
			return duplicate("event", e.ID)
		}
	}
	t.st.seq++
	e.Seq = t.st.seq
	t.st.events = append(t.st.events, cloneEvent(*e))
	return nil
}

func (t *memTx) GetCursor(_ context.Context, consumer string) (int64, error) {
	t.check()
	return t.st.cursors[consumer], nil
}

func (t *memTx) SetCursor(_ context.Context, consumer string, seq int64) error {
	t.check()
	t.st.cursors[consumer] = seq
	return nil
}

// ---- webhooks ----

func (t *memTx) GetWebhookEndpoint(_ context.Context, id string) (core.WebhookEndpoint, error) {
	t.check()
	e, ok := t.st.endpoints[id]
	if !ok {
		return core.WebhookEndpoint{}, notFound("webhook endpoint", id)
	}
	return cloneEndpoint(e), nil
}

func (t *memTx) ListWebhookEndpoints(context.Context) ([]core.WebhookEndpoint, error) {
	t.check()
	var out []core.WebhookEndpoint
	for _, e := range t.st.endpoints {
		out = append(out, cloneEndpoint(e))
	}
	slices.SortFunc(out, func(a, b core.WebhookEndpoint) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func (t *memTx) InsertWebhookEndpoint(_ context.Context, e core.WebhookEndpoint) error {
	t.check()
	if _, ok := t.st.endpoints[e.ID]; ok {
		return duplicate("webhook endpoint", e.ID)
	}
	t.st.endpoints[e.ID] = cloneEndpoint(e)
	return nil
}

func (t *memTx) UpdateWebhookEndpoint(_ context.Context, e core.WebhookEndpoint) error {
	t.check()
	if _, ok := t.st.endpoints[e.ID]; !ok {
		return notFound("webhook endpoint", e.ID)
	}
	t.st.endpoints[e.ID] = cloneEndpoint(e)
	return nil
}

func (t *memTx) DeleteWebhookEndpoint(_ context.Context, id string) error {
	t.check()
	if _, ok := t.st.endpoints[id]; !ok {
		return notFound("webhook endpoint", id)
	}
	delete(t.st.endpoints, id)
	maps.DeleteFunc(t.st.deliveries, func(_ string, d core.WebhookDelivery) bool { return d.EndpointID == id })
	return nil
}

func (t *memTx) GetDelivery(_ context.Context, id string) (core.WebhookDelivery, error) {
	t.check()
	d, ok := t.st.deliveries[id]
	if !ok {
		return core.WebhookDelivery{}, notFound("webhook delivery", id)
	}
	return cloneDelivery(d), nil
}

func matchDelivery(d core.WebhookDelivery, f core.DeliveryFilter) bool {
	switch {
	case f.EndpointID != "" && d.EndpointID != f.EndpointID,
		f.PaymentID != "" && d.PaymentID != f.PaymentID,
		f.EventID != "" && d.EventID != f.EventID,
		len(f.Statuses) > 0 && !slices.Contains(f.Statuses, d.Status),
		!f.DueBefore.IsZero() && (d.NextAttemptAt.IsZero() || d.NextAttemptAt.After(f.DueBefore)),
		!f.CreatedFrom.IsZero() && d.CreatedAt.Before(f.CreatedFrom),
		f.Unattempted != nil && *f.Unattempted != (d.EventSeq > 0 && d.Attempt == 0):
		return false
	}
	return true
}

func (t *memTx) ListDeliveries(_ context.Context, f core.DeliveryFilter, page core.Page) ([]core.WebhookDelivery, error) {
	t.check()
	var out []core.WebhookDelivery
	for _, d := range t.st.deliveries {
		if matchDelivery(d, f) {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b core.WebhookDelivery) int {
		if !f.DueBefore.IsZero() {
			return cmp.Or(a.NextAttemptAt.Compare(b.NextAttemptAt), cmp.Compare(a.ID, b.ID))
		}
		if f.Unattempted != nil && *f.Unattempted {
			return cmp.Or(cmp.Compare(a.EventSeq, b.EventSeq), cmp.Compare(a.ID, b.ID))
		}
		return -cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	out = window(out, page)
	for i := range out {
		out[i] = cloneDelivery(out[i])
	}
	return out, nil
}

func (t *memTx) CountDeliveries(_ context.Context, f core.DeliveryFilter) (int, error) {
	t.check()
	n := 0
	for _, d := range t.st.deliveries {
		if matchDelivery(d, f) {
			n++
		}
	}
	return n, nil
}

func (t *memTx) InsertDelivery(_ context.Context, d core.WebhookDelivery) error {
	t.check()
	if _, ok := t.st.deliveries[d.ID]; ok {
		return duplicate("webhook delivery", d.ID)
	}
	t.st.deliveries[d.ID] = cloneDelivery(d)
	return nil
}

func (t *memTx) UpdateDelivery(_ context.Context, d core.WebhookDelivery) error {
	t.check()
	if _, ok := t.st.deliveries[d.ID]; !ok {
		return notFound("webhook delivery", d.ID)
	}
	t.st.deliveries[d.ID] = cloneDelivery(d)
	return nil
}

func (t *memTx) DeleteDeliveriesBefore(_ context.Context, before time.Time) (int, error) {
	t.check()
	n := len(t.st.deliveries)
	maps.DeleteFunc(t.st.deliveries, func(_ string, d core.WebhookDelivery) bool { return d.CreatedAt.Before(before) })
	return n - len(t.st.deliveries), nil
}

// ---- API keys ----

func (t *memTx) GetAPIKey(_ context.Context, id string) (core.APIKey, error) {
	t.check()
	k, ok := t.st.keys[id]
	if !ok {
		return core.APIKey{}, notFound("api key", id)
	}
	return cloneKey(k), nil
}

func (t *memTx) GetAPIKeyByHash(_ context.Context, hash string) (core.APIKey, error) {
	t.check()
	for _, k := range t.st.keys {
		if k.SecretHash == hash {
			return cloneKey(k), nil
		}
	}
	return core.APIKey{}, notFound("api key with hash", "…")
}

func (t *memTx) ListAPIKeys(context.Context) ([]core.APIKey, error) {
	t.check()
	var out []core.APIKey
	for _, k := range t.st.keys {
		out = append(out, cloneKey(k))
	}
	slices.SortFunc(out, func(a, b core.APIKey) int {
		return -cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func (t *memTx) InsertAPIKey(_ context.Context, k core.APIKey) error {
	t.check()
	if _, ok := t.st.keys[k.ID]; ok {
		return duplicate("api key", k.ID)
	}
	for _, q := range t.st.keys {
		if q.SecretHash == k.SecretHash {
			return duplicate("api key hash", "…")
		}
	}
	t.st.keys[k.ID] = cloneKey(k)
	return nil
}

func (t *memTx) UpdateAPIKey(_ context.Context, k core.APIKey) error {
	t.check()
	if _, ok := t.st.keys[k.ID]; !ok {
		return notFound("api key", k.ID)
	}
	t.st.keys[k.ID] = cloneKey(k)
	return nil
}

// ---- operator and sessions ----

func (t *memTx) GetOperator(context.Context) (core.Operator, error) {
	t.check()
	if t.st.operator == nil {
		return core.Operator{}, notFound("operator", "")
	}
	return *t.st.operator, nil
}

func (t *memTx) SaveOperator(_ context.Context, o core.Operator) error {
	t.check()
	t.st.operator = &o
	return nil
}

func (t *memTx) GetSessionByTokenHash(_ context.Context, hash string) (core.Session, error) {
	t.check()
	for _, s := range t.st.sessions {
		if s.TokenHash == hash {
			return s, nil
		}
	}
	return core.Session{}, notFound("session", "…")
}

func (t *memTx) ListSessions(context.Context) ([]core.Session, error) {
	t.check()
	out := slices.Collect(maps.Values(t.st.sessions))
	slices.SortFunc(out, func(a, b core.Session) int {
		return -cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func (t *memTx) InsertSession(_ context.Context, s core.Session) error {
	t.check()
	if _, ok := t.st.sessions[s.ID]; ok {
		return duplicate("session", s.ID)
	}
	for _, q := range t.st.sessions {
		if q.TokenHash == s.TokenHash {
			return duplicate("session token", "…")
		}
	}
	t.st.sessions[s.ID] = s
	return nil
}

func (t *memTx) UpdateSession(_ context.Context, s core.Session) error {
	t.check()
	if _, ok := t.st.sessions[s.ID]; !ok {
		return notFound("session", s.ID)
	}
	t.st.sessions[s.ID] = s
	return nil
}

func (t *memTx) DeleteSession(_ context.Context, id string) error {
	t.check()
	delete(t.st.sessions, id)
	return nil
}

func (t *memTx) DeleteSessionsExcept(_ context.Context, keepID string) (int, error) {
	t.check()
	n := len(t.st.sessions)
	maps.DeleteFunc(t.st.sessions, func(id string, _ core.Session) bool { return id != keepID })
	return n - len(t.st.sessions), nil
}

func (t *memTx) DeleteSessionsExpiredBefore(_ context.Context, before time.Time) (int, error) {
	t.check()
	n := len(t.st.sessions)
	maps.DeleteFunc(t.st.sessions, func(_ string, s core.Session) bool { return s.ExpiresAt.Before(before) })
	return n - len(t.st.sessions), nil
}
