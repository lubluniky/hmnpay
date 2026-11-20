// Package coretest is the test kit of the hmnpay core.
//
// It provides an in-memory implementation of every port — [MemStore],
// [Clock], [Rand], [Addresses], [Hasher], [Prober] — and [Env], which
// wires them into a ready-to-use [core.Service] for adapter tests.
//
// It also provides conformance suites that adapter authors run against
// their implementations of the ports:
//
//   - [RunStoreSuite] checks a [core.Store] against the full store
//     contract: atomicity, isolation, uniqueness, ordering, and every
//     filter, sort and paging combination.
//   - [RunServiceSuite] drives the whole service — payment lifecycle,
//     links, webhooks, authentication — on top of a store, proving the
//     business rules hold on that store.
//   - [RunAddressSourceSuite] and [RunPasswordHasherSuite] check the
//     smaller ports.
package coretest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// Epoch is the time a new [Clock] starts at.
var Epoch = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

// Clock is a manually advanced [core.Clock]. It is safe for concurrent use.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock set to [Epoch].
func NewClock() *Clock { return &Clock{now: Epoch} }

// Now implements [core.Clock].
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d and returns the new time.
func (c *Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Rand is a deterministic source of "randomness" for [core.Config.Rand]:
// a SHA-256 counter stream. The same seed yields the same ids and secrets,
// which keeps tests reproducible. Never use it outside tests.
type Rand struct {
	mu      sync.Mutex
	seed    uint64
	counter uint64
	buf     []byte
}

// NewRand returns a deterministic reader for the given seed.
func NewRand(seed uint64) *Rand { return &Rand{seed: seed} }

// Read implements io.Reader; it never fails.
func (r *Rand) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for n := 0; n < len(p); {
		if len(r.buf) == 0 {
			var in [16]byte
			binary.BigEndian.PutUint64(in[:8], r.seed)
			binary.BigEndian.PutUint64(in[8:], r.counter)
			r.counter++
			sum := sha256.Sum256(in[:])
			r.buf = sum[:]
		}
		c := copy(p[n:], r.buf)
		r.buf = r.buf[c:]
		n += c
	}
	return len(p), nil
}

// XPub is a syntactically plausible extended public key accepted by
// [Addresses]. It is not a real key.
const XPub = "xpub6CUGRUonZSQ4TWtTMmzXdrXDtypWKiKrhko4egpiMZbpiaQL2jkwSB1icqYh2cfDfVxdx4df189oLKnC5fSwqPfgyP3hooxujYzAu3fDVmz"

// Addresses is a fake [core.AddressSource]: it maps (xpub, path, index) to
// a deterministic pseudo-address by hashing. It accepts any key that starts
// with "xpub" and rejects everything else, like the real one rejects
// private or malformed keys.
type Addresses struct {
	// Err, when set, is returned by every call.
	Err error
}

// DeriveAddress implements [core.AddressSource].
func (a *Addresses) DeriveAddress(_ context.Context, w core.WalletSettings, index uint32) (string, error) {
	if a.Err != nil {
		return "", a.Err
	}
	if !strings.HasPrefix(w.XPub, "xpub") {
		return "", errors.New("not an extended public key")
	}
	return Address(w.XPub, w.DerivationPath, index), nil
}

// Hasher is a fast fake [core.PasswordHasher] for tests that do not care
// about KDF cost. It is not secure.
type Hasher struct{}

// Hash implements [core.PasswordHasher].
func (Hasher) Hash(password string) (string, error) {
	return "fake$" + Hash("password", password), nil
}

// Verify implements [core.PasswordHasher].
func (Hasher) Verify(hash, password string) (bool, error) {
	if !strings.HasPrefix(hash, "fake$") {
		return false, errors.New("coretest: not a fake hash")
	}
	return hash == "fake$"+Hash("password", password), nil
}

// Prober is a fake [core.ChainProber]. By default every URL is a healthy
// endpoint of the expected chain with the expected token; override per URL
// with Facts.
type Prober struct {
	// Facts maps a URL to what probing it reports.
	Facts map[string]core.ProbeFacts
	// Err, when set, is returned by every call.
	Err error
}

// Probe implements [core.ChainProber].
func (p *Prober) Probe(_ context.Context, rpcURL string, asset core.Asset) (core.ProbeFacts, error) {
	if p.Err != nil {
		return core.ProbeFacts{}, p.Err
	}
	if f, ok := p.Facts[rpcURL]; ok {
		return f, nil
	}
	return core.ProbeFacts{
		Reachable: true, ChainID: asset.ChainID, BlockNumber: 46_218_904, LatencyMs: 42,
		TokenFound: true, TokenDecimals: asset.Decimals, TokenSymbol: asset.Symbol,
	}, nil
}

// Hash returns a deterministic 0x-prefixed 32-byte hex string derived from
// the parts: a stand-in for transaction and block hashes.
func Hash(parts ...any) string {
	sum := sha256.Sum256([]byte(fmt.Sprintln(parts...)))
	return "0x" + hex.EncodeToString(sum[:])
}

// Address returns a deterministic 0x-prefixed 20-byte hex string derived
// from the parts: a stand-in for EVM addresses.
func Address(parts ...any) string {
	sum := sha256.Sum256([]byte(fmt.Sprintln(parts...)))
	return "0x" + hex.EncodeToString(sum[:20])
}
