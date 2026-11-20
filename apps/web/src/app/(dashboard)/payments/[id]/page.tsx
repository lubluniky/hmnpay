import type { Metadata } from "next"

import { PaymentDetailView } from "@/components/payments/payment-detail-view"
import { PLACEHOLDER_ID_PARAMS } from "@/lib/route-params"

export const metadata: Metadata = { title: "Payment" }

// A static export: the page is built once, for the placeholder id, and the Go server answers
// every /payments/{id} with it. The view reads the real id from the address bar.
export function generateStaticParams() {
  return PLACEHOLDER_ID_PARAMS
}

export default function Page() {
  return <PaymentDetailView />
}
