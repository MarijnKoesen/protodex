#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Fails if a tracked source file lacks an SPDX-License-Identifier header
# in its first 5 lines.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

missing=0
while IFS= read -r -d '' f; do
  [[ -f "$f" ]] || continue
  # Vendored upstream protos and generated code keep their own headers.
  case "$f" in proto/buf/*|gen/*) continue ;; esac
  if ! head -n 5 "$f" | grep -q 'SPDX-License-Identifier: Apache-2.0'; then
    echo "missing SPDX header: $f" >&2
    missing=1
  fi
done < <(git ls-files -z --cached --others --exclude-standard -- \
  '*.go' '*.proto' '*.sh' 'Makefile' 'Dockerfile' '**/Dockerfile')

if [[ $missing -ne 0 ]]; then
  echo "Add '// SPDX-License-Identifier: Apache-2.0' (or '#' comment) to the files above." >&2
  exit 1
fi
echo "license headers OK"
