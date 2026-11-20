/**
 * Decides, for one payment, whether a view that has just arrived may replace the one on screen.
 *
 * A checkout page hears about its payment from two places: answers to its own polls, and
 * messages of the event stream. They travel separately, so they can arrive in an order that is
 * not the order the server produced them in — a poll answer held up for a few seconds on a bad
 * connection arrives after the stream has already said "paid". Applying whatever arrives last
 * would then show a paid payment as waiting for money again. This is the one place that says
 * what is newer.
 *
 * Every request the page makes takes a ticket when it is sent. Two things are known for certain,
 * without trusting any clock:
 *
 *  - an answer was produced by the server after its request was sent. So if nothing has been
 *    applied since the ticket was taken, the answer is newer than everything on screen;
 *  - messages of one stream connection arrive in the order the server sent them. So a message
 *    is newer than the previous message of the same connection.
 *
 * In every other case — something from another source was applied while this one was under
 * way — the order is not known from timing, and the view's own `version` decides: the number of
 * events the payment has had, which the server sends with every view and which only grows.
 *
 *  - a late poll answer is dropped (the page asks again if it still needs to);
 *  - a stream message is applied only if its version is higher, or the same with a
 *    confirmation count that is not lower. Within one version the confirmation count is the
 *    only thing about the payment that changes, and it only counts up while a transfer
 *    confirms; if the chain's head steps back without removing the transfer, the page shows the
 *    higher count for the few seconds until the head has passed it again.
 */

export interface Versioned {
  /** How many events the payment has had. */
  version: number
  confirmations: number
}

export type Source = "poll" | "stream"

/** Taken when a request is sent; handed back with every answer to it. */
export interface Ticket {
  readonly id: number
  readonly source: Source
  /** How many views had been applied when the request was sent. */
  readonly applied: number
}

export class ViewOrder<V extends Versioned> {
  /** The view on screen. */
  current: V | undefined
  private applied = 0
  private last = 0
  private next = 1

  /** A request is about to be sent: one poll, or one stream connection. */
  begin(source: Source): Ticket {
    return { id: this.next++, source, applied: this.applied }
  }

  /** An answer to `ticket` arrived. Returns whether it is now the current view. */
  offer(ticket: Ticket, view: V): boolean {
    const nothingSince = ticket.applied === this.applied
    const sameStream = ticket.source === "stream" && this.last === ticket.id
    if (!nothingSince && !sameStream) {
      if (ticket.source === "poll") return false
      const shown = this.current
      if (shown !== undefined) {
        if (view.version < shown.version) return false
        if (view.version === shown.version && view.confirmations < shown.confirmations) return false
      }
    }
    this.current = view
    this.applied += 1
    this.last = ticket.id
    return true
  }
}
