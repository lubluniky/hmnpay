import { cn } from "@/lib/utils"

/**
 * Neutral wireframe block for pages that are designed in a later pass.
 * It reserves the shape of the content so the shell can be judged; delete it when the page is built.
 */
export function PlaceholderBlock({
  label,
  className,
  ...props
}: React.ComponentProps<"div"> & { label: string }) {
  return (
    <div
      data-slot="placeholder"
      className={cn(
        "flex min-h-24 items-center justify-center rounded-xl border border-dashed border-border-strong bg-card/40 px-4 text-center",
        className,
      )}
      {...props}
    >
      <span className="num text-xs text-faint">{label}</span>
    </div>
  )
}

const layouts = {
  overview: (
    <>
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        {["Volume", "Paid payments", "Conversion", "Average ticket"].map((label) => (
          <PlaceholderBlock key={label} label={label} className="min-h-28" />
        ))}
      </div>
      <PlaceholderBlock label="Volume chart" className="min-h-72" />
      <PlaceholderBlock label="Recent payments" className="min-h-80" />
    </>
  ),
  table: (
    <>
      <div className="flex gap-3">
        <PlaceholderBlock label="Search" className="min-h-9 flex-1 rounded-lg sm:max-w-xs" />
        <PlaceholderBlock label="Filters" className="min-h-9 w-28 rounded-lg" />
      </div>
      <PlaceholderBlock label="Table" className="min-h-[520px]" />
    </>
  ),
  detail: (
    <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_320px]">
      <div className="space-y-6">
        <PlaceholderBlock label="Summary" className="min-h-48" />
        <PlaceholderBlock label="Timeline" className="min-h-72" />
      </div>
      <PlaceholderBlock label="Details" className="min-h-96" />
    </div>
  ),
  settings: (
    <>
      <PlaceholderBlock label="Settings section" className="min-h-52" />
      <PlaceholderBlock label="Settings section" className="min-h-52" />
      <PlaceholderBlock label="Settings section" className="min-h-40" />
    </>
  ),
} as const

export function PagePlaceholder({ layout = "table", className }: { layout?: keyof typeof layouts; className?: string }) {
  return <div className={cn("space-y-6", className)}>{layouts[layout]}</div>
}
