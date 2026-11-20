// SPDX-License-Identifier: GPL-3.0-or-later
// hmnpay — Copyright (C) 2026 lubluniky and the hmnpay contributors. Distributed under the GNU GPL,
// version 3 or later (LICENSE), with additional terms under its section 7 (ADDITIONAL_TERMS.md):
// the "Powered by hmnpay" author attribution on payment pages must be preserved.
import { cn } from "@/lib/utils"

import { Logo } from "./logo"

/** The project's home page. The one place this address is written down. */
export const PROJECT_URL = "https://github.com/lubluniky/hmnpay"

/** The author attribution every payment page keeps showing (additional term under GPL section 7(b)). */
export const ATTRIBUTION = "Powered by hmnpay"

/**
 * "Powered by hmnpay": the author attribution that every page a payer sees must keep
 * (ADDITIONAL_TERMS.md), shown once on the operator's pages too.
 *
 * It is not configurable. There is no prop, setting, environment variable or API field that
 * hides it or changes its words, and pages render it unconditionally — in every state,
 * including loading, not-found and errors.
 *
 * The visible text is "Powered by" followed by the logo, which spells "hmnpay"; the link's
 * accessible name is the exact phrase. It opens the project page in a new tab.
 */
export function PoweredBy({ className, ...props }: Omit<React.ComponentProps<"a">, "href" | "target" | "rel" | "children">) {
  return (
    <a
      data-slot="powered-by"
      href={PROJECT_URL}
      target="_blank"
      rel="noopener noreferrer"
      aria-label={ATTRIBUTION}
      className={cn(
        "focus-ring inline-flex items-center gap-2 rounded-md py-2 text-xs text-faint transition-colors hover:text-muted-foreground pointer-coarse:py-3.5",
        className,
      )}
      {...props}
    >
      <span>Powered by</span>
      <Logo size="sm" className="text-[13px] opacity-80" />
    </a>
  )
}
