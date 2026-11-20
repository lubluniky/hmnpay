"use client"

import * as React from "react"
import { QRCodeSVG } from "qrcode.react"

import {
  AddressGroups,
  Amount,
  amountText,
  AssetIcon,
  CopyGlyph,
  networkName,
  NETWORK_STANDARD,
  NetworkIcon,
  StatusDot,
  toneClasses,
  useCopy,
  useWaveEntrance,
  type Tone,
} from "@/components/hmn"
import { inviteAt, type Invite } from "@/lib/invite"
import { serverNow, toServerTime } from "@/lib/server-clock"
import { cn } from "@/lib/utils"

/* -------------------------------------------------------------------------- */
/* Countdown                                                                  */
/* -------------------------------------------------------------------------- */

/**
 * Milliseconds left until `expiresAt`, ticking. The deadline is a moment on the server's clock,
 * so the time is counted on that clock as well as the page can tell it (lib/server-clock): a
 * device whose own clock is minutes off still shows the time that is really left. Only ever
 * rendered in the browser, after the payment has loaded, so there is no server-rendered value
 * to disagree with.
 */
export function useCountdown(expiresAt: string) {
  const deadline = Date.parse(expiresAt)
  const [now, setNow] = React.useState(() => serverNow())

  React.useEffect(() => {
    const tick = () => setNow(serverNow())
    const id = setInterval(tick, 250)
    return () => clearInterval(id)
  }, [])

  return Math.max(0, deadline - now)
}

/** [inviteAt], kept current while the page is open. */
export function useInvite(expiresAt: string, contactAt: number | undefined): Invite {
  const [now, setNow] = React.useState(() => Date.now())
  React.useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 500)
    return () => clearInterval(id)
  }, [])
  return inviteAt(expiresAt, contactAt, now, toServerTime(0))
}

export function formatClock(ms: number) {
  const total = Math.max(0, Math.ceil(ms / 1000))
  const h = Math.floor(total / 3600)
  const mm = String(Math.floor((total % 3600) / 60)).padStart(2, "0")
  const ss = String(total % 60).padStart(2, "0")
  return h > 0 ? `${h}:${mm}:${ss}` : `${mm}:${ss}`
}

const PIE_R = 2.75
const PIE_C = 2 * Math.PI * PIE_R

/** "Expires in 23:59" with a depleting ring. Tabular digits: the layout never moves. */
export function Countdown({
  expiresAt,
  createdAt,
  label = "Expires in",
  className,
}: {
  expiresAt: string
  createdAt: string
  label?: string
  className?: string
}) {
  const remaining = useCountdown(expiresAt)
  const total = Math.max(1, Date.parse(expiresAt) - Date.parse(createdAt))
  const fraction = Math.min(1, Math.max(0, remaining / total))
  const urgent = remaining <= 5 * 60_000

  return (
    <div
      role="timer"
      className={cn(
        "inline-flex shrink-0 items-center gap-2 text-[13px] leading-5 whitespace-nowrap",
        urgent ? "text-warning-fg" : "text-muted-foreground",
        className,
      )}
    >
      {/* A draining pie, not a ring: a partial ring reads as a loading spinner. */}
      <svg viewBox="0 0 16 16" aria-hidden="true" className="size-4 -rotate-90">
        <circle cx="8" cy="8" r="6.75" fill="none" stroke="currentColor" strokeOpacity="0.45" strokeWidth="1.5" />
        <circle
          cx="8"
          cy="8"
          r={PIE_R}
          fill="none"
          stroke="currentColor"
          strokeWidth={PIE_R * 2}
          strokeDasharray={`${(PIE_C * fraction).toFixed(3)} ${PIE_C.toFixed(3)}`}
        />
      </svg>
      <span>{label}</span>
      <span className={cn("num font-medium", urgent ? "text-warning-fg" : "text-foreground")}>
        {formatClock(remaining)}
      </span>
    </div>
  )
}

/* -------------------------------------------------------------------------- */
/* QR                                                                         */
/* -------------------------------------------------------------------------- */

/**
 * QR on a light tile in both themes (scanners need dark-on-light and a quiet zone).
 * The asset mark sits in the centre; error correction is set high enough to cover it.
 */
export function QrTile({ value, size = 160, className }: { value: string; size?: number; className?: string }) {
  return (
    <div
      className={cn("relative shrink-0 rounded-xl bg-white p-3.5 ring-1 ring-black/[0.07] dark:ring-0", className)}
      style={{ width: size + 28, height: size + 28 }}
    >
      <QRCodeSVG
        value={value}
        size={size}
        level="Q"
        marginSize={0}
        bgColor="transparent"
        fgColor="#0a0a0a"
        role="img"
        aria-label="QR code of the payment address"
        className="block size-full"
      />
      <span className="absolute top-1/2 left-1/2 flex -translate-x-1/2 -translate-y-1/2 rounded-full bg-white p-1">
        <AssetIcon size={Math.round(size * 0.2)} />
      </span>
    </div>
  )
}

/* -------------------------------------------------------------------------- */
/* Pay-to card: the network and the address as one object                     */
/* -------------------------------------------------------------------------- */

/**
 * Mirrors the "send" form of every wallet and exchange: Network, Address, Amount — the three
 * things the payer has to enter, in one bordered object and in the order they are asked for.
 * The network is a first-class field, not a warning, so the address is never seen without it.
 * Address and amount rows are whole-row copy targets.
 */
export function PayTo({
  address,
  chainId,
  amount,
  amountLabel = "Amount",
  amountNote,
  onAddressCopied,
  className,
}: {
  address: string
  /** The payment's chain: its name is shown with the address. */
  chainId: number
  /** Decimal string, e.g. "149.00". */
  amount: string
  amountLabel?: string
  amountNote?: React.ReactNode
  onAddressCopied?: () => void
  className?: string
}) {
  // Each copy sends one lap of light around the card: taken, and we are watching for it.
  const [laps, setLaps] = React.useState(0)
  const lap = () => setLaps((n) => n + 1)
  return (
    <div className={cn("relative divide-y divide-border overflow-hidden rounded-xl border border-border-strong bg-background/60", className)}>
      {laps > 0 && <span key={laps} aria-hidden="true" className="beam-lap" />}
      <div className="flex items-center gap-3 px-4 py-3">
        <div className="min-w-0 flex-1">
          <div className="text-xs leading-4 text-muted-foreground">Network</div>
          <div className="mt-1 flex items-center gap-2 text-[15px] leading-6 font-medium">
            <NetworkIcon size={20} />
            <span className="truncate">{networkName(chainId)}</span>
            <span className="num rounded-[5px] border border-border-strong px-1.5 text-[11px] leading-[18px] font-medium tracking-normal text-muted-foreground">
              {NETWORK_STANDARD}
            </span>
          </div>
        </div>
      </div>
      <CopyRow
        label="Address"
        value={address}
        noun="Address"
        onCopied={() => {
          lap()
          onAddressCopied?.()
        }}
      >
        <AddressGroups value={address} />
      </CopyRow>
      <CopyRow label={amountLabel} value={amountText(amount).replaceAll(",", "")} noun="Amount" onCopied={lap}>
        <span className="flex flex-wrap items-baseline gap-x-2.5">
          <Amount value={amount} asset className="text-[15px] leading-6 font-medium" />
          {amountNote && <span className="text-xs leading-5 text-muted-foreground">{amountNote}</span>}
        </span>
      </CopyRow>
    </div>
  )
}

function CopyRow({
  label,
  value,
  noun,
  onCopied,
  children,
}: {
  label: string
  value: string
  noun: string
  onCopied?: () => void
  children: React.ReactNode
}) {
  const { copied, copy } = useCopy(2200)
  return (
    <button
      type="button"
      onClick={async () => {
        if (await copy(value)) onCopied?.()
      }}
      className="group flex min-h-11 w-full cursor-pointer flex-col gap-1 px-4 py-3 text-left transition-colors outline-none hover:bg-muted/50 focus-visible:bg-muted/50 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring"
    >
      <span className="flex w-full items-center justify-between gap-3">
        <span className="text-xs leading-4 text-muted-foreground">{label}</span>
        <span
          className={cn(
            "inline-flex items-center gap-1.5 text-xs leading-4 font-medium transition-colors",
            copied ? "text-success-fg" : "text-muted-foreground group-hover:text-foreground",
          )}
        >
          <CopyGlyph copied={copied} className="size-3.5" />
          {copied ? "Copied" : "Copy"}
        </span>
      </span>
      {children}
      <span className="sr-only" role="status">
        {copied ? `${noun} copied` : ""}
      </span>
    </button>
  )
}

/* -------------------------------------------------------------------------- */
/* Confirmations                                                              */
/* -------------------------------------------------------------------------- */

/**
 * One block per required confirmation. The next expected block breathes; a block that fills
 * while you watch lands with a small settle (blocks already full on load are at rest).
 */
export function ConfirmationBlocks({
  confirmations,
  required,
  tone = "info",
  className,
}: {
  confirmations: number
  required: number
  tone?: Tone
  className?: string
}) {
  const [onLoad] = React.useState(confirmations)
  return (
    <div
      role="progressbar"
      aria-label="Confirmations"
      aria-valuemin={0}
      aria-valuemax={required}
      aria-valuenow={Math.min(confirmations, required)}
      aria-valuetext={`${Math.min(confirmations, required)} of ${required} confirmations`}
      className={cn("flex gap-[3px]", className)}
    >
      {Array.from({ length: required }, (_, i) => (
        <span
          key={i}
          className={cn(
            "h-5 flex-1 origin-bottom rounded-[3px] transition-colors duration-500",
            i < confirmations
              ? cn(toneClasses[tone].solid, i >= onLoad && "motion-safe:animate-block-fill")
              : i === confirmations
                ? cn(toneClasses[tone].solid, "opacity-40 motion-safe:animate-tick")
                : "bg-muted",
          )}
        />
      ))}
    </div>
  )
}

/* -------------------------------------------------------------------------- */
/* Small shared bits                                                          */
/* -------------------------------------------------------------------------- */

/** "● Watching for your payment" — the quiet live line at the bottom of an open payment. */
export function LiveLine({
  tone = "brand",
  className,
  children,
}: {
  tone?: Tone
  className?: string
  children: React.ReactNode
}) {
  return (
    <div className={cn("flex items-center gap-2.5 text-[13px] leading-5 text-muted-foreground", className)}>
      <span className="flex size-4 items-center justify-center">
        <StatusDot tone={tone} live className="size-2" />
      </span>
      <span>{children}</span>
    </div>
  )
}

/** Round state mark at the top of a finished / stopped payment. */
export function StateMark({
  tone,
  solid = false,
  className,
  children,
  ...props
}: Omit<React.ComponentProps<"span">, "children"> & {
  tone: Tone
  /** Filled disc with a soft halo: reserved for the one moment that deserves it (paid). */
  solid?: boolean
  children: React.ReactNode
}) {
  // Carried in by a wave: the wave is the entrance, the mark is already there.
  const still = useWaveEntrance()
  return (
    <span
      aria-hidden="true"
      className={cn(
        "flex size-12 items-center justify-center rounded-full [&_svg]:size-[22px]",
        !still && "motion-safe:animate-pop-in",
        solid
          ? cn(toneClasses[tone].solid, "text-background shadow-[0_0_0_6px_var(--success-subtle)] [&_svg]:size-6")
          : cn("border", toneClasses[tone].badge),
        className,
      )}
      {...props}
    >
      {children}
    </span>
  )
}

/** A check that draws itself once. Static under reduced motion, and when a wave carried it in. */
export function DrawnCheck({ className }: { className?: string }) {
  const still = useWaveEntrance()
  return (
    <svg viewBox="0 0 24 24" fill="none" className={className}>
      <path
        d="M5 12.5l4.5 4.5L19 7.5"
        stroke="currentColor"
        strokeWidth="2.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        pathLength={1}
        strokeDasharray={1}
        className={still ? undefined : "motion-safe:animate-draw"}
      />
    </svg>
  )
}

/* -------------------------------------------------------------------------- */
/* Progress rail                                                              */
/* -------------------------------------------------------------------------- */

export interface RailStep {
  label: string
  detail?: React.ReactNode
  state: "done" | "current" | "todo" | "stopped"
  tone?: Tone
}

/**
 * Three-step vertical rail: created → sent → confirmed. The labels stay put and only the
 * markers move, like parcel tracking. Markers are static — the pulse belongs to `LiveLine`.
 */
export function ProgressRail({ steps, className }: { steps: RailStep[]; className?: string }) {
  // A step that completes while you watch gets its check popped in; what was done on load is at rest.
  const [onLoad] = React.useState(() => steps.map((step) => step.state))
  return (
    <ol className={cn("flex flex-col", className)}>
      {steps.map((step, i) => {
        const last = i === steps.length - 1
        const tone = step.tone ?? "neutral"
        return (
          <li key={step.label} className="relative flex gap-3 pb-4 last:pb-0" aria-current={step.state === "current" ? "step" : undefined}>
            {!last && (
              <span
                aria-hidden="true"
                className={cn("absolute top-5 bottom-0 left-[7.5px] w-px", step.state === "done" ? "bg-border-strong" : "bg-border")}
              />
            )}
            <span aria-hidden="true" className="relative mt-0.5 flex size-4 shrink-0 items-center justify-center">
              {step.state === "done" && (
                <span
                  className={cn(
                    "flex size-4 items-center justify-center rounded-full",
                    step.tone ? toneClasses[tone].solid : "bg-foreground",
                    onLoad[i] !== "done" && "motion-safe:animate-pop-in",
                  )}
                >
                  <svg viewBox="0 0 12 12" fill="none" className="size-2.5 text-background">
                    <path d="M2.5 6.2l2.3 2.3 4.7-5" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
                  </svg>
                </span>
              )}
              {step.state === "current" && (
                <span className={cn("size-2 rounded-full ring-[3px]", toneClasses[tone].solid, toneClasses[tone].ring)} />
              )}
              {step.state === "todo" && <span className="size-2 rounded-full border border-border-strong" />}
              {step.state === "stopped" && (
                <span className={cn("flex size-4 items-center justify-center rounded-full border", toneClasses[tone].badge)}>
                  <svg viewBox="0 0 12 12" fill="none" className="size-2.5">
                    <path d="M3.5 3.5l5 5m0-5l-5 5" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
                  </svg>
                </span>
              )}
            </span>
            <span className="flex min-w-0 flex-1 items-baseline justify-between gap-3 text-[13px] leading-5">
              <span className={cn(step.state === "todo" ? "text-faint" : "text-foreground", step.state === "current" && "font-medium")}>
                {step.label}
              </span>
              {step.detail && <span className="num shrink-0 text-xs text-muted-foreground">{step.detail}</span>}
            </span>
          </li>
        )
      })}
    </ol>
  )
}
