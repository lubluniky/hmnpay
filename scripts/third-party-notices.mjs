#!/usr/bin/env node
// Writes THIRD_PARTY_NOTICES.md: what a build of hmnpay carries that is somebody else's work,
// and under which licence, with the licence texts.
//
//   node scripts/third-party-notices.mjs            write the file
//   node scripts/third-party-notices.mjs --check    exit 1 if the file is not what would be written
//
// The file is embedded in the binary (notices.go) and served at /third-party-notices.txt, because
// the binary and the Docker image are distributed without this source tree.
//
// Nothing here is typed in by hand except the short descriptions of what is vendored:
//
//  - Go modules: the modules go.mod requires, with the licence files of the downloaded modules.
//  - JavaScript: the packages that really end up in the browser bundle. They are read from the
//    source maps of a build made for this purpose (into apps/web/.next-notices, which is not the
//    build that ships), so the list is neither a guess nor the whole dependency tree. Two
//    stylesheet packages that only reach the CSS are added by name.
//
// Needs Go, Node.js and the installed dependencies (pnpm install, go mod download). Run it after
// changing dependencies; a Go test (cmd/hmnpay) fails when a dependency is missing from the file.

import { execFileSync } from "node:child_process"
import { existsSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..")
const web = path.join(root, "apps/web")
const check = process.argv.includes("--check")
const read = (file) => readFileSync(file, "utf8").replace(/\r\n/g, "\n")

/* ------------------------------- licence texts ------------------------------- */

const LICENCE_FILE = /^(licen[cs]e|copying|notice|patents|authors)([.-].*)?$/i

/** The licence files at the top of a package or module directory, in a stable order. */
function licenceFiles(dir) {
  if (!existsSync(dir)) return []
  return readdirSync(dir)
    .filter((name) => LICENCE_FILE.test(name) && !/\.(js|ts|json)$/i.test(name))
    .sort((a, b) => a.toLowerCase().localeCompare(b.toLowerCase()))
    .map((name) => ({ name, text: read(path.join(dir, name)).trim() }))
}

/** "Copyright (c) 2023 Somebody" lines of a licence text. */
function copyrights(text) {
  const out = []
  for (const line of text.split("\n")) {
    const clean = line.replace(/^[\s>*#-]+/, "").trim()
    if (/^(copyright\b|\(c\)|©)/i.test(clean) && !/^copyright (notice|holders?|owner|and|license|\(c\) <year>)/i.test(clean) && clean.length < 200) out.push(clean)
  }
  return [...new Set(out)]
}

/** A licence text without what differs from package to package: titles, copyright lines, layout. */
function body(text) {
  return text
    .split("\n")
    .filter((line) => !/^[\s>*#-]*(copyright\b|\(c\)|©)/i.test(line) && !/^[\s#*]*(the )?(mit|isc|bsd|apache)[\w .()-]*licen[cs]e[\s*()mit]*$/i.test(line.trim()) && !/all rights reserved\.?$/i.test(line.trim()))
    .join(" ")
    .replace(/[`*_#>"“”'’]/g, "")
    .replace(/\s+/g, " ")
    .trim()
    .toLowerCase()
}

/** A licence id from the text, for modules that do not declare one (Go). */
function guessLicence(text) {
  const t = text.toLowerCase()
  if (t.includes("apache license") && t.includes("version 2.0")) return "Apache-2.0"
  if (t.includes("permission to use, copy, modify, and/or distribute") || t.includes("isc license")) return "ISC"
  if (t.includes("permission is hereby granted, free of charge")) return "MIT"
  if (t.includes("redistribution and use in source and binary forms")) return /neither the name|nor the names/.test(t) ? "BSD-3-Clause" : "BSD-2-Clause"
  if (t.includes("mozilla public license")) return "MPL-2.0"
  return "see text"
}

const FAMILY_TITLE = {
  MIT: "MIT License",
  ISC: "ISC License",
  "BSD-2-Clause": "BSD 2-Clause License",
  "BSD-3-Clause": "BSD 3-Clause License",
  "Apache-2.0": "Apache License, Version 2.0",
  "0BSD": "BSD Zero Clause License",
}

/* ----------------------------------- Go ----------------------------------- */

function goModules() {
  const wanted = new Set()
  for (const line of read(path.join(root, "go.mod")).split("\n")) {
    const fields = line.trim().split(/\s+/)
    if (fields.length >= 2 && fields[0].includes(".") && /^v\d/.test(fields[1])) wanted.add(fields[0])
  }
  const listed = execFileSync("go", ["list", "-m", "-f", "{{.Path}}\t{{.Version}}\t{{.Dir}}", "all"], { cwd: root, encoding: "utf8" })
  const out = []
  for (const line of listed.trim().split("\n")) {
    const [name, version, dir] = line.split("\t")
    if (!wanted.has(name)) continue
    if (!dir) throw new Error(`Go module ${name} is not downloaded: run "go mod download"`)
    const files = licenceFiles(dir)
    if (files.length === 0) throw new Error(`Go module ${name} has no licence file in ${dir}`)
    const main = files.find((f) => /^(licen[cs]e|copying)/i.test(f.name)) ?? files[0]
    out.push({ name, version, licence: guessLicence(main.text), files, copyright: copyrights(main.text) })
  }
  for (const name of wanted) if (!out.some((m) => m.name === name)) throw new Error(`Go module ${name} was not found by "go list"`)
  return out.sort((a, b) => a.name.localeCompare(b.name))
}

/* -------------------------------- JavaScript -------------------------------- */

/** Stylesheet packages: they reach the CSS, which has no source map entry per package. */
const CSS_PACKAGES = ["tailwindcss", "tw-animate-css"]

function bundledPackages() {
  const dist = ".next-notices"
  rmSync(path.join(web, dist), { recursive: true, force: true })
  execFileSync("pnpm", ["exec", "next", "build"], {
    cwd: web,
    stdio: ["ignore", "ignore", "inherit"],
    env: { ...process.env, HMNPAY_NOTICES_BUILD: dist, NEXT_TELEMETRY_DISABLED: "1" },
  })
  const chunks = path.join(web, dist, "_next/static/chunks")
  const dirs = new Set()
  for (const file of readdirSync(chunks)) {
    if (!file.endsWith(".js.map")) continue
    const map = JSON.parse(read(path.join(chunks, file)))
    const sources = [...(map.sources ?? []), ...(map.sections ?? []).flatMap((section) => section.map?.sources ?? [])]
    for (const source of sources) {
      const at = source.lastIndexOf("node_modules/")
      if (at < 0) continue
      const local = source.replace(/^turbopack:\/\/\/\[project\]\//, "")
      if (local === source) continue // the bundler's own runtime, not a package
      const rest = source.slice(at + "node_modules/".length).split("/")
      const name = rest[0].startsWith("@") ? `${rest[0]}/${rest[1]}` : rest[0]
      const dir = path.join(root, local.slice(0, local.lastIndexOf("node_modules/")), "node_modules", name)
      dirs.add(dir)
      // Next.js ships its own builds of React and a few others inside the package.
      const inside = /\/dist\/compiled\/((?:@[^/]+\/)?[^/]+)\//.exec(source.slice(at))
      if (name === "next" && inside) dirs.add(path.join(dir, "dist/compiled", inside[1]))
    }
  }
  rmSync(path.join(web, dist), { recursive: true, force: true })
  for (const name of CSS_PACKAGES) dirs.add(path.join(web, "node_modules", name))

  const byKey = new Map()
  for (const dir of dirs) {
    const manifest = path.join(dir, "package.json")
    if (!existsSync(manifest)) continue // a path inside another package's build, not a package of its own
    const pkg = JSON.parse(read(manifest))
    if (!pkg.name) continue
    const files = licenceFiles(dir)
    const licence = typeof pkg.license === "string" ? pkg.license : (pkg.license?.type ?? (files[0] ? guessLicence(files[0].text) : "see text"))
    const version = pkg.version ?? ""
    const viaNext = dir.includes(`${path.sep}next${path.sep}dist${path.sep}compiled${path.sep}`)
    const key = `${pkg.name}@${version}`
    if (byKey.has(key)) continue
    byKey.set(key, {
      name: pkg.name,
      version,
      licence,
      files,
      copyright: files.flatMap((f) => copyrights(f.text)),
      note: viaNext ? "as built into Next.js" : CSS_PACKAGES.includes(pkg.name) ? "stylesheet" : "",
    })
  }
  return [...byKey.values()].sort((a, b) => a.name.localeCompare(b.name) || a.version.localeCompare(b.version))
}

/* ---------------------------------- output ---------------------------------- */

function build() {
  const modules = goModules()
  const packages = bundledPackages()
  const goLicence = modules.find((m) => m.name === "golang.org/x/crypto")
  const fence = (text) => "```text\n" + text.replace(/```/g, "'''").trim() + "\n```"
  const lines = []
  const say = (...text) => lines.push(...text)

  say(
    "# Third-party notices",
    "",
    "hmnpay itself is licensed under the GNU General Public License, version 3 or",
    "(at your option) any later version (`LICENSE`), with additional terms under",
    "section 7 of that licence (`ADDITIONAL_TERMS.md`). Those additional terms do",
    "not apply to anything listed here. This file lists what a build of hmnpay —",
    "the binary and the Docker image — carries that is somebody else's work, and",
    "under which licence, with the licence texts. The binary is distributed without",
    "the source tree, so the server serves all three files: `/license.txt`,",
    "`/additional-terms.txt` and `/third-party-notices.txt`; the Docker image also",
    "has them in `/usr/share/doc/hmnpay/`.",
    "",
    "The file is generated by `scripts/third-party-notices.mjs`; do not edit it by",
    "hand.",
    "",
    "Contents:",
    "",
    "1. Fonts and code kept in the source tree",
    "2. Go: the standard library and the modules compiled into the binary",
    "3. JavaScript packages bundled into the web interface",
    "4. The Docker image",
    "5. Licence texts",
    "",
    "## 1. Fonts and code kept in the source tree",
    "",
    "| What | Where | Licence | Copyright |",
    "|---|---|---|---|",
    "| **Geist** and **Geist Mono** (fonts) | `apps/web/src/fonts/*.woff2`; embedded in the binary as part of the web interface | SIL Open Font License 1.1 | Copyright 2024 The Geist Project Authors (https://github.com/vercel/geist-font.git) |",
    "| **distortion-waves** (the wave transition) | `apps/web/src/vendor/distortion-waves/` | MIT | Copyright (c) 2026 Jakub Antalik |",
    "| **shadcn/ui** stylesheet | `apps/web/src/vendor/shadcn/tailwind.css` | MIT | Copyright (c) 2023 shadcn |",
    "| Components derived from **shadcn/ui** | `apps/web/src/components/ui/` | MIT | Copyright (c) 2023 shadcn |",
    "",
    "The font files are the script subsets of the two variable fonts as Google",
    "Fonts serves them, unmodified, eleven files in all. The fonts have no Reserved",
    "Font Name. The wave code is adapted for this project (what was changed is in",
    "its `README.md`). The components in `src/components/ui` were generated with the",
    "shadcn/ui tool and then edited; the stylesheet is the tool's, unmodified.",
    "These files keep their own licences: the additional terms of hmnpay's licence",
    "do not apply to them.",
    "",
    "## 2. Go",
    "",
    "The binary contains the Go runtime and standard library (BSD 3-Clause,",
    "Copyright 2009 The Go Authors; the text is the one printed for",
    "`golang.org/x/crypto` in section 5, and the Go project's additional patent",
    "grant is printed there too) and these modules:",
    "",
  )
  for (const m of modules) say(`- ${m.name} ${m.version} — ${m.licence}${m.copyright.length ? ` — ${m.copyright.slice(0, 2).join("; ")}` : ""}`)
  say(
    "",
    "`modernc.org/sqlite` is SQLite translated to Go; SQLite itself is in the public",
    "domain. Each module's licence files are printed in full in section 5.",
    "",
    "## 3. JavaScript packages bundled into the web interface",
    "",
    `These ${packages.length} packages are the ones whose code is in the interface's JavaScript`,
    "(read from the source maps of a build) or stylesheet. Packages used only to",
    "build it — the compiler, the linter, the test runner — are not distributed and",
    "are not listed.",
    "",
  )
  for (const p of packages) {
    const who = p.copyright.length ? ` — ${p.copyright.slice(0, 2).join("; ")}` : ""
    say(`- ${[p.name, p.version].filter(Boolean).join(" ")} — ${p.licence}${p.note ? ` (${p.note})` : ""}${who}`)
  }
  say(
    "",
    "## 4. The Docker image",
    "",
    "The image is the binary on `gcr.io/distroless/static-debian12` (pinned by",
    "digest in the `Dockerfile`): no shell and no package manager, only what a",
    "static binary needs — CA certificates (Mozilla Public License 2.0), time zone",
    "data (public domain), and Debian's `base-files` and `netbase` (GPL). Their",
    "notices are in the image under `/usr/share/doc/`; the project is at",
    "https://github.com/GoogleContainerTools/distroless (Apache License 2.0).",
    "",
    "## 5. Licence texts",
    "",
    "### SIL Open Font License 1.1 — Geist, Geist Mono",
    "",
    fence(read(path.join(web, "src/fonts/OFL.txt"))),
    "",
    "### MIT — distortion-waves",
    "",
    fence(read(path.join(web, "src/vendor/distortion-waves/LICENSE"))),
    "",
    "### MIT — shadcn/ui (stylesheet and derived components)",
    "",
    fence(read(path.join(web, "src/vendor/shadcn/LICENSE.md"))),
    "",
    "### Go modules",
    "",
    "The licence files of each module, as distributed with it.",
    "",
  )
  for (const m of modules) {
    for (const file of m.files) say(`#### ${m.name} ${m.version} — ${file.name}`, "", fence(file.text), "")
  }
  if (!goLicence) throw new Error("golang.org/x/crypto is expected among the modules (its licence text stands for the Go standard library's)")

  // JavaScript: one text per licence family where the packages' texts are that text, and every
  // text that is not printed in full under its package.
  say("### JavaScript packages", "")
  const families = new Map()
  const own = []
  for (const p of packages) {
    const main = p.files.find((f) => /^(licen[cs]e|copying)/i.test(f.name))
    if (!main) {
      own.push({ p, text: null })
      continue
    }
    const key = `${p.licence}\n${body(main.text)}`
    if (!families.has(key)) families.set(key, { licence: p.licence, text: main.text, packages: [] })
    families.get(key).packages.push(p)
  }
  // The most widely shared text of each licence is its family text; the rest are printed per package.
  const best = new Map()
  for (const family of families.values()) {
    const current = best.get(family.licence)
    if (!current || family.packages.length > current.packages.length) best.set(family.licence, family)
  }
  const standard = [...best.values()].filter((family) => family.packages.length > 1 || FAMILY_TITLE[family.licence]).sort((a, b) => b.packages.length - a.packages.length)
  for (const family of standard) {
    say(
      `#### ${FAMILY_TITLE[family.licence] ?? family.licence}`,
      "",
      `The text below is the licence of these ${family.packages.length} packages. Each has its own copyright line, given in section 3; the text is otherwise the same.`,
      "",
      family.packages.map((p) => [p.name, p.version].filter(Boolean).join(" ")).join(", ") + ".",
      "",
      fence(family.text.replace(/^[\s>*#-]*(copyright\b|\(c\)|©).*$/gim, "Copyright (c) <the copyright holders named in section 3>").replace(/(Copyright \(c\) <the copyright holders named in section 3>\n)+/g, "$1")),
      "",
    )
  }
  for (const family of families.values()) {
    if (standard.includes(family)) continue
    for (const p of family.packages) own.push({ p, text: family.text })
  }
  own.sort((a, b) => a.p.name.localeCompare(b.p.name))
  for (const { p, text } of own) {
    say(`#### ${[p.name, p.version].filter(Boolean).join(" ")} — ${p.licence}`, "")
    if (text === null) {
      say(`The package has no licence file; its \`package.json\` declares the licence ${p.licence}${FAMILY_TITLE[p.licence] ? `, whose text is printed above (${FAMILY_TITLE[p.licence]})` : ""}.`, "")
    } else {
      say(fence(text), "")
    }
    for (const extra of p.files.filter((f) => !/^(licen[cs]e|copying)/i.test(f.name))) say(`${p.name} ${p.version} — ${extra.name}`, "", fence(extra.text), "")
  }
  return lines.join("\n").replace(/\n{3,}/g, "\n\n").trimEnd() + "\n"
}

const target = path.join(root, "THIRD_PARTY_NOTICES.md")
const text = build()
if (check) {
  if (!existsSync(target) || read(target) !== text) {
    console.error("THIRD_PARTY_NOTICES.md is out of date: run node scripts/third-party-notices.mjs")
    process.exit(1)
  }
  console.log("THIRD_PARTY_NOTICES.md is up to date")
} else {
  writeFileSync(target, text)
  console.log(`wrote THIRD_PARTY_NOTICES.md (${text.split("\n").length} lines, ${Math.round(text.length / 1024)} KB)`)
}
