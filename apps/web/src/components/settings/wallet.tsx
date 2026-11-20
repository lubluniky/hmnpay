"use client"

import * as React from "react"
import { ArrowDownIcon, ArrowRightIcon, EyeIcon, LockKeyholeIcon, RefreshCwIcon, ShieldAlertIcon, TriangleAlertIcon } from "lucide-react"
import { toast } from "sonner"

import { Callout, CopyButton, DetailList, DetailRow, ErrorState, SettingsSection, StatusBadge, type Tone } from "@/components/hmn"
import { useDashboard } from "@/components/shell/session"
import { checkXpub, DERIVATION_PATHS, type DerivationPath } from "@/components/setup/data"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Select, SelectContent, SelectItem, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { api } from "@/lib/api/client"
import type { DerivedAddress } from "@/lib/api/types"
import { useQuery } from "@/lib/api/use-query"
import { describeError, fieldErrors } from "@/lib/errors"
import { truncateMiddle } from "@/lib/format"
import { cn } from "@/lib/utils"

import { BlockLabel, Button, IconTile, initialParam, Input, SelectTrigger, stripParams } from "./shared"

/**
 * Settings → Wallet. hmnpay is watch-only: it holds an extended public key, derives one deposit
 * address per payment from it, and watches those addresses. It never holds a key that can
 * spend, and it moves no funds: what customers pay sits on addresses of the operator's own
 * wallet from the moment it arrives. This page shows exactly that — the key, the path, how many
 * addresses were handed out, and the first addresses to compare with the wallet.
 */
export function WalletSettings() {
  // `?replace=1` opens the dialog.
  const [replaceOpen, setReplaceOpen] = React.useState(() => initialParam("replace") === "1")
  const [indexOpen, setIndexOpen] = React.useState(false)
  return (
    <>
      <CustodySection />
      <DepositWalletSection onReplace={() => setReplaceOpen(true)} onSetIndex={() => setIndexOpen(true)} />
      <NextIndexDialog open={indexOpen} onOpenChange={setIndexOpen} />
      <ReplaceDialog
        open={replaceOpen}
        onOpenChange={(open) => {
          setReplaceOpen(open)
          if (!open) stripParams("replace")
        }}
      />
    </>
  )
}

/* -------------------------------------------------------------------------------------------
 * Custody: where money goes, what lives here, and the worst case
 * ----------------------------------------------------------------------------------------- */

function CustodySection() {
  const { settings } = useDashboard()
  const { wallet, asset } = settings
  const steps = [
    {
      label: "Customer pays",
      value: <span className="text-[15px] leading-6 font-medium">A fresh address</span>,
      caption: (
        <>
          One per payment, derived from your xpub. Next is <span className="num text-foreground">#{wallet.next_address_index}</span>.
        </>
      ),
    },
    {
      label: "Funds arrive",
      value: <span className="text-[15px] leading-6 font-medium">In your own wallet</span>,
      caption: `The ${asset.symbol} sits on that address, which only your wallet’s keys can spend from. hmnpay watches it; it does not move it.`,
    },
  ]

  return (
    <SettingsSection
      title="Custody"
      description="hmnpay is non-custodial and watch-only. Your seed phrase never touches this server, and nothing here can spend."
    >
      <ol className="grid rounded-lg border border-border bg-background/60 md:grid-cols-2">
        {steps.map((step, index) => (
          <li key={step.label} className={cn("relative min-w-0 p-4", index > 0 && "border-t border-border md:border-t-0 md:border-l")}>
            {index > 0 && (
              <span
                aria-hidden="true"
                className="absolute -top-[11px] left-4 flex size-[22px] items-center justify-center rounded-full border border-border bg-card text-muted-foreground md:top-1/2 md:-left-[11px] md:-translate-y-1/2"
              >
                <ArrowDownIcon className="size-3 md:hidden" />
                <ArrowRightIcon className="hidden size-3 md:block" />
              </span>
            )}
            <div className="flex items-center gap-2 text-[13px] leading-5 text-muted-foreground">
              <span className="num text-xs text-faint">{index + 1}</span>
              {step.label}
            </div>
            <div className="mt-1.5 flex min-h-6 items-center">{step.value}</div>
            <p className="mt-1 text-[13px] leading-5 text-pretty text-muted-foreground">{step.caption}</p>
          </li>
        ))}
      </ol>

      <div>
        <BlockLabel>Kept on this server</BlockLabel>
        <ul className="mt-1 divide-y divide-border">
          <KeyRow icon={<EyeIcon />} name="Extended public key" badge={{ tone: "neutral", label: "Watch-only" }} id={truncateMiddle(wallet.xpub, 8, 4)}>
            Derives deposit addresses. It can’t sign, so it can’t move funds.
          </KeyRow>
        </ul>
      </div>

      <div>
        <BlockLabel>Never on this server</BlockLabel>
        <ul className="mt-1">
          <KeyRow icon={<LockKeyholeIcon />} name="Seed phrase and private keys" badge={{ tone: "success", label: "Yours alone" }}>
            They stay in your own wallet. To move what customers paid, use that wallet: every deposit address is one of its addresses.
          </KeyRow>
        </ul>
      </div>

      <Callout icon={<ShieldAlertIcon />} title="If this server is compromised">
        An attacker cannot spend anything: there is no spending key here. What they could do is replace the public key, so that new
        payments go to their wallet. That is why changing it asks you to compare addresses, and why the first address below is worth a
        look now and then.
      </Callout>
    </SettingsSection>
  )
}

function KeyRow({
  icon,
  name,
  badge,
  id,
  children,
}: {
  icon: React.ReactNode
  name: string
  badge: { tone: Tone; label: string; hollow?: boolean }
  id?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <li className="flex gap-3 py-3.5 last:pb-0">
      <IconTile>{icon}</IconTile>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
          <span className="text-sm leading-5 font-medium">{name}</span>
          <StatusBadge tone={badge.tone} size="sm" hollow={badge.hollow}>
            {badge.label}
          </StatusBadge>
          {id && <span className="num ml-auto hidden text-xs text-faint sm:block">{id}</span>}
        </div>
        <p className="mt-1 max-w-[60ch] text-[13px] leading-5 text-pretty text-muted-foreground">{children}</p>
      </div>
    </li>
  )
}

/* -------------------------------------------------------------------------------------------
 * The deposit wallet: key, path, addresses handed out, first-address check
 * ----------------------------------------------------------------------------------------- */

/** An address laid out for comparing by eye: complete, with the first and last characters at full strength. */
function CheckAddress({ value }: { value: string }) {
  return (
    <span className="num min-w-0 flex-1 text-[13px] leading-5 whitespace-nowrap" title={value}>
      <span className="font-medium text-foreground">{value.slice(0, 6)}</span>
      <span className="text-muted-foreground max-[520px]:hidden">{value.slice(6, -4)}</span>
      <span className="text-muted-foreground min-[521px]:hidden">
        {value.slice(6, 14)}…{value.slice(-10, -4)}
      </span>
      <span className="font-medium text-foreground">{value.slice(-4)}</span>
    </span>
  )
}

function AddressList({ addresses }: { addresses: DerivedAddress[] }) {
  return (
    <ol className="divide-y divide-border rounded-lg border border-border bg-background/60" data-slot="derived-addresses">
      {addresses.map(({ index, address, path }) => (
        <li key={address} data-derived={index} className="flex min-h-10 items-center gap-3 py-0.5 pr-2 pl-4 pointer-coarse:min-h-11">
          <span className="num w-5 shrink-0 text-xs text-faint" title={path}>
            /{index}
          </span>
          <CheckAddress value={address} />
          <CopyButton value={address} label={`Copy address ${index}`} className="pointer-coarse:size-10" />
        </li>
      ))}
    </ol>
  )
}

function DepositWalletSection({ onReplace, onSetIndex }: { onReplace: () => void; onSetIndex: () => void }) {
  const { settings } = useDashboard()
  const { wallet } = settings
  // The first addresses of the configured key, derived by the server (the same call the setup wizard uses).
  const first = useQuery(`wallet/first/${wallet.xpub}/${wallet.derivation_path}`, (signal) =>
    api.previewAddresses({ xpub: wallet.xpub, derivation_path: wallet.derivation_path, count: 3 }, undefined, signal),
  )

  return (
    <SettingsSection
      title="Deposit wallet"
      description="The key deposit addresses are derived from. Every payment gets the next address; an address is never used twice."
      footer="Changing the key affects new payments only. Open payments keep their addresses and stay watched."
      actions={
        <Button size="sm" variant="outline" onClick={onReplace}>
          <RefreshCwIcon data-icon="inline-start" />
          Replace key
        </Button>
      }
    >
      <DetailList className="-my-2 text-[13px]">
        <DetailRow label="Extended public key" className="min-h-10">
          <span className="num min-w-0 truncate" title={wallet.xpub} data-slot="xpub">
            {truncateMiddle(wallet.xpub, 12, 8)}
          </span>
          <CopyButton value={wallet.xpub} label="Copy extended public key" size="icon-xs" className="-mr-1.5 ml-1 pointer-coarse:size-10" />
        </DetailRow>
        <DetailRow label="Derivation path" className="min-h-10">
          <span className="num">{wallet.derivation_path}/n</span>
        </DetailRow>
        <DetailRow label="Next address index" className="min-h-10">
          <span className="num" data-slot="next-address-index" title={`${wallet.derivation_path}/${wallet.next_address_index}`}>
            {wallet.next_address_index.toLocaleString("en-US")}
          </span>
          <Button size="xs" variant="ghost" className="-mr-2 ml-1.5 text-muted-foreground pointer-coarse:h-10" onClick={onSetIndex}>
            Change
          </Button>
        </DetailRow>
      </DetailList>

      <div className="flex flex-col gap-2.5">
        <BlockLabel>Check these against your wallet</BlockLabel>
        {first.data ? (
          <AddressList addresses={first.data.data} />
        ) : first.error ? (
          <ErrorState error={first.error} title="Couldn’t derive the addresses" onRetry={first.reload} bordered={false} className="py-6" />
        ) : (
          <div className="space-y-2" role="status" aria-label="Deriving addresses">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        )}
        <p className="text-[13px] leading-5 text-pretty text-muted-foreground">
          Address <span className="num text-foreground">/0</span> must be the first receiving address your own wallet shows for this
          account. If it is not, payments are going to a wallet that is not yours: replace the key.
        </p>
      </div>
    </SettingsSection>
  )
}

/* -------------------------------------------------------------------------------------------
 * Dialog: replace the key
 * ----------------------------------------------------------------------------------------- */

function ReplaceDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const { settings, setSettings } = useDashboard()
  const [xpub, setXpub] = React.useState("")
  const [derivation, setDerivation] = React.useState<DerivationPath>("bip44")
  const [verified, setVerified] = React.useState(false)
  const [dropped, setDropped] = React.useState(false)
  const [busy, setBusy] = React.useState(false)
  const [failure, setFailure] = React.useState<string | null>(null)
  const shape = checkXpub(xpub)
  const path = DERIVATION_PATHS[derivation].path
  const same = xpub === settings.wallet.xpub && path === settings.wallet.derivation_path

  const preview = useQuery(open && shape.ok ? `wallet/preview/${derivation}/${xpub}` : null, (signal) =>
    api.previewAddresses({ xpub, derivation_path: path, count: 3 }, undefined, signal),
  )
  const previewError = preview.error ? (fieldErrors(preview.error).xpub ?? fieldErrors(preview.error).derivation_path ?? describeError(preview.error)) : null
  const shapeError = dropped
    ? "That was a private key, so it was removed and never left this page. Paste the public key instead. It starts with “xpub”."
    : !shape.ok && shape.reason !== "empty"
      ? shape.message
      : null

  const close = (next: boolean) => {
    onOpenChange(next)
    if (!next) {
      window.setTimeout(() => {
        setXpub("")
        setVerified(false)
        setDropped(false)
        setFailure(null)
      }, 150)
    }
  }

  const replace = async () => {
    setBusy(true)
    setFailure(null)
    try {
      setSettings(await api.updateWallet({ xpub, derivation_path: path }))
      toast.success("Deposit wallet replaced", { description: "New payments use the new key, starting at address 0." })
      close(false)
    } catch (error) {
      const fields = fieldErrors(error)
      setFailure(fields.xpub ?? fields.derivation_path ?? describeError(error))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <form
          className="contents"
          onSubmit={(event) => {
            event.preventDefault()
            if (!busy && shape.ok && verified && preview.data && !same) void replace()
          }}
        >
          <DialogHeader>
            <DialogTitle>Replace the deposit key</DialogTitle>
            <DialogDescription>
              New payments will use addresses of the wallet this key belongs to. Payments that are open now keep their addresses and stay
              watched.
            </DialogDescription>
          </DialogHeader>

          <Field data-invalid={shapeError || previewError || failure ? true : undefined}>
            <FieldLabel htmlFor="wallet-xpub">Extended public key</FieldLabel>
            <textarea
              id="wallet-xpub"
              rows={2}
              autoComplete="off"
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              placeholder="xpub6…"
              value={xpub}
              aria-invalid={shapeError || previewError || failure ? true : undefined}
              onChange={(event) => {
                const value = event.target.value.replace(/\s+/g, "")
                const isPrivate = /^[xyz]prv/i.test(value)
                setDropped(isPrivate)
                setXpub(isPrivate ? "" : value)
                setVerified(false)
                setFailure(null)
              }}
              className="field-sizing-content min-h-[60px] w-full resize-none rounded-lg border border-input bg-transparent px-3 py-2.5 font-mono text-[13px] leading-5 break-all outline-none transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 aria-invalid:border-destructive dark:bg-input/30"
            />
            {(shapeError || previewError || failure) && (
              <FieldDescription className="text-[13px] text-danger-fg" role="alert">
                {shapeError ?? previewError ?? failure}
              </FieldDescription>
            )}
            {same && <FieldDescription className="text-[13px]">This is the key that is already in use.</FieldDescription>}
          </Field>

          <Field>
            <FieldLabel>Derivation path</FieldLabel>
            <Select
              value={derivation}
              onValueChange={(value) => {
                setDerivation(value as DerivationPath)
                setVerified(false)
              }}
            >
              <SelectTrigger className="w-full" aria-label="Derivation path">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {(Object.keys(DERIVATION_PATHS) as DerivationPath[]).map((key) => (
                  <SelectItem key={key} value={key}>
                    {DERIVATION_PATHS[key].label}
                    <span className="num ml-2 text-xs text-muted-foreground">{DERIVATION_PATHS[key].path}/n</span>
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>

          {shape.ok && !previewError && (
            <div className="flex flex-col gap-2.5">
              <BlockLabel>Check these against your wallet</BlockLabel>
              {preview.data ? (
                <AddressList addresses={preview.data.data} />
              ) : (
                <p className="flex min-h-[120px] items-center justify-center gap-2.5 rounded-lg border border-border text-[13px] leading-5 text-muted-foreground" role="status">
                  <Spinner className="size-3.5" />
                  Deriving the first addresses on the server
                </p>
              )}
              <label className="flex min-h-11 cursor-pointer items-center gap-3 rounded-lg border border-border bg-muted/40 px-4 py-2.5 text-sm leading-5">
                <Checkbox checked={verified} disabled={!preview.data} onCheckedChange={(value) => setVerified(value === true)} />
                <span className="font-medium">These match the first addresses in my wallet</span>
              </label>
            </div>
          )}

          <Callout tone="warning" icon={<TriangleAlertIcon />}>
            If the addresses are not yours, the money customers pay is not yours either. Compare them in your wallet before you confirm.
          </Callout>

          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline" type="button">
                Cancel
              </Button>
            </DialogClose>
            <Button type="submit" disabled={busy || !shape.ok || !verified || !preview.data || same}>
              {busy ? "Replacing…" : "Replace key"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/* -------------------------------------------------------------------------------------------
 * Dialog: set the next address index
 * ----------------------------------------------------------------------------------------- */

/** The server refuses an index at or above 2^31: child keys from there on are hardened and cannot be derived from a public key. */
const INDEX_LIMIT = 2 ** 31
/** The address preview derives from index 0 and gives at most this many. */
const PREVIEW_MAX = 20

/**
 * Where the next payment's address comes from. Normally nobody touches this: every payment
 * takes the next index. It is for the two cases where the counter and the wallet disagree — the
 * same key was used elsewhere and the first addresses are taken (move forward), or an instance
 * was restored and the counter is ahead (move back).
 *
 * What the server guarantees is narrow and the dialog says exactly that: an address that a
 * payment of this instance has is never handed out again, whatever the index says. It knows
 * nothing about addresses used anywhere else.
 */
function NextIndexDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const { settings, setSettings } = useDashboard()
  const { wallet } = settings
  const current = wallet.next_address_index
  const [text, setText] = React.useState("")
  const [confirmed, setConfirmed] = React.useState(false)
  const [busy, setBusy] = React.useState(false)
  const [failure, setFailure] = React.useState<string | null>(null)

  const digits = /^\d{1,10}$/.test(text)
  const value = digits ? Number(text) : null
  const tooLarge = value !== null && value >= INDEX_LIMIT
  const usable = value !== null && !tooLarge && value !== current
  const back = usable && value < current
  const forward = usable && value > current

  // The address at the chosen index, when the preview reaches that far.
  const preview = useQuery(
    open && usable && value < PREVIEW_MAX ? `wallet/index-preview/${wallet.xpub}/${wallet.derivation_path}/${value}` : null,
    (signal) => api.previewAddresses({ xpub: wallet.xpub, derivation_path: wallet.derivation_path, count: value! + 1 }, undefined, signal),
  )
  const address = usable ? preview.data?.data.find((item) => item.index === value)?.address : undefined

  const close = (next: boolean) => {
    onOpenChange(next)
    if (!next) {
      window.setTimeout(() => {
        setText("")
        setConfirmed(false)
        setFailure(null)
      }, 150)
    }
  }

  const save = async () => {
    if (value === null) return
    setBusy(true)
    setFailure(null)
    try {
      setSettings(await api.updateWallet({ xpub: wallet.xpub, derivation_path: wallet.derivation_path, next_address_index: value }))
      toast.success(`Next address index set to ${value.toLocaleString("en-US")}`, {
        description: back ? "Addresses this server has already given to a payment are still skipped." : "The addresses in between stay unused.",
      })
      close(false)
    } catch (error) {
      setFailure(fieldErrors(error).next_address_index ?? describeError(error))
    } finally {
      setBusy(false)
    }
  }

  const fieldError = tooLarge ? `The index must be below ${INDEX_LIMIT.toLocaleString("en-US")}.` : text !== "" && !digits ? "Enter a whole number, 0 or more." : failure
  const ready = usable && (!back || confirmed) && !busy

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg" data-slot="next-index-dialog">
        <form
          className="contents"
          onSubmit={(event) => {
            event.preventDefault()
            if (ready) void save()
          }}
        >
          <DialogHeader>
            <DialogTitle>Set the next address index</DialogTitle>
            <DialogDescription>
              The next payment gets the address at this index of your key (<span className="num">{wallet.derivation_path}/n</span>), and
              every payment after it the one that follows. Right now it is <span className="num text-foreground">{current.toLocaleString("en-US")}</span>.
            </DialogDescription>
          </DialogHeader>

          <Field data-invalid={fieldError ? true : undefined}>
            <FieldLabel htmlFor="wallet-next-index">Next address index</FieldLabel>
            <Input
              id="wallet-next-index"
              inputMode="numeric"
              autoComplete="off"
              placeholder={String(current)}
              value={text}
              aria-invalid={fieldError ? true : undefined}
              onChange={(event) => {
                setText(event.target.value.replace(/\D/g, "").slice(0, 10))
                setConfirmed(false)
                setFailure(null)
              }}
              className="num w-48"
            />
            {fieldError ? (
              <FieldDescription className="text-[13px] text-danger-fg" role="alert">
                {fieldError}
              </FieldDescription>
            ) : value === current ? (
              <FieldDescription className="text-[13px]">That is the index in use now.</FieldDescription>
            ) : address ? (
              <FieldDescription className="text-[13px]">
                Address <span className="num text-foreground">/{value}</span> is <span className="num break-all text-foreground" data-slot="index-address">{address}</span>
              </FieldDescription>
            ) : (
              <FieldDescription className="text-[13px]">From 0 to {(INDEX_LIMIT - 1).toLocaleString("en-US")}; the server refuses anything else.</FieldDescription>
            )}
          </Field>

          {back && (
            <>
              <Callout tone="warning" icon={<TriangleAlertIcon />} title="Going back can reuse an address" data-slot="index-warning">
                Addresses <span className="num text-foreground">{value}</span> to <span className="num text-foreground">{current - 1}</span> were
                counted as used. The server still never gives out an address that a payment in this database already has: it skips those. It
                cannot know about anything else. If one of these addresses was used by another installation with the same key, or by payments
                that are no longer in this database, a new customer gets an address someone has paid to before, and money sent to it late
                would be counted for the wrong payment.
              </Callout>
              <label className="flex min-h-11 cursor-pointer items-center gap-3 rounded-lg border border-border bg-muted/40 px-4 py-2.5 text-sm leading-5">
                <Checkbox checked={confirmed} onCheckedChange={(next) => setConfirmed(next === true)} />
                <span className="font-medium">Nothing outside this server has used these addresses</span>
              </label>
            </>
          )}
          {forward && (
            <Callout icon={<ArrowRightIcon />} title={`Skips ${(value - current).toLocaleString("en-US")} ${value - current === 1 ? "address" : "addresses"}`} data-slot="index-note">
              Addresses <span className="num text-foreground">{current}</span> to <span className="num text-foreground">{value - 1}</span> will not be
              given to any payment. Money still arrives in your own wallet; if the wallet app lists addresses one by one, it may need that many
              added before it shows the new ones.
            </Callout>
          )}

          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline" type="button">
                Cancel
              </Button>
            </DialogClose>
            <Button type="submit" disabled={!ready}>
              {busy ? "Saving…" : "Set index"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
