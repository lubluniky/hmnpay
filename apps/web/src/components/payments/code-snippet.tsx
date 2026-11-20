"use client"

import { CopyButton } from "@/components/hmn"
import { cn } from "@/lib/utils"

/**
 * A quiet, monochrome code block: sunken surface, mono, horizontal scroll, copy button.
 * Pass plain text as `code`; pass `children` to render the same text with your own emphasis
 * (use `text-muted-foreground` / `text-faint` spans — no syntax colours, the product is monochrome).
 */
export function CodeSnippet({
  code,
  label,
  copyLabel = "Copy",
  className,
  children,
  ...props
}: Omit<React.ComponentProps<"div">, "children"> & {
  code: string
  /** Optional caption on the header row ("cURL", "Metadata"). */
  label?: React.ReactNode
  copyLabel?: string
  children?: React.ReactNode
}) {
  return (
    <div
      data-slot="code-snippet"
      className={cn("relative min-w-0 overflow-hidden rounded-lg border border-border bg-sunken", className)}
      {...props}
    >
      {label ? (
        <div className="flex h-9 items-center justify-between gap-2 border-b border-border pr-1 pl-3 text-xs text-muted-foreground">
          <span className="truncate">{label}</span>
          <CopyButton value={code} label={copyLabel} size="icon-sm" />
        </div>
      ) : (
        <CopyButton value={code} label={copyLabel} size="icon-sm" className="absolute top-1 right-1 bg-sunken" />
      )}
      <pre className="num overflow-x-auto px-3 py-2.5 text-[12.5px] leading-5 tracking-normal">
        <code>{children ?? code}</code>
      </pre>
    </div>
  )
}

/** Pretty-printed JSON with quiet keys and punctuation. For flat or shallow objects (metadata, payloads). */
export function JsonSnippet({
  value,
  label,
  className,
}: {
  value: Record<string, unknown>
  label?: React.ReactNode
  className?: string
}) {
  const code = JSON.stringify(value, null, 2)
  return (
    <CodeSnippet code={code} label={label} className={className}>
      {code.split("\n").map((line, i) => {
        const match = line.match(/^(\s*)("(?:[^"\\]|\\.)*")(: )(.*?)(,?)$/)
        return (
          <span key={i} className="block">
            {match ? (
              <>
                {match[1]}
                <span className="text-muted-foreground">{match[2]}</span>
                <span className="text-faint">{match[3]}</span>
                {match[4]}
                <span className="text-faint">{match[5]}</span>
              </>
            ) : (
              <span className="text-faint">{line}</span>
            )}
          </span>
        )
      })}
    </CodeSnippet>
  )
}
