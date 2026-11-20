package sqlite

import (
	"context"

	"github.com/lubluniky/hmnpay/core"
)

const selectPayment = `SELECT id, status, amount, asset, address, address_index,
order_id, description, customer_email, redirect_url, metadata, link_id,
idempotency_key, required_confirmations, tolerance, amount_received, amount_confirmed, tx_hash,
from_address, pending_block, created_at_block, attention, resolution, created_at,
expires_at, updated_at, paid_at, settled_at, watch_until, cancelled_at,
cancelled_by, late_funds_mark, retired_at, retired_unchecked, oldest_pending_block FROM payments`

func scanPayment(row scanner) (core.Payment, error) {
	var v core.Payment
	err := row.Scan(
		&v.ID, &v.Status, (*amount)(&v.Amount),
		jsonField{&v.Asset}, &v.Address, &v.AddressIndex,
		&v.OrderID, &v.Description, &v.CustomerEmail,
		&v.RedirectURL, jsonField{&v.Metadata}, &v.LinkID,
		&v.IdempotencyKey, &v.RequiredConfirmations, (*amount)(&v.Tolerance),
		(*amount)(&v.AmountReceived), (*amount)(&v.AmountConfirmed), &v.TxHash,
		&v.FromAddress, &v.PendingBlock, &v.CreatedAtBlock,
		&v.Attention, jsonField{&v.Resolution}, (*timestamp)(&v.CreatedAt),
		(*timestamp)(&v.ExpiresAt), (*timestamp)(&v.UpdatedAt), (*timestamp)(&v.PaidAt),
		(*timestamp)(&v.SettledAt), (*timestamp)(&v.WatchUntil), (*timestamp)(&v.CancelledAt),
		&v.CancelledBy, (*amount)(&v.LateFundsMark), (*timestamp)(&v.RetiredAt), &v.RetiredUnchecked, &v.OldestPendingBlock,
	)
	if v.Metadata == nil {
		v.Metadata = map[string]string{}
	}
	if v.Resolution != nil {
		v.Resolution.At = v.Resolution.At.UTC()
	}
	return v, mapped(err)
}

func (t transaction) GetPayment(ctx context.Context, value string) (core.Payment, error) {
	return scanPayment(t.QueryRowContext(ctx, selectPayment+" WHERE id=?", value))
}

func (t transaction) GetPaymentByAddress(ctx context.Context, value string) (core.Payment, error) {
	return scanPayment(t.QueryRowContext(ctx, selectPayment+" WHERE address=?", value))
}

func (t transaction) GetPaymentByIdempotencyKey(ctx context.Context, value string) (core.Payment, error) {
	return scanPayment(t.QueryRowContext(ctx, selectPayment+" WHERE idempotency_key=? AND idempotency_key <> ''", value))
}

func paymentValues(v core.Payment) []any {
	return []any{
		v.ID, v.Status, v.Amount.String(),
		jsonField{v.Asset}, v.Address, v.AddressIndex,
		v.OrderID, v.Description, v.CustomerEmail,
		v.RedirectURL, jsonField{v.Metadata}, v.LinkID,
		v.IdempotencyKey, v.RequiredConfirmations, v.Tolerance.String(),
		v.AmountReceived.String(), v.AmountConfirmed.String(), v.TxHash,
		v.FromAddress, unsigned(v.PendingBlock), unsigned(v.CreatedAtBlock),
		v.Attention, jsonField{v.Resolution}, v.CreatedAt.UnixMilli(),
		v.ExpiresAt.UnixMilli(), v.UpdatedAt.UnixMilli(), v.PaidAt.UnixMilli(),
		v.SettledAt.UnixMilli(), v.WatchUntil.UnixMilli(), v.Amount.SortKey(),
		unsigned(v.Asset.ChainID), v.NeedsAttention(), v.CancelledAt.UnixMilli(),
		v.CancelledBy, v.LateFundsMark.String(), v.RetiredAt.UnixMilli(), v.RetiredUnchecked,
		v.Asset.Contract, unsigned(v.OldestPendingBlock),
	}
}

func (t transaction) InsertPayment(ctx context.Context, v core.Payment) error {
	_, err := t.ExecContext(ctx, `INSERT INTO payments (id, status, amount, asset, address, address_index,
order_id, description, customer_email, redirect_url, metadata, link_id,
idempotency_key, required_confirmations, tolerance, amount_received, amount_confirmed, tx_hash,
from_address, pending_block, created_at_block, attention, resolution, created_at,
expires_at, updated_at, paid_at, settled_at, watch_until, amount_key,
chain_id, needs_attention, cancelled_at, cancelled_by, late_funds_mark, retired_at, retired_unchecked,
asset_contract, oldest_pending_block)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, paymentValues(v)...)
	return mapped(err)
}

func (t transaction) UpdatePayment(ctx context.Context, v core.Payment) error {
	return t.replace(ctx, `UPDATE payments SET
status=?2, amount=?3, asset=?4, address=?5,
address_index=?6, order_id=?7, description=?8, customer_email=?9,
redirect_url=?10, metadata=?11, link_id=?12, idempotency_key=?13,
required_confirmations=?14, tolerance=?15, amount_received=?16, amount_confirmed=?17,
tx_hash=?18, from_address=?19, pending_block=?20, created_at_block=?21,
attention=?22, resolution=?23, created_at=?24, expires_at=?25,
updated_at=?26, paid_at=?27, settled_at=?28, watch_until=?29,
amount_key=?30, chain_id=?31, needs_attention=?32, cancelled_at=?33,
cancelled_by=?34, late_funds_mark=?35, retired_at=?36, retired_unchecked=?37,
asset_contract=?38, oldest_pending_block=?39
WHERE id=?1`, paymentValues(v)...)
}
