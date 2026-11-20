package httpapi

import (
	"net/http"

	"github.com/lubluniky/hmnpay/core"
)

type endpointRequest struct {
	URL         string           `json:"url"`
	Description string           `json:"description"`
	Events      []core.EventType `json:"events"`
	Enabled     *bool            `json:"enabled"` // default true on create; required on update
}

func (in endpointRequest) core() core.WebhookEndpointInput {
	return core.WebhookEndpointInput{URL: in.URL, Description: in.Description, Events: in.Events, Enabled: in.Enabled == nil || *in.Enabled}
}

// createEndpoint returns the endpoint with its signing secret unmasked —
// one of the three responses that reveal it (create, rotate, reveal). It is
// the only one an API key can get: the secret of the endpoint it has just
// created, once. Rotate and reveal are operator-only (the core enforces
// it), and every other response carries a mask — or, for an API key, no
// secret material at all.
func (h *Handler) createEndpoint(w http.ResponseWriter, r *http.Request) error {
	var in endpointRequest
	if err := decodeJSON(w, r, &in, defaultBodyLimit); err != nil {
		return err
	}
	e, err := h.svc.CreateWebhookEndpoint(r.Context(), in.core())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, newEndpointDTO(e, true))
}

func (h *Handler) updateEndpoint(w http.ResponseWriter, r *http.Request) error {
	var in endpointRequest
	if err := decodeJSON(w, r, &in, defaultBodyLimit); err != nil {
		return err
	}
	if in.Enabled == nil {
		return errRequired("enabled")
	}
	e, err := h.svc.UpdateWebhookEndpoint(r.Context(), r.PathValue("id"), in.core())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, newEndpointDTO(e, false))
}

func (h *Handler) deleteEndpoint(w http.ResponseWriter, r *http.Request) error {
	if err := h.svc.DeleteWebhookEndpoint(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) rotateSecret(w http.ResponseWriter, r *http.Request) error {
	if err := decodeOptionalJSON(w, r, &struct{}{}, smallBodyLimit); err != nil {
		return err
	}
	e, err := h.svc.RotateWebhookSecret(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, newEndpointDTO(e, true))
}

// revealSecret is the explicit "show secret" action, for the operator only
// (403 for API keys: a key that could read a signing secret could forge
// events). It is a POST so that it is never cached, prefetched or triggered
// by a cross-site link, and is subject to the CSRF check for cookie
// sessions.
func (h *Handler) revealSecret(w http.ResponseWriter, r *http.Request) error {
	if err := decodeOptionalJSON(w, r, &struct{}{}, smallBodyLimit); err != nil {
		return err
	}
	sum, err := h.svc.RevealWebhookSecret(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	out := secretDTO{Secret: sum.Secret}
	if sum.PreviousSecret != "" {
		out.PreviousSecret = &sum.PreviousSecret
		out.PreviousSecretExpiresAt = optTime(sum.PreviousSecretExpiresAt)
	}
	return writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getEndpoint(w http.ResponseWriter, r *http.Request) error {
	sum, err := h.svc.GetWebhookEndpoint(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, newEndpointSummaryDTO(sum))
}

func (h *Handler) listEndpoints(w http.ResponseWriter, r *http.Request) error {
	list, err := h.svc.ListWebhookEndpoints(r.Context())
	if err != nil {
		return err
	}
	out := make([]endpointDTO, 0, len(list))
	for _, s := range list {
		out = append(out, newEndpointSummaryDTO(s))
	}
	return writeJSON(w, http.StatusOK, listBody[endpointDTO]{Data: out})
}

func (h *Handler) testEndpoint(w http.ResponseWriter, r *http.Request) error {
	if err := decodeOptionalJSON(w, r, &struct{}{}, smallBodyLimit); err != nil {
		return err
	}
	d, err := h.svc.SendTestWebhook(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, newDeliveryDTO(d))
}

// listDeliveries: ?endpoint_id=&payment_id=&event_id=&status=a,b&
// created_from=&limit= (1..200, default 50)&offset=. Statuses are
// validated by the core (422 with a field error on "status").
func (h *Handler) listDeliveries(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f := core.DeliveryFilter{EndpointID: q.Get("endpoint_id"), PaymentID: q.Get("payment_id"), EventID: q.Get("event_id")}
	for _, s := range queryList(q, "status") {
		f.Statuses = append(f.Statuses, core.DeliveryStatus(s))
	}
	var err error
	if f.CreatedFrom, err = queryTime(q, "created_from", false); err != nil {
		return err
	}
	limit, err := queryInt(q, "limit", 50)
	if err != nil {
		return err
	}
	offset, err := queryInt(q, "offset", 0)
	if err != nil {
		return err
	}
	page, err := h.svc.ListWebhookDeliveries(r.Context(), f, core.Page{Limit: limit, Offset: offset})
	if err != nil {
		return err
	}
	limit = clamp(limit, 50, 200)
	return writeJSON(w, http.StatusOK, listBody[deliveryDTO]{Data: newDeliveryDTOs(page.Items), Page: newPage(limit, offset, page.Total)})
}

func (h *Handler) getDelivery(w http.ResponseWriter, r *http.Request) error {
	d, err := h.svc.GetWebhookDelivery(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, newDeliveryDTO(d))
}

func (h *Handler) resendDelivery(w http.ResponseWriter, r *http.Request) error {
	if err := decodeOptionalJSON(w, r, &struct{}{}, smallBodyLimit); err != nil {
		return err
	}
	d, err := h.svc.ResendWebhookDelivery(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, newDeliveryDTO(d))
}
