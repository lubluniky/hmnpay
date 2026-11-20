"use client"

import * as React from "react"
import Link from "next/link"
import { ActivityIcon, ArrowLeftRightIcon, CheckIcon, MoonIcon, MoreHorizontalIcon, PlusIcon, ShieldAlertIcon, ShieldCheckIcon, Trash2Icon, TriangleAlertIcon, XIcon, ZapIcon } from "lucide-react"
import { toast } from "sonner"

import { Address, Callout, DetailList, DetailRow, NetworkIcon, PageSection, Panel, StatusBadge, type Tone } from "@/components/hmn"
import { chainName, nodeHealth } from "@/components/shell/node-status"
import { endpointIdentity, sameEndpointAs, sameEndpointMessage, sameProviderGroups, sameProviderMessage } from "@/lib/endpoints"
import {
  finalityDispute,
  finalityStalled,
  holdInWords,
  isClockBehind,
  isClockDispute,
  isFinalityHold,
  operatorWarnings,
  unusableEndpoints,
  waitingForFirstBlock,
  workInWords,
} from "@/lib/node-health"
import { useDashboard } from "@/components/shell/session"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { SUGGESTED_RPC } from "@/components/setup/data"
import { api, isApiError } from "@/lib/api/client"
import type { EndpointState, EndpointStatus, FieldError, NodeStatus, ProbeResult, Settings } from "@/lib/api/types"
import { useQuery } from "@/lib/api/use-query"
import { describeError, fieldErrors } from "@/lib/errors"
import { relativeTime } from "@/lib/format"
import { useNow } from "@/lib/use-now"
import { cn } from "@/lib/utils"

import { Button, initialParam, Input, stripParams } from "./shared"

/**
 * Settings → Network: the chain this instance watches, how the watcher is doing, and the RPC
 * endpoints it reads the chain through.
 *
 * Everything about the watcher comes from GET /api/v1/node-status (refreshed every few seconds
 * by the dashboard) and is said here in plain words: whether it is starting, working or
 * holding confirmations back and why, which endpoint it reads from, which one checks that
 * endpoint, and what is worth the operator's attention.
 */

/**
 * scheme://host[:port] of a URL as the server names endpoints in the node status (its canonical
 * identity: never a path or a provider key, one spelling per host), so that what the operator
 * typed and what the watcher reports are matched by the server's rule.
 */
function origin(url: string) {
  const id = endpointIdentity(url)
  return id === "invalid-url" ? url : id
}
const hostOf = (url: string) => origin(url).replace(/^https?:\/\//, "")
const count = (value: number) => <span className="num">{value.toLocaleString("en-US")}</span>
/** A field problem of the stored settings, as a sentence (the server's words, capitalised). */
const problemSentence = (p: FieldError) => {
  const text = p.message.trim()
  return text ? `${text[0].toUpperCase()}${text.slice(1)}${/[.!?]$/.test(text) ? "" : "."}` : "The server can’t use this entry."
}
/** "a", "a and b", "a, b and c". */
const listOf = (items: string[]) => (items.length < 2 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`)

export function NetworkSettings() {
  const [addOpen, setAddOpen] = React.useState(() => initialParam("add") === "1")
  const [switchOpen, setSwitchOpen] = React.useState(false)
  return (
    <>
      <StatusPanel />
      <RolesPanel />
      <EndpointsSection onAdd={() => setAddOpen(true)} />
      <SwitchSection onSwitch={() => setSwitchOpen(true)} />
      <SwitchNetworkDialog open={switchOpen} onOpenChange={setSwitchOpen} />
      <AddEndpointDialog
        open={addOpen}
        onOpenChange={(open) => {
          setAddOpen(open)
          if (!open) stripParams("add")
        }}
      />
    </>
  )
}

/* -------------------------------------------------------------------------------------------
 * Chain identity + what the watcher is doing
 * ----------------------------------------------------------------------------------------- */

/** What is worth saying about the node right now, most important first. */
function notices(node: NodeStatus, problems: FieldError[]): { tone: Tone; title: string; text: React.ReactNode; icon?: React.ReactNode }[] {
  const out: { tone: Tone; title: string; text: React.ReactNode; icon?: React.ReactNode }[] = []
  if (node.network_problem) {
    // Settings an earlier version stored that the rules now refuse: no watcher runs at all.
    const marked = problems.some((p) => p.field.startsWith("rpc_urls."))
    out.push({
      tone: "danger",
      title: "The network settings can’t be used",
      text: (
        <>
          No chain watcher runs and no payment can be created until they are repaired:{" "}
          <span className="text-foreground">{node.network_problem}</span>.{" "}
          {marked ? "The endpoint that is wrong is marked in the list below; correct or remove it." : "Correct them below."} Payments work again as
          soon as the settings are saved.
        </>
      ),
    })
    return out
  }
  if (node.starting) {
    out.push({
      tone: "info",
      title: "Starting",
      text: "The watcher has just started (or the network settings changed) and is making its first pass over the chain. This normally takes a second or two; nothing is wrong.",
    })
    return out
  }
  if (node.hold) {
    out.push({
      tone: "warning",
      title: "Confirmations are waiting",
      text: (
        <>
          {holdInWords(node.hold) && <>{holdInWords(node.hold)} </>}
          Nothing is confirmed or expired while this lasts, and nothing is lost. The reason: <span className="text-foreground">{node.hold}</span>
          {isFinalityHold(node.hold) && (
            <>
              {" "}
              One of the endpoints is behind or wrong about which blocks are final. Check both providers, and replace the one that is wrong.
            </>
          )}
          {unusableEndpoints(node).length > 0 && (
            <> If an endpoint named here was mistyped or is gone for good, fix or remove it in the list below.</>
          )}
        </>
      ),
    })
  } else if (waitingForFirstBlock(node)) {
    const unusable = unusableEndpoints(node).map(hostOf)
    out.push({
      tone: "warning",
      title: "Payments can’t start yet",
      text:
        unusable.length > 0 ? (
          <>
            Before its first block hmnpay waits until two endpoints agree on where the chain ends, and{" "}
            <span className="text-foreground">{listOf(unusable)}</span> can’t be used right now. Until then
            no payment can be created. Check {unusable.length === 1 ? "its" : "their"} URL in the list below, or remove{" "}
            {unusable.length === 1 ? "it" : "them"} if {unusable.length === 1 ? "it was" : "they were"} added by mistake (with a single endpoint,
            hmnpay trusts that one alone).
          </>
        ) : (
          "Before its first block hmnpay waits until two endpoints agree on where the chain ends. Until then no payment can be created. The list below says what each endpoint answers."
        ),
    })
  } else if (!node.healthy) {
    out.push({
      tone: "danger",
      title: node.last_checked_at ? "The watcher is not keeping up with the chain" : "The watcher has not reported",
      text: node.last_checked_at
        ? "The endpoint in use is failing or behind. Payments are not being detected until an endpoint answers again; check the endpoints below."
        : "No status has arrived from the chain watcher. If this does not change within a minute, look at the server log.",
    })
  }
  if (node.solo) {
    out.push({
      tone: "warning",
      title: "One endpoint is being trusted alone",
      text: "Every other endpoint has been unreachable, and the server runs in best-effort mode, so the remaining one is believed without a cross-check until another answers again.",
    })
  }
  if (node.logs_unchecked) {
    out.push({
      tone: "warning",
      title: "Recent blocks were settled on one endpoint’s logs",
      text: "Only one endpoint is serving logs right now, so a transfer it left out would not be noticed. Add a second endpoint that serves logs (eth_getLogs); the blocks are re-scanned once one answers.",
    })
  }
  if (node.watch_list_full) {
    out.push({
      tone: "warning",
      title: "Payment links are closed for the moment",
      text: "The number of addresses watched for payment-link checkouts is at its limit, so links refuse new payments until some age out. Payments you create yourself are not affected.",
    })
  }
  if (node.released_unchecked > 0) {
    const n = node.released_unchecked
    out.push({
      tone: "danger",
      title: "Some addresses stopped being watched too early",
      text: `${n.toLocaleString("en-US")} settled payment${n === 1 ? "’s address" : "s’ addresses"} left the watch list before a second endpoint had checked the blocks of ${n === 1 ? "its" : "their"} watch window, because too many were waiting for that check. If the one endpoint whose logs settled those blocks left a payment out, it will not be found. Add a second endpoint that serves logs (eth_getLogs), or bring back the one that is not answering.`,
    })
  }
  // What the watcher is busy with, when it is worth saying (blocks owed a second log scan, a re-scan…).
  const work = workInWords(node.work)
  if (work) out.push({ tone: "neutral", title: "What the watcher is doing", text: work, icon: <ActivityIcon /> })
  const stalled = finalityStalled(node)
  const dispute = finalityDispute(node)
  const waiting = node.awaiting_finality
  if (stalled !== undefined) {
    out.push({
      tone: "warning",
      title: "The chain has stopped finalising blocks",
      text: (
        <>
          <span className="text-foreground">{stalled}</span>. Nothing is lost:{" "}
          {waiting ? `${waiting.payments.toLocaleString("en-US")} payment${waiting.payments === 1 ? " waits" : "s wait"}` : "payments wait"} as
          confirming and are paid by themselves once the chain finalises their blocks. The finality_stalled alert has been raised. If the chain is fine
          on a block explorer, an endpoint may be reporting an old finalised block.
        </>
      ),
    })
  } else if (waiting && waiting.final < waiting.block) {
    out.push({
      tone: "info",
      title: "Waiting for finality",
      text: `${waiting.payments.toLocaleString("en-US")} payment${waiting.payments === 1 ? " has" : "s have"} all ${waiting.payments === 1 ? "its" : "their"} confirmations and ${waiting.payments === 1 ? "waits" : "wait"} for the chain to finalise block ${waiting.block.toLocaleString("en-US")} (finalised so far: ${waiting.final.toLocaleString("en-US")}). This is normal and usually over in seconds; they are paid when it is. If the chain stops finalising for two minutes, it is reported here as a stall.`,
    })
  }
  if (dispute !== undefined && !(node.hold && node.hold.includes(dispute))) {
    out.push({
      tone: "warning",
      title: "Two endpoints disagree about finality",
      text: (
        <>
          <span className="text-foreground">{dispute}</span>. One of them is behind or wrong about which blocks are final. Payments in later blocks are
          held as confirming, not lost, and are paid once the endpoints agree. Check both providers, and replace the one that is wrong.
        </>
      ),
    })
  }
  for (const warning of operatorWarnings(node)) {
    // The watcher also lists a hold among its warnings; it has been said above.
    if (node.hold && warning.includes(node.hold)) continue
    if (isClockBehind(warning)) {
      out.push({
        tone: "danger",
        title: "The server’s clock is behind the chain",
        text: `${warning}. While it is more than 2 minutes behind (HMNPAY_MAX_HEAD_TIME_AHEAD), no payment can be created, and waiting does not fix it.`,
      })
    } else if (isClockDispute(warning)) {
      out.push({
        tone: "warning",
        title: "Two endpoints disagree about the time",
        text: `${warning.replace(/^clock dispute: /, "")}. Payments are still taken: one endpoint’s word does not move the clock. But one of them serves a wrong time or a stale chain; check them, and replace the one that is wrong.`,
      })
    } else if (/addresses are on the chain watch list/.test(warning)) {
      out.push({ tone: "warning", title: "Many addresses are being watched", text: warning })
    } else {
      out.push({ tone: "warning", title: "Warning from the watcher", text: warning })
    }
  }
  if (node.idle && node.healthy && out.length === 0) {
    out.push(
      node.quiet
        ? {
            tone: "neutral",
            title: "Quiet — watching closed payments for late funds",
            text: `No payment is open. The ${node.watched_addresses === 1 ? "address" : `${node.watched_addresses.toLocaleString("en-US")} addresses`} still watched belong to closed payments, kept on the list in case somebody sends money late, so the watcher looks at the chain less often to save RPC calls. Every block is still scanned; late funds just show up a little later. The blocks it is behind by are that pause. Creating a payment wakes it at once.`,
          }
        : {
            tone: "neutral",
            title: "Idle — nothing to watch",
            text: "No payment is open and no address is watched, so the watcher looks at the chain less often to save RPC calls. The blocks it is behind by are that pause. Creating a payment wakes it at once.",
          },
    )
  }
  return out
}

function StatusPanel() {
  const { settings, node, nodeError } = useDashboard()
  const now = useNow(5000)
  const health = nodeHealth(node, Boolean(nodeError))
  const testnet = settings.network.testnet
  const height = node?.head?.number ?? 0

  return (
    <Panel data-slot="node-status" data-state={health.label}>
      <div className="flex flex-wrap items-center justify-between gap-3 px-5 py-4 sm:px-6 sm:py-5">
        <div className="flex min-w-0 items-center gap-3">
          <NetworkIcon size={32} />
          <div className="min-w-0">
            <h2 className="text-base leading-6 font-semibold tracking-[-0.015em]">{chainName(settings.network.chain_id)}</h2>
            <p className="text-[13px] leading-5 text-muted-foreground">
              Chain ID <span className="num text-foreground">{settings.network.chain_id}</span>
              {" · "}
              {testnet ? "test tokens, no value" : "real funds"}
            </p>
          </div>
        </div>
        <StatusBadge tone={health.tone} live={health.live}>
          {health.label}
        </StatusBadge>
      </div>

      {node && notices(node, settings.network.problems).length > 0 && (
        <div className="flex flex-col gap-2.5 px-5 pb-4 sm:px-6 sm:pb-5">
          {notices(node, settings.network.problems).map((notice, index) => (
            <Callout key={index} tone={notice.tone} icon={notice.icon ?? (notice.tone === "neutral" ? <MoonIcon /> : <TriangleAlertIcon />)} title={notice.title}>
              {notice.text}
            </Callout>
          ))}
        </div>
      )}
      {!node && Boolean(nodeError) && (
        <div className="px-5 pb-4 sm:px-6 sm:pb-5">
          <Callout tone="danger" icon={<TriangleAlertIcon />} title="The node status could not be loaded">
            {describeError(nodeError)}
          </Callout>
        </div>
      )}

      <dl className="grid grid-cols-2 gap-px border-y border-border bg-border sm:grid-cols-4">
        <Metric label="Processed block" value={node ? height > 0 ? count(height) : "—" : <Skeleton className="h-6 w-24" />} />
        <Metric
          label={node?.idle ? (node.quiet ? "Behind (quiet pause)" : "Behind (idle pause)") : "Behind the chain"}
          value={node ? count(node.lag_blocks) : <Skeleton className="h-6 w-10" />}
          unit={node ? (node.lag_blocks === 1 ? "block" : "blocks") : undefined}
        />
        <Metric label="Latency" value={node ? count(node.rpc_latency_ms) : <Skeleton className="h-6 w-12" />} unit={node ? "ms" : undefined} />
        <Metric
          label="Watched addresses"
          value={node ? count(node.watched_addresses) : <Skeleton className="h-6 w-12" />}
          unit={
            node
              ? [
                  node.max_watched_addresses > 0 ? `${node.watched_link_addresses} of ${node.max_watched_addresses} from links` : "",
                  node.retained_unchecked > 0 ? `${node.retained_unchecked} kept for a second endpoint’s check` : "",
                ]
                  .filter(Boolean)
                  .join(", ") || undefined
              : undefined
          }
        />
      </dl>

      <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-1 px-5 py-2.5 text-[13px] leading-5 text-muted-foreground sm:px-6">
        <span className="flex items-center gap-2">
          {settings.asset.symbol} contract
          <Address value={settings.asset.contract} chainId={settings.asset.chain_id} className="text-foreground" />
        </span>
        {node?.last_checked_at && (
          <span>
            Checked {relativeTime(node.last_checked_at, now)}
            {node.primary && (
              <>
                {" "}
                via <span className="num whitespace-nowrap">{hostOf(node.primary)}</span>
              </>
            )}
          </span>
        )}
      </div>
    </Panel>
  )
}

function Metric({ label, value, unit }: { label: string; value: React.ReactNode; unit?: string }) {
  return (
    <div className="bg-card px-5 py-3.5 sm:px-6">
      <dt className="text-[13px] leading-5 text-muted-foreground">{label}</dt>
      <dd className="num mt-0.5 flex flex-wrap items-baseline gap-x-1.5 text-lg leading-7 font-medium tracking-[-0.03em]">
        {value}
        {unit && <span className="font-sans text-[13px] font-normal tracking-normal text-muted-foreground">{unit}</span>}
      </dd>
    </div>
  )
}

/* -------------------------------------------------------------------------------------------
 * Who does what
 * ----------------------------------------------------------------------------------------- */

function RolesPanel() {
  const { node, settings } = useDashboard()
  const several = new Set(settings.network.rpc_urls.map(origin)).size > 1
  const role = (value: string | undefined, none: string) =>
    value === undefined ? <Skeleton className="h-4 w-40" /> : value ? <span className="num">{hostOf(value)}</span> : <span className="text-muted-foreground">{none}</span>

  return (
    <PageSection
      title="Who checks whom"
      description="hmnpay reports nothing to its payment engine on one endpoint’s word: what one endpoint says, another must confirm."
    >
      <Panel className="px-5 py-1.5 sm:px-6" data-slot="node-roles">
        <DetailList className="text-[13px]">
          <DetailRow label="Reads the chain (primary)" className="min-h-11">
            <span data-role="primary">{role(node?.primary, "None yet")}</span>
          </DetailRow>
          <DetailRow label="Cross-checks it (verifier)" className="min-h-11">
            <span data-role="verifier">{role(
                node?.verifier,
                node?.solo
                  ? "Nobody: one endpoint is trusted alone"
                  : several
                    ? "Nobody right now: no other endpoint is answering"
                    : "None: there is only one endpoint",
              )}</span>
          </DetailRow>
          <DetailRow label="Confirms log scans (log verifier)" className="min-h-11">
            <span data-role="log-verifier">{role(node?.log_verifier, "None needed right now, or no second log endpoint")}</span>
          </DetailRow>
        </DetailList>
      </Panel>
    </PageSection>
  )
}

/* -------------------------------------------------------------------------------------------
 * RPC endpoints
 * ----------------------------------------------------------------------------------------- */

const STATE: Record<EndpointState, { tone: Tone; label: string; hollow?: boolean; meaning: string }> = {
  healthy: { tone: "success", label: "Healthy", meaning: "Answering, on the right chain, up to date." },
  not_checked: { tone: "neutral", label: "Not asked yet", hollow: true, meaning: "The watcher is starting and has not asked it anything yet." },
  unverified: { tone: "warning", label: "No answer yet", meaning: "It has been asked, but has not answered, so its chain is not verified." },
  failing: { tone: "danger", label: "Failing", meaning: "Paused after several failures in a row; it is tried again regularly." },
  lagging: { tone: "warning", label: "Behind", meaning: "Answering, but too far behind the other endpoints." },
  wrong_chain: { tone: "danger", label: "Wrong chain", meaning: "It answers for another chain. Remove it." },
  unsupported: { tone: "danger", label: "Unusable", meaning: "hmnpay cannot use this URL (a websocket address, for example)." },
  idle: { tone: "neutral", label: "Idle", hollow: true, meaning: "Nothing runs on it: the network configuration as a whole cannot be used." },
}

/** How a state reads. A state this build does not know yet is shown by the server's own word, never as a fault. */
function stateOf(state: string): { tone: Tone; label: string; hollow?: boolean; meaning: string } {
  return (
    (STATE as Record<string, (typeof STATE)[EndpointState]>)[state] ?? {
      tone: "neutral",
      label: state.replaceAll("_", " ") || "Unknown",
      hollow: true,
      meaning: "The server reports a state this version of the interface has no description for.",
    }
  )
}

const ROW_GRID = "sm:grid sm:grid-cols-[minmax(0,1fr)_7rem_4.25rem_6.25rem_1.75rem] sm:items-center sm:gap-x-4"

/** The two-endpoint rule, counted on what is configured. */
function TrustNote({ total }: { total: number }) {
  const enough = total >= 2
  return (
    <span className="flex gap-2.5">
      {enough ? (
        <ShieldCheckIcon aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-success-fg" />
      ) : (
        <ShieldAlertIcon aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-warning-fg" />
      )}
      <span>
        {enough ? (
          <>
            With two or more independent endpoints, a single faulty one cannot fake, revert or mis-time a payment. Use different providers,
            at least two of which serve logs (<span className="num">eth_getLogs</span>): then one cannot hide a payment either.
          </>
        ) : (
          <>
            <span className="font-medium text-foreground">A single endpoint has to be trusted completely</span>: it can invent, hide or
            mis-time payments. Add a second from a different provider, at least two of which serve logs (<span className="num">eth_getLogs</span>).
          </>
        )}
      </span>
    </span>
  )
}

function EndpointsSection({ onAdd }: { onAdd: () => void }) {
  const { settings, setSettings, node } = useDashboard()
  const urls = settings.network.rpc_urls
  const statusOf = (url: string) => node?.endpoints.find((endpoint) => endpoint.url === origin(url))
  // Settings saved by an earlier version can hold two URLs of one endpoint (now refused); the
  // server warns about endpoints that look like one provider, and the page says it in its words.
  const duplicates: [string, string][] = urls.flatMap((url, i) => {
    const other = sameEndpointAs(url, urls.slice(0, i))
    return other ? [[other, url] as [string, string]] : []
  })
  const providers = sameProviderGroups(urls)
  const [busy, setBusy] = React.useState<string | null>(null)

  const test = async (url: string) => {
    setBusy(url)
    try {
      const result = await api.probeRpc({ url, chain_id: settings.network.chain_id })
      if (result.outcome === "ok") toast.success(`${hostOf(url)} answered in ${result.facts.latency_ms} ms`, { description: `Block #${result.facts.block_number}.` })
      else toast.error(`${hostOf(url)}: ${probeProblem(result, settings.network.chain_id)}`)
    } catch (error) {
      toast.error("Couldn’t test the endpoint", { description: describeError(error) })
    } finally {
      setBusy(null)
    }
  }
  const remove = async (url: string) => {
    setBusy(url)
    try {
      setSettings(await api.updateNetwork({ chain_id: settings.network.chain_id, rpc_urls: urls.filter((item) => item !== url) }))
      toast(`Removed ${hostOf(url)}`, { description: "The watcher restarts with the remaining endpoints." })
    } catch (error) {
      toast.error("Couldn’t remove the endpoint", { description: describeError(error) })
    } finally {
      setBusy(null)
    }
  }

  return (
    <PageSection
      title="RPC endpoints"
      description="How hmnpay reads the chain. They are part of what you trust: it learns about payments from them and nowhere else."
      actions={
        <Button onClick={onAdd}>
          <PlusIcon data-icon="inline-start" />
          Add<span className="max-sm:sr-only"> endpoint</span>
        </Button>
      }
    >
      <Panel>
        <div className={cn("hidden border-b border-border px-4 py-2.5 text-[13px] leading-5 text-muted-foreground sm:px-5", ROW_GRID)}>
          <span>Endpoint</span>
          <span>State</span>
          <span className="text-right">Latency</span>
          <span className="text-right">Block</span>
          <span />
        </div>
        <ul className="divide-y divide-border" data-slot="rpc-endpoints">
          {urls.map((url) => (
            <EndpointRow
              key={url}
              url={url}
              status={statusOf(url)}
              node={node}
              busy={busy === url}
              removable={urls.length > 1}
              twiceOf={(() => {
                const pair = duplicates.find(([, b]) => b === url)
                return pair ? urls.indexOf(pair[0]) + 1 : undefined
              })()}
              problem={settings.network.problems.find((p) => p.field === `rpc_urls.${urls.indexOf(url)}`)}
              onTest={() => void test(url)}
              onRemove={() => void remove(url)}
            />
          ))}
        </ul>
        {providers.length > 0 && (
          <div className="flex flex-col gap-2.5 border-t border-border px-4 py-3 sm:px-5" data-slot="endpoint-warnings">
            {providers.map((group) => (
              <Callout key={group.join(" ")} tone="warning" icon={<TriangleAlertIcon />} title="These look like one provider" data-slot="same-provider">
                {sameProviderMessage(group)}
              </Callout>
            ))}
          </div>
        )}
        <div className="border-t border-border bg-muted/40 px-4 py-2.5 text-[13px] leading-5 text-pretty text-muted-foreground sm:px-5">
          <TrustNote total={new Set(urls.map(origin)).size} />
        </div>
      </Panel>
    </PageSection>
  )
}

function EndpointRow({
  url,
  status,
  node,
  busy,
  removable,
  twiceOf,
  problem,
  onTest,
  onRemove,
}: {
  url: string
  status: EndpointStatus | undefined
  node: NodeStatus | undefined
  busy: boolean
  removable: boolean
  /** The position (1-based) of the earlier entry this one is the same endpoint as. */
  twiceOf?: number
  /** What the server says makes this entry unusable (network.problems), if anything. */
  problem?: FieldError
  onTest: () => void
  onRemove: () => void
}) {
  const host = hostOf(url)
  const key = origin(url)
  const state = status ? stateOf(status.state) : null
  const roles = [
    node?.primary === key && "Primary",
    node?.verifier === key && "Verifier",
    node?.log_verifier === key && "Log verifier",
    node?.solo && node.primary === key && "Trusted alone",
  ].filter(Boolean) as string[]
  const answering = status && status.block_number > 0

  return (
    <li data-endpoint={key} data-url={url} data-state={status?.state} data-twice={twiceOf ? "" : undefined} data-problem={problem ? problem.code : undefined} className={cn("relative px-4 py-3 sm:px-5", ROW_GRID)}>
      <div className="min-w-0 pr-9 sm:p-0">
        <div className="flex flex-wrap items-center gap-2">
          <span className="num truncate text-[13px] leading-5" title={key}>
            <span className="text-faint">{url.startsWith("http://") ? "http://" : "https://"}</span>
            {host}
          </span>
          {twiceOf ? (
            <StatusBadge tone="danger" size="sm" dot={false}>
              Same endpoint as #{twiceOf}
            </StatusBadge>
          ) : (
            problem && (
              <StatusBadge tone="danger" size="sm" dot={false}>
                Can’t be used
              </StatusBadge>
            )
          )}
          {roles.map((role) => (
            <StatusBadge key={role} tone={role === "Trusted alone" ? "warning" : "neutral"} size="sm" dot={false} className="text-foreground">
              {role}
            </StatusBadge>
          ))}
        </div>
        <div className={cn("mt-0.5 text-[13px] leading-5 break-words text-muted-foreground", status?.error && !status.healthy && "text-danger-fg")}>
          {twiceOf
            ? `Endpoint #${twiceOf} again, with another path or key: remove one of the two.`
            : problem
              ? problemSentence(problem)
              : status?.error || state?.meaning || (node ? "Not in the watcher’s report yet." : "")}
        </div>
      </div>

      <div className="mt-2 sm:mt-0">
        {state ? (
          <StatusBadge tone={state.tone} hollow={state.hollow} size="sm">
            {state.label}
          </StatusBadge>
        ) : node ? (
          <StatusBadge tone="neutral" hollow size="sm">
            Pending
          </StatusBadge>
        ) : (
          <Skeleton className="h-5 w-16" />
        )}
      </div>
      <span className={cn("hidden text-right text-[13px] sm:block", !answering && "text-faint")}>
        {answering ? (
          <>
            <span className="num">{status.latency_ms}</span> <span className="text-muted-foreground">ms</span>
          </>
        ) : (
          "—"
        )}
      </span>
      <span className={cn("hidden text-right text-[13px] sm:block", !answering && "text-faint")}>{answering ? count(status.block_number) : "—"}</span>

      <div className="absolute top-2.5 right-3 sm:static">
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${host}`} disabled={busy} className="text-muted-foreground pointer-coarse:size-9">
              {busy ? <Spinner /> : <MoreHorizontalIcon />}
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-44">
            <DropdownMenuItem onSelect={onTest}>
              <ZapIcon />
              Test now
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" disabled={!removable} onSelect={onRemove}>
              <Trash2Icon />
              Remove
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </li>
  )
}

/** What is wrong with an endpoint a probe did not accept, in a few words. */
function probeProblem(result: ProbeResult, chainId: number): string {
  switch (result.outcome) {
    case "unreachable":
      return result.facts.error || "nothing answered"
    case "wrong_chain":
      return `it serves chain ${result.facts.chain_id}, not ${chainId}`
    case "token_mismatch":
      return result.facts.token_found ? "the token contract there is a different token" : "the token contract is not there"
    default:
      return "ok"
  }
}

/* -------------------------------------------------------------------------------------------
 * Dialog: add endpoint
 * ----------------------------------------------------------------------------------------- */

function AddEndpointDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const { settings, setSettings } = useDashboard()
  const [url, setUrl] = React.useState("")
  const [testing, setTesting] = React.useState(false)
  const [saving, setSaving] = React.useState(false)
  const [result, setResult] = React.useState<{ url: string; probe: ProbeResult } | null>(null)
  const [failure, setFailure] = React.useState<string | null>(null)
  const trimmed = url.trim()
  const valid = /^https?:\/\/[^\s/]+/.test(trimmed)
  const known = settings.network.rpc_urls.includes(trimmed)
  const sameAs = valid && !known ? sameEndpointAs(trimmed, settings.network.rpc_urls) : undefined
  const providerGroup = valid && !known && !sameAs ? sameProviderGroups([...settings.network.rpc_urls, trimmed]).find((g) => g.some((u) => trimmed.toLowerCase().startsWith(u))) : undefined
  const tested = result?.url === trimmed ? result.probe : null
  const chainId = settings.network.chain_id

  const close = (next: boolean) => {
    onOpenChange(next)
    if (!next) {
      window.setTimeout(() => {
        setUrl("")
        setResult(null)
        setFailure(null)
      }, 150)
    }
  }
  const test = async () => {
    setTesting(true)
    setFailure(null)
    try {
      setResult({ url: trimmed, probe: await api.probeRpc({ url: trimmed, chain_id: chainId }) })
    } catch (error) {
      setFailure(fieldErrors(error).url ?? describeError(error))
    } finally {
      setTesting(false)
    }
  }
  const add = async () => {
    setSaving(true)
    setFailure(null)
    try {
      const next: Settings = await api.updateNetwork({ chain_id: chainId, rpc_urls: [...settings.network.rpc_urls, trimmed] })
      setSettings(next)
      toast.success(`Added ${hostOf(trimmed)}`, { description: "The watcher restarts with the new set of endpoints." })
      close(false)
    } catch (error) {
      const fields = fieldErrors(error)
      setFailure(Object.values(fields)[0] ?? describeError(error))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <form
          className="contents"
          onSubmit={(event) => {
            event.preventDefault()
            if (tested?.outcome === "ok" && !saving) void add()
            else if (valid && !known && !sameAs && !testing) void test()
          }}
        >
          <DialogHeader>
            <DialogTitle>Add RPC endpoint</DialogTitle>
            <DialogDescription>
              An endpoint for {chainName(chainId)} (chain <span className="num">{chainId}</span>). It is tested first: it has to answer for
              the right chain and know the {settings.asset.symbol} contract.
            </DialogDescription>
          </DialogHeader>

          <Field data-invalid={failure || known || sameAs ? true : undefined}>
            <FieldLabel htmlFor="rpc-url">Endpoint URL</FieldLabel>
            <Input
              id="rpc-url"
              value={url}
              placeholder="https://"
              inputMode="url"
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              aria-invalid={failure || known || sameAs ? true : undefined}
              onChange={(event) => {
                setUrl(event.target.value)
                setFailure(null)
              }}
              className="num"
            />
            {known ? (
              <FieldDescription className="text-[13px] text-danger-fg">This endpoint is already in the list.</FieldDescription>
            ) : sameAs ? (
              <FieldDescription className="text-[13px] text-danger-fg" role="alert" data-slot="same-endpoint">
                {sameEndpointMessage(sameAs)}
              </FieldDescription>
            ) : failure ? (
              <FieldDescription className="text-[13px] text-danger-fg" role="alert">
                {failure}
              </FieldDescription>
            ) : (
              <FieldDescription className="text-[13px]">
                A provider key in the URL is stored with the settings and never shown in the node status.
              </FieldDescription>
            )}
          </Field>

          {(testing || tested) && (
            <div className="rounded-lg border border-border bg-background/60" aria-live="polite" data-probe={testing ? "testing" : tested?.outcome}>
              {testing ? (
                <p className="flex items-center gap-2.5 px-3.5 py-3 text-[13px] leading-5 text-muted-foreground">
                  <Spinner className="size-3.5" />
                  Asking {hostOf(trimmed)}
                </p>
              ) : tested?.outcome === "ok" ? (
                <dl className="divide-y divide-border text-[13px] leading-5">
                  <TestLine label="Chain ID">
                    <span className="num">{tested.facts.chain_id}</span>
                  </TestLine>
                  <TestLine label="Latest block">
                    <span className="num">#{tested.facts.block_number.toLocaleString("en-US")}</span>
                  </TestLine>
                  <TestLine label="Latency">
                    <span className="num">{tested.facts.latency_ms}</span> ms
                  </TestLine>
                  <TestLine label="Token contract">
                    {tested.facts.token_symbol}, <span className="num">{tested.facts.token_decimals}</span> decimals
                  </TestLine>
                </dl>
              ) : (
                tested && (
                  <p className="px-3.5 py-3 text-[13px] leading-5 text-danger-fg" role="alert">
                    Not usable: {probeProblem(tested, chainId)}.
                  </p>
                )
              )}
            </div>
          )}

          {providerGroup && (
            <Callout tone="warning" icon={<TriangleAlertIcon />} title="This looks like a provider you already have" data-slot="same-provider">
              {sameProviderMessage(providerGroup)} You can still add it.
            </Callout>
          )}

          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline" type="button">
                Cancel
              </Button>
            </DialogClose>
            {tested?.outcome === "ok" ? (
              <Button type="submit" disabled={saving}>
                {saving ? "Adding…" : "Add endpoint"}
              </Button>
            ) : (
              <Button type="submit" disabled={!valid || known || Boolean(sameAs) || testing}>
                <ZapIcon data-icon="inline-start" />
                {testing ? "Testing" : tested ? "Test again" : "Test connection"}
              </Button>
            )}
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function TestLine({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4 px-3.5 py-2">
      <dt className="text-muted-foreground">{label}</dt>
      <dd>{children}</dd>
    </div>
  )
}

/* -------------------------------------------------------------------------------------------
 * Switching between mainnet and testnet
 * ----------------------------------------------------------------------------------------- */

/** The networks this server has a built-in asset for; the other one is where a switch can go. */
function useOtherChain() {
  const { settings } = useDashboard()
  const status = useQuery("setup-status", (signal) => api.setupStatus(signal))
  const other = status.data?.supported_chains.find((chain) => chain.chain_id !== settings.network.chain_id) ?? null
  return { other, loading: !status.data && !status.error, error: status.error, reload: status.reload }
}

function SwitchSection({ onSwitch }: { onSwitch: () => void }) {
  const { settings } = useDashboard()
  const { other, loading } = useOtherChain()
  if (!other && !loading) return null
  const testnet = settings.network.testnet

  return (
    <PageSection
      title="Mainnet or testnet"
      description="One instance watches one network at a time. Switching is for moving from a trial on the testnet to real payments, or back."
    >
      <Panel className="flex flex-wrap items-center justify-between gap-x-6 gap-y-3 px-5 py-4 sm:px-6" data-slot="network-switch">
        <div className="min-w-0 text-[13px] leading-5">
          <p className="text-sm leading-5 font-medium">
            {chainName(settings.network.chain_id)} <span className="font-normal text-muted-foreground">is in use</span>
          </p>
          <p className="mt-0.5 text-pretty text-muted-foreground">
            {testnet ? "Test tokens without value." : "Real funds."} The server refuses to switch while any payment is still open: its address
            would stop being watched.
          </p>
        </div>
        {other ? (
          <Button variant="outline" onClick={onSwitch}>
            <ArrowLeftRightIcon data-icon="inline-start" />
            Switch to {chainName(other.chain_id)}
          </Button>
        ) : (
          <Skeleton className="h-8 w-44" />
        )}
      </Panel>
    </PageSection>
  )
}

interface Candidate {
  url: string
  probe: ProbeResult
}

/**
 * Switching the chain is PUT /settings/network with the other chain id and that chain's
 * endpoints. The server decides whether it may happen: it answers 409 "open_payments" while a
 * payment is awaiting, detected or confirming, and the dialog shows that refusal with the way
 * out, instead of guessing beforehand from numbers that may be stale.
 */
function SwitchNetworkDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const { settings, setSettings, counts } = useDashboard()
  const { other } = useOtherChain()
  const [url, setUrl] = React.useState("")
  const [endpoints, setEndpoints] = React.useState<Candidate[]>([])
  const [testing, setTesting] = React.useState(false)
  const [problem, setProblem] = React.useState<string | null>(null)
  const [saving, setSaving] = React.useState(false)
  const [failure, setFailure] = React.useState<string | null>(null)
  const [refused, setRefused] = React.useState(false)
  const [understood, setUnderstood] = React.useState(false)

  const from = settings.network.chain_id
  const to = other?.chain_id ?? 0
  const trimmed = url.trim()
  const valid = /^https?:\/\/[^\s/]+/.test(trimmed)
  const known = endpoints.some((item) => item.url === trimmed)
  const suggestion = SUGGESTED_RPC[to]
  const openNow = counts?.open ?? 0

  const close = (next: boolean) => {
    onOpenChange(next)
    if (!next) {
      window.setTimeout(() => {
        setUrl("")
        setEndpoints([])
        setProblem(null)
        setFailure(null)
        setRefused(false)
        setUnderstood(false)
      }, 150)
    }
  }
  const test = async () => {
    setTesting(true)
    setProblem(null)
    try {
      const probe = await api.probeRpc({ url: trimmed, chain_id: to })
      if (probe.outcome === "ok") {
        setEndpoints((current) => [...current, { url: trimmed, probe }])
        setUrl("")
      } else {
        setProblem(`Not usable: ${probeProblem(probe, to)}.`)
      }
    } catch (error) {
      setProblem(fieldErrors(error).url ?? describeError(error))
    } finally {
      setTesting(false)
    }
  }
  const save = async () => {
    setSaving(true)
    setFailure(null)
    setRefused(false)
    try {
      setSettings(await api.updateNetwork({ chain_id: to, rpc_urls: endpoints.map((item) => item.url) }))
      toast.success(`Now on ${chainName(to)}`, { description: "The watcher restarts on the new network. Payment links need saving once before they take payments there." })
      close(false)
    } catch (error) {
      if (isApiError(error) && error.code === "open_payments") setRefused(true)
      else setFailure(Object.values(fieldErrors(error))[0] ?? describeError(error))
    } finally {
      setSaving(false)
    }
  }

  const ready = endpoints.length > 0 && understood && !saving

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg" data-slot="switch-network-dialog">
        <form
          className="contents"
          onSubmit={(event) => {
            event.preventDefault()
            // Enter in the URL field tests that URL; the switch itself is its own button.
            if (valid && !known && !testing) void test()
          }}
        >
          <DialogHeader>
            <DialogTitle>Switch to {chainName(to)}</DialogTitle>
            <DialogDescription>
              From {chainName(from)} (chain <span className="num">{from}</span>) to {chainName(to)} (chain <span className="num">{to}</span>,{" "}
              {other?.testnet ? "test tokens without value" : "real funds"}). The wallet key stays; the endpoints do not, they belong to a chain.
            </DialogDescription>
          </DialogHeader>

          <ul className="flex flex-col gap-1.5 text-[13px] leading-5 text-pretty text-muted-foreground" data-slot="switch-consequences">
            <Consequence>Finished payments stay in Payments. The overview counts the network in use only.</Consequence>
            <Consequence>
              Addresses of {chainName(from)} are no longer watched: money sent late to a finished payment there will not be noticed.
            </Consequence>
            <Consequence>
              Payment links made for {chainName(from)} refuse new checkouts until each is opened and saved once, which moves it to {chainName(to)}.
            </Consequence>
          </ul>

          <Field data-invalid={problem || known ? true : undefined}>
            <FieldLabel htmlFor="switch-rpc-url">{endpoints.length === 0 ? `RPC endpoint for ${chainName(to)}` : "Another endpoint"}</FieldLabel>
            <div className="flex gap-2">
              <Input
                id="switch-rpc-url"
                value={url}
                placeholder="https://"
                inputMode="url"
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                aria-invalid={problem || known ? true : undefined}
                onChange={(event) => {
                  setUrl(event.target.value)
                  setProblem(null)
                }}
                className="num min-w-0 flex-1"
              />
              <Button type="submit" variant="outline" disabled={!valid || known || testing}>
                {testing ? <Spinner /> : <ZapIcon data-icon="inline-start" />}
                {testing ? "Testing" : "Test and add"}
              </Button>
            </div>
            {known ? (
              <FieldDescription className="text-[13px] text-danger-fg">This endpoint is already in the list.</FieldDescription>
            ) : problem ? (
              <FieldDescription className="text-[13px] text-danger-fg" role="alert">
                {problem}
              </FieldDescription>
            ) : (
              <FieldDescription className="text-[13px]">
                Each endpoint is tested first: it has to answer for chain <span className="num">{to}</span> and know the {other?.asset.symbol ?? settings.asset.symbol} contract
                there.
                {suggestion && trimmed !== suggestion && !endpoints.some((item) => item.url === suggestion) && (
                  <>
                    {" "}
                    <button
                      type="button"
                      onClick={() => setUrl(suggestion)}
                      className="focus-ring rounded-sm underline decoration-border-strong underline-offset-4 transition-colors hover:text-foreground"
                    >
                      Use a public endpoint
                    </button>
                  </>
                )}
              </FieldDescription>
            )}
          </Field>

          {endpoints.length > 0 && (
            <div className="rounded-lg border border-border bg-background/60">
              <ul className="divide-y divide-border" data-slot="switch-endpoints">
                {endpoints.map((item) => (
                  <li key={item.url} className="flex min-h-10 items-center gap-3 py-0.5 pr-1.5 pl-3.5 text-[13px] leading-5">
                    <span className="flex size-4 shrink-0 items-center justify-center rounded-full bg-success-subtle text-success-fg">
                      <CheckIcon strokeWidth={3} className="size-2.5" />
                    </span>
                    <span className="num min-w-0 flex-1 truncate" title={origin(item.url)}>
                      {hostOf(item.url)}
                    </span>
                    <span className="text-muted-foreground max-sm:hidden">
                      block <span className="num">#{item.probe.facts.block_number.toLocaleString("en-US")}</span>
                    </span>
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon-sm"
                      aria-label={`Remove ${hostOf(item.url)}`}
                      className="text-muted-foreground pointer-coarse:size-10"
                      onClick={() => setEndpoints((current) => current.filter((entry) => entry.url !== item.url))}
                    >
                      <XIcon />
                    </Button>
                  </li>
                ))}
              </ul>
              <div className="border-t border-border bg-muted/40 px-3.5 py-2.5 text-[13px] leading-5 text-pretty text-muted-foreground">
                <TrustNote total={new Set(endpoints.map((item) => origin(item.url))).size} />
              </div>
            </div>
          )}

          {refused ? (
            <Callout tone="danger" icon={<TriangleAlertIcon />} title="The server refused: payments are still open" role="alert" data-slot="switch-refused">
              A payment that is awaiting, detected or confirming on {chainName(from)} would lose its watcher, so nothing was changed. Wait
              until every open payment is paid or has expired, or cancel the ones still awaiting, then switch.{" "}
              <Link href="/payments?status=open" className="focus-ring rounded-sm font-medium text-foreground underline decoration-border-strong underline-offset-4">
                Show open payments
              </Link>
            </Callout>
          ) : failure ? (
            <Callout tone="danger" icon={<TriangleAlertIcon />} title="Couldn’t switch" role="alert">
              {failure}
            </Callout>
          ) : (
            openNow > 0 && (
              <Callout tone="warning" icon={<TriangleAlertIcon />} title={`${openNow.toLocaleString("en-US")} ${openNow === 1 ? "payment is" : "payments are"} open right now`} data-slot="switch-open-warning">
                The server will refuse the switch while {openNow === 1 ? "it is" : "they are"} open.
              </Callout>
            )
          )}

          <label className="flex min-h-11 cursor-pointer items-center gap-3 rounded-lg border border-border bg-muted/40 px-4 py-2.5 text-sm leading-5">
            <Checkbox checked={understood} onCheckedChange={(next) => setUnderstood(next === true)} />
            <span className="font-medium">
              {other?.testnet ? "I am moving this instance to test tokens" : "I am moving this instance to real funds"}
            </span>
          </label>

          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline" type="button">
                Cancel
              </Button>
            </DialogClose>
            <Button type="button" disabled={!ready} onClick={() => void save()}>
              {saving ? "Switching…" : `Switch to ${chainName(to)}`}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function Consequence({ children }: { children: React.ReactNode }) {
  return (
    <li className="flex gap-2.5">
      <span aria-hidden="true" className="mt-[9px] size-1 shrink-0 rounded-full bg-faint" />
      <span>{children}</span>
    </li>
  )
}
