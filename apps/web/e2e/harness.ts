/**
 * The end-to-end stack: a local chain (anvil) running the real BSC-USD token bytecode, the real
 * hmnpay binary with the interface built in, and a webhook receiver — all on loopback.
 *
 * Nothing here is mocked. Money moves as real ERC-20 transfers on the local chain; hmnpay sees
 * them through its own watcher; the browser talks to the binary exactly as in production.
 *
 * Needs: the binary (HMNPAY_E2E_BIN, default <repo>/hmnpay — "make build"), and Foundry
 * (anvil, cast). The token bytecode is read from e2e/.cache/usdt-bsc-runtime.hex and fetched
 * once from a public BSC node if it is not there (HMNPAY_E2E_USDT_CODE points at another copy).
 */
import { execFileSync, spawn, type ChildProcess } from "node:child_process"
import { createServer, type Server } from "node:http"
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import os from "node:os"
import path from "node:path"

export const REPO = path.resolve(__dirname, "../../..")
export const BIN = process.env.HMNPAY_E2E_BIN ?? path.join(REPO, "hmnpay")

/** Real BSC-USD (BEP20USDT). Storage layout: 0 owner, 1 balances, 3 totalSupply, 4 decimals, 5 symbol, 6 name. */
export const USDT = "0x55d398326f99059fF775485246999027B3197955"
const USDT_STORAGE: Record<string, string> = {
  "0x0": "0x000000000000000000000000f68a4b64162906eff0ff6ae34e2bb1cd42fef62d",
  "0x3": "0x00000000000000000000000000000000000000001dada59ce83bb99f2c8d17c1",
  "0x4": "0x0000000000000000000000000000000000000000000000000000000000000012",
  "0x5": "0x5553445400000000000000000000000000000000000000000000000000000008", // "USDT"
  "0x6": "0x5465746865722055534400000000000000000000000000000000000000000014", // "Tether USD"
}
/** The well-known Hardhat test mnemonic's account-level key: m/44'/60'/0'. Its first address is 0xf39F…2266. */
export const XPUB = "xpub6Ce9NcJvTk36xtLSrJLZqE7wtgA5deCeYs7rSQtreh4cj6ByPtrg9sD7V2FNFLPnf8heNP3FGkeV9qwfzvZNSd54JoNXVsXFYSYwHsnJxqP"
export const FIRST_ADDRESS = "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266"

export const OPERATOR = { email: "owner@example.com", password: "correct horse battery staple 42" }
export const MERCHANT = { name: "Northwind Digital", supportEmail: "billing@northwind.example.com" }

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

/** 18-decimal base units of a decimal string, as a 0x-prefixed 32-byte word. */
export function units(amount: string): bigint {
  const [whole, fraction = ""] = amount.split(".")
  return BigInt(whole + fraction.padEnd(18, "0").slice(0, 18))
}
const word = (value: bigint | string) => (typeof value === "bigint" ? value.toString(16) : value.toLowerCase().replace(/^0x/, "")).padStart(64, "0")

function tokenCode(): string {
  const override = process.env.HMNPAY_E2E_USDT_CODE
  const cached = override ?? path.join(__dirname, ".cache", "usdt-bsc-runtime.hex")
  if (existsSync(cached)) return readFileSync(cached, "utf8").trim()
  // One read-only call to a public node, then cached for good.
  const code = execFileSync("cast", ["code", USDT, "--rpc-url", "https://bsc-rpc.publicnode.com"], { encoding: "utf8" }).trim()
  if (code.length < 1000) throw new Error("could not fetch the BSC-USD bytecode; put it in e2e/.cache/usdt-bsc-runtime.hex")
  mkdirSync(path.dirname(cached), { recursive: true })
  writeFileSync(cached, code)
  return code
}

/** The commonly used USDT test token on BSC testnet: hmnpay's built-in asset for chain 97. */
export const USDT_TESTNET = "0x337610d27c682e347c9cd60bd4b3b107c9d34ddd"

/** A local chain: anvil with the token contract at `token`, one block a second. */
export class Chain {
  private anvil?: ChildProcess
  private payerSeq = 0

  constructor(
    readonly rpcUrl: string,
    readonly chainId: number,
    readonly token: string,
    /** Keeps payer addresses of different chains and workers apart. */
    private readonly salt: number,
  ) {}

  async rpc<T = unknown>(method: string, ...params: unknown[]): Promise<T> {
    const response = await fetch(this.rpcUrl, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
    })
    const body = (await response.json()) as { result?: T; error?: { message: string } }
    if (body.error) throw new Error(`${method}: ${body.error.message}`)
    return body.result as T
  }

  async start() {
    const port = new URL(this.rpcUrl).port
    this.anvil = spawn(
      "anvil",
      // Its own mnemonic: anvil's default accounts are the Hardhat ones, which are hmnpay's deposit addresses here.
      ["--host", "127.0.0.1", "--port", port, "--chain-id", String(this.chainId), "--block-time", "1", "--auto-impersonate", "--accounts", "2", "--silent",
       "--mnemonic", "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"],
      { stdio: "ignore" },
    )
    for (let i = 0; ; i++) {
      try {
        await this.rpc("eth_blockNumber")
        break
      } catch {
        if (i > 100) throw new Error("anvil did not start")
        await sleep(100)
      }
    }
    await this.rpc("anvil_setCode", this.token, tokenCode())
    for (const [slot, value] of Object.entries(USDT_STORAGE)) await this.rpc("anvil_setStorageAt", this.token, slot, value)
    return this
  }

  stop() {
    this.anvil?.kill("SIGTERM")
  }

  /** A fresh "customer" address holding `usdt` tokens and gas money. */
  async payer(usdt = "100000"): Promise<string> {
    const address = `0xa11ce${(this.salt * 1000 + ++this.payerSeq).toString(16).padStart(35, "0")}`
    const slot = execFileSync("cast", ["index", "address", address, "1"], { encoding: "utf8" }).trim()
    await this.rpc("anvil_setBalance", address, "0xde0b6b3a7640000")
    await this.rpc("anvil_setStorageAt", this.token, slot, `0x${word(units(usdt))}`)
    return address
  }

  /** A real token transfer of `amount` USDT to `to`. Returns the transaction hash. */
  async pay(to: string, amount: string, from?: string): Promise<string> {
    const sender = from ?? (await this.payer())
    return this.rpc<string>("eth_sendTransaction", { from: sender, to: this.token, data: `0xa9059cbb${word(to)}${word(units(amount))}`, gas: "0x30d40" })
  }
}

/**
 * An RPC endpoint that can be taken away: a plain HTTP relay in front of a chain's node which,
 * when `down`, drops every connection the way a dead provider does. hmnpay is given its URL as
 * one of its endpoints, so a test can make that endpoint fail and come back.
 */
export class Relay {
  down = false
  private server?: Server

  constructor(
    readonly url: string,
    private readonly target: string,
  ) {}

  start() {
    this.server = createServer((request, response) => {
      if (this.down) {
        request.socket.destroy()
        return
      }
      const chunks: Buffer[] = []
      request.on("data", (chunk: Buffer) => chunks.push(chunk))
      request.on("end", () => {
        fetch(this.target, { method: "POST", headers: { "content-type": "application/json" }, body: Buffer.concat(chunks) })
          .then(async (upstream) => {
            const body = Buffer.from(await upstream.arrayBuffer())
            if (this.down) return void request.socket.destroy()
            response.writeHead(upstream.status, { "content-type": "application/json" })
            response.end(body)
          })
          .catch(() => request.socket.destroy())
      })
    }).listen(Number(new URL(this.url).port), "127.0.0.1")
    return this
  }

  stop() {
    this.server?.closeAllConnections?.()
    this.server?.close()
  }
}

export interface WebhookHit {
  headers: Record<string, string | string[] | undefined>
  body: string
}

export class Stack {
  readonly url: string
  readonly rpcUrl: string
  /** hmnpay is configured with the node under two names, so the default verify mode has two endpoints. */
  readonly rpcUrls: string[]
  readonly hookUrl: string
  readonly dataDir: string
  setupToken = ""
  readonly hooks: WebhookHit[] = []
  /** What the receiver answers (a test can make it fail). */
  hookStatus = 200
  readonly chain: Chain
  private server?: ChildProcess
  private receiver?: Server
  private log = ""
  private extras: { stop(): void }[] = []

  constructor(readonly index: number) {
    const base = 18600 + index * 10
    this.url = `http://127.0.0.1:${base}`
    this.rpcUrl = `http://127.0.0.1:${base + 1}`
    this.rpcUrls = [this.rpcUrl, `http://localhost:${base + 1}`]
    this.hookUrl = `http://127.0.0.1:${base + 2}/hook`
    this.dataDir = mkdtempSync(path.join(os.tmpdir(), `hmnpay-e2e-${index}-`))
    this.chain = new Chain(this.rpcUrl, 56, USDT, index)
  }

  /** A port of this stack's own range (base + n; 0, 1 and 2 are taken by hmnpay, the node and the webhook receiver). */
  port(n: number) {
    return 18600 + this.index * 10 + n
  }

  /** A relay in front of this stack's node, to give hmnpay as an endpoint that can fail. Stopped with the stack. */
  relay(n = 3): Relay {
    const relay = new Relay(`http://127.0.0.1:${this.port(n)}`, this.rpcUrl).start()
    this.extras.push(relay)
    return relay
  }

  /** A second local chain: BSC testnet (chain 97) with its test USDT. Stopped with the stack. */
  async testnet(n = 4): Promise<Chain> {
    const chain = await new Chain(`http://127.0.0.1:${this.port(n)}`, 97, USDT_TESTNET, this.index + 500).start()
    this.extras.push(chain)
    return chain
  }

  /* --------------------------------- chain --------------------------------- */

  rpc<T = unknown>(method: string, ...params: unknown[]): Promise<T> {
    return this.chain.rpc<T>(method, ...params)
  }

  /** A fresh "customer" address holding `usdt` tokens and gas money. */
  payer(usdt = "100000"): Promise<string> {
    return this.chain.payer(usdt)
  }

  /** A real token transfer of `amount` USDT to `to`. Returns the transaction hash. */
  pay(to: string, amount: string, from?: string): Promise<string> {
    return this.chain.pay(to, amount, from)
  }

  /* --------------------------------- hmnpay -------------------------------- */

  private async startServer() {
    if (!existsSync(BIN)) throw new Error(`no hmnpay binary at ${BIN}: run "make build" first (or set HMNPAY_E2E_BIN)`)
    const { host } = new URL(this.url)
    this.server = spawn(BIN, ["serve"], {
      env: {
        PATH: process.env.PATH ?? "",
        HMNPAY_LISTEN: host,
        HMNPAY_DATA_DIR: this.dataDir,
        // The tests play several customers from one machine: they say who they are in X-Forwarded-For.
        HMNPAY_TRUSTED_PROXIES: "127.0.0.1/32",
        // The webhook receiver lives on loopback.
        HMNPAY_WEBHOOK_ALLOWED_PREFIXES: "127.0.0.1/32",
        HMNPAY_LOG_LEVEL: "warn",
      } as unknown as NodeJS.ProcessEnv,
      stdio: ["ignore", "ignore", "pipe"],
    })
    this.server.stderr?.on("data", (chunk: Buffer) => {
      this.log += chunk.toString()
    })
    for (let i = 0; ; i++) {
      try {
        if ((await fetch(`${this.url}/healthz`)).ok) break
      } catch {
        // not listening yet
      }
      if (i > 150) throw new Error(`hmnpay did not start:\n${this.log}`)
      await sleep(100)
    }
    // The first-run banner on stderr carries the setup token.
    for (let i = 0; i < 50 && !this.setupToken; i++) {
      const lines = this.log.split("\n")
      const at = lines.findIndex((line) => line.includes("enter this setup token"))
      this.setupToken = at >= 0 ? (lines.slice(at + 1).find((line) => line.trim() !== "")?.trim() ?? "") : ""
      if (!this.setupToken) await sleep(100)
    }
  }

  private startReceiver() {
    const { port } = new URL(this.hookUrl)
    this.receiver = createServer((request, response) => {
      const chunks: Buffer[] = []
      request.on("data", (chunk: Buffer) => chunks.push(chunk))
      request.on("end", () => {
        this.hooks.push({ headers: request.headers, body: Buffer.concat(chunks).toString() })
        // A path with "failing" in it plays a receiver that is down.
        const status = request.url?.includes("failing") ? 503 : this.hookStatus
        response.writeHead(status, { "content-type": "application/json" })
        response.end(status < 300 ? '{"ok":true}' : '{"error":"service unavailable"}')
      })
    }).listen(Number(port), "127.0.0.1")
  }

  async start() {
    await this.chain.start()
    this.startReceiver()
    await this.startServer()
    return this
  }

  async stop() {
    this.server?.kill("SIGTERM")
    this.chain.stop()
    for (const extra of this.extras) extra.stop()
    await new Promise<void>((resolve) => (this.receiver ? this.receiver.close(() => resolve()) : resolve()))
  }

  serverLog() {
    return this.log
  }

  /** `hmnpay recovery-code`: a one-time code, minted on the "server" while it runs. */
  recoveryCode(): string {
    const out = execFileSync(BIN, ["recovery-code", "--data-dir", this.dataDir], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] })
    const code = /hmnr_[A-Za-z0-9_-]+/.exec(out)?.[0]
    if (!code) throw new Error(`no recovery code in: ${out}`)
    return code
  }
}

/* ----------------------------------- API ----------------------------------- */

/** A plain API client for arranging and checking state from the test process (the operator's session cookie, like a browser). */
export class Api {
  cookie = ""
  constructor(
    readonly base: string,
    /** The client address this caller claims (the server trusts loopback as a proxy in the tests). */
    readonly ip = "10.9.0.1",
  ) {}

  async call<T = unknown>(method: string, route: string, body?: unknown, headers: Record<string, string> = {}): Promise<{ status: number; body: T; headers: Headers }> {
    const response = await fetch(`${this.base}/api/v1${route}`, {
      method,
      headers: {
        ...(body !== undefined ? { "content-type": "application/json" } : {}),
        ...(this.cookie ? { cookie: this.cookie } : {}),
        "x-forwarded-for": this.ip,
        ...headers,
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    })
    const setCookie = response.headers.getSetCookie?.() ?? []
    const session = setCookie.map((c) => c.split(";")[0]).filter((c) => c.startsWith("hmn_session=") && c !== "hmn_session=")
    if (session.length > 0) this.cookie = session[0]
    const text = await response.text()
    let parsed: unknown = text
    try {
      parsed = text ? JSON.parse(text) : null
    } catch {
      // not JSON
    }
    return { status: response.status, body: parsed as T, headers: response.headers }
  }

  /** The call must answer `status`; returns the body. */
  async must<T = unknown>(status: number, method: string, route: string, body?: unknown, headers?: Record<string, string>): Promise<T> {
    const result = await this.call<T>(method, route, body, headers)
    if (result.status !== status) throw new Error(`${method} ${route} = ${result.status}, want ${status}: ${JSON.stringify(result.body)}`)
    return result.body
  }

  /** First-run setup through the API, as the wizard does it. Signs this client in. */
  async setup(stack: Stack, rpcUrls: string[] = stack.rpcUrls) {
    await this.must(201, "POST", "/setup/complete", {
      email: OPERATOR.email,
      password: OPERATOR.password,
      chain_id: 56,
      rpc_urls: rpcUrls,
      xpub: XPUB,
      derivation_path: "m/44'/60'/0'/0",
      merchant_name: MERCHANT.name,
      support_email: MERCHANT.supportEmail,
      public_url: stack.url,
      setup_token: stack.setupToken,
    })
    // Fewer confirmations than the default keep the tests short and still show the counter moving.
    const settings = await this.must<{ payments: Record<string, unknown> }>(200, "GET", "/settings")
    await this.must(200, "PUT", "/settings/payments", { ...settings.payments, required_confirmations: 5 })
  }

  async login() {
    await this.must(200, "POST", "/auth/login", OPERATOR)
  }

  createPayment(input: Record<string, unknown>) {
    return this.must<PaymentBody>(201, "POST", "/payments", input, { "idempotency-key": `e2e-${Math.random().toString(36).slice(2)}-${Date.now()}` })
  }

  payment(id: string) {
    return this.must<PaymentBody>(200, "GET", `/payments/${id}`)
  }

  /** Waits until the payment reaches one of `statuses` (the watcher ticks every 3 seconds). */
  async waitForStatus(id: string, statuses: string[], timeoutMs = 90_000): Promise<PaymentBody> {
    const deadline = Date.now() + timeoutMs
    for (;;) {
      const payment = await this.payment(id)
      if (statuses.includes(payment.status)) return payment
      if (Date.now() > deadline) throw new Error(`payment ${id} is ${payment.status}, wanted ${statuses.join("/")}`)
      await sleep(500)
    }
  }

  /**
   * Waits until the chain watcher has completed a pass, is healthy, and has processed a block of
   * the instance's network (of `chainId` when given): before that the server creates no payment
   * (chain_not_ready).
   */
  async waitForNode(timeoutMs = 60_000, chainId?: number) {
    const deadline = Date.now() + timeoutMs
    for (;;) {
      const node = await this.must<{ healthy: boolean; starting: boolean; head: { chain_id: number; number: number } | null }>(200, "GET", "/node-status")
      if (node.healthy && !node.starting && node.head && node.head.number > 0 && (chainId === undefined || node.head.chain_id === chainId)) return node
      if (Date.now() > deadline) throw new Error(`node not healthy: ${JSON.stringify(node)}`)
      await sleep(500)
    }
  }
}

export interface PaymentBody {
  id: string
  status: string
  address: string
  amount: string
  amount_received: string
  amount_due: string
  checkout_url: string
  needs_attention: boolean
  attention: string
  confirmations: number
  required_confirmations: number
  /** Set while the payment has its confirmations and waits only for the chain to finalise its block. */
  awaiting_finality: { block: number; final: number } | null
  [key: string]: unknown
}
