import type { Page } from "@playwright/test"

import { expect, expectAttribution, pane, test } from "./fixtures"

// A payment is paid only once the chain has finalised its block. anvil finalises the block 64
// below its tip (1-second blocks), while the 5 confirmations these tests ask for come some 20
// blocks after the payment's block: so every payment here spends about 45 seconds with all its
// confirmations, waiting only for finality — the "finalising" state, which on BNB Smart Chain,
// whose finality trails the tip by two or three blocks, is over before the confirmations are.

/** The payer's finalising pane: received, nothing to do; no block numbers, no 5/5, no jargon, no error. */
async function expectPayerFinalising(page: Page, required: number) {
  await expect(pane(page)).toHaveAttribute("data-pane", "finalising", { timeout: 60_000 })
  const card = page.locator("[data-checkout]")
  await expect(page.getByRole("heading", { name: "Payment received" })).toBeVisible()
  await expect(card).toContainText("has all the confirmations it needs")
  await expect(card).toContainText("there is nothing more for you to do")
  await expect(card).toContainText("You can close this page")
  await expect(card.locator("[data-finalising]").first()).toHaveText("Final check")
  await expect(card).toContainText("Final check by the network")
  const text = await card.innerText()
  expect(text).not.toMatch(/finali[sz]|\bblock\b|error|failed/i)
  expect(text).not.toContain(`${required}/${required}`)
  expect(text).not.toContain(`${required} / ${required}`)
  await expect(card.getByRole("alert")).toHaveCount(0)
  await expectAttribution(page)
}

test("a payment with its confirmations waits for finality: Finalising for the operator, a calm wait for the payer", async ({ customer, operatorPage, instance }) => {
  test.setTimeout(300_000)
  const { stack, api, url } = instance
  const payment = await api.createPayment({ amount: "21", order_id: "ORD-FINAL", description: "Finality test" })

  // The payer on a desktop (with a clock the test can move on) and on a phone.
  const desktop = await customer()
  await desktop.clock.install()
  await desktop.goto(`${url}/pay/${payment.id}`)
  const phone = await customer({ viewport: { width: 390, height: 844 } })
  await phone.goto(`${url}/pay/${payment.id}`)
  await expect(pane(desktop)).toHaveAttribute("data-pane", "awaiting")
  await expect(pane(phone)).toHaveAttribute("data-pane", "awaiting")
  await operatorPage.goto(`${url}/payments`)
  const row = operatorPage.locator("tr", { has: operatorPage.getByRole("link", { name: "ORD-FINAL" }) })
  await expect(row).toBeVisible()

  await stack.pay(payment.address, "21")
  // The API first: confirming, every confirmation in, and the block it waits for above the final one.
  await expect.poll(async () => (await api.payment(payment.id)).awaiting_finality, { timeout: 90_000 }).not.toBeNull()
  const waiting = await api.payment(payment.id)
  expect(waiting.status).toBe("confirming")
  expect(waiting.confirmations).toBeGreaterThanOrEqual(waiting.required_confirmations)
  expect(waiting.awaiting_finality!.block).toBeGreaterThan(waiting.awaiting_finality!.final)
  const required = waiting.required_confirmations

  // The payer, on both screens.
  await expectPayerFinalising(desktop, required)
  await expectPayerFinalising(phone, required)

  // The operator's list: "Finalising", not a counter stuck at 5/5.
  const badge = row.locator("[data-finalising]")
  await expect(badge).toHaveText("Finalising")
  await expect(row).not.toContainText(`${required}/${required}`)

  // The detail names the reason, with the block numbers; so does the timeline.
  await operatorPage.goto(`${url}/payments/${payment.id}`)
  await expect(operatorPage.locator("[data-finalising]").first()).toHaveText("Finalising")
  await expect(operatorPage.locator("[data-slot=finalising]")).toContainText(/All 5 confirmations are in\. Waiting for the chain to finalise block [\d,]+; finalised so far: [\d,]+\./)
  await expect(operatorPage.getByText(/^Waiting for the chain to finalise block [\d,]+; finalised so far: [\d,]+\.$/)).toBeVisible()
  const transfer = operatorPage.locator("li", { has: operatorPage.locator("[data-finalising]") })
  await expect(transfer.locator("[data-finalising]")).toHaveText("Finalising")
  await expect(transfer).not.toContainText(`${required}/${required}`)

  // When the wait lasts (two minutes on the payer's clock), one sentence more — still no error.
  await expect(desktop.locator("[data-slot=finalising-longer]")).toHaveCount(0)
  await desktop.clock.fastForward("02:01")
  const longer = desktop.locator("[data-slot=finalising-longer]")
  await expect(longer).toBeVisible()
  await expect(longer).toHaveText("The network is taking longer than usual to finish. Your payment is safe, and there is still nothing you need to do.")
  await expect(pane(desktop)).toHaveAttribute("data-pane", "finalising")
  await expect(desktop.locator("[data-checkout]").getByRole("alert")).toHaveCount(0)

  // Finality reaches the block: paid, everywhere, by itself.
  await expect(desktop.locator("[data-checkout]")).toHaveAttribute("data-status", "paid", { timeout: 90_000 })
  await expect(phone.locator("[data-checkout]")).toHaveAttribute("data-status", "paid", { timeout: 30_000 })
  await expect(desktop.getByText("Payment complete").last()).toBeVisible()
  await expect(operatorPage.locator("[data-slot=finalising]")).toHaveCount(0, { timeout: 30_000 })
  const paid = await api.payment(payment.id)
  expect(paid.status).toBe("paid")
  expect(paid.awaiting_finality).toBeNull()
})
