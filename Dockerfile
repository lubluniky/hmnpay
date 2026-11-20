# syntax=docker/dockerfile:1
#
# hmnpay server image: one static binary — the API and the web interface —
# on distroless, running as non-root, state in the /data volume.
#
#   docker build --build-arg VERSION=1.0.0 --build-arg COMMIT=$(git rev-parse --short HEAD) -t hmnpay .
#
# Multi-arch: docker buildx build --platform linux/amd64,linux/arm64 …
# (both build stages run on the build platform: the interface is plain
# files, and the Go toolchain cross-compiles; no emulation is needed).

# Base images are pinned by multi-arch index digest (resolved from the
# registries on 2026-11-18). To update: re-resolve the tag's digest, e.g.
#   docker buildx imagetools inspect golang:1.27-bookworm
# and replace both the tag comment and the digest.

# ---- 1. The web interface: a static export of apps/web ----------------------
# Node.js 22, the version .nvmrc and package.json name.
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim@sha256:e9f565d32859f0cbfb52125eafe8be39390f24dac302dc3cdf566e8ba3ab21ee AS web
WORKDIR /src
ENV NEXT_TELEMETRY_DISABLED=1 CI=1 COREPACK_ENABLE_DOWNLOAD_PROMPT=0

# Dependencies first, for layer caching; exactly what the lockfile says.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY apps/web/package.json apps/web/
# pnpm comes through corepack (part of the Node image) in exactly the
# version package.json names in "packageManager", and corepack checks the
# download against the SHA-512 written there: a different pnpm does not
# install.
RUN corepack enable pnpm && corepack install && pnpm --version
RUN pnpm install --frozen-lockfile --filter web...

COPY apps/web apps/web
# Writes apps/web/out: HTML, JS, CSS and fonts, nothing that needs Node to serve.
# The build itself needs no network (the fonts are in the source tree), and
# running it without one keeps it that way: a dependency that starts fetching
# at build time fails here.
RUN --network=none COREPACK_ENABLE_NETWORK=0 pnpm --filter web build

# ---- 2. The binary, with the interface embedded -----------------------------
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
WORKDIR /src

# Dependencies first, for layer caching.
COPY go.mod go.sum ./
RUN go mod download

# LICENSE, ADDITIONAL_TERMS.md and THIRD_PARTY_NOTICES.md are embedded in
# the binary (notices.go) and served at /license.txt, /additional-terms.txt
# and /third-party-notices.txt.
COPY *.go LICENSE ADDITIONAL_TERMS.md THIRD_PARTY_NOTICES.md ./
COPY core ./core
COPY internal ./internal
COPY cmd ./cmd
# internal/webui embeds this directory (go:embed). The build context brings
# only its placeholder; the export of stage 1 goes next to it.
COPY --from=web /src/apps/web/out/ ./internal/webui/dist/
RUN test -f internal/webui/dist/index.html

# Set by BuildKit to the platform being built. They must be declared
# WITHOUT defaults: a default here overrides BuildKit's value, which once
# put an amd64 binary into an arm64 image. With the legacy builder they are
# empty and Go builds for the container's own platform.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
# Reproducible: no cgo, no paths, no VCS stamping, empty build id, fixed
# ldflags. The same source and arguments give the same binary.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -buildid= \
        -X github.com/lubluniky/hmnpay.Version=${VERSION} \
        -X github.com/lubluniky/hmnpay.Commit=${COMMIT} \
        -X github.com/lubluniky/hmnpay/internal/webhook.Version=${VERSION}" \
      -o /out/hmnpay ./cmd/hmnpay \
 && mkdir -p /out/data /out/doc \
 && cp LICENSE ADDITIONAL_TERMS.md THIRD_PARTY_NOTICES.md /out/doc/

# distroless/static: CA certificates and tzdata, no shell, no package
# manager. "nonroot" is uid/gid 65532.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/hmnpay /usr/local/bin/hmnpay
# The licence, its additional terms and the third-party notices, as files
# beside the binary (it also serves them, see above).
COPY --from=build /out/doc/ /usr/share/doc/hmnpay/
LABEL org.opencontainers.image.title="hmnpay" \
      org.opencontainers.image.description="Self-hosted, non-custodial USDT payment gateway for BNB Smart Chain" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.source="https://github.com/lubluniky/hmnpay"
COPY --from=build --chown=65532:65532 /out/data /data

ENV HMNPAY_LISTEN=:8080 \
    HMNPAY_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
USER 65532:65532

# The image has no shell or curl: the binary checks itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/hmnpay", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/hmnpay"]
CMD ["serve"]
