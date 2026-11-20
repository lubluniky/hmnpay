import assert from "node:assert/strict"
import { test } from "node:test"

import { safeEmail, safeHttpUrl, safeLogoUrl, safeNextPath, safePaymentUri, safeSiteUrl, sameSitePath } from "./url.ts"

test("only http(s) URLs become link targets", () => {
  assert.equal(safeHttpUrl("https://shop.example.com/thanks?order=1"), "https://shop.example.com/thanks?order=1")
  assert.equal(safeHttpUrl("http://localhost:3000/"), "http://localhost:3000/")
  for (const bad of ["javascript:alert(1)", "JaVaScRiPt:alert(1)", " javascript:alert(1)", "data:text/html,<script>alert(1)</script>", "vbscript:x", "file:///etc/passwd", "ftp://x", "//evil.example", "/relative", "", null, undefined, "not a url"]) {
    assert.equal(safeHttpUrl(bad as string), null, String(bad))
  }
})

test("a logo is an http(s) URL or an inline bitmap, never anything that can run", () => {
  assert.equal(safeLogoUrl("https://cdn.example.com/logo.png"), "https://cdn.example.com/logo.png")
  assert.equal(safeLogoUrl("data:image/png;base64,iVBORw0KGgo="), "data:image/png;base64,iVBORw0KGgo=")
  assert.equal(safeLogoUrl("data:image/svg+xml;base64,PHN2Zz4="), null) // SVG can carry script
  assert.equal(safeLogoUrl("data:text/html;base64,PGI+"), null)
  assert.equal(safeLogoUrl("javascript:alert(1)"), null)
  assert.equal(safeLogoUrl(""), null)
})

test("site links, return paths, emails and payment URIs", () => {
  const site = "https://pay.example.com"
  assert.equal(safeSiteUrl("https://pay.example.com/l/pro", site), "https://pay.example.com/l/pro")
  assert.equal(safeSiteUrl("/l/pro", site), "/l/pro")
  assert.equal(safeSiteUrl("//evil.example/l/pro", site), "#")
  assert.equal(safeSiteUrl("javascript:alert(1)", site), "#")

  assert.equal(safeNextPath("/payments?status=open", site), "/payments?status=open")
  for (const bad of ["//evil.example", "https://evil.example", "/\\evil.example", "payments", "/api/v1/auth/logout", "", null]) {
    assert.equal(safeNextPath(bad as string, site), "/", String(bad))
  }
  // Without a page there is no site to stay on: nothing is accepted.
  assert.equal(safeNextPath("/payments"), "/")

  assert.equal(safeEmail("billing@shop.example.com"), "billing@shop.example.com")
  assert.equal(safeEmail("x@y.z?subject=hi&body=evil"), null)
  assert.equal(safeEmail("not an email"), null)

  const uri = "ethereum:0x55d398326f99059fF775485246999027B3197955@56/transfer?address=0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266&uint256=25000000000000000000"
  assert.equal(safePaymentUri(uri), uri)
  assert.equal(safePaymentUri("javascript:alert(1)"), null)
  assert.equal(safePaymentUri(uri + "&x=1"), null)
})

/* What a browser does with a navigation target, which is what counts: it strips tab, CR and LF
   before parsing, and reads "\" as "/". A target is judged by where it resolves. */
const ORIGIN = "https://pay.example.com"
const OFF_SITE = [
  // A control character between the two slashes of "//host": browsers drop it, so this is "//evil.example".
  "/\t/evil.example/hmnpay-login",
  "/\n/evil.example/hmnpay-login",
  "/\r/evil.example/hmnpay-login",
  "/\t\n\r/evil.example",
  "\t//evil.example",
  "/\x00/evil.example",
  "/\x1f/evil.example",
  "/\x7f/evil.example",
  "/x\tx", // a control character anywhere is refused, harmless or not
  "//evil.example/x",
  "///evil.example/x",
  "/\\evil.example/x",
  "/\\/evil.example/x",
  "\\\\evil.example/x",
  "/ok\\path",
  "https://evil.example/x",
  "HTTPS://evil.example/x",
  "https:evil.example",
  "javascript:alert(1)",
  "data:text/html,x",
  "evil.example/x",
  "payments",
  "?next=/x",
  "#/x",
  ".",
  "",
  " /payments",
  "/",
]

test("a path on this site is accepted only if it resolves to this site", () => {
  for (const good of ["/payments", "/payments?status=open&q=a%20b", "/payments/pay_01hzy#transfers", "/settings/network?add=1", "/l/pro-annual", "/a b"]) {
    const resolved = sameSitePath(good, ORIGIN)
    assert.ok(resolved !== null, good)
    assert.equal(new URL(resolved, ORIGIN).origin, ORIGIN, good)
  }
  assert.equal(sameSitePath("/payments?status=open", ORIGIN), "/payments?status=open")
  assert.equal(sameSitePath("/payments/../settings", ORIGIN), "/settings") // what the browser would open
  for (const bad of OFF_SITE) assert.equal(sameSitePath(bad, ORIGIN), null, JSON.stringify(bad))
  assert.equal(sameSitePath(null, ORIGIN), null)
  assert.equal(sameSitePath(undefined, ORIGIN), null)
  // Whatever is accepted, a browser resolving it from a page of this site stays on this site.
  for (const candidate of [...OFF_SITE, "/payments", "/%09/evil.example", "/%5Cevil.example"]) {
    const resolved = sameSitePath(candidate, ORIGIN)
    if (resolved !== null) assert.equal(new URL(resolved, ORIGIN + "/login").origin, ORIGIN, JSON.stringify(candidate))
  }
})

test("?next= cannot leave the site", () => {
  for (const bad of OFF_SITE) assert.equal(safeNextPath(bad, ORIGIN), "/", JSON.stringify(bad))
  // As the login page receives them: URLSearchParams has already decoded %09, %0A, %0D.
  for (const query of ["?next=/%09/evil.example/x", "?next=/%0A/evil.example/x", "?next=/%0D/evil.example/x", "?next=%2F%09%2Fevil.example", "?next=/%5Cevil.example/x", "?next=//evil.example/x"]) {
    assert.equal(safeNextPath(new URLSearchParams(query).get("next"), ORIGIN), "/", query)
  }
  assert.equal(safeNextPath("/payments?status=open", ORIGIN), "/payments?status=open")
  assert.equal(safeNextPath("/settings/network", ORIGIN), "/settings/network")
  // Never into the API, however it is spelled.
  for (const api of ["/api/v1/auth/logout", "/api", "/payments/../api/v1/operator", "/API/v1/x"]) assert.equal(safeNextPath(api, ORIGIN), "/", api)
})

test("a link the server built stays a web address or a path of this site", () => {
  assert.equal(safeSiteUrl("/l/pro", ORIGIN), "/l/pro")
  assert.equal(safeSiteUrl("https://pay.example.com/l/pro", ORIGIN), "https://pay.example.com/l/pro")
  for (const bad of ["/\t/evil.example/l/pro", "/\\evil.example", "//evil.example/l/pro", "javascript:alert(1)", ""]) assert.equal(safeSiteUrl(bad, ORIGIN), "#", JSON.stringify(bad))
})
