package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// arrival is one request a receiver got.
type arrival struct {
	seq        int64
	typ        core.EventType
	start, end time.Time
}

type recorder struct {
	mu       sync.Mutex
	arrivals []arrival
}

func (r *recorder) list() []arrival {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.arrivals)
}

// lifecycle creates a payment and pays it in full: payment.created,
// payment.confirming and payment.paid, in that order, all at once.
func (h *harness) lifecycle(url string) (core.WebhookEndpoint, []core.Event) {
	h.t.Helper()
	ep, err := h.env.Service.CreateWebhookEndpoint(h.op, core.WebhookEndpointInput{URL: url, Events: core.WebhookEventTypes, Enabled: true})
	if err != nil {
		h.t.Fatal(err)
	}
	p := h.env.CreatePayment("149")
	h.env.Pay(p, "149", 100)
	h.env.Head(114)
	events, err := h.env.Service.Events(h.env.System(), core.EventFilter{PaymentID: p.ID})
	if err != nil {
		h.t.Fatal(err)
	}
	if len(events) < 3 {
		h.t.Fatalf("test setup: %d events", len(events))
	}
	return ep, events
}

// TestFirstAttemptsInEventOrder: the events of one payment reach a healthy
// receiver in the order they happened, each first attempt after the
// previous one has finished, and every body carries the event's seq. (Sent
// side by side, as they were, a slow first event arrived after a later
// one, and the receiver had nothing to tell which was newer.)
func TestFirstAttemptsInEventOrder(t *testing.T) {
	h := newHarness(t)
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		began := time.Now()
		body, _ := io.ReadAll(r.Body)
		var env core.EventEnvelope
		_ = json.Unmarshal(body, &env)
		if env.Type == core.EventPaymentCreated {
			time.Sleep(150 * time.Millisecond) // the first event is the slow one
		} else {
			time.Sleep(20 * time.Millisecond)
		}
		rec.mu.Lock()
		rec.arrivals = append(rec.arrivals, arrival{seq: env.Seq, typ: env.Type, start: began, end: time.Now()})
		rec.mu.Unlock()
	}))
	defer srv.Close()
	ep, events := h.lifecycle(srv.URL)

	start(t, newDispatcher(t, h.env.Service, Options{Concurrency: 8, MaxPerHost: 8}))
	// The receiver records an arrival before it answers, so the last request
	// arriving is not yet the dispatcher having recorded its result: wait for
	// the results too.
	waitFor(t, 10*time.Second, "every event delivered and recorded", func() bool {
		if len(rec.list()) != len(events) {
			return false
		}
		ds := h.deliveries(ep.ID)
		for _, d := range ds {
			if d.Status != core.DeliverySucceeded {
				return false
			}
		}
		return len(ds) == len(events)
	})

	got := rec.list()
	for i, a := range got {
		if a.seq != events[i].Seq || a.typ != events[i].Type {
			t.Fatalf("arrival %d is %s seq %d; want %s seq %d (all: %+v)", i, a.typ, a.seq, events[i].Type, events[i].Seq, got)
		}
		if i > 0 && a.start.Before(got[i-1].end) {
			t.Fatalf("the first attempt of %s started before that of %s had finished", a.typ, got[i-1].typ)
		}
	}
	for _, d := range h.deliveries(ep.ID) {
		if d.Status != core.DeliverySucceeded || d.Attempt != 1 {
			t.Fatalf("delivery: %+v", d)
		}
	}
}

// TestFailedFirstAttemptHoldsNothingUp: a first attempt that fails lets the
// later events go out at once; its retry follows its own schedule.
func TestFailedFirstAttemptHoldsNothingUp(t *testing.T) {
	h := newHarness(t)
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env core.EventEnvelope
		_ = json.Unmarshal(body, &env)
		rec.mu.Lock()
		rec.arrivals = append(rec.arrivals, arrival{seq: env.Seq, typ: env.Type})
		rec.mu.Unlock()
		if env.Type == core.EventPaymentCreated {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	ep, events := h.lifecycle(srv.URL)

	start(t, newDispatcher(t, h.env.Service, Options{}))
	// The core's clock does not move: the failed delivery's retry (a
	// minute later) never falls due here.
	waitFor(t, 10*time.Second, "the later events delivered", func() bool {
		n := 0
		for _, d := range h.deliveries(ep.ID) {
			if d.EventType != core.EventPaymentCreated && d.Status == core.DeliverySucceeded {
				n++
			}
		}
		return n == len(events)-1
	})
	if got := rec.list(); len(got) != len(events) || got[0].typ != core.EventPaymentCreated {
		t.Fatalf("arrivals: %+v", got)
	}
	for _, d := range h.deliveries(ep.ID) {
		if d.EventType == core.EventPaymentCreated && (d.Status != core.DeliveryRetrying || d.Attempt != 1) {
			t.Fatalf("failed delivery: %+v", d)
		}
	}
}
