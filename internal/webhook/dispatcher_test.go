package webhook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
	"github.com/lubluniky/hmnpay/core/coretest"
)

// ---- harness ----

type harness struct {
	t   *testing.T
	env *coretest.Env
	op  context.Context
}

func newHarness(t *testing.T, opts ...coretest.EnvOption) *harness {
	t.Helper()
	env := coretest.NewEnv(t, opts...)
	return &harness{t: t, env: env, op: env.Operator()}
}

// endpoint registers an enabled endpoint subscribed to payment.created.
func (h *harness) endpoint(url string) core.WebhookEndpoint {
	h.t.Helper()
	e, err := h.env.Service.CreateWebhookEndpoint(h.op, core.WebhookEndpointInput{
		URL: url, Events: []core.EventType{core.EventPaymentCreated}, Enabled: true,
	})
	if err != nil {
		h.t.Fatalf("CreateWebhookEndpoint: %v", err)
	}
	return e
}

// sameHost creates n endpoints with the same URL: deliveries to one host
// that the core hands out side by side (first attempts to one endpoint go
// out one at a time, in event order).
func (h *harness) sameHost(url string, n int) []core.WebhookEndpoint {
	h.t.Helper()
	eps := make([]core.WebhookEndpoint, n)
	for i := range eps {
		eps[i] = h.endpoint(url)
	}
	return eps
}

// deliveriesOf returns the deliveries of several endpoints.
func (h *harness) deliveriesOf(eps []core.WebhookEndpoint) []core.WebhookDelivery {
	h.t.Helper()
	var all []core.WebhookDelivery
	for _, e := range eps {
		all = append(all, h.deliveries(e.ID)...)
	}
	return all
}

// pay creates a payment, which emits payment.created.
func (h *harness) pay() core.PaymentView { return h.env.CreatePayment("10.00") }

func (h *harness) deliveries(endpointID string) []core.WebhookDelivery {
	h.t.Helper()
	page, err := h.env.Service.ListWebhookDeliveries(h.op, core.DeliveryFilter{EndpointID: endpointID}, core.Page{Limit: 200})
	if err != nil {
		h.t.Fatalf("ListWebhookDeliveries: %v", err)
	}
	return page.Items
}

// only returns the single delivery of an endpoint.
func (h *harness) only(endpointID string) core.WebhookDelivery {
	h.t.Helper()
	ds := h.deliveries(endpointID)
	if len(ds) != 1 {
		h.t.Fatalf("endpoint %s has %d deliveries, want 1", endpointID, len(ds))
	}
	return ds[0]
}

// waitDelivery polls until the endpoint's single delivery satisfies cond.
func (h *harness) waitDelivery(endpointID string, what string, cond func(core.WebhookDelivery) bool) core.WebhookDelivery {
	h.t.Helper()
	var last core.WebhookDelivery
	waitFor(h.t, 5*time.Second, what, func() bool {
		ds := h.deliveries(endpointID)
		if len(ds) != 1 {
			return false
		}
		last = ds[0]
		return cond(last)
	}, func() string { return fmt.Sprintf("last: %+v", last) })
	return last
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool, detail ...func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			msg := ""
			for _, d := range detail {
				msg += " " + d()
			}
			t.Fatalf("timed out waiting for %s.%s", what, msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var loopbackOnly = EgressPolicy{AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}

// logBuffer collects log output for leak checks.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newDispatcher builds a dispatcher with fast test settings.
func newDispatcher(t *testing.T, svc *core.Service, opts Options) *Dispatcher {
	t.Helper()
	if opts.PollInterval == 0 {
		opts.PollInterval = 20 * time.Millisecond
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Egress.AllowedPrefixes == nil && opts.Egress.Resolver == nil {
		opts.Egress = loopbackOnly
	}
	d, err := New(svc, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.maxTimeout = 2 * time.Second
	d.backoffMin, d.backoffMax = 10*time.Millisecond, 100*time.Millisecond
	d.finalWait = 3 * time.Second
	return d
}

// start runs d until the returned stop function is called; stop returns
// Run's result. The test fails if Run does not return.
func start(t *testing.T, d *Dispatcher) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- d.Run(ctx) }()
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-errc:
			case <-time.After(20 * time.Second):
				t.Errorf("Run did not return after cancel")
			}
		})
		return result
	}
	t.Cleanup(func() { stop() })
	return stop
}

// ---- end to end ----

func TestDeliverEndToEnd(t *testing.T) {
	h := newHarness(t)
	type got struct {
		header http.Header
		body   []byte
	}
	reqs := make(chan got, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/hook" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reqs <- got{r.Header.Clone(), body}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	ep := h.endpoint(srv.URL + "/hook")

	logs := &logBuffer{}
	d := newDispatcher(t, h.env.Service, Options{
		// The nudge, not the timer, must drive delivery.
		PollInterval: time.Hour,
		Logger:       slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	stop := start(t, d)
	time.Sleep(50 * time.Millisecond) // let the initial (empty) pass finish
	p := h.pay()

	var r got
	select {
	case r = <-reqs:
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook received (nudge not honoured?)")
	}
	if err := core.VerifyWebhookSignature(r.header.Get(core.HeaderSignature), r.body, []string{ep.Secret}, h.env.Clock.Now(), 0); err != nil {
		t.Fatalf("signature: %v", err)
	}
	var env core.EventEnvelope
	if err := json.Unmarshal(r.body, &env); err != nil || env.Type != core.EventPaymentCreated {
		t.Fatalf("body %s: %v", r.body, err)
	}
	var view core.PaymentView
	if err := json.Unmarshal(env.Data, &view); err != nil || view.ID != p.ID {
		t.Fatalf("payment in body: %+v %v", view, err)
	}
	if ua := r.header.Get("User-Agent"); ua != "hmnpay-webhook/"+Version {
		t.Errorf("User-Agent %q", ua)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	if r.header.Get(core.HeaderEventID) != env.ID || r.header.Get(core.HeaderDeliveryID) == "" {
		t.Errorf("id headers: %v", r.header)
	}

	dl := h.waitDelivery(ep.ID, "success", func(d core.WebhookDelivery) bool { return d.Status == core.DeliverySucceeded })
	if dl.Attempt != 1 || dl.ResponseCode != 200 || dl.ResponseBody != "ok" || dl.Error != "" || len(dl.Attempts) != 1 || !dl.Attempts[0].Succeeded {
		t.Fatalf("delivery: %+v", dl)
	}
	if dl.ID != r.header.Get(core.HeaderDeliveryID) {
		t.Fatalf("delivery id header %q, want %q", r.header.Get(core.HeaderDeliveryID), dl.ID)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := logs.String()
	for _, secret := range []string{ep.Secret, "v1=", core.HeaderSignature, "/hook"} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain %q:\n%s", secret, out)
		}
	}
}

func TestRetryAfterServerError(t *testing.T) {
	h := newHarness(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	ep := h.endpoint(srv.URL)
	h.pay()
	start(t, newDispatcher(t, h.env.Service, Options{}))

	dl := h.waitDelivery(ep.ID, "first failure", func(d core.WebhookDelivery) bool { return d.Attempt == 1 })
	if dl.Status != core.DeliveryRetrying || dl.ResponseCode != 500 || !strings.HasPrefix(dl.ResponseBody, "boom") || dl.Error != "" {
		t.Fatalf("after 500: %+v", dl)
	}
	time.Sleep(100 * time.Millisecond) // several polls: nothing is due yet
	if n := calls.Load(); n != 1 {
		t.Fatalf("retried before the backoff: %d calls", n)
	}
	wait, _ := core.WebhookBackoff(1)
	h.env.Clock.Advance(wait)
	dl = h.waitDelivery(ep.ID, "retry success", func(d core.WebhookDelivery) bool { return d.Status == core.DeliverySucceeded })
	if dl.Attempt != 2 || dl.ResponseCode != 204 {
		t.Fatalf("after retry: %+v", dl)
	}
}

func TestTimeout(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // lets the server notice the client hanging up
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()
	ep := h.endpoint(srv.URL)
	h.pay()
	d := newDispatcher(t, h.env.Service, Options{})
	d.maxTimeout = 300 * time.Millisecond
	start(t, d)
	dl := h.waitDelivery(ep.ID, "timeout", func(d core.WebhookDelivery) bool { return d.Attempt == 1 })
	if dl.Error != ResultTimeout || dl.ResponseCode != 0 || dl.DurationMs < 290 || dl.DurationMs > 2000 {
		t.Fatalf("delivery: %+v", dl)
	}
}

// TestSlowLorisHeaders: an endpoint that takes the request and then trickles
// its response headers, one every 20 ms, for ever. The attempt ends at its
// deadline and is recorded as a timeout.
//
// The server reads the whole request before it writes anything. It used to
// write "HTTP/1.1 200 OK" the moment the connection was accepted, and about
// once in two hundred runs those bytes reached the client before the client
// had written its request: net/http then sees a server speaking out of turn
// on an idle connection, drops the connection, and the attempt is recorded as
// connection_error — correctly, but that is another scenario than the one
// this test is about.
func TestSlowLorisHeaders(t *testing.T) {
	h := newHarness(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var served atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				if _, err := io.Copy(io.Discard, req.Body); err != nil {
					return
				}
				served.Add(1)
				io.WriteString(c, "HTTP/1.1 200 OK\r\n")
				for i := 0; i < 1000; i++ {
					if _, err := fmt.Fprintf(c, "X-Slow-%d: a\r\n", i); err != nil {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}()
		}
	}()
	ep := h.endpoint("http://" + ln.Addr().String() + "/")
	h.pay()
	d := newDispatcher(t, h.env.Service, Options{})
	d.maxTimeout = 300 * time.Millisecond
	start(t, d)
	dl := h.waitDelivery(ep.ID, "timeout", func(d core.WebhookDelivery) bool { return d.Attempt == 1 })
	if dl.Error != ResultTimeout || dl.ResponseCode != 0 || dl.DurationMs > 2000 {
		t.Fatalf("delivery: %+v", dl)
	}
	if served.Load() == 0 {
		t.Fatal("the attempt ended before the server had read the request: this was not a slow response")
	}
}

func TestConnectionRefused(t *testing.T) {
	h := newHarness(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ep := h.endpoint("http://" + addr + "/hook")
	h.pay()
	start(t, newDispatcher(t, h.env.Service, Options{}))
	dl := h.waitDelivery(ep.ID, "refused", func(d core.WebhookDelivery) bool { return d.Attempt == 1 })
	if dl.Error != ResultConnectionRefused || dl.ResponseCode != 0 || dl.Status != core.DeliveryRetrying {
		t.Fatalf("delivery: %+v", dl)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	h := newHarness(t)
	var followed atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) { followed.Add(1) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ep := h.endpoint(srv.URL + "/hook")
	h.pay()
	start(t, newDispatcher(t, h.env.Service, Options{}))
	dl := h.waitDelivery(ep.ID, "redirect", func(d core.WebhookDelivery) bool { return d.Attempt == 1 })
	if dl.ResponseCode != http.StatusTemporaryRedirect || dl.Status != core.DeliveryRetrying || dl.Error != "" {
		t.Fatalf("delivery: %+v", dl)
	}
	if followed.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestEndlessResponseBody(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte("A"), 1024)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	trickle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, strings.Repeat("B", 5000)) // more than the cap …
		w.(http.Flusher).Flush()
		for r.Context().Err() == nil { // … then a body that never ends
			if _, err := io.WriteString(w, "B"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer trickle.Close()
	fast := h.endpoint(srv.URL)
	slow := h.endpoint(trickle.URL)
	h.pay()
	d := newDispatcher(t, h.env.Service, Options{})
	d.maxTimeout = 5 * time.Second
	start(t, d)
	for _, id := range []string{fast.ID, slow.ID} {
		dl := h.waitDelivery(id, "success", func(d core.WebhookDelivery) bool { return d.Attempt == 1 })
		if dl.Status != core.DeliverySucceeded || len(dl.ResponseBody) != core.WebhookMaxResponseBody {
			t.Fatalf("delivery: status %s, body %d bytes", dl.Status, len(dl.ResponseBody))
		}
		if dl.DurationMs > 2000 {
			t.Fatalf("an endless body held the attempt for %d ms", dl.DurationMs)
		}
	}
}

// panicTransport panics on its first request and forwards the rest.
type panicTransport struct {
	n    atomic.Int32
	next http.RoundTripper
}

func (p *panicTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if p.n.Add(1) == 1 {
		panic("transport exploded")
	}
	return p.next.RoundTrip(r)
}

func TestPanicInHTTPPath(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	ep := h.endpoint(srv.URL)
	h.pay()
	d := newDispatcher(t, h.env.Service, Options{Transport: &panicTransport{next: srv.Client().Transport}})
	start(t, d)
	dl := h.waitDelivery(ep.ID, "panic recorded", func(d core.WebhookDelivery) bool { return d.Attempt == 1 })
	if dl.Error != ResultInternalError || dl.Status != core.DeliveryRetrying {
		t.Fatalf("delivery: %+v", dl)
	}
	h.env.Clock.Advance(time.Minute)
	h.waitDelivery(ep.ID, "success after panic", func(d core.WebhookDelivery) bool { return d.Status == core.DeliverySucceeded })
}

func TestTLSVerified(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	ep := h.endpoint(srv.URL)
	h.pay()
	start(t, newDispatcher(t, h.env.Service, Options{}))
	dl := h.waitDelivery(ep.ID, "tls failure", func(d core.WebhookDelivery) bool { return d.Attempt == 1 })
	if dl.Error != ResultTLSError || dl.ResponseCode != 0 {
		t.Fatalf("delivery: %+v", dl)
	}
}

func TestBlockedByPolicy(t *testing.T) {
	h := newHarness(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, r.Host)
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	literal := h.endpoint(srv.URL)
	byName := h.endpoint(fmt.Sprintf("http://receiver.test:%d/hook", port))
	resolver := &fakeResolver{answers: map[string][][]string{"receiver.test": {{"127.0.0.1"}}}}
	h.pay()

	// Default policy: both refused, the receiver never sees a request.
	d := newDispatcher(t, h.env.Service, Options{Egress: EgressPolicy{Resolver: resolver}})
	stop := start(t, d)
	for _, id := range []string{literal.ID, byName.ID} {
		dl := h.waitDelivery(id, "refusal", func(d core.WebhookDelivery) bool { return d.Attempt == 1 })
		if dl.Error != ResultBlockedByPolicy || dl.ResponseCode != 0 {
			t.Fatalf("delivery: %+v", dl)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("blocked destination was contacted")
	}
	stop()

	// With the loopback allowlisted the name resolves through the guard
	// and the request carries the original Host.
	allow := loopbackOnly
	allow.Resolver = resolver
	h.env.Clock.Advance(time.Minute)
	start(t, newDispatcher(t, h.env.Service, Options{Egress: allow}))
	dl := h.waitDelivery(byName.ID, "delivery by name", func(d core.WebhookDelivery) bool { return d.Status == core.DeliverySucceeded })
	if want := fmt.Sprintf("receiver.test:%d", port); dl.ResponseBody != want {
		t.Fatalf("Host seen by receiver %q, want %q", dl.ResponseBody, want)
	}
}

// ---- endpoint changes while a request is in flight ----

// blockingServer holds every request until release is closed (or the
// client gives up).
type blockingServer struct {
	*httptest.Server
	arrived chan string // delivery ids
	release chan struct{}
	once    sync.Once
}

func newBlockingServer(t *testing.T) *blockingServer {
	b := &blockingServer{arrived: make(chan string, 100), release: make(chan struct{})}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		b.arrived <- r.Header.Get(core.HeaderDeliveryID)
		select {
		case <-b.release:
			io.WriteString(w, "late ok")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { b.Release(); b.Close() })
	return b
}

func (b *blockingServer) Release() { b.once.Do(func() { close(b.release) }) }

func (b *blockingServer) waitArrival(t *testing.T) string {
	t.Helper()
	select {
	case id := <-b.arrived:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("no request arrived")
		return ""
	}
}

func TestEndpointDisabledMidFlight(t *testing.T) {
	h := newHarness(t)
	b := newBlockingServer(t)
	ep := h.endpoint(b.URL)
	h.pay()
	start(t, newDispatcher(t, h.env.Service, Options{}))
	b.waitArrival(t)
	if _, err := h.env.Service.UpdateWebhookEndpoint(h.op, ep.ID, core.WebhookEndpointInput{
		URL: ep.URL, Events: ep.Events, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	// Disabling failed the delivery and revoked its claim …
	if dl := h.only(ep.ID); dl.Status != core.DeliveryFailed || dl.Error != "endpoint disabled" || dl.Attempt != 0 || dl.ClaimID != "" {
		t.Fatalf("delivery after disabling: %+v", dl)
	}
	b.Release()
	// … but the request was already on the wire and the endpoint answered
	// 200: the record says what happened instead of "failed, 0 attempts".
	dl := h.waitDelivery(ep.ID, "late success recorded", func(d core.WebhookDelivery) bool { return d.Status == core.DeliverySucceeded })
	if dl.Attempt != 1 || dl.Error != "" || dl.ResponseCode != 200 || dl.ResponseBody != "late ok" ||
		len(dl.Attempts) != 1 || !dl.Attempts[0].Late || !dl.Attempts[0].Succeeded {
		t.Fatalf("delivery: %+v", dl)
	}
	// The endpoint stays disabled.
	sum, err := h.env.Service.GetWebhookEndpoint(h.op, ep.ID)
	if err != nil || sum.Enabled {
		t.Fatalf("endpoint: %+v, %v", sum, err)
	}
}

// statusServer holds every request until Release, then answers with the
// configured status.
type statusServer struct {
	*httptest.Server
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

func newStatusServer(t *testing.T, status int, body string) *statusServer {
	s := &statusServer{arrived: make(chan struct{}, 10), release: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		s.arrived <- struct{}{}
		select {
		case <-s.release:
			w.WriteHeader(status)
			io.WriteString(w, body)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { s.Release(); s.Close() })
	return s
}

func (s *statusServer) Release() { s.once.Do(func() { close(s.release) }) }

// The stale-completion race: a dispatcher's attempt outlives its claim
// lease, another worker claims the same delivery and records a result,
// and only then does the first dispatcher report. Its result must not be
// counted as another attempt and must not overwrite the newer one.
func TestStaleCompletionIsFenced(t *testing.T) {
	for _, tc := range []struct {
		name string
		// the slow dispatcher's endpoint answer, arriving last
		slowStatus int
		// what the other worker recorded in between
		other core.AttemptResult
		// the delivery afterwards
		wantStatus core.DeliveryStatus
		wantCode   int
		wantBody   string
	}{
		{"late failure changes nothing", 503, core.AttemptResult{ResponseCode: 500, ResponseBody: "other worker"},
			core.DeliveryRetrying, 500, "other worker"},
		{"late failure cannot undo a success", 503, core.AttemptResult{ResponseCode: 200, ResponseBody: "other worker"},
			core.DeliverySucceeded, 200, "other worker"},
		{"late success is still a delivery", 200, core.AttemptResult{ResponseCode: 500, ResponseBody: "other worker"},
			core.DeliverySucceeded, 200, "slow worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			srv := newStatusServer(t, tc.slowStatus, "slow worker")
			ep := h.endpoint(srv.URL)
			h.pay()
			logs := &logBuffer{}
			// No polling: after its first claim this dispatcher claims nothing
			// else, so the test controls who holds the delivery.
			d := newDispatcher(t, h.env.Service, Options{
				PollInterval: time.Hour,
				Logger:       slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			})
			stop := start(t, d)
			select {
			case <-srv.arrived:
			case <-time.After(5 * time.Second):
				t.Fatal("no request arrived")
			}
			slowClaim := h.only(ep.ID).ClaimID
			if slowClaim == "" {
				t.Fatal("the in-flight delivery has no claim")
			}

			// The lease runs out; another worker claims the delivery and
			// records its result.
			h.env.Clock.Advance(core.WebhookClaimLease)
			sys := h.env.System()
			jobs, err := h.env.Service.ClaimWebhookDeliveries(sys, 10)
			if err != nil || len(jobs) != 1 || jobs[0].ClaimID == slowClaim || jobs[0].Attempt != 1 {
				t.Fatalf("re-claim: %+v, %v", jobs, err)
			}
			res := tc.other
			res.ClaimID, res.Attempt = jobs[0].ClaimID, jobs[0].Attempt
			if out, err := h.env.Service.CompleteWebhookDelivery(sys, jobs[0].DeliveryID, res); err != nil || out != core.CompletionCounted {
				t.Fatalf("other worker's completion: %s, %v", out, err)
			}
			counted := h.only(ep.ID)

			// Now the slow dispatcher's request finishes.
			srv.Release()
			dl := h.waitDelivery(ep.ID, "the late attempt to be recorded", func(d core.WebhookDelivery) bool { return len(d.Attempts) == 2 })
			if dl.Attempt != 1 {
				t.Errorf("attempts counted = %d, want 1 (no double count)", dl.Attempt)
			}
			if dl.Status != tc.wantStatus || dl.ResponseCode != tc.wantCode || dl.ResponseBody != tc.wantBody {
				t.Errorf("delivery: status %s code %d body %q; want %s %d %q", dl.Status, dl.ResponseCode, dl.ResponseBody, tc.wantStatus, tc.wantCode, tc.wantBody)
			}
			if dl.Attempts[0].Late || dl.Attempts[0].N != 1 || !dl.Attempts[1].Late || dl.Attempts[1].N != 1 || dl.Attempts[1].ResponseCode != tc.slowStatus {
				t.Errorf("attempt history: %+v", dl.Attempts)
			}
			if tc.wantStatus == core.DeliveryRetrying && !dl.NextAttemptAt.Equal(counted.NextAttemptAt) {
				t.Errorf("the stale result moved the schedule: %v, want %v", dl.NextAttemptAt, counted.NextAttemptAt)
			}
			sum, err := h.env.Service.GetWebhookEndpoint(h.op, ep.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantFailures := 1
			if tc.wantStatus == core.DeliverySucceeded {
				wantFailures = 0
			}
			if sum.ConsecutiveFailures != wantFailures {
				t.Errorf("endpoint failures = %d, want %d", sum.ConsecutiveFailures, wantFailures)
			}
			if err := stop(); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !strings.Contains(logs.String(), "recorded as late") {
				t.Errorf("the dispatcher did not notice its stale completion:\n%s", logs.String())
			}
		})
	}
}

func TestEndpointDeletedMidFlight(t *testing.T) {
	h := newHarness(t)
	b := newBlockingServer(t)
	gone := h.endpoint(b.URL)
	var hits atomic.Int32
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer ok.Close()
	h.pay()
	start(t, newDispatcher(t, h.env.Service, Options{}))
	b.waitArrival(t)
	if err := h.env.Service.DeleteWebhookEndpoint(h.op, gone.ID); err != nil {
		t.Fatal(err)
	}
	b.Release()
	// The dispatcher shrugs off the vanished delivery and keeps working.
	healthy := h.endpoint(ok.URL)
	h.pay()
	h.waitDelivery(healthy.ID, "next delivery", func(d core.WebhookDelivery) bool { return d.Status == core.DeliverySucceeded })
}

// ---- shutdown ----

func TestShutdownDrainsInFlight(t *testing.T) {
	h := newHarness(t)
	b := newBlockingServer(t)
	ep := h.endpoint(b.URL)
	h.pay()
	d := newDispatcher(t, h.env.Service, Options{ShutdownGrace: 5 * time.Second})
	stop := start(t, d)
	b.waitArrival(t)
	go func() { time.Sleep(100 * time.Millisecond); b.Release() }()
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dl := h.only(ep.ID)
	if dl.Status != core.DeliverySucceeded || dl.ResponseBody != "late ok" {
		t.Fatalf("delivery: %+v", dl)
	}
}

func TestShutdownAbortsAfterGraceAndReleasesUnstarted(t *testing.T) {
	h := newHarness(t)
	b := newBlockingServer(t)
	eps := h.sameHost(b.URL, 3)
	h.pay()
	d := newDispatcher(t, h.env.Service, Options{Concurrency: 4, MaxPerHost: 1, ShutdownGrace: 200 * time.Millisecond})
	stop := start(t, d)
	first := b.waitArrival(t)
	time.Sleep(50 * time.Millisecond) // the other two are parked behind the per-host cap
	began := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("shutdown took %v", took)
	}
	var aborted, released int
	for _, dl := range h.deliveriesOf(eps) {
		switch {
		case dl.ID == first:
			// Started: always completed, here as aborted.
			if dl.Attempt != 1 || dl.Error != ResultAborted || dl.Status != core.DeliveryRetrying {
				t.Fatalf("in-flight delivery: %+v", dl)
			}
			aborted++
		default:
			// Never started: no attempt recorded, and the claim was given
			// back, so it is due at once instead of after the lease.
			if dl.Attempt != 0 || len(dl.Attempts) != 0 || dl.Status != core.DeliveryPending || dl.ClaimID != "" || dl.NextAttemptAt.After(h.env.Clock.Now()) {
				t.Fatalf("parked delivery: %+v", dl)
			}
			released++
		}
	}
	if aborted != 1 || released != 2 {
		t.Fatalf("aborted %d, released %d", aborted, released)
	}
	if len(b.arrived) != 0 {
		t.Fatal("a parked delivery was sent")
	}

	// A new dispatcher sends the released ones right away — no lease to
	// wait for (the core's clock has not moved) …
	b.Release()
	start(t, newDispatcher(t, h.env.Service, Options{}))
	waitFor(t, 5*time.Second, "released deliveries delivered without waiting", func() bool {
		n := 0
		for _, dl := range h.deliveriesOf(eps) {
			if dl.ID != first && dl.Status == core.DeliverySucceeded && dl.Attempt == 1 {
				n++
			}
		}
		return n == 2
	})
	// … and the aborted one after its retry backoff.
	h.env.Clock.Advance(time.Minute)
	waitFor(t, 5*time.Second, "all delivered", func() bool {
		for _, dl := range h.deliveriesOf(eps) {
			if dl.Status != core.DeliverySucceeded {
				return false
			}
		}
		return true
	})
}

// A claimed delivery that cannot start early enough to finish inside its
// lease is never sent by this dispatcher.
func TestParkedDeliveryNotSentAfterLeaseBudget(t *testing.T) {
	h := newHarness(t)
	b := newBlockingServer(t)
	eps := h.sameHost(b.URL, 2)
	h.pay()
	d := newDispatcher(t, h.env.Service, Options{Concurrency: 4, MaxPerHost: 1})
	d.lease = 2 * time.Second
	d.leaseMargin = 200 * time.Millisecond
	d.maxTimeout = time.Second // start-by = claim + 0.8 s; the first attempt holds the host for 1 s
	start(t, d)
	first := b.waitArrival(t)
	time.Sleep(1500 * time.Millisecond)
	if n := len(b.arrived); n != 0 {
		t.Fatalf("%d parked deliveries were sent after their start-by time", n)
	}
	for _, dl := range h.deliveriesOf(eps) {
		if dl.ID == first {
			if dl.Error != ResultTimeout {
				t.Fatalf("first: %+v", dl)
			}
			continue
		}
		if dl.Attempt != 0 || len(dl.Attempts) != 0 {
			t.Fatalf("parked delivery was attempted: %+v", dl)
		}
		// It was given back to come again after one attempt timeout, not
		// left hidden for the rest of its lease.
		if dl.ClaimID != "" || dl.Status != core.DeliveryPending || !dl.NextAttemptAt.Equal(h.env.Clock.Now().Add(d.requeueDelay)) {
			t.Fatalf("parked delivery was not released: %+v", dl)
		}
	}
}

// Releases for a busy host are rate-limited: a saturated host with a long
// backlog must not turn into one store write per queued delivery.
func TestBusyHostReleaseBudget(t *testing.T) {
	h := newHarness(t)
	b := newBlockingServer(t)
	const total = 12
	eps := h.sameHost(b.URL, total)
	h.pay()
	d := newDispatcher(t, h.env.Service, Options{Concurrency: 2, MaxPerHost: 1})
	if d.requeueBudget != 4 || d.requeueDelay != core.WebhookTimeout {
		t.Fatalf("budget %d per %v", d.requeueBudget, d.requeueDelay)
	}
	start(t, d)
	b.waitArrival(t)
	// One in flight, two parked, the rest claimed with nowhere to go: four
	// of those are released (the budget), the others stay on their lease.
	var released, claimed int
	waitFor(t, 5*time.Second, "every delivery claimed once", func() bool {
		released, claimed = 0, 0
		now := h.env.Clock.Now()
		for _, dl := range h.deliveriesOf(eps) {
			switch {
			case dl.Attempt != 0 || dl.Status != core.DeliveryPending:
				t.Fatalf("unexpected delivery state: %+v", dl)
			case dl.ClaimID == "" && dl.NextAttemptAt.Equal(now.Add(d.requeueDelay)):
				released++
			case dl.ClaimID != "" && dl.NextAttemptAt.Equal(now.Add(core.WebhookClaimLease)):
				claimed++
			}
		}
		return released+claimed == total
	})
	if released != d.requeueBudget || claimed != total-d.requeueBudget {
		t.Fatalf("released %d (budget %d), still claimed %d (3 of them in flight or parked)", released, d.requeueBudget, claimed)
	}
	if n := len(b.arrived); n != 0 {
		t.Fatalf("%d more requests reached the busy host", n)
	}
}

// ---- several dispatchers ----

func TestTwoDispatchersDoNotDoubleDeliver(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		seen[r.Header.Get(core.HeaderDeliveryID)]++
		mu.Unlock()
	}))
	defer srv.Close()
	ep := h.endpoint(srv.URL)
	const n = 40
	for range n {
		h.pay()
	}
	start(t, newDispatcher(t, h.env.Service, Options{Concurrency: 3, BatchSize: 2}))
	start(t, newDispatcher(t, h.env.Service, Options{Concurrency: 3, BatchSize: 2}))
	waitFor(t, 10*time.Second, "all delivered", func() bool {
		ds := h.deliveries(ep.ID)
		for _, dl := range ds {
			if dl.Status != core.DeliverySucceeded {
				return false
			}
		}
		return len(ds) == n
	})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Fatalf("%d distinct deliveries received, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("delivery %s received %d times", id, c)
		}
	}
	for _, dl := range h.deliveries(ep.ID) {
		if dl.Attempt != 1 {
			t.Errorf("delivery %s has %d attempts", dl.ID, dl.Attempt)
		}
	}
}

// ---- loop behaviour ----

// countingStore counts transactions and can fail them.
type countingStore struct {
	core.Store
	updates atomic.Int64
	fail    atomic.Bool
}

var errStoreDown = errors.New("store is down")

func (s *countingStore) Update(ctx context.Context, fn func(tx core.Tx) error) error {
	s.updates.Add(1)
	if s.fail.Load() {
		return errStoreDown
	}
	return s.Store.Update(ctx, fn)
}

func TestNoBusyLoop(t *testing.T) {
	store := &countingStore{Store: coretest.NewMemStore()}
	h := newHarness(t, coretest.WithStore(store))

	// Idle: one plan and one claim per poll.
	d := newDispatcher(t, h.env.Service, Options{PollInterval: 50 * time.Millisecond})
	stop := start(t, d)
	before := store.updates.Load()
	time.Sleep(500 * time.Millisecond)
	if n := store.updates.Load() - before; n > 30 {
		t.Fatalf("idle dispatcher made %d transactions in 500 ms", n)
	}
	stop()

	// Failing core: exponential backoff, not one call per poll.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	ep := h.endpoint(srv.URL)
	h.pay()
	store.fail.Store(true)
	d = newDispatcher(t, h.env.Service, Options{PollInterval: 5 * time.Millisecond})
	d.backoffMin, d.backoffMax = 50*time.Millisecond, 200*time.Millisecond
	start(t, d)
	before = store.updates.Load()
	time.Sleep(600 * time.Millisecond)
	if n := store.updates.Load() - before; n > 12 {
		t.Fatalf("failing core was called %d times in 600 ms", n)
	}
	store.fail.Store(false)
	h.waitDelivery(ep.ID, "recovery", func(d core.WebhookDelivery) bool { return d.Status == core.DeliverySucceeded })
}

// One dead host must not occupy every worker.
func TestPerHostCapKeepsOthersFlowing(t *testing.T) {
	h := newHarness(t)
	b := newBlockingServer(t)
	dead := h.sameHost(b.URL, 6)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	h.pay() // six deliveries to the dead host first …
	d := newDispatcher(t, h.env.Service, Options{Concurrency: 4, MaxPerHost: 2})
	start(t, d)
	b.waitArrival(t)
	b.waitArrival(t)
	healthy := h.endpoint(ok.URL) // … then one to a healthy host
	h.pay()
	h.waitDelivery(healthy.ID, "healthy host served", func(d core.WebhookDelivery) bool { return d.Status == core.DeliverySucceeded })
	if n := len(b.arrived); n != 0 {
		t.Fatalf("dead host got %d more concurrent requests than its cap", n)
	}
	_ = dead
}

func TestRunTwiceConcurrently(t *testing.T) {
	h := newHarness(t)
	d := newDispatcher(t, h.env.Service, Options{})
	start(t, d)
	time.Sleep(20 * time.Millisecond)
	if err := d.Run(context.Background()); err == nil {
		t.Fatal("second concurrent Run succeeded")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("nil service accepted")
	}
	h := newHarness(t)
	d, err := New(h.env.Service, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.concurrency != DefaultConcurrency || d.maxPerHost != DefaultMaxPerHost || d.pollInterval != DefaultPollInterval ||
		d.batchSize != DefaultConcurrency || d.shutdownGrace != DefaultShutdownGrace {
		t.Fatalf("defaults: %+v", d)
	}
	if _, err := New(h.env.Service, Options{Egress: EgressPolicy{AllowedPrefixes: []netip.Prefix{{}}}}); err == nil {
		t.Fatal("invalid prefix accepted")
	}
}
