#!/usr/bin/env bash
# apply_and_publish.sh — deterministic forge-write post-script (issue #210)
#
# The ONLY entry point in the implement stage that uses a forge write
# credential. It runs after the engine exits; the engine's environment
# carried only FORGE_TOKEN_READ (a read-scoped installation token), so a
# compromised or manipulated agent cannot reach this credential.
#
# Everything here is deterministic: the script applies the diff the engine
# left in the working tree (commit → push → PR). No agent text is executed;
# the issue title is used only as a branch-slug source and a PR title.
#
# Env contract:
#   FORGE_TOKEN_WRITE  required — write-scoped forge token (gh + git push auth)
#   REPO               required — owner/name
#   ISSUE              required — issue number (branch slug, PR title, Closes #N)
#   BASE               optional — PR base branch (default: main)
#   RUN_ID             optional — workflow run id for the PR body footer
#   ISSUE_TITLE        optional — issue title override; else read from
#                      issue.json (prompt-step artifact) or via `gh issue view`
#   DRY_RUN            '1' = stage + commit + resolve branch, then stop before
#                      push/PR — used by the test suite (no network, no forge)
# Outputs: pr_number + branch written to $GITHUB_OUTPUT; the same values are
# echoed on stdout for the audit log.
set -euo pipefail

: "${FORGE_TOKEN_WRITE:?required — apply_and_publish.sh is the forge-write boundary (issue #210)}"
: "${REPO:?required — owner/name}"
: "${ISSUE:?required — issue number}"
BASE="${BASE:-main}"

# All forge API calls in this script authenticate with FORGE_TOKEN_WRITE.
export GH_TOKEN="$FORGE_TOKEN_WRITE"

git config user.name "ai-sdlc[bot]"
git config user.email "ai-sdlc[bot]@users.noreply.github.com"

# Engine session artifacts are run evidence uploaded as workflow artifacts —
# exclude them so only real source changes land in the consumer diff
# (issue #68). They are gitignored only in the runtime repo, and `git add`
# aborts when a pathspec names an ignored path, so unstage whatever got
# staged instead of `:!` pathspec excludes. `.github/workflows` is stripped:
# the pipeline never edits workflow files in consumer repos (AGENTS.md).
git add -A -- . ':!.github/workflows'
git diff --cached --name-only -z -- \
  'scc-*.json*' 'ai-sdlc-run.json' 'issue.json' 'task-prompt.md' \
  | xargs -0 -r git reset -q --

committed=false
if ! git diff --cached --quiet; then
  git commit -q -m "feat: implement issue #$ISSUE (ai-sdlc runtime)"
  committed=true
fi

# Branch slug from the issue title: ISSUE_TITLE env first (test path), then
# the issue.json artifact the prompt step already wrote, else one forge read.
title="${ISSUE_TITLE:-}"
if [ -z "$title" ] && [ -f issue.json ]; then
  title="$(node -e 'try{process.stdout.write(JSON.parse(require("fs").readFileSync("issue.json","utf8")).title||"")}catch(e){}' 2>/dev/null || true)"
fi
if [ -z "$title" ]; then
  title="$(gh issue view "$ISSUE" -R "$REPO" --json title -q '.title')"
fi
SLUG=$(printf '%s' "$title" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]/-/g' | cut -c1-40 | sed 's/-*$//')
BRANCH="feat/issue-${ISSUE}-${SLUG}"

echo "branch=$BRANCH"
echo "committed=$committed"
echo "branch=$BRANCH" >> "$GITHUB_OUTPUT"

if [ "${DRY_RUN:-0}" = "1" ]; then
  echo "dry-run: diff committed locally; push + PR creation skipped"
  exit 0
fi

AUTH=$(printf 'x-access-token:%s' "$FORGE_TOKEN_WRITE" | base64 -w0)
echo "::add-mask::$AUTH"
git -c "http.https://github.com/.extraheader=AUTHORIZATION: basic $AUTH" \
  push origin "HEAD:$BRANCH"

node "$(dirname "${BASH_SOURCE[0]}")/render_pr_body.js" > pr-body.md
PR_URL=$(gh pr create -R "$REPO" --base "$BASE" --head "$BRANCH" \
  --title "$title" \
  --body-file pr-body.md)
echo "pr_url=$PR_URL"
echo "pr_number=${PR_URL##*/}" >> "$GITHUB_OUTPUT"
