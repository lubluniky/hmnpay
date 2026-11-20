"use client"

import * as React from "react"
import Link from "next/link"
import { ArrowUpRightIcon, SearchXIcon } from "lucide-react"

import { ConfirmationBlocks, LiveLine } from "@/components/checkout/parts"
import {
  Address,
  Amount,
  CopyButton,
  DetailList,
  DetailRow,
  EmptyState,
  ErrorState,
  networkName,
  NetworkBadge,
  PageContainer,
  PageHeader,
  Panel,
  PaymentStatusBadge,
  StaleNote,
  TxHash,
} from "@/components/hmn"
import { useLiveEvents } from "@/components/shell/session"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { finalityShort, operatorFinalityLine } from "@/lib/finality"
import { api } from "@/lib/api/client"
import type { Payment, PaymentDetail } from "@/lib/api/types"
import { useQuery } from "@/lib/api/use-query"
import { isNotFound } from "@/lib/errors"
import { formatDateTime, truncateMiddle } from "@/lib/format"
import { ratioPercent } from "@/lib/money"
import { safeHttpUrl } from "@/lib/url"
import { useNow } from "@/lib/use-now"
import { isPaymentId, usePathSegment } from "@/lib/use-route"
import { cn } from "@/lib/utils"

import { JsonSnippet } from "./code-snippet"
import { CreatePaymentDialog } from "./create-payment"
import { amountDelta, checkoutPath, checkoutUrl, formatDuration, formatShort, hasLateFunds, hasReceived, isCancelled, isIssue, isOpen, paymentLabel } from "./lib"
import { CancelPaymentButton, ExpiryClock, ResolutionBlock, TransfersList, WebhookDeliveries } from "./payment-actions"
import { PaymentDetailSkeleton } from "./skeletons"
import { paymentTimeline, Timeline } from "./timeline"

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <PageContainer>
      {children}
      <CreatePaymentDialog />
    </PageContainer>
  )
}

/** One payment: what was asked, what arrived, what happened, and what (if anything) you need to do. */
export function PaymentDetailView() {
  const id = usePathSegment(1)
  const valid = id !== null && isPaymentId(id)
  const detail = useQuery(valid ? `payments/detail/${id}` : null, (signal) => api.paymentDetail(id!, signal), { refreshMs: 4000 })
  useLiveEvents((event) => {
    if (event.payment_id === id) detail.reload()
  })
  const reload = detail.reload
  const mutate = detail.mutate
  /** After an action: show the payment the server returned at once, then fetch the rest (timeline, deliveries). */
  const changed = React.useCallback(
    (next?: Payment) => {
      if (next) mutate((current) => (current ? { ...current, payment: next } : current))
      reload()
    },
    [mutate, reload],
  )

  const label = detail.data ? paymentLabel(detail.data.payment) : null
  React.useEffect(() => {
    if (label) document.title = `${label} · hmnpay`
  }, [label])

  if (id !== null && (!valid || (isNotFound(detail.error) && !detail.data))) {
    return (
      <Frame>
        <EmptyState
          icon={<SearchXIcon />}
          title="No such payment"
          description="There is no payment with this ID on this instance. It may belong to another instance, or the link was cut short."
          action={
            <Button variant="outline" asChild>
              <Link href="/payments">All payments</Link>
            </Button>
          }
          className="py-24"
        />
      </Frame>
    )
  }
  if (!detail.data) {
    return (
      <Frame>
        {detail.error ? (
          <ErrorState error={detail.error} title="Couldn’t load this payment" onRetry={detail.reload} className="py-24" />
        ) : (
          <>
            <div className="mb-6 space-y-3 lg:mb-8">
              <Skeleton className="h-7 w-44" />
              <Skeleton className="h-4 w-64" />
            </div>
            <PaymentDetailSkeleton />
          </>
        )}
      </Frame>
    )
  }
  return (
    <Frame>
      {/* Keyed by the payment: this page is reused when the operator goes from one payment to
          another, and nothing typed or opened for one (a resolution note, a dialog) may stay
          for the next. */}
      <Loaded key={detail.data.payment.id} detail={detail.data} stale={detail.error} onChanged={changed} />
    </Frame>
  )
}

function Loaded({ detail, stale, onChanged }: { detail: PaymentDetail; stale: unknown; onChanged: (next?: Payment) => void }) {
  const { payment, transfers, deliveries } = detail
  const now = useNow(10_000)
  const hasMetadata = Object.keys(payment.metadata).length > 0
  const first = transfers.find((t) => t.status !== "orphaned") ?? transfers[0]
  const redirect = safeHttpUrl(payment.redirect_url)

  return (
    <>
      <PageHeader
        title={<span className="num tracking-[-0.03em]">{paymentLabel(payment)}</span>}
        description={payment.description || undefined}
        meta={
          <PaymentStatusBadge
            status={payment.status}
            confirmations={payment.confirmations}
            required={payment.required_confirmations}
            finalising={payment.awaiting_finality !== null}
            // The summary panel carries the heartbeat for open payments.
            live={false}
          />
        }
        actions={
          <>
            {payment.status === "awaiting" && <CancelPaymentButton payment={payment} onChanged={onChanged} />}
            <CopyButton value={checkoutUrl(payment)} variant="outline" size="default" copiedLabel="Link copied">
              Copy checkout link
            </CopyButton>
            <Button variant="outline" asChild>
              <a href={checkoutPath(payment.id)} target="_blank" rel="noopener noreferrer">
                Open checkout
                <ArrowUpRightIcon data-icon="inline-end" />
              </a>
            </Button>
          </>
        }
      />

      {Boolean(stale) && <StaleNote error={stale} className="mb-4" />}

      {/* Phones read top to bottom: state, history, chain facts, order, then the developer panels. */}
      <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,1fr)_340px]">
        <div className="min-w-0 max-lg:contents lg:space-y-6">
          <Summary payment={payment} onChanged={onChanged} />

          {transfers.length > 0 && (
            <Panel>
              <PanelHeader
                title="Transfers"
                aside={`${transfers.length} on the deposit address`}
                className="border-b border-border"
              />
              <TransfersList transfers={transfers} payment={payment} />
            </Panel>
          )}

          <Panel>
            <PanelHeader title="Timeline" aside="Times in UTC" />
            <Timeline items={paymentTimeline(detail, now)} className="px-5 pt-1 pb-5" />
          </Panel>

          <Panel className="max-lg:order-3">
            <PanelHeader
              title="Webhooks"
              aside={
                <Link href="/settings/webhooks" className="focus-ring rounded-sm transition-colors hover:text-foreground">
                  Manage endpoints
                </Link>
              }
              className="border-b border-border"
            />
            {deliveries.length > 0 ? (
              <WebhookDeliveries deliveries={deliveries} onChanged={() => onChanged()} />
            ) : (
              <p className="px-5 py-6 text-[13px] leading-5 text-muted-foreground">
                No webhook has been sent for this payment. Endpoints are notified of the events they subscribe to.
              </p>
            )}
          </Panel>

          <Panel className="max-lg:order-4">
            <PanelHeader title="Metadata" />
            <div className="px-5 pb-5">
              {hasMetadata ? (
                <JsonSnippet value={payment.metadata} />
              ) : (
                <p className="text-[13px] leading-5 text-muted-foreground">
                  None. Attach your own keys when you create a payment and they come back in every webhook.
                </p>
              )}
            </div>
          </Panel>
        </div>

        <div className="min-w-0 max-lg:contents lg:space-y-6">
          <Panel className="max-lg:order-1">
            <PanelHeader title="On-chain" />
            <DetailList className="px-5 pb-1.5 text-[13px]">
              <DetailRow label="Network">
                <NetworkBadge variant="inline" size="sm" chainId={payment.chain_id} />
              </DetailRow>
              <DetailRow label="Deposit address">
                <Address value={payment.address} chainId={payment.chain_id} />
              </DetailRow>
              {payment.tx_hash ? (
                <>
                  <DetailRow label="Transaction">
                    <TxHash value={payment.tx_hash} truncate={[8, 6]} chainId={payment.chain_id} />
                  </DetailRow>
                  {payment.from_address && (
                    <DetailRow label="Sender">
                      <Address value={payment.from_address} chainId={payment.chain_id} />
                    </DetailRow>
                  )}
                  {first && (
                    <DetailRow label="Block">
                      <span className="num">#{first.block_number}</span>
                    </DetailRow>
                  )}
                  <DetailRow label="Confirmations">
                    <span className="num">
                      {payment.confirmations}
                      <span className="text-faint"> / </span>
                      <span className="text-muted-foreground">{payment.required_confirmations}</span>
                    </span>
                  </DetailRow>
                </>
              ) : (
                <DetailRow label="Transaction">
                  <span className="text-muted-foreground">{isOpen(payment) ? "Not seen yet" : "None"}</span>
                </DetailRow>
              )}
            </DetailList>
          </Panel>

          <Panel className="max-lg:order-2">
            <PanelHeader title="Order" />
            <DetailList className="px-5 pb-1.5 text-[13px]">
              <DetailRow label="Order ID">
                {payment.order_id ? (
                  <>
                    <span className="num truncate">{payment.order_id}</span>
                    <CopyButton value={payment.order_id} label="Copy order ID" size="icon-xs" className="ml-1 pointer-coarse:size-10" />
                  </>
                ) : (
                  <span className="text-muted-foreground">Not provided</span>
                )}
              </DetailRow>
              <DetailRow label="Payment ID">
                <span className="num" title={payment.id}>
                  {truncateMiddle(payment.id, 8, 4)}
                </span>
                <CopyButton value={payment.id} label="Copy payment ID" size="icon-xs" className="ml-1 pointer-coarse:size-10" />
              </DetailRow>
              <DetailRow label="Customer">
                {payment.customer_email ? (
                  <span className="truncate">{payment.customer_email}</span>
                ) : (
                  <span className="text-muted-foreground">Not provided</span>
                )}
              </DetailRow>
              {payment.link_id && (
                <DetailRow label="Created from">
                  <Link href="/payment-links" className="focus-ring rounded-sm underline underline-offset-4">
                    a payment link
                  </Link>
                </DetailRow>
              )}
              <DetailRow label="Created">
                <span className="tabular-nums">{formatDateTime(payment.created_at)}</span>
              </DetailRow>
              {payment.paid_at ? (
                <DetailRow label="Confirmed">
                  <span className="tabular-nums">{formatDateTime(payment.paid_at)}</span>
                </DetailRow>
              ) : payment.cancelled_at ? (
                <DetailRow label="Cancelled">
                  <span className="tabular-nums">{formatDateTime(payment.cancelled_at)}</span>
                </DetailRow>
              ) : (
                <DetailRow label={isOpen(payment) ? "Expires" : "Window closed"}>
                  <span className="tabular-nums">{formatDateTime(payment.expires_at)}</span>
                </DetailRow>
              )}
              {payment.redirect_url && (
                <DetailRow label="Return URL">
                  <span className="truncate" title={payment.redirect_url}>
                    {redirect ? payment.redirect_url.replace(/^https?:\/\//, "") : "Not a web address"}
                  </span>
                </DetailRow>
              )}
            </DetailList>
          </Panel>
        </div>
      </div>
    </>
  )
}

function PanelHeader({ title, aside, className }: { title: string; aside?: React.ReactNode; className?: string }) {
  return (
    <div className={cn("flex h-12 items-center justify-between gap-4 px-5", className)}>
      <h2 className="text-sm leading-6 font-semibold tracking-[-0.01em]">{title}</h2>
      {aside && <div className="text-[13px] leading-5 text-muted-foreground">{aside}</div>}
    </div>
  )
}

function Figure({ label, children, className }: { label: string; children: React.ReactNode; className?: string }) {
  return (
    <div className={cn("flex items-baseline justify-between gap-4 px-5 py-3 sm:flex-col sm:items-start sm:justify-start sm:gap-1.5 sm:py-4", className)}>
      <dt className="text-[13px] leading-5 text-muted-foreground">{label}</dt>
      <dd className="num min-w-0 text-base leading-7 font-medium tracking-[-0.03em] sm:text-xl">{children}</dd>
    </div>
  )
}

/** Requested / received / the one figure that matters for this state, then the state itself. */
function Summary({ payment, onChanged }: { payment: Payment; onChanged: (next?: Payment) => void }) {
  const { status, asset } = payment
  const mismatch = status === "underpaid" || status === "overpaid"
  const received = hasReceived(payment)

  let third: React.ReactNode
  if (status === "awaiting") {
    third = (
      <Figure label="Expires in">
        <ExpiryClock expiresAt={payment.expires_at} />
      </Figure>
    )
  } else if (status === "detected") {
    third = (
      <Figure label="Still due">
        <Amount value={payment.amount_due} asset={asset} className="text-info-fg" />
      </Figure>
    )
  } else if (status === "confirming" && payment.awaiting_finality && finalityShort(payment.awaiting_finality)) {
    third = (
      <Figure label="Finalised so far">
        <span className="num">{payment.awaiting_finality.final.toLocaleString("en-US")}</span>
        <span className="text-faint"> / </span>
        <span className="num text-muted-foreground">{payment.awaiting_finality.block.toLocaleString("en-US")}</span>
      </Figure>
    )
  } else if (status === "confirming") {
    third = (
      <Figure label="Confirmations">
        {payment.confirmations}
        <span className="text-faint"> / </span>
        <span className="text-muted-foreground">{payment.required_confirmations}</span>
      </Figure>
    )
  } else if (mismatch) {
    third = (
      <Figure label="Difference">
        <Amount value={amountDelta(payment)} sign="always" asset={asset} className="text-warning-fg" />
      </Figure>
    )
  } else if (status === "paid" && payment.paid_at) {
    third = (
      <Figure label="Paid after">
        <span className="font-sans tracking-normal tabular-nums">
          {formatDuration(Date.parse(payment.paid_at) - Date.parse(payment.created_at))}
        </span>
      </Figure>
    )
  } else {
    third = (
      <Figure label={isCancelled(payment) ? "Cancelled" : status === "failed" ? "Closed" : "Expired"}>
        <span className="font-sans tracking-normal tabular-nums">
          {formatShort(payment.cancelled_at ?? payment.settled_at ?? payment.expires_at)}
        </span>
      </Figure>
    )
  }

  return (
    <Panel>
      <dl className="grid divide-y divide-border sm:grid-cols-3 sm:divide-x sm:divide-y-0">
        <Figure label="Requested">
          <Amount value={payment.amount} asset={asset} />
        </Figure>
        <Figure label="Received">
          <Amount
            value={payment.amount_received}
            asset={asset}
            className={cn(!received && "font-normal text-muted-foreground", (mismatch || hasLateFunds(payment)) && "text-warning-fg")}
          />
        </Figure>
        {third}
      </dl>

      {status === "awaiting" && (
        <div className="border-t border-border px-5 py-4">
          <LiveLine>
            {received
              ? "An amount too small to count has arrived. Still watching the deposit address."
              : "Watching the deposit address. This page updates on its own."}
          </LiveLine>
        </div>
      )}
      {status === "detected" && (
        <div className="space-y-3 border-t border-border px-5 py-4">
          <div
            className="h-1.5 overflow-hidden rounded-full bg-muted"
            role="progressbar"
            aria-label="Share of the amount received"
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={Math.round(ratioPercent(payment.amount_received, payment.amount, payment.decimals))}
          >
            <div className="h-full rounded-full bg-info" style={{ width: `${ratioPercent(payment.amount_received, payment.amount, payment.decimals)}%` }} />
          </div>
          <LiveLine tone="info">Part of the amount arrived. The customer can send the rest until the payment expires.</LiveLine>
        </div>
      )}
      {status === "confirming" && (
        <div className="space-y-3 border-t border-border px-5 py-4" data-slot={payment.awaiting_finality ? "finalising" : "confirming"}>
          <ConfirmationBlocks confirmations={payment.confirmations} required={payment.required_confirmations} />
          {payment.awaiting_finality ? (
            <LiveLine tone="info">
              All {payment.required_confirmations} confirmations are in. {operatorFinalityLine(payment.awaiting_finality)} It is paid when the
              chain has finalised that block.
            </LiveLine>
          ) : (
            <LiveLine tone="info">Confirming on {networkName(payment.chain_id)}. One confirmation per block, then the chain finalises the block.</LiveLine>
          )}
        </div>
      )}
      {(isIssue(payment) || status === "expired") && (
        <div className="border-t border-border p-4 sm:p-5">
          <ResolutionBlock payment={payment} onChanged={onChanged} />
        </div>
      )}
    </Panel>
  )
}
