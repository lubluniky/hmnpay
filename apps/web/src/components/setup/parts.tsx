"use client"

import * as React from "react"
import Link from "next/link"
import { ArrowLeftIcon, ArrowRightIcon, CornerDownLeftIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

import { STEP_IDS, STEPS, type StepId } from "./data"

export const stepHref = (step: StepId) => `/setup?step=${step}`

/* -------------------------------------------------------------------------- */
/* Step header                                                                */
/* -------------------------------------------------------------------------- */

export function StepHeader({
  title,
  children,
  className,
}: {
  title: React.ReactNode
  children?: React.ReactNode
  className?: string
}) {
  return (
    <header className={className}>
      <h1 className="text-[22px] leading-7 font-semibold tracking-[-0.025em] text-balance lg:text-2xl lg:leading-8">
        {title}
      </h1>
      {children && (
        <p className="mt-2 max-w-[64ch] text-sm leading-6 text-pretty text-muted-foreground">{children}</p>
      )}
    </header>
  )
}

/* -------------------------------------------------------------------------- */
/* Progress: vertical rail on desktop, segments on mobile                     */
/* -------------------------------------------------------------------------- */

export type RailState = "done" | "current" | "todo"

export interface RailItem {
  id: StepId
  state: RailState
  /** One-line summary of what was entered, shown once the step is done. */
  summary?: React.ReactNode
  /** Earlier steps and the next open one can be jumped to. */
  reachable: boolean
}

function Marker({ state, final }: { state: RailState; final: boolean }) {
  return (
    <span aria-hidden="true" className="relative mt-0.5 flex size-4 shrink-0 items-center justify-center">
      {state === "done" && (
        <span className={cn("flex size-4 items-center justify-center rounded-full", final ? "bg-success" : "bg-foreground")}>
          <svg viewBox="0 0 12 12" fill="none" className="size-2.5 text-background">
            <path d="M2.5 6.2l2.3 2.3 4.7-5" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
          </svg>
        </span>
      )}
      {state === "current" && <span className="size-2 rounded-full bg-foreground ring-[3px] ring-neutral-subtle" />}
      {state === "todo" && <span className="size-2 rounded-full border border-border-strong" />}
    </span>
  )
}

/**
 * The setup rail: same marker geometry as the checkout's `ProgressRail`, but every step
 * is a link and a finished step keeps a one-line receipt of what was entered.
 */
export function SetupRail({ items, className }: { items: RailItem[]; className?: string }) {
  return (
    <ol className={cn("flex flex-col", className)}>
      {items.map((item, i) => {
        const last = i === items.length - 1
        const body = (
          <>
            <Marker state={item.state} final={last} />
            <span className="min-w-0 flex-1">
              <span
                className={cn(
                  "block text-[13px] leading-5",
                  item.state === "todo" ? "text-faint" : "text-foreground",
                  item.state === "current" && "font-medium",
                )}
              >
                {STEPS[item.id].label}
              </span>
              {item.summary && item.state === "done" && (
                <span className="block truncate text-xs leading-4 text-muted-foreground">{item.summary}</span>
              )}
            </span>
          </>
        )
        return (
          <li key={item.id} className="relative pb-5 last:pb-0" aria-current={item.state === "current" ? "step" : undefined}>
            {!last && (
              <span
                aria-hidden="true"
                className={cn("absolute top-5 bottom-0 left-[7.5px] w-px", item.state === "done" ? "bg-border-strong" : "bg-border")}
              />
            )}
            {item.reachable && item.state !== "current" ? (
              <Link
                href={stepHref(item.id)}
                className="focus-ring relative -mx-2 flex gap-3 rounded-md px-2 transition-colors hover:bg-muted/60"
              >
                {body}
              </Link>
            ) : (
              <span className="relative flex gap-3">{body}</span>
            )}
          </li>
        )
      })}
    </ol>
  )
}

/** Mobile: "Step 3 of 5" and five segments. */
export function SetupSegments({ step, className }: { step: StepId; className?: string }) {
  const index = STEP_IDS.indexOf(step)
  return (
    <div
      role="progressbar"
      aria-label="Setup progress"
      aria-valuemin={1}
      aria-valuemax={STEP_IDS.length}
      aria-valuenow={index + 1}
      aria-valuetext={`Step ${index + 1} of ${STEP_IDS.length}: ${STEPS[step].label}`}
      className={cn("flex gap-1", className)}
    >
      {STEP_IDS.map((id, i) => (
        <span
          key={id}
          className={cn(
            "h-1 flex-1 rounded-full transition-colors duration-300",
            i < index ? "bg-foreground" : i === index ? (step === "done" ? "bg-success" : "bg-foreground") : "bg-muted",
          )}
        />
      ))}
    </div>
  )
}

/* -------------------------------------------------------------------------- */
/* Back / continue                                                            */
/* -------------------------------------------------------------------------- */

/**
 * Bottom of every step: back on the left, the one primary action on the right.
 * The primary button is the form's submit, so Enter anywhere in the step continues.
 */
export function StepNav({
  back,
  label = "Continue",
  hint,
  busy = false,
  className,
}: {
  back?: StepId
  label?: string
  /** Why you can't continue yet, next to the button. */
  hint?: React.ReactNode
  busy?: boolean
  className?: string
}) {
  return (
    <div className={cn("mt-auto pt-7", className)}>
      <div className="flex items-center gap-3 border-t border-border pt-4">
        {back && (
          <Button asChild variant="ghost" className="h-11 px-3 text-sm text-muted-foreground hover:text-foreground max-sm:size-11 max-sm:border-border max-sm:px-0">
            <Link href={stepHref(back)} aria-label="Back">
              <ArrowLeftIcon />
              <span className="max-sm:hidden">Back</span>
            </Link>
          </Button>
        )}
        {hint && (
          <p aria-live="polite" className="ml-auto hidden text-right text-[13px] leading-5 text-muted-foreground sm:block">
            {hint}
          </p>
        )}
        <Button
          type="submit"
          disabled={busy}
          data-wave-source=""
          className={cn("h-11 px-4 text-sm max-sm:flex-1 sm:min-w-36", !hint && "ml-auto")}
        >
          {label}
          <ArrowRightIcon data-icon="inline-end" className="lg:pointer-fine:hidden" />
          <kbd
            aria-hidden="true"
            className="ml-1 hidden h-5 items-center rounded-[5px] border border-primary-foreground/25 px-1 text-primary-foreground/70 lg:pointer-fine:inline-flex"
          >
            <CornerDownLeftIcon className="size-3" />
          </kbd>
        </Button>
      </div>
      {hint && <p className="mt-3 text-center text-[13px] leading-5 text-muted-foreground sm:hidden">{hint}</p>}
    </div>
  )
}
