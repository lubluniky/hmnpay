"use client"

import * as React from "react"

/**
 * The current time in milliseconds, refreshed every `intervalMs` — for "5 minutes ago" texts and
 * countdowns, so rendering never calls `Date.now()` itself. Components that show it render only
 * after their data has loaded in the browser, so there is no server-rendered time to mismatch.
 */
export function useNow(intervalMs = 30_000): number {
  const [now, setNow] = React.useState(() => Date.now())
  React.useEffect(() => {
    const tick = () => setNow(Date.now())
    const id = setInterval(tick, intervalMs)
    document.addEventListener("visibilitychange", tick)
    return () => {
      clearInterval(id)
      document.removeEventListener("visibilitychange", tick)
    }
  }, [intervalMs])
  return now
}
