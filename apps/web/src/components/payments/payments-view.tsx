"use client"

import * as React from "react"
import Link from "next/link"
import { useSearchParams } from "next/navigation"
import { ArrowLeftRightIcon, ChevronLeftIcon, ChevronRightIcon, SearchXIcon, XIcon } from "lucide-react"

import { EmptyState, ErrorState, PageContainer, PageHeader, Panel, StaleNote, StatusDot } from "@/components/hmn"
import { useDashboard, useLiveEvents } from "@/components/shell/session"
import { Button } from "@/components/ui/button"
import { api } from "@/lib/api/client"
import { useQuery } from "@/lib/api/use-query"
import { useNow } from "@/lib/use-now"
import { cn } from "@/lib/utils"

import { CreatePaymentButton, CreatePaymentDialog } from "./create-payment"
import { PAYMENT_VIEWS, parsePaymentQuery, paymentsHref, toApiQuery, type PaymentQuery } from "./lib"
import { PaymentsTable } from "./payments-table"
import { ExportMenu, PaymentsToolbar } from "./payments-toolbar"
import { PaymentsTableSkeleton } from "./skeletons"

const PAGE_SIZE = 15

/**
 * Payments list. All state lives in the URL — `status` (view), `q`, `range` or `from`+`to`,
 * `sort`+`dir`, `page` — and the API does the filtering, searching, sorting and paging.
 * The list refreshes itself every few seconds and at once when the instance logs an event.
 */
export function PaymentsView() {
  const params = useSearchParams()
  const { page, ...query } = React.useMemo(() => parsePaymentQuery(new URLSearchParams(params)), [params])
  const { counts: instanceCounts } = useDashboard()
  // The day the date presets count from.
  const today = useNow(60_000)
  const apiQuery = React.useMemo(() => toApiQuery(query as PaymentQuery, today), [query, today])
  const key = JSON.stringify([apiQuery, page])

  const list = useQuery(
    `payments/list/${key}`,
    (signal) => api.payments({ ...apiQuery, limit: PAGE_SIZE, offset: (page - 1) * PAGE_SIZE, counts: true }, signal),
    { refreshMs: 5000, keepPrevious: true },
  )
  useLiveEvents((event) => {
    if (event.type.startsWith("payment.")) list.reload()
  })

  const filtered = query.view !== "all" || query.q !== "" || query.range !== "all" || Boolean(query.from) || Boolean(query.link)
  const rows = list.data?.data ?? []
  const total = list.data?.page.total ?? 0
  const counts = list.data?.counts
  const pageCount = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const openCount = instanceCounts?.open ?? 0
  // A brand-new instance: nothing exists at all (not "nothing matches").
  const empty = !filtered && list.data !== undefined && total === 0 && (counts?.all ?? 0) === 0
  const sortHref = (key: "created" | "amount") =>
    paymentsHref({ ...query, sort: key, dir: query.sort === key && query.dir === "desc" ? "asc" : "desc" })

  return (
    <PageContainer>
      <PageHeader
        title="Payments"
        description="Every payment created on this instance, through the API, payment links or this dashboard."
        actions={
          <>
            <ExportMenu query={apiQuery} total={total} filtered={filtered} />
            <CreatePaymentButton />
          </>
        }
      />

      {empty ? (
        <EmptyState
          icon={<ArrowLeftRightIcon />}
          title="No payments yet"
          description="Create one here to try the checkout, or create them from your backend with an API key. They show up the moment they exist."
          secondaryAction={
            <Button variant="outline" asChild>
              <Link href="/settings/api-keys">Get an API key</Link>
            </Button>
          }
          action={<CreatePaymentButton />}
          className="py-24"
        />
      ) : (
        <div className="space-y-4">
          {/* Views. Counts follow the search and date range, so a tab never promises rows it can't show. */}
          <nav
            aria-label="Payment status"
            className="-mx-4 flex gap-5 overflow-x-auto border-b border-border px-4 [scrollbar-width:none] sm:mx-0 sm:px-0 [&::-webkit-scrollbar]:hidden"
          >
            {PAYMENT_VIEWS.map((view) => {
              const active = view.id === query.view
              return (
                <Link
                  key={view.id}
                  href={paymentsHref({ ...query, view: view.id })}
                  scroll={false}
                  aria-current={active ? "page" : undefined}
                  className={cn(
                    "focus-ring relative flex h-10 shrink-0 items-center gap-1.5 rounded-sm text-sm whitespace-nowrap transition-colors",
                    active
                      ? "font-medium text-foreground after:absolute after:inset-x-0 after:-bottom-px after:h-0.5 after:bg-foreground"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {view.label}
                  {counts && <span className={cn("num text-xs", active ? "text-muted-foreground" : "text-faint")}>{counts[view.id]}</span>}
                </Link>
              )
            })}
          </nav>

          <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-3">
            <PaymentsToolbar query={query} className="max-sm:contents sm:flex-1" />
            {query.link && (
              <Button variant="outline" asChild className="gap-2 font-normal">
                <Link href={paymentsHref({ ...query, link: undefined })} scroll={false}>
                  From one payment link
                  <XIcon className="size-3.5 text-muted-foreground" aria-label="Show all payments" />
                </Link>
              </Button>
            )}
            {openCount > 0 && (
              <p className="flex items-center gap-2 text-[13px] leading-5 text-muted-foreground">
                <span className="flex size-4 items-center justify-center">
                  <StatusDot tone="brand" live className="size-2" />
                </span>
                <span>
                  Live. Watching <span className="num text-foreground">{openCount}</span> open {openCount === 1 ? "payment" : "payments"}
                </span>
              </p>
            )}
          </div>

          {list.error && list.data && <StaleNote error={list.error} />}

          <Panel>
            {list.loading ? (
              <PaymentsTableSkeleton rows={PAGE_SIZE} />
            ) : !list.data ? (
              <ErrorState error={list.error} title="Couldn’t load payments" onRetry={list.reload} bordered={false} className="py-20" />
            ) : rows.length === 0 ? (
              <EmptyState
                bordered={false}
                icon={<SearchXIcon />}
                title="No payments match"
                description={
                  query.q ? (
                    <>
                      Nothing found for <span className="num text-foreground">{query.q}</span>
                      {query.view !== "all" || query.range !== "all" || query.from ? " with these filters" : ""}. Search looks
                      at order IDs, addresses, transaction hashes and emails.
                    </>
                  ) : (
                    "No payments in this view for the selected dates."
                  )
                }
                action={
                  <Button variant="outline" asChild>
                    <Link href="/payments" scroll={false}>
                      Clear filters
                    </Link>
                  </Button>
                }
                className="py-20"
              />
            ) : (
              <>
                <PaymentsTable
                  rows={rows}
                  sort={{ key: query.sort, dir: query.dir, hrefs: { created: sortHref("created"), amount: sortHref("amount") } }}
                />
                <footer className="flex items-center justify-between gap-4 border-t border-border py-2.5 pr-3 pl-4 text-[13px] leading-5 text-muted-foreground md:pl-5">
                  <p>
                    <span className="num text-foreground">
                      {(page - 1) * PAGE_SIZE + 1}–{(page - 1) * PAGE_SIZE + rows.length}
                    </span>{" "}
                    of <span className="num text-foreground">{total}</span>
                  </p>
                  <div className="flex items-center gap-2">
                    <span className="mr-1 max-sm:hidden">
                      Page <span className="num">{page}</span> of <span className="num">{pageCount}</span>
                    </span>
                    <PageLink href={paymentsHref({ ...query, page: page - 1 })} disabled={page <= 1} label="Previous page">
                      <ChevronLeftIcon />
                    </PageLink>
                    <PageLink href={paymentsHref({ ...query, page: page + 1 })} disabled={page >= pageCount} label="Next page">
                      <ChevronRightIcon />
                    </PageLink>
                  </div>
                </footer>
              </>
            )}
          </Panel>
        </div>
      )}

      <CreatePaymentDialog />
    </PageContainer>
  )
}

function PageLink({ href, disabled, label, children }: { href: string; disabled: boolean; label: string; children: React.ReactNode }) {
  if (disabled) {
    return (
      <Button variant="outline" size="icon" disabled aria-label={label} className="pointer-coarse:size-10">
        {children}
      </Button>
    )
  }
  return (
    <Button variant="outline" size="icon" asChild className="pointer-coarse:size-10">
      <Link href={href} aria-label={label} scroll={false}>
        {children}
      </Link>
    </Button>
  )
}
