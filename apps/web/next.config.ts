import { createHash } from "node:crypto"
import { readdirSync, readFileSync } from "node:fs"
import path from "node:path"

import type { NextConfig } from "next"
import { PHASE_DEVELOPMENT_SERVER } from "next/constants"

/**
 * hmnpay's web interface is a static export: `next build` writes plain HTML, JS, CSS and fonts to
 * `out/`, and the Go server embeds and serves them (internal/webui). There is no Node server in
 * production and nothing here may depend on one: no server-side rendering per request, no
 * rewrites, no response headers (the Go server sends them, including the Content-Security-Policy).
 *
 * `next dev` is not an export, so that dynamic routes work for any id while developing. Open the
 * interface through the backend, which proxies to this dev server
 * (`HMNPAY_WEB_UPSTREAM=http://localhost:3000 hmnpay serve`, then http://localhost:8080): the API
 * and the pages then share one origin, exactly as in production.
 */

/**
 * The build id names this build of the interface (Next.js uses it to notice that a tab runs an
 * older build and reloads). Derived from the sources, so the same tree always gives the same id
 * and the build stays reproducible; a random id would make every build differ.
 */
function sourceBuildId(): string {
  const hash = createHash("sha256")
  const root = import.meta.dirname
  const walk = (dir: string) => {
    for (const entry of readdirSync(dir, { withFileTypes: true }).sort((a, b) => (a.name < b.name ? -1 : 1))) {
      const full = path.join(dir, entry.name)
      if (entry.isDirectory()) walk(full)
      else {
        hash.update(path.relative(root, full))
        hash.update("\0")
        hash.update(readFileSync(full))
        hash.update("\0")
      }
    }
  }
  walk(path.join(root, "src"))
  for (const file of ["package.json", "next.config.ts", "../../pnpm-lock.yaml"]) {
    hash.update(file)
    hash.update(readFileSync(path.join(root, file)))
  }
  return hash.digest("hex").slice(0, 20)
}

export default function config(phase: string): NextConfig {
  const dev = phase === PHASE_DEVELOPMENT_SERVER
  // The styleguide (/design) is a design-time page. Its files are named *.demo.tsx and are only
  // routes when the build asks for them, so they are not in the production bundle.
  const demo = dev || process.env.HMNPAY_DEMO === "1"

  return {
    output: dev ? undefined : "export",
    generateBuildId: sourceBuildId,
    pageExtensions: demo ? ["demo.tsx", "tsx", "ts"] : ["tsx", "ts"],
    // Keep the dev-mode "N" badge out of screenshots (errors still surface in the console / overlay).
    devIndicators: false,
    // No image optimisation server in a static export (the app uses plain <img>/<svg> anyway).
    images: { unoptimized: true },
    // Source maps are not part of a build. scripts/third-party-notices.mjs asks for them, into a
    // directory of its own, to read from them which packages really end up in the bundle.
    ...(process.env.HMNPAY_NOTICES_BUILD ? { productionBrowserSourceMaps: true, distDir: process.env.HMNPAY_NOTICES_BUILD } : {}),
    // Tailwind v4 is compiled by its native Turbopack loader (no PostCSS config).
    turbopack: {
      rules: {
        "*.css": {
          loaders: ["@tailwindcss/turbopack"],
          as: "*.css",
        },
      },
    },
  }
}
