import { OPERATOR } from "./harness"
import { expect, expectAttribution, test } from "./fixtures"

test("cancelling is refused once funds have arrived, and the page says why", async ({ operatorPage: page, instance }) => {
  const { stack, api } = instance
  const payment = await api.createPayment({ amount: "25", order_id: "ORD-CANCEL" })
  await page.goto(`${instance.url}/payments/${payment.id}`)
  await expect(page.getByRole("button", { name: "Cancel payment" })).toBeVisible()

  // Freeze what the page knows (as if it had not refreshed yet), then let money arrive.
  await page.route("**/api/v1/payments/*/detail", (route) => route.abort())
  await page.route("**/api/v1/events*", (route) => route.abort())
  await stack.pay(payment.address, "10")
  await api.waitForStatus(payment.id, ["detected"])

  await page.getByRole("button", { name: "Cancel payment" }).click()
  const dialog = page.getByRole("alertdialog")
  await dialog.getByRole("button", { name: "Cancel payment" }).click()
  await expect(dialog.getByText("This payment can’t be cancelled")).toBeVisible()
  await expect(dialog.getByText("Funds have already arrived for this payment")).toBeVisible()
  await dialog.getByRole("button", { name: "Close" }).click()

  // Nothing was cancelled; with the page live again it shows the partial payment and no cancel button.
  await page.unroute("**/api/v1/payments/*/detail")
  await page.unroute("**/api/v1/events*")
  expect((await api.payment(payment.id)).status).toBe("detected")
  await expect(page.getByText("Still due")).toBeVisible({ timeout: 30_000 })
  await expect(page.getByRole("button", { name: "Cancel payment" })).toHaveCount(0)
  await expect(page.getByRole("heading", { name: "Transfers" })).toBeVisible()
})

test("an awaiting payment can be cancelled; an overpayment is resolved with a note and reopened", async ({ operatorPage: page, instance }) => {
  const { stack, api } = instance
  const open = await api.createPayment({ amount: "11", order_id: "ORD-DROP" })
  await page.goto(`${instance.url}/payments/${open.id}`)
  await page.getByRole("button", { name: "Cancel payment" }).click()
  await page.getByRole("alertdialog").getByRole("button", { name: "Cancel payment" }).click()
  await expect(page.getByText("Cancelled", { exact: true }).first()).toBeVisible()
  const cancelled = await api.payment(open.id)
  expect(cancelled.status).toBe("expired")
  expect(cancelled.cancelled_at).not.toBeNull()

  const over = await api.createPayment({ amount: "20", order_id: "ORD-RESOLVE" })
  await stack.pay(over.address, "21")
  await api.waitForStatus(over.id, ["overpaid"])
  await page.goto(`${instance.url}/payments/${over.id}`)
  await expect(page.getByText(/Over by\s*1\.00\s*USDT/)).toBeVisible()
  await page.getByLabel("Resolution note").fill("Refunded 1 USDT from the treasury wallet")
  await page.getByRole("button", { name: "Mark as resolved" }).click()
  await expect(page.getByText("Resolved", { exact: true })).toBeVisible()
  await expect(page.getByText("Refunded 1 USDT from the treasury wallet").first()).toBeVisible()
  let now = await api.payment(over.id)
  expect(now.needs_attention).toBe(false)
  expect((now.resolution as { note: string; by: string }).note).toBe("Refunded 1 USDT from the treasury wallet")

  await page.getByRole("button", { name: "Reopen" }).click()
  await expect(page.getByRole("button", { name: "Mark as resolved" })).toBeVisible()
  now = await api.payment(over.id)
  expect(now.needs_attention).toBe(true)

  // It is under "Needs attention" in the list, which the server filters.
  await page.goto(`${instance.url}/payments?status=attention`)
  await expect(page.getByRole("link", { name: "ORD-RESOLVE" }).first()).toBeVisible()
  await expect(page.getByRole("link", { name: "ORD-DROP" })).toHaveCount(0)
  // Search is the server's too.
  await page.goto(`${instance.url}/payments?q=ORD-DROP`)
  await expect(page.getByRole("link", { name: "ORD-DROP" }).first()).toBeVisible()
  await expect(page.getByRole("link", { name: "ORD-RESOLVE" })).toHaveCount(0)
})

test("the node status says who reads the chain and who checks it", async ({ operatorPage: page, instance }) => {
  const { stack } = instance
  const [primary, verifier] = stack.rpcUrls.map((url) => new URL(url).host)
  await page.goto(instance.url + "/")
  const indicator = page.locator("[data-node-state]")
  await expect(indicator).toHaveAttribute("data-node-state", /^(Synced|Idle|Quiet)$/, { timeout: 60_000 })
  await expect(indicator).toContainText("BNB Smart Chain")
  await expect(indicator).toContainText(/#\d+/)

  await indicator.click()
  await expect(page).toHaveURL(/\/settings\/network$/)
  const panel = page.locator("[data-slot=node-status]")
  await expect(panel).toHaveAttribute("data-state", /^(Synced|Idle|Quiet)$/)
  await expect(panel).toContainText("Chain ID 56")
  // Two endpoints: one is read from, the other verifies it.
  const roles = page.locator("[data-slot=node-roles]")
  const hosts = [primary, verifier]
  const shown = [await roles.locator("[data-role=primary]").innerText(), await roles.locator("[data-role=verifier]").innerText()]
  expect(hosts).toContain(shown[0])
  expect(hosts).toContain(shown[1])
  expect(shown[0]).not.toBe(shown[1])
  const rows = page.locator("[data-slot=rpc-endpoints] > li")
  await expect(rows).toHaveCount(2)
  await expect(rows.filter({ hasText: "Primary" })).toHaveCount(1)
  await expect(rows.filter({ hasText: "Verifier" })).toHaveCount(1)
  await expect(rows.first()).toHaveAttribute("data-state", "healthy")
  // What the API says is what the page says.
  const node = await instance.api.must<{ primary: string; verifier: string; healthy: boolean; starting: boolean; hold: string }>(200, "GET", "/node-status")
  expect(node).toMatchObject({ healthy: true, starting: false, hold: "" })
  expect(new URL(node.primary).host).toBe(shown[0])

  // A test of an endpoint goes through the server and reports what it answered.
  await rows.first().getByRole("button", { name: /Actions for/ }).click()
  await page.getByRole("menuitem", { name: "Test now" }).click()
  await expect(page.getByText(/answered in \d+ ms/)).toBeVisible()

  // The wallet page shows the key and the first address to compare.
  await page.goto(instance.url + "/settings/wallet")
  await expect(page.locator('[data-derived="0"]')).toContainText("0xf39f")
  await expect(page.locator("[data-slot=next-address-index]")).toHaveText(/\d+/)
  await expect(page.getByText(/sweep/i)).toHaveCount(0)
})

test("API keys: created once, usable, revoked", async ({ operatorPage: page, instance }) => {
  await page.goto(instance.url + "/settings/api-keys")
  await page.getByRole("button", { name: "Create key" }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Name").fill("Storefront checkout")
  await dialog.getByRole("button", { name: "Create key" }).click()
  await expect(dialog.getByText("Save your key now")).toBeVisible()
  const secret = (await dialog.locator(".select-all").innerText()).trim()
  expect(secret).toMatch(/^hmn_/)

  // The key works: an integration can list payments with it (and nothing it was not given).
  const withKey = (route: string, init: RequestInit = {}) =>
    fetch(`${instance.url}/api/v1${route}`, { ...init, headers: { authorization: `Bearer ${secret}`, "x-forwarded-for": "10.66.0.1", ...init.headers } })
  expect((await withKey("/payments")).status).toBe(200)
  expect((await withKey("/api-keys")).status).toBe(403)

  await dialog.getByRole("button", { name: "I’ve saved it" }).click()
  // Shown once: the list has the masked key only.
  const row = page.locator("[data-key]").filter({ hasText: "Storefront checkout" })
  await expect(row).toBeVisible()
  await expect(page.getByText(secret)).toHaveCount(0)
  await expect(row).toContainText("Active")

  await row.getByRole("button", { name: "Actions for Storefront checkout" }).click()
  await page.getByRole("menuitem", { name: "Revoke" }).click()
  await page.getByRole("alertdialog").getByRole("button", { name: "Revoke key" }).click()
  await expect(row).toContainText("Revoked")
  expect((await withKey("/payments")).status).toBe(401)
})

test("webhooks: add an endpoint, send a test, reveal and rotate the secret, see a payment's delivery with its seq", async ({ operatorPage: page, instance }) => {
  const { stack, api } = instance
  stack.hooks.length = 0
  await page.goto(instance.url + "/settings/webhooks")
  await page.getByRole("button", { name: /Add/ }).first().click()
  const add = page.getByRole("dialog")
  await add.getByLabel("Endpoint URL").fill("ftp://not-a-webhook")
  await expect(add.getByRole("button", { name: "Add endpoint" })).toBeDisabled()
  await add.getByLabel("Endpoint URL").fill(stack.hookUrl)
  await add.getByLabel(/^Name/).fill("Order service")
  await add.getByRole("button", { name: "Add endpoint" }).click()
  await expect(add.getByText("Endpoint added")).toBeVisible()
  const created = (await add.locator("[data-slot=new-secret]").innerText()).trim()
  expect(created).toMatch(/^whsec_[A-Za-z0-9_-]{16,}$/)
  await add.getByRole("button", { name: "Done" }).click()

  // The list never carries the secret itself.
  const row = page.locator("[data-endpoint]").filter({ hasText: "Order service" })
  await expect(row).toBeVisible()
  await expect(page.getByText(created)).toHaveCount(0)

  // Test delivery: sent for real, answered by the receiver, shown with its result.
  await row.getByRole("button").click()
  const sheet = page.getByRole("dialog")
  await expect(sheet.locator("[data-slot=signing-secret]")).toContainText("whsec_…")
  await sheet.getByRole("button", { name: "Send test" }).click()
  const delivery = page.locator("[data-delivery-sheet]")
  await expect(delivery).toHaveAttribute("data-delivery-sheet", "succeeded", { timeout: 30_000 })
  await expect(delivery.getByText("webhook.test").first()).toBeVisible()
  await expect(delivery.locator("[data-slot=attempts] li")).toHaveCount(1)
  await expect(delivery.getByText("from your server")).toBeVisible()
  await expect(delivery.getByText("None (test events are not in the log)")).toBeVisible()
  await expect.poll(() => stack.hooks.length).toBeGreaterThan(0)
  const hit = stack.hooks[0]
  expect(JSON.parse(hit.body).type).toBe("webhook.test")
  expect(String(hit.headers["hmnpay-signature"])).toMatch(/^t=\d+,v1=[0-9a-f]{64}$/)
  await page.keyboard.press("Escape")
  // Every sheet has gone, the closing one included: a click on a sheet that is still animating
  // out reaches nothing (seen on Linux: no reveal request was sent).
  await expect(page.getByRole("dialog")).toHaveCount(0)

  // Reveal asks the server; rotate gives a new secret.
  await row.getByRole("button").click()
  await expect(sheet).toHaveCount(1)
  await expect(sheet.locator("[data-slot=signing-secret]")).toContainText("whsec_…")
  await sheet.getByRole("button", { name: "Reveal secret" }).click()
  await expect(sheet.locator("[data-slot=signing-secret]")).toHaveText(created)
  await sheet.getByRole("button", { name: "Rotate" }).click()
  await page.getByRole("alertdialog").getByRole("button", { name: "Rotate secret" }).click()
  await expect(sheet.locator("[data-slot=signing-secret]")).not.toHaveText(created)
  const rotated = (await sheet.locator("[data-slot=signing-secret]").innerText()).trim()
  expect(rotated).toMatch(/^whsec_/)
  await expect(sheet.getByText("signed with the previous secret too until")).toBeVisible()
  await page.keyboard.press("Escape")

  // A real payment: its events are delivered, and the delivery shows the event's seq.
  const payment = await api.createPayment({ amount: "6", order_id: "ORD-HOOK" })
  await stack.pay(payment.address, "6")
  await api.waitForStatus(payment.id, ["paid"])
  await expect.poll(() => stack.hooks.some((h) => JSON.parse(h.body).type === "payment.paid"), { timeout: 30_000 }).toBe(true)
  const paid = stack.hooks.map((h) => JSON.parse(h.body)).find((event) => event.type === "payment.paid")
  expect(paid.data.id).toBe(payment.id)
  const paidRow = page.locator("[data-delivery]").filter({ hasText: "payment.paid" }).first()
  await expect(paidRow).toBeVisible({ timeout: 30_000 })
  await paidRow.getByRole("button").click()
  await expect(page.locator("[data-slot=event-seq]")).toHaveText(String(paid.seq))
  await page.getByRole("button", { name: "Resend" }).click()
  await expect.poll(() => stack.hooks.filter((h) => JSON.parse(h.body).type === "payment.paid").length, { timeout: 30_000 }).toBeGreaterThan(1)
  // A resend keeps the event id and the seq.
  const resent = stack.hooks.map((h) => JSON.parse(h.body)).filter((event) => event.type === "payment.paid")
  expect(new Set(resent.map((event) => `${event.id}:${event.seq}`)).size).toBe(1)
})

test("sessions are listed and can be signed out; the password can be changed", async ({ operatorPage: page, instance, browser }) => {
  await page.goto(instance.url + "/settings")
  const sessions = page.locator("[data-slot=sessions] > li")
  await expect(sessions.filter({ hasText: "This browser" })).toHaveCount(1)
  await expect(page.locator("[data-slot=about-version]")).toContainText("dev")
  await expectAttribution(page) // once in the dashboard: the sidebar

  // A second session, signed out from the first.
  const other = await browser.newContext()
  const otherPage = await other.newPage()
  await otherPage.goto(instance.url + "/login")
  await otherPage.getByLabel("Email").fill(OPERATOR.email)
  await otherPage.getByRole("textbox", { name: "Password" }).fill(OPERATOR.password)
  await otherPage.getByRole("button", { name: "Sign in" }).click()
  await expect(otherPage.getByRole("heading", { name: "Overview" })).toBeVisible()
  await page.reload()
  await expect(sessions).toHaveCount(2)
  await sessions.filter({ hasNotText: "This browser" }).getByRole("button", { name: "Sign out" }).click()
  await expect(sessions).toHaveCount(1)
  // The signed-out browser is sent to the login on its next request.
  await otherPage.goto(instance.url + "/payments")
  await expect(otherPage).toHaveURL(/\/login/)
  await other.close()

  // A wrong current password is refused on its field.
  await page.getByLabel("Current password").fill("wrong wrong wrong wrong")
  await page.getByLabel("New password").fill("another long passphrase 99")
  await page.getByRole("button", { name: "Change password" }).click()
  await expect(page.getByText("Not saved").first()).toBeVisible()
})
