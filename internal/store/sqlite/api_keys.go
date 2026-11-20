package sqlite

import (
	"context"

	"github.com/lubluniky/hmnpay/core"
)

const selectAPIKey = `SELECT id, name, mode, prefix, last4, secret_hash,
scopes, created_at, last_used_at, expires_at, revoked_at FROM api_keys`

func scanAPIKey(row scanner) (core.APIKey, error) {
	var v core.APIKey
	err := row.Scan(
		&v.ID, &v.Name, &v.Mode,
		&v.Prefix, &v.Last4, &v.SecretHash,
		jsonField{&v.Scopes}, (*timestamp)(&v.CreatedAt), (*timestamp)(&v.LastUsedAt),
		(*timestamp)(&v.ExpiresAt), (*timestamp)(&v.RevokedAt),
	)
	return v, mapped(err)
}

func (t transaction) GetAPIKey(ctx context.Context, value string) (core.APIKey, error) {
	return scanAPIKey(t.QueryRowContext(ctx, selectAPIKey+" WHERE id=?", value))
}

func (t transaction) GetAPIKeyByHash(ctx context.Context, value string) (core.APIKey, error) {
	return scanAPIKey(t.QueryRowContext(ctx, selectAPIKey+" WHERE secret_hash=?", value))
}

func (t transaction) ListAPIKeys(ctx context.Context) ([]core.APIKey, error) {
	return many(ctx, t, scanAPIKey, selectAPIKey+" ORDER BY created_at DESC, id DESC")
}

func apiKeyValues(v core.APIKey) []any {
	return []any{
		v.ID, v.Name, v.Mode,
		v.Prefix, v.Last4, v.SecretHash,
		jsonField{v.Scopes}, v.CreatedAt.UnixMilli(), v.LastUsedAt.UnixMilli(),
		v.ExpiresAt.UnixMilli(), v.RevokedAt.UnixMilli(),
	}
}

func (t transaction) InsertAPIKey(ctx context.Context, v core.APIKey) error {
	_, err := t.ExecContext(ctx, `INSERT INTO api_keys (id, name, mode, prefix, last4, secret_hash,
scopes, created_at, last_used_at, expires_at, revoked_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`, apiKeyValues(v)...)
	return mapped(err)
}

func (t transaction) UpdateAPIKey(ctx context.Context, v core.APIKey) error {
	return t.replace(ctx, `UPDATE api_keys SET
name=?2, mode=?3, prefix=?4, last4=?5,
secret_hash=?6, scopes=?7, created_at=?8, last_used_at=?9,
expires_at=?10, revoked_at=?11
WHERE id=?1`, apiKeyValues(v)...)
}
