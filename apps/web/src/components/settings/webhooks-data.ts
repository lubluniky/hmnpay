import type { Delivery, WebhookEndpoint, WebhookEventType } from "@/lib/api/types"

/**
 * The delivery policy the screens explain. These mirror the server's constants
 * (core/webhook.go: WebhookMaxAttempts, WebhookTimeout, WebhookAutoDisableAfter,
 * WebhookRetention, WebhookSecretOverlap) and the retry schedule in the README; what a
 * particular endpoint or delivery will do next always comes from the API (`disable_at`,
 * `next_attempt_at`, `max_attempts`, `previous_secret_expires_at`), not from here.
 */
export const WEBHOOK_POLICY = {
  maxAttempts: 8,
  timeoutMs: 10_000,
  autoDisableAfterHours: 72,
  retentionDays: 30,
  secretOverlapHours: 24,
} as const

/** What each event means, for the event picker. */
export const WEBHOOK_EVENT_INFO: Record<WebhookEventType, string> = {
  "payment.created": "Created, waiting for funds.",
  "payment.awaiting": "A transfer dropped out of the chain; waiting again.",
  "payment.detected": "Part of the amount arrived.",
  "payment.confirming": "The full amount is on-chain; paid once it has its confirmations and the chain has finalised its block.",
  "payment.paid": "Confirmed in full. Fulfil the order.",
  "payment.underpaid": "The time ran out with less than requested.",
  "payment.overpaid": "Confirmed, with more than requested.",
  "payment.expired": "Closed with nothing that counts, or cancelled.",
  "payment.failed": "The transfer dropped out and the time ran out.",
  "payment.late_funds": "Funds confirmed after the payment had closed.",
  "payment.resolved": "An operator resolved the payment’s issue.",
  "payment.reopened": "An operator reopened the issue.",
}

/** What an endpoint is called: its description, or its host when it has none. */
export function endpointName(endpoint: Pick<WebhookEndpoint, "url" | "description">): string {
  return endpoint.description || endpoint.url.replace(/^https?:\/\//, "").split("/")[0]
}

/** The event a delivery carries, as far as the stored request body says: its position in the event log. */
export function deliverySeq(delivery: Pick<Delivery, "request_body">): number | null {
  try {
    const seq = (JSON.parse(delivery.request_body) as { seq?: unknown }).seq
    return typeof seq === "number" ? seq : null
  } catch {
    return null
  }
}

/**
 * The request hmnpay sends for a delivery, as text. Everything here is known from the
 * delivery itself except the signature, which is computed per attempt from the endpoint's
 * secret and is never stored: its place shows the format.
 */
export function deliveryRequestHead(delivery: Delivery): string {
  let path = "/"
  let host = ""
  try {
    const url = new URL(delivery.url)
    path = url.pathname + url.search
    host = url.host
  } catch {
    // Keep defaults.
  }
  return [
    `POST ${path} HTTP/1.1`,
    `Host: ${host}`,
    "Content-Type: application/json",
    `Hmnpay-Event-Id: ${delivery.event_id}`,
    `Hmnpay-Delivery-Id: ${delivery.id}`,
    "Hmnpay-Signature: t=<unix seconds>,v1=<HMAC-SHA256 of “t.body”, hex>",
  ].join("\n")
}
