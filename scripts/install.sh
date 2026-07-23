#!/bin/sh
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
set -eu

REPO="Elysium-Labs-EU/argus"

# ECDSA P-256 public key (SubjectPublicKeyInfo, PEM) used to verify the
# detached signature over each release's sha256sums.txt. Keep in sync with
# releaseSigningPublicKeyPEM in cmd/update.go — `make check-pubkey-sync`
# fails CI if they diverge. The matching private key lives only as the
# RELEASE_SIGNING_KEY secret in GitHub Actions.
RELEASE_SIGNING_PUBKEY='-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEKixhYiZA8bWtyh5sBs0hLdOhVXj3
zHHnA3f89l/hPJOQljhWQPOWUcVWnxpVkiIfMPfvxuH4CxnRfFL2azqr8A==
-----END PUBLIC KEY-----'

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

usage() {
  cat <<EOF
Usage: install.sh [--print-install-dir]
       install.sh --strip-quarantine <path>
       install.sh --verify-signature <pubkey-file> <sig-file> <data-file>
       install.sh --verify-signature-flow <pubkey-file> <sig-file> <checksums-file>

  --print-install-dir  Print the directory the script would install into
                        (honoring ARGUS_INSTALL_DIR and PATH) and exit,
                        without downloading anything.
  --strip-quarantine <path>
                        Remove the macOS Gatekeeper quarantine attribute
                        from <path> and exit. No-op on non-Darwin. Useful
                        after manually downloading a release asset instead
                        of using this script.
  --verify-signature <pubkey-file> <sig-file> <data-file>
                        Verify an ECDSA signature with openssl and exit with
                        its result (0 = valid). Exposes the same check the
                        installer runs on sha256sums.txt, for testing.
  --verify-signature-flow <pubkey-file> <sig-file> <checksums-file>
                        Run the same soft-fail (missing sig-file)/hard-fail
                        (invalid sig-file)/pass (valid sig-file) decision the
                        installer runs on a downloaded release, and exit with
                        its result. <sig-file> may point to a missing or
                        empty file to exercise the soft-fail path. For
                        testing.
  --fetch-signature <url> <dest-file>
                        Download <url> to <dest-file>, exiting 0 for a 200 or
                        a 404 (dest-file is left missing on 404) and 1 for
                        any other outcome (non-2xx status, or the request
                        failing outright). Exposes the same
                        signed-vs-unsigned-vs-broken-download classification
                        the installer runs on sha256sums.txt.sig, for
                        testing.
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

# strip_quarantine removes the macOS Gatekeeper "com.apple.quarantine" xattr
# from a downloaded binary. Without this, a binary carrying that attribute
# (set by curl/browsers on Darwin, or a manual re-download) is killed
# (SIGKILL) by Gatekeeper on first run, since the project has no signing key
# provisioned yet (issue #47). No-op on non-Darwin, and tolerant of the
# attribute already being absent.
strip_quarantine() {
  if [ "$(uname -s)" = "Darwin" ]; then
    xattr -d com.apple.quarantine "$1" 2>/dev/null || true
  fi
}

# resign_darwin_binary re-applies an ad-hoc codesign to a binary at its final
# installed path. Go's linker already ad-hoc-signs arm64 binaries at build
# time, but the kernel's per-vnode code-signature cache can go stale when a
# Mach-O's bytes land on a path that reuses an inode (e.g. a prior install at
# the same path) — see issue #66 for the reproduced SIGKILL/CODESIGNING
# failure and cited golang/go#42684, golang/go#63997. This script's
# mktemp+install pattern already avoids the specific in-place-overwrite
# trigger, but re-signing after final placement is cheap, local, and closes
# the gap regardless. No-op on non-Darwin or without codesign on PATH.
resign_darwin_binary() {
  if [ "$(uname -s)" = "Darwin" ] && command -v codesign >/dev/null 2>&1; then
    if [ "${2:-}" = "sudo" ]; then
      sudo codesign --force -s - "$1" 2>/dev/null || true
    else
      codesign --force -s - "$1" 2>/dev/null || true
    fi
  fi
}

# verify_release_signature checks sig_file (an ASN.1 DER ECDSA signature, as
# produced by `openssl dgst -sha256 -sign`) against data_file using
# pubkey_file, via openssl. Exit status communicates the verdict; prints
# nothing so the caller controls all user-facing messaging.
verify_release_signature() {
  pubkey_file="$1"
  sig_file="$2"
  data_file="$3"
  openssl dgst -sha256 -verify "$pubkey_file" -signature "$sig_file" "$data_file" >/dev/null 2>&1
}

# fetch_release_signature downloads url (a sha256sums.txt.sig URL) to dest,
# distinguishing "no signature published for this release" from "the
# download failed" the same way cmd/update.go's SignatureAsset()-then-
# downloadFile split does: a 404 means the release predates signing (soft-
# fail candidate — dest is left missing, and verify_checksums_signature_flow
# warns and passes). Anything else — a non-2xx status, or curl unable to
# complete the request at all (DNS failure, connection refused, timeout) —
# is a hard failure, since it's indistinguishable from an attacker blocking
# just this one URL to force the checksum-only fallback. Silently treating
# every curl failure as "unsigned" (as an earlier version of this script
# did) would let that attack through.
fetch_release_signature() {
  url="$1"
  dest="$2"
  status="$(curl -sSL -w '%{http_code}' -o "$dest" "$url" 2>/dev/null)" || status="000"
  case "$status" in
    200) return 0 ;;
    404)
      rm -f "$dest"
      return 0
      ;;
    *)
      rm -f "$dest"
      log "fetching ${url}: HTTP ${status}"
      return 1
      ;;
  esac
}

# verify_checksums_signature_flow implements the same soft-fail/hard-fail
# decision the main install flow uses when checking a release's signature:
# a missing or empty sig_file means the release predates signing (soft-fail:
# warn, return 0); a present-but-invalid sig_file is a hard failure (return
# 1); a present-and-valid sig_file is success (return 0). Takes pubkey_file
# explicitly rather than reading RELEASE_SIGNING_PUBKEY, so it's testable
# against a throwaway keypair instead of the real embedded one.
verify_checksums_signature_flow() {
  pubkey_file="$1"
  sig_file="$2"
  checksums_file="$3"

  if [ ! -s "$sig_file" ]; then
    # Soft-fail: releases published before signing was introduced have no
    # sha256sums.txt.sig. Keep in sync with requireReleaseSignature in
    # cmd/update.go — once that flips to true, this should too.
    log "warning: release has no sha256sums.txt.sig — checksum-only integrity (release predates signing)"
    return 0
  fi

  log "Verifying release signature..."
  if ! command -v openssl >/dev/null 2>&1; then
    log "sha256sums.txt.sig is present but openssl is not installed — cannot verify it"
    return 1
  fi

  if verify_release_signature "$pubkey_file" "$sig_file" "$checksums_file"; then
    log "Signature verified"
    return 0
  fi
  log "signature does not match sha256sums.txt"
  return 1
}

PRINT_INSTALL_DIR_ONLY=false
while [ $# -gt 0 ]; do
  case "$1" in
    --print-install-dir) PRINT_INSTALL_DIR_ONLY=true ;;
    --strip-quarantine)
      shift
      [ $# -gt 0 ] || die "--strip-quarantine requires a path argument"
      strip_quarantine "$1"
      exit 0
      ;;
    --verify-signature)
      shift
      [ $# -ge 3 ] || die "--verify-signature requires <pubkey-file> <sig-file> <data-file>"
      if verify_release_signature "$1" "$2" "$3"; then
        log "signature valid"
        exit 0
      else
        log "signature invalid"
        exit 1
      fi
      ;;
    --verify-signature-flow)
      shift
      [ $# -ge 3 ] || die "--verify-signature-flow requires <pubkey-file> <sig-file> <checksums-file>"
      if verify_checksums_signature_flow "$1" "$2" "$3"; then
        exit 0
      else
        exit 1
      fi
      ;;
    --fetch-signature)
      shift
      [ $# -ge 2 ] || die "--fetch-signature requires <url> <dest-file>"
      if fetch_release_signature "$1" "$2"; then
        exit 0
      else
        exit 1
      fi
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
  shift
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

# extract_tag_name prints the first "tag_name" value found in a JSON blob
# piped in on stdin (a single release's JSON, always small enough that the
# writer finishes before `grep -m1` closes its end — unlike the /releases
# list, see pick_latest_tag below). Callers must pipe in with `printf '%s'
# "$X" |`, not a `<<<` herestring — this script is invoked via `sh` per its
# own documented usage, and herestrings are a bash-only extension.
extract_tag_name() {
  grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/'
}

# pick_latest_tag prints a tag_name from a /releases list JSON blob passed
# on stdin, preferring a non-prerelease over any prerelease (matching what
# GitHub's own /releases/latest does when it isn't 404ing on us). GitHub's
# list endpoint is documented as newest-first but was observed live to
# return a release out of order (a freshly created release landed 3rd, not
# 1st) — so list position isn't trustworthy, and `sort -V` alone isn't
# either: every implementation tested (GNU, BSD, uutils, BusyBox) sorts a
# bare "v0.1.0" *before* "v0.1.0-rc.9", the opposite of semver precedence —
# so version-sorting the raw tag list would silently prefer a stale
# prerelease the moment a real v0.1.0 ships. Pairing tag_name with
# created_at isn't reliable either: each release's assets array carries its
# own nested created_at fields, so a flat grep picks up several timestamps
# per release, not one. tag_name and prerelease are both release-level only
# (never on assets), so they pair up 1:1 in list order.
pick_latest_tag() {
  json="$(cat)"
  scratch="$(mktemp -d)"
  printf '%s' "$json" | grep -o '"tag_name": *"[^"]*"' | sed -E 's/.*"([^"]+)"$/\1/' >"$scratch/tags"
  printf '%s' "$json" | grep -o '"prerelease": *[a-z]*' | sed -E 's/.*: *//' >"$scratch/prerelease"
  stable="$(paste -d ' ' "$scratch/prerelease" "$scratch/tags" | awk '$1 == "false" { print $2 }' | sort -V | tail -1)"
  if [ -n "$stable" ]; then
    printf '%s' "$stable"
  else
    sort -V "$scratch/tags" | tail -1
  fi
  rm -rf "$scratch"
}

log "Resolving ${VERSION} release for ${PLATFORM}..."
if [ "$VERSION" = "latest" ]; then
  # A 404 here is an expected, handled case below (every argus release so
  # far is a prerelease, and this endpoint excludes those) — silence it so
  # a normal install doesn't print a scary raw curl error on every run.
  RELEASE_JSON="$(curl -sSfL "$API_URL" 2>/dev/null)" || true
else
  RELEASE_JSON="$(curl -sSfL "$API_URL")" || true
fi
if [ -n "$RELEASE_JSON" ]; then
  TAG="$(printf '%s' "$RELEASE_JSON" | extract_tag_name)"
elif [ "$VERSION" = "latest" ]; then
  # /releases/latest only returns non-prerelease releases; every argus
  # release so far is a prerelease (v0.1.0-rc.N), so it 404s here. Fall
  # back to picking the newest entry in the full release list instead.
  API_URL="https://api.github.com/repos/${REPO}/releases?per_page=100"
  RELEASE_JSON="$(curl -sSfL "$API_URL")" || die "fetching release metadata from ${API_URL}"
  TAG="$(printf '%s' "$RELEASE_JSON" | pick_latest_tag)"
else
  die "fetching release metadata from ${API_URL}"
fi
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

SIG_URL="https://github.com/${REPO}/releases/download/${TAG}/sha256sums.txt.sig"
SIG_FILE="${TMP_DIR}/sha256sums.txt.sig"
fetch_release_signature "$SIG_URL" "$SIG_FILE" || die "fetching ${SIG_URL} failed — refusing to install (release may be tampered, or this may be a network problem)"

PUBKEY_FILE="${TMP_DIR}/release-signing-pubkey.pem"
printf '%s\n' "$RELEASE_SIGNING_PUBKEY" >"$PUBKEY_FILE"

verify_checksums_signature_flow "$PUBKEY_FILE" "$SIG_FILE" "${TMP_DIR}/sha256sums.txt" ||
  die "signature verification failed for ${TAG} — refusing to install (release may be tampered)"

strip_quarantine "${TMP_DIR}/${ASSET}"

chmod +x "${TMP_DIR}/${ASSET}"

INSTALL_DIR="$(select_install_dir)"
mkdir -p "$INSTALL_DIR" 2>/dev/null || true

if [ -w "$INSTALL_DIR" ]; then
  install -m 0755 "${TMP_DIR}/${ASSET}" "${INSTALL_DIR}/argus"
  resign_darwin_binary "${INSTALL_DIR}/argus"
elif command -v sudo >/dev/null 2>&1; then
  log "${INSTALL_DIR} is not writable, using sudo..."
  sudo install -m 0755 "${TMP_DIR}/${ASSET}" "${INSTALL_DIR}/argus"
  resign_darwin_binary "${INSTALL_DIR}/argus" sudo
else
  die "${INSTALL_DIR} is not writable and sudo is unavailable; set ARGUS_INSTALL_DIR to a writable directory on PATH"
fi

log "installed argus ${TAG} to ${INSTALL_DIR}/argus"
if ! path_contains "$INSTALL_DIR"; then
  log "warning: ${INSTALL_DIR} is not on PATH"
fi
