#!/usr/bin/env bash
# load-config driver: locate → validate (schema + secret scan) → resolve.
# Inputs via env: CONTRACT_ROOT, CONTRACT_FILE, OVERRIDES, ORG_CONFIG,
# ORG_ROOT, ORG_REPO, ORG_REF, ORG_PATH, SOURCE_REPO, SOURCE_REF, OUT_DIR.
# Fails fast; the failure reason is written to "$OUT_DIR/error.txt" for
# downstream signal steps.
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

yaml_to_json() {
  if python3 -c 'import yaml' 2>/dev/null; then
    python3 -c 'import yaml,json,sys; json.dump(yaml.safe_load(open(sys.argv[1])), open(sys.argv[2], "w"))' "$1" "$2"
  else
    npx -y -q js-yaml "$1" > "$2"
  fi
}

RAW="$OUT_DIR/raw.json"
if [ ! -f "$CFG" ]; then
  if [ "${REQUIRED:-true}" = "true" ]; then
    fail "contract not found: $WHERE — every AI-SDLC repo must carry a .ai-sdlc.yaml (spec/ai-sdlc.schema.json). The stage refuses to run on implicit defaults."
  fi
  echo "::warning title=ai-sdlc contract::contract not found: $WHERE — proceeding on runtime defaults (required=false). Dispatch-affecting stages must keep required=true."
  echo '{}' > "$RAW"
else
  if ! report="$(bash "$VALIDATOR" "$CFG" "$SCHEMA" 2>&1)"; then
    fail "contract invalid: $WHERE
$report"
  fi
  echo "$report"
  yaml_to_json "$CFG" "$RAW"
fi


# Org baseline layer (spec/org-policy.md): fetched by the action into
# ORG_ROOT/ORG_PATH. Unreachable or invalid baselines fail safe to
# repo + defaults and log once; they never brick the consumer pipeline.
ORG_JSON=""
ORG_STATE="not-applied"
if [ -n "${ORG_CONFIG:-}" ]; then
  ORG_FILE="${ORG_ROOT:-.ai-sdlc-org-src}/${ORG_PATH:-.ai-sdlc.yaml}"
  if [ ! -f "$ORG_FILE" ]; then
    ORG_STATE="unavailable"
    echo "::warning title=ai-sdlc contract::org baseline ${ORG_CONFIG} was not fetched (${ORG_FILE} missing) — failing safe to repo contract + defaults (spec/org-policy.md)."
  elif ! orgreport="$(bash "$VALIDATOR" "$ORG_FILE" "$SCHEMA" 2>&1)"; then
    ORG_STATE="unavailable"
    echo "::warning title=ai-sdlc contract::org baseline ${ORG_CONFIG} failed validation — failing safe to repo contract + defaults (spec/org-policy.md)."
    printf '%s\n' "$orgreport"
  else
    echo "$orgreport"
    ORG_JSON="$OUT_DIR/org.json"
    yaml_to_json "$ORG_FILE" "$ORG_JSON"
    ORG_STATE="applied"
  fi
fi

OVR="${OVERRIDES:-}"; [ -n "${OVR//[[:space:]]/}" ] || OVR='{}'
if ! err="$(python3 "$ACTION_DIR/resolve.py" --input "$RAW" --overrides "$OVR" \
      --org "$ORG_JSON" --org-state "$ORG_STATE" \
      --org-repo "${ORG_REPO:-}" --org-ref "${ORG_REF:-}" --org-path "${ORG_PATH:-.ai-sdlc.yaml}" \
      --out-dir "$OUT_DIR" --source-repo "${SOURCE_REPO:-}" --source-ref "${SOURCE_REF:-}" \
      --source-path "${CONTRACT_FILE:-.ai-sdlc.yaml}" 2>&1)"; then
  fail "contract resolution failed: $WHERE
$err"
fi
echo "$err"
rm -f "$RAW" ${ORG_JSON:+"$ORG_JSON"}
