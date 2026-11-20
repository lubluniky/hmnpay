import { cn } from "@/lib/utils"

/**
 * The hmnpay mark: a lowercase "h" whose right shoulder carries a dot — an "h" and a
 * person standing next to it. The dot is the only place the mark uses the brand accent.
 * Glyph inherits `currentColor`; size it with `className` (defaults to 1.25em).
 */
export function LogoMark({ className, ...props }: React.ComponentProps<"svg">) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      aria-hidden="true"
      className={cn("size-[1.25em] shrink-0", className)}
      {...props}
    >
      <path d="M6.3 3v18" stroke="currentColor" strokeWidth="3.6" />
      <path d="M6.3 21v-5.5a5.7 5.7 0 0 1 11.4 0V21" stroke="currentColor" strokeWidth="3.6" />
      <circle cx="17.7" cy="4.4" r="2.2" className="fill-brand" />
    </svg>
  )
}

const sizes = {
  sm: "text-sm gap-1.5",
  md: "text-base gap-2",
  lg: "text-xl gap-2.5",
} as const

/**
 * Mark + wordmark. `wordmark={false}` renders the mark only (collapsed sidebar, favicon-like spots).
 */
export function Logo({
  size = "md",
  wordmark = true,
  className,
  ...props
}: React.ComponentProps<"span"> & { size?: keyof typeof sizes; wordmark?: boolean }) {
  return (
    <span
      role="img"
      aria-label="hmnpay"
      className={cn("inline-flex items-center font-semibold tracking-[-0.03em] text-foreground select-none", sizes[size], className)}
      {...props}
    >
      <LogoMark />
      {wordmark && (
        <span aria-hidden="true">
          hmn<span className="text-muted-foreground">pay</span>
        </span>
      )}
    </span>
  )
}
