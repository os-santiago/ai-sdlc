#!/usr/bin/env bash
# load-config driver: locate → validate (schema + secret scan) → resolve.
# Inputs via env: CONTRACT_ROOT, CONTRACT_FILE, OVERRIDES, ORG_CONFIG,
# SOURCE_REPO, SOURCE_REF, OUT_DIR. Fails fast; the failure reason is
# written to "$OUT_DIR/error.txt" for downstream signal steps.
set -euo pipefail

ACTION_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUNTIME_ROOT="$(cd "$ACTION_DIR/../../.." && pwd)"
SCHEMA="$RUNTIME_ROOT/spec/ai-sdlc.schema.json"
VALIDATOR="$RUNTIME_ROOT/tools/validate-ai-sdlc-config.sh"
OUT_DIR="${OUT_DIR:?OUT_DIR required}"
CFG="${CONTRACT_ROOT:-.}/${CONTRACT_FILE:-.ai-sdlc.yaml}"
WHERE="${SOURCE_REPO:-<repo>}@${SOURCE_REF:-<default branch>}:${CONTRACT_FILE:-.ai-sdlc.yaml}"

rm -rf "$OUT_DIR"; mkdir -p "$OUT_DIR"

fail() {
  printf '%s\n' "$1" > "$OUT_DIR/error.txt"
  local msg="${1//'%'/%25}"; msg="${msg//$'\r'/}"; msg="${msg//$'\n'/%0A}"
  echo "::error title=ai-sdlc contract (scc:config-error)::$msg"
  exit 1
}

if [ ! -f "$CFG" ]; then
  fail "contract not found: $WHERE — every AI-SDLC repo must carry a .ai-sdlc.yaml (spec/ai-sdlc.schema.json). The stage refuses to run on implicit defaults."
fi

if ! report="$(bash "$VALIDATOR" "$CFG" "$SCHEMA" 2>&1)"; then
  fail "contract invalid: $WHERE
$report"
fi
echo "$report"

if [ -n "${ORG_CONFIG:-}" ]; then
  echo "::warning title=ai-sdlc contract::org-config is a reserved hook — org baseline resolution (spec/org-policy.md) is not implemented yet; using repo contract + runtime defaults only."
fi

RAW="$OUT_DIR/raw.json"
if python3 -c 'import yaml' 2>/dev/null; then
  python3 -c 'import yaml,json,sys; json.dump(yaml.safe_load(open(sys.argv[1])), open(sys.argv[2], "w"))' "$CFG" "$RAW"
else
  npx -y -q js-yaml "$CFG" > "$RAW"
fi

OVR="${OVERRIDES:-}"; [ -n "${OVR//[[:space:]]/}" ] || OVR='{}'
if ! err="$(python3 "$ACTION_DIR/resolve.py" --input "$RAW" --overrides "$OVR" \
      --out-dir "$OUT_DIR" --source-repo "${SOURCE_REPO:-}" --source-ref "${SOURCE_REF:-}" \
      --source-path "${CONTRACT_FILE:-.ai-sdlc.yaml}" 2>&1)"; then
  fail "contract resolution failed: $WHERE
$err"
fi
echo "$err"
rm -f "$RAW"
