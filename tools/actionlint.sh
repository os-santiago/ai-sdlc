#!/usr/bin/env bash
# Run actionlint exactly as CI does (.github/workflows/actionlint.yml):
# downloads the pinned release, verifies its SHA-256, extracts to a temp dir
# and runs it against this checkout. Invoke from the repo root:
#
#   bash tools/actionlint.sh [actionlint args]    # CI passes: -color
#
# With no file operands (the CI posture) the run covers BOTH surfaces:
#   1. .github/workflows/*.yml — actionlint's auto-discovery.
#   2. .github/actions/*/action.yml — actionlint cannot lint action files as
#      direct operands (every file argument is parsed as a workflow), but
#      checks them transitively when a workflow `uses:` them (v1.7.0+). A
#      generated caller workflow (tools/actionlint_sweep.py) references
#      every local action — including ones only consumers use via
#      `owner/repo/path@ref` — so a broken action.yml cannot merge unlinted
#      (issue #130).
# With explicit file operands the sweep is skipped: only the files given.
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
sweep=""
cleanup() { rm -rf "$tmpdir"; [ -n "$sweep" ] && rm -f "$sweep"; }
trap cleanup EXIT

tarball="actionlint_${ACTIONLINT_VERSION}_linux_amd64.tar.gz"
curl -fsSL -o "$tmpdir/$tarball" \
  "https://github.com/rhysd/actionlint/releases/download/v${ACTIONLINT_VERSION}/${tarball}"
(cd "$tmpdir" \
  && echo "${ACTIONLINT_SHA256}  ${tarball}" | sha256sum -c - \
  && tar -xzf "$tarball")

# Not exec: the EXIT trap must run to clean up $tmpdir; the script's exit
# status is still actionlint's own.
"$tmpdir/actionlint" "$@"

# Composite-action sweep (issue #130): only when the caller wants the repo
# lint — i.e. no file operands (a lone '-' is the stdin operand and counts).
has_operands=0
for a in "$@"; do
  case "$a" in -?*) ;; *) has_operands=1 ;; esac
done
if [ "$has_operands" -eq 0 ]; then
  ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
  # The generated caller must live inside the repo for `./.github/actions/…`
  # resolution — actionlint finds the project root by walking up to .github/.
  sweep="$ROOT/.actionlint-sweep.$$.yml"
  python3 "$ROOT/tools/actionlint_sweep.py" "$ROOT/.github/actions" "$sweep"
  (cd "$ROOT" && "$tmpdir/actionlint" "$sweep")
fi
