import assert from "node:assert/strict"
import { test } from "node:test"

import type { NodeStatus } from "./api/types.ts"
import {
  finalityDispute,
  finalityStalled,
  holdInWords,
  isClockBehind,
  isClockDispute,
  isFinalityHold,
  nodeHealth,
  operatorWarnings,
  unusableEndpoints,
  waitingForFirstBlock,
  workInWords,
} from "./node-health.ts"

const healthy: NodeStatus = {
  healthy: true,
  starting: false,
  idle: false,
  quiet: false,
  head: { chain_id: 56, number: 100, final: 98, hash: "0xabc", time: null, checked: null, observed_at: null },
  latest_block: 100,
  lag_blocks: 0,
  rpc_latency_ms: 42,
  last_checked_at: "2026-11-18T12:00:00Z",
  endpoints: [],
  primary: "https://a.example",
  verifier: "https://b.example",
  log_verifier: "",
  solo: false,
  logs_unchecked: false,
  hold: "",
  warnings: [],
  watched_addresses: 0,
  watched_link_addresses: 0,
  max_watched_addresses: 5000,
  watch_list_full: false,
  network_problem: "",
  same_provider: [],
  retained_unchecked: 0,
  released_unchecked: 0,
  awaiting_finality: null,
  work: "",
}

test("the node status reads in plain words, most urgent first", () => {
  assert.equal(nodeHealth(undefined).label, "Checking")
  assert.equal(nodeHealth(undefined, true).label, "Unknown")
  assert.deepEqual(nodeHealth(healthy), { tone: "success", label: "Synced", detail: "42 ms", live: true })

  // Right after a start the server says healthy=false; that is "starting", not a fault.
  assert.equal(nodeHealth({ ...healthy, healthy: false, starting: true }).label, "Starting")
  assert.equal(nodeHealth({ ...healthy, healthy: false, starting: true }).tone, "info")

  // A hold is what the operator must act on, healthy or not.
  assert.equal(nodeHealth({ ...healthy, hold: "waiting for https://b.example to confirm block 101" }).label, "Waiting")
  assert.equal(nodeHealth({ ...healthy, healthy: false, hold: "x" }).label, "Waiting")

  assert.equal(nodeHealth({ ...healthy, healthy: false }).label, "Not syncing")
  assert.equal(nodeHealth({ ...healthy, healthy: false, last_checked_at: null }).label, "Not running")
  assert.equal(nodeHealth({ ...healthy, solo: true }).label, "Unverified")
  assert.equal(nodeHealth({ ...healthy, warnings: ["server clock is behind"] }).detail, "1 warning")
  assert.equal(nodeHealth({ ...healthy, warnings: ["a"], logs_unchecked: true }).detail, "2 warnings")
  assert.equal(nodeHealth({ ...healthy, watch_list_full: true }).label, "Check")
  assert.equal(nodeHealth({ ...healthy, lag_blocks: 1200 }).detail, "1,200 blocks behind")
  assert.equal(nodeHealth({ ...healthy, lag_blocks: 20 }).label, "Synced")

  // Idle is healthy: the blocks it trails by are the pause between looks, not a lag.
  assert.deepEqual(nodeHealth({ ...healthy, idle: true, lag_blocks: 85 }), { tone: "success", label: "Idle", detail: "Nothing to watch", live: true })
  // …but a problem is still said first.
  assert.equal(nodeHealth({ ...healthy, idle: true, warnings: ["one endpoint serves no logs"] }).label, "Check")
  assert.equal(nodeHealth({ ...healthy, idle: true, healthy: false }).label, "Not syncing")

  // A field a newer server adds must never read as a fault here.
  const newer = { ...healthy, some_future_field: 1 } as unknown as NodeStatus
  assert.equal(nodeHealth(newer).tone, "success")
  assert.equal(nodeHealth({ ...newer, idle: true, lag_blocks: 85 }).label, "Idle")
})

test("idle and quiet read as healthy resting states, in plain words", () => {
  const idle = nodeHealth({ ...healthy, idle: true, quiet: false, lag_blocks: 85 })
  assert.deepEqual(idle, { tone: "success", label: "Idle", detail: "Nothing to watch", live: true })

  const quiet = nodeHealth({ ...healthy, idle: true, quiet: true, watched_addresses: 3, lag_blocks: 85 })
  assert.deepEqual(quiet, { tone: "success", label: "Quiet", detail: "Watching closed payments for late funds", live: true })

  // However far the pause puts the head behind, a resting node is neither syncing nor lagging.
  for (const status of [{ idle: true, quiet: false }, { idle: true, quiet: true }]) {
    const health = nodeHealth({ ...healthy, ...status, lag_blocks: 100_000 })
    assert.equal(health.tone, "success")
    assert.doesNotMatch(`${health.label} ${health.detail}`, /sync|behind|lag|catching/i)
  }

  // Quiet is a kind of idle: without idle it means nothing, and the node is judged as working.
  assert.equal(nodeHealth({ ...healthy, idle: false, quiet: true }).label, "Synced")
  assert.equal(nodeHealth({ ...healthy, idle: false, quiet: true, lag_blocks: 1200 }).label, "Catching up")

  // A problem is still said before the resting state.
  assert.equal(nodeHealth({ ...healthy, idle: true, quiet: true, solo: true }).label, "Unverified")
  assert.equal(nodeHealth({ ...healthy, idle: true, quiet: true, healthy: false }).label, "Not syncing")
  assert.equal(nodeHealth({ ...healthy, idle: true, quiet: true, hold: "x" }).label, "Waiting")
})

test("no first block yet: the indicator says the instance has not started, and why, never 'no usable endpoint'", () => {
  const before: NodeStatus = {
    ...healthy,
    healthy: false,
    head: null,
    endpoints: [
      { url: "https://a.example", healthy: true, in_use: true, state: "healthy", latency_ms: 40, block_number: 100, error: "" },
      { url: "https://b.example", healthy: false, in_use: false, state: "failing", latency_ms: 0, block_number: 0, error: "eth_chainId: transport: EOF" },
    ],
    warnings: ["the last 7 scans failed: watcher: cannot choose a start position: no second endpoint to confirm the chain head"],
  }
  assert.equal(waitingForFirstBlock(before), true)
  assert.deepEqual(unusableEndpoints(before), ["https://b.example"])
  assert.deepEqual(nodeHealth(before), { tone: "warning", label: "Not started", detail: "An endpoint can’t be used", live: false })
  // The same while the server still calls itself healthy (its first minute) and lists the warning.
  assert.equal(nodeHealth({ ...before, healthy: true }).label, "Not started")
  // Two that cannot be used; and none (then it is simply waiting).
  const both: NodeStatus = { ...before, endpoints: before.endpoints.map((e) => ({ ...e, state: "failing" as const })) }
  assert.equal(nodeHealth(both).detail, "2 endpoints can’t be used")
  const waiting = { ...before, endpoints: [before.endpoints[0]] }
  assert.deepEqual(nodeHealth(waiting), { tone: "warning", label: "Not started", detail: "Waiting for the first block", live: true })
  // Not before the first pass (that is "Starting"), not once there is a head.
  assert.equal(nodeHealth({ ...before, starting: true }).label, "Starting")
  assert.equal(waitingForFirstBlock({ ...before, last_checked_at: null }), false)
  assert.equal(nodeHealth({ ...before, head: healthy.head }).label, "Not syncing")
  // A hold is still said first.
  assert.equal(nodeHealth({ ...before, hold: "x" }).label, "Waiting")
  for (const s of [before, both, waiting]) assert.doesNotMatch(`${nodeHealth(s).label} ${nodeHealth(s).detail}`, /no usable|syncing/i)
})

test("what the node status adds about the watch list and the endpoints, weighed for the operator", () => {
  // Addresses kept for a second endpoint's check are normal operation: not a warning.
  const retained = { ...healthy, retained_unchecked: 3, warnings: ["3 settled payment address(es) stay on the watch list past their window until blocks settled on one endpoint's logs have been scanned by a second log endpoint (at most 5000 are kept)"] }
  assert.deepEqual(operatorWarnings(retained), [])
  assert.equal(nodeHealth(retained).label, "Synced")
  // Addresses let go before that check: the operator has to act.
  assert.equal(nodeHealth({ ...healthy, released_unchecked: 2 }).label, "Check")
  // Endpoints that look like one provider: a warning, said once (the server's sentence is left out).
  const provider = { ...healthy, same_provider: [["https://a.example.org", "https://b.example.org"]], warnings: ["https://a.example.org and https://b.example.org look like the same provider: the second opinion they give each other is worth less. Configure endpoints of independent providers."] }
  assert.deepEqual(operatorWarnings(provider), [])
  assert.deepEqual(nodeHealth(provider), { tone: "warning", label: "Check", detail: "1 warning", live: true })
  // The clock behind the chain is recognised in the watcher's words.
  assert.equal(isClockBehind("server clock skew: the newest block is stamped 5m36s ahead of this server's clock; the clock is behind — check time synchronisation (NTP)"), true)
  assert.equal(isClockBehind("server clock skew: the newest block is stamped 3m ago; the clock is ahead"), false)
})

test("stored network settings that can't be used: not running, and why, before anything else", () => {
  const problem = "rpc_urls.1: this URL reaches the same endpoint (http://127.0.0.1:8545) as another one in the list"
  const invalid: NodeStatus = {
    ...healthy,
    healthy: false,
    last_checked_at: null,
    network_problem: problem,
    warnings: [`the network settings cannot be used: ${problem} — no chain watcher runs and no payment can be created until they are fixed in the network settings`],
  }
  assert.deepEqual(nodeHealth(invalid), { tone: "danger", label: "Not running", detail: "The network settings need repair", live: false })
  assert.equal(nodeHealth({ ...invalid, starting: true }).label, "Not running")
  // Its warning is said by the page on its own, not again as a watcher warning.
  assert.deepEqual(operatorWarnings(invalid), [])
  assert.equal(nodeHealth({ ...invalid, network_problem: "", warnings: [] }).detail, "The watcher has not reported")
})

test("a clock dispute is recognised in the watcher's words", () => {
  assert.equal(isClockDispute("clock dispute: the endpoints disagree about the time of the chain's newest block — the newest block of https://a.example puts this server's clock 3m0s behind, that of https://b.example 1s ahead"), true)
  assert.equal(isClockDispute("server clock skew: the newest block is stamped 5m ahead of this server's clock; the clock is behind — check time synchronisation (NTP)"), false)
})

test("finality: a brief wait is normal, a stall or a dispute is a warning, and each is said once", () => {
  const waitSentence = "2 payment(s) have their confirmations and are waiting for the chain to finalise block 1200 (the newest final block is 1190); they confirm when it does, and stay confirming while it does not"
  const waiting: NodeStatus = { ...healthy, awaiting_finality: { payments: 2, block: 1200, final: 1190 }, warnings: [waitSentence] }
  assert.deepEqual(nodeHealth(waiting), { tone: "info", label: "Finalising", detail: "2 payments waiting for finality", live: true })
  assert.deepEqual(operatorWarnings(waiting), [])
  assert.equal(nodeHealth({ ...waiting, awaiting_finality: { payments: 1, block: 1200, final: 1190 } }).detail, "1 payment waiting for finality")

  const stall = "the chain has not finalised a block for 2m0s: the newest final block is 1190, the processed head 1260; payments in later blocks have their confirmations but stay confirming until it does"
  const stalled: NodeStatus = { ...waiting, warnings: [waitSentence, `finality stalled: ${stall}`] }
  assert.equal(finalityStalled(stalled), stall)
  assert.deepEqual(nodeHealth(stalled), { tone: "warning", label: "Finality stalled", detail: "2 payments waiting", live: true })
  assert.deepEqual(operatorWarnings(stalled), [])

  const dispute = "https://a.example reports block 1250 as finalized, https://b.example block 1190: a block is final when both say so, so payments wait for https://b.example"
  const disputed: NodeStatus = { ...waiting, warnings: [waitSentence, `finality dispute: ${dispute}`] }
  assert.equal(finalityDispute(disputed), dispute)
  assert.equal(finalityDispute(healthy), undefined)
  assert.deepEqual(nodeHealth(disputed), { tone: "warning", label: "Check", detail: "Endpoints disagree on finality", live: true })
  assert.deepEqual(operatorWarnings(disputed), [])

  // No agreed finalised block yet: the hold, in its own words.
  const hold = "waiting for the endpoints to agree on a finalized block: they have not reported the same one yet"
  assert.equal(isFinalityHold(hold), true)
  assert.equal(nodeHealth({ ...healthy, hold }).detail, "Endpoints disagree on finality")
  assert.equal(nodeHealth({ ...healthy, hold: "https://a.example cannot be reached" }).detail, "Confirmations are on hold")
})

test("a held head and the watcher's work, in plain words; routine work is not news", () => {
  assert.match(holdInWords("settling again the blocks below where the head was (block 900; the cursor is at block 870): a re-scan is under way: block 870 of 900")!, /went back and is settling those blocks again/)
  assert.match(
    holdInWords("the logs of https://b.example are at block 850, 50 blocks behind the chain (block 900): nothing above block 850 is confirmed by a second endpoint")!,
    /second endpoint that checks log scans is behind/,
  )
  assert.match(holdInWords("https://b.example is at block 880, 20 blocks behind https://a.example; nothing is settled beyond what both have")!, /One endpoint is behind the other/)
  assert.equal(holdInWords("something new"), undefined)

  assert.equal(
    workInWords("blocks 1200..1260 were settled on one endpoint's logs and are still to be scanned by a second one (waiting for https://b.example)"),
    "Blocks 1,200 to 1,260 were read from one endpoint’s logs only; a second log endpoint still has to scan them (waiting for https://b.example). Until it does, their addresses stay watched.",
  )
  assert.equal(workInWords("a re-scan is under way: block 870 of 900"), "Scanning the chain again: at block 870 of 900.")
  assert.equal(workInWords("1 transfer(s) are waiting to be confirmed by another endpoint"), "1 transfer seen by one endpoint is waiting for another endpoint to confirm it.")
  for (const routine of ["3 address(es) are watched", "2 reported transfer(s) are still inside the reorg window", "the processed head is waiting: x", "the last tick failed", ""]) {
    assert.equal(workInWords(routine), undefined, routine)
  }
})

test("not syncing says no usable endpoint only when none can be used", () => {
  const behind = { ...healthy, healthy: false, endpoints: [{ url: "https://a.example", state: "healthy" }, { url: "https://b.example", state: "healthy" }] } as unknown as NodeStatus
  assert.deepEqual(nodeHealth(behind), { tone: "danger", label: "Not syncing", detail: "Behind the chain", live: false })
  const one = { ...behind, endpoints: [{ url: "https://a.example", state: "healthy" }, { url: "https://b.example", state: "failing" }] } as unknown as NodeStatus
  assert.equal(nodeHealth(one).detail, "1 endpoint can’t be used")
  const none = { ...behind, endpoints: [{ url: "https://a.example", state: "failing" }, { url: "https://b.example", state: "failing" }] } as unknown as NodeStatus
  assert.equal(nodeHealth(none).detail, "No usable RPC endpoint")
  assert.equal(nodeHealth({ ...healthy, hold: "settling again the blocks below where the head was (block 9; the cursor is at block 2): x" }).detail, "Settling blocks again")
})
