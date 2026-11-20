"use client"

import * as React from "react"

import { ErrorState, LogoMark, SoundProvider } from "@/components/hmn"
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar"

import { AppHeader } from "./app-header"
import { AppSidebar } from "./app-sidebar"
import { CommandMenuProvider } from "./command-menu"
import { DashboardProvider, type GateState } from "./session"

/** The sidebar remembers whether it was collapsed (the cookie ui/sidebar.tsx writes). */
function sidebarWasOpen() {
  return !document.cookie.split("; ").includes("sidebar_state=false")
}

/** What stands in for the dashboard while the session is being checked, or when that failed. */
function Gate({ gate }: { gate: GateState }) {
  if (gate.state === "error") {
    return (
      <div className="flex min-h-svh items-center justify-center p-6">
        <ErrorState error={gate.error} title="hmnpay can’t load the dashboard" onRetry={gate.retry} className="w-full max-w-md" />
      </div>
    )
  }
  return (
    <div className="flex min-h-svh items-center justify-center" role="status" aria-label="Loading">
      <LogoMark className="size-6 animate-pulse text-muted-foreground" />
    </div>
  )
}

function Chrome({ children }: { children: React.ReactNode }) {
  // Rendered only once the session is known, so always in the browser: the cookie can be read.
  const [open, setOpen] = React.useState(sidebarWasOpen)
  return (
    <CommandMenuProvider>
      <SidebarProvider open={open} onOpenChange={setOpen} style={{ "--sidebar-width": "15rem" } as React.CSSProperties}>
        <AppSidebar />
        <SidebarInset className="min-w-0">
          <AppHeader />
          <div className="flex min-w-0 flex-1 flex-col">{children}</div>
        </SidebarInset>
      </SidebarProvider>
    </CommandMenuProvider>
  )
}

/** Merchant dashboard chrome: sidebar + top bar + ⌘K. Used once, by `(dashboard)/layout.tsx`. */
export function DashboardShell({ children }: { children: React.ReactNode }) {
  return (
    <SoundProvider surface="dashboard">
      <DashboardProvider fallback={(gate) => <Gate gate={gate} />}>
        <Chrome>{children}</Chrome>
      </DashboardProvider>
    </SoundProvider>
  )
}
