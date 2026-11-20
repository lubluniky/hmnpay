import { ArrowDownRightIcon, ArrowUpRightIcon, MinusIcon } from "lucide-react"

import { cn } from "@/lib/utils"

/**
 * A row of metric tiles sharing one bordered surface, split by hairlines.
 * Use 2–4 tiles; it collapses to 2 columns on mobile.
 */
export function StatGroup({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="stat-group"
      className={cn(
        "grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-border bg-border shadow-card lg:grid-cols-4",
        "*:data-[slot=stat]:rounded-none *:data-[slot=stat]:border-0 *:data-[slot=stat]:shadow-none",
        className,
      )}
      {...props}
    />
  )
}

/**
 * Change vs. the previous period. Direction is carried by the arrow and the sign, colour is secondary.
 * `invert` for metrics where down is good.
 */
export function Delta({
  value,
  trend,
  invert = false,
  className,
}: {
  value: string
  trend: "up" | "down" | "flat"
  invert?: boolean
  className?: string
}) {
  const good = trend === "flat" ? null : (trend === "up") !== invert
  const Icon = trend === "up" ? ArrowUpRightIcon : trend === "down" ? ArrowDownRightIcon : MinusIcon
  return (
    <span
      className={cn(
        "num inline-flex items-center gap-0.5 text-xs font-medium",
        good === null ? "text-muted-foreground" : good ? "text-success-fg" : "text-danger-fg",
        className,
      )}
    >
      <Icon className="size-3.5" aria-hidden="true" />
      {value}
    </span>
  )
}

/**
 * One metric: quiet label, a big mono value, an optional delta + hint line.
 * Standalone it draws its own border; inside `StatGroup` it is borderless.
 */
export function Stat({
  label,
  value,
  unit,
  delta,
  hint,
  icon,
  className,
  ...props
}: Omit<React.ComponentProps<"div">, "children"> & {
  label: React.ReactNode
  value: React.ReactNode
  /** Quiet suffix after the value: "USDT", "%". */
  unit?: React.ReactNode
  delta?: React.ComponentProps<typeof Delta>
  hint?: React.ReactNode
  icon?: React.ReactNode
}) {
  return (
    <div
      data-slot="stat"
      className={cn(
        "flex min-w-0 flex-col gap-2 rounded-xl border border-border bg-card p-4 shadow-card lg:p-5",
        className,
      )}
      {...props}
    >
      <div className="flex items-center justify-between gap-2 text-[13px] leading-5 text-muted-foreground">
        <span className="truncate">{label}</span>
        {icon && <span className="shrink-0 text-faint [&_svg]:size-4">{icon}</span>}
      </div>
      <div className="num flex items-baseline gap-1.5 text-2xl leading-8 font-medium tracking-[-0.04em]">
        <span className="truncate">{value}</span>
        {unit && <span className="text-sm font-normal tracking-normal text-muted-foreground">{unit}</span>}
      </div>
      {(delta || hint) && (
        <div className="flex flex-wrap items-center gap-x-1.5 text-xs leading-5 text-muted-foreground">
          {delta && <Delta {...delta} />}
          {hint && <span>{hint}</span>}
        </div>
      )}
    </div>
  )
}
