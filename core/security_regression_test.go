package core_test

// Regression tests for ways the service could be abused: dust that pays an
// invoice, a webhook secret that is empty, a watch list filled for free, a
// login flood that monopolises the password KDF, logouts that write, and key
// switches that run into used addresses. Each describes the attack and
// asserts the safe behaviour. More of them live in coretest.RunServiceSuite
// (group "Security"), where they also run against every store
// implementation.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

func setUnderpaymentTolerance(t *testing.T, e *coretest.Env, tolerance string) {
	t.Helper()
	_, err := e.Service.UpdatePaymentDefaults(e.Operator(), core.PaymentSettings{
		TTL: core.DefaultPaymentTTL, RequiredConfirmations: 1, UnderpaymentTolerance: tolerance,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The underpayment tolerance is an absolute amount the operator sets. Were
// it applied as it stands, need = Amount − Tolerance would saturate at 0 for
// an invoice whose amount is at or below the tolerance, and a single base
// unit (10^-18 USDT) would settle it as PAID. It is capped relative to the
// amount, so dust never pays.
func TestDustDoesNotPayWhenTheToleranceCoversTheAmount(t *testing.T) {
	e := coretest.NewEnv(t)
	setUnderpaymentTolerance(t, e, "1.00") // operator: "forgive up to 1 USDT of exchange fees"
	e.Head(100)

	p := e.CreatePayment("1.00") // a 1 USDT invoice
	e.Pay(p, "0.000000000000000001", 101)
	e.Head(101)

	got := e.Payment(p.ID)
	t.Logf("invoice %s USDT, received %s USDT -> status %q", got.Amount, got.AmountReceived, got.Status)
	if got.Status == core.StatusPaid {
		t.Fatalf("a %s USDT invoice was settled as paid by %s USDT", got.Amount, got.AmountReceived)
	}
}

// The public variant: on a customer-chooses-amount payment link the payer
// picks the invoice amount, so an anonymous payer could choose one at or
// below the tolerance and get payment.paid — and a "use" of the link — for
// one base unit. That must not work.
func TestDustDoesNotPayACustomerAmountLinkPayment(t *testing.T) {
	e := coretest.NewEnv(t)
	setUnderpaymentTolerance(t, e, "0.50")
	e.Head(100)

	link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Top-up", Active: true, MaxUses: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Anonymous: no actor in the context. An amount at the tolerance is not
	// accepted at all (a customer-chosen amount has a
	// minimum of one unit) ...
	if _, _, err := e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "0.50"}); core.KindOf(err) != core.KindInvalid {
		t.Fatalf("an amount below the link's minimum must be refused as invalid, got %v", err)
	}
	// ... and at the minimum, the tolerance (capped at 5 % of the amount)
	// leaves 0.95 to pay: one base unit still buys nothing.
	co, _, err := e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "1"})
	if err != nil {
		t.Fatal(err)
	}
	p := e.Payment(co.ID)
	e.Pay(p, "0.000000000000000001", 101)
	e.Head(101)

	got := e.Payment(p.ID)
	lv, err := e.Service.GetLink(e.Operator(), link.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("link payment: amount %s, received %s, status %q; link uses %d/1, status %q",
		got.Amount, got.AmountReceived, got.Status, lv.Uses, lv.Status)
	if got.Status == core.StatusPaid {
		t.Fatalf("anonymous payer got payment.paid for %s USDT on a %s USDT invoice and consumed the link's only use", got.AmountReceived, got.Amount)
	}
}

// VerifyWebhookSignature must not accept an empty secret: a receiver that
// loads its secret from an unset environment variable would verify with "",
// and anyone can compute HMAC("", t.body).
func TestEmptyWebhookSecretNeverVerifies(t *testing.T) {
	e := coretest.NewEnv(t)
	now := e.Clock.Now()
	body := []byte(`{"id":"evt_forged","type":"payment.paid","data":{"id":"pay_x","status":"paid"}}`)
	forged := core.WebhookSignatureHeader(now, body, "") // attacker knows no secret
	if err := core.VerifyWebhookSignature(forged, body, []string{""}, now, 0); err == nil {
		t.Fatal("a signature made with the empty key verified against an empty configured secret")
	}
}

// A payment that nobody ever paid must not stay on the chain watch list for
// the full watch window (7 days) after it expired. Payments from a public
// link are created anonymously (default cap ≈ 720/hour instance-wide), so
// the watch set — one eth_getLogs topic per address, 500 per request, every
// tick — would grow to ~120 000 addresses at no cost to an attacker.
func TestExpiredUnpaidLinkPaymentsLeaveTheWatchList(t *testing.T) {
	e := coretest.NewEnv(t)
	e.Head(100)
	link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Donate", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	var last core.CheckoutView
	for i := 0; i < n; i++ {
		if last, _, err = e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "1"}); err != nil {
			t.Fatal(err)
		}
	}
	e.Expire(e.Payment(last.ID))
	e.Head(101) // every one of them expires, unpaid
	e.Clock.Advance(6 * 24 * time.Hour)
	e.Head(102)
	cs, err := e.Service.ChainState(e.System())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d anonymous, never-paid link checkouts; 6 days after they expired the watcher must still scan %d addresses", n, len(cs.Addresses))
	if len(cs.Addresses) > n/10 {
		t.Fatalf("%d of %d expired, never-funded anonymous payments are still on the watch list after 6 days", len(cs.Addresses), n)
	}
}

// Password verification is slow by design and logins are
// unauthenticated. The service runs only a few KDFs at a time; the rest
// wait briefly and are then told to come back.
type gatedHasher struct {
	running, peak atomic.Int64
	gate          chan struct{} // closed to let verifications finish
}

func (h *gatedHasher) Hash(p string) (string, error) { return "gated$" + p, nil }
func (h *gatedHasher) Verify(hash, p string) (bool, error) {
	n := h.running.Add(1)
	defer h.running.Add(-1)
	for {
		old := h.peak.Load()
		if n <= old || h.peak.CompareAndSwap(old, n) {
			break
		}
	}
	<-h.gate
	return hash == "gated$"+p, nil
}

func TestPasswordKDFConcurrencyIsBounded(t *testing.T) {
	h := &gatedHasher{gate: make(chan struct{})}
	e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.Hasher = h; c.MaxConcurrentKDF = 3 }))
	const attempts = 40
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			_, err := e.Service.Login(context.Background(), core.LoginInput{Email: fmt.Sprintf("nobody%d@example.com", i), Password: "x"})
			errs <- err
		})
	}
	// Three are verifying; everyone else waits, then gives up as busy.
	deadline := time.Now().Add(10 * time.Second)
	for h.running.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// A caller that cannot wait is released by its context, not stuck.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err := e.Service.Login(ctx, core.LoginInput{Email: e.Email, Password: e.Password})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("login with an expiring context while all slots are busy: %v", err)
	}
	busy := 0
	for range attempts - 3 {
		err := <-errs
		if !errors.Is(err, core.ErrBusy) || core.KindOf(err) != core.KindUnavailable {
			t.Fatalf("a login beyond the KDF bound got %v, want ErrBusy", err)
		}
		busy++
	}
	close(h.gate)
	wg.Wait()
	for range 3 {
		if err := <-errs; !errors.Is(err, core.ErrInvalidCredentials) {
			t.Fatalf("a login that got a KDF slot: %v", err)
		}
	}
	if peak := h.peak.Load(); peak != 3 {
		t.Fatalf("%d password verifications ran at the same time, bound is 3", peak)
	}
	t.Logf("%d logins: 3 verified, %d told to retry, peak concurrency %d", attempts, busy, h.peak.Load())
	// With the slots free again, logging in works — and the other password
	// operations go through the same gate.
	if _, err := e.Service.Login(context.Background(), core.LoginInput{Email: e.Email, Password: e.Password}); err != nil {
		t.Fatalf("login after the burst: %v", err)
	}
	if err := e.Service.ChangePassword(e.Operator(), e.Password, "a brand new passphrase"); err != nil {
		t.Fatalf("change password: %v", err)
	}
}

// writeCounter counts write transactions.
type writeCounter struct {
	core.Store
	updates atomic.Int64
}

func (s *writeCounter) Update(ctx context.Context, fn func(core.Tx) error) error {
	s.updates.Add(1)
	return s.Store.Update(ctx, fn)
}

// Logging out with a token that is not a session must not open a
// write transaction (the route is unauthenticated).
func TestLogoutOfUnknownTokenDoesNotWrite(t *testing.T) {
	store := &writeCounter{Store: coretest.NewMemStore()}
	e := coretest.NewEnv(t, coretest.WithStore(store))
	before := store.updates.Load()
	for _, token := range []string{"", "garbage", "hmns_", "hmns_0000000000000000000000000000000000000000000000000000", "hmn_live_notasession"} {
		if err := e.Service.Logout(context.Background(), token); err != nil {
			t.Fatalf("Logout(%q): %v", token, err)
		}
	}
	if n := store.updates.Load() - before; n != 0 {
		t.Fatalf("logging out unknown tokens opened %d write transactions", n)
	}
	// A real session is ended with exactly one write.
	if err := e.Service.Logout(context.Background(), e.Token); err != nil {
		t.Fatal(err)
	}
	if n := store.updates.Load() - before; n != 1 {
		t.Fatalf("logout of a real session: %d writes, want 1", n)
	}
	if _, err := e.Service.AuthenticateSession(context.Background(), e.Token); !errors.Is(err, core.ErrInvalidCredentials) {
		t.Fatalf("session after logout: %v", err)
	}
}

// countingAddresses counts derivations.
type countingAddresses struct {
	coretest.Addresses
	calls atomic.Int64
}

func (a *countingAddresses) DeriveAddress(ctx context.Context, w core.WalletSettings, i uint32) (string, error) {
	a.calls.Add(1)
	return a.Addresses.DeriveAddress(ctx, w, i)
}

// Going back to a key that was used before restarts its index at 0
// on top of a long run of used addresses. Allocation must get past them —
// it used to give up after 100 — and must not derive every one of them.
func TestSwitchingBackToAnEarlierKeySkipsUsedAddresses(t *testing.T) {
	src := &countingAddresses{}
	e := coretest.NewEnv(t, coretest.WithConfig(func(c *core.Config) { c.Addresses = src }))
	const used = 333
	seen := map[string]bool{}
	for range used {
		seen[e.CreatePayment("1").Address] = true
	}
	other := coretest.XPub[:len(coretest.XPub)-4] + "zzzz"
	if _, err := e.Service.UpdateWallet(e.Operator(), core.UpdateWalletInput{XPub: other}); err != nil {
		t.Fatal(err)
	}
	e.CreatePayment("1")
	st, err := e.Service.UpdateWallet(e.Operator(), core.UpdateWalletInput{XPub: coretest.XPub})
	if err != nil || st.Wallet.NextAddressIndex != 0 {
		t.Fatalf("back to the first key: %+v, %v", st.Wallet, err)
	}

	before := src.calls.Load()
	p := e.CreatePayment("1")
	derivations := src.calls.Load() - before
	if seen[p.Address] {
		t.Fatalf("address %s was handed out twice", p.Address)
	}
	if want := coretest.Address(coretest.XPub, core.DefaultDerivationPath, uint32(used)); p.Address != want {
		t.Fatalf("allocation did not resume right after the used run: got %s, want index %d", p.Address, used)
	}
	if derivations > 40 {
		t.Fatalf("skipping %d used addresses took %d derivations", used, derivations)
	}
	t.Logf("skipped %d used addresses with %d derivations", used, derivations)
	// From here on it is one derivation per payment again.
	before = src.calls.Load()
	q := e.CreatePayment("1")
	if n := src.calls.Load() - before; n != 1 || q.Address != coretest.Address(coretest.XPub, core.DefaultDerivationPath, uint32(used+1)) {
		t.Fatalf("next payment: %d derivations, address %s", n, q.Address)
	}
	// A hand-set index in the middle of a used run is skipped the same way.
	mid := uint32(100)
	if _, err := e.Service.UpdateWallet(e.Operator(), core.UpdateWalletInput{XPub: coretest.XPub, NextAddressIndex: &mid}); err != nil {
		t.Fatal(err)
	}
	if r := e.CreatePayment("1"); seen[r.Address] || r.Address == p.Address || r.Address == q.Address {
		t.Fatalf("address %s was handed out twice", r.Address)
	}
}
