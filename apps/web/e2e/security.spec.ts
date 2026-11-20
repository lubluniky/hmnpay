import { OPERATOR } from "./harness"
import { expect, test } from "./fixtures"

/**
 * Regression tests for the web interface's defences: where a sign-in may send the browser, which
 * of two answers about a payment wins, what an expired checkout still shows, how amounts are
 * read, what the operator's session survives, and what the running product serves. Each test
 * describes an attack or a failure and asserts that it does not succeed.
 */

const EVIL = "evil.example"

/* ---------------------------------------------------------------------------------------------
 * /login?next= must not leave the site
 * ------------------------------------------------------------------------------------------- */

const OFF_SITE_NEXT: [string, string][] = [
  ["a tab", `/%09/${EVIL}/hmnpay-login`],
  ["a line feed", `/%0A/${EVIL}/hmnpay-login`],
  ["a carriage return", `/%0D/${EVIL}/hmnpay-login`],
  ["tab, LF and CR together", `/%09%0A%0D/${EVIL}/hmnpay-login`],
  ["two slashes", `//${EVIL}/hmnpay-login`],
  ["a backslash", `/%5C${EVIL}/hmnpay-login`],
  ["a full address", `https://${EVIL}/hmnpay-login`],
]

test("already signed in, /login?next= with a crafted target goes to the overview, never to another site", async ({ operatorPage: page, instance }) => {
  // Whatever would be asked of the other host is answered here, and counted.
  const offsite: string[] = []
  await page.context().route((url) => url.hostname === EVIL, (route) => {
    offsite.push(route.request().url())
    return route.fulfill({ status: 200, contentType: "text/html", body: "<title>attacker</title>attacker page" })
  })
  for (const [label, next] of OFF_SITE_NEXT) {
    await page.goto(`${instance.url}/login?next=${next}`)
    await page.waitForURL(`${instance.url}/`, { timeout: 20_000 })
    await expect(page.getByText("Recent payments"), label).toBeVisible()
    expect(offsite, label).toEqual([])
  }
  // A real path of this site is still honoured.
  await page.goto(`${instance.url}/login?next=${encodeURIComponent("/settings/network?add=1")}`)
  await page.waitForURL(`${instance.url}/settings/network?add=1`, { timeout: 20_000 })
})

test("signing in through the form with a crafted next= lands on this site", async ({ customer, instance }) => {
  for (const next of [`/%09/${EVIL}/session-expired`, `/%0A/${EVIL}/session-expired`]) {
    const page = await customer()
    const offsite: string[] = []
    await page.context().route((url) => url.hostname === EVIL, (route) => {
      offsite.push(route.request().url())
      return route.fulfill({ status: 200, contentType: "text/html", body: "attacker page" })
    })
    await page.goto(`${instance.url}/login?next=${next}`)
    await page.getByLabel("Email").fill(OPERATOR.email)
    await page.getByLabel("Password", { exact: true }).fill(OPERATOR.password)
    await page.getByRole("button", { name: "Sign in" }).click()
    await page.waitForURL(`${instance.url}/`, { timeout: 30_000 })
    await expect(page.getByText("Recent payments")).toBeVisible()
    expect(offsite).toEqual([])
  }
})

/* ---------------------------------------------------------------------------------------------
 * An older view never replaces a newer one, whichever way the answers are delayed
 * ------------------------------------------------------------------------------------------- */

/** Records every status the checkout shows, in order, so a test can tell that it never went back. */
async function recordStatuses(page: import("@playwright/test").Page) {
  await page.addInitScript(() => {
    const seen: string[] = []
    ;(window as unknown as { __statuses: string[] }).__statuses = seen
    const note = () => {
      const status = document.querySelector("[data-checkout]")?.getAttribute("data-status")
      if (status && seen[seen.length - 1] !== status) seen.push(status)
    }
    new MutationObserver(note).observe(document, { subtree: true, attributes: true, childList: true, attributeFilter: ["data-status"] })
    note()
  })
  return () => page.evaluate(() => (window as unknown as { __statuses: string[] }).__statuses.slice())
}

/** After "paid" nothing else may be shown, least of all a request for money. */
function expectNeverBack(statuses: string[]) {
  const paidAt = statuses.indexOf("paid")
  expect(paidAt, `the checkout reached paid (${statuses.join(" → ")})`).toBeGreaterThanOrEqual(0)
  expect(statuses.slice(paidAt), `statuses shown: ${statuses.join(" → ")}`).toEqual(["paid"])
}

const sse = (view: { version: number }) => `retry: 3000\n\nid: ${view.version}\nevent: checkout\ndata: ${JSON.stringify(view)}\n\n`

test("a poll answer that arrives late does not put a paid checkout back to 'send your payment'", async ({ customer, instance }) => {
  const { api, stack, url } = instance
  const payment = await api.createPayment({ amount: "25.00", order_id: "ORD-STALE-POLL" })
  const page = await customer()
  const statuses = await recordStatuses(page)

  // The first stream attempt breaks (a network blip): the page falls back to polling.
  let streams = 0
  await page.route(`**/api/v1/public/payments/${payment.id}/stream`, (route) => (++streams === 1 ? route.abort("connectionreset") : route.continue()))
  // The first poll after that gets its truthful answer ("awaiting") from the server at once, but
  // the answer reaches the page late.
  let reads = 0
  let release!: () => void
  const held = new Promise<void>((resolve) => (release = resolve))
  let heldStatus = ""
  await page.route(`**/api/v1/public/payments/${payment.id}`, async (route) => {
    if (++reads !== 2) return route.continue()
    const response = await route.fetch()
    const body = await response.text()
    heldStatus = JSON.parse(body).status
    await held
    await route.fulfill({ response, body }).catch(() => {}) // the page may have called the request off
  })

  await page.goto(`${url}/pay/${payment.id}`)
  const root = page.locator("[data-checkout]")
  await expect(root).toHaveAttribute("data-status", "awaiting")
  await expect(root).toHaveAttribute("data-live", "stream", { timeout: 30_000 })
  expect(reads).toBe(2)

  await stack.pay(payment.address, "25.00")
  await expect(root).toHaveAttribute("data-status", "paid", { timeout: 90_000 })
  await expect(page.getByText("Payment complete")).toBeVisible()

  // The delayed answer arrives now.
  expect(heldStatus).toBe("awaiting")
  release()
  await page.waitForTimeout(8000)
  await expect(root).toHaveAttribute("data-status", "paid")
  await expect(page.getByText("Payment complete")).toBeVisible()
  await expect(page.getByText("Send from your wallet")).toHaveCount(0)
  await expect(page.locator('[data-slot="address-groups"]')).toHaveCount(0)
  expectNeverBack(await statuses())
})

test("the other order, a stream message that arrives late is not put over a newer poll answer; a late one that is newer is taken", async ({ customer, instance }) => {
  const { api, stack, url } = instance
  const payment = await api.createPayment({ amount: "31.00", order_id: "ORD-STALE-STREAM" })
  const view = () => api.must<{ status: string; version: number }>(200, "GET", `/public/payments/${payment.id}`)
  const early = await view()
  expect(early.status).toBe("awaiting")

  /* A. The stream's message is the stale one. */
  {
    const page = await customer()
    const statuses = await recordStatuses(page)
    // First attempt: broken. Second: the server's answer (rendered while the payment was still
    // awaiting) is held on the way. Later ones go through.
    let streams = 0
    let release!: () => void
    const held = new Promise<void>((resolve) => (release = resolve))
    await page.route(`**/api/v1/public/payments/${payment.id}/stream`, async (route) => {
      streams += 1
      if (streams === 1) return route.abort("connectionreset")
      if (streams !== 2) return route.continue()
      await held
      await route.fulfill({ status: 200, contentType: "text/event-stream; charset=utf-8", headers: { "cache-control": "no-store" }, body: sse(early) })
    })
    await page.goto(`${url}/pay/${payment.id}`)
    const root = page.locator("[data-checkout]")
    await expect(root).toHaveAttribute("data-status", "awaiting")
    await expect.poll(() => streams, { timeout: 30_000 }).toBeGreaterThanOrEqual(2)

    // Meanwhile the customer pays, and polling follows the payment to paid.
    await stack.pay(payment.address, "31.00")
    await expect(root).toHaveAttribute("data-status", "paid", { timeout: 90_000 })
    await expect(root).toHaveAttribute("data-live", "polling")
    release()
    await page.waitForTimeout(6000)
    await expect(root).toHaveAttribute("data-status", "paid")
    await expect(page.getByText("Send from your wallet")).toHaveCount(0)
    expectNeverBack(await statuses())
  }

  /* B. The poll answers are the stale ones, and the stream's late message is newer: it is shown. */
  {
    const now = await view()
    expect(now.status).toBe("paid")
    expect(now.version).toBeGreaterThan(early.version)
    const page = await customer()
    let stale = true
    let streams = 0
    let release!: () => void
    const held = new Promise<void>((resolve) => (release = resolve))
    // Every plain read is answered with the old view (a cache in the way, say) while `stale`.
    await page.route(`**/api/v1/public/payments/${payment.id}`, (route) =>
      stale ? route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(early) }) : route.continue(),
    )
    await page.route(`**/api/v1/public/payments/${payment.id}/stream`, async (route) => {
      streams += 1
      if (streams === 1) return route.abort("connectionreset")
      if (streams !== 2) return route.continue()
      await held
      await route.fulfill({ status: 200, contentType: "text/event-stream; charset=utf-8", headers: { "cache-control": "no-store" }, body: sse(now) })
    })
    await page.goto(`${url}/pay/${payment.id}`)
    const root = page.locator("[data-checkout]")
    await expect(root).toHaveAttribute("data-status", "awaiting")
    await expect.poll(() => streams, { timeout: 30_000 }).toBeGreaterThanOrEqual(2)
    // Polls (answered "awaiting") have been applied since the stream was asked for.
    await page.waitForTimeout(5000)
    await expect(root).toHaveAttribute("data-status", "awaiting")
    stale = false
    release()
    await expect(root).toHaveAttribute("data-status", "paid", { timeout: 10_000 })
    await page.waitForTimeout(4000)
    await expect(root).toHaveAttribute("data-status", "paid")
  }
})

/* ---------------------------------------------------------------------------------------------
 * Past its deadline the checkout stops asking for money, with or without the server
 * ------------------------------------------------------------------------------------------- */

test("at 00:00 the checkout withdraws address and QR, says what it knows, and follows the server when it can", async ({ customer, instance }) => {
  test.setTimeout(300_000)
  const { api, url } = instance
  const offline = await api.createPayment({ amount: "40.00", order_id: "ORD-DEADLINE-OFFLINE", ttl_seconds: 60 })
  const online = await api.createPayment({ amount: "41.00", order_id: "ORD-DEADLINE-ONLINE", ttl_seconds: 60 })
  const quiet = await api.createPayment({ amount: "42.00", order_id: "ORD-DEADLINE-LATER" }) // the usual half hour
  const expires = Date.parse((await api.payment(offline.id)).expires_at as string)

  // One payer whose connection drops before the deadline…
  const lost = await customer()
  let down = false
  await lost.route("**/api/v1/public/payments/*/stream", (route) => route.abort("connectionreset"))
  await lost.route(`**/api/v1/public/payments/${offline.id}`, (route) => (down ? route.abort("internetdisconnected") : route.continue()))
  await lost.goto(`${url}/pay/${offline.id}`)
  // …one who stays connected…
  const watching = await customer()
  await watching.goto(`${url}/pay/${online.id}`)
  // …and one with plenty of time whose connection drops for a long while.
  const cut = await customer()
  let cutDown = false
  await cut.route("**/api/v1/public/payments/*/stream", (route) => route.abort("connectionreset"))
  await cut.route(`**/api/v1/public/payments/${quiet.id}`, (route) => (cutDown ? route.abort("internetdisconnected") : route.continue()))
  await cut.goto(`${url}/pay/${quiet.id}`)

  for (const page of [lost, watching, cut]) {
    await expect(page.locator("[data-checkout]")).toHaveAttribute("data-status", "awaiting")
    await expect(page.locator("[data-pane]").last()).toHaveAttribute("data-invite", "open")
    await expect(page.locator('[data-slot="address-groups"]').first()).toBeVisible()
  }
  await expect(lost.locator("[data-checkout]")).toHaveAttribute("data-live", "polling", { timeout: 20_000 })
  down = true
  cutDown = true

  // Long before its deadline, the third page says that it is not watching, and keeps the address.
  await expect(cut.locator("[data-slot=live-unavailable]")).toBeVisible({ timeout: 80_000 })
  await expect(cut.locator("[data-slot=live-unavailable]")).toContainText("Live status is unavailable")
  await expect(cut.locator("[data-pane]").last()).toHaveAttribute("data-invite", "unwatched")
  await expect(cut.locator('[data-slot="address-groups"]').first()).toBeVisible()
  await expect(cut.getByRole("img", { name: "QR code of the payment address" }).first()).toBeVisible()
  await expect(cut.getByText("Watching for your payment")).toHaveCount(0)

  // Past the deadline by the server's clock.
  await lost.waitForTimeout(Math.max(0, expires - Date.now()) + 12_000)
  expect((await api.payment(offline.id)).status).toBe("awaiting") // the server's minute of grace is still running

  // Without the server: no address, no QR, nothing to copy, and plain words.
  const closedOffline = lost.locator("[data-slot=deadline-passed]")
  await expect(closedOffline).toBeVisible()
  await expect(closedOffline).toHaveAttribute("data-reason", "unreachable")
  await expect(closedOffline).toContainText("This invoice has probably expired")
  await expect(closedOffline).toContainText("Do not send anything now")
  await expect(closedOffline).toContainText("it will update when it reconnects")
  // With the server: the same withdrawal, and that the server is checking.
  const closedOnline = watching.locator("[data-slot=deadline-passed]")
  await expect(closedOnline).toBeVisible()
  await expect(closedOnline).toHaveAttribute("data-reason", "checking")
  await expect(closedOnline).toContainText("The time to pay is up")
  await expect(closedOnline).toContainText("Do not send anything now")
  for (const page of [lost, watching]) {
    await expect(page.locator('[data-slot="address-groups"]')).toHaveCount(0)
    await expect(page.getByRole("img", { name: "QR code of the payment address" })).toHaveCount(0)
    await expect(page.getByText("Send from your wallet")).toHaveCount(0)
    await expect(page.getByText("Watching for your payment")).toHaveCount(0)
    await expect(page.getByRole("button", { name: /copy/i })).toHaveCount(0)
    await expect(page.getByText(/your payment is not affected/)).toHaveCount(0)
  }

  // The server settles the payments as expired; the connected page follows, the other one as
  // soon as its connection is back.
  await api.waitForStatus(offline.id, ["expired"], 180_000)
  await expect(watching.locator("[data-checkout]")).toHaveAttribute("data-status", "expired", { timeout: 60_000 })
  await expect(lost.locator("[data-slot=deadline-passed]")).toHaveAttribute("data-reason", "unreachable")
  down = false
  await expect(lost.locator("[data-checkout]")).toHaveAttribute("data-status", "expired", { timeout: 60_000 })
  await expect(lost.locator("[data-slot=deadline-passed]")).toHaveCount(0)
  // And the page that had lost the server long before its deadline is watching again.
  cutDown = false
  await expect(cut.locator("[data-slot=live-unavailable]")).toHaveCount(0, { timeout: 60_000 })
  await expect(cut.getByText("Watching for your payment")).toBeVisible()
  await expect(cut.locator('[data-slot="address-groups"]').first()).toBeVisible()
  await api.must(200, "POST", `/payments/${quiet.id}/cancel`)
})

/* ---------------------------------------------------------------------------------------------
 * An amount is digits with at most one dot; nothing is guessed
 * ------------------------------------------------------------------------------------------- */

test("'1,250' on an open-amount link is refused, not turned into 1.25; the button states the exact amount", async ({ customer, instance }) => {
  const { api, url } = instance
  const slug = `w04-${Date.now().toString(36)}`
  const link = await api.must<{ id: string }>(201, "POST", "/payment-links", { name: "Top up your balance", slug, amount: null })
  const page = await customer()
  await page.goto(`${url}/l/${slug}`)
  const field = page.getByLabel("Amount")
  await expect(page.getByText("Digits, with a dot for decimals: 1250 or 1250.50.")).toBeVisible()

  for (const typed of ["1,250", "1,250.50", "12,5", "1 250"]) {
    await field.fill("")
    await field.pressSequentially(typed)
    // The field keeps what was typed; the page says what is wrong with it.
    await expect(field).toHaveValue(typed)
    await expect(page.getByRole("alert").filter({ hasText: "with no commas or spaces" })).toBeVisible()
    // Nothing is offered to pay, and asking anyway opens nothing.
    await expect(page.locator("[data-slot=pay-exact]")).toHaveCount(0)
    await page.getByRole("button", { name: "Continue to payment" }).click()
    await expect(page).toHaveURL(new RegExp(`/l/${slug}$`))
  }
  const made = await api.must<{ page: { total: number } }>(200, "GET", `/payments?link_id=${link.id}`)
  expect(made.page.total).toBe(0)

  // What will be asked for is on the button before anything exists.
  await field.fill("1.25")
  await expect(page.locator("[data-slot=pay-exact]")).toHaveText("Pay 1.25 USDT")
  await field.fill("1250")
  await expect(page.locator("[data-slot=pay-exact]")).toHaveText("Pay 1250.00 USDT")
  await page.getByRole("button", { name: "Pay 1250.00 USDT" }).click()
  await page.waitForURL(/\/pay\/pay_/)
  const created = await api.must<{ amount: string }>(200, "GET", `/public/payments/${page.url().split("/").pop()!}`)
  expect(created.amount).toBe("1250.00")
})

test("the dashboard's amount fields read amounts the same way", async ({ operatorPage: page, instance }) => {
  const { api, url } = instance
  const before = (await api.must<{ page: { total: number } }>(200, "GET", "/payments?limit=1")).page.total

  // Create payment.
  await page.goto(`${url}/payments?new=1`)
  const create = page.getByRole("dialog")
  await create.getByLabel("Amount").pressSequentially("1,250")
  await expect(create.getByLabel("Amount")).toHaveValue("1,250")
  await expect(create.getByRole("alert").filter({ hasText: "with no commas or spaces" })).toBeVisible()
  await create.getByRole("button", { name: /Create payment/ }).click()
  await expect(create.getByRole("alert").filter({ hasText: "with no commas or spaces" })).toBeVisible()
  expect((await api.must<{ page: { total: number } }>(200, "GET", "/payments?limit=1")).page.total).toBe(before)
  await page.keyboard.press("Escape")

  // Link price and minimum.
  await page.goto(`${url}/payment-links?new=1`)
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Name").fill("Comma amount")
  await dialog.getByLabel("Amount").pressSequentially("49,99")
  await expect(dialog.getByLabel("Amount")).toHaveValue("49,99")
  await expect(dialog.getByRole("alert").filter({ hasText: "with no commas or spaces" })).toBeVisible()
  await dialog.getByRole("button", { name: "Create link" }).click()
  await expect(dialog.getByText("Link created")).toHaveCount(0)
  await dialog.getByRole("tab", { name: "Customer chooses" }).click()
  await dialog.getByLabel("Minimum amount").pressSequentially("5,5")
  await expect(dialog.getByRole("alert").filter({ hasText: "with no commas or spaces" })).toBeVisible()
  await dialog.getByRole("button", { name: "Create link" }).click()
  await expect(dialog.getByText("Link created")).toHaveCount(0)
  const links = await api.must<{ data: { name: string }[] }>(200, "GET", "/payment-links")
  expect(links.data.map((link) => link.name)).not.toContain("Comma amount")
  await page.keyboard.press("Escape")

  // Underpayment tolerance.
  await page.goto(`${url}/settings`)
  const defaults = page.locator("[data-slot=settings-section]").filter({ hasText: "Payment defaults" })
  const tolerance = defaults.getByLabel(/Underpayment tolerance/)
  await tolerance.fill("")
  await tolerance.pressSequentially("0,5")
  await expect(tolerance).toHaveValue("0,5")
  await expect(defaults.locator("[data-slot=tolerance-problem]")).toContainText("with no commas or spaces")
  await expect(defaults.getByRole("button", { name: "Save" })).toBeDisabled()
  expect((await api.must<{ payments: { underpayment_tolerance: string } }>(200, "GET", "/settings")).payments.underpayment_tolerance).toBe("0")
})

/* ---------------------------------------------------------------------------------------------
 * Three suspected weaknesses, each checked: a late answer after switching payments, a note typed
 * for another payment, signing out while the server cannot be reached
 * ------------------------------------------------------------------------------------------- */

/** From payment B's page to payment A's and straight back to B, without a page load in between. */
async function openAThenBackToB(page: import("@playwright/test").Page, url: string, a: { id: string }, b: { id: string }, orderA: string) {
  await page.goto(`${url}/payments/${b.id}`)
  await page.getByRole("link", { name: "Payments", exact: true }).first().click()
  await page.getByRole("link", { name: orderA }).first().click()
  await page.waitForURL(`${url}/payments/${a.id}`)
}

test("an action answered after moving on to another payment does not put its payment on that page", async ({ operatorPage: page, instance }) => {
  const { api, url } = instance
  const a = await api.createPayment({ amount: "11", order_id: "ORD-MOVED-A" })
  const b = await api.createPayment({ amount: "22", order_id: "ORD-MOVED-B" })
  await openAThenBackToB(page, url, a, b, "ORD-MOVED-A")
  await expect(page.getByRole("heading", { name: /ORD-MOVED-A/ }).first()).toBeVisible()

  // The cancellation is carried out at once; its answer is held on the way back.
  let release!: () => void
  const held = new Promise<void>((resolve) => (release = resolve))
  await page.route(`**/api/v1/payments/${a.id}/cancel`, async (route) => {
    const response = await route.fetch()
    await held
    await route.fulfill({ response }).catch(() => {})
  })
  await page.getByRole("button", { name: "Cancel payment" }).click()
  await page.getByRole("alertdialog").getByRole("button", { name: "Cancel payment" }).click()
  await expect.poll(async () => (await api.payment(a.id)).status).toBe("expired")

  // Meanwhile the operator goes straight to the other payment (two steps back in history, one navigation).
  await page.evaluate(() => window.history.go(-2))
  await page.waitForURL(`${url}/payments/${b.id}`)
  await expect(page.getByRole("heading", { name: /ORD-MOVED-B/ }).first()).toBeVisible()
  // From here on only the late answer could change what the page shows.
  await page.route(`**/api/v1/payments/${b.id}/detail`, (route) => route.abort())
  release()
  await page.waitForTimeout(2500)
  await expect(page.getByRole("heading", { name: /ORD-MOVED-B/ }).first()).toBeVisible()
  await expect(page.getByText("ORD-MOVED-A")).toHaveCount(0)
  await expect(page.getByText("Cancelled", { exact: true })).toHaveCount(0)
  await expect(page.getByRole("button", { name: "Cancel payment" })).toBeVisible()
  await page.unroute(`**/api/v1/payments/${b.id}/detail`)
  await api.must(200, "POST", `/payments/${b.id}/cancel`)
})

test("a resolution note typed for one payment is not left in the field of another", async ({ operatorPage: page, instance }) => {
  const { api, stack, url } = instance
  const a = await api.createPayment({ amount: "10", order_id: "ORD-NOTE-A" })
  const b = await api.createPayment({ amount: "10", order_id: "ORD-NOTE-B" })
  await stack.pay(a.address, "11")
  await stack.pay(b.address, "12")
  await api.waitForStatus(a.id, ["overpaid"])
  await api.waitForStatus(b.id, ["overpaid"])

  await openAThenBackToB(page, url, a, b, "ORD-NOTE-A")
  await page.getByLabel("Resolution note").fill("Refund 1 USDT to the sender of ORD-NOTE-A")
  await page.evaluate(() => window.history.go(-2))
  await page.waitForURL(`${url}/payments/${b.id}`)
  await expect(page.getByText(/Over by\s*2\.00\s*USDT/)).toBeVisible()
  await expect(page.getByLabel("Resolution note")).toHaveValue("")
  // And nothing of it reaches the other payment when that one is resolved.
  await page.getByRole("button", { name: "Mark as resolved" }).click()
  await expect(page.getByText("Resolved", { exact: true })).toBeVisible()
  const resolved = await api.must<{ resolution: { note: string } }>(200, "GET", `/payments/${b.id}`)
  expect(resolved.resolution.note).toBe("")
})

test("signing out says so when the server did not hear it, and the session is not mistaken for ended", async ({ customer, instance }) => {
  const page = await customer()
  await page.goto(`${instance.url}/login`)
  await page.getByLabel("Email").fill(OPERATOR.email)
  await page.getByLabel("Password", { exact: true }).fill(OPERATOR.password)
  await page.getByRole("button", { name: "Sign in" }).click()
  await page.waitForURL(`${instance.url}/`)
  const signedIn = () => page.evaluate(async () => (await fetch("/api/v1/auth/me", { credentials: "same-origin" })).status)
  expect(await signedIn()).toBe(200)

  // The request to end the session does not reach the server.
  await page.route("**/api/v1/auth/logout", (route) => route.abort("internetdisconnected"))
  await page.getByRole("button", { name: /owner@example\.com/ }).click()
  await page.getByRole("menuitem", { name: "Log out" }).click()
  // The operator is told, stays where they are, and is in fact still signed in.
  await expect(page.getByText("Couldn’t sign out")).toBeVisible()
  await expect(page.getByText(/still signed in/)).toBeVisible()
  await page.waitForTimeout(1500)
  await expect(page).toHaveURL(`${instance.url}/`)
  expect(await signedIn()).toBe(200)

  // With the server reachable it works, and the session is gone.
  await page.unroute("**/api/v1/auth/logout")
  await page.getByRole("button", { name: /owner@example\.com/ }).click()
  await page.getByRole("menuitem", { name: "Log out" }).click()
  await page.waitForURL(/\/login$/)
  expect(await signedIn()).toBe(401)
})

/* ---------------------------------------------------------------------------------------------
 * What ships with the product
 * ------------------------------------------------------------------------------------------- */

test("the running product serves the notices of the third-party software in it, linked from About", async ({ operatorPage: page, instance }) => {
  await page.goto(`${instance.url}/settings`)
  const link = page.locator("a[data-slot=third-party-notices]")
  await expect(link).toHaveText("Third-party notices")
  await expect(link).toHaveAttribute("href", "/third-party-notices.txt")

  // Open to anybody who has the product, signed in or not: a plain text file.
  const response = await fetch(`${instance.url}/third-party-notices.txt`)
  expect(response.status).toBe(200)
  expect(response.headers.get("content-type")).toBe("text/plain; charset=utf-8")
  expect(response.headers.get("x-content-type-options")).toBe("nosniff")
  expect(response.headers.get("content-security-policy")).toContain("default-src 'none'")
  const text = await response.text()
  for (const wanted of ["SIL OPEN FONT LICENSE", "The Geist Project Authors", "Jakub Antalik", "shadcn", "MIT License", "The Go Authors", "modernc.org/sqlite", "- next ", "react-dom"]) {
    expect(text, wanted).toContain(wanted)
  }
  // The link opens it.
  const [opened] = await Promise.all([page.context().waitForEvent("page"), link.click()])
  await opened.waitForLoadState()
  expect(opened.url()).toBe(`${instance.url}/third-party-notices.txt`)
  await expect(opened.locator("body")).toContainText("Third-party notices")
  await opened.close()
})

test("the running product serves its licence and the additional terms, named and linked from About", async ({ operatorPage: page, instance }) => {
  await page.goto(`${instance.url}/settings`)
  const about = page.locator("[data-slot=about-license]")
  await expect(about).toHaveText(/GNU GPL 3\.0 or later\s*with\s*additional terms/)
  for (const [slot, path, marks] of [
    ["license", "/license.txt", ["GNU GENERAL PUBLIC LICENSE", "Version 3, 29 June 2007"]],
    ["additional-terms", "/additional-terms.txt", ["section 7(b)", "Powered by hmnpay", "THIRD_PARTY_NOTICES.md"]],
  ] as const) {
    const link = about.locator(`a[data-slot=${slot}]`)
    await expect(link).toHaveAttribute("href", path)
    // Open to anybody who has the product, signed in or not: a plain text file.
    const response = await fetch(instance.url + path)
    expect(response.status, path).toBe(200)
    expect(response.headers.get("content-type")).toBe("text/plain; charset=utf-8")
    expect(response.headers.get("x-content-type-options")).toBe("nosniff")
    expect(response.headers.get("content-security-policy")).toContain("default-src 'none'")
    const text = await response.text()
    for (const mark of marks) expect(text, mark).toContain(mark)
    const [opened] = await Promise.all([page.context().waitForEvent("page"), link.click()])
    await opened.waitForLoadState()
    expect(opened.url()).toBe(instance.url + path)
    await opened.close()
  }
})

test("a page asked for under a host name the server does not answer for is refused, like the API", async ({ instance }) => {
  // (fetch cannot set Host; a raw request can.)
  const http = await import("node:http")
  const get = (path: string, host: string) =>
    new Promise<{ status: number; body: string }>((resolve, reject) => {
      const { hostname, port } = new URL(instance.url)
      const request = http.request({ hostname, port, path, method: "GET", headers: { Host: host } }, (response) => {
        let body = ""
        response.on("data", (chunk) => (body += chunk))
        response.on("end", () => resolve({ status: response.statusCode ?? 0, body }))
      })
      request.on("error", reject)
      request.end()
    })
  for (const path of ["/", "/login", "/pay/pay_00000000000000000000000000", "/third-party-notices.txt", "/license.txt", "/additional-terms.txt", "/api/v1/setup"]) {
    const refused = await get(path, "rebind.attacker.example")
    expect(refused.status, path).toBe(421)
    expect(refused.body, path).toContain("host_not_allowed")
  }
  expect((await get("/login", new URL(instance.url).host)).status).toBe(200)
})
