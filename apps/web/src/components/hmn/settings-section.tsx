import { cn } from "@/lib/utils"

/**
 * The settings card: title + one-sentence description, the controls, and a footer bar
 * with a hint on the left and the save action on the right. One concern per section.
 * `tone="danger"` for destructive zones.
 */
export function SettingsSection({
  title,
  description,
  footer,
  actions,
  tone = "default",
  className,
  children,
  ...props
}: Omit<React.ComponentProps<"section">, "title"> & {
  title: React.ReactNode
  description?: React.ReactNode
  /** Hint text in the footer bar (constraints, docs link). */
  footer?: React.ReactNode
  /** Footer actions, usually a single "Save" button. */
  actions?: React.ReactNode
  tone?: "default" | "danger"
}) {
  const danger = tone === "danger"
  return (
    <section
      data-slot="settings-section"
      className={cn(
        "overflow-hidden rounded-xl border bg-card shadow-card",
        danger ? "border-danger-border" : "border-border",
        className,
      )}
      {...props}
    >
      <div className="flex flex-col gap-5 p-5 sm:p-6">
        <header>
          <h2 className="text-base leading-6 font-semibold tracking-[-0.015em]">{title}</h2>
          {description && (
            <p className="mt-1 max-w-2xl text-sm leading-6 text-pretty text-muted-foreground">{description}</p>
          )}
        </header>
        {children}
      </div>
      {(footer || actions) && (
        <footer
          className={cn(
            "flex min-h-13 flex-wrap items-center justify-between gap-x-6 gap-y-2 border-t px-5 py-3 text-[13px] leading-5 text-muted-foreground sm:px-6",
            danger ? "border-danger-border bg-danger-subtle" : "border-border bg-muted/40",
          )}
        >
          <div className="min-w-0">{footer}</div>
          {actions && <div className="ml-auto flex shrink-0 items-center gap-2">{actions}</div>}
        </footer>
      )}
    </section>
  )
}

/**
 * A label-left / control-right row inside a `SettingsSection` (toggles, read-only values).
 * Rows are separated by hairlines; stack them directly.
 */
export function SettingsRow({
  label,
  description,
  className,
  children,
  ...props
}: React.ComponentProps<"div"> & { label: React.ReactNode; description?: React.ReactNode }) {
  return (
    <div
      data-slot="settings-row"
      className={cn(
        "flex flex-col gap-3 border-t border-border py-4 first:border-t-0 first:pt-0 last:pb-0 sm:flex-row sm:items-center sm:justify-between sm:gap-8",
        className,
      )}
      {...props}
    >
      <div className="min-w-0">
        <div className="text-sm leading-5 font-medium">{label}</div>
        {description && <div className="mt-0.5 text-[13px] leading-5 text-muted-foreground">{description}</div>}
      </div>
      <div className="flex shrink-0 items-center gap-2">{children}</div>
    </div>
  )
}
