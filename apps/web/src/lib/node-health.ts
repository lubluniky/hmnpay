import type { NodeStatus } from "./api/types"

export interface NodeHealth {
  tone: "neutral" | "info" | "success" | "warning" | "danger"
  /** One or two words for the sidebar. */
  label: string
  /** A short second line: what that means right now. */
  detail: string
  /** A slow pulse: the watcher is working. */
  live: boolean
}

/**
 * The state of the chain connection in plain words, derived in one place so the sidebar, the
 * overview and the network page agree. In the order an operator needs to know it:
 *
 *   nothing known yet → starting → confirmations on hold → no first block yet → not syncing →
 *   one endpoint trusted alone → something to look at → resting (idle or quiet) → behind → fine.
 *
 * "Starting" comes first on purpose: right after a start the server reports healthy=false,
 * and that is not a fault. A hold comes before "not syncing": while confirmations wait the
 * node may be healthy or not, and the hold is what the operator has to act on.
 *
 * "Idle" and "Quiet" are healthy resting states, never "syncing" or "behind": no payment is
 * open, so the watcher pauses between looks at the chain, and the blocks it trails by then are
 * that pause. Idle: nothing is watched at all. Quiet: only the addresses of closed payments are
 * watched, for money sent to them late; every pass still scans every block for them.
 */
/**
 * Warning sentences of the server that a page says in its own words instead: endpoints that look
 * like one provider (same_provider), and the addresses kept for a second endpoint's scan
 * (retained_unchecked: normal while it lasts, shown with the watched addresses, not a warning).
 */
const SAID_OTHERWISE = [
  /look like the same provider/,
  /stay on the watch list past their window until blocks settled on one endpoint/,
  // network_problem, which a page says on its own (it heads the warnings as well).
  /^the network settings cannot be used: /,
  // awaiting_finality, finality stalled and finality dispute: said by finalityNotices' callers.
  /payment\(s\) have their confirmations and are waiting for the chain to finalise block/,
  /^finality stalled: /,
  /^finality dispute: /,
]

const after = (prefix: RegExp) => (status: NodeStatus) => {
  const warning = status.warnings.find((w) => prefix.test(w))
  return warning === undefined ? undefined : warning.replace(prefix, "")
}

/**
 * The chain has not finalised a block for two minutes while blocks kept coming (the server raised
 * finality_stalled): payments with their confirmations stay confirming until it does. The server's
 * sentence, without its prefix.
 */
export const finalityStalled = after(/^finality stalled: /)

/** Two endpoints disagree about the finalised block: the server's sentence, which names them. */
export const finalityDispute = after(/^finality dispute: /)

const n = (value: string) => Number(value).toLocaleString("en-US")

/**
 * What a hold means, in a sentence an operator can act on, put before the server's own reason.
 * Every held head has a reason in `hold`; these are the ones worth saying in other words.
 */
export function holdInWords(hold: string): string | undefined {
  if (/^settling again the blocks below where the head was/.test(hold))
    return "The watcher went back and is settling those blocks again; confirmations go on once it is back where it was."
  if (/^the logs of .+ nothing above block \d+ is confirmed by a second endpoint/.test(hold))
    return "The second endpoint that checks log scans is behind, so newer blocks are not confirmed by two endpoints yet."
  if (/agree on a finalized block/.test(hold)) return "The endpoints have not agreed on a finalised block yet, so no block counts as final."
  if (/is at block \d+, \d+ blocks behind .+; nothing is (scanned or )?settled beyond what both have/.test(hold))
    return "One endpoint is behind the other; nothing is settled beyond what both have."
  if (/reports the chain at block \d+, but nothing above block \d+ could be settled/.test(hold))
    return "An endpoint shows a longer chain than the endpoints in use can confirm."
  if (/waiting to be confirmed by another endpoint/.test(hold)) return "A transfer seen by one endpoint is waiting for another to confirm it."
  return undefined
}

/**
 * What the watcher is busy with (`work`), when it is worth saying: a re-scan, blocks owed a
 * second log scan, transfers or endpoints waiting for each other. Routine work — addresses
 * watched, transfers inside the reorg window, a head about to be reported — and what other
 * notices already say (a hold, a failed pass, the start) is left out.
 */
export function workInWords(work: string): string | undefined {
  let m: RegExpExecArray | null
  if ((m = /^blocks (\d+)\.\.(\d+) were settled on one endpoint's logs and are still to be scanned by a second one \((.*)\)$/.exec(work)))
    return `Blocks ${n(m[1])} to ${n(m[2])} were read from one endpoint’s logs only; a second log endpoint still has to scan them (${m[3]}). Until it does, their addresses stay watched.`
  if ((m = /^a re-scan is under way: block (\d+) of (\d+)$/.exec(work))) return `Scanning the chain again: at block ${n(m[1])} of ${n(m[2])}.`
  if ((m = /^a re-scan from block (\d+) has been asked for$/.exec(work))) return `A new scan from block ${n(m[1])} has been asked for.`
  if ((m = /^catching up: the cursor is at block (\d+), the chain at block (\d+)$/.exec(work))) return `Catching up: processed block ${n(m[1])}, the chain is at block ${n(m[2])}.`
  if ((m = /^(\d+) transfer\(s\) are waiting to be confirmed by another endpoint$/.exec(work)))
    return `${n(m[1])} transfer${m[1] === "1" ? " seen by one endpoint is" : "s seen by one endpoint are"} waiting for another endpoint to confirm ${m[1] === "1" ? "it" : "them"}.`
  if ((m = /^the endpoints are (\d+) blocks apart about where the chain ends$/.exec(work))) return `The endpoints are ${n(m[1])} blocks apart about where the chain ends.`
  if (work === "the endpoints disagree about where the chain ends" || work === "a disagreement with the endpoints is being resolved")
    return "Settling a disagreement between the endpoints about the chain."
  if (work === "a reorganisation was dealt with in the last tick") return "A chain reorganisation was handled in the last pass."
  if (work === "no second endpoint confirmed the chain in the last tick") return "No second endpoint confirmed the chain in the last pass; nothing is settled on one endpoint’s word."
  if (/^.+ shows the chain at block \d+, more than two settle depths above the cursor/.test(work)) return `${work.charAt(0).toUpperCase()}${work.slice(1)}.`
  return undefined
}

/** The hold the watcher keeps while the endpoints have not agreed on any finalised block. */
export const isFinalityHold = (hold: string) => /agree on a finalized block/.test(hold)

const payments = (n: number) => `${n.toLocaleString("en-US")} payment${n === 1 ? "" : "s"}`

/** A warning that two endpoints disagree about the time of the chain: one of them is wrong. */
export const isClockDispute = (warning: string) => /^clock dispute: /.test(warning)

/** The server's warnings an operator is shown as warnings. */
export const operatorWarnings = (status: NodeStatus) => status.warnings.filter((w) => !SAID_OTHERWISE.some((r) => r.test(w)))

/** A warning that the server clock is behind the chain: payments are refused while it lasts. */
export const isClockBehind = (warning: string) => /clock is behind|clock is .* behind the chain/i.test(warning)

/** Endpoint states in which an endpoint cannot be used right now. */
const UNUSABLE = new Set(["failing", "unverified", "wrong_chain", "unsupported"])

/** The configured endpoints that cannot be used right now (down, unanswered, wrong chain…). */
export const unusableEndpoints = (status: NodeStatus) => status.endpoints.filter((e) => UNUSABLE.has(e.state)).map((e) => e.url)

/**
 * The watcher has run but has not processed a single block yet. With two or more endpoints it
 * starts only once two of them agree on where the chain ends, so one endpoint that never comes up
 * (a mistyped URL, a provider that is down) keeps it here, and no payment can be created.
 */
export const waitingForFirstBlock = (status: NodeStatus) => !status.starting && status.head === null && status.last_checked_at !== null

export function nodeHealth(status: NodeStatus | undefined, failed = false): NodeHealth {
  if (!status) {
    return failed
      ? { tone: "danger", label: "Unknown", detail: "Status unavailable", live: false }
      : { tone: "neutral", label: "Checking", detail: "Asking the server", live: false }
  }
  // The stored network settings can't be used: no watcher runs, nothing else is worth saying first.
  if (status.network_problem) return { tone: "danger", label: "Not running", detail: "The network settings need repair", live: false }
  if (status.starting) return { tone: "info", label: "Starting", detail: "First pass over the chain", live: true }
  if (status.hold) {
    return { tone: "warning", label: "Waiting", detail: isFinalityHold(status.hold) ? "Endpoints disagree on finality" : /^settling again/.test(status.hold) ? "Settling blocks again" : "Confirmations are on hold", live: true }
  }
  if (waitingForFirstBlock(status)) {
    const unusable = unusableEndpoints(status).length
    return {
      tone: "warning",
      label: "Not started",
      detail: unusable === 0 ? "Waiting for the first block" : unusable === 1 ? "An endpoint can’t be used" : `${unusable} endpoints can’t be used`,
      live: unusable === 0,
    }
  }
  if (!status.healthy) {
    if (!status.last_checked_at) return { tone: "danger", label: "Not running", detail: "The watcher has not reported", live: false }
    // "No usable endpoint" only when that is so: an endpoint that answers but is behind, or serves
    // an old chain, keeps the watcher from syncing as well.
    const unusable = unusableEndpoints(status).length
    const detail =
      status.endpoints.length > 0 && unusable === status.endpoints.length
        ? "No usable RPC endpoint"
        : unusable > 0
          ? `${unusable} endpoint${unusable === 1 ? "" : "s"} can’t be used`
          : "Behind the chain"
    return { tone: "danger", label: "Not syncing", detail, live: false }
  }
  if (status.solo) return { tone: "warning", label: "Unverified", detail: "One endpoint trusted alone", live: true }
  // Finality: a stall or a dispute is a warning; payments waiting for it briefly are normal.
  const waiting = status.awaiting_finality
  if (finalityStalled(status) !== undefined) {
    return { tone: "warning", label: "Finality stalled", detail: waiting ? `${payments(waiting.payments)} waiting` : "The chain is not finalising", live: true }
  }
  if (finalityDispute(status) !== undefined) return { tone: "warning", label: "Check", detail: "Endpoints disagree on finality", live: true }
  const warnings =
    operatorWarnings(status).length +
    (status.logs_unchecked ? 1 : 0) +
    (status.watch_list_full ? 1 : 0) +
    (status.released_unchecked > 0 ? 1 : 0) +
    status.same_provider.length
  if (warnings > 0) return { tone: "warning", label: "Check", detail: `${warnings} warning${warnings > 1 ? "s" : ""}`, live: true }
  if (waiting) return { tone: "info", label: "Finalising", detail: `${payments(waiting.payments)} waiting for finality`, live: true }
  if (status.idle) {
    return status.quiet
      ? { tone: "success", label: "Quiet", detail: "Watching closed payments for late funds", live: true }
      : { tone: "success", label: "Idle", detail: "Nothing to watch", live: true }
  }
  if (status.lag_blocks > 20) {
    return { tone: "warning", label: "Catching up", detail: `${status.lag_blocks.toLocaleString("en-US")} blocks behind`, live: true }
  }
  return { tone: "success", label: "Synced", detail: `${status.rpc_latency_ms} ms`, live: true }
}

/** The network's name for a chain id hmnpay supports. */
export const chainName = (chainId: number) => (chainId === 97 ? "BSC Testnet" : "BNB Smart Chain")
