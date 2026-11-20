# distortion-waves (vendored)

"Distortion waves" by Jakub Antalik — the WebGL 2 wave transition shown at
https://libraries.dev/waves. MIT licensed; see `LICENSE` (kept as required).

| | |
| --- | --- |
| Upstream | https://github.com/Jakubantalik/Libraries.dev |
| Directory | `packages/distortion-waves` (`src/*`, `LICENSE`, `README.md`) |
| Package | `distortion-waves` 0.1.0 (peer deps: react, react-dom >= 18) |
| Commit | `371b324a253b8ee71b9ce1bc8a1ecc5a59e1a16a` |
| Vendored | 2026-11-18 |

Why it is vendored: the site says `npm install distortion-waves`, but that package is not
published on npm. The npm package `distortion-wave` (singular) is an unrelated project — do not
install it.

## Files

Git blob hashes at the pinned commit, to check a file against upstream with `git hash-object`:

| File | Blob at upstream commit | State here |
| --- | --- | --- |
| `DistortionWave.tsx` | `167e3721254e87a914df98d918867fc421d7bea8` | identical (not imported by the app) |
| `capture.ts` | `ea0561050081623265770345be3ed046d80df177` | **modified**, see below |
| `controller.ts` | `7a248a4b6c17856fc7cca057fe75420cbac1b731` | identical |
| `index.ts` | `038d71fcf43650502fb2e62d7277acbf3b6d6bfd` | identical (not imported by the app) |
| `model.ts` | `c6276215422cffb9aeb0cf45c310bfb42e811384` | identical |
| `renderer.ts` | `414b2ccbe3a2f7d76d117dbc6bba6539678ac2d9` | identical |
| `LICENSE` | `0e9405a2ee75ce720c665c04568fedda3c07906a` | identical |
| `UPSTREAM_README.md` | `6165a7cbf478f69dfaa8c3ba49a0909434074b61` | upstream `README.md`, renamed |

## Local modifications

One, in `capture.ts`, marked with `hmnpay:` comments (`STYLE_OF_WIDTH`, `styledWidth`, and the two
places that call it).

The capture writes a computed style only where it differs from a bare element's. A border's
width computes to `0px` while its style is `none`, so a bare element reports `0px` — and so does
an element reset with `border: 0 solid`, which Tailwind's preflight applies to every element.
The width was therefore left out as a default while the style (`solid`) was written, and the
picture drew a 3px (`medium`) border around every element, which also reflowed the text. The
fix: a border, outline or column-rule width is always written when its style is.

Nothing else is changed. The directory is excluded from ESLint (`eslint.config.mjs`) rather
than reformatted, so it stays diffable against upstream.

## How the app uses it

Only through `src/components/hmn/wave-transition.tsx`, which loads `controller.ts` (and with it
`model.ts`, `renderer.ts`, `capture.ts`) with a dynamic `import()`. It never enters the server
bundle or a route's first-load JavaScript. The React component (`DistortionWave.tsx`) is kept
for reference but unused: the app drives `WaveController` directly so the wave can be attached
to markup that is already on screen.

## What the code does (reviewed 2026-11-18)

Read in full before use. It is a rendering library and behaves like one:

- `model.ts` — pure math: timeline, palettes, particles.
- `renderer.ts` — WebGL 2 shaders and draw calls on one canvas.
- `controller.ts` — lifecycle: a canvas inside the element, MutationObserver / ResizeObserver /
  pointer and focus listeners on the element, `requestAnimationFrame`. Sets `inert`,
  `aria-hidden`, `opacity` and `pointer-events` on the two view wrappers.
- `capture.ts` — draws the element by serialising its DOM with computed styles into an SVG
  `<foreignObject>` data URL, loading that as an image and drawing it to a 2D canvas.

No `eval` / `new Function`, no XHR / WebSocket / `sendBeacon` / EventSource, no cookies or
storage, no script injection, no telemetry, no hard-coded URLs other than XML namespaces.

Worth knowing:

- `capture.ts` calls `fetch()` (GET, no body, 4 s timeout) to re-read resources the page itself
  already references, so they can be embedded in the SVG as data URLs: `@font-face` files,
  `<img>` sources, CSS `url()` backgrounds, SVG `href`s, and the text of a cross-origin
  stylesheet whose rules the browser will not expose. Nothing is sent anywhere. In hmnpay this
  is the two self-hosted Geist `.woff2` files, served from the HTTP cache. A strict CSP needs
  `connect-src 'self'` and `img-src data:`.
  The one resource on a page that somebody else chooses is the merchant's logo, which may be a
  URL on another host. `MerchantAvatar` marks such an image `data-dw-skip`, so the capture
  leaves it out and never fetches it; and the Content-Security-Policy the Go server sends
  (`internal/webui`: `connect-src 'self'`) would refuse the fetch regardless. An end-to-end test
  (`e2e/checkout.spec.ts`) runs a wave with a remote logo and checks that no script request
  goes to its host.
- `capture.ts` appends one hidden, 0×0, `src`-less `<iframe>` to `<body>` and writes a static
  blank document into it, to read the browser's default styles. It stays for the page's lifetime.
  It never loads anything: the policy's `frame-src 'none'` forbids a frame with a source, and a
  `src`-less frame makes no request.
- It sniffs the user agent for WebKit / Firefox rendering quirks, and logs one `console.warn` if
  a picture cannot be taken (the wave then runs without its lens).
