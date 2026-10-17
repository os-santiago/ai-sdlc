#!/usr/bin/env bash
# tests/apply-patch.test.sh — shell-level e2e for the deterministic apply
# gate (issue #212). Drives the real CLI (`node scripts/apply-patch.js`)
# against a throwaway git repo and asserts the exit contract:
#   0 applied/validated · 2 validation rejection · 1 operational error
set -euo pipefail
cd "$(dirname "$0")/.."

APPLY="node scripts/apply-patch.js"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0; fail=0
ok()   { pass=$((pass+1)); printf '[✓] %s\n' "$*"; }
bad()  { fail=$((fail+1)); printf '[X] %s\n' "$*" >&2; }

# throwaway repo with one tracked file
git -C "$WORK" init -q
git -C "$WORK" config user.email t@t && git -C "$WORK" config user.name t
printf 'old\n' > "$WORK/x.js"
git -C "$WORK" add . && git -C "$WORK" commit -qm init

expect() { # $1 expected exit · $2 label · $3 patch file · rest: extra args
  local want="$1" label="$2" pf="$3"; shift 3
  set +e
  $APPLY --patch "$pf" --repo "$WORK" "$@" >/dev/null 2>"$WORK/err.txt"
  local got=$?
  set -e
  if [ "$got" = "$want" ]; then ok "$label (exit $got)"; else bad "$label: want $want got $got — $(cat "$WORK/err.txt")"; fi
}

# --- fixtures ------------------------------------------------------------------
cat > "$WORK/clean.patch" <<'EOF'
diff --git a/x.js b/x.js
index 1111111..2222222 100644
--- a/x.js
+++ b/x.js
@@ -1,1 +1,1 @@
-old
+new
EOF

cat > "$WORK/traverse.patch" <<'EOF'
diff --git a/../evil.sh b/../evil.sh
new file mode 100644
index 0000000..1111111
--- /dev/null
+++ b/../evil.sh
@@ -0,0 +1 @@
+#!/bin/sh
EOF

cat > "$WORK/workflow.patch" <<'EOF'
diff --git a/.github/workflows/evil.yml b/.github/workflows/evil.yml
new file mode 100644
index 0000000..1111111
--- /dev/null
+++ b/.github/workflows/evil.yml
@@ -0,0 +1 @@
+on: push
EOF

cat > "$WORK/symlink.patch" <<'EOF'
diff --git a/link b/link
new file mode 120000
index 0000000..1111111
--- /dev/null
+++ b/link
@@ -0,0 +1 @@
+/etc/shadow
EOF

cat > "$WORK/binary.patch" <<'EOF'
diff --git a/x.png b/x.png
index 1111111..2222222 100644
Binary files a/x.png and b/x.png differ
EOF

printf 'this is not a patch\n' > "$WORK/notpatch.txt"
head -c 3000 /dev/zero | tr '\0' 'x' > "$WORK/big.patch"

# --- assertions ------------------------------------------------------------------
expect 0 "clean patch applies"               "$WORK/clean.patch"
if [ "$(cat "$WORK/x.js")" = "new" ]; then ok "applied content landed"; else bad "applied content missing"; fi

expect 2 "dotdot traversal rejected"         "$WORK/traverse.patch"
expect 2 "workflow path rejected"            "$WORK/workflow.patch"
expect 2 "symlink mode rejected"             "$WORK/symlink.patch"
expect 2 "binary patch rejected"             "$WORK/binary.patch"
expect 2 "non-patch input rejected"          "$WORK/notpatch.txt"
expect 2 "oversized patch rejected"          "$WORK/big.patch" --max-bytes 100
printf 'old\n' > "$WORK/x.js"  # restore pre-patch content for a clean check
expect 0 "check-only validates, no mutation" "$WORK/clean.patch" --check-only
if [ "$(cat "$WORK/x.js")" = "old" ]; then ok "check-only left worktree untouched"; else bad "check-only mutated the worktree"; fi

# git apply --check failure is a rejection (exit 2), not a crash
cat > "$WORK/stale.patch" <<'EOF'
diff --git a/x.js b/x.js
index 1111111..2222222 100644
--- a/x.js
+++ b/x.js
@@ -1,1 +1,1 @@
-different
+newer
EOF
expect 2 "stale/forged context rejected"     "$WORK/stale.patch"

if [ -e "$WORK/../evil.sh" ]; then bad "traversal escaped the worktree"; fi
if [ -e "$WORK/.github" ];    then bad "workflow path was written"; fi

printf '%s\n' "--- $pass passed, $fail failed ---"
[ "$fail" -eq 0 ]
