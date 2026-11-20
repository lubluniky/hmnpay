import { expect, expectAttribution, expectedErrors, FINAL_MS, pane, test } from "./fixtures"

test("underpay, then top up: the checkout asks for the rest and completes", async ({ customer, instance }) => {
  const { stack, api } = instance
  const payment = await api.createPayment({ amount: "25", order_id: "ORD-TOPUP", description: "Top-up test" })
  const page = await customer()
  await page.goto(`${instance.url}/pay/${payment.id}`)
  await expect(pane(page)).toHaveAttribute("data-pane", "awaiting")

  // Part of the amount: the payment stays open and the page asks for exactly what is missing.
  const payer = await stack.payer()
  await stack.pay(payment.address, "10", payer)
  await expect(pane(page)).toHaveAttribute("data-pane", "detected", { timeout: 30_000 })
  await expect(page.getByRole("heading", { name: "Send the rest" })).toBeVisible()
  const summary = page.getByLabel("Order summary")
  await expect(summary).toContainText("10.00 received")
  await expect(summary).toContainText("15.00 to go")
  await expect(page.getByText("Remaining amount").first()).toBeVisible()
  await expect(page.getByRole("timer")).toBeVisible()
  await expectAttribution(page)
  expect((await api.payment(payment.id)).amount_due).toBe("15.00")

  // The rest: confirming, then paid.
  await stack.pay(payment.address, "15", payer)
  await expect(pane(page)).toHaveAttribute("data-pane", "confirming", { timeout: 30_000 })
  await expect(page.locator("[data-checkout]")).toHaveAttribute("data-status", "paid", { timeout: FINAL_MS })
  await expect(page.getByText("Payment complete").last()).toBeVisible()
  expect((await api.payment(payment.id)).amount_received).toBe("25.00")
})

test("an overpayment is paid, and says how much was too much", async ({ customer, instance }) => {
  const { stack, api } = instance
  const payment = await api.createPayment({ amount: "20", order_id: "ORD-OVER" })
  await stack.pay(payment.address, "23.5")
  await api.waitForStatus(payment.id, ["overpaid"])
  // Loaded after the fact: the receipt at rest.
  const page = await customer()
  await page.goto(`${instance.url}/pay/${payment.id}`)
  await expect(pane(page)).toHaveAttribute("data-pane", "overpaid")
  await expect(page.getByText("Payment complete")).toBeVisible()
  await expect(page.getByText("You sent", { exact: true })).toBeVisible()
  await expect(page.getByText(/You sent\s*3\.50\s*USDT\s*too much/)).toBeVisible()
  await expectAttribution(page)
})

test("when the stream is refused the checkout polls instead, and still reaches paid", async ({ customer, instance }) => {
  const { stack, api } = instance
  const payment = await api.createPayment({ amount: "12", order_id: "ORD-POLL" })
  const page = await customer()
  const ip = "10.77.0.9"
  await page.context().setExtraHTTPHeaders({ "X-Forwarded-For": ip })

  // Somebody on the payer's network already holds every stream one address may have on a payment (three).
  // The responses are kept, not only their controllers: an unreferenced response is collected,
  // and collecting it cancels its body, which closes the stream and frees its slot.
  const held: AbortController[] = []
  const streams: Response[] = []
  for (let i = 0; i < 3; i++) {
    const controller = new AbortController()
    const response = await fetch(`${instance.url}/api/v1/public/payments/${payment.id}/stream`, {
      headers: { "x-forwarded-for": ip, accept: "text/event-stream" },
      signal: controller.signal,
    })
    expect(response.status).toBe(200)
    held.push(controller)
    streams.push(response)
  }
  const refused = await fetch(`${instance.url}/api/v1/public/payments/${payment.id}/stream`, { headers: { "x-forwarded-for": ip } })
  expect(refused.status).toBe(429)
  expect((await refused.json()).error.code).toBe("too_many_streams")

  try {
    await page.goto(`${instance.url}/pay/${payment.id}`)
    await expect(pane(page)).toHaveAttribute("data-pane", "awaiting")
    // Refused: the page says it is checking, and keeps itself current by polling.
    await expect(page.locator("[data-checkout]")).toHaveAttribute("data-live", "polling")
    await expect(page.getByText("Checking every few seconds")).toBeVisible()

    await stack.pay(payment.address, "12")
    await expect(pane(page)).toHaveAttribute("data-pane", "confirming", { timeout: 30_000 })
    await expect(page.locator("[data-checkout]")).toHaveAttribute("data-status", "paid", { timeout: FINAL_MS })
    expect((await api.payment(payment.id)).status).toBe("paid")
  } finally {
    for (const controller of held) controller.abort()
    expect(streams).toHaveLength(3)
  }
})

test("a stream that breaks is resumed, and the page never stops following the payment", async ({ customer, instance }) => {
  const { stack, api } = instance
  const payment = await api.createPayment({ amount: "8", order_id: "ORD-BREAK" })
  const page = await customer()
  await page.goto(`${instance.url}/pay/${payment.id}`)
  await expect(page.locator("[data-checkout]")).toHaveAttribute("data-live", "stream")

  // Cut the connection under the page: every stream request fails for a while.
  const resumed: (string | undefined)[] = []
  await page.route("**/stream", async (route) => {
    resumed.push(route.request().headers()["last-event-id"])
    await route.abort("connectionreset")
  })
  await page.evaluate(() => window.dispatchEvent(new Event("offline")))
  // Force the open stream to end: the server closes it when the context navigates; here the page is reloaded instead.
  await page.reload()
  await expect(pane(page)).toHaveAttribute("data-pane", "awaiting")
  await expect(page.locator("[data-checkout]")).toHaveAttribute("data-live", "polling")

  // The payment arrives while there is no stream: polling shows it.
  await stack.pay(payment.address, "8")
  await expect(pane(page)).toHaveAttribute("data-pane", "confirming", { timeout: 30_000 })

  // The connection is back: the page reconnects by itself and says where it left off.
  await page.unroute("**/stream")
  const request = await page.waitForRequest((r) => r.url().endsWith("/stream"), { timeout: 60_000 })
  expect(request.headers()["accept"]).toContain("text/event-stream")
  await expect(page.locator("[data-checkout]")).toHaveAttribute("data-status", "paid", { timeout: FINAL_MS })
  expect((await api.payment(payment.id)).status).toBe("paid")
})

test("a payment that does not exist, and one whose page cannot load", async ({ customer, instance }) => {
  const page = await customer()
  await page.goto(`${instance.url}/pay/pay_00000000000000000000000000`)
  await expect(pane(page)).toHaveAttribute("data-pane", "not-found")
  await expect(page.getByText("This payment doesn’t exist")).toBeVisible()
  await expectAttribution(page)

  await page.goto(`${instance.url}/pay/not-a-payment-id`)
  await expect(pane(page)).toHaveAttribute("data-pane", "not-found")
  await expectAttribution(page)

  // The server is unreachable for the API: the page says so, keeps the attribution, and can retry.
  const { api } = instance
  const payment = await api.createPayment({ amount: "5" })
  await page.route("**/api/v1/public/**", (route) => route.abort("connectionrefused"))
  await page.goto(`${instance.url}/pay/${payment.id}`)
  await expect(pane(page)).toHaveAttribute("data-pane", "error")
  await expect(page.getByText("Couldn’t load this payment")).toBeVisible()
  await expectAttribution(page)
  await page.unroute("**/api/v1/public/**")
  await page.getByRole("button", { name: "Try again" }).click()
  await expect(pane(page)).toHaveAttribute("data-pane", "awaiting")
})

test("a cancelled payment says so on the checkout", async ({ customer, instance }) => {
  const { api } = instance
  const payment = await api.createPayment({ amount: "9", order_id: "ORD-CANCELLED" })
  const page = await customer()
  await page.goto(`${instance.url}/pay/${payment.id}`)
  await expect(pane(page)).toHaveAttribute("data-pane", "awaiting")
  await api.must(200, "POST", `/payments/${payment.id}/cancel`)
  // Live: the open page turns to "cancelled" on its own.
  await expect(pane(page)).toHaveAttribute("data-pane", "cancelled", { timeout: 30_000 })
  await expect(page.getByText("This payment was cancelled")).toBeVisible()
  await expectAttribution(page)
})

test("a logo on another host is shown as an image and never fetched by script, wave included", async ({ customer, instance }) => {
  const { stack, api } = instance
  const settings = await api.must<{ public_url: string; merchant: { name: string; logo_url: string; support_email: string; support_url: string } }>(200, "GET", "/settings")
  const merchant = { merchant_name: settings.merchant.name, support_email: settings.merchant.support_email, support_url: settings.merchant.support_url, public_url: settings.public_url }
  // The webhook receiver's port stands in for the merchant's own site: another origin.
  const logo = stack.hookUrl.replace("/hook", "/logo.png")
  await api.must(200, "PUT", "/settings/merchant", { ...merchant, logo_url: logo })
  try {
    const payment = await api.createPayment({ amount: "7", order_id: "ORD-LOGO" })
    const page = await customer()
    const toLogoHost: string[] = []
    page.on("request", (request) => {
      if (new URL(request.url()).host === new URL(logo).host) toLogoHost.push(request.resourceType())
    })
    await page.goto(`${instance.url}/pay/${payment.id}`)
    await expect(pane(page)).toHaveAttribute("data-pane", "awaiting")
    await expect(page.locator("[data-slot=merchant-avatar] img").first()).toHaveAttribute("src", logo)
    await expect(page.locator("[data-slot=merchant-avatar] img").first()).toHaveAttribute("referrerpolicy", "no-referrer")
    await page.mouse.click(5, 5)

    // Paid while watching: the wave runs (it takes a picture of the card), and still nothing but <img> asks that host.
    await stack.pay(payment.address, "7")
    await expect(page.locator("[data-checkout]")).toHaveAttribute("data-status", "paid", { timeout: 90_000 })
    await page.waitForTimeout(2500)
    expect(toLogoHost.length).toBeGreaterThan(0)
    expect([...new Set(toLogoHost)]).toEqual(["image"])
    // Nothing tried and was refused, either: the policy logged no violation during the wave.
    expect(expectedErrors(page.context(), /Content Security Policy/)).toEqual([])

    // And had something tried, the policy would have refused it: the page may connect to its own origin only.
    const refused = await page.evaluate(async (url) => {
      try {
        await fetch(url)
        return "allowed"
      } catch {
        return "refused"
      }
    }, logo)
    expect(refused).toBe("refused")
    expect(expectedErrors(page.context(), /Content Security Policy/).length).toBeGreaterThan(0)
    // A frame with a source is refused too (the vendored effect only ever makes a src-less one).
    const framed = await page.evaluate(
      (url) =>
        new Promise<string>((resolve) => {
          document.addEventListener("securitypolicyviolation", (event) => resolve(event.violatedDirective), { once: true })
          const frame = document.createElement("iframe")
          frame.src = url
          document.body.appendChild(frame)
          setTimeout(() => resolve("loaded"), 3000)
        }),
      logo,
    )
    expect(framed).toMatch(/^frame-src/)
    expectedErrors(page.context(), /Content Security Policy|frame/i)
  } finally {
    await api.must(200, "PUT", "/settings/merchant", { ...merchant, logo_url: "" })
  }
})
