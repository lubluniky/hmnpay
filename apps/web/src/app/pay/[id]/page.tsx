// SPDX-License-Identifier: GPL-3.0-or-later
// hmnpay — Copyright (C) 2026 lubluniky and the hmnpay contributors. Distributed under the GNU GPL,
// version 3 or later (LICENSE), with additional terms under its section 7 (ADDITIONAL_TERMS.md):
// the "Powered by hmnpay" author attribution on payment pages must be preserved.
import type { Metadata } from "next"

import { CheckoutPage } from "@/components/checkout/checkout"
import { PLACEHOLDER_ID_PARAMS } from "@/lib/route-params"

export const metadata: Metadata = {
  title: { absolute: "Pay" },
  robots: { index: false },
}

// A static export: built once for the placeholder id; the Go server answers every /pay/{id}
// with this page, and the page reads the real id from the address bar.
export function generateStaticParams() {
  return PLACEHOLDER_ID_PARAMS
}

/** Hosted checkout — what a merchant's customer sees. No dashboard chrome. */
export default function Page() {
  return <CheckoutPage />
}
