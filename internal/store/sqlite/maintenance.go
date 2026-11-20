package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (t transaction) GetCursor(ctx context.Context, consumer string) (int64, error) {
	var seq int64
	err := t.QueryRowContext(ctx, "SELECT seq FROM cursors WHERE consumer=?", consumer).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

func (t transaction) SetCursor(ctx context.Context, consumer string, seq int64) error {
	_, err := t.ExecContext(ctx, "INSERT INTO cursors(consumer,seq) VALUES(?,?) ON CONFLICT(consumer) DO UPDATE SET seq=excluded.seq", consumer, seq)
	return err
}

func (t transaction) DeleteWebhookEndpoint(ctx context.Context, id string) error {
	return t.replace(ctx, "DELETE FROM endpoints WHERE id=?", id)
}

func (t transaction) DeleteSession(ctx context.Context, id string) error {
	_, err := t.change(ctx, "DELETE FROM sessions WHERE id=?", id)
	return err
}

func (t transaction) DeleteSessionsExcept(ctx context.Context, id string) (int, error) {
	if id == "" {
		return t.change(ctx, "DELETE FROM sessions")
	}
	return t.change(ctx, "DELETE FROM sessions WHERE id<>?", id)
}

func (t transaction) DeleteSessionsExpiredBefore(ctx context.Context, before time.Time) (int, error) {
	return t.change(ctx, "DELETE FROM sessions WHERE expires_at<?", ceilMillis(before))
}

func (t transaction) DeleteDeliveriesBefore(ctx context.Context, before time.Time) (int, error) {
	return t.change(ctx, "DELETE FROM deliveries WHERE created_at<?", ceilMillis(before))
}
