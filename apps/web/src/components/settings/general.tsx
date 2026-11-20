"use client"

import * as React from "react"
import { ImageUpIcon, MonitorIcon } from "lucide-react"
import { toast } from "sonner"

import {
  Amount,
  CopyButton,
  DetailList,
  DetailRow,
  ErrorState,
  MerchantAvatar,
  NetworkBadge,
  PaymentStatusBadge,
  SettingsRow,
  SettingsSection,
  StatusBadge,
} from "@/components/hmn"
import { useDashboard } from "@/components/shell/session"
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field"
import { InputGroupAddon, InputGroupInput, InputGroupText } from "@/components/ui/input-group"
import { Select, SelectContent, SelectItem, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { api } from "@/lib/api/client"
import type { SessionInfo } from "@/lib/api/types"
import { useQuery } from "@/lib/api/use-query"
import { describeError, fieldErrors } from "@/lib/errors"
import { formatDate, formatDateTime, formatDuration, relativeTime } from "@/lib/format"
import { amountInputMessage, parseAmountInput } from "@/lib/money"
import { useNow } from "@/lib/use-now"

import { Button, Input, InputGroup, SelectTrigger } from "./shared"

const PAYMENT_WINDOWS = [
  { value: 900, label: "15 minutes" },
  { value: 1800, label: "30 minutes" },
  { value: 3600, label: "1 hour" },
  { value: 10800, label: "3 hours" },
  { value: 86400, label: "24 hours" },
]

/** The API takes an inline logo of at most 512 KB as a data: URL; base64 makes a file a third larger. */
const LOGO_MAX_BYTES = 380 * 1024
const LOGO_TYPES = ["image/png", "image/jpeg", "image/webp", "image/gif"]

/**
 * Settings → General: what customers see on the checkout, the defaults of new payments, the
 * operator's own account and sessions, and what this instance is.
 */
export function GeneralSettings() {
  return (
    <>
      <IdentitySection />
      <PaymentDefaultsSection />
      <AccountSection />
      <SessionsSection />
      <AboutSection />
    </>
  )
}

/** Runs a save: busy flag, the server's field errors, a toast either way. */
function useSave<T>() {
  const [busy, setBusy] = React.useState(false)
  const [errors, setErrors] = React.useState<Record<string, string>>({})
  const run = async (action: () => Promise<T>, done: (result: T) => void, message = "Saved") => {
    setBusy(true)
    setErrors({})
    try {
      done(await action())
      toast.success(message)
    } catch (error) {
      const fields = fieldErrors(error)
      setErrors(fields)
      toast.error("Not saved", { description: describeError(error) })
    } finally {
      setBusy(false)
    }
  }
  return { busy, errors, run, clear: () => setErrors({}) }
}

/* -------------------------------------------------------------------------------------------
 * Checkout identity + live preview
 * ----------------------------------------------------------------------------------------- */

function IdentitySection() {
  const { settings, setSettings } = useDashboard()
  const [name, setName] = React.useState(settings.merchant.name)
  const [email, setEmail] = React.useState(settings.merchant.support_email)
  const [url, setUrl] = React.useState(settings.merchant.support_url)
  const [logo, setLogo] = React.useState(settings.merchant.logo_url)
  const [logoError, setLogoError] = React.useState<string>()
  const fileRef = React.useRef<HTMLInputElement>(null)
  const save = useSave<typeof settings>()
  const shownName = name.trim() || "Your business"
  const dirty =
    name !== settings.merchant.name || email !== settings.merchant.support_email || url !== settings.merchant.support_url || logo !== settings.merchant.logo_url

  const pickLogo = (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    event.target.value = ""
    if (!file) return
    if (!LOGO_TYPES.includes(file.type)) return setLogoError("Use a PNG, JPEG, WebP or GIF image.")
    if (file.size > LOGO_MAX_BYTES) return setLogoError("That file is over 380 KB. Use a smaller image.")
    setLogoError(undefined)
    const reader = new FileReader()
    reader.onload = () => typeof reader.result === "string" && setLogo(reader.result)
    reader.onerror = () => setLogoError("That file could not be read.")
    reader.readAsDataURL(file)
  }

  return (
    <SettingsSection
      title="Checkout identity"
      description="What customers see at the top of the checkout page and on payment links."
      footer="Applies to open and new payments straight away."
      actions={
        <Button
          size="sm"
          disabled={save.busy || !dirty}
          onClick={() =>
            void save.run(
              () =>
                api.updateMerchant({
                  merchant_name: name.trim(),
                  logo_url: logo,
                  support_email: email.trim(),
                  support_url: url.trim(),
                  public_url: settings.public_url,
                }),
              setSettings,
            )
          }
        >
          {save.busy ? "Saving…" : "Save"}
        </Button>
      }
    >
      <div className="grid gap-x-8 gap-y-6 md:grid-cols-[minmax(0,1fr)_264px]">
        <FieldGroup>
          <Field data-invalid={save.errors.merchant_name ? true : undefined}>
            <FieldLabel htmlFor="merchant-name">Business name</FieldLabel>
            <Input
              id="merchant-name"
              value={name}
              maxLength={80}
              autoComplete="organization"
              aria-invalid={save.errors.merchant_name ? true : undefined}
              onChange={(event) => setName(event.target.value)}
            />
            {save.errors.merchant_name && <FieldError>{save.errors.merchant_name}</FieldError>}
          </Field>

          <Field data-invalid={logoError || save.errors.logo_url ? true : undefined}>
            <FieldLabel htmlFor="merchant-logo">Logo</FieldLabel>
            <div className="flex items-center gap-3">
              <MerchantAvatar name={shownName} logoUrl={logo} className="size-10 rounded-lg text-base" />
              <input ref={fileRef} id="merchant-logo" type="file" accept={LOGO_TYPES.join(",")} className="sr-only" onChange={pickLogo} />
              <Button variant="outline" onClick={() => fileRef.current?.click()}>
                <ImageUpIcon data-icon="inline-start" />
                {logo ? "Replace" : "Upload"}
              </Button>
              {logo && (
                <Button variant="ghost" className="text-muted-foreground" onClick={() => setLogo("")}>
                  Remove
                </Button>
              )}
            </div>
            {logoError || save.errors.logo_url ? (
              <FieldError>{logoError ?? save.errors.logo_url}</FieldError>
            ) : (
              <FieldDescription className="text-[13px]">
                Square PNG, JPEG, WebP or GIF, up to 380 KB. Without one, customers see your initial.
              </FieldDescription>
            )}
          </Field>

          <Field data-invalid={save.errors.support_email ? true : undefined}>
            <FieldLabel htmlFor="support-email">Support email</FieldLabel>
            <Input
              id="support-email"
              type="email"
              value={email}
              autoComplete="email"
              aria-invalid={save.errors.support_email ? true : undefined}
              onChange={(event) => setEmail(event.target.value)}
            />
            {save.errors.support_email ? (
              <FieldError>{save.errors.support_email}</FieldError>
            ) : (
              <FieldDescription className="text-[13px]">Where the checkout “Help” link sends customers.</FieldDescription>
            )}
          </Field>

          <Field data-invalid={save.errors.support_url ? true : undefined}>
            <FieldLabel htmlFor="support-url">
              Support page <span className="font-normal text-muted-foreground">Optional</span>
            </FieldLabel>
            <Input
              id="support-url"
              type="url"
              inputMode="url"
              value={url}
              placeholder="https://"
              aria-invalid={save.errors.support_url ? true : undefined}
              onChange={(event) => setUrl(event.target.value)}
            />
            {save.errors.support_url ? (
              <FieldError>{save.errors.support_url}</FieldError>
            ) : (
              <FieldDescription className="text-[13px]">Used for “Help” when there is no support email.</FieldDescription>
            )}
          </Field>
        </FieldGroup>

        <CheckoutPreview name={shownName} logo={logo} help={email.trim() || (url.trim() ? hostOf(url) : "")} asset={settings.asset.symbol} />
      </div>
    </SettingsSection>
  )
}

function hostOf(url: string) {
  return url.replace(/^https?:\/\//, "").replace(/\/$/, "")
}

/** A miniature of the checkout's merchant panel, fed by the form as you type. The order is an example. */
function CheckoutPreview({ name, logo, help, asset }: { name: string; logo: string; help: string; asset: string }) {
  return (
    <div className="flex flex-col gap-2 md:pt-[26px]">
      <div aria-hidden="true" className="rounded-lg border border-border bg-background/60 p-4">
        <div className="flex items-center justify-between gap-2">
          <div className="flex min-w-0 items-center gap-2">
            <MerchantAvatar name={name} logoUrl={logo} className="size-6 rounded-[5px] text-[11px]" />
            <span className="truncate text-[13px] font-medium">{name}</span>
          </div>
          <PaymentStatusBadge status="awaiting" size="sm" live={false} />
        </div>
        <div className="mt-6 text-[13px] leading-5 font-medium">Example order</div>
        <div className="num text-[11px] leading-4 text-muted-foreground">Order EXAMPLE-1</div>
        <Amount value="149.00" asset={asset} className="mt-3 block text-[26px] leading-8 font-medium tracking-[-0.04em] [&>[data-slot=amount-symbol]]:text-sm" />
        <NetworkBadge asset size="sm" className="mt-3" />
        <div className="mt-6 flex items-center justify-between gap-3 border-t border-border pt-3 text-[11px] leading-4 text-faint">
          <span>Help</span>
          <span className="truncate text-muted-foreground">{help || "not set"}</span>
        </div>
      </div>
      <p className="text-xs leading-5 text-faint">Preview of the checkout header, with an example order. Updates as you type.</p>
    </div>
  )
}

/* -------------------------------------------------------------------------------------------
 * Payment defaults
 * ----------------------------------------------------------------------------------------- */

function PaymentDefaultsSection() {
  const { settings, setSettings } = useDashboard()
  const current = settings.payments
  const [ttl, setTtl] = React.useState(current.ttl_seconds)
  const [confirmations, setConfirmations] = React.useState(String(current.required_confirmations))
  const [tolerance, setTolerance] = React.useState(current.underpayment_tolerance)
  const [redirect, setRedirect] = React.useState(current.default_redirect_url)
  const save = useSave<typeof settings>()
  const asset = settings.asset.symbol
  // The same reading as every amount field (digits, at most one dot; nothing guessed), except
  // that zero is an answer here: "no tolerance".
  const typed = parseAmountInput(tolerance, settings.asset.decimals)
  const toleranceProblem = typed.ok || typed.reason === "zero" || typed.reason === "empty" ? undefined : amountInputMessage(typed.reason, settings.asset.decimals)
  const toleranceValue = typed.ok ? typed.amount : "0"
  const strict = !typed.ok
  const windows = PAYMENT_WINDOWS.some((option) => option.value === ttl)
    ? PAYMENT_WINDOWS
    : [...PAYMENT_WINDOWS, { value: ttl, label: formatDuration(ttl * 1000) }].sort((a, b) => a.value - b.value)
  const dirty =
    ttl !== current.ttl_seconds ||
    confirmations !== String(current.required_confirmations) ||
    tolerance !== current.underpayment_tolerance ||
    redirect !== current.default_redirect_url

  return (
    <SettingsSection
      title="Payment defaults"
      description="Used for every new payment unless the API request sets its own value. Payments that already exist keep theirs."
      footer="Exchanges often take their withdrawal fee out of the amount sent. A small tolerance absorbs it; it never exceeds 5% of a payment."
      actions={
        <Button
          size="sm"
          disabled={save.busy || !dirty || toleranceProblem !== undefined}
          onClick={() =>
            void save.run(
              () =>
                api.updatePaymentDefaults({
                  ttl_seconds: ttl,
                  required_confirmations: Number(confirmations) || 0,
                  underpayment_tolerance: toleranceValue,
                  default_redirect_url: redirect.trim(),
                }),
              (next) => {
                setSettings(next)
                setTolerance(next.payments.underpayment_tolerance)
                setConfirmations(String(next.payments.required_confirmations))
              },
            )
          }
        >
          {save.busy ? "Saving…" : "Save"}
        </Button>
      }
    >
      <div>
        <SettingsRow
          label="Payment window"
          description={save.errors.ttl_seconds ?? "How long a customer has to pay before the payment expires. Checkouts of limited payment links get 10 minutes."}
        >
          <Select value={String(ttl)} onValueChange={(value) => setTtl(Number(value))}>
            <SelectTrigger className="w-40" aria-label="Payment window">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {windows.map((option) => (
                <SelectItem key={option.value} value={String(option.value)}>
                  {option.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </SettingsRow>
        <SettingsRow
          label="Confirmations"
          description={
            save.errors.required_confirmations ?? "Blocks a transfer must be buried under before a payment counts as paid. More is safer and slower."
          }
        >
          <InputGroup className="w-40">
            <InputGroupInput
              aria-label="Required confirmations"
              inputMode="numeric"
              value={confirmations}
              aria-invalid={save.errors.required_confirmations ? true : undefined}
              onChange={(event) => setConfirmations(event.target.value.replace(/\D/g, ""))}
              className="num text-right"
            />
            <InputGroupAddon align="inline-end">
              <InputGroupText className="text-[13px] font-normal">blocks</InputGroupText>
            </InputGroupAddon>
          </InputGroup>
        </SettingsRow>
        <SettingsRow
          label="Underpayment tolerance"
          description={
            save.errors.underpayment_tolerance ??
            (toleranceProblem ? (
              <span role="alert" className="text-danger-fg" data-slot="tolerance-problem">
                {toleranceProblem}
              </span>
            ) : strict ? (
              "Payments must arrive in full. Anything less stays open for a top-up, then closes as underpaid."
            ) : (
              <>
                Payments short by up to <Amount value={toleranceValue} asset={asset} className="text-foreground" /> still count as paid.
              </>
            ))
          }
        >
          <InputGroup className="w-40">
            <InputGroupInput
              aria-label={`Underpayment tolerance in ${asset}`}
              inputMode="decimal"
              value={tolerance}
              aria-invalid={save.errors.underpayment_tolerance || toleranceProblem ? true : undefined}
              maxLength={40}
              onChange={(event) => setTolerance(event.target.value)}
              className="num text-right"
            />
            <InputGroupAddon align="inline-end">
              <InputGroupText className="text-[13px] font-normal">{asset}</InputGroupText>
            </InputGroupAddon>
          </InputGroup>
        </SettingsRow>
      </div>
      <div className="border-t border-border pt-5">
        <Field data-invalid={save.errors.default_redirect_url ? true : undefined}>
          <FieldLabel htmlFor="default-redirect">
            Return URL <span className="font-normal text-muted-foreground">Optional</span>
          </FieldLabel>
          <Input
            id="default-redirect"
            type="url"
            inputMode="url"
            placeholder="https://"
            value={redirect}
            aria-invalid={save.errors.default_redirect_url ? true : undefined}
            onChange={(event) => setRedirect(event.target.value)}
            className="max-w-md"
          />
          {save.errors.default_redirect_url ? (
            <FieldError>{save.errors.default_redirect_url}</FieldError>
          ) : (
            <FieldDescription className="text-[13px]">
              Where the checkout’s “Return to store” button leads. A <span className="num text-foreground">redirect_url</span> on a payment
              or a payment link overrides it.
            </FieldDescription>
          )}
        </Field>
      </div>
    </SettingsSection>
  )
}

/* -------------------------------------------------------------------------------------------
 * The operator's account
 * ----------------------------------------------------------------------------------------- */

function AccountSection() {
  const { operator } = useDashboard()
  const [current, setCurrent] = React.useState("")
  const [next, setNext] = React.useState("")
  const save = useSave<void>()

  return (
    <SettingsSection
      title="Your account"
      description="The one account that runs this instance. A lost password is reset on the server with “hmnpay reset-password”."
      footer="Changing the password signs out every other session and keeps this one."
      actions={
        <Button
          size="sm"
          disabled={save.busy || !current || !next}
          onClick={() =>
            void save.run(
              () => api.changePassword({ current_password: current, new_password: next }),
              () => {
                setCurrent("")
                setNext("")
              },
              "Password changed",
            )
          }
        >
          {save.busy ? "Saving…" : "Change password"}
        </Button>
      }
    >
      <FieldGroup>
        <Field>
          <FieldLabel>Email</FieldLabel>
          <p className="num text-sm leading-6">{operator.email}</p>
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field data-invalid={save.errors.current_password ? true : undefined}>
            <FieldLabel htmlFor="current-password">Current password</FieldLabel>
            <Input
              id="current-password"
              type="password"
              autoComplete="current-password"
              value={current}
              aria-invalid={save.errors.current_password ? true : undefined}
              onChange={(event) => setCurrent(event.target.value)}
            />
            {save.errors.current_password && <FieldError>{save.errors.current_password}</FieldError>}
          </Field>
          <Field data-invalid={save.errors.new_password ? true : undefined}>
            <FieldLabel htmlFor="new-password">New password</FieldLabel>
            <Input
              id="new-password"
              type="password"
              autoComplete="new-password"
              value={next}
              aria-invalid={save.errors.new_password ? true : undefined}
              onChange={(event) => setNext(event.target.value)}
            />
            {save.errors.new_password ? (
              <FieldError>{save.errors.new_password}</FieldError>
            ) : (
              <FieldDescription className="text-[13px]">At least 12 characters. A passphrase works well.</FieldDescription>
            )}
          </Field>
        </div>
      </FieldGroup>
    </SettingsSection>
  )
}

/** "Chrome on macOS" from a User-Agent, or the raw string when it says nothing recognisable. */
function describeAgent(agent: string): string {
  if (!agent) return "Unknown browser"
  const browser = /Edg\//.test(agent) ? "Edge" : /Firefox\//.test(agent) ? "Firefox" : /Chrome\//.test(agent) ? "Chrome" : /Safari\//.test(agent) ? "Safari" : null
  const system = /Windows/.test(agent)
    ? "Windows"
    : /iPhone|iPad/.test(agent)
      ? "iOS"
      : /Android/.test(agent)
        ? "Android"
        : /Mac OS X/.test(agent)
          ? "macOS"
          : /Linux/.test(agent)
            ? "Linux"
            : null
  if (!browser) return agent.length > 48 ? `${agent.slice(0, 48)}…` : agent
  return system ? `${browser} on ${system}` : browser
}

function SessionsSection() {
  const { signOut } = useDashboard()
  const sessions = useQuery("operator/sessions", (signal) => api.sessions(signal), { refreshMs: 60_000 })
  const now = useNow()
  const [ending, setEnding] = React.useState<string | null>(null)

  const end = async (session: SessionInfo) => {
    if (session.current) return void signOut()
    setEnding(session.id)
    try {
      await api.revokeSession(session.id)
      sessions.mutate((list) => (list ? { ...list, data: list.data.filter((item) => item.id !== session.id) } : list))
      toast("Session signed out")
    } catch (error) {
      toast.error("Couldn’t sign that session out", { description: describeError(error) })
      sessions.reload()
    } finally {
      setEnding(null)
    }
  }

  return (
    <SettingsSection
      title="Sessions"
      description="Browsers signed in to this dashboard. Sign out any you don’t recognise, then change the password."
    >
      {!sessions.data ? (
        sessions.error ? (
          <ErrorState error={sessions.error} onRetry={sessions.reload} bordered={false} className="py-8" />
        ) : (
          <div className="space-y-3" role="status" aria-label="Loading sessions">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        )
      ) : (
        <ul className="-my-1 divide-y divide-border" data-slot="sessions">
          {sessions.data.data.map((session) => (
            <li key={session.id} className="flex items-center gap-3 py-3">
              <span
                aria-hidden="true"
                className="flex size-8 shrink-0 items-center justify-center rounded-lg border border-border bg-background/60 text-muted-foreground"
              >
                <MonitorIcon className="size-4" />
              </span>
              <div className="min-w-0 flex-1">
                <p className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm leading-5 font-medium">
                  <span className="truncate" title={session.user_agent}>
                    {describeAgent(session.user_agent)}
                  </span>
                  {session.current && (
                    <StatusBadge tone="success" size="sm">
                      This browser
                    </StatusBadge>
                  )}
                </p>
                <p className="truncate text-[13px] leading-5 text-muted-foreground">
                  <span className="num">{session.ip || "unknown address"}</span>
                  <span className="mx-1.5 text-faint">·</span>
                  signed in {relativeTime(session.created_at, now)}
                  <span className="mx-1.5 text-faint">·</span>
                  <span title={formatDateTime(session.expires_at)}>expires {relativeTime(session.expires_at, now)}</span>
                </p>
              </div>
              <Button variant="outline" size="sm" disabled={ending === session.id} onClick={() => void end(session)}>
                {session.current ? "Log out" : "Sign out"}
              </Button>
            </li>
          ))}
        </ul>
      )}
    </SettingsSection>
  )
}

/* -------------------------------------------------------------------------------------------
 * About
 * ----------------------------------------------------------------------------------------- */

/** An inline link to a plain-text file the server serves. */
const textLink = "focus-ring rounded-sm underline decoration-border-strong underline-offset-4 transition-colors hover:text-foreground"

function AboutSection() {
  const { settings, setSettings, instance } = useDashboard()
  const operator = useQuery("operator", (signal) => api.operator(signal))
  const [publicUrl, setPublicUrl] = React.useState(settings.public_url)
  const save = useSave<typeof settings>()

  return (
    <SettingsSection
      title="This instance"
      footer={
        <>
          Checkout and payment-link addresses are built from the public URL. The server’s own setting{" "}
          <span className="num text-foreground">HMNPAY_PUBLIC_URL</span> decides cookies and which host names it answers for.
        </>
      }
      actions={
        <Button
          size="sm"
          disabled={save.busy || publicUrl.trim() === settings.public_url}
          onClick={() =>
            void save.run(
              () =>
                api.updateMerchant({
                  merchant_name: settings.merchant.name,
                  logo_url: settings.merchant.logo_url,
                  support_email: settings.merchant.support_email,
                  support_url: settings.merchant.support_url,
                  public_url: publicUrl.trim(),
                }),
              (next) => {
                setSettings(next)
                setPublicUrl(next.public_url)
              },
            )
          }
        >
          {save.busy ? "Saving…" : "Save"}
        </Button>
      }
    >
      <Field data-invalid={save.errors.public_url ? true : undefined}>
        <FieldLabel htmlFor="public-url">Public URL</FieldLabel>
        <div className="flex max-w-md items-center gap-1">
          <Input
            id="public-url"
            type="url"
            inputMode="url"
            placeholder="https://pay.example.com"
            value={publicUrl}
            aria-invalid={save.errors.public_url ? true : undefined}
            onChange={(event) => setPublicUrl(event.target.value)}
            className="num"
          />
          <CopyButton value={settings.public_url} label="Copy public URL" size="icon-sm" className="pointer-coarse:size-10" />
        </div>
        {save.errors.public_url && <FieldError>{save.errors.public_url}</FieldError>}
      </Field>
      <DetailList className="-my-2">
        <DetailRow label="Version">
          {instance ? (
            <span className="num" data-slot="about-version">
              {/^\d/.test(instance.version) ? `v${instance.version}` : instance.version}
              <span className="text-muted-foreground"> · build {instance.commit}</span>
            </span>
          ) : (
            <Skeleton className="h-4 w-24" />
          )}
        </DetailRow>
        <DetailRow label="Set up">{operator.data?.created_at ? formatDate(operator.data.created_at) : <Skeleton className="h-4 w-24" />}</DetailRow>
        {/* Files the server itself serves (not pages of this app), built into the binary: its
            licence, the additional terms of that licence, and the licences of the third-party
            software in it. */}
        <DetailRow label="License">
          <span data-slot="about-license" className="inline-flex flex-wrap items-baseline justify-end gap-x-1.5">
            <a href="/license.txt" target="_blank" rel="noopener noreferrer" data-slot="license" className={textLink}>
              GNU GPL 3.0 or later
            </a>
            <span className="text-muted-foreground">with</span>
            <a href="/additional-terms.txt" target="_blank" rel="noopener noreferrer" data-slot="additional-terms" className={textLink}>
              additional terms
            </a>
          </span>
        </DetailRow>
        <DetailRow label="Open-source software">
          <a href="/third-party-notices.txt" target="_blank" rel="noopener noreferrer" data-slot="third-party-notices" className={textLink}>
            Third-party notices
          </a>
        </DetailRow>
      </DetailList>
    </SettingsSection>
  )
}
