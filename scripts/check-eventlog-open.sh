#!/usr/bin/env bash
# eventlog.Open resolves ~/.argus/runs from whatever home string it's given, so a
# test calling it directly (instead of eventlog.OpenForTest, which is pinned to
# t.TempDir()) can silently write into the developer's real run-log corpus. Grep
# is deterministic and has zero false negatives for this one failure mode.
set -euo pipefail

TARGET="${1:-.}"

hits="$(grep -rn 'eventlog\.Open(' --include='*_test.go' "$TARGET" || true)"
if [ -n "$hits" ]; then
  echo "tests must use eventlog.OpenForTest, not eventlog.Open directly:" >&2
  echo "$hits" >&2
  exit 1
fi
echo "eventlog-open gate OK: no test calls eventlog.Open directly."
