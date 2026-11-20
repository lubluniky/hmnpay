"use client"

import * as React from "react"

import { api, ApiError, checkoutStreamUrl, isApiError, parseCheckout } from "@/lib/api/client"
import type { Checkout } from "@/lib/api/types"
import { noteServerDate, serverNow } from "@/lib/server-clock"
import { ViewOrder, type Ticket } from "@/lib/view-order"

/**
 * Keeps a checkout current while its page is open.
 *
 * Two ways to learn that a payment changed, and the page needs only one of them to work:
 *
 *  1. The event stream (GET …/stream, server-sent events). Each message is a complete public
 *     view of the payment with an id; after a break the stream is reopened with
 *     `Last-Event-ID`, so the server sends only what is new.
 *  2. Polling the plain view (GET /public/payments/{id}).
 *
 * The stream is read with fetch rather than EventSource, because the server may refuse one
 * (429 too_many_streams: the payer's network holds several, or somebody is holding this
 * payment's slots) and EventSource cannot tell a refusal from an outage. So:
 *
 *  - the first thing shown is one plain read, which also tells "no such payment" at once;
 *  - then the stream is opened. While it is connected, nothing is polled; while it is slow to
 *    open, polling runs alongside;
 *  - refused, broken, silent or ended: polling takes over immediately, and the stream is tried
 *    again later (after the server's Retry-After, then with a growing pause). The page never
 *    depends on the stream coming back.
 *
 * Answers from the two can arrive out of order (a poll answer held up on a bad connection
 * arrives after the stream has moved on). Every view therefore goes through one place,
 * [ViewOrder], which never lets an older view replace a newer one: a paid payment is never
 * shown as waiting for money again. A poll that is still under way when the stream connects is
 * aborted, and only one poll is ever under way.
 *
 * A stream that is connected but silent is not trusted: the server sends a keep-alive every 15
 * seconds, and after 40 seconds without a byte the connection is given up and polling takes
 * over. The page also remembers when the server last answered at all (`contactAt`), so the
 * checkout can stop inviting a payment once its deadline has passed without the server
 * confirming that it is still open.
 *
 * Identical views are not re-applied, so React state changes only when the payment does.
 */

export type LiveMode = "connecting" | "stream" | "polling"

export interface CheckoutState {
  checkout: Checkout | undefined
  /** How updates are arriving right now. */
  mode: LiveMode
  /** The payment does not exist (or the id is not a payment id). */
  notFound: boolean
  /** The last read failed and there is nothing to show yet. */
  error: ApiError | undefined
  /** The last read failed, but an earlier view is on screen. */
  stale: boolean
  /**
   * When the server last answered for this payment, by this device's clock (ms): a poll answer,
   * or any byte of the stream, keep-alives included. Undefined until the first answer.
   */
  contactAt: number | undefined
  retry: () => void
}

/** The server's keep-alive comes every 15 seconds; a stream silent for this long is treated as broken. */
const STREAM_SILENCE_MS = 40_000
/** A stream that has not opened after this long gets polling for company. */
const STREAM_CONNECT_PATIENCE_MS = 3000

const isActive = (c: Checkout | undefined) => c?.status === "detected" || c?.status === "confirming"
const isOpen = (c: Checkout | undefined) => c === undefined || c.status === "awaiting" || isActive(c)

/** How often to poll when there is no stream: fast while confirmations tick, slow once settled. */
function pollDelay(c: Checkout | undefined) {
  if (isActive(c)) return 2000
  return isOpen(c) ? 4000 : 20_000
}

interface SseMessage {
  id?: string
  event: string
  data: string
}

/**
 * Reads a text/event-stream body and calls `onMessage` for every complete message, and `onBytes`
 * for everything that arrives (the server's keep-alive comments too).
 */
async function readEventStream(body: ReadableStream<Uint8Array>, onMessage: (message: SseMessage) => void, onBytes: () => void) {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""
  let id: string | undefined
  let event = "message"
  let data: string[] = []
  for (;;) {
    const { value, done } = await reader.read()
    if (done) return
    onBytes()
    buffer += decoder.decode(value, { stream: true })
    for (;;) {
      const end = buffer.search(/\r\n|\n|\r/)
      if (end < 0) break
      const line = buffer.slice(0, end)
      buffer = buffer.slice(end + (buffer.startsWith("\r\n", end) ? 2 : 1))
      if (line === "") {
        if (data.length > 0) onMessage({ id, event, data: data.join("\n") })
        event = "message"
        data = []
        continue
      }
      if (line.startsWith(":")) continue // a comment: the server's keep-alive
      const colon = line.indexOf(":")
      const field = colon < 0 ? line : line.slice(0, colon)
      const text = colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "")
      if (field === "id") id = text
      else if (field === "event") event = text
      else if (field === "data") data.push(text)
    }
  }
}

export function useCheckout(id: string | null): CheckoutState {
  const [checkout, setCheckout] = React.useState<Checkout | undefined>(undefined)
  const [mode, setMode] = React.useState<LiveMode>("connecting")
  const [notFound, setNotFound] = React.useState(false)
  const [error, setError] = React.useState<ApiError | undefined>(undefined)
  const [stale, setStale] = React.useState(false)
  const [contactAt, setContactAt] = React.useState<number | undefined>(undefined)
  const [attempt, setAttempt] = React.useState(0)
  const [shownFor, setShownFor] = React.useState(id)
  if (shownFor !== id) {
    setShownFor(id)
    setCheckout(undefined)
    setNotFound(false)
    setError(undefined)
    setStale(false)
    setContactAt(undefined)
    setMode("connecting")
  }

  React.useEffect(() => {
    if (id === null) return
    const controller = new AbortController()
    let stopped = false
    // The one place a view is accepted or refused.
    const order = new ViewOrder<Checkout>()
    let latestJson = ""
    let lastEventId: string | undefined
    let pollTimer: ReturnType<typeof setTimeout> | undefined
    let streamTimer: ReturnType<typeof setTimeout> | undefined
    let silenceTimer: ReturnType<typeof setTimeout> | undefined
    let streaming = false
    let streamFailures = 0
    // The poll under way, if any: there is never more than one.
    let polling: AbortController | undefined
    let contact = 0
    let deadlineTimer: ReturnType<typeof setTimeout> | undefined
    let deadlineFor = ""

    /** The server answered just now. */
    const heard = () => {
      const now = Date.now()
      // Once a second is enough for anything that is shown.
      if (now - contact >= 1000) {
        contact = now
        setContactAt(now)
      }
    }
    /** A view arrived in answer to `ticket`. Returns whether it was taken. */
    const offer = (ticket: Ticket, next: Checkout): boolean => {
      heard()
      if (!order.offer(ticket, next)) return false
      const json = JSON.stringify(next)
      if (json !== latestJson) {
        latestJson = json
        setCheckout(next)
      }
      setError(undefined)
      setStale(false)
      watchDeadline(next)
      return true
    }
    /**
     * The moment an open payment's deadline passes, the server is asked — also while the stream
     * is connected, which only speaks when something changes. The checkout stops inviting a
     * payment at that moment by its own clock; this is how it learns within a second whether
     * the server still has the payment open.
     */
    const watchDeadline = (view: Checkout) => {
      if (!isOpen(view)) return clearTimeout(deadlineTimer)
      if (deadlineFor === view.expires_at) return
      deadlineFor = view.expires_at
      clearTimeout(deadlineTimer)
      const wait = Date.parse(view.expires_at) - serverNow()
      if (wait < 0 || wait > 0x7fffffff) return
      deadlineTimer = setTimeout(() => {
        if (stopped) return
        if (!streaming) return void poll()
        // One read on the side. Its answer goes through the same order as every other view.
        const ticket = order.begin("poll")
        api.checkout(id, controller.signal).then(
          (answer) => void (stopped || offer(ticket, answer)),
          (cause) => {
            if (!stopped && !isAbort(cause) && failed(cause).status === 404) gone()
          },
        )
      }, wait + 300)
    }
    const gone = () => {
      stopped = true
      clearTimeout(pollTimer)
      clearTimeout(streamTimer)
      clearTimeout(silenceTimer)
      clearTimeout(deadlineTimer)
      controller.abort()
      setNotFound(true)
    }
    const failed = (cause: unknown) => {
      const apiError = isApiError(cause) ? cause : new ApiError({ status: 0, code: "network", message: String(cause) })
      // A refusal is still an answer: the server is there.
      if (apiError.status > 0) heard()
      if (order.current) setStale(true)
      else setError(apiError)
      return apiError
    }
    const isAbort = (cause: unknown) => cause instanceof DOMException && cause.name === "AbortError"

    const poll = async () => {
      clearTimeout(pollTimer)
      if (stopped || streaming || polling) return
      let delay = pollDelay(order.current)
      const mine = new AbortController()
      const forward = () => mine.abort()
      controller.signal.addEventListener("abort", forward)
      polling = mine
      const ticket = order.begin("poll")
      try {
        const view = await api.checkout(id, mine.signal)
        // Something newer was applied while this answer was on its way: it is dropped, and
        // the page asks again at once if it is still polling.
        if (!mine.signal.aborted && !offer(ticket, view)) delay = 0
      } catch (cause) {
        if (stopped || mine.signal.aborted || isAbort(cause)) return
        const apiError = failed(cause)
        if (apiError.status === 404) return gone()
        delay = Math.max(delay, (apiError.retryAfter ?? 0) * 1000, 3000)
      } finally {
        controller.signal.removeEventListener("abort", forward)
        if (polling === mine) polling = undefined
      }
      if (!stopped && !streaming) pollTimer = setTimeout(poll, document.visibilityState === "visible" ? delay : Math.max(delay, 15_000))
    }

    /** Polling takes over now; the stream gets another chance after `retryMs`. */
    const fallBack = (retryMs: number) => {
      streaming = false
      clearTimeout(silenceTimer)
      if (stopped) return
      setMode("polling")
      void poll()
      clearTimeout(streamTimer)
      streamTimer = setTimeout(openStream, retryMs)
    }

    const openStream = async () => {
      if (stopped || streaming) return
      // One connection at a time, with its own switch: a silent one is given up through it.
      const connection = new AbortController()
      const forward = () => connection.abort()
      controller.signal.addEventListener("abort", forward)
      const ticket = order.begin("stream")
      // A connection that is slow to open must not leave the page without news: after a few
      // seconds polling runs alongside, until the stream is there (or gives up).
      const slow = setTimeout(() => {
        if (stopped || streaming) return
        setMode("polling")
        void poll()
      }, STREAM_CONNECT_PATIENCE_MS)
      let response: Response
      try {
        response = await fetch(checkoutStreamUrl(id), {
          headers: { Accept: "text/event-stream", ...(lastEventId ? { "Last-Event-ID": lastEventId } : {}) },
          credentials: "same-origin",
          cache: "no-store",
          signal: connection.signal,
        })
      } catch {
        clearTimeout(slow)
        controller.signal.removeEventListener("abort", forward)
        if (stopped) return
        streamFailures += 1
        return fallBack(Math.min(30_000, 3000 * streamFailures))
      }
      clearTimeout(slow)
      if (stopped) return
      noteServerDate(response.headers.get("Date"))
      heard()
      if (response.status === 404) return gone()
      if (!response.ok || !response.body) {
        // Refused (429 too_many_streams, or rate limited): polling is the answer, not an error.
        // Come back after what the server asked for, and less often each time it says no.
        const retryAfter = Number(response.headers.get("Retry-After")) || 5
        streamFailures += 1
        void response.body?.cancel().catch(() => {})
        controller.signal.removeEventListener("abort", forward)
        return fallBack(Math.min(120_000, Math.max(retryAfter * 1000, 15_000) * streamFailures))
      }
      streaming = true
      // From here on the stream speaks for the payment: no poll is started, and one that is
      // still under way is called off (its answer could only be older than what follows).
      clearTimeout(pollTimer)
      polling?.abort()
      setMode("stream")
      setError(undefined)
      setStale(false)
      let received = false
      let silent = false
      const listen = () => {
        clearTimeout(silenceTimer)
        silenceTimer = setTimeout(() => {
          silent = true
          connection.abort()
        }, STREAM_SILENCE_MS)
      }
      listen()
      try {
        await readEventStream(
          response.body,
          (message) => {
            if (message.event !== "checkout") return
            let view: Checkout
            try {
              view = parseCheckout(JSON.parse(message.data))
            } catch {
              return // one unreadable message: the next one is a full view again
            }
            received = true
            streamFailures = 0
            // The id is remembered only for a view that was taken: a reconnect must not claim
            // to have something the page refused to show.
            if (offer(ticket, view) && message.id !== undefined) lastEventId = message.id
          },
          () => {
            heard()
            listen()
          },
        )
      } catch {
        // The connection broke, or went silent and was given up; handled below like a stream that ended.
      }
      clearTimeout(silenceTimer)
      controller.signal.removeEventListener("abort", forward)
      if (stopped) return
      // Ended (the server closes streams after a while, or on shutdown), broke or went silent:
      // reconnect with Last-Event-ID, and poll in the meantime.
      if (!received || silent) streamFailures += 1
      fallBack(received && !silent ? 1000 : Math.min(30_000, 3000 * streamFailures))
    }

    const onVisible = () => {
      if (document.visibilityState !== "visible" || stopped) return
      if (!streaming) void poll()
    }

    // First one plain read: content on screen at once, and "no such payment" without a stream.
    void (async () => {
      const ticket = order.begin("poll")
      try {
        offer(ticket, await api.checkout(id, controller.signal))
      } catch (cause) {
        if (stopped || isAbort(cause)) return
        if (failed(cause).status === 404) return gone()
      }
      if (!stopped) void openStream()
    })()
    document.addEventListener("visibilitychange", onVisible)

    return () => {
      stopped = true
      controller.abort()
      clearTimeout(pollTimer)
      clearTimeout(streamTimer)
      clearTimeout(silenceTimer)
      clearTimeout(deadlineTimer)
      document.removeEventListener("visibilitychange", onVisible)
    }
  }, [id, attempt])

  const retry = React.useCallback(() => {
    setError(undefined)
    setAttempt((n) => n + 1)
  }, [])

  return { checkout, mode, notFound, error, stale, contactAt, retry }
}
