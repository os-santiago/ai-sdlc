#!/usr/bin/env bash
# Run shellcheck exactly as CI does: downloads the pinned release, verifies
# its SHA-256, extracts to a temp dir and runs it against the given files.
# Invoke from the repo root:
#
#   bash tools/shellcheck.sh tools/*.sh .github/actions/*/*.sh tests/*.sh
#
# The version + checksum live here (not inline in a workflow) so CI gates
# and local/pre-PR runs share a single pinned source of truth — same
# pattern as tools/actionlint.sh (issue #129). Override with
# SHELLCHECK_VERSION / SHELLCHECK_SHA256 to test a different release.
set -euo pipefail

# Pinned release: https://github.com/koalaman/shellcheck/releases/tag/v0.11.0
SHELLCHECK_VERSION="${SHELLCHECK_VERSION:-0.11.0}"
# SHA-256 of shellcheck-v0.11.0.linux.x86_64.tar.xz. Shellcheck publishes no
# checksums file; this digest was computed from the official release asset.
SHELLCHECK_SHA256="${SHELLCHECK_SHA256:-8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

tarball="shellcheck-v${SHELLCHECK_VERSION}.linux.x86_64.tar.xz"
curl -fsSL -o "$tmpdir/$tarball" \
  "https://github.com/koalaman/shellcheck/releases/download/v${SHELLCHECK_VERSION}/${tarball}"
(cd "$tmpdir" \
  && echo "${SHELLCHECK_SHA256}  ${tarball}" | sha256sum -c - \
  && tar -xJf "$tarball")

# Not exec: the EXIT trap must run to clean up $tmpdir; the script's exit
# status is still shellcheck's own.
"$tmpdir/shellcheck-v${SHELLCHECK_VERSION}/shellcheck" "$@"
