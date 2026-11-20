import { cn } from "@/lib/utils"

import { toneClasses, type Tone } from "./status"

/**
 * A quiet inline note: reassurance, a consequence, a next step. One icon, an optional
 * bold lead-in, one or two sentences. Not for errors that block the page (use an Alert dialog).
 */
export function Callout({
  tone = "neutral",
  icon,
  title,
  className,
  children,
  ...props
}: Omit<React.ComponentProps<"div">, "title"> & { tone?: Tone; icon?: React.ReactNode; title?: React.ReactNode }) {
  return (
    <div
      data-slot="callout"
      className={cn(
        "flex gap-3 rounded-lg border px-3.5 py-3 text-[13px] leading-5",
        toneClasses[tone].border,
        toneClasses[tone].subtle,
        className,
      )}
      {...props}
    >
      {icon && <span className={cn("mt-0.5 shrink-0 [&_svg]:size-4", toneClasses[tone].text)}>{icon}</span>}
      <div className="min-w-0 text-muted-foreground">
        {title && <p className="font-medium text-foreground">{title}</p>}
        {children}
      </div>
    </div>
  )
}
