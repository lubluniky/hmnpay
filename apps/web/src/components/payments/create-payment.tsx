"use client"

import * as React from "react"
import Link from "next/link"
import { usePathname, useRouter, useSearchParams } from "next/navigation"
import { ArrowUpRightIcon, ChevronRightIcon, HourglassIcon, PlusIcon, TerminalIcon, XIcon } from "lucide-react"

import { Countdown, QrTile } from "@/components/checkout/parts"
import { Amount, AssetIcon, Callout, CopyButton, NetworkBadge, PaymentStatusBadge, WaveTransition } from "@/components/hmn"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { useDashboard } from "@/components/shell/session"
import { api, idempotencyKey, type CreatePaymentInput } from "@/lib/api/client"
import type { Payment } from "@/lib/api/types"
import { CHAIN_NOT_READY_OPERATOR, CLOCK_BEHIND_OPERATOR, NETWORK_INVALID_OPERATOR } from "@/lib/chain-ready"
import { describeError, fieldErrors, isChainNotReady, isClockBehindError, isNetworkInvalidError } from "@/lib/errors"
import { AMOUNT_FORMAT_HINT, amountInputMessage, parseAmountInput } from "@/lib/money"
import { cn } from "@/lib/utils"

import { CodeSnippet } from "./code-snippet"
import { checkoutPath, checkoutUrl, formatDuration, paymentPath } from "./lib"

/* -------------------------------------------------------------------------- */
/* Trigger                                                                    */
/* -------------------------------------------------------------------------- */

/** Build the href that opens the create-payment dialog on a page that mounts `CreatePaymentDialog`. */
export function createPaymentHref(
  base: string,
  prefill: { amount?: string; order?: string; description?: string; email?: string } = {},
) {
  const params = new URLSearchParams({ new: "1" })
  for (const [key, value] of Object.entries(prefill)) if (value) params.set(key, value)
  return `${base}${base.includes("?") ? "&" : "?"}${params}`
}

/**
 * The "Create payment" action. Opens the dialog by adding `?new=1` to the current URL, so the
 * open state is linkable (⌘K uses the same param) and Back closes it.
 */
export function CreatePaymentButton({
  children = "Create payment",
  ...props
}: Omit<React.ComponentProps<typeof Button>, "onClick" | "asChild">) {
  const router = useRouter()
  const pathname = usePathname()
  return (
    <Button
      type="button"
      onClick={() => {
        const params = new URLSearchParams(window.location.search)
        params.set("new", "1")
        params.delete("created")
        router.push(`${pathname}?${params}`, { scroll: false })
      }}
      {...props}
    >
      <PlusIcon data-icon="inline-start" />
      {children}
    </Button>
  )
}

/* -------------------------------------------------------------------------- */
/* Dialog                                                                     */
/* -------------------------------------------------------------------------- */

const DIALOG_PARAMS = ["new", "api", "amount", "order", "description", "email"]

/** "default" sends no TTL: the payment takes the instance's payment window (Settings → General). */
const TTL_DEFAULT = "default"
const TTL_OPTIONS = [
  { value: "15", label: "15 minutes" },
  { value: "30", label: "30 minutes" },
  { value: "60", label: "1 hour" },
  { value: "1440", label: "24 hours" },
] as const

interface Draft {
  amount: string
  orderId: string
  description: string
  email: string
  ttl: string
  metadata: { key: string; value: string }[]
}

/**
 * Create-payment dialog. Mount once per page; it opens whenever the URL has `?new=1`.
 * `&api=1` opens with the API request expanded, and `amount` / `order` / `description` / `email`
 * prefill the form ("Create again").
 */
export function CreatePaymentDialog() {
  // Reads the URL's query, which a static export only knows in the browser.
  return (
    <React.Suspense>
      <CreatePaymentDialogInner />
    </React.Suspense>
  )
}

function CreatePaymentDialogInner() {
  const router = useRouter()
  const pathname = usePathname()
  const params = useSearchParams()
  const open = params.get("new") === "1"

  const close = () => {
    const next = new URLSearchParams(params)
    for (const key of DIALOG_PARAMS) next.delete(key)
    const text = next.toString()
    router.replace(text ? `${pathname}?${text}` : pathname, { scroll: false })
  }

  return (
    <Dialog open={open} onOpenChange={(value) => !value && close()}>
      <DialogContent
        showCloseButton={false}
        tabIndex={-1}
        className={cn(
          "flex max-h-[calc(100dvh-2rem)] flex-col gap-0 overflow-hidden p-0 shadow-overlay sm:max-w-[520px]",
          // Phones: a bottom sheet, full width, thumb-reachable actions.
          "max-sm:top-auto max-sm:bottom-0 max-sm:max-h-[calc(100dvh-1.5rem)] max-sm:max-w-full max-sm:translate-y-0 max-sm:rounded-b-none",
        )}
      >
        <CreatePaymentFlow
          initial={{
            api: params.get("api") === "1",
            amount: params.get("amount") ?? "",
            orderId: params.get("order") ?? "",
            description: params.get("description") ?? "",
            email: params.get("email") ?? "",
          }}
          onClose={close}
        />
      </DialogContent>
    </Dialog>
  )
}

function CreatePaymentFlow({
  initial,
  onClose,
}: {
  initial: { api: boolean; amount: string; orderId: string; description: string; email: string }
  onClose: () => void
}) {
  const { refreshCounts } = useDashboard()
  const [created, setCreated] = React.useState<Payment | null>(null)
  const [draft, setDraft] = React.useState<Draft>({
    amount: initial.amount,
    orderId: initial.orderId,
    description: initial.description,
    email: initial.email,
    ttl: TTL_DEFAULT,
    metadata: [],
  })

  // Creating the payment sends a wave up from the button; "Create another" just swaps back.
  return (
    <WaveTransition
      revealed={created !== null}
      cue="created"
      className="min-h-0 rounded-[inherit]"
      layerClassName="flex flex-col"
      revealedContent={
        created && (
          <CreatedView
            payment={created}
            onClose={onClose}
            onAnother={() => {
              setDraft((d) => ({ ...d, amount: "", orderId: "", description: "", email: "", metadata: [] }))
              setCreated(null)
            }}
          />
        )
      }
    >
      <FormView
        draft={draft}
        setDraft={setDraft}
        initialApi={initial.api}
        onClose={onClose}
        onCreate={(payment) => {
          setCreated(payment)
          refreshCounts()
        }}
      />
    </WaveTransition>
  )
}

/* ------------------------------- Form ------------------------------------- */

/** The request body for a draft, exactly as the form sends it. `amount` stays a string end to end. */
function toRequest(draft: Draft, amount: string): CreatePaymentInput {
  const body: CreatePaymentInput = { amount }
  if (draft.orderId.trim()) body.order_id = draft.orderId.trim()
  if (draft.description.trim()) body.description = draft.description.trim()
  if (draft.email.trim()) body.customer_email = draft.email.trim()
  if (draft.ttl !== TTL_DEFAULT) body.ttl_seconds = Number(draft.ttl) * 60
  const metadata = Object.fromEntries(draft.metadata.filter((m) => m.key.trim()).map((m) => [m.key.trim(), m.value]))
  if (Object.keys(metadata).length) body.metadata = metadata
  return body
}

function buildCurl(draft: Draft, amount: string, baseUrl: string) {
  const json = JSON.stringify(toRequest(draft, amount), null, 2)
    .split("\n")
    .map((line, i) => (i === 0 ? line : `  ${line}`))
    .join("\n")
    // The body sits inside single quotes in the shell command.
    .replaceAll("'", `'\\''`)
  return [
    `curl ${baseUrl}/api/v1/payments \\`,
    `  -H "Authorization: Bearer $HMNPAY_API_KEY" \\`,
    `  -H "Content-Type: application/json" \\`,
    `  -H "Idempotency-Key: $(uuidgen)" \\`,
    `  -d '${json}'`,
  ].join("\n")
}

function FieldLabel({ htmlFor, optional, children }: { htmlFor: string; optional?: boolean; children: React.ReactNode }) {
  return (
    <Label htmlFor={htmlFor} className="text-[13px] leading-5 font-medium">
      {children}
      {optional && <span className="font-normal text-faint">Optional</span>}
    </Label>
  )
}

function FormView({
  draft,
  setDraft,
  initialApi,
  onClose,
  onCreate,
}: {
  draft: Draft
  setDraft: React.Dispatch<React.SetStateAction<Draft>>
  initialApi: boolean
  onClose: () => void
  onCreate: (payment: Payment) => void
}) {
  const { settings } = useDashboard()
  const decimals = settings.asset.decimals
  const [showApi, setShowApi] = React.useState(initialApi)
  const [submitted, setSubmitted] = React.useState(false)
  const [sending, setSending] = React.useState(false)
  const [serverErrors, setServerErrors] = React.useState<Record<string, string>>({})
  const [failure, setFailure] = React.useState<string | null>(null)
  // The server cannot create payments at the moment — its chain watcher has no first block yet
  // ("chain"), or its clock is behind the chain ("clock"): said in plain words, with the page that
  // shows why one click away.
  const [notReady, setNotReady] = React.useState<"chain" | "clock" | "network" | null>(null)
  // One key per attempt to create this payment: a retry after a lost answer returns the same payment.
  const [key, setKey] = React.useState(idempotencyKey)
  const set = <K extends keyof Draft>(name: K, value: Draft[K]) => {
    setDraft((d) => ({ ...d, [name]: value }))
    setServerErrors({})
    setFailure(null)
    setNotReady(null)
    setKey(idempotencyKey())
  }

  const parsed = parseAmountInput(draft.amount, decimals)
  // Said while typing when the text is not an amount (a comma, a space, a letter); "enter an
  // amount" and what the server refused only after the form was sent.
  const amountMessage =
    (submitted ? serverErrors.amount : undefined) ??
    (parsed.ok || (parsed.reason === "empty" && !submitted) ? undefined : amountInputMessage(parsed.reason, decimals))
  const amountError = amountMessage !== undefined
  const baseUrl = settings.public_url || (typeof window === "undefined" ? "" : window.location.origin)
  const curl = buildCurl(draft, parsed.ok ? parsed.amount : "0", baseUrl)
  const defaultTtl = formatDuration(settings.payments.ttl_seconds * 1000)

  const submit = async () => {
    setSubmitted(true)
    if (!parsed.ok || sending) return
    setSending(true)
    setFailure(null)
    setNotReady(null)
    try {
      const payment = await api.createPayment(toRequest(draft, parsed.amount), key)
      setKey(idempotencyKey())
      onCreate(payment)
    } catch (error) {
      const fields = fieldErrors(error, { order_id: "orderId", customer_email: "email", ttl_seconds: "ttl" })
      setServerErrors(fields)
      // A field error is shown under its field; anything else (and fields this form has no place for) up top.
      const placed = ["amount", "orderId", "description", "email", "ttl"].some((name) => name in fields)
      // Nothing was created, so the same key is kept: pressing again is the same request.
      if (isChainNotReady(error)) setNotReady("chain")
      else if (isClockBehindError(error)) setNotReady("clock")
      else if (isNetworkInvalidError(error)) setNotReady("network")
      else setFailure(placed ? null : describeError(error))
    } finally {
      setSending(false)
    }
  }

  return (
    <form
      noValidate
      className="flex min-h-0 flex-col"
      onSubmit={(event) => {
        event.preventDefault()
        void submit()
      }}
    >
      <header className="flex items-start justify-between gap-4 px-5 pt-5 pb-4">
        <div>
          <DialogTitle className="text-base leading-6 font-semibold tracking-[-0.015em]">Create payment</DialogTitle>
          <DialogDescription className="mt-0.5 text-[13px] leading-5">
            You get a checkout link to send to your customer.
          </DialogDescription>
        </div>
        <Button type="button" variant="ghost" size="icon-sm" onClick={onClose} className="-mt-1 -mr-1.5 text-muted-foreground">
          <XIcon />
          <span className="sr-only">Close</span>
        </Button>
      </header>

      <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-5 pb-5">
        {/* Amount: the one required field, and the one that must be right. */}
        <div className="flex flex-col gap-1.5">
          <div className="flex items-center justify-between gap-3">
            <FieldLabel htmlFor="cp-amount">Amount</FieldLabel>
            <NetworkBadge variant="inline" size="sm" chainId={settings.network.chain_id} className="font-normal text-muted-foreground" />
          </div>
          <div
            className={cn(
              "flex h-12 items-center gap-2.5 rounded-lg border border-input px-3 transition-colors dark:bg-input/30",
              "focus-within:border-ring focus-within:ring-3 focus-within:ring-ring/50",
              amountError && "border-destructive ring-3 ring-destructive/20",
            )}
          >
            <AssetIcon size={22} />
            <input
              id="cp-amount"
              inputMode="decimal"
              autoComplete="off"
              autoFocus
              placeholder="0.00"
              value={draft.amount}
              maxLength={40}
              onChange={(event) => set("amount", event.target.value)}
              aria-invalid={amountError || undefined}
              aria-describedby={amountError ? "cp-amount-error" : "cp-amount-hint"}
              className="num h-full min-w-0 flex-1 bg-transparent text-xl font-medium outline-none placeholder:text-faint"
            />
            <span className="num text-sm text-muted-foreground">{settings.asset.symbol}</span>
          </div>
          {amountError ? (
            <p id="cp-amount-error" role="alert" className="text-[13px] leading-5 text-danger-fg">
              {amountMessage}
            </p>
          ) : (
            <p id="cp-amount-hint" className="text-[13px] leading-5 text-muted-foreground">
              {AMOUNT_FORMAT_HINT}
            </p>
          )}
        </div>

        <div className="grid gap-4 sm:grid-cols-[minmax(0,1fr)_168px]">
          <div className="flex flex-col gap-1.5">
            <FieldLabel htmlFor="cp-order" optional>
              Order ID
            </FieldLabel>
            <Input
              id="cp-order"
              placeholder="ORD-20432"
              value={draft.orderId}
              onChange={(event) => set("orderId", event.target.value)}
              aria-invalid={serverErrors.orderId ? true : undefined}
              className="num"
            />
            <FieldError message={serverErrors.orderId} />
          </div>
          <div className="flex flex-col gap-1.5">
            <FieldLabel htmlFor="cp-ttl">Expires after</FieldLabel>
            <Select value={draft.ttl} onValueChange={(value) => set("ttl", value)}>
              <SelectTrigger id="cp-ttl" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={TTL_DEFAULT}>Default, {defaultTtl}</SelectItem>
                {TTL_OPTIONS.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <FieldError message={serverErrors.ttl} />
          </div>
        </div>

        <div className="flex flex-col gap-1.5">
          <FieldLabel htmlFor="cp-description" optional>
            Description
          </FieldLabel>
          <Input
            id="cp-description"
            placeholder="What the customer is paying for"
            value={draft.description}
            onChange={(event) => set("description", event.target.value)}
            aria-invalid={serverErrors.description ? true : undefined}
          />
          <FieldError message={serverErrors.description} />
        </div>

        <div className="flex flex-col gap-1.5">
          <FieldLabel htmlFor="cp-email" optional>
            Customer email
          </FieldLabel>
          <Input
            id="cp-email"
            type="email"
            autoComplete="off"
            placeholder="customer@example.com"
            value={draft.email}
            onChange={(event) => set("email", event.target.value)}
            aria-invalid={serverErrors.email ? true : undefined}
          />
          <FieldError message={serverErrors.email} />
        </div>

        <MetadataEditor rows={draft.metadata} onChange={(rows) => set("metadata", rows)} />
        <FieldError message={Object.entries(serverErrors).find(([name]) => name.startsWith("metadata"))?.[1]} />

        {failure && (
          <p role="alert" className="rounded-lg border border-danger-border bg-danger-subtle px-3 py-2 text-[13px] leading-5 text-danger-fg">
            {failure}
          </p>
        )}
        {notReady && (
          <Callout
            tone={notReady === "chain" ? "info" : "danger"}
            icon={<HourglassIcon />}
            title={REFUSAL[notReady].title}
            role="alert"
            data-slot={REFUSAL[notReady].slot}
          >
            {REFUSAL[notReady].text}{" "}
            <Link href="/settings/network" className="font-medium text-foreground underline underline-offset-4" onClick={onClose}>
              Open Network
            </Link>
          </Callout>
        )}

        {showApi && (
          <CodeSnippet
            code={curl}
            label={
              <span>
                The same request with the API. <span className="text-faint">Updates as you type.</span>
              </span>
            }
            copyLabel="Copy request"
            className="shrink-0"
          />
        )}
      </div>

      <footer className="flex items-center justify-between gap-2 border-t border-border bg-muted/40 px-5 py-3 max-sm:pb-[max(0.75rem,env(safe-area-inset-bottom))]">
        <Button
          type="button"
          variant="ghost"
          aria-expanded={showApi}
          onClick={() => setShowApi((v) => !v)}
          className="-ml-2 font-normal text-muted-foreground"
        >
          <TerminalIcon data-icon="inline-start" />
          {showApi ? "Hide API request" : "Show API request"}
        </Button>
        <div className="flex items-center gap-2">
          <Button type="button" variant="outline" onClick={onClose} className="max-sm:hidden">
            Cancel
          </Button>
          <Button type="submit" data-wave-source="" disabled={sending}>
            {sending ? "Creating…" : "Create payment"}
          </Button>
        </div>
      </footer>
    </form>
  )
}

/** What the dialog says when the server cannot create payments at the moment. */
const REFUSAL = {
  chain: { ...CHAIN_NOT_READY_OPERATOR, slot: "chain-not-ready" },
  clock: { ...CLOCK_BEHIND_OPERATOR, slot: "clock-behind" },
  network: { ...NETWORK_INVALID_OPERATOR, slot: "network-invalid" },
} as const

function FieldError({ message }: { message?: string }) {
  if (!message) return null
  return (
    <p role="alert" className="text-[13px] leading-5 text-danger-fg">
      {message}
    </p>
  )
}

function MetadataEditor({
  rows,
  onChange,
}: {
  rows: Draft["metadata"]
  onChange: (rows: Draft["metadata"]) => void
}) {
  const update = (index: number, patch: Partial<Draft["metadata"][number]>) =>
    onChange(rows.map((row, i) => (i === index ? { ...row, ...patch } : row)))

  if (rows.length === 0) {
    return (
      <button
        type="button"
        onClick={() => onChange([{ key: "", value: "" }])}
        className="focus-ring -my-1 flex w-fit items-center gap-1 rounded-md py-1 text-[13px] leading-5 text-muted-foreground transition-colors hover:text-foreground"
      >
        <ChevronRightIcon className="size-3.5" />
        Add metadata
      </button>
    )
  }

  return (
    <fieldset className="flex flex-col gap-1.5">
      <legend className="mb-1.5 flex w-full items-center gap-2 text-[13px] leading-5 font-medium">
        Metadata
        <span className="font-normal text-faint">Returned in webhooks, never shown to the customer</span>
      </legend>
      {rows.map((row, index) => (
        <div key={index} className="flex items-center gap-2">
          <Input
            aria-label="Key"
            placeholder="key"
            value={row.key}
            onChange={(event) => update(index, { key: event.target.value })}
            className="num w-[38%] shrink-0"
          />
          <Input
            aria-label="Value"
            placeholder="value"
            value={row.value}
            onChange={(event) => update(index, { value: event.target.value })}
            className="num"
          />
          <Button
            type="button"
            variant="ghost"
            size="icon"
            aria-label="Remove row"
            onClick={() => onChange(rows.filter((_, i) => i !== index))}
            className="shrink-0 text-muted-foreground"
          >
            <XIcon />
          </Button>
        </div>
      ))}
      <button
        type="button"
        onClick={() => onChange([...rows, { key: "", value: "" }])}
        className="focus-ring mt-0.5 flex w-fit items-center gap-1 rounded-md py-1 text-[13px] leading-5 text-muted-foreground transition-colors hover:text-foreground"
      >
        <PlusIcon className="size-3.5" />
        Add row
      </button>
    </fieldset>
  )
}

/* ------------------------------ Created ------------------------------------ */

function CreatedView({ payment, onClose, onAnother }: { payment: Payment; onClose: () => void; onAnother: () => void }) {
  const url = checkoutUrl(payment)

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <header className="flex items-start justify-between gap-4 px-5 pt-5 pb-4">
        <div>
          <DialogTitle className="text-base leading-6 font-semibold tracking-[-0.015em]">Payment created</DialogTitle>
          <DialogDescription className="mt-0.5 text-[13px] leading-5">
            Send the link to your customer, or let them scan the code.
          </DialogDescription>
        </div>
        <Button type="button" variant="ghost" size="icon-sm" onClick={onClose} className="-mt-1 -mr-1.5 text-muted-foreground">
          <XIcon />
          <span className="sr-only">Close</span>
        </Button>
      </header>

      <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-5 pb-5">
        <div className="flex items-center gap-5 max-sm:gap-4">
          <QrTile value={url} size={112} className="max-sm:scale-[0.86] max-sm:-m-2.5" />
          <div className="flex min-w-0 flex-1 flex-col gap-3">
            <div className="min-w-0">
              <Amount value={payment.amount} asset={payment.asset} className="text-[26px] leading-8 font-medium tracking-[-0.04em]" />
              <p className="mt-1 truncate text-[13px] leading-5 text-muted-foreground">
                {payment.order_id && <span className="num text-foreground">{payment.order_id}</span>}
                {payment.order_id && payment.description && <span className="mx-1.5 text-faint">·</span>}
                {payment.description}
                {!payment.order_id && !payment.description && "No order details"}
              </p>
            </div>
            <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
              <PaymentStatusBadge status={payment.status} live={false} />
              <Countdown expiresAt={payment.expires_at} createdAt={payment.created_at} />
            </div>
          </div>
        </div>

        <div className="flex flex-col gap-1.5">
          <span className="text-[13px] leading-5 font-medium">Checkout link</span>
          <div className="flex h-9 items-center gap-1 rounded-lg border border-input pr-1 pl-3 dark:bg-input/30">
            <span className="num min-w-0 flex-1 truncate text-[13px]" title={url}>
              {url}
            </span>
            <CopyButton value={url} variant="secondary" size="sm" className="shrink-0">
              Copy link
            </CopyButton>
          </div>
        </div>
      </div>

      <footer className="flex items-center justify-between gap-2 border-t border-border bg-muted/40 px-5 py-3 max-sm:flex-col-reverse max-sm:items-stretch max-sm:pb-[max(0.75rem,env(safe-area-inset-bottom))]">
        <Button type="button" variant="ghost" onClick={onAnother} className="font-normal text-muted-foreground sm:-ml-2">
          <PlusIcon data-icon="inline-start" />
          Create another
        </Button>
        <div className="flex items-center gap-2 max-sm:grid max-sm:grid-cols-2">
          <Button variant="outline" asChild className="pointer-coarse:h-10">
            <Link href={paymentPath(payment.id)}>View payment</Link>
          </Button>
          <Button asChild className="pointer-coarse:h-10">
            <a href={checkoutPath(payment.id)} target="_blank" rel="noopener noreferrer">
              Open checkout
              <ArrowUpRightIcon data-icon="inline-end" />
            </a>
          </Button>
        </div>
      </footer>
    </div>
  )
}
