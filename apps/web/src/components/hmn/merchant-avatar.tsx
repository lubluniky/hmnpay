import { safeLogoUrl } from "@/lib/url"
import { cn } from "@/lib/utils"

/**
 * Merchant / instance identity tile: the logo if one is configured, otherwise the initial
 * on an ink tile. Square with a small radius — people are round, businesses are square.
 */
export function MerchantAvatar({
  name,
  logoUrl,
  className,
  ...props
}: Omit<React.ComponentProps<"span">, "children"> & { name: string; logoUrl?: string | null }) {
  // The logo is whatever the merchant configured: only an http(s) URL or an inline bitmap is shown.
  const logo = safeLogoUrl(logoUrl)
  // A logo on somebody else's host is shown by the browser as an image and nothing more: the
  // wave effect (src/vendor/distortion-waves) is told to leave it out of its picture, because
  // drawing it would mean fetching that URL from script. (The page's Content-Security-Policy
  // would refuse that fetch anyway: connect-src 'self'.) An inline logo needs no fetch.
  const remote = logo !== null && !logo.startsWith("data:")
  return (
    <span
      data-slot="merchant-avatar"
      aria-hidden="true"
      className={cn(
        "inline-flex size-8 shrink-0 items-center justify-center overflow-hidden rounded-md bg-foreground text-[0.8125rem] font-semibold text-background select-none",
        className,
      )}
      {...props}
    >
      {logo ? (
        // eslint-disable-next-line @next/next/no-img-element -- merchant-supplied URL on a self-hosted instance
        <img src={logo} alt="" referrerPolicy="no-referrer" data-dw-skip={remote ? "" : undefined} className="size-full object-cover" />
      ) : (
        name.trim().charAt(0).toUpperCase()
      )}
    </span>
  )
}
