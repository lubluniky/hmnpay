"use client"

import * as React from "react"

import { cn } from "@/lib/utils"

/**
 * Shared motion for hmnpay. The rules, which every use site inherits by using these:
 *  - Motion explains a change or answers an action. Nothing here moves on an idle screen.
 *  - One easing family (quint) and three durations, defined once in globals.css
 *    (`--ease-out-quint`, `--ease-in-quint`, `--ease-in-out-quint`, `--ease-settle`;
 *    `--duration-fast|base|slow`) and mirrored below for JS-driven motion.
 *  - Transform and opacity only. Nothing changes size, so nothing shifts.
 *  - `prefers-reduced-motion`: the new state appears at once (CSS is gated on `motion-safe:`,
 *    JS on `prefersReducedMotion()`).
 *  - Content rendered on the server is the finished content: entrances are CSS, so they need
 *    no JavaScript to end and never flash.
 */

/* --------------------------------- tokens --------------------------------- */

/** cubic-bezier control points, for `motion` and the Web Animations API. Keep in sync with globals.css. */
export const EASE = {
  /** Arriving and changing. The default. */
  out: [0.22, 1, 0.36, 1],
  /** Leaving. */
  in: [0.64, 0, 0.78, 0],
  /** Moving between two resting places. */
  inOut: [0.83, 0, 0.17, 1],
} as const

/** Seconds. */
export const DURATION = { fast: 0.12, base: 0.2, slow: 0.32 } as const

const cubic = (points: readonly number[]) => `cubic-bezier(${points.join(", ")})`

const REDUCED = "(prefers-reduced-motion: reduce)"
export const prefersReducedMotion = () => typeof window !== "undefined" && window.matchMedia(REDUCED).matches

function subscribeReduced(onChange: () => void) {
  const query = window.matchMedia(REDUCED)
  query.addEventListener("change", onChange)
  return () => query.removeEventListener("change", onChange)
}

/** Follows the OS setting. False on the server and while hydrating. */
export function usePrefersReducedMotion() {
  return React.useSyncExternalStore(subscribeReduced, prefersReducedMotion, () => false)
}

/**
 * False until `value` first differs from what it was when the component mounted, true ever
 * after. For "animate changes, not the first paint": a state that is there on load is at rest.
 */
export function useChanged<T>(value: T) {
  const [first] = React.useState(value)
  const [changed, setChanged] = React.useState(false)
  if (!changed && !Object.is(first, value)) setChanged(true)
  return changed
}

/* ---------------------------------- Roll ---------------------------------- */

const isDigit = (char: string) => char >= "0" && char <= "9"
const toNumber = (text: string) => Number(text.replace(/[^\d.-]/g, "")) || 0

function RollCell({ char, direction, enter, delay }: { char: string; direction: 1 | -1; enter: boolean; delay: number }) {
  const [shown, setShown] = React.useState(char)
  const [leaving, setLeaving] = React.useState<{ char: string; id: number } | null>(null)
  // Decided at mount: a cell that is there on load is at rest; one added later rolls in.
  const [rolls, setRolls] = React.useState(enter)
  if (char !== shown) {
    setLeaving({ char: shown, id: (leaving?.id ?? 0) + 1 })
    setShown(char)
    setRolls(true)
  }
  const style = { "--roll-from": direction, animationDelay: `${delay}ms` } as React.CSSProperties

  return (
    // clip-path, not overflow: an inline-block that hides overflow would drop off the baseline.
    <span className="relative inline-block [clip-path:inset(-0.08em_-0.02em)]">
      <span key={`in-${shown}`} className={cn("inline-block", rolls && "motion-safe:animate-roll-in")} style={style}>
        {shown}
      </span>
      {leaving && (
        <span
          key={`out-${leaving.id}`}
          className="absolute inset-0 motion-safe:animate-roll-out motion-reduce:hidden"
          style={style}
          onAnimationEnd={() => setLeaving(null)}
        >
          {leaving.char}
        </span>
      )}
    </span>
  )
}

/**
 * A number whose digits roll to their new value, like a counter. Set it in `num` (tabular
 * digits): a rolling digit never changes width, so nothing around it moves.
 *
 * - When `value` changes, only the digits that differ roll: up when the number grew, down when
 *   it shrank. Digits are matched from the right, so "999" → "1,000" rolls three and adds two.
 * - `intro` also rolls the digits in once on load, left to right (CSS only; the server renders
 *   the final number). For the headline figures of a page, not for tables.
 * - `value` is the formatted text ("100,673.46"). "," and "." are pulled in the way `Amount`
 *   does; `dimFraction` quiets what follows the ".".
 * Screen readers get the plain text, once, and no announcements while digits move.
 */
export function Roll({
  value,
  intro = false,
  dimFraction = false,
  className,
  ...props
}: Omit<React.ComponentProps<"span">, "children"> & { value: string; intro?: boolean; dimFraction?: boolean }) {
  const [state, setState] = React.useState({ value, direction: 1 as 1 | -1, changed: false })
  if (state.value !== value) {
    setState({ value, direction: toNumber(value) >= toNumber(state.value) ? 1 : -1, changed: true })
  }
  const chars = [...value]
  const point = chars.indexOf(".")

  return (
    <span data-slot="roll" className={cn("whitespace-nowrap", className)} {...props}>
      <span className="sr-only select-none">{value}</span>
      <span aria-hidden="true">
        {chars.map((char, i) => {
          // Keyed from the right: the units digit stays the units digit when the number gets longer.
          const key = chars.length - i
          const dim = dimFraction && point !== -1 && i >= point
          if (!isDigit(char)) {
            return (
              <span key={`${key}${char}`} className={cn((char === "," || char === ".") && "-mx-[0.11em]", dim && "text-muted-foreground")}>
                {char}
              </span>
            )
          }
          return (
            <span key={key} className={dim ? "text-muted-foreground" : undefined}>
              <RollCell char={char} direction={state.direction} enter={intro || state.changed} delay={Math.min(i, 12) * 18} />
            </span>
          )
        })}
      </span>
    </span>
  )
}

/* ---------------------------------- Swap ---------------------------------- */

/**
 * Wraps something that is replaced when `id` changes (a status badge, a label). The
 * replacement eases in; what is there on load does not animate.
 */
export function Swap({ id, className, children, ...props }: React.ComponentProps<"span"> & { id: string | number }) {
  const changed = useChanged(id)
  return (
    <span key={id} className={cn("inline-flex", changed && "motion-safe:animate-swap", className)} {...props}>
      {children}
    </span>
  )
}

/* -------------------------------- Arrivals -------------------------------- */

/**
 * For a list that gets new rows at the top while it is on screen (a live payments table).
 * When `ids` gains leading entries, the new rows ease in and the rows below glide down to make
 * room, by transform only. Returns a ref for the container; rows are its `[data-row]`s.
 * A different list (another page, another sort) does not animate.
 */
export function useArrivals<T extends HTMLElement>(ids: readonly string[]) {
  const ref = React.useRef<T>(null)
  const previous = React.useRef<readonly string[] | null>(null)
  const signature = ids.join("\n")

  React.useLayoutEffect(() => {
    const before = previous.current
    const now = signature ? signature.split("\n") : []
    previous.current = now
    const container = ref.current
    if (!before || !container || before.length === 0 || prefersReducedMotion()) return
    const known = new Set(before)
    const arrived = now.findIndex((id) => known.has(id))
    // Nothing new on top, or what follows is not the list that was there.
    if (arrived <= 0 || now[arrived] !== before[0]) return

    const rows = [...container.querySelectorAll<HTMLElement>("[data-row]")]
    const shift = rows.slice(0, arrived).reduce((sum, row) => sum + row.getBoundingClientRect().height, 0)
    if (!shift) return
    const timing = { duration: DURATION.slow * 1000, easing: cubic(EASE.out) }
    rows.forEach((row, i) => {
      if (i < arrived) {
        row.animate(
          [
            { opacity: 0, transform: "translateY(-10px)" },
            { opacity: 1, transform: "none" },
          ],
          { ...timing, delay: 60 + i * 40, fill: "backwards" },
        )
      } else {
        row.animate([{ transform: `translateY(${-shift}px)` }, { transform: "none" }], timing)
      }
    })
  }, [signature])

  return ref
}
