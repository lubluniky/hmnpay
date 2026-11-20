#!/bin/sh
# Builds the web interface (apps/web, a Next.js static export) and puts it where the Go
# binary embeds it from: internal/webui/dist. Run it before "go build" to get a binary with
# the interface; "make build" does both.
#
# Needs Node.js 22 (.nvmrc) and pnpm (the version package.json pins). Only "pnpm install" uses the network, and only when the
# dependencies are not installed yet; the build itself fetches nothing (the fonts are in
# apps/web/src/fonts).
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out="$root/apps/web/out"
dist="$root/internal/webui/dist"
cd "$root"

pnpm install --frozen-lockfile
pnpm --filter web build

test -f "$out/index.html" || { echo "build-ui: apps/web/out has no index.html; the build did not produce an export" >&2; exit 1; }

# Make internal/webui/dist an exact copy of the export: files of an older build must not stay
# behind (they would be embedded too). The placeholder that keeps the directory in git stays.
if command -v rsync >/dev/null 2>&1; then
	rsync -a --delete --exclude=/.placeholder "$out/" "$dist/"
else
	find "$dist" -mindepth 1 ! -name .placeholder -delete
	cp -R "$out/." "$dist/"
fi

echo "build-ui: internal/webui/dist now holds the interface ($(find "$dist" -type f ! -name .placeholder | wc -l | tr -d ' ') files)"
