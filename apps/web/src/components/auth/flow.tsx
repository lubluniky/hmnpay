"use client"

import * as React from "react"
import { CircleAlertIcon, EyeIcon, EyeOffIcon } from "lucide-react"

import { PoweredBy, PROJECT_URL, SoundToggle, ThemeToggle } from "@/components/hmn"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

/**
 * Building blocks for the chrome-less operator flows (first-run setup, sign-in).
 * Same family as the hosted checkout: one hero card, 44px controls, a quiet footer under it.
 */

/** Placeholder until the docs site exists (the checkout footer points at the same org). */
export const DOCS_URL = `${PROJECT_URL}#readme`

/** 44px text control. 16px text on phones so iOS does not zoom the page on focus. */
export const flowControl =
  "h-11 w-full min-w-0 rounded-lg border border-input bg-transparent px-3.5 text-base outline-none transition-colors placeholder:text-faint focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 aria-invalid:border-danger aria-invalid:ring-3 aria-invalid:ring-danger/20 md:text-sm dark:bg-input/30"

/**
 * Focus a field when its view appears, but only with a mouse / trackpad: on a phone it would
 * throw the keyboard over a screen the person has not read yet.
 */
export function useAutoFocus<T extends HTMLElement>(enabled: boolean) {
  const ref = React.useRef<T>(null)
  const initial = React.useRef(enabled)
  React.useEffect(() => {
    if (initial.current && window.matchMedia("(pointer: fine)").matches) ref.current?.focus({ preventScroll: true })
  }, [])
  return ref
}

export function FlowInput({ className, ...props }: React.ComponentProps<"input">) {
  return <input data-slot="flow-input" className={cn(flowControl, className)} {...props} />
}

/**
 * Label + control + one line underneath. The line is the hint until there is an error,
 * then it is the error: the field never grows a second message.
 */
export function FlowField({
  label,
  htmlFor,
  optional = false,
  action,
  hint,
  error,
  className,
  children,
}: {
  label: React.ReactNode
  htmlFor: string
  optional?: boolean
  /** Right of the label: "Forgot password?", "Where do I find it?". */
  action?: React.ReactNode
  hint?: React.ReactNode
  error?: React.ReactNode
  className?: string
  children: React.ReactNode
}) {
  return (
    <div data-slot="flow-field" className={cn("flex flex-col gap-2", className)}>
      <div className="flex items-baseline justify-between gap-3">
        <label htmlFor={htmlFor} className="text-[13px] leading-5 font-medium">
          {label}
          {optional && <span className="ml-1.5 font-normal text-faint">Optional</span>}
        </label>
        {action && <div className="shrink-0 text-[13px] leading-5">{action}</div>}
      </div>
      {children}
      {error ? (
        <FieldMessage id={`${htmlFor}-message`} tone="danger">
          {error}
        </FieldMessage>
      ) : (
        hint && <FieldMessage id={`${htmlFor}-message`}>{hint}</FieldMessage>
      )}
    </div>
  )
}

export function FieldMessage({
  tone = "neutral",
  className,
  children,
  ...props
}: React.ComponentProps<"p"> & { tone?: "neutral" | "danger" }) {
  return (
    <p
      role={tone === "danger" ? "alert" : undefined}
      className={cn(
        "flex gap-1.5 text-[13px] leading-5 text-pretty",
        tone === "danger" ? "text-danger-fg" : "text-muted-foreground",
        className,
      )}
      {...props}
    >
      {tone === "danger" && <CircleAlertIcon aria-hidden="true" className="mt-[3px] size-3.5 shrink-0" />}
      <span className="min-w-0">{children}</span>
    </p>
  )
}

/** Password control with a show / hide toggle instead of a "confirm password" field. */
export function PasswordInput({ className, ...props }: Omit<React.ComponentProps<"input">, "type">) {
  const [visible, setVisible] = React.useState(false)
  return (
    <div className="relative">
      <FlowInput type={visible ? "text" : "password"} className={cn("pr-11", className)} {...props} />
      <span className="absolute inset-y-0 right-1 flex items-center">
        <Button
          type="button"
          variant="ghost"
          aria-label={visible ? "Hide password" : "Show password"}
          aria-pressed={visible}
          onClick={() => setVisible((v) => !v)}
          className="size-9 text-muted-foreground hover:text-foreground"
        >
          {visible ? <EyeOffIcon /> : <EyeIcon />}
        </Button>
      </span>
    </div>
  )
}

/** Inline text link in the flow screens (the one place the brand accent is allowed: links). */
export function FlowLink({ className, ...props }: React.ComponentProps<"a">) {
  return (
    <a
      className={cn(
        "focus-ring rounded-sm text-muted-foreground underline decoration-border-strong underline-offset-4 transition-colors hover:text-foreground hover:decoration-current",
        className,
      )}
      {...props}
    />
  )
}

/**
 * Under the hero card: the attribution on the left, docs and theme on the right. (The version
 * is not shown here: these pages are public, and the server tells its version only to a
 * signed-in operator.)
 */
export function FlowFooter({ className }: { className?: string }) {
  return (
    <footer className={cn("mt-3 flex items-center justify-between gap-4 pr-0.5 pl-2 lg:mt-4", className)}>
      <PoweredBy />
      <div className="flex items-center gap-1">
        <Button
          asChild
          variant="ghost"
          className="h-9 px-2.5 text-[13px] font-normal text-muted-foreground hover:text-foreground pointer-coarse:h-11 pointer-coarse:px-3"
        >
          <a href={`${PROJECT_URL}#readme`} target="_blank" rel="noopener noreferrer">
            Docs
          </a>
        </Button>
        <SoundToggle className="size-9 pointer-coarse:size-11" />
        <ThemeToggle className="size-9 pointer-coarse:size-11" />
      </div>
    </footer>
  )
}
