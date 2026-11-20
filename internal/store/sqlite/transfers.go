package sqlite

import (
	"context"

	"github.com/lubluniky/hmnpay/core"
)

const selectTransfer = `SELECT tx_hash, log_index, block_hash, payment_id, block_number, block_time,
sender, recipient, amount, status, seen_at, confirmed_at,
orphaned_at, not_before FROM transfers`

func scanTransfer(row scanner) (core.Transfer, error) {
	var v core.Transfer
	err := row.Scan(
		&v.TxHash, &v.LogIndex, &v.BlockHash,
		&v.PaymentID, &v.BlockNumber, (*timestamp)(&v.BlockTime),
		&v.From, &v.To, (*amount)(&v.Amount),
		&v.Status, (*timestamp)(&v.SeenAt), (*timestamp)(&v.ConfirmedAt),
		(*timestamp)(&v.OrphanedAt), (*timestamp)(&v.NotBefore),
	)
	return v, mapped(err)
}

func (t transaction) GetTransfer(ctx context.Context, key core.TransferKey) (core.Transfer, error) {
	return scanTransfer(t.QueryRowContext(ctx, selectTransfer+" WHERE tx_hash=? AND log_index=? AND block_hash=?", key.TxHash, unsigned(key.LogIndex), key.BlockHash))
}

func (t transaction) ListTransfers(ctx context.Context, value string) ([]core.Transfer, error) {
	return many(ctx, t, scanTransfer, selectTransfer+" WHERE payment_id=? ORDER BY block_number, log_index, block_hash, tx_hash", value)
}

func (t transaction) ListTransfersByTxHash(ctx context.Context, value string) ([]core.Transfer, error) {
	return many(ctx, t, scanTransfer, selectTransfer+" WHERE tx_hash=? ORDER BY block_number, log_index, block_hash, tx_hash", value)
}

func (t transaction) ListPendingTransfers(ctx context.Context) ([]core.Transfer, error) {
	return many(ctx, t, scanTransfer, selectTransfer+" WHERE status='pending' ORDER BY block_number, log_index, block_hash, tx_hash")
}

func transferValues(v core.Transfer) []any {
	return []any{
		v.TxHash, unsigned(v.LogIndex), v.BlockHash,
		v.PaymentID, unsigned(v.BlockNumber), v.BlockTime.UnixMilli(),
		v.From, v.To, v.Amount.String(),
		v.Status, v.SeenAt.UnixMilli(), v.ConfirmedAt.UnixMilli(),
		v.OrphanedAt.UnixMilli(), v.NotBefore.UnixMilli(),
	}
}

func (t transaction) InsertTransfer(ctx context.Context, v core.Transfer) error {
	_, err := t.ExecContext(ctx, `INSERT INTO transfers (tx_hash, log_index, block_hash, payment_id, block_number, block_time,
sender, recipient, amount, status, seen_at, confirmed_at,
orphaned_at, not_before)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, transferValues(v)...)
	return mapped(err)
}

func (t transaction) UpdateTransfer(ctx context.Context, v core.Transfer) error {
	return t.replace(ctx, `UPDATE transfers SET
payment_id=?4, block_number=?5, block_time=?6, sender=?7,
recipient=?8, amount=?9, status=?10, seen_at=?11,
confirmed_at=?12, orphaned_at=?13, not_before=?14
WHERE tx_hash=?1 AND log_index=?2 AND block_hash=?3`, transferValues(v)...)
}
