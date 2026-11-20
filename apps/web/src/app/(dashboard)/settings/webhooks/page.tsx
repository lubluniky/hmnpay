import type { Metadata } from "next"

import { WebhooksSettings } from "@/components/settings/webhooks"

export const metadata: Metadata = { title: "Webhooks · Settings" }

/** Settings → Webhooks. `?new=1` add-endpoint dialog · `?endpoint=<id>` endpoint sheet · `?delivery=<id>` delivery sheet. */
export default function Page() {
  return <WebhooksSettings />
}
