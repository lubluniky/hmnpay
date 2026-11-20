import { displayParts } from "@/lib/money"
import { cn } from "@/lib/utils"

import { Roll } from "./motion"

/**
 * Split a decimal string into grouped integer + fraction without going through floats. Keeps at
 * least `minDecimals` and every digit the value has: an amount is shown exactly as the API sent
 * it, never rounded or cut. (`maxDecimals` exists for the two places that deliberately show a
 * whole number of units: a phone-width headline and chart axes. It truncates, it does not round.)
 */
export function amountParts(value: string, minDecimals = 2, maxDecimals?: number) {
  const { negative, whole, fraction: full } = displayParts(value, 0)
  let fraction = maxDecimals === undefined ? full : full.slice(0, maxDecimals)
  if (maxDecimals !== undefined) fraction = fraction.replace(/0+$/, "")
  if (fraction.length < minDecimals) fraction = fraction.padEnd(minDecimals, "0")
  return { negative, integer: whole, fraction }
}

/** Plain-text version of what `<Amount>` renders, for clipboard / aria: "1,234.50". */
export function amountText(value: string, minDecimals = 2, maxDecimals?: number) {
  const { negative, integer, fraction } = amountParts(value, minDecimals, maxDecimals)
  return `${negative ? "-" : ""}${integer}${fraction ? `.${fraction}` : ""}`
}

/**
 * A money amount: Geist Mono, tabular digits, never wraps.
 * - `asset` appends the symbol in a quieter weight ("USDT" by default when `true`).
 * - `dimFraction` softens the decimals — for dense tables only. Never on checkout,
 *   where the exact decimals are the point.
 * - `sign="always"` prefixes "+" on positives (deltas, overpayments).
 * - `roll` makes the digits roll when the value changes (live totals); `roll="intro"` also rolls
 *   them in once on load (headline figures). Never on checkout amounts a payer has to read.
 * Size and weight come from `className` (e.g. `text-2xl font-medium`).
 */
export function Amount({
  value,
  asset = false,
  decimals = 2,
  maxDecimals,
  dimFraction = false,
  sign = "auto",
  roll = false,
  className,
  ...props
}: Omit<React.ComponentProps<"span">, "children"> & {
  /** A decimal string, as the API sends amounts. Never a number: floats cannot hold 18 decimals. */
  value: string
  asset?: boolean | string
  decimals?: number
  maxDecimals?: number
  dimFraction?: boolean
  sign?: "auto" | "always"
  roll?: boolean | "intro"
}) {
  const { negative, integer, fraction } = amountParts(value, decimals, maxDecimals)
  const symbol = asset === true ? "USDT" : asset || null
  const prefix = negative ? "−" : sign === "always" ? "+" : ""
  return (
    <span data-slot="amount" className={cn("num whitespace-nowrap", className)} {...props}>
      {prefix}
      {roll ? (
        <Roll value={fraction ? `${integer}.${fraction}` : integer} intro={roll === "intro"} dimFraction={dimFraction} />
      ) : (
        <>
          {/* Geist Mono's full-width "," and "." read as gaps inside a number: pull them in. */}
          {integer.split(",").map((group, i) => (
            <span key={i}>
              {i > 0 && <span className="-mx-[0.11em]">,</span>}
              {group}
            </span>
          ))}
          {fraction && (
            <span className={cn(dimFraction && "text-muted-foreground")}>
              <span className="-mx-[0.11em]">.</span>
              {fraction}
            </span>
          )}
        </>
      )}
      {symbol && (
        <span data-slot="amount-symbol" className="ml-[0.35em] font-normal text-muted-foreground">
          {symbol}
        </span>
      )}
    </span>
  )
}
