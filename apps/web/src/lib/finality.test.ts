import assert from "node:assert/strict"
import { test } from "node:test"

import { FINALISING, FINALISING_PATIENCE_SECONDS, finalityShort, isFinalising, operatorFinalityLine, payerFinalising } from "./finality.ts"

test("a payment finalising is one with its confirmations waiting only for finality", () => {
  const wait = { block: 1200, final: 1180 }
  assert.equal(isFinalising({ status: "confirming", awaiting_finality: wait }), true)
  assert.equal(isFinalising({ status: "detected", awaiting_finality: wait }), true)
  assert.equal(isFinalising({ status: "confirming", awaiting_finality: null }), false)
  assert.equal(isFinalising({ status: "paid", awaiting_finality: null }), false)
})

test("the operator gets the block numbers", () => {
  assert.equal(operatorFinalityLine({ block: 48213077, final: 48212990 }), "Waiting for the chain to finalise block 48,213,077; finalised so far: 48,212,990.")
  assert.equal(FINALISING.label, "Finalising")
  // The final block can be above the processed head for a while; never "finalised so far 90 of 86".
  assert.equal(finalityShort({ block: 86, final: 90 }), false)
  assert.equal(operatorFinalityLine({ block: 86, final: 90 }), "The chain has finalised block 86; the payment is settled at the watcher's next pass.")
})

test("the payer gets plain words: received, nothing to do, safe — never numbers, jargon or an error", () => {
  const words = payerFinalising("Northwind Digital")
  assert.equal(words.title, "Payment received")
  assert.match(words.text, /nothing more for you to do/)
  assert.match(words.text, /You can close this page — Northwind Digital is told as soon as it is done/)
  assert.match(words.longer, /taking longer than usual/)
  assert.match(words.longer, /Your payment is safe/)
  assert.equal(FINALISING_PATIENCE_SECONDS, 120)
  for (const text of Object.values(words)) {
    assert.doesNotMatch(text, /final(ity|i[sz]ed)|block|confirmations? \d|\d+\s*\/\s*\d+|error|fail|stuck/i)
  }
})
