/**
 * What time it is on the server.
 *
 * A payment's deadline is a moment on the server's clock. The checkout counts down to it on the
 * payer's device, whose clock may be minutes off, so the page takes the difference from the
 * `Date` header of the server's own answers and counts with that. The header has whole seconds
 * and the answer took a moment to arrive: the estimate is good to about a second, and a
 * difference of less than two seconds is treated as none.
 */

let offsetMs = 0

/** The server's clock minus this device's, from one answer's `Date` header; null if the header says nothing usable. */
export function clockOffset(dateHeader: string | null | undefined, localNowMs: number): number | null {
  if (!dateHeader) return null
  const server = Date.parse(dateHeader)
  if (!Number.isFinite(server)) return null
  // The header is cut to the second: the middle of that second is the best guess.
  const offset = server + 500 - localNowMs
  return Math.abs(offset) < 2000 ? 0 : Math.round(offset)
}

/** Takes the server's time from an answer. Called for every answer of the API. */
export function noteServerDate(dateHeader: string | null | undefined) {
  const offset = clockOffset(dateHeader, Date.now())
  if (offset !== null) offsetMs = offset
}

/** Milliseconds since the epoch by the server's clock, as well as this page can tell. */
export function serverNow(): number {
  return Date.now() + offsetMs
}

/** A moment of this device's clock on the server's. */
export function toServerTime(localMs: number): number {
  return localMs + offsetMs
}
