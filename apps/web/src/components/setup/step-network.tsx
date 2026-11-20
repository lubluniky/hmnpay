"use client"

import { CheckIcon, PlusIcon, RotateCwIcon, ShieldAlertIcon, ShieldCheckIcon, TriangleAlertIcon, XIcon, ZapIcon } from "lucide-react"

import { FlowField, FlowInput } from "@/components/auth/flow"
import { Address, Callout, NETWORK_NAME, NetworkIcon, StatusBadge } from "@/components/hmn"
import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import type { SetupStatus } from "@/lib/api/types"
import { sameEndpointAs, sameEndpointMessage, sameProviderGroups, sameProviderMessage } from "@/lib/endpoints"
import { cn } from "@/lib/utils"

import { CHECK_IDS, CHECK_LABEL, checkStates, hostOf, isUrl, SUGGESTED_RPC, type CheckId, type CheckState, type RpcProbe, type SetupForm } from "./data"
import { StepHeader } from "./parts"

const chainLabel = (chainId: number, testnet: boolean) => (chainId === 56 ? NETWORK_NAME : testnet ? "BSC Testnet" : `Chain ${chainId}`)

export function StepNetwork({
  form,
  set,
  chainId,
  chains,
  probe,
  onTest,
  showErrors,
  errors,
}: {
  form: SetupForm
  set: (patch: Partial<SetupForm>) => void
  chainId: number
  chains: SetupStatus["supported_chains"]
  probe: RpcProbe
  onTest: () => void
  showErrors: boolean
  /** What the server said about fields of this step ("rpc_urls.0", …). */
  errors: Record<string, string>
}) {
  const typed = form.rpcUrl.trim()
  const serverError = Object.entries(errors).find(([name]) => name.startsWith("rpc_urls") || name === "chain_id")?.[1]
  // Another URL of an endpoint already in the list (only the path or the key differs).
  const sameAs = typed !== "" && isUrl(typed) && !form.endpoints.includes(typed) ? sameEndpointAs(typed, form.endpoints) : undefined
  const urlError =
    (sameAs ? sameEndpointMessage(sameAs) : undefined) ??
    serverError ??
    (showErrors && typed !== "" && !isUrl(typed)
      ? "Enter a full URL, starting with https:// or http://."
      : showErrors && form.endpoints.length === 0 && typed === ""
        ? "Add at least one endpoint."
        : undefined)
  const testing = probe.status === "testing"
  const added = form.endpoints.includes(typed)
  const suggestion = SUGGESTED_RPC[chainId]
  const testnet = chains.find((c) => c.chain_id === chainId)?.testnet ?? false

  return (
    <>
      <StepHeader title={`Connect to ${chainLabel(chainId, testnet)}`}>
        hmnpay reads the chain through RPC endpoints to see payments arrive. Test each endpoint you want it to use; it is added
        once it answers for the right chain and token.
      </StepHeader>

      {chains.length > 1 && (
        <Tabs
          value={String(chainId)}
          onValueChange={(value) => {
            const next = Number(value)
            // Endpoints of the other chain are not endpoints of this one.
            set({ chainId: next, endpoints: [], rpcUrl: SUGGESTED_RPC[next] ?? "" })
          }}
          className="mt-6"
        >
          <TabsList aria-label="Network">
            {chains.map((chain) => (
              <TabsTrigger key={chain.chain_id} value={String(chain.chain_id)} className="px-3">
                {chain.testnet ? "Testnet" : "Mainnet"}
                <span className="num ml-1.5 text-xs text-muted-foreground">{chain.chain_id}</span>
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>
      )}

      <FlowField
        className="mt-6"
        label={form.endpoints.length === 0 ? "RPC endpoint" : "Another RPC endpoint"}
        htmlFor="setup-rpc"
        error={urlError}
        action={
          suggestion &&
          typed !== suggestion &&
          !form.endpoints.includes(suggestion) && (
            <button
              type="button"
              onClick={() => set({ rpcUrl: suggestion })}
              className="focus-ring rounded-sm text-muted-foreground underline decoration-border-strong underline-offset-4 transition-colors hover:text-foreground"
            >
              Use a public endpoint
            </button>
          )
        }
      >
        <div className="flex flex-col gap-2 sm:flex-row">
          <FlowInput
            id="setup-rpc"
            name="rpc-url"
            type="url"
            inputMode="url"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            placeholder="https://"
            value={form.rpcUrl}
            onChange={(e) => set({ rpcUrl: e.target.value })}
            aria-invalid={urlError ? true : undefined}
            className="font-mono text-[15px] tracking-[-0.01em] md:text-[13px]"
          />
          <Button
            type="button"
            variant="outline"
            onClick={onTest}
            disabled={testing || typed === "" || Boolean(sameAs) || (added && probe.status === "ok")}
            className="h-11 shrink-0 px-4 text-sm sm:w-[164px]"
          >
            {testing ? <Spinner /> : probe.status === "idle" ? form.endpoints.length > 0 ? <PlusIcon /> : <ZapIcon /> : <RotateCwIcon />}
            {testing ? "Testing" : probe.status === "idle" ? (form.endpoints.length > 0 ? "Test and add" : "Test connection") : "Test again"}
          </Button>
        </div>
      </FlowField>

      {(typed !== "" || probe.status !== "idle") && <Readout className="mt-4" url={form.rpcUrl} probe={probe} chainId={chainId} />}

      {form.endpoints.length > 0 && (
        <section aria-label="Endpoints hmnpay will use" className="mt-4 overflow-hidden rounded-xl border border-border-strong bg-background/60">
          <header className="flex min-h-10 items-center justify-between gap-3 border-b border-border px-4 py-2 text-[13px] leading-5">
            <span className="font-medium">Endpoints hmnpay will use</span>
            <span className="num text-muted-foreground">{form.endpoints.length}</span>
          </header>
          <ul className="divide-y divide-border">
            {form.endpoints.map((url) => (
              <li key={url} className="flex min-h-10 items-center gap-3 py-0.5 pr-1.5 pl-4 text-[13px] leading-5">
                <span className="flex size-4 shrink-0 items-center justify-center rounded-full bg-success-subtle text-success-fg">
                  <CheckIcon strokeWidth={3} className="size-2.5" />
                </span>
                <span className="num min-w-0 flex-1 truncate" title={url}>
                  {hostOf(url)}
                </span>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  aria-label={`Remove ${hostOf(url)}`}
                  className="text-muted-foreground pointer-coarse:size-10"
                  onClick={() => set({ endpoints: form.endpoints.filter((item) => item !== url) })}
                >
                  <XIcon />
                </Button>
              </li>
            ))}
          </ul>
        </section>
      )}

      {sameProviderGroups(form.endpoints).map((group) => (
        <Callout key={group.join(" ")} tone="warning" icon={<TriangleAlertIcon />} title="These look like one provider" className="mt-4" data-slot="same-provider">
          {sameProviderMessage(group)}
        </Callout>
      ))}

      <TrustNote count={form.endpoints.length} className="mt-5" />
    </>
  )
}

/**
 * The rule the server documents for RPC endpoints, where the choice is made: hmnpay only knows
 * what its endpoints tell it. (Whether an endpoint serves logs is found out by the watcher once
 * it runs; the node status in the dashboard reports it.)
 */
function TrustNote({ count, className }: { count: number; className?: string }) {
  const enough = count >= 2
  return (
    <p className={cn("flex gap-2.5 text-[13px] leading-5 text-pretty text-muted-foreground", className)}>
      {enough ? (
        <ShieldCheckIcon aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-success-fg" />
      ) : (
        <ShieldAlertIcon aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-warning-fg" />
      )}
      <span>
        {enough ? (
          <>
            <span className="font-medium text-foreground">Good: {count} endpoints.</span> hmnpay reports nothing on one endpoint’s word, so
            a single faulty one cannot fake or revert a payment. Use different providers, and make sure at least two of them serve logs (
            <span className="num">eth_getLogs</span>; many free endpoints don’t). The dashboard’s node status says if one doesn’t.
          </>
        ) : (
          <>
            <span className="font-medium text-foreground">
              {count === 0 ? "Add at least two independent endpoints." : "One endpoint has to be trusted completely."}
            </span>{" "}
            With a single endpoint, that endpoint can invent, hide or mis-time payments. Add a second from a different provider (ideally
            one you run), at least two of which serve logs (<span className="num">eth_getLogs</span>; many free endpoints don’t). You can
            add more later in Settings.
          </>
        )}
      </span>
    </p>
  )
}

/* -------------------------------- readout --------------------------------- */

function Readout({ url, probe, chainId, className }: { url: string; probe: RpcProbe; chainId: number; className?: string }) {
  const states = checkStates(probe)
  const host = hostOf(url)
  const result = probe.result
  const facts = result?.facts
  const outcome = result?.outcome

  const values: Record<CheckId, React.ReactNode> = {
    reach: facts && (
      <>
        <span className="num text-foreground">{facts.latency_ms}</span> ms
      </>
    ),
    chain: facts && (
      <span className="inline-flex items-center gap-1.5">
        <span className="num text-foreground">{facts.chain_id}</span>
        <NetworkIcon size={14} />
        <span className="max-sm:hidden">{facts.chain_id === 56 ? NETWORK_NAME : "BSC Testnet"}</span>
      </span>
    ),
    block: facts && <span className="num text-foreground">#{facts.block_number.toLocaleString("en-US")}</span>,
    token: result && (
      <span className="inline-flex items-center gap-2">
        <span className="num max-sm:hidden">
          {result.facts.token_symbol} · {result.facts.token_decimals} decimals
        </span>
        <Address value={result.expected.contract} copy={false} chainId={result.expected.chain_id} className="-mr-1.5 text-foreground" />
      </span>
    ),
  }

  const failures: Partial<Record<CheckId, React.ReactNode>> = {
    reach: facts?.error || probe.failure || "No answer",
    chain: facts && (
      <>
        <span className="num">{facts.chain_id}</span>, not <span className="num">{chainId}</span>
      </>
    ),
    token: facts && (facts.token_found ? `${facts.token_symbol || "Another token"}, ${facts.token_decimals} decimals` : "Not found here"),
  }

  return (
    <section
      aria-label="Connection test"
      aria-live="polite"
      data-probe={probe.status}
      className={cn("overflow-hidden rounded-xl border border-border-strong bg-background/60", className)}
    >
      <header className="flex min-h-11 items-center justify-between gap-3 border-b border-border px-4 py-2">
        <span className="num min-w-0 truncate text-[13px] leading-5 text-muted-foreground">{host}</span>
        {probe.status === "ok" && <StatusBadge tone="success">Connected</StatusBadge>}
        {probe.status === "error" && (
          <StatusBadge tone="danger">
            {outcome === "wrong_chain" ? "Wrong network" : outcome === "token_mismatch" ? "Wrong token" : "Can’t connect"}
          </StatusBadge>
        )}
        {probe.status === "testing" && <StatusBadge tone="info">Testing</StatusBadge>}
        {probe.status === "idle" && (
          <StatusBadge tone="neutral" hollow>
            Not tested
          </StatusBadge>
        )}
      </header>

      <ul className="divide-y divide-border">
        {CHECK_IDS.map((id) => {
          const state = states[id]
          return (
            <li key={id} className="flex min-h-11 items-center gap-3 px-4 py-2 text-sm">
              <CheckMark state={state} />
              <span className={cn("flex-1", state === "todo" || state === "skipped" ? "text-faint" : "text-foreground")}>{CHECK_LABEL[id]}</span>
              <span
                key={state}
                className={cn(
                  "min-w-0 text-right text-[13px] leading-5",
                  state === "ok" && "text-muted-foreground motion-safe:animate-rise",
                  state === "fail" && "font-medium break-words text-danger-fg",
                  (state === "todo" || state === "skipped" || state === "running") && "text-faint",
                )}
              >
                {state === "ok" && values[id]}
                {state === "fail" && failures[id]}
                {state === "running" && "checking"}
                {state === "skipped" && "skipped"}
              </span>
            </li>
          )
        })}
      </ul>

      {probe.status === "error" && (
        <p className="border-t border-danger-border bg-danger-subtle px-4 py-3 text-[13px] leading-5 text-pretty text-muted-foreground">
          {outcome === "wrong_chain" ? (
            <>
              <span className="font-medium text-foreground">This endpoint serves a different network.</span> hmnpay needs chain ID{" "}
              <span className="num text-foreground">{chainId}</span>.
            </>
          ) : outcome === "token_mismatch" ? (
            <>
              <span className="font-medium text-foreground">The token contract is not what it should be here.</span> The endpoint answers
              for the right chain, but the contract hmnpay watches is missing or is a different token. Use another endpoint.
            </>
          ) : outcome === "unreachable" ? (
            <>
              <span className="font-medium text-foreground">Nothing usable answered at this address.</span> Check the URL and the port, that
              the node is running, and that this server is allowed to reach it.
            </>
          ) : (
            <>
              <span className="font-medium text-foreground">The test could not be made.</span> {probe.failure}
            </>
          )}
        </p>
      )}
    </section>
  )
}

function CheckMark({ state }: { state: CheckState }) {
  return (
    <span aria-hidden="true" className="flex size-4 shrink-0 items-center justify-center">
      {state === "ok" && (
        <span className="flex size-4 items-center justify-center rounded-full bg-success-subtle text-success-fg motion-safe:animate-pop-in">
          <CheckIcon strokeWidth={3} className="size-2.5" />
        </span>
      )}
      {state === "fail" && (
        <span className="flex size-4 items-center justify-center rounded-full bg-danger-subtle text-danger-fg motion-safe:animate-pop-in">
          <XIcon strokeWidth={3} className="size-2.5" />
        </span>
      )}
      {state === "running" && <Spinner className="size-3.5 text-muted-foreground" />}
      {(state === "todo" || state === "skipped") && <span className="size-2 rounded-full border border-border-strong" />}
    </span>
  )
}
