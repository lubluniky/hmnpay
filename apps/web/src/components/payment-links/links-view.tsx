"use client"

import * as React from "react"
import {
  ArrowUpRightIcon,
  CircleSlashIcon,
  CopyIcon,
  EllipsisIcon,
  Link2Icon,
  PencilIcon,
  PlayIcon,
  PlusIcon,
  QrCodeIcon,
  SearchIcon,
  Trash2Icon,
} from "lucide-react"
import { toast } from "sonner"

import {
  Amount,
  CopyButton,
  EmptyState,
  ErrorState,
  PageContainer,
  PageHeader,
  Panel,
  StatusBadge,
  useCopy,
} from "@/components/hmn"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { formatDate } from "@/lib/format"
import { useDashboard } from "@/components/shell/session"
import { Skeleton } from "@/components/ui/skeleton"
import { api } from "@/lib/api/client"
import type { PaymentLink } from "@/lib/api/types"
import { useQuery } from "@/lib/api/use-query"
import { describeError, fieldErrors } from "@/lib/errors"
import { initialParam } from "@/components/settings/shared"
import { safeSiteUrl } from "@/lib/url"
import { useNow } from "@/lib/use-now"
import { cn } from "@/lib/utils"

import { CreateLinkDialog } from "./create-link-dialog"
import { draftFromLink, draftToInput, EMPTY_DRAFT, LINK_FIELD_NAMES, LINK_STATUS, sumCollected, type DraftErrors, type LinkDraft } from "./data"
import { downloadQr, LinkQr, LinkText } from "./link-share"
import { LinkSheet } from "./link-sheet"

type Filter = "all" | "active" | "inactive"

/** Keep the URL shareable without a server round-trip (Next picks up replaceState). */
function syncUrl(params: Record<string, string | null>) {
  const url = new URL(window.location.href)
  for (const [key, value] of Object.entries(params)) {
    if (value === null) url.searchParams.delete(key)
    else url.searchParams.set(key, value)
  }
  window.history.replaceState(null, "", url)
}

/**
 * Payment links: reusable hosted checkout links. `?new=1` opens the create dialog and
 * `?link=<id>` a link's detail sheet, so both are linkable.
 */
export function LinksView() {
  const { settings } = useDashboard()
  const decimals = settings.asset.decimals
  const now = useNow()
  const list = useQuery("payment-links", (signal) => api.links(signal), { refreshMs: 15_000 })
  // Newest first.
  const links = React.useMemo(() => [...(list.data?.data ?? [])].sort((a, b) => b.created_at.localeCompare(a.created_at)), [list.data])
  const setLinks = (update: (current: PaymentLink[]) => PaymentLink[]) => list.mutate((current) => (current ? { ...current, data: update(current.data) } : current))
  const base = `${(settings.public_url || (typeof window === "undefined" ? "" : window.location.origin)).replace(/\/$/, "")}/l`

  const [query, setQuery] = React.useState("")
  const [filter, setFilter] = React.useState<Filter>("all")

  const [dialogOpen, setDialogOpen] = React.useState(() => initialParam("new") === "1")
  const [draft, setDraftState] = React.useState<LinkDraft>(EMPTY_DRAFT)
  const [created, setCreated] = React.useState<PaymentLink | null>(null)
  // The link the dialog is editing; null while it creates one.
  const [editing, setEditing] = React.useState<PaymentLink | null>(null)
  const [serverErrors, setServerErrors] = React.useState<DraftErrors>({})
  const [failure, setFailure] = React.useState<string | null>(null)
  const [busy, setBusy] = React.useState(false)
  const setDraft = (next: LinkDraft) => {
    setDraftState(next)
    setServerErrors({})
    setFailure(null)
  }

  const [sheetId, setSheetId] = React.useState<string | null>(() => initialParam("link"))
  const [qrLink, setQrLink] = React.useState<PaymentLink | null>(null)
  const [deleting, setDeleting] = React.useState<PaymentLink | null>(null)

  const sheetLink = links.find((link) => link.id === sheetId) ?? null
  // Keep showing the last link while the sheet animates out.
  const [lastSheetLink, setLastSheetLink] = React.useState(sheetLink)
  if (sheetLink && sheetLink !== lastSheetLink) setLastSheetLink(sheetLink)
  const sheetPayments = useQuery(sheetLink ? `payment-links/${sheetLink.id}/payments` : null, (signal) => api.payments({ link_id: sheetLink!.id, limit: 5 }, signal), {
    refreshMs: 10_000,
  })

  const counts = React.useMemo(() => {
    const active = links.filter((link) => link.status === "active").length
    return { all: links.length, active, inactive: links.length - active }
  }, [links])

  const visible = React.useMemo(() => {
    const needle = query.trim().toLowerCase()
    return links.filter((link) => {
      const active = link.status === "active"
      if (filter === "active" && !active) return false
      if (filter === "inactive" && active) return false
      return !needle || link.name.toLowerCase().includes(needle) || link.slug.includes(needle)
    })
  }, [links, query, filter])

  /* -------------------------------- actions -------------------------------- */

  const openCreate = () => {
    setCreated(null)
    setEditing(null)
    setDraft(EMPTY_DRAFT)
    setDialogOpen(true)
    syncUrl({ new: "1" })
  }
  const closeCreate = (open: boolean) => {
    setDialogOpen(open)
    if (!open) syncUrl({ new: null })
  }
  const openEdit = (link: PaymentLink) => {
    setCreated(null)
    setEditing(link)
    setDraft(draftFromLink(link))
    setDialogOpen(true)
  }
  const create = async () => {
    setBusy(true)
    try {
      if (editing) {
        const link = await api.updateLink(editing.id, draftToInput(draft, decimals, editing))
        setLinks((current) => current.map((item) => (item.id === link.id ? link : item)))
        setDialogOpen(false)
        toast("Link saved", { description: "New checkouts of this link use the changes." })
        return
      }
      const link = await api.createLink(draftToInput(draft, decimals))
      setLinks((current) => [link, ...current])
      setCreated(link)
    } catch (error) {
      const fields = fieldErrors(error, LINK_FIELD_NAMES) as DraftErrors
      // "taken" on the slug is the common one: say it where the slug is.
      setServerErrors(fields)
      setFailure(Object.keys(fields).length > 0 ? null : describeError(error))
    } finally {
      setBusy(false)
    }
  }
  const openSheet = (link: PaymentLink) => {
    setSheetId(link.id)
    syncUrl({ link: link.id })
  }
  const closeSheet = () => {
    setSheetId(null)
    syncUrl({ link: null })
  }
  const toggle = async (link: PaymentLink) => {
    try {
      const next = await api.setLinkActive(link.id, !link.active)
      setLinks((current) => current.map((item) => (item.id === next.id ? next : item)))
      toast(next.active ? "Link enabled" : "Link disabled", {
        description: next.active ? "It takes payments again." : "Customers who open it see that it is no longer available.",
      })
    } catch (error) {
      toast.error("Couldn’t change the link", { description: describeError(error) })
      list.reload()
    }
  }
  const remove = async (link: PaymentLink) => {
    try {
      await api.deleteLink(link.id)
      setLinks((current) => current.filter((item) => item.id !== link.id))
      if (sheetId === link.id) closeSheet()
      toast("Link deleted")
    } catch (error) {
      toast.error("Couldn’t delete the link", { description: describeError(error) })
      list.reload()
    } finally {
      setDeleting(null)
    }
  }

  const empty = links.length === 0

  return (
    <PageContainer>
      <PageHeader
        title="Payment links"
        description="Reusable checkout links you can share without writing code."
        actions={
          <Button onClick={openCreate}>
            <PlusIcon data-icon="inline-start" />
            Create link
          </Button>
        }
      />

      {!list.data ? (
        list.error ? (
          <ErrorState error={list.error} title="Couldn’t load payment links" onRetry={list.reload} className="py-20" />
        ) : (
          <Panel role="status" aria-label="Loading payment links" className="space-y-4 p-5">
            {[0, 1, 2, 3].map((i) => (
              <Skeleton key={i} className="h-5 w-full" />
            ))}
          </Panel>
        )
      ) : empty ? (
        <EmptyState
          className="py-20"
          icon={<Link2Icon />}
          title="No payment links yet"
          description={`Create a link once, share it anywhere, and get paid in ${settings.asset.symbol} straight to your wallet, with no code to write.`}
          action={
            <Button onClick={openCreate}>
              <PlusIcon data-icon="inline-start" />
              Create link
            </Button>
          }
        />
      ) : (
        <div className="space-y-4">
          <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
            <InputGroup className="sm:w-64">
              <InputGroupAddon>
                <SearchIcon />
              </InputGroupAddon>
              <InputGroupInput
                type="search"
                aria-label="Search links"
                placeholder="Search links"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
            </InputGroup>
            <Tabs value={filter} onValueChange={(value) => setFilter(value as Filter)}>
              <TabsList className="max-sm:w-full">
                {(["all", "active", "inactive"] as const).map((key) => (
                  <TabsTrigger key={key} value={key} className="gap-1.5 px-2.5 capitalize">
                    {key}
                    <span className="num text-xs text-muted-foreground">{counts[key]}</span>
                  </TabsTrigger>
                ))}
              </TabsList>
            </Tabs>
          </div>

          <Panel>
            {visible.length === 0 ? (
              <EmptyState
                bordered={false}
                icon={<SearchIcon />}
                title="No links match"
                description={query ? `Nothing named like “${query.trim()}” in this view.` : "No links in this view."}
                action={
                  <Button
                    variant="outline"
                    onClick={() => {
                      setQuery("")
                      setFilter("all")
                    }}
                  >
                    Show all links
                  </Button>
                }
              />
            ) : (
              <>
                <LinksTable
                  links={visible}
                  onOpen={openSheet}
                  onQr={setQrLink}
                  onToggle={toggle}
                  onEdit={openEdit}
                  onDelete={setDeleting}
                />
                <LinksList links={visible} onOpen={openSheet} />
                <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-1 border-t border-border bg-muted/40 px-4 py-2.5 text-[13px] leading-5 text-muted-foreground">
                  <span>
                    <span className="num text-foreground">{visible.length}</span> {visible.length === 1 ? "link" : "links"}
                  </span>
                  <span>
                    <span className="num text-foreground">
                      {visible.reduce((total, link) => total + link.uses, 0).toLocaleString("en-US")}
                    </span>{" "}
                    payments, <Amount value={sumCollected(visible, decimals)} asset={settings.asset.symbol} className="text-foreground" /> collected
                  </span>
                </div>
              </>
            )}
          </Panel>
        </div>
      )}

      <CreateLinkDialog
        open={dialogOpen}
        onOpenChange={closeCreate}
        draft={draft}
        onDraft={setDraft}
        created={created}
        editing={editing}
        onCreate={create}
        onAnother={openCreate}
        base={base}
        today={new Date(now).toISOString().slice(0, 10)}
        decimals={decimals}
        asset={settings.asset.symbol}
        serverErrors={serverErrors}
        failure={failure}
        busy={busy}
      />

      <LinkSheet
        link={sheetLink}
        fallback={lastSheetLink}
        payments={sheetPayments.data?.data}
        now={now}
        onOpenChange={(open) => !open && closeSheet()}
        onToggle={toggle}
        onEdit={openEdit}
        onDelete={setDeleting}
      />

      <QrDialog link={qrLink} onOpenChange={(open) => !open && setQrLink(null)} />

      <AlertDialog open={deleting !== null} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this link?</AlertDialogTitle>
            <AlertDialogDescription>
              <span className="font-medium text-foreground">{deleting?.name}</span> stops working right away, including
              printed QR codes. Its{" "}
              <span className="num text-foreground">{deleting?.uses.toLocaleString("en-US")}</span> payments stay in
              Payments.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep link</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              onClick={(event) => {
                event.preventDefault()
                if (deleting) void remove(deleting)
              }}
            >
              Delete link
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </PageContainer>
  )
}

/* ---------------------------------- table --------------------------------- */

const head = "h-10 px-3 text-[13px] font-normal text-muted-foreground"
const cell = "h-11 px-3 py-0"

function StatusCell({ link, size }: { link: PaymentLink; size?: "sm" | "md" }) {
  const meta = LINK_STATUS[link.status]
  return (
    <StatusBadge tone={meta.tone} hollow={meta.hollow} size={size}>
      {meta.label}
    </StatusBadge>
  )
}

function Uses({ link }: { link: PaymentLink }) {
  return (
    <span className="num">
      {link.uses.toLocaleString("en-US")}
      {link.max_uses !== null && (
        <span className="text-muted-foreground">
          <span className="mx-[0.2em] text-faint">/</span>
          {link.max_uses}
        </span>
      )}
    </span>
  )
}

function LinksTable({
  links,
  onOpen,
  onQr,
  onToggle,
  onEdit,
  onDelete,
}: {
  links: PaymentLink[]
  onOpen: (link: PaymentLink) => void
  onQr: (link: PaymentLink) => void
  onToggle: (link: PaymentLink) => void
  onEdit: (link: PaymentLink) => void
  onDelete: (link: PaymentLink) => void
}) {
  return (
    <Table className="max-md:hidden">
      <TableHeader>
        <TableRow className="hover:bg-transparent">
          <TableHead className={cn(head, "pl-4")}>Name</TableHead>
          <TableHead className={head}>Link</TableHead>
          <TableHead className={cn(head, "text-right")}>Amount</TableHead>
          <TableHead className={cn(head, "text-right")}>Payments</TableHead>
          <TableHead className={cn(head, "text-right")}>Collected</TableHead>
          <TableHead className={cn(head, "pl-6")}>Status</TableHead>
          <TableHead className={cn(head, "max-lg:hidden")}>Created</TableHead>
          <TableHead className={cn(head, "w-11 pr-2")}>
            <span className="sr-only">Actions</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {links.map((link) => {
          const status = link.status
          return (
            <TableRow key={link.id} className="cursor-pointer" onClick={() => onOpen(link)}>
              <TableCell className={cn(cell, "max-w-64 pl-4")}>
                <button
                  type="button"
                  onClick={(event) => {
                    event.stopPropagation()
                    onOpen(link)
                  }}
                  className="focus-ring block max-w-full truncate rounded-sm text-left font-medium"
                >
                  {link.name}
                </button>
              </TableCell>
              <TableCell className={cell}>
                <span className="flex items-center gap-0.5" onClick={(event) => event.stopPropagation()}>
                  <LinkText url={link.url} className="text-[13px]" />
                  <CopyButton value={link.url} label={`Copy link to ${link.name}`} size="icon-xs" />
                </span>
              </TableCell>
              <TableCell className={cn(cell, "text-right")}>
                {link.amount ? (
                  <Amount value={link.amount} dimFraction />
                ) : (
                  <span className="text-[13px] text-muted-foreground">Customer chooses</span>
                )}
              </TableCell>
              <TableCell className={cn(cell, "text-right")}>
                <Uses link={link} />
              </TableCell>
              <TableCell className={cn(cell, "text-right")}>
                <Amount value={link.total_collected} dimFraction />
              </TableCell>
              <TableCell className={cn(cell, "pl-6")}>
                <StatusCell link={link} />
              </TableCell>
              <TableCell className={cn(cell, "text-[13px] text-muted-foreground max-lg:hidden")}>
                {formatDate(link.created_at)}
              </TableCell>
              <TableCell className={cn(cell, "pr-2")} onClick={(event) => event.stopPropagation()}>
                <RowMenu link={link} status={status} onQr={onQr} onToggle={onToggle} onEdit={onEdit} onDelete={onDelete} />
              </TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}

function RowMenu({
  link,
  status,
  onQr,
  onToggle,
  onEdit,
  onDelete,
}: {
  link: PaymentLink
  status: PaymentLink["status"]
  onQr: (link: PaymentLink) => void
  onToggle: (link: PaymentLink) => void
  onEdit: (link: PaymentLink) => void
  onDelete: (link: PaymentLink) => void
}) {
  const { copy } = useCopy()
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${link.name}`} className="text-muted-foreground hover:text-foreground">
          <EllipsisIcon />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-44">
        <DropdownMenuItem
          onSelect={async () => {
            if (await copy(link.url)) toast("Link copied")
          }}
        >
          <CopyIcon />
          Copy link
        </DropdownMenuItem>
        <DropdownMenuItem asChild>
          <a href={safeSiteUrl(link.url)} target="_blank" rel="noopener noreferrer">
            <ArrowUpRightIcon />
            Open checkout
          </a>
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onQr(link)}>
          <QrCodeIcon />
          QR code
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => onEdit(link)}>
          <PencilIcon />
          Edit
        </DropdownMenuItem>
        {(status === "active" || status === "disabled") && (
          <DropdownMenuItem onSelect={() => onToggle(link)}>
            {status === "active" ? <CircleSlashIcon /> : <PlayIcon />}
            {status === "active" ? "Disable" : "Enable"}
          </DropdownMenuItem>
        )}
        <DropdownMenuItem variant="destructive" onSelect={() => onDelete(link)}>
          <Trash2Icon />
          Delete
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/* ------------------------------- mobile list ------------------------------ */

/**
 * Under 768px a row becomes two columns: what the link is on the left
 * (name, price, URL), how it is doing on the right (status, collected, payments).
 */
function LinksList({ links, onOpen }: { links: PaymentLink[]; onOpen: (link: PaymentLink) => void }) {
  return (
    <ul className="divide-y divide-border md:hidden">
      {links.map((link) => (
        <li
          key={link.id}
          className="relative grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 gap-y-1 px-4 py-3.5 text-[13px] leading-5"
        >
          <button
            type="button"
            onClick={() => onOpen(link)}
            className="truncate text-left text-sm leading-5 font-medium outline-none after:absolute after:inset-0 focus-visible:after:outline-2 focus-visible:after:-outline-offset-2 focus-visible:after:outline-ring"
          >
            {link.name}
          </button>
          <span className="justify-self-end">
            <StatusCell link={link} size="sm" />
          </span>

          <span className="text-muted-foreground">
            {link.amount ? <Amount value={link.amount} asset className="text-foreground" /> : "Customer chooses"}
          </span>
          <Amount value={link.total_collected} dimFraction className="justify-self-end" />

          <span className="relative z-10 -my-1.5 flex min-w-0 items-center gap-0.5">
            <LinkText url={link.url.replace(/^.*(?=\/l\/)/, "")} className="truncate" />
            <CopyButton value={link.url} label={`Copy link to ${link.name}`} size="icon-xs" className="pointer-coarse:size-8" />
          </span>
          <span className="justify-self-end text-muted-foreground">
            <Uses link={link} /> {link.uses === 1 ? "payment" : "payments"}
          </span>
        </li>
      ))}
    </ul>
  )
}

/* ----------------------------------- QR ----------------------------------- */

function QrDialog({ link, onOpenChange }: { link: PaymentLink | null; onOpenChange: (open: boolean) => void }) {
  const qr = React.useRef<HTMLDivElement>(null)
  const [shown, setShown] = React.useState(link)
  if (link && link !== shown) setShown(link)

  return (
    <Dialog open={link !== null} onOpenChange={onOpenChange}>
      <DialogContent
        className="gap-0 p-0 shadow-overlay sm:max-w-[360px]"
        onOpenAutoFocus={(event) => {
          event.preventDefault()
          ;(event.currentTarget as HTMLElement | null)?.querySelector<HTMLElement>("[data-autofocus]")?.focus()
        }}
      >
        {shown && (
          <>
            <DialogHeader className="gap-1 p-5 pr-12">
              <DialogTitle className="text-base leading-6 font-semibold">{shown.name}</DialogTitle>
              <DialogDescription>
                {shown.amount ? <Amount value={shown.amount} asset className="text-foreground" /> : "Any amount"}. Scan to
                open the checkout.
              </DialogDescription>
            </DialogHeader>
            <div className="flex flex-col items-center gap-3 px-5 pb-5">
              <LinkQr ref={qr} url={shown.url} size={208} />
              <LinkText url={shown.url} className="text-center text-[13px] leading-5 wrap-anywhere" />
            </div>
            <div className="flex gap-2 border-t border-border bg-muted/40 px-5 py-3.5">
              <Button variant="outline" className="flex-1 pointer-coarse:h-10" onClick={() => downloadQr(qr.current, `hmnpay-${shown.slug}-qr.svg`)}>
                Download
              </Button>
              <CopyButton value={shown.url} variant="default" size="default" data-autofocus className="flex-1 pointer-coarse:h-10 [&_.lucide-check]:text-primary-foreground">
                Copy link
              </CopyButton>
            </div>
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}
