import { cn } from "@/lib/utils"

import { AssetIcon, NetworkIcon } from "./asset-icon"

export const NETWORK_NAME = "BNB Smart Chain"
export const NETWORK_STANDARD = "BEP-20"

/**
 * The name of the chain a payment, link or checkout is on: its own chain, not the instance's
 * current one (a payment made before a switch to the testnet stays a BNB Smart Chain payment).
 */
export const networkName = (chainId?: number) => (chainId === 97 ? "BSC Testnet" : NETWORK_NAME)

/**
 * The network, always spelled the same way: mark + "BNB Smart Chain" (or "BSC Testnet", for
 * `chainId` 97) + "BEP-20".
 * `variant="pill"` for standalone use, `"inline"` inside text rows and table cells.
 * `asset` prefixes the token: "USDT · BNB Smart Chain · BEP-20" with the combined asset mark.
 */
export function NetworkBadge({
  variant = "pill",
  size = "md",
  asset = false,
  chainId,
  className,
  ...props
}: React.ComponentProps<"span"> & { variant?: "pill" | "inline"; size?: "sm" | "md"; asset?: boolean; chainId?: number }) {
  const sm = size === "sm"
  return (
    <span
      className={cn(
        "inline-flex items-center font-medium whitespace-nowrap text-foreground",
        sm ? "gap-1.5 text-xs" : "gap-2 text-[13px]",
        variant === "pill" && "rounded-full border border-border bg-card",
        variant === "pill" && (sm ? "h-6 pr-2.5 pl-1" : "h-7 pr-3 pl-1.5"),
        className,
      )}
      {...props}
    >
      {asset ? <AssetIcon size={sm ? 16 : 18} /> : <NetworkIcon size={sm ? 14 : 16} />}
      <span>
        {asset && (
          <>
            USDT
            <span className="mx-1.5 text-faint" aria-hidden="true">
              ·
            </span>
          </>
        )}
        {networkName(chainId)}
        <span className="mx-1.5 text-faint" aria-hidden="true">
          ·
        </span>
        <span className="text-muted-foreground">{NETWORK_STANDARD}</span>
      </span>
    </span>
  )
}
