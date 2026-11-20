import type { Browser } from "@playwright/test"

import { Api, Stack, type Chain } from "./harness"
import { expect, prepare, signIn, test, verify, type Instance } from "./fixtures"

/**
 * These two tests change how the instance reaches the chain, so each runs on a stack of its own
 * instead of the worker's shared one.
 */

/** A signed-in operator page on a stack of its own. */
async function operator(browser: Browser, instance: Instance) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: "dark" })
  await prepare(context)
  await signIn(context, instance)
  return { context, page: await context.newPage() }
}

interface NodeBody {
  healthy: boolean
  idle: boolean
  hold: string
  warnings: string[]
  primary: string
  verifier: string
  head: { number: number } | null
}

test("a dead endpoint: the watcher warns, then holds confirmations; the dashboard says so, and it recovers", async ({ browser }, info) => {
  test.setTimeout(360_000)
  const stack = await new Stack(40 + info.parallelIndex).start()
  try {
    // Two endpoints, the second through a relay the test can switch off.
    const relay = stack.relay()
    const api = new Api(stack.url)
    await api.setup(stack, [stack.rpcUrl, relay.url])
    await api.waitForNode()
    const node = () => api.must<NodeBody>(200, "GET", "/node-status")
    const relayHost = new URL(relay.url).host
    const { context, page } = await operator(browser, { stack, api, url: stack.url })

    await page.goto(stack.url + "/settings/network")
    const status = page.locator("[data-slot=node-status]")
    const indicator = page.locator("[data-node-state]")
    const callouts = status.locator("[data-slot=callout]")
    const relayRow = page.locator(`[data-slot=rpc-endpoints] > li[data-endpoint="${relay.url}"]`)
    const verifier = page.locator("[data-role=verifier]")
    await expect(status).toHaveAttribute("data-state", /^(Synced|Idle|Quiet)$/)
    await expect(verifier).toHaveText(relayHost)
    await expect(relayRow).toHaveAttribute("data-state", "healthy")

    /* 1. The second endpoint is already dead when a payment is created: the watcher cannot even
          tell where to start scanning for it, and reports that as a warning. */
    relay.down = true
    const first = await api.createPayment({ amount: "25", order_id: "ORD-WARN" })
    await expect(status).toHaveAttribute("data-state", /^(Check|Waiting)$/, { timeout: 60_000 })
    const reported = await node()
    expect(reported.hold !== "" || reported.warnings.length > 0).toBe(true)
    // The page passes on the watcher's own sentence, whichever of the two it is.
    const sentence = reported.hold || reported.warnings[0]
    await expect(callouts.filter({ hasText: reported.hold ? "Confirmations are waiting" : "Warning from the watcher" }).first()).toContainText(sentence.slice(0, 40))
    await expect(relayRow).toHaveAttribute("data-state", "failing", { timeout: 30_000 })
    await expect(relayRow).toContainText("Failing")
    await expect(verifier).toHaveText("Nobody right now: no other endpoint is answering")
    await expect(indicator).toHaveAttribute("data-node-state", /^(Check|Waiting)$/)
    await expect(indicator).toHaveAccessibleName(/node: (Check\. 1 warning|Waiting\. Confirmations are on hold)/)

    // It comes back: the warning goes away by itself.
    relay.down = false
    await expect(status).toHaveAttribute("data-state", /^(Synced|Idle|Quiet)$/, { timeout: 60_000 })
    await expect(callouts.filter({ hasText: /Warning from the watcher|Confirmations are waiting/ })).toHaveCount(0)
    await expect(relayRow).toHaveAttribute("data-state", "healthy", { timeout: 30_000 })
    await expect(verifier).toHaveText(relayHost)

    /* 2. The watcher is following the payment when the endpoint dies, and the customer pays:
          now it is a hold. */
    await expect
      .poll(async () => {
        const now = await node()
        return !now.idle && now.hold === "" && now.warnings.length === 0 ? (now.head?.number ?? 0) : 0
      }, { timeout: 60_000 })
      .toBeGreaterThan(0)
    const settled = (await node()).head!.number
    await expect.poll(async () => (await node()).head?.number ?? 0, { timeout: 60_000 }).toBeGreaterThan(settled + 2)
    relay.down = true
    await stack.pay(first.address, "25")

    // The page names the state, the reason and the endpoint at fault.
    await expect(status).toHaveAttribute("data-state", "Waiting", { timeout: 60_000 })
    const hold = callouts.filter({ hasText: "Confirmations are waiting" })
    await expect(hold).toBeVisible()
    await expect(hold).toContainText("Nothing is confirmed or expired while this lasts")
    await expect(hold).toContainText(/no second endpoint/)
    // Said once: the watcher repeats the hold among its warnings, the page does not.
    await expect(callouts.filter({ hasText: "Warning from the watcher" })).toHaveCount(0)
    await expect(relayRow).toHaveAttribute("data-state", "failing", { timeout: 30_000 })
    await expect(verifier).toHaveText("Nobody right now: no other endpoint is answering")
    await expect(indicator).toHaveAttribute("data-node-state", "Waiting")
    await expect(indicator).toHaveAccessibleName(/node: Waiting\. Confirmations are on hold/)
    const held = await node()
    expect(held.hold).toMatch(/no second endpoint/)
    expect(held.verifier).toBe("")
    expect(held.primary).toBe(stack.rpcUrl)

    // The same on the overview. And the money on the chain is not believed on one endpoint's
    // word: the transfer is many blocks deep by now and the payment has not moved.
    await page.goto(stack.url + "/")
    await expect(page.locator("[data-node-state]")).toHaveAttribute("data-node-state", "Waiting")
    await stack.rpc("anvil_mine", "0x8")
    await page.waitForTimeout(7000)
    expect((await api.payment(first.id)).status).toBe("awaiting")
    expect((await node()).hold).not.toBe("")

    // The endpoint comes back: the hold lifts by itself and the payment goes through.
    relay.down = false
    await page.goto(stack.url + "/settings/network")
    await expect(callouts.filter({ hasText: "Confirmations are waiting" })).toHaveCount(0, { timeout: 60_000 })
    await expect(relayRow).toHaveAttribute("data-state", "healthy", { timeout: 30_000 })
    await expect(verifier).toHaveText(relayHost)
    expect((await api.waitForStatus(first.id, ["paid"])).status).toBe("paid")
    await expect(status).toHaveAttribute("data-state", /^(Synced|Idle|Quiet)$/, { timeout: 90_000 })
    await expect(page.locator("[data-node-state]")).toHaveAttribute("data-node-state", /^(Synced|Idle|Quiet)$/)

    await verify(context)
    await context.close()
  } finally {
    await stack.stop()
  }
})

test("switching to the testnet is refused while a payment is open, then goes through and payments work there", async ({ browser }, info) => {
  // Two payments are paid in here, one on each chain, each about 70 seconds with anvil's finality.
  test.setTimeout(420_000)
  const stack = await new Stack(50 + info.parallelIndex).start()
  try {
    const api = new Api(stack.url)
    await api.setup(stack)
    await api.waitForNode()
    const testnet: Chain = await stack.testnet()
    const testnetUrls = [testnet.rpcUrl, testnet.rpcUrl.replace("127.0.0.1", "localhost")]
    // A payment paid on BNB Smart Chain before the switch: it stays that chain's payment.
    const mainnet = await api.createPayment({ amount: "9", order_id: "ORD-MAINNET" })
    await stack.pay(mainnet.address, "9")
    const open = await api.createPayment({ amount: "12", order_id: "ORD-OPEN" })
    const { context, page } = await operator(browser, { stack, api, url: stack.url })

    await page.goto(stack.url + "/settings/network")
    const status = page.locator("[data-slot=node-status]")
    await expect(status.getByRole("heading", { name: "BNB Smart Chain" })).toBeVisible()
    await expect(status).toContainText("real funds")
    await page.locator("[data-slot=network-switch]").getByRole("button", { name: "Switch to BSC Testnet" }).click()
    const dialog = page.getByRole("dialog")
    await expect(dialog.getByRole("heading", { name: "Switch to BSC Testnet" })).toBeVisible()
    const submit = dialog.getByRole("button", { name: "Switch to BSC Testnet" })
    await expect(submit).toBeDisabled()

    // An endpoint of the chain in use is not an endpoint of the other one: the server's probe says so.
    const field = dialog.getByLabel(/RPC endpoint for BSC Testnet|Another endpoint/)
    await field.fill(stack.rpcUrl)
    await dialog.getByRole("button", { name: "Test and add" }).click()
    await expect(dialog.getByRole("alert").filter({ hasText: "Not usable: it serves chain 56, not 97." })).toBeVisible()
    await expect(dialog.locator("[data-slot=switch-endpoints] > li")).toHaveCount(0)

    // The testnet node, under two names, passes: right chain, and the test USDT contract is there.
    for (const url of testnetUrls) {
      await field.fill(url)
      await dialog.getByRole("button", { name: "Test and add" }).click()
      await expect(dialog.locator("[data-slot=switch-endpoints] > li").filter({ hasText: new URL(url).host })).toBeVisible()
    }
    await expect(dialog.locator("[data-slot=switch-endpoints] > li")).toHaveCount(2)
    await expect(submit).toBeDisabled()
    await dialog.getByRole("checkbox").check()
    await expect(submit).toBeEnabled()

    // A payment is awaiting: the server refuses, the dialog says why, and nothing has changed.
    await submit.click()
    const refused = dialog.locator("[data-slot=switch-refused]")
    await expect(refused).toBeVisible()
    await expect(refused).toContainText("The server refused: payments are still open")
    await expect(refused.getByRole("link", { name: "Show open payments" })).toHaveAttribute("href", "/payments?status=open")
    const unchanged = await api.must<{ network: { chain_id: number; rpc_urls: string[] } }>(200, "GET", "/settings")
    expect(unchanged.network.chain_id).toBe(56)
    expect(unchanged.network.rpc_urls).toEqual(stack.rpcUrls)

    // With the payment cancelled (and the other one paid) the same request is accepted.
    await api.must(200, "POST", `/payments/${open.id}/cancel`)
    expect((await api.waitForStatus(mainnet.id, ["paid"], 150_000)).status).toBe("paid")
    await submit.click()
    await expect(dialog).toHaveCount(0)
    await expect(page.getByText("Now on BSC Testnet")).toBeVisible()
    await expect(status.getByRole("heading", { name: "BSC Testnet" })).toBeVisible()
    await expect(status).toContainText("test tokens, no value")
    const switched = await api.must<{ network: { chain_id: number; testnet: boolean; rpc_urls: string[] }; asset: { contract: string; chain_id: number } }>(200, "GET", "/settings")
    expect(switched.network).toMatchObject({ chain_id: 97, testnet: true, rpc_urls: testnetUrls })
    expect(switched.asset).toMatchObject({ chain_id: 97, contract: testnet.token })
    await expect(page.locator("[data-slot=rpc-endpoints] > li")).toHaveCount(2)
    await expect(page.locator(`[data-slot=rpc-endpoints] > li[data-endpoint="${testnet.rpcUrl}"]`)).toBeVisible()
    await expect(page.locator("[data-slot=network-switch]").getByRole("button", { name: "Switch to BNB Smart Chain" })).toBeVisible()

    // The watcher is really on the other chain: a payment made there is detected and confirmed
    // (and can be made once the watcher has processed a block of it).
    await api.waitForNode(60_000, 97)
    await expect(status).toHaveAttribute("data-state", /^(Synced|Idle|Quiet)$/, { timeout: 60_000 })
    const payment = await api.createPayment({ amount: "7.5", order_id: "ORD-TESTNET" })
    await testnet.pay(payment.address, "7.5")
    expect((await api.waitForStatus(payment.id, ["paid"])).status).toBe("paid")
    await page.goto(`${stack.url}/payments/${payment.id}`)
    await expect(page.getByText("Paid").first()).toBeVisible()
    // The payment of the old network is still there, as it ended.
    expect((await api.payment(open.id)).status).toBe("expired")

    // Token isolation, as the operator sees it: the list holds the payments of both chains, each
    // as it ended, and each payment's page names its own chain — not the one the instance is on now.
    await page.goto(`${stack.url}/payments`)
    const row = (order: string) => page.locator("tr", { has: page.getByRole("link", { name: order }) })
    await expect(row("ORD-TESTNET")).toContainText("Paid")
    await expect(row("ORD-MAINNET")).toContainText("Paid")
    await expect(row("ORD-OPEN")).toContainText("Expired")
    await expect(page.locator("[data-finalising]")).toHaveCount(0)
    await page.goto(`${stack.url}/payments/${mainnet.id}`)
    await expect(page.getByText("Paid").first()).toBeVisible()
    await expect(page.locator("main")).toContainText("BNB Smart Chain")
    await expect(page.locator("main")).not.toContainText("BSC Testnet")
    await expect(page.locator("main")).toContainText("9.00")
    await page.goto(`${stack.url}/payments/${payment.id}`)
    await expect(page.locator("main")).toContainText("BSC Testnet")
    // Money sent to the old chain's address now is nobody's business here: the chain is not watched.
    await stack.pay(mainnet.address, "1")
    await new Promise((resolve) => setTimeout(resolve, 10_000))
    const after = await api.payment(mainnet.id)
    expect(after).toMatchObject({ status: "paid", amount_received: "9.00", attention: "" })

    await verify(context)
    await context.close()
  } finally {
    await stack.stop()
  }
})

test("with no payment open the node rests: idle with nothing watched, quiet with only closed payments, healthy both times", async ({ browser }, info) => {
  test.setTimeout(300_000)
  const stack = await new Stack(60 + info.parallelIndex).start()
  try {
    const api = new Api(stack.url)
    await api.setup(stack)
    await api.waitForNode()
    interface Resting extends NodeBody {
      quiet: boolean
      watched_addresses: number
    }
    const node = () => api.must<Resting>(200, "GET", "/node-status")
    const { context, page } = await operator(browser, { stack, api, url: stack.url })
    const status = page.locator("[data-slot=node-status]")
    const indicator = page.locator("[data-node-state]")

    // A fresh instance watches nothing: idle, not quiet, and the page says so as a healthy state.
    await expect.poll(async () => { const n = await node(); return n.idle && !n.quiet && n.healthy }, { timeout: 90_000 }).toBe(true)
    await page.goto(stack.url + "/settings/network")
    await expect(status).toHaveAttribute("data-state", "Idle", { timeout: 30_000 })
    await expect(indicator).toHaveAttribute("data-node-state", "Idle")
    await expect(indicator).toContainText("Nothing to watch")
    await expect(status.locator("[data-slot=callout]").filter({ hasText: "Idle — nothing to watch" })).toBeVisible()

    // An open payment wakes the watcher at once.
    const payment = await api.createPayment({ amount: "9", order_id: "ORD-RESTING" })
    await expect.poll(async () => (await node()).idle, { timeout: 15_000 }).toBe(false)
    await expect(status).toHaveAttribute("data-state", /^(Synced|Check)$/, { timeout: 30_000 })

    // Paid, and the transfer past the blocks it is watched for reorganisations: the payment is
    // closed, its address stays watched for late funds, and the node goes quiet.
    await stack.pay(payment.address, "9")
    expect((await api.waitForStatus(payment.id, ["paid"])).status).toBe("paid")
    // Mined at once, stamped with the time of the last block (not a second apart: the server refuses
    // heads stamped far ahead of its clock).
    await stack.rpc("anvil_mine", "0x150", "0x0")
    await expect.poll(async () => { const n = await node(); return n.idle && n.quiet && n.healthy && n.watched_addresses >= 1 }, { timeout: 120_000 }).toBe(true)
    await expect(status).toHaveAttribute("data-state", "Quiet", { timeout: 30_000 })
    await expect(indicator).toHaveAttribute("data-node-state", "Quiet")
    await expect(indicator).toContainText("Watching closed payments for late funds")
    await expect(status.locator("[data-slot=callout]").filter({ hasText: "Quiet — watching closed payments for late funds" })).toBeVisible()
    // Resting is never shown as syncing or lagging.
    await expect(indicator).not.toContainText(/sync|behind|catching/i)
    await expect(status.getByText("Behind (quiet pause)")).toBeVisible()

    await verify(context)
    await context.close()
  } finally {
    await stack.stop()
  }
})

test("an endpoint that never comes up: no first block, the page names it and says what to do, and removing it starts the instance", async ({ browser }, info) => {
  test.setTimeout(300_000)
  const stack = await new Stack(82 + info.parallelIndex).start()
  try {
    // A good endpoint and one that is down from the start (a mistyped URL, a provider that is gone).
    const dead = stack.relay(3)
    dead.down = true
    const api = new Api(stack.url)
    await api.setup(stack, [stack.rpcUrl, dead.url])
    const deadHost = new URL(dead.url).host
    const { context, page } = await operator(browser, { stack, api, url: stack.url })

    // Two endpoints must agree on where the chain ends before the first block: none is processed.
    await expect.poll(async () => (await api.must<{ last_checked_at: string | null }>(200, "GET", "/node-status")).last_checked_at, { timeout: 60_000 }).not.toBeNull()
    expect((await api.call<{ error: { code: string } }>("POST", "/payments", { amount: "5" })).body.error.code).toBe("chain_not_ready")

    await page.goto(stack.url + "/settings/network")
    const status = page.locator("[data-slot=node-status]")
    const indicator = page.locator("[data-node-state]")
    await expect(status).toHaveAttribute("data-state", "Not started", { timeout: 60_000 })
    await expect(indicator).toHaveAttribute("data-node-state", "Not started")
    await expect(indicator).toContainText("An endpoint can’t be used")
    await expect(indicator).not.toContainText(/no usable|syncing/i)
    const notice = status.locator("[data-slot=callout]").filter({ hasText: "Payments can’t start yet" })
    await expect(notice).toBeVisible()
    await expect(notice).toContainText(deadHost)
    await expect(notice).toContainText("Check its URL in the list below, or remove it")
    await expect(status).not.toContainText("The endpoint in use is failing")

    // The operator removes it: with one endpoint left the watcher starts, and payments can be made.
    const row = page.locator(`[data-slot=rpc-endpoints] > li[data-endpoint="${dead.url}"]`)
    await row.getByRole("button", { name: /Actions for/ }).click()
    await page.getByRole("menuitem", { name: "Remove" }).click()
    await expect(page.locator("[data-slot=rpc-endpoints] > li")).toHaveCount(1)
    await api.waitForNode(120_000)
    await expect(status).not.toHaveAttribute("data-state", "Not started", { timeout: 60_000 })
    expect((await api.createPayment({ amount: "5", order_id: "ORD-AFTER-FIX" })).status).toBe("awaiting")

    await verify(context)
    await context.close()
  } finally {
    await stack.stop()
  }
})
