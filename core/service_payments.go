package core

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"
	"unicode/utf8"
)

// CreatePaymentInput describes a new payment.
type CreatePaymentInput struct {
	// Amount is the amount due as a decimal string in whole units
	// ("149.00"). Required, positive, at most the asset's decimals.
	Amount string
	// OrderID is the merchant's reference (≤ 128 characters). Optional and
	// not unique.
	OrderID string
	// Description is shown on the checkout page (≤ 500 characters).
	Description string
	// CustomerEmail is optional.
	CustomerEmail string
	// RedirectURL overrides the default redirect; http(s) only.
	RedirectURL string
	// Metadata is up to 20 key/value pairs (keys ≤ 40, values ≤ 500
	// characters).
	Metadata map[string]string
	// TTL overrides the default time to pay; 0 uses the setting. Between
	// [MinPaymentTTL] and [MaxPaymentTTL].
	TTL time.Duration
	// IdempotencyKey makes creation safe to retry (≤ 255 printable ASCII
	// characters). Creating again with the same key returns the first
	// payment instead of a second one; reusing a key with different
	// parameters fails with [ErrIdempotencyMismatch]. Keys never expire.
	IdempotencyKey string
}

// paymentParams is a validated CreatePaymentInput.
type paymentParams struct {
	amount         Amount
	orderID        string
	description    string
	email          string
	redirectURL    string
	metadata       map[string]string
	ttl            time.Duration
	explicitTTL    bool
	idempotencyKey string
	linkID         string
}

func validatePaymentInput(in CreatePaymentInput, st Settings) (paymentParams, error) {
	var v validation
	p := paymentParams{
		orderID:     cleanText(&v, "order_id", in.OrderID, 128, false),
		description: cleanMultiline(&v, "description", in.Description, 500, false),
		email:       cleanEmail(&v, "customer_email", in.CustomerEmail, false),
		redirectURL: cleanWebURL(&v, "redirect_url", in.RedirectURL, false),
		metadata:    map[string]string{},
		ttl:         st.Payments.TTL,
	}
	p.amount = parsePaymentAmount(&v, "amount", in.Amount, st.Asset)
	if p.redirectURL == "" {
		p.redirectURL = st.Payments.DefaultRedirectURL
	}
	if in.TTL != 0 {
		p.ttl, p.explicitTTL = in.TTL, true
		if in.TTL < MinPaymentTTL || in.TTL > MaxPaymentTTL {
			v.add("ttl", "invalid", "ttl must be between %s and %s", MinPaymentTTL, MaxPaymentTTL)
		}
	}
	if len(in.Metadata) > 20 {
		v.add("metadata", "too_many", "metadata can hold at most 20 keys")
	}
	for k, val := range in.Metadata {
		if k == "" || utf8.RuneCountInString(k) > 40 || !utf8.ValidString(k) || strings.ContainsFunc(k, isControl) {
			v.add("metadata", "invalid", "metadata keys must be 1 to 40 characters")
			break
		}
		if utf8.RuneCountInString(val) > 500 {
			v.add("metadata."+k, "too_long", "metadata values must be at most 500 characters")
			continue
		}
		if !utf8.ValidString(val) || strings.ContainsFunc(val, isControl) {
			v.add("metadata."+k, "invalid", "metadata values must not contain control characters")
			continue
		}
		p.metadata[k] = val
	}
	p.idempotencyKey = in.IdempotencyKey
	if len(p.idempotencyKey) > 255 || strings.ContainsFunc(p.idempotencyKey, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		v.add("idempotency_key", "invalid", "idempotency key must be at most 255 printable ASCII characters without spaces")
	}
	return p, v.errOrNil()
}

// parsePaymentAmount parses a positive, bounded amount in whole units.
func parsePaymentAmount(v *validation, field, s string, asset Asset) Amount {
	s = strings.TrimSpace(s)
	if s == "" {
		v.add(field, "required", "amount is required")
		return Amount{}
	}
	a, err := ParseAmount(s, asset.Decimals)
	switch {
	case err != nil:
		v.add(field, "invalid", "%s", errMessage(err))
	case a.IsZero():
		v.add(field, "too_small", "amount must be greater than zero")
	case a.Cmp(wholeUnits(MaxPaymentUnits, asset.Decimals)) > 0:
		v.add(field, "too_large", "amount must be at most %d %s", MaxPaymentUnits, asset.Symbol)
	}
	return a
}

// CreatePayment creates a payment with a fresh deposit address and returns
// it together with whether it was newly created (false when an existing
// payment was returned for the idempotency key). Requires
// [ScopePaymentsWrite].
//
// The payment starts in status awaiting; payment.created is appended to
// the event log in the same transaction.
func (s *Service) CreatePayment(ctx context.Context, in CreatePaymentInput) (PaymentView, bool, error) {
	if _, err := authorize(ctx, permScope(ScopePaymentsWrite)); err != nil {
		return PaymentView{}, false, err
	}
	var view PaymentView
	var created bool
	err := s.update(ctx, func(u *unit) error {
		st, err := u.loadSettings(ctx)
		if err != nil {
			return err
		}
		if !st.SetupCompleted {
			return newError(ErrSetupRequired, "")
		}
		params, err := validatePaymentInput(in, st)
		if err != nil {
			return err
		}
		p, isNew, err := s.createPayment(ctx, u, st, params)
		if err != nil {
			return err
		}
		head, err := u.loadHead(ctx)
		if err != nil {
			return err
		}
		view, created = NewPaymentView(p, headFor(head, st, p), st.PublicURL), isNew
		return nil
	})
	return view, created, err
}

// createPayment inserts a payment for validated parameters, or returns the
// payment that already owns the idempotency key.
func (s *Service) createPayment(ctx context.Context, u *unit, st Settings, in paymentParams) (Payment, bool, error) {
	if in.idempotencyKey != "" {
		existing, err := u.GetPaymentByIdempotencyKey(ctx, in.idempotencyKey)
		switch {
		case err == nil:
			if !sameRequest(existing, in) {
				return Payment{}, false, newError(ErrIdempotencyMismatch, "")
			}
			return existing, false, nil
		case !errors.Is(err, ErrNotFound):
			return Payment{}, false, err
		}
	}

	// The address is scanned for from the processed head's final block
	// (CreatedAtBlock, ChainHead.Final): a block the chain adapter has
	// checked to be on the chain before anybody could pay to the address,
	// and that no reorganisation it accepts can replace — the head itself
	// can be, and the payer's transaction mined again at its height or
	// below. Without a head no address is handed out (ErrChainNotReady). A
	// block derived from the creation time instead would be as right as
	// the server clock was at that moment — and a clock that was ahead
	// puts the start of the scan above the payment.
	if problem := st.Network.problem(); problem != "" {
		// No chain watcher runs on these settings: whatever head the core
		// holds is from before, and nothing is scanned for a new address.
		return Payment{}, false, newError(ErrNetworkInvalid, "the network settings cannot be used: %s; fix them in the network settings", problem)
	}
	head, err := u.loadHead(ctx)
	if err != nil {
		return Payment{}, false, err
	}
	if head.ChainID != st.Asset.ChainID || head.Number == 0 {
		return Payment{}, false, newError(ErrChainNotReady, "")
	}
	if behind := s.clockBehind(u.now, head, st); behind > 0 {
		return Payment{}, false, newError(ErrClockBehind, "the server clock is %s behind the chain (tolerance %s); no payment can be created until it is right — check time synchronisation (NTP)",
			behind.Round(time.Second), s.maxHeadAhead)
	}

	// Allocate the next address index. The read-modify-write of the
	// settings is safe because Update transactions are serializable.
	index, address, err := s.allocateAddress(ctx, u, st.Wallet)
	if err != nil {
		return Payment{}, false, err
	}
	st.Wallet.NextAddressIndex = index + 1
	if err := u.saveSettings(ctx, st); err != nil {
		return Payment{}, false, err
	}

	id, err := s.newID(prefixPayment)
	if err != nil {
		return Payment{}, false, err
	}
	tolerance, err := ParseAmount(st.Payments.UnderpaymentTolerance, st.Asset.Decimals)
	if err != nil {
		tolerance = Amount{}
	}
	// Counted from the chain's time if the clock is behind it (within the
	// tolerance clockBehind allows): the deadline is judged by both.
	expires := windowStart(u.now, head).Add(in.ttl)
	p := Payment{
		ID:                    id,
		Status:                StatusAwaiting,
		Amount:                in.amount,
		Asset:                 st.Asset,
		Address:               address,
		AddressIndex:          index,
		OrderID:               in.orderID,
		Description:           in.description,
		CustomerEmail:         in.email,
		RedirectURL:           in.redirectURL,
		Metadata:              maps.Clone(in.metadata),
		LinkID:                in.linkID,
		IdempotencyKey:        in.idempotencyKey,
		RequiredConfirmations: st.Payments.RequiredConfirmations,
		Tolerance:             effectiveTolerance(in.amount, tolerance),
		CreatedAt:             u.now,
		ExpiresAt:             expires,
		UpdatedAt:             u.now,
		WatchUntil:            expires.Add(s.grace + s.unfundedWatchWindow),
		CreatedAtBlock:        head.startBlock(),
	}
	if p.Metadata == nil {
		p.Metadata = map[string]string{}
	}
	if err := u.InsertPayment(ctx, p); err != nil {
		return Payment{}, false, err
	}
	if err := u.emitPayment(ctx, EventPaymentCreated, p); err != nil {
		return Payment{}, false, err
	}
	return p, true, nil
}

// allocateAddress returns the first deposit address at or after the
// wallet's next index that no payment owns yet. An address is never handed
// out twice, even if the operator rewinds the index or goes back to a key
// that was used before (the index then restarts at 0 over a long run of
// used addresses).
//
// Used indexes are skipped by galloping — probing 1, 2, 4, 8, … ahead until
// a free address is found, then bisecting back to the edge of the used
// run — so getting past n used addresses costs about 2·log2(n) derivations
// instead of n, once; the result is saved as the wallet's next index. If
// the used indexes are not one contiguous run (the index was set by hand)
// the address found is still unused, just not necessarily the lowest one.
func (s *Service) allocateAddress(ctx context.Context, u *unit, w WalletSettings) (uint32, string, error) {
	const maxIndex = 1<<31 - 1 // the last non-hardened child index
	probe := func(i uint32) (addr string, used bool, err error) {
		raw, err := s.addresses.DeriveAddress(ctx, w, i)
		if err != nil {
			return "", false, internal("derive deposit address", err)
		}
		if addr, err = NormalizeAddress(raw); err != nil {
			return "", false, internal("derive deposit address", err)
		}
		_, err = u.GetPaymentByAddress(ctx, addr)
		if errors.Is(err, ErrNotFound) {
			return addr, false, nil
		}
		return addr, err == nil, err
	}
	exhausted := &Error{Kind: KindUnavailable, Code: "no_free_address", Message: "the wallet has no unused deposit address left"}
	if w.NextAddressIndex > maxIndex {
		return 0, "", exhausted
	}
	lo := w.NextAddressIndex
	addr, used, err := probe(lo)
	if err != nil || !used {
		return lo, addr, err
	}
	// lo is used. Gallop to a free index hi.
	hi, hiAddr := lo, ""
	for step := uint32(1); ; step *= 2 {
		if lo == maxIndex {
			return 0, "", exhausted
		}
		next := uint32(maxIndex)
		if step <= maxIndex-lo {
			next = lo + step
		}
		a, used, err := probe(next)
		if err != nil {
			return 0, "", err
		}
		if !used {
			hi, hiAddr = next, a
			break
		}
		lo = next
	}
	// lo is used, hi is free: bisect to the first free index after lo.
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		a, used, err := probe(mid)
		if err != nil {
			return 0, "", err
		}
		if used {
			lo = mid
		} else {
			hi, hiAddr = mid, a
		}
	}
	return hi, hiAddr, nil
}

// sameRequest reports whether an idempotent retry asks for the payment
// that already exists.
func sameRequest(p Payment, in paymentParams) bool {
	return p.Amount == in.amount &&
		p.OrderID == in.orderID &&
		p.Description == in.description &&
		p.CustomerEmail == in.email &&
		p.RedirectURL == in.redirectURL &&
		p.LinkID == in.linkID &&
		maps.Equal(p.Metadata, in.metadata) &&
		(!in.explicitTTL || p.ExpiresAt.Sub(p.CreatedAt) == in.ttl)
}

// viewContext is what rendering views needs besides the object itself.
type viewContext struct {
	head     ChainHead
	settings Settings
}

func loadViewContext(ctx context.Context, r SettingsReader) (viewContext, error) {
	var vc viewContext
	var err error
	if vc.settings, err = loadSettings(ctx, r); err != nil {
		return vc, err
	}
	vc.head, err = loadHead(ctx, r)
	return vc, err
}

func (vc viewContext) payment(p Payment) PaymentView {
	return NewPaymentView(p, headFor(vc.head, vc.settings, p), vc.settings.PublicURL)
}

// getPayment maps the store's not-found to a payment-specific error.
func getPayment(ctx context.Context, r PaymentReader, id string) (Payment, error) {
	p, err := r.GetPayment(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return p, notFound("payment", id)
	}
	return p, err
}

// GetPayment returns one payment. Requires [ScopePaymentsRead].
func (s *Service) GetPayment(ctx context.Context, id string) (PaymentView, error) {
	var view PaymentView
	if _, err := authorize(ctx, permScope(ScopePaymentsRead)); err != nil {
		return view, err
	}
	err := s.store.View(ctx, func(r Reader) error {
		vc, err := loadViewContext(ctx, r)
		if err != nil {
			return err
		}
		p, err := getPayment(ctx, r, id)
		if err != nil {
			return err
		}
		view = vc.payment(p)
		return nil
	})
	return view, err
}

// PaymentDetail is a payment with everything that happened to it.
type PaymentDetail struct {
	Payment PaymentView
	// Transfers are all observed transfers, orphaned ones included, in
	// chain order.
	Transfers []TransferView
	// Events is the payment's slice of the event log, oldest first: its
	// timeline.
	Events []Event
	// Deliveries are the webhook deliveries of those events, newest first.
	// They are included only for the operator, system actors and API keys
	// holding [ScopeWebhooksManage] — the same callers that may read the
	// delivery log — and nil otherwise: a delivery record contains the
	// endpoint URL and the endpoint's response.
	Deliveries []WebhookDelivery
}

// GetPaymentDetail returns a payment with its transfers, timeline and —
// for callers that may read the delivery log — its webhook deliveries,
// read in one consistent snapshot. Requires [ScopePaymentsRead].
func (s *Service) GetPaymentDetail(ctx context.Context, id string) (PaymentDetail, error) {
	var d PaymentDetail
	if _, err := authorize(ctx, permScope(ScopePaymentsRead)); err != nil {
		return d, err
	}
	_, deliveryErr := authorize(ctx, permScope(ScopeWebhooksManage))
	withDeliveries := deliveryErr == nil
	err := s.store.View(ctx, func(r Reader) error {
		vc, err := loadViewContext(ctx, r)
		if err != nil {
			return err
		}
		p, err := getPayment(ctx, r, id)
		if err != nil {
			return err
		}
		d.Payment = vc.payment(p)
		transfers, err := r.ListTransfers(ctx, id)
		if err != nil {
			return err
		}
		d.Transfers = make([]TransferView, 0, len(transfers))
		for _, t := range transfers {
			d.Transfers = append(d.Transfers, NewTransferView(t, p, headFor(vc.head, vc.settings, p)))
		}
		if d.Events, err = r.ListEvents(ctx, EventFilter{PaymentID: id}); err != nil {
			return err
		}
		if withDeliveries {
			d.Deliveries, err = r.ListDeliveries(ctx, DeliveryFilter{PaymentID: id}, Page{})
		}
		return err
	})
	return d, err
}

// PaymentGroup is a named status group, matching the dashboard's tabs.
type PaymentGroup string

// Payment groups.
const (
	// GroupAll matches every payment.
	GroupAll PaymentGroup = "all"
	// GroupOpen matches awaiting, detected and confirming.
	GroupOpen PaymentGroup = "open"
	// GroupPaid matches status paid.
	GroupPaid PaymentGroup = "paid"
	// GroupAttention matches payments with an unresolved issue.
	GroupAttention PaymentGroup = "attention"
	// GroupExpired matches status expired.
	GroupExpired PaymentGroup = "expired"
)

// Apply returns f restricted to the group, replacing f.Statuses and
// f.NeedsAttention. Unknown groups behave like [GroupAll].
func (g PaymentGroup) Apply(f PaymentFilter) PaymentFilter {
	f.Statuses, f.NeedsAttention = nil, nil
	switch g {
	case GroupOpen:
		f.Statuses = []PaymentStatus{StatusAwaiting, StatusDetected, StatusConfirming}
	case GroupPaid:
		f.Statuses = []PaymentStatus{StatusPaid}
	case GroupAttention:
		yes := true
		f.NeedsAttention = &yes
	case GroupExpired:
		f.Statuses = []PaymentStatus{StatusExpired}
	}
	return f
}

// ListPaymentsInput selects a page of payments.
type ListPaymentsInput struct {
	// Filter selects payments; use [PaymentGroup.Apply] for the dashboard
	// tabs. ExpiresBefore, WatchedAfter, WatchEndsBy and Retired are
	// internal and ignored here.
	Filter PaymentFilter
	// Sort orders the result; the zero value is newest first.
	Sort *PaymentSort
	// Page is the window; Limit defaults to 50 and is capped at 500.
	Page Page
}

// PaymentPage is one page of a payment list.
type PaymentPage struct {
	Items []PaymentView
	// Total is the number of payments matching the filter, ignoring the
	// page window.
	Total int
}

// ListPayments returns a filtered, sorted page of payments and the total
// number of matches. Requires [ScopePaymentsRead].
func (s *Service) ListPayments(ctx context.Context, in ListPaymentsInput) (PaymentPage, error) {
	var page PaymentPage
	if _, err := authorize(ctx, permScope(ScopePaymentsRead)); err != nil {
		return page, err
	}
	filter, err := publicFilter(in.Filter)
	if err != nil {
		return page, err
	}
	sort := PaymentSort{By: SortByCreated, Desc: true}
	if in.Sort != nil {
		sort = *in.Sort
		switch sort.By {
		case "":
			sort.By = SortByCreated
		case SortByCreated, SortByAmount:
		default:
			return page, invalidField("sort", "invalid", "sort must be %q or %q", SortByCreated, SortByAmount)
		}
	}
	if in.Page.Offset < 0 {
		return page, invalidField("offset", "invalid", "offset must not be negative")
	}
	in.Page.Limit = clampLimit(in.Page.Limit, 50, 500)
	err = s.store.View(ctx, func(r Reader) error {
		vc, err := loadViewContext(ctx, r)
		if err != nil {
			return err
		}
		if page.Total, err = r.CountPayments(ctx, filter); err != nil {
			return err
		}
		items, err := r.ListPayments(ctx, filter, sort, in.Page)
		if err != nil {
			return err
		}
		page.Items = make([]PaymentView, 0, len(items))
		for _, p := range items {
			page.Items = append(page.Items, vc.payment(p))
		}
		return nil
	})
	return page, err
}

// publicFilter sanitises a caller-supplied filter.
func publicFilter(f PaymentFilter) (PaymentFilter, error) {
	f.Search = strings.TrimSpace(f.Search)
	f.ExpiresBefore, f.WatchedAfter, f.WatchEndsBy, f.Retired = time.Time{}, time.Time{}, time.Time{}, nil
	if len(f.Search) > 200 {
		return f, invalidField("search", "too_long", "search must be at most 200 characters")
	}
	for _, st := range f.Statuses {
		if !st.Valid() {
			return f, invalidField("status", "invalid", "unknown status %q", st)
		}
	}
	return f, nil
}

// PaymentCounts is the number of payments in each [PaymentGroup].
type PaymentCounts struct {
	All       int
	Open      int
	Paid      int
	Attention int
	Expired   int
}

// CountPayments returns, for the given search and date filter, how many
// payments fall into each dashboard group. filter.Statuses and
// filter.NeedsAttention are ignored. Requires [ScopePaymentsRead].
func (s *Service) CountPayments(ctx context.Context, filter PaymentFilter) (PaymentCounts, error) {
	var c PaymentCounts
	if _, err := authorize(ctx, permScope(ScopePaymentsRead)); err != nil {
		return c, err
	}
	filter, err := publicFilter(filter)
	if err != nil {
		return c, err
	}
	err = s.store.View(ctx, func(r Reader) error {
		for _, g := range []struct {
			group PaymentGroup
			n     *int
		}{{GroupAll, &c.All}, {GroupOpen, &c.Open}, {GroupPaid, &c.Paid}, {GroupAttention, &c.Attention}, {GroupExpired, &c.Expired}} {
			n, err := r.CountPayments(ctx, g.group.Apply(filter))
			if err != nil {
				return err
			}
			*g.n = n
		}
		return nil
	})
	return c, err
}

// ResolvePayment marks a payment's issue (underpaid, overpaid, failed or
// late funds) as dealt with, recording who did it and an optional note
// (≤ 1000 characters). The status does not change. It fails with
// [ErrNothingToResolve] when the payment has no open issue. Operator only.
func (s *Service) ResolvePayment(ctx context.Context, id, note string) (PaymentView, error) {
	return s.setResolution(ctx, id, note, true)
}

// ReopenPayment clears a resolution, putting the payment back under
// "needs attention". It fails with [ErrNothingToResolve] when the payment
// is not resolved. Operator only.
func (s *Service) ReopenPayment(ctx context.Context, id string) (PaymentView, error) {
	return s.setResolution(ctx, id, "", false)
}

func (s *Service) setResolution(ctx context.Context, id, note string, resolve bool) (PaymentView, error) {
	var view PaymentView
	actor, err := authorize(ctx, permOperator)
	if err != nil {
		return view, err
	}
	var v validation
	note = cleanMultiline(&v, "note", note, 1000, false)
	if err := v.errOrNil(); err != nil {
		return view, err
	}
	err = s.update(ctx, func(u *unit) error {
		p, err := getPayment(ctx, u, id)
		if err != nil {
			return err
		}
		event := EventPaymentReopened
		switch {
		case resolve && p.NeedsAttention():
			p.Resolution = &Resolution{At: u.now, By: actor.Name, Note: note}
			// Whoever resolves the issue has seen everything that is
			// confirmed on the payment by now: late funds are counted
			// afresh from here (see Evaluate, rule 3).
			p.LateFundsMark = p.AmountConfirmed
			event = EventPaymentResolved
		case !resolve && p.Resolution != nil:
			p.Resolution = nil
		default:
			return newError(ErrNothingToResolve, "")
		}
		p.UpdatedAt = u.now
		if err := u.UpdatePayment(ctx, p); err != nil {
			return err
		}
		if err := u.emitPayment(ctx, event, p); err != nil {
			return err
		}
		vc, err := loadViewContext(ctx, u)
		if err != nil {
			return err
		}
		view = vc.payment(p)
		return nil
	})
	return view, err
}

// CancelPayment closes a payment that has not been paid: the merchant's
// customer abandoned the order, or the invoice was issued by mistake.
// Requires [ScopePaymentsWrite].
//
// Cancelling is closing the payment window early, so the payment settles
// as expired — with [Payment.CancelledAt] and CancelledBy set — and
// payment.expired is emitted (its data carries cancelled_at). There is no
// separate status or event: a cancelled invoice needs exactly the handling
// of an expired one, existing payment.expired subscriptions keep working,
// and the status vocabulary stays closed.
//
// A payment can be cancelled only while it is awaiting with nothing on
// its address. Once funds have been seen (detected or confirming) it
// fails with [ErrPaymentHasFunds]: money in flight is never cancelled
// under the payer's feet — the payment then runs its course. A payment
// that settled by itself fails with [ErrPaymentSettled]. Cancelling a
// payment that is already cancelled is a no-op that returns it, so the
// call is safe to retry.
//
// Nothing is lost by cancelling. The deposit address stays on the watch
// list for the usual window (an hour, or the long window if funds that
// had been seen were reorganised out), and a transfer that shows up
// afterwards —
// one that was broadcast as the payment was cancelled, or a transaction
// that had been reorganised out and is mined again — is recorded like any
// other late funds: the status stays expired, the amounts are updated,
// the payment needs attention and payment.late_funds is emitted.
func (s *Service) CancelPayment(ctx context.Context, id string) (PaymentView, error) {
	var view PaymentView
	actor, err := authorize(ctx, permScope(ScopePaymentsWrite))
	if err != nil {
		return view, err
	}
	err = s.update(ctx, func(u *unit) error {
		p, err := getPayment(ctx, u, id)
		if err != nil {
			return err
		}
		switch {
		case !p.CancelledAt.IsZero():
			// Already cancelled: nothing to do.
		case p.Status.Settled():
			return newError(ErrPaymentSettled, "this payment is already %s", p.Status)
		case p.Status != StatusAwaiting || p.AmountReceived.Cmp(dustLimit(p.Amount, p.Asset.Decimals)) >= 0:
			// Dust is not "funds": anybody can send it, and it must not
			// take the merchant's cancel button away.
			return newError(ErrPaymentHasFunds, "")
		default:
			p.Status = StatusExpired
			p.SettledAt, p.CancelledAt, p.UpdatedAt = u.now, u.now, u.now
			p.CancelledBy = actor.Name
			// Nothing that counts is live, but funds that were reorganised
			// out may be mined again: then keep the long watch window.
			seen, err := u.ListTransfers(ctx, p.ID)
			if err != nil {
				return err
			}
			var orphaned Amount
			for _, t := range seen {
				if !t.Live() {
					orphaned = orphaned.Add(t.Amount)
				}
			}
			window := s.unfundedWatchWindow
			if p.fundedBy(orphaned) {
				window = s.watchWindow
			}
			head, err := u.loadHead(ctx)
			if err != nil {
				return err
			}
			if head.ChainID != p.Asset.ChainID {
				head = ChainHead{}
			}
			if until := windowStart(u.now, head).Add(window); until.After(p.WatchUntil) {
				p.WatchUntil = until
			}
			if err := u.UpdatePayment(ctx, p); err != nil {
				return err
			}
			if err := u.emitPayment(ctx, EventPaymentExpired, p); err != nil {
				return err
			}
		}
		vc, err := loadViewContext(ctx, u)
		if err != nil {
			return err
		}
		view = vc.payment(p)
		return nil
	})
	return view, err
}

// GetCheckout returns the public view of a payment for the hosted checkout
// page. Public: the unguessable payment id is the capability.
//
// The public checkout surface — GetCheckout, [Service.GetLinkCheckout],
// [Service.CreatePaymentFromLink] and [Service.SubscribePayment] — answers
// about the object it is asked for and nothing else: an id or slug that
// does not exist is a not-found error, also on an instance that has not
// completed setup (where nothing can exist yet). It never reports
// setup_required, so it does not disclose the instance's state.
func (s *Service) GetCheckout(ctx context.Context, paymentID string) (CheckoutView, error) {
	var view CheckoutView
	err := s.store.View(ctx, func(r Reader) error {
		vc, err := loadViewContext(ctx, r)
		if err != nil {
			return err
		}
		p, err := getPayment(ctx, r, paymentID)
		if err != nil {
			return err
		}
		view = NewCheckoutView(p, headFor(vc.head, vc.settings, p), vc.settings.Merchant)
		return nil
	})
	return view, err
}
