import type { Metadata } from "next"

import { WalletSettings } from "@/components/settings/wallet"

export const metadata: Metadata = { title: "Wallet · Settings" }

/** Settings → Wallet. `?replace=1` opens the replace-key dialog. */
export default function Page() {
  return <WalletSettings />
}
