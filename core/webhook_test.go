package core_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

func TestWebhookPolicy(t *testing.T) {
	// The contract the dashboard explains to operators.
	if core.WebhookMaxAttempts != 8 || core.WebhookTimeout != 10*time.Second ||
		core.WebhookAutoDisableAfter != 72*time.Hour || core.WebhookRetention != 30*24*time.Hour ||
		core.WebhookSecretOverlap != 24*time.Hour {
		t.Fatal("webhook policy constants changed")
	}
	want := []time.Duration{1, 5, 15, 60, 180, 360, 720}
	var total time.Duration
	for attempts := 1; attempts <= 7; attempts++ {
		wait, ok := core.WebhookBackoff(attempts)
		if !ok || wait != want[attempts-1]*time.Minute {
			t.Errorf("WebhookBackoff(%d) = %v, %v; want %v minutes", attempts, wait, ok, want[attempts-1])
		}
		total += wait
	}
	if total != 22*time.Hour+21*time.Minute {
		t.Errorf("all retries span %v", total)
	}
	for _, attempts := range []int{8, 9, 100} {
		if _, ok := core.WebhookBackoff(attempts); ok {
			t.Errorf("WebhookBackoff(%d) offers another attempt", attempts)
		}
	}
	if wait, ok := core.WebhookBackoff(0); !ok || wait != 0 {
		t.Errorf("WebhookBackoff(0) = %v, %v", wait, ok)
	}
}

func TestWebhookSignature(t *testing.T) {
	// A fixed vector, so that receivers in any language can check their
	// implementation: HMAC-SHA256("whsec_test", "1700000000." + body).
	const secret = "whsec_test"
	body := []byte(`{"id":"evt_1","type":"payment.paid"}`)
	at := time.Unix(1700000000, 0)
	sig := core.SignWebhook(secret, at, body)
	if sig != "efa51d9ca3a76699a91b6338ee3c81b938c6da8aea8f4080480d00aedf20efab" {
		t.Fatalf("signature of the fixed vector changed: %q", sig)
	}
	if sig != core.SignWebhook(secret, at.Add(999*time.Millisecond), body) {
		t.Error("the timestamp is whole seconds")
	}
	for name, other := range map[string]string{
		"secret": core.SignWebhook("whsec_other", at, body),
		"time":   core.SignWebhook(secret, at.Add(time.Second), body),
		"body":   core.SignWebhook(secret, at, append([]byte(" "), body...)),
	} {
		if other == sig {
			t.Errorf("signature does not depend on the %s", name)
		}
	}

	header := core.WebhookSignatureHeader(at, body, secret)
	if header != "t=1700000000,v1="+sig {
		t.Fatalf("header = %q", header)
	}
	verify := func(header string, body []byte, secrets []string, now time.Time, tol time.Duration) error {
		return core.VerifyWebhookSignature(header, body, secrets, now, tol)
	}
	ok := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	bad := func(err error, what string) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: verified", what)
		} else if core.KindOf(err) != core.KindUnauthorized {
			t.Errorf("%s: kind %q, want unauthorized", what, core.KindOf(err))
		}
	}
	ok(verify(header, body, []string{secret}, at, 0), "fresh signature")
	ok(verify(header, body, []string{"whsec_new", secret}, at, 0), "any of several secrets")
	ok(verify(" t=1700000000 , v1="+sig+" ", body, []string{secret}, at, 0), "whitespace around parts")
	ok(verify("v1="+sig+",t=1700000000,v0=ignored", body, []string{secret}, at, 0), "order and unknown schemes")
	bad(verify(header, body, []string{"whsec_other"}, at, 0), "wrong secret")
	// No usable secret is a configuration error, reported as such
	// before any comparison — never a signature that verifies.
	for name, secrets := range map[string][]string{
		"no secrets": nil, "empty list": {}, "empty secret": {""}, "an empty secret next to a real one": {secret, ""},
	} {
		err := verify(header, body, secrets, at, 0)
		if !errors.Is(err, core.ErrNoWebhookSecret) || core.KindOf(err) != core.KindUnavailable {
			t.Errorf("%s: %v, want ErrNoWebhookSecret", name, err)
		}
	}
	forged := core.WebhookSignatureHeader(at, body, "") // anyone can compute this
	if err := verify(forged, body, []string{""}, at, 0); !errors.Is(err, core.ErrNoWebhookSecret) {
		t.Errorf("a signature made with the empty key: %v, want ErrNoWebhookSecret", err)
	}
	bad(verify(header, []byte(`{"id":"evt_2"}`), []string{secret}, at, 0), "tampered body")
	bad(verify("t=1700000001,v1="+sig, body, []string{secret}, at, 0), "tampered timestamp")
	bad(verify("t=1700000000,v1="+strings.ToUpper(sig), body, []string{secret}, at, 0), "upper-case signature")
	bad(verify("t=1700000000,v1="+sig[:63], body, []string{secret}, at, 0), "truncated signature")
	for _, malformed := range []string{"", "garbage", "t=1700000000", "v1=" + sig, "t=abc,v1=" + sig, "t=,v1=" + sig} {
		bad(verify(malformed, body, []string{secret}, at, 0), "malformed header "+malformed)
	}

	// Replay protection.
	ok(verify(header, body, []string{secret}, at.Add(5*time.Minute), 0), "exactly at the default tolerance")
	bad(verify(header, body, []string{secret}, at.Add(5*time.Minute+time.Second), 0), "older than the default tolerance")
	bad(verify(header, body, []string{secret}, at.Add(-5*time.Minute-time.Second), 0), "from the future")
	ok(verify(header, body, []string{secret}, at.Add(time.Hour), 2*time.Hour), "custom tolerance")
	bad(verify(header, body, []string{secret}, at.Add(time.Hour), time.Minute), "custom tolerance exceeded")
	ok(verify(header, body, []string{secret}, at.Add(1000*time.Hour), -1), "age check disabled")

	// Rotation: two signatures, either secret verifies.
	rotated := core.WebhookSignatureHeader(at, body, "whsec_new", secret)
	if strings.Count(rotated, "v1=") != 2 {
		t.Fatalf("rotation header = %q", rotated)
	}
	ok(verify(rotated, body, []string{secret}, at, 0), "old secret during rotation")
	ok(verify(rotated, body, []string{"whsec_new"}, at, 0), "new secret during rotation")
	bad(verify(rotated, body, []string{"whsec_other"}, at, 0), "unrelated secret during rotation")

	ep := core.WebhookEndpoint{Secret: "new", PreviousSecret: "old", PreviousSecretExpiresAt: at}
	if got := ep.SigningSecrets(at.Add(-time.Second)); len(got) != 2 || got[0] != "new" || got[1] != "old" {
		t.Errorf("signing secrets during overlap: %v", got)
	}
	if got := ep.SigningSecrets(at); len(got) != 1 || got[0] != "new" {
		t.Errorf("signing secrets after overlap: %v", got)
	}
}

// The signature of one fixed request, pinned. The receiver example in
// docs/examples/verify-webhook.mjs (shown in the README) is tested against
// the same header (apps/web/src/lib/webhook-example.test.ts), so the two
// cannot drift apart without a test failing.
func TestWebhookSignatureExampleVector(t *testing.T) {
	body := []byte(`{"id":"evt_example","type":"payment.paid","data":{"id":"pay_example","status":"paid"},"seq":42}`)
	at := time.Unix(1767225600, 0)
	const want = "t=1767225600,v1=abbb4eb677a7b39f59c56c106ed94d3ace20ab5aa145a97f96e3ae5dee2c0d77,v1=7bcd5ec49b5306d81a0147e180991c3d9d654c44034feeb0c049a42e793235ab"
	if got := core.WebhookSignatureHeader(at, body, "whsec_current_example", "whsec_previous_example"); got != want {
		t.Fatalf("signature header = %s, want %s", got, want)
	}
	if err := core.VerifyWebhookSignature(want, body, []string{"whsec_previous_example"}, at.Add(time.Minute), 0); err != nil {
		t.Fatalf("the pinned header does not verify: %v", err)
	}
}

func TestEventTypes(t *testing.T) {
	// The frontend's eight, plus the ones the core added.
	for _, typ := range []string{
		"payment.created", "payment.detected", "payment.confirming", "payment.paid",
		"payment.underpaid", "payment.overpaid", "payment.expired", "payment.failed",
		"payment.awaiting", "payment.late_funds", "payment.resolved", "payment.reopened",
	} {
		if !core.EventType(typ).Subscribable() || !core.EventType(typ).IsPayment() {
			t.Errorf("%s must be a subscribable payment event", typ)
		}
	}
	if len(core.WebhookEventTypes) != 12 {
		t.Errorf("WebhookEventTypes has %d entries", len(core.WebhookEventTypes))
	}
	for _, typ := range []core.EventType{core.EventSettingsUpdated, core.EventWebhookEndpointDisabled, core.EventWebhookTest, "payment.unknown"} {
		if typ.Subscribable() {
			t.Errorf("%s must not be subscribable", typ)
		}
	}
	for _, s := range []core.DeliveryStatus{core.DeliveryPending, core.DeliveryRetrying, core.DeliverySucceeded, core.DeliveryFailed} {
		if !s.Valid() || s.Done() != (s == core.DeliverySucceeded || s == core.DeliveryFailed) {
			t.Errorf("delivery status %s: valid %v done %v", s, s.Valid(), s.Done())
		}
	}
	if core.DeliveryStatus("lost").Valid() || core.DeliveryStatus("").Valid() {
		t.Error("unknown delivery statuses must not be valid")
	}
	for _, s := range core.PaymentStatuses {
		if !s.Valid() || s.Open() == s.Settled() {
			t.Errorf("status %s: valid %v open %v settled %v", s, s.Valid(), s.Open(), s.Settled())
		}
	}
	if core.PaymentStatus("refunded").Valid() || core.PaymentStatus("").Settled() {
		t.Error("unknown statuses must be neither valid nor settled")
	}
}
