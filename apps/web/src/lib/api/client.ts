/**
 * The hmnpay API client for the browser.
 *
 * Same origin, cookie session: the server sets an HttpOnly session cookie on login and on setup,
 * and every call here just sends it along (`credentials: "same-origin"`). Nothing is kept in
 * localStorage or in JavaScript at all — a script cannot read the session, and there is no token
 * to leak. The server's cross-origin protection accepts these calls because they are
 * same-origin; nothing needs to be added for CSRF.
 *
 * Errors come back as `ApiError` with the server's code, message and field errors
 * (lib/errors.ts turns them into sentences and form errors). A 401 on a dashboard call means the
 * session is gone: the registered handler sends the operator to the login page.
 */
import { noteServerDate } from "@/lib/server-clock"

import { check, type Schema } from "./schema"
import * as t from "./types"

const BASE = "/api/v1"

export class ApiError extends Error {
  /** HTTP status; 0 when the request never got an answer (offline, server down). */
  readonly status: number
  /** The server's machine-readable code ("validation_failed", "rate_limited", …) or "network". */
  readonly code: string
  readonly fields: t.FieldError[]
  /** Seconds to wait before trying again (Retry-After), if the server said so. */
  readonly retryAfter?: number
  readonly requestId?: string

  constructor(init: { status: number; code: string; message: string; fields?: t.FieldError[]; retryAfter?: number; requestId?: string }) {
    super(init.message)
    this.name = "ApiError"
    this.status = init.status
    this.code = init.code
    this.fields = init.fields ?? []
    this.retryAfter = init.retryAfter
    this.requestId = init.requestId
  }
}

export const isApiError = (error: unknown): error is ApiError => error instanceof ApiError

/* ------------------------------- validation -------------------------------- */

declare global {
  interface Window {
    /** Set by the end-to-end tests: check every response against its schema and fail loudly. */
    __HMNPAY_STRICT_API__?: boolean
    /** Schema violations seen so far (strict mode); the tests assert it stays empty. */
    __hmnpayApiDrift?: string[]
  }
}

/**
 * Responses are checked against their schema while developing and in the end-to-end tests, not
 * in production: the interface and the API are built into one binary, so they cannot be of
 * different versions there, and a harmless extra field must never break a page.
 */
function validation(): "strict" | "warn" | "off" {
  if (typeof window !== "undefined" && window.__HMNPAY_STRICT_API__) return "strict"
  return process.env.NODE_ENV === "production" ? "off" : "warn"
}

function validated<T>(schema: Schema<T>, value: unknown, label: string): T {
  const mode = validation()
  if (mode === "off") return value as T
  const issues = check(schema, value, label)
  if (issues.length > 0) {
    const text = `API response does not match the client's types:\n${issues.slice(0, 20).join("\n")}`
    if (mode === "strict") {
      ;(window.__hmnpayApiDrift ??= []).push(...issues)
      throw new ApiError({ status: 200, code: "contract", message: text })
    }
    console.warn(text)
  }
  return value as T
}

/* --------------------------------- request --------------------------------- */

let onUnauthorized: (() => void) | null = null

/** The dashboard registers what to do when its session has ended (go to the login page). */
export function setUnauthorizedHandler(handler: (() => void) | null) {
  onUnauthorized = handler
}

interface RequestOptions {
  method?: "GET" | "POST" | "PUT" | "DELETE"
  body?: unknown
  query?: Record<string, string | number | boolean | undefined | null>
  headers?: Record<string, string>
  signal?: AbortSignal
  /** A dashboard call: a 401 means the session ended. Public and login calls leave it off. */
  session?: boolean
}

function buildUrl(path: string, query?: RequestOptions["query"]) {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query ?? {})) {
    if (value !== undefined && value !== null && value !== "") params.set(key, String(value))
  }
  const text = params.toString()
  return `${BASE}${path}${text ? `?${text}` : ""}`
}

function retryAfterSeconds(response: Response): number | undefined {
  const raw = response.headers.get("Retry-After")
  if (!raw) return undefined
  const seconds = Number(raw)
  return Number.isFinite(seconds) && seconds >= 0 ? Math.ceil(seconds) : undefined
}

async function toError(response: Response): Promise<ApiError> {
  let code = `http_${response.status}`
  let message = response.statusText || "Request failed"
  let fields: t.FieldError[] | undefined
  let requestId = response.headers.get("X-Request-Id") ?? undefined
  try {
    const body: unknown = await response.json()
    if (check(t.errorBody, body).length === 0) {
      const parsed = body as { error: { code: string; message: string; fields?: t.FieldError[] }; request_id: string }
      code = parsed.error.code
      message = parsed.error.message
      fields = parsed.error.fields
      requestId = parsed.request_id || requestId
    }
  } catch {
    // Not JSON (a proxy's error page): keep the status line.
  }
  return new ApiError({ status: response.status, code, message, fields, retryAfter: retryAfterSeconds(response), requestId })
}

async function send(path: string, options: RequestOptions): Promise<Response> {
  const headers: Record<string, string> = { Accept: "application/json", ...options.headers }
  let body: string | undefined
  if (options.body !== undefined) {
    headers["Content-Type"] = "application/json"
    body = JSON.stringify(options.body)
  }
  let response: Response
  try {
    response = await fetch(buildUrl(path, options.query), {
      method: options.method ?? "GET",
      headers,
      body,
      credentials: "same-origin",
      cache: "no-store",
      redirect: "error",
      signal: options.signal,
    })
  } catch (cause) {
    if (cause instanceof DOMException && cause.name === "AbortError") throw cause
    throw new ApiError({ status: 0, code: "network", message: "The server could not be reached." })
  }
  noteServerDate(response.headers.get("Date"))
  if (!response.ok) {
    const error = await toError(response)
    if (error.status === 401 && options.session) onUnauthorized?.()
    throw error
  }
  return response
}

async function request<T>(schema: Schema<T>, path: string, options: RequestOptions = {}): Promise<T> {
  const response = await send(path, options)
  let value: unknown
  try {
    value = await response.json()
  } catch {
    throw new ApiError({ status: response.status, code: "bad_response", message: "The server sent something that is not JSON." })
  }
  return validated(schema, value, `${options.method ?? "GET"} ${path}`)
}

/** For endpoints that answer 204 No Content. */
async function requestEmpty(path: string, options: RequestOptions = {}): Promise<void> {
  await send(path, options)
}

const session = <O extends RequestOptions>(options?: O) => ({ ...options, session: true }) as RequestOptions

/** A fresh idempotency key: a retried create returns the payment the first try made. */
export function idempotencyKey(): string {
  const bytes = new Uint8Array(16)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("")
}

/* -------------------------------- endpoints -------------------------------- */

export interface PaymentListQuery {
  view?: "all" | "open" | "paid" | "attention" | "expired"
  q?: string
  created_from?: string
  created_to?: string
  link_id?: string
  sort?: "created" | "amount"
  dir?: "asc" | "desc"
  limit?: number
  offset?: number
  counts?: boolean
}

export interface CreatePaymentInput {
  amount: string
  order_id?: string
  description?: string
  customer_email?: string
  redirect_url?: string
  metadata?: Record<string, string>
  ttl_seconds?: number
}

export interface LinkInput {
  slug?: string
  name: string
  description: string
  amount: string | null
  min_amount: string | null
  max_uses: number | null
  expires_at: string | null
  redirect_url: string
  active: boolean
}

export interface EndpointInput {
  url: string
  description: string
  events: t.EventType[]
  enabled: boolean
}

export interface SetupInput {
  email: string
  password: string
  chain_id: number
  rpc_urls: string[]
  xpub: string
  derivation_path: string
  merchant_name: string
  logo_url?: string
  support_email: string
  support_url?: string
}

/** The setup token travels in a header, never in a URL. */
const setupHeaders = (token: string | undefined): Record<string, string> => (token ? { "X-Hmnpay-Setup-Token": token } : {})

export const api = {
  /* session */
  login: (input: { email: string; password: string; recovery_code?: string }, signal?: AbortSignal) =>
    request(t.login, "/auth/login", { method: "POST", body: input, signal }),
  logout: () => requestEmpty("/auth/logout", { method: "POST" }),
  me: (signal?: AbortSignal) => request(t.me, "/auth/me", { signal }),
  instance: (signal?: AbortSignal) => request(t.instance, "/instance", session({ signal })),

  /* first-run setup */
  setupStatus: (signal?: AbortSignal) => request(t.setupStatus, "/setup", { signal }),
  probeRpc: (input: { url: string; chain_id: number }, setupToken?: string, signal?: AbortSignal) =>
    request(t.probeResult, "/setup/probe-rpc", { method: "POST", body: input, headers: setupHeaders(setupToken), signal }),
  previewAddresses: (input: { xpub: string; derivation_path: string; count: number }, setupToken?: string, signal?: AbortSignal) =>
    request(t.derivedAddressList, "/setup/preview-addresses", { method: "POST", body: input, headers: setupHeaders(setupToken), signal }),
  completeSetup: (input: SetupInput, setupToken: string) =>
    request(t.login, "/setup/complete", { method: "POST", body: input, headers: setupHeaders(setupToken) }),

  /* payments */
  payments: (query: PaymentListQuery, signal?: AbortSignal) => request(t.paymentList, "/payments", session({ query: { ...query }, signal })),
  payment: (id: string, signal?: AbortSignal) => request(t.payment, `/payments/${encodeURIComponent(id)}`, session({ signal })),
  paymentDetail: (id: string, signal?: AbortSignal) =>
    request(t.paymentDetail, `/payments/${encodeURIComponent(id)}/detail`, session({ signal })),
  createPayment: (input: CreatePaymentInput, key: string) =>
    request(t.payment, "/payments", session({ method: "POST", body: input, headers: { "Idempotency-Key": key } })),
  cancelPayment: (id: string) => request(t.payment, `/payments/${encodeURIComponent(id)}/cancel`, session({ method: "POST" })),
  resolvePayment: (id: string, note: string) =>
    request(t.payment, `/payments/${encodeURIComponent(id)}/resolve`, session({ method: "POST", body: { note } })),
  reopenPayment: (id: string) => request(t.payment, `/payments/${encodeURIComponent(id)}/reopen`, session({ method: "POST" })),
  overview: (days: number, signal?: AbortSignal) => request(t.overview, "/overview", session({ query: { days }, signal })),
  nodeStatus: (signal?: AbortSignal) => request(t.nodeStatus, "/node-status", session({ signal })),
  events: (query: { after: number; limit?: number; payment_id?: string }, signal?: AbortSignal) =>
    request(t.events, "/events", session({ query, signal })),

  /* payment links */
  links: (signal?: AbortSignal) => request(t.linkList, "/payment-links", session({ signal })),
  createLink: (input: LinkInput) => request(t.link, "/payment-links", session({ method: "POST", body: input })),
  updateLink: (id: string, input: LinkInput) =>
    request(t.link, `/payment-links/${encodeURIComponent(id)}`, session({ method: "PUT", body: input })),
  deleteLink: (id: string) => requestEmpty(`/payment-links/${encodeURIComponent(id)}`, session({ method: "DELETE" })),
  setLinkActive: (id: string, active: boolean) =>
    request(t.link, `/payment-links/${encodeURIComponent(id)}/${active ? "activate" : "deactivate"}`, session({ method: "POST" })),

  /* webhooks */
  endpoints: (signal?: AbortSignal) => request(t.endpointList, "/webhook-endpoints", session({ signal })),
  createEndpoint: (input: EndpointInput) => request(t.endpoint, "/webhook-endpoints", session({ method: "POST", body: input })),
  updateEndpoint: (id: string, input: EndpointInput) =>
    request(t.endpoint, `/webhook-endpoints/${encodeURIComponent(id)}`, session({ method: "PUT", body: input })),
  deleteEndpoint: (id: string) => requestEmpty(`/webhook-endpoints/${encodeURIComponent(id)}`, session({ method: "DELETE" })),
  rotateSecret: (id: string) => request(t.endpoint, `/webhook-endpoints/${encodeURIComponent(id)}/rotate-secret`, session({ method: "POST" })),
  revealSecret: (id: string) => request(t.secret, `/webhook-endpoints/${encodeURIComponent(id)}/reveal-secret`, session({ method: "POST" })),
  testEndpoint: (id: string) => request(t.delivery, `/webhook-endpoints/${encodeURIComponent(id)}/test`, session({ method: "POST" })),
  deliveries: (
    query: { endpoint_id?: string; payment_id?: string; status?: string; limit?: number; offset?: number },
    signal?: AbortSignal,
  ) => request(t.deliveryList, "/webhook-deliveries", session({ query, signal })),
  delivery: (id: string, signal?: AbortSignal) => request(t.delivery, `/webhook-deliveries/${encodeURIComponent(id)}`, session({ signal })),
  resendDelivery: (id: string) => request(t.delivery, `/webhook-deliveries/${encodeURIComponent(id)}/resend`, session({ method: "POST" })),

  /* API keys */
  apiKeys: (signal?: AbortSignal) => request(t.apiKeyList, "/api-keys", session({ signal })),
  createApiKey: (input: { name: string; scopes: t.ApiKeyScope[]; expires_at: string | null }) =>
    request(t.apiKey, "/api-keys", session({ method: "POST", body: input })),
  revokeApiKey: (id: string) => request(t.apiKey, `/api-keys/${encodeURIComponent(id)}/revoke`, session({ method: "POST" })),

  /* settings and the operator account */
  settings: (signal?: AbortSignal) => request(t.settings, "/settings", session({ signal })),
  updateMerchant: (input: { merchant_name: string; logo_url: string; support_email: string; support_url: string; public_url: string }) =>
    request(t.settings, "/settings/merchant", session({ method: "PUT", body: input })),
  updatePaymentDefaults: (input: t.PaymentSettings) => request(t.settings, "/settings/payments", session({ method: "PUT", body: input })),
  updateNetwork: (input: { chain_id: number; rpc_urls: string[] }) =>
    request(t.settings, "/settings/network", session({ method: "PUT", body: input })),
  updateWallet: (input: { xpub: string; derivation_path: string; next_address_index?: number }) =>
    request(t.settings, "/settings/wallet", session({ method: "PUT", body: input })),
  operator: (signal?: AbortSignal) => request(t.operator, "/operator", session({ signal })),
  changePassword: (input: { current_password: string; new_password: string }) =>
    requestEmpty("/operator/password", session({ method: "POST", body: input })),
  sessions: (signal?: AbortSignal) => request(t.sessionList, "/operator/sessions", session({ signal })),
  revokeSession: (id: string) => requestEmpty(`/operator/sessions/${encodeURIComponent(id)}`, session({ method: "DELETE" })),

  /* public checkout (no credentials: the id or slug is the capability) */
  checkout: (id: string, signal?: AbortSignal) => request(t.checkout, `/public/payments/${encodeURIComponent(id)}`, { signal }),
  publicLink: (slug: string, signal?: AbortSignal) => request(t.linkCheckout, `/public/links/${encodeURIComponent(slug)}`, { signal }),
  createLinkPayment: (slug: string, input: { amount?: string; customer_email?: string }, key: string) =>
    request(t.checkout, `/public/links/${encodeURIComponent(slug)}/payments`, {
      method: "POST",
      body: input,
      headers: { "Idempotency-Key": key },
    }),
}

/** Where the checkout's event stream lives (see lib/checkout-stream.ts). */
export const checkoutStreamUrl = (id: string) => `${BASE}/public/payments/${encodeURIComponent(id)}/stream`

/** Validates one decoded stream message. */
export const parseCheckout = (value: unknown) => validated(t.checkout, value, "checkout stream")
