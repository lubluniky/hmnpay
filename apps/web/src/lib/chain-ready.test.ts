import assert from "node:assert/strict"
import { test } from "node:test"

import { CHAIN_NOT_READY, CHAIN_NOT_READY_OPERATOR, CHAIN_NOT_READY_PAYER, chainNotReadyDelay, CLOCK_BEHIND, CLOCK_BEHIND_OPERATOR, PAYER_PATIENCE_SECONDS, payerUnavailable } from "./chain-ready.ts"

test("the operator is told what chain_not_ready means and that it passes, in plain words", () => {
  assert.equal(CHAIN_NOT_READY, "chain_not_ready")
  assert.equal(CHAIN_NOT_READY_OPERATOR.short, "The server has not finished reading the chain yet. Try again in a moment.")
  assert.match(CHAIN_NOT_READY_OPERATOR.text, /Try again in a moment/)
  assert.match(CHAIN_NOT_READY_OPERATOR.text, /Network page/)
  for (const text of Object.values(CHAIN_NOT_READY_OPERATOR)) assert.doesNotMatch(text, /chain_not_ready|503|error/i)
})

test("a payer sees a calm wait, never an error or a dead end", () => {
  const all = Object.values(CHAIN_NOT_READY_PAYER).join(" ")
  assert.match(all, /tries again by itself/)
  assert.match(all, /nothing has been charged/)
  assert.doesNotMatch(all, /error|fail|chain_not_ready|503|RPC|block|try again later|contact/i)
})

test("the page asks again soon, then less often, and never sooner than the server says", () => {
  assert.deepEqual([1, 2, 3, 4, 5, 50].map((n) => chainNotReadyDelay(n)), [3, 6, 12, 20, 60, 60])
  assert.equal(chainNotReadyDelay(1, 2), 3)
  assert.equal(chainNotReadyDelay(1, 45), 45, "a Retry-After of a rate limit is respected")
  assert.equal(chainNotReadyDelay(0), 3)
  // A page left open never asks more often than the public checkout's per-address budget
  // allows (a burst of five, then one a minute): it is never told "too many requests".
  let t = 0
  let tokens = 5
  for (let n = 1; n <= 200; n++) {
    assert.ok(tokens >= 1, `request ${n} at ${t}s would be rate-limited`)
    tokens -= 1
    const wait = chainNotReadyDelay(n)
    tokens = Math.min(5, tokens + wait / 60)
    t += wait
  }
})

test("clock_behind: the operator is told the clock is wrong and that waiting does not fix it", () => {
  assert.equal(CLOCK_BEHIND, "clock_behind")
  assert.match(CLOCK_BEHIND_OPERATOR.text, /does not go away by waiting/)
  assert.match(CLOCK_BEHIND_OPERATOR.text, /Correct the server’s time/)
  assert.match(CLOCK_BEHIND_OPERATOR.short, /waiting does not fix it/)
  for (const text of Object.values(CLOCK_BEHIND_OPERATOR)) assert.doesNotMatch(text, /clock_behind|503|Retry-After/)
})

test("a payer who has waited a while is told plainly what to do, and that nothing was charged", () => {
  assert.equal(PAYER_PATIENCE_SECONDS, 45)
  const words = payerUnavailable("Northwind Digital")
  assert.equal(words.title, "Payments are paused for the moment")
  assert.equal(
    words.text,
    "Northwind Digital can’t take payments through this page right now. Please try again later, or contact Northwind Digital. Nothing has been charged; this page keeps trying in the meantime.",
  )
  assert.doesNotMatch(`${words.title} ${words.text}`, /error|clock|chain|RPC|503|code/i)
})
