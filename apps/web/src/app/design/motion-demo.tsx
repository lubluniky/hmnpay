"use client"

import * as React from "react"
import { PlusIcon, RotateCcwIcon } from "lucide-react"

import { ConfirmationBlocks } from "@/components/checkout/parts"
import { Amount, PaymentStatusBadge, Roll, Swap, useArrivals } from "@/components/hmn"
import { Button } from "@/components/ui/button"
import type { PaymentStatus } from "@/lib/api/types"
import { cn } from "@/lib/utils"

const CARD = "flex flex-col rounded-xl border border-border bg-card p-5 shadow-card"

function Caption({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <p className="mt-auto pt-4 text-[13px] leading-5 text-pretty text-muted-foreground">
      <span className="font-medium text-foreground">{title}</span> {children}
    </p>
  )
}

/** The shared motion primitives, each with a button that makes it move. */
export function MotionDemo() {
  return (
    <div className="space-y-6">
      <Tokens />
      <div className="grid gap-6 md:grid-cols-2 xl:grid-cols-3">
        <RollDemo />
        <BlocksDemo />
        <SwapDemo />
        <BeamDemo />
        <ArrivalsDemo />
        <StaggerDemo />
      </div>
    </div>
  )
}

const EASINGS = [
  ["ease-out-quint", "Arriving, changing. The default.", "var(--ease-out-quint)"],
  ["ease-in-quint", "Leaving.", "var(--ease-in-quint)"],
  ["ease-in-out-quint", "Between two resting places.", "var(--ease-in-out-quint)"],
  ["ease-settle", "Landing with a little weight.", "var(--ease-settle)"],
] as const
const DURATIONS = [
  ["duration-fast", "120 ms", "Taps, hovers, icon swaps."],
  ["duration-base", "200 ms", "UI changes, fades."],
  ["duration-slow", "320 ms", "Entrances and moves."],
] as const

function Tokens() {
  const [side, setSide] = React.useState(false)
  return (
    <div className="grid gap-6 rounded-xl border border-border bg-card p-5 shadow-card lg:grid-cols-[minmax(0,1.2fr)_minmax(0,1fr)]">
      <div>
        <div className="flex items-center justify-between gap-3">
          <h3 className="text-sm leading-6 font-medium">Easing</h3>
          <Button type="button" variant="outline" size="sm" onClick={() => setSide((v) => !v)}>
            Run
          </Button>
        </div>
        <ul className="mt-3 space-y-3">
          {EASINGS.map(([name, use, curve]) => (
            <li key={name} className="grid grid-cols-[150px_minmax(0,1fr)] items-center gap-4">
              <div className="min-w-0">
                <div className="num text-xs leading-5">{name}</div>
                <div className="text-xs leading-4 text-muted-foreground">{use}</div>
              </div>
              <div className="relative h-5 rounded-full bg-muted px-2.5">
                <div className="relative h-full">
                  <span
                    className="absolute top-1/2 -mt-1.5 -ml-1.5 size-3 rounded-full bg-brand motion-safe:transition-[left] motion-safe:duration-700"
                    style={{ left: side ? "100%" : "0%", transitionTimingFunction: curve }}
                  />
                </div>
              </div>
            </li>
          ))}
        </ul>
      </div>
      <div>
        <h3 className="text-sm leading-6 font-medium">Duration</h3>
        <ul className="mt-3 divide-y divide-border">
          {DURATIONS.map(([name, ms, use]) => (
            <li key={name} className="flex items-baseline gap-3 py-2 text-[13px] leading-5">
              <span className="num w-28 shrink-0 text-xs">{name}</span>
              <span className="num w-14 shrink-0 text-muted-foreground">{ms}</span>
              <span className="text-muted-foreground">{use}</span>
            </li>
          ))}
        </ul>
        <p className="mt-3 text-xs leading-[18px] text-pretty text-faint">
          Transform and opacity only, nothing on an idle screen, everything gated on reduced motion.
        </p>
      </div>
    </div>
  )
}

function RollDemo() {
  const [total, setTotal] = React.useState(100673.46)
  const [paid, setPaid] = React.useState(851)
  return (
    <div className={CARD} data-demo="roll">
      <div className="text-[13px] leading-5 text-muted-foreground">Volume</div>
      <Amount value={total.toFixed(2)} asset roll className="mt-1 text-2xl leading-8 font-medium tracking-[-0.04em]" />
      <div className="num mt-1 text-[13px] leading-5 text-muted-foreground">
        <Roll value={paid.toLocaleString("en-US")} className="text-foreground" /> paid payments
      </div>
      <div className="mt-4 flex gap-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => {
            setTotal((v) => v + 149)
            setPaid((v) => v + 1)
          }}
        >
          <PlusIcon data-icon="inline-start" />
          149.00 paid
        </Button>
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => {
            setTotal((v) => (v > 50_000 ? 21930.12 : 100673.46))
            setPaid((v) => (v > 500 ? 193 : 851))
          }}
        >
          Switch period
        </Button>
      </div>
      <Caption title="Roll.">
        Digits that differ roll, up when the number grew. <code className="num text-xs">&lt;Amount roll&gt;</code>,{" "}
        <code className="num text-xs">&lt;Roll value&gt;</code>; <code className="num text-xs">intro</code> rolls them in once on load.
      </Caption>
    </div>
  )
}

function BlocksDemo() {
  const [n, setN] = React.useState(9)
  return (
    <div className={CARD} data-demo="blocks">
      <div className="flex items-baseline justify-between">
        <span className="text-[13px] leading-5 text-muted-foreground">Confirmations</span>
        <span className="num text-sm leading-5 text-muted-foreground">
          <Roll value={String(n)} className="text-base font-medium text-foreground" /> / 15
        </span>
      </div>
      <ConfirmationBlocks confirmations={n} required={15} className="mt-2.5" />
      <div className="mt-4 flex gap-2">
        <Button type="button" variant="outline" size="sm" disabled={n >= 15} onClick={() => setN((v) => Math.min(15, v + 1))}>
          Next block
        </Button>
        <Button type="button" variant="ghost" size="sm" className="text-muted-foreground" onClick={() => setN(9)}>
          <RotateCcwIcon data-icon="inline-start" />
          Reset
        </Button>
      </div>
      <Caption title="Blocks.">A block that fills while you watch settles into place and the count rolls. Blocks full on load are at rest.</Caption>
    </div>
  )
}

const CYCLE: PaymentStatus[] = ["awaiting", "detected", "confirming", "paid"]

function SwapDemo() {
  const [i, setI] = React.useState(0)
  const status = CYCLE[i % CYCLE.length]
  return (
    <div className={CARD}>
      <div className="flex h-8 items-center">
        <Swap id={status}>
          <PaymentStatusBadge status={status} confirmations={7} required={15} live={false} />
        </Swap>
      </div>
      <div className="mt-4">
        <Button type="button" variant="outline" size="sm" onClick={() => setI((v) => v + 1)}>
          Next status
        </Button>
      </div>
      <Caption title="Swap.">
        Something replaced in place eases in. <code className="num text-xs">&lt;Swap id&gt;</code> around a badge or a label.
      </Caption>
    </div>
  )
}

function BeamDemo() {
  const [laps, setLaps] = React.useState(0)
  return (
    <div className={CARD} data-demo="beam">
      <div className="relative rounded-xl border border-border-strong bg-background/60 px-4 py-3">
        {laps > 0 && <span key={laps} aria-hidden="true" className="beam-lap" />}
        <div className="text-xs leading-4 text-muted-foreground">Address</div>
        <div className="num mt-1 text-[15px] leading-6">0x 7A3f 9C2e 41b8 … 1b92</div>
      </div>
      <div className="mt-4">
        <Button type="button" variant="outline" size="sm" onClick={() => setLaps((v) => v + 1)}>
          Run a lap
        </Button>
      </div>
      <Caption title="Beam lap.">
        One lap of light around a card, once, in answer to an action (the checkout runs it when the address or amount is copied).
        Never looping. <code className="num text-xs">beam-lap</code>
      </Caption>
    </div>
  )
}

function ArrivalsDemo() {
  const [rows, setRows] = React.useState(() => [20429, 20428, 20423, 20421])
  const [fresh, setFresh] = React.useState<number[]>([])
  const ref = useArrivals<HTMLUListElement>(rows.map(String))
  return (
    <div className={CARD}>
      <ul ref={ref} className="divide-y divide-border overflow-hidden rounded-lg border border-border">
        {rows.map((id) => (
          <li
            key={id}
            data-row=""
            className={cn("flex h-9 items-center justify-between px-3 text-[13px]", fresh.includes(id) && "motion-safe:animate-row-flash")}
          >
            <span className="num font-medium">ORD-{id}</span>
            <PaymentStatusBadge status={fresh.includes(id) ? "paid" : "awaiting"} size="sm" live={false} />
          </li>
        ))}
      </ul>
      <div className="mt-4">
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => {
            const id = Math.max(...rows) + 3
            setRows((list) => [id, ...list].slice(0, 4))
            setFresh((list) => [...list, id])
          }}
        >
          <PlusIcon data-icon="inline-start" />
          New payment
        </Button>
      </div>
      <Caption title="Arrivals.">
        A new row glides in at the top, the rest make room, and it carries a wash of brand for a moment.{" "}
        <code className="num text-xs">useArrivals</code>, <code className="num text-xs">animate-row-flash</code>
      </Caption>
    </div>
  )
}

function StaggerDemo() {
  const [run, setRun] = React.useState(0)
  return (
    <div className={CARD}>
      <div key={run} className={cn("space-y-2", run > 0 && "stagger")}>
        {["Figures", "Chart", "Table"].map((name) => (
          <div key={name} className="flex h-8 items-center rounded-lg border border-border bg-background/60 px-3 text-[13px] text-muted-foreground">
            {name}
          </div>
        ))}
      </div>
      <div className="mt-4">
        <Button type="button" variant="outline" size="sm" onClick={() => setRun((v) => v + 1)}>
          Run
        </Button>
      </div>
      <Caption title="Stagger.">
        The sections of a first screen rise together, 45 ms apart, once. CSS only. <code className="num text-xs">stagger</code>
      </Caption>
    </div>
  )
}
