import { cn } from "@/lib/utils"

/**
 * Empty / zero-data pattern: icon tile, one-line title, one sentence, at most two actions.
 * `bordered` (default) draws a dashed frame for use directly on the page;
 * turn it off inside a `Panel` or table body.
 */
export function EmptyState({
  icon,
  title,
  description,
  action,
  secondaryAction,
  bordered = true,
  className,
  ...props
}: Omit<React.ComponentProps<"div">, "title"> & {
  icon?: React.ReactNode
  title: React.ReactNode
  description?: React.ReactNode
  action?: React.ReactNode
  secondaryAction?: React.ReactNode
  bordered?: boolean
}) {
  return (
    <div
      data-slot="empty-state"
      className={cn(
        "flex flex-col items-center justify-center px-6 py-14 text-center",
        bordered && "rounded-xl border border-dashed border-border-strong",
        className,
      )}
      {...props}
    >
      {icon && (
        <div className="mb-4 flex size-10 items-center justify-center rounded-lg border border-border bg-card text-muted-foreground shadow-card [&_svg]:size-[18px]">
          {icon}
        </div>
      )}
      <h3 className="text-sm leading-6 font-semibold">{title}</h3>
      {description && <p className="mt-1 max-w-sm text-sm leading-6 text-pretty text-muted-foreground">{description}</p>}
      {(action || secondaryAction) && (
        <div className="mt-5 flex flex-wrap items-center justify-center gap-2">
          {secondaryAction}
          {action}
        </div>
      )}
    </div>
  )
}
