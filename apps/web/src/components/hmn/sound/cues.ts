/**
 * The hmnpay sound palette, as data: what each cue is called, when it fires and how it reads.
 * No audio code here — this file is safe to import anywhere (the styleguide lists it).
 * The synthesis lives in `palette.ts` and is only ever loaded on demand.
 *
 * One key for everything: D major, A4 = 440 Hz. Cues that land close together are consonant,
 * and the three "money" cues tell one story in solfège:
 *   created  mi → sol   (opens: something payable exists)
 *   detected sol        (held: we see it)
 *   paid     sol → do   (closes: resolved)
 */

export type CueName = "copy" | "detected" | "attention" | "closed" | "created" | "paid" | "received"

export interface CueInfo {
  name: CueName
  label: string
  /** When it fires, in product terms. */
  when: string
  /** What you hear. */
  sound: string
  /** Notes, low to high in the order they sound. */
  notes: string
  /** Seconds until the dry sound is gone (the room adds a short tail). */
  length: number
  /** `navigator.vibrate` pattern paired with the cue (Android). */
  haptic?: number | number[]
}

/** In the order a payer, then a merchant, would hear them. */
export const CUES: readonly CueInfo[] = [
  {
    name: "copy",
    label: "Copy",
    when: "An address, amount, link, key or hash was copied.",
    sound: "A dry, tactile tick: a felt-mallet tap with a pitch you barely notice.",
    notes: "D6",
    length: 0.07,
    haptic: 8,
  },
  {
    name: "detected",
    label: "Payment detected",
    when: "Checkout: the transfer was seen on-chain (awaiting → detected / confirming).",
    sound: "One soft glass note that stays open: the question the paid cue answers.",
    notes: "A4 + A5",
    length: 0.26,
    haptic: 10,
  },
  {
    name: "attention",
    label: "Needs action",
    when: "Checkout: part of the amount arrived (underpaid).",
    sound: "A neutral double tap on an open fifth. Neither good news nor bad.",
    notes: "A4 + E5, twice",
    length: 0.3,
    haptic: [10, 70, 10],
  },
  {
    name: "closed",
    label: "Expired / failed",
    when: "Checkout: the payment expired or the transfer was rejected.",
    sound: "Two low, muffled notes stepping down to rest. Soft, never a buzzer.",
    notes: "F♯4 → D4",
    length: 0.3,
    haptic: 16,
  },
  {
    name: "created",
    label: "Created",
    when: "Dashboard: a payment or a payment link was created, or setup finished.",
    sound: "A light two-note lift, the smaller cousin of the paid cue. Opens, does not resolve.",
    notes: "F♯5 → A5",
    length: 0.34,
    haptic: 10,
  },
  {
    name: "paid",
    label: "Payment complete",
    when: "Checkout: the payment is confirmed. Rides the wave: grace note as it gathers, strike as it launches.",
    sound: "The signature. A grace note, then a warm bell on the tonic that blooms into a major chord and rings out.",
    notes: "A5 → D6 over D4 · D5 · F♯5 · A5",
    length: 1.15,
    haptic: [14, 70, 24],
  },
  {
    name: "received",
    label: "Payment received",
    when: "Dashboard: a payment turned paid while you are looking. At most 3 in a burst, then one per 20 s.",
    sound: "The paid motif, compact and brighter: a muted tap, then a small coin-like ring.",
    notes: "A5 → D6 + A6",
    length: 0.5,
    haptic: [10, 50, 14],
  },
]

export const CUE_INFO = Object.fromEntries(CUES.map((cue) => [cue.name, cue])) as Record<CueName, CueInfo>
