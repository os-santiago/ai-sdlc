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
# Besides schema conformance, the contract is scanned for secret-looking
# values (API keys, tokens, passwords, private keys): credentials live in
# GitHub secrets only, never in .ai-sdlc.yaml (see AGENTS.md).
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

# --- Secret scan (runs first so schema errors never echo a secret) --------
# Rejects credential-shaped keys/values anywhere in the contract. Env-var
# references ($VAR, ${VAR}) are allowed; literal secrets are not.
if ! command -v python3 >/dev/null 2>&1; then
  echo "secret scan requires python3" >&2; exit 2
fi
python3 - "$TMPJSON" "$CONFIG" <<'PY'
import json, re, sys

data = json.load(open(sys.argv[1]))
config = sys.argv[2]

SECRET_KEY = re.compile(
    r'^(?:.*[-_])?(?:api[-_]?key|apikey|access[-_]?key|secret(?:[-_]?key)?|'
    r'client[-_]?secret|token|auth[-_]?token|access[-_]?token|password|passwd|pwd|'
    r'credentials?|private[-_]?key)$', re.I)
SECRET_VALUE = [
    ('GitHub token', re.compile(r'\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})')),
    ('OpenAI/Anthropic-style key', re.compile(r'\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}')),
    ('NVIDIA API key', re.compile(r'\bnvapi-[A-Za-z0-9_-]{20,}')),
    ('Slack token', re.compile(r'\bxox[abprs]-[A-Za-z0-9-]{10,}')),
    ('AWS access key id', re.compile(r'\b(?:AKIA|ASIA)[0-9A-Z]{16}\b')),
    ('Google API key', re.compile(r'\bAIza[0-9A-Za-z_-]{35}')),
    ('private key block', re.compile(r'-----BEGIN [A-Z ]*PRIVATE KEY-----')),
    ('JWT', re.compile(r'\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.')),
    ('bearer credential', re.compile(r'\bbearer\s+[A-Za-z0-9._~+/=-]{16,}', re.I)),
    ('URL with embedded credentials', re.compile(r'://[^/\s:@]+:[^/\s@]+@')),
    ('inline credential assignment', re.compile(
        r'\b[A-Za-z0-9_-]*(?:api[-_]?key|apikey|token|secret|password|passwd)\s*[:=]\s*'
        r'[\'"]?(?![\'"]?\$)[^\s\'"]{8,}', re.I)),
]

findings = []
def walk(node, path):
    if isinstance(node, dict):
        for k, v in node.items():
            p = path + [str(k)]
            if SECRET_KEY.match(str(k)) and isinstance(v, str) and v.strip():
                findings.append(('credential-named key', p))
            walk(v, p)
    elif isinstance(node, list):
        for i, v in enumerate(node):
            walk(v, path + [str(i)])
    elif isinstance(node, str):
        for name, rx in SECRET_VALUE:
            if rx.search(node):
                findings.append((name, path)); break

walk(data, [])
if findings:
    for name, p in findings:
        # Never echo the offending value — only its location.
        print(f"[X] secret-looking value ({name}) at {'.'.join(p) or '<root>'} — "
              "credentials belong in GitHub secrets, not in the contract", file=sys.stderr)
    sys.exit(1)
print(f"[✓] {config}: no secret-looking values")
PY

# --- JSON Schema validation ------------------------------------------------
if python3 -c 'import jsonschema; jsonschema.Draft202012Validator' 2>/dev/null; then
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
    echo "[✓] $CONFIG conforms to $SCHEMA"
  else
    echo "[X] $CONFIG does NOT conform to $SCHEMA" >&2
    npx -y -q ajv-cli validate -s "$SCHEMA" -d "$TMPJSON" --spec draft2020 --strict=false 2>&1 | tail -20 >&2
    exit 1
  fi
else
  echo "no validator available (need python3+jsonschema or npx)" >&2; exit 2
fi
