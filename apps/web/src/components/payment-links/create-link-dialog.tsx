"use client"

import * as React from "react"

import { DrawnCheck } from "@/components/checkout/parts"
import { Amount, AssetIcon, NETWORK_NAME, useWaveEntrance, WaveTransition } from "@/components/hmn"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { InputGroup, InputGroupAddon, InputGroupInput, InputGroupText } from "@/components/ui/input-group"
import { Switch } from "@/components/ui/switch"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import type { PaymentLink } from "@/lib/api/types"
import { cn } from "@/lib/utils"

import { AMOUNT_FORMAT_HINT } from "@/lib/money"

import { amountTyping, linkParts, slugify, validateDraft, type DraftErrors, type LinkDraft } from "./data"
import { LinkShare } from "./link-share"

/** Dashboard controls are 32px; on touch screens they grow to a tappable 40px. */
const touch = "pointer-coarse:h-10 placeholder:text-faint"

export function CreateLinkDialog({
  open,
  onOpenChange,
  draft,
  onDraft,
  created,
  editing = null,
  onCreate,
  onAnother,
  base,
  today,
  decimals,
  asset,
  serverErrors,
  failure,
  busy,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  draft: LinkDraft
  onDraft: (draft: LinkDraft) => void
  /** Set once the link exists: the dialog switches to its share state. */
  created: PaymentLink | null
  /**
   * The link being edited, or null when the dialog creates one. Editing shows the same form
   * filled in, without the link's ending (it cannot change) and without the share state;
   * `onCreate` then saves the changes.
   */
  editing?: PaymentLink | null
  onCreate: () => void
  onAnother: () => void
  /** "https://pay.example.com/l" */
  base: string
  /** YYYY-MM-DD, for the expiry field's minimum. */
  today: string
  decimals: number
  asset: string
  /** What the server said about the last attempt, by field. Cleared by the parent on edit. */
  serverErrors: DraftErrors
  /** A refusal that belongs to no field. */
  failure: string | null
  busy: boolean
}) {
  const [submitted, setSubmitted] = React.useState(false)
  const original = editing ?? undefined
  const errors: DraftErrors = { ...(submitted ? validateDraft(draft, today, decimals, original) : {}), ...serverErrors }
  // Text that is not an amount is answered while it is typed (the fields keep it as typed).
  const typing = amountTyping(draft, decimals)
  const set = (patch: Partial<LinkDraft>) => onDraft({ ...draft, ...patch })
  const prefix = linkParts(`${base}/x`).base

  const submit = (event: React.FormEvent) => {
    event.preventDefault()
    if (busy) return
    if (Object.keys(validateDraft(draft, today, decimals, original)).length > 0) {
      setSubmitted(true)
      return
    }
    setSubmitted(false)
    onCreate()
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="flex max-h-[calc(100svh-2rem)] flex-col gap-0 overflow-hidden p-0 shadow-overlay sm:max-w-[520px]"
        onOpenAutoFocus={(event) => {
          const target = (event.currentTarget as HTMLElement | null)?.querySelector<HTMLElement>("[data-autofocus]")
          if (target) {
            event.preventDefault()
            target.focus()
          }
        }}
      >
        {/* Creating the link sends a wave up from the button; "Create another" just swaps back. */}
        <WaveTransition
          revealed={created !== null && editing === null}
          cue="created"
          className="min-h-0 rounded-[inherit]"
          layerClassName="flex flex-col"
          revealedContent={created && <LinkCreated link={created} onAnother={onAnother} onDone={() => onOpenChange(false)} />}
        >
          <form noValidate onSubmit={submit} className="flex min-h-0 flex-col">
            <DialogHeader className="gap-1.5 p-5 pb-4 sm:p-6 sm:pb-5">
              <DialogTitle className="text-base leading-6 font-semibold">{editing ? "Edit payment link" : "Create payment link"}</DialogTitle>
              <DialogDescription>
                {editing
                  ? "Changes apply to checkouts opened from now on. Payments already made or in progress keep their amount."
                  : "A hosted checkout page you can share. No code needed."}
              </DialogDescription>
            </DialogHeader>

            <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto px-5 pb-5 *:shrink-0 sm:px-6 sm:pb-6">
              <Field label="Name" htmlFor="link-name" error={errors.name}>
                <Input
                  id="link-name"
                  autoFocus
                  autoComplete="off"
                  placeholder="Pro plan — annual"
                  maxLength={64}
                  value={draft.name}
                  onChange={(e) =>
                    set({ name: e.target.value, ...(draft.slugEdited ? {} : { slug: slugify(e.target.value) }) })
                  }
                  aria-invalid={errors.name ? true : undefined}
                  className={touch}
                />
              </Field>

              <Field
                label="Amount"
                htmlFor="link-amount"
                error={(draft.pricing === "fixed" ? errors.amount : errors.minAmount) ?? typing[draft.pricing === "fixed" ? "amount" : "minAmount"]}
                hint={
                  draft.pricing === "open"
                    ? `The customer types the amount on the link’s page. Good for donations, tips and top-ups. The least they can pay is 1 ${asset} unless you set a higher minimum. ${AMOUNT_FORMAT_HINT}`
                    : AMOUNT_FORMAT_HINT
                }
              >
                <Tabs value={draft.pricing} onValueChange={(value) => set({ pricing: value as LinkDraft["pricing"] })}>
                  <TabsList className={cn("w-full", "group-data-horizontal/tabs:pointer-coarse:h-10")}>
                    <TabsTrigger value="fixed">Fixed amount</TabsTrigger>
                    <TabsTrigger value="open">Customer chooses</TabsTrigger>
                  </TabsList>
                </Tabs>
                {draft.pricing === "fixed" && (
                  <InputGroup className={touch}>
                    <InputGroupAddon>
                      <AssetIcon size={16} />
                    </InputGroupAddon>
                    <InputGroupInput
                      id="link-amount"
                      inputMode="decimal"
                      autoComplete="off"
                      placeholder="0.00"
                      value={draft.amount}
                      maxLength={40}
                      onChange={(e) => set({ amount: e.target.value })}
                      aria-invalid={errors.amount ? true : undefined}
                      className="num placeholder:text-faint"
                    />
                    <InputGroupAddon align="inline-end">
                      <InputGroupText className="text-[13px] font-normal">
                        {asset} on {NETWORK_NAME}
                      </InputGroupText>
                    </InputGroupAddon>
                  </InputGroup>
                )}
                {draft.pricing === "open" && (
                  <InputGroup className={touch}>
                    <InputGroupAddon>
                      <InputGroupText className="text-[13px] font-normal">Minimum</InputGroupText>
                    </InputGroupAddon>
                    <InputGroupInput
                      id="link-min-amount"
                      inputMode="decimal"
                      autoComplete="off"
                      aria-label="Minimum amount"
                      placeholder="1.00"
                      value={draft.minAmount}
                      maxLength={40}
                      onChange={(e) => set({ minAmount: e.target.value })}
                      aria-invalid={errors.minAmount ? true : undefined}
                      className="num placeholder:text-faint"
                    />
                    <InputGroupAddon align="inline-end">
                      <InputGroupText className="text-[13px] font-normal">{asset}</InputGroupText>
                    </InputGroupAddon>
                  </InputGroup>
                )}
              </Field>

              <Field
                label="Link"
                htmlFor="link-slug"
                error={errors.slug}
                hint={editing ? "The address of a link never changes, so links already shared and printed QR codes keep working." : undefined}
              >
                <InputGroup className={cn(touch, editing && "bg-muted/40")}>
                  <InputGroupAddon>
                    <InputGroupText className="num text-[13px] font-normal">{prefix}</InputGroupText>
                  </InputGroupAddon>
                  <InputGroupInput
                    id="link-slug"
                    autoComplete="off"
                    autoCapitalize="none"
                    spellCheck={false}
                    placeholder="pro-annual"
                    readOnly={editing !== null}
                    value={draft.slug}
                    onChange={(e) => set({ slug: slugify(e.target.value.replace(/\s+/g, "-")) || "", slugEdited: true })}
                    aria-invalid={errors.slug ? true : undefined}
                    className={cn("num !pl-0 placeholder:text-faint", editing && "text-muted-foreground")}
                  />
                </InputGroup>
              </Field>

              <Field label="Description" htmlFor="link-description" optional>
                <Textarea
                  id="link-description"
                  rows={2}
                  maxLength={160}
                  placeholder="Shown under the name on the checkout."
                  value={draft.description}
                  onChange={(e) => set({ description: e.target.value })}
                  className="min-h-[52px] resize-none placeholder:text-faint"
                />
              </Field>

              <div className="divide-y divide-border rounded-lg border border-border">
                <Option
                  id="link-redirect"
                  label="Redirect after payment"
                  hint="Otherwise customers stay on the receipt."
                  checked={draft.redirect}
                  onChange={(redirect) => set({ redirect })}
                  error={errors.redirectUrl}
                >
                  <Input
                    id="link-redirect-url"
                    type="url"
                    inputMode="url"
                    aria-label="Redirect URL"
                    placeholder="https://yoursite.com/thanks"
                    value={draft.redirectUrl}
                    onChange={(e) => set({ redirectUrl: e.target.value })}
                    aria-invalid={errors.redirectUrl ? true : undefined}
                    className={touch}
                  />
                </Option>
                <Option
                  id="link-limit"
                  label="Limit payments"
                  hint={
                    editing && editing.uses > 0
                      ? `For tickets, seats or limited stock. ${editing.uses.toLocaleString("en-US")} ${editing.uses === 1 ? "payment has" : "payments have"} been made already and count towards the limit.`
                      : "For tickets, seats or limited stock. Each checkout of a limited link holds a place for 10 minutes."
                  }
                  checked={draft.limit}
                  onChange={(limit) => set({ limit })}
                  error={errors.maxUses}
                >
                  <InputGroup className={cn("w-40", touch)}>
                    <InputGroupInput
                      id="link-max-uses"
                      inputMode="numeric"
                      aria-label="Maximum payments"
                      placeholder="50"
                      value={draft.maxUses}
                      onChange={(e) => set({ maxUses: e.target.value.replace(/\D/g, "") })}
                      aria-invalid={errors.maxUses ? true : undefined}
                      className="num placeholder:text-faint"
                    />
                    <InputGroupAddon align="inline-end">
                      <InputGroupText className="text-[13px] font-normal">payments</InputGroupText>
                    </InputGroupAddon>
                  </InputGroup>
                </Option>
                <Option
                  id="link-expires"
                  label="Set an end date"
                  hint="Closes at the end of that day, UTC."
                  checked={draft.expires}
                  onChange={(expires) => set({ expires })}
                  error={errors.expiresOn}
                >
                  <Input
                    id="link-expires-on"
                    type="date"
                    aria-label="Last day"
                    min={today}
                    value={draft.expiresOn}
                    onChange={(e) => set({ expiresOn: e.target.value })}
                    aria-invalid={errors.expiresOn ? true : undefined}
                    className={cn("num w-44", touch)}
                  />
                </Option>
              </div>
            </div>

            {failure && (
              <p role="alert" className="border-t border-danger-border bg-danger-subtle px-5 py-2.5 text-[13px] leading-5 text-danger-fg sm:px-6">
                {failure}
              </p>
            )}
            <div className="flex flex-col-reverse gap-2 border-t border-border bg-muted/40 px-5 py-3.5 sm:flex-row sm:justify-end sm:px-6">
              <Button type="button" variant="outline" className={touch} onClick={() => onOpenChange(false)}>
                Cancel
              </Button>
              <Button type="submit" className={touch} data-wave-source="" disabled={busy}>
                {editing ? (busy ? "Saving…" : "Save changes") : busy ? "Creating…" : "Create link"}
              </Button>
            </div>
          </form>
        </WaveTransition>
      </DialogContent>
    </Dialog>
  )
}

/* --------------------------------- pieces --------------------------------- */

/** The share state: the link exists, here is everything needed to hand it over. */
function LinkCreated({ link, onAnother, onDone }: { link: PaymentLink; onAnother: () => void; onDone: () => void }) {
  return (
    <>
      <DialogHeader className="gap-1.5 p-5 pb-0 sm:p-6 sm:pb-0">
        <CreatedMark />
        <DialogTitle className="text-base leading-6 font-semibold">Link created</DialogTitle>
        <DialogDescription className="text-pretty">
          <span className="font-medium text-foreground">{link.name}</span>
          {link.amount ? (
            <>
              {" "}
              for <Amount value={link.amount} asset={link.asset} className="text-foreground [&_[data-slot=amount-symbol]]:text-foreground" />
            </>
          ) : (
            ", any amount"
          )}
          . Put it behind a button, in an email or in a chat. It is live now.
        </DialogDescription>
      </DialogHeader>
      <div className="p-5 sm:p-6">
        <LinkShare
          url={link.url}
          slug={link.slug}
          qrSize={128}
          focusCopy
          className="rounded-xl border border-border bg-background/60 p-4"
        />
      </div>
      <div className="mt-auto flex flex-col-reverse gap-2 border-t border-border bg-muted/40 px-5 py-3.5 sm:flex-row sm:justify-end sm:px-6">
        <Button type="button" variant="ghost" className={touch} onClick={onAnother}>
          Create another
        </Button>
        <Button type="button" variant="outline" className={touch} onClick={onDone}>
          Done
        </Button>
      </div>
    </>
  )
}

/** The green check of the "link created" state. It pops in, unless a wave has just carried it in. */
function CreatedMark() {
  const still = useWaveEntrance()
  return (
    <span
      aria-hidden="true"
      className={cn(
        "mb-2.5 flex size-9 items-center justify-center rounded-full bg-success text-background",
        !still && "motion-safe:animate-pop-in",
      )}
    >
      <DrawnCheck className="size-[18px]" />
    </span>
  )
}

function Field({
  label,
  htmlFor,
  optional = false,
  hint,
  error,
  children,
}: {
  label: string
  htmlFor: string
  optional?: boolean
  hint?: React.ReactNode
  error?: string
  children: React.ReactNode
}) {
  return (
    <div className="flex flex-col gap-2">
      <label htmlFor={htmlFor} className="text-[13px] leading-5 font-medium">
        {label}
        {optional && <span className="ml-1.5 font-normal text-faint">Optional</span>}
      </label>
      {children}
      {error ? (
        <p role="alert" className="text-[13px] leading-5 text-danger-fg">
          {error}
        </p>
      ) : (
        hint && <p className="text-[13px] leading-5 text-pretty text-muted-foreground">{hint}</p>
      )}
    </div>
  )
}

/** A switch row that reveals its one input when turned on. */
function Option({
  id,
  label,
  hint,
  checked,
  onChange,
  error,
  children,
}: {
  id: string
  label: string
  hint: string
  checked: boolean
  onChange: (checked: boolean) => void
  error?: string
  children: React.ReactNode
}) {
  return (
    <div className="px-3.5 py-3">
      <div className="flex items-center justify-between gap-4">
        <label htmlFor={id} className="min-w-0 cursor-pointer">
          <span className="block text-[13px] leading-5 font-medium">{label}</span>
          <span className="block text-[13px] leading-5 text-muted-foreground">{hint}</span>
        </label>
        <Switch id={id} checked={checked} onCheckedChange={onChange} />
      </div>
      {checked && (
        <div className="mt-2.5 flex flex-col gap-1.5">
          {children}
          {error && (
            <p role="alert" className="text-[13px] leading-5 text-danger-fg">
              {error}
            </p>
          )}
        </div>
      )}
    </div>
  )
}
