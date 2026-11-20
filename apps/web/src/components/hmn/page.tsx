import { cn } from "@/lib/utils"

const widths = {
  /** Lists, overview, anything with tables. */
  default: "max-w-[1200px]",
  /** Detail pages and forms. */
  narrow: "max-w-[880px]",
  /** Rare full-bleed data views. */
  full: "max-w-none",
} as const

/** Standard page container for every dashboard route. Owns the gutters and vertical padding. */
export function PageContainer({
  size = "default",
  className,
  ...props
}: React.ComponentProps<"div"> & { size?: keyof typeof widths }) {
  return (
    <div
      data-slot="page"
      className={cn("mx-auto w-full px-4 pt-6 pb-16 sm:px-6 lg:px-8 lg:pt-8", widths[size], className)}
      {...props}
    />
  )
}

/**
 * Page title row. One per page, first child of `PageContainer`.
 * `actions` sit right on desktop and drop under the title on mobile; put the primary action last.
 * `children` render under the row (filters, tabs).
 */
export function PageHeader({
  title,
  description,
  actions,
  meta,
  className,
  children,
  ...props
}: Omit<React.ComponentProps<"header">, "title"> & {
  title: React.ReactNode
  description?: React.ReactNode
  actions?: React.ReactNode
  /** Inline next to the title: a status badge, a count. */
  meta?: React.ReactNode
}) {
  return (
    <header data-slot="page-header" className={cn("mb-6 lg:mb-8", className)} {...props}>
      <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
            <h1 className="text-2xl leading-8 font-semibold tracking-[-0.025em] text-balance">{title}</h1>
            {meta}
          </div>
          {description && <p className="mt-1 max-w-2xl text-sm leading-6 text-muted-foreground">{description}</p>}
        </div>
        {actions && <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>}
      </div>
      {children && <div className="mt-6">{children}</div>}
    </header>
  )
}

/**
 * A titled block inside a page (e.g. "Recent payments" above a table).
 * Sections are separated by `space-y-8` on the parent.
 */
export function PageSection({
  title,
  description,
  actions,
  className,
  children,
  ...props
}: Omit<React.ComponentProps<"section">, "title"> & {
  title?: React.ReactNode
  description?: React.ReactNode
  actions?: React.ReactNode
}) {
  return (
    <section data-slot="page-section" className={cn("flex flex-col gap-3", className)} {...props}>
      {(title || actions) && (
        <div className="flex items-end justify-between gap-4">
          <div className="min-w-0">
            {title && <h2 className="text-sm leading-6 font-semibold tracking-[-0.01em]">{title}</h2>}
            {description && <p className="text-[13px] leading-5 text-muted-foreground">{description}</p>}
          </div>
          {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
        </div>
      )}
      {children}
    </section>
  )
}

/** The standard bordered surface: tables, charts and lists sit in one of these. */
export function Panel({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="panel"
      className={cn("overflow-hidden rounded-xl border border-border bg-card shadow-card", className)}
      {...props}
    />
  )
}
