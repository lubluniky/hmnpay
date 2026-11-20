package sqlite

import (
	"context"

	"github.com/lubluniky/hmnpay/core"
)

const selectOperator = `SELECT id, email, password_hash, created_at, updated_at,
generation, recovery_hash, recovery_expires_at, recovery_attempts FROM operator`

func scanOperator(row scanner) (core.Operator, error) {
	var v core.Operator
	err := row.Scan(
		&v.ID, &v.Email, &v.PasswordHash,
		(*timestamp)(&v.CreatedAt), (*timestamp)(&v.UpdatedAt),
		&v.Generation, &v.RecoveryHash, (*timestamp)(&v.RecoveryExpiresAt), &v.RecoveryAttempts,
	)
	return v, mapped(err)
}

func (t transaction) GetOperator(ctx context.Context) (core.Operator, error) {
	return scanOperator(t.QueryRowContext(ctx, selectOperator))
}

func (t transaction) SaveOperator(ctx context.Context, v core.Operator) error {
	_, err := t.ExecContext(ctx, `INSERT INTO operator (singleton, id, email, password_hash, created_at, updated_at,
generation, recovery_hash, recovery_expires_at, recovery_attempts)
VALUES (1,?,?,?,?,?,?,?,?,?)
ON CONFLICT(singleton) DO UPDATE SET id=excluded.id, email=excluded.email, password_hash=excluded.password_hash,
created_at=excluded.created_at, updated_at=excluded.updated_at, generation=excluded.generation,
recovery_hash=excluded.recovery_hash, recovery_expires_at=excluded.recovery_expires_at,
recovery_attempts=excluded.recovery_attempts`,
		v.ID, v.Email, v.PasswordHash,
		v.CreatedAt.UnixMilli(), v.UpdatedAt.UnixMilli(),
		v.Generation, v.RecoveryHash, v.RecoveryExpiresAt.UnixMilli(), v.RecoveryAttempts,
	)
	return mapped(err)
}
