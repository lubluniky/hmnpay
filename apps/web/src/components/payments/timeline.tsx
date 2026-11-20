import { Amount, toneClasses, type Tone } from "@/components/hmn"
import { operatorFinalityLine } from "@/lib/finality"
import type { ApiEvent, Delivery, Payment, PaymentDetail } from "@/lib/api/types"
import { relativeTime, truncateMiddle } from "@/lib/format"
import { hostOf } from "@/lib/url"
import { cn } from "@/lib/utils"

import { amountDelta, formatStamp, isOpen } from "./lib"

/* -------------------------------------------------------------------------- */
/* Timeline primitive                                                         */
/* -------------------------------------------------------------------------- */

export interface TimelineItem {
  id: string
  title: React.ReactNode
  detail?: React.ReactNode
  /** ISO timestamp, shown right-aligned as "Nov 18, 11:55:04". */
  at?: string
  /** Shown instead of a time (e.g. "6/15", "in 24 minutes"). */
  meta?: React.ReactNode
  /**
   * done: happened. current: happening now. todo: not yet. stopped: ended here (✕).
   * alert: happened, but needs a look (!).
   */
  state: "done" | "current" | "todo" | "stopped" | "alert"
  tone?: Tone
}

function Marker({ state, tone }: { state: TimelineItem["state"]; tone?: Tone }) {
  const t = tone ?? "neutral"
  if (state === "done") {
    return (
      <span className={cn("flex size-4 items-center justify-center rounded-full", tone ? toneClasses[t].solid : "bg-foreground")}>
        <svg viewBox="0 0 12 12" fill="none" className="size-2.5 text-background">
          <path d="M2.5 6.2l2.3 2.3 4.7-5" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      </span>
    )
  }
  if (state === "current") return <span className={cn("size-2 rounded-full ring-[3px]", toneClasses[t].solid, toneClasses[t].ring)} />
  if (state === "todo") return <span className="size-2 rounded-full border border-border-strong" />
  return (
    <span className={cn("flex size-4 items-center justify-center rounded-full border", toneClasses[t].badge)}>
      <svg viewBox="0 0 12 12" fill="none" className="size-2.5">
        {state === "stopped" ? (
          <path d="M3.5 3.5l5 5m0-5l-5 5" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
        ) : (
          <path d="M6 2.8v3.9M6 8.9v.2" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" />
        )}
      </svg>
    </span>
  )
}

/**
 * Vertical event log: marker rail on the left, title + one quiet detail line, time on the right.
 * Same marker language as the checkout's progress rail. Markers never animate.
 */
export function Timeline({ items, className }: { items: TimelineItem[]; className?: string }) {
  return (
    <ol className={cn("flex flex-col", className)}>
      {items.map((item, i) => {
        const last = i === items.length - 1
        const next = items[i + 1]
        return (
          <li key={item.id} className="relative flex gap-3 pb-5 last:pb-0" aria-current={item.state === "current" ? "step" : undefined}>
            {!last && (
              <span
                aria-hidden="true"
                className={cn(
                  "absolute top-[22px] bottom-0.5 left-[7.5px] w-px",
                  next?.state === "todo" ? "bg-border" : "bg-border-strong",
                )}
              />
            )}
            <span aria-hidden="true" className="relative mt-0.5 flex size-4 shrink-0 items-center justify-center">
              <Marker state={item.state} tone={item.tone} />
            </span>
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-baseline justify-between gap-x-4 text-sm leading-5">
                <span className={cn(item.state === "todo" ? "text-faint" : "font-medium text-foreground")}>{item.title}</span>
                {item.at ? (
                  <time dateTime={item.at} className="shrink-0 text-xs whitespace-nowrap text-muted-foreground tabular-nums">
                    {formatStamp(item.at)}
                  </time>
                ) : (
                  item.meta && <span className="shrink-0 text-xs whitespace-nowrap text-muted-foreground">{item.meta}</span>
                )}
              </div>
              {item.detail && <div className="mt-0.5 text-[13px] leading-5 text-pretty text-muted-foreground">{item.detail}</div>}
            </div>
          </li>
        )
      })}
    </ol>
  )
}

/* -------------------------------------------------------------------------- */
/* A payment's history                                                        */
/* -------------------------------------------------------------------------- */

const Mono = ({ children }: { children: React.ReactNode }) => <span className="num text-foreground">{children}</span>
const Usdt = ({ value, asset }: { value: string; asset: string }) => <Amount value={value} asset={asset} className="text-foreground" />

function webhookItem(d: Delivery, now: number): TimelineItem | null {
  const target = (
    <>
      <Mono>{d.event_type}</Mono> to {hostOf(d.url)}
    </>
  )
  if (d.status === "succeeded") {
    return {
      id: d.id,
      state: "done",
      title: "Webhook delivered",
      at: d.completed_at ?? d.updated_at,
      detail: (
        <>
          {target}. {d.response_code !== null && <Mono>{d.response_code}</Mono>} in <Mono>{d.duration_ms} ms</Mono>
          {d.attempt > 1 && <>, on attempt {d.attempt}</>}.
        </>
      ),
    }
  }
  if (d.status === "retrying") {
    return {
      id: d.id,
      state: "alert",
      tone: "warning",
      title: "Webhook not delivered yet",
      at: d.updated_at,
      detail: (
        <>
          {target}. {d.response_code !== null ? <>Got <Mono>{d.response_code}</Mono></> : "No response"}, attempt {d.attempt} of {d.max_attempts}
          {d.next_attempt_at && <>. Next try {relativeTime(d.next_attempt_at, now)}</>}.
        </>
      ),
    }
  }
  if (d.status === "failed") {
    return {
      id: d.id,
      state: "stopped",
      tone: "danger",
      title: "Webhook failed",
      at: d.completed_at ?? d.updated_at,
      detail: (
        <>
          {target}. Gave up after {d.attempt} {d.attempt === 1 ? "attempt" : "attempts"}.
        </>
      ),
    }
  }
  return null
}

/** The payment as it was when an event was logged (every payment event carries that snapshot). */
function snapshot(event: ApiEvent): Payment | null {
  const data = event.data as Partial<Payment> | null
  return data && typeof data === "object" && typeof data.amount_received === "string" && typeof data.amount === "string" ? (data as Payment) : null
}

/** One logged event of a payment, in words. */
function eventItem(event: ApiEvent, current: Payment): TimelineItem | null {
  const was = snapshot(event) ?? current
  const base = { id: event.id, at: event.created_at }
  const asset = current.asset
  switch (event.type) {
    case "payment.created":
      return {
        ...base,
        state: "done",
        title: "Payment created",
        detail: (
          <>
            Deposit address <Mono>{truncateMiddle(current.address)}</Mono> assigned.
          </>
        ),
      }
    case "payment.detected":
      return {
        ...base,
        state: "done",
        title: "Part of the amount arrived",
        detail: (
          <>
            <Usdt value={was.amount_received} asset={asset} /> of <Usdt value={was.amount} asset={asset} /> seen on the deposit address.
          </>
        ),
      }
    case "payment.confirming":
      return {
        ...base,
        state: "done",
        title: "Full amount seen",
        detail: (
          <>
            <Usdt value={was.amount_received} asset={asset} /> is on-chain. Waiting for {was.required_confirmations} confirmations.
          </>
        ),
      }
    case "payment.awaiting":
      return {
        ...base,
        state: "alert",
        tone: "warning",
        title: "Transfer dropped out of the chain",
        detail: "A chain reorganisation removed the transfer that had been seen. Waiting for funds again.",
      }
    case "payment.paid":
      return {
        ...base,
        state: "done",
        tone: "success",
        title: "Paid",
        detail: (
          <>
            Reached {was.required_confirmations} confirmations. <Usdt value={was.amount_confirmed} asset={asset} /> received.
          </>
        ),
      }
    case "payment.overpaid":
    case "payment.underpaid":
      return {
        ...base,
        state: "alert",
        tone: "warning",
        title: event.type === "payment.underpaid" ? "Closed short" : "Confirmed, with extra",
        detail: (
          <>
            <Usdt value={was.amount_received} asset={asset} /> received for a <Usdt value={was.amount} asset={asset} /> payment (
            <Amount value={amountDelta(was)} sign="always" className="text-foreground" />
            ).
          </>
        ),
      }
    case "payment.expired":
      return was.cancelled_at
        ? {
            ...base,
            state: "stopped",
            tone: "neutral",
            title: "Cancelled",
            detail: was.cancelled_by ? <>Closed early by {was.cancelled_by}.</> : "Closed early.",
          }
        : { ...base, state: "stopped", tone: "neutral", title: "Expired", detail: "Nothing that counts arrived before the time ran out." }
    case "payment.failed":
      return {
        ...base,
        state: "stopped",
        tone: "danger",
        title: "Failed",
        detail: "The transfer that had been seen dropped out of the chain, and the time ran out.",
      }
    case "payment.late_funds":
      return {
        ...base,
        state: "alert",
        tone: "warning",
        title: "Funds arrived after it closed",
        detail: (
          <>
            <Usdt value={was.amount_confirmed} asset={asset} /> is now confirmed on the deposit address. The status does not change.
          </>
        ),
      }
    case "payment.resolved":
      return {
        ...base,
        state: "done",
        title: was.resolution?.by ? `Resolved by ${was.resolution.by}` : "Resolved",
        detail: was.resolution?.note || undefined,
      }
    case "payment.reopened":
      return { ...base, state: "alert", tone: "warning", title: "Reopened", detail: "Back under Needs attention." }
    default:
      return null
  }
}

/**
 * Everything that happened to a payment, oldest first — the instance's own event log and its
 * webhook deliveries — plus what is still expected while it is open.
 */
export function paymentTimeline({ payment: p, timeline, deliveries }: PaymentDetail, now: number): TimelineItem[] {
  const dated: { at: number; item: TimelineItem }[] = []
  for (const event of timeline) {
    const item = eventItem(event, p)
    if (item) dated.push({ at: Date.parse(event.created_at), item })
  }
  for (const delivery of deliveries) {
    const item = webhookItem(delivery, now)
    if (item?.at) dated.push({ at: Date.parse(item.at), item })
  }
  // Stable: events that share a timestamp keep the log's order.
  const items = dated.sort((a, b) => a.at - b.at).map((entry) => entry.item)

  if (!isOpen(p)) return items
  switch (p.status) {
    case "awaiting":
      items.push(
        {
          id: "waiting",
          state: "current",
          tone: "brand",
          title: "Waiting for the customer",
          meta: `Expires ${relativeTime(p.expires_at, now)}`,
          detail: "Nothing that counts has arrived on the deposit address yet.",
        },
        { id: "todo-paid", state: "todo", title: `Paid after ${p.required_confirmations} confirmations` },
      )
      break
    case "detected":
      items.push(
        {
          id: "topup",
          state: "current",
          tone: "info",
          title: "Waiting for the rest",
          meta: `Expires ${relativeTime(p.expires_at, now)}`,
          detail: (
            <>
              <Usdt value={p.amount_due} asset={p.asset} /> still to come.
            </>
          ),
        },
        { id: "todo-paid", state: "todo", title: `Paid after ${p.required_confirmations} confirmations` },
      )
      break
    case "confirming":
      items.push(
        p.awaiting_finality
          ? {
              id: "confirming",
              state: "current",
              tone: "info",
              title: "Finalising",
              meta: (
                <span className="num">
                  {p.confirmations}/{p.required_confirmations}
                </span>
              ),
              detail: operatorFinalityLine(p.awaiting_finality),
            }
          : {
              id: "confirming",
              state: "current",
              tone: "info",
              title: "Confirming",
              meta: (
                <span className="num">
                  {p.confirmations}/{p.required_confirmations}
                </span>
              ),
              detail: "One confirmation per block, then the chain finalises the block.",
            },
        { id: "todo-paid", state: "todo", title: "Paid" },
      )
      break
  }
  return items
}
