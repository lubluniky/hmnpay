import type { Metadata } from "next"

import { ApiKeysSettings } from "@/components/settings/api-keys"

export const metadata: Metadata = { title: "API keys · Settings" }

/** Settings → API keys. `?new=1` opens the create dialog. */
export default function Page() {
  return <ApiKeysSettings />
}
