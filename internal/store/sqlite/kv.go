package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"
)

// This file is a small durable key-value facility for adapters (today: the
// chain watcher's checkpoint). It lives outside the core.Store port on
// purpose: the core never sees these values, and they are not part of its
// transactions. Writes go through the same single write connection as core
// transactions, so they are serialized with them and never interleave.

// GetValue returns the value stored under key, or nil when there is none.
func (s *Store) GetValue(ctx context.Context, key string) ([]byte, error) {
	var v []byte
	err := s.readers.QueryRowContext(ctx, "SELECT value FROM kv WHERE key=?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(v) == 0 {
		return nil, nil
	}
	return bytes.Clone(v), nil
}

// PutValue atomically replaces the value stored under key. An empty value
// deletes the key, so a later GetValue returns nil. The value is durable
// when PutValue returns nil (the database runs with synchronous=FULL).
func (s *Store) PutValue(ctx context.Context, key string, value []byte) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(value) == 0 {
		_, err = tx.ExecContext(ctx, "DELETE FROM kv WHERE key=?", key)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO kv (key, value, updated_at) VALUES (?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			key, value, time.Now().UnixMilli())
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CheckpointKey is the key under which [Store.Checkpoints] keeps the chain
// watcher's checkpoint.
const CheckpointKey = "chain.watcher.checkpoint"

// Checkpoints returns the durable store of the chain watcher's checkpoint
// (it implements watcher.Store, which corebridge.New takes).
func (s *Store) Checkpoints() *Checkpoints { return &Checkpoints{s: s, key: CheckpointKey} }

// Checkpoints keeps one opaque value, replaced atomically.
type Checkpoints struct {
	s   *Store
	key string
}

// LoadCheckpoint returns the saved checkpoint, or nil if there is none.
func (c *Checkpoints) LoadCheckpoint(ctx context.Context) ([]byte, error) {
	return c.s.GetValue(ctx, c.key)
}

// SaveCheckpoint durably replaces the checkpoint; saving an empty value
// removes it.
func (c *Checkpoints) SaveCheckpoint(ctx context.Context, data []byte) error {
	return c.s.PutValue(ctx, c.key, data)
}
