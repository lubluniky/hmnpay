/**
 * Money arithmetic and formatting on decimal strings, without floating point.
 *
 * The API speaks amounts as decimal strings in whole units of the asset ("149.00", or
 * "0.000000000000000001" — USDT on BNB Smart Chain has 18 decimals). A JavaScript number cannot
 * hold those exactly, so nothing here ever converts an amount to one: values are parsed into
 * BigInt base units, compared and added there, and printed back digit by digit.
 *
 * (The one place an amount becomes a number is the height of a bar in a chart — `toChartNumber` —
 * which is geometry, not money. The figure next to the bar is always the exact string.)
 */

/** Decimals of the amounts the API returns when a view does not say (USDT BEP-20). */
export const DEFAULT_DECIMALS = 18

const DECIMAL = /^(\d+)(?:\.(\d+))?$/

/** Whether `text` is a plain non-negative decimal ("12", "12.5", "0.50"). No sign, exponent, spaces or separators. */
export function isDecimal(text: string): boolean {
  return DECIMAL.test(text)
}

/**
 * Parses a decimal string into base units. Returns null for anything that is not a plain
 * non-negative decimal or that has more fractional digits than the asset has decimals
 * (nothing is ever rounded silently).
 */
export function parseUnits(text: string, decimals = DEFAULT_DECIMALS): bigint | null {
  const match = DECIMAL.exec(text)
  if (!match) return null
  const [, whole, fraction = ""] = match
  if (fraction.length > decimals && /[1-9]/.test(fraction.slice(decimals))) return null
  return BigInt(whole + fraction.slice(0, decimals).padEnd(decimals, "0"))
}

/** Base units of an amount the API sent. Throws on a malformed one: that is a bug, never user input. */
export function toUnits(amount: string, decimals = DEFAULT_DECIMALS): bigint {
  const units = parseUnits(amount, decimals)
  if (units === null) throw new Error(`not an amount: ${JSON.stringify(amount)}`)
  return units
}

/**
 * Prints base units the way the API does (core.Amount.Format): trailing fractional zeros
 * trimmed, at least two fractional digits kept. "149.00", "0.50", "12.345678".
 */
export function formatUnits(units: bigint, decimals = DEFAULT_DECIMALS, minFraction = 2): string {
  const negative = units < BigInt(0)
  const digits = (negative ? -units : units).toString().padStart(decimals + 1, "0")
  const whole = digits.slice(0, digits.length - decimals)
  let fraction = decimals > 0 ? digits.slice(-decimals).replace(/0+$/, "") : ""
  const keep = Math.min(minFraction, decimals)
  if (fraction.length < keep) fraction = fraction.padEnd(keep, "0")
  return `${negative ? "-" : ""}${whole}${fraction ? `.${fraction}` : ""}`
}

/** a + b, as the API would print it. */
export function addAmounts(a: string, b: string, decimals = DEFAULT_DECIMALS): string {
  return formatUnits(toUnits(a, decimals) + toUnits(b, decimals), decimals)
}

/** a − b, as the API would print it; negative results carry a "-" sign. */
export function subtractAmounts(a: string, b: string, decimals = DEFAULT_DECIMALS): string {
  return formatUnits(toUnits(a, decimals) - toUnits(b, decimals), decimals)
}

/** -1, 0 or 1 as a is less than, equal to or greater than b. */
export function compareAmounts(a: string, b: string, decimals = DEFAULT_DECIMALS): -1 | 0 | 1 {
  const x = toUnits(a, decimals)
  const y = toUnits(b, decimals)
  return x < y ? -1 : x > y ? 1 : 0
}

export function isZeroAmount(amount: string, decimals = DEFAULT_DECIMALS): boolean {
  return toUnits(amount, decimals) === BigInt(0)
}

/** Sum of amounts, as the API would print it. */
export function sumAmounts(amounts: readonly string[], decimals = DEFAULT_DECIMALS): string {
  return formatUnits(
    amounts.reduce((total, a) => total + toUnits(a, decimals), BigInt(0)),
    decimals,
  )
}

/**
 * Splits a decimal string for display: thousands separators in the whole part, and the
 * fraction exactly as given except that it is padded to `minFraction` digits. Nothing is
 * rounded or cut: what the API sent is what is shown.
 */
export function displayParts(amount: string, minFraction = 2): { negative: boolean; whole: string; fraction: string } {
  const text = amount.trim()
  const negative = text.startsWith("-")
  const [wholeRaw = "0", fractionRaw = ""] = text.replace(/^[+-]/, "").split(".")
  const whole = (wholeRaw.replace(/^0+(?=\d)/, "") || "0").replace(/\B(?=(\d{3})+(?!\d))/g, ",")
  const fraction = fractionRaw.length < minFraction ? fractionRaw.padEnd(minFraction, "0") : fractionRaw
  return { negative, whole, fraction }
}

/** "1,234.50" — plain text of an amount for clipboard, aria labels and titles. */
export function displayAmount(amount: string, minFraction = 2): string {
  const { negative, whole, fraction } = displayParts(amount, minFraction)
  return `${negative ? "-" : ""}${whole}${fraction ? `.${fraction}` : ""}`
}

/** Why a typed amount was not accepted. */
export type AmountInputProblem = "empty" | "separator" | "invalid" | "too_precise" | "zero"

/**
 * Reads what a person typed into an amount field. Exactly one format is accepted: digits, and
 * at most one dot before the decimals — "1250" or "1250.50". Nothing is guessed. A comma, a
 * space, an apostrophe or anything else between the digits is refused with the reason, because
 * "1,250" is one thousand two hundred and fifty to one person and one and a quarter to
 * another, and money must not depend on which of them wrote the parser. Signs, exponents,
 * letters, more decimals than the asset has, and zero are refused as well.
 *
 * Every amount field of the interface goes through this, and none of them rewrites what was
 * typed: the field shows the person's own text, and [amountInputMessage] says what is wrong
 * with it.
 */
export function parseAmountInput(
  input: string,
  decimals = DEFAULT_DECIMALS,
): { ok: true; amount: string } | { ok: false; reason: AmountInputProblem } {
  const text = input.trim()
  if (text === "") return { ok: false, reason: "empty" }
  const match = DECIMAL.exec(text)
  if (!match) {
    // Digits held apart or together by the usual separators: say which format is wanted.
    return { ok: false, reason: /^[\d.,'’_\s\u00a0\u202f]+$/.test(text) && /\d/.test(text) ? "separator" : "invalid" }
  }
  const whole = match[1].replace(/^0+(?=\d)/, "")
  const fraction = (match[2] ?? "").replace(/0+$/, "")
  if (fraction.length > decimals) return { ok: false, reason: "too_precise" }
  if (/^0*$/.test(whole) && fraction === "") return { ok: false, reason: "zero" }
  return { ok: true, amount: fraction ? `${whole}.${fraction}` : whole }
}

/** The format every amount field accepts, in a few words, for the hint next to the field. */
export const AMOUNT_FORMAT_HINT = "Digits, with a dot for decimals: 1250 or 1250.50."

/** What to say about a typed amount that was not accepted. */
export function amountInputMessage(reason: AmountInputProblem, decimals = DEFAULT_DECIMALS): string {
  switch (reason) {
    case "empty":
      return "Enter an amount."
    case "separator":
      return "Use digits and one dot for decimals, with no commas or spaces: 1250.50, not 1,250.50."
    case "invalid":
      return "Use digits and one dot for decimals, like 1250 or 1250.50."
    case "too_precise":
      return `At most ${decimals} digits after the dot.`
    case "zero":
      return "The amount must be more than zero."
  }
}

/**
 * An amount as a number, for chart geometry only (bar heights, axis ticks). Loses precision
 * beyond 15 significant digits by design; never show the result as a figure.
 */
export function toChartNumber(amount: string): number {
  return Number(amount)
}

/**
 * received / total as a percentage 0–100 for progress bars, computed on base units (two
 * decimals of precision). Geometry, not money.
 */
export function ratioPercent(part: string, total: string, decimals = DEFAULT_DECIMALS): number {
  const whole = toUnits(total, decimals)
  if (whole === BigInt(0)) return 0
  const scaled = (toUnits(part, decimals) * BigInt(10000)) / whole
  return Math.min(100, Number(scaled) / 100)
}

/**
 * Change of `current` against `previous` in percent, to one decimal, computed on base units.
 * Null when there is nothing to compare with (previous is zero). A statistic, not an amount.
 */
export function percentChange(current: string, previous: string, decimals = DEFAULT_DECIMALS): number | null {
  const before = toUnits(previous, decimals)
  if (before === BigInt(0)) return null
  const tenths = ((toUnits(current, decimals) - before) * BigInt(1000)) / before
  return Number(tenths) / 10
}
