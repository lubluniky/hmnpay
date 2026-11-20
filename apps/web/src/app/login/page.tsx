import { Suspense } from "react"
import type { Metadata } from "next"

import { Login } from "@/components/auth/login"

export const metadata: Metadata = {
  title: "Sign in",
  robots: { index: false },
}

/** Operator sign-in. `?step=reset` explains how to reset access from the server; `?next=` is where to go afterwards. */
export default function Page() {
  return (
    <Suspense>
      <Login />
    </Suspense>
  )
}
