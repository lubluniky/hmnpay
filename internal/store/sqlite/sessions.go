package sqlite

import (
	"context"

	"github.com/lubluniky/hmnpay/core"
)

const selectSession = `SELECT id, operator_id, token_hash, user_agent, ip, created_at,
expires_at, last_seen_at FROM sessions`

func scanSession(row scanner) (core.Session, error) {
	var v core.Session
	err := row.Scan(
		&v.ID, &v.OperatorID, &v.TokenHash,
		&v.UserAgent, &v.IP, (*timestamp)(&v.CreatedAt),
		(*timestamp)(&v.ExpiresAt), (*timestamp)(&v.LastSeenAt),
	)
	return v, mapped(err)
}

func (t transaction) GetSessionByTokenHash(ctx context.Context, value string) (core.Session, error) {
	return scanSession(t.QueryRowContext(ctx, selectSession+" WHERE token_hash=?", value))
}

func (t transaction) ListSessions(ctx context.Context) ([]core.Session, error) {
	return many(ctx, t, scanSession, selectSession+" ORDER BY created_at DESC, id DESC")
}

func (t transaction) InsertSession(ctx context.Context, v core.Session) error {
	_, err := t.ExecContext(ctx, `INSERT INTO sessions (id, operator_id, token_hash, user_agent, ip, created_at,
expires_at, last_seen_at)
VALUES (?,?,?,?,?,?,?,?)`,
		v.ID, v.OperatorID, v.TokenHash,
		v.UserAgent, v.IP, v.CreatedAt.UnixMilli(),
		v.ExpiresAt.UnixMilli(), v.LastSeenAt.UnixMilli(),
	)
	return mapped(err)
}

func (t transaction) UpdateSession(ctx context.Context, v core.Session) error {
	return t.replace(ctx, `UPDATE sessions SET
operator_id=?2, token_hash=?3, user_agent=?4, ip=?5,
created_at=?6, expires_at=?7, last_seen_at=?8
WHERE id=?1`,
		v.ID, v.OperatorID, v.TokenHash,
		v.UserAgent, v.IP, v.CreatedAt.UnixMilli(),
		v.ExpiresAt.UnixMilli(), v.LastSeenAt.UnixMilli(),
	)
}
