/**
 * Whether the page may still ask the payer to send money, for a payment the server last
 * reported as open (awaiting, or partly paid).
 *
 * The page knows the deadline itself, and it knows when the server last answered. It does not
 * wait for the server to say "expired" before it stops asking: a transfer sent after the
 * deadline does not pay the order and has to be sorted out by hand.
 *
 *  - "open": before the deadline, and the server is answering.
 *  - "unwatched": before the deadline, but the server has not answered for a while. The address
 *    is still good; what the page cannot do is show the payment arriving.
 *  - "closing": the deadline has passed and the server, asked since, still has the payment
 *    open. It is checking for a transfer sent in time (it allows a minute for one to land).
 *  - "unknown": the deadline has passed and the server has not answered since, or stopped
 *    answering. The payment has probably expired; the page cannot tell.
 */
export type Invite = "open" | "unwatched" | "closing" | "unknown"

/** Before the deadline: this long without an answer and the page says it is not watching. */
export const UNWATCHED_AFTER_MS = 45_000
/** After the deadline: an answer older than this no longer counts as the server's word. */
const CONFIRMED_WITHIN_MS = 25_000
/**
 * Right at the deadline the page asks the server (lib/checkout-stream does, in any mode). For
 * this long the answer may still be on its way, and a server that was answering until a moment
 * ago is not yet called unreachable.
 */
const ASKING_FOR_MS = 8_000

/**
 * `contactAt` and `localNow` are moments of this device's clock; `clockOffsetMs` is the server's
 * clock minus this device's (lib/server-clock).
 */
export function inviteAt(expiresAt: string, contactAt: number | undefined, localNow: number, clockOffsetMs = 0): Invite {
  const deadline = Date.parse(expiresAt)
  const silentFor = contactAt === undefined ? Infinity : localNow - contactAt
  if (localNow + clockOffsetMs < deadline) return silentFor > UNWATCHED_AFTER_MS ? "unwatched" : "open"
  const askedSince = contactAt !== undefined && contactAt + clockOffsetMs >= deadline
  if (askedSince) return silentFor <= CONFIRMED_WITHIN_MS ? "closing" : "unknown"
  const overdue = localNow + clockOffsetMs - deadline
  return overdue <= ASKING_FOR_MS && silentFor <= CONFIRMED_WITHIN_MS ? "closing" : "unknown"
}
