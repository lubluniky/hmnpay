import { mkdirSync } from "node:fs"
import path from "node:path"

import { Api, Stack } from "./harness"
import { expect, expectAttribution, pane, prepare, signIn, test, verify } from "./fixtures"

/**
 * A payment's address is scanned for from the block the server had processed when it was
 * created, so before the chain watcher has processed its first block the server creates no
 * payment (503 chain_not_ready). Here it cannot reach the chain at all, from the start: the
 * operator is told what is going on and where to look; a customer on a payment link waits in a
 * calm state that tries again by itself; and once the chain is reachable both go on without a
 * reload.
 */
test("before the server has read the chain: the operator is told why, a payer waits, and both go on by themselves", async ({ browser }, info) => {
  test.setTimeout(300_000)
  const stack = await new Stack(66 + info.parallelIndex).start()
  const shots = process.env.HMNPAY_E2E_SHOTS_DIR
  const shoot = async (page: import("@playwright/test").Page, name: string) => {
    const file = shots ? path.join(shots, `${name}.png`) : info.outputPath(`${name}.png`)
    mkdirSync(path.dirname(file), { recursive: true })
    await page.screenshot({ path: file })
  }
  try {
    // Two endpoints, both relays that are down from the start: no head can ever be agreed on.
    const first = stack.relay(3)
    const second = stack.relay(4)
    first.down = true
    second.down = true
    const api = new Api(stack.url)
    await api.setup(stack, [first.url, second.url])
    const refused = await api.call<{ error: { code: string; message: string } }>("POST", "/payments", { amount: "25" })
    expect(refused.status).toBe(503)
    expect(refused.body.error.code).toBe("chain_not_ready")
    expect(refused.headers.get("retry-after")).toBe("5")
    // Payment links can be made already; only a checkout waits for the chain.
    await api.must(201, "POST", "/payment-links", { name: "Launch workshop seat", slug: "launch", amount: "75" })

    // The operator, in the dashboard.
    const operator = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: "dark" })
    await prepare(operator)
    await signIn(operator, { stack, api, url: stack.url })
    const page = await operator.newPage()
    await page.goto(stack.url + "/payments")
    await page.getByRole("button", { name: "Create payment" }).first().click()
    const dialog = page.getByRole("dialog")
    await dialog.getByLabel("Amount").fill("25")
    await dialog.getByLabel("Order ID").fill("ORD-EARLY")
    await dialog.getByRole("button", { name: "Create payment" }).click()
    const notice = dialog.locator("[data-slot=chain-not-ready]")
    await expect(notice).toBeVisible()
    await expect(notice).toContainText("The server has not finished reading the chain yet")
    await expect(notice).toContainText("Try again in a moment")
    await expect(notice).not.toContainText(/chain_not_ready|503/)
    await expect(notice.getByRole("link", { name: "Open Network" })).toHaveAttribute("href", "/settings/network")
    await shoot(page, "chain-not-ready-operator")

    // A customer on the payment link: no error, a wait that retries by itself.
    const customerContext = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: "light" })
    await prepare(customerContext, { theme: "light" })
    const customer = await customerContext.newPage()
    await customer.goto(stack.url + "/l/launch")
    await customer.getByRole("button", { name: /Pay/ }).click()
    const waiting = customer.locator("[data-slot=chain-not-ready]")
    await expect(waiting).toBeVisible()
    await expect(waiting).toContainText("Getting your payment ready")
    await expect(waiting).toContainText("tries again by itself")
    await expect(customer.getByRole("button", { name: "Getting your payment ready…" })).toBeDisabled()
    await expect(customer.locator("form").getByRole("alert")).toHaveCount(0) // (the router's own announcer is not ours)
    await expectAttribution(customer)
    await shoot(customer, "chain-not-ready-payer")
    // It keeps waiting calmly through several refusals: never an error, never a dead end.
    await customer.waitForTimeout(12_000)
    await expect(waiting).toBeVisible()
    await expect(customer.locator("form").getByRole("alert")).toHaveCount(0) // (the router's own announcer is not ours)

    // The chain becomes reachable: the customer's page goes on to the checkout by itself.
    first.down = false
    second.down = false
    await api.waitForNode(120_000)
    await customer.waitForURL(/\/pay\/pay_[0-9a-z]{26}$/, { timeout: 90_000 })
    await expect(pane(customer)).toHaveAttribute("data-pane", "awaiting")
    // One payment for one press of the button, however often the page asked.
    const list = await api.must<{ data: { id: string }[] }>(200, "GET", "/payments")
    expect(list.data).toHaveLength(1)

    // And the operator's same request now goes through.
    await dialog.getByRole("button", { name: "Create payment" }).click()
    await expect(dialog.getByText("Payment created")).toBeVisible()
    await expect(notice).toHaveCount(0)

    await verify(operator)
    await verify(customerContext)
    await operator.close()
    await customerContext.close()
  } finally {
    await stack.stop()
  }
})

/**
 * The server's clock behind the chain: a deadline would be set early, so no payment is created
 * (503 clock_behind) — and waiting does not end it. The operator is told the clock must be
 * corrected; a customer on a payment link waits calmly at first and, once that has gone on for a
 * while, is told the merchant cannot take payments right now, with the merchant's contact.
 */
test("the server clock behind the chain: the operator is told to correct it, a payer is told kindly to try later", async ({ browser }, info) => {
  test.setTimeout(300_000)
  const stack = await new Stack(68 + info.parallelIndex).start()
  const shots = process.env.HMNPAY_E2E_SHOTS_DIR
  const shoot = async (page: import("@playwright/test").Page, name: string) => {
    const file = shots ? path.join(shots, `${name}.png`) : info.outputPath(`${name}.png`)
    mkdirSync(path.dirname(file), { recursive: true })
    await page.screenshot({ path: file })
  }
  try {
    const api = new Api(stack.url)
    await api.setup(stack)
    await api.waitForNode()
    await api.must(201, "POST", "/payment-links", { name: "Launch workshop seat", slug: "launch", amount: "75" })
    // The chain moves an hour ahead of the server's clock (as if the server's clock were an hour behind).
    const latest = await stack.rpc<{ timestamp: string }>("eth_getBlockByNumber", "latest", false)
    await stack.rpc("evm_setNextBlockTimestamp", Number(latest.timestamp) + 3600)
    await stack.rpc("evm_mine")
    // (Until the watcher has seen the newer block, a payment is still created: those are not counted.)
    await expect
      .poll(async () => (await api.call<{ error?: { code: string } }>("POST", "/payments", { amount: "5" })).body.error?.code, { timeout: 90_000 })
      .toBe("clock_behind")
    const madeBefore = (await api.must<{ data: unknown[] }>(200, "GET", "/payments")).data.length
    const refused = await api.call("POST", "/payments", { amount: "5" })
    expect(refused.status).toBe(503)
    expect(refused.headers.get("retry-after")).toBe("60")

    // The operator, in the dashboard: what is wrong and that waiting does not fix it.
    const operator = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: "dark" })
    await prepare(operator)
    await signIn(operator, { stack, api, url: stack.url })
    const page = await operator.newPage()
    await page.goto(stack.url + "/payments")
    await page.getByRole("button", { name: "Create payment" }).first().click()
    const dialog = page.getByRole("dialog")
    await dialog.getByLabel("Amount").fill("25")
    await dialog.getByRole("button", { name: "Create payment" }).click()
    const notice = dialog.locator("[data-slot=clock-behind]")
    await expect(notice).toBeVisible()
    await expect(notice).toContainText("The server’s clock is behind")
    await expect(notice).toContainText("This does not go away by waiting")
    await expect(notice).not.toContainText(/clock_behind|503/)
    await expect(notice.getByRole("link", { name: "Open Network" })).toHaveAttribute("href", "/settings/network")
    await shoot(page, "clock-behind-operator")
    await page.goto(stack.url + "/settings/network")
    await expect(page.locator("[data-slot=node-status] [data-slot=callout]").filter({ hasText: "The server’s clock is behind the chain" })).toBeVisible({ timeout: 60_000 })
    await shoot(page, "clock-behind-network")

    // A customer on the payment link: a calm wait first, then a plain word and the merchant's contact.
    const customerContext = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: "light" })
    await prepare(customerContext, { theme: "light" })
    const customer = await customerContext.newPage()
    await customer.goto(stack.url + "/l/launch")
    await customer.getByRole("button", { name: /Pay/ }).click()
    await expect(customer.locator("[data-slot=chain-not-ready]")).toBeVisible()
    await expect(customer.locator("form").getByRole("alert")).toHaveCount(0) // (the router's own announcer is not ours)
    const paused = customer.locator("[data-slot=payments-paused]")
    await expect(paused).toBeVisible({ timeout: 70_000 })
    await expect(paused).toContainText("Payments are paused for the moment")
    await expect(paused).toContainText("Nothing has been charged")
    await expect(paused).not.toContainText(/clock|chain|error|503/i)
    await expect(paused.locator("[data-slot=contact-merchant]")).toHaveAttribute("href", /^mailto:/)
    await expect(customer.getByRole("button", { name: "Payments are paused" })).toBeDisabled()
    await expectAttribution(customer)
    await shoot(customer, "clock-behind-payer")
    // However often the page asked, no payment was made while the clock was behind.
    expect((await api.must<{ data: unknown[] }>(200, "GET", "/payments")).data).toHaveLength(madeBefore)

    await verify(operator)
    await verify(customerContext)
    await operator.close()
    await customerContext.close()
  } finally {
    await stack.stop()
  }
})

/**
 * Network settings an earlier version stored that the rules now refuse — two URLs of one endpoint
 * — keep the instance from watching the chain and from creating payments (503 network_invalid).
 * The operator is told what is wrong and where; the wrong entry is marked, and removing it starts
 * the watcher. A customer on a payment link meanwhile waits, then is told payments are paused.
 */
test("stored network settings that can't be used: the operator repairs them on the Network page, a payer is told payments are paused", async ({ browser }, info) => {
  test.setTimeout(300_000)
  const stack = await new Stack(62 + info.parallelIndex).start()
  const shots = process.env.HMNPAY_E2E_SHOTS_DIR
  const shoot = async (page: import("@playwright/test").Page, name: string) => {
    const file = shots ? path.join(shots, `${name}.png`) : info.outputPath(`${name}.png`)
    mkdirSync(path.dirname(file), { recursive: true })
    await page.screenshot({ path: file })
  }
  try {
    const api = new Api(stack.url)
    await api.setup(stack)
    await api.waitForNode()
    await api.must(201, "POST", "/payment-links", { name: "Launch workshop seat", slug: "launch", amount: "75" })

    // The server stops; its stored endpoints become the first one twice (as an earlier version
    // could save them), and it starts again.
    const internals = stack as unknown as { server: import("node:child_process").ChildProcess; startServer(): Promise<void> }
    internals.server.kill("SIGTERM")
    await new Promise((resolve) => internals.server.once("exit", resolve))
    const { DatabaseSync } = await import("node:sqlite")
    const db = new DatabaseSync(path.join(stack.dataDir, "hmnpay.db"))
    const row = db.prepare("SELECT network FROM settings").get() as { network: string }
    const network = JSON.parse(row.network) as Record<string, unknown>
    const key = Object.keys(network).find((k) => Array.isArray(network[k]) && (network[k] as unknown[]).includes(stack.rpcUrls[0]))!
    network[key] = [stack.rpcUrls[0], `${stack.rpcUrls[0]}/second-key`]
    db.prepare("UPDATE settings SET network = ?").run(JSON.stringify(network))
    db.close()
    await internals.startServer()
    await api.login()

    const refused = await api.call<{ error: { code: string } }>("POST", "/payments", { amount: "5" })
    expect(refused.status).toBe(503)
    expect(refused.body.error.code).toBe("network_invalid")
    expect(refused.headers.get("retry-after")).toBe("60")
    const node = await api.must<{ network_problem: string; healthy: boolean }>(200, "GET", "/node-status")
    expect(node.network_problem).toContain("same endpoint")
    const settings = await api.must<{ network: { problems: { field: string; code: string }[] } }>(200, "GET", "/settings")
    expect(settings.network.problems).toContainEqual(expect.objectContaining({ field: "rpc_urls.1", code: "same_endpoint" }))

    // A customer on the payment link: the calm wait, then "paused", with headline and button agreeing.
    const customerContext = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: "light" })
    await prepare(customerContext, { theme: "light" })
    const customer = await customerContext.newPage()
    await customer.goto(stack.url + "/l/launch")
    await customer.getByRole("button", { name: /Pay/ }).click()
    await expect(customer.locator("[data-slot=chain-not-ready]")).toBeVisible()
    const paused = customer.locator("[data-slot=payments-paused]")
    await expect(paused).toBeVisible({ timeout: 70_000 })
    await expect(paused.getByRole("heading")).toHaveText("Payments are paused for the moment")
    await expect(customer.getByRole("button", { name: "Payments are paused" })).toBeDisabled()
    await expect(customer.getByText("Ready when you are")).toHaveCount(0)
    await expect(paused).not.toContainText(/network|settings|endpoint|error|503/i)
    await shoot(customer, "network-invalid-payer")

    // The operator: the dialog, the Network page, the marked entry.
    const operator = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: "dark" })
    await prepare(operator)
    await signIn(operator, { stack, api, url: stack.url })
    const page = await operator.newPage()
    await page.goto(stack.url + "/payments")
    await page.getByRole("button", { name: "Create payment" }).first().click()
    const dialog = page.getByRole("dialog")
    await dialog.getByLabel("Amount").fill("25")
    await dialog.getByRole("button", { name: "Create payment" }).click()
    const notice = dialog.locator("[data-slot=network-invalid]")
    await expect(notice).toContainText("The network settings need repair")
    await expect(notice).toContainText("This does not go away by waiting")
    await expect(notice).not.toContainText(/network_invalid|503/)
    await shoot(page, "network-invalid-operator")
    await page.goto(stack.url + "/settings/network")
    const status = page.locator("[data-slot=node-status]")
    await expect(status.locator("[data-slot=callout]").filter({ hasText: "The network settings can’t be used" })).toBeVisible()
    await expect(page.locator("[data-node-state]")).toContainText("The network settings need repair")
    const marked = page.locator("[data-slot=rpc-endpoints] > li[data-problem=same_endpoint]")
    await expect(marked).toHaveCount(1)
    await expect(marked).toContainText("Same endpoint as #1")
    await shoot(page, "network-invalid-network")

    // The repair: remove the marked entry. The watcher starts, and payments work again.
    await marked.getByRole("button", { name: /Actions for/ }).click()
    await page.getByRole("menuitem", { name: "Remove" }).click()
    await expect(page.locator("[data-slot=rpc-endpoints] > li")).toHaveCount(1)
    await api.waitForNode(120_000)
    await expect(status.locator("[data-slot=callout]").filter({ hasText: "The network settings can’t be used" })).toHaveCount(0, { timeout: 30_000 })
    expect((await api.createPayment({ amount: "5", order_id: "ORD-REPAIRED" })).status).toBe("awaiting")
    // The customer's page, still trying, goes on to the checkout by itself.
    await customer.waitForURL(/\/pay\/pay_[0-9a-z]{26}$/, { timeout: 90_000 })

    await verify(operator)
    await verify(customerContext)
    await operator.close()
    await customerContext.close()
  } finally {
    await stack.stop()
  }
})
