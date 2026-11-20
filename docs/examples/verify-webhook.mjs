import { createHmac, timingSafeEqual } from "node:crypto"

/**
 * Checks the Hmnpay-Signature header of a webhook request: "t=<unix seconds>,v1=<hex>",
 * with a second v1= while a secret is being rotated. v1 is the HMAC-SHA256, under the
 * endpoint's signing secret (whsec_…), of "<t>.<raw request body>".
 *
 * body must be the raw bytes of the request (a Buffer or a string), not re-serialised JSON.
 */
export function verifyHmnpaySignature(header, body, secret, toleranceSeconds = 300, now = Date.now() / 1000) {
  if (!secret) throw new Error("no webhook signing secret configured")
  let t
  const signatures = []
  for (const part of String(header ?? "").split(",")) {
    const [key, value] = part.trim().split("=")
    if (key === "t") t = value
    else if (key === "v1") signatures.push(value)
  }
  if (!/^\d+$/.test(t ?? "") || signatures.length === 0) return false
  if (Math.abs(now - Number(t)) > toleranceSeconds) return false // a replayed request
  const expected = createHmac("sha256", secret).update(`${t}.`).update(body).digest()
  return signatures.some((signature) => {
    const given = Buffer.from(signature ?? "", "hex")
    return given.length === expected.length && timingSafeEqual(given, expected)
  })
}
