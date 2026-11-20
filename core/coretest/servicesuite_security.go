package coretest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// securityCases pin the rules that defend the service against abuse: lying
// block times, claiming an instance that is not yours, dust that pays, link
// checkouts that oversell or fill the watch list, secrets that leak to the
// wrong reader, and the operator's way back in.
var securityCases = []serviceCase{
	{"a block timestamp from the future is refused, and could not expire anything anyway", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		// Production default (the test kit turns the check off): the head
		// itself is refused, loudly.
		e := newEnv(WithConfig(func(c *core.Config) { c.MaxHeadTimeAhead = 0 }))
		p := e.CreatePayment("10")
		e.Head(100)
		lying := core.ChainHead{ChainID: 56, Number: 101, Hash: BlockHash(101, 0), Time: e.Clock.Now().Add(40 * 24 * time.Hour), Final: 101}
		err := e.Service.AdvanceHead(e.System(), lying)
		wantIs(t, err, core.ErrHeadTimeAhead)
		wantKind(t, err, core.KindInvalid)
		state, err := e.Service.ChainState(e.System())
		noErr(t, err)
		if state.Head.Number != 100 {
			t.Fatalf("a refused head was recorded: %+v", state.Head)
		}
		wantStatus(t, e, p.ID, core.StatusAwaiting)
		// Ordinary skew is fine: two minutes ahead is still accepted.
		ok := core.ChainHead{ChainID: 56, Number: 101, Hash: BlockHash(101, 0), Time: e.Clock.Now().Add(core.DefaultMaxHeadTimeAhead), Final: 101}
		noErr(t, e.Service.AdvanceHead(e.System(), ok))
		tooFar := core.ChainHead{ChainID: 56, Number: 102, Hash: BlockHash(102, 0), Time: e.Clock.Now().Add(core.DefaultMaxHeadTimeAhead + time.Second), Final: 102}
		wantIs(t, e.Service.AdvanceHead(e.System(), tooFar), core.ErrHeadTimeAhead)
	}},

	{"with the head check off, a lying timestamp still expires nothing early", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithConfig(func(c *core.Config) { c.MaxHeadTimeAhead = -1 }))
		unpaid, willPay := e.CreatePayment("10"), e.CreatePayment("25")
		e.Head(100)
		lie := func(n uint64) {
			t.Helper()
			head := core.ChainHead{ChainID: 56, Number: n, Hash: BlockHash(n, 0), Time: e.Clock.Now().Add(40 * 24 * time.Hour), Final: n}
			noErr(t, e.Service.AdvanceHead(e.System(), head))
		}
		// One endpoint claims every block is 40 days in the future.
		lie(101)
		lie(102)
		wantStatus(t, e, unpaid.ID, core.StatusAwaiting)
		wantStatus(t, e, willPay.ID, core.StatusAwaiting)
		// The payer pays well inside the window and is paid, not "late".
		e.Clock.Advance(5 * time.Minute)
		e.Pay(willPay, "25", 103)
		lie(117)
		if got := wantStatus(t, e, willPay.ID, core.StatusPaid); got.NeedsAttention {
			t.Fatalf("in-time payment: %+v", got)
		}
		wantEvents(t, e, willPay.ID, created, confirming, paid)
		wantEvents(t, e, unpaid.ID, created)
		// The wall clock is what a lie cannot move: when the real deadline
		// has passed too, the unpaid invoice expires.
		e.Clock.Set(unpaid.ExpiresAt.Add(core.DefaultExpiryGrace - time.Second))
		lie(118)
		wantStatus(t, e, unpaid.ID, core.StatusAwaiting)
		e.Clock.Set(unpaid.ExpiresAt.Add(core.DefaultExpiryGrace))
		lie(119)
		wantStatus(t, e, unpaid.ID, core.StatusExpired)
		// A payment created while the head claims the future counts its
		// deadline from the chain's time — the later clock: a lie that
		// way makes deadlines later, never earlier.
		e.Clock.Advance(time.Minute)
		ahead := e.CreatePayment("5")
		if want := e.Clock.Now().Add(40*24*time.Hour - time.Minute + core.DefaultPaymentTTL); !ahead.ExpiresAt.After(want.Add(-time.Minute)) {
			t.Fatalf("a payment created under a head 40 days ahead expires at %s", ahead.ExpiresAt)
		}
		// And a clock lying backwards only delays: nothing expires while
		// chain time stands still, however late the wall clock is.
		e.Head(120)
		late := e.CreatePayment("5")
		e.Clock.Advance(3 * time.Hour)
		stuck := core.ChainHead{ChainID: 56, Number: 121, Hash: BlockHash(121, 0), Time: late.CreatedAt, Final: 121}
		noErr(t, e.Service.AdvanceHead(e.System(), stuck))
		wantStatus(t, e, late.ID, core.StatusAwaiting)
		e.Head(122)
		wantStatus(t, e, late.ID, core.StatusExpired)
	}},

	{"setup needs the setup token", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		const token = "st_9f2c4e81a7b3d05c6e19f4a2"
		e := newEnv(WithoutSetup(), WithConfig(func(c *core.Config) { c.SetupToken = token }))
		public := context.Background()
		status, err := e.Service.SetupStatus(public)
		noErr(t, err)
		if status.Completed || !status.TokenRequired {
			t.Fatalf("setup status: %+v", status)
		}
		in := e.SetupInput()

		// Without the token nothing of setup is reachable, and the token is
		// checked before anything else in the input is looked at.
		_, err = e.Service.CompleteSetup(public, in)
		wantIs(t, err, core.ErrSetupTokenRequired)
		wantKind(t, err, core.KindUnauthorized)
		garbage := core.SetupInput{Email: "not-an-email"}
		_, err = e.Service.CompleteSetup(public, garbage)
		wantIs(t, err, core.ErrSetupTokenRequired)
		for _, wrong := range []string{"x", token[:len(token)-1], token + "0", strings.ToUpper(token)} {
			in.SetupToken = wrong
			_, err = e.Service.CompleteSetup(public, in)
			wantIs(t, err, core.ErrSetupTokenInvalid)
			wantKind(t, err, core.KindForbidden)
			if strings.Contains(err.Error(), token) {
				t.Fatalf("the error reveals the token: %v", err)
			}
		}
		_, err = e.Service.ProbeRPC(public, core.ProbeRPCInput{URL: "https://rpc.example.org"})
		wantIs(t, err, core.ErrSetupTokenRequired)
		_, err = e.Service.ProbeRPC(public, core.ProbeRPCInput{URL: "https://rpc.example.org", SetupToken: "wrong"})
		wantIs(t, err, core.ErrSetupTokenInvalid)
		_, err = e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: XPub})
		wantIs(t, err, core.ErrSetupTokenRequired)
		_, err = e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: XPub, SetupToken: "wrong"})
		wantIs(t, err, core.ErrSetupTokenInvalid)
		// An API-key-shaped or operator-shaped actor does not exist yet; a
		// made-up one is not a way around the token.
		fake := core.WithActor(public, core.Actor{Kind: core.ActorOperator, ID: "op_x"})
		_, err = e.Service.CompleteSetup(fake, e.SetupInput())
		wantIs(t, err, core.ErrSetupTokenRequired)
		if status, _ := e.Service.SetupStatus(public); status.Completed {
			t.Fatal("setup completed without the token")
		}

		// With the token the wizard works.
		probe, err := e.Service.ProbeRPC(public, core.ProbeRPCInput{URL: "https://rpc.example.org", SetupToken: token})
		noErr(t, err)
		if probe.Outcome != core.ProbeOK {
			t.Fatalf("probe: %+v", probe)
		}
		_, err = e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: XPub, SetupToken: " " + token + "\n"})
		noErr(t, err)
		in = e.SetupInput()
		in.SetupToken = token
		res, err := e.Service.CompleteSetup(public, in)
		noErr(t, err)
		if res.Token == "" {
			t.Fatal("no session after setup")
		}
		// Afterwards the token means nothing: setup is over …
		status, _ = e.Service.SetupStatus(public)
		if !status.Completed || status.TokenRequired {
			t.Fatalf("setup status after setup: %+v", status)
		}
		_, err = e.Service.CompleteSetup(public, in)
		wantIs(t, err, core.ErrSetupCompleted)
		// … and the helpers belong to the operator, token or not.
		_, err = e.Service.ProbeRPC(public, core.ProbeRPCInput{URL: "https://rpc.example.org", SetupToken: token})
		wantIs(t, err, core.ErrUnauthorized)
		_, err = e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: XPub, SetupToken: token})
		wantIs(t, err, core.ErrUnauthorized)
	}},

	{"trusted in-process code sets up without the token", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithoutSetup(), WithConfig(func(c *core.Config) { c.SetupToken = "st_secret" }))
		_, err := e.Service.PreviewAddresses(e.System(), core.PreviewAddressesInput{XPub: XPub})
		noErr(t, err)
		_, err = e.Service.CompleteSetup(e.System(), e.SetupInput())
		noErr(t, err)
	}},

	{"the owner can see what an instance is configured for, and revoke every key", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		fresh := newEnv(WithoutSetup())
		sum, err := fresh.Service.InstanceSummary(fresh.System())
		noErr(t, err)
		if sum.SetupCompleted || sum.XPub != "" || sum.OperatorEmail != "" {
			t.Fatalf("summary of a fresh instance: %+v", sum)
		}

		e := newEnv(WithSetup(func(in *core.SetupInput) {
			in.RPCURLs = []string{"https://user:hunter2@rpc.example.org/v1/API-KEY-123?token=abc", "wss://node.example.net:8546/ws"}
		}))
		ctx, sys := e.Operator(), e.System()
		_, _, err = e.Service.CreateAPIKey(ctx, core.CreateAPIKeyInput{Name: "a", Scopes: core.Scopes})
		noErr(t, err)
		revokedKey, _, err := e.Service.CreateAPIKey(ctx, core.CreateAPIKeyInput{Name: "b", Scopes: core.Scopes})
		noErr(t, err)
		_, err = e.Service.RevokeAPIKey(ctx, revokedKey.ID)
		noErr(t, err)
		_, liveSecret, err := e.Service.CreateAPIKey(ctx, core.CreateAPIKeyInput{Name: "c", Scopes: core.Scopes, ExpiresAt: e.Clock.Now().Add(time.Hour)})
		noErr(t, err)
		for _, enabled := range []bool{true, true, false} {
			_, err := e.Service.CreateWebhookEndpoint(ctx, core.WebhookEndpointInput{URL: "https://hooks.example.com/x", Events: []core.EventType{paid}, Enabled: enabled})
			noErr(t, err)
		}
		e.CreatePayment("1")
		e.CreatePayment("1")

		sum, err = e.Service.InstanceSummary(sys)
		noErr(t, err)
		if !sum.SetupCompleted || sum.OperatorEmail != e.Email || sum.ChainID != 56 || sum.Asset != core.USDTBSC ||
			sum.XPub != XPub || sum.DerivationPath != core.DefaultDerivationPath || sum.NextAddressIndex != 2 ||
			sum.FirstAddress != Address(XPub, core.DefaultDerivationPath, uint32(0)) || sum.FirstAddressError != "" ||
			sum.APIKeys != 3 || sum.ActiveAPIKeys != 2 || sum.WebhookEndpoints != 3 || sum.EnabledWebhookEndpoints != 2 || sum.Sessions != 1 {
			t.Fatalf("summary: %+v", sum)
		}
		// RPC endpoints are reduced to scheme and host: no keys, no paths.
		if len(sum.RPCHosts) != 2 || sum.RPCHosts[0] != "https://rpc.example.org" || sum.RPCHosts[1] != "wss://node.example.net:8546" {
			t.Fatalf("rpc hosts: %v", sum.RPCHosts)
		}
		// Not for API keys.
		reader, _ := e.APIKey(core.ScopePaymentsRead)
		_, err = e.Service.InstanceSummary(reader)
		wantKind(t, err, core.KindForbidden)
		_, err = e.Service.RevokeAllAPIKeys(reader)
		wantKind(t, err, core.KindForbidden)
		_, err = e.Service.RevokeAllAPIKeys(context.Background())
		wantKind(t, err, core.KindUnauthorized)

		// Password reset alone leaves the keys working; revoking all ends them.
		noErr(t, e.Service.ResetOperatorPassword(sys, "a brand new passphrase"))
		_, err = e.Service.AuthenticateAPIKey(context.Background(), liveSecret)
		noErr(t, err)
		e.Clock.Advance(time.Minute)
		n, err := e.Service.RevokeAllAPIKeys(sys)
		noErr(t, err)
		if n != 3 { // a, c and the reader key; b was revoked before
			t.Fatalf("revoked %d keys, want 3", n)
		}
		_, err = e.Service.AuthenticateAPIKey(context.Background(), liveSecret)
		wantIs(t, err, core.ErrCredentialsRevoked)
		again, err := e.Service.RevokeAllAPIKeys(sys)
		noErr(t, err)
		keys, err := e.Service.ListAPIKeys(sys)
		noErr(t, err)
		if again != 0 || len(keys) != 4 {
			t.Fatalf("second revoke-all: %d, %d keys listed", again, len(keys))
		}
		for _, k := range keys {
			if k.RevokedAt.IsZero() {
				t.Fatalf("key %s survived revoke-all", k.Name)
			}
			if k.ID == revokedKey.ID && k.RevokedAt.Equal(e.Clock.Now()) {
				t.Fatal("revoke-all changed the time of an earlier revocation")
			}
		}
		sum, _ = e.Service.InstanceSummary(sys)
		if sum.ActiveAPIKeys != 0 || sum.APIKeys != 4 || sum.Sessions != 0 {
			t.Fatalf("summary after reset and revoke-all: %+v", sum)
		}
		// A broken wallet is reported, not hidden.
		e.Addresses.Err = errors.New("checksum mismatch")
		sum, err = e.Service.InstanceSummary(sys)
		noErr(t, err)
		if sum.FirstAddress != "" || sum.FirstAddressError != "checksum mismatch" {
			t.Fatalf("summary with a broken wallet: %+v", sum)
		}
	}},

	{"the tolerance is capped at 5 % of the amount; dust never pays", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		setTolerance(t, e, "1.00")
		e.Head(100)
		// A 1.00 invoice under a 1.00 tolerance needs 0.95, not "anything".
		p := e.CreatePayment("1.00")
		e.Pay(p, "0.000000000000000001", 101)
		e.Head(115)
		wantStatus(t, e, p.ID, core.StatusAwaiting) // dust: recorded, nothing more
		e.Pay(p, "0.949999999999999998", 116)
		e.Head(130)
		wantStatus(t, e, p.ID, core.StatusDetected) // 0.949999999999999999 so far
		e.Pay(p, "0.000000000000000001", 131)
		e.Head(145)
		wantAmounts(t, wantStatus(t, e, p.ID, core.StatusPaid), "0.95", "0.95")
		// On a normal-sized invoice the configured tolerance applies in full.
		big := e.CreatePayment("149")
		e.Pay(big, "148", 150)
		e.Head(164)
		wantStatus(t, e, big.ID, core.StatusPaid)
		short := e.CreatePayment("149")
		e.Pay(short, "147.99", 150)
		e.Head(164)
		wantStatus(t, e, short.ID, core.StatusDetected)
		// The smallest amounts get no tolerance at all.
		tiny := e.CreatePayment("0.000000000000000019")
		e.Pay(tiny, "0.000000000000000018", 170)
		e.Head(184)
		wantStatus(t, e, tiny.ID, core.StatusDetected)

		// The public variant: on a customer-chooses link the payer picks the
		// amount; picking one at or below the tolerance buys nothing. (It
		// is not even accepted: a chosen amount has a minimum of one unit,
		// At the minimum the capped tolerance leaves 0.95 to pay.)
		link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Top-up", Active: true, MaxUses: 1})
		noErr(t, err)
		_, _, err = e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "0.50"})
		wantKind(t, err, core.KindInvalid)
		co, _, err := e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "1"})
		noErr(t, err)
		e.Pay(e.Payment(co.ID), "0.000000000000000001", 200)
		e.Head(214)
		wantStatus(t, e, co.ID, core.StatusAwaiting)
		lv, err := e.Service.GetLink(e.Operator(), link.ID)
		noErr(t, err)
		if lv.Uses != 0 {
			t.Fatalf("dust consumed a link use: %+v", lv)
		}
	}},

	{"only the operator ever sees a webhook signing secret", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		op := e.Operator()
		key, _ := e.APIKey(core.ScopeWebhooksManage)
		theirs, err := e.Service.CreateWebhookEndpoint(op, core.WebhookEndpointInput{URL: "https://shop.example.com/hook", Events: []core.EventType{paid}, Enabled: true})
		noErr(t, err)
		if theirs.Secret == "" {
			t.Fatal("the operator did not get the new endpoint's secret")
		}
		_, err = e.Service.RotateWebhookSecret(op, theirs.ID)
		noErr(t, err)
		revealed, err := e.Service.RevealWebhookSecret(op, theirs.ID)
		noErr(t, err)
		if revealed.Secret == "" || revealed.PreviousSecret != theirs.Secret {
			t.Fatalf("operator reveal: %+v", revealed)
		}

		// A webhooks:manage key manages endpoints without reading secrets.
		got, err := e.Service.GetWebhookEndpoint(key, theirs.ID)
		noErr(t, err)
		list, err := e.Service.ListWebhookEndpoints(key)
		noErr(t, err)
		updated, err := e.Service.UpdateWebhookEndpoint(key, theirs.ID, core.WebhookEndpointInput{URL: "https://shop.example.com/hook2", Events: []core.EventType{paid}, Enabled: true})
		noErr(t, err)
		for what, ep := range map[string]core.WebhookEndpoint{"get": got.WebhookEndpoint, "list": list[0].WebhookEndpoint, "update": updated} {
			if ep.Secret != "" || ep.PreviousSecret != "" || ep.ID != theirs.ID {
				t.Fatalf("%s leaks a signing secret to an API key: %+v", what, ep)
			}
		}
		if updated.URL != "https://shop.example.com/hook2" || got.PreviousSecretExpiresAt.IsZero() {
			t.Fatalf("non-secret fields must still be there: %+v / %+v", updated, got)
		}
		_, err = e.Service.RevealWebhookSecret(key, theirs.ID)
		wantKind(t, err, core.KindForbidden)
		_, err = e.Service.RotateWebhookSecret(key, theirs.ID)
		wantKind(t, err, core.KindForbidden)
		// The stored secret is untouched by all of that.
		after, err := e.Service.RevealWebhookSecret(op, theirs.ID)
		noErr(t, err)
		if after.Secret != revealed.Secret || after.PreviousSecret != revealed.PreviousSecret {
			t.Fatal("managing an endpoint with an API key changed its secrets")
		}
		// A key that creates an endpoint receives that endpoint's secret —
		// once: reading it back later gives nothing.
		own, err := e.Service.CreateWebhookEndpoint(key, core.WebhookEndpointInput{URL: "https://integration.example.com/hook", Events: []core.EventType{paid}, Enabled: true})
		noErr(t, err)
		if !strings.HasPrefix(own.Secret, "whsec_") {
			t.Fatalf("creator did not receive the secret: %+v", own)
		}
		back, err := e.Service.GetWebhookEndpoint(key, own.ID)
		noErr(t, err)
		if back.Secret != "" {
			t.Fatal("an API key read a secret back after creation")
		}
		// The operator and system actors see everything.
		for _, ctx := range []context.Context{op, e.System()} {
			full, err := e.Service.GetWebhookEndpoint(ctx, theirs.ID)
			noErr(t, err)
			all, err := e.Service.ListWebhookEndpoints(ctx)
			noErr(t, err)
			if full.Secret != revealed.Secret || len(all) != 2 {
				t.Fatal("the operator must see the secrets")
			}
			for _, ep := range all {
				if want := map[string]string{theirs.ID: revealed.Secret, own.ID: own.Secret}[ep.ID]; ep.Secret != want {
					t.Fatalf("the operator must see the secret of %s in the list", ep.ID)
				}
			}
		}
	}},

	{"a limited link's checkouts are short-lived and hold a use until they close", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		e.Head(100)
		public := context.Background()
		link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Slug: "launch", Name: "Launch ticket", Amount: "25", MaxUses: 3, Active: true})
		noErr(t, err)
		checkout := func() (core.CheckoutView, error) {
			co, _, err := e.Service.CreatePaymentFromLink(public, "launch", core.LinkPaymentInput{})
			return co, err
		}
		status := func() core.LinkStatus {
			t.Helper()
			page, err := e.Service.GetLinkCheckout(public, "launch")
			noErr(t, err)
			return page.Status
		}
		uses := func() int {
			t.Helper()
			lv, err := e.Service.GetLink(e.Operator(), link.ID)
			noErr(t, err)
			return lv.Uses
		}
		// Three strangers open a checkout and never pay.
		var squat [3]core.CheckoutView
		for i := range squat {
			squat[i], err = checkout()
			noErr(t, err)
			// The customer has the reservation window to pay, not the
			// instance's 30-minute payment TTL.
			if got := squat[i].ExpiresAt.Sub(squat[i].CreatedAt); got != core.DefaultLinkReservation {
				t.Fatalf("a limited link's checkout lives %s, want %s", got, core.DefaultLinkReservation)
			}
			e.Clock.Advance(time.Minute)
		}
		// While they are open the link is full, so that three honest
		// customers checking out together cannot be oversold.
		_, err = checkout()
		wantIs(t, err, core.ErrLinkLimitReached)
		if status() != core.LinkLimit {
			t.Fatalf("link status while reserved: %s", status())
		}
		// A use is released when the checkout's deadline has passed by both
		// clocks. The wall clock alone is not enough: a payment made in
		// time may not have been reported yet.
		e.Clock.Set(squat[0].ExpiresAt.Add(core.DefaultExpiryGrace))
		_, err = checkout()
		wantIs(t, err, core.ErrLinkLimitReached)
		e.Head(101)
		wantStatus(t, e, squat[0].ID, core.StatusExpired)
		if status() != core.LinkActive {
			t.Fatalf("link status after a checkout closed: %s", status())
		}
		customer, err := checkout()
		noErr(t, err)
		_, err = checkout() // only one checkout had closed
		wantIs(t, err, core.ErrLinkLimitReached)

		// A checkout whose full amount is in sight holds its use past its
		// deadline, until it is counted.
		e.Pay(e.Payment(customer.ID), "25", 102)
		e.Clock.Advance(core.DefaultLinkReservation + 2*time.Minute)
		e.Head(103)
		wantStatus(t, e, customer.ID, core.StatusConfirming)
		wantStatus(t, e, squat[1].ID, core.StatusExpired)
		wantStatus(t, e, squat[2].ID, core.StatusExpired)
		a, err := checkout()
		noErr(t, err)
		b, err := checkout()
		noErr(t, err)
		_, err = checkout() // 1 confirming + 2 open = 3
		wantIs(t, err, core.ErrLinkLimitReached)
		// Paid uses count for good.
		e.Head(116)
		wantStatus(t, e, customer.ID, core.StatusPaid)
		if uses() != 1 {
			t.Fatalf("uses after one payment: %d", uses())
		}

		// Whoever holds the address of a closed checkout can still send
		// money to it. That is late funds on an expired payment — flagged
		// for the merchant — and never a use of the link.
		e.Pay(e.Payment(squat[0].ID), "25", 117)
		e.Head(131)
		late := wantStatus(t, e, squat[0].ID, core.StatusExpired)
		if late.Attention != core.AttentionLateFunds || !late.NeedsAttention {
			t.Fatalf("a payment to a closed checkout: %+v", late)
		}
		wantEvents(t, e, squat[0].ID, created, expired, lateFunds)
		if uses() != 1 {
			t.Fatalf("a late payment was counted as a use: %d", uses())
		}
		// The same when no head was processed between the deadline and the
		// payment: the transfer is dated, not the moment it was processed.
		e.Clock.Set(e.Payment(a.ID).ExpiresAt.Add(core.DefaultExpiryGrace))
		e.Pay(e.Payment(a.ID), "25", 132)
		wantStatus(t, e, a.ID, core.StatusAwaiting)
		e.Head(146)
		lateA := wantStatus(t, e, a.ID, core.StatusExpired)
		if lateA.Attention != core.AttentionLateFunds {
			t.Fatalf("a payment after the deadline, before any head: %+v", lateA)
		}
		wantEvents(t, e, a.ID, created, expired, lateFunds)
		wantStatus(t, e, b.ID, core.StatusExpired)
		if uses() != 1 {
			t.Fatalf("a late payment was counted as a use: %d", uses())
		}
		// Two uses are left, and exactly two checkouts can be open.
		_, err = checkout()
		noErr(t, err)
		_, err = checkout()
		noErr(t, err)
		_, err = checkout() // 1 paid + 2 open
		wantIs(t, err, core.ErrLinkLimitReached)
	}},

	{"a limited link is never oversold, however the checkouts are timed", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		setPaymentDefaults(t, e, 1, "0")
		e.Head(100)
		public := context.Background()
		const maxUses = 2
		link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Early bird", Amount: "10", MaxUses: maxUses, Active: true})
		noErr(t, err)
		// One buyer opens MaxUses checkouts in every window, lets them
		// close, and pays all of them at the end.
		var mine []core.CheckoutView
		block := uint64(100)
		for round := range 3 {
			for range maxUses {
				co, _, err := e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{})
				noErr(t, err)
				mine = append(mine, co)
			}
			_, _, err = e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{})
			wantIs(t, err, core.ErrLinkLimitReached)
			if round == 2 {
				break // the last two are still inside their window
			}
			e.Clock.Advance(core.DefaultLinkReservation + core.DefaultExpiryGrace)
			block++
			e.Head(block)
		}
		for _, co := range mine {
			e.Pay(e.Payment(co.ID), "10", block+1)
		}
		e.Head(block + 1)
		paid, flagged := 0, 0
		for _, co := range mine {
			switch p := e.Payment(co.ID); {
			case p.Status == core.StatusPaid:
				paid++
			case p.Status == core.StatusExpired && p.Attention == core.AttentionLateFunds:
				flagged++
			default:
				t.Fatalf("payment %s: %+v", co.ID, p)
			}
		}
		lv, err := e.Service.GetLink(e.Operator(), link.ID)
		noErr(t, err)
		if paid != maxUses || lv.Uses != maxUses || flagged != len(mine)-maxUses || lv.Status != core.LinkLimit {
			t.Fatalf("a link limited to %d uses: %d paid, %d late, link %+v", maxUses, paid, flagged, lv)
		}
	}},

	{"with a negative LinkReservation a checkout holds its use for the payment TTL", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithConfig(func(c *core.Config) { c.LinkReservation = -1 }))
		e.Head(100)
		public := context.Background()
		_, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Slug: "launch", Name: "Launch", Amount: "25", MaxUses: 1, Active: true})
		noErr(t, err)
		first, _, err := e.Service.CreatePaymentFromLink(public, "launch", core.LinkPaymentInput{})
		noErr(t, err)
		if got := first.ExpiresAt.Sub(first.CreatedAt); got != core.DefaultPaymentTTL {
			t.Fatalf("checkout lifetime %s, want the payment TTL", got)
		}
		e.Clock.Advance(core.DefaultLinkReservation + 2*time.Minute)
		e.Head(101)
		_, _, err = e.Service.CreatePaymentFromLink(public, "launch", core.LinkPaymentInput{})
		wantIs(t, err, core.ErrLinkLimitReached)
		e.Expire(e.Payment(first.ID))
		e.Head(102)
		_, _, err = e.Service.CreatePaymentFromLink(public, "launch", core.LinkPaymentInput{})
		noErr(t, err)
	}},

	{"a link without MaxUses keeps the payment TTL", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Donate", Active: true})
		noErr(t, err)
		co, _, err := e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "3"})
		noErr(t, err)
		if got := co.ExpiresAt.Sub(co.CreatedAt); got != core.DefaultPaymentTTL {
			t.Fatalf("checkout lifetime %s, want the payment TTL", got)
		}
	}},

	{"dust neither holds a link's use nor raises anything", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		setPaymentDefaults(t, e, 1, "0")
		e.Head(100)
		public := context.Background()
		link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Limited", Amount: "100", MaxUses: 1, Active: true})
		noErr(t, err)
		squat, _, err := e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{})
		noErr(t, err)
		e.Pay(e.Payment(squat.ID), "0.000000000000000001", 101)
		e.Head(101)
		got := wantStatus(t, e, squat.ID, core.StatusAwaiting)
		wantAmounts(t, got, "0.000000000000000001", "0.000000000000000001") // recorded
		e.Expire(e.Payment(squat.ID))
		e.Head(102)
		got = wantStatus(t, e, squat.ID, core.StatusExpired)
		if got.NeedsAttention || got.Attention != "" {
			t.Fatalf("a dusted checkout needs nobody's attention: %+v", got)
		}
		wantEvents(t, e, squat.ID, created, expired)
		// More dust afterwards: still nothing.
		e.Pay(e.Payment(squat.ID), "0.9", 103)
		e.Head(104)
		if got = wantStatus(t, e, squat.ID, core.StatusExpired); got.NeedsAttention {
			t.Fatalf("late dust raised attention: %+v", got)
		}
		wantEvents(t, e, squat.ID, created, expired)
		_, _, err = e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{})
		noErr(t, err)
	}},

	{"dust does not take the cancel button away", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		setPaymentDefaults(t, e, 1, "0")
		e.Head(100)
		p := e.CreatePayment("100")
		e.Pay(p, "0.999999999999999999", 101)
		e.Head(101)
		wantStatus(t, e, p.ID, core.StatusAwaiting)
		got, err := e.Service.CancelPayment(e.Operator(), p.ID)
		noErr(t, err)
		if got.Status != core.StatusExpired || got.CancelledAt == nil || got.NeedsAttention {
			t.Fatalf("cancelled dusted payment: %+v", got)
		}
		wantEvents(t, e, p.ID, created, expired)
		// One unit is a partial payment: that invoice runs its course.
		q := e.CreatePayment("100")
		e.Pay(q, "1", 102)
		_, err = e.Service.CancelPayment(e.Operator(), q.ID)
		wantIs(t, err, core.ErrPaymentHasFunds)
	}},

	{"a partial payment still surfaces, and frees the link's use at the deadline", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		e.Head(100)
		public := context.Background()
		link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Limited", Amount: "100", MaxUses: 1, Active: true})
		noErr(t, err)
		first, _, err := e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{})
		noErr(t, err)
		e.Pay(e.Payment(first.ID), "60", 101) // pending for 15 blocks
		e.Head(101)
		wantStatus(t, e, first.ID, core.StatusDetected)
		_, _, err = e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{})
		wantIs(t, err, core.ErrLinkLimitReached)
		// Past the deadline the pending 60 keeps the payment open, but it
		// can no longer become a paid use: the use is free.
		e.Expire(e.Payment(first.ID))
		e.Head(102)
		wantStatus(t, e, first.ID, core.StatusDetected)
		second, _, err := e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{})
		noErr(t, err)
		// The first payer tops up, too late; the second pays in time.
		e.Pay(e.Payment(first.ID), "40", 103)
		e.Pay(e.Payment(second.ID), "100", 103)
		e.Head(117)
		got := wantStatus(t, e, first.ID, core.StatusUnderpaid)
		wantAmounts(t, got, "100.00", "100.00")
		if got.Attention != core.AttentionUnderpaid || !got.NeedsAttention {
			t.Fatalf("a real partial payment must surface: %+v", got)
		}
		wantEvents(t, e, first.ID, created, detected, underpaid, lateFunds)
		wantStatus(t, e, second.ID, core.StatusPaid)
		lv, err := e.Service.GetLink(e.Operator(), link.ID)
		noErr(t, err)
		if lv.Uses != 1 || lv.TotalCollected != "100.00" {
			t.Fatalf("link after one in-time and one late payment: %+v", lv)
		}
	}},

	{"dusted link checkouts leave the watch list like unfunded ones", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithConfig(func(c *core.Config) { c.MaxWatchedAddresses = 5 }))
		setPaymentDefaults(t, e, 1, "0")
		e.Head(100)
		public := context.Background()
		link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Donate", Active: true})
		noErr(t, err)
		linkWatched := func() int {
			t.Helper()
			ns, err := e.Service.NodeStatus(e.System())
			noErr(t, err)
			return ns.WatchedLinkAddresses
		}
		var dusted []core.CheckoutView
		for range 4 {
			co, _, err := e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{Amount: "100"})
			noErr(t, err)
			e.Pay(e.Payment(co.ID), "0.000000000000000001", 101)
			dusted = append(dusted, co)
		}
		// One that somebody really tried to pay: under half, and half.
		short, _, err := e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{Amount: "100"})
		noErr(t, err)
		e.Pay(e.Payment(short.ID), "49.99", 101)
		e.Head(101)
		_, _, err = e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{Amount: "100"})
		wantIs(t, err, core.ErrWatchListFull)
		e.Expire(e.Payment(short.ID))
		e.Head(102)
		for _, co := range dusted {
			wantStatus(t, e, co.ID, core.StatusExpired)
		}
		wantStatus(t, e, short.ID, core.StatusUnderpaid)
		// Dust sent every now and then does not keep an address watched.
		e.Clock.Advance(core.DefaultUnfundedWatchWindow - time.Minute)
		if linkWatched() != 5 {
			t.Fatalf("watched shortly after expiry: %d", linkWatched())
		}
		e.Pay(e.Payment(dusted[0].ID), "0.5", 103)
		e.Head(103)
		e.Clock.Advance(2*time.Minute + core.WatchRetirementMargin)
		e.Head(104) // the chain the watcher has processed is past the hour too
		if linkWatched() != 0 {
			t.Fatalf("an hour after they closed, %d dusted or half-paid link checkouts are still watched", linkWatched())
		}
		half, _, err := e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{Amount: "100"})
		noErr(t, err)
		// Half the amount is real money: that address stays for the long
		// window.
		e.Pay(e.Payment(half.ID), "50", 104)
		e.Head(105)
		e.Expire(e.Payment(half.ID))
		e.Head(105)
		wantStatus(t, e, half.ID, core.StatusUnderpaid)
		e.Clock.Advance(core.DefaultWatchWindow - time.Hour)
		e.Head(106)
		if linkWatched() != 1 {
			t.Fatalf("a half-paid link payment must stay watched: %d", linkWatched())
		}
		// On a link for next to nothing, "half" of the amount is not enough
		// either: at least one whole unit. (Only the merchant can make such
		// a link, by giving it a fixed price: an amount the customer chooses
		// is at least one unit.)
		micro, err := e.Service.CreateLink(e.Relogin(), core.LinkInput{Name: "Micro", Amount: "0.000000000000000002", Active: true})
		noErr(t, err)
		tiny, _, err := e.Service.CreatePaymentFromLink(public, micro.Slug, core.LinkPaymentInput{})
		noErr(t, err)
		e.Pay(e.Payment(tiny.ID), "0.000000000000000002", 107)
		e.Head(107)
		wantStatus(t, e, tiny.ID, core.StatusPaid)
		e.Clock.Advance(core.DefaultPaymentTTL + core.DefaultExpiryGrace + core.DefaultUnfundedWatchWindow + time.Minute + core.WatchRetirementMargin)
		e.Head(108)
		if linkWatched() != 0 {
			t.Fatalf("a link payment of two base units is still watched an hour after its deadline: %d", linkWatched())
		}
	}},

	{"the merchant's payments and the links' share of the watch list are separate", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithConfig(func(c *core.Config) { c.MaxWatchedAddresses = 3 }))
		e.Head(100)
		public := context.Background()
		link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Donate", Active: true})
		noErr(t, err)
		// Any number of merchant payments leaves the links' share alone …
		for range 6 {
			e.CreatePayment("10")
		}
		for range 3 {
			_, _, err = e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{Amount: "1"})
			noErr(t, err)
		}
		// … and a full share refuses link payments only.
		_, _, err = e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{Amount: "1"})
		wantIs(t, err, core.ErrWatchListFull)
		e.CreatePayment("10")
		ns, err := e.Service.NodeStatus(e.System())
		noErr(t, err)
		if ns.WatchedAddresses != 10 || ns.WatchedLinkAddresses != 3 || ns.MaxWatchedAddresses != 3 || !ns.WatchListFull {
			t.Fatalf("node status: %+v", ns)
		}
		// A merchant invoice keeps the long window for any payment that is
		// not dust: a customer who paid a tenth may well send the rest
		// tomorrow.
		p := e.CreatePayment("100")
		e.Pay(p, "10", 101)
		dusty := e.CreatePayment("100")
		e.Pay(dusty, "0.5", 101)
		e.Expire(p)
		e.Head(120)
		wantStatus(t, e, p.ID, core.StatusUnderpaid)
		wantStatus(t, e, dusty.ID, core.StatusExpired)
		e.Clock.Advance(2 * time.Hour)
		e.Head(121)
		ns, err = e.Service.NodeStatus(e.System())
		noErr(t, err)
		if ns.WatchedAddresses != 1 || ns.WatchedLinkAddresses != 0 || ns.WatchListFull {
			t.Fatalf("two hours later only the part-paid invoice may be watched: %+v", ns)
		}
	}},

	{"a transfer above the processed head is dated by that head", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		// What the production chain adapter does: it reports no block
		// time. The core then dates a transfer by the head it had
		// processed when it first saw it.
		e := newEnv()
		setPaymentDefaults(t, e, 1, "0")
		e.Head(100)
		undated := func(p core.PaymentView, amount string, block uint64) core.TransferObservation {
			obs := e.Transfer(p, amount, block)
			obs.BlockTime = time.Time{}
			return obs
		}
		detail := func(id string) core.PaymentDetail {
			t.Helper()
			d, err := e.Service.GetPaymentDetail(e.Operator(), id)
			noErr(t, err)
			return d
		}
		// The last processed head is from before the deadline: the
		// transfer may have been mined in time, and counts.
		slow := e.CreatePayment("10")
		e.Expire(slow)
		e.Observe(undated(slow, "10", 101))
		e.Head(101)
		wantStatus(t, e, slow.ID, core.StatusPaid)
		if d := detail(slow.ID); len(d.Transfers) != 1 || d.Transfers[0].BlockTime != nil {
			t.Fatalf("transfer of the slow payment: %+v", d.Transfers)
		}
		// The processed head is already past the deadline when the
		// transfer shows up in a later block: it is late, whatever the
		// payment's status says at that moment.
		late := e.CreatePayment("10")
		e.Expire(late)
		e.Head(102)
		wantStatus(t, e, late.ID, core.StatusExpired)
		e.Observe(undated(late, "10", 103))
		e.Head(103)
		got := wantStatus(t, e, late.ID, core.StatusExpired)
		if got.Attention != core.AttentionLateFunds {
			t.Fatalf("late payment: %+v", got)
		}
		// A transfer at or below the processed head cannot be dated this
		// way and gets the benefit of the doubt.
		below := e.CreatePayment("10")
		e.Expire(below)
		e.Observe(undated(below, "10", 103))
		e.Head(104)
		wantStatus(t, e, below.ID, core.StatusPaid)
	}},

	{"a one-time recovery code, minted on the server", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		bg := context.Background()
		login := func(password, code string) error {
			_, err := e.Service.Login(bg, core.LoginInput{Email: e.Email, Password: password, RecoveryCode: code})
			return err
		}
		valid := func(code string) bool {
			t.Helper()
			ok, err := e.Service.RecoveryCodeValid(bg, code)
			noErr(t, err)
			return ok
		}
		// Only the system mints one.
		for _, ctx := range []context.Context{bg, e.Operator()} {
			_, err := e.Service.MintRecoveryCode(ctx)
			if core.KindOf(err) != core.KindUnauthorized && core.KindOf(err) != core.KindForbidden {
				t.Fatalf("MintRecoveryCode without the system actor: %v", err)
			}
		}
		if valid("") || valid("hmnr_guess") {
			t.Fatal("a code is valid before any was minted")
		}
		wantIs(t, login(e.Password, "hmnr_guess"), core.ErrRecoveryCodeInvalid)

		first, err := e.Service.MintRecoveryCode(e.System())
		noErr(t, err)
		if !strings.HasPrefix(first.Code, "hmnr_") || len(first.Code) != 31 || !first.ExpiresAt.Equal(e.Clock.Now().Add(core.RecoveryCodeTTL)) {
			t.Fatalf("recovery code: %q until %v", first.Code, first.ExpiresAt)
		}
		// Hashed at rest, and never shown again.
		op, err := e.Service.GetOperator(e.Operator())
		noErr(t, err)
		if op.RecoveryHash != "" || op.PasswordHash != "" || !op.RecoveryExpiresAt.IsZero() {
			t.Fatalf("GetOperator leaks credential state: %+v", op)
		}
		noErr(t, e.Store.View(bg, func(r core.Reader) error {
			stored, err := r.GetOperator(bg)
			if err == nil && (stored.RecoveryHash == "" || strings.Contains(stored.RecoveryHash, first.Code[5:])) {
				t.Errorf("stored recovery state: %+v", stored)
			}
			return err
		}))
		// A new code replaces the old one.
		code, err := e.Service.MintRecoveryCode(e.System())
		noErr(t, err)
		if valid(first.Code) || !valid(code.Code) || !valid("  "+code.Code+"\n") {
			t.Fatal("minting must replace the previous code")
		}
		wantIs(t, login(e.Password, first.Code), core.ErrRecoveryCodeInvalid)

		// It never replaces the password, and every attempt is counted:
		// after RecoveryCodeMaxAttempts the code is dead, right password
		// or not.
		for i := range core.RecoveryCodeMaxAttempts {
			if !valid(code.Code) {
				t.Fatalf("code dead after %d attempts", i)
			}
			wantIs(t, login("not the password", code.Code), core.ErrInvalidCredentials)
		}
		if valid(code.Code) {
			t.Fatal("the code survived its attempt limit")
		}
		wantIs(t, login(e.Password, code.Code), core.ErrRecoveryCodeInvalid)
		noErr(t, login(e.Password, "")) // the ordinary login is unaffected

		// One successful login uses it up.
		code, err = e.Service.MintRecoveryCode(e.System())
		noErr(t, err)
		wantIs(t, login("not the password", code.Code), core.ErrInvalidCredentials)
		noErr(t, login(e.Password, code.Code))
		if valid(code.Code) {
			t.Fatal("the code works twice")
		}
		wantIs(t, login(e.Password, code.Code), core.ErrRecoveryCodeInvalid)

		// It expires after 15 minutes.
		code, err = e.Service.MintRecoveryCode(e.System())
		noErr(t, err)
		e.Clock.Advance(core.RecoveryCodeTTL - time.Second)
		if !valid(code.Code) {
			t.Fatal("the code expired early")
		}
		e.Clock.Advance(time.Second)
		if valid(code.Code) {
			t.Fatal("the code outlived its 15 minutes")
		}
		wantIs(t, login(e.Password, code.Code), core.ErrRecoveryCodeInvalid)

		// A password reset or change cancels an outstanding code.
		code, err = e.Service.MintRecoveryCode(e.System())
		noErr(t, err)
		noErr(t, e.Service.ResetOperatorPassword(e.System(), "a brand new passphrase"))
		if valid(code.Code) {
			t.Fatal("a code minted before the reset still works")
		}
		code, err = e.Service.MintRecoveryCode(e.System())
		noErr(t, err)
		res, err := e.Service.Login(bg, core.LoginInput{Email: e.Email, Password: "a brand new passphrase", RecoveryCode: code.Code})
		noErr(t, err)
		if res.Operator.PasswordHash != "" || res.Operator.RecoveryHash != "" {
			t.Fatalf("login result leaks credential state: %+v", res.Operator)
		}
		code, err = e.Service.MintRecoveryCode(e.System())
		noErr(t, err)
		actor, err := e.Service.AuthenticateSession(bg, res.Token)
		noErr(t, err)
		noErr(t, e.Service.ChangePassword(core.WithActor(bg, actor), "a brand new passphrase", "yet another passphrase"))
		if valid(code.Code) {
			t.Fatal("a code minted before the password change still works")
		}
	}},

	{"a recovery code needs a configured instance", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithoutSetup())
		_, err := e.Service.MintRecoveryCode(e.System())
		wantIs(t, err, core.ErrSetupRequired)
		ok, err := e.Service.RecoveryCodeValid(context.Background(), "hmnr_anything")
		if ok || err != nil {
			t.Fatalf("RecoveryCodeValid before setup: %v, %v", ok, err)
		}
		_, err = e.Service.DeviceBinding(context.Background())
		wantIs(t, err, core.ErrSetupRequired)
	}},

	{"the device binding moves with every password change and with nothing else", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		bg := context.Background()
		binding := func() string {
			t.Helper()
			b, err := e.Service.DeviceBinding(bg)
			noErr(t, err)
			if len(b) != 32 {
				t.Fatalf("binding %q", b)
			}
			return b
		}
		b0 := binding()
		// Logging in, minting and using a recovery code, sessions coming
		// and going: none of that moves it.
		code, err := e.Service.MintRecoveryCode(e.System())
		noErr(t, err)
		res, err := e.Service.Login(bg, core.LoginInput{Email: e.Email, Password: e.Password, RecoveryCode: code.Code})
		noErr(t, err)
		noErr(t, e.Service.Logout(bg, res.Token))
		if binding() != b0 {
			t.Fatal("the binding moved without a password change")
		}
		noErr(t, e.Service.ResetOperatorPassword(e.System(), "a brand new passphrase"))
		b1 := binding()
		if b1 == b0 {
			t.Fatal("a password reset must move the binding")
		}
		res, err = e.Service.Login(bg, core.LoginInput{Email: e.Email, Password: "a brand new passphrase"})
		noErr(t, err)
		actor, err := e.Service.AuthenticateSession(bg, res.Token)
		noErr(t, err)
		noErr(t, e.Service.ChangePassword(core.WithActor(bg, actor), "a brand new passphrase", "yet another passphrase"))
		b2 := binding()
		if b2 == b1 || b2 == b0 {
			t.Fatal("a password change must move the binding")
		}
		// A refused change moves nothing.
		if err := e.Service.ChangePassword(core.WithActor(bg, actor), "wrong", "one more passphrase!"); err == nil {
			t.Fatal("password changed with a wrong current password")
		}
		if binding() != b2 {
			t.Fatal("a refused password change moved the binding")
		}
	}},

	{"webhook deliveries in the payment detail need webhooks:manage", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		_, err := e.Service.CreateWebhookEndpoint(e.Operator(), core.WebhookEndpointInput{
			URL: "https://shop.example.com/hook?token=s3cr3t", Events: []core.EventType{created}, Enabled: true})
		noErr(t, err)
		p := e.CreatePayment("10")
		_, err = e.Service.PlanWebhookDeliveries(e.System())
		noErr(t, err)
		reader, _ := e.APIKey(core.ScopePaymentsRead)
		both, _ := e.APIKey(core.ScopePaymentsRead, core.ScopeWebhooksManage)
		d, err := e.Service.GetPaymentDetail(reader, p.ID)
		noErr(t, err)
		if d.Deliveries != nil || len(d.Events) != 1 || d.Payment.ID != p.ID {
			t.Fatalf("detail for a payments:read key: %d deliveries, %d events", len(d.Deliveries), len(d.Events))
		}
		for who, ctx := range map[string]context.Context{"operator": e.Operator(), "system": e.System(), "read+webhooks key": both} {
			d, err := e.Service.GetPaymentDetail(ctx, p.ID)
			noErr(t, err)
			if len(d.Deliveries) != 1 {
				t.Fatalf("%s sees %d deliveries, want 1", who, len(d.Deliveries))
			}
		}
	}},

	{"never-funded payments leave the watch list after an hour", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		e.Head(100)
		link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Donate", Active: true})
		noErr(t, err)
		var last core.CheckoutView
		for range 30 {
			last, _, err = e.Service.CreatePaymentFromLink(context.Background(), link.Slug, core.LinkPaymentInput{Amount: "1"})
			noErr(t, err)
		}
		funded := e.CreatePayment("10") // one that somebody tried to pay
		e.Pay(funded, "4", 101)
		watched := func() int {
			t.Helper()
			cs, err := e.Service.ChainState(e.System())
			noErr(t, err)
			ns, err := e.Service.NodeStatus(e.System())
			noErr(t, err)
			if ns.WatchedAddresses != len(cs.Addresses) {
				t.Fatalf("node status counts %d watched addresses, the watch list has %d", ns.WatchedAddresses, len(cs.Addresses))
			}
			return len(cs.Addresses)
		}
		if watched() != 31 {
			t.Fatalf("open payments watched: %d", watched())
		}
		e.Expire(e.Payment(last.ID))
		e.Head(116)
		wantStatus(t, e, last.ID, core.StatusExpired)
		wantStatus(t, e, funded.ID, core.StatusUnderpaid)
		// The realistic late payer — minutes late — is still seen …
		e.Clock.Advance(core.DefaultUnfundedWatchWindow - time.Minute)
		if watched() != 31 {
			t.Fatalf("watched shortly after expiry: %d, want 31", watched())
		}
		// … after an hour — once the processed chain is past it too — the
		// never-funded ones are gone; the one that saw funds keeps the long
		// window.
		e.Clock.Advance(2*time.Minute + core.WatchRetirementMargin)
		e.Head(117)
		if watched() != 1 {
			t.Fatalf("watched an hour after expiry: %d, want only the funded payment", watched())
		}
		e.Clock.Advance(core.DefaultWatchWindow - 2*time.Hour)
		if watched() != 1 {
			t.Fatalf("funded payment dropped early: %d", watched())
		}
		e.Clock.Advance(3 * time.Hour)
		e.Head(118)
		if watched() != 0 {
			t.Fatalf("watched after the long window: %d", watched())
		}
		// Late funds inside the hour move a payment to the long window.
		p := e.CreatePayment("5")
		e.Expire(p)
		e.Head(200)
		wantStatus(t, e, p.ID, core.StatusExpired)
		e.Clock.Advance(10 * time.Minute)
		e.Pay(p, "5", 201)
		e.Clock.Advance(3 * 24 * time.Hour)
		if watched() != 1 {
			t.Fatalf("a payment with late funds must stay watched: %d", watched())
		}
	}},

	{"at the watch-list cap public links refuse, merchants do not", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithConfig(func(c *core.Config) { c.MaxWatchedAddresses = 5 }))
		public := context.Background()
		link, err := e.Service.CreateLink(e.Operator(), core.LinkInput{Name: "Donate", Active: true})
		noErr(t, err)
		fromLink := func(key string) error {
			_, _, err := e.Service.CreatePaymentFromLink(public, link.Slug, core.LinkPaymentInput{Amount: "1", IdempotencyKey: key})
			return err
		}
		for i := range 5 {
			noErr(t, fromLink("k"+string(rune('a'+i))))
		}
		ns, err := e.Service.NodeStatus(e.System())
		noErr(t, err)
		if ns.WatchedAddresses != 5 || ns.WatchedLinkAddresses != 5 || ns.MaxWatchedAddresses != 5 || !ns.WatchListFull {
			t.Fatalf("node status at the cap: %+v", ns)
		}
		err = fromLink("one-too-many")
		wantIs(t, err, core.ErrWatchListFull)
		wantKind(t, err, core.KindUnavailable)
		// A customer who already has a payment gets it back.
		noErr(t, fromLink("ka"))
		// The merchant's own payments are never refused.
		p := e.CreatePayment("10")
		ns, _ = e.Service.NodeStatus(e.System())
		if ns.WatchedAddresses != 6 || !ns.WatchListFull {
			t.Fatalf("node status above the cap: %+v", ns)
		}
		// Addresses age out and the link works again.
		e.Head(100)
		e.Pay(p, "10", 101)
		e.Clock.Advance(core.DefaultPaymentTTL + core.DefaultExpiryGrace)
		e.Head(200) // the unpaid ones expire now, and stay watched for one more hour
		e.Clock.Advance(core.DefaultUnfundedWatchWindow + time.Minute + core.WatchRetirementMargin)
		e.Head(201) // … by the processed chain's clock too
		ns, _ = e.Service.NodeStatus(e.System())
		if ns.WatchedAddresses != 1 || ns.WatchListFull {
			t.Fatalf("node status after expiry: %+v", ns)
		}
		noErr(t, fromLink("later"))
	}},

	{"text, logo and URL validation", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		sys, op := e.System(), e.Operator()
		pay := func(in core.CreatePaymentInput) error {
			in.Amount = "1"
			_, _, err := e.Service.CreatePayment(sys, in)
			return err
		}
		// Control characters (C0, DEL, C1) are refused in single-line fields.
		for _, bad := range []string{"a\x00b", "a\x1bb", "a\x7fb", "a\u0085b", "a\u009fb", "line\nbreak", "tab\there", "￾"[:1]} {
			wantField(t, pay(core.CreatePaymentInput{OrderID: bad}), "order_id")
			wantField(t, pay(core.CreatePaymentInput{Metadata: map[string]string{"k": bad}}), "metadata.k")
			wantField(t, pay(core.CreatePaymentInput{Metadata: map[string]string{bad: "v"}}), "metadata")
			_, err := e.Service.CreateLink(op, core.LinkInput{Name: bad, Active: true})
			wantField(t, err, "name")
			_, _, err = e.Service.CreateAPIKey(op, core.CreateAPIKeyInput{Name: bad, Scopes: core.Scopes})
			wantField(t, err, "name")
			_, err = e.Service.UpdateMerchant(op, core.UpdateMerchantInput{Merchant: core.MerchantSettings{Name: bad, SupportEmail: "a@b.co"}})
			wantField(t, err, "merchant_name")
		}
		// Descriptions and notes may span lines, nothing more exotic.
		noErr(t, pay(core.CreatePaymentInput{Description: "Line one\nLine two\twith a tab\r\nand a CRLF"}))
		for _, bad := range []string{"bell\x07!", "esc\x1b[31m", "nel\u0085x", "del\x7f!", "cr\rmid"} {
			wantField(t, pay(core.CreatePaymentInput{Description: bad}), "description")
		}
		// Ordinary Unicode is ordinary text.
		noErr(t, pay(core.CreatePaymentInput{
			OrderID: "Заказ №42 — 注文 ☕️", Description: "Café crème · 日本語 · العربية · 👩‍💻",
			Metadata: map[string]string{"ключ": "значение — ok", "emoji": "🎟️"},
		}))
		wantField(t, pay(core.CreatePaymentInput{CustomerEmail: "a\u0085b@example.com"}), "customer_email")

		// Redirects and other browser-facing URLs: absolute http(s) only,
		// no credentials.
		for _, bad := range []string{
			"javascript:alert(1)", "JaVaScRiPt:alert(1)", "data:text/html,<script>alert(1)</script>", "vbscript:x",
			"file:///etc/passwd", "//evil.example/path", "/relative", "evil.example", "ftp://example.com/x",
			"https://shop.example.com@evil.example/", "https://user:pw@shop.example.com/", "https://", "http://exa mple.com",
			"https://example.com/\x00", "mailto:a@b.co",
		} {
			wantField(t, pay(core.CreatePaymentInput{RedirectURL: bad}), "redirect_url")
			_, err := e.Service.CreateLink(op, core.LinkInput{Name: "L", RedirectURL: bad, Active: true})
			wantField(t, err, "redirect_url")
			_, err = e.Service.UpdatePaymentDefaults(op, core.PaymentSettings{TTL: time.Hour, RequiredConfirmations: 1, DefaultRedirectURL: bad})
			wantField(t, err, "default_redirect_url")
			_, err = e.Service.UpdateMerchant(op, core.UpdateMerchantInput{Merchant: core.MerchantSettings{Name: "N", SupportEmail: "a@b.co", SupportURL: bad}})
			wantField(t, err, "support_url")
			_, err = e.Service.UpdateMerchant(op, core.UpdateMerchantInput{Merchant: core.MerchantSettings{Name: "N", SupportEmail: "a@b.co"}, PublicURL: bad})
			wantField(t, err, "public_url")
		}
		for _, good := range []string{"https://shop.example.com/thanks?order=1#top", "http://localhost:3000/done", "HTTPS://Shop.Example.com/"} {
			noErr(t, pay(core.CreatePaymentInput{RedirectURL: good}))
		}

		// Logos: an http(s) URL, or an inline PNG/JPEG/WebP/GIF — never SVG.
		logo := func(u string) error {
			_, err := e.Service.UpdateMerchant(op, core.UpdateMerchantInput{Merchant: core.MerchantSettings{Name: "N", SupportEmail: "a@b.co", LogoURL: u}})
			return err
		}
		for _, good := range []string{
			"", "https://cdn.example.com/logo.svg", "data:image/png;base64,iVBORw0KGgo=", "data:image/jpeg;base64,/9j/4AAQ",
			"data:image/webp;base64,UklGRg==", "data:image/gif;base64,R0lGODlh",
		} {
			noErr(t, logo(good))
		}
		for _, bad := range []string{
			"data:image/svg+xml;base64,PHN2Zz48c2NyaXB0PmFsZXJ0KDEpPC9zY3JpcHQ+PC9zdmc+", "data:image/svg+xml,<svg onload=alert(1)>",
			"data:text/html;base64,PHNjcmlwdD4=", "data:image/png,rawbytes", "data:image/png;base64,", "data:image/png;base64,not base64!",
			"data:image/png;charset=utf-8;base64,iVBORw0KGgo=", "DATA:image/svg+xml;base64,PHN2Zz4=", "javascript:alert(1)",
			"https://u:p@cdn.example.com/logo.png", "data:image/png;base64," + strings.Repeat("A", 513*1024),
		} {
			wantField(t, logo(bad), "logo_url")
		}
	}},

	{"a private key is recognised by its prefix, not by letters inside the key", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithoutSetup())
		public := context.Background()
		// A valid public key may contain "prv" anywhere in its Base58 text.
		for _, key := range []string{
			"xpub6CUGRUonZSQ4TWtTMmzXdrXDprvWKiKrhko4egpiMZbpiaQL2jkwSB1icqYh2cfDfVxdx4df189oLKnC5fSwqPfgyP3hooxujYzAu3fDVmz",
			"xpubPRVprvPrVzXdrXDtypWKiKrhko4egpiMZbpiaQL2jkwSB1icqYh2cfDfVxdx4df189oLKnC5fSwqPfgyP3hooxujYzAu3fDVmzprv",
		} {
			_, err := e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: key})
			noErr(t, err)
		}
		// Extended private keys of every common version are named as such.
		for _, prefix := range []string{"xprv", "yprv", "zprv", "tprv", "uprv", "vprv", "Yprv", "Zprv", "Uprv", "Vprv"} {
			_, err := e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: prefix + "9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi"})
			wantField(t, err, "xpub")
			if f := core.FieldErrors(err); f[0].Code != "private_key" {
				t.Fatalf("%s…: code %q, want private_key", prefix, f[0].Code)
			}
		}
		// Anything else is the address source's call (here: the fake's).
		_, err := e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: "garbage"})
		wantField(t, err, "xpub")
		if f := core.FieldErrors(err); f[0].Code != "invalid" {
			t.Fatalf("garbage: code %q, want invalid", f[0].Code)
		}
	}},

	{"resend-driven attempt history is capped", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		oneDelivery(t, e)
		var id string
		for range 70 {
			job := claimOne(t, e)
			id = job.DeliveryID
			complete(t, e, job, core.AttemptResult{ResponseCode: 500})
			_, err := e.Service.ResendWebhookDelivery(e.Operator(), id)
			noErr(t, err)
		}
		d := delivery(t, e, id)
		if d.Attempt != 70 || len(d.Attempts) != 64 || d.Attempts[0].N != 7 || d.Attempts[63].N != 70 {
			t.Fatalf("history after 70 resends: attempt %d, %d entries, first N %d, last N %d", d.Attempt, len(d.Attempts), d.Attempts[0].N, d.Attempts[len(d.Attempts)-1].N)
		}
	}},
}
