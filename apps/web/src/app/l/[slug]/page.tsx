// SPDX-License-Identifier: GPL-3.0-or-later
// hmnpay — Copyright (C) 2026 lubluniky and the hmnpay contributors. Distributed under the GNU GPL,
// version 3 or later (LICENSE), with additional terms under its section 7 (ADDITIONAL_TERMS.md):
// the "Powered by hmnpay" author attribution on payment pages must be preserved.
import type { Metadata } from "next"

import { LinkPage } from "@/components/checkout/link-page"
import { PLACEHOLDER_SLUG_PARAMS } from "@/lib/route-params"

export const metadata: Metadata = {
  title: { absolute: "Pay" },
  robots: { index: false },
}

// A static export: built once for the placeholder slug; the Go server answers every /l/{slug}
// with this page, and the page reads the real slug from the address bar.
export function generateStaticParams() {
  return PLACEHOLDER_SLUG_PARAMS
}

/** Public page of a payment link — where a customer starts a payment from a shared link. */
export default function Page() {
  return <LinkPage />
}
