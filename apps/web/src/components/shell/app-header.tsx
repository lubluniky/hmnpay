"use client"

import * as React from "react"
import Link from "next/link"
import { usePathname } from "next/navigation"

import { MerchantAvatar, SoundToggle, ThemeToggle } from "@/components/hmn"
import { SidebarTrigger } from "@/components/ui/sidebar"
import { truncateMiddle } from "@/lib/format"

import { CommandMenuTrigger } from "./command-menu"
import { trail } from "./nav"
import { useDashboard } from "./session"

/**
 * Top bar: sidebar toggle, instance identity + breadcrumb, then ⌘K, sound and the theme toggle.
 * Page-level actions do NOT go here — they belong to the page's `PageHeader`.
 */
export function AppHeader() {
  const { settings } = useDashboard()
  const merchant = { name: settings.merchant.name, logoUrl: settings.merchant.logo_url }
  const pathname = usePathname()
  const crumbs = trail(pathname)

  return (
    <header className="sticky top-0 z-20 flex h-14 shrink-0 items-center gap-2 border-b border-border bg-background px-3 sm:px-4 lg:px-6">
      <SidebarTrigger className="-ml-1 size-8 text-muted-foreground hover:text-foreground" />
      <span aria-hidden="true" className="mx-1 hidden h-4 w-px bg-border sm:block" />
      <nav aria-label="Breadcrumb" className="flex min-w-0 flex-1 items-center gap-2 text-sm">
        <Link href="/" className="focus-ring hidden items-center gap-2 rounded-md sm:flex">
          <MerchantAvatar name={merchant.name} logoUrl={merchant.logoUrl} className="size-5 rounded-[5px] text-[10px]" />
          <span className="max-w-40 truncate font-medium">{merchant.name}</span>
        </Link>
        {crumbs.map((crumb, i) => {
          const last = i === crumbs.length - 1
          const title = crumb.title.startsWith("pay_") ? truncateMiddle(crumb.title, 8, 4) : crumb.title
          return (
            <React.Fragment key={`${crumb.title}-${i}`}>
              <span aria-hidden="true" className={i === 0 ? "hidden text-faint sm:inline" : "text-faint"}>
                /
              </span>
              {crumb.href && !last ? (
                <Link
                  href={crumb.href}
                  className="focus-ring truncate rounded-sm text-muted-foreground transition-colors hover:text-foreground"
                >
                  {title}
                </Link>
              ) : (
                <span aria-current="page" className={crumb.title.startsWith("pay_") ? "num truncate" : "truncate font-medium sm:font-normal"}>
                  {title}
                </span>
              )}
            </React.Fragment>
          )
        })}
      </nav>
      <div className="flex shrink-0 items-center gap-1">
        <CommandMenuTrigger />
        <SoundToggle className="size-8" />
        <ThemeToggle className="size-8" />
      </div>
    </header>
  )
}
