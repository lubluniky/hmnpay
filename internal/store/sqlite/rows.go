package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lubluniky/hmnpay/core"
	driverSQLite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Go's Unicode lowercase mapping matches the core's substring semantics.
// SQLite's built-in lower() only folds ASCII.
func init() {
	driverSQLite.MustRegisterDeterministicScalarFunction("hmnpay_lower", 1, func(_ *driverSQLite.FunctionContext, args []driver.Value) (driver.Value, error) {
		s, ok := args[0].(string)
		if !ok {
			return nil, errors.New("hmnpay_lower expects text")
		}
		return strings.ToLower(s), nil
	})
}

type timestamp time.Time

func (t *timestamp) Scan(v any) error {
	ms, ok := v.(int64)
	if !ok {
		return fmt.Errorf("expected millisecond timestamp, got %T", v)
	}
	*t = timestamp(time.UnixMilli(ms).UTC())
	return nil
}

type amount core.Amount

func (a *amount) Scan(v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("expected amount text, got %T", v)
	}
	return (*core.Amount)(a).UnmarshalText([]byte(s))
}

type jsonField struct{ value any }

func (j jsonField) Value() (driver.Value, error) {
	b, err := json.Marshal(j.value)
	return string(b), err
}

func (j jsonField) Scan(v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("expected JSON text, got %T", v)
	}
	return json.Unmarshal([]byte(s), j.value)
}

// Fixed-width text preserves the unsigned range and numeric order.
func unsigned[T ~uint64 | ~uint](v T) string { return fmt.Sprintf("%020d", v) }

func mapped(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrNotFound
	}
	var e *driverSQLite.Error
	if errors.As(err, &e) && (e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || e.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY) {
		return fmt.Errorf("%w: %w", core.ErrConflict, err)
	}
	return err
}

func (t transaction) change(ctx context.Context, stmt string, args ...any) (int, error) {
	r, err := t.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, mapped(err)
	}
	n, err := r.RowsAffected()
	return int(n), err
}

func (t transaction) replace(ctx context.Context, stmt string, args ...any) error {
	n, err := t.change(ctx, stmt, args...)
	if err == nil && n == 0 {
		return core.ErrNotFound
	}
	return err
}

type scanner interface{ Scan(...any) error }

func many[T any](ctx context.Context, t transaction, scan func(scanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := t.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
