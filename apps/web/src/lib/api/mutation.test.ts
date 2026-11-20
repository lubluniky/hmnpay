import assert from "node:assert/strict"
import { test } from "node:test"

import { applyMutation, type State } from "./mutation.ts"

interface Detail {
  payment: { id: string; status: string }
}
const state = (key: string, data: Detail | undefined): State<Detail> => ({ key, data, error: undefined, loading: false })
const A: Detail = { payment: { id: "A", status: "awaiting" } }
const B: Detail = { payment: { id: "B", status: "awaiting" } }
const cancelledA = { id: "A", status: "expired" }
const withPayment = (payment: Detail["payment"]) => (current: Detail | undefined) => (current ? { ...current, payment } : current)

test("a mutation for the key on screen changes the screen and the cache", () => {
  const cache = new Map<string, unknown>([["payments/detail/A", A]])
  const next = applyMutation(cache, "payments/detail/A", state("payments/detail/A", A), withPayment(cancelledA))
  assert.equal(next.data?.payment.status, "expired")
  assert.equal((cache.get("payments/detail/A") as Detail).payment.status, "expired")
})

test("an answer for payment A that arrives while payment B is on screen does not touch B", () => {
  const cache = new Map<string, unknown>([
    ["payments/detail/A", A],
    ["payments/detail/B", B],
  ])
  const showingB = state("payments/detail/B", B)
  const next = applyMutation(cache, "payments/detail/A", showingB, withPayment(cancelledA))
  // B's screen and B's cache entry are exactly what they were…
  assert.equal(next, showingB)
  assert.deepEqual(cache.get("payments/detail/B"), B)
  // …and the answer is kept where it belongs, for when A is opened again.
  assert.equal((cache.get("payments/detail/A") as Detail).payment.status, "expired")
  assert.equal((cache.get("payments/detail/A") as Detail).payment.id, "A")
})

test("a plain value is stored under its own key as well", () => {
  const cache = new Map<string, unknown>()
  const showingB = state("payments/detail/B", B)
  assert.equal(applyMutation(cache, "payments/detail/A", showingB, A), showingB)
  assert.deepEqual(cache.get("payments/detail/A"), A)
  assert.equal(cache.has("payments/detail/B"), false)
})
