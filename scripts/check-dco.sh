#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Usage: check-dco.sh <base-ref> [<head-ref>]
# Fails if any non-merge commit in base..head lacks a Signed-off-by trailer.
set -euo pipefail

base="${1:?usage: check-dco.sh <base-ref> [<head-ref>]}"
head="${2:-HEAD}"

bad=0
for c in $(git rev-list --no-merges "${base}..${head}"); do
  if ! git log -1 --format='%(trailers:key=Signed-off-by,valueonly)' "$c" | grep -q .; then
    echo "commit $(git log -1 --format='%h %s' "$c") is missing 'Signed-off-by:' (use git commit -s)" >&2
    bad=1
  fi
done

if [[ $bad -ne 0 ]]; then
  echo "See CONTRIBUTING.md#developer-certificate-of-origin-dco" >&2
  exit 1
fi
echo "DCO OK"
