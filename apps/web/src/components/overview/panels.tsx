"use client"

import Link from "next/link"
import { ArrowRightIcon, CheckIcon, KeyRoundIcon, WebhookIcon } from "lucide-react"

import {
  Amount,
  EmptyState,
  ErrorState,
  Panel,
  PaymentStatusBadge,
  StatusBadge,
  StatusDot,
  toneClasses,
  type Tone,
} from "@/components/hmn"
import { CodeSnippet } from "@/components/payments/code-snippet"
import { CreatePaymentButton } from "@/components/payments/create-payment"
import { issueSummary, paymentLabel, paymentPath, paymentsHref } from "@/components/payments/lib"
import { chainName, nodeHealth } from "@/components/shell/node-status"
import { useDashboard } from "@/components/shell/session"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import type { Delivery, Payment, PaymentCounts } from "@/lib/api/types"
import { relativeTime, truncateMiddle } from "@/lib/format"
import { useNow } from "@/lib/use-now"
import { cn } from "@/lib/utils"

export const PERIODS = [7, 30, 90] as const
export type Period = (typeof PERIODS)[number]

/* -------------------------------------------------------------------------- */
/* Shared bits                                                                */
/* -------------------------------------------------------------------------- */

export function PanelHeader({
  title,
  count,
  aside,
  className,
}: {
  title: string
  count?: number
  aside?: React.ReactNode
  className?: string
}) {
  return (
    <div className={cn("flex h-12 shrink-0 items-center justify-between gap-4 px-5", className)}>
      <h2 className="flex items-center gap-2 text-sm leading-6 font-semibold tracking-[-0.01em]">
        {title}
        {count !== undefined && count > 0 && (
          <span className="num rounded-full border border-border px-1.5 text-[11px] leading-[18px] font-medium text-muted-foreground">
            {count}
          </span>
        )}
      </h2>
      {aside && <div className="text-[13px] leading-5 text-muted-foreground">{aside}</div>}
    </div>
  )
}

export function TextLink({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <Link
      href={href}
      className="focus-ring group/link inline-flex items-center gap-1 rounded-sm text-[13px] leading-5 text-muted-foreground transition-colors hover:text-foreground"
    >
      {children}
      <ArrowRightIcon className="size-3.5 transition-transform group-hover/link:translate-x-0.5" aria-hidden="true" />
    </Link>
  )
}

/* -------------------------------------------------------------------------- */
/* Period switch                                                              */
/* -------------------------------------------------------------------------- */

/** 7 / 30 / 90 days as links (`?period=`): the choice is shareable and needs no client state. */
export function PeriodSwitch({ value, periods = PERIODS }: { value: Period; periods?: readonly Period[] }) {
  return (
    <nav aria-label="Period" className="flex h-8 items-center rounded-lg border border-border bg-card p-0.5 pointer-coarse:h-10">
      {periods.map((period) => (
        <Link
          key={period}
          href={period === 30 ? "/" : `/?period=${period}`}
          scroll={false}
          aria-current={period === value ? "true" : undefined}
          aria-label={`Last ${period} days`}
          className={cn(
            "focus-ring num flex h-full items-center rounded-md px-2.5 text-[13px] transition-colors pointer-coarse:px-3.5",
            period === value ? "bg-muted font-medium text-foreground" : "text-muted-foreground hover:text-foreground",
          )}
        >
          {period}d
        </Link>
      ))}
    </nav>
  )
}

/* -------------------------------------------------------------------------- */
/* Needs attention                                                            */
/* -------------------------------------------------------------------------- */

interface AttentionItem {
  key: string
  href: string
  label: string
  badge: React.ReactNode
  text: React.ReactNode
  at: string
}

function paymentItem(p: Payment): AttentionItem {
  const summary = issueSummary(p)
  return {
    key: p.id,
    href: paymentPath(p.id),
    label: paymentLabel(p),
    badge:
      p.attention === "late_funds" ? (
        <StatusBadge tone="warning" size="sm">
          Late funds
        </StatusBadge>
      ) : (
        <PaymentStatusBadge status={p.status} finalising={p.awaiting_finality !== null} size="sm" />
      ),
    text: (
      <>
        {summary.lead}
        {summary.amount && (
          <>
            {" "}
            <Amount value={summary.amount} asset={p.asset} className="text-foreground" />
          </>
        )}
      </>
    ),
    at: p.updated_at,
  }
}

function webhookItem(d: Delivery): AttentionItem {
  return {
    key: d.id,
    href: d.payment_id ? paymentPath(d.payment_id) : "/settings/webhooks",
    label: d.payment_id ? truncateMiddle(d.payment_id, 8, 4) : "Webhook",
    badge: (
      <StatusBadge tone={d.status === "failed" ? "danger" : "warning"} size="sm">
        {d.status === "failed" ? "Webhook failed" : "Webhook retrying"}
      </StatusBadge>
    ),
    text: (
      <>
        <span className="num text-foreground">{d.event_type}</span> got{" "}
        <span className="num text-foreground">{d.response_code ?? "no response"}</span>, attempt {d.attempt} of {d.max_attempts}
      </>
    ),
    at: d.updated_at,
  }
}

/**
 * Everything that needs a human: payments with an unresolved issue (the API's "attention"
 * view), plus webhook deliveries that are not getting through. `total` is the number of
 * payments needing attention, which can be more than the few shown.
 */
export function AttentionPanel({
  payments,
  deliveries,
  total,
  loading,
  error,
  onRetry,
  limit = 4,
  className,
}: {
  payments: Payment[] | undefined
  deliveries: Delivery[] | undefined
  total: number
  loading: boolean
  error?: unknown
  onRetry?: () => void
  limit?: number
  className?: string
}) {
  const now = useNow()
  const items = [...(payments ?? []).map(paymentItem), ...(deliveries ?? []).map(webhookItem)].sort((a, b) => Date.parse(b.at) - Date.parse(a.at))
  const visible = items.slice(0, limit)
  const count = total + (deliveries?.length ?? 0)

  return (
    <Panel className={cn("flex flex-col", className)}>
      <PanelHeader title="Needs attention" count={count} className="border-b border-border" />
      {loading ? (
        <div className="flex-1 space-y-4 px-5 py-4" role="status" aria-label="Loading">
          {[0, 1, 2].map((i) => (
            <div key={i} className="space-y-2">
              <Skeleton className="h-3.5 w-28" />
              <Skeleton className="h-3.5 w-48" />
            </div>
          ))}
        </div>
      ) : payments === undefined ? (
        <ErrorState error={error} onRetry={onRetry} bordered={false} className="flex-1 py-10" />
      ) : items.length === 0 ? (
        <div className="flex flex-1 flex-col items-center justify-center gap-2 px-6 py-10 text-center">
          <span className="flex size-8 items-center justify-center rounded-full border border-success-border bg-success-subtle text-success-fg">
            <CheckIcon className="size-4" />
          </span>
          <p className="text-sm font-medium">Nothing needs you</p>
          <p className="max-w-56 text-[13px] leading-5 text-muted-foreground">
            Underpaid, overpaid and failed payments and late funds show up here until you resolve them.
          </p>
        </div>
      ) : (
        <>
          <ul className="flex-1 divide-y divide-border">
            {visible.map((item) => (
              <li key={item.key}>
                <Link
                  href={item.href}
                  className="flex flex-col gap-1 px-5 py-3 outline-none transition-colors hover:bg-muted/50 focus-visible:bg-muted/50"
                >
                  <span className="flex items-center justify-between gap-3">
                    <span className="num min-w-0 truncate text-[13px] leading-5 font-medium">{item.label}</span>
                    {item.badge}
                  </span>
                  <span className="flex items-baseline justify-between gap-3 text-[13px] leading-5 text-muted-foreground">
                    <span className="min-w-0 truncate">{item.text}</span>
                    <time dateTime={item.at} className="shrink-0 text-xs text-faint">
                      {relativeTime(item.at, now).replace(" ago", "")}
                    </time>
                  </span>
                </Link>
              </li>
            ))}
          </ul>
          <div className="flex h-11 shrink-0 items-center border-t border-border px-5">
            <TextLink href={paymentsHref({ view: "attention" })}>
              {count > visible.length ? `View all ${total} in Payments` : "Review in Payments"}
            </TextLink>
          </div>
        </>
      )}
    </Panel>
  )
}

/* -------------------------------------------------------------------------- */
/* Outcomes                                                                   */
/* -------------------------------------------------------------------------- */

const pct = (part: number, whole: number) => {
  if (!whole) return "0%"
  const value = (part / whole) * 100
  return `${value >= 10 || value === 0 ? value.toFixed(0) : value.toFixed(1)}%`
}

/**
 * Where the payments created in the period stand: one thin segmented bar and a legend. The
 * figures are the API's own counts for payments created since the period began (its list views),
 * so they always add up to what the Payments page shows for the same dates. "Other" is what the
 * four views leave over: settled payments whose issue was resolved (an overpayment you closed).
 */
export function OutcomesPanel({
  counts,
  days,
  footer,
  className,
}: {
  counts: PaymentCounts | undefined
  days: number
  /** Pinned to the bottom edge (e.g. what is in flight right now). */
  footer?: React.ReactNode
  className?: string
}) {
  const total = counts?.all ?? 0
  const other = counts ? Math.max(0, counts.all - counts.paid - counts.open - counts.expired - counts.attention) : 0
  const rows: { label: string; value: number; tone: Tone }[] = [
    { label: "Paid", value: counts?.paid ?? 0, tone: "success" },
    { label: "In progress", value: counts?.open ?? 0, tone: "info" },
    { label: "Expired", value: counts?.expired ?? 0, tone: "neutral" },
    { label: "Needs attention", value: counts?.attention ?? 0, tone: "warning" },
    ...(other > 0 ? [{ label: "Other", value: other, tone: "brand" as Tone }] : []),
  ]

  return (
    <Panel className={cn("flex flex-col", className)}>
      <PanelHeader title="Outcomes" aside={`Last ${days} days`} />
      <div className="px-5 pb-4">
        <p className="text-[13px] leading-5 text-muted-foreground">
          {counts ? (
            <>
              <span className="num text-foreground">{total.toLocaleString("en-US")}</span> {total === 1 ? "payment" : "payments"} created
            </>
          ) : (
            <Skeleton className="inline-block h-3.5 w-36 align-middle" />
          )}
        </p>
        <div
          role="img"
          aria-label={rows.map((r) => `${r.label} ${pct(r.value, total)}`).join(", ")}
          className="mt-3 flex h-2 gap-0.5 overflow-hidden rounded-full"
        >
          {rows
            .filter((r) => r.value > 0)
            .map((r) => (
              <span
                key={r.label}
                className={cn("h-full min-w-1", toneClasses[r.tone].solid, r.tone === "neutral" && "opacity-60")}
                style={{ flexGrow: r.value, flexBasis: 0 }}
              />
            ))}
        </div>
        <dl className="mt-3 divide-y divide-border">
          {rows.map((r) => (
            <div key={r.label} className="flex h-9 items-center gap-2.5 text-[13px] leading-5">
              <StatusDot tone={r.tone} className={cn("size-2", r.tone === "neutral" && "opacity-60")} />
              <dt className="flex-1 text-muted-foreground">{r.label}</dt>
              <dd className="num font-medium">{r.value.toLocaleString("en-US")}</dd>
              <dd className="num w-12 text-right text-muted-foreground">{pct(r.value, total)}</dd>
            </div>
          ))}
        </dl>
      </div>
      {footer && (
        <div className="mt-auto flex min-h-11 items-center border-t border-border px-5 py-2 text-[13px] leading-5 text-muted-foreground">
          {footer}
        </div>
      )}
    </Panel>
  )
}

/* -------------------------------------------------------------------------- */
/* First run                                                                  */
/* -------------------------------------------------------------------------- */

const firstRequest = (baseUrl: string) => `curl ${baseUrl}/api/v1/payments \\
  -H "Authorization: Bearer $HMNPAY_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "amount": "49.00",
    "order_id": "ORD-1001"
  }'`

function Step({
  n,
  title,
  children,
  action,
}: {
  n: number
  title: string
  children: React.ReactNode
  action: React.ReactNode
}) {
  return (
    <li className="flex gap-4 px-5 py-4">
      <span className="num mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full border border-border-strong text-xs font-medium text-muted-foreground">
        {n}
      </span>
      <div className="flex min-w-0 flex-1 flex-wrap items-center justify-between gap-x-6 gap-y-3">
        <div className="min-w-0 basis-64 flex-1">
          <h3 className="text-sm leading-6 font-medium">{title}</h3>
          <p className="text-[13px] leading-5 text-pretty text-muted-foreground">{children}</p>
        </div>
        <div className="shrink-0">{action}</div>
      </div>
    </li>
  )
}

function Ready({ title, ok = true, children }: { title: string; ok?: boolean; children: React.ReactNode }) {
  return (
    <div className="flex gap-3 bg-card px-5 py-4">
      {ok ? (
        <span className="mt-1 flex size-4 shrink-0 items-center justify-center rounded-full bg-success text-background">
          <CheckIcon className="size-2.5" strokeWidth={3} />
        </span>
      ) : (
        <span className="mt-1 flex size-4 shrink-0 items-center justify-center">
          <StatusDot tone="warning" className="size-2" />
        </span>
      )}
      <div className="min-w-0">
        <p className="text-[13px] leading-5 font-medium">{title}</p>
        <p className="truncate text-[13px] leading-5 text-muted-foreground">{children}</p>
      </div>
    </div>
  )
}

/** A brand-new instance: what works already, and the three things to do next. */
export function FirstRun() {
  const { settings, node, nodeError } = useDashboard()
  const health = nodeHealth(node, Boolean(nodeError))
  const network = chainName(settings.network.chain_id)
  const baseUrl = settings.public_url || (typeof window === "undefined" ? "" : window.location.origin)
  const height = node?.head?.number ?? node?.latest_block ?? 0
  return (
    <div className="space-y-6">
      <Panel className="grid lg:grid-cols-[minmax(0,1fr)_minmax(0,440px)]">
        <div className="min-w-0">
          <div className="px-5 pt-5 pb-1">
            <h2 className="text-base leading-6 font-semibold tracking-[-0.015em]">Take your first payment</h2>
            <p className="mt-1 text-sm leading-6 text-muted-foreground">
              Your gateway is set up. Three steps and your product is taking {settings.asset.symbol}.
            </p>
          </div>
          <ol className="divide-y divide-border">
            <Step n={1} title="Create a test payment" action={<CreatePaymentButton />}>
              See the checkout your customers get. Pay it from any wallet and watch it confirm here.
            </Step>
            <Step
              n={2}
              title="Get an API key"
              action={
                <Button variant="outline" asChild>
                  <Link href="/settings/api-keys">
                    <KeyRoundIcon data-icon="inline-start" />
                    API keys
                  </Link>
                </Button>
              }
            >
              Your backend uses it to create payments and read their status.
            </Step>
            <Step
              n={3}
              title="Add a webhook endpoint"
              action={
                <Button variant="outline" asChild>
                  <Link href="/settings/webhooks">
                    <WebhookIcon data-icon="inline-start" />
                    Webhooks
                  </Link>
                </Button>
              }
            >
              hmnpay calls your server the moment a payment is confirmed, so you can ship the order.
            </Step>
          </ol>
        </div>
        <div className="flex min-w-0 flex-col gap-3 border-t border-border bg-muted/30 p-5 lg:border-t-0 lg:border-l">
          <div>
            <h3 className="text-sm leading-6 font-medium">Or start from your backend</h3>
            <p className="text-[13px] leading-5 text-muted-foreground">
              One request creates a payment and returns the checkout link.
            </p>
          </div>
          <CodeSnippet code={firstRequest(baseUrl)} label="cURL" copyLabel="Copy request" />
          <p className="text-[13px] leading-5 text-muted-foreground">
            The response has a <span className="num text-foreground">checkout_url</span> to send your customer to.
          </p>
        </div>
      </Panel>

      <div className="grid gap-px overflow-hidden rounded-xl border border-border bg-border shadow-card md:grid-cols-3">
        <Ready ok={health.tone === "success"} title={`${network} node: ${health.label === "Idle" ? "ready" : health.label.toLowerCase()}`}>
          {height > 0 ? (
            <>
              Block <span className="num">#{height}</span>, {health.detail}
            </>
          ) : (
            health.detail
          )}
        </Ready>
        <Ready title="Wallet is connected">
          Deposit addresses derive from <span className="num">{truncateMiddle(settings.wallet.xpub, 8, 4)}</span>
        </Ready>
        <Ready title="Checkout is live">
          <span className="num">{baseUrl.replace(/^https?:\/\//, "")}/pay</span>
        </Ready>
      </div>

      <EmptyState
        title="No payments yet"
        description="Once payments come in, this page shows your volume, conversion, and anything that needs a decision from you."
        className="py-12"
      />
    </div>
  )
}
