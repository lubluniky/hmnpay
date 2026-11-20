"use client"

import * as React from "react"
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from "recharts"

import { Amount, usePrefersReducedMotion } from "@/components/hmn"
import { ChartContainer, ChartTooltip, type ChartConfig } from "@/components/ui/chart"

/**
 * One day of the overview series, ready to plot. `volume` is a number only so the chart can
 * place the point (geometry); the figure a person reads is `volumeText`, the exact amount.
 */
export interface VolumePoint {
  /** YYYY-MM-DD (UTC). */
  date: string
  volume: number
  volumeText: string
  /** Number of payments that settled as paid or overpaid that day. */
  payments: number
  /** Number of payments created that day. */
  created: number
}

import { cn } from "@/lib/utils"

type Measure = "volume" | "payments"

const MEASURES: { id: Measure; label: string; title: string }[] = [
  { id: "volume", label: "Volume", title: "Daily volume" },
  { id: "payments", label: "Payments", title: "Paid payments per day" },
]

const chartConfig = {
  volume: { label: "Volume", color: "var(--chart-1)" },
  payments: { label: "Paid payments", color: "var(--chart-1)" },
} satisfies ChartConfig


const dayFormatter = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", timeZone: "UTC" })
const longDayFormatter = new Intl.DateTimeFormat("en-US", {
  weekday: "short",
  month: "short",
  day: "numeric",
  timeZone: "UTC",
})
const day = (date: string) => dayFormatter.format(new Date(`${date}T00:00:00Z`))

/** 0 … top in 3–5 round steps (1, 2, 2.5 or 5 × 10ⁿ). */
function niceTicks(max: number, target = 4) {
  if (max <= 0) return [0, 1]
  const raw = max / target
  const magnitude = 10 ** Math.floor(Math.log10(raw))
  const normalized = raw / magnitude
  const step = (normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 2.5 ? 2.5 : normalized <= 5 ? 5 : 10) * magnitude
  const top = Math.ceil(max / step) * step
  return Array.from({ length: Math.round(top / step) + 1 }, (_, i) => i * step)
}

/** A handful of evenly spaced dates, always ending on the latest one. */
function dateTicks(points: VolumePoint[]) {
  const every = points.length <= 10 ? 1 : points.length <= 31 ? 5 : 15
  const ticks: string[] = []
  for (let i = points.length - 1; i >= 0; i -= every) ticks.unshift(points[i].date)
  return ticks
}

function VolumeTooltip({
  active,
  payload,
  measure,
  asset,
  today,
}: {
  active?: boolean
  payload?: ReadonlyArray<{ payload?: VolumePoint }>
  measure: Measure
  asset: string
  /** The last day of the series: still running, so its figures are "so far". */
  today: string
}) {
  const point = payload?.[0]?.payload
  if (!active || !point) return null
  return (
    <div className="min-w-40 rounded-lg border border-border bg-popover px-3 py-2.5 text-xs leading-5 shadow-overlay">
      <div className="text-muted-foreground">
        {longDayFormatter.format(new Date(`${point.date}T00:00:00Z`))}
        {point.date === today && <span className="text-faint"> · today so far</span>}
      </div>
      <div className="mt-1 flex items-center gap-2">
        <span aria-hidden="true" className="h-0.5 w-3 shrink-0 rounded-full bg-chart-1" />
        {measure === "volume" ? (
          <Amount value={point.volumeText} asset={asset} className="text-sm font-medium" />
        ) : (
          <span className="text-sm font-medium">
            <span className="num">{point.payments}</span>{" "}
            <span className="font-normal text-muted-foreground">paid</span>
          </span>
        )}
      </div>
      <div className="pl-5 text-muted-foreground">
        {measure === "volume" ? (
          <>
            <span className="num">{point.payments}</span> paid of <span className="num">{point.created}</span> created
          </>
        ) : (
          <>
            <Amount value={point.volumeText} asset={asset} /> · <span className="num">{point.created}</span> created
          </>
        )}
      </div>
    </div>
  )
}

/**
 * Daily confirmed volume (or paid-payment count) as a single-series area: 2px line in `chart-1`,
 * a flat 10% wash, hairline grid, round axis steps, crosshair + tooltip. No legend — one series,
 * named by the panel title.
 * Motion: the series draws in from the left once (on load, and again for a new period, which
 * remounts the chart); switching the measure morphs the line, point for point.
 */
export function VolumeChart({ points, asset, className }: { points: VolumePoint[]; asset: string; className?: string }) {
  const [measure, setMeasure] = React.useState<Measure>("volume")
  const still = usePrefersReducedMotion()
  // Armed for the first draw and for each change of measure, off in between: otherwise the
  // series would draw itself again whenever the chart is resized (window, sidebar).
  const [animate, setAnimate] = React.useState(true)
  const active = MEASURES.find((m) => m.id === measure)!
  const yTicks = niceTicks(Math.max(...points.map((p) => p[measure])))
  const xTicks = dateTicks(points)
  // Room for the widest label ("10,000") so it is never clipped, and no more.
  const yWidth = Math.max(...yTicks.map((t) => t.toLocaleString("en-US").length)) * 7 + 12

  return (
    <div className={cn("flex min-w-0 flex-col", className)}>
      <div className="flex items-start justify-between gap-4 px-5 pt-4">
        <div className="min-w-0">
          <h2 className="text-sm leading-6 font-semibold tracking-[-0.01em]">
            {active.title}
            {measure === "volume" && `, ${asset}`}
          </h2>
          <p className="text-[13px] leading-5 text-muted-foreground">
            {day(points[0].date)} – {day(points[points.length - 1].date)}
          </p>
        </div>
        <div role="group" aria-label="Chart measure" className="flex h-8 shrink-0 items-center rounded-lg border border-border p-0.5">
          {MEASURES.map((m) => (
            <button
              key={m.id}
              type="button"
              aria-pressed={m.id === measure}
              onClick={() => {
                if (m.id === measure) return
                setMeasure(m.id)
                setAnimate(true)
              }}
              className={cn(
                "focus-ring h-full rounded-md px-2.5 text-[13px] transition-colors",
                m.id === measure ? "bg-muted font-medium text-foreground" : "text-muted-foreground hover:text-foreground",
              )}
            >
              {m.label}
            </button>
          ))}
        </div>
      </div>

      <ChartContainer config={chartConfig} className="mt-3 aspect-auto min-h-[236px] w-full flex-1 pr-3 pb-3 pl-3">
        <AreaChart data={points} margin={{ top: 8, right: 4, bottom: 0, left: 0 }}>
          <CartesianGrid vertical={false} stroke="var(--border)" />
          <XAxis
            dataKey="date"
            ticks={xTicks}
            tickFormatter={day}
            tickLine={false}
            axisLine={false}
            tickMargin={10}
            padding={{ left: 18, right: 18 }}
            interval="preserveEnd"
            minTickGap={20}
            tick={{ fontSize: 11 }}
          />
          <YAxis
            ticks={yTicks}
            domain={[0, yTicks[yTicks.length - 1]]}
            tickFormatter={(value: number) => value.toLocaleString("en-US")}
            tickLine={false}
            axisLine={false}
            tickMargin={8}
            width={yWidth}
            tick={{ fontSize: 11, fontFamily: "var(--font-mono)" }}
          />
          <ChartTooltip
            cursor={{ stroke: "var(--border-strong)", strokeWidth: 1 }}
            content={<VolumeTooltip measure={measure} asset={asset} today={points[points.length - 1].date} />}
            isAnimationActive={false}
            offset={14}
          />
          <Area
            dataKey={measure}
            type="linear"
            stroke={`var(--color-${measure})`}
            strokeWidth={2}
            strokeLinejoin="round"
            strokeLinecap="round"
            fill={`var(--color-${measure})`}
            fillOpacity={0.1}
            dot={false}
            activeDot={{ r: 4, strokeWidth: 2, stroke: "var(--card)", fill: `var(--color-${measure})` }}
            isAnimationActive={animate && !still}
            onAnimationEnd={() => setAnimate(false)}
            animationBegin={60}
            animationDuration={620}
            animationEasing="ease-out"
          />
        </AreaChart>
      </ChartContainer>
    </div>
  )
}
