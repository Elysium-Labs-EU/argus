#!/usr/bin/env bash
# cmd/update.go and scripts/install.sh each hand-embed a copy of the release
# signing public key, since a Go const and a POSIX sh var can't share a
# single source of truth without adding a build step. A key rotation that
# updates one copy but not the other breaks signature verification for one
# of the two install paths without any error at rotation time — this gate
# catches that drift before merge.
set -euo pipefail

TARGET="${1:-.}"
GO_FILE="$TARGET/cmd/update.go"
SH_FILE="$TARGET/scripts/install.sh"

for f in "$GO_FILE" "$SH_FILE"; do
  [ -f "$f" ] || { echo "check-pubkey-sync: $f not found" >&2; exit 1; }
done

extract_pem() {
  sed -n '/-----BEGIN PUBLIC KEY-----/,/-----END PUBLIC KEY-----/p' "$1" |
    sed -e 's/^.*\(-----BEGIN PUBLIC KEY-----\)/\1/' -e 's/\(-----END PUBLIC KEY-----\).*$/\1/'
}

go_pem="$(extract_pem "$GO_FILE")"
sh_pem="$(extract_pem "$SH_FILE")"

if [ -z "$go_pem" ]; then
  echo "check-pubkey-sync: no PEM public-key block found in $GO_FILE" >&2
  exit 1
fi
if [ -z "$sh_pem" ]; then
  echo "check-pubkey-sync: no PEM public-key block found in $SH_FILE" >&2
  exit 1
fi

if [ "$go_pem" != "$sh_pem" ]; then
  echo "check-pubkey-sync: release signing pubkey differs between $GO_FILE and $SH_FILE:" >&2
  diff <(printf '%s\n' "$go_pem") <(printf '%s\n' "$sh_pem") >&2 || true
  exit 1
fi

echo "check-pubkey-sync OK: release signing pubkey matches in $GO_FILE and $SH_FILE."
