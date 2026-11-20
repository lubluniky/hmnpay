package core

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"
)

// EventType names a domain event.
type EventType string

// Event types.
//
// The "payment.*" events carry a [PaymentView] as data and can be subscribed
// to by webhook endpoints. "payment.<status>" is emitted on every status
// change; payment.created announces a new payment (in status awaiting).
const (
	EventPaymentCreated EventType = "payment.created"
	// EventPaymentAwaiting: an open payment fell back to awaiting because
	// every transfer seen so far was reorganised out.
	EventPaymentAwaiting   EventType = "payment.awaiting"
	EventPaymentDetected   EventType = "payment.detected"
	EventPaymentConfirming EventType = "payment.confirming"
	EventPaymentPaid       EventType = "payment.paid"
	EventPaymentUnderpaid  EventType = "payment.underpaid"
	EventPaymentOverpaid   EventType = "payment.overpaid"
	EventPaymentExpired    EventType = "payment.expired"
	EventPaymentFailed     EventType = "payment.failed"
	// EventPaymentLateFunds: funds were confirmed on a payment that had
	// already settled. The status does not change.
	EventPaymentLateFunds EventType = "payment.late_funds"
	// EventPaymentResolved / EventPaymentReopened: an operator closed or
	// reopened the payment's issue.
	EventPaymentResolved EventType = "payment.resolved"
	EventPaymentReopened EventType = "payment.reopened"

	// EventSettingsUpdated: instance settings changed. Data is
	// [SettingsUpdatedData]. Long-running adapters (the chain watcher) use
	// it to reload configuration.
	EventSettingsUpdated EventType = "settings.updated"
	// EventWebhookEndpointDisabled: an endpoint was disabled automatically
	// after failing continuously. Data is [EndpointDisabledData].
	EventWebhookEndpointDisabled EventType = "webhook_endpoint.disabled"
	// EventWebhookTest is the type of the synthetic event sent by
	// [Service.SendTestWebhook]. It never appears in the event log.
	EventWebhookTest EventType = "webhook.test"
)

// WebhookEventTypes lists the event types a webhook endpoint may subscribe
// to, in display order.
var WebhookEventTypes = []EventType{
	EventPaymentCreated, EventPaymentDetected, EventPaymentConfirming,
	EventPaymentPaid, EventPaymentUnderpaid, EventPaymentOverpaid,
	EventPaymentExpired, EventPaymentFailed,
	EventPaymentAwaiting, EventPaymentLateFunds,
	EventPaymentResolved, EventPaymentReopened,
}

// Subscribable reports whether webhook endpoints can subscribe to t.
func (t EventType) Subscribable() bool { return slices.Contains(WebhookEventTypes, t) }

// IsPayment reports whether t is a "payment.*" event.
func (t EventType) IsPayment() bool { return strings.HasPrefix(string(t), "payment.") }

// statusEvent returns the event announcing a status.
func statusEvent(s PaymentStatus) EventType { return EventType("payment." + string(s)) }

// Event is one entry of the durable, ordered event log. Events are appended
// in the same store transaction as the state change they describe
// (transactional outbox) and are never modified.
type Event struct {
	// Seq is the event's position in the log, assigned by the store on
	// append. It is strictly increasing in commit order and is the cursor
	// consumers resume from. Gaps are allowed.
	Seq int64
	// ID is "evt_" plus 26 random characters. It is stable across webhook
	// retries and resends, so receivers can deduplicate on it.
	ID string
	// Type is the event type.
	Type EventType
	// PaymentID is set for "payment.*" events and empty otherwise.
	PaymentID string
	// CreatedAt is when the event was appended.
	CreatedAt time.Time
	// Data is the event's JSON payload: a [PaymentView] snapshot taken at
	// the moment of the event for "payment.*" events.
	Data json.RawMessage
}

// EventEnvelope is the wire form of an event: the body of a webhook request
// and the natural shape for any other transport.
//
//	{"id":"evt_…","type":"payment.paid","created_at":"…","data":{…},"seq":1234}
//
// Seq is the event's position in the event log ([Event.Seq]; the "seq" of
// GET /api/v1/events): it increases with every event of the instance, in
// the order the changes happened. Deliveries can arrive out of that order —
// a retry of an older event after a newer one, two dispatchers — so a
// receiver orders the events of one payment by seq and ignores an event
// whose seq is below the highest it has already applied for that payment
// (the payment snapshot in data is then stale). Retries and resends carry
// the seq of their event unchanged. Seq is omitted only from the synthetic
// webhook.test event, which is not in the log.
type EventEnvelope struct {
	ID        string          `json:"id"`
	Type      EventType       `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
	Seq       int64           `json:"seq,omitempty"`
}

// Envelope returns the wire form of the event.
func (e Event) Envelope() EventEnvelope {
	data := e.Data
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	return EventEnvelope{ID: e.ID, Type: e.Type, CreatedAt: e.CreatedAt, Data: data, Seq: e.Seq}
}

// Payment decodes the data of a "payment.*" event.
func (e Event) Payment() (PaymentView, error) {
	var v PaymentView
	if !e.Type.IsPayment() {
		return v, newError(ErrInvalid, "event %s carries no payment", e.Type)
	}
	err := json.Unmarshal(e.Data, &v)
	return v, err
}

// SettingsUpdatedData is the payload of [EventSettingsUpdated].
type SettingsUpdatedData struct {
	// Section is "setup", "merchant", "payments", "network" or "wallet".
	Section string `json:"section"`
}

// EndpointDisabledData is the payload of [EventWebhookEndpointDisabled].
type EndpointDisabledData struct {
	EndpointID string `json:"endpoint_id"`
	URL        string `json:"url"`
	// Reason is the endpoint's DisabledReason ("failing").
	Reason string `json:"reason"`
	// FailingSince is the start of the failure streak.
	FailingSince time.Time `json:"failing_since"`
}

// EventFilter selects events from the log. Results are always ordered by
// ascending Seq.
type EventFilter struct {
	// After returns only events with Seq > After. Use the Seq of the last
	// event you processed; 0 starts from the beginning.
	After int64
	// PaymentID, when non-empty, returns only that payment's events.
	PaymentID string
	// Types, when non-empty, returns only events of those types.
	Types []EventType
	// Limit caps the number of events; 0 means no limit.
	Limit int
}

// Subscription is a low-latency, in-process nudge that new events were
// appended to the log — all events ([Service.Subscribe]) or those of one
// payment ([Service.SubscribePayment]). It carries no data and may
// coalesce any number of appends into one signal; it can also fire
// spuriously. It is an optimisation on top of the cursor, never a
// replacement: consumers must re-read the log from their cursor on every
// signal and should also poll on a slow timer.
type Subscription struct {
	c         chan struct{}
	hub       *hub
	paymentID string // "" subscribes to everything
}

// C returns the channel that receives a value after events were appended.
func (s *Subscription) C() <-chan struct{} { return s.c }

// Close stops the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.paymentID == "" {
		delete(h.all, s)
		return
	}
	if subs := h.byPayment[s.paymentID]; subs != nil {
		delete(subs, s)
		if len(subs) == 0 {
			delete(h.byPayment, s.paymentID)
		}
	}
}

// hub fans wake-ups out to subscriptions.
type hub struct {
	mu        sync.Mutex
	all       map[*Subscription]struct{}
	byPayment map[string]map[*Subscription]struct{}
}

func (h *hub) subscribe(paymentID string) *Subscription {
	s := &Subscription{c: make(chan struct{}, 1), hub: h, paymentID: paymentID}
	h.mu.Lock()
	defer h.mu.Unlock()
	if paymentID == "" {
		if h.all == nil {
			h.all = make(map[*Subscription]struct{})
		}
		h.all[s] = struct{}{}
		return s
	}
	if h.byPayment == nil {
		h.byPayment = make(map[string]map[*Subscription]struct{})
	}
	subs := h.byPayment[paymentID]
	if subs == nil {
		subs = make(map[*Subscription]struct{})
		h.byPayment[paymentID] = subs
	}
	subs[s] = struct{}{}
	return s
}

// wake signals every global subscription and the subscriptions of the
// given payments. The cost is proportional to the subscriptions that are
// actually signalled, not to all open ones.
func (h *hub) wake(paymentIDs []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	signal := func(s *Subscription) {
		select {
		case s.c <- struct{}{}:
		default:
		}
	}
	for s := range h.all {
		signal(s)
	}
	for _, id := range paymentIDs {
		for s := range h.byPayment[id] {
			signal(s)
		}
	}
}
