import { useId } from "react"

import { cn } from "@/lib/utils"

const USDT_GLYPH =
  "M17.922 17.383v-.002c-.11.008-.677.042-1.942.042-1.01 0-1.721-.03-1.971-.042v.003c-3.888-.171-6.79-.848-6.79-1.658 0-.809 2.902-1.486 6.79-1.66v2.644c.254.018.982.061 1.988.061 1.207 0 1.812-.05 1.925-.06v-2.643c3.88.173 6.775.85 6.775 1.658 0 .81-2.895 1.485-6.775 1.657m0-3.59v-2.366h5.414V7.819H8.595v3.608h5.414v2.365c-4.4.202-7.709 1.074-7.709 2.118 0 1.044 3.309 1.915 7.709 2.118v7.582h3.913v-7.584c4.393-.202 7.694-1.073 7.694-2.116 0-1.043-3.301-1.914-7.694-2.117"

const BNB_GLYPH =
  "M12.116 14.404 16 10.52l3.886 3.886 2.26-2.26L16 6l-6.144 6.144 2.26 2.26ZM6 16l2.26-2.26L10.52 16l-2.26 2.26L6 16Zm6.116 1.596L16 21.48l3.886-3.886 2.26 2.259L16 26l-6.144-6.144-.003-.003 2.263-2.257ZM21.48 16l2.26-2.26L26 16l-2.26 2.26L21.48 16Zm-3.188-.002h.002V16L16 18.294l-2.291-2.29-.004-.004.004-.003.401-.402.195-.195L16 13.706l2.293 2.293Z"

/** Brand colours of the asset / network marks. The only place these hues may appear. */
export const ASSET_COLORS = { usdt: "#26A17B", bnb: "#F3BA2F" } as const

type IconProps = Omit<React.ComponentProps<"svg">, "children"> & {
  /** Pixel size (width = height). Crisp from 16 to 40+. */
  size?: number
}

/**
 * USDT token mark with a BNB Smart Chain corner badge.
 * The badge sits in a transparent cut-out, so the icon works on any surface.
 * `network={false}` renders the bare token (only where the network is stated right next to it).
 */
export function AssetIcon({ size = 24, network = true, className, ...props }: IconProps & { network?: boolean }) {
  const maskId = useId()
  return (
    <svg
      viewBox="0 0 32 32"
      width={size}
      height={size}
      role="img"
      aria-label={network ? "USDT on BNB Smart Chain" : "USDT"}
      className={cn("shrink-0", className)}
      {...props}
    >
      {network && (
        <mask id={maskId}>
          <rect width="32" height="32" fill="#fff" />
          <circle cx="25" cy="25" r="8.6" fill="#000" />
        </mask>
      )}
      <g mask={network ? `url(#${maskId})` : undefined}>
        <circle cx="16" cy="16" r="16" fill={ASSET_COLORS.usdt} />
        <path d={USDT_GLYPH} fill="#fff" fillRule="evenodd" />
      </g>
      {network && (
        <g transform="translate(18 18) scale(0.4375)">
          <circle cx="16" cy="16" r="16" fill={ASSET_COLORS.bnb} />
          <path d={BNB_GLYPH} fill="#fff" />
        </g>
      )}
    </svg>
  )
}

/** BNB Smart Chain mark on its own. */
export function NetworkIcon({ size = 16, className, ...props }: IconProps) {
  return (
    <svg
      viewBox="0 0 32 32"
      width={size}
      height={size}
      role="img"
      aria-label="BNB Smart Chain"
      className={cn("shrink-0", className)}
      {...props}
    >
      <circle cx="16" cy="16" r="16" fill={ASSET_COLORS.bnb} />
      <path d={BNB_GLYPH} fill="#fff" />
    </svg>
  )
}
