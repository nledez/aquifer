# syntax=docker/dockerfile:1.9

# Aquifer ships as a single static binary on a distroless runtime.
#
# Alpine appears nowhere, in no stage: musl changes the behaviour of the
# resolver and of getaddrinfo in ways that are a nuisance to debug, and there
# is nothing to gain here when the binary is static and the runtime image is
# already 2 MiB.

# --- build ---------------------------------------------------------------------
# Both base images carry a literal tag and a digest. The tag is what a human
# reads; the digest is what actually gets pulled, so a build is reproducible
# even after the tag moves. Dependabot parses these lines and bumps tag and
# digest together — it skips a FROM whose tag comes from a build arg, which is
# why the Go version is spelled out here rather than kept in an ARG.
#
# The Go version must match go.mod and .tool-versions.
FROM --platform=$BUILDPLATFORM golang:1.26.5-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599 AS builder

WORKDIR /src

# Dependencies first, so that a source-only change reuses this layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# TARGETOS and TARGETARCH come from buildx; on a single-platform build they
# default to the host's.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

# -trimpath strips the build path and -buildid= empties the build identifier,
# so that the same source produces the same bytes on any machine.
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -buildid= -X github.com/nledez/aquifer/internal/cli.version=${VERSION}" \
      -o /out/aquifer ./cmd/aquifer

# The runtime has no shell, so the cache directory has to be created here with
# the right ownership.
RUN mkdir -p /out/cache && chown 65532:65532 /out/cache

# --- runtime -------------------------------------------------------------------
# :nonroot is a floating tag — it moves whenever distroless rebuilds. The
# digest is what pins the runtime; Dependabot refreshes it when the tag moves.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a

ARG VERSION=dev
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="aquifer" \
      org.opencontainers.image.description="Distributed APT mirror with a content-addressed cache" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/nledez/aquifer" \
      org.opencontainers.image.licenses="BSD-3-Clause" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian12:nonroot" \
      org.opencontainers.image.base.digest="sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35"

COPY --from=builder /out/aquifer /aquifer
COPY --from=builder --chown=65532:65532 /out/cache /var/cache/aquifer
COPY LICENSE /LICENSE
COPY THIRD-PARTY-NOTICES.txt /THIRD-PARTY-NOTICES.txt

# 65532 is the nonroot user the base image provides.
USER 65532:65532

EXPOSE 8080 8081

ENTRYPOINT ["/aquifer"]
CMD ["serve"]

# Distroless has no shell, which rules out both curl and the shell form of
# HEALTHCHECK: that form wraps the command in /bin/sh -c. The exec form runs
# the binary directly, and "aquifer ping" needs no arguments because it
# resolves the admin address on its own.
#
# The generous start period leaves time for the pinned patterns to prefetch.
HEALTHCHECK --interval=15s --timeout=3s --start-period=60s --retries=3 \
    CMD ["/aquifer", "ping"]
