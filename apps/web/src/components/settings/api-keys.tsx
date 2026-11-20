"use client"

import * as React from "react"
import { BookOpenIcon, KeyRoundIcon, MoreHorizontalIcon, PlusIcon, Trash2Icon, TriangleAlertIcon } from "lucide-react"
import { toast } from "sonner"

import {
  Callout,
  CopyButton,
  DetailList,
  DetailRow,
  EmptyState,
  ErrorState,
  PageSection,
  Panel,
  SettingsSection,
  StatusBadge,
  type Tone,
} from "@/components/hmn"
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Select, SelectContent, SelectItem, SelectValue } from "@/components/ui/select"
import { PROJECT_URL } from "@/components/hmn"
import { useDashboard } from "@/components/shell/session"
import { Skeleton } from "@/components/ui/skeleton"
import { api } from "@/lib/api/client"
import { API_KEY_SCOPES, type ApiKey, type ApiKeyScope } from "@/lib/api/types"
import { useQuery } from "@/lib/api/use-query"
import { describeError, fieldErrors } from "@/lib/errors"
import { formatDate, relativeTime } from "@/lib/format"
import { useNow } from "@/lib/use-now"
import { cn } from "@/lib/utils"

import { Button, CodeSnippet, initialParam, Input, MonoTag, SelectTrigger, stripParams } from "./shared"

const DOCS = `${PROJECT_URL}#readme`

/** Human copy for each scope, used by the create-key dialog. What each one allows is the server's rule. */
const API_KEY_SCOPE_INFO: Record<ApiKeyScope, { label: string; description: string }> = {
  "payments:read": { label: "Read payments", description: "List and read payments, events and statistics." },
  "payments:write": { label: "Create payments", description: "Create and cancel payments." },
  "links:write": { label: "Manage payment links", description: "Read, create, edit, disable and delete payment links." },
  "webhooks:manage": {
    label: "Manage webhooks",
    description: "Add and change endpoints and resend deliveries. It can read every payment event, but never a signing secret.",
  },
}

const DAY_MS = 86_400_000
const maskedKey = (key: Pick<ApiKey, "prefix" | "last4">) => `${key.prefix}••••••••${key.last4}`

function keyStatus(key: ApiKey, now: number): { tone: Tone; label: string; hollow?: boolean; note?: string } {
  if (key.revoked_at) return { tone: "neutral", label: "Revoked", hollow: true, note: formatDate(key.revoked_at) }
  if (key.expires_at) {
    const days = Math.ceil((Date.parse(key.expires_at) - now) / DAY_MS)
    if (days <= 0) return { tone: "neutral", label: "Expired", hollow: true, note: formatDate(key.expires_at) }
    if (days <= 14) return { tone: "warning", label: "Expiring", note: `in ${days} day${days === 1 ? "" : "s"}` }
    return { tone: "success", label: "Active", note: `until ${formatDate(key.expires_at)}` }
  }
  return { tone: "success", label: "Active" }
}

/** Settings → API keys. `?new=1` opens the create dialog. */
export function ApiKeysSettings() {
  const list = useQuery("api-keys", (signal) => api.apiKeys(signal), { refreshMs: 30_000 })
  const keys = list.data?.data ?? []
  const [createOpen, setCreateOpen] = React.useState(() => initialParam("new") === "1")
  const [revokeId, setRevokeId] = React.useState<string | null>(null)
  const revoking = keys.find((key) => key.id === revokeId) ?? null
  const now = useNow()

  const closeCreate = (open: boolean) => {
    setCreateOpen(open)
    if (!open) stripParams("new")
  }
  const upsert = (key: ApiKey) =>
    list.mutate((current) =>
      current
        ? { ...current, data: current.data.some((item) => item.id === key.id) ? current.data.map((item) => (item.id === key.id ? key : item)) : [key, ...current.data] }
        : current,
    )

  return (
    <>
      <PageSection
        title="API keys"
        description="Keys let your server create and read payments. Treat them like passwords."
        actions={
          keys.length > 0 && (
            <Button onClick={() => setCreateOpen(true)}>
              <PlusIcon data-icon="inline-start" />
              Create key
            </Button>
          )
        }
      >
        {!list.data ? (
          list.error ? (
            <ErrorState error={list.error} title="Couldn’t load API keys" onRetry={list.reload} />
          ) : (
            <Panel role="status" aria-label="Loading API keys" className="space-y-4 p-5">
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-9 w-full" />
            </Panel>
          )
        ) : keys.length === 0 ? (
          <EmptyState
            icon={<KeyRoundIcon />}
            title="No API keys yet"
            description="Create a key to take payments from your own backend. You can limit what each key is allowed to do."
            action={
              <Button onClick={() => setCreateOpen(true)}>
                <PlusIcon data-icon="inline-start" />
                Create key
              </Button>
            }
            secondaryAction={
              <Button variant="outline" asChild>
                <a href={DOCS} target="_blank" rel="noopener noreferrer">
                  <BookOpenIcon data-icon="inline-start" />
                  Documentation
                </a>
              </Button>
            }
          />
        ) : (
          <Panel>
            <div className={cn("hidden border-b border-border px-4 py-2.5 text-[13px] leading-5 text-muted-foreground sm:px-5", KEY_GRID)}>
              <span>Name</span>
              <span>Permissions</span>
              <span>Last used</span>
              <span>Status</span>
              <span />
            </div>
            <ul className="divide-y divide-border">
              {keys.map((key) => (
                <KeyRow key={key.id} apiKey={key} now={now} onRevoke={() => setRevokeId(key.id)} />
              ))}
            </ul>
          </Panel>
        )}
      </PageSection>

      <QuickStart />

      <CreateKeyDialog
        open={createOpen}
        onOpenChange={closeCreate}
        onCreated={(key) => upsert({ ...key, secret: undefined })}
      />

      <RevokeDialog
        apiKey={revoking}
        now={now}
        onOpenChange={(open) => {
          if (!open) setRevokeId(null)
        }}
        onConfirm={async (key) => {
          try {
            upsert(await api.revokeApiKey(key.id))
            toast(`Revoked “${key.name}”`)
          } catch (error) {
            toast.error("Couldn’t revoke the key", { description: describeError(error) })
            list.reload()
          }
        }}
      />
    </>
  )
}

/* -------------------------------------------------------------------------------------------
 * List
 * ----------------------------------------------------------------------------------------- */

const KEY_GRID = "sm:grid sm:grid-cols-[minmax(0,1fr)_8.25rem_9.25rem_6.75rem_1.75rem] sm:items-center sm:gap-x-4"

function ScopeTags({ scopes }: { scopes: ApiKeyScope[] }) {
  if (scopes.length === API_KEY_SCOPES.length) return <MonoTag className="font-sans tracking-normal">All permissions</MonoTag>
  const shown = scopes.slice(0, 2)
  const rest = scopes.length - shown.length
  return (
    <span className="flex flex-wrap gap-1 sm:flex-col">
      {shown.map((scope) => (
        <MonoTag key={scope}>{scope}</MonoTag>
      ))}
      {rest > 0 && <MonoTag>+{rest}</MonoTag>}
    </span>
  )
}

function KeyRow({ apiKey, now, onRevoke }: { apiKey: ApiKey; now: number; onRevoke: () => void }) {
  const status = keyStatus(apiKey, now)
  const dead = Boolean(apiKey.revoked_at)
  const lastUsed = apiKey.last_used_at ? relativeTime(apiKey.last_used_at, now) : "Never"

  return (
    <li data-key={apiKey.id} className={cn("relative px-4 py-3 sm:px-5", KEY_GRID)}>
      <div className="min-w-0 pr-10 sm:pr-0">
        <div className="flex items-center gap-2">
          <span className={cn("truncate text-sm leading-5 font-medium", dead && "text-muted-foreground")}>{apiKey.name}</span>
          {apiKey.mode === "test" && (
            <StatusBadge tone="neutral" size="sm" dot={false}>
              Test
            </StatusBadge>
          )}
        </div>
        <div className={cn("num mt-0.5 truncate text-xs leading-5 text-muted-foreground", dead && "line-through decoration-faint/60")}>
          {maskedKey(apiKey)}
        </div>
      </div>

      <div className={cn("mt-2 sm:mt-0", dead && "opacity-60")}>
        <ScopeTags scopes={apiKey.scopes} />
      </div>

      <div className="mt-2 flex items-baseline gap-1.5 text-[13px] leading-5 sm:mt-0 sm:block">
        <span className="text-muted-foreground sm:hidden">Used</span>
        <span className={cn(dead && "text-muted-foreground")}>{lastUsed}</span>
        <span className="text-xs text-muted-foreground sm:block">
          <span className="sm:hidden">· </span>
          Created {formatDate(apiKey.created_at)}
        </span>
      </div>

      <div className="mt-2 flex items-center gap-2 sm:mt-0 sm:block">
        <StatusBadge tone={status.tone} hollow={status.hollow} size="sm">
          {status.label}
        </StatusBadge>
        {status.note && <div className="text-xs leading-5 text-muted-foreground sm:mt-0.5">{status.note}</div>}
      </div>

      <div className="absolute top-2.5 right-3 sm:static">
        {!dead && (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={`Actions for ${apiKey.name}`}
                className="text-muted-foreground pointer-coarse:size-9"
              >
                <MoreHorizontalIcon />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-44">
              <DropdownMenuItem variant="destructive" onSelect={onRevoke}>
                <Trash2Icon />
                Revoke
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        )}
      </div>
    </li>
  )
}

/* -------------------------------------------------------------------------------------------
 * Quick start
 * ----------------------------------------------------------------------------------------- */

const quickstartRequest = (base: string) => `curl ${base}/api/v1/payments \\
  -H "Authorization: Bearer $HMNPAY_API_KEY" \\
  -H "Idempotency-Key: ORD-20481" \\
  -H "Content-Type: application/json" \\
  -d '{
    "amount": "149.00",
    "order_id": "ORD-20481",
    "description": "Pro plan, annual"
  }'`

/** The fields of the answer an integration needs first (the full payment has more; see the README). */
const quickstartResponse = (base: string, asset: string, network: string) => `{
  "id": "pay_…",
  "status": "awaiting",
  "amount": "149.00",
  "asset": "${asset}",
  "network": "${network}",
  "address": "0x…",
  "checkout_url": "${base}/pay/pay_…",
  "expires_at": "…"
}`

function QuickStart() {
  const { settings } = useDashboard()
  const base = settings.public_url || (typeof window === "undefined" ? "" : window.location.origin)
  return (
    <SettingsSection
      title="Quick start"
      description="Create a payment from your server, then send the customer to the checkout URL in the response."
      footer={
        <>
          Authenticate every request with <span className="num text-foreground">Authorization: Bearer</span> and your key.
        </>
      }
      actions={
        <Button size="sm" variant="outline" asChild>
          <a href={DOCS} target="_blank" rel="noopener noreferrer">
            <BookOpenIcon data-icon="inline-start" />
            Documentation
          </a>
        </Button>
      }
    >
      <div className="grid gap-3">
        <CodeSnippet code={quickstartRequest(base)} language="bash" title="Request" />
        <CodeSnippet
          code={quickstartResponse(base, settings.asset.symbol, settings.asset.network)}
          language="json"
          title={
            <>
              Response <span className="num ml-1.5 text-success-fg">201</span> <span className="ml-1.5 text-faint">shortened</span>
            </>
          }
        />
      </div>
    </SettingsSection>
  )
}

/* -------------------------------------------------------------------------------------------
 * Dialog: create key → reveal once
 * ----------------------------------------------------------------------------------------- */

const EXPIRY_OPTIONS = [
  { value: "never", label: "Never", days: null },
  { value: "30", label: "30 days", days: 30 },
  { value: "90", label: "90 days", days: 90 },
  { value: "365", label: "1 year", days: 365 },
] as const

function CreateKeyDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onCreated: (key: ApiKey) => void
}) {
  const [name, setName] = React.useState("")
  const [scopes, setScopes] = React.useState<ApiKeyScope[]>(["payments:read", "payments:write"])
  const [expiry, setExpiry] = React.useState<(typeof EXPIRY_OPTIONS)[number]["value"]>("90")
  const [expiresAt, setExpiresAt] = React.useState<string | null>(() => new Date(Date.now() + 90 * DAY_MS).toISOString())
  const [busy, setBusy] = React.useState(false)
  const [errors, setErrors] = React.useState<Record<string, string>>({})
  /** The API answers with the stored key plus its secret, once. It lives here until the dialog closes, and nowhere else. */
  const [created, setCreated] = React.useState<(ApiKey & { secret: string }) | null>(null)
  const reveal = created !== null

  const chooseExpiry = (value: typeof expiry) => {
    setExpiry(value)
    const days = EXPIRY_OPTIONS.find((option) => option.value === value)?.days ?? null
    setExpiresAt(days ? new Date(Date.now() + days * DAY_MS).toISOString() : null)
  }

  const create = async () => {
    setBusy(true)
    setErrors({})
    try {
      const key = await api.createApiKey({ name: name.trim(), scopes, expires_at: expiresAt })
      if (!key.secret) throw new Error("the server did not return the key")
      setCreated({ ...key, secret: key.secret })
      onCreated(key)
    } catch (error) {
      const fields = fieldErrors(error)
      setErrors(Object.keys(fields).length > 0 ? fields : { form: describeError(error) })
    } finally {
      setBusy(false)
    }
  }

  const close = () => {
    onOpenChange(false)
    // Reset after the close animation so the form doesn't flash back. The secret is dropped here.
    window.setTimeout(() => {
      setCreated(null)
      setName("")
      setErrors({})
    }, 150)
  }

  return (
    <Dialog open={open} onOpenChange={(next) => (next ? onOpenChange(true) : close())}>
      <DialogContent
        className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-md"
        showCloseButton={!reveal}
        onInteractOutside={(event) => reveal && event.preventDefault()}
        onEscapeKeyDown={(event) => reveal && event.preventDefault()}
        onOpenAutoFocus={(event) => reveal && event.preventDefault()}
      >
        {created ? (
          <>
            <DialogHeader>
              <DialogTitle>Save your key now</DialogTitle>
              <DialogDescription>
                This is the only time the full key is shown. hmnpay stores a hash of it, not the key itself.
              </DialogDescription>
            </DialogHeader>

            <div className="rounded-lg border border-border bg-background/60">
              <div className="num px-3.5 pt-3 pb-2.5 text-[13px] leading-5 break-all select-all">{created.secret}</div>
              <div className="flex items-center justify-between gap-2 border-t border-border px-2 py-2">
                <CopyButton value={`HMNPAY_API_KEY=${created.secret}`} variant="ghost" className="text-muted-foreground">
                  Copy as .env line
                </CopyButton>
                <CopyButton value={created.secret} variant="default" copiedLabel="Copied">
                  Copy key
                </CopyButton>
              </div>
            </div>

            <Callout tone="warning" icon={<TriangleAlertIcon />}>
              You won’t see this key again. If you lose it, revoke it and create a new one.
            </Callout>

            <DetailList className="-my-2 text-[13px]">
              <DetailRow label="Name" className="min-h-9">
                {created.name}
              </DetailRow>
              <DetailRow label="Permissions" className="min-h-9">
                <span className="flex flex-wrap justify-end gap-1">
                  {created.scopes.map((scope) => (
                    <MonoTag key={scope}>{scope}</MonoTag>
                  ))}
                </span>
              </DetailRow>
              <DetailRow label="Expires" className="min-h-9">
                {created.expires_at ? formatDate(created.expires_at) : "Never"}
              </DetailRow>
            </DetailList>

            <DialogFooter>
              <Button variant="outline" onClick={close}>
                I’ve saved it
              </Button>
            </DialogFooter>
          </>
        ) : (
          <form
            className="contents"
            onSubmit={(event) => {
              event.preventDefault()
              if (!busy) void create()
            }}
          >
            <DialogHeader>
              <DialogTitle>Create API key</DialogTitle>
              <DialogDescription>Give each service its own key, with only the permissions it needs.</DialogDescription>
            </DialogHeader>

            <Field data-invalid={errors.name ? true : undefined}>
              <FieldLabel htmlFor="key-name">Name</FieldLabel>
              <Input
                id="key-name"
                value={name}
                placeholder="Storefront checkout"
                autoComplete="off"
                maxLength={80}
                aria-invalid={errors.name ? true : undefined}
                onChange={(event) => setName(event.target.value)}
              />
              {errors.name && <FieldError>{errors.name}</FieldError>}
            </Field>

            <Field>
              <FieldLabel>Permissions</FieldLabel>
              <ul className="divide-y divide-border rounded-lg border border-border">
                {API_KEY_SCOPES.map((scope) => {
                  const info = API_KEY_SCOPE_INFO[scope]
                  const checked = scopes.includes(scope)
                  return (
                    <li key={scope}>
                      <label className="flex cursor-pointer items-start gap-3 px-3 py-2.5 pointer-coarse:py-3">
                        <Checkbox
                          checked={checked}
                          onCheckedChange={(next) =>
                            setScopes((list) =>
                              next === true ? API_KEY_SCOPES.filter((item) => item === scope || list.includes(item)) : list.filter((item) => item !== scope),
                            )
                          }
                          className="mt-0.5"
                        />
                        <span className="min-w-0 flex-1">
                          <span className="flex flex-wrap items-center justify-between gap-x-2 text-sm leading-5 font-medium">
                            {info.label}
                            <span className="num text-[11px] font-normal text-faint">{scope}</span>
                          </span>
                          <span className="block text-[13px] leading-5 text-muted-foreground">{info.description}</span>
                        </span>
                      </label>
                    </li>
                  )
                })}
              </ul>
            </Field>

            <Field>
              <FieldLabel>Expires</FieldLabel>
              <div className="flex items-center gap-3">
                <Select value={expiry} onValueChange={(value) => chooseExpiry(value as typeof expiry)}>
                  <SelectTrigger className="w-36" aria-label="Expires">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {EXPIRY_OPTIONS.map((option) => (
                      <SelectItem key={option.value} value={option.value}>
                        {option.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <span className="text-[13px] leading-5 text-muted-foreground">
                  {expiresAt ? `Stops working on ${formatDate(expiresAt)}.` : "Works until you revoke it."}
                </span>
              </div>
              {(errors.scopes || errors.expires_at || errors.form) && <FieldError>{errors.scopes ?? errors.expires_at ?? errors.form}</FieldError>}
            </Field>

            <DialogFooter>
              <DialogClose asChild>
                <Button variant="outline" type="button">
                  Cancel
                </Button>
              </DialogClose>
              <Button type="submit" disabled={busy || !name.trim() || scopes.length === 0}>
                {busy ? "Creating…" : "Create key"}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}

/* -------------------------------------------------------------------------------------------
 * Dialog: revoke
 * ----------------------------------------------------------------------------------------- */

function RevokeDialog({
  apiKey,
  now,
  onOpenChange,
  onConfirm,
}: {
  apiKey: ApiKey | null
  now: number
  onOpenChange: (open: boolean) => void
  onConfirm: (key: ApiKey) => Promise<void>
}) {
  const [busy, setBusy] = React.useState(false)
  // Keep the last key around so the content doesn't empty out during the close animation.
  const [shown, setShown] = React.useState(apiKey)
  if (apiKey && apiKey !== shown) setShown(apiKey)
  const key = apiKey ?? shown
  const recentlyUsed = key?.last_used_at ? now - Date.parse(key.last_used_at) < DAY_MS : false

  return (
    <AlertDialog open={apiKey !== null} onOpenChange={onOpenChange}>
      <AlertDialogContent className="data-[size=default]:max-w-[calc(100%-2rem)] data-[size=default]:sm:max-w-md">
        {key && (
          <>
            <AlertDialogHeader className="place-items-start text-left">
              <AlertDialogTitle>Revoke “{key.name}”?</AlertDialogTitle>
              <AlertDialogDescription className="text-pretty">
                Requests using <span className="num whitespace-nowrap text-foreground">{maskedKey(key)}</span> start failing
                with <span className="num text-foreground">401</span> straight away. A revoked key can’t be restored.
              </AlertDialogDescription>
            </AlertDialogHeader>
            {recentlyUsed && key.last_used_at && (
              <Callout tone="warning" icon={<TriangleAlertIcon />} title={`Last used ${relativeTime(key.last_used_at, now)}`}>
                Something is still calling the API with this key. Switch it to a new key first, or it will stop working.
              </Callout>
            )}
            <AlertDialogFooter>
              <AlertDialogCancel className="pointer-coarse:h-10">Cancel</AlertDialogCancel>
              <Button
                variant="destructive"
                disabled={busy}
                onClick={async () => {
                  setBusy(true)
                  await onConfirm(key)
                  setBusy(false)
                  onOpenChange(false)
                }}
              >
                {busy ? "Revoking…" : "Revoke key"}
              </Button>
            </AlertDialogFooter>
          </>
        )}
      </AlertDialogContent>
    </AlertDialog>
  )
}
