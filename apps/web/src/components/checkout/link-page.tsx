// SPDX-License-Identifier: GPL-3.0-or-later
// hmnpay — Copyright (C) 2026 lubluniky and the hmnpay contributors. Distributed under the GNU GPL,
// version 3 or later (LICENSE), with additional terms under its section 7 (ADDITIONAL_TERMS.md):
// the "Powered by hmnpay" author attribution on payment pages must be preserved.
"use client"

import * as React from "react"
import { useRouter } from "next/navigation"
import { ArrowRightIcon, BanIcon, ClockIcon, HourglassIcon, SearchXIcon, TicketXIcon, TriangleAlertIcon } from "lucide-react"

import { Amount, AssetIcon, Callout, MerchantAvatar, NetworkBadge, PoweredBy, StatusBadge, ThemeToggle } from "@/components/hmn"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { api, idempotencyKey, isApiError } from "@/lib/api/client"
import type { LinkCheckout, LinkStatus } from "@/lib/api/types"
import { useQuery } from "@/lib/api/use-query"
import { CHAIN_NOT_READY_PAYER, chainNotReadyDelay, PAYER_PATIENCE_SECONDS, payerUnavailable } from "@/lib/chain-ready"
import { describeError, fieldErrors, isChainNotReady, isClockBehindError, isNetworkInvalidError, isNotFound } from "@/lib/errors"
import { AMOUNT_FORMAT_HINT, amountInputMessage, compareAmounts, displayAmount, formatUnits, parseAmountInput, toUnits } from "@/lib/money"
import { safeEmail, safeHttpUrl } from "@/lib/url"
import { isLinkSlug, isPaymentId, usePathSegment } from "@/lib/use-route"
import { cn } from "@/lib/utils"

import { StateMark } from "./parts"

/**
 * The public page of a payment link (/l/{slug}): the customer sees what the link is for, enters
 * an amount if the link lets them choose one, and starts a payment. Starting one creates a
 * checkout (POST /api/v1/public/links/{slug}/payments) and goes to its page.
 *
 * A link that cannot take a payment says why — switched off, past its end date, or used up —
 * and what the server refuses when the button is pressed (the last place was just taken, the
 * amount is below the minimum, too many attempts) is shown as what it is.
 *
 * Every state carries the "Powered by hmnpay" attribution in the footer.
 */
export function LinkPage() {
  const slug = usePathSegment(1)
  const valid = slug !== null && isLinkSlug(slug)
  const link = useQuery(valid ? `public/links/${slug}` : null, (signal) => api.publicLink(slug!, signal))

  React.useEffect(() => {
    const name = link.data?.merchant.name
    if (name) document.title = `Pay ${name}`
  }, [link.data?.merchant.name])

  let body: React.ReactNode
  if (slug !== null && (!valid || (isNotFound(link.error) && !link.data))) body = <NotFoundCard />
  else if (link.data) body = <LinkCard link={link.data} onChanged={link.reload} />
  else if (link.error) body = <ErrorCard message={describeError(link.error)} onRetry={link.reload} />
  else body = <LoadingCard />

  return (
    <div data-link-page="" data-status={link.data?.status} className="flex min-h-svh flex-col items-center px-3 pt-3 pb-6 sm:px-6 sm:pt-10 lg:justify-center lg:py-12">
      <div className="w-full max-w-[460px] lg:max-w-[960px]">
        <main>{body}</main>
        <footer className="mt-3 flex items-center justify-between gap-4 pr-0.5 pl-2 lg:mt-4">
          <PoweredBy />
          <div className="flex items-center gap-1">
            <HelpLink link={link.data} />
            <ThemeToggle className="size-9 pointer-coarse:size-11" />
          </div>
        </footer>
      </div>
    </div>
  )
}

function HelpLink({ link }: { link: LinkCheckout | undefined }) {
  if (!link) return null
  const email = safeEmail(link.merchant.support_email)
  const url = safeHttpUrl(link.merchant.support_url)
  const href = email ? `mailto:${email}?subject=${encodeURIComponent(link.name)}` : url
  if (!href) return null
  return (
    <Button asChild variant="ghost" className="h-9 px-2.5 text-[13px] font-normal text-muted-foreground hover:text-foreground pointer-coarse:h-11 pointer-coarse:px-3">
      <a href={href} {...(email ? {} : { target: "_blank", rel: "noopener noreferrer" })}>
        Help
      </a>
    </Button>
  )
}

/** " Contact <merchant>." when the merchant gave a way to reach them; nothing otherwise. */
function ContactLink({ link }: { link: LinkCheckout }) {
  const email = safeEmail(link.merchant.support_email)
  const url = safeHttpUrl(link.merchant.support_url)
  const href = email ? `mailto:${email}?subject=${encodeURIComponent(link.name)}` : url
  if (!href) return null
  return (
    <>
      {" "}
      <a
        href={href}
        className="font-medium text-foreground underline underline-offset-4"
        data-slot="contact-merchant"
        {...(email ? {} : { target: "_blank", rel: "noopener noreferrer" })}
      >
        Contact {link.merchant.name}
      </a>
      .
    </>
  )
}

const CARD = "overflow-hidden rounded-2xl border border-border bg-card shadow-raised lg:grid lg:min-h-[420px] lg:grid-cols-[400px_minmax(0,1fr)]"

function LinkCard({ link, onChanged }: { link: LinkCheckout; onChanged: () => void }) {
  const { merchant } = link
  return (
    <div className={CARD}>
      <section
        aria-label="What you are paying for"
        className="flex flex-col border-b border-border bg-background/70 p-5 sm:p-6 lg:border-r lg:border-b-0 lg:p-8 dark:bg-background/55"
      >
        <div className="flex min-w-0 items-center gap-2.5">
          <MerchantAvatar name={merchant.name} logoUrl={merchant.logo_url} className="size-7 text-xs" />
          <span className="truncate text-sm font-medium">{merchant.name}</span>
        </div>

        <div className="mt-7 lg:mt-14">
          <h1 className="text-base leading-6 font-medium text-pretty break-words">{link.name}</h1>
          {link.description && <p className="mt-1 text-[13px] leading-5 text-pretty break-words whitespace-pre-line text-muted-foreground">{link.description}</p>}

          {link.amount ? (
            <Amount
              value={link.amount}
              asset={link.asset}
              className={cn(
                "mt-5 block text-[40px] leading-[44px] font-medium tracking-[-0.055em] lg:mt-7 lg:text-[46px] lg:leading-[52px] [&_[data-slot=amount-symbol]]:text-[0.45em] [&_[data-slot=amount-symbol]]:tracking-normal",
                link.amount.length > 12 && "text-[26px] leading-8 whitespace-normal break-all lg:text-[28px] lg:leading-9",
                link.status !== "active" && "text-muted-foreground",
              )}
            />
          ) : (
            <p className="mt-5 text-[26px] leading-8 font-medium tracking-[-0.03em] lg:mt-7">You choose the amount</p>
          )}

          <NetworkBadge asset className="mt-3" />
        </div>
      </section>

      <div className="flex min-w-0 flex-col p-5 sm:p-6 lg:p-8" data-pane={link.status}>
        {link.status === "active" ? <StartForm link={link} onChanged={onChanged} /> : <Unavailable status={link.status} merchant={merchant.name} />}
      </div>
    </div>
  )
}

const UNAVAILABLE: Record<Exclude<LinkStatus, "active">, { icon: React.ReactNode; title: string; text: (merchant: string) => string; badge: string }> = {
  disabled: {
    icon: <BanIcon />,
    title: "This link is no longer available",
    text: (merchant) => `${merchant} switched it off. Ask them for a new link if you still want to pay.`,
    badge: "Switched off",
  },
  expired: {
    icon: <ClockIcon />,
    title: "This link has expired",
    text: (merchant) => `It stopped taking payments on its end date. Ask ${merchant} for a new link if you still want to pay.`,
    badge: "Expired",
  },
  limit: {
    icon: <TicketXIcon />,
    title: "This link has been used up",
    text: (merchant) =>
      `Every payment ${merchant} allowed through it has been made or is being made right now. A place is released if someone does not finish within 10 minutes, so it can be worth trying again a little later.`,
    badge: "Used up",
  },
}

function Unavailable({ status, merchant }: { status: Exclude<LinkStatus, "active">; merchant: string }) {
  const meta = UNAVAILABLE[status]
  return (
    <>
      <div className="flex items-center justify-between gap-3">
        <StateMark tone="neutral">{meta.icon}</StateMark>
        <StatusBadge tone="neutral" hollow>
          {meta.badge}
        </StatusBadge>
      </div>
      <h2 className="mt-5 text-xl leading-7 font-semibold tracking-[-0.02em] text-balance">{meta.title}</h2>
      <p className="mt-1 text-sm leading-6 text-pretty text-muted-foreground">{meta.text(merchant)}</p>
    </>
  )
}

function StartForm({ link, onChanged }: { link: LinkCheckout; onChanged: () => void }) {
  const router = useRouter()
  const open = link.amount === null
  const minimum = link.min_amount
  const [amount, setAmount] = React.useState("")
  const [email, setEmail] = React.useState("")
  const [submitted, setSubmitted] = React.useState(false)
  const [busy, setBusy] = React.useState(false)
  const [server, setServer] = React.useState<Record<string, string>>({})
  const [refusal, setRefusal] = React.useState<string | null>(null)
  // One key per thing the customer asked for: pressing twice does not open two checkouts.
  const [key, setKey] = React.useState(idempotencyKey)
  // The server cannot hand out an address yet (its chain watcher has not read its first block:
  // seconds, normally). The customer is not shown an error: the page waits and asks again by
  // itself, with the same key, until it can go on. `waiting` counts the refusals so far.
  const [waiting, setWaiting] = React.useState(0)
  const retry = React.useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const attempts = React.useRef(0) // read by the timer's call, which a render's state would not reach
  // The state can last (a server clock that is wrong, an endpoint that is down): after a while of
  // trying, the page says so in words a payer can act on, and keeps trying.
  const [patienceOver, setPatienceOver] = React.useState(false)
  const patience = React.useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const stopWaiting = () => {
    clearTimeout(retry.current)
    clearTimeout(patience.current)
    retry.current = undefined
    patience.current = undefined
    attempts.current = 0
    setWaiting(0)
    setPatienceOver(false)
  }
  React.useEffect(
    () => () => {
      clearTimeout(retry.current)
      clearTimeout(patience.current)
    },
    [],
  )
  const edited = () => {
    if (retry.current) {
      stopWaiting()
      setBusy(false)
    }
    setServer({})
    setRefusal(null)
    setKey(idempotencyKey())
  }

  const parsed = open ? parseAmountInput(amount, link.decimals) : null
  // What is wrong with the amount is said as soon as it is typed, not only on submit: the field
  // keeps the customer's own text, so a comma or a space has to be answered where it stands.
  let amountError: string | undefined
  if (open) {
    if (server.amount && submitted) amountError = server.amount
    else if (!parsed?.ok) {
      if (parsed && (parsed.reason !== "empty" || submitted)) {
        amountError = parsed.reason === "empty" ? "Enter the amount you want to pay." : amountInputMessage(parsed.reason, link.decimals)
      }
    } else if (submitted && minimum && compareAmounts(parsed.amount, minimum, link.decimals) < 0) {
      amountError = `The minimum is ${displayAmount(minimum)} ${link.asset}.`
    }
  }
  // Exactly what will be asked for, as the payment will state it: shown on the button before
  // anything is created.
  const exact = open ? (parsed?.ok ? formatUnits(toUnits(parsed.amount, link.decimals), link.decimals) : null) : link.amount
  const emailError = submitted ? (server.customer_email ?? (email.trim() && !safeEmail(email.trim()) ? "That doesn’t look like an email address." : undefined)) : undefined

  const start = async (again = false) => {
    setSubmitted(true)
    if (busy && !again) return
    if (open) {
      if (!parsed?.ok) return
      if (minimum && compareAmounts(parsed.amount, minimum, link.decimals) < 0) return
    }
    if (email.trim() && !safeEmail(email.trim())) return
    setBusy(true)
    setRefusal(null)
    try {
      const checkout = await api.createLinkPayment(
        link.slug,
        { ...(open && parsed?.ok ? { amount: parsed.amount } : {}), ...(email.trim() ? { customer_email: email.trim() } : {}) },
        key,
      )
      // The id comes from the API: it becomes part of an address only if it is a payment id.
      if (!isPaymentId(checkout.id)) throw new Error("the server answered with something that is not a payment id")
      stopWaiting()
      router.push(`/pay/${checkout.id}`)
      // Stay "busy" while the checkout page loads: the button must not offer a second payment.
    } catch (error) {
      if (isChainNotReady(error) || isClockBehindError(error) || isNetworkInvalidError(error) || (attempts.current > 0 && isApiError(error) && (error.status === 429 || error.status === 0))) {
        // Not able to take payments yet (or, while waiting, slowed down or briefly offline): wait
        // and ask again, with the same key.
        const attempt = ++attempts.current
        setWaiting(attempt)
        if (attempt === 1) patience.current = setTimeout(() => setPatienceOver(true), PAYER_PATIENCE_SECONDS * 1000)
        clearTimeout(retry.current)
        retry.current = setTimeout(() => void start(true), chainNotReadyDelay(attempt, isApiError(error) ? error.retryAfter : undefined) * 1000)
        return
      }
      stopWaiting()
      setBusy(false)
      const fields = fieldErrors(error)
      setServer(fields)
      if (isApiError(error) && ["link_limit_reached", "link_disabled", "link_expired"].includes(error.code)) {
        // The link changed under the customer (the last place was just taken): show what it is now.
        onChanged()
        setRefusal(describeError(error))
      } else if (isApiError(error) && error.status === 429) {
        setRefusal(
          `Too many payments were started from your network just now. Try again in ${
            error.retryAfter && error.retryAfter >= 90 ? `${Math.ceil(error.retryAfter / 60)} minutes` : `${error.retryAfter ?? 60} seconds`
          }.`,
        )
      } else if (!("amount" in fields) && !("customer_email" in fields)) {
        setRefusal(describeError(error))
      }
    }
  }

  return (
    <form
      noValidate
      className="flex flex-1 flex-col"
      onSubmit={(event) => {
        event.preventDefault()
        void start()
      }}
    >
      {waiting > 0 ? (
        // The page is waiting for the server to take payments again: the headline, the words and
        // the button all say so, and nothing else on the page says otherwise.
        <div role="status" data-slot={patienceOver ? "payments-paused" : "chain-not-ready"}>
          <h2 className="flex items-center gap-2 text-lg leading-7 font-semibold tracking-[-0.02em] text-balance">
            <HourglassIcon aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />
            {patienceOver ? payerUnavailable(link.merchant.name).title : CHAIN_NOT_READY_PAYER.title}
          </h2>
          <p className="mt-1 text-sm leading-6 text-pretty text-muted-foreground">
            {patienceOver ? (
              <>
                {payerUnavailable(link.merchant.name).text}
                <ContactLink link={link} />
              </>
            ) : (
              CHAIN_NOT_READY_PAYER.text
            )}
          </p>
        </div>
      ) : (
        <>
          <h2 className="text-lg leading-7 font-semibold tracking-[-0.02em] text-balance">{open ? "How much do you want to pay?" : "Ready when you are"}</h2>
          <p className="mt-1 text-sm leading-6 text-pretty text-muted-foreground">
            {open
              ? `Enter the amount, then you get an address to send ${link.asset} to from your wallet.`
              : `You get an address to send ${link.asset} to from your wallet. Nothing is charged until you send it.`}
          </p>
        </>
      )}

      {open && (
        <div className="mt-5 flex flex-col gap-1.5">
          <Label htmlFor="lp-amount" className="text-[13px] leading-5 font-medium">
            Amount
          </Label>
          <div
            className={cn(
              "flex h-12 items-center gap-2.5 rounded-lg border border-input px-3 transition-colors dark:bg-input/30",
              "focus-within:border-ring focus-within:ring-3 focus-within:ring-ring/50",
              amountError && "border-destructive ring-3 ring-destructive/20",
            )}
          >
            <AssetIcon size={22} />
            <input
              id="lp-amount"
              inputMode="decimal"
              autoComplete="off"
              placeholder="0.00"
              value={amount}
              maxLength={40}
              onChange={(event) => {
                // Nothing is rewritten: what was typed is what is judged.
                setAmount(event.target.value)
                edited()
              }}
              aria-invalid={amountError ? true : undefined}
              aria-describedby={amountError ? "lp-amount-error" : "lp-amount-hint"}
              className="num h-full min-w-0 flex-1 bg-transparent text-xl font-medium outline-none placeholder:text-faint"
            />
            <span className="num text-sm text-muted-foreground">{link.asset}</span>
          </div>
          {amountError ? (
            <p id="lp-amount-error" role="alert" className="text-[13px] leading-5 text-danger-fg">
              {amountError}
            </p>
          ) : (
            <p id="lp-amount-hint" className="text-[13px] leading-5 text-muted-foreground">
              {AMOUNT_FORMAT_HINT}
              {minimum && (
                <>
                  {" "}
                  Minimum <Amount value={minimum} asset={link.asset} className="text-foreground" />.
                </>
              )}
            </p>
          )}
        </div>
      )}

      <div className="mt-4 flex flex-col gap-1.5">
        <Label htmlFor="lp-email" className="text-[13px] leading-5 font-medium">
          Email
          <span className="font-normal text-faint">Optional</span>
        </Label>
        <Input
          id="lp-email"
          type="email"
          autoComplete="email"
          placeholder="you@example.com"
          value={email}
          onChange={(event) => {
            setEmail(event.target.value)
            edited()
          }}
          aria-invalid={emailError ? true : undefined}
          className="h-10"
        />
        {emailError ? (
          <p role="alert" className="text-[13px] leading-5 text-danger-fg">
            {emailError}
          </p>
        ) : (
          <p className="text-[13px] leading-5 text-muted-foreground">Shared with {link.merchant.name}, so they know who paid.</p>
        )}
      </div>

      {refusal && (
        <Callout tone="warning" icon={<TriangleAlertIcon />} className="mt-5" role="alert">
          {refusal}
        </Callout>
      )}

      <div className="mt-auto pt-6">
        <Button type="submit" className="h-11 w-full text-sm" disabled={busy}>
          {waiting > 0 ? (
            patienceOver ? payerUnavailable(link.merchant.name).button : CHAIN_NOT_READY_PAYER.button
          ) : busy ? (
            "Opening your payment…"
          ) : exact === null ? (
            "Continue to payment"
          ) : open ? (
            // The customer's own amount, digit for digit and without grouping: this is the
            // moment to notice that 1.25 was not what they meant.
            <span data-slot="pay-exact">
              Pay <span className="num">{exact}</span> {link.asset}
            </span>
          ) : (
            <>
              Pay <Amount value={exact} asset={link.asset} className="[&_[data-slot=amount-symbol]]:text-current" />
            </>
          )}
          {!busy && <ArrowRightIcon data-icon="inline-end" />}
        </Button>
      </div>
    </form>
  )
}

/* ------------------------- loading / missing / error ----------------------- */

function PlainCard({ children, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      className="flex min-h-[360px] flex-col items-center justify-center overflow-hidden rounded-2xl border border-border bg-card p-8 text-center shadow-raised lg:min-h-[420px]"
      {...props}
    >
      {children}
    </div>
  )
}

function LoadingCard() {
  return (
    <div role="status" aria-label="Loading" data-pane="loading" className={CARD}>
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
        <Skeleton className="h-12 w-full rounded-lg" />
        <Skeleton className="h-11 w-full rounded-lg" />
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
      <h2 className="mt-5 text-xl leading-7 font-semibold tracking-[-0.02em]">This payment link doesn’t exist</h2>
      <p className="mt-1 max-w-sm text-sm leading-6 text-pretty text-muted-foreground">
        It may have been deleted, or the link was cut short. Check the link you were given, or ask the seller for a new one.
      </p>
    </PlainCard>
  )
}

function ErrorCard({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <PlainCard role="alert" data-pane="error">
      <StateMark tone="warning">
        <TriangleAlertIcon />
      </StateMark>
      <h2 className="mt-5 text-xl leading-7 font-semibold tracking-[-0.02em]">Couldn’t load this link</h2>
      <p className="mt-1 max-w-sm text-sm leading-6 text-pretty text-muted-foreground">{message}</p>
      <Button variant="outline" className="mt-6 h-11 px-5" onClick={onRetry}>
        Try again
      </Button>
    </PlainCard>
  )
}
