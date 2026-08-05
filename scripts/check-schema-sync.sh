#!/usr/bin/env bash
# schemas/config.schema.json hand-mirrors the key set internal/repoconfig/
# yaml.go's parseYAML/listFieldFor/assignPhaseKey switches recognize, plus the
# deprecated aliases deprecatedKeyAliases still accepts. Three hand-duplicated
# sources drift silently if only one is updated, so this gate diffs the key
# sets on every run instead of trusting them to stay in sync by hand. Dotted
# phase.<name>.<subkey> keys live under the schema's patternProperties (a
# literal property per phase name would be 10 near-identical blocks), so
# schema_keys below also pulls the trailing subkey out of each pattern. The
# ship:/rework:/review: operation regions are themselves nested objects, not
# top-level schema properties, so schema_keys also pulls each region's own
# subkeys in directly — a canonical-only key with no flat top-level alias
# (e.g. rework.budget, rework.max_rounds) has no other way to appear here.
set -euo pipefail

TARGET="${1:-.}"
GO_FILE="$TARGET/internal/repoconfig/yaml.go"
SCHEMA_FILE="$TARGET/schemas/config.schema.json"

for f in "$GO_FILE" "$SCHEMA_FILE"; do
  [ -f "$f" ] || { echo "check-schema-sync: $f not found" >&2; exit 1; }
done

go_keys="$(grep -oE '(case )?"[a-z_]+":' "$GO_FILE" | sed -E 's/^(case )?"([a-z_]+)":$/\2/' | sort -u)"
schema_keys="$(python3 -c '
import json, sys
with open(sys.argv[1]) as f:
    doc = json.load(f)
top_level = doc.get("properties", {})
keys = set(top_level)
# ship:/rework:/review: are nested objects: their own subkeys never appear
# as top-level schema properties, only inside the properties nested under
# each region.
for region in ("ship", "rework", "review"):
    keys.update(top_level.get(region, {}).get("properties", {}))
# patternProperties keys are regexes, not literal names — a dotted phase
# policy key (phase.<name>.skip/deny) always ends in \.<subkey>$, so the
# trailing segment is the same literal subkey yaml.go switches on.
for pattern in doc.get("patternProperties", {}):
    keys.add(pattern.rstrip("$").rsplit("\\.", 1)[-1])
for key in sorted(keys):
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
