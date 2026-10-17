# Run-evidence layout

Normative layout for run evidence produced by the `ai-sdlc-*` workflows
(issue #141). Two historical conventions existed — ci-repair/review wrote
under `${{ runner.temp }}` while implement wrote engine session files to
the workspace root — which complicated artifact layout and debugging.
This document is the single source of truth for where each class of
evidence lives, what the upload is named, and how long it is retained.
`tools/check-run-evidence.py` enforces it inside
`tools/validate-repo.sh`: a `upload-artifact` step whose name or paths do
not match a declared entry fails the repo check.

## Rules

R1. **Working evidence lives under `${{ runner.temp }}/ai-sdlc-<stage>/`.**
    One directory per stage, named after the stage (`ai-sdlc-review`,
    `ai-sdlc-ci-repair`, `ai-sdlc-dep-update`, `ai-sdlc-contract` for the
    load-config action). Never the workspace root: run files must not
    pollute the checkout, the diff surface, or `.gitignore`.

R2. **Ledger segments live under `${{ runner.temp }}/ai-sdlc-ledger/`**
    as `ledger-<stage>.jsonl` and upload as
    `ai-sdlc-ledger-<stage>-<subject>-<run-attempt>` — 14-day retention.

R3. **Artifact names follow `ai-sdlc-<stage>-<subject>[-attempt-<n>]`**,
    where `<subject>` is the issue or PR number (or the run id/attempt
    for unscoped sweeps) rendered as a `${{ }}` expression. Attempt-scoped
    bundles add `-attempt-<n>` before the run-attempt suffix. Fixed-name
    reports (`ai-sdlc-slo-metrics`, `ai-sdlc-drift-report`) are allowed
    only when declared below.

R4. **Workspace-root uploads are the report/session class — a closed,
    enumerated set.** The implement stage still writes its engine session
    files to the workspace root (the engine CLI and `git` operate on the
    checkout; the file set is `git add`-excluded and root-`.gitignore`d);
    migrating it to R1 removes the `.gitignore` run-artifact entries —
    that is the tracked remainder of issue #141. New workspace writes
    outside the declared `paths` glob fail the check.

R5. **Retention:** stage evidence 14 days; reports may declare more
    (ready-queue 30, drift/metrics 90, release 30). A `retention-days`
    that disagrees with the declared value fails the check.

## Declared artifacts

The table mirrors the machine-readable block below; keep both in sync —
the linter reads the block, humans read the table.

| Artifact | Stage | Class | Location | Retention |
|---|---|---|---|---|
| `ai-sdlc-run-<issue>` | implement | workspace-session | session files at repo root (R4) | 14d |
| `ai-sdlc-review-<pr>-<attempt>` | review | engine-session | `${{ runner.temp }}/ai-sdlc-review/` | 14d |
| `ai-sdlc-review-fix-<pr>-attempt-<n>` | review-fix | engine-session | `${{ runner.temp }}/ai-sdlc-review-fix/` | 14d |
| `ai-sdlc-ci-repair-<pr>-attempt-<n>` | ci-repair | engine-session | `${{ runner.temp }}/ai-sdlc-ci-repair/` | 14d |
| `ai-sdlc-dep-update-<attempt>` | dep-update | engine-session | `${{ runner.temp }}/ai-sdlc-dep-update/` (+ its ledger segment) | 14d |
| `ai-sdlc-contract-<job>-<attempt>` | all (load-config) | contract | `${{ runner.temp }}/ai-sdlc-contract/` | 14d |
| `ai-sdlc-ledger-<stage>-<subject>-<attempt>` | every sealed stage | ledger | `${{ runner.temp }}/ai-sdlc-ledger/ledger-<stage>.jsonl` | 14d |
| `ai-sdlc-ready-queue-<run>-<attempt>` | sweep | workspace-report | `ready-queue.json`/`ready-queue.md` at root | 30d |
| `ai-sdlc-slo-metrics` | metrics | workspace-report | `slo-metrics.json`/`.prom`, `slo-input.json` at root | 90d |
| `ai-sdlc-drift-report` | drift | workspace-report | `drift-report.json`, `drift-input.json` at root | 90d |
| `ai-sdlc-<tag>` | release | build | `dist/` — the shipped artifact, not run evidence | 30d |

<!-- ai-sdlc:run-evidence -->
```json
{
  "version": 1,
  "comment": "machine-readable run-evidence layout — tools/check-run-evidence.py consumes this; keep the table above in sync. match templates use <e> for one ${{ }} expression; 'regex' matches the raw name text; files scopes the artifact to workflow/action files; path_root runner_temp requires every path under ${{ runner.temp }}/ai-sdlc-, workspace requires every path to match the declared paths globs.",
  "artifacts": [
    {
      "id": "implement-run",
      "match": "ai-sdlc-run-<e>",
      "files": [".github/workflows/ai-sdlc-implement.yml"],
      "class": "workspace-session",
      "path_root": "workspace",
      "paths": ["scc-*", "engine-run*", "ai-sdlc-*", "apply-result.json",
                "issue.json", "task-prompt.md", "injection-audit.jsonl"],
      "retention": 14
    },
    {
      "id": "review-bundle",
      "match": "ai-sdlc-review-<e>-<e>",
      "files": [".github/workflows/ai-sdlc-review.yml"],
      "class": "engine-session",
      "path_root": "runner_temp",
      "retention": 14
    },
    {
      "id": "review-fix-bundle",
      "match": "ai-sdlc-review-fix-<e>-attempt-<e>",
      "files": [".github/workflows/ai-sdlc-review.yml"],
      "class": "engine-session",
      "path_root": "runner_temp",
      "retention": 14
    },
    {
      "id": "ci-repair-bundle",
      "match": "ai-sdlc-ci-repair-<e>-attempt-<e>",
      "files": [".github/workflows/ai-sdlc-ci-repair.yml"],
      "class": "engine-session",
      "path_root": "runner_temp",
      "retention": 14
    },
    {
      "id": "dep-update-bundle",
      "match": "ai-sdlc-dep-update-<e>",
      "files": [".github/workflows/ai-sdlc-dep-update.yml"],
      "class": "engine-session",
      "path_root": "runner_temp",
      "retention": 14
    },
    {
      "id": "contract-bundle",
      "regex": "ai-sdlc-contract-",
      "files": [".github/actions/load-config/action.yml"],
      "class": "contract",
      "path_root": "runner_temp",
      "retention": 14
    },
    {
      "id": "ledger-segment",
      "regex": "^ai-sdlc-ledger-(intake|implement|review|review-fix|ci-repair|automerge|dep-update|verify|kill-switch|drift|fleet|metrics|sweep)-",
      "files": [".github/workflows/ai-sdlc-intake.yml",
                ".github/workflows/ai-sdlc-implement.yml",
                ".github/workflows/ai-sdlc-review.yml",
                ".github/workflows/ai-sdlc-ci-repair.yml",
                ".github/workflows/ai-sdlc-automerge.yml",
                ".github/workflows/ai-sdlc-dep-update.yml",
                ".github/workflows/ai-sdlc-verify.yml",
                ".github/workflows/ai-sdlc-kill.yml"],
      "class": "ledger",
      "path_root": "runner_temp",
      "retention": 14
    },
    {
      "id": "ready-queue",
      "match": "ai-sdlc-ready-queue-<e>-<e>",
      "files": [".github/workflows/ai-sdlc-sweep.yml"],
      "class": "workspace-report",
      "path_root": "workspace",
      "paths": ["ready-queue.json", "ready-queue.md"],
      "retention": 30
    },
    {
      "id": "slo-metrics",
      "match": "ai-sdlc-slo-metrics",
      "files": [".github/workflows/ai-sdlc-metrics.yml"],
      "class": "workspace-report",
      "path_root": "workspace",
      "paths": ["slo-metrics.json", "slo-metrics.prom", "slo-input.json"],
      "retention": 90
    },
    {
      "id": "drift-report",
      "match": "ai-sdlc-drift-report",
      "files": [".github/workflows/ai-sdlc-drift.yml"],
      "class": "workspace-report",
      "path_root": "workspace",
      "paths": ["drift-report.json", "drift-input.json"],
      "retention": 90
    },
    {
      "id": "release-bundle",
      "match": "ai-sdlc-<e>",
      "files": [".github/workflows/ai-sdlc-release.yml"],
      "class": "build",
      "path_root": "workspace",
      "paths": ["dist/"],
      "retention": 30
    }
  ]
}
```

## Adding an artifact

1. Put the working files under `${{ runner.temp }}/ai-sdlc-<stage>/`
   (or `ai-sdlc-ledger/` for a sealed segment). Workspace writes require
   a declared `workspace` entry and `.gitignore` coverage — prefer R1.
2. Name the upload per R3.
3. Declare it in the JSON block + the table, scoped to its file(s) —
   the linter fails undeclared names.
