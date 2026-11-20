// SPDX-License-Identifier: GPL-3.0-or-later
// hmnpay — Copyright (C) 2026 lubluniky and the hmnpay contributors. Distributed under the GNU GPL,
// version 3 or later (LICENSE), with additional terms under its section 7 (ADDITIONAL_TERMS.md):
// the "Powered by hmnpay" author attribution on payment pages must be preserved.
"use client"

import * as React from "react"
import {
  ArrowRightIcon,
  BanIcon,
  CircleHelpIcon,
  ClockIcon,
  QrCodeIcon,
  SearchXIcon,
  ShieldCheckIcon,
  TriangleAlertIcon,
  WalletIcon,
  XIcon,
} from "lucide-react"
import { AnimatePresence, MotionConfig, motion } from "motion/react"

import {
  AddressGroups,
  Amount,
  Callout,
  DetailList,
  DetailRow,
  MerchantAvatar,
  networkName,
  NETWORK_STANDARD,
  NetworkBadge,
  PaymentStatusBadge,
  PoweredBy,
  Roll,
  SoundProvider,
  SoundToggle,
  StatusBadge,
  Swap,
  ThemeToggle,
  TxHash,
  useSound,
  WaveTransition,
} from "@/components/hmn"
import { Button } from "@/components/ui/button"
import { Drawer, DrawerContent, DrawerDescription, DrawerHeader, DrawerTitle, DrawerTrigger } from "@/components/ui/drawer"
import { Skeleton } from "@/components/ui/skeleton"
import type { Checkout as CheckoutData, Merchant, PaymentStatus } from "@/lib/api/types"
import { useCheckout, type LiveMode } from "@/lib/checkout-stream"
import { describeError } from "@/lib/errors"
import type { Invite } from "@/lib/invite"
import { FINALISING_PATIENCE_SECONDS, payerFinalising } from "@/lib/finality"
import { formatDateTime, formatTime } from "@/lib/format"
import { compareAmounts, ratioPercent, subtractAmounts } from "@/lib/money"
import { safeEmail, safeHttpUrl, safePaymentUri } from "@/lib/url"
import { useNow } from "@/lib/use-now"
import { isPaymentId, usePathSegment } from "@/lib/use-route"
import { cn } from "@/lib/utils"

import {
  ConfirmationBlocks,
  Countdown,
  DrawnCheck,
  LiveLine,
  PayTo,
  ProgressRail,
  QrTile,
  StateMark,
  useInvite,
  type RailStep,
} from "./parts"

/**
 * Hosted checkout — what a merchant's customer sees. No dashboard chrome.
 *
 * The page shows exactly what the public API says about the payment (GET
 * /api/v1/public/payments/{id} and its event stream) and follows it live. What each status
 * means here, in the payer's terms:
 *
 *   awaiting    nothing that counts has arrived: how to pay, and the time left
 *   detected    part of the amount arrived: send the rest (the top-up), time left
 *   confirming  the full amount is on-chain: the confirmation counter; once every confirmation
 *               is in and only the chain's finality is missing (awaiting_finality), a calm
 *               "payment received" wait without numbers
 *   paid        done — the wave carries the card into the receipt
 *   overpaid    done, with extra: how to get the difference back
 *   underpaid   the time ran out with part of the amount received
 *   expired     the time ran out with nothing that counts (or the merchant cancelled it);
 *               money that shows up on the address afterwards is said to be there, not lost
 *   failed      the transfer that was seen dropped out of the chain and the time ran out
 *
 * Every state, including loading, not-found and errors, carries the "Powered by hmnpay"
 * attribution in the footer.
 */

/* --------------------------------- root ----------------------------------- */

const isSettled = (status: PaymentStatus) => status === "paid" || status === "overpaid"
/** Funds have been seen on-chain: from here the payment can complete at any moment. */
const isInFlight = (status: PaymentStatus) => status === "detected" || status === "confirming"

const received = (c: CheckoutData) => compareAmounts(c.amount_received, "0", c.decimals) > 0
/** What the payer may call the order: the merchant's order id, if there is one. */
const supportContact = (merchant: Merchant) => {
  const email = safeEmail(merchant.support_email)
  const url = safeHttpUrl(merchant.support_url)
  return { email, url }
}

/** The page for /pay/{id}: finds the id in the address bar and follows that payment. */
export function CheckoutPage() {
  const id = usePathSegment(1)
  const valid = id !== null && isPaymentId(id)
  const live = useCheckout(valid ? id : null)

  React.useEffect(() => {
    const name = live.checkout?.merchant.name
    if (name) document.title = `Pay ${name}`
  }, [live.checkout?.merchant.name])

  let body: React.ReactNode
  if (id !== null && (!valid || live.notFound)) body = <NotFoundCard />
  else if (live.checkout) body = <CheckoutView checkout={live.checkout} mode={live.mode} stale={live.stale} contactAt={live.contactAt} />
  else if (live.error) body = <ErrorCard message={describeError(live.error)} onRetry={live.retry} />
  else body = <LoadingCard />

  return (
    <SoundProvider surface="checkout">
      <MotionConfig reducedMotion="user">
        <div
          data-checkout=""
          data-status={live.checkout?.status}
          data-live={live.mode}
          className="flex min-h-svh flex-col items-center px-3 pt-3 pb-6 sm:px-6 sm:pt-10 lg:justify-center lg:py-12"
        >
          <div className="w-full max-w-[460px] lg:max-w-[960px]">
            <main>{body}</main>
            <CheckoutFooter checkout={live.checkout} />
          </div>
        </div>
      </MotionConfig>
    </SoundProvider>
  )
}

/**
 * The quiet cues: one soft note when the full amount is first seen, a neutral double tap when
 * only part arrived, a low close when the payment ends unpaid. Only for changes that happen
 * while the page is open, never for the state it loads in. (Paid belongs to the wave.)
 */
function useStatusCues(status: PaymentStatus) {
  const play = useSound()
  const previous = React.useRef(status)
  React.useEffect(() => {
    const from = previous.current
    previous.current = status
    if (from === status) return
    if (status === "confirming") play("detected")
    else if (status === "detected") play("attention")
    else if (status === "expired" || status === "failed" || status === "underpaid") play("closed")
  }, [status, play])
}

function CheckoutView({ checkout, mode, stale, contactAt }: { checkout: CheckoutData; mode: LiveMode; stale: boolean; contactAt: number | undefined }) {
  useStatusCues(checkout.status)
  // Whether the page may still ask for money: the deadline and the server's last answer decide
  // (see lib/invite). It matters only while the payment is open.
  const invite = useInvite(checkout.expires_at, contactAt)
  const asking = isAsking(checkout.status)

  // The one loud moment of the checkout: when the payment completes while the payer is watching,
  // a wave carries the card they were looking at into the receipt. `before` is that card; it
  // follows every other change as it happens and holds still once the payment is settled.
  // Loading an already paid payment shows the receipt at rest (no wave, no sound).
  const settled = isSettled(checkout.status)
  const [before, setBefore] = React.useState(checkout)
  if (!settled && before !== checkout) setBefore(checkout)

  return (
    <>
      {/* Outside the wave: the new status is announced when it happens, not when the wave lands. */}
      <p className="sr-only" role="status">
        Payment status: {checkout.status}
      </p>
      <WaveTransition
        revealed={settled}
        type="center"
        armed={isInFlight(checkout.status)}
        cue="paid"
        className="overflow-hidden rounded-2xl border border-border bg-card shadow-raised"
        // 498 + the card's two hairlines = the 500px card.
        layerClassName="lg:grid lg:min-h-[498px] lg:grid-cols-[400px_minmax(0,1fr)]"
        revealedContent={<CheckoutCard checkout={checkout} mode={mode} invite={invite} />}
      >
        <CheckoutCard checkout={before} mode={mode} invite={invite} />
      </WaveTransition>
      {/* Under the card only while the pane itself does not already say that the server is out
          of reach: past the deadline it does, and "not affected" would be the wrong thing to
          read next to a deadline that has passed. */}
      {stale && !(asking && invite !== "open") && (
        <p role="status" data-slot="stale-note" className="mt-3 px-2 text-center text-[13px] leading-5 text-warning-fg">
          {asking ? "Connection lost. Trying again; your payment is not affected." : "Connection lost. Trying again."}
        </p>
      )}
    </>
  )
}

/** The payment is waiting for money: the page shows where to send it, while it may. */
const isAsking = (status: PaymentStatus) => status === "awaiting" || status === "detected"

/** The key of the right-hand pane: a change of it plays the pane transition. */
function paneKey(c: CheckoutData) {
  if (c.status === "expired") return c.cancelled_at ? "cancelled" : "expired"
  if (c.status === "confirming" && c.awaiting_finality) return "finalising"
  return c.status
}

/** Both halves of the card for one payment: the order on the left, what to do about it on the right. */
function CheckoutCard({ checkout, mode, invite }: PaneProps) {
  // Past the deadline the open panes give way to one that asks for nothing.
  const closed = isAsking(checkout.status) && (invite === "closing" || invite === "unknown")
  return (
    <>
      <Summary checkout={checkout} />
      <div className="relative flex min-w-0 flex-col">
        <AnimatePresence mode="wait" initial={false}>
          <motion.div
            key={closed ? `${paneKey(checkout)}:deadline` : paneKey(checkout)}
            data-pane={paneKey(checkout)}
            data-invite={isAsking(checkout.status) ? invite : undefined}
            className="flex flex-1 flex-col p-5 sm:p-6 lg:p-8"
            initial={{ opacity: 0, y: 8 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -4 }}
            transition={{ duration: 0.28, ease: [0.22, 1, 0.36, 1] }}
          >
            {closed ? <DeadlinePane checkout={checkout} mode={mode} invite={invite} /> : <StatePane checkout={checkout} mode={mode} invite={invite} />}
          </motion.div>
        </AnimatePresence>
      </div>
    </>
  )
}

/* -------------------------------- summary --------------------------------- */

function Summary({ checkout }: { checkout: CheckoutData }) {
  const { merchant } = checkout
  const partial = checkout.status === "detected" || checkout.status === "underpaid"
  const closed = checkout.status === "expired" || checkout.status === "failed" || checkout.status === "underpaid"
  const percent = ratioPercent(checkout.amount_received, checkout.amount, checkout.decimals)

  return (
    <section
      aria-label="Order summary"
      className="flex flex-col border-b border-border bg-background/70 p-5 sm:p-6 lg:border-r lg:border-b-0 lg:p-8 dark:bg-background/55"
    >
      <div className="flex items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <MerchantAvatar name={merchant.name} logoUrl={merchant.logo_url} className="size-7 text-xs" />
          <span className="truncate text-sm font-medium">{merchant.name}</span>
        </div>
        <Swap id={paneKey(checkout)}>
          {checkout.cancelled_at ? (
            <StatusBadge tone="neutral" hollow>
              Cancelled
            </StatusBadge>
          ) : checkout.status === "confirming" && checkout.awaiting_finality ? (
            // The payer's words: no counter stuck at 15/15, no "finalising".
            <StatusBadge tone="info" live={false} data-finalising="">
              {payerFinalising(checkout.merchant.name).badge}
            </StatusBadge>
          ) : (
            <PaymentStatusBadge
              status={checkout.status}
              confirmations={checkout.confirmations}
              required={checkout.required_confirmations}
              live={false}
            />
          )}
        </Swap>
      </div>

      <div className="mt-7 lg:mt-14">
        <h1 className="text-base leading-6 font-medium text-pretty break-words">{checkout.description || `Payment to ${merchant.name}`}</h1>
        {checkout.order_id && (
          <p className="mt-0.5 text-[13px] leading-5 text-muted-foreground">
            Order <span className="num break-all">{checkout.order_id}</span>
          </p>
        )}

        <Amount
          value={checkout.amount}
          asset={checkout.asset}
          className={cn(
            "mt-5 block text-[40px] leading-[44px] font-medium tracking-[-0.055em] lg:mt-7 lg:text-[46px] lg:leading-[52px] [&_[data-slot=amount-symbol]]:text-[0.45em] [&_[data-slot=amount-symbol]]:tracking-normal",
            // An amount with many decimals must stay on the card: let it wrap and step down a size.
            checkout.amount.length > 12 && "text-[26px] leading-8 whitespace-normal break-all lg:text-[28px] lg:leading-9",
            closed && "text-muted-foreground",
          )}
        />

        <div className="mt-3 flex flex-wrap items-center gap-2">
          <NetworkBadge asset chainId={checkout.chain_id} />
          {checkout.chain_id !== 56 && (
            <StatusBadge tone="warning" dot={false}>
              Testnet
            </StatusBadge>
          )}
        </div>

        {partial && (
          <div className="mt-5">
            <div
              className="h-1.5 overflow-hidden rounded-full bg-muted"
              role="progressbar"
              aria-label="Share of the amount received"
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={Math.round(percent)}
            >
              <div className="h-full rounded-full bg-warning" style={{ width: `${percent}%` }} />
            </div>
            <div className="mt-2 flex flex-wrap justify-between gap-x-4 text-[13px] leading-5 text-muted-foreground">
              <span>
                <Amount value={checkout.amount_received} className="text-foreground" /> received
              </span>
              <span>
                <Amount value={checkout.amount_due} className="text-foreground" /> {checkout.status === "detected" ? "to go" : "short"}
              </span>
            </div>
          </div>
        )}
      </div>

      <ProgressRail steps={railSteps(checkout)} className="mt-auto hidden pt-10 lg:flex" />
    </section>
  )
}

function railSteps(c: CheckoutData): RailStep[] {
  const created: RailStep = { label: "Payment created", detail: formatTime(c.created_at), state: "done" }
  const sent: RailStep = { label: "Payment sent", state: "done" }
  switch (c.status) {
    case "awaiting":
      return [
        created,
        { label: "Payment sent", detail: "waiting", state: "current", tone: "brand" },
        { label: "Payment confirmed", state: "todo" },
      ]
    case "detected":
      return [
        created,
        { label: "Payment sent", detail: "in part", state: "current", tone: "warning" },
        { label: "Payment confirmed", state: "todo" },
      ]
    case "confirming":
      return [
        created,
        sent,
        {
          label: "Payment confirmed",
          // Finalising: the counter would sit at 15/15; say the final check instead.
          detail: c.awaiting_finality ? "final check" : `${c.confirmations}/${c.required_confirmations}`,
          state: "current",
          tone: "info",
        },
      ]
    case "paid":
    case "overpaid":
      return [
        created,
        sent,
        { label: "Payment confirmed", detail: c.paid_at ? formatTime(c.paid_at) : undefined, state: "done", tone: "success" },
      ]
    case "underpaid":
      return [
        created,
        { label: "Sent in part", state: "done" },
        { label: "Closed short", detail: formatTime(c.expires_at), state: "stopped", tone: "warning" },
      ]
    case "expired":
      return [
        created,
        c.cancelled_at
          ? { label: "Cancelled", detail: formatTime(c.cancelled_at), state: "stopped", tone: "neutral" }
          : { label: "Expired, not paid", detail: formatTime(c.expires_at), state: "stopped", tone: "neutral" },
        { label: "Payment confirmed", state: "todo" },
      ]
    case "failed":
      return [created, sent, { label: "Dropped by the network", state: "stopped", tone: "danger" }]
  }
}

/* --------------------------------- panes ---------------------------------- */

interface PaneProps {
  checkout: CheckoutData
  mode: LiveMode
  /** Whether the page may ask for money right now (only read by the panes of an open payment). */
  invite: Invite
}

function StatePane(props: PaneProps) {
  switch (props.checkout.status) {
    case "awaiting":
      return <AwaitingPane {...props} />
    case "detected":
      return <TopUpPane {...props} />
    case "confirming":
      return props.checkout.awaiting_finality ? <FinalisingPane {...props} /> : <ConfirmingPane {...props} />
    case "paid":
    case "overpaid":
      return <PaidPane {...props} />
    case "underpaid":
      return <ShortPane {...props} />
    case "expired":
      return props.checkout.cancelled_at ? <CancelledPane {...props} /> : <ExpiredPane {...props} />
    case "failed":
      return <FailedPane {...props} />
  }
}

function PaneTitle({ children, className }: { children: React.ReactNode; className?: string }) {
  return <h2 className={cn("text-lg leading-7 font-semibold tracking-[-0.02em] text-balance", className)}>{children}</h2>
}

function PaneText({ children, className }: { children: React.ReactNode; className?: string }) {
  return <p className={cn("mt-1 text-sm leading-6 text-pretty text-muted-foreground", className)}>{children}</p>
}

/** Bottom strip of a pane: pinned to the bottom on desktop, follows the content on mobile. */
function PaneFooter({ children, className }: { children: React.ReactNode; className?: string }) {
  return (
    <div className={cn("mt-auto pt-6", className)}>
      <div className="flex items-center justify-between gap-4 border-t border-border pt-4">{children}</div>
    </div>
  )
}

function ReturnButton({
  checkout,
  children,
  variant = "default",
}: {
  checkout: CheckoutData
  children?: React.ReactNode
  variant?: "default" | "outline"
}) {
  // The return address was set by whoever created the payment: only a web address is followed.
  const href = safeHttpUrl(checkout.redirect_url)
  if (!href) return null
  return (
    <Button asChild variant={variant} className="h-11 w-full text-sm">
      <a href={href} rel="noopener noreferrer">
        {children ?? `Return to ${checkout.merchant.name}`}
        <ArrowRightIcon data-icon="inline-end" />
      </a>
    </Button>
  )
}

/** "write to billing@…" / "visit the help page" / "contact the merchant", whichever the merchant set up. */
function Contact({ merchant, lead }: { merchant: Merchant; lead: "Email" | "email" | "contact" | "Contact" }) {
  const { email, url } = supportContact(merchant)
  const link = "font-medium text-foreground underline underline-offset-4 break-all"
  const verb = lead[0] === lead[0].toUpperCase() ? "Contact" : "contact"
  if (email) {
    return (
      <>
        {lead.toLowerCase() === "email" ? lead : verb}{" "}
        <a className={link} href={`mailto:${email}`}>
          {email}
        </a>
      </>
    )
  }
  if (url) {
    return (
      <>
        {verb} {merchant.name} through{" "}
        <a className={link} href={url} target="_blank" rel="noopener noreferrer">
          their help page
        </a>
      </>
    )
  }
  return (
    <>
      {verb} {merchant.name}
    </>
  )
}

/**
 * The bottom of an open payment's pane: that the page is watching, or — when the server has not
 * answered for a while — that it is not, in so many words. The address above stays: it is still
 * the right one, and the server sees a payment whether or not this page does.
 */
function WatchFooter({ invite, mode, children }: { invite: Invite; mode: LiveMode; children: React.ReactNode }) {
  if (invite === "unwatched") {
    return (
      <div className="mt-auto pt-6" data-slot="live-unavailable">
        <Callout tone="warning" icon={<TriangleAlertIcon />} title="Live status is unavailable" role="status">
          This page cannot reach the server right now, so it will not show your payment arriving. A payment you send is still
          recorded. The page reconnects on its own.
        </Callout>
      </div>
    )
  }
  return (
    <PaneFooter>
      <LiveLine>{children}</LiveLine>
      <LiveNote mode={mode} />
    </PaneFooter>
  )
}

/**
 * The deadline has passed and the server has not said "expired" yet — or cannot be asked. The
 * page stops asking for money here, by its own clock: no address, no QR code, nothing to copy.
 * A transfer sent now would arrive after the deadline and not pay the order.
 *
 *  - "closing": the server answered after the deadline and still has the payment open. It is
 *    waiting, for about a minute, for a transfer that was sent in time to land.
 *  - "unknown": nobody has answered since the deadline. The payment has probably expired.
 *
 * The page goes on asking the server, and shows the outcome as soon as it has one.
 */
function DeadlinePane({ checkout, invite }: PaneProps) {
  const { merchant } = checkout
  const part = checkout.status === "detected"
  const offline = invite === "unknown"
  return (
    <div data-slot="deadline-passed" data-reason={offline ? "unreachable" : "checking"} className="flex flex-1 flex-col">
      <StateMark tone={offline ? "warning" : "neutral"}>{offline ? <TriangleAlertIcon /> : <ClockIcon />}</StateMark>
      <PaneTitle className="mt-4">{offline ? "This invoice has probably expired" : "The time to pay is up"}</PaneTitle>
      <PaneText>
        {offline
          ? "The time to pay has run out, and this page cannot reach the server to check what happened."
          : part
            ? "Part of the amount arrived. The server is checking whether the rest was sent in time."
            : "The server is checking whether a payment was sent in time."}
      </PaneText>
      <Callout tone="warning" icon={<TriangleAlertIcon />} title="Do not send anything now" className="mt-5">
        A transfer sent after the deadline does not pay this order. If you have already sent your payment, it is not lost: keep this
        page open{offline ? ", it will update when it reconnects" : " and it will update"}.
      </Callout>
      <p className="mt-4 text-[13px] leading-5 text-pretty text-muted-foreground">
        Need more time? <Contact merchant={merchant} lead="Contact" /> for a new invoice.
      </p>
      <PaneFooter>
        <LiveLine tone={offline ? "warning" : "brand"}>{offline ? "Not connected. Trying again" : "Checking with the server"}</LiveLine>
      </PaneFooter>
    </div>
  )
}

/** How the page is being kept current, when it is not the live stream. */
function LiveNote({ mode }: { mode: LiveMode }) {
  return (
    <span className="hidden text-[13px] leading-5 text-faint sm:block" data-live-note={mode}>
      {mode === "polling" ? "Checking every few seconds" : "This page updates on its own"}
    </span>
  )
}

/** Network + address + QR, with the one-line network rule. Shared by awaiting and underpaid. */
function SendInstructions({
  checkout,
  amount,
  amountLabel,
}: { checkout: CheckoutData; amount: string; amountLabel?: string }) {
  // The wallet link the API built for the amount still due; shown only when it is what it claims to be.
  const walletLink = safePaymentUri(checkout.payment_uri)
  const [justCopied, setJustCopied] = React.useState(false)
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null)
  React.useEffect(() => () => void (timer.current && clearTimeout(timer.current)), [])

  const handleCopied = () => {
    setJustCopied(true)
    if (timer.current) clearTimeout(timer.current)
    timer.current = setTimeout(() => setJustCopied(false), 5000)
  }

  return (
    <>
      <div className="flex gap-5">
        <div className="hidden flex-col items-center gap-2.5 lg:flex">
          <QrTile value={checkout.address} size={168} />
          <span className="text-xs leading-4 text-muted-foreground">Scan with your wallet</span>
        </div>
        <PayTo
          address={checkout.address}
          chainId={checkout.chain_id}
          amount={amount}
          amountLabel={amountLabel}
          amountNote="Exactly. Exchange fees go on top."
          onAddressCopied={handleCopied}
          className="min-w-0 flex-1 self-start"
        />
      </div>

      <div className="mt-4 grid text-[13px] leading-5" aria-live="polite">
        <p
          className={cn(
            "col-start-1 row-start-1 flex gap-2.5 text-muted-foreground transition-opacity duration-300",
            justCopied && "opacity-0",
          )}
          aria-hidden={justCopied}
        >
          <TriangleAlertIcon className="mt-0.5 size-4 shrink-0 text-warning-fg" aria-hidden="true" />
          <span className="text-pretty">
            Send only <span className="font-medium text-foreground">{checkout.asset}</span> on{" "}
            <span className="font-medium text-foreground">
              {networkName(checkout.chain_id)} ({NETWORK_STANDARD})
            </span>
            . Other networks or tokens can’t be recovered.
          </span>
        </p>
        <p
          className={cn(
            "col-start-1 row-start-1 flex gap-2.5 text-muted-foreground transition-opacity duration-300",
            !justCopied && "pointer-events-none opacity-0",
          )}
          aria-hidden={!justCopied}
        >
          <ShieldCheckIcon className="mt-0.5 size-4 shrink-0 text-brand-fg" aria-hidden="true" />
          <span className="text-pretty">
            Address copied. In your wallet, choose{" "}
            <span className="font-medium text-foreground">
              {networkName(checkout.chain_id)} ({NETWORK_STANDARD})
            </span>{" "}
            as the network.
          </span>
        </p>
      </div>

      <div className="mt-5 flex gap-2 lg:hidden">
        {walletLink && (
          <Button asChild className="h-11 flex-1 text-sm">
            <a href={walletLink}>
              <WalletIcon data-icon="inline-start" />
              Open in wallet
            </a>
          </Button>
        )}
        <Drawer>
          <DrawerTrigger asChild>
            <Button variant="outline" className={cn("h-11", walletLink ? "w-11" : "flex-1 gap-2 text-sm")} aria-label="Show QR code">
              <QrCodeIcon className="size-[18px]" />
              {!walletLink && "Show QR code"}
            </Button>
          </DrawerTrigger>
          <DrawerContent>
            <DrawerHeader className="text-center">
              <DrawerTitle>Scan to pay</DrawerTitle>
              <DrawerDescription>
                <Amount value={amount} asset={checkout.asset} className="text-foreground" /> on {networkName(checkout.chain_id)} ({NETWORK_STANDARD})
              </DrawerDescription>
            </DrawerHeader>
            <div className="flex flex-col items-center gap-5 px-4 pb-8">
              <QrTile value={checkout.address} size={232} />
              <AddressGroups value={checkout.address} />
            </div>
          </DrawerContent>
        </Drawer>
      </div>
    </>
  )
}

function AwaitingPane({ checkout, mode, invite }: PaneProps) {
  const dust = received(checkout)
  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
        <PaneTitle className="text-base leading-6">Send from your wallet</PaneTitle>
        <Countdown expiresAt={checkout.expires_at} createdAt={checkout.created_at} />
      </div>
      {dust && (
        <PaneText>
          <Amount value={checkout.amount_received} asset={checkout.asset} className="text-foreground" /> has arrived, which is too little
          to count. Send the amount below to pay.
        </PaneText>
      )}
      <div className="mt-5">
        <SendInstructions checkout={checkout} amount={checkout.amount_due} />
      </div>
      <WatchFooter invite={invite} mode={mode}>
        Watching for your payment
      </WatchFooter>
    </>
  )
}

/** Part of the amount arrived: the top-up. The payment completes when the rest lands in time. */
function TopUpPane({ checkout, mode, invite }: PaneProps) {
  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
        <PaneTitle className="text-base leading-6">Send the rest</PaneTitle>
        <Countdown expiresAt={checkout.expires_at} createdAt={checkout.created_at} label="Time left" />
      </div>
      <PaneText>Part of the payment arrived. Send the remaining amount to the same address to finish.</PaneText>

      <div className="mt-5">
        <SendInstructions checkout={checkout} amount={checkout.amount_due} amountLabel="Remaining amount" />
      </div>
      <WatchFooter invite={invite} mode={mode}>
        Watching for the remaining payment
      </WatchFooter>
    </>
  )
}

function ConfirmingPane({ checkout }: PaneProps) {
  const { merchant } = checkout
  return (
    <>
      <PaneTitle>Confirming your payment</PaneTitle>
      <PaneText>Your transaction is on-chain. The network is confirming it now.</PaneText>

      <div className="mt-7">
        <div className="flex items-baseline justify-between">
          <span className="text-[13px] leading-5 text-muted-foreground">Confirmations</span>
          <span className="num text-sm leading-5 text-muted-foreground" data-confirmations={checkout.confirmations}>
            <Roll value={String(checkout.confirmations)} className="text-base font-medium text-foreground" /> /{" "}
            {checkout.required_confirmations}
          </span>
        </div>
        <ConfirmationBlocks confirmations={checkout.confirmations} required={checkout.required_confirmations} className="mt-2.5" />
        <p className="mt-2.5 text-xs leading-4 text-faint">One confirmation per block.</p>
      </div>

      <DetailList className="mt-6 border-y border-border">
        <DetailRow label="Received">
          <Amount value={checkout.amount_received} asset={checkout.asset} className="font-medium" />
        </DetailRow>
        {checkout.tx_hash && (
          <DetailRow label="Transaction">
            <TxHash value={checkout.tx_hash} truncate={[8, 6]} chainId={checkout.chain_id} className="-mr-1.5 pointer-coarse:-mr-3" />
          </DetailRow>
        )}
      </DetailList>

      <Callout tone="neutral" icon={<ShieldCheckIcon />} title="You can close this page" className="mt-5">
        {merchant.name} is notified as soon as your payment is confirmed.
      </Callout>

      <PaneFooter>
        <LiveLine tone="info">Confirming on {networkName(checkout.chain_id)}</LiveLine>
      </PaneFooter>
    </>
  )
}

/**
 * All confirmations are in and the chain has not finalised the block yet: a calm wait, in the
 * payer's words — no block numbers, no counter at 15/15, never an error. After a couple of minutes
 * one sentence says it is taking longer than usual and that the payment is safe. The stream stays
 * live meanwhile (the payment is still confirming), so the page turns to "complete" by itself.
 */
function FinalisingPane({ checkout }: PaneProps) {
  const words = payerFinalising(checkout.merchant.name)
  const [since] = React.useState(() => Date.now())
  const now = useNow(5000)
  const longer = now - since >= FINALISING_PATIENCE_SECONDS * 1000
  return (
    <div data-slot="finalising" className="contents">
      <StateMark tone="info">
        <ShieldCheckIcon />
      </StateMark>
      <PaneTitle className="mt-5">{words.title}</PaneTitle>
      <PaneText>{words.text}</PaneText>

      <DetailList className="mt-6 border-y border-border">
        <DetailRow label="Received">
          <Amount value={checkout.amount_received} asset={checkout.asset} className="font-medium" />
        </DetailRow>
        {checkout.tx_hash && (
          <DetailRow label="Transaction">
            <TxHash value={checkout.tx_hash} truncate={[8, 6]} chainId={checkout.chain_id} className="-mr-1.5 pointer-coarse:-mr-3" />
          </DetailRow>
        )}
      </DetailList>

      {longer && (
        <Callout tone="neutral" icon={<ClockIcon />} className="mt-5" data-slot="finalising-longer">
          {words.longer}
        </Callout>
      )}

      <PaneFooter>
        <LiveLine tone="info">{words.line}</LiveLine>
      </PaneFooter>
    </div>
  )
}

function PaidPane({ checkout }: PaneProps) {
  const { merchant } = checkout
  const over = checkout.status === "overpaid"
  const extra = over ? subtractAmounts(checkout.amount_received, checkout.amount, checkout.decimals) : "0"
  return (
    <>
      <StateMark tone="success" solid className="ml-1.5 mt-1.5" data-wave-source="">
        <DrawnCheck />
      </StateMark>
      <PaneTitle className="mt-5 text-xl">Payment complete</PaneTitle>
      <PaneText>
        {merchant.name} has received your payment{over ? ", with a little extra." : ". Thank you."}
      </PaneText>

      <DetailList className="mt-6 border-y border-border">
        {over ? (
          <>
            <DetailRow label="Requested">
              <Amount value={checkout.amount} asset={checkout.asset} />
            </DetailRow>
            <DetailRow label="You sent">
              <Amount value={checkout.amount_received} asset={checkout.asset} className="font-medium" />
            </DetailRow>
          </>
        ) : (
          <DetailRow label="Amount paid">
            <Amount value={checkout.amount_received} asset={checkout.asset} className="font-medium" />
          </DetailRow>
        )}
        {checkout.paid_at && <DetailRow label="Paid">{formatDateTime(checkout.paid_at)}</DetailRow>}
        {checkout.order_id && (
          <DetailRow label="Order">
            <span className="num truncate">{checkout.order_id}</span>
          </DetailRow>
        )}
        {checkout.tx_hash && (
          <DetailRow label="Transaction">
            <TxHash value={checkout.tx_hash} truncate={[8, 6]} chainId={checkout.chain_id} className="-mr-1.5 pointer-coarse:-mr-3" />
          </DetailRow>
        )}
      </DetailList>

      {over && (
        <Callout
          tone="warning"
          icon={<TriangleAlertIcon />}
          className="mt-5"
          title={
            <>
              You sent <Amount value={extra} asset={checkout.asset} className="[&_[data-slot=amount-symbol]]:text-foreground" /> too much
            </>
          }
        >
          Your order is paid. To get the difference back, <Contact merchant={merchant} lead="contact" />.
        </Callout>
      )}

      <div className="mt-auto pt-6">
        <ReturnButton checkout={checkout} />
        <p className="mt-3 text-center text-xs leading-4 text-faint">The transaction hash is your proof of payment.</p>
      </div>
    </>
  )
}

/** The time ran out with part of the amount received. The money is with the merchant; the order is not paid. */
function ShortPane({ checkout }: PaneProps) {
  const { merchant } = checkout
  return (
    <>
      <StateMark tone="warning">
        <TriangleAlertIcon />
      </StateMark>
      <PaneTitle className="mt-5 text-xl">This payment closed short</PaneTitle>
      <PaneText>
        The time ran out before the full amount arrived, so the order was not paid. Please don’t send more to this address.
      </PaneText>

      <DetailList className="mt-6 border-y border-border">
        <DetailRow label="Requested">
          <Amount value={checkout.amount} asset={checkout.asset} />
        </DetailRow>
        <DetailRow label="Received">
          <Amount value={checkout.amount_received} asset={checkout.asset} className="font-medium" />
        </DetailRow>
        {checkout.tx_hash && (
          <DetailRow label="Transaction">
            <TxHash value={checkout.tx_hash} truncate={[8, 6]} chainId={checkout.chain_id} className="-mr-1.5 pointer-coarse:-mr-3" />
          </DetailRow>
        )}
      </DetailList>

      <Callout tone="neutral" icon={<CircleHelpIcon />} title="What you sent is not lost" className="mt-5">
        {merchant.name} has it. To complete the order or get it back, <Contact merchant={merchant} lead="contact" />
        {checkout.order_id && (
          <>
            {" "}
            and mention order <span className="num text-foreground">{checkout.order_id}</span>
          </>
        )}
        .
      </Callout>

      <div className="mt-auto pt-6">
        <ReturnButton checkout={checkout} variant="outline" />
      </div>
    </>
  )
}

/** Money on the address of a payment that had already closed: said plainly, with what to do. */
function StrayFunds({ checkout }: { checkout: CheckoutData }) {
  const { merchant } = checkout
  return (
    <Callout
      tone="warning"
      icon={<TriangleAlertIcon />}
      className="mt-6"
      title={
        <>
          <Amount value={checkout.amount_received} asset={checkout.asset} className="[&_[data-slot=amount-symbol]]:text-foreground" /> reached
          this address
        </>
      }
    >
      It did not pay this order: it arrived too late, or was too little to count. It isn’t lost. <Contact merchant={merchant} lead="Contact" />
      {checkout.tx_hash ? " with your transaction hash" : ""}
      {checkout.order_id && (
        <>
          {" "}
          and order <span className="num text-foreground">{checkout.order_id}</span>
        </>
      )}
      , and {merchant.name} will sort it out by hand.
    </Callout>
  )
}

function ExpiredPane({ checkout }: PaneProps) {
  const { merchant } = checkout
  return (
    <>
      <StateMark tone="neutral">
        <ClockIcon />
      </StateMark>
      <PaneTitle className="mt-5 text-xl">This payment has expired</PaneTitle>
      <PaneText>It wasn’t paid before {formatDateTime(checkout.expires_at)}. Please don’t send funds to its address.</PaneText>

      {received(checkout) ? (
        <StrayFunds checkout={checkout} />
      ) : (
        <Callout tone="neutral" icon={<CircleHelpIcon />} title="Already sent the payment?" className="mt-6">
          It isn’t lost. <Contact merchant={merchant} lead="Contact" /> with your transaction hash
          {checkout.order_id && (
            <>
              {" "}
              and order <span className="num text-foreground">{checkout.order_id}</span>
            </>
          )}
          , and {merchant.name} will match it by hand.
        </Callout>
      )}

      <div className="mt-auto pt-6">
        <ReturnButton checkout={checkout}>Start a new payment</ReturnButton>
      </div>
    </>
  )
}

function CancelledPane({ checkout }: PaneProps) {
  const { merchant } = checkout
  return (
    <>
      <StateMark tone="neutral">
        <BanIcon />
      </StateMark>
      <PaneTitle className="mt-5 text-xl">This payment was cancelled</PaneTitle>
      <PaneText>
        {merchant.name} cancelled it{checkout.cancelled_at ? ` on ${formatDateTime(checkout.cancelled_at)}` : ""}. Please don’t send funds to
        its address.
      </PaneText>

      {received(checkout) ? (
        <StrayFunds checkout={checkout} />
      ) : (
        <Callout tone="neutral" icon={<CircleHelpIcon />} title="Sent it anyway?" className="mt-6">
          It isn’t lost. <Contact merchant={merchant} lead="Contact" /> with your transaction hash, and {merchant.name} will refund or honour
          it.
        </Callout>
      )}

      <div className="mt-auto pt-6">
        <ReturnButton checkout={checkout}>Return to {merchant.name}</ReturnButton>
      </div>
    </>
  )
}

function FailedPane({ checkout }: PaneProps) {
  const { merchant } = checkout
  return (
    <>
      <StateMark tone="danger">
        <XIcon />
      </StateMark>
      <PaneTitle className="mt-5 text-xl">Payment didn’t go through</PaneTitle>
      <PaneText>
        Your transfer was seen, but the network dropped the block it was in and it did not come back before the time ran out.
      </PaneText>

      <DetailList className="mt-6 border-y border-border">
        {checkout.tx_hash && (
          <DetailRow label="Transaction">
            <TxHash value={checkout.tx_hash} truncate={[8, 6]} chainId={checkout.chain_id} className="-mr-1.5 pointer-coarse:-mr-3" />
          </DetailRow>
        )}
        {checkout.order_id && (
          <DetailRow label="Order">
            <span className="num truncate">{checkout.order_id}</span>
          </DetailRow>
        )}
      </DetailList>

      <Callout tone="neutral" icon={<CircleHelpIcon />} title="Check your wallet" className="mt-5">
        If the funds left your wallet after all, they are not lost: <Contact merchant={merchant} lead="contact" /> with your transaction
        hash. Otherwise, start a new payment.
      </Callout>

      <div className="mt-auto pt-6">
        <ReturnButton checkout={checkout}>Try again</ReturnButton>
      </div>
    </>
  )
}

/* ------------------------- loading / missing / error ----------------------- */

/** The card's frame for the states that have no payment to show. Same size, so nothing jumps. */
function PlainCard({ children, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      className="flex min-h-[420px] flex-col items-center justify-center overflow-hidden rounded-2xl border border-border bg-card p-8 text-center shadow-raised lg:min-h-[500px]"
      {...props}
    >
      {children}
    </div>
  )
}

function LoadingCard() {
  return (
    <div
      role="status"
      aria-label="Loading payment"
      data-pane="loading"
      className="overflow-hidden rounded-2xl border border-border bg-card shadow-raised lg:grid lg:min-h-[500px] lg:grid-cols-[400px_minmax(0,1fr)]"
    >
      <div className="space-y-5 border-b border-border bg-background/70 p-5 sm:p-6 lg:border-r lg:border-b-0 lg:p-8 dark:bg-background/55">
        <div className="flex items-center gap-2.5">
          <Skeleton className="size-7 rounded-md" />
          <Skeleton className="h-4 w-32" />
        </div>
        <Skeleton className="mt-7 h-4 w-44 lg:mt-14" />
        <Skeleton className="h-11 w-56" />
        <Skeleton className="h-7 w-52 rounded-full" />
      </div>
      <div className="space-y-5 p-5 sm:p-6 lg:p-8">
        <Skeleton className="h-5 w-48" />
        <Skeleton className="h-44 w-full rounded-xl" />
        <Skeleton className="h-4 w-full" />
      </div>
    </div>
  )
}

function NotFoundCard() {
  return (
    <PlainCard data-pane="not-found">
      <StateMark tone="neutral">
        <SearchXIcon />
      </StateMark>
      <PaneTitle className="mt-5 text-xl">This payment doesn’t exist</PaneTitle>
      <PaneText className="max-w-sm">
        The link may have been cut short, or it belongs to another site. Check the link you were given, or ask the seller for a new
        one.
      </PaneText>
    </PlainCard>
  )
}

function ErrorCard({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <PlainCard role="alert" data-pane="error">
      <StateMark tone="warning">
        <TriangleAlertIcon />
      </StateMark>
      <PaneTitle className="mt-5 text-xl">Couldn’t load this payment</PaneTitle>
      <PaneText className="max-w-sm">{message} Nothing has happened to your money.</PaneText>
      <Button variant="outline" className="mt-6 h-11 px-5" onClick={onRetry}>
        Try again
      </Button>
    </PlainCard>
  )
}

/* -------------------------------- footer ---------------------------------- */

/**
 * Under the card in every state: the attribution on the left, help and the sound and theme
 * toggles on the right. The attribution is always rendered, whatever was or was not loaded.
 */
function CheckoutFooter({ checkout }: { checkout: CheckoutData | undefined }) {
  const contact = checkout ? supportContact(checkout.merchant) : { email: null, url: null }
  const subject = checkout?.order_id ? `?subject=${encodeURIComponent(`Order ${checkout.order_id}`)}` : ""
  const help = contact.email ? `mailto:${contact.email}${subject}` : contact.url
  return (
    <footer className="mt-3 flex items-center justify-between gap-4 pr-0.5 pl-2 lg:mt-4">
      <PoweredBy />
      <div className="flex items-center gap-1">
        {help && (
          <Button asChild variant="ghost" className="h-9 px-2.5 text-[13px] font-normal text-muted-foreground hover:text-foreground pointer-coarse:h-11 pointer-coarse:px-3">
            <a href={help} {...(contact.email ? {} : { target: "_blank", rel: "noopener noreferrer" })}>
              Help
            </a>
          </Button>
        )}
        <SoundToggle className="size-9 pointer-coarse:size-11" />
        <ThemeToggle className="size-9 pointer-coarse:size-11" />
      </div>
    </footer>
  )
}
