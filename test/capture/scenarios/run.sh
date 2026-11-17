#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs every scenario against the capture proxy with the buf CLI installed in
# this container. Env:
#   BUF_VERSION   CLI version label (set by the image)
#   UPSTREAM      real registry (default https://buf.build)
#   BUF_TOKEN     optional token for write scenarios (never recorded)
#   PUSH_MODULE   optional <owner>/<repo> you own, enables push/label scenarios
#   OUT_DIR       where recordings go (default /out)
set -uo pipefail

UPSTREAM=${UPSTREAM:-https://buf.build}
OUT_DIR=${OUT_DIR:-/out}
PUBLIC_MODULE=${PUBLIC_MODULE:-bufbuild/protovalidate}
HOST=buf.build   # the CLI derives the registry host from the module name
PORT=443   # the CLI dials the registry host on 443
dir="$OUT_DIR/$BUF_VERSION"
mkdir -p "$dir"

# Self-signed cert for the proxy; the CLI trusts it via SSL_CERT_FILE and
# /etc/hosts points the registry host at the proxy.
tmp=$(mktemp -d)
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=$HOST" \
  -addext "subjectAltName=DNS:$HOST" -keyout "$tmp/k.pem" -out "$tmp/c.pem" 2>/dev/null
cat "$tmp/c.pem" /etc/ssl/certs/ca-certificates.crt > "$tmp/bundle.pem"
export SSL_CERT_FILE="$tmp/bundle.pem"
# The proxy must reach the real upstream, not itself: pin its real IP under
# the upstream name, and point the registry host the CLI uses at the proxy.
upstream_host=${UPSTREAM#*://}
ip=$(getent ahostsv4 "$upstream_host" | awk '{print $1; exit}')
echo "127.0.0.1 $HOST" >> /etc/hosts
export BUF_CACHE_DIR="$tmp/cache"

run() { # run <scenario> <command...>
  local name=$1; shift
  rm -f "$dir/$name.jsonl"
  capture proxy -upstream "$UPSTREAM" -upstream-addr "$ip:443" -listen "127.0.0.1:$PORT" -out "$dir/$name.jsonl" \
    -cli-version "$BUF_VERSION" -scenario "$name" -tls-cert "$tmp/c.pem" -tls-key "$tmp/k.pem" \
    >/dev/null 2>&1 &
  local pid=$!; sleep 1
  echo "== $BUF_VERSION: $name"
  ( cd "$work" && "$@" ) > "$dir/$name.log" 2>&1 || echo "   (exit non-zero; see $dir/$name.log)"
  kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
  if [[ ! -s "$dir/$name.jsonl" ]]; then
    echo "   ERROR: no traffic captured for $name (proxy not reached?)" >&2
    failed=1
  fi
}
failed=0

work=$(mktemp -d)
cat > "$work/buf.yaml" <<YAML
version: v2
modules:
  - path: .
deps:
  - $HOST/$PUBLIC_MODULE
YAML
mkdir -p "$work/proto/demo" && cat > "$work/proto/demo/demo.proto" <<'PROTO'
syntax = "proto3";
package demo;
message Ping { string id = 1; }
PROTO
sed -i 's#path: \.#path: proto#' "$work/buf.yaml"
cat > "$work/buf.gen.yaml" <<'YAML'
version: v2
plugins:
  - remote: buf.build/protocolbuffers/go
    out: gen
YAML

# login with a dummy token only exercises the auth-header path; real
# credentials (if any) are redacted by the proxy.
run login       bash -c 'echo "${BUF_TOKEN:-dummy-token}" | buf registry login '"$HOST"' --username capture --token-stdin'
run dep-update  buf dep update
run build       buf build
run generate    buf generate
run registry-module-info  buf registry module info "$HOST/$PUBLIC_MODULE"
run registry-label-list   buf registry label list "$HOST/$PUBLIC_MODULE"

if [[ -n "${PUSH_MODULE:-}" && -n "${BUF_TOKEN:-}" ]]; then
  run push         bash -c "sed -i 's#- path: proto#- path: proto\n    name: $HOST/$PUSH_MODULE#' buf.yaml && buf push --label capture-$BUF_VERSION"
  run label-list-own buf registry label list "$HOST/$PUSH_MODULE"
else
  echo "PUSH_MODULE/BUF_TOKEN not set: skipping push scenarios"
fi
exit "$failed"
