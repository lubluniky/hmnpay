"use client"

import * as React from "react"
import { LockIcon, UploadIcon } from "lucide-react"

import { FlowField, FlowInput, useAutoFocus } from "@/components/auth/flow"
import { Amount, MerchantAvatar, NetworkBadge, PaymentStatusBadge } from "@/components/hmn"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

import { isEmail, type SetupForm } from "./data"
import { StepHeader } from "./parts"

/** The API takes an inline logo of at most 512 KB as a data: URL; base64 makes a file a third larger. */
const LOGO_MAX_BYTES = 380 * 1024
const LOGO_TYPES = ["image/png", "image/jpeg", "image/webp", "image/gif"]

export function StepBusiness({
  form,
  set,
  showErrors,
  host,
  errors,
}: {
  form: SetupForm
  set: (patch: Partial<SetupForm>) => void
  showErrors: boolean
  host: string
  /** What the server said about fields of this step. */
  errors: Record<string, string>
}) {
  const nameRef = useAutoFocus<HTMLInputElement>(!form.merchantName)
  const fileRef = React.useRef<HTMLInputElement>(null)
  const [logoError, setLogoError] = React.useState<string>()

  const nameError = errors.merchant_name ?? (showErrors && !form.merchantName.trim() ? "Customers need to see who they’re paying." : undefined)
  const emailError = errors.support_email ?? (showErrors && !isEmail(form.supportEmail) ? "Enter an address customers can write to." : undefined)

  const pickLogo = (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    event.target.value = ""
    if (!file) return
    if (!LOGO_TYPES.includes(file.type)) {
      setLogoError("Use a PNG, JPEG, WebP or GIF image.")
      return
    }
    if (file.size > LOGO_MAX_BYTES) {
      setLogoError("That file is over 380 KB. Use a smaller image.")
      return
    }
    setLogoError(undefined)
    // The logo is stored with the settings as an inline image, so it is read here, not uploaded.
    const reader = new FileReader()
    reader.onload = () => typeof reader.result === "string" && set({ logoUrl: reader.result })
    reader.onerror = () => setLogoError("That file could not be read.")
    reader.readAsDataURL(file)
  }

  return (
    <>
      <StepHeader title="Your business">
        This is what customers see when they pay you. Nothing else about you is shown.
      </StepHeader>

      <div className="mt-8 grid gap-8 md:grid-cols-[minmax(0,1fr)_300px] md:gap-10">
        <div className="flex flex-col gap-5">
          <FlowField label="Business name" htmlFor="setup-merchant" error={nameError}>
            <FlowInput
              ref={nameRef}
              id="setup-merchant"
              name="organization"
              autoComplete="organization"
              placeholder="Acme Inc."
              maxLength={80}
              value={form.merchantName}
              onChange={(e) => set({ merchantName: e.target.value })}
              aria-invalid={nameError ? true : undefined}
            />
          </FlowField>

          <FlowField
            label="Support email"
            htmlFor="setup-support"
            error={emailError}
            hint="Behind the Help link, for customers whose payment needs a hand."
          >
            <FlowInput
              id="setup-support"
              type="email"
              name="support-email"
              inputMode="email"
              autoComplete="off"
              spellCheck={false}
              placeholder="billing@company.com"
              value={form.supportEmail}
              onChange={(e) => set({ supportEmail: e.target.value })}
              aria-invalid={emailError ? true : undefined}
            />
          </FlowField>

          <FlowField label="Logo" htmlFor="setup-logo" optional error={logoError ?? errors.logo_url}>
            <div className="flex items-center gap-3">
              <MerchantAvatar
                name={form.merchantName || "?"}
                logoUrl={form.logoUrl}
                className={cn("size-11 rounded-lg text-base", !form.merchantName && !form.logoUrl && "bg-muted text-faint")}
              />
              <input
                ref={fileRef}
                id="setup-logo"
                type="file"
                accept="image/png,image/jpeg,image/webp,image/gif"
                className="sr-only"
                onChange={pickLogo}
              />
              <Button type="button" variant="outline" className="h-11 px-3.5 text-sm" onClick={() => fileRef.current?.click()}>
                <UploadIcon />
                {form.logoUrl ? "Replace" : "Upload"}
              </Button>
              {form.logoUrl ? (
                <Button
                  type="button"
                  variant="ghost"
                  className="h-11 px-3 text-sm text-muted-foreground"
                  onClick={() => set({ logoUrl: null })}
                >
                  Remove
                </Button>
              ) : (
                <span className="text-[13px] leading-5 whitespace-nowrap text-muted-foreground">Square, up to 380 KB</span>
              )}
            </div>
          </FlowField>
        </div>

        <CheckoutPreview
          name={form.merchantName}
          logoUrl={form.logoUrl}
          supportEmail={form.supportEmail}
          host={host}
        />
      </div>
    </>
  )
}

/* ----------------------------- checkout preview --------------------------- */

/**
 * A miniature of the hosted checkout's summary, built from the same primitives,
 * so the operator sees their name, logo and support address in place as they type.
 */
function CheckoutPreview({
  name,
  logoUrl,
  supportEmail,
  host,
}: {
  name: string
  logoUrl: string | null
  supportEmail: string
  host: string
}) {
  const shownName = name.trim() || "Your business"
  const shownEmail = supportEmail.trim() || "your support email"
  return (
    <figure className="min-w-0">
      <figcaption className="mb-2 text-[13px] leading-5 font-medium">On your checkout (example)</figcaption>
      <div aria-hidden="true" className="overflow-hidden rounded-xl border border-border-strong bg-background/60 select-none">
        <div className="flex h-8 items-center justify-center gap-1.5 border-b border-border text-faint">
          <LockIcon className="size-3" />
          <span className="num text-[11px] leading-none">{host}</span>
        </div>

        <div className="p-4">
          <div className="flex items-center justify-between gap-2">
            <div className="flex min-w-0 items-center gap-2">
              <MerchantAvatar
                name={name || "?"}
                logoUrl={logoUrl}
                className={cn("size-6 text-[11px]", !name.trim() && !logoUrl && "bg-muted text-faint")}
              />
              <span className={cn("truncate text-[13px] leading-5 font-medium", !name.trim() && "text-faint")}>{shownName}</span>
            </div>
            <PaymentStatusBadge status="awaiting" size="sm" live={false} />
          </div>

          {/* An example order, so the layout can be judged; not data of this instance. */}
          <p className="mt-5 text-[13px] leading-5 font-medium">Example order</p>
          <p className="text-xs leading-4 text-muted-foreground">
            Order <span className="num">EXAMPLE-1</span>
          </p>
          <Amount
            value="149.00"
            asset
            className="mt-3 block text-[28px] leading-8 font-medium tracking-[-0.055em] [&_[data-slot=amount-symbol]]:text-[0.45em] [&_[data-slot=amount-symbol]]:tracking-normal"
          />
          <NetworkBadge asset size="sm" className="mt-2.5" />
        </div>

        <div className="flex items-baseline justify-between gap-3 border-t border-border px-4 py-2.5 text-xs leading-4">
          <span className="shrink-0 text-muted-foreground">Help</span>
          <span className={cn("truncate", supportEmail.trim() ? "text-foreground" : "text-faint")}>{shownEmail}</span>
        </div>
      </div>
    </figure>
  )
}
