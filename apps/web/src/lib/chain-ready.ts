/**
 * "chain_not_ready": the server refuses to create a payment (503, Retry-After) until its chain
 * watcher has processed the first block of the network — normally a second or two after setup
 * or a change of network, longer while no RPC endpoint can be agreed with. No payment, no
 * address and no use of a payment link is taken by such a refusal, so trying again is safe.
 *
 * The operator is told what it is and where to look; a payer is never shown an error for it,
 * only a calm wait while the page tries again by itself.
 */

export const CHAIN_NOT_READY = "chain_not_ready"

/**
 * "clock_behind": the server refuses to create a payment (503, Retry-After 60) while its clock is
 * more than the tolerance behind the chain: deadlines would be set early. Waiting does not end it;
 * the operator has to correct the server's time. Nothing is taken by the refusal.
 */
export const CLOCK_BEHIND = "clock_behind"

/** For the operator, wherever a payment is created in the dashboard. */
export const CLOCK_BEHIND_OPERATOR = {
  title: "The server’s clock is behind",
  text: "The clock of the server hmnpay runs on is behind the blockchain, so no payment can be created: its deadline would be set too early. This does not go away by waiting. Correct the server’s time (turn on time synchronisation, NTP); payments can be created again as soon as it is right.",
  short: "The server’s clock is behind the blockchain, so no payment can be created. Correct the server’s time; waiting does not fix it.",
}

/** For the operator, wherever a payment is created in the dashboard. */
export const CHAIN_NOT_READY_OPERATOR = {
  title: "The server has not finished reading the chain yet",
  text: "No payment can be created until it has processed its first block, normally a few seconds after setup or a change of network. Try again in a moment. If this lasts, the Network page shows what it is waiting for.",
  /** describeError's one-line form. */
  short: "The server has not finished reading the chain yet. Try again in a moment.",
}

/**
 * "network_invalid": the server refuses to create a payment (503, Retry-After 60) while its stored
 * network settings break the rules new ones are held to (settings an earlier version accepted, two
 * URLs of one endpoint for example): it runs no chain watcher. Waiting does not end it.
 */
export const NETWORK_INVALID = "network_invalid"

/** For the operator, wherever a payment is created in the dashboard. */
export const NETWORK_INVALID_OPERATOR = {
  title: "The network settings need repair",
  text: "The server’s saved network settings can’t be used, so it watches no chain and creates no payment. This does not go away by waiting: fix them on the Network page, where the entry that is wrong is marked. Payments work again as soon as they are saved.",
  short: "The server’s network settings can’t be used, so no payment can be created. Fix them on the Network page.",
}

/** For a payer on a payment-link page: what they see while the page tries again by itself. */
export const CHAIN_NOT_READY_PAYER = {
  title: "Getting your payment ready",
  text: "The payment service isn’t ready to take your payment yet. This page tries again by itself in a moment; you don’t need to do anything, and nothing has been charged.",
  button: "Getting your payment ready…",
}

/**
 * After this long of trying by itself (chain_not_ready or clock_behind), the page says so: the
 * state can last (a server clock that is wrong, an endpoint that is down), and a payer should not be
 * left waiting without knowing. The page keeps trying.
 */
export const PAYER_PATIENCE_SECONDS = 45

/** What a payer sees once the page has been trying for a while. `merchant` is the business's name. */
export function payerUnavailable(merchant: string) {
  return {
    title: "Payments are paused for the moment",
    text: `${merchant} can’t take payments through this page right now. Please try again later, or contact ${merchant}. Nothing has been charged; this page keeps trying in the meantime.`,
    button: "Payments are paused",
  }
}

/**
 * Seconds until the payer's page asks again, after `attempt` refusals (1 for the first). The
 * server's Retry-After is a floor; the waits grow (3, 6, 12, 20 seconds, then once a minute) so
 * a page left open while the server cannot reach the chain stays inside the public checkout's
 * rate limit: five requests per address, then one a minute.
 */
export function chainNotReadyDelay(attempt: number, retryAfter?: number): number {
  const steps = [3, 6, 12, 20]
  const own = attempt <= steps.length ? steps[Math.max(0, attempt - 1)] : 60
  return Math.max(own, retryAfter && retryAfter > 0 ? retryAfter : 0)
}
