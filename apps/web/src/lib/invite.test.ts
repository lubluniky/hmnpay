import assert from "node:assert/strict"
import { test } from "node:test"

import { inviteAt, UNWATCHED_AFTER_MS } from "./invite.ts"

const deadline = "2026-11-18T12:30:00Z"
const T = Date.parse(deadline)
const s = 1000

test("before the deadline the page asks for the payment, and says when it is not watching", () => {
  assert.equal(inviteAt(deadline, T - 600 * s, T - 599 * s), "open")
  assert.equal(inviteAt(deadline, T - 20 * s, T - 5 * s), "open")
  // The stream's keep-alive comes every 15 seconds: that much silence is normal.
  assert.equal(inviteAt(deadline, T - 100 * s, T - 100 * s + UNWATCHED_AFTER_MS), "open")
  assert.equal(inviteAt(deadline, T - 100 * s, T - 100 * s + UNWATCHED_AFTER_MS + 1), "unwatched")
  // Never heard from the server at all (the view on screen came from before a reload of state).
  assert.equal(inviteAt(deadline, undefined, T - 300 * s), "unwatched")
})

test("once the deadline has passed the page stops asking, whatever the connection", () => {
  // The server was asked after the deadline and still has the payment open: it is checking.
  assert.equal(inviteAt(deadline, T + 2 * s, T + 3 * s), "closing")
  assert.equal(inviteAt(deadline, T + 2 * s, T + 20 * s), "closing")
  // Its last word is from just before the deadline: the page is asking again right now.
  assert.equal(inviteAt(deadline, T - 1 * s, T), "closing")
  assert.equal(inviteAt(deadline, T - 10 * s, T + 5 * s), "closing")
  // The answer did not come: nobody has confirmed anything since the deadline.
  assert.equal(inviteAt(deadline, T - 1 * s, T + 9 * s), "unknown")
  assert.equal(inviteAt(deadline, T - 30 * s, T + 1 * s), "unknown") // it had gone quiet before the deadline already
  assert.equal(inviteAt(deadline, T - 30 * s, T + 94 * s), "unknown")
  assert.equal(inviteAt(deadline, undefined, T + 1), "unknown")
  // It answered after the deadline and then went quiet.
  assert.equal(inviteAt(deadline, T + 2 * s, T + 60 * s), "unknown")
  // In none of these is the payer still asked to send.
  for (const [contact, now] of [[T + 2 * s, T + 3 * s], [T - 1 * s, T], [undefined, T + 1]] as const) {
    assert.notEqual(inviteAt(deadline, contact, now), "open")
    assert.notEqual(inviteAt(deadline, contact, now), "unwatched")
  }
})

test("the deadline is the server's: a device clock that is off does not move it", () => {
  const behind = -600 * s // the device is ten minutes behind, so the server's clock is ten minutes ahead of it
  // By the device's clock there are nine minutes left; really the deadline passed a minute ago.
  const localNow = T - 540 * s
  assert.equal(inviteAt(deadline, localNow - 1 * s, localNow, 0), "open")
  assert.equal(inviteAt(deadline, localNow - 1 * s, localNow, -behind), "closing")
  assert.equal(inviteAt(deadline, localNow - 200 * s, localNow, -behind), "unknown")
  assert.equal(inviteAt(deadline, localNow - 1 * s, localNow + 30 * s, -behind), "unknown")
  // A device ten minutes ahead: its clock says the time is up, the server says nine minutes are left.
  const aheadNow = T + 60 * s
  assert.equal(inviteAt(deadline, aheadNow - 1 * s, aheadNow, -600 * s), "open")
})
