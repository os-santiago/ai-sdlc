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
#   4. every .github/workflows job that runs steps declares timeout-minutes
#      (bounded-everything doctrine; jobs calling reusable workflows via
#      `uses:` can't carry the key — the callee's job-level bound applies;
#      issue #136)
#   5. actionlint, when a binary is already installed (CI owns the pinned
#      release — .github/workflows/actionlint.yml; ./actionlint fetches it).
#      With shellcheck on PATH it also lints every workflow `run:` block.
#      Composite actions are covered by a generated caller workflow that
#      `uses:` every .github/actions/*/action.yml — actionlint checks action
#      files only transitively, never as direct operands (issue #130).
#   6. shellcheck over the repo's standalone scripts (issue #129): the
#      ./actionlint shim, tools/*.sh, .github/actions/*/*.sh, tests/*.sh —
#      when a binary is installed (CI runners ship one; tools/shellcheck.sh
#      fetches the pinned release for local parity)
#   7. node --test on */*.test.js + scripts/gha/*.test.js, when node and
#      test files are present
#   8. shell e2e tests (tests/*.test.sh) — CLI-level contracts that assert
#      process exit codes, which node --test can't cover as directly
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

# --- 4. Workflow jobs declare timeout-minutes ------------------------------------
python3 - <<'PY'
import glob, re, sys

try:
    import yaml
except ImportError:
    yaml = None

def jobs_of(path):
    """Map each job id to {'uses': bool, 'timeout': bool}."""
    if yaml is not None:
        doc = yaml.safe_load(open(path)) or {}
        jobs = doc.get('jobs') or {}
        return {jid: {'uses': 'uses' in (job or {}),
                      'timeout': 'timeout-minutes' in (job or {})}
                for jid, job in jobs.items()}
    # No YAML parser: indentation scan — job keys sit at 2-space indent
    # under a top-level 'jobs:' key; uses/timeout-minutes at 4 spaces in.
    out, cur, in_jobs = {}, None, False
    for line in open(path):
        if re.match(r'^\S', line):
            in_jobs = line.startswith('jobs:')
            cur = None
            continue
        if not in_jobs:
            continue
        m = re.match(r'^  ([\w.-]+):\s*(?:#.*)?$', line)
        if m:
            cur = m.group(1)
            out[cur] = {'uses': False, 'timeout': False}
            continue
        if cur and re.match(r'^    uses:', line):
            out[cur]['uses'] = True
        elif cur and re.match(r'^    timeout-minutes:', line):
            out[cur]['timeout'] = True
    return out

bad = []
for f in sorted(glob.glob('.github/workflows/*.yml')):
    for jid, j in jobs_of(f).items():
        # `uses:` jobs call a reusable workflow — GitHub rejects
        # timeout-minutes there; the callee's own job bound applies.
        if not j['uses'] and not j['timeout']:
            bad.append(f"{f}: job '{jid}' runs steps without timeout-minutes")
if bad:
    for b in bad:
        print(f"[X] {b}", file=sys.stderr)
    sys.exit("[X] every step-running job must be bounded — "
             "declare timeout-minutes (issue #136)")
print("[✓] workflows: every step-running job declares timeout-minutes")
PY

# --- 5. actionlint (when installed) ---------------------------------------------
if command -v actionlint >/dev/null 2>&1; then
  actionlint || fail "actionlint reported workflow violations"
  echo "[✓] actionlint: workflows clean"
  # Composite-action sweep (issue #130): actionlint checks action.yml files
  # only transitively via `uses:`, so lint a generated caller referencing
  # every local action — placed inside the repo for ./.github/actions
  # resolution, removed after the lint.
  sweep=".actionlint-sweep.$$.yml"
  if python3 tools/actionlint_sweep.py .github/actions "$sweep"; then
    actionlint "$sweep" || { rm -f "$sweep"; fail "actionlint reported composite-action violations"; }
    rm -f "$sweep"
    echo "[✓] actionlint: composite actions clean"
  else
    rm -f "$sweep"
    note "actionlint sweep generator failed — composite-action lint skipped"
  fi
else
  note "actionlint not on PATH — skipped (CI pin: .github/workflows/actionlint.yml; run ./actionlint -color for full parity)"
fi

# --- 6. shellcheck over standalone scripts (when installed) ---------------------
# Workflow `run:` blocks are not listed here: actionlint already pipes them
# through shellcheck when the binary is on PATH (CI asserts it in
# .github/workflows/actionlint.yml) with proper ${{ }} masking and inline
# disable support — re-extracting them would duplicate that pass badly.
if command -v shellcheck >/dev/null 2>&1; then
  shfiles=()
  for s in actionlint tools/*.sh .github/actions/*/*.sh tests/*.sh; do
    [ -f "$s" ] || continue
    shfiles+=("$s")
  done
  if [ "${#shfiles[@]}" -gt 0 ]; then
    shellcheck "${shfiles[@]}" \
      || fail "shellcheck reported violations — fix, or inline-disable with a reason"
    echo "[✓] shellcheck: ${#shfiles[@]} script(s) clean"
  fi
else
  note "shellcheck not on PATH — skipped (CI runners ship it; local parity: bash tools/shellcheck.sh <files>)"
fi

# --- 7. node --test (when node and test files exist) ----------------------------
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

# --- 8. shell e2e tests ---------------------------------------------------------
ran=0
for t in tests/*.test.sh; do
  [ -f "$t" ] || continue
  ran=$((ran + 1))
  bash "$t" || fail "shell test failed: $t"
done
[ "$ran" -gt 0 ] && echo "[✓] shell tests: $ran file(s) green"

note "repo validation complete"
