"use client"

import { ArrowUpRightIcon } from "lucide-react"

import { explorerAddressUrl, explorerTxUrl, explorerUrl, truncateMiddle } from "@/lib/format"
import { cn } from "@/lib/utils"

import { CopyButton } from "./copy-button"

type HashProps = Omit<React.ComponentProps<"span">, "children"> & {
  value: string
  /** `true` (default) truncates the middle; `false` shows everything; `[start, end]` sets the kept lengths. */
  truncate?: boolean | [number, number]
  /** Show the copy button. */
  copy?: boolean
  /** Show the block-explorer link. */
  link?: boolean
  /** Chain the value lives on (56 mainnet, 97 testnet): picks the explorer. Defaults to mainnet. */
  chainId?: number
}

function Hash({
  value,
  truncate = true,
  copy = true,
  link = true,
  chainId: _chainId,
  href,
  noun,
  defaults,
  className,
  ...props
}: HashProps & { href: string | null; noun: string; defaults: [number, number] }) {
  void _chainId
  const [start, end] = Array.isArray(truncate) ? truncate : defaults
  const text = truncate === false ? value : truncateMiddle(value, start, end)
  return (
    <span data-slot="hash" className={cn("inline-flex max-w-full items-center gap-0.5 align-middle", className)} {...props}>
      <span className={cn("num min-w-0", truncate === false ? "break-all" : "whitespace-nowrap")} title={value}>
        {text}
      </span>
      {copy && <CopyButton value={value} label={`Copy ${noun}`} size="icon-xs" className="ml-0.5 pointer-coarse:size-10" />}
      {link && href && (
        <a
          href={href}
          target="_blank"
          rel="noopener noreferrer"
          aria-label={`View ${noun} on BscScan`}
          className="focus-ring inline-flex size-6 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground pointer-coarse:size-10"
        >
          <ArrowUpRightIcon className="size-3.5" />
        </a>
      )}
    </span>
  )
}

/** A wallet / contract address: mono, middle-truncated (0x7A3f…1b92), copy + explorer link. */
export function Address(props: HashProps) {
  const chain = props.chainId ?? 56
  return <Hash {...props} href={explorerUrl(chain) && explorerAddressUrl(props.value, chain)} noun="address" defaults={[6, 4]} />
}

/** A transaction hash: mono, middle-truncated (0x9f2c4e81…f0a9b8), copy + explorer link. */
export function TxHash(props: HashProps) {
  const chain = props.chainId ?? 56
  return <Hash {...props} href={explorerUrl(chain) && explorerTxUrl(props.value, chain)} noun="transaction hash" defaults={[10, 6]} />
}

/**
 * A full address set for *reading and checking*: "0x" then ten groups of four on two rows,
 * with the first and last group at full strength (what people actually compare).
 * Copying a selection still yields the raw, ungrouped address.
 */
export function AddressGroups({ value, className, ...props }: Omit<React.ComponentProps<"span">, "children"> & { value: string }) {
  const hex = value.startsWith("0x") ? value.slice(2) : value
  const groups = hex.match(/.{1,4}/g) ?? []
  return (
    <span
      data-slot="address-groups"
      className={cn("num grid w-fit grid-cols-[auto_repeat(5,auto)] gap-x-[0.65ch] gap-y-1 text-[15px] leading-6", className)}
      onCopy={(event) => {
        event.clipboardData.setData("text/plain", value)
        event.preventDefault()
      }}
      {...props}
    >
      <span className="sr-only">{value}</span>
      <span aria-hidden="true" className="text-faint">
        0x
      </span>
      {groups.map((group, i) => (
        <span
          key={i}
          aria-hidden="true"
          className={cn(
            i === 5 && "col-start-2",
            i === 0 || i === groups.length - 1 ? "font-medium text-foreground" : "text-muted-foreground",
          )}
        >
          {group}
        </span>
      ))}
    </span>
  )
}
