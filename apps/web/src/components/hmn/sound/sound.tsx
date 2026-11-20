"use client"

import * as React from "react"

import type { CueName } from "./cues"

/**
 * Sound for hmnpay: `useSound()` gives a `play(cue)` that is safe to call anywhere, any time.
 *
 * This file is the small, always-loaded half: the on / off preference, the unlock on the first
 * gesture, and the gates. The synthesiser (`palette.ts`) and the engine (`engine.ts`) are one
 * separate chunk, fetched on that first gesture, so a page that never makes a sound pays for
 * neither and the first paint pays for nothing.
 *
 * The rules a cue has to pass, in order:
 *  1. Sound is on for this surface (a per-browser preference; see `DEFAULT_ON`).
 *  2. The page is visible. A cue accompanies a change you can see; a hidden tab stays silent.
 *  3. The person has interacted with the page. Browsers refuse audio before that, so we do not
 *     ask: no context is created, nothing is logged, the cue is simply skipped.
 *  4. It is not rate-limited, and it can start on time (see `engine.ts`).
 * Sound is never the only signal: every cue goes with a visible change and the existing
 * `role="status"` announcements.
 */

/* ------------------------------- preference ------------------------------- */

export type SoundSurface = "checkout" | "dashboard"

/**
 * On for both, because every cue is gated on a gesture and on visibility, the level is modest,
 * and off is one tap away and remembered.
 *  - checkout: the cues answer what the payer just did (copy) or is waiting for (paid).
 *  - dashboard: the merchant runs their own instance; hearing a payment land is the point.
 * Separate keys: muting the dashboard you live in must not mute the checkout you are testing.
 */
const DEFAULT_ON: Record<SoundSurface, boolean> = { checkout: true, dashboard: true }

const storageKey = (surface: SoundSurface) => `hmn:sound:${surface}`

const subscribers = new Set<() => void>()

function readEnabled(surface: SoundSurface) {
  try {
    const stored = window.localStorage.getItem(storageKey(surface))
    return stored === null ? DEFAULT_ON[surface] : stored === "on"
  } catch {
    return DEFAULT_ON[surface]
  }
}

function subscribe(onChange: () => void) {
  subscribers.add(onChange)
  // Another tab of this instance changed it.
  window.addEventListener("storage", onChange)
  return () => {
    subscribers.delete(onChange)
    window.removeEventListener("storage", onChange)
  }
}

/** Whether sound is on for a surface. Renders the default on the server and while hydrating. */
export function useSoundEnabled(surface: SoundSurface) {
  return React.useSyncExternalStore(
    subscribe,
    () => readEnabled(surface),
    () => DEFAULT_ON[surface],
  )
}

/** Turn sound on or off for a surface. Call it from the click that asks for it. */
export function setSoundEnabled(surface: SoundSurface, on: boolean) {
  try {
    window.localStorage.setItem(storageKey(surface), on ? "on" : "off")
  } catch {}
  for (const notify of subscribers) notify()
  // This click is a gesture: sound can start from here on.
  if (on) unlock()
}

/* --------------------------------- unlock --------------------------------- */

type Engine = typeof import("./engine")

let context: AudioContext | null = null
let engine: Engine | null = null
let loading: Promise<Engine | null> | null = null
let unlocked = false

function loadEngine() {
  loading ??= import("./engine").then(
    (module) => {
      if (context) module.attach(context)
      return (engine = module)
    },
    () => {
      loading = null
      return null
    },
  )
  return loading
}

/**
 * Create the page's one `AudioContext` and start it. Called from inside a user gesture: Chrome
 * would also accept it any time after one (activation is sticky); Safari starts a context only
 * from within a gesture, and keeps it startable afterwards. The engine suspends it again as
 * soon as it is idle.
 */
function unlock() {
  unlocked = true
  try {
    if (!context) {
      const Context = window.AudioContext ?? (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext
      if (!Context) return
      context = new Context({ latencyHint: "interactive" })
      context.addEventListener("statechange", () => {
        if (context?.state !== "running" || started) return
        started = true
        disarm()
      })
      void loadEngine()
    }
    if (!started) void context.resume().catch(() => {})
  } catch {
    context = null
  }
}

const GESTURES = ["pointerdown", "keydown", "touchend"] as const
const armed = new Map<SoundSurface, number>()
/** The context has run once: from here on the engine can wake it without a gesture. */
let started = false
let listening = false

function disarm() {
  if (!listening) return
  listening = false
  for (const type of GESTURES) window.removeEventListener(type, onGesture, true)
}

function onGesture(event: Event) {
  if (!event.isTrusted) return
  // Wait for a gesture on a surface that wants sound; until then, create nothing.
  if (![...armed.keys()].some(readEnabled)) return
  // Until the context has started, every gesture is another chance to start it.
  unlock()
}

/** A surface that makes sound is on screen: listen for gestures until audio has started. */
function arm(surface: SoundSurface) {
  armed.set(surface, (armed.get(surface) ?? 0) + 1)
  if (!started && !listening) {
    listening = true
    for (const type of GESTURES) window.addEventListener(type, onGesture, { capture: true, passive: true })
  }
  return () => {
    const count = (armed.get(surface) ?? 1) - 1
    if (count > 0) armed.set(surface, count)
    else armed.delete(surface)
    if (armed.size === 0) disarm()
  }
}

/* ---------------------------------- play ---------------------------------- */

export interface PlayOptions {
  /** Seconds from now at which the cue's main note should land: aim it at an animation. */
  delay?: number
  /** The animation this cue belongs to is not playing; drop the parts that follow it. */
  still?: boolean
  /** Styleguide only: ignore the rate limits. */
  audition?: boolean
}

function play(surface: SoundSurface, name: CueName, options: PlayOptions = {}) {
  if (typeof window === "undefined") return
  if (!readEnabled(surface) || document.visibilityState !== "visible") return
  // A gesture made before this code was listening (while the page was still hydrating) counts.
  if (!unlocked && navigator.userActivation?.hasBeenActive) unlock()
  if (!unlocked || !context) return
  const request = { ...options, requested: performance.now() }
  if (engine) void engine.play(name, request)
  else void loadEngine().then((loaded) => loaded?.play(name, request))
}

const SoundContext = React.createContext<SoundSurface>("dashboard")

/** Declares which surface a tree is (its own preference and default) and arms the unlock. */
export function SoundProvider({ surface, children }: { surface: SoundSurface; children: React.ReactNode }) {
  React.useEffect(() => arm(surface), [surface])
  return <SoundContext value={surface}>{children}</SoundContext>
}

export function useSoundSurface() {
  return React.use(SoundContext)
}

/**
 * `const play = useSound()` then `play("copy")`. Stable, safe during SSR, silent when sound is
 * off, the tab is hidden or the browser will not allow it. Fire it with the visible change.
 */
export function useSound() {
  const surface = React.use(SoundContext)
  return React.useCallback((name: CueName, options?: PlayOptions) => play(surface, name, options), [surface])
}
