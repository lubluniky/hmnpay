"use client"

import Link from "next/link"
import { useRouter } from "next/navigation"
import { ArrowDownIcon, ArrowUpIcon, ChevronsUpDownIcon } from "lucide-react"

import { Amount, PaymentStatusBadge, Swap, useArrivals } from "@/components/hmn"
import type { Payment } from "@/lib/api/types"
import { formatDateTime, relativeTime, truncateMiddle } from "@/lib/format"
import { displayAmount } from "@/lib/money"
import { useNow } from "@/lib/use-now"
import { cn } from "@/lib/utils"

import { amountDelta, amountGap, formatShort, paymentLabel, paymentPath } from "./lib"

type SortKey = "created" | "amount"

export interface PaymentsTableSort {
  key: SortKey
  dir: "asc" | "desc"
  /** Href that sorts by this column (and flips direction if it is already the active one). */
  hrefs: Record<SortKey, string>
}

/** Money that has actually arrived reads at full strength; requested-but-not-received is quieter. */
const settled = (p: Payment) => ["paid", "underpaid", "overpaid", "confirming", "detected"].includes(p.status)

function AmountCell({ payment, className }: { payment: Payment; className?: string }) {
  const mismatch = payment.status === "underpaid" || payment.status === "overpaid"
  return (
    <span className={cn("inline-flex items-baseline justify-end gap-2", className)}>
      {mismatch && (
        <Amount
          value={amountDelta(payment)}
          sign="always"
          className="text-xs text-warning-fg"
          aria-label={`${payment.status} by ${displayAmount(amountGap(payment))} ${payment.asset}`}
        />
      )}
      <Amount
        value={payment.amount}
        dimFraction
        className={cn("font-medium", !settled(payment) && "font-normal text-muted-foreground")}
      />
    </span>
  )
}

function StatusCell({ payment, size }: { payment: Payment; size?: "sm" | "md" }) {
  return (
    <Swap id={`${payment.status}${payment.awaiting_finality ? "-finalising" : ""}`}>
      <PaymentStatusBadge
        status={payment.status}
        confirmations={payment.confirmations}
        required={payment.required_confirmations}
        finalising={payment.awaiting_finality !== null}
        size={size}
        // Rows never pulse: a page gets one heartbeat, and it lives outside the table.
        live={false}
      />
    </Swap>
  )
}

function SortHeader({
  label,
  column,
  sort,
  align = "left",
  children,
}: {
  label: string
  column: SortKey
  sort?: PaymentsTableSort
  align?: "left" | "right"
  children?: React.ReactNode
}) {
  if (!sort) {
    return (
      <span className={cn("inline-flex items-center gap-1.5", align === "right" && "justify-end")}>
        {label}
        {children}
      </span>
    )
  }
  const active = sort.key === column
  const Icon = !active ? ChevronsUpDownIcon : sort.dir === "asc" ? ArrowUpIcon : ArrowDownIcon
  return (
    <Link
      href={sort.hrefs[column]}
      scroll={false}
      className={cn(
        "focus-ring group/sort -mx-1.5 inline-flex h-7 items-center gap-1 rounded-md px-1.5 transition-colors hover:text-foreground",
        active && "text-foreground",
        align === "right" && "flex-row-reverse",
      )}
    >
      <span className="inline-flex items-center gap-1.5">
        {label}
        {children}
      </span>
      <Icon className={cn("size-3.5", active ? "text-muted-foreground" : "text-faint/70 group-hover/sort:text-muted-foreground")} aria-hidden="true" />
    </Link>
  )
}

const TH = "h-10 px-3 text-left align-middle text-[13px] font-normal whitespace-nowrap text-muted-foreground first:pl-5 last:pr-5"
const TD = "h-11 px-3 align-middle first:pl-5 last:pr-5"

/**
 * The payments table. `variant="full"` is the workhorse on /payments (customer, transaction, sortable
 * headers); `variant="compact"` is the short version for overview-style panels.
 * Below `md` it becomes a two-line list — never a sideways-scrolling table.
 * Whole rows open the payment; the order ID is the real link for keyboard and screen-reader users.
 * Live: when `rows` gains payments at the top, they glide in and the rest make room; rows named
 * in `fresh` (arrived or changed while the page was open) get a wash of brand that drains away.
 */
export function PaymentsTable({
  rows,
  variant = "full",
  sort,
  fresh,
  className,
}: {
  rows: Payment[]
  variant?: "full" | "compact"
  sort?: PaymentsTableSort
  fresh?: readonly string[]
  className?: string
}) {
  const router = useRouter()
  const now = useNow()
  const ids = rows.map((payment) => payment.id)
  const body = useArrivals<HTMLTableSectionElement>(ids)
  const list = useArrivals<HTMLUListElement>(ids)
  const flash = (payment: Payment) => fresh?.includes(payment.id) && "motion-safe:animate-row-flash"
  const full = variant === "full"
  const ariaSort = (column: SortKey) =>
    sort?.key === column ? (sort.dir === "asc" ? "ascending" : "descending") : undefined

  return (
    <div className={className}>
      <table className="hidden w-full table-fixed border-collapse text-[13px] leading-5 md:table">
        <colgroup>
          <col />
          <col className={full ? "w-[148px]" : "w-[150px]"} />
          {full && <col className="hidden w-[212px] lg:table-column" />}
          {full && <col className="hidden w-[168px] xl:table-column" />}
          <col className={full ? "w-[132px]" : "w-[136px]"} />
          <col className="w-[148px]" />
        </colgroup>
        <thead>
          <tr className="border-b border-border">
            <th scope="col" className={TH}>
              Order
            </th>
            <th scope="col" className={TH}>
              Status
            </th>
            {full && (
              <th scope="col" className={cn(TH, "hidden lg:table-cell")}>
                Customer
              </th>
            )}
            {full && (
              <th scope="col" className={cn(TH, "hidden xl:table-cell")}>
                Transaction
              </th>
            )}
            <th scope="col" className={TH} aria-sort={ariaSort("created")}>
              <SortHeader label="Created" column="created" sort={sort} />
            </th>
            <th scope="col" className={cn(TH, "text-right")} aria-sort={ariaSort("amount")}>
              <SortHeader label="Amount" column="amount" sort={sort} align="right">
                <span className="num text-[11px] text-faint">USDT</span>
              </SortHeader>
            </th>
          </tr>
        </thead>
        <tbody ref={body} className="divide-y divide-border">
          {rows.map((payment) => (
            <tr
              key={payment.id}
              data-row=""
              onClick={(event) => {
                if ((event.target as HTMLElement).closest("a,button")) return
                if (event.metaKey || event.ctrlKey) window.open(paymentPath(payment.id), "_blank")
                else router.push(paymentPath(payment.id))
              }}
              className={cn("group cursor-pointer transition-colors hover:bg-muted/50", flash(payment))}
            >
              <td className={TD}>
                <div className="flex min-w-0 items-center gap-3">
                  <Link
                    href={paymentPath(payment.id)}
                    title={payment.order_id || payment.id}
                    className="focus-ring num w-[84px] shrink-0 truncate rounded-sm font-medium underline-offset-4 group-hover:underline"
                  >
                    {paymentLabel(payment)}
                  </Link>
                  <span className="truncate text-muted-foreground">{payment.description}</span>
                </div>
              </td>
              <td className={TD}>
                <StatusCell payment={payment} />
              </td>
              {full && (
                <td className={cn(TD, "hidden lg:table-cell")}>
                  {payment.customer_email ? (
                    <span className="block truncate text-muted-foreground">{payment.customer_email}</span>
                  ) : (
                    <span className="text-faint">—</span>
                  )}
                </td>
              )}
              {full && (
                <td className={cn(TD, "hidden xl:table-cell")}>
                  {payment.tx_hash ? (
                    <span className="num text-muted-foreground" title={payment.tx_hash}>
                      {truncateMiddle(payment.tx_hash, 8, 6)}
                    </span>
                  ) : (
                    <span className="text-faint">—</span>
                  )}
                </td>
              )}
              <td className={TD}>
                <time
                  dateTime={payment.created_at}
                  title={formatDateTime(payment.created_at)}
                  className="whitespace-nowrap text-muted-foreground tabular-nums"
                >
                  {full ? formatShort(payment.created_at) : relativeTime(payment.created_at, now)}
                </time>
              </td>
              <td className={cn(TD, "text-right")}>
                <AmountCell payment={payment} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {/* Phones: two lines per payment, the whole row is the link. */}
      <ul ref={list} className="divide-y divide-border md:hidden">
        {rows.map((payment) => (
          <li key={payment.id} data-row="" className={cn(flash(payment))}>
            <Link
              href={paymentPath(payment.id)}
              className="flex min-h-16 flex-col justify-center gap-1 px-4 py-3 outline-none transition-colors active:bg-muted/50 focus-visible:bg-muted/50"
            >
              <span className="flex items-baseline justify-between gap-3">
                <span className="num min-w-0 truncate text-sm font-medium">{paymentLabel(payment)}</span>
                <AmountCell payment={payment} className="text-sm" />
              </span>
              <span className="flex items-center justify-between gap-3">
                <span className="min-w-0 truncate text-[13px] leading-5 text-muted-foreground">
                  <time dateTime={payment.created_at} className="tabular-nums">
                    {full ? formatShort(payment.created_at) : relativeTime(payment.created_at, now)}
                  </time>
                  <span className="mx-1.5 text-faint">·</span>
                  {payment.description}
                </span>
                <StatusCell payment={payment} size="sm" />
              </span>
            </Link>
          </li>
        ))}
      </ul>
    </div>
  )
}
