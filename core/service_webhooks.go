package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"time"
)

// webhookCursor is the event-log consumer name of the webhook fan-out.
const webhookCursor = "core.webhooks"

// WebhookEndpointInput describes a webhook endpoint, for both creation and
// update (updates replace every editable field).
type WebhookEndpointInput struct {
	// URL is the http(s) URL to POST events to. Required. The core checks
	// its syntax, refuses URLs that embed credentials ("user:pass@host"),
	// and — when [Config.ValidateWebhookURL] is set — asks the wiring's
	// egress policy, so every transport rejects the same URLs. The URL is
	// checked on creation and on every update that changes it or leaves
	// the endpoint enabled; an endpoint can always be disabled as is.
	URL string
	// Description is the operator's label (≤ 200 characters).
	Description string
	// Events are the event types to deliver; at least one, each from
	// [WebhookEventTypes].
	Events []EventType
	// Enabled says whether events are delivered.
	Enabled bool
}

func (s *Service) applyEndpointInput(in WebhookEndpointInput, e *WebhookEndpoint, now time.Time, isNew bool) error {
	var v validation
	target := cleanURL(&v, "url", in.URL, true, "http", "https")
	if v.ok() && (isNew || in.Enabled || target != e.URL) {
		if u, err := url.Parse(target); err == nil && u.User != nil {
			v.add("url", "credentials", "url must not contain a user name or password")
		} else if s.validateWebhookURL != nil {
			if err := s.validateWebhookURL(target); err != nil {
				v.add("url", "not_allowed", "%s", errMessage(err))
			}
		}
	}
	e.URL = target
	e.Description = cleanText(&v, "description", in.Description, 200, false)
	events := make([]EventType, 0, len(in.Events))
	for _, t := range in.Events {
		if !t.Subscribable() {
			v.add("events", "invalid", "unknown event type %q", t)
			continue
		}
		if !slices.Contains(events, t) {
			events = append(events, t)
		}
	}
	if len(events) == 0 && v.ok() {
		v.add("events", "required", "subscribe to at least one event")
	}
	e.Events = events
	switch {
	case in.Enabled && !e.Enabled:
		// (Re-)enabling starts with a clean bill of health.
		e.Enabled = true
		e.DisabledAt, e.DisabledReason = time.Time{}, ""
		e.FailingSince, e.ConsecutiveFailures = time.Time{}, 0
	case !in.Enabled && e.Enabled:
		e.Enabled = false
		e.DisabledAt, e.DisabledReason = now, "manual"
	}
	e.UpdatedAt = now
	return v.errOrNil()
}

func getEndpoint(ctx context.Context, r WebhookReader, id string) (WebhookEndpoint, error) {
	e, err := r.GetWebhookEndpoint(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return e, notFound("webhook_endpoint", id)
	}
	return e, err
}

func getDelivery(ctx context.Context, r WebhookReader, id string) (WebhookDelivery, error) {
	d, err := r.GetDelivery(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return d, notFound("webhook_delivery", id)
	}
	return d, err
}

// newWebhookSecret returns a fresh signing secret: "whsec_" plus 32
// characters (160 random bits).
func (s *Service) newWebhookSecret() (string, error) {
	r, err := s.randomString(20)
	if err != nil {
		return "", err
	}
	return "whsec_" + r, nil
}

// CreateWebhookEndpoint registers an endpoint and generates its signing
// secret (returned in the result's Secret). It receives events appended
// from now on. Requires [ScopeWebhooksManage].
func (s *Service) CreateWebhookEndpoint(ctx context.Context, in WebhookEndpointInput) (WebhookEndpoint, error) {
	var e WebhookEndpoint
	if _, err := authorize(ctx, permScope(ScopeWebhooksManage)); err != nil {
		return e, err
	}
	err := s.update(ctx, func(u *unit) error {
		e = WebhookEndpoint{CreatedAt: u.now}
		if err := s.applyEndpointInput(in, &e, u.now, true); err != nil {
			return err
		}
		if !in.Enabled {
			e.DisabledAt, e.DisabledReason = u.now, "manual"
		}
		var err error
		if e.ID, err = s.newID(prefixEndpoint); err != nil {
			return err
		}
		if e.Secret, err = s.newWebhookSecret(); err != nil {
			return err
		}
		return u.InsertWebhookEndpoint(ctx, e)
	})
	return e, err
}

// UpdateWebhookEndpoint replaces an endpoint's URL, description, events
// and enabled flag (the result carries the signing secrets only for the
// operator, see [Service.GetWebhookEndpoint]). Disabling fails its
// undelivered deliveries (they can
// be resent later) and revokes their claims; a request that was already
// on the wire is still recorded when its result arrives (see
// [CompletionLate]). Enabling resets its failure streak. Requires
// [ScopeWebhooksManage].
func (s *Service) UpdateWebhookEndpoint(ctx context.Context, id string, in WebhookEndpointInput) (WebhookEndpoint, error) {
	var e WebhookEndpoint
	actor, err := authorize(ctx, permScope(ScopeWebhooksManage))
	if err != nil {
		return e, err
	}
	err = s.update(ctx, func(u *unit) error {
		var err error
		if e, err = getEndpoint(ctx, u, id); err != nil {
			return err
		}
		wasEnabled := e.Enabled
		if err := s.applyEndpointInput(in, &e, u.now, false); err != nil {
			return err
		}
		if err := u.UpdateWebhookEndpoint(ctx, e); err != nil {
			return err
		}
		if wasEnabled && !e.Enabled {
			return failOutstanding(ctx, u, e.ID)
		}
		return nil
	})
	return withoutSecrets(actor, e), err
}

// failOutstanding fails every unfinished delivery of a disabled endpoint.
func failOutstanding(ctx context.Context, u *unit, endpointID string) error {
	open, err := u.ListDeliveries(ctx, DeliveryFilter{
		EndpointID: endpointID,
		Statuses:   []DeliveryStatus{DeliveryPending, DeliveryRetrying},
	}, Page{})
	if err != nil {
		return err
	}
	for _, d := range open {
		d.Status = DeliveryFailed
		d.Error = "endpoint disabled"
		d.ClaimID = ""
		d.NextAttemptAt = time.Time{}
		d.CompletedAt, d.UpdatedAt = u.now, u.now
		if err := u.UpdateDelivery(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// DeleteWebhookEndpoint removes an endpoint together with its delivery
// history. Requires [ScopeWebhooksManage].
func (s *Service) DeleteWebhookEndpoint(ctx context.Context, id string) error {
	if _, err := authorize(ctx, permScope(ScopeWebhooksManage)); err != nil {
		return err
	}
	return s.update(ctx, func(u *unit) error {
		err := u.DeleteWebhookEndpoint(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return notFound("webhook_endpoint", id)
		}
		return err
	})
}

// RotateWebhookSecret gives an endpoint a new signing secret (returned in
// the result's Secret). For [WebhookSecretOverlap] deliveries are signed
// with both the new and the previous secret, so the receiver can switch
// without dropping events. Operator only: the result is the live secret of
// an endpoint the caller did not necessarily create.
func (s *Service) RotateWebhookSecret(ctx context.Context, id string) (WebhookEndpoint, error) {
	var e WebhookEndpoint
	if _, err := authorize(ctx, permOperator); err != nil {
		return e, err
	}
	err := s.update(ctx, func(u *unit) error {
		var err error
		if e, err = getEndpoint(ctx, u, id); err != nil {
			return err
		}
		e.PreviousSecret = e.Secret
		e.PreviousSecretExpiresAt = u.now.Add(WebhookSecretOverlap)
		if e.Secret, err = s.newWebhookSecret(); err != nil {
			return err
		}
		e.UpdatedAt = u.now
		return u.UpdateWebhookEndpoint(ctx, e)
	})
	return e, err
}

// RevealWebhookSecret returns an endpoint with its signing secrets, for the
// dashboard's explicit "show secret" action. Operator only: whoever knows
// the secret can forge events the merchant's receiver accepts.
func (s *Service) RevealWebhookSecret(ctx context.Context, id string) (WebhookEndpoint, error) {
	var e WebhookEndpoint
	if _, err := authorize(ctx, permOperator); err != nil {
		return e, err
	}
	err := s.store.View(ctx, func(r Reader) error {
		var err error
		e, err = getEndpoint(ctx, r, id)
		return err
	})
	return e, err
}

// withoutSecrets blanks an endpoint's signing secrets unless the caller is
// the operator (or trusted in-process code). An API key holding
// webhooks:manage manages endpoints; it must not be able to read the secret
// of an endpoint — with it, it could sign events the gateway never emitted.
func withoutSecrets(actor Actor, e WebhookEndpoint) WebhookEndpoint {
	if actor.Kind != ActorOperator && actor.Kind != ActorSystem {
		e.Secret, e.PreviousSecret = "", ""
	}
	return e
}

// WebhookEndpointSummary is an endpoint with its recent delivery record.
type WebhookEndpointSummary struct {
	WebhookEndpoint
	// Succeeded7d and Failed7d count deliveries created in the last seven
	// days that succeeded or failed for good. The success rate is
	// Succeeded7d / (Succeeded7d + Failed7d).
	Succeeded7d int
	Failed7d    int
}

func summarize(ctx context.Context, r WebhookReader, e WebhookEndpoint, now time.Time) (WebhookEndpointSummary, error) {
	sum := WebhookEndpointSummary{WebhookEndpoint: e}
	since := now.Add(-7 * 24 * time.Hour)
	var err error
	sum.Succeeded7d, err = r.CountDeliveries(ctx, DeliveryFilter{EndpointID: e.ID, Statuses: []DeliveryStatus{DeliverySucceeded}, CreatedFrom: since})
	if err != nil {
		return sum, err
	}
	sum.Failed7d, err = r.CountDeliveries(ctx, DeliveryFilter{EndpointID: e.ID, Statuses: []DeliveryStatus{DeliveryFailed}, CreatedFrom: since})
	return sum, err
}

// GetWebhookEndpoint returns one endpoint. Requires [ScopeWebhooksManage].
//
// Secret and PreviousSecret are filled in only for the operator and for
// system actors; for an API key they are empty, here and in every other
// method that returns an endpoint — except [Service.CreateWebhookEndpoint],
// whose caller receives the secret of the endpoint it just created, once.
// Transports still decide whether to mask what the operator sees.
func (s *Service) GetWebhookEndpoint(ctx context.Context, id string) (WebhookEndpointSummary, error) {
	var sum WebhookEndpointSummary
	actor, err := authorize(ctx, permScope(ScopeWebhooksManage))
	if err != nil {
		return sum, err
	}
	now := s.now()
	err = s.store.View(ctx, func(r Reader) error {
		e, err := getEndpoint(ctx, r, id)
		if err != nil {
			return err
		}
		sum, err = summarize(ctx, r, e, now)
		sum.WebhookEndpoint = withoutSecrets(actor, sum.WebhookEndpoint)
		return err
	})
	return sum, err
}

// ListWebhookEndpoints returns all endpoints, oldest first, with secrets
// only for the operator (see [Service.GetWebhookEndpoint]). Requires
// [ScopeWebhooksManage].
func (s *Service) ListWebhookEndpoints(ctx context.Context) ([]WebhookEndpointSummary, error) {
	actor, err := authorize(ctx, permScope(ScopeWebhooksManage))
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []WebhookEndpointSummary
	err = s.store.View(ctx, func(r Reader) error {
		endpoints, err := r.ListWebhookEndpoints(ctx)
		if err != nil {
			return err
		}
		out = make([]WebhookEndpointSummary, 0, len(endpoints))
		for _, e := range endpoints {
			sum, err := summarize(ctx, r, e, now)
			if err != nil {
				return err
			}
			sum.WebhookEndpoint = withoutSecrets(actor, sum.WebhookEndpoint)
			out = append(out, sum)
		}
		return nil
	})
	return out, err
}

// DeliveryPage is one page of webhook deliveries.
type DeliveryPage struct {
	Items []WebhookDelivery
	// Total is the number of deliveries matching the filter.
	Total int
}

// ListWebhookDeliveries returns a page of deliveries, newest first.
// filter.DueBefore is internal and ignored; unknown statuses in
// filter.Statuses are a validation error on "status". Limit defaults to 50
// and is capped at 200. Requires [ScopeWebhooksManage].
func (s *Service) ListWebhookDeliveries(ctx context.Context, filter DeliveryFilter, page Page) (DeliveryPage, error) {
	var out DeliveryPage
	if _, err := authorize(ctx, permScope(ScopeWebhooksManage)); err != nil {
		return out, err
	}
	filter.DueBefore = time.Time{}
	for _, st := range filter.Statuses {
		if !st.Valid() {
			return out, invalidField("status", "invalid", "unknown delivery status %q; use pending, retrying, succeeded or failed", st)
		}
	}
	page.Limit = clampLimit(page.Limit, 50, 200)
	if page.Offset < 0 {
		return out, invalidField("offset", "invalid", "offset must not be negative")
	}
	err := s.store.View(ctx, func(r Reader) error {
		var err error
		if out.Total, err = r.CountDeliveries(ctx, filter); err != nil {
			return err
		}
		out.Items, err = r.ListDeliveries(ctx, filter, page)
		return err
	})
	return out, err
}

// GetWebhookDelivery returns one delivery with its attempt history.
// Requires [ScopeWebhooksManage].
func (s *Service) GetWebhookDelivery(ctx context.Context, id string) (WebhookDelivery, error) {
	var d WebhookDelivery
	if _, err := authorize(ctx, permScope(ScopeWebhooksManage)); err != nil {
		return d, err
	}
	err := s.store.View(ctx, func(r Reader) error {
		var err error
		d, err = getDelivery(ctx, r, id)
		return err
	})
	return d, err
}

// ResendWebhookDelivery queues a delivery for one more immediate attempt,
// whatever its current status. The event id stays the same, so receivers
// can deduplicate. If the delivery had used up its attempts it gets
// exactly one more; otherwise its normal schedule continues after a
// failure. Fails with [ErrEndpointDisabled] while the endpoint is
// disabled. Requires [ScopeWebhooksManage].
func (s *Service) ResendWebhookDelivery(ctx context.Context, id string) (WebhookDelivery, error) {
	var d WebhookDelivery
	if _, err := authorize(ctx, permScope(ScopeWebhooksManage)); err != nil {
		return d, err
	}
	err := s.update(ctx, func(u *unit) error {
		var err error
		if d, err = getDelivery(ctx, u, id); err != nil {
			return err
		}
		e, err := getEndpoint(ctx, u, d.EndpointID)
		if err != nil {
			return err
		}
		if !e.Enabled {
			return newError(ErrEndpointDisabled, "")
		}
		d.Status = DeliveryPending
		d.NextAttemptAt = u.now
		d.ClaimID = "" // supersedes a claim that is still out
		d.CompletedAt = time.Time{}
		d.MaxAttempts = max(d.MaxAttempts, d.Attempt+1)
		d.UpdatedAt = u.now
		if err := u.UpdateDelivery(ctx, d); err != nil {
			return err
		}
		u.emitted++ // wake the dispatcher
		return nil
	})
	return d, err
}

// SendTestWebhook queues a synthetic "webhook.test" event for one endpoint
// (a single attempt, no retries) and returns the delivery, whose outcome
// can be read with [Service.GetWebhookDelivery]. The test event is not
// part of the event log. Requires [ScopeWebhooksManage].
func (s *Service) SendTestWebhook(ctx context.Context, endpointID string) (WebhookDelivery, error) {
	var d WebhookDelivery
	if _, err := authorize(ctx, permScope(ScopeWebhooksManage)); err != nil {
		return d, err
	}
	err := s.update(ctx, func(u *unit) error {
		e, err := getEndpoint(ctx, u, endpointID)
		if err != nil {
			return err
		}
		if !e.Enabled {
			return newError(ErrEndpointDisabled, "")
		}
		eventID, err := s.newID(prefixEvent)
		if err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]string{"message": "This is a test event from hmnpay."})
		event := Event{ID: eventID, Type: EventWebhookTest, CreatedAt: u.now, Data: data}
		if d, err = s.newDelivery(u, e, event); err != nil {
			return err
		}
		d.MaxAttempts = 1
		if err := u.InsertDelivery(ctx, d); err != nil {
			return err
		}
		u.emitted++ // wake the dispatcher
		return nil
	})
	return d, err
}

// newDelivery builds a pending delivery of event to endpoint, due now.
func (s *Service) newDelivery(u *unit, e WebhookEndpoint, event Event) (WebhookDelivery, error) {
	body, err := json.Marshal(event.Envelope())
	if err != nil {
		return WebhookDelivery{}, internal("encode webhook body", err)
	}
	id, err := s.newID(prefixDelivery)
	if err != nil {
		return WebhookDelivery{}, err
	}
	return WebhookDelivery{
		ID:            id,
		EndpointID:    e.ID,
		EventID:       event.ID,
		EventSeq:      event.Seq,
		EventType:     event.Type,
		PaymentID:     event.PaymentID,
		URL:           e.URL,
		Status:        DeliveryPending,
		MaxAttempts:   WebhookMaxAttempts,
		NextAttemptAt: u.now,
		RequestBody:   string(body),
		CreatedAt:     u.now,
		UpdatedAt:     u.now,
	}, nil
}

// ---- dispatcher workflow (system only) ----

// maxAttemptHistory bounds [WebhookDelivery.Attempts], so that neither a
// misbehaving dispatcher nor endless resends can grow a delivery without
// limit: late results beyond it are still honoured but no longer listed,
// and counted attempts beyond it push out the oldest entries.
const maxAttemptHistory = 64

// planBatch is how many events one fan-out transaction handles.
const planBatch = 100

// PlanWebhookDeliveries turns new events into deliveries: it reads the
// event log from the webhook cursor, creates one pending delivery per
// enabled endpoint subscribed to each event, and advances the cursor — all
// in one transaction per batch, so every event is fanned out exactly once.
// It returns the number of deliveries created.
//
// The webhook dispatcher calls it whenever its [Subscription] fires and on
// a slow timer, before [Service.ClaimWebhookDeliveries]. An endpoint gets
// the events that are fanned out while it is enabled and subscribed, and
// never an event older than the endpoint itself. System only.
func (s *Service) PlanWebhookDeliveries(ctx context.Context) (int, error) {
	if _, err := authorize(ctx, permSystem); err != nil {
		return 0, err
	}
	total := 0
	for {
		more := false
		err := s.update(ctx, func(u *unit) error {
			cursor, err := u.GetCursor(ctx, webhookCursor)
			if err != nil {
				return err
			}
			events, err := u.ListEvents(ctx, EventFilter{After: cursor, Limit: planBatch})
			if err != nil || len(events) == 0 {
				return err
			}
			endpoints, err := u.ListWebhookEndpoints(ctx)
			if err != nil {
				return err
			}
			for _, event := range events {
				if !event.Type.Subscribable() {
					continue
				}
				for _, e := range endpoints {
					// CreatedAt guards against delivering history to an
					// endpoint registered after the event.
					if !e.Enabled || !slices.Contains(e.Events, event.Type) || event.CreatedAt.Before(e.CreatedAt) {
						continue
					}
					d, err := s.newDelivery(u, e, event)
					if err != nil {
						return err
					}
					if err := u.InsertDelivery(ctx, d); err != nil {
						return err
					}
					total++
				}
			}
			more = len(events) == planBatch
			return u.SetCursor(ctx, webhookCursor, events[len(events)-1].Seq)
		})
		if err != nil {
			return total, err
		}
		if !more {
			return total, nil
		}
	}
}

// ClaimWebhookDeliveries returns up to limit deliveries that are due and
// claims them for [WebhookClaimLease]: a claimed delivery is not returned
// by another claim until the lease runs out, so several dispatcher workers
// can run side by side and a crashed worker's deliveries are picked up
// again. Each job is fully prepared — URL, body and signed headers — and
// must be sent right away, then reported with
// [Service.CompleteWebhookDelivery]; a job that will not be sent is given
// back with [Service.ReleaseWebhookDelivery].
//
// Every claim gets a fresh [DeliveryJob.ClaimID], the fencing token: a
// delivery has at most one current claim, and claiming it again (after
// the lease ran out) makes the earlier claim stale. limit defaults to 20,
// maximum 100. System only.
//
// First attempts go out in event order. A delivery's first attempt is
// claimed only when every earlier event's delivery to the same endpoint has
// had its first attempt — has been attempted once (successfully or not)
// or is done — so an endpoint receives the first attempt of an event only
// after the first attempt of every earlier event has finished: one first
// attempt per endpoint is out at a time. Retries are not ordered: a first
// attempt that failed holds nothing up, and its retries follow their own
// schedule beside the later events. Neither is ordering applied while an
// endpoint is failing (its last attempt failed): its events then go out as
// they fall due, so that a receiver that times out does not turn into a
// queue that grows by every event. Receivers order by the envelope's seq
// in any case ([EventEnvelope]); the ordering here is what keeps a healthy
// receiver from having to. The synthetic test event is never held up.
func (s *Service) ClaimWebhookDeliveries(ctx context.Context, limit int) ([]DeliveryJob, error) {
	if _, err := authorize(ctx, permSystem); err != nil {
		return nil, err
	}
	limit = clampLimit(limit, 20, 100)
	var jobs []DeliveryJob
	err := s.update(ctx, func(u *unit) error {
		jobs = nil
		due, err := dueDeliveries(ctx, u, limit)
		if err != nil {
			return err
		}
		endpoints := map[string]WebhookEndpoint{}
		for _, d := range due {
			e, ok := endpoints[d.EndpointID]
			if !ok {
				if e, err = u.GetWebhookEndpoint(ctx, d.EndpointID); err != nil && !errors.Is(err, ErrNotFound) {
					return err
				}
				endpoints[d.EndpointID] = e
			}
			d.UpdatedAt = u.now
			if !e.Enabled {
				d.Status = DeliveryFailed
				d.Error = "endpoint disabled"
				d.ClaimID = ""
				d.NextAttemptAt = time.Time{}
				d.CompletedAt = u.now
				if err := u.UpdateDelivery(ctx, d); err != nil {
					return err
				}
				continue
			}
			if d.ClaimID, err = s.newID(prefixClaim); err != nil {
				return err
			}
			d.NextAttemptAt = u.now.Add(WebhookClaimLease)
			if err := u.UpdateDelivery(ctx, d); err != nil {
				return err
			}
			body := []byte(d.RequestBody)
			jobs = append(jobs, DeliveryJob{
				DeliveryID:     d.ID,
				EndpointID:     e.ID,
				ClaimID:        d.ClaimID,
				Attempt:        d.Attempt + 1,
				LeaseExpiresAt: d.NextAttemptAt,
				URL:            e.URL,
				Headers: map[string]string{
					"Content-Type":   "application/json",
					HeaderSignature:  WebhookSignatureHeader(u.now, body, e.SigningSecrets(u.now)...),
					HeaderEventID:    d.EventID,
					HeaderDeliveryID: d.ID,
				},
				Body:    body,
				Timeout: WebhookTimeout,
			})
		}
		return nil
	})
	return jobs, err
}

// dueDeliveries returns up to limit deliveries that may be attempted now, in
// the order they fell due: every due retry, resend and test event, and of
// the first attempts only those that are not waiting for an earlier
// event's (see [Service.ClaimWebhookDeliveries]).
func dueDeliveries(ctx context.Context, u *unit, limit int) ([]WebhookDelivery, error) {
	open := []DeliveryStatus{DeliveryPending, DeliveryRetrying}
	unattempted, attempted := true, false
	due, err := u.ListDeliveries(ctx, DeliveryFilter{Statuses: open, DueBefore: u.now, Unattempted: &attempted}, Page{Limit: limit})
	if err != nil {
		return nil, err
	}
	endpoints, err := u.ListWebhookEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range endpoints {
		if e.Enabled && e.ConsecutiveFailures == 0 {
			// In order: the endpoint's earliest unattempted event, if it
			// is due — not claimed by someone else right now.
			head, err := u.ListDeliveries(ctx, DeliveryFilter{EndpointID: e.ID, Statuses: open, Unattempted: &unattempted}, Page{Limit: 1})
			if err != nil {
				return nil, err
			}
			if len(head) == 1 && !head[0].NextAttemptAt.IsZero() && !head[0].NextAttemptAt.After(u.now) {
				due = append(due, head[0])
			}
			continue
		}
		// Failing, or disabled (what is due is failed by the claim):
		// whatever is due.
		more, err := u.ListDeliveries(ctx, DeliveryFilter{EndpointID: e.ID, Statuses: open, DueBefore: u.now, Unattempted: &unattempted}, Page{Limit: limit})
		if err != nil {
			return nil, err
		}
		due = append(due, more...)
	}
	slices.SortFunc(due, func(a, b WebhookDelivery) int {
		return cmp.Or(a.NextAttemptAt.Compare(b.NextAttemptAt), cmp.Compare(a.ID, b.ID))
	})
	if len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

// ReleaseWebhookDelivery gives a claim back without an attempt having been
// made — the dispatcher is shutting down, or it has no free slot for the
// endpoint's host. Nothing is recorded: the attempt count, the schedule
// and the endpoint's health are untouched, and the delivery becomes due
// again after delay (0 for at once; at most [WebhookClaimLease]) instead
// of sitting out the rest of its lease.
//
// The release is fenced like a completion: it only takes effect if claimID
// is still the delivery's current claim. released reports whether it did;
// a stale release is a harmless no-op, not an error. Do not release a job
// whose request was sent — complete it. System only.
func (s *Service) ReleaseWebhookDelivery(ctx context.Context, deliveryID, claimID string, delay time.Duration) (released bool, err error) {
	if _, err := authorize(ctx, permSystem); err != nil {
		return false, err
	}
	if claimID == "" {
		return false, invalidField("claim_id", "required", "claim id is required")
	}
	delay = min(max(delay, 0), WebhookClaimLease)
	err = s.update(ctx, func(u *unit) error {
		released = false
		d, err := getDelivery(ctx, u, deliveryID)
		if err != nil {
			return err
		}
		if d.ClaimID != claimID || d.Status.Done() {
			return nil
		}
		d.ClaimID = ""
		d.NextAttemptAt = u.now.Add(delay)
		d.UpdatedAt = u.now
		if err := u.UpdateDelivery(ctx, d); err != nil {
			return err
		}
		released = true
		if delay == 0 {
			u.emitted++ // due now: wake dispatchers
		}
		return nil
	})
	return released, err
}

// CompleteWebhookDelivery records the outcome of one attempt. Any 2xx
// response is a success; everything else — other statuses, timeouts,
// transport errors — is a failure. res.ClaimID and res.Attempt identify
// the claim the attempt was made under (see [DeliveryJob.Result]).
//
// If that claim is still the delivery's current one, the attempt is
// counted and the delivery policy applies ([CompletionCounted]): after a
// failure the delivery is retried after [WebhookBackoff] until
// [WebhookMaxAttempts] attempts have been made, then it fails for good. An
// endpoint whose attempts have all failed for [WebhookAutoDisableAfter] is
// disabled: its unfinished deliveries fail and webhook_endpoint.disabled
// is appended to the event log. Any success resets the endpoint's failure
// streak. A claim stays current after its lease ran out for as long as
// nobody else has claimed the delivery, so a slow dispatcher still gets
// its result counted.
//
// If the claim is stale ([CompletionLate]) — the delivery was claimed
// again, released, resent, or failed because its endpoint was disabled —
// the result cannot be double-counted and cannot overwrite a newer one:
// the attempt is only appended to the history, marked
// [DeliveryAttempt.Late]. The one exception keeps the record truthful: a
// late 2xx means the receiver has the event, so the delivery becomes
// succeeded (cancelling further retries and any newer claim) even if it
// had been failed as "endpoint disabled". A late result never changes an
// endpoint's failure streak, except that a late success on an enabled
// endpoint ends it.
//
// It returns an error matching [ErrNotFound] if the delivery no longer
// exists (its endpoint was deleted). System only.
func (s *Service) CompleteWebhookDelivery(ctx context.Context, deliveryID string, res AttemptResult) (CompletionOutcome, error) {
	if _, err := authorize(ctx, permSystem); err != nil {
		return "", err
	}
	var v validation
	if res.ClaimID == "" {
		v.add("claim_id", "required", "claim id is required")
	}
	if res.Attempt < 1 {
		v.add("attempt", "required", "attempt number is required")
	}
	if err := v.errOrNil(); err != nil {
		return "", err
	}
	ok := res.ResponseCode >= 200 && res.ResponseCode < 300
	if len(res.ResponseBody) > WebhookMaxResponseBody {
		res.ResponseBody = res.ResponseBody[:WebhookMaxResponseBody]
	}
	if len(res.Error) > 500 {
		res.Error = res.Error[:500]
	}
	durationMs := int(res.Duration / time.Millisecond)

	var outcome CompletionOutcome
	err := s.update(ctx, func(u *unit) error {
		d, err := getDelivery(ctx, u, deliveryID)
		if err != nil {
			return err
		}
		attempt := DeliveryAttempt{
			N: res.Attempt, At: u.now, ResponseCode: res.ResponseCode,
			DurationMs: durationMs, Error: res.Error, Succeeded: ok,
		}
		summarize := func() {
			d.ResponseCode, d.ResponseBody = res.ResponseCode, res.ResponseBody
			d.DurationMs, d.Error = durationMs, res.Error
		}
		d.UpdatedAt = u.now

		if d.ClaimID != res.ClaimID || d.Status.Done() {
			outcome = CompletionLate
			attempt.Late = true
			if len(d.Attempts) < maxAttemptHistory {
				d.Attempts = append(d.Attempts, attempt)
			}
			delivered := ok && d.Status != DeliverySucceeded
			if delivered {
				// The receiver has the event, whatever happened to the
				// claim since: say so, and stop sending.
				d.Status, d.NextAttemptAt, d.CompletedAt = DeliverySucceeded, time.Time{}, u.now
				d.ClaimID = ""
				d.Attempt = max(d.Attempt, res.Attempt)
				summarize()
			}
			if err := u.UpdateDelivery(ctx, d); err != nil {
				return err
			}
			if !delivered {
				return nil
			}
			e, err := u.GetWebhookEndpoint(ctx, d.EndpointID)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			e.LastDeliveryAt, e.LastSuccessAt, e.UpdatedAt = u.now, u.now, u.now
			if e.Enabled {
				e.FailingSince, e.ConsecutiveFailures = time.Time{}, 0
			}
			return u.UpdateWebhookEndpoint(ctx, e)
		}

		outcome = CompletionCounted
		d.ClaimID = ""
		d.Attempt++
		attempt.N = d.Attempt
		// A delivery that is resent over and over keeps its newest
		// maxAttemptHistory entries.
		if n := len(d.Attempts); n >= maxAttemptHistory {
			d.Attempts = slices.Delete(d.Attempts, 0, n-maxAttemptHistory+1)
		}
		d.Attempts = append(d.Attempts, attempt)
		summarize()
		wait, retry := WebhookBackoff(d.Attempt)
		switch {
		case ok:
			d.Status, d.NextAttemptAt, d.CompletedAt = DeliverySucceeded, time.Time{}, u.now
		case d.Attempt >= d.MaxAttempts || !retry:
			d.Status, d.NextAttemptAt, d.CompletedAt = DeliveryFailed, time.Time{}, u.now
		default:
			d.Status, d.NextAttemptAt = DeliveryRetrying, u.now.Add(wait)
		}
		if err := u.UpdateDelivery(ctx, d); err != nil {
			return err
		}

		e, err := u.GetWebhookEndpoint(ctx, d.EndpointID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		e.LastDeliveryAt, e.UpdatedAt = u.now, u.now
		if ok {
			e.LastSuccessAt = u.now
			e.FailingSince, e.ConsecutiveFailures = time.Time{}, 0
			return u.UpdateWebhookEndpoint(ctx, e)
		}
		e.ConsecutiveFailures++
		if e.FailingSince.IsZero() {
			e.FailingSince = u.now
		}
		disable := e.Enabled && u.now.Sub(e.FailingSince) >= WebhookAutoDisableAfter
		if disable {
			e.Enabled = false
			e.DisabledAt, e.DisabledReason = u.now, "failing"
		}
		if err := u.UpdateWebhookEndpoint(ctx, e); err != nil {
			return err
		}
		if !disable {
			return nil
		}
		if err := failOutstanding(ctx, u, e.ID); err != nil {
			return err
		}
		return u.emit(ctx, EventWebhookEndpointDisabled, "", EndpointDisabledData{
			EndpointID: e.ID, URL: e.URL, Reason: e.DisabledReason, FailingSince: e.FailingSince,
		})
	})
	if err != nil {
		return "", err
	}
	return outcome, nil
}
