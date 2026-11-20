"use client"

import * as React from "react"
import { ArrowRightIcon, RotateCcwIcon } from "lucide-react"

import { ConfirmationBlocks, DrawnCheck, StateMark } from "@/components/checkout/parts"
import { Amount, DetailList, DetailRow, PaymentStatusBadge, WaveTransition } from "@/components/hmn"
import { Button } from "@/components/ui/button"

const CARD = "overflow-hidden rounded-xl border border-border bg-card shadow-card"
const LAYER = "flex min-h-[292px] flex-col p-5"

/** Two live waves for the styleguide: one from a press, one from an event nobody pressed anything for. */
export function WaveDemo() {
  return (
    <div className="grid gap-6 lg:grid-cols-2">
      <PressDemo />
      <EventDemo />
    </div>
  )
}

function Caption({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <p className="mt-3 text-[13px] leading-5 text-pretty text-muted-foreground">
      <span className="font-medium text-foreground">{title}</span> {children}
    </p>
  )
}

function Receipt({ title, onReset }: { title: string; onReset: () => void }) {
  return (
    <>
      <div className="flex items-center gap-3.5">
        <StateMark tone="success" solid className="size-10 [&_svg]:size-5" data-wave-source="">
          <DrawnCheck />
        </StateMark>
        <div>
          <div className="text-base leading-6 font-semibold tracking-[-0.015em]">{title}</div>
          <div className="text-[13px] leading-5 text-muted-foreground">Order <span className="num">ORD-20480</span></div>
        </div>
      </div>
      <DetailList className="mt-4 border-y border-border">
        <DetailRow label="Amount">
          <Amount value="149.00" asset className="font-medium" />
        </DetailRow>
        <DetailRow label="Status">
          <PaymentStatusBadge status="paid" />
        </DetailRow>
      </DetailList>
      <Button type="button" variant="outline" className="mt-auto w-full" onClick={onReset} data-autofocus="">
        <RotateCcwIcon data-icon="inline-start" />
        Run it again
      </Button>
    </>
  )
}

function PressDemo() {
  const [done, setDone] = React.useState(false)
  return (
    <div>
      <WaveTransition
        revealed={done}
        className={CARD}
        layerClassName={LAYER}
        data-demo="wave-press"
        revealedContent={<Receipt title="Payment created" onReset={() => setDone(false)} />}
      >
        <div className="text-base leading-6 font-semibold tracking-[-0.015em]">Create payment</div>
        <div className="text-[13px] leading-5 text-muted-foreground">Pro plan — annual</div>
        <DetailList className="mt-4 border-y border-border">
          <DetailRow label="Amount">
            <Amount value="149.00" asset className="font-medium" />
          </DetailRow>
          <DetailRow label="Expires after">30 minutes</DetailRow>
        </DetailList>
        <Button type="button" className="mt-auto w-full" onClick={() => setDone(true)} data-wave-source="">
          Create payment
          <ArrowRightIcon data-icon="inline-end" />
        </Button>
      </WaveTransition>
      <Caption title="From a press.">
        <code className="num text-xs">type=&quot;vertical&quot;</code>: the wave leaves the button marked{" "}
        <code className="num text-xs">data-wave-source</code> and leans toward the pointer.
      </Caption>
    </div>
  )
}

function EventDemo() {
  const [done, setDone] = React.useState(false)
  return (
    <div>
      <WaveTransition
        revealed={done}
        type="center"
        className={CARD}
        layerClassName={LAYER}
        data-demo="wave-event"
        revealedContent={<Receipt title="Payment complete" onReset={() => setDone(false)} />}
      >
        <div className="text-base leading-6 font-semibold tracking-[-0.015em]">Confirming your payment</div>
        <div className="text-[13px] leading-5 text-muted-foreground">The network is confirming it now.</div>
        <div className="mt-5 flex items-baseline justify-between text-[13px] leading-5 text-muted-foreground">
          <span>Confirmations</span>
          <span className="num">
            <span className="font-medium text-foreground">14</span> / 15
          </span>
        </div>
        <ConfirmationBlocks confirmations={14} required={15} className="mt-2.5" />
        <Button type="button" variant="outline" className="mt-auto w-full" onClick={() => setDone(true)}>
          Simulate the last confirmation
        </Button>
      </WaveTransition>
      <Caption title="From an event.">
        <code className="num text-xs">type=&quot;center&quot;</code>: a ring out from the mark the new view is about, for
        changes nobody pressed anything for.
      </Caption>
    </div>
  )
}
