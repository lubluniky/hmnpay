package httpapi

// routes registers every endpoint. The table is the API's contract; the
// request and response shapes are the types in dto.go and the core's view
// types (the web interface mirrors them in apps/web/src/lib/api/types.ts).
func (h *Handler) routes() {
	for _, rt := range []route{
		// Health.
		{pattern: "GET /healthz", mode: authPublic, fn: h.healthz},
		{pattern: "GET /readyz", mode: authPublic, fn: h.readyz},

		// Session.
		{pattern: "POST /api/v1/auth/login", mode: authPublic, fn: h.login},
		{pattern: "POST /api/v1/auth/logout", mode: authPublic, limit: limitPublicRead, fn: h.logout},
		{pattern: "GET /api/v1/auth/me", mode: authRequired, fn: h.me},
		{pattern: "GET /api/v1/instance", mode: authRequired, fn: h.instance},

		// First-run setup: public until completed (the core decides).
		{pattern: "GET /api/v1/setup", mode: authPublic, limit: limitPublicRead, fn: h.setupStatus},
		{pattern: "POST /api/v1/setup/probe-rpc", mode: authOptional, limit: limitSetup, fn: h.probeRPC},
		{pattern: "POST /api/v1/setup/preview-addresses", mode: authOptional, limit: limitSetup, fn: h.previewAddresses},
		{pattern: "POST /api/v1/setup/complete", mode: authPublic, limit: limitSetup, fn: h.completeSetup},

		// Payments and reporting.
		{pattern: "POST /api/v1/payments", mode: authRequired, fn: h.createPayment},
		{pattern: "GET /api/v1/payments", mode: authRequired, fn: h.listPayments},
		{pattern: "GET /api/v1/payments/{id}", mode: authRequired, fn: h.getPayment},
		{pattern: "GET /api/v1/payments/{id}/detail", mode: authRequired, fn: h.getPaymentDetail},
		{pattern: "POST /api/v1/payments/{id}/resolve", mode: authRequired, fn: h.resolvePayment},
		{pattern: "POST /api/v1/payments/{id}/reopen", mode: authRequired, fn: h.reopenPayment},
		{pattern: "POST /api/v1/payments/{id}/cancel", mode: authRequired, fn: h.cancelPayment},
		{pattern: "GET /api/v1/overview", mode: authRequired, fn: h.overview},
		{pattern: "GET /api/v1/node-status", mode: authRequired, fn: h.nodeStatus},
		{pattern: "GET /api/v1/events", mode: authRequired, fn: h.events},

		// Payment links.
		{pattern: "POST /api/v1/payment-links", mode: authRequired, fn: h.createLink},
		{pattern: "GET /api/v1/payment-links", mode: authRequired, fn: h.listLinks},
		{pattern: "GET /api/v1/payment-links/{id}", mode: authRequired, fn: h.getLink},
		{pattern: "PUT /api/v1/payment-links/{id}", mode: authRequired, fn: h.updateLink},
		{pattern: "DELETE /api/v1/payment-links/{id}", mode: authRequired, fn: h.deleteLink},
		{pattern: "POST /api/v1/payment-links/{id}/activate", mode: authRequired, fn: h.setLinkActive(true)},
		{pattern: "POST /api/v1/payment-links/{id}/deactivate", mode: authRequired, fn: h.setLinkActive(false)},

		// Webhooks.
		{pattern: "POST /api/v1/webhook-endpoints", mode: authRequired, fn: h.createEndpoint},
		{pattern: "GET /api/v1/webhook-endpoints", mode: authRequired, fn: h.listEndpoints},
		{pattern: "GET /api/v1/webhook-endpoints/{id}", mode: authRequired, fn: h.getEndpoint},
		{pattern: "PUT /api/v1/webhook-endpoints/{id}", mode: authRequired, fn: h.updateEndpoint},
		{pattern: "DELETE /api/v1/webhook-endpoints/{id}", mode: authRequired, fn: h.deleteEndpoint},
		{pattern: "POST /api/v1/webhook-endpoints/{id}/rotate-secret", mode: authRequired, fn: h.rotateSecret},
		{pattern: "POST /api/v1/webhook-endpoints/{id}/reveal-secret", mode: authRequired, fn: h.revealSecret},
		{pattern: "POST /api/v1/webhook-endpoints/{id}/test", mode: authRequired, fn: h.testEndpoint},
		{pattern: "GET /api/v1/webhook-deliveries", mode: authRequired, fn: h.listDeliveries},
		{pattern: "GET /api/v1/webhook-deliveries/{id}", mode: authRequired, fn: h.getDelivery},
		{pattern: "POST /api/v1/webhook-deliveries/{id}/resend", mode: authRequired, fn: h.resendDelivery},

		// API keys.
		{pattern: "POST /api/v1/api-keys", mode: authRequired, fn: h.createAPIKey},
		{pattern: "GET /api/v1/api-keys", mode: authRequired, fn: h.listAPIKeys},
		{pattern: "POST /api/v1/api-keys/{id}/revoke", mode: authRequired, fn: h.revokeAPIKey},

		// Settings and the operator account.
		{pattern: "GET /api/v1/settings", mode: authRequired, fn: h.getSettings},
		{pattern: "PUT /api/v1/settings/merchant", mode: authRequired, fn: h.updateMerchant},
		{pattern: "PUT /api/v1/settings/payments", mode: authRequired, fn: h.updatePaymentDefaults},
		{pattern: "PUT /api/v1/settings/network", mode: authRequired, fn: h.updateNetwork},
		{pattern: "PUT /api/v1/settings/wallet", mode: authRequired, fn: h.updateWallet},
		{pattern: "GET /api/v1/operator", mode: authRequired, fn: h.getOperator},
		{pattern: "POST /api/v1/operator/password", mode: authRequired, fn: h.changePassword},
		{pattern: "GET /api/v1/operator/sessions", mode: authRequired, fn: h.listSessions},
		{pattern: "DELETE /api/v1/operator/sessions/{id}", mode: authRequired, fn: h.revokeSession},

		// Public hosted checkout (no credentials; ids and slugs are the
		// capability).
		{pattern: "GET /api/v1/public/payments/{id}", mode: authPublic, limit: limitPublicRead, fn: h.publicCheckout},
		{pattern: "GET /api/v1/public/payments/{id}/stream", mode: authPublic, limit: limitPublicRead, stream: true, fn: h.checkoutStream},
		{pattern: "GET /api/v1/public/links/{slug}", mode: authPublic, limit: limitPublicRead, fn: h.publicLink},
		{pattern: "POST /api/v1/public/links/{slug}/payments", mode: authPublic, fn: h.publicLinkPayment},
	} {
		h.handle(rt)
	}
}
