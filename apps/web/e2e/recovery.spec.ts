import { Api, OPERATOR, Stack } from "./harness"
import { expect, prepare, test, verify } from "./fixtures"

/**
 * "Too many attempts": the login throttles that stop password guessing are shared, so anybody
 * can use them up. The operator gets past with a one-time code printed on the server.
 * A stack of its own: the test exhausts a login budget on purpose.
 */
test("a throttled login offers the recovery code, and the code from the server gets in", async ({ browser }, info) => {
  const stack = await new Stack(20 + info.parallelIndex).start()
  const context = await browser.newContext()
  await prepare(context)
  try {
    const setup = new Api(stack.url)
    await setup.setup(stack)

    // Guesses at the operator's account from many addresses: each is refused, until the account's budget is gone.
    let throttled = false
    for (let i = 0; i < 40 && !throttled; i++) {
      const guess = await new Api(stack.url, `10.99.${Math.floor(i / 200)}.${(i % 200) + 1}`).call("POST", "/auth/login", { email: OPERATOR.email, password: `wrong-guess-${i}-xxxxxxxx` })
      throttled = guess.status === 429
      if (!throttled) expect([401, 503]).toContain(guess.status)
    }
    expect(throttled, "the account's login budget was used up").toBe(true)

    // The real operator, on a browser the instance has never seen, with the right password: refused too.
    const page = await context.newPage()
    await page.goto(stack.url + "/login")
    await expect(page.getByLabel("Recovery code")).toHaveCount(0)
    await page.getByLabel("Email").fill(OPERATOR.email)
    await page.getByRole("textbox", { name: "Password" }).fill(OPERATOR.password)
    await page.getByRole("button", { name: "Sign in" }).click()
    await expect(page.getByText(/Too many sign-in attempts/)).toBeVisible()
    // The page offers the way out by itself.
    await expect(page.getByLabel("Recovery code")).toBeVisible()

    // A made-up code is refused before any password is checked.
    await page.getByLabel("Recovery code").fill("hmnr_not-a-real-code")
    await page.getByRole("button", { name: "Sign in" }).click()
    await expect(page.getByText(/recovery code is wrong, has expired or was already used/i)).toBeVisible()

    // `hmnpay recovery-code` on the server prints one; with the password it signs in.
    await page.getByLabel("Recovery code").fill(stack.recoveryCode())
    await page.getByRole("textbox", { name: "Password" }).fill(OPERATOR.password)
    await page.getByRole("button", { name: "Sign in" }).click()
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible()
    await verify(context)
  } finally {
    await context.close()
    await stack.stop()
  }
})

test("the recovery code field is there on demand", async ({ browser, instance }) => {
  const context = await browser.newContext()
  await prepare(context)
  const page = await context.newPage()
  await page.goto(instance.url + "/login")
  await page.getByRole("button", { name: "Have a recovery code?" }).click()
  await expect(page.getByLabel("Recovery code")).toBeVisible()
  await page.getByRole("button", { name: "Hide" }).click()
  await expect(page.getByLabel("Recovery code")).toHaveCount(0)
  // "Forgot password?" explains the reset on the server.
  await page.getByRole("link", { name: "Forgot password?" }).click()
  await expect(page.getByText("hmnpay reset-password").first()).toBeVisible()
  await verify(context)
  await context.close()
})
