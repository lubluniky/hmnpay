"use client"

import { PlayIcon } from "lucide-react"

import { SoundProvider, SoundToggle, useSound, useSoundEnabled, useSoundSurface } from "@/components/hmn"
import { CUES, type CueInfo } from "@/components/hmn/sound/cues"
import { Button } from "@/components/ui/button"

/**
 * Every cue of the palette with a play button: where a contributor auditions and tunes.
 * The sounds are synthesised in src/components/hmn/sound/palette.ts; edit a number there,
 * save, press play. Audition ignores the rate limits, not the mute.
 */
export function SoundBoard() {
  return (
    <SoundProvider surface="dashboard">
      <Board />
    </SoundProvider>
  )
}

function Board() {
  const on = useSoundEnabled(useSoundSurface())
  return (
    <div className="overflow-hidden rounded-xl border border-border bg-card shadow-card">
      <div className="flex items-center justify-between gap-4 border-b border-border py-2.5 pr-3 pl-5">
        <p className="text-[13px] leading-5 text-muted-foreground">
          <span className="font-medium text-foreground">D major, one glass-mallet voice.</span> Synthesised in the browser, no
          audio files. {on ? "Sound is on for the dashboard in this browser." : "Sound is off: turn it on to audition."}
        </p>
        <SoundToggle />
      </div>
      <ul className="divide-y divide-border">
        {CUES.map((cue) => (
          <CueRow key={cue.name} cue={cue} />
        ))}
      </ul>
    </div>
  )
}

function CueRow({ cue }: { cue: CueInfo }) {
  const play = useSound()
  return (
    <li className="grid grid-cols-[auto_minmax(0,1fr)] items-start gap-x-4 px-5 py-3.5 lg:grid-cols-[auto_260px_minmax(0,1fr)_minmax(0,1fr)]">
      <Button
        type="button"
        variant="outline"
        size="icon"
        aria-label={`Play ${cue.label}`}
        data-cue={cue.name}
        className="row-span-3 mt-0.5 lg:row-span-1"
        onClick={() => play(cue.name, { audition: true })}
      >
        <PlayIcon className="size-3.5 fill-current" />
      </Button>
      <div className="min-w-0">
        <div className="text-sm leading-6 font-medium">{cue.label}</div>
        <div className="num text-xs leading-5 text-muted-foreground">
          {cue.name} · {cue.notes} · {Math.round(cue.length * 1000)} ms
        </div>
        {cue.name === "paid" && (
          <button
            type="button"
            className="focus-ring mt-1 rounded-sm text-xs text-muted-foreground underline decoration-border-strong underline-offset-4 transition-colors hover:text-foreground"
            onClick={() => play("paid", { audition: true, still: true })}
          >
            Play the short form (no wave)
          </button>
        )}
      </div>
      <p className="text-[13px] leading-5 text-pretty text-muted-foreground max-lg:mt-1">{cue.when}</p>
      <p className="text-[13px] leading-5 text-pretty text-muted-foreground max-lg:mt-1">{cue.sound}</p>
    </li>
  )
}
