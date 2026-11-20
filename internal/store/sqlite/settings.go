package sqlite

import (
	"context"

	"github.com/lubluniky/hmnpay/core"
)

const selectSettings = `SELECT setup_completed, public_url, merchant, payments, network, asset,
wallet, updated_at FROM settings`

func scanSettings(row scanner) (core.Settings, error) {
	var v core.Settings
	err := row.Scan(
		&v.SetupCompleted, &v.PublicURL, jsonField{&v.Merchant},
		jsonField{&v.Payments}, jsonField{&v.Network}, jsonField{&v.Asset},
		jsonField{&v.Wallet}, (*timestamp)(&v.UpdatedAt),
	)
	return v, mapped(err)
}

func (t transaction) GetSettings(ctx context.Context) (core.Settings, error) {
	return scanSettings(t.QueryRowContext(ctx, selectSettings))
}

func (t transaction) SaveSettings(ctx context.Context, v core.Settings) error {
	_, err := t.ExecContext(ctx, `INSERT INTO settings (singleton, setup_completed, public_url, merchant, payments, network, asset,
wallet, updated_at)
VALUES (1,?,?,?,?,?,?,?,?)
ON CONFLICT(singleton) DO UPDATE SET setup_completed=excluded.setup_completed, public_url=excluded.public_url, merchant=excluded.merchant,
payments=excluded.payments, network=excluded.network, asset=excluded.asset,
wallet=excluded.wallet, updated_at=excluded.updated_at`,
		v.SetupCompleted, v.PublicURL, jsonField{v.Merchant},
		jsonField{v.Payments}, jsonField{v.Network}, jsonField{v.Asset},
		jsonField{v.Wallet}, v.UpdatedAt.UnixMilli(),
	)
	return mapped(err)
}
