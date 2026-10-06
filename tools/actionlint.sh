#!/usr/bin/env bash
# Run actionlint exactly as CI does (.github/workflows/actionlint.yml):
# downloads the pinned release, verifies its SHA-256, extracts to a temp dir
# and runs it against this checkout. Invoke from the repo root:
#
#   bash tools/actionlint.sh [actionlint args]    # CI passes: -color
#
# The version + checksum live here (not inline in the workflow) so CI gates
# and local/pre-PR runs share a single pinned source of truth. Override with
# ACTIONLINT_VERSION / ACTIONLINT_SHA256 to test a different release.
set -euo pipefail

# Pinned release: https://github.com/rhysd/actionlint/releases/tag/v1.7.12
ACTIONLINT_VERSION="${ACTIONLINT_VERSION:-1.7.12}"
# SHA-256 of actionlint_1.7.12_linux_amd64.tar.gz, from the release's
# published actionlint_1.7.12_checksums.txt.
ACTIONLINT_SHA256="${ACTIONLINT_SHA256:-8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

tarball="actionlint_${ACTIONLINT_VERSION}_linux_amd64.tar.gz"
curl -fsSL -o "$tmpdir/$tarball" \
  "https://github.com/rhysd/actionlint/releases/download/v${ACTIONLINT_VERSION}/${tarball}"
(cd "$tmpdir" \
  && echo "${ACTIONLINT_SHA256}  ${tarball}" | sha256sum -c - \
  && tar -xzf "$tarball")

# Not exec: the EXIT trap must run to clean up $tmpdir; the script's exit
# status is still actionlint's own.
"$tmpdir/actionlint" "$@"
