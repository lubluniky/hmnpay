import { defineConfig } from "@playwright/test"

/**
 * End-to-end tests against the real hmnpay binary and a local chain (see e2e/harness.ts).
 * Each worker runs its own chain and its own hmnpay, so two workers never share state.
 * Build the binary with the interface first: `make build` (or set HMNPAY_E2E_BIN).
 */
export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/*.spec.ts",
  outputDir: "./e2e/.results",
  workers: Number(process.env.HMNPAY_E2E_WORKERS ?? 2),
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  retries: 0,
  // Real blocks take real seconds: a payment needs a handful of them to confirm.
  timeout: 180_000,
  expect: { timeout: 30_000 },
  reporter: [["list"]],
  use: {
    // The locally installed Chrome, as scripts/shots.mjs uses: no browser download.
    channel: process.env.HMNPAY_E2E_CHANNEL ?? "chrome",
    headless: true,
    viewport: { width: 1440, height: 900 },
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
})
