import type { PaymentStatus } from "@/lib/api/types"
import { FINALISING } from "@/lib/finality"
import { cn } from "@/lib/utils"

import { Roll } from "./motion"

export type Tone = "neutral" | "info" | "success" | "warning" | "danger" | "brand"

/** Class recipes per tone. Use these instead of hand-writing colour combinations. */
export const toneClasses: Record<
  Tone,
  { badge: string; solid: string; text: string; subtle: string; border: string; ring: string }
> = {
  neutral: {
    badge: "border-neutral-border bg-neutral-subtle text-neutral-fg",
    solid: "bg-neutral",
    text: "text-neutral-fg",
    subtle: "bg-neutral-subtle",
    border: "border-neutral-border",
    ring: "ring-neutral-subtle",
  },
  info: {
    badge: "border-info-border bg-info-subtle text-info-fg",
    solid: "bg-info",
    text: "text-info-fg",
    subtle: "bg-info-subtle",
    border: "border-info-border",
    ring: "ring-info-subtle",
  },
  success: {
    badge: "border-success-border bg-success-subtle text-success-fg",
    solid: "bg-success",
    text: "text-success-fg",
    subtle: "bg-success-subtle",
    border: "border-success-border",
    ring: "ring-success-subtle",
  },
  warning: {
    badge: "border-warning-border bg-warning-subtle text-warning-fg",
    solid: "bg-warning",
    text: "text-warning-fg",
    subtle: "bg-warning-subtle",
    border: "border-warning-border",
    ring: "ring-warning-subtle",
  },
  danger: {
    badge: "border-danger-border bg-danger-subtle text-danger-fg",
    solid: "bg-danger",
    text: "text-danger-fg",
    subtle: "bg-danger-subtle",
    border: "border-danger-border",
    ring: "ring-danger-subtle",
  },
  brand: {
    badge: "border-brand-border bg-brand-subtle text-brand-fg",
    solid: "bg-brand",
    text: "text-brand-fg",
    subtle: "bg-brand-subtle",
    border: "border-brand-border",
    ring: "ring-brand-subtle",
  },
}

/**
 * A status dot. `live` adds a slow radar ping (only for things that are happening right now).
 * `hollow` draws a ring instead of a disc (ended / inactive).
 */
export function StatusDot({
  tone = "neutral",
  live = false,
  hollow = false,
  className,
  ...props
}: React.ComponentProps<"span"> & { tone?: Tone; live?: boolean; hollow?: boolean }) {
  return (
    <span aria-hidden="true" className={cn("relative inline-flex size-1.5 shrink-0", className)} {...props}>
      {live && (
        <span
          className={cn("absolute inset-0 rounded-full motion-safe:animate-live-ping", toneClasses[tone].solid)}
        />
      )}
      <span
        className={cn(
          "relative size-full rounded-full",
          hollow ? "border border-current bg-transparent opacity-70" : toneClasses[tone].solid,
        )}
      />
    </span>
  )
}

/**
 * Generic status pill: tinted background + hairline + dot. For anything with a state
 * (webhook deliveries, API keys, node health…). Payments use `PaymentStatusBadge`.
 */
export function StatusBadge({
  tone = "neutral",
  dot = true,
  live = false,
  hollow = false,
  size = "md",
  className,
  children,
  ...props
}: React.ComponentProps<"span"> & {
  tone?: Tone
  dot?: boolean
  live?: boolean
  hollow?: boolean
  size?: "sm" | "md"
}) {
  return (
    <span
      data-slot="status-badge"
      data-tone={tone}
      className={cn(
        "inline-flex w-fit shrink-0 items-center rounded-full border font-medium whitespace-nowrap",
        size === "sm" ? "h-5 gap-1.5 px-2 text-[11px]" : "h-[22px] gap-1.5 px-2 text-xs",
        toneClasses[tone].badge,
        className,
      )}
      {...props}
    >
      {dot && <StatusDot tone={tone} live={live} hollow={hollow} />}
      {children}
    </span>
  )
}

/** Single source of truth for how each payment status reads and looks. */
export const paymentStatusMeta: Record<
  PaymentStatus,
  { label: string; tone: Tone; live: boolean; hollow?: boolean; description: string }
> = {
  awaiting: { label: "Awaiting", tone: "neutral", live: true, description: "Waiting for the customer to send funds." },
  detected: { label: "Detected", tone: "info", live: true, description: "Part of the amount has arrived; the customer can send the rest." },
  confirming: {
    label: "Confirming",
    tone: "info",
    live: true,
    description: "The full amount arrived; collecting block confirmations, then waiting for the chain to finalise its block.",
  },
  paid: { label: "Paid", tone: "success", live: false, description: "Confirmed in full." },
  underpaid: { label: "Underpaid", tone: "warning", live: false, description: "The time ran out with less than the amount received." },
  overpaid: { label: "Overpaid", tone: "warning", live: false, description: "Received more than requested." },
  expired: { label: "Expired", tone: "neutral", live: false, hollow: true, description: "Not paid in time, or cancelled." },
  failed: { label: "Failed", tone: "danger", live: false, description: "The transfer that was seen dropped out of the chain." },
}

/**
 * The payment status pill. Pass `confirmations`/`required` to show "Confirming 7/15", and
 * `finalising` when the confirmations are complete and only the chain's finality is missing: it
 * then reads "Finalising", without a counter that would look stuck at 15/15.
 * In-flight states pulse by default; pass `live={false}` where something else on screen
 * already carries the heartbeat (one pulse per view).
 */
export function PaymentStatusBadge({
  status,
  confirmations,
  required,
  finalising,
  live,
  size,
  className,
  ...props
}: Omit<React.ComponentProps<typeof StatusBadge>, "tone" | "hollow" | "children"> & {
  status: PaymentStatus
  confirmations?: number
  required?: number
  finalising?: boolean
}) {
  const meta = paymentStatusMeta[status]
  const final = finalising && (status === "confirming" || status === "detected")
  const showCount = !final && status === "confirming" && confirmations !== undefined && required !== undefined
  return (
    <StatusBadge
      tone={meta.tone}
      live={live ?? meta.live}
      hollow={meta.hollow}
      size={size}
      className={className}
      title={final ? FINALISING.description : meta.description}
      data-finalising={final ? "" : undefined}
      {...props}
    >
      {final ? FINALISING.label : meta.label}
      {showCount && (
        <span className="num opacity-80">
          <Roll value={String(confirmations)} />/{required}
        </span>
      )}
    </StatusBadge>
  )
}
