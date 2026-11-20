/**
 * URLs that come from the API were typed by a merchant (redirect targets, support pages, a logo)
 * or by whoever created a payment. Before one is used as a link target or an image source it is
 * checked here: only absolute http(s) URLs pass, so a "javascript:" or "data:text/html" value can
 * never become something a click runs.
 *
 * Targets that must stay on this site (the "next" of the login page, links the server built
 * without a public URL) go through [sameSitePath], which judges where a value resolves, not what
 * it looks like.
 */

/** The URL if it is an absolute http or https URL, otherwise null. */
export function safeHttpUrl(value: string | null | undefined): string | null {
  if (!value) return null
  let url: URL
  try {
    url = new URL(value)
  } catch {
    return null
  }
  return url.protocol === "https:" || url.protocol === "http:" ? url.href : null
}

/** This page's own origin; "" where there is no page (a test, the build). */
const pageOrigin = () => (typeof window === "undefined" ? "" : window.location.origin)

// Tab, CR and LF are removed by browsers before a URL is parsed, and the other control
// characters have no business in a path: none is allowed.
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/

/**
 * A path on this site ("/payments?status=open"), or null.
 *
 * This is the one rule for every navigation target that comes from the address bar or from API
 * data and is meant to stay on this site. It is a positive rule, in two parts that must both
 * hold:
 *
 *  - the text itself starts with one "/" followed by something that is neither "/" nor "\",
 *    and contains no backslash and no control character anywhere (a browser reads "\" as "/"
 *    and drops tab, CR and LF, which is how "/<tab>/evil.example" turns into "//evil.example");
 *  - resolved by the URL parser against this page's origin, it is a URL of this origin.
 *
 * What is returned is the resolved path, query and fragment — what the browser would open —
 * never the text that was passed in.
 */
export function sameSitePath(value: string | null | undefined, origin: string = pageOrigin()): string | null {
  if (!value || !origin) return null
  if (value[0] !== "/" || value.length < 2 || value[1] === "/" || value[1] === "\\") return null
  if (value.includes("\\") || CONTROL.test(value)) return null
  let url: URL
  try {
    url = new URL(value, origin)
  } catch {
    return null
  }
  if (url.origin !== new URL(origin).origin) return null
  const path = url.pathname + url.search + url.hash
  // Belt and braces: the result must itself be a single-slash path.
  return path[0] === "/" && path[1] !== "/" && path[1] !== "\\" ? path : null
}

/**
 * A link the server built from the instance's public URL (a checkout or payment-link address):
 * an absolute http(s) URL, or a path on this site when no public URL is configured. Anything
 * else becomes "#".
 */
export function safeSiteUrl(value: string | null | undefined, origin: string = pageOrigin()): string {
  return sameSitePath(value, origin) ?? safeHttpUrl(value) ?? "#"
}

const INLINE_LOGO = /^data:image\/(png|jpeg|webp|gif);base64,[A-Za-z0-9+/=]+$/

/** A merchant logo the page may show: an http(s) URL or an inline PNG, JPEG, WebP or GIF (what the API accepts). */
export function safeLogoUrl(value: string | null | undefined): string | null {
  if (!value) return null
  if (INLINE_LOGO.test(value)) return value
  return safeHttpUrl(value)
}

/** A plain email address, safe to put behind "mailto:". */
export function safeEmail(value: string | null | undefined): string | null {
  if (!value) return null
  return /^[^\s<>"'`,;:?&#]+@[^\s<>"'`,;:?&#@]+$/.test(value) ? value : null
}

/** An EIP-681 payment URI as the API builds it ("ethereum:0x…@56/transfer?address=0x…&uint256=…"), or null. */
export function safePaymentUri(value: string | null | undefined): string | null {
  if (!value) return null
  return /^ethereum:0x[0-9a-fA-F]{40}@\d+\/transfer\?address=0x[0-9a-fA-F]{40}&uint256=\d+$/.test(value) ? value : null
}

/** Host of a URL for display ("hooks.example.com"), or the text itself when it is not a URL. */
export function hostOf(value: string): string {
  try {
    return new URL(value).host
  } catch {
    return value
  }
}

/**
 * Where "next" may send the operator after signing in: a path on this site ([sameSitePath]) that
 * is not part of the API. Anything else — another site, however it is spelled — becomes the
 * overview.
 */
export function safeNextPath(value: string | null | undefined, origin: string = pageOrigin()): string {
  const path = sameSitePath(value, origin)
  if (path === null) return "/"
  const lower = path.toLowerCase()
  if (lower === "/api" || lower.startsWith("/api/") || lower.startsWith("/api?") || lower.startsWith("/api#")) return "/"
  return path
}
