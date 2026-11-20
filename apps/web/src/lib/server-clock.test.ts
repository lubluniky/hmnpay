import assert from "node:assert/strict"
import { test } from "node:test"

import { clockOffset } from "./server-clock.ts"

test("the difference to the server's clock is taken from the Date header", () => {
  const local = Date.parse("2026-11-18T12:00:00.000Z")
  // In step, within the header's resolution: no correction.
  assert.equal(clockOffset("Wed, 18 Nov 2026 12:00:00 GMT", local + 400), 0)
  assert.equal(clockOffset("Wed, 18 Nov 2026 12:00:01 GMT", local), 0)
  // The device is ten minutes behind, or ahead.
  assert.equal(clockOffset("Wed, 18 Nov 2026 12:10:00 GMT", local), 600_500)
  assert.equal(clockOffset("Wed, 18 Nov 2026 11:50:00 GMT", local), -599_500)
  // Nothing usable: no opinion.
  assert.equal(clockOffset(null, local), null)
  assert.equal(clockOffset("", local), null)
  assert.equal(clockOffset("not a date", local), null)
})
