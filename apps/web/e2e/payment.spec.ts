import { expect, expectAttribution, FINAL_MS, pane, test } from "./fixtures"

/**
 * The main road: the operator creates a payment in the dashboard, a customer pays it on-chain,
 * the checkout follows it live to "paid", and the dashboard shows it.
 */
test("create a payment, pay it on-chain, watch it confirm, see it in the dashboard", async ({ operatorPage: page, customer, instance }) => {
  const { stack, api } = instance
  await page.goto(instance.url + "/payments")
  await expect(page.getByRole("heading", { name: "Payments", exact: true })).toBeVisible()

  // Create it in the dialog. A server-side validation error lands on its field.
  await page.getByRole("button", { name: "Create payment" }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Amount").fill("25")
  await dialog.getByLabel("Order ID").fill("ORD-E2E-1")
  await dialog.getByLabel("Description").fill("End-to-end test order")
  await dialog.getByLabel("Customer email").fill("not an email")
  await dialog.getByRole("button", { name: "Create payment" }).click()
  await expect(dialog.getByRole("alert")).toBeVisible()
  await dialog.getByLabel("Customer email").fill("maria@example.com")
  await dialog.getByRole("button", { name: "Create payment" }).click()
  await expect(dialog.getByText("Payment created")).toBeVisible()
  const link = (await dialog.locator("span[title^='http']").getAttribute("title"))!
  expect(link).toMatch(new RegExp(`^${instance.url}/pay/pay_[0-9a-z]{26}$`))
  const id = link.split("/").pop()!
  await dialog.getByRole("button", { name: "Close" }).click()

  // The customer opens the checkout: awaiting, the exact amount, the deposit address, live.
  const checkout = await customer()
  await checkout.goto(link)
  await expect(pane(checkout)).toHaveAttribute("data-pane", "awaiting")
  await expect(checkout.locator("[data-checkout]")).toHaveAttribute("data-live", "stream")
  await expect(checkout.getByRole("heading", { name: "End-to-end test order" })).toBeVisible()
  await expect(checkout.getByText("ORD-E2E-1")).toBeVisible()
  await expect(checkout.getByLabel("Order summary").locator("[data-slot=amount]").first()).toHaveText("25.00USDT")
  const payment = await api.payment(id)
  await expect(checkout.locator("[data-slot=address-groups] .sr-only").first()).toHaveText(payment.address)
  await expect(checkout.getByRole("timer")).toBeVisible()
  await expectAttribution(checkout)

  // The dashboard lists it as awaiting.
  await expect(page.getByRole("link", { name: "ORD-E2E-1" }).first()).toBeVisible()

  // Pay on-chain. The checkout goes to confirming, counts confirmations up, and lands on paid.
  await stack.pay(payment.address, "25")
  await expect(pane(checkout)).toHaveAttribute("data-pane", "confirming", { timeout: 30_000 })
  await expect(checkout.getByText("Confirming your payment")).toBeVisible()
  const counter = checkout.locator("[data-confirmations]")
  const firstCount = Number(await counter.getAttribute("data-confirmations"))
  await expect.poll(async () => Number(await counter.getAttribute("data-confirmations").catch(() => "99")), { timeout: 30_000 }).toBeGreaterThan(firstCount)
  await expectAttribution(checkout)
  await expect(checkout.locator("[data-checkout]")).toHaveAttribute("data-status", "paid", { timeout: FINAL_MS })
  await expect(checkout.getByText("Payment complete").last()).toBeVisible()
  await expect(checkout.getByText("Amount paid").last()).toBeVisible()
  await expectAttribution(checkout)

  // Reloading a paid checkout shows the receipt at rest.
  await checkout.reload()
  await expect(pane(checkout)).toHaveAttribute("data-pane", "paid")
  await expect(checkout.getByText("Payment complete")).toBeVisible()

  // The dashboard follows on its own: the row turns Paid, and the detail has the transfer and the timeline.
  const row = page.locator("tr", { has: page.getByRole("link", { name: "ORD-E2E-1" }) })
  await expect(row.getByText("Paid")).toBeVisible({ timeout: 30_000 })
  await row.getByRole("link", { name: "ORD-E2E-1" }).click()
  await expect(page).toHaveURL(new RegExp(`/payments/${id}$`))
  await expect(page.getByRole("heading", { name: "ORD-E2E-1" })).toBeVisible()
  await expect(page.getByText("Requested")).toBeVisible()
  await expect(page.getByRole("heading", { name: "Transfers" })).toBeVisible()
  await expect(page.getByText("Confirmed", { exact: true }).first()).toBeVisible()
  const timeline = page.locator("ol").filter({ hasText: "Payment created" })
  await expect(timeline.getByText("Full amount seen")).toBeVisible()
  await expect(timeline.getByText("Paid", { exact: true })).toBeVisible()
  await expect(page.getByText("maria@example.com")).toBeVisible()

  // A direct load of the detail URL works too (the server answers every /payments/{id} with one page).
  await page.reload()
  await expect(page.getByRole("heading", { name: "ORD-E2E-1" })).toBeVisible()

  // The overview counts it.
  await page.goto(instance.url + "/")
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible()
  const overview = await api.must<{ current: { paid: number; volume: string } }>(200, "GET", "/overview?days=30")
  expect(overview.current.paid).toBeGreaterThanOrEqual(1)
  await expect(page.locator("[data-slot=stat]").filter({ hasText: "Paid payments" })).toContainText(String(overview.current.paid))
  await expect(page.getByRole("link", { name: "ORD-E2E-1" }).first()).toBeVisible()
})
