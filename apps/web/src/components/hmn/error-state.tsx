"use client"

import { RotateCwIcon, TriangleAlertIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { describeError } from "@/lib/errors"
import { cn } from "@/lib/utils"

import { EmptyState } from "./empty-state"

/**
 * A view that could not load: what went wrong in one sentence, and a way to try again.
 * Same shape as `EmptyState`, so a failed panel keeps the place of the content it replaces.
 */
export function ErrorState({
  error,
  title = "Couldn’t load this",
  onRetry,
  bordered = true,
  className,
}: {
  error: unknown
  title?: React.ReactNode
  onRetry?: () => void
  bordered?: boolean
  className?: string
}) {
  return (
    <EmptyState
      role="alert"
      bordered={bordered}
      icon={<TriangleAlertIcon />}
      title={title}
      description={describeError(error)}
      action={
        onRetry && (
          <Button variant="outline" onClick={onRetry}>
            <RotateCwIcon data-icon="inline-start" />
            Try again
          </Button>
        )
      }
      className={cn(className)}
    />
  )
}

/** One line under a section that is showing older data because the last refresh failed. */
export function StaleNote({ error, className }: { error: unknown; className?: string }) {
  return (
    <p role="status" className={cn("flex items-center gap-2 text-[13px] leading-5 text-warning-fg", className)}>
      <TriangleAlertIcon className="size-3.5 shrink-0" aria-hidden="true" />
      <span>{describeError(error)} Showing the last data received.</span>
    </p>
  )
}
