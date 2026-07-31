#!/usr/bin/env bash
# any and the empty interface literal compile to the identical type, but
# letting both spellings coexist means a `grep`-for-`any` audit of the
# codebase can never be trusted to be complete. This gate keeps the repo on
# one spelling so that search stays exhaustive.
set -euo pipefail

TARGET="${1:-.}"

hits="$(grep -rn 'interface{}' --include='*.go' "$TARGET" | grep -v '/vendor/' || true)"
if [ -n "$hits" ]; then
  echo "use 'any' instead of the empty interface literal:" >&2
  echo "$hits" >&2
  exit 1
fi
echo "any-not-interface gate OK: no empty interface literal found."
