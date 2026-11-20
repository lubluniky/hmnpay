import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { test } from "node:test"

// The receiver example the README shows, as a file of its own so that it can be run. It is
// loaded at run time: it lies outside this package (and outside the image build's context).
const EXAMPLE = new URL("../../../../docs/examples/verify-webhook.mjs", import.meta.url)
const { verifyHmnpaySignature } = (await import(EXAMPLE.href)) as { verifyHmnpaySignature: unknown }

/**
 * One request as the server signs it: the same body, secrets and time as the Go test
 * TestWebhookSignatureExampleVector (core/webhook_test.go), which pins the server's side.
 */
const BODY = `{"id":"evt_example","type":"payment.paid","data":{"id":"pay_example","status":"paid"},"seq":42}`
const T = 1767225600
const HEADER = `t=${T},v1=abbb4eb677a7b39f59c56c106ed94d3ace20ab5aa145a97f96e3ae5dee2c0d77,v1=7bcd5ec49b5306d81a0147e180991c3d9d654c44034feeb0c049a42e793235ab`
const verify = verifyHmnpaySignature as (header: unknown, body: string | Buffer, secret: string, tolerance?: number, now?: number) => boolean

test("the README's webhook receiver accepts what the server signs, with either secret of a rotation", () => {
  assert.equal(verify(HEADER, BODY, "whsec_current_example", 300, T + 10), true)
  assert.equal(verify(HEADER, Buffer.from(BODY), "whsec_current_example", 300, T + 10), true)
  assert.equal(verify(HEADER, BODY, "whsec_previous_example", 300, T - 10), true)
})

test("the README's webhook receiver refuses everything else", () => {
  assert.equal(verify(HEADER, BODY, "whsec_wrong", 300, T), false, "another secret")
  assert.equal(verify(HEADER, BODY.replace("paid", "PAID"), "whsec_current_example", 300, T), false, "a changed body")
  assert.equal(verify(HEADER, BODY, "whsec_current_example", 300, T + 301), false, "too old: a replay")
  assert.equal(verify(HEADER, BODY, "whsec_current_example", 300, T - 301), false, "from the future")
  assert.equal(verify(`t=${T}`, BODY, "whsec_current_example", 300, T), false, "no signature")
  assert.equal(verify(HEADER.replace(`t=${T}`, "t=x"), BODY, "whsec_current_example", 300, T), false, "a bad timestamp")
  assert.equal(verify(HEADER.replace(/v1=ab/, "v1=zz"), BODY, "whsec_previous_example", 300, T), true, "one broken signature does not hide a good one")
  assert.equal(verify(undefined, BODY, "whsec_current_example", 300, T), false, "no header")
  assert.throws(() => verify(HEADER, BODY, ""), /no webhook signing secret/, "an empty secret is a configuration error")
})

test("the README shows the example exactly as it is tested", () => {
  const example = readFileSync(EXAMPLE, "utf8")
  const readme = readFileSync(new URL("../../../../README.md", import.meta.url), "utf8")
  assert.ok(readme.includes("```js\n" + example + "```"), "README.md must contain docs/examples/verify-webhook.mjs verbatim in a ```js block")
})
