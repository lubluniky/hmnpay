import type { Metadata } from "next"

import { LinksView } from "@/components/payment-links/links-view"

export const metadata: Metadata = { title: "Payment links" }

export default function Page() {
  return <LinksView />
}
