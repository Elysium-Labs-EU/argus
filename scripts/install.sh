#!/usr/bin/env bash
# Downloads and installs a released argus binary for the current platform.
#
# Usage:
#   curl -sSfL https://raw.githubusercontent.com/Elysium-Labs-EU/argus/main/scripts/install.sh | sh
#
# Env vars:
#   ARGUS_VERSION      Release tag to install (default: latest).
#   ARGUS_INSTALL_DIR  Directory to install the binary into. Overrides the
#                       automatic selection below.
#
# Install directory, in priority order (no sudo unless step 3 is reached):
#   1. $ARGUS_INSTALL_DIR, if set.
#   2. ~/.local/bin, if it's already on PATH.
#   3. /usr/local/bin, via sudo if it isn't writable.
set -euo pipefail

REPO="Elysium-Labs-EU/argus"

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

usage() {
  cat <<EOF
Usage: install.sh [--print-install-dir]

  --print-install-dir  Print the directory the script would install into
                        (honoring ARGUS_INSTALL_DIR and PATH) and exit,
                        without downloading anything.
EOF
}

detect_os() {
  case "$(uname -s)" in
    Linux) echo linux ;;
    Darwin) echo darwin ;;
    *) die "unsupported OS: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    arm64 | aarch64) echo arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

path_contains() {
  case ":${PATH}:" in
    *":$1:"*) return 0 ;;
    *) return 1 ;;
  esac
}

# select_install_dir prints the directory argus should be installed into,
# without touching the filesystem, so it can be exercised in tests.
select_install_dir() {
  if [ -n "${ARGUS_INSTALL_DIR:-}" ]; then
    printf '%s\n' "$ARGUS_INSTALL_DIR"
  elif path_contains "${HOME}/.local/bin"; then
    printf '%s\n' "${HOME}/.local/bin"
  else
    printf '%s\n' "/usr/local/bin"
  fi
}

PRINT_INSTALL_DIR_ONLY=false
for arg in "$@"; do
  case "$arg" in
    --print-install-dir) PRINT_INSTALL_DIR_ONLY=true ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) die "unknown argument: $arg (see --help)" ;;
  esac
done

if [ "$PRINT_INSTALL_DIR_ONLY" = true ]; then
  select_install_dir
  exit 0
fi

PLATFORM="$(detect_os)-$(detect_arch)"
VERSION="${ARGUS_VERSION:-latest}"

if [ "$VERSION" = "latest" ]; then
  API_URL="https://api.github.com/repos/${REPO}/releases/latest"
else
  API_URL="https://api.github.com/repos/${REPO}/releases/tags/${VERSION}"
fi

log "Resolving ${VERSION} release for ${PLATFORM}..."
RELEASE_JSON="$(curl -sSfL "$API_URL")" || die "fetching release metadata from ${API_URL}"
TAG="$(printf '%s' "$RELEASE_JSON" | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
[ -n "$TAG" ] || die "could not determine release tag from ${API_URL}"

ASSET="argus-${PLATFORM}"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
CHECKSUMS_URL="https://github.com/${REPO}/releases/download/${TAG}/sha256sums.txt"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

log "Downloading ${ASSET} (${TAG})..."
curl -sSfL -o "${TMP_DIR}/${ASSET}" "$DOWNLOAD_URL" || die "downloading ${DOWNLOAD_URL}"
curl -sSfL -o "${TMP_DIR}/sha256sums.txt" "$CHECKSUMS_URL" || die "downloading ${CHECKSUMS_URL}"

log "Verifying checksum..."
(
  cd "$TMP_DIR"
  if command -v sha256sum >/dev/null 2>&1; then
    grep " ${ASSET}\$" sha256sums.txt | sha256sum -c -
  elif command -v shasum >/dev/null 2>&1; then
    grep " ${ASSET}\$" sha256sums.txt | shasum -a 256 -c -
  else
    die "neither sha256sum nor shasum is available to verify the download"
  fi
) || die "checksum verification failed for ${ASSET}"

chmod +x "${TMP_DIR}/${ASSET}"

INSTALL_DIR="$(select_install_dir)"
mkdir -p "$INSTALL_DIR" 2>/dev/null || true

if [ -w "$INSTALL_DIR" ]; then
  install -m 0755 "${TMP_DIR}/${ASSET}" "${INSTALL_DIR}/argus"
elif command -v sudo >/dev/null 2>&1; then
  log "${INSTALL_DIR} is not writable, using sudo..."
  sudo install -m 0755 "${TMP_DIR}/${ASSET}" "${INSTALL_DIR}/argus"
else
  die "${INSTALL_DIR} is not writable and sudo is unavailable; set ARGUS_INSTALL_DIR to a writable directory on PATH"
fi

log "installed argus ${TAG} to ${INSTALL_DIR}/argus"
if ! path_contains "$INSTALL_DIR"; then
  log "warning: ${INSTALL_DIR} is not on PATH"
fi
