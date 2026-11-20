"use client"

import * as React from "react"
import { usePathname } from "next/navigation"

const subscribe = () => () => {}

/** False while rendering on the server and during hydration, true from then on. */
export function useHydrated() {
  return React.useSyncExternalStore(
    subscribe,
    () => true,
    () => false,
  )
}

/**
 * The n-th segment of the address bar's path ("/pay/pay_01…" → segment 1 is the id), or null
 * until the page has hydrated.
 *
 * The app is a static export: a dynamic route is exported once, for the placeholder parameter
 * "_", and the Go server answers every id with that one page (internal/webui). `useParams()`
 * would therefore say "_"; the real value is in the address bar. It is not read while
 * hydrating, because the HTML was rendered for the placeholder and must match first.
 */
export function usePathSegment(index: number): string | null {
  const hydrated = useHydrated()
  const pathname = usePathname()
  if (!hydrated) return null
  const segment = pathname.split("/").filter(Boolean)[index]
  if (!segment) return null
  try {
    return decodeURIComponent(segment)
  } catch {
    return segment
  }
}

/** Shape of a payment id: "pay_" + 26 characters of the server's id alphabet. */
export const isPaymentId = (value: string) => /^pay_[0-9a-hjkmnp-tv-z]{26}$/.test(value)

/** Shape of a payment-link slug: 1–64 lowercase letters, digits and inner hyphens. */
export const isLinkSlug = (value: string) => /^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$/.test(value)
