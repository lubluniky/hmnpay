package core

import (
	"fmt"
	"strings"
	"time"
)

// PaymentView is the wire representation of a payment: the one shape every
// transport speaks. The REST API returns it, webhook and event payloads
// embed it as "data", and bots or embedding programs get it from the
// service. Amounts are decimal strings in whole units of the asset.
type PaymentView struct {
	ID     string        `json:"id"`
	Status PaymentStatus `json:"status"`

	OrderID     string `json:"order_id"`
	Description string `json:"description"`

	// Amount is the amount due in total; AmountReceived is everything seen
	// on-chain so far (confirmed or not); AmountConfirmed is the final part
	// of it; AmountDue is what is still missing (never negative).
	Amount          string `json:"amount"`
	AmountReceived  string `json:"amount_received"`
	AmountConfirmed string `json:"amount_confirmed"`
	AmountDue       string `json:"amount_due"`

	Asset    string `json:"asset"`
	Network  string `json:"network"`
	ChainID  uint64 `json:"chain_id"`
	Decimals int    `json:"decimals"`

	// Address is the deposit address (lowercase hex).
	Address string `json:"address"`
	// TxHash and FromAddress describe the earliest live transfer; null
	// until one is seen.
	TxHash      *string `json:"tx_hash"`
	FromAddress *string `json:"from_address"`

	Confirmations         int `json:"confirmations"`
	RequiredConfirmations int `json:"required_confirmations"`
	// AwaitingFinality is set while some of the payment's money has the
	// confirmations it needs but is not confirmed yet, because the chain
	// has not finalised its block: the status stays "confirming" (or
	// "detected") until it does. Null otherwise.
	AwaitingFinality *FinalityWaitView `json:"awaiting_finality"`

	CustomerEmail string            `json:"customer_email"`
	RedirectURL   string            `json:"redirect_url"`
	Metadata      map[string]string `json:"metadata"`
	LinkID        *string           `json:"link_id"`
	// CheckoutURL is the hosted checkout page: <public URL>/pay/<id>.
	CheckoutURL string `json:"checkout_url"`

	// NeedsAttention is true while an issue is open; Attention says which
	// ("underpaid", "overpaid", "failed", "late_funds", or "" for none).
	NeedsAttention bool            `json:"needs_attention"`
	Attention      Attention       `json:"attention"`
	Resolution     *ResolutionView `json:"resolution"`

	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	PaidAt    *time.Time `json:"paid_at"`
	SettledAt *time.Time `json:"settled_at"`
	// CancelledAt is set when the payment was cancelled; its status is
	// then "expired". CancelledBy names who did it.
	CancelledAt *time.Time `json:"cancelled_at"`
	CancelledBy string     `json:"cancelled_by"`
}

// FinalityWaitView says which block a payment waits for the chain to
// finalise ([PaymentView.AwaitingFinality]).
type FinalityWaitView struct {
	// Block is the next block to be finalised for the payment: that of its
	// oldest pending transfer.
	Block uint64 `json:"block"`
	// Final is the newest block that is final so far ([ChainHead.Final]).
	Final uint64 `json:"final"`
}

// awaitingFinality returns the view of p's wait for finality under head, or
// nil if it does not wait for it: nothing is pending, or no pending transfer
// has its confirmations yet. It looks at the oldest pending transfer, the
// one with the most confirmations: whenever any of the payment's money has
// its confirmations and is not final, that one has them too, and is not
// final either (a pending transfer with its confirmations at or below the
// final block is confirmed by the head that gives them).
func awaitingFinality(p Payment, head ChainHead) *FinalityWaitView {
	block := p.OldestPendingBlock
	if block == 0 {
		block = p.PendingBlock // recorded before OldestPendingBlock was
	}
	if block == 0 || !head.awaitingFinality(block, p.RequiredConfirmations) {
		return nil
	}
	return &FinalityWaitView{Block: block, Final: head.Final}
}

// ResolutionView is the wire form of a [Resolution].
type ResolutionView struct {
	ResolvedAt time.Time `json:"resolved_at"`
	By         string    `json:"by"`
	Note       string    `json:"note"`
}

// NewPaymentView renders a payment for the wire. head is the current chain
// head of the payment's asset, or the zero head (for the confirmation count
// and the wait for finality), and publicURL the instance's public base URL
// (for CheckoutURL).
func NewPaymentView(p Payment, head ChainHead, publicURL string) PaymentView {
	d := p.Asset.Decimals
	v := PaymentView{
		ID:                    p.ID,
		Status:                p.Status,
		OrderID:               p.OrderID,
		Description:           p.Description,
		Amount:                p.Amount.Format(d),
		AmountReceived:        p.AmountReceived.Format(d),
		AmountConfirmed:       p.AmountConfirmed.Format(d),
		AmountDue:             p.AmountDue().Format(d),
		Asset:                 p.Asset.Symbol,
		Network:               p.Asset.Network,
		ChainID:               p.Asset.ChainID,
		Decimals:              d,
		Address:               p.Address,
		TxHash:                optString(p.TxHash),
		FromAddress:           optString(p.FromAddress),
		Confirmations:         p.Confirmations(head.Number),
		RequiredConfirmations: p.RequiredConfirmations,
		AwaitingFinality:      awaitingFinality(p, head),
		CustomerEmail:         p.CustomerEmail,
		RedirectURL:           p.RedirectURL,
		Metadata:              map[string]string{},
		LinkID:                optString(p.LinkID),
		CheckoutURL:           CheckoutURL(publicURL, p.ID),
		NeedsAttention:        p.NeedsAttention(),
		Attention:             p.Attention,
		CreatedAt:             p.CreatedAt,
		ExpiresAt:             p.ExpiresAt,
		UpdatedAt:             p.UpdatedAt,
		PaidAt:                optTime(p.PaidAt),
		SettledAt:             optTime(p.SettledAt),
		CancelledAt:           optTime(p.CancelledAt),
		CancelledBy:           p.CancelledBy,
	}
	for k, val := range p.Metadata {
		v.Metadata[k] = val
	}
	if p.Resolution != nil {
		v.Resolution = &ResolutionView{ResolvedAt: p.Resolution.At, By: p.Resolution.By, Note: p.Resolution.Note}
	}
	return v
}

// CheckoutURL returns the hosted checkout URL of a payment:
// <publicURL>/pay/<id>. With an empty publicURL the result is a path.
func CheckoutURL(publicURL, paymentID string) string {
	return strings.TrimRight(publicURL, "/") + "/pay/" + paymentID
}

// LinkURL returns the public URL of a payment link: <publicURL>/l/<slug>.
func LinkURL(publicURL, slug string) string {
	return strings.TrimRight(publicURL, "/") + "/l/" + slug
}

// TransferView is the wire representation of a [Transfer].
type TransferView struct {
	TxHash        string         `json:"tx_hash"`
	LogIndex      uint           `json:"log_index"`
	BlockNumber   uint64         `json:"block_number"`
	BlockHash     string         `json:"block_hash"`
	BlockTime     *time.Time     `json:"block_time"`
	From          string         `json:"from"`
	To            string         `json:"to"`
	Amount        string         `json:"amount"`
	Status        TransferStatus `json:"status"`
	Confirmations int            `json:"confirmations"`
	// AwaitingFinality is true while the transfer is pending with the
	// confirmations it needs: the chain has not finalised its block yet.
	AwaitingFinality bool       `json:"awaiting_finality"`
	SeenAt           time.Time  `json:"seen_at"`
	ConfirmedAt      *time.Time `json:"confirmed_at"`
	OrphanedAt       *time.Time `json:"orphaned_at"`
}

// NewTransferView renders a transfer of payment p for the wire, under the
// head of p's asset (or the zero head).
func NewTransferView(t Transfer, p Payment, head ChainHead) TransferView {
	v := TransferView{
		TxHash:      t.TxHash,
		LogIndex:    t.LogIndex,
		BlockNumber: t.BlockNumber,
		BlockHash:   t.BlockHash,
		BlockTime:   optTime(t.BlockTime),
		From:        t.From,
		To:          t.To,
		Amount:      t.Amount.Format(p.Asset.Decimals),
		Status:      t.Status,
		SeenAt:      t.SeenAt,
		ConfirmedAt: optTime(t.ConfirmedAt),
		OrphanedAt:  optTime(t.OrphanedAt),
	}
	switch t.Status {
	case TransferConfirmed:
		v.Confirmations = p.RequiredConfirmations
	case TransferPending:
		v.Confirmations = confirmations(t.BlockNumber, head.Number, p.RequiredConfirmations)
		v.AwaitingFinality = head.awaitingFinality(t.BlockNumber, p.RequiredConfirmations)
	}
	return v
}

// MerchantView is the merchant identity shown on public pages.
type MerchantView struct {
	Name         string `json:"name"`
	LogoURL      string `json:"logo_url"`
	SupportEmail string `json:"support_email"`
	SupportURL   string `json:"support_url"`
}

// CheckoutView is what the public, unauthenticated checkout page (or a bot
// talking to a payer) may know about a payment. It deliberately omits
// merchant-private data: metadata, customer email, issue and resolution.
type CheckoutView struct {
	ID          string        `json:"id"`
	Status      PaymentStatus `json:"status"`
	OrderID     string        `json:"order_id"`
	Description string        `json:"description"`

	Amount         string `json:"amount"`
	AmountReceived string `json:"amount_received"`
	AmountDue      string `json:"amount_due"`

	Asset    string `json:"asset"`
	Network  string `json:"network"`
	ChainID  uint64 `json:"chain_id"`
	Contract string `json:"contract"`
	Decimals int    `json:"decimals"`

	Address string `json:"address"`
	// PaymentURI is an EIP-681 token-transfer URI for the amount still due:
	// ethereum:<contract>@<chain id>/transfer?address=<address>&uint256=<base units>
	// A wallet that understands it opens with token, recipient and amount
	// filled in. The checkout page of the web interface offers it as an
	// "open in wallet" link; its QR code encodes the bare Address, which
	// every wallet can scan.
	PaymentURI string  `json:"payment_uri"`
	TxHash     *string `json:"tx_hash"`

	Confirmations         int `json:"confirmations"`
	RequiredConfirmations int `json:"required_confirmations"`
	// AwaitingFinality: as in [PaymentView] — the payment has its
	// confirmations and waits for the chain to finalise Block.
	AwaitingFinality *FinalityWaitView `json:"awaiting_finality"`

	RedirectURL string     `json:"redirect_url"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	PaidAt      *time.Time `json:"paid_at"`
	// CancelledAt is set when the merchant cancelled the payment (status
	// "expired").
	CancelledAt *time.Time `json:"cancelled_at"`

	Merchant MerchantView `json:"merchant"`
}

// NewCheckoutView renders the public view of a payment, under the head of
// its asset (or the zero head).
func NewCheckoutView(p Payment, head ChainHead, m MerchantSettings) CheckoutView {
	d := p.Asset.Decimals
	due := p.AmountDue()
	if due.IsZero() {
		due = p.Amount
	}
	return CheckoutView{
		ID:                    p.ID,
		Status:                p.Status,
		OrderID:               p.OrderID,
		Description:           p.Description,
		Amount:                p.Amount.Format(d),
		AmountReceived:        p.AmountReceived.Format(d),
		AmountDue:             p.AmountDue().Format(d),
		Asset:                 p.Asset.Symbol,
		Network:               p.Asset.Network,
		ChainID:               p.Asset.ChainID,
		Contract:              p.Asset.Contract,
		Decimals:              d,
		Address:               p.Address,
		PaymentURI:            fmt.Sprintf("ethereum:%s@%d/transfer?address=%s&uint256=%s", p.Asset.Contract, p.Asset.ChainID, p.Address, due),
		TxHash:                optString(p.TxHash),
		Confirmations:         p.Confirmations(head.Number),
		RequiredConfirmations: p.RequiredConfirmations,
		AwaitingFinality:      awaitingFinality(p, head),
		RedirectURL:           p.RedirectURL,
		CreatedAt:             p.CreatedAt,
		ExpiresAt:             p.ExpiresAt,
		PaidAt:                optTime(p.PaidAt),
		CancelledAt:           optTime(p.CancelledAt),
		Merchant:              merchantView(m),
	}
}

func merchantView(m MerchantSettings) MerchantView {
	return MerchantView{Name: m.Name, LogoURL: m.LogoURL, SupportEmail: m.SupportEmail, SupportURL: m.SupportURL}
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
