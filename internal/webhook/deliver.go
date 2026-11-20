package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// Stable, non-sensitive values of [core.AttemptResult.Error].
const (
	ResultTimeout            = "timeout"
	ResultDNSError           = "dns_error"
	ResultConnectionRefused  = "connection_refused"
	ResultConnectionReset    = "connection_reset"
	ResultNetworkUnreachable = "network_unreachable"
	ResultTLSError           = "tls_error"
	ResultBlockedByPolicy    = "blocked_by_policy"
	ResultInvalidURL         = "invalid_url"
	ResultInvalidRequest     = "invalid_request"
	ResultProtocolError      = "protocol_error"
	ResultConnectionError    = "connection_error"
	ResultAborted            = "aborted"
	ResultInternalError      = "internal_error"
	errResponseBodyLimit     = core.WebhookMaxResponseBody
	drainLimit               = 16 << 10
)

// deliver runs one attempt and records it. It always reports t on done,
// whatever happens.
func (d *Dispatcher) deliver(parent context.Context, t *task, done chan<- *task, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() { done <- t }()
	res := d.attempt(parent, t)
	d.complete(t, res)
}

// attempt sends the job once. A panic anywhere in the HTTP path becomes an
// "internal_error" result instead of a lost delivery.
func (d *Dispatcher) attempt(parent context.Context, t *task) (res core.AttemptResult) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("webhook delivery panicked", "delivery_id", t.job.DeliveryID, "panic", r)
			res = core.AttemptResult{Error: ResultInternalError}
		}
		res.Duration = time.Since(start)
	}()

	u, _, err := d.policy.parseURL(t.job.URL)
	if err != nil {
		cat := ResultInvalidURL
		if errors.Is(err, ErrBlockedDestination) {
			cat = ResultBlockedByPolicy
		}
		d.log.Warn("webhook endpoint URL refused", "delivery_id", t.job.DeliveryID, "endpoint_id", t.job.EndpointID, "reason", err.Error())
		return core.AttemptResult{Error: cat}
	}

	ctx, cancel := context.WithTimeout(parent, t.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(t.job.Body))
	if err != nil {
		return core.AttemptResult{Error: ResultInvalidURL}
	}
	// The core supplies the headers it owns (content type, signature, event
	// and delivery ids); the dispatcher identifies itself. The User-Agent
	// is not signed.
	for k, v := range t.job.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", d.userAgent)

	resp, err := d.client.Do(req)
	if err != nil {
		res = core.AttemptResult{Error: d.classify(parent, ctx, err)}
		d.log.Info("webhook attempt failed", "delivery_id", t.job.DeliveryID, "endpoint_id", t.job.EndpointID,
			"host", u.Host, "attempt", t.job.Attempt, "error", res.Error, "cause", causeOf(err))
		return res
	}
	defer resp.Body.Close()

	// Keep at most WebhookMaxResponseBody bytes; a read error after the
	// status arrived does not change the outcome — the endpoint answered.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errResponseBodyLimit))
	// Drain a little more so the connection can be reused, but never let
	// an endless or trickling body hold the worker: a short timer cuts the
	// drain by cancelling the request.
	stop := time.AfterFunc(d.drainTimeout, cancel)
	_, _ = io.CopyN(io.Discard, resp.Body, drainLimit)
	stop.Stop()

	res = core.AttemptResult{ResponseCode: resp.StatusCode, ResponseBody: string(body)}
	d.log.Debug("webhook attempt answered", "delivery_id", t.job.DeliveryID, "endpoint_id", t.job.EndpointID,
		"host", u.Host, "attempt", t.job.Attempt, "status", resp.StatusCode)
	return res
}

// classify maps a transport error to a stable result string. It never
// returns raw error text, which may contain the URL (and any token in its
// query string).
func (d *Dispatcher) classify(parent, attemptCtx context.Context, err error) string {
	var pe *PolicyError
	if errors.As(err, &pe) {
		if errors.Is(pe, ErrBlockedDestination) {
			return ResultBlockedByPolicy
		}
		return ResultInvalidURL
	}
	if parent.Err() != nil {
		return ResultAborted
	}
	if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return ResultTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsTimeout {
			return ResultTimeout
		}
		return ResultDNSError
	}
	var (
		certErr    *tls.CertificateVerificationError
		recErr     tls.RecordHeaderError
		alertErr   tls.AlertError
		unknownCA  x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		invalidErr x509.CertificateInvalidError
	)
	switch {
	case errors.As(err, &certErr), errors.As(err, &recErr), errors.As(err, &alertErr),
		errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &invalidErr):
		return ResultTLSError
	case errors.Is(err, syscall.ECONNREFUSED):
		return ResultConnectionRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return ResultConnectionReset
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return ResultNetworkUnreachable
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ResultTimeout
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "tls: "), strings.Contains(msg, "x509: "):
		return ResultTLSError
	case strings.Contains(msg, "invalid header"), strings.Contains(msg, "net/http: invalid"):
		return ResultInvalidRequest
	case strings.Contains(msg, "malformed HTTP"), strings.Contains(msg, "server response headers exceeded"),
		strings.Contains(msg, "http2:"):
		return ResultProtocolError
	}
	return ResultConnectionError
}

// causeOf is a log-only description of err without the request URL
// (which may carry a token in its query string).
func causeOf(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// complete records res for t under the claim the attempt was made with,
// retrying transient core errors for about as long as the lease lasts. It
// is called exactly once per started attempt.
//
// The claim is the fence: if this dispatcher took longer than its lease
// and the delivery was claimed again (or it was released, resent, or failed
// with its endpoint meanwhile), the core does not count the attempt and
// does not let it overwrite a newer result; it records it as late — and
// still marks the delivery succeeded if the endpoint answered 2xx.
func (d *Dispatcher) complete(t *task, res core.AttemptResult) {
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("recording webhook attempt panicked; the delivery returns after its lease", "delivery_id", t.job.DeliveryID, "panic", r)
		}
	}()
	deadline := t.leaseEnd(d.lease)
	if time.Until(deadline) < time.Second {
		deadline = time.Now().Add(time.Second)
	}
	ctx, cancel := context.WithDeadline(core.WithActor(context.Background(), core.SystemActor(actorName)), deadline)
	defer cancel()
	res.ClaimID, res.Attempt = t.job.ClaimID, t.job.Attempt
	wait := d.completeWait
	for try := 1; ; try++ {
		outcome, err := d.svc.CompleteWebhookDelivery(ctx, t.job.DeliveryID, res)
		if err == nil {
			if outcome == core.CompletionLate {
				d.log.Warn("webhook attempt finished after its claim was lost; recorded as late, not counted",
					"delivery_id", t.job.DeliveryID, "endpoint_id", t.job.EndpointID, "attempt", t.job.Attempt,
					"status", res.ResponseCode, "took", time.Since(t.claimedAt).Round(time.Millisecond))
			}
			return
		}
		if errors.Is(err, core.ErrNotFound) {
			// The endpoint (and its history) was deleted meanwhile.
			d.log.Info("webhook delivery vanished before its result was recorded", "delivery_id", t.job.DeliveryID)
			return
		}
		if try >= d.completeTries || ctx.Err() != nil {
			d.log.Error("recording webhook attempt failed; the delivery will be retried after its lease",
				"delivery_id", t.job.DeliveryID, "error", err, "tries", try)
			return
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
		wait *= 2
	}
}
