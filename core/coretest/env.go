package coretest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// Env is a complete hmnpay core wired to fakes: a [core.Service] on a
// store, with a controllable clock, deterministic ids and a scripted
// chain. Adapter tests (HTTP handlers, the webhook dispatcher, a bot) use
// it to exercise real business rules without real infrastructure.
type Env struct {
	T         testing.TB
	Service   *core.Service
	Store     core.Store
	Clock     *Clock
	Addresses *Addresses
	Prober    *Prober

	// Token is the operator's session token after setup ("" if setup was
	// skipped).
	Token string
	// Email and Password are the operator's credentials used by setup.
	Email    string
	Password string

	txSeq atomic.Uint64
}

// EnvOption customises [NewEnv].
type EnvOption func(*envConfig)

type envConfig struct {
	store     core.Store
	skipSetup bool
	noHead    bool
	configure []func(*core.Config)
	setup     []func(*core.SetupInput)
}

// WithStore runs the Env on the given store instead of a new [MemStore].
func WithStore(s core.Store) EnvOption { return func(c *envConfig) { c.store = s } }

// WithoutSetup leaves the instance in its first-run state.
func WithoutSetup() EnvOption { return func(c *envConfig) { c.skipSetup = true } }

// WithoutChainHead leaves a set-up instance without a processed chain head,
// as it is before its chain adapter has processed the first block: no
// payment can be created yet ([core.ErrChainNotReady]). By default [NewEnv]
// reports [InitialHead] right after setup, as a chain adapter would.
func WithoutChainHead() EnvOption { return func(c *envConfig) { c.noHead = true } }

// InitialHead is the block number of the processed head [NewEnv] reports
// after setup (unless [WithoutChainHead] is given), stamped with the clock's
// time. Tests move the head on with [Env.Head], to any block number.
const InitialHead = 1

// WithConfig adjusts the [core.Config] before the service is built.
func WithConfig(f func(*core.Config)) EnvOption {
	return func(c *envConfig) { c.configure = append(c.configure, f) }
}

// WithSetup adjusts the [core.SetupInput] used to complete setup.
func WithSetup(f func(*core.SetupInput)) EnvOption {
	return func(c *envConfig) { c.setup = append(c.setup, f) }
}

// NewEnv builds a service on fakes and, unless [WithoutSetup] is given,
// completes first-run setup: BSC mainnet USDT, one RPC URL, [XPub], and an
// operator with Email/Password.
//
// The service runs with production defaults, with one exception: the check
// that refuses chain heads timestamped ahead of the wall clock
// (core.Config.MaxHeadTimeAhead) is disabled, because tests set chain time
// by hand. Use WithConfig to turn it on.
func NewEnv(t testing.TB, opts ...EnvOption) *Env {
	t.Helper()
	var ec envConfig
	for _, o := range opts {
		o(&ec)
	}
	if ec.store == nil {
		ec.store = NewMemStore()
	}
	e := &Env{
		T:         t,
		Store:     ec.store,
		Clock:     NewClock(),
		Addresses: &Addresses{},
		Prober:    &Prober{},
		Email:     "owner@example.com",
		Password:  "correct horse battery staple",
	}
	cfg := core.Config{
		Store:     e.Store,
		Addresses: e.Addresses,
		Hasher:    Hasher{},
		Prober:    e.Prober,
		Clock:     e.Clock,
		Rand:      NewRand(1),
		// Tests script chain time freely against a clock that only moves
		// when told to, so the plausibility check on head timestamps
		// (core.Config.MaxHeadTimeAhead, on by default in production) is
		// off here; a test that wants it sets the field with WithConfig.
		MaxHeadTimeAhead: -1,
	}
	for _, f := range ec.configure {
		f(&cfg)
	}
	svc, err := core.New(cfg)
	if err != nil {
		t.Fatalf("coretest: core.New: %v", err)
	}
	e.Service = svc
	if !ec.skipSetup {
		in := e.SetupInput()
		for _, f := range ec.setup {
			f(&in)
		}
		res, err := svc.CompleteSetup(context.Background(), in)
		if err != nil {
			t.Fatalf("coretest: CompleteSetup: %v", err)
		}
		e.Token = res.Token
		if !ec.noHead {
			chain := in.ChainID
			if chain == 0 {
				chain = core.ChainBSCMainnet
			}
			head := core.ChainHead{ChainID: chain, Number: InitialHead, Hash: BlockHash(InitialHead, 0), Time: e.Clock.Now(), Final: InitialHead}
			if err := svc.AdvanceHead(e.System(), head); err != nil {
				t.Fatalf("coretest: AdvanceHead(%d): %v", InitialHead, err)
			}
		}
	}
	return e
}

// SetupInput returns a valid setup request for this Env.
func (e *Env) SetupInput() core.SetupInput {
	return core.SetupInput{
		Email:     e.Email,
		Password:  e.Password,
		ChainID:   core.ChainBSCMainnet,
		RPCURLs:   []string{"https://bsc-dataseed.example.org"},
		XPub:      XPub,
		Merchant:  core.MerchantSettings{Name: "Northwind Digital", SupportEmail: "billing@example.com"},
		PublicURL: "https://pay.example.com",
	}
}

// System returns a context acting as trusted in-process code.
func (e *Env) System() context.Context {
	return core.WithActor(context.Background(), core.SystemActor("test"))
}

// Operator returns a context acting as the signed-in operator.
func (e *Env) Operator() context.Context {
	e.T.Helper()
	actor, err := e.Service.AuthenticateSession(context.Background(), e.Token)
	if err != nil {
		e.T.Fatalf("coretest: AuthenticateSession: %v", err)
	}
	return core.WithActor(context.Background(), actor)
}

// Relogin logs the operator in again — after the clock was moved past the
// session's lifetime, say — makes the new session the Env's, and returns a
// context acting as the operator.
func (e *Env) Relogin() context.Context {
	e.T.Helper()
	res, err := e.Service.Login(context.Background(), core.LoginInput{Email: e.Email, Password: e.Password})
	if err != nil {
		e.T.Fatalf("coretest: Login: %v", err)
	}
	e.Token = res.Token
	return e.Operator()
}

// APIKey creates an API key with the given scopes and returns a context
// authenticated with it, plus the secret.
func (e *Env) APIKey(scopes ...core.Scope) (context.Context, string) {
	e.T.Helper()
	_, secret, err := e.Service.CreateAPIKey(e.Operator(), core.CreateAPIKeyInput{Name: "test key", Scopes: scopes})
	if err != nil {
		e.T.Fatalf("coretest: CreateAPIKey: %v", err)
	}
	actor, err := e.Service.AuthenticateAPIKey(context.Background(), secret)
	if err != nil {
		e.T.Fatalf("coretest: AuthenticateAPIKey: %v", err)
	}
	return core.WithActor(context.Background(), actor), secret
}

// CreatePayment creates a payment for amount (a decimal string) as system.
func (e *Env) CreatePayment(amount string) core.PaymentView {
	e.T.Helper()
	p, _, err := e.Service.CreatePayment(e.System(), core.CreatePaymentInput{Amount: amount})
	if err != nil {
		e.T.Fatalf("coretest: CreatePayment(%s): %v", amount, err)
	}
	return p
}

// Payment re-reads a payment.
func (e *Env) Payment(id string) core.PaymentView {
	e.T.Helper()
	p, err := e.Service.GetPayment(e.System(), id)
	if err != nil {
		e.T.Fatalf("coretest: GetPayment(%s): %v", id, err)
	}
	return p
}

// BlockHash returns the canonical fake hash of block n; fork selects an
// alternative chain (0 is the main one).
func BlockHash(n uint64, fork int) string { return Hash("block", n, fork) }

// Transfer builds — without reporting it — the observation of a transfer
// of amount (a decimal string) to payment p in block n of the main chain,
// with a fresh transaction hash.
func (e *Env) Transfer(p core.PaymentView, amount string, block uint64) core.TransferObservation {
	e.T.Helper()
	a, err := core.ParseAmount(amount, p.Decimals)
	if err != nil {
		e.T.Fatalf("coretest: bad amount %q: %v", amount, err)
	}
	seq := e.txSeq.Add(1)
	asset, _ := core.DefaultAsset(p.ChainID)
	return core.TransferObservation{
		ChainID:     p.ChainID,
		Contract:    asset.Contract,
		TxHash:      Hash("tx", seq),
		LogIndex:    uint(seq % 200),
		BlockNumber: block,
		BlockHash:   BlockHash(block, 0),
		BlockTime:   e.Clock.Now(),
		From:        Address("payer", seq),
		To:          p.Address,
		Amount:      a,
	}
}

// Observe reports a transfer and fails the test on error.
func (e *Env) Observe(obs core.TransferObservation) {
	e.T.Helper()
	if err := e.Service.ObserveTransfer(e.System(), obs); err != nil {
		e.T.Fatalf("coretest: ObserveTransfer: %v", err)
	}
}

// Pay reports a fresh transfer of amount to p in block n and returns it.
func (e *Env) Pay(p core.PaymentView, amount string, block uint64) core.TransferObservation {
	e.T.Helper()
	obs := e.Transfer(p, amount, block)
	e.Observe(obs)
	return obs
}

// Retract reports that a transfer left the canonical chain.
func (e *Env) Retract(obs core.TransferObservation) {
	e.T.Helper()
	key := core.TransferKey{TxHash: obs.TxHash, LogIndex: obs.LogIndex, BlockHash: obs.BlockHash}
	if err := e.Service.RetractTransfer(e.System(), key); err != nil {
		e.T.Fatalf("coretest: RetractTransfer: %v", err)
	}
}

// Head advances the processed chain head to block n of the main chain,
// final at n (as on BSC, where the chain has finalised a block long before
// the processed head reaches it: see HeadFinal for one that is not). The
// block's timestamp is the clock's current time, so tests move chain time
// with [Clock.Advance].
func (e *Env) Head(n uint64) {
	e.T.Helper()
	head := core.ChainHead{ChainID: core.ChainBSCMainnet, Number: n, Hash: BlockHash(n, 0), Time: e.Clock.Now(), Final: n}
	if err := e.Service.AdvanceHead(e.System(), head); err != nil {
		e.T.Fatalf("coretest: AdvanceHead(%d): %v", n, err)
	}
}

// HeadFinal is Head with the head's final block at final (ChainHead.Final).
func (e *Env) HeadFinal(n, final uint64) {
	e.T.Helper()
	head := core.ChainHead{ChainID: core.ChainBSCMainnet, Number: n, Hash: BlockHash(n, 0), Time: e.Clock.Now(), Final: final}
	if err := e.Service.AdvanceHead(e.System(), head); err != nil {
		e.T.Fatalf("coretest: AdvanceHead(%d, final %d): %v", n, final, err)
	}
}

// EventTypes returns the types of all events of a payment, oldest first.
func (e *Env) EventTypes(paymentID string) []core.EventType {
	e.T.Helper()
	events, err := e.Service.Events(e.System(), core.EventFilter{PaymentID: paymentID, Limit: 1000})
	if err != nil {
		e.T.Fatalf("coretest: Events: %v", err)
	}
	types := make([]core.EventType, len(events))
	for i, ev := range events {
		types[i] = ev.Type
	}
	return types
}

// Expire moves the clock past a payment's deadline (TTL plus grace). It
// never moves the clock backwards.
func (e *Env) Expire(p core.PaymentView) {
	if t := p.ExpiresAt.Add(core.DefaultExpiryGrace + time.Second); t.After(e.Clock.Now()) {
		e.Clock.Set(t)
	}
}
