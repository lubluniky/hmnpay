import assert from "node:assert/strict"
import { test } from "node:test"

import { ViewOrder } from "./view-order.ts"

interface View {
  status: string
  version: number
  confirmations: number
}
const awaiting: View = { status: "awaiting", version: 1, confirmations: 0 }
const confirming = (n: number): View => ({ status: "confirming", version: 3, confirmations: n })
const paid: View = { status: "paid", version: 4, confirmations: 5 }

test("answers in the order they were asked for are all applied", () => {
  const order = new ViewOrder<View>()
  for (const view of [awaiting, confirming(1), confirming(2), paid]) {
    assert.equal(order.offer(order.begin("poll"), view), true)
    assert.equal(order.current, view)
  }
})

test("a poll answer that arrives after the stream moved on is dropped", () => {
  const order = new ViewOrder<View>()
  assert.equal(order.offer(order.begin("poll"), awaiting), true)
  // The page polls; the answer is held up. Meanwhile the stream connects and follows the payment to paid.
  const held = order.begin("poll")
  const stream = order.begin("stream")
  assert.equal(order.offer(stream, awaiting), true)
  assert.equal(order.offer(stream, confirming(1)), true)
  assert.equal(order.offer(stream, paid), true)
  // The held answer (truthful when it was produced: "awaiting") arrives now.
  assert.equal(order.offer(held, awaiting), false)
  assert.equal(order.current, paid)
})

test("the other order: a stream message that arrives after a newer poll answer is dropped", () => {
  const order = new ViewOrder<View>()
  assert.equal(order.offer(order.begin("poll"), awaiting), true)
  // A stream connection is under way; its first message (rendered while still awaiting) is slow.
  const stream = order.begin("stream")
  assert.equal(order.offer(order.begin("poll"), paid), true)
  assert.equal(order.offer(stream, awaiting), false)
  assert.equal(order.current, paid)
  // The stream then catches up: the same state is accepted, and from there it is in order again.
  assert.equal(order.offer(stream, paid), true)
  assert.equal(order.current, paid)
})

test("late but newer is applied: nothing that is ahead is ever thrown away from the stream", () => {
  const order = new ViewOrder<View>()
  assert.equal(order.offer(order.begin("poll"), awaiting), true)
  const stream = order.begin("stream")
  // A poll answer (still awaiting) is applied while the stream's first message is on its way…
  assert.equal(order.offer(order.begin("poll"), awaiting), true)
  // …and that message already says paid.
  assert.equal(order.offer(stream, paid), true)
  assert.equal(order.current, paid)
})

test("a late poll answer is dropped even when it happens to be newer; the next poll brings it", () => {
  const order = new ViewOrder<View>()
  assert.equal(order.offer(order.begin("poll"), awaiting), true)
  const slow = order.begin("poll")
  const stream = order.begin("stream")
  assert.equal(order.offer(stream, awaiting), true)
  assert.equal(order.offer(slow, paid), false) // its order is not known, so it is not trusted
  assert.equal(order.offer(order.begin("poll"), paid), true) // asked after everything applied: certain
  assert.equal(order.current, paid)
})

test("two polls in flight: only the one sent after the last applied view counts", () => {
  const order = new ViewOrder<View>()
  const first = order.begin("poll")
  const second = order.begin("poll")
  assert.equal(order.offer(second, paid), true)
  assert.equal(order.offer(first, awaiting), false)
  assert.equal(order.current, paid)
})

test("within one version the confirmation count never goes back because of a late message", () => {
  const order = new ViewOrder<View>()
  assert.equal(order.offer(order.begin("poll"), confirming(1)), true)
  const stream = order.begin("stream")
  assert.equal(order.offer(order.begin("poll"), confirming(3)), true)
  assert.equal(order.offer(stream, confirming(2)), false) // rendered before the poll's answer
  assert.equal(order.current?.confirmations, 3)
  assert.equal(order.offer(stream, confirming(3)), true)
  assert.equal(order.offer(stream, confirming(4)), true)
  // From the stream itself, in order, anything is taken: the server knows best.
  assert.equal(order.offer(stream, confirming(3)), true)
  assert.equal(order.current?.confirmations, 3)
})

test("a version that goes back is believed when it certainly is newer (a restored database)", () => {
  const order = new ViewOrder<View>()
  assert.equal(order.offer(order.begin("poll"), paid), true)
  // Asked after "paid" was applied, and nothing applied since: this is what the server says now.
  assert.equal(order.offer(order.begin("poll"), awaiting), true)
  assert.equal(order.current, awaiting)
  const stream = order.begin("stream")
  assert.equal(order.offer(stream, awaiting), true)
})

test("a new stream connection after a break continues in order", () => {
  const order = new ViewOrder<View>()
  const one = order.begin("stream")
  assert.equal(order.offer(one, awaiting), true)
  assert.equal(order.offer(one, confirming(1)), true)
  // The connection breaks; a poll runs; a second connection opens.
  assert.equal(order.offer(order.begin("poll"), confirming(2)), true)
  const two = order.begin("stream")
  assert.equal(order.offer(two, paid), true)
  // A straggler of the first connection (it cannot happen once its reader is gone, but if it did):
  assert.equal(order.offer(one, confirming(1)), false)
  assert.equal(order.current, paid)
})

test("every interleaving of a stale poll answer with a stream that reaches paid ends on paid", () => {
  const streamViews = [awaiting, confirming(1), confirming(3), paid]
  for (let at = 0; at <= streamViews.length; at++) {
    const order = new ViewOrder<View>()
    assert.equal(order.offer(order.begin("poll"), awaiting), true)
    const held = order.begin("poll")
    const stream = order.begin("stream")
    streamViews.forEach((view, i) => {
      if (i === at) order.offer(held, awaiting)
      order.offer(stream, view)
    })
    if (at === streamViews.length) order.offer(held, awaiting)
    assert.equal(order.current, paid, `stale answer delivered before stream message ${at}`)
  }
})
