import type { Metadata } from "next"

import { NetworkSettings } from "@/components/settings/network"

export const metadata: Metadata = { title: "Network · Settings" }

/** Settings → Network. `?add=1` opens the add-endpoint dialog. */
export default function Page() {
  return <NetworkSettings />
}
