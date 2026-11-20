"use client"

import Link from "next/link"
import { usePathname } from "next/navigation"

import { cn } from "@/lib/utils"

import { isActive, settingsNav } from "./nav"

/**
 * Settings sub-navigation: a grouped vertical list on desktop, a single horizontally
 * scrolling row of tabs on mobile.
 */
export function SettingsNav({ className }: { className?: string }) {
  const pathname = usePathname()
  return (
    <nav
      aria-label="Settings"
      className={cn(
        "no-scrollbar -mx-4 flex gap-6 overflow-x-auto border-b border-border px-4 sm:-mx-6 sm:px-6 lg:mx-0 lg:flex-col lg:gap-5 lg:overflow-visible lg:border-b-0 lg:px-0",
        className,
      )}
    >
      {settingsNav.map((group) => (
        <div key={group.label} className="flex gap-6 lg:flex-col lg:gap-0.5">
          <div className="hidden px-2.5 pb-1.5 text-[11px] leading-4 font-medium tracking-wide text-faint lg:block">
            {group.label}
          </div>
          {group.items.map((item) => {
            const active = isActive(pathname, item)
            return (
              <Link
                key={item.href}
                href={item.href}
                aria-current={active ? "page" : undefined}
                className={cn(
                  "focus-ring relative flex h-10 shrink-0 items-center text-sm whitespace-nowrap transition-colors lg:h-8 lg:rounded-md lg:px-2.5 lg:text-[13px]",
                  active
                    ? "font-medium text-foreground after:absolute after:inset-x-0 after:bottom-0 after:h-0.5 after:bg-foreground lg:bg-accent lg:after:hidden"
                    : "text-muted-foreground hover:text-foreground lg:hover:bg-accent/60",
                )}
              >
                {item.title}
              </Link>
            )
          })}
        </div>
      ))}
    </nav>
  )
}
