package coretest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// RunStoreSuite checks a [core.Store] implementation against the store
// contract documented on [core.Store] and its sub-interfaces. newStore
// must return a new, empty store on every call (use t.TempDir and
// t.Cleanup for files and handles).
//
// Run it from the implementation's tests, with the race detector:
//
//	func TestStore(t *testing.T) {
//		coretest.RunStoreSuite(t, func(t *testing.T) core.Store { return openTestDB(t) })
//	}
func RunStoreSuite(t *testing.T, newStore func(t *testing.T) core.Store) {
	t.Helper()
	tests := []struct {
		name string
		run  func(t *testing.T, s core.Store)
	}{
		{"Empty", storeEmpty},
		{"RoundTrip", storeRoundTrip},
		{"EmptyCollections", storeEmptyCollections},
		{"CopySemantics", storeCopySemantics},
		{"Rollback", storeRollback},
		{"RollbackOnPanic", storeRollbackOnPanic},
		{"ReadYourWrites", storeReadYourWrites},
		{"Uniqueness", storeUniqueness},
		{"UpdateMissing", storeUpdateMissing},
		{"AddressIndexAllocation", storeAddressIndex},
		{"IdempotencyKeyRace", storeIdempotencyRace},
		{"EventLog", storeEventLog},
		{"EventLogConcurrent", storeEventLogConcurrent},
		{"Cursors", storeCursors},
		{"PaymentFilters", storePaymentFilters},
		{"PaymentSortAndPaging", storePaymentSortPaging},
		{"Transfers", storeTransfers},
		{"Links", storeLinks},
		{"WebhookEndpoints", storeEndpoints},
		{"Deliveries", storeDeliveries},
		{"DeliveryFirstAttempts", storeDeliveryFirstAttempts},
		{"APIKeys", storeAPIKeys},
		{"Sessions", storeSessions},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newStore(t)) })
	}
}

var bg = context.Background()

// at returns a fixed timestamp n minutes after Epoch.
func at(n int) time.Time { return Epoch.Add(time.Duration(n) * time.Minute) }

func update(t *testing.T, s core.Store, fn func(tx core.Tx) error) {
	t.Helper()
	if err := s.Update(bg, fn); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func view(t *testing.T, s core.Store, fn func(r core.Reader) error) {
	t.Helper()
	if err := s.View(bg, fn); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func wantErr(t *testing.T, err, target error, what string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: got error %v, want one matching %v", what, err, target)
	}
}

func wantEqual[T any](t *testing.T, got, want T, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got  %+v\n want %+v", what, got, want)
	}
}

func units(n int64) core.Amount {
	a, _ := core.AmountFromBig(new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))
	return a
}

// ---- fixtures: every field set to a distinctive non-zero value ----

func fullSettings() core.Settings {
	return core.Settings{
		SetupCompleted: true,
		PublicURL:      "https://pay.example.com",
		Merchant:       core.MerchantSettings{Name: "Northwind", LogoURL: "https://example.com/logo.png", SupportEmail: "help@example.com", SupportURL: "https://example.com/help"},
		Payments:       core.PaymentSettings{TTL: 45 * time.Minute, RequiredConfirmations: 12, UnderpaymentTolerance: "0.50", DefaultRedirectURL: "https://example.com/thanks"},
		Network:        core.NetworkSettings{ChainID: 97, RPCURLs: []string{"https://rpc-1.example.com", "wss://rpc-2.example.com"}},
		Asset:          core.USDTBSCTestnet,
		Wallet:         core.WalletSettings{XPub: XPub, DerivationPath: "m/44'/60'/0'/0", NextAddressIndex: 1287},
		UpdatedAt:      at(1),
	}
}

func fullPayment(id string) core.Payment {
	return core.Payment{
		ID:                    id,
		Status:                core.StatusUnderpaid,
		Amount:                units(149),
		Asset:                 core.USDTBSC,
		Address:               Address("deposit", id),
		AddressIndex:          41,
		OrderID:               "ORD-" + id,
		Description:           "Pro plan — annual",
		CustomerEmail:         "maria@example.com",
		RedirectURL:           "https://shop.example.com/thanks",
		Metadata:              map[string]string{"customer_id": "cus_4821", "plan": "pro"},
		LinkID:                "plink_x",
		IdempotencyKey:        "idem-" + id,
		RequiredConfirmations: 15,
		Tolerance:             core.NewAmount(500),
		AmountReceived:        units(120),
		AmountConfirmed:       units(100),
		TxHash:                Hash("tx", id),
		FromAddress:           Address("payer", id),
		PendingBlock:          46_218_904,
		CreatedAtBlock:        46_218_000,
		Attention:             core.AttentionUnderpaid,
		Resolution:            &core.Resolution{At: at(50), By: "owner@example.com", Note: "Refunded."},
		CreatedAt:             at(1),
		ExpiresAt:             at(31),
		UpdatedAt:             at(40),
		PaidAt:                at(35),
		SettledAt:             at(36),
		CancelledAt:           at(37),
		CancelledBy:           "owner@example.com",
		LateFundsMark:         units(97),
		WatchUntil:            at(10_000),
		RetiredAt:             at(10_100),
		RetiredUnchecked:      true,
	}
}

func fullTransfer(paymentID string, n int) core.Transfer {
	return core.Transfer{
		TxHash:      Hash("tx", paymentID, n),
		LogIndex:    uint(n),
		BlockHash:   BlockHash(uint64(1000+n), 0),
		PaymentID:   paymentID,
		BlockNumber: uint64(1000 + n),
		BlockTime:   at(2),
		From:        Address("payer", paymentID, n),
		To:          Address("deposit", paymentID),
		Amount:      units(int64(10 + n)),
		Status:      core.TransferConfirmed,
		SeenAt:      at(3),
		NotBefore:   at(1),
		ConfirmedAt: at(4),
		OrphanedAt:  at(5),
	}
}

func fullLink(id, slug string) core.PaymentLink {
	return core.PaymentLink{
		ID: id, Slug: slug, Name: "Conference ticket", Description: "VIP pass",
		Amount: units(450), MinAmount: units(5), Asset: core.USDTBSC, Active: true, MaxUses: 50, ExpiresAt: at(9000),
		RedirectURL: "https://example.com/done", Uses: 38, TotalCollected: units(17100),
		LastPaidAt: at(70), CreatedAt: at(1), UpdatedAt: at(71),
	}
}

func fullEndpoint(id string) core.WebhookEndpoint {
	return core.WebhookEndpoint{
		ID: id, URL: "https://shop.example.com/hook", Description: "Production",
		Events: []core.EventType{core.EventPaymentPaid, core.EventPaymentExpired}, Enabled: true,
		Secret: "whsec_current", PreviousSecret: "whsec_previous", PreviousSecretExpiresAt: at(1500),
		DisabledAt: at(20), DisabledReason: "manual", FailingSince: at(30), ConsecutiveFailures: 7,
		LastDeliveryAt: at(40), LastSuccessAt: at(25), CreatedAt: at(1), UpdatedAt: at(41),
	}
}

func fullDelivery(id, endpointID string) core.WebhookDelivery {
	return core.WebhookDelivery{
		ID: id, EndpointID: endpointID, EventID: "evt_" + id, EventSeq: 41, EventType: core.EventPaymentPaid,
		PaymentID: "pay_x", URL: "https://shop.example.com/hook", Status: core.DeliveryRetrying,
		Attempt: 2, MaxAttempts: 8, NextAttemptAt: at(60), ClaimID: "whc_" + id,
		RequestBody:  `{"id":"evt_1","type":"payment.paid","data":{"amount":"149.00"}}`,
		ResponseCode: 503, ResponseBody: "upstream \"down\"\n", DurationMs: 812, Error: "",
		Attempts: []core.DeliveryAttempt{
			{N: 1, At: at(50), ResponseCode: 0, DurationMs: 10_000, Error: "timeout", Succeeded: false},
			{N: 1, At: at(52), ResponseCode: 200, DurationMs: 31_000, Succeeded: true, Late: true},
			{N: 2, At: at(55), ResponseCode: 503, DurationMs: 812},
		},
		CreatedAt: at(49), UpdatedAt: at(55), CompletedAt: at(56),
	}
}

func fullKey(id string) core.APIKey {
	return core.APIKey{
		ID: id, Name: "Production backend", Mode: "live", Prefix: "hmn_live_3fa9", Last4: "c2d1",
		SecretHash: Hash("secret", id), Scopes: []core.Scope{core.ScopePaymentsRead, core.ScopePaymentsWrite},
		CreatedAt: at(1), LastUsedAt: at(90), ExpiresAt: at(100_000), RevokedAt: at(95),
	}
}

func fullEvent(id, paymentID string) core.Event {
	return core.Event{
		ID: id, Type: core.EventPaymentPaid, PaymentID: paymentID, CreatedAt: at(7),
		Data: json.RawMessage(`{"id":"` + paymentID + `","amount":"149.00","note":"ünïcode \"quoted\""}`),
	}
}

// ---- tests ----

func storeEmpty(t *testing.T, s core.Store) {
	view(t, s, func(r core.Reader) error {
		_, err := r.GetSettings(bg)
		wantErr(t, err, core.ErrNotFound, "GetSettings")
		_, err = r.GetChainHead(bg)
		wantErr(t, err, core.ErrNotFound, "GetChainHead")
		_, err = r.GetOperator(bg)
		wantErr(t, err, core.ErrNotFound, "GetOperator")
		_, err = r.GetPayment(bg, "pay_missing")
		wantErr(t, err, core.ErrNotFound, "GetPayment")
		_, err = r.GetPaymentByAddress(bg, Address("nobody"))
		wantErr(t, err, core.ErrNotFound, "GetPaymentByAddress")
		_, err = r.GetPaymentByIdempotencyKey(bg, "nope")
		wantErr(t, err, core.ErrNotFound, "GetPaymentByIdempotencyKey")
		_, err = r.GetPaymentByIdempotencyKey(bg, "")
		wantErr(t, err, core.ErrNotFound, "GetPaymentByIdempotencyKey(\"\")")
		_, err = r.GetTransfer(bg, core.TransferKey{TxHash: Hash("x"), BlockHash: Hash("y")})
		wantErr(t, err, core.ErrNotFound, "GetTransfer")
		_, err = r.GetLink(bg, "plink_missing")
		wantErr(t, err, core.ErrNotFound, "GetLink")
		_, err = r.GetLinkBySlug(bg, "missing")
		wantErr(t, err, core.ErrNotFound, "GetLinkBySlug")
		_, err = r.GetWebhookEndpoint(bg, "we_missing")
		wantErr(t, err, core.ErrNotFound, "GetWebhookEndpoint")
		_, err = r.GetDelivery(bg, "whd_missing")
		wantErr(t, err, core.ErrNotFound, "GetDelivery")
		_, err = r.GetAPIKey(bg, "key_missing")
		wantErr(t, err, core.ErrNotFound, "GetAPIKey")
		_, err = r.GetAPIKeyByHash(bg, "nope")
		wantErr(t, err, core.ErrNotFound, "GetAPIKeyByHash")
		_, err = r.GetSessionByTokenHash(bg, "nope")
		wantErr(t, err, core.ErrNotFound, "GetSessionByTokenHash")

		payments, err := r.ListPayments(bg, core.PaymentFilter{}, core.PaymentSort{}, core.Page{})
		if err != nil || len(payments) != 0 {
			t.Errorf("ListPayments on empty store = %v, %v", payments, err)
		}
		n, err := r.CountPayments(bg, core.PaymentFilter{})
		if err != nil || n != 0 {
			t.Errorf("CountPayments on empty store = %d, %v", n, err)
		}
		events, err := r.ListEvents(bg, core.EventFilter{})
		if err != nil || len(events) != 0 {
			t.Errorf("ListEvents on empty store = %v, %v", events, err)
		}
		cur, err := r.GetCursor(bg, "nobody")
		if err != nil || cur != 0 {
			t.Errorf("GetCursor of unknown consumer = %d, %v; want 0, nil", cur, err)
		}
		pending, err := r.ListPendingTransfers(bg)
		if err != nil || len(pending) != 0 {
			t.Errorf("ListPendingTransfers on empty store = %v, %v", pending, err)
		}
		return nil
	})
}

func storeRoundTrip(t *testing.T, s core.Store) {
	settings := fullSettings()
	head := core.ChainHead{ChainID: 97, Number: 46_218_904, Hash: BlockHash(46_218_904, 0), Time: at(3), Checked: at(2), Final: 46_218_890, ObservedAt: at(4)}
	operator := core.Operator{ID: "op_1", Email: "owner@example.com", PasswordHash: "$pbkdf2-sha256$i=1$c2FsdA$aGFzaA", CreatedAt: at(1), UpdatedAt: at(2),
		Generation: 7, RecoveryHash: Hash("recovery code"), RecoveryExpiresAt: at(900), RecoveryAttempts: 2}
	session := core.Session{ID: "sess_1", OperatorID: "op_1", TokenHash: Hash("token"), UserAgent: "Mozilla/5.0", IP: "203.0.113.7", CreatedAt: at(1), ExpiresAt: at(20_000), LastSeenAt: at(77)}
	payment := fullPayment("pay_1")
	transfer := fullTransfer("pay_1", 3)
	link := fullLink("plink_1", "devcon-vip")
	endpoint := fullEndpoint("we_1")
	delivery := fullDelivery("whd_1", "we_1")
	key := fullKey("key_1")
	event := fullEvent("evt_1", "pay_1")

	update(t, s, func(tx core.Tx) error {
		return errors.Join(
			tx.SaveSettings(bg, settings),
			tx.SaveChainHead(bg, head),
			tx.SaveOperator(bg, operator),
			tx.InsertSession(bg, session),
			tx.InsertPayment(bg, payment),
			tx.InsertTransfer(bg, transfer),
			tx.InsertLink(bg, link),
			tx.InsertWebhookEndpoint(bg, endpoint),
			tx.InsertDelivery(bg, delivery),
			tx.InsertAPIKey(bg, key),
			tx.AppendEvent(bg, &event),
		)
	})
	if event.Seq <= 0 {
		t.Errorf("AppendEvent assigned Seq %d, want > 0", event.Seq)
	}

	check := func(r core.Reader) error {
		gotSettings, err := r.GetSettings(bg)
		wantErr(t, err, nil, "GetSettings")
		wantEqual(t, gotSettings, settings, "Settings")
		gotHead, err := r.GetChainHead(bg)
		wantErr(t, err, nil, "GetChainHead")
		wantEqual(t, gotHead, head, "ChainHead")
		gotOp, err := r.GetOperator(bg)
		wantErr(t, err, nil, "GetOperator")
		wantEqual(t, gotOp, operator, "Operator")
		gotSess, err := r.GetSessionByTokenHash(bg, session.TokenHash)
		wantErr(t, err, nil, "GetSessionByTokenHash")
		wantEqual(t, gotSess, session, "Session")

		gotPay, err := r.GetPayment(bg, payment.ID)
		wantErr(t, err, nil, "GetPayment")
		wantEqual(t, gotPay, payment, "Payment by id")
		gotPay, err = r.GetPaymentByAddress(bg, payment.Address)
		wantErr(t, err, nil, "GetPaymentByAddress")
		wantEqual(t, gotPay, payment, "Payment by address")
		gotPay, err = r.GetPaymentByIdempotencyKey(bg, payment.IdempotencyKey)
		wantErr(t, err, nil, "GetPaymentByIdempotencyKey")
		wantEqual(t, gotPay, payment, "Payment by idempotency key")
		list, err := r.ListPayments(bg, core.PaymentFilter{}, core.PaymentSort{}, core.Page{})
		wantErr(t, err, nil, "ListPayments")
		wantEqual(t, list, []core.Payment{payment}, "Payment via list")

		gotTr, err := r.GetTransfer(bg, transfer.Key())
		wantErr(t, err, nil, "GetTransfer")
		wantEqual(t, gotTr, transfer, "Transfer")
		gotLink, err := r.GetLink(bg, link.ID)
		wantErr(t, err, nil, "GetLink")
		wantEqual(t, gotLink, link, "Link")
		gotEp, err := r.GetWebhookEndpoint(bg, endpoint.ID)
		wantErr(t, err, nil, "GetWebhookEndpoint")
		wantEqual(t, gotEp, endpoint, "WebhookEndpoint")
		gotDel, err := r.GetDelivery(bg, delivery.ID)
		wantErr(t, err, nil, "GetDelivery")
		wantEqual(t, gotDel, delivery, "WebhookDelivery")
		gotKey, err := r.GetAPIKey(bg, key.ID)
		wantErr(t, err, nil, "GetAPIKey")
		wantEqual(t, gotKey, key, "APIKey")
		events, err := r.ListEvents(bg, core.EventFilter{})
		wantErr(t, err, nil, "ListEvents")
		wantEqual(t, events, []core.Event{event}, "Event (Data must round-trip byte for byte)")
		return nil
	}
	view(t, s, check)

	// The singletons are replaced by a second save.
	settings.Merchant.Name = "Renamed"
	settings.Network.RPCURLs = []string{"https://only.example.com"}
	head.Number++
	operator.Email = "new@example.com"
	update(t, s, func(tx core.Tx) error {
		return errors.Join(tx.SaveSettings(bg, settings), tx.SaveChainHead(bg, head), tx.SaveOperator(bg, operator))
	})
	view(t, s, check)
}

func storeEmptyCollections(t *testing.T, s core.Store) {
	p := fullPayment("pay_1")
	p.Metadata = nil
	p.Resolution = nil
	p.PaidAt, p.SettledAt, p.CancelledAt = time.Time{}, time.Time{}, time.Time{}
	e := fullEndpoint("we_1")
	e.Events = nil
	e.PreviousSecretExpiresAt = time.Time{}
	d := fullDelivery("whd_1", "we_1")
	d.Attempts = nil
	k := fullKey("key_1")
	k.Scopes = nil
	st := fullSettings()
	st.Network.RPCURLs = nil
	ev := fullEvent("evt_1", "")
	update(t, s, func(tx core.Tx) error {
		return errors.Join(tx.InsertPayment(bg, p), tx.InsertWebhookEndpoint(bg, e), tx.InsertDelivery(bg, d),
			tx.InsertAPIKey(bg, k), tx.SaveSettings(bg, st), tx.AppendEvent(bg, &ev))
	})
	view(t, s, func(r core.Reader) error {
		gp, _ := r.GetPayment(bg, p.ID)
		if gp.Metadata == nil || len(gp.Metadata) != 0 {
			t.Errorf("Payment.Metadata = %#v, want empty non-nil map", gp.Metadata)
		}
		if gp.Resolution != nil {
			t.Errorf("Payment.Resolution = %+v, want nil", gp.Resolution)
		}
		if !gp.PaidAt.IsZero() || !gp.SettledAt.IsZero() || !gp.CancelledAt.IsZero() {
			t.Errorf("zero times must stay zero: PaidAt=%v SettledAt=%v CancelledAt=%v", gp.PaidAt, gp.SettledAt, gp.CancelledAt)
		}
		ge, _ := r.GetWebhookEndpoint(bg, e.ID)
		if len(ge.Events) != 0 || !ge.PreviousSecretExpiresAt.IsZero() {
			t.Errorf("endpoint: Events=%v PreviousSecretExpiresAt=%v", ge.Events, ge.PreviousSecretExpiresAt)
		}
		gd, _ := r.GetDelivery(bg, d.ID)
		if len(gd.Attempts) != 0 {
			t.Errorf("delivery Attempts = %v, want empty", gd.Attempts)
		}
		gk, _ := r.GetAPIKey(bg, k.ID)
		if len(gk.Scopes) != 0 {
			t.Errorf("key Scopes = %v, want empty", gk.Scopes)
		}
		gs, _ := r.GetSettings(bg)
		if len(gs.Network.RPCURLs) != 0 {
			t.Errorf("settings RPCURLs = %v, want empty", gs.Network.RPCURLs)
		}
		evs, _ := r.ListEvents(bg, core.EventFilter{})
		if len(evs) != 1 || evs[0].PaymentID != "" {
			t.Errorf("event without payment: %+v", evs)
		}
		return nil
	})
}

func storeCopySemantics(t *testing.T, s core.Store) {
	p := fullPayment("pay_1")
	update(t, s, func(tx core.Tx) error { return tx.InsertPayment(bg, p) })
	p.Metadata["plan"] = "mutated-after-insert"
	p.Resolution.Note = "mutated-after-insert"
	var got core.Payment
	view(t, s, func(r core.Reader) error {
		var err error
		got, err = r.GetPayment(bg, "pay_1")
		return err
	})
	if got.Metadata["plan"] != "pro" || got.Resolution.Note != "Refunded." {
		t.Errorf("store kept a reference to the inserted value: %+v / %+v", got.Metadata, got.Resolution)
	}
	got.Metadata["plan"] = "mutated-after-read"
	got.Resolution.Note = "mutated-after-read"
	view(t, s, func(r core.Reader) error {
		again, err := r.GetPayment(bg, "pay_1")
		if again.Metadata["plan"] != "pro" || again.Resolution.Note != "Refunded." {
			t.Errorf("store handed out a reference to its own value: %+v / %+v", again.Metadata, again.Resolution)
		}
		return err
	})
}

func storeRollback(t *testing.T, s core.Store) {
	base := fullPayment("pay_base")
	update(t, s, func(tx core.Tx) error {
		return errors.Join(tx.InsertPayment(bg, base), tx.SaveSettings(bg, fullSettings()), tx.SetCursor(bg, "c", 5))
	})

	boom := errors.New("boom")
	err := s.Update(bg, func(tx core.Tx) error {
		changed := base
		changed.Status = core.StatusPaid
		st := fullSettings()
		st.Wallet.NextAddressIndex = 9999
		ev := fullEvent("evt_rolled_back", "pay_base")
		if err := errors.Join(
			tx.UpdatePayment(bg, changed),
			tx.InsertPayment(bg, fullPayment("pay_new")),
			tx.InsertTransfer(bg, fullTransfer("pay_new", 1)),
			tx.InsertLink(bg, fullLink("plink_1", "slug")),
			tx.InsertWebhookEndpoint(bg, fullEndpoint("we_1")),
			tx.InsertDelivery(bg, fullDelivery("whd_1", "we_1")),
			tx.InsertAPIKey(bg, fullKey("key_1")),
			tx.SaveSettings(bg, st),
			tx.SaveChainHead(bg, core.ChainHead{ChainID: 56, Number: 1, Hash: Hash("h"), Time: at(1)}),
			tx.SaveOperator(bg, core.Operator{ID: "op_1", Email: "a@b.c"}),
			tx.InsertSession(bg, core.Session{ID: "sess_1", TokenHash: "h", ExpiresAt: at(9)}),
			tx.AppendEvent(bg, &ev),
			tx.SetCursor(bg, "c", 77),
		); err != nil {
			t.Errorf("writes inside the transaction failed: %v", err)
		}
		return boom
	})
	if err != boom {
		t.Errorf("Update must return fn's error unchanged: got %v", err)
	}

	view(t, s, func(r core.Reader) error {
		got, err := r.GetPayment(bg, "pay_base")
		wantErr(t, err, nil, "GetPayment after rollback")
		wantEqual(t, got, base, "payment after rollback")
		_, err = r.GetPayment(bg, "pay_new")
		wantErr(t, err, core.ErrNotFound, "rolled-back payment")
		_, err = r.GetTransfer(bg, fullTransfer("pay_new", 1).Key())
		wantErr(t, err, core.ErrNotFound, "rolled-back transfer")
		_, err = r.GetLink(bg, "plink_1")
		wantErr(t, err, core.ErrNotFound, "rolled-back link")
		_, err = r.GetWebhookEndpoint(bg, "we_1")
		wantErr(t, err, core.ErrNotFound, "rolled-back endpoint")
		_, err = r.GetDelivery(bg, "whd_1")
		wantErr(t, err, core.ErrNotFound, "rolled-back delivery")
		_, err = r.GetAPIKey(bg, "key_1")
		wantErr(t, err, core.ErrNotFound, "rolled-back api key")
		_, err = r.GetChainHead(bg)
		wantErr(t, err, core.ErrNotFound, "rolled-back chain head")
		_, err = r.GetOperator(bg)
		wantErr(t, err, core.ErrNotFound, "rolled-back operator")
		_, err = r.GetSessionByTokenHash(bg, "h")
		wantErr(t, err, core.ErrNotFound, "rolled-back session")
		st, _ := r.GetSettings(bg)
		if st.Wallet.NextAddressIndex != 1287 {
			t.Errorf("settings after rollback: NextAddressIndex = %d, want 1287", st.Wallet.NextAddressIndex)
		}
		events, _ := r.ListEvents(bg, core.EventFilter{})
		if len(events) != 0 {
			t.Errorf("rolled-back event is visible: %+v", events)
		}
		cur, _ := r.GetCursor(bg, "c")
		if cur != 5 {
			t.Errorf("cursor after rollback = %d, want 5", cur)
		}
		return nil
	})

	// The store keeps working after a rollback.
	update(t, s, func(tx core.Tx) error { return tx.InsertPayment(bg, fullPayment("pay_new")) })
}

func storeRollbackOnPanic(t *testing.T, s core.Store) {
	func() {
		defer func() {
			if r := recover(); r != "kaboom" {
				t.Errorf("Update must re-raise fn's panic, recovered %v", r)
			}
		}()
		_ = s.Update(bg, func(tx core.Tx) error {
			if err := tx.InsertPayment(bg, fullPayment("pay_1")); err != nil {
				t.Errorf("InsertPayment: %v", err)
			}
			panic("kaboom")
		})
		t.Error("Update returned instead of panicking")
	}()
	view(t, s, func(r core.Reader) error {
		_, err := r.GetPayment(bg, "pay_1")
		wantErr(t, err, core.ErrNotFound, "payment written before a panic")
		return nil
	})
	// The store is not left locked.
	update(t, s, func(tx core.Tx) error { return tx.InsertPayment(bg, fullPayment("pay_2")) })
}

func storeReadYourWrites(t *testing.T, s core.Store) {
	update(t, s, func(tx core.Tx) error {
		p := fullPayment("pay_1")
		p.Status = core.StatusAwaiting
		if err := tx.InsertPayment(bg, p); err != nil {
			return err
		}
		got, err := tx.GetPayment(bg, "pay_1")
		wantErr(t, err, nil, "read own insert")
		wantEqual(t, got, p, "own insert")
		p.Status = core.StatusPaid
		if err := tx.UpdatePayment(bg, p); err != nil {
			return err
		}
		n, err := tx.CountPayments(bg, core.PaymentFilter{Statuses: []core.PaymentStatus{core.StatusPaid}})
		if err != nil || n != 1 {
			t.Errorf("count inside tx after update = %d, %v; want 1", n, err)
		}
		ev := fullEvent("evt_1", "pay_1")
		if err := tx.AppendEvent(bg, &ev); err != nil {
			return err
		}
		events, err := tx.ListEvents(bg, core.EventFilter{})
		if err != nil || len(events) != 1 {
			t.Errorf("own event not visible inside tx: %v, %v", events, err)
		}
		if err := tx.SetCursor(bg, "c", ev.Seq); err != nil {
			return err
		}
		cur, _ := tx.GetCursor(bg, "c")
		if cur != ev.Seq {
			t.Errorf("own cursor = %d, want %d", cur, ev.Seq)
		}
		st := fullSettings()
		if err := tx.SaveSettings(bg, st); err != nil {
			return err
		}
		got2, err := tx.GetSettings(bg)
		wantErr(t, err, nil, "read own settings")
		wantEqual(t, got2, st, "own settings")
		return nil
	})
}

func storeUniqueness(t *testing.T, s core.Store) {
	p := fullPayment("pay_1")
	tr := fullTransfer("pay_1", 1)
	l := fullLink("plink_1", "slug-a")
	e := fullEndpoint("we_1")
	d := fullDelivery("whd_1", "we_1")
	k := fullKey("key_1")
	sess := core.Session{ID: "sess_1", OperatorID: "op_1", TokenHash: "hash-1", CreatedAt: at(1), ExpiresAt: at(9)}
	ev := fullEvent("evt_1", "pay_1")
	update(t, s, func(tx core.Tx) error {
		return errors.Join(tx.InsertPayment(bg, p), tx.InsertTransfer(bg, tr), tx.InsertLink(bg, l),
			tx.InsertWebhookEndpoint(bg, e), tx.InsertDelivery(bg, d), tx.InsertAPIKey(bg, k),
			tx.InsertSession(bg, sess), tx.AppendEvent(bg, &ev))
	})

	conflicts := []struct {
		name string
		fn   func(tx core.Tx) error
	}{
		{"payment id", func(tx core.Tx) error {
			q := fullPayment("pay_other")
			q.ID = p.ID
			return tx.InsertPayment(bg, q)
		}},
		{"payment address", func(tx core.Tx) error {
			q := fullPayment("pay_other")
			q.Address = p.Address
			return tx.InsertPayment(bg, q)
		}},
		{"payment idempotency key", func(tx core.Tx) error {
			q := fullPayment("pay_other")
			q.IdempotencyKey = p.IdempotencyKey
			return tx.InsertPayment(bg, q)
		}},
		{"transfer key", func(tx core.Tx) error {
			q := fullTransfer("pay_other", 9)
			q.TxHash, q.LogIndex, q.BlockHash = tr.TxHash, tr.LogIndex, tr.BlockHash
			return tx.InsertTransfer(bg, q)
		}},
		{"link id", func(tx core.Tx) error { return tx.InsertLink(bg, fullLink("plink_1", "slug-b")) }},
		{"link slug", func(tx core.Tx) error { return tx.InsertLink(bg, fullLink("plink_2", "slug-a")) }},
		{"endpoint id", func(tx core.Tx) error { return tx.InsertWebhookEndpoint(bg, fullEndpoint("we_1")) }},
		{"delivery id", func(tx core.Tx) error { return tx.InsertDelivery(bg, fullDelivery("whd_1", "we_1")) }},
		{"api key id", func(tx core.Tx) error {
			q := fullKey("key_other")
			q.ID = k.ID
			return tx.InsertAPIKey(bg, q)
		}},
		{"api key hash", func(tx core.Tx) error {
			q := fullKey("key_other")
			q.SecretHash = k.SecretHash
			return tx.InsertAPIKey(bg, q)
		}},
		{"session id", func(tx core.Tx) error {
			return tx.InsertSession(bg, core.Session{ID: "sess_1", TokenHash: "hash-2", ExpiresAt: at(9)})
		}},
		{"session token hash", func(tx core.Tx) error {
			return tx.InsertSession(bg, core.Session{ID: "sess_2", TokenHash: "hash-1", ExpiresAt: at(9)})
		}},
		{"event id", func(tx core.Tx) error {
			dup := fullEvent("evt_1", "pay_1")
			return tx.AppendEvent(bg, &dup)
		}},
	}
	for _, c := range conflicts {
		err := s.Update(bg, c.fn)
		wantErr(t, err, core.ErrConflict, "duplicate "+c.name)
	}

	// What is allowed to repeat.
	update(t, s, func(tx core.Tx) error {
		a, b := fullPayment("pay_a"), fullPayment("pay_b")
		a.IdempotencyKey, b.IdempotencyKey = "", ""
		a.OrderID, b.OrderID = p.OrderID, p.OrderID
		// The same transaction and log index in another block is another
		// transfer (re-inclusion after a reorganisation).
		reincluded := tr
		reincluded.BlockHash = BlockHash(2000, 1)
		// Different logs of one transaction in one block.
		sibling := tr
		sibling.LogIndex++
		return errors.Join(tx.InsertPayment(bg, a), tx.InsertPayment(bg, b),
			tx.InsertTransfer(bg, reincluded), tx.InsertTransfer(bg, sibling))
	})
}

func storeUpdateMissing(t *testing.T, s core.Store) {
	checks := []struct {
		name string
		fn   func(tx core.Tx) error
	}{
		{"UpdatePayment", func(tx core.Tx) error { return tx.UpdatePayment(bg, fullPayment("pay_missing")) }},
		{"UpdateTransfer", func(tx core.Tx) error { return tx.UpdateTransfer(bg, fullTransfer("pay_missing", 1)) }},
		{"UpdateLink", func(tx core.Tx) error { return tx.UpdateLink(bg, fullLink("plink_missing", "s")) }},
		{"DeleteLink", func(tx core.Tx) error { return tx.DeleteLink(bg, "plink_missing") }},
		{"UpdateSession", func(tx core.Tx) error { return tx.UpdateSession(bg, core.Session{ID: "sess_missing"}) }},
		{"UpdateWebhookEndpoint", func(tx core.Tx) error { return tx.UpdateWebhookEndpoint(bg, fullEndpoint("we_missing")) }},
		{"DeleteWebhookEndpoint", func(tx core.Tx) error { return tx.DeleteWebhookEndpoint(bg, "we_missing") }},
		{"UpdateDelivery", func(tx core.Tx) error { return tx.UpdateDelivery(bg, fullDelivery("whd_missing", "we_1")) }},
		{"UpdateAPIKey", func(tx core.Tx) error { return tx.UpdateAPIKey(bg, fullKey("key_missing")) }},
	}
	for _, c := range checks {
		wantErr(t, s.Update(bg, c.fn), core.ErrNotFound, c.name+" of a missing object")
	}
	update(t, s, func(tx core.Tx) error { return tx.DeleteSession(bg, "sess_missing") })
	view(t, s, func(r core.Reader) error {
		n, _ := r.CountPayments(bg, core.PaymentFilter{})
		if n != 0 {
			t.Errorf("Update of a missing payment created it")
		}
		return nil
	})
}

// storeAddressIndex is the core's address allocation, verbatim: a
// read-modify-write of the settings inside Update. Serializable
// transactions must hand every payment a different index.
func storeAddressIndex(t *testing.T, s core.Store) {
	update(t, s, func(tx core.Tx) error {
		st := fullSettings()
		st.Wallet.NextAddressIndex = 0
		return tx.SaveSettings(bg, st)
	})
	const workers, perWorker = 8, 25
	var mu sync.Mutex
	seen := map[uint32]int{}
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				var index uint32
				err := s.Update(bg, func(tx core.Tx) error {
					st, err := tx.GetSettings(bg)
					if err != nil {
						return err
					}
					index = st.Wallet.NextAddressIndex
					st.Wallet.NextAddressIndex++
					if err := tx.SaveSettings(bg, st); err != nil {
						return err
					}
					p := fullPayment(fmt.Sprintf("pay_%d_%d", w, i))
					p.Address = Address("index", index)
					p.AddressIndex = index
					return tx.InsertPayment(bg, p)
				})
				if err != nil {
					t.Errorf("allocate: %v", err)
					return
				}
				mu.Lock()
				seen[index]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(seen) != workers*perWorker {
		t.Errorf("got %d distinct indexes for %d payments: transactions are not serializable", len(seen), workers*perWorker)
	}
	for i := range uint32(workers * perWorker) {
		if seen[i] != 1 {
			t.Errorf("index %d was handed out %d times", i, seen[i])
			break
		}
	}
	view(t, s, func(r core.Reader) error {
		st, _ := r.GetSettings(bg)
		if st.Wallet.NextAddressIndex != workers*perWorker {
			t.Errorf("NextAddressIndex = %d, want %d", st.Wallet.NextAddressIndex, workers*perWorker)
		}
		return nil
	})
}

// storeIdempotencyRace is the core's idempotent create: look the key up,
// insert if absent. Under concurrency exactly one payment may exist.
func storeIdempotencyRace(t *testing.T, s core.Store) {
	const workers = 16
	ids := make([]string, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			err := s.Update(bg, func(tx core.Tx) error {
				existing, err := tx.GetPaymentByIdempotencyKey(bg, "the-key")
				if err == nil {
					ids[w] = existing.ID
					return nil
				}
				if !errors.Is(err, core.ErrNotFound) {
					return err
				}
				p := fullPayment(fmt.Sprintf("pay_%d", w))
				p.IdempotencyKey = "the-key"
				ids[w] = p.ID
				return tx.InsertPayment(bg, p)
			})
			if err != nil {
				t.Errorf("idempotent create: %v", err)
			}
		})
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent creates with one idempotency key returned different payments: %v", ids)
		}
	}
	view(t, s, func(r core.Reader) error {
		n, _ := r.CountPayments(bg, core.PaymentFilter{})
		if n != 1 {
			t.Errorf("%d payments exist for one idempotency key", n)
		}
		return nil
	})
}

func storeEventLog(t *testing.T, s core.Store) {
	var seqs []int64
	appendEvent := func(id, paymentID string, typ core.EventType) {
		update(t, s, func(tx core.Tx) error {
			e := fullEvent(id, paymentID)
			e.Type = typ
			if err := tx.AppendEvent(bg, &e); err != nil {
				return err
			}
			seqs = append(seqs, e.Seq)
			return nil
		})
	}
	appendEvent("evt_1", "pay_a", core.EventPaymentCreated)
	appendEvent("evt_2", "pay_b", core.EventPaymentCreated)
	// A rolled-back append must leave no trace.
	_ = s.Update(bg, func(tx core.Tx) error {
		e := fullEvent("evt_ghost", "pay_a")
		_ = tx.AppendEvent(bg, &e)
		return errors.New("rollback")
	})
	appendEvent("evt_3", "pay_a", core.EventPaymentPaid)
	appendEvent("evt_4", "", core.EventSettingsUpdated)
	// Several events in one transaction keep their order.
	update(t, s, func(tx core.Tx) error {
		for _, id := range []string{"evt_5", "evt_6"} {
			e := fullEvent(id, "pay_b")
			e.Type = core.EventPaymentExpired
			if err := tx.AppendEvent(bg, &e); err != nil {
				return err
			}
			seqs = append(seqs, e.Seq)
		}
		return nil
	})

	for i, seq := range seqs {
		if seq <= 0 || (i > 0 && seq <= seqs[i-1]) {
			t.Fatalf("Seq must be positive and strictly increasing, got %v", seqs)
		}
	}

	ids := func(f core.EventFilter) []string {
		var out []string
		view(t, s, func(r core.Reader) error {
			events, err := r.ListEvents(bg, f)
			if err != nil {
				return err
			}
			for i, e := range events {
				if i > 0 && e.Seq <= events[i-1].Seq {
					t.Errorf("ListEvents(%+v) is not in ascending Seq order", f)
				}
				out = append(out, e.ID)
			}
			return nil
		})
		return out
	}
	all := []string{"evt_1", "evt_2", "evt_3", "evt_4", "evt_5", "evt_6"}
	wantEqual(t, ids(core.EventFilter{}), all, "all events")
	wantEqual(t, ids(core.EventFilter{After: seqs[1]}), all[2:], "After is exclusive")
	wantEqual(t, ids(core.EventFilter{After: seqs[5]}), nil, "After the last event")
	wantEqual(t, ids(core.EventFilter{Limit: 2}), all[:2], "Limit takes the oldest")
	wantEqual(t, ids(core.EventFilter{After: seqs[0], Limit: 3}), all[1:4], "After + Limit")
	wantEqual(t, ids(core.EventFilter{PaymentID: "pay_a"}), []string{"evt_1", "evt_3"}, "by payment")
	wantEqual(t, ids(core.EventFilter{PaymentID: "pay_b", After: seqs[1]}), []string{"evt_5", "evt_6"}, "by payment + After")
	wantEqual(t, ids(core.EventFilter{Types: []core.EventType{core.EventPaymentCreated, core.EventSettingsUpdated}}), []string{"evt_1", "evt_2", "evt_4"}, "by types")
	wantEqual(t, ids(core.EventFilter{Types: []core.EventType{core.EventPaymentExpired}, PaymentID: "pay_b", Limit: 1}), []string{"evt_5"}, "types + payment + limit")
	wantEqual(t, ids(core.EventFilter{PaymentID: "pay_missing"}), nil, "unknown payment")
}

// storeEventLogConcurrent checks the property cursors depend on: a reader
// that polls "everything after my cursor" while writers append never
// misses an event.
func storeEventLogConcurrent(t *testing.T, s core.Store) {
	const writers, perWriter = 6, 30
	var wg sync.WaitGroup
	done := make(chan struct{})
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				err := s.Update(bg, func(tx core.Tx) error {
					e := fullEvent(fmt.Sprintf("evt_%d_%d", w, i), "pay_x")
					return tx.AppendEvent(bg, &e)
				})
				if err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		})
	}
	go func() { wg.Wait(); close(done) }()

	seen := map[string]bool{}
	var cursor int64
	poll := func() {
		view(t, s, func(r core.Reader) error {
			events, err := r.ListEvents(bg, core.EventFilter{After: cursor, Limit: 7})
			if err != nil {
				return err
			}
			for _, e := range events {
				if e.Seq <= cursor {
					t.Errorf("event %s has Seq %d, not after cursor %d", e.ID, e.Seq, cursor)
				}
				if seen[e.ID] {
					t.Errorf("event %s delivered twice", e.ID)
				}
				seen[e.ID] = true
				cursor = e.Seq
			}
			return nil
		})
	}
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			poll()
		}
	}
	for before := -1; before != len(seen); {
		before = len(seen)
		poll()
	}
	if len(seen) != writers*perWriter {
		t.Errorf("cursor reader saw %d of %d events: Seq is not increasing in commit order", len(seen), writers*perWriter)
	}
}

func storeCursors(t *testing.T, s core.Store) {
	update(t, s, func(tx core.Tx) error {
		return errors.Join(tx.SetCursor(bg, "webhooks", 12), tx.SetCursor(bg, "telegram", 3))
	})
	update(t, s, func(tx core.Tx) error { return tx.SetCursor(bg, "webhooks", 40) })
	view(t, s, func(r core.Reader) error {
		a, err := r.GetCursor(bg, "webhooks")
		if err != nil || a != 40 {
			t.Errorf("cursor webhooks = %d, %v; want 40", a, err)
		}
		b, err := r.GetCursor(bg, "telegram")
		if err != nil || b != 3 {
			t.Errorf("cursor telegram = %d, %v; want 3", b, err)
		}
		return nil
	})
}

// paymentDataset is the fixture for filter, sort and paging tests.
//
//	id   status      created  amount (base units)  notes
//	p01  awaiting    1        9                    order ORD-1001, alice
//	p02  detected    2        10                   order ORD-1002, bob
//	p03  confirming  3        10^30
//	p04  paid        4        150e18               paid at 5, link A, has a transfer
//	p05  paid        6        150e18               paid at 7, link A
//	p06  overpaid    8        151e18               paid at 9, needs attention
//	p07  underpaid   10       100                  attention resolved
//	p08  expired     11       1000                 has an orphaned transfer
//	p09  expired     11       5                    late funds, needs attention
//	p10  failed      12       7                    chain 97, needs attention
//	p11  awaiting    13       8                    order ORD-1001 again
//	p12  awaiting    14       2^256-1              description with % and _
func paymentDataset(t *testing.T, s core.Store) {
	t.Helper()
	maxAmount, _ := core.AmountFromBig(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)))
	huge, _ := core.ParseBaseUnits("1" + strings.Repeat("0", 30))
	mk := func(id string, status core.PaymentStatus, created int, amount core.Amount, mod func(p *core.Payment)) core.Payment {
		p := core.Payment{
			ID: id, Status: status, Amount: amount, Asset: core.USDTBSC,
			Address: Address("deposit", id), OrderID: "ORD-" + id, Description: "Order " + id,
			Metadata: map[string]string{}, RequiredConfirmations: 15,
			CreatedAt: at(created), ExpiresAt: at(created + 30), UpdatedAt: at(created),
			WatchUntil: at(created + 100),
		}
		if mod != nil {
			mod(&p)
		}
		return p
	}
	payments := []core.Payment{
		mk("p01", core.StatusAwaiting, 1, core.NewAmount(9), func(p *core.Payment) {
			p.OrderID, p.CustomerEmail, p.Description = "ORD-1001", "alice@example.com", "Pro plan"
		}),
		mk("p02", core.StatusDetected, 2, core.NewAmount(10), func(p *core.Payment) {
			p.OrderID, p.CustomerEmail, p.Description = "ORD-1002", "bob@example.com", "Team Plan — 5 seats"
		}),
		mk("p03", core.StatusConfirming, 3, huge, nil),
		mk("p04", core.StatusPaid, 4, units(150), func(p *core.Payment) { p.PaidAt, p.LinkID = at(5), "plink_a" }),
		mk("p05", core.StatusPaid, 6, units(150), func(p *core.Payment) { p.PaidAt, p.LinkID, p.RetiredAt = at(7), "plink_a", at(150) }),
		mk("p06", core.StatusOverpaid, 8, units(151), func(p *core.Payment) {
			p.PaidAt, p.Attention = at(9), core.AttentionOverpaid
		}),
		mk("p07", core.StatusUnderpaid, 10, core.NewAmount(100), func(p *core.Payment) {
			p.Attention = core.AttentionUnderpaid
			p.Resolution = &core.Resolution{At: at(20), By: "owner", Note: "waived"}
		}),
		mk("p08", core.StatusExpired, 11, core.NewAmount(1000), func(p *core.Payment) { p.RetiredAt, p.RetiredUnchecked = at(200), true }),
		mk("p09", core.StatusExpired, 11, core.NewAmount(5), func(p *core.Payment) { p.Attention = core.AttentionLateFunds }),
		mk("p10", core.StatusFailed, 12, core.NewAmount(7), func(p *core.Payment) {
			p.Attention, p.Asset = core.AttentionFailed, core.USDTBSCTestnet
		}),
		mk("p11", core.StatusAwaiting, 13, core.NewAmount(8), func(p *core.Payment) { p.OrderID = "ORD-1001" }),
		mk("p12", core.StatusAwaiting, 14, maxAmount, func(p *core.Payment) { p.Description = "100% cotton_shirt" }),
	}
	live := core.Transfer{
		TxHash: Hash("tx", "p04"), LogIndex: 1, BlockHash: BlockHash(10, 0), PaymentID: "p04", BlockNumber: 10,
		From: Address("payer", "p04"), To: Address("deposit", "p04"), Amount: units(150), Status: core.TransferConfirmed, SeenAt: at(4),
	}
	orphan := core.Transfer{
		TxHash: Hash("tx", "p08"), LogIndex: 2, BlockHash: BlockHash(11, 1), PaymentID: "p08", BlockNumber: 11,
		From: Address("payer", "p08"), To: Address("deposit", "p08"), Amount: units(1), Status: core.TransferOrphaned, SeenAt: at(11), OrphanedAt: at(12),
	}
	update(t, s, func(tx core.Tx) error {
		// Insert in a scrambled order so no test passes by insertion order.
		for _, i := range []int{6, 0, 11, 3, 8, 1, 10, 5, 2, 9, 4, 7} {
			if err := tx.InsertPayment(bg, payments[i]); err != nil {
				return err
			}
		}
		return errors.Join(tx.InsertTransfer(bg, live), tx.InsertTransfer(bg, orphan))
	})
}

func listIDs(t *testing.T, s core.Store, f core.PaymentFilter, sort core.PaymentSort, page core.Page) []string {
	t.Helper()
	var ids []string
	view(t, s, func(r core.Reader) error {
		items, err := r.ListPayments(bg, f, sort, page)
		if err != nil {
			return err
		}
		for _, p := range items {
			ids = append(ids, p.ID)
		}
		return nil
	})
	return ids
}

func storePaymentFilters(t *testing.T, s core.Store) {
	paymentDataset(t, s)
	yes, no := true, false
	all := []string{"p01", "p02", "p03", "p04", "p05", "p06", "p07", "p08", "p09", "p10", "p11", "p12"}
	depositP03 := Address("deposit", "p03")
	txP04 := Hash("tx", "p04")

	tests := []struct {
		name   string
		filter core.PaymentFilter
		want   []string
	}{
		{"zero filter matches everything", core.PaymentFilter{}, all},

		{"one status", core.PaymentFilter{Statuses: []core.PaymentStatus{core.StatusPaid}}, []string{"p04", "p05"}},
		{"open statuses", core.PaymentFilter{Statuses: core.OpenStatuses}, []string{"p01", "p02", "p03", "p11", "p12"}},
		{"status without payments", core.PaymentFilter{Statuses: []core.PaymentStatus{core.StatusUnderpaid, core.StatusFailed}}, []string{"p07", "p10"}},

		{"needs attention", core.PaymentFilter{NeedsAttention: &yes}, []string{"p06", "p09", "p10"}},
		{"does not need attention (resolved counts as not)", core.PaymentFilter{NeedsAttention: &no}, []string{"p01", "p02", "p03", "p04", "p05", "p07", "p08", "p11", "p12"}},
		{"attention and status", core.PaymentFilter{NeedsAttention: &yes, Statuses: []core.PaymentStatus{core.StatusExpired}}, []string{"p09"}},

		{"search order id", core.PaymentFilter{Search: "ORD-1002"}, []string{"p02"}},
		{"search order id substring", core.PaymentFilter{Search: "1001"}, []string{"p01", "p11"}},
		{"search is case-insensitive", core.PaymentFilter{Search: "ord-1002"}, []string{"p02"}},
		{"search payment id", core.PaymentFilter{Search: "p07"}, []string{"p07"}},
		{"search email", core.PaymentFilter{Search: "ALICE@"}, []string{"p01"}},
		{"search description", core.PaymentFilter{Search: "team plan"}, []string{"p02"}},
		{"search non-ASCII description", core.PaymentFilter{Search: "— 5 seats"}, []string{"p02"}},
		{"search address", core.PaymentFilter{Search: depositP03}, []string{"p03"}},
		{"search address fragment, upper case", core.PaymentFilter{Search: strings.ToUpper(depositP03[2:20])}, []string{"p03"}},
		{"search tx hash of a transfer", core.PaymentFilter{Search: txP04}, []string{"p04"}},
		{"search tx hash fragment", core.PaymentFilter{Search: txP04[10:40]}, []string{"p04"}},
		{"search sender address of a transfer", core.PaymentFilter{Search: Address("payer", "p04")}, []string{"p04"}},
		{"search finds orphaned transfers too", core.PaymentFilter{Search: Hash("tx", "p08")}, []string{"p08"}},
		{"search without match", core.PaymentFilter{Search: "no-such-thing"}, nil},
		{"percent is a literal, not a wildcard", core.PaymentFilter{Search: "%"}, []string{"p12"}},
		{"underscore is a literal, not a wildcard", core.PaymentFilter{Search: "cotton_s"}, []string{"p12"}},
		{"wildcard-looking search without literal match", core.PaymentFilter{Search: "cotton%shirt"}, nil},
		{"search and status", core.PaymentFilter{Search: "1001", Statuses: []core.PaymentStatus{core.StatusAwaiting}}, []string{"p01", "p11"}},

		{"order id exact", core.PaymentFilter{OrderID: "ORD-1001"}, []string{"p01", "p11"}},
		{"order id is not a substring match", core.PaymentFilter{OrderID: "ORD-100"}, nil},
		{"link", core.PaymentFilter{LinkID: "plink_a"}, []string{"p04", "p05"}},
		{"from a link", core.PaymentFilter{FromLink: &yes}, []string{"p04", "p05"}},
		{"from a link and paid range", core.PaymentFilter{FromLink: &yes, PaidFrom: at(6)}, []string{"p05"}},
		{"not from a link excludes a link's payments", core.PaymentFilter{FromLink: &no, LinkID: "plink_a"}, nil},
		{"not from a link, by status", core.PaymentFilter{FromLink: &no, Statuses: []core.PaymentStatus{core.StatusPaid, core.StatusUnderpaid}}, []string{"p07"}},
		{"chain", core.PaymentFilter{ChainID: 97}, []string{"p10"}},
		{"chain and status", core.PaymentFilter{ChainID: 56, Statuses: []core.PaymentStatus{core.StatusExpired, core.StatusFailed}}, []string{"p08", "p09"}},

		{"created from is inclusive", core.PaymentFilter{CreatedFrom: at(12)}, []string{"p10", "p11", "p12"}},
		{"created to is exclusive", core.PaymentFilter{CreatedTo: at(3)}, []string{"p01", "p02"}},
		{"created range", core.PaymentFilter{CreatedFrom: at(4), CreatedTo: at(11)}, []string{"p04", "p05", "p06", "p07"}},
		{"created range, sub-minute bounds", core.PaymentFilter{CreatedFrom: at(4).Add(time.Millisecond), CreatedTo: at(11).Add(time.Millisecond)}, []string{"p05", "p06", "p07", "p08", "p09"}},
		{"empty created range", core.PaymentFilter{CreatedFrom: at(5), CreatedTo: at(5)}, nil},

		{"paid from excludes unpaid payments", core.PaymentFilter{PaidFrom: at(0)}, []string{"p04", "p05", "p06"}},
		{"paid to excludes unpaid payments", core.PaymentFilter{PaidTo: at(1000)}, []string{"p04", "p05", "p06"}},
		{"paid range: from inclusive, to exclusive", core.PaymentFilter{PaidFrom: at(5), PaidTo: at(9)}, []string{"p04", "p05"}},
		{"paid range and link", core.PaymentFilter{PaidFrom: at(6), LinkID: "plink_a"}, []string{"p05"}},

		{"expires before is inclusive", core.PaymentFilter{ExpiresBefore: at(33)}, []string{"p01", "p02", "p03"}},
		{"expires before and open", core.PaymentFilter{ExpiresBefore: at(100), Statuses: core.OpenStatuses}, []string{"p01", "p02", "p03", "p11", "p12"}},
		{"watched after is exclusive", core.PaymentFilter{WatchedAfter: at(112)}, []string{"p11", "p12"}},
		{"watched after and chain", core.PaymentFilter{WatchedAfter: at(110), ChainID: 97}, []string{"p10"}},
		{"watch ends by is inclusive", core.PaymentFilter{WatchEndsBy: at(104)}, []string{"p01", "p02", "p03", "p04"}},
		{"retired", core.PaymentFilter{Retired: &yes}, []string{"p05", "p08"}},
		{"not retired", core.PaymentFilter{Retired: &no}, []string{"p01", "p02", "p03", "p04", "p06", "p07", "p09", "p10", "p11", "p12"}},
		{"retired unchecked", core.PaymentFilter{RetiredUnchecked: true}, []string{"p08"}},
		{"contract", core.PaymentFilter{Contract: core.USDTBSCTestnet.Contract}, []string{"p10"}},
		{"contract of no payment", core.PaymentFilter{Contract: "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d"}, nil},
		{"not retired, settled, window ended", core.PaymentFilter{Retired: &no, WatchEndsBy: at(111), Statuses: []core.PaymentStatus{core.StatusPaid, core.StatusExpired}}, []string{"p04", "p09"}},

		{"everything at once", core.PaymentFilter{
			Statuses: []core.PaymentStatus{core.StatusPaid, core.StatusOverpaid}, NeedsAttention: &no, Search: "order p0",
			LinkID: "plink_a", FromLink: &yes, ChainID: 56, CreatedFrom: at(4), CreatedTo: at(7), PaidFrom: at(5), PaidTo: at(8),
			ExpiresBefore: at(40), WatchedAfter: at(100), WatchEndsBy: at(106),
		}, []string{"p04", "p05"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := listIDs(t, s, tc.filter, core.PaymentSort{By: core.SortByCreated}, core.Page{})
			wantEqual(t, got, tc.want, "ListPayments")
			view(t, s, func(r core.Reader) error {
				n, err := r.CountPayments(bg, tc.filter)
				if err != nil || n != len(tc.want) {
					t.Errorf("CountPayments = %d, %v; want %d", n, err, len(tc.want))
				}
				return nil
			})
		})
	}
}

func storePaymentSortPaging(t *testing.T, s core.Store) {
	paymentDataset(t, s)
	byCreated := []string{"p01", "p02", "p03", "p04", "p05", "p06", "p07", "p08", "p09", "p10", "p11", "p12"}
	byAmount := []string{"p09", "p10", "p11", "p01", "p02", "p07", "p08", "p04", "p05", "p06", "p03", "p12"}
	reversed := func(in []string) []string {
		out := slices.Clone(in)
		slices.Reverse(out)
		return out
	}
	created := core.PaymentSort{By: core.SortByCreated}
	createdDesc := core.PaymentSort{By: core.SortByCreated, Desc: true}
	amount := core.PaymentSort{By: core.SortByAmount}
	amountDesc := core.PaymentSort{By: core.SortByAmount, Desc: true}

	tests := []struct {
		name   string
		filter core.PaymentFilter
		sort   core.PaymentSort
		page   core.Page
		want   []string
	}{
		{"created ascending, ties by id", core.PaymentFilter{}, created, core.Page{}, byCreated},
		{"created descending, ties by id descending", core.PaymentFilter{}, createdDesc, core.Page{}, reversed(byCreated)},
		{"empty sort key means created", core.PaymentFilter{}, core.PaymentSort{}, core.Page{}, byCreated},
		{"amount ascending is numeric, ties by id", core.PaymentFilter{}, amount, core.Page{}, byAmount},
		{"amount descending", core.PaymentFilter{}, amountDesc, core.Page{}, reversed(byAmount)},

		{"first page", core.PaymentFilter{}, created, core.Page{Limit: 5}, byCreated[:5]},
		{"second page", core.PaymentFilter{}, created, core.Page{Limit: 5, Offset: 5}, byCreated[5:10]},
		{"last, short page", core.PaymentFilter{}, created, core.Page{Limit: 5, Offset: 10}, byCreated[10:]},
		{"page past the end", core.PaymentFilter{}, created, core.Page{Limit: 5, Offset: 12}, nil},
		{"offset without limit", core.PaymentFilter{}, created, core.Page{Offset: 9}, byCreated[9:]},
		{"limit larger than the result", core.PaymentFilter{}, created, core.Page{Limit: 100}, byCreated},
		{"paging descending", core.PaymentFilter{}, createdDesc, core.Page{Limit: 3, Offset: 3}, reversed(byCreated)[3:6]},
		{"paging by amount across a tie", core.PaymentFilter{}, amount, core.Page{Limit: 1, Offset: 8}, []string{"p05"}},
		{"paging by amount descending across a tie", core.PaymentFilter{}, amountDesc, core.Page{Limit: 2, Offset: 3}, []string{"p05", "p04"}},

		{"filter, sort by amount, page", core.PaymentFilter{Statuses: core.OpenStatuses}, amountDesc, core.Page{Limit: 3, Offset: 1},
			[]string{"p03", "p02", "p01"}},
		{"filter, sort by created desc, page", core.PaymentFilter{NeedsAttention: new(bool)}, createdDesc, core.Page{Limit: 4, Offset: 2},
			[]string{"p08", "p07", "p05", "p04"}},
		{"search, sorted by amount", core.PaymentFilter{Search: "1001"}, amount, core.Page{}, []string{"p11", "p01"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantEqual(t, listIDs(t, s, tc.filter, tc.sort, tc.page), tc.want, "ListPayments")
		})
	}

	// Walking every page yields every item exactly once, in order.
	for _, sort := range []core.PaymentSort{created, createdDesc, amount, amountDesc} {
		var walked []string
		for offset := 0; ; offset += 5 {
			page := listIDs(t, s, core.PaymentFilter{}, sort, core.Page{Limit: 5, Offset: offset})
			if len(page) == 0 {
				break
			}
			walked = append(walked, page...)
		}
		wantEqual(t, walked, listIDs(t, s, core.PaymentFilter{}, sort, core.Page{}), fmt.Sprintf("paged walk with sort %+v", sort))
	}
}

func storeTransfers(t *testing.T, s core.Store) {
	mk := func(payment string, block uint64, fork int, logIndex uint, tx string, status core.TransferStatus) core.Transfer {
		return core.Transfer{
			TxHash: Hash("tx", tx), LogIndex: logIndex, BlockHash: BlockHash(block, fork), PaymentID: payment,
			BlockNumber: block, From: Address("payer", tx), To: Address("deposit", payment),
			Amount: units(1), Status: status, SeenAt: at(1),
		}
	}
	a1 := mk("pay_a", 20, 0, 4, "t1", core.TransferPending)
	a2 := mk("pay_a", 10, 0, 9, "t2", core.TransferConfirmed)
	a3 := mk("pay_a", 20, 0, 1, "t3", core.TransferPending)
	a4 := mk("pay_a", 15, 1, 0, "t1", core.TransferOrphaned) // t1 in a stale block
	b1 := mk("pay_b", 12, 0, 3, "t4", core.TransferPending)
	b2 := mk("pay_b", 20, 0, 5, "t1", core.TransferPending) // t1 pays two addresses
	update(t, s, func(tx core.Tx) error {
		return errors.Join(tx.InsertTransfer(bg, a1), tx.InsertTransfer(bg, a2), tx.InsertTransfer(bg, a3),
			tx.InsertTransfer(bg, a4), tx.InsertTransfer(bg, b1), tx.InsertTransfer(bg, b2))
	})
	view(t, s, func(r core.Reader) error {
		got, err := r.ListTransfers(bg, "pay_a")
		wantErr(t, err, nil, "ListTransfers")
		wantEqual(t, got, []core.Transfer{a2, a4, a3, a1}, "ListTransfers order (block, log index)")
		got, err = r.ListTransfers(bg, "pay_none")
		if err != nil || len(got) != 0 {
			t.Errorf("ListTransfers of unknown payment = %v, %v", got, err)
		}
		got, err = r.ListTransfersByTxHash(bg, Hash("tx", "t1"))
		wantErr(t, err, nil, "ListTransfersByTxHash")
		wantEqual(t, got, []core.Transfer{a4, a1, b2}, "ListTransfersByTxHash")
		got, err = r.ListPendingTransfers(bg)
		wantErr(t, err, nil, "ListPendingTransfers")
		wantEqual(t, got, []core.Transfer{b1, a3, a1, b2}, "ListPendingTransfers")
		return nil
	})

	a1.Status, a1.ConfirmedAt = core.TransferConfirmed, at(9)
	a3.Status, a3.OrphanedAt = core.TransferOrphaned, at(8)
	update(t, s, func(tx core.Tx) error {
		return errors.Join(tx.UpdateTransfer(bg, a1), tx.UpdateTransfer(bg, a3))
	})
	view(t, s, func(r core.Reader) error {
		got, _ := r.GetTransfer(bg, a1.Key())
		wantEqual(t, got, a1, "updated transfer")
		pending, _ := r.ListPendingTransfers(bg)
		wantEqual(t, pending, []core.Transfer{b1, b2}, "pending after update")
		// The stale-block record with the same tx hash is untouched.
		stale, _ := r.GetTransfer(bg, a4.Key())
		wantEqual(t, stale, a4, "transfer with the same tx hash in another block")
		return nil
	})
}

func storeLinks(t *testing.T, s core.Store) {
	a := fullLink("plink_a", "alpha")
	a.CreatedAt = at(1)
	b := fullLink("plink_b", "beta")
	b.CreatedAt = at(5)
	c := fullLink("plink_c", "gamma")
	c.CreatedAt = at(5)
	c.Amount, c.MaxUses, c.ExpiresAt, c.LastPaidAt = core.Amount{}, 0, time.Time{}, time.Time{}
	update(t, s, func(tx core.Tx) error {
		return errors.Join(tx.InsertLink(bg, b), tx.InsertLink(bg, a), tx.InsertLink(bg, c))
	})
	view(t, s, func(r core.Reader) error {
		got, err := r.GetLinkBySlug(bg, "gamma")
		wantErr(t, err, nil, "GetLinkBySlug")
		wantEqual(t, got, c, "link by slug")
		_, err = r.GetLinkBySlug(bg, "GAMMA")
		wantErr(t, err, core.ErrNotFound, "slug lookup is exact")
		list, err := r.ListLinks(bg)
		wantErr(t, err, nil, "ListLinks")
		wantEqual(t, list, []core.PaymentLink{c, b, a}, "ListLinks newest first, ties by id descending")
		return nil
	})
	a.Uses, a.TotalCollected, a.Active, a.LastPaidAt = 39, units(17550), false, at(99)
	update(t, s, func(tx core.Tx) error { return tx.UpdateLink(bg, a) })
	view(t, s, func(r core.Reader) error {
		got, _ := r.GetLink(bg, "plink_a")
		wantEqual(t, got, a, "updated link")
		return nil
	})

	// Deleting a link removes only the link: payments created from it keep
	// their LinkID, and the slug is free again.
	p := fullPayment("pay_from_b")
	p.LinkID = "plink_b"
	update(t, s, func(tx core.Tx) error {
		return errors.Join(tx.InsertPayment(bg, p), tx.DeleteLink(bg, "plink_b"))
	})
	wantErr(t, s.Update(bg, func(tx core.Tx) error { return tx.DeleteLink(bg, "plink_b") }), core.ErrNotFound, "deleting a link twice")
	view(t, s, func(r core.Reader) error {
		_, err := r.GetLink(bg, "plink_b")
		wantErr(t, err, core.ErrNotFound, "deleted link by id")
		_, err = r.GetLinkBySlug(bg, "beta")
		wantErr(t, err, core.ErrNotFound, "deleted link by slug")
		list, _ := r.ListLinks(bg)
		wantEqual(t, list, []core.PaymentLink{c, a}, "links after a delete")
		got, err := r.GetPayment(bg, "pay_from_b")
		wantErr(t, err, nil, "payment of a deleted link")
		wantEqual(t, got, p, "payment of a deleted link")
		n, _ := r.CountPayments(bg, core.PaymentFilter{LinkID: "plink_b"})
		if n != 1 {
			t.Errorf("payments filtered by a deleted link's id = %d, want 1", n)
		}
		return nil
	})
	reuse := fullLink("plink_d", "beta")
	update(t, s, func(tx core.Tx) error { return tx.InsertLink(bg, reuse) })
}

func storeEndpoints(t *testing.T, s core.Store) {
	a, b, c := fullEndpoint("we_a"), fullEndpoint("we_b"), fullEndpoint("we_c")
	a.CreatedAt, b.CreatedAt, c.CreatedAt = at(3), at(1), at(3)
	update(t, s, func(tx core.Tx) error {
		return errors.Join(tx.InsertWebhookEndpoint(bg, c), tx.InsertWebhookEndpoint(bg, a), tx.InsertWebhookEndpoint(bg, b),
			tx.InsertDelivery(bg, fullDelivery("whd_1", "we_a")), tx.InsertDelivery(bg, fullDelivery("whd_2", "we_a")),
			tx.InsertDelivery(bg, fullDelivery("whd_3", "we_b")))
	})
	view(t, s, func(r core.Reader) error {
		list, err := r.ListWebhookEndpoints(bg)
		wantErr(t, err, nil, "ListWebhookEndpoints")
		wantEqual(t, list, []core.WebhookEndpoint{b, a, c}, "ListWebhookEndpoints oldest first, ties by id")
		return nil
	})
	a.Enabled, a.Events, a.Secret = false, []core.EventType{core.EventPaymentCreated}, "whsec_rotated"
	update(t, s, func(tx core.Tx) error { return tx.UpdateWebhookEndpoint(bg, a) })
	view(t, s, func(r core.Reader) error {
		got, _ := r.GetWebhookEndpoint(bg, "we_a")
		wantEqual(t, got, a, "updated endpoint")
		return nil
	})
	update(t, s, func(tx core.Tx) error { return tx.DeleteWebhookEndpoint(bg, "we_a") })
	view(t, s, func(r core.Reader) error {
		_, err := r.GetWebhookEndpoint(bg, "we_a")
		wantErr(t, err, core.ErrNotFound, "deleted endpoint")
		_, err = r.GetDelivery(bg, "whd_1")
		wantErr(t, err, core.ErrNotFound, "delivery of a deleted endpoint")
		_, err = r.GetDelivery(bg, "whd_2")
		wantErr(t, err, core.ErrNotFound, "delivery of a deleted endpoint")
		_, err = r.GetDelivery(bg, "whd_3")
		wantErr(t, err, nil, "delivery of another endpoint")
		return nil
	})
}

func storeDeliveries(t *testing.T, s core.Store) {
	mk := func(id, endpoint, payment, event string, status core.DeliveryStatus, created, next int) core.WebhookDelivery {
		d := fullDelivery(id, endpoint)
		d.PaymentID, d.EventID, d.Status, d.CreatedAt = payment, event, status, at(created)
		d.NextAttemptAt = time.Time{}
		if next > 0 {
			d.NextAttemptAt = at(next)
		}
		return d
	}
	deliveries := []core.WebhookDelivery{
		mk("d1", "we_a", "pay_1", "evt_1", core.DeliverySucceeded, 1, 0),
		mk("d2", "we_b", "pay_1", "evt_1", core.DeliveryFailed, 1, 0),
		mk("d3", "we_a", "pay_2", "evt_2", core.DeliveryPending, 5, 50),
		mk("d4", "we_a", "pay_2", "evt_3", core.DeliveryRetrying, 6, 40),
		mk("d5", "we_b", "pay_3", "evt_4", core.DeliveryRetrying, 7, 40),
		mk("d6", "we_a", "", "evt_5", core.DeliveryPending, 8, 60),
	}
	update(t, s, func(tx core.Tx) error {
		for _, i := range []int{3, 0, 5, 1, 4, 2} {
			if err := tx.InsertDelivery(bg, deliveries[i]); err != nil {
				return err
			}
		}
		return nil
	})
	open := []core.DeliveryStatus{core.DeliveryPending, core.DeliveryRetrying}
	tests := []struct {
		name   string
		filter core.DeliveryFilter
		page   core.Page
		want   []string
		total  int
	}{
		{"all, newest first, ties by id descending", core.DeliveryFilter{}, core.Page{}, []string{"d6", "d5", "d4", "d3", "d2", "d1"}, 6},
		{"by endpoint", core.DeliveryFilter{EndpointID: "we_b"}, core.Page{}, []string{"d5", "d2"}, 2},
		{"by payment", core.DeliveryFilter{PaymentID: "pay_2"}, core.Page{}, []string{"d4", "d3"}, 2},
		{"by event", core.DeliveryFilter{EventID: "evt_1"}, core.Page{}, []string{"d2", "d1"}, 2},
		{"by statuses", core.DeliveryFilter{Statuses: open}, core.Page{}, []string{"d6", "d5", "d4", "d3"}, 4},
		{"endpoint and status", core.DeliveryFilter{EndpointID: "we_a", Statuses: []core.DeliveryStatus{core.DeliverySucceeded}}, core.Page{}, []string{"d1"}, 1},
		{"created from is inclusive", core.DeliveryFilter{CreatedFrom: at(6)}, core.Page{}, []string{"d6", "d5", "d4"}, 3},
		{"endpoint, status, created from", core.DeliveryFilter{EndpointID: "we_a", Statuses: open, CreatedFrom: at(6)}, core.Page{}, []string{"d6", "d4"}, 2},
		{"page", core.DeliveryFilter{}, core.Page{Limit: 2, Offset: 1}, []string{"d5", "d4"}, 6},
		{"page past the end", core.DeliveryFilter{}, core.Page{Limit: 2, Offset: 6}, nil, 6},
		{"due: ordered by next attempt then id; undated never due", core.DeliveryFilter{DueBefore: at(1000)}, core.Page{}, []string{"d4", "d5", "d3", "d6"}, 4},
		{"due before is inclusive", core.DeliveryFilter{DueBefore: at(50)}, core.Page{}, []string{"d4", "d5", "d3"}, 3},
		{"due with status and limit", core.DeliveryFilter{DueBefore: at(55), Statuses: open}, core.Page{Limit: 2}, []string{"d4", "d5"}, 3},
		{"nothing due yet", core.DeliveryFilter{DueBefore: at(39)}, core.Page{}, nil, 0},
		{"no match", core.DeliveryFilter{EndpointID: "we_zzz"}, core.Page{}, nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			view(t, s, func(r core.Reader) error {
				items, err := r.ListDeliveries(bg, tc.filter, tc.page)
				wantErr(t, err, nil, "ListDeliveries")
				var ids []string
				for _, d := range items {
					ids = append(ids, d.ID)
				}
				wantEqual(t, ids, tc.want, "ListDeliveries")
				n, err := r.CountDeliveries(bg, tc.filter)
				if err != nil || n != tc.total {
					t.Errorf("CountDeliveries = %d, %v; want %d", n, err, tc.total)
				}
				return nil
			})
		})
	}

	d4 := deliveries[3]
	d4.Status, d4.Attempt, d4.NextAttemptAt, d4.CompletedAt = core.DeliverySucceeded, 3, time.Time{}, at(70)
	d4.Attempts = append(slices.Clone(d4.Attempts), core.DeliveryAttempt{N: 3, At: at(70), ResponseCode: 200, DurationMs: 91, Succeeded: true})
	update(t, s, func(tx core.Tx) error { return tx.UpdateDelivery(bg, d4) })
	view(t, s, func(r core.Reader) error {
		got, _ := r.GetDelivery(bg, "d4")
		wantEqual(t, got, d4, "updated delivery")
		return nil
	})

	var removed int
	update(t, s, func(tx core.Tx) error {
		var err error
		removed, err = tx.DeleteDeliveriesBefore(bg, at(6))
		return err
	})
	if removed != 3 {
		t.Errorf("DeleteDeliveriesBefore removed %d, want 3 (strictly before the cut-off)", removed)
	}
	view(t, s, func(r core.Reader) error {
		items, _ := r.ListDeliveries(bg, core.DeliveryFilter{}, core.Page{})
		var ids []string
		for _, d := range items {
			ids = append(ids, d.ID)
		}
		wantEqual(t, ids, []string{"d6", "d5", "d4"}, "deliveries left after pruning")
		return nil
	})
}

// storeDeliveryFirstAttempts: the Unattempted filter and its order, which
// the core uses to hand out first attempts in event order.
func storeDeliveryFirstAttempts(t *testing.T, s core.Store) {
	mk := func(id, endpoint string, seq int64, attempt int, next int) core.WebhookDelivery {
		d := fullDelivery(id, endpoint)
		d.EventSeq, d.Attempt, d.Status, d.NextAttemptAt, d.CreatedAt = seq, attempt, core.DeliveryPending, at(next), at(next)
		return d
	}
	update(t, s, func(tx core.Tx) error {
		return errors.Join(
			tx.InsertDelivery(bg, mk("q1", "we_a", 30, 0, 5)), // created after q2, earlier event
			tx.InsertDelivery(bg, mk("q2", "we_a", 31, 0, 3)),
			tx.InsertDelivery(bg, mk("q3", "we_a", 12, 1, 2)), // attempted: a retry
			tx.InsertDelivery(bg, mk("q4", "we_a", 0, 0, 1)),  // the test event: not in the log
			tx.InsertDelivery(bg, mk("q5", "we_b", 20, 0, 4)),
		)
	})
	yes, no := true, false
	for _, tc := range []struct {
		name   string
		filter core.DeliveryFilter
		page   core.Page
		want   []string
	}{
		{"unattempted, in event order", core.DeliveryFilter{Unattempted: &yes}, core.Page{}, []string{"q5", "q1", "q2"}},
		{"head of an endpoint's queue", core.DeliveryFilter{EndpointID: "we_a", Unattempted: &yes}, core.Page{Limit: 1}, []string{"q1"}},
		{"everything else (sorted here)", core.DeliveryFilter{Unattempted: &no}, core.Page{}, []string{"q3", "q4"}},
		{"due, unattempted: in due order", core.DeliveryFilter{Unattempted: &yes, DueBefore: at(4)}, core.Page{}, []string{"q2", "q5"}},
		{"due, attempted or not logged", core.DeliveryFilter{Unattempted: &no, DueBefore: at(100)}, core.Page{}, []string{"q4", "q3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view(t, s, func(r core.Reader) error {
				items, err := r.ListDeliveries(bg, tc.filter, tc.page)
				wantErr(t, err, nil, "ListDeliveries")
				var ids []string
				for _, d := range items {
					ids = append(ids, d.ID)
				}
				if tc.name == "everything else (sorted here)" {
					slices.Sort(ids)
				}
				wantEqual(t, ids, tc.want, "ListDeliveries")
				return nil
			})
		})
	}
}

func storeAPIKeys(t *testing.T, s core.Store) {
	a, b, c := fullKey("key_a"), fullKey("key_b"), fullKey("key_c")
	a.CreatedAt, b.CreatedAt, c.CreatedAt = at(2), at(9), at(2)
	update(t, s, func(tx core.Tx) error {
		return errors.Join(tx.InsertAPIKey(bg, a), tx.InsertAPIKey(bg, b), tx.InsertAPIKey(bg, c))
	})
	view(t, s, func(r core.Reader) error {
		got, err := r.GetAPIKeyByHash(bg, b.SecretHash)
		wantErr(t, err, nil, "GetAPIKeyByHash")
		wantEqual(t, got, b, "key by hash")
		list, err := r.ListAPIKeys(bg)
		wantErr(t, err, nil, "ListAPIKeys")
		wantEqual(t, list, []core.APIKey{b, c, a}, "ListAPIKeys newest first, ties by id descending")
		return nil
	})
	a.LastUsedAt, a.RevokedAt, a.Scopes = at(500), time.Time{}, []core.Scope{core.ScopeWebhooksManage}
	update(t, s, func(tx core.Tx) error { return tx.UpdateAPIKey(bg, a) })
	view(t, s, func(r core.Reader) error {
		got, _ := r.GetAPIKey(bg, "key_a")
		wantEqual(t, got, a, "updated key")
		return nil
	})
}

func storeSessions(t *testing.T, s core.Store) {
	mk := func(id string, expires int) core.Session {
		return core.Session{ID: id, OperatorID: "op_1", TokenHash: Hash("token", id), UserAgent: "ua", IP: "::1", CreatedAt: at(1), ExpiresAt: at(expires)}
	}
	sessions := []core.Session{mk("s1", 10), mk("s2", 20), mk("s3", 30), mk("s4", 40), mk("s5", 50)}
	sessions[1].CreatedAt, sessions[3].CreatedAt = at(7), at(7)
	view(t, s, func(r core.Reader) error {
		list, err := r.ListSessions(bg)
		if err != nil || len(list) != 0 {
			t.Errorf("ListSessions on an empty store = %v, %v", list, err)
		}
		return nil
	})
	update(t, s, func(tx core.Tx) error {
		for _, sess := range sessions {
			if err := tx.InsertSession(bg, sess); err != nil {
				return err
			}
		}
		return nil
	})
	view(t, s, func(r core.Reader) error {
		list, err := r.ListSessions(bg)
		wantErr(t, err, nil, "ListSessions")
		wantEqual(t, list, []core.Session{sessions[3], sessions[1], sessions[4], sessions[2], sessions[0]},
			"ListSessions newest first, ties by id descending, expired included")
		return nil
	})
	alive := func() []string {
		var ids []string
		view(t, s, func(r core.Reader) error {
			for _, sess := range sessions {
				if got, err := r.GetSessionByTokenHash(bg, sess.TokenHash); err == nil {
					wantEqual(t, got, sess, "session")
					ids = append(ids, sess.ID)
				} else {
					wantErr(t, err, core.ErrNotFound, "missing session")
				}
			}
			return nil
		})
		return ids
	}
	// UpdateSession replaces a session (the core records its last use).
	sessions[2].LastSeenAt, sessions[2].UserAgent = at(25), "ua-2"
	update(t, s, func(tx core.Tx) error { return tx.UpdateSession(bg, sessions[2]) })
	view(t, s, func(r core.Reader) error {
		got, err := r.GetSessionByTokenHash(bg, sessions[2].TokenHash)
		wantErr(t, err, nil, "GetSessionByTokenHash after UpdateSession")
		wantEqual(t, got, sessions[2], "updated session")
		other, _ := r.GetSessionByTokenHash(bg, sessions[0].TokenHash)
		if !other.LastSeenAt.IsZero() {
			t.Errorf("a session that was never used must keep a zero LastSeenAt: %v", other.LastSeenAt)
		}
		return nil
	})
	var n int
	update(t, s, func(tx core.Tx) error {
		var err error
		n, err = tx.DeleteSessionsExpiredBefore(bg, at(20))
		return err
	})
	if n != 1 {
		t.Errorf("DeleteSessionsExpiredBefore removed %d, want 1 (strictly before)", n)
	}
	wantEqual(t, alive(), []string{"s2", "s3", "s4", "s5"}, "after expiring")
	update(t, s, func(tx core.Tx) error { return tx.DeleteSession(bg, "s3") })
	wantEqual(t, alive(), []string{"s2", "s4", "s5"}, "after deleting one")
	update(t, s, func(tx core.Tx) error {
		var err error
		n, err = tx.DeleteSessionsExcept(bg, "s4")
		return err
	})
	if n != 2 {
		t.Errorf("DeleteSessionsExcept removed %d, want 2", n)
	}
	wantEqual(t, alive(), []string{"s4"}, "after deleting all but one")
	update(t, s, func(tx core.Tx) error {
		var err error
		n, err = tx.DeleteSessionsExcept(bg, "")
		return err
	})
	if n != 1 {
		t.Errorf("DeleteSessionsExcept(\"\") removed %d, want 1", n)
	}
	wantEqual(t, alive(), nil, "after deleting all")
}
