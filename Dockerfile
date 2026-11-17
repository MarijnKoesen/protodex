# syntax=docker/dockerfile:1
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/marijnkoesen/wireyard/internal/version.Version=${VERSION}" \
      -o /out/wireyard ./cmd/wireyard

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wireyard /wireyard
USER nonroot:nonroot
ENTRYPOINT ["/wireyard"]
