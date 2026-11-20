import type { Metadata } from "next"

import { GeneralSettings } from "@/components/settings/general"

export const metadata: Metadata = { title: "General · Settings" }

/** Settings → General. */
export default function Page() {
  return <GeneralSettings />
}
