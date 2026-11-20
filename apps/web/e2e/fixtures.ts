import { expect, test as base, type BrowserContext, type Page } from "@playwright/test"

import { Api, Stack } from "./harness"

export { expect }

/** A set-up instance: the stack and an operator API client signed in to it. */
export interface Instance {
  stack: Stack
  api: Api
  url: string
}

interface Watch {
  drift: string[]
  errors: string[]
}
const watched = new WeakMap<BrowserContext, Watch>()
let visitor = 0

/**
 * Prepares a browser context like a visitor of its own:
 *  - every API response is checked against the client's schemas (strict mode), and what does
 *    not match is collected: the test fails on it. This is the type-drift alarm.
 *  - uncaught exceptions and console errors are collected too (a refused request is logged by
 *    the browser as a console error; those are expected and left out).
 *  - it has its own client address, so one test's rate-limit budget is not another's.
 */
export async function prepare(context: BrowserContext, options: { theme?: "dark" | "light" } = {}) {
  const watch: Watch = { drift: [], errors: [] }
  watched.set(context, watch)
  visitor += 1
  await context.setExtraHTTPHeaders({ "X-Forwarded-For": `10.${20 + (process.pid % 200)}.${Math.floor(visitor / 250)}.${(visitor % 250) + 1}` })
  await context.addInitScript((theme) => {
    window.__HMNPAY_STRICT_API__ = true
    try {
      if (theme) window.localStorage.setItem("theme", theme)
    } catch {}
  }, options.theme ?? null)
  context.on("page", (page) => {
    page.on("pageerror", (error) => watch.errors.push(`uncaught: ${error.message}`))
    page.on("console", (message) => {
      if (message.type() !== "error") return
      const text = message.text()
      if (text.startsWith("Failed to load resource")) return
      watch.errors.push(text)
    })
  })
  return watch
}

/** Fails the test if a page saw an API response that does not match the client's types, or a script error. */
export async function verify(context: BrowserContext) {
  const watch = watched.get(context)
  if (!watch) return
  for (const page of context.pages()) {
    if (page.isClosed()) continue
    const drift = await page.evaluate(() => window.__hmnpayApiDrift ?? []).catch(() => [])
    watch.drift.push(...drift)
  }
  expect(watch.drift, "API responses that do not match src/lib/api/types.ts").toEqual([])
  expect(watch.errors, "console errors and uncaught exceptions").toEqual([])
}

/** Console errors a test provoked on purpose (a request the policy must refuse): taken off the list, and returned. */
export function expectedErrors(context: BrowserContext, pattern: RegExp): string[] {
  const watch = watched.get(context)
  if (!watch) return []
  const matching = watch.errors.filter((text) => pattern.test(text))
  watch.errors = watch.errors.filter((text) => !pattern.test(text))
  return matching
}

/** Puts the operator's session into a browser context: the cookie the server set on login. */
export async function signIn(context: BrowserContext, instance: Instance) {
  const [name, ...value] = instance.api.cookie.split("=")
  await context.addCookies([{ name, value: value.join("="), url: instance.url, httpOnly: true, sameSite: "Lax" }])
}

export const test = base.extend<
  { operatorPage: Page; customer: (options?: { theme?: "dark" | "light"; viewport?: { width: number; height: number } }) => Promise<Page> },
  { stack: Stack; instance: Instance }
>({
  /** One chain + one hmnpay per worker, not set up yet. */
  stack: [
    async ({}, provide, info) => {
      const stack = await new Stack(info.parallelIndex).start()
      await provide(stack)
      await stack.stop()
    },
    { scope: "worker", timeout: 120_000 },
  ],

  /** The worker's instance, set up through the API, with the watcher running. */
  instance: [
    async ({ stack }, provide) => {
      const api = new Api(stack.url)
      await api.setup(stack)
      await api.waitForNode()
      await provide({ stack, api, url: stack.url })
    },
    { scope: "worker", timeout: 180_000 },
  ],

  context: async ({ context }, provide) => {
    await prepare(context)
    await provide(context)
    await verify(context)
  },

  /** A page of the signed-in operator. */
  operatorPage: async ({ context, instance }, provide) => {
    await signIn(context, instance)
    await provide(await context.newPage())
  },

  /** A page of somebody who is not signed in (a customer), in a browser context of their own. */
  customer: async ({ browser }, provide) => {
    const contexts: BrowserContext[] = []
    await provide(async (options = {}) => {
      const context = await browser.newContext({ viewport: options.viewport ?? { width: 1440, height: 900 }, colorScheme: options.theme ?? "dark" })
      await prepare(context, { theme: options.theme })
      contexts.push(context)
      return context.newPage()
    })
    for (const context of contexts) {
      await verify(context)
      await context.close()
    }
  },
})

/**
 * How long to wait, from a transfer being mined (or its payment turning confirming), for what rests
 * on the transfer being final: paid, overpaid, late funds. A transfer is final once the chain has
 * finalised its block, and anvil finalises the block 64 below its tip — at one block a second, some
 * 65 to 70 seconds after mining with the watcher's 3-second ticks (on BNB Smart Chain, whose
 * finality trails the tip by two or three blocks, it is the confirmations that take longest: about
 * 30 blocks). Twice that is room for a loaded machine.
 */
export const FINAL_MS = 120_000

/** The checkout card's current pane ("awaiting", "detected", "confirming", "paid", "expired", "cancelled", …). */
export const pane = (page: Page) => page.locator("[data-pane]").last()

/**
 * The author attribution every payment page must show: the words "Powered by hmnpay", as a link
 * to the project page that opens in a new tab, visible and no smaller than body text.
 */
export async function expectAttribution(page: Page) {
  const link = page.locator('a[data-slot="powered-by"]')
  await expect(link).toHaveCount(1)
  await expect(link).toBeVisible()
  await expect(link).toHaveAttribute("href", "https://github.com/lubluniky/hmnpay")
  await expect(link).toHaveAttribute("target", "_blank")
  await expect(link).toHaveAttribute("rel", "noopener noreferrer")
  await expect(link).toHaveAccessibleName("Powered by hmnpay")
  // The visible words: "Powered by" and the wordmark "hmnpay".
  expect((await link.innerText()).replace(/\s+/g, " ").trim()).toBe("Powered by hmnpay")
  const facts = await link.evaluate((element) => {
    const smallest = (root: Element) => Math.min(...[root, ...root.querySelectorAll("*")].map((node) => parseFloat(getComputedStyle(node).fontSize)))
    const style = getComputedStyle(element)
    // Colours are written in modern colour spaces; a canvas turns any of them into sRGB bytes.
    const canvas = document.createElement("canvas")
    canvas.width = canvas.height = 1
    const pen = canvas.getContext("2d", { willReadFrequently: true })!
    const rgb = (value: string) => {
      pen.clearRect(0, 0, 1, 1)
      pen.fillStyle = "#000"
      pen.fillStyle = value
      pen.fillRect(0, 0, 1, 1)
      return Array.from(pen.getImageData(0, 0, 1, 1).data.slice(0, 3))
    }
    const luminance = (value: string) => {
      const [r, g, b] = rgb(value).map((c) => {
        const s = c / 255
        return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
      })
      return 0.2126 * r + 0.7152 * g + 0.0722 * b
    }
    // The page background behind the footer.
    const probe = document.createElement("div")
    probe.style.color = "var(--background)"
    document.body.appendChild(probe)
    const background = getComputedStyle(probe).color
    probe.remove()
    const label = element.querySelector("span")!
    const [hi, lo] = [luminance(getComputedStyle(label).color), luminance(background)].sort((a, b) => b - a)
    const box = element.getBoundingClientRect()
    return {
      size: smallest(element),
      opacity: Number(style.opacity),
      contrast: (hi + 0.05) / (lo + 0.05),
      width: box.width,
      height: box.height,
      insideViewportX: box.left >= 0 && box.right <= window.innerWidth,
    }
  })
  expect(facts.size, "attribution text size in px").toBeGreaterThanOrEqual(12)
  expect(facts.opacity).toBe(1)
  expect(facts.contrast, "contrast of the attribution against the page").toBeGreaterThanOrEqual(3)
  expect(facts.width).toBeGreaterThan(80)
  expect(facts.insideViewportX, "attribution is not cut off horizontally").toBe(true)
  // Reachable by ordinary scrolling, never hidden behind something.
  await link.scrollIntoViewIfNeeded()
  await expect(link).toBeInViewport()
}
