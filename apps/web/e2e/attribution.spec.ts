import { expect, expectAttribution, FINAL_MS, pane, test } from "./fixtures"

const THEMES = ["dark", "light"] as const
const VIEWPORTS = { desktop: { width: 1440, height: 900 }, phone: { width: 390, height: 844 } } as const

/**
 * "Powered by hmnpay" is on every page a payer sees, in every state, in both themes and on a
 * phone: as text, as a link to the project, visible and legible.
 */
test("the attribution is on the checkout and the payment-link page in every state, theme and width", async ({ customer, instance }) => {
  test.setTimeout(300_000)
  const { stack, api } = instance

  // One payment per state that can be held still.
  const awaiting = await api.createPayment({ amount: "15", order_id: "ATTR-AWAITING", description: "Attribution check" })
  const detected = await api.createPayment({ amount: "15", order_id: "ATTR-DETECTED" })
  const paid = await api.createPayment({ amount: "15", order_id: "ATTR-PAID" })
  const overpaid = await api.createPayment({ amount: "15", order_id: "ATTR-OVERPAID" })
  const cancelled = await api.createPayment({ amount: "15", order_id: "ATTR-CANCELLED" })
  await stack.pay(detected.address, "5")
  await stack.pay(paid.address, "15")
  await stack.pay(overpaid.address, "16")
  await api.must(200, "POST", `/payments/${cancelled.id}/cancel`)
  await api.waitForStatus(detected.id, ["detected"])
  await api.waitForStatus(paid.id, ["paid"])
  await api.waitForStatus(overpaid.id, ["overpaid"])

  // Links in each state.
  await api.must(201, "POST", "/payment-links", { name: "Attribution fixed", slug: "attr-fixed", amount: "9.99" })
  await api.must(201, "POST", "/payment-links", { name: "Attribution open", slug: "attr-open", amount: null })
  const off = await api.must<{ id: string }>(201, "POST", "/payment-links", { name: "Attribution off", slug: "attr-off", amount: "5" })
  await api.must(200, "POST", `/payment-links/${off.id}/deactivate`)
  await api.must(201, "POST", "/payment-links", { name: "Attribution expired", slug: "attr-expired", amount: "5", expires_at: new Date(Date.now() + 4000).toISOString() })
  await api.must(201, "POST", "/payment-links", { name: "Attribution limited", slug: "attr-limited", amount: "5", max_uses: 1 })
  await api.must(201, "POST", "/public/links/attr-limited/payments", {}, { "x-forwarded-for": "10.55.0.1", "idempotency-key": "attr-limited-1" })
  await new Promise((resolve) => setTimeout(resolve, 4500)) // the "expired" link's end date passes

  const checkouts: [string, string][] = [
    [awaiting.id, "awaiting"],
    [detected.id, "detected"],
    [paid.id, "paid"],
    [overpaid.id, "overpaid"],
    [cancelled.id, "cancelled"],
    ["pay_00000000000000000000000000", "not-found"],
  ]
  const links: [string, string][] = [
    ["attr-fixed", "active"],
    ["attr-open", "active"],
    ["attr-off", "disabled"],
    ["attr-expired", "expired"],
    ["attr-limited", "limit"],
    ["attr-missing", "not-found"],
  ]

  for (const theme of THEMES) {
    for (const [name, viewport] of Object.entries(VIEWPORTS)) {
      const page = await customer({ theme, viewport })
      for (const [id, state] of checkouts) {
        await test.step(`checkout ${state}, ${theme}, ${name}`, async () => {
          await page.goto(`${instance.url}/pay/${id}`)
          await expect(pane(page)).toHaveAttribute("data-pane", state)
          await expect(page.locator("html")).toHaveClass(new RegExp(`\\b${theme}\\b`))
          await expectAttribution(page)
          // No sideways scrolling on a phone.
          expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
        })
      }
      for (const [slug, state] of links) {
        await test.step(`link ${slug} (${state}), ${theme}, ${name}`, async () => {
          await page.goto(`${instance.url}/l/${slug}`)
          await expect(page.locator("[data-pane]").last()).toHaveAttribute("data-pane", state)
          await expectAttribution(page)
          expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
        })
      }
      // While loading (the API held back) the attribution is already there.
      await test.step(`checkout loading, ${theme}, ${name}`, async () => {
        await page.route("**/api/v1/public/payments/*", async (route) => {
          await new Promise((resolve) => setTimeout(resolve, 2500))
          await route.continue().catch(() => {})
        })
        await page.goto(`${instance.url}/pay/${awaiting.id}`, { waitUntil: "commit" })
        await expect(pane(page)).toHaveAttribute("data-pane", "loading")
        await expectAttribution(page)
        await expect(pane(page)).toHaveAttribute("data-pane", "awaiting")
        await page.unroute("**/api/v1/public/payments/*")
      })
    }
  }

  // Confirming cannot be held still: catch it live, on a phone, in the light theme.
  const confirming = await api.createPayment({ amount: "15", order_id: "ATTR-CONFIRMING" })
  const page = await customer({ theme: "light", viewport: VIEWPORTS.phone })
  await page.goto(`${instance.url}/pay/${confirming.id}`)
  await expect(pane(page)).toHaveAttribute("data-pane", "awaiting")
  await stack.pay(confirming.address, "15")
  await expect(pane(page)).toHaveAttribute("data-pane", "confirming", { timeout: 30_000 })
  await expectAttribution(page)
  await expect(page.locator("[data-checkout]")).toHaveAttribute("data-status", "paid", { timeout: FINAL_MS })
  await expectAttribution(page)
})

/**
 * The endings that take the payment window to arrive: expired, underpaid (closed short) and
 * late funds. The shortest window the API allows is a minute, plus a minute of grace.
 */
test("expired, closed short and late funds are presented as the API reports them", async ({ customer, instance }) => {
  test.setTimeout(420_000)
  const { stack, api } = instance
  const expired = await api.createPayment({ amount: "20", order_id: "END-EXPIRED", ttl_seconds: 60 })
  const short = await api.createPayment({ amount: "20", order_id: "END-SHORT", ttl_seconds: 60 })
  const late = await api.createPayment({ amount: "20", order_id: "END-LATE", ttl_seconds: 60 })
  await stack.pay(short.address, "8")
  await api.waitForStatus(short.id, ["detected"])

  const page = await customer({ viewport: VIEWPORTS.phone })
  await page.goto(`${instance.url}/pay/${expired.id}`)
  await expect(pane(page)).toHaveAttribute("data-pane", "awaiting")

  // The window closes (a minute, a minute of grace, the next block): the open page turns by itself.
  await expect(pane(page)).toHaveAttribute("data-pane", "expired", { timeout: 200_000 })
  await expect(page.getByText("This payment has expired")).toBeVisible()
  await expect(page.getByText("Already sent the payment?")).toBeVisible()
  await expectAttribution(page)

  await api.waitForStatus(short.id, ["underpaid"], 120_000)
  await page.goto(`${instance.url}/pay/${short.id}`)
  await expect(pane(page)).toHaveAttribute("data-pane", "underpaid")
  await expect(page.getByText("This payment closed short")).toBeVisible()
  await expect(page.getByLabel("Order summary")).toContainText("8.00 received")
  await expect(page.getByLabel("Order summary")).toContainText("12.00 short")
  await expectAttribution(page)

  // Money that arrives after the payment closed: flagged for the merchant, and said to the payer.
  await api.waitForStatus(late.id, ["expired"], 120_000)
  await stack.pay(late.address, "20")
  await expect.poll(async () => (await api.payment(late.id)).attention, { timeout: FINAL_MS }).toBe("late_funds")
  await page.goto(`${instance.url}/pay/${late.id}`)
  await expect(pane(page)).toHaveAttribute("data-pane", "expired")
  await expect(page.getByText(/20\.00\s*USDT\s*reached\s*this address/)).toBeVisible()
  await expect(page.getByText("It isn’t lost.")).toBeVisible()
  await expectAttribution(page)
  expect((await api.payment(late.id)).status).toBe("expired")
})
