# DORA Metrics — Canonical Definitions

Normative definitions for the four DORA metrics the runtime reports on
itself and on consumer repos. The reference implementation is
[`scripts/dora-metrics.js`](../../scripts/dora-metrics.js); the scheduled
publisher is `.github/workflows/ai-sdlc-dora.yml`, which regenerates the
committed report at [`docs/metrics/dora.md`](dora.md) (diff-gated —
`spec/generated-artifact-publish-contract.md`).

This document is canonical. If implementation and prose ever disagree,
the drift is a bug — fix whichever side is wrong in the same PR.

## Scope and population

- **Window** — a trailing `--window` interval (default `30d`), half-open
  `[start, end)`. All timestamps are ISO-8601 UTC.
- **Base branch** — `--base` (default `main`). Only merges and runs on the
  base branch count.
- **Population** — pull requests fetched via `gh pr list --state all`
  (bounded by `--max-prs`, default 200, cap 500). A merge counts when its
  `mergedAt` lands inside the window and `baseRefName` equals the base.
- **Failure window** — `--failure-window` (default `48h`): how far back a
  red base run may look for the merge that introduced it.

## Event sources (GitHub API)

The computation is pure over a collected model — five bounded reads plus
at most one timeline call per merged-in-window PR. No run ledger is
required; everything derives from the GitHub API.

| Signal | API surface | Fields consumed |
|---|---|---|
| PR lifecycle | `gh pr list --state all --json …` | `number`, `title`, `state`, `headRefName`, `baseRefName`, `createdAt`, `mergedAt`, `closedAt`, `isDraft`, `labels`, `author.login`, `body`, `mergeCommit.oid` |
| Label events | REST `GET /repos/{repo}/issues/{n}/timeline` (`--paginate`) | `event == "labeled"` entries where `label.name == "ready-to-implement"` — the earliest `created_at` wins |
| Check runs / commit status | `gh run list --branch <base>` — Actions workflow runs, which GitHub surfaces as the check runs on each base commit | `databaseId`, `createdAt`, `updatedAt`, `status`, `conclusion`, `event`, `headSha`, `workflowName` |
| Issue state | `gh issue list --state all` | `number`, `state`, `labels` — current `needs-human` / `scc:failed` holds |
| Releases (secondary) | `gh release list` | `tagName`, `isDraft`, `isPrerelease`, `createdAt`, `publishedAt` |

Notes on the sources:

- **Label events, not current labels.** The `ready-to-implement` trigger
  is consumed at dispatch, so the *current* label set cannot answer "when
  was it armed". The timeline lookup targets the **linked issue** first —
  branch `feat|fix/issue-N` binds, else a closing keyword
  (`close[sd]?|fix(e[sd])?|resolve[sd]? #N`) in the PR body; `Refs #N`
  does not bind — falling back to the PR number itself when the PR is
  unlinked. An unreadable timeline degrades to `basis=pr_created`.
- **Check status = Actions run conclusions.** Only runs with
  `status == "completed"` are considered. A run is **red** when
  `conclusion ∈ {failure, cancelled, timed_out, startup_failure}` and
  **green** when `conclusion == "success"`. There is no separate
  commit-status API read; the run list *is* the check-run rollup on the
  base branch.
- **Bounded by design.** One `pr list`, one `issue list`, one `run list`
  (`--runs`, default 200, cap 500), one `release list` (`--releases`,
  default 50, cap 100), then ≤ one timeline call per merged-in-window PR.

## Author classifier — agentic vs human

Every merged-in-window PR is classified `agentic` when **any** of the
following holds, otherwise `human`:

1. Head branch matches `^(feat|fix)/issue-(\d+)` — the canonical branch
   convention `implement` pushes.
2. The PR carries the `ai-sdlc` label.
3. `author.login` (case-insensitive) is in `--agentic-authors` —
   default `ai-sdlc-runtime[bot]`, the GitHub App identity all pipeline
   automation runs as.

This is the same sweep surface the merge gate and SLO metrics feed on, so
the agentic population matches what the pipeline actually ships. The
comparison is **within the same repo** — identical CI and review surface —
which is the only apples-to-apples baseline (epic #88).

## The four metrics

### 1. Deployment frequency

A **deployment** is a merge to the base branch whose `mergedAt` falls in
the window. Reported as count, per-day and per-week rates, split by
author type.

Boundaries:

- Only merged PRs to the base branch count. Closed-unmerged PRs, PRs to
  other branches, and direct pushes (no PR) are not deployments.
- Release tags (non-draft releases, `publishedAt` else `createdAt` in
  window) are tracked as a **secondary counter only** — too sparse for a
  frequency signal on CLI/library repos. A failed release fetch renders
  as *unavailable*, distinct from zero.

### 2. Lead time for changes

Hours from the earliest `ready-to-implement` label event on the linked
issue (or the PR) to the merge: `basis=ready_label`.

- **Fallback:** when no label event is found — unlabeled PR, unreadable
  timeline — the PR `createdAt` is used and the sample is flagged
  `basis=pr_created`. Fallback samples are never silently mixed in.
- Negative latencies (clock skew) are dropped, never clamped.
- Reported per author type as median, mean, min, max and p90
  (nearest-rank) in hours.

### 3. Change failure rate

`failed merges / total merges × 100`, per author type. A merged-in-window
PR is a failure when **any** of these signals binds:

| Signal | Boundary |
|---|---|
| `red_main` | A base-branch red episode is attributed to the merge — exact match when the episode-opening run's `headSha` equals `mergeCommit.oid`; otherwise the nearest preceding merge within `--failure-window` (scheduled/dispatch runs carry a moved sha) |
| `revert` | A merged revert PR bound to the merge — head branch `revert-N(-…)`, or a `Revert …` title carrying a `#N` reference in title/body. A revert merged *after* the window still fails the original merge |
| `non_converging_repair` | The merge's linked subject (the PR itself or its linked issue) still holds `needs-human` or `scc:failed`, **or** an attributed red episode never recovered inside the window — the merge is done but the failure is not |

A merge with several signals still counts once.

### 4. MTTR — time to restore

A **recovery episode** is a run of consecutive red completed runs on the
base branch, closed by the next completed `success` run.
`restore = endTs(green) − endTs(opening red)` where `endTs` is the run's
`updatedAt` (falling back to `createdAt`).

- `--gate-workflows` scopes which workflows count (case-insensitive);
  the default is **all** completed base-branch runs.
- Episodes are attributed to the introducing merge (same attribution as
  `red_main`); episodes with no attributable merge land in the
  `unattributed` bucket.
- An episode still red at collection time is **ongoing**: counted, but
  excluded from the mean until it recovers — ongoing red is reported
  honestly rather than silently omitted or zero-timed.

## Boundary summary

| Question | Counts when |
|---|---|
| Deployment? | Merged PR to `main` with `mergedAt` in window (release tags secondary) |
| Lead-time start? | Earliest `ready-to-implement` label event on linked issue/PR; else PR creation |
| Change failure? | Merge followed by attributed red main, merged revert, or non-converging repair |
| Recovered? | Next completed `success` run on `main` after a red streak |
| Agentic? | `feat\|fix/issue-N` branch, `ai-sdlc` label, or `ai-sdlc-runtime[bot]` author |

## External-benchmark caveat

These numbers are **illustrative and non-comparable** against published
DORA / Accelerate benchmarks:

- "Deployment" here means *merge to the base branch* of a CLI/library
  repo — not a production release — so deployment frequency and MTTR sit
  on a different operational event than industry benchmarks assume.
- The population is GitHub-API-derived over a bounded window with bounded
  scan sizes; long windows on busy repos can truncate at `--max-prs`.
- The valid comparison is **internal**: agentic vs human PRs inside the
  same repo, sharing CI and review. External benchmark tiers may be
  quoted as context only — never as a like-for-like claim.

## Output surfaces

- `docs/metrics/dora.md` — committed markdown report, regenerated weekly
  (Mondays 06:07 UTC) by `ai-sdlc-dora.yml`, diff-gated.
- `dora-metrics-json` workflow artifact (90-day retention) plus
  `dora-input.json` — the collected model the compute replays over.
- GitHub step summary and flat `$GITHUB_OUTPUT` keys (`dora_*`) for
  downstream steps.
