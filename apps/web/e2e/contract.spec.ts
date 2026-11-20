import { check, type Schema } from "../src/lib/api/schema"
import * as t from "../src/lib/api/types"
import { expect, test } from "./fixtures"

/**
 * The client's types against the server's answers, endpoint by endpoint, strictly: a field the
 * server added, dropped, renamed or retyped fails here. (The pages check the same way while
 * the other specs drive them; this one makes sure every endpoint is covered even where no
 * spec happens to visit it.)
 */
test("every API response matches the client's types exactly", async ({ instance }) => {
  const { stack, api } = instance
  const issues: string[] = []
  const expectShape = async <T>(schema: Schema<T>, method: string, route: string, body?: unknown, status = 200, headers?: Record<string, string>): Promise<T> => {
    const result = await api.call<T>(method, route, body, headers)
    expect(result.status, `${method} ${route}: ${JSON.stringify(result.body)}`).toBe(status)
    issues.push(...check(schema, result.body, `${method} ${route}`))
    return result.body
  }

  // Something of everything, so no list is checked empty.
  const payment = await expectShape(t.payment, "POST", "/payments", { amount: "10", order_id: "CONTRACT-1", metadata: { plan: "pro" }, customer_email: "c@example.com" }, 201, {
    "idempotency-key": `contract-${Date.now()}`,
  })
  await stack.pay(payment.address, "10.5")
  await api.waitForStatus(payment.id, ["overpaid"])
  const link = await expectShape(t.link, "POST", "/payment-links", { name: "Contract link", slug: `contract-${Date.now()}`, amount: null, min_amount: "2", max_uses: 3 }, 201)
  const endpoint = await expectShape(t.endpoint, "POST", "/webhook-endpoints", { url: stack.hookUrl + "/contract", events: ["payment.paid", "payment.overpaid"] }, 201)
  const key = await expectShape(t.apiKey, "POST", "/api-keys", { name: "contract", scopes: ["payments:read"], expires_at: new Date(Date.now() + 86_400_000).toISOString() }, 201)
  const test_ = await expectShape(t.delivery, "POST", `/webhook-endpoints/${endpoint.id}/test`, undefined, 202)

  await expectShape(t.me, "GET", "/auth/me")
  await expectShape(t.instance, "GET", "/instance")
  await expectShape(t.setupStatus, "GET", "/setup")
  await expectShape(t.settings, "GET", "/settings")
  await expectShape(t.operator, "GET", "/operator")
  await expectShape(t.sessionList, "GET", "/operator/sessions")
  await expectShape(t.nodeStatus, "GET", "/node-status")
  await expectShape(t.overview, "GET", "/overview?days=7")
  await expectShape(t.paymentList, "GET", "/payments?counts=true&limit=5")
  await expectShape(t.paymentList, "GET", "/payments?view=attention")
  await expectShape(t.payment, "GET", `/payments/${payment.id}`)
  const detail = await expectShape(t.paymentDetail, "GET", `/payments/${payment.id}/detail`)
  expect(detail.transfers.length).toBeGreaterThan(0)
  expect(detail.timeline.length).toBeGreaterThan(1)
  await expectShape(t.payment, "POST", `/payments/${payment.id}/resolve`, { note: "contract" })
  await expectShape(t.payment, "POST", `/payments/${payment.id}/reopen`)
  await expectShape(t.events, "GET", "/events?after=0&limit=50")
  await expectShape(t.linkList, "GET", "/payment-links")
  await expectShape(t.link, "POST", `/payment-links/${link.id}/deactivate`)
  await expectShape(t.link, "POST", `/payment-links/${link.id}/activate`)
  await expectShape(t.endpointList, "GET", "/webhook-endpoints")
  await expectShape(t.endpoint, "PUT", `/webhook-endpoints/${endpoint.id}`, { url: endpoint.url, description: "renamed", events: ["payment.paid"], enabled: true })
  await expectShape(t.secret, "POST", `/webhook-endpoints/${endpoint.id}/reveal-secret`)
  await expectShape(t.endpoint, "POST", `/webhook-endpoints/${endpoint.id}/rotate-secret`)
  await expectShape(t.secret, "POST", `/webhook-endpoints/${endpoint.id}/reveal-secret`)
  await expectShape(t.deliveryList, "GET", "/webhook-deliveries?limit=10")
  await expect.poll(async () => (await api.must<t.Delivery>(200, "GET", `/webhook-deliveries/${test_.id}`)).status).not.toBe("pending")
  await expectShape(t.delivery, "GET", `/webhook-deliveries/${test_.id}`)
  await expectShape(t.delivery, "POST", `/webhook-deliveries/${test_.id}/resend`, undefined, 202)
  await expectShape(t.apiKeyList, "GET", "/api-keys")
  await expectShape(t.apiKey, "POST", `/api-keys/${key.id}/revoke`)
  await expectShape(t.probeResult, "POST", "/setup/probe-rpc", { url: stack.rpcUrl, chain_id: 56 })
  await expectShape(t.probeResult, "POST", "/setup/probe-rpc", { url: "http://127.0.0.1:1", chain_id: 56 })
  await expectShape(t.derivedAddressList, "POST", "/setup/preview-addresses", { xpub: (await api.must<t.Settings>(200, "GET", "/settings")).wallet.xpub, derivation_path: "m/44'/60'/0'/0", count: 2 })
  const settings = await api.must<t.Settings>(200, "GET", "/settings")
  await expectShape(t.settings, "PUT", "/settings/payments", settings.payments)
  await expectShape(t.settings, "PUT", "/settings/merchant", {
    merchant_name: settings.merchant.name,
    logo_url: settings.merchant.logo_url,
    support_email: settings.merchant.support_email,
    support_url: settings.merchant.support_url,
    public_url: settings.public_url,
  })

  // The public views, and the error envelope.
  await expectShape(t.checkout, "GET", `/public/payments/${payment.id}`)
  await expectShape(t.linkCheckout, "GET", `/public/links/${link.slug}`)
  await expectShape(t.checkout, "POST", `/public/links/${link.slug}/payments`, { amount: "3" }, 201, { "idempotency-key": `contract-link-${Date.now()}`, "x-forwarded-for": "10.44.0.1" })
  await expectShape(t.errorBody, "GET", "/payments/pay_00000000000000000000000000", undefined, 404)
  await expectShape(t.errorBody, "POST", "/payments", { amount: "-1" }, 422, { "idempotency-key": `contract-bad-${Date.now()}` })
  await expectShape(t.errorBody, "GET", "/nope", undefined, 404)

  // One message of the checkout stream is a checkout view too.
  const controller = new AbortController()
  const stream = await fetch(`${instance.url}/api/v1/public/payments/${payment.id}/stream`, { signal: controller.signal, headers: { "x-forwarded-for": "10.44.0.2" } })
  const reader = stream.body!.getReader()
  let text = ""
  while (!text.includes("\n\ndata:") && !/data: .*\n\n/.test(text)) text += new TextDecoder().decode((await reader.read()).value)
  controller.abort()
  const data = /data: (.*)\n/.exec(text)![1]
  expect(text).toMatch(/id: \d+\nevent: checkout\n/)
  issues.push(...check(t.checkout, JSON.parse(data), "stream message"))

  await api.must(204, "DELETE", `/webhook-endpoints/${endpoint.id}`)
  await api.must(204, "DELETE", `/payment-links/${link.id}`)
  expect(issues, "differences between the server's responses and src/lib/api/types.ts").toEqual([])
})
