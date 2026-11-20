import { formatDistanceStrict } from "date-fns"

/**
 * Text formatting for dates, hashes and chart axes. Money is not formatted here: amounts are
 * exact decimal strings and go through lib/money.ts and the `<Amount>` component.
 */

/** Compact number for chart axes only: 12345 -> "12.3K". Never for an amount someone relies on. */
export function formatCompact(value: number): string {
  return new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 }).format(value)
}

/** Shorten the middle of an address or hash: 0x7A3f…1b92 */
export function truncateMiddle(value: string, start = 6, end = 4): string {
  if (value.length <= start + end + 1) return value
  return `${value.slice(0, start)}…${value.slice(-end)}`
}

/**
 * "5 minutes ago" / "in 24 minutes", relative to `now` (milliseconds; take it from `useNow()` so
 * the text keeps itself current and rendering stays pure).
 */
export function relativeTime(date: string | number | Date, now: number): string {
  return formatDistanceStrict(new Date(date), new Date(now), { addSuffix: true })
}

const dateTimeFormatter = new Intl.DateTimeFormat("en-US", {
  year: "numeric",
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
  timeZone: "UTC",
})

const dateFormatter = new Intl.DateTimeFormat("en-US", {
  year: "numeric",
  month: "short",
  day: "numeric",
  timeZone: "UTC",
})

/** "Nov 18, 2026, 11:54 UTC". Fixed to UTC to stay hydration-safe. */
export function formatDateTime(date: string | number | Date): string {
  return `${dateTimeFormatter.format(new Date(date))} UTC`
}

const timeFormatter = new Intl.DateTimeFormat("en-US", {
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
  timeZone: "UTC",
})

/** "11:54 UTC". Fixed to UTC to stay hydration-safe. */
export function formatTime(date: string | number | Date): string {
  return `${timeFormatter.format(new Date(date))} UTC`
}

/** "Nov 18, 2026" (UTC). */
export function formatDate(date: string | number | Date): string {
  return dateFormatter.format(new Date(date))
}

/** Block explorers of the chains hmnpay supports. */
const EXPLORERS: Record<number, string> = {
  56: "https://bscscan.com",
  97: "https://testnet.bscscan.com",
}

/** Explorer base URL of a chain, or null for one without a public explorer. */
export const explorerUrl = (chainId: number): string | null => EXPLORERS[chainId] ?? null
export const explorerTxUrl = (hash: string, chainId = 56) => `${EXPLORERS[chainId] ?? EXPLORERS[56]}/tx/${hash}`
export const explorerAddressUrl = (address: string, chainId = 56) => `${EXPLORERS[chainId] ?? EXPLORERS[56]}/address/${address}`

const shortFormatter = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
  timeZone: "UTC",
})

const stampFormatter = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
  timeZone: "UTC",
})

const dayFormatter = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", timeZone: "UTC" })

/** "Nov 18, 11:55" (UTC) — table cells. */
export const formatShort = (date: string | number | Date) => shortFormatter.format(new Date(date))
/** "Nov 18, 11:55:04" (UTC) — event logs, where seconds matter. */
export const formatStamp = (date: string | number | Date) => stampFormatter.format(new Date(date))
/** "Nov 18" (UTC). */
export const formatDay = (date: string | number | Date) => dayFormatter.format(new Date(date))

/** "4 min 12 s", "38 s", "2 h 05 min". */
export function formatDuration(ms: number) {
  const total = Math.max(0, Math.round(ms / 1000))
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  if (h > 0) return `${h} h ${String(m).padStart(2, "0")} min`
  if (m > 0) return `${m} min ${String(s).padStart(2, "0")} s`
  return `${s} s`
}
