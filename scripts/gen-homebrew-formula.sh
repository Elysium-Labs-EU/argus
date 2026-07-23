#!/bin/sh
# Renders Formula/argus.rb for the Elysium-Labs-EU/homebrew-tap repo from a
# release's version tag and sha256sums.txt (both already produced by
# release.yml). Output goes to stdout; the tap repo has no automated push
# access set up yet, so copy the printed formula into homebrew-tap by hand.
#
# Usage:
#   gen-homebrew-formula.sh <version> <sha256sums-file>
#
# <version> is the release tag (e.g. v1.2.3 or 1.2.3 — a leading "v" is
# stripped for the formula's `version` field, but the URL always uses the
# vX.Y.Z tag form release.yml publishes under).
# <sha256sums-file> is a path to sha256sums.txt, or "-" to read stdin.
set -eu

REPO="Elysium-Labs-EU/argus"

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

usage() {
  cat <<EOF
Usage: gen-homebrew-formula.sh <version> <sha256sums-file>

  <version>            Release tag, e.g. v1.2.3 (a leading "v" is stripped
                        for the formula's version field).
  <sha256sums-file>     Path to sha256sums.txt from the release, or "-" to
                        read it from stdin.
EOF
}

# sha_for prints the sha256 for the given asset name from the sums file, or
# dies if the asset is missing from it.
sha_for() {
  asset="$1"
  sha="$(awk -v want="$asset" '$2 == want { print $1 }' "$SUMS_FILE")"
  [ -n "$sha" ] || die "no sha256 for $asset in $SUMS_SRC"
  printf '%s' "$sha"
}

[ "$#" -eq 2 ] || { usage >&2; exit 1; }

TAG="$1"
SUMS_SRC="$2"
VERSION="${TAG#v}"

if [ "$SUMS_SRC" = "-" ]; then
  SUMS_FILE="$(mktemp)"
  trap 'rm -f "$SUMS_FILE"' EXIT
  cat >"$SUMS_FILE"
else
  [ -f "$SUMS_SRC" ] || die "sha256sums file not found: $SUMS_SRC"
  SUMS_FILE="$SUMS_SRC"
fi

DARWIN_ARM64_SHA="$(sha_for argus-darwin-arm64)"
DARWIN_AMD64_SHA="$(sha_for argus-darwin-amd64)"
LINUX_ARM64_SHA="$(sha_for argus-linux-arm64)"
LINUX_AMD64_SHA="$(sha_for argus-linux-amd64)"

cat <<FORMULA
class Argus < Formula
  desc "Deterministic supervisor for multi-agent AI coding workflows"
  homepage "https://github.com/${REPO}"
  version "${VERSION}"
  license "Apache-2.0"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/${REPO}/releases/download/${TAG}/argus-darwin-arm64"
      sha256 "${DARWIN_ARM64_SHA}"
    else
      url "https://github.com/${REPO}/releases/download/${TAG}/argus-darwin-amd64"
      sha256 "${DARWIN_AMD64_SHA}"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/${REPO}/releases/download/${TAG}/argus-linux-arm64"
      sha256 "${LINUX_ARM64_SHA}"
    else
      url "https://github.com/${REPO}/releases/download/${TAG}/argus-linux-amd64"
      sha256 "${LINUX_AMD64_SHA}"
    end
  end

  def install
    bin.install Dir["argus-*"].first => "argus"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/argus system version")
  end
end
FORMULA
