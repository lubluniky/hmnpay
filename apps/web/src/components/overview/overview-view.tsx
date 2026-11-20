"use client"

import * as React from "react"
import { useSearchParams } from "next/navigation"

import { Amount, ErrorState, PageContainer, PageHeader, Panel, StaleNote, StatusDot } from "@/components/hmn"
import { CreatePaymentButton, CreatePaymentDialog } from "@/components/payments/create-payment"
import { PaymentsTable } from "@/components/payments/payments-table"
import { PaymentsTableSkeleton } from "@/components/payments/skeletons"
import { useDashboard, useLiveEvents } from "@/components/shell/session"
import { Skeleton } from "@/components/ui/skeleton"
import { api } from "@/lib/api/client"
import { useQuery } from "@/lib/api/use-query"
import { sumAmounts, toChartNumber } from "@/lib/money"
import { useNow } from "@/lib/use-now"

import { OverviewStats } from "./live"
import { AttentionPanel, FirstRun, OutcomesPanel, PanelHeader, PERIODS, PeriodSwitch, TextLink, type Period } from "./panels"
import { VolumeChart, type VolumePoint } from "./volume-chart"

const utcDay = (ms: number) => new Date(ms).toISOString().slice(0, 10)

/**
 * Overview: is money flowing, and is anything on fire. `?period=7|30|90` switches the period.
 * Every figure comes from the API (overview, payment counts, the newest payments, what needs
 * attention) and is fetched again whenever the instance logs a payment event, so the page
 * follows what happens while it is open: a new row glides in, a row that settles flashes, the
 * headline digits roll.
 */
export function OverviewView() {
  const params = useSearchParams()
  const period = (PERIODS.find((p) => String(p) === params.get("period")) ?? 30) as Period
  const { counts: instanceCounts, settings } = useDashboard()

  const overview = useQuery(`overview/${period}`, (signal) => api.overview(period, signal), { refreshMs: 30_000, keepPrevious: true })
  const recent = useQuery("overview/recent", (signal) => api.payments({ limit: 6 }, signal), { refreshMs: 10_000 })
  const attention = useQuery("overview/attention", (signal) => api.payments({ view: "attention", limit: 4 }, signal), { refreshMs: 30_000 })
  const stuck = useQuery("overview/webhooks", (signal) => api.deliveries({ status: "retrying,failed", limit: 4 }, signal), { refreshMs: 30_000 })
  const open = useQuery("overview/open", (signal) => api.payments({ view: "open", limit: 500 }, signal), { refreshMs: 30_000 })
  // Outcomes: the API's counts for payments created since the period began (UTC days, like the overview).
  const now = useNow(60_000)
  const periodStart = utcDay(now - (period - 1) * 86_400_000)
  const outcomes = useQuery(
    `overview/outcomes/${periodStart}`,
    async (signal) => (await api.payments({ created_from: periodStart, limit: 1, counts: true }, signal)).counts,
    { refreshMs: 30_000, keepPrevious: true },
  )

  // Rows that arrived or changed while the page was open get a wash of brand that drains away.
  const [fresh, setFresh] = React.useState<string[]>([])
  useLiveEvents((event) => {
    if (!event.type.startsWith("payment.") || !event.payment_id) return
    const id = event.payment_id
    setFresh((ids) => (ids.includes(id) ? ids : [...ids.slice(-20), id]))
    overview.reload()
    recent.reload()
    attention.reload()
    open.reload()
    outcomes.reload()
  })

  const asset = overview.data?.asset ?? settings.asset.symbol
  const points: VolumePoint[] = React.useMemo(
    () =>
      (overview.data?.series ?? []).map((day) => ({
        date: day.date,
        volume: toChartNumber(day.volume),
        volumeText: day.volume,
        payments: day.paid,
        created: day.created,
      })),
    [overview.data],
  )

  // A brand-new instance: nothing has ever been created.
  if (instanceCounts?.all === 0 && recent.data?.data.length === 0) {
    return (
      <PageContainer>
        <PageHeader title="Overview" description="Your gateway is running. No payments have come in yet." />
        <FirstRun />
        <CreatePaymentDialog />
      </PageContainer>
    )
  }

  const openRows = open.data?.data ?? []
  const openTotal = open.data?.page.total ?? 0

  return (
    <PageContainer>
      <PageHeader
        title="Overview"
        description={`Volume and payment activity for the last ${period} days.`}
        actions={
          <>
            <PeriodSwitch value={period} />
            <CreatePaymentButton />
          </>
        }
      />

      <div className="stagger space-y-6">
        {overview.error && overview.data && <StaleNote error={overview.error} />}
        {overview.data ? (
          <OverviewStats
            current={overview.data.current}
            previous={overview.data.previous}
            days={overview.data.days}
            asset={asset}
            decimals={overview.data.decimals}
          />
        ) : overview.error ? (
          <ErrorState error={overview.error} title="Couldn’t load the figures" onRetry={overview.reload} />
        ) : (
          <div
            role="status"
            aria-label="Loading figures"
            className="grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-border bg-border shadow-card lg:grid-cols-4"
          >
            {[0, 1, 2, 3].map((i) => (
              <div key={i} className="space-y-3 bg-card p-5">
                <Skeleton className="h-3.5 w-20" />
                <Skeleton className="h-7 w-28" />
                <Skeleton className="h-3 w-32" />
              </div>
            ))}
          </div>
        )}

        <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_340px]">
          <Panel className="flex min-w-0 flex-col">
            {points.length > 0 ? (
              <VolumeChart key={period} points={points} asset={asset} className="flex-1" />
            ) : (
              <div className="min-h-[340px] flex-1 p-5" role="status" aria-label="Loading chart">
                {!overview.error && <Skeleton className="h-full min-h-[300px] w-full" />}
              </div>
            )}
          </Panel>
          <AttentionPanel
            payments={attention.data?.data}
            deliveries={stuck.data?.data}
            total={attention.data?.page.total ?? 0}
            loading={attention.loading}
            error={attention.error}
            onRetry={attention.reload}
          />
        </div>

        <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_340px]">
          <Panel className="min-w-0">
            <PanelHeader
              title="Recent payments"
              aside={<TextLink href="/payments">View all</TextLink>}
              className="border-b border-border md:border-b-0"
            />
            {recent.data ? (
              <PaymentsTable rows={recent.data.data} variant="compact" fresh={fresh} />
            ) : recent.error ? (
              <ErrorState error={recent.error} title="Couldn’t load payments" onRetry={recent.reload} bordered={false} className="py-12" />
            ) : (
              <PaymentsTableSkeleton rows={6} />
            )}
          </Panel>
          <OutcomesPanel
            counts={outcomes.data}
            days={period}
            footer={
              openTotal > 0 && (
                <span className="flex items-center gap-2">
                  <span className="flex size-4 items-center justify-center">
                    <StatusDot tone="brand" live className="size-2" />
                  </span>
                  <span>
                    <span className="num text-foreground">{openTotal}</span> in progress now
                    {/* The total is exact or not shown: only when every open payment is on this page. */}
                    {openRows.length === openTotal && (
                      <>
                        ,{" "}
                        <Amount
                          value={sumAmounts(openRows.map((p) => p.amount), settings.asset.decimals)}
                          asset={asset}
                          className="text-foreground"
                        />
                      </>
                    )}
                  </span>
                </span>
              )
            }
          />
        </div>
      </div>

      <CreatePaymentDialog />
    </PageContainer>
  )
}
