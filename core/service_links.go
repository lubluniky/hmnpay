package core

import (
	"context"
	"errors"
	"strings"
	"time"
)

// LinkInput describes a payment link, for both creation and update
// (updates replace every editable field).
type LinkInput struct {
	// Slug is the public path segment: 1–64 lowercase letters, digits and
	// inner hyphens. On creation an empty slug gets a random one; on
	// update the slug is ignored — it is immutable, because the link has
	// been shared.
	Slug string
	// Name is the title shown to customers (≤ 80 characters). Required.
	Name string
	// Description is an optional longer text (≤ 500 characters).
	Description string
	// Amount is the fixed price as a decimal string; empty means the
	// customer chooses the amount.
	Amount string
	// MinAmount is the least a customer may choose to pay, as a decimal
	// string, on a link without a fixed price. Empty means the default,
	// [LinkMinAmountUnits] whole units (1 USDT), and nothing below that is
	// accepted. It must be empty on a fixed-price link.
	MinAmount string
	// MaxUses limits the number of paid payments; 0 means unlimited.
	MaxUses int
	// ExpiresAt stops the link at that time; zero means never.
	ExpiresAt time.Time
	// RedirectURL is where the checkout sends the customer after paying.
	RedirectURL string
	// Active is the on/off switch.
	Active bool
}

func (in LinkInput) apply(l *PaymentLink, asset Asset) error {
	var v validation
	l.Name = cleanText(&v, "name", in.Name, 80, true)
	l.Description = cleanMultiline(&v, "description", in.Description, 500, false)
	l.RedirectURL = cleanWebURL(&v, "redirect_url", in.RedirectURL, false)
	l.Amount = Amount{}
	if strings.TrimSpace(in.Amount) != "" {
		l.Amount = parsePaymentAmount(&v, "amount", in.Amount, asset)
	}
	l.MinAmount = Amount{}
	if strings.TrimSpace(in.MinAmount) != "" {
		var mv validation
		floor := wholeUnits(LinkMinAmountUnits, asset.Decimals)
		switch m := parsePaymentAmount(&mv, "min_amount", in.MinAmount, asset); {
		case !mv.ok():
			v.fields = append(v.fields, mv.fields...)
		case !l.Amount.IsZero():
			v.add("min_amount", "invalid", "a minimum amount applies only to a link where the customer chooses the amount")
		case m.Cmp(floor) < 0:
			v.add("min_amount", "too_small", "the minimum amount must be at least %s %s", floor.Format(asset.Decimals), asset.Symbol)
		default:
			l.MinAmount = m
		}
	}
	if in.MaxUses < 0 {
		v.add("max_uses", "invalid", "max uses must not be negative")
	}
	l.MaxUses = in.MaxUses
	l.ExpiresAt = time.Time{}
	if !in.ExpiresAt.IsZero() {
		l.ExpiresAt = in.ExpiresAt.UTC().Truncate(time.Millisecond)
	}
	l.Active = in.Active
	l.Asset = asset
	return v.errOrNil()
}

func getLink(ctx context.Context, r LinkReader, id string) (PaymentLink, error) {
	l, err := r.GetLink(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return l, notFound("payment_link", id)
	}
	return l, err
}

// CreateLink creates a payment link. A slug that is already taken fails
// with a validation error on "slug" (code "taken"). Requires
// [ScopeLinksWrite].
func (s *Service) CreateLink(ctx context.Context, in LinkInput) (LinkView, error) {
	var view LinkView
	if _, err := authorize(ctx, permScope(ScopeLinksWrite)); err != nil {
		return view, err
	}
	err := s.update(ctx, func(u *unit) error {
		st, err := requireSetup(ctx, u)
		if err != nil {
			return err
		}
		var l PaymentLink
		if err := in.apply(&l, st.Asset); err != nil {
			return err
		}
		l.Slug = strings.TrimSpace(in.Slug)
		if l.Slug == "" {
			r, err := s.randomString(5)
			if err != nil {
				return err
			}
			l.Slug = r
		} else if !slugPattern.MatchString(l.Slug) {
			return invalidField("slug", "invalid", "slug must be 1 to 64 lowercase letters, digits or hyphens, and must not start or end with a hyphen")
		}
		switch _, err := u.GetLinkBySlug(ctx, l.Slug); {
		case err == nil:
			return invalidField("slug", "taken", "this slug is already in use")
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if l.ID, err = s.newID(prefixLink); err != nil {
			return err
		}
		l.CreatedAt, l.UpdatedAt = u.now, u.now
		if err := u.InsertLink(ctx, l); err != nil {
			return err
		}
		view = NewLinkView(l, u.now, st.PublicURL)
		return nil
	})
	return view, err
}

// UpdateLink replaces the editable fields of a link (everything in
// [LinkInput] except the slug). Statistics are untouched. Requires
// [ScopeLinksWrite].
func (s *Service) UpdateLink(ctx context.Context, id string, in LinkInput) (LinkView, error) {
	var view LinkView
	if _, err := authorize(ctx, permScope(ScopeLinksWrite)); err != nil {
		return view, err
	}
	err := s.update(ctx, func(u *unit) error {
		st, err := requireSetup(ctx, u)
		if err != nil {
			return err
		}
		l, err := getLink(ctx, u, id)
		if err != nil {
			return err
		}
		if err := in.apply(&l, st.Asset); err != nil {
			return err
		}
		l.UpdatedAt = u.now
		if err := u.UpdateLink(ctx, l); err != nil {
			return err
		}
		view = NewLinkView(l, u.now, st.PublicURL)
		return nil
	})
	return view, err
}

// SetLinkActive switches a link on or off without touching anything else.
// Requires [ScopeLinksWrite].
func (s *Service) SetLinkActive(ctx context.Context, id string, active bool) (LinkView, error) {
	var view LinkView
	if _, err := authorize(ctx, permScope(ScopeLinksWrite)); err != nil {
		return view, err
	}
	err := s.update(ctx, func(u *unit) error {
		st, err := u.loadSettings(ctx)
		if err != nil {
			return err
		}
		l, err := getLink(ctx, u, id)
		if err != nil {
			return err
		}
		l.Active = active
		l.UpdatedAt = u.now
		if err := u.UpdateLink(ctx, l); err != nil {
			return err
		}
		view = NewLinkView(l, u.now, st.PublicURL)
		return nil
	})
	return view, err
}

// DeleteLink removes a payment link for good. Its public page is gone at
// once and its slug can be used again. Payments created through it are
// kept untouched — they keep their LinkID (which then refers to nothing),
// stay payable while open, and settle normally; only the link's own
// statistics are gone. To stop a link while keeping it, use
// [Service.SetLinkActive]. Requires [ScopeLinksWrite].
func (s *Service) DeleteLink(ctx context.Context, id string) error {
	if _, err := authorize(ctx, permScope(ScopeLinksWrite)); err != nil {
		return err
	}
	return s.update(ctx, func(u *unit) error {
		err := u.DeleteLink(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return notFound("payment_link", id)
		}
		return err
	})
}

// GetLink returns one payment link. Requires [ScopeLinksWrite].
func (s *Service) GetLink(ctx context.Context, id string) (LinkView, error) {
	var view LinkView
	if _, err := authorize(ctx, permScope(ScopeLinksWrite)); err != nil {
		return view, err
	}
	now := s.now()
	err := s.store.View(ctx, func(r Reader) error {
		st, err := loadSettings(ctx, r)
		if err != nil {
			return err
		}
		l, err := getLink(ctx, r, id)
		if err != nil {
			return err
		}
		view = NewLinkView(l, now, st.PublicURL)
		return nil
	})
	return view, err
}

// ListLinks returns all payment links, newest first. Requires
// [ScopeLinksWrite].
func (s *Service) ListLinks(ctx context.Context) ([]LinkView, error) {
	if _, err := authorize(ctx, permScope(ScopeLinksWrite)); err != nil {
		return nil, err
	}
	now := s.now()
	var views []LinkView
	err := s.store.View(ctx, func(r Reader) error {
		st, err := loadSettings(ctx, r)
		if err != nil {
			return err
		}
		links, err := r.ListLinks(ctx)
		if err != nil {
			return err
		}
		views = make([]LinkView, 0, len(links))
		for _, l := range links {
			views = append(views, NewLinkView(l, now, st.PublicURL))
		}
		return nil
	})
	return views, err
}

// linkAvailability reports why a link cannot take a new payment right now,
// or nil. Unlike [PaymentLink.Status] it also counts the checkouts that
// hold one of a limited link's uses. The rule is one sentence: a checkout
// holds a use for exactly as long as it can still become a paid use.
//
//   - Before its deadline every open checkout holds one, funded or not —
//     it can still be paid in time. That is why checkouts of a limited link
//     are short-lived (see limitedLinkTTL).
//   - After its deadline only a checkout that is confirming holds one: the
//     full amount arrived in time and is waiting for confirmations. Any
//     other checkout can no longer be paid — funds that arrive after the
//     deadline are late funds ([Evaluate]) — so its use is free again, even
//     if dust or a partial payment still keeps the payment itself open.
//
// "After its deadline" takes both clocks, exactly as settling a payment
// does: the block time of the processed head and the wall clock. Chain time
// matters here: when it has passed the deadline, every transfer made in
// time has already been reported, so releasing the use cannot race with a
// payment that is on its way. The price is that a limited link does not
// free uses while the chain watcher is not processing heads — and during
// such an outage no payment could be detected anyway.
//
// Consequence: Uses + (checkouts holding a use) never exceeds MaxUses, and
// every payment that ends as paid held a use from its creation until it
// was counted. A limited link is therefore never oversold. Residual
// exposure: someone who keeps opening MaxUses fresh checkouts every
// lifetime can keep a limited link at "limit reached" without paying —
// transports cap that per client, see the HTTP adapter.
func (s *Service) linkAvailability(ctx context.Context, r PaymentReader, l PaymentLink, st Settings, head ChainHead, now time.Time) error {
	switch l.Status(now) {
	case LinkExpired:
		return newError(ErrLinkExpired, "")
	case LinkLimit:
		return newError(ErrLinkLimitReached, "")
	case LinkDisabled:
		return newError(ErrLinkDisabled, "")
	}
	if l.Asset != st.Asset {
		return conflict("link_asset_mismatch", "this payment link was created for another asset")
	}
	if l.MaxUses > 0 {
		held, err := r.CountPayments(ctx, PaymentFilter{LinkID: l.ID, Statuses: OpenStatuses})
		if err != nil {
			return err
		}
		if head.ChainID == l.Asset.ChainID && !head.Time.IsZero() {
			over, err := r.CountPayments(ctx, PaymentFilter{
				LinkID: l.ID, Statuses: []PaymentStatus{StatusAwaiting, StatusDetected},
				ExpiresBefore: earlier(head.Time, now).Add(-s.grace),
			})
			if err != nil {
				return err
			}
			held -= over
		}
		if l.Uses+held >= l.MaxUses {
			return newError(ErrLinkLimitReached, "")
		}
	}
	return nil
}

// limitedLinkTTL is the lifetime of a checkout opened through a link with
// MaxUses: [Config.LinkReservation], but never longer than the instance's
// payment TTL nor shorter than [MinPaymentTTL]. Such a checkout holds one
// of the link's uses for its whole life, so its life is short: the customer
// has this long to pay, then the use is released and the checkout closed.
func (s *Service) limitedLinkTTL(st Settings) time.Duration {
	ttl := st.Payments.TTL
	if s.linkReservation > 0 && s.linkReservation < ttl {
		ttl = s.linkReservation
	}
	return max(ttl, MinPaymentTTL)
}

// watchedCount returns the size of the chain watch list: open payments of
// the asset plus settled ones whose address has not been retired (the list
// the chain watcher is given, ChainState). With fromLink set it counts only
// the payments created through a public link (true) or only those the
// merchant created (false).
func watchedCount(ctx context.Context, r PaymentReader, asset Asset, fromLink *bool) (int, error) {
	open, err := r.CountPayments(ctx, PaymentFilter{Statuses: OpenStatuses, ChainID: asset.ChainID, Contract: asset.Contract, FromLink: fromLink})
	if err != nil {
		return 0, err
	}
	notRetired := false
	settled, err := r.CountPayments(ctx, PaymentFilter{Statuses: settledStatuses, ChainID: asset.ChainID, Contract: asset.Contract, FromLink: fromLink, Retired: &notRetired})
	return open + settled, err
}

// GetLinkCheckout returns the public view of a payment link by slug, for
// the hosted link page. Status is "active" only if the link can take a
// payment right now. An unknown slug is a not-found error, whether or not
// the instance is set up (see [Service.GetCheckout]). Public.
func (s *Service) GetLinkCheckout(ctx context.Context, slug string) (LinkCheckoutView, error) {
	var view LinkCheckoutView
	now := s.now()
	err := s.store.View(ctx, func(r Reader) error {
		st, err := loadSettings(ctx, r)
		if err != nil {
			return err
		}
		l, err := r.GetLinkBySlug(ctx, slug)
		if errors.Is(err, ErrNotFound) {
			return notFound("payment_link", slug)
		}
		if err != nil {
			return err
		}
		view = LinkCheckoutView{
			Slug:        l.Slug,
			Name:        l.Name,
			Description: l.Description,
			Asset:       l.Asset.Symbol,
			Network:     l.Asset.Network,
			Decimals:    l.Asset.Decimals,
			Status:      l.Status(now),
			Merchant:    merchantView(st.Merchant),
		}
		if !l.Amount.IsZero() {
			a := l.Amount.Format(l.Asset.Decimals)
			view.Amount = &a
		} else {
			m := l.MinPayment().Format(l.Asset.Decimals)
			view.MinAmount = &m
		}
		if view.Status == LinkActive {
			head, err := loadHead(ctx, r)
			if err != nil {
				return err
			}
			switch err := s.linkAvailability(ctx, r, l, st, head, now); {
			case errors.Is(err, ErrLinkLimitReached):
				view.Status = LinkLimit
			case KindOf(err) == KindConflict:
				view.Status = LinkDisabled
			case err != nil:
				return err
			}
		}
		return nil
	})
	return view, err
}

// LinkPaymentInput is what a customer supplies when paying through a link.
type LinkPaymentInput struct {
	// Amount is required for customer-chooses links, where it must be at
	// least the link's minimum ([PaymentLink.MinPayment]: one whole unit
	// unless the link sets more), and must be empty (or equal to the price)
	// for fixed-price links.
	Amount string
	// CustomerEmail is optional.
	CustomerEmail string
	// IdempotencyKey deduplicates per link: the checkout page should
	// generate one per visit so a reload or double click returns the same
	// payment. Optional.
	IdempotencyKey string
}

// A link with MaxUses hands out at most MaxUses − Uses open checkouts at a
// time, and each lives only for [Config.LinkReservation] (10 minutes by
// default) instead of the instance's payment TTL: the customer has that
// long to pay, then the use is released and the checkout expires. Funds
// sent after that are late funds on an expired payment, never a use of the
// link, so a limited link is not oversold (the rule is documented on
// linkAvailability in the source and in the package documentation,
// "Payment links"). A link without MaxUses uses the normal payment TTL.
//
// When the public links' share of the chain watch list is used up
// ([Config.MaxWatchedAddresses]) it fails with [ErrWatchListFull] — only
// this method does, because it is the only one that lets anonymous callers
// add to the list.
//
// CreatePaymentFromLink creates a payment for a customer who opened a
// payment link, and returns its public checkout view together with
// whether it was newly created (false when the idempotency key returned
// the payment of an earlier call), like [Service.CreatePayment]. It fails
// with a not-found error for an unknown slug, with [ErrLinkDisabled],
// [ErrLinkExpired] or [ErrLinkLimitReached] when the link cannot take a
// payment, and with a validation error on "amount" (code "too_small",
// "the minimum amount is 1.00 USDT") when the customer chose less than the
// link's minimum. Public — transports should rate-limit it, since every new
// payment allocates a deposit address.
func (s *Service) CreatePaymentFromLink(ctx context.Context, slug string, in LinkPaymentInput) (CheckoutView, bool, error) {
	var view CheckoutView
	var created bool
	err := s.update(ctx, func(u *unit) error {
		l, err := u.GetLinkBySlug(ctx, slug)
		if errors.Is(err, ErrNotFound) {
			return notFound("payment_link", slug)
		}
		if err != nil {
			return err
		}
		// A link cannot exist before setup; this only guards the invariant.
		st, err := requireSetup(ctx, u)
		if err != nil {
			return err
		}
		amount := strings.TrimSpace(in.Amount)
		fixed := !l.Amount.IsZero()
		if fixed {
			if amount != "" {
				if a, err := ParseAmount(amount, l.Asset.Decimals); err != nil || a != l.Amount {
					return invalidField("amount", "invalid", "this link has a fixed price")
				}
			}
			amount = l.Amount.Format(l.Asset.Decimals)
		}
		create := CreatePaymentInput{
			Amount:        amount,
			Description:   l.Name,
			CustomerEmail: in.CustomerEmail,
			RedirectURL:   l.RedirectURL,
		}
		if in.IdempotencyKey != "" {
			create.IdempotencyKey = "link:" + l.ID + ":" + in.IdempotencyKey
		}
		params, err := validatePaymentInput(create, st)
		if err != nil {
			return err
		}
		params.linkID = l.ID
		if l.MaxUses > 0 {
			params.ttl = s.limitedLinkTTL(st)
		}
		// An idempotent retry must get its payment back even if the link
		// filled up or closed in the meantime.
		replay := false
		if params.idempotencyKey != "" {
			switch _, err := u.GetPaymentByIdempotencyKey(ctx, params.idempotencyKey); {
			case err == nil:
				replay = true
			case !errors.Is(err, ErrNotFound):
				return err
			}
		}
		if !replay {
			head, err := u.loadHead(ctx)
			if err != nil {
				return err
			}
			if err := s.linkAvailability(ctx, u, l, st, head, u.now); err != nil {
				return err
			}
			// The customer chooses the amount, not whether it is one: see
			// PaymentLink.MinPayment.
			if least := l.MinPayment(); !fixed && params.amount.Cmp(least) < 0 {
				return invalidField("amount", "too_small", "the minimum amount is %s %s", least.Format(l.Asset.Decimals), l.Asset.Symbol)
			}
			// This is the one unauthenticated way to create a payment, and
			// every payment joins the chain watch list: refuse here, loudly,
			// rather than let the list grow without bound or silently stop
			// watching. Only link payments count against the cap, so
			// nothing the merchant creates can use up the links' share and
			// nothing done to the links can reach the merchant's payments.
			if s.maxWatched > 0 {
				fromLink := true
				n, err := watchedCount(ctx, u, st.Asset, &fromLink)
				if err != nil {
					return err
				}
				if n >= s.maxWatched {
					return newError(ErrWatchListFull, "")
				}
			}
		}
		p, isNew, err := s.createPayment(ctx, u, st, params)
		if err != nil {
			return err
		}
		head, err := u.loadHead(ctx)
		if err != nil {
			return err
		}
		view, created = NewCheckoutView(p, headFor(head, st, p), st.Merchant), isNew
		return nil
	})
	return view, created, err
}
