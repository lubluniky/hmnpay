/**
 * Which RPC endpoint URLs are one endpoint, and which look like one provider: the server's rules
 * (core/endpoints.go), so that a screen can say so while the operator is still typing. The server
 * decides; these only let the page say it first.
 *
 * - One endpoint: the same scheme, host and port (a default port written out or left away is the
 *   same). The path, the query and user information, where providers put their keys, do not make
 *   another endpoint; nor do spellings of one host (letter case, a trailing dot, an
 *   internationalised name and its punycode, the forms of one IP address, an IPv4-mapped IPv6
 *   address). The server refuses such a pair (`same_endpoint`) by core/endpointid; this is the same
 *   rule, tested on the same cases, and the server's answer is what counts when settings are saved.
 * - One provider, as far as a name tells: hosts that share a registrable domain (the last two
 *   labels, three under a country code with a common second level). Accepted, with a warning.
 *   Addresses of this machine are not compared.
 */

function parse(raw: string): URL | null {
  try {
    return new URL(raw.trim())
  } catch {
    return null
  }
}

/**
 * The canonical host, as the server writes it: an IP address in its standard form (an IPv4-mapped
 * IPv6 address as its IPv4 address), a name in lower case, without trailing dots, each label that
 * is not ASCII in punycode. The URL parser does the lower case, the punycode and the IPv6 form.
 */
function canonicalHost(hostname: string): string {
  let h = hostname.toLowerCase().replace(/^\[|\]$/g, "")
  const mapped = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(h)
  if (mapped) {
    const [a, b] = [parseInt(mapped[1], 16), parseInt(mapped[2], 16)]
    return `${a >> 8}.${a & 255}.${b >> 8}.${b & 255}`
  }
  if (h.includes(":")) return `[${h}]`
  h = h.replace(/\.+$/, "")
  return h
}

/** "scheme://host[:port]" as the server names an endpoint (core/endpointid.Identity). */
export function endpointIdentity(raw: string): string {
  const u = parse(raw)
  if (!u || !u.hostname) return "invalid-url"
  const scheme = u.protocol.replace(/:$/, "").toLowerCase()
  // URL already drops a default port (for http, https, ws and wss) and leading zeros.
  return `${scheme}://${canonicalHost(u.hostname)}${u.port ? `:${u.port}` : ""}`
}

const COMMON_SECOND_LEVEL = new Set(["co", "com", "net", "org", "gov", "edu", "ac", "or", "ne", "go"])
const LOOPBACK = /^(127\.\d+\.\d+\.\d+|\[?::1\]?)$/

/** The registrable domain of a host, as far as the name tells ("" for this machine's addresses). */
export function providerDomain(host: string): string {
  const h = host.toLowerCase().replace(/\.+$/, "").replace(/^\[|\]$/g, "")
  if (!h || h === "localhost" || h.endsWith(".localhost") || LOOPBACK.test(h)) return ""
  if (/^\d+\.\d+\.\d+\.\d+$/.test(h) || h.includes(":")) return h
  const labels = h.split(".")
  const n = labels.length
  if (n < 2) return h
  if (n >= 3 && labels[n - 1].length === 2 && COMMON_SECOND_LEVEL.has(labels[n - 2])) return labels.slice(n - 3).join(".")
  return labels.slice(n - 2).join(".")
}

/** The URL in `list` that is the same endpoint as `url` (the very same URL included), if any. */
export function sameEndpointAs(url: string, list: string[]): string | undefined {
  const id = endpointIdentity(url)
  return list.find((other) => endpointIdentity(other) === id)
}

/** Groups of two or more URLs (as scheme://host[:port]) that look like one provider, in list order. */
export function sameProviderGroups(urls: string[]): string[][] {
  const byDomain = new Map<string, string[]>()
  for (const raw of urls) {
    const u = parse(raw)
    if (!u) continue
    const d = providerDomain(canonicalHost(u.hostname))
    if (!d) continue
    const label = endpointIdentity(raw)
    byDomain.set(d, [...(byDomain.get(d) ?? []), label])
  }
  return [...byDomain.values()].filter((group) => group.length > 1)
}

const hostOf = (label: string) => label.replace(/^[a-z]+:\/\//i, "")
const listOf = (items: string[]) => (items.length < 2 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`)

/** Said on the field of a URL that is the same endpoint as one already in the list. */
export function sameEndpointMessage(existing: string): string {
  return `This is the same endpoint as ${hostOf(endpointIdentity(existing))}, already in the list: only the path or the key differs, so it can’t check that endpoint’s answers. Add an endpoint of another provider.`
}

/** Said, as a warning, about endpoints that look like one provider. */
export function sameProviderMessage(group: string[]): string {
  const hosts = group.map(hostOf)
  return `${listOf(hosts)} look like the same provider. Endpoints are only a check on each other when different companies run them, so consider replacing ${hosts.length === 2 ? "one of them" : "some of them"} with an endpoint of another provider.`
}

/** Whether a list holds two URLs of one endpoint (settings saved by an earlier version can). */
export const hasEndpointTwice = (urls: string[]) => urls.some((url, i) => sameEndpointAs(url, urls.slice(0, i)) !== undefined)
