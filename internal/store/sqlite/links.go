package sqlite

import (
	"context"

	"github.com/lubluniky/hmnpay/core"
)

const selectPaymentLink = `SELECT id, slug, name, description, amount, asset,
active, max_uses, expires_at, redirect_url, uses, total_collected,
last_paid_at, created_at, updated_at, min_amount FROM links`

func scanPaymentLink(row scanner) (core.PaymentLink, error) {
	var v core.PaymentLink
	err := row.Scan(
		&v.ID, &v.Slug, &v.Name,
		&v.Description, (*amount)(&v.Amount), jsonField{&v.Asset},
		&v.Active, &v.MaxUses, (*timestamp)(&v.ExpiresAt),
		&v.RedirectURL, &v.Uses, (*amount)(&v.TotalCollected),
		(*timestamp)(&v.LastPaidAt), (*timestamp)(&v.CreatedAt), (*timestamp)(&v.UpdatedAt),
		(*amount)(&v.MinAmount),
	)
	return v, mapped(err)
}

func (t transaction) GetLink(ctx context.Context, value string) (core.PaymentLink, error) {
	return scanPaymentLink(t.QueryRowContext(ctx, selectPaymentLink+" WHERE id=?", value))
}

func (t transaction) GetLinkBySlug(ctx context.Context, value string) (core.PaymentLink, error) {
	return scanPaymentLink(t.QueryRowContext(ctx, selectPaymentLink+" WHERE slug=?", value))
}

func (t transaction) ListLinks(ctx context.Context) ([]core.PaymentLink, error) {
	return many(ctx, t, scanPaymentLink, selectPaymentLink+" ORDER BY created_at DESC, id DESC")
}

func paymentLinkValues(v core.PaymentLink) []any {
	return []any{
		v.ID, v.Slug, v.Name,
		v.Description, v.Amount.String(), jsonField{v.Asset},
		v.Active, v.MaxUses, v.ExpiresAt.UnixMilli(),
		v.RedirectURL, v.Uses, v.TotalCollected.String(),
		v.LastPaidAt.UnixMilli(), v.CreatedAt.UnixMilli(), v.UpdatedAt.UnixMilli(),
		v.MinAmount.String(),
	}
}

func (t transaction) InsertLink(ctx context.Context, v core.PaymentLink) error {
	_, err := t.ExecContext(ctx, `INSERT INTO links (id, slug, name, description, amount, asset,
active, max_uses, expires_at, redirect_url, uses, total_collected,
last_paid_at, created_at, updated_at, min_amount)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, paymentLinkValues(v)...)
	return mapped(err)
}

func (t transaction) DeleteLink(ctx context.Context, id string) error {
	return t.replace(ctx, "DELETE FROM links WHERE id=?", id)
}

func (t transaction) UpdateLink(ctx context.Context, v core.PaymentLink) error {
	return t.replace(ctx, `UPDATE links SET
slug=?2, name=?3, description=?4, amount=?5,
asset=?6, active=?7, max_uses=?8, expires_at=?9,
redirect_url=?10, uses=?11, total_collected=?12, last_paid_at=?13,
created_at=?14, updated_at=?15, min_amount=?16
WHERE id=?1`, paymentLinkValues(v)...)
}
