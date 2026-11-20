"use client"

import Link from "next/link"

import { StatusDot } from "@/components/hmn"
import { chainName, nodeHealth } from "@/lib/node-health"
import { cn } from "@/lib/utils"

export { chainName, nodeHealth, type NodeHealth } from "@/lib/node-health"

import { useDashboard } from "./session"

/**
 * Sidebar node indicator: network, sync state, block height, latency. Links to network settings,
 * where the same status is spelled out. Collapses to a single dot when the sidebar is in icon mode.
 */
export function NodeStatus({ className }: { className?: string }) {
  const { node, nodeError, settings } = useDashboard()
  const health = nodeHealth(node, Boolean(nodeError))
  const network = chainName(settings.network.chain_id)
  const height = node?.head?.number ?? node?.latest_block ?? 0
  return (
    <Link
      href="/settings/network"
      aria-label={`${network} node: ${health.label}. ${health.detail}`}
      data-node-state={health.label}
      className={cn(
        "focus-ring block rounded-lg border border-sidebar-border bg-card px-2.5 py-2 text-xs leading-4 shadow-card transition-colors hover:border-border-strong",
        "group-data-[collapsible=icon]:flex group-data-[collapsible=icon]:size-8 group-data-[collapsible=icon]:items-center group-data-[collapsible=icon]:justify-center group-data-[collapsible=icon]:p-0",
        className,
      )}
    >
      <span className="flex items-center justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2 font-medium">
          <span className="flex size-4 shrink-0 items-center justify-center">
            <StatusDot tone={health.tone} live={health.live} />
          </span>
          <span className="truncate group-data-[collapsible=icon]:hidden">{network}</span>
        </span>
        <span
          className={cn(
            "shrink-0 group-data-[collapsible=icon]:hidden",
            health.tone === "success" || health.tone === "neutral"
              ? "text-muted-foreground"
              : health.tone === "danger"
                ? "text-danger-fg"
                : health.tone === "info"
                  ? "text-info-fg"
                  : "text-warning-fg",
          )}
        >
          {health.label}
        </span>
      </span>
      <span className="num mt-1 flex items-center justify-between gap-2 pl-6 text-[11px] text-faint group-data-[collapsible=icon]:hidden">
        <span>{height > 0 ? `#${height}` : "—"}</span>
        <span className="truncate">{health.detail}</span>
      </span>
    </Link>
  )
}
