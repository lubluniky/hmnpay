"use client"

import { flushSync } from "react-dom"
import { MoonIcon, SunIcon } from "lucide-react"
import { useTheme } from "next-themes"

import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

import { prefersReducedMotion } from "./motion"

/**
 * Dark / light switch. The icon is swapped with CSS (`dark:`), so it renders identically
 * on the server and never flashes on hydration. Where the browser can, the two themes
 * cross-fade (a view transition of the whole page) instead of cutting.
 */
export function ThemeToggle({ className, size = "icon", ...props }: Omit<React.ComponentProps<typeof Button>, "onClick">) {
  const { resolvedTheme, setTheme } = useTheme()

  return (
    <Button
      type="button"
      variant="ghost"
      size={size}
      aria-label="Switch theme"
      className={cn("text-muted-foreground hover:text-foreground", className)}
      onClick={() => {
        const next = resolvedTheme === "dark" ? "light" : "dark"
        if (typeof document.startViewTransition !== "function" || prefersReducedMotion()) return setTheme(next)
        document.documentElement.dataset.themeSwitch = ""
        const transition = document.startViewTransition(() => flushSync(() => setTheme(next)))
        const done = () => delete document.documentElement.dataset.themeSwitch
        transition.finished.then(done, done)
      }}
      {...props}
    >
      <SunIcon className="hidden dark:block" />
      <MoonIcon className="block dark:hidden" />
    </Button>
  )
}
