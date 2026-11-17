# SPDX-License-Identifier: Apache-2.0
#
# Runs the capture proxy plus one pinned buf CLI version and executes the
# scenarios in scenarios/. Build arg BUF_VERSION selects the CLI.
FROM golang:1.27 AS proxy
WORKDIR /src
COPY go.mod ./
COPY test/capture ./test/capture
RUN CGO_ENABLED=0 go build -o /capture ./test/capture/cmd/capture

FROM debian:bookworm-slim
ARG BUF_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl openssl \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "https://github.com/bufbuild/buf/releases/download/v${BUF_VERSION}/buf-Linux-$(uname -m)" \
      -o /usr/local/bin/buf && chmod +x /usr/local/bin/buf && buf --version
COPY --from=proxy /capture /usr/local/bin/capture
COPY test/capture/scenarios /scenarios
ENV BUF_VERSION=${BUF_VERSION}
ENTRYPOINT ["/scenarios/run.sh"]
