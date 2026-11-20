import { cn } from "@/lib/utils"

/**
 * Label / value rows separated by hairlines: receipts, payment details, read-only config.
 * Labels are quiet sans; values are right-aligned and usually mono (`Amount`, `Address`, `TxHash`).
 */
export function DetailList({ className, ...props }: React.ComponentProps<"dl">) {
  return <dl data-slot="detail-list" className={cn("divide-y divide-border text-sm", className)} {...props} />
}

export function DetailRow({
  label,
  className,
  children,
  ...props
}: React.ComponentProps<"div"> & { label: React.ReactNode }) {
  return (
    <div
      data-slot="detail-row"
      className={cn("flex min-h-11 items-center justify-between gap-6 py-2", className)}
      {...props}
    >
      <dt className="shrink-0 text-muted-foreground">{label}</dt>
      <dd className="flex min-w-0 items-center justify-end text-right">{children}</dd>
    </div>
  )
}
