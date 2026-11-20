/**
 * The wire shapes of the hmnpay REST API (/api/v1), mirroring the Go side field by field:
 * internal/httpapi/dto.go and the view types in core (PaymentView, CheckoutView, TransferView,
 * LinkView, LinkCheckoutView, Overview). Names are the JSON names; nothing is renamed or
 * re-cased on the way in.
 *
 * Each shape is a schema (schema.ts) and its TypeScript type is inferred from that schema. The
 * end-to-end tests validate every response of the real server against these schemas strictly,
 * so a field added, removed or retyped in Go fails a test here until this file follows.
 *
 * Amounts are decimal strings in whole units of the asset ("149.00"); never parse them into a
 * JavaScript number (see lib/money.ts). Timestamps are RFC 3339 strings in UTC.
 */
import { amount, arr, bool, int, json, nullable, num, obj, oneOf, optional, record, str, time, type Infer } from "./schema"

/* --------------------------------- enums ---------------------------------- */

export const PAYMENT_STATUSES = ["awaiting", "detected", "confirming", "paid", "underpaid", "overpaid", "expired", "failed"] as const
export const paymentStatus = oneOf(...PAYMENT_STATUSES)
export type PaymentStatus = Infer<typeof paymentStatus>

/** Which issue is open on a payment ("" for none). */
export const attention = oneOf("", "underpaid", "overpaid", "failed", "late_funds")
export type Attention = Infer<typeof attention>

export const transferStatus = oneOf("pending", "confirmed", "orphaned")
export const linkStatus = oneOf("active", "disabled", "expired", "limit")
export type LinkStatus = Infer<typeof linkStatus>
export const deliveryStatus = oneOf("pending", "retrying", "succeeded", "failed")
export type DeliveryStatus = Infer<typeof deliveryStatus>

export const API_KEY_SCOPES = ["payments:read", "payments:write", "links:write", "webhooks:manage"] as const
export const scope = oneOf(...API_KEY_SCOPES)
export type ApiKeyScope = Infer<typeof scope>

/** Events an endpoint can subscribe to (core.WebhookEventTypes). */
export const WEBHOOK_EVENT_TYPES = [
  "payment.created",
  "payment.awaiting",
  "payment.detected",
  "payment.confirming",
  "payment.paid",
  "payment.underpaid",
  "payment.overpaid",
  "payment.expired",
  "payment.failed",
  "payment.late_funds",
  "payment.resolved",
  "payment.reopened",
] as const
export type WebhookEventType = (typeof WEBHOOK_EVENT_TYPES)[number]
/** Every event type in the log: the payment events plus the instance's own. */
export const eventType = oneOf(...WEBHOOK_EVENT_TYPES, "settings.updated", "webhook_endpoint.disabled", "webhook.test")
export type EventType = Infer<typeof eventType>

export const probeOutcome = oneOf("ok", "unreachable", "wrong_chain", "token_mismatch")
export type ProbeOutcome = Infer<typeof probeOutcome>

/* -------------------------------- envelopes -------------------------------- */

export const page = obj({ limit: int, offset: int, total: int, has_more: bool })
export type Page = Infer<typeof page>

export const fieldError = obj({ field: str, code: str, message: str })
export type FieldError = Infer<typeof fieldError>

/**
 * Every error answer (internal/httpapi/errors.go). `code` is an open set of strings; the codes a
 * screen treats specially are listed in lib/errors.ts.
 */
export const errorBody = obj({
  error: obj({ code: str, message: str, fields: optional(arr(fieldError)) }),
  request_id: str,
})

/**
 * 503 answers that mean "not now, the same request will work later" (each with Retry-After).
 * chain_not_ready (5 s): POST /payments and POST /public/links/{slug}/payments before the chain
 * watcher has processed its first block (after setup or a change of network). clock_behind
 * (60 s): the same routes while the server clock is behind the chain; it lasts until the clock is
 * corrected. network_invalid (60 s): the same routes while the stored network settings cannot
 * be used; it lasts until they are repaired. watch_list_full (60 s): the public link route while
 * the links' share of the watch list is used up. None takes a payment, an address or a link use,
 * and on the public link route none spends the client's own rate-limit budgets. busy: too many password checks at once.
 * watch_list_full: a payment link's share of the chain watch list is used up.
 */
export const RETRYABLE_ERROR_CODES = ["chain_not_ready", "clock_behind", "network_invalid", "busy", "watch_list_full"] as const
export type RetryableErrorCode = (typeof RETRYABLE_ERROR_CODES)[number]

/* -------------------------------- payments -------------------------------- */

export const resolution = obj({ resolved_at: time, by: str, note: str })

/**
 * A payment that has the confirmations it needs but is not confirmed yet, because the chain has not
 * finalised the block of its newest pending transfer (block); final is the newest block that is
 * final so far. The status stays "confirming" (or "detected") meanwhile. Null otherwise.
 */
export const finalityWait = obj({ block: int, final: int })
export type FinalityWait = Infer<typeof finalityWait>

export const payment = obj({
  id: str,
  status: paymentStatus,
  order_id: str,
  description: str,
  amount,
  amount_received: amount,
  amount_confirmed: amount,
  amount_due: amount,
  asset: str,
  network: str,
  chain_id: int,
  decimals: int,
  address: str,
  tx_hash: nullable(str),
  from_address: nullable(str),
  confirmations: int,
  required_confirmations: int,
  awaiting_finality: nullable(finalityWait),
  customer_email: str,
  redirect_url: str,
  metadata: record(str),
  link_id: nullable(str),
  checkout_url: str,
  needs_attention: bool,
  attention,
  resolution: nullable(resolution),
  created_at: time,
  expires_at: time,
  updated_at: time,
  paid_at: nullable(time),
  settled_at: nullable(time),
  cancelled_at: nullable(time),
  cancelled_by: str,
})
export type Payment = Infer<typeof payment>

export const counts = obj({ all: int, open: int, paid: int, attention: int, expired: int })
export type PaymentCounts = Infer<typeof counts>

export const paymentList = obj({ data: arr(payment), page, counts: optional(counts) })
export type PaymentList = Infer<typeof paymentList>

export const transfer = obj({
  tx_hash: str,
  log_index: int,
  block_number: int,
  block_hash: str,
  block_time: nullable(time),
  from: str,
  to: str,
  amount,
  status: transferStatus,
  confirmations: int,
  /** Pending with the confirmations it needs: the chain has not finalised its block yet. */
  awaiting_finality: bool,
  seen_at: time,
  confirmed_at: nullable(time),
  orphaned_at: nullable(time),
})
export type Transfer = Infer<typeof transfer>

export const event = obj({
  seq: int,
  id: str,
  type: eventType,
  payment_id: nullable(str),
  created_at: time,
  data: json,
})
export type ApiEvent = Infer<typeof event>

export const events = obj({ data: arr(event), next: int })

export const attempt = obj({
  n: int,
  at: time,
  response_code: nullable(int),
  duration_ms: int,
  error: str,
  succeeded: bool,
  late: bool,
})
export type DeliveryAttempt = Infer<typeof attempt>

export const delivery = obj({
  id: str,
  endpoint_id: str,
  event_id: str,
  event_type: eventType,
  payment_id: nullable(str),
  url: str,
  status: deliveryStatus,
  attempt: int,
  max_attempts: int,
  next_attempt_at: nullable(time),
  request_body: str,
  response_code: nullable(int),
  response_body: str,
  duration_ms: int,
  error: str,
  attempts: arr(attempt),
  created_at: time,
  updated_at: time,
  completed_at: nullable(time),
})
export type Delivery = Infer<typeof delivery>
export const deliveryList = obj({ data: arr(delivery), page: optional(page) })

export const paymentDetail = obj({
  payment,
  transfers: arr(transfer),
  timeline: arr(event),
  deliveries: arr(delivery),
})
export type PaymentDetail = Infer<typeof paymentDetail>

export const periodTotals = obj({ volume: amount, paid: int, created: int, conversion_rate: num, average_ticket: amount })
export type PeriodTotals = Infer<typeof periodTotals>
export const dayPoint = obj({ date: str, volume: amount, paid: int, created: int })
export type DayPoint = Infer<typeof dayPoint>
export const overview = obj({
  days: int,
  asset: str,
  decimals: int,
  current: periodTotals,
  previous: periodTotals,
  series: arr(dayPoint),
  open_payments: int,
  needs_attention: int,
})
export type Overview = Infer<typeof overview>

/* ----------------------------- public checkout ----------------------------- */

export const merchant = obj({ name: str, logo_url: str, support_email: str, support_url: str })
export type Merchant = Infer<typeof merchant>

export const checkout = obj({
  id: str,
  status: paymentStatus,
  order_id: str,
  description: str,
  amount,
  amount_received: amount,
  amount_due: amount,
  asset: str,
  network: str,
  chain_id: int,
  contract: str,
  decimals: int,
  address: str,
  payment_uri: str,
  tx_hash: nullable(str),
  confirmations: int,
  required_confirmations: int,
  awaiting_finality: nullable(finalityWait),
  redirect_url: str,
  created_at: time,
  expires_at: time,
  paid_at: nullable(time),
  cancelled_at: nullable(time),
  merchant,
  /** How many events the payment has had: of two views, the one with the higher version is newer. */
  version: int,
})
export type Checkout = Infer<typeof checkout>

export const linkCheckout = obj({
  slug: str,
  name: str,
  description: str,
  amount: nullable(amount),
  min_amount: nullable(amount),
  asset: str,
  network: str,
  decimals: int,
  status: linkStatus,
  merchant,
})
export type LinkCheckout = Infer<typeof linkCheckout>

/* ------------------------------ payment links ------------------------------ */

export const link = obj({
  id: str,
  slug: str,
  name: str,
  description: str,
  amount: nullable(amount),
  min_amount: nullable(amount),
  asset: str,
  network: str,
  active: bool,
  status: linkStatus,
  uses: int,
  max_uses: nullable(int),
  total_collected: amount,
  expires_at: nullable(time),
  redirect_url: str,
  last_paid_at: nullable(time),
  url: str,
  created_at: time,
  updated_at: time,
})
export type PaymentLink = Infer<typeof link>
export const linkList = obj({ data: arr(link), page: optional(page) })

/* -------------------------------- webhooks -------------------------------- */

export const endpoint = obj({
  id: str,
  url: str,
  description: str,
  events: arr(eventType),
  enabled: bool,
  secret: str,
  secret_masked: bool,
  previous_secret_expires_at: nullable(time),
  disabled_at: nullable(time),
  disabled_reason: str,
  failing_since: nullable(time),
  consecutive_failures: int,
  disable_at: nullable(time),
  last_delivery_at: nullable(time),
  last_success_at: nullable(time),
  succeeded_7d: optional(int),
  failed_7d: optional(int),
  success_rate: optional(num),
  created_at: time,
  updated_at: time,
})
export type WebhookEndpoint = Infer<typeof endpoint>
export const endpointList = obj({ data: arr(endpoint), page: optional(page) })

export const secret = obj({ secret: str, previous_secret: nullable(str), previous_secret_expires_at: nullable(time) })
export type WebhookSecret = Infer<typeof secret>

/* -------------------------------- API keys -------------------------------- */

export const apiKey = obj({
  id: str,
  name: str,
  mode: str,
  prefix: str,
  last4: str,
  scopes: arr(scope),
  created_at: time,
  last_used_at: nullable(time),
  expires_at: nullable(time),
  revoked_at: nullable(time),
  /** Present only in the response that created the key. */
  secret: optional(str),
})
export type ApiKey = Infer<typeof apiKey>
export const apiKeyList = obj({ data: arr(apiKey), page: optional(page) })

/* -------------------------------- settings -------------------------------- */

export const asset = obj({ symbol: str, network: str, chain_id: int, contract: str, decimals: int })
export type Asset = Infer<typeof asset>

export const paymentSettings = obj({
  ttl_seconds: int,
  required_confirmations: int,
  underpayment_tolerance: amount,
  default_redirect_url: str,
})
export type PaymentSettings = Infer<typeof paymentSettings>

export const settings = obj({
  setup_completed: bool,
  public_url: str,
  merchant,
  payments: paymentSettings,
  /**
   * warnings: what the operator should know although the endpoints are valid (endpoints that
   * look like one provider: hosts sharing a registrable domain). problems: what makes stored
   * settings unusable by the rules new ones are held to (settings an earlier version accepted),
   * each with its field ("rpc_urls.1"); while there are any, no chain watcher runs and no payment
   * is created (network_invalid). Both never null.
   */
  network: obj({ chain_id: int, testnet: bool, rpc_urls: arr(str), warnings: arr(str), problems: arr(fieldError) }),
  asset,
  wallet: obj({ xpub: str, derivation_path: str, next_address_index: int }),
  updated_at: nullable(time),
})
export type Settings = Infer<typeof settings>

/* --------------------------------- account --------------------------------- */

export const operator = obj({ id: str, email: str, created_at: optional(time), updated_at: optional(time) })
export type Operator = Infer<typeof operator>
export const session = obj({ id: str, expires_at: time })

export const login = obj({ operator, session, token: optional(str) })
export type LoginResponse = Infer<typeof login>

export const me = obj({
  kind: oneOf("operator", "api_key", "system"),
  operator: optional(operator),
  session: optional(session),
  api_key: optional(obj({ id: str, name: str, scopes: arr(scope) })),
})
export type Me = Infer<typeof me>

export const sessionInfo = obj({
  id: str,
  user_agent: str,
  ip: str,
  created_at: time,
  expires_at: time,
  last_seen_at: nullable(time),
  current: bool,
})
export type SessionInfo = Infer<typeof sessionInfo>
export const sessionList = obj({ data: arr(sessionInfo), page: optional(page) })

/** GET /api/v1/instance: which build is running (authenticated callers only). */
export const instance = obj({ version: str, commit: str })
export type Instance = Infer<typeof instance>

/* ---------------------------------- chain ---------------------------------- */

export const endpointState = oneOf("healthy", "not_checked", "unverified", "failing", "lagging", "wrong_chain", "unsupported", "idle")
export type EndpointState = Infer<typeof endpointState>

export const endpointStatus = obj({
  url: str,
  healthy: bool,
  in_use: bool,
  state: endpointState,
  latency_ms: int,
  block_number: int,
  error: str,
})
export type EndpointStatus = Infer<typeof endpointStatus>

export const nodeStatus = obj({
  healthy: bool,
  starting: bool,
  /**
   * No payment is open and no transfer waits for confirmations: the watcher looks at the chain at
   * its idle interval instead of every few seconds; head and lag show that pause, not a fault.
   */
  idle: bool,
  /**
   * The kind of idle: true while the watch list holds only addresses of closed payments, which
   * every idle pass still scans for late funds; false while there is nothing to scan for. Never
   * true while idle is false.
   */
  quiet: bool,
  /**
   * checked: null while every block up to the head has been scanned by two log endpoints;
   * otherwise a time no later than the oldest block still waiting for a second endpoint's scan.
   */
  /**
   * final: the newest block that is final — finalised by the chain as two endpoints attest. It never
   * goes back, and can be above number for a while (the processed head went back). A transfer counts
   * towards paid only at or below it.
   */
  head: nullable(obj({ chain_id: int, number: int, hash: str, time: nullable(time), checked: nullable(time), final: int, observed_at: nullable(time) })),
  latest_block: int,
  lag_blocks: int,
  rpc_latency_ms: int,
  last_checked_at: nullable(time),
  endpoints: arr(endpointStatus),
  primary: str,
  verifier: str,
  log_verifier: str,
  solo: bool,
  logs_unchecked: bool,
  hold: str,
  /** What the watcher is busy with when it is not at rest; "" at rest. */
  work: str,
  warnings: arr(str),
  watched_addresses: int,
  watched_link_addresses: int,
  max_watched_addresses: int,
  watch_list_full: bool,
  /**
   * Not empty while the stored network settings cannot be used: no chain watcher runs and no
   * payment is created until they are repaired (the same text heads `warnings`).
   */
  network_problem: str,
  /**
   * Payments that have their confirmations and wait for the chain to finalise block `block` (the
   * lowest they wait for); `final` is the newest final block. Normal while brief; a "finality
   * stalled: …" warning says when it has stopped moving. Null while nothing waits.
   */
  awaiting_finality: nullable(obj({ payments: int, block: int, final: int })),
  /** Groups of configured endpoints (scheme and host) that look like one provider. Never null. */
  same_provider: arr(arr(str)),
  /**
   * Settled payments' addresses kept on the watch list past their window until blocks of it that
   * were settled on one endpoint's logs are scanned by a second endpoint (normal while it lasts).
   */
  retained_unchecked: int,
  /**
   * Addresses let go before that second scan because too many were waiting for it: a payment
   * hidden from the logs for one of them is not found. Above zero, the operator has to act.
   */
  released_unchecked: int,
})
export type NodeStatus = Infer<typeof nodeStatus>

/* ---------------------------------- setup ---------------------------------- */

export const setupStatus = obj({
  completed: bool,
  setup_token_required: bool,
  min_password_length: int,
  defaults: obj({ chain_id: int, asset, derivation_path: str, payments: paymentSettings }),
  supported_chains: arr(obj({ chain_id: int, testnet: bool, asset })),
})
export type SetupStatus = Infer<typeof setupStatus>

export const probeResult = obj({
  outcome: probeOutcome,
  expected: asset,
  facts: obj({
    reachable: bool,
    chain_id: int,
    block_number: int,
    latency_ms: int,
    token_found: bool,
    token_decimals: int,
    token_symbol: str,
    error: str,
  }),
})
export type ProbeResult = Infer<typeof probeResult>

export const derivedAddress = obj({ index: int, path: str, address: str })
export type DerivedAddress = Infer<typeof derivedAddress>
export const derivedAddressList = obj({ data: arr(derivedAddress), page: optional(page) })
