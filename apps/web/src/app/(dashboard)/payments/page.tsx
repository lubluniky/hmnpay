import { Suspense } from "react"
import type { Metadata } from "next"

import { PaymentsView } from "@/components/payments/payments-view"

export const metadata: Metadata = { title: "Payments" }

export default function Page() {
  return (
    <Suspense>
      <PaymentsView />
    </Suspense>
  )
}
