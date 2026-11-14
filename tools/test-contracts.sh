#!/usr/bin/env bash
# Run from the repository root. Requires tools/quality-requirements.txt.
set -euo pipefail
for contract in examples/*.ai-sdlc.yaml; do
  bash tools/validate-ai-sdlc-config.sh "$contract"
done
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
printf 'version: invalid\n' > "$tmpdir/invalid.yaml"
if bash tools/validate-ai-sdlc-config.sh "$tmpdir/invalid.yaml"; then
  echo 'Invalid schema unexpectedly accepted' >&2
  exit 1
fi
printf 'token: synthetic-credential-fixture\n' > "$tmpdir/secret.yaml"
if bash tools/validate-ai-sdlc-config.sh "$tmpdir/secret.yaml"; then
  echo 'Inline credential unexpectedly accepted' >&2
  exit 1
fi
