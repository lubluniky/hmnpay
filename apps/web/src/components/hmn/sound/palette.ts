import type { CueName } from "./cues"

/**
 * The hmnpay sound palette, synthesised. No samples, no assets: every cue is a handful of
 * oscillators, so the whole palette is a few kB of code and stays tunable here.
 *
 * Works on any `BaseAudioContext`: the live engine (`engine.ts`) and the offline renderer that
 * writes the audition files (`.shots/sound`) call the same two functions, `createBus` and
 * `scheduleCue`, so what is measured is what plays.
 *
 * The instrument is one voice, a soft glass mallet (`strike`):
 *   - a sine carrier with a 3 ms attack and an exponential decay;
 *   - a little FM from a non-integer ratio whose depth dies in ~35 ms: a bright, slightly
 *     inharmonic "tink" that mellows into an almost pure tone (bell, not beep);
 *   - an octave partial for body and a 2.76× partial (the second mode of a struck bar) for glass,
 *     both shorter than the fundamental;
 *   - a few milliseconds of band-passed noise: the felt touching the bar.
 * Plus `pad`, slow sine swells for the bloom under the paid cue. Everything goes through one
 * bus: a gentle low-pass, a short generated room and a soft clipper that only acts on overload.
 *
 * Imported with a dynamic `import()` only. Keep it free of React and of runtime imports.
 */

/* --------------------------------- tuning --------------------------------- */

const A4 = 440
const note = (semitonesFromA4: number) => A4 * 2 ** (semitonesFromA4 / 12)

/** D major. */
const D4 = note(-7)
const Fs4 = note(-3)
const A4n = note(0)
const D5 = note(5)
const E5 = note(7)
const Fs5 = note(9)
const A5 = note(12)
const D6 = note(17)
const Fs6 = note(21)
const A6 = note(24)

/** Product-wide output level. Peaks land around -12 dBFS: a UI sound, well under media level. */
export const MASTER_VOLUME = 0.7

/** How long the room rings (to -60 dB), seconds. */
const ROOM_T60 = 0.85
const ROOM_LENGTH = 1.0

/**
 * Seconds from the start of a cue to the note that carries it. A cue that rides the wave is
 * aimed so that this note lands when the wave launches: what comes before it sounds while the
 * source gathers.
 */
export const LEAD: Partial<Record<CueName, number>> = { paid: 0.18, created: 0.08 }

/* ----------------------------------- bus ---------------------------------- */

export interface Bus {
  /** Dry input. */
  input: GainNode
  /** Room input. */
  send: GainNode
  /** Seconds the room keeps ringing after the last dry sound. */
  tail: number
}

/** Deterministic noise, so the room (and with it every rendered file) is identical everywhere. */
function prng(seed: number) {
  let s = seed >>> 0
  return () => {
    s = (s + 0x6d2b79f5) >>> 0
    let t = Math.imul(s ^ (s >>> 15), 1 | s)
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

/** A small, dark room: decorrelated left / right noise, exponential decay, highs dying first. */
function roomImpulse(ctx: BaseAudioContext) {
  const rate = ctx.sampleRate
  const length = Math.floor(rate * ROOM_LENGTH)
  const buffer = ctx.createBuffer(2, length, rate)
  const predelay = 0.012
  for (let channel = 0; channel < 2; channel++) {
    const random = prng(channel === 0 ? 0x68_6d_6e_01 : 0x68_6d_6e_02)
    const data = buffer.getChannelData(channel)
    let low = 0
    let energy = 0
    for (let i = 0; i < length; i++) {
      const t = i / rate - predelay
      if (t < 0) continue
      // One-pole low-pass whose cutoff falls from 6.5 kHz to 1.2 kHz as the room decays.
      const cutoff = 1200 + 5300 * Math.exp(-t * 5)
      low += (1 - Math.exp((-2 * Math.PI * cutoff) / rate)) * (random() * 2 - 1 - low)
      const onset = Math.min(1, t / 0.006)
      const value = low * onset * Math.exp((-6.91 * t) / ROOM_T60)
      data[i] = value
      energy += value * value
    }
    const scale = 1 / Math.sqrt(energy)
    for (let i = 0; i < length; i++) data[i] *= scale
  }
  return buffer
}

/** Linear up to half scale, then eases into a ceiling of 0.88: silent unless cues pile up. */
function softClipCurve() {
  const curve = new Float32Array(2049)
  for (let i = 0; i < curve.length; i++) {
    const x = (i / (curve.length - 1)) * 2 - 1
    const a = Math.abs(x)
    curve[i] = Math.sign(x) * (a <= 0.5 ? a : 0.5 + 0.5 * Math.tanh((a - 0.5) / 0.5))
  }
  return curve
}

/** The one signal path every cue shares. Build it once per context. */
export function createBus(ctx: BaseAudioContext, destination: AudioNode = ctx.destination, volume = MASTER_VOLUME): Bus {
  const input = ctx.createGain()
  const send = ctx.createGain()

  const room = ctx.createConvolver()
  room.normalize = false
  room.buffer = roomImpulse(ctx)
  const wet = ctx.createGain()
  wet.gain.value = 0.5

  const soften = ctx.createBiquadFilter()
  soften.type = "lowpass"
  soften.frequency.value = 9000
  soften.Q.value = 0.5

  const ceiling = ctx.createWaveShaper()
  ceiling.curve = softClipCurve()

  const master = ctx.createGain()
  master.gain.value = volume

  input.connect(soften)
  send.connect(room).connect(wet).connect(soften)
  soften.connect(ceiling).connect(master).connect(destination)
  return { input, send, tail: ROOM_T60 }
}

/* --------------------------------- voices --------------------------------- */

interface Strike {
  /** Hz. */
  f: number
  /** Context time of the hit. */
  at: number
  /** Peak level of the fundamental, 0–1. */
  gain: number
  /** Seconds to -60 dB. */
  decay: number
  /** FM depth at the hit (modulation index). 0 = pure sine, 1 = clearly struck. */
  bright?: number
  /** Level of the octave partial relative to `gain`. */
  body?: number
  /** Level of the 2.76× partial relative to `gain`. */
  glass?: number
  /** Level of the felt-on-bar noise relative to `gain`. */
  tap?: number
  /** -1 (left) … 1 (right). */
  pan?: number
  /** Cents: adds a quiet second carrier this much sharp on the other side, for a slow shimmer. */
  wide?: number
}

interface Pad {
  f: number
  at: number
  gain: number
  /** Seconds to the peak. */
  attack: number
  /** Seconds from the peak to -60 dB. */
  decay: number
  pan?: number
}

const ATTACK = 0.003
/** Seconds to -60 dB → time constant of the exponential. */
const tau = (seconds: number) => seconds / 6.91

const noiseBuffers = new WeakMap<BaseAudioContext, AudioBuffer>()
function noise(ctx: BaseAudioContext) {
  let buffer = noiseBuffers.get(ctx)
  if (!buffer) {
    buffer = ctx.createBuffer(1, Math.floor(ctx.sampleRate * 0.05), ctx.sampleRate)
    const random = prng(0x74_61_70)
    const data = buffer.getChannelData(0)
    for (let i = 0; i < data.length; i++) data[i] = random() * 2 - 1
    noiseBuffers.set(ctx, buffer)
  }
  return buffer
}

/** Everything a cue is built from, bound to one context and one output. */
function voices(ctx: BaseAudioContext, out: AudioNode) {
  const sources: AudioScheduledSourceNode[] = []
  let end = 0

  const placed = (pan: number | undefined) => {
    if (!pan || typeof ctx.createStereoPanner !== "function") return out
    const panner = ctx.createStereoPanner()
    panner.pan.value = pan
    panner.connect(out)
    return panner
  }

  const run = (source: AudioScheduledSourceNode, from: number, until: number) => {
    source.start(from)
    source.stop(until)
    sources.push(source)
    end = Math.max(end, until)
  }

  /** A sine that is hit and dies away. */
  const partial = (f: number, at: number, gain: number, decay: number, target: AudioNode, detune = 0) => {
    const osc = ctx.createOscillator()
    osc.frequency.value = f
    osc.detune.value = detune
    const amp = ctx.createGain()
    amp.gain.setValueAtTime(0, at)
    amp.gain.linearRampToValueAtTime(gain, at + ATTACK)
    amp.gain.setTargetAtTime(0, at + ATTACK, tau(decay))
    osc.connect(amp).connect(target)
    run(osc, at, at + ATTACK + decay)
    return osc
  }

  const tap = (at: number, gain: number, centre: number, target: AudioNode = out) => {
    const source = ctx.createBufferSource()
    source.buffer = noise(ctx)
    const band = ctx.createBiquadFilter()
    band.type = "bandpass"
    band.frequency.value = centre
    band.Q.value = 1.1
    const amp = ctx.createGain()
    amp.gain.setValueAtTime(0, at)
    amp.gain.linearRampToValueAtTime(gain, at + 0.001)
    amp.gain.setTargetAtTime(0, at + 0.001, 0.004)
    source.connect(band).connect(amp).connect(target)
    run(source, at, at + 0.04)
  }

  const strike = ({ f, at, gain, decay, bright = 0.5, body = 0, glass = 0, tap: felt = 0.25, pan, wide = 0 }: Strike) => {
    const target = placed(pan)
    const carriers = [partial(f, at, wide ? gain * 0.8 : gain, decay, target)]
    // A quieter twin a few cents sharp, off to the other side: the slow beat of a real bell.
    if (wide) carriers.push(partial(f, at, gain * 0.25, decay, placed(-(pan ?? 0)), wide))

    if (bright > 0) {
      // 3.53: no partial of the tone lines up with a harmonic, which is what makes it glass.
      const mod = ctx.createOscillator()
      mod.frequency.value = f * 3.53
      const depth = ctx.createGain()
      depth.gain.setValueAtTime(bright * f, at)
      depth.gain.setTargetAtTime(bright * f * 0.04, at, 0.035)
      mod.connect(depth)
      for (const carrier of carriers) depth.connect(carrier.frequency)
      run(mod, at, at + ATTACK + decay)
    }
    if (body > 0) partial(f * 2, at, gain * body, decay * 0.55, target)
    if (glass > 0) partial(f * 2.76, at, gain * glass, decay * 0.32, target)
    if (felt > 0) tap(at, gain * felt, Math.min(f * 3, 5200), target)
  }

  const pad = ({ f, at, gain, attack, decay, pan }: Pad) => {
    const osc = ctx.createOscillator()
    osc.frequency.value = f
    const amp = ctx.createGain()
    amp.gain.setValueAtTime(0, at)
    // An S-curve up (no audible onset), exponential down.
    amp.gain.setTargetAtTime(gain, at, attack / 3)
    amp.gain.setTargetAtTime(0, at + attack, tau(decay))
    osc.connect(amp).connect(placed(pan))
    run(osc, at, at + attack + decay)
  }

  return { strike, pad, tap, sources, end: () => end }
}

type Voices = ReturnType<typeof voices>

/* ---------------------------------- cues ---------------------------------- */

interface CueSpec {
  /** Level of the whole cue: where the palette is balanced. */
  trim: number
  /** How much of it goes to the room, 0–1. */
  room: number
  /** Low-pass over the cue, Hz. */
  tone: number
  build: (v: Voices, t: number, still: boolean) => void
}

const SPECS: Record<CueName, CueSpec> = {
  copy: {
    trim: 0.5,
    room: 0.04,
    tone: 7000,
    build(v, t) {
      v.tap(t, 0.34, 3200)
      v.strike({ f: D6, at: t, gain: 0.3, decay: 0.075, bright: 1.1, glass: 0.2, tap: 0.6 })
    },
  },
  detected: {
    trim: 0.72,
    room: 0.2,
    tone: 5200,
    build(v, t) {
      v.strike({ f: A5, at: t, gain: 0.46, decay: 0.3, bright: 0.45, body: 0.14, glass: 0.05 })
      v.strike({ f: A4n, at: t, gain: 0.3, decay: 0.24, bright: 0.2, body: 0.3, tap: 0 })
    },
  },
  attention: {
    trim: 0.88,
    room: 0.14,
    tone: 4600,
    build(v, t) {
      for (const [after, level] of [
        [0, 1],
        [0.13, 0.7],
      ]) {
        v.strike({ f: E5, at: t + after, gain: 0.4 * level, decay: 0.19, bright: 0.4, body: 0.18 })
        v.strike({ f: A4n, at: t + after, gain: 0.34 * level, decay: 0.17, bright: 0.25, body: 0.3, tap: 0 })
      }
    },
  },
  closed: {
    trim: 0.53,
    room: 0.12,
    tone: 2400,
    build(v, t) {
      // Octave partials carry these on phone speakers, which have little below 400 Hz.
      v.strike({ f: Fs4, at: t, gain: 0.48, decay: 0.2, bright: 0.25, body: 0.9, tap: 0.3 })
      v.strike({ f: D4, at: t + 0.11, gain: 0.52, decay: 0.24, bright: 0.2, body: 0.9, tap: 0.2 })
    },
  },
  created: {
    trim: 0.91,
    room: 0.2,
    tone: 5600,
    build(v, t) {
      v.strike({ f: Fs5, at: t, gain: 0.46, decay: 0.2, bright: 0.5, body: 0.24, pan: -0.15 })
      v.strike({ f: A5, at: t + LEAD.created!, gain: 0.52, decay: 0.34, bright: 0.55, body: 0.18, glass: 0.06, pan: 0.15 })
    },
  },
  paid: {
    trim: 0.55,
    room: 0.3,
    tone: 6200,
    build(v, t, still) {
      // Without the wave there is no gather to ride and no swell to bloom with: the two notes, closer, drier.
      const hit = t + (still ? 0.1 : LEAD.paid!)
      v.strike({ f: A5, at: t, gain: 0.4, decay: 0.42, bright: 0.5, body: 0.28, pan: -0.18 })
      v.strike({ f: A4n, at: t, gain: 0.2, decay: 0.3, bright: 0.2, tap: 0, pan: -0.1 })
      v.strike({ f: D6, at: hit, gain: 0.5, decay: still ? 0.7 : 1.15, bright: 0.7, body: 0.12, glass: 0.1, pan: 0.2, wide: 5 })
      v.strike({ f: D5, at: hit, gain: 0.46, decay: still ? 0.6 : 1.0, bright: 0.35, body: 0.25, tap: 0, pan: -0.05 })
      v.strike({ f: D4, at: hit, gain: 0.28, decay: 0.55, bright: 0.15, body: 0.4, tap: 0 })
      if (still) return
      v.pad({ f: Fs5, at: hit, gain: 0.1, attack: 0.24, decay: 0.75, pan: -0.4 })
      v.pad({ f: A5, at: hit, gain: 0.08, attack: 0.28, decay: 0.7, pan: 0.4 })
      v.pad({ f: D5, at: hit, gain: 0.07, attack: 0.22, decay: 0.8 })
      v.strike({ f: Fs6, at: hit + 0.26, gain: 0.09, decay: 0.6, bright: 0.4, glass: 0.15, tap: 0.1, pan: 0.35 })
    },
  },
  received: {
    trim: 0.92,
    room: 0.22,
    tone: 7000,
    build(v, t) {
      v.strike({ f: A5, at: t, gain: 0.36, decay: 0.12, bright: 0.6, tap: 0.5, pan: -0.12 })
      v.strike({ f: D6, at: t + 0.075, gain: 0.46, decay: 0.5, bright: 0.9, body: 0.1, glass: 0.22, pan: 0.15 })
      v.strike({ f: A6, at: t + 0.075, gain: 0.15, decay: 0.38, bright: 0.5, glass: 0.2, tap: 0, pan: -0.25 })
    },
  },
}

export interface CueOptions {
  /**
   * The motion this cue belongs to is not playing (reduced motion, or the wave fell back to a
   * crossfade). Drops the parts that only exist to follow it. Only the paid cue has any.
   */
  still?: boolean
  /** Extra level for this one play, 0–1. */
  level?: number
}

/**
 * Schedule one cue at `at` (context time). Returns when its dry sound ends and a `stop` that
 * silences it early. Its nodes disconnect themselves once the last one has played.
 */
export function scheduleCue(ctx: BaseAudioContext, bus: Bus, name: CueName, at: number, options: CueOptions = {}) {
  const spec = SPECS[name]
  const out = ctx.createGain()
  out.gain.value = spec.trim * (options.level ?? 1)
  const tone = ctx.createBiquadFilter()
  tone.type = "lowpass"
  tone.frequency.value = spec.tone
  tone.Q.value = 0.6
  const room = ctx.createGain()
  room.gain.value = spec.room
  out.connect(tone)
  tone.connect(bus.input)
  tone.connect(room).connect(bus.send)

  const v = voices(ctx, out)
  spec.build(v, at, Boolean(options.still))

  let pending = v.sources.length
  const release = () => {
    tone.disconnect()
    room.disconnect()
  }
  for (const source of v.sources) {
    source.onended = () => {
      if (--pending === 0) release()
    }
  }
  return {
    end: v.end(),
    stop() {
      for (const source of v.sources) {
        source.onended = null
        try {
          source.stop()
        } catch {}
      }
      release()
    },
  }
}
