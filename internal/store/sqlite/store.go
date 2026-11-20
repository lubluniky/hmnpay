// Package sqlite implements the hmnpay persistence port using SQLite.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/lubluniky/hmnpay/core"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store owns the SQLite connection pools for one database.
type Store struct{ writer, readers *sql.DB }
type transaction struct{ *sql.Tx }

// Option configures Open.
type Option func(*options)
type options struct{ readers int }

// WithReaders sets the maximum number of concurrent file-backed views.
func WithReaders(n int) Option { return func(o *options) { o.readers = n } }

// Open opens a database file, or a private database for ":memory:".
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	cfg := options{readers: 4}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.readers < 1 {
		return nil, errors.New("sqlite: reader count must be positive")
	}
	memory := path == ":memory:"
	var base string
	if memory {
		base = "file::memory:"
	} else {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
		base = (&url.URL{Scheme: "file", Path: abs}).String()
	}
	// FULL synchronizes the WAL at every commit, including the payment and
	// its outbox records. Apply connection-local pragmas on every pool dial.
	base += "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(FULL)"
	writer, err := sql.Open("sqlite", base+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	writer.SetMaxOpenConns(1)
	s := &Store{writer: writer, readers: writer}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	if !memory {
		var mode string
		if err = writer.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
			return nil, err
		}
		if mode != "wal" {
			return nil, fmt.Errorf("sqlite: WAL unavailable (journal mode %q)", mode)
		}
	}
	if err = s.migrate(ctx); err != nil {
		return nil, err
	}
	if !memory {
		readers, err := sql.Open("sqlite", base+"&mode=ro&_pragma=query_only(ON)")
		if err != nil {
			return nil, err
		}
		s.readers = readers
		readers.SetMaxOpenConns(cfg.readers)
		if err = readers.PingContext(ctx); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	files := []string{
		"migrations/001_initial.sql",
		"migrations/002_cancel_and_claims.sql",
		"migrations/003_kv.sql",
		"migrations/004_session_last_seen.sql",
		"migrations/005_late_funds_and_recovery.sql",
		"migrations/006_link_minimum.sql",
		"migrations/007_late_funds_mark.sql",
		"migrations/008_delivery_event_seq.sql",
		"migrations/009_delivery_body_seq.sql",
		"migrations/010_payment_retired.sql",
		"migrations/011_head_final.sql",
		"migrations/012_head_without_final.sql",
		"migrations/013_final_time.sql",
	}
	if version < 0 || version > len(files) {
		return fmt.Errorf("sqlite: unsupported schema version %d (latest %d)", version, len(files))
	}
	for i := version; i < len(files); i++ {
		script, err := migrations.ReadFile(files[i])
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(script)); err != nil {
			return fmt.Errorf("sqlite: migration %s: %w", files[i], err)
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", i+1)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Close() error {
	if s.readers == s.writer {
		return s.writer.Close()
	}
	return errors.Join(s.readers.Close(), s.writer.Close())
}

func (s *Store) Update(ctx context.Context, fn func(core.Tx) error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(transaction{tx}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) View(ctx context.Context, fn func(core.Reader) error) error {
	tx, err := s.readers.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(struct{ core.Reader }{transaction{tx}})
}
