#!/usr/bin/env bash
# tests/validate-config.test.sh — CLI-level e2e for the contract validator
# (issue #128). Drives the real tools/validate-ai-sdlc-config.sh over the
# tests/contracts fixture suite and asserts the process contract:
#   exit 0 = valid · exit 1 = rejection (yaml parse | secret scan | schema)
#   exit 2 = usage error (missing config/schema/tools)
# Each invalid fixture declares its expected rejection stage via a
# `# expect: <yaml|secret|schema>` header — read here and in
# tests/contract_fixtures.test.js (which owns the schema-semantics side).
set -euo pipefail
cd "$(dirname "$0")/.."

VALIDATE="tools/validate-ai-sdlc-config.sh"
SCHEMA="spec/ai-sdlc.schema.json"
FIX="tests/contracts"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0; fail=0
ok()  { pass=$((pass+1)); printf '[✓] %s\n' "$*"; }
bad() { fail=$((fail+1)); printf '[X] %s\n' "$*" >&2; }

# The secret-scan stage is python-only (stdlib re/json); without python3
# the validator exits 2 for every input and the exit-code contract being
# tested here does not exist — same precondition as validate-repo.sh.
if ! command -v python3 >/dev/null 2>&1; then
  echo "[i] python3 not on PATH — validator e2e skipped" >&2
  exit 0
fi

# $1 expected exit · $2 label · $3 config · $4 stderr regex ("" = skip) · $5 schema override
expect() {
  local want="$1" label="$2" cfg="$3" re="${4:-}" schema="${5:-$SCHEMA}"
  set +e
  bash "$VALIDATE" "$cfg" "$schema" >"$WORK/out.txt" 2>"$WORK/err.txt"
  local got=$?
  set -e
  if [ "$got" != "$want" ]; then
    bad "$label: want exit $want, got $got — stderr: $(head -5 "$WORK/err.txt")"
    return
  fi
  if [ -n "$re" ] && ! grep -Eq "$re" "$WORK/err.txt"; then
    bad "$label: exit $want ok but stderr missing /$re/ — got: $(head -5 "$WORK/err.txt")"
    return
  fi
  ok "$label (exit $got)"
}

# --- valid fixtures: exit 0 and a conformance report on stdout -------------
for f in "$FIX"/valid/*.yaml; do
  base="$(basename "$f")"
  expect 0 "valid/$base accepted" "$f"
  if [ -s "$WORK/out.txt" ] && grep -Eq 'conforms' "$WORK/out.txt"; then
    ok "valid/$base: conformance reported on stdout"
  else
    bad "valid/$base: stdout missing the conformance marker"
  fi
done

# --- invalid fixtures: exit 1, stderr names the rejection stage ------------
for f in "$FIX"/invalid/*.yaml; do
  base="$(basename "$f")"
  stage="$(sed -n 's/^# expect:[[:space:]]*\([a-zA-Z]*\).*/\1/p' "$f" | head -n1)"
  case "$stage" in
    schema)
      # python path: "[X] validation failed at [...]: <message>"
      # ajv path:    "[X] <cfg> does NOT conform to <schema>"
      expect 1 "invalid/$base [schema reject]" "$f" 'validation failed|does NOT conform'
      ;;
    secret)
      expect 1 "invalid/$base [secret scan]" "$f" 'secret-looking value'
      ;;
    yaml)
      # parse failure — any stderr diagnostic, exit 1 under both parsers
      expect 1 "invalid/$base [yaml parse]" "$f" '.'
      ;;
    *)
      bad "invalid/$base: missing or unknown '# expect:' header (got '$stage')"
      ;;
  esac
done

# --- usage errors: exit 2 ----------------------------------------------------
expect 2 "missing config file" "$WORK/no-such-config.yaml" 'config not found'
expect 2 "missing schema file" "$FIX/valid/minimal.yaml" 'schema not found' "$WORK/no-such-schema.json"

printf '%s\n' "--- $pass passed, $fail failed ---"
[ "$fail" -eq 0 ]
