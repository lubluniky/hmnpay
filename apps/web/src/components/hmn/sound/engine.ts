import { CUE_INFO, type CueName } from "./cues"
import { createBus, LEAD, scheduleCue, type Bus } from "./palette"

/**
 * The live half of the sound system: one bus on the page's one `AudioContext`, rate limits,
 * late-cue rejection, haptics, and putting the context back to sleep.
 *
 * Loaded with `import()` by `sound.tsx` on the first user gesture; nothing here runs before
 * that. The context itself is created there too (Safari only starts one inside a gesture) and
 * handed over with `attach`.
 */

let ctx: AudioContext | null = null
let bus: Bus | null = null

export function attach(context: AudioContext) {
  if (ctx === context) return
  ctx = context
  bus = createBus(context)
  // Whenever it starts running, for whatever reason, make sure it is put back to sleep.
  context.addEventListener("statechange", rest)
  rest()
}

/* --------------------------------- idling --------------------------------- */

let busyUntil = 0
let idle: ReturnType<typeof setTimeout> | undefined

/**
 * A running context keeps the audio hardware awake. Whenever nothing is sounding, suspend it:
 * no processing, no timers. One timeout at a time, cleared by the next cue.
 */
function rest() {
  clearTimeout(idle)
  const context = ctx
  if (!context) return
  const wait = Math.max(0, busyUntil - context.currentTime) + 0.25
  idle = setTimeout(() => {
    idle = undefined
    if (context.state === "running" && context.currentTime >= busyUntil) void context.suspend().catch(() => {})
  }, wait * 1000)
}

/* ------------------------------- rate limits ------------------------------ */

/** Seconds before the same cue may sound again. */
const GAP: Record<CueName, number> = {
  copy: 0.09,
  detected: 1,
  attention: 1,
  closed: 1,
  created: 0.6,
  paid: 2,
  received: 2.5,
}
/** `received` on top of its gap: three in a burst, then one more every 20 s. A busy shop is not a slot machine. */
const BURST = 3
const REFILL = 20
/** Cues that may sound at once. */
const VOICES = 4
/**
 * Seconds a cue may start late (chunk still loading, context still waking) before it is dropped
 * instead. A tick that trails its tap is worse than no tick.
 */
const LATE: Record<CueName, number> = {
  copy: 0.14,
  detected: 0.4,
  attention: 0.4,
  closed: 0.4,
  created: 0.3,
  paid: 0.3,
  received: 0.5,
}

const last = new Map<CueName, number>()
let tokens = BURST
let refilled = 0
const sounding: number[] = []

function allowed(name: CueName, now: number) {
  const previous = last.get(name)
  if (previous !== undefined && now - previous < GAP[name]) return false
  while (sounding.length && sounding[0] <= now) sounding.shift()
  if (sounding.length >= VOICES) return false
  if (name === "received") {
    const earned = Math.floor((now - refilled) / REFILL)
    if (earned > 0) {
      tokens = Math.min(BURST, tokens + earned)
      refilled = tokens === BURST ? now : refilled + earned * REFILL
    }
    if (tokens === 0) return false
    if (tokens === BURST) refilled = now
    tokens--
  }
  last.set(name, now)
  return true
}

/* ---------------------------------- play ---------------------------------- */

export interface PlayRequest {
  /** `performance.now()` when the product asked for the cue. */
  requested: number
  /** Seconds after `requested` at which the cue's main note should land (it is aimed at an animation). */
  delay?: number
  /** See `CueOptions.still`. */
  still?: boolean
  /** Skip the rate limits: the styleguide's audition buttons. */
  audition?: boolean
}

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms))

function buzz(name: CueName, afterMs: number) {
  const pattern = CUE_INFO[name].haptic
  // Only after a gesture (Chrome logs an intervention otherwise), and only where it exists.
  if (!pattern || typeof navigator.vibrate !== "function" || navigator.userActivation?.hasBeenActive === false) return
  const run = () => {
    try {
      navigator.vibrate(pattern)
    } catch {}
  }
  if (afterMs > 30) setTimeout(run, afterMs)
  else run()
}

/** Play a cue now, or not at all. Resolves to whether it was scheduled. Never throws. */
export async function play(name: CueName, { requested, delay = 0, still = false, audition = false }: PlayRequest) {
  const context = ctx
  if (!context || !bus) return false
  try {
    if (!audition && !allowed(name, performance.now() / 1000)) return false
    // Read through a function: the state changes while we wait, which narrowing cannot know.
    const running = () => context.state === "running"
    if (!running()) {
      // Waking a suspended context is quick; one the browser refuses to start never resolves.
      await Promise.race([context.resume(), sleep(350)])
      if (!running()) return false
    }
    // Start early by the cue's lead-in, so that its main note is what lands on `delay`.
    const lead = still ? 0 : (LEAD[name] ?? 0)
    const startIn = Math.max(0, delay - lead)
    const late = (performance.now() - requested) / 1000
    if (late - startIn > LATE[name]) return false

    const wait = Math.max(0, startIn - late)
    const cue = scheduleCue(context, bus, name, context.currentTime + 0.012 + wait, { still })
    sounding.push(performance.now() / 1000 + wait + CUE_INFO[name].length)
    sounding.sort((a, b) => a - b)
    busyUntil = Math.max(busyUntil, cue.end + bus.tail)
    rest()
    // The buzz lands with the main note, not with what leads into it.
    buzz(name, (wait + lead) * 1000)
    // For tests and recordings: what was scheduled, and for when (`performance.now()` time).
    window.dispatchEvent(new CustomEvent("hmn:cue", { detail: { name, still, at: performance.now() + (0.012 + wait) * 1000 } }))
    return true
  } catch {
    return false
  }
}
