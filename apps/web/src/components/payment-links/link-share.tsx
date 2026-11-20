"use client"

import * as React from "react"
import { ArrowUpRightIcon, DownloadIcon } from "lucide-react"
import { QRCodeSVG } from "qrcode.react"

import { AssetIcon, CopyButton } from "@/components/hmn"
import { Button } from "@/components/ui/button"
import { safeSiteUrl } from "@/lib/url"
import { cn } from "@/lib/utils"

import { linkParts } from "./data"

/* ----------------------------------- QR ----------------------------------- */

/**
 * QR of a payment link. Same tile as the checkout's `QrTile` (white in both themes, asset
 * mark in the centre) but it encodes a URL, has its own accessible name, and can be saved.
 */
export function LinkQr({
  url,
  size = 132,
  className,
  ref,
}: {
  url: string
  size?: number
  className?: string
  ref?: React.Ref<HTMLDivElement>
}) {
  return (
    <div
      ref={ref}
      className={cn("relative shrink-0 rounded-xl bg-white p-3 ring-1 ring-black/[0.07] dark:ring-0", className)}
      style={{ width: size + 24, height: size + 24 }}
    >
      <QRCodeSVG
        value={url}
        size={size}
        level="Q"
        marginSize={0}
        bgColor="transparent"
        fgColor="#0a0a0a"
        role="img"
        aria-label="QR code of the payment link"
        className="block size-full"
      />
      <span className="absolute top-1/2 left-1/2 flex -translate-x-1/2 -translate-y-1/2 rounded-full bg-white p-1">
        <AssetIcon size={Math.round(size * 0.2)} />
      </span>
    </div>
  )
}

/** Save the QR inside `container` as a standalone SVG with a white quiet zone. */
export function downloadQr(container: HTMLElement | null, filename: string) {
  const svg = container?.querySelector("svg")
  if (!svg) return
  const viewBox = svg.getAttribute("viewBox")?.split(" ").map(Number) ?? [0, 0, 29, 29]
  const modules = viewBox[2]
  const quiet = 4
  const total = modules + quiet * 2
  const source = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${total} ${total}" width="1024" height="1024" shape-rendering="crispEdges"><rect width="${total}" height="${total}" fill="#fff"/><g transform="translate(${quiet} ${quiet})" fill="#0a0a0a">${svg.innerHTML}</g></svg>`
  const href = URL.createObjectURL(new Blob([source], { type: "image/svg+xml" }))
  const anchor = document.createElement("a")
  anchor.href = href
  anchor.download = filename
  anchor.click()
  URL.revokeObjectURL(href)
}

/* ---------------------------------- share --------------------------------- */

/** The link written out: host quiet, the ending at full strength. */
export function LinkText({ url, className }: { url: string; className?: string }) {
  const { base, slug } = linkParts(url)
  return (
    <span className={cn("num", className)} title={url}>
      <span className="text-muted-foreground">{base}</span>
      <wbr />
      <span className="text-foreground">{slug}</span>
    </span>
  )
}

/**
 * Everything needed to hand a link to a customer: QR, the URL, copy, open, save the QR.
 * Used in the "link created" state and at the top of the link's detail sheet.
 */
export function LinkShare({
  url,
  slug,
  qrSize = 132,
  focusCopy = false,
  className,
}: {
  url: string
  slug: string
  qrSize?: number
  /** Make "Copy link" the first thing focused (the "link created" state). */
  focusCopy?: boolean
  className?: string
}) {
  const qr = React.useRef<HTMLDivElement>(null)
  return (
    <div className={className}>
      <div className="flex gap-4">
        <LinkQr ref={qr} url={url} size={qrSize} />
        <div className="flex min-w-0 flex-1 flex-col">
          <div className="text-xs leading-4 text-muted-foreground">Link</div>
          <LinkText url={url} className="mt-1 block text-[13px] leading-5 wrap-anywhere" />
          <button
            type="button"
            onClick={() => downloadQr(qr.current, `hmnpay-${slug}-qr.svg`)}
            className="focus-ring mt-auto inline-flex w-fit items-center gap-1.5 rounded-sm pt-3 text-[13px] leading-5 text-muted-foreground transition-colors hover:text-foreground"
          >
            <DownloadIcon aria-hidden="true" className="size-3.5" />
            Download QR
          </button>
        </div>
      </div>
      <div className="mt-4 grid grid-cols-2 gap-2">
        <CopyButton
          value={url}
          variant="default"
          size="default"
          autoFocus={focusCopy}
          data-autofocus={focusCopy || undefined}
          className="pointer-coarse:h-10 [&_.lucide-check]:text-primary-foreground"
        >
          Copy link
        </CopyButton>
        <Button asChild variant="outline" className="pointer-coarse:h-10">
          <a href={safeSiteUrl(url)} target="_blank" rel="noopener noreferrer">
            Open checkout
            <ArrowUpRightIcon data-icon="inline-end" />
          </a>
        </Button>
      </div>
    </div>
  )
}
