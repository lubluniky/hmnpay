"use client"

import * as React from "react"
import { useRouter, useSearchParams } from "next/navigation"
import { ServerIcon } from "lucide-react"
import { AnimatePresence, MotionConfig, motion } from "motion/react"

import { FlowFooter } from "@/components/auth/flow"
import { DURATION, EASE, ErrorState, Logo, SoundProvider, WaveTransition } from "@/components/hmn"
import { api, isApiError } from "@/lib/api/client"
import type { DerivedAddress, SetupStatus } from "@/lib/api/types"
import { useQuery } from "@/lib/api/use-query"
import { describeError, fieldErrors } from "@/lib/errors"
import { sameEndpointAs } from "@/lib/endpoints"
import { truncateMiddle } from "@/lib/format"
import { useHydrated } from "@/lib/use-route"

import {
  checkXpub,
  DERIVATION_PATHS,
  IDLE_PROBE,
  isStepId,
  isUrl,
  PASSWORD_MIN_LENGTH,
  STEP_IDS,
  STEPS,
  stepComplete,
  SUGGESTED_RPC,
  type RpcProbe,
  type SetupForm,
  type StepId,
} from "./data"
import { SetupRail, SetupSegments, stepHref, StepNav, type RailItem } from "./parts"
import { StepAccount } from "./step-account"
import { StepBusiness } from "./step-business"
import { StepDone } from "./step-done"
import { StepNetwork } from "./step-network"
import { StepWallet } from "./step-wallet"

/** What the server derived from the xpub the operator pasted. */
export interface Derived {
  /** The xpub and path the addresses belong to; anything else on screen is stale. */
  key: string
  status: "loading" | "ok" | "error"
  addresses: DerivedAddress[]
  error: string | null
}

/** What the server said about fields when setup was submitted, by request key ("rpc_urls.0", "xpub", …). */
export type ServerErrors = Record<string, string>

/** Which step a server field error belongs to. */
function stepOfField(field: string): StepId {
  if (field === "email" || field === "password" || field === "setup_token") return "account"
  if (field === "chain_id" || field.startsWith("rpc_urls") || field === "asset") return "network"
  if (field === "xpub" || field === "derivation_path") return "wallet"
  return "business"
}

/**
 * Steps slide the way you are going: Continue brings the next one in from the right, Back
 * brings the previous one in from the left. `direction` is 1 (forward) or -1.
 */
const STEP_MOTION = {
  enter: (direction: number) => ({ opacity: 0, x: direction * 16 }),
  rest: { opacity: 1, x: 0, transition: { duration: DURATION.slow, ease: EASE.out } },
  exit: (direction: number) => ({ opacity: 0, x: direction * -10, transition: { duration: DURATION.fast, ease: EASE.in } }),
}

const derivedKey = (form: Pick<SetupForm, "xpub" | "derivation">) => `${form.derivation}:${form.xpub}`

/**
 * First-run setup — the first screen after `docker compose up`. No dashboard chrome.
 *
 * Everything the wizard shows as a fact comes from the server: whether a setup token is needed
 * and the password rule (GET /setup), what each RPC endpoint really answers (POST
 * /setup/probe-rpc), the addresses the pasted key derives (POST /setup/preview-addresses). The
 * last step submits it all (POST /setup/complete), which also signs the new operator in.
 * The form lives in memory only: a reload starts over, and the password and token are never
 * stored anywhere in the browser.
 */
export function SetupWizard() {
  const params = useSearchParams()
  const router = useRouter()
  const hydrated = useHydrated()
  const status = useQuery("setup", (signal) => api.setupStatus(signal))
  const facts: SetupStatus | undefined = status.data
  const tokenRequired = facts?.setup_token_required ?? true
  const minPassword = facts?.min_password_length ?? PASSWORD_MIN_LENGTH
  const defaultChain = facts?.defaults.chain_id ?? 56

  const [form, setForm] = React.useState<SetupForm>({
    setupToken: "",
    email: "",
    password: "",
    chainId: 0,
    rpcUrl: "",
    endpoints: [],
    xpub: "",
    derivation: "bip44",
    addressesVerified: false,
    merchantName: "",
    supportEmail: "",
    logoUrl: null,
  })
  // The chain and its suggested endpoint follow the server's defaults until the operator chooses.
  const chainId = form.chainId || defaultChain
  const [suggested, setSuggested] = React.useState(false)
  if (facts && !suggested) {
    setSuggested(true)
    if (!form.rpcUrl) setForm((current) => ({ ...current, rpcUrl: SUGGESTED_RPC[defaultChain] ?? "" }))
  }

  const [probe, setProbe] = React.useState<RpcProbe>(IDLE_PROBE)
  const [derived, setDerived] = React.useState<Derived | null>(null)
  const [serverErrors, setServerErrors] = React.useState<ServerErrors>({})
  /** A refusal that belongs to no field (rate limit, server down), shown on the step it happened on. */
  const [failure, setFailure] = React.useState<string | null>(null)
  const [busy, setBusy] = React.useState(false)
  const [finished, setFinished] = React.useState(false)
  /** Steps the operator tried to leave while incomplete: their errors stay visible. */
  const [attempted, setAttempted] = React.useState<Partial<Record<StepId, boolean>>>({})

  const set = React.useCallback((patch: Partial<SetupForm>) => {
    setForm((current) => ({ ...current, ...patch }))
    setFailure(null)
    // A different endpoint is an untested endpoint; another chain makes every endpoint untested.
    if (patch.rpcUrl !== undefined || patch.chainId !== undefined) setProbe(IDLE_PROBE)
    setServerErrors((current) => (Object.keys(current).length === 0 ? current : {}))
  }, [])

  /* The addresses of the pasted key, asked from the server a moment after typing stops. */
  const xpubOk = checkXpub(form.xpub).ok
  const wantKey = xpubOk ? derivedKey(form) : null
  const token = form.setupToken.trim()
  React.useEffect(() => {
    if (wantKey === null) return
    const controller = new AbortController()
    const timer = setTimeout(() => {
      setDerived({ key: wantKey, status: "loading", addresses: [], error: null })
      api
        .previewAddresses({ xpub: form.xpub, derivation_path: DERIVATION_PATHS[form.derivation].path, count: 3 }, token, controller.signal)
        .then((list) => setDerived({ key: wantKey, status: "ok", addresses: list.data, error: null }))
        .catch((error) => {
          if (controller.signal.aborted) return
          const fields = fieldErrors(error)
          setDerived({ key: wantKey, status: "error", addresses: [], error: fields.xpub ?? fields.derivation_path ?? describeError(error) })
        })
    }, 350)
    return () => {
      clearTimeout(timer)
      controller.abort()
    }
    // form.xpub and form.derivation are what wantKey is made of.
  }, [wantKey, token]) // eslint-disable-line react-hooks/exhaustive-deps
  const currentDerived = derived && derived.key === wantKey ? derived : null

  const complete = (step: StepId) => stepComplete(step, form, { tokenRequired, minPassword, derived: currentDerived?.status === "ok" })
  const firstOpen = STEP_IDS.findIndex((id) => id !== "done" && !complete(id))
  const frontier = finished ? STEP_IDS.length - 1 : firstOpen === -1 ? STEP_IDS.indexOf("business") : firstOpen

  // The step the URL asks for, but never past what has been filled in, and "done" only once it is.
  const requested = params.get("step") ?? undefined
  const wanted = finished ? "done" : isStepId(requested) && requested !== "done" ? requested : "account"
  const active: StepId = STEP_IDS[Math.min(STEP_IDS.indexOf(wanted), frontier)]

  // An instance that is already set up has nothing to ask here.
  const alreadyDone = facts?.completed === true && !finished
  React.useEffect(() => {
    if (alreadyDone) window.location.replace("/login")
  }, [alreadyDone])

  // Finishing setup is the wizard's one loud moment: a wave leaves "Finish setup" and carries the
  // last step into the ready state. `before` is the step that was showing; it follows every
  // other step change and holds still once setup is done.
  const done = active === "done"
  const [before, setBefore] = React.useState(active)
  const [direction, setDirection] = React.useState(1)
  if (!done && before !== active) {
    setDirection(STEP_IDS.indexOf(active) > STEP_IDS.indexOf(before) ? 1 : -1)
    setBefore(active)
  }

  /* ---------------------------------- actions --------------------------------- */

  /** Is this the server's setup token? Asked with a call that needs it and changes nothing. */
  const checkToken = async (): Promise<boolean> => {
    if (!tokenRequired) return true
    setBusy(true)
    try {
      await api.previewAddresses({ xpub: "", derivation_path: "", count: 1 }, token)
      return true
    } catch (error) {
      if (isApiError(error) && (error.code === "setup_token_invalid" || error.code === "setup_token_required")) {
        setServerErrors({ setup_token: describeError(error) })
        return false
      }
      if (isApiError(error) && error.status === 409) {
        window.location.replace("/login") // somebody completed setup meanwhile
        return false
      }
      // 422: the token was accepted and the (empty) key was not, which is all this asks.
      if (isApiError(error) && error.status === 422) return true
      setFailure(describeError(error))
      return false
    } finally {
      setBusy(false)
    }
  }

  const runTest = async () => {
    const url = form.rpcUrl.trim()
    if (!isUrl(url) || (!form.endpoints.includes(url) && sameEndpointAs(url, form.endpoints))) {
      // Not a URL, or another URL of an endpoint already in the list: the field says why.
      setAttempted((a) => ({ ...a, network: true }))
      return
    }
    setProbe({ status: "testing", result: null, failure: null })
    try {
      const result = await api.probeRpc({ url, chain_id: chainId }, token)
      if (result.outcome === "ok") {
        setProbe({ status: "ok", result, failure: null })
        setForm((current) => (current.endpoints.includes(url) ? current : { ...current, endpoints: [...current.endpoints, url] }))
      } else setProbe({ status: "error", result, failure: null })
    } catch (error) {
      const fields = fieldErrors(error)
      setProbe({ status: "error", result: null, failure: fields.url ?? describeError(error) })
    }
  }

  const finish = async () => {
    setBusy(true)
    setFailure(null)
    try {
      await api.completeSetup(
        {
          email: form.email.trim(),
          password: form.password,
          chain_id: chainId,
          rpc_urls: form.endpoints,
          xpub: form.xpub,
          derivation_path: DERIVATION_PATHS[form.derivation].path,
          merchant_name: form.merchantName.trim(),
          support_email: form.supportEmail.trim(),
          ...(form.logoUrl ? { logo_url: form.logoUrl } : {}),
        },
        token,
      )
      // The server has signed the new operator in (session cookie). The secrets are not needed any more.
      setFinished(true)
      setForm((current) => ({ ...current, password: "", setupToken: "" }))
      router.replace(stepHref("done"))
    } catch (error) {
      const fields = fieldErrors(error)
      if (isApiError(error) && (error.code === "setup_token_invalid" || error.code === "setup_token_required")) {
        fields.setup_token = describeError(error)
      }
      const names = Object.keys(fields)
      if (names.length > 0) {
        setServerErrors(fields)
        // Go to the first step that has something to fix.
        const target = STEP_IDS.find((id) => names.some((name) => stepOfField(name) === id)) ?? "business"
        setAttempted((a) => ({ ...a, [target]: true }))
        if (target !== "business") router.push(stepHref(target))
      } else if (isApiError(error) && error.code === "setup_completed") {
        window.location.replace("/login")
      } else setFailure(describeError(error))
    } finally {
      setBusy(false)
    }
  }

  /* ----------------------------------- card ----------------------------------- */

  /** The whole card (rail + step) as it reads at `step`. */
  const card = (step: StepId) => {
    const index = STEP_IDS.indexOf(step)
    const next = STEP_IDS[index + 1]
    const back = index > 0 && step !== "done" ? STEP_IDS[index - 1] : undefined

    const onSubmit = async (event: React.FormEvent) => {
      event.preventDefault()
      if (step === "done" || busy) return
      if (step === "network") {
        // An address in the field that has not been tested yet: test it rather than drop it.
        const typed = form.rpcUrl.trim()
        if (typed && !form.endpoints.includes(typed) && sameEndpointAs(typed, form.endpoints)) {
          setAttempted((a) => ({ ...a, network: true }))
          return
        }
        if (typed && !form.endpoints.includes(typed) && probe.status !== "error") {
          void runTest()
          return
        }
      }
      if (!complete(step)) {
        setAttempted((a) => ({ ...a, [step]: true }))
        return
      }
      if (step === "account" && !(await checkToken())) {
        setAttempted((a) => ({ ...a, account: true }))
        return
      }
      if (step === "business") {
        void finish()
        return
      }
      router.push(stepHref(next))
    }

    /* Rail: everything before the current step reads as done, with a one-line receipt. */
    const summaries: Record<StepId, React.ReactNode> = {
      account: form.email,
      network: (
        <>
          Chain <span className="num">{chainId}</span>, <span className="num">{form.endpoints.length}</span>{" "}
          {form.endpoints.length === 1 ? "endpoint" : "endpoints"}
        </>
      ),
      wallet: (
        <>
          Watch-only, <span className="num">{truncateMiddle(form.xpub, 8, 4)}</span>
        </>
      ),
      business: form.merchantName,
      done: undefined,
    }
    const rail: RailItem[] = STEP_IDS.map((id, i) => ({
      id,
      state: i === index && step !== "done" ? "current" : (i < index || step === "done") && (step === "done" || complete(id)) ? "done" : "todo",
      summary: step === "done" && (id === "account" || id === "wallet") ? undefined : summaries[id],
      // Once setup is finished the steps are a receipt, not navigation.
      reachable: step !== "done" && i <= frontier,
    }))

    const nav =
      step === "account"
        ? { hint: undefined, busy }
        : step === "network"
          ? {
              hint:
                probe.status === "error"
                  ? "Fix the endpoint, or clear the field, to continue"
                  : form.endpoints.length === 0
                    ? "Test an endpoint first"
                    : undefined,
              busy: probe.status === "testing",
            }
          : step === "wallet"
            ? {
                hint:
                  xpubOk && currentDerived?.status === "ok" && !form.addressesVerified
                    ? "Confirm the addresses match"
                    : xpubOk && currentDerived?.status !== "ok" && currentDerived?.status !== "error"
                      ? "Deriving addresses"
                      : undefined,
                busy: false,
              }
            : { hint: busy ? "Saving" : undefined, busy }

    const showErrors = Boolean(attempted[step])

    return (
      <>
        {/* Desktop rail */}
        <aside className="hidden flex-col border-r border-border bg-background/70 p-8 lg:flex dark:bg-background/55">
          <Logo />
          <div className="mt-12">
            <h2 className="text-base leading-6 font-medium">{step === "done" ? "Setup complete" : "Set up your gateway"}</h2>
            <p className="mt-0.5 text-[13px] leading-5 text-muted-foreground">
              {step === "done" ? "Saved. Change any of it in Settings." : "Five short steps, about three minutes."}
            </p>
          </div>
          <nav aria-label="Setup steps" className="mt-8">
            <SetupRail items={rail} />
          </nav>
          <p className="mt-auto flex gap-2.5 pt-10 text-xs leading-[18px] text-pretty text-faint">
            <ServerIcon aria-hidden="true" className="mt-px size-3.5 shrink-0" />
            <span>Everything you enter stays on this server. hmnpay doesn’t phone home.</span>
          </p>
        </aside>

        <div className="relative flex min-w-0 flex-col">
          {/* Mobile progress */}
          <div className="border-b border-border bg-background/70 px-5 pt-4 pb-4 sm:px-6 lg:hidden dark:bg-background/55">
            <div className="flex items-center justify-between gap-3">
              <Logo size="sm" />
              <span className="text-[13px] leading-5 text-muted-foreground">
                <span className="font-medium text-foreground">{STEPS[step].short}</span>
                <span className="mx-1.5 text-faint" aria-hidden="true">
                  ·
                </span>
                <span className="num">
                  {index + 1}/{STEP_IDS.length}
                </span>
              </span>
            </div>
            <SetupSegments step={step} className="mt-3.5" />
          </div>

          <AnimatePresence mode="wait" initial={false} custom={direction}>
            <motion.form
              key={step}
              noValidate
              onSubmit={onSubmit}
              data-step={step}
              className="flex flex-1 flex-col p-5 sm:p-6 lg:p-10"
              custom={direction}
              variants={STEP_MOTION}
              initial="enter"
              animate="rest"
              exit="exit"
            >
              {step === "account" && (
                <StepAccount
                  form={form}
                  set={set}
                  showErrors={showErrors}
                  host={hydrated ? window.location.host : ""}
                  tokenRequired={tokenRequired}
                  minPassword={minPassword}
                  errors={serverErrors}
                />
              )}
              {step === "network" && (
                <StepNetwork
                  form={form}
                  set={set}
                  chainId={chainId}
                  chains={facts?.supported_chains ?? []}
                  probe={probe}
                  onTest={() => void runTest()}
                  showErrors={showErrors}
                  errors={serverErrors}
                />
              )}
              {step === "wallet" && <StepWallet form={form} set={set} showErrors={showErrors} derived={currentDerived} errors={serverErrors} />}
              {step === "business" && (
                <StepBusiness form={form} set={set} showErrors={showErrors} host={hydrated ? window.location.host : ""} errors={serverErrors} />
              )}
              {step === "done" && <StepDone merchantName={form.merchantName} asset={facts?.defaults.asset.symbol ?? "USDT"} />}

              {failure && step !== "done" && (
                <p role="alert" className="mt-5 rounded-lg border border-danger-border bg-danger-subtle px-3.5 py-2.5 text-[13px] leading-5 text-danger-fg">
                  {failure}
                </p>
              )}

              {step !== "done" && <StepNav back={back} label={step === "business" ? "Finish setup" : "Continue"} hint={nav.hint} busy={nav.busy} />}
            </motion.form>
          </AnimatePresence>
        </div>
      </>
    )
  }

  return (
    <SoundProvider surface="dashboard">
      <MotionConfig reducedMotion="user">
        <div className="flex min-h-svh flex-col items-center px-3 pt-3 pb-6 sm:px-6 sm:pt-10 lg:pt-[clamp(2.5rem,calc((100svh-47rem)/2),9rem)] lg:pb-10">
          <div className="w-full max-w-[520px] lg:max-w-[1000px]">
            <main>
              {status.error && !facts ? (
                <ErrorState error={status.error} title="Can’t reach this hmnpay server" onRetry={status.reload} className="bg-card py-24" />
              ) : (
                <WaveTransition
                  revealed={done}
                  armed={active === "business"}
                  cue="created"
                  className="overflow-hidden rounded-2xl border border-border bg-card shadow-raised"
                  // 618 + the card's two hairlines = the 620px card.
                  layerClassName="lg:grid lg:min-h-[618px] lg:grid-cols-[300px_minmax(0,1fr)]"
                  revealedContent={card("done")}
                >
                  {card(before)}
                </WaveTransition>
              )}
            </main>
            <FlowFooter />
          </div>
        </div>
      </MotionConfig>
    </SoundProvider>
  )
}
