#!/usr/bin/env bash
# tests/repo-hygiene.test.sh — e2e for the repo-hygiene linters wired into
# tools/validate-repo.sh (issues #137/#138/#141):
#   tools/check-doc-snippets.py  — caller snippets vs workflow_call contracts
#   tools/check-run-evidence.py  — upload-artifact names/paths vs the layout
# Asserts the exit contract: 0 clean, 1 violations. Negative fixtures live
# in a throwaway --root so the real tree stays untouched.
set -euo pipefail
cd "$(dirname "$0")/.."

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0; fail=0
ok()  { pass=$((pass+1)); printf '[✓] %s\n' "$*"; }
bad() { fail=$((fail+1)); printf '[X] %s\n' "$*" >&2; }

expect() { # $1 expected exit · $2 label · rest: command
  local want="$1" label="$2"; shift 2
  set +e
  "$@" >"$WORK/out.txt" 2>&1
  local got=$?
  set -e
  if [ "$got" = "$want" ]; then
    ok "$label (exit $got)"
  else
    bad "$label: want $want got $got — $(tail -5 "$WORK/out.txt")"
  fi
}

# --- 1. the real repo is clean -------------------------------------------------
expect 0 "doc snippets: real repo clean" \
  python3 tools/check-doc-snippets.py
expect 0 "run evidence: real repo clean" \
  python3 tools/check-run-evidence.py

# --- 2. fixture callee + docs ---------------------------------------------------
FIX="$WORK/fix"
mkdir -p "$FIX/.github/workflows" "$FIX/docs"
cat > "$FIX/.github/workflows/ai-sdlc-impl.yml" <<'EOF'
name: ai-sdlc-impl
on:
  workflow_call:
    inputs:
      repo: { type: string, required: true }
      issue_number: { type: number, required: true }
    secrets:
      AI_SDLC_APP_ID: { required: true }
      AI_SDLC_APP_PRIVATE_KEY: { required: true }
      MODEL_API_KEY: { required: false }
jobs:
  noop:
    runs-on: ubuntu-latest
    timeout-minutes: 1
    steps: [{ run: 'true' }]
EOF

good_doc() {
  cat > "$1" <<'EOF'
# doc
```yaml
jobs:
  impl:
    uses: os-santiago/ai-sdlc/.github/workflows/ai-sdlc-impl.yml@main
    with:
      repo: ${{ github.repository }}
      issue_number: 1
    secrets:
      AI_SDLC_APP_ID: ${{ secrets.AI_SDLC_APP_ID }}
      AI_SDLC_APP_PRIVATE_KEY: ${{ secrets.AI_SDLC_APP_PRIVATE_KEY }}
```
EOF
}

# 2a. clean fixture passes
good_doc "$FIX/docs/runtime.md"; : > "$FIX/README.md"
expect 0 "doc snippets: conforming fixture" \
  python3 tools/check-doc-snippets.py --root "$FIX"

# 2b. secrets: inherit is rejected
cat > "$FIX/docs/runtime.md" <<'EOF'
```yaml
jobs:
  impl:
    uses: ./.github/workflows/ai-sdlc-impl.yml
    with:
      repo: a/b
      issue_number: 1
    secrets: inherit
```
EOF
expect 1 "doc snippets: secrets:inherit rejected" \
  python3 tools/check-doc-snippets.py --root "$FIX"

# 2c. typo'd input + missing required secret fails
cat > "$FIX/docs/runtime.md" <<'EOF'
```yaml
jobs:
  impl:
    uses: ./.github/workflows/ai-sdlc-impl.yml
    with:
      repo: a/b
      issue_numberr: 1
    secrets:
      AI_SDLC_APP_ID: ${{ secrets.AI_SDLC_APP_ID }}
```
EOF
expect 1 "doc snippets: typo input + missing required secret" \
  python3 tools/check-doc-snippets.py --root "$FIX"

# 2d. a referenced repo path that does not exist fails
good_doc "$FIX/docs/runtime.md"
cat > "$FIX/README.md" <<'EOF'
Run `scripts/does-not-exist.js` to fix it.
EOF
expect 1 "doc snippets: dangling path reference" \
  python3 tools/check-doc-snippets.py --root "$FIX"

# --- 3. run-evidence fixture ----------------------------------------------------
EV="$WORK/ev"
mkdir -p "$EV/.github/workflows" "$EV/spec"
cp spec/run-evidence.md "$EV/spec/run-evidence.md"
cat > "$EV/.github/workflows/wf.yml" <<'EOF'
name: wf
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    timeout-minutes: 1
    steps:
      - uses: actions/upload-artifact@v4
        with:
          name: rogue-artifact
          path: ${{ runner.temp }}/ai-sdlc-x/f.json
          retention-days: 14
EOF
expect 1 "run evidence: undeclared artifact name" \
  python3 tools/check-run-evidence.py --root "$EV"

# 3b. declared name but workspace path where runner_temp is required
cat > "$EV/.github/workflows/ai-sdlc-ci-repair.yml" <<'EOF'
name: wf
on: push
jobs:
  j:
    runs-on: ubuntu-latest
    timeout-minutes: 1
    steps:
      - uses: actions/upload-artifact@v4
        with:
          name: ai-sdlc-ci-repair-${{ inputs.pr_number }}-attempt-${{ github.run_attempt }}
          path: scc-audit.jsonl
          retention-days: 14
EOF
rm "$EV/.github/workflows/wf.yml"
expect 1 "run evidence: workspace path in runner_temp class" \
  python3 tools/check-run-evidence.py --root "$EV"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
