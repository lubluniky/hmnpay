import { expect, expectAttribution, pane, test } from "./fixtures"

test("a fixed-price link: created in the dashboard, opened by a customer, turned into a checkout", async ({ operatorPage: page, customer, instance }) => {
  await page.goto(instance.url + "/payment-links")
  await page.getByRole("button", { name: "Create link" }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Name").fill("Pro plan, annual")
  await dialog.getByLabel("Amount").fill("12.5")
  await dialog.getByRole("button", { name: "Create link" }).click()
  await expect(dialog.getByText("Link created")).toBeVisible()
  const links = await instance.api.must<{ data: { slug: string; url: string; amount: string }[] }>(200, "GET", "/payment-links")
  const link = links.data.find((item) => item.slug === "pro-plan-annual")!
  expect(link.amount).toBe("12.50")
  expect(link.url).toBe(`${instance.url}/l/pro-plan-annual`)

  // The same slug again is refused by the server, on the slug field.
  await dialog.getByRole("button", { name: /another/i }).click()
  await dialog.getByLabel("Name").fill("Pro plan, annual")
  await dialog.getByLabel("Amount").fill("3")
  await dialog.getByRole("button", { name: "Create link" }).click()
  await expect(dialog.getByRole("alert").first()).toBeVisible()
  await page.keyboard.press("Escape")

  const visitor = await customer()
  await visitor.goto(link.url)
  await expect(visitor.getByRole("heading", { name: "Pro plan, annual" })).toBeVisible()
  await expect(visitor.locator("[data-slot=amount]").first()).toHaveText("12.50USDT")
  await expectAttribution(visitor)
  await visitor.getByRole("button", { name: /^Pay/ }).click()
  await expect(visitor).toHaveURL(/\/pay\/pay_[0-9a-z]{26}$/)
  await expect(pane(visitor)).toHaveAttribute("data-pane", "awaiting")
  await expect(visitor.getByLabel("Order summary").locator("[data-slot=amount]").first()).toHaveText("12.50USDT")
  await expectAttribution(visitor)

  // The dashboard shows the checkout among the link's payments.
  await page.goto(instance.url + "/payment-links")
  await page.getByRole("button", { name: "Pro plan, annual" }).first().click().catch(async () => {
    await page.getByText("Pro plan, annual").first().click()
  })
  await expect(page.getByRole("heading", { name: "Recent payments" })).toBeVisible()
  await expect(page.getByRole("dialog").getByText("Awaiting")).toBeVisible()
})

test("a link where the customer chooses the amount enforces its minimum", async ({ customer, instance }) => {
  const { api } = instance
  await api.must(201, "POST", "/payment-links", { name: "Donate", slug: "donate", amount: null, min_amount: "5" })
  const page = await customer()
  await page.goto(`${instance.url}/l/donate`)
  await expect(page.getByText("You choose the amount")).toBeVisible()
  await expect(page.getByText("Minimum")).toBeVisible()
  await expectAttribution(page)

  // Below the minimum: said before asking the server.
  await page.getByLabel("Amount").fill("2")
  await page.getByRole("button", { name: "Pay 2.00 USDT" }).click()
  await expect(page.getByText("The minimum is 5.00 USDT.")).toBeVisible()
  await expect(page).toHaveURL(/\/l\/donate$/)

  // The server refuses it too (422 too_small), for a client that does not check.
  const refused = await api.call<{ error: { fields: { field: string; code: string }[] } }>("POST", "/public/links/donate/payments", { amount: "2" }, { "x-forwarded-for": "10.88.0.1" })
  expect(refused.status).toBe(422)
  expect(refused.body.error.fields[0]).toMatchObject({ field: "amount", code: "too_small" })

  await page.getByLabel("Amount").fill("7.25")
  await page.getByLabel("Email").fill("donor@example.com")
  await page.getByRole("button", { name: "Pay 7.25 USDT" }).click()
  await expect(page).toHaveURL(/\/pay\/pay_/)
  await expect(page.getByLabel("Order summary").locator("[data-slot=amount]").first()).toHaveText("7.25USDT")
  const id = page.url().split("/").pop()!
  expect((await api.payment(id)).customer_email).toBe("donor@example.com")
})

test("a limited link gives ten minutes and is used up while its place is held", async ({ customer, instance }) => {
  const { api } = instance
  await api.must(201, "POST", "/payment-links", { name: "Last ticket", slug: "last-ticket", amount: "30", max_uses: 1 })
  const first = await customer()
  await first.goto(`${instance.url}/l/last-ticket`)
  await first.getByRole("button", { name: /^Pay/ }).click()
  await expect(pane(first)).toHaveAttribute("data-pane", "awaiting")
  // The checkout of a limited link expires in ten minutes, not in the usual payment window.
  await expect(first.getByRole("timer")).toHaveText(/Expires in\s*(09:[0-5]\d|10:00)/)
  const id = first.url().split("/").pop()!
  const payment = await api.payment(id)
  const window = Date.parse(payment.expires_at as string) - Date.parse(payment.created_at as string)
  expect(window).toBe(10 * 60 * 1000)

  // Somebody else, a moment later: the one place is taken.
  const second = await customer()
  await second.goto(`${instance.url}/l/last-ticket`)
  await expect(second.getByText("This link has been used up")).toBeVisible()
  await expect(second.locator("[data-link-page]")).toHaveAttribute("data-status", "limit")
  await expectAttribution(second)
  const refused = await api.call<{ error: { code: string } }>("POST", "/public/links/last-ticket/payments", {}, { "x-forwarded-for": "10.88.0.2" })
  expect(refused.status).toBe(409)
  expect(refused.body.error.code).toBe("link_limit_reached")
})

test("a link taken from under the customer, a switched-off link and a missing one", async ({ customer, instance }) => {
  const { api } = instance
  const link = await api.must<{ id: string }>(201, "POST", "/payment-links", { name: "Workshop seat", slug: "workshop", amount: "40" })
  const page = await customer()
  await page.goto(`${instance.url}/l/workshop`)
  await expect(page.getByRole("button", { name: /^Pay/ })).toBeVisible()
  // The merchant switches it off while the page is open: the button gets the server's refusal.
  await api.must(200, "POST", `/payment-links/${link.id}/deactivate`)
  await page.getByRole("button", { name: /^Pay/ }).click()
  await expect(page.getByText("This link is no longer available")).toBeVisible()
  await expect(page).toHaveURL(/\/l\/workshop$/)
  await expectAttribution(page)

  await page.goto(`${instance.url}/l/no-such-link`)
  await expect(page.getByText("This payment link doesn’t exist")).toBeVisible()
  await expectAttribution(page)
})

test("a link is edited: name, price, limit and end date change; its address does not", async ({ operatorPage: page, customer, instance }) => {
  const { api } = instance
  type Link = { id: string; slug: string; url: string; name: string; description: string; amount: string | null; min_amount: string | null; max_uses: number | null; expires_at: string | null; redirect_url: string; active: boolean }
  const slug = `edit-${Date.now().toString(36)}`
  const made = await api.must<Link>(201, "POST", "/payment-links", { slug, name: "Evening class seat", amount: "40", description: "One seat." })

  await page.goto(instance.url + "/payment-links")
  // From the row's menu: the form opens filled in with what the link has.
  await page.getByRole("button", { name: "Actions for Evening class seat" }).click()
  await page.getByRole("menuitem", { name: "Edit" }).click()
  const dialog = page.getByRole("dialog").filter({ hasText: "Edit payment link" })
  await expect(dialog.getByLabel("Name")).toHaveValue("Evening class seat")
  await expect(dialog.getByLabel("Amount")).toHaveValue("40.00")
  await expect(dialog.getByLabel("Description")).toHaveValue("One seat.")
  // The ending of the link is shown and cannot be typed over.
  const ending = dialog.locator("#link-slug")
  await expect(ending).toHaveValue(slug)
  await expect(ending).toHaveAttribute("readonly", "")
  await expect(dialog).toContainText("The address of a link never changes")

  // What the form can tell is wrong it says before asking the server.
  await dialog.getByLabel("Name").fill("")
  await dialog.getByRole("button", { name: "Save changes" }).click()
  await expect(dialog.getByRole("alert").filter({ hasText: "Give the link a name" })).toBeVisible()

  await dialog.getByLabel("Name").fill("Evening class seat, spring")
  await dialog.getByLabel("Amount").fill("55.5")
  await dialog.getByLabel("Description").fill("One seat, lunch included.")
  await dialog.getByRole("switch", { name: /Limit payments/ }).click()
  await dialog.getByLabel("Maximum payments").fill("3")
  await dialog.getByRole("switch", { name: /Set an end date/ }).click()
  await dialog.getByLabel("Last day").fill("2031-05-20")
  await dialog.getByRole("button", { name: "Save changes" }).click()
  await expect(dialog).toHaveCount(0)
  await expect(page.getByText("Link saved")).toBeVisible()

  const saved = await api.must<Link>(200, "GET", `/payment-links/${made.id}`)
  expect(saved).toMatchObject({ slug, url: made.url, name: "Evening class seat, spring", amount: "55.50", description: "One seat, lunch included.", max_uses: 3, expires_at: "2031-05-20T23:59:59Z", active: true })
  // The list shows the change without a reload.
  const row = page.getByRole("row").filter({ hasText: "Evening class seat, spring" })
  await expect(row.locator("[data-slot=amount]").first()).toHaveText("55.50")
  await expect(row).toContainText("0/3")

  // The same address now sells at the new price.
  const visitor = await customer()
  await visitor.goto(made.url)
  await expect(visitor.getByRole("heading", { name: "Evening class seat, spring" })).toBeVisible()
  await expect(visitor.locator("[data-slot=amount]").first()).toHaveText("55.50USDT")

  // From the link's sheet: to "customer chooses" with a minimum; the limit and end date are taken off.
  await row.getByRole("button", { name: "Evening class seat, spring", exact: true }).click()
  await page.getByRole("dialog").getByRole("button", { name: "Edit link" }).click()
  await expect(dialog.getByLabel("Maximum payments")).toHaveValue("3")
  await expect(dialog.getByLabel("Last day")).toHaveValue("2031-05-20")
  await dialog.getByRole("tab", { name: "Customer chooses" }).click()
  // The server's floor of one whole unit, said on the field.
  await dialog.getByLabel("Minimum amount").fill("0.5")
  await dialog.getByRole("button", { name: "Save changes" }).click()
  await expect(dialog.getByRole("alert").filter({ hasText: /can’t be less than 1/ })).toBeVisible()
  await dialog.getByLabel("Minimum amount").fill("5")
  await dialog.getByRole("switch", { name: /Limit payments/ }).click()
  await dialog.getByRole("switch", { name: /Set an end date/ }).click()
  await dialog.getByRole("button", { name: "Save changes" }).click()
  await expect(dialog).toHaveCount(0)
  const opened = await api.must<Link>(200, "GET", `/payment-links/${made.id}`)
  expect(opened).toMatchObject({ slug, amount: null, min_amount: "5.00", max_uses: null, expires_at: null, name: "Evening class seat, spring" })

  // A switched-off link stays switched off through an edit.
  await api.must(200, "POST", `/payment-links/${made.id}/deactivate`)
  // (A plain visit: the address still carries ?link=, which would reopen the sheet.)
  await page.goto(instance.url + "/payment-links")
  await page.getByRole("button", { name: "Actions for Evening class seat, spring" }).click()
  await page.getByRole("menuitem", { name: "Edit" }).click()
  await dialog.getByLabel("Name").fill("Evening class seat, autumn")
  await dialog.getByRole("button", { name: "Save changes" }).click()
  await expect(dialog).toHaveCount(0)
  expect(await api.must<Link>(200, "GET", `/payment-links/${made.id}`)).toMatchObject({ name: "Evening class seat, autumn", active: false })
})
