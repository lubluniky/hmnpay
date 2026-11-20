package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestMigrations(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "version.db")
	for range 2 {
		s := openAt(t, path)
		var version int
		if err := s.writer.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != latestSchema {
			t.Fatalf("version %d: %v", version, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", latestSchema+1)); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err == nil {
		s.Close()
		t.Fatal("accepted newer schema")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("unsupported schema version %d", latestSchema+1)) {
		t.Fatal(err)
	}
	var version int
	if err := raw.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != latestSchema+1 {
		t.Fatalf("newer schema version %d: %v", version, err)
	}
}

// latestSchema is the schema version a freshly opened database has.
const latestSchema = 13

// A database created by the first release is upgraded in place: existing
// payments and deliveries read back with the new fields at their zero
// values and can be written again.
func TestMigrateFromV1(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "v1.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	script, err := migrations.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	zero := time.Time{}.UnixMilli()
	now := time.Date(2026, 8, 17, 10, 20, 30, 0, time.UTC).UnixMilli()
	for _, stmt := range []string{
		string(script),
		"PRAGMA user_version=1",
		fmt.Sprintf(`INSERT INTO payments (id, status, amount, asset, address, address_index,
order_id, description, customer_email, redirect_url, metadata, link_id,
idempotency_key, required_confirmations, tolerance, amount_received, amount_confirmed, tx_hash,
from_address, pending_block, created_at_block, attention, resolution, created_at,
expires_at, updated_at, paid_at, settled_at, watch_until, amount_key, chain_id, needs_attention)
VALUES ('pay_old','awaiting','5','{}','0xabc',7,'ORD-1','','','','{}','','',15,'0','0','0','','',
'00000000000000000000','00000000000000000000','','null',%[1]d,%[1]d,%[1]d,%[2]d,%[2]d,%[1]d,'5','00000000000000000056',0)`, now, zero),
		fmt.Sprintf(`INSERT INTO deliveries (id, endpoint_id, event_id, event_type, payment_id, url,
status, attempt, max_attempts, next_attempt_at, request_body, response_code,
response_body, duration_ms, error, attempts, created_at, updated_at, completed_at)
VALUES ('whd_old','we_1','evt_1','payment.paid','pay_old','https://example.com','retrying',1,8,%[1]d,'{}',500,'',12,'',
'[{"N":1,"At":"2026-08-17T10:20:30Z","ResponseCode":500,"DurationMs":12,"Error":"","Succeeded":false}]',%[1]d,%[1]d,%[2]d)`, now, zero),
		// Migration 008 gives deliveries the seq of their event.
		fmt.Sprintf(`INSERT INTO events (id, type, payment_id, created_at, data) VALUES
('evt_0','payment.created','pay_old',%[1]d,'{}'), ('evt_1','payment.paid','pay_old',%[1]d,'{}')`, now),
		fmt.Sprintf(`INSERT INTO sessions VALUES ('sess_old','op_1','hash','ua','::1',%[1]d,%[1]d)`, now),
		fmt.Sprintf(`INSERT INTO operator (id, email, password_hash, created_at, updated_at, singleton)
VALUES ('op_1','owner@example.com','$hash',%[1]d,%[1]d,1)`, now),
		fmt.Sprintf(`INSERT INTO transfers (tx_hash, log_index, block_hash, payment_id, block_number, block_time,
sender, recipient, amount, status, seen_at, confirmed_at, orphaned_at)
VALUES ('0xtx','00000000000000000001','0xblock','pay_old','00000000000000000010',%[2]d,'0xfrom','0xabc','5','confirmed',%[1]d,%[1]d,%[2]d)`, now, zero),
	} {
		if _, err = raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%.60s…: %v", stmt, err)
		}
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}

	s := openAt(t, path)
	var version int
	if err := s.writer.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != latestSchema {
		t.Fatalf("version %d: %v", version, err)
	}
	err = s.Update(ctx, func(tx core.Tx) error {
		p, err := tx.GetPayment(ctx, "pay_old")
		if err != nil {
			return err
		}
		if p.OrderID != "ORD-1" || p.AddressIndex != 7 || !p.CancelledAt.IsZero() || p.CancelledBy != "" {
			t.Errorf("migrated payment: %+v", p)
		}
		d, err := tx.GetDelivery(ctx, "whd_old")
		if err != nil {
			return err
		}
		if d.ClaimID != "" || d.Attempt != 1 || len(d.Attempts) != 1 || d.Attempts[0].Late || d.Attempts[0].ResponseCode != 500 || d.EventSeq != 2 {
			t.Errorf("migrated delivery: %+v", d)
		}
		sessions, err := tx.ListSessions(ctx)
		if err != nil || len(sessions) != 1 || sessions[0].ID != "sess_old" || !sessions[0].LastSeenAt.IsZero() {
			t.Errorf("migrated sessions: %+v, %v", sessions, err)
		}
		if err == nil && len(sessions) == 1 {
			sessions[0].LastSeenAt = time.UnixMilli(now).UTC()
			if err := tx.UpdateSession(ctx, sessions[0]); err != nil {
				return err
			}
			if got, err := tx.GetSessionByTokenHash(ctx, "hash"); err != nil || !got.LastSeenAt.Equal(sessions[0].LastSeenAt) {
				t.Errorf("rewritten session: %+v, %v", got, err)
			}
		}
		// Migration 005: credentials start at generation 0 with no recovery
		// code, old transfers are undated; both take the new fields.
		op, err := tx.GetOperator(ctx)
		if err != nil {
			return err
		}
		if op.Email != "owner@example.com" || op.Generation != 0 || op.RecoveryHash != "" || !op.RecoveryExpiresAt.IsZero() || op.RecoveryAttempts != 0 {
			t.Errorf("migrated operator: %+v", op)
		}
		op.Generation, op.RecoveryHash, op.RecoveryExpiresAt, op.RecoveryAttempts = 3, "abc", time.UnixMilli(now).UTC(), 2
		if err := tx.SaveOperator(ctx, op); err != nil {
			return err
		}
		if got, err := tx.GetOperator(ctx); err != nil || got != op {
			t.Errorf("rewritten operator: %+v, %v", got, err)
		}
		transfers, err := tx.ListTransfers(ctx, "pay_old")
		if err != nil || len(transfers) != 1 || !transfers[0].NotBefore.IsZero() || !transfers[0].BlockTime.IsZero() {
			t.Errorf("migrated transfers: %+v, %v", transfers, err)
		}
		if err == nil && len(transfers) == 1 {
			transfers[0].NotBefore = time.UnixMilli(now).UTC()
			if err := tx.UpdateTransfer(ctx, transfers[0]); err != nil {
				return err
			}
			if got, err := tx.GetTransfer(ctx, transfers[0].Key()); err != nil || !got.NotBefore.Equal(transfers[0].NotBefore) {
				t.Errorf("rewritten transfer: %+v, %v", got, err)
			}
		}
		// The upgraded rows accept the new fields.
		p.CancelledAt, p.CancelledBy = time.UnixMilli(now).UTC(), "owner"
		d.ClaimID = "whc_1"
		d.Attempts = append(d.Attempts, core.DeliveryAttempt{N: 1, Late: true})
		if err := errors.Join(tx.UpdatePayment(ctx, p), tx.UpdateDelivery(ctx, d)); err != nil {
			return err
		}
		p2, _ := tx.GetPayment(ctx, "pay_old")
		d2, _ := tx.GetDelivery(ctx, "whd_old")
		if !reflect.DeepEqual(p2, p) || d2.ClaimID != "whc_1" || !d2.Attempts[1].Late {
			t.Errorf("rewritten rows: %+v / %+v", p2, d2)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigrationRollback(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "collision.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.ExecContext(ctx, "CREATE TABLE cursors(existing TEXT)"); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err == nil {
		s.Close()
		t.Fatal("expected migration collision")
	}
	var count int
	if err := raw.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='payments'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial migration: payments count %d: %v", count, err)
	}
}

func TestPersistence(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "persistent ?#.db")
	s := openAt(t, path)
	now := time.Date(2026, 8, 17, 10, 20, 30, 123000000, time.UTC)
	settings := core.DefaultSettings()
	head := core.ChainHead{Number: math.MaxUint64, ObservedAt: now}
	op := core.Operator{ID: "op", PasswordHash: "password hash"}
	key := core.APIKey{ID: "key", SecretHash: "hash"}
	session := core.Session{ID: "session", TokenHash: "token", ExpiresAt: now}
	payment := core.Payment{ID: "p", Description: "CAFÉ_50%", Address: "address", Asset: core.USDTBSC, Amount: core.NewAmount(math.MaxUint64), Metadata: map[string]string{"k": "v"}, CreatedAt: now}
	transfer := core.Transfer{TxHash: "hash", PaymentID: "p", BlockNumber: math.MaxUint64, Amount: payment.Amount}
	link := core.PaymentLink{ID: "link", Slug: "slug", TotalCollected: payment.Amount}
	event := core.Event{ID: "event", PaymentID: "p", CreatedAt: now, Data: []byte(" { \"a\": \"<x>\" }\n")}
	ep := core.WebhookEndpoint{ID: "endpoint", Secret: "webhook secret"}
	delivery := core.WebhookDelivery{ID: "delivery", EndpointID: ep.ID, Attempts: []core.DeliveryAttempt{{At: now}}}
	err := s.Update(ctx, func(tx core.Tx) error {
		return errors.Join(
			tx.SaveSettings(ctx, settings),
			tx.SaveChainHead(ctx, head),
			tx.SaveOperator(ctx, op),
			tx.InsertAPIKey(ctx, key),
			tx.InsertSession(ctx, session),
			tx.InsertPayment(ctx, payment),
			tx.InsertTransfer(ctx, transfer),
			tx.InsertLink(ctx, link),
			tx.AppendEvent(ctx, &event),
			tx.SetCursor(ctx, "cursor", event.Seq),
			tx.InsertWebhookEndpoint(ctx, ep),
			tx.InsertDelivery(ctx, delivery),
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path)
	err = s.View(ctx, func(r core.Reader) error {
		check(t, settings)(r.GetSettings(ctx))
		check(t, head)(r.GetChainHead(ctx))
		check(t, op)(r.GetOperator(ctx))
		check(t, key)(r.GetAPIKey(ctx, key.ID))
		check(t, session)(r.GetSessionByTokenHash(ctx, session.TokenHash))
		check(t, []core.Payment{payment})(r.ListPayments(ctx, core.PaymentFilter{Search: "é_50%"}, core.PaymentSort{}, core.Page{}))
		check(t, transfer)(r.GetTransfer(ctx, transfer.Key()))
		check(t, link)(r.GetLink(ctx, link.ID))
		check(t, []core.Event{event})(r.ListEvents(ctx, core.EventFilter{}))
		check(t, event.Seq)(r.GetCursor(ctx, "cursor"))
		check(t, ep)(r.GetWebhookEndpoint(ctx, ep.ID))
		check(t, delivery)(r.GetDelivery(ctx, delivery.ID))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func check[T any](t *testing.T, want T) func(T, error) {
	return func(got T, err error) {
		t.Helper()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("read %T: got %#v, %v; want %#v", want, got, err, want)
		}
	}
}

func TestConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contended.db")
	stores := []*Store{openAt(t, path), openAt(t, path)}
	ctx := t.Context()
	const writers = 64
	start := make(chan struct{})
	done := make(chan error, writers)
	for i := range writers {
		go func() {
			<-start
			done <- stores[i%2].Update(ctx, func(tx core.Tx) error {
				n, err := tx.GetCursor(ctx, "counter")
				if err != nil {
					return err
				}
				return tx.SetCursor(ctx, "counter", n+1)
			})
		}()
	}
	close(start)
	for range writers {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	if err := stores[0].View(ctx, func(r core.Reader) error { check(t, int64(writers))(r.GetCursor(ctx, "counter")); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestViewDuringWriter(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	s := openTestDB(t)
	written := make(chan struct{})
	commit := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Update(ctx, func(tx core.Tx) error {
			if err := tx.SetCursor(ctx, "counter", 1); err != nil {
				return err
			}
			close(written)
			select {
			case <-commit:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-written:
	case err := <-done:
		t.Fatalf("writer failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	err := s.View(ctx, func(r core.Reader) error {
		readOld := func() error {
			n, err := r.GetCursor(ctx, "counter")
			if err == nil && n != 0 {
				return fmt.Errorf("expected old snapshot, got %d", n)
			}
			return err
		}
		if err := readOld(); err != nil {
			return err
		}
		close(commit)
		if err := <-done; err != nil {
			return err
		}
		return readOld()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.View(ctx, func(r core.Reader) error { check(t, int64(1))(r.GetCursor(ctx, "counter")); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestMemory(t *testing.T) {
	s := openAt(t, ":memory:")
	ctx := t.Context()
	if err := s.Update(ctx, func(tx core.Tx) error { return tx.SetCursor(ctx, "consumer", 3) }); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(r core.Reader) error { check(t, int64(3))(r.GetCursor(ctx, "consumer")); return nil }); err != nil {
		t.Fatal(err)
	}
}

// A database of the previous release (schema 5) is upgraded in place by
// migrations 006 and 007: a link gets the default minimum, and whatever is
// confirmed on a payment counts as accounted for, so that no payment that
// settled before the upgrade is flagged for late funds because of it.
func TestMigrateFromV5(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "v5.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var stmts []string
	for _, f := range []string{"001_initial", "002_cancel_and_claims", "003_kv", "004_session_last_seen", "005_late_funds_and_recovery"} {
		script, err := migrations.ReadFile("migrations/" + f + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		stmts = append(stmts, string(script))
	}
	zero := time.Time{}.UnixMilli()
	now := time.Date(2026, 11, 18, 10, 20, 30, 0, time.UTC).UnixMilli()
	payment := func(id, status, address, confirmed string) string {
		return fmt.Sprintf(`INSERT INTO payments (id, status, amount, asset, address, address_index,
order_id, description, customer_email, redirect_url, metadata, link_id,
idempotency_key, required_confirmations, tolerance, amount_received, amount_confirmed, tx_hash,
from_address, pending_block, created_at_block, attention, resolution, created_at,
expires_at, updated_at, paid_at, settled_at, watch_until, amount_key, chain_id, needs_attention)
VALUES ('%[3]s','%[4]s','149','{}','%[5]s',7,'','','','','{}','','',15,'0','%[6]s','%[6]s','','',
'00000000000000000000','00000000000000000000','','null',%[1]d,%[1]d,%[1]d,%[2]d,%[2]d,%[1]d,'149','00000000000000000056',0)`,
			now, zero, id, status, address, confirmed)
	}
	stmts = append(stmts,
		"PRAGMA user_version=5",
		payment("pay_paid", "paid", "0xa", "149"),
		payment("pay_expired", "expired", "0xb", "60"),
		payment("pay_open", "awaiting", "0xc", "0"),
		fmt.Sprintf(`INSERT INTO links (id, slug, name, description, amount, asset, active, max_uses, expires_at,
redirect_url, uses, total_collected, last_paid_at, created_at, updated_at)
VALUES ('plink_old','tips','Tips','','0','{}',1,3,%[2]d,'',0,'0',%[2]d,%[1]d,%[1]d)`, now, zero),
	)
	for _, stmt := range stmts {
		if _, err = raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%.60s…: %v", stmt, err)
		}
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}

	s := openAt(t, path)
	err = s.Update(ctx, func(tx core.Tx) error {
		for id, want := range map[string]string{"pay_paid": "149", "pay_expired": "60", "pay_open": "0"} {
			p, err := tx.GetPayment(ctx, id)
			if err != nil {
				return err
			}
			if p.LateFundsMark.String() != want || p.AmountConfirmed.String() != want {
				t.Errorf("%s: late funds mark %s, confirmed %s; want both %s", id, p.LateFundsMark, p.AmountConfirmed, want)
			}
			p.LateFundsMark = core.NewAmount(7)
			if err := tx.UpdatePayment(ctx, p); err != nil {
				return err
			}
			if got, err := tx.GetPayment(ctx, id); err != nil || got.LateFundsMark != core.NewAmount(7) {
				t.Errorf("%s rewritten: %+v, %v", id, got.LateFundsMark, err)
			}
		}
		l, err := tx.GetLink(ctx, "plink_old")
		if err != nil {
			return err
		}
		if !l.MinAmount.IsZero() || l.MaxUses != 3 {
			t.Errorf("migrated link: %+v", l)
		}
		l.MinAmount = core.NewAmount(5)
		if err := tx.UpdateLink(ctx, l); err != nil {
			return err
		}
		if got, err := tx.GetLinkBySlug(ctx, "tips"); err != nil || got.MinAmount != core.NewAmount(5) {
			t.Errorf("rewritten link: %+v, %v", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A delivery queued before webhook bodies carried "seq" is upgraded with its
// body: migration 008 gave it its event's position, 009 puts it into the
// body that is signed and sent — byte for byte what this version writes for
// the same event. The test event, which is not in the log, keeps a body
// without it; a body that is not JSON (none is ever written) is left alone
// and does not stop the upgrade.
func TestMigrateDeliveryBodiesGetTheirSeq(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "v7.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var stmts []string
	for _, f := range []string{"001_initial", "002_cancel_and_claims", "003_kv", "004_session_last_seen", "005_late_funds_and_recovery", "006_link_minimum", "007_late_funds_mark"} {
		script, err := migrations.ReadFile("migrations/" + f + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		stmts = append(stmts, string(script))
	}
	created := time.Date(2026, 11, 18, 10, 20, 30, 0, time.UTC)
	event := core.Event{ID: "evt_old", Type: core.EventPaymentPaid, PaymentID: "pay_1", CreatedAt: created, Data: json.RawMessage(`{"id":"pay_1","metadata":{"seq":"7","note":"zürich"}}`)}
	legacy, err := json.Marshal(event.Envelope()) // Seq 0: written without it
	if err != nil {
		t.Fatal(err)
	}
	delivery := func(id, eventID, body string) string {
		return fmt.Sprintf(`INSERT INTO deliveries (id, endpoint_id, event_id, event_type, payment_id, url, status, attempt,
max_attempts, next_attempt_at, request_body, response_code, response_body, duration_ms, error, attempts,
created_at, updated_at, completed_at, claim_id)
VALUES ('%s','whe_1','%s','payment.paid','pay_1','https://example.org','pending',0,8,%d,'%s',0,'',0,'','[]',%d,%d,0,'')`,
			id, eventID, created.UnixMilli(), strings.ReplaceAll(body, "'", "''"), created.UnixMilli(), created.UnixMilli())
	}
	stmts = append(stmts,
		"PRAGMA user_version=7",
		// Some events before it, so that its seq is not 1.
		`INSERT INTO events (id, type, payment_id, created_at, data) VALUES ('evt_a','payment.created','pay_1',0,'{}'),('evt_b','payment.detected','pay_1',0,'{}')`,
		fmt.Sprintf(`INSERT INTO events (id, type, payment_id, created_at, data) VALUES ('evt_old','payment.paid','pay_1',%d,'%s')`, created.UnixMilli(), event.Data),
		delivery("whd_old", "evt_old", string(legacy)),
		delivery("whd_test", "evt_test", `{"id":"evt_test","type":"webhook.test","created_at":"2026-11-18T10:20:30Z","data":{"message":"x"}}`),
		delivery("whd_bad", "evt_old", `not json}`),
	)
	for _, stmt := range stmts {
		if _, err = raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%.60s…: %v", stmt, err)
		}
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}

	s := openAt(t, path)
	err = s.View(ctx, func(r core.Reader) error {
		d, err := r.GetDelivery(ctx, "whd_old")
		if err != nil {
			return err
		}
		event.Seq = 3
		want, _ := json.Marshal(event.Envelope())
		if d.EventSeq != 3 || d.RequestBody != string(want) {
			t.Errorf("upgraded delivery: seq %d, body\n%s\nwant\n%s", d.EventSeq, d.RequestBody, want)
		}
		var env core.EventEnvelope
		if err := json.Unmarshal([]byte(d.RequestBody), &env); err != nil || env.Seq != 3 || env.ID != "evt_old" {
			t.Errorf("upgraded body: %+v, %v", env, err)
		}
		test, err := r.GetDelivery(ctx, "whd_test")
		if err != nil {
			return err
		}
		if test.EventSeq != 0 || strings.Contains(test.RequestBody, `"seq"`) {
			t.Errorf("the test event's delivery: seq %d, body %s", test.EventSeq, test.RequestBody)
		}
		bad, err := r.GetDelivery(ctx, "whd_bad")
		if err != nil {
			return err
		}
		if bad.RequestBody != "not json}" {
			t.Errorf("a body that is not JSON was changed: %s", bad.RequestBody)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMigrateRetirement: schema 10 records an address's retirement from the
// watch list and the asset contract of every payment, filled in for the
// payments that are there already; nothing is retired by the upgrade.
func TestMigrateRetirement(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "v9.db")
	s := openAt(t, path)
	p := core.Payment{ID: "pay_1", Status: core.StatusPaid, Amount: core.NewAmount(1), Asset: core.USDTBSC,
		Address: "0x0000000000000000000000000000000000000001", CreatedAt: time.Unix(1, 0), WatchUntil: time.Unix(100, 0)}
	if err := s.Update(ctx, func(tx core.Tx) error { return tx.InsertPayment(ctx, p) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"DROP INDEX payments_retirement",
		"ALTER TABLE payments DROP COLUMN retired_at",
		"ALTER TABLE payments DROP COLUMN retired_unchecked",
		"ALTER TABLE payments DROP COLUMN asset_contract",
		"ALTER TABLE chain_head DROP COLUMN checked",
		"ALTER TABLE chain_head DROP COLUMN final",
		"ALTER TABLE chain_head DROP COLUMN final_time",
		"ALTER TABLE payments DROP COLUMN oldest_pending_block",
		"PRAGMA user_version=9",
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s = openAt(t, path)
	no := false
	err = s.View(ctx, func(r core.Reader) error {
		n, err := r.CountPayments(ctx, core.PaymentFilter{Contract: core.USDTBSC.Contract, Retired: &no})
		if err != nil || n != 1 {
			t.Errorf("payments of the asset, not retired, after the upgrade: %d, %v", n, err)
		}
		got, err := r.GetPayment(ctx, "pay_1")
		if err != nil || !got.RetiredAt.IsZero() || got.RetiredUnchecked {
			t.Errorf("payment after the upgrade: %+v, %v", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMigrateDropsAHeadWithoutFinal: a head stored before heads carried
// their final block (schema 11 gave it 0) is dropped by the upgrade, and the
// core has no head until the chain adapter's first: a head with a final
// block of 0 would let a new payment's scan start at that head (and confirm
// nothing), and 0 cannot tell "none reported" from "block 0". Payments are
// kept as they are.
func TestMigrateDropsAHeadWithoutFinal(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "v11.db")
	s := openAt(t, path)
	p := core.Payment{ID: "pay_1", Status: core.StatusAwaiting, Amount: core.NewAmount(1), Asset: core.USDTBSC,
		Address: "0x0000000000000000000000000000000000000001", CreatedAt: time.Unix(1, 0), CreatedAtBlock: 49_984}
	head := core.ChainHead{ChainID: 56, Number: 49_984, Hash: "0x" + strings.Repeat("ab", 32), Time: time.Unix(2, 0)}
	if err := s.Update(ctx, func(tx core.Tx) error {
		if err := tx.InsertPayment(ctx, p); err != nil {
			return err
		}
		return tx.SaveChainHead(ctx, head)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"ALTER TABLE chain_head DROP COLUMN final_time",
		"ALTER TABLE payments DROP COLUMN oldest_pending_block",
		"PRAGMA user_version=11",
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s = openAt(t, path)
	err = s.View(ctx, func(r core.Reader) error {
		if got, err := r.GetChainHead(ctx); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("head after the upgrade: %+v, %v; want none", got, err)
		}
		got, err := r.GetPayment(ctx, "pay_1")
		if err != nil || got.CreatedAtBlock != 49_984 || got.Status != core.StatusAwaiting {
			t.Errorf("payment after the upgrade: %+v, %v", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The next head is stored with its final block.
	head.Number, head.Final = 50_000, 49_998
	head.Hash = "0x" + strings.Repeat("cd", 32)
	if err := s.Update(ctx, func(tx core.Tx) error { return tx.SaveChainHead(ctx, head) }); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(r core.Reader) error {
		got, err := r.GetChainHead(ctx)
		if err != nil || got.Final != 49_998 {
			t.Errorf("head stored after the upgrade: %+v, %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
