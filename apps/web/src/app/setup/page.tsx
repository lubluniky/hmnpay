import { Suspense } from "react"
import type { Metadata } from "next"

import { SetupWizard } from "@/components/setup/wizard"

export const metadata: Metadata = {
  title: "Set up",
  robots: { index: false },
}

/** First-run setup. Steps: `?step=account|network|wallet|business`; `done` once it is. */
export default function Page() {
  return (
    <Suspense>
      <SetupWizard />
    </Suspense>
  )
}
