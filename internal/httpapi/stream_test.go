package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

type sseEvent struct {
	id, event, data string
	comment         bool
}

type sseClient struct {
	t      *testing.T
	resp   *http.Response
	events chan sseEvent
	cancel context.CancelFunc
}

func fastStreams(o *Options) {
	o.Stream = StreamOptions{Heartbeat: 40 * time.Millisecond, ActivePoll: 30 * time.Millisecond, IdlePoll: 60 * time.Millisecond,
		Debounce: 5 * time.Millisecond, MaxLifetime: 20 * time.Second,
		// Every stream in these tests comes from one loopback address; the
		// per-address share of a payment's streams has its own test.
		MaxPerIPPerPayment: 100}
}

func openStream(t *testing.T, url string, h hdr) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	r.Header.Set("Accept", "text/event-stream")
	for k, v := range h {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &sseClient{t: t, resp: resp, events: make(chan sseEvent, 256), cancel: cancel}
	t.Cleanup(c.close)
	if resp.StatusCode != http.StatusOK {
		close(c.events)
		return c
	}
	go func() {
		defer close(c.events)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev != (sseEvent{}) {
					c.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, ":"):
				c.events <- sseEvent{comment: true, data: line}
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return c
}

func (c *sseClient) close() {
	c.cancel()
	_ = c.resp.Body.Close()
}

// next returns the next checkout event matching pred.
func (c *sseClient) next(pred func(core.CheckoutView) bool) (sseEvent, core.CheckoutView) {
	c.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				c.t.Fatal("stream ended")
			}
			if ev.comment || ev.event != "checkout" {
				continue
			}
			var v core.CheckoutView
			if err := json.Unmarshal([]byte(ev.data), &v); err != nil {
				c.t.Fatalf("bad data %q: %v", ev.data, err)
			}
			if pred == nil || pred(v) {
				return ev, v
			}
		case <-deadline:
			c.t.Fatal("timed out waiting for a checkout event")
		}
	}
}

// quiet asserts that no checkout event arrives for d, and that a heartbeat
// does.
func (c *sseClient) quiet(d time.Duration) {
	c.t.Helper()
	timeout := time.After(d)
	ping := false
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				c.t.Fatal("stream ended")
			}
			if ev.comment {
				ping = ping || ev.data == ": ping"
				continue
			}
			if ev.event == "checkout" {
				c.t.Fatalf("unexpected event %+v", ev)
			}
		case <-timeout:
			if !ping {
				c.t.Fatal("no heartbeat")
			}
			return
		}
	}
}

func (c *sseClient) waitEnd() {
	c.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-c.events:
			if !ok {
				return
			}
		case <-deadline:
			c.t.Fatal("stream did not end")
		}
	}
}

// startServer serves h on a real listener. Its cleanup is registered
// before any stream client's, so clients disconnect first.
func startServer(t *testing.T, h http.Handler) *httptest.Server {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func waitStreams(t *testing.T, h *Handler, n int) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if h.streams.open() == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("open streams %d, want %d", h.streams.open(), n)
}

func seq(t *testing.T, ev sseEvent) int64 {
	t.Helper()
	n, err := strconv.ParseInt(ev.id, 10, 64)
	if err != nil {
		t.Fatalf("bad id %q", ev.id)
	}
	return n
}

func TestStream(t *testing.T) {
	e := newTenv(t, withOpts(fastStreams))
	srv := startServer(t, e.h)
	p := e.CreatePayment("10")
	url := srv.URL + "/api/v1/public/payments/" + p.ID + "/stream"

	c := openStream(t, url, nil)
	if ct := c.resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") || c.resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers %v", c.resp.Header)
	}
	first, v := c.next(nil)
	if v.Status != core.StatusAwaiting || v.ID != p.ID || seq(t, first) <= 0 {
		t.Fatalf("initial %+v %+v", first, v)
	}
	// The payload is the public view, never the raw event.
	if strings.Contains(first.data, "metadata") || strings.Contains(first.data, "customer_email") || strings.Contains(first.data, "needs_attention") {
		t.Fatalf("private fields in stream: %s", first.data)
	}

	// A partial payment is pushed (nudge from the core).
	e.Pay(e.Payment(p.ID), "4", 100)
	det, v := c.next(func(v core.CheckoutView) bool { return v.Status == core.StatusDetected })
	if seq(t, det) <= seq(t, first) || v.AmountReceived != "4.00" {
		t.Fatalf("detected %+v %+v", det, v)
	}
	// Topped up: confirming at 1 confirmation.
	e.Pay(e.Payment(p.ID), "6", 100)
	e.Head(100)
	conf, v := c.next(func(v core.CheckoutView) bool { return v.Status == core.StatusConfirming })
	if v.Confirmations != 1 {
		t.Fatalf("confirming %+v", v)
	}
	// Confirmations grow without any event: the active poll picks them up.
	e.Head(105)
	more, v := c.next(func(v core.CheckoutView) bool { return v.Confirmations == 6 })
	if more.id != conf.id || v.Status != core.StatusConfirming {
		t.Fatalf("confirmation refresh %+v %+v", more, v)
	}
	e.Head(120)
	paid, v := c.next(func(v core.CheckoutView) bool { return v.Status == core.StatusPaid })
	if seq(t, paid) <= seq(t, conf) || v.PaidAt == nil {
		t.Fatalf("paid %+v %+v", paid, v)
	}
	c.quiet(150 * time.Millisecond) // no duplicates, heartbeats flow

	// Resume at the latest id: nothing to send, only heartbeats.
	r := openStream(t, url, hdr{"Last-Event-ID": paid.id})
	r.quiet(200 * time.Millisecond)
	// Resume from a stale id: the current snapshot comes immediately.
	s := openStream(t, url, hdr{"Last-Event-ID": first.id})
	if ev, v := s.next(nil); ev.id != paid.id || v.Status != core.StatusPaid {
		t.Fatalf("stale resume %+v", ev)
	}
	// A garbage Last-Event-ID is ignored (fresh snapshot).
	g := openStream(t, url, hdr{"Last-Event-ID": "x"})
	if ev, _ := g.next(nil); ev.id != paid.id {
		t.Fatalf("garbage resume %+v", ev)
	}

	// Disconnects release their slots (handlers return, nothing leaks).
	waitStreams(t, e.h, 4)
	c.close()
	r.close()
	waitStreams(t, e.h, 2)
	s.close()
	g.close()
	waitStreams(t, e.h, 0)
}

func TestStreamResumeThenChange(t *testing.T) {
	e := newTenv(t, withOpts(fastStreams))
	srv := startServer(t, e.h)
	p := e.CreatePayment("10")
	url := srv.URL + "/api/v1/public/payments/" + p.ID + "/stream"
	first, _ := openStream(t, url, nil).next(nil)
	r := openStream(t, url, hdr{"Last-Event-ID": first.id})
	r.quiet(150 * time.Millisecond)
	e.Pay(e.Payment(p.ID), "1", 50)
	if ev, v := r.next(nil); seq(t, ev) <= seq(t, first) || v.Status != core.StatusDetected {
		t.Fatalf("after resume %+v %+v", ev, v)
	}
}

func TestStreamLimitsAndErrors(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) { fastStreams(o); o.Stream.MaxPerIP = 1 }))
	srv := startServer(t, e.h)
	p := e.CreatePayment("1")
	url := srv.URL + "/api/v1/public/payments/" + p.ID + "/stream"

	a := openStream(t, url, nil)
	a.next(nil)
	b := openStream(t, url, nil)
	if b.resp.StatusCode != http.StatusTooManyRequests || b.resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second stream: %d", b.resp.StatusCode)
	}
	a.close()
	waitStreams(t, e.h, 0)
	openStream(t, url, nil).next(nil)

	for _, id := range []string{"pay_0000000000000000000000000z", "nope"} {
		resp, err := http.Get(srv.URL + "/api/v1/public/payments/" + id + "/stream")
		if err != nil {
			t.Fatal(err)
		}
		var er errResp
		_ = json.NewDecoder(resp.Body).Decode(&er)
		resp.Body.Close()
		if resp.StatusCode != 404 || er.Error.Code != "not_found" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Fatalf("%s: %d %+v", id, resp.StatusCode, er)
		}
	}
}

func TestStreamShutdownAndLifetime(t *testing.T) {
	t.Run("shutdown", func(t *testing.T) {
		e := newTenv(t, withOpts(fastStreams))
		srv := startServer(t, e.h)
		p := e.CreatePayment("1")
		c := openStream(t, srv.URL+"/api/v1/public/payments/"+p.ID+"/stream", nil)
		c.next(nil)
		e.h.Close()
		c.waitEnd()
		waitStreams(t, e.h, 0)
	})
	t.Run("server shutdown via NewServer", func(t *testing.T) {
		e := newTenv(t, withOpts(fastStreams))
		srv := httptest.NewUnstartedServer(nil)
		hs := e.h.NewServer("", nil)
		if hs.WriteTimeout != 0 || hs.ReadHeaderTimeout == 0 || hs.MaxHeaderBytes == 0 || hs.IdleTimeout == 0 {
			t.Fatalf("server hardening %+v", hs)
		}
		srv.Config = hs
		srv.Start()
		t.Cleanup(srv.Close)
		p := e.CreatePayment("1")
		c := openStream(t, srv.URL+"/api/v1/public/payments/"+p.ID+"/stream", nil)
		c.next(nil)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := hs.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown waited on the stream: %v", err)
		}
		c.waitEnd()
	})
	t.Run("max lifetime", func(t *testing.T) {
		e := newTenv(t, withOpts(func(o *Options) { fastStreams(o); o.Stream.MaxLifetime = 150 * time.Millisecond }))
		srv := startServer(t, e.h)
		p := e.CreatePayment("1")
		c := openStream(t, srv.URL+"/api/v1/public/payments/"+p.ID+"/stream", nil)
		c.next(nil)
		c.waitEnd()
		waitStreams(t, e.h, 0)
	})
	t.Run("survives server ReadTimeout", func(t *testing.T) {
		e := newTenv(t, withOpts(fastStreams))
		srv := httptest.NewUnstartedServer(e.h)
		srv.Config.ReadTimeout = 100 * time.Millisecond
		srv.Start()
		t.Cleanup(srv.Close)
		p := e.CreatePayment("1")
		c := openStream(t, srv.URL+"/api/v1/public/payments/"+p.ID+"/stream", nil)
		c.next(nil)
		time.Sleep(350 * time.Millisecond)
		e.Pay(e.Payment(p.ID), "0.5", 10)
		if _, v := c.next(func(v core.CheckoutView) bool { return v.Status == core.StatusDetected }); v.AmountReceived != "0.50" {
			t.Fatalf("after ReadTimeout %+v", v)
		}
	})
}

func TestStreamPublicCORS(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) { fastStreams(o); o.PublicCORS = true }))
	srv := startServer(t, e.h)
	p := e.CreatePayment("1")
	c := openStream(t, srv.URL+"/api/v1/public/payments/"+p.ID+"/stream", hdr{"Origin": "https://merchant.example"})
	if c.resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("headers %v", c.resp.Header)
	}
	c.next(nil)
}

func TestStreamHead(t *testing.T) {
	e := newTenv(t, withOpts(fastStreams))
	p := e.CreatePayment("1")
	w := e.do(req{method: "HEAD", path: "/api/v1/public/payments/" + p.ID + "/stream"})
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") || e.h.streams.open() != 0 {
		t.Fatalf("HEAD: %d %v", w.Code, w.Header())
	}
}

// A message of the checkout stream is a view and the id it is sent under,
// and the two belong together: the id is the number of events the payment
// has had, and every change of status is one event. The stream used to read
// the event count first and the view afterwards, so a change that committed
// between the two reads went out as the new view under the old id — a
// client that reconnected with that id as its Last-Event-ID was then told
// nothing was new, and the lifecycle tests that expect "paid" to arrive
// under id 3 failed once in a few hundred runs on a busy machine.
//
// Here many payments move through their statuses while their streams are
// polling as fast as they can, and every single message is checked.
func TestStreamIDBelongsToItsView(t *testing.T) {
	e := newTenv(t, withOpts(func(o *Options) {
		fastStreams(o)
		o.Stream.ActivePoll, o.Stream.IdlePoll, o.Stream.Debounce = time.Millisecond, time.Millisecond, time.Millisecond
		o.Stream.MaxPerIP, o.Stream.MaxTotal = 1000, 1000
	}))
	srv := startServer(t, e.h)
	// What a payment of 10 goes through here, and the number of events it
	// has had when it gets there.
	ids := map[core.PaymentStatus]string{
		core.StatusAwaiting: "1", core.StatusDetected: "2", core.StatusConfirming: "3", core.StatusPaid: "4",
	}
	const payments = 24
	e.Head(100)
	var wg sync.WaitGroup
	var mu sync.Mutex // the test environment's helpers are not for concurrent use
	for i := 0; i < payments; i++ {
		mu.Lock()
		p := e.CreatePayment("10")
		mu.Unlock()
		c := openStream(t, srv.URL+"/api/v1/public/payments/"+p.ID+"/stream", nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.After(20 * time.Second)
			for {
				select {
				case ev, ok := <-c.events:
					if !ok {
						t.Errorf("payment %s: the stream ended", p.ID)
						return
					}
					if ev.comment || ev.event != "checkout" {
						continue
					}
					var v core.CheckoutView
					if err := json.Unmarshal([]byte(ev.data), &v); err != nil {
						t.Errorf("bad data %q: %v", ev.data, err)
						return
					}
					if want := ids[v.Status]; ev.id != want {
						t.Errorf("payment %s: a view with status %s was sent under id %s; the payment has had %s events when it is %s",
							p.ID, v.Status, ev.id, want, v.Status)
					}
					if v.Status == core.StatusPaid {
						return
					}
				case <-deadline:
					t.Errorf("payment %s never got to paid on its stream", p.ID)
					return
				}
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, amount := range []string{"4", "6"} {
				time.Sleep(time.Duration(i%7) * 300 * time.Microsecond)
				mu.Lock()
				e.Pay(e.Payment(p.ID), amount, 101)
				mu.Unlock()
			}
		}()
	}
	// The head moves on while the payments come in: each is paid when it
	// has its confirmations.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for block := uint64(101); block < 101+400; block++ {
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			e.Head(block)
			mu.Unlock()
		}
	}()
	wg.Wait()
	<-done
}
