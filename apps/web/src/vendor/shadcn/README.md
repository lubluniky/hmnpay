# shadcn/ui stylesheet (vendored)

`tailwind.css` is the stylesheet the `shadcn` npm package ships as
`shadcn/tailwind.css`: the keyframes and custom variants the shadcn/ui
components in `src/components/ui` rely on. It is the only thing the
interface used from that package, which is otherwise a code-generator
command line with a large dependency tree; the file is kept here instead so
that the generator is not a dependency of the interface.

| | |
| --- | --- |
| Upstream | https://github.com/shadcn-ui/ui |
| Package | `shadcn` 4.23.0, file `dist/tailwind.css` |
| SHA-256 | `4c371f7a1ff5d219ae2f7ff28bd256b4346fd546fe46fbae22092e57db2f0fae` (unmodified) |
| Vendored | 2026-11-19 |
| Licence | MIT, Copyright (c) 2023 shadcn — see `LICENSE.md` |

The components in `src/components/ui` were generated with the same tool
and then edited for this project; they are derived from shadcn/ui and
carry the same licence (see `THIRD_PARTY_NOTICES.md` at the repository
root).

To update: take `dist/tailwind.css` from a newer `shadcn` package, replace
the file, and record the version and hash here.
