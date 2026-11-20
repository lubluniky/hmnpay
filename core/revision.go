package core

import (
	"context"
	"slices"
)

// The chain configuration's revision, and the fence it is.
//
// Everything the chain adapter reports — heads (with Checked, and the
// retirements and expiries they decide), transfers, retractions, status
// reports — is a fact about the chain as one generation of the adapter saw
// it: a generation built from one chain configuration (network, endpoints,
// token, finality depth). Once that configuration has been replaced, the
// generation's facts are about another configuration, however much the new
// one looks like the old: a head of another token is not a head of this one,
// and a configuration switched away and back has had blocks nobody scanned
// for it in between. So the rule, for every change of the settings a
// generation is built from and across restarts:
//
//	a fact reaches the books only if the configuration it was made under is
//	the current one — by identity (the revision), checked in the transaction
//	that applies it.
//
// [NetworkSettings.Revision] is that identity. The core increments it in
// the transaction that changes any part of the chain configuration
// (chainConfigChanged), never otherwise, and never back: a configuration
// that is switched away and back has a new revision. The adapter reads it
// from [ChainState.Revision], builds its generation for it, and passes it
// with every fact ([WithChainRevision]); [Service.AdvanceHead],
// [Service.ObserveTransfer] and [Service.RetractTransfer] refuse a fact of
// another revision with [ErrConfigurationReplaced] inside their transaction,
// and [Service.ReportChainStatus]'s report counts only while its revision is
// the current one. After a restart there is no generation of the old process
// left to report anything; what it left behind — the processed head, the
// adapter's checkpoint — is checked against the configuration it is read for
// (the head by network and token, which a change of token resets; the
// checkpoint by the adapter, by chain and token).

type chainRevisionKey struct{}

// WithChainRevision returns ctx carrying the revision of the chain
// configuration ([ChainState.Revision]) under which the chain adapter made
// the facts it reports with it. The chain adapter passes it with every fact;
// a fact without one is not fenced (tests, tools).
func WithChainRevision(ctx context.Context, revision uint64) context.Context {
	return context.WithValue(ctx, chainRevisionKey{}, revision)
}

func chainRevision(ctx context.Context) (uint64, bool) {
	r, ok := ctx.Value(chainRevisionKey{}).(uint64)
	return r, ok
}

// fence returns [ErrConfigurationReplaced] if ctx carries the revision of a
// chain configuration other than st's.
func fence(ctx context.Context, st Settings) error {
	if r, ok := chainRevision(ctx); ok && r != st.Network.Revision {
		return newError(ErrConfigurationReplaced, "the fact was made under chain configuration revision %d; the current one is %d", r, st.Network.Revision)
	}
	return nil
}

// chainConfigChanged reports whether anything a chain adapter generation is
// built from differs between a and b.
func chainConfigChanged(a, b Settings) bool {
	return a.SetupCompleted != b.SetupCompleted ||
		a.Network.ChainID != b.Network.ChainID ||
		!slices.Equal(a.Network.RPCURLs, b.Network.RPCURLs) ||
		a.Asset != b.Asset ||
		a.Payments.RequiredConfirmations != b.Payments.RequiredConfirmations
}
