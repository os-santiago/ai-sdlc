#!/usr/bin/env bash
# Validate a .ai-sdlc.yaml against spec/ai-sdlc.schema.json — CI-usable.
#
# Usage:
#   tools/validate-ai-sdlc-config.sh [config-file] [schema-file]
#
# Requirements (auto-resolved, no global installs):
#   - YAML→JSON: python3 + PyYAML  OR  npx js-yaml
#   - Schema:    npx ajv-cli       OR  python3 + jsonschema
#
# Exit 0 = valid, 1 = invalid, 2 = usage error / missing tools.
set -euo pipefail

CONFIG="${1:-.ai-sdlc.yaml}"
SCHEMA="${2:-spec/ai-sdlc.schema.json}"

if [ ! -f "$CONFIG" ]; then echo "config not found: $CONFIG" >&2; exit 2; fi
if [ ! -f "$SCHEMA" ]; then echo "schema not found: $SCHEMA" >&2; exit 2; fi

TMPJSON="$(mktemp --suffix=.json)"
trap 'rm -f "$TMPJSON"' EXIT

# --- YAML → JSON -----------------------------------------------------------
if python3 -c 'import yaml' 2>/dev/null; then
  python3 -c 'import yaml,json,sys; json.dump(yaml.safe_load(open(sys.argv[1])), sys.stdout)' "$CONFIG" > "$TMPJSON"
elif command -v npx >/dev/null 2>&1; then
  npx -y -q js-yaml "$CONFIG" > "$TMPJSON"
else
  echo "no YAML parser available (need python3+PyYAML or npx)" >&2; exit 2
fi

# --- JSON Schema validation ------------------------------------------------
if python3 -c 'import jsonschema' 2>/dev/null; then
  python3 - "$SCHEMA" "$TMPJSON" <<'PY'
import json, sys, jsonschema
schema = json.load(open(sys.argv[1])); data = json.load(open(sys.argv[2]))
try:
    jsonschema.Draft202012Validator.check_schema(schema)
    jsonschema.validate(data, schema)
    print(f"[✓] config conforms to {sys.argv[1]}")
except jsonschema.ValidationError as e:
    print(f"[X] validation failed at {list(e.absolute_path)}: {e.message}", file=sys.stderr)
    sys.exit(1)
PY
elif command -v npx >/dev/null 2>&1; then
  if npx -y -q ajv-cli validate -s "$SCHEMA" -d "$TMPJSON" --spec draft2020 --strict=false; then
    echo "[✓] $CONFIG conforms to $SCHEMA"; exit 0
  else
    echo "[X] $CONFIG does NOT conform to $SCHEMA" >&2
    npx -y -q ajv-cli validate -s "$SCHEMA" -d "$TMPJSON" --spec draft2020 --strict=false 2>&1 | tail -20 >&2
    exit 1
  fi
else
  echo "no validator available (need python3+jsonschema or npx)" >&2; exit 2
fi
