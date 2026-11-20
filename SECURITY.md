# Security policy

hmnpay handles other people's payments, so reports are taken seriously and
answered quickly.

## Reporting a vulnerability

Please report privately, not in a public issue:

1. Open the repository's **Security** tab on GitHub.
2. Choose **Report a vulnerability** (GitHub private vulnerability
   reporting).

Include what you can of: the version or commit, how the instance is
configured (number of RPC endpoints, verify mode, behind a reverse proxy or
not), the steps to reproduce, and what an attacker gains. A failing test or
a small script is the best possible report.

You will get an answer within three working days. Please give us a
reasonable time to release a fix before publishing details.

## Supported versions

hmnpay is in beta. Only the latest release (and the `main` branch) receives
security fixes.

## What is in scope

- Crediting a payment that did not happen, losing or reverting one that
  did, confirming or expiring one early — in the configurations the README
  section "Trust in RPC endpoints" says are protected.
- Authentication and authorisation: the operator account, sessions, API
  keys, first-run setup, the recovery code.
- The HTTP API and the web UI: injection, request forgery, information
  leaks between the public checkout and the merchant's data.
- Webhooks: signature forgery, requests to networks the egress policy is
  meant to block.
- Anything that makes the server reveal secrets it stores (password and
  key hashes, webhook signing secrets, RPC URLs with provider keys).

## What is not a vulnerability

- Behaviour the README documents as a limit of the chosen configuration —
  for example, a single RPC endpoint must be trusted completely.
- Loss of funds through the merchant's own wallet: hmnpay is non-custodial.
  It only ever sees an extended public key and cannot move funds.
- Denial of service that needs the operator's credentials.

## What has and has not been checked

Stated plainly, so that nobody assumes more than is true: hmnpay has **not**
had an independent third-party security audit. What it has had:

- Six rounds of adversarial review, run by the project itself — not
  independent. They looked hardest at the chain watcher and its trust in
  RPC endpoints (lying, failing, stale and duplicated endpoints, finality,
  reorganisations, the server clock, a lost checkpoint) and at the payment
  engine's rules for settling payments and letting addresses go; the HTTP
  API, the web interface and the payment pages were reviewed as well, in
  less depth.
- Fuzzing of the parsers (addresses, RPC answers, strict JSON, key
  derivation) and of the chain watcher's checkpoint.
- Randomised models: the watcher, alone and together with the payment
  engine, is run against generated histories of lying and failing
  endpoints, reorganisations, finality that stalls or changes, outages,
  crashes and wrong server clocks — 30,000 histories of each model before a
  release.
- The test suites, including end-to-end tests of the web interface against
  the real binary and a local chain, run on macOS and Linux.
- The whole flow has been exercised on BSC mainnet with small amounts and
  against public RPC providers.

That reduces risk; it does not remove it. What hmnpay knowingly does not
handle is listed under "Known limits" in the README. Start with amounts you
can afford to have delayed.
