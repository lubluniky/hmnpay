import { mkdirSync } from "node:fs"
import path from "node:path"

import type { Browser, Page } from "@playwright/test"

import { expect, FINAL_MS, pane, prepare, signIn, test, type Instance } from "./fixtures"
import { MERCHANT, OPERATOR, REPO, Stack, XPUB } from "./harness"

/**
 * Screenshots of every screen, in both themes, from the real binary with real data: payments
 * that were really paid, short, late, cancelled or reorganised out on a local chain; real
 * links, webhooks (one receiver answering, one down) and keys. Plus a frame sequence of a
 * checkout going from awaiting to paid.
 */
const OUT = process.env.HMNPAY_SHOTS_DIR ?? path.join(REPO, ".shots")
const THEMES = ["dark", "light"] as const
const DESKTOP = { width: 1440, height: 900 }
const PHONE = { width: 390, height: 844 }
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

async function shot(page: Page, name: string, options: { fullPage?: boolean } = {}) {
  await page.evaluate(() => document.fonts.ready.then(() => undefined))
  await page.waitForTimeout(600) // entrances settle
  mkdirSync(path.dirname(path.join(OUT, name)), { recursive: true })
  await page.screenshot({ path: path.join(OUT, `${name}.png`), fullPage: options.fullPage ?? true })
}

async function open(browser: Browser, theme: "dark" | "light", viewport = DESKTOP, instance?: Instance) {
  const context = await browser.newContext({ viewport, colorScheme: theme, deviceScaleFactor: 2 })
  await prepare(context, { theme })
  if (instance) await signIn(context, instance)
  return context.newPage()
}

test("screenshots of every screen from the real binary", async ({ browser, instance }) => {
  const { stack, api } = instance
  mkdirSync(OUT, { recursive: true })

  /* ------------------------------ seed real data ------------------------------ */
  // The endings that need the payment window to pass are started first (a minute, plus a minute of grace).
  const expired = await api.createPayment({ amount: "49", order_id: "ORD-1009", description: "Starter plan", ttl_seconds: 60 })
  const short = await api.createPayment({ amount: "120", order_id: "ORD-1010", description: "Team plan, monthly", customer_email: "li.wei@example.com", ttl_seconds: 60 })
  const late = await api.createPayment({ amount: "75", order_id: "ORD-1011", description: "Workshop ticket", ttl_seconds: 60 })
  // A transfer that a chain reorganisation removes: seen, then gone, and the window closes ("failed").
  const settings = await api.must<{ payments: Record<string, unknown> }>(200, "GET", "/settings")
  await api.must(200, "PUT", "/settings/payments", { ...settings.payments, required_confirmations: 60 })
  const failed = await api.createPayment({ amount: "64", order_id: "ORD-1012", description: "Annual licence", ttl_seconds: 60 })
  await api.must(200, "PUT", "/settings/payments", { ...settings.payments, required_confirmations: 5 })
  await stack.pay(failed.address, "64")
  await api.waitForStatus(failed.id, ["confirming"])
  const detail = await api.must<{ transfers: { block_number: number }[] }>(200, "GET", `/payments/${failed.id}/detail`)
  const head = Number(await stack.rpc<string>("eth_blockNumber"))
  await stack.rpc("anvil_reorg", head - detail.transfers[0].block_number + 1, [])
  const started = Date.now()
  // Only now the partial payment: the reorganisation above must not take it with it.
  await stack.pay(short.address, "80")

  const paidOrders: [string, string, string][] = [
    ["149", "ORD-1001", "Pro plan, annual"],
    ["49.99", "ORD-1002", "Starter plan"],
    ["320", "ORD-1003", "Team plan, 5 seats"],
    ["18.5", "ORD-1004", "Add-on: priority support"],
  ]
  for (const [amount, order, description] of paidOrders) {
    const p = await api.createPayment({ amount, order_id: order, description, customer_email: "maria.k@example.com", metadata: { plan: "pro" } })
    await stack.pay(p.address, amount)
  }
  const overpaid = await api.createPayment({ amount: "60", order_id: "ORD-1005", description: "Consulting hour" })
  await stack.pay(overpaid.address, "61.5")
  const resolved = await api.createPayment({ amount: "30", order_id: "ORD-1006", description: "E-book bundle" })
  await stack.pay(resolved.address, "33")
  const cancelled = await api.createPayment({ amount: "25", order_id: "ORD-1007", description: "Gift card" })
  await api.must(200, "POST", `/payments/${cancelled.id}/cancel`)
  const detected = await api.createPayment({ amount: "200", order_id: "ORD-1013", description: "Hardware deposit" })
  await stack.pay(detected.address, "120")
  const awaiting = await api.createPayment({ amount: "149", order_id: "ORD-1014", description: "Pro plan, annual", customer_email: "sam@example.com" })
  await api.waitForStatus(overpaid.id, ["overpaid"])
  await api.waitForStatus(resolved.id, ["overpaid"])
  await api.must(200, "POST", `/payments/${resolved.id}/resolve`, { note: "Refunded 3 USDT to the sender on Nov 18." })
  await api.waitForStatus(detected.id, ["detected"])

  await api.must(201, "POST", "/payment-links", { name: "Pro plan, annual", slug: "pro-annual", description: "One year of Pro, billed once.", amount: "149" })
  await api.must(201, "POST", "/payment-links", { name: "Support the project", slug: "donate", description: "Any amount helps.", amount: null, min_amount: "5" })
  await api.must(201, "POST", "/payment-links", { name: "Launch workshop seat", slug: "workshop", amount: "75", max_uses: 1 })
  const off = await api.must<{ id: string }>(201, "POST", "/payment-links", { name: "Black Friday bundle", slug: "black-friday", amount: "99" })
  await api.must(200, "POST", `/payment-links/${off.id}/deactivate`)
  const viaLink = await api.must<{ id: string; address: string }>(201, "POST", "/public/links/pro-annual/payments", {}, { "x-forwarded-for": "10.31.0.1", "idempotency-key": "shots-1" })
  await stack.pay(viaLink.address, "149")
  await api.must(201, "POST", "/public/links/workshop/payments", {}, { "x-forwarded-for": "10.31.0.2", "idempotency-key": "shots-2" })

  const good = await api.must<{ id: string }>(201, "POST", "/webhook-endpoints", { url: stack.hookUrl, description: "Order service", events: ["payment.paid", "payment.overpaid", "payment.underpaid", "payment.expired"] })
  await api.must(201, "POST", "/webhook-endpoints", { url: stack.hookUrl.replace("/hook", "/failing"), description: "Accounting sync", events: ["payment.paid"] })
  await api.must(202, "POST", `/webhook-endpoints/${good.id}/test`)
  const hooked = await api.createPayment({ amount: "88", order_id: "ORD-1015", description: "Design review" })
  await stack.pay(hooked.address, "88")
  await api.must(201, "POST", "/api-keys", { name: "Storefront checkout", scopes: ["payments:read", "payments:write"], expires_at: new Date(Date.now() + 90 * 86_400_000).toISOString() })
  const old = await api.must<{ id: string }>(201, "POST", "/api-keys", { name: "Old storefront plugin", scopes: ["payments:read"], expires_at: null })
  await api.must(200, "POST", `/api-keys/${old.id}/revoke`)
  await api.waitForStatus(hooked.id, ["paid"])
  await api.waitForStatus(viaLink.id, ["paid"])

  /* ------------------------- a payment going to paid, live ------------------------- */
  {
    // Frames always; a video too with HMNPAY_SHOTS_VIDEO=1 (needs `playwright install ffmpeg`).
    const context = await browser.newContext({
      viewport: DESKTOP,
      colorScheme: "dark",
      ...(process.env.HMNPAY_SHOTS_VIDEO === "1" ? { recordVideo: { dir: path.join(OUT, "paid-live"), size: DESKTOP } } : {}),
    })
    await prepare(context, { theme: "dark" })
    const page = await context.newPage()
    const live = await api.createPayment({ amount: "149", order_id: "ORD-1016", description: "Pro plan, annual" })
    await page.goto(`${instance.url}/pay/${live.id}`)
    await expect(pane(page)).toHaveAttribute("data-pane", "awaiting")
    await page.mouse.click(700, 880) // a gesture, so the page may play its cues
    mkdirSync(path.join(OUT, "paid-live"), { recursive: true })
    let frame = 0
    const snap = () => page.screenshot({ path: path.join(OUT, "paid-live", `frame-${String(++frame).padStart(3, "0")}.png`) })
    await snap()
    await stack.pay(live.address, "149")
    // Until it is paid: finality on the local chain (see FINAL_MS), then a few seconds of "paid".
    const end = Date.now() + FINAL_MS
    let paidAt = 0
    while (Date.now() < end && (paidAt === 0 || Date.now() < paidAt + 3500)) {
      await snap()
      if (paidAt === 0 && (await page.locator("[data-checkout]").getAttribute("data-status")) === "paid") paidAt = Date.now()
      await sleep(paidAt ? 120 : 450)
    }
    expect(paidAt, "the checkout reached paid while it was being filmed").toBeGreaterThan(0)
    await context.close() // writes the video
  }

  /* ------------------------------- the endings ------------------------------- */
  await api.waitForStatus(expired.id, ["expired"], Math.max(1000, 200_000 - (Date.now() - started)))
  await api.waitForStatus(short.id, ["underpaid"], 60_000)
  await api.waitForStatus(late.id, ["expired"], 60_000)
  await api.waitForStatus(failed.id, ["failed"], 60_000)
  await stack.pay(late.address, "75")
  await expect.poll(async () => (await api.payment(late.id)).attention, { timeout: FINAL_MS }).toBe("late_funds")

  const confirmingFor = async () => {
    const p = await api.createPayment({ amount: "149", order_id: "ORD-1017", description: "Pro plan, annual" })
    return p
  }

  // The other chain, for the "switch network" dialog.
  const testnet = await stack.testnet()

  /* --------------------------------- capture --------------------------------- */
  for (const theme of THEMES) {
    // The dashboard.
    const page = await open(browser, theme, DESKTOP, instance)
    const go = async (route: string, name: string, ready: () => Promise<unknown>) => {
      await page.goto(instance.url + route)
      await ready()
      await shot(page, `${theme}/${name}`)
    }
    await go("/", "dashboard-overview", () => expect(page.getByText("Recent payments")).toBeVisible())
    await go("/payments", "payments", () => expect(page.getByRole("link", { name: "ORD-1001" }).first()).toBeVisible())
    await go("/payments?status=attention", "payments-needs-attention", () => expect(page.getByRole("link", { name: "ORD-1005" }).first()).toBeVisible())
    await go(`/payments/${hooked.id}`, "payment-detail-paid", () => expect(page.getByRole("heading", { name: "Transfers" })).toBeVisible())
    await go(`/payments/${overpaid.id}`, "payment-detail-overpaid", () => expect(page.getByRole("button", { name: "Mark as resolved" })).toBeVisible())
    await go(`/payments/${resolved.id}`, "payment-detail-resolved", () => expect(page.getByRole("button", { name: "Reopen" })).toBeVisible())
    await go(`/payments/${short.id}`, "payment-detail-underpaid", () => expect(page.getByText(/Short by/)).toBeVisible())
    await go(`/payments/${late.id}`, "payment-detail-late-funds", () => expect(page.getByText(/on a payment that had already closed/)).toBeVisible())
    await go(`/payments/${failed.id}`, "payment-detail-failed", () => expect(page.getByText("The transfer dropped out of the chain").first()).toBeVisible())
    await go(`/payments/${detected.id}`, "payment-detail-detected", () => expect(page.getByText("Still due")).toBeVisible())
    await go(`/payments/${awaiting.id}`, "payment-detail-awaiting", () => expect(page.getByRole("button", { name: "Cancel payment" })).toBeVisible())
    await go(`/payments/${cancelled.id}`, "payment-detail-cancelled", () => expect(page.getByText("Cancelled", { exact: true }).first()).toBeVisible())
    await go("/payments?new=1", "payment-create", () => expect(page.getByRole("dialog")).toBeVisible())
    await go("/payment-links", "payment-links", () => expect(page.getByText("Pro plan, annual").first()).toBeVisible())
    await go("/payment-links?new=1", "payment-link-create", () => expect(page.getByRole("dialog")).toBeVisible())
    await page.goto(instance.url + "/payment-links")
    await page.getByRole("button", { name: "Actions for Launch workshop seat" }).click()
    await page.getByRole("menuitem", { name: "Edit" }).click()
    await expect(page.getByRole("dialog").getByLabel("Maximum payments")).toHaveValue("1")
    await shot(page, `${theme}/payment-link-edit`, { fullPage: false })
    await go("/settings", "settings-general", () => expect(page.locator("[data-slot=sessions]")).toBeVisible())
    {
      // The About block on its own: version, licence, notices.
      const about = page.locator("[data-slot=settings-section]").filter({ has: page.locator("[data-slot=about-license]") })
      await expect(about.locator("[data-slot=about-license]")).toBeVisible()
      await about.scrollIntoViewIfNeeded()
      await about.screenshot({ path: path.join(OUT, `${theme}/settings-about.png`) })
    }
    await go("/settings/network", "settings-network", () => expect(page.locator("[data-slot=rpc-endpoints] > li").first()).toHaveAttribute("data-state", "healthy"))
    // Switching network: endpoints of the other chain pass the probe, and the server refuses while payments are open.
    await page.locator("[data-slot=network-switch]").getByRole("button", { name: "Switch to BSC Testnet" }).click()
    {
      const dialog = page.getByRole("dialog")
      for (const url of [testnet.rpcUrl, testnet.rpcUrl.replace("127.0.0.1", "localhost")]) {
        await dialog.getByLabel(/RPC endpoint for BSC Testnet|Another endpoint/).fill(url)
        await dialog.getByRole("button", { name: "Test and add" }).click()
        await expect(dialog.locator("[data-slot=switch-endpoints] > li").filter({ hasText: new URL(url).host })).toBeVisible()
      }
      await shot(page, `${theme}/settings-network-switch`, { fullPage: false })
      await dialog.getByRole("checkbox").check()
      await dialog.getByRole("button", { name: "Switch to BSC Testnet" }).click()
      await expect(dialog.locator("[data-slot=switch-refused]")).toBeVisible()
      await dialog.locator("[data-slot=switch-refused]").scrollIntoViewIfNeeded()
      await shot(page, `${theme}/settings-network-switch-refused`, { fullPage: false })
      await page.keyboard.press("Escape")
    }
    await go("/settings/wallet", "settings-wallet", () => expect(page.locator('[data-derived="0"]')).toBeVisible())
    await page.locator("[data-slot=settings-section]").filter({ hasText: "Deposit wallet" }).getByRole("button", { name: "Change" }).click()
    await page.getByRole("dialog").getByLabel("Next address index").fill("3")
    await expect(page.getByRole("dialog").locator("[data-slot=index-address]")).toBeVisible()
    await shot(page, `${theme}/settings-wallet-next-index`, { fullPage: false })
    await page.keyboard.press("Escape")
    await go("/settings/webhooks", "settings-webhooks", () => expect(page.locator("[data-delivery]").first()).toBeVisible())
    await page.locator("[data-endpoint]").filter({ hasText: "Order service" }).getByRole("button").click()
    await expect(page.locator("[data-slot=signing-secret]")).toBeVisible()
    await shot(page, `${theme}/settings-webhook-endpoint`, { fullPage: false })
    await page.keyboard.press("Escape")
    await page.locator("[data-delivery]").filter({ hasText: "payment.paid" }).first().getByRole("button").click()
    await expect(page.locator("[data-slot=event-seq]")).toBeVisible()
    await shot(page, `${theme}/settings-webhook-delivery`, { fullPage: false })
    await go("/settings/api-keys", "settings-api-keys", () => expect(page.locator("[data-key]").first()).toBeVisible())
    // A key at the one moment it is shown.
    await page.getByRole("button", { name: "Create key" }).first().click()
    await page.getByRole("dialog").getByLabel("Name").fill(`Fulfilment worker (${theme})`)
    await page.getByRole("dialog").getByRole("button", { name: "Create key" }).click()
    await expect(page.getByRole("dialog").getByText("Save your key now")).toBeVisible()
    await shot(page, `${theme}/settings-api-key-created`, { fullPage: false })
    await page.context().close()

    // What a customer sees, on a desktop and on a phone.
    for (const [device, viewport] of [["desktop", DESKTOP], ["phone", PHONE]] as const) {
      const customer = await open(browser, theme, viewport)
      const states: [string, string, string][] = [
        [awaiting.id, "awaiting", "awaiting"],
        [detected.id, "detected", "detected-top-up"],
        [hooked.id, "paid", "paid"],
        [overpaid.id, "overpaid", "overpaid"],
        [short.id, "underpaid", "underpaid"],
        [expired.id, "expired", "expired"],
        [late.id, "expired", "expired-late-funds"],
        [cancelled.id, "cancelled", "cancelled"],
        [failed.id, "failed", "failed"],
        ["pay_00000000000000000000000000", "not-found", "not-found"],
      ]
      for (const [id, state, name] of states) {
        await customer.goto(`${instance.url}/pay/${id}`)
        await expect(pane(customer)).toHaveAttribute("data-pane", state)
        await shot(customer, `${theme}/checkout-${name}.${device}`)
      }
      // Confirming, caught live.
      const p = await confirmingFor()
      await customer.goto(`${instance.url}/pay/${p.id}`)
      await expect(pane(customer)).toHaveAttribute("data-pane", "awaiting")
      await stack.pay(p.address, "149")
      await expect(pane(customer)).toHaveAttribute("data-pane", "confirming", { timeout: 30_000 })
      await customer.waitForTimeout(1200)
      await customer.screenshot({ path: path.join(OUT, `${theme}/checkout-confirming.${device}.png`), fullPage: true })

      for (const [slug, name] of [["pro-annual", "fixed"], ["donate", "choose-amount"], ["workshop", "used-up"], ["black-friday", "switched-off"], ["nope", "not-found"]] as const) {
        await customer.goto(`${instance.url}/l/${slug}`)
        await expect(customer.locator("[data-pane]").last()).not.toHaveAttribute("data-pane", "loading")
        await shot(customer, `${theme}/payment-link-${name}.${device}`)
      }
      await customer.goto(`${instance.url}/login`)
      await expect(customer.getByRole("button", { name: "Sign in" })).toBeVisible()
      await shot(customer, `${theme}/login.${device}`)
      if (device === "desktop") {
        await customer.getByRole("button", { name: "Have a recovery code?" }).click()
        await shot(customer, `${theme}/login-recovery-code.${device}`)
        await customer.goto(`${instance.url}/login?step=reset`)
        await expect(customer.getByText("Reset from the server")).toBeVisible()
        await shot(customer, `${theme}/login-reset.${device}`)
      }
      await customer.context().close()
    }
  }
})

test("screenshots of the first-run setup", async ({ browser }) => {
  const stack = await new Stack(30).start()
  try {
    for (const theme of THEMES) {
      const page = await open(browser, theme)
      await page.goto(stack.url + "/setup")
      await expect(page.getByRole("heading", { name: "Welcome to hmnpay" })).toBeVisible()
      await shot(page, `${theme}/setup-1-account`)
      await page.getByLabel("Setup token").fill(stack.setupToken)
      await page.getByLabel("Email").fill(OPERATOR.email)
      await page.getByLabel("Password", { exact: true }).fill(OPERATOR.password)
      await shot(page, `${theme}/setup-1-account-filled`)
      await page.getByRole("button", { name: "Continue" }).click()
      await expect(page.getByRole("heading", { name: /Connect to/ })).toBeVisible()
      await page.getByLabel("RPC endpoint").fill("http://127.0.0.1:1")
      await page.getByRole("button", { name: "Test connection" }).click()
      await expect(page.getByText("Can’t connect")).toBeVisible()
      await shot(page, `${theme}/setup-2-network-unreachable`)
      await page.getByLabel("RPC endpoint").fill(stack.rpcUrls[0])
      await page.getByRole("button", { name: "Test connection" }).click()
      await expect(page.getByText("Connected")).toBeVisible()
      await shot(page, `${theme}/setup-2-network-one-endpoint`)
      await page.getByLabel("Another RPC endpoint").fill(stack.rpcUrls[1])
      await page.getByRole("button", { name: "Test and add" }).click()
      await expect(page.getByText("Good: 2 endpoints.")).toBeVisible()
      await page.getByLabel("Another RPC endpoint").fill("")
      await shot(page, `${theme}/setup-2-network-two-endpoints`)
      await page.getByRole("button", { name: "Continue" }).click()
      await expect(page.getByRole("heading", { name: "Connect your wallet" })).toBeVisible()
      await shot(page, `${theme}/setup-3-wallet-empty`)
      await page.getByLabel("Extended public key").fill(XPUB)
      await expect(page.locator('[data-derived="0"]')).toBeVisible()
      await page.getByText("These match the first addresses in my wallet").click()
      await shot(page, `${theme}/setup-3-wallet`)
      await page.getByRole("button", { name: "Continue" }).click()
      await page.getByLabel("Business name").fill(MERCHANT.name)
      await page.getByLabel("Support email").fill(MERCHANT.supportEmail)
      await shot(page, `${theme}/setup-4-business`)
      if (theme === THEMES[THEMES.length - 1]) {
        // Only once: finishing setup closes it.
        await page.getByRole("button", { name: "Finish setup" }).click()
        await expect(page.getByRole("heading", { name: "You’re ready to take payments" })).toBeVisible()
        await page.waitForTimeout(1800) // the wave has passed
        await shot(page, `${theme}/setup-5-done`)
        await page.getByRole("link", { name: "Go to dashboard" }).click()
        await expect(page.getByText("Take your first payment")).toBeVisible()
        await shot(page, `${theme}/dashboard-first-run`)
      }
      await page.context().close()
    }
  } finally {
    await stack.stop()
  }
})
