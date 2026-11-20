import { defineConfig } from "@playwright/test"

import base from "./playwright.config"

/**
 * `pnpm --filter web shots`: screenshots of every screen from the real binary with real data
 * (e2e/screens.shots.ts seeds it on a local chain). Not part of the test run.
 * HMNPAY_SHOTS_DIR says where they go (default: <repo>/.shots).
 */
export default defineConfig({
  ...base,
  testMatch: "**/*.shots.ts",
  workers: 1,
  timeout: 1_500_000,
})
