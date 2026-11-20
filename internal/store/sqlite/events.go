package sqlite

import (
	"context"

	"github.com/lubluniky/hmnpay/core"
)

const selectEvent = `SELECT seq, id, type, payment_id, created_at, data FROM events`

func scanEvent(row scanner) (core.Event, error) {
	var v core.Event
	err := row.Scan(
		&v.Seq, &v.ID, &v.Type,
		&v.PaymentID, (*timestamp)(&v.CreatedAt), (*[]byte)(&v.Data),
	)
	return v, mapped(err)
}

func (t transaction) AppendEvent(ctx context.Context, v *core.Event) error {
	result, err := t.ExecContext(ctx, `INSERT INTO events (id, type, payment_id, created_at, data)
VALUES (?,?,?,?,?)`,
		v.ID, v.Type, v.PaymentID,
		v.CreatedAt.UnixMilli(), []byte(v.Data),
	)
	if err != nil {
		return mapped(err)
	}
	v.Seq, err = result.LastInsertId()
	return err
}
