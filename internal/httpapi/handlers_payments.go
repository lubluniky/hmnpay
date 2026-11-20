package httpapi

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/lubluniky/hmnpay/core"
)

// ---- payments ----

// chainNotReadyRetry is the Retry-After of chain_not_ready: soon (the state
// normally lasts seconds), and not sooner than the public link route's
// per-link budget refills (LinkPaymentPerLink, one every 5 seconds by
// default), so that a client retrying exactly as told is never answered 429.
const chainNotReadyRetry = 5 * time.Second

type createPaymentRequest struct {
	Amount        string            `json:"amount"`
	OrderID       string            `json:"order_id"`
	Description   string            `json:"description"`
	CustomerEmail string            `json:"customer_email"`
	RedirectURL   string            `json:"redirect_url"`
	Metadata      map[string]string `json:"metadata"`
	TTLSeconds    int64             `json:"ttl_seconds"`
}

// createPayment creates a payment. The Idempotency-Key header makes it
// safe to retry: 201 when created, 200 when the key returned an existing
// payment, 409 idempotency_key_reused when the parameters differ. 503
// chain_not_ready while the chain watcher has not processed a block of the
// network yet (right after setup or a change of network): retry.
func (h *Handler) createPayment(w http.ResponseWriter, r *http.Request) error {
	var in createPaymentRequest
	if err := decodeJSON(w, r, &in, defaultBodyLimit); err != nil {
		return err
	}
	ttl, err := seconds("ttl_seconds", in.TTLSeconds)
	if err != nil {
		return err
	}
	view, created, err := h.svc.CreatePayment(r.Context(), core.CreatePaymentInput{
		Amount: in.Amount, OrderID: in.OrderID, Description: in.Description,
		CustomerEmail: in.CustomerEmail, RedirectURL: in.RedirectURL, Metadata: in.Metadata,
		TTL: ttl, IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		return withFieldNames(err, map[string]string{"ttl": "ttl_seconds", "idempotency_key": "Idempotency-Key"})
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", "/api/v1/payments/"+view.ID)
	}
	return writeJSON(w, status, view)
}

func (h *Handler) getPayment(w http.ResponseWriter, r *http.Request) error {
	view, err := h.svc.GetPayment(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, view)
}

func (h *Handler) getPaymentDetail(w http.ResponseWriter, r *http.Request) error {
	d, err := h.svc.GetPaymentDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, paymentDetailDTO{
		Payment:    d.Payment,
		Transfers:  nonNil(d.Transfers),
		Timeline:   newEventDTOs(d.Events),
		Deliveries: newDeliveryDTOs(d.Deliveries),
	})
}

var paymentViews = []core.PaymentGroup{core.GroupAll, core.GroupOpen, core.GroupPaid, core.GroupAttention, core.GroupExpired}

// paymentFilter maps list query parameters onto the core's filter:
//
//	view          all | open | paid | attention | expired (the dashboard tabs)
//	status        comma-separated statuses (cannot be combined with view)
//	q             search (id, order id, description, email, address, tx hash, payer)
//	created_from  RFC 3339 or YYYY-MM-DD (inclusive)
//	created_to    RFC 3339 (exclusive) or YYYY-MM-DD (inclusive day)
//	order_id      exact order id
//	link_id       payments created from a payment link
func paymentFilter(r *http.Request) (core.PaymentFilter, error) {
	q := r.URL.Query()
	var f core.PaymentFilter
	var err error
	f.Search = q.Get("q")
	f.OrderID = q.Get("order_id")
	f.LinkID = q.Get("link_id")
	if f.CreatedFrom, err = queryTime(q, "created_from", false); err != nil {
		return f, err
	}
	if f.CreatedTo, err = queryTime(q, "created_to", true); err != nil {
		return f, err
	}
	statuses := queryList(q, "status")
	view := q.Get("view")
	if view != "" && len(statuses) > 0 {
		return f, errQuery("status", "status cannot be combined with view")
	}
	if view != "" {
		g := core.PaymentGroup(view)
		if !slices.Contains(paymentViews, g) {
			return f, errQuery("view", "view must be one of all, open, paid, attention, expired")
		}
		f = g.Apply(f)
	}
	for _, s := range statuses {
		f.Statuses = append(f.Statuses, core.PaymentStatus(s))
	}
	return f, nil
}

// listPayments returns a page of payments.
//
//	sort    created | amount (default created)
//	dir     asc | desc (default desc)
//	limit   1..500 (default 50); offset ≥ 0
//	counts  true adds per-tab counts for the same search and date range
func (h *Handler) listPayments(w http.ResponseWriter, r *http.Request) error {
	f, err := paymentFilter(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	sort := core.PaymentSort{By: core.PaymentSortKey(q.Get("sort")), Desc: true}
	switch q.Get("dir") {
	case "", "desc":
	case "asc":
		sort.Desc = false
	default:
		return errQuery("dir", "dir must be asc or desc")
	}
	limit, err := queryInt(q, "limit", 50)
	if err != nil {
		return err
	}
	offset, err := queryInt(q, "offset", 0)
	if err != nil {
		return err
	}
	page, err := h.svc.ListPayments(r.Context(), core.ListPaymentsInput{Filter: f, Sort: &sort, Page: core.Page{Limit: limit, Offset: offset}})
	if err != nil {
		return err
	}
	limit = clamp(limit, 50, 500)
	out := paymentListBody{Data: nonNil(page.Items), Page: newPage(limit, offset, page.Total)}
	if q.Get("counts") == "true" || q.Get("counts") == "1" {
		c, err := h.svc.CountPayments(r.Context(), f)
		if err != nil {
			return err
		}
		out.Counts = &countsDTO{All: c.All, Open: c.Open, Paid: c.Paid, Attention: c.Attention, Expired: c.Expired}
	}
	return writeJSON(w, http.StatusOK, out)
}

type resolveRequest struct {
	Note string `json:"note"`
}

func (h *Handler) resolvePayment(w http.ResponseWriter, r *http.Request) error {
	var in resolveRequest
	if err := decodeOptionalJSON(w, r, &in, smallBodyLimit); err != nil {
		return err
	}
	view, err := h.svc.ResolvePayment(r.Context(), r.PathValue("id"), in.Note)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, view)
}

func (h *Handler) reopenPayment(w http.ResponseWriter, r *http.Request) error {
	if err := decodeOptionalJSON(w, r, &struct{}{}, smallBodyLimit); err != nil {
		return err
	}
	view, err := h.svc.ReopenPayment(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, view)
}

// cancelPayment closes an unpaid payment. The payment becomes "expired"
// with cancelled_at set; 409 payment_has_funds once funds were seen, 409
// payment_settled if it settled by itself. Cancelling twice answers 200
// with the same payment, so the call is safe to retry.
func (h *Handler) cancelPayment(w http.ResponseWriter, r *http.Request) error {
	if err := decodeOptionalJSON(w, r, &struct{}{}, smallBodyLimit); err != nil {
		return err
	}
	view, err := h.svc.CancelPayment(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, view)
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) error {
	days, err := queryInt(r.URL.Query(), "days", 0)
	if err != nil {
		return err
	}
	ov, err := h.svc.Overview(r.Context(), days)
	if err != nil {
		return err
	}
	ov.Series = nonNil(ov.Series)
	return writeJSON(w, http.StatusOK, ov)
}

func (h *Handler) nodeStatus(w http.ResponseWriter, r *http.Request) error {
	ns, err := h.svc.NodeStatus(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, newNodeStatusDTO(ns))
}

type eventsBody struct {
	Data []eventDTO `json:"data"`
	// Next is the cursor to pass as ?after= on the next call (the last
	// event's seq, or the request's after when nothing was returned).
	Next int64 `json:"next"`
}

// events reads the event log from a cursor: ?after=<seq>&payment_id=&
// types=a,b&limit= (1..1000, default 100). Delivery is at-least-once;
// poll with the returned next cursor.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	after, err := queryInt64(q, "after")
	if err != nil {
		return err
	}
	limit, err := queryInt(q, "limit", 100)
	if err != nil {
		return err
	}
	f := core.EventFilter{After: after, PaymentID: q.Get("payment_id"), Limit: clamp(limit, 100, 1000)}
	for _, t := range queryList(q, "types") {
		f.Types = append(f.Types, core.EventType(t))
	}
	evs, err := h.svc.Events(r.Context(), f)
	if err != nil {
		return err
	}
	next := after
	if len(evs) > 0 {
		next = evs[len(evs)-1].Seq
	}
	return writeJSON(w, http.StatusOK, eventsBody{Data: newEventDTOs(evs), Next: next})
}

// ---- payment links ----

type linkRequest struct {
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Amount      *string    `json:"amount"`     // null or absent: customer chooses
	MinAmount   *string    `json:"min_amount"` // customer chooses: null or absent means the default, one whole unit
	MaxUses     *int       `json:"max_uses"`   // null or absent: unlimited
	ExpiresAt   *time.Time `json:"expires_at"` // null or absent: never
	RedirectURL string     `json:"redirect_url"`
	Active      *bool      `json:"active"` // default true on create; required on update
}

func (in linkRequest) core() core.LinkInput {
	out := core.LinkInput{Slug: in.Slug, Name: in.Name, Description: in.Description, RedirectURL: in.RedirectURL, Active: in.Active == nil || *in.Active}
	if in.Amount != nil {
		out.Amount = *in.Amount
	}
	if in.MinAmount != nil {
		out.MinAmount = *in.MinAmount
	}
	if in.MaxUses != nil {
		out.MaxUses = *in.MaxUses
	}
	if in.ExpiresAt != nil {
		out.ExpiresAt = *in.ExpiresAt
	}
	return out
}

func (h *Handler) createLink(w http.ResponseWriter, r *http.Request) error {
	var in linkRequest
	if err := decodeJSON(w, r, &in, defaultBodyLimit); err != nil {
		return err
	}
	view, err := h.svc.CreateLink(r.Context(), in.core())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, view)
}

// updateLink replaces every editable field (the slug is immutable and
// ignored, as in the core).
func (h *Handler) updateLink(w http.ResponseWriter, r *http.Request) error {
	var in linkRequest
	if err := decodeJSON(w, r, &in, defaultBodyLimit); err != nil {
		return err
	}
	if in.Active == nil {
		return errRequired("active")
	}
	view, err := h.svc.UpdateLink(r.Context(), r.PathValue("id"), in.core())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, view)
}

// deleteLink removes a payment link for good; its payments are kept.
func (h *Handler) deleteLink(w http.ResponseWriter, r *http.Request) error {
	if err := h.svc.DeleteLink(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) setLinkActive(active bool) apiFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if err := decodeOptionalJSON(w, r, &struct{}{}, smallBodyLimit); err != nil {
			return err
		}
		view, err := h.svc.SetLinkActive(r.Context(), r.PathValue("id"), active)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, view)
	}
}

func (h *Handler) getLink(w http.ResponseWriter, r *http.Request) error {
	view, err := h.svc.GetLink(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, view)
}

func (h *Handler) listLinks(w http.ResponseWriter, r *http.Request) error {
	views, err := h.svc.ListLinks(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, listBody[core.LinkView]{Data: nonNil(views)})
}

// ---- public checkout ----

func (h *Handler) publicCheckout(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if !validPaymentID(id) {
		return errPublicNotFound
	}
	// The log is read once before the view, so that the usual case needs
	// one more reading only (see checkoutAt).
	ctx := r.Context()
	cursor, count, err := h.paymentEvents(systemContext(ctx), id, 0)
	if err != nil {
		return asPublic(err)
	}
	view, _, count, err := h.checkoutAt(ctx, id, cursor, count)
	if err != nil {
		return asPublic(err)
	}
	return writeJSON(w, http.StatusOK, checkoutDTO{CheckoutView: view, Version: count})
}

func (h *Handler) publicLink(w http.ResponseWriter, r *http.Request) error {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		return errPublicNotFound
	}
	view, err := h.svc.GetLinkCheckout(r.Context(), slug)
	if err != nil {
		return asPublic(err)
	}
	return writeJSON(w, http.StatusOK, view)
}

type linkPaymentRequest struct {
	Amount        string `json:"amount"`
	CustomerEmail string `json:"customer_email"`
}

// publicLinkPayment creates a payment from a payment link. A new payment
// allocates a deposit address and joins the chain watch list, so the route
// is limited per IP, per IP and link, per link and instance-wide; the core
// additionally answers 503 watch_list_full when the watch list is at its
// cap, and 503 chain_not_ready (as for POST /api/v1/payments) while the
// chain watcher has not processed a block of the network. The checkout page should send an Idempotency-Key per
// visit so a reload returns the same payment: 201 when created, 200 when
// the key returned the payment of an earlier call — as for POST
// /api/v1/payments. (A replay still spends rate-limit budget: it is a
// write transaction either way.) A checkout the link's rules refuse (409,
// 422) gives back its per-IP-and-link token and spends the others.
func (h *Handler) publicLinkPayment(w http.ResponseWriter, r *http.Request) error {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		return errPublicNotFound
	}
	// Four budgets, narrowest first; a refusal further down gives the
	// tokens taken above back. Per IP and link: one client cannot hold
	// several of a limited link's uses. Per link: a distributed flood on one
	// link does not starve the customers of every other link.
	ipKey := h.rateKey(r)
	type budget struct {
		lim *limiter
		key string
	}
	budgets := []budget{
		{h.lim.linkIP, ipKey},
		{h.lim.linkIPLink, ipKey + "|" + slug},
		{h.lim.linkPerLink, slug},
		{h.lim.linkGlobal, "global"},
	}
	for i, b := range budgets {
		if err := b.lim.take(b.key); err != nil {
			for _, taken := range budgets[:i] {
				taken.lim.refund(taken.key)
			}
			return err
		}
	}
	var in linkPaymentRequest
	if err := decodeJSON(w, r, &in, smallBodyLimit); err != nil {
		return err
	}
	view, created, err := h.svc.CreatePaymentFromLink(r.Context(), slug, core.LinkPaymentInput{
		Amount: in.Amount, CustomerEmail: in.CustomerEmail, IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")),
	})
	if err != nil {
		if k := core.KindOf(err); k == core.KindConflict || k == core.KindInvalid {
			// The link's rules refused the checkout (used up, disabled,
			// expired, an amount below the minimum, a reused key): no
			// payment, no address, no use of the link was taken. The
			// per-IP-and-link budget exists to stop one client from
			// holding several uses, so a refusal gives its token back — a
			// customer who is told "used up" twice must not be locked out
			// of the link once a use frees up. The other budgets stay
			// spent: they bound how often anybody can make the core say
			// no, and what the refusal says the public link page shows
			// anyway.
			h.lim.linkIPLink.refund(ipKey + "|" + slug)
		}
		if core.KindOf(err) == core.KindUnavailable {
			// The instance cannot take payments at the moment — no processed
			// block yet (chain_not_ready), a server clock behind the chain
			// (clock_behind), network settings that cannot be used
			// (network_invalid), the links' share of the watch list used up
			// (watch_list_full): nothing was handed out, and the answer says
			// when to ask again (Retry-After). The client's own budgets are
			// given back, so a checkout page or an API client retrying as
			// told is never locked out of the link by the wait; the per-link
			// and instance-wide budgets stay spent and still bound how often
			// anybody can make the core say no. Every such Retry-After is at
			// least the per-link refill (chainNotReadyRetry), so one client
			// retrying as told keeps that budget full as well.
			h.lim.linkIP.refund(ipKey)
			h.lim.linkIPLink.refund(ipKey + "|" + slug)
		}
		return asPublic(withFieldNames(err, map[string]string{"idempotency_key": "Idempotency-Key"}))
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	// The payment exists whatever happens from here on, so the answer does
	// not depend on this second look: if the view cannot be read again with
	// its version, what the core returned is sent with the lowest version a
	// payment can have, which no later view is older than.
	out := checkoutDTO{CheckoutView: view, Version: 1}
	if fresh, _, count, err := h.checkoutAt(r.Context(), view.ID, 0, 0); err == nil && count > 0 {
		out = checkoutDTO{CheckoutView: fresh, Version: count}
	}
	return writeJSON(w, status, out)
}
