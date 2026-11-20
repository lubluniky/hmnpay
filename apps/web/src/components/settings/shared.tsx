"use client"

import * as React from "react"
import Link from "next/link"
import { CopyButton, toneClasses, type Tone } from "@/components/hmn"
import { Button as BaseButton } from "@/components/ui/button"
import {
  CodeBlock,
  CodeBlockActions,
  CodeBlockContent,
  CodeBlockHeader,
  CodeBlockLine,
  CodeBlockTitle,
} from "@/components/ui/code-block"
import { Input as BaseInput } from "@/components/ui/input"
import { InputGroup as BaseInputGroup } from "@/components/ui/input-group"
import { SelectTrigger as BaseSelectTrigger } from "@/components/ui/select"
import { useHighlightedCode } from "@/hooks/use-highlighted-code"
import { cn } from "@/lib/utils"

/* -------------------------------------------------------------------------------------------
 * Touch-sized controls
 *
 * Dashboard controls are 32px high; on coarse pointers they grow to 40px (44px with the gap
 * around them). The stock shadcn controls don't do this, so the settings screens import these
 * thin wrappers instead. Same props, same look.
 * ----------------------------------------------------------------------------------------- */

export function Button({ className, size, ...props }: React.ComponentProps<typeof BaseButton>) {
  const icon = typeof size === "string" && size.startsWith("icon")
  return (
    <BaseButton
      size={size}
      className={cn(icon ? "pointer-coarse:size-10" : "pointer-coarse:h-10 pointer-coarse:px-3.5 pointer-coarse:text-sm", className)}
      {...props}
    />
  )
}

export function Input({ className, ...props }: React.ComponentProps<typeof BaseInput>) {
  return <BaseInput className={cn("pointer-coarse:h-10", className)} {...props} />
}

export function InputGroup({ className, ...props }: React.ComponentProps<typeof BaseInputGroup>) {
  return <BaseInputGroup className={cn("pointer-coarse:h-10", className)} {...props} />
}

export function SelectTrigger({ className, ...props }: React.ComponentProps<typeof BaseSelectTrigger>) {
  return <BaseSelectTrigger className={cn("pointer-coarse:data-[size=default]:h-10", className)} {...props} />
}

/* -------------------------------------------------------------------------------------------
 * URL-addressable overlays
 * ----------------------------------------------------------------------------------------- */

/**
 * A query parameter of the page as it was opened ("?new=1"), for overlays a link can open.
 * The dashboard's pages are first rendered in the browser (the shell waits for the session),
 * so the address bar can be read when their state is created.
 */
export function initialParam(name: string): string | null {
  return typeof window === "undefined" ? null : new URLSearchParams(window.location.search).get(name)
}

/** Drop query params without a navigation (used when a URL-opened dialog closes). */
export function stripParams(...names: string[]) {
  if (typeof window === "undefined") return
  const url = new URL(window.location.href)
  for (const name of names) url.searchParams.delete(name)
  window.history.replaceState(null, "", `${url.pathname}${url.search}`)
}

/**
 * Open state for a dialog/sheet that can also be opened by a query param (`?new=1`).
 * Closing it removes the param(s), so a refresh doesn't reopen it.
 */
export function useUrlOverlay(initiallyOpen: boolean, ...params: string[]) {
  const [open, setOpenState] = React.useState(initiallyOpen)
  const setOpen = React.useCallback(
    (next: boolean) => {
      setOpenState(next)
      if (!next) stripParams(...params)
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps -- params are literals at every call site
    [],
  )
  return [open, setOpen] as const
}

/* -------------------------------------------------------------------------------------------
 * Small display pieces
 * ----------------------------------------------------------------------------------------- */

type SegmentedOption<T extends string> = { value: T; label: React.ReactNode; href?: string; count?: number }

/**
 * Segmented control: 2–4 mutually exclusive views or modes. Options with `href` render as
 * links (URL-addressable), the rest as radio buttons calling `onValueChange`.
 */
export function Segmented<T extends string>({
  value,
  onValueChange,
  options,
  className,
  ...props
}: Omit<React.ComponentProps<"div">, "onChange"> & {
  value: T
  onValueChange?: (value: T) => void
  options: SegmentedOption<T>[]
}) {
  return (
    <div
      role="radiogroup"
      data-slot="segmented"
      className={cn("inline-flex h-8 w-fit items-center gap-0.5 rounded-lg border border-border bg-sunken p-0.5 pointer-coarse:h-10", className)}
      {...props}
    >
      {options.map((option) => {
        const active = option.value === value
        const itemClass = cn(
          "focus-ring inline-flex h-full items-center justify-center gap-1.5 rounded-md px-2.5 text-[13px] font-medium whitespace-nowrap transition-colors pointer-coarse:px-3.5",
          active
            ? "bg-card text-foreground shadow-card ring-1 ring-border dark:bg-accent"
            : "text-muted-foreground hover:text-foreground",
        )
        const content = (
          <>
            {option.label}
            {option.count !== undefined && (
              <span className={cn("num text-xs font-normal", active ? "text-muted-foreground" : "text-faint")}>{option.count}</span>
            )}
          </>
        )
        return option.href ? (
          <Link key={option.value} href={option.href} scroll={false} role="radio" aria-checked={active} className={itemClass}>
            {content}
          </Link>
        ) : (
          <button
            key={option.value}
            type="button"
            role="radio"
            aria-checked={active}
            className={itemClass}
            onClick={() => onValueChange?.(option.value)}
          >
            {content}
          </button>
        )
      })}
    </div>
  )
}

/** A thin horizontal meter. `tone="neutral"` fills with ink; status tones use the solid tone colour. */
export function Meter({
  value,
  max = 1,
  tone = "neutral",
  label,
  className,
}: {
  value: number
  max?: number
  tone?: Tone
  label: string
  className?: string
}) {
  const ratio = Math.max(0, Math.min(1, max > 0 ? value / max : 0))
  return (
    <div
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={max}
      aria-valuenow={Math.min(value, max)}
      className={cn("h-1.5 w-full overflow-hidden rounded-full bg-muted", className)}
    >
      <div
        className={cn("h-full rounded-full", tone === "neutral" ? "bg-foreground" : toneClasses[tone].solid)}
        style={{ width: `${(ratio * 100).toFixed(2)}%` }}
      />
    </div>
  )
}

/** Small mono chip for machine names: scopes, event types, header names. */
export function MonoTag({ className, ...props }: React.ComponentProps<"span">) {
  return (
    <span
      data-slot="mono-tag"
      className={cn(
        "num inline-flex h-5 w-fit shrink-0 items-center rounded-md border border-border bg-muted/50 px-1.5 text-[11px] leading-none whitespace-nowrap text-muted-foreground",
        className,
      )}
      {...props}
    />
  )
}

/** A quiet 13px label above a block inside a section or sheet. */
export function BlockLabel({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("text-[13px] leading-5 font-medium text-foreground", className)} {...props} />
}

/** Icon in a hairline tile, used to lead list rows (keys, endpoints). */
export function IconTile({ className, ...props }: React.ComponentProps<"span">) {
  return (
    <span
      aria-hidden="true"
      className={cn(
        "flex size-8 shrink-0 items-center justify-center rounded-lg border border-border bg-background/60 text-muted-foreground [&_svg]:size-4",
        className,
      )}
      {...props}
    />
  )
}

/**
 * Syntax-highlighted snippet on the shared `code-block`, with the hmnpay copy button.
 * `title` adds a header row; without it the copy button floats top-right.
 */
export function CodeSnippet({
  code,
  language,
  title,
  wrap = false,
  copy = true,
  className,
  contentClassName,
}: {
  code: string
  language: string
  title?: React.ReactNode
  wrap?: boolean
  copy?: boolean
  className?: string
  contentClassName?: string
}) {
  const lines = useHighlightedCode(code, language)
  const copyButton = copy ? <CopyButton value={code} label="Copy code" size="icon-xs" /> : null
  return (
    <CodeBlock size="sm" className={cn("border-border bg-background/60 dark:bg-background/60", className)}>
      {title ? (
        <CodeBlockHeader className="border-b border-border pb-2.5">
          <CodeBlockTitle className="font-normal text-muted-foreground">{title}</CodeBlockTitle>
          {copyButton && <CodeBlockActions>{copyButton}</CodeBlockActions>}
        </CodeBlockHeader>
      ) : (
        copyButton && <CodeBlockActions>{copyButton}</CodeBlockActions>
      )}
      <CodeBlockContent wrap={wrap} className={cn("num text-[12.5px] leading-5 tracking-normal", contentClassName)}>
        {lines.map((line, index) => (
          <CodeBlockLine key={index}>{line}</CodeBlockLine>
        ))}
      </CodeBlockContent>
    </CodeBlock>
  )
}
