# syntax=docker/dockerfile:1
# SPDX-License-Identifier: Apache-2.0
#
# Multi-arch image for protodex. The Go build stage always runs on the build
# host's native platform (BUILDPLATFORM) and cross-compiles for TARGETOS /
# TARGETARCH, so `docker buildx build --platform linux/amd64,linux/arm64`
# needs no QEMU emulation for the compile step. The runtime stage is
# distroless static (no shell, no libc) running as the nonroot user.
#
# Base images are pinned by digest; dependabot (docker ecosystem) bumps them.

ARG GO_IMAGE=golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/marijnkoesen/protodex/internal/version.Version=${VERSION}" \
      -o /out/protodex ./cmd/protodex \
 && mkdir -p /out/data

FROM ${RUNTIME_IMAGE}
ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=unknown
LABEL org.opencontainers.image.title="protodex" \
      org.opencontainers.image.description="Self-hostable Protobuf schema registry compatible with the buf CLI" \
      org.opencontainers.image.source="https://github.com/marijnkoesen/protodex" \
      org.opencontainers.image.url="https://github.com/marijnkoesen/protodex" \
      org.opencontainers.image.documentation="https://github.com/marijnkoesen/protodex/tree/main/deploy" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="protodex contributors" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}"
COPY --from=build /out/protodex /protodex
# Data directory for the (upcoming) SQLite database. Owned by nonroot so a
# fresh named volume mounted here inherits writable ownership; the rest of the
# filesystem can be mounted read-only.
COPY --from=build --chown=65532:65532 /out/data /var/lib/protodex
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/protodex"]
CMD ["serve"]
