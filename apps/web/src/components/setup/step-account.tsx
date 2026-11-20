"use client"

import { KeyRoundIcon, UserRoundIcon } from "lucide-react"

import { FlowField, FlowInput, PasswordInput, useAutoFocus } from "@/components/auth/flow"
import { NETWORK_NAME, StatusBadge, toneClasses } from "@/components/hmn"
import { cn } from "@/lib/utils"

import { isEmail, passwordStrength, type SetupForm } from "./data"
import { StepHeader } from "./parts"

export function StepAccount({
  form,
  set,
  showErrors,
  host,
  tokenRequired,
  minPassword,
  errors,
}: {
  form: SetupForm
  set: (patch: Partial<SetupForm>) => void
  showErrors: boolean
  /** Where this page was opened ("pay.example.com"); empty until the browser knows. */
  host: string
  /** The server asks for its setup token (GET /api/v1/setup). */
  tokenRequired: boolean
  minPassword: number
  /** What the server said about fields of this step. */
  errors: Record<string, string>
}) {
  const tokenRef = useAutoFocus<HTMLInputElement>(tokenRequired && !form.setupToken)
  const emailRef = useAutoFocus<HTMLInputElement>(!tokenRequired && !form.email)
  const strength = passwordStrength(form.password, minPassword)
  const tokenError = errors.setup_token ?? (showErrors && tokenRequired && !form.setupToken.trim() ? "Enter the setup token from the server log." : undefined)
  const emailError = errors.email ?? (showErrors && !isEmail(form.email) ? "Enter the email address you’ll sign in with." : undefined)
  const passwordError =
    errors.password ??
    (showErrors && strength.score < 2
      ? form.password
        ? `${strength.label}. ${strength.hint}`
        : "Choose a password."
      : undefined)

  return (
    <>
      {/* The first thing after `docker compose up`: proof that it worked. */}
      <p className="mb-5 flex flex-wrap items-center gap-x-2.5 gap-y-1.5 text-[13px] leading-5 text-muted-foreground">
        <StatusBadge tone="success" live>
          Running
        </StatusBadge>
        {host && (
          <span>
            at <span className="num text-foreground">{host}</span>
          </span>
        )}
      </p>

      <StepHeader title="Welcome to hmnpay">
        A payment gateway you run yourself. Customers pay USDT on {NETWORK_NAME} straight into your wallet. First,
        the account that runs this instance.
      </StepHeader>

      <div className="mt-8 flex max-w-[420px] flex-col gap-5">
        {tokenRequired && (
          <FlowField
            label="Setup token"
            htmlFor="setup-token"
            error={tokenError}
            hint={
              <>
                Printed in the server log when hmnpay started (<span className="num text-foreground">docker compose logs hmnpay</span>), or
                the one you set in <span className="num text-foreground">HMNPAY_SETUP_TOKEN</span>. It proves this is your server to set up.
              </>
            }
          >
            <div className="relative">
              <KeyRoundIcon aria-hidden="true" className="pointer-events-none absolute top-1/2 left-3.5 size-4 -translate-y-1/2 text-faint" />
              <FlowInput
                ref={tokenRef}
                id="setup-token"
                name="setup-token"
                autoComplete="off"
                autoCapitalize="none"
                autoCorrect="off"
                spellCheck={false}
                placeholder="From the server log"
                value={form.setupToken}
                onChange={(e) => set({ setupToken: e.target.value.trim() })}
                aria-invalid={tokenError ? true : undefined}
                className="pl-10 font-mono text-[15px] tracking-[-0.01em] md:text-[13px]"
              />
            </div>
          </FlowField>
        )}
        <FlowField label="Email" htmlFor="setup-email" error={emailError}>
          <FlowInput
            ref={emailRef}
            id="setup-email"
            type="email"
            name="email"
            autoComplete="username"
            inputMode="email"
            spellCheck={false}
            placeholder="you@company.com"
            value={form.email}
            onChange={(e) => set({ email: e.target.value })}
            aria-invalid={emailError ? true : undefined}
            aria-describedby={emailError ? "setup-email-message" : undefined}
          />
        </FlowField>

        <FlowField label="Password" htmlFor="setup-password" error={passwordError}>
          <PasswordInput
            id="setup-password"
            name="password"
            autoComplete="new-password"
            placeholder="A long passphrase"
            value={form.password}
            onChange={(e) => set({ password: e.target.value })}
            aria-invalid={passwordError ? true : undefined}
            aria-describedby="setup-password-strength"
          />
          <div id="setup-password-strength" className="flex flex-col gap-2">
            <div className="flex gap-1" aria-hidden="true">
              {[1, 2, 3, 4].map((step) => (
                <span
                  key={step}
                  className={cn(
                    "h-1 flex-1 rounded-full transition-colors duration-300",
                    step <= strength.score ? toneClasses[strength.tone].solid : "bg-muted",
                  )}
                />
              ))}
            </div>
            {!passwordError && (
              <p className="flex items-baseline justify-between gap-3 text-[13px] leading-5 text-muted-foreground">
                <span className="text-pretty">{strength.hint}</span>
                {strength.label && (
                  <span className={cn("shrink-0 font-medium", toneClasses[strength.tone].text)}>{strength.label}</span>
                )}
              </p>
            )}
          </div>
        </FlowField>
      </div>

      <p className="mt-8 flex max-w-[420px] gap-2.5 text-[13px] leading-5 text-pretty text-muted-foreground">
        <UserRoundIcon aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-faint" />
        <span>
          This is the only account. There is no public sign-up, and a lost password is reset from the server’s command
          line, not by email.
        </span>
      </p>
    </>
  )
}
