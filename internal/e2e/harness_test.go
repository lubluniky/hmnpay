// Package e2e is hmnpay's end-to-end safety net: the real engine (package
// hmnpay) on a file-backed SQLite database, the real HTTP API on an
// httptest server, the real webhook dispatcher delivering to an httptest
// receiver, and the real chain watcher reading two scripted JSON-RPC nodes
// (internal/chain/evm/evmtest). The tests drive the instance only through
// HTTP, as a merchant integration, the dashboard and a checkout page would;
// the only thing not real is the blockchain.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay"
	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/internal/chain/ethaddr"
	"github.com/lubluniky/hmnpay/internal/chain/evm"
	"github.com/lubluniky/hmnpay/internal/chain/evm/evmtest"
)

const (
	// Account xpub (m/44'/60'/0') of the Hardhat development mnemonic
	// "test test … junk": a public test key, watch-only here.
	hardhatXpub = "xpub6Ce9NcJvTk36xtLSrJLZqE7wtgA5deCeYs7rSQtreh4cj6ByPtrg9sD7V2FNFLPnf8heNP3FGkeV9qwfzvZNSd54JoNXVsXFYSYwHsnJxqP"
	// Its first two addresses, as every Ethereum developer knows them.
	hardhatAddr0 = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	hardhatAddr1 = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"

	operatorEmail    = "owner@example.com"
	operatorPassword = "correct horse battery staple"

	settleDepth   = 2
	confirmations = 3
	chainStart    = 80_000

	// waitFor bounds every "eventually" in this package; generous because
	// the suite runs under -race.
	waitFor = 30 * time.Second
)

var (
	usdt  = ethaddr.MustParse("0x55d398326f99059fF775485246999027B3197955")
	payer = ethaddr.MustParse("0x8894E0a0c962CB723c1976a4421c95949bE2D4E3")
)

// ---- clock ----

// testClock is the core's clock: real time plus an offset the test can
// advance (webhook retry schedules, payment deadlines). Chain timestamps
// follow it.
type testClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off += d
}

// ---- logs ----

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ---- webhook receiver ----

// delivery is one webhook request as the merchant's server saw it.
type delivery struct {
	EventID    string
	DeliveryID string
	Signature  string
	Body       []byte
	Envelope   core.EventEnvelope
	At         time.Time
}

// receiver is the merchant's webhook endpoint.
type receiver struct {
	srv *httptest.Server

	mu     sync.Mutex
	got    []delivery
	status int // what to answer; 0 means 200
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		d := delivery{
			EventID: req.Header.Get(core.HeaderEventID), DeliveryID: req.Header.Get(core.HeaderDeliveryID),
			Signature: req.Header.Get(core.HeaderSignature), Body: body, At: time.Now(),
		}
		_ = json.Unmarshal(body, &d.Envelope)
		r.mu.Lock()
		r.got = append(r.got, d)
		status := r.status
		r.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) url() string { return r.srv.URL + "/hooks/hmnpay" }

func (r *receiver) setStatus(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = code
}

func (r *receiver) all() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

// forPayment returns the deliveries of a payment's events, in arrival order.
func (r *receiver) forPayment(paymentID string) []delivery {
	var out []delivery
	for _, d := range r.all() {
		var data struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(d.Envelope.Data, &data)
		if data.ID == paymentID {
			out = append(out, d)
		}
	}
	return out
}

// uniqueTypes returns the event types of distinct event ids, in order of
// first arrival (receivers deduplicate on the event id: delivery is
// at-least-once).
func uniqueTypes(ds []delivery) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range ds {
		if !seen[d.EventID] {
			seen[d.EventID] = true
			out = append(out, string(d.Envelope.Type))
		}
	}
	return out
}

// ---- the instance ----

type instanceOptions struct {
	limits hmnpay.HTTPLimits
}

// instance is one hmnpay deployment plus the chain it watches and the
// merchant's webhook receiver.
type instance struct {
	t     *testing.T
	dir   string
	clock *testClock
	logs  *syncBuffer
	opts  instanceOptions

	chain *evmtest.Chain
	nodes []*evmtest.Node
	urls  []string

	recv *receiver

	eng     *hmnpay.Engine
	handler *hmnpay.HTTPHandler
	srv     *httptest.Server
	stopRun context.CancelFunc
	runDone chan error

	http *http.Client // carries the dashboard session cookie
}

// newInstance builds the chain (two well-behaved RPC nodes), the receiver
// and a fresh, not yet set up hmnpay instance, and starts it.
func newInstance(t *testing.T, opts ...func(*instanceOptions)) *instance {
	t.Helper()
	in := &instance{t: t, dir: t.TempDir(), clock: &testClock{}, logs: &syncBuffer{}}
	for _, o := range opts {
		o(&in.opts)
	}
	in.chain = evmtest.NewChain(core.ChainBSCMainnet, chainStart-100)
	in.chain.AddToken(usdt, "USDT", 18)
	in.chain.SetTime(in.clock.Now())
	in.chain.Mine(100)
	for range 2 {
		n := evmtest.NewNode(in.chain)
		// The chain finalises a block as deep as the processed head and the
		// confirmations together: a transfer is final in the block it would
		// be by its confirmations alone.
		n.Set(func(b *evmtest.Behavior) { b.FinalityDepth = settleDepth + confirmations - 1 })
		in.nodes = append(in.nodes, n)
		in.urls = append(in.urls, n.Serve(t))
	}
	in.recv = newReceiver(t)
	// One browser for the whole test: its cookies (session, known-device
	// marker) survive restarts of the instance, as a real browser's would.
	jar, _ := cookiejar.New(nil)
	in.http = &http.Client{Jar: jar, Timeout: waitFor}
	in.start()
	t.Cleanup(func() {
		in.stop()
		// Nothing in these scenarios is an operational error; an ERROR line
		// is a bug somewhere (or a scenario that must say so explicitly).
		for _, l := range strings.Split(in.logs.String(), "\n") {
			if strings.Contains(l, "level=ERROR") {
				t.Errorf("error logged: %s", l)
			}
		}
		if t.Failed() {
			t.Logf("instance log (tail):\n%s", tail(withoutDebug(in.logs.String()), 4000))
		}
	})
	return in
}

// start opens the engine on the instance's data directory, runs its
// workers and serves its API: a process start.
func (in *instance) start() {
	t := in.t
	t.Helper()
	if in.eng != nil {
		t.Fatal("instance already running")
	}
	loopback := netip.MustParsePrefix("127.0.0.1/32")
	eng, err := hmnpay.Open(t.Context(), hmnpay.Config{
		DataDir: in.dir,
		Logger:  slog.New(slog.NewTextHandler(in.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		// The receiver is on loopback: allow exactly that, keeping the
		// egress policy on for everything else.
		Webhooks: hmnpay.WebhookConfig{AllowedPrefixes: []netip.Prefix{loopback}, PollInterval: 20 * time.Millisecond},
		Chain: hmnpay.ChainConfig{
			PollInterval: 5 * time.Millisecond, SettleDepth: settleDepth,
			StatusInterval: 20 * time.Millisecond, ReloadInterval: 50 * time.Millisecond,
			MaxBackoff: 20 * time.Millisecond, RequestTimeout: 5 * time.Second,
		},
		ShutdownTimeout: 15 * time.Second,
		Clock:           in.clock,
		// The real hasher with a test work factor (600k iterations under
		// -race would dominate the suite).
		PasswordHasher: core.PBKDF2Hasher{Iterations: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := eng.HTTPHandler(hmnpay.HTTPOptions{
		Limits: in.opts.limits,
		Stream: hmnpay.StreamOptions{ActivePoll: 20 * time.Millisecond, IdlePoll: 200 * time.Millisecond, Debounce: 5 * time.Millisecond, Heartbeat: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	in.eng, in.handler = eng, h
	in.srv = httptest.NewServer(h)
	ctx, cancel := context.WithCancel(context.Background())
	in.stopRun = cancel
	in.runDone = make(chan error, 1)
	go func() { in.runDone <- eng.Run(ctx) }()
}

// stop shuts the instance down gracefully, in the binary's order: HTTP
// (and its event streams), then the workers, then the database.
func (in *instance) stop() {
	if in.eng == nil {
		return
	}
	t := in.t
	in.handler.Close()
	in.srv.Close()
	in.stopRun()
	select {
	case err := <-in.runDone:
		if err != nil {
			t.Errorf("engine Run: %v", err)
		}
	case <-time.After(waitFor):
		t.Error("engine did not stop")
	}
	if err := in.eng.Close(); err != nil {
		t.Errorf("engine Close: %v", err)
	}
	in.eng = nil
}

func withoutDebug(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.Contains(l, "level=DEBUG") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// ---- HTTP ----

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) String() string { return fmt.Sprintf("%d %s", r.status, bytes.TrimSpace(r.body)) }

// errCode returns the API error code of an error response.
func (r response) errCode() string {
	var b struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.body, &b)
	return b.Error.Code
}

type reqOption func(*http.Request)

// bearer authenticates with an API key (or a session token).
func bearer(secret string) reqOption {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

func header(k, v string) reqOption { return func(r *http.Request) { r.Header.Set(k, v) } }

// call sends a request; without bearer it carries the session cookie, if
// any.
func (in *instance) call(method, path string, body any, opts ...reqOption) response {
	in.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			in.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, in.srv.URL+path, rd)
	if err != nil {
		in.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	res, err := in.http.Do(req)
	if err != nil {
		in.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return response{status: res.StatusCode, header: res.Header, body: b}
}

// must sends a request and requires the given status.
func (in *instance) must(want int, method, path string, body any, opts ...reqOption) response {
	in.t.Helper()
	r := in.call(method, path, body, opts...)
	if r.status != want {
		in.t.Fatalf("%s %s = %s; want %d", method, path, r, want)
	}
	return r
}

func decode[T any](t *testing.T, r response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %T from %s: %v", v, r, err)
	}
	return v
}

type list[T any] struct {
	Data []T `json:"data"`
}

// eventually polls cond until it holds or waitFor elapses.
func (in *instance) eventually(what string, cond func() bool) {
	in.t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			in.t.Fatalf("timed out after %s waiting for: %s", waitFor, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---- setup and fixtures ----

// setup runs the first-run wizard over HTTP (as the web wizard would) and
// leaves the operator signed in with a session cookie. It returns an API
// key holding every scope.
func (in *instance) setup() string {
	t := in.t
	t.Helper()
	in.completeSetup()
	in.must(200, "PUT", "/api/v1/settings/payments", map[string]any{
		"ttl_seconds": 3600, "required_confirmations": confirmations, "underpayment_tolerance": "0",
	})
	in.synced()
	return in.createAPIKey("integration", core.Scopes...)
}

func (in *instance) completeSetup() {
	t := in.t
	t.Helper()
	r := in.must(201, "POST", "/api/v1/setup/complete", map[string]any{
		"setup_token": in.eng.SetupToken(),
		"email":       operatorEmail, "password": operatorPassword,
		"chain_id": core.ChainBSCMainnet, "rpc_urls": in.urls, "xpub": hardhatXpub,
		"merchant_name": "Northwind", "support_email": "help@example.com",
	})
	if len(r.header.Values("Set-Cookie")) == 0 {
		t.Fatal("setup did not sign the operator in")
	}
}

func (in *instance) createAPIKey(name string, scopes ...core.Scope) string {
	in.t.Helper()
	r := in.must(201, "POST", "/api/v1/api-keys", map[string]any{"name": name, "scopes": scopes})
	k := decode[struct {
		Secret string `json:"secret"`
	}](in.t, r)
	if !strings.HasPrefix(k.Secret, "hmn_live_") {
		in.t.Fatalf("api key secret %q", k.Secret)
	}
	return k.Secret
}

// endpoint registers the receiver for every payment event and returns the
// endpoint id and signing secret.
func (in *instance) endpoint(key string) (id, secret string) {
	in.t.Helper()
	r := in.must(201, "POST", "/api/v1/webhook-endpoints", map[string]any{
		"url": in.recv.url(), "description": "shop", "events": core.WebhookEventTypes,
	}, bearer(key))
	e := decode[struct {
		ID           string `json:"id"`
		Secret       string `json:"secret"`
		SecretMasked bool   `json:"secret_masked"`
	}](in.t, r)
	if e.SecretMasked || !strings.HasPrefix(e.Secret, "whsec_") {
		in.t.Fatalf("endpoint secret not revealed on create: %+v", e)
	}
	return e.ID, e.Secret
}

func (in *instance) createPayment(key, amount string, extra ...map[string]any) core.PaymentView {
	in.t.Helper()
	body := map[string]any{"amount": amount}
	for _, e := range extra {
		for k, v := range e {
			body[k] = v
		}
	}
	return decode[core.PaymentView](in.t, in.must(201, "POST", "/api/v1/payments", body, bearer(key)))
}

func (in *instance) payment(key, id string) core.PaymentView {
	in.t.Helper()
	return decode[core.PaymentView](in.t, in.must(200, "GET", "/api/v1/payments/"+id, nil, bearer(key)))
}

type paymentDetail struct {
	Payment   core.PaymentView    `json:"payment"`
	Transfers []core.TransferView `json:"transfers"`
	Timeline  []struct {
		ID   string         `json:"id"`
		Type core.EventType `json:"type"`
	} `json:"timeline"`
	Deliveries []struct {
		ID        string              `json:"id"`
		EventID   string              `json:"event_id"`
		EventType core.EventType      `json:"event_type"`
		Status    core.DeliveryStatus `json:"status"`
		Attempt   int                 `json:"attempt"`
	} `json:"deliveries"`
}

// detail reads the dashboard's payment page (operator session).
func (in *instance) detail(id string) paymentDetail {
	in.t.Helper()
	return decode[paymentDetail](in.t, in.must(200, "GET", "/api/v1/payments/"+id+"/detail", nil))
}

// timeline returns the payment's event types without the "payment." prefix.
func (in *instance) timeline(id string) string {
	in.t.Helper()
	var names []string
	for _, e := range in.detail(id).Timeline {
		names = append(names, strings.TrimPrefix(string(e.Type), "payment."))
	}
	return strings.Join(names, " ")
}

func (in *instance) waitStatus(key, id string, want core.PaymentStatus) core.PaymentView {
	in.t.Helper()
	var p core.PaymentView
	in.eventually(fmt.Sprintf("payment %s to become %s", id, want), func() bool {
		p = in.payment(key, id)
		return p.Status == want
	})
	return p
}

// ---- chain ----

// pay sends whole USDT units to a payment's address; the transaction is
// mined with the next block.
func (in *instance) pay(address string, whole int64) evm.Hash {
	return in.payUnits(address, new(big.Int).Mul(big.NewInt(whole), big.NewInt(1e18)))
}

func (in *instance) payUnits(address string, units *big.Int) evm.Hash {
	in.t.Helper()
	to, err := ethaddr.Parse(strings.ToLower(address))
	if err != nil {
		in.t.Fatal(err)
	}
	return in.chain.Transfer(usdt, payer, to, units)
}

// mine mines n blocks stamped with the clock's current time.
func (in *instance) mine(n int) evmtest.Block {
	in.chain.SetTime(in.clock.Now())
	return in.chain.Mine(n)
}

type nodeStatus struct {
	Healthy bool `json:"healthy"`
	Head    *struct {
		Number uint64 `json:"number"`
		Hash   string `json:"hash"`
	} `json:"head"`
	Endpoints []struct {
		Healthy bool `json:"healthy"`
	} `json:"endpoints"`
}

// synced waits until the instance has processed the chain up to the tip
// minus the settle depth, as reported by the node-status API.
func (in *instance) synced() {
	in.t.Helper()
	in.eventually("the processed head to reach tip - settle depth", func() bool {
		blk, ok := in.chain.BlockByNumber(in.chain.Head().Number - settleDepth)
		ns := decode[nodeStatus](in.t, in.must(200, "GET", "/api/v1/node-status", nil))
		return ok && ns.Head != nil && ns.Head.Number == blk.Number && ns.Head.Hash == blk.Hash.String()
	})
}

// ---- checkout stream ----

// checkoutStream is an open EventSource on a payment's public stream.
type checkoutStream struct {
	cancel context.CancelFunc
	views  chan core.CheckoutView
	ids    chan string // the SSE id of each view, in the same order
	done   chan struct{}
	lastID string // id of the view next/until returned last
}

func (in *instance) openStream(paymentID string) *checkoutStream {
	in.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", in.srv.URL+"/api/v1/public/payments/"+url.PathEscape(paymentID)+"/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		in.t.Fatal(err)
	}
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		in.t.Fatalf("stream: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	s := &checkoutStream{cancel: cancel, views: make(chan core.CheckoutView, 1024), ids: make(chan string, 1024), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		var event, id string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == "checkout":
				var v core.CheckoutView
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v) == nil {
					s.views <- v
					s.ids <- id
				}
			case line == "":
				event, id = "", ""
			}
		}
	}()
	in.t.Cleanup(s.close)
	return s
}

func (s *checkoutStream) close() {
	s.cancel()
	<-s.done
}

// next returns the next view the stream sends.
func (s *checkoutStream) next(t *testing.T) core.CheckoutView {
	t.Helper()
	select {
	case v := <-s.views:
		s.lastID = <-s.ids
		return v
	case <-time.After(waitFor):
		t.Fatal("checkout stream sent nothing")
	case <-s.done:
		t.Fatal("checkout stream ended")
	}
	panic("unreachable")
}

// until reads views until one has the wanted status and returns every view
// read on the way, including it.
func (s *checkoutStream) until(t *testing.T, want core.PaymentStatus) []core.CheckoutView {
	t.Helper()
	var seen []core.CheckoutView
	for {
		v := s.next(t)
		seen = append(seen, v)
		if v.Status == want {
			return seen
		}
	}
}

// setupBody is a complete, valid setup request with the given token.
func (in *instance) setupBody(token string) map[string]any {
	return map[string]any{
		"setup_token": token, "email": operatorEmail, "password": operatorPassword,
		"chain_id": core.ChainBSCMainnet, "rpc_urls": in.urls, "xpub": hardhatXpub,
		"merchant_name": "Northwind", "support_email": "help@example.com",
	}
}

// verifySignatures checks every delivery's Hmnpay-Signature with the
// core's own verification function, as a Go receiver would.
func verifySignatures(t *testing.T, clock *testClock, secret string, ds []delivery) {
	t.Helper()
	for _, d := range ds {
		if err := core.VerifyWebhookSignature(d.Signature, d.Body, []string{secret}, clock.Now(), time.Hour); err != nil {
			t.Errorf("delivery %s (%s): signature does not verify: %v", d.DeliveryID, d.Envelope.Type, err)
		}
		if d.EventID != d.Envelope.ID {
			t.Errorf("delivery %s: event id header %q != body id %q", d.DeliveryID, d.EventID, d.Envelope.ID)
		}
	}
	// And a forged body must not verify.
	if len(ds) > 0 {
		forged := append(bytes.Clone(ds[0].Body), ' ')
		if core.VerifyWebhookSignature(ds[0].Signature, forged, []string{secret}, clock.Now(), time.Hour) == nil {
			t.Error("a modified body verified")
		}
	}
}
