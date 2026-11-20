"use client"

import * as React from "react"
import Link from "next/link"
import { usePathname } from "next/navigation"
import { BanIcon, CheckIcon, CircleAlertIcon, CircleSlashIcon, RotateCwIcon, TriangleAlertIcon } from "lucide-react"
import { toast } from "sonner"

import { formatClock, useCountdown } from "@/components/checkout/parts"
import { Amount, Callout, CopyButton, StatusBadge, TxHash, type Tone } from "@/components/hmn"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { FINALISING } from "@/lib/finality"
import { api, isApiError } from "@/lib/api/client"
import type { Delivery, Payment, Transfer } from "@/lib/api/types"
import { relativeTime, truncateMiddle } from "@/lib/format"
import { describeError } from "@/lib/errors"
import { hostOf } from "@/lib/url"
import { useNow } from "@/lib/use-now"
import { cn } from "@/lib/utils"

import { createPaymentHref } from "./create-payment"
import { amountGap, formatShort, hasReceived, isCancelled, paymentLabel } from "./lib"

/* -------------------------------------------------------------------------- */
/* Expiry clock                                                               */
/* -------------------------------------------------------------------------- */

/** Ticking "24:59" for an open payment. Turns warning-toned in the last five minutes. */
export function ExpiryClock({ expiresAt, className }: { expiresAt: string; className?: string }) {
  const remaining = useCountdown(expiresAt)
  return (
    <span role="timer" className={cn("num", remaining <= 5 * 60_000 && "text-warning-fg", className)}>
      {formatClock(remaining)}
    </span>
  )
}

/* -------------------------------------------------------------------------- */
/* Cancel                                                                     */
/* -------------------------------------------------------------------------- */

/**
 * Cancel an open payment. The server allows it only while nothing but dust has arrived; once
 * funds were seen it refuses (409 payment_has_funds), and that refusal is shown as what it is:
 * money is on its way, the payment stays.
 */
export function CancelPaymentButton({ payment, onChanged }: { payment: Payment; onChanged: (next?: Payment) => void }) {
  const [open, setOpen] = React.useState(false)
  const [busy, setBusy] = React.useState(false)
  const [refusal, setRefusal] = React.useState<string | null>(null)

  const cancel = async () => {
    setBusy(true)
    try {
      const next = await api.cancelPayment(payment.id)
      setOpen(false)
      onChanged(next)
      toast.success("Payment cancelled", { description: `${paymentLabel(payment)} no longer accepts funds. Its address is still watched for at least an hour.` })
    } catch (error) {
      if (isApiError(error) && (error.code === "payment_has_funds" || error.code === "payment_settled")) {
        // Not a failure of the request: the payment moved on. Say so and show where it stands.
        setRefusal(describeError(error))
        onChanged()
      } else {
        toast.error("Couldn’t cancel", { description: describeError(error) })
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <AlertDialog
      open={open}
      onOpenChange={(value) => {
        setOpen(value)
        if (!value) setRefusal(null)
      }}
    >
      <AlertDialogTrigger asChild>
        <Button variant="outline">
          <BanIcon data-icon="inline-start" />
          Cancel payment
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent>
        {refusal ? (
          <>
            <AlertDialogHeader>
              <AlertDialogTitle>This payment can’t be cancelled</AlertDialogTitle>
              <AlertDialogDescription role="alert">{refusal}</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>Close</AlertDialogCancel>
            </AlertDialogFooter>
          </>
        ) : (
          <>
            <AlertDialogHeader>
              <AlertDialogTitle>Cancel this payment?</AlertDialogTitle>
              <AlertDialogDescription>
                The checkout closes at once and shows the customer that the payment was cancelled. If they send money anyway within the
                next hour it is flagged as late funds for you to refund or honour; after that the address is no longer watched.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel disabled={busy}>Keep it</AlertDialogCancel>
              <AlertDialogAction
                variant="destructive"
                disabled={busy}
                onClick={(event) => {
                  event.preventDefault()
                  void cancel()
                }}
              >
                {busy ? "Cancelling…" : "Cancel payment"}
              </AlertDialogAction>
            </AlertDialogFooter>
          </>
        )}
      </AlertDialogContent>
    </AlertDialog>
  )
}

/* -------------------------------------------------------------------------- */
/* Resolution                                                                 */
/* -------------------------------------------------------------------------- */

/**
 * What to do about a payment that did not settle cleanly. Explains the situation in one or two
 * sentences, offers the next step, and lets the operator close it (with a note that is kept on
 * the payment). hmnpay holds no keys, so a refund is always something the merchant sends from
 * their own wallet — the copy says so.
 */
export function ResolutionBlock({ payment, onChanged }: { payment: Payment; onChanged: (next?: Payment) => void }) {
  const pathname = usePathname()
  const [note, setNote] = React.useState("")
  const [busy, setBusy] = React.useState(false)
  const asset = payment.asset
  const usdt = (value: string) => <Amount value={value} asset={asset} className="text-foreground" />
  const gap = amountGap(payment)

  const act = async (run: () => Promise<Payment>, done: () => void) => {
    setBusy(true)
    try {
      const next = await run()
      onChanged(next)
      done()
    } catch (error) {
      toast.error("That didn’t go through", { description: describeError(error) })
      onChanged()
    } finally {
      setBusy(false)
    }
  }
  const reopen = () => act(() => api.reopenPayment(payment.id), () => toast("Reopened. It is back under Needs attention."))
  const resolve = () =>
    act(
      () => api.resolvePayment(payment.id, note.trim()),
      () => {
        setNote("")
        toast.success("Marked as resolved", { description: `${paymentLabel(payment)} no longer needs attention.` })
      },
    )

  if (payment.resolution && !payment.needs_attention) {
    const { resolution } = payment
    return (
      <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
        <div className="flex min-w-0 gap-3">
          <span className="mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-full bg-foreground text-background">
            <CheckIcon className="size-2.5" strokeWidth={3} />
          </span>
          <div className="min-w-0 text-[13px] leading-5 text-muted-foreground">
            <p className="font-medium text-foreground">Resolved</p>
            {resolution.note && <p className="break-words whitespace-pre-line">{resolution.note}</p>}
            <p>
              {resolution.by}, <span className="tabular-nums">{formatShort(resolution.resolved_at)}</span> UTC
            </p>
          </div>
        </div>
        <Button variant="outline" size="sm" disabled={busy} onClick={() => void reopen()}>
          Reopen
        </Button>
      </div>
    )
  }

  const again = (amount: string, description: string) =>
    createPaymentHref(pathname, { amount, order: payment.order_id, description, email: payment.customer_email })
  const requestRest = (
    <Button variant="outline" asChild className="text-foreground">
      <Link href={again(payment.amount_due, `Balance for ${paymentLabel(payment)}`)} scroll={false}>
        Request <Amount value={payment.amount_due} asset={asset} />
      </Link>
    </Button>
  )
  const copySender = payment.from_address ? (
    <CopyButton value={payment.from_address} variant="outline" size="default" className="text-foreground">
      Copy sender address
    </CopyButton>
  ) : null
  const createAgain = (
    <Button variant="outline" asChild className="text-foreground">
      <Link href={again(payment.amount, payment.description)} scroll={false}>
        Create again
      </Link>
    </Button>
  )

  let tone: Tone = "warning"
  let icon = <TriangleAlertIcon />
  let title: React.ReactNode
  let body: React.ReactNode
  let secondary: React.ReactNode = null

  switch (payment.attention) {
    case "underpaid":
      title = (
        <>
          Short by <Amount value={gap} asset={asset} />
        </>
      )
      body = (
        <>
          The time ran out with {usdt(payment.amount_received)} of {usdt(payment.amount)} received. The funds are on
          the deposit address. Ask for the rest with a new payment, or accept it as it is. To refund, send from your own wallet:
          hmnpay never holds your keys.
        </>
      )
      secondary = (
        <>
          {requestRest}
          {copySender}
        </>
      )
      break
    case "overpaid":
      title = (
        <>
          Over by <Amount value={gap} asset={asset} />
        </>
      )
      body = (
        <>
          The customer sent {usdt(payment.amount_received)} instead of {usdt(payment.amount)}. The order is covered.
          If you want to return the extra, send it from your own wallet to the sender.
        </>
      )
      secondary = copySender
      break
    case "late_funds":
      title = (
        <>
          <Amount value={payment.amount_received} asset={asset} /> on a payment that had already closed
        </>
      )
      body = (
        <>
          Funds were confirmed on the deposit address after this payment {isCancelled(payment) ? "was cancelled" : `ended as ${payment.status}`}.
          They are yours to honour or to refund from your own wallet; the status of the payment does not change.
        </>
      )
      secondary = copySender
      break
    case "failed":
      tone = "danger"
      icon = <CircleAlertIcon />
      title = "The transfer dropped out of the chain"
      body =
        "A transfer was seen, then a chain reorganisation removed it and it did not come back before the time ran out. Nothing is confirmed on the address. If the customer still wants to pay, create the payment again."
      secondary = createAgain
      break
    default:
      // Closed with nothing that counts: not a problem, just an ending.
      return (
        <Callout
          tone="neutral"
          icon={<CircleSlashIcon />}
          title={isCancelled(payment) ? "Cancelled" : "Expired without a payment"}
          className="px-4 py-3.5"
        >
          <p className="mt-0.5 max-w-[60ch] text-pretty">
            {isCancelled(payment)
              ? "This payment was closed before anything arrived."
              : hasReceived(payment)
                ? "Only an amount too small to count arrived in time."
                : "No transfer arrived in time."}{" "}
            The address is still watched for at least an hour after closing. Create it again if the customer still wants to pay.
          </p>
          <div className="mt-3 flex flex-wrap items-center gap-2">{createAgain}</div>
        </Callout>
      )
  }

  return (
    <Callout tone={tone} icon={icon} title={title} className="px-4 py-3.5">
      <p className="mt-0.5 max-w-[60ch] text-pretty">{body}</p>
      <form
        className="mt-3 flex flex-wrap items-center gap-2"
        onSubmit={(event) => {
          event.preventDefault()
          void resolve()
        }}
      >
        <Input
          value={note}
          onChange={(event) => setNote(event.target.value)}
          placeholder="Note (optional): what you did about it"
          aria-label="Resolution note"
          maxLength={500}
          className="h-8 w-full bg-background text-foreground sm:w-72 pointer-coarse:h-10"
        />
        <Button type="submit" disabled={busy}>
          {busy ? "Saving…" : "Mark as resolved"}
        </Button>
        {secondary}
      </form>
    </Callout>
  )
}

/* -------------------------------------------------------------------------- */
/* Transfers                                                                  */
/* -------------------------------------------------------------------------- */

const TRANSFER_STATUS: Record<Transfer["status"], { label: string; tone: Tone }> = {
  confirmed: { label: "Confirmed", tone: "success" },
  pending: { label: "Confirming", tone: "info" },
  orphaned: { label: "Dropped", tone: "danger" },
}

/** Every transfer seen on the payment's deposit address, as the chain reported it. */
export function TransfersList({ transfers, payment }: { transfers: Transfer[]; payment: Payment }) {
  return (
    <ul className="divide-y divide-border">
      {transfers.map((t) => {
        // All its confirmations, its block not final yet: "Finalising", not a counter at 15/15.
        const finalising = t.status === "pending" && t.awaiting_finality
        const status = finalising ? { label: FINALISING.label, tone: "info" as Tone } : TRANSFER_STATUS[t.status]
        return (
          <li key={`${t.tx_hash}:${t.log_index}:${t.block_hash}`} className="flex items-center gap-4 px-5 py-3">
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
                <Amount value={t.amount} asset={payment.asset} className="text-[13px] leading-5 font-medium" />
                <StatusBadge tone={status.tone} size="sm" data-finalising={finalising ? "" : undefined} title={finalising ? FINALISING.description : undefined}>
                  {status.label}
                  {t.status === "pending" && !finalising && (
                    <span className="num opacity-80">
                      {t.confirmations}/{payment.required_confirmations}
                    </span>
                  )}
                </StatusBadge>
              </div>
              <p className="mt-1 text-[13px] leading-5 text-muted-foreground">
                from <span className="num" title={t.from}>{truncateMiddle(t.from)}</span>
                <span className="mx-1.5 text-faint">·</span>
                block <span className="num">#{t.block_number}</span>
                <span className="mx-1.5 text-faint">·</span>
                <span className="tabular-nums">{formatShort(t.block_time ?? t.seen_at)}</span>
              </p>
              {t.status === "orphaned" && (
                <p className="text-[13px] leading-5 text-muted-foreground">Removed by a chain reorganisation; it no longer counts.</p>
              )}
            </div>
            <TxHash value={t.tx_hash} truncate={[8, 6]} chainId={payment.chain_id} className="shrink-0 text-[13px]" />
          </li>
        )
      })}
    </ul>
  )
}

/* -------------------------------------------------------------------------- */
/* Webhook deliveries                                                         */
/* -------------------------------------------------------------------------- */

export const DELIVERY_STATUS: Record<Delivery["status"], { label: string; tone: Tone }> = {
  succeeded: { label: "Delivered", tone: "success" },
  retrying: { label: "Retrying", tone: "warning" },
  failed: { label: "Failed", tone: "danger" },
  pending: { label: "Queued", tone: "neutral" },
}

/** Webhook deliveries of one payment: event, endpoint, HTTP result, attempts, and a resend. */
export function WebhookDeliveries({ deliveries, onChanged }: { deliveries: Delivery[]; onChanged: () => void }) {
  const [resending, setResending] = React.useState<Record<string, boolean>>({})
  const now = useNow()

  const resend = async (d: Delivery) => {
    setResending((r) => ({ ...r, [d.id]: true }))
    try {
      await api.resendDelivery(d.id)
      toast.success("Webhook queued", { description: `${d.event_type} will be sent again in a moment.` })
      onChanged()
    } catch (error) {
      toast.error("Couldn’t resend", { description: describeError(error) })
    } finally {
      setResending((r) => ({ ...r, [d.id]: false }))
    }
  }

  return (
    <ul className="divide-y divide-border">
      {deliveries.map((d) => {
        const status = DELIVERY_STATUS[d.status]
        return (
          <li key={d.id} className="flex items-center gap-4 px-5 py-3">
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
                <span className="num text-[13px] leading-5 font-medium">{d.event_type}</span>
                <StatusBadge tone={status.tone} size="sm">
                  {status.label}
                  {d.response_code !== null && <span className="num opacity-80">{d.response_code}</span>}
                </StatusBadge>
              </div>
              <p className="mt-1 truncate text-[13px] leading-5 text-muted-foreground">
                <span className="num">{hostOf(d.url)}</span>
              </p>
              <p className="text-[13px] leading-5 text-muted-foreground">
                {d.status === "pending" && d.attempt === 0 ? (
                  "Queued. It goes out within a few seconds."
                ) : (
                  <>
                    Attempt <span className="num">{d.attempt}</span> of <span className="num">{d.max_attempts}</span>
                    {d.attempt > 0 && (
                      <>
                        <span className="mx-1.5 text-faint">·</span>
                        <span className="num">{d.duration_ms} ms</span>
                      </>
                    )}
                    <span className="mx-1.5 text-faint">·</span>
                    {d.status === "retrying" && d.next_attempt_at ? (
                      <>next try {relativeTime(d.next_attempt_at, now)}</>
                    ) : (
                      <span className="tabular-nums">{formatShort(d.updated_at)}</span>
                    )}
                  </>
                )}
              </p>
              {d.error && d.status !== "succeeded" && <p className="truncate text-[13px] leading-5 text-danger-fg">{d.error}</p>}
            </div>
            {d.status !== "pending" && (
              <Button variant="outline" size="sm" disabled={resending[d.id]} onClick={() => void resend(d)} className="shrink-0 pointer-coarse:h-9">
                <RotateCwIcon data-icon="inline-start" />
                {resending[d.id] ? "Queued" : "Resend"}
              </Button>
            )}
          </li>
        )
      })}
    </ul>
  )
}
