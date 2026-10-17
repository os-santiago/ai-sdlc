#!/usr/bin/env bash
# Run gitleaks exactly as CI does (.github/workflows/secret-scan.yml):
# downloads the pinned release, verifies its SHA-256, extracts to a temp
# dir and runs it. Invoke from the repo root:
#
#   bash tools/gitleaks.sh git . --redact                    # full history
#   bash tools/gitleaks.sh git . --redact --log-opts A..B    # commit range
#   bash tools/gitleaks.sh dir . --redact                    # working tree
#
# The version + checksum live here (not inline in the workflow) so CI gates
# and local/pre-PR scans share a single pinned source of truth — same
# pattern as tools/actionlint.sh and tools/shellcheck.sh. Override with
# GITLEAKS_VERSION / GITLEAKS_SHA256 to test a different release.
set -euo pipefail

# Pinned release: https://github.com/gitleaks/gitleaks/releases/tag/v8.30.1
GITLEAKS_VERSION="${GITLEAKS_VERSION:-8.30.1}"
# SHA-256 of gitleaks_8.30.1_linux_x64.tar.gz, from the release's
# published gitleaks_8.30.1_checksums.txt.
GITLEAKS_SHA256="${GITLEAKS_SHA256:-551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

tarball="gitleaks_${GITLEAKS_VERSION}_linux_x64.tar.gz"
curl -fsSL -o "$tmpdir/$tarball" \
  "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/${tarball}"
(cd "$tmpdir" \
  && echo "${GITLEAKS_SHA256}  ${tarball}" | sha256sum -c - \
  && tar -xzf "$tarball")

# Not exec: the EXIT trap must run to clean up $tmpdir; the script's exit
# status is still gitleaks' own.
"$tmpdir/gitleaks" "$@"
