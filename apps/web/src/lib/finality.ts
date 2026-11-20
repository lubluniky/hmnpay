/**
 * Finality, said for each reader. A payment is paid only once the block holding its money has been
 * finalised by the chain (as two endpoints attest) and has the configured confirmations. Until then
 * it stays "confirming"; when it has its confirmations and only finality is missing, the views carry
 * `awaiting_finality` (the oldest block waited for, and the newest final block). On BNB Smart Chain
 * with finality working this lasts seconds, if at all; when the chain stops finalising, it lasts as
 * long as that.
 */
import type { FinalityWait, PaymentStatus } from "./api/types"

/** Whether a payment (or checkout) has all its confirmations and waits only for finality. */
export const isFinalising = (p: { status: PaymentStatus; awaiting_finality: FinalityWait | null }) =>
  p.awaiting_finality !== null && (p.status === "confirming" || p.status === "detected")

/** The status pill and its description, in lists and headers. */
export const FINALISING = {
  label: "Finalising",
  description: "All confirmations are in; the chain is finalising the block that holds the payment.",
}

/**
 * Whether finality is still short of the block (as it is whenever the server says a payment waits).
 * The final block never goes back and can be above the processed head for a while: a view read at
 * that moment must not say "finalised so far 90 of 86".
 */
export const finalityShort = (wait: FinalityWait) => wait.final < wait.block

/** For the operator: which block the payment waits for, and how far finality has come. */
export function operatorFinalityLine(wait: FinalityWait): string {
  const n = (value: number) => value.toLocaleString("en-US")
  if (!finalityShort(wait)) return `The chain has finalised block ${n(wait.block)}; the payment is settled at the watcher's next pass.`
  return `Waiting for the chain to finalise block ${n(wait.block)}; finalised so far: ${n(wait.final)}.`
}

/** After this long in the finalising state, the payer's page adds a sentence that it is taking longer. */
export const FINALISING_PATIENCE_SECONDS = 120

/** For a payer on the checkout: received and counted, nothing more to do. No block numbers, no jargon. */
export function payerFinalising(merchant: string) {
  return {
    title: "Payment received",
    text: `Your payment has arrived and has all the confirmations it needs. The network is finishing its final check now; there is nothing more for you to do. You can close this page — ${merchant} is told as soon as it is done.`,
    longer: "The network is taking longer than usual to finish. Your payment is safe, and there is still nothing you need to do.",
    line: "Final check by the network",
    badge: "Final check",
  }
}
