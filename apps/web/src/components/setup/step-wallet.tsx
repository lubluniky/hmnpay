"use client"

import * as React from "react"
import { ArrowRightIcon, EyeIcon, KeyRoundIcon } from "lucide-react"

import { DOCS_URL, FieldMessage, FlowField, FlowLink, flowControl, useAutoFocus } from "@/components/auth/flow"
import { CopyButton } from "@/components/hmn"
import { Checkbox } from "@/components/ui/checkbox"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { cn } from "@/lib/utils"

import { Spinner } from "@/components/ui/spinner"

import { checkXpub, DERIVATION_PATHS, type DerivationPath, type SetupForm } from "./data"
import { StepHeader } from "./parts"
import type { Derived } from "./wizard"

export function StepWallet({
  form,
  set,
  showErrors,
  derived,
  errors,
}: {
  form: SetupForm
  set: (patch: Partial<SetupForm>) => void
  showErrors: boolean
  /** The addresses the server derived for the key and path on screen; null before it was asked. */
  derived: Derived | null
  /** What the server said about fields of this step. */
  errors: Record<string, string>
}) {
  const xpubRef = useAutoFocus<HTMLTextAreaElement>(!form.xpub)
  const xpub = checkXpub(form.xpub)
  // A private key is never kept: it is dropped from the field the moment it is recognised.
  const [privateKeyDropped, setPrivateKeyDropped] = React.useState(false)
  // A wrong key is reported as soon as it is pasted; an empty field only after trying to continue.
  const xpubError = privateKeyDropped
    ? "That was a private key, so it was removed and never left this page. Paste the public key instead. It starts with “xpub”."
    : xpub.ok
      ? (errors.xpub ?? errors.derivation_path ?? (derived?.status === "error" ? (derived.error ?? undefined) : undefined))
      : xpub.reason === "empty"
        ? showErrors
          ? "Paste your wallet’s extended public key."
          : undefined
        : xpub.message

  return (
    <>
      <StepHeader title="Connect your wallet">
        Paste your wallet’s extended public key. The private keys stay where they are.
      </StepHeader>

      <Custody className="mt-6" />

      <FlowField
        className="mt-5"
        label="Extended public key"
        htmlFor="setup-xpub"
        error={xpubError}
        action={
          <FlowLink href={DOCS_URL} target="_blank" rel="noopener noreferrer">
            Where do I find it?
          </FlowLink>
        }
      >
        <textarea
          ref={xpubRef}
          id="setup-xpub"
          name="xpub"
          rows={2}
          autoComplete="off"
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          placeholder="xpub6…"
          value={form.xpub}
          onChange={(e) => {
            const value = e.target.value.replace(/\s+/g, "")
            const isPrivate = checkXpub(value).ok === false && /^[xyz]prv/i.test(value)
            setPrivateKeyDropped(isPrivate)
            set({ xpub: isPrivate ? "" : value, addressesVerified: false })
          }}
          aria-invalid={xpubError ? true : undefined}
          aria-describedby={xpubError ? "setup-xpub-message" : undefined}
          className={cn(
            flowControl,
            "field-sizing-content h-auto min-h-[60px] resize-none py-2.5 font-mono text-[15px] leading-5 tracking-[-0.01em] break-all md:text-[13px]",
          )}
        />
      </FlowField>

      <DerivedAddresses
        className="mt-4"
        ready={xpub.ok}
        derived={derived}
        derivation={form.derivation}
        verified={form.addressesVerified}
        nudge={showErrors && xpub.ok && !form.addressesVerified}
        onDerivation={(derivation) => set({ derivation, addressesVerified: false })}
        onVerified={(addressesVerified) => set({ addressesVerified })}
      />

    </>
  )
}

/* -------------------------------- custody --------------------------------- */

/**
 * Who holds what, as one object split in two: the keys stay on the left,
 * only the public key crosses the line.
 */
function Custody({ className }: { className?: string }) {
  return (
    <div
      className={cn(
        "relative grid overflow-hidden rounded-xl border border-border-strong bg-background/60 sm:grid-cols-2",
        className,
      )}
    >
      <CustodySide icon={<KeyRoundIcon />} title="Your wallet" lines={["Keeps the private keys", "Receives every payment directly"]} />
      <CustodySide
        icon={<EyeIcon />}
        title="hmnpay"
        lines={["Derives deposit addresses", "Sees payments, can’t spend them"]}
        className="border-t border-border sm:border-t-0 sm:border-l sm:pl-11"
      />
      <span
        aria-hidden="true"
        className="num absolute top-1/2 left-1/2 flex h-6 -translate-x-1/2 -translate-y-1/2 items-center gap-1 rounded-full border border-border-strong bg-card px-2 text-[11px] leading-none text-muted-foreground max-sm:rotate-0"
      >
        xpub
        <ArrowRightIcon className="size-3 max-sm:rotate-90" />
      </span>
    </div>
  )
}

function CustodySide({
  icon,
  title,
  lines,
  className,
}: {
  icon: React.ReactNode
  title: string
  lines: string[]
  className?: string
}) {
  return (
    <div className={cn("flex gap-3 px-4 py-3", className)}>
      <span className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg border border-border bg-card text-muted-foreground [&_svg]:size-4">
        {icon}
      </span>
      <div className="min-w-0">
        <div className="text-sm leading-5 font-medium">{title}</div>
        <ul className="mt-0.5 text-[13px] leading-5 text-muted-foreground">
          {lines.map((line) => (
            <li key={line}>{line}</li>
          ))}
        </ul>
      </div>
    </div>
  )
}

/* ---------------------------- derived addresses --------------------------- */

function DerivedAddresses({
  ready,
  derived,
  derivation,
  verified,
  nudge,
  onDerivation,
  onVerified,
  className,
}: {
  ready: boolean
  derived: Derived | null
  derivation: DerivationPath
  verified: boolean
  nudge: boolean
  onDerivation: (value: DerivationPath) => void
  onVerified: (value: boolean) => void
  className?: string
}) {
  const path = DERIVATION_PATHS[derivation].path

  if (!ready || derived?.status === "error") {
    return (
      <div
        className={cn(
          "flex min-h-[96px] flex-col items-center justify-center rounded-xl border border-dashed border-border-strong px-6 py-5 text-center",
          className,
        )}
      >
        <p className="text-sm leading-5 font-medium">Your first addresses appear here</p>
        <p className="mt-1 max-w-sm text-[13px] leading-5 text-pretty text-muted-foreground">
          You’ll compare them with your wallet before anything is saved.
        </p>
      </div>
    )
  }

  return (
    <section
      aria-labelledby="derived-title"
      className={cn(
        "overflow-hidden rounded-xl border bg-background/60 transition-colors motion-safe:animate-rise",
        nudge ? "border-warning-border" : "border-border-strong",
        className,
      )}
    >
      <header className="flex min-h-11 flex-wrap items-center justify-between gap-x-4 gap-y-2 border-b border-border py-2 pr-2 pl-4">
        <h2 id="derived-title" className="min-w-0 text-sm leading-5 font-medium">
          Check these against your wallet
        </h2>
        <Select value={derivation} onValueChange={(value) => onDerivation(value as DerivationPath)}>
          <SelectTrigger size="sm" aria-label="Derivation path" className="num text-xs text-muted-foreground">
            <SelectValue>{path}/n</SelectValue>
          </SelectTrigger>
          <SelectContent align="end" className="min-w-64">
            {(Object.keys(DERIVATION_PATHS) as DerivationPath[]).map((key) => (
              <SelectItem key={key} value={key}>
                <span className="flex flex-col">
                  <span>
                    {DERIVATION_PATHS[key].label}
                    <span className="num ml-2 text-xs text-muted-foreground">{DERIVATION_PATHS[key].path}/n</span>
                  </span>
                  <span className="text-xs text-muted-foreground">{DERIVATION_PATHS[key].wallets}</span>
                </span>
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </header>

      <ol className="divide-y divide-border">
        {derived?.status !== "ok" && (
          <li className="flex min-h-[120px] items-center justify-center gap-2.5 text-[13px] leading-5 text-muted-foreground" role="status">
            <Spinner className="size-3.5" />
            Deriving the first addresses on the server
          </li>
        )}
        {derived?.status === "ok" &&
          derived.addresses.map(({ index, address, path: full }) => (
            <li key={address} data-derived={index} className="flex min-h-10 items-center gap-3 py-0.5 pr-2 pl-4 pointer-coarse:min-h-11">
              <span className="num w-5 shrink-0 text-xs text-faint" title={full}>
                /{index}
              </span>
              <CheckAddress value={address} className="min-w-0 flex-1" />
              <CopyButton value={address} label={`Copy address ${index}`} className="pointer-coarse:size-10" />
            </li>
          ))}
      </ol>

      <label
        className={cn(
          "flex min-h-11 cursor-pointer items-center gap-3 border-t px-4 py-2.5 text-sm leading-5 transition-colors",
          nudge ? "border-warning-border bg-warning-subtle" : "border-border bg-muted/40",
        )}
      >
        <Checkbox
          checked={verified}
          disabled={derived?.status !== "ok"}
          onCheckedChange={(value) => onVerified(value === true)}
          aria-describedby={nudge ? "verify-nudge" : undefined}
        />
        <span className="font-medium">These match the first addresses in my wallet</span>
      </label>
      {nudge && (
        <FieldMessage id="verify-nudge" className="bg-warning-subtle px-4 pb-3 pl-11 text-muted-foreground">
          Compare the addresses first: address /0 here must be the first address your wallet shows. If they differ, try the other
          derivation path. Payments must land in a wallet you control.
        </FieldMessage>
      )}
    </section>
  )
}

/**
 * An address laid out for comparing by eye: complete, with the first and last characters
 * at full strength (what people actually check). Shortened on narrow screens.
 */
function CheckAddress({ value, className }: { value: string; className?: string }) {
  const head = value.slice(0, 6)
  const tail = value.slice(-4)
  const middle = value.slice(6, -4)
  return (
    <span className={cn("num text-[13px] leading-5 whitespace-nowrap", className)} title={value}>
      <span className="font-medium text-foreground">{head}</span>
      <span className="text-muted-foreground max-[520px]:hidden">{middle}</span>
      <span className="text-muted-foreground min-[521px]:hidden">
        {middle.slice(0, 8)}…{middle.slice(-6)}
      </span>
      <span className="font-medium text-foreground">{tail}</span>
    </span>
  )
}
