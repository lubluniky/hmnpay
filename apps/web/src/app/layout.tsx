import type { Metadata } from "next"
import { Providers } from "@/components/providers"
import { fontVariables } from "@/fonts"

import "./globals.css"

export const metadata: Metadata = {
  title: {
    default: "hmnpay",
    template: "%s · hmnpay",
  },
  description: "Self-hosted crypto payment gateway",
}

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    // suppressHydrationWarning: next-themes sets the class on <html> before hydration.
    <html
      lang="en"
      suppressHydrationWarning
      className={`${fontVariables} h-full antialiased`}
    >
      <body className="flex min-h-full flex-col">
        <Providers>{children}</Providers>
      </body>
    </html>
  )
}
