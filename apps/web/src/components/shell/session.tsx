"use client"

import * as React from "react"
import { toast } from "sonner"

import { useSound } from "@/components/hmn"
import { api, isApiError, setUnauthorizedHandler } from "@/lib/api/client"
import type { ApiEvent, Instance, NodeStatus, Operator, PaymentCounts, Settings } from "@/lib/api/types"
import { clearQueryCache, useQuery } from "@/lib/api/use-query"
import { describeError } from "@/lib/errors"

/**
 * Everything the dashboard knows about the instance it is looking at, loaded once and kept
 * current: who is signed in, the instance settings, the node status, how many payments are open,
 * and the live event feed.
 *
 * It is also the gate. Nothing of the dashboard renders until the session is confirmed: a visitor
 * without one is sent to the login page (or to setup, if the instance has none yet), and a
 * session that ends later takes the operator back there from whatever they were doing.
 */

export interface Dashboard {
  operator: Operator
  settings: Settings
  /** Put settings a save just returned in place of the loaded ones. */
  setSettings: (next: Settings) => void
  /** Which build is running; undefined until it has answered. */
  instance: Instance | undefined
  node: NodeStatus | undefined
  /** The last node-status request failed (the status shown, if any, is old). */
  nodeError: unknown
  counts: PaymentCounts | undefined
  /** Ask for the counts again (after creating or cancelling something). */
  refreshCounts: () => void
  signOut: () => Promise<void>
}

const DashboardContext = React.createContext<Dashboard | null>(null)

export function useDashboard(): Dashboard {
  const value = React.use(DashboardContext)
  if (!value) throw new Error("useDashboard must be used inside the dashboard")
  return value
}

/* ------------------------------- live events ------------------------------- */

type Listener = (event: ApiEvent) => void
const LiveContext = React.createContext<{ subscribe: (listener: Listener) => () => void } | null>(null)

/**
 * Calls `listener` for every event the instance logs while the dashboard is open (a payment
 * created, detected, paid, …), in order. Events from before the page loaded are not replayed.
 */
export function useLiveEvents(listener: Listener) {
  const live = React.use(LiveContext)
  const latest = React.useRef(listener)
  React.useEffect(() => {
    latest.current = listener
  })
  React.useEffect(() => live?.subscribe((event) => latest.current(event)), [live])
}

const POLL_MS = 3000

/**
 * Follows the instance's event log (GET /api/v1/events) from "now".
 *
 * The API has no "latest position" call, so the start is found from below: the newest payment's
 * own timeline gives a recent position, and the log is read forward from there to its end
 * without announcing anything. From then on it is polled every few seconds while the tab is
 * visible. A failed poll is simply tried again; nothing is skipped, because the cursor only
 * moves past events that were delivered.
 */
function useEventFeed(enabled: boolean, emit: Listener) {
  React.useEffect(() => {
    if (!enabled) return
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout> | undefined
    let cursor: number | null = null
    let stopped = false

    const start = async () => {
      let from = 0
      const newest = await api.payments({ limit: 1 }, controller.signal)
      if (newest.data.length > 0) {
        const detail = await api.paymentDetail(newest.data[0].id, controller.signal)
        from = detail.timeline.reduce((max, e) => Math.max(max, e.seq), 0)
      }
      for (;;) {
        const batch = await api.events({ after: from, limit: 1000 }, controller.signal)
        from = batch.next
        if (batch.data.length < 1000) return from
      }
    }

    const poll = async () => {
      let delay = POLL_MS
      try {
        if (cursor === null) cursor = await start()
        else {
          const batch = await api.events({ after: cursor, limit: 200 }, controller.signal)
          for (const event of batch.data) emit(event)
          cursor = batch.next
          if (batch.data.length === 200) delay = 0
        }
      } catch (cause) {
        if (stopped || (cause instanceof DOMException && cause.name === "AbortError")) return
        if (isApiError(cause) && cause.status === 401) return
        delay = Math.max(POLL_MS * 2, (isApiError(cause) ? (cause.retryAfter ?? 0) : 0) * 1000)
      }
      if (!stopped) timer = setTimeout(next, delay)
    }
    const next = () => {
      if (document.visibilityState === "visible") void poll()
    }
    const onVisible = () => {
      if (document.visibilityState !== "visible") return
      clearTimeout(timer)
      void poll()
    }

    void poll()
    document.addEventListener("visibilitychange", onVisible)
    return () => {
      stopped = true
      controller.abort()
      clearTimeout(timer)
      document.removeEventListener("visibilitychange", onVisible)
    }
  }, [enabled, emit])
}

/** The cue for an event, if it has one: money landing is heard, trouble is heard, the rest is seen. */
function cueFor(event: ApiEvent) {
  switch (event.type) {
    case "payment.paid":
    case "payment.overpaid":
      return "received" as const
    case "payment.underpaid":
    case "payment.failed":
    case "payment.late_funds":
      return "attention" as const
    default:
      return null
  }
}

/* --------------------------------- provider -------------------------------- */

function redirectToLogin() {
  const here = window.location.pathname + window.location.search
  const next = here === "/" ? "" : `?next=${encodeURIComponent(here)}`
  // A full navigation on purpose: the login page must start from nothing this session left behind.
  // eslint-disable-next-line @next/next/no-location-assign-relative-destination
  window.location.assign(`/login${next}`)
}

export type GateState = { state: "loading" } | { state: "redirecting" } | { state: "error"; error: unknown; retry: () => void }

/**
 * Loads the session and the settings, then renders `children` with the dashboard context.
 * Until then (and when it cannot) it renders `fallback` with what is going on.
 */
export function DashboardProvider({ children, fallback }: { children: React.ReactNode; fallback: (gate: GateState) => React.ReactNode }) {
  const me = useQuery("auth/me", (signal) => api.me(signal))
  const signedIn = me.data?.kind === "operator" && me.data.operator !== undefined
  const settings = useQuery(signedIn ? "settings" : null, (signal) => api.settings(signal))
  const instance = useQuery(signedIn ? "instance" : null, (signal) => api.instance(signal))
  const node = useQuery(signedIn ? "node-status" : null, (signal) => api.nodeStatus(signal), { refreshMs: 5000 })
  const counts = useQuery(signedIn ? "payments/counts" : null, async (signal) => (await api.payments({ limit: 1, counts: true }, signal)).counts, {
    refreshMs: 15000,
  })
  const play = useSound()

  // A session that ends while the dashboard is open: back to the login page.
  React.useEffect(() => {
    setUnauthorizedHandler(() => {
      clearQueryCache()
      redirectToLogin()
    })
    return () => setUnauthorizedHandler(null)
  }, [])

  // No session on arrival: setup if the instance has none yet, otherwise sign in.
  const unauthenticated = me.error?.status === 401
  React.useEffect(() => {
    if (!unauthenticated) return
    let cancelled = false
    api
      .setupStatus()
      .then((status) => {
        if (cancelled) return
        if (status.completed) redirectToLogin()
        // eslint-disable-next-line @next/next/no-location-assign-relative-destination -- a full navigation, as above
        else window.location.assign("/setup")
      })
      .catch(() => {
        if (!cancelled) redirectToLogin()
      })
    return () => {
      cancelled = true
    }
  }, [unauthenticated])

  const listeners = React.useRef(new Set<Listener>())
  const live = React.useMemo(
    () => ({
      subscribe(listener: Listener) {
        listeners.current.add(listener)
        return () => void listeners.current.delete(listener)
      },
    }),
    [],
  )
  const reloadCounts = counts.reload
  const emit = React.useCallback(
    (event: ApiEvent) => {
      const cue = cueFor(event)
      // Rate-limited in the engine: a burst of payments is seen in full and heard a few times.
      if (cue) play(cue)
      if (event.type.startsWith("payment.")) reloadCounts()
      for (const listener of listeners.current) listener(event)
    },
    [play, reloadCounts],
  )
  useEventFeed(signedIn && settings.data !== undefined, emit)

  const setSettings = settings.mutate
  const value = React.useMemo<Dashboard | null>(() => {
    if (!signedIn || !me.data?.operator || !settings.data) return null
    return {
      operator: me.data.operator,
      settings: settings.data,
      setSettings,
      instance: instance.data,
      node: node.data,
      nodeError: node.error,
      counts: counts.data,
      refreshCounts: reloadCounts,
      async signOut() {
        try {
          await api.logout()
        } catch (error) {
          // 401: the session is gone already, which is what was asked for. Anything else — the
          // request did not arrive, or the server could not do it — leaves the session valid:
          // sending the operator to the login page now would look like signed out on a browser
          // that is still signed in.
          if (!(isApiError(error) && error.status === 401)) {
            toast.error("Couldn’t sign out", {
              description: `${describeError(error)} You are still signed in on this browser; try again.`,
            })
            return
          }
        }
        clearQueryCache()
        // eslint-disable-next-line @next/next/no-location-assign-relative-destination -- a full navigation, as above
        window.location.assign("/login")
      },
    }
  }, [signedIn, me.data, settings.data, setSettings, instance.data, node.data, node.error, counts.data, reloadCounts])

  if (!value) {
    if (unauthenticated) return fallback({ state: "redirecting" })
    const error = me.error ?? settings.error ?? (me.data && !signedIn ? new Error("not an operator session") : undefined)
    if (error) {
      return fallback({
        state: "error",
        error,
        retry: () => {
          me.reload()
          settings.reload()
        },
      })
    }
    return fallback({ state: "loading" })
  }
  return (
    <DashboardContext value={value}>
      <LiveContext value={live}>{children}</LiveContext>
    </DashboardContext>
  )
}
