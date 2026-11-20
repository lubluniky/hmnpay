package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Webhook delivery policy. These values are part of the product contract
// and are shown to operators.
const (
	// WebhookMaxAttempts is the number of automatic attempts per delivery.
	WebhookMaxAttempts = 8
	// WebhookTimeout is how long the dispatcher waits for an endpoint.
	WebhookTimeout = 10 * time.Second
	// WebhookAutoDisableAfter: an endpoint whose every attempt failed for
	// this long is disabled automatically.
	WebhookAutoDisableAfter = 72 * time.Hour
	// WebhookRetention is how long deliveries are kept (and resendable).
	WebhookRetention = 30 * 24 * time.Hour
	// WebhookSecretOverlap is how long the previous signing secret still
	// signs deliveries after a rotation.
	WebhookSecretOverlap = 24 * time.Hour
	// WebhookSignatureTolerance is the default maximum age of a signature
	// accepted by [VerifyWebhookSignature].
	WebhookSignatureTolerance = 5 * time.Minute
	// WebhookClaimLease is how long a claimed delivery is hidden from other
	// claims; a dispatcher that dies gets its deliveries back after it.
	WebhookClaimLease = time.Minute
	// WebhookMaxResponseBody is how many bytes of an endpoint's response
	// are kept; the dispatcher should not read more.
	WebhookMaxResponseBody = 4096
)

// Webhook request headers.
const (
	// HeaderSignature carries "t=<unix seconds>,v1=<hex>[,v1=<hex>]".
	HeaderSignature = "Hmnpay-Signature"
	// HeaderEventID carries the event id (stable across retries).
	HeaderEventID = "Hmnpay-Event-Id"
	// HeaderDeliveryID carries the delivery id.
	HeaderDeliveryID = "Hmnpay-Delivery-Id"
)

// webhookBackoff is the wait before attempt n+1 after attempt n failed.
var webhookBackoff = [WebhookMaxAttempts - 1]time.Duration{
	1 * time.Minute, 5 * time.Minute, 15 * time.Minute, 60 * time.Minute,
	180 * time.Minute, 360 * time.Minute, 720 * time.Minute,
}

// WebhookBackoff returns how long to wait before the next attempt after
// `attempts` attempts have failed (1 minute after the first, then 5, 15,
// 60, 180, 360 and 720 minutes). ok is false when no automatic attempt is
// left, i.e. attempts >= [WebhookMaxAttempts].
func WebhookBackoff(attempts int) (wait time.Duration, ok bool) {
	if attempts < 1 {
		return 0, true
	}
	if attempts > len(webhookBackoff) {
		return 0, false
	}
	return webhookBackoff[attempts-1], true
}

// SignWebhook returns the hex-encoded HMAC-SHA256 of "<unix seconds>.<body>"
// under secret: the v1 signature scheme.
func SignWebhook(secret string, t time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(t.Unix(), 10)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// WebhookSignatureHeader builds the value of the Hmnpay-Signature header:
// one v1 signature per secret, so receivers can verify with either the
// current or the previous secret during a rotation.
func WebhookSignatureHeader(t time.Time, body []byte, secrets ...string) string {
	var b strings.Builder
	b.WriteString("t=")
	b.WriteString(strconv.FormatInt(t.Unix(), 10))
	for _, s := range secrets {
		b.WriteString(",v1=")
		b.WriteString(SignWebhook(s, t, body))
	}
	return b.String()
}

// VerifyWebhookSignature checks a Hmnpay-Signature header against the raw
// request body. It succeeds when any v1 signature in the header matches any
// of the secrets and the timestamp is within tolerance of now (in either
// direction). A tolerance of 0 uses [WebhookSignatureTolerance]; a negative
// tolerance disables the age check.
//
// An empty list of secrets, or an empty secret in it, is refused with
// [ErrNoWebhookSecret] before anything is compared: a receiver that loads
// its secret from an unset environment variable must fail loudly, not
// accept signatures made with the empty key (which anyone can compute).
//
// Receivers written in Go can use it directly; it also documents the scheme
// for every other language. Comparison is constant-time.
func VerifyWebhookSignature(header string, body []byte, secrets []string, now time.Time, tolerance time.Duration) error {
	if len(secrets) == 0 || slices.Contains(secrets, "") {
		return newError(ErrNoWebhookSecret, "")
	}
	var ts int64
	var haveTS bool
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return newError(ErrUnauthorized, "malformed signature timestamp")
			}
			ts, haveTS = n, true
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if !haveTS || len(sigs) == 0 {
		return newError(ErrUnauthorized, "malformed signature header")
	}
	t := time.Unix(ts, 0)
	if tolerance == 0 {
		tolerance = WebhookSignatureTolerance
	}
	if tolerance > 0 {
		if age := now.Sub(t); age > tolerance || age < -tolerance {
			return newError(ErrUnauthorized, "signature timestamp outside tolerance")
		}
	}
	match := false
	for _, secret := range secrets {
		want := []byte(SignWebhook(secret, t, body))
		for _, got := range sigs {
			if hmac.Equal(want, []byte(got)) {
				match = true
			}
		}
	}
	if !match {
		return newError(ErrUnauthorized, "signature mismatch")
	}
	return nil
}

// WebhookEndpoint is a merchant URL that receives events.
type WebhookEndpoint struct {
	// ID is "we_" plus 26 random characters.
	ID string
	// URL is the http(s) URL events are POSTed to.
	URL string
	// Description is the operator's label.
	Description string
	// Events lists the subscribed event types (see [WebhookEventTypes]).
	Events []EventType
	// Enabled says whether new events are delivered.
	Enabled bool

	// Secret is the current signing secret ("whsec_…"). It has to be kept
	// in a recoverable form because HMAC needs the secret itself.
	Secret string
	// PreviousSecret also signs deliveries until PreviousSecretExpiresAt;
	// both are empty/zero outside a rotation.
	PreviousSecret          string
	PreviousSecretExpiresAt time.Time

	// DisabledAt and DisabledReason ("manual" or "failing") describe the
	// last time the endpoint was switched off.
	DisabledAt     time.Time
	DisabledReason string
	// FailingSince is the time of the first failed attempt since the last
	// success; zero while healthy.
	FailingSince time.Time
	// ConsecutiveFailures counts failed attempts since the last success.
	ConsecutiveFailures int
	// LastDeliveryAt and LastSuccessAt are the times of the latest attempt
	// and the latest successful attempt.
	LastDeliveryAt time.Time
	LastSuccessAt  time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// SigningSecrets returns the secrets that sign a delivery at time now: the
// current one and, during the rotation overlap, the previous one.
func (e WebhookEndpoint) SigningSecrets(now time.Time) []string {
	secrets := []string{e.Secret}
	if e.PreviousSecret != "" && now.Before(e.PreviousSecretExpiresAt) {
		secrets = append(secrets, e.PreviousSecret)
	}
	return secrets
}

// DisableAt returns when a failing endpoint will be disabled automatically
// if nothing succeeds; zero while healthy.
func (e WebhookEndpoint) DisableAt() time.Time {
	if e.FailingSince.IsZero() {
		return time.Time{}
	}
	return e.FailingSince.Add(WebhookAutoDisableAfter)
}

// DeliveryStatus is the state of a [WebhookDelivery].
type DeliveryStatus string

// Delivery statuses.
const (
	// DeliveryPending: not attempted yet (or queued again by a resend).
	DeliveryPending DeliveryStatus = "pending"
	// DeliveryRetrying: at least one attempt failed; another is scheduled.
	DeliveryRetrying DeliveryStatus = "retrying"
	// DeliverySucceeded: the endpoint answered 2xx.
	DeliverySucceeded DeliveryStatus = "succeeded"
	// DeliveryFailed: all attempts failed, or the endpoint was disabled.
	DeliveryFailed DeliveryStatus = "failed"
)

// Done reports whether no further attempt is scheduled.
func (s DeliveryStatus) Done() bool { return s == DeliverySucceeded || s == DeliveryFailed }

// Valid reports whether s is a known delivery status.
func (s DeliveryStatus) Valid() bool {
	switch s {
	case DeliveryPending, DeliveryRetrying, DeliverySucceeded, DeliveryFailed:
		return true
	}
	return false
}

// DeliveryAttempt records one HTTP attempt of a delivery.
type DeliveryAttempt struct {
	// N is the attempt number, starting at 1.
	N int
	// At is when the result was recorded.
	At time.Time
	// ResponseCode is the HTTP status, or 0 if the endpoint never answered.
	ResponseCode int
	// DurationMs is how long the attempt took.
	DurationMs int
	// Error describes a transport failure (timeout, DNS, TLS, …); empty
	// when the endpoint answered.
	Error string
	// Succeeded reports a 2xx answer.
	Succeeded bool
	// Late marks an attempt whose result was reported after its claim had
	// been lost (the lease ran out and the delivery was claimed again, or
	// the delivery was released, resent, or failed because its endpoint was
	// disabled). The request was really made, so it is kept in the
	// history, but it is not counted in [WebhookDelivery.Attempt] and did
	// not move the retry schedule. N is the number of the attempt the
	// claim was for, so a late entry can share its N with a counted one.
	Late bool
}

// WebhookDelivery is one event addressed to one endpoint, with its attempt
// history.
type WebhookDelivery struct {
	// ID is "whd_" plus 26 random characters.
	ID         string
	EndpointID string
	// EventID is the id of the delivered event; resends keep it.
	EventID string
	// EventSeq is the event's position in the event log ([Event.Seq]), 0
	// for the synthetic test event. First attempts to one endpoint are
	// made in this order (see [Service.ClaimWebhookDeliveries]).
	EventSeq  int64
	EventType EventType
	// PaymentID is the payment the event is about, if any.
	PaymentID string
	// URL is the endpoint URL at the time the delivery was created.
	URL    string
	Status DeliveryStatus
	// Attempt is the number of counted attempts made so far: attempts
	// whose result was reported under a valid claim. It drives the retry
	// schedule. Late attempts (see [DeliveryAttempt.Late]) are not counted,
	// except that a late success raises it to that attempt's number.
	Attempt int
	// MaxAttempts is the attempt after which the delivery fails for good.
	MaxAttempts int
	// NextAttemptAt is when the delivery is due; zero once done. While the
	// delivery is claimed it is the end of the claim's lease.
	NextAttemptAt time.Time
	// ClaimID identifies the claim currently held on the delivery (see
	// [Service.ClaimWebhookDeliveries]); empty when nobody holds one. It is
	// the fencing token: only a result that presents the current ClaimID
	// is counted.
	ClaimID string
	// RequestBody is the exact JSON body that is POSTed (an
	// [EventEnvelope]).
	RequestBody string
	// ResponseCode, ResponseBody, DurationMs and Error describe the latest
	// attempt. ResponseBody is truncated to [WebhookMaxResponseBody].
	ResponseCode int
	ResponseBody string
	DurationMs   int
	Error        string
	// Attempts is the full attempt history, oldest first.
	Attempts []DeliveryAttempt

	CreatedAt time.Time
	UpdatedAt time.Time
	// CompletedAt is when the delivery succeeded or failed for good.
	CompletedAt time.Time
}

// DeliveryFilter selects webhook deliveries. All set fields must match.
//
// Ordering: when DueBefore is set, results are ordered by NextAttemptAt
// ascending (then ID); otherwise, when Unattempted is true, by EventSeq
// ascending (then ID); otherwise newest first (CreatedAt descending, then
// ID descending).
type DeliveryFilter struct {
	EndpointID string
	PaymentID  string
	EventID    string
	// Statuses, when non-empty, matches any of the listed statuses.
	Statuses []DeliveryStatus
	// DueBefore, when non-zero, matches deliveries whose NextAttemptAt is
	// non-zero and not after it.
	DueBefore time.Time
	// CreatedFrom, when non-zero, matches CreatedAt >= CreatedFrom.
	CreatedFrom time.Time
	// Unattempted, when set, matches — if true — the deliveries of a
	// logged event (EventSeq > 0) that have no counted attempt yet
	// (Attempt == 0), or — if false — every other delivery. With true
	// and no DueBefore, results are ordered by EventSeq ascending (then
	// ID): the order first attempts are owed in.
	Unattempted *bool
}

// DeliveryJob is one claimed delivery, ready to be sent: POST Body to URL
// with Headers, wait at most Timeout, then report the outcome with
// [Service.CompleteWebhookDelivery] — or, if the request is never made,
// give the claim back with [Service.ReleaseWebhookDelivery].
type DeliveryJob struct {
	DeliveryID string
	EndpointID string
	// ClaimID is the fencing token of this claim. Pass it back in
	// [AttemptResult.ClaimID] (or to ReleaseWebhookDelivery).
	ClaimID string
	// Attempt is the number of the attempt being made (1-based). Pass it
	// back in [AttemptResult.Attempt].
	Attempt int
	// LeaseExpiresAt is when the core may hand the delivery to another
	// claim, by the core's clock.
	LeaseExpiresAt time.Time
	URL            string
	// Headers are the request headers the core owns: Content-Type, the
	// signature, the event id and the delivery id. The dispatcher adds its
	// own User-Agent. The signature is computed at claim time, so the job
	// must be sent promptly (well inside [WebhookSignatureTolerance]).
	Headers map[string]string
	Body    []byte
	Timeout time.Duration
}

// Result returns an [AttemptResult] addressed to this job's claim, for the
// dispatcher to fill in.
func (j DeliveryJob) Result() AttemptResult {
	return AttemptResult{ClaimID: j.ClaimID, Attempt: j.Attempt}
}

// CompletionOutcome says how [Service.CompleteWebhookDelivery] treated a
// result.
type CompletionOutcome string

// Completion outcomes.
const (
	// CompletionCounted: the result was reported under the delivery's
	// current claim. The attempt was counted and the delivery policy
	// applied (success, retry schedule, endpoint health).
	CompletionCounted CompletionOutcome = "counted"
	// CompletionLate: the claim was no longer current — the lease had run
	// out and someone else claimed the delivery, or it was released,
	// resent or failed with its endpoint in the meantime. The attempt is
	// kept in the history marked Late and changes neither the attempt
	// count nor the schedule nor a newer result; only a 2xx still marks
	// the delivery succeeded, because the receiver did get the event.
	CompletionLate CompletionOutcome = "late"
)

// AttemptResult is what the dispatcher observed when it sent a job.
type AttemptResult struct {
	// ClaimID is the [DeliveryJob.ClaimID] the attempt was made under.
	// Required.
	ClaimID string
	// Attempt is the [DeliveryJob.Attempt] the claim was for. Required.
	Attempt int
	// ResponseCode is the HTTP status, or 0 if no response was received.
	ResponseCode int
	// ResponseBody is the beginning of the response body (it is truncated
	// to [WebhookMaxResponseBody]).
	ResponseBody string
	// Duration is how long the attempt took.
	Duration time.Duration
	// Error describes a transport failure; leave it empty when a response
	// was received.
	Error string
}
