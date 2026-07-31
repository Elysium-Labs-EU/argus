#!/usr/bin/env bash
# A large binary landing in git history (a debug build, a captured
# recording, an accidentally-committed archive) bloats every future clone
# forever -- there's no history rewrite that a shared branch can casually
# absorb. Catching it in the diff, before merge, is the only cheap point to
# stop it.
#
#   CHECK_FILE_SIZE_BASE       base ref (default: origin/main)
#   CHECK_FILE_SIZE_MAX_BYTES  size ceiling per file (default: 1048576 = 1MiB)
set -euo pipefail

TARGET="${1:-.}"
BASE="${CHECK_FILE_SIZE_BASE:-origin/main}"
MAX_BYTES="${CHECK_FILE_SIZE_MAX_BYTES:-1048576}"

cd "$TARGET"

# CI checkouts are often shallow; make the base ref resolvable.
if ! git rev-parse --verify --quiet "$BASE" >/dev/null 2>&1; then
  git fetch --quiet origin "${BASE#origin/}" 2>/dev/null || true
fi
if git rev-parse --verify --quiet "$BASE" >/dev/null 2>&1; then
  DIFF_BASE="$(git merge-base "$BASE" HEAD 2>/dev/null || echo "$BASE")"
else
  echo "check-file-size: base ref '$BASE' unresolvable; nothing to compare against, passing." >&2
  exit 0
fi

# Added or modified files only -- a deletion can't add bytes to history.
changed="$(git diff --name-only --diff-filter=ACMR "$DIFF_BASE" HEAD || true)"
if [ -z "$changed" ]; then
  echo "check-file-size: no added/modified files vs $BASE; nothing to gate."
  exit 0
fi

bad=()
while IFS= read -r f; do
  [ -f "$f" ] || continue
  # A Git-LFS pointer file stands in for the real blob in the working tree;
  # the guard exists to stop large content from landing directly in git
  # history, so a path already routed through the LFS filter is exempt.
  if git check-attr filter -- "$f" | grep -q 'filter: lfs$'; then
    continue
  fi
  size="$(wc -c <"$f" | tr -d ' ')"
  if [ "$size" -gt "$MAX_BYTES" ]; then
    bad+=("$f ($size bytes)")
  fi
done <<<"$changed"

if [ "${#bad[@]}" -gt 0 ]; then
  echo "check-file-size: file(s) exceed $MAX_BYTES bytes and are not Git-LFS tracked:" >&2
  printf '  %s\n' "${bad[@]}" >&2
  echo "Track large/binary assets with Git LFS, or keep them out of version control." >&2
  exit 1
fi
echo "check-file-size gate OK: no oversized non-LFS files vs $BASE."
