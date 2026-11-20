import type { Tone } from "@/components/hmn"
import type { LinkInput } from "@/lib/api/client"
import type { LinkStatus, PaymentLink } from "@/lib/api/types"
import { amountInputMessage, compareAmounts, parseAmountInput, sumAmounts } from "@/lib/money"

/* --------------------------------- status --------------------------------- */

export const LINK_STATUS: Record<LinkStatus, { label: string; tone: Tone; hollow: boolean; reason: string }> = {
  active: { label: "Active", tone: "success", hollow: false, reason: "Taking payments." },
  disabled: { label: "Disabled", tone: "neutral", hollow: true, reason: "Turned off. Customers see “no longer available”." },
  expired: { label: "Expired", tone: "neutral", hollow: true, reason: "Past its end date." },
  limit: { label: "Limit reached", tone: "neutral", hollow: true, reason: "Every allowed payment has been made." },
}

/** "pay.example.com/l/pro-annual": the link without its scheme, split for display. */
export function linkParts(url: string) {
  const bare = url.replace(/^[a-z]+:\/\//i, "")
  const cut = bare.lastIndexOf("/") + 1
  return { base: bare.slice(0, cut), slug: bare.slice(cut) }
}

/** Total collected over several links, exact (amounts are decimal strings and never go through floats). */
export function sumCollected(links: PaymentLink[], decimals: number) {
  return sumAmounts(
    links.map((link) => link.total_collected),
    decimals,
  )
}

/** The least a customer may choose on a link without a fixed price when the link sets none: one whole unit. */
export const DEFAULT_MIN_AMOUNT = "1"

/* ---------------------------------- draft --------------------------------- */

export interface LinkDraft {
  name: string
  pricing: "fixed" | "open"
  amount: string
  /** Customer chooses: the least they may pay. Empty means the default, one whole unit. */
  minAmount: string
  slug: string
  /** False until the operator edits the slug by hand; until then it follows the name. */
  slugEdited: boolean
  description: string
  redirect: boolean
  redirectUrl: string
  limit: boolean
  maxUses: string
  expires: boolean
  /** YYYY-MM-DD */
  expiresOn: string
}

export const EMPTY_DRAFT: LinkDraft = {
  name: "",
  pricing: "fixed",
  amount: "",
  minAmount: "",
  slug: "",
  slugEdited: false,
  description: "",
  redirect: false,
  redirectUrl: "",
  limit: false,
  maxUses: "",
  expires: false,
  expiresOn: "",
}

export function slugify(value: string) {
  return value
    .toLowerCase()
    .normalize("NFKD")
    .replace(/[̀-ͯ]/g, "")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48)
}

export type DraftErrors = Partial<Record<"name" | "amount" | "minAmount" | "slug" | "description" | "redirectUrl" | "maxUses" | "expiresOn", string>>

/**
 * What can be said before asking the server. The server validates everything again and has the
 * last word (its field errors are shown on the same fields); this only saves a round trip for
 * the obvious.
 */
export function validateDraft(draft: LinkDraft, today: string, decimals: number, original?: PaymentLink): DraftErrors {
  const errors: DraftErrors = {}
  if (!draft.name.trim()) errors.name = "Give the link a name customers will recognise."
  if (draft.pricing === "fixed") {
    const parsed = parseAmountInput(draft.amount, decimals)
    if (!parsed.ok) errors.amount = parsed.reason === "empty" ? "Enter the price." : amountInputMessage(parsed.reason, decimals)
  } else if (draft.minAmount.trim()) {
    const parsed = parseAmountInput(draft.minAmount, decimals)
    if (!parsed.ok) errors.minAmount = parsed.reason === "zero" ? "The minimum can’t be less than 1." : amountInputMessage(parsed.reason, decimals)
    else if (compareAmounts(parsed.amount, DEFAULT_MIN_AMOUNT, decimals) < 0) errors.minAmount = "The minimum can’t be less than 1."
  }
  // An existing link keeps its ending for good (the server ignores it on an update).
  if (!original && !draft.slug) errors.slug = "The link needs an ending."
  if (draft.redirect && !/^https?:\/\/[^\s/]+/.test(draft.redirectUrl.trim())) {
    errors.redirectUrl = "Enter a full URL, starting with https://."
  }
  if (draft.limit && !/^[1-9]\d*$/.test(draft.maxUses.trim())) errors.maxUses = "Enter a whole number, 1 or more."
  // An end date that is already set may stay as it is, even once it has passed: editing the
  // name of an expired link must not force a new date on it.
  const keptDate = original?.expires_at != null && draft.expiresOn === original.expires_at.slice(0, 10)
  if (draft.expires && !keptDate && (!draft.expiresOn || draft.expiresOn <= today)) errors.expiresOn = "Pick a date after today."
  return errors
}

/**
 * What is wrong with the text of the amount fields as it stands, before the form is sent: only
 * that it is not an amount (a comma, a space, a letter). An empty field and a value the rules
 * refuse are left to [validateDraft].
 */
export function amountTyping(draft: LinkDraft, decimals: number): Pick<DraftErrors, "amount" | "minAmount"> {
  const problem = (text: string) => {
    const parsed = parseAmountInput(text, decimals)
    return parsed.ok || parsed.reason === "empty" || parsed.reason === "zero" ? undefined : amountInputMessage(parsed.reason, decimals)
  }
  return draft.pricing === "fixed" ? { amount: problem(draft.amount) } : { minAmount: problem(draft.minAmount) }
}

/** The form filled in from an existing link, for editing it. */
export function draftFromLink(link: PaymentLink): LinkDraft {
  return {
    name: link.name,
    pricing: link.amount === null ? "open" : "fixed",
    amount: link.amount ?? "",
    minAmount: link.min_amount ?? "",
    slug: link.slug,
    slugEdited: true,
    description: link.description,
    redirect: link.redirect_url !== "",
    redirectUrl: link.redirect_url,
    limit: link.max_uses !== null,
    maxUses: link.max_uses === null ? "" : String(link.max_uses),
    expires: link.expires_at !== null,
    expiresOn: link.expires_at?.slice(0, 10) ?? "",
  }
}

/**
 * The request body for a draft. Amounts stay exact decimal strings.
 *
 * With `original` it is the body of an update, which replaces every editable field: the link
 * stays switched on or off as it was, and an end date whose day was not changed keeps its
 * exact time (a link made through the API may end at any time of day, not only at midnight).
 */
export function draftToInput(draft: LinkDraft, decimals: number, original?: PaymentLink): LinkInput {
  const amount = draft.pricing === "fixed" ? parseAmountInput(draft.amount, decimals) : null
  const min = draft.pricing === "open" && draft.minAmount.trim() ? parseAmountInput(draft.minAmount, decimals) : null
  return {
    slug: draft.slug,
    name: draft.name.trim(),
    description: draft.description.trim(),
    amount: amount?.ok ? amount.amount : null,
    min_amount: min?.ok ? min.amount : null,
    max_uses: draft.limit ? Number(draft.maxUses) : null,
    // The link closes at the end of that day, UTC.
    expires_at: !draft.expires
      ? null
      : original?.expires_at != null && original.expires_at.slice(0, 10) === draft.expiresOn
        ? original.expires_at
        : `${draft.expiresOn}T23:59:59Z`,
    redirect_url: draft.redirect ? draft.redirectUrl.trim() : "",
    active: original ? original.active : true,
  }
}

/** The server's field names on the draft's own. */
export const LINK_FIELD_NAMES: Record<string, string> = {
  min_amount: "minAmount",
  redirect_url: "redirectUrl",
  max_uses: "maxUses",
  expires_at: "expiresOn",
}
