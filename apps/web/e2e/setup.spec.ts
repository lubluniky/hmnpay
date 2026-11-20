import { Api, FIRST_ADDRESS, MERCHANT, OPERATOR, Stack, XPUB } from "./harness"
import { expect, expectAttribution, prepare, signIn, test, verify } from "./fixtures"

/**
 * First run, the way a person does it: a fresh server, the setup token from its log, the
 * wizard, then signing out and in again.
 */
test("first-run setup with the token, then sign out and sign in", async ({ browser }, info) => {
  const stack = await new Stack(10 + info.parallelIndex).start()
  const context = await browser.newContext()
  await prepare(context)
  const page = await context.newPage()
  try {
    // A fresh instance sends every visitor to setup.
    await page.goto(stack.url + "/")
    await expect(page).toHaveURL(/\/setup/)
    await expect(page.getByRole("heading", { name: "Welcome to hmnpay" })).toBeVisible()
    await expectAttribution(page)

    // Account. A wrong setup token is refused by the server, on the field.
    await page.getByLabel("Setup token").fill("not-the-token-not-the-token")
    await page.getByLabel("Email").fill(OPERATOR.email)
    await page.getByLabel("Password", { exact: true }).fill(OPERATOR.password)
    await page.getByRole("button", { name: "Continue" }).click()
    await expect(page.getByText("That setup token is not the one this server printed")).toBeVisible()
    await page.getByLabel("Setup token").fill(stack.setupToken)
    await page.getByRole("button", { name: "Continue" }).click()

    // Network: real probe results, and the two-endpoint rule.
    await expect(page.getByRole("heading", { name: /Connect to BNB Smart Chain/ })).toBeVisible()
    await expect(page.getByText("Add at least two independent endpoints")).toBeVisible()
    await page.getByLabel("RPC endpoint").fill("http://127.0.0.1:1") // nothing listens there
    await page.getByRole("button", { name: "Test connection" }).click()
    await expect(page.getByText("Can’t connect")).toBeVisible()
    await page.getByLabel("RPC endpoint").fill(stack.rpcUrls[0])
    await page.getByRole("button", { name: "Test connection" }).click()
    const readout = page.getByRole("region", { name: "Connection test" })
    await expect(readout.getByText("Connected")).toBeVisible()
    await expect(readout).toContainText("56") // the chain id the endpoint reported
    await expect(readout).toContainText("USDT · 18 decimals") // what the token contract answered
    await expect(page.getByText("One endpoint has to be trusted completely")).toBeVisible()
    await page.getByLabel("Another RPC endpoint").fill(stack.rpcUrls[1])
    await page.getByRole("button", { name: "Test and add" }).click()
    await expect(page.getByText("Good: 2 endpoints.")).toBeVisible()
    await page.getByLabel("Another RPC endpoint").fill("")
    await page.getByRole("button", { name: "Continue" }).click()

    // Wallet: the server derives the first addresses from the xpub; the first one is the wallet's first address.
    await expect(page.getByRole("heading", { name: "Connect your wallet" })).toBeVisible()
    await page.getByLabel("Extended public key").fill(XPUB)
    const first = page.locator('[data-derived="0"]')
    await expect(first).toBeVisible()
    expect(((await first.locator("[title]").last().getAttribute("title")) ?? "").toLowerCase()).toBe(FIRST_ADDRESS)
    await expect(page.locator("[data-derived]")).toHaveCount(3)
    await page.getByRole("button", { name: "Continue" }).click()
    await expect(page.getByText("Compare the addresses first")).toBeVisible() // not confirmed yet
    await page.getByText("These match the first addresses in my wallet").click()
    await page.getByRole("button", { name: "Continue" }).click()

    // Business, and finish.
    await page.getByLabel("Business name").fill(MERCHANT.name)
    await page.getByLabel("Support email").fill(MERCHANT.supportEmail)
    await page.getByRole("button", { name: "Finish setup" }).click()
    await expect(page.getByRole("heading", { name: "You’re ready to take payments" })).toBeVisible()

    // The server signed the new operator in: the dashboard opens, with the merchant's name and a working node.
    await page.getByRole("link", { name: "Go to dashboard" }).click()
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible()
    await expect(page.getByRole("navigation", { name: "Breadcrumb" }).getByText(MERCHANT.name)).toBeVisible()
    await expect(page.locator("[data-node-state]")).toHaveAttribute("data-node-state", /^(Synced|Idle|Quiet)$/, { timeout: 60_000 })
    await expect(page.getByText("Take your first payment")).toBeVisible() // nothing exists yet
    await expectAttribution(page) // once, in the sidebar

    // What the wizard sent is what the server stored.
    const api = new Api(stack.url)
    await api.login()
    const settings = await api.must<{ network: { rpc_urls: string[]; chain_id: number }; wallet: { xpub: string }; merchant: { name: string } }>(200, "GET", "/settings")
    expect(settings.network).toMatchObject({ chain_id: 56, rpc_urls: stack.rpcUrls })
    expect(settings.wallet.xpub).toBe(XPUB)
    expect(settings.merchant.name).toBe(MERCHANT.name)

    // Sign out; the dashboard is closed to a visitor without a session.
    await page.getByRole("button", { name: OPERATOR.email }).click()
    await page.getByRole("menuitem", { name: "Log out" }).click()
    await expect(page).toHaveURL(/\/login$/)
    await page.goto(stack.url + "/payments")
    await expect(page).toHaveURL(/\/login\?next=%2Fpayments$/)
    await expectAttribution(page)

    // A wrong password is refused in words; the right one goes where the operator was headed.
    await page.getByLabel("Email").fill(OPERATOR.email)
    await page.getByRole("textbox", { name: "Password" }).fill("definitely not the password")
    await page.getByRole("button", { name: "Sign in" }).click()
    await expect(page.getByText("That email and password don’t match.")).toBeVisible()
    await page.getByRole("textbox", { name: "Password" }).fill(OPERATOR.password)
    await page.getByRole("button", { name: "Sign in" }).click()
    await expect(page).toHaveURL(/\/payments$/)
    await expect(page.getByRole("heading", { name: "Payments", exact: true })).toBeVisible()

    // Setup is closed now: its page sends visitors to the login.
    const again = await context.newPage()
    await context.clearCookies()
    await again.goto(stack.url + "/setup")
    await expect(again).toHaveURL(/\/login/)
    await verify(context)
  } finally {
    await context.close()
    await stack.stop()
  }
})

/**
 * Endpoints that are not independent, where the operator chooses them. Another URL of an endpoint
 * already in the list (only the path or the key differs) is refused on its field before it is
 * tested, and the server refuses it too; two hosts that look like one provider are accepted with a
 * warning that says why it matters. The same on the Network page after setup.
 */
test("the same endpoint twice is refused on its field; endpoints of one provider are a warning, in setup and after it", async ({ browser }, info) => {
  const stack = await new Stack(16 + info.parallelIndex).start()
  const context = await browser.newContext()
  await prepare(context)
  const page = await context.newPage()
  try {
    await page.goto(stack.url + "/setup")
    await page.getByLabel("Setup token").fill(stack.setupToken)
    await page.getByLabel("Email").fill(OPERATOR.email)
    await page.getByLabel("Password", { exact: true }).fill(OPERATOR.password)
    await page.getByRole("button", { name: "Continue" }).click()
    await expect(page.getByRole("heading", { name: /Connect to BNB Smart Chain/ })).toBeVisible()
    const field = page.getByLabel(/RPC endpoint/)
    await field.fill(stack.rpcUrls[0])
    await page.getByRole("button", { name: "Test connection" }).click()
    await expect(page.locator("section[aria-label='Endpoints hmnpay will use'] li")).toHaveCount(1)

    // The same endpoint with another path and key: said on the field at once, never tested or added.
    const host = new URL(stack.rpcUrls[0]).host
    await field.fill(`${stack.rpcUrls[0]}/v1/another-key`)
    await expect(page.getByText(`This is the same endpoint as ${host}, already in the list`)).toBeVisible()
    await expect(page.getByRole("button", { name: "Test and add" })).toBeDisabled()
    await expect(page.locator("section[aria-label='Endpoints hmnpay will use'] li")).toHaveCount(1)

    // Two hosts of one registrable domain. (Names a public resolver does not know: the probe is
    // answered for them with what the real endpoint answered.)
    const real = await new Api(stack.url).must(200, "POST", "/setup/probe-rpc", { url: stack.rpcUrls[0], chain_id: 56, setup_token: stack.setupToken })
    await page.route("**/api/v1/setup/probe-rpc", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(real) }))
    for (const name of ["rpc-a.example.com", "rpc-b.example.com"]) {
      await field.fill(`https://${name}/key`)
      await page.getByRole("button", { name: "Test and add" }).click()
      await expect(page.locator("section[aria-label='Endpoints hmnpay will use'] li").filter({ hasText: name })).toBeVisible()
    }
    const warning = page.locator("[data-slot=same-provider]")
    await expect(warning).toContainText("rpc-a.example.com and rpc-b.example.com look like the same provider")
    await expect(warning).toContainText("Endpoints are only a check on each other when different companies run them")
    await expect(page.getByRole("button", { name: "Continue" })).toBeEnabled() // a warning, not an error
    await page.unroute("**/api/v1/setup/probe-rpc")

    // After setup: the server refuses the same endpoint twice on its field, and names a provider pair.
    const api = new Api(stack.url)
    await api.setup(stack)
    const refused = await api.call<{ error: { code: string; fields: { field: string; code: string }[] } }>("PUT", "/settings/network", { chain_id: 56, rpc_urls: [stack.rpcUrls[0], `${stack.rpcUrls[0]}/v1/key`] })
    expect(refused.status).toBe(422)
    expect(refused.body.error.fields).toContainEqual(expect.objectContaining({ field: "rpc_urls.1", code: "same_endpoint" }))
    // Other spellings of the same host are the same endpoint too: an IPv4-mapped IPv6 address,
    // a trailing dot, a port written with a leading zero.
    const port = new URL(stack.rpcUrls[0]).port
    for (const spelling of [`http://[::ffff:127.0.0.1]:${port}/x`, `http://127.0.0.1.:${port}`, `http://127.0.0.1:0${port}/rpc`]) {
      const other = await api.call<{ error: { fields: { field: string; code: string }[] } }>("PUT", "/settings/network", { chain_id: 56, rpc_urls: [stack.rpcUrls[0], spelling] })
      expect(other.status, spelling).toBe(422)
      expect(other.body.error.fields, spelling).toContainEqual(expect.objectContaining({ field: "rpc_urls.1", code: "same_endpoint" }))
    }
    const saved = await api.must<{ network: { warnings: string[] } }>(200, "PUT", "/settings/network", { chain_id: 56, rpc_urls: [...stack.rpcUrls, "https://rpc-a.example.com/k", "https://rpc-b.example.com/k"] })
    expect(saved.network.warnings).toHaveLength(1)
    const dashboard = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    await prepare(dashboard)
    await signIn(dashboard, { stack, api, url: stack.url })
    const settingsPage = await dashboard.newPage()
    await settingsPage.goto(stack.url + "/settings/network")
    await expect(settingsPage.locator("[data-slot=endpoint-warnings] [data-slot=same-provider]")).toContainText("rpc-a.example.com and rpc-b.example.com look like the same provider")
    // The add dialog says "same endpoint" on its field, before anything is sent.
    await settingsPage.getByRole("button", { name: /^Add/ }).click()
    await settingsPage.getByRole("dialog").getByLabel("Endpoint URL").fill(`${stack.rpcUrls[0]}/v1/key`)
    await expect(settingsPage.getByRole("dialog").locator("[data-slot=same-endpoint]")).toContainText(`same endpoint as ${host}`)
    await expect(settingsPage.getByRole("dialog").getByRole("button", { name: "Test connection" })).toBeDisabled()
    // The dialog knows the other spellings the server refuses, as it does.
    for (const spelling of [`http://[::ffff:127.0.0.1]:${port}/x`, `http://127.0.0.1.:${port}`, `http://127.0.0.1:0${port}/rpc`]) {
      await settingsPage.getByRole("dialog").getByLabel("Endpoint URL").fill(spelling)
      await expect(settingsPage.getByRole("dialog").locator("[data-slot=same-endpoint]"), spelling).toBeVisible()
    }
    await verify(context)
    await verify(dashboard)
    await dashboard.close()
  } finally {
    await context.close()
    await stack.stop()
  }
})
