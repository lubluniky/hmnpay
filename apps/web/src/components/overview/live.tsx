"use client"

import * as React from "react"

import { Amount, Roll, Stat, StatGroup } from "@/components/hmn"
import type { PeriodTotals } from "@/lib/api/types"
import { percentChange } from "@/lib/money"

/**
 * The four headline figures of a period, from the API's overview. Digits roll in once on load,
 * roll to the new period when it is switched, and roll to the new totals when a payment lands
 * (the page fetches the overview again on every payment event).
 */

type Change = { value: string; trend: "up" | "down" | "flat" }

function describe(delta: number | null): Change {
  if (delta === null) return { value: "0.0%", trend: "flat" }
  const trend = Math.abs(delta) < 0.05 ? ("flat" as const) : delta > 0 ? ("up" as const) : ("down" as const)
  return { value: `${Math.abs(delta).toFixed(1)}%`, trend }
}

/** "+12.4%" style change of a count against the period before. */
function countChange(current: number, previous: number): Change {
  return describe(previous ? ((current - previous) / previous) * 100 : null)
}

export function OverviewStats({
  current,
  previous,
  days,
  asset,
  decimals,
}: {
  current: PeriodTotals
  previous: PeriodTotals
  days: number
  asset: string
  decimals: number
}) {
  const versus = `vs previous ${days} days`
  const points = (current.conversion_rate - previous.conversion_rate) * 100

  return (
    <StatGroup>
      <Stat
        label="Volume"
        value={
          <>
            {/* Phones: whole units (cut, not rounded), so six figures never truncate in a half-width
                tile; the exact amount is in the chart and on wider screens. The one that does not
                apply is taken out of flow and made invisible rather than `hidden`: a box that is
                un-hidden plays its CSS entrance again. */}
            <Amount value={current.volume} decimals={0} maxDecimals={0} roll="intro" className="sm:invisible sm:absolute" />
            <Amount value={current.volume} roll="intro" className="max-sm:invisible max-sm:absolute" />
          </>
        }
        unit={asset}
        delta={describe(percentChange(current.volume, previous.volume, decimals))}
        hint={versus}
      />
      <Stat
        label="Paid payments"
        value={<Roll value={current.paid.toLocaleString("en-US")} intro />}
        delta={countChange(current.paid, previous.paid)}
        hint={versus}
      />
      <Stat
        label="Conversion"
        value={<Roll value={(current.conversion_rate * 100).toFixed(1)} intro />}
        unit="%"
        delta={{
          value: `${Math.abs(points).toFixed(1)} pts`,
          trend: Math.abs(points) < 0.05 ? "flat" : points > 0 ? "up" : "down",
        }}
        hint="paid of created"
      />
      <Stat
        label="Average payment"
        value={<Amount value={current.average_ticket} roll="intro" />}
        unit={asset}
        delta={describe(percentChange(current.average_ticket, previous.average_ticket, decimals))}
        hint={versus}
      />
    </StatGroup>
  )
}
