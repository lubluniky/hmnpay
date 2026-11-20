"use client"

import { ArrowRightIcon, ChevronRightIcon, KeyRoundIcon, ReceiptTextIcon, WebhookIcon } from "lucide-react"

import { DrawnCheck, StateMark } from "@/components/checkout/parts"
import { NETWORK_NAME } from "@/components/hmn"
import { Button } from "@/components/ui/button"

import { StepHeader } from "./parts"

const NEXT_ACTIONS = [
  {
    href: "/payments?new=1",
    icon: ReceiptTextIcon,
    title: "Create a test payment",
    text: "Pay it from your own wallet and watch it confirm, end to end.",
  },
  {
    href: "/settings/api-keys",
    icon: KeyRoundIcon,
    title: "Create an API key",
    text: "So your backend can create payments.",
  },
  {
    href: "/settings/webhooks",
    icon: WebhookIcon,
    title: "Add a webhook",
    text: "Get a signed request the moment a payment is confirmed.",
  },
] as const

export function StepDone({ merchantName, asset }: { merchantName: string; asset: string }) {
  const name = merchantName.trim() || "Your instance"
  return (
    <>
      <StateMark tone="success" solid className="mt-1.5 ml-1.5">
        <DrawnCheck />
      </StateMark>

      <StepHeader title="You’re ready to take payments" className="mt-6">
        {name} now accepts {asset} on {NETWORK_NAME}, straight into your own wallet. Three things worth doing next:
      </StepHeader>

      <ol className="mt-6 divide-y divide-border overflow-hidden rounded-xl border border-border-strong bg-background/60">
        {NEXT_ACTIONS.map((action, i) => (
          <li key={action.href}>
            <a
              href={action.href}
              className="group flex min-h-16 items-center gap-3.5 px-4 py-3 transition-colors outline-none hover:bg-muted/50 focus-visible:bg-muted/50 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring"
            >
              <span className="flex size-9 shrink-0 items-center justify-center rounded-lg border border-border bg-card text-muted-foreground">
                <action.icon aria-hidden="true" className="size-4" />
              </span>
              <span className="min-w-0 flex-1">
                <span className="block text-sm leading-5 font-medium">
                  <span className="sr-only">{i + 1}. </span>
                  {action.title}
                </span>
                <span className="block text-[13px] leading-5 text-pretty text-muted-foreground">{action.text}</span>
              </span>
              <ChevronRightIcon
                aria-hidden="true"
                className="size-4 shrink-0 text-faint transition-transform group-hover:translate-x-0.5 group-hover:text-foreground"
              />
            </a>
          </li>
        ))}
      </ol>

      <div className="mt-auto pt-8">
        <Button asChild className="h-11 w-full text-sm">
          {/* A full navigation on purpose: the dashboard starts from the session setup has just opened. */}
          {/* eslint-disable-next-line @next/next/no-html-link-for-pages */}
          <a href="/">
            Go to dashboard
            <ArrowRightIcon data-icon="inline-end" />
          </a>
        </Button>
        <p className="mt-3 text-center text-xs leading-4 text-faint">Everything you entered can be changed in Settings.</p>
      </div>
    </>
  )
}
