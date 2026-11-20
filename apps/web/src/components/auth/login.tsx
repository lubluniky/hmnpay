"use client"

import * as React from "react"
import Link from "next/link"
import { useSearchParams } from "next/navigation"
import { ArrowLeftIcon, CircleAlertIcon, ServerIcon, TerminalIcon } from "lucide-react"
import { AnimatePresence, MotionConfig, motion } from "motion/react"

import { Callout, CopyButton, LogoMark, SoundProvider } from "@/components/hmn"
import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"
import { api, isApiError } from "@/lib/api/client"
import { clearQueryCache } from "@/lib/api/use-query"
import { describeError } from "@/lib/errors"
import { safeNextPath } from "@/lib/url"
import { useHydrated } from "@/lib/use-route"
import { cn } from "@/lib/utils"

import { FlowField, FlowFooter, FlowInput, PasswordInput, useAutoFocus } from "./flow"

export type LoginView = "signin" | "reset"

/** Shown on two lines with a shell continuation; copied as one. */
const RESET_COMMAND = ["docker compose exec hmnpay", "hmnpay reset-password"] as const
const RECOVERY_COMMAND = "hmnpay recovery-code"

/**
 * Operator sign-in. Single-tenant, so there is no sign-up and no email-based recovery: a lost
 * password is reset on the server (`?step=reset` says how).
 *
 * The session is a cookie the server sets (HttpOnly; this page never sees a token). After
 * signing in, the operator goes to `?next=` if it is a path on this site, otherwise the overview.
 *
 * The recovery code: logins are throttled, and the throttles that stop password guessing are
 * shared, so somebody hammering the login can make the server answer "too many attempts" to
 * the real operator on a browser it has not seen. A one-time code printed on the server
 * (`hmnpay recovery-code`) lets one login past that. The field appears when the server says
 * "too many attempts", or on demand.
 */
export function Login() {
  const params = useSearchParams()
  const hydrated = useHydrated()
  const view: LoginView = params.get("step") === "reset" ? "reset" : "signin"
  const next = safeNextPath(params.get("next"))

  const [email, setEmail] = React.useState("")
  const [password, setPassword] = React.useState("")
  const [recovery, setRecovery] = React.useState("")
  const [showRecovery, setShowRecovery] = React.useState(false)
  const [missing, setMissing] = React.useState(false)
  const [pending, setPending] = React.useState(false)
  /** What the server said about the last attempt. */
  const [failure, setFailure] = React.useState<{ kind: "credentials" | "throttled" | "recovery" | "other"; text: string } | null>(null)
  const emailRef = useAutoFocus<HTMLInputElement>(view === "signin")
  const passwordRef = React.useRef<HTMLInputElement>(null)
  const recoveryRef = React.useRef<HTMLInputElement>(null)

  // Already signed in (or not set up yet): this page has nothing to ask.
  React.useEffect(() => {
    let cancelled = false
    api
      .me()
      .then(() => {
        if (!cancelled) window.location.replace(next)
      })
      .catch(async (error) => {
        if (cancelled || !isApiError(error) || error.status !== 401) return
        try {
          const status = await api.setupStatus()
          if (!cancelled && !status.completed) window.location.replace("/setup")
        } catch {
          // The form will say what is wrong when it is submitted.
        }
      })
    return () => {
      cancelled = true
    }
  }, [next])

  const submitSignIn = async (event: React.FormEvent) => {
    event.preventDefault()
    if (pending) return
    if (!email.trim() || !password || (showRecovery && !recovery.trim())) {
      setMissing(true)
      return
    }
    setPending(true)
    setFailure(null)
    try {
      await api.login({ email: email.trim(), password, ...(showRecovery ? { recovery_code: recovery.trim() } : {}) })
      clearQueryCache()
      // A full navigation: the dashboard starts from the new session with nothing left over.
      window.location.assign(next)
    } catch (error) {
      setPending(false)
      if (isApiError(error) && error.code === "recovery_code_invalid") {
        setFailure({ kind: "recovery", text: describeError(error) })
        setRecovery("")
        recoveryRef.current?.focus()
      } else if (isApiError(error) && error.status === 401) {
        // A rejected attempt keeps the email and clears the password.
        setFailure({ kind: "credentials", text: "" })
        setPassword("")
        passwordRef.current?.focus()
      } else if (isApiError(error) && error.status === 429) {
        const wait = error.retryAfter ? (error.retryAfter >= 90 ? `${Math.ceil(error.retryAfter / 60)} minutes` : `${error.retryAfter} seconds`) : "a moment"
        setFailure({ kind: "throttled", text: `Too many sign-in attempts. Try again in ${wait}, or use a recovery code.` })
        setShowRecovery(true)
      } else {
        setFailure({ kind: "other", text: describeError(error) })
      }
    }
  }

  return (
    <SoundProvider surface="dashboard">
      <MotionConfig reducedMotion="user">
        <div className="flex min-h-svh flex-col items-center px-3 pt-3 pb-6 sm:px-6 sm:pt-[clamp(2.5rem,calc((100svh-36rem)/2),14rem)]">
          <div className="w-full max-w-[400px]">
            <main className="overflow-hidden rounded-2xl border border-border bg-card shadow-raised">
              <AnimatePresence mode="wait" initial={false}>
                <motion.div
                  key={view}
                  className="p-6 sm:p-8"
                  initial={{ opacity: 0, y: 6 }}
                  animate={{ opacity: 1, y: 0 }}
                  exit={{ opacity: 0, y: -4 }}
                  transition={{ duration: 0.22, ease: [0.22, 1, 0.36, 1] }}
                >
                  {view === "signin" && (
                    <form noValidate onSubmit={submitSignIn}>
                      <IconTile>
                        <LogoMark className="size-5 text-foreground" />
                      </IconTile>
                      <Heading title="Sign in">Sign in to your payment dashboard.</Heading>

                      {failure && (
                        <Callout tone={failure.kind === "throttled" ? "warning" : "danger"} icon={<CircleAlertIcon />} className="mt-6" role="alert">
                          {failure.kind === "credentials" ? (
                            <>
                              <span className="text-foreground">That email and password don’t match.</span> Check both and try again.
                            </>
                          ) : (
                            <span className="text-foreground">{failure.text}</span>
                          )}
                        </Callout>
                      )}

                      <div className="mt-6 flex flex-col gap-4">
                        <FlowField label="Email" htmlFor="login-email" error={missing && !email.trim() ? "Enter your email." : undefined}>
                          <FlowInput
                            ref={emailRef}
                            id="login-email"
                            type="email"
                            name="email"
                            autoComplete="username"
                            inputMode="email"
                            spellCheck={false}
                            placeholder="you@company.com"
                            value={email}
                            onChange={(e) => setEmail(e.target.value)}
                            aria-invalid={missing && !email.trim() ? true : undefined}
                          />
                        </FlowField>
                        <FlowField
                          label="Password"
                          htmlFor="login-password"
                          error={missing && !password ? "Enter your password." : undefined}
                          action={
                            <Link
                              href="/login?step=reset"
                              className="focus-ring rounded-sm text-muted-foreground transition-colors hover:text-foreground"
                            >
                              Forgot password?
                            </Link>
                          }
                        >
                          <PasswordInput
                            ref={passwordRef}
                            id="login-password"
                            name="password"
                            autoComplete="current-password"
                            value={password}
                            onChange={(e) => setPassword(e.target.value)}
                            aria-invalid={(missing && !password) || failure?.kind === "credentials" ? true : undefined}
                          />
                        </FlowField>
                        {showRecovery && (
                          <FlowField
                            label="Recovery code"
                            htmlFor="login-recovery"
                            error={
                              missing && !recovery.trim()
                                ? "Enter the recovery code, or hide this field."
                                : failure?.kind === "recovery"
                                  ? "Not accepted. Each code works once, for 15 minutes."
                                  : undefined
                            }
                            hint={
                              <>
                                Printed by <span className="num text-foreground">{RECOVERY_COMMAND}</span> on the server. It gets one sign-in
                                past the attempt limit; your password is still checked.
                              </>
                            }
                            action={
                              <button
                                type="button"
                                onClick={() => {
                                  setShowRecovery(false)
                                  setRecovery("")
                                }}
                                className="focus-ring rounded-sm text-muted-foreground transition-colors hover:text-foreground"
                              >
                                Hide
                              </button>
                            }
                          >
                            <FlowInput
                              ref={recoveryRef}
                              id="login-recovery"
                              name="recovery_code"
                              autoComplete="off"
                              autoCapitalize="none"
                              spellCheck={false}
                              placeholder="hmnr_…"
                              value={recovery}
                              onChange={(e) => setRecovery(e.target.value)}
                              aria-invalid={failure?.kind === "recovery" ? true : undefined}
                              className="num"
                            />
                          </FlowField>
                        )}
                      </div>

                      <SubmitButton pending={pending}>Sign in</SubmitButton>
                      {!showRecovery && (
                        <p className="mt-4 text-center text-[13px] leading-5">
                          <button
                            type="button"
                            onClick={() => setShowRecovery(true)}
                            className="focus-ring rounded-sm text-muted-foreground underline decoration-border-strong underline-offset-4 transition-colors hover:text-foreground hover:decoration-current"
                          >
                            Have a recovery code?
                          </button>
                        </p>
                      )}
                    </form>
                  )}

                  {view === "reset" && (
                    <div>
                      <BackLink />
                      <IconTile>
                        <TerminalIcon />
                      </IconTile>
                      <Heading title="Reset from the server">
                        This instance doesn’t send email, so access is reset by whoever runs it. On the server, run:
                      </Heading>

                      <div className="mt-5 flex items-start gap-1 rounded-lg border border-border-strong bg-background/60 py-1.5 pr-1.5 pl-3.5">
                        <code className="num min-w-0 flex-1 overflow-x-auto py-1.5 text-[13px] leading-5 whitespace-pre">
                          <span aria-hidden="true" className="text-faint select-none">
                            {"$ "}
                          </span>
                          {RESET_COMMAND[0]}
                          <span aria-hidden="true" className="text-faint select-none">
                            {" \\\n  "}
                          </span>
                          <span className="sr-only"> </span>
                          {RESET_COMMAND[1]}
                        </code>
                        <CopyButton value={RESET_COMMAND.join(" ")} label="Copy command" className="size-8 pointer-coarse:size-10" />
                      </div>

                      <p className="mt-4 text-sm leading-6 text-pretty text-muted-foreground">
                        It asks for a new password, signs out every session, and prints a one-time recovery code (valid 15 minutes) to
                        enter here with the new password if the sign-in page says “too many attempts”. Without Docker, run{" "}
                        <span className="num text-foreground">hmnpay reset-password</span> next to the data directory.
                      </p>

                      <Button asChild variant="outline" className="mt-7 h-11 w-full text-sm">
                        <Link href="/login">Back to sign in</Link>
                      </Button>
                    </div>
                  )}
                </motion.div>
              </AnimatePresence>

              <div className="flex items-center justify-between gap-4 border-t border-border bg-background/70 px-6 py-3 text-xs leading-5 text-muted-foreground sm:px-8 dark:bg-background/55">
                <span className="inline-flex shrink-0 items-center gap-2">
                  <ServerIcon aria-hidden="true" className="size-3.5 text-faint" />
                  Self-hosted instance
                </span>
                {/* The host is known only in the browser; the exported HTML leaves the place empty. */}
                <span className="num min-w-0 truncate">{hydrated ? window.location.host : ""}</span>
              </div>
            </main>
            <FlowFooter />
          </div>
        </div>
      </MotionConfig>
    </SoundProvider>
  )
}

/* --------------------------------- pieces --------------------------------- */

function Heading({ title, children }: { title: React.ReactNode; children: React.ReactNode }) {
  return (
    <header className="mt-5">
      <h1 className="text-xl leading-7 font-semibold tracking-[-0.02em] text-balance">{title}</h1>
      <p className="mt-1 text-sm leading-6 text-pretty text-muted-foreground">{children}</p>
    </header>
  )
}

function IconTile({ children }: { children: React.ReactNode }) {
  return (
    <span
      aria-hidden="true"
      className="flex size-10 items-center justify-center rounded-lg border border-border bg-background/60 text-muted-foreground [&_svg]:size-[18px]"
    >
      {children}
    </span>
  )
}

function BackLink() {
  return (
    <Link
      href="/login"
      className="focus-ring mb-6 -ml-1 inline-flex h-7 items-center gap-1.5 rounded-md px-1 text-[13px] leading-5 text-muted-foreground transition-colors hover:text-foreground pointer-coarse:h-10"
    >
      <ArrowLeftIcon aria-hidden="true" className="size-3.5" />
      Sign in
    </Link>
  )
}

function SubmitButton({
  pending,
  disabled,
  className,
  children,
}: {
  pending: boolean
  disabled?: boolean
  className?: string
  children: React.ReactNode
}) {
  return (
    <Button type="submit" disabled={pending || disabled} className={cn("mt-6 h-11 w-full text-sm", className)}>
      {pending && <Spinner />}
      {children}
    </Button>
  )
}
