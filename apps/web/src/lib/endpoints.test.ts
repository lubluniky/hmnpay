import assert from "node:assert/strict"
import { test } from "node:test"

import { endpointIdentity, hasEndpointTwice, providerDomain, sameEndpointAs, sameEndpointMessage, sameProviderGroups, sameProviderMessage } from "./endpoints.ts"

// The cases of core/endpoints_test.go: the page says first what the server then decides.
test("two URLs of one scheme, host and port are one endpoint, whatever the path or the key", () => {
  for (const [a, b] of [
    ["https://bsc.example.org/v1/key-a", "https://bsc.example.org/v1/key-b"],
    ["https://bsc.example.org", "https://BSC.example.org:443/?apikey=x"],
    ["http://10.0.0.5:8545", "http://10.0.0.5:8545/rpc"],
  ]) {
    assert.equal(endpointIdentity(a), endpointIdentity(b), `${a} / ${b}`)
    assert.equal(sameEndpointAs(b, ["https://other.example.net", a]), a)
  }
  assert.notEqual(endpointIdentity("http://10.0.0.5:8545"), endpointIdentity("http://10.0.0.5:8546"), "another port")
  assert.notEqual(endpointIdentity("http://bsc.example.org"), endpointIdentity("https://bsc.example.org"), "another scheme")
  assert.equal(sameEndpointAs("http://127.0.0.1:8545", ["http://localhost:8545"]), undefined, "another name is another endpoint")
})

test("hosts of one registrable domain look like one provider; this machine's addresses are not compared", () => {
  const groups = sameProviderGroups([
    "https://bsc-dataseed1.binance.org", "https://bsc-rpc.publicnode.com/", "https://bsc-dataseed2.binance.org/key",
    "https://rpc.node.example.co.uk", "https://other.example.co.uk",
    "http://127.0.0.1:8545", "http://localhost:8546", "http://127.0.0.1:8547",
  ])
  assert.deepEqual(groups, [
    ["https://bsc-dataseed1.binance.org", "https://bsc-dataseed2.binance.org"],
    ["https://rpc.node.example.co.uk", "https://other.example.co.uk"],
  ])
  assert.deepEqual(sameProviderGroups(["https://bsc-dataseed.bnbchain.org", "https://bsc-rpc.publicnode.com"]), [])
  assert.equal(providerDomain("localhost"), "")
  assert.equal(providerDomain("a.b.example.com"), "example.com")
})

test("the words: plain, no key, no code", () => {
  const same = sameEndpointMessage("https://bsc.example.org/v1/secret-key")
  assert.match(same, /same endpoint as bsc\.example\.org, already in the list/)
  assert.doesNotMatch(same, /secret-key|same_endpoint/)
  const provider = sameProviderMessage(["https://bsc-dataseed1.binance.org", "https://bsc-dataseed2.binance.org"])
  assert.equal(
    provider,
    "bsc-dataseed1.binance.org and bsc-dataseed2.binance.org look like the same provider. Endpoints are only a check on each other when different companies run them, so consider replacing one of them with an endpoint of another provider.",
  )
})

test("a list that holds one endpoint twice is recognised", () => {
  assert.equal(hasEndpointTwice(["http://127.0.0.1:8545", "http://127.0.0.1:8545/second-key", "http://localhost:8545"]), true)
  assert.equal(hasEndpointTwice(["http://127.0.0.1:8545", "http://localhost:8545"]), false)
})

// The cases of core/endpointid/endpointid_test.go: the page names endpoints as the server does.
test("spellings of one host are one endpoint, as the server's identity has it", () => {
  const one = [
    ["https://abc.example.org", "https://ABC.example.org:443", "https://abc.example.org.", "https://abc.example.org/v1/KEY", "https://user:pw@abc.example.org:0443/?k=1"],
    ["https://bücher.example", "https://xn--bcher-kva.example", "https://BÜCHER.example."],
    ["https://[2001:db8::1]", "https://[2001:0db8::1]", "https://[2001:db8:0:0:0:0:0:1]:443"],
    ["http://192.0.2.7", "http://[::ffff:192.0.2.7]", "http://192.0.2.7:80"],
  ]
  for (const group of one) for (const u of group.slice(1)) assert.equal(endpointIdentity(u), endpointIdentity(group[0]), `${group[0]} / ${u}`)
  for (const [a, b] of [["https://abc.example.org", "http://abc.example.org"], ["https://abc.example.org", "https://abc.example.org:444"], ["https://abc.example.org", "https://abd.example.org"]]) {
    assert.notEqual(endpointIdentity(a), endpointIdentity(b), `${a} / ${b}`)
  }
  assert.equal(endpointIdentity("not a url"), "invalid-url")
  // The names the server writes into the node status.
  assert.equal(endpointIdentity("https://BÜCHER.example./key"), "https://xn--bcher-kva.example")
  assert.equal(endpointIdentity("http://[::ffff:192.0.2.7]:8545/rpc"), "http://192.0.2.7:8545")
  assert.equal(endpointIdentity("https://[2001:0db8::1]:8443"), "https://[2001:db8::1]:8443")
})
