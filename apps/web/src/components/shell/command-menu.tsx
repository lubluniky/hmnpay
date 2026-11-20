"use client"

import * as React from "react"
import { useRouter } from "next/navigation"
import { MoonIcon, PlusIcon, SearchIcon, SunIcon } from "lucide-react"
import { useTheme } from "next-themes"

import { Amount, PaymentStatusBadge } from "@/components/hmn"
import { Button } from "@/components/ui/button"
import {
  CommandDialog,
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "@/components/ui/command"
import { Kbd } from "@/components/ui/kbd"
import { api } from "@/lib/api/client"
import { useQuery } from "@/lib/api/use-query"
import { cn } from "@/lib/utils"

import { paymentLabel } from "@/components/payments/lib"

import { developerNav, mainNav, settingsNav } from "./nav"

const CommandMenuContext = React.createContext<{ open: () => void } | null>(null)

/** Owns the ⌘K palette. Wrap the dashboard once; open it from anywhere with `useCommandMenu()`. */
export function CommandMenuProvider({ children }: { children: React.ReactNode }) {
  const [isOpen, setOpen] = React.useState(false)
  const router = useRouter()
  const { resolvedTheme, setTheme } = useTheme()
  // Loaded when the palette is first opened, refreshed each time it opens again.
  const recent = useQuery(isOpen ? "payments/recent" : null, (signal) => api.payments({ limit: 5 }, signal), { keepPrevious: true })
  const payments = recent.data?.data ?? []

  React.useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key.toLowerCase() === "k" && (event.metaKey || event.ctrlKey)) {
        event.preventDefault()
        setOpen((v) => !v)
      }
    }
    window.addEventListener("keydown", onKeyDown)
    return () => window.removeEventListener("keydown", onKeyDown)
  }, [])

  const value = React.useMemo(() => ({ open: () => setOpen(true) }), [])
  const run = (fn: () => void) => () => {
    setOpen(false)
    fn()
  }
  const go = (href: string) => run(() => router.push(href))
  const pages = [...mainNav, ...settingsNav.flatMap((g) => g.items)].filter(
    (item, i, all) => all.findIndex((x) => x.href === item.href) === i,
  )

  return (
    <CommandMenuContext.Provider value={value}>
      {children}
      <CommandDialog
        open={isOpen}
        onOpenChange={setOpen}
        title="Command menu"
        description="Search payments, jump to a page or run an action."
        className="sm:max-w-xl"
      >
        <Command>
          <CommandInput placeholder="Search payments, pages, actions…" />
          <CommandList className="max-h-[min(420px,60vh)]">
            <CommandEmpty>Nothing found.</CommandEmpty>
            <CommandGroup heading="Actions">
              <CommandItem onSelect={go("/payments?new=1")}>
                <PlusIcon />
                Create payment
              </CommandItem>
              <CommandItem onSelect={go("/payment-links?new=1")}>
                <PlusIcon />
                Create payment link
              </CommandItem>
              <CommandItem onSelect={run(() => setTheme(resolvedTheme === "dark" ? "light" : "dark"))}>
                <SunIcon className="hidden dark:block" />
                <MoonIcon className="block dark:hidden" />
                Switch theme
              </CommandItem>
            </CommandGroup>
            <CommandSeparator />
            <CommandGroup heading="Go to">
              {pages.map((item) => (
                <CommandItem key={item.href} onSelect={go(item.href)} keywords={developerNav.includes(item) ? ["developers"] : undefined}>
                  <item.icon />
                  {item.title}
                </CommandItem>
              ))}
            </CommandGroup>
            {payments.length > 0 && (
              <>
                <CommandSeparator />
                <CommandGroup heading="Recent payments">
                  {payments.map((payment) => (
                    <CommandItem
                      key={payment.id}
                      value={`${payment.order_id} ${payment.description} ${payment.id}`}
                      onSelect={go(`/payments/${payment.id}`)}
                    >
                      <span className="num w-20 shrink-0 truncate text-muted-foreground">{paymentLabel(payment)}</span>
                      <span className="min-w-0 flex-1 truncate">{payment.description}</span>
                      <Amount value={payment.amount} className="text-muted-foreground" />
                      <PaymentStatusBadge status={payment.status} finalising={payment.awaiting_finality !== null} size="sm" className="w-24 justify-start" />
                    </CommandItem>
                  ))}
                </CommandGroup>
              </>
            )}
          </CommandList>
        </Command>
      </CommandDialog>
    </CommandMenuContext.Provider>
  )
}

export function useCommandMenu() {
  const context = React.useContext(CommandMenuContext)
  if (!context) throw new Error("useCommandMenu must be used within CommandMenuProvider")
  return context
}

/** The ⌘K affordance: a quiet search field on desktop, an icon button on mobile. */
export function CommandMenuTrigger({ className }: { className?: string }) {
  const { open } = useCommandMenu()
  return (
    <>
      <Button
        type="button"
        variant="outline"
        onClick={open}
        className={cn(
          "hidden h-8 w-56 justify-start gap-2 px-2.5 font-normal text-muted-foreground shadow-none md:inline-flex lg:w-64",
          className,
        )}
      >
        <SearchIcon className="size-3.5" />
        <span className="flex-1 text-left text-[13px]">Search…</span>
        <Kbd className="font-mono text-[11px]">⌘K</Kbd>
      </Button>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        onClick={open}
        aria-label="Search"
        className="text-muted-foreground hover:text-foreground md:hidden"
      >
        <SearchIcon />
      </Button>
    </>
  )
}
