/**
 * API errors in the product's voice.
 *
 * The server's messages are written for developers reading a response body ("amount must be
 * positive"). Screens show sentences instead: `describeError` for a toast or a callout,
 * `fieldErrors` to put the server's per-field reasons under the form fields they belong to.
 */
import { ApiError, isApiError } from "@/lib/api/client"

import { CHAIN_NOT_READY, CHAIN_NOT_READY_OPERATOR, CLOCK_BEHIND, CLOCK_BEHIND_OPERATOR, NETWORK_INVALID, NETWORK_INVALID_OPERATOR } from "./chain-ready"

function wait(seconds: number | undefined) {
  if (!seconds) return "a moment"
  if (seconds < 90) return `${seconds} second${seconds === 1 ? "" : "s"}`
  const minutes = Math.ceil(seconds / 60)
  return `${minutes} minutes`
}

const sentence = (text: string) => {
  const trimmed = text.trim()
  if (!trimmed) return ""
  const capitalised = trimmed[0].toUpperCase() + trimmed.slice(1)
  return /[.!?]$/.test(capitalised) ? capitalised : `${capitalised}.`
}

/** Field error codes said in the product's words rather than the server's. */
const FIELD_BY_CODE: Record<string, string> = {
  same_endpoint:
    "This is the same endpoint as another one in the list: only the path or the key differs, so it can’t check that endpoint’s answers. Use an endpoint of another provider.",
}

/** Codes whose meaning the interface knows better than the server's generic message. */
const BY_CODE: Record<string, (error: ApiError) => string> = {
  network: () => "Can’t reach the server. Check that hmnpay is running and that you are online.",
  rate_limited: (e) => `Too many requests. Try again in ${wait(e.retryAfter)}.`,
  too_many_streams: () => "Too many live connections from this network. The page keeps itself up to date by checking instead.",
  busy: (e) => `The server is busy. Try again in ${wait(e.retryAfter)}.`,
  unauthorized: () => "You are signed out. Sign in again.",
  invalid_credentials: () => "That email and password don’t match.",
  credentials_expired: () => "Your session has expired. Sign in again.",
  credentials_revoked: () => "This session was signed out. Sign in again.",
  recovery_code_invalid: () => "That recovery code is wrong, has expired or was already used. Run “hmnpay recovery-code” on the server for a new one.",
  csrf_rejected: () => "The request was refused because it did not come from this site. Reload the page and try again.",
  host_not_allowed: () =>
    "The server does not answer for this address. Open hmnpay through its public URL, or add this host name to HMNPAY_ALLOWED_HOSTS.",
  setup_token_required: () => "Enter the setup token from the server log.",
  setup_token_invalid: () => "That setup token is not the one this server printed. Check the server log for the current one.",
  setup_completed: () => "This instance is already set up. Sign in instead.",
  setup_required: () => "This instance has not been set up yet.",
  payment_has_funds: () => "Funds have already arrived for this payment, so it can’t be cancelled any more.",
  payment_settled: () => "This payment is already settled; there is nothing left to cancel.",
  nothing_to_resolve: () => "Someone already changed this. Reload to see where it stands.",
  link_disabled: () => "This payment link is switched off.",
  link_expired: () => "This payment link has expired.",
  link_limit_reached: () => "This payment link has been used up.",
  watch_list_full: () => "Too many payments are open right now. Try again later.",
  [CHAIN_NOT_READY]: () => CHAIN_NOT_READY_OPERATOR.short,
  [CLOCK_BEHIND]: () => CLOCK_BEHIND_OPERATOR.short,
  [NETWORK_INVALID]: () => NETWORK_INVALID_OPERATOR.short,
  idempotency_key_reused: () => "This was already submitted with different details. Reload and try again.",
  endpoint_disabled: () => "This endpoint is disabled. Enable it first.",
  open_payments: () => "Payments are still open on the current network. Wait for them to finish, or cancel them, before switching.",
  body_too_large: () => "That is too large to send. If you attached a logo, use a smaller image.",
  internal: (e) => `Something went wrong on the server${e.requestId ? ` (request ${e.requestId})` : ""}. The server log has the details.`,
  not_found: () => "That no longer exists.",
  contract: () => "The server answered in a shape this page does not understand. Reload; if it persists, the interface and the server are different versions.",
}

/** One sentence (occasionally two) for a person, for any thrown value. */
export function describeError(error: unknown): string {
  if (!isApiError(error)) return "Something went wrong. Try again."
  const known = BY_CODE[error.code]
  if (known) return known(error)
  if (error.status === 429) return BY_CODE.rate_limited(error)
  if (error.status >= 500) return BY_CODE.internal(error)
  if (error.code === "validation_failed" && error.fields.length > 0) {
    return error.fields.length === 1 ? sentence(error.fields[0].message) : "Some fields need another look."
  }
  return sentence(error.message) || "Something went wrong. Try again."
}

/**
 * The server's field errors as `{ field: sentence }`. `rename` maps request keys onto the
 * form's own field names ("merchant_name" → "name"); indexed keys ("rpc_urls.1") stay as they
 * are unless renamed.
 */
export function fieldErrors(error: unknown, rename: Record<string, string> = {}): Record<string, string> {
  if (!isApiError(error)) return {}
  const out: Record<string, string> = {}
  for (const field of error.fields) {
    const name = rename[field.field] ?? field.field
    if (!(name in out)) out[name] = FIELD_BY_CODE[field.code] ?? sentence(field.message)
  }
  return out
}

/** Whether an error is the server saying "slow down". */
export const isRateLimited = (error: unknown): error is ApiError => isApiError(error) && error.status === 429

/** Whether the server cannot create payments yet: its chain watcher has not processed a block. */
export const isChainNotReady = (error: unknown): error is ApiError => isApiError(error) && error.code === CHAIN_NOT_READY

/** Whether the server cannot create payments because its clock is behind the chain. */
export const isClockBehindError = (error: unknown): error is ApiError => isApiError(error) && error.code === CLOCK_BEHIND

/** Whether the server cannot create payments because its stored network settings can't be used. */
export const isNetworkInvalidError = (error: unknown): error is ApiError => isApiError(error) && error.code === NETWORK_INVALID

/** Whether the thing asked for does not exist (or is not ours to see). */
export const isNotFound = (error: unknown): error is ApiError => isApiError(error) && error.status === 404
