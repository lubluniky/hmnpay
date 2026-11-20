"use client"

import * as React from "react"
import Link from "next/link"
import {
  BookOpenIcon,
  ChevronRightIcon,
  EyeIcon,
  EyeOffIcon,
  PlusIcon,
  RotateCwIcon,
  SendIcon,
  TriangleAlertIcon,
  WebhookIcon,
} from "lucide-react"
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
  StatusDot,
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
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { PROJECT_URL } from "@/components/hmn"
import { Skeleton } from "@/components/ui/skeleton"
import { api } from "@/lib/api/client"
import { WEBHOOK_EVENT_TYPES, type Delivery as WebhookDelivery, type WebhookEndpoint, type WebhookEventType, type WebhookSecret } from "@/lib/api/types"
import { useQuery } from "@/lib/api/use-query"
import { describeError, fieldErrors } from "@/lib/errors"
import { formatDate, formatDateTime, formatTime, relativeTime } from "@/lib/format"
import { useNow } from "@/lib/use-now"
import { cn } from "@/lib/utils"

import { BlockLabel, Button, CodeSnippet, initialParam, Input, MonoTag, Segmented, stripParams } from "./shared"
import { deliveryRequestHead, deliverySeq, endpointName, WEBHOOK_EVENT_INFO, WEBHOOK_POLICY } from "./webhooks-data"

const DOCS = `${PROJECT_URL}#receiving-webhooks`

const DEFAULT_EVENTS: WebhookEventType[] = ["payment.paid", "payment.underpaid", "payment.overpaid", "payment.expired", "payment.failed"]

const hostOf = (url: string) => url.replace(/^https?:\/\//, "").split("/")[0]
const pathOf = (url: string) => {
  const rest = url.replace(/^https?:\/\//, "")
  const slash = rest.indexOf("/")
  return slash === -1 ? "" : rest.slice(slash)
}
/** A share of deliveries as "98.4" (a statistic of counts, not an amount). */
const percent = (rate: number) => <span className="num">{(rate * 100).toFixed(1)}</span>

type EndpointHealth = { tone: Tone; label: string; hollow?: boolean }

/** An endpoint whose last attempt failed and that is still switched on. */
const isFailing = (endpoint: WebhookEndpoint) => endpoint.enabled && endpoint.failing_since !== null

function endpointHealth(endpoint: WebhookEndpoint): EndpointHealth {
  if (!endpoint.enabled) return { tone: "neutral", label: "Disabled", hollow: true }
  if (isFailing(endpoint)) return { tone: "danger", label: "Failing" }
  return { tone: "success", label: "Active" }
}

/** List responses carry a mask; create and rotate answer with the secret itself, which never goes into the list. */
const maskSecret = (secret: string) => (secret.includes("…") ? secret : `whsec_…${secret.slice(-4)}`)

function deliveryMeta(delivery: WebhookDelivery): { tone: Tone; code: string; label: string; hollow?: boolean } {
  const code = delivery.response_code !== null ? String(delivery.response_code) : delivery.attempt === 0 ? "Queued" : "No reply"
  switch (delivery.status) {
    case "succeeded":
      return { tone: "success", code, label: "Delivered" }
    case "failed":
      return { tone: "danger", code, label: "Failed" }
    case "retrying":
      return { tone: "warning", code, label: "Retrying" }
    default:
      return { tone: "neutral", code: "Queued", label: "Queued", hollow: true }
  }
}

const DELIVERY_PAGE = 10

/**
 * Settings → Webhooks. `?new=1` opens the add-endpoint dialog, `?endpoint=<id>` an endpoint's
 * sheet and `?delivery=<id>` a delivery's.
 */
export function WebhooksSettings() {
  const list = useQuery("webhook-endpoints", (signal) => api.endpoints(signal), { refreshMs: 15_000 })
  const endpoints = React.useMemo(() => list.data?.data ?? [], [list.data])
  const [addOpen, setAddOpen] = React.useState(() => initialParam("new") === "1")
  const [endpointId, setEndpointId] = React.useState<string | null>(() => initialParam("endpoint"))
  const [deliveryId, setDeliveryId] = React.useState<string | null>(() => initialParam("delivery"))
  const [filter, setFilterState] = React.useState<{ endpointId: string | null; status: "all" | "failed" }>({ endpointId: null, status: "all" })
  const [limit, setLimit] = React.useState(DELIVERY_PAGE)
  const setFilter = (next: typeof filter) => {
    setFilterState(next)
    setLimit(DELIVERY_PAGE)
  }
  const now = useNow()

  // The delivery log: the API filters and counts; "Show more" asks for a longer page.
  const scopeId = filter.endpointId ?? undefined
  const deliveries = useQuery(
    `webhook-deliveries/${JSON.stringify([filter, limit])}`,
    (signal) => api.deliveries({ endpoint_id: scopeId, status: filter.status === "failed" ? "failed,retrying" : undefined, limit }, signal),
    { refreshMs: 5000, keepPrevious: true },
  )
  const counts = useQuery(
    `webhook-deliveries/counts/${scopeId ?? ""}`,
    async (signal) => {
      const [all, failed] = await Promise.all([
        api.deliveries({ endpoint_id: scopeId, limit: 1 }, signal),
        api.deliveries({ endpoint_id: scopeId, status: "failed,retrying", limit: 1 }, signal),
      ])
      return { all: all.page?.total ?? 0, failed: failed.page?.total ?? 0 }
    },
    { refreshMs: 10_000, keepPrevious: true },
  )
  const reloadAll = () => {
    deliveries.reload()
    counts.reload()
    list.reload()
  }

  const byId = React.useMemo(() => new Map(endpoints.map((endpoint) => [endpoint.id, endpoint])), [endpoints])
  const failing = endpoints.find(isFailing) ?? null
  /** Put an endpoint the server just returned into the list (never with its secret unmasked). */
  const upsert = (endpoint: WebhookEndpoint) =>
    list.mutate((current) => {
      if (!current) return current
      // Create, rotate and update answer without the 7-day figures: keep the ones already shown.
      const known = current.data.find((item) => item.id === endpoint.id)
      const merged = { ...known, ...endpoint, secret: maskSecret(endpoint.secret), secret_masked: true }
      return { ...current, data: known ? current.data.map((item) => (item.id === endpoint.id ? merged : item)) : [...current.data, merged] }
    })

  const sendTest = async (endpoint: WebhookEndpoint) => {
    try {
      const delivery = await api.testEndpoint(endpoint.id)
      toast.success("Test event queued", { description: "A webhook.test event is on its way. This is its delivery." })
      reloadAll()
      setEndpointId(null)
      setDeliveryId(delivery.id)
    } catch (error) {
      toast.error("Couldn’t send a test event", { description: describeError(error) })
    }
  }

  if (!list.data) {
    return (
      <PageSection title="Webhooks" description="hmnpay calls your server whenever a payment changes state.">
        {list.error ? (
          <ErrorState error={list.error} title="Couldn’t load webhooks" onRetry={list.reload} />
        ) : (
          <Panel role="status" aria-label="Loading webhooks" className="space-y-4 p-5">
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
          </Panel>
        )}
      </PageSection>
    )
  }

  // Keyed: the dialog that adds the first endpoint is still showing its secret when the page
  // around it changes from "no endpoints yet" to the list, and must stay the same dialog.
  const addDialog = (
    <AddEndpointDialog
      key="add-endpoint"
      open={addOpen}
      onOpenChange={(open) => {
        setAddOpen(open)
        if (!open) stripParams("new")
      }}
      onAdded={upsert}
    />
  )

  if (endpoints.length === 0) {
    return (
      <>
        <PageSection title="Webhooks" description="hmnpay calls your server whenever a payment changes state.">
          <EmptyState
            icon={<WebhookIcon />}
            title="No endpoints yet"
            description="Add a URL on your server and hmnpay will POST a signed event to it the moment a payment is paid, expires or fails."
            action={
              <Button onClick={() => setAddOpen(true)}>
                <PlusIcon data-icon="inline-start" />
                Add endpoint
              </Button>
            }
            secondaryAction={
              <Button variant="outline" asChild>
                <a href={DOCS} target="_blank" rel="noopener noreferrer">
                  <BookOpenIcon data-icon="inline-start" />
                  Webhook guide
                </a>
              </Button>
            }
          />
        </PageSection>
        <ExampleEvent />
        {addDialog}
      </>
    )
  }

  return (
    <>
      {failing && (
        <Callout tone="danger" icon={<TriangleAlertIcon />} title={`“${endpointName(failing)}” is failing`}>
          <span className="num whitespace-nowrap text-foreground">{hostOf(failing.url)}</span> has failed the last{" "}
          <span className="num text-foreground">{failing.consecutive_failures}</span> {failing.consecutive_failures === 1 ? "attempt" : "attempts"}
          {failing.failing_since && <>, since {formatDateTime(failing.failing_since)}</>}. hmnpay keeps retrying
          {failing.disable_at && (
            <>
              {" "}
              and will disable the endpoint on <span className="text-foreground">{formatDateTime(failing.disable_at)}</span> if nothing gets
              through
            </>
          )}
          . Missed events are kept for {WEBHOOK_POLICY.retentionDays} days and can be resent.
          <span className="mt-2.5 flex flex-wrap gap-2">
            <Button variant="outline" size="sm" onClick={() => setFilter({ endpointId: failing.id, status: "failed" })}>
              Show failed deliveries
            </Button>
            <Button variant="outline" size="sm" onClick={() => void sendTest(failing)}>
              <SendIcon data-icon="inline-start" />
              Send test event
            </Button>
          </span>
        </Callout>
      )}

      <PageSection
        title="Endpoints"
        description="hmnpay POSTs a signed event to these URLs whenever a payment changes state."
        actions={
          <Button onClick={() => setAddOpen(true)}>
            <PlusIcon data-icon="inline-start" />
            Add<span className="max-sm:sr-only"> endpoint</span>
          </Button>
        }
      >
        <Panel>
          <div className={cn("hidden border-b border-border px-4 py-2.5 text-[13px] leading-5 text-muted-foreground sm:px-5", ENDPOINT_GRID)}>
            <span>URL</span>
            <span>Events</span>
            <span>Delivered, 7 days</span>
            <span>Status</span>
            <span />
          </div>
          <ul className="divide-y divide-border">
            {endpoints.map((endpoint) => (
              <EndpointRow
                key={endpoint.id}
                endpoint={endpoint}
                health={endpointHealth(endpoint)}
                now={now}
                onOpen={() => setEndpointId(endpoint.id)}
              />
            ))}
          </ul>
        </Panel>
      </PageSection>

      <DeliveriesSection
        deliveries={deliveries.data?.data}
        total={deliveries.data?.page?.total ?? 0}
        counts={counts.data}
        error={deliveries.error}
        onRetry={deliveries.reload}
        onMore={() => setLimit((n) => Math.min(200, n + DELIVERY_PAGE))}
        endpoints={byId}
        filter={filter}
        now={now}
        onFilterChange={setFilter}
        onOpen={setDeliveryId}
      />

      {addDialog}

      <EndpointSheet
        endpoint={endpointId ? (byId.get(endpointId) ?? null) : null}
        now={now}
        onOpenChange={(open) => {
          if (!open) {
            setEndpointId(null)
            stripParams("endpoint")
          }
        }}
        onChanged={upsert}
        onDeleted={(endpoint) => {
          list.mutate((current) => (current ? { ...current, data: current.data.filter((item) => item.id !== endpoint.id) } : current))
          setEndpointId(null)
          reloadAll()
        }}
        onSendTest={sendTest}
        onShowDeliveries={(endpoint) => {
          setFilter({ endpointId: endpoint.id, status: "all" })
          setEndpointId(null)
          stripParams("endpoint")
        }}
      />

      <DeliverySheet
        deliveryId={deliveryId}
        listed={deliveries.data?.data}
        endpoints={byId}
        now={now}
        onResent={reloadAll}
        onOpenChange={(open) => {
          if (!open) {
            setDeliveryId(null)
            stripParams("delivery")
          }
        }}
      />
    </>
  )
}

/* -------------------------------------------------------------------------------------------
 * Endpoints list
 * ----------------------------------------------------------------------------------------- */

const ENDPOINT_GRID = "sm:grid sm:grid-cols-[minmax(0,1fr)_7rem_8.5rem_5.5rem_1rem] sm:items-center sm:gap-x-4"

function EventsSummary({ events }: { events: readonly string[] }) {
  if (events.length === WEBHOOK_EVENT_TYPES.length) return <span>All events</span>
  if (events.length === 1) return <MonoTag>{events[0]}</MonoTag>
  return (
    <span>
      <span className="num">{events.length}</span> events
    </span>
  )
}

function EndpointRow({ endpoint, health, now, onOpen }: { endpoint: WebhookEndpoint; health: EndpointHealth; now: number; onOpen: () => void }) {
  const off = !endpoint.enabled
  // The share of the last 7 days' deliveries that got through; absent when there were none.
  const rate = endpoint.success_rate
  const rateTone: Tone = rate === undefined || rate >= 0.97 ? "neutral" : rate >= 0.9 ? "warning" : "danger"

  return (
    <li data-endpoint={endpoint.id}>
      <button
        type="button"
        onClick={onOpen}
        aria-label={`Open ${endpointName(endpoint)}`}
        className={cn(
          "focus-ring relative block w-full px-4 py-3 text-left transition-colors hover:bg-muted/40 focus-visible:-outline-offset-2 sm:px-5",
          ENDPOINT_GRID,
        )}
      >
        <span className="block min-w-0 pr-24 sm:pr-0">
          <span className={cn("block truncate text-sm leading-5 font-medium", off && "text-muted-foreground")}>{endpointName(endpoint)}</span>
          <span className="num mt-0.5 block truncate text-xs leading-5 text-muted-foreground" title={endpoint.url}>
            {hostOf(endpoint.url)}
            <span className="text-faint">{pathOf(endpoint.url)}</span>
          </span>
        </span>

        <span className="mt-2 flex flex-wrap items-center gap-x-2 gap-y-1 text-[13px] leading-5 text-muted-foreground sm:mt-0 sm:contents">
          <span className="sm:block">
            <EventsSummary events={endpoint.events} />
          </span>
          <span className="text-faint sm:hidden">·</span>
          <span className="sm:block">
            {rate === undefined || !endpoint.last_delivery_at ? (
              <span className="text-faint">No deliveries</span>
            ) : (
              <>
                <span className={cn("text-foreground", rateTone === "warning" && "text-warning-fg", rateTone === "danger" && "text-danger-fg")}>
                  {percent(rate)}%
                </span>
                <span className="sm:block sm:text-xs">
                  <span className="sm:hidden"> · </span>last sent {relativeTime(endpoint.last_delivery_at, now)}
                </span>
              </>
            )}
          </span>
        </span>

        <span className="absolute top-3 right-9 sm:static">
          <StatusBadge tone={health.tone} hollow={health.hollow} size="sm">
            {health.label}
          </StatusBadge>
        </span>
        <ChevronRightIcon className="absolute top-3.5 right-3.5 size-4 text-faint sm:static" aria-hidden="true" />
      </button>
    </li>
  )
}

/* -------------------------------------------------------------------------------------------
 * Delivery log
 * ----------------------------------------------------------------------------------------- */

const DELIVERY_GRID = "sm:grid sm:grid-cols-[4.75rem_minmax(0,1fr)_9.5rem_3.25rem_4.25rem_7rem] sm:items-center sm:gap-x-4"

function DeliveriesSection({
  deliveries,
  total,
  counts,
  error,
  onRetry,
  onMore,
  endpoints,
  filter,
  now,
  onFilterChange,
  onOpen,
}: {
  /** The page of deliveries the API returned for the filter; undefined while the first one loads. */
  deliveries: WebhookDelivery[] | undefined
  /** How many deliveries match the filter in all. */
  total: number
  counts: { all: number; failed: number } | undefined
  error: unknown
  onRetry: () => void
  onMore: () => void
  endpoints: Map<string, WebhookEndpoint>
  filter: { endpointId: string | null; status: "all" | "failed" }
  now: number
  onFilterChange: (filter: { endpointId: string | null; status: "all" | "failed" }) => void
  onOpen: (id: string) => void
}) {
  const visible = deliveries ?? []
  const endpoint = filter.endpointId ? endpoints.get(filter.endpointId) : undefined

  return (
    <PageSection
      title="Deliveries"
      description={
        endpoint ? (
          <>
            To “{endpointName(endpoint)}”.{" "}
            <button
              type="button"
              className="focus-ring rounded-sm text-foreground underline decoration-border-strong underline-offset-4 hover:decoration-foreground"
              onClick={() => onFilterChange({ endpointId: null, status: filter.status })}
            >
              Show all endpoints
            </button>
          </>
        ) : (
          "Every event sent to your endpoints, newest first."
        )
      }
      actions={
        <Segmented
          aria-label="Filter deliveries"
          value={filter.status}
          onValueChange={(status) => onFilterChange({ ...filter, status })}
          options={[
            { value: "all", label: "All", count: counts?.all },
            { value: "failed", label: "Failed", count: counts?.failed },
          ]}
        />
      }
    >
      <Panel>
        <div className={cn("hidden border-b border-border px-4 py-2.5 text-[13px] leading-5 text-muted-foreground sm:px-5", DELIVERY_GRID)}>
          <span>Response</span>
          <span>Event</span>
          <span>Endpoint</span>
          <span>Try</span>
          <span className="text-right">Took</span>
          <span className="text-right">Sent</span>
        </div>
        {deliveries === undefined ? (
          error ? (
            <ErrorState error={error} title="Couldn’t load deliveries" onRetry={onRetry} bordered={false} className="py-10" />
          ) : (
            <div className="space-y-3 p-5" role="status" aria-label="Loading deliveries">
              <Skeleton className="h-5 w-full" />
              <Skeleton className="h-5 w-full" />
              <Skeleton className="h-5 w-full" />
            </div>
          )
        ) : visible.length === 0 ? (
          <EmptyState
            bordered={false}
            className="py-10"
            title={filter.status === "failed" ? "No failed deliveries" : "Nothing sent yet"}
            description={
              filter.status === "failed"
                ? "Every event in this view reached its endpoint."
                : "Deliveries show up here as soon as a payment changes state."
            }
          />
        ) : (
          <ul className="divide-y divide-border" data-slot="deliveries">
            {visible.map((delivery) => (
              <DeliveryRow key={delivery.id} delivery={delivery} endpoint={endpoints.get(delivery.endpoint_id)} now={now} onOpen={() => onOpen(delivery.id)} />
            ))}
          </ul>
        )}
        <div className="flex min-h-11 flex-wrap items-center justify-between gap-x-4 gap-y-2 border-t border-border bg-muted/40 px-4 py-2 text-[13px] leading-5 text-muted-foreground sm:px-5">
          <span>
            Showing <span className="num text-foreground">{visible.length}</span> of <span className="num text-foreground">{total}</span>. Events are
            kept for {WEBHOOK_POLICY.retentionDays} days.
          </span>
          {total > visible.length && visible.length < 200 && (
            <Button variant="outline" size="sm" onClick={onMore}>
              Show more
            </Button>
          )}
        </div>
      </Panel>
    </PageSection>
  )
}

/**
 * The response of a delivery. In the log (`quiet`) a success is just a dot and the code, so
 * the rows that need attention — retries and failures — are the only pills on the page.
 */
function ResponseBadge({ delivery, size = "sm", quiet = false }: { delivery: WebhookDelivery; size?: "sm" | "md"; quiet?: boolean }) {
  const meta = deliveryMeta(delivery)
  const numeric = /^\d+$/.test(meta.code)
  if (quiet && delivery.status === "succeeded") {
    return (
      <span className="num inline-flex h-5 items-center gap-1.5 pl-[9px] text-xs font-medium text-muted-foreground">
        <StatusDot tone="success" />
        {meta.code}
      </span>
    )
  }
  return (
    <StatusBadge tone={meta.tone} hollow={meta.hollow} size={size} className={cn(numeric && "num")}>
      {meta.code}
    </StatusBadge>
  )
}

function Duration({ ms }: { ms: number }) {
  const timeout = ms >= WEBHOOK_POLICY.timeoutMs
  return (
    <span className="whitespace-nowrap">
      <span className="num">{timeout ? WEBHOOK_POLICY.timeoutMs / 1000 : ms}</span> {timeout ? "s" : "ms"}
    </span>
  )
}

function DeliveryRow({
  delivery,
  endpoint,
  now,
  onOpen,
}: {
  delivery: WebhookDelivery
  endpoint: WebhookEndpoint | undefined
  now: number
  onOpen: () => void
}) {
  const sent = relativeTime(delivery.created_at, now)
  const took = delivery.attempt === 0 ? null : <Duration ms={delivery.duration_ms} />
  const retried = delivery.attempt > 1
  return (
    <li data-delivery={delivery.id} data-status={delivery.status}>
      <button
        type="button"
        onClick={onOpen}
        aria-label={`Delivery of ${delivery.event_type}, ${deliveryMeta(delivery).label}`}
        className={cn(
          "focus-ring block w-full px-4 py-2.5 text-left text-[13px] leading-5 transition-colors hover:bg-muted/40 focus-visible:-outline-offset-2 sm:min-h-11 sm:px-5 sm:py-2",
          DELIVERY_GRID,
        )}
      >
        <span className="flex items-center justify-between gap-3 sm:contents">
          <span className="flex min-w-0 items-center gap-2.5 sm:contents">
            <span>
              <ResponseBadge delivery={delivery} quiet />
            </span>
            <span className="num min-w-0 truncate">{delivery.event_type}</span>
          </span>
          <span className="shrink-0 text-muted-foreground sm:hidden">{sent}</span>
        </span>
        <span className="mt-1 flex flex-wrap items-center gap-x-1.5 text-muted-foreground sm:mt-0 sm:contents">
          <span className="min-w-0 truncate">{endpoint ? endpointName(endpoint) : hostOf(delivery.url)}</span>
          <span className="text-faint sm:hidden">·</span>
          <span className={cn("num", retried ? "text-foreground" : "sm:text-faint")}>
            <span className="font-sans sm:hidden">try </span>
            {delivery.attempt}
            <span className={cn(retried && "text-muted-foreground")}>/{delivery.max_attempts}</span>
          </span>
          {took && <span className="text-faint sm:hidden">·</span>}
          <span className="sm:text-right">{took ?? <span className="text-faint max-sm:hidden">—</span>}</span>
        </span>
        <span className="hidden text-right text-muted-foreground sm:block" title={formatDateTime(delivery.created_at)}>
          {sent}
        </span>
      </button>
    </li>
  )
}

/* -------------------------------------------------------------------------------------------
 * Event picker (add dialog + endpoint sheet)
 * ----------------------------------------------------------------------------------------- */

function EventPicker({
  value,
  onChange,
  className,
}: {
  value: WebhookEventType[]
  onChange: (next: WebhookEventType[]) => void
  className?: string
}) {
  const all = value.length === WEBHOOK_EVENT_TYPES.length
  return (
    <div className={cn("rounded-lg border border-border", className)}>
      <div className="flex items-center justify-between gap-3 border-b border-border px-3 py-2 text-[13px] leading-5 text-muted-foreground">
        <span>
          <span className="num text-foreground">{value.length}</span> of <span className="num">{WEBHOOK_EVENT_TYPES.length}</span> selected
        </span>
        <button
          type="button"
          className="focus-ring rounded-sm font-medium text-foreground hover:underline hover:underline-offset-4"
          onClick={() => onChange(all ? [] : [...WEBHOOK_EVENT_TYPES])}
        >
          {all ? "Clear" : "Select all"}
        </button>
      </div>
      <ul className="divide-y divide-border">
        {WEBHOOK_EVENT_TYPES.map((event) => {
          const checked = value.includes(event)
          return (
            <li key={event}>
              <label className="flex cursor-pointer items-start gap-3 px-3 py-2 pointer-coarse:py-2.5">
                <Checkbox
                  checked={checked}
                  onCheckedChange={(next) =>
                    onChange(next === true ? WEBHOOK_EVENT_TYPES.filter((item) => item === event || value.includes(item)) : value.filter((item) => item !== event))
                  }
                  className="mt-0.5"
                />
                <span className="min-w-0 flex-1 sm:grid sm:grid-cols-[10.25rem_minmax(0,1fr)] sm:items-baseline sm:gap-x-3">
                  <span className="num block text-[13px] leading-5">{event}</span>
                  <span className="block text-xs leading-5 text-muted-foreground sm:truncate" title={WEBHOOK_EVENT_INFO[event]}>
                    {WEBHOOK_EVENT_INFO[event]}
                  </span>
                </span>
              </label>
            </li>
          )
        })}
      </ul>
    </div>
  )
}

/* -------------------------------------------------------------------------------------------
 * Dialog: add endpoint
 * ----------------------------------------------------------------------------------------- */

function AddEndpointDialog({
  open,
  onOpenChange,
  onAdded,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onAdded: (endpoint: WebhookEndpoint) => void
}) {
  const [url, setUrl] = React.useState("")
  const [description, setDescription] = React.useState("")
  const [events, setEvents] = React.useState<WebhookEventType[]>(DEFAULT_EVENTS)
  const [busy, setBusy] = React.useState(false)
  const [errors, setErrors] = React.useState<Record<string, string>>({})
  /** The server's answer to the create call: the only response that carries the new secret unmasked. */
  const [created, setCreated] = React.useState<WebhookEndpoint | null>(null)
  const trimmed = url.trim()
  const valid = /^https?:\/\/[^\s/]+/i.test(trimmed)

  const close = (next: boolean) => {
    onOpenChange(next)
    if (!next) {
      // After the close animation; the secret is dropped here.
      window.setTimeout(() => {
        setCreated(null)
        setUrl("")
        setDescription("")
        setEvents(DEFAULT_EVENTS)
        setErrors({})
      }, 150)
    }
  }

  const add = async () => {
    setBusy(true)
    setErrors({})
    try {
      const endpoint = await api.createEndpoint({ url: trimmed, description: description.trim(), events, enabled: true })
      setCreated(endpoint)
      onAdded(endpoint)
    } catch (error) {
      const fields = fieldErrors(error)
      setErrors(Object.keys(fields).length > 0 ? fields : { form: describeError(error) })
    } finally {
      setBusy(false)
    }
  }

  if (created) {
    return (
      <Dialog open={open} onOpenChange={close}>
        <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg" onOpenAutoFocus={(event) => event.preventDefault()}>
          <DialogHeader>
            <DialogTitle>Endpoint added</DialogTitle>
            <DialogDescription>
              Give your server this signing secret: it uses it to check that a request really comes from hmnpay. You can see it again
              later in the endpoint’s details.
            </DialogDescription>
          </DialogHeader>
          <div className="flex items-center gap-1 rounded-lg border border-border bg-background/60 py-1.5 pr-1.5 pl-3">
            <span className="num min-w-0 flex-1 text-[13px] leading-5 break-all select-all" data-slot="new-secret">
              {created.secret}
            </span>
            <CopyButton value={created.secret} label="Copy secret" />
          </div>
          <DialogFooter>
            <Button onClick={() => close(false)}>Done</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    )
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <form
          className="contents"
          onSubmit={(event) => {
            event.preventDefault()
            if (!busy) void add()
          }}
        >
          <DialogHeader>
            <DialogTitle>Add webhook endpoint</DialogTitle>
            <DialogDescription>
              hmnpay sends a signed POST request here for each event you pick. Answer with any 2xx within{" "}
              {WEBHOOK_POLICY.timeoutMs / 1000} seconds.
            </DialogDescription>
          </DialogHeader>

          <Field data-invalid={errors.url ? true : undefined}>
            <FieldLabel htmlFor="webhook-url">Endpoint URL</FieldLabel>
            <Input
              id="webhook-url"
              value={url}
              placeholder="https://shop.example.com/api/hmnpay/webhook"
              inputMode="url"
              autoComplete="off"
              spellCheck={false}
              aria-invalid={errors.url ? true : undefined}
              onChange={(event) => {
                setUrl(event.target.value)
                setErrors({})
              }}
              className="num"
            />
            {errors.url ? (
              <FieldDescription className="text-[13px] text-danger-fg" role="alert">
                {errors.url}
              </FieldDescription>
            ) : (
              /^http:\/\//i.test(trimmed) && (
                <FieldDescription className="text-[13px] text-warning-fg">
                  Plain http: events carry payment details, so use https:// outside a private network.
                </FieldDescription>
              )
            )}
          </Field>

          <Field>
            <FieldLabel htmlFor="webhook-description">
              Name <span className="font-normal text-muted-foreground">Optional</span>
            </FieldLabel>
            <Input
              id="webhook-description"
              value={description}
              placeholder="Production store"
              autoComplete="off"
              onChange={(event) => setDescription(event.target.value)}
            />
          </Field>

          <Field>
            <FieldLabel>Events to send</FieldLabel>
            <EventPicker value={events} onChange={setEvents} />
            {errors.events || errors.form ? (
              <FieldDescription className="text-[13px] text-danger-fg" role="alert">
                {errors.events ?? errors.form}
              </FieldDescription>
            ) : (
              <FieldDescription className="text-[13px]">
                Most stores only need the final states. <span className="num">payment.paid</span> is the one to fulfil orders on.
              </FieldDescription>
            )}
          </Field>

          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline" type="button">
                Cancel
              </Button>
            </DialogClose>
            <Button type="submit" disabled={busy || !valid || events.length === 0}>
              {busy ? "Adding…" : "Add endpoint"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/* -------------------------------------------------------------------------------------------
 * Sheet: endpoint detail (status, signing secret, events)
 * ----------------------------------------------------------------------------------------- */

const SHEET_CLASS = "gap-0 outline-none data-[side=right]:w-full data-[side=right]:sm:max-w-[520px]"

const VERIFY_SNIPPET = `import { createHmac, timingSafeEqual } from "node:crypto"

// header: "t=1795003200,v1=5f2a…" (two v1 values while a secret is being rotated)
export function verify(rawBody, header, secret) {
  const parts = header.split(",").map((part) => part.trim().split("="))
  const t = parts.find(([key]) => key === "t")?.[1]
  const expected = createHmac("sha256", secret)
    .update(\`\${t}.\${rawBody}\`)
    .digest()
  // Also refuse a t that is more than a few minutes old.
  return parts.some(
    ([key, value]) =>
      key === "v1" &&
      value.length === 64 &&
      timingSafeEqual(Buffer.from(value, "hex"), expected),
  )
}`

function useLast<T>(value: T | null) {
  // Keep the last non-null value so content stays put during the close animation.
  const [last, setLast] = React.useState(value)
  if (value !== null && value !== last) setLast(value)
  return value ?? last
}

function EndpointSheet({
  endpoint: current,
  now,
  onOpenChange,
  onChanged,
  onDeleted,
  onSendTest,
  onShowDeliveries,
}: {
  endpoint: WebhookEndpoint | null
  now: number
  onOpenChange: (open: boolean) => void
  onChanged: (endpoint: WebhookEndpoint) => void
  onDeleted: (endpoint: WebhookEndpoint) => void
  onSendTest: (endpoint: WebhookEndpoint) => Promise<void>
  onShowDeliveries: (endpoint: WebhookEndpoint) => void
}) {
  const endpoint = useLast(current)
  /** The secret, once the operator asked to see it (POST …/reveal-secret) or rotated it. Dropped when the sheet closes. */
  const [revealed, setRevealed] = React.useState<{ id: string; secret: WebhookSecret } | null>(null)
  const [rotateOpen, setRotateOpen] = React.useState(false)
  const [deleteOpen, setDeleteOpen] = React.useState(false)
  const [events, setEvents] = React.useState<{ id: string; value: WebhookEventType[] } | null>(null)
  const [busy, setBusy] = React.useState<"reveal" | "save" | "toggle" | "test" | "delete" | null>(null)

  if (!endpoint) return null
  const health = endpointHealth(endpoint)
  const failing = isFailing(endpoint)
  const secret = revealed?.id === endpoint.id ? revealed.secret : null
  const selected = events?.id === endpoint.id ? events.value : (endpoint.events as WebhookEventType[])
  const dirty = events?.id === endpoint.id && events.value.join() !== endpoint.events.join()
  const input = (patch: Partial<{ events: WebhookEventType[]; enabled: boolean }>) => ({
    url: endpoint.url,
    description: endpoint.description,
    events: selected,
    enabled: endpoint.enabled,
    ...patch,
  })

  const run = async (what: NonNullable<typeof busy>, action: () => Promise<void>, failure: string) => {
    setBusy(what)
    try {
      await action()
    } catch (error) {
      toast.error(failure, { description: describeError(error) })
    } finally {
      setBusy(null)
    }
  }
  const reveal = () =>
    run(
      "reveal",
      async () => {
        setRevealed({ id: endpoint.id, secret: await api.revealSecret(endpoint.id) })
      },
      "Couldn’t show the secret",
    )
  const save = () =>
    run(
      "save",
      async () => {
        onChanged(await api.updateEndpoint(endpoint.id, input({})))
        setEvents(null)
        toast.success("Events saved")
      },
      "Not saved",
    )
  const toggle = () =>
    run(
      "toggle",
      async () => {
        const next = await api.updateEndpoint(endpoint.id, input({ enabled: !endpoint.enabled, events: endpoint.events as WebhookEventType[] }))
        onChanged(next)
        toast(next.enabled ? "Endpoint enabled" : "Endpoint disabled")
      },
      "Couldn’t change the endpoint",
    )

  return (
    <Sheet
      open={current !== null}
      onOpenChange={(open) => {
        onOpenChange(open)
        if (!open) setRevealed(null)
      }}
    >
      <SheetContent className={SHEET_CLASS} onOpenAutoFocus={(event) => event.preventDefault()}>
        <SheetHeader className="gap-1 border-b border-border p-5 pr-12">
          <div className="flex flex-wrap items-center gap-2.5">
            <SheetTitle className="font-semibold tracking-[-0.015em]">{endpointName(endpoint)}</SheetTitle>
            <StatusBadge tone={health.tone} hollow={health.hollow} size="sm">
              {health.label}
            </StatusBadge>
          </div>
          <SheetDescription asChild>
            <div className="flex items-center gap-1 text-[13px] leading-5">
              <span className="num min-w-0 truncate" title={endpoint.url}>
                {endpoint.url}
              </span>
              <CopyButton value={endpoint.url} label="Copy URL" size="icon-xs" />
            </div>
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-6 overflow-y-auto p-5">
          {failing && (
            <Callout
              tone="danger"
              icon={<TriangleAlertIcon />}
              title={`${endpoint.consecutive_failures} ${endpoint.consecutive_failures === 1 ? "attempt" : "attempts"} failed in a row`}
            >
              {endpoint.failing_since && <>Nothing has got through since {formatDateTime(endpoint.failing_since)}. </>}
              {endpoint.disable_at && (
                <>
                  It will be disabled on <span className="text-foreground">{formatDateTime(endpoint.disable_at)}</span> unless a delivery
                  succeeds.
                </>
              )}
            </Callout>
          )}
          {!endpoint.enabled && endpoint.disabled_reason === "failing" && (
            <Callout tone="warning" icon={<TriangleAlertIcon />} title="Disabled after failing for 72 hours">
              hmnpay switched this endpoint off{endpoint.disabled_at && <> on {formatDateTime(endpoint.disabled_at)}</>} because every
              delivery failed. Fix your server, enable it again, and resend what was missed.
            </Callout>
          )}

          <DetailList className="-my-2 text-[13px]">
            <DetailRow label="Delivered, 7 days" className="min-h-10">
              {endpoint.success_rate !== undefined ? (
                <span>
                  {percent(endpoint.success_rate)}%
                  <span className="text-muted-foreground">
                    {" "}
                    · <span className="num">{endpoint.succeeded_7d ?? 0}</span> of{" "}
                    <span className="num">{(endpoint.succeeded_7d ?? 0) + (endpoint.failed_7d ?? 0)}</span>
                  </span>
                </span>
              ) : (
                <span className="text-muted-foreground">No deliveries in the last 7 days</span>
              )}
            </DetailRow>
            <DetailRow label="Last attempt" className="min-h-10">
              {endpoint.last_delivery_at ? relativeTime(endpoint.last_delivery_at, now) : <span className="text-muted-foreground">Never</span>}
            </DetailRow>
            <DetailRow label="Added" className="min-h-10">
              {formatDate(endpoint.created_at)}
            </DetailRow>
          </DetailList>

          <section className="flex flex-col gap-2.5">
            <BlockLabel>Signing secret</BlockLabel>
            <div className="flex items-center gap-1 rounded-lg border border-border bg-background/60 py-1.5 pr-1.5 pl-3">
              <span className={cn("num min-w-0 flex-1 text-[13px] leading-5", secret ? "break-all" : "truncate")} data-slot="signing-secret">
                {secret ? secret.secret : endpoint.secret}
              </span>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={secret ? "Hide secret" : "Reveal secret"}
                aria-pressed={secret !== null}
                disabled={busy === "reveal"}
                className="text-muted-foreground hover:text-foreground"
                onClick={() => (secret ? setRevealed(null) : void reveal())}
              >
                {secret ? <EyeOffIcon /> : <EyeIcon />}
              </Button>
              {secret && <CopyButton value={secret.secret} label="Copy secret" />}
            </div>
            {(secret?.previous_secret_expires_at ?? endpoint.previous_secret_expires_at) && (
              <p className="text-[13px] leading-5 text-warning-fg">
                Rotated: requests are signed with the previous secret too until{" "}
                {formatDateTime((secret?.previous_secret_expires_at ?? endpoint.previous_secret_expires_at)!)}.
              </p>
            )}
            <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
              <p className="min-w-0 flex-1 basis-56 text-[13px] leading-5 text-muted-foreground">
                Every request carries an <span className="num text-foreground">Hmnpay-Signature</span> header made with this secret.
              </p>
              <Button variant="outline" size="sm" onClick={() => setRotateOpen(true)}>
                <RotateCwIcon data-icon="inline-start" />
                Rotate
              </Button>
            </div>
            <CodeSnippet code={VERIFY_SNIPPET} language="javascript" title="Verify a request (Node.js)" />
          </section>

          <section className="flex flex-col gap-2.5">
            <BlockLabel>Events</BlockLabel>
            <EventPicker value={selected} onChange={(value) => setEvents({ id: endpoint.id, value })} />
          </section>

          <section className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-t border-border pt-5">
            <p className="min-w-0 flex-1 basis-56 text-[13px] leading-5 text-muted-foreground">
              Deleting an endpoint stops its deliveries for good. Its delivery history goes with it.
            </p>
            <Button variant="outline" size="sm" className="text-danger-fg hover:text-danger-fg" onClick={() => setDeleteOpen(true)}>
              Delete endpoint
            </Button>
          </section>
        </div>

        <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border bg-muted/40 px-5 py-3">
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" disabled={busy === "toggle"} onClick={() => void toggle()}>
              {endpoint.enabled ? "Disable" : "Enable"}
            </Button>
            <Button variant="outline" onClick={() => onShowDeliveries(endpoint)}>
              Deliveries
            </Button>
          </div>
          <div className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              disabled={!endpoint.enabled || busy === "test"}
              onClick={() => void run("test", () => onSendTest(endpoint), "Couldn’t send a test event")}
            >
              <SendIcon data-icon="inline-start" />
              Send test
            </Button>
            <Button disabled={!dirty || selected.length === 0 || busy === "save"} onClick={() => void save()}>
              Save
            </Button>
          </div>
        </div>

        <RotateDialog
          open={rotateOpen}
          endpoint={endpoint}
          onOpenChange={setRotateOpen}
          onConfirm={async () => {
            try {
              const next = await api.rotateSecret(endpoint.id)
              // The answer carries the new secret once; show it, and keep only its mask in the list.
              setRevealed({
                id: endpoint.id,
                secret: { secret: next.secret, previous_secret: null, previous_secret_expires_at: next.previous_secret_expires_at },
              })
              onChanged(next)
              toast.success("Secret rotated", { description: "The new secret is shown now. Deploy it to your server." })
            } catch (error) {
              toast.error("Couldn’t rotate the secret", { description: describeError(error) })
            }
          }}
        />

        <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
          <AlertDialogContent className="data-[size=default]:max-w-[calc(100%-2rem)] data-[size=default]:sm:max-w-md">
            <AlertDialogHeader className="place-items-start text-left">
              <AlertDialogTitle>Delete this endpoint?</AlertDialogTitle>
              <AlertDialogDescription className="text-pretty">
                hmnpay stops sending events to <span className="num text-foreground">{hostOf(endpoint.url)}</span> straight away, including
                retries that were still due. This can’t be undone.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel className="pointer-coarse:h-10">Keep endpoint</AlertDialogCancel>
              <Button
                variant="destructive"
                disabled={busy === "delete"}
                onClick={() =>
                  void run(
                    "delete",
                    async () => {
                      await api.deleteEndpoint(endpoint.id)
                      setDeleteOpen(false)
                      onDeleted(endpoint)
                      toast("Endpoint deleted")
                    },
                    "Couldn’t delete the endpoint",
                  )
                }
              >
                Delete endpoint
              </Button>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </SheetContent>
    </Sheet>
  )
}

function RotateDialog({
  open,
  endpoint,
  onOpenChange,
  onConfirm,
}: {
  open: boolean
  endpoint: WebhookEndpoint
  onOpenChange: (open: boolean) => void
  onConfirm: () => Promise<void>
}) {
  const [busy, setBusy] = React.useState(false)
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent className="data-[size=default]:max-w-[calc(100%-2rem)] data-[size=default]:sm:max-w-md">
        <AlertDialogHeader className="place-items-start text-left">
          <AlertDialogTitle>Rotate the signing secret?</AlertDialogTitle>
          <AlertDialogDescription className="text-pretty">
            “{endpointName(endpoint)}” gets a new secret straight away. For the next {WEBHOOK_POLICY.secretOverlapHours} hours every
            request is signed with both, so you can deploy the new one without dropping an event. After that the old secret
            stops working.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel className="pointer-coarse:h-10">Cancel</AlertDialogCancel>
          <Button
            disabled={busy}
            onClick={async () => {
              setBusy(true)
              await onConfirm()
              setBusy(false)
              onOpenChange(false)
            }}
          >
            {busy ? "Rotating…" : "Rotate secret"}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

/* -------------------------------------------------------------------------------------------
 * Sheet: delivery detail (attempts, request, response, resend)
 * ----------------------------------------------------------------------------------------- */

function prettyBody(body: string) {
  try {
    return { code: JSON.stringify(JSON.parse(body), null, 2), language: "json" }
  } catch {
    return { code: body, language: "text" }
  }
}

function DeliverySheet({
  deliveryId,
  listed,
  endpoints,
  now,
  onResent,
  onOpenChange,
}: {
  deliveryId: string | null
  /** The deliveries on screen: the one opened is shown from here at once, then refreshed on its own. */
  listed: WebhookDelivery[] | undefined
  endpoints: Map<string, WebhookEndpoint>
  now: number
  onResent: () => void
  onOpenChange: (open: boolean) => void
}) {
  // The sheet follows its delivery by itself (a test event is answered within seconds).
  const fresh = useQuery(deliveryId ? `webhook-delivery/${deliveryId}` : null, (signal) => api.delivery(deliveryId!, signal), { refreshMs: 2000 })
  const current = deliveryId ? (fresh.data ?? listed?.find((item) => item.id === deliveryId) ?? null) : null
  const delivery = useLast(current)
  const [resending, setResending] = React.useState(false)
  if (!delivery) {
    if (deliveryId && fresh.error) {
      return (
        <Sheet open onOpenChange={onOpenChange}>
          <SheetContent className={SHEET_CLASS}>
            <SheetHeader className="border-b border-border p-5 pr-12">
              <SheetTitle>Delivery</SheetTitle>
              <SheetDescription>{describeError(fresh.error)}</SheetDescription>
            </SheetHeader>
          </SheetContent>
        </Sheet>
      )
    }
    return null
  }

  const endpoint = endpoints.get(delivery.endpoint_id)
  const meta = deliveryMeta(delivery)
  const attempts = delivery.attempts
  const seq = deliverySeq(delivery)
  const response = delivery.response_body ? prettyBody(delivery.response_body) : null
  const answered = delivery.response_code !== null
  const waiting = delivery.status === "pending" && delivery.attempt === 0

  const summary =
    delivery.status === "succeeded"
      ? delivery.attempt > 1
        ? `Delivered on try ${delivery.attempt}`
        : "Delivered"
      : delivery.status === "retrying"
        ? `Failed ${delivery.attempt} of ${delivery.max_attempts} tries`
        : delivery.status === "failed"
          ? `Gave up after ${delivery.attempt} ${delivery.attempt === 1 ? "try" : "tries"}`
          : "Queued"

  const resend = async () => {
    setResending(true)
    try {
      await api.resendDelivery(delivery.id)
      toast.success("Delivery queued", { description: "It goes out again within a few seconds." })
      fresh.reload()
      onResent()
    } catch (error) {
      toast.error("Couldn’t resend", { description: describeError(error) })
    } finally {
      setResending(false)
    }
  }

  return (
    <Sheet open={deliveryId !== null} onOpenChange={onOpenChange}>
      <SheetContent className={SHEET_CLASS} onOpenAutoFocus={(event) => event.preventDefault()} data-delivery-sheet={delivery.status}>
        <SheetHeader className="gap-1 border-b border-border p-5 pr-12">
          <div className="flex flex-wrap items-center gap-2.5">
            <SheetTitle className="num font-medium">{delivery.event_type}</SheetTitle>
            <ResponseBadge delivery={delivery} />
          </div>
          <SheetDescription className="text-[13px] leading-5">
            {summary} · to {endpoint ? endpointName(endpoint) : "a deleted endpoint"}
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-6 overflow-y-auto p-5">
          {delivery.status === "retrying" && delivery.next_attempt_at && (
            <Callout tone="warning" icon={<RotateCwIcon />} title={`Next try ${relativeTime(delivery.next_attempt_at, now)}`}>
              hmnpay tries up to {delivery.max_attempts} times over about a day, waiting longer each time. Resend to try right now.
            </Callout>
          )}
          {delivery.status === "failed" && (
            <Callout tone="danger" icon={<TriangleAlertIcon />} title="This event never reached your server">
              Every try failed. Fix the endpoint, then resend. The event is kept until{" "}
              {formatDate(Date.parse(delivery.created_at) + WEBHOOK_POLICY.retentionDays * 86_400_000)}.
            </Callout>
          )}

          <DetailList className="-my-2 text-[13px]">
            <DetailRow label="Endpoint" className="min-h-10">
              <span className="num min-w-0 truncate" title={delivery.url}>
                {hostOf(delivery.url) + pathOf(delivery.url)}
              </span>
            </DetailRow>
            {delivery.payment_id && (
              <DetailRow label="Payment" className="min-h-10">
                <Link
                  href={`/payments/${delivery.payment_id}`}
                  className="focus-ring num min-w-0 truncate rounded-sm underline decoration-border-strong underline-offset-4 hover:decoration-foreground"
                >
                  {delivery.payment_id}
                </Link>
              </DetailRow>
            )}
            <DetailRow label="Event ID" className="min-h-10">
              <span className="num min-w-0 truncate">{delivery.event_id}</span>
              <CopyButton value={delivery.event_id} label="Copy event ID" size="icon-xs" className="-mr-1.5 ml-1" />
            </DetailRow>
            <DetailRow label="Event seq" className="min-h-10">
              {seq !== null ? (
                <span className="num" data-slot="event-seq" title="The event’s position in the instance’s event log. Order by it; ignore a lower one.">
                  {seq}
                </span>
              ) : (
                <span className="text-muted-foreground">None (test events are not in the log)</span>
              )}
            </DetailRow>
            <DetailRow label="First queued" className="min-h-10">
              {formatDateTime(delivery.created_at)}
            </DetailRow>
          </DetailList>

          <section className="flex flex-col gap-2.5">
            <BlockLabel>{attempts.length === 1 ? "Attempt" : `Attempts`}</BlockLabel>
            {attempts.length === 0 ? (
              <p className="rounded-lg border border-dashed border-border-strong px-3.5 py-3 text-[13px] leading-5 text-muted-foreground">
                Not sent yet. It goes out within a few seconds.
              </p>
            ) : (
              <ol className="divide-y divide-border rounded-lg border border-border" data-slot="attempts">
                {attempts.map((attempt, index) => {
                  const tone: Tone = attempt.succeeded ? "success" : "danger"
                  return (
                    <li key={`${attempt.n}-${index}`} className="px-3 py-2 text-[13px] leading-5">
                      <div className="flex items-center gap-3">
                        <span className="num w-4 text-faint">{attempt.n}</span>
                        <StatusDot tone={tone} />
                        <span className={cn(attempt.response_code !== null && "num")}>{attempt.response_code ?? "No response"}</span>
                        {attempt.late && <MonoTag className="font-sans tracking-normal">late, not counted</MonoTag>}
                        <span className="ml-auto text-muted-foreground">
                          <Duration ms={attempt.duration_ms} />
                        </span>
                        <span className="w-[5.5rem] text-right text-muted-foreground" title={formatDateTime(attempt.at)}>
                          {formatTime(attempt.at)}
                        </span>
                      </div>
                      {attempt.error && <p className="mt-1 pl-10 break-words text-danger-fg">{attempt.error}</p>}
                    </li>
                  )
                })}
              </ol>
            )}
          </section>

          <section className="flex flex-col gap-2.5">
            <BlockLabel>Request</BlockLabel>
            <CodeSnippet code={deliveryRequestHead(delivery)} language="text" title="Headers" wrap contentClassName="[overflow-wrap:anywhere]" />
            <CodeSnippet code={prettyBody(delivery.request_body).code} language="json" title="Body" />
          </section>

          <section className="flex flex-col gap-2.5">
            <BlockLabel>Response</BlockLabel>
            {answered ? (
              response ? (
                <CodeSnippet
                  code={response.code}
                  language={response.language}
                  title={
                    <>
                      <span className={cn("num", meta.tone === "success" ? "text-success-fg" : "text-danger-fg")}>{delivery.response_code}</span>
                      <span className="ml-1.5">from your server</span>
                    </>
                  }
                />
              ) : (
                <p className="rounded-lg border border-dashed border-border-strong px-3.5 py-3 text-[13px] leading-5 text-muted-foreground">
                  Your server answered <span className="num text-foreground">{delivery.response_code}</span> with an empty body.
                </p>
              )
            ) : (
              <p className="rounded-lg border border-dashed border-border-strong px-3.5 py-3 text-[13px] leading-5 text-muted-foreground">
                {waiting
                  ? "Waiting to be sent."
                  : delivery.error
                    ? `No answer from your server: ${delivery.error}`
                    : `Your server didn’t answer within ${WEBHOOK_POLICY.timeoutMs / 1000} seconds, so hmnpay closed the connection.`}
              </p>
            )}
          </section>
        </div>

        <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-t border-border bg-muted/40 px-5 py-3">
          <p className="min-w-0 flex-1 basis-48 text-[13px] leading-5 text-muted-foreground">
            A resend keeps the same event ID and seq, so your server can ignore duplicates.
          </p>
          <Button onClick={() => void resend()} disabled={resending || delivery.status === "pending"}>
            <SendIcon data-icon="inline-start" />
            Resend
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  )
}

/* -------------------------------------------------------------------------------------------
 * Empty state companion: what an event looks like
 * ----------------------------------------------------------------------------------------- */

/** The shape of an event, with placeholders where a real one has values. */
const EXAMPLE_EVENT = `{
  "id": "evt_…",
  "type": "payment.paid",
  "created_at": "…",
  "seq": 1234,
  "data": {
    "id": "pay_…",
    "status": "paid",
    "order_id": "ORD-1001",
    "amount": "49.00",
    "amount_received": "49.00",
    "amount_confirmed": "49.00",
    "asset": "USDT",
    "address": "0x…",
    "tx_hash": "0x…",
    "paid_at": "…"
  }
}`

function ExampleEvent() {
  return (
    <SettingsSection
      title="What you’ll receive"
      description="One JSON event per state change, signed with the endpoint’s secret. Failed deliveries are retried for about a day."
      footer={
        <>
          Fulfil orders on <span className="num text-foreground">payment.paid</span>. Order events by <span className="num text-foreground">seq</span>{" "}
          and ignore a lower one: everything before “paid” can still change.
        </>
      }
      actions={
        <Button size="sm" variant="outline" asChild>
          <a href={DOCS} target="_blank" rel="noopener noreferrer">
            <BookOpenIcon data-icon="inline-start" />
            Webhook guide
          </a>
        </Button>
      }
    >
      <CodeSnippet
        code={EXAMPLE_EVENT}
        language="json"
        title={
          <>
            <span className="num">POST · payment.paid</span> <span className="ml-1.5 text-faint">shortened</span>
          </>
        }
      />
    </SettingsSection>
  )
}
