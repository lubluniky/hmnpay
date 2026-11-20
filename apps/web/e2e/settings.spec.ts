import { FIRST_ADDRESS, XPUB } from "./harness"
import { expect, test } from "./fixtures"

test("settings are saved through the API and take effect", async ({ operatorPage: page, customer, instance }) => {
  const { api } = instance
  const before = await api.must<{ merchant: { name: string; support_url: string }; payments: { required_confirmations: number; underpayment_tolerance: string; default_redirect_url: string } }>(200, "GET", "/settings")
  await page.goto(instance.url + "/settings")
  try {
  // Checkout identity: a new name shows on the checkout at once.
  const name = page.getByLabel("Business name")
  await expect(name).toHaveValue(before.merchant.name)
  await name.fill("Northwind Digital Ltd")
  await page.getByLabel(/^Support page/).fill("not a url")
  const identity = page.locator("[data-slot=settings-section]").filter({ hasText: "Checkout identity" })
  await identity.getByRole("button", { name: "Save" }).click()
  // The server's validation, on the field it is about.
  await expect(identity.locator("[data-slot=field-error]").first()).toBeVisible()
  await page.getByLabel(/^Support page/).fill("https://northwind.example.com/help")
  await identity.getByRole("button", { name: "Save" }).click()
  await expect(page.getByText("Saved").first()).toBeVisible()
  const payment = await api.createPayment({ amount: "5", order_id: "ORD-SETTINGS" })
  const checkout = await customer()
  await checkout.goto(`${instance.url}/pay/${payment.id}`)
  await expect(checkout.getByLabel("Order summary").getByText("Northwind Digital Ltd", { exact: true })).toBeVisible()

  // Payment defaults: tolerance and the return URL; a new payment carries them.
  const defaults = page.locator("[data-slot=settings-section]").filter({ hasText: "Payment defaults" })
  await defaults.getByLabel(/Underpayment tolerance/).fill("0.5")
  await defaults.getByLabel(/^Return URL/).fill("https://northwind.example.com/thanks")
  await defaults.getByRole("button", { name: "Save" }).click()
  await expect.poll(async () => (await api.must<typeof before>(200, "GET", "/settings")).payments.underpayment_tolerance).toBe("0.5")
  const next = await api.createPayment({ amount: "40" })
  expect(next.redirect_url).toBe("https://northwind.example.com/thanks")

  } finally {
    // Put things back for the tests that follow in this worker.
    await api.must(200, "PUT", "/settings/merchant", { merchant_name: "Northwind Digital", logo_url: "", support_email: "billing@northwind.example.com", support_url: "", public_url: instance.url })
    await api.must(200, "PUT", "/settings/payments", { ttl_seconds: 1800, required_confirmations: 5, underpayment_tolerance: "0", default_redirect_url: "" })
  }
})

test("RPC endpoints can be tested, added and removed; the watcher carries on", async ({ operatorPage: page, instance }) => {
  const { stack, api } = instance
  await page.goto(instance.url + "/settings/network")
  const rows = page.locator("[data-slot=rpc-endpoints] > li")
  await expect(rows).toHaveCount(2)

  await page.getByRole("button", { name: /^Add/ }).click()
  const dialog = page.getByRole("dialog")
  // Nothing listens there: the server's probe says so, and the endpoint cannot be added.
  await dialog.getByLabel("Endpoint URL").fill("http://127.0.0.1:1")
  await dialog.getByRole("button", { name: "Test connection" }).click()
  await expect(dialog.getByText(/Not usable/)).toBeVisible()
  await expect(dialog.getByRole("button", { name: "Add endpoint" })).toHaveCount(0)
  // Another URL of an endpoint in the list (only the query differs) is not a third endpoint: the
  // dialog says so before anything is tested.
  await dialog.getByLabel("Endpoint URL").fill(`${stack.rpcUrl}/?name=third`)
  await expect(dialog.locator("[data-slot=same-endpoint]")).toBeVisible()
  await expect(dialog.getByRole("button", { name: "Test connection" })).toBeDisabled()
  // A third endpoint (a relay of the same node on a port of its own) answers for the right chain and token.
  const extra = stack.relay(5).url
  await dialog.getByLabel("Endpoint URL").fill(extra)
  await dialog.getByRole("button", { name: /Test/ }).click()
  await expect(dialog.locator("[data-probe=ok]")).toBeVisible()
  await expect(dialog).toContainText("USDT, 18 decimals")
  await dialog.getByRole("button", { name: "Add endpoint" }).click()
  await expect(rows).toHaveCount(3)
  expect((await api.must<{ network: { rpc_urls: string[] } }>(200, "GET", "/settings")).network.rpc_urls).toEqual([...stack.rpcUrls, extra])

  // Remove it again: back to two, and the watcher comes back healthy after its restart.
  await rows.nth(2).getByRole("button", { name: /Actions for/ }).click()
  await page.getByRole("menuitem", { name: "Remove" }).click()
  await expect(rows).toHaveCount(2)
  expect((await api.must<{ network: { rpc_urls: string[] } }>(200, "GET", "/settings")).network.rpc_urls).toEqual(stack.rpcUrls)
  await api.waitForNode()
  await expect(page.locator("[data-slot=node-status]")).toHaveAttribute("data-state", /^(Synced|Idle|Quiet)$/, { timeout: 60_000 })
})

test("replacing the deposit key asks to compare addresses first", async ({ operatorPage: page, instance }) => {
  const { api } = instance
  await page.goto(instance.url + "/settings/wallet")
  await page.getByRole("button", { name: "Replace key" }).click()
  const dialog = page.getByRole("dialog")
  const replace = dialog.getByRole("button", { name: "Replace key" })
  await expect(replace).toBeDisabled()

  // A private key is dropped the moment it is pasted.
  await dialog.getByLabel("Extended public key").fill("xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi")
  await expect(dialog.getByText(/That was a private key/)).toBeVisible()
  await expect(dialog.getByLabel("Extended public key")).toHaveValue("")

  // The key that is already in use is recognised; a truncated one is refused.
  await dialog.getByLabel("Extended public key").fill(XPUB)
  await expect(dialog.getByText("This is the key that is already in use.")).toBeVisible()
  await expect(dialog.locator('[data-derived="0"]')).toContainText("0xf39f")
  await expect(replace).toBeDisabled()
  await dialog.getByLabel("Extended public key").fill(XPUB.slice(0, 80))
  await expect(dialog.getByText(/cut short/)).toBeVisible()
  await page.keyboard.press("Escape")
  expect((await api.must<{ wallet: { xpub: string } }>(200, "GET", "/settings")).wallet.xpub).toBe(XPUB)
})

test("a payment link can be switched off and deleted from the dashboard", async ({ operatorPage: page, customer, instance }) => {
  const { api } = instance
  await api.must(201, "POST", "/payment-links", { name: "Spring sale", slug: "spring-sale", amount: "19" })
  await page.goto(instance.url + "/payment-links")
  const row = page.getByRole("row").filter({ hasText: "Spring sale" })
  await row.getByRole("button", { name: "Actions for Spring sale" }).click()
  await page.getByRole("menuitem", { name: /Disable/ }).click()
  await expect(row.getByText("Disabled")).toBeVisible()
  const visitor = await customer()
  await visitor.goto(`${instance.url}/l/spring-sale`)
  await expect(visitor.getByText("This link is no longer available")).toBeVisible()

  await row.getByRole("button", { name: "Actions for Spring sale" }).click()
  await page.getByRole("menuitem", { name: /Delete/ }).click()
  await page.getByRole("alertdialog").getByRole("button", { name: "Delete link" }).click()
  await expect(page.getByRole("row").filter({ hasText: "Spring sale" })).toHaveCount(0)
  await visitor.reload()
  await expect(visitor.getByText("This payment link doesn’t exist")).toBeVisible()
})

test("the next address index can be moved forward and back; an address the server has given out is never given again", async ({ operatorPage: page, instance }) => {
  const { api } = instance
  type Wallet = { xpub: string; derivation_path: string; next_address_index: number }
  const wallet = async () => (await api.must<{ wallet: Wallet }>(200, "GET", "/settings")).wallet
  // Some addresses are in use for certain.
  const made = [await api.createPayment({ amount: "3", order_id: "ORD-INDEX-1" }), await api.createPayment({ amount: "3", order_id: "ORD-INDEX-2" })]
  const { xpub, derivation_path, next_address_index: start } = await wallet()
  try {
    await page.goto(instance.url + "/settings/wallet")
    const shown = page.locator("[data-slot=next-address-index]")
    await expect(shown).toHaveText(start.toLocaleString("en-US"))
    const row = page.locator("[data-slot=settings-section]").filter({ hasText: "Deposit wallet" })
    await row.getByRole("button", { name: "Change" }).click()
    const dialog = page.getByRole("dialog")
    const field = dialog.getByLabel("Next address index")
    const set = dialog.getByRole("button", { name: "Set index" })
    await expect(set).toBeDisabled()

    // Beyond what a public key can derive: said on the field, and the server enforces the same.
    await field.fill("2147483648")
    await expect(dialog.getByRole("alert")).toContainText("The index must be below 2,147,483,648.")
    await expect(set).toBeDisabled()
    const refused = await api.call<{ error: { code: string; fields: { field: string; code: string }[] } }>("PUT", "/settings/wallet", { xpub, derivation_path, next_address_index: 2147483648 })
    expect(refused.status).toBeGreaterThanOrEqual(400)
    expect(refused.body.error.fields).toEqual([expect.objectContaining({ field: "next_address_index", code: "too_large" })])
    await field.fill(String(start))
    await expect(dialog).toContainText("That is the index in use now.")
    await expect(set).toBeDisabled()

    // Forward: the addresses in between are skipped, and the dialog says how many.
    await field.fill(String(start + 5))
    await expect(dialog.locator("[data-slot=index-note]")).toContainText("Skips 5 addresses")
    await expect(dialog.locator("[data-slot=index-warning]")).toHaveCount(0)
    await set.click()
    await expect(dialog).toHaveCount(0)
    await expect(shown).toHaveText((start + 5).toLocaleString("en-US"))
    expect((await wallet()).next_address_index).toBe(start + 5)
    made.push(await api.createPayment({ amount: "3", order_id: "ORD-INDEX-3" }))
    expect((await wallet()).next_address_index).toBe(start + 6)

    // Back to the very first address: a warning that says what the server guarantees and what it
    // cannot know, the address in question, and a box to tick before it is allowed.
    await page.reload()
    await row.getByRole("button", { name: "Change" }).click()
    await field.fill("0")
    const warning = dialog.locator("[data-slot=index-warning]")
    await expect(warning).toContainText("Going back can reuse an address")
    await expect(warning).toContainText("never gives out an address that a payment in this database already has")
    await expect(warning).toContainText("It cannot know about anything else")
    expect((await dialog.locator("[data-slot=index-address]").innerText()).toLowerCase()).toBe(FIRST_ADDRESS)
    await expect(set).toBeDisabled()
    await dialog.getByRole("checkbox").check()
    await set.click()
    await expect(dialog).toHaveCount(0)
    await expect(shown).toHaveText("0")
    expect((await wallet()).next_address_index).toBe(0)

    // What the server enforces: with the index at 0, where an address is long taken, the next
    // payment still gets one that no payment has, and the index moves past it.
    const before = await api.must<{ data: { address: string }[]; page: { total: number } }>(200, "GET", "/payments?limit=500")
    expect(before.data.length).toBe(before.page.total)
    expect(before.data.map((payment) => payment.address)).toContain(FIRST_ADDRESS)
    const fresh = await api.createPayment({ amount: "3", order_id: "ORD-INDEX-4" })
    made.push(fresh)
    expect(before.data.map((payment) => payment.address)).not.toContain(fresh.address)
    expect((await wallet()).next_address_index).toBeGreaterThan(0)
  } finally {
    // Leave the instance as other tests expect it: past every address used, nothing of ours open.
    const now = (await wallet()).next_address_index
    await api.must(200, "PUT", "/settings/wallet", { xpub, derivation_path, next_address_index: Math.max(now, start + 7) })
    for (const payment of made) await api.call("POST", `/payments/${payment.id}/cancel`)
  }
})
