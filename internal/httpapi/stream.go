package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// StreamOptions tunes the public checkout event stream. Zero fields take
// the defaults documented on each field.
type StreamOptions struct {
	// Heartbeat is the interval of keep-alive comments. Default 15s.
	Heartbeat time.Duration
	// ActivePoll is how often the checkout is re-read while the payment is
	// detected or confirming (confirmation counts change without events).
	// Default 1.5s.
	ActivePoll time.Duration
	// IdlePoll is the slow safety poll otherwise (the nudge from the core
	// is an optimisation, not a guarantee). Default 15s.
	IdlePoll time.Duration
	// Debounce coalesces bursts of nudges into one re-read. Default 200ms.
	Debounce time.Duration
	// MaxLifetime ends a stream after this long; EventSource reconnects
	// with Last-Event-ID. Default 30m.
	MaxLifetime time.Duration
	// WriteTimeout bounds each write to the client, so a stalled client
	// cannot pin the handler. Default 10s.
	WriteTimeout time.Duration
	// MaxPerIP and MaxTotal cap concurrent streams. Defaults 8 and 1000.
	MaxPerIP int
	MaxTotal int
	// MaxPerPayment caps concurrent streams of one payment, whatever
	// addresses they come from. A checkout is watched by its payer — a few
	// tabs and devices at most. Default 20.
	MaxPerPayment int
	// MaxPerIPPerPayment caps the streams one client address may hold on
	// one payment, so that filling a payment's MaxPerPayment slots — which
	// would leave its payer without live updates — takes several addresses
	// rather than two or three (7 with the defaults). Default 3. A
	// checkout page that is refused a stream (429 too_many_streams) must
	// fall back to polling GET /api/v1/public/payments/{id}.
	MaxPerIPPerPayment int
}

func (o StreamOptions) withDefaults() StreamOptions {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.Heartbeat, 15*time.Second)
	def(&o.ActivePoll, 1500*time.Millisecond)
	def(&o.IdlePoll, 15*time.Second)
	def(&o.Debounce, 200*time.Millisecond)
	def(&o.MaxLifetime, 30*time.Minute)
	def(&o.WriteTimeout, 10*time.Second)
	if o.MaxPerIP <= 0 {
		o.MaxPerIP = 8
	}
	if o.MaxTotal <= 0 {
		o.MaxTotal = 1000
	}
	if o.MaxPerPayment <= 0 {
		o.MaxPerPayment = 20
	}
	if o.MaxPerIPPerPayment <= 0 {
		o.MaxPerIPPerPayment = 3
	}
	return o
}

// checkoutStream serves GET /api/v1/public/payments/{id}/stream as
// server-sent events. Each message is
//
//	id: <number of events this payment has had>
//	event: checkout
//	data: <CheckoutView JSON>
//
// The id counts the payment's own events (1 for a new payment). It is not
// the instance-wide event sequence, which would tell every payer how much
// business the merchant does.
//
// It never forwards raw events (they carry merchant-private fields): the
// event log only tells it *that* something changed and supplies the
// cursor; the payload is always a fresh public CheckoutView. A snapshot is
// sent on connect — unless Last-Event-ID equals the payment's current event
// count, in which case the client is already current — and again whenever
// the view changes. While the payment is detected or confirming the view
// is re-read every ActivePoll, because confirmation counts change without
// events. Identical consecutive views are not re-sent.
//
// Streams are capped in total, per client address, per payment and per
// address per payment ([StreamOptions]); beyond a cap the answer is 429
// too_many_streams with Retry-After. The stream is a convenience, never the
// only way to learn a payment's state: a checkout page that is refused one
// — its payer may be behind an address that holds several, or somebody may
// be holding this payment's slots from many addresses — must fall back to
// polling GET /api/v1/public/payments/{id} and may retry the stream later.
func (h *Handler) checkoutStream(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if !validPaymentID(id) {
		return errPublicNotFound
	}
	ctx := r.Context()
	opts := h.opts.Stream
	// Subscribe before the first read so no nudge can fall between them.
	// The subscription is per payment: a stream wakes only for events of
	// its own payment, not for every event in the system.
	sub := h.svc.SubscribePayment(id)
	defer sub.Close()

	view, err := h.svc.GetCheckout(ctx, id)
	if err != nil {
		return asPublic(err)
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return nil
	}
	// Only streams for real payments take a slot.
	release, ok := h.streams.acquire(h.rateKey(r), id)
	if !ok {
		return &apiError{Status: http.StatusTooManyRequests, Code: "too_many_streams", Message: "too many open streams; retry later", RetryAfter: 5 * time.Second}
	}
	defer release()
	// cursor is the position in the instance's event log (internal);
	// count is the number of this payment's events, which is what the
	// client sees as the id.
	var cursor, count int64
	// snapshot reads the payment's public view together with the id it is
	// sent under ([Handler.checkoutAt]); cursor and count move with it.
	snapshot := func() (core.CheckoutView, error) {
		v, seq, n, err := h.checkoutAt(ctx, id, cursor, count)
		cursor, count = seq, n
		return v, err
	}
	if view, err = snapshot(); err != nil {
		return asPublic(err)
	}

	rc := http.NewResponseController(w)
	// Clear any read deadline left from ReadTimeout so it cannot cancel a
	// long-lived stream. Current net/http already does this when it starts
	// its background read; this keeps the guarantee explicit.
	_ = rc.SetReadDeadline(time.Time{})
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// last and lastSeq are what the client has: the view and the id it was
	// sent under.
	var last []byte
	lastSeq := int64(-1)
	write := func(s string) error {
		_ = rc.SetWriteDeadline(time.Now().Add(opts.WriteTimeout))
		if _, err := fmt.Fprint(w, s); err != nil {
			return err
		}
		return rc.Flush()
	}
	send := func(v core.CheckoutView, seq int64) error {
		b, err := json.Marshal(checkoutDTO{CheckoutView: v, Version: seq})
		if err != nil {
			return err
		}
		// Skip only when neither the view nor its id moved. (A view and its
		// id are read so that they belong together — snapshot — but an id
		// can still move under an unchanged view, when an event changes
		// nothing the public view shows; the client's Last-Event-ID must
		// not stay behind then.)
		if bytes.Equal(b, last) && seq == lastSeq {
			return nil
		}
		last, lastSeq = b, seq
		return write("id: " + strconv.FormatInt(seq, 10) + "\nevent: checkout\ndata: " + string(b) + "\n\n")
	}

	if err := write("retry: 3000\n\n"); err != nil {
		return nil
	}
	if resume, ok := lastEventID(r); !ok || resume != count {
		if err := send(view, count); err != nil {
			return nil
		}
	} else {
		// The client is current; remember what it has so polls only send
		// real changes.
		if last, err = json.Marshal(checkoutDTO{CheckoutView: view, Version: count}); err != nil {
			return nil
		}
		lastSeq = count
	}

	pollEvery := func(st core.PaymentStatus) time.Duration {
		if st == core.StatusDetected || st == core.StatusConfirming {
			return opts.ActivePoll
		}
		return opts.IdlePoll
	}
	heartbeat := time.NewTicker(opts.Heartbeat)
	defer heartbeat.Stop()
	poll := time.NewTimer(pollEvery(view.Status))
	defer poll.Stop()
	lifetime := time.NewTimer(opts.MaxLifetime)
	defer lifetime.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	pending := false

	refresh := func() error {
		v, err := snapshot()
		if err != nil {
			return err
		}
		view = v
		return send(v, count)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-h.done:
			return nil
		case <-lifetime.C:
			return nil
		case <-heartbeat.C:
			if err := write(": ping\n\n"); err != nil {
				return nil
			}
		case <-sub.C():
			if !pending {
				pending = true
				debounce.Reset(opts.Debounce)
			}
		case <-debounce.C:
			pending = false
			if err := refresh(); err != nil {
				h.streamEnded(r, err)
				return nil
			}
		case <-poll.C:
			if err := refresh(); err != nil {
				h.streamEnded(r, err)
				return nil
			}
			poll.Reset(pollEvery(view.Status))
		}
	}
}

// streamEnded logs why a stream stopped early (write errors to a gone
// client are expected and not logged).
func (h *Handler) streamEnded(r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	h.log.LogAttrs(r.Context(), slog.LevelWarn, "checkout stream ended", slog.String("request_id", info(r).id), slog.String("error", err.Error()))
}

// checkoutAt reads a payment's public view together with the number of
// events the payment has had, so that the two belong together: a view is
// never paired with a count that leaves out the event that produced it,
// and a count never includes an event the view does not show yet. The
// count is the view's "version" and the stream's event id.
//
// cursor and count are where an earlier call left off (zero for a first
// read): cursor is a position in the instance's event log (internal),
// count the number of this payment's events up to it.
//
// The view and the events come from two reads, and a change can commit
// between them. Every change of a payment appends an event in the same
// transaction, so the two are known to match when the view was read
// between two readings of the event log that found the same number of
// events: the view is read first, then the log; if the log has grown
// since it was last read, the view may be older than the event that was
// just counted — or the previous count older than the view — and both are
// read again. (A payment has a handful of events in its life; the loop
// ends after one more round. The bound is for a store that never stops
// changing.)
func (h *Handler) checkoutAt(ctx context.Context, id string, cursor, count int64) (core.CheckoutView, int64, int64, error) {
	sys := systemContext(ctx)
	for round := 0; ; round++ {
		v, err := h.svc.GetCheckout(ctx, id)
		if err != nil {
			return v, cursor, count, err
		}
		seq, added, err := h.paymentEvents(sys, id, cursor)
		if err != nil {
			return v, cursor, count, err
		}
		cursor, count = seq, count+added
		if added == 0 || round == 8 {
			return v, cursor, count, nil
		}
	}
}

// paymentEvents reads the payment's events after the log position `after`
// and returns the new position and how many events there were.
func (h *Handler) paymentEvents(ctx context.Context, paymentID string, after int64) (seq int64, n int64, err error) {
	seq = after
	for {
		evs, err := h.svc.Events(ctx, core.EventFilter{PaymentID: paymentID, After: seq, Limit: 1000})
		if err != nil {
			return seq, n, err
		}
		n += int64(len(evs))
		if len(evs) > 0 {
			seq = evs[len(evs)-1].Seq
		}
		if len(evs) < 1000 {
			return seq, n, nil
		}
	}
}

// lastEventID parses the Last-Event-ID header (or the lastEventId query
// parameter some EventSource polyfills use).
func lastEventID(r *http.Request) (int64, bool) {
	s := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if s == "" {
		s = strings.TrimSpace(r.URL.Query().Get("lastEventId"))
	}
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
