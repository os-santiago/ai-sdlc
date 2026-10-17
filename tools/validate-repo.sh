#!/usr/bin/env bash
# tools/validate-repo.sh — the repo's local test suite. Everything a change
# must keep green, in one command; wired to `npm test` / `npm run build`.
#
#   1. .ai-sdlc.yaml + examples/*.ai-sdlc.yaml — JSON-schema conformance and
#      secret scan via tools/validate-ai-sdlc-config.sh
#   2. spec/*.json parse as JSON and carry no duplicate keys (a dup key is a
#      silent spec ambiguity — parsers go last-wins, so the stricter of two
#      definitions is dead without anyone noticing; issue #255)
#   3. .github/workflows/*.yml, .github/actions/*/action.yml and the contracts
#      parse as YAML
#   4. actionlint, when a binary is already installed (CI owns the pinned
#      release — .github/workflows/actionlint.yml; ./actionlint fetches it)
#   5. node --test on */*.test.js + scripts/gha/*.test.js, when node and
#      test files are present
#
# Exit 0 = clean, 1 = a check failed, 2 = required tooling missing.
# Safe to run from anywhere: cd's to the repo root first.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { printf '[X] %s\n' "$*" >&2; exit 1; }
note() { printf '[i] %s\n' "$*"; }

# --- 1. Contracts -------------------------------------------------------------
contracts=(.ai-sdlc.yaml)
for cfg in examples/*.ai-sdlc.yaml; do
  [ -f "$cfg" ] || continue
  contracts+=("$cfg")
done
for cfg in "${contracts[@]}"; do
  bash tools/validate-ai-sdlc-config.sh "$cfg" spec/ai-sdlc.schema.json
done

# --- 2. Spec JSON parses + no duplicate keys -----------------------------------
# (the contract validator above already hard-requires python3)
python3 - <<'PY'
import glob, json, sys

class Obj(dict):
    """dict that remembers which keys appeared twice while parsing."""

def hook(pairs):
    o = Obj()
    o.dupes = []
    for k, v in pairs:
        if k in o:
            o.dupes.append(k)
        o[k] = v
    return o

def walk(node, path, out):
    if isinstance(node, Obj):
        for k in node.dupes:
            out.append(f"{path}.{k}" if path else k)
        for k, v in node.items():
            walk(v, f"{path}.{k}" if path else k, out)
    elif isinstance(node, list):
        for i, v in enumerate(node):
            walk(v, f"{path}[{i}]", out)

ok = True
for f in sorted(glob.glob('spec/*.json')):
    try:
        doc = json.load(open(f), object_pairs_hook=hook)
    except Exception as e:
        print(f"[X] {f}: invalid JSON — {e}", file=sys.stderr)
        ok = False
        continue
    dupes = []
    walk(doc, '', dupes)
    if dupes:
        print(f"[X] {f}: duplicate key(s) {', '.join(dupes)} — last-wins "
              "parsing leaves one definition dead; a normative spec must be "
              "unambiguous", file=sys.stderr)
        ok = False
    else:
        print(f"[✓] {f}: valid JSON, no duplicate keys")
sys.exit(0 if ok else 1)
PY

# --- 3. Workflow / action / contract files parse as YAML -----------------------
if python3 -c 'import yaml' 2>/dev/null; then
  python3 - <<'PY'
import glob, sys, yaml
files = sorted(glob.glob('.ai-sdlc.yaml') + glob.glob('examples/*.yaml')
               + glob.glob('.github/workflows/*.yml')
               + glob.glob('.github/actions/*/action.yml'))
for f in files:
    try:
        with open(f) as fh:
            yaml.safe_load(fh)
    except Exception as e:
        sys.exit(f"[X] {f}: {e}")
    print(f"[✓] {f}")
PY
elif command -v npx >/dev/null 2>&1; then
  for f in .ai-sdlc.yaml examples/*.yaml .github/workflows/*.yml .github/actions/*/action.yml; do
    [ -f "$f" ] || continue
    if npx -y -q js-yaml "$f" >/dev/null; then
      echo "[✓] $f"
    else
      fail "$f: YAML parse failed"
    fi
  done
else
  note "no YAML parser (need python3+PyYAML or npx) — workflow YAML parse-check skipped"
fi

# --- 4. actionlint (when installed) ---------------------------------------------
if command -v actionlint >/dev/null 2>&1; then
  actionlint || fail "actionlint reported workflow violations"
  echo "[✓] actionlint: workflows clean"
else
  note "actionlint not on PATH — skipped (CI pin: .github/workflows/actionlint.yml; run ./actionlint -color for full parity)"
fi

# --- 5. node --test (when node and test files exist) ----------------------------
if command -v node >/dev/null 2>&1; then
  tests=()
  # */*.test.js misses action-dir tests (.github/actions/*/), nested
  # scripts (scripts/gha/) and nested tests (tests/e2e/) — enumerate all.
  for t in */*.test.js .github/actions/*/*.test.js scripts/**/*.test.js tests/**/*.test.js; do
    [ -f "$t" ] || continue
    tests+=("$t")
  done
  if [ "${#tests[@]}" -gt 0 ]; then
    node --test "${tests[@]}" || fail "node --test failed"
    echo "[✓] node --test: ${#tests[@]} file(s) green"
  fi
else
  note "node not on PATH — node --test skipped"
fi

note "repo validation complete"
