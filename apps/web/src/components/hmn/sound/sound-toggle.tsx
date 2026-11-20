"use client"

import { Volume2Icon, VolumeXIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

import { setSoundEnabled, useSound, useSoundEnabled, useSoundSurface } from "./sound"

/**
 * Sound on / off for the surface it sits in, remembered per browser. Sits next to the theme
 * toggle. Turning sound on answers with the quietest cue, so "on" is heard at once.
 */
export function SoundToggle({ className, size = "icon", ...props }: Omit<React.ComponentProps<typeof Button>, "onClick">) {
  const surface = useSoundSurface()
  const on = useSoundEnabled(surface)
  const play = useSound()

  return (
    <Button
      type="button"
      variant="ghost"
      size={size}
      aria-label="Sound"
      aria-pressed={on}
      title={on ? "Sound on" : "Sound off"}
      className={cn("text-muted-foreground hover:text-foreground", className)}
      onClick={() => {
        setSoundEnabled(surface, !on)
        // A beat later: the engine may still be on its way the first time.
        if (!on) play("copy", { delay: 0.1 })
      }}
      {...props}
    >
      {on ? <Volume2Icon /> : <VolumeXIcon />}
    </Button>
  )
}
