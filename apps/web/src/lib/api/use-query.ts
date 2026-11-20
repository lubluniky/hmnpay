"use client"

import * as React from "react"

import { ApiError, isApiError } from "./client"
import { applyMutation, type State } from "./mutation"

/**
 * Data fetching for a static, client-rendered app: one hook, no cache library.
 *
 *   const { data, error, loading, reload } = useQuery(key, (signal) => api.payments(query, signal), { refreshMs: 5000 })
 *
 * - `key` identifies the request. A new key fetches again and, with `keepPrevious`, keeps showing
 *   the old data meanwhile (a table that changes page must not blink to a skeleton). `null`
 *   means "not yet": nothing is fetched.
 * - `refreshMs` polls while the tab is visible and refetches when it becomes visible again. A
 *   failed poll keeps the last good data and tries again; a 429 waits for the server's
 *   Retry-After first.
 * - The last result of every key is remembered for the lifetime of the page, so going back to a
 *   screen shows what it showed and refreshes underneath. `clearQueryCache` forgets everything
 *   (on sign-out).
 */

const cache = new Map<string, unknown>()

export function clearQueryCache() {
  cache.clear()
}

export interface QueryResult<T> {
  data: T | undefined
  /** The last failure, cleared by the next success. With `data` set it is a failed refresh. */
  error: ApiError | undefined
  /** True until the first answer (success or failure) for this key. */
  loading: boolean
  /** Fetch again now. */
  reload: () => void
  /** Replace the data locally (after a mutation whose response is the new state). */
  mutate: (next: T | ((current: T | undefined) => T | undefined)) => void
}


export function useQuery<T>(
  key: string | null,
  fetcher: (signal: AbortSignal) => Promise<T>,
  options: { refreshMs?: number; keepPrevious?: boolean } = {},
): QueryResult<T> {
  const { refreshMs, keepPrevious = false } = options
  const [state, setState] = React.useState<State<T>>(() => ({
    key,
    data: key !== null ? (cache.get(key) as T | undefined) : undefined,
    error: undefined,
    loading: key !== null,
  }))
  const [tick, setTick] = React.useState(0)
  const latest = React.useRef(fetcher)
  React.useEffect(() => {
    latest.current = fetcher
  })

  // A new key: show what is remembered for it (or keep the previous rows) until the answer.
  if (state.key !== key) {
    const remembered = key !== null ? (cache.get(key) as T | undefined) : undefined
    setState({
      key,
      data: remembered ?? (keepPrevious ? state.data : undefined),
      error: undefined,
      loading: key !== null,
    })
  }

  React.useEffect(() => {
    if (key === null) return
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout> | undefined
    let stopped = false

    const run = async () => {
      let delay = refreshMs
      try {
        const data = await latest.current(controller.signal)
        if (stopped) return
        cache.set(key, data)
        setState({ key, data, error: undefined, loading: false })
      } catch (cause) {
        if (stopped || (cause instanceof DOMException && cause.name === "AbortError")) return
        const error = isApiError(cause) ? cause : new ApiError({ status: 0, code: "unknown", message: String(cause) })
        setState((current) => (current.key === key ? { ...current, error, loading: false } : current))
        if (error.status === 401 || error.status === 404) return // not going to get better by asking again
        // Without polling, a throttled or failed first load still deserves another try.
        if (delay === undefined && error.retryAfter !== undefined) delay = error.retryAfter * 1000
        else if (delay !== undefined && error.retryAfter !== undefined) delay = Math.max(delay, error.retryAfter * 1000)
        else if (delay !== undefined && error.status === 0) delay = Math.max(delay, 5000)
      }
      if (delay !== undefined && !stopped) timer = setTimeout(tickIfVisible, delay)
    }
    const tickIfVisible = () => {
      if (document.visibilityState === "visible") void run()
      // Hidden: the visibilitychange listener below picks it up again.
    }
    const onVisible = () => {
      if (document.visibilityState !== "visible" || refreshMs === undefined) return
      clearTimeout(timer)
      void run()
    }

    void run()
    document.addEventListener("visibilitychange", onVisible)
    return () => {
      stopped = true
      controller.abort()
      clearTimeout(timer)
      document.removeEventListener("visibilitychange", onVisible)
    }
  }, [key, refreshMs, tick])

  const reload = React.useCallback(() => setTick((n) => n + 1), [])
  // Bound to the key it was made for. A mutation is usually the answer to a request, and by the
  // time it arrives the screen may show something else (another payment's page reuses this very
  // hook with another key): the answer then belongs in the cache under its own key, and must
  // not be put on the screen that is showing now.
  const mutate = React.useCallback(
    (next: T | ((current: T | undefined) => T | undefined)) => {
      if (key === null) return
      setState((current) => applyMutation(cache, key, current, next))
    },
    [key],
  )

  const current = state.key === key ? state : { data: undefined, error: undefined, loading: key !== null }
  return { data: current.data, error: current.error, loading: current.loading && current.data === undefined, reload, mutate }
}
