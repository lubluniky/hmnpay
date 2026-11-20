"use client"

import * as React from "react"
import { CopyIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

import { useSound } from "./sound"

async function writeClipboard(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    // Self-hosted instances are sometimes reached over plain http, where the async API is unavailable.
    try {
      const el = document.createElement("textarea")
      el.value = text
      el.setAttribute("readonly", "")
      el.style.position = "fixed"
      el.style.opacity = "0"
      document.body.appendChild(el)
      el.select()
      const ok = document.execCommand("copy")
      el.remove()
      return ok
    } catch {
      return false
    }
  }
}

/** Copy-to-clipboard state: `copied` flips back after `resetMs`. A successful copy plays the copy cue. */
export function useCopy(resetMs = 1800) {
  const [copied, setCopied] = React.useState(false)
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null)
  const play = useSound()

  React.useEffect(() => () => void (timer.current && clearTimeout(timer.current)), [])

  const copy = React.useCallback(
    async (text: string) => {
      const ok = await writeClipboard(text)
      if (!ok) return false
      play("copy")
      setCopied(true)
      if (timer.current) clearTimeout(timer.current)
      timer.current = setTimeout(() => setCopied(false), resetMs)
      return true
    },
    [resetMs, play],
  )

  return { copied, copy }
}

/**
 * Copy icon that gives way to a check which draws itself. Shared by every copy affordance.
 * Under reduced motion the two simply cross-fade.
 */
export function CopyGlyph({ copied, className }: { copied: boolean; className?: string }) {
  return (
    <span aria-hidden="true" className={cn("relative inline-grid size-4 shrink-0 place-items-center", className)}>
      <CopyIcon
        className={cn(
          "col-start-1 row-start-1 size-full transition-[opacity,scale] duration-fast ease-out-quint",
          copied && "scale-75 opacity-0",
        )}
      />
      <svg
        viewBox="0 0 24 24"
        fill="none"
        className={cn(
          "col-start-1 row-start-1 size-full text-success-fg transition-opacity duration-fast ease-out-quint",
          !copied && "opacity-0",
        )}
      >
        <path
          d="M4.5 12.5l5 5L19.5 7"
          stroke="currentColor"
          strokeWidth="2.5"
          strokeLinecap="round"
          strokeLinejoin="round"
          pathLength={1}
          strokeDasharray={1}
          strokeDashoffset={copied ? 0 : 1}
          // Draws in after the copy icon has let go; resets unseen.
          className={cn("motion-reduce:transition-none", copied && "transition-[stroke-dashoffset] delay-75 duration-[260ms] ease-out-quint")}
        />
      </svg>
    </span>
  )
}

/**
 * Copy button. Icon-only by default; pass `children` for a labelled button
 * (the label swaps to `copiedLabel` while copied).
 * `label` is the accessible name for the icon-only form ("Copy address").
 */
export function CopyButton({
  value,
  label = "Copy",
  copiedLabel = "Copied",
  onCopied,
  variant = "ghost",
  size,
  className,
  children,
  ...props
}: Omit<React.ComponentProps<typeof Button>, "value" | "onClick"> & {
  value: string
  label?: string
  copiedLabel?: string
  onCopied?: () => void
}) {
  const { copied, copy } = useCopy()
  const iconOnly = children === undefined

  return (
    <Button
      type="button"
      variant={variant}
      size={size ?? (iconOnly ? "icon-sm" : "sm")}
      aria-label={iconOnly ? (copied ? copiedLabel : label) : undefined}
      data-copied={copied || undefined}
      className={cn(iconOnly && "text-muted-foreground hover:text-foreground", className)}
      onClick={async () => {
        if (await copy(value)) onCopied?.()
      }}
      {...props}
    >
      <CopyGlyph copied={copied} className="size-3.5" />
      {!iconOnly && <span>{copied ? copiedLabel : children}</span>}
      <span className="sr-only" role="status">
        {copied ? copiedLabel : ""}
      </span>
    </Button>
  )
}
