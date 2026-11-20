/**
 * Geist Sans and Geist Mono, bundled with the source so that the build fetches nothing.
 *
 * The files next to this one are the subsets of the two variable fonts as Google Fonts serves
 * them (the same bytes `next/font/google` used to download at build time); they are licensed
 * under the SIL Open Font License 1.1, see OFL.txt here. Each subset is its own face with its
 * own unicode-range, so a browser downloads only the scripts a page really shows. Only the two
 * Latin files are preloaded.
 *
 * next/font needs every call spelled out with literal options, hence the repetition. The
 * families are put together into --font-geist-sans and --font-geist-mono in globals.css, Latin
 * first: that is the order a browser looks through faces of one family too (last declared
 * first), and it matters only for the few code points more than one subset carries.
 */
import localFont from "next/font/local"

/* --------------------------------- Geist Sans --------------------------------- */

const sansLatin = localFont({
  src: "./geist-latin.woff2",
  weight: "100 900",
  style: "normal",
  display: "swap",
  variable: "--font-gs-latin",
  adjustFontFallback: false,
  preload: true,
  declarations: [
    {
      prop: "unicode-range",
      value:
        "U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD",
    },
  ],
})

const sansLatinExt = localFont({
  src: "./geist-latin-ext.woff2",
  weight: "100 900",
  style: "normal",
  display: "swap",
  variable: "--font-gs-latin-ext",
  adjustFontFallback: false,
  preload: false,
  declarations: [
    {
      prop: "unicode-range",
      value:
        "U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C4, U+2113, U+2C60-2C7F, U+A720-A7FF",
    },
  ],
})

const sansVietnamese = localFont({
  src: "./geist-vietnamese.woff2",
  weight: "100 900",
  style: "normal",
  display: "swap",
  variable: "--font-gs-vietnamese",
  adjustFontFallback: false,
  preload: false,
  declarations: [
    {
      prop: "unicode-range",
      value:
        "U+0102-0103, U+0110-0111, U+0128-0129, U+0168-0169, U+01A0-01A1, U+01AF-01B0, U+0300-0301, U+0303-0304, U+0308-0309, U+0323, U+0329, U+1EA0-1EF9, U+20AB",
    },
  ],
})

const sansCyrillic = localFont({
  src: "./geist-cyrillic.woff2",
  weight: "100 900",
  style: "normal",
  display: "swap",
  variable: "--font-gs-cyrillic",
  adjustFontFallback: false,
  preload: false,
  declarations: [{ prop: "unicode-range", value: "U+0301, U+0400-045F, U+0490-0491, U+04B0-04B1, U+2116" }],
})

const sansCyrillicExt = localFont({
  src: "./geist-cyrillic-ext.woff2",
  weight: "100 900",
  style: "normal",
  display: "swap",
  variable: "--font-gs-cyrillic-ext",
  adjustFontFallback: false,
  preload: false,
  declarations: [{ prop: "unicode-range", value: "U+0460-052F, U+1C80-1C8A, U+20B4, U+2DE0-2DFF, U+A640-A69F, U+FE2E-FE2F" }],
})

/* --------------------------------- Geist Mono --------------------------------- */

const monoLatin = localFont({
  src: "./geist-mono-latin.woff2",
  weight: "100 900",
  style: "normal",
  display: "swap",
  variable: "--font-gm-latin",
  adjustFontFallback: false,
  preload: true,
  declarations: [
    {
      prop: "unicode-range",
      value:
        "U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD",
    },
  ],
})

const monoLatinExt = localFont({
  src: "./geist-mono-latin-ext.woff2",
  weight: "100 900",
  style: "normal",
  display: "swap",
  variable: "--font-gm-latin-ext",
  adjustFontFallback: false,
  preload: false,
  declarations: [
    {
      prop: "unicode-range",
      value:
        "U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C4, U+2113, U+2C60-2C7F, U+A720-A7FF",
    },
  ],
})

const monoVietnamese = localFont({
  src: "./geist-mono-vietnamese.woff2",
  weight: "100 900",
  style: "normal",
  display: "swap",
  variable: "--font-gm-vietnamese",
  adjustFontFallback: false,
  preload: false,
  declarations: [
    {
      prop: "unicode-range",
      value:
        "U+0102-0103, U+0110-0111, U+0128-0129, U+0168-0169, U+01A0-01A1, U+01AF-01B0, U+0300-0301, U+0303-0304, U+0308-0309, U+0323, U+0329, U+1EA0-1EF9, U+20AB",
    },
  ],
})

const monoSymbols = localFont({
  src: "./geist-mono-symbols.woff2",
  weight: "100 900",
  style: "normal",
  display: "swap",
  variable: "--font-gm-symbols",
  adjustFontFallback: false,
  preload: false,
  declarations: [{ prop: "unicode-range", value: "U+2000-2001, U+2004-2008, U+200A, U+23B8-23BD, U+2500-259F" }],
})

const monoCyrillic = localFont({
  src: "./geist-mono-cyrillic.woff2",
  weight: "100 900",
  style: "normal",
  display: "swap",
  variable: "--font-gm-cyrillic",
  adjustFontFallback: false,
  preload: false,
  declarations: [{ prop: "unicode-range", value: "U+0301, U+0400-045F, U+0490-0491, U+04B0-04B1, U+2116" }],
})

const monoCyrillicExt = localFont({
  src: "./geist-mono-cyrillic-ext.woff2",
  weight: "100 900",
  style: "normal",
  display: "swap",
  variable: "--font-gm-cyrillic-ext",
  adjustFontFallback: false,
  preload: false,
  declarations: [{ prop: "unicode-range", value: "U+0460-052F, U+1C80-1C8A, U+20B4, U+2DE0-2DFF, U+A640-A69F, U+FE2E-FE2F" }],
})

/** The classes that put every face's variable on <html>; globals.css builds the two stacks from them. */
export const fontVariables = [
  sansLatin,
  sansLatinExt,
  sansVietnamese,
  sansCyrillic,
  sansCyrillicExt,
  monoLatin,
  monoLatinExt,
  monoVietnamese,
  monoSymbols,
  monoCyrillic,
  monoCyrillicExt,
]
  .map((font) => font.variable)
  .join(" ")
