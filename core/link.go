package core

import (
	"regexp"
	"time"
)

// LinkStatus is the derived, displayed state of a payment link.
type LinkStatus string

// Link statuses.
const (
	LinkActive   LinkStatus = "active"
	LinkDisabled LinkStatus = "disabled"
	LinkExpired  LinkStatus = "expired"
	// LinkLimit: the link has collected its maximum number of payments.
	LinkLimit LinkStatus = "limit"
)

// PaymentLink is a reusable hosted link; every customer who opens it gets a
// fresh [Payment].
type PaymentLink struct {
	// ID is "plink_" plus 26 random characters.
	ID string
	// Slug is the public path segment (/l/<slug>). Unique and immutable.
	Slug string
	// Name is the title shown to the customer; it becomes the payment's
	// description.
	Name string
	// Description is an optional longer text for the link page.
	Description string
	// Amount is the fixed price in base units of Asset. Zero means the
	// customer chooses the amount.
	Amount Amount
	// MinAmount is the least a customer may choose to pay, in base units of
	// Asset, on a link without a fixed price. Zero means the default:
	// [LinkMinAmountUnits] whole units. Always zero on a fixed-price link.
	// See [PaymentLink.MinPayment].
	MinAmount Amount
	// Asset is the asset the link was created for. A link cannot take
	// payments while the instance runs a different asset.
	Asset Asset
	// Active is the operator's on/off switch.
	Active bool
	// MaxUses limits the number of paid payments; 0 means unlimited.
	MaxUses int
	// ExpiresAt stops the link at that time; zero means never.
	ExpiresAt time.Time
	// RedirectURL is where the checkout sends the customer after paying.
	RedirectURL string

	// Uses counts payments from this link that settled as paid or
	// overpaid. Maintained by the core.
	Uses int
	// TotalCollected sums the confirmed funds of those payments.
	TotalCollected Amount
	// LastPaidAt is when the most recent one settled; zero if none.
	LastPaidAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Status derives the displayed status at time now. The order is fixed:
// expired, then limit reached, then disabled, then active.
func (l PaymentLink) Status(now time.Time) LinkStatus {
	switch {
	case !l.ExpiresAt.IsZero() && !l.ExpiresAt.After(now):
		return LinkExpired
	case l.MaxUses > 0 && l.Uses >= l.MaxUses:
		return LinkLimit
	case !l.Active:
		return LinkDisabled
	}
	return LinkActive
}

// MinPayment is the least a customer may choose to pay through the link:
// MinAmount, or [LinkMinAmountUnits] whole units when the link sets none. It
// is zero for a fixed-price link, where the customer chooses nothing.
//
// Without a minimum a use of a limited link — "a paid payment", whatever was
// paid — could be taken for one base unit and a network fee, and every such
// payment would emit payment.paid. With it, a use costs a real amount, and
// that amount goes to the merchant.
func (l PaymentLink) MinPayment() Amount {
	switch {
	case !l.Amount.IsZero():
		return Amount{}
	case !l.MinAmount.IsZero():
		return l.MinAmount
	}
	return wholeUnits(LinkMinAmountUnits, l.Asset.Decimals)
}

// slugPattern: lowercase letters, digits and inner hyphens, 1–64 characters.
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// LinkView is the wire representation of a payment link for operators.
type LinkView struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Amount is the fixed price, or null when the customer chooses.
	Amount *string `json:"amount"`
	// MinAmount is the least a customer may choose to pay when the link has
	// no fixed price (the link's own minimum, or the default of one whole
	// unit); null on a fixed-price link.
	MinAmount *string    `json:"min_amount"`
	Asset     string     `json:"asset"`
	Network   string     `json:"network"`
	Active    bool       `json:"active"`
	Status    LinkStatus `json:"status"`
	Uses      int        `json:"uses"`
	// MaxUses is null when unlimited.
	MaxUses        *int       `json:"max_uses"`
	TotalCollected string     `json:"total_collected"`
	ExpiresAt      *time.Time `json:"expires_at"`
	RedirectURL    string     `json:"redirect_url"`
	LastPaidAt     *time.Time `json:"last_paid_at"`
	// URL is <public URL>/l/<slug>.
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewLinkView renders a link for the wire at time now.
func NewLinkView(l PaymentLink, now time.Time, publicURL string) LinkView {
	v := LinkView{
		ID:             l.ID,
		Slug:           l.Slug,
		Name:           l.Name,
		Description:    l.Description,
		Asset:          l.Asset.Symbol,
		Network:        l.Asset.Network,
		Active:         l.Active,
		Status:         l.Status(now),
		Uses:           l.Uses,
		TotalCollected: l.TotalCollected.Format(l.Asset.Decimals),
		ExpiresAt:      optTime(l.ExpiresAt),
		RedirectURL:    l.RedirectURL,
		LastPaidAt:     optTime(l.LastPaidAt),
		URL:            LinkURL(publicURL, l.Slug),
		CreatedAt:      l.CreatedAt,
		UpdatedAt:      l.UpdatedAt,
	}
	if !l.Amount.IsZero() {
		a := l.Amount.Format(l.Asset.Decimals)
		v.Amount = &a
	} else {
		m := l.MinPayment().Format(l.Asset.Decimals)
		v.MinAmount = &m
	}
	if l.MaxUses > 0 {
		n := l.MaxUses
		v.MaxUses = &n
	}
	return v
}

// LinkCheckoutView is what the public link page may know about a link.
type LinkCheckoutView struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Amount is the fixed price, or null when the customer enters it.
	Amount *string `json:"amount"`
	// MinAmount is the least the customer may enter; null on a fixed-price
	// link. An amount below it is refused with a validation error on
	// "amount" (code "too_small").
	MinAmount *string `json:"min_amount"`
	Asset     string  `json:"asset"`
	Network   string  `json:"network"`
	Decimals  int     `json:"decimals"`
	// Status says whether the link takes payments ("active") or why not.
	Status   LinkStatus   `json:"status"`
	Merchant MerchantView `json:"merchant"`
}
