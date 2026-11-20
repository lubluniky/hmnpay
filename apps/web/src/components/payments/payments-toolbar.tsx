"use client"

import * as React from "react"
import { useRouter } from "next/navigation"
import { format } from "date-fns"
import { CalendarIcon, CheckIcon, ChevronDownIcon, DownloadIcon, SearchIcon, XIcon } from "lucide-react"
import { toast } from "sonner"

import { Button } from "@/components/ui/button"
import { Calendar } from "@/components/ui/calendar"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import type { PaymentListQuery } from "@/lib/api/client"
import { describeError } from "@/lib/errors"
import { cn } from "@/lib/utils"

import { DATE_RANGES, EXPORT_LIMIT, fetchAllPayments, formatDay, paymentsHref, paymentsToCsv, type PaymentQuery } from "./lib"

/* -------------------------------------------------------------------------- */
/* Search                                                                     */
/* -------------------------------------------------------------------------- */

function SearchField({ query }: { query: PaymentQuery }) {
  const router = useRouter()
  const [value, setValue] = React.useState(query.q)
  // What the URL last showed and what this field last sent, to tell an outside reset
  // ("Clear filters", Back) from the echo of the user's own typing.
  const [sync, setSync] = React.useState({ url: query.q, sent: query.q })
  if (query.q !== sync.url) {
    setSync({ url: query.q, sent: query.q })
    if (query.q === "" && sync.sent !== "") setValue("")
  }
  const [, startTransition] = React.useTransition()
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null)
  const inputRef = React.useRef<HTMLInputElement>(null)

  React.useEffect(() => () => void (timer.current && clearTimeout(timer.current)), [])

  const commit = (q: string, delay: number) => {
    if (timer.current) clearTimeout(timer.current)
    timer.current = setTimeout(() => {
      setSync((prev) => ({ ...prev, sent: q }))
      startTransition(() => router.replace(paymentsHref({ ...query, q }), { scroll: false }))
    }, delay)
  }

  return (
    <div
      className={cn(
        "flex h-8 min-w-0 flex-1 items-center gap-2 rounded-lg border border-input pr-1 pl-2.5 transition-colors max-sm:basis-full sm:max-w-[340px] dark:bg-input/30",
        "focus-within:border-ring focus-within:ring-3 focus-within:ring-ring/50 pointer-coarse:h-10",
      )}
    >
      <SearchIcon className="size-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />
      <input
        ref={inputRef}
        type="search"
        value={value}
        onChange={(event) => {
          setValue(event.target.value)
          commit(event.target.value, 220)
        }}
        onKeyDown={(event) => {
          if (event.key === "Enter") commit(value, 0)
        }}
        placeholder="Order ID, address, tx hash or email"
        aria-label="Search payments"
        autoComplete="off"
        spellCheck={false}
        className="h-full min-w-0 flex-1 bg-transparent text-base outline-none placeholder:text-muted-foreground md:text-[13px] [&::-webkit-search-cancel-button]:hidden"
      />
      {value && (
        <Button
          type="button"
          variant="ghost"
          size="icon-xs"
          aria-label="Clear search"
          className="text-muted-foreground"
          onClick={() => {
            setValue("")
            commit("", 0)
            inputRef.current?.focus()
          }}
        >
          <XIcon />
        </Button>
      )}
    </div>
  )
}

/* -------------------------------------------------------------------------- */
/* Date range                                                                 */
/* -------------------------------------------------------------------------- */

const parseDay = (day: string) => {
  const [y, m, d] = day.split("-").map(Number)
  return new Date(y, m - 1, d)
}

function DateRangeFilter({ query }: { query: PaymentQuery }) {
  const router = useRouter()
  const [open, setOpen] = React.useState(false)
  const [custom, setCustom] = React.useState(Boolean(query.from && query.to))
  const [draft, setDraft] = React.useState<{ from?: Date; to?: Date } | undefined>(
    query.from && query.to ? { from: parseDay(query.from), to: parseDay(query.to) } : undefined,
  )
  // The calendar's "today": evaluated when the filter mounts, in the browser.
  const [today] = React.useState(() => new Date())
  const isCustom = Boolean(query.from && query.to)
  const preset = DATE_RANGES.find((r) => r.id === query.range) ?? DATE_RANGES[0]
  const filtered = isCustom || preset.id !== "all"
  const label = isCustom
    ? `${formatDay(`${query.from}T00:00:00Z`)} – ${formatDay(`${query.to}T00:00:00Z`)}`
    : preset.label

  const go = (next: Partial<PaymentQuery>) => {
    setOpen(false)
    router.replace(paymentsHref({ ...query, from: undefined, to: undefined, ...next }), { scroll: false })
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="outline" className={cn("shrink-0 gap-2 font-normal pointer-coarse:h-10", !filtered && "text-muted-foreground")}>
          <CalendarIcon className="size-3.5 text-muted-foreground" />
          <span className={cn(isCustom && "num")}>{label}</span>
          <ChevronDownIcon className="size-3.5 text-muted-foreground" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-auto min-w-52 gap-0 p-1 shadow-overlay">
        <div role="listbox" aria-label="Date range" className="flex flex-col">
          {DATE_RANGES.map((range) => {
            const selected = !isCustom && range.id === preset.id
            return (
              <button
                key={range.id}
                type="button"
                role="option"
                aria-selected={selected}
                onClick={() => go({ range: range.id })}
                className="flex h-8 items-center justify-between gap-6 rounded-md px-2 text-left text-[13px] outline-none transition-colors hover:bg-muted focus-visible:bg-muted pointer-coarse:h-10"
              >
                {range.label}
                {selected && <CheckIcon className="size-4" strokeWidth={2.5} />}
              </button>
            )
          })}
        </div>
        <div className="mt-1 border-t border-border pt-1">
          {custom ? (
            <Calendar
              mode="range"
              selected={draft?.from ? { from: draft.from, to: draft.to } : undefined}
              onSelect={(range) => {
                setDraft(range ?? undefined)
                if (range?.from && range.to && range.from.getTime() !== range.to.getTime()) {
                  go({ range: "all", from: format(range.from, "yyyy-MM-dd"), to: format(range.to, "yyyy-MM-dd") })
                }
              }}
              today={today}
              defaultMonth={draft?.from ?? today}
              disabled={{ after: today }}
              className="p-1.5"
            />
          ) : (
            <button
              type="button"
              onClick={() => setCustom(true)}
              className="flex h-8 w-full items-center justify-between gap-6 rounded-md px-2 text-left text-[13px] outline-none transition-colors hover:bg-muted focus-visible:bg-muted pointer-coarse:h-10"
            >
              Custom range…
              {isCustom && <CheckIcon className="size-4" strokeWidth={2.5} />}
            </button>
          )}
        </div>
      </PopoverContent>
    </Popover>
  )
}

/* -------------------------------------------------------------------------- */
/* Toolbar                                                                    */
/* -------------------------------------------------------------------------- */

/** Search + date range for the payments list. Both write to the URL, so every view is linkable. */
export function PaymentsToolbar({ query, className }: { query: PaymentQuery; className?: string }) {
  return (
    <div className={cn("flex items-center gap-2", className)}>
      <SearchField query={query} />
      <DateRangeFilter key={`${query.range}-${query.from}-${query.to}`} query={query} />
    </div>
  )
}

/* -------------------------------------------------------------------------- */
/* Export                                                                     */
/* -------------------------------------------------------------------------- */

function download(filename: string, text: string, type: string) {
  const url = URL.createObjectURL(new Blob([text], { type }))
  const link = document.createElement("a")
  link.href = url
  link.download = filename
  link.click()
  URL.revokeObjectURL(url)
}

/**
 * Exports exactly what the current filters match (not just the visible page): every page is
 * fetched from the API when a format is chosen. Amounts are written as the exact strings.
 */
export function ExportMenu({ query, total, filtered }: { query: PaymentListQuery; total: number; filtered: boolean }) {
  const [busy, setBusy] = React.useState(false)

  const run = async (kind: "CSV" | "JSON") => {
    setBusy(true)
    try {
      const { rows, truncated } = await fetchAllPayments(query)
      const stamp = new Date().toISOString().slice(0, 10)
      if (kind === "CSV") download(`hmnpay-payments-${stamp}.csv`, paymentsToCsv(rows), "text/csv")
      else download(`hmnpay-payments-${stamp}.json`, JSON.stringify(rows, null, 2), "application/json")
      const count = `${rows.length} ${rows.length === 1 ? "payment" : "payments"}`
      if (truncated) {
        toast.warning(`Exported the first ${count} as ${kind}`, {
          description: `An export holds at most ${EXPORT_LIMIT.toLocaleString("en-US")} payments. Narrow the dates to get the rest.`,
        })
      } else toast.success(`Exported ${count} as ${kind}`)
    } catch (error) {
      toast.error("Export failed", { description: describeError(error) })
    } finally {
      setBusy(false)
    }
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="outline" disabled={total === 0 || busy}>
          <DownloadIcon data-icon="inline-start" />
          {busy ? "Exporting…" : "Export"}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-52">
        <DropdownMenuLabel className="text-xs font-normal text-muted-foreground">
          <span className="num">{total}</span> {total === 1 ? "payment" : "payments"}
          {filtered ? " matching your filters" : ", all time"}
        </DropdownMenuLabel>
        <DropdownMenuItem onSelect={() => void run("CSV")}>Download CSV</DropdownMenuItem>
        <DropdownMenuItem onSelect={() => void run("JSON")}>Download JSON</DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
