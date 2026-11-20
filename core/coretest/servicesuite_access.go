package coretest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

var accessCases = []serviceCase{
	{"every privileged method needs an actor; scopes are enforced in the core", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		anon := context.Background()
		readOnly, _ := e.APIKey(core.ScopePaymentsRead)
		writer, _ := e.APIKey(core.ScopePaymentsWrite)
		operator, system := e.Operator(), e.System()
		p := e.CreatePayment("1")
		head := core.ChainHead{ChainID: 56, Number: 1, Hash: BlockHash(1, 0), Time: e.Clock.Now(), Final: 1}

		type call struct {
			name string
			fn   func(ctx context.Context) error
			// who may call it
			read, write, op bool
		}
		calls := []call{
			{"CreatePayment", func(ctx context.Context) error {
				_, _, err := e.Service.CreatePayment(ctx, core.CreatePaymentInput{Amount: "1"})
				return err
			}, false, true, true},
			{"CancelPayment", func(ctx context.Context) error {
				q, _, err := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "1"})
				if err != nil {
					return err
				}
				_, err = e.Service.CancelPayment(ctx, q.ID)
				return err
			}, false, true, true},
			{"GetPayment", func(ctx context.Context) error { _, err := e.Service.GetPayment(ctx, p.ID); return err }, true, false, true},
			{"GetPaymentDetail", func(ctx context.Context) error { _, err := e.Service.GetPaymentDetail(ctx, p.ID); return err }, true, false, true},
			{"ListPayments", func(ctx context.Context) error {
				_, err := e.Service.ListPayments(ctx, core.ListPaymentsInput{})
				return err
			}, true, false, true},
			{"CountPayments", func(ctx context.Context) error {
				_, err := e.Service.CountPayments(ctx, core.PaymentFilter{})
				return err
			}, true, false, true},
			{"Overview", func(ctx context.Context) error { _, err := e.Service.Overview(ctx, 7); return err }, true, false, true},
			{"Events", func(ctx context.Context) error { _, err := e.Service.Events(ctx, core.EventFilter{}); return err }, true, false, true},
			{"NodeStatus", func(ctx context.Context) error { _, err := e.Service.NodeStatus(ctx); return err }, true, false, true},
			{"ListLinks", func(ctx context.Context) error { _, err := e.Service.ListLinks(ctx); return err }, false, false, true},
			{"CreateLink", func(ctx context.Context) error {
				_, err := e.Service.CreateLink(ctx, core.LinkInput{Name: "x", Active: true})
				return err
			}, false, false, true},
			{"DeleteLink", func(ctx context.Context) error {
				err := e.Service.DeleteLink(ctx, "plink_missing")
				if errors.Is(err, core.ErrNotFound) {
					return nil
				}
				return err
			}, false, false, true},
			{"ListWebhookEndpoints", func(ctx context.Context) error { _, err := e.Service.ListWebhookEndpoints(ctx); return err }, false, false, true},
			{"ListWebhookDeliveries", func(ctx context.Context) error {
				_, err := e.Service.ListWebhookDeliveries(ctx, core.DeliveryFilter{}, core.Page{})
				return err
			}, false, false, true},
			{"GetSettings", func(ctx context.Context) error { _, err := e.Service.GetSettings(ctx); return err }, false, false, true},
			{"UpdateMerchant", func(ctx context.Context) error {
				_, err := e.Service.UpdateMerchant(ctx, core.UpdateMerchantInput{Merchant: core.MerchantSettings{Name: "N", SupportEmail: "a@b.co"}})
				return err
			}, false, false, true},
			{"ListAPIKeys", func(ctx context.Context) error { _, err := e.Service.ListAPIKeys(ctx); return err }, false, false, true},
			{"CreateAPIKey", func(ctx context.Context) error {
				_, _, err := e.Service.CreateAPIKey(ctx, core.CreateAPIKeyInput{Name: "k", Scopes: core.Scopes})
				return err
			}, false, false, true},
			{"GetOperator", func(ctx context.Context) error { _, err := e.Service.GetOperator(ctx); return err }, false, false, true},
			{"ListSessions", func(ctx context.Context) error { _, err := e.Service.ListSessions(ctx); return err }, false, false, true},
			{"RevokeSession", func(ctx context.Context) error {
				err := e.Service.RevokeSession(ctx, "sess_missing")
				if errors.Is(err, core.ErrNotFound) {
					return nil
				}
				return err
			}, false, false, true},
			{"ResolvePayment", func(ctx context.Context) error {
				_, err := e.Service.ResolvePayment(ctx, p.ID, "")
				if errors.Is(err, core.ErrNothingToResolve) {
					return nil
				}
				return err
			}, false, false, true},
			// System only: not even the operator may inject chain facts or
			// drive the delivery workflow.
			{"ObserveTransfer", func(ctx context.Context) error { return e.Service.ObserveTransfer(ctx, e.Transfer(p, "1", 5)) }, false, false, false},
			{"RetractTransfer", func(ctx context.Context) error {
				return e.Service.RetractTransfer(ctx, core.TransferKey{TxHash: Hash("t"), BlockHash: Hash("b")})
			}, false, false, false},
			{"AdvanceHead", func(ctx context.Context) error { return e.Service.AdvanceHead(ctx, head) }, false, false, false},
			{"ChainState", func(ctx context.Context) error { _, err := e.Service.ChainState(ctx); return err }, false, false, false},
			{"ReportChainStatus", func(ctx context.Context) error { return e.Service.ReportChainStatus(ctx, core.ChainStatus{}) }, false, false, false},
			{"PlanWebhookDeliveries", func(ctx context.Context) error { _, err := e.Service.PlanWebhookDeliveries(ctx); return err }, false, false, false},
			{"ClaimWebhookDeliveries", func(ctx context.Context) error { _, err := e.Service.ClaimWebhookDeliveries(ctx, 1); return err }, false, false, false},
			{"CompleteWebhookDelivery", func(ctx context.Context) error {
				_, err := e.Service.CompleteWebhookDelivery(ctx, "whd_missing", core.AttemptResult{ClaimID: "whc_x", Attempt: 1})
				if errors.Is(err, core.ErrNotFound) {
					return nil
				}
				return err
			}, false, false, false},
			{"ReleaseWebhookDelivery", func(ctx context.Context) error {
				_, err := e.Service.ReleaseWebhookDelivery(ctx, "whd_missing", "whc_x", 0)
				if errors.Is(err, core.ErrNotFound) {
					return nil
				}
				return err
			}, false, false, false},
			{"Cursor", func(ctx context.Context) error { _, err := e.Service.Cursor(ctx, "c"); return err }, false, false, false},
			{"SaveCursor", func(ctx context.Context) error { return e.Service.SaveCursor(ctx, "c", 1) }, false, false, false},
			{"Prune", func(ctx context.Context) error { _, err := e.Service.Prune(ctx); return err }, false, false, false},
		}
		for _, c := range calls {
			if err := c.fn(anon); !errors.Is(err, core.ErrUnauthorized) {
				t.Errorf("%s without an actor: %v, want unauthorized", c.name, err)
			}
			check := func(who string, ctx context.Context, allowed bool) {
				err := c.fn(ctx)
				if allowed && err != nil {
					t.Errorf("%s as %s: %v, want success", c.name, who, err)
				}
				if !allowed && !errors.Is(err, core.ErrForbidden) {
					t.Errorf("%s as %s: %v, want forbidden", c.name, who, err)
				}
			}
			check("read-only key", readOnly, c.read)
			check("write key", writer, c.write)
			check("operator", operator, c.op)
			check("system", system, true)
		}
		// Scoped keys reach exactly their own area.
		links, _ := e.APIKey(core.ScopeLinksWrite)
		if _, err := e.Service.ListLinks(links); err != nil {
			t.Errorf("links key: %v", err)
		}
		hooks, _ := e.APIKey(core.ScopeWebhooksManage)
		if _, err := e.Service.ListWebhookEndpoints(hooks); err != nil {
			t.Errorf("webhooks key: %v", err)
		}
		_, err := e.Service.ListLinks(hooks)
		wantKind(t, err, core.KindForbidden)
	}},

	{"API keys: shown once, hashed at rest, expiry and revocation", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx := e.Operator()
		key, secret, err := e.Service.CreateAPIKey(ctx, core.CreateAPIKeyInput{
			Name: " Production backend ", Scopes: []core.Scope{core.ScopePaymentsRead, core.ScopePaymentsWrite, core.ScopePaymentsRead},
			ExpiresAt: e.Clock.Now().Add(90 * 24 * time.Hour),
		})
		noErr(t, err)
		if !strings.HasPrefix(secret, "hmn_live_") || len(secret) != len("hmn_live_")+40 ||
			!strings.HasPrefix(key.ID, "key_") || key.Name != "Production backend" || key.Mode != "live" ||
			key.Prefix != secret[:13] || key.Last4 != secret[len(secret)-4:] || len(key.Scopes) != 2 ||
			key.SecretHash == "" || strings.Contains(key.SecretHash, secret[9:]) || !key.LastUsedAt.IsZero() {
			t.Fatalf("api key: %+v (secret %s)", key, secret)
		}
		for field, in := range map[string]core.CreateAPIKeyInput{
			"name":       {Scopes: core.Scopes},
			"scopes":     {Name: "x"},
			"expires_at": {Name: "x", Scopes: core.Scopes, ExpiresAt: e.Clock.Now()},
		} {
			_, _, err := e.Service.CreateAPIKey(ctx, in)
			wantField(t, err, field)
		}
		_, _, err = e.Service.CreateAPIKey(ctx, core.CreateAPIKeyInput{Name: "x", Scopes: []core.Scope{"admin"}})
		wantField(t, err, "scopes")

		public := context.Background()
		actor, err := e.Service.AuthenticateAPIKey(public, secret)
		noErr(t, err)
		if actor.Kind != core.ActorAPIKey || actor.ID != key.ID || len(actor.Scopes) != 2 {
			t.Fatalf("actor: %+v", actor)
		}
		list, err := e.Service.ListAPIKeys(ctx)
		noErr(t, err)
		if len(list) != 1 || !list[0].LastUsedAt.Equal(e.Clock.Now()) {
			t.Fatalf("last used not tracked: %+v", list)
		}
		// Refreshed at most once a minute.
		first := e.Clock.Now()
		e.Clock.Advance(30 * time.Second)
		_, err = e.Service.AuthenticateAPIKey(public, secret)
		noErr(t, err)
		list, _ = e.Service.ListAPIKeys(ctx)
		if !list[0].LastUsedAt.Equal(first) {
			t.Fatalf("last used rewritten within a minute: %v", list[0].LastUsedAt)
		}
		e.Clock.Advance(31 * time.Second)
		_, err = e.Service.AuthenticateAPIKey(public, "  "+secret+"\n")
		noErr(t, err)
		list, _ = e.Service.ListAPIKeys(ctx)
		if !list[0].LastUsedAt.Equal(e.Clock.Now()) {
			t.Fatalf("last used not refreshed after a minute: %v", list[0].LastUsedAt)
		}

		for _, bad := range []string{"", "hmn_live_", secret[:len(secret)-1], secret + "x", strings.ToUpper(secret), "Bearer " + secret, key.SecretHash, key.ID} {
			_, err := e.Service.AuthenticateAPIKey(public, bad)
			wantIs(t, err, core.ErrInvalidCredentials)
			wantKind(t, err, core.KindUnauthorized)
		}

		// Expiry.
		e.Clock.Set(key.ExpiresAt.Add(-time.Second))
		_, err = e.Service.AuthenticateAPIKey(public, secret)
		noErr(t, err)
		e.Clock.Set(key.ExpiresAt)
		_, err = e.Service.AuthenticateAPIKey(public, secret)
		wantIs(t, err, core.ErrCredentialsExpired)
		wantIs(t, err, core.ErrUnauthorized)
	}},

	{"API keys: revocation", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx := e.Operator()
		key, secret, err := e.Service.CreateAPIKey(ctx, core.CreateAPIKeyInput{Name: "k", Scopes: core.Scopes})
		noErr(t, err)
		other, otherSecret, err := e.Service.CreateAPIKey(ctx, core.CreateAPIKeyInput{Name: "other", Scopes: core.Scopes})
		noErr(t, err)
		if secret == otherSecret || key.ID == other.ID {
			t.Fatal("keys must be unique")
		}
		e.Clock.Advance(time.Minute)
		revoked, err := e.Service.RevokeAPIKey(ctx, key.ID)
		noErr(t, err)
		if !revoked.RevokedAt.Equal(e.Clock.Now()) {
			t.Fatalf("revoked key: %+v", revoked)
		}
		_, err = e.Service.AuthenticateAPIKey(context.Background(), secret)
		wantIs(t, err, core.ErrCredentialsRevoked)
		_, err = e.Service.AuthenticateAPIKey(context.Background(), otherSecret)
		noErr(t, err)
		e.Clock.Advance(time.Hour)
		again, err := e.Service.RevokeAPIKey(ctx, key.ID)
		noErr(t, err)
		if !again.RevokedAt.Equal(revoked.RevokedAt) {
			t.Fatal("revoking twice must keep the first revocation time")
		}
		_, err = e.Service.RevokeAPIKey(ctx, "key_missing")
		wantIs(t, err, core.ErrNotFound)
		list, _ := e.Service.ListAPIKeys(ctx)
		if len(list) != 2 {
			t.Fatalf("revoked keys stay listed: %+v", list)
		}
	}},

	{"login, sessions, logout, password change", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		public := context.Background()
		for _, in := range []core.LoginInput{
			{Email: e.Email, Password: "wrong password!"},
			{Email: "someone@else.com", Password: e.Password},
			{Email: e.Email},
			{},
			{Email: e.Email, Password: strings.Repeat("x", 5000)},
		} {
			_, err := e.Service.Login(public, in)
			wantIs(t, err, core.ErrInvalidCredentials)
		}
		res, err := e.Service.Login(public, core.LoginInput{Email: "  OWNER@example.com ", Password: e.Password, UserAgent: "Mozilla", IP: "203.0.113.9"})
		noErr(t, err)
		if !strings.HasPrefix(res.Token, "hmns_") || res.Token == e.Token || res.Session.UserAgent != "Mozilla" || res.Session.IP != "203.0.113.9" ||
			!res.Session.ExpiresAt.Equal(e.Clock.Now().Add(core.DefaultSessionTTL)) || res.Session.TokenHash == res.Token || res.Operator.Email != e.Email {
			t.Fatalf("login result: %+v", res)
		}
		actor, err := e.Service.AuthenticateSession(public, res.Token)
		noErr(t, err)
		// The actor says when the session ends if it is not used again: the
		// idle timeout, long before the absolute expiry.
		if actor.Kind != core.ActorOperator || actor.Name != e.Email || actor.SessionID != res.Session.ID ||
			!actor.SessionExpiresAt.Equal(e.Clock.Now().Add(core.DefaultSessionIdleTimeout)) || !res.Session.LastSeenAt.Equal(e.Clock.Now()) {
			t.Fatalf("actor: %+v", actor)
		}
		op, err := e.Service.GetOperator(core.WithActor(public, actor))
		noErr(t, err)
		if op.Email != e.Email || op.PasswordHash != "" {
			t.Fatalf("operator: %+v", op)
		}
		for _, bad := range []string{"", "hmns_", res.Token + "x", res.Session.TokenHash, res.Session.ID} {
			_, err := e.Service.AuthenticateSession(public, bad)
			wantIs(t, err, core.ErrInvalidCredentials)
		}

		// Changing the password keeps this session and ends the others.
		ctx := core.WithActor(public, actor)
		wantField(t, e.Service.ChangePassword(ctx, "not the password", "a brand new passphrase"), "current_password")
		wantField(t, e.Service.ChangePassword(ctx, e.Password, "short"), "new_password")
		noErr(t, e.Service.ChangePassword(ctx, e.Password, "a brand new passphrase"))
		_, err = e.Service.AuthenticateSession(public, e.Token)
		wantIs(t, err, core.ErrInvalidCredentials)
		_, err = e.Service.AuthenticateSession(public, res.Token)
		noErr(t, err)
		_, err = e.Service.Login(public, core.LoginInput{Email: e.Email, Password: e.Password})
		wantIs(t, err, core.ErrInvalidCredentials)
		second, err := e.Service.Login(public, core.LoginInput{Email: e.Email, Password: "a brand new passphrase"})
		noErr(t, err)

		// Logout ends one session only.
		noErr(t, e.Service.Logout(public, res.Token))
		noErr(t, e.Service.Logout(public, res.Token))
		noErr(t, e.Service.Logout(public, "garbage"))
		_, err = e.Service.AuthenticateSession(public, res.Token)
		wantIs(t, err, core.ErrInvalidCredentials)
		_, err = e.Service.AuthenticateSession(public, second.Token)
		noErr(t, err)

		// A session that is not used ends after the idle timeout.
		e.Clock.Advance(core.DefaultSessionIdleTimeout - time.Second)
		_, err = e.Service.AuthenticateSession(public, second.Token)
		noErr(t, err)
		e.Clock.Advance(core.DefaultSessionIdleTimeout)
		_, err = e.Service.AuthenticateSession(public, second.Token)
		wantIs(t, err, core.ErrCredentialsExpired)
		wantIs(t, err, core.ErrUnauthorized)
	}},

	{"session lifetime: sliding idle timeout inside an absolute limit", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		public := context.Background()
		res, err := e.Service.Login(public, core.LoginInput{Email: e.Email, Password: e.Password})
		noErr(t, err)
		absolute := res.Session.ExpiresAt
		if !absolute.Equal(e.Clock.Now().Add(core.DefaultSessionTTL)) {
			t.Fatalf("absolute expiry: %v", absolute)
		}
		// Used every 20 hours it lives on, up to the absolute limit.
		for e.Clock.Now().Add(20 * time.Hour).Before(absolute) {
			e.Clock.Advance(20 * time.Hour)
			actor, err := e.Service.AuthenticateSession(public, res.Token)
			noErr(t, err)
			want := e.Clock.Now().Add(core.DefaultSessionIdleTimeout)
			if want.After(absolute) {
				want = absolute
			}
			if !actor.SessionExpiresAt.Equal(want) {
				t.Fatalf("session deadline %v, want %v", actor.SessionExpiresAt, want)
			}
		}
		e.Clock.Set(absolute.Add(-time.Second))
		_, err = e.Service.AuthenticateSession(public, res.Token)
		noErr(t, err)
		e.Clock.Set(absolute)
		_, err = e.Service.AuthenticateSession(public, res.Token)
		wantIs(t, err, core.ErrCredentialsExpired)

		// Use is recorded at most every five minutes: a busy dashboard does
		// not write on every request.
		res, err = e.Service.Login(public, core.LoginInput{Email: e.Email, Password: e.Password})
		noErr(t, err)
		seen := func() time.Time {
			actor, err := e.Service.AuthenticateSession(public, res.Token)
			noErr(t, err)
			return actor.SessionExpiresAt.Add(-core.DefaultSessionIdleTimeout)
		}
		start := e.Clock.Now()
		e.Clock.Advance(4 * time.Minute)
		if !seen().Equal(start) {
			t.Fatal("last use rewritten within five minutes")
		}
		e.Clock.Advance(time.Minute)
		if !seen().Equal(e.Clock.Now()) {
			t.Fatal("last use not recorded after five minutes")
		}
		// Idle sessions are pruned.
		e.Clock.Advance(core.DefaultSessionIdleTimeout)
		pruned, err := e.Service.Prune(e.System())
		noErr(t, err)
		if pruned.Sessions < 1 {
			t.Fatalf("prune: %+v", pruned)
		}
		_, err = e.Service.AuthenticateSession(public, res.Token)
		wantIs(t, err, core.ErrInvalidCredentials)
	}},

	{"session idle timeout can be disabled", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithConfig(func(c *core.Config) { c.SessionIdleTimeout = -1 }))
		e.Clock.Advance(core.DefaultSessionTTL - time.Second)
		actor, err := e.Service.AuthenticateSession(context.Background(), e.Token)
		noErr(t, err)
		if !actor.SessionExpiresAt.Equal(e.Clock.Now().Add(time.Second)) {
			t.Fatalf("deadline without idle timeout: %v", actor.SessionExpiresAt)
		}
	}},
}

var instanceCases = []serviceCase{
	{"before setup: nothing works except the wizard", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithoutSetup())
		public, system := context.Background(), e.System()
		status, err := e.Service.SetupStatus(public)
		noErr(t, err)
		if status.Completed || status.MinPasswordLength != 12 || status.Defaults.Network.ChainID != 56 || status.Defaults.Asset != core.USDTBSC ||
			status.Defaults.Payments.TTL != 30*time.Minute || status.Defaults.Payments.RequiredConfirmations != 15 ||
			status.Defaults.Wallet.DerivationPath != "m/44'/60'/0'/0" {
			t.Fatalf("setup status: %+v", status)
		}
		_, _, err = e.Service.CreatePayment(system, core.CreatePaymentInput{Amount: "1"})
		wantIs(t, err, core.ErrSetupRequired)
		wantKind(t, err, core.KindSetupRequired)
		_, err = e.Service.Login(public, core.LoginInput{Email: e.Email, Password: e.Password})
		wantIs(t, err, core.ErrSetupRequired)
		_, err = e.Service.CreateLink(system, core.LinkInput{Name: "x"})
		wantIs(t, err, core.ErrSetupRequired)
		// The public checkout surface answers about the object only: nothing
		// exists yet, so everything is "not found" — never setup_required.
		_, err = e.Service.GetLinkCheckout(public, "x")
		wantIs(t, err, core.ErrNotFound)
		_, err = e.Service.GetCheckout(public, "pay_x")
		wantIs(t, err, core.ErrNotFound)
		_, _, err = e.Service.CreatePaymentFromLink(public, "x", core.LinkPaymentInput{})
		wantIs(t, err, core.ErrNotFound)
		_, err = e.Service.UpdateNetwork(system, core.UpdateNetworkInput{RPCURLs: []string{"https://rpc.example.com"}})
		wantIs(t, err, core.ErrSetupRequired)
		state, err := e.Service.ChainState(system)
		noErr(t, err)
		if state.SetupCompleted || len(state.Addresses) != 0 {
			t.Fatalf("chain state before setup: %+v", state)
		}

		// The wizard's helpers are public until setup is done.
		addrs, err := e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: "  " + XPub[:40] + "\n" + XPub[40:] + " "})
		noErr(t, err)
		if len(addrs) != 3 || addrs[0].Index != 0 || addrs[2].Path != "m/44'/60'/0'/0/2" ||
			addrs[1].Address != Address(XPub, core.DefaultDerivationPath, uint32(1)) || addrs[0].Address == addrs[1].Address {
			t.Fatalf("preview: %+v", addrs)
		}
		ledger, err := e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: XPub, DerivationPath: "m/44'/60'/0'", Count: 5})
		noErr(t, err)
		if len(ledger) != 5 || ledger[0].Address == addrs[0].Address {
			t.Fatalf("preview with another path: %+v", ledger)
		}
		_, err = e.Service.PreviewAddresses(public, core.PreviewAddressesInput{})
		wantField(t, err, "xpub")
		_, err = e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: "xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi"})
		wantField(t, err, "xpub")
		_, err = e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: "0x7A3f9C2e41b8D05a6E19c4F2B3d8a7E6c5F41b92"})
		wantField(t, err, "xpub")

		probe, err := e.Service.ProbeRPC(public, core.ProbeRPCInput{URL: "https://bsc.example.org"})
		noErr(t, err)
		if probe.Outcome != core.ProbeOK || probe.Expected != core.USDTBSC || probe.Facts.BlockNumber == 0 {
			t.Fatalf("probe: %+v", probe)
		}
		e.Prober.Facts = map[string]core.ProbeFacts{
			"https://down.example.org": {Error: "connection refused"},
			"https://eth.example.org":  {Reachable: true, ChainID: 1, BlockNumber: 5},
			"https://odd.example.org":  {Reachable: true, ChainID: 56, BlockNumber: 5, TokenFound: true, TokenDecimals: 6},
			"https://none.example.org": {Reachable: true, ChainID: 56, BlockNumber: 5},
		}
		for url, want := range map[string]core.ProbeOutcome{
			"https://down.example.org": core.ProbeUnreachable,
			"https://eth.example.org":  core.ProbeWrongChain,
			"https://odd.example.org":  core.ProbeTokenMismatch,
			"https://none.example.org": core.ProbeTokenMismatch,
		} {
			probe, err := e.Service.ProbeRPC(public, core.ProbeRPCInput{URL: url})
			noErr(t, err)
			if probe.Outcome != want {
				t.Errorf("probe %s: %s, want %s", url, probe.Outcome, want)
			}
		}
		testnet, err := e.Service.ProbeRPC(public, core.ProbeRPCInput{URL: "https://testnet.example.org", ChainID: 97})
		noErr(t, err)
		if testnet.Outcome != core.ProbeOK || testnet.Expected != core.USDTBSCTestnet {
			t.Fatalf("testnet probe: %+v", testnet)
		}
		_, err = e.Service.ProbeRPC(public, core.ProbeRPCInput{URL: "not a url"})
		wantField(t, err, "url")
		_, err = e.Service.ProbeRPC(public, core.ProbeRPCInput{URL: "https://x.example.org", ChainID: 1})
		wantField(t, err, "chain_id")
	}},

	{"complete setup: validation, atomicity, once only", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithoutSetup())
		public := context.Background()
		bad := map[string]func(in *core.SetupInput){
			"email":           func(in *core.SetupInput) { in.Email = "nope" },
			"password":        func(in *core.SetupInput) { in.Password = "too short" },
			"rpc_urls":        func(in *core.SetupInput) { in.RPCURLs = nil },
			"rpc_urls.0":      func(in *core.SetupInput) { in.RPCURLs = []string{"ftp://x"} },
			"xpub":            func(in *core.SetupInput) { in.XPub = "garbage" },
			"derivation_path": func(in *core.SetupInput) { in.DerivationPath = "44/60" },
			"merchant_name":   func(in *core.SetupInput) { in.Merchant.Name = " " },
			"support_email":   func(in *core.SetupInput) { in.Merchant.SupportEmail = "" },
			"chain_id":        func(in *core.SetupInput) { in.ChainID = 1 },
			"public_url":      func(in *core.SetupInput) { in.PublicURL = "pay.example.com" },
		}
		for field, mutate := range bad {
			in := e.SetupInput()
			mutate(&in)
			_, err := e.Service.CompleteSetup(public, in)
			if core.KindOf(err) != core.KindInvalid {
				t.Errorf("%s: %v, want a validation error", field, err)
				continue
			}
			found := false
			for _, f := range core.FieldErrors(err) {
				found = found || f.Field == field
			}
			if !found {
				t.Errorf("%s: no field error in %v", field, err)
			}
		}
		// Nothing was stored by the failed attempts.
		status, _ := e.Service.SetupStatus(public)
		if status.Completed {
			t.Fatal("a failed setup left the instance set up")
		}
		_, err := e.Service.Login(public, core.LoginInput{Email: e.Email, Password: e.Password})
		wantIs(t, err, core.ErrSetupRequired)

		in := e.SetupInput()
		in.ChainID = core.ChainBSCTestnet
		in.RPCURLs = []string{"https://a.example.org", "wss://b.example.org", "https://a.example.org"}
		in.PublicURL = "https://pay.example.com/"
		in.UserAgent = "wizard"
		res, err := e.Service.CompleteSetup(public, in)
		noErr(t, err)
		if res.Token == "" || res.Operator.Email != e.Email || res.Operator.PasswordHash != "" || res.Session.UserAgent != "wizard" || !strings.HasPrefix(res.Operator.ID, "op_") {
			t.Fatalf("setup result: %+v", res)
		}
		actor, err := e.Service.AuthenticateSession(public, res.Token)
		noErr(t, err)
		ctx := core.WithActor(public, actor)
		st, err := e.Service.GetSettings(ctx)
		noErr(t, err)
		if !st.SetupCompleted || st.Network.ChainID != 97 || st.Asset != core.USDTBSCTestnet || len(st.Network.RPCURLs) != 2 ||
			st.Wallet.XPub != XPub || st.Wallet.DerivationPath != core.DefaultDerivationPath || st.Wallet.NextAddressIndex != 0 ||
			st.Merchant.Name != "Northwind Digital" || st.PublicURL != "https://pay.example.com" ||
			st.Payments.TTL != core.DefaultPaymentTTL || st.Payments.UnderpaymentTolerance != "0" {
			t.Fatalf("settings after setup: %+v", st)
		}
		status, _ = e.Service.SetupStatus(public)
		if !status.Completed {
			t.Fatal("setup not marked completed")
		}

		_, err = e.Service.CompleteSetup(public, e.SetupInput())
		wantIs(t, err, core.ErrSetupCompleted)
		wantKind(t, err, core.KindConflict)
		// After setup the wizard's helpers need the operator.
		_, err = e.Service.PreviewAddresses(public, core.PreviewAddressesInput{XPub: XPub})
		wantIs(t, err, core.ErrUnauthorized)
		_, err = e.Service.ProbeRPC(public, core.ProbeRPCInput{URL: "https://x.example.org"})
		wantIs(t, err, core.ErrUnauthorized)
		_, err = e.Service.PreviewAddresses(ctx, core.PreviewAddressesInput{XPub: XPub})
		noErr(t, err)

		// On a test network API keys are marked as test keys.
		key, secret, err := e.Service.CreateAPIKey(ctx, core.CreateAPIKeyInput{Name: "k", Scopes: core.Scopes})
		noErr(t, err)
		if key.Mode != "test" || !strings.HasPrefix(secret, "hmn_test_") {
			t.Fatalf("test-network key: %+v %s", key, secret)
		}
		// No address is handed out before the chain adapter has processed
		// a block of the new instance's network.
		_, _, err = e.Service.CreatePayment(ctx, core.CreatePaymentInput{Amount: "5"})
		wantIs(t, err, core.ErrChainNotReady)
		wantKind(t, err, core.KindUnavailable)
		noErr(t, e.Service.AdvanceHead(e.System(), core.ChainHead{ChainID: core.ChainBSCTestnet, Number: 7, Hash: BlockHash(7, 0), Time: e.Clock.Now(), Final: 7}))
		p, _, err := e.Service.CreatePayment(ctx, core.CreatePaymentInput{Amount: "5"})
		noErr(t, err)
		if p.ChainID != 97 {
			t.Fatalf("payment chain: %d", p.ChainID)
		}
	}},

	{"probe without a prober is unavailable", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithoutSetup(), WithConfig(func(c *core.Config) { c.Prober = nil }))
		_, err := e.Service.ProbeRPC(context.Background(), core.ProbeRPCInput{URL: "https://x.example.org"})
		wantIs(t, err, core.ErrUnavailable)
		wantKind(t, err, core.KindUnavailable)
	}},

	{"settings: defaults apply to new payments only; changes are announced", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx := e.Operator()
		old := e.CreatePayment("10")
		st, err := e.Service.UpdatePaymentDefaults(ctx, core.PaymentSettings{
			TTL: 2 * time.Hour, RequiredConfirmations: 3, UnderpaymentTolerance: "0.25", DefaultRedirectURL: "https://example.com/thanks",
		})
		noErr(t, err)
		if st.Payments.TTL != 2*time.Hour || st.Payments.RequiredConfirmations != 3 || st.Payments.UnderpaymentTolerance != "0.25" {
			t.Fatalf("settings: %+v", st.Payments)
		}
		fresh := e.CreatePayment("10")
		if fresh.RequiredConfirmations != 3 || !fresh.ExpiresAt.Equal(fresh.CreatedAt.Add(2*time.Hour)) || fresh.RedirectURL != "https://example.com/thanks" {
			t.Fatalf("new payment ignores new defaults: %+v", fresh)
		}
		if got := e.Payment(old.ID); got.RequiredConfirmations != 15 || !got.ExpiresAt.Equal(old.ExpiresAt) || got.RedirectURL != "" {
			t.Fatalf("existing payment changed: %+v", got)
		}
		e.Pay(fresh, "9.75", 100)
		e.Pay(old, "9.75", 100)
		e.Head(102)
		wantStatus(t, e, fresh.ID, core.StatusPaid)
		wantStatus(t, e, old.ID, core.StatusDetected)

		for field, in := range map[string]core.PaymentSettings{
			"ttl":                    {TTL: time.Second, RequiredConfirmations: 1},
			"required_confirmations": {TTL: time.Hour},
			"underpayment_tolerance": {TTL: time.Hour, RequiredConfirmations: 1, UnderpaymentTolerance: "-1"},
			"default_redirect_url":   {TTL: time.Hour, RequiredConfirmations: 1, DefaultRedirectURL: "x"},
		} {
			_, err := e.Service.UpdatePaymentDefaults(ctx, in)
			wantField(t, err, field)
		}

		st, err = e.Service.UpdateMerchant(ctx, core.UpdateMerchantInput{
			Merchant:  core.MerchantSettings{Name: "Acme", SupportEmail: "Help@Acme.example", SupportURL: "https://acme.example/help", LogoURL: "data:image/png;base64,AAAA"},
			PublicURL: "https://pay.acme.example/",
		})
		noErr(t, err)
		if st.Merchant.Name != "Acme" || st.Merchant.SupportEmail != "help@acme.example" || st.PublicURL != "https://pay.acme.example" {
			t.Fatalf("merchant: %+v", st)
		}
		if got := e.Payment(old.ID); got.CheckoutURL != "https://pay.acme.example/pay/"+old.ID {
			t.Fatalf("checkout url: %s", got.CheckoutURL)
		}
		_, err = e.Service.UpdateMerchant(ctx, core.UpdateMerchantInput{Merchant: core.MerchantSettings{Name: "Acme", SupportEmail: "x@y.zz", LogoURL: "ftp://logo"}})
		wantField(t, err, "logo_url")

		events, err := e.Service.Events(e.System(), core.EventFilter{Types: []core.EventType{core.EventSettingsUpdated}})
		noErr(t, err)
		var sections []string
		for _, ev := range events {
			sections = append(sections, strings.Trim(string(ev.Data), `{}"`))
		}
		if strings.Join(sections, " ") != `section":"setup section":"payments section":"merchant` {
			t.Fatalf("settings events: %v", sections)
		}
	}},

	{"network: endpoints change freely, the chain only without open payments", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx := e.Operator()
		e.Head(500)
		p := e.CreatePayment("10")
		st, err := e.Service.UpdateNetwork(ctx, core.UpdateNetworkInput{RPCURLs: []string{"https://one.example.org", "https://two.example.org"}})
		noErr(t, err)
		if st.Network.ChainID != 56 || len(st.Network.RPCURLs) != 2 || st.Asset != core.USDTBSC {
			t.Fatalf("network: %+v", st)
		}
		_, err = e.Service.UpdateNetwork(ctx, core.UpdateNetworkInput{ChainID: 97, RPCURLs: []string{"https://testnet.example.org"}})
		wantIs(t, err, core.ErrOpenPayments)
		_, err = e.Service.UpdateNetwork(ctx, core.UpdateNetworkInput{RPCURLs: nil})
		wantField(t, err, "rpc_urls")
		_, err = e.Service.UpdateNetwork(ctx, core.UpdateNetworkInput{ChainID: 1, RPCURLs: []string{"https://eth.example.org"}})
		wantField(t, err, "chain_id")

		e.Pay(p, "10", 501)
		e.Head(515)
		wantStatus(t, e, p.ID, core.StatusPaid)
		st, err = e.Service.UpdateNetwork(ctx, core.UpdateNetworkInput{ChainID: 97, RPCURLs: []string{"https://testnet.example.org"}})
		noErr(t, err)
		if st.Network.ChainID != 97 || st.Asset != core.USDTBSCTestnet {
			t.Fatalf("switched network: %+v", st)
		}
		state, err := e.Service.ChainState(e.System())
		noErr(t, err)
		if state.Head.Number != 0 || len(state.Addresses) != 0 || state.Asset != core.USDTBSCTestnet {
			t.Fatalf("chain state after switching: %+v", state)
		}
		// A mainnet payment can no longer be credited from the new chain …
		late := e.Transfer(p, "10", 600)
		late.ChainID, late.Contract = 97, core.USDTBSCTestnet.Contract
		wantIs(t, e.Service.ObserveTransfer(e.System(), late), core.ErrChainMismatch)
		// … and old heads are refused.
		wantIs(t, e.Service.AdvanceHead(e.System(), core.ChainHead{ChainID: 56, Number: 600, Hash: BlockHash(600, 0), Time: e.Clock.Now(), Final: 600}), core.ErrChainMismatch)
		// A custom asset can be supplied for chains without a preset.
		custom := core.Asset{Symbol: "USDC", Network: "ERC20", ChainID: 1, Contract: "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", Decimals: 6}
		st, err = e.Service.UpdateNetwork(ctx, core.UpdateNetworkInput{ChainID: 1, Asset: &custom, RPCURLs: []string{"https://eth.example.org"}})
		noErr(t, err)
		if st.Asset.Decimals != 6 || st.Asset.Contract != strings.ToLower(custom.Contract) {
			t.Fatalf("custom asset: %+v", st.Asset)
		}
		// The head was reset with the switch: no payment until the chain
		// adapter has processed a block of the new network.
		_, _, err = e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "1"})
		wantIs(t, err, core.ErrChainNotReady)
		noErr(t, e.Service.AdvanceHead(e.System(), core.ChainHead{ChainID: 1, Number: 900, Hash: BlockHash(900, 0), Time: e.Clock.Now(), Final: 900}))
		q := e.CreatePayment("12.345678")
		if q.Amount != "12.345678" || q.Decimals != 6 || q.Asset != "USDC" {
			t.Fatalf("payment in a 6-decimal asset: %+v", q)
		}
		_, _, err = e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "1.0000001"})
		wantField(t, err, "amount")
	}},

	{"wallet update is verified", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv()
		ctx := e.Operator()
		_, err := e.Service.UpdateWallet(ctx, core.UpdateWalletInput{XPub: "ypub-not-supported"})
		wantField(t, err, "xpub")
		big := uint32(1 << 31)
		_, err = e.Service.UpdateWallet(ctx, core.UpdateWalletInput{XPub: XPub, NextAddressIndex: &big})
		wantField(t, err, "next_address_index")
		e.CreatePayment("1")
		st, err := e.Service.UpdateWallet(ctx, core.UpdateWalletInput{XPub: XPub, DerivationPath: core.DefaultDerivationPath})
		noErr(t, err)
		if st.Wallet.NextAddressIndex != 1 {
			t.Fatalf("same wallet must keep its index: %+v", st.Wallet)
		}
		// A broken address source fails payments loudly, not silently.
		e.Addresses.Err = errors.New("hsm offline")
		_, _, err = e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: "1"})
		if err == nil || core.KindOf(err) != core.KindInternal {
			t.Fatalf("create with a broken address source: %v", err)
		}
		e.Addresses.Err = nil
		p := e.CreatePayment("1")
		if p.Address != Address(XPub, core.DefaultDerivationPath, uint32(1)) {
			t.Fatalf("a failed create burned an index: %s", p.Address)
		}
	}},

	{"node status", func(t *testing.T, newEnv func(...EnvOption) *Env) {
		e := newEnv(WithoutChainHead())
		sys := e.System()
		ns, err := e.Service.NodeStatus(sys)
		noErr(t, err)
		if ns.Healthy || !ns.LastCheckedAt.IsZero() || ns.Head.Number != 0 {
			t.Fatalf("status before any report: %+v", ns)
		}
		e.Head(1000)
		noErr(t, e.Service.ReportChainStatus(sys, core.ChainStatus{
			LatestBlock: 1002,
			Endpoints: []core.RPCEndpointStatus{
				{URL: "https://a", Healthy: true, InUse: true, LatencyMs: 184, BlockNumber: 1002},
				{URL: "https://b", Healthy: false, Error: "timeout"},
			},
		}))
		ns, err = e.Service.NodeStatus(sys)
		noErr(t, err)
		if !ns.Healthy || ns.Head.Number != 1000 || ns.LatestBlock != 1002 || ns.LagBlocks != 2 || ns.RPCLatencyMs != 184 ||
			!ns.LastCheckedAt.Equal(e.Clock.Now()) || len(ns.Endpoints) != 2 {
			t.Fatalf("status: %+v", ns)
		}
		noErr(t, e.Service.ReportChainStatus(sys, core.ChainStatus{LatestBlock: 1100, Endpoints: []core.RPCEndpointStatus{{URL: "https://a", Healthy: true, InUse: true}}}))
		if ns, _ = e.Service.NodeStatus(sys); ns.Healthy || ns.LagBlocks != 100 {
			t.Fatalf("lagging watcher must be unhealthy: %+v", ns)
		}
		// An idle adapter (nothing to watch: it looks at the chain every
		// half minute) is as far behind as its pause is long. That is not
		// lag, and the status says what it is.
		idle := core.ChainStatus{Idle: true, LatestBlock: 1100, Endpoints: []core.RPCEndpointStatus{{URL: "https://a", Healthy: true, InUse: true}}}
		noErr(t, e.Service.ReportChainStatus(sys, idle))
		if ns, _ = e.Service.NodeStatus(sys); !ns.Healthy || !ns.Idle || ns.LagBlocks != 100 || ns.Head.Number != 1000 || ns.LatestBlock != 1100 {
			t.Fatalf("an idle watcher is not a lagging one: %+v", ns)
		}
		if ns.Quiet {
			t.Fatalf("idle with an empty watch list is not quiet: %+v", ns)
		}
		// Quiet is the kind of idle in which closed payments are still
		// scanned for: the same pause, the same health. It is never
		// reported without idle.
		quiet := idle
		quiet.Quiet = true
		noErr(t, e.Service.ReportChainStatus(sys, quiet))
		if ns, _ = e.Service.NodeStatus(sys); !ns.Healthy || !ns.Idle || !ns.Quiet || ns.LagBlocks != 100 {
			t.Fatalf("a quiet watcher is an idle one, and not a lagging one: %+v", ns)
		}
		notIdle := quiet
		notIdle.Idle = false
		noErr(t, e.Service.ReportChainStatus(sys, notIdle))
		if ns, _ = e.Service.NodeStatus(sys); ns.Healthy || ns.Idle || ns.Quiet {
			t.Fatalf("quiet without idle excuses nothing and is not passed on: %+v", ns)
		}
		// Idle excuses the distance to the tip and nothing else: not an
		// endpoint in use that is unhealthy, not a watcher that is starting,
		// not a report that is stale.
		sick := idle
		sick.Endpoints = []core.RPCEndpointStatus{{URL: "https://a", Healthy: false, InUse: true, Error: "timeout"}}
		noErr(t, e.Service.ReportChainStatus(sys, sick))
		if ns, _ = e.Service.NodeStatus(sys); ns.Healthy || !ns.Idle {
			t.Fatalf("idle with a failing endpoint in use must be unhealthy: %+v", ns)
		}
		starting := idle
		starting.Starting = true
		noErr(t, e.Service.ReportChainStatus(sys, starting))
		if ns, _ = e.Service.NodeStatus(sys); ns.Healthy || ns.Idle || !ns.Starting {
			t.Fatalf("a starting watcher is neither healthy nor idle: %+v", ns)
		}
		noErr(t, e.Service.ReportChainStatus(sys, quiet))
		e.Clock.Advance(2 * time.Minute)
		if ns, _ = e.Service.NodeStatus(sys); ns.Healthy || ns.Idle || ns.Quiet {
			t.Fatalf("a stale report of an idle or quiet watcher says nothing about now: %+v", ns)
		}

		noErr(t, e.Service.ReportChainStatus(sys, core.ChainStatus{LatestBlock: 1001, Endpoints: []core.RPCEndpointStatus{{URL: "https://a", Healthy: true, InUse: true}}}))
		e.Clock.Advance(2 * time.Minute)
		if ns, _ = e.Service.NodeStatus(sys); ns.Healthy {
			t.Fatalf("a stale report must be unhealthy: %+v", ns)
		}
	}},
}
