#!/usr/bin/env bash
# schemas/config.schema.json hand-mirrors the key set internal/repoconfig/
# yaml.go's parseYAML/listFieldFor switches actually recognize for
# .argus/config.yml. A key added to one and not the other drifts silently —
# the same class of unnoticed drift that broke eos/themis's pubkey copies
# after a rotation touched only one of two hand-duplicated sources — so this
# gate diffs the two key sets on every run instead of trusting them to stay
# in sync by hand.
set -euo pipefail

TARGET="${1:-.}"
GO_FILE="$TARGET/internal/repoconfig/yaml.go"
SCHEMA_FILE="$TARGET/schemas/config.schema.json"

for f in "$GO_FILE" "$SCHEMA_FILE"; do
  [ -f "$f" ] || { echo "check-schema-sync: $f not found" >&2; exit 1; }
done

go_keys="$(grep -oE 'case "[a-z_]+":' "$GO_FILE" | sed -E 's/case "([a-z_]+)":/\1/' | sort -u)"
schema_keys="$(python3 -c '
import json, sys
with open(sys.argv[1]) as f:
    doc = json.load(f)
for key in sorted(doc.get("properties", {})):
    print(key)
' "$SCHEMA_FILE")"

if [ -z "$go_keys" ]; then
  echo "check-schema-sync: no config keys found in $GO_FILE" >&2
  exit 1
fi
if [ -z "$schema_keys" ]; then
  echo "check-schema-sync: no properties found in $SCHEMA_FILE" >&2
  exit 1
fi

if [ "$go_keys" != "$schema_keys" ]; then
  echo "check-schema-sync: config keys differ between $GO_FILE and $SCHEMA_FILE:" >&2
  diff <(printf '%s\n' "$go_keys") <(printf '%s\n' "$schema_keys") >&2 || true
  exit 1
fi

echo "check-schema-sync OK: config keys match in $GO_FILE and $SCHEMA_FILE."
