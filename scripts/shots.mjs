#!/usr/bin/env node
// Screenshots of a running hmnpay instance using the locally installed Google Chrome
// (playwright-core, channel "chrome" — no browser download).
//
//   pnpm shots --routes "/payments,/settings/network" [--out <dir>] [--themes dark,light]
//              [--viewports desktop,mobile] [--base http://localhost:8080] [--name-prefix x]
//              [--email owner@example.com --password …] [--full-page true|false] [--wait <ms>]
//              [--freeze] [--timeout <ms>]
//
// The base is the hmnpay server itself (the binary serves the interface; while developing,
// the binary in front of `pnpm dev` via HMNPAY_WEB_UPSTREAM). Dashboard routes need a session:
// give the operator's --email and --password (or HMNPAY_SHOTS_EMAIL / HMNPAY_SHOTS_PASSWORD)
// and the script signs in through the API first. Public routes (/pay/<id>, /l/<slug>, /login,
// /setup) need nothing.
//
// For a complete set with seeded data — every screen and every checkout state, from the real
// binary on a local chain — run `pnpm --filter web shots` instead (apps/web/e2e/screens.shots.ts).
//
// Prints absolute PNG paths to stdout (one per line); diagnostics go to stderr.
// Exit codes: 0 ok, 1 page errors (console errors / uncaught exceptions / hydration / HTTP >= 400),
//             2 bad arguments, 3 dev server unreachable, 4 Chrome failed to launch.

import { execFileSync } from "node:child_process"
import { mkdir, mkdtemp, rm } from "node:fs/promises"
import net from "node:net"
import os from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { parseArgs } from "node:util"

import { chromium } from "playwright-core"

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..")
// pnpm runs scripts from the package root; INIT_CWD is where the user actually ran the command.
const CALLER_CWD = process.env.INIT_CWD ?? process.cwd()

const VIEWPORTS = {
  desktop: { viewport: { width: 1440, height: 900 }, isMobile: false, hasTouch: false },
  mobile: { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true },
}
const THEMES = ["dark", "light"]
// Must match storageKey in apps/web/src/components/providers.tsx
const THEME_STORAGE_KEY = "theme"

function fail(code, message) {
  console.error(`\n[shots] ERROR: ${message}\n`)
  process.exit(code)
}

function list(value) {
  return value
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
}

let args
try {
  args = parseArgs({
    // pnpm may forward a literal "--" separator; drop it.
    args: process.argv.slice(2).filter((a) => a !== "--"),
    options: {
      out: { type: "string" },
      routes: { type: "string" },
      themes: { type: "string", default: "dark,light" },
      viewports: { type: "string", default: "desktop,mobile" },
      base: { type: "string", default: "http://localhost:8080" },
      email: { type: "string", default: process.env.HMNPAY_SHOTS_EMAIL ?? "" },
      password: { type: "string", default: process.env.HMNPAY_SHOTS_PASSWORD ?? "" },
      "name-prefix": { type: "string", default: "" },
      "full-page": { type: "string", default: "true" },
      wait: { type: "string", default: "0" },
      timeout: { type: "string", default: "30000" },
      freeze: { type: "boolean", default: false },
      help: { type: "boolean", short: "h", default: false },
    },
    strict: true,
  }).values
} catch (error) {
  fail(2, error.message)
}

if (args.help) {
  console.log(
    'Usage: pnpm shots --routes "/payments,/settings" [--out dir] [--themes dark,light] [--viewports desktop,mobile] [--base url] [--email e --password p] [--name-prefix x] [--full-page true|false] [--wait ms] [--freeze]',
  )
  process.exit(0)
}

if (!args.routes) fail(2, 'Missing --routes, e.g. --routes "/payments,/settings/network"')
const routes = list(args.routes).map((r) => (r.startsWith("/") ? r : `/${r}`))
const themes = list(args.themes)
const viewports = list(args.viewports)
for (const t of themes) if (!THEMES.includes(t)) fail(2, `Unknown theme "${t}" (use: ${THEMES.join(", ")})`)
for (const v of viewports)
  if (!(v in VIEWPORTS)) fail(2, `Unknown viewport "${v}" (use: ${Object.keys(VIEWPORTS).join(", ")})`)
if (!["true", "false"].includes(args["full-page"])) fail(2, "--full-page must be true or false")
const fullPage = args["full-page"] === "true"
const extraWait = Number(args.wait)
const timeout = Number(args.timeout)
if (!Number.isFinite(extraWait) || extraWait < 0) fail(2, "--wait must be a non-negative number of ms")
const base = args.base.replace(/\/+$/, "")
const outDir = args.out ? path.resolve(CALLER_CWD, args.out) : path.join(REPO_ROOT, ".shots")
const prefix = args["name-prefix"]

function slugify(route) {
  const [pathname, query = ""] = route.split("?")
  const clean = (s) =>
    s
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
  const pathSlug = clean(pathname) || "root"
  const querySlug = clean(query)
  const slug = querySlug ? `${pathSlug}__${querySlug}` : pathSlug
  return prefix ? `${clean(prefix)}-${slug}` : slug
}

// 1. Is the server up?
try {
  const res = await fetch(base, { signal: AbortSignal.timeout(10_000), redirect: "manual" })
  if (res.status >= 500) console.error(`[shots] warning: ${base} answered HTTP ${res.status}`)
} catch (error) {
  fail(3, `No server at ${base} (${error.cause?.code ?? error.message}). Start hmnpay (\`./hmnpay\`, or \`pnpm dev\` behind it).`)
}

await mkdir(outDir, { recursive: true })

// 2. Launch local Chrome.
//    "direct": playwright spawns Chrome as a child process (the normal path).
//    "open":   macOS fallback — start headless Chrome through LaunchServices (`open -n -a`) and attach
//              over CDP. Observed need: on a machine where Chrome has never been opened (no
//              ~/Library/Application Support/Google/Chrome yet), a directly spawned Chrome — at least from
//              an agent/sandboxed process — dies at startup with "FATAL ... Failed to get the path for 1001".
//              One LaunchServices start creates that dir; after that the direct path works too.
//    Override with SHOTS_LAUNCH=direct|open (default: try direct, fall back to open on macOS).
async function launchViaOpen() {
  const port = await new Promise((resolve, reject) => {
    const srv = net.createServer()
    srv.unref()
    srv.on("error", reject)
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address()
      srv.close(() => resolve(port))
    })
  })
  const profile = await mkdtemp(path.join(os.tmpdir(), "hmnpay-shots-"))
  execFileSync("open", [
    "-n",
    "-g",
    "-a",
    "Google Chrome",
    "--args",
    "--headless",
    "--no-first-run",
    "--no-default-browser-check",
    "--hide-scrollbars",
    "--force-color-profile=srgb",
    `--remote-debugging-port=${port}`,
    `--user-data-dir=${profile}`,
    "about:blank",
  ])
  const endpoint = `http://127.0.0.1:${port}`
  const deadline = Date.now() + 20_000
  let lastError
  while (Date.now() < deadline) {
    try {
      const b = await chromium.connectOverCDP(endpoint, { timeout: 5_000 })
      return {
        browser: b,
        async close() {
          try {
            const session = await b.newBrowserCDPSession()
            await session.send("Browser.close")
          } catch {}
          await rm(profile, { recursive: true, force: true }).catch(() => {})
        },
      }
    } catch (error) {
      lastError = error
      await new Promise((r) => setTimeout(r, 250))
    }
  }
  try {
    execFileSync("pkill", ["-f", `--user-data-dir=${profile}`])
  } catch {}
  throw lastError ?? new Error("timed out waiting for Chrome DevTools endpoint")
}

async function launchChrome() {
  const mode = process.env.SHOTS_LAUNCH ?? "auto"
  if (mode !== "open") {
    try {
      const b = await chromium.launch({ channel: "chrome", headless: true })
      return { browser: b, close: () => b.close() }
    } catch (error) {
      if (mode === "direct" || process.platform !== "darwin") throw error
      console.error(`[shots] direct Chrome launch failed (${error.message.split("\n")[0]}); falling back to \`open -a\` + CDP`)
    }
  }
  return launchViaOpen()
}

let browser, closeBrowser
try {
  ;({ browser, close: closeBrowser } = await launchChrome())
} catch (error) {
  fail(4, `Could not launch Google Chrome via playwright-core (channel "chrome"): ${error.message.split("\n")[0]}`)
}
for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, async () => {
    await closeBrowser().catch(() => {})
    process.exit(130)
  })
}

const FREEZE_CSS = `*, *::before, *::after {
  animation-duration: 0s !important; animation-delay: 0s !important; animation-iteration-count: 1 !important;
  transition-duration: 0s !important; transition-delay: 0s !important; caret-color: transparent !important;
}`

const HYDRATION_RE = /hydrat|did not match|server rendered|text content does not match/i
const problems = [] // { route, theme, viewport, messages[] }

try {
  for (const theme of themes) {
    for (const viewportName of viewports) {
      const context = await browser.newContext({
        ...VIEWPORTS[viewportName],
        deviceScaleFactor: 2,
        colorScheme: theme,
        reducedMotion: args.freeze ? "reduce" : "no-preference",
      })
      context.setDefaultTimeout(timeout)
      if (args.email && args.password) {
        // The session is the cookie the API sets; it stays in this browser context.
        const login = await context.request.post(`${base}/api/v1/auth/login`, { data: { email: args.email, password: args.password } })
        if (!login.ok()) fail(1, `Could not sign in as ${args.email}: HTTP ${login.status()} ${await login.text()}`)
      }
      // Seed next-themes before any page script runs so the first paint is already themed.
      await context.addInitScript(
        ([key, value]) => {
          try {
            window.localStorage.setItem(key, value)
          } catch {}
        },
        [THEME_STORAGE_KEY, theme],
      )
      if (args.freeze) {
        await context.addInitScript((css) => {
          const inject = () => {
            const style = document.createElement("style")
            style.setAttribute("data-shots-freeze", "")
            style.textContent = css
            document.head.appendChild(style)
          }
          if (document.head) inject()
          else document.addEventListener("DOMContentLoaded", inject, { once: true })
        }, FREEZE_CSS)
      }

      for (const route of routes) {
        const page = await context.newPage()
        const messages = []
        page.on("console", (msg) => {
          if (msg.type() === "error") {
            const text = msg.text()
            messages.push(`${HYDRATION_RE.test(text) ? "hydration error" : "console.error"}: ${text}`)
          }
        })
        page.on("pageerror", (error) => messages.push(`uncaught exception: ${error.message}`))

        const url = base + route
        try {
          const response = await page.goto(url, { waitUntil: "networkidle", timeout })
          if (!response) messages.push("navigation returned no response")
          else if (response.status() >= 400) messages.push(`HTTP ${response.status()} for ${url}`)
          await page.evaluate(() => document.fonts.ready.then(() => undefined))
          if (extraWait > 0) await page.waitForTimeout(extraWait)

          const htmlClass = await page.evaluate(() => document.documentElement.className)
          if (!htmlClass.split(/\s+/).includes(theme))
            messages.push(`theme not applied: <html class="${htmlClass}"> lacks "${theme}"`)

          const file = path.join(outDir, `${slugify(route)}.${theme}.${viewportName}.png`)
          await page.screenshot({ path: file, fullPage, animations: args.freeze ? "disabled" : "allow" })
          console.log(file)
        } catch (error) {
          messages.push(`screenshot failed: ${error.message.split("\n")[0]}`)
        }

        if (messages.length) {
          problems.push({ route, theme, viewport: viewportName, messages })
          console.error(`\n[shots] ${route} (${theme}/${viewportName}) — ${messages.length} error(s):`)
          for (const m of messages) console.error(`  - ${m}`)
        }
        await page.close()
      }
      await context.close()
    }
  }
} finally {
  await closeBrowser()
}

if (problems.length) {
  fail(1, `${problems.length} route/theme/viewport combination(s) reported errors (see above). Screenshots that could be taken were still written.`)
}
console.error(`[shots] ok — ${routes.length * themes.length * viewports.length} screenshot(s) in ${outDir}`)
