package sqlite

import (
	"context"

	"github.com/lubluniky/hmnpay/core"
)

const selectChainHead = `SELECT chain_id, number, hash, time, observed_at, checked, final, final_time FROM chain_head`

func scanChainHead(row scanner) (core.ChainHead, error) {
	var v core.ChainHead
	err := row.Scan(
		&v.ChainID, &v.Number, &v.Hash,
		(*timestamp)(&v.Time), (*timestamp)(&v.ObservedAt), (*timestamp)(&v.Checked), &v.Final, (*timestamp)(&v.FinalTime),
	)
	return v, mapped(err)
}

func (t transaction) GetChainHead(ctx context.Context) (core.ChainHead, error) {
	return scanChainHead(t.QueryRowContext(ctx, selectChainHead))
}

func (t transaction) SaveChainHead(ctx context.Context, v core.ChainHead) error {
	_, err := t.ExecContext(ctx, `INSERT INTO chain_head (singleton, chain_id, number, hash, time, observed_at, checked, final, final_time)
VALUES (1,?,?,?,?,?,?,?,?)
ON CONFLICT(singleton) DO UPDATE SET chain_id=excluded.chain_id, number=excluded.number, hash=excluded.hash,
time=excluded.time, observed_at=excluded.observed_at, checked=excluded.checked, final=excluded.final, final_time=excluded.final_time`,
		unsigned(v.ChainID), unsigned(v.Number), v.Hash,
		v.Time.UnixMilli(), v.ObservedAt.UnixMilli(), v.Checked.UnixMilli(), unsigned(v.Final), v.FinalTime.UnixMilli(),
	)
	return mapped(err)
}
