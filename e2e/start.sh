#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Starts a throwaway protodex (binary from `make build`) for the e2e tests.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
bin=${PROTODEX_BIN:-$here/../bin/protodex}
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
export PROTODEX_DB_PATH="$dir/db"
export PROTODEX_BOOTSTRAP_TOKEN=$E2E_TOKEN
export PROTODEX_BOOTSTRAP_ORG=acme
"$bin" bootstrap >/dev/null
"$bin" serve -addr 127.0.0.1:18080
