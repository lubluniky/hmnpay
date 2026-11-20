/**
 * First-run setup: step model, form state and the checks that can be made before asking the
 * server. The server's answers (setup status, RPC probes, derived addresses, the final
 * validation) come from the API; see wizard.tsx.
 */
import type { ProbeResult } from "@/lib/api/types"

export const STEP_IDS = ["account", "network", "wallet", "business", "done"] as const
export type StepId = (typeof STEP_IDS)[number]

export const STEPS: Record<StepId, { label: string; short: string }> = {
  account: { label: "Admin account", short: "Account" },
  network: { label: "Network", short: "Network" },
  wallet: { label: "Wallet", short: "Wallet" },
  business: { label: "Your business", short: "Business" },
  done: { label: "Ready", short: "Ready" },
}

export function isStepId(value: string | undefined): value is StepId {
  return STEP_IDS.includes(value as StepId)
}

/* ------------------------------- account ---------------------------------- */

/** The server's minimum (GET /api/v1/setup says the real one; this is the fallback until it has answered). */
export const PASSWORD_MIN_LENGTH = 12

export const isEmail = (value: string) => /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value.trim())

export interface PasswordStrength {
  /** 0 = empty, 1 weak … 4 strong. */
  score: 0 | 1 | 2 | 3 | 4
  label: string
  tone: "neutral" | "danger" | "warning" | "success"
  /** What to do next, or why it is good. */
  hint: string
}

/**
 * A rough estimate from length and variety, to nudge towards a passphrase. The rule that is
 * enforced is the server's: at least `minLength` characters.
 */
export function passwordStrength(password: string, minLength = PASSWORD_MIN_LENGTH): PasswordStrength {
  const PASSWORD_MIN_LENGTH = minLength
  if (!password) {
    return { score: 0, label: "", tone: "neutral", hint: `At least ${PASSWORD_MIN_LENGTH} characters. A passphrase works well.` }
  }
  const classes = [/[a-z]/, /[A-Z]/, /\d/, /[^A-Za-z0-9]/].filter((re) => re.test(password)).length
  const unique = new Set(password).size
  if (password.length < PASSWORD_MIN_LENGTH) {
    const left = PASSWORD_MIN_LENGTH - password.length
    return { score: 1, label: "Too short", tone: "danger", hint: `${left} more character${left === 1 ? "" : "s"} to go.` }
  }
  if (unique < 6) return { score: 1, label: "Weak", tone: "danger", hint: "Too repetitive. Mix in different words." }
  if (password.length >= 20 || (password.length >= 16 && classes >= 2)) {
    return { score: 4, label: "Strong", tone: "success", hint: "Long and hard to guess. Keep it in a password manager." }
  }
  if (classes >= 3 || password.length >= 16) {
    return { score: 3, label: "Good", tone: "success", hint: "Good. A few more characters make it strong." }
  }
  return { score: 2, label: "Fair", tone: "warning", hint: "Add another word, a number or a symbol." }
}

/* ------------------------------- network ---------------------------------- */

export type CheckId = "reach" | "chain" | "block" | "token"
export const CHECK_IDS: CheckId[] = ["reach", "chain", "block", "token"]

export const CHECK_LABEL: Record<CheckId, string> = {
  reach: "Endpoint responds",
  chain: "Chain ID",
  block: "Latest block",
  token: "Token contract",
}

export type CheckState = "todo" | "running" | "ok" | "fail" | "skipped"

/** What is known about one RPC endpoint the operator entered. */
export interface RpcProbe {
  /** `idle` = never tested (or the URL changed since). */
  status: "idle" | "testing" | "ok" | "error"
  /** The server's probe result, once there is one. */
  result: ProbeResult | null
  /** The probe itself could not be made (rate limit, the URL was refused, the server is down). */
  failure: string | null
}

export const IDLE_PROBE: RpcProbe = { status: "idle", result: null, failure: null }

export const isUrl = (value: string) => /^https?:\/\/[^\s/]+/.test(value.trim())

export function hostOf(url: string) {
  return url.trim().replace(/^[a-z]+:\/\//i, "").replace(/[/?#].*$/, "") || "—"
}

/** Each check of the readout, from the probe's facts: everything up to the first failure passed. */
export function checkStates(probe: RpcProbe): Record<CheckId, CheckState> {
  const states = { reach: "todo", chain: "todo", block: "todo", token: "todo" } as Record<CheckId, CheckState>
  if (probe.status === "idle") return states
  if (probe.status === "testing") {
    for (const id of CHECK_IDS) states[id] = "running"
    return states
  }
  const outcome = probe.result?.outcome
  const failAt = !probe.result || outcome === "unreachable" ? 0 : outcome === "wrong_chain" ? 1 : outcome === "token_mismatch" ? 3 : CHECK_IDS.length
  CHECK_IDS.forEach((id, i) => (states[id] = i < failAt ? "ok" : i === failAt ? "fail" : "skipped"))
  return states
}

/** A public endpoint to start from, per chain. Only a suggestion: any endpoint of the chain works. */
export const SUGGESTED_RPC: Record<number, string> = {
  56: "https://bsc-rpc.publicnode.com",
  97: "https://bsc-testnet-rpc.publicnode.com",
}

/* -------------------------------- wallet ---------------------------------- */

export type DerivationPath = "bip44" | "ledger-legacy"

export const DERIVATION_PATHS: Record<DerivationPath, { label: string; path: string; wallets: string }> = {
  bip44: { label: "Standard (BIP-44)", path: "m/44'/60'/0'/0", wallets: "MetaMask, Trezor, most wallets" },
  "ledger-legacy": { label: "Ledger legacy", path: "m/44'/60'/0'", wallets: "Older Ledger setups, MEW" },
}

export const XPUB_LENGTH = 111

export type XpubCheck =
  | { ok: true }
  | { ok: false; reason: "empty" }
  | { ok: false; reason: "private" | "address" | "prefix" | "charset" | "length"; message: string }

/** Shape checks only; the server verifies the key itself when it derives the addresses. */
export function checkXpub(raw: string): XpubCheck {
  const value = raw.replace(/\s+/g, "")
  if (!value) return { ok: false, reason: "empty" }
  if (/^[xyz]prv/i.test(value)) {
    return {
      ok: false,
      reason: "private",
      message: "That is a private key. Remove it and paste the public key instead. It starts with “xpub”.",
    }
  }
  if (/^0x[0-9a-f]*$/i.test(value)) {
    return {
      ok: false,
      reason: "address",
      message: "That looks like a single address. hmnpay needs the extended public key, which starts with “xpub”.",
    }
  }
  if (!value.startsWith("xpub")) {
    return { ok: false, reason: "prefix", message: "An extended public key starts with “xpub”. Check what you copied." }
  }
  if (!/^[1-9A-HJ-NP-Za-km-z]+$/.test(value)) {
    return {
      ok: false,
      reason: "charset",
      message: "This key has characters an xpub can’t contain. Copy it again from your wallet.",
    }
  }
  if (value.length !== XPUB_LENGTH) {
    return {
      ok: false,
      reason: "length",
      message:
        value.length < XPUB_LENGTH
          ? `This key is cut short: ${value.length} of ${XPUB_LENGTH} characters. Copy it again from your wallet.`
          : `This key is too long: ${value.length} characters, expected ${XPUB_LENGTH}. Check for anything pasted twice.`,
    }
  }
  return { ok: true }
}

/* --------------------------------- form ----------------------------------- */

export interface SetupForm {
  /** The first-run setup token from the server log. */
  setupToken: string
  email: string
  password: string
  chainId: number
  /** The endpoint being typed (tested ones are in `endpoints`). */
  rpcUrl: string
  /** Endpoints that passed the probe, in the order they were added. */
  endpoints: string[]
  xpub: string
  derivation: DerivationPath
  /** The operator ticked "these match my wallet". Cleared whenever xpub or path changes. */
  addressesVerified: boolean
  merchantName: string
  supportEmail: string
  /** The logo as an inline image (data: URL), or null. */
  logoUrl: string | null
}

export function stepComplete(step: StepId, form: SetupForm, facts: { tokenRequired: boolean; minPassword: number; derived: boolean }): boolean {
  switch (step) {
    case "account":
      return (
        (!facts.tokenRequired || form.setupToken.trim().length > 0) &&
        isEmail(form.email) &&
        form.password.length >= facts.minPassword &&
        passwordStrength(form.password, facts.minPassword).score >= 2
      )
    case "network":
      return form.endpoints.length > 0
    case "wallet":
      return checkXpub(form.xpub).ok && facts.derived && form.addressesVerified
    case "business":
      return form.merchantName.trim().length > 0 && isEmail(form.supportEmail)
    case "done":
      return true
  }
}
