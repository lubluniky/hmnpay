"use client"

import * as React from "react"

import { cn } from "@/lib/utils"
import type { WaveController, WaveOptions, WaveStyleInput } from "@/vendor/distortion-waves/controller"
import type { Palette } from "@/vendor/distortion-waves/model"

import { useSound, type CueName } from "./sound"

/**
 * The hmnpay wave: one view becomes another, carried by a wave of colour that leaves the
 * element that caused the change. It is the vendored "Distortion waves" engine
 * (src/vendor/distortion-waves) with the house defaults fixed here, so every use looks alike.
 *
 * When to use it — an earned, forward moment where something payable now exists or money has
 * arrived: payment confirmed, payment created, link created, setup finished. Never on failure,
 * destructive or neutral changes, never on navigation, and at most one per user action.
 *
 * What it guarantees:
 * - Resting states are untouched: nothing is drawn, loaded or observed until a transition starts.
 * - The engine (26 kB gzipped) is a separate chunk, fetched when `armed` and the browser is idle.
 * - Going back (`revealed` true → false) never waves; it is an instant swap.
 * - Reduced motion, no WebGL 2 or an engine that isn't there in time: a 200 ms crossfade.
 * - The first view is inert from the moment the wave starts and unmounted once the second is
 *   live; focus moves to `[data-autofocus]` in the second view (or its dialog).
 * - Nothing shifts before the wave: the element keeps the first view's size while the wave is
 *   out, and eases to the second view's height once the front has passed.
 * - `cue` gives the moment its sound, timed to the wave: the cue's main note lands as the wave
 *   launches. A crossfade gets the cue's short form; a wave that had to wait more than ten
 *   minutes for the tab to be looked at again runs silent.
 *
 * Mark where the wave starts with `data-wave-source`: the button that was pressed (in the first
 * view, `type="vertical"`) or the mark it grows from (in the second view, `type="center"`).
 */

/* ------------------------------ house defaults ---------------------------- */

type WaveType = "vertical" | "center"

/**
 * Ultramarine first, then its neighbours on the wheel, with one warm note so it still reads as
 * colour rather than as a blue glow. Shared by every wave in the product.
 */
const PALETTE: Palette = {
  colors: [
    [66, 99, 255],
    [112, 190, 255],
    [140, 96, 255],
    [232, 104, 196],
    [92, 124, 255],
    [84, 222, 190],
    [255, 184, 108],
  ],
  ahead: [170, 190, 255],
  behind: [48, 72, 230],
}

/**
 * Quicker than the library's own (which takes about 1.4 s to hand over and 4 s to clear).
 * After a press the second view is live about 0.8 s later and the edges are clear by 1.5 s;
 * the ring for an event takes a breath longer.
 */
const STYLE: Record<WaveType, WaveStyleInput> = {
  vertical: {
    anticipation: 0.16,
    travel: 1.25,
    linger: -0.2,
    rimFade: 1.35,
    hairlineLinger: 0.2,
    hairlineFade: 1.2,
    particleLife: 1.2,
  },
  center: {
    anticipation: 0.2,
    travel: 1.6,
    linger: -0.2,
    rimFade: 1.5,
    hairlineLinger: 0.3,
    hairlineFade: 1.3,
    particleLife: 1.5,
  },
}

const EASE_OUT_QUINT = "cubic-bezier(0.22, 1, 0.36, 1)"
/** How long a transition waits for the engine chunk before it crossfades instead. */
const LOAD_BUDGET = 700
/** How long it waits for the pictures of the two views before starting anyway. */
const PICTURE_BUDGET = 450
/** If the second view is not live by then, show it. */
const SAFETY = 4000
/** A transition held back by a hidden tab for longer than this no longer sounds: it is old news. */
const CUE_HOLD = 10 * 60_000
/**
 * The share of `travel` after which the engine has all but stopped bending and blurring its
 * pictures (the swell settles between 0.2 and 0.55 in the vendored model.ts; at 0.5 it is 5% of
 * itself). From then on only colour is drawn, and the element can change size without a
 * picture being stretched over the live view.
 */
const CALM = 0.5

/* --------------------------------- engine --------------------------------- */

type Engine = typeof import("@/vendor/distortion-waves/controller")

let engine: Engine | null = null
let loading: Promise<Engine | null> | null = null

/** Fetch the wave engine ahead of a transition. Safe to call often; resolves to null if it can't load. */
export function preloadWave(): Promise<Engine | null> {
  if (typeof window === "undefined") return Promise.resolve(null)
  loading ??= import("@/vendor/distortion-waves/controller").then(
    (module) => (engine = module),
    () => {
      loading = null
      return null
    },
  )
  return loading
}

const sleep = (ms: number) => new Promise<null>((resolve) => setTimeout(() => resolve(null), ms))
const prefersReducedMotion = () => window.matchMedia("(prefers-reduced-motion: reduce)").matches

interface Press {
  x: number
  y: number
  t: number
}

type Handover = "wave" | "fade" | "cut"

/**
 * One transition, from the first view to the second. Owns the engine's controller for as long as
 * anything is on screen and removes every trace of it afterwards.
 */
function runWave({
  root,
  first,
  second,
  type,
  press,
  onStart,
  onReveal,
}: {
  root: HTMLElement
  first: HTMLElement
  second: HTMLElement
  type: WaveType
  press: Press | null
  /**
   * The handover is starting to show, exactly once per run. For a wave, `launchIn` is the
   * seconds until its front leaves the source.
   */
  onStart: (how: Handover, launchIn: number) => void
  /**
   * The second view is now the live one. Called exactly once unless the run is stopped first.
   * `calmAt` is when (performance.now) the element may start changing size.
   */
  onReveal: (how: Handover, calmAt: number) => void
}) {
  let stopped = false
  let started = false
  let shown = false
  let calmAt = 0
  let controller: WaveController | null = null
  let timer: ReturnType<typeof setTimeout> | undefined
  const cleanups: (() => void)[] = []

  const stop = () => {
    if (stopped) return
    stopped = true
    clearTimeout(timer)
    for (const cleanup of cleanups) cleanup()
    controller?.destroy()
    controller = null
  }
  const begun = (how: Handover, launchIn = 0) => {
    if (started) return
    started = true
    onStart(how, launchIn)
  }
  const reveal = (how: Handover) => {
    if (stopped || shown) return
    begun(how)
    shown = true
    onReveal(how, how === "wave" ? calmAt : 0)
  }
  /** Give up on the wave and show the second view. */
  const cut = (how: Handover) => {
    if (stopped) return
    controller?.destroy()
    controller = null
    reveal(how)
    stop()
  }

  const begin = async () => {
    if (prefersReducedMotion()) return cut("fade")
    const loaded = engine ?? (await Promise.race([preloadWave(), sleep(LOAD_BUDGET)]))
    if (stopped) return
    if (!loaded) return cut("fade")

    const wave = (controller = new loaded.WaveController(root, first, second))
    const source = root.querySelector("[data-wave-source]")
    let focus: WaveOptions["focus"]
    if (source && press && performance.now() - press.t < 1500) {
      const s = source.getBoundingClientRect()
      if (press.x >= s.left && press.x <= s.right && press.y >= s.top && press.y <= s.bottom) {
        const box = root.getBoundingClientRect()
        focus = { x: press.x - box.left - root.clientLeft, y: press.y - box.top - root.clientTop }
      }
    }
    const style = STYLE[type]
    const options: WaveOptions = {
      type,
      palette: PALETTE,
      waveStyle: style,
      source,
      focus,
      // The pictures are taken once, below: nothing in an inert view changes while the wave is out.
      live: false,
      prewarm: false,
      onFinish: stop,
    }
    wave.update({ ...options, revealed: false })
    // The controller has just marked the first view as the live one; it is not, a wave is leaving it.
    first.inert = true

    // The controller makes the second view live (drops `inert`) once the front has left the element.
    const live = new MutationObserver(() => {
      if (!second.hasAttribute("inert")) reveal("wave")
    })
    live.observe(second, { attributes: true, attributeFilter: ["inert"] })
    cleanups.push(() => live.disconnect())

    // The pictures show one theme; if it changes mid-flight, stop there.
    const theme = new MutationObserver(() => cut("cut"))
    theme.observe(document.documentElement, { attributes: true, attributeFilter: ["class", "style"] })
    cleanups.push(() => theme.disconnect())

    await Promise.race([wave.prepare(), sleep(PICTURE_BUDGET)])
    if (stopped) return
    if (!wave.debug.webgl) return cut("fade")
    wave.update({ ...options, revealed: true })
    begun("wave", style.anticipation ?? 0)
    calmAt = performance.now() + ((style.anticipation ?? 0) + CALM * (style.travel ?? 0)) * 1000 + 50
    timer = setTimeout(() => cut("cut"), SAFETY)
  }

  // A hidden tab paints nothing: keep the first view and let the wave run when the payer is back.
  if (document.visibilityState === "hidden") {
    const onVisible = () => {
      if (document.visibilityState !== "visible") return
      document.removeEventListener("visibilitychange", onVisible)
      void begin()
    }
    document.addEventListener("visibilitychange", onVisible)
    cleanups.push(() => document.removeEventListener("visibilitychange", onVisible))
  } else {
    void begin()
  }

  return stop
}

/* -------------------------------- component -------------------------------- */

const WaveEntranceContext = React.createContext(false)

/**
 * True inside a view that arrived through a `WaveTransition`. Such a view is pictured the
 * moment it mounts, so it should render finished: skip pop-ins, drawn checks and the like.
 */
export function useWaveEntrance() {
  return React.use(WaveEntranceContext)
}

type Phase = "first" | "wave" | "second"

/* The two views share one grid cell. Constant objects: React writes a layer's style only when
   the phase changes, never over what the engine sets while a wave is out. */
const LAYER: React.CSSProperties = { gridArea: "1 / 1", isolation: "isolate" }
const LAYER_HIDDEN: React.CSSProperties = { ...LAYER, opacity: 0, pointerEvents: "none" }
/* Grid items default to min-size auto; these let a view shrink and scroll inside (overridable). */
const LAYER_CLASS = "min-h-0 min-w-0"

export interface WaveTransitionProps extends Omit<React.ComponentProps<"div">, "children"> {
  /** Which view shows. false → true runs the wave; true → false swaps back at once. */
  revealed: boolean
  /** The first view. */
  children: React.ReactNode
  /** The view the wave leaves behind. Only rendered while `revealed`. */
  revealedContent: React.ReactNode
  /**
   * "vertical" (default): up from the control that was pressed. "center": a ring out from a
   * mark, for changes nobody pressed anything for (a payment confirming on-chain).
   */
  type?: WaveType
  /** Fetch the engine when the browser is idle. Set it once a transition has become possible. Default true. */
  armed?: boolean
  /** Classes for both view wrappers (they are the grid items; lay the view out here). */
  layerClassName?: string
  /** The sound of this moment, timed to the wave. */
  cue?: CueName
  /** The second view is live: interactive, focusable, announced. */
  onRevealed?: () => void
}

export function WaveTransition({
  revealed,
  children,
  revealedContent,
  type = "vertical",
  armed = true,
  layerClassName,
  cue,
  onRevealed,
  className,
  onPointerDownCapture,
  ...props
}: WaveTransitionProps) {
  const root = React.useRef<HTMLDivElement>(null)
  const first = React.useRef<HTMLDivElement>(null)
  const second = React.useRef<HTMLDivElement>(null)
  const press = React.useRef<Press | null>(null)
  const run = React.useRef<(() => void) | null>(null)
  const handover = React.useRef<{ height: number; how: Handover; calmAt: number; refocus: boolean } | null>(null)
  const play = useSound()

  const [phase, setPhase] = React.useState<Phase>(revealed ? "second" : "first")
  const [seen, setSeen] = React.useState(revealed)
  /** The second view came in through a transition (as opposed to being there on load). */
  const [entered, setEntered] = React.useState(false)
  if (seen !== revealed) {
    setSeen(revealed)
    setPhase(revealed ? "wave" : "first")
    setEntered(revealed)
  }

  React.useEffect(() => {
    if (!armed || revealed || prefersReducedMotion()) return
    const idle = window.requestIdleCallback
    if (!idle) {
      const id = setTimeout(preloadWave, 200)
      return () => clearTimeout(id)
    }
    const id = idle(() => void preloadWave(), { timeout: 2000 })
    return () => window.cancelIdleCallback(id)
  }, [armed, revealed])

  const start = React.useEffectEvent(() => {
    const el = root.current
    const a = first.current
    const b = second.current
    if (!el || !a || !b) return
    run.current?.()
    // A view a wave is leaving takes no more input. (Set here, not by React, so that where the
    // focus was can be read first.)
    const refocus = a.contains(document.activeElement)
    a.inert = true
    // Hold the element at the first view's size while the wave is out, whatever the second
    // view needs: nothing may shift before the wave. (The row is minmax(0, 1fr), so a taller
    // second view runs past the edge for now instead of stretching the first.)
    b.style.display = "none"
    const held = el.getBoundingClientRect().height
    b.style.display = ""
    el.style.height = `${held}px`
    el.style.overflow = "hidden"
    const asked = performance.now()
    run.current = runWave({
      root: el,
      first: a,
      second: b,
      type,
      press: press.current,
      onStart: (how, launchIn) => {
        if (cue && performance.now() - asked < CUE_HOLD) play(cue, { delay: launchIn, still: how !== "wave" })
      },
      onReveal: (how, calmAt) => {
        handover.current = { height: held, how, calmAt, refocus }
        setPhase("second")
      },
    })
  })
  const revealedEvent = React.useEffectEvent(() => onRevealed?.())

  React.useLayoutEffect(() => {
    if (phase === "wave") start()
    const el = root.current
    const b = second.current
    if (!el || !b) return
    if (phase === "first") {
      run.current?.()
      run.current = null
      handover.current = null
      el.style.height = el.style.overflow = ""
      if (first.current) first.current.inert = false
    }
    if (phase !== "second") return
    const h = handover.current
    handover.current = null
    if (!h) return

    // The first view is gone now; ease to the height the second one wants, once the wave has
    // stopped drawing its pictures (it holds the old height until then).
    el.style.height = ""
    const height = el.getBoundingClientRect().height
    if (h.how !== "cut" && Math.abs(height - h.height) > 1 && !prefersReducedMotion()) {
      const settle = el.animate([{ height: `${h.height}px` }, { height: `${height}px` }], {
        duration: 320,
        delay: Math.max(0, h.calmAt - performance.now()),
        easing: EASE_OUT_QUINT,
        fill: "backwards",
      })
      const release = () => void (el.style.overflow = "")
      settle.finished.then(release, release)
    } else {
      el.style.overflow = ""
    }
    if (h.how === "fade") b.animate([{ opacity: 0 }, { opacity: 1 }], { duration: 200, easing: "ease-out" })

    // Focus follows the view: to what the second view asks for, else, if the focus was in the
    // first view, to the dialog or the view itself. Otherwise it stays where the person left it.
    const target = b.querySelector<HTMLElement>("[data-autofocus]")
    if (target) target.focus({ preventScroll: true })
    else if (h.refocus) (el.closest<HTMLElement>('[role="dialog"]') ?? b).focus({ preventScroll: true })
    revealedEvent()
  }, [phase])

  React.useEffect(
    () => () => {
      run.current?.()
      run.current = null
    },
    [],
  )

  return (
    <div
      ref={root}
      data-wave-phase={phase}
      className={cn("relative isolate grid grid-rows-[minmax(0,1fr)]", className)}
      onPointerDownCapture={(event) => {
        press.current = { x: event.clientX, y: event.clientY, t: event.timeStamp }
        onPointerDownCapture?.(event)
      }}
      {...props}
    >
      <div
        ref={first}
        data-wave-layer="first"
        className={cn(LAYER_CLASS, layerClassName)}
        style={phase === "second" ? LAYER_HIDDEN : LAYER}
        inert={phase === "second"}
        aria-hidden={phase === "second" || undefined}
      >
        {phase === "second" ? null : children}
      </div>
      <div
        ref={second}
        data-wave-layer="second"
        tabIndex={-1}
        className={cn(LAYER_CLASS, "outline-none", layerClassName)}
        style={phase === "second" ? LAYER : LAYER_HIDDEN}
        inert={phase !== "second"}
        aria-hidden={phase !== "second" || undefined}
      >
        {phase === "first" ? null : <WaveEntranceContext value={entered}>{revealedContent}</WaveEntranceContext>}
      </div>
    </div>
  )
}
