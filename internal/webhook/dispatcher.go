// Package webhook is the webhook delivery worker: the adapter that POSTs
// the core's signed webhook deliveries to merchant endpoints.
//
// The core owns every rule — which events go to which endpoint, the
// payload and its signature, the retry schedule, the claim (its lease and
// its fencing token) and automatic disabling
// ([core.Service.PlanWebhookDeliveries],
// [core.Service.ClaimWebhookDeliveries],
// [core.Service.CompleteWebhookDelivery],
// [core.Service.ReleaseWebhookDelivery]). This package owns the HTTP
// egress loop and its safety: bounded concurrency, timeouts, response
// limits and the [EgressPolicy] that keeps webhooks out of the gateway's
// own network.
//
// Wiring:
//
//	d, err := webhook.New(svc, webhook.Options{Logger: log})
//	if err != nil { … }
//	go func() { errc <- d.Run(ctx) }() // returns after ctx is cancelled
//	                                    // and in-flight deliveries drained
//
// Several dispatchers (processes or goroutines) may run against the same
// store: the core's claim lease keeps them from sending the same delivery
// twice within a lease, and every result is reported under the claim it
// was made with, so a dispatcher that outlives its lease cannot
// double-count an attempt or overwrite a newer result — the core records
// such a result as late. [core.Service.Prune] is not called here; wiring
// runs it on its own timer.
//
// A webhook endpoint URL is checked twice: statically when the endpoint is
// saved — wire [ValidateURL] (or [EgressPolicy.ValidateURL]) into
// core.Config.ValidateWebhookURL so every transport rejects the same URLs
// — and at dial time on every connection, which is the actual boundary.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// Version is reported in the User-Agent ("hmnpay-webhook/<Version>"). The
// build may set it with -ldflags "-X …/internal/webhook.Version=1.2.3".
var Version = "dev"

// actorName is the system actor all core calls are made as.
const actorName = "webhook-dispatcher"

// Defaults of [Options].
const (
	DefaultConcurrency   = 8
	DefaultMaxPerHost    = 4
	DefaultPollInterval  = 5 * time.Second
	DefaultShutdownGrace = core.WebhookTimeout + 5*time.Second
)

// Options configure a [Dispatcher]. The zero value is usable.
type Options struct {
	// Concurrency is the number of deliveries sent at the same time.
	// Default [DefaultConcurrency].
	Concurrency int
	// MaxPerHost caps concurrent deliveries to one endpoint host
	// (host:port of the URL) so that a slow or dead receiver cannot occupy
	// every worker. Default [DefaultMaxPerHost], at most Concurrency.
	MaxPerHost int
	// PollInterval is the fallback timer for due retries and missed
	// nudges. Default [DefaultPollInterval].
	PollInterval time.Duration
	// BatchSize is the most deliveries claimed at once. Claims never ask
	// for more than there are free workers. Default Concurrency, at most
	// 100.
	BatchSize int
	// ShutdownGrace is how long Run waits, after its context is cancelled,
	// for in-flight deliveries to finish before aborting them. Default
	// [DefaultShutdownGrace].
	ShutdownGrace time.Duration
	// Logger receives operational logs. It never receives request headers
	// (they carry the signature), bodies or full URLs. Default
	// slog.Default().
	Logger *slog.Logger
	// Egress is the destination policy. The zero value is the strict
	// default; see [EgressPolicy].
	Egress EgressPolicy
	// Transport replaces the HTTP transport. The dial-time egress guard
	// lives in the default transport, so a replacement bypasses it (only
	// the URL-level checks of [EgressPolicy.ValidateURL] still apply).
	// Meant for tests; production wiring should leave it nil.
	Transport http.RoundTripper
	// UserAgent overrides "hmnpay-webhook/<Version>".
	UserAgent string
}

// Dispatcher delivers webhook deliveries; create it with [New] and run it
// with [Dispatcher.Run].
type Dispatcher struct {
	svc       *core.Service
	log       *slog.Logger
	policy    EgressPolicy
	client    *http.Client
	transport http.RoundTripper
	userAgent string

	concurrency   int
	maxPerHost    int
	maxPending    int
	pollInterval  time.Duration
	batchSize     int
	shutdownGrace time.Duration

	// Tunables kept unexported; tests shrink them.
	lease          time.Duration // the core's claim lease
	leaseMargin    time.Duration // time reserved after the attempt to complete it
	requeueDelay   time.Duration // when a job released for a busy host is due again
	requeueBudget  int           // busy-host releases allowed per requeueDelay
	releaseTimeout time.Duration // bound of one release call
	maxTimeout     time.Duration // upper bound of one attempt
	drainTimeout   time.Duration // how long to drain a response for connection reuse
	backoffMin     time.Duration
	backoffMax     time.Duration
	completeTries  int
	completeWait   time.Duration
	finalWait      time.Duration // after aborting, how long Run waits for workers

	running atomic.Bool
}

// New returns a dispatcher for svc.
func New(svc *core.Service, opts Options) (*Dispatcher, error) {
	if svc == nil {
		return nil, errors.New("webhook: nil service")
	}
	for _, p := range opts.Egress.AllowedPrefixes {
		if !p.IsValid() {
			return nil, fmt.Errorf("webhook: invalid allowed prefix %v", p)
		}
	}
	d := &Dispatcher{
		svc:           svc,
		log:           opts.Logger,
		policy:        opts.Egress,
		transport:     opts.Transport,
		userAgent:     opts.UserAgent,
		concurrency:   opts.Concurrency,
		maxPerHost:    opts.MaxPerHost,
		pollInterval:  opts.PollInterval,
		batchSize:     opts.BatchSize,
		shutdownGrace: opts.ShutdownGrace,

		lease:          core.WebhookClaimLease,
		leaseMargin:    10 * time.Second,
		requeueDelay:   core.WebhookTimeout,
		releaseTimeout: 2 * time.Second,
		maxTimeout:     30 * time.Second,
		drainTimeout:   250 * time.Millisecond,
		backoffMin:     time.Second,
		backoffMax:     time.Minute,
		completeTries:  4,
		completeWait:   250 * time.Millisecond,
		finalWait:      10 * time.Second,
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	d.log = d.log.With("component", actorName)
	if d.userAgent == "" {
		d.userAgent = "hmnpay-webhook/" + Version
	}
	if d.concurrency <= 0 {
		d.concurrency = DefaultConcurrency
	}
	if d.maxPerHost <= 0 {
		d.maxPerHost = DefaultMaxPerHost
	}
	d.maxPerHost = min(d.maxPerHost, d.concurrency)
	d.maxPending = d.concurrency
	d.requeueBudget = 2 * d.concurrency
	if d.pollInterval <= 0 {
		d.pollInterval = DefaultPollInterval
	}
	if d.batchSize <= 0 {
		d.batchSize = d.concurrency
	}
	d.batchSize = min(d.batchSize, 100)
	if d.shutdownGrace <= 0 {
		d.shutdownGrace = DefaultShutdownGrace
	}
	if d.transport == nil {
		d.transport = newTransport(d.policy, nil)
	}
	d.client = &http.Client{
		Transport: d.transport,
		// A redirect is a failed attempt: report the 3xx, never follow it
		// (following would also escape the URL that was validated).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return d, nil
}

// task is one claimed delivery inside the dispatcher.
type task struct {
	job       core.DeliveryJob
	host      string    // per-host key: host:port of the URL ("" if unparsable)
	claimedAt time.Time // local time just before the claim call
	startBy   time.Time // the attempt must start before this to finish inside the lease
	timeout   time.Duration
}

// leaseEnd is the earliest the core may hand the delivery to someone else.
func (t *task) leaseEnd(lease time.Duration) time.Time { return t.claimedAt.Add(lease) }

// Run delivers webhooks until ctx is cancelled, then stops claiming,
// gives in-flight deliveries up to ShutdownGrace to finish, aborts the
// rest and waits for their results to be recorded.
//
// Wake-ups: the core's [core.Subscription] nudge (new events, manual
// resends, test webhooks), the poll timer (due retries), and a worker
// finishing — which may have made the next event's first attempt to its
// endpoint claimable: the core hands out first attempts to one endpoint in
// event order, one at a time (see [core.Service.ClaimWebhookDeliveries]). Errors from the core back
// off exponentially (1 s doubling to 1 min, with jitter); nothing spins.
//
// Every delivery whose HTTP attempt starts is completed exactly once with
// [core.Service.CompleteWebhookDelivery], under the claim it was made with
// — including after a panic in the HTTP path (Error "internal_error") and
// when shutdown aborts it (Error "aborted").
//
// A claimed delivery whose attempt never started is not completed — no
// attempt was made, so none is recorded — but given back with
// [core.Service.ReleaseWebhookDelivery] instead of sitting out its 60 s
// lease:
//
//   - claimed as shutdown began, or still parked when Run stops: released
//     at once, so another dispatcher (or this process after a restart)
//     sends it without delay;
//   - parked behind the per-host cap until it could no longer finish inside
//     its lease, or not even parked because too much is waiting already:
//     released to be due again after one attempt timeout (10 s), when a
//     slot on that host has had the chance to free up.
//
// The second kind is rate-limited (twice Concurrency per 10 s). A host that
// is saturated with a large backlog would otherwise turn into one store
// write per queued delivery every 10 s; beyond the budget such a delivery
// is simply left to its lease, which costs nothing and still lets claims
// move on to other hosts. A release that fails is likewise left to the
// lease.
//
// Run returns nil after a clean shutdown. It may be called again after it
// returned, but not concurrently.
func (d *Dispatcher) Run(ctx context.Context) error {
	if !d.running.CompareAndSwap(false, true) {
		return errors.New("webhook: dispatcher is already running")
	}
	defer d.running.Store(false)
	if ct, ok := d.transport.(interface{ CloseIdleConnections() }); ok {
		defer ct.CloseIdleConnections()
	}

	sub := d.svc.Subscribe()
	defer sub.Close()

	sysCtx := core.WithActor(ctx, core.SystemActor(actorName))
	// Attempts outlive ctx by up to the grace period; abort cancels them.
	attemptCtx, abort := context.WithCancel(core.WithActor(context.WithoutCancel(ctx), core.SystemActor(actorName)))
	defer abort()

	s := &scheduler{
		d:        d,
		perHost:  map[string]int{},
		done:     make(chan *task, d.concurrency),
		ctx:      attemptCtx,
		abort:    abort,
		wantWork: true,
		planDue:  true,
	}
	d.log.Info("webhook dispatcher started", "concurrency", d.concurrency, "max_per_host", d.maxPerHost,
		"poll_interval", d.pollInterval, "allow_private_networks", d.policy.AllowPrivateNetworks)

	timer := time.NewTimer(0)
	defer timer.Stop()
	nextPoll := time.Now().Add(d.pollInterval)

	for ctx.Err() == nil {
		s.dispatchPending(time.Now())
		if s.wantWork && !time.Now().Before(s.notBefore) {
			s.work(sysCtx)
			if ctx.Err() != nil {
				break
			}
		}
		// Loop again at once when a full batch suggests more is due and
		// workers are free; otherwise sleep until something happens.
		if s.wantWork && s.free() > 0 && !time.Now().Before(s.notBefore) {
			continue
		}
		wake := nextPoll
		if s.wantWork && s.notBefore.After(time.Now()) && s.notBefore.Before(wake) {
			wake = s.notBefore
		}
		if t, ok := s.earliestPendingDeadline(); ok && t.Before(wake) {
			wake = t
		}
		resetTimer(timer, time.Until(wake))
		select {
		case <-ctx.Done():
		case <-sub.C():
			s.wantWork, s.planDue = true, true
		case <-timer.C:
			if now := time.Now(); !now.Before(nextPoll) {
				s.wantWork, s.planDue = true, true
				nextPoll = now.Add(d.pollInterval)
			}
		case t := <-s.done:
			s.finished(t)
			// A finished first attempt lets the next event's first attempt
			// to that endpoint be claimed (the core claims them in event
			// order): look at once, not at the next poll.
			s.wantWork = true
		}
	}
	return s.shutdown()
}

func resetTimer(t *time.Timer, d time.Duration) {
	if d < 0 {
		d = 0
	}
	t.Reset(d) // Go ≥ 1.23: Reset on an unstopped timer is safe and drains
}

// scheduler is the state of one Run; it is only touched by Run's goroutine.
type scheduler struct {
	d        *Dispatcher
	ctx      context.Context    // parent of every attempt
	abort    context.CancelFunc // cancels ctx
	inflight int
	perHost  map[string]int
	pending  []*task // claimed, waiting for a per-host slot
	done     chan *task
	wg       sync.WaitGroup

	wantWork  bool // claims may find something
	planDue   bool // new events may need fanning out
	notBefore time.Time
	failures  int

	requeueWindow time.Time // start of the current busy-host release window
	requeued      int       // releases made in it
}

func (s *scheduler) free() int { return s.d.concurrency - s.inflight }

// work fans out new events (when due) and claims as many deliveries as
// there are free workers.
func (s *scheduler) work(ctx context.Context) {
	d := s.d
	if s.planDue {
		n, err := d.svc.PlanWebhookDeliveries(ctx)
		if err != nil {
			s.fail(ctx, "plan webhook deliveries", err)
			return
		}
		s.planDue = false
		if n > 0 {
			d.log.Debug("planned webhook deliveries", "count", n)
		}
	}
	free := s.free()
	if free <= 0 {
		return // a finishing worker brings us back
	}
	limit := min(d.batchSize, free)
	claimedAt := time.Now()
	jobs, err := d.svc.ClaimWebhookDeliveries(ctx, limit)
	if err != nil {
		s.fail(ctx, "claim webhook deliveries", err)
		return
	}
	s.failures = 0
	s.notBefore = time.Time{}
	s.wantWork = len(jobs) >= limit
	for _, job := range jobs {
		t := d.newTask(job, claimedAt)
		if ctx.Err() != nil {
			d.release(t, 0, "shutting down")
			continue
		}
		s.schedule(t)
	}
}

// release gives the claim on a job whose attempt never started back to the
// core, to be due again after delay. On failure the job is left to its
// lease, which is always safe.
func (d *Dispatcher) release(t *task, delay time.Duration, why string) {
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("releasing a webhook delivery panicked; it returns after its lease", "delivery_id", t.job.DeliveryID, "panic", r)
		}
	}()
	// Not the Run context: releases matter most while it is being cancelled.
	ctx, cancel := context.WithTimeout(core.WithActor(context.Background(), core.SystemActor(actorName)), d.releaseTimeout)
	defer cancel()
	released, err := d.svc.ReleaseWebhookDelivery(ctx, t.job.DeliveryID, t.job.ClaimID, delay)
	switch {
	case errors.Is(err, core.ErrNotFound):
		// The endpoint (and its deliveries) was deleted meanwhile.
	case err != nil:
		d.log.Warn("releasing a webhook delivery failed; it returns after its lease", "delivery_id", t.job.DeliveryID, "reason", why, "error", err)
	case !released:
		d.log.Debug("webhook delivery claim was already superseded", "delivery_id", t.job.DeliveryID, "reason", why)
	default:
		d.log.Debug("webhook delivery released", "delivery_id", t.job.DeliveryID, "reason", why, "due_in", delay)
	}
}

// requeue releases a job that cannot be sent because its host is busy, so
// that it is due again after requeueDelay — unless the release budget of
// the current window is spent, in which case it stays hidden for the rest
// of its lease (see Run).
func (s *scheduler) requeue(t *task, why string) {
	d := s.d
	now := time.Now()
	if now.Sub(s.requeueWindow) >= d.requeueDelay {
		s.requeueWindow, s.requeued = now, 0
	}
	if s.requeued >= d.requeueBudget {
		d.log.Debug("webhook delivery left to its lease ("+why+"; release budget spent)", "delivery_id", t.job.DeliveryID, "host", t.host)
		return
	}
	s.requeued++
	d.release(t, d.requeueDelay, why)
}

// fail records a core error and backs off.
func (s *scheduler) fail(ctx context.Context, what string, err error) {
	if ctx.Err() != nil {
		return // shutting down; not an error
	}
	s.failures++
	wait := s.d.backoffMin << min(s.failures-1, 16)
	wait = min(wait, s.d.backoffMax)
	wait = wait/2 + rand.N(wait/2+1) // jitter: [wait/2, wait]
	s.notBefore = time.Now().Add(wait)
	s.wantWork = true
	s.d.log.Error("webhook dispatcher: "+what+" failed; backing off", "error", err, "retry_in", wait, "failures", s.failures)
}

func (d *Dispatcher) newTask(job core.DeliveryJob, claimedAt time.Time) *task {
	t := &task{job: job, claimedAt: claimedAt, timeout: job.Timeout}
	if t.timeout <= 0 {
		t.timeout = core.WebhookTimeout
	}
	t.timeout = min(t.timeout, d.maxTimeout, d.lease/2)
	t.startBy = claimedAt.Add(d.lease - d.leaseMargin - t.timeout)
	if u, _, err := d.policy.parseURL(job.URL); err == nil {
		t.host = u.Host
	}
	return t
}

// schedule starts t, parks it until its host has a free slot, or gives it
// back when too much is parked already.
func (s *scheduler) schedule(t *task) {
	if s.canStart(t) {
		s.start(t)
		return
	}
	if len(s.pending) < s.d.maxPending {
		s.pending = append(s.pending, t)
		return
	}
	s.requeue(t, "host busy")
}

func (s *scheduler) canStart(t *task) bool {
	return s.inflight < s.d.concurrency && s.perHost[t.host] < s.d.maxPerHost
}

func (s *scheduler) start(t *task) {
	s.inflight++
	s.perHost[t.host]++
	s.wg.Add(1)
	go s.d.deliver(s.ctx, t, s.done, &s.wg)
}

func (s *scheduler) finished(t *task) {
	s.inflight--
	if s.perHost[t.host]--; s.perHost[t.host] <= 0 {
		delete(s.perHost, t.host)
	}
}

// dispatchPending starts parked tasks whose host has a slot and gives back
// those that could no longer finish inside their lease.
func (s *scheduler) dispatchPending(now time.Time) {
	kept := s.pending[:0]
	for _, t := range s.pending {
		switch {
		case !now.Before(t.startBy):
			s.requeue(t, "no slot in time")
		case s.canStart(t):
			s.start(t)
		default:
			kept = append(kept, t)
		}
	}
	clear(s.pending[len(kept):])
	s.pending = kept
}

func (s *scheduler) earliestPendingDeadline() (time.Time, bool) {
	var first time.Time
	for _, t := range s.pending {
		if first.IsZero() || t.startBy.Before(first) {
			first = t.startBy
		}
	}
	return first, !first.IsZero()
}

// shutdown releases parked tasks, waits for in-flight ones for the grace
// period, aborts the rest and waits for their completion to be recorded.
func (s *scheduler) shutdown() error {
	d := s.d
	if n := len(s.pending); n > 0 {
		d.log.Info("releasing parked webhook deliveries (shutting down)", "count", n)
		for _, t := range s.pending {
			d.release(t, 0, "shutting down")
		}
		s.pending = nil
	}
	finished := make(chan struct{})
	go func() { s.wg.Wait(); close(finished) }()
	grace := time.NewTimer(d.shutdownGrace)
	defer grace.Stop()
	select {
	case <-finished:
		d.log.Info("webhook dispatcher stopped")
		return nil
	case <-grace.C:
	}
	d.log.Warn("webhook dispatcher: grace period over; aborting in-flight deliveries", "in_flight", s.inflightApprox())
	s.abort()
	final := time.NewTimer(d.finalWait)
	defer final.Stop()
	select {
	case <-finished:
		d.log.Info("webhook dispatcher stopped")
		return nil
	case <-final.C:
		return errors.New("webhook: deliveries still running after shutdown; their results are recorded when they finish or their leases expire")
	}
}

func (s *scheduler) inflightApprox() int { return s.inflight - len(s.done) }
