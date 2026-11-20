import { api, type PaymentListQuery } from "@/lib/api/client"
import type { Payment, PaymentStatus } from "@/lib/api/types"
import { truncateMiddle } from "@/lib/format"
import { compareAmounts, subtractAmounts } from "@/lib/money"
import { safeHttpUrl, sameSitePath } from "@/lib/url"
import { isPaymentId } from "@/lib/use-route"

export { formatDay, formatDuration, formatShort, formatStamp } from "@/lib/format"

/* -------------------------------------------------------------------------- */
/* Status groups                                                              */
/* -------------------------------------------------------------------------- */

export const OPEN_STATUSES: readonly PaymentStatus[] = ["awaiting", "detected", "confirming"]

export const isOpen = (p: Pick<Payment, "status">) => OPEN_STATUSES.includes(p.status)

/** Signed difference between what arrived and what was asked for, exact ("-21.03", "0.50"). */
export function amountDelta(p: Pick<Payment, "amount" | "amount_received" | "decimals">): string {
  return subtractAmounts(p.amount_received, p.amount, p.decimals)
}

/** The difference without its sign. */
export const amountGap = (p: Pick<Payment, "amount" | "amount_received" | "decimals">) => amountDelta(p).replace(/^-/, "")

export const hasReceived = (p: Pick<Payment, "amount_received" | "decimals">) => compareAmounts(p.amount_received, "0", p.decimals) > 0

/** Funds were confirmed after the payment had already closed. */
export const hasLateFunds = (p: Pick<Payment, "attention">) => p.attention === "late_funds"

/** The payment was cancelled by the merchant (its status is then "expired"). */
export const isCancelled = (p: Pick<Payment, "cancelled_at">) => p.cancelled_at !== null

/** Something a human has to decide on, resolved or not: the server says which. */
export const isIssue = (p: Pick<Payment, "attention">) => p.attention !== ""

/** One plain sentence saying what is wrong. Amounts are returned separately so they can be set in mono. */
export function issueSummary(p: Payment): { lead: string; amount?: string } {
  switch (p.attention) {
    case "underpaid":
      return { lead: "Short by", amount: amountGap(p) }
    case "overpaid":
      return { lead: "Over by", amount: amountGap(p) }
    case "failed":
      return { lead: "The transfer dropped out of the chain" }
    case "late_funds":
      return { lead: "Arrived after it closed:", amount: p.amount_received }
    default:
      return { lead: "" }
  }
}

/* -------------------------------------------------------------------------- */
/* Names and links                                                            */
/* -------------------------------------------------------------------------- */

/** What a payment is called in lists: the merchant's order id, or a short form of its own id. */
export const paymentLabel = (p: Pick<Payment, "id" | "order_id">) => p.order_id || truncateMiddle(p.id, 8, 4)

/** Same page on this instance (what "Open checkout" navigates to from the dashboard). */
export const checkoutPath = (id: string) => `/pay/${id}`
/** The dashboard page of a payment. An id that is not a payment id (the API never sends one) leads to the list. */
export const paymentPath = (id: string) => (isPaymentId(id) ? `/payments/${id}` : "/payments")

/**
 * Public checkout URL the customer opens: the one the API built from the instance's public URL.
 * If that is only a path (no public URL configured), this site's origin is put in front.
 */
export function checkoutUrl(p: Pick<Payment, "id" | "checkout_url">): string {
  const origin = typeof window === "undefined" ? "" : window.location.origin
  // A path of this site gets this site's origin; a full address must be a web address. Anything
  // else the API might say is replaced by the checkout path of the payment's own id.
  const path = sameSitePath(p.checkout_url) ?? (safeHttpUrl(p.checkout_url) ? null : checkoutPath(p.id))
  return path !== null ? origin + path : safeHttpUrl(p.checkout_url)!
}

/* -------------------------------------------------------------------------- */
/* List views, search, sort                                                   */
/* -------------------------------------------------------------------------- */

/** The list's tabs: exactly the API's `view` values. */
export const PAYMENT_VIEWS = [
  { id: "all", label: "All" },
  { id: "open", label: "In progress" },
  { id: "paid", label: "Paid" },
  { id: "attention", label: "Needs attention" },
  { id: "expired", label: "Expired" },
] as const

export type PaymentViewId = (typeof PAYMENT_VIEWS)[number]["id"]

export const isViewId = (value: unknown): value is PaymentViewId =>
  typeof value === "string" && PAYMENT_VIEWS.some((v) => v.id === value)

export const DATE_RANGES = [
  { id: "all", label: "All time", days: null },
  { id: "today", label: "Today", days: 1 },
  { id: "7d", label: "Last 7 days", days: 7 },
  { id: "30d", label: "Last 30 days", days: 30 },
  { id: "90d", label: "Last 90 days", days: 90 },
] as const

export type DateRangeId = (typeof DATE_RANGES)[number]["id"]

export const isDateRangeId = (value: unknown): value is DateRangeId =>
  typeof value === "string" && DATE_RANGES.some((r) => r.id === value)

export interface PaymentQuery {
  view: PaymentViewId
  q: string
  range: DateRangeId
  /** Custom range, YYYY-MM-DD (UTC), inclusive. Wins over `range` when both ends are set. */
  from?: string
  to?: string
  sort: "created" | "amount"
  dir: "asc" | "desc"
  /** Only payments created through this payment link (a link's "View all"). */
  link?: string
}

const isDay = (value: string | null | undefined): value is string => Boolean(value && /^\d{4}-\d{2}-\d{2}$/.test(value))

/** The list state the URL describes. */
export function parsePaymentQuery(params: URLSearchParams): PaymentQuery & { page: number } {
  const from = params.get("from")
  const to = params.get("to")
  const status = params.get("status")
  const range = params.get("range")
  const custom = isDay(from) && isDay(to)
  return {
    view: isViewId(status) ? status : "all",
    q: params.get("q") ?? "",
    range: isDateRangeId(range) ? range : "all",
    from: custom ? from : undefined,
    to: custom ? to : undefined,
    sort: params.get("sort") === "amount" ? "amount" : "created",
    dir: params.get("dir") === "asc" ? "asc" : "desc",
    link: params.get("link") || undefined,
    page: Math.max(1, Math.floor(Number(params.get("page")) || 1)),
  }
}

const utcDay = (ms: number) => new Date(ms).toISOString().slice(0, 10)

/**
 * The API query for a list state. Date presets are UTC days, like the API's own day
 * boundaries: "Today" starts at 00:00 UTC, "Last 7 days" is today and the six days before.
 * `today` is passed in (milliseconds) so this stays a pure function.
 */
export function toApiQuery(query: PaymentQuery, today: number): PaymentListQuery {
  const out: PaymentListQuery = { sort: query.sort, dir: query.dir }
  if (query.view !== "all") out.view = query.view
  if (query.q.trim()) out.q = query.q.trim()
  if (query.link) out.link_id = query.link
  if (query.from && query.to) {
    out.created_from = query.from
    out.created_to = query.to
  } else {
    const range = DATE_RANGES.find((r) => r.id === query.range)
    if (range?.days) out.created_from = utcDay(today - (range.days - 1) * 86_400_000)
  }
  return out
}

/** Build a `/payments?…` href, dropping defaults so URLs stay short. */
export function paymentsHref(query: Partial<PaymentQuery> & { page?: number }, base = "/payments") {
  const params = new URLSearchParams()
  if (query.view && query.view !== "all") params.set("status", query.view)
  if (query.q) params.set("q", query.q)
  if (query.from && query.to) {
    params.set("from", query.from)
    params.set("to", query.to)
  } else if (query.range && query.range !== "all") params.set("range", query.range)
  if (query.sort && !(query.sort === "created" && (query.dir ?? "desc") === "desc")) {
    params.set("sort", query.sort)
    params.set("dir", query.dir ?? "desc")
  }
  if (query.link) params.set("link", query.link)
  if (query.page && query.page > 1) params.set("page", String(query.page))
  const text = params.toString()
  return text ? `${base}?${text}` : base
}

/* -------------------------------------------------------------------------- */
/* Export                                                                     */
/* -------------------------------------------------------------------------- */

/** Rows an export may hold. Beyond it the export says so instead of silently stopping. */
export const EXPORT_LIMIT = 20_000

/** Every payment a list query matches, fetched page by page (the API serves at most 500 at a time). */
export async function fetchAllPayments(query: PaymentListQuery): Promise<{ rows: Payment[]; truncated: boolean }> {
  const rows: Payment[] = []
  for (let offset = 0; ; offset += 500) {
    const page = await api.payments({ ...query, limit: 500, offset })
    rows.push(...page.data)
    if (!page.page.has_more) return { rows, truncated: false }
    if (rows.length >= EXPORT_LIMIT) return { rows, truncated: true }
  }
}

const CSV_COLUMNS = [
  "id",
  "order_id",
  "status",
  "amount",
  "amount_received",
  "amount_confirmed",
  "asset",
  "network",
  "address",
  "tx_hash",
  "from_address",
  "customer_email",
  "attention",
  "created_at",
  "expires_at",
  "paid_at",
  "cancelled_at",
] as const satisfies readonly (keyof Payment)[]

/**
 * CSV of payments, amounts as the exact decimal strings. A cell that a spreadsheet would run as
 * a formula (it starts with = + - @, or a tab or carriage return) is prefixed with an
 * apostrophe: order ids, descriptions and emails are typed by other people.
 */
export function paymentsToCsv(rows: Payment[]) {
  const cell = (value: string | null) => {
    let text = value ?? ""
    if (/^[=+\-@\t\r]/.test(text)) text = `'${text}`
    return /[",\n\r]/.test(text) ? `"${text.replaceAll('"', '""')}"` : text
  }
  const lines = rows.map((p) => CSV_COLUMNS.map((column) => cell(p[column] === null ? null : String(p[column]))).join(","))
  return [CSV_COLUMNS.join(","), ...lines].join("\n")
}
