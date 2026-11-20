"use client"

import Link from "next/link"
import { ArrowRightIcon } from "lucide-react"

import { Amount, CopyButton, DetailList, DetailRow, NetworkBadge, PaymentStatusBadge, StatusBadge } from "@/components/hmn"
import { useDashboard } from "@/components/shell/session"
import { Button } from "@/components/ui/button"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { formatDate, relativeTime } from "@/lib/format"
import type { Payment, PaymentLink } from "@/lib/api/types"

import { Skeleton } from "@/components/ui/skeleton"

import { LINK_STATUS } from "./data"
import { LinkShare } from "./link-share"

export function LinkSheet({
  link,
  fallback,
  payments,
  now,
  onOpenChange,
  onToggle,
  onEdit,
  onDelete,
}: {
  /** The link being inspected, or null when the sheet is closed. */
  link: PaymentLink | null
  /** Last inspected link, kept on screen while the sheet slides out. */
  fallback: PaymentLink | null
  /** The latest payments made through the link; undefined while they load. */
  payments: Payment[] | undefined
  now: number
  onOpenChange: (open: boolean) => void
  onToggle: (link: PaymentLink) => void
  onEdit: (link: PaymentLink) => void
  onDelete: (link: PaymentLink) => void
}) {
  return (
    <Sheet open={link !== null} onOpenChange={onOpenChange}>
      <SheetContent
        className="gap-0 shadow-overlay outline-none data-[side=right]:w-full data-[side=right]:sm:max-w-[440px]"
        // Land on the sheet itself, not on its first button: opening a link is reading, not acting.
        onOpenAutoFocus={(event) => {
          event.preventDefault()
          ;(event.currentTarget as HTMLElement).focus()
        }}
      >
        {(link ?? fallback) && (
          <Body link={(link ?? fallback)!} payments={payments} now={now} onToggle={onToggle} onEdit={onEdit} onDelete={onDelete} />
        )}
      </SheetContent>
    </Sheet>
  )
}

function Body({
  link,
  payments,
  now,
  onToggle,
  onEdit,
  onDelete,
}: {
  link: PaymentLink
  /** The latest payments made through the link; undefined while they load. */
  payments: Payment[] | undefined
  now: number
  onToggle: (link: PaymentLink) => void
  onEdit: (link: PaymentLink) => void
  onDelete: (link: PaymentLink) => void
}) {
  const { settings } = useDashboard()
  const status = link.status
  const meta = LINK_STATUS[status]
  const left = link.max_uses !== null ? Math.max(0, link.max_uses - link.uses) : null

  return (
    <>
      <SheetHeader className="gap-1.5 border-b border-border p-5 pr-12">
        <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1.5">
          <SheetTitle className="text-base leading-6 font-semibold tracking-[-0.015em]">{link.name}</SheetTitle>
          <StatusBadge tone={meta.tone} hollow={meta.hollow}>
            {meta.label}
          </StatusBadge>
        </div>
        <SheetDescription className="text-[13px] leading-5 text-pretty">
          {link.description || meta.reason}
        </SheetDescription>
      </SheetHeader>

      <div className="min-h-0 flex-1 space-y-6 overflow-y-auto p-5">
        <LinkShare url={link.url} slug={link.slug} qrSize={116} />

        <dl className="grid grid-cols-3 divide-x divide-border overflow-hidden rounded-xl border border-border">
          <Figure label="Payments">
            <span className="num">{link.uses.toLocaleString("en-US")}</span>
            {link.max_uses !== null && (
              <span className="num font-normal text-muted-foreground">
                <span className="mx-[0.2em]">/</span>
                {link.max_uses}
              </span>
            )}
          </Figure>
          <Figure label="Collected">
            <Amount value={link.total_collected} dimFraction />
          </Figure>
          <Figure label="Last paid">
            <span className="text-[13px]">{link.last_paid_at ? relativeTime(link.last_paid_at, now).replace(" ago", "") : "Never"}</span>
            {link.last_paid_at && <span className="text-[13px] text-muted-foreground"> ago</span>}
          </Figure>
        </dl>

        <DetailList className="border-y border-border text-[13px]">
          <DetailRow label="Amount" className="min-h-10">
            {link.amount ? (
              <Amount value={link.amount} asset={link.asset} />
            ) : (
              <>
                Customer chooses{link.min_amount && <span className="text-muted-foreground">, from <Amount value={link.min_amount} asset={link.asset} /></span>}
              </>
            )}
          </DetailRow>
          <DetailRow label="Paid in" className="min-h-10">
            <NetworkBadge variant="inline" size="sm" asset chainId={settings.network.chain_id} />
          </DetailRow>
          <DetailRow label="After payment" className="min-h-10">
            {link.redirect_url ? (
              <span className="num truncate" title={link.redirect_url}>
                {link.redirect_url.replace(/^https?:\/\//, "")}
              </span>
            ) : (
              <span className="text-muted-foreground">Stays on the receipt</span>
            )}
          </DetailRow>
          {left !== null && (
            <DetailRow label="Limit" className="min-h-10">
              <span>
                <span className="num">{link.max_uses}</span> payments
                <span className="text-muted-foreground">
                  , <span className="num">{left}</span> left
                </span>
              </span>
            </DetailRow>
          )}
          {link.expires_at && (
            <DetailRow label={status === "expired" ? "Expired" : "Expires"} className="min-h-10">
              {formatDate(link.expires_at)}
            </DetailRow>
          )}
          <DetailRow label="Created" className="min-h-10">
            {formatDate(link.created_at)}
          </DetailRow>
          <DetailRow label="ID" className="min-h-10">
            <span className="num">{link.id}</span>
            <CopyButton value={link.id} label="Copy ID" size="icon-xs" className="-mr-1.5 ml-1 pointer-coarse:size-10" />
          </DetailRow>
        </DetailList>

        <section aria-labelledby="link-payments-title">
          <div className="flex items-center justify-between gap-3">
            <h3 id="link-payments-title" className="text-sm leading-6 font-semibold">
              Recent payments
            </h3>
            {payments && payments.length > 0 && (
              <Link
                href={`/payments?link=${link.id}`}
                className="focus-ring inline-flex items-center gap-1 rounded-sm text-[13px] leading-5 text-muted-foreground transition-colors hover:text-foreground"
              >
                View all
                <ArrowRightIcon aria-hidden="true" className="size-3.5" />
              </Link>
            )}
          </div>
          {payments === undefined ? (
            <div className="mt-3 space-y-3" role="status" aria-label="Loading payments">
              <Skeleton className="h-4 w-full" />
              <Skeleton className="h-4 w-full" />
            </div>
          ) : payments.length === 0 ? (
            <p className="mt-2 rounded-lg border border-dashed border-border-strong px-4 py-5 text-center text-[13px] leading-5 text-muted-foreground">
              Nothing yet. Payments made through this link show up here.
            </p>
          ) : (
            <ul className="mt-1 divide-y divide-border">
              {payments.map((payment) => (
                <li key={payment.id}>
                  <Link
                    href={`/payments/${payment.id}`}
                    className="-mx-2 flex min-h-10 items-center gap-3 rounded-md px-2 text-[13px] leading-5 transition-colors outline-none hover:bg-muted/50 focus-visible:bg-muted/50 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring"
                  >
                    <span className="w-24 shrink-0">
                      <PaymentStatusBadge status={payment.status} finalising={payment.awaiting_finality !== null} size="sm" live={false} />
                    </span>
                    <span className="min-w-0 flex-1 truncate text-muted-foreground">{relativeTime(payment.created_at, now)}</span>
                    <Amount value={payment.amount} dimFraction />
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>

      <div className="flex items-center justify-between gap-2 border-t border-border bg-muted/40 px-5 py-3.5">
        <Button variant="ghost" className="-ml-2 text-danger-fg hover:bg-danger-subtle hover:text-danger-fg pointer-coarse:h-10" onClick={() => onDelete(link)}>
          Delete
        </Button>
        <div className="flex items-center gap-2">
          {(status === "active" || status === "disabled") && (
            <Button variant="ghost" className="pointer-coarse:h-10 pointer-coarse:px-3.5" onClick={() => onToggle(link)}>
              {status === "active" ? "Disable" : "Enable"}
            </Button>
          )}
          <Button variant="outline" className="pointer-coarse:h-10 pointer-coarse:px-3.5" onClick={() => onEdit(link)}>
            Edit link
          </Button>
        </div>
      </div>
    </>
  )
}

function Figure({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="min-w-0 px-3.5 py-3">
      <dt className="text-xs leading-4 text-muted-foreground">{label}</dt>
      <dd className="mt-1 truncate text-[15px] leading-6 font-medium">{children}</dd>
    </div>
  )
}
