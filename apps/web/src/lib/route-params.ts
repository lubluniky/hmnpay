/**
 * The one value every dynamic route is exported for.
 *
 * The app is a static export: `/pay/[id]`, `/payments/[id]` and `/l/[slug]` are each built
 * once, for this placeholder, and the Go server answers every real id with that one page
 * (internal/webui, `Placeholder`). The pages read the real value from the address bar
 * (`usePathSegment` in use-route.ts). Keep the three in step.
 *
 * Not a client module on purpose: `generateStaticParams` runs at build time on the server side.
 */
export const PLACEHOLDER = "_"
export const PLACEHOLDER_ID_PARAMS = [{ id: PLACEHOLDER }]
export const PLACEHOLDER_SLUG_PARAMS = [{ slug: PLACEHOLDER }]
